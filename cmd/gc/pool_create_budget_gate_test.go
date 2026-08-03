package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// capWriter is defined in provider_health_gate_test.go and reused here.

func TestRecordPoolCreateBudgetExhaustion_AlertsOnceThenSuppressesWithinCooldown(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	w := &capWriter{fn: func(b []byte) { out.Write(b) }}
	base := time.Date(2026, 7, 26, 13, 12, 0, 0, time.UTC)

	recordPoolCreateBudgetExhaustion(dir, "seo-author/nils/reese", base, w)
	recordPoolCreateBudgetExhaustion(dir, "seo-author/nils/reese", base.Add(time.Minute), w)
	recordPoolCreateBudgetExhaustion(dir, "seo-author/nils/reese", base.Add(2*time.Minute), w)

	if got := strings.Count(out.String(), "budget gate OPEN"); got != 1 {
		t.Fatalf("alert count = %d, want exactly 1 within the cooldown window; stderr=%q", got, out.String())
	}
}

func TestRecordPoolCreateBudgetExhaustion_ReAlertsAfterCooldown(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	w := &capWriter{fn: func(b []byte) { out.Write(b) }}
	base := time.Date(2026, 7, 26, 13, 12, 0, 0, time.UTC)

	recordPoolCreateBudgetExhaustion(dir, "worker", base, w)
	recordPoolCreateBudgetExhaustion(dir, "worker", base.Add(poolCreateBudgetAlertCooldown+time.Second), w)

	if got := strings.Count(out.String(), "budget gate OPEN"); got != 2 {
		t.Fatalf("alert count = %d, want 2 (one per cooldown window); stderr=%q", got, out.String())
	}
}

func TestRecordPoolCreateBudgetExhaustion_PersistsDeferCountWithinEpisode(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	w := &capWriter{fn: func(b []byte) { out.Write(b) }}
	base := time.Date(2026, 7, 26, 13, 12, 0, 0, time.UTC)

	recordPoolCreateBudgetExhaustion(dir, "worker", base, w)
	recordPoolCreateBudgetExhaustion(dir, "worker", base.Add(time.Minute), w)

	gateFile := loadPoolCreateBudgetGateFile(filepath.Join(dir, poolCreateBudgetGateRelPath))
	entry, ok := gateFile.Templates["worker"]
	if !ok {
		t.Fatal("expected a persisted gate entry for template \"worker\"")
	}
	if entry.DeferCount != 2 {
		t.Fatalf("DeferCount = %d, want 2", entry.DeferCount)
	}
	if !entry.FirstDeferredAt.Equal(base) {
		t.Fatalf("FirstDeferredAt = %v, want %v (unchanged within one episode)", entry.FirstDeferredAt, base)
	}
}

func TestRecordPoolCreateBudgetExhaustion_FreshEpisodeResetsCounterAfterGap(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	w := &capWriter{fn: func(b []byte) { out.Write(b) }}
	base := time.Date(2026, 7, 26, 13, 12, 0, 0, time.UTC)

	recordPoolCreateBudgetExhaustion(dir, "worker", base, w)
	recordPoolCreateBudgetExhaustion(dir, "worker", base.Add(time.Minute), w) // same episode: DeferCount -> 2

	laterBase := base.Add(poolCreateBudgetEpisodeGap + time.Hour) // well past the episode gap
	recordPoolCreateBudgetExhaustion(dir, "worker", laterBase, w)

	gateFile := loadPoolCreateBudgetGateFile(filepath.Join(dir, poolCreateBudgetGateRelPath))
	entry, ok := gateFile.Templates["worker"]
	if !ok {
		t.Fatal("expected a persisted gate entry for template \"worker\"")
	}
	if entry.DeferCount != 1 {
		t.Fatalf("DeferCount after gap = %d, want 1 (fresh episode, not a continuation)", entry.DeferCount)
	}
	if !entry.FirstDeferredAt.Equal(laterBase) {
		t.Fatalf("FirstDeferredAt after gap = %v, want %v (episode restarted)", entry.FirstDeferredAt, laterBase)
	}
	// A fresh episode must re-alert even though it followed close on the
	// heels of an alert from the prior (now-stale) episode.
	if got := strings.Count(out.String(), "budget gate OPEN"); got != 2 {
		t.Fatalf("alert count = %d, want 2 (one per episode); stderr=%q", got, out.String())
	}
}

