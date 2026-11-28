package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/domain/task"
)

// greenTestStepsForTest returns steps proving a fix task passed the E07 gate:
// the failing reproduction test (a full-suite command) ran red, then the
// same command ran green. The two runs share one request digest, which is
// what the gate matches on.
func greenTestStepsForTest(current task.Task) []task.Step {
	digest := sha256.Sum256([]byte("go test ./..."))
	return []task.Step{
		{ID: "s1", TaskID: current.ID, Kind: "tool", Status: task.StepFailed, Sequence: 1,
			ToolName: runtimeToolShellExec, FailureReason: taskToolTestFailureReason, RequestDigest: digest[:], TaskRevision: current.CurrentRevision, Summary: directTestEvidenceSummary},
		{ID: "s2", TaskID: current.ID, Kind: "tool", Status: task.StepSucceeded, Sequence: 2,
			ToolName: runtimeToolShellExec, FailureReason: taskToolTestSuitePassedReason, RequestDigest: digest[:], TaskRevision: current.CurrentRevision, Summary: directTestEvidenceSummary},
	}
}

func testGateDigest(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}

func TestFullSuiteTestCommand(t *testing.T) {
	for _, test := range []struct {
		command string
		want    bool
	}{
		{command: "go test ./...", want: true},
		{command: "go test .", want: true},
		{command: "go test -count=1 ./...", want: true},
		{command: "go test ./... -count 1", want: true},
		{command: "go test -run TestLogin ./auth", want: false},
		{command: "go test -run=TestLogin ./...", want: false},
		{command: "go test ./pkg/auth", want: false},
		{command: "go test -testify.m TestLogin ./...", want: false},
		{command: "npm test", want: true},
		{command: "npm run test:unit", want: true},
		{command: "npm test -- --coverage", want: true},
		{command: "npm test -- -t login", want: false},
		{command: "npm test -- auth", want: false},
		{command: "yarn test", want: true},
		{command: "pnpm test", want: true},
		{command: "bun test", want: true},
		{command: "deno test", want: true},
		{command: "cargo test", want: true},
		{command: "cargo test -- --nocapture", want: true},
		{command: "cargo test login", want: false},
		{command: "cargo test -- --exact login", want: false},
		{command: "pytest", want: true},
		{command: "pytest -q", want: true},
		{command: "pytest tests/", want: true},
		{command: "python -m pytest tests/", want: true},
		{command: "pytest tests/test_login.py", want: false},
		{command: "pytest tests/test_login.py::TestLogin", want: false},
		{command: "pytest -k login", want: false},
		{command: "pytest -m slow", want: false},
		{command: "python -m unittest", want: true},
		{command: "python -m unittest test_login", want: false},
		{command: "make test", want: true},
		{command: "bazel test //...", want: true},
		{command: "bazel test //pkg/auth/...", want: false},
		{command: "dotnet test", want: true},
		{command: "dotnet test --filter login", want: false},
		{command: "mvn test", want: true},
		{command: "mvn test -Dtest=TestLogin", want: false},
		{command: "jest --watchAll=false", want: true},
		{command: "jest auth.test.js", want: false},
		{command: "npx jest", want: true},
		{command: "npx jest auth", want: false},
		{command: "phpunit", want: true},
		{command: "phpunit --testsuite unit", want: false},
		{command: "bundle exec rspec", want: true},
		{command: "bundle exec rspec spec/login_spec.rb", want: false},
		{command: "rspec -e login", want: false},
		{command: "ctest", want: true},
		{command: "ctest -R login", want: false},
		{command: "go test -run TestLogin ./... && go test ./...", want: false},
		{command: "false && go test ./... || true", want: false},
		{command: "go vet ./...", want: false},
		{command: "go build ./...", want: false},
		{command: "npm run lint", want: false},
		{command: "test -f README.md", want: false},
		{command: "", want: false},
	} {
		t.Run(test.command, func(t *testing.T) {
			if got := isFullSuiteTestCommand(test.command); got != test.want {
				t.Fatalf("isFullSuiteTestCommand(%q) = %t, want %t", test.command, got, test.want)
			}
		})
	}
}

