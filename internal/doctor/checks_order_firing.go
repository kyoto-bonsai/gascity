package doctor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orderdiscovery"
	"github.com/gastownhall/gascity/internal/orders"
)

const (
	orderFiringCurrentName    = "order-firing-current"
	orderFiringInspectHintFmt = "Inspect with: gc order check && gc order history %s"
	orderFiringHistoryTimeout = 15 * time.Second
	// orderFiringEventTailLimit bounds the order.fired scan to the most
	// recent N matching events instead of the full history (ga-17ow3v: on a
	// live city this file plus its rotated archives can exceed 350MB and
	// 40k+ matching lines, which reliably blew the 15s budget below). The
	// tail read only ever opens the active file, never the gzip archives.
	// Any order whose true last-fired time falls outside this window still
	// gets a correct answer via the c.lastRun fallback in latestOrderFiredAt
	// — this limit only bounds the fast path, not correctness.
	orderFiringEventTailLimit = 20000
	// orderFiringLastRunTimeout bounds each PER-ORDER call to c.lastRun in
	// latestOrderFiredAt (ga-17ow3v follow-up: the event-tail fix above
	// shipped and verified bounded, but the check still timed out on this
	// machine). Root cause is upstream, not this check: c.lastRun resolves
	// through orders.LastRunAcross -> Store.LastRun, an already well-formed
	// Limit:1/sorted/label-filtered beads query — the slowness is this
	// city's Dolt sql-server saturating under normal fleet concurrency
	// (tracked separately as ga-t2brh8, "schema migration lock unavailable:
	// timeout"; closed 2026-07-26 with adaptive backoff on a DIFFERENT
	// codepath — gc's own session/hook work-query polling — so contention
	// here is mitigated by that fix, not eliminated by it). Direct
	// measurement on this machine: a single `gc order history <name>` call
	// for the one order that actually needed this fallback took 92s; a
	// `gc order check` pass over all ~20 monitored orders took 2m2s. No fixed
	// WHOLE-check timeout below several minutes could make the old design
	// (any one order's lastRun call blocks the entire check, discarding every
	// other order's already-computed result) reliably pass here. Bounding the
	// call per-order instead means the orders that resolve from the
	// in-memory event tail are unaffected, and only the order(s) actually
	// needing the fallback degrade to tail-only data (see
	// errOrderHistoryLookupTimedOut) rather than blinding the whole check.
	//
	// A FIXED per-order bound alone does not bound the AGGREGATE, though:
	// enough orders needing the fallback at once — this city monitors ~27,
	// several on multi-hour cooldowns liable to fall outside the event tail
	// together — can still sum past orderFiringHistoryTimeout and trip the
	// whole-check timeout in Run, reintroducing the exact symptom this bound
	// was meant to fix (persona-marcus review on this bead, 2026-07-26).
	// latestOrderFiredAt now additionally clamps each attempt to whatever
	// remains of the whole-check deadline (see orderFiringDeadlineReserve),
	// so no combination of stalled orders can exceed it: once the shared
	// budget is spent, remaining orders degrade immediately without even
	// attempting the call.
	orderFiringLastRunTimeout = 5 * time.Second
	// orderFiringDeadlineReserve is held back from the whole-check deadline
	// when computing how much of the remaining budget a per-order lastRun
	// attempt may use (see latestOrderFiredAt). It covers Run's own
	// goroutine-dispatch and channel-select overhead so a fully-consumed
	// per-order budget can't itself tip the whole check past
	// orderFiringHistoryTimeout. Measured against real wall-clock time
	// (time.Now/time.Until), never the mockable clock field — the whole-check
	// budget is a real-process constraint, independent of whatever simulated
	// "now" business-logic classification uses.
	orderFiringDeadlineReserve = 1 * time.Second
)

// errOrderHistoryLookupTimedOut signals that a single order's Dolt/beads
// fallback lookup (c.lastRun) did not return within the check's configured
// last-run timeout. Callers should treat this as a soft degradation, not a
// hard failure: the accompanying time.Time is still the best available
// (tail-derived) answer, and ga-t2brh8 — not this check — owns fixing the
// underlying contention.
var errOrderHistoryLookupTimedOut = errors.New("order-run history lookup timed out")

