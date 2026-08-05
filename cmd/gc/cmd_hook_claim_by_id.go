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
// claimFirstEligibleHookCandidate, reportHookClaimRejected — unmodified. The
// claim primitive, the existing/ready-assignment adoption rules, and the
// bead.claim_rejected audit event (ADR-0009) are all inherited from that
// code, not reimplemented here. This does NOT touch claim eligibility: an
// awaiting-parked bead is still claimable per ratified ruling ga-982pdy R1,
// exactly as on the pool path.
//
// One by-ID-specific addition: when the initial read already shows the bead
// held by someone else, claimFirstEligibleHookCandidate's own
// hookCandidateClaimable pre-filter would skip it without ever attempting a
// claim, so its reportHookClaimRejected call would never run — silently
// missing the audit event for what is actually the dominant real-world
// collision shape on this path (a sibling discovers the bead sometime after
// another already claimed it). This calls reportHookClaimRejected directly
// for that known-already-held case, mutually exclusive with the
// claimFirstEligibleHookCandidate branch so it cannot double-report.
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

	claimsErrored := false
	if hookCandidateClaimable(bead, opts.RouteTargets) {
		claimResult := claimFirstEligibleHookCandidate(candidates, opts, ops, dir, stdout, stderr)
		if claimResult.terminal {
			return claimResult.code
		}
		claimsErrored = claimResult.claimsErrored
	} else if strings.TrimSpace(bead.Assignee) != "" {
		// Already held by someone else at the moment of our initial read, so
		// claimFirstEligibleHookCandidate's own hookCandidateClaimable pre-filter
		// (assignee must be empty) would skip this single candidate without ever
		// attempting ops.Claim — meaning its reportHookClaimRejected call would never
		// run and the bead.claim_rejected audit event (ADR-0009) would silently not
		// fire for this loser, even though the event is advertised as inherited from
		// that same code path. This is not a rare timing edge: it is the DOMINANT
		// real-world shape of a collision on this path (a sibling discovers the
		// routed bead via search/mail/handoff-watchdog sometime after a different
		// sibling already claimed it, not at the exact instant of the winning
		// write) — the narrow simultaneous-read window is the only case that still
		// reaches claimFirstEligibleHookCandidate's own reporting. Report the
		// rejection directly for this known-already-held case, once, before falling
		// through to the refusal writer, so every loser is audited the same way
		// regardless of which side of that window it landed on. Mutually exclusive
		// with the branch above, so this cannot double-report a race that
		// claimFirstEligibleHookCandidate already did.
		reportHookClaimRejected(bead, bead, opts, ops)
	}

	return writeHookClaimByIDRefused(store, bead, opts, claimsErrored, stdout, stderr)
}

// writeHookClaimByIDRefused reports that the single targeted bead could not
// be claimed: every step in doHookClaimByID fell through, meaning it was
// neither an existing/ready assignment nor won by a fresh claim attempt (not
// currently open, not routed to this session, the claim raced and lost, or
// the claim mutation itself errored — see claimsErrored).
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
//
// claimsErrored distinguishes a genuine lost race from an operational claim
// mutation failure (transient store/write error) on the by-ID path's single
// candidate — mirroring the pool path's no_work vs. claims_errored split
// (writeHookClaimNoWork) instead of reporting both as the same
// "claim_conflict" reason, which a machine JSON consumer could not tell
// apart (ga-64l8t8 validation V6). The underlying error itself is already
// logged by claimFirstEligibleHookCandidate's own "skipping ...: err" line;
// this only makes the terminal result reflect it too.
func writeHookClaimByIDRefused(store beads.Store, bead beads.Bead, opts hookClaimOptions, claimsErrored bool, stdout, stderr io.Writer) int {
	current := bead
	if fresh, err := store.Get(bead.ID); err == nil {
		current = fresh
	}
	holder := strings.TrimSpace(current.Assignee)
	reason := "claim_conflict"
	if claimsErrored {
		reason = hookClaimReasonClaimsErrored
	}
	result := hookClaimJSONResult{
		SchemaVersion: "1",
		OK:            false,
		Command:       hookClaimCommandName,
		Action:        "drain",
		Reason:        reason,
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
		if claimsErrored {
			fmt.Fprintf(stderr, "gc hook --claim --id: could not claim %s: the claim mutation errored (transient store/write failure, see prior log line) — not a conflict\n", current.ID) //nolint:errcheck
			return 1
		}
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
