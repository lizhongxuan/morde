#!/usr/bin/env bash
# smoke.sh — run the P0 smoke 10 cases (design proposal §16.3) in one
# controlled run and record the result as JSON.
#
# Usage: deploy/single/smoke.sh
#   SMOKE_CASES   TSV path (default: smoke-cases.tsv next to this script)
#   SMOKE_RECORD  JSON output path (default: smoke-record.json next to this script)
#   GO_BIN        go binary (default: `go` on PATH, else the persistent toolchain)
#
# Fail-closed (P3-3): per-case status is pass / fail / exception only.
#   pass      the mapped check ran and produced passing evidence
#   fail      the mapped check ran and failed, OR produced no evidence at
#             all (mapped test no longer exists, every mapped test skipped,
#             every subtest of a mapped test skipped), OR the table row
#             itself is malformed (unknown needs / unknown check kind)
#   exception the mapped check cannot run in this environment, and the
#             reason is one of the two auto-detected environment limits:
#             needs CH_TEST_DATABASE_URL and the variable is unset, or the
#             check is manual / needs real-pc (a real PC + real model/DSH,
#             never available to an automated gate). An exception is NOT a
#             pass: it is listed in the record and printed as a notice, so
#             the deploy log shows exactly which evidence is missing.
# The gate exits 0 only when fail == 0. An exception never fails the gate
# by itself — but the exception is a gap in evidence, not a pass, and the
# notice below the summary says so.
# Every record entry carries the §16.3 level and what the mapped check
# actually executed, so a pass never claims an unexercised layer passed.
#
# Evidence parsing: a go-test check runs `go test -v` and the verdict lines
# (`--- PASS/FAIL/SKIP:`) are parsed leaf by leaf. A top-level test with
# subtests is judged by its subtests only — a parent that passes while all
# its subtests skipped produced no evidence and fails the case. A test
# that only skips is not a pass: exit code 0 from `go test` does not by
# itself prove anything ran.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
SERVER_DIR="$REPO_ROOT/server"
CASES_FILE="${SMOKE_CASES:-$SCRIPT_DIR/smoke-cases.tsv}"
RECORD_FILE="${SMOKE_RECORD:-$SCRIPT_DIR/smoke-record.json}"

[[ -f "$CASES_FILE" ]] || { echo "smoke: cases file not found: $CASES_FILE" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "smoke: python3 is required (JSON record)" >&2; exit 2; }

# Go discovery: explicit GO_BIN, else `go` on PATH, else the persistent
# toolchain kept under /home/hatch/go (this VM has no /usr/bin/go).
GO=""
if [[ -n "${GO_BIN:-}" ]]; then
	GO="$GO_BIN"
elif command -v go >/dev/null 2>&1; then
	GO="$(command -v go)"
else
	GO="$(ls -d /home/hatch/go/pkg/mod/golang.org/toolchain@*/bin/go 2>/dev/null | sort -V | tail -n 1 || true)"
	if [[ -n "$GO" ]]; then
		export GOTOOLCHAIN=local
	fi
fi
[[ -n "$GO" && -x "$GO" ]] || { echo "smoke: no usable go binary (set GO_BIN)" >&2; exit 2; }

HEAD="$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || echo unknown)"
RUN_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

RESULTS="$(mktemp)"
trap 'rm -f "$RESULTS"' EXIT

# json_case <id> <level> <status> <executed> <detail>
json_case() {
	SMOKE_ID="$1" SMOKE_LEVEL="$2" SMOKE_STATUS="$3" SMOKE_EXECUTED="$4" SMOKE_DETAIL="$5" python3 -c \
		'import json,os; print(json.dumps({"case":os.environ["SMOKE_ID"],"level":os.environ["SMOKE_LEVEL"],"status":os.environ["SMOKE_STATUS"],"executed":os.environ["SMOKE_EXECUTED"],"detail":os.environ["SMOKE_DETAIL"]},ensure_ascii=False))' >>"$RESULTS"
}

# go_test_verdict <output-file>: print "pass" or "fail: <reason>" for a
# `go test -v` run whose exit code was 0. Judged leaf by leaf (see header).
go_test_verdict() {
	SMOKE_OUT="$1" python3 - <<'PYEOF'
import os, re, sys
line_re = re.compile(r"^ *--- (PASS|FAIL|SKIP): (\S+)")
tops = {}
with open(os.environ["SMOKE_OUT"], encoding="utf-8", errors="replace") as f:
    for line in f:
        m = line_re.match(line.rstrip("\n"))
        if not m:
            continue
        verdict, name = m.groups()
        top, _, sub = name.partition("/")
        d = tops.setdefault(top, {"sub": False, "pass": 0, "fail": 0, "skip": 0, "own": None})
        if sub:
            d["sub"] = True
            d[{"PASS": "pass", "FAIL": "fail", "SKIP": "skip"}[verdict]] += 1
        else:
            d["own"] = verdict
failed, skipped = [], []
for top, d in tops.items():
    if d["sub"]:
        if d["fail"] > 0:
            failed.append(top)
        elif d["pass"] == 0:
            skipped.append(top)
    elif d["own"] == "FAIL":
        failed.append(top)
    elif d["own"] != "PASS":
        skipped.append(top)
if failed:
    print("fail: test failed: " + ", ".join(sorted(failed)))
elif skipped:
    print("fail: no test evidence, all skipped: " + ", ".join(sorted(skipped)))
elif not tops:
    print("fail: no test evidence, no test verdicts in output")
else:
    print("pass")
PYEOF
}

