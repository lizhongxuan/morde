package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func TestMinimalDiffPathNormalization(t *testing.T) {
	plan := briefPlan(1, true)
	plan.FixPlan = &task.FixPlan{ChangedFiles: []string{"auth/session.go", "pkg/api/handler.go"}}
	for _, test := range []struct {
		name    string
		changed []string
		want    []string
	}{
		{name: "in set", changed: []string{"auth/session.go"}},
		{name: "second file in set", changed: []string{"pkg/api/handler.go"}},
		{name: "leading slash", changed: []string{"/auth/session.go"}},
		{name: "dot slash prefix", changed: []string{"./auth/session.go"}},
		{name: "double slash", changed: []string{"auth//session.go"}},
		{name: "dot segment", changed: []string{"auth/./session.go"}},
		{name: "dotdot resolves inside the set", changed: []string{"auth/x/../session.go"}},
		{name: "backslash separator", changed: []string{`auth\session.go`}},
		{name: "out of set", changed: []string{"other/file.go"}, want: []string{"other/file.go"}},
		{name: "sibling file", changed: []string{"auth/other.go"}, want: []string{"auth/other.go"}},
		{name: "parent escape", changed: []string{"../auth/session.go"}, want: []string{"../auth/session.go"}},
		{name: "mixed in and out", changed: []string{"auth/session.go", "other/file.go"}, want: []string{"other/file.go"}},
		{name: "duplicate out of set is reported once", changed: []string{"other/file.go", "other/file.go"}, want: []string{"other/file.go"}},
		{name: "no changed files", changed: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := minimalDiffOutOfScopeFiles(plan, test.changed)
			if len(got) != len(test.want) {
				t.Fatalf("minimalDiffOutOfScopeFiles(%v) = %v, want %v", test.changed, got, test.want)
			}
			for index := range got {
				if got[index] != test.want[index] {
					t.Fatalf("minimalDiffOutOfScopeFiles(%v) = %v, want %v", test.changed, got, test.want)
				}
			}
		})
	}
	t.Run("empty allow set rejects any write", func(t *testing.T) {
		empty := briefPlan(1, true)
		empty.FixPlan = &task.FixPlan{ChangedFiles: []string{}}
		if got := minimalDiffOutOfScopeFiles(empty, []string{"auth/session.go"}); len(got) != 1 || got[0] != "auth/session.go" {
			t.Fatalf("empty allow set violations = %v, want [auth/session.go]", got)
		}
	})
	t.Run("no fix plan is unaffected", func(t *testing.T) {
		if got := minimalDiffOutOfScopeFiles(briefPlan(1, true), []string{"other/file.go"}); len(got) != 0 {
			t.Fatalf("plan without a fix plan violations = %v, want none", got)
		}
		if got := minimalDiffOutOfScopeFiles(nil, []string{"other/file.go"}); len(got) != 0 {
			t.Fatalf("nil plan violations = %v, want none", got)
		}
	})
}

func minimalDiffHostWriteCall(current task.Task, callID, path string) ModelToolCall {
	return ModelToolCall{CallID: callID, Name: runtimeToolFileWrite,
		Arguments: `{"workspace_id":"w1234567","path":"` + path + `","content":"synthetic"}`,
		TaskFence: &ModelTaskFence{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
			LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version, AgentInstanceID: current.AgentInstanceID,
			AgentGeneration: current.AgentGeneration, AgentSessionID: current.AgentSessionID,
			AuthorizationVersion: current.Authorization.Version}}
}

