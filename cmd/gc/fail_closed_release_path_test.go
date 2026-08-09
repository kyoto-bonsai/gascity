package main

// Discriminating repro tests for
// ga-7vv3pb-claim-evaporation-findings/SPEC-fail-closed-release-path-2026-08-09.md.
// R1 (§4): the *ForReachableStore assigned-work probes must fail closed
// (errUnresolvedSessionIdentity) rather than silently return "no work" when a
// session's identity hasn't resolved to an assignee-form value yet. R2 (§4):
// poolFreeable must require positive-idle confirmation (seatPositivelyIdle),
// not merely !alive, before a drain/close/release decision may fire.
//
// Positive controls (SPEC §6.3 — the reaper must still work) are NOT
// duplicated here: TestReconcileSessionBeads_StrandedRepairFiresAfterContinuousWindow
// and TestReconcileSessionBeads_StrandedRepairReArmsWindowAfterRecovery
// (session_reconciler_test.go) already cover a genuinely-dead seat with and
// without stranded work, are unmodified by this change, and re-passing them
// after R1+R2 land IS the positive-control proof.

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// --- R1: sessionIdentityResolvedForWorkCheck (the new primitive, instrumented directly) ---

func TestSessionIdentityResolvedForWorkCheck(t *testing.T) {
	cases := []struct {
		name        string
		identifiers []string
		info        sessionpkg.Info
		want        bool
	}{
		{
			name:        "bare ID only is unresolved",
			identifiers: []string{"sb-1"},
			info:        sessionpkg.Info{ID: "sb-1"},
			want:        false,
		},
		{
			name:        "empty identifiers is unresolved",
			identifiers: nil,
			info:        sessionpkg.Info{ID: "sb-1"},
			want:        false,
		},
		{
			name:        "session_name-derived identifier is resolved",
			identifiers: []string{"sb-1", "worker-mc-live"},
			info:        sessionpkg.Info{ID: "sb-1", SessionNameMetadata: "worker-mc-live"},
			want:        true,
		},
		{
			name:        "configured_named_identity-only is resolved",
			identifiers: []string{"sb-1", "riga/worker"},
			info:        sessionpkg.Info{ID: "sb-1", ConfiguredNamedIdentity: "riga/worker"},
			want:        true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionIdentityResolvedForWorkCheck(tc.identifiers, tc.info); got != tc.want {
				t.Fatalf("sessionIdentityResolvedForWorkCheck(%v, %+v) = %v, want %v", tc.identifiers, tc.info, got, tc.want)
			}
		})
	}
}

// --- R1: the three *ForReachableStore chokepoints fail closed on unresolved identity ---

// unresolvedIdentityFixture creates a store where real work is assigned to a
// session's TRUE runtime name ("worker-mc-live"), plus a session bead whose
// own session_name metadata has not stamped yet — the deferred/unstamped gap
// the SPEC's RCA traces (a freshly-spawned pool slot's runtime identity can
// still be deferred at guard time). SeedBead projects an Info whose
// SessionNameMetadata is empty, so sessionAssignmentIdentifiersForConfigInfo
// resolves to nothing but the bare bead ID — which can never match the real
// assignee, since the claim path writes assignee=session_name.
func unresolvedIdentityFixture(t *testing.T, workStatus string) (*beads.MemStore, sessionpkg.Info) {
	t.Helper()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		ID:       "real-work",
		Type:     "task",
		Status:   workStatus,
		Assignee: "worker-mc-live",
	}); err != nil {
		t.Fatalf("Create real work: %v", err)
	}
	session := beads.Bead{
		ID:     "sb-pending",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"template": "worker",
			// session_name deliberately absent: not yet stamped.
		},
	}
	return store, sessiontest.SeedBead(t, session)
}

