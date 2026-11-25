package app

import (
	"morde/server/internal/domain/task"
)

// TaskValidationApprovalRecord is one stored approval-gated action of a task,
// read from task_pending_requests. It is a stored server fact, never a model
// claim: the approval gate is the server's record of an irreversible or
// sensitive action, so only an approved row proves such an action ran.
type TaskValidationApprovalRecord struct {
	CallID string
	Status task.PendingRequestStatus
}

// TaskValidationStepRecord is one stored tool step of a task, read from
// task_steps. Sequence orders steps inside the task; ToolCallKey is the model
// tool call id the step records, or empty when the step never dispatched one.
type TaskValidationStepRecord struct {
	ToolName      string
	FailureReason string
	Status        string
	Sequence      int64
	ToolCallKey   string
}

// DeriveTaskValidationApprovalFacts derives the H05 (approval) verdict facts
// from stored approval rows and the stored operation ledger
// (operation_key -> status).
//
// IrreversibleAction is present when an approval-gated action was actually
// approved; it is absent when no approval-gated action ran, which makes H05
// not applicable. Approval history is verified only when every approval row
// is in a terminal state and every approved action has a matching settled
// operation, so a reviewer claim about approvals can be checked against the
// ledger instead of trusted on its own.
func DeriveTaskValidationApprovalFacts(approvals []TaskValidationApprovalRecord, operations map[string]string) (TaskValidationFactState, bool) {
	state := TaskValidationFactAbsent
	verified := len(approvals) > 0
	for _, approval := range approvals {
		if approval.Status == task.PendingRequestApproved {
			state = TaskValidationFactPresent
		}
		switch approval.Status {
		case task.PendingRequestApproved, task.PendingRequestRejected, task.PendingRequestExpired, task.PendingRequestCancelled:
		default:
			verified = false
			continue
		}
		if approval.Status != task.PendingRequestApproved {
			continue
		}
		if approval.CallID == "" {
			verified = false
			continue
		}
		status, ok := operations[AgentOperationKey(approval.CallID)]
		if !ok || (status != "succeeded" && status != "failed") {
			verified = false
		}
	}
	return state, verified
}

// DeriveTaskValidationCorrectionFacts derives the H06 (correction) verdict
// facts from stored tool steps. A repeated error is the same (tool, failure
// reason) failing twice; its correction is verified only when the record
// shows a later succeeding step for the same tool, i.e. the retry chain
// records the error being fixed. A single failure is not a repeated error.
//
// There is no corrections table in this base, so a correction settled into a
// reusable lint, check or skill cannot be verified here; the verified flag
// means the retry chain records a succeeding correction, not that a reusable
// check was created.
func DeriveTaskValidationCorrectionFacts(steps []TaskValidationStepRecord) (TaskValidationFactState, bool) {
	type failureSignature struct {
		tool   string
		reason string
	}
	failed := make(map[failureSignature][]int64)
	succeeded := make(map[string]int64)
	for _, step := range steps {
		if step.ToolName == "" {
			continue
		}
		switch step.Status {
		case "failed":
			signature := failureSignature{tool: step.ToolName, reason: step.FailureReason}
			failed[signature] = append(failed[signature], step.Sequence)
		case "succeeded":
			if step.Sequence > succeeded[step.ToolName] {
				succeeded[step.ToolName] = step.Sequence
			}
		}
	}
	state := TaskValidationFactAbsent
	verified := false
	for signature, sequences := range failed {
		if len(sequences) < 2 {
			continue
		}
		state = TaskValidationFactPresent
		lastFailed := sequences[0]
		for _, sequence := range sequences[1:] {
			if sequence > lastFailed {
				lastFailed = sequence
			}
		}
		if succeededAt, ok := succeeded[signature.tool]; ok && succeededAt > lastFailed {
			verified = true
		} else {
			return state, false
		}
	}
	return state, verified
}

// DeriveTaskValidationReplayVerified derives the H07 (replay) verdict fact:
// the stored operation ledger covers every recorded tool call and is fully
// settled, so the execution can be replayed from stored records. A call
// whose outcome is a terminal approval decision (rejected, expired or
// cancelled) is covered by that decision instead of an operation row, since
// it never ran. Any unsettled operation, or any recorded call with neither a
// settled operation nor a terminal decision, leaves replay unverified.
func DeriveTaskValidationReplayVerified(steps []TaskValidationStepRecord, approvals []TaskValidationApprovalRecord, operations map[string]string) bool {
	for _, status := range operations {
		if status != "succeeded" && status != "failed" {
			return false
		}
	}
	terminalDecision := make(map[string]struct{})
	for _, approval := range approvals {
		if approval.CallID == "" {
			continue
		}
		switch approval.Status {
		case task.PendingRequestRejected, task.PendingRequestExpired, task.PendingRequestCancelled:
			terminalDecision[approval.CallID] = struct{}{}
		}
	}
	checked := 0
	for _, step := range steps {
		if step.ToolCallKey == "" {
			continue
		}
		checked++
		if status, ok := operations[AgentOperationKey(step.ToolCallKey)]; ok && (status == "succeeded" || status == "failed") {
			continue
		}
		if _, ok := terminalDecision[step.ToolCallKey]; ok {
			continue
		}
		return false
	}
	return checked > 0
}

// VerifyTaskValidationVerdictReplay re-derives the verdict from the same
// report and facts and compares it with the stored verdict state. It is the
// deterministic re-derivation plus read-back comparison: the verdict the
// store holds must equal the verdict the server derives from the stored
// records, otherwise the completion must fail closed instead of reporting a
// verdict the store does not hold.
func VerifyTaskValidationVerdictReplay(report TaskValidationReviewReport, facts TaskValidationVerdictFacts, stored TaskValidationState) bool {
	return SummarizeTaskValidationEvaluations(EvaluateTaskValidationReview(report, facts)).State == stored
}
