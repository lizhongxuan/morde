package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func failingTestStepForTest(current task.Task, id string) task.Step {
	return task.Step{ID: id, TaskID: current.ID, Kind: "tool", Status: task.StepFailed,
		ToolName: runtimeToolShellExec, FailureReason: taskToolTestFailureReason, TaskRevision: current.CurrentRevision, Summary: directTestEvidenceSummary}
}

func TestFailingTestCommand(t *testing.T) {
	for _, test := range []struct {
		command string
		want    bool
	}{
		{command: "go test ./...", want: true},
		{command: "go test -run TestLogin ./auth", want: true},
		{command: "cd server && go test ./internal/app", want: true},
		{command: "npm test", want: true},
		{command: "npm run test:unit", want: true},
		{command: "yarn test", want: true},
		{command: "pnpm test", want: true},
		{command: "bun test", want: true},
		{command: "deno test", want: true},
		{command: "cargo test", want: true},
		{command: "pytest -q", want: true},
		{command: "python -m pytest tests/", want: true},
		{command: "python3 -m unittest", want: true},
		{command: "make test", want: true},
		{command: "bazel test //...", want: true},
		{command: "dotnet test", want: true},
		{command: "swift test", want: true},
		{command: "jest --watchAll=false", want: true},
		{command: "npx jest auth.test.js", want: true},
		{command: "phpunit --testsuite unit", want: true},
		{command: "bundle exec rspec", want: true},
		{command: "sh -c \"go test ./...\"", want: true},
		{command: "sudo go test ./...", want: true},
		{command: "FOO=bar go test ./...", want: true},
		{command: "go vet ./...", want: false},
		{command: "go build ./...", want: false},
		{command: "npm run lint", want: false},
		{command: "npm run build", want: false},
		{command: "test -f README.md", want: false},
		{command: "rm -rf /tmp/build", want: false},
		{command: "echo go test", want: false},
		{command: "git status", want: false},
		{command: "", want: false},
	} {
		t.Run(test.command, func(t *testing.T) {
			if got := isTestCommand(test.command); got != test.want {
				t.Fatalf("isTestCommand(%q) = %t, want %t", test.command, got, test.want)
			}
		})
	}
}

func testFailingActivity(t *testing.T, command string, exitCode int, timedOut bool, status task.StepStatus) (*TaskToolCallProgress, error) {
	t.Helper()
	revision := int64(0)
	digest := sha256.Sum256([]byte("shell-1"))
	activity := cloudDSHToolActivity{
		call: ModelToolCall{CallID: "shell-1", Name: runtimeToolShellExec,
			Arguments: `{"workspace_id":"w1234567","command":` + jsonQuote(command) + `}`,
			TaskFence: &ModelTaskFence{AgentSessionID: "session-1", AgentInstanceID: "agent-1",
				AgentGeneration: 1, AuthorizationVersion: 1}},
		current:        task.Task{ID: "q1234567", WorkspaceID: "w1234567", CurrentRevision: &revision},
		digest:         digest[:],
		shellExecution: &TaskStepShellExecutionEvidence{ExitCode: exitCode, TimedOut: timedOut},
		verificationCheck: &task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRiskSelected,
			Command: command, CoveragePaths: []string{"auth"}},
		verificationPlanRevision: 1,
		verificationStepID:       "repro",
		phaseOverride:            "execute",
		failureReason:            taskToolExecutionFailureReason,
	}
	step := task.Step{ID: "s1234567", TaskID: "q1234567", Kind: "tool", Status: status}
	return taskToolCallProgress(activity, step)
}

func jsonQuote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

