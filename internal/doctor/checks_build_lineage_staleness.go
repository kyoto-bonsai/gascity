package doctor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/git"
)

const (
	defaultBuildLineageMaxAge     = 14 * 24 * time.Hour
	defaultBuildLineageMaxCommits = 50
)

// BuildLineageStalenessCheck fails loud when the primary gascity-src
// checkout's current HEAD has drifted too far from origin/main — the
// recurring failure mode ga-tk5mcg.11 named in the abstract and ga-89t7e2
// confirmed for real: a branch cut from a stale point silently drops
// local-only hardening that was never merged upstream (PR access to
// gastownhall/gascity is blocked for both local GitHub identities, so nothing
// here ever lands via a real merge — see reference_gascity_src_dev_gotchas).
//
// Deliberately checks sourceRepoPath's own HEAD against its own origin/main,
// not one specific already-built binary's embedded commit traced backward:
// every build in this fleet's history is a cherry-pick stack, and per-bead
// clones routinely have separate object stores (reference_gascity_src_dev_gotchas
// "Corollary"), so a specific historical commit is not reliably resolvable in
// any one local repo. HEAD-vs-origin/main is always resolvable within a
// single repo and answers the operationally relevant question: is the
// lineage a new build would be cut from right now already too far gone.
//
// This check reads local git state only — it does not fetch. Staleness is
// therefore bounded by how recently something last ran `git fetch` in
// sourceRepoPath, not by wall-clock reality; that tradeoff is deliberate
// (avoids adding network I/O and auth dependency to a routine doctor scan)
// and is stated in the result message so it is never silently over-trusted.
//
// Also asserts binary-to-checkout lineage IDENTITY (ga-jpvkyz, remediating
// tomoko's FAIL on the original version of this check): checkout staleness
// and the running binary's own hardening-symbol content were previously two
// independent measurements that never connected, so a binary built from an
// entirely different lineage than the measured checkout could still report
// OK — precisely the ga-19easp failure this guard exists to catch. Run()
// resolves the running binary's embedded build commit and asserts it is an
// ancestor of (or equal to) the checkout's HEAD before trusting the
// checkout's own staleness number as applicable to what is actually
// deployed; a definitively-not-an-ancestor result blocks independent of the
// staleness numbers, since it means those numbers describe a lineage the
// running binary was never part of.
type BuildLineageStalenessCheck struct {
	sourceRepoPath string
	maxAge         time.Duration
	maxCommits     int
	gitPath        func(name string) (string, error) // injectable for tests
	now            func() time.Time                  // injectable for tests
	binaryPath     func() (string, error)            // injectable for tests
	goVersionM     func(path string) (string, error) // injectable for tests
}

// NewBuildLineageStalenessCheck creates a check comparing sourceRepoPath's
// HEAD against its own origin/main. sourceRepoPath resolution: the
// GC_SRC_PATH env var if set, else ~/personal/github/gascity-src (this
// fleet's established "primary checkout" convention — see
// reference_gascity_src_dev_gotchas).
func NewBuildLineageStalenessCheck() *BuildLineageStalenessCheck {
	return &BuildLineageStalenessCheck{
		sourceRepoPath: resolveSourceRepoPath(),
		maxAge:         defaultBuildLineageMaxAge,
		maxCommits:     defaultBuildLineageMaxCommits,
		gitPath:        exec.LookPath,
		now:            time.Now,
		binaryPath:     os.Executable,
		goVersionM:     runGoVersionM,
	}
}

// errCommitUnresolvable means the queried commit could not be resolved as
// an object in the target repo at all — e.g. it exists only in a different
// local clone with a separate object store (reference_gascity_src_dev_gotchas
// "Corollary"). This is a "cannot verify" state, not a "verified diverged"
// one, and must not block on its own.
var errCommitUnresolvable = errors.New("commit not resolvable in this repo")

// errNotAncestor means the commit IS a valid object in the target repo but
// is definitively not an ancestor of the compare-to ref — real lineage
// divergence, the ga-19easp signature.
var errNotAncestor = errors.New("commit is not an ancestor")

