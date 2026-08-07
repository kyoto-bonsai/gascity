package config

import "strings"

// RoutingExemptGroup names one category of routing-exempt personas. Name is
// provenance only; runtime behavior flattens Personas across all groups.
type RoutingExemptGroup struct {
	Name     string   `toml:"name"`
	Personas []string `toml:"personas,omitempty"`
}

// RoutingPolicyConfig holds city-level staff-routing governance authored in
// root city.toml under [routing].
type RoutingPolicyConfig struct {
	RoutingExempt              []RoutingExemptGroup `toml:"routing_exempt,omitempty"`
	OfficerOfRecordValueDomain []string             `toml:"officer_of_record_value_domain,omitempty"`
	ReportsTo                  map[string]string    `toml:"reports_to,omitempty"`
}

// Configured reports whether the city opted in to routing governance.
func (r RoutingPolicyConfig) Configured() bool {
	return len(r.RoutingExempt) > 0 || len(r.OfficerOfRecordValueDomain) > 0 || len(r.ReportsTo) > 0
}

// Exempt reports whether target is in any exempt group.
func (r RoutingPolicyConfig) Exempt(target string) bool {
	target = strings.TrimSpace(target)
	for _, group := range r.RoutingExempt {
		for _, persona := range group.Personas {
			if strings.TrimSpace(persona) == target {
				return true
			}
		}
	}
	return false
}

// DeriveOfficerOfRecord returns the reports_to value for target.
func (r RoutingPolicyConfig) DeriveOfficerOfRecord(target string) (string, bool) {
	value, ok := r.ReportsTo[strings.TrimSpace(target)]
	if !ok || strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, true
}
