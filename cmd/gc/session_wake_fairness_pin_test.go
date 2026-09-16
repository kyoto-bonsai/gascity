package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestWakeFairnessInfoTwinCharacterization is the #2574-class regression guard for
// WI-6 W5 (start-execution feed typing). The per-tick wake budget is spent
// least-recently-woken first (wakeFairnessTime → sortCandidatesByWakeFairness). A
// same-tick sleep→re-wake (max-age kill / idle kill) clears last_woke_at via
// SleepPatch BEFORE the startCandidate is appended, so the re-woken session must
// sort by its cleared-fallback key (slept_at, stamped by the same SleepPatch;
// falling further back to CreatedAt only when slept_at is also unset),
// competing fairly instead of jumping the queue on a stale last_woke_at.
//
// This pins two things across the W5 A→B read cutover:
//  1. wakeFairnessTime returns the right key for every coupling-mirror scenario
//     (cleared last_woke_at → slept_at fallback → CreatedAt when slept_at is also
//     unset; valid last_woke_at honored; all empty → zero) and sorts accordingly.
//  2. The captured Info twin agrees with the raw pointer for that key — a fairness
//     time computed off session.Info equals wakeFairnessTime over the same bead.
//     In Commit A wakeFairnessTime still reads the raw pointer, so this catches a
//     twin projection drift; in Commit B it reads Info, so the scenario coverage in
//     (1) stays load-bearing.
func TestWakeFairnessInfoTwinCharacterization(t *testing.T) {
	base := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)

	// infoFairnessTime mirrors wakeFairnessTime's rule off the typed twin: parse
	// last_woke_at, else slept_at, else fall back to CreatedAt, else zero. Kept
	// local so the pin stays honest even after wakeFairnessTime itself moves onto
	// Info.
	infoFairnessTime := func(i session.Info) time.Time {
		if t, err := time.Parse(time.RFC3339, i.LastWokeAt); err == nil {
			return t
		}
		if t, err := time.Parse(time.RFC3339, i.SleptAt); err == nil {
			return t
		}
		if !i.CreatedAt.IsZero() {
			return i.CreatedAt
		}
		return time.Time{}
	}

	// candidateFor builds a startCandidate the way the reconciler append site does:
	// the raw bead plus the coherent Info twin projected from it.
	candidateFor := func(bead beads.Bead) startCandidate {
		return startCandidate{info: sessiontest.SeedBead(t, bead)}
	}

	beadWithMeta := func(id string, created time.Time, meta map[string]string) beads.Bead {
		return beads.Bead{
			ID:        id,
			Type:      session.BeadType,
			Title:     "worker",
			Labels:    []string{session.LabelSession},
			CreatedAt: created,
			Metadata:  meta,
		}
	}

	// applySleep mirrors the max-age / idle-kill coupling: SleepPatch clears
	// last_woke_at onto the bead metadata, exactly what the reconciler folds before
	// the append (session_reconciler.go).
	applySleep := func(bead beads.Bead) beads.Bead {
		for k, v := range session.SleepPatch(base, "idle") {
			bead.Metadata[k] = v
		}
		return bead
	}

	// applyDrainAck mirrors the drain-ack coupling: AcknowledgeDrainPatch also
	// clears last_woke_at (the dominant real-world path for wake_mode=fresh
	// roles) and, like SleepPatch, stamps slept_at as the fallback fairness key.
	applyDrainAck := func(bead beads.Bead) beads.Bead {
		for k, v := range session.AcknowledgeDrainPatch(base, false) {
			bead.Metadata[k] = v
		}
		return bead
	}

	valid := base.Add(-30 * time.Minute).Format(time.RFC3339)

	scenarios := []struct {
		name string
		bead beads.Bead
		want time.Time
	}{
		{
			// SleepPatch stamps slept_at=base alongside clearing last_woke_at, so
			// the fallback now lands on the slept_at tier (base), not CreatedAt.
			name: "max-age-kill-clears-last-woke-at-falls-back-to-slept-at",
			bead: applySleep(beadWithMeta("ga-maxage", base.Add(-2*time.Hour), map[string]string{
				"template": "worker", "last_woke_at": valid,
			})),
			want: base,
		},
		{
			name: "idle-kill-clears-last-woke-at-falls-back-to-slept-at",
			bead: applySleep(beadWithMeta("ga-idle", base.Add(-90*time.Minute), map[string]string{
				"template": "worker", "last_woke_at": valid, "sleep_reason": "idle",
			})),
			want: base,
		},
		{
			name: "valid-last-woke-at-honored",
			bead: beadWithMeta("ga-valid", base.Add(-3*time.Hour), map[string]string{
				"template": "worker", "last_woke_at": valid,
			}),
			want: mustParseRFC3339(t, valid),
		},
		{
			name: "empty-last-woke-at-created-fallback",
			bead: beadWithMeta("ga-created", base.Add(-45*time.Minute), map[string]string{
				"template": "worker",
			}),
			want: base.Add(-45 * time.Minute),
		},
		{
			name: "both-empty-zero-time",
			bead: beadWithMeta("ga-zero", time.Time{}, map[string]string{
				"template": "worker",
			}),
			want: time.Time{},
		},
		{
			// The actual production gap: AcknowledgeDrainPatch is the dominant
			// real-world drain path for wake_mode=fresh roles, and (until this fix)
			// it clears last_woke_at without stamping slept_at, collapsing straight
			// to the CreatedAt floor. This scenario pins the fixed behavior: the
			// drain-ack's own slept_at stamp (base) wins over the far-older
			// CreatedAt.
			name: "drain-ack-clears-last-woke-at-falls-back-to-slept-at",
			bead: applyDrainAck(beadWithMeta("ga-drainack", base.Add(-5*time.Hour), map[string]string{
				"template": "worker", "last_woke_at": valid,
			})),
			want: base,
		},
		{
			// A malformed slept_at must fall through the middle tier to CreatedAt
			// rather than being treated as present-but-zero.
			name: "malformed-slept-at-falls-through-to-created",
			bead: beadWithMeta("ga-badslept", base.Add(-70*time.Minute), map[string]string{
				"template": "worker", "slept_at": "not-a-timestamp",
			}),
			want: base.Add(-70 * time.Minute),
		},
	}

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			cand := candidateFor(sc.bead)
			got := wakeFairnessTime(cand)
			if !got.Equal(sc.want) {
				t.Errorf("wakeFairnessTime = %v, want %v", got, sc.want)
			}
			// Twin coherence: the Info-derived key equals the wakeFairnessTime output
			// (the coupling mirrors all fold onto the captured Info before append).
			if twin := infoFairnessTime(cand.info); !twin.Equal(got) {
				t.Errorf("info-derived fairness time %v != wakeFairnessTime %v (twin drift)", twin, got)
			}
		})
	}

	// Same-tick re-wake ordering (#2574): two sessions slept THIS tick (last_woke_at
	// cleared) sort by their shared slept_at among themselves and ahead of one that
	// still carries a newer valid last_woke_at. sortCandidatesByWakeFairness must
	// rotate the budget onto the longest-waiting sessions rather than defer them on
	// a stale key.
	oldSlept := candidateFor(applySleep(beadWithMeta("ga-old", base.Add(-3*time.Hour), map[string]string{
		"template": "worker", "last_woke_at": valid,
	})))
	newSlept := candidateFor(applySleep(beadWithMeta("ga-new", base.Add(-1*time.Hour), map[string]string{
		"template": "worker", "last_woke_at": valid,
	})))
	recentlyWoken := candidateFor(beadWithMeta("ga-recent", base.Add(-4*time.Hour), map[string]string{
		"template": "worker", "last_woke_at": base.Add(-10 * time.Minute).Format(time.RFC3339),
	}))

	cands := []startCandidate{recentlyWoken, newSlept, oldSlept}
	sortCandidatesByWakeFairness(cands)
	gotOrder := []string{cands[0].info.ID, cands[1].info.ID, cands[2].info.ID}
	// ga-old and ga-new both fall back to slept_at=base (applySleep stamps the same
	// base on both, regardless of their distinct CreatedAt), so they tie on the
	// fairness key; sort.SliceStable then resolves the tie by original input-slice
	// order (ga-new precedes ga-old in cands), not by CreatedAt. ga-recent's own
	// last_woke_at (base-10m) is chronologically earlier than the tied base, so it
	// still sorts first. This fully reverses the pre-slept_at-tier order
	// ([]string{"ga-old", "ga-new", "ga-recent"}), which was really just
	// FIFO-by-creation once last_woke_at was cleared with no slept_at tier to fall
	// back to.
	wantOrder := []string{"ga-recent", "ga-new", "ga-old"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("fairness sort order = %v, want %v", gotOrder, wantOrder)
		}
	}
}

