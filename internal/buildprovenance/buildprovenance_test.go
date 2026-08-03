package buildprovenance

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordPath(t *testing.T) {
	if got := RecordPath("/opt/homebrew/bin/gc"); got != "/opt/homebrew/bin/gc.provenance.json" {
		t.Errorf("RecordPath() = %q, want suffixed alongside the binary", got)
	}
}

func TestWriteRead_RoundTrip(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "gc")
	want := Record{
		SchemaVersion:     SchemaVersion,
		BuildCommit:       "abc123",
		BuildVersion:      "v1",
		BuildDate:         "2026-08-03T00:00:00Z",
		SourceClonePath:   "/some/clone",
		OriginMainSHA:     "def456",
		MergeBase:         "789abc",
		AncestryVerdict:   VerdictAhead,
		InstallTimestamp:  "2026-08-03T00:00:01Z",
		InstallerIdentity: "test",
	}

	if err := Write(binPath, want); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	got, err := Read(binPath)
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	if got != want {
		t.Errorf("Read() = %+v, want %+v", got, want)
	}
}

func TestRead_MissingFile_ErrRecordMissing(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "gc")
	_, err := Read(binPath)
	if !errors.Is(err, ErrRecordMissing) {
		t.Fatalf("Read() error = %v, want ErrRecordMissing", err)
	}
}

func TestRead_CorruptFile_NotErrRecordMissing(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "gc")
	if err := os.WriteFile(RecordPath(binPath), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Read(binPath)
	if err == nil {
		t.Fatal("Read() error = nil, want a parse error")
	}
	if errors.Is(err, ErrRecordMissing) {
		t.Error("Read() classified a corrupt-but-present file as ErrRecordMissing — must be distinguishable (see doc comment)")
	}
}

func mustGitBinBP(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	return bin
}

func initRepoBP(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitBP(t, dir, "init", "-b", "main")
	runGitBP(t, dir, "config", "user.name", "BuildProvenance Test")
	runGitBP(t, dir, "config", "user.email", "buildprovenance-test@example.invalid")
	return dir
}

func commitFileBP(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitBP(t, dir, "add", name)
	runGitBP(t, dir, "commit", "-m", "commit "+name)
	return strings.TrimSpace(runGitOutputBP(t, dir, "rev-parse", "HEAD"))
}

func runGitBP(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s (dir=%s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func runGitOutputBP(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s (dir=%s): %v", strings.Join(args, " "), dir, err)
	}
	return string(out)
}

// TestVerify_BuildCommitIsAncestor_VerdictAncestor: build commit is an
// older point that origin/main has since moved past — the rare/ideal case
// where the build fully reflects (a point at or behind) current upstream.
func TestVerify_BuildCommitIsAncestor_VerdictAncestor(t *testing.T) {
	dir := initRepoBP(t)
	buildCommit := commitFileBP(t, dir, "old.txt")
	commitFileBP(t, dir, "newer.txt") // origin/main advances past buildCommit
	runGitBP(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")

	res, err := Verify(mustGitBinBP(t), dir, buildCommit)
	if err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	if res.AncestryVerdict != VerdictAncestor {
		t.Errorf("AncestryVerdict = %q, want %q", res.AncestryVerdict, VerdictAncestor)
	}
}

// TestVerify_OriginIsAncestor_VerdictAhead is the realistic healthy shape
// for this fleet's actual workflow: local commits stacked on top of a
// current origin/main point. Must NOT be treated as diverged/blocking —
// that would make ordinary local development permanently fail the guard.
func TestVerify_OriginIsAncestor_VerdictAhead(t *testing.T) {
	dir := initRepoBP(t)
	base := commitFileBP(t, dir, "base.txt")
	runGitBP(t, dir, "update-ref", "refs/remotes/origin/main", base)
	buildCommit := commitFileBP(t, dir, "local-hardening.txt") // stacked on top of base

	res, err := Verify(mustGitBinBP(t), dir, buildCommit)
	if err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	if res.AncestryVerdict != VerdictAhead {
		t.Errorf("AncestryVerdict = %q, want %q", res.AncestryVerdict, VerdictAhead)
	}
}

// TestVerify_NoCommonAncestor_VerdictDiverged is the ga-19easp signature:
// a build cut from a point origin/main has no relationship to at all.
func TestVerify_NoCommonAncestor_VerdictDiverged(t *testing.T) {
	dir := initRepoBP(t)
	buildCommit := commitFileBP(t, dir, "local.txt")
	// A separate, unrelated root commit as origin/main — no shared history.
	runGitBP(t, dir, "checkout", "--orphan", "unrelated-root")
	commitFileBP(t, dir, "unrelated.txt")
	runGitBP(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")

	res, err := Verify(mustGitBinBP(t), dir, buildCommit)
	if err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	if res.AncestryVerdict != VerdictDiverged {
		t.Errorf("AncestryVerdict = %q, want %q", res.AncestryVerdict, VerdictDiverged)
	}
}

func TestVerify_BuildCommitNotInClone_Errors(t *testing.T) {
	dir := initRepoBP(t)
	commitFileBP(t, dir, "seed.txt")
	runGitBP(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")

	_, err := Verify(mustGitBinBP(t), dir, "deadbeef00deadbeef00deadbeef00deadbeef0")
	if err == nil {
		t.Fatal("Verify() error = nil, want error for a commit absent from this clone")
	}
}

func TestVerify_NoOriginMainRef_Errors(t *testing.T) {
	dir := initRepoBP(t)
	buildCommit := commitFileBP(t, dir, "seed.txt")

	_, err := Verify(mustGitBinBP(t), dir, buildCommit)
	if err == nil {
		t.Fatal("Verify() error = nil, want error for a clone with no origin/main ref")
	}
}
