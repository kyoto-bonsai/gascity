package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// ga-luwtw4: RETIRE-at-close for wisps. This suite is deliberately structured
// around the bead's own mandatory 3-arm bar (idle-and-finished /
// idle-and-frozen / active — all three demonstrated, not just the
// destructive-path arm), then goes further and isolates each individual
// safety gate so a regression in any one of them fails a specific,
// attributable test rather than only the headline retire case. Prior burns
// on this exact substrate (dead-assignee sweeper cleared live claims;
// provider-outage breaker selected the complement of its target) are why
// this suite treats "does not retire live/claimed/frozen work" with equal
// weight to "does retire genuinely closed-molecule wisps."

func ephemeralWispBead(sessionName, triggerBeadID string) beads.Bead {
	const template = "worker" // the only agent wispCfg() configures
	md := map[string]string{
		"session_name":         sessionName,
		"template":             template,
		"agent_name":           template,
		"pool_slot":            "1",
		poolManagedMetadataKey: boolMetadata(true),
		"state":                "asleep",
		"continuation_epoch":   "1",
		"generation":           "1",
	}
	if triggerBeadID != "" {
		md[beadmeta.TriggerBeadIDMetadataKey] = triggerBeadID
	}
	return beads.Bead{
		Title:    "wisp session",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel, "agent:" + template},
		Metadata: md,
	}
}

func wispCfg() *config.City {
	return &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}
}

func infoWithTrigger(triggerID, storeRef string) sessionpkg.Info {
	return sessionpkg.Info{TriggerBeadID: triggerID, TriggerBeadStoreRef: storeRef}
}

// --- Arm 1: idle-and-finished — the destructive path itself ---

func TestSweepClosedTriggerWispSessions_RetiresClosedTriggerIdleFinishedWisp(t *testing.T) {
	store := beads.NewMemStore()
	trigger, err := store.Create(beads.Bead{Title: "molecule step", Status: "open"})
	if err != nil {
		t.Fatalf("Create trigger: %v", err)
	}
	if err := store.Close(trigger.ID); err != nil {
		t.Fatalf("Close trigger: %v", err)
	}

	sess, err := store.Create(ephemeralWispBead("wisp-fin-1", trigger.ID))
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	rec := events.NewFake()
	closed := sweepClosedTriggerWispSessions(
		store, nil, newSessionBeadSnapshot([]beads.Bead{sess}), wispCfg(),
		runtime.NewFake(), false, rec, nil,
	)
	if len(closed) != 1 || closed[0] != sess.ID {
		t.Fatalf("closed = %v, want [%s]", closed, sess.ID)
	}
	got, err := store.Get(sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("session status = %q, want closed", got.Status)
	}

	if len(rec.Events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(rec.Events))
	}
	ev := rec.Events[0]
	if ev.Type != events.SessionWispRetired {
		t.Errorf("event type = %q, want %q", ev.Type, events.SessionWispRetired)
	}
	if ev.Subject != sess.ID || ev.SessionID != sess.ID {
		t.Errorf("event subject/session_id = %q/%q, want %s", ev.Subject, ev.SessionID, sess.ID)
	}
	wantPayload := fmt.Sprintf(`{"session_id":%q,"trigger_bead_id":%q,"template":"worker"}`, sess.ID, trigger.ID)
	if string(ev.Payload) != wantPayload {
		t.Errorf("event payload = %s, want %s", ev.Payload, wantPayload)
	}
}

// --- Arm 2: active — a live process must never be retired ---

