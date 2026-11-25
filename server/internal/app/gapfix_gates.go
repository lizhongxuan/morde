package app

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

// CheckBriefGate enforces the Planner brief rule (E01): a complex task must
// carry a complete brief before it is dispatched. The returned error names
// the missing fields so the agent can repair the plan itself; it is never
// turned into a user-visible question. Simple tasks always pass.
// CheckRootCauseGate enforces the root-cause evidence rule (E04): a fix
// task — a plan that carries a structured fix proposal — must also carry an
// evidence-backed root-cause conclusion (statement, reproduction output, and
// at least one quoted code line). The returned error names the missing
// evidence so the agent can repair the plan itself; it is never turned into
// a user-visible question. Plans without a fix proposal are not fix tasks
// and always pass.
func CheckRootCauseGate(plan task.Plan) error {
	if plan.Fix == nil {
		return nil
	}
	missing := plan.Fix.RootCause.MissingEvidence()
	if len(missing) == 0 {
		return nil
	}
	return domain.InvalidArgument("task_plan.fix.root_cause", "task plan fix root cause is missing evidence: "+strings.Join(missing, ", "))
}

func CheckBriefGate(plan task.Plan) error {
	gaps := plan.BlockingBriefGaps()
	if len(gaps) == 0 {
		return nil
	}
	return domain.InvalidArgument("task_plan.brief", "task plan brief is incomplete: missing "+strings.Join(gaps, ", "))
}

// UnrunCoveringTests returns the covering tests in the recorded impact map
// that were not run (E02 regression对照). The regression step must run every
// covering test the impact map names; a test missing from tested is reported
// by name so the gate can block with a precise reason. A task without a
// recorded impact map has nothing to check and returns nil.
func UnrunCoveringTests(impact *task.ImpactMap, tested []string) []string {
	if impact == nil {
		return nil
	}
	ran := make(map[string]struct{}, len(tested))
	for _, test := range tested {
		ran[test] = struct{}{}
	}
	var unrun []string
	for _, test := range impact.CoveringTests {
		if _, ok := ran[test]; !ok {
			unrun = append(unrun, test)
		}
	}
	return unrun
}

// CheckMinimalDiffGate enforces the minimal-diff rule (E06): when the task
// recorded an impact map, every file it writes must be inside that map
// (changed files or their callers). A file outside the map is reported by
// name so the agent can narrow the change. A task without a recorded impact
// map has nothing to bind and passes: the gate only binds work whose scope
// the agent declared. Impact paths must be recorded in the same
// workspace-relative form the write tools use.
func CheckMinimalDiffGate(plan *task.Plan, files []string) error {
	if plan == nil || plan.Impact == nil {
		return nil
	}
	if outside := task.FilesOutsideImpact(plan.Impact, files); len(outside) > 0 {
		return domain.InvalidArgument("task_plan.impact", "files outside the recorded impact map: "+strings.Join(outside, ", "))
	}
	return nil
}

// CheckFailingTestGate enforces the failing-test-first rule (E03): a declared
// bug-fix task (its plan carries a fix proposal) must have recorded the
// failing test that reproduces the bug before any code write. The returned
// error names the missing record so the agent can record the test and retry;
// it is never turned into a user-visible question.
//
// hostless is the known design gap: a Hostless task has no host runtime to
// run the reproduction against, so its writes are exempt. The exemption is
// kept deliberately (see the gapfix notes); it does not apply to host tasks.
// A task with no plan, or a plan without a fix proposal, is not a declared
// fix task and passes: the gate only binds work the agent declared as a fix.
func CheckFailingTestGate(plan *task.Plan, hostless bool) error {
	if plan == nil || plan.Fix == nil || hostless {
		return nil
	}
	if plan.FailingTest == nil {
		return domain.InvalidArgument("task_plan.failing_test", "fix task has no failing test record: record the test that reproduces the bug before writing code")
	}
	return nil
}

// agentFileEvidencePaths extracts the written file paths from file-write
// evidence, for the completion-time minimal-diff recheck (E06).
func agentFileEvidencePaths(evidence []AgentFileEvidence) []string {
	paths := make([]string, 0, len(evidence))
	for _, item := range evidence {
		if item.Path != "" {
			paths = append(paths, item.Path)
		}
	}
	return paths
}

// TestGateReport is the evidence the test gate (E07) decides on: whether the
// new test for the fix ran and passed, whether the full existing suite ran
// and passed, and which covering tests ran.
type TestGateReport struct {
	NewTest             string
	NewTestRan          bool
	NewTestPassed       bool
	ExistingSuiteRan    bool
	ExistingSuitePassed bool
	CoveringTests       []string
	CoveringTestsRan    []string
}

// TestGateDecision is the test gate verdict. Reason is empty when Allowed.
type TestGateDecision struct {
	Allowed bool
	Reason  string
}