func TestPassingTestProgressStamp(t *testing.T) {
	for _, test := range []struct {
		name     string
		command  string
		exitCode int
		timedOut bool
		status   task.StepStatus
		want     string
	}{
		{name: "passing go suite", command: "go test ./...", status: task.StepSucceeded, want: taskToolTestSuitePassedReason},
		{name: "passing targeted go test", command: "go test -run TestLogin ./auth", status: task.StepSucceeded, want: taskToolTestPassedReason},
		{name: "passing npm suite", command: "npm test", status: task.StepSucceeded, want: taskToolTestSuitePassedReason},
		{name: "passing pytest file", command: "pytest tests/test_login.py", status: task.StepSucceeded, want: taskToolTestPassedReason},
		{name: "passing pytest dir", command: "pytest tests/", status: task.StepSucceeded, want: taskToolTestSuitePassedReason},
		{name: "compound command does not prove suite execution", command: "go test -run TestLogin ./... && go test ./...", status: task.StepSucceeded, want: ""},
		{name: "skipped suite is not stamped", command: "false && go test ./... || true", status: task.StepSucceeded, want: ""},
		{name: "test listing is not a test run", command: "go test -list TestLogin ./...", status: task.StepSucceeded, want: ""},
		{name: "test listing with equals is not a test run", command: "go test -list=TestLogin ./...", status: task.StepSucceeded, want: ""},
		{name: "test collection is not a test run", command: "pytest --collect-only", status: task.StepSucceeded, want: ""},
		{name: "passing non-test command is not stamped", command: "go build ./...", status: task.StepSucceeded, want: ""},
		{name: "passing go vet is not stamped", command: "go vet ./...", status: task.StepSucceeded, want: ""},
		{name: "failed test is not stamped as passed", command: "go test ./...", exitCode: 1, status: task.StepFailed, want: taskToolTestFailureReason},
	} {
		t.Run(test.name, func(t *testing.T) {
			progress, err := testFailingActivity(t, test.command, test.exitCode, test.timedOut, test.status)
			if err != nil || progress == nil {
				t.Fatalf("taskToolCallProgress() = %#v, err=%v; want a progress record", progress, err)
			}
			if progress.FailureReason != test.want {
				t.Fatalf("FailureReason = %q, want %q", progress.FailureReason, test.want)
			}
		})
	}
	t.Run("passing test with no shell evidence is not stamped", func(t *testing.T) {
		revision := int64(0)
		digest := sha256.Sum256([]byte("shell-1"))
		activity := cloudDSHToolActivity{
			call: ModelToolCall{CallID: "shell-1", Name: runtimeToolShellExec,
				Arguments: `{"workspace_id":"w1234567","command":"go test ./..."}`,
				TaskFence: &ModelTaskFence{AgentSessionID: "session-1", AgentInstanceID: "agent-1",
					AgentGeneration: 1, AuthorizationVersion: 1}},
			current: task.Task{ID: "q1234567", WorkspaceID: "w1234567", CurrentRevision: &revision},
			digest:  digest[:],
		}
		progress, err := taskToolCallProgress(activity, task.Step{ID: "s1234567", TaskID: "q1234567", Kind: "tool", Status: task.StepSucceeded})
		if err != nil || progress == nil || progress.FailureReason != "" {
			t.Fatalf("taskToolCallProgress() = %#v, err=%v; want a progress record with no stamp", progress, err)
		}
	})
}

