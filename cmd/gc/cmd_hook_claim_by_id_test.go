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
			return beads.Bead{}, false, nil
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

	ops := testHookClaimOpsFor(store, newMutexClaimFunc(store))
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
}

// TestDoHookClaimByID_RefusalFallsBackToRawAssignee covers the case where the
// current holder cannot be resolved to any live session bead (a hand-set or
// stale value) — the refusal must still fire, falling back to the raw string
// rather than silently succeeding or panicking.
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

	ops := testHookClaimOpsFor(store, newMutexClaimFunc(store))
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
}

// TestDoHookClaimByID_ConcurrentRace_ExactlyOneWinner is the positive control
// the design doc's §5 asks for: construct the race deliberately with real
// goroutines sharing one atomic claim primitive, and show exactly one wins
// while every other caller receives a deterministic refusal — never a silent
// no-op, and never a second successful claim.
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
}
