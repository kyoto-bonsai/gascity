package doctor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

func TestOrderFiringCurrent_NeverFired_BeyondUptime(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cron", "0 */4 * * *")
	writeOrderFiringTestEvents(t, cityPath, events.Event{
		Type: events.ControllerStarted,
		Ts:   now.Add(-8 * time.Hour),
	})

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	// The "never fired beyond uptime" path is advisory — it most often
	// reflects the cron-scheduler bug (ga-97qngx) rather than a real
	// outage, so it must not wedge dispatch gates that read BlockingFailed.
	if result.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want SeverityAdvisory for never-fired-beyond-uptime path", result.Severity)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "never fired since controller start") {
		t.Fatalf("details = %v, want never-fired controller-start message", result.Details)
	}
	if result.FixHint != "Inspect with: gc order check && gc order history mol-dog-stale-db" {
		t.Fatalf("FixHint = %q, want inspect hint for order", result.FixHint)
	}
}

func TestOrderFiringCurrent_Stale_StaysBlocking(t *testing.T) {
	// Cooldown stale (CRITICAL) must remain blocking even though the
	// sibling "never fired" path was demoted to advisory; the stale
	// signal reflects a real execution gap consumers should gate on.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-6 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if result.Severity != SeverityBlocking {
		t.Fatalf("Severity = %v, want SeverityBlocking for cooldown stale", result.Severity)
	}
}

func TestOrderFiringCurrent_MixedAdvisoryAndBlocking_AggregatesBlocking(t *testing.T) {
	// One advisory (never-fired cron) + one blocking (cooldown stale) →
	// aggregate severity must be Blocking so the presence of any real
	// outage keeps gates closed even if other entries are merely advisory.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cron", "0 */4 * * *")
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-6 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if result.Severity != SeverityBlocking {
		t.Fatalf("Severity = %v, want SeverityBlocking when any non-OK entry is blocking", result.Severity)
	}
}

func TestOrderFiringCurrent_MixedAdvisoryAndWarning_AggregatesAdvisory(t *testing.T) {
	// A warning-level overdue order should stay visible in details without
	// converting an advisory error into a blocking gate failure.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cron", "0 */4 * * *")
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-2 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if result.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want SeverityAdvisory when only error entries are advisory", result.Severity)
	}
}

func TestOrderFiringCurrent_NeverFiredCooldown_StaysBlocking(t *testing.T) {
	// Never-fired cooldown orders represent the same execution gap as stale
	// cooldown orders and should continue to gate dispatch consumers.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath, events.Event{
		Type: events.ControllerStarted,
		Ts:   now.Add(-2 * time.Hour),
	})

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if result.Severity != SeverityBlocking {
		t.Fatalf("Severity = %v, want SeverityBlocking for never-fired cooldown", result.Severity)
	}
}

func TestOrderFiringCurrent_NeverFired_WithinFirstCycle(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cron", "0 */4 * * *")
	writeOrderFiringTestEvents(t, cityPath, events.Event{
		Type: events.ControllerStarted,
		Ts:   now.Add(-30 * time.Minute),
	})

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "within first cycle") {
		t.Fatalf("details = %v, want within-first-cycle message", result.Details)
	}
}

func TestOrderFiringCurrent_FiredRecently(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cron", "0 */4 * * *")
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "4h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-8 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "mol-dog-stale-db", Ts: now.Add(-1 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-1 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "last fired 1h ago, expected every 4h") {
		t.Fatalf("details = %v, want recent-fire detail", result.Details)
	}
}

func TestOrderFiringCurrent_UsesNewestOrderRunHistory(t *testing.T) {
	// Manual `gc order run` creates order-run beads even when no controller
	// order.fired event is emitted. Doctor must therefore merge bead history
	// with event history and select the newest execution, not a stale event.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cron", "0 */4 * * *")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "mol-dog-stale-db", Ts: now.Add(-13 * time.Hour)},
	)
	store := beads.NewMemStoreFrom(2, []beads.Bead{
		{
			ID:        "old-run",
			Title:     "old mol-dog-stale-db",
			Status:    "closed",
			Type:      "molecule",
			Labels:    []string{"order-run:mol-dog-stale-db"},
			CreatedAt: now.Add(-13 * time.Hour),
		},
		{
			ID:        "new-run",
			Title:     "new mol-dog-stale-db",
			Status:    "closed",
			Type:      "molecule",
			Labels:    []string{"order-run:mol-dog-stale-db"},
			CreatedAt: now.Add(-1 * time.Hour),
		},
	}, nil)

	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(func(order orders.Order) (time.Time, error) {
		return orders.NewStoreWithGraph(beads.OrdersStore{Store: store}, beads.GraphStore{Store: store}).LastRun(order.ScopedName())
	}))
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "last fired 1h ago, expected every 4h") {
		t.Fatalf("details = %v, want newest order-run bead to win over stale event", result.Details)
	}
}