// OrderFiringCurrentLastRunFunc reports the newest persisted run time for an order.
type OrderFiringCurrentLastRunFunc func(order orders.Order) (time.Time, error)

// OrderFiringCurrentOption configures the scheduled-order freshness check.
type OrderFiringCurrentOption func(*OrderFiringCurrentCheck)

// WithOrderFiringCurrentLastRunFunc lets callers provide the same order-run
// history source used by `gc order history` so doctor can classify manual runs.
func WithOrderFiringCurrentLastRunFunc(fn OrderFiringCurrentLastRunFunc) OrderFiringCurrentOption {
	return func(c *OrderFiringCurrentCheck) {
		c.lastRun = fn
	}
}

// OrderFiringCurrentCheck reports scheduled orders whose last firing is stale.
type OrderFiringCurrentCheck struct {
	cfg             *config.City
	cityPath        string
	clock           func() time.Time
	lastRun         OrderFiringCurrentLastRunFunc
	historyTimeout  time.Duration
	lastRunTimeout  time.Duration
	deadlineReserve time.Duration
}

// NewOrderFiringCurrentCheck creates a check for cron and cooldown order freshness.
func NewOrderFiringCurrentCheck(cfg *config.City, cityPath string, opts ...OrderFiringCurrentOption) *OrderFiringCurrentCheck {
	check := &OrderFiringCurrentCheck{
		cfg:             cfg,
		cityPath:        cityPath,
		clock:           time.Now,
		historyTimeout:  orderFiringHistoryTimeout,
		lastRunTimeout:  orderFiringLastRunTimeout,
		deadlineReserve: orderFiringDeadlineReserve,
	}
	for _, opt := range opts {
		opt(check)
	}
	return check
}

// resolvedLastRunTimeout returns the configured per-order lastRun timeout,
// falling back to orderFiringLastRunTimeout for checks constructed via a bare
// struct literal (as several tests do) rather than NewOrderFiringCurrentCheck.
func (c *OrderFiringCurrentCheck) resolvedLastRunTimeout() time.Duration {
	if c.lastRunTimeout > 0 {
		return c.lastRunTimeout
	}
	return orderFiringLastRunTimeout
}

// resolvedDeadlineReserve returns the configured deadline reserve, falling
// back to orderFiringDeadlineReserve for checks constructed via a bare struct
// literal rather than NewOrderFiringCurrentCheck.
func (c *OrderFiringCurrentCheck) resolvedDeadlineReserve() time.Duration {
	if c.deadlineReserve > 0 {
		return c.deadlineReserve
	}
	return orderFiringDeadlineReserve
}

// Name returns the check identifier shown by gc doctor.
func (c *OrderFiringCurrentCheck) Name() string { return orderFiringCurrentName }

// CanFix reports whether the check can repair stale order firing state.
func (c *OrderFiringCurrentCheck) CanFix() bool { return false }

// Fix is a no-op because stale order remediation depends on the root cause.
func (c *OrderFiringCurrentCheck) Fix(_ *CheckContext) error { return nil }

// Run compares each cron or cooldown order with its order.fired history.
func (c *OrderFiringCurrentCheck) Run(ctx *CheckContext) *CheckResult {
	timeout := c.historyTimeout
	if timeout <= 0 {
		timeout = orderFiringHistoryTimeout
	}
	// Shared deadline for the whole check, threaded down to each per-order
	// lastRun fallback attempt so no combination of stalled orders can sum
	// past this budget (see orderFiringDeadlineReserve). Measured against
	// real wall-clock time, not c.clock — the mockable clock field is for
	// business-logic "now" (order-age classification) and is frozen to a
	// fixed instant in most tests, which would never let a real-time budget
	// shrink; the whole-check timeout below is likewise always real-time.
	deadline := time.Now().Add(timeout)

	// The order-history resolver opens the beads/Dolt store and does not accept
	// a context. Keep that potentially blocking I/O from wedging the complete
	// doctor run; the gc process exits after printing this failed check.
	results := make(chan *CheckResult, 1)
	go func() {
		results <- c.run(ctx, deadline)
	}()

	select {
	case result := <-results:
		return result
	case <-time.After(timeout):
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("order history lookup timed out after %s", timeout),
			FixHint: "check beads/Dolt connectivity, then rerun gc doctor",
		}
	}
}

