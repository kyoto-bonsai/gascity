//go:build darwin

package main

import "testing"

// clearImmutableForTest clears UF_IMMUTABLE on path (real file or symlink
// name — chflags -h nouchg is safe to run on a non-symlink too) so t.TempDir
// cleanup doesn't fail closed against its own fixtures. Test-only: the
// check's real implementation never clears a flag, only reads and re-arms
// one (Fix only ever arms).
func clearImmutableForTest(t *testing.T, path string) {
	t.Helper()
	if err := runChflags("-h", "nouchg", path); err != nil {
		t.Logf("clearImmutableForTest(%s): %v (non-fatal, best-effort cleanup)", path, err)
	}
}
