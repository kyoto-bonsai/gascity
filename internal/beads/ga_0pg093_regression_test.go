package beads_test

// Property regression tests for ga-0pg093's B1/B2 acceptance floor: mail
// present => mail returned, at the exact boundary the original fix's own
// tests never constructed — one alias saturating Limit by itself, with the
// recipient's true-newest message sitting on a DIFFERENT alias.
//
// Provenance: this boundary was first identified by persona-marcus's
// validation of commit 21cf204a2 (uncommitted probe at
// internal/beads/marcus_ga_0pg093_union_order_test.go in worktree
// gascity-src-ga-jcnrqn, TestMarcusGa0pg093UnionDropsNewestMailOnLaterAlias).
// That probe's mock models a per-alias-filtered fan-out and does not parse
// the disjunctive "(assignee=a OR assignee=b)" clause the B2 fix now sends
// for the wisps tier, so it does not faithfully exercise the corrected code
// (it would pass, but by accident: its assignee-extraction silently fails to
// match the new clause shape and falls back to returning every row
// unfiltered, which happens to already be newest-first by fixture
// construction). Rewritten here as two tests that each faithfully exercise
// one of the two surviving merge-of-bounded-subqueries paths.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestBdStoreListByAliasesUnionOrdersBySortBeforeTruncating exercises
// listByAliasesUnion directly (via the bd-list/TierIssues tier, the one
// surviving fan-out-and-merge path — bd list has no OR mechanism, so message
// beads aside, this is still how BdStore resolves an AssigneesAreAliases
// query against the issues tier). alias-a holds `bulk` older beads and
// saturates Limit on its own; alias-b holds exactly one bead, the newest
// overall. Before the B1 fix, sortBeadsForQuery(merged, query.Sort) was a
// no-op under SortDefault, so merged stayed in alias-iteration order
// (alias-a's results first) and truncating to Limit dropped alias-b's bead
// entirely, even though it ranks #1 by recency.
func TestBdStoreListByAliasesUnionOrdersBySortBeforeTruncating(t *testing.T) {
	const limit = 3
	const bulk = limit
	runner := func(_, name string, args ...string) ([]byte, error) {
		full := name + " " + strings.Join(args, " ")
		if !strings.HasPrefix(full, "bd list ") {
			return nil, fmt.Errorf("unexpected: %s", full)
		}
		assignee := ""
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "--assignee="); ok {
				assignee = v
			}
		}
		switch assignee {
		case "alias-a":
			rows := make([]map[string]any, 0, bulk)
			for i := 0; i < bulk; i++ {
				rows = append(rows, map[string]any{
					"id": fmt.Sprintf("bd-old-%d", i), "title": "older mail on alias-a",
					"status": "open", "issue_type": "task", "assignee": "alias-a",
					"created_at": fmt.Sprintf("2026-07-01T%02d:%02d:00Z", (bulk-i)/60%24, (bulk-i)%60),
				})
			}
			b, _ := json.Marshal(rows)
			return b, nil
		case "alias-b":
			return []byte(`[{"id":"bd-newest","title":"the recipient's newest mail","status":"open","issue_type":"task","assignee":"alias-b","created_at":"2026-07-28T23:59:00Z"}]`), nil
		default:
			return []byte(`[]`), nil
		}
	}
	s := beads.NewBdStore("/city", runner)

	got, err := s.List(beads.ListQuery{
		Type:                "task",
		Status:              "open",
		Assignees:           []string{"alias-a", "alias-b"},
		AssigneesAreAliases: true,
		// SortDefault, deliberately unset — messageCandidatesAll's exact shape.
		Limit: limit,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) == 0 || got[0].ID != "bd-newest" {
		ids := make([]string, len(got))
		for i, b := range got {
			ids[i] = b.ID
		}
		t.Fatalf("SILENT MAIL LOSS (ga-0pg093 B1): recipient's newest bead (bd-newest, on alias-b, "+
			"which saturated after alias-a) missing or not first from a %d-row read that returned %v. "+
			"listByAliasesUnion must sort by recency before truncating, not trust SortDefault as a no-op "+
			"across concatenated per-alias sub-reads.", limit, ids)
	}
}

