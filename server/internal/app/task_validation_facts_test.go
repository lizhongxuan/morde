package app

import (
	"testing"

	"morde/server/internal/domain/task"
)

func TestDeriveTaskValidationApprovalFacts(t *testing.T) {
	approvedCall := "call-approved-1"
	operations := func(pairs ...string) map[string]string {
		ledger := make(map[string]string)
		for index := 0; index+1 < len(pairs); index += 2 {
			ledger[pairs[index]] = pairs[index+1]
		}
		return ledger
	}
	for _, tc := range []struct {
		name       string
		approvals  []TaskValidationApprovalRecord
		operations map[string]string
		wantState  TaskValidationFactState
		wantOK     bool
	}{
		{
			name:      "no approvals",
			wantState: TaskValidationFactAbsent,
			wantOK:    false,
		},
		{
			name:       "approved with settled operation",
			approvals:  []TaskValidationApprovalRecord{{CallID: approvedCall, Status: task.PendingRequestApproved}},
			operations: operations(AgentOperationKey(approvedCall), "succeeded"),
			wantState:  TaskValidationFactPresent,
			wantOK:     true,
		},
		{
			name:       "approved with failed operation",
			approvals:  []TaskValidationApprovalRecord{{CallID: approvedCall, Status: task.PendingRequestApproved}},
			operations: operations(AgentOperationKey(approvedCall), "failed"),
			wantState:  TaskValidationFactPresent,
			wantOK:     true,
		},
		{
			name:       "approved without operation",
			approvals:  []TaskValidationApprovalRecord{{CallID: approvedCall, Status: task.PendingRequestApproved}},
			operations: operations(),
			wantState:  TaskValidationFactPresent,
			wantOK:     false,
		},
		{
			name:       "approved with dispatched operation",
			approvals:  []TaskValidationApprovalRecord{{CallID: approvedCall, Status: task.PendingRequestApproved}},
			operations: operations(AgentOperationKey(approvedCall), "dispatched"),
			wantState:  TaskValidationFactPresent,
			wantOK:     false,
		},
		{
			name:       "pending approval open",
			approvals:  []TaskValidationApprovalRecord{{CallID: approvedCall, Status: task.PendingRequestPending}},
			operations: operations(),
			wantState:  TaskValidationFactAbsent,
			wantOK:     false,
		},
		{
			name:       "rejected only",
			approvals:  []TaskValidationApprovalRecord{{CallID: approvedCall, Status: task.PendingRequestRejected}},
			operations: operations(),
			wantState:  TaskValidationFactAbsent,
			wantOK:     true,
		},
		{
			name:       "approved with empty call id",
			approvals:  []TaskValidationApprovalRecord{{Status: task.PendingRequestApproved}},
			operations: operations(),
			wantState:  TaskValidationFactPresent,
			wantOK:     false,
		},
		{
			name: "approved plus pending",
			approvals: []TaskValidationApprovalRecord{
				{CallID: approvedCall, Status: task.PendingRequestApproved},
				{CallID: "call-pending-1", Status: task.PendingRequestPending},
			},
			operations: operations(AgentOperationKey(approvedCall), "succeeded"),
			wantState:  TaskValidationFactPresent,
			wantOK:     false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, ok := DeriveTaskValidationApprovalFacts(tc.approvals, tc.operations)
			if state != tc.wantState || ok != tc.wantOK {
				t.Fatalf("DeriveTaskValidationApprovalFacts() = (%q, %v), want (%q, %v)", state, ok, tc.wantState, tc.wantOK)
			}
		})
	}
}

