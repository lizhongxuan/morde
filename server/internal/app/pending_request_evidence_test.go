package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestTaskBillingTotalActualMicros(t *testing.T) {
	known := func(value int64) *int64 { return &value }

	cases := []struct {
		name    string
		billing *TaskBilling
		want    *int64
	}{
		{name: "nil billing", billing: nil, want: nil},
		{name: "no costs", billing: &TaskBilling{}, want: nil},
		{name: "all settled", billing: &TaskBilling{Costs: []TaskCostSummary{
			{State: "a", ActualMicros: known(100)},
			{State: "b", ActualMicros: known(250)},
		}}, want: known(350)},
		{name: "one unsettled", billing: &TaskBilling{Costs: []TaskCostSummary{
			{State: "a", ActualMicros: known(100)},
			{State: "b", ActualMicros: nil},
		}}, want: nil},
		{name: "negative amount", billing: &TaskBilling{Costs: []TaskCostSummary{
			{State: "a", ActualMicros: known(-1)},
		}}, want: nil},
		{name: "overflow", billing: &TaskBilling{Costs: []TaskCostSummary{
			{State: "a", ActualMicros: known(math.MaxInt64)},
			{State: "b", ActualMicros: known(1)},
		}}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TaskBillingTotalActualMicros(tc.billing)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("TaskBillingTotalActualMicros() = %d, want nil", *got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("TaskBillingTotalActualMicros() = %v, want %d", got, *tc.want)
			}
		})
	}
}

func TestNewPendingApprovalCarriesEvidenceFields(t *testing.T) {
	call := ModelToolCall{CallID: "call-evidence", Name: runtimeToolShellExec, Arguments: `{}`}
	hash, err := agentOperationHash(call.Name, json.RawMessage(call.Arguments))
	if err != nil {
		t.Fatal(err)
	}
	revision := int64(3)
	preparation := AgentToolPreparation{
		Status: AgentToolApprovalRequired,
		Approval: AgentApprovalRequest{
			Object: "Host Workspace shell command", Content: "Run the command?",
			Impact:               "The command can change files.",
			RejectionConsequence: "If you reject, the command is not run and the task stops at this step.",
		},
		Fence:       AgentHostFence{RuntimeGeneration: 1, RuntimeInstanceID: "runtime-1", ControlVersion: 1, AIAllowed: true},
		TriggerRule: task.ApprovalTriggerHostSensitiveTool,
	}
	pending, err := newPendingApproval(task.Task{ID: "q1234567", CurrentRevision: &revision}, call, hash[:], preparation, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pending.RejectionConsequence != preparation.Approval.RejectionConsequence {
		t.Fatalf("RejectionConsequence = %q, want %q", pending.RejectionConsequence, preparation.Approval.RejectionConsequence)
	}
	if pending.TriggerRule != task.ApprovalTriggerHostSensitiveTool {
		t.Fatalf("TriggerRule = %q, want %q", pending.TriggerRule, task.ApprovalTriggerHostSensitiveTool)
	}
	// No trusted per-action cost source exists: the single-action cost must
	// stay unknown (nil), never a fabricated 0. EvidencePointer and
	// TotalCostMicros are filled by the pause caller, not here.
	if pending.ThisCostMicros != nil {
		t.Fatalf("ThisCostMicros = %d, want nil (unknown)", *pending.ThisCostMicros)
	}
	if pending.EvidencePointer != "" || pending.TotalCostMicros != nil {
		t.Fatalf("EvidencePointer/TotalCostMicros = %q/%v, want empty/nil from newPendingApproval", pending.EvidencePointer, pending.TotalCostMicros)
	}
}

func TestNewPendingApprovalRejectsUnknownTriggerRule(t *testing.T) {
	call := ModelToolCall{CallID: "call-evidence", Name: runtimeToolShellExec, Arguments: `{}`}
	hash, err := agentOperationHash(call.Name, json.RawMessage(call.Arguments))
	if err != nil {
		t.Fatal(err)
	}
	revision := int64(0)
	preparation := AgentToolPreparation{
		Status: AgentToolApprovalRequired,
		Approval: AgentApprovalRequest{
			Object: "Host Workspace shell command", Content: "Run the command?", Impact: "The command can change files.",
		},
		Fence:       AgentHostFence{RuntimeGeneration: 1, RuntimeInstanceID: "runtime-1", ControlVersion: 1, AIAllowed: true},
		TriggerRule: "bogus-rule",
	}
	if _, err := newPendingApproval(task.Task{ID: "q1234567", CurrentRevision: &revision}, call, hash[:], preparation, 1); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("newPendingApproval() error = %v, want invalid state for unknown trigger rule", err)
	}
}

