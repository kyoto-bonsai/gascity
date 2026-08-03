package doctor

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// --- isAncestor ---

func TestIsAncestor_TrueWhenAncestor(t *testing.T) {
	remote := initBareableRemote(t)
	commitDatedFile(t, remote, "seed.txt", time.Now())
	local := cloneRepo(t, remote)
	head := strings.TrimSpace(mustRunGit(t, local, "rev-parse", "HEAD"))

	if err := isAncestor(mustGitBin(t), local, head, "HEAD"); err != nil {
		t.Fatalf("isAncestor(HEAD, HEAD) = %v, want nil (a commit is its own ancestor)", err)
	}
}

// TestIsAncestor_FalseWhenNotAncestor is the ga-jpvkyz positive control at
// the helper level: a valid commit object that genuinely shares no history
// with the target ref must classify as errNotAncestor, not merely "error".
func TestIsAncestor_FalseWhenNotAncestor(t *testing.T) {
	remote := initBareableRemote(t)
	commitDatedFile(t, remote, "seed.txt", time.Now())
	local := cloneRepo(t, remote)

	other := initBareableRemote(t)
	commitDatedFile(t, other, "other.txt", time.Now())
	// Fetch by BRANCH NAME, not raw SHA — fetching a bare SHA from a
	// local-path remote fails with "couldn't find remote ref"
	// (reference_gascity_src_dev_gotchas, ga-vtv442 update).
	runGitForLineageTest(t, local, "fetch", other, "main:refs/heads/unrelated")
	otherHead := strings.TrimSpace(mustRunGit(t, local, "rev-parse", "refs/heads/unrelated"))

	err := isAncestor(mustGitBin(t), local, otherHead, "HEAD")
	if !errors.Is(err, errNotAncestor) {
		t.Fatalf("isAncestor = %v, want errNotAncestor", err)
	}
}

func TestIsAncestor_UnresolvableWhenCommitAbsent(t *testing.T) {
	remote := initBareableRemote(t)
	commitDatedFile(t, remote, "seed.txt", time.Now())
	local := cloneRepo(t, remote)

	err := isAncestor(mustGitBin(t), local, "deadbeef00deadbeef00deadbeef00deadbeef0", "HEAD")
	if !errors.Is(err, errCommitUnresolvable) {
		t.Fatalf("isAncestor = %v, want errCommitUnresolvable", err)
	}
}

// --- buildCommitOf ---

func TestBuildCommitOf_PrefersLdflagsMainCommit(t *testing.T) {
	out := "/opt/homebrew/bin/gc: go1.26.5\n" +
		"\tbuild\t-ldflags=\"-X main.version=v1 -X main.commit=b3c8d97d2 -X main.date=2026-08-02T19:37:33Z\"\n" +
		"\tbuild\tvcs.revision=deadbeef\n"
	commit, err := buildCommitOf(func(string) (string, error) { return out, nil }, "/opt/homebrew/bin/gc")
	if err != nil {
		t.Fatalf("buildCommitOf() error = %v", err)
	}
	if commit != "b3c8d97d2" {
		t.Errorf("commit = %q, want b3c8d97d2 (ldflags preferred over vcs.revision)", commit)
	}
}

func TestBuildCommitOf_FallsBackToVCSRevision(t *testing.T) {
	out := "/some/bin: go1.26.5\n\tbuild\tvcs.revision=cafef00d\n"
	commit, err := buildCommitOf(func(string) (string, error) { return out, nil }, "/some/bin")
	if err != nil {
		t.Fatalf("buildCommitOf() error = %v", err)
	}
	if commit != "cafef00d" {
		t.Errorf("commit = %q, want cafef00d", commit)
	}
}

func TestBuildCommitOf_TrimsDirtySuffix(t *testing.T) {
	out := "\tbuild\t-ldflags=\"-X main.commit=abc123-dirty\"\n"
	commit, err := buildCommitOf(func(string) (string, error) { return out, nil }, "/bin")
	if err != nil {
		t.Fatalf("buildCommitOf() error = %v", err)
	}
	if commit != "abc123" {
		t.Errorf("commit = %q, want abc123 (dirty suffix trimmed)", commit)
	}
}

func TestBuildCommitOf_NoCommitFound_Errors(t *testing.T) {
	out := "/bin: go1.26.5\n\tpath\tsomething\n"
	if _, err := buildCommitOf(func(string) (string, error) { return out, nil }, "/bin"); err == nil {
		t.Error("buildCommitOf() error = nil, want error when no commit stamp present")
	}
}

func TestBuildCommitOf_GoVersionMFails_PropagatesError(t *testing.T) {
	wantErr := errors.New("boom")
	_, err := buildCommitOf(func(string) (string, error) { return "", wantErr }, "/bin")
	if !errors.Is(err, wantErr) {
		t.Errorf("buildCommitOf() error = %v, want %v", err, wantErr)
	}
}

func mustGitBin(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	return bin
}

func mustRunGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGitCommand(mustGitBin(t), dir, args...)
	if err != nil {
		t.Fatalf("git %s (dir=%s): %v", strings.Join(args, " "), dir, err)
	}
	return out
}

// --- Run() integration: the actual ga-jpvkyz remediation ---