func (c *OrderFiringCurrentCheck) run(ctx *CheckContext, deadline time.Time) *CheckResult {
	result := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		result.Status = StatusOK
		result.Message = "no city config loaded"
		return result
	}

	cityPath := c.cityPath
	if cityPath == "" && ctx != nil {
		cityPath = ctx.CityPath
	}
	if cityPath == "" {
		result.Status = StatusError
		result.Message = "city path unavailable"
		return result
	}

	allOrders, err := scanOrderFiringCurrentOrders(cityPath, c.cfg)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("scan orders: %v", err)
		return result
	}

	eventPath := filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl")
	firedEvents, err := events.ReadFilteredTail(context.Background(), eventPath, events.Filter{Type: events.OrderFired}, orderFiringEventTailLimit)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("read order firing events: %v", err)
		return result
	}
	startedAt, err := latestControllerStartedAt(eventPath)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("read controller start events: %v", err)
		return result
	}

	now := c.clock()
	if now.IsZero() {
		now = time.Now()
	}
	cronIntervals := map[string]time.Duration{}
	worst := StatusOK
	monitored := 0
	var firstNonOK string
	// Track severity contributions across error-level entries. Warnings should
	// stay visible without converting an advisory error into a blocking gate.
	var blockingErrors, advisoryErrors int
	suspendedRigs := orderFiringCurrentSuspendedRigs(c.cfg)
	zeroMinPools := orderFiringCurrentZeroMinPools(c.cfg)

	for _, order := range allOrders {
		if order.Trigger != "cron" && order.Trigger != "cooldown" {
			continue
		}
		if orderFiringCurrentOrderSuspended(suspendedRigs, order) {
			continue
		}
		monitored++
		expected, err := expectedIntervalForOrder(order, cronIntervals)
		if err != nil {
			worst = worseStatus(worst, StatusError)
			result.Details = append(result.Details, fmt.Sprintf("%s: cannot compute expected interval: %v", orderDisplayName(order), err))
			if firstNonOK == "" {
				firstNonOK = orderHistoryHintTarget(order)
			}
			blockingErrors++
			continue
		}
		lastFired, err := c.latestOrderFiredAt(firedEvents, order, expected, now, deadline)
		degradedLookup := errors.Is(err, errOrderHistoryLookupTimedOut)
		if err != nil && !degradedLookup {
			worst = worseStatus(worst, StatusError)
			result.Details = append(result.Details, fmt.Sprintf("%s: cannot read order history: %v", orderDisplayName(order), err))
			if firstNonOK == "" {
				firstNonOK = orderHistoryHintTarget(order)
			}
			blockingErrors++
			continue
		}
		poolParked := orderFiringCurrentOrderPoolParked(zeroMinPools, order)
		status, severity, detail := classifyOrderFiring(order, now, expected, lastFired, startedAt, poolParked)
		if degradedLookup {
			// lastFired is still the tail-derived value classifyOrderFiring
			// just used (see latestOrderFiredAt) — only the Dolt/beads
			// confirmation was skipped. Never silently report clean when we
			// could not actually confirm it (see resolvedLastRunTimeout).
			detail = fmt.Sprintf("%s — order-run history confirmation timed out after %s (see ga-t2brh8)", detail, c.resolvedLastRunTimeout())
			if status == StatusOK {
				status = StatusWarning
			}
		}
		worst = worseStatus(worst, status)
		result.Details = append(result.Details, detail)
		if status != StatusOK {
			if firstNonOK == "" {
				firstNonOK = orderHistoryHintTarget(order)
			}
			if status == StatusError {
				if severity == SeverityBlocking {
					blockingErrors++
				} else {
					advisoryErrors++
				}
			}
		}
	}

	if monitored == 0 {
		result.Status = StatusOK
		result.Message = "no cron or cooldown orders"
		return result
	}

	result.Status = worst
	switch worst {
	case StatusOK:
		result.Message = "all scheduled orders are current"
	case StatusWarning:
		result.Message = "scheduled orders are overdue"
	case StatusError:
		result.Message = "scheduled orders are stale"
	}
	if blockingErrors == 0 && advisoryErrors > 0 {
		result.Severity = SeverityAdvisory
	}
	if firstNonOK != "" {
		result.FixHint = fmt.Sprintf(orderFiringInspectHintFmt, firstNonOK)
	}
	return result
}

