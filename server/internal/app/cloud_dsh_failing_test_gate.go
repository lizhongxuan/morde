package app

import (
	"encoding/json"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

// hasFailingTestRecord reports whether the task steps contain a failed test
// run. A step counts only when the broker recorded it as a failed test run:
// the shell command matched the test-command heuristic (see isTestCommand)
// and the run failed with a non-zero exit or a timeout. That record is
// stamped on the step when the shell result is committed, because a stored
// step carries no command text or exit code of its own: the command and exit
// code live only in the sealed step detail. A passing test does not count,
// and neither does a failed non-test command.
func hasFailingTestRecord(steps []task.Step) bool {
	for _, step := range steps {
		if step.ToolName == runtimeToolShellExec && step.Status == task.StepFailed &&
			step.FailureReason == taskToolTestFailureReason {
			return true
		}
	}
	return false
}

// requireFailingTestRecord is the write gate (E03): a task whose plan carries
// a fix plan may only mutate the workspace after a failing reproduction test
// has run in this task. It applies only on paths where the task can run a
// shell test: the host file_write path. Hostless workspace writes are exempt
// because that path has no shell tool, so a failing test could never be
// recorded and the gate would deadlock every fix task. Tasks without a fix
// plan are unaffected, so ordinary writes and feature work are never blocked
// by this gate. The error tells the agent what to do next; it must not
// surface as a user question.
func requireFailingTestRecord(details TaskDetails) error {
	if details.Plan == nil || details.Plan.FixPlan == nil {
		return nil
	}
	if !taskPlanHasTypedTestCheck(details.Plan) {
		return domain.InvalidArgument("task_plan.verification_check", "a fix plan requires an explicit typed test check")
	}
	if hasFailingTestRecord(currentRevisionTestSteps(details.Steps, details.Task.CurrentRevision)) {
		return nil
	}
	return domain.InvalidArgument("task_plan.failing_test", "run a failing reproduction test before changing code")
}

func taskPlanHasTypedTestCheck(plan *task.Plan) bool {
	if plan == nil {
		return false
	}
	for _, step := range plan.Steps {
		if step.VerificationCheck != nil && step.VerificationCheck.Kind == task.PlanVerificationCheckTest &&
			validTaskPlanVerificationCommand(*step.VerificationCheck) {
			return true
		}
	}
	return false
}

// failingTestFailureReason returns taskToolTestFailureReason when the tool
// activity is a shell run of a test command that failed. It returns "" for
// every other call, including a failed non-test command and a test command
// whose output could not be parsed into shell evidence, so the gate fails
// closed: an unproven test run never unlocks a fix.
func failingTestFailureReason(activity cloudDSHToolActivity) string {
	if activity.call.Name != runtimeToolShellExec || activity.shellExecution == nil ||
		(!activity.shellExecution.TimedOut && activity.shellExecution.ExitCode == 0) {
		return ""
	}
	if activity.verificationCheck == nil || activity.verificationCheck.Kind != task.PlanVerificationCheckTest ||
		activity.verificationStepID == "" || activity.verificationPlanRevision < 1 {
		return ""
	}
	var arguments struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(activity.call.Arguments), &arguments) != nil || !isDirectTestCommand(arguments.Command) {
		return ""
	}
	if arguments.Command != activity.verificationCheck.Command {
		return ""
	}
	return taskToolTestFailureReason
}

// isDirectTestCommand accepts only a single, unwrapped command for test
// evidence. An exit code for a shell expression cannot prove which of its
// branches actually ran, and an environment override can change test
// selection. More complicated invocations remain ordinary tool activity.
func isDirectTestCommand(command string) bool {
	if strings.ContainsAny(command, "&|;\n\r<>$`\\\"'()#!") {
		return false
	}
	tokens := strings.Fields(command)
	if len(tokens) == 0 || tokens[0] == "env" || tokens[0] == "sudo" ||
		strings.Contains(tokens[0], "=") {
		return false
	}
	for _, token := range tokens[1:] {
		token = strings.ToLower(token)
		if strings.HasPrefix(token, "-list=") || strings.HasPrefix(token, "--list=") ||
			strings.HasPrefix(token, "--collect-only=") || strings.HasPrefix(token, "--dry-run=") {
			return false
		}
		switch token {
		case "-h", "--help", "--version", "--collect-only", "--collectonly", "--dry-run", "-list", "--list", "--listtests":
			return false
		}
	}
	return testCommandSegment(tokens)
}

