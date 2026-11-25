package app

import (
	"bytes"

	"morde/server/internal/domain/task"
)

// E09 independent verifier: server-attested facts for the seven hard
// checks. The review model runs in a separate, tool-less DSH session (the
// "different agent"), but a model claim alone cannot resolve the checks
// that need execution facts: before E09, loadTaskValidationVerdictFacts
// left IrreversibleAction, RepeatedError and ReplayVerified unknown for
// every task that executed anything, so the verdict was pending for every
// executed task (see TestEvaluateTaskValidationReviewDerivesNAButKeepsUnknownExecutionFactsPending).
//
// The functions in this file are pure derivations over the durable ledger
// (steps, pending requests, MCP operations, plan). They run on the review
// completion path, a different code path from the execution broker that
// recorded the steps, so the verifier independently re-derives the
// evidence instead of trusting the broker's gates or the model's claim:
//
//   - approval coverage (H05): every executed action in a per-call
//     approval category must match an approved pending request (approval
//     ID/version, task revision, call identity, success ledger), per
//     M06-03. Unknown action classification stays pending, never PASS.
//   - repeated errors (H06): the same tool call failing twice or more is
//     a repeated error; it is verified only when the same call succeeds
//     after its last failure (the correction is recorded in the ledger
//     and holds).
//   - replay (H07): the E07 test gate can re-derive whether a failing
//     reproduction test and full suite passed, but those step stamps alone
//     do not establish the required plan-revision, immutable Workspace
//     version, and coverage binding. The PostgreSQL completion path therefore
//     keeps fix-task replay pending until those identities are available. For
//     other tasks, the read-back compare checks whether evidence claims remain
//     covered by currently verified content.
//
// No new agent session is created: M06-04 keeps the existing tool-less
// task_validation_review DSH session, and the verifier does not re-execute
// anything (the review session cannot run tools by protocol design, and
// the server has no other path to the cloud computer). "Re-run the tests"
// therefore means the independent re-derivation of the recorded test
// runs, plus the read-back compare of the artifacts.

// TaskToolRequiresPerCallApproval reports whether a tool call, identified
// only by its step tool name, belongs to one of the per-call approval
// categories of the Step 05 authorization matrix (the same four trigger
// rules E05 records on PendingRequest): host sensitive tools, external
// MCP tools, workspace live writes, and media publishes.
//
// The classification is deliberately conservative and argument-free: the
// review-time ledger carries no tool arguments, so a press-key call (whose
// risk depends on the key) counts as approval-requiring, and any name
// outside the known read-only / task-scope sets counts as
// approval-requiring too — matching runtimeToolRisk, whose default tier is
// sensitive. Read-only tools (host reads, workspace reads, web reads,
// history, skill read) and Harness-internal writes governed by task scope
// (plan updates, goal/schedule records, personal-object updates) do not
// require per-call approval and so do not count as irreversible actions:
// H05 verifies the coverage of actions that required per-call approval,
// nothing more. A goal or schedule write is a persistent user-data change,
// but it is not per-call-approval-gated at dispatch, so the verifier
// cannot demand an approval row for it.
func TaskToolRequiresPerCallApproval(toolName string) bool {
	switch toolName {
	case runtimeToolFileRead, runtimeToolFileList, runtimeToolFileFind,
		runtimeToolBrowserSnapshot, runtimeToolBrowserScreenshot,
		runtimeToolDesktopScreenshot, runtimeToolWindowList,
		runtimeToolBrowserObserveField,
		runtimeToolFileWrite, runtimeToolBrowserOpenTab,
		runtimeToolBrowserNavigate, runtimeToolBrowserUserHandoff,
		runtimeToolBrowserClick, runtimeToolWindowActivate,
		cloudDSHWorkspaceListToolName, cloudDSHWorkspaceReadToolName,
		cloudDSHWorkspaceSearchToolName,
		cloudDSHGoalListToolName, cloudDSHGoalCreateToolName,
		cloudDSHGoalUpdateToolName, cloudDSHGoalDeleteToolName,
		cloudDSHScheduleListToolName, cloudDSHScheduleCreateToolName,
		cloudDSHScheduleUpdateToolName, cloudDSHScheduleActionToolName,
		cloudDSHScheduleRunsToolName,
		cloudDSHConversationHistorySearchToolName,
		cloudDSHRecentHistorySummaryToolName,
		cloudDSHCurrentHistorySearchToolName,
		cloudDSHCurrentMessageReadToolName,
		cloudDSHCurrentMessageSearchToolName,
		cloudDSHImpactMapToolName,
		cloudDSHWebSearchToolName, cloudDSHWebReadToolName,
		cloudDSHSkillReadToolName, cloudDSHTaskAttachmentReadToolName,
		cloudDSHTaskPlanToolName, cloudDSHPersonalUpdateToolName:
		return false
	case "morde_capability_probe":
		return false
	default:
		return true
	}
}

// TaskApprovalAction is one executed action that required per-call
// approval, as read back from the ledger.
type TaskApprovalAction struct {
	CallID        string
	ToolName      string
	RequestDigest []byte
	TaskRevision  *int64
}

// TaskApprovalRecord is one approved pending request, as read back from
// the ledger. Only rows that were actually approved (status approved,
// decision approve, answered by a user, not expired) are passed in.
type TaskApprovalRecord struct {
	CallID       string
	ToolName     string
	RequestHash  []byte
	TaskRevision *int64
}

