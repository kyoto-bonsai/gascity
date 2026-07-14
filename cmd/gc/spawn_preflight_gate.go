// spawn_preflight_gate.go implements the P2 fail-closed spawn preflight gate
// from doctrine/proposal-enforcement-hardening-2026-07-13.md (ga-6l32x0):
// refuse `gc session new` / `gc sling` dispatches when the running supervisor
// is on a stale binary or the pending-create queue already has entries past
// their lease, instead of letting the create silently vanish the way
// ga-ptm6dm's incident did. --force-degraded overrides either trip condition.
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

// spawnPreflightResult reports whether a create/sling dispatch should be
// refused before it starts.
type spawnPreflightResult struct {
	Blocked bool
	Reason  string
	FixCmd  string
}

// checkSpawnPreflightGate runs both P2 checks: (a) supervisor binary
// staleness via the same BuildID comparison gc start's drift check uses,
// (b) count of pending-create sessions already past
// pendingCreateNeverStartedTimeout (one full lease). Either trips the gate
// unless forceDegraded is set. Read-only: never mutates session or store
// state — repair stays the reconciler's/watchdog's job.
func checkSpawnPreflightGate(store beads.Store, forceDegraded bool) spawnPreflightResult {
	if forceDegraded {
		return spawnPreflightResult{}
	}
	if res := checkSupervisorBinaryStaleness(); res.Blocked {
		return res
	}
	return checkAgedPendingCreates(store)
}

// checkSupervisorBinaryStaleness compares the running supervisor's reported
// BuildID (via its /health status) against this binary's own build commit
// (the same `commit` ldflags variable `gc version` and `gc start`'s drift
// check use). Fails open (never blocks) when there's no supervisor to compare
// against, no reachable API, or an unresponsive supervisor — those are
// pre-existing, separately-surfaced conditions, not this gate's job.
func checkSupervisorBinaryStaleness() spawnPreflightResult {
	pid := supervisorAliveHook()
	if pid == 0 {
		return spawnPreflightResult{}
	}
	baseURL, err := supervisorAPIBaseURLHook()
	if err != nil {
		return spawnPreflightResult{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := newHTTPSupervisorClient(baseURL).Status(ctx)
	if err != nil {
		return spawnPreflightResult{}
	}
	if !DetectBinaryDrift(commit, status) {
		return spawnPreflightResult{}
	}
	return spawnPreflightResult{
		Blocked: true,
		Reason:  fmt.Sprintf("supervisor is running a stale binary (supervisor=%s local=%s)", status.BuildID, commit),
		FixCmd:  supervisorRestartGuidance(),
	}
}

// checkAgedPendingCreates counts pending-create sessions that have already
// exceeded pendingCreateNeverStartedTimeout (one full lease) — the ga-ptm6dm
// signature of a reconciler deferring starts faster than it can land them. A
// nil store or a snapshot-load error fails open: this gate augments the
// create path, it doesn't replace the store's own error surfacing.
func checkAgedPendingCreates(store beads.Store) spawnPreflightResult {
	if store == nil {
		return spawnPreflightResult{}
	}
	snap, err := loadSessionBeadSnapshot(store)
	if err != nil {
		return spawnPreflightResult{}
	}
	clk := clock.Real{}
	aged := 0
	for _, info := range snap.OpenInfos() {
		if info.PendingCreateClaim && pendingCreateNeverStartedLeaseExpiredInfo(info, clk) {
			aged++
		}
	}
	if aged == 0 {
		return spawnPreflightResult{}
	}
	return spawnPreflightResult{
		Blocked: true,
		Reason:  fmt.Sprintf("%d pending-create session(s) already past the %s lease — reconciler may be stuck", aged, pendingCreateNeverStartedTimeout),
		FixCmd:  "gc supervisor reload",
	}
}

// spawnPreflightRefusalMessage formats the one-line refusal + fix command +
// override hint. cmdName is the failing command's own error prefix (e.g.
// "gc session new", "gc sling") to match each command's existing stderr
// style.
func spawnPreflightRefusalMessage(cmdName string, res spawnPreflightResult) string {
	return fmt.Sprintf("%s: refused — %s. Fix: %s. Override with --force-degraded if you understand the risk.", cmdName, res.Reason, res.FixCmd)
}
