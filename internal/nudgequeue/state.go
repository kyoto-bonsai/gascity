// Package nudgequeue manages the persisted deferred-nudge queue.
package nudgequeue

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/fsys"
)

// wakeSocketPathLimit caps the canonical socket path length below the
// platform sockaddr_un limit (108 bytes on Linux, 104 on macOS). Matches
// the controllerSocketPathLimit pattern in cmd/gc/controller.go.
const wakeSocketPathLimit = 100

// Reference links a queued nudge back to the object that produced it.
type Reference struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Item is a persisted deferred nudge.
type Item struct {
	ID                string     `json:"id"`
	BeadID            string     `json:"bead_id,omitempty"`
	Agent             string     `json:"agent"`
	SessionID         string     `json:"session_id,omitempty"`
	ContinuationEpoch string     `json:"continuation_epoch,omitempty"`
	Source            string     `json:"source"`
	Message           string     `json:"message"`
	Reference         *Reference `json:"reference,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	DeliverAfter      time.Time  `json:"deliver_after"`
	ExpiresAt         time.Time  `json:"expires_at"`
	Attempts          int        `json:"attempts,omitempty"`
	LastAttemptAt     time.Time  `json:"last_attempt_at,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
	ClaimedAt         time.Time  `json:"claimed_at,omitempty"`
	LeaseUntil        time.Time  `json:"lease_until,omitempty"`
	DeadAt            time.Time  `json:"dead_at,omitempty"`
}

// State is the persisted nudge queue snapshot.
type State struct {
	Pending  []Item `json:"pending,omitempty"`
	InFlight []Item `json:"in_flight,omitempty"`
	Dead     []Item `json:"dead,omitempty"`

	// DispatchSkips counts, by reason, how many times the supervisor
	// dispatch tick's per-session loop has silently skipped a target
	// without delivering (see dispatchAllQueuedNudges in cmd/gc). It is a
	// running total since this state file was first created; there is no
	// reset/rotation. Persisted here (rather than kept in-process) so it
	// stays visible to a `gc nudge status` invocation running in a
	// different process than the supervisor that incremented it.
	DispatchSkips map[string]int64 `json:"dispatch_skips,omitempty"`
}

// SortState orders items deterministically inside each queue bucket.
func SortState(state *State) {
	sort.SliceStable(state.Pending, func(i, j int) bool {
		if !state.Pending[i].DeliverAfter.Equal(state.Pending[j].DeliverAfter) {
			return state.Pending[i].DeliverAfter.Before(state.Pending[j].DeliverAfter)
		}
		if !state.Pending[i].CreatedAt.Equal(state.Pending[j].CreatedAt) {
			return state.Pending[i].CreatedAt.Before(state.Pending[j].CreatedAt)
		}
		return state.Pending[i].ID < state.Pending[j].ID
	})
	sort.SliceStable(state.InFlight, func(i, j int) bool {
		if !state.InFlight[i].LeaseUntil.Equal(state.InFlight[j].LeaseUntil) {
			return state.InFlight[i].LeaseUntil.Before(state.InFlight[j].LeaseUntil)
		}
		if !state.InFlight[i].ClaimedAt.Equal(state.InFlight[j].ClaimedAt) {
			return state.InFlight[i].ClaimedAt.Before(state.InFlight[j].ClaimedAt)
		}
		return state.InFlight[i].ID < state.InFlight[j].ID
	})
	sort.SliceStable(state.Dead, func(i, j int) bool {
		if !state.Dead[i].DeadAt.Equal(state.Dead[j].DeadAt) {
			return state.Dead[i].DeadAt.Before(state.Dead[j].DeadAt)
		}
		if !state.Dead[i].CreatedAt.Equal(state.Dead[j].CreatedAt) {
			return state.Dead[i].CreatedAt.Before(state.Dead[j].CreatedAt)
		}
		return state.Dead[i].ID < state.Dead[j].ID
	})
}

// defaultLockWaitTimeout bounds how long WithState waits to acquire the
// queue's exclusive flock before giving up with a descriptive error
// (ga-2kzci3 FR1/FR2). Set to 4x nudgeEnqueueMaintenanceBudget (cmd/gc,
// 2s), per NFR2 -- sized against normal uncontended turnaround, so it
// doesn't false-trigger under ordinary contention while still failing fast
// enough to diagnose in seconds, not the multi-minute hangs this fix
// replaces. It is not a bound every holder respects: the supervisor sweep
// runs against nudgeMaintenanceSweepBudget (cmd/gc, 5m) instead, and the
// lazy bead-store open in nudgeMaintenanceStore.frontForState runs inside
// the locked callback, before the first per-item deadline check, with no
// budget of its own. A waiter can legitimately time out behind either.
const defaultLockWaitTimeout = 8 * time.Second

// WithState locks, loads, mutates, and atomically rewrites the queue state.
// The wait to acquire the lock is bounded to defaultLockWaitTimeout
// (ga-2kzci3 FR1/FR2); a caller that needs a different budget -- e.g. one
// that must keep cycling other work under contention -- can call
// withStateBounded directly instead.
func WithState(cityPath string, fn func(*State) error) error {
	return withStateBounded(cityPath, defaultLockWaitTimeout, clock.Real{}, fn)
}

