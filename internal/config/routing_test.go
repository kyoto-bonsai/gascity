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
