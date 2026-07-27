package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestControllerLockExclusion_SymlinkSpelling empirically checks whether a
// symlink-based spelling divergence in cityPath (e.g. /tmp vs /private/tmp,
// or any "alias dir -> real dir" case) can bypass acquireControllerLock's
// mutual exclusion. Unix flock(2) locks are associated with the underlying
// open file description / inode, and both open(2) and connect(2) resolve
// symlink path components at the kernel level regardless of Go-level string
// canonicalization -- so the hypothesis under test is that this specific
// class of divergence does NOT bypass the lock, even though acquireControllerLock
// never canonicalizes its input (unlike its sibling controllerSocketPath).
func TestControllerLockExclusion_SymlinkSpelling(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real-city")
	if err := os.MkdirAll(filepath.Join(real, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias-city")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}

	lock1, err := acquireControllerLock(real)
	if err != nil {
		t.Fatalf("first lock via real path %q: %v", real, err)
	}
	defer lock1.Close() //nolint:errcheck // test cleanup

	_, err = acquireControllerLock(alias)
	if err == nil {
		t.Fatalf("BUG REPRODUCED: second lock via symlinked spelling %q succeeded — mutual exclusion bypassed", alias)
	}
	t.Logf("second lock via symlinked spelling correctly failed: %v", err)
}

// TestControllerLockExclusion_DifferentRealDirs is the contrast case: two
// GENUINELY different directories (no symlink relationship at all) must NOT
// contend with each other -- confirms the test harness/helper distinguishes
// "same city, different spelling" from "different city" correctly.
func TestControllerLockExclusion_DifferentRealDirs(t *testing.T) {
	base := t.TempDir()
	dirA := filepath.Join(base, "city-a")
	dirB := filepath.Join(base, "city-b")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(filepath.Join(d, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	lockA, err := acquireControllerLock(dirA)
	if err != nil {
		t.Fatalf("lock on dirA: %v", err)
	}
	defer lockA.Close() //nolint:errcheck // test cleanup

	lockB, err := acquireControllerLock(dirB)
	if err != nil {
		t.Fatalf("lock on dirB should succeed independently, got: %v", err)
	}
	defer lockB.Close() //nolint:errcheck // test cleanup
}