// nudgeQueueLockPollInterval is how often withStateBounded retries a
// non-blocking lock acquisition while waiting for its budget to expire.
const nudgeQueueLockPollInterval = 10 * time.Millisecond

// writeStateFile is replaceable by serial tests to count physical state writes.
var writeStateFile = fsys.WriteFileAtomic

// afterReadUnlock is a serial-test seam for the shared-to-exclusive gap.
var afterReadUnlock func()

// withStateBounded is WithState's bounded-wait implementation, callable
// directly by a caller that needs a different timeout budget than
// WithState's default -- e.g. the supervisor dispatch tick, which must keep
// cycling other sessions even when the queue is contended (ga-2kzci3
// FR1/FR2). flock offers no notification API, so the bound is enforced by
// polling LOCK_EX|LOCK_NB against clk until either the lock is acquired or
// the budget elapses.
func withStateBounded(cityPath string, waitTimeout time.Duration, clk clock.Clock, fn func(*State) error) error {
	return withStateBoundedMode(cityPath, waitTimeout, clk, stateProducerLock, fn)
}

func withStateBoundedMode(cityPath string, waitTimeout time.Duration, clk clock.Clock, mode stateLockMode, fn func(*State) error) error {
	release, err := lockState(cityPath, mode, waitTimeout, clk)
	if err != nil {
		return err
	}
	defer release()

	state, err := LoadState(cityPath)
	if err != nil {
		return err
	}

	// ga-cssm95: skip the marshal+atomic-rewrite when fn made no observable
	// change. Every read-shaped caller (list/status/liveness/claim-scan
	// paths) routes through this same exclusive lock and runs its
	// maintenance/claim-scan pass inside fn regardless of whether there is
	// anything to recover/prune/claim -- on a healthy queue that finds
	// nothing to do on the overwhelming majority of calls. Under ~50
	// concurrent poller ticks every ~2s, paying a full JSON marshal + fsync +
	// rename on every no-op call is what was starving real writers
	// (sling/mail/nudge) past their defaultLockWaitTimeout budget on a loaded
	// box: that write was the dominant cost per critical section, not the
	// lock acquisition itself.
	//
	// Compared via json.Marshal, deliberately not reflect.DeepEqual:
	// DeepEqual on time.Time can report a difference from monotonic-clock
	// reading noise alone even when the wall-clock value fn produced is
	// identical; JSON has no concept of the monotonic reading, so marshaling
	// both sides normalizes that away.
	before, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal nudge queue (pre-check): %w", err)
	}
	if err := fn(&state); err != nil {
		return err
	}
	after, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal nudge queue: %w", err)
	}
	if bytes.Equal(before, after) {
		return nil
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal nudge queue: %w", err)
	}
	if err := writeStateFile(fsys.OSFS{}, StatePath(cityPath), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write nudge queue: %w", err)
	}
	return nil
}

// ReadThenWrite reads under a shared lock. When needsWrite finds maintenance
// or a claim, it releases the shared lock and reruns write under an exclusive
// lock against freshly loaded state. read must have no side effects.
func ReadThenWrite(cityPath string, needsWrite func(State) bool, read func(State) error, write func(*State) error) error {
	release, err := lockState(cityPath, stateReadLock, defaultLockWaitTimeout, clock.Real{})
	if err != nil {
		return err
	}
	state, err := LoadState(cityPath)
	if err != nil {
		release()
		return err
	}
	if !needsWrite(state) {
		err = read(state)
		release()
		return err
	}
	release()
	if afterReadUnlock != nil {
		afterReadUnlock()
	}
	return withStateBoundedMode(cityPath, defaultLockWaitTimeout, clock.Real{}, stateMaintenanceLock, write)
}

// LoadState reads the persisted queue state from disk.
func LoadState(cityPath string) (State, error) {
	data, err := os.ReadFile(StatePath(cityPath))
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read nudge queue: %w", err)
	}
	if len(data) == 0 {
		return State{}, nil
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("parse nudge queue: %w", err)
	}
	SortState(&state)
	return state, nil
}

// StatePath returns the persisted queue state path for a city.
func StatePath(cityPath string) string {
	return citylayout.RuntimePath(cityPath, "nudges", "state.json")
}

// LockPath returns the queue state lock path for a city.
func LockPath(cityPath string) string {
	return citylayout.RuntimePath(cityPath, "nudges", "state.lock")
}

// WakeSocketPath returns the path to the supervisor nudge-dispatcher wake
// socket. Producers connect to this path after enqueue to trigger immediate
// dispatch; the supervisor listens on it when daemon.nudge_dispatcher is
// "supervisor".
//
// Preserves the legacy `<city>/.gc/runtime/nudges/wake.sock` location for
// short city paths but falls back to a deterministic short temp-path
// when the legacy pathname is too close to the platform sockaddr_un
// limit. Mirrors the controllerSocketPath pattern in cmd/gc/controller.go.
func WakeSocketPath(cityPath string) string {
	legacy := citylayout.RuntimePath(cityPath, "nudges", "wake.sock")
	if len(legacy) <= wakeSocketPathLimit {
		return legacy
	}
	canonical, err := filepath.Abs(cityPath)
	if err != nil {
		canonical = cityPath
	}
	canonical = filepath.Clean(canonical)
	sum := sha256.Sum256([]byte(canonical))
	return filepath.Join("/tmp", "gascity-nudge", fmt.Sprintf("%x.sock", sum[:16]))
}
