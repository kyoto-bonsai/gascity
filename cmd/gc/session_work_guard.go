package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// closeSessionBeadIfUnassigned closes a session bead only when the live store
// confirms no open or in-progress work is assigned to it across the primary
// store AND any attached rig stores. Use this cross-store guard for cleanup
// paths that must not orphan work in any attached store. Reconciler paths that
// close a session according to its configured agent reachability should use
// closeSessionBeadIfReachableStoreUnassigned instead.
//
// Callers must NOT pass a pre-computed work snapshot — this helper queries the
// stores itself so its decision cannot be poisoned by a stale snapshot taken
// earlier in the tick (see the PR that retired the snapshot-based variant).
// Live-query failures fail closed: the bead stays open until assignment can be
// re-verified.
func closeSessionBeadIfUnassigned(
	cityPath string,
	store beads.Store,
	rigStores map[string]beads.Store,
	cfg *config.City,
	session beads.Bead,
	reason string,
	now time.Time,
	stderr io.Writer,
) bool {
	if stderr == nil {
		stderr = io.Discard
	}
	hasAssignedWork, err := sessionHasOpenAssignedWorkForConfig(cityPath, cfg, store, rigStores, session)
	if err != nil {
		fmt.Fprintf(stderr, "session work guard: checking assigned work for %s: %v\n", session.ID, err) //nolint:errcheck
		return false
	}
	if hasAssignedWork {
		return false
	}
	if isFailedCreateSessionBead(session) {
		return closeFailedCreateBead(sessionFrontDoor(store), session.ID, now, stderr)
	}
	return closeBead(store, session.ID, reason, now, stderr)
}

// closeSessionInfoIfUnassigned is the session.Info form of
// closeSessionBeadIfUnassigned: it closes the session identified by info only when
// the live cross-store query confirms no open or in-progress work is assigned to
// it. The identity/close reads route through the typed projection and the session
// front door (closeBead / closeFailedCreateBead, which funnel writes through
// sessionFrontDoor and run the extmsg/orphaned-work release cascade). Byte-
// identical to the raw form for the GCSweep close op.
func closeSessionInfoIfUnassigned(
	cityPath string,
	store beads.Store,
	rigStores map[string]beads.Store,
	cfg *config.City,
	info sessionpkg.Info,
	reason string,
	now time.Time,
	stderr io.Writer,
) bool {
	if stderr == nil {
		stderr = io.Discard
	}
	hasAssignedWork, err := sessionHasOpenAssignedWorkForConfigInfo(cityPath, cfg, store, rigStores, info)
	if err != nil {
		fmt.Fprintf(stderr, "session work guard: checking assigned work for %s: %v\n", info.ID, err) //nolint:errcheck
		return false
	}
	if hasAssignedWork {
		return false
	}
	if isFailedCreateSessionInfo(info) {
		return closeFailedCreateBead(sessionFrontDoor(store), info.ID, now, stderr)
	}
	return closeBead(store, info.ID, reason, now, stderr)
}