func TestRecordPoolCreateBudgetExhaustion_TracksTemplatesIndependently(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	w := &capWriter{fn: func(b []byte) { out.Write(b) }}
	base := time.Date(2026, 7, 26, 13, 12, 0, 0, time.UTC)

	recordPoolCreateBudgetExhaustion(dir, "seo-author", base, w)
	recordPoolCreateBudgetExhaustion(dir, "nils", base, w)
	recordPoolCreateBudgetExhaustion(dir, "reese", base, w)

	gateFile := loadPoolCreateBudgetGateFile(filepath.Join(dir, poolCreateBudgetGateRelPath))
	for _, template := range []string{"seo-author", "nils", "reese"} {
		if _, ok := gateFile.Templates[template]; !ok {
			t.Errorf("expected a persisted gate entry for template %q, templates=%v", template, gateFile.Templates)
		}
	}
	if got := strings.Count(out.String(), "budget gate OPEN"); got != 3 {
		t.Fatalf("alert count = %d, want 3 (one per distinct template)", got)
	}
}

func TestRecordPoolCreateBudgetExhaustion_WritesEvent(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	w := &capWriter{fn: func(b []byte) { out.Write(b) }}
	now := time.Date(2026, 7, 26, 13, 12, 0, 0, time.UTC)

	recordPoolCreateBudgetExhaustion(dir, "worker", now, w)

	data, err := os.ReadFile(filepath.Join(dir, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("reading events.jsonl: %v", err)
	}
	if !strings.Contains(string(data), events.PoolCreateBudgetGateAlert) {
		t.Fatalf("events.jsonl missing %q event: %s", events.PoolCreateBudgetGateAlert, data)
	}
	if !strings.Contains(string(data), `"subject":"worker"`) {
		t.Fatalf("events.jsonl event missing subject=worker: %s", data)
	}
}

func TestEmitPoolCreateBudgetGateAlert_Format(t *testing.T) {
	var captured string
	w := &capWriter{fn: func(b []byte) { captured += string(b) }}
	since := time.Date(2026, 7, 26, 13, 12, 0, 0, time.UTC)

	emitPoolCreateBudgetGateAlert(t.TempDir(), "seo-author/nils/reese", since, 7, since.Add(time.Minute), w)

	for _, want := range []string{"seo-author/nils/reese", "2026-07-26T13:12:00Z", "deferred=7"} {
		if !strings.Contains(captured, want) {
			t.Errorf("alert message missing %q\ngot: %s", want, captured)
		}
	}
}

// TestEmitPoolCreateBudgetGateAlert_DoesNotAdviseTheForbiddenRemedy guards
// ga-n75udx: the gate's own remediation text must never coach raising
// [daemon].max_wakes_per_tick — city.toml:150-152 forbids re-raising it
// without a machine-checked revert trigger (ga-v8mtlp), so the alert most
// likely to be read during slot pressure must not point at the one remedy
// that reproduces the thundering herd. Asserted on the emitted message, not
// the source constant, per the bead's acceptance criterion.
func TestEmitPoolCreateBudgetGateAlert_DoesNotAdviseTheForbiddenRemedy(t *testing.T) {
	var captured string
	w := &capWriter{fn: func(b []byte) { captured += string(b) }}
	since := time.Date(2026, 7, 26, 13, 12, 0, 0, time.UTC)

	emitPoolCreateBudgetGateAlert(t.TempDir(), "seo-author/nils/reese", since, 7, since.Add(time.Minute), w)

	if strings.Contains(captured, "or raise [daemon].max_wakes_per_tick") {
		t.Errorf("alert message still coaches the forbidden remedy (see city.toml:150-152, ga-v8mtlp)\ngot: %s", captured)
	}
	for _, want := range []string{"Do NOT raise [daemon].max_wakes_per_tick", "ga-v8mtlp", "`gc session list`"} {
		if !strings.Contains(captured, want) {
			t.Errorf("alert message missing %q\ngot: %s", want, captured)
		}
	}
}
