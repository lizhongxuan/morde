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
//   - replay (H07): fix-task checks are re-derived from current plan revisions,
//     typed check identities, exact operation bindings, verified requirement
//     sources and Runtime Workspace manifests. A confirmed applicable failure
//     is FAIL; incomplete or mismatched facts stay pending. Other tasks use the
//     verified read-back compare.
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

// TaskValidationReplayEvidence is the server-attested set of typed plan
// checks sent to the review model. Legacy heuristic fields remain local to
// older unit helpers and are never serialized into the review prompt.
type TaskValidationReplayEvidence struct {
	State TaskValidationReplayState `json:"state"`
	// RequirementSetVerified means the current plan's canonical candidate
	// manifest was explicitly confirmed by an authenticated user, every
	// obligation source is still verified, and the stored plan/event/auth fences
	// match. It binds the confirmation to that exact candidate; it does not
	// independently prove that the candidate is complete or semantically correct.
	RequirementSetVerified bool `json:"requirement_set_verified"`
	// FinalWorkspaceSnapshotVerified is set only when the delivered Task
	// Workspace still matches the snapshot on which all checks ran.
	FinalWorkspaceSnapshotVerified bool `json:"final_workspace_snapshot_verified"`
	// FinalWorkspaceSnapshotProof is internal completion input. It is never
	// serialized to the reviewer model; the Store must compare it with the
	// current Task claim and the durable check-operation fences.
	FinalWorkspaceSnapshotProof *TaskValidationFinalWorkspaceSnapshotProof `json:"-"`
	PlanRevision                int64                                      `json:"plan_revision"`
	Checks                      []TaskValidationReplayCheckEvidence        `json:"checks"`
	PlanStepsWithoutChecks      []string                                   `json:"plan_steps_without_checks,omitempty"`
	NewTestPassed               bool                                       `json:"-"`
	FullSuitePassed             bool                                       `json:"-"`
	NewTestFailedSequence       int64                                      `json:"-"`
	NewTestPassedSequence       int64                                      `json:"-"`
	FullSuitePassedSequence     int64                                      `json:"-"`
}

type TaskValidationReplayState string

const (
	TaskValidationReplayPending TaskValidationReplayState = "pending"
	TaskValidationReplayPass    TaskValidationReplayState = "pass"
	TaskValidationReplayFail    TaskValidationReplayState = "fail"
)

// TaskValidationReplayCheckEvidence is a bounded, server-read view of the
// latest real operation bound to one typed plan check. It omits shell output
// and file contents while preserving the identities needed to assess H07.
type TaskValidationReplayCheckEvidence struct {
	CheckID                   string                                  `json:"check_id"`
	TaskID                    string                                  `json:"task_id,omitempty"`
	Kind                      task.PlanVerificationCheckKind          `json:"kind,omitempty"`
	Basis                     task.PlanVerificationCheckBasis         `json:"basis,omitempty"`
	ScopeMode                 task.PlanVerificationScopeMode          `json:"scope_mode,omitempty"`
	CoveragePaths             []string                                `json:"coverage_paths,omitempty"`
	Selectors                 []string                                `json:"selectors,omitempty"`
	RequirementIDs            []task.PlanVerificationRequirementID    `json:"requirement_ids,omitempty"`
	RequirementSource         *task.PlanVerificationRequirementSource `json:"requirement_source,omitempty"`
	RequirementSourceVerified bool                                    `json:"requirement_source_verified"`
	// ActualCoverageVerified is set only after Store validates a trusted,
	// structured Runtime runner receipt against the exact plan command, scope,
	// discovered/executed inventories, operation fence and stable snapshot.
	// Missing, unsupported or inconsistent receipts leave this false even when
	// the shell command exits successfully.
	ActualCoverageVerified        bool                      `json:"actual_coverage_verified"`
	Status                        TaskValidationReplayState `json:"status"`
	TaskRevision                  int64                     `json:"task_revision,omitempty"`
	PlanRevision                  int64                     `json:"plan_revision,omitempty"`
	OperationID                   string                    `json:"operation_id,omitempty"`
	CallID                        string                    `json:"call_id,omitempty"`
	RequestSHA256                 string                    `json:"request_sha256,omitempty"`
	CommandSHA256                 string                    `json:"command_sha256,omitempty"`
	CheckSHA256                   string                    `json:"check_sha256,omitempty"`
	CoverageSHA256                string                    `json:"coverage_sha256,omitempty"`
	RunSequence                   int64                     `json:"run_sequence,omitempty"`
	ExitCode                      int                       `json:"exit_code,omitempty"`
	TimedOut                      bool                      `json:"timed_out,omitempty"`
	WorkspaceID                   string                    `json:"workspace_id,omitempty"`
	WorkspaceBindingVersion       int64                     `json:"workspace_binding_version,omitempty"`
	WorkspaceCheckpointRevision   int64                     `json:"workspace_checkpoint_revision,omitempty"`
	WorkspaceViewID               string                    `json:"workspace_view_id,omitempty"`
	WorkspaceManifestSHA256Before string                    `json:"workspace_manifest_sha256_before,omitempty"`
	WorkspaceManifestSHA256After  string                    `json:"workspace_manifest_sha256_after,omitempty"`
	WorkspaceSnapshotSHA256Before string                    `json:"-"`
	WorkspaceSnapshotSHA256After  string                    `json:"-"`
	// Runtime and input identity fields are server-only ledger baselines. The
	// reviewer model does not need these values to assess the declared checks.
	HostID                string `json:"-"`
	HostRuntimeGeneration int64  `json:"-"`
	HostRuntimeInstanceID string `json:"-"`
	HostControlVersion    int64  `json:"-"`
	InputRevision         int64  `json:"-"`
	InputManifestSHA256   string `json:"-"`
}

// TaskValidationFinalWorkspaceSnapshotProof carries a review-time read of the
// Runtime's current Task Workspace manifest. Its identity fields are claims
// from the trusted server-side reader; Store completion still checks each one
// against current Task and operation rows before accepting the proof.
type TaskValidationFinalWorkspaceSnapshotProof struct {
	Claim                       TaskValidationReviewClaim
	TaskID                      string
	TaskRevision                int64
	HostID                      string
	RuntimeGeneration           int64
	RuntimeInstanceID           string
	ControlVersion              int64
	WorkspaceID                 string
	BindingVersion              int64
	InputRevision               int64
	WorkspaceCheckpointRevision int64
	InputManifestSHA256         string
	ViewID                      string
	SnapshotSHA256              string
}

// TaskValidationReplaySnapshotBaseline contains the database-owned Task and
// current Host identity used to validate a runtime snapshot proof.
type TaskValidationReplaySnapshotBaseline struct {
	OwnerID                     string
	InitiatorID                 string
	ConversationID              string
	TaskID                      string
	TaskRevision                int64
	HostID                      string
	HostRuntimeGeneration       int64
	HostRuntimeInstanceID       string
	HostControlVersion          int64
	HostState                   string
	HostControlState            string
	WorkspaceID                 string
	InputRevision               int64
	WorkspaceCheckpointRevision int64
	InputManifestSHA256         string
}

// ReplayEvidenceForSteps retains the pre-H07 heuristic projection for tests
// that exercise E07's legacy stamps. The review worker no longer sends it as
// H07 evidence; fix-task verdicts use durable typed plan-check receipts.
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
