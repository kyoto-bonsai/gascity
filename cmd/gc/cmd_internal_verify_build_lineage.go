package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/buildprovenance"
	"github.com/spf13/cobra"
)

// newInternalVerifyBuildLineageCmd builds the hidden `gc internal
// verify-build-lineage` subcommand — the install gate (ga-tk5mcg.11.4 Unit
// D, amended spec A1). Invoked by the install pipeline immediately after a
// fresh build, in the clone that produced it — not by humans directly, and
// not by `gc doctor` (A1: the gate does not script doctor and read its
// exit code; A2: doctor never fetches or re-derives, it only reads what
// this command records).
func newInternalVerifyBuildLineageCmd(stdout, stderr io.Writer) *cobra.Command {
	var sourceClone string
	cmd := &cobra.Command{
		Use:    "verify-build-lineage",
		Short:  "Install-time authoritative build-lineage check + provenance record",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runVerifyBuildLineage(stdout, stderr, sourceClone)
		},
	}
	cmd.Flags().StringVar(&sourceClone, "source-clone", "",
		"path to the git clone that produced this binary (default: current directory)")
	return cmd
}

// runVerifyBuildLineage resolves this process's OWN embedded build commit
// (the commit/version/date vars cmd_version.go already populates via
// debug.ReadBuildInfo — no separate introspection needed, since this
// command IS the binary being verified) and this process's own path
// (os.Executable — a syscall, not a subprocess), runs the authoritative
// local check, and writes the record.
//
// Design decision, stated explicitly per this bead's own convention of
// arguing rather than silently deciding: gate failure (non-zero exit)
// means "could not determine the facts" (commit absent from the given
// clone, no origin/main ref, git unavailable) — NOT "verdict was
// diverged". A diverged verdict is still written to the record and the
// gate still succeeds. Rationale: this fleet's local-only hardening
// lineage is chronically diverged from origin/main today (ga-tk5mcg.11,
// Unit C reconciliation not yet landed) — a gate that refused every
// diverged install would block ALL future local-hardening installs until
// that reconciliation lands, a severe consequence this implementer is not
// authorized to impose unilaterally. Blocking on a bad verdict is `gc
// doctor`'s job (A2), running after install, not the gate's.
func runVerifyBuildLineage(stdout, stderr io.Writer, sourceClone string) error {
	if strings.TrimSpace(sourceClone) == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "gc internal verify-build-lineage: %v\n", err) //nolint:errcheck // best-effort stderr
			return errExit
		}
		sourceClone = wd
	}

	if commit == "unknown" || commit == "dev" {
		fmt.Fprintln(stderr, "gc internal verify-build-lineage: no embedded build commit (dev build) — refusing to record a meaningless provenance record") //nolint:errcheck // best-effort stderr
		return errExit
	}

	binPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "gc internal verify-build-lineage: resolving own binary path: %v\n", err) //nolint:errcheck // best-effort stderr
		return errExit
	}

	gitBin, err := exec.LookPath("git")
	if err != nil {
		fmt.Fprintf(stderr, "gc internal verify-build-lineage: git unavailable: %v\n", err) //nolint:errcheck // best-effort stderr
		return errExit
	}

	res, err := buildprovenance.Verify(gitBin, sourceClone, commit)
	if err != nil {
		fmt.Fprintf(stderr, "gc internal verify-build-lineage: %v\n", err) //nolint:errcheck // best-effort stderr
		return errExit
	}

	rec := buildprovenance.Record{
		SchemaVersion:     buildprovenance.SchemaVersion,
		BuildCommit:       commit,
		BuildVersion:      version,
		BuildDate:         date,
		SourceClonePath:   sourceClone,
		OriginMainSHA:     res.OriginMainSHA,
		MergeBase:         res.MergeBase,
		AncestryVerdict:   res.AncestryVerdict,
		InstallTimestamp:  time.Now().UTC().Format(time.RFC3339),
		InstallerIdentity: installerIdentity(),
	}
	if err := buildprovenance.Write(binPath, rec); err != nil {
		fmt.Fprintf(stderr, "gc internal verify-build-lineage: writing provenance record: %v\n", err) //nolint:errcheck // best-effort stderr
		return errExit
	}

	fmt.Fprintf(stdout, "gc internal verify-build-lineage: recorded %s (%s) -> %s\n", //nolint:errcheck // best-effort stdout
		commit, res.AncestryVerdict, buildprovenance.RecordPath(binPath))
	if res.AncestryVerdict == buildprovenance.VerdictDiverged {
		fmt.Fprintf(stdout, "gc internal verify-build-lineage: NOTE — lineage has diverged from origin/main (merge-base %s); recorded, not refused (see this file's doc comment for why); gc doctor will report this as blocking once installed\n", res.MergeBase) //nolint:errcheck // best-effort stdout
	}
	return nil
}

func installerIdentity() string {
	if u := strings.TrimSpace(os.Getenv("USER")); u != "" {
		return u
	}
	return "unknown"
}
