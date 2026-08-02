package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// fakeLookPath always succeeds, so provider resolution in these tests never
// depends on a real "codex"-named binary being installed on the test
// machine's PATH.
func fakeLookPath(string) (string, error) { return "/usr/bin/fake", nil }

func seatCapTestConfig(providerName string, maxSeats *int) *config.City { //nolint:unparam // providerName kept explicit at call sites for readability; every current caller happens to route through "codex"
	return &config.City{
		Agents: []config.Agent{
			{Name: "codex-worker", Provider: providerName},
			{Name: "claude-worker", Provider: "claude"},
		},
		Providers: map[string]config.ProviderSpec{
			providerName: {Command: providerName, MaxSeats: maxSeats},
			"claude":     {Command: "claude"},
		},
	}
}

func seedOpenSession(t *testing.T, store beads.Store, sessionName, template string) {
	t.Helper()
	if _, err := store.Create(beads.Bead{
		Title:  "open session " + sessionName,
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": sessionName,
			"template":     template,
		},
	}); err != nil {
		t.Fatalf("seed open session bead %s: %v", sessionName, err)
	}
}

// intPtr is defined in pool_desired_state_test.go (shared package-level
// test helper).

// TestCheckProviderSeatCap_AtCapBlocks pins the core trip condition: a
// provider already at its ratified max_seats refuses one more dispatch to
// that same provider.
func TestCheckProviderSeatCap_AtCapBlocks(t *testing.T) {
	oldLookPath := seatCapLookPathHook
	t.Cleanup(func() { seatCapLookPathHook = oldLookPath })
	seatCapLookPathHook = fakeLookPath

	cfg := seatCapTestConfig("codex", intPtr(2))
	store := beads.NewMemStore()
	seedOpenSession(t, store, "codex-1", "codex-worker")
	seedOpenSession(t, store, "codex-2", "codex-worker")

	res := checkProviderSeatCap(store, cfg, "codex")
	if !res.Blocked {
		t.Fatal("at-cap provider did not trip the seat cap gate")
	}
	if !strings.Contains(res.Reason, "seat cap") {
		t.Errorf("Reason = %q, want mention of seat cap", res.Reason)
	}
	if !strings.Contains(res.Reason, "2/2") {
		t.Errorf("Reason = %q, want the 2/2 count", res.Reason)
	}
}

// TestCheckProviderSeatCap_UnderCapProceeds is the negative control: a
// provider below its cap must not trip the gate.
func TestCheckProviderSeatCap_UnderCapProceeds(t *testing.T) {
	oldLookPath := seatCapLookPathHook
	t.Cleanup(func() { seatCapLookPathHook = oldLookPath })
	seatCapLookPathHook = fakeLookPath

	cfg := seatCapTestConfig("codex", intPtr(2))
	store := beads.NewMemStore()
	seedOpenSession(t, store, "codex-1", "codex-worker")

	res := checkProviderSeatCap(store, cfg, "codex")
	if res.Blocked {
		t.Fatalf("under-cap provider blocked: reason=%q", res.Reason)
	}
}

// TestCheckProviderSeatCap_OnlyCountsMatchingProvider pins the attribution
// half: sessions attributed to a different provider (via a different
// template) must not count against codex's cap, even past codex's own
// count.
func TestCheckProviderSeatCap_OnlyCountsMatchingProvider(t *testing.T) {
	oldLookPath := seatCapLookPathHook
	t.Cleanup(func() { seatCapLookPathHook = oldLookPath })
	seatCapLookPathHook = fakeLookPath

	cfg := seatCapTestConfig("codex", intPtr(2))
	store := beads.NewMemStore()
	seedOpenSession(t, store, "codex-1", "codex-worker")
	seedOpenSession(t, store, "claude-1", "claude-worker")
	seedOpenSession(t, store, "claude-2", "claude-worker")
	seedOpenSession(t, store, "claude-3", "claude-worker")

	res := checkProviderSeatCap(store, cfg, "codex")
	if res.Blocked {
		t.Fatalf("codex blocked by unrelated claude sessions: reason=%q", res.Reason)
	}
}

