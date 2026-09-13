package closeattr

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
)

func TestPatch(t *testing.T) {
	staff := config.CloseAttribution{RoutedTo: "persona-nils", OfficerOfRecord: "persona-marcus"}
	unknown := config.CloseAttribution{RoutedTo: "persona-stranger"} // resolved identity, absent from every routing table

	tests := []struct {
		name     string
		existing map[string]string
		attr     config.CloseAttribution
		want     map[string]string
	}{
		{
			"blank bead gets both stamps",
			map[string]string{},
			staff,
			map[string]string{
				beadmeta.RoutedToMetadataKey:        "persona-nils",
				beadmeta.OfficerOfRecordMetadataKey: "persona-marcus",
			},
		},
		{
			"nil metadata is treated as blank",
			nil,
			staff,
			map[string]string{
				beadmeta.RoutedToMetadataKey:        "persona-nils",
				beadmeta.OfficerOfRecordMetadataKey: "persona-marcus",
			},
		},
		{
			"existing routed_to is never overwritten (earlier router's decision wins), officer still derived",
			map[string]string{beadmeta.RoutedToMetadataKey: "persona-kieran"},
			staff,
			map[string]string{beadmeta.OfficerOfRecordMetadataKey: "persona-marcus"},
		},
		{
			"existing officer_of_record=operator is never overwritten (ga-kcfrcn shape)",
			map[string]string{
				beadmeta.RoutedToMetadataKey:        "persona-nils",
				beadmeta.OfficerOfRecordMetadataKey: "operator",
			},
			staff,
			map[string]string{},
		},
		{
			"fully stamped bead yields an empty patch (idempotent second writer)",
			map[string]string{
				beadmeta.RoutedToMetadataKey:        "persona-nils",
				beadmeta.OfficerOfRecordMetadataKey: "persona-marcus",
			},
			staff,
			map[string]string{},
		},
		{
			"declared exemption suppresses officer derivation, routed_to still attributed",
			map[string]string{beadmeta.OfficerExemptReasonMetadataKey: "Tier-1 single-seat self-close"},
			staff,
			map[string]string{beadmeta.RoutedToMetadataKey: "persona-nils"},
		},
		{
			"unknown persona: routed_to stamped, blank officer never written",
			map[string]string{},
			unknown,
			map[string]string{beadmeta.RoutedToMetadataKey: "persona-stranger"},
		},
		{
			"whitespace-only existing values count as absent",
			map[string]string{
				beadmeta.RoutedToMetadataKey:        "  ",
				beadmeta.OfficerOfRecordMetadataKey: "",
			},
			staff,
			map[string]string{
				beadmeta.RoutedToMetadataKey:        "persona-nils",
				beadmeta.OfficerOfRecordMetadataKey: "persona-marcus",
			},
		},
		{
			"blank attribution writes nothing",
			map[string]string{},
			config.CloseAttribution{},
			map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Patch(tt.existing, tt.attr)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Patch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPatchDoesNotMutateInput(t *testing.T) {
	existing := map[string]string{"other": "value"}
	_ = Patch(existing, config.CloseAttribution{RoutedTo: "persona-nils", OfficerOfRecord: "persona-marcus"})
	if len(existing) != 1 || existing["other"] != "value" {
		t.Fatalf("Patch mutated its input: %v", existing)
	}
}

func TestExemptionPatch(t *testing.T) {
	tests := []struct {
		name     string
		existing map[string]string
		closer   string
		reason   string
		want     map[string]string
		wantErr  bool
	}{
		{
			"blank bead: reason + routed_to, no officer",
			map[string]string{},
			"persona-marcus",
			"Tier-1 single-seat self-close, no validation loop",
			map[string]string{
				beadmeta.OfficerExemptReasonMetadataKey: "Tier-1 single-seat self-close, no validation loop",
				beadmeta.RoutedToMetadataKey:            "persona-marcus",
			},
			false,
		},
		{
			"blank reason is refused (an exemption must be declared, not blank)",
			map[string]string{},
			"persona-marcus",
			"   ",
			nil,
			true,
		},
		{
			"existing reason is never overwritten",
			map[string]string{beadmeta.OfficerExemptReasonMetadataKey: "original"},
			"persona-marcus",
			"replacement",
			map[string]string{beadmeta.RoutedToMetadataKey: "persona-marcus"},
			false,
		},
		{
			"existing routed_to is never overwritten",
			map[string]string{beadmeta.RoutedToMetadataKey: "persona-kieran"},
			"persona-marcus",
			"why",
			map[string]string{beadmeta.OfficerExemptReasonMetadataKey: "why"},
			false,
		},
		{
			"existing officer_of_record is left untouched (exemption never clears a stamped officer)",
			map[string]string{beadmeta.OfficerOfRecordMetadataKey: "operator"},
			"persona-marcus",
			"why",
			map[string]string{
				beadmeta.OfficerExemptReasonMetadataKey: "why",
				beadmeta.RoutedToMetadataKey:            "persona-marcus",
			},
			false,
		},
		{
			"unresolved closer still records the declared reason",
			map[string]string{},
			"",
			"why",
			map[string]string{beadmeta.OfficerExemptReasonMetadataKey: "why"},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExemptionPatch(tt.existing, tt.closer, tt.reason)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ExemptionPatch() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ExemptionPatch() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ExemptionPatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNeedsStamp(t *testing.T) {
	tests := []struct {
		name     string
		existing map[string]string
		want     bool
	}{
		{"blank", map[string]string{}, true},
		{"nil", nil, true},
		{"routed only", map[string]string{beadmeta.RoutedToMetadataKey: "persona-nils"}, true},
		{"officer only", map[string]string{beadmeta.OfficerOfRecordMetadataKey: "persona-marcus"}, true},
		{
			"routed + officer",
			map[string]string{beadmeta.RoutedToMetadataKey: "persona-nils", beadmeta.OfficerOfRecordMetadataKey: "persona-marcus"},
			false,
		},
		{
			"routed + declared exemption",
			map[string]string{beadmeta.RoutedToMetadataKey: "persona-marcus", beadmeta.OfficerExemptReasonMetadataKey: "why"},
			false,
		},
		{"exemption only (closer still unattributed)", map[string]string{beadmeta.OfficerExemptReasonMetadataKey: "why"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NeedsStamp(tt.existing); got != tt.want {
				t.Fatalf("NeedsStamp(%v) = %v, want %v", tt.existing, got, tt.want)
			}
		})
	}
}

func TestCloserFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			"pool seat: GC_TEMPLATE is the persona, alias/session/actor are ignored",
			map[string]string{
				"GC_TEMPLATE":     "persona-marcus",
				"GC_ALIAS":        "persona-marcus-3",
				"GC_AGENT":        "persona-marcus-3",
				"GC_SESSION_NAME": "persona-marcus-ga-ynyptp",
				"BEADS_ACTOR":     "persona-marcus-ga-ynyptp",
			},
			"persona-marcus",
		},
		{
			"named session: GC_TEMPLATE carries the agent's qualified name",
			map[string]string{"GC_TEMPLATE": "persona-tomoko", "GC_AGENT": "persona-tomoko"},
			"persona-tomoko",
		},
		{
			"operator terminal (no gc session): nothing resolves — never the actor or a shell user",
			map[string]string{"BEADS_ACTOR": "Andrew Pierce", "USER": "drew", "GC_AGENT": "persona-marcus-3"},
			"",
		},
		{"whitespace template is unresolved", map[string]string{"GC_TEMPLATE": "  "}, ""},
		{"template is trimmed", map[string]string{"GC_TEMPLATE": " persona-nils \n"}, "persona-nils"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CloserFromEnv(env(tt.env)); got != tt.want {
				t.Fatalf("CloserFromEnv() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCloserFromEnvNilLookup(t *testing.T) {
	if got := CloserFromEnv(nil); got != "" {
		t.Fatalf("CloserFromEnv(nil) = %q, want empty", got)
	}
}