pass_count=0
fail_count=0
exception_count=0

while IFS=$'\t' read -r case_id level description check needs; do
	[[ -z "${case_id:-}" || "$case_id" =~ ^# ]] && continue
	case "$needs" in
		CH_TEST_DATABASE_URL)
			if [[ -z "${CH_TEST_DATABASE_URL:-}" ]]; then
				json_case "$case_id" "$level" "exception" "none" "needs CH_TEST_DATABASE_URL (PostgreSQL integration test)"
				exception_count=$((exception_count + 1))
				continue
			fi
			;;
		real-pc)
			json_case "$case_id" "$level" "exception" "none" "needs real PC + real model/DSH; cannot run in an automated gate"
			exception_count=$((exception_count + 1))
			continue
			;;
		none) ;;
		*)
			json_case "$case_id" "$level" "fail" "none" "unknown needs value: $needs"
			fail_count=$((fail_count + 1))
			continue
			;;
	esac
	case "$check" in
		go-test:*)
			rest="${check#go-test:}"
			pkg="${rest%%:*}"
			regex="${rest#*:}"
			out="$(mktemp)"
			if (cd "$SERVER_DIR" && "$GO" test -count=1 -timeout=5m -v -run "$regex" "./$pkg/" >"$out" 2>&1); then
				if grep -q "no tests to run" "$out"; then
					json_case "$case_id" "$level" "fail" "none" "mapped test no longer exists: $pkg -run $regex"
					fail_count=$((fail_count + 1))
				else
					verdict="$(go_test_verdict "$out")"
					if [[ "$verdict" == "pass" ]]; then
						json_case "$case_id" "$level" "pass" "service" "go test passed: $pkg -run $regex"
						pass_count=$((pass_count + 1))
					else
						json_case "$case_id" "$level" "fail" "service" "go test passed with no evidence ($pkg -run $regex): ${verdict#fail: }"
						fail_count=$((fail_count + 1))
					fi
				fi
			else
				detail="$(tail -n 5 "$out" | tr '\n' ' ' | sed 's/  */ /g')"
				json_case "$case_id" "$level" "fail" "service" "go test failed: $pkg -run $regex: $detail"
				fail_count=$((fail_count + 1))
			fi
			rm -f "$out"
			;;
		manual:*)
			json_case "$case_id" "$level" "exception" "none" "${check#manual:}"
			exception_count=$((exception_count + 1))
			;;
		*)
			json_case "$case_id" "$level" "fail" "none" "unknown check kind: $check"
			fail_count=$((fail_count + 1))
			;;
	esac
done <"$CASES_FILE"

SMOKE_RECORD="$RECORD_FILE" SMOKE_HEAD="$HEAD" SMOKE_RUN_AT="$RUN_AT" SMOKE_RESULTS="$RESULTS" python3 - <<'PYEOF'
import json, os, sys
results = []
with open(os.environ["SMOKE_RESULTS"], encoding="utf-8") as f:
    for line in f:
        line = line.strip()
        if line:
            results.append(json.loads(line))
counts = {"pass": 0, "fail": 0, "exception": 0}
for entry in results:
    counts[entry["status"]] = counts.get(entry["status"], 0) + 1
record = {
    "head": os.environ["SMOKE_HEAD"],
    "run_at": os.environ["SMOKE_RUN_AT"],
    "counts": counts,
    "cases": results,
}
with open(os.environ["SMOKE_RECORD"], "w", encoding="utf-8") as f:
    json.dump(record, f, ensure_ascii=False, indent=2)
    f.write("\n")
exceptions = [e for e in results if e["status"] == "exception"]
if exceptions:
    detail = "; ".join(f'{e["case"]} ({e["detail"]})' for e in exceptions)
    print(f"smoke: EXCEPTION ({len(exceptions)} case(s) not run — environment limits, not counted as pass): {detail}", file=sys.stderr)
PYEOF

echo "smoke: pass=$pass_count fail=$fail_count exception=$exception_count"
echo "smoke record: $RECORD_FILE"
if ((fail_count > 0)); then
	echo "smoke: FAILED ($fail_count case(s) failed)" >&2
	exit 1
fi
exit 0