func scanOrderFiringCurrentOrders(cityPath string, cfg *config.City) ([]orders.Order, error) {
	scanCfg := orderFiringCurrentScanConfig(cfg)
	scanCfg = orderFiringCurrentPruneSuspendedOnlyWildcardOverrides(cityPath, cfg, scanCfg)
	allOrders, err := orderdiscovery.ScanAll(cityPath, scanCfg, orderFiringCurrentScanOptions(cityPath))
	if err != nil {
		return nil, err
	}
	return orders.FilterEnabled(allOrders), nil
}

func orderFiringCurrentScanOptions(cityPath string) orderdiscovery.ScanOptions {
	return orderdiscovery.ScanOptions{
		OnValidateError: func(orderName string, err error) error {
			log.Printf("gc doctor: skipping invalid order %s for %s: %v", orderName, cityPath, err)
			return nil
		},
		ValidateOrder: orders.ValidateExecEnvOverrides,
	}
}

func orderFiringCurrentScanConfig(cfg *config.City) *config.City {
	if cfg == nil {
		return nil
	}
	suspended := orderFiringCurrentSuspendedRigs(cfg)
	if len(suspended) == 0 {
		return cfg
	}
	clone := *cfg
	if len(cfg.FormulaLayers.Rigs) > 0 {
		clone.FormulaLayers.Rigs = make(map[string][]string, len(cfg.FormulaLayers.Rigs))
		for rigName, layers := range cfg.FormulaLayers.Rigs {
			if suspended[rigName] {
				continue
			}
			clone.FormulaLayers.Rigs[rigName] = layers
		}
	}
	if len(cfg.RigPackDirs) > 0 {
		clone.RigPackDirs = make(map[string][]string, len(cfg.RigPackDirs))
		for rigName, dirs := range cfg.RigPackDirs {
			if suspended[rigName] {
				continue
			}
			clone.RigPackDirs[rigName] = dirs
		}
	}
	if len(cfg.Orders.Overrides) > 0 {
		clone.Orders.Overrides = make([]config.OrderOverride, 0, len(cfg.Orders.Overrides))
		for _, override := range cfg.Orders.Overrides {
			if suspended[strings.TrimSpace(override.Rig)] {
				continue
			}
			clone.Orders.Overrides = append(clone.Orders.Overrides, override)
		}
	}
	return &clone
}

func orderFiringCurrentPruneSuspendedOnlyWildcardOverrides(cityPath string, originalCfg, scanCfg *config.City) *config.City {
	if originalCfg == nil || scanCfg == nil || len(scanCfg.Orders.Overrides) == 0 {
		return scanCfg
	}
	suspended := orderFiringCurrentSuspendedRigs(originalCfg)
	if len(suspended) == 0 {
		return scanCfg
	}
	activeOrders, err := orderFiringCurrentScanWithoutOverrides(cityPath, scanCfg)
	if err != nil {
		return scanCfg
	}
	allOrders, err := orderFiringCurrentScanWithoutOverrides(cityPath, originalCfg)
	if err != nil {
		return scanCfg
	}
	activeNames := map[string]bool{}
	for _, order := range activeOrders {
		activeNames[order.Name] = true
	}
	suspendedOnlyNames := map[string]bool{}
	for _, order := range allOrders {
		if order.Name == "" || !suspended[order.Rig] || activeNames[order.Name] {
			continue
		}
		suspendedOnlyNames[order.Name] = true
	}
	if len(suspendedOnlyNames) == 0 {
		return scanCfg
	}
	clone := *scanCfg
	clone.Orders.Overrides = make([]config.OrderOverride, 0, len(scanCfg.Orders.Overrides))
	for _, override := range scanCfg.Orders.Overrides {
		if strings.TrimSpace(override.Rig) == orders.RigWildcard && suspendedOnlyNames[strings.TrimSpace(override.Name)] {
			continue
		}
		clone.Orders.Overrides = append(clone.Orders.Overrides, override)
	}
	return &clone
}