func TestOrderFiringCurrent_SkipsSuspendedRigOrders(t *testing.T) {
	// The dispatcher intentionally skips suspended rigs. Doctor should not turn
	// their paused recurring orders into blocking stale-order failures.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	rigPath := filepath.Join(cityPath, "rigs", "parked")
	rigFormulas := filepath.Join(rigPath, "formulas")
	rigOrders := filepath.Join(rigPath, "orders")
	if err := os.MkdirAll(rigOrders, 0o755); err != nil {
		t.Fatalf("creating rig orders dir: %v", err)
	}
	cfg.Rigs = []config.Rig{{Name: "parked", Path: rigPath, Suspended: true}}
	cfg.FormulaLayers.Rigs = map[string][]string{"parked": {cfg.FormulaLayers.City[0], rigFormulas}}
	writeOrderFiringTestOrderInDir(t, rigOrders, "gate-sweep", "cooldown", "1m")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "gate-sweep:rig:parked", Ts: now.Add(-24 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK for paused rig; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if strings.Contains(strings.Join(result.Details, "\n"), "parked") {
		t.Fatalf("details = %v, suspended rig order should be skipped", result.Details)
	}
}

func TestOrderFiringCurrent_SkipsSuspendedRigOverrides(t *testing.T) {
	// Suspended rig orders are pruned from the doctor scan; matching overrides
	// must be pruned with them so a harmless paused rig does not become a scan
	// error before the stale-order filter can skip it.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	rigPath := filepath.Join(cityPath, "rigs", "parked")
	rigFormulas := filepath.Join(rigPath, "formulas")
	rigOrders := filepath.Join(rigPath, "orders")
	if err := os.MkdirAll(rigOrders, 0o755); err != nil {
		t.Fatalf("creating rig orders dir: %v", err)
	}
	cfg.Rigs = []config.Rig{{Name: "parked", Path: rigPath, Suspended: true}}
	cfg.FormulaLayers.Rigs = map[string][]string{"parked": {cfg.FormulaLayers.City[0], rigFormulas}}
	interval := "2m"
	cfg.Orders.Overrides = []config.OrderOverride{{Name: "gate-sweep", Rig: "parked", Interval: &interval}}
	writeOrderFiringTestOrderInDir(t, rigOrders, "gate-sweep", "cooldown", "1m")
	writeOrderFiringTestEvents(t, cityPath, events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)})

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK for paused rig override; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
}

func TestOrderFiringCurrent_SkipsWildcardOverridesForSuspendedOnlyOrders(t *testing.T) {
	// Wildcard overrides should not turn a suspended-only order into a scan
	// error after the doctor prunes that suspended rig from the active view.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	rigPath := filepath.Join(cityPath, "rigs", "parked")
	rigFormulas := filepath.Join(rigPath, "formulas")
	rigOrders := filepath.Join(rigPath, "orders")
	if err := os.MkdirAll(rigOrders, 0o755); err != nil {
		t.Fatalf("creating rig orders dir: %v", err)
	}
	cfg.Rigs = []config.Rig{{Name: "parked", Path: rigPath, Suspended: true}}
	cfg.FormulaLayers.Rigs = map[string][]string{"parked": {cfg.FormulaLayers.City[0], rigFormulas}}
	interval := "2m"
	cfg.Orders.Overrides = []config.OrderOverride{{Name: "gate-sweep", Rig: orders.RigWildcard, Interval: &interval}}
	writeOrderFiringTestOrderInDir(t, rigOrders, "gate-sweep", "cooldown", "1m")
	writeOrderFiringTestEvents(t, cityPath, events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)})

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK for wildcard override targeting only a paused rig; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
}

func TestOrderFiringCurrent_SkipsInvalidOrderDuringScan(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "4h")
	writeOrderFiringRawOrder(t, cityPath, "invalid-env-on-formula", `[order]
formula = "mol-maintenance"
trigger = "manual"

[order.env]
CUSTOM_ORDER_FLAG = "enabled"
`)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-8 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-1 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "cleanup-cooldown: last fired 1h ago, expected every 4h") {
		t.Fatalf("details = %v, want valid order firing detail", result.Details)
	}
	if strings.Contains(details, "invalid-env-on-formula") {
		t.Fatalf("details = %v, want invalid order skipped", result.Details)
	}
}

