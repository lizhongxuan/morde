package app

import (
	"encoding/json"
	"strings"

	"morde/server/internal/domain/task"
)

// The E07 test gate runs inside Complete(), after the minimal-diff recheck
// and before regression verification. A fix task may only complete when both
// halves hold, proven by real shell runs recorded on the task steps:
//
//   - the new test passed: the same test command the E03 gate recorded as
//     failing ran again after the fix and passed (same tool call arguments,
//     so the same request digest, at a later step sequence);
//   - the full suite passed: a run containing a full-suite test command
//     exited 0.
//
// Neither half trusts the model's TaskResultClaim: the claim is self-reported,
// while these records come from the shell exit code captured when the tool
// result was committed (see testPassReason). An unparsable run stamps
// nothing, so the gate fails closed. Tasks without a fix plan are unaffected,
// and hostless tasks are exempt: they have no shell tool, so no test run can
// ever be recorded and the gate would deadlock them (see E03). A hostless
// fix task therefore completes with no test gate; that is a known limitation,
// and giving hostless tasks a test runner is follow-up work.

// testPassReason returns the passing-test record for a shell run, or "" when
// the activity is not a passing shell test run. A run that included a
// full-suite test command is stamped test_suite_passed; any other passing
// test run is stamped test_passed. A timed-out or non-zero run stamps
// nothing here: it is the failing side, handled by failingTestFailureReason.
func testPassReason(activity cloudDSHToolActivity) string {
	if activity.call.Name != runtimeToolShellExec || activity.shellExecution == nil ||
		activity.shellExecution.TimedOut || activity.shellExecution.ExitCode != 0 {
		return ""
	}
	var arguments struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(activity.call.Arguments), &arguments) != nil {
		return ""
	}
	if isFullSuiteTestCommand(arguments.Command) {
		return taskToolTestSuitePassedReason
	}
	if isTestCommand(arguments.Command) {
		return taskToolTestPassedReason
	}
	return ""
}

// isFullSuiteTestCommand reports whether a shell command runs a full test
// suite: at least one `&&`/`||`/`;`-separated segment is a test command with
// no test selection. A command that only runs a selected test (a -run
// filter, a :: selector, a single test file) is not a full suite, even when
// it passes.
func isFullSuiteTestCommand(command string) bool {
	for _, segment := range strings.FieldsFunc(command, func(r rune) bool {
		return r == '&' || r == '|' || r == ';' || r == '\n'
	}) {
		if fullSuiteTestCommandSegment(strings.Fields(segment)) {
			return true
		}
	}
	return false
}

func fullSuiteTestCommandSegment(tokens []string) bool {
	runner, rest, ok := testCommandRunner(tokens)
	return ok && !testCommandHasSelection(runner, rest)
}

// testSelectionFlags are flags that select a subset of tests. Exact matches
// and `flag=value` spellings both count; tokens are already lowercased by
// cleanTestCommandTokens.
var testSelectionFlags = []string{
	"-k",       // pytest / jest keyword filter
	"--filter", // dotnet / cargo filter
	"-t",       // jest / vitest test-name pattern
	"--testnamepattern",
	"--testpathpattern",
	"--tests",     // maven / gradle test filter
	"-dtest",      // maven -Dtest
	"-run",        // go test filter
	"-testify.m",  // go testify filter
	"--deselect",  // pytest
	"-e",          // rspec example filter
	"--testsuite", // phpunit
	"-r",          // ctest regex filter
	"-m",          // pytest marker selection
}

func testSelectionFlag(token string) bool {
	for _, flag := range testSelectionFlags {
		if token == flag || strings.HasPrefix(token, flag+"=") {
			return true
		}
	}
	return false
}

