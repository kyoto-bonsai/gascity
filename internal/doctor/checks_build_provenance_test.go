package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/buildprovenance"
)

func TestBuildProvenanceCheck_RecordMissing_TransitionAdvisory(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "gc")
	c := NewBuildProvenanceCheck("abc123", false)
	c.binaryPath = func() (string, error) { return binPath, nil }

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %d, want SeverityAdvisory during the transition grace period", r.Severity)
	}
	if !strings.Contains(r.Message, "sanctioned gate") {
		t.Errorf("message = %q, want mention of the gate", r.Message)
	}
}

func TestBuildProvenanceCheck_RecordMissing_PostTransitionBlocks(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "gc")
	c := NewBuildProvenanceCheck("abc123", true)
	c.binaryPath = func() (string, error) { return binPath, nil }

	r := c.Run(&CheckContext{})

	if r.Status != StatusError {
		t.Fatalf("status = %d (%s), want StatusError once the transition has flipped", r.Status, r.Message)
	}
	if r.Severity != SeverityBlocking {
		t.Errorf("severity = %d, want SeverityBlocking", r.Severity)
	}
}

func TestBuildProvenanceCheck_RecordCorrupt_Blocks(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "gc")
	if err := os.WriteFile(buildprovenance.RecordPath(binPath), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewBuildProvenanceCheck("abc123", false)
	c.binaryPath = func() (string, error) { return binPath, nil }

	r := c.Run(&CheckContext{})

	if r.Status != StatusError {
		t.Fatalf("status = %d (%s), want StatusError — corrupt is not the same as the missing-record transition case", r.Status, r.Message)
	}
	if r.Severity != SeverityBlocking {
		t.Errorf("severity = %d, want SeverityBlocking even during the transition — corrupt is not 'no gate ran'", r.Severity)
	}
}

func TestBuildProvenanceCheck_CommitMismatch_Blocks(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "gc")
	rec := buildprovenance.Record{
		SchemaVersion:   buildprovenance.SchemaVersion,
		BuildCommit:     "recorded-commit",
		AncestryVerdict: buildprovenance.VerdictAncestor,
	}
	if err := buildprovenance.Write(binPath, rec); err != nil {
		t.Fatal(err)
	}
	c := NewBuildProvenanceCheck("actually-running-commit", false)
	c.binaryPath = func() (string, error) { return binPath, nil }

	r := c.Run(&CheckContext{})

	if r.Status != StatusError {
		t.Fatalf("status = %d (%s), want StatusError", r.Status, r.Message)
	}
	if r.Severity != SeverityBlocking {
		t.Errorf("severity = %d, want SeverityBlocking", r.Severity)
	}
	if !strings.Contains(r.Message, "STALE") {
		t.Errorf("message = %q, want STALE mention", r.Message)
	}
}

func TestBuildProvenanceCheck_VerdictAncestor_OK(t *testing.T) {
	testVerdictOK(t, buildprovenance.VerdictAncestor)
}

func TestBuildProvenanceCheck_VerdictAhead_OK(t *testing.T) {
	testVerdictOK(t, buildprovenance.VerdictAhead)
}

func testVerdictOK(t *testing.T, verdict string) {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "gc")
	rec := buildprovenance.Record{
		SchemaVersion:   buildprovenance.SchemaVersion,
		BuildCommit:     "the-commit",
		AncestryVerdict: verdict,
	}
	if err := buildprovenance.Write(binPath, rec); err != nil {
		t.Fatal(err)
	}
	c := NewBuildProvenanceCheck("the-commit", false)
	c.binaryPath = func() (string, error) { return binPath, nil }

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK for verdict=%s", r.Status, r.Message, verdict)
	}
}

// TestBuildProvenanceCheck_VerdictDiverged_Blocks is the bead's own
// positive control, ported to the amended spec: a build whose recorded
// lineage genuinely diverged from origin/main must block, independent of
// everything else being consistent.
func TestBuildProvenanceCheck_VerdictDiverged_Blocks(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "gc")
	rec := buildprovenance.Record{
		SchemaVersion:   buildprovenance.SchemaVersion,
		BuildCommit:     "the-commit",
		MergeBase:       "old-common-point",
		AncestryVerdict: buildprovenance.VerdictDiverged,
	}
	if err := buildprovenance.Write(binPath, rec); err != nil {
		t.Fatal(err)
	}
	c := NewBuildProvenanceCheck("the-commit", false)
	c.binaryPath = func() (string, error) { return binPath, nil }

	r := c.Run(&CheckContext{})

	if r.Status != StatusError {
		t.Fatalf("status = %d (%s), want StatusError", r.Status, r.Message)
	}
	if r.Severity != SeverityBlocking {
		t.Errorf("severity = %d, want SeverityBlocking", r.Severity)
	}
	if !strings.Contains(r.Message, "DIVERGED") {
		t.Errorf("message = %q, want DIVERGED mention", r.Message)
	}
}

func TestBuildProvenanceCheck_BackfillRecord_AnnotatedInDetails(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "gc")
	rec := buildprovenance.Record{
		SchemaVersion:   buildprovenance.SchemaVersion,
		BuildCommit:     "the-commit",
		AncestryVerdict: buildprovenance.VerdictDiverged,
		Backfill:        true,
		BackfillNote:    "captured post-hoc",
	}
	if err := buildprovenance.Write(binPath, rec); err != nil {
		t.Fatal(err)
	}
	c := NewBuildProvenanceCheck("the-commit", false)
	c.binaryPath = func() (string, error) { return binPath, nil }

	r := c.Run(&CheckContext{})

	found := false
	for _, d := range r.Details {
		if strings.Contains(d, "backfilled") {
			found = true
		}
	}
	if !found {
		t.Errorf("Details = %v, want a backfill annotation", r.Details)
	}
}

func TestBuildProvenanceCheck_DevBuild_SkipsGracefully(t *testing.T) {
	for _, commit := range []string{"", "unknown", "dev"} {
		c := NewBuildProvenanceCheck(commit, false)
		c.binaryPath = func() (string, error) { return "/fake/gc", nil }

		r := c.Run(&CheckContext{})

		if r.Status != StatusOK {
			t.Errorf("commit=%q: status = %d (%s), want StatusOK (dev build, no record to check)", commit, r.Status, r.Message)
		}
	}
}

func TestBuildProvenanceCheck_BinaryPathUnresolvable_WarnsAdvisory(t *testing.T) {
	c := NewBuildProvenanceCheck("abc123", false)
	c.binaryPath = func() (string, error) { return "", os.ErrNotExist }

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %d, want SeverityAdvisory", r.Severity)
	}
}

func TestBuildProvenanceCheck_CanFixAndWarmup(t *testing.T) {
	c := NewBuildProvenanceCheck("abc123", false)
	if c.CanFix() {
		t.Error("CanFix() = true, want false")
	}
	if c.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false")
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Errorf("Fix() = %v, want nil no-op", err)
	}
}