func TestOrderFiringCurrent_SkipsReservedExecEnvDuringScan(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "4h")
	writeOrderFiringRawOrder(t, cityPath, "invalid-reserved-env", `[order]
exec = "true"
trigger = "cooldown"
interval = "4h"

[order.env]
GC_CITY = "shadow-city"
`)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-8 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-1 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "cleanup-cooldown: last fired 1h ago, expected every 4h") {
		t.Fatalf("details = %v, want valid order firing detail", result.Details)
	}
	if strings.Contains(details, "invalid-reserved-env") {
		t.Fatalf("details = %v, want reserved-env order skipped", result.Details)
	}
}

func TestOrderFiringCurrent_Overdue(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cron", "0 */4 * * *")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-8 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "mol-dog-stale-db", Ts: now.Add(-7 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "(overdue)") {
		t.Fatalf("details = %v, want overdue detail", result.Details)
	}
}

func TestOrderFiringCurrent_Stale(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cron", "0 */4 * * *")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "mol-dog-stale-db", Ts: now.Add(-13 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "(CRITICAL: stale)") {
		t.Fatalf("details = %v, want stale detail", result.Details)
	}
}

func TestOrderFiringCurrent_StaleWithParkedPool_IsAdvisory(t *testing.T) {
	// ga-gfdfdc: mol-dog-stale-db is routed to the "dog" pool, which is
	// deliberately scaled to zero standing instances (min_active_sessions=0,
	// doctrine/token-cost-evaluation-2026-06-07.md — "dog is min=0/stopped").
	// Staleness there reflects the intended scaling policy, not a
	// detection-worthy outage, so it must stay visible (Status still Error)
	// but must not gate BlockingFailed the way a live pool's staleness does.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	cfg.Agents = []config.Agent{{Name: "dog", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}
	writeOrderFiringRawOrder(t, cityPath, "mol-dog-stale-db", `[order]
formula = "mol-dog-stale-db"
trigger = "cron"
schedule = "0 */4 * * *"
pool = "dog"
`)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "mol-dog-stale-db", Ts: now.Add(-13 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "(CRITICAL: stale)") {
		t.Fatalf("details = %v, want stale detail preserved for visibility", result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "pool scaled to 0 standing instances") {
		t.Fatalf("details = %v, want parked-pool annotation", result.Details)
	}
	if result.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want SeverityAdvisory for a stale order whose pool is parked (min=0)", result.Severity)
	}
}