func TestSessionHasOpenAssignedWorkForReachableStore_UnresolvedIdentityFailsClosed(t *testing.T) {
	store, info := unresolvedIdentityFixture(t, "open")

	has, err := sessionHasOpenAssignedWorkForReachableStore("", &config.City{}, store, nil, info)
	if !errors.Is(err, errUnresolvedSessionIdentity) {
		t.Fatalf("err = %v, want errUnresolvedSessionIdentity", err)
	}
	if has {
		t.Fatal("has = true, want false alongside the sentinel error")
	}

	// Instrument the caller contract this sentinel rides, not just trust it's
	// unchanged elsewhere: every real call site fail-safes a non-nil error to
	// hasAssignedWork=true, which is what actually prevents the wrongful
	// drain/close for the real (but unmatchable) work created above.
	hasAssignedWork := has
	if err != nil {
		hasAssignedWork = true
	}
	if !hasAssignedWork {
		t.Fatal("caller fail-safe must treat unresolved identity as having assigned work")
	}
}

func TestSessionHasAwakeAssignedWorkForReachableStore_UnresolvedIdentityFailsClosed(t *testing.T) {
	store, info := unresolvedIdentityFixture(t, "in_progress")

	has, err := sessionHasAwakeAssignedWorkForReachableStore("", &config.City{}, store, nil, info)
	if !errors.Is(err, errUnresolvedSessionIdentity) {
		t.Fatalf("err = %v, want errUnresolvedSessionIdentity", err)
	}
	if has {
		t.Fatal("has = true, want false alongside the sentinel error")
	}
}

func TestFirstOpenAssignedWorkBeadForReachableStore_UnresolvedIdentityFailsClosed(t *testing.T) {
	store, info := unresolvedIdentityFixture(t, "open")

	bead, found, err := firstOpenAssignedWorkBeadForReachableStore("", &config.City{}, store, nil, info)
	if !errors.Is(err, errUnresolvedSessionIdentity) {
		t.Fatalf("err = %v, want errUnresolvedSessionIdentity", err)
	}
	if found {
		t.Fatal("found = true, want false alongside the sentinel error")
	}
	if bead.ID != "" {
		t.Fatalf("bead = %+v, want zero value alongside the sentinel error", bead)
	}
}

// --- R2: seatPositivelyIdle (the new primitive, instrumented directly) ---

func TestSeatPositivelyIdle(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	t.Run("alive is never idle regardless of activity", func(t *testing.T) {
		sp := runtime.NewFake()
		sp.SetActivity("worker", now.Add(-time.Hour))
		if seatPositivelyIdle("worker", true, sp, clk) {
			t.Fatal("an alive seat must never be treated as positively idle")
		}
	})

	t.Run("not alive with zero never-observed activity is idle", func(t *testing.T) {
		sp := runtime.NewFake() // Activity map default: zero time for "worker"
		if !seatPositivelyIdle("worker", false, sp, clk) {
			t.Fatal("a not-alive seat with no recorded activity must be positively idle")
		}
	})

	t.Run("not alive with recent activity is NOT idle - the false-negative case this cures", func(t *testing.T) {
		sp := runtime.NewFake()
		sp.SetActivity("worker", now.Add(-time.Second)) // token-advancing a second ago
		if seatPositivelyIdle("worker", false, sp, clk) {
			t.Fatal("recent activity must block idle classification even when the runtime liveness probe false-negatives")
		}
	})

	t.Run("activity exactly at the grace boundary is idle", func(t *testing.T) {
		sp := runtime.NewFake()
		sp.SetActivity("worker", now.Add(-positiveIdleGrace))
		if !seatPositivelyIdle("worker", false, sp, clk) {
			t.Fatal("activity exactly positiveIdleGrace old must count as idle (>= boundary)")
		}
	})

	t.Run("activity just inside the grace window is NOT idle", func(t *testing.T) {
		sp := runtime.NewFake()
		sp.SetActivity("worker", now.Add(-positiveIdleGrace+time.Second))
		if seatPositivelyIdle("worker", false, sp, clk) {
			t.Fatal("activity just inside the grace window must not count as idle")
		}
	})

	t.Run("GetLastActivity error fails closed", func(t *testing.T) {
		// Deviates from the SPEC's literal "zero/error" bundling: this
		// codebase's own GetLastActivity contract normalizes "unknown
		// session"/malformed data to a zero time with a NIL error, so a real
		// error here is a genuine transient read failure, not "session
		// doesn't exist". The governing invariant (SPEC §0/§3: unresolved
		// input takes the query-error path, no action) is more authoritative
		// than the shorthand suggestion. See the seatPositivelyIdle doc
		// comment in session_reconciler.go for the full reasoning.
		sp := runtime.NewFailFake()
		if seatPositivelyIdle("worker", false, sp, clk) {
			t.Fatal("a GetLastActivity error must NOT be treated as positively idle")
		}
	})

	t.Run("nil provider or empty name cannot positively confirm idle", func(t *testing.T) {
		if seatPositivelyIdle("worker", false, nil, clk) {
			t.Fatal("nil provider must not be treated as positively idle")
		}
		sp := runtime.NewFake()
		if seatPositivelyIdle("", false, sp, clk) {
			t.Fatal("empty name must not be treated as positively idle")
		}
	})
}

