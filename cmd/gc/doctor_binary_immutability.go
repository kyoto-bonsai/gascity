package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/doctor"
)

// binaryImmutabilityCheck detects the enforcement gap behind ga-y275uo: a
// direct write onto a live, running, code-signed gc binary (cp / go build -o
// / mv) crashes the process (SIGKILL, invalid code signature) instead of a
// graceful staged restart, because nothing at the filesystem level stops the
// write. Two independent recurrences (2026-07-28, 2026-08-10) both trace to
// an install path that bypassed the documented rename-not-overwrite
// convention; the second froze the entire validator-dispatch plane for ~4h,
// including a live financial control.
//
// For each conventional gc install path present on this host, this check
// verifies both enforcement prongs from ai/fleet/bin/lib/gc-safe-swap.sh are
// armed — they cover different syscalls, neither alone is sufficient (see
// ga-y275uo comment thread for the full syscall matrix):
//
//   - prong 1: the symlink-resolved real binary file is immutable, blocking
//     write-through (e.g. `cp <new> <live-name>` follows the symlink and
//     writes the target).
//   - prong 2: the PATH-visible name itself is immutable, blocking
//     rename-over-name (e.g. `go build -o <live-name>` or `mv` replaces the
//     symlink; this was the observed vector in the 08-10 recurrence).
//
// It also checks a gc.provenance.json sibling exists and matches this
// process's own running commit — an untracked install (the actual root
// cause both times) shows up here as a missing or stale record, catching
// drift at the next `gc doctor` run instead of only after a crash.
//
// This is detection, not a hard boundary: the immutable flag is
// user-clearable (`chflags nouchg`, no privilege required for the file
// owner), so it fails closed against accidental / convention-violating
// bypass — the actual defect class — not a determined same-user adversary.
// Advisory severity: it never gates dispatch or automation, only surfaces
// drift.
type binaryImmutabilityCheck struct {
	// targets returns the conventional PATH-visible gc binary locations to
	// check. Overridable for tests.
	targets func() []string
	// runningCommit returns this process's own build commit. Overridable
	// for tests.
	runningCommit func() string

	// unprotected records targets found missing a flag this Run, so Fix can
	// re-arm exactly those. Reset at the start of each Run — the doctor
	// framework re-invokes Run after a successful Fix to verify.
	unprotected []binaryProtectionGap
}

// binaryProtectionGap names which prong(s) are missing for one target.
type binaryProtectionGap struct {
	target        string
	missingProng1 bool // real binary not immutable
	missingProng2 bool // PATH-visible name not immutable
}

func newBinaryImmutabilityCheck() *binaryImmutabilityCheck {
	return &binaryImmutabilityCheck{
		targets:       defaultGCBinaryTargets,
		runningCommit: func() string { return commit },
	}
}

func (c *binaryImmutabilityCheck) Name() string         { return "binary-immutability" }
func (c *binaryImmutabilityCheck) WarmupEligible() bool { return false }