// isTestCommand reports whether a shell command runs a test suite. It is a
// heuristic over the command text, not a shell parser: the command is split
// on shell separators, leading env assignments, sudo and `sh -c` wrappers are
// skipped, and the remaining head must be a known test binary (pytest, jest,
// vitest, phpunit, rspec, ctest) or a known runner invoked with `test`
// (go test, cargo test, npm/yarn/pnpm/bun/deno test, `npm run test*`,
// python -m pytest/unittest, make test, bazel test, dotnet/swift/mvn/gradle
// test). A bare `test` builtin does not count, and neither do lookalikes such
// as `go vet` or `npm run lint`. Runners outside this list do not satisfy the
// gate; extend the list when a new runner is adopted.
func isTestCommand(command string) bool {
	for _, segment := range strings.FieldsFunc(command, func(r rune) bool {
		return r == '&' || r == '|' || r == ';' || r == '\n'
	}) {
		if testCommandSegment(strings.Fields(segment)) {
			return true
		}
	}
	return false
}

func testCommandSegment(tokens []string) bool {
	_, _, ok := testCommandRunner(tokens)
	return ok
}

// cleanTestCommandTokens lowercases and unquotes the tokens, then skips
// leading env assignments, sudo and `cd <dir>` / `sh -c` wrappers, so the
// remaining head is the test runner invocation. Cleaning twice is a no-op:
// after the first pass no leading wrapper is left.
func cleanTestCommandTokens(tokens []string) []string {
	cleaned := make([]string, 0, len(tokens))
	for _, token := range tokens {
		token = strings.Trim(strings.ToLower(token), `"'`)
		if token != "" {
			cleaned = append(cleaned, token)
		}
	}
	tokens = cleaned
	for len(tokens) > 0 {
		head := tokens[0]
		if head == "sudo" || head == "env" {
			tokens = tokens[1:]
			continue
		}
		if strings.Contains(head, "=") && !strings.HasPrefix(head, "-") {
			tokens = tokens[1:]
			continue
		}
		break
	}
	if len(tokens) >= 2 && tokens[0] == "cd" {
		tokens = tokens[2:]
	}
	if len(tokens) >= 2 && (tokens[0] == "sh" || tokens[0] == "bash") && tokens[1] == "-c" {
		tokens = tokens[2:]
	}
	return tokens
}

// testCommandRunnerHead returns the basename of the runner token, so an
// absolute or relative runner path still matches its binary name.
func testCommandRunnerHead(tokens []string) string {
	head := tokens[0]
	if index := strings.LastIndex(head, "/"); index >= 0 {
		head = head[index+1:]
	}
	return head
}

// testCommandRunner reports whether a cleaned segment runs a test suite and,
// when it does, the runner name and the tokens after the runner invocation.
// Wrapper runners report the test binary they invoke (npx/node report
// jest/vitest, python reports pytest/unittest, bundle reports rspec), so the
// caller can apply one selection rule per runner. The rest excludes the
// invocation itself: `go test`, `npm run test:unit`, `python -m pytest` and
// their kin are consumed, leaving only flags and selectors.
func testCommandRunner(tokens []string) (runner string, rest []string, ok bool) {
	tokens = cleanTestCommandTokens(tokens)
	if len(tokens) == 0 {
		return "", nil, false
	}
	head := testCommandRunnerHead(tokens)
	switch head {
	case "pytest", "jest", "vitest", "phpunit", "rspec", "ctest":
		return head, tokens[1:], true
	}
	if len(tokens) < 2 {
		return "", nil, false
	}
	switch head {
	case "go", "cargo", "dotnet", "swift", "mvn", "gradle", "gradlew", "bazel", "make":
		if tokens[1] != "test" {
			return "", nil, false
		}
		return head, tokens[2:], true
	case "npm", "yarn", "pnpm", "bun", "deno":
		if tokens[1] == "test" {
			return head, tokens[2:], true
		}
		if tokens[1] == "run" && len(tokens) >= 3 && strings.HasPrefix(tokens[2], "test") {
			return head, tokens[3:], true
		}
		return "", nil, false
	case "npx", "node":
		if tokens[1] == "jest" || tokens[1] == "vitest" {
			return tokens[1], tokens[2:], true
		}
		return "", nil, false
	case "python", "python3":
		if tokens[1] == "-m" && len(tokens) >= 3 && (tokens[2] == "pytest" || tokens[2] == "unittest") {
			return tokens[2], tokens[3:], true
		}
		return "", nil, false
	case "bundle":
		if tokens[1] == "exec" && len(tokens) >= 3 && tokens[2] == "rspec" {
			return "rspec", tokens[3:], true
		}
		return "", nil, false
	default:
		return "", nil, false
	}
}