func TestMinimalDiffHostFileWriteGate(t *testing.T) {
	fix := briefPlan(1, true)
	fix.FixPlan = fixPlanForTest()
	empty := briefPlan(1, true)
	empty.FixPlan = &task.FixPlan{ChangedFiles: []string{}}
	for _, test := range []struct {
		name        string
		plan        *task.Plan
		path        string
		withRedTest bool
		wantSubstr  string
		wantAllow   bool
	}{
		{name: "in-scope file passes the gates", plan: fix, path: "auth/session.go", withRedTest: true, wantAllow: true},
		{name: "dot slash spelling passes", plan: fix, path: "./auth/session.go", withRedTest: true, wantAllow: true},
		{name: "leading slash spelling passes", plan: fix, path: "/auth/session.go", withRedTest: true, wantAllow: true},
		{name: "out-of-scope file is rejected", plan: fix, path: "other/file.go", withRedTest: true, wantSubstr: "fix_plan.changed_files"},
		{name: "empty allow set rejects any write", plan: empty, path: "auth/session.go", withRedTest: true, wantSubstr: "fix_plan.changed_files"},
		{name: "no fix plan is unaffected", plan: briefPlan(1, true), path: "other/file.go", wantAllow: true},
		{name: "failing-test gate runs before the minimal-diff gate", plan: fix, path: "other/file.go", wantSubstr: "failing reproduction test"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			current := store.details.Task
			current.ExecutionLocation = "server"
			current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-brief", 1, "session-brief"
			store.details.Task = current
			store.details.Plan = test.plan
			if test.withRedTest {
				store.details.Steps = []task.Step{failingTestStepForTest(current, "s1")}
			}
			broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			result, err := broker.Execute(context.Background(), current, minimalDiffHostWriteCall(current, "file-write-minimal-1", test.path))
			if test.wantAllow {
				// The fixture has no file-write handler, so the call fails at
				// a later stage; the assertion is that neither the
				// failing-test gate nor the minimal-diff gate fired.
				if err != nil && (strings.Contains(err.Error(), "failing reproduction test") || strings.Contains(err.Error(), "fix_plan.changed_files")) {
					t.Fatalf("in-scope host file write was rejected by a write gate: err=%v", err)
				}
				return
			}
			if err != nil || result.Status != AgentToolFailed || !strings.Contains(result.Output, test.wantSubstr) {
				t.Fatalf("host file write was not returned to the Agent with %q: result=%#v, err=%v", test.wantSubstr, result, err)
			}
			if test.wantSubstr == "failing reproduction test" && strings.Contains(result.Output, "fix_plan.changed_files") {
				t.Fatalf("host file write result mixed the gates: result=%#v", result)
			}
		})
	}
}

func TestMinimalDiffWorkspaceWriteGate(t *testing.T) {
	fix := briefPlan(1, true)
	fix.FixPlan = fixPlanForTest()
	empty := briefPlan(1, true)
	empty.FixPlan = &task.FixPlan{ChangedFiles: []string{}}
	incomplete := briefPlan(1, false)
	incomplete.FixPlan = fixPlanForTest()
	for _, test := range []struct {
		name       string
		plan       *task.Plan
		path       string
		wantSubstr string
		wantAllow  bool
	}{
		{name: "no plan is unaffected", plan: nil, path: "reports/summary.txt", wantAllow: true},
		{name: "complete brief without fix plan is unaffected", plan: briefPlan(1, true), path: "reports/summary.txt", wantAllow: true},
		{name: "in-scope file passes the gate", plan: fix, path: "auth/session.go", wantAllow: true},
		{name: "dot slash spelling passes", plan: fix, path: "./auth/session.go", wantAllow: true},
		{name: "out-of-scope file is rejected", plan: fix, path: "other/file.go", wantSubstr: "fix_plan.changed_files"},
		{name: "empty allow set rejects any write", plan: empty, path: "auth/session.go", wantSubstr: "fix_plan.changed_files"},
		{name: "incomplete brief reports the brief error first", plan: incomplete, path: "other/file.go", wantSubstr: "brief"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, store, broker, _, current := newCloudDSHT08WorkspaceBroker(t)
			store.details.Plan = test.plan
			call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-minimal-1", cloudDSHWorkspaceWriteArguments{
				ExpectedRevision: 0, Path: test.path, Content: "draft saved only after confirmation",
			})
			result, err := broker.Execute(context.Background(), current, call)
			if test.wantAllow {
				if err != nil || result.Status != AgentToolNeedsUser || store.details.PendingRequest == nil {
					t.Fatalf("write was blocked without a minimal-diff reason: result=%#v, err=%v pending=%#v", result, err, store.details.PendingRequest)
				}
				return
			}
			if err == nil || domain.CodeOf(err) != domain.CodeInvalidArgument || !strings.Contains(err.Error(), test.wantSubstr) {
				t.Fatalf("write was not rejected with %q: result=%#v, err=%v", test.wantSubstr, result, err)
			}
			if test.wantSubstr == "brief" && strings.Contains(err.Error(), "fix_plan.changed_files") {
				t.Fatalf("write error mixed the gates: err=%v", err)
			}
			if store.details.PendingRequest != nil {
				t.Fatal("rejected write created an approval request")
			}
		})
	}
}

