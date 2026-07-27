//go:build integration

package tmux

import (
	"os"
	"testing"
)

// TestSubprocessEnv_SurvivesDivergentAmbientTMPDIR is the end-to-end
// regression test for ga-mwylzp's real mechanism: two Tmux clients with
// TMUX_TMPDIR unset but different ambient TMPDIR values — simulating a
// spawner process (no TMPDIR override) and an observer process whose
// launchd-assigned per-boot TMPDIR points somewhere else entirely, the
// shape macOS produces across a reboot — must still reach the SAME real
// tmux server. Before the fix (TMUX_TMPDIR, then a TMPDIR fallback, passed
// straight through to the tmux subprocess), the second client's session
// lookup would use its own divergent TMPDIR and find nothing — exactly how
// ga-mwylzp's supervisor lost track of every session after a reboot.
//
// Deliberately uses short, flat paths directly under /tmp rather than
// t.TempDir() (which nests under macOS's deep per-test /var/folders/.../T/
// hierarchy): once tmux appends "tmux-$UID/<socket>", a nested t.TempDir()
// path can overflow the ~104-byte sun_path limit on Unix domain sockets and
// fail with an unrelated "File name too long" — a real, pre-existing tmux
// constraint, not a symptom of the bug this test targets.
func TestSubprocessEnv_SurvivesDivergentAmbientTMPDIR(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	origTmuxTmpDir, hadTmuxTmpDir := os.LookupEnv("TMUX_TMPDIR")
	origTmpDir, hadTmpDir := os.LookupEnv("TMPDIR")
	t.Cleanup(func() {
		if hadTmuxTmpDir {
			_ = os.Setenv("TMUX_TMPDIR", origTmuxTmpDir)
		} else {
			_ = os.Unsetenv("TMUX_TMPDIR")
		}
		if hadTmpDir {
			_ = os.Setenv("TMPDIR", origTmpDir)
		} else {
			_ = os.Unsetenv("TMPDIR")
		}
	})

	unrelated, err := os.MkdirTemp("/tmp", "gcu")
	if err != nil {
		t.Fatalf("creating unrelated dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(unrelated) })

	const socket = "gcw2"
	const name = "gcw2-s"

	// "Process A" (spawner-like): no TMPDIR override at all.
	if err := os.Unsetenv("TMUX_TMPDIR"); err != nil {
		t.Fatalf("unsetting TMUX_TMPDIR: %v", err)
	}
	if err := os.Unsetenv("TMPDIR"); err != nil {
		t.Fatalf("unsetting TMPDIR: %v", err)
	}
	cfgA := DefaultConfig()
	cfgA.SocketName = socket
	tmA := NewTmuxWithConfig(cfgA)
	t.Cleanup(func() {
		_ = os.Unsetenv("TMUX_TMPDIR")
		_ = os.Unsetenv("TMPDIR")
		_ = tmA.KillServer()
	})
	if err := tmA.NewSession(name, ""); err != nil {
		t.Fatalf("creating session with no TMPDIR override: %v", err)
	}

	// "Process B" (observer-like): TMUX_TMPDIR still unset, but TMPDIR
	// points at a directory that has nothing to do with /tmp — the
	// post-reboot shape.
	if err := os.Setenv("TMPDIR", unrelated); err != nil {
		t.Fatalf("setting TMPDIR to unrelated dir: %v", err)
	}
	cfgB := DefaultConfig()
	cfgB.SocketName = socket
	tmB := NewTmuxWithConfig(cfgB)
	has, err := tmB.HasSession(name)
	if err != nil {
		t.Fatalf("HasSession with divergent ambient TMPDIR: %v", err)
	}
	if !has {
		t.Fatal("session created by a TMPDIR-unset process is invisible to a process with a divergent ambient TMPDIR — the exact ga-mwylzp observer/spawner split")
	}
}
