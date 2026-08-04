package config

import (
	"reflect"
	"strings"
	"testing"
)

// mustDefaultArgs unwraps ResolveDefaultArgs for providers whose effective
// defaults are expected to resolve; it fails the test on a resolution error.
func mustDefaultArgs(t *testing.T, rp *ResolvedProvider) []string {
	t.Helper()
	args, err := rp.ResolveDefaultArgs()
	if err != nil {
		t.Fatalf("ResolveDefaultArgs() error = %v", err)
	}
	return args
}

// uniformModelOption mirrors the built-in claude model option shape: every
// emitting choice is ["--model", <id>], plus a no-args "" default entry.
func uniformModelOption() ProviderOption {
	return ProviderOption{
		Key:  "model",
		Type: "select",
		Choices: []OptionChoice{
			{Value: "", Label: "Default"},
			{Value: "fable-5", FlagArgs: []string{"--model", "claude-fable-5"}, FlagAliases: [][]string{{"-m", "claude-fable-5"}}},
			{Value: "sonnet", FlagArgs: []string{"--model", "claude-sonnet-5"}, FlagAliases: [][]string{{"-m", "claude-sonnet-5"}}},
		},
	}
}

// mixedPermissionOption mirrors the built-in permission_mode shape: mixed
// arities (a bare flag next to flag+value pairs), so no verbatim pass-through.
func mixedPermissionOption() ProviderOption {
	return ProviderOption{
		Key:  "permission_mode",
		Type: "select",
		Choices: []OptionChoice{
			{Value: "auto-edit", FlagArgs: []string{"--permission-mode", "auto-edit"}},
			{Value: "unrestricted", FlagArgs: []string{"--dangerously-skip-permissions"}},
		},
	}
}

// The ga-b0flc8 regression: an agent-level model pin the choice table does
// not know ("fable") must reach the launch argv verbatim on the managed
// (reconciler) path instead of being silently dropped.
func TestResolveDefaultArgs_UnknownModelPassesThroughVerbatim(t *testing.T) {
	rp := &ResolvedProvider{
		OptionsSchema:     []ProviderOption{uniformModelOption()},
		EffectiveDefaults: map[string]string{"model": "fable"},
	}
	args, err := rp.ResolveDefaultArgs()
	if err != nil {
		t.Fatalf("ResolveDefaultArgs() error = %v, want verbatim pass-through", err)
	}
	if want := []string{"--model", "fable"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("ResolveDefaultArgs() = %v, want %v", args, want)
	}
}

// The ga-1qirw5 class: a literal provider model id that is not a declared
// choice value must also pass through instead of breaking the spawn.
func TestResolveDefaultArgs_LiteralModelIDPassesThrough(t *testing.T) {
	rp := &ResolvedProvider{
		OptionsSchema:     []ProviderOption{uniformModelOption()},
		EffectiveDefaults: map[string]string{"model": "claude-sonnet-5"},
	}
	args, err := rp.ResolveDefaultArgs()
	if err != nil {
		t.Fatalf("ResolveDefaultArgs() error = %v, want verbatim pass-through", err)
	}
	if want := []string{"--model", "claude-sonnet-5"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("ResolveDefaultArgs() = %v, want %v", args, want)
	}
}

func TestResolveDefaultArgs_UnknownValueNonUniformShapeErrors(t *testing.T) {
	rp := &ResolvedProvider{
		OptionsSchema:     []ProviderOption{mixedPermissionOption()},
		EffectiveDefaults: map[string]string{"permission_mode": "yolo"},
	}
	args, err := rp.ResolveDefaultArgs()
	if err == nil {
		t.Fatalf("ResolveDefaultArgs() = %v, want error for non-uniform option shape", args)
	}
	if !strings.Contains(err.Error(), "permission_mode") || !strings.Contains(err.Error(), "yolo") {
		t.Fatalf("error %q should name the option key and value", err)
	}
}

func TestResolveDefaultArgs_MalformedValueErrors(t *testing.T) {
	for _, value := range []string{"fable --extra", "-fable", "fable;rm", "fa ble", "=fable"} {
		rp := &ResolvedProvider{
			OptionsSchema:     []ProviderOption{uniformModelOption()},
			EffectiveDefaults: map[string]string{"model": value},
		}
		if args, err := rp.ResolveDefaultArgs(); err == nil {
			t.Errorf("value %q: ResolveDefaultArgs() = %v, want error (not a plain token)", value, args)
		}
	}
}

func TestResolveDefaultArgs_DeclaredChoiceStillMapsThroughAlias(t *testing.T) {
	// A declared choice keeps its alias mapping: value "sonnet" emits the
	// claude-sonnet-5 id, not the raw value.
	rp := &ResolvedProvider{
		OptionsSchema:     []ProviderOption{uniformModelOption()},
		EffectiveDefaults: map[string]string{"model": "sonnet"},
	}
	if want := []string{"--model", "claude-sonnet-5"}; !reflect.DeepEqual(mustDefaultArgs(t, rp), want) {
		t.Fatalf("ResolveDefaultArgs() = %v, want %v", mustDefaultArgs(t, rp), want)
	}
}

func TestResolveOptions_ExplicitUnknownUniformValuePassesThrough(t *testing.T) {
	schema := []ProviderOption{uniformModelOption()}
	args, metadata, err := ResolveOptions(schema, map[string]string{"model": "fable"}, nil)
	if err != nil {
		t.Fatalf("ResolveOptions() error = %v, want pass-through", err)
	}
	if want := []string{"--model", "fable"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("ResolveOptions() args = %v, want %v", args, want)
	}
	if metadata["opt_model"] != "fable" {
		t.Fatalf("metadata = %v, want opt_model=fable", metadata)
	}
}

