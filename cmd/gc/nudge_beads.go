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
// Hardcoded to the SAME 8s value as internal/nudgequeue/state.go's
// defaultLockWaitTimeout, deliberately not cross-package-referenced: that
// constant is unexported (a different package, cmd/gc cannot see it without
// exporting it, which this follow-on fix chose not to do to keep its diff
// confined to cmd/gc -- the same package this whole holder-side lives in).
// If defaultLockWaitTimeout's value ever changes, this one must change with
// it by hand; that coupling is documented here and at the other end.
const nudgeMaintenanceStoreOpenTimeout = 8 * time.Second

// openNudgeBeadStoreBounded calls openNudgeBeadStore (the existing test seam
// above) with a hard wall-clock bound. On timeout it returns the zero-value
// beads.NudgesStore{} -- exactly what openNudgeBeadStore itself already
// returns on an open error (see its own swallowed-error comment: "a nil
// store means do nothing"), so a slow-but-eventually-successful open
// degrades to "nothing to do this tick" via the SAME nil-tolerant contract
// every maintenance pass already respects, not a new failure shape.
//
// The losing goroutine is deliberately abandoned rather than canceled:
// openNudgeBeadStore has no cancellation hook to call, and closing a store
// handle out from under a goroutine that might still be using it would be
// worse than leaving it to finish (or fail) on its own. The result channel
// is buffered so that goroutine's eventual send never blocks on a caller
// who stopped listening.
func openNudgeBeadStoreBounded(cityPath string, timeout time.Duration) beads.NudgesStore {
	result := make(chan beads.NudgesStore, 1)
	go func() { result <- openNudgeBeadStore(cityPath) }()
	select {
	case store := <-result:
		return store
	case <-time.After(timeout):
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