func TestFailingTestProgressStamp(t *testing.T) {
	for _, test := range []struct {
		name     string
		command  string
		exitCode int
		timedOut bool
		status   task.StepStatus
		want     string
	}{
		{name: "failed go test", command: "go test ./...", exitCode: 1, status: task.StepFailed, want: taskToolTestFailureReason},
		{name: "failed npm test", command: "npm test", exitCode: 2, status: task.StepFailed, want: taskToolTestFailureReason},
		{name: "timed out test", command: "go test ./...", timedOut: true, status: task.StepFailed, want: taskToolTestFailureReason},
		{name: "passing suite is stamped as a suite pass (E07)", command: "go test ./...", status: task.StepSucceeded, want: taskToolTestSuitePassedReason},
		{name: "failed non-test command is not stamped", command: "rm -rf /tmp/build", exitCode: 1, status: task.StepFailed, want: taskToolExecutionFailureReason},
		{name: "failed go vet is not stamped", command: "go vet ./...", exitCode: 1, status: task.StepFailed, want: taskToolExecutionFailureReason},
		{name: "failed test with exit zero is not stamped", command: "go test ./...", status: task.StepFailed, want: taskToolExecutionFailureReason},
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
}

func fixPlanForTest() *task.FixPlan {
	return &task.FixPlan{ChangedFiles: []string{"auth/session.go"}, Risks: []string{"session expiry shifts by a day"},
		Alternatives: []string{"restart the service daily"}}
}

func TestFixPlanWorkspaceWriteDoesNotRequireRedTest(t *testing.T) {
	// Verification is independent from writes. Both hostless and Host-backed
	// writes remain subject to plan, authorization, approval, and scope gates.
	complete := briefPlan(1, true)
	complete.FixPlan = fixPlanForTest()
	incomplete := briefPlan(1, false)
	incomplete.FixPlan = fixPlanForTest()
	plainComplete := briefPlan(1, true)
	for _, test := range []struct {
		name       string
		plan       *task.Plan
		steps      func(task.Task) []task.Step
		wantSubstr string
		wantAllow  bool
	}{
		{name: "no plan is unaffected", plan: nil, wantAllow: true},
		{name: "complete brief without fix plan is unaffected", plan: plainComplete, wantAllow: true},
		{name: "fix plan without any test is allowed: hostless has no shell", plan: complete, wantAllow: true},
		{name: "fix plan with only a passing test is allowed", plan: complete,
			steps: func(current task.Task) []task.Step {
				return []task.Step{{ID: "s1", TaskID: current.ID, Kind: "tool", Status: task.StepSucceeded,
					ToolName: runtimeToolShellExec}}
			}, wantAllow: true},
		{name: "fix plan with only a failed non-test command is allowed", plan: complete,
			steps: func(current task.Task) []task.Step {
				return []task.Step{{ID: "s1", TaskID: current.ID, Kind: "tool", Status: task.StepFailed,
					ToolName: runtimeToolShellExec, FailureReason: taskToolExecutionFailureReason}}
			}, wantAllow: true},
		{name: "fix plan with a failed test passes", plan: complete,
			steps: func(current task.Task) []task.Step {
				return []task.Step{failingTestStepForTest(current, "s1")}
			}, wantAllow: true},
		{name: "incomplete brief reports the brief error first", plan: incomplete,
			steps: func(current task.Task) []task.Step {
				return []task.Step{failingTestStepForTest(current, "s1")}
			}, wantSubstr: "brief"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, store, broker, _, current := newCloudDSHT08WorkspaceBroker(t)
			store.details.Plan = test.plan
			if test.steps != nil {
				store.details.Steps = test.steps(current)
			}
			call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-failing-1", cloudDSHWorkspaceWriteArguments{
				ExpectedRevision: 0, Path: "auth/session.go", Content: "draft saved only after confirmation",
			})
			result, err := broker.Execute(context.Background(), current, call)
			if test.wantAllow {
				if err != nil || result.Status != AgentToolNeedsUser || store.details.PendingRequest == nil {
					t.Fatalf("write was blocked without a failing-test gate reason: result=%#v, err=%v pending=%#v", result, err, store.details.PendingRequest)
				}
				return
			}
			if err == nil || domain.CodeOf(err) != domain.CodeInvalidArgument || !strings.Contains(err.Error(), test.wantSubstr) {
				t.Fatalf("write was not rejected with %q: result=%#v, err=%v", test.wantSubstr, result, err)
			}
			if strings.Contains(err.Error(), "failing reproduction test") != (test.wantSubstr == "failing reproduction test") {
				t.Fatalf("write error mixed the gates: err=%v", err)
			}
			if store.details.PendingRequest != nil {
				t.Fatal("rejected write created an approval request")
			}
		})
	}
}