// testCommandHasSelection reports whether the tokens after a test runner
// invocation select a subset of the suite instead of running it all. A `::`
// selector anywhere, a selection flag, or a positional argument that names a
// subset all count. Positional rules differ per runner: most runners treat
// any positional as a filter, while pytest/phpunit/rspec/ctest only treat a
// positional naming a test file as one (a directory is the suite), go only
// treats a package path outside `.` / `./...` as one, and bazel only treats
// a target other than `//...` as one. Tokens that are plain values of a
// preceding flag (for example `-count 1`) carry no slash or leading dot, so
// the go rule ignores them; `make test` has no selection at all.
func testCommandHasSelection(runner string, rest []string) bool {
	for _, token := range rest {
		if strings.Contains(token, "::") || testSelectionFlag(token) {
			return true
		}
		if strings.HasPrefix(token, "-") || token == "--" || strings.Contains(token, "=") && !strings.HasPrefix(token, "-") {
			continue
		}
		switch runner {
		case "make":
			continue
		case "go":
			if (strings.HasPrefix(token, ".") || strings.Contains(token, "/")) && token != "." && token != "./..." {
				return true
			}
		case "bazel":
			if token != "//..." {
				return true
			}
		case "pytest", "phpunit", "rspec", "ctest":
			if testFilePositional(token) {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// testFilePositional reports whether a positional argument names a single
// test file rather than a directory or a package.
func testFilePositional(token string) bool {
	for _, suffix := range []string{".py", ".php", ".rb", ".js", ".jsx", ".ts", ".tsx", ".go"} {
		if strings.HasSuffix(token, suffix) {
			return true
		}
	}
	return false
}

// testGateResult is the outcome of the E07 gate over a task's steps. The
// sequence fields name the steps that prove each half, so a reviewer can
// pair the failing run with the passing run of the same command; 0 means
// that half has no proving step.
type testGateResult struct {
	newTestPassed           bool
	fullSuitePassed         bool
	newTestFailedSequence   int64
	newTestPassedSequence   int64
	fullSuitePassedSequence int64
}

// TestGateResult is the exported outcome of EvaluateTestGate: the E07 test
// gate re-derived from the durable step ledger by a caller other than the
// execution broker (for example the independent verification path, E09).
type TestGateResult struct {
	NewTestPassed           bool
	FullSuitePassed         bool
	NewTestFailedSequence   int64
	NewTestPassedSequence   int64
	FullSuitePassedSequence int64
}

// EvaluateTestGate re-derives the E07 test gate from the recorded steps.
// It is the same derivation the execution broker runs at completion time,
// exposed so the independent verifier can re-run it from the ledger instead
// of trusting the broker's gate or the model's claim.
func EvaluateTestGate(steps []task.Step) TestGateResult {
	result := evaluateTestGate(steps)
	return TestGateResult{
		NewTestPassed:           result.newTestPassed,
		FullSuitePassed:         result.fullSuitePassed,
		NewTestFailedSequence:   result.newTestFailedSequence,
		NewTestPassedSequence:   result.newTestPassedSequence,
		FullSuitePassedSequence: result.fullSuitePassedSequence,
	}
}

// evaluateTestGate reads the durable test-run records off the task steps. A
// new test counts as passed only when a step stamped test_failed (the E03
// record) and a later step stamped test_passed or test_suite_passed share
// the same request digest, which is the hash of the tool name and arguments:
// the passing run must repeat the exact failing test command, not a
// different test. The full suite counts as passed when any step is stamped
// test_suite_passed.
func evaluateTestGate(steps []task.Step) testGateResult {
	failed := make(map[string]int64)
	for _, step := range steps {
		if step.ToolName != runtimeToolShellExec || step.Status != task.StepFailed ||
			step.FailureReason != taskToolTestFailureReason || len(step.RequestDigest) == 0 {
			continue
		}
		digest := string(step.RequestDigest)
		if sequence, ok := failed[digest]; !ok || step.Sequence < sequence {
			failed[digest] = step.Sequence
		}
	}
	var result testGateResult
	for _, step := range steps {
		if step.ToolName != runtimeToolShellExec || step.Status != task.StepSucceeded {
			continue
		}
		// The suite half needs no digest: any stamped full-suite pass proves
		// a suite passed. Only the new-test match needs the digest, to pair
		// the passing run with the failing run of the same command.
		if step.FailureReason == taskToolTestSuitePassedReason {
			result.fullSuitePassed = true
			if result.fullSuitePassedSequence == 0 || step.Sequence < result.fullSuitePassedSequence {
				result.fullSuitePassedSequence = step.Sequence
			}
		}
		if len(step.RequestDigest) == 0 {
			continue
		}
		switch step.FailureReason {
		case taskToolTestPassedReason, taskToolTestSuitePassedReason:
			if failedSequence, ok := failed[string(step.RequestDigest)]; ok && step.Sequence > failedSequence {
				result.newTestPassed = true
				result.newTestFailedSequence = failedSequence
				if result.newTestPassedSequence == 0 || step.Sequence < result.newTestPassedSequence {
					result.newTestPassedSequence = step.Sequence
				}
			}
		}
	}
	return result
}

// testGateMissing describes what the E07 gate still lacks for a fix task, or
// "" when the gate passes. Tasks without a fix plan are unaffected. The
// message names the missing half so the agent knows which run to do next;
// it must not surface as a user question.
func testGateMissing(details TaskDetails) string {
	if details.Plan == nil || details.Plan.FixPlan == nil {
		return ""
	}
	result := evaluateTestGate(details.Steps)
	var missing []string
	if !result.newTestPassed {
		missing = append(missing, "the failing reproduction test has not passed yet; re-run the same test command that failed, after the fix, and confirm it passes")
	}
	if !result.fullSuitePassed {
		missing = append(missing, "the full test suite has not passed yet; run the full test suite and confirm it passes")
	}
	if len(missing) == 0 {
		return ""
	}
	return strings.Join(missing, "; ")
}
