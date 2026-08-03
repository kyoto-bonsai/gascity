package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This file is the mechanical check ga-icjcus asked for, for its one confirmed
// instance.
//
// THE HAZARD CLASS: a local-only commit fixes function X by making it
// populate some field. Later, upstream adds a sibling function Y with the
// same shape but never gets the local patch. A rebase applies the old
// commit's hunks to X cleanly — Y didn't exist when the commit was written,
// so the diff has no hunk for it — and Y ships unfixed. Nothing catches it:
// no merge conflict (no hunk targets Y), cherry-pick reports clean, the old
// commit's own tests still pass (they only ever covered X), and a
// "symbol/line is still present" check also passes (it IS present, in X).
// Those are all commit-hunk-scoped or symbol-presence-scoped checks. The only
// way to catch a sibling that never had the fix is to re-assert the INVARIANT
// the commit established over the WHOLE current tree, every time — not over
// the commit's own diff.
//
// CONFIRMED INSTANCE (ga-8gq4ff): every hookClaimJSONResult that reports an
// actual claimed/assigned bead (Action: "work") must also carry Awaiting —
// the local-only field a hook consumer needs to decide whether to
// self-throttle. hookClaimResultInvariantViolations below re-derives this
// invariant structurally (via go/ast, not line counts) against every
// composite literal of that type anywhere in this package, so a future
// sibling call site — in this file or a new one — is caught the same way,
// without depending on which commit introduced it.
//
// THE RECIPE, for whoever adds the next local-only cross-cutting field: when
// a fix must be threaded through every existing call site of some type or
// function, don't stop at fixing today's call sites. Add a structural test
// like this one, scoped by something in the literal that reliably identifies
// "this call site is in the invariant's domain" (here, Action == "work" —
// see the adjacent-class negative control below for why that discriminator
// matters), asserting the required field is present. That test then
// re-verifies itself against the tree on every future `go test ./cmd/gc/...`
// — the same command this repo's own convention already runs before every
// rebase/cherry-pick/install (see reference_gascity_src_dev_gotchas) — so a
// new sibling added after this test was written is caught by the SAME run
// that would have caught branch B, with no new pipeline step required.

// hookClaimResultInvariantViolations returns one message per
// hookClaimJSONResult composite literal in f whose Action field is the
// literal string "work" — i.e. it reports a specific claimed/assigned bead —
// but has no Awaiting field at all (regardless of that field's value; the
// invariant is that the field is wired, matching every other claim-success
// builder, not that it is non-empty). relPath is used only to format the
// violation message.
func hookClaimResultInvariantViolations(fset *token.FileSet, f *ast.File, relPath string) []string {
	var violations []string
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		ident, ok := cl.Type.(*ast.Ident)
		if !ok || ident.Name != "hookClaimJSONResult" {
			return true
		}
		action, isLiteralAction := compositeLitStringField(cl, "Action")
		if !isLiteralAction || action != "work" {
			// Not a bead-reporting result — e.g. the "drain" no-work case,
			// which legitimately has no bead in scope and so no Awaiting to
			// carry. See TestHookClaimResultInvariantViolations_AllowsDrainActionWithoutAwaiting.
			return true
		}
		if !compositeLitHasField(cl, "Awaiting") {
			line := fset.Position(cl.Pos()).Line
			violations = append(violations, fmt.Sprintf(
				"%s:%d: hookClaimJSONResult{Action: \"work\", ...} has no Awaiting field — "+
					"every claim-success result must carry it (ga-icjcus sibling-function rebase hazard)",
				relPath, line))
		}
		return true
	})
	return violations
}

// compositeLitHasField reports whether cl has a keyed field literally named
// field, regardless of its value.
func compositeLitHasField(cl *ast.CompositeLit, field string) bool {
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == field {
			return true
		}
	}
	return false
}

// compositeLitStringField returns the value of cl's keyed field named field,
// and true, only when that field is present AND its value is a plain string
// literal (as every Action in this file's hookClaimJSONResult literals is
// today). A field that is missing, or whose value is not a string literal
// (e.g. a variable, as writeHookClaimNoWork's Reason is), returns ("", false)
// — this check degrades to skipping that literal rather than guessing, the
// same fail-open-on-unmet-precondition shape the drift-guard checks in
// internal/doctor use.
func compositeLitStringField(cl *ast.CompositeLit, field string) (string, bool) {
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		id, ok := kv.Key.(*ast.Ident)
		if !ok || id.Name != field {
			continue
		}
		bl, ok := kv.Value.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return "", false
		}
		val, err := strconv.Unquote(bl.Value)
		if err != nil {
			return "", false
		}
		return val, true
	}
	return "", false
}