func TestDirectTestEvidenceSurvivesTaskProgress(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
		marked  bool
	}{
		{name: "direct suite", command: "go test ./...", marked: true},
		{name: "skipped suite", command: "false && go test ./... || true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			current := configureInvalidRuntimeToolTask(&store.details.Task)
			broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			call := invalidRuntimeToolCall(current, "test-evidence", runtimeToolShellExec,
				`{"workspace_id":"w1234567","command":`+jsonQuote(test.command)+`}`)
			check := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest,
				Basis: task.PlanVerificationCheckRiskSelected, Command: test.command, CoveragePaths: []string{"server"}}
			ctx := broker.toolActivityContext(context.Background(), current, call)
			ctx = withTaskValidationPlanCheck(ctx, 1, "verify", &check)
			ctx = withTaskToolShellExecutionEvidence(ctx, call.Name, AgentToolResult{Status: AgentToolSucceeded,
				Output: `{"exit_code":0,"stdout":"","stderr":"","stdout_truncated":false,"stderr_truncated":false,"timed_out":false}`})
			step, err := newAgentStep(current.ID, 1, "tool", task.StepSucceeded, "Run test", "Tool completed.")
			if err != nil {
				t.Fatal(err)
			}
			if err := broker.commitProgress(ctx, current, step, current.Usage, task.StatusRunning, "", "task.step", nil); err != nil {
				t.Fatal(err)
			}
			if len(store.details.Steps) != 1 || (store.details.Steps[0].Summary == directTestEvidenceSummary) != test.marked {
				t.Fatalf("stored steps = %+v; direct test marker=%v", store.details.Steps, test.marked)
			}
		})
	}
}

func TestEvaluateTestGate(t *testing.T) {
	suite := testGateDigest("go test ./...")
	targeted := testGateDigest("go test -run TestLogin ./auth")
	other := testGateDigest("go test -run TestOther ./auth")
	failedSuite := func(sequence int64) task.Step {
		return task.Step{ID: "red", Kind: "tool", Status: task.StepFailed, Sequence: sequence,
			ToolName: runtimeToolShellExec, FailureReason: taskToolTestFailureReason, RequestDigest: suite}
	}
	failedTargeted := func(sequence int64) task.Step {
		return task.Step{ID: "red", Kind: "tool", Status: task.StepFailed, Sequence: sequence,
			ToolName: runtimeToolShellExec, FailureReason: taskToolTestFailureReason, RequestDigest: targeted}
	}
	passed := func(id string, sequence int64, reason string, digest []byte) task.Step {
		return task.Step{ID: id, Kind: "tool", Status: task.StepSucceeded, Sequence: sequence,
			ToolName: runtimeToolShellExec, FailureReason: reason, RequestDigest: digest}
	}
	for _, test := range []struct {
		name  string
		steps []task.Step
		want  testGateResult
	}{
		{name: "no steps"},
		{name: "only a failing test", steps: []task.Step{failedSuite(1)}},
		{name: "failing suite then the same suite passes",
			steps: []task.Step{failedSuite(1), passed("green", 2, taskToolTestSuitePassedReason, suite)},
			want:  testGateResult{newTestPassed: true, fullSuitePassed: true, newTestFailedSequence: 1, newTestPassedSequence: 2, fullSuitePassedSequence: 2}},
		{name: "failing targeted test then the same test passes",
			steps: []task.Step{failedTargeted(1), passed("green", 2, taskToolTestPassedReason, targeted)},
			want:  testGateResult{newTestPassed: true, newTestFailedSequence: 1, newTestPassedSequence: 2}},
		{name: "failing targeted test, same test passes, plus a suite pass",
			steps: []task.Step{failedTargeted(1), passed("green", 2, taskToolTestPassedReason, targeted),
				passed("suite", 3, taskToolTestSuitePassedReason, suite)},
			want: testGateResult{newTestPassed: true, fullSuitePassed: true, newTestFailedSequence: 1, newTestPassedSequence: 2, fullSuitePassedSequence: 3}},
		{name: "a different passing test cannot stand in for the failing one",
			steps: []task.Step{failedTargeted(1), passed("green", 2, taskToolTestPassedReason, other),
				passed("suite", 3, taskToolTestSuitePassedReason, suite)},
			want: testGateResult{fullSuitePassed: true, fullSuitePassedSequence: 3}},
		{name: "a pass before the failure does not count",
			steps: []task.Step{passed("green", 1, taskToolTestSuitePassedReason, suite), failedSuite(2)},
			want:  testGateResult{fullSuitePassed: true, fullSuitePassedSequence: 1}},
		{name: "suite pass with no failing record is not a new test",
			steps: []task.Step{passed("suite", 1, taskToolTestSuitePassedReason, suite)},
			want:  testGateResult{fullSuitePassed: true, fullSuitePassedSequence: 1}},
		{name: "marker on the wrong tool does not count",
			steps: []task.Step{
				{ID: "red", Kind: "tool", Status: task.StepFailed, Sequence: 1, ToolName: runtimeToolFileWrite,
					FailureReason: taskToolTestFailureReason, RequestDigest: suite},
				passed("green", 2, taskToolTestSuitePassedReason, suite)},
			want: testGateResult{fullSuitePassed: true, fullSuitePassedSequence: 2}},
		{name: "passing step with no digest does not count",
			steps: []task.Step{failedSuite(1),
				{ID: "green", Kind: "tool", Status: task.StepSucceeded, Sequence: 2,
					ToolName: runtimeToolShellExec, FailureReason: taskToolTestSuitePassedReason}},
			want: testGateResult{fullSuitePassed: true, fullSuitePassedSequence: 2}},
		{name: "failed non-test command does not start a new test",
			steps: []task.Step{
				{ID: "red", Kind: "tool", Status: task.StepFailed, Sequence: 1, ToolName: runtimeToolShellExec,
					FailureReason: taskToolExecutionFailureReason, RequestDigest: suite},
				passed("green", 2, taskToolTestSuitePassedReason, suite)},
			want: testGateResult{fullSuitePassed: true, fullSuitePassedSequence: 2}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := evaluateTestGate(test.steps); got != test.want {
				t.Fatalf("evaluateTestGate() = %+v, want %+v", got, test.want)
			}
		})
	}
}

