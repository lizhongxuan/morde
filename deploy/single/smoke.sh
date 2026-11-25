#!/usr/bin/env bash
# smoke.sh — P0 smoke 10 deploy/merge gate (E10).
#
# Runs the ten P0 smoke cases (T01, T04, T06, T10, T13, T19, T22, T26,
# T36, T43) as one controlled batch, writes a JSON record, and exits
# non-zero when any runnable case fails. A deploy that does not run this
# gate is blocked: update.sh runs it before building, and refuses to
# deploy when it fails.
#
# Case results:
#   pass    — at least one mapped test passed, none failed or missing.
#   fail    — a mapped test failed, a mapped test was not found (the
#             mapping went stale), or `go test` itself failed.
#   blocked — no mapped test could run. T10/T43 are blocked by design
#             (they need a real PC + real model/DSH and cannot run
#             headless); a PostgreSQL-backed test is blocked when
#             CH_TEST_DATABASE_URL is not set (those tests skip by repo
#             rule). Blocked is reported, never passed and never failed.
#
# The gate blocks on fail only. A blocked case is visible in the record
# and in the summary, so a deploy never silently claims full coverage.
#
# Usage:
#   smoke.sh [--src <repo-root>] [--record <path>] [--timeout <seconds>]
#            [--commit <sha>] [--list]
#   --src:     repo root (default: the checkout this script lives in).
#   --record:  JSON record path (default: ./smoke-record.json).
#   --timeout: per `go test` invocation timeout in seconds (default 600).
#   --commit:  commit the record is for (default: git rev-parse HEAD).
#   --list:    print the ten cases with their mapped tests and exit 0.
#
# Environment:
#   CH_TEST_DATABASE_URL — when set, the PostgreSQL-backed smoke tests
#     run against it (they create and drop their own temporary
#     databases); when unset they skip and are recorded as blocked.
#   TMPDIR — honoured by `go test` for its temporary files.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="${SRC:-$SCRIPT_DIR/../..}"
SRC="$(cd "$SRC" && pwd)"
SERVER_DIR="$SRC/server"
CASES_FILE="$SCRIPT_DIR/smoke-cases.tsv"
RECORD="./smoke-record.json"
TIMEOUT=600
COMMIT=""
LIST_ONLY=0

while (($# > 0)); do
  case "$1" in
    --src) SRC="$(cd "$2" && pwd)"; SERVER_DIR="$SRC/server"; shift 2 ;;
    --record) RECORD="$2"; shift 2 ;;
    --timeout) TIMEOUT="$2"; shift 2 ;;
    --commit) COMMIT="$2"; shift 2 ;;
    --list) LIST_ONLY=1; shift ;;
    -h | --help) sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "smoke: unknown argument: $1 (see --help)" >&2; exit 2 ;;
  esac
done

[[ -f "$CASES_FILE" ]] || { echo "smoke: missing case table: $CASES_FILE" >&2; exit 2; }
[[ -d "$SERVER_DIR" ]] || { echo "smoke: server dir not found: $SERVER_DIR" >&2; exit 2; }

# The ten cases, in order. T10/T43 have no rows in the table: blocked.
CASES=(T01 T04 T06 T10 T13 T19 T22 T26 T36 T43)

blocked_reason() {
  case "$1" in
    T10) echo "needs a real PC + real model; cannot run headless" ;;
    T43) echo "needs a real PC + DSH + isolated environment; cannot run headless" ;;
    *) echo "" ;;
  esac
}

if [[ "$LIST_ONLY" == "1" ]]; then
  for case_id in "${CASES[@]}"; do
    reason="$(blocked_reason "$case_id")"
    if [[ -n "$reason" ]]; then
      echo "$case_id blocked: $reason"
      continue
    fi
    while IFS=$'\t' read -r c pkg test_name; do
      [[ "$c" == "$case_id" ]] || continue
      echo "$case_id $pkg $test_name"
    done < <(grep -v '^#' "$CASES_FILE" | grep -v '^$')
  done
  exit 0
fi

command -v go >/dev/null 2>&1 || { echo "smoke: go is not on PATH" >&2; exit 2; }
if [[ -z "$COMMIT" ]]; then
  COMMIT="$(git -C "$SRC" rev-parse HEAD 2>/dev/null || echo unknown)"
fi
BRANCH="$(git -C "$SRC" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
if [[ -n "${CH_TEST_DATABASE_URL:-}" ]]; then
  TEST_DB="set"
else
  TEST_DB="unset"
  echo "smoke: CH_TEST_DATABASE_URL is not set; PostgreSQL-backed tests will skip and be recorded as blocked"
fi

STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
TMPDIR_SMOKE="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_SMOKE"' EXIT

# Per-case results, kept in files so the record writer can read them.
RESULTS_DIR="$TMPDIR_SMOKE/results"
mkdir -p "$RESULTS_DIR"
pass_count=0
fail_count=0
blocked_count=0

