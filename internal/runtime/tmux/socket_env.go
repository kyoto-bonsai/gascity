package tmux

import (
	"os"
	"path/filepath"
	"strings"
)

// CanonicalTmuxTmpDir returns the directory tmux uses as the base for its
// per-user socket directory (tmux-$UID/), pinned to a location that is
// stable across reboots and consistent regardless of which process asks.
//
// tmux itself resolves this from TMUX_TMPDIR, falling back to TMPDIR, then
// "/tmp". That TMPDIR fallback is the actual mechanism behind ga-mwylzp: on
// macOS, TMPDIR is a per-process, per-boot EPHEMERAL directory that the OS
// wipes clean on every reboot — a launchd-spawned daemon inherits one value,
// a plain shell invocation may see another (or none). Pre-reboot, a
// TMPDIR-based and a /tmp-based tmux server can both happen to exist, so the
// split stays latent; post-reboot, TMPDIR is freshly empty while /tmp
// (persistent) still holds whatever the spawner created, and any caller that
// fell through to TMPDIR — like the observer's state-cache refresh — finds
// nothing and reports every session dead.
//
// (A plausible-sounding alternative theory — that a symlinked TMUX_TMPDIR
// gets rejected by tmux — does NOT reproduce empirically: connect(2)/open(2)
// resolve symlink path components at the kernel level regardless of
// Go-level string handling, the same finding ga-ulroht made for the
// controller's own socket. This fix does not depend on that theory.)
//
// Fix: only honor an EXPLICIT TMUX_TMPDIR (what test/tmuxtest.Guard sets for
// sandboxed test isolation); never fall through to ambient TMPDIR. The
// remaining default, "/tmp", is resolved through symlinks as defense in
// depth against a future divergent spelling (e.g. /tmp vs /private/tmp) —
// verified NOT to be this bug's mechanism, but still worth removing as a
// class, same reasoning as ga-ulroht's own hygiene fix.
func CanonicalTmuxTmpDir() string {
	if dir := os.Getenv("TMUX_TMPDIR"); dir != "" {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return resolved
		}
		return dir
	}
	if resolved, err := filepath.EvalSymlinks("/tmp"); err == nil {
		return resolved
	}
	return "/tmp"
}

// SubprocessEnv returns the environment to use for a tmux (or tmux-attaching,
// e.g. `script`) subprocess: the current process's environment, with any
// inherited TMUX_TMPDIR replaced by the canonical value from
// CanonicalTmuxTmpDir — and, critically, with no ambient TMPDIR able to
// influence tmux's own socket-directory fallback, since replacing
// (unconditionally stamping TMUX_TMPDIR) rather than appending pins the
// child's resolution regardless of what either variable held in the parent's
// environment.
func SubprocessEnv() []string {
	base := os.Environ()
	env := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, "TMUX_TMPDIR=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "TMUX_TMPDIR="+CanonicalTmuxTmpDir())
}