var (
	buildCommitLdflagsRe = regexp.MustCompile(`main\.commit=([0-9a-fA-F]+(?:-dirty)?)`)
	buildCommitVCSRe     = regexp.MustCompile(`vcs\.revision=([0-9a-fA-F]+)`)
)

// buildCommitOf extracts the embedded build commit from `go version -m`
// output for the binary at path, preferring the ldflags -X main.commit=
// stamp (cmd/gc's own convention, cmd/gc/cmd_version.go) and falling back
// to the toolchain's automatic vcs.revision stamp for binaries built
// without ldflags (e.g. a plain `go install`).
func buildCommitOf(goVersionM func(string) (string, error), path string) (string, error) {
	out, err := goVersionM(path)
	if err != nil {
		return "", err
	}
	if m := buildCommitLdflagsRe.FindStringSubmatch(out); len(m) == 2 {
		return strings.TrimSuffix(m[1], "-dirty"), nil
	}
	if m := buildCommitVCSRe.FindStringSubmatch(out); len(m) == 2 {
		return m[1], nil
	}
	return "", errors.New("no build commit found in go version -m output")
}

// runGoVersionM shells out to `go version -m <path>` and returns raw stdout.
func runGoVersionM(path string) (string, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", err
	}
	out, err := exec.Command(goBin, "version", "-m", path).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// isAncestor runs `git merge-base --is-ancestor <commit> <ref>` in dir and
// classifies the three possible outcomes by exit code: 0 -> true (nil
// error), 1 -> definitively false (errNotAncestor), anything else -> the
// commit could not be resolved at all (errCommitUnresolvable). Uses Run()
// directly rather than the package's runGitCommand helper because
// --is-ancestor communicates its result via exit code alone (no stdout to
// capture) and the exit-code distinction (1 vs. anything else) is the
// entire point.
func isAncestor(gitBin, dir, commit, ref string) error {
	cmd := exec.Command(gitBin, "merge-base", "--is-ancestor", commit, ref)
	cmd.Dir = dir
	cmd.Env = git.SanitizedEnv()
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return errNotAncestor
	}
	return errCommitUnresolvable
}

func resolveSourceRepoPath() string {
	if p := strings.TrimSpace(os.Getenv("GC_SRC_PATH")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "personal", "github", "gascity-src")
}

// Name returns the check identifier.
func (c *BuildLineageStalenessCheck) Name() string { return "build-lineage-staleness" }

