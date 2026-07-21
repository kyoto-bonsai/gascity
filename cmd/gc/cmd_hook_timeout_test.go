package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/dispatch"
)

// TestWorkQueryTimeoutsAccommodateMultiRoundTripProbe guards the work-query
// timeout budget. The default work-probe (config.Agent.EffectiveWorkQuery)
// issues ~6 sequential bd/store round-trips — three session identifiers across
// the in-progress and ready assigned tiers — before the pool-demand tier that
// finds routed work. On a multi-rig dolt city under concurrent load each
// round-trip costs several seconds, so at the prior 30s work-query subprocess
// budget the probe was killed before reaching pool-demand and pool operators
// (gc.run-operator) were starved of work they had already been routed. Keep the
// subprocess budget generous enough to clear the realistic loaded cost.
//
// This guards hookWorkQueryTimeout, the cap that actually bounds the work query
// (shellWorkQueryWithEnv in `gc hook` and the workflow serve loop). It does not
// constrain defaultHookRunTimeout: that budget bounds the separate `gc hook run`
// managed-hook wrapper (nudge drain / mail check) and does not enclose the work
// query, so the two are intentionally independent and not asserted against each
// other here.
func TestWorkQueryTimeoutsAccommodateMultiRoundTripProbe(t *testing.T) {
	// minProbeBudget is the remediation target, not merely the old cap: keeping it
	// at 60s means a regression of hookWorkQueryTimeout back to the known-bad 30s
	// budget (which starved pool operators) fails this guard rather than passing it.
	const minProbeBudget = 60 * time.Second

	if hookWorkQueryTimeout < minProbeBudget {
		t.Errorf("hookWorkQueryTimeout = %s, want >= %s (multi-round-trip probe budget)", hookWorkQueryTimeout, minProbeBudget)
	}
}

// clampHookWorkQueryRetryVars clamps every retry-related knob for a single
// test and restores the originals on cleanup. Tests that only need a subset
// still pass all four so nothing leaks a production-scale value (particularly
// hookWorkQueryOverallDeadline, whose 100s default would make a misconfigured
// test hang instead of fail fast).
func clampHookWorkQueryRetryVars(t *testing.T, timeout, overall, baseDelay time.Duration, maxAttempts int) {
	t.Helper()
	oldTimeout, oldOverall, oldBaseDelay, oldMaxAttempts := hookWorkQueryTimeout, hookWorkQueryOverallDeadline, hookWorkQueryRetryBaseDelay, hookWorkQueryMaxAttempts
	hookWorkQueryTimeout, hookWorkQueryOverallDeadline, hookWorkQueryRetryBaseDelay, hookWorkQueryMaxAttempts = timeout, overall, baseDelay, maxAttempts
	t.Cleanup(func() {
		hookWorkQueryTimeout, hookWorkQueryOverallDeadline, hookWorkQueryRetryBaseDelay, hookWorkQueryMaxAttempts = oldTimeout, oldOverall, oldBaseDelay, oldMaxAttempts
	})
}

// TestShellWorkQueryRetriesOnDeadlineExceeded guards the actual behavior
// ga-t2brh8 candidate 1 was scoped for: a single work-query attempt that
// fails with its own context deadline (transient store load) gets retried,
// and a subsequent success is surfaced normally rather than returning the
// first attempt's timeout error.
func TestShellWorkQueryRetriesOnDeadlineExceeded(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	clampHookWorkQueryRetryVars(t, 100*time.Millisecond, 5*time.Second, 10*time.Millisecond, 2)

	marker := filepath.Join(t.TempDir(), "attempted")
	// First invocation: creates the marker then hangs past the per-attempt
	// timeout (killed, retried). Second invocation: marker exists, returns
	// immediately with success output.
	command := fmt.Sprintf(`if [ -f %q ]; then printf 'ok'; else touch %q; sleep 5; fi`, marker, marker)

	out, err := shellWorkQueryWithEnv(command, "", nil)
	if err != nil {
		t.Fatalf("shellWorkQueryWithEnv() error = %v, want nil (second attempt should succeed)", err)
	}
	if out != "ok" {
		t.Fatalf("out = %q, want %q", out, "ok")
	}
}

// TestShellWorkQueryDoesNotRetryNonTimeoutError guards the other half of the
// same contract: a deterministic failure (bad command, non-zero exit) must
// NOT be retried, since retrying a deterministic failure only adds load to an
// already-saturated store without changing the outcome (ga-t2brh8 audit).
func TestShellWorkQueryDoesNotRetryNonTimeoutError(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	clampHookWorkQueryRetryVars(t, 5*time.Second, 5*time.Second, 10*time.Millisecond, 3)

	counter := filepath.Join(t.TempDir(), "attempts")
	command := fmt.Sprintf(`printf 'x' >> %q; exit 1`, counter)

	_, err := shellWorkQueryWithEnv(command, "", nil)
	if err == nil {
		t.Fatal("shellWorkQueryWithEnv() error = nil, want non-nil (deterministic exit 1)")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a non-deadline error for this case", err)
	}
	data, readErr := os.ReadFile(counter)
	if readErr != nil {
		t.Fatalf("reading attempt counter: %v", readErr)
	}
	if got := len(data); got != 1 {
		t.Fatalf("attempt count = %d, want exactly 1 (non-timeout errors must not retry)", got)
	}
}

// TestShellWorkQueryOverallDeadlineBoundsRetries guards constraint (a) from
// the ga-t2brh8 audit: hookWorkQueryOverallDeadline must cap total wall-clock
// across every attempt, even when hookWorkQueryMaxAttempts alone would allow
// far longer (here, 10 attempts * 80ms = 800ms if unbounded).
func TestShellWorkQueryOverallDeadlineBoundsRetries(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	clampHookWorkQueryRetryVars(t, 80*time.Millisecond, 150*time.Millisecond, 5*time.Millisecond, 10)

	start := time.Now()
	_, err := shellWorkQueryWithEnv("sleep 5", "", nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("shellWorkQueryWithEnv() error = nil, want timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want errors.Is(err, context.DeadlineExceeded)", err)
	}
	if !dispatch.IsTransientControllerError(err) {
		t.Fatalf("dispatch.IsTransientControllerError(%v) = false, want true (exhausted retries must still classify transient)", err)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("elapsed = %s, want well under hookWorkQueryMaxAttempts*hookWorkQueryTimeout unbounded (800ms) — overall deadline should cap retries near 150ms", elapsed)
	}
}

// TestShellWorkQueryMaxAttemptsOneDisablesRetry guards the escape hatch: ops
// can dial hookWorkQueryMaxAttempts back to 1 to fully restore pre-ga-t2brh8
// single-attempt behavior without a binary rollback.
func TestShellWorkQueryMaxAttemptsOneDisablesRetry(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	clampHookWorkQueryRetryVars(t, 50*time.Millisecond, 5*time.Second, 10*time.Millisecond, 1)

	counter := filepath.Join(t.TempDir(), "attempts")
	command := fmt.Sprintf(`printf 'x' >> %q; sleep 5`, counter)

	_, err := shellWorkQueryWithEnv(command, "", nil)
	if err == nil {
		t.Fatal("shellWorkQueryWithEnv() error = nil, want timeout")
	}
	data, readErr := os.ReadFile(counter)
	if readErr != nil {
		t.Fatalf("reading attempt counter: %v", readErr)
	}
	if got := len(data); got != 1 {
		t.Fatalf("attempt count = %d, want exactly 1 (hookWorkQueryMaxAttempts=1 must disable retry)", got)
	}
}
