package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// The P0 smoke 10 gate (E10) lives in deploy/single/smoke.sh with its
// case table in deploy/single/smoke-cases.tsv. These tests check the
// table against the real test suite and the gate's pass/fail/blocked
// accounting, using a fake `go` binary so no real test, database, DSH
// or model is ever started.

// repoRoot walks up from this test file until deploy/single/smoke.sh
// is found.
func smokeGateRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "deploy", "single", "smoke.sh")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("deploy/single/smoke.sh not found above the test file")
		}
		dir = parent
	}
}

type smokeCaseRow struct {
	caseID string
	pkg    string
	test   string
}

func parseSmokeCases(t *testing.T, path string) []smokeCaseRow {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read smoke cases: %v", err)
	}
	var rows []smokeCaseRow
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			t.Fatalf("smoke-cases.tsv: bad line %q (want case, package, test)", line)
		}
		rows = append(rows, smokeCaseRow{caseID: fields[0], pkg: fields[1], test: fields[2]})
	}
	return rows
}

func TestSmokeCasesTableMatchesRealTests(t *testing.T) {
	root := smokeGateRepoRoot(t)
	rows := parseSmokeCases(t, filepath.Join(root, "deploy", "single", "smoke-cases.tsv"))

	wantCases := []string{"T01", "T04", "T06", "T10", "T13", "T19", "T22", "T26", "T36", "T43"}
	// T10/T43 are blocked by design and have no rows in the table; the
	// table must cover exactly the other eight.
	blockedByDesign := []string{"T10", "T43"}
	byCase := map[string][]smokeCaseRow{}
	for _, row := range rows {
		byCase[row.caseID] = append(byCase[row.caseID], row)
	}
	gotCases := []string{}
	for id := range byCase {
		gotCases = append(gotCases, id)
	}
	gotCases = append(gotCases, blockedByDesign...)
	sort.Strings(gotCases)
	if len(gotCases) != len(wantCases) {
		t.Fatalf("smoke cases = %v, want %v", gotCases, wantCases)
	}
	for i, id := range wantCases {
		if gotCases[i] != id {
			t.Fatalf("smoke cases = %v, want %v", gotCases, wantCases)
		}
	}

	// T10/T43 are blocked by design (real PC + real model/DSH) and must
	// have no runnable tests; every other case must have at least one.
	for _, id := range []string{"T10", "T43"} {
		if len(byCase[id]) != 0 {
			t.Fatalf("case %s must have no mapped tests (blocked by design), got %d", id, len(byCase[id]))
		}
	}
	for _, id := range wantCases {
		if id == "T10" || id == "T43" {
			continue
		}
		if len(byCase[id]) == 0 {
			t.Fatalf("case %s has no mapped tests", id)
		}
	}

	// Every mapped test must exist exactly once as a Go test function
	// under server/internal, and its package must match the table.
	// (Unrelated duplicate test names elsewhere in the repo are not
	// this test's business; only a mapped test with two homes is.)
	testFunc := regexp.MustCompile(`^func (Test\w+)\(t \*testing\.T\)`)
	found := map[string][]string{} // test name -> package dirs (relative to server/)
	err := filepath.WalkDir(filepath.Join(root, "server", "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		pkg, _ := filepath.Rel(filepath.Join(root, "server"), filepath.Dir(path))
		for _, line := range strings.Split(string(data), "\n") {
			if m := testFunc.FindStringSubmatch(line); m != nil {
				found[m[1]] = append(found[m[1]], pkg)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk server/internal: %v", err)
	}
	for _, row := range rows {
		pkgs, ok := found[row.test]
		if !ok {
			t.Errorf("case %s maps to missing test %s (mapping is stale)", row.caseID, row.test)
			continue
		}
		if len(pkgs) != 1 {
			t.Errorf("case %s test %s exists in %d packages %v, want exactly one", row.caseID, row.test, len(pkgs), pkgs)
			continue
		}
		if pkgs[0] != row.pkg {
			t.Errorf("case %s test %s lives in %s, table says %s", row.caseID, row.test, pkgs[0], row.pkg)
		}
	}
}

// fakeGoScript is a stand-in for `go test`: it reads the -run regex,
// prints one --- PASS/FAIL/SKIP line per named test and exits non-zero
// when any test fails. FAKE_GO_FAIL / FAKE_GO_SKIP / FAKE_GO_OMIT /
// FAKE_GO_SUBSKIP are comma-separated test names. SUBSKIP mimics a test
// whose subtests all skip: the parent still prints --- PASS, which the
// gate must not count as evidence.
const fakeGoScript = `#!/bin/sh
run=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-run" ]; then run="$a"; fi
  prev="$a"
done
case "$run" in
  *" "*) echo "malformed -run regex contains a space: $run" >&2; exit 2 ;;
esac
names=$(printf '%s' "$run" | tr -d '^()$' | tr '|' ' ')
failed=0
for n in $names; do
  case ",${FAKE_GO_FAIL:-}," in
    *",$n,"*) echo "--- FAIL: $n (0.00s)"; failed=1 ;;
    *) case ",${FAKE_GO_SKIP:-}," in
         *",$n,"*) echo "--- SKIP: $n (0.00s)" ;;
         *) case ",${FAKE_GO_OMIT:-}," in
              *",$n,"*) ;;
              *) case ",${FAKE_GO_SUBSKIP:-}," in
                   *",$n,"*) echo "--- SKIP: $n/sub (0.00s)"; echo "--- PASS: $n (0.00s)" ;;
                   *) echo "--- PASS: $n (0.00s)" ;;
                 esac ;;
            esac ;;
       esac ;;
  esac
done
exit "$failed"
`

type smokeRecord struct {
	Commit  string `json:"commit"`
	Summary struct {
		Pass      int `json:"pass"`
		Fail      int `json:"fail"`
		Exception int `json:"exception"`
	} `json:"summary"`
	Cases []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Tests  []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"tests"`
	} `json:"cases"`
}

func runSmokeGate(t *testing.T, root string, fakeGoDir string, env map[string]string) (int, smokeRecord) {
	t.Helper()
	tmp := t.TempDir()
	recordPath := filepath.Join(tmp, "smoke-record.json")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(root, "deploy", "single", "smoke.sh"),
		"--record", recordPath, "--commit", "test-commit")
	// Each fake scenario must choose its own database and fake-go inputs;
	// inheriting a real integration DSN changes the expected skip verdict.
	cmd.Env = append(os.Environ(), "PATH="+fakeGoDir+":"+os.Getenv("PATH"), "CH_TEST_DATABASE_URL=",
		"FAKE_GO_FAIL=", "FAKE_GO_SKIP=", "FAKE_GO_OMIT=", "FAKE_GO_SUBSKIP=")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	t.Logf("smoke output:\n%s", out)
	var rec smokeRecord
	data, readErr := os.ReadFile(recordPath)
	if readErr != nil {
		t.Fatalf("smoke record not written: %v\noutput:\n%s", readErr, out)
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("smoke record is not valid JSON: %v\n%s", err, data)
	}
	if rec.Commit != "test-commit" {
		t.Errorf("record commit = %q, want test-commit", rec.Commit)
	}
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("run smoke.sh: %v", err)
		}
	}
	return exitCode, rec
}

func writeFakeGo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "go")
	if err := os.WriteFile(path, []byte(fakeGoScript), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}
	return dir
}

func caseStatus(rec smokeRecord, id string) string {
	for _, c := range rec.Cases {
		if c.ID == id {
			return c.Status
		}
	}
	return "<missing>"
}

func TestSmokeGateAccounting(t *testing.T) {
	root := smokeGateRepoRoot(t)
	fakeGoDir := writeFakeGo(t)

	t.Run("all runnable tests pass", func(t *testing.T) {
		code, rec := runSmokeGate(t, root, fakeGoDir, nil)
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
		if rec.Summary.Pass != 8 || rec.Summary.Fail != 0 || rec.Summary.Exception != 2 {
			t.Errorf("summary = %+v, want pass=8 fail=0 exception=2", rec.Summary)
		}
		for _, id := range []string{"T10", "T43"} {
			if got := caseStatus(rec, id); got != "exception" {
				t.Errorf("case %s = %q, want exception", id, got)
			}
		}
	})

	t.Run("one failing test fails the gate", func(t *testing.T) {
		code, rec := runSmokeGate(t, root, fakeGoDir, map[string]string{
			"FAKE_GO_FAIL": "TestConversationRouteSteerRequiresOneAuthorizedRunningTask",
		})
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero")
		}
		if got := caseStatus(rec, "T06"); got != "fail" {
			t.Errorf("case T06 = %q, want fail", got)
		}
		if rec.Summary.Fail != 1 {
			t.Errorf("summary fail = %d, want 1", rec.Summary.Fail)
		}
		for _, c := range rec.Cases {
			if c.ID != "T06" {
				continue
			}
			for _, tst := range c.Tests {
				if tst.Name == "TestConversationRouteSteerRequiresOneAuthorizedRunningTask" && tst.Status != "fail" {
					t.Errorf("failing test recorded as %q, want fail", tst.Status)
				}
			}
		}
	})

	t.Run("missing test fails the gate", func(t *testing.T) {
		code, rec := runSmokeGate(t, root, fakeGoDir, map[string]string{
			"FAKE_GO_OMIT": "TestTaskApprovalDisclosuresRequireKnownValuesAndCostBasis",
		})
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero")
		}
		if got := caseStatus(rec, "T19"); got != "fail" {
			t.Errorf("case T19 = %q, want fail (a mapped test did not run)", got)
		}
	})

	// Fail-closed: a case whose mapped tests all skip produces no
	// evidence, so it fails the gate — unless the skip is fully
	// explained by the missing test database (next subtest). The
	// database URL is set here so the skip is unexplained.
	t.Run("all tests of a case skipped fails the gate", func(t *testing.T) {
		code, rec := runSmokeGate(t, root, fakeGoDir, map[string]string{
			"CH_TEST_DATABASE_URL": "postgres://fake/smoke-test",
			"FAKE_GO_SKIP":         "TestPostgresTaskSubmissionAndMessageReceiptReplay,TestPostgresConcurrentMessageReceiptReplaySurvivesTaskStateChange",
		})
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero (no evidence is a failure)")
		}
		if got := caseStatus(rec, "T01"); got != "fail" {
			t.Errorf("case T01 = %q, want fail", got)
		}
		if rec.Summary.Fail != 1 {
			t.Errorf("summary fail = %d, want 1", rec.Summary.Fail)
		}
	})

	// The missing test database is the one skip the gate excuses: every
	// mapped test of the case is PostgreSQL-backed and the URL is unset,
	// so the environment — not the code — cannot produce the evidence.
	// It is recorded as an exception-gate notice, not a failure.
	t.Run("missing test database is an exception not a failure", func(t *testing.T) {
		code, rec := runSmokeGate(t, root, fakeGoDir, map[string]string{
			"FAKE_GO_SKIP": "TestPostgresTaskSubmissionAndMessageReceiptReplay,TestPostgresConcurrentMessageReceiptReplaySurvivesTaskStateChange",
		})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (environment limitation is an exception, not a failure)", code)
		}
		if got := caseStatus(rec, "T01"); got != "exception" {
			t.Errorf("case T01 = %q, want exception", got)
		}
		if rec.Summary.Pass != 7 || rec.Summary.Fail != 0 || rec.Summary.Exception != 3 {
			t.Errorf("summary = %+v, want pass=7 fail=0 exception=3", rec.Summary)
		}
	})

	// A test whose body is only subtests that skip prints a parent
	// `--- PASS:` line. The gate must not count that hollow pass as
	// evidence: the case fails, it is not passed and not excepted.
	t.Run("subtests all skipped fails the gate", func(t *testing.T) {
		code, rec := runSmokeGate(t, root, fakeGoDir, map[string]string{
			"CH_TEST_DATABASE_URL": "postgres://fake/smoke-test",
			"FAKE_GO_SUBSKIP":      "TestConversationResultAggregationConcurrentRevisionCannotPublishOldResult,TestConversationResultAggregationQueuesSingleAndPreservesStream",
		})
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero (hollow pass is not evidence)")
		}
		if got := caseStatus(rec, "T26"); got != "fail" {
			t.Errorf("case T26 = %q, want fail (its tests only skipped inside subtests)", got)
		}
		if rec.Summary.Fail != 1 {
			t.Errorf("summary fail = %d, want 1", rec.Summary.Fail)
		}
	})
}