// TestSortCandidatesByWakeFairness_PendingCreatesPrecedeRewakes pins the
// ga-r6lc7g ordering change: pending-create candidates sort ahead of every
// re-wake candidate regardless of wake-fairness time, because a
// pending-create is racing a hard lease expiry
// (pendingCreateNeverStartedLeaseExpiredInfo) that a re-wake does not carry.
// Within each tier the pre-existing least-recently-woken order must be
// unchanged -- this only moves the tier boundary, never reorders inside a
// tier (verified here by giving the re-wake tier the same relative ordering
// TestWakeFairnessInfoTwinCharacterization already pins).
func TestSortCandidatesByWakeFairness_PendingCreatesPrecedeRewakes(t *testing.T) {
	base := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)

	beadWithMeta := func(id string, created time.Time, meta map[string]string) beads.Bead {
		return beads.Bead{
			ID:        id,
			Type:      session.BeadType,
			Title:     "worker",
			Labels:    []string{session.LabelSession},
			CreatedAt: created,
			Metadata:  meta,
		}
	}
	candidateFor := func(bead beads.Bead) startCandidate {
		return startCandidate{info: sessiontest.SeedBead(t, bead)}
	}

	// Two re-wakes with a well-established relative order (ga-old before
	// ga-new is wrong; wake fairness wants the OLDER last_woke_at first).
	rewakeOld := candidateFor(beadWithMeta("ga-rewake-old", base.Add(-3*time.Hour), map[string]string{
		"template": "worker", "last_woke_at": base.Add(-2 * time.Hour).Format(time.RFC3339),
	}))
	rewakeNew := candidateFor(beadWithMeta("ga-rewake-new", base.Add(-3*time.Hour), map[string]string{
		"template": "worker", "last_woke_at": base.Add(-10 * time.Minute).Format(time.RFC3339),
	}))
	// A pending-create minted AFTER both re-wakes' last_woke_at (so on the old
	// single-tier fairness sort it would land LAST, behind both re-wakes --
	// exactly the ga-r6lc7g starvation pattern).
	pendingCreate := candidateFor(beadWithMeta("ga-wisp-6p5f0b", base, map[string]string{
		"template": "worker", "pending_create_claim": "true", "state": "creating",
	}))
	if !pendingCreate.info.PendingCreateClaim {
		t.Fatal("test setup: expected pendingCreate candidate to carry PendingCreateClaim=true")
	}

	cands := []startCandidate{rewakeOld, rewakeNew, pendingCreate}
	sortCandidatesByWakeFairness(cands)
	gotOrder := []string{cands[0].info.ID, cands[1].info.ID, cands[2].info.ID}
	wantOrder := []string{"ga-wisp-6p5f0b", "ga-rewake-old", "ga-rewake-new"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("fairness sort order = %v, want %v (pending-create must lead, re-wake relative order must be unchanged)", gotOrder, wantOrder)
		}
	}
}