// CanFix returns false; reconciling a diverged lineage requires operator/
// engineering judgment (fold vs. full rebase), not automated remediation.
func (c *BuildLineageStalenessCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *BuildLineageStalenessCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false — this check shells out to git and is not
// needed on every `gc start`.
func (c *BuildLineageStalenessCheck) WarmupEligible() bool { return false }

// Run compares sourceRepoPath's HEAD against its own origin/main.
func (c *BuildLineageStalenessCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Severity: SeverityAdvisory}

	if c.sourceRepoPath == "" {
		r.Status = StatusWarning
		r.Message = "could not resolve a source repo path — set GC_SRC_PATH to enable build-lineage staleness checking"
		return r
	}
	if info, err := os.Stat(c.sourceRepoPath); err != nil || !info.IsDir() {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("source repo not found at %s — set GC_SRC_PATH to enable build-lineage staleness checking", c.sourceRepoPath)
		return r
	}

	gitBin, err := c.gitPath("git")
	if err != nil {
		r.Status = StatusWarning
		r.Message = "unable to check build lineage — git unavailable"
		return r
	}

	mergeBaseOut, err := runGitCommand(gitBin, c.sourceRepoPath, "merge-base", "HEAD", "origin/main")
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("unable to compute merge-base(HEAD, origin/main) in %s — no origin/main ref? try git fetch there first", c.sourceRepoPath)
		return r
	}
	mergeBase := strings.TrimSpace(mergeBaseOut)

	countOut, err := runGitCommand(gitBin, c.sourceRepoPath, "rev-list", "--count", mergeBase+"..origin/main")
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("unable to count commits behind origin/main in %s", c.sourceRepoPath)
		return r
	}
	behindCount, err := strconv.Atoi(strings.TrimSpace(countOut))
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("unexpected rev-list output in %s: %q", c.sourceRepoPath, countOut)
		return r
	}

	ageOut, err := runGitCommand(gitBin, c.sourceRepoPath, "log", "-1", "--format=%ct", mergeBase)
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("unable to determine merge-base age in %s", c.sourceRepoPath)
		return r
	}
	epochSecs, err := strconv.ParseInt(strings.TrimSpace(ageOut), 10, 64)
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("unexpected commit-date output in %s: %q", c.sourceRepoPath, ageOut)
		return r
	}
	age := c.now().Sub(time.Unix(epochSecs, 0))

	fetchCaveat := fmt.Sprintf(" (against %s's cached origin/main ref — run git fetch there if it hasn't recently)", c.sourceRepoPath)

	// Binary-to-checkout lineage identity (ga-jpvkyz). A definitively
	// diverged binary blocks independent of the staleness numbers above —
	// those numbers describe sourceRepoPath's own lineage, which this
	// result proves the running binary was never part of, so trusting them
	// as "what's actually deployed is this fresh" would be exactly wrong.
	provenanceDetail := "binary provenance not checked (no resolvable running binary or embedded build commit)"
	if binPath, binErr := c.binaryPath(); binErr == nil {
		if commit, cerr := buildCommitOf(c.goVersionM, binPath); cerr == nil && commit != "" {
			switch ancErr := isAncestor(gitBin, c.sourceRepoPath, commit, "HEAD"); {
			case errors.Is(ancErr, errNotAncestor):
				r.Status = StatusError
				r.Severity = SeverityBlocking
				r.Message = fmt.Sprintf(
					"%s: running binary %s (build commit %s) is NOT part of this checkout's history — binary and measured checkout lineages have diverged (ga-19easp class)",
					c.sourceRepoPath, binPath, shortSHA(commit))
				r.FixHint = "the checkout being measured does not reflect what's actually running — point GC_SRC_PATH at the repo/branch that actually produced this binary, or rebuild from this checkout so binary and checkout agree"
				r.Details = []string{fmt.Sprintf("checkout-only staleness (not applicable to the running binary): merge-base with origin/main is %s old, %d commits behind", age.Round(time.Hour), behindCount)}
				return r
			case errors.Is(ancErr, errCommitUnresolvable):
				provenanceDetail = fmt.Sprintf("binary provenance NOT verified — build commit %s not found in %s's object store (fetch, or point GC_SRC_PATH at the repo that actually produced this binary)", shortSHA(commit), c.sourceRepoPath)
			case ancErr == nil:
				provenanceDetail = fmt.Sprintf("binary provenance confirmed: running binary's build commit %s is part of this checkout's history", shortSHA(commit))
			}
		}
	}

	if age > c.maxAge || behindCount > c.maxCommits {
		r.Status = StatusError
		r.Severity = SeverityBlocking
		r.Message = fmt.Sprintf(
			"%s: merge-base with origin/main is %s old and %d commits behind (thresholds: %s / %d commits)%s",
			c.sourceRepoPath, age.Round(time.Hour), behindCount, c.maxAge, c.maxCommits, fetchCaveat)
		r.FixHint = "triage the missing upstream commits, fold this lineage's local-only fixes into the next build's manifest on the LIVE base (not origin/main — see reference_gascity_src_dev_gotchas REPO TOPOLOGY: local main is a dead tracking branch nothing builds from), rebuild, reinstall"
		r.Details = []string{provenanceDetail}
		return r
	}

	r.Status = StatusOK
	r.Message = fmt.Sprintf("%s: merge-base with origin/main is %s old, %d commits behind — within thresholds%s",
		c.sourceRepoPath, age.Round(time.Hour), behindCount, fetchCaveat)
	r.Details = []string{provenanceDetail}
	return r
}

// shortSHA truncates a commit SHA for compact messages.
func shortSHA(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}