func TestOrderFiringCurrent_StaleWithLiveMinPool_StaysBlocking(t *testing.T) {
	// Adjacent-class control for the parked-pool advisory carve-out above: a
	// pool with a real standing minimum (min_active_sessions=1) that still
	// fails to fire its order is a genuine outage and must keep gating
	// BlockingFailed — the carve-out must not blanket every pool-routed order,
	// only ones whose pool is deliberately scaled to zero.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	cfg.Agents = []config.Agent{{Name: "dog", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2)}}
	writeOrderFiringRawOrder(t, cityPath, "mol-dog-stale-db", `[order]
formula = "mol-dog-stale-db"
trigger = "cron"
schedule = "0 */4 * * *"
pool = "dog"
`)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "mol-dog-stale-db", Ts: now.Add(-13 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if result.Severity != SeverityBlocking {
		t.Fatalf("Severity = %v, want SeverityBlocking — pool has a live standing minimum, so staleness is a real outage", result.Severity)
	}
}

func TestOrderFiringCurrent_StaleWithUnsetMinPool_StaysBlocking(t *testing.T) {
	// Discriminating control persona-marcus's ga-gfdfdc validation demanded:
	// MinActiveSessions left nil (never configured) is NOT the same claim as
	// MinActiveSessions explicitly set to 0 (deliberately parked), and must
	// not be treated as one. 88 of this city's 92 live agents have the field
	// unset — TestOrderFiringCurrent_StaleWithParkedPool_IsAdvisory's intPtr(0)
	// and TestOrderFiringCurrent_StaleWithLiveMinPool_StaysBlocking's intPtr(1)
	// both leave this shape unexercised, so neither could have caught a
	// regression back to EffectiveMinActiveSessions()'s nil-means-zero
	// semantics, which is exactly what shipped here initially.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	cfg.Agents = []config.Agent{{Name: "dog"}}
	writeOrderFiringRawOrder(t, cityPath, "mol-dog-stale-db", `[order]
formula = "mol-dog-stale-db"
trigger = "cron"
schedule = "0 */4 * * *"
pool = "dog"
`)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "mol-dog-stale-db", Ts: now.Add(-13 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if result.Severity != SeverityBlocking {
		t.Fatalf("Severity = %v, want SeverityBlocking — MinActiveSessions unset is not an assertion the pool was deliberately parked", result.Severity)
	}
	if strings.Contains(strings.Join(result.Details, "\n"), "pool scaled to 0 standing instances") {
		t.Fatalf("details = %v, must not carry the parked-pool annotation for an unset (never-configured) pool", result.Details)
	}
}

func TestOrderFiringCurrent_ParkedPlusLiveStale_StaysBlocking(t *testing.T) {
	// Pins the bead's actual claimed benefit (persona-marcus's ga-gfdfdc
	// validation, probe 2), not just the demotion mechanism: a parked pool's
	// advisory demotion must not mask a DIFFERENT, genuinely live pool's real
	// stale-order incident sitting alongside it in the same check run. Both
	// sibling tests above use a single-order city, so neither proves the
	// mixed shape — 20+ monitored orders in production — actually keeps
	// gating when it matters.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	cfg.Agents = []config.Agent{
		{Name: "dog", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
		{Name: "content-writer", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2)},
	}
	// Parked pool, stale — should be demoted to advisory.
	writeOrderFiringRawOrder(t, cityPath, "mol-dog-stale-db", `[order]
formula = "mol-dog-stale-db"
trigger = "cron"
schedule = "0 */4 * * *"
pool = "dog"
`)
	// Live pool, ALSO stale — a real incident hiding underneath. Must still block.
	writeOrderFiringRawOrder(t, cityPath, "real-incident-order", `[order]
formula = "real-incident-order"
trigger = "cron"
schedule = "0 */4 * * *"
pool = "content-writer"
`)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "mol-dog-stale-db", Ts: now.Add(-13 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "real-incident-order", Ts: now.Add(-13 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, "real-incident-order") {
		t.Fatalf("details lost the live-pool order entirely: %v", result.Details)
	}
	if result.Severity != SeverityBlocking {
		t.Fatalf("Severity = %v, want SeverityBlocking — a stale order on a LIVE pool "+
			"must not be masked by a parked order's advisory demotion; details = %v",
			result.Severity, result.Details)
	}
}

func TestOrderFiringCurrent_IgnoresManualAndEventTriggers(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "manual-maintenance", "manual", "")
	writeOrderFiringTestOrder(t, cityPath, "convoy-check", "event", "bead.closed")
	writeOrderFiringTestOrder(t, cityPath, "condition-check", "condition", "")
	writeOrderFiringTestEvents(t, cityPath, events.Event{
		Type: events.ControllerStarted,
		Ts:   now.Add(-8 * time.Hour),
	})

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if len(result.Details) != 0 {
		t.Fatalf("details = %v, want no rows for manual/event triggers", result.Details)
	}
}

func TestComputeExpectedIntervalForCronSchedules(t *testing.T) {
	tests := []struct {
		name     string
		schedule string
		want     time.Duration
	}{
		{"every-4h", "0 */4 * * *", 4 * time.Hour},
		{"every-15min", "*/15 * * * *", 15 * time.Minute},
		{"daily-0300", "0 3 * * *", 24 * time.Hour},
		{"hourly-business", "0 9-17 * * *", time.Hour},
		// #2499: schedules coarser than daily must compute an honest interval
		// instead of erroring on an empty 24h scan window. Weekly, biweekly,
		// monthly, and yearly are the common shapes; the progressive-widen
		// algorithm walks 24h → 7d → 31d → 366d. Per the Copilot review on
		// #2525, a single match in a smaller window no longer fixes the
		// interval — the algorithm keeps widening until either a second match
		// lands or the largest window is exhausted, at which point the window
		// length is used as a conservative interval.
		{"weekly-monday-0830", "30 8 * * 1", 7 * 24 * time.Hour}, // 31d window: 5 Mondays → minGap 7d
		{"weekly-sunday", "0 0 * * 0", 7 * 24 * time.Hour},
		{"biweekly-1st-and-15th", "0 0 1,15 * *", 14 * 24 * time.Hour}, // 31d window: Jan 1, 15, Feb 1, 15, ... → minGap 14d
		{"mon-wed-fri-0830", "30 8 * * 1,3,5", 2 * 24 * time.Hour},     // 7d window has 3 matches → minGap min(Mon→Wed, Wed→Fri) = 2d
		{"monthly-first-midnight", "0 0 1 * *", 29 * 24 * time.Hour},   // 31d window: only Jan 1 → continue. 366d window: 12 matches → minGap = Feb→Mar in leap-year base 2024 = 29d
		{"yearly-new-year", "0 0 1 1 *", 366 * 24 * time.Hour},         // 366d window: Jan 1 base + (next Jan 1 at boundary excluded) → single match → window length
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := computeExpectedIntervalForCronSchedule(tt.schedule)
			if err != nil {
				t.Fatalf("computeExpectedIntervalForCronSchedule(%q): %v", tt.schedule, err)
			}
			if got != tt.want {
				t.Fatalf("computeExpectedIntervalForCronSchedule(%q) = %s, want %s", tt.schedule, got, tt.want)
			}
		})
	}
}