func TestBoundedApprovalRejectionConsequence(t *testing.T) {
	base := AgentApprovalRequest{Object: "Object", Content: "Content", Impact: "Impact"}
	if _, ok := boundedApproval(base); !ok {
		t.Fatal("boundedApproval() rejected a blank rejection consequence, want it allowed for legacy approvals")
	}
	overlong := base
	overlong.RejectionConsequence = strings.Repeat("x", agentApprovalFieldMaxRunes+1)
	if _, ok := boundedApproval(overlong); ok {
		t.Fatal("boundedApproval() accepted an over-long rejection consequence")
	}
	invalid := base
	invalid.RejectionConsequence = "bad\xfftext"
	if _, ok := boundedApproval(invalid); ok {
		t.Fatal("boundedApproval() accepted invalid UTF-8 in the rejection consequence")
	}
}

func TestApprovalRequestConstructorsStateRejectionConsequence(t *testing.T) {
	shell := runtimeToolApprovalRequest(runtimeToolMapping{name: runtimeToolShellExec}, json.RawMessage(`{"command":"ls"}`))
	if shell.RejectionConsequence == "" {
		t.Fatal("shell approval request has no rejection consequence")
	}
	browser := runtimeToolApprovalRequest(runtimeToolMapping{name: runtimeToolBrowserType}, json.RawMessage(`{"text":"hi"}`))
	if browser.RejectionConsequence == "" {
		t.Fatal("browser approval request has no rejection consequence")
	}
	click := runtimeToolApprovalRequest(runtimeToolMapping{name: runtimeToolDesktopClick}, json.RawMessage(`{"window_id":"w1","x":10,"y":20}`))
	if click.RejectionConsequence == "" {
		t.Fatal("desktop click approval request has no rejection consequence")
	}
	unknown := runtimeToolApprovalRequest(runtimeToolMapping{name: "unknown-tool"}, json.RawMessage(`{}`))
	if unknown.RejectionConsequence == "" {
		t.Fatal("default approval request has no rejection consequence")
	}
	mcp := cloudDSHMCPApprovalRequest(
		cloudDSHMCPExecutionSource{connectorID: "k1234567", connectorName: "Search"},
		json.RawMessage(`{"query":"x"}`), make([]byte, sha256.Size))
	if mcp.RejectionConsequence == "" {
		t.Fatal("MCP approval request has no rejection consequence")
	}
	write := cloudDSHWorkspaceWriteApproval(
		cloudDSHWorkspaceWriteArguments{Path: "notes/a.txt", Content: "hello", ExpectedRevision: 1}, "w1234567")
	if write.RejectionConsequence == "" {
		t.Fatal("Workspace write approval request has no rejection consequence")
	}
	for name, request := range map[string]AgentApprovalRequest{
		"shell": shell, "browser": browser, "click": click, "unknown": unknown, "mcp": mcp, "write": write,
	} {
		if _, ok := boundedApproval(request); !ok {
			t.Fatalf("boundedApproval(%s) rejected a constructor approval request", name)
		}
	}
}

func TestNewHostlessWorkspaceApprovalCarriesEvidenceFields(t *testing.T) {
	revision := int64(0)
	current := task.Task{
		ID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567",
		ExecutionLocation: "server", WorkspaceID: "w1234567",
		InputRevision: &revision, WorkspaceCheckpointRevision: &revision,
		ContextDigest: make([]byte, sha256.Size), CurrentRevision: &revision,
	}
	call := ModelToolCall{CallID: "call-write", Name: cloudDSHWorkspaceWriteToolName, Arguments: `{}`}
	hash, err := agentOperationHash(call.Name, json.RawMessage(call.Arguments))
	if err != nil {
		t.Fatal(err)
	}
	approval := AgentApprovalRequest{
		Object: "Save a file to your Workspace", Content: "Path: notes/a.txt",
		Impact:               "This writes the shown file.",
		RejectionConsequence: "If you reject, the file is not saved and the task stops at this step.",
	}
	pending, err := newHostlessWorkspaceApproval(current, call, hash[:], approval, 1, task.ApprovalTriggerWorkspaceLiveWrite)
	if err != nil {
		t.Fatal(err)
	}
	if pending.TriggerRule != task.ApprovalTriggerWorkspaceLiveWrite {
		t.Fatalf("TriggerRule = %q, want %q", pending.TriggerRule, task.ApprovalTriggerWorkspaceLiveWrite)
	}
	if pending.RejectionConsequence != approval.RejectionConsequence {
		t.Fatalf("RejectionConsequence = %q, want %q", pending.RejectionConsequence, approval.RejectionConsequence)
	}
	if pending.ThisCostMicros != nil {
		t.Fatalf("ThisCostMicros = %d, want nil (unknown)", *pending.ThisCostMicros)
	}
	if _, err := newHostlessWorkspaceApproval(current, call, hash[:], approval, 1, "bogus-rule"); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("newHostlessWorkspaceApproval() error = %v, want invalid state for unknown trigger rule", err)
	}
}