# run_case <case-id>: runs the case's tests, writes one line per test
# (name, package, status) to $RESULTS_DIR/<case-id>.tsv and prints the
# case status on stdout.
run_case() {
  local case_id="$1"
  local reason=""
  reason="$(blocked_reason "$case_id")"
  if [[ -n "$reason" ]]; then
    printf 'blocked\t%s\n' "$reason" >"$RESULTS_DIR/$case_id.status"
    echo "$case_id: BLOCKED ($reason)"
    return 0
  fi
  # Group the case's tests by package: one `go test` per package.
  local pkg
  local -a pkgs
  mapfile -t pkgs < <(awk -F'\t' -v c="$case_id" '$1 == c {print $2}' "$CASES_FILE" | sort -u)
  if [[ "${#pkgs[@]}" == "0" ]]; then
    printf 'blocked\t%s\n' "no mapped tests" >"$RESULTS_DIR/$case_id.status"
    echo "$case_id: BLOCKED (no mapped tests)"
    return 0
  fi
  local case_failed=0
  local case_ran=0
  local case_skipped=0
  : >"$RESULTS_DIR/$case_id.tsv"
  for pkg in "${pkgs[@]}"; do
    local -a names
    mapfile -t names < <(awk -F'\t' -v c="$case_id" -v p="$pkg" '$1 == c && $2 == p {print $3}' "$CASES_FILE")
    local regex="^($(IFS='|'; echo "${names[*]}"))$"
    local out="$TMPDIR_SMOKE/$case_id.$(echo "$pkg" | tr '/' '_').log"
    local go_rc=0
    (cd "$SERVER_DIR" && go test -count=1 -timeout "${TIMEOUT}s" -v "./$pkg/" -run "$regex" >"$out" 2>&1) || go_rc=$?
    local name status
    for name in "${names[@]}"; do
      # A test whose body is only subtests that skip still prints a
      # parent `--- PASS:` line; that is a hollow pass, not evidence.
      # A test counts as passed only when it, or at least one of its
      # subtests, actually ran and passed. (Subtest verdict lines are
      # indented, so the patterns are not anchored to column 0.)
      if grep -q -e "--- FAIL: $name (" "$out" 2>/dev/null || grep -q -e "--- FAIL: $name/" "$out" 2>/dev/null; then
        status="fail"
      elif grep -q -e "--- PASS: $name/" "$out" 2>/dev/null; then
        status="pass"
      elif grep -q -e "--- PASS: $name (" "$out" 2>/dev/null; then
        if grep -q -e "--- SKIP: $name/" "$out" 2>/dev/null; then
          status="skip"
        else
          status="pass"
        fi
      elif grep -q -e "--- SKIP: $name (" "$out" 2>/dev/null; then
        status="skip"
      else
        status="missing"
      fi
      printf '%s\t%s\t%s\n' "$name" "$pkg" "$status" >>"$RESULTS_DIR/$case_id.tsv"
      case "$status" in
        pass) case_ran=1 ;;
        fail | missing) case_failed=1 ;;
        skip) case_skipped=1 ;;
      esac
    done
    if [[ "$go_rc" != "0" && "$case_failed" == "0" ]]; then
      # `go test` failed without failing a mapped test (build failure,
      # TestMain failure, panic): the case cannot be trusted as passed.
      case_failed=1
      echo "smoke: go test failed for $pkg (exit $go_rc); last lines of $out:" >&2
      tail -5 "$out" >&2 || true
    fi
  done
  if [[ "$case_failed" == "1" ]]; then
    echo "fail" >"$RESULTS_DIR/$case_id.status"
    echo "$case_id: FAIL"
  elif [[ "$case_ran" == "0" ]]; then
    local why="no mapped test ran"
    [[ "$case_skipped" == "1" ]] && why="all mapped tests skipped (no test database?)"
    printf 'blocked\t%s\n' "$why" >"$RESULTS_DIR/$case_id.status"
    echo "$case_id: BLOCKED ($why)"
  else
    echo "pass" >"$RESULTS_DIR/$case_id.status"
    if [[ "$case_skipped" == "1" ]]; then
      echo "$case_id: PASS (some mapped tests skipped; see the record)"
    else
      echo "$case_id: PASS"
    fi
  fi
}

for case_id in "${CASES[@]}"; do
  run_case "$case_id"
done

FINISHED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# ---- JSON record (all values here are controlled strings) ----
{
  echo "{"
  echo "  \"commit\": \"$COMMIT\","
  echo "  \"branch\": \"$BRANCH\","
  echo "  \"started_at\": \"$STARTED_AT\","
  echo "  \"finished_at\": \"$FINISHED_AT\","
  echo "  \"test_database\": \"$TEST_DB\","
  echo "  \"cases\": ["
  first_case=1
  for case_id in "${CASES[@]}"; do
    [[ "$first_case" == "1" ]] || echo ","
    first_case=0
    status="$(cut -f1 "$RESULTS_DIR/$case_id.status")"
    reason="$(cut -s -f2 "$RESULTS_DIR/$case_id.status" 2>/dev/null || true)"
    case "$status" in
      pass) pass_count=$((pass_count + 1)) ;;
      fail) fail_count=$((fail_count + 1)) ;;
      *) blocked_count=$((blocked_count + 1)) ;;
    esac
    printf '    {"id": "%s", "status": "%s", "reason": "%s", "tests": [' "$case_id" "$status" "$reason"
    first_test=1
    if [[ -f "$RESULTS_DIR/$case_id.tsv" ]]; then
      while IFS=$'\t' read -r tname tpkg tstatus; do
        [[ "$first_test" == "1" ]] || printf ', '
        first_test=0
        printf '{"name": "%s", "package": "%s", "status": "%s"}' "$tname" "$tpkg" "$tstatus"
      done <"$RESULTS_DIR/$case_id.tsv"
    fi
    printf ']}'
  done
  echo ""
  echo "  ],"
  echo "  \"summary\": {\"pass\": $pass_count, \"fail\": $fail_count, \"blocked\": $blocked_count}"
  echo "}"
} >"$RECORD"

echo "smoke: pass=$pass_count fail=$fail_count blocked=$blocked_count (record: $RECORD)"
if [[ "$fail_count" != "0" ]]; then
  echo "smoke: FAILED — see the record for the failing tests; the deploy is blocked"
  exit 1
fi
exit 0
