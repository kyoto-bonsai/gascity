package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/reconcilerhealth"
)

func TestReconcilerTickHealthCheck_ControllerNotRunning_SkipsAsOK(t *testing.T) {
	c := newReconcilerTickHealthCheck(t.TempDir(), false)
	r := c.Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want StatusOK when controller is not running", r.Status)
	}
}

func TestReconcilerTickHealthCheck_NoGaugeYet_Warns(t *testing.T) {
	c := newReconcilerTickHealthCheck(t.TempDir(), true)
	r := c.Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning when the controller is running but no gauge has been written yet", r.Status)
	}
}

func TestReconcilerTickHealthCheck_FreshHealthyGauge_OK(t *testing.T) {
	cityDir := t.TempDir()
	if err := reconcilerhealth.Record(fsys.OSFS{}, cityDir, 3*time.Second, 2, 1, io.Discard); err != nil {
		t.Fatalf("seeding gauge: %v", err)
	}
	c := newReconcilerTickHealthCheck(cityDir, true)
	r := c.Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want StatusOK for a fresh, fast tick (%s)", r.Status, r.Message)
	}
}

func TestReconcilerTickHealthCheck_StaleTick_BlockingError(t *testing.T) {
	cityDir := t.TempDir()
	if err := reconcilerhealth.Record(fsys.OSFS{}, cityDir, time.Second, 1, 1, io.Discard); err != nil {
		t.Fatalf("seeding gauge: %v", err)
	}
	// Backdate the gauge past the staleness limit by writing it directly --
	// Record always stamps "now", so simulate elapsed time the way the check
	// itself measures it: by using a threshold no real clock could still
	// satisfy from a fresh write. Since Record cannot be given an explicit
	// timestamp, this test asserts on the constant relationship instead:
	// a gauge older than reconcilerTickStalenessLimit must block. Sleeping a
	// real 2x(10m) in a unit test is infeasible, so this exercises the
	// threshold constant and the comparison directly.
	c := newReconcilerTickHealthCheck(cityDir, true)
	c.now = func() time.Time { return time.Now().Add(reconcilerTickStalenessLimit + time.Minute) }
	r := c.Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusError {
		t.Fatalf("Status = %v, want StatusError for a gauge older than %s (%s)", r.Status, reconcilerTickStalenessLimit, r.Message)
	}
	if r.Severity != doctor.SeverityBlocking {
		t.Errorf("Severity = %v, want SeverityBlocking (the zero value) for a stale-tick error", r.Severity)
	}
}

func TestReconcilerTickHealthCheck_SlowWave_BlockingError(t *testing.T) {
	cityDir := t.TempDir()
	if err := reconcilerhealth.Record(fsys.OSFS{}, cityDir, reconcilerTickWaveLimit+time.Second, 1, 1, io.Discard); err != nil {
		t.Fatalf("seeding gauge: %v", err)
	}
	c := newReconcilerTickHealthCheck(cityDir, true)
	r := c.Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusError {
		t.Fatalf("Status = %v, want StatusError for a phase duration over %s (%s)", r.Status, reconcilerTickWaveLimit, r.Message)
	}
	if r.Severity != doctor.SeverityBlocking {
		t.Errorf("Severity = %v, want SeverityBlocking (the zero value) for a slow-wave error", r.Severity)
	}
}
