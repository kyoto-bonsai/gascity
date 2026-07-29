package api

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// exhaustiveShortTailProvider is a minimal events.Provider + TailProvider +
// ExhaustiveTailProvider double. ListTail returns a fixed, deliberately short
// (< requested fetch) result; List panics if called, so a regression makes
// the test fail loudly rather than silently passing via a slow fallback that
// should have been skipped entirely.
type exhaustiveShortTailProvider struct {
	tail []events.Event
}

func (p *exhaustiveShortTailProvider) Record(events.Event) {}

func (p *exhaustiveShortTailProvider) List(context.Context, events.Filter) ([]events.Event, error) {
	panic("fetchEventPageAscending must not fall back to List for an ExhaustiveTailProvider's " +
		"short ListTail result — this is the ga-96zjze regression: a sparse/absent event type " +
		"used to always pay a second, unconditional full-history scan here")
}

func (p *exhaustiveShortTailProvider) ListTail(context.Context, events.Filter, int) ([]events.Event, error) {
	return p.tail, nil
}

func (p *exhaustiveShortTailProvider) ExhaustiveTail() {}

func (p *exhaustiveShortTailProvider) LatestSeq() (uint64, error) { return uint64(len(p.tail)), nil }

func (p *exhaustiveShortTailProvider) Watch(context.Context, uint64) (events.Watcher, error) {
	return nil, errors.New("not implemented")
}

func (p *exhaustiveShortTailProvider) Close() error { return nil }

var (
	_ events.Provider               = (*exhaustiveShortTailProvider)(nil)
	_ events.TailProvider           = (*exhaustiveShortTailProvider)(nil)
	_ events.ExhaustiveTailProvider = (*exhaustiveShortTailProvider)(nil)
)

// TestFetchEventPageAscendingTrustsExhaustiveShortTail is the direct
// regression test for ga-96zjze: a provider whose ListTail comes up short of
// the requested fetch count, but which promises (via ExhaustiveTailProvider)
// that the short result already reflects the complete retained history, must
// not trigger the redundant, unconditional full-history fallback scan.
// Before this fix, ANY short ListTail result fell through to
// listWithInFlight unconditionally — the mechanism that made a sparse or
// wholly absent event type (convoy.closed; enforcement.violation with zero
// occurrences) always pay a full, unbounded archive scan, regardless of what
// ListTail itself had already determined.
func TestFetchEventPageAscendingTrustsExhaustiveShortTail(t *testing.T) {
	want := []events.Event{
		{Seq: 5, Type: "convoy.closed"},
		{Seq: 9, Type: "convoy.closed"},
	}
	p := &exhaustiveShortTailProvider{tail: want}

	// limit=1000 -> fetch=1001; ListTail returns only 2, far short of fetch.
	// This must not panic (i.e. must not call List) and must trust the 2 as
	// final rather than treating "short" as ambiguous.
	got, scanned, err := fetchEventPageAscending(context.Background(), p, events.Filter{Type: "convoy.closed"}, 1000)
	if err != nil {
		t.Fatalf("fetchEventPageAscending: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if scanned != len(want) {
		t.Fatalf("scanned = %d, want %d (exact match count, not the requested limit)", scanned, len(want))
	}
}

// nonExhaustiveShortTailProvider models a TailProvider whose tail view is
// narrower than full history (e.g. an active-file-only scan) — the ambiguous
// case fetchEventPageAscending must still treat conservatively.
type nonExhaustiveShortTailProvider struct {
	tail       []events.Event
	all        []events.Event
	listCalled bool
}

func (p *nonExhaustiveShortTailProvider) Record(events.Event) {}

func (p *nonExhaustiveShortTailProvider) List(context.Context, events.Filter) ([]events.Event, error) {
	p.listCalled = true
	return p.all, nil
}

func (p *nonExhaustiveShortTailProvider) ListTail(context.Context, events.Filter, int) ([]events.Event, error) {
	return p.tail, nil
}

func (p *nonExhaustiveShortTailProvider) LatestSeq() (uint64, error) { return uint64(len(p.all)), nil }

func (p *nonExhaustiveShortTailProvider) Watch(context.Context, uint64) (events.Watcher, error) {
	return nil, errors.New("not implemented")
}

func (p *nonExhaustiveShortTailProvider) Close() error { return nil }

var (
	_ events.Provider     = (*nonExhaustiveShortTailProvider)(nil)
	_ events.TailProvider = (*nonExhaustiveShortTailProvider)(nil)
)

// TestFetchEventPageAscendingStillFallsBackWithoutExhaustiveMarker guards the
// other direction: a plain TailProvider without the ExhaustiveTailProvider
// promise must still trigger the fallback on a short result, exactly as
// before this fix — its short result is ambiguous, not proven exhaustive.
// This is the safety property that keeps TestEventListWalkCrossesArchiveBoundary
// and TestEventListWalkCrossesInFlightRotation (handler_events_keyset_test.go)
// passing: their fakes are deliberately non-exhaustive and must not be
// treated as if they were.
func TestFetchEventPageAscendingStillFallsBackWithoutExhaustiveMarker(t *testing.T) {
	fallbackAll := []events.Event{
		{Seq: 1, Type: "convoy.closed"},
		{Seq: 5, Type: "convoy.closed"},
		{Seq: 9, Type: "convoy.closed"},
	}
	p := &nonExhaustiveShortTailProvider{
		tail: fallbackAll[len(fallbackAll)-1:], // simulate an active-file-only view: only the newest
		all:  fallbackAll,
	}

	got, scanned, err := fetchEventPageAscending(context.Background(), p, events.Filter{Type: "convoy.closed"}, 1000)
	if err != nil {
		t.Fatalf("fetchEventPageAscending: %v", err)
	}
	if !p.listCalled {
		t.Fatal("fell through without calling List — a non-exhaustive TailProvider's short result must still trigger the fallback")
	}
	if !reflect.DeepEqual(got, fallbackAll) {
		t.Fatalf("got %+v, want %+v (the full fallback scan's result)", got, fallbackAll)
	}
	if scanned != len(fallbackAll) {
		t.Fatalf("scanned = %d, want %d", scanned, len(fallbackAll))
	}
}
