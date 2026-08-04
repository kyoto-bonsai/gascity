package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestHookClaimWithBdStoreReloadsCanonicalBeadAfterPartialMutation(t *testing.T) {
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })

	var calls [][]string
	hookClaimCommandRunnerWithEnvContext = func(_ context.Context, _ map[string]string) beads.CommandRunner {
		return func(_ string, name string, args ...string) ([]byte, error) {
			if name != "bd" {
				t.Fatalf("command name = %q, want bd", name)
			}
			calls = append(calls, append([]string(nil), args...))
			switch {
			case reflect.DeepEqual(args, []string{"update", "work-1", "--claim", "--json"}):
				return []byte(`[{"id":"work-1","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"rig/worker"}}]`), nil
			case reflect.DeepEqual(args, []string{"show", "--json", "work-1"}):
				return []byte(`[{"id":"work-1","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"rig/worker","gc.root_bead_id":"root-1","gc.continuation_group":"review"}}]`), nil
			default:
				t.Fatalf("unexpected bd args: %#v", args)
				return nil, nil
			}
		}
	}

	claimed, ok, err := hookClaimWithBdStore(context.Background(), "/rig", nil, "work-1", "worker-1")
	if err != nil {
		t.Fatalf("hookClaimWithBdStore: %v", err)
	}
	if !ok {
		t.Fatal("hookClaimWithBdStore ok = false, want true")
	}
	if claimed.Metadata["gc.root_bead_id"] != "root-1" || claimed.Metadata["gc.continuation_group"] != "review" {
		t.Fatalf("claimed metadata = %#v, want canonical root and continuation group", claimed.Metadata)
	}
	if len(calls) != 2 {
		t.Fatalf("bd calls = %#v, want claim update followed by canonical show", calls)
	}
}

func TestDoHookClaimStopsAfterCommittedClaimReadbackFailure(t *testing.T) {
	runner := func(string, string) (string, error) {
		return `[
			{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker"}},
			{"id":"work-2","status":"open","metadata":{"gc.routed_to":"worker"}}
		]`, nil
	}
	var attempts []string
	drained := false
	ops := hookClaimOps{
		Runner: runner,
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			attempts = append(attempts, beadID)
			return beads.Bead{ID: beadID, Assignee: assignee}, true, errors.New("canonical read failed")
		},
		DrainAck: func(io.Writer) error {
			drained = true
			return nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		DrainAck:     true,
		JSON:         true,
	}, ops, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("doHookClaim = %d, want 1", code)
	}
	if got := strings.Join(attempts, ","); got != "work-1" {
		t.Fatalf("claim attempts = %q, want only committed work-1", got)
	}
	if drained {
		t.Fatal("drain acknowledged after committed claim readback failure")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "claimed work-1 but loading canonical bead failed") {
		t.Fatalf("stderr = %q, want committed-claim diagnostic", stderr.String())
	}
}

func TestDoHookClaimUsesSelectedStoreContextForMutationAndContinuation(t *testing.T) {
	var claimedDir string
	var claimedEnv []string
	var listedDir string
	var listedEnv []string
	var assignedDir string
	var assignedEnv []string
	var assignedBead string

	storeDir := "rig-store"
	storeEnv := []string{"BEADS_DIR=rig-store", "GC_RIG_ROOT=rig-root"}
	candidates := []beads.Bead{{
		ID:       "bead-1",
		Status:   "open",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.run_target": "route-1", "gc.root_bead_id": "root-1", "gc.continuation_group": "group-a"},
	}}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimedDir = dir
			claimedEnv = append([]string(nil), env...)
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress", Metadata: candidates[0].Metadata}, true, nil
		},
		ListContinuation: func(_ context.Context, dir string, env []string, rootID, group string) ([]beads.Bead, error) {
			listedDir = dir
			listedEnv = append([]string(nil), env...)
			if rootID != "root-1" || group != "group-a" {
				t.Fatalf("continuation lookup = (%q, %q), want (root-1, group-a)", rootID, group)
			}
			return []beads.Bead{{ID: "sib-1", Status: "open", Metadata: candidates[0].Metadata}}, nil
		},
		AssignContinuation: func(_ context.Context, dir string, env []string, beadID, assignee string) error {
			assignedDir = dir
			assignedEnv = append([]string(nil), env...)
			assignedBead = beadID
			if assignee != "worker-1" {
				t.Fatalf("assignee = %q, want worker-1", assignee)
			}
			return nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", storeDir, hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"route-1"},
		Env:                storeEnv,
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if claimedDir != storeDir {
		t.Fatalf("claimedDir = %q, want %q", claimedDir, storeDir)
	}
	if listedDir != storeDir {
		t.Fatalf("listedDir = %q, want %q", listedDir, storeDir)
	}
	if assignedDir != storeDir {
		t.Fatalf("assignedDir = %q, want %q", assignedDir, storeDir)
	}
	if !reflect.DeepEqual(claimedEnv, storeEnv) {
		t.Fatalf("claimedEnv = %#v, want %#v", claimedEnv, storeEnv)
	}
	if !reflect.DeepEqual(listedEnv, storeEnv) {
		t.Fatalf("listedEnv = %#v, want %#v", listedEnv, storeEnv)
	}
	if !reflect.DeepEqual(assignedEnv, storeEnv) {
		t.Fatalf("assignedEnv = %#v, want %#v", assignedEnv, storeEnv)
	}
	if assignedBead != "sib-1" {
		t.Fatalf("assignedBead = %q, want sib-1", assignedBead)
	}
}