// defaultGCBinaryTargets returns the conventional PATH-visible gc binary
// locations on this host, plus the currently-running executable's own path
// if not already covered, deduplicated to paths that actually exist. A
// missing conventional path is not itself a gap (a fresh/partial install is
// not this check's concern) — only an existing, unprotected one is.
func defaultGCBinaryTargets() []string {
	home, _ := os.UserHomeDir()
	candidates := []string{"/opt/homebrew/bin/gc"}
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, "go", "bin", "gc"),
			filepath.Join(home, ".local", "bin", "gc"),
		)
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		candidates = append(candidates, exe)
	}

	seen := make(map[string]bool, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, p := range candidates {
		if p == "" || seen[p] {
			continue
		}
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// gcProvenance is the minimal subset of the gc.provenance.json convention
// this check reads. Installers may (and per doctrine should) record a much
// richer narrative record; only build_commit is load-bearing here.
type gcProvenance struct {
	BuildCommit string `json:"build_commit"`
}

// provenancePathFor returns the conventional gc.provenance.json sibling path
// for a gc binary target, e.g. /opt/homebrew/bin/gc ->
// /opt/homebrew/bin/gc.provenance.json.
func provenancePathFor(target string) string {
	return target + ".provenance.json"
}

// readProvenanceCommit reads and parses a provenance.json, returning its
// build_commit. ok is false if the file is missing, unreadable,
// unparseable, or the field is empty.
func readProvenanceCommit(path string) (buildCommit string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var p gcProvenance
	if err := json.Unmarshal(data, &p); err != nil {
		return "", false
	}
	if strings.TrimSpace(p.BuildCommit) == "" {
		return "", false
	}
	return p.BuildCommit, true
}

func (c *binaryImmutabilityCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	res := &doctor.CheckResult{Name: c.Name(), Severity: doctor.SeverityAdvisory}
	c.unprotected = nil

	targets := c.targets()
	if len(targets) == 0 {
		res.Status = doctor.StatusOK
		res.Message = "binary-immutability: no conventional gc install path found on this host — skipped"
		return res
	}

	runningCommit := c.runningCommit()
	var details []string
	issues := 0

	for _, target := range targets {
		prong1OK, prong1Err := realBinaryIsImmutable(target)
		prong2OK, prong2Err := nameIsImmutable(target)

		if isPlatformUnsupported(prong1Err) || isPlatformUnsupported(prong2Err) {
			res.Status = doctor.StatusOK
			res.Message = "binary-immutability: not supported on this platform — skipped"
			return res
		}
		if prong1Err != nil || prong2Err != nil {
			issues++
			details = append(details, fmt.Sprintf("%s: could not read protection state (%v / %v)", target, prong1Err, prong2Err))
			continue
		}

		gap := binaryProtectionGap{target: target, missingProng1: !prong1OK, missingProng2: !prong2OK}
		if gap.missingProng1 || gap.missingProng2 {
			c.unprotected = append(c.unprotected, gap)
			issues++
			var missing []string
			if gap.missingProng1 {
				missing = append(missing, "real-binary uchg")
			}
			if gap.missingProng2 {
				missing = append(missing, "symlink uchg")
			}
			details = append(details, fmt.Sprintf("%s: missing %s — writable in place (ga-y275uo)", target, strings.Join(missing, ", ")))
		}

		provCommit, provOK := readProvenanceCommit(provenancePathFor(target))
		switch {
		case !provOK:
			issues++
			details = append(details, fmt.Sprintf("%s: no provenance record (%s) — untracked install", target, provenancePathFor(target)))
		case runningCommit != "" && runningCommit != "unknown" && provCommit != runningCommit:
			issues++
			details = append(details, fmt.Sprintf("%s: provenance build_commit=%s does not match running commit=%s", target, provCommit, runningCommit))
		}
	}

	if issues == 0 {
		res.Status = doctor.StatusOK
		res.Message = fmt.Sprintf("%d gc binary target(s) protected + provenance-current", len(targets))
		return res
	}

	res.Status = doctor.StatusWarning
	res.Message = fmt.Sprintf("%d issue(s) across %d gc binary target(s) — see ga-y275uo", issues, len(targets))
	res.Details = details
	res.FixHint = "`gc doctor --fix` re-arms a missing immutable flag on an otherwise-intact target; a missing/stale provenance record needs a real install via ai/fleet/bin/lib/gc-safe-swap.sh"
	return res
}

// CanFix reports whether at least one target is missing a chflags prong.
// A missing/stale provenance record is not auto-fixable (it needs a real,
// judged install, not a fabricated record) and is left to FixHint.
func (c *binaryImmutabilityCheck) CanFix() bool {
	return len(c.unprotected) > 0
}

// Fix re-arms the immutable flag on targets found missing it — a narrow,
// content-preserving, idempotent action (it never touches binary bytes,
// only the write-protection state), which is what makes it safe to
// automate. It is the backstop for a flag left cleared by a crash mid-swap
// that the install library's own EXIT trap somehow missed.
func (c *binaryImmutabilityCheck) Fix(_ *doctor.CheckContext) error {
	if len(c.unprotected) == 0 {
		return fmt.Errorf("binary-immutability: no re-armable gap found")
	}
	var errs []string
	for _, gap := range c.unprotected {
		if gap.missingProng1 {
			if err := armRealBinaryImmutable(gap.target); err != nil {
				errs = append(errs, fmt.Sprintf("%s real-binary: %v", gap.target, err))
			}
		}
		if gap.missingProng2 {
			if err := armNameImmutable(gap.target); err != nil {
				errs = append(errs, fmt.Sprintf("%s symlink: %v", gap.target, err))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("binary-immutability: %s", strings.Join(errs, "; "))
	}
	return nil
}
