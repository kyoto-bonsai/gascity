package config

import "strings"

// RoutingExemptGroup names one category of routing-exempt personas, purely
// for provenance/readability in city.toml (e.g. "officers", "cos_office",
// "independent_audit", "meta"). The runtime check flattens every group's
// Personas into one flat exempt set — Name carries no behavior.
type RoutingExemptGroup struct {
	// Name labels the group for humans reading city.toml. Not read at runtime.
	Name string `toml:"name"`
	// Personas lists the persona identifiers exempt from the officer-of-record
	// gate by virtue of membership in this group.
	Personas []string `toml:"personas,omitempty"`
}

// RoutingPolicyConfig holds city-level staff-routing governance authored in
// the root city.toml under [routing]. Like WebhookPolicyConfig, it is
// intentionally never merged from packs or fragments — a pack must not be
// able to self-grant a routing exemption or expand the valid
// officer-of-record value domain. This is the trust boundary: the operator,
// via the root city.toml alone, decides who is exempt from the
// officer-of-record gate and what values are legal for it.
//
// rigs/personas/ariadne-plan-persona-standards-2026-07-25.md phase_2_slinggate
// ruling (a): gc sling refuses staff-persona dispatch without
// gc.officer_of_record metadata, with no --force override. See
// internal/sling checkOfficerOfRecord for the enforcement side.
type RoutingPolicyConfig struct {
	// RoutingExempt lists personas that are legal gc sling targets without
	// gc.officer_of_record metadata on the target bead (officers, CoS office,
	// independent audit, meta seats — accountable-by-construction actors,
	// not staff specialists).
	RoutingExempt []RoutingExemptGroup `toml:"routing_exempt,omitempty"`
	// OfficerOfRecordValueDomain lists the legal values for a stamped
	// gc.officer_of_record (the department officers plus "operator"). Not
	// enforced by the sling gate itself (which only checks presence) — used
	// by doctrine/lint-layer tooling that additionally validates the stamped
	// value, kept here so both layers can reference one declared domain.
	OfficerOfRecordValueDomain []string `toml:"officer_of_record_value_domain,omitempty"`
	// ReportsTo maps a staff persona to the officer_of_record value gc sling
	// should auto-stamp when routing to that persona and the target bead does
	// not yet carry gc.officer_of_record. Source of truth for the mapping is
	// rigs/personas/doctrine/agent-org-departments-v2-2026-07-12.md (the
	// Reports-to column) — kept in sync by hand, same discipline as
	// RoutingExempt/OfficerOfRecordValueDomain above. Deliberately omits
	// personas already covered by RoutingExempt (officers, CoS office,
	// independent audit, meta): Exempt() short-circuits before this map is
	// ever consulted for them, so an entry there would be dead data. This is
	// the "fix at the source" ga-owbb42 asks for: stamping here, before
	// checkOfficerOfRecord evaluates, means a mapped persona never trips the
	// gate in the first place instead of being caught by fleet-lint V8 24h
	// later or requiring a human hand-stamp mid-dispatch.
	ReportsTo map[string]string `toml:"reports_to,omitempty"`
}

// Configured reports whether the operator has authored a [routing] table at
// all. The officer-of-record gate no-ops when this is false, so upgrading
// the gc binary alone never silently changes sling behavior for a city (or a
// test fixture) that has not opted in via city.toml — this governs WHETHER
// the policy applies at all; Exempt governs WHO it applies to once it does.
func (r RoutingPolicyConfig) Configured() bool {
	return len(r.RoutingExempt) > 0 || len(r.OfficerOfRecordValueDomain) > 0 || len(r.ReportsTo) > 0
}

// Exempt reports whether persona is a member of any RoutingExempt group.
func (r RoutingPolicyConfig) Exempt(persona string) bool {
	for _, group := range r.RoutingExempt {
		for _, p := range group.Personas {
			if p == persona {
				return true
			}
		}
	}
	return false
}

// DeriveOfficerOfRecord looks up the officer_of_record value gc sling should
// auto-stamp for persona, per ReportsTo. Returns ("", false) when persona has
// no entry (e.g. a brand-new persona not yet added to city.toml) rather than
// guessing — an unmapped persona still hits checkOfficerOfRecord's
// fail-closed refusal, exactly as before this field existed.
func (r RoutingPolicyConfig) DeriveOfficerOfRecord(persona string) (string, bool) {
	v, ok := r.ReportsTo[persona]
	if !ok || strings.TrimSpace(v) == "" {
		return "", false
	}
	return v, true
}

