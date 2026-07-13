package main

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// ga-b0flc8 regression: an agent.toml [option_defaults] model pin the builtin
// choice table does not declare (the "fable" alias) was silently dropped on
// the reconciler create path — the seat launched with no --model flag at all
// and fell back to the fleet-default settings.json model. The pin must reach
// the composed command verbatim.
func TestResolveTemplatePassesUnknownModelPinThroughVerbatim(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "claude"},
		providers:  map[string]config.ProviderSpec{"claude": {}},
		lookPath:   func(string) (string, error) { return "/usr/local/bin/claude", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agent := &config.Agent{
		Name:           "persona-ariadne-sim",
		Provider:       "claude",
		OptionDefaults: map[string]string{"model": "fable"},
	}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	tokens := shellquote.Split(tp.Command)
	modelAt := -1
	for i, tok := range tokens {
		if tok == "--model" {
			modelAt = i
			break
		}
	}
	if modelAt == -1 || modelAt+1 >= len(tokens) {
		t.Fatalf("Command = %q, want a --model flag (pin silently dropped — ga-b0flc8)", tp.Command)
	}
	if tokens[modelAt+1] != "fable" {
		t.Fatalf("Command = %q, want --model fable passed through verbatim, got --model %q", tp.Command, tokens[modelAt+1])
	}
	// The other schema defaults must still compose alongside the pass-through.
	if !strings.Contains(tp.Command, "--dangerously-skip-permissions") {
		t.Fatalf("Command = %q, lost permission_mode default while passing model through", tp.Command)
	}
}

// The loud-fail half of the contract: a default that can neither map to a
// declared choice nor pass through verbatim must fail the spawn, not launch
// the seat without the configured flag.
func TestResolveTemplateFailsSpawnOnUnresolvableOptionDefault(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "claude"},
		providers:  map[string]config.ProviderSpec{"claude": {}},
		lookPath:   func(string) (string, error) { return "/usr/local/bin/claude", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agent := &config.Agent{
		Name:           "persona-bogus-sim",
		Provider:       "claude",
		OptionDefaults: map[string]string{"permission_mode": "bogus-mode"},
	}
	_, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err == nil {
		t.Fatal("resolveTemplate = nil error, want loud spawn failure for unresolvable permission_mode default")
	}
	if !strings.Contains(err.Error(), "permission_mode") {
		t.Fatalf("error %q should name the failing option", err)
	}
}