func TestSweepClosedTriggerWispSessions_KeepsActiveRunningSessionAlive(t *testing.T) {
	store := beads.NewMemStore()
	trigger, _ := store.Create(beads.Bead{Title: "molecule step", Status: "open"})
	_ = store.Close(trigger.ID)

	sess, err := store.Create(ephemeralWispBead("wisp-active-1", trigger.ID))
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	// No assigned-work bead exists at all — the ga-imla7x shape: a seat can be
	// genuinely alive and busy (an unanswered interactive decision, mid-turn
	// exploratory work) with nothing bead-shaped to show for it. The liveness
	// gate must protect this case on its own, independent of any claim check.
	sp := &sweepLivenessProvider{Fake: runtime.NewFake(), running: map[string]bool{"wisp-active-1": true}}

	closed := sweepClosedTriggerWispSessions(
		store, nil, newSessionBeadSnapshot([]beads.Bead{sess}), wispCfg(),
		sp, false, nil, nil,
	)
	if len(closed) != 0 {
		t.Fatalf("closed = %v, want none: a running session must never be retired even with a closed trigger and no assigned-work bead", closed)
	}
	got, _ := store.Get(sess.ID)
	if got.Status == "closed" {
		t.Fatal("active session was closed")
	}
}

// --- Arm 3: idle-and-frozen — two independent sub-shapes ---

// Frozen-shape A: not running, but the trigger bead itself is still open —
// the molecule's work is not actually done, only the process happens to be
// down right now (crash, transient outage). Must not be mistaken for
// "finished."
func TestSweepClosedTriggerWispSessions_KeepsFrozenSessionWithOpenTriggerAlive(t *testing.T) {
	store := beads.NewMemStore()
	trigger, err := store.Create(beads.Bead{Title: "molecule step", Status: "open"})
	if err != nil {
		t.Fatalf("Create trigger: %v", err)
	}
	// Deliberately NOT closed.

	sess, err := store.Create(ephemeralWispBead("wisp-frozen-a", trigger.ID))
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	closed := sweepClosedTriggerWispSessions(
		store, nil, newSessionBeadSnapshot([]beads.Bead{sess}), wispCfg(),
		runtime.NewFake(), false, nil, nil,
	)
	if len(closed) != 0 {
		t.Fatalf("closed = %v, want none: trigger bead is still open", closed)
	}
	got, _ := store.Get(sess.ID)
	if got.Status == "closed" {
		t.Fatal("session with an open trigger bead was closed")
	}
}

// Frozen-shape B: not running, trigger IS closed, but the session still
// holds a DIFFERENT open/in-progress work bead as assignee — crashed mid a
// second task, not actually done. This is the sharpest test of the
// ga-ven9sa finding: a session can look done by one signal (its named
// trigger closed) while still legitimately holding unrelated live work: the
// fresh cross-store assigned-work check must catch what the trigger check
// alone cannot.
func TestSweepClosedTriggerWispSessions_KeepsCrashedSessionWithOtherAssignedWorkAlive(t *testing.T) {
	store := beads.NewMemStore()
	trigger, _ := store.Create(beads.Bead{Title: "molecule step", Status: "open"})
	_ = store.Close(trigger.ID)

	sess, err := store.Create(ephemeralWispBead("wisp-frozen-b", trigger.ID))
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	otherWork, err := store.Create(beads.Bead{
		Title:    "unrelated in-flight work",
		Assignee: sess.ID,
	})
	if err != nil {
		t.Fatalf("Create other work: %v", err)
	}
	// MemStore.Create always defaults Status to "open" regardless of what's
	// passed in — Update is the real way to reach "in_progress", and using it
	// here genuinely exercises the in-progress case rather than silently
	// testing "open" twice.
	if err := store.Update(otherWork.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Update other work to in_progress: %v", err)
	}

	closed := sweepClosedTriggerWispSessions(
		store, nil, newSessionBeadSnapshot([]beads.Bead{sess}), wispCfg(),
		runtime.NewFake(), false, nil, nil,
	)
	if len(closed) != 0 {
		t.Fatalf("closed = %v, want none: session still holds a different open work bead", closed)
	}
	got, _ := store.Get(sess.ID)
	if got.Status == "closed" {
		t.Fatal("session with other open assigned work was closed")
	}
	stillOpen, _ := store.Get(otherWork.ID)
	if stillOpen.Status != "in_progress" {
		t.Fatalf("unrelated work bead status = %q, want unchanged in_progress", stillOpen.Status)
	}
}