func TestDeriveTaskValidationCorrectionFacts(t *testing.T) {
	failed := func(tool, reason string, sequence int64) TaskValidationStepRecord {
		return TaskValidationStepRecord{ToolName: tool, FailureReason: reason, Status: "failed", Sequence: sequence}
	}
	succeeded := func(tool string, sequence int64) TaskValidationStepRecord {
		return TaskValidationStepRecord{ToolName: tool, Status: "succeeded", Sequence: sequence}
	}
	for _, tc := range []struct {
		name      string
		steps     []TaskValidationStepRecord
		wantState TaskValidationFactState
		wantOK    bool
	}{
		{
			name:      "no steps",
			wantState: TaskValidationFactAbsent,
			wantOK:    false,
		},
		{
			name:      "single failure is not repeated",
			steps:     []TaskValidationStepRecord{failed("go_test", "test_failed", 1)},
			wantState: TaskValidationFactAbsent,
			wantOK:    false,
		},
		{
			name:      "repeated failure without correction",
			steps:     []TaskValidationStepRecord{failed("go_test", "test_failed", 1), failed("go_test", "test_failed", 2)},
			wantState: TaskValidationFactPresent,
			wantOK:    false,
		},
		{
			name: "repeated failure with later success",
			steps: []TaskValidationStepRecord{
				failed("go_test", "test_failed", 1), failed("go_test", "test_failed", 2), succeeded("go_test", 3),
			},
			wantState: TaskValidationFactPresent,
			wantOK:    true,
		},
		{
			name: "repeated failure with earlier success only",
			steps: []TaskValidationStepRecord{
				succeeded("go_test", 1), failed("go_test", "test_failed", 2), failed("go_test", "test_failed", 3),
			},
			wantState: TaskValidationFactPresent,
			wantOK:    false,
		},
		{
			name: "different reasons are different errors",
			steps: []TaskValidationStepRecord{
				failed("go_test", "test_failed", 1), failed("go_test", "compile_failed", 2),
			},
			wantState: TaskValidationFactAbsent,
			wantOK:    false,
		},
		{
			name: "empty reason repeats by tool",
			steps: []TaskValidationStepRecord{
				failed("go_test", "", 1), failed("go_test", "", 2),
			},
			wantState: TaskValidationFactPresent,
			wantOK:    false,
		},
		{
			name: "one repeated pair corrected and another not",
			steps: []TaskValidationStepRecord{
				failed("go_test", "test_failed", 1), failed("go_test", "test_failed", 2), succeeded("go_test", 3),
				failed("go_build", "compile_failed", 4), failed("go_build", "compile_failed", 5),
			},
			wantState: TaskValidationFactPresent,
			wantOK:    false,
		},
		{
			name: "unnamed tool steps ignored",
			steps: []TaskValidationStepRecord{
				{FailureReason: "test_failed", Status: "failed", Sequence: 1},
				{FailureReason: "test_failed", Status: "failed", Sequence: 2},
			},
			wantState: TaskValidationFactAbsent,
			wantOK:    false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, ok := DeriveTaskValidationCorrectionFacts(tc.steps)
			if state != tc.wantState || ok != tc.wantOK {
				t.Fatalf("DeriveTaskValidationCorrectionFacts() = (%q, %v), want (%q, %v)", state, ok, tc.wantState, tc.wantOK)
			}
		})
	}
}