// TestEvaluateTestGateExported covers the exported E09 re-derivation: the
// independent verifier reads the same gate, including the proving step
// sequences it sends to the review model in the replay section.
func TestEvaluateTestGateExported(t *testing.T) {
	suite := testGateDigest("go test ./...")
	revision := int64(1)
	steps := []task.Step{
		{ID: "red", Kind: "tool", Status: task.StepFailed, Sequence: 1, ToolName: runtimeToolShellExec,
			FailureReason: taskToolTestFailureReason, RequestDigest: suite, TaskRevision: &revision, Summary: directTestEvidenceSummary},
		{ID: "green", Kind: "tool", Status: task.StepSucceeded, Sequence: 2, ToolName: runtimeToolShellExec,
			FailureReason: taskToolTestSuitePassedReason, RequestDigest: suite, TaskRevision: &revision, Summary: directTestEvidenceSummary},
	}
	want := TestGateResult{NewTestPassed: true, FullSuitePassed: true,
		NewTestFailedSequence: 1, NewTestPassedSequence: 2, FullSuitePassedSequence: 2}
	if got := EvaluateTestGate(steps, &revision); got != want {
		t.Fatalf("EvaluateTestGate() = %+v, want %+v", got, want)
	}
	if got := EvaluateTestGate(nil, &revision); got != (TestGateResult{}) {
		t.Fatalf("EvaluateTestGate(nil) = %+v, want zero value", got)
	}
}