// TestComputeExpectedIntervalForCronSchedule_YearlyWithFirstWindowMatch
// pins the Copilot review finding on PR #2525 thread @ line 211: a yearly
// schedule whose firing minute coincidentally falls inside the 24h or 7d
// window must not be classified as a 24h/7d interval. The progressive-widen
// loop continues past single-match windows until either a second match
// lands or the largest (366d) window is exhausted; the chosen base of
// 2024-01-01 plus the leap-year window covers `0 H 1 1 *` schedules
// honestly (1 match in 366d → 366d, not 24h).
func TestComputeExpectedIntervalForCronSchedule_YearlyWithFirstWindowMatch(t *testing.T) {
	// `0 0 1 1 *` matches at base (Jan 1 2024 00:00 — i=0) and the next
	// occurrence is Jan 1 2025, which lies at exactly base+366d and is
	// excluded by the `i < windowMinutes` loop boundary. Before the
	// Copilot fix, the 24h-window early-return would have returned 24h.
	// After the fix, the loop continues past every window-with-one-match
	// and returns the 366d window length only at the last step.
	got, err := computeExpectedIntervalForCronSchedule("0 0 1 1 *")
	if err != nil {
		t.Fatalf("computeExpectedIntervalForCronSchedule(yearly): %v", err)
	}
	if got < 30*24*time.Hour {
		t.Fatalf("yearly schedule classified as %s (< 30d) — early-return-on-first-window bug regressed", got)
	}
}

// TestComputeExpectedIntervalForCronSchedule_LeapDay pins the Copilot
// finding on PR #2525 thread @ line 192: `0 0 29 2 *` (Feb 29) must not
// produce a permanent doctor-red on cities whose check window starts
// outside a leap-year window. The leap-year base (2024-01-01) means the
// 366d window includes 2024-02-29, so Feb 29 schedules match once and
// are classified as 366d (single-match-in-largest-window).
func TestComputeExpectedIntervalForCronSchedule_LeapDay(t *testing.T) {
	got, err := computeExpectedIntervalForCronSchedule("0 0 29 2 *")
	if err != nil {
		t.Fatalf("computeExpectedIntervalForCronSchedule(Feb 29): %v — leap-day schedule should not error", err)
	}
	if got != 366*24*time.Hour {
		t.Fatalf("Feb 29 schedule = %s, want 366d (single match in leap-year 366d window)", got)
	}
}

// TestComputeExpectedIntervalForCronSchedule_NoMatchInAYear pins the only
// remaining error path now that coarse schedules widen the scan up to 366
// days: a schedule that cannot match any minute in a year (here, an
// impossible day-of-month) still returns an explicit error so doctor
// surfaces it rather than silently mis-classifying the order.
func TestComputeExpectedIntervalForCronSchedule_NoMatchInAYear(t *testing.T) {
	const impossible = "0 0 31 2 *" // Feb 31 — never matches
	_, err := computeExpectedIntervalForCronSchedule(impossible)
	if err == nil {
		t.Fatalf("computeExpectedIntervalForCronSchedule(%q) returned no error; want diagnostic for unmatched schedule", impossible)
	}
}

func orderFiringTestCity(t *testing.T) (string, *config.City) {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "orders"), 0o755); err != nil {
		t.Fatalf("creating orders dir: %v", err)
	}
	formulasDir := filepath.Join(cityPath, "formulas")
	return cityPath, &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{formulasDir},
		},
	}
}

func writeOrderFiringTestOrder(t *testing.T, cityPath, name, trigger, timing string) {
	t.Helper()
	writeOrderFiringTestOrderInDir(t, filepath.Join(cityPath, "orders"), name, trigger, timing)
}

func writeOrderFiringTestOrderInDir(t *testing.T, orderDir, name, trigger, timing string) {
	t.Helper()
	var body string
	switch trigger {
	case "cron":
		body = `[order]
exec = "true"
trigger = "cron"
schedule = "` + timing + `"
`
	case "cooldown":
		body = `[order]
exec = "true"
trigger = "cooldown"
interval = "` + timing + `"
`
	case "event":
		body = `[order]
exec = "true"
trigger = "event"
on = "` + timing + `"
`
	case "condition":
		body = `[order]
exec = "true"
trigger = "condition"
check = "true"
`
	default:
		body = `[order]
exec = "true"
trigger = "` + trigger + `"
`
	}
	if err := os.WriteFile(filepath.Join(orderDir, name+".toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing order %s: %v", name, err)
	}
}

func writeOrderFiringRawOrder(t *testing.T, cityPath, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cityPath, "orders", name+".toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing order %s: %v", name, err)
	}
}