func TestDeriveTaskValidationReplayVerified(t *testing.T) {
	step := func(callID string, sequence int64) TaskValidationStepRecord {
		return TaskValidationStepRecord{ToolName: "shell_exec", Status: "succeeded", Sequence: sequence, ToolCallKey: callID}
	}
	for _, tc := range []struct {
		name       string
		steps      []TaskValidationStepRecord
		approvals  []TaskValidationApprovalRecord
		operations map[string]string
		want       bool
	}{
		{
			name: "no steps",
			want: false,
		},
		{
			name:       "step with settled operation",
			steps:      []TaskValidationStepRecord{step("call-1", 1)},
			operations: map[string]string{AgentOperationKey("call-1"): "succeeded"},
			want:       true,
		},
		{
			name:       "step with failed operation",
			steps:      []TaskValidationStepRecord{step("call-1", 1)},
			operations: map[string]string{AgentOperationKey("call-1"): "failed"},
			want:       true,
		},
		{
			name:       "step without operation",
			steps:      []TaskValidationStepRecord{step("call-1", 1)},
			operations: map[string]string{},
			want:       false,
		},
		{
			name:       "step with dispatched operation",
			steps:      []TaskValidationStepRecord{step("call-1", 1)},
			operations: map[string]string{AgentOperationKey("call-1"): "dispatched"},
			want:       false,
		},
		{
			name:       "unrelated operation stuck dispatched",
			steps:      []TaskValidationStepRecord{step("call-1", 1)},
			operations: map[string]string{AgentOperationKey("call-1"): "succeeded", AgentOperationKey("call-2"): "dispatched"},
			want:       false,
		},
		{
			name:       "rejected call covered by decision",
			steps:      []TaskValidationStepRecord{step("call-1", 1)},
			approvals:  []TaskValidationApprovalRecord{{CallID: "call-1", Status: task.PendingRequestRejected}},
			operations: map[string]string{},
			want:       true,
		},
		{
			name:       "approved call without operation is not covered",
			steps:      []TaskValidationStepRecord{step("call-1", 1)},
			approvals:  []TaskValidationApprovalRecord{{CallID: "call-1", Status: task.PendingRequestApproved}},
			operations: map[string]string{},
			want:       false,
		},
		{
			name:  "empty call key steps ignored",
			steps: []TaskValidationStepRecord{{ToolName: "shell_exec", Status: "succeeded", Sequence: 1}},
			want:  false,
		},
		{
			name: "empty call key step plus matched step",
			steps: []TaskValidationStepRecord{
				{ToolName: "shell_exec", Status: "succeeded", Sequence: 1},
				step("call-1", 2),
			},
			operations: map[string]string{AgentOperationKey("call-1"): "succeeded"},
			want:       true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveTaskValidationReplayVerified(tc.steps, tc.approvals, tc.operations); got != tc.want {
				t.Fatalf("DeriveTaskValidationReplayVerified() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVerifyTaskValidationVerdictReplay(t *testing.T) {
	report := validTaskValidationReviewReport()
	facts := TaskValidationVerdictFacts{
		TaskExecution:             TaskValidationFactAbsent,
		IrreversibleAction:        TaskValidationFactAbsent,
		RepeatedError:             TaskValidationFactAbsent,
		EvidenceContentVerified:   true,
		ClarificationHistoryKnown: true,
	}
	if !VerifyTaskValidationVerdictReplay(report, facts, TaskValidationPass) {
		t.Fatalf("VerifyTaskValidationVerdictReplay() = false for the re-derived PASS verdict")
	}
	if VerifyTaskValidationVerdictReplay(report, facts, TaskValidationPending) {
		t.Fatalf("VerifyTaskValidationVerdictReplay() = true for a stored pending verdict that differs from the re-derived PASS")
	}
}

// TestIndependentVerifierResolvesFixTaskVerdict is the end-to-end check for
// E09: a fix task whose stored records show an approved action with a settled
// operation, a repeated test failure fixed by a later succeeding retry, and a
// fully settled operation ledger resolves to PASS instead of staying pending.
// The same report stays pending when any one server fact is unverified.
func TestIndependentVerifierResolvesFixTaskVerdict(t *testing.T) {
	fixApprovals := []TaskValidationApprovalRecord{{CallID: "call-fix-1", Status: task.PendingRequestApproved}}
	fixSteps := []TaskValidationStepRecord{
		{ToolName: "go_test", FailureReason: "test_failed", Status: "failed", Sequence: 1},
		{ToolName: "go_test", FailureReason: "test_failed", Status: "failed", Sequence: 2},
		{ToolName: "go_test", Status: "succeeded", Sequence: 3},
		{ToolName: "shell_exec", Status: "succeeded", Sequence: 4, ToolCallKey: "call-fix-1"},
	}
	fixOperations := map[string]string{AgentOperationKey("call-fix-1"): "succeeded"}
	resolve := func(approvals []TaskValidationApprovalRecord, operations map[string]string, steps []TaskValidationStepRecord) TaskValidationState {
		facts := TaskValidationVerdictFacts{
			TaskExecution:             TaskValidationFactPresent,
			EvidenceContentVerified:   true,
			ClarificationHistoryKnown: true,
		}
		facts.IrreversibleAction, facts.ApprovalHistoryVerified = DeriveTaskValidationApprovalFacts(approvals, operations)
		facts.RepeatedError, facts.CorrectionRecordVerified = DeriveTaskValidationCorrectionFacts(steps)
		facts.ReplayVerified = DeriveTaskValidationReplayVerified(steps, approvals, operations)
		return SummarizeTaskValidationEvaluations(EvaluateTaskValidationReview(validTaskValidationReviewReport(), facts)).State
	}
	if got := resolve(fixApprovals, fixOperations, fixSteps); got != TaskValidationPass {
		t.Fatalf("fix Task verdict = %q, want PASS", got)
	}
	if got := resolve(fixApprovals, map[string]string{}, fixSteps); got != TaskValidationPending {
		t.Fatalf("fix Task verdict without a settled operation = %q, want pending", got)
	}
	if got := resolve(fixApprovals, fixOperations, fixSteps[:3]); got != TaskValidationPending {
		t.Fatalf("fix Task verdict without replay coverage = %q, want pending", got)
	}
}
