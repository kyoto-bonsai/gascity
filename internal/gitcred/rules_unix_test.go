//go:build !windows

package gitcred

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestStatOwnerReportsRealOwnership pins the plumbing between os.Stat and
// secureMode. Every other permission test is a rejection, and a broken
// statOwner would fail closed and still pass them; only the accept path
// depends on these values being the real uid/gid, and that path needs root to
// reproduce (see TestLoadAcceptsRootOwnedGroupReadable).
func TestStatOwnerReportsRealOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cred")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	uid, gid, ok := statOwner(info)
	if !ok {
		t.Fatalf("statOwner reported no Unix ownership for %s", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("os.Stat returned %T, want *syscall.Stat_t", info.Sys())
	}
	if uid != stat.Uid || gid != stat.Gid {
		t.Fatalf("statOwner = (%d,%d), want file ownership (%d,%d)", uid, gid, stat.Uid, stat.Gid)
	}
}
