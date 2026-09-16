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
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// seatCapStaleActiveAfter bounds how long a session in a capacity-owning
// state (per sessionpkg.CountsAgainstCapacity) may go without its bead
// being touched before the seat cap stops trusting it (ga-r1wouq). A
// crashed process leaves its bead's state metadata claiming the runtime is
// still live; nothing else corrects that field until the next health-patrol
// cycle or an explicit `gc session` command touches the bead, so an
// unbounded count accumulates every such session forever — the reported
// symptom was a supervisor sessions API showing 6 live Codex sessions while
// the cap counter, unfiltered, read 20+ against a cap of 16, counting
// stale/asleep beads back to 2026-08-07. 30 minutes is generous relative to
// normal agent turn cadence (a bead's UpdatedAt bumps on essentially every
// heartbeat/metadata write) and to the reconciler's own health-patrol
// cycle, which should heal a truly-dead session's state well before this
// bound is reached. A judgment call, not derived from any measurement —
// adjust freely; this is the first time this signal has been observed.
const seatCapStaleActiveAfter = 30 * time.Minute

// seatCapLookPathHook is config.ResolveProvider's PATH-existence check,
// swappable in tests (same seam convention as supervisorAliveHook) so
// counting sessions against the seat cap doesn't depend on every
// configured provider's binary actually being installed on the test
// machine's PATH.
var seatCapLookPathHook config.LookPathFunc = exec.LookPath

// checkProviderSeatCap counts LIVE sessions currently attributed to
// providerName (via each session's template->provider resolution) and
// refuses a dispatch once that count has already reached the provider's
// ratified max_seats — the cap itself is reachable; only the dispatch that
// would push the count past it is refused. "Live" (ga-r1wouq) means: the
// session is in a state sessionpkg.CountsAgainstCapacity recognizes as
// owning (or about to own, or mid-shutdown still holding) a real runtime
// process, AND its bead has been touched within seatCapStaleActiveAfter —
// an open session bead in any other state (asleep, suspended, drained,
// archived, failed-create, orphaned) or whose "active"-claiming state has
// gone stale does not count. Fails open — same posture as the other two P2
// checks — when store/cfg/providerName are unset, the provider has no
// configured cap (nil MaxSeats), or the cap is the explicit-unlimited
// sentinel (-1). A snapshot-load error also fails open: this gate augments
// dispatch, it doesn't replace the store's own error surfacing.
func checkProviderSeatCap(store beads.Store, cfg *config.City, providerName string) spawnPreflightResult {
	return checkProviderSeatCapAt(store, cfg, providerName, clock.Real{})
}

// checkProviderSeatCapAt is checkProviderSeatCap with an injected clock, so
// tests can construct stale-heartbeat fixtures deterministically instead of
// racing wall-clock time — same seam convention as
// pendingCreateNeverStartedLeaseExpiredInfo's clk parameter.
func checkProviderSeatCapAt(store beads.Store, cfg *config.City, providerName string, clk clock.Clock) spawnPreflightResult {
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
	excluded := map[string]int{} // ga-r1wouq acceptance: "diagnostic output names excluded-state counts"
	for _, info := range snap.OpenInfos() {
		if sessionProviderName(info, cfg) != providerName {
			continue
		}
		if sessionCountsForSeatCap(info, clk) {
			count++
			continue
		}
		excluded[excludedSeatCapReason(info)]++
	}
	if count < seatCap {
		return spawnPreflightResult{}
	}
	return spawnPreflightResult{
		Blocked: true,
		Reason: fmt.Sprintf("provider %q is at its ratified seat cap (%d/%d active sessions)%s",
			providerName, count, seatCap, formatExcludedSeatCapCounts(excluded)),
		FixCmd: fmt.Sprintf("wait for a %s session to close, or raise [providers.%s].max_seats in city.toml",
			providerName, providerName),
	}
}

// sessionCountsForSeatCap reports whether an open session should be counted
// against a provider's ratified seat cap (ga-r1wouq): its state must be one
// sessionpkg.CountsAgainstCapacity recognizes as owning (or about to own, or
// mid-shutdown still holding) a real runtime process, AND — for exactly
// those states, which all claim a live or imminent runtime — its bead must
// have been touched within seatCapStaleActiveAfter of now. A session whose
// structural state already excludes it (asleep, suspended, drained,
// archived, failed-create, orphaned, none) is excluded regardless of
// freshness; there is nothing "live" about it to go stale.
func sessionCountsForSeatCap(info sessionpkg.Info, clk clock.Clock) bool {
	if !sessionpkg.CountsAgainstCapacity(info.State) {
		return false
	}
	now := time.Now()
	if clk != nil {
		now = clk.Now()
	}
	if info.UpdatedAt.IsZero() {
		// No bead touch recorded at all -- CreatedAt is the only signal
		// available (e.g. a freshly-reserved start-pending identity that has
		// not been written to since); fall back to it so a just-created
		// session isn't excluded before it has had a chance to run.
		return !info.CreatedAt.IsZero() && now.Sub(info.CreatedAt) <= seatCapStaleActiveAfter
	}
	return now.Sub(info.UpdatedAt) <= seatCapStaleActiveAfter
}

// excludedSeatCapReason labels why sessionCountsForSeatCap excluded a
// session, for the diagnostic breakdown: either its raw state string
// (asleep, suspended, ...) or "stale-<state>" when the state itself would
// count but the freshness check is what excluded it. No clock needed: the
// caller only reaches here for a session sessionCountsForSeatCap already
// rejected, so if the state itself counts, staleness must have been why.
func excludedSeatCapReason(info sessionpkg.Info) string {
	state := string(info.State)
	if state == "" {
		state = "none"
	}
	if sessionpkg.CountsAgainstCapacity(info.State) {
		return "stale-" + state
	}
	return state
}

// formatExcludedSeatCapCounts renders the excluded-state breakdown as a
// suffix for the block Reason (ga-r1wouq: "log numerator membership for
// diagnosis without credentials") — sorted for deterministic output. Empty
// when nothing was excluded.
func formatExcludedSeatCapCounts(excluded map[string]int) string {
	if len(excluded) == 0 {
		return ""
	}
	keys := make([]string, 0, len(excluded))
	for k := range excluded {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, excluded[k]))
	}
	return fmt.Sprintf(" (excluded: %s)", strings.Join(parts, ", "))
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
