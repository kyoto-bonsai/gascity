package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
)

// skipIfImmutabilityUnsupported lets these tests compile and run everywhere
// (matching the check's own graceful-skip contract) while only asserting
// real chflags behavior on a platform that implements it — this fleet's
// Darwin hosts.
func skipIfImmutabilityUnsupported(t *testing.T) {
	t.Helper()
	if _, err := realBinaryIsImmutable(os.DevNull); isPlatformUnsupported(err) {
		t.Skip("no immutable-flag implementation on this platform")
	}
}

// setupProtectedTarget builds a real+protected target (both prongs armed)
// under dir, mirroring gc_safe_swap_install's own sequence, and returns the
// target path. t.Cleanup unwinds the flags so TempDir removal doesn't fail
// closed (mirrors gc_safe_swap_cleanup_old's own "clear before rm" step).
func setupProtectedTarget(t *testing.T, dir string) string {
	t.Helper()
	realPath := filepath.Join(dir, "gc-realbin")
	if err := os.WriteFile(realPath, []byte("v1"), 0o755); err != nil {
		t.Fatalf("write real binary: %v", err)
	}
	if err := armRealBinaryImmutable(realPath); err != nil {
		t.Fatalf("arm real binary: %v", err)
	}
	target := filepath.Join(dir, "gc")
	if err := os.Symlink(realPath, target); err != nil {
		t.Fatalf("symlink target: %v", err)
	}
	if err := armNameImmutable(target); err != nil {
		t.Fatalf("arm symlink name: %v", err)
	}
	t.Cleanup(func() {
		clearImmutableForTest(t, realPath)
		clearImmutableForTest(t, target)
	})
	return target
}

func newFixedCheck(targets []string, runningCommit string) *binaryImmutabilityCheck {
	return &binaryImmutabilityCheck{
		targets:       func() []string { return targets },
		runningCommit: func() string { return runningCommit },
	}
}

func TestBinaryImmutabilityCheck_NoTargetsSkips(t *testing.T) {
	c := newFixedCheck(nil, "abc123")
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK (no targets is not a gap)", r.Status)
	}
	if !strings.Contains(strings.ToLower(r.Message), "skip") {
		t.Fatalf("message should indicate skipped, got %q", r.Message)
	}
}

func TestBinaryImmutabilityCheck_FullyProtectedAndProvenanceCurrent_OK(t *testing.T) {
	skipIfImmutabilityUnsupported(t)
	dir := t.TempDir()
	target := setupProtectedTarget(t, dir)
	writeProvenanceForTest(t, target, "abc123")

	c := newFixedCheck([]string{target}, "abc123")
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK, details=%v", r.Status, r.Details)
	}
	if r.Severity != doctor.SeverityAdvisory {
		t.Fatalf("severity = %v, want SeverityAdvisory (never gates)", r.Severity)
	}
	if c.CanFix() {
		t.Fatalf("CanFix() = true on a fully-protected target, want false")
	}
}

func TestBinaryImmutabilityCheck_MissingProng1_WarnsAndFixRearms(t *testing.T) {
	skipIfImmutabilityUnsupported(t)
	dir := t.TempDir()
	target := setupProtectedTarget(t, dir)
	writeProvenanceForTest(t, target, "abc123")

	// Simulate a crash that left prong 1 cleared (real file unprotected)
	// but prong 2 (the symlink name) still armed.
	realPath, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("resolve real: %v", err)
	}
	clearImmutableForTest(t, realPath)

	c := newFixedCheck([]string{target}, "abc123")
	r := c.Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning, details=%v", r.Status, r.Details)
	}
	if !containsSubstring(r.Details, "real-binary uchg") {
		t.Fatalf("details should name the missing real-binary prong, got %v", r.Details)
	}
	if !c.CanFix() {
		t.Fatalf("CanFix() = false, want true (a chflags gap is re-armable)")
	}
	if err := c.Fix(nil); err != nil {
		t.Fatalf("Fix() error: %v", err)
	}

	// Re-run (mirrors the doctor framework's own verify-after-fix behavior).
	r2 := c.Run(nil)
	if r2.Status != doctor.StatusOK {
		t.Fatalf("status after Fix = %v, want StatusOK, details=%v", r2.Status, r2.Details)
	}
}

