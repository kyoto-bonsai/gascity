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

// TestBuildLineageStalenessCheck_BinaryCommitUnresolvable_WarnsVisiblyNotGreen
// is ga-c7np1n's regression target. The commit is unresolvable ANYWHERE
// (not present locally, not fetchable from the sole configured remote,
// which genuinely never had it — a fabricated SHA), so the fetch-attempt
// must fail gracefully and the result must be a VISIBLE, non-OK status with
// "NOT verified" in Message itself (not only Details, which normal doctor
// output never prints) — this is the exact defect the old version of this
// test certified as correct by asserting StatusOK here.
func TestBuildLineageStalenessCheck_BinaryCommitUnresolvable_WarnsVisiblyNotGreen(t *testing.T) {
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

	if r.Status == StatusOK {
		t.Fatalf("status = StatusOK (%s) — MUST NOT be OK when binary provenance cannot be verified (ga-c7np1n: fail-closed, not a passing state)", r.Message)
	}
	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %d, want SeverityAdvisory — uncertain is not the same as confirmed-bad, so this must not block", r.Severity)
	}
	if !strings.Contains(r.Message, "NOT verified") {
		t.Fatalf("message = %q, want \"NOT verified\" IN THE MESSAGE ITSELF — Details alone is the exact defect this test regresses", r.Message)
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

// --- attemptFetchCommit (ga-c7np1n) ---

// TestAttemptFetchCommit_SucceedsWhenRemoteHasIt deliberately uses TWO
// separate, fully-connected (non-shallow) remotes rather than a shallow
// clone: a plain bare-SHA fetch into a shallow clone brings the OBJECT in
// but does not reconnect it into the ancestor graph (verified by hand —
// cat-file -e succeeds, merge-base --is-ancestor still fails, because the
// shallow boundary isn't deepened by a single-commit fetch). This fleet's
// real checkouts are always full clones (no --depth anywhere in its
// documented workflow), so the realistic "unresolvable" shape is "no
// configured remote has been asked for this ref yet," not "history is
// shallow" — two full remotes, one with the commit, models that correctly.
func TestAttemptFetchCommit_SucceedsWhenRemoteHasIt(t *testing.T) {
	remote := initBareableRemote(t)
	commitDatedFile(t, remote, "seed.txt", time.Now())
	local := cloneRepo(t, remote)

	other := initBareableRemote(t)
	commitDatedFile(t, other, "other.txt", time.Now())
	otherHead := strings.TrimSpace(mustRunGit(t, other, "rev-parse", "HEAD"))
	runGitForLineageTest(t, local, "remote", "add", "other", other)

	if err := isAncestor(mustGitBin(t), local, otherHead, "HEAD"); !errors.Is(err, errCommitUnresolvable) {
		t.Fatalf("precondition failed: commit already resolvable before fetch (err=%v)", err)
	}

	if err := attemptFetchCommit(mustGitBin(t), local, otherHead); err != nil {
		t.Fatalf("attemptFetchCommit() = %v, want nil — origin lacks it but the 'other' remote has it", err)
	}
}

func TestAttemptFetchCommit_FailsWhenNoRemoteHasIt(t *testing.T) {
	remote := initBareableRemote(t)
	commitDatedFile(t, remote, "seed.txt", time.Now())
	local := cloneRepo(t, remote)

	err := attemptFetchCommit(mustGitBin(t), local, "deadbeef00deadbeef00deadbeef00deadbeef0")
	if err == nil {
		t.Error("attemptFetchCommit() = nil, want error — no configured remote has this fabricated commit")
	}
}

func TestAttemptFetchCommit_FailsWhenNoRemotesConfigured(t *testing.T) {
	dir := t.TempDir()
	runGitForLineageTest(t, dir, "init", "-b", "main")
	runGitForLineageTest(t, dir, "config", "user.name", "Lineage Test")
	runGitForLineageTest(t, dir, "config", "user.email", "lineage-test@example.invalid")

	err := attemptFetchCommit(mustGitBin(t), dir, "deadbeef00deadbeef00deadbeef00deadbeef0")
	if err == nil {
		t.Error("attemptFetchCommit() = nil, want error — repo has no remotes at all")
	}
}

// TestBuildLineageStalenessCheck_UnresolvableCommitFetchedThenDiverged_Blocks
// is the end-to-end proof that Run() actually wires attemptFetchCommit into
// the ancestor check, not just that the two pieces work in isolation: the
// build commit starts genuinely absent (not on "origin" at all), Run()
// itself must try the second configured remote ("other") to resolve it,
// and — the realistic outcome per the discovery above (a full local clone
// can never be missing a true ancestor of its own HEAD; the only way it
// can be missing a commit is that commit living on a lineage the checkout
// never fetched at all) — correctly lands on diverged/blocking, exactly
// like the live case this whole bead is about.
func TestBuildLineageStalenessCheck_UnresolvableCommitFetchedThenDiverged_Blocks(t *testing.T) {
	remote := initBareableRemote(t)
	commitDatedFile(t, remote, "seed.txt", time.Now())
	local := cloneRepo(t, remote)

	other := initBareableRemote(t)
	commitDatedFile(t, other, "other.txt", time.Now())
	otherHead := strings.TrimSpace(mustRunGit(t, other, "rev-parse", "HEAD"))
	runGitForLineageTest(t, local, "remote", "add", "other", other)

	if err := isAncestor(mustGitBin(t), local, otherHead, "HEAD"); !errors.Is(err, errCommitUnresolvable) {
		t.Fatalf("precondition failed: commit already resolvable before Run() (err=%v)", err)
	}

	c := NewBuildLineageStalenessCheck()
	c.sourceRepoPath = local
	c.now = func() time.Time { return time.Now() }
	c.binaryPath = func() (string, error) { return "/fake/gc", nil }
	c.goVersionM = func(string) (string, error) {
		return "\tbuild\t-ldflags=\"-X main.commit=" + otherHead + "\"\n", nil
	}

	r := c.Run(&CheckContext{})

	if r.Status != StatusError {
		t.Fatalf("status = %d (%s), want StatusError — Run() should have fetched the commit via the 'other' remote and found it diverged", r.Status, r.Message)
	}
	if r.Severity != SeverityBlocking {
		t.Errorf("severity = %d, want SeverityBlocking", r.Severity)
	}
	if !strings.Contains(r.Message, "diverged") {
		t.Errorf("message = %q, want mention of diverged lineage", r.Message)
	}
}

// TestBuildLineageStalenessCheck_StaleAndUnverified_ErrorMentionsBoth covers
// the new combined branch: checkout is ALSO genuinely stale, not just
// unverified. The more severe, more specific finding (real staleness) must
// stay StatusError/Blocking (not get diluted into a mere warning), while
// still surfacing the provenance gap in the same message rather than
// silently dropping it.
func TestBuildLineageStalenessCheck_StaleAndUnverified_ErrorMentionsBoth(t *testing.T) {
	remote := initBareableRemote(t)
	commitDatedFile(t, remote, "seed.txt", time.Now().Add(-time.Hour))
	local := cloneRepo(t, remote)
	for i := 0; i < defaultBuildLineageMaxCommits+5; i++ {
		commitDatedFile(t, remote, "upstream.txt", time.Now())
	}
	runGitForLineageTest(t, local, "fetch", "origin")

	c := NewBuildLineageStalenessCheck()
	c.sourceRepoPath = local
	c.now = func() time.Time { return time.Now() }
	c.binaryPath = func() (string, error) { return "/fake/gc", nil }
	c.goVersionM = func(string) (string, error) {
		return "\tbuild\t-ldflags=\"-X main.commit=deadbeef00deadbeef00deadbeef00deadbeef0\"\n", nil
	}

	r := c.Run(&CheckContext{})

	if r.Status != StatusError {
		t.Fatalf("status = %d (%s), want StatusError", r.Status, r.Message)
	}
	if r.Severity != SeverityBlocking {
		t.Errorf("severity = %d, want SeverityBlocking — genuine staleness must not be diluted to advisory", r.Severity)
	}
	if !strings.Contains(r.Message, "commits behind") {
		t.Errorf("message = %q, want the staleness numbers still present", r.Message)
	}
	if !strings.Contains(r.Message, "NOT verified") {
		t.Errorf("message = %q, want the provenance gap also surfaced, not silently dropped", r.Message)
	}
}