func TestEvaluateTestGateRejectsStaleEvidence(t *testing.T) {
	oldRevision, currentRevision := int64(1), int64(2)
	digest := testGateDigest("go test ./...")
	steps := []task.Step{
		{Sequence: 1, TaskRevision: &oldRevision, ToolName: runtimeToolShellExec, Status: task.StepFailed, FailureReason: taskToolTestFailureReason, RequestDigest: digest, Summary: directTestEvidenceSummary},
		{Sequence: 2, TaskRevision: &oldRevision, ToolName: runtimeToolShellExec, Status: task.StepSucceeded, FailureReason: taskToolTestSuitePassedReason, RequestDigest: digest, Summary: directTestEvidenceSummary},
		{Sequence: 3, TaskRevision: &currentRevision, ToolName: runtimeToolFileWrite, Status: task.StepSucceeded},
	}
	if got := EvaluateTestGate(steps, &currentRevision); got.NewTestPassed || got.FullSuitePassed {
		t.Fatalf("old revision proved current test gate: %+v", got)
	}
	steps = append(steps,
		task.Step{Sequence: 4, TaskRevision: &currentRevision, ToolName: runtimeToolShellExec, Status: task.StepFailed, FailureReason: taskToolTestFailureReason, RequestDigest: digest, Summary: directTestEvidenceSummary},
		task.Step{Sequence: 5, TaskRevision: &currentRevision, ToolName: runtimeToolShellExec, Status: task.StepSucceeded, FailureReason: taskToolTestSuitePassedReason, RequestDigest: digest, Summary: directTestEvidenceSummary},
		task.Step{Sequence: 6, TaskRevision: &currentRevision, ToolName: runtimeToolFileWrite, Status: task.StepSucceeded})
	if got := EvaluateTestGate(steps, &currentRevision); got.NewTestPassed || got.FullSuitePassed {
		t.Fatalf("test predating a write proved current test gate: %+v", got)
	}
	steps = append(steps, task.Step{Sequence: 7, TaskRevision: &currentRevision, ToolName: runtimeToolShellExec, Status: task.StepSucceeded, FailureReason: taskToolTestSuitePassedReason, RequestDigest: digest, Summary: directTestEvidenceSummary})
	if got := EvaluateTestGate(steps, &currentRevision); !got.NewTestPassed || !got.FullSuitePassed {
		t.Fatalf("fresh test did not prove current test gate: %+v", got)
	}
	if got := EvaluateTestGate(steps, nil); got.NewTestPassed || got.FullSuitePassed {
		t.Fatalf("missing revision proved test gate: %+v", got)
	}
	steps = append(steps, task.Step{Sequence: 8, TaskRevision: &currentRevision, ToolName: runtimeToolShellExec, Status: task.StepFailed,
		FailureReason: taskToolTestFailureReason, RequestDigest: testGateDigest("another test"), Summary: directTestEvidenceSummary})
	if got := EvaluateTestGate(steps, &currentRevision); got.FullSuitePassed {
		t.Fatalf("suite run predating a later test failure proved the gate: %+v", got)
	}
	for index := range steps {
		steps[index].Summary = "The cloud dsh tool completed and its result was saved."
	}
	if got := EvaluateTestGate(steps, &currentRevision); got.NewTestPassed || got.FullSuitePassed {
		t.Fatalf("legacy stamps without direct-command evidence proved test gate: %+v", got)
	}
}