// TestSortCandidatesByWakeFairness_RewakeStarvedBySustainedPendingCreates was
// a KNOWN-LIMITATION pin (ga-8nuqlp): sortCandidatesByWakeFairness's strict
// categorical priority for the pending-create tier had no fairness floor, so
// a re-wake could be starved indefinitely by sustained pending-create arrival
// at or above the per-tick wake budget (DefaultMaxWakesPerTick = 5,
// internal/config/config.go) -- not just deferred to "the next tick". This is
// the finding's own reproduction: one re-wake starved 6h against 5
// freshly-minted pending-creates.
//
// FLIPPED (ga-yjbz92, the FULL deliverable): applyRewakeFairnessFloor now
// reserves a re-wake slot whenever the budget would otherwise go entirely to
// pending-creates, so this asserts the fix instead of the gap -- the starved
// re-wake must land INSIDE the budget, not past it.
func TestSortCandidatesByWakeFairness_RewakeStarvedBySustainedPendingCreates(t *testing.T) {
	base := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)

	beadWithMeta := func(id string, created time.Time, meta map[string]string) beads.Bead {
		return beads.Bead{
			ID: id, Type: session.BeadType, Title: "worker",
			Labels: []string{session.LabelSession}, CreatedAt: created, Metadata: meta,
		}
	}
	candidateFor := func(bead beads.Bead) startCandidate {
		return startCandidate{info: sessiontest.SeedBead(t, bead)}
	}

	starvedRewake := candidateFor(beadWithMeta("ga-rewake-starved-6h", base.Add(-6*time.Hour), map[string]string{
		"template": "worker", "last_woke_at": base.Add(-6 * time.Hour).Format(time.RFC3339),
	}))

	cands := []startCandidate{starvedRewake}
	for i := 0; i < config.DefaultMaxWakesPerTick; i++ {
		id := "ga-wisp-fresh-" + string(rune('a'+i))
		cands = append(cands, candidateFor(beadWithMeta(id, base, map[string]string{
			"template": "worker", "pending_create_claim": "true", "state": "creating",
		})))
	}

	sortCandidatesByWakeFairness(cands)
	applyRewakeFairnessFloor(cands, config.DefaultMaxWakesPerTick)

	starvedRank := -1
	for i, c := range cands {
		if c.info.ID == "ga-rewake-starved-6h" {
			starvedRank = i
			break
		}
	}
	if starvedRank < 0 {
		t.Fatal("test setup: starved re-wake candidate went missing from the sorted slice")
	}
	if starvedRank >= config.DefaultMaxWakesPerTick {
		t.Fatalf("starved re-wake landed at rank %d, outside the %d-wide per-tick budget -- "+
			"the fairness floor (ga-yjbz92) did not serve it", starvedRank, config.DefaultMaxWakesPerTick)
	}
	// The floor must not overcorrect: exactly one budget slot goes to the
	// re-wake, the rest of the budget prefix is still pending-creates.
	pendingInBudget := 0
	for i := 0; i < config.DefaultMaxWakesPerTick; i++ {
		if cands[i].info.PendingCreateClaim {
			pendingInBudget++
		}
	}
	if want := config.DefaultMaxWakesPerTick - 1; pendingInBudget != want {
		t.Fatalf("pending-creates inside budget = %d, want %d (exactly one budget slot reserved for the re-wake)", pendingInBudget, want)
	}
}

