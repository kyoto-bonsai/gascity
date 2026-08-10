#!/bin/sh
# compact-fixed-commit-verify.sh — race-free fallback for the per-table
# post-flatten integrity check.
#
# verify_counts reads DOLT_HASHOF_TABLE, which Dolt hard-codes to the WORKING
# root (sql.Function1, Eval reads roots.Working — there is no ref argument). On a
# continuously-written database every per-table read races a live writer, so
# "the hash moved" can never be told apart from corruption. The per-table pass
# therefore defers on a detected race — but a database that is NEVER idle would
# defer forever, and a real corruption would never be caught. Deferral is bounded
# for that reason, and past the bound the decision is made here instead.
#
# Both endpoints are already-written Dolt commits, which are immutable, so
# reading AS OF them has no race window no matter how busy the database is.
# That is also why the endpoints must be real commits: a working-root hash names
# a root value, not a committish, and cannot be resolved AS OF.
#
# This path only runs after the bound is exceeded, so a full-table scan is
# acceptable here; it must never be reached on the common path.
#
# Depends on `query_single_cell` and `valid_table_name` from run.sh.

# fixed_commit_drift_tables <verify_counts_drift_details>
# Emit the deduplicated table names from verify_counts' `;`-separated detail
# string (records look like `table=beads,before_rows=10,...,category=...`).
fixed_commit_drift_tables() {
  _fc_seen=""
  for _fc_rec in $(printf '%s' "$1" | tr ';' ' '); do
    case "$_fc_rec" in
      table=*) ;;
      *) continue ;;
    esac
    _fc_t=${_fc_rec#table=}
    _fc_t=${_fc_t%%,*}
    [ -n "$_fc_t" ] || continue
    case " $_fc_seen " in
      *" $_fc_t "*) continue ;;
    esac
    _fc_seen="$_fc_seen $_fc_t"
  done
  printf '%s\n' "${_fc_seen# }"
}

# verify_table_drift_via_fixed_commits <db> <from_commit> <to_commit> <drift_details>
# Returns 0 iff every drifted table is provably intact between the two commits:
# no rows lost, and no row removed or modified (pure additions are concurrent
# writer data, exactly as the live decision tree treats a gain with a stable
# hash). Returns non-zero when genuine drift is confirmed, and also whenever the
# comparison cannot be made — a missing endpoint, an empty table list, an invalid
# table name, or any probe failure fails closed into quarantine.
verify_table_drift_via_fixed_commits() {
  _fv_db="$1"
  _fv_from="$2"
  _fv_to="$3"
  _fv_details="$4"

  [ -n "$_fv_from" ] && [ -n "$_fv_to" ] || return 1

  _fv_seen=0
  for _fv_t in $(fixed_commit_drift_tables "$_fv_details"); do
    _fv_seen=1
    valid_table_name "$_fv_t" || return 1

    if ! _fv_from_rows=$(query_single_cell "$_fv_db" \
      "fixed-commit row count probe failed for table=$_fv_t at $_fv_from" \
      "SELECT COUNT(*) FROM \`$_fv_t\` AS OF '$_fv_from'"); then
      return 1
    fi
    case "$_fv_from_rows" in ''|*[!0-9]*) return 1 ;; esac

    if ! _fv_to_rows=$(query_single_cell "$_fv_db" \
      "fixed-commit row count probe failed for table=$_fv_t at $_fv_to" \
      "SELECT COUNT(*) FROM \`$_fv_t\` AS OF '$_fv_to'"); then
      return 1
    fi
    case "$_fv_to_rows" in ''|*[!0-9]*) return 1 ;; esac

    if [ "$_fv_to_rows" -lt "$_fv_from_rows" ]; then
      printf 'compact: db=%s fixed-commit verification table=%s lost rows before=%s after=%s across %s..%s — genuine drift\n' \
        "$_fv_db" "$_fv_t" "$_fv_from_rows" "$_fv_to_rows" "$_fv_from" "$_fv_to" >&2
      return 1
    fi

    # Content, not a hash: DOLT_HASHOF_TABLE cannot be pinned to a ref, and a
    # GROUP_CONCAT signature would silently truncate at group_concat_max_len
    # (1024 bytes by default) and read as "identical" for any corruption past
    # the first few rows. The diff names exactly which rows changed and how.
    if ! _fv_nonadded=$(query_single_cell "$_fv_db" \
      "fixed-commit content diff probe failed for table=$_fv_t" \
      "SELECT COUNT(*) FROM DOLT_DIFF('$_fv_from', '$_fv_to', '$_fv_t') WHERE diff_type <> 'added'"); then
      return 1
    fi
    case "$_fv_nonadded" in
      0) ;;
      ''|*[!0-9]*) return 1 ;;
      *)
        printf 'compact: db=%s fixed-commit verification table=%s has %s removed/modified row(s) across %s..%s — genuine drift\n' \
          "$_fv_db" "$_fv_t" "$_fv_nonadded" "$_fv_from" "$_fv_to" >&2
        return 1
        ;;
    esac
  done

  # No drifted table to check is not a proof of anything.
  [ "$_fv_seen" = "1" ] || return 1
  return 0
}
