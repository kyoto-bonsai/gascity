// Package reconcilerhealth manages the session reconciler's per-tick
// liveness gauge (.gc/runtime/reconciler-tick-health.json).
//
// The session reconciler ticks on a variable cadence driven by upstream
// store latency (Dolt, providers). Under degraded upstream conditions a
// tick can take minutes instead of the healthy single-digit seconds
// (ga-r6lc7g: one observed wave took 4m49s against a 10-minute
// pending-create lease, so a session created between two ticks could have
// its lease expire before the reconciler got around to it at all). Before
// this package, answering "is the reconciler currently keeping up" required
// grepping supervisor.log for "session lifecycle: op=start wave=" lines or
// parsing trace segments after the fact — there was no live, structured
// signal `gc doctor` could check.
//
// This file is intentionally per-clone and gitignored (city-local runtime
// state, like suspension-state.json): it is a liveness gauge, not
// configuration, and carries no meaning across machines.
package reconcilerhealth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// State is the runtime tick-health gauge persisted to disk.
type State struct {
	// LastTickCompletedAt is stamped by Save on every write — the wall-clock
	// time the reconciler last completed a start-execution phase. A doctor
	// check compares time.Since(LastTickCompletedAt) against an expected
	// cadence to detect a stalled or badly-degraded reconciler.
	LastTickCompletedAt time.Time `json:"last_tick_completed_at"`
	// LastPhaseDurationMs is how long the just-completed start-execution
	// phase (session_reconcile.execute_planned_starts, covering every wave
	// this tick) took, in milliseconds. The healthy baseline is low
	// single-digit seconds; ga-r6lc7g observed 4m49s (289000ms) during the
	// incident.
	LastPhaseDurationMs int64 `json:"last_phase_duration_ms"`
	// StartCandidateCount and PlannedWakeCount mirror the same tick's trace
	// fields (session_reconcile.execute_planned_starts) so a doctor check or
	// a human reading the file does not need to cross-reference the trace to
	// see roughly what the last tick was doing.
	StartCandidateCount int `json:"start_candidate_count"`
	PlannedWakeCount    int `json:"planned_wake_count"`
	// TickCount is a best-effort monotonic counter of start-execution phases
	// observed since this file was first created (not since controller
	// start — the file can predate or outlive any one controller process).
	// It exists only so a reader can distinguish "freshly created, one
	// sample so far" from "long-running, many samples" — never treat it as
	// an exact tick census.
	TickCount int64 `json:"tick_count"`
}

// Load reads the reconciler tick-health gauge. A missing file returns a
// zero-value State and no error — the reconciler may simply not have
// written one yet (fresh city, or a controller binary older than this
// gauge), which callers should treat as "no data," not "unhealthy."
func Load(fs fsys.FS, cityPath string) (State, error) {
	p := citylayout.ReconcilerTickHealthFile(cityPath)
	data, err := fs.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, nil
		}
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, err
	}
	return st, nil
}

// Record stamps and atomically writes an updated tick-health gauge,
// incrementing TickCount off the PREVIOUSLY persisted value (best-effort:
// a concurrent writer racing this read-modify-write can under-count, which
// is acceptable for a liveness gauge that only needs "count is roughly
// increasing," never an exact total). LastTickCompletedAt is stamped to
// now(); callers pass everything else. log receives one diagnostic line for
// a benign no-op (see below); pass io.Discard to suppress it. Record never
// returns a non-nil error for a condition a caller should treat as fatal or
// even noteworthy beyond logging -- see the doc on the cityPath-vanished
// case below.
//
// ga-r6lc7g / ga-78s673 hardening (persona-ava's round-2 validation,
// 2026-09-14): a reconciler tick runs on a background goroutine relative to
// whatever else might be concurrently tearing down or moving cityPath (a
// test's t.TempDir() cleanup is the proven case -- see
// TestRecord_ToleratesConcurrentCityPathRemoval -- but nothing here assumes
// it's ONLY tests; a city directory disappearing out from under a live
// process is exactly the kind of thing a best-effort liveness gauge must
// survive, not propagate). Before this hardening, a write that raced a
// directory removal could recreate cityPath/.gc/runtime/<file> in a
// directory a concurrent os.RemoveAll had already listed as empty,
// producing "directory not empty" in the REMOVER, not here -- Record itself
// often returned nil (a "successful" write to a path about to vanish
// anyway). The fix is to check immediately before writing whether cityPath
// itself is still present and, if not, skip the write entirely rather than
// recreate content in a directory something else believes is empty. This
// narrows the race window; it cannot make it zero-width (TOCTOU is
// unavoidable from the writer's side alone against a concurrent, unrelated
// remover) -- the actually-sufficient fix for the reproduced test flake is
// synchronizing the racing goroutine so it never overlaps cityPath removal
// in the first place. This hardening is defense in depth for every OTHER
// case (a vanished city directory this package cannot control the timing
// of), logged and swallowed rather than bubbled up as a tick-recording
// failure.
func Record(fs fsys.FS, cityPath string, phaseDuration time.Duration, startCandidateCount, plannedWakeCount int, log io.Writer) error {
	if log == nil {
		log = io.Discard
	}
	if _, err := fs.Lstat(cityPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(log, "reconcilerhealth: skipping tick-health write: city path %q no longer exists (benign -- likely mid-teardown)\n", cityPath) //nolint:errcheck // best-effort diagnostics
			return nil
		}
		return err
	}

	prior, err := Load(fs, cityPath)
	if err != nil {
		// A corrupt or unreadable prior file must not block the reconciler
		// from recording fresh liveness data -- start the counter over.
		prior = State{}
	}
	st := State{
		LastTickCompletedAt: time.Now().UTC(),
		LastPhaseDurationMs: phaseDuration.Milliseconds(),
		StartCandidateCount: startCandidateCount,
		PlannedWakeCount:    plannedWakeCount,
		TickCount:           prior.TickCount + 1,
	}
	p := citylayout.ReconcilerTickHealthFile(cityPath)
	if err := fs.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(log, "reconcilerhealth: skipping tick-health write: %v (benign -- city path removed concurrently)\n", err) //nolint:errcheck // best-effort diagnostics
			return nil
		}
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := fsys.WriteFileAtomic(fs, p, data, 0o644); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(log, "reconcilerhealth: skipping tick-health write: %v (benign -- city path removed concurrently)\n", err) //nolint:errcheck // best-effort diagnostics
			return nil
		}
		return err
	}
	return nil
}
