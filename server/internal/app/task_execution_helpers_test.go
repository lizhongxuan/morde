package app

import (
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestNewPendingApprovalRejectsInvalidDisplayText(t *testing.T) {
	call := ModelToolCall{CallID: "call-approval", Name: runtimeToolShellExec, Arguments: `{}`}
	hash, err := agentOperationHash(call.Name, json.RawMessage(call.Arguments))
	if err != nil {
		t.Fatal(err)
	}
	_, err = newPendingApproval(task.Task{ID: "t1234567"}, call, hash[:], AgentApprovalRequest{
		Object: "Workspace command", Content: "Run the command?", Impact: strings.Repeat("x", agentApprovalFieldMaxRunes+1),
	}, AgentHostFence{RuntimeGeneration: 1, RuntimeInstanceID: "runtime-1", ControlVersion: 1, AIAllowed: true}, 1)
	if domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("newPendingApproval() error = %v, want invalid state for unbounded display text", err)
	}
}

func evidenceCompleteApprovalRequest() AgentApprovalRequest {
	return AgentApprovalRequest{
		Object: "Workspace command", Content: "Run the command?", Impact: "The command can change files.",
		EvidencePointer: "task_step:s1234567", TriggerRule: ApprovalTriggerSensitiveHostTool,
		RejectionConsequence: ApprovalRejectionConsequenceHostTool,
	}
}

func TestNewPendingApprovalRequiresEvidenceFields(t *testing.T) {
	call := ModelToolCall{CallID: "call-approval", Name: runtimeToolShellExec, Arguments: `{}`}
	hash, err := agentOperationHash(call.Name, json.RawMessage(call.Arguments))
	if err != nil {
		t.Fatal(err)
	}
	fence := AgentHostFence{RuntimeGeneration: 1, RuntimeInstanceID: "runtime-1", ControlVersion: 1, AIAllowed: true}
	pending, err := newPendingApproval(task.Task{ID: "t1234567"}, call, hash[:], evidenceCompleteApprovalRequest(), fence, 1)
	if err != nil {
		t.Fatalf("newPendingApproval() with complete evidence error = %v", err)
	}
	if !pending.EvidenceComplete() {
		t.Fatal("newPendingApproval() must produce an evidence-complete pending request")
	}
	if pending.CurrentCost != nil || pending.TotalCost != nil {
		t.Fatal("newPendingApproval() must leave unknown costs nil, never 0")
	}
	// Unknown costs are a carried value, not a gap: the same approval with
	// an explicit known cost also passes.
	knownCost := evidenceCompleteApprovalRequest()
	knownCost.CurrentCost = &task.ApprovalCost{AmountMicros: 1200, Currency: "USD", Basis: task.ApprovalCostBasisEstimate}
	knownCost.TotalCost = &task.ApprovalCost{AmountMicros: 5400, Currency: "USD", Basis: task.ApprovalCostBasisSettled}
	if _, err := newPendingApproval(task.Task{ID: "t1234567"}, call, hash[:], knownCost, fence, 1); err != nil {
		t.Fatalf("newPendingApproval() with known costs error = %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*AgentApprovalRequest)
		want   []string
	}{
		{name: "no evidence pointer", mutate: func(a *AgentApprovalRequest) { a.EvidencePointer = "" }, want: []string{"evidence_pointer"}},
		{name: "no trigger rule", mutate: func(a *AgentApprovalRequest) { a.TriggerRule = "" }, want: []string{"trigger_rule"}},
		{name: "no rejection consequence", mutate: func(a *AgentApprovalRequest) { a.RejectionConsequence = "" }, want: []string{"rejection_consequence"}},
		{name: "no evidence at all", mutate: func(a *AgentApprovalRequest) {
			a.EvidencePointer, a.TriggerRule, a.RejectionConsequence = "", "", ""
		}, want: []string{"evidence_pointer", "trigger_rule", "rejection_consequence"}},
		{name: "negative current cost", mutate: func(a *AgentApprovalRequest) {
			a.CurrentCost = &task.ApprovalCost{AmountMicros: -1, Currency: "USD", Basis: task.ApprovalCostBasisEstimate}
		}, want: []string{"current_cost"}},
		{name: "invalid total cost currency", mutate: func(a *AgentApprovalRequest) {
			a.TotalCost = &task.ApprovalCost{AmountMicros: 1, Currency: "usd", Basis: task.ApprovalCostBasisEstimate}
		}, want: []string{"total_cost"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			approval := evidenceCompleteApprovalRequest()
			tc.mutate(&approval)
			_, err := newPendingApproval(task.Task{ID: "t1234567"}, call, hash[:], approval, fence, 1)
			if err == nil {
				t.Fatal("newPendingApproval() must reject an approval without complete evidence")
			}
			if domain.CodeOf(err) != domain.CodeInvalidState {
				t.Fatalf("newPendingApproval() error = %v, want invalid state", err)
			}
			for _, field := range tc.want {
				if !strings.Contains(err.Error(), field) {
					t.Fatalf("newPendingApproval() error %q does not name missing evidence %q", err.Error(), field)
				}
			}
		})
	}
}

func TestWithStepEvidencePointer(t *testing.T) {
	step := task.Step{ID: "s1234567"}
	filled := withStepEvidencePointer(AgentApprovalRequest{Object: "x"}, step)
	if filled.EvidencePointer != "task_step:s1234567" {
		t.Fatalf("evidence pointer = %q, want task_step:s1234567", filled.EvidencePointer)
	}
	kept := withStepEvidencePointer(AgentApprovalRequest{Object: "x", EvidencePointer: "task_step:s7654321"}, step)
	if kept.EvidencePointer != "task_step:s7654321" {
		t.Fatalf("existing evidence pointer must be kept, got %q", kept.EvidencePointer)
	}
	if got := withStepEvidencePointer(AgentApprovalRequest{Object: "x"}, task.Step{}); got.EvidencePointer != "" {
		t.Fatalf("evidence pointer without a step must stay empty, got %q", got.EvidencePointer)
	}
}

func TestApprovalRequestFromPending(t *testing.T) {
	pending := &task.PendingRequest{
		Object: "Workspace command", Content: "Run the command?", Impact: "The command can change files.",
		EvidencePointer: "task_step:s1234567",
		CurrentCost:     &task.ApprovalCost{AmountMicros: 1200, Currency: "USD", Basis: task.ApprovalCostBasisEstimate},
		TriggerRule:     ApprovalTriggerSensitiveHostTool, RejectionConsequence: ApprovalRejectionConsequenceHostTool,
	}
	request := approvalRequestFromPending(pending)
	if request.EvidencePointer != pending.EvidencePointer || request.TriggerRule != pending.TriggerRule ||
		request.RejectionConsequence != pending.RejectionConsequence || request.CurrentCost == nil ||
		request.CurrentCost.AmountMicros != 1200 || request.TotalCost != nil {
		t.Fatalf("approval request from pending lost body fields: %+v", request)
	}
	if got := approvalRequestFromPending(nil); got != (AgentApprovalRequest{}) {
		t.Fatalf("approval request from nil pending must be empty, got %+v", got)
	}
}