func TestTestGateMissing(t *testing.T) {
	fix := briefPlan(1, true)
	fix.FixPlan = fixPlanForTest()
	for _, test := range []struct {
		name       string
		details    TaskDetails
		wantEmpty  bool
		wantSubstr string
	}{
		{name: "no plan is unaffected", details: TaskDetails{}, wantEmpty: true},
		{name: "plan without fix plan is unaffected", details: TaskDetails{Plan: briefPlan(1, true)}, wantEmpty: true},
		{name: "fix plan with no test records names both halves",
			details:    TaskDetails{Plan: fix},
			wantSubstr: "failing reproduction test"},
		{name: "fix plan with only a suite pass names the new test",
			details: TaskDetails{Plan: fix, Steps: []task.Step{
				{ID: "suite", Kind: "tool", Status: task.StepSucceeded, Sequence: 1, ToolName: runtimeToolShellExec,
					FailureReason: taskToolTestSuitePassedReason, RequestDigest: testGateDigest("go test ./...")},
			}},
			wantSubstr: "failing reproduction test"},
		{name: "fix plan with only a new test pass names the suite",
			details: TaskDetails{Plan: fix, Steps: []task.Step{
				{ID: "red", Kind: "tool", Status: task.StepFailed, Sequence: 1, ToolName: runtimeToolShellExec,
					FailureReason: taskToolTestFailureReason, RequestDigest: testGateDigest("go test -run TestLogin ./auth")},
				{ID: "green", Kind: "tool", Status: task.StepSucceeded, Sequence: 2, ToolName: runtimeToolShellExec,
					FailureReason: taskToolTestPassedReason, RequestDigest: testGateDigest("go test -run TestLogin ./auth")},
			}},
			wantSubstr: "full test suite"},
		{name: "fix plan with both halves passes",
			details: TaskDetails{Plan: fix, Steps: []task.Step{
				{ID: "red", Kind: "tool", Status: task.StepFailed, Sequence: 1, ToolName: runtimeToolShellExec,
					FailureReason: taskToolTestFailureReason, RequestDigest: testGateDigest("go test ./...")},
				{ID: "green", Kind: "tool", Status: task.StepSucceeded, Sequence: 2, ToolName: runtimeToolShellExec,
					FailureReason: taskToolTestSuitePassedReason, RequestDigest: testGateDigest("go test ./...")},
			}},
			wantEmpty: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			revision := int64(1)
			test.details.Task.CurrentRevision = &revision
			for index := range test.details.Steps {
				test.details.Steps[index].TaskRevision = &revision
				test.details.Steps[index].Summary = directTestEvidenceSummary
			}
			got := testGateMissing(test.details)
			if test.wantEmpty && got != "" {
				t.Fatalf("testGateMissing() = %q, want empty", got)
			}
			if test.wantSubstr != "" && !strings.Contains(got, test.wantSubstr) {
				t.Fatalf("testGateMissing() = %q, want it to contain %q", got, test.wantSubstr)
			}
			if test.wantSubstr == "" && !test.wantEmpty && got == "" {
				t.Fatalf("testGateMissing() is empty, want a missing-half description")
			}
		})
	}
	t.Run("fix plan with no test records names the suite too", func(t *testing.T) {
		if got := testGateMissing(TaskDetails{Plan: fix}); !strings.Contains(got, "full test suite") {
			t.Fatalf("testGateMissing() = %q, want it to name the full test suite", got)
		}
	})
}