func TestResolveOptions_DefaultUnknownNonUniformValueErrors(t *testing.T) {
	schema := []ProviderOption{mixedPermissionOption()}
	_, _, err := ResolveOptions(schema, nil, map[string]string{"permission_mode": "yolo"})
	if err == nil {
		t.Fatal("ResolveOptions() = nil error, want loud failure instead of silent default drop")
	}
}

func TestResolveExplicitOptions_UnknownUniformValuePassesThrough(t *testing.T) {
	schema := []ProviderOption{uniformModelOption()}
	args, err := ResolveExplicitOptions(schema, map[string]string{"model": "fable"})
	if err != nil {
		t.Fatalf("ResolveExplicitOptions() error = %v, want pass-through", err)
	}
	if want := []string{"--model", "fable"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("ResolveExplicitOptions() args = %v, want %v", args, want)
	}
}

func TestResolveExplicitOptions_UnknownNonUniformValueErrors(t *testing.T) {
	schema := []ProviderOption{mixedPermissionOption()}
	if args, err := ResolveExplicitOptions(schema, map[string]string{"permission_mode": "yolo"}); err == nil {
		t.Fatalf("ResolveExplicitOptions() = %v, want error", args)
	}
}

func TestValidateOptionDefaults_AcceptsPassthroughValue(t *testing.T) {
	schema := []ProviderOption{uniformModelOption()}
	if err := ValidateOptionDefaults(schema, map[string]string{"model": "fable"}); err != nil {
		t.Fatalf("ValidateOptionDefaults() error = %v, want pass-through accepted", err)
	}
	if err := ValidateOptionDefaults(schema, map[string]string{"model": "fa ble"}); err == nil {
		t.Fatal("ValidateOptionDefaults() = nil, want error for malformed value")
	}
	if err := ValidateOptionDefaults([]ProviderOption{mixedPermissionOption()}, map[string]string{"permission_mode": "yolo"}); err == nil {
		t.Fatal("ValidateOptionDefaults() = nil, want error for non-uniform option")
	}
}

func TestValidateOptionsSchema_AcceptsPassthroughDefault(t *testing.T) {
	opt := uniformModelOption()
	opt.Default = "fable"
	if err := ValidateOptionsSchema([]ProviderOption{opt}); err != nil {
		t.Fatalf("ValidateOptionsSchema() error = %v, want pass-through accepted", err)
	}
	mixed := mixedPermissionOption()
	mixed.Default = "yolo"
	if err := ValidateOptionsSchema([]ProviderOption{mixed}); err == nil {
		t.Fatal("ValidateOptionsSchema() = nil, want error for non-uniform option")
	}
}

func TestUniformChoiceFlagWord(t *testing.T) {
	cases := []struct {
		name    string
		choices []OptionChoice
		want    string
	}{
		{"uniform model", uniformModelOption().Choices, "--model"},
		{"mixed arity", mixedPermissionOption().Choices, ""},
		{"no emitting choices", []OptionChoice{{Value: ""}}, ""},
		{"differing flag words", []OptionChoice{
			{Value: "a", FlagArgs: []string{"--model", "a"}},
			{Value: "b", FlagArgs: []string{"--engine", "b"}},
		}, ""},
		{"value position holds a flag", []OptionChoice{
			{Value: "a", FlagArgs: []string{"--opts", "--deep"}},
		}, ""},
		{"single choice uniform", []OptionChoice{
			{Value: "a", FlagArgs: []string{"--effort", "a"}},
		}, "--effort"},
	}
	for _, tc := range cases {
		if got := uniformChoiceFlagWord(tc.choices); got != tc.want {
			t.Errorf("%s: uniformChoiceFlagWord() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPassthroughValueOK(t *testing.T) {
	for _, ok := range []string{"fable", "claude-fable-5", "gpt-5.6-sol", "opencode/big-pickle", "xiaomi-token-plan-sgp/mimo-v2.5", "o3", "K2:instruct", "a@b"} {
		if !passthroughValueOK(ok) {
			t.Errorf("passthroughValueOK(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "-m", "--model", "fa ble", "fable;x", "fable&&x", "$MODEL", ".hidden", "/abs", "a=b", "fable\tx", "fable\nx", "quote'x", `quote"x`} {
		if passthroughValueOK(bad) {
			t.Errorf("passthroughValueOK(%q) = true, want false", bad)
		}
	}
}

// Resume-command completion appends a pass-through default when the command
// lacks the flag, and recognizes an already-present synthesized pair (shape
// match) so it never doubles the flag.
func TestMissingDefaultArgs_PassthroughAndNoDuplicate(t *testing.T) {
	schema := []ProviderOption{uniformModelOption()}
	defaults := map[string]string{"model": "fable"}

	got := missingDefaultArgsForCommand("claude --resume abc", schema, defaults)
	if want := []string{"--model", "fable"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("missingDefaultArgsForCommand() = %v, want %v", got, want)
	}

	if got := missingDefaultArgsForCommand("claude --model fable --resume abc", schema, defaults); len(got) != 0 {
		t.Fatalf("missingDefaultArgsForCommand() = %v, want none (flag already present)", got)
	}
}