// EvaluateTestGate decides the test gate (E07): the new test and the full
// existing suite must both have run and passed, and every covering test in
// the recorded impact map must have run. Any failure — including a test that
// never ran — blocks the task from entering regression. The decision never
// invents a pass: an unreported test counts as not run.
func EvaluateTestGate(report TestGateReport) TestGateDecision {
	if report.NewTest == "" || !report.NewTestRan {
		return TestGateDecision{Reason: "the new test did not run"}
	}
	if !report.NewTestPassed {
		return TestGateDecision{Reason: "the new test failed: " + report.NewTest}
	}
	if !report.ExistingSuiteRan {
		return TestGateDecision{Reason: "the existing test suite did not run"}
	}
	if !report.ExistingSuitePassed {
		return TestGateDecision{Reason: "the existing test suite failed"}
	}
	if unrun := UnrunCoveringTests(&task.ImpactMap{CoveringTests: report.CoveringTests}, report.CoveringTestsRan); len(unrun) > 0 {
		return TestGateDecision{Reason: "covering tests did not run: " + strings.Join(unrun, ", ")}
	}
	return TestGateDecision{Allowed: true}
}

// RunShellTestCommand runs one test command and judges it by its real shell
// exit code: exit 0 passes, any other exit (or a failure to start) fails.
// No output parsing is involved; the exit code is the verdict.
func RunShellTestCommand(ctx context.Context, name string, args ...string) error {
	if ctx == nil || name == "" {
		return domain.InvalidArgument("test_command", "test command is invalid")
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

// shellTestOutput is the JSON shape a host shell_exec call returns: the real
// exit code of the command that ran on the host.
type shellTestOutput struct {
	ExitCode int `json:"exit_code"`
}

// CheckImpactCoverageGate enforces the regression对照 (E02/P3-2): every
// changed file must have at least one covering test that ran. It runs the
// graph-level CheckImpactCoverage over the per-file coverage the plan
// recorded, so the check names the uncovered file instead of only the
// missing test. When the plan recorded no per-file coverage, there is no
// graph to check against and the gate passes: the flat covering-tests
// check in EvaluateTestGate is the only对照, and old plans without the
// coverage field keep their previous behavior. A coverage map that names
// only caller files still blocks: a changed file with no recorded
// covering test is uncovered, fail-closed.
func CheckImpactCoverageGate(plan *task.Plan, tested []string) error {
	if plan == nil || plan.Impact == nil || len(plan.Impact.Coverage) == 0 {
		return nil
	}
	impact := plan.Impact.Normalized()
	graph := task.NewDependencyGraph()
	for file, tests := range impact.Coverage {
		graph.AddCoverage(file, tests...)
	}
	if uncovered := graph.CheckImpactCoverage(*impact, tested); len(uncovered) > 0 {
		return domain.InvalidArgument("task_plan.impact.coverage", "impact coverage missing for changed files: "+strings.Join(uncovered, ", "))
	}
	return nil
}

// TestReportFromInputs extracts a TestGateReport from the completion's
// recorded tool inputs: shell_exec calls that ran tests, and their recorded
// exit codes. The exit code in the output is the real shell exit code from
// the host run; an output without a readable exit code counts as not passed
// (fail-closed: an unreadable result is not a pass).
//
// Classification: a test command names the new test when its text contains
// the test name; a suite command is a test command without a single-test
// selector (-run / -k). A passing suite run counts as running a covering
// test only for the scope it ran (P3-2): a whole-tree run covers all of
// them, while a package-scoped run covers a test only when every file that
// test covers (per the impact's per-file coverage) is inside a package the
// suite ran. Without per-file coverage a scoped run proves nothing about
// any covering test, so only whole-tree runs count. A test named in a test
// command counts as run regardless of scope.
func TestReportFromInputs(inputs []ModelInput, newTestName string, impact *task.ImpactMap) TestGateReport {
	var coveringTests []string
	if impact != nil {
		coveringTests = impact.CoveringTests
	}
	report := TestGateReport{NewTest: newTestName, CoveringTests: coveringTests}
	type testRun struct {
		command string
		passed  bool
		hasExit bool
	}
	var runs []testRun
	calls := make(map[string]ModelInput)
	for _, item := range inputs {
		if item.Type == "function_call" && item.Name == runtimeToolShellExec {
			calls[item.CallID] = item
		}
	}
	for _, item := range inputs {
		if item.Type != "function_call_output" {
			continue
		}
		call, ok := calls[item.CallID]
		if !ok {
			continue
		}
		var args struct {
			Command string `json:"command"`
		}
		// Plain unmarshal here: this is evidence extraction, not an argument
		// boundary, and shell_exec arguments carry fields beyond the command.
		if json.Unmarshal([]byte(call.Arguments), &args) != nil || !isTestCommand(args.Command) {
			continue
		}
		run := testRun{command: args.Command}
		var output shellTestOutput
		if err := json.Unmarshal([]byte(item.Output), &output); err == nil {
			run.hasExit = true
			run.passed = output.ExitCode == 0
		}
		runs = append(runs, run)
	}
	for _, run := range runs {
		if newTestName != "" && strings.Contains(run.command, newTestName) {
			report.NewTestRan = true
			// The last run of the new test is the current evidence.
			report.NewTestPassed = run.hasExit && run.passed
		}
		if isSuiteCommand(run.command) {
			report.ExistingSuiteRan = true
			report.ExistingSuitePassed = run.hasExit && run.passed
		}
	}
	ran := make(map[string]struct{})
	for _, run := range runs {
		for _, test := range coveringTests {
			if strings.Contains(run.command, test) {
				ran[test] = struct{}{}
			}
		}
	}
	if report.ExistingSuitePassed {
		for _, run := range runs {
			if !run.passed || !isSuiteCommand(run.command) {
				continue
			}
			wholeTree, packages := testSuiteScope(run.command)
			for _, test := range coveringTests {
				if wholeTree || coveringTestCoveredBySuite(impact, test, packages) {
					ran[test] = struct{}{}
				}
			}
		}
	}
	for _, test := range coveringTests {
		if _, ok := ran[test]; ok {
			report.CoveringTestsRan = append(report.CoveringTestsRan, test)
		}
	}
	return report
}

// testSuiteScope parses the package scope of a suite command (P3-2). It
// returns wholeTree when the command runs the whole project suite,
// otherwise the package arguments it ran. Non-Go suite runners (make,
// npm, pnpm, yarn, pytest, cargo) run the project suite, so they count
// as whole-tree; a scoped non-Go run (for example `pytest tests/foo.py`)
// is a known limitation and also counts as whole-tree.
func testSuiteScope(command string) (wholeTree bool, packages []string) {
	if !strings.Contains(command, "go test") {
		return true, nil
	}
	for _, token := range strings.Fields(command) {
		if token == "go" || token == "test" || strings.HasPrefix(token, "-") {
			continue
		}
		if token == "..." || strings.HasSuffix(token, "/...") {
			return true, nil
		}
		if token == "." {
			continue
		}
		packages = append(packages, strings.TrimSuffix(strings.TrimPrefix(token, "./"), "/"))
	}
	return false, packages
}

// coveringTestCoveredBySuite reports whether a package-scoped suite run
// proves a covering test ran: every file the test covers (per the
// impact's per-file coverage) must sit inside one of the ran packages.
// A test with no recorded covered file proves nothing from a scoped run.
func coveringTestCoveredBySuite(impact *task.ImpactMap, test string, packages []string) bool {
	if impact == nil || len(packages) == 0 {
		return false
	}
	covered := false
	for file, tests := range impact.Coverage {
		for _, coveredTest := range tests {
			if coveredTest != test {
				continue
			}
			covered = true
			if !testSuiteCoversFile(packages, file) {
				return false
			}
		}
	}
	return covered
}

// testSuiteCoversFile reports whether a file sits inside one of the
// packages a suite command ran. Package arguments are matched by path
// suffix because the evidence does not say whether the command ran from
// the repo root (./server/internal/app/) or the module root
// (./internal/app/): either spelling matches the workspace-relative
// file path.
func testSuiteCoversFile(packages []string, file string) bool {
	dir := file
	if i := strings.LastIndex(file, "/"); i >= 0 {
		dir = file[:i]
	} else {
		return false
	}
	for _, pkg := range packages {
		pkg = strings.TrimSuffix(strings.TrimPrefix(pkg, "./"), "/")
		if pkg == "" {
			continue
		}
		if dir == pkg || strings.HasSuffix(dir, "/"+pkg) || strings.HasSuffix(pkg, "/"+dir) {
			return true
		}
	}
	return false
}

func isTestCommand(command string) bool {
	for _, marker := range []string{"go test", "npm test", "pnpm test", "yarn test", "pytest", "cargo test", "make test"} {
		if strings.Contains(command, marker) {
			return true
		}
	}
	return false
}

func isSuiteCommand(command string) bool {
	if !isTestCommand(command) {
		return false
	}
	// A single-test selector (-run / -k) means the run covers one test, not
	// the suite.
	return !strings.Contains(command, "-run ") && !strings.Contains(command, " -k ")
}

// testGateNewTestName returns the new test name a fix task declared, or ""
// when the task recorded no failing test.
func testGateNewTestName(plan *task.Plan) string {
	if plan == nil || plan.FailingTest == nil {
		return ""
	}
	return plan.FailingTest.TestName
}

// QueryTaskImpact is the read-only impact query (E02): it returns the impact
// map recorded in the task's plan, if any. It never writes and never
// recomputes anything; the dependency-graph analysis itself lives in
// task.DependencyGraph.AnalyzeImpact.
func QueryTaskImpact(plan *task.Plan) (task.ImpactMap, bool) {
	if plan == nil || plan.Impact == nil {
		return task.ImpactMap{}, false
	}
	return *plan.Impact.Normalized(), true
}
