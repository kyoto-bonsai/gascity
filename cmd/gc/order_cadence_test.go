package main

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestOrderCadenceCapacityTracksConfiguredCooldowns(t *testing.T) {
	set := []orders.Order{
		{Name: "health-one", Trigger: "cooldown", Interval: "30s"},
		{Name: "health-two", Trigger: "cooldown", Interval: "30s"},
		{Name: "health-three", Trigger: "cooldown", Interval: "30s"},
	}
	for i := 0; i < 5; i++ {
		set = append(set, orders.Order{Name: "minute", Trigger: "cooldown", Interval: "1m"})
	}
	got := orderCadenceForSet(set)
	if got < minOrderCadenceInterval || got >= 30*time.Second {
		t.Fatalf("cadence = %s, want bounded interval below 30s for 11 launches/min", got)
	}
	if got := orderCadenceForSet(nil); got != maxOrderCadenceInterval {
		t.Fatalf("empty set cadence = %s, want %s", got, maxOrderCadenceInterval)
	}
}

// A slow session reconciliation must not set the order firing cadence.
func TestOrderCadenceRunsWhileReconciliationIsBusy(t *testing.T) {
	fired := make(chan struct{}, 2)
	orders := &recordingOrderDispatcher{onDispatch: func(context.Context, string, time.Time) {
		fired <- struct{}{}
	}}
	cr := &CityRuntime{od: orders, stderr: io.Discard}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	pulses := make(chan time.Time)
	go cr.runOrderCadence(ctx, "", pulses, done)
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("order cadence did not stop after cancellation")
		}
	}()

	// The reconciler owns a different goroutine and is deliberately parked.
	reconcileDone := make(chan struct{})
	reconcileEntered := make(chan struct{})
	go func() {
		close(reconcileEntered)
		<-reconcileDone
	}()
	defer close(reconcileDone)
	<-reconcileEntered
	for i := 0; i < 2; i++ {
		select {
		case pulses <- time.Now():
		case <-time.After(time.Second):
			t.Fatal("order cadence stopped receiving pulses")
		}
		select {
		case <-fired:
		case <-time.After(time.Second):
			t.Fatal("order cadence did not dispatch during busy reconciliation")
		}
	}
}

func TestOrderCadenceSerializesWithDispatcherReplacement(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	orders := &recordingOrderDispatcher{onDispatch: func(context.Context, string, time.Time) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}}
	cr := &CityRuntime{od: orders, stderr: io.Discard}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	pulses := make(chan time.Time)
	go cr.runOrderCadence(ctx, "", pulses, done)
	select {
	case pulses <- time.Now():
	case <-time.After(time.Second):
		t.Fatal("order cadence stopped receiving pulses")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("order cadence never entered dispatch")
	}
	if cr.orderMu.TryLock() {
		cr.orderMu.Unlock()
		t.Fatal("dispatcher lock was not held during dispatch")
	}
	replaced := make(chan struct{})
	go func() {
		cr.orderMu.Lock()
		cr.od = &recordingOrderDispatcher{}
		cr.orderMu.Unlock()
		close(replaced)
	}()
	close(release)
	select {
	case <-replaced:
	case <-time.After(time.Second):
		t.Fatal("dispatcher replacement remained blocked")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("order cadence did not stop")
	}
}

// A cold-start reconcile can take minutes, so the real run wiring must start
// the cadence loop before entering it rather than only after marking ready.
func TestCityRuntimeOrderCadenceFiresDuringStartupReconcile(t *testing.T) {
	const deadline = 10 * time.Second
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pulses := make(chan time.Time)
	dispatched := make(chan struct{}, 2)
	reconcileEntered := make(chan struct{})
	releaseReconcile := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseReconcile) }) }
	defer release()
	od := &recordingOrderDispatcher{onDispatch: func(context.Context, string, time.Time) {
		dispatched <- struct{}{}
	}}
	cr, err := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			close(reconcileEntered)
			<-releaseReconcile
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:      newDrainOps(sp),
		Rec:       events.Discard,
		OnStarted: cancel,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
	})
	if err != nil {
		t.Fatalf("building the city runtime: %v", err)
	}
	cr.od = od
	cr.orderCadencePulses = pulses
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	done := make(chan struct{})
	go func() {
		cr.run(ctx)
		close(done)
	}()

	select {
	case <-reconcileEntered:
	case <-time.After(deadline):
		t.Fatal("startup reconcile was not entered")
	}
	select {
	case <-dispatched: // the bounded pre-startup pass
	case <-time.After(deadline):
		t.Fatal("startup order pass did not dispatch")
	}
	select {
	case pulses <- time.Now():
	case <-time.After(deadline):
		t.Fatal("cadence loop was not running during startup reconcile")
	}
	select {
	case <-dispatched:
	case <-time.After(deadline):
		t.Fatal("cadence did not dispatch during startup reconcile")
	}
	cancel()
	release()
	select {
	case <-done:
	case <-time.After(deadline):
		t.Fatal("runtime did not stop after startup reconcile was released")
	}
}