func writeOrderFiringTestEvents(t *testing.T, cityPath string, evts ...events.Event) {
	t.Helper()
	rec, err := events.NewFileRecorder(filepath.Join(cityPath, ".gc", "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() {
		if err := rec.Close(); err != nil {
			t.Fatalf("closing FileRecorder: %v", err)
		}
	})
	for _, e := range evts {
		rec.Record(e)
	}
}

func runOrderFiringCurrentTest(t *testing.T, cfg *config.City, cityPath string, now time.Time) *CheckResult {
	t.Helper()
	check := NewOrderFiringCurrentCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }
	return check.Run(&CheckContext{CityPath: cityPath})
}

func TestLatestOrderFiredAt_RecentEventSkipsLastRun(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	expected := 4 * time.Hour
	order := orders.Order{Name: "cleanup-cooldown", Trigger: "cooldown"}
	evts := []events.Event{
		{Type: events.OrderFired, Subject: order.ScopedName(), Ts: now.Add(-1 * time.Hour)},
	}

	lastRunCalled := false
	check := &OrderFiringCurrentCheck{
		lastRun: func(orders.Order) (time.Time, error) {
			lastRunCalled = true
			return time.Time{}, fmt.Errorf("lastRun must not be consulted for a recent event")
		},
	}

	got, err := check.latestOrderFiredAt(evts, order, expected, now, time.Time{})
	if err != nil {
		t.Fatalf("latestOrderFiredAt returned error: %v", err)
	}
	if lastRunCalled {
		t.Fatalf("lastRun was consulted for an in-band event; want fast path to skip it")
	}
	if want := now.Add(-1 * time.Hour); !got.Equal(want) {
		t.Fatalf("latest = %v, want %v (event timestamp)", got, want)
	}
}

func TestLatestOrderFiredAt_StaleEventConsultsLastRun(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	expected := 4 * time.Hour
	order := orders.Order{Name: "mol-dog-stale-db", Trigger: "cron"}
	// Event age (13h) exceeds expected*1.5 (6h), so the fast path must not apply.
	staleEvent := now.Add(-13 * time.Hour)
	freshRun := now.Add(-1 * time.Hour)
	evts := []events.Event{
		{Type: events.OrderFired, Subject: order.ScopedName(), Ts: staleEvent},
	}

	lastRunCalled := false
	check := &OrderFiringCurrentCheck{
		lastRun: func(orders.Order) (time.Time, error) {
			lastRunCalled = true
			return freshRun, nil
		},
	}

	got, err := check.latestOrderFiredAt(evts, order, expected, now, time.Time{})
	if err != nil {
		t.Fatalf("latestOrderFiredAt returned error: %v", err)
	}
	if !lastRunCalled {
		t.Fatalf("lastRun was not consulted for a stale event; want order-run history queried")
	}
	if !got.Equal(freshRun) {
		t.Fatalf("latest = %v, want %v (newer order-run history)", got, freshRun)
	}
}

// TestOrderFiringCurrent_StalledLastRun_DegradesInsteadOfBlindingWholeCheck
// used to pin the OUTER whole-check timeout firing whenever a single
// lastRun call stalled under a tight historyTimeout. Superseded by the
// persona-marcus review finding (2026-07-26, ga-17ow3v): a per-order lastRun
// bound alone is not enough, because an extremely tight whole-check budget
// (or several stalled orders summing past a looser one) still tripped Run's
// own outer timeout and discarded every order's already-computed result —
// the exact "blinds the order-staleness gate" symptom in this bead's title,
// just moved from "unbounded event scan" to "unbounded aggregate lastRun
// spend." latestOrderFiredAt now clamps each attempt to the shared deadline
// (see orderFiringDeadlineReserve), so once the budget is already spent
// before the loop even starts, the stalled order degrades to its
// tail-derived classification (annotated as unconfirmed) instead of the
// whole check going blank with a generic "lookup timed out" message.
func TestOrderFiringCurrent_StalledLastRun_DegradesInsteadOfBlindingWholeCheck(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stalled-history", "cron", "0 */4 * * *")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "mol-dog-stalled-history", Ts: now.Add(-13 * time.Hour)},
	)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	lastRunCalled := false
	check := NewOrderFiringCurrentCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }
	// 20ms « the default 1s deadline reserve: the shared budget is already
	// negative before the per-order loop starts, so the attempt must be
	// skipped outright rather than raced against a real timer.
	check.historyTimeout = 20 * time.Millisecond
	check.lastRun = func(orders.Order) (time.Time, error) {
		lastRunCalled = true
		<-release
		return time.Time{}, nil
	}

	result := check.Run(&CheckContext{CityPath: cityPath})
	if lastRunCalled {
		t.Fatalf("lastRun was called; want the already-exhausted shared deadline to skip the attempt outright")
	}
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error (tail-derived classification is genuinely CRITICAL: stale on its own); msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if strings.Contains(result.Message, "order history lookup timed out") {
		t.Fatalf("message = %q, want the whole check to classify from tail data instead of blanking itself with a lookup-timeout message", result.Message)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "CRITICAL: stale") {
		t.Fatalf("details = %v, want the tail-derived stale classification to still surface", result.Details)
	}
	if !strings.Contains(details, "confirmation timed out") {
		t.Fatalf("details = %v, want an unconfirmed-lookup annotation", result.Details)
	}
}

