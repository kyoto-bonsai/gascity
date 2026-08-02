package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
type BuildLineageStalenessCheck struct {
	sourceRepoPath string
	maxAge         time.Duration
	maxCommits     int
	gitPath        func(name string) (string, error) // injectable for tests
	now            func() time.Time                  // injectable for tests
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
	}
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

	if age > c.maxAge || behindCount > c.maxCommits {
		r.Status = StatusError
		r.Severity = SeverityBlocking
		r.Message = fmt.Sprintf(
			"%s: merge-base with origin/main is %s old and %d commits behind (thresholds: %s / %d commits)%s",
			c.sourceRepoPath, age.Round(time.Hour), behindCount, c.maxAge, c.maxCommits, fetchCaveat)
		r.FixHint = "triage the missing upstream commits, fold this lineage's local-only fixes into the next build's manifest on the LIVE base (not origin/main — see reference_gascity_src_dev_gotchas REPO TOPOLOGY: local main is a dead tracking branch nothing builds from), rebuild, reinstall"
		return r
	}

	r.Status = StatusOK
	r.Message = fmt.Sprintf("%s: merge-base with origin/main is %s old, %d commits behind — within thresholds%s",
		c.sourceRepoPath, age.Round(time.Hour), behindCount, fetchCaveat)
	return r
}
