package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestCheckSpawnPreflightGate_HealthyStateProceeds pins the common case: no
// supervisor running (as in most unit tests / a fresh city) and no aged
// pending-creates in the store means the gate never blocks.
func TestCheckSpawnPreflightGate_HealthyStateProceeds(t *testing.T) {
	oldAlive := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = oldAlive })
	supervisorAliveHook = func() int { return 0 }

	store := beads.NewMemStore()
	res := checkSpawnPreflightGate(store, false, nil, "")
	if res.Blocked {
		t.Fatalf("healthy state blocked: reason=%q fixCmd=%q", res.Reason, res.FixCmd)
	}
}

// TestCheckSpawnPreflightGate_StaleBinaryBlocks pins the P2 spec's first trip
// condition: supervisor.BuildID != local commit refuses the dispatch.
func TestCheckSpawnPreflightGate_StaleBinaryBlocks(t *testing.T) {
	_, setCommit := driftCheckEnv(t, "supervisor-build-id")
	setCommit("local-build-id") // differs from the supervisor's reported build_id

	store := beads.NewMemStore()
	res := checkSpawnPreflightGate(store, false, nil, "")
	if !res.Blocked {
		t.Fatal("stale binary did not trip the gate")
	}
	if !strings.Contains(res.Reason, "stale binary") {
		t.Errorf("Reason = %q, want mention of stale binary", res.Reason)
	}
	if res.FixCmd == "" {
		t.Error("FixCmd empty, want a concrete restart command")
	}
}

// TestCheckSpawnPreflightGate_AgedPendingCreatesBlocks pins the P2 spec's
// second trip condition: a pending-create session already past
// pendingCreateNeverStartedTimeout (one full lease) refuses the dispatch,
// even with no supervisor drift.
func TestCheckSpawnPreflightGate_AgedPendingCreatesBlocks(t *testing.T) {
	oldAlive := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = oldAlive })
	supervisorAliveHook = func() int { return 0 }

	store := beads.NewMemStore()
	agedStart := time.Now().Add(-(pendingCreateNeverStartedTimeout + time.Minute)).UTC().Format(time.RFC3339)
	if _, err := store.Create(beads.Bead{
		Title:  "aged pending create",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":              "agent-aged-pending",
			"template":                  "template-aged",
			"pending_create_claim":      "true",
			"pending_create_started_at": agedStart,
		},
	}); err != nil {
		t.Fatalf("seed aged pending-create bead: %v", err)
	}

	res := checkSpawnPreflightGate(store, false, nil, "")
	if !res.Blocked {
		t.Fatal("aged pending-create did not trip the gate")
	}
	if !strings.Contains(res.Reason, "pending-create") {
		t.Errorf("Reason = %q, want mention of pending-create", res.Reason)
	}
}

// TestCheckSpawnPreflightGate_FreshPendingCreateProceeds is the negative
// control: a pending-create still well within its lease must not trip the
// gate.
func TestCheckSpawnPreflightGate_FreshPendingCreateProceeds(t *testing.T) {
	oldAlive := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = oldAlive })
	supervisorAliveHook = func() int { return 0 }

	store := beads.NewMemStore()
	freshStart := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	if _, err := store.Create(beads.Bead{
		Title:  "fresh pending create",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":              "agent-fresh-pending",
			"template":                  "template-fresh",
			"pending_create_claim":      "true",
			"pending_create_started_at": freshStart,
		},
	}); err != nil {
		t.Fatalf("seed fresh pending-create bead: %v", err)
	}

	res := checkSpawnPreflightGate(store, false, nil, "")
	if res.Blocked {
		t.Fatalf("fresh pending-create blocked: reason=%q", res.Reason)
	}
}

// TestCheckSpawnPreflightGate_ForceDegradedBypassesBoth pins the escape
// hatch: --force-degraded must skip all three checks even when more than
// one would otherwise trip.
func TestCheckSpawnPreflightGate_ForceDegradedBypassesBoth(t *testing.T) {
	_, setCommit := driftCheckEnv(t, "supervisor-build-id")
	setCommit("local-build-id")

	store := beads.NewMemStore()
	agedStart := time.Now().Add(-(pendingCreateNeverStartedTimeout + time.Minute)).UTC().Format(time.RFC3339)
	if _, err := store.Create(beads.Bead{
		Title:  "aged pending create",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":              "agent-aged-pending-2",
			"template":                  "template-aged-2",
			"pending_create_claim":      "true",
			"pending_create_started_at": agedStart,
		},
	}); err != nil {
		t.Fatalf("seed aged pending-create bead: %v", err)
	}

	res := checkSpawnPreflightGate(store, true, nil, "")
	if res.Blocked {
		t.Fatalf("--force-degraded did not bypass the gate: reason=%q", res.Reason)
	}
}