func TestFixPlanHostFileWriteDoesNotRequireRedTest(t *testing.T) {
	complete := briefPlan(1, true)
	complete.FixPlan = fixPlanForTest()
	for _, test := range []struct {
		name  string
		steps func(task.Task) []task.Step
	}{
		{name: "fix plan without test evidence proceeds to later gates"},
		{name: "fix plan with only a passing test proceeds to later gates",
			steps: func(current task.Task) []task.Step {
				return []task.Step{{ID: "s1", TaskID: current.ID, Kind: "tool", Status: task.StepSucceeded,
					ToolName: runtimeToolShellExec}}
			}},
		{name: "fix plan with only a failed non-test command proceeds to later gates",
			steps: func(current task.Task) []task.Step {
				return []task.Step{{ID: "s1", TaskID: current.ID, Kind: "tool", Status: task.StepFailed,
					ToolName: runtimeToolShellExec, FailureReason: taskToolExecutionFailureReason}}
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			current := store.details.Task
			current.ExecutionLocation = "server"
			current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-brief", 1, "session-brief"
			store.details.Task = current
			store.details.Plan = complete
			if test.steps != nil {
				store.details.Steps = test.steps(current)
			}
			broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			call := ModelToolCall{CallID: "file-write-failing-1", Name: runtimeToolFileWrite,
				Arguments: `{"workspace_id":"w1234567","path":"auth/session.go","content":"synthetic"}`,
				TaskFence: &ModelTaskFence{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
					LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version, AgentInstanceID: current.AgentInstanceID,
					AgentGeneration: current.AgentGeneration, AgentSessionID: current.AgentSessionID,
					AuthorizationVersion: current.Authorization.Version}}
			call = setCloudDSHPlanStepMetadataForTest(t, call, "repro", &complete.Revision)
			result, err := broker.Execute(context.Background(), current, call)
			if (err != nil && strings.Contains(err.Error(), "failing reproduction test")) ||
				strings.Contains(result.Output, "failing reproduction test") {
				t.Fatalf("host file write was blocked by a universal red-test gate: result=%#v, err=%v", result, err)
			}
		})
	}
	t.Run("a failed test remains ordinary H07 evidence", func(t *testing.T) {
		fixture := newRuntimeToolExecutorFixture(t, nil)
		store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
		fixture.executor.store = store
		current := store.details.Task
		current.ExecutionLocation = "server"
		current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-brief", 1, "session-brief"
		store.details.Task = current
		store.details.Plan = complete
		store.details.Steps = []task.Step{failingTestStepForTest(current, "s1")}
		broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		call := ModelToolCall{CallID: "file-write-failing-2", Name: runtimeToolFileWrite,
			Arguments: `{"workspace_id":"w1234567","path":"auth/session.go","content":"synthetic"}`,
			TaskFence: &ModelTaskFence{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
				LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version, AgentInstanceID: current.AgentInstanceID,
				AgentGeneration: current.AgentGeneration, AgentSessionID: current.AgentSessionID,
				AuthorizationVersion: current.Authorization.Version}}
		call = setCloudDSHPlanStepMetadataForTest(t, call, "repro", &complete.Revision)
		// The assertion is that the same scoped write path is used regardless
		// of whether a red-test step exists. The fixture may fail at a later
		// execution boundary, but no red-test prerequisite may block it.
		_, err = broker.Execute(context.Background(), current, call)
		if err != nil && strings.Contains(err.Error(), "failing reproduction test") {
			t.Fatalf("host file write was blocked by a universal red-test gate: err=%v", err)
		}
	})
}