// --- Never touch configured/named/manual/non-ephemeral sessions ---

func TestSweepClosedTriggerWispSessions_KeepsNamedSessionOpen(t *testing.T) {
	store := beads.NewMemStore()
	trigger, _ := store.Create(beads.Bead{Title: "molecule step", Status: "open"})
	_ = store.Close(trigger.ID)

	bead := ephemeralWispBead("persona-nils-1", trigger.ID)
	bead.Metadata["configured_named_identity"] = "persona-nils-1"
	bead.Metadata["session_origin"] = "named"
	sess, err := store.Create(bead)
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	closed := sweepClosedTriggerWispSessions(
		store, nil, newSessionBeadSnapshot([]beads.Bead{sess}), wispCfg(),
		runtime.NewFake(), false, nil, nil,
	)
	if len(closed) != 0 {
		t.Fatalf("closed = %v, want none: a configured-named session must never be retired by this sweep", closed)
	}
}

func TestSweepClosedTriggerWispSessions_KeepsManualSessionOpen(t *testing.T) {
	store := beads.NewMemStore()
	trigger, _ := store.Create(beads.Bead{Title: "molecule step", Status: "open"})
	_ = store.Close(trigger.ID)

	bead := ephemeralWispBead("manual-1", trigger.ID)
	bead.Metadata["session_origin"] = "manual"
	sess, err := store.Create(bead)
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	closed := sweepClosedTriggerWispSessions(
		store, nil, newSessionBeadSnapshot([]beads.Bead{sess}), wispCfg(),
		runtime.NewFake(), false, nil, nil,
	)
	if len(closed) != 0 {
		t.Fatalf("closed = %v, want none: a manual session must never be retired by this sweep", closed)
	}
}

