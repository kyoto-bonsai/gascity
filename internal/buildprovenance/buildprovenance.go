// Package buildprovenance implements the install-time authoritative
// build-lineage check (ga-tk5mcg.11.4 Unit D, amended spec per marcus's RCA
// ga-gbx5lc / audits/rca-drift-guard-fetch-semantics-2026-08-03-marcus-decision.md).
//
// The RCA's core finding: an install gate and `gc doctor` differ in
// INFORMATION AVAILABILITY, not merely latency tolerance. At install time,
// in the build clone, the build commit's presence and its relationship to
// origin/main are both free, local, and exact — no fetch needed. Once that
// moment passes the fact may no longer be locally derivable at all (a
// different clone, a pruned object) — which is why three prior doctor-side
// attempts each pushed harder on reconstruction and each dragged in more
// network I/O. So verification happens exactly once, here, and the result
// is recorded; internal/doctor only ever reads that record, never
// re-derives it.
package buildprovenance

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"

	"github.com/gastownhall/gascity/internal/git"
)

// Ancestry verdicts. Deliberately three states, not a bare ancestor/not
// boolean: this fleet's real local-only hardening lineage virtually always
// carries commits origin/main does not have, so a single-direction "is
// build commit an ancestor of origin/main" test would classify nearly
// every real build as failing, permanently, regardless of health — the
// exact "floor that cannot pass" antipattern. VerdictAhead is the healthy,
// expected shape for ordinary local development (commits stacked on a
// reasonably current origin point); only VerdictDiverged — neither commit
// reachable from the other — is the actual ga-19easp signature: a build
// cut from a stale or unrelated point, not merely carrying local commits.
const (
	VerdictAncestor = "ancestor" // build commit is an ancestor of (or equal to) origin/main
	VerdictAhead    = "ahead"    // origin/main is an ancestor of build commit — healthy
	VerdictDiverged = "diverged" // neither — genuine fork, the ga-19easp signature
)

// SchemaVersion identifies the Record JSON shape for forward compatibility.
const SchemaVersion = "1"

// Record is the durable provenance artifact written next to an installed
// gc binary. Field set matches the RCA's A1 requirement verbatim.
type Record struct {
	SchemaVersion     string `json:"schema_version"`
	BuildCommit       string `json:"build_commit"`
	BuildVersion      string `json:"build_version"`
	BuildDate         string `json:"build_date"`
	SourceClonePath   string `json:"source_clone_path"`
	OriginMainSHA     string `json:"origin_main_sha"`
	MergeBase         string `json:"merge_base"`
	AncestryVerdict   string `json:"ancestry_verdict"`
	InstallTimestamp  string `json:"install_timestamp"`
	InstallerIdentity string `json:"installer_identity"`
	Backfill          bool   `json:"backfill,omitempty"`
	BackfillNote      string `json:"backfill_note,omitempty"`
}

// RecordPath returns the provenance-record path for a given binary path.
// Suffixed alongside the binary itself (not ~/.gc/) so the record's own
// lifecycle is tied to the specific artifact it describes, and the two
// independently-drifting install targets this fleet has (/opt/homebrew/bin
// and ~/go/bin — see reference_gascity_src_dev_gotchas) each carry their
// own record rather than sharing one ambiguous file.
func RecordPath(binaryPath string) string {
	return binaryPath + ".provenance.json"
}

// ErrRecordMissing distinguishes "no gate ever ran for this binary" from
// "the gate ran but wrote something unreadable" — callers (doctor) treat
// the two differently (missing has an explicit transition grace period;
// corrupt does not).
var ErrRecordMissing = errors.New("provenance record missing")

// Read loads and parses the record for binaryPath. Returns ErrRecordMissing
// (wrapped) if the file does not exist; any other error means the file
// exists but could not be read or parsed.
func Read(binaryPath string) (Record, error) {
	data, err := os.ReadFile(RecordPath(binaryPath))
	if err != nil {
		if os.IsNotExist(err) {
			return Record{}, ErrRecordMissing
		}
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// Write serializes rec as indented JSON to RecordPath(binaryPath).
func Write(binaryPath string, rec Record) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(RecordPath(binaryPath), data, 0o644)
}

// VerifyResult holds the outcome of the authoritative, install-time-only
// check: whether it could be completed at all, and if so, the facts it
// established.
type VerifyResult struct {
	OriginMainSHA   string
	MergeBase       string
	AncestryVerdict string
}

// Verify runs the authoritative, local-only (no fetch) check in
// sourceClonePath: does buildCommit resolve there, and what is its
// relationship to sourceClonePath's own origin/main? This is the ONLY
// place in this codebase that computes ancestry for the drift guard — a
// deliberate, install-time-only concentration of the git work that three
// prior doctor-side attempts each tried and failed to do safely at
// check-time (see the package doc). Returns an error if the check could
// not be completed at all (commit unresolvable in this clone, no
// origin/main ref, git unavailable) — as distinct from a completed check
// that found VerdictDiverged, which is a valid result, not a failure.
func Verify(gitBin, sourceClonePath, buildCommit string) (VerifyResult, error) {
	if err := runGit(gitBin, sourceClonePath, "cat-file", "-e", buildCommit); err != nil {
		return VerifyResult{}, errors.New("build commit " + buildCommit + " not found in " + sourceClonePath + " — this must be the exact clone that produced the binary")
	}

	originMainSHA, err := runGitOutput(gitBin, sourceClonePath, "rev-parse", "origin/main")
	if err != nil {
		return VerifyResult{}, errors.New("no origin/main ref in " + sourceClonePath)
	}

	// merge-base can fail outright (not merely "found nothing") when the two
	// commits share NO history at all — e.g. two independent root commits.
	// That is a valid, if extreme, form of divergence, not a technical
	// failure of this check: both objects are already confirmed to exist
	// (cat-file/rev-parse above), so a failure here can only mean "no
	// common ancestor exists," which the ancestry classification below
	// already correctly resolves to VerdictDiverged on its own.
	mergeBase, _ := runGitOutput(gitBin, sourceClonePath, "merge-base", buildCommit, originMainSHA)

	buildIsAncestor := runGit(gitBin, sourceClonePath, "merge-base", "--is-ancestor", buildCommit, originMainSHA) == nil
	originIsAncestor := runGit(gitBin, sourceClonePath, "merge-base", "--is-ancestor", originMainSHA, buildCommit) == nil

	verdict := VerdictDiverged
	switch {
	case buildIsAncestor:
		verdict = VerdictAncestor
	case originIsAncestor:
		verdict = VerdictAhead
	}

	return VerifyResult{
		OriginMainSHA:   originMainSHA,
		MergeBase:       mergeBase,
		AncestryVerdict: verdict,
	}, nil
}

func runGit(gitBin, dir string, args ...string) error {
	cmd := exec.Command(gitBin, args...)
	cmd.Dir = dir
	cmd.Env = git.SanitizedEnv()
	return cmd.Run()
}

func runGitOutput(gitBin, dir string, args ...string) (string, error) {
	cmd := exec.Command(gitBin, args...)
	cmd.Dir = dir
	cmd.Env = git.SanitizedEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return trimTrailingNewline(string(out)), nil
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