// triggerBeadClosedInfo reports whether info's TriggerBeadID names a bead
// that exists and is closed. It resolves the bead through
// TriggerBeadStoreRef when set (empty means the primary store, mirroring the
// storeRef convention releaseOrphanedPoolAssignments and the idle-claim
// backstop already use for the same field pair) — an unattached/unknown
// store ref fails closed rather than guessing at the primary store.
//
// ga-81we34: resolution goes through resolveTriggerBeadStore, which
// normalizes via normalizeIdleClaimStoreRef (idle_nudge.go) instead of a
// raw rigStores[storeRef] lookup. Production writes this field as "",
// "city", or "rig:<name>" (SessionRequest.WorkStoreRef's own documented
// shape) — never as a bare rig name — but rigStores (city_runtime.go
// rigBeadStores()) is keyed by bare rig name with the city entry deleted.
// A raw lookup on "city" or "rig:<name>" therefore always missed, silently
// skipping every such wisp; the escaped defect's own live count found 2 of
// 4 closed-trigger candidates on this exact spelling. Reusing the
// normalizer (not a second switch) is what keeps this from drifting from
// idle-claim resolution again.
//
// Empty TriggerBeadID, an unresolvable store ref, or a not-found bead all
// report (false, nil): a wisp with no known trigger, or one whose trigger
// cannot be located, is never eligible for retirement on this signal alone.
// A genuine store query error (as opposed to a clean not-found) is returned
// so the caller can fail closed AND log it, rather than silently treating a
// transient read failure the same as "trigger is open." This is a fresh,
// uncached read every call; callers must not memoize it across ticks.
func triggerBeadClosedInfo(store beads.Store, rigStores map[string]beads.Store, info sessionpkg.Info) (bool, error) {
	triggerID := strings.TrimSpace(info.TriggerBeadID)
	if triggerID == "" {
		return false, nil
	}
	target, resolved := resolveTriggerBeadStore(info.TriggerBeadStoreRef, store, rigStores)
	if !resolved || target == nil {
		return false, nil
	}
	b, err := target.Get(triggerID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return b.Status == "closed", nil
}

// resolveTriggerBeadStore resolves a TriggerBeadStoreRef spelling to the
// beads.Store it names, sharing exactly one normalization path
// (normalizeIdleClaimStoreRef) with idle-claim resolution so the two can
// never drift apart again (ga-81we34). "" and any ref that normalizes to
// "city" (also covers "city:<name>" and class refs) resolve to primary;
// "rig:<name>" (including a normalized bare rig name) resolves to
// rigStores[<name>] — rigStores itself is keyed by bare rig name, never by
// "rig:<name>" or "city" (city_runtime.go rigBeadStores() deletes the city
// entry), so the "rig:" prefix is stripped before the lookup. Anything else
// (an unrecognized ref shape) fails closed: (nil, false).
//
// Split out from triggerBeadClosedInfo (rather than inlined) so
// sweepClosedTriggerWispSessions can independently ask "would this ref have
// resolved at all" for its skip-observability log, without a second Get
// call or a second copy of this switch.
func resolveTriggerBeadStore(storeRef string, primary beads.Store, rigStores map[string]beads.Store) (beads.Store, bool) {
	storeRef = strings.TrimSpace(storeRef)
	if storeRef == "" {
		return primary, true
	}
	switch normalized := normalizeIdleClaimStoreRef(storeRef); {
	case normalized == "city":
		return primary, true
	case strings.HasPrefix(normalized, "rig:"):
		rs, ok := rigStores[strings.TrimPrefix(normalized, "rig:")]
		if !ok || rs == nil {
			return nil, false
		}
		return rs, true
	default:
		return nil, false
	}
}

// closeSessionBeadIfReachableStoreUnassigned closes a session bead only when
// the live store scope its configured agent can query has no open or
// in-progress work assigned to the session. It returns whether the close
// succeeded, matching closeSessionBeadIfUnassigned's contract.
// The session parameter is a session.Info: the reachable-store gate reads the
// session through the typed front door, while the close routes through closeBead
// (which already funnels its writes through sessionFrontDoor AND runs the
// extmsg/orphaned-work release cascade Store.Close does not — so the close stays
// on closeBead, not Store.Close, to preserve that behavior).
//
// excludeOwnDrainStep selects the drain-ack close-gate form of the
// assigned-work probe (sessionHasOpenAssignedWorkForReachableStoreForCloseGate),
// which excludes the session's own mol-do-work "drain" step so a session that
// has already signaled completion is not judged to still have work.
// Pass true ONLY from the drain-ack finalize path; every
// other caller (failed-create close, generic idle/config-drift close) passes
// false to keep its existing behavior unchanged.
func closeSessionBeadIfReachableStoreUnassigned(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	info sessionpkg.Info,
	reason string,
	now time.Time,
	stderr io.Writer,
	excludeOwnDrainStep bool,
) bool {
	if stderr == nil {
		stderr = io.Discard
	}
	assignedWorkProbe := sessionHasOpenAssignedWorkForReachableStore
	if excludeOwnDrainStep {
		assignedWorkProbe = sessionHasOpenAssignedWorkForReachableStoreForCloseGate
	}
	hasAssignedWork, err := assignedWorkProbe(cityPath, cfg, store, rigStores, info)
	if err != nil {
		fmt.Fprintf(stderr, "session work guard: checking reachable assigned work for %s: %v\n", info.ID, err) //nolint:errcheck
		return false
	}
	if hasAssignedWork {
		return false
	}
	if isFailedCreateSessionInfo(info) {
		return closeFailedCreateBead(sessionFrontDoor(store), info.ID, now, stderr)
	}
	return closeBead(store, info.ID, reason, now, stderr)
}