// TestDoHookClaimSkipsBlockedRoutedHeadAndClaimsReadyBehindIt guards the
// widened-routed-tier fix: a routed tier's oldest candidate can be
// is_blocked (e.g. gated on a PR), and the hook must fall through to a
// Ready routed bead behind it rather than idle-exiting on the blocked head.
func TestDoHookClaimSkipsBlockedRoutedHeadAndClaimsReadyBehindIt(t *testing.T) {
	candidates := []beads.Bead{
		{ID: "blocked-head", Status: "open", IsBlocked: boolPtr(true), Metadata: map[string]string{"gc.routed_to": "route-1"}},
		{ID: "ready-behind", Status: "open", Metadata: map[string]string{"gc.routed_to": "route-1"}},
	}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	var claimedBead string
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimedBead = beadID
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress"}, true, nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if claimedBead != "ready-behind" {
		t.Fatalf("claimedBead = %q, want ready-behind (blocked-head must be skipped)", claimedBead)
	}
}

// The tests below pin ruling ga-982pdy section 6 (R3): hookClaimJSONResult
// surfaces gc.awaiting, verbatim and unparsed, at all three construction
// sites, so a claiming session is told why a bead is parked instead of
// reconstructing the gate from the comment thread.

func TestDoHookClaimSurfacesAwaitingOnFreshClaim(t *testing.T) {
	candidates := []beads.Bead{
		{ID: "bead-1", Status: "open", Metadata: map[string]string{"gc.routed_to": "route-1", "gc.awaiting": "validator"}},
	}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress", Metadata: candidates[0].Metadata}, true, nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}

	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decoding stdout JSON: %v; stdout=%s", err, stdout.String())
	}
	if result.Reason != "claimed" {
		t.Fatalf("reason = %q, want claimed", result.Reason)
	}
	if result.Awaiting != "validator" {
		t.Fatalf("awaiting = %q, want validator", result.Awaiting)
	}
}

func TestDoHookClaimSurfacesAwaitingOnExistingInProgressAssignment(t *testing.T) {
	candidates := []beads.Bead{
		{ID: "bead-1", Status: "in_progress", Assignee: "worker-1", Metadata: map[string]string{"gc.routed_to": "route-1", "gc.awaiting": "operator_decision"}},
	}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	ops := hookClaimOps{
		Runner:   func(string, string) (string, error) { return string(output), nil },
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}

	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decoding stdout JSON: %v; stdout=%s", err, stdout.String())
	}
	if result.Reason != "existing_assignment" {
		t.Fatalf("reason = %q, want existing_assignment", result.Reason)
	}
	if result.Awaiting != "operator_decision" {
		t.Fatalf("awaiting = %q, want operator_decision", result.Awaiting)
	}
}

func TestDoHookClaimSurfacesAwaitingOnReadyAssignment(t *testing.T) {
	candidates := []beads.Bead{
		{ID: "bead-1", Status: "open", Assignee: "worker-1", Metadata: map[string]string{"gc.routed_to": "route-1", "gc.awaiting": "dependency:ga-1234"}},
	}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, true, nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}

	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decoding stdout JSON: %v; stdout=%s", err, stdout.String())
	}
	if result.Reason != "ready_assignment" {
		t.Fatalf("reason = %q, want ready_assignment", result.Reason)
	}
	if result.Awaiting != "dependency:ga-1234" {
		t.Fatalf("awaiting = %q, want dependency:ga-1234", result.Awaiting)
	}
}

// TestDoHookClaimPassesThroughOffVocabularyAwaitingValueUnmodified pins ruling
// ga-982pdy section 6, item 3: gc.awaiting is emitted verbatim, never
// validated, normalized, or mapped. "cass" was a live off-vocabulary value
// (ga-6ud310) at ruling time — vocabulary enforcement is ga-gzgggk's job on
// the producer side, not this read path's.
func TestDoHookClaimPassesThroughOffVocabularyAwaitingValueUnmodified(t *testing.T) {
	candidates := []beads.Bead{
		{ID: "bead-1", Status: "open", Metadata: map[string]string{"gc.routed_to": "route-1", "gc.awaiting": "cass"}},
	}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress", Metadata: candidates[0].Metadata}, true, nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}

	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decoding stdout JSON: %v; stdout=%s", err, stdout.String())
	}
	if result.Awaiting != "cass" {
		t.Fatalf("awaiting = %q, want verbatim passthrough of off-vocabulary value \"cass\"", result.Awaiting)
	}
}

