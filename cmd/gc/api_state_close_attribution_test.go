package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// closeAttributionControllerFixture wires a controllerState the way the
// convoy-autoclose tests do — a MemStore-backed CachingStore as the work store
// and the city store — with a [routing]-configured city and one session bead
// (a persona-nils pool seat) whose id a work bead can point at via
// gc.session_id. Dispatch is made synchronous so the stamp can be asserted
// right after applyBeadEventToStores returns.
type closeAttributionControllerFixture struct {
	backing *beads.MemStore
	cached  *beads.CachingStore
	cs      *controllerState
	stderr  *bytes.Buffer
}

func newCloseAttributionControllerFixture(t *testing.T) *closeAttributionControllerFixture {
	t.Helper()
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })

	backing := beads.NewMemStore()
	backing.HonorExplicitIDs = true
	if _, err := backing.Create(beads.Bead{
		ID:        "gc-sess1",
		Type:      session.BeadType,
		Status:    "open",
		Title:     "persona-nils-7",
		Labels:    []string{session.LabelSession},
		CreatedAt: time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC),
		Metadata: map[string]string{
			"template":     "persona-nils",
			"alias":        "persona-nils-7",
			"session_name": "persona-nils-ga-sess1",
			"provider":     "claude",
			"state":        "awake",
			"pool_managed": "true",
		},
	}); err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	cached := beads.NewCachingStoreForTest(backing, nil)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	stderr := &bytes.Buffer{}
	cs := &controllerState{
		cfg: &config.City{RoutingPolicy: config.RoutingPolicyConfig{
			RoutingExempt:              []config.RoutingExemptGroup{{Name: "officers", Personas: []string{"persona-marcus"}}},
			OfficerOfRecordValueDomain: []string{"persona-marcus", "operator"},
			ReportsTo:                  map[string]string{"persona-nils": "persona-marcus"},
		}},
		beadStores:             map[string]beads.Store{"test": cached},
		cityBeadStore:          cached,
		pokeCh:                 make(chan struct{}, 1),
		closeAttributionStderr: stderr,
	}
	return &closeAttributionControllerFixture{backing: backing, cached: cached, cs: cs, stderr: stderr}
}

// closeWork creates a work bead with meta, closes it in the backing store, syncs
// the cache, and fires the bead.closed event the reconciler would emit.
func (f *closeAttributionControllerFixture) closeWork(t *testing.T, meta map[string]string) beads.Bead {
	t.Helper()
	work, err := f.cached.Create(beads.Bead{Title: "work", Metadata: meta})
	if err != nil {
		t.Fatalf("Create work: %v", err)
	}
	if err := f.cached.Close(work.ID); err != nil {
		t.Fatalf("Close work: %v", err)
	}
	payload, err := json.Marshal(beads.Bead{ID: work.ID, Title: "work", Status: "closed", Type: "task", Metadata: meta})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	f.cs.applyBeadEventToStores(events.Event{
		Type:    events.BeadClosed,
		Actor:   cacheReconcileActor,
		Subject: work.ID,
		Payload: payload,
	})
	got, err := f.backing.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work: %v", err)
	}
	return got
}

func TestControllerStampsCloseAttributionFromSessionID(t *testing.T) {
	f := newCloseAttributionControllerFixture(t)
	got := f.closeWork(t, map[string]string{beadmeta.SessionIDMetadataKey: "gc-sess1"})
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "persona-nils" || got.Metadata[beadmeta.OfficerOfRecordMetadataKey] != "persona-marcus" {
		t.Fatalf("metadata = %v; stderr=%q", got.Metadata, f.stderr.String())
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q after stamp, want closed", got.Status)
	}
	if f.stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", f.stderr.String())
	}
}

func TestControllerCloseAttributionIsIdempotentWithClientStamp(t *testing.T) {
	f := newCloseAttributionControllerFixture(t)
	// The gc bd close projection already stamped both fields (with a different
	// routed_to, as an earlier legitimate router would): nothing is overwritten.
	got := f.closeWork(t, map[string]string{
		beadmeta.SessionIDMetadataKey:       "gc-sess1",
		beadmeta.RoutedToMetadataKey:        "persona-kieran",
		beadmeta.OfficerOfRecordMetadataKey: "operator",
	})
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "persona-kieran" || got.Metadata[beadmeta.OfficerOfRecordMetadataKey] != "operator" {
		t.Fatalf("existing stamps clobbered: %v", got.Metadata)
	}
}

func TestControllerCloseAttributionHonorsDeclaredExemption(t *testing.T) {
	f := newCloseAttributionControllerFixture(t)
	got := f.closeWork(t, map[string]string{
		beadmeta.SessionIDMetadataKey:           "gc-sess1",
		beadmeta.OfficerExemptReasonMetadataKey: "Tier-1 single-seat self-close",
	})
	if got.Metadata[beadmeta.OfficerOfRecordMetadataKey] != "" {
		t.Fatalf("declared exemption must suppress officer derivation: %v", got.Metadata)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "persona-nils" {
		t.Fatalf("exempt close still attributed to its closer: %v", got.Metadata)
	}
}

func TestControllerCloseAttributionUnresolvableSessionWritesNothing(t *testing.T) {
	f := newCloseAttributionControllerFixture(t)
	t.Run("no session back-reference (never claimed, raw bd close)", func(t *testing.T) {
		got := f.closeWork(t, nil)
		if len(got.Metadata) != 0 {
			t.Fatalf("metadata = %v, want none", got.Metadata)
		}
	})
	t.Run("dangling gc.session_id", func(t *testing.T) {
		got := f.closeWork(t, map[string]string{beadmeta.SessionIDMetadataKey: "gc-gone"})
		if got.Metadata[beadmeta.RoutedToMetadataKey] != "" {
			t.Fatalf("metadata = %v, want no attribution", got.Metadata)
		}
		if !strings.Contains(f.stderr.String(), "close-attribution") || !strings.Contains(f.stderr.String(), "gc-gone") {
			t.Fatalf("expected a diagnostic naming the dangling session, got %q", f.stderr.String())
		}
	})
}

func TestControllerCloseAttributionNoopsWhenRoutingNotConfigured(t *testing.T) {
	f := newCloseAttributionControllerFixture(t)
	f.cs.cfg = &config.City{}
	got := f.closeWork(t, map[string]string{beadmeta.SessionIDMetadataKey: "gc-sess1"})
	if len(got.Metadata) != 1 {
		t.Fatalf("opted-out city must not be stamped: %v", got.Metadata)
	}
}

func TestControllerCloseAttributionIgnoresNonCloseEvents(t *testing.T) {
	f := newCloseAttributionControllerFixture(t)
	work, err := f.cached.Create(beads.Bead{Title: "work", Metadata: map[string]string{beadmeta.SessionIDMetadataKey: "gc-sess1"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	payload, _ := json.Marshal(beads.Bead{ID: work.ID, Title: "work", Status: "in_progress", Type: "task"})
	f.cs.applyBeadEventToStores(events.Event{Type: events.BeadUpdated, Actor: cacheReconcileActor, Subject: work.ID, Payload: payload})
	got, _ := f.backing.Get(work.ID)
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		t.Fatalf("an update must not stamp close attribution: %v", got.Metadata)
	}
}