func orderFiringCurrentScanWithoutOverrides(cityPath string, cfg *config.City) ([]orders.Order, error) {
	if cfg == nil {
		return orderdiscovery.ScanAll(cityPath, nil, orderFiringCurrentScanOptions(cityPath))
	}
	clone := *cfg
	clone.Orders.Overrides = nil
	return orderdiscovery.ScanAll(cityPath, &clone, orderFiringCurrentScanOptions(cityPath))
}

func orderFiringCurrentSuspendedRigs(cfg *config.City) map[string]bool {
	out := make(map[string]bool)
	if cfg == nil {
		return out
	}
	for _, rig := range cfg.Rigs {
		if rig.Suspended && strings.TrimSpace(rig.Name) != "" {
			out[rig.Name] = true
		}
	}
	return out
}

func orderFiringCurrentOrderSuspended(suspended map[string]bool, order orders.Order) bool {
	if suspended[order.Rig] {
		return true
	}
	// Defensive support for legacy qualified pool values. Bare pool names parse
	// with an empty rig and intentionally do not imply suspension by themselves.
	if rigName, _ := config.ParseQualifiedName(order.Pool); rigName != "" && suspended[rigName] {
		return true
	}
	return false
}

// orderFiringCurrentZeroMinPools returns the set of agent identities
// (both QualifiedName and bare Name, to tolerate however order.Pool happens
// to be spelled) whose min_active_sessions resolves to zero — i.e. pools
// deliberately scaled to no standing instances rather than pools that
// should be running and aren't.
func orderFiringCurrentZeroMinPools(cfg *config.City) map[string]bool {
	out := make(map[string]bool)
	if cfg == nil {
		return out
	}
	for _, a := range cfg.Agents {
		if a.EffectiveMinActiveSessions() > 0 {
			continue
		}
		if qn := a.QualifiedName(); qn != "" {
			out[qn] = true
		}
		if a.Name != "" {
			out[a.Name] = true
		}
	}
	return out
}

// orderFiringCurrentOrderPoolParked reports whether order's backing pool is a
// deliberately-scaled-to-zero pool (min_active_sessions=0): a cron/cooldown
// order routed there has no standing agent to pick it up, so its staleness
// reflects an intentional scaling policy rather than a detection-worthy
// outage (ga-gfdfdc). Matches order.Pool the same tolerant way
// orderFiringCurrentOrderSuspended matches order.Rig: exact string first,
// then its unqualified name via ParseQualifiedName.
func orderFiringCurrentOrderPoolParked(zeroMinPools map[string]bool, order orders.Order) bool {
	pool := strings.TrimSpace(order.Pool)
	if pool == "" {
		return false
	}
	if zeroMinPools[pool] {
		return true
	}
	if _, name := config.ParseQualifiedName(pool); name != "" && zeroMinPools[name] {
		return true
	}
	return false
}

func expectedIntervalForOrder(order orders.Order, cronCache map[string]time.Duration) (time.Duration, error) {
	switch order.Trigger {
	case "cooldown":
		interval, err := time.ParseDuration(order.Interval)
		if err != nil {
			return 0, fmt.Errorf("parse cooldown interval %q: %w", order.Interval, err)
		}
		if interval <= 0 {
			return 0, fmt.Errorf("cooldown interval %q must be positive", order.Interval)
		}
		return interval, nil
	case "cron":
		if cached, ok := cronCache[order.Schedule]; ok {
			return cached, nil
		}
		interval, err := computeExpectedIntervalForCronSchedule(order.Schedule)
		if err != nil {
			return 0, err
		}
		cronCache[order.Schedule] = interval
		return interval, nil
	default:
		return 0, fmt.Errorf("unsupported trigger %q", order.Trigger)
	}
}

