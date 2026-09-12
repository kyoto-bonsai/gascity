package config

import "testing"

func TestRoutingPolicyConfigConfigured(t *testing.T) {
	tests := []struct {
		name   string
		policy RoutingPolicyConfig
		want   bool
	}{
		{"zero value", RoutingPolicyConfig{}, false},
		{
			"exempt group only",
			RoutingPolicyConfig{RoutingExempt: []RoutingExemptGroup{{Name: "officers", Personas: []string{"persona-marcus"}}}},
			true,
		},
		{
			"value domain only",
			RoutingPolicyConfig{OfficerOfRecordValueDomain: []string{"operator"}},
			true,
		},
		{
			"empty exempt group slice (declared but no groups)",
			RoutingPolicyConfig{RoutingExempt: []RoutingExemptGroup{}},
			false,
		},
		{
			"reports-to only",
			RoutingPolicyConfig{ReportsTo: map[string]string{"persona-kieran": "persona-marcus"}},
			true,
		},
		{
			"empty reports-to map (declared but no entries)",
			RoutingPolicyConfig{ReportsTo: map[string]string{}},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.policy.Configured(); got != tt.want {
				t.Errorf("Configured() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRoutingPolicyConfigExempt(t *testing.T) {
	policy := RoutingPolicyConfig{
		RoutingExempt: []RoutingExemptGroup{
			{Name: "officers", Personas: []string{"persona-marcus", "persona-cmo"}},
			{Name: "meta", Personas: []string{"persona-architect"}},
		},
	}

	tests := []struct {
		persona string
		want    bool
	}{
		{"persona-marcus", true},
		{"persona-cmo", true},
		{"persona-architect", true},
		{"persona-nils", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.persona, func(t *testing.T) {
			if got := policy.Exempt(tt.persona); got != tt.want {
				t.Errorf("Exempt(%q) = %v, want %v", tt.persona, got, tt.want)
			}
		})
	}
}

func TestRoutingPolicyConfigExemptOnZeroValue(t *testing.T) {
	var policy RoutingPolicyConfig
	if policy.Exempt("persona-marcus") {
		t.Error("zero-value RoutingPolicyConfig should exempt nobody")
	}
}

func TestRoutingPolicyConfigDeriveOfficerOfRecord(t *testing.T) {
	policy := RoutingPolicyConfig{
		ReportsTo: map[string]string{
			"persona-kieran": "persona-marcus",
			"persona-cass":   "persona-dan",
			"blank-entry":    "",
		},
	}

	tests := []struct {
		persona   string
		wantValue string
		wantOK    bool
	}{
		{"persona-kieran", "persona-marcus", true},
		{"persona-cass", "persona-dan", true},
		{"blank-entry", "", false},
		{"persona-unmapped", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.persona, func(t *testing.T) {
			got, ok := policy.DeriveOfficerOfRecord(tt.persona)
			if got != tt.wantValue || ok != tt.wantOK {
				t.Errorf("DeriveOfficerOfRecord(%q) = (%q, %v), want (%q, %v)",
					tt.persona, got, ok, tt.wantValue, tt.wantOK)
			}
		})
	}
}

func TestRoutingPolicyConfigDeriveOfficerOfRecordOnZeroValue(t *testing.T) {
	var policy RoutingPolicyConfig
	if v, ok := policy.DeriveOfficerOfRecord("persona-kieran"); ok || v != "" {
		t.Errorf("zero-value RoutingPolicyConfig should derive nothing, got (%q, %v)", v, ok)
	}
}

func TestRoutingPolicyConfigIsOfficerOfRecordValue(t *testing.T) {
	policy := RoutingPolicyConfig{
		OfficerOfRecordValueDomain: []string{"persona-marcus", "persona-dan", "operator"},
	}
	tests := []struct {
		persona string
		want    bool
	}{
		{"persona-marcus", true},
		{"persona-dan", true},
		{"operator", true},
		{"persona-kieran", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.persona, func(t *testing.T) {
			if got := policy.IsOfficerOfRecordValue(tt.persona); got != tt.want {
				t.Errorf("IsOfficerOfRecordValue(%q) = %v, want %v", tt.persona, got, tt.want)
			}
		})
	}
}

// fullRoutingPolicy mirrors the real city.toml [routing] shape: officers and
// CoS/audit/meta are routing-exempt; officers (plus operator) are the legal
// officer_of_record values; staff specialists carry ReportsTo entries.
func fullRoutingPolicy() RoutingPolicyConfig {
	return RoutingPolicyConfig{
		RoutingExempt: []RoutingExemptGroup{
			{Name: "officers", Personas: []string{"persona-marcus", "persona-dan"}},
			{Name: "cos_office", Personas: []string{"persona-ariadne", "persona-archivist"}},
			{Name: "independent_audit", Personas: []string{"persona-daedalus"}},
			{Name: "meta", Personas: []string{"persona-architect"}},
		},
		OfficerOfRecordValueDomain: []string{"persona-marcus", "persona-dan", "operator"},
		ReportsTo: map[string]string{
			"persona-kieran": "persona-marcus",
			"persona-cass":   "persona-dan",
		},
	}
}

func TestRoutingPolicyConfigDeriveCloseAttribution(t *testing.T) {
	policy := fullRoutingPolicy()

	tests := []struct {
		name        string
		closer      string
		wantRouted  string
		wantOfficer string
		wantOK      bool
	}{
		// Staff specialist → reports up to their department officer (the sling reuse).
		{"staff reports up", "persona-kieran", "persona-kieran", "persona-marcus", true},
		{"staff reports up (other dept)", "persona-cass", "persona-cass", "persona-dan", true},
		// Officer closer → their own officer of record (self), not up a chain.
		{"officer is self", "persona-marcus", "persona-marcus", "persona-marcus", true},
		// CoS / audit / meta → accountable to the operator (the exempt-closer amendment).
		{"cos office to operator", "persona-ariadne", "persona-ariadne", "operator", true},
		{"independent audit to operator", "persona-daedalus", "persona-daedalus", "operator", true},
		{"meta to operator", "persona-architect", "persona-architect", "operator", true},
		// Unknown persona → routed_to stamped, officer left blank (don't guess), still ok.
		{"unknown persona routed but no officer", "persona-newbie", "persona-newbie", "", true},
		// Never stamp a shell username / blank closer.
		{"empty closer stamps nothing", "", "", "", false},
		{"whitespace closer stamps nothing", "   ", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := policy.DeriveCloseAttribution(tt.closer)
			if ok != tt.wantOK {
				t.Fatalf("DeriveCloseAttribution(%q) ok = %v, want %v", tt.closer, ok, tt.wantOK)
			}
			if got.RoutedTo != tt.wantRouted || got.OfficerOfRecord != tt.wantOfficer {
				t.Errorf("DeriveCloseAttribution(%q) = {RoutedTo:%q, OfficerOfRecord:%q}, want {RoutedTo:%q, OfficerOfRecord:%q}",
					tt.closer, got.RoutedTo, got.OfficerOfRecord, tt.wantRouted, tt.wantOfficer)
			}
		})
	}
}

func TestRoutingPolicyConfigDeriveCloseAttributionNotConfigured(t *testing.T) {
	var policy RoutingPolicyConfig
	if attr, ok := policy.DeriveCloseAttribution("persona-marcus"); ok {
		t.Errorf("unconfigured policy should stamp nothing, got ok=true, attr=%+v", attr)
	}
}

// TestRoutingPolicyConfigDeriveCloseAttributionExemptWithoutOperatorDomain guards
// the misconfiguration case: an exempt closer whose city omits "operator" from
// the value domain must yield a blank officer (caught by lint), never an invented
// one.
func TestRoutingPolicyConfigDeriveCloseAttributionExemptWithoutOperatorDomain(t *testing.T) {
	policy := RoutingPolicyConfig{
		RoutingExempt:              []RoutingExemptGroup{{Name: "cos_office", Personas: []string{"persona-ariadne"}}},
		OfficerOfRecordValueDomain: []string{"persona-marcus"}, // no "operator"
	}
	got, ok := policy.DeriveCloseAttribution("persona-ariadne")
	if !ok {
		t.Fatal("configured policy should be ok=true")
	}
	if got.RoutedTo != "persona-ariadne" || got.OfficerOfRecord != "" {
		t.Errorf("exempt closer without operator in domain = %+v, want {RoutedTo:persona-ariadne, OfficerOfRecord:}", got)
	}
}
