package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// P3-3: the smoke gate is fail-closed. The accounting lives in
// deploy/single/smoke.sh (bash + python), so this test runs the real script
// against fixture tables and asserts the record it writes — it does not
// re-implement the accounting in Go. The fixture table covers every
// outcome the gate must distinguish: a pass, the two auto-detected
// environment exceptions, a mapped test that no longer exists, a test that
// only skips, and a test whose subtests all skip while the parent passes.

type smokeRecord struct {
	Counts map[string]int `json:"counts"`
	Cases  []struct {
		Case   string `json:"case"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"cases"`
}

func smokeGateGoBinary(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("go"); err == nil {
		return path
	}
	matches, _ := filepath.Glob("/home/hatch/go/pkg/mod/golang.org/toolchain@*/bin/go")
	if len(matches) == 0 {
		t.Skip("no go binary available for the smoke gate test")
	}
	sort.Strings(matches)
	return matches[len(matches)-1]
}

func runSmokeGate(t *testing.T, goBin, casesFile string, withDBURL bool) (smokeRecord, int) {
	t.Helper()
	script, err := filepath.Abs("../../../deploy/single/smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("smoke script not found: %v", err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required by the smoke script")
	}
	recordFile := filepath.Join(t.TempDir(), "smoke-record.json")
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", script)
	cmd.Env = nil
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CH_TEST_DATABASE_URL=") {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	if withDBURL {
		cmd.Env = append(cmd.Env, "CH_TEST_DATABASE_URL=postgres://smoke-gate-fixture.invalid:5432/morde")
	}
	cmd.Env = append(cmd.Env,
		"SMOKE_CASES="+casesFile,
		"SMOKE_RECORD="+recordFile,
		"GO_BIN="+goBin,
	)
	if strings.Contains(goBin, "/home/hatch/go/pkg/mod/") {
		cmd.Env = append(cmd.Env, "GOTOOLCHAIN=local")
	}
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	runErr := cmd.Run()
	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("run smoke.sh: %v\noutput:\n%s", runErr, output.String())
		}
	}
	raw, err := os.ReadFile(recordFile)
	if err != nil {
		t.Fatalf("smoke.sh did not write a record: %v\noutput:\n%s", err, output.String())
	}
	var record smokeRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("smoke record is not valid JSON: %v\nrecord:\n%s", err, raw)
	}
	return record, exitCode
}

func smokeCaseByID(t *testing.T, record smokeRecord, id string) (string, string) {
	t.Helper()
	for _, c := range record.Cases {
		if c.Case == id {
			return c.Status, c.Detail
		}
	}
	t.Fatalf("smoke record has no case %q: %+v", id, record.Cases)
	return "", ""
}

func TestSmokeGateAccounting(t *testing.T) {
	goBin := smokeGateGoBinary(t)
	casesFile, err := filepath.Abs("testdata/smoke-gate-fixture.tsv")
	if err != nil {
		t.Fatal(err)
	}
	record, exitCode := runSmokeGate(t, goBin, casesFile, false)
	if exitCode != 1 {
		t.Fatalf("smoke gate exit code = %d, want 1 (failures present)", exitCode)
	}
	if got, want := record.Counts["pass"], 1; got != want {
		t.Fatalf("smoke counts pass = %d, want %d: %+v", got, want, record.Counts)
	}
	if got, want := record.Counts["fail"], 3; got != want {
		t.Fatalf("smoke counts fail = %d, want %d: %+v", got, want, record.Counts)
	}
	if got, want := record.Counts["exception"], 2; got != want {
		t.Fatalf("smoke counts exception = %d, want %d: %+v", got, want, record.Counts)
	}
	// Fail-closed vocabulary: no blocked / missing status may appear.
	for _, c := range record.Cases {
		switch c.Status {
		case "pass", "fail", "exception":
		default:
			t.Fatalf("smoke case %q has status %q, want pass/fail/exception only", c.Case, c.Status)
		}
	}
	cases := map[string]string{
		"FIX-PASS":         "pass",
		"FIX-EXCEPTION-DB": "exception",
		"FIX-EXCEPTION-PC": "exception",
		"FIX-MISSING":      "fail",
		"FIX-SKIP-TOP":     "fail",
		"FIX-SKIP-SUBS":    "fail",
	}
	for id, want := range cases {
		status, _ := smokeCaseByID(t, record, id)
		if status != want {
			t.Fatalf("smoke case %s status = %q, want %q", id, status, want)
		}
	}
	if _, detail := smokeCaseByID(t, record, "FIX-EXCEPTION-DB"); !strings.Contains(detail, "CH_TEST_DATABASE_URL") {
		t.Fatalf("missing-database exception detail does not name the variable: %q", detail)
	}
	if _, detail := smokeCaseByID(t, record, "FIX-MISSING"); !strings.Contains(detail, "no longer exists") {
		t.Fatalf("missing-test failure detail does not say the test is gone: %q", detail)
	}
	for _, id := range []string{"FIX-SKIP-TOP", "FIX-SKIP-SUBS"} {
		if _, detail := smokeCaseByID(t, record, id); !strings.Contains(detail, "skipped") {
			t.Fatalf("all-skipped failure detail does not say skipped (%s): %q", id, detail)
		}
	}
}

func TestSmokeGateExceptionsDoNotFail(t *testing.T) {
	goBin := smokeGateGoBinary(t)
	casesFile, err := filepath.Abs("testdata/smoke-gate-fixture-exceptions.tsv")
	if err != nil {
		t.Fatal(err)
	}
	record, exitCode := runSmokeGate(t, goBin, casesFile, false)
	if exitCode != 0 {
		t.Fatalf("smoke gate with only exceptions exit code = %d, want 0", exitCode)
	}
	if got, want := record.Counts["exception"], 2; got != want {
		t.Fatalf("smoke counts exception = %d, want %d: %+v", got, want, record.Counts)
	}
	if got := record.Counts["pass"]; got != 0 {
		t.Fatalf("smoke counts pass = %d, want 0 (exceptions are not passes)", got)
	}
	if got := record.Counts["fail"]; got != 0 {
		t.Fatalf("smoke counts fail = %d, want 0", got)
	}
}
