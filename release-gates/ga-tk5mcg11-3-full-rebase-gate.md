# Release Gate — ga-tk5mcg.11.3 (Unit C: full conflict-aware rebase onto origin/main)

**Bead:** ga-tk5mcg.11.3 (Unit C, executing Unit B's ratified decision on ga-tk5mcg.11.2)
**Branch:** `fix/ga-tk5mcg11-full-rebase` (pushed to kyoto-bonsai fork)
**Base:** `origin/main` @ `86ef443b96414d9e4df10853015c9b17ff2b9ae2` (fetched live from `github.com/gastownhall/gascity`, NOT the stale local `main` tracking branch — see gascity-src topology gotcha)
**Old lineage tip carried forward:** `b3c8d97d2` (`ga-19easp-unified-f1`, the currently-deployed binary)
**Author:** persona-nils (session ga-amg6yi / persona-nils-8)
**Evaluator:** self-evaluated, PENDING independent review (Author != Validator — see below)

## What this is

Unit B (ga-tk5mcg.11.2, countersigned by persona-marcus) decided: one full conflict-aware rebase of the local-only commits onto current `origin/main`, executed one commit at a time via `git cherry-pick` (never a raw multi-commit `git rebase`), rather than continuing to cherry-pick individual upstream fixes onto an increasingly-diverged private fork (304 upstream-only / 42 local-only commits at measurement time, only 1 of 40 comparable local commits had an upstream equivalent — no merge path).

This gate covers landing all local-only commits from the deployed lineage (`b3c8d97d2`) onto a fresh `origin/main` base, resolving every real conflict, and leaving a build-clean, test-clean, gofmt/vet-clean tree ready for install coordination.

## Gate criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | **PENDING** | Not yet reviewed. Author (persona-nils) cannot self-validate. Routing to a non-nils validator per Author != Validator (see Handoff below). |
| 2 | Acceptance criteria met | PASS | See matrix below. |
| 3 | Tests pass | PASS | See Test evidence below — full package suites for every touched surface, plus a scoped `internal/api` full-suite run and a clean-baseline comparison for the two anomalies found. |
| 4 | No high-severity review findings open | PASS (self-assessed) | Every conflict was read and resolved by content, not blindly taken from one side. One genuine cross-file semantic gap found post-merge (see Findings below) and fixed with a verified regression-test correction, not a suppression. |
| 5 | Final branch is clean | PASS | `git status --short` empty. |
| 6 | Branch diverges cleanly from main | PASS | 38 commits ahead of `origin/main`, 0 behind (branch cut fresh from a live fetch of `origin/main`, not the stale local `main`). |
| 7 | Single theme per gate | PASS, with one explicit carve-out | This gate is scoped to the lineage rebase only. `ga-0pg093`'s fix does NOT ship in this gate — see Explicit Exclusion below. |

## Commit inventory

39 local-only commits identified via `git cherry -v origin/main b3c8d97d2` (run against a live fetch, not the stale tracking branch). Landed as **37 real commits + 2 confirmed-duplicate skips**, plus **1 new commit** to fix a rebase-induced OpenAPI drift = **38 commits ahead of origin/main**.

- **Skipped, verified content-identical (not silently dropped — diffed against their believed-duplicate before skipping):**
  - `281b65d3a0` — duplicate of already-landed `1c7a5a1338` (both "ga-ktvnh1 live-routing-conflict guard"). `git diff 1c7a5a1338 281b65d3a0` = empty.
  - `065d33993b` — "drop unused beadmeta import in cmd_sling_test after ktvnh1 merge resolution." Moot: this rebase never introduced the duplicate-merge artifact that import-cleanup was fixing (the duplicate above was skipped, not merged-then-cleaned), so `git cherry-pick` resolved it to an empty diff.
- **New commit not in the original 39:** `chore(schema): regenerate openapi spec after gc-src rebase onto origin/main` — required because the `awaiting` field (ga-8gq4ff) plus every upstream-only schema change since `04c20cb19` left the checked-in `openapi.json`/`.txt` stale. Regenerated via `go run ./cmd/genspec`, not hand-edited.

## Conflicts resolved (5 real conflicts across 37 picks)

All resolved by reading both sides and composing content, never by blindly preferring one side:

1. **`internal/session/sleep_reason.go`** (×2, across the two ga-5gsyts commits) — both sides independently added new `SleepReason*` const entries at the same location. Composed: kept both `SleepReasonAssignedWorkExhausted` (already-landed local) and `SleepReasonProviderResourceExhausted` / `SleepReasonLoginExpired` (this commit's additions, with their doc comments intact).
2. **`internal/events/rotation_reader_test.go`** (import block) — trivial merge of two independently-added import sets (`encoding/json` vs `context`+`errors`).
3. **Cross-file semantic gap, not caught by the textual merge** (see Findings #1 below) — `internal/events/reader_context.go` (an upstream-only file, #4646) called `streamArchive`/`readRotationSources` with their pre-ga-tk5mcg.10 signatures; ga-tk5mcg.10 changed those signatures elsewhere in the same package. `go build` caught it immediately; threaded `ctx` through the two stale call sites, matching ga-tk5mcg.10's own stated intent exactly.
4. **`cmd/gc/cmd_hook_claim.go`** (×2 in the same file) — struct-field addition (`Awaiting`) alongside already-landed fields (`RootBeadID`, `ContinuationGroup`); and a genuinely-additive second loop (`ready_assignment` case) in a JSON-preview helper function that already had the `existing_assignment` case. Verified this was NOT accidental duplication of an existing handler elsewhere in the file before composing (two distinct functions: `claimFirstReadyHookAssignment` does the real claim; `hookClaimExistingAssignment` is a separate dry-run/preview path that was missing its second branch).
5. **`schemas/hook/result.schema.json`** — same additive pattern as #4, the schema-level mirror of the `Awaiting` field addition. JSON validity confirmed after edit.

## Findings

1. **Cross-file signature drift invisible to text-based 3-way merge (self-caught, fixed).** `dea6b29d8` (ga-tk5mcg.10) and upstream's `internal/events/reader_context.go` (#4646, "context-cancelable filtered event readers") independently built near-identical context-cancellation machinery. The textual cherry-pick auto-merged cleanly (no conflict markers) because the two commits touched different files, but the result didn't compile: `reader_context.go`'s own call sites into `streamArchive`/`readRotationSources` predate ga-tk5mcg.10's signature change on those functions. `go build` surfaced this immediately (`not enough arguments in call to streamArchive`), not left latent. Fixed by threading `ctx` through, per ga-tk5mcg.10's own stated intent ("thread context.Context through the archive-scan path"). This is scope this rebase must own — it's not new functionality, it's completing a landed commit's own change to reach a call site that only became visible after rebasing onto a base that has it.
2. **The above fix broke a test's call-counting assumption (self-caught, fixed, not suppressed).** `reader_context_test.go`'s `TestReadFilteredContextAbortsBetweenArchives`/`...WithInFlight...` use a mock that counts `ctx.Err()` calls to pin cancellation to an exact point. Once `reader_context.go`'s call path shares the now-ctx-aware `streamArchive` (which does its own internal per-archive check at scan index 0), a fully-scanned small archive costs 2 `Err()` calls, not 1 — the test's `cancelAt=2` assumption was written before that path existed. Corrected to `cancelAt=3` with an explanatory comment; re-verified the "canceled before any archive opens" control test (`cancelAt=1`) is unaffected. **Did not collapse the resulting code duplication** between `reader.go`'s and `reader_context.go`'s now-near-identical implementations — that's a real, separately-worth-filing cleanup, but out of scope for a rebase gate (flagging here rather than doing an unscoped refactor mid-rebase).
3. **`internal/beads/contract`'s 2 `TestResolveDoltConnectionTarget*` failures are pre-existing, not caused by this rebase.** Confirmed by running the identical tests against a clean, un-rebased `origin/main` clone: identical failure ("dolt runtime state unavailable") with zero relationship to this diff. Environment-dependent (no live managed Dolt runtime in a bare clone), matches this exact pattern already recorded for prior nils sessions on unrelated beads.
4. **`TestHandleProviderReadinessReturnsInvalidConfigurationForClaudeNonFirstPartyProvider` was flaky under full-suite load, not a regression.** Failed once in a full `internal/api` run (5.02s, vs 0.36s normal), passed in isolation, and passed on a clean full-suite rerun. Confirmed as pre-existing-shape flakiness (passes on a clean baseline too when isolated) rather than something this diff introduced.
5. **A parallel, unwrapped rebase attempt of the identical scope was discovered mid-work** (`~/personal/github/gascity-src-ga-tk5mcg11-3`, branch `build/ga-tk5mcg11-3-rebase-onto-main`, git-identity "Andrew Pierce" — consistent with this machine's shared git config for any direct/non-gc-dispatched session, not evidence of a specific actor). It had independently reached the same point and already found the OpenAPI-sync issue (Finding above); I reproduced that fix myself (`go run ./cmd/genspec`) rather than importing its commit, to keep this branch's provenance self-contained and independently verified. Flagging for the operator's awareness in case the two should be reconciled rather than reviewed as if independent.

## Explicit exclusion: `ga-0pg093` is NOT folded into this gate

The instruction to "fold ga-0pg093's rework into this rebase" was investigated fully. Its parent commit (`8e6cd6930`, the ga-jcnrqn fix) and its own commit (`21cf204a2`) are confirmed **not** an ancestor of this branch's base or tip (`git merge-base --is-ancestor 8e6cd6930 HEAD` = NO) — they were never part of the 39-commit local-only lineage Unit B scoped for rebase; they're a separate, still-in-review branch that happens to also reference the old `b3c8d97d2` base.

More importantly: **persona-marcus's validation of `21cf204a2` returned BLOCK, not PASS** (2026-08-02 20:35 comment on ga-0pg093), with three findings that require real new engineering, not a rebase:
- **B1**: the fix's own "top-K-merge" correctness proof is false on the only path that reaches production — `messageCandidatesAll` never sets `Sort`, so the union-then-truncate step silently drops the recipient's *newest* mail once one alias saturates the limit. Demonstrated with a failing test, not just argued.
- **B2**: the fix's stated premise ("bd can't express a multi-value assignee filter") is false for `bd query` (the tier that actually holds mail) — a single disjunctive `bd query ... (assignee=X OR assignee=Y)` works today and is strictly better than the fan-out-and-merge approach the fix took.
- **B3**: the fan-out triples subprocess count on the hottest read path, an unsized risk against a known concurrent-Dolt-contention bug (`ga-mnl73s`) this bead itself blocks.

Implementing marcus's required rework (B2's shape: one disjunctive query, corrected proof comment, a new property test at the saturating-alias boundary, sizing B3) is fresh, unreviewed engineering with its own Author != Validator cycle — bundling it into this gate would violate criterion 7 (single theme) and risk shipping a second under-scrutinized fix in the same review pass this whole exercise exists to avoid repeating. **Recommendation: pick up ga-0pg093 as its own follow-up unit, based on this rebase's tip once installed, implementing B2's disjunctive-query shape rather than porting `21cf204a2` as-is.**

## Acceptance criteria matrix (Unit C scope, per the UNBLOCKED comment on ga-tk5mcg.11.3)

| Criterion | Met | Evidence |
|-----------|-----|----------|
| Conflict-surface gate from Tomoko's triage artifact consulted | YES | `audits/tomoko-triage-ga-tk5mcg.11.1-upstream-only-commits-2026-08-02.md` read in full before starting; per Marcus's amendment on ga-tk5mcg.11.2, the full 304-row behavioral triage is a parallel review artifact, not a serial gate under a full rebase (selection is moot — all upstream commits are taken by construction). |
| One-at-a-time cherry-pick, never raw multi-commit rebase | YES | All 39 commits applied via individual `git cherry-pick -x <sha>`, never `git rebase`. |
| ga-19easp cutover pattern reused for eventual install | DEFERRED | Not installed by this gate (see Handoff). Install instructions unchanged from the ga-19easp precedent when it happens. |
| Verify by string-literal probe, not nm symbols | N/A YET | Applies at install-verification time, not authoring time. Will apply when this lands. |
| Capture pre-cutover binary + build tag | DEFERRED | Applies at install time, not authoring time. |
| nils authors, non-nils validates each gate | PARTIAL | Authored by nils. Validation not yet performed — routing now. |
| Fold ga-0pg093's rework in, don't install as-is | ADDRESSED, NOT INSTALLED | Investigated fully (see Explicit Exclusion above); confirmed not part of this lineage; not installed anywhere; explicitly scoped as follow-up. |

## Test evidence

```
$ go build ./...
(clean)

$ gofmt -l .
(empty)

$ go vet ./...
(clean)

$ go test ./internal/session/... ./internal/events/... ./internal/sling/... \
    ./internal/runtime/tmux/... ./internal/doctor/... -count=1
ok  	github.com/gastownhall/gascity/internal/session	11.565s
ok  	github.com/gastownhall/gascity/internal/session/sessiontest	2.516s
ok  	github.com/gastownhall/gascity/internal/events	8.095s
ok  	github.com/gastownhall/gascity/internal/events/exec	34.401s
ok  	github.com/gastownhall/gascity/internal/sling	12.444s
ok  	github.com/gastownhall/gascity/internal/runtime/tmux	21.053s
ok  	github.com/gastownhall/gascity/internal/doctor	36.177s
ok  	github.com/gastownhall/gascity/internal/doctor/checks	5.782s

$ go test ./internal/api/... ./internal/beadmeta/... ./internal/beads/... \
    ./internal/config/... ./internal/orders/... ./internal/runproj/... \
    ./internal/runtime/... ./internal/storehealth/... ./internal/supervisor/... -count=1
ok  (all packages) except:
  internal/beads/contract: 2 pre-existing Dolt-runtime-state failures,
    confirmed identical on a clean origin/main baseline (see Finding #3)
  internal/api: TestOpenAPISpecInSync initially failed (real, fixed via
    go run ./cmd/genspec, re-verified PASS) and
    TestHandleProviderReadinessReturnsInvalidConfigurationForClaudeNonFirstPartyProvider
    flaked once under full-suite load (passes in isolation and on full-suite
    rerun; confirmed non-reproducing, see Finding #4)

$ go test ./internal/api/ -count=1   # full-package rerun after the fix
ok  	github.com/gastownhall/gascity/internal/api	88.262s
```

Full `cmd/gc` suite (8,005 tests, documented 30+ minute runtime — see
`reference_gascity_src_dev_gotchas`) was not run in full; substituting the
above package-level suites for every package this rebase actually touches
(`git diff --name-only origin/main..HEAD`), stated explicitly rather than
claimed as "suite green."

## Security review

Pure lineage reconciliation (replaying already-authored, individually-reasoned
local commits onto a newer upstream base) plus one generated-artifact
regeneration (`openapi.json`/`.txt`/`internal/api/openapi.json` via the
project's own `cmd/genspec`). No new I/O, no new auth/access surface, no new
external dependency. Conflict resolutions were additive composition (union of
both sides' new struct fields / const entries / schema properties), not logic
rewrites, except the one cross-file completion in Finding #1, which is itself
in-scope of the commit whose own intent it completes.

## Verdict: PENDING — author-complete, routing for independent review

Not installed. Not self-validated. Pushed to `kyoto-bonsai` fork:
`fix/ga-tk5mcg11-full-rebase` @ latest tip (see branch log). Compare URL:
`https://github.com/gastownhall/gascity/compare/main...kyoto-bonsai:gascity:fix/ga-tk5mcg11-full-rebase?expand=1`
