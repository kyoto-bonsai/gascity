#!/bin/sh
# Unit test for verify_table_drift_via_fixed_commits (bounded per-table
# writer-race fallback).
# Lib under test: examples/bd/dolt/assets/scripts/compact-fixed-commit-verify.sh
# Run: sh test/dolt/compact_fixed_commit_verify_test.sh
#
# Stubs the run.sh-provided dependencies (query_single_cell, valid_table_name) so
# the classification is exercised without a live Dolt server. The end-to-end
# flatten path is covered by the TestCompactScriptBoundedFallback* tests in
# examples/bd/dolt/dog_exec_scripts_test.go; this covers the decision logic and
# its fail-closed branches.
set -u

HERE=$(unset CDPATH; cd -- "$(dirname "$0")" && pwd)
LIB="$HERE/../../examples/bd/dolt/assets/scripts/compact-fixed-commit-verify.sh"
[ -f "$LIB" ] || { echo "FAIL: lib not found at $LIB"; exit 1; }

# --- stubs for run.sh-provided helpers --------------------------------------
# query_single_cell <db> <msg> <query>: answers the two probe shapes the lib
# issues. Row counts come from stub_rows_<endpoint>; the DOLT_DIFF probe returns
# stub_nonadded. STUB_FAIL_QUERY forces a probe failure on a matching substring;
# STUB_EMPTY_QUERY forces an empty result.
query_single_cell() {
  _q="$3"
  if [ -n "${STUB_FAIL_QUERY:-}" ]; then
    case "$_q" in *"$STUB_FAIL_QUERY"*) return 1 ;; esac
  fi
  if [ -n "${STUB_EMPTY_QUERY:-}" ]; then
    case "$_q" in *"$STUB_EMPTY_QUERY"*) printf ''; return 0 ;; esac
  fi
  case "$_q" in
    *"DOLT_DIFF("*) printf '%s' "${stub_nonadded:-0}" ;;
    *"AS OF 'H1'"*) printf '%s' "${stub_rows_from:-10}" ;;
    *"AS OF 'H2'"*) printf '%s' "${stub_rows_to:-10}" ;;
    *) printf '0' ;;
  esac
  return 0
}
valid_table_name() {
  if [ -n "${STUB_INVALID_TABLE:-}" ] && [ "$1" = "$STUB_INVALID_TABLE" ]; then
    return 1
  fi
  return 0
}

# shellcheck disable=SC1090  # $LIB path is computed from the test's own location
. "$LIB"

# --- harness ----------------------------------------------------------------
pass=0
fail=0
ok() { pass=$((pass + 1)); printf 'ok   - %s\n' "$1"; }
no() { fail=$((fail + 1)); printf 'FAIL - %s\n' "$1"; }
reset() {
  unset STUB_FAIL_QUERY STUB_EMPTY_QUERY STUB_INVALID_TABLE \
    stub_rows_from stub_rows_to stub_nonadded 2>/dev/null || true
}

DRIFT=";table=beads,before_rows=10,after_rows=10,before_hash=h1,after_hash=h2,category=same_row_count_hash_drift"
DRIFT2="$DRIFT;table=notes,before_rows=5,after_rows=6,before_hash=h3,after_hash=h4,category=row_count_gain_hash_drift"
DRIFT_DUP="$DRIFT;table=beads,before_rows=10,after_rows=10,before_hash=h1,after_hash=h2,category=row_count_decrease_hash_drift"

# 1. same rows, no removed/modified content -> the live drift was a writer race
reset; stub_rows_from=10; stub_rows_to=10; stub_nonadded=0
if verify_table_drift_via_fixed_commits db H1 H2 "$DRIFT"; then ok "stable across commits -> race disproven"; else no "stable across commits -> race disproven"; fi

# 2. rows gained between the commits, purely additive -> still benign
reset; stub_rows_from=10; stub_rows_to=11; stub_nonadded=0
if verify_table_drift_via_fixed_commits db H1 H2 "$DRIFT"; then ok "additive gain -> race disproven"; else no "additive gain -> race disproven"; fi

# 3. rows LOST between two immutable commits -> genuine drift
reset; stub_rows_from=10; stub_rows_to=9; stub_nonadded=0
if verify_table_drift_via_fixed_commits db H1 H2 "$DRIFT"; then no "row loss -> quarantine"; else ok "row loss -> quarantine"; fi

# 4. same count but rows removed/modified -> genuine drift
reset; stub_rows_from=10; stub_rows_to=10; stub_nonadded=3
if verify_table_drift_via_fixed_commits db H1 H2 "$DRIFT"; then no "removed/modified -> quarantine"; else ok "removed/modified -> quarantine"; fi

# 5. multiple drifted tables, all clean -> race disproven
reset; stub_rows_from=10; stub_rows_to=10; stub_nonadded=0
if verify_table_drift_via_fixed_commits db H1 H2 "$DRIFT2"; then ok "two clean tables -> race disproven"; else no "two clean tables -> race disproven"; fi

# 6. duplicate records for one table are collapsed, not double-probed
reset; stub_rows_from=10; stub_rows_to=10; stub_nonadded=0
if [ "$(fixed_commit_drift_tables "$DRIFT_DUP")" = "beads" ]; then ok "duplicate drift records dedup to one table"; else no "duplicate drift records dedup to one table"; fi

# 7. row-count probe failure -> fail closed
reset; STUB_FAIL_QUERY="AS OF 'H2'"
if verify_table_drift_via_fixed_commits db H1 H2 "$DRIFT"; then no "count probe failure -> quarantine"; else ok "count probe failure -> quarantine"; fi

# 8. diff probe failure -> fail closed
reset; stub_rows_from=10; stub_rows_to=10; STUB_FAIL_QUERY="DOLT_DIFF("
if verify_table_drift_via_fixed_commits db H1 H2 "$DRIFT"; then no "diff probe failure -> quarantine"; else ok "diff probe failure -> quarantine"; fi

# 9. empty / non-numeric count -> fail closed
reset; STUB_EMPTY_QUERY="AS OF 'H1'"
if verify_table_drift_via_fixed_commits db H1 H2 "$DRIFT"; then no "empty count -> quarantine"; else ok "empty count -> quarantine"; fi

# 10. empty / non-numeric diff result -> fail closed
reset; stub_rows_from=10; stub_rows_to=10; STUB_EMPTY_QUERY="DOLT_DIFF("
if verify_table_drift_via_fixed_commits db H1 H2 "$DRIFT"; then no "empty diff result -> quarantine"; else ok "empty diff result -> quarantine"; fi

# 11. missing from-commit -> fail closed (a working-root hash is not a committish)
reset; stub_rows_from=10; stub_rows_to=10; stub_nonadded=0
if verify_table_drift_via_fixed_commits db "" H2 "$DRIFT"; then no "missing from-commit -> quarantine"; else ok "missing from-commit -> quarantine"; fi

# 12. missing to-commit -> fail closed
reset; stub_rows_from=10; stub_rows_to=10; stub_nonadded=0
if verify_table_drift_via_fixed_commits db H1 "" "$DRIFT"; then no "missing to-commit -> quarantine"; else ok "missing to-commit -> quarantine"; fi

# 13. no drifted table to check is not a proof of anything
reset
if verify_table_drift_via_fixed_commits db H1 H2 ""; then no "empty drift details -> quarantine"; else ok "empty drift details -> quarantine"; fi

# 14. invalid table name -> fail closed
reset; STUB_INVALID_TABLE=beads
if verify_table_drift_via_fixed_commits db H1 H2 "$DRIFT"; then no "invalid table name -> quarantine"; else ok "invalid table name -> quarantine"; fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
