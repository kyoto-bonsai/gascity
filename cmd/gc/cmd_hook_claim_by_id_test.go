package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// newMutexClaimFunc returns a hookClaimFunc backed by a real mutex-guarded
// compare-and-swap against store: it succeeds (assignee = actor, status =
// in_progress) iff the bead is currently unassigned or already assigned to
// actor, and fails (conflict) otherwise. This mirrors the "empty OR mine"
// contract the vendored bd tool's own ClaimIssueInTx implements as a single
// transactional SQL UPDATE (github.com/steveyegge/beads,
// internal/storage/issueops/claim.go — verified by direct source read for
// ga-64l8t8, not assumed).
//
// On conflict it returns the current populated bead (not a zero-value one) —
// matching production's actual ops.Claim, hookClaimWithBdStore, which
// re-reads and returns the current bead on a lost race so the caller can
// name the winner in the bead.claim_rejected event (ADR-0009). An earlier
// version of this mock returned beads.Bead{} on conflict, silently mirroring
// the vendored bd tool's own lower-level BdStore.Claim instead of the
// production wrapper actually wired as ops.Claim — ga-64l8t8 validation V2
// caught that every test built on it therefore drove reportHookClaimRejected
// down its existing == "" early-return branch, leaving the claim_rejected
// emission unexercised by any test despite being advertised three times
// (commit message, bead comment, doc comment). See
// TestDoHookClaimByID_ConcurrentRace_ExactlyOneWinner, which asserts the
// event directly, for the coverage that was missing (ga-64l8t8 validation
// F5: this comment previously cited a test name,
// TestDoHookClaimByID_ConcurrentRace_LosersReportRejectionWithWinner, that
// was never added — the coverage always lived in _ExactlyOneWinner).
//
// The mutex stands in for that SQL transaction boundary so a test built on
// this is a genuine concurrency exercise of doHookClaimByID's OWN
// orchestration code (candidate wrapping, existing/ready/fresh-claim
// sequencing, refusal reporting) — it is not a re-proof of bd's own SQL
// atomicity, which is a different codebase's guarantee and was verified
// separately, from source, not by a Go-level test here.
func newMutexClaimFunc(store beads.Store) hookClaimFunc {
	var mu sync.Mutex
	return func(_ context.Context, _ string, _ []string, beadID, actor string) (beads.Bead, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		current, err := store.Get(beadID)
		if err != nil {
			return beads.Bead{}, false, err
		}
		assignee := strings.TrimSpace(current.Assignee)
		if assignee != "" && assignee != actor {
			return current, false, nil
		}
		status := "in_progress"
		if err := store.Update(beadID, beads.UpdateOpts{Assignee: &actor, Status: &status}); err != nil {
			return beads.Bead{}, false, err
		}
		updated, err := store.Get(beadID)
		if err != nil {
			return beads.Bead{}, false, err
		}
		return updated, true, nil
	}
}

func testHookClaimOpsFor(store beads.Store, claim hookClaimFunc) hookClaimOps {
	return hookClaimOps{
		Claim:             claim,
		Store:             func(string, []string, string) beads.Store { return store },
		EmitClaimRejected: func(string, string, string) {},
		ResolveWorkBranch: func(string) string { return "" },
	}
}

// claimRejectedRecorder captures bead.claim_rejected calls (hookEmitClaimRejectedFunc's
// beadID, existingClaimant, attemptedClaimant) so a test can assert on them
// instead of wiring the usual no-op stub. Safe for concurrent use: it is
// exercised by every goroutine in a deliberate multi-sibling race.
type claimRejectedRecorder struct {
	mu    sync.Mutex
	calls []claimRejectedCall
}

type claimRejectedCall struct {
	beadID, existing, attempted string
}

func newClaimRejectedRecorder() *claimRejectedRecorder {
	return &claimRejectedRecorder{}
}

func (r *claimRejectedRecorder) record(beadID, existingClaimant, attemptedClaimant string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, claimRejectedCall{beadID: beadID, existing: existingClaimant, attempted: attemptedClaimant})
}

func (r *claimRejectedRecorder) snapshot() []claimRejectedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]claimRejectedCall, len(r.calls))
	copy(out, r.calls)
	return out
}