// TestBuildLineageStalenessCheck_BinaryDiverged_BlocksIndependentOfStaleness
// is the bead's own positive control: checkout is FRESH (would otherwise be
// StatusOK) but the running binary's build commit shares no history with
// it. This is the exact ga-19easp signature this remediation exists to
// catch, and must block regardless of how fresh the checkout looks.
func TestBuildLineageStalenessCheck_BinaryDiverged_BlocksIndependentOfStaleness(t *testing.T) {
	remote := initBareableRemote(t)
	commitDatedFile(t, remote, "seed.txt", time.Now())
	local := cloneRepo(t, remote)

	other := initBareableRemote(t)
	commitDatedFile(t, other, "other.txt", time.Now())
	runGitForLineageTest(t, local, "fetch", other, "main:refs/heads/unrelated")
	unrelatedHead := strings.TrimSpace(mustRunGit(t, local, "rev-parse", "refs/heads/unrelated"))

	c := NewBuildLineageStalenessCheck()
	c.sourceRepoPath = local
	c.now = func() time.Time { return time.Now() }
	c.binaryPath = func() (string, error) { return "/fake/gc", nil }
	c.goVersionM = func(string) (string, error) {
		return "\tbuild\t-ldflags=\"-X main.commit=" + unrelatedHead + "\"\n", nil
	}

	r := c.Run(&CheckContext{})

	if r.Status != StatusError {
		t.Fatalf("status = %d (%s), want StatusError — checkout is fresh but binary diverged", r.Status, r.Message)
	}
	if r.Severity != SeverityBlocking {
		t.Errorf("severity = %d, want SeverityBlocking", r.Severity)
	}
	if !strings.Contains(r.Message, "diverged") {
		t.Errorf("message = %q, want mention of diverged lineage", r.Message)
	}
	// Positional, not just substring-presence: catches the field-order class
	// of bug (binary path and commit SHA swapped into the wrong slot) that a
	// bare Contains() check on each value independently cannot detect, since
	// both values are still present somewhere in the message either way.
	if !strings.Contains(r.Message, "running binary /fake/gc") {
		t.Errorf("message = %q, want \"running binary /fake/gc\" (binary path in the binary slot)", r.Message)
	}
	if !strings.Contains(r.Message, "build commit "+shortSHA(unrelatedHead)) {
		t.Errorf("message = %q, want \"build commit %s\" (commit SHA in the commit slot)", r.Message, shortSHA(unrelatedHead))
	}
}

// TestBuildLineageStalenessCheck_BinaryIsAncestor_OKWithProvenanceConfirmed
// is the paired negative control: binary commit IS part of the checkout's
// history, so the existing staleness verdict stands and Details records
// that provenance was actually confirmed, not merely unexamined.
func TestBuildLineageStalenessCheck_BinaryIsAncestor_OKWithProvenanceConfirmed(t *testing.T) {
	remote := initBareableRemote(t)
	commitDatedFile(t, remote, "seed.txt", time.Now())
	local := cloneRepo(t, remote)
	head := strings.TrimSpace(mustRunGit(t, local, "rev-parse", "HEAD"))

	c := NewBuildLineageStalenessCheck()
	c.sourceRepoPath = local
	c.now = func() time.Time { return time.Now() }
	c.binaryPath = func() (string, error) { return "/fake/gc", nil }
	c.goVersionM = func(string) (string, error) {
		return "\tbuild\t-ldflags=\"-X main.commit=" + head + "\"\n", nil
	}

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if !anyDetailContains(r.Details, "provenance confirmed") {
		t.Errorf("Details = %v, want a provenance-confirmed detail", r.Details)
	}
}

// TestBuildLineageStalenessCheck_BinaryCommitUnresolvable_DoesNotBlockOnItsOwn
// covers the "cannot verify" branch distinctly from "verified diverged":
// an unresolvable build commit (only present in some other local clone's
// object store, per reference_gascity_src_dev_gotchas) must not itself
// produce a false blocking alarm.
func TestBuildLineageStalenessCheck_BinaryCommitUnresolvable_DoesNotBlockOnItsOwn(t *testing.T) {
	remote := initBareableRemote(t)
	commitDatedFile(t, remote, "seed.txt", time.Now())
	local := cloneRepo(t, remote)

	c := NewBuildLineageStalenessCheck()
	c.sourceRepoPath = local
	c.now = func() time.Time { return time.Now() }
	c.binaryPath = func() (string, error) { return "/fake/gc", nil }
	c.goVersionM = func(string) (string, error) {
		return "\tbuild\t-ldflags=\"-X main.commit=deadbeef00deadbeef00deadbeef00deadbeef0\"\n", nil
	}

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK — fresh checkout, unresolvable provenance must not block on its own", r.Status, r.Message)
	}
	if !anyDetailContains(r.Details, "NOT verified") {
		t.Errorf("Details = %v, want a not-verified provenance detail", r.Details)
	}
}

// TestBuildLineageStalenessCheck_NoBuildCommit_SkipsProvenanceGracefully
// covers a dev build with no embedded stamps at all — must degrade
// gracefully, same as every other "precondition not met" branch.
func TestBuildLineageStalenessCheck_NoBuildCommit_SkipsProvenanceGracefully(t *testing.T) {
	remote := initBareableRemote(t)
	commitDatedFile(t, remote, "seed.txt", time.Now())
	local := cloneRepo(t, remote)

	c := NewBuildLineageStalenessCheck()
	c.sourceRepoPath = local
	c.now = func() time.Time { return time.Now() }
	c.binaryPath = func() (string, error) { return "/fake/gc", nil }
	c.goVersionM = func(string) (string, error) { return "dev build, no stamps", nil }

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if !anyDetailContains(r.Details, "not checked") {
		t.Errorf("Details = %v, want a not-checked provenance detail", r.Details)
	}
}

func anyDetailContains(details []string, substr string) bool {
	for _, d := range details {
		if strings.Contains(d, substr) {
			return true
		}
	}
	return false
}