// TestDoHookClaimStillClaimsBeadWithAwaitingSet is the executable form of
// ruling ga-982pdy R1: gc.awaiting is a routing hint, never a claim-eligibility
// gate. It must go red if a future change adds an awaiting-based exclusion to
// hookCandidateClaimable or filterUnreadyHookCandidates — verified by
// temporarily introducing exactly such an exclusion and confirming this test
// fails before reverting (see ga-8gq4ff close notes).
func TestDoHookClaimStillClaimsBeadWithAwaitingSet(t *testing.T) {
	candidates := []beads.Bead{
		{ID: "bead-1", Status: "open", Metadata: map[string]string{"gc.routed_to": "route-1", "gc.awaiting": "validator"}},
	}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	var claimedBead string
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimedBead = beadID
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress", Metadata: candidates[0].Metadata}, true, nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if claimedBead != "bead-1" {
		t.Fatalf("claimedBead = %q, want bead-1 (a bead with gc.awaiting set must remain claimable — ruling ga-982pdy R1)", claimedBead)
	}

	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decoding stdout JSON: %v; stdout=%s", err, stdout.String())
	}
	if !result.OK || result.Reason != "claimed" {
		t.Fatalf("result = %#v, want ok=true reason=claimed", result)
	}
}

// ga-kk6mke: the "claim-then-announce" half of the R1 remedy. R1 forbids
// refusing the claim itself (see TestDoHookClaimStillClaimsBeadWithAwaitingSet
// above), so the consumer this bead asks for must act on the Awaiting value
// AFTER a successful claim, not gate the claim. These three tests pin that a
// non-empty Awaiting produces a loud stderr warning on all three result sites
// (fresh pool-claim, existing in-progress assignment, ready assignment) and
// that a normal claim with no gc.awaiting stays silent (adjacent-class
// control — the warning must be conditioned on Awaiting, not fire
// unconditionally on every claim).

func TestDoHookClaimWarnsOnAwaitingParkedFreshClaim(t *testing.T) {
	candidates := []beads.Bead{
		{ID: "bead-1", Status: "open", Metadata: map[string]string{"gc.routed_to": "route-1", "gc.awaiting": "validator"}},
	}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress", Metadata: candidates[0].Metadata}, true, nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "bead-1") || !strings.Contains(stderr.String(), "PARKED") || !strings.Contains(stderr.String(), "validator") {
		t.Errorf("stderr = %q, want a PARKED warning naming bead-1 and validator", stderr.String())
	}
}

func TestDoHookClaimWarnsOnAwaitingParkedExistingAssignment(t *testing.T) {
	candidates := []beads.Bead{
		{ID: "bead-1", Status: "in_progress", Assignee: "worker-1", Metadata: map[string]string{"gc.routed_to": "route-1", "gc.awaiting": "close_decision"}},
	}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	ops := hookClaimOps{
		Runner:   func(string, string) (string, error) { return string(output), nil },
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "bead-1") || !strings.Contains(stderr.String(), "PARKED") || !strings.Contains(stderr.String(), "close_decision") {
		t.Errorf("stderr = %q, want a PARKED warning naming bead-1 and close_decision", stderr.String())
	}
}

func TestDoHookClaimNoWarningWhenNotAwaitingParked(t *testing.T) {
	candidates := []beads.Bead{
		{ID: "bead-1", Status: "open", Metadata: map[string]string{"gc.routed_to": "route-1"}},
	}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress", Metadata: candidates[0].Metadata}, true, nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "PARKED") {
		t.Errorf("stderr = %q, want no PARKED warning for a bead with no gc.awaiting", stderr.String())
	}
}

func TestWarnHookClaimAwaitingParked(t *testing.T) {
	cases := map[string]struct {
		result   hookClaimJSONResult
		wantWarn bool
	}{
		"empty awaiting":      {result: hookClaimJSONResult{BeadID: "b1", Awaiting: ""}, wantWarn: false},
		"whitespace awaiting": {result: hookClaimJSONResult{BeadID: "b1", Awaiting: "   "}, wantWarn: false},
		"validator":           {result: hookClaimJSONResult{BeadID: "b1", Awaiting: "validator"}, wantWarn: true},
		"off-vocabulary":      {result: hookClaimJSONResult{BeadID: "b1", Awaiting: "cass"}, wantWarn: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			warnHookClaimAwaitingParked(tc.result, &stderr)
			got := strings.Contains(stderr.String(), "PARKED")
			if got != tc.wantWarn {
				t.Errorf("warned = %v, want %v; stderr=%q", got, tc.wantWarn, stderr.String())
			}
		})
	}
	// A nil stderr must never panic (best-effort, same contract as every
	// other diagnostic in this file).
	warnHookClaimAwaitingParked(hookClaimJSONResult{BeadID: "b1", Awaiting: "validator"}, nil)
}
