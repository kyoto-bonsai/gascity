package nudgequeue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/fsys"
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
	var writes atomic.Int64
	previousWrite := writeStateFile
	writeStateFile = func(fs fsys.FS, path string, data []byte, perm os.FileMode) error {
		writes.Add(1)
		return previousWrite(fs, path, data, perm)
	}
	t.Cleanup(func() { writeStateFile = previousWrite })

	const pollers = 50
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var firstTick sync.WaitGroup
	firstTick.Add(pollers)
	for i := 0; i < pollers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			first := true
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
				_ = WithState(cityPath, func(_ *State) error { return nil })
				if first {
					firstTick.Done()
					first = false
				}
			}
		}()
	}
	// Every poller has completed a real locked tick before the writer starts.
	firstTick.Wait()

	const writeID = "real-write"
	start := time.Now()
	writeErr := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending, Item{ID: writeID, Agent: "a", Source: "s", Message: "m"})
		return nil
	})
	writerLatency := time.Since(start)

	close(stop)
	wg.Wait()
	t.Logf("writer acquisition and commit under %d pollers: %s", pollers, writerLatency)

	if writeErr != nil {
		t.Fatalf("WithState (real write) failed under %d-poller no-op load: %v (ga-cssm95: a writer must not time out behind no-op reader/maintenance ticks)", pollers, writeErr)
	}
	if got := writes.Load(); got != 1 {
		t.Fatalf("physical state writes = %d, want exactly the real writer's one write", got)
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

// A writer that has entered the turnstile must commit before newly arriving
// busy pollers. The assertion is on persisted order and write count, not on a
// scheduler-dependent latency threshold.
func TestWithState_WriterPrecedesFiftyBusyPollers(t *testing.T) {
	cityPath := t.TempDir()
	if err := WithState(cityPath, func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int64
	firstWrite := make(chan []byte, 1)
	previousWrite := writeStateFile
	writeStateFile = func(fs fsys.FS, path string, data []byte, perm os.FileMode) error {
		if writes.Add(1) == 1 {
			firstWrite <- append([]byte(nil), data...)
		}
		return previousWrite(fs, path, data, perm)
	}
	t.Cleanup(func() { writeStateFile = previousWrite })
	hold, err := os.OpenFile(LockPath(cityPath), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close() //nolint:errcheck
	if err := syscall.Flock(int(hold.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(hold.Fd()), syscall.LOCK_UN) //nolint:errcheck

	writerDone := make(chan error, 1)
	start := time.Now()
	go func() {
		writerDone <- WithState(cityPath, func(state *State) error {
			state.Pending = append(state.Pending, Item{ID: "priority-writer"})
			return nil
		})
	}()
	gate, err := os.OpenFile(LockPath(cityPath)+".gate", os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close() //nolint:errcheck
	// Wait for the writer to hold the gate while our shared state lock keeps
	// its commit pending; this makes the later poller arrival order explicit.
	for {
		err := syscall.Flock(int(gate.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err == syscall.EWOULDBLOCK {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = syscall.Flock(int(gate.Fd()), syscall.LOCK_UN)
		select {
		case err := <-writerDone:
			t.Fatalf("writer exited before gate contention: %v", err)
		default:
		}
		time.Sleep(time.Millisecond)
	}

	const pollers = 50
	var wg sync.WaitGroup
	results := make(chan error, pollers)
	for i := 0; i < pollers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- WithState(cityPath, func(state *State) error {
				state.Pending = append(state.Pending, Item{ID: fmt.Sprintf("busy-poller-%d", i)})
				return nil
			})
		}(i)
	}
	if err := syscall.Flock(int(hold.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := <-writerDone; err != nil {
		t.Fatalf("writer timed out behind busy pollers: %v", err)
	}
	t.Logf("priority writer commit under %d busy pollers: %s", pollers, time.Since(start))
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("busy poller write failed: %v", err)
		}
	}
	if got := writes.Load(); got != pollers+1 {
		t.Fatalf("physical state writes = %d, want %d", got, pollers+1)
	}
	var state State
	if err := json.Unmarshal(<-firstWrite, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("first physical write contained %d items, want one", len(state.Pending))
	}
	if state.Pending[0].ID != "priority-writer" {
		t.Fatalf("first persisted write = %q, want priority writer", state.Pending[0].ID)
	}
}

// Fifty pollers have already decided to mutate and are queued at the gate
// before the producer arrives. The first physical write must be the producer's
// even though it joined last; this exercises priority before gate admission.
func TestReadThenWrite_ProducerPrecedesBusyMaintenanceWriters(t *testing.T) {
	cityPath := t.TempDir()
	if err := WithState(cityPath, func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int64
	firstWrite := make(chan []byte, 1)
	previousWrite := writeStateFile
	writeStateFile = func(fs fsys.FS, path string, data []byte, perm os.FileMode) error {
		if writes.Add(1) == 1 {
			firstWrite <- append([]byte(nil), data...)
		}
		return previousWrite(fs, path, data, perm)
	}
	t.Cleanup(func() { writeStateFile = previousWrite })

	gate, err := os.OpenFile(LockPath(cityPath)+".gate", os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close() //nolint:errcheck
	if err := syscall.Flock(int(gate.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(gate.Fd()), syscall.LOCK_UN) //nolint:errcheck

	const pollers = 50
	var blockedScans sync.WaitGroup
	blockedScans.Add(pollers)
	var blockedCount atomic.Int64
	maintenancePermit := make(chan struct{})
	producerBlocked := make(chan struct{})
	producerResume := make(chan struct{})
	maintenanceYielded := make(chan struct{}, 1)
	var producerOnce sync.Once
	stateLockTestHook = func(mode stateLockMode, event stateLockEvent) {
		switch {
		case mode == stateMaintenanceLock && event == stateGateBlocked:
			if blockedCount.Add(1) <= pollers {
				blockedScans.Done()
			}
			<-maintenancePermit
		case mode == stateProducerLock && event == stateGateBlocked:
			producerOnce.Do(func() {
				close(producerBlocked)
				<-producerResume
			})
		case mode == stateMaintenanceLock && event == stateMaintenanceYield:
			select {
			case maintenanceYielded <- struct{}{}:
			default:
			}
		}
	}
	t.Cleanup(func() { stateLockTestHook = nil })
	var scans sync.WaitGroup
	var pollerWG sync.WaitGroup
	scans.Add(pollers)
	proceed := make(chan struct{})
	pollerErrors := make(chan error, pollers)
	for i := 0; i < pollers; i++ {
		pollerWG.Add(1)
		go func(i int) {
			defer pollerWG.Done()
			pollerErrors <- ReadThenWrite(cityPath, func(State) bool {
				scans.Done()
				<-proceed
				return true
			}, func(State) error {
				return fmt.Errorf("busy poller unexpectedly took read path")
			}, func(state *State) error {
				state.Pending = append(state.Pending, Item{ID: fmt.Sprintf("busy-poller-%d", i)})
				return nil
			})
		}(i)
	}
	scans.Wait()
	close(proceed)
	blockedScans.Wait()
	intentDir := writerIntentDir(cityPath)
	entries, err := os.ReadDir(intentDir)
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".maintenance-") && strings.HasSuffix(entry.Name(), ".active") {
			active++
		}
	}
	if active != pollers {
		t.Fatalf("queued maintenance intents = %d, want %d", active, pollers)
	}

	producerDone := make(chan error, 1)
	start := time.Now()
	go func() {
		producerDone <- WithState(cityPath, func(state *State) error {
			state.Pending = append(state.Pending, Item{ID: "producer"})
			return nil
		})
	}()
	<-producerBlocked
	if err := syscall.Flock(int(gate.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	// Pause the producer after its first failed gate attempt and admit one
	// already queued maintenance writer. It must yield without committing.
	go func() { maintenancePermit <- struct{}{} }()
	select {
	case <-maintenanceYielded:
	case data := <-firstWrite:
		t.Fatalf("maintenance overtook pending producer: first physical write %s", data)
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance writer never resolved gate admission")
	}
	close(producerResume)
	if err := <-producerDone; err != nil {
		t.Fatalf("producer timed out behind busy pollers: %v", err)
	}
	t.Logf("producer commit behind %d queued mutating pollers: %s", pollers, time.Since(start))
	close(maintenancePermit)
	pollerWG.Wait()
	close(pollerErrors)
	for err := range pollerErrors {
		if err != nil {
			t.Fatalf("busy poller failed: %v", err)
		}
	}
	if got := writes.Load(); got != pollers+1 {
		t.Fatalf("physical state writes = %d, want %d", got, pollers+1)
	}
	var first State
	if err := json.Unmarshal(<-firstWrite, &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Pending) != 1 || first.Pending[0].ID != "producer" {
		t.Fatalf("first physical write = %+v, want producer only", first.Pending)
	}
}

func TestReadThenWriteReloadsAfterSharedLockRelease(t *testing.T) {
	cityPath := t.TempDir()
	if err := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending, Item{ID: "initial"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var interveningErr error
	afterReadUnlock = func() {
		interveningErr = WithState(cityPath, func(state *State) error {
			state.Pending = append(state.Pending, Item{ID: "intervening"})
			return nil
		})
	}
	t.Cleanup(func() { afterReadUnlock = nil })
	readCalled := false
	err := ReadThenWrite(cityPath, func(state State) bool {
		return len(state.Pending) == 1
	}, func(State) error {
		readCalled = true
		return nil
	}, func(state *State) error {
		if len(state.Pending) != 2 {
			return fmt.Errorf("exclusive reload saw %d items, want intervening write", len(state.Pending))
		}
		state.Pending = append(state.Pending, Item{ID: "upgrade"})
		return nil
	})
	if interveningErr != nil || err != nil || readCalled {
		t.Fatalf("upgrade: intervening=%v write=%v readCalled=%v", interveningErr, err, readCalled)
	}
	state, err := LoadState(cityPath)
	if err != nil || len(state.Pending) != 3 {
		t.Fatalf("final state: count=%d err=%v", len(state.Pending), err)
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
