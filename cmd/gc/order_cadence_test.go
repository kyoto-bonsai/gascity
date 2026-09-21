package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/orders"
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
