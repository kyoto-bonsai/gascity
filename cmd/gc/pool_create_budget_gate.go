// pool_create_budget_gate.go — detect→signal for the reconciler's shared
// pool session-create budget (poolplan.CreateBudget). Mirrors
// provider_health_gate.go's alert shape (one alert per episode, not one per
// tick) but tracks episode state via a small on-disk cache file instead of
// an in-memory struct living on CityRuntime: agentBuildParams — unlike
// CityRuntime — is rebuilt fresh every tick, so there is nowhere in-process
// to keep state across calls.
//
// Before this existed, errPoolSessionCreateBudgetExhausted was logged to
// stderr only (buildDesiredState's "fresh create deferred" line) with no
// bead, event, or other operator-visible signal — see ga-stpvzg.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

const (
	// poolCreateBudgetGateRelPath mirrors providerHealthCacheRelPath's
	// directory convention (provider_health_gate.go).
	poolCreateBudgetGateRelPath = ".gc/cache/pool-create-budget-gate.json"

	// poolCreateBudgetAlertCooldown bounds re-alert frequency for a template
	// that stays exhausted across many consecutive ticks.
	poolCreateBudgetAlertCooldown = 15 * time.Minute

	// poolCreateBudgetEpisodeGap: a template with no recorded deferral for
	// longer than this is treated as a fresh episode (onset time and defer
	// counter reset) rather than a continuation of an old one.
	poolCreateBudgetEpisodeGap = 30 * time.Minute
)

// poolCreateBudgetGateMu serializes read-modify-write access to the gate
// state file. buildDesiredState's own pool-realization loop calls into this
// gate serially (Phase A of realizePoolDesiredSessions is explicitly
// single-threaded), but the mutex is kept — matching providerHealthGate's
// own defensive locking — so this stays correct if that ever changes.
var poolCreateBudgetGateMu sync.Mutex

// poolCreateBudgetGateEntry tracks one pool template's exhaustion episode.
type poolCreateBudgetGateEntry struct {
	FirstDeferredAt time.Time `json:"first_deferred_at"`
	LastSeenAt      time.Time `json:"last_seen_at"`
	LastAlertedAt   time.Time `json:"last_alerted_at"`
	DeferCount      int       `json:"defer_count"`
}

type poolCreateBudgetGateFile struct {
	Templates map[string]poolCreateBudgetGateEntry `json:"templates"`
}

func loadPoolCreateBudgetGateFile(path string) poolCreateBudgetGateFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return poolCreateBudgetGateFile{}
	}
	var f poolCreateBudgetGateFile
	if err := json.Unmarshal(data, &f); err != nil {
		return poolCreateBudgetGateFile{}
	}
	return f
}

func savePoolCreateBudgetGateFile(path string, f poolCreateBudgetGateFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// recordPoolCreateBudgetExhaustion notes that qualifiedName's fresh pool
// session create was deferred this tick because the shared create budget
// was exhausted (errPoolSessionCreateBudgetExhausted). It escalates beyond
// the caller's existing per-tick stderr line by firing a structured
// gc-events record — at most once per poolCreateBudgetAlertCooldown per
// template, and immediately on a fresh episode — so a sustained exhaustion
// is discoverable via `gc events` without tailing supervisor.log.
//
// Best-effort: state-file or event-write failures are logged to stderr and
// never block the reconciler tick.
func recordPoolCreateBudgetExhaustion(cityPath, qualifiedName string, now time.Time, stderr io.Writer) {
	poolCreateBudgetGateMu.Lock()
	defer poolCreateBudgetGateMu.Unlock()

	path := filepath.Join(cityPath, poolCreateBudgetGateRelPath)
	gateFile := loadPoolCreateBudgetGateFile(path)
	if gateFile.Templates == nil {
		gateFile.Templates = make(map[string]poolCreateBudgetGateEntry)
	}

	entry, existed := gateFile.Templates[qualifiedName]
	freshEpisode := !existed || now.Sub(entry.LastSeenAt) > poolCreateBudgetEpisodeGap
	if freshEpisode {
		entry = poolCreateBudgetGateEntry{FirstDeferredAt: now}
	}
	entry.DeferCount++
	entry.LastSeenAt = now

	if freshEpisode || now.Sub(entry.LastAlertedAt) >= poolCreateBudgetAlertCooldown {
		emitPoolCreateBudgetGateAlert(cityPath, qualifiedName, entry.FirstDeferredAt, entry.DeferCount, now, stderr)
		entry.LastAlertedAt = now
	}

	gateFile.Templates[qualifiedName] = entry
	if err := savePoolCreateBudgetGateFile(path, gateFile); err != nil {
		fmt.Fprintf(stderr, "recordPoolCreateBudgetExhaustion: persisting gate state for %q: %v\n", qualifiedName, err) //nolint:errcheck
	}
}

// emitPoolCreateBudgetGateAlert writes an operator-facing escalation line to
// stderr and records a PoolCreateBudgetGateAlert event. Call shape mirrors
// emitProviderHealthGateAlert (provider_health_gate.go).
func emitPoolCreateBudgetGateAlert(cityPath, qualifiedName string, since time.Time, deferCount int, now time.Time, stderr io.Writer) {
	msg := fmt.Sprintf(
		"Pool session-create budget gate OPEN: template=%s since=%s deferred=%d. "+
			"Fresh spawns for %s are being deferred (shared daemon.max_wakes_per_tick budget "+
			"exhausted this tick); existing-session reuse is unaffected. Check `gc session list` "+
			"for idle/finished seats holding slots. Do NOT raise [daemon].max_wakes_per_tick to "+
			"compensate (see ga-v8mtlp: raising the rate limiter to mask a slot-release problem "+
			"is what produced the 105-seat herd).",
		qualifiedName, since.UTC().Format(time.RFC3339), deferCount, qualifiedName,
	)
	fmt.Fprintln(stderr, msg) //nolint:errcheck

	ep := defaultOpenStoreHealthEvents(cityPath, stderr)
	if ep == nil {
		return
	}
	defer func() {
		if closer, ok := ep.(io.Closer); ok {
			_ = closer.Close()
		}
	}()
	payload, _ := json.Marshal(map[string]any{
		"template":        qualifiedName,
		"exhausted_since": since.UTC().Format(time.RFC3339),
		"defer_count":     deferCount,
	})
	ep.Record(events.Event{
		Type:    events.PoolCreateBudgetGateAlert,
		Ts:      now.UTC(),
		Actor:   "gc",
		Subject: qualifiedName,
		Message: msg,
		Payload: payload,
	})
}