func TestFixCompletionDoesNotUseE07AsBlanketGate(t *testing.T) {
	fix := briefPlan(1, true)
	fix.FixPlan = fixPlanForTest()
	suite := testGateDigest("go test ./...")
	targeted := testGateDigest("go test -run TestLogin ./auth")
	for _, test := range []struct {
		name   string
		plan   *task.Plan
		steps  []task.Step
		events []task.Event
	}{
		{name: "no fix plan is unaffected", plan: briefPlan(1, true)},
		{name: "fix plan with no test records proceeds to independent review", plan: fix},
		{name: "fix plan with only a failing test proceeds to independent review",
			plan: fix,
			steps: []task.Step{{ID: "s1", Kind: "tool", Status: task.StepFailed, Sequence: 1,
				ToolName: runtimeToolShellExec, FailureReason: taskToolTestFailureReason, RequestDigest: suite}},
		},
		{name: "fix plan with a focused pass proceeds to independent review",
			plan: fix,
			steps: []task.Step{
				{ID: "s1", Kind: "tool", Status: task.StepFailed, Sequence: 1,
					ToolName: runtimeToolShellExec, FailureReason: taskToolTestFailureReason, RequestDigest: targeted},
				{ID: "s2", Kind: "tool", Status: task.StepSucceeded, Sequence: 2,
					ToolName: runtimeToolShellExec, FailureReason: taskToolTestPassedReason, RequestDigest: targeted},
			},
		},
		{name: "a different passing test proceeds to independent review",
			plan: fix,
			steps: []task.Step{
				{ID: "s1", Kind: "tool", Status: task.StepFailed, Sequence: 1,
					ToolName: runtimeToolShellExec, FailureReason: taskToolTestFailureReason, RequestDigest: targeted},
				{ID: "s2", Kind: "tool", Status: task.StepSucceeded, Sequence: 2,
					ToolName: runtimeToolShellExec, FailureReason: taskToolTestPassedReason, RequestDigest: suite},
				{ID: "s3", Kind: "tool", Status: task.StepSucceeded, Sequence: 3,
					ToolName: runtimeToolShellExec, FailureReason: taskToolTestSuitePassedReason, RequestDigest: suite},
			},
		},
		{name: "legacy passing stamps do not change review handoff",
			plan: fix,
			steps: []task.Step{
				{ID: "s1", Kind: "tool", Status: task.StepFailed, Sequence: 1,
					ToolName: runtimeToolShellExec, FailureReason: taskToolTestFailureReason, RequestDigest: targeted},
				{ID: "s2", Kind: "tool", Status: task.StepSucceeded, Sequence: 2,
					ToolName: runtimeToolShellExec, FailureReason: taskToolTestPassedReason, RequestDigest: targeted},
				{ID: "s3", Kind: "tool", Status: task.StepSucceeded, Sequence: 3,
					ToolName: runtimeToolShellExec, FailureReason: taskToolTestSuitePassedReason, RequestDigest: suite},
			},
			// A legacy map event is not evidence of coverage and does not block H07 review.
			events: []task.Event{impactMapEventForTest(t, cloudDSHImpactMapToolName, "succeeded",
				ImpactMap{Component: "auth/session.go", CoveringTests: []string{"auth/session_test.go"}})}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			store.details.Task.Objective = "Answer the question in chat."
			setCloudDSHCompletionFence(&store.details.Task)
			store.details.Plan = test.plan
			if test.steps != nil {
				for index := range test.steps {
					test.steps[index].TaskID = store.details.Task.ID
					test.steps[index].TaskRevision = store.details.Task.CurrentRevision
					test.steps[index].Summary = directTestEvidenceSummary
				}
				store.details.Steps = test.steps
			}
			if test.events != nil {
				store.events = append(store.events, test.events...)
			}
			broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			claimed := store.details.Task
			if err := broker.Complete(context.Background(), claimed,
				cloudDSHCompletionFor(claimed, "Fixed the login crash.", minimalDiffFileWriteInputs("file-write-test-gate-1", "auth/session.go"))); err != nil {
				t.Fatalf("Complete() error = %v", err)
			}
			if store.details.Task.WaitingReason == "test_gate_failed" {
				t.Fatalf("H07 handoff was blocked by the legacy E07 gate: task=%#v", store.details.Task)
			}
		})
	}
}

func TestHostFixWithoutRecordedWritesCanReachIndependentReview(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	store.details.Task.Objective = "Fix the login bug."
	store.details.Task.FileDeliveryIntent = task.FileDeliveryNone
	setCloudDSHCompletionFence(&store.details.Task)
	plan := briefPlan(1, true)
	plan.FixPlan = fixPlanForTest()
	store.details.Plan = plan
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	current := store.details.Task
	if err := broker.Complete(context.Background(), current, cloudDSHCompletionFor(current, "Fixed the login crash.", nil)); err != nil {
		t.Fatal(err)
	}
	if store.details.Task.Status != task.StatusCompleted || store.details.Task.Result != "Fixed the login crash." ||
		store.details.Task.WaitingReason != "" {
		t.Fatalf("fix without recorded writes did not reach independent review: %+v", store.details.Task)
	}
}