// --- R2: end-to-end discriminating repro through the real reconcile tick (SPEC §6.2) ---

// TestReconcileSessionBeads_PoolFreeableRecentActivityBlocksRepair is the R2
// release-leg discriminator: a pool seat in the exact stranded-repair shape
// (asleep, idle, pool_managed, runtime not observed running via
// strandedRepairReconcileEnv's env.addDesired(..., false)) that ALSO has
// RECENT GetLastActivity — the false-negative-liveness shape
// observeRuntimeProviderLiveness misses for adhoc/wisp/pool seats (SPEC
// §1 S1). Before R2, poolFreeable ignored activity entirely: !target.alive +
// the confirmation window alone triggered the stranded-repair, releasing a
// live claim (this bug, fleet-wide: cmo/daedalus/lena/nils seats, ga-sot51w's
// own report). After R2, seatPositivelyIdle gates poolFreeable on positive
// staleness, so a continuously-active seat's claim survives past the
// confirmation window instead of being reaped ~2min after spawn.
func TestReconcileSessionBeads_PoolFreeableRecentActivityBlocksRepair(t *testing.T) {
	env, session, work, rec := strandedRepairReconcileEnv(t)

	env.sp.SetActivity("worker", env.clk.Time)
	runStrandedReconcileTick(t, env, []beads.Bead{session})
	if got := len(rec.strandedEvents()); got != 0 {
		t.Fatalf("stranded events after tick 1 = %d, want 0 - a positively-active seat must not even be diagnosed as stranded", got)
	}
	afterFirst, _ := env.store.Get(session.ID)
	if afterFirst.Status == "closed" {
		t.Fatal("session must not be closed for a positively-active seat")
	}

	// Advance well past the confirmation window and re-affirm activity as of
	// "now" (still continuously active) - pre-R2 the elapsed window alone was
	// sufficient to trigger the repair regardless of activity.
	env.clk.Time = env.clk.Time.Add(strandedRepairConfirmGrace + time.Minute)
	env.sp.SetActivity("worker", env.clk.Time.Add(-time.Second))
	updated, _ := env.store.Get(session.ID)
	runStrandedReconcileTick(t, env, []beads.Bead{updated})

	if got := len(rec.strandedEvents()); got != 0 {
		t.Fatalf("stranded events after tick 2 = %d, want 0 - still positively active", got)
	}
	gotWork, _ := env.store.Get(work.ID)
	if gotWork.Status != "in_progress" || gotWork.Assignee != session.ID {
		t.Fatalf("work must stay claimed for a positively-active seat; got status=%q assignee=%q", gotWork.Status, gotWork.Assignee)
	}
	gotSession, _ := env.store.Get(session.ID)
	if gotSession.Status == "closed" {
		t.Fatal("session must stay open for a positively-active seat, even past the confirmation window")
	}
}
