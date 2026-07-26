package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestDeriveOfficerOfRecord covers the ga-owbb42 "fix at the source" gate:
// gc sling should stamp gc.officer_of_record from RoutingPolicy.ReportsTo
// before checkOfficerOfRecord evaluates, so a mapped persona never trips the
// refusal that checkOfficerOfRecord (ga-ui3tes) already enforces.
func TestDeriveOfficerOfRecord(t *testing.T) {
	newDepsWithPolicy := func(t *testing.T, policy config.RoutingPolicyConfig) (SlingDeps, string) {
		t.Helper()
		cfg := &config.City{Workspace: config.Workspace{Name: "test"}, RoutingPolicy: policy}
		deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
		b, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		return deps, b.ID
	}

	t.Run("mapped persona gets stamped and dispatch is not refused", func(t *testing.T) {
		policy := config.RoutingPolicyConfig{
			RoutingExempt: []config.RoutingExemptGroup{
				{Name: "officers", Personas: []string{"persona-marcus"}},
			},
			ReportsTo: map[string]string{"persona-kieran": "persona-marcus"},
		}
		deps, beadID := newDepsWithPolicy(t, policy)
		a := config.Agent{Name: "persona-kieran", MaxActiveSessions: intPtr(1)}

		if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: beadID}, deps, deps.Store); err != nil {
			t.Fatalf("DoSling: unexpected refusal after derive should have stamped officer_of_record: %v", err)
		}

		b, err := deps.Store.Get(beadID)
		if err != nil {
			t.Fatalf("re-fetching bead: %v", err)
		}
		if got := b.Metadata[beadmeta.OfficerOfRecordMetadataKey]; got != "persona-marcus" {
			t.Errorf("gc.officer_of_record = %q, want %q", got, "persona-marcus")
		}
	})

	t.Run("unmapped persona still hits the fail-closed gate", func(t *testing.T) {
		policy := config.RoutingPolicyConfig{
			RoutingExempt: []config.RoutingExemptGroup{
				{Name: "officers", Personas: []string{"persona-marcus"}},
			},
			ReportsTo: map[string]string{"persona-kieran": "persona-marcus"},
		}
		deps, beadID := newDepsWithPolicy(t, policy)
		// persona-nils has no ReportsTo entry in this policy -- derive must not
		// invent a value, and the pre-existing gate must still refuse exactly
		// as it did before this feature existed.
		a := config.Agent{Name: "persona-nils", MaxActiveSessions: intPtr(1)}

		_, err := DoSling(SlingOpts{Target: a, BeadOrFormula: beadID}, deps, deps.Store)
		if err == nil {
			t.Fatal("expected MissingOfficerOfRecordError for an unmapped, non-exempt persona")
		}
		if _, ok := err.(*MissingOfficerOfRecordError); !ok {
			t.Errorf("error = %T (%v), want *MissingOfficerOfRecordError", err, err)
		}
	})

	t.Run("never overwrites an already-stamped value", func(t *testing.T) {
		policy := config.RoutingPolicyConfig{
			ReportsTo: map[string]string{"persona-kieran": "persona-marcus"},
		}
		deps, beadID := newDepsWithPolicy(t, policy)
		if err := deps.Store.SetMetadata(beadID, beadmeta.OfficerOfRecordMetadataKey, "operator"); err != nil {
			t.Fatal(err)
		}
		a := config.Agent{Name: "persona-kieran", MaxActiveSessions: intPtr(1)}

		if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: beadID}, deps, deps.Store); err != nil {
			t.Fatalf("DoSling: unexpected error: %v", err)
		}

		b, err := deps.Store.Get(beadID)
		if err != nil {
			t.Fatalf("re-fetching bead: %v", err)
		}
		if got := b.Metadata[beadmeta.OfficerOfRecordMetadataKey]; got != "operator" {
			t.Errorf("gc.officer_of_record = %q, want unchanged %q", got, "operator")
		}
	})

	t.Run("exempt persona is never looked up in ReportsTo", func(t *testing.T) {
		policy := config.RoutingPolicyConfig{
			RoutingExempt: []config.RoutingExemptGroup{
				{Name: "officers", Personas: []string{"persona-marcus"}},
			},
			// Deliberately no ReportsTo entry for persona-marcus (mirrors the
			// real city.toml: exempt personas are never given a ReportsTo
			// entry since Exempt() short-circuits before it would be read).
		}
		deps, beadID := newDepsWithPolicy(t, policy)
		a := config.Agent{Name: "persona-marcus", MaxActiveSessions: intPtr(1)}

		if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: beadID}, deps, deps.Store); err != nil {
			t.Fatalf("DoSling: exempt persona should never be refused: %v", err)
		}
		b, err := deps.Store.Get(beadID)
		if err != nil {
			t.Fatalf("re-fetching bead: %v", err)
		}
		if got := b.Metadata[beadmeta.OfficerOfRecordMetadataKey]; got != "" {
			t.Errorf("gc.officer_of_record = %q, want empty for an exempt persona", got)
		}
	})

	t.Run("policy not configured is a no-op, matching pre-feature behavior", func(t *testing.T) {
		deps, beadID := newDepsWithPolicy(t, config.RoutingPolicyConfig{})
		a := config.Agent{Name: "persona-kieran", MaxActiveSessions: intPtr(1)}

		if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: beadID}, deps, deps.Store); err != nil {
			t.Fatalf("DoSling: unconfigured RoutingPolicy must never refuse: %v", err)
		}
		b, err := deps.Store.Get(beadID)
		if err != nil {
			t.Fatalf("re-fetching bead: %v", err)
		}
		if got := b.Metadata[beadmeta.OfficerOfRecordMetadataKey]; got != "" {
			t.Errorf("gc.officer_of_record = %q, want empty when RoutingPolicy is unconfigured", got)
		}
	})
}
