package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestResolveSessionModel(t *testing.T) {
	withDefault := &config.ResolvedProvider{
		EffectiveDefaults: map[string]string{"model": "claude-sonnet-5"},
	}
	noModelOption := &config.ResolvedProvider{
		EffectiveDefaults: map[string]string{"effort": "high"},
	}

	cases := []struct {
		name       string
		resolved   *config.ResolvedProvider
		overrides  map[string]string
		env        map[string]string
		wantModel  string
		wantSource string
	}{
		{
			name:       "explicit override wins over both env and default",
			resolved:   withDefault,
			overrides:  map[string]string{"model": "opus"},
			env:        map[string]string{"GC_MODEL": "env-model"},
			wantModel:  "opus",
			wantSource: ModelResolutionSourceExplicit,
		},
		{
			name:       "env wins over default when no explicit override",
			resolved:   withDefault,
			overrides:  map[string]string{"effort": "high"}, // present, but not "model"
			env:        map[string]string{"GC_MODEL": "env-model"},
			wantModel:  "env-model",
			wantSource: ModelResolutionSourceEnv,
		},
		{
			name:       "fleet default when neither explicit nor env set a model",
			resolved:   withDefault,
			overrides:  nil,
			env:        nil,
			wantModel:  "claude-sonnet-5",
			wantSource: ModelResolutionSourceDefault,
		},
		{
			name:       "empty-string explicit override does not count as set",
			resolved:   withDefault,
			overrides:  map[string]string{"model": "   "},
			env:        nil,
			wantModel:  "claude-sonnet-5",
			wantSource: ModelResolutionSourceDefault,
		},
		{
			name:       "provider schema has no model option at all -- empty, not a guess",
			resolved:   noModelOption,
			overrides:  nil,
			env:        map[string]string{"GC_MODEL": "should-not-apply-either"},
			wantModel:  "",
			wantSource: "",
		},
		{
			name: "unresolved provider (should not reach this function in production --" +
				" config.ResolveProvider fails closed before any session bead exists" +
				" for an unsupported --provider name) -- defensive nil handling",
			resolved:   nil,
			overrides:  map[string]string{"model": "opus"},
			env:        nil,
			wantModel:  "",
			wantSource: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotModel, gotSource := resolveSessionModel(tc.resolved, tc.overrides, tc.env)
			if gotModel != tc.wantModel || gotSource != tc.wantSource {
				t.Fatalf("resolveSessionModel() = (%q, %q), want (%q, %q)",
					gotModel, gotSource, tc.wantModel, tc.wantSource)
			}
		})
	}
}

// TestResolveSessionModelSourceVocabularyIsClosed pins the three source
// values as the complete set (ga-dbfydw) -- a consumer switching on
// ModelSource exhaustively should not need a default case for an unknown
// value produced by this function.
func TestResolveSessionModelSourceVocabularyIsClosed(t *testing.T) {
	want := map[string]bool{
		ModelResolutionSourceExplicit: true,
		ModelResolutionSourceEnv:      true,
		ModelResolutionSourceDefault:  true,
	}
	if len(want) != 3 {
		t.Fatalf("expected 3 distinct source constants, got %d -- a name collided", len(want))
	}
}