// officerOfRecordOperator is the officer_of_record value for closers who are
// accountable directly to the human operator rather than up a department chain:
// the CoS office, independent audit, and meta seats. These are routing-exempt
// (Exempt() is true) but are not themselves department officers (not present in
// OfficerOfRecordValueDomain except as this sentinel), so ReportsTo has no entry
// for them by construction. Declared as a constant, not a hardcoded role name:
// it is a structural sentinel already part of OfficerOfRecordValueDomain's
// semantics ("the department officers plus operator"), not a user-configurable
// role whose behavior lives in Go.
const officerOfRecordOperator = "operator"

// IsOfficerOfRecordValue reports whether persona is itself a legal
// officer_of_record value (a department officer, per
// OfficerOfRecordValueDomain). An officer is their own officer of record, so a
// close performed by one derives to self rather than up a chain.
func (r RoutingPolicyConfig) IsOfficerOfRecordValue(persona string) bool {
	for _, v := range r.OfficerOfRecordValueDomain {
		if v == persona {
			return true
		}
	}
	return false
}

// CloseAttribution is the routing identity a close-time auto-stamp should apply
// to a bead that does not already carry it. RoutedTo is the closing persona;
// OfficerOfRecord is the derived accountable officer, or "" when it cannot be
// derived (an unknown persona) — in which case the caller must stamp RoutedTo
// but MUST NOT stamp a blank officer.
type CloseAttribution struct {
	RoutedTo        string
	OfficerOfRecord string
}

// DeriveCloseAttribution computes the gc.routed_to / gc.officer_of_record a
// close performed by `closer` should carry. It mirrors the sling-time
// derivation (DeriveOfficerOfRecord) but is keyed on the CLOSING persona and
// additionally resolves the exempt-closer case the sling path never sees: sling
// only ever targets staff specialists, but a close can be performed by an
// exempt actor (an officer, the CoS office, independent audit, or a meta seat)
// closing ad-hoc or operator-authored work. Those actors have no ReportsTo
// entry, so a close-time stamp that only consulted ReportsTo would leave them
// blank — the very defect (indistinguishable from an oversight) this closes.
//
// It is pure and config-driven (ZFC): every decision is a lookup against the
// operator-authored [routing] policy, never a hardcoded persona list. The
// caller applies don't-overwrite (an existing gc.officer_of_record — e.g. an
// operator-authorization override — is never clobbered) and honors any
// pre-existing exemption marker.
//
// Returns ok=false (the caller stamps nothing at all) when the [routing] policy
// is not Configured() — so upgrading the gc binary alone never changes close
// behavior for a city that has not opted in — or when `closer` does not resolve
// to a persona identity, so a shell username is never written into a
// persona-typed field.
func (r RoutingPolicyConfig) DeriveCloseAttribution(closer string) (CloseAttribution, bool) {
	if !r.Configured() {
		return CloseAttribution{}, false
	}
	closer = strings.TrimSpace(closer)
	if closer == "" {
		return CloseAttribution{}, false
	}

	attr := CloseAttribution{RoutedTo: closer}
	switch {
	case r.IsOfficerOfRecordValue(closer):
		// An officer (or the operator) is their own officer of record.
		attr.OfficerOfRecord = closer
	case r.hasReportsTo(closer):
		// A staff specialist reports up to their department officer.
		attr.OfficerOfRecord, _ = r.DeriveOfficerOfRecord(closer)
	case r.Exempt(closer) && r.IsOfficerOfRecordValue(officerOfRecordOperator):
		// CoS office / independent audit / meta: routing-exempt but not a
		// department officer — accountable directly to the operator. Guarded on
		// the operator sentinel being a declared legal value so a misconfigured
		// city yields a blank (caught by lint) rather than an invented officer.
		attr.OfficerOfRecord = officerOfRecordOperator
	}
	// Unknown persona (resolved identity, but absent from every routing table):
	// OfficerOfRecord stays "". The caller still stamps RoutedTo, so the close is
	// attributed and lint can prompt adding the persona to city.toml, rather than
	// leaving the fully-blank "unexamined" state.
	return attr, true
}

// hasReportsTo reports whether persona has a non-empty ReportsTo entry.
func (r RoutingPolicyConfig) hasReportsTo(persona string) bool {
	_, ok := r.DeriveOfficerOfRecord(persona)
	return ok
}
