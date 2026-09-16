package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
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
	// ga-r1wouq: an explicit "active" state, matching a real just-created,
	// running session bead. Before ga-r1wouq's fix every open bead counted
	// against the seat cap regardless of state, so this helper's callers
	// never needed to say what state they meant; now that only sessions
	// sessionpkg.CountsAgainstCapacity recognizes (and whose freshness holds)
	// count, "active" is what every existing caller of this helper actually
	// intends -- a live, counted session. Tests that need a specific
	// non-counting or stale state use seedSessionWithState directly.
	seedSessionWithState(t, store, sessionName, template, "active")
}

// seedSessionWithState is seedOpenSession's more general form (ga-r1wouq):
// callers that need a specific lifecycle state -- asleep, suspended,
// start-pending, orphaned, or any other raw state string -- for the
// counts-against-capacity / freshness fixtures use this directly.
func seedSessionWithState(t *testing.T, store beads.Store, sessionName, template, state string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  "open session " + sessionName,
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": sessionName,
			"template":     template,
			"state":        state,
		},
	})
	if err != nil {
		t.Fatalf("seed open session bead %s: %v", sessionName, err)
	}
	return b
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

// --- ga-r1wouq: cap counter must exclude stale/asleep historical session beads ---

// nonCountingSeatCapStates are every raw state that projects to a
// BaseState/State sessionpkg.CountsAgainstCapacity excludes. "" (no state
// metadata at all -- BaseStateNone) is included: a session bead with no
// lifecycle state recorded yet has no live runtime to count either.
var nonCountingSeatCapStates = []string{"asleep", "suspended", "drained", "archived", "failed-create", "orphaned", ""}

// countingSeatCapStates are every raw state sessionpkg.CountsAgainstCapacity
// recognizes as owning (or about to own, or mid-shutdown still holding) a
// real runtime process. "awake" and "active" both project to the same live
// state (compatStateForBase folds them together); both are listed because
// they are two distinct raw spellings a real bead can carry.
var countingSeatCapStates = []string{"active", "awake", "start-pending", "creating", "draining", "quarantined"}

// TestCheckProviderSeatCap_ExcludesNonCountingStates pins ga-r1wouq's core
// fix: every non-counting state, seeded as an otherwise-ordinary open
// session bead, must NOT trip a cap of 1 -- proving the old code's "every
// open bead counts" behavior is gone for each state individually.
func TestCheckProviderSeatCap_ExcludesNonCountingStates(t *testing.T) {
	oldLookPath := seatCapLookPathHook
	t.Cleanup(func() { seatCapLookPathHook = oldLookPath })
	seatCapLookPathHook = fakeLookPath

	for _, state := range nonCountingSeatCapStates {
		t.Run("state="+state, func(t *testing.T) {
			cfg := seatCapTestConfig("codex", intPtr(1))
			store := beads.NewMemStore()
			seedSessionWithState(t, store, "codex-1", "codex-worker", state)

			res := checkProviderSeatCap(store, cfg, "codex")
			if res.Blocked {
				t.Fatalf("state=%q counted against a cap of 1: reason=%q", state, res.Reason)
			}
		})
	}
}

// TestCheckProviderSeatCap_CountsAllPolicyActiveStates is the positive
// mirror: every counting state must trip a cap sized to match, one at a
// time, proving the fix did not overcorrect into excluding real live
// sessions too.
func TestCheckProviderSeatCap_CountsAllPolicyActiveStates(t *testing.T) {
	oldLookPath := seatCapLookPathHook
	t.Cleanup(func() { seatCapLookPathHook = oldLookPath })
	seatCapLookPathHook = fakeLookPath

	for _, state := range countingSeatCapStates {
		t.Run("state="+state, func(t *testing.T) {
			cfg := seatCapTestConfig("codex", intPtr(1))
			store := beads.NewMemStore()
			seedSessionWithState(t, store, "codex-1", "codex-worker", state)

			res := checkProviderSeatCap(store, cfg, "codex")
			if !res.Blocked {
				t.Fatalf("state=%q did not count against a cap of 1", state)
			}
		})
	}
}

