package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// sleptEvents returns the captured events.SessionSlept events, in emission order.
func sleptEvents(rec *events.Fake) []events.Event {
	out := make([]events.Event, 0, len(rec.Events))
	for _, e := range rec.Events {
		if e.Type == events.SessionSlept {
			out = append(out, e)
		}
	}
	return out
}

func decodeSleptPayload(t *testing.T, e events.Event) events.SessionSleptPayload {
	t.Helper()
	var p events.SessionSleptPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("decoding SessionSleptPayload: %v", err)
	}
	return p
}

// TestReconcileSessionBeads_MaxSessionAgeKillEmitsSessionSlept pins ga-e5ygdf:
// before this, a max-age preemptive restart killed the runtime and rewrote the
// bead to state=asleep with zero observable signal (ga-v8mtlp finding F5 — 500
// events over a 3h window included no session.slept). The kill already emits
// SessionMaxAgeKilled; this asserts the sibling sleep-transition event also
// fires, with a payload describing the session's actual resolved policy.
func TestReconcileSessionBeads_MaxSessionAgeKillEmitsSessionSlept(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{InteractiveResume: "45m"},
		Agents:       []config.Agent{{Name: "witness", MaxSessionAge: "5h"}},
	}
	env.addDesired("witness", "witness", true)
	session := env.createSessionBead("witness", "witness")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"creation_complete_at": env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
	})

	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	rec := events.NewFake()
	env.rec = rec

	env.maxAgeReconcile([]beads.Bead{session}, tr)

	got := sleptEvents(rec)
	if len(got) != 1 {
		t.Fatalf("session.slept events = %d, want exactly 1", len(got))
	}
	if got[0].SessionID != session.ID {
		t.Errorf("SessionID = %q, want %q", got[0].SessionID, session.ID)
	}
	p := decodeSleptPayload(t, got[0])
	if p.Reason != "max-session-age" {
		t.Errorf("Reason = %q, want max-session-age", p.Reason)
	}
	if p.Template != "witness" {
		t.Errorf("Template = %q, want witness", p.Template)
	}
	if p.PolicyClass != "interactive_resume" {
		t.Errorf("PolicyClass = %q, want interactive_resume", p.PolicyClass)
	}
	if p.ResolvedTTL != "45m" {
		t.Errorf("ResolvedTTL = %q, want 45m", p.ResolvedTTL)
	}
	if p.ResolutionSource != config.SessionSleepSourceWorkspaceDefault {
		t.Errorf("ResolutionSource = %q, want %q", p.ResolutionSource, config.SessionSleepSourceWorkspaceDefault)
	}
}

// TestReconcileSessionBeads_MaxSessionAgeDeferredDoesNotEmitSessionSlept guards
// the negative arm: a max-age restart deferred by a held_until blocker (see
// TestReconcileSessionBeads_MaxSessionAgeRespectsUserHold) must not fabricate a
// sleep transition that never happened.
func TestReconcileSessionBeads_MaxSessionAgeDeferredDoesNotEmitSessionSlept(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "witness", MaxSessionAge: "5h"}}}
	env.addDesired("witness", "witness", true)
	session := env.createSessionBead("witness", "witness")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"creation_complete_at": env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
		"held_until":           env.clk.Now().Add(100 * time.Hour).UTC().Format(time.RFC3339),
	})

	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	rec := events.NewFake()
	env.rec = rec

	env.maxAgeReconcile([]beads.Bead{session}, tr)

	if got := sleptEvents(rec); len(got) != 0 {
		t.Fatalf("session.slept events = %d, want 0 while held_until defers the restart", len(got))
	}
}

// TestReconcileSessionBeads_IdleTimeoutKillEmitsSessionSlept mirrors the
// max-age case for the idle-timeout kill site (session_reconciler.go's other
// TimerActionStop branch) — the second of the two kill-triggered sleep
// transitions enumerated in the ga-e5ygdf investigation.
func TestReconcileSessionBeads_IdleTimeoutKillEmitsSessionSlept(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"pending_create_claim": "true",
		"sleep_intent":         "idle-stop-pending",
	})
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	it := newFakeIdleTracker()
	it.idle["worker"] = true
	rec := events.NewFake()
	env.rec = rec

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		it, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)

	got := sleptEvents(rec)
	if len(got) != 1 {
		t.Fatalf("session.slept events = %d, want exactly 1", len(got))
	}
	if got[0].SessionID != session.ID {
		t.Errorf("SessionID = %q, want %q", got[0].SessionID, session.ID)
	}
	p := decodeSleptPayload(t, got[0])
	if p.Reason != "idle-timeout" {
		t.Errorf("Reason = %q, want idle-timeout", p.Reason)
	}
	if p.Template != "worker" {
		t.Errorf("Template = %q, want worker", p.Template)
	}
}

// TestReconcileSessionBeads_RecoversPendingIdleSleepEmitsSessionSlept covers
// the third site: the idle-sleep engine's own drain-completion transition
// (recoverPendingIdleSleepInfo), which is the specific mechanism ga-v8mtlp's
// Arm A validated as working-but-silent (32 seats engine-slept, zero events).
func TestReconcileSessionBeads_RecoversPendingIdleSleepEmitsSessionSlept(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{InteractiveResume: "60s"},
		Agents:       []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	policy := resolveSessionSleepPolicyInfo(sessiontest.SeedBead(t, session), env.cfg, env.sp)
	lastWoke := env.clk.Time.Add(-10 * time.Second).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadataBatch(session.ID, map[string]string{
		"state":                    "active",
		"sleep_intent":             "idle-stop-pending",
		"sleep_policy_fingerprint": policy.Fingerprint,
		"last_woke_at":             lastWoke,
	})
	session.Metadata["state"] = "active"
	session.Metadata["sleep_intent"] = "idle-stop-pending"
	session.Metadata["sleep_policy_fingerprint"] = policy.Fingerprint
	session.Metadata["last_woke_at"] = lastWoke

	rec := events.NewFake()
	env.rec = rec

	if got := env.reconcile([]beads.Bead{session}); got != 0 {
		t.Fatalf("planned wakes = %d, want 0", got)
	}

	got := sleptEvents(rec)
	if len(got) != 1 {
		t.Fatalf("session.slept events = %d, want exactly 1", len(got))
	}
	if got[0].SessionID != session.ID {
		t.Errorf("SessionID = %q, want %q", got[0].SessionID, session.ID)
	}
	p := decodeSleptPayload(t, got[0])
	if p.Reason != "idle" {
		t.Errorf("Reason = %q, want idle", p.Reason)
	}
	if p.PolicyClass != "interactive_resume" {
		t.Errorf("PolicyClass = %q, want interactive_resume", p.PolicyClass)
	}
	if p.ResolvedTTL != "60s" {
		t.Errorf("ResolvedTTL = %q, want 60s", p.ResolvedTTL)
	}
}