func computeExpectedIntervalForCronSchedule(schedule string) (time.Duration, error) {
	fields := strings.Fields(schedule)
	if len(fields) != 5 {
		return 0, fmt.Errorf("invalid cron schedule: want 5 fields, got %d", len(fields))
	}

	// Scan minute-by-minute from a fixed base so the result is deterministic
	// and independent of when the check runs. Widen the scan progressively so
	// weekly, monthly, and yearly schedules are computed honestly instead of
	// erroring out: the typical 24h window has zero matches for any schedule
	// coarser than daily (#2499). The 24h fast-path stays cheap for the
	// common case; coarser schedules pay the larger scan once per unique
	// schedule (results are cached at the caller).
	//
	// Base is the start of a leap year so the 366d window can include a
	// Feb 29 occurrence — `0 0 29 2 *` (leap-day schedules) would otherwise
	// produce a permanent doctor-red on cities whose check started outside
	// a leap-year window (Copilot review on #2525).
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	windowsMinutes := []int{
		1440,       // 24h — covers sub-daily and daily schedules
		7 * 1440,   // 7d  — covers weekly and weekday-set schedules
		31 * 1440,  // 31d — covers monthly schedules (longest month)
		366 * 1440, // 366d — covers yearly + leap-year (Feb 29) schedules
	}
	lastWindowIndex := len(windowsMinutes) - 1
	for windowIndex, windowMinutes := range windowsMinutes {
		matches := make([]time.Time, 0, 16)
		for i := 0; i < windowMinutes; i++ {
			ts := base.Add(time.Duration(i) * time.Minute)
			matched, err := cronScheduleMatchesAt(fields, ts)
			if err != nil {
				return 0, err
			}
			if matched {
				matches = append(matches, ts)
			}
		}
		if len(matches) == 0 {
			continue
		}
		window := time.Duration(windowMinutes) * time.Minute
		if len(matches) == 1 {
			// Don't fix the interval on the first window that happens to
			// catch one match: a yearly schedule whose firing minute
			// coincidentally falls inside the 24h or 7d window (e.g.
			// `0 0 12 5 *` from a base near May 5) would otherwise be
			// mis-classified as sub-daily. Keep widening until either a
			// second match lands (use the real minGap) or we exhaust the
			// horizon — only then is the window length a defensible
			// conservative interval (Copilot review on #2525).
			if windowIndex < lastWindowIndex {
				continue
			}
			return window, nil
		}
		minGap := window
		for i := 1; i < len(matches); i++ {
			gap := matches[i].Sub(matches[i-1])
			if gap < minGap {
				minGap = gap
			}
		}
		// Do not include a wrap-around gap (matches[0]+window - matches[last]).
		// It is only meaningful when the schedule's natural period divides the
		// window evenly, and produces wrong results for schedules whose period
		// does not — e.g. a weekly schedule in the 31d window would report a
		// bogus 3d "wrap" from Mon to Mon-of-next-month-mod-31d, drowning out
		// the real 7d gap from the loop above.
		return minGap, nil
	}
	return 0, fmt.Errorf("cron schedule %q has no firing minutes in a 366-day window", schedule)
}

func cronScheduleMatchesAt(fields []string, ts time.Time) (bool, error) {
	specs := []struct {
		name     string
		field    string
		value    int
		min, max int
	}{
		{name: "minute", field: fields[0], value: ts.Minute(), min: 0, max: 59},
		{name: "hour", field: fields[1], value: ts.Hour(), min: 0, max: 23},
		{name: "day-of-month", field: fields[2], value: ts.Day(), min: 1, max: 31},
		{name: "month", field: fields[3], value: int(ts.Month()), min: 1, max: 12},
		{name: "day-of-week", field: fields[4], value: int(ts.Weekday()), min: 0, max: 6},
	}
	for _, spec := range specs {
		matched, err := cronFieldMatchesForDoctor(spec.field, spec.value, spec.min, spec.max)
		if err != nil {
			return false, fmt.Errorf("invalid cron schedule: cannot parse %s field %q", spec.name, spec.field)
		}
		if !matched {
			return false, nil
		}
	}
	return true, nil
}

