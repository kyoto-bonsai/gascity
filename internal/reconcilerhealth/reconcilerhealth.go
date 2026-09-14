// Package reconcilerhealth manages the session reconciler's per-tick
// liveness gauge (.gc/runtime/reconciler-tick-health.json).
//
// The session reconciler ticks on a variable cadence driven by upstream
// store latency (Dolt, providers). Under degraded upstream conditions a
// tick can take minutes instead of the healthy single-digit seconds
// (ga-r6lc7g: one observed wave took 4m49s against a 10-minute
// pending-create lease, so a session created between two ticks could have
// its lease expire before the reconciler got around to it at all). Before
// this package, answering "is the reconciler currently keeping up" required
// grepping supervisor.log for "session lifecycle: op=start wave=" lines or
// parsing trace segments after the fact — there was no live, structured
// signal `gc doctor` could check.
//
// This file is intentionally per-clone and gitignored (city-local runtime
// state, like suspension-state.json): it is a liveness gauge, not
// configuration, and carries no meaning across machines.
package reconcilerhealth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// State is the runtime tick-health gauge persisted to disk.
type State struct {
	// LastTickCompletedAt is stamped by Save on every write — the wall-clock
	// time the reconciler last completed a start-execution phase. A doctor
	// check compares time.Since(LastTickCompletedAt) against an expected
	// cadence to detect a stalled or badly-degraded reconciler.
	LastTickCompletedAt time.Time `json:"last_tick_completed_at"`
	// LastPhaseDurationMs is how long the just-completed start-execution
	// phase (session_reconcile.execute_planned_starts, covering every wave
	// this tick) took, in milliseconds. The healthy baseline is low
	// single-digit seconds; ga-r6lc7g observed 4m49s (289000ms) during the
	// incident.
	LastPhaseDurationMs int64 `json:"last_phase_duration_ms"`
	// StartCandidateCount and PlannedWakeCount mirror the same tick's trace
	// fields (session_reconcile.execute_planned_starts) so a doctor check or
	// a human reading the file does not need to cross-reference the trace to
	// see roughly what the last tick was doing.
	StartCandidateCount int `json:"start_candidate_count"`
	PlannedWakeCount    int `json:"planned_wake_count"`
	// TickCount is a best-effort monotonic counter of start-execution phases
	// observed since this file was first created (not since controller
	// start — the file can predate or outlive any one controller process).
	// It exists only so a reader can distinguish "freshly created, one
	// sample so far" from "long-running, many samples" — never treat it as
	// an exact tick census.
	TickCount int64 `json:"tick_count"`
}

// Load reads the reconciler tick-health gauge. A missing file returns a
// zero-value State and no error — the reconciler may simply not have
// written one yet (fresh city, or a controller binary older than this
// gauge), which callers should treat as "no data," not "unhealthy."
func Load(fs fsys.FS, cityPath string) (State, error) {
	p := citylayout.ReconcilerTickHealthFile(cityPath)
	data, err := fs.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, nil
		}
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, err
	}
	return st, nil
}

// Record stamps and atomically writes an updated tick-health gauge,
// incrementing TickCount off the PREVIOUSLY persisted value (best-effort:
// a concurrent writer racing this read-modify-write can under-count, which
// is acceptable for a liveness gauge that only needs "count is roughly
// increasing," never an exact total). LastTickCompletedAt is stamped to
// now(); callers pass everything else.
func Record(fs fsys.FS, cityPath string, phaseDuration time.Duration, startCandidateCount, plannedWakeCount int) error {
	prior, err := Load(fs, cityPath)
	if err != nil {
		// A corrupt or unreadable prior file must not block the reconciler
		// from recording fresh liveness data -- start the counter over.
		prior = State{}
	}
	st := State{
		LastTickCompletedAt: time.Now().UTC(),
		LastPhaseDurationMs: phaseDuration.Milliseconds(),
		StartCandidateCount: startCandidateCount,
		PlannedWakeCount:    plannedWakeCount,
		TickCount:           prior.TickCount + 1,
	}
	p := citylayout.ReconcilerTickHealthFile(cityPath)
	if err := fs.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsys.WriteFileAtomic(fs, p, data, 0o644)
}
