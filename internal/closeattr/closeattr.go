// Package closeattr is the close-time routing-attribution rule: when a bead
// closes, the closing persona is recorded as gc.routed_to and their accountable
// officer as gc.officer_of_record, so a bead created outside gc sling (an
// agent's ad-hoc `gc bd create`, an operator-authored bead) does not end its
// life with both fields blank — the identity-blank-on-close defect class
// ga-d882cp / ga-45ltic / ga-15x4xy recurred on.
//
// The WHO is derived by config.RoutingPolicyConfig.DeriveCloseAttribution (pure,
// config-driven, ZFC: no persona name appears in Go). This package owns the
// WHETHER/WHAT-TO-WRITE half that the domain function's contract leaves to its
// caller, so that both projections — the `gc bd close` passthrough (client side,
// identity from the session env) and the controller's in-process bead.closed
// handler (identity from the durable gc.session_id stamp) — apply one identical
// rule and are idempotent with each other:
//
//   - an existing gc.routed_to is never overwritten (an earlier router's
//     decision is not clobbered by whoever happens to close the bead);
//   - an existing gc.officer_of_record is never overwritten (an operator-
//     authorization override — ga-kcfrcn's shape — wins over derivation);
//   - a declared gc.officer_exempt_reason suppresses officer derivation
//     entirely: the exemption is the positive marker, not an absent field;
//   - a blank derived officer (an identity absent from every routing table) is
//     never written — gc.routed_to alone is stamped so the close is attributed
//     and lint can prompt the operator to add the persona to city.toml.
//
// Every function here is pure: callers perform the store write.
package closeattr

import (
	"errors"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
)

// ErrBlankExemptReason is returned by ExemptionPatch when the declared reason
// is empty or whitespace. An exemption that says nothing is indistinguishable
// from the oversight it exists to rule out, so it is refused rather than
// recorded.
var ErrBlankExemptReason = errors.New("close attribution: exempt reason must not be blank")

// closerEnvKey is the one session-environment variable that carries the
// routing persona identity. gc sets it on every session it spawns: a pool
// seat's template (e.g. "persona-marcus", while GC_ALIAS is the seat
// "persona-marcus-3" and GC_SESSION_NAME the session "persona-marcus-ga-xxxx")
// and, for a named session, the agent's qualified name — in both cases exactly
// the string internal/sling keys the [routing] policy on. It is deliberately
// the ONLY source consulted: GC_ALIAS and GC_SESSION_NAME are seat/session
// identities, and BEADS_ACTOR / the git or shell user are never persona
// identities, so a close from an operator terminal correctly resolves to
// nothing rather than to a username in a persona-typed field.
const closerEnvKey = "GC_TEMPLATE"

// Patch returns the metadata writes a close attributed to attr should apply to
// a bead currently carrying existing. An empty result means nothing to write.
// existing is not mutated; a nil map is treated as blank.
func Patch(existing map[string]string, attr config.CloseAttribution) map[string]string {
	patch := map[string]string{}
	routedTo := strings.TrimSpace(attr.RoutedTo)
	if routedTo != "" && !has(existing, beadmeta.RoutedToMetadataKey) {
		patch[beadmeta.RoutedToMetadataKey] = routedTo
	}
	if has(existing, beadmeta.OfficerExemptReasonMetadataKey) {
		return patch
	}
	officer := strings.TrimSpace(attr.OfficerOfRecord)
	if officer != "" && !has(existing, beadmeta.OfficerOfRecordMetadataKey) {
		patch[beadmeta.OfficerOfRecordMetadataKey] = officer
	}
	return patch
}

// ExemptionPatch returns the writes for a close the closer declares exempt from
// the officer-of-record stamp: gc.officer_exempt_reason (iff absent) and
// gc.routed_to (iff absent and closer resolved). It never touches an existing
// gc.officer_of_record — declaring an exemption does not retract a stamped
// officer. A blank reason is refused with ErrBlankExemptReason.
func ExemptionPatch(existing map[string]string, closer, reason string) (map[string]string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, ErrBlankExemptReason
	}
	patch := map[string]string{}
	if !has(existing, beadmeta.OfficerExemptReasonMetadataKey) {
		patch[beadmeta.OfficerExemptReasonMetadataKey] = reason
	}
	if closer = strings.TrimSpace(closer); closer != "" && !has(existing, beadmeta.RoutedToMetadataKey) {
		patch[beadmeta.RoutedToMetadataKey] = closer
	}
	return patch, nil
}

// NeedsStamp reports whether a close-time stamp could still change existing:
// false once the bead carries gc.routed_to AND either gc.officer_of_record or a
// declared gc.officer_exempt_reason. It is the cheap pre-check a caller runs
// before resolving the closer identity, so fully-attributed closes cost no
// session lookup and no write.
func NeedsStamp(existing map[string]string) bool {
	if !has(existing, beadmeta.RoutedToMetadataKey) {
		return true
	}
	return !has(existing, beadmeta.OfficerOfRecordMetadataKey) &&
		!has(existing, beadmeta.OfficerExemptReasonMetadataKey)
}

// CloserFromEnv resolves the closing persona identity from a session
// environment, via lookup (typically os.Getenv or a KEY=VALUE scan). It returns
// "" — stamp nothing — when the environment is not a gc session's. See
// closerEnvKey for why only one variable is consulted.
func CloserFromEnv(lookup func(string) string) string {
	if lookup == nil {
		return ""
	}
	return strings.TrimSpace(lookup(closerEnvKey))
}

func has(m map[string]string, key string) bool {
	return strings.TrimSpace(m[key]) != ""
}