// TestOrderFiringCurrent_MultipleStalledLastRuns_ShareWholeCheckBudget pins
// the core of the aggregate-budget fix (persona-marcus review, 2026-07-26):
// a FIXED per-order lastRun bound does not bound the AGGREGATE — enough
// orders needing the fallback at once can still sum past the whole-check
// budget and trip Run's outer timeout, discarding every order's result
// (including ones that already resolved cleanly from the event tail). Three
// orders here each need the fallback and would block if actually attempted;
// the shared deadline must let the first two spend their full per-order
// bound, then skip the third's attempt outright once the budget is gone —
// and a fourth, healthy order must resolve from the tail completely
// unaffected, exactly as in the single-stall test above.
func TestOrderFiringCurrent_MultipleStalledLastRuns_ShareWholeCheckBudget(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "stall-a", "cooldown", "1h")
	writeOrderFiringTestOrder(t, cityPath, "stall-b", "cooldown", "1h")
	writeOrderFiringTestOrder(t, cityPath, "stall-c", "cooldown", "1h")
	writeOrderFiringTestOrder(t, cityPath, "healthy-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-5 * time.Minute)},
		events.Event{Type: events.OrderFired, Subject: "healthy-cooldown", Ts: now.Add(-10 * time.Minute)},
	)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var mu sync.Mutex
	attempted := 0
	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(func(orders.Order) (time.Time, error) {
		mu.Lock()
		attempted++
		mu.Unlock()
		<-release // simulates ga-t2brh8 Dolt contention
		return time.Time{}, nil
	}))
	check.clock = func() time.Time { return now }
	check.lastRunTimeout = 100 * time.Millisecond
	check.deadlineReserve = 100 * time.Millisecond
	// 2 full 100ms attempts (200ms) + the 100ms reserve exhausts this budget
	// before a 3rd attempt can start; ~100ms of slack over the 2-attempt
	// case keeps this robust against scheduling jitter without making the
	// test slow.
	check.historyTimeout = 300 * time.Millisecond

	result := check.Run(&CheckContext{CityPath: cityPath})

	if strings.Contains(result.Message, "order history lookup timed out") {
		t.Fatalf("whole check timed out and discarded every order's result — the exact aggregate-budget bug this test pins; details = %v", result.Details)
	}
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning (degraded confirmations stay visible but must not escalate past warning on their own); msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	for _, name := range []string{"stall-a", "stall-b", "stall-c"} {
		if !strings.Contains(details, name) {
			t.Fatalf("details = %v, want %s to still appear (nothing discarded)", result.Details, name)
		}
	}
	if !strings.Contains(details, "healthy-cooldown: last fired 10m ago") {
		t.Fatalf("details = %v, want healthy-cooldown resolved normally, unaffected by its siblings' stalled lookups", result.Details)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempted != 2 {
		t.Fatalf("lastRun attempted %d times across stall-a/b/c, want exactly 2 (the shared budget covers 2 full per-order attempts before the 3rd must skip outright); details = %v", attempted, result.Details)
	}
}

// TestLatestOrderFiredAt_LastRunTimeout_ReturnsTailDataWithSentinelError pins
// the ga-17ow3v follow-up: on a live city, c.lastRun (the Dolt/beads-store
// fallback) can itself stall for well over a minute under known Dolt
// contention (ga-t2brh8) even though the query it issues is already bounded
// (Limit:1). A single stalled call must not block latestOrderFiredAt
// indefinitely; it should return the tail-derived value plus a distinguishable
// sentinel error so the caller can degrade gracefully instead of discarding
// this (and every other) order's result.
func TestLatestOrderFiredAt_LastRunTimeout_ReturnsTailDataWithSentinelError(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	expected := 4 * time.Hour
	order := orders.Order{Name: "mol-dog-stale-db", Trigger: "cron"}
	// Event age (13h) exceeds expected*1.5 (6h), so the fast path must not
	// apply and the (stalled) fallback must be consulted.
	staleEvent := now.Add(-13 * time.Hour)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	check := &OrderFiringCurrentCheck{
		lastRunTimeout: 20 * time.Millisecond,
		lastRun: func(orders.Order) (time.Time, error) {
			<-release // simulates a stalled Dolt/beads round trip (ga-t2brh8)
			return now, nil
		},
	}

	got, err := check.latestOrderFiredAt(
		[]events.Event{{Type: events.OrderFired, Subject: order.ScopedName(), Ts: staleEvent}},
		order, expected, now, time.Time{},
	)
	if !errors.Is(err, errOrderHistoryLookupTimedOut) {
		t.Fatalf("err = %v, want errOrderHistoryLookupTimedOut", err)
	}
	if !got.Equal(staleEvent) {
		t.Fatalf("latest = %v, want %v (tail-derived fallback value)", got, staleEvent)
	}
}

