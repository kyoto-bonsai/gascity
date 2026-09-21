package nudgequeue

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
)

// TestWithState_SkipsRewriteWhenUnchanged guards ga-cssm95: a no-op fn (the
// common case for the poller fleet's list/claim-scan ticks, which run a
// maintenance pass but usually find nothing to recover/prune/claim) must not
// pay for a JSON marshal + atomic rewrite of state.json. Proven by seeding
// the state file by hand in a form WithState's own MarshalIndent would never
// produce (compact, single line) and confirming a no-op call leaves those
// exact bytes untouched -- if a rewrite happened, the file would come back
// re-indented even though the field values are unchanged.
func TestWithState_SkipsRewriteWhenUnchanged(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(StatePath(cityPath)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const handWritten = `{"pending":[{"id":"seed","agent":"a","source":"s","message":"m","created_at":"2026-01-01T00:00:00Z","deliver_after":"2026-01-01T00:00:00Z","expires_at":"2026-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(StatePath(cityPath), []byte(handWritten), 0o644); err != nil {
		t.Fatalf("seed state.json: %v", err)
	}

	if err := WithState(cityPath, func(_ *State) error {
		return nil // no-op: the common poller-tick shape
	}); err != nil {
		t.Fatalf("WithState (no-op): %v", err)
	}

	got, err := os.ReadFile(StatePath(cityPath))
	if err != nil {
		t.Fatalf("reading state.json after no-op call: %v", err)
	}
	if string(got) != handWritten {
		t.Fatalf("state.json was rewritten by a no-op WithState call: got %q, want untouched %q (ga-cssm95: this unconditional rewrite is what starves writers under poller load)", got, handWritten)
	}
}

// TestWithState_WriterSucceedsUnderPollerLoad guards ga-cssm95: a real writer
// (sling/mail/nudge -- anything that actually mutates the queue) must not
// starve behind a fleet of no-op poller ticks hammering the same exclusive
// lock. Models the production shape: ~50 concurrent `gc nudge poll` sidecars,
// each re-entering WithState roughly every 2s and finding nothing to
// recover/prune/claim on the overwhelming majority of ticks.
//
// The assertion here is deliberately just "did the write succeed at all",
// not a tight wall-clock figure: WithState already self-enforces
// defaultLockWaitTimeout internally, and a per-test absolute-millisecond
// bound would be load-flaky on a busy box -- exactly the failure class this
// bug itself is (ga-cssm95's writers weren't slow, they were failing
// outright after the full 8s budget).
func TestWithState_WriterSucceedsUnderPollerLoad(t *testing.T) {
	cityPath := t.TempDir()

	const pollers = 50
	stop := make(chan struct{})
	entered := make(chan struct{}, pollers)
	var wg sync.WaitGroup
	var stopOnce sync.Once
	stopPollers := func() {
		stopOnce.Do(func() { close(stop) })
		wg.Wait()
	}
	defer stopPollers()
	for i := 0; i < pollers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			firstPass := true
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Mirrors the real poller's dominant-case tick: runs the
				// same locked pass a maintenance/claim-scan call does and
				// finds nothing to change. Errors are intentionally ignored
				// here (a real poller just retries on its own schedule);
				// the test's actual assertion is on the writer below.
				_ = WithState(cityPath, func(_ *State) error {
					if firstPass {
						firstPass = false
						entered <- struct{}{}
					}
					return nil
				})
			}
		}()
	}
	// Each poller must enter its first locked callback before the writer joins.
	// This proves contention has begun without guessing a sleep budget.
	readyDeadline := time.NewTimer(10 * time.Second)
	defer readyDeadline.Stop()
	for i := 0; i < pollers; i++ {
		select {
		case <-entered:
		case <-readyDeadline.C:
			t.Fatalf("only %d/%d pollers entered WithState before deadline", i, pollers)
		}
	}

	const writeID = "real-write"
	writeErr := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending, Item{ID: writeID, Agent: "a", Source: "s", Message: "m"})
		return nil
	})

	stopPollers()

	if writeErr != nil {
		t.Fatalf("WithState (real write) failed under %d-poller no-op load: %v (ga-cssm95: a writer must not time out behind no-op reader/maintenance ticks)", pollers, writeErr)
	}

	state, err := LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState after write: %v", err)
	}
	found := false
	for _, item := range state.Pending {
		if item.ID == writeID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("real write did not persist despite WithState reporting success")
	}
}

// TestWithState_TimesOutInsteadOfBlockingForever guards ga-2kzci3 FR1/FR2:
// WithState itself -- not just the withStateBounded helper it wraps -- must
// give a caller that cannot acquire the queue lock within its default budget
// a clear, actionable error, never an unbounded hang. The bound was
// specified to live inside WithState, since every production caller goes
// through it directly, not sit beside it as dead code only a test exercises.
func TestWithState_TimesOutInsteadOfBlockingForever(t *testing.T) {
	cityPath := t.TempDir()

	if err := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending, Item{ID: "seed"})
		return nil
	}); err != nil {
		t.Fatalf("seed queue state: %v", err)
	}

	lockFile, err := os.OpenFile(LockPath(cityPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer lockFile.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("hold queue lock: %v", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck

	done := make(chan error, 1)
	go func() {
		done <- WithState(cityPath, func(_ *State) error {
			t.Error("WithState invoked fn after failing to acquire the lock")
			return nil
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("WithState returned nil error while queue lock was held; want a timeout error")
		}
		lower := strings.ToLower(err.Error())
		mentionsLock := strings.Contains(lower, "lock")
		mentionsTimeout := strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out")
		if !mentionsLock || !mentionsTimeout {
			t.Fatalf("WithState error = %q, want a descriptive lock-timeout error (mentioning lock + timeout)", err)
		}
	case <-time.After(defaultLockWaitTimeout + 2*time.Second):
		t.Fatal("WithState blocked far past its default wait timeout while the queue lock was held (unbounded wait regression)")
	}
}

// TestWithStateBounded_TimesOutInsteadOfBlockingForever guards ga-2kzci3 FR1/FR2:
// a caller that cannot acquire the queue lock within its budget must get a
// clear, actionable error, never an unbounded hang.
func TestWithStateBounded_TimesOutInsteadOfBlockingForever(t *testing.T) {
	cityPath := t.TempDir()

	if err := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending, Item{ID: "seed"})
		return nil
	}); err != nil {
		t.Fatalf("seed queue state: %v", err)
	}

	lockFile, err := os.OpenFile(LockPath(cityPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer lockFile.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("hold queue lock: %v", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck

	const waitTimeout = 150 * time.Millisecond
	done := make(chan error, 1)
	var called bool
	var mu sync.Mutex
	go func() {
		done <- withStateBounded(cityPath, waitTimeout, clock.Real{}, func(_ *State) error {
			mu.Lock()
			called = true
			mu.Unlock()
			return nil
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("withStateBounded returned nil error while queue lock was held; want a timeout error")
		}
		lower := strings.ToLower(err.Error())
		mentionsLock := strings.Contains(lower, "lock")
		mentionsTimeout := strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out")
		if !mentionsLock || !mentionsTimeout {
			t.Fatalf("withStateBounded error = %q, want a descriptive lock-timeout error (mentioning lock + timeout)", err)
		}
		mu.Lock()
		gotCalled := called
		mu.Unlock()
		if gotCalled {
			t.Fatal("withStateBounded invoked fn after failing to acquire the lock")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("withStateBounded blocked far past its wait timeout while the queue lock was held (unbounded wait regression)")
	}
}
