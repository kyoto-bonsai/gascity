package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// doHookClaimByID atomically claims one specific bead for opts.Assignee,
// instead of auto-selecting from a work-query-derived candidate pool. It is
// the officer-style-dispatch counterpart to claimFirstEligibleHookCandidate:
// a session that discovered a gc.routed_to-addressed bead by search/read (not
// via the pool-worker hook-claim flow) calls this before acting, so a live
// sibling of the same persona family gets a loud, attributable refusal
// instead of a silently overwritten decision (ga-64l8t8).
//
// This wraps the single fetched bead as a one-element candidate slice and
// hands it to the exact same, already-tested pool-worker functions —
// hookClaimExistingAssignment, claimFirstReadyHookAssignment,
// claimFirstEligibleHookCandidate — unmodified. The claim primitive, the
// existing/ready-assignment adoption rules, and the bead.claim_rejected audit
// event are all inherited from that code, not reimplemented here. This does
// NOT touch claim eligibility: an awaiting-parked bead is still claimable per
// ratified ruling ga-982pdy R1, exactly as on the pool path.
//
// Only the caller's own store (dir) is checked — no cross-store federation
// like claimHookWork's stores list — matching how the caller already
// discovered this specific id (by searching that same store).
func doHookClaimByID(id, dir string, opts hookClaimOptions, ops hookClaimOps, stdout, stderr io.Writer) int {
	id = strings.TrimSpace(id)
	opts.Assignee = strings.TrimSpace(opts.Assignee)
	opts.IdentityCandidates = hookClaimIdentityCandidates(append([]string{opts.Assignee}, opts.IdentityCandidates...)...)
	opts.RouteTargets = hookClaimRouteTargets(opts.RouteTargets...)
	if id == "" {
		fmt.Fprintln(stderr, "gc hook --claim --id: bead id is empty") //nolint:errcheck
		return 1
	}
	if opts.Assignee == "" {
		fmt.Fprintln(stderr, "gc hook --claim --id: assignee not specified (set $GC_SESSION_NAME or $GC_SESSION_ID)") //nolint:errcheck
		return 1
	}
	ops.applyDefaults()

	store := ops.Store(dir, opts.Env, opts.Assignee)
	bead, err := store.Get(id)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim --id: %s: %v\n", id, err) //nolint:errcheck
		return 1
	}

	candidates := []beads.Bead{bead}
	if result, claimedBead, ok := hookClaimExistingAssignment(candidates, opts); ok {
		return writeHookClaimWorkResultForBead(result, claimedBead, opts, ops, dir, stdout, stderr)
	}
	if readyResult := claimFirstReadyHookAssignment(candidates, opts, ops, dir, stdout, stderr); readyResult.terminal {
		return readyResult.code
	}
	if claimResult := claimFirstEligibleHookCandidate(candidates, opts, ops, dir, stdout, stderr); claimResult.terminal {
		return claimResult.code
	}

	return writeHookClaimByIDRefused(store, bead, opts, stdout, stderr)
}

// writeHookClaimByIDRefused reports that the single targeted bead could not
// be claimed: every step in doHookClaimByID fell through, meaning it was
// neither an existing/ready assignment nor won by a fresh claim attempt (not
// currently open, not routed to this session, or the claim raced and lost).
//
// Unlike the pool-worker path — which silently tries the next candidate, or
// reports the shared "no_work" drain when none are left — an explicit single
// id deserves an explicit, attributable answer. This re-reads the bead
// (rather than trusting the zero-value Bead a lost race's Claim call returns,
// see BdStore.Claim's conflict path) and names the live session holding it
// when resolveLiveSessionAssignment can resolve one, falling back to the raw
// assignee string otherwise — exactly the "stand down, and say who" behavior
// ga-64l8t8 asks for, using the same identity-set matching that would have
// caught the numbered-alias near-miss documented there (see
// resolveLiveSessionAssignment's own doc comment).
func writeHookClaimByIDRefused(store beads.Store, bead beads.Bead, opts hookClaimOptions, stdout, stderr io.Writer) int {
	current := bead
	if fresh, err := store.Get(bead.ID); err == nil {
		current = fresh
	}
	holder := strings.TrimSpace(current.Assignee)
	result := hookClaimJSONResult{
		SchemaVersion: "1",
		OK:            false,
		Command:       hookClaimCommandName,
		Action:        "drain",
		Reason:        "claim_conflict",
		BeadID:        current.ID,
		Assignee:      holder,
		Route:         hookClaimRoute(current),
		Awaiting:      current.Metadata[beadmeta.AwaitingMetadataKey],
	}
	if opts.JSON {
		if err := writeCLIJSONLine(stdout, result); err != nil {
			fmt.Fprintf(stderr, "gc hook --claim --id: writing JSON: %v\n", err) //nolint:errcheck
		}
		return 1
	}
	if holder == "" {
		fmt.Fprintf(stderr, "gc hook --claim --id: could not claim %s (not open, or not routed to this session)\n", current.ID) //nolint:errcheck
		return 1
	}
	if sb, ok := resolveLiveSessionAssignment(store, holder); ok {
		identity := strings.TrimSpace(sb.Metadata["session_name"])
		if identity == "" {
			identity = sb.ID
		}
		fmt.Fprintf(stderr, "gc hook --claim --id: %s is already held by live session %s (assignee %q) — stand down\n", current.ID, identity, holder) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stderr, "gc hook --claim --id: %s is already held by %q (no live session resolved it — may be a stale/hand-set value)\n", current.ID, holder) //nolint:errcheck
	return 1
}
