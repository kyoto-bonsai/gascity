package sling

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestCheckTargetAssigneeConflict reproduces the ga-tk5mcg.9 shape: gc sling
// stamping gc.routed_to onto an in_progress bead that already has a
// different assignee, reporting success on a dispatch the pool-demand probe
// (bd ready --metadata-field gc.routed_to=<target> --unassigned) can never
// return, because --unassigned excludes any assigned bead regardless of
// status or gc.routed_to. checkTargetDispatchable alone does not catch this:
// beads.IsStatusDispatchable deliberately treats in_progress as
// dispatchable (a live --reassign target is legitimate), so the gap only
// shows up on the assignee axis. As with TestCheckTargetDispatchable above,
// a non-nil preflight error short-circuits DoSling before finalize (the
// only place gc.routed_to is written), so a typed *NonDispatchableTargetError
// here is itself the proof gc.routed_to was never touched.
func TestCheckTargetAssigneeConflict(t *testing.T) {
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	newDeps := func(seed beads.Bead) SlingDeps {
		deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), newFakeRunner().run)
		if seed.Metadata == nil {
			seed.Metadata = map[string]string{}
		}
		deps.Store = beads.NewMemStoreFrom(0, []beads.Bead{seed}, nil)
		return deps
	}

	t.Run("refuses in_progress bead assigned to someone else", func(t *testing.T) {
		deps := newDeps(beads.Bead{ID: "BL-1", Title: "claimed", Type: "task", Status: "in_progress", Assignee: "persona-daedalus"})

		_, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "BL-1"}, deps, deps.Store)
		var nde *NonDispatchableTargetError
		if !errors.As(err, &nde) {
			t.Fatalf("DoSling err = %v (%T), want *NonDispatchableTargetError", err, err)
		}
		if !strings.Contains(nde.Status, "persona-daedalus") {
			t.Errorf("Status = %q, want to name the current assignee", nde.Status)
		}
		if !strings.Contains(nde.Fix, "--reassign") {
			t.Errorf("Fix = %q, want to mention --reassign", nde.Fix)
		}
	})

	t.Run("allows in_progress bead already assigned to the same target", func(t *testing.T) {
		deps := newDeps(beads.Bead{ID: "BL-1", Title: "self", Type: "task", Status: "in_progress", Assignee: "mayor"})

		if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "BL-1"}, deps, deps.Store); err != nil {
			t.Fatalf("DoSling: %v", err)
		}
	})

	t.Run("allows in_progress bead with no assignee", func(t *testing.T) {
		deps := newDeps(beads.Bead{ID: "BL-1", Title: "unassigned", Type: "task", Status: "in_progress"})

		if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "BL-1"}, deps, deps.Store); err != nil {
			t.Fatalf("DoSling: %v", err)
		}
	})

	t.Run("allows open bead with an unrelated assignee (status axis only gates in_progress)", func(t *testing.T) {
		deps := newDeps(beads.Bead{ID: "BL-1", Title: "reopened", Type: "task", Status: "open", Assignee: "persona-daedalus"})

		if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "BL-1"}, deps, deps.Store); err != nil {
			t.Fatalf("DoSling: %v", err)
		}
	})

	t.Run("allows re-slinging a bead a live member of the same target pool already holds", func(t *testing.T) {
		// Regression guard for a real false-positive this check produced
		// during development: assignee "mayor-live-1" and target "mayor" are
		// the same owner in different string forms (a specific session
		// instance vs. its pool/config name), not a conflict. Caught by
		// TestDoSlingLiveRoutingConflictSameBeadIsNotAConflict in
		// cmd/gc/cmd_sling_test.go, reproduced narrowly here at the
		// checkTargetAssigneeConflict level.
		deps := newDeps(beads.Bead{ID: "BL-1", Title: "already mine", Type: "task", Status: "in_progress", Assignee: "mayor-live-1"})
		if _, err := deps.Store.Create(beads.Bead{
			Title:  "mayor-live-1",
			Type:   "session",
			Status: "open",
			Labels: []string{"gc:session"},
			Metadata: map[string]string{
				"template":     "mayor",
				"session_name": "mayor-live-1",
				"state":        "active",
			},
		}); err != nil {
			t.Fatalf("seeding live session bead: %v", err)
		}

		if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "BL-1"}, deps, deps.Store); err != nil {
			t.Fatalf("DoSling: %v", err)
		}
	})

	t.Run("--reassign bypasses the refusal and reopenForReassign clears it for real", func(t *testing.T) {
		deps := newDeps(beads.Bead{ID: "BL-1", Title: "handoff", Type: "task", Status: "in_progress", Assignee: "persona-daedalus"})

		if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "BL-1", Reassign: true}, deps, deps.Store); err != nil {
			t.Fatalf("DoSling --reassign: %v", err)
		}
		got, err := deps.Store.Get("BL-1")
		if err != nil {
			t.Fatalf("store.Get: %v", err)
		}
		if got.Assignee != "" {
			t.Errorf("Assignee = %q, want empty after --reassign", got.Assignee)
		}
		if got.Status != "open" {
			t.Errorf("Status = %q, want open after --reassign", got.Status)
		}
	})

	t.Run("--force does not bypass the refusal", func(t *testing.T) {
		deps := newDeps(beads.Bead{ID: "BL-1", Title: "claimed", Type: "task", Status: "in_progress", Assignee: "persona-daedalus"})

		_, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "BL-1", Force: true}, deps, deps.Store)
		var nde *NonDispatchableTargetError
		if !errors.As(err, &nde) {
			t.Fatalf("DoSling with --force err = %v (%T), want *NonDispatchableTargetError (no override for this check)", err, err)
		}
	})
}
