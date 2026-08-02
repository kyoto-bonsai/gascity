package beads_test

// Validator-authored probe (persona-marcus, ga-jcnrqn validation 2026-07-28),
// extended by the ga-0pg093 fix. Purpose: determine whether 8e6cd6930's
// AssigneesAreAliases exemption can silently DROP the querying recipient's own
// mail.
//
// The author's TestBdStoreListBothTiersAssigneeAliasesAppliesLimit uses a mock
// runner that IGNORES the --limit argument and returns one always-matching
// bead, so it cannot observe truncation. This probe differs in two respects:
// the mock HONORS --limit, the way a real bd/Dolt backend does (marcus's
// original addition) — and HONORS an assignee filter in the query args
// (--assignee=X for bd list, assignee=X within the bd query DSL string), the
// way a real backend's indexed assignee field does. The second is required to
// exercise the ga-0pg093 fix: it now pushes a real per-alias assignee filter
// server-side, so a mock that ignores assignee content cannot distinguish
// "found this alias's own mail" from "found whatever the fixture always
// returns."
import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// limitHonoringRunner simulates a server that filters by assignee (when the
// query args carry one) and applies --limit to the (possibly filtered)
// result set, sorted newest-first. decoys are other recipients' newer
// messages; the target is the querying persona's own older message,
// positioned past the limit in the unfiltered set.
func limitHonoringRunner(t *testing.T, decoys int, targetAssignee string) func(string, string, ...string) ([]byte, error) {
	t.Helper()
	return func(_, name string, args ...string) ([]byte, error) {
		full := name + " " + strings.Join(args, " ")

		limit := -1
		for i, a := range args {
			if a == "--limit" && i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil {
					limit = n
				}
			}
		}

		// A real backend's --assignee=X (bd list) or "assignee=X" (bd query
		// DSL) filters BEFORE limiting. Extract whichever form is present so
		// this mock does the same.
		queryAssignee := ""
		for i, a := range args {
			if a == "--assignee" && i+1 < len(args) {
				queryAssignee = args[i+1]
			} else if strings.HasPrefix(a, "--assignee=") {
				queryAssignee = strings.TrimPrefix(a, "--assignee=")
			}
		}
		if queryAssignee == "" {
			for _, a := range args {
				for _, clause := range strings.Split(a, " AND ") {
					if v, ok := strings.CutPrefix(clause, "assignee="); ok {
						queryAssignee = v
					}
				}
			}
		}

		type issue struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			Status    string `json:"status"`
			IssueType string `json:"issue_type"`
			Assignee  string `json:"assignee"`
			CreatedAt string `json:"created_at"`
			Ephemeral bool   `json:"ephemeral"`
		}

		isQuery := strings.HasPrefix(full, "bd query ")
		rows := make([]issue, 0, decoys+1)
		// Newest-first: decoys for OTHER recipients occupy the head of the set.
		for i := 0; i < decoys; i++ {
			rows = append(rows, issue{
				ID:        fmt.Sprintf("bd-decoy-%d", i),
				Title:     "someone else's newer mail",
				Status:    "open",
				IssueType: "message",
				Assignee:  "human",
				CreatedAt: fmt.Sprintf("2026-07-28T%02d:%02d:00Z", 23-(i/60)%24, 59-i%60),
				Ephemeral: isQuery,
			})
		}
		// The querying persona's own, older, still-unread message.
		rows = append(rows, issue{
			ID:        "bd-mine",
			Title:     "MY unread mail",
			Status:    "open",
			IssueType: "message",
			Assignee:  targetAssignee,
			CreatedAt: "2026-07-01T00:00:00Z",
			Ephemeral: isQuery,
		})

		// A real backend's indexed assignee field filters BEFORE truncating.
		if queryAssignee != "" {
			filtered := rows[:0:0]
			for _, r := range rows {
				if r.Assignee == queryAssignee {
					filtered = append(filtered, r)
				}
			}
			rows = filtered
		}
		// A real backend truncates here. limit==0 means unbounded.
		if limit > 0 && limit < len(rows) {
			rows = rows[:limit]
		}
		b, err := json.Marshal(rows)
		if err != nil {
			return nil, err
		}
		return b, nil
	}
}

// TestMarcusGaJcnrqnAliasLimitDropsOwnMail asserts the property that actually
// matters to a mail consumer: if I have unread mail, an inbox read returns it.
//
// With AssigneesAreAliases=true (the 8e6cd6930 path), --limit 500 is applied
// server-side while the assignee predicate is still client-side, so the window
// fills with other recipients' newer messages and the caller's own mail is
// never fetched to be filtered.
func TestMarcusGaJcnrqnAliasLimitDropsOwnMail(t *testing.T) {
	const limit = 500
	s := beads.NewBdStore("/city", limitHonoringRunner(t, limit, "persona-marcus"))

	got, err := s.List(beads.ListQuery{
		Type:     "message",
		Status:   "open",
		TierMode: beads.TierBoth,
		// Exactly the shape messageCandidatesAll builds for a live named
		// persona: one logical recipient, several stable-mailbox spellings.
		Assignees:           []string{"persona-marcus", "ga-wisp-abc123", "qo-marcus-2"},
		AssigneesAreAliases: true,
		Limit:               limit,
	})
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, b := range got {
		if b.ID == "bd-mine" {
			found = true
		}
	}
	if !found {
		t.Fatalf("SILENT MAIL LOSS: recipient has unread mail (bd-mine) but List returned %d bead(s), none of them theirs. "+
			"--limit %d was applied server-side while the assignee predicate ran client-side, so the window filled "+
			"with other recipients' newer messages.", len(got), limit)
	}
}

// Control: the SAME fixture without the aliases flag. Pre-8e6cd6930 behavior
// for this query shape — the limit is vetoed, the scan is unbounded, and the
// caller's mail is found. If this passes while the test above fails, the
// message loss is introduced by the AssigneesAreAliases exemption and is not a
// pre-existing property of the fixture.
func TestMarcusGaJcnrqnControlWithoutAliasesFlagKeepsOwnMail(t *testing.T) {
	const limit = 500
	s := beads.NewBdStore("/city", limitHonoringRunner(t, limit, "persona-marcus"))

	got, err := s.List(beads.ListQuery{
		Type:                "message",
		Status:              "open",
		TierMode:            beads.TierBoth,
		Assignees:           []string{"persona-marcus", "ga-wisp-abc123", "qo-marcus-2"},
		AssigneesAreAliases: false,
		Limit:               limit,
	})
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, b := range got {
		if b.ID == "bd-mine" {
			found = true
		}
	}
	if !found {
		t.Fatalf("control failed: mail lost even WITHOUT the aliases flag (%d bead(s) returned) — "+
			"the fixture would then not isolate the exemption", len(got))
	}
}
