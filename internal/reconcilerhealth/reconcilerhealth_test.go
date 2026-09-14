package reconcilerhealth

import (
	"os"
	"path/filepath"
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

	if err := Record(fsys.OSFS{}, cityDir, 250*time.Millisecond, 3, 2); err != nil {
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
		if err := Record(fsys.OSFS{}, cityDir, time.Second, 1, 1); err != nil {
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

	if err := Record(fsys.OSFS{}, cityDir, time.Second, 1, 1); err != nil {
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
