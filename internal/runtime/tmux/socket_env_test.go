package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCanonicalTmuxTmpDir_IgnoresAmbientTMPDIR is the discriminating
// regression test for ga-mwylzp's actual mechanism: when TMUX_TMPDIR is
// unset, the result must be "/tmp" (resolved), never whatever TMPDIR
// happens to hold. Pre-fix code (TMUX_TMPDIR, then TMPDIR, then "/tmp")
// fails this the moment TMPDIR is set to any other directory — exactly the
// shape of a launchd-assigned per-boot TMPDIR diverging from the persistent
// /tmp a spawner process used.
func TestCanonicalTmuxTmpDir_IgnoresAmbientTMPDIR(t *testing.T) {
	other := t.TempDir()
	tmpWant, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatalf("resolving /tmp: %v", err)
	}

	t.Setenv("TMUX_TMPDIR", "")
	t.Setenv("TMPDIR", other)

	got := CanonicalTmuxTmpDir()
	if got == other {
		t.Fatalf("CanonicalTmuxTmpDir followed ambient TMPDIR (%q) — this is the exact ga-mwylzp bug (a fresh/unrelated TMPDIR silently strands the caller)", other)
	}
	if got != tmpWant {
		t.Errorf("got %q, want %q (the stable /tmp default)", got, tmpWant)
	}
}

// TestCanonicalTmuxTmpDir_ExplicitTMUXTMPDIRWins confirms the one override
// test isolation (test/tmuxtest.Guard) depends on — an explicit TMUX_TMPDIR
// — still takes effect, resolved through symlinks.
func TestCanonicalTmuxTmpDir_ExplicitTMUXTMPDIRWins(t *testing.T) {
	realDir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("symlinks not supported on this filesystem: %v", err)
	}
	realResolved, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatalf("resolving real dir: %v", err)
	}

	t.Setenv("TMPDIR", "/should/not/matter")

	t.Setenv("TMUX_TMPDIR", realDir)
	if got := CanonicalTmuxTmpDir(); got != realResolved {
		t.Errorf("real dir: got %q, want %q", got, realResolved)
	}

	t.Setenv("TMUX_TMPDIR", alias)
	if got := CanonicalTmuxTmpDir(); got != realResolved {
		t.Errorf("symlink alias: got %q, want %q (should resolve to the same canonical dir as the real path)", got, realResolved)
	}
}

// TestCanonicalTmuxTmpDir_MissingDirFallsBackToRaw confirms a not-yet-created
// explicit TMUX_TMPDIR (the common case before tmux has ever created
// tmux-$UID/ under it) doesn't error out — it falls back to the raw,
// unresolved value.
func TestCanonicalTmuxTmpDir_MissingDirFallsBackToRaw(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv("TMUX_TMPDIR", missing)
	if got := CanonicalTmuxTmpDir(); got != missing {
		t.Errorf("got %q, want raw fallback %q", got, missing)
	}
}

// TestSubprocessEnv_ReplacesStaleTMUXTMPDIR confirms exactly one TMUX_TMPDIR
// entry reaches the child, with the canonical value — not left to
// implementation-defined duplicate-key precedence.
func TestSubprocessEnv_ReplacesStaleTMUXTMPDIR(t *testing.T) {
	canonical := t.TempDir()
	t.Setenv("TMUX_TMPDIR", canonical)

	env := SubprocessEnv()

	var matches []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMUX_TMPDIR=") {
			matches = append(matches, kv)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 TMUX_TMPDIR entry, got %d: %v", len(matches), matches)
	}
	want := "TMUX_TMPDIR=" + CanonicalTmuxTmpDir()
	if matches[0] != want {
		t.Errorf("got %q, want %q", matches[0], want)
	}
}

// TestSubprocessEnv_PreservesOtherVars confirms the rest of the environment
// passes through untouched (only TMUX_TMPDIR is filtered/replaced).
func TestSubprocessEnv_PreservesOtherVars(t *testing.T) {
	t.Setenv("GC_TEST_MARKER_XYZ", "present")
	env := SubprocessEnv()
	found := false
	for _, kv := range env {
		if kv == "GC_TEST_MARKER_XYZ=present" {
			found = true
			break
		}
	}
	if !found {
		t.Error("SubprocessEnv dropped an unrelated environment variable")
	}
}