func TestPauseForApprovalSetsEvidencePointerAndTotalCost(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	current := fixture.store.details.Task
	step := task.Step{ID: "s1234567", TaskID: current.ID, Sequence: 1, Kind: "tool", Status: task.StepRunning, Title: "Run", Summary: "Run the tool."}
	call := ModelToolCall{CallID: "call-pause", Name: runtimeToolShellExec, Arguments: `{}`}
	hash, err := agentOperationHash(call.Name, json.RawMessage(call.Arguments))
	if err != nil {
		t.Fatal(err)
	}
	totalCost := int64(12345)
	preparation := AgentToolPreparation{
		Status: AgentToolApprovalRequired,
		Approval: AgentApprovalRequest{
			Object: "Host Workspace shell command", Content: "Run the command?",
			Impact:               "The command can change files.",
			RejectionConsequence: "If you reject, the command is not run and the task stops at this step.",
		},
		Fence:       AgentHostFence{RuntimeGeneration: 1, RuntimeInstanceID: "runtime-1", ControlVersion: 1, AIAllowed: true},
		TriggerRule: task.ApprovalTriggerHostSensitiveTool,
	}
	request := AgentToolRequest{Task: current, Call: call, Arguments: json.RawMessage(call.Arguments)}
	result, err := broker.pauseForApproval(context.Background(), current, step, current.Usage, request, hash[:], preparation, nil, &totalCost)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != AgentToolNeedsUser {
		t.Fatalf("pauseForApproval() status = %q, want needs_user", result.Status)
	}
	if len(store.committedTaskProgress) != 1 {
		t.Fatalf("committed progress = %d entries, want 1", len(store.committedTaskProgress))
	}
	pending := store.committedTaskProgress[0].PendingRequest
	if pending == nil {
		t.Fatal("committed progress has no pending request")
	}
	if pending.EvidencePointer != "step:s1234567" {
		t.Fatalf("EvidencePointer = %q, want step:s1234567", pending.EvidencePointer)
	}
	if pending.TotalCostMicros == nil || *pending.TotalCostMicros != totalCost {
		t.Fatalf("TotalCostMicros = %v, want %d", pending.TotalCostMicros, totalCost)
	}
	if pending.ThisCostMicros != nil {
		t.Fatalf("ThisCostMicros = %d, want nil (unknown)", *pending.ThisCostMicros)
	}
	if pending.TriggerRule != task.ApprovalTriggerHostSensitiveTool {
		t.Fatalf("TriggerRule = %q, want %q", pending.TriggerRule, task.ApprovalTriggerHostSensitiveTool)
	}
	if pending.RejectionConsequence != preparation.Approval.RejectionConsequence {
		t.Fatalf("RejectionConsequence = %q, want %q", pending.RejectionConsequence, preparation.Approval.RejectionConsequence)
	}
}

func TestPauseForApprovalRejectsInvalidStepID(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	current := fixture.store.details.Task
	step := task.Step{ID: "not-a-step-id", TaskID: current.ID, Sequence: 1, Kind: "tool", Status: task.StepRunning, Title: "Run", Summary: "Run the tool."}
	call := ModelToolCall{CallID: "call-pause", Name: runtimeToolShellExec, Arguments: `{}`}
	hash, err := agentOperationHash(call.Name, json.RawMessage(call.Arguments))
	if err != nil {
		t.Fatal(err)
	}
	preparation := AgentToolPreparation{
		Status: AgentToolApprovalRequired,
		Approval: AgentApprovalRequest{
			Object: "Host Workspace shell command", Content: "Run the command?", Impact: "The command can change files.",
		},
		Fence:       AgentHostFence{RuntimeGeneration: 1, RuntimeInstanceID: "runtime-1", ControlVersion: 1, AIAllowed: true},
		TriggerRule: task.ApprovalTriggerHostSensitiveTool,
	}
	request := AgentToolRequest{Task: current, Call: call, Arguments: json.RawMessage(call.Arguments)}
	if _, err := broker.pauseForApproval(context.Background(), current, step, current.Usage, request, hash[:], preparation, nil, nil); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("pauseForApproval() error = %v, want invalid state for an invalid evidence step ID", err)
	}
	if len(store.committedTaskProgress) != 0 {
		t.Fatalf("committed progress = %d entries, want 0 after a rejected pause", len(store.committedTaskProgress))
	}
}
