//go:build !darwin

package main

import "testing"

// clearImmutableForTest is unreachable on non-Darwin at runtime (every test
// that calls it first skips via skipIfImmutabilityUnsupported), but must
// still exist so the package compiles on every platform.
func clearImmutableForTest(t *testing.T, _ string) {
	t.Helper()
}