func TestBinaryImmutabilityCheck_MissingProng2_WarnsAndFixRearms(t *testing.T) {
	skipIfImmutabilityUnsupported(t)
	dir := t.TempDir()
	target := setupProtectedTarget(t, dir)
	writeProvenanceForTest(t, target, "abc123")

	// Simulate the observed 08-10 vector: the symlink name's own flag got
	// cleared (e.g. mid-swap) while the real binary stayed protected.
	clearImmutableForTest(t, target)

	c := newFixedCheck([]string{target}, "abc123")
	r := c.Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning, details=%v", r.Status, r.Details)
	}
	if !containsSubstring(r.Details, "symlink uchg") {
		t.Fatalf("details should name the missing symlink prong, got %v", r.Details)
	}
	if err := c.Fix(nil); err != nil {
		t.Fatalf("Fix() error: %v", err)
	}
	if r2 := c.Run(nil); r2.Status != doctor.StatusOK {
		t.Fatalf("status after Fix = %v, want StatusOK, details=%v", r2.Status, r2.Details)
	}
}

func TestBinaryImmutabilityCheck_MissingProvenance_WarnsButNotFixable(t *testing.T) {
	skipIfImmutabilityUnsupported(t)
	dir := t.TempDir()
	target := setupProtectedTarget(t, dir)
	// No provenance file written.

	c := newFixedCheck([]string{target}, "abc123")
	r := c.Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning, details=%v", r.Status, r.Details)
	}
	if !containsSubstring(r.Details, "no provenance record") {
		t.Fatalf("details should call out the missing provenance record, got %v", r.Details)
	}
	if c.CanFix() {
		t.Fatalf("CanFix() = true, want false — a missing provenance record cannot be auto-fabricated")
	}
}

func TestBinaryImmutabilityCheck_ProvenanceCommitMismatch_Warns(t *testing.T) {
	skipIfImmutabilityUnsupported(t)
	dir := t.TempDir()
	target := setupProtectedTarget(t, dir)
	writeProvenanceForTest(t, target, "stale-commit")

	c := newFixedCheck([]string{target}, "current-commit")
	r := c.Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning, details=%v", r.Status, r.Details)
	}
	if !containsSubstring(r.Details, "does not match running commit") {
		t.Fatalf("details should call out the commit mismatch, got %v", r.Details)
	}
}

// TestBinaryImmutabilityCheck_SelfOnlyTargetExemptFromProvenance covers the
// ga-y275uo kieran validation finding (2026-08-11 02:50): a target that IS
// the running executable but is NOT a conventional install path (an ad hoc
// dev build, not an install) must not warn for a missing provenance record
// — it was never installed, so it will never have one, and a permanent
// guaranteed warning here is exactly the alert-fatigue risk kieran flagged.
func TestBinaryImmutabilityCheck_SelfOnlyTargetExemptFromProvenance(t *testing.T) {
	skipIfImmutabilityUnsupported(t)
	dir := t.TempDir()
	target := setupProtectedTarget(t, dir)
	// Deliberately no provenance file written — the ad hoc dev build case.

	c := &binaryImmutabilityCheck{
		targets:       func() []string { return []string{target} },
		runningCommit: func() string { return "abc123" },
		selfPath:      func() string { return target },
	}
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK — a self-only target must not warn for a missing provenance record, details=%v", r.Status, r.Details)
	}
}

