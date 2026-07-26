package sling

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestAttachFormulaToBeadEntryShapes exercises the two attachment entry points
// that share attachFormulaToBead — --on-formula and default-formula — and
// pins the per-path pieces the wrappers select: the sling method and the
// error-label prefix ("formula" vs "default formula"). This is the drift the
// S13 consolidation eliminated: before the merge these copies could diverge
// independently, so the test asserts both success method and error prefix for
// each entry shape.
func TestAttachFormulaToBeadEntryShapes(t *testing.T) {
	newDeps := func(t *testing.T) (SlingDeps, string) {
		t.Helper()
		cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
		deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
		b, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		return deps, b.ID
	}

	t.Run("on-formula success", func(t *testing.T) {
		deps, beadID := newDeps(t)
		a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
		result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: beadID, OnFormula: "code-review"}, deps, deps.Store)
		if err != nil {
			t.Fatalf("DoSling on-formula: %v", err)
		}
		if result.Method != "on-formula" {
			t.Errorf("Method = %q, want on-formula", result.Method)
		}
		if result.FormulaName != "code-review" {
			t.Errorf("FormulaName = %q, want code-review", result.FormulaName)
		}
		if result.WispRootID == "" {
			t.Error("expected non-empty WispRootID")
		}
	})

	t.Run("default-formula success", func(t *testing.T) {
		deps, beadID := newDeps(t)
		a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("code-review")}
		result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: beadID}, deps, deps.Store)
		if err != nil {
			t.Fatalf("DoSling default-formula: %v", err)
		}
		if result.Method != "default-on-formula" {
			t.Errorf("Method = %q, want default-on-formula", result.Method)
		}
		if result.FormulaName != "code-review" {
			t.Errorf("FormulaName = %q, want code-review", result.FormulaName)
		}
		if result.WispRootID == "" {
			t.Error("expected non-empty WispRootID")
		}
	})

	t.Run("on-formula error label", func(t *testing.T) {
		deps, beadID := newDeps(t)
		a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
		_, err := DoSling(SlingOpts{Target: a, BeadOrFormula: beadID, OnFormula: "nonexistent-formula"}, deps, deps.Store)
		if err == nil {
			t.Fatal("expected instantiation error for nonexistent on-formula")
		}
		if want := `instantiating formula "nonexistent-formula" on`; !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want prefix %q", err.Error(), want)
		}
	})

	t.Run("default-formula error label", func(t *testing.T) {
		deps, beadID := newDeps(t)
		a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("nonexistent-formula")}
		_, err := DoSling(SlingOpts{Target: a, BeadOrFormula: beadID}, deps, deps.Store)
		if err == nil {
			t.Fatal("expected instantiation error for nonexistent default formula")
		}
		if want := `instantiating default formula "nonexistent-formula" on`; !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want prefix %q", err.Error(), want)
		}
	})
}

// TestCheckTargetDispatchable reproduces the ga-96zjze shape (ga-tk5mcg.2):
// gc sling stamping gc.routed_to onto a target whose status/defer state
// keeps it invisible to Ready()'s pool-demand probe, reporting success on a
// dispatch that will never be delivered. Because a non-nil preflight error
// short-circuits DoSling before finalize (the only place gc.routed_to is
// written — see DoSling's switch), a typed *NonDispatchableTargetError here
// is itself the proof gc.routed_to was never touched: asserting only that
// DoSling returned *some* error would not distinguish this from an unrelated
// failure, so each case checks the concrete error type.
func TestCheckTargetDispatchable(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	newDeps := func(seed beads.Bead) SlingDeps {
		deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), newFakeRunner().run)
		if seed.Metadata == nil {
			seed.Metadata = map[string]string{}
		}
		deps.Store = beads.NewMemStoreFrom(0, []beads.Bead{seed}, nil)
		return deps
	}

	t.Run("refuses indefinitely deferred bead", func(t *testing.T) {
		deps := newDeps(beads.Bead{ID: "BL-1", Title: "stuck", Type: "task", Status: "deferred"})

		_, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "BL-1"}, deps, deps.Store)
		var nde *NonDispatchableTargetError
		if !errors.As(err, &nde) {
			t.Fatalf("DoSling err = %v (%T), want *NonDispatchableTargetError", err, err)
		}
		if !strings.Contains(nde.Status, "deferred indefinitely") {
			t.Errorf("Status = %q, want to mention indefinite defer", nde.Status)
		}
	})

	t.Run("refuses closed bead", func(t *testing.T) {
		deps := newDeps(beads.Bead{ID: "BL-1", Title: "done", Type: "task", Status: "closed"})

		_, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "BL-1"}, deps, deps.Store)
		var nde *NonDispatchableTargetError
		if !errors.As(err, &nde) {
			t.Fatalf("DoSling err = %v (%T), want *NonDispatchableTargetError", err, err)
		}
		if nde.Status != "closed" {
			t.Errorf("Status = %q, want %q", nde.Status, "closed")
		}
	})

	t.Run("refuses future-dated defer window", func(t *testing.T) {
		deps := newDeps(beads.Bead{ID: "BL-1", Title: "snoozed", Type: "task", Status: "deferred", DeferUntil: &future})

		_, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "BL-1"}, deps, deps.Store)
		var nde *NonDispatchableTargetError
		if !errors.As(err, &nde) {
			t.Fatalf("DoSling err = %v (%T), want *NonDispatchableTargetError", err, err)
		}
	})

	t.Run("allows expired defer window to resurface", func(t *testing.T) {
		deps := newDeps(beads.Bead{ID: "BL-1", Title: "resurfaced", Type: "task", Status: "deferred", DeferUntil: &past})

		if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "BL-1"}, deps, deps.Store); err != nil {
			t.Fatalf("DoSling: %v", err)
		}
	})

	t.Run("allows open bead", func(t *testing.T) {
		deps := newDeps(beads.Bead{ID: "BL-1", Title: "fresh", Type: "task", Status: "open"})

		if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "BL-1"}, deps, deps.Store); err != nil {
			t.Fatalf("DoSling: %v", err)
		}
	})

	t.Run("allows in_progress bead (reassign target)", func(t *testing.T) {
		deps := newDeps(beads.Bead{ID: "BL-1", Title: "in flight", Type: "task", Status: "in_progress"})

		if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "BL-1"}, deps, deps.Store); err != nil {
			t.Fatalf("DoSling: %v", err)
		}
	})

	t.Run("--force does not bypass the refusal", func(t *testing.T) {
		deps := newDeps(beads.Bead{ID: "BL-1", Title: "stuck", Type: "task", Status: "deferred"})

		_, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "BL-1", Force: true}, deps, deps.Store)
		var nde *NonDispatchableTargetError
		if !errors.As(err, &nde) {
			t.Fatalf("DoSling with --force err = %v (%T), want *NonDispatchableTargetError (no override for this check)", err, err)
		}
	})
}
