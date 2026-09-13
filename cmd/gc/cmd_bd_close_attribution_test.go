package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/closeattr"
	"github.com/gastownhall/gascity/internal/config"
)

func TestBdCloseAttributionTargets(t *testing.T) {
	type result struct {
		ids []string
		ok  bool
	}
	cases := []struct {
		name string
		args []string
		want result
	}{
		{"close with reason", []string{"close", "-r", "done", "gcy-dv7"}, result{ids: []string{"gcy-dv7"}, ok: true}},
		{"close without reason still attributes", []string{"close", "gcy-dv7"}, result{ids: []string{"gcy-dv7"}, ok: true}},
		{"close batch", []string{"close", "--reason", "done", "id1", "id2"}, result{ids: []string{"id1", "id2"}, ok: true}},
		{"forced close still attributes", []string{"close", "--force", "-r", "x", "id1"}, result{ids: []string{"id1"}, ok: true}},
		{"update --status closed", []string{"update", "--status", "closed", "id1"}, result{ids: []string{"id1"}, ok: true}},
		{"update -s closed", []string{"update", "id1", "-s", "closed"}, result{ids: []string{"id1"}, ok: true}},
		{"update --status=closed", []string{"update", "--status=closed", "id1"}, result{ids: []string{"id1"}, ok: true}},
		{"update to a non-closed status", []string{"update", "--status", "in_progress", "id1"}, result{ok: false}},
		{"update metadata only", []string{"update", "--set-metadata", "k=v", "id1"}, result{ok: false}},
		{"reopen", []string{"reopen", "id1"}, result{ok: false}},
		{"delete", []string{"delete", "id1"}, result{ok: false}},
		{"close with no id (last-touched fallback)", []string{"close", "-r", "done"}, result{ok: false}},
		{"ambiguous close (unknown flag)", []string{"close", "--unknown-future-flag", "x", "id1"}, result{ok: false}},
		{"empty", nil, result{ok: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ids, ok := bdCloseAttributionTargets(tc.args)
			if ok != tc.want.ok {
				t.Fatalf("bdCloseAttributionTargets(%q) ok = %v, want %v", tc.args, ok, tc.want.ok)
			}
			if ok && !reflect.DeepEqual(ids, tc.want.ids) {
				t.Fatalf("bdCloseAttributionTargets(%q) ids = %v, want %v", tc.args, ids, tc.want.ids)
			}
		})
	}
}

func TestExtractBdExemptReason(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantRest   []string
		wantReason string
		wantErr    bool
	}{
		{
			"separate value, stripped before the bd scanner sees it",
			[]string{"close", "--exempt-reason", "Tier-1 self-close", "-r", "done", "id1"},
			[]string{"close", "-r", "done", "id1"},
			"Tier-1 self-close", false,
		},
		{
			"equals form",
			[]string{"close", "--exempt-reason=why", "id1"},
			[]string{"close", "id1"},
			"why", false,
		},
		{
			"absent: args pass through untouched",
			[]string{"close", "-r", "done", "id1"},
			[]string{"close", "-r", "done", "id1"},
			"", false,
		},
		{"blank value refused", []string{"close", "--exempt-reason", "  ", "id1"}, nil, "", true},
		{"missing value refused", []string{"close", "id1", "--exempt-reason"}, nil, "", true},
		{"repeated flag refused", []string{"close", "--exempt-reason", "a", "--exempt-reason", "b", "id1"}, nil, "", true},
		{"only meaningful on close: update refused", []string{"update", "--exempt-reason", "why", "--status", "closed", "id1"}, nil, "", true},
		{"only meaningful on close: list refused", []string{"list", "--exempt-reason", "why"}, nil, "", true},
		{"after -- it is a positional, not our flag", []string{"close", "id1", "--", "--exempt-reason"}, []string{"close", "id1", "--", "--exempt-reason"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rest, reason, err := extractBdExemptReason(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("extractBdExemptReason(%q) error = nil, want error", tc.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractBdExemptReason(%q) error = %v", tc.args, err)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) || reason != tc.wantReason {
				t.Fatalf("extractBdExemptReason(%q) = (%v, %q), want (%v, %q)", tc.args, rest, reason, tc.wantRest, tc.wantReason)
			}
		})
	}
}