// TestHookClaimResultsCarryAwaitingWhenReportingWork is the actual guard: it
// scans every git-tracked, non-test .go file in cmd/gc (not just
// cmd_hook_claim.go — a future sibling call site could land in a new file in
// this package) and fails loud on any hookClaimJSONResult{Action: "work",
// ...} literal missing Awaiting. This is the check that would have flagged
// ga-icjcus's branch B before validation, run the same way every fix in this
// repo already gets validated: `go test ./cmd/gc/...`.
func TestHookClaimResultsCarryAwaitingWhenReportingWork(t *testing.T) {
	root := hookClaimInvariantRepoRoot(t)

	var violations []string
	for _, rel := range hookClaimInvariantTrackedFiles(t, root) {
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if perr != nil {
			continue // unparseable file is not this guard's concern
		}
		violations = append(violations, hookClaimResultInvariantViolations(fset, f, rel)...)
	}

	if len(violations) > 0 {
		t.Fatalf("found %d hookClaimJSONResult claim-success result(s) not carrying Awaiting:\n%s",
			len(violations), strings.Join(violations, "\n"))
	}
}

// hookClaimInvariantRepoRoot walks up from the working directory to the
// module root (the directory containing go.mod).
func hookClaimInvariantRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod (module root)")
		}
		dir = parent
	}
}

// hookClaimInvariantTrackedFiles returns the repo-relative paths of every
// git-tracked, non-test .go file under cmd/gc, via `git ls-files` rather than
// a filesystem walk — immune to untracked scaffold noise (a stray bead
// worktree checkout, a staging dir) landing under root, same rationale as
// internal/beadmeta's guard_test.go trackedGoFiles.
func hookClaimInvariantTrackedFiles(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", "cmd/gc/*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files in %s: %v", root, err)
	}
	var files []string
	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		files = append(files, rel)
	}
	return files
}

// --- Verification bar: prove the check actually fires, per this repo's own
// standing rule (ga-tk5mcg.11.4: "manually break the guard's precondition and
// confirm it actually fires before trusting it in production"). These three
// tests exercise hookClaimResultInvariantViolations directly against small
// synthetic sources, so they need no real files on disk and can't be
// satisfied by coincidentally-correct current state in cmd_hook_claim.go.

// TestHookClaimResultInvariantViolations_CatchesMissingAwaitingOnWorkAction
// reproduces branch B's exact defect shape: a claim-success result reporting
// Action: "work" with no Awaiting field. This is the synthetic stand-in for
// "the guard would have flagged branch B before validation."
func TestHookClaimResultInvariantViolations_CatchesMissingAwaitingOnWorkAction(t *testing.T) {
	const src = `package main

func example() {
	_ = hookClaimJSONResult{
		Action: "work",
		Reason: "ready_assignment",
		BeadID: candidate.ID,
	}
}
`
	got := parseAndCheckHookClaimInvariant(t, src)
	if len(got) != 1 {
		t.Fatalf("want exactly 1 violation for a work-action literal missing Awaiting, got %d: %v", len(got), got)
	}
}

// TestHookClaimResultInvariantViolations_AllowsWorkActionWithAwaiting is the
// positive control: a correctly-formed claim-success literal (mirroring the
// real "claimed" builder) must not be flagged.
func TestHookClaimResultInvariantViolations_AllowsWorkActionWithAwaiting(t *testing.T) {
	const src = `package main

func example() {
	_ = hookClaimJSONResult{
		Action:   "work",
		Reason:   "claimed",
		BeadID:   claimed.ID,
		Awaiting: claimed.Metadata[beadmeta.AwaitingMetadataKey],
	}
}
`
	got := parseAndCheckHookClaimInvariant(t, src)
	if len(got) != 0 {
		t.Fatalf("want 0 violations for a correctly-formed work-action literal, got %d: %v", len(got), got)
	}
}

// TestHookClaimResultInvariantViolations_AllowsDrainActionWithoutAwaiting is
// the ADJACENT-CLASS negative control, and the reason the guard discriminates
// on Action=="work" instead of flagging every hookClaimJSONResult lacking
// Awaiting: the real "drain" result (writeHookClaimNoWork) has no bead in
// scope at all, so it legitimately has no Awaiting either. A guard that
// didn't gate on Action would permanently fail against known-good code —
// exactly the "floor that can never pass" failure mode this fleet has been
// burned by before. This test pins that the drain shape stays clean.
func TestHookClaimResultInvariantViolations_AllowsDrainActionWithoutAwaiting(t *testing.T) {
	const src = `package main

func example() {
	reason := "no_work"
	_ = hookClaimJSONResult{
		Action: "drain",
		Reason: reason,
	}
}
`
	got := parseAndCheckHookClaimInvariant(t, src)
	if len(got) != 0 {
		t.Fatalf("want 0 violations for the drain shape (no bead in scope), got %d: %v", len(got), got)
	}
}

func parseAndCheckHookClaimInvariant(t *testing.T, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parsing synthetic source: %v", err)
	}
	return hookClaimResultInvariantViolations(fset, f, "synthetic.go")
}