func TestSweepClosedTriggerWispSessions_KeepsNonEphemeralSessionOpen(t *testing.T) {
	store := beads.NewMemStore()
	trigger, _ := store.Create(beads.Bead{Title: "molecule step", Status: "open"})
	_ = store.Close(trigger.ID)

	// No pool markers at all (no pool_managed, no pool_slot, no
	// dependency_only, no resolvable pool-slot template) — sessionOriginInfo
	// falls through to "", which is neither "ephemeral" nor "named"/"manual".
	sess, err := store.Create(beads.Bead{
		Title:  "wisp session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":                    "not-ephemeral-1",
			"template":                        "worker",
			"agent_name":                      "worker",
			"state":                           "asleep",
			beadmeta.TriggerBeadIDMetadataKey: trigger.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	closed := sweepClosedTriggerWispSessions(
		store, nil, newSessionBeadSnapshot([]beads.Bead{sess}), wispCfg(),
		runtime.NewFake(), false, nil, nil,
	)
	if len(closed) != 0 {
		t.Fatalf("closed = %v, want none: a non-ephemeral session must never be retired by this sweep", closed)
	}
}

// --- Trigger-bead resolution: fail closed on anything unresolvable ---

func TestSweepClosedTriggerWispSessions_SkipsSessionWithNoTriggerBead(t *testing.T) {
	store := beads.NewMemStore()
	sess, err := store.Create(ephemeralWispBead("wisp-notrigger", ""))
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	closed := sweepClosedTriggerWispSessions(
		store, nil, newSessionBeadSnapshot([]beads.Bead{sess}), wispCfg(),
		runtime.NewFake(), false, nil, nil,
	)
	if len(closed) != 0 {
		t.Fatalf("closed = %v, want none: a session with no TriggerBeadID has nothing to check closure of", closed)
	}
}

func TestSweepClosedTriggerWispSessions_SkipsSessionWithUnresolvableTriggerBead(t *testing.T) {
	store := beads.NewMemStore()
	sess, err := store.Create(ephemeralWispBead("wisp-badtrigger", "ga-does-not-exist"))
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	closed := sweepClosedTriggerWispSessions(
		store, nil, newSessionBeadSnapshot([]beads.Bead{sess}), wispCfg(),
		runtime.NewFake(), false, nil, nil,
	)
	if len(closed) != 0 {
		t.Fatalf("closed = %v, want none: an unresolvable trigger bead must fail closed, not be treated as closed", closed)
	}
}

// --- Cross-store (rig) trigger resolution ---

func TestSweepClosedTriggerWispSessions_RetiresWispWithTriggerInRigStore(t *testing.T) {
	primary := beads.NewMemStore()
	rig := beads.NewMemStore()
	trigger, err := rig.Create(beads.Bead{Title: "rig molecule step", Status: "open"})
	if err != nil {
		t.Fatalf("Create rig trigger: %v", err)
	}
	if err := rig.Close(trigger.ID); err != nil {
		t.Fatalf("Close rig trigger: %v", err)
	}

	bead := ephemeralWispBead("wisp-rig-1", trigger.ID)
	bead.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = "myrig"
	sess, err := primary.Create(bead)
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	closed := sweepClosedTriggerWispSessions(
		primary, map[string]beads.Store{"myrig": rig}, newSessionBeadSnapshot([]beads.Bead{sess}), wispCfg(),
		runtime.NewFake(), false, nil, nil,
	)
	if len(closed) != 1 || closed[0] != sess.ID {
		t.Fatalf("closed = %v, want [%s]: trigger bead closed in the attached rig store", closed, sess.ID)
	}
}

func TestSweepClosedTriggerWispSessions_SkipsWispWithUnknownStoreRef(t *testing.T) {
	primary := beads.NewMemStore()
	rig := beads.NewMemStore()
	trigger, _ := rig.Create(beads.Bead{Title: "rig molecule step", Status: "open"})
	_ = rig.Close(trigger.ID)

	bead := ephemeralWispBead("wisp-rig-2", trigger.ID)
	bead.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = "notattached"
	sess, err := primary.Create(bead)
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	// rigStores does NOT contain "notattached".
	closed := sweepClosedTriggerWispSessions(
		primary, map[string]beads.Store{"myrig": rig}, newSessionBeadSnapshot([]beads.Bead{sess}), wispCfg(),
		runtime.NewFake(), false, nil, nil,
	)
	if len(closed) != 0 {
		t.Fatalf("closed = %v, want none: an unattached/unknown store ref must fail closed", closed)
	}
}

// --- Idempotence and grace-period parity with the existing sweep ---

func TestSweepClosedTriggerWispSessions_SkipsAlreadyClosedSession(t *testing.T) {
	store := beads.NewMemStore()
	trigger, _ := store.Create(beads.Bead{Title: "molecule step", Status: "open"})
	_ = store.Close(trigger.ID)

	sess, err := store.Create(ephemeralWispBead("wisp-already-closed", trigger.ID))
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	if err := store.Close(sess.ID); err != nil {
		t.Fatalf("Close session: %v", err)
	}
	sess, _ = store.Get(sess.ID)

	closed := sweepClosedTriggerWispSessions(
		store, nil, newSessionBeadSnapshot([]beads.Bead{sess}), wispCfg(),
		runtime.NewFake(), false, nil, nil,
	)
	if len(closed) != 0 {
		t.Fatalf("closed = %v, want none: an already-closed session must be a no-op", closed)
	}
}

func TestSweepClosedTriggerWispSessions_SkipsPendingCreateSession(t *testing.T) {
	store := beads.NewMemStore()
	trigger, _ := store.Create(beads.Bead{Title: "molecule step", Status: "open"})
	_ = store.Close(trigger.ID)

	bead := ephemeralWispBead("wisp-creating", trigger.ID)
	bead.Metadata["state"] = "creating"
	bead.Metadata["pending_create_claim"] = "true"
	bead.Metadata["pending_create_started_at"] = time.Now().UTC().Format(time.RFC3339)
	sess, err := store.Create(bead)
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	closed := sweepClosedTriggerWispSessions(
		store, nil, newSessionBeadSnapshot([]beads.Bead{sess}), wispCfg(),
		runtime.NewFake(), false, nil, nil,
	)
	if len(closed) != 0 {
		t.Fatalf("closed = %v, want none: a fresh pending-create session must be protected even with a closed trigger", closed)
	}
}

func TestSweepClosedTriggerWispSessions_NoopsOnPartialSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	trigger, _ := store.Create(beads.Bead{Title: "molecule step", Status: "open"})
	_ = store.Close(trigger.ID)

	sess, err := store.Create(ephemeralWispBead("wisp-partial", trigger.ID))
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	closed := sweepClosedTriggerWispSessions(
		store, nil, newSessionBeadSnapshot([]beads.Bead{sess}), wispCfg(),
		runtime.NewFake(), true /* storeQueryPartial */, nil, nil,
	)
	if len(closed) != 0 {
		t.Fatalf("closed = %v, want none: a partial snapshot must no-op the whole sweep", closed)
	}
	got, _ := store.Get(sess.ID)
	if got.Status == "closed" {
		t.Fatal("session was closed despite storeQueryPartial=true")
	}
}

// --- triggerBeadClosedInfo, isolated from the sweep loop ---

type erroringGetStore struct {
	beads.Store
}

func (e erroringGetStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errors.New("boom: simulated store failure")
}

func TestTriggerBeadClosedInfo(t *testing.T) {
	store := beads.NewMemStore()
	closedTrigger, _ := store.Create(beads.Bead{Title: "closed trigger", Status: "open"})
	_ = store.Close(closedTrigger.ID)
	openTrigger, _ := store.Create(beads.Bead{Title: "open trigger", Status: "open"})
	inProgressTrigger, _ := store.Create(beads.Bead{Title: "in-progress trigger"})
	// MemStore.Create always defaults Status to "open" — Update reaches
	// "in_progress" for real, so this case doesn't silently duplicate "open".
	if err := store.Update(inProgressTrigger.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Update trigger to in_progress: %v", err)
	}

	rig := beads.NewMemStore()
	rigClosedTrigger, _ := rig.Create(beads.Bead{Title: "rig closed trigger", Status: "open"})
	_ = rig.Close(rigClosedTrigger.ID)

	rigStores := map[string]beads.Store{"myrig": rig}

	tests := []struct {
		name       string
		triggerID  string
		storeRef   string
		wantClosed bool
		wantErr    bool
	}{
		{"empty trigger id", "", "", false, false},
		{"closed trigger in primary store", closedTrigger.ID, "", true, false},
		{"open trigger in primary store", openTrigger.ID, "", false, false},
		{"in-progress trigger in primary store", inProgressTrigger.ID, "", false, false},
		{"unresolvable trigger id", "ga-nonexistent", "", false, false},
		{"closed trigger in attached rig store", rigClosedTrigger.ID, "myrig", true, false},
		{"unknown store ref fails closed", rigClosedTrigger.ID, "notattached", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := infoWithTrigger(tt.triggerID, tt.storeRef)
			gotClosed, err := triggerBeadClosedInfo(store, rigStores, info)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if gotClosed != tt.wantClosed {
				t.Fatalf("closed = %v, want %v", gotClosed, tt.wantClosed)
			}
		})
	}
}

func TestTriggerBeadClosedInfo_SurfacesGenuineStoreError(t *testing.T) {
	info := infoWithTrigger("ga-anything", "")
	_, err := triggerBeadClosedInfo(erroringGetStore{Store: beads.NewMemStore()}, nil, info)
	if err == nil {
		t.Fatal("want a genuine store error to be surfaced, not swallowed into (false, nil)")
	}
}