// EvaluateTaskApprovalCoverage decides the H05 facts from the executed
// approval-requiring actions and the approved pending requests. An action
// matches a record when call ID, tool name, request digest and task
// revision all agree: the digest binds the exact tool arguments (the same
// agentOperationHash the broker stored on the pending request), the
// revision binds the task state at execution. The runtime fence
// (generation / instance / control version) is not in the step ledger, so
// it is not re-verified here; it was enforced at dispatch by
// requireApprovedTaskOperationBinding. When unclassifiable is true (a
// succeeded step carries no tool name, so the ledger cannot say what it
// did), the fact stays unknown and H05 stays pending.
func EvaluateTaskApprovalCoverage(actions []TaskApprovalAction, approvals []TaskApprovalRecord, unclassifiable bool) (TaskValidationFactState, bool) {
	if unclassifiable {
		return TaskValidationFactUnknown, false
	}
	if len(actions) == 0 {
		return TaskValidationFactAbsent, false
	}
	for _, action := range actions {
		if !taskApprovalActionMatched(action, approvals) {
			return TaskValidationFactPresent, false
		}
	}
	return TaskValidationFactPresent, true
}

func taskApprovalActionMatched(action TaskApprovalAction, approvals []TaskApprovalRecord) bool {
	if action.CallID == "" || action.ToolName == "" || len(action.RequestDigest) != 32 || action.TaskRevision == nil {
		return false
	}
	for _, approval := range approvals {
		if approval.CallID != action.CallID || approval.ToolName != action.ToolName ||
			len(approval.RequestHash) != 32 || !bytes.Equal(approval.RequestHash, action.RequestDigest) ||
			approval.TaskRevision == nil || *approval.TaskRevision != *action.TaskRevision {
			continue
		}
		return true
	}
	return false
}

// EvaluateRepeatedErrorCorrections decides the H06 facts from the step
// ledger. A repeated error is the same tool call (same request digest,
// which binds tool name and arguments) failing at least twice. It is
// verified only when that same call succeeds after its last failure: the
// correction is then recorded in the ledger as a passing re-run that
// holds. A pass followed by another failure of the same call is not a
// verified correction. Steps without a digest cannot be grouped, so they
// are ignored here.
func EvaluateRepeatedErrorCorrections(steps []task.Step) (TaskValidationFactState, bool) {
	failedCounts := make(map[string]int)
	lastFailedSequence := make(map[string]int64)
	for _, step := range steps {
		if step.Status != task.StepFailed || len(step.RequestDigest) == 0 {
			continue
		}
		digest := string(step.RequestDigest)
		failedCounts[digest]++
		if sequence, ok := lastFailedSequence[digest]; !ok || step.Sequence > sequence {
			lastFailedSequence[digest] = step.Sequence
		}
	}
	var repeated []string
	for digest, count := range failedCounts {
		if count >= 2 {
			repeated = append(repeated, digest)
		}
	}
	if len(repeated) == 0 {
		return TaskValidationFactAbsent, false
	}
	for _, digest := range repeated {
		corrected := false
		for _, step := range steps {
			if step.Status == task.StepSucceeded && string(step.RequestDigest) == digest &&
				step.Sequence > lastFailedSequence[digest] {
				corrected = true
				break
			}
		}
		if !corrected {
			return TaskValidationFactPresent, false
		}
	}
	return TaskValidationFactPresent, true
}

// EvaluateTaskValidationReplay decides the H07 replay fact. For fix tasks
// the replay is the test re-run: the E07 gate re-derived from the steps
// (the failing reproduction test passed after the fix, and the full suite
// passed). For other tasks the replay is the read-back compare: the
// evidence claims are still covered by currently verified content. A fix
// task whose gate does not hold stays unverified even when the model
// claims the tests passed; a hostless fix task has no shell runs to
// re-derive from, so it stays unverified too (the same known limitation
// as the E07 hostless exemption).
func EvaluateTaskValidationReplay(hasFixPlan bool, gate TestGateResult, evidenceContentVerified bool) bool {
	if hasFixPlan {
		return gate.NewTestPassed && gate.FullSuitePassed
	}
	return evidenceContentVerified
}

// TaskValidationReplayEvidence is the server-attested test re-run, sent to
// the review model inside the review packet so it can judge H07 on ledger
// fact instead of on shell exit codes alone (which the review prompt
// correctly says cannot prove test assertions). Sequences are 0 when that
// half has no proving step.
type TaskValidationReplayEvidence struct {
	NewTestPassed           bool  `json:"new_test_passed"`
	FullSuitePassed         bool  `json:"full_suite_passed"`
	NewTestFailedSequence   int64 `json:"new_test_failed_sequence"`
	NewTestPassedSequence   int64 `json:"new_test_passed_sequence"`
	FullSuitePassedSequence int64 `json:"full_suite_passed_sequence"`
}

// ReplayEvidenceForSteps builds the replay section of the review packet
// for a fix task, or nil for any other task (its read-back is already in
// the packet as artifact content proofs). The section is included even
// when the gate fails, so the model can see the re-run did not happen.
func ReplayEvidenceForSteps(steps []task.Step, currentRevision *int64, hasFixPlan bool) *TaskValidationReplayEvidence {
	if !hasFixPlan {
		return nil
	}
	gate := EvaluateTestGate(steps, currentRevision)
	return &TaskValidationReplayEvidence{
		NewTestPassed:           gate.NewTestPassed,
		FullSuitePassed:         gate.FullSuitePassed,
		NewTestFailedSequence:   gate.NewTestFailedSequence,
		NewTestPassedSequence:   gate.NewTestPassedSequence,
		FullSuitePassedSequence: gate.FullSuitePassedSequence,
	}
}