// TestBdStoreListEphemeralAliasUnionAppliesLimitOnceOverTrueUnion exercises
// listEphemeral's B2 fix: for an AssigneesAreAliases query, it must push ONE
// disjunctive "(assignee=a OR assignee=b)" bd query clause and apply --limit
// once over the true union, rather than fanning out one bounded query per
// alias and merging in-process (the shape that could drop a later alias's
// newest row — see the sibling issues-tier test above). The mock here
// faithfully parses that clause shape (unlike marcus's original probe) and
// implements real bd query semantics: match ANY OR'd assignee, newest-first,
// apply --limit server-side — the exact behavior marcus live-verified against
// the real backend during ga-0pg093 validation.
func TestBdStoreListEphemeralAliasUnionAppliesLimitOnceOverTrueUnion(t *testing.T) {
	const limit = 3
	const bulk = limit
	var queryCalls []string

	// fixture: alias-a (a live named persona's primary route) holds `bulk`
	// older messages and saturates Limit by itself; alias-b (that same
	// recipient's second route) holds exactly one message, the newest
	// overall — modeling messageCandidatesAll's exact AssigneesAreAliases
	// shape (beadmail.go), TierBoth, Sort unset.
	rows := make([]map[string]any, 0, bulk+1)
	rows = append(rows, map[string]any{
		"id": "bd-newest", "title": "MY newest unread mail (arrived on my second route)",
		"status": "open", "issue_type": "message", "assignee": "alias-b",
		"created_at": "2026-07-28T23:59:00Z", "ephemeral": true,
	})
	for i := 0; i < bulk; i++ {
		rows = append(rows, map[string]any{
			"id": fmt.Sprintf("bd-old-%d", i), "title": "older mail on my primary route",
			"status": "open", "issue_type": "message", "assignee": "alias-a",
			"created_at": fmt.Sprintf("2026-07-01T%02d:%02d:00Z", (bulk-i)/60%24, (bulk-i)%60), "ephemeral": true,
		})
	}

	runner := func(_, name string, args ...string) ([]byte, error) {
		full := name + " " + strings.Join(args, " ")
		if strings.HasPrefix(full, "bd list ") {
			// bd list never sees ephemeral rows, matching production.
			return []byte(`[]`), nil
		}
		if !strings.HasPrefix(full, "bd query ") {
			return nil, fmt.Errorf("unexpected: %s", full)
		}
		queryCalls = append(queryCalls, full)

		limitArg := 0
		for i, a := range args {
			if a == "--limit" && i+1 < len(args) {
				limitArg, _ = strconv.Atoi(args[i+1])
			}
		}
		matched := parseOrAssigneeClause(t, full)

		filtered := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			if matched[r["assignee"].(string)] {
				filtered = append(filtered, r)
			}
		}
		// Fixture is already constructed newest-first (bd-newest, then
		// descending bd-old-N) — mirrors bd query's live-verified native
		// default order (ga-0pg093 validation), not re-sorted here.
		if limitArg > 0 && len(filtered) > limitArg {
			filtered = filtered[:limitArg]
		}
		b, _ := json.Marshal(filtered)
		return b, nil
	}
	s := beads.NewBdStore("/city", runner)

	got, err := s.List(beads.ListQuery{
		Type:                "message",
		Status:              "open",
		TierMode:            beads.TierBoth,
		Assignees:           []string{"alias-a", "alias-b"},
		AssigneesAreAliases: true,
		Limit:               limit,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(queryCalls) != 1 {
		t.Fatalf("bd query calls = %v, want exactly 1 (B2: one disjunctive query, not one per alias)", queryCalls)
	}
	if !strings.Contains(queryCalls[0], "(assignee=alias-a OR assignee=alias-b)") {
		t.Fatalf("bd query command = %q, want the disjunctive assignee clause", queryCalls[0])
	}
	if !strings.Contains(queryCalls[0], "--limit 3") {
		t.Fatalf("bd query command = %q, want --limit 3 applied once over the union", queryCalls[0])
	}

	found := false
	for _, b := range got {
		if b.ID == "bd-newest" {
			found = true
		}
	}
	if !found {
		ids := make([]string, len(got))
		for i, b := range got {
			ids[i] = b.ID
		}
		t.Fatalf("recipient's newest message (bd-newest, on alias-b) missing from %v — the union query "+
			"must retrieve it since it ranks #1 by recency across the whole alias union", ids)
	}
}

// parseOrAssigneeClause extracts the assignee set from a
// "... AND (assignee=a OR assignee=b) AND ..." clause embedded in a bd query
// command line, modeling what a real indexed backend's query planner would
// match. Fails the test if no such clause is present, so a caller regression
// (e.g. reverting to a single assignee= clause) fails loudly here rather than
// silently matching nothing.
func parseOrAssigneeClause(t *testing.T, cmd string) map[string]bool {
	t.Helper()
	start := strings.Index(cmd, "(assignee=")
	if start == -1 {
		t.Fatalf("cmd = %q, want a disjunctive (assignee=... OR ...) clause", cmd)
	}
	end := strings.Index(cmd[start:], ")")
	if end == -1 {
		t.Fatalf("cmd = %q, unterminated assignee clause", cmd)
	}
	group := cmd[start+1 : start+end]
	matched := make(map[string]bool)
	for _, term := range strings.Split(group, " OR ") {
		if v, ok := strings.CutPrefix(term, "assignee="); ok {
			matched[v] = true
		}
	}
	return matched
}
