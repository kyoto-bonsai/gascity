package doctor

import (
	"errors"
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/buildprovenance"
)

// BuildProvenanceCheck fails loud when the running gc binary's install-time
// provenance record is missing, stale, or records a diverged build lineage
// — the ga-19easp failure class (a binary built from a different lineage
// than expected, silently regressing shipped hardening).
//
// Amended spec (ga-tk5mcg.11.4, marcus's RCA ga-gbx5lc,
// audits/rca-drift-guard-fetch-semantics-2026-08-03-marcus-decision.md).
// Three prior attempts each tried to verify lineage AT CHECK TIME and each
// pushed harder on reconstruction (checkout comparison, binary-to-checkout
// ancestry, a fetch to resolve missing objects) — which is what dragged in
// network I/O and, in the third attempt, an unbounded, un-reaped
// subprocess. The RCA's finding: an install gate and doctor differ in
// INFORMATION AVAILABILITY, not merely latency tolerance — the install
// gate (internal/buildprovenance, `gc internal verify-build-lineage`) has
// the fact for free, locally, exactly once, at the one moment it is
// cheaply available. This check NEVER shells out to git and NEVER touches
// the network: it only reads the record that gate already wrote and
// compares it against this process's own embedded build commit (also no
// subprocess — commit is cmd/gc's own package var, already resolved via
// debug.ReadBuildInfo).
type BuildProvenanceCheck struct {
	binaryPath        func() (string, error) // injectable for tests
	commit            string                 // this process's own build commit
	missingIsBlocking bool                   // A4 transition flag — see NewBuildProvenanceCheck
}

// NewBuildProvenanceCheck creates a check reading the provenance record for
// the running binary.
//
// commit is the running binary's own embedded build commit (cmd/gc's
// package-level `commit` var) — passed in rather than re-resolved here,
// matching this package's existing pattern of receiving config via
// constructors (e.g. NewRigRootBranchCheck(rig)).
//
// missingIsBlocking implements the A4 transition: the gate ships before
// every already-installed binary has a record, so a missing record starts
// advisory for exactly one install cycle rather than blocking day 1 (which
// would red-line the fleet) — see ga-tk5mcg.11.5, the dated flip bead this
// parameter must track. An un-flipped advisory repeats the ga-c7np1n
// fail-open class this whole unit exists to close.
func NewBuildProvenanceCheck(commit string, missingIsBlocking bool) *BuildProvenanceCheck {
	return &BuildProvenanceCheck{
		binaryPath:        os.Executable,
		commit:            commit,
		missingIsBlocking: missingIsBlocking,
	}
}

// Name returns the check identifier.
func (c *BuildProvenanceCheck) Name() string { return "build-provenance" }

// CanFix returns false; a missing or diverged record requires re-running
// the install gate, not automated remediation.
func (c *BuildProvenanceCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *BuildProvenanceCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false — not needed on every `gc start`.
func (c *BuildProvenanceCheck) WarmupEligible() bool { return false }

// shortSHA truncates a commit SHA for compact messages.
func shortSHA(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// Run reads the provenance record for the running binary and cross-checks
// it — pure field comparisons plus a file read, zero git, zero network.
func (c *BuildProvenanceCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Severity: SeverityBlocking}

	path, err := c.binaryPath()
	if err != nil {
		r.Status = StatusWarning
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("unable to determine running binary path: %v", err)
		return r
	}

	if c.commit == "" || c.commit == "unknown" || c.commit == "dev" {
		r.Status = StatusOK
		r.Message = "no embedded build commit on this binary (dev build) — skipping provenance check"
		return r
	}

	rec, err := buildprovenance.Read(path)
	if err != nil {
		if errors.Is(err, buildprovenance.ErrRecordMissing) {
			if !c.missingIsBlocking {
				r.Status = StatusWarning
				r.Severity = SeverityAdvisory
				r.Message = fmt.Sprintf("no provenance record for %s — installed outside the sanctioned gate (advisory during the one-cycle transition; see ga-tk5mcg.11.5)", path)
				r.FixHint = "run `gc internal verify-build-lineage` from the clone that built this binary before the transition grace period ends"
				return r
			}
			r.Status = StatusError
			r.Message = fmt.Sprintf("no provenance record for %s — installed outside the sanctioned gate", path)
			r.FixHint = "run `gc internal verify-build-lineage` from the clone that built this binary, then reinstall through the gate"
			return r
		}
		r.Status = StatusError
		r.Message = fmt.Sprintf("provenance record for %s is unreadable: %v", path, err)
		r.FixHint = "the record is corrupt — rerun the install gate to regenerate it"
		return r
	}

	if rec.BuildCommit != c.commit {
		r.Status = StatusError
		r.Message = fmt.Sprintf(
			"provenance record for %s is STALE: record says build commit %s, running binary is actually %s — record does not describe what is running (swapped binary, or gate never rerun after a manual copy)",
			path, shortSHA(rec.BuildCommit), shortSHA(c.commit))
		r.FixHint = "rerun the install gate against the actual running binary's source clone"
		return r
	}

	switch rec.AncestryVerdict {
	case buildprovenance.VerdictAncestor, buildprovenance.VerdictAhead:
		r.Status = StatusOK
		r.Message = fmt.Sprintf("%s: build commit %s verdict=%s (recorded %s)", path, shortSHA(rec.BuildCommit), rec.AncestryVerdict, rec.InstallTimestamp)
		if rec.Backfill {
			r.Details = []string{"this record was backfilled post-hoc, not written live by the gate: " + rec.BackfillNote}
		}
		return r
	default: // VerdictDiverged, or an unrecognized/future value — fail closed, not open.
		r.Status = StatusError
		r.Message = fmt.Sprintf("%s: build commit %s has DIVERGED from origin/main (merge-base %s) — the ga-19easp regression signature", path, shortSHA(rec.BuildCommit), shortSHA(rec.MergeBase))
		r.FixHint = "reconcile the build lineage (fold local-only fixes onto a current base) before trusting this binary; see ga-tk5mcg.11.3 (Unit C)"
		if rec.Backfill {
			r.Details = []string{"this record was backfilled post-hoc: " + rec.BackfillNote}
		}
		return r
	}
}