func TestCheckpointMismatchNeverPublishesStaleCompletion(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	store.details.Task.Objective = "Answer the question in chat."
	setCloudDSHCompletionFence(&store.details.Task)
	plan := briefPlan(1, true)
	plan.FixPlan = fixPlanForTest()
	store.details.Plan = plan
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	completion := cloudDSHCompletionFor(claimed, "Fixed the login crash.", minimalDiffFileWriteInputs("file-write-test-gate-1", "auth/session.go"))
	// The checkpoint changed, so this completion belongs to an older DSH state.
	staleDigest := sha256.Sum256([]byte("stale checkpoint"))
	store.details.Task.CheckpointDigest = staleDigest[:]
	if err := broker.Complete(context.Background(), claimed, completion); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "output_verification_pending" ||
		store.details.Task.Result != "" {
		t.Fatalf("stale completion was published or not paused: status=%q waiting=%q result=%q",
			store.details.Task.Status, store.details.Task.WaitingReason, store.details.Task.Result)
	}
}

func TestTestGateHostlessCompleteExempt(t *testing.T) {
	fixture, store, broker, workspaceStore, current := newCloudDSHT08WorkspaceBroker(t)
	current.Objective = "Fix the login crash in the auth package."
	setCloudDSHCompletionFence(&current)
	store.details.Task = current
	plan := briefPlan(1, true)
	plan.FixPlan = fixPlanForTest()
	store.details.Plan = plan
	// No test records at all: a hostless task has no shell tool, so no test
	// run could ever be recorded and the gate must not block it.
	inputs := minimalDiffHostlessWriteInputs(t, workspaceStore, fixture, current, "auth/session.go")
	if err := broker.Complete(context.Background(), current,
		cloudDSHCompletionFor(current, "Fixed the login crash.", inputs)); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if store.details.Task.Status != task.StatusCompleted || store.details.Task.Result != "Fixed the login crash." {
		t.Fatalf("hostless fix task was blocked by the test gate: status=%q waiting=%q result=%q",
			store.details.Task.Status, store.details.Task.WaitingReason, store.details.Task.Result)
	}
}

func impactMapEventForTest(t *testing.T, tool, status string, impact any) task.Event {
	t.Helper()
	detail := map[string]any{"tool": tool, "operation_status": status}
	if impact != nil {
		detail["impact_map"] = impact
	}
	payload, err := json.Marshal(map[string]any{"detail": detail})
	if err != nil {
		t.Fatal(err)
	}
	return task.Event{Type: "task.step", Sequence: 1, Payload: payload}
}

func TestLegacyTestGateHookDoesNotBlockH07Review(t *testing.T) {
	for _, test := range []struct {
		name      string
		events    []task.Event
		withTests bool
	}{
		{name: "no map and actual tests", withTests: true},
		{name: "legacy map with unchanged caller", withTests: true, events: []task.Event{impactMapEventForTest(t, cloudDSHImpactMapToolName, "succeeded", ImpactMap{Component: "auth/session.go", Callers: []string{"auth/login.go"}, Dependencies: []string{"auth/token.go"}})}},
		{name: "unrelated map does not affect review", events: []task.Event{impactMapEventForTest(t, cloudDSHImpactMapToolName, "succeeded", ImpactMap{Component: "other.go"})}},
		{name: "old map does not affect review", events: []task.Event{impactMapEventForTest(t, cloudDSHImpactMapToolName, "succeeded", ImpactMap{Component: "auth/session.go"})}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			current := store.details.Task
			store.details.Plan = &task.Plan{FixPlan: &task.FixPlan{ChangedFiles: []string{"auth/session.go"}}}
			if test.withTests {
				store.details.Steps = greenTestStepsForTest(current)
			}
			store.events = append(store.events, test.events...)
			step, err := newAgentStep(current.ID, maxStepSequence(store.details.Steps)+1, "result", task.StepSucceeded, "Complete cloud task", "The session completed.")
			if err != nil {
				t.Fatal(err)
			}
			paused, err := broker.pauseForTestGate(context.Background(), current, step, store.details)
			if err != nil || paused {
				t.Fatalf("legacy E07 hook paused the H07 handoff: paused=%t err=%v", paused, err)
			}
		})
	}
}