func cronFieldMatchesForDoctor(field string, value, lowerBound, upperBound int) (bool, error) {
	if strings.TrimSpace(field) == "" {
		return false, fmt.Errorf("empty field")
	}
	for _, rawPart := range strings.Split(field, ",") {
		part := strings.TrimSpace(rawPart)
		matched, err := cronPartMatchesForDoctor(part, value, lowerBound, upperBound)
		if err != nil {
			return false, err
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

func cronPartMatchesForDoctor(part string, value, lowerBound, upperBound int) (bool, error) {
	if part == "" {
		return false, fmt.Errorf("empty part")
	}
	rangePart, stepPart, hasStep := strings.Cut(part, "/")
	step := 1
	if hasStep {
		parsed, err := strconv.Atoi(strings.TrimSpace(stepPart))
		if err != nil || parsed <= 0 {
			return false, fmt.Errorf("invalid step")
		}
		step = parsed
	}

	lo, hi, err := cronRangeForDoctor(strings.TrimSpace(rangePart), lowerBound, upperBound)
	if err != nil {
		return false, err
	}
	if value < lo || value > hi {
		return false, nil
	}
	return (value-lo)%step == 0, nil
}

func cronRangeForDoctor(rangePart string, lowerBound, upperBound int) (int, int, error) {
	switch {
	case rangePart == "*":
		return lowerBound, upperBound, nil
	case strings.Contains(rangePart, "-"):
		start, end, ok := strings.Cut(rangePart, "-")
		if !ok {
			return 0, 0, fmt.Errorf("invalid range")
		}
		lo, err := strconv.Atoi(strings.TrimSpace(start))
		if err != nil {
			return 0, 0, err
		}
		hi, err := strconv.Atoi(strings.TrimSpace(end))
		if err != nil {
			return 0, 0, err
		}
		if lo < lowerBound || hi > upperBound || lo > hi {
			return 0, 0, fmt.Errorf("range out of bounds")
		}
		return lo, hi, nil
	default:
		value, err := strconv.Atoi(rangePart)
		if err != nil {
			return 0, 0, err
		}
		if value < lowerBound || value > upperBound {
			return 0, 0, fmt.Errorf("value out of bounds")
		}
		return value, value, nil
	}
}

func latestControllerStartedAt(eventPath string) (time.Time, error) {
	// Only the single newest controller.started event matters here, so a
	// tail read of 1 is sufficient — see orderFiringEventTailLimit above for
	// why an unbounded scan of this file is worth avoiding.
	startEvents, err := events.ReadFilteredTail(context.Background(), eventPath, events.Filter{Type: events.ControllerStarted}, 1)
	if err != nil {
		return time.Time{}, err
	}
	var latest time.Time
	for _, event := range startEvents {
		if event.Ts.After(latest) {
			latest = event.Ts
		}
	}
	return latest, nil
}

func (c *OrderFiringCurrentCheck) latestOrderFiredAt(evts []events.Event, order orders.Order, expected time.Duration, now time.Time, deadline time.Time) (time.Time, error) {
	latest := latestOrderFiredAt(evts, order.ScopedName())
	if c.lastRun == nil {
		return latest, nil
	}
	if !latest.IsZero() && now.Sub(latest) < expected+expected/2 {
		return latest, nil
	}

	// c.lastRun opens the beads/Dolt store and does not accept a context (see
	// the comment on Run for why that call is kept off the main goroutine).
	// Bound it per-order here too: under known Dolt contention (ga-t2brh8) a
	// single call can take well over a minute, and letting that block this
	// whole method serially would once again let one stalled order consume
	// every other order's time budget (the original ga-17ow3v symptom, just
	// moved one level down).
	//
	// A fixed per-order bound alone still lets enough stalled orders sum past
	// the whole-check budget (persona-marcus review, 2026-07-26), so clamp
	// this attempt to whatever remains of the shared deadline first. Once
	// that shared budget is gone, degrade immediately without even attempting
	// the call — an attempt that can't complete in time isn't worth the
	// goroutine/scheduling overhead, and every order after the exhaustion
	// point still needs to be classified and reported, not discarded.
	perCallTimeout := c.resolvedLastRunTimeout()
	if !deadline.IsZero() {
		if remaining := time.Until(deadline) - c.resolvedDeadlineReserve(); remaining < perCallTimeout {
			perCallTimeout = remaining
		}
	}
	if perCallTimeout <= 0 {
		return latest, errOrderHistoryLookupTimedOut
	}

	type lastRunResult struct {
		at  time.Time
		err error
	}
	resultCh := make(chan lastRunResult, 1)
	go func() {
		at, err := c.lastRun(order)
		resultCh <- lastRunResult{at, err}
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			return time.Time{}, res.err
		}
		if res.at.After(latest) {
			return res.at, nil
		}
		return latest, nil
	case <-time.After(perCallTimeout):
		return latest, errOrderHistoryLookupTimedOut
	}
}

func latestOrderFiredAt(evts []events.Event, subject string) time.Time {
	var latest time.Time
	for _, event := range evts {
		if event.Subject != subject {
			continue
		}
		if event.Ts.After(latest) {
			latest = event.Ts
		}
	}
	return latest
}

func classifyOrderFiring(order orders.Order, now time.Time, expected time.Duration, lastFired, controllerStarted time.Time, poolParked bool) (CheckStatus, CheckSeverity, string) {
	name := orderDisplayName(order)
	// A pool deliberately scaled to zero standing instances (min_active_sessions=0)
	// has no agent to pick up cron/cooldown work; staleness there is the expected,
	// intended state — not a detection-worthy outage (ga-gfdfdc) — so it stays
	// advisory rather than gating dispatch/exit codes. Status is left at its normal
	// (non-OK) value so the condition is still visible, just not blocking.
	parkedNote := ""
	blockingUnlessParked := SeverityBlocking
	if poolParked {
		parkedNote = " (pool scaled to 0 standing instances: advisory, not blocking)"
		blockingUnlessParked = SeverityAdvisory
	}

	if lastFired.IsZero() {
		if controllerStarted.IsZero() {
			return StatusOK, SeverityBlocking, fmt.Sprintf("%s: never fired (controller start unknown)", name)
		}
		uptime := nonNegativeDuration(now.Sub(controllerStarted))
		if uptime >= expected+expected/2 {
			// Advisory for cron (may be the cron-scheduler bug, ga-97qngx) or for
			// any order whose pool is deliberately parked. Cooldown never-fired/
			// stale paths against a live pool remain blocking — they indicate a
			// real execution gap.
			if order.Trigger == "cron" || poolParked {
				return StatusError, SeverityAdvisory, fmt.Sprintf("%s: never fired since controller start %s ago%s", name, formatOrderFiringDuration(uptime), parkedNote)
			}
			return StatusError, SeverityBlocking, fmt.Sprintf("%s: never fired since controller start %s ago", name, formatOrderFiringDuration(uptime))
		}
		return StatusOK, SeverityBlocking, fmt.Sprintf("%s: never fired (controller running %s, within first cycle)", name, formatOrderFiringDuration(uptime))
	}

	age := nonNegativeDuration(now.Sub(lastFired))
	switch {
	case age >= expected*3:
		return StatusError, blockingUnlessParked, fmt.Sprintf("%s: last fired %s ago, expected every %s (CRITICAL: stale)%s", name, formatOrderFiringDuration(age), formatOrderFiringDuration(expected), parkedNote)
	case age >= expected+expected/2:
		return StatusWarning, blockingUnlessParked, fmt.Sprintf("%s: last fired %s ago, expected every %s (overdue)%s", name, formatOrderFiringDuration(age), formatOrderFiringDuration(expected), parkedNote)
	default:
		return StatusOK, SeverityBlocking, fmt.Sprintf("%s: last fired %s ago, expected every %s", name, formatOrderFiringDuration(age), formatOrderFiringDuration(expected))
	}
}

func orderDisplayName(order orders.Order) string {
	if order.Rig == "" {
		return order.Name
	}
	return order.ScopedName()
}

func orderHistoryHintTarget(order orders.Order) string {
	if order.Rig != "" {
		return fmt.Sprintf("%s --rig %s", order.Name, order.Rig)
	}
	return order.Name
}

func worseStatus(a, b CheckStatus) CheckStatus {
	if b > a {
		return b
	}
	return a
}

func nonNegativeDuration(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

func formatOrderFiringDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	if d == 0 {
		return "0s"
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}