// TestCheckProviderSeatCap_UnconfiguredCapFailsOpen pins the fail-open
// posture: a provider with no max_seats set (nil) is never blocked,
// regardless of how many sessions are open.
func TestCheckProviderSeatCap_UnconfiguredCapFailsOpen(t *testing.T) {
	oldLookPath := seatCapLookPathHook
	t.Cleanup(func() { seatCapLookPathHook = oldLookPath })
	seatCapLookPathHook = fakeLookPath

	cfg := seatCapTestConfig("codex", nil)
	store := beads.NewMemStore()
	seedOpenSession(t, store, "codex-1", "codex-worker")
	seedOpenSession(t, store, "codex-2", "codex-worker")
	seedOpenSession(t, store, "codex-3", "codex-worker")

	res := checkProviderSeatCap(store, cfg, "codex")
	if res.Blocked {
		t.Fatalf("unconfigured max_seats blocked: reason=%q", res.Reason)
	}
}

// TestCheckProviderSeatCap_UnlimitedSentinelFailsOpen pins the explicit
// -1-means-unlimited sentinel, mirroring Agent.MaxActiveSessions semantics.
func TestCheckProviderSeatCap_UnlimitedSentinelFailsOpen(t *testing.T) {
	oldLookPath := seatCapLookPathHook
	t.Cleanup(func() { seatCapLookPathHook = oldLookPath })
	seatCapLookPathHook = fakeLookPath

	cfg := seatCapTestConfig("codex", intPtr(-1))
	store := beads.NewMemStore()
	seedOpenSession(t, store, "codex-1", "codex-worker")
	seedOpenSession(t, store, "codex-2", "codex-worker")
	seedOpenSession(t, store, "codex-3", "codex-worker")

	res := checkProviderSeatCap(store, cfg, "codex")
	if res.Blocked {
		t.Fatalf("max_seats=-1 (unlimited) blocked: reason=%q", res.Reason)
	}
}

// TestCheckProviderSeatCap_ZeroCapBlocksImmediately pins the literal-cap
// end of the convention: max_seats=0 is a real cap (no seats), not an
// unset/unlimited sentinel, so a single open session already trips it.
func TestCheckProviderSeatCap_ZeroCapBlocksImmediately(t *testing.T) {
	oldLookPath := seatCapLookPathHook
	t.Cleanup(func() { seatCapLookPathHook = oldLookPath })
	seatCapLookPathHook = fakeLookPath

	cfg := seatCapTestConfig("codex", intPtr(0))
	store := beads.NewMemStore()

	res := checkProviderSeatCap(store, cfg, "codex")
	if !res.Blocked {
		t.Fatal("max_seats=0 did not block with zero open sessions")
	}
}

// TestCheckSpawnPreflightGate_SeatCapWiredIn pins the integration point:
// checkSpawnPreflightGate itself (not just checkProviderSeatCap directly)
// refuses when the resolved provider is at cap.
func TestCheckSpawnPreflightGate_SeatCapWiredIn(t *testing.T) {
	oldAlive := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = oldAlive })
	supervisorAliveHook = func() int { return 0 }

	oldLookPath := seatCapLookPathHook
	t.Cleanup(func() { seatCapLookPathHook = oldLookPath })
	seatCapLookPathHook = fakeLookPath

	cfg := seatCapTestConfig("codex", intPtr(1))
	store := beads.NewMemStore()
	seedOpenSession(t, store, "codex-1", "codex-worker")

	res := checkSpawnPreflightGate(store, false, cfg, "codex")
	if !res.Blocked {
		t.Fatal("checkSpawnPreflightGate did not refuse at the wired-in seat cap")
	}

	// A different, under-cap provider on the same store must still proceed.
	res = checkSpawnPreflightGate(store, false, cfg, "claude")
	if res.Blocked {
		t.Fatalf("checkSpawnPreflightGate blocked an unrelated under-cap provider: reason=%q", res.Reason)
	}
}

// TestSessionProviderName_StaleTemplateReturnsEmpty pins the safe-exclusion
// behavior: a session whose template no longer matches any configured
// agent is excluded from every provider's count rather than misattributed.
func TestSessionProviderName_StaleTemplateReturnsEmpty(t *testing.T) {
	oldLookPath := seatCapLookPathHook
	t.Cleanup(func() { seatCapLookPathHook = oldLookPath })
	seatCapLookPathHook = fakeLookPath

	cfg := seatCapTestConfig("codex", intPtr(1))
	info := session.Info{Template: "removed-template"}
	if got := sessionProviderName(info, cfg); got != "" {
		t.Errorf("sessionProviderName(stale template) = %q, want empty", got)
	}
}
