package main

import (
	"context"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/orders"
)

const (
	minOrderCadenceInterval = 15 * time.Second
	maxOrderCadenceInterval = time.Minute
	orderCadenceHeadroom    = 1.25
)

// orderCadenceForSet provides enough four-launch sweeps for configured
// cooldown demand, including headroom for event orders. Its floor prevents a
// tight loop under a large order set; a slow scan coalesces ticker pulses.
func orderCadenceForSet(set []orders.Order) time.Duration {
	var demandPerMinute float64
	shortest := maxOrderCadenceInterval
	for _, order := range set {
		if !order.IsEnabled() || order.Trigger != "cooldown" {
			continue
		}
		period, err := time.ParseDuration(order.Interval)
		if err != nil || period <= 0 {
			continue
		}
		demandPerMinute += float64(time.Minute) / float64(period)
		if period < shortest {
			shortest = period
		}
	}
	if demandPerMinute > 0 {
		capacityInterval := time.Duration(float64(time.Minute) * float64(defaultMaxOrderDispatchesPerTick) / (demandPerMinute * orderCadenceHeadroom))
		if capacityInterval < shortest {
			shortest = capacityInterval
		}
	}
	if shortest < minOrderCadenceInterval {
		return minOrderCadenceInterval
	}
	return shortest
}

func (cr *CityRuntime) currentOrderCadence() time.Duration {
	cr.orderMu.Lock()
	defer cr.orderMu.Unlock()
	return orderCadenceForSet(cr.orderSet)
}

// runOrderCadence is the controller-owned order loop. It never starts a second
// scan while one is in progress: ticker notifications coalesce during a slow
// scan, and orderMu serializes it with patrol dispatch and config reload.
// An injected pulse channel gives coordination tests deterministic ticks.
func (cr *CityRuntime) runOrderCadence(ctx context.Context, cityRoot string, pulses <-chan time.Time, done chan<- struct{}) {
	defer close(done)
	var current time.Duration
	var ticker *time.Ticker
	if pulses == nil {
		current = cr.currentOrderCadence()
		ticker = time.NewTicker(current)
		pulses = ticker.C
		defer ticker.Stop()
	}
	for {
		if ticker != nil {
			if next := cr.currentOrderCadence(); next != current {
				current = next
				ticker.Reset(current)
			}
		}
		select {
		case <-ctx.Done():
			return
		case _, ok := <-pulses:
			if !ok {
				return
			}
			cr.safeTick(func() {
				cr.orderCadenceTick(ctx, cityRoot)
			}, "order-cadence")
		}
	}
}

// orderCadenceTick runs only the loaded order dispatcher. The normal patrol
// still owns order rescan and tracking retention; running those here would
// multiply store maintenance and race its stateful watchdogs.
func (cr *CityRuntime) orderCadenceTick(ctx context.Context, cityRoot string) {
	if ctx.Err() != nil {
		return
	}
	if pressure, ok := currentFSPressureStatus(cr.stderr); ok && pressure.High {
		return
	}
	// A patrol dispatch or reload already owns the dispatcher. Skip this
	// cadence pulse and let the next one retry instead of queuing a second scan.
	if !cr.orderMu.TryLock() {
		return
	}
	defer cr.orderMu.Unlock()
	if ctx.Err() != nil || cr.od == nil {
		return
	}
	started := time.Now()
	cr.od.dispatch(ctx, cityRoot, started)
	if elapsed := time.Since(started); elapsed >= minOrderCadenceInterval && cr.stderr != nil {
		fmt.Fprintf(cr.stderr, "%s: order cadence scan exceeded interval: %s\n", cr.logPrefix, elapsed.Round(time.Millisecond)) //nolint:errcheck // best-effort stderr
	}
}
