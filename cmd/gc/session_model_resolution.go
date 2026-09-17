package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// Model resolution source vocabulary (ga-dbfydw). A closed, small set so a
// consumer can switch on it exhaustively; add a value here and to
// resolveSessionModel together, never invent a new string at a call site.
const (
	// ModelResolutionSourceExplicit means a session-level template_overrides
	// entry (options.model on the create request, or `gc session set-option`)
	// named the model directly.
	ModelResolutionSourceExplicit = "explicit"
	// ModelResolutionSourceEnv means no explicit override was set, but the
	// resolved launch environment carries a GC_MODEL-style pin (the same
	// mechanism internal/runtime/t3bridge already reads for its own model
	// selection, generalized here to the main launch path).
	ModelResolutionSourceEnv = "env"
	// ModelResolutionSourceDefault means neither of the above applied and the
	// value came from the schema/provider/agent EffectiveDefaults cascade
	// (config.ComputeEffectiveDefaults) -- the fleet default for this template.
	ModelResolutionSourceDefault = "default"
)

// modelResolutionEnvKey is the env var an operator or pack sets to pin a
// model without an explicit per-session override. Mirrors
// t3BridgeStartupEnvelopeModel's own GC_MODEL read
// (cmd/gc/template_resolve_t3bridge.go) -- one name, both launch paths.
const modelResolutionEnvKey = "GC_MODEL"

// resolveSessionModel returns the model string that will actually be passed
// to the provider command, and which tier supplied it, given the same inputs
// applySchemaOptionOverridesForLaunch already has in hand: the resolved
// provider spec (for its schema-derived EffectiveDefaults), the session's
// merged option overrides map (explicit template_overrides; per-dispatch
// opt_model is intentionally not distinguished from it here -- both are "a
// human or dispatcher explicitly chose this," and the acceptance criteria
// asks for one "explicit pin" category, not a four-way split), and the
// resolved launch environment (for the env tier).
//
// Precedence, matching applySchemaOptionOverridesForLaunch's own merge order:
// explicit overrides beat everything (fullOptions overlays overrides onto
// EffectiveDefaults), so explicit beats env beats default here too. Returns
// ("", "") if resolved is nil or carries no "model" option at all (a provider
// whose schema has no model concept, or a launch that never reached
// resolution -- start-pending, or the config-load-time hard error an
// unsupported --provider name produces before any session bead exists, see
// config.ResolveProvider/ErrProviderNotFound: that case never reaches this
// function, there is no session to resolve a model for).
func resolveSessionModel(resolved *config.ResolvedProvider, overrides map[string]string, env map[string]string) (model, source string) {
	if resolved == nil {
		return "", ""
	}
	// Gate on the KEY existing in EffectiveDefaults, not on its value being
	// non-empty: EffectiveDefaults covers every schema-declared option, so an
	// absent "model" key means this provider's schema has no model concept at
	// all, and neither an explicit override nor an env pin should fabricate
	// one for it. A present-but-empty default (a provider that declares the
	// option but ships no fleet-wide default) still allows explicit/env to
	// apply -- only the schema's own declaration gates this, not the default's
	// value.
	if _, hasModelOption := resolved.EffectiveDefaults["model"]; !hasModelOption {
		return "", ""
	}
	if v := strings.TrimSpace(overrides["model"]); v != "" {
		return v, ModelResolutionSourceExplicit
	}
	if v := strings.TrimSpace(env[modelResolutionEnvKey]); v != "" {
		return v, ModelResolutionSourceEnv
	}
	if v := strings.TrimSpace(resolved.EffectiveDefaults["model"]); v != "" {
		return v, ModelResolutionSourceDefault
	}
	return "", ""
}