// TestOrderFiringCurrent_LastRunTimeout_DegradesOneOrder_OthersUnaffected pins
// the actual value of the per-order bound: before this fix, ANY one order's
// stalled c.lastRun call blew the whole-check timeout and discarded every
// other order's already-computed result (the exact "blinds the order-staleness
// gate" symptom in ga-17ow3v's title). new-order has no tail history at all
// (forcing the fallback attempt) while healthy-cooldown resolves entirely from
// the in-memory tail and must never touch the stalled mock.
func TestOrderFiringCurrent_LastRunTimeout_DegradesOneOrder_OthersUnaffected(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "new-order", "cooldown", "1h")
	writeOrderFiringTestOrder(t, cityPath, "healthy-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-5 * time.Minute)},
		events.Event{Type: events.OrderFired, Subject: "healthy-cooldown", Ts: now.Add(-10 * time.Minute)},
	)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(func(orders.Order) (time.Time, error) {
		<-release // simulates ga-t2brh8 Dolt contention
		return time.Time{}, nil
	}))
	check.clock = func() time.Time { return now }
	check.lastRunTimeout = 20 * time.Millisecond

	result := check.Run(&CheckContext{CityPath: cityPath})

	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "confirmation timed out") {
		t.Fatalf("details = %v, want a confirmation-timed-out note for new-order", result.Details)
	}
	if !strings.Contains(details, "healthy-cooldown: last fired 10m ago") {
		t.Fatalf("details = %v, want healthy-cooldown resolved normally, unaffected by new-order's stalled lookup", result.Details)
	}
	// Tail data alone says new-order is still within its first cycle (clean
	// OK), but confirmation timed out — must not be silently reported clean.
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning (degraded confirmation must stay visible, not silently OK); msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
}

// TestOrderFiringCurrent_FindsRecentFiring_AmongLargeNoisyHistory pins the
// ga-17ow3v fix: on a live city, events.jsonl plus its rotated archives can
// carry 40k+ order.fired lines and exceed 350MB, and the old unconditional
// events.ReadFiltered scan of the whole history (twice — once per event
// type) reliably blew the 15s budget above. The fix switched both reads to
// events.ReadFilteredTail, bounded to orderFiringEventTailLimit for
// order.fired and to 1 for the single newest controller.started. This test
// writes noisy history well past what a naive "just read the last line"
// implementation would need, with the target order's real firing placed
// before a run of newer, irrelevant events, to confirm the bounded tail
// read still resolves the correct per-order last-fired time rather than
// picking up the trailing noise or missing the match entirely.
func TestOrderFiringCurrent_FindsRecentFiring_AmongLargeNoisyHistory(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "target-order", "cooldown", "1h")

	evts := []events.Event{
		{Type: events.ControllerStarted, Ts: now.Add(-48 * time.Hour)},
	}
	// Older noise from unrelated orders, oldest first.
	for i := 500; i > 20; i-- {
		evts = append(evts, events.Event{
			Type:    events.OrderFired,
			Subject: fmt.Sprintf("noise-order-%d", i),
			Ts:      now.Add(-time.Duration(i) * time.Minute),
		})
	}
	// The real, recent firing for the order under test — deliberately NOT
	// the last line written, so a correct implementation must actually
	// match on Subject within the tail window rather than assume position.
	evts = append(evts, events.Event{
		Type:    events.OrderFired,
		Subject: "target-order",
		Ts:      now.Add(-10 * time.Minute),
	})
	// More recent noise from unrelated orders after the real firing.
	for i := 19; i > 0; i-- {
		evts = append(evts, events.Event{
			Type:    events.OrderFired,
			Subject: fmt.Sprintf("noise-order-%d", i),
			Ts:      now.Add(-time.Duration(i) * time.Minute),
		})
	}
	writeOrderFiringTestEvents(t, cityPath, evts...)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "target-order: last fired 10m ago") {
		t.Fatalf("details = %v, want target-order last-fired-10m-ago entry", result.Details)
	}
}
