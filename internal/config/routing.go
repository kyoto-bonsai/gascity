package config

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
}

// Configured reports whether the operator has authored a [routing] table at
// all. The officer-of-record gate no-ops when this is false, so upgrading
// the gc binary alone never silently changes sling behavior for a city (or a
// test fixture) that has not opted in via city.toml — this governs WHETHER
// the policy applies at all; Exempt governs WHO it applies to once it does.
func (r RoutingPolicyConfig) Configured() bool {
	return len(r.RoutingExempt) > 0 || len(r.OfficerOfRecordValueDomain) > 0
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
