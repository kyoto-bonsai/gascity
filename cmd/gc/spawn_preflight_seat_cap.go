// spawn_preflight_seat_cap.go extends the P2 spawn preflight gate
// (spawn_preflight_gate.go, ga-6l32x0) with a third check (ga-mpb0xu):
// refuse create/sling dispatches that would push a provider's concurrent
// active-session count past its ratified city.toml max_seats cap. Complements
// the existing agent.toml max_active_sessions pool-autoscale layer — neither
// alone closes the seat-cap hole (a named-session fan like txw-1..8 bypasses
// pool autoscale entirely), per ga-5gbi30's layering note.
package main

import (
	"fmt"
	"os/exec"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// seatCapLookPathHook is config.ResolveProvider's PATH-existence check,
// swappable in tests (same seam convention as supervisorAliveHook) so
// counting sessions against the seat cap doesn't depend on every
// configured provider's binary actually being installed on the test
// machine's PATH.
var seatCapLookPathHook config.LookPathFunc = exec.LookPath

// checkProviderSeatCap counts open sessions currently attributed to
// providerName (via each session's template->provider resolution) and
// refuses a dispatch once that count has already reached the provider's
// ratified max_seats — the cap itself is reachable; only the dispatch that
// would push the count past it is refused. Fails open — same posture as
// the other two P2 checks — when store/cfg/providerName are unset, the
// provider has no configured cap
// (nil MaxSeats), or the cap is the explicit-unlimited sentinel (-1). A
// snapshot-load error also fails open: this gate augments dispatch, it
// doesn't replace the store's own error surfacing.
func checkProviderSeatCap(store beads.Store, cfg *config.City, providerName string) spawnPreflightResult {
	if store == nil || cfg == nil || providerName == "" {
		return spawnPreflightResult{}
	}
	spec, ok := cfg.Providers[providerName]
	if !ok || spec.MaxSeats == nil || *spec.MaxSeats < 0 {
		return spawnPreflightResult{}
	}
	seatCap := *spec.MaxSeats

	snap, err := loadSessionBeadSnapshot(store)
	if err != nil {
		return spawnPreflightResult{}
	}

	count := 0
	for _, info := range snap.OpenInfos() {
		if sessionProviderName(info, cfg) == providerName {
			count++
		}
	}
	if count < seatCap {
		return spawnPreflightResult{}
	}
	return spawnPreflightResult{
		Blocked: true,
		Reason: fmt.Sprintf("provider %q is at its ratified seat cap (%d/%d active sessions)",
			providerName, count, seatCap),
		FixCmd: fmt.Sprintf("wait for a %s session to close, or raise [providers.%s].max_seats in city.toml",
			providerName, providerName),
	}
}

// sessionProviderName resolves the provider an open session is attributed
// to, via the same template->agent->provider chain a fresh dispatch for that
// template would resolve (findAgentByTemplate + config.ResolveProvider).
// Returns "" when the session's template no longer matches a configured
// agent (stale/removed template) or provider resolution fails — such
// sessions are excluded from every provider's count rather than
// misattributed to one.
func sessionProviderName(info sessionpkg.Info, cfg *config.City) string {
	agent := findAgentByTemplate(cfg, info.Template)
	if agent == nil {
		return ""
	}
	resolved, err := config.ResolveProvider(agent, &cfg.Workspace, cfg.Providers, seatCapLookPathHook)
	if err != nil {
		return ""
	}
	return resolved.Name
}