func TestTaskPlanFixPlanSubmission(t *testing.T) {
	store, broker, current := newBriefPlanBroker(t)
	steps := `[{"id":"repro","title":"Reproduce","status":"pending","completion_criteria":["crash reproduces"],"verification_applicability":"check","verification_check":{"kind":"test","basis":"risk_selected","command":"go test ./...","scope_mode":"targeted","coverage_paths":["auth"],"requirement_ids":[]}}]`
	manifest := `,"requirement_manifest":{"version":1,"obligations":[]}`
	base := `{"goal":"Fix the login crash","scope":"auth package only","verification_method":"run the failing test","steps":` + steps + manifest
	fixPlan := `,"fix_plan":{"changed_files":["auth/session.go"],"risks":["session expiry shifts"],"alternatives":["restart daily"]}}`

	result, err := broker.Execute(context.Background(), current, briefPlanCall(current, "plan-fix-1", base+fixPlan))
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("plan with a fix plan was rejected: result=%#v, err=%v", result, err)
	}
	var submitted task.Plan
	if err := json.Unmarshal([]byte(result.Output), &submitted); err != nil || submitted.Revision != 1 ||
		submitted.FixPlan == nil || len(submitted.FixPlan.ChangedFiles) != 1 || submitted.FixPlan.ChangedFiles[0] != "auth/session.go" ||
		len(submitted.FixPlan.Risks) != 1 || len(submitted.FixPlan.Alternatives) != 1 {
		t.Fatalf("submitted plan output = %s, err=%v", result.Output, err)
	}
	store.details.Plan = &submitted
	current = store.details.Task
	result, err = broker.Execute(context.Background(), current, briefPlanCall(current, "plan-fix-2", base+fixPlan))
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("revised plan with a fix plan was rejected: result=%#v, err=%v", result, err)
	}
	var resubmitted task.Plan
	if err := json.Unmarshal([]byte(result.Output), &resubmitted); err != nil || resubmitted.Revision != 2 || resubmitted.FixPlan == nil {
		t.Fatalf("revised plan output = %s, err=%v; want revision 2 with the fix plan", result.Output, err)
	}

	result, err = broker.Execute(context.Background(), current, briefPlanCall(current, "plan-fix-3", base+`}`))
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("plan without a fix plan was rejected: result=%#v, err=%v", result, err)
	}
	var plain task.Plan
	if err := json.Unmarshal([]byte(result.Output), &plain); err != nil || plain.FixPlan != nil ||
		strings.Contains(result.Output, "fix_plan") {
		t.Fatalf("plan without a fix plan output = %s, err=%v", result.Output, err)
	}

	tooMany := `{"goal":"Fix the login crash","scope":"auth package only","verification_method":"run the failing test","steps":` + steps + manifest +
		`,"fix_plan":{"changed_files":["a","b","c","d","e","f","g","h","i"]}}`
	if result, err := broker.Execute(context.Background(), current, briefPlanCall(current, "plan-fix-4", tooMany)); err != nil ||
		result.Status != AgentToolFailed {
		t.Fatalf("plan with too many fix plan files was not returned to the Agent: result=%#v, err=%v", result, err)
	}
	tooLong := `{"goal":"Fix the login crash","scope":"auth package only","verification_method":"run the failing test","steps":` + steps + manifest +
		`,"fix_plan":{"risks":["` + strings.Repeat("x", 513) + `"]}}`
	if result, err := broker.Execute(context.Background(), current, briefPlanCall(current, "plan-fix-5", tooLong)); err != nil ||
		result.Status != AgentToolFailed {
		t.Fatalf("plan with an overlong fix plan risk was not returned to the Agent: result=%#v, err=%v", result, err)
	}
	unknownField := `{"goal":"Fix the login crash","scope":"auth package only","verification_method":"run the failing test","steps":` + steps + manifest +
		`,"fix_plan":{"changed_files":[],"unknown":1}}`
	if _, err := broker.Execute(context.Background(), current, briefPlanCall(current, "plan-fix-6", unknownField)); err == nil ||
		domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("plan with an unknown fix plan field was not rejected: err=%v", err)
	}
}