// TestBinaryImmutabilityCheck_SelfPathNilStillWarnsForMissingProvenance
// guards the other direction: with selfPath unset (nil, the zero value
// every production-unrelated construction gets unless it opts in), the
// exemption must never fire — every existing test above relies on this.
func TestBinaryImmutabilityCheck_SelfPathNilStillWarnsForMissingProvenance(t *testing.T) {
	skipIfImmutabilityUnsupported(t)
	dir := t.TempDir()
	target := setupProtectedTarget(t, dir)

	c := newFixedCheck([]string{target}, "abc123")
	if c.selfPath != nil {
		t.Fatalf("newFixedCheck must leave selfPath nil (no test above sets it)")
	}
	r := c.Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning — selfPath=nil must never exempt a target, details=%v", r.Status, r.Details)
	}
}

func TestReadProvenanceCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gc.provenance.json")

	if _, ok := readProvenanceCommit(path); ok {
		t.Fatalf("readProvenanceCommit on a missing file should return ok=false")
	}

	if err := os.WriteFile(path, []byte(`{"schema_version":"1","build_commit":"deadbeef"}`), 0o644); err != nil {
		t.Fatalf("write provenance: %v", err)
	}
	c, ok := readProvenanceCommit(path)
	if !ok || c != "deadbeef" {
		t.Fatalf("readProvenanceCommit = (%q,%v), want (deadbeef,true)", c, ok)
	}

	if err := os.WriteFile(path, []byte(`not json`), 0o644); err != nil {
		t.Fatalf("write bad provenance: %v", err)
	}
	if _, ok := readProvenanceCommit(path); ok {
		t.Fatalf("readProvenanceCommit on unparseable content should return ok=false")
	}
}

func TestProvenancePathFor(t *testing.T) {
	got := provenancePathFor("/opt/homebrew/bin/gc")
	want := "/opt/homebrew/bin/gc.provenance.json"
	if got != want {
		t.Fatalf("provenancePathFor = %q, want %q", got, want)
	}
}

func TestConventionalGCBinaryPaths(t *testing.T) {
	got := conventionalGCBinaryPaths()
	if len(got) == 0 {
		t.Fatalf("conventionalGCBinaryPaths() returned empty, want at least /opt/homebrew/bin/gc")
	}
	if got[0] != "/opt/homebrew/bin/gc" {
		t.Fatalf("conventionalGCBinaryPaths()[0] = %q, want /opt/homebrew/bin/gc", got[0])
	}
	if home, _ := os.UserHomeDir(); home != "" {
		want := []string{
			"/opt/homebrew/bin/gc",
			filepath.Join(home, "go", "bin", "gc"),
			filepath.Join(home, ".local", "bin", "gc"),
		}
		if len(got) != len(want) {
			t.Fatalf("conventionalGCBinaryPaths() = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("conventionalGCBinaryPaths()[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	}
}

// TestDefaultGCBinaryTargets_IncludesExistingSelfDedupedAndSorted directly
// exercises defaultGCBinaryTargets (previously zero test coverage — every
// other test goes through newFixedCheck's overridden targets func instead,
// per kieran's ga-y275uo validation, 2026-08-11 02:50). Only asserts
// invariants that hold regardless of which conventional paths happen to
// exist on the machine running this test.
func TestDefaultGCBinaryTargets_IncludesExistingSelfDedupedAndSorted(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	got := defaultGCBinaryTargets()

	if !containsString(got, self) {
		t.Fatalf("defaultGCBinaryTargets() = %v, want it to include the running test binary %q", got, self)
	}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("defaultGCBinaryTargets() = %v, want sorted", got)
	}
	seen := make(map[string]bool, len(got))
	for _, p := range got {
		if seen[p] {
			t.Fatalf("defaultGCBinaryTargets() returned duplicate %q: %v", p, got)
		}
		seen[p] = true
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("defaultGCBinaryTargets() returned non-existent path %q: %v", p, err)
		}
	}
}

// -- test helpers --

func writeProvenanceForTest(t *testing.T, target, buildCommit string) {
	t.Helper()
	data := `{"schema_version":"1","build_commit":"` + buildCommit + `"}`
	if err := os.WriteFile(provenancePathFor(target), []byte(data), 0o644); err != nil {
		t.Fatalf("write provenance: %v", err)
	}
}

// containsSubstring is defined in split_topology_conformance_test.go (same package).