func minimalDiffFileWriteInputs(callID, path string) []ModelInput {
	arguments, err := json.Marshal(map[string]string{"workspace_id": "w1234567", "path": path, "content": "synthetic"})
	if err != nil {
		panic(err)
	}
	output, err := json.Marshal(map[string]any{"path": path, "bytes_written": len("synthetic")})
	if err != nil {
		panic(err)
	}
	return []ModelInput{
		{Type: "function_call", CallID: callID, Name: runtimeToolFileWrite, Arguments: string(arguments)},
		{Type: "function_call_output", CallID: callID, Output: string(output)},
	}
}

func TestMinimalDiffCompleteRecheck(t *testing.T) {
	fix := briefPlan(1, true)
	fix.FixPlan = fixPlanForTest()
	for _, test := range []struct {
		name          string
		plan          *task.Plan
		path          string
		wantViolation bool
	}{
		{name: "in-scope change passes the recheck", plan: fix, path: "auth/session.go"},
		{name: "out-of-scope change blocks delivery", plan: fix, path: "other/file.go", wantViolation: true},
		{name: "no fix plan is unaffected", plan: briefPlan(1, true), path: "other/file.go"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			store.details.Task.Objective = "Answer the question in chat."
			setCloudDSHCompletionFence(&store.details.Task)
			store.details.Plan = test.plan
			// The E07 test gate runs after the minimal-diff recheck: the
			// fix-plan cases carry green test evidence so the recheck, not
			// the test gate, decides the outcome.
			if test.plan != nil && test.plan.FixPlan != nil {
				store.details.Steps = greenTestStepsForTest(store.details.Task)
			}
			broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			claimed := store.details.Task
			// The evidence is the completion input history, which predates
			// the plan in the real flow: a file written before the fix plan
			// was submitted is caught here exactly like one written after.
			if err := broker.Complete(context.Background(), claimed,
				cloudDSHCompletionFor(claimed, "Fixed the login crash.", minimalDiffFileWriteInputs("file-write-minimal-1", test.path))); err != nil {
				t.Fatalf("Complete() error = %v", err)
			}
			if test.wantViolation {
				if store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "minimal_diff_violation" ||
					store.details.Task.Result == "Fixed the login crash." {
					t.Fatalf("out-of-scope change was not blocked: status=%q waiting=%q result=%q",
						store.details.Task.Status, store.details.Task.WaitingReason, store.details.Task.Result)
				}
				return
			}
			if store.details.Task.WaitingReason == "minimal_diff_violation" {
				t.Fatalf("in-scope change was blocked by the minimal-diff recheck: task=%#v", store.details.Task)
			}
		})
	}
}

func TestMinimalDiffCompleteRecheckOnCheckpointMismatch(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	store.details.Task.Objective = "Answer the question in chat."
	setCloudDSHCompletionFence(&store.details.Task)
	plan := briefPlan(1, true)
	plan.FixPlan = fixPlanForTest()
	store.details.Plan = plan
	// Green test evidence, so the E07 test gate passes and the minimal-diff
	// recheck decides the outcome.
	store.details.Steps = greenTestStepsForTest(store.details.Task)
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	completion := cloudDSHCompletionFor(claimed, "Fixed the login crash.", minimalDiffFileWriteInputs("file-write-minimal-1", "other/file.go"))
	store.details.Task.CheckpointDigest = bytes.Repeat([]byte{8}, 32)
	if err := broker.Complete(context.Background(), claimed, completion); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "minimal_diff_violation" ||
		store.details.Task.Result == "Fixed the login crash." {
		t.Fatalf("stale completion with an out-of-scope change was not blocked: status=%q waiting=%q result=%q",
			store.details.Task.Status, store.details.Task.WaitingReason, store.details.Task.Result)
	}
}

