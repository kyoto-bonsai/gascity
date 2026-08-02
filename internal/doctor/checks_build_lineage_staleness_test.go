package doctor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBuildLineageStalenessCheck_FreshLineage_OK builds a local clone whose
// HEAD equals a just-fetched origin/main (zero commits behind, merge-base
// dated "now") and asserts StatusOK.
func TestBuildLineageStalenessCheck_FreshLineage_OK(t *testing.T) {
	remote := initBareableRemote(t)
	commitDatedFile(t, remote, "seed.txt", time.Now().Add(-time.Hour))
	local := cloneRepo(t, remote)

	c := NewBuildLineageStalenessCheck()
	c.sourceRepoPath = local
	c.now = func() time.Time { return time.Now() }

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %d, want SeverityAdvisory on OK", r.Severity)
	}
	if !strings.Contains(r.Message, "0 commits behind") {
		t.Errorf("message = %q, want 0 commits behind", r.Message)
	}
}

// TestBuildLineageStalenessCheck_StaleByCommitCount_Errors simulates the
// ga-tk5mcg.11 shape: local HEAD did not move, origin/main advanced past
// the commit-count threshold. This is the positive control this bead's own
// acceptance floor demands (break the precondition, confirm the guard fires).
func TestBuildLineageStalenessCheck_StaleByCommitCount_Errors(t *testing.T) {
	remote := initBareableRemote(t)
	commitDatedFile(t, remote, "seed.txt", time.Now().Add(-time.Hour))
	local := cloneRepo(t, remote)

	// Upstream advances past the threshold; local never fetches HEAD forward.
	for i := 0; i < defaultBuildLineageMaxCommits+5; i++ {
		commitDatedFile(t, remote, "upstream.txt", time.Now())
	}
	runGitForLineageTest(t, local, "fetch", "origin")

	c := NewBuildLineageStalenessCheck()
	c.sourceRepoPath = local
	c.now = func() time.Time { return time.Now() }

	r := c.Run(&CheckContext{})

	if r.Status != StatusError {
		t.Fatalf("status = %d (%s), want StatusError", r.Status, r.Message)
	}
	if r.Severity != SeverityBlocking {
		t.Errorf("severity = %d, want SeverityBlocking", r.Severity)
	}
	if !strings.Contains(r.Message, "commits behind") {
		t.Errorf("message = %q, want commit-count mention", r.Message)
	}
	if r.FixHint == "" || !strings.Contains(r.FixHint, "LIVE base") {
		t.Errorf("FixHint = %q, want remediation citing the live-base fold pattern, not origin/main merge", r.FixHint)
	}
}

// TestBuildLineageStalenessCheck_StaleByAge_Errors holds commit count at 1
// (well under threshold) but backdates the shared commit past maxAge —
// isolates the age trigger from the count trigger.
func TestBuildLineageStalenessCheck_StaleByAge_Errors(t *testing.T) {
	remote := initBareableRemote(t)
	old := time.Now().Add(-(defaultBuildLineageMaxAge + 48*time.Hour))
	commitDatedFile(t, remote, "seed.txt", old)
	local := cloneRepo(t, remote)
	// One upstream commit so origin/main != local HEAD, well under the count threshold.
	commitDatedFile(t, remote, "upstream.txt", time.Now())
	runGitForLineageTest(t, local, "fetch", "origin")

	c := NewBuildLineageStalenessCheck()
	c.sourceRepoPath = local
	c.now = func() time.Time { return time.Now() }

	r := c.Run(&CheckContext{})

	if r.Status != StatusError {
		t.Fatalf("status = %d (%s), want StatusError", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "1 commits behind") {
		t.Errorf("message = %q, want exactly 1 commit behind (isolating the age trigger)", r.Message)
	}
	if !strings.Contains(r.Message, "old") {
		t.Errorf("message = %q, want age mention", r.Message)
	}
}

// TestBuildLineageStalenessCheck_UnderBothThresholds_OK is the negative
// control paired with the two Errors tests above: small drift, recent date.
func TestBuildLineageStalenessCheck_UnderBothThresholds_OK(t *testing.T) {
	remote := initBareableRemote(t)
	commitDatedFile(t, remote, "seed.txt", time.Now().Add(-time.Hour))
	local := cloneRepo(t, remote)
	for i := 0; i < 3; i++ {
		commitDatedFile(t, remote, "upstream.txt", time.Now())
	}
	runGitForLineageTest(t, local, "fetch", "origin")

	c := NewBuildLineageStalenessCheck()
	c.sourceRepoPath = local
	c.now = func() time.Time { return time.Now() }

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK (3 commits / ~1h old, both under threshold)", r.Status, r.Message)
	}
}

func TestBuildLineageStalenessCheck_SourceRepoMissing_WarnsAdvisory(t *testing.T) {
	c := NewBuildLineageStalenessCheck()
	c.sourceRepoPath = filepath.Join(t.TempDir(), "does-not-exist")

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %d, want SeverityAdvisory — an absent source checkout must not block doctor", r.Severity)
	}
	if !strings.Contains(r.Message, "GC_SRC_PATH") {
		t.Errorf("message = %q, want GC_SRC_PATH remediation hint", r.Message)
	}
}

