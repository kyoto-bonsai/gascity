package nudgequeue

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
)

// lockState uses a short reader turnstile before the state flock. A writer
// holds the turnstile exclusively while waiting for and holding the state
// lock. A durable, flock-protected intent marker makes queued writers visible
// before they reach the gate, so a stream of readers cannot starve them.
// Readers release the turnstile as soon as they hold LOCK_SH on state.lock.
func lockState(cityPath string, write bool, waitTimeout time.Duration, clk clock.Clock) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(StatePath(cityPath)), 0o755); err != nil {
		return nil, fmt.Errorf("creating nudge queue dir: %w", err)
	}
	gate, err := os.OpenFile(LockPath(cityPath)+".gate", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening nudge queue gate: %w", err)
	}
	state, err := os.OpenFile(LockPath(cityPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = gate.Close()
		return nil, fmt.Errorf("opening nudge queue lock: %w", err)
	}
	closeFiles := func() {
		_ = state.Close()
		_ = gate.Close()
	}
	deadline := clk.Now().Add(waitTimeout)
	gateMode, stateMode := syscall.LOCK_SH, syscall.LOCK_SH
	if write {
		gateMode, stateMode = syscall.LOCK_EX, syscall.LOCK_EX
		clearIntent, err := createWriterIntent(cityPath)
		if err != nil {
			closeFiles()
			return nil, err
		}
		defer clearIntent()
	}
	for {
		if !write {
			// Check before and after locking: a writer can publish an intent
			// while a reader is acquiring the two flocks.
			active, err := activeWriterIntent(cityPath)
			if err != nil {
				closeFiles()
				return nil, err
			}
			if active {
				if !clk.Now().Before(deadline) {
					closeFiles()
					return nil, fmt.Errorf("locking nudge queue: timed out waiting %s for lock", waitTimeout)
				}
				time.Sleep(nudgeQueueLockPollInterval)
				continue
			}
		}
		if err := waitFlock(gate, gateMode, deadline, waitTimeout, clk); err != nil {
			closeFiles()
			return nil, err
		}
		if err := waitFlock(state, stateMode, deadline, waitTimeout, clk); err != nil {
			_ = syscall.Flock(int(gate.Fd()), syscall.LOCK_UN)
			closeFiles()
			return nil, err
		}
		if !write {
			active, err := activeWriterIntent(cityPath)
			if err != nil || active {
				_ = syscall.Flock(int(state.Fd()), syscall.LOCK_UN)
				_ = syscall.Flock(int(gate.Fd()), syscall.LOCK_UN)
				if err != nil {
					closeFiles()
					return nil, err
				}
				continue
			}
			_ = syscall.Flock(int(gate.Fd()), syscall.LOCK_UN)
		}
		break
	}
	return func() {
		_ = syscall.Flock(int(state.Fd()), syscall.LOCK_UN)
		if write {
			_ = syscall.Flock(int(gate.Fd()), syscall.LOCK_UN)
		}
		closeFiles()
	}, nil
}

func writerIntentDir(cityPath string) string {
	return filepath.Join(filepath.Dir(StatePath(cityPath)), "writer-intents")
}

// createWriterIntent publishes the marker only after taking its flock. A
// crashed writer leaves an unlocked marker that readers can safely reap.
func createWriterIntent(cityPath string) (func(), error) {
	dir := writerIntentDir(cityPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating nudge writer intent dir: %w", err)
	}
	file, err := os.CreateTemp(dir, ".writer-")
	if err != nil {
		return nil, fmt.Errorf("creating nudge writer intent: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = os.Remove(file.Name())
		_ = file.Close()
		return nil, fmt.Errorf("locking nudge writer intent: %w", err)
	}
	activePath := file.Name() + ".active"
	if err := os.Rename(file.Name(), activePath); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = os.Remove(file.Name())
		_ = file.Close()
		return nil, fmt.Errorf("publishing nudge writer intent: %w", err)
	}
	return func() {
		_ = os.Remove(activePath)
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func activeWriterIntent(cityPath string) (bool, error) {
	dir := writerIntentDir(cityPath)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading nudge writer intents: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".active") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("opening nudge writer intent: %w", err)
		}
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return true, nil
		}
		if err != nil {
			_ = file.Close()
			return false, fmt.Errorf("checking nudge writer intent: %w", err)
		}
		_ = os.Remove(path) // abandoned marker; its owning writer no longer holds the flock
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}
	return false, nil
}

func waitFlock(file *os.File, mode int, deadline time.Time, waitTimeout time.Duration, clk clock.Clock) error {
	for {
		err := syscall.Flock(int(file.Fd()), mode|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("locking nudge queue: %w", err)
		}
		if !clk.Now().Before(deadline) {
			return fmt.Errorf("locking nudge queue: timed out waiting %s for lock", waitTimeout)
		}
		time.Sleep(nudgeQueueLockPollInterval)
	}
}
