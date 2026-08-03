package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestCommitStartResult_SessionResumedOnlyOnGenuineResume pins ga-e5ygdf: before
// this, session.woke fired identically for a brand-new session's first-ever
// start and a genuine resume-from-sleep (ga-v8mtlp finding F5 established the
// conflation with three independent proofs — a start-pending create, a
// never-slept fresh spawn, and wake events on a template that cannot sleep at
// all). session.resumed is additive and must fire ONLY when
// preparedStart.resumedFromSleep is true, alongside — never instead of —
// session.woke, so existing session.woke consumers see no change either way.
func TestCommitStartResult_SessionResumedOnlyOnGenuineResume(t *testing.T) {
	buildResult := func(session *beads.Bead, resumed bool) startResult {
		return startResult{
			prepared: preparedStart{
				candidate: startCandidate{
					info: sessiontest.SeedBead(t, *session),
					tp: TemplateParams{
						SessionName:  "sky",
						TemplateName: "helper",
					},
				},
				coreHash:         "core",
				liveHash:         "live",
				resumedFromSleep: resumed,
			},
			outcome:  "success",
			started:  time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC),
			finished: time.Date(2026, 3, 18, 12, 0, 1, 0, time.UTC),
		}
	}
	sessionMeta := func() map[string]string {
		return map[string]string{
			"session_name": "sky",
			"state":        "creating",
		}
	}
	clk := &clock.Fake{Time: time.Date(2026, 3, 18, 12, 0, 1, 0, time.UTC)}

	t.Run("fresh spawn: session.woke fires, session.resumed does not", func(t *testing.T) {
		store := beads.NewMemStore()
		session, err := store.Create(beads.Bead{
			Title:    "helper",
			Type:     sessionBeadType,
			Labels:   []string{sessionBeadLabel},
			Metadata: sessionMeta(),
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := events.NewFake()
		if !commitStartResult(buildResult(&session, false), sessionFrontDoor(store), clk, rec, 0, ioDiscard{}, ioDiscard{}) {
			t.Fatal("commitStartResult returned false for successful start")
		}
		if n := countEventType(rec, events.SessionWoke); n != 1 {
			t.Fatalf("session.woke events = %d, want exactly 1", n)
		}
		if n := countEventType(rec, events.SessionResumed); n != 0 {
			t.Fatalf("session.resumed events = %d, want 0 for a first-ever start (resumedFromSleep=false)", n)
		}
	})

	t.Run("genuine resume: both session.woke and session.resumed fire", func(t *testing.T) {
		store := beads.NewMemStore()
		session, err := store.Create(beads.Bead{
			Title:    "helper",
			Type:     sessionBeadType,
			Labels:   []string{sessionBeadLabel},
			Metadata: sessionMeta(),
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := events.NewFake()
		if !commitStartResult(buildResult(&session, true), sessionFrontDoor(store), clk, rec, 0, ioDiscard{}, ioDiscard{}) {
			t.Fatal("commitStartResult returned false for successful start")
		}
		if n := countEventType(rec, events.SessionWoke); n != 1 {
			t.Fatalf("session.woke events = %d, want exactly 1 (must still fire unconditionally)", n)
		}
		if n := countEventType(rec, events.SessionResumed); n != 1 {
			t.Fatalf("session.resumed events = %d, want exactly 1 for a genuine resume (resumedFromSleep=true)", n)
		}
		woke, err := rec.List(context.Background(), events.Filter{Type: events.SessionResumed})
		if err != nil {
			t.Fatal(err)
		}
		if len(woke) != 1 || woke[0].SessionID != session.ID {
			t.Fatalf("session.resumed SessionID = %+v, want %q", woke, session.ID)
		}
	})

	t.Run("metadata batch failure suppresses both events", func(t *testing.T) {
		store := &failingMetadataBatchStore{MemStore: beads.NewMemStore(), failBatch: true}
		session, err := store.Create(beads.Bead{
			Title:    "helper",
			Type:     sessionBeadType,
			Labels:   []string{sessionBeadLabel},
			Metadata: sessionMeta(),
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := events.NewFake()
		if commitStartResult(buildResult(&session, true), sessionFrontDoor(store), clk, rec, 0, ioDiscard{}, ioDiscard{}) {
			t.Fatal("commitStartResult returned true, want false when metadata batch fails")
		}
		if n := countEventType(rec, events.SessionResumed); n != 0 {
			t.Fatalf("session.resumed events = %d, want 0 when the durable commit failed (mirrors session.woke's own suppression)", n)
		}
	})
}

func countEventType(rec *events.Fake, typ string) int {
	n := 0
	for _, e := range rec.Events {
		if e.Type == typ {
			n++
		}
	}
	return n
}