// TestSortCandidatesByWakeFairness_RewakeFloorBoundedAcrossSustainedTicks is
// the sustained-pressure case the ga-8nuqlp FULL acceptance criteria asked
// for beyond the single-tick pin above: pending-creates keep arriving at or
// above budget for N consecutive ticks, and the starved re-wake's wait must
// stay bounded independent of N -- not merely served once by luck on one
// tick. applyRewakeFairnessFloor is a pure per-call permutation with no
// state carried between calls, so the guarantee it gives on tick 1 must
// reproduce identically on every later tick; this exercises that directly
// rather than trusting it from the mechanism alone.
func TestSortCandidatesByWakeFairness_RewakeFloorBoundedAcrossSustainedTicks(t *testing.T) {
	base := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)

	beadWithMeta := func(id string, created time.Time, meta map[string]string) beads.Bead {
		return beads.Bead{
			ID: id, Type: session.BeadType, Title: "worker",
			Labels: []string{session.LabelSession}, CreatedAt: created, Metadata: meta,
		}
	}
	candidateFor := func(bead beads.Bead) startCandidate {
		return startCandidate{info: sessiontest.SeedBead(t, bead)}
	}

	const sustainedTicks = 50
	starvedRewake := candidateFor(beadWithMeta("ga-rewake-sustained-starved", base.Add(-6*time.Hour), map[string]string{
		"template": "worker", "last_woke_at": base.Add(-6 * time.Hour).Format(time.RFC3339),
	}))

	for tick := 0; tick < sustainedTicks; tick++ {
		// A fresh batch of above-budget pending-creates every tick, simulating
		// sustained arrival rather than one static snapshot -- the candidate
		// set a real tick would re-derive from current session state each
		// time it runs.
		cands := []startCandidate{starvedRewake}
		for i := 0; i < config.DefaultMaxWakesPerTick+2; i++ {
			id := "ga-wisp-tick" + string(rune('0'+tick%10)) + "-" + string(rune('a'+i))
			cands = append(cands, candidateFor(beadWithMeta(id, base, map[string]string{
				"template": "worker", "pending_create_claim": "true", "state": "creating",
			})))
		}

		sortCandidatesByWakeFairness(cands)
		applyRewakeFairnessFloor(cands, config.DefaultMaxWakesPerTick)

		starvedRank := -1
		for i, c := range cands {
			if c.info.ID == "ga-rewake-sustained-starved" {
				starvedRank = i
				break
			}
		}
		if starvedRank < 0 {
			t.Fatalf("tick %d: starved re-wake candidate went missing", tick)
		}
		if starvedRank >= config.DefaultMaxWakesPerTick {
			t.Fatalf("tick %d: starved re-wake landed at rank %d, outside the %d-wide budget -- "+
				"wait is unbounded, not merely deferred one tick", tick, starvedRank, config.DefaultMaxWakesPerTick)
		}
	}
}