func TestDoHookClaimByID_ClaimsUnassignedRoutedBead(t *testing.T) {
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{
		Title:    "routed work",
		Metadata: map[string]string{"gc.routed_to": "persona-nils"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ops := testHookClaimOpsFor(store, newMutexClaimFunc(store))
	opts := hookClaimOptions{
		Assignee:     "persona-nils-ga-me",
		RouteTargets: []string{"persona-nils"},
		JSON:         true,
	}
	var stdout, stderr bytes.Buffer
	code := doHookClaimByID(target.ID, "/work", opts, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaimByID() = %d, want 0; stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout not JSON: %v; raw=%s", err, stdout.String())
	}
	if !result.OK || result.Assignee != opts.Assignee || result.BeadID != target.ID {
		t.Fatalf("unexpected result: %+v", result)
	}
	got, err := store.Get(target.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Assignee != opts.Assignee || got.Status != "in_progress" {
		t.Fatalf("bead after claim = %+v, want assignee=%q status=in_progress", got, opts.Assignee)
	}
}

// TestDoHookClaimByID_IdempotentResume covers the same session re-running
// --claim --id against a bead it already owns (retry after a transient
// failure, or a deliberate resume) — must succeed as a no-op, not be treated
// as a collision with itself. This is the design doc's §5 negative control.
func TestDoHookClaimByID_IdempotentResume(t *testing.T) {
	store := beads.NewMemStore()
	me := "persona-nils-ga-me"
	target, err := store.Create(beads.Bead{
		Title:    "routed work, already mine",
		Metadata: map[string]string{"gc.routed_to": "persona-nils"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	status := "in_progress"
	if err := store.Update(target.ID, beads.UpdateOpts{Assignee: &me, Status: &status}); err != nil {
		t.Fatalf("seed existing assignment: %v", err)
	}

	ops := testHookClaimOpsFor(store, newMutexClaimFunc(store))
	opts := hookClaimOptions{
		Assignee:     me,
		RouteTargets: []string{"persona-nils"},
		JSON:         true,
	}
	var stdout, stderr bytes.Buffer
	code := doHookClaimByID(target.ID, "/work", opts, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaimByID(resume) = %d, want 0; stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout not JSON: %v; raw=%s", err, stdout.String())
	}
	if result.Reason != "existing_assignment" || result.Assignee != me {
		t.Fatalf("unexpected result: %+v", result)
	}
}

// TestDoHookClaimByID_RefusalNamesLiveSibling covers the core ga-64l8t8 ask:
// when the bead is already held by a DIFFERENT live session, the refusal
// names that session (via resolveLiveSessionAssignment) instead of silently
// letting the caller overwrite it or reporting an unattributed failure.
//
// It also pins C2 of ga-64l8t8 validation F4: this already-held path shares
// one aggregate assertion (len(calls) != n-1) with the fresh-claim path in
// TestDoHookClaimByID_ConcurrentRace_ExactlyOneWinner, so a regression that
// deletes the already-held branch's own reportHookClaimRejected call can
// still pass that shared assertion whenever the race's nondeterministic
// read/write split happens to route every loser through the other path
// (vega's ablation measured this at 16% escape over 50 runs). Wiring the
// recorder here, deterministically, rather than the no-op stub every other
// non-F4 test in this file uses, closes that gap for this path specifically.
func TestDoHookClaimByID_RefusalNamesLiveSibling(t *testing.T) {
	store := beads.NewMemStore()
	holderIdentity := "persona-nils-ga-livesibling"
	if _, err := store.Create(beads.Bead{
		Title:    "the live holder's own session bead",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: map[string]string{"session_name": holderIdentity},
	}); err != nil {
		t.Fatalf("Create holder session bead: %v", err)
	}
	target, err := store.Create(beads.Bead{
		Title:    "routed work, held by a live sibling",
		Metadata: map[string]string{"gc.routed_to": "persona-nils"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	status := "in_progress"
	if err := store.Update(target.ID, beads.UpdateOpts{Assignee: &holderIdentity, Status: &status}); err != nil {
		t.Fatalf("seed existing assignment: %v", err)
	}

	rec := newClaimRejectedRecorder()
	ops := testHookClaimOpsFor(store, newMutexClaimFunc(store))
	ops.EmitClaimRejected = rec.record
	opts := hookClaimOptions{
		Assignee:     "persona-nils-ga-me",
		RouteTargets: []string{"persona-nils"},
	}
	var stdout, stderr bytes.Buffer
	code := doHookClaimByID(target.ID, "/work", opts, ops, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doHookClaimByID() = 0, want non-zero (bead is held by a different live session); stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), holderIdentity) {
		t.Errorf("stderr = %q, want it to name the live holder %q", stderr.String(), holderIdentity)
	}
	if !strings.Contains(strings.ToLower(stderr.String()), "stand down") {
		t.Errorf("stderr = %q, want a stand-down instruction", stderr.String())
	}
	// The refusal must not have mutated the bead.
	got, err := store.Get(target.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Assignee != holderIdentity {
		t.Fatalf("assignee after refused claim = %q, want unchanged %q", got.Assignee, holderIdentity)
	}
	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("EmitClaimRejected fired %d times, want exactly 1; calls=%+v", len(calls), calls)
	}
	if calls[0].existing != holderIdentity || calls[0].attempted != opts.Assignee {
		t.Errorf("claim_rejected = %+v, want existing=%q attempted=%q", calls[0], holderIdentity, opts.Assignee)
	}
}

// TestDoHookClaimByID_RefusalFallsBackToRawAssignee covers the case where the
// current holder cannot be resolved to any live session bead (a hand-set or
// stale value) — the refusal must still fire, falling back to the raw string
// rather than silently succeeding or panicking.
//
// Also pins C2 of ga-64l8t8 validation F4, same reasoning as the sibling
// case in TestDoHookClaimByID_RefusalNamesLiveSibling above: this path
// shares the concurrent-race test's aggregate event count, so it needs its
// own deterministic assertion to guard against a regression the shared
// count could silently absorb.
func TestDoHookClaimByID_RefusalFallsBackToRawAssignee(t *testing.T) {
	store := beads.NewMemStore()
	staleHolder := "persona-marcus" // bare persona-type string, no session ever claimed it
	target, err := store.Create(beads.Bead{
		Title:    "routed work, hand-set to a bare persona type",
		Assignee: staleHolder,
		Metadata: map[string]string{"gc.routed_to": "persona-marcus"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec := newClaimRejectedRecorder()
	ops := testHookClaimOpsFor(store, newMutexClaimFunc(store))
	ops.EmitClaimRejected = rec.record
	opts := hookClaimOptions{
		Assignee:     "persona-marcus-ga-me",
		RouteTargets: []string{"persona-marcus"},
	}
	var stdout, stderr bytes.Buffer
	code := doHookClaimByID(target.ID, "/work", opts, ops, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doHookClaimByID() = 0, want non-zero; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), staleHolder) {
		t.Errorf("stderr = %q, want it to fall back to the raw holder string %q", stderr.String(), staleHolder)
	}
	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("EmitClaimRejected fired %d times, want exactly 1; calls=%+v", len(calls), calls)
	}
	if calls[0].existing != staleHolder || calls[0].attempted != opts.Assignee {
		t.Errorf("claim_rejected = %+v, want existing=%q attempted=%q", calls[0], staleHolder, opts.Assignee)
	}
}

// TestDoHookClaimByID_ConcurrentRace_ExactlyOneWinner is the positive control
// the design doc's §5 asks for: construct the race deliberately with real
// goroutines sharing one atomic claim primitive, and show exactly one wins
// while every other caller receives a deterministic refusal — never a silent
// no-op, and never a second successful claim. It also asserts the
// bead.claim_rejected audit event (ADR-0009) fires for every loser and names
// the true winner — the commit message, the bead comment, and this func's own
// doc comment all advertise that event as "inherited, not reimplemented" from
// the pool-worker path, but until ga-64l8t8 validation V2, no test exercised
// it: newMutexClaimFunc used to return a zero-value Bead on conflict (mirroring
// the vendored bd tool's own lower-level BdStore.Claim instead of the
// production wrapper actually wired as ops.Claim, hookClaimWithBdStore, which
// re-reads and returns the populated current bead — see that func's doc
// comment), so reportHookClaimRejected always saw an empty existing claimant
// and took its early return without ever calling EmitClaimRejected.
func TestDoHookClaimByID_ConcurrentRace_ExactlyOneWinner(t *testing.T) {
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{
		Title:    "routed work, contested by live siblings",
		Metadata: map[string]string{"gc.routed_to": "persona-nils"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	claim := newMutexClaimFunc(store)
	rejections := newClaimRejectedRecorder()

	const n = 8
	codes := make([]int, n)
	stderrs := make([]string, n)
	assignees := make([]string, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		assignees[i] = fmt.Sprintf("persona-nils-ga-sibling-%d", i)
		go func(i int) {
			defer wg.Done()
			<-start // maximize actual concurrent overlap across goroutines
			ops := testHookClaimOpsFor(store, claim)
			ops.EmitClaimRejected = rejections.record
			opts := hookClaimOptions{
				Assignee:     assignees[i],
				RouteTargets: []string{"persona-nils"},
				// Plain-text mode (not JSON): a lost race's refusal is
				// asserted on stderr below, and writeHookClaimByIDRefused
				// routes JSON-mode refusals to stdout instead (see
				// TestDoHookClaimByID_ClaimsUnassignedRoutedBead for JSON
				// mode's own coverage on the winning path).
			}
			var stdout, stderr bytes.Buffer
			codes[i] = doHookClaimByID(target.ID, "/work", opts, ops, &stdout, &stderr)
			stderrs[i] = stderr.String()
		}(i)
	}
	close(start)
	wg.Wait()

	wins, losses := 0, 0
	var winnerIdentity string
	for i, code := range codes {
		if code == 0 {
			wins++
			winnerIdentity = assignees[i]
			continue
		}
		losses++
		if stderrs[i] == "" {
			t.Errorf("sibling %d: lost the race but wrote no refusal to stderr", i)
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1 (losses=%d, codes=%v)", wins, losses, codes)
	}

	// Every loser must have generated exactly one claim_rejected event naming
	// the true winner — not zero (the pre-fix gap), not a stale intermediate
	// claimant (there should be none, since newMutexClaimFunc re-reads under
	// the same mutex that performs the write), and not duplicated.
	calls := rejections.snapshot()
	if len(calls) != n-1 {
		t.Fatalf("EmitClaimRejected fired %d times, want %d (one per loser); calls=%+v", len(calls), n-1, calls)
	}
	seenAttempted := make(map[string]int, n-1)
	for _, c := range calls {
		if c.beadID != target.ID {
			t.Errorf("claim_rejected beadID = %q, want %q", c.beadID, target.ID)
		}
		if c.existing != winnerIdentity {
			t.Errorf("claim_rejected existing claimant = %q, want the true winner %q", c.existing, winnerIdentity)
		}
		seenAttempted[c.attempted]++
	}
	for i, assignee := range assignees {
		if assignee == winnerIdentity {
			continue
		}
		if seenAttempted[assignee] != 1 {
			t.Errorf("sibling %d (%s): claim_rejected fired %d times for it, want exactly 1", i, assignee, seenAttempted[assignee])
		}
	}

	if losses != n-1 {
		t.Fatalf("losses = %d, want %d", losses, n-1)
	}

	final, err := store.Get(target.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Status != "in_progress" {
		t.Fatalf("final status = %q, want in_progress", final.Status)
	}
	if final.Assignee != winnerIdentity {
		t.Fatalf("final assignee = %q, want the one winner %q", final.Assignee, winnerIdentity)
	}
}

// TestDoHookClaimByID_NotRoutedToThisSession covers an unassigned bead whose
// gc.routed_to does not match the caller's route targets: it must be
// refused, not silently claimed outside the caller's own routing.
//
// Also covers the third shape of ga-64l8t8 validation F3: an unassigned,
// route-mismatched bead is not a claim conflict (nothing to contend for),
// so the JSON reason must be hookClaimReasonNotClaimable, not
// "claim_conflict" — before the fix, this shape and a genuine lost race
// were indistinguishable to a machine consumer of `--json`.
func TestDoHookClaimByID_NotRoutedToThisSession(t *testing.T) {
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{
		Title:    "routed to a different persona family",
		Metadata: map[string]string{"gc.routed_to": "persona-marcus"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ops := testHookClaimOpsFor(store, newMutexClaimFunc(store))
	opts := hookClaimOptions{
		Assignee:     "persona-nils-ga-me",
		RouteTargets: []string{"persona-nils"},
		JSON:         true,
	}
	var stdout, stderr bytes.Buffer
	code := doHookClaimByID(target.ID, "/work", opts, ops, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doHookClaimByID() = 0, want non-zero for an unrouted bead; stdout=%s", stdout.String())
	}
	got, err := store.Get(target.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee after refused claim = %q, want still unassigned", got.Assignee)
	}
	var result hookClaimJSONResult
	if jsonErr := json.Unmarshal(stdout.Bytes(), &result); jsonErr != nil {
		t.Fatalf("stdout not JSON: %v; raw=%s", jsonErr, stdout.String())
	}
	if result.Reason != hookClaimReasonNotClaimable {
		t.Errorf("Reason = %q, want %q (an unassigned, unrouted bead is not a claim conflict)", result.Reason, hookClaimReasonNotClaimable)
	}
}

// TestDoHookClaimByID_RouteMismatchHeldBeadRefusesWithoutClaimRejected covers
// ga-64l8t8 validation F1: a bead held by someone else, but routed to a
// DIFFERENT persona family than this session's own route targets, is not a
// collision this session could have contended for. Before the fix, the
// already-held branch gated on bare assignee-non-empty and reported
// bead.claim_rejected for this session anyway — inflating the audit event's
// collision count with attempts that were never eligible in the first
// place. The refusal must still fire (this session still cannot claim the
// bead), but it must not emit claim_rejected, and the JSON reason must be
// hookClaimReasonNotClaimable rather than "claim_conflict".
func TestDoHookClaimByID_RouteMismatchHeldBeadRefusesWithoutClaimRejected(t *testing.T) {
	store := beads.NewMemStore()
	holder := "persona-marcus-ga-holder"
	target, err := store.Create(beads.Bead{
		Title:    "held by marcus, routed to marcus, caller is nils",
		Metadata: map[string]string{"gc.routed_to": "persona-marcus"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	status := "in_progress"
	if err := store.Update(target.ID, beads.UpdateOpts{Assignee: &holder, Status: &status}); err != nil {
		t.Fatalf("seed existing assignment: %v", err)
	}

	rec := newClaimRejectedRecorder()
	ops := testHookClaimOpsFor(store, newMutexClaimFunc(store))
	ops.EmitClaimRejected = rec.record
	opts := hookClaimOptions{
		Assignee:     "persona-nils-ga-me",
		RouteTargets: []string{"persona-nils"},
		JSON:         true,
	}
	var stdout, stderr bytes.Buffer
	code := doHookClaimByID(target.ID, "/work", opts, ops, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doHookClaimByID() = 0, want non-zero (bead is routed to a different persona family); stdout=%s", stdout.String())
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Errorf("EmitClaimRejected fired %d times, want 0 (this session was never eligible to claim a route-mismatched bead); calls=%+v", len(calls), calls)
	}
	var result hookClaimJSONResult
	if jsonErr := json.Unmarshal(stdout.Bytes(), &result); jsonErr != nil {
		t.Fatalf("stdout not JSON: %v; raw=%s", jsonErr, stdout.String())
	}
	if result.Reason != hookClaimReasonNotClaimable {
		t.Errorf("Reason = %q, want %q (a route-mismatched bead is not a claim conflict)", result.Reason, hookClaimReasonNotClaimable)
	}
}

// TestDoHookClaimByID_ClosedBeadRefusesWithoutClaimRejected covers ga-64l8t8
// validation F2: a closed bead retains its assignee as its normal
// post-close shape, which officer-style dispatch (search / mail /
// handoff-watchdog) routinely surfaces as a stale pointer. No race
// occurred, so — same as the route-mismatch shape in F1 — the refusal must
// fire without a claim_rejected event, and with reason
// hookClaimReasonNotClaimable rather than "claim_conflict".
func TestDoHookClaimByID_ClosedBeadRefusesWithoutClaimRejected(t *testing.T) {
	store := beads.NewMemStore()
	holder := "persona-nils-ga-whoever"
	target, err := store.Create(beads.Bead{
		Title:    "closed, still carrying its old assignee",
		Metadata: map[string]string{"gc.routed_to": "persona-nils"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	status := "closed"
	if err := store.Update(target.ID, beads.UpdateOpts{Assignee: &holder, Status: &status}); err != nil {
		t.Fatalf("seed closed assignment: %v", err)
	}

	rec := newClaimRejectedRecorder()
	ops := testHookClaimOpsFor(store, newMutexClaimFunc(store))
	ops.EmitClaimRejected = rec.record
	opts := hookClaimOptions{
		Assignee:     "persona-nils-ga-me",
		RouteTargets: []string{"persona-nils"},
		JSON:         true,
	}
	var stdout, stderr bytes.Buffer
	code := doHookClaimByID(target.ID, "/work", opts, ops, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doHookClaimByID() = 0, want non-zero (bead is closed); stdout=%s", stdout.String())
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Errorf("EmitClaimRejected fired %d times, want 0 (a closed bead is not a live race); calls=%+v", len(calls), calls)
	}
	var result hookClaimJSONResult
	if jsonErr := json.Unmarshal(stdout.Bytes(), &result); jsonErr != nil {
		t.Fatalf("stdout not JSON: %v; raw=%s", jsonErr, stdout.String())
	}
	if result.Reason != hookClaimReasonNotClaimable {
		t.Errorf("Reason = %q, want %q (a closed bead is not a claim conflict)", result.Reason, hookClaimReasonNotClaimable)
	}
}

// erroringClaimFunc returns a hookClaimFunc whose Claim always fails with err
// and ok=false — an operational claim-mutation failure (a transient
// store/write error), not a lost race. Used to distinguish that shape from a
// genuine conflict, which newMutexClaimFunc produces instead.
func erroringClaimFunc(err error) hookClaimFunc {
	return func(_ context.Context, _ string, _ []string, _, _ string) (beads.Bead, bool, error) {
		return beads.Bead{}, false, err
	}
}

// TestDoHookClaimByID_ClaimMutationErrorReportsClaimsErrored covers
// ga-64l8t8 validation V6: when the single candidate's claim mutation itself
// errors (store contention, a controller-socket flap in the read→write
// window — not a lost race), the terminal JSON result must report
// claims_errored, not claim_conflict, so a machine consumer of `gc hook
// --claim --id --json` can tell "a live sibling holds it" apart from "the
// write failed" — mirroring the pool path's existing no_work/claims_errored
// split (writeHookClaimNoWork) instead of collapsing both refusal shapes
// into one reason on the by-ID path.
func TestDoHookClaimByID_ClaimMutationErrorReportsClaimsErrored(t *testing.T) {
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{
		Title:    "routed work, claim mutation will error",
		Metadata: map[string]string{"gc.routed_to": "persona-nils"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantErr := fmt.Errorf("transient store write failure")
	ops := testHookClaimOpsFor(store, erroringClaimFunc(wantErr))
	opts := hookClaimOptions{
		Assignee:     "persona-nils-ga-me",
		RouteTargets: []string{"persona-nils"},
		JSON:         true,
	}
	var stdout, stderr bytes.Buffer
	code := doHookClaimByID(target.ID, "/work", opts, ops, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doHookClaimByID() = 0, want non-zero on a claim mutation error; stdout=%s", stdout.String())
	}
	var result hookClaimJSONResult
	if jsonErr := json.Unmarshal(stdout.Bytes(), &result); jsonErr != nil {
		t.Fatalf("stdout not JSON: %v; raw=%s", jsonErr, stdout.String())
	}
	if result.Reason != hookClaimReasonClaimsErrored {
		t.Errorf("Reason = %q, want %q (a claim mutation error must not be reported as claim_conflict)", result.Reason, hookClaimReasonClaimsErrored)
	}
	if result.OK {
		t.Errorf("OK = true, want false on a claim mutation error")
	}
	// The underlying error is still surfaced on stderr by
	// claimFirstEligibleHookCandidate's own "skipping ...: err" line — this
	// test only adds the terminal JSON result distinguishing the reason.
	if !strings.Contains(stderr.String(), wantErr.Error()) {
		t.Errorf("stderr = %q, want it to surface the underlying error %q", stderr.String(), wantErr.Error())
	}
}
