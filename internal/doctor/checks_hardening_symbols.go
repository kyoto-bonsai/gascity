package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// hardeningSymbol pairs a must-never-regress function name with the bead
// that authored it, so a missing-symbol message points straight at the fix
// to restore rather than just naming a bare Go identifier.
type hardeningSymbol struct {
	name string
	bead string
}

// hardeningSymbols is the fixed list from ga-tk5mcg.11.4: the exact 5
// functions ga-89t7e2 confirmed present-by-nm on 2026-08-02. All 5 are
// large, non-inlined functions — nm cannot see inlined functions (a small
// predicate collapses to a one-liner the compiler drops the symbol for; see
// reference_gascity_src_dev_gotchas, ga-2w649o false-negative). Do not add a
// small helper/predicate to this list without first confirming with
// `go tool nm` that it survives as a standalone symbol on a real build.
var hardeningSymbols = []hardeningSymbol{
	{"checkOfficerOfRecord", "ga-ui3tes"},
	{"deriveOfficerOfRecord", "ga-owbb42"},
	{"checkLiveRoutingConflict", "ga-ktvnh1"},
	{"checkProviderSeatCap", "ga-mpb0xu"},
	{"prioritizeHookClaimCandidatesByWorkDir", "ga-vtv442"},
}

// HardeningSymbolsCheck fails loud when the running gc binary is missing one
// of a fixed set of previously-shipped, must-never-regress hardening
// functions. This is a reachability check only — symbol presence proves the
// code is linked in, it does not prove the behavior is wired or correct
// (each restored fix still needs its own effect check; see ga-89t7e2's own
// scope note). It exists to catch a specific, recurring failure mode this
// fleet has hit four confirmed times (ga-tk5mcg.11): a binary built from a
// stale/wrong base silently drops local-only hardening that was never
// merged upstream, because PR access to gastownhall/gascity is blocked for
// both local GitHub identities and every local fix instead lives only on
// this machine's build lineage.
type HardeningSymbolsCheck struct {
	binaryPath func() (string, error)            // injectable for tests
	runNM      func(path string) (string, error) // injectable for tests
}

// NewHardeningSymbolsCheck creates a check that probes the currently-running
// gc binary (resolved via os.Executable — the binary this very process was
// launched from, so "after every install" is automatic: any doctor run
// against the newly-installed binary probes that exact file) for the fixed
// hardening-symbol list.
func NewHardeningSymbolsCheck() *HardeningSymbolsCheck {
	return &HardeningSymbolsCheck{
		binaryPath: os.Executable,
		runNM:      runGoToolNM,
	}
}

// Name returns the check identifier.
func (c *HardeningSymbolsCheck) Name() string { return "hardening-symbol-presence" }

// CanFix returns false; a missing symbol requires a real rebuild, not an
// automated remediation.
func (c *HardeningSymbolsCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *HardeningSymbolsCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false — this check shells out to `go tool nm`
// against a large binary and is not needed on every `gc start`.
func (c *HardeningSymbolsCheck) WarmupEligible() bool { return false }

// Run probes the running binary for each symbol in hardeningSymbols.
func (c *HardeningSymbolsCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Severity: SeverityBlocking}

	path, err := c.binaryPath()
	if err != nil {
		r.Status = StatusWarning
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("unable to determine running binary path: %v", err)
		return r
	}

	nmOut, err := c.runNM(path)
	if err != nil {
		r.Status = StatusWarning
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("symbol probe unavailable (go toolchain missing, or nm failed: %v) — skipping hardening-symbol check on this host", err)
		return r
	}

	var missing []string
	for _, sym := range hardeningSymbols {
		if !strings.Contains(nmOut, sym.name) {
			missing = append(missing, fmt.Sprintf("%s (%s)", sym.name, sym.bead))
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		r.Status = StatusError
		r.Message = fmt.Sprintf("%d/%d must-never-regress hardening symbols missing from %s: %s",
			len(missing), len(hardeningSymbols), path, strings.Join(missing, ", "))
		r.FixHint = "binary was likely built from a stale/wrong base (ga-tk5mcg.11 class) — fold the named fixes into the next build's manifest on the LIVE base (not origin/main), rebuild, reinstall via the standard rename-aside+codesign install script"
		r.Details = []string{"symbol presence is reachability evidence only — it does not prove the behavior is wired or correct; verify each restored fix's own effect check before treating this as resolved (see reference_gascity_src_dev_gotchas / feedback_validate_effect_not_reachability)"}
		return r
	}

	r.Status = StatusOK
	r.Message = fmt.Sprintf("all %d must-never-regress hardening symbols present in %s", len(hardeningSymbols), path)
	return r
}

// runGoToolNM shells out to `go tool nm <path>` and returns raw stdout.
func runGoToolNM(path string) (string, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", err
	}
	out, err := exec.Command(goBin, "tool", "nm", path).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