// TestExtractBdExemptReasonKeepsScannerHappy pins the reason the flag is
// stripped early: bdMutationWriteIDs is fail-closed on unknown flags, so an
// unstripped --exempt-reason would abort every close that declares one.
func TestExtractBdExemptReasonKeepsScannerHappy(t *testing.T) {
	raw := []string{"close", "--exempt-reason", "why", "-r", "done", "id1"}
	if _, _, ambiguous := bdMutationWriteIDs(raw); !ambiguous {
		t.Fatalf("precondition: bd scanner should treat --exempt-reason as ambiguous")
	}
	rest, _, err := extractBdExemptReason(raw)
	if err != nil {
		t.Fatalf("extractBdExemptReason: %v", err)
	}
	ids, ok, ambiguous := bdMutationWriteIDs(rest)
	if !ok || ambiguous || !reflect.DeepEqual(ids, []string{"id1"}) {
		t.Fatalf("after stripping: ids=%v ok=%v ambiguous=%v", ids, ok, ambiguous)
	}
}

func closeAttributionTestPolicy() config.RoutingPolicyConfig {
	return config.RoutingPolicyConfig{
		RoutingExempt: []config.RoutingExemptGroup{
			{Name: "officers", Personas: []string{"persona-marcus"}},
			{Name: "cos_office", Personas: []string{"persona-ariadne"}},
		},
		OfficerOfRecordValueDomain: []string{"persona-marcus", "operator"},
		ReportsTo:                  map[string]string{"persona-nils": "persona-marcus"},
	}
}

func closeAttributionEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestStampBdCloseAttribution(t *testing.T) {
	cfg := &config.City{RoutingPolicy: closeAttributionTestPolicy()}
	nilsEnv := closeAttributionEnv(map[string]string{"GC_TEMPLATE": "persona-nils", "BEADS_ACTOR": "persona-nils-ga-abc"})

	t.Run("staff closer stamps routed_to and derived officer", func(t *testing.T) {
		store := beads.NewMemStore()
		b, _ := store.Create(beads.Bead{Title: "ad-hoc follow-up"})
		var stderr bytes.Buffer
		stampBdCloseAttribution([]string{b.ID}, "", store, cfg, nilsEnv, &stderr)
		got, _ := store.Get(b.ID)
		if got.Metadata[beadmeta.RoutedToMetadataKey] != "persona-nils" || got.Metadata[beadmeta.OfficerOfRecordMetadataKey] != "persona-marcus" {
			t.Fatalf("metadata = %v; stderr=%q", got.Metadata, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("unexpected stderr: %q", stderr.String())
		}
	})

	t.Run("declared exemption records reason + routed_to, never an officer", func(t *testing.T) {
		store := beads.NewMemStore()
		b, _ := store.Create(beads.Bead{Title: "tier-1 self-close"})
		var stderr bytes.Buffer
		stampBdCloseAttribution([]string{b.ID}, "Tier-1 single-seat, no validation loop", store, cfg, nilsEnv, &stderr)
		got, _ := store.Get(b.ID)
		if got.Metadata[beadmeta.OfficerExemptReasonMetadataKey] != "Tier-1 single-seat, no validation loop" ||
			got.Metadata[beadmeta.RoutedToMetadataKey] != "persona-nils" ||
			got.Metadata[beadmeta.OfficerOfRecordMetadataKey] != "" {
			t.Fatalf("metadata = %v", got.Metadata)
		}
	})

	t.Run("operator terminal (no GC_TEMPLATE) writes nothing and says why", func(t *testing.T) {
		store := beads.NewMemStore()
		b, _ := store.Create(beads.Bead{Title: "operator-authored"})
		var stderr bytes.Buffer
		stampBdCloseAttribution([]string{b.ID}, "", store, cfg, closeAttributionEnv(map[string]string{"BEADS_ACTOR": "Andrew Pierce"}), &stderr)
		got, _ := store.Get(b.ID)
		if len(got.Metadata) != 0 {
			t.Fatalf("metadata = %v, want none", got.Metadata)
		}
		if !strings.Contains(stderr.String(), "close-attribution") {
			t.Fatalf("expected a diagnostic, got %q", stderr.String())
		}
	})

	t.Run("operator terminal with a declared exemption still records the reason", func(t *testing.T) {
		store := beads.NewMemStore()
		b, _ := store.Create(beads.Bead{Title: "operator-authored"})
		var stderr bytes.Buffer
		stampBdCloseAttribution([]string{b.ID}, "operator direct close", store, cfg, closeAttributionEnv(nil), &stderr)
		got, _ := store.Get(b.ID)
		if got.Metadata[beadmeta.OfficerExemptReasonMetadataKey] != "operator direct close" || got.Metadata[beadmeta.RoutedToMetadataKey] != "" {
			t.Fatalf("metadata = %v", got.Metadata)
		}
	})

	t.Run("policy not configured: no write, no noise", func(t *testing.T) {
		store := beads.NewMemStore()
		b, _ := store.Create(beads.Bead{Title: "opted-out city"})
		var stderr bytes.Buffer
		stampBdCloseAttribution([]string{b.ID}, "", store, &config.City{}, nilsEnv, &stderr)
		got, _ := store.Get(b.ID)
		if len(got.Metadata) != 0 || stderr.Len() != 0 {
			t.Fatalf("metadata = %v stderr = %q", got.Metadata, stderr.String())
		}
	})

	t.Run("existing officer_of_record=operator is preserved (ga-kcfrcn)", func(t *testing.T) {
		store := beads.NewMemStore()
		b, _ := store.Create(beads.Bead{Title: "gate bypass", Metadata: map[string]string{beadmeta.OfficerOfRecordMetadataKey: "operator"}})
		var stderr bytes.Buffer
		stampBdCloseAttribution([]string{b.ID}, "", store, cfg, nilsEnv, &stderr)
		got, _ := store.Get(b.ID)
		if got.Metadata[beadmeta.OfficerOfRecordMetadataKey] != "operator" || got.Metadata[beadmeta.RoutedToMetadataKey] != "persona-nils" {
			t.Fatalf("metadata = %v", got.Metadata)
		}
	})

	t.Run("missing bead is reported and does not stop the batch", func(t *testing.T) {
		store := beads.NewMemStore()
		b, _ := store.Create(beads.Bead{Title: "real"})
		var stderr bytes.Buffer
		stampBdCloseAttribution([]string{"gc-nope", b.ID}, "", store, cfg, nilsEnv, &stderr)
		got, _ := store.Get(b.ID)
		if got.Metadata[beadmeta.RoutedToMetadataKey] != "persona-nils" {
			t.Fatalf("second id not stamped: %v", got.Metadata)
		}
		if !strings.Contains(stderr.String(), "gc-nope") {
			t.Fatalf("missing-bead diagnostic absent: %q", stderr.String())
		}
	})

	t.Run("nil store is a no-op", func(t *testing.T) {
		var stderr bytes.Buffer
		stampBdCloseAttribution([]string{"x"}, "", nil, cfg, nilsEnv, &stderr)
		if stderr.Len() != 0 {
			t.Fatalf("unexpected stderr: %q", stderr.String())
		}
	})
}

func TestExtractBdExemptReasonBlankIsTheSharedSentinel(t *testing.T) {
	_, _, err := extractBdExemptReason([]string{"close", "--exempt-reason", "", "id1"})
	if !errors.Is(err, closeattr.ErrBlankExemptReason) {
		t.Fatalf("blank reason error = %v, want closeattr.ErrBlankExemptReason", err)
	}
}

// closeAttributionFakeBdScript answers the calls the close-attribution path
// makes through a bd-backed store: `show --json <id>` returns one open bead
// (so the already-closed guard lets the close through), and every argv is
// appended to $ARGV_LOG so the test can assert exactly what reached bd —
// the close itself, the attribution write, and never gc's own flag.
const closeAttributionFakeBdScript = `#!/bin/sh
printf '%s\n' "$*" >> "$GC_TEST_BD_ARGV_LOG"
case "$1" in
  show)
    printf '[{"id":"demo-abc","title":"ad-hoc follow-up","status":"open","issue_type":"task","created_at":"2026-09-13T00:00:00Z","metadata":{}}]\n'
    ;;
  list|query)
    printf '[]\n'
    ;;
esac
exit 0
`

// closeAttributionDoBdSetup builds a bd-backed city (managed-Dolt state, like
// silentFallbackTestSetup) that has opted into [routing], with the fake bd
// above on PATH. It returns the path of the fake bd's argv log.
func closeAttributionDoBdSetup(t *testing.T) string {
	t.Helper()
	clearInheritedBeadsEnv(t)
	origCityFlag, origRigFlag := cityFlag, rigFlag
	t.Cleanup(func() { cityFlag, rigFlag = origCityFlag, origRigFlag })
	cityFlag, rigFlag = "", ""

	cityDir := t.TempDir()
	port := strconv.Itoa(writeReachableManagedDoltState(t, cityDir))
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[routing]
officer_of_record_value_domain = ["persona-marcus", "operator"]

[[routing.routing_exempt]]
name = "officers"
personas = ["persona-marcus"]

[routing.reports_to]
persona-nils = "persona-marcus"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "issue_prefix: demo\n" +
		"gc.endpoint_origin: managed_city\n" +
		"gc.endpoint_status: verified\n" +
		"dolt.auto-start: false\n"
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(closeAttributionFakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(binDir, "argv.log")
	t.Setenv("GC_TEST_BD_ARGV_LOG", argvLog)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_DOLT_PORT", port)
	return argvLog
}

// bdArgvLines returns the fake bd's recorded invocations, one per line.
func bdArgvLines(t *testing.T, argvLog string) []string {
	t.Helper()
	raw, err := os.ReadFile(argvLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read argv log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func bdArgvContains(lines []string, needle string) bool {
	for _, l := range lines {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}

func TestDoBdCloseStampsAttributionFromSessionEnv(t *testing.T) {
	argvLog := closeAttributionDoBdSetup(t)
	t.Setenv("GC_TEMPLATE", "persona-nils")
	t.Setenv("GC_ALIAS", "persona-nils-7")
	t.Setenv("BEADS_ACTOR", "persona-nils-ga-xl90sr")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"close", "demo-abc", "-r", "done"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd(close) = %d, want 0; stderr=%q", got, stderr.String())
	}
	lines := bdArgvLines(t, argvLog)
	if !bdArgvContains(lines, "close demo-abc -r done") {
		t.Fatalf("fake bd never saw the close: %q", lines)
	}
	// BdStore.SetMetadataBatch sorts keys, so the write is byte-deterministic.
	want := "update --json demo-abc --set-metadata gc.officer_of_record=persona-marcus --set-metadata gc.routed_to=persona-nils"
	if !bdArgvContains(lines, want) {
		t.Fatalf("attribution write did not reach bd; want %q in %q; stderr=%q", want, lines, stderr.String())
	}
}

func TestDoBdCloseExemptReasonIsStrippedAndRecorded(t *testing.T) {
	argvLog := closeAttributionDoBdSetup(t)
	t.Setenv("GC_TEMPLATE", "persona-marcus")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"close", "--exempt-reason", "Tier-1 single-seat self-close", "demo-abc", "-r", "done"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd(close --exempt-reason) = %d, want 0; stderr=%q", got, stderr.String())
	}
	lines := bdArgvLines(t, argvLog)
	if bdArgvContains(lines, "exempt-reason") {
		t.Fatalf("gc-only flag leaked to bd: %q", lines)
	}
	if !bdArgvContains(lines, "close demo-abc -r done") {
		t.Fatalf("fake bd never saw the close: %q", lines)
	}
	want := "update --json demo-abc --set-metadata gc.officer_exempt_reason=Tier-1 single-seat self-close --set-metadata gc.routed_to=persona-marcus"
	if !bdArgvContains(lines, want) {
		t.Fatalf("exemption write did not reach bd; want %q in %q; stderr=%q", want, lines, stderr.String())
	}
	if bdArgvContains(lines, "gc.officer_of_record") {
		t.Fatalf("a declared exemption must never derive an officer: %q", lines)
	}
}

func TestDoBdCloseBlankExemptReasonExitsBeforeBd(t *testing.T) {
	argvLog := closeAttributionDoBdSetup(t)
	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"close", "--exempt-reason", "  ", "demo-abc"}, &stdout, &stderr); got != 1 {
		t.Fatalf("doBd(close --exempt-reason '  ') = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "must not be blank") {
		t.Fatalf("stderr = %q, want blank-reason refusal", stderr.String())
	}
	if lines := bdArgvLines(t, argvLog); len(lines) != 0 {
		t.Fatalf("bd was invoked despite the refusal: %q", lines)
	}
}

func TestDoBdCloseFromOperatorTerminalLeavesUnattributedAndSaysSo(t *testing.T) {
	argvLog := closeAttributionDoBdSetup(t)
	t.Setenv("GC_TEMPLATE", "")
	t.Setenv("BEADS_ACTOR", "Andrew Pierce")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"close", "demo-abc", "-r", "done"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd(close) = %d, want 0; stderr=%q", got, stderr.String())
	}
	lines := bdArgvLines(t, argvLog)
	if bdArgvContains(lines, "--set-metadata") {
		t.Fatalf("operator close must not write a persona attribution: %q", lines)
	}
	if !strings.Contains(stderr.String(), "close-attribution") || !strings.Contains(stderr.String(), bdExemptReasonFlag) {
		t.Fatalf("expected an unattributed-close diagnostic naming %s, got %q", bdExemptReasonFlag, stderr.String())
	}
}
