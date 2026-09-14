package reconcilerhealth

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestLoad_MissingFileReturnsZeroStateNoError(t *testing.T) {
	cityDir := t.TempDir()
	st, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("Load on a missing file returned an error: %v", err)
	}
	if !st.LastTickCompletedAt.IsZero() {
		t.Fatalf("expected zero-value State, got %+v", st)
	}
}

func TestRecord_RoundTrip(t *testing.T) {
	cityDir := t.TempDir()
	before := time.Now().UTC()

	if err := Record(fsys.OSFS{}, cityDir, 250*time.Millisecond, 3, 2, io.Discard); err != nil {
		t.Fatalf("Record: %v", err)
	}

	st, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.LastTickCompletedAt.Before(before) {
		t.Errorf("LastTickCompletedAt %v is before the call (%v) -- not freshly stamped", st.LastTickCompletedAt, before)
	}
	if st.LastPhaseDurationMs != 250 {
		t.Errorf("LastPhaseDurationMs = %d, want 250", st.LastPhaseDurationMs)
	}
	if st.StartCandidateCount != 3 {
		t.Errorf("StartCandidateCount = %d, want 3", st.StartCandidateCount)
	}
	if st.PlannedWakeCount != 2 {
		t.Errorf("PlannedWakeCount = %d, want 2", st.PlannedWakeCount)
	}
	if st.TickCount != 1 {
		t.Errorf("TickCount = %d, want 1 (first-ever write)", st.TickCount)
	}
}

func TestRecord_TickCountIncrementsAcrossWrites(t *testing.T) {
	cityDir := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := Record(fsys.OSFS{}, cityDir, time.Second, 1, 1, io.Discard); err != nil {
			t.Fatalf("Record #%d: %v", i, err)
		}
	}
	st, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.TickCount != 3 {
		t.Errorf("TickCount after 3 writes = %d, want 3", st.TickCount)
	}
}

func TestRecord_CorruptPriorFileDoesNotBlockAFreshWrite(t *testing.T) {
	cityDir := t.TempDir()
	runtimeDir := filepath.Join(cityDir, ".gc", "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "reconciler-tick-health.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seeding corrupt file: %v", err)
	}

	if err := Record(fsys.OSFS{}, cityDir, time.Second, 1, 1, io.Discard); err != nil {
		t.Fatalf("Record after a corrupt prior file: %v", err)
	}
	st, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("Load after recovery write: %v", err)
	}
	if st.TickCount != 1 {
		t.Errorf("TickCount after recovering from a corrupt prior file = %d, want 1 (counter restarts)", st.TickCount)
	}
}

// TestRecord_ToleratesConcurrentCityPathRemoval is the ga-r6lc7g /
// persona-ava round-2 regression pin: a background reconciler tick's
// Record call raced Go's t.TempDir() cleanup in
// TestCityRuntimeBeadReconcileTick_BootDoesNotBlockOnWispSweep (11/30
// isolated failures on f6c2573ee, "unlinkat .../.gc/runtime: directory not
// empty" -- the write recreated content in a directory a concurrent
// os.RemoveAll had already listed as empty). Record must recognize a
// vanished cityPath and skip the write entirely -- logged, not erroring --
// rather than attempt to write into (and thereby recreate) a directory
// that no longer exists.
func TestRecord_ToleratesConcurrentCityPathRemoval(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.RemoveAll(cityDir); err != nil {
		t.Fatalf("removing cityDir to simulate a raced teardown: %v", err)
	}

	var log bytes.Buffer
	if err := Record(fsys.OSFS{}, cityDir, time.Second, 1, 1, &log); err != nil {
		t.Fatalf("Record against a vanished cityPath returned an error, want nil (benign no-op): %v", err)
	}
	if _, err := os.Stat(cityDir); err == nil {
		t.Errorf("Record recreated cityDir %q after it was removed -- it must not recreate a vanished target", cityDir)
	}
	if !strings.Contains(log.String(), "no longer exists") {
		t.Errorf("expected a logged no-op diagnostic mentioning the vanished city path, got: %q", log.String())
	}
}

// TestRecord_NilLogDoesNotPanic pins the io.Discard fallback for callers
// that pass a nil log writer (defensive -- session_reconciler.go always
// passes stderr today, but Record's own contract should not crash a caller
// that doesn't).
func TestRecord_NilLogDoesNotPanic(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.RemoveAll(cityDir); err != nil {
		t.Fatalf("removing cityDir: %v", err)
	}
	if err := Record(fsys.OSFS{}, cityDir, time.Second, 1, 1, nil); err != nil {
		t.Fatalf("Record with a nil log writer returned an error, want nil: %v", err)
	}
}
