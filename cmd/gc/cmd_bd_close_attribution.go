package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/closeattr"
	"github.com/gastownhall/gascity/internal/config"
)

// bdExemptReasonFlag is the one flag gc bd adds to `bd close` that bd itself
// does not know: the closer's declaration that this close legitimately carries
// no gc.officer_of_record (a Tier-1 single-seat self-close, an operator's own
// direct close). It is stripped before the arguments reach bd — and before
// bdMutationWriteIDs, which is fail-closed on unknown separate-value flags —
// and applied as closeattr.ExemptionPatch once bd reports the close succeeded.
// Kept at the gc layer on purpose: bd is a generic tracker with no notion of
// routing policy, so teaching `bd close` a gc.* key would be the same layering
// violation the stamp itself avoids (ga-15x4xy).
const bdExemptReasonFlag = "--exempt-reason"

// bdCloseAttributionTargets returns the bead ids a `gc bd` invocation closes,
// for the close-time attribution stamp (ga-15x4xy): every `close <ids>` —
// with or without a reason, forced or not — and `update <ids> --status closed`
// (also -s closed, --status=closed). Nothing to stamp when the argument list is
// ambiguous to bdMutationWriteIDs (an unknown flag may have swallowed an id),
// names no id (bd's last-touched fallback), or does not close.
func bdCloseAttributionTargets(args []string) (ids []string, ok bool) {
	if len(args) == 0 {
		return nil, false
	}
	switch args[0] {
	case "close":
	case "update":
		if !bdUpdateSetsStatusClosed(args) {
			return nil, false
		}
	default:
		return nil, false
	}
	writeIDs, writeOK, ambiguous := bdMutationWriteIDs(args)
	if !writeOK || ambiguous || len(writeIDs) == 0 {
		return nil, false
	}
	return writeIDs, true
}

// bdUpdateSetsStatusClosed reports whether a `bd update` argument list moves
// the bead to closed via --status/-s, in either the separate-value or the
// --status=closed form. It does not validate the rest of the arguments; the
// caller's bdMutationWriteIDs pass does that.
func bdUpdateSetsStatusClosed(args []string) bool {
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		switch {
		case arg == "--status" || arg == "-s":
			return i+1 < len(args) && strings.EqualFold(strings.TrimSpace(args[i+1]), "closed")
		case strings.HasPrefix(arg, "--status="):
			return strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(arg, "--status=")), "closed")
		case strings.HasPrefix(arg, "-s="):
			return strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(arg, "-s=")), "closed")
		}
	}
	return false
}

// extractBdExemptReason removes gc's --exempt-reason flag (separate-value or
// --exempt-reason=value form) from a `gc bd` argument list and returns the
// remaining bd arguments plus the declared reason. Tokens after "--" are bd
// positionals and are never interpreted. The flag is refused (an error, exit 1
// before anything is written) when it appears on any subcommand but close,
// appears more than once, has no value, or has a blank value — an exemption
// that says nothing is the oversight it exists to rule out, so the blank case
// surfaces closeattr.ErrBlankExemptReason rather than being dropped.
func extractBdExemptReason(args []string) (rest []string, reason string, err error) {
	rest = make([]string, 0, len(args))
	seen := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		var value string
		switch {
		case arg == bdExemptReasonFlag:
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("%s requires a value", bdExemptReasonFlag)
			}
			i++
			value = args[i]
		case strings.HasPrefix(arg, bdExemptReasonFlag+"="):
			value = strings.TrimPrefix(arg, bdExemptReasonFlag+"=")
		default:
			rest = append(rest, arg)
			continue
		}
		if seen {
			return nil, "", fmt.Errorf("%s given more than once", bdExemptReasonFlag)
		}
		seen = true
		if len(args) == 0 || args[0] != "close" {
			return nil, "", fmt.Errorf("%s is only meaningful on gc bd close", bdExemptReasonFlag)
		}
		reason = strings.TrimSpace(value)
		if reason == "" {
			return nil, "", closeattr.ErrBlankExemptReason
		}
	}
	return rest, reason, nil
}

// stampBdCloseAttribution is the client-side projection of the close-time
// attribution rule (ga-15x4xy): after bd has closed ids, record the closing
// persona as gc.routed_to and its accountable officer as gc.officer_of_record
// (closeattr.Patch), or — when the closer declared exemptReason — record the
// positive exemption marker instead (closeattr.ExemptionPatch). The closer is
// resolved from the process environment (closeattr.CloserFromEnv: GC_TEMPLATE,
// the same identity the sling side keys routing on), never from --actor or the
// shell user. Because this runs on every `gc bd close`, it covers the two paths
// the bead names that no sling-time stamp ever sees: a bead an agent minted
// ad hoc with `gc bd create`, and an operator-authored bead closed by whichever
// agent picked it up — whether or not the bead was ever claimed.
//
// Best-effort by contract: the close has already happened and the exit code
// belongs to bd. Every failure here is one stderr line prefixed
// "gc bd: close-attribution:" and the next id is still processed. It no-ops
// silently when the city has not opted in ([routing] not Configured()) and
// when the store is unavailable.
func stampBdCloseAttribution(ids []string, exemptReason string, store beads.Store, cfg *config.City, env func(string) string, stderr io.Writer) {
	if store == nil || cfg == nil || !cfg.RoutingPolicy.Configured() {
		return
	}
	closer := closeattr.CloserFromEnv(env)
	for _, id := range ids {
		bead, err := store.Get(id)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd: close-attribution: %s: reading bead: %v\n", id, err) //nolint:errcheck // best-effort stderr
			continue
		}
		var patch map[string]string
		if exemptReason != "" {
			patch, err = closeattr.ExemptionPatch(bead.Metadata, closer, exemptReason)
			if err != nil {
				fmt.Fprintf(stderr, "gc bd: close-attribution: %s: %v\n", id, err) //nolint:errcheck // best-effort stderr
				continue
			}
		} else {
			if !closeattr.NeedsStamp(bead.Metadata) {
				continue
			}
			if closer == "" {
				fmt.Fprintf(stderr, "gc bd: close-attribution: %s: no persona identity in this environment (GC_TEMPLATE unset — not a gc session); close left unattributed. Declare %s \"<why>\" if this is a legitimate direct close.\n", id, bdExemptReasonFlag) //nolint:errcheck // best-effort stderr
				continue
			}
			attr, ok := cfg.RoutingPolicy.DeriveCloseAttribution(closer)
			if !ok {
				continue
			}
			patch = closeattr.Patch(bead.Metadata, attr)
		}
		if len(patch) == 0 {
			continue
		}
		if err := store.SetMetadataBatch(id, patch); err != nil {
			fmt.Fprintf(stderr, "gc bd: close-attribution: %s: stamping %v: %v\n", id, patch, err) //nolint:errcheck // best-effort stderr
		}
	}
}

// errBdExemptReasonUsage wraps an extractBdExemptReason failure for doBd's
// single "gc bd: <err>" reporting line.
func errBdExemptReasonUsage(err error) error {
	if errors.Is(err, closeattr.ErrBlankExemptReason) {
		return fmt.Errorf("%s must not be blank", bdExemptReasonFlag)
	}
	return err
}
