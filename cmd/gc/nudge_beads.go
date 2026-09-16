package main

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

const (
	nudgeBeadType = "chore"
	// nudgeBeadLabel is the label applied to queued-nudge beads. coordclass
	// mirrors this string privately (as labelNudge) for store routing; the two
	// must stay in sync.
	nudgeBeadLabel = "gc:nudge"
)

type nudgeReference = nudgequeue.Reference

// openNudgeBeadStore is a test seam (mirrors the injectable vars in
// cmd_nudge.go) so tests can substitute a fake store and assert that
// per-tick poll helpers close every store they open. Tests that replace this
// package variable must stay serial; do not use t.Parallel in those tests.
// It routes the opened work store through resolveNudgesStore and returns the
// strongly-typed beads.NudgesStore so the nudges class is statically visible to
// every leaf nudge-bead helper; the wrapper carries the same underlying store
// value (identity to the work store until the nudges class relocates).
var openNudgeBeadStore = func(cityPath string) beads.NudgesStore {
	store, _ := openNudgeBeadStoreErr(cityPath)
	return store
}

// nudgeMaintenanceStoreOpenTimeout bounds nudgeMaintenanceStore.ensureOpen's
// wait for openNudgeBeadStore (ga-cssm95, 2026-09-15 recurrence). ensureOpen
// runs INSIDE the nudge queue's locked callback (see frontForState's doc
// comment, which already flagged this as an unbounded holder path before
// this fix landed) -- when the Dolt bead store is slow, an uncapped open
// here pinned the flock for minutes and starved every 8s-bounded writer.
//
// Must stay well below internal/nudgequeue/state.go's defaultLockWaitTimeout
// (8s, unexported): a holder bounded at the writers' own wait still starves
// any writer that arrives just after the holder takes the lock. 2s keeps the
// same 4x ratio that constant's comment sizes against
// nudgeEnqueueMaintenanceBudget. Skipping maintenance on a slow tick is safe
// (nil store = do nothing); the next tick retries.
//
// A var, not a const, so tests can shorten it.
var nudgeMaintenanceStoreOpenTimeout = 2 * time.Second

// openNudgeBeadStoreBounded calls openNudgeBeadStore (the existing test seam
// above) with a hard wall-clock bound. On timeout it returns the zero-value
// beads.NudgesStore{} -- exactly what openNudgeBeadStore itself already
// returns on an open error (see its own swallowed-error comment: "a nil
// store means do nothing"), so a slow-but-eventually-successful open
// degrades to "nothing to do this tick" via the SAME nil-tolerant contract
// every maintenance pass already respects, not a new failure shape.
//
// openNudgeBeadStore has no cancellation hook, so the open runs to completion
// in the background. On timeout the caller never receives that store, so a
// drainer closes it when it arrives; otherwise every slow tick leaks a
// connection into the already-slow sql-server.
func openNudgeBeadStoreBounded(cityPath string, timeout time.Duration) beads.NudgesStore {
	result := make(chan beads.NudgesStore, 1)
	go func() { result <- openNudgeBeadStore(cityPath) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case store := <-result:
		return store
	case <-timer.C:
		go func() {
			if late := <-result; late.Store != nil {
				_ = closeBeadStoreHandle(late.Store)
			}
		}()
		return beads.NudgesStore{}
	}
}

// openNudgeBeadStoreErr is openNudgeBeadStore with the open failure kept instead
// of swallowed into a nil-safe zero store.
//
// The zero store is not harmless: every nudge helper below is nil-tolerant, so a
// city whose store will not open reported "opening city store for X" with no
// cause at all — the operator could not tell a missing city from a locked
// database from a storage refusal. Call sites that surface a failure to a human
// use this form and print the reason; the seam above stays for the poll/drain
// helpers whose contract is already "a nil store means do nothing".
func openNudgeBeadStoreErr(cityPath string) (beads.NudgesStore, error) {
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return beads.NudgesStore{}, fmt.Errorf("opening the city store at %q: %w", cityPath, err)
	}
	return beads.NudgesStore{Store: resolveNudgesStore(cliStorageRoutes(cityPath), store, nil, cityPath, nil)}, nil
}

// nudgeFrontDoor wraps a strongly-typed nudges store as the nudge object's
// front door (internal/nudgequeue.Store). The bead is a SHADOW of the flock'd
// state.json queue; the front door confines the Item<->Bead codec, leaving these
// cmd/gc helpers as thin adapters that keep the methods callable inside the
// withNudgeQueueState transaction.
func nudgeFrontDoor(store beads.NudgesStore) *nudgequeue.Store {
	return nudgequeue.NewStore(store)
}

func ensureQueuedNudgeBead(store beads.NudgesStore, item queuedNudge) (string, bool, error) {
	return nudgeFrontDoor(store).Save(item)
}

func markQueuedNudgeTerminal(store beads.NudgesStore, item queuedNudge, state, reason, commitBoundary string, now time.Time) error {
	return nudgeFrontDoor(store).Terminalize(item, state, reason, commitBoundary, now)
}

func formatOptionalTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}