// TestCheckProviderSeatCap_ExcludesStaleHeartbeat pins the freshness half
// of ga-r1wouq's fix: a session whose state claims live (active) but whose
// bead has not been touched within seatCapStaleActiveAfter must not count
// -- the crashed-process signature the false 20+/16 result traced to. Uses
// the injected clock so the test is deterministic, not a wall-clock race.
func TestCheckProviderSeatCap_ExcludesStaleHeartbeat(t *testing.T) {
	oldLookPath := seatCapLookPathHook
	t.Cleanup(func() { seatCapLookPathHook = oldLookPath })
	seatCapLookPathHook = fakeLookPath

	cfg := seatCapTestConfig("codex", intPtr(1))
	store := beads.NewMemStore()
	b := seedSessionWithState(t, store, "codex-1", "codex-worker", "active")

	fresh := &clock.Fake{Time: b.UpdatedAt.Add(1 * time.Minute)}
	if res := checkProviderSeatCapAt(store, cfg, "codex", fresh); !res.Blocked {
		t.Fatal("fresh active session (1m old) did not count against a cap of 1")
	}

	stale := &clock.Fake{Time: b.UpdatedAt.Add(seatCapStaleActiveAfter + time.Minute)}
	if res := checkProviderSeatCapAt(store, cfg, "codex", stale); res.Blocked {
		t.Fatalf("stale active session (%s old) still counted: reason=%q", seatCapStaleActiveAfter+time.Minute, res.Reason)
	}
}

// TestCheckProviderSeatCap_DiagnosticNamesExcludedStateCounts pins the
// acceptance criterion "diagnostic output names excluded-state counts":
// the block Reason must name each excluded state and how many sessions it
// excluded, not just the final live count.
func TestCheckProviderSeatCap_DiagnosticNamesExcludedStateCounts(t *testing.T) {
	oldLookPath := seatCapLookPathHook
	t.Cleanup(func() { seatCapLookPathHook = oldLookPath })
	seatCapLookPathHook = fakeLookPath

	cfg := seatCapTestConfig("codex", intPtr(1))
	store := beads.NewMemStore()
	seedSessionWithState(t, store, "codex-live", "codex-worker", "active")
	seedSessionWithState(t, store, "codex-asleep-1", "codex-worker", "asleep")
	seedSessionWithState(t, store, "codex-asleep-2", "codex-worker", "asleep")
	seedSessionWithState(t, store, "codex-orphaned", "codex-worker", "orphaned")

	res := checkProviderSeatCap(store, cfg, "codex")
	if !res.Blocked {
		t.Fatalf("the one live session did not trip a cap of 1: reason=%q", res.Reason)
	}
	for _, want := range []string{"asleep=2", "orphaned=1"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("Reason = %q, want it to name %q", res.Reason, want)
		}
	}
}

// TestCheckProviderSeatCap_ReproducesFalseOvercountRegression is the named
// acceptance scenario: "reproduce the prior false 20+/16 result and show
// corrected live count consistent with supervisor state." 6 live sessions
// (what a real supervisor sessions API would report) plus 14 historical
// asleep/orphaned beads (the ga-3oiiyf shape -- old, status=open, long since
// dormant) against a cap of 16. The naive "every open bead counts" read (20)
// would have falsely blocked; the fixed, state-and-freshness-aware read (6)
// must not.
func TestCheckProviderSeatCap_ReproducesFalseOvercountRegression(t *testing.T) {
	oldLookPath := seatCapLookPathHook
	t.Cleanup(func() { seatCapLookPathHook = oldLookPath })
	seatCapLookPathHook = fakeLookPath

	cfg := seatCapTestConfig("codex", intPtr(16))
	store := beads.NewMemStore()

	const liveCount = 6
	for i := 0; i < liveCount; i++ {
		seedSessionWithState(t, store, sessionNameForIndex("codex-live", i), "codex-worker", "active")
	}
	const historicalCount = 14
	historicalStates := []string{"asleep", "orphaned"}
	for i := 0; i < historicalCount; i++ {
		seedSessionWithState(t, store, sessionNameForIndex("codex-historical", i), "codex-worker", historicalStates[i%len(historicalStates)])
	}

	totalOpen := liveCount + historicalCount // 20 -- the naive, pre-fix count
	if totalOpen < *cfg.Providers["codex"].MaxSeats {
		t.Fatalf("test setup error: totalOpen=%d must be >= the cap to reproduce a false block", totalOpen)
	}

	res := checkProviderSeatCap(store, cfg, "codex")
	if res.Blocked {
		t.Fatalf("false overcount regression reproduced: %d truly live sessions "+
			"(cap 16) refused a dispatch: reason=%q", liveCount, res.Reason)
	}
}

func sessionNameForIndex(prefix string, i int) string {
	return prefix + "-" + string(rune('a'+i))
}