func minimalDiffHostlessWriteInputs(t *testing.T, workspaceStore *cloudDSHT08WorkspaceStore,
	fixture *runtimeToolExecutorFixture, current task.Task, path string) []ModelInput {
	t.Helper()
	content := []byte("package auth\n\n// fixed session handling\n")
	file := syncServiceFile(path, content)
	workspaceStore.workspace.Revision = 1
	workspaceStore.snapshots[1] = []workspace.File{file}
	fixture.workspaceBlocks = &proofBlocks{syncBlockMemory: newSyncBlockMemory()}
	fixture.workspaceBlocks.data[file.SHA256] = content
	fixture.executor.workspaces.blocks = fixture.workspaceBlocks
	manifestSHA, err := workspace.ManifestTreeWithMetadataSHA256([]workspace.File{file}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-minimal-diff-complete",
		cloudDSHWorkspaceWriteArguments{ExpectedRevision: 0, Path: file.Path, Content: string(content)})
	requestHash, err := agentOperationHash(call.Name, json.RawMessage(call.Arguments))
	if err != nil {
		t.Fatal(err)
	}
	operationKey := agentOperationKey(call.CallID)
	receipt := TaskWorkspacePublishReceipt{State: TaskWorkspacePublishSucceeded, WorkspaceID: current.WorkspaceID,
		BaseRevision: 0, PublishedRevision: 1, ManifestSHA256: manifestSHA, RequestHash: hex.EncodeToString(requestHash[:])}
	receiptKey := taskWorkspacePublishReceiptKey(current.ID, operationKey)
	workspaceStore.taskWorkspacePublishReceipts[receiptKey] = receipt
	workspaceStore.taskWorkspacePublishCommands[receiptKey] = TaskWorkspacePublishCommand{
		OwnerID: current.OwnerID, TaskID: current.ID, WorkspaceID: current.WorkspaceID, BaseRevision: 0, RequestHash: requestHash[:],
	}
	toolResult := cloudDSHWorkspaceReceiptResult(receipt, operationKey)
	return []ModelInput{
		{Type: "function_call", CallID: call.CallID, Name: call.Name, Arguments: call.Arguments},
		{Type: "function_call_output", CallID: call.CallID, Output: toolResult.Output},
	}
}

func TestMinimalDiffHostlessCompleteRecheck(t *testing.T) {
	fix := briefPlan(1, true)
	fix.FixPlan = fixPlanForTest()
	for _, test := range []struct {
		name          string
		plan          *task.Plan
		path          string
		wantViolation bool
	}{
		{name: "in-scope change completes", plan: fix, path: "auth/session.go"},
		{name: "out-of-scope change blocks delivery", plan: fix, path: "other/file.go", wantViolation: true},
		{name: "no fix plan is unaffected", plan: briefPlan(1, true), path: "other/file.go"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, store, broker, workspaceStore, current := newCloudDSHT08WorkspaceBroker(t)
			current.Objective = "Fix the login crash in the auth package."
			setCloudDSHCompletionFence(&current)
			store.details.Task = current
			store.details.Plan = test.plan
			inputs := minimalDiffHostlessWriteInputs(t, workspaceStore, fixture, current, test.path)
			if err := broker.Complete(context.Background(), current,
				cloudDSHCompletionFor(current, "Fixed the login crash.", inputs)); err != nil {
				t.Fatalf("Complete() error = %v", err)
			}
			if test.wantViolation {
				if store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "minimal_diff_violation" ||
					store.details.Task.Result == "Fixed the login crash." {
					t.Fatalf("out-of-scope hostless change was not blocked: status=%q waiting=%q result=%q",
						store.details.Task.Status, store.details.Task.WaitingReason, store.details.Task.Result)
				}
				return
			}
			if store.details.Task.Status != task.StatusCompleted || store.details.Task.Result != "Fixed the login crash." {
				t.Fatalf("in-scope hostless change did not complete: status=%q waiting=%q result=%q",
					store.details.Task.Status, store.details.Task.WaitingReason, store.details.Task.Result)
			}
		})
	}
}