// TestEnumeratedStartCandidateSummaries pins the ga-r6lc7g trace-visibility
// addition: every candidate this tick considered is named (id, session name,
// pending-create flag) so a future incident can directly answer "was this
// bead even enumerated this tick" from the trace, instead of inferring it
// from the absence of a downstream record.
func TestEnumeratedStartCandidateSummaries(t *testing.T) {
	base := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	beadWithMeta := func(id string, created time.Time, meta map[string]string) beads.Bead {
		return beads.Bead{
			ID: id, Type: session.BeadType, Title: "worker",
			Labels: []string{session.LabelSession}, CreatedAt: created, Metadata: meta,
		}
	}
	candidateFor := func(bead beads.Bead) startCandidate {
		return startCandidate{info: sessiontest.SeedBead(t, bead)}
	}

	t.Run("empty input returns nil, not an empty slice", func(t *testing.T) {
		if got := enumeratedStartCandidateSummaries(nil); got != nil {
			t.Errorf("enumeratedStartCandidateSummaries(nil) = %#v, want nil", got)
		}
	})

	t.Run("records id, session name, and pending-create flag", func(t *testing.T) {
		pc := candidateFor(beadWithMeta("ga-wisp-6p5f0b", base, map[string]string{
			"template": "worker", "pending_create_claim": "true", "session_name": "np51-probe",
		}))
		rw := candidateFor(beadWithMeta("ga-rewake", base, map[string]string{
			"template": "worker", "session_name": "persona-nils-4-pool",
		}))
		out := enumeratedStartCandidateSummaries([]startCandidate{pc, rw})
		if len(out) != 2 {
			t.Fatalf("len(out) = %d, want 2", len(out))
		}
		if out[0]["id"] != "ga-wisp-6p5f0b" || out[0]["pending_create"] != true {
			t.Errorf("out[0] = %#v, want id=ga-wisp-6p5f0b pending_create=true", out[0])
		}
		if out[1]["id"] != "ga-rewake" || out[1]["pending_create"] != false {
			t.Errorf("out[1] = %#v, want id=ga-rewake pending_create=false", out[1])
		}
	})

	t.Run("caps at maxEnumeratedCandidateSummaries and notes the remainder", func(t *testing.T) {
		var many []startCandidate
		for i := 0; i < maxEnumeratedCandidateSummaries+5; i++ {
			many = append(many, candidateFor(beadWithMeta("ga-c"+string(rune('a'+i%26)), base, map[string]string{
				"template": "worker",
			})))
		}
		out := enumeratedStartCandidateSummaries(many)
		if len(out) != maxEnumeratedCandidateSummaries+1 {
			t.Fatalf("len(out) = %d, want %d (cap + 1 truncation marker)", len(out), maxEnumeratedCandidateSummaries+1)
		}
		last := out[len(out)-1]
		if remaining, ok := last["truncated_remaining"].(int); !ok || remaining != 5 {
			t.Errorf("truncation marker = %#v, want truncated_remaining=5", last)
		}
	})
}

func mustParseRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return parsed
}