func TestBuildLineageStalenessCheck_NoOriginRemote_WarnsAdvisory(t *testing.T) {
	dir := t.TempDir()
	runGitForLineageTest(t, dir, "init", "-b", "main")
	runGitForLineageTest(t, dir, "config", "user.name", "Lineage Test")
	runGitForLineageTest(t, dir, "config", "user.email", "lineage-test@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitForLineageTest(t, dir, "add", "f.txt")
	runGitForLineageTest(t, dir, "commit", "-m", "no remote configured")

	c := NewBuildLineageStalenessCheck()
	c.sourceRepoPath = dir

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %d, want SeverityAdvisory", r.Severity)
	}
}

func TestBuildLineageStalenessCheck_GitUnavailable_WarnsAdvisory(t *testing.T) {
	c := NewBuildLineageStalenessCheck()
	c.sourceRepoPath = t.TempDir()
	c.gitPath = func(string) (string, error) { return "", errors.New("git unavailable") }

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %d, want SeverityAdvisory", r.Severity)
	}
}

func TestResolveSourceRepoPath_EnvOverride(t *testing.T) {
	t.Setenv("GC_SRC_PATH", "/custom/src/path")
	if got := resolveSourceRepoPath(); got != "/custom/src/path" {
		t.Errorf("resolveSourceRepoPath() = %q, want /custom/src/path", got)
	}
}

func TestResolveSourceRepoPath_DefaultsUnderHome(t *testing.T) {
	t.Setenv("GC_SRC_PATH", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir in this environment: %v", err)
	}
	want := filepath.Join(home, "personal", "github", "gascity-src")
	if got := resolveSourceRepoPath(); got != want {
		t.Errorf("resolveSourceRepoPath() = %q, want %q", got, want)
	}
}

// TestBuildLineageStalenessCheck_IgnoresPoisonedGitEnv mirrors
// TestRigRootBranchCheck_IgnoresPoisonedGitEnv: runGitCommand's
// git.SanitizedEnv() must protect this check the same way, since it reuses
// the identical helper.
func TestBuildLineageStalenessCheck_IgnoresPoisonedGitEnv(t *testing.T) {
	remote := initBareableRemote(t)
	commitDatedFile(t, remote, "seed.txt", time.Now().Add(-time.Hour))
	local := cloneRepo(t, remote)
	for i := 0; i < 3; i++ {
		commitDatedFile(t, remote, "upstream.txt", time.Now())
	}
	runGitForLineageTest(t, local, "fetch", "origin")

	poison := initBareableRemote(t)
	commitDatedFile(t, poison, "poison.txt", time.Now())
	t.Setenv("GIT_DIR", filepath.Join(poison, ".git"))
	t.Setenv("GIT_WORK_TREE", poison)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(poison, ".git", "index"))

	c := NewBuildLineageStalenessCheck()
	c.sourceRepoPath = local
	c.now = func() time.Time { return time.Now() }

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK read from local, not poisoned repo", r.Status, r.Message)
	}
}

func TestBuildLineageStalenessCheck_CanFixAndWarmup(t *testing.T) {
	c := NewBuildLineageStalenessCheck()
	if c.CanFix() {
		t.Error("CanFix() = true, want false — reconciling a diverged lineage needs engineering judgment")
	}
	if c.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false")
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Errorf("Fix() = %v, want nil no-op", err)
	}
}

// --- test helpers ---

func initBareableRemote(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	dir := t.TempDir()
	runGitForLineageTest(t, dir, "init", "-b", "main")
	runGitForLineageTest(t, dir, "config", "user.name", "Lineage Test")
	runGitForLineageTest(t, dir, "config", "user.email", "lineage-test@example.invalid")
	// receive.denyCurrentBranch=updateInstead lets a non-bare repo accept
	// pushes/fetches-by-clone to its checked-out branch, which is all this
	// helper needs (we only ever fetch FROM it, never push).
	runGitForLineageTest(t, dir, "config", "receive.denyCurrentBranch", "updateInstead")
	return dir
}

func cloneRepo(t *testing.T, remote string) string {
	t.Helper()
	dir := t.TempDir()
	local := filepath.Join(dir, "clone")
	runGitForLineageTest(t, dir, "clone", remote, local)
	runGitForLineageTest(t, local, "config", "user.name", "Lineage Test")
	runGitForLineageTest(t, local, "config", "user.email", "lineage-test@example.invalid")
	return local
}

// commitDatedFile writes a uniquely-named file and commits it with both
// author and committer date pinned to when, so Run()'s `%ct`-based age math
// is deterministic rather than racing wall-clock "now".
func commitDatedFile(t *testing.T, dir, name string, when time.Time) {
	t.Helper()
	path := filepath.Join(dir, name+"."+time.Now().Format("150405.000000000"))
	if err := os.WriteFile(path, []byte("content\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	cmd := exec.Command("git", "add", filepath.Base(path))
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	dateStr := when.Format(time.RFC3339)
	cmd = exec.Command("git", "commit", "-m", "commit "+filepath.Base(path))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE="+dateStr,
		"GIT_COMMITTER_DATE="+dateStr,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

func runGitForLineageTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (dir=%s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}
