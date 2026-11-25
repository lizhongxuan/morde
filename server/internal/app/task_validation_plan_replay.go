package app

import (
	"bytes"
	"encoding/json"

	"morde/server/internal/domain/task"
)

// PendingTaskValidationReplayEvidence creates an explicit pending packet for
// a fix plan before durable check receipts are available. Missing check
// metadata is visible as unknown; it is never treated as an empty check set.
func PendingTaskValidationReplayEvidence(plan *task.Plan) *TaskValidationReplayEvidence {
	if plan == nil || plan.FixPlan == nil {
		return nil
	}
	evidence := &TaskValidationReplayEvidence{State: TaskValidationReplayPending, PlanRevision: plan.Revision, Checks: make([]TaskValidationReplayCheckEvidence, 0)}
	for _, step := range plan.Steps {
		if step.VerificationCheck == nil {
			if step.VerificationApplicability != task.PlanVerificationApplicabilityNotApplicable {
				evidence.PlanStepsWithoutChecks = append(evidence.PlanStepsWithoutChecks, step.ID)
			}
			continue
		}
		check := TaskValidationReplayCheckEvidence{CheckID: step.ID, Status: TaskValidationReplayPending,
			Kind: step.VerificationCheck.Kind, Basis: step.VerificationCheck.Basis,
			CoveragePaths:             append([]string(nil), step.VerificationCheck.CoveragePaths...),
			RequirementSourceVerified: step.VerificationCheck.Basis == task.PlanVerificationCheckRiskSelected}
		if step.VerificationCheck.RequirementSource != nil {
			source := *step.VerificationCheck.RequirementSource
			if source.WorkspaceRevision != nil {
				revision := *source.WorkspaceRevision
				source.WorkspaceRevision = &revision
			}
			check.RequirementSource = &source
		}
		evidence.Checks = append(evidence.Checks, check)
	}
	return evidence
}

// EvaluateTaskValidationPlanReplay derives H07 from a current plan and the
// latest durable operation fact for each typed check. It returns FAIL for a
// verified applicable failing check, PASS only when every declared check
// passed on one unchanged Task Workspace snapshot, and PENDING otherwise.
func EvaluateTaskValidationPlanReplay(plan task.Plan, evidence *TaskValidationReplayEvidence) TaskValidationReplayState {
	if plan.Validate() != nil || plan.FixPlan == nil || evidence == nil || evidence.PlanRevision != plan.Revision {
		return TaskValidationReplayPending
	}
	type expectedCheck struct {
		stepID string
		check  task.PlanVerificationCheck
	}
	expected := make([]expectedCheck, 0, len(plan.Steps))
	expectedByID := make(map[string]task.PlanVerificationCheck, len(plan.Steps))
	unresolvedApplicability := make([]string, 0)
	for _, step := range plan.Steps {
		if step.VerificationCheck == nil {
			if step.VerificationApplicability != task.PlanVerificationApplicabilityNotApplicable {
				unresolvedApplicability = append(unresolvedApplicability, step.ID)
			}
			continue
		}
		if step.VerificationApplicability == task.PlanVerificationApplicabilityNotApplicable {
			return TaskValidationReplayPending
		}
		expected = append(expected, expectedCheck{stepID: step.ID, check: *step.VerificationCheck})
		expectedByID[step.ID] = *step.VerificationCheck
	}
	if len(expected) == 0 {
		return TaskValidationReplayPending
	}
	// An implementation-only step must be explicitly marked not_applicable.
	// Missing applicability remains unknown for legacy plans and cannot close
	// H07. The evidence projection must name the same unresolved steps.
	if !equalReplayStepIDs(unresolvedApplicability, evidence.PlanStepsWithoutChecks) || len(unresolvedApplicability) != 0 {
		return TaskValidationReplayPending
	}
	provided := make(map[string]TaskValidationReplayCheckEvidence, len(evidence.Checks))
	for _, check := range evidence.Checks {
		if _, duplicate := provided[check.CheckID]; duplicate {
			return TaskValidationReplayPending
		}
		if _, exists := expectedByID[check.CheckID]; !exists {
			return TaskValidationReplayPending
		}
		provided[check.CheckID] = check
	}

	hasUnknown, hasFailure := len(provided) != len(expected), false
	var sharedWorkspaceID, sharedViewID, sharedManifest string
	var sharedBindingVersion, sharedCheckpointRevision int64
	for _, item := range expected {
		fact, exists := provided[item.stepID]
		if !exists || !validTaskValidationReplayCheckIdentity(plan, item.stepID, item.check, fact) {
			hasUnknown = true
			continue
		}
		if item.check.Basis == task.PlanVerificationCheckRequired && !fact.RequirementSourceVerified {
			hasUnknown = true
			continue
		}
		switch fact.Status {
		case TaskValidationReplayFail:
			if (fact.ExitCode != 0 || fact.TimedOut) && fact.WorkspaceManifestSHA256Before == fact.WorkspaceManifestSHA256After {
				hasFailure = true
			} else {
				hasUnknown = true
			}
		case TaskValidationReplayPass:
			if fact.ExitCode != 0 || fact.TimedOut || fact.WorkspaceManifestSHA256Before != fact.WorkspaceManifestSHA256After {
				hasUnknown = true
				continue
			}
			if sharedWorkspaceID == "" {
				sharedWorkspaceID, sharedViewID, sharedManifest = fact.WorkspaceID, fact.WorkspaceViewID, fact.WorkspaceManifestSHA256Before
				sharedBindingVersion, sharedCheckpointRevision = fact.WorkspaceBindingVersion, fact.WorkspaceCheckpointRevision
			} else if sharedWorkspaceID != fact.WorkspaceID || sharedViewID != fact.WorkspaceViewID ||
				sharedManifest != fact.WorkspaceManifestSHA256Before || sharedBindingVersion != fact.WorkspaceBindingVersion ||
				sharedCheckpointRevision != fact.WorkspaceCheckpointRevision {
				hasUnknown = true
			}
		default:
			hasUnknown = true
		}
	}
	if hasFailure {
		return TaskValidationReplayFail
	}
	if hasUnknown {
		return TaskValidationReplayPending
	}
	// Matching receipts prove only that the plan's declared checks ran. They
	// do not prove that the plan captured every explicit user/project
	// requirement (for example, a requested full suite omitted by the plan).
	if !evidence.RequirementSetVerified || !evidence.FinalWorkspaceSnapshotVerified {
		return TaskValidationReplayPending
	}
	return TaskValidationReplayPass
}

func equalReplayStepIDs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validTaskValidationReplayCheckIdentity(plan task.Plan, checkID string, check task.PlanVerificationCheck,
	fact TaskValidationReplayCheckEvidence) bool {
	commandDigest, checkDigest, coverageDigest, ok := taskValidationPlanCheckDigests(check)
	if !ok {
		return false
	}
	expectedCheckBytes, expectedErr := json.Marshal(check)
	providedCheck := task.PlanVerificationCheck{Kind: fact.Kind, Basis: fact.Basis, Command: check.Command,
		CoveragePaths: fact.CoveragePaths, RequirementSource: fact.RequirementSource}
	providedCheckBytes, providedErr := json.Marshal(providedCheck)
	return expectedErr == nil && providedErr == nil && bytes.Equal(expectedCheckBytes, providedCheckBytes) &&
		fact.CheckID == checkID && fact.TaskRevision == plan.TaskRevision && fact.PlanRevision == plan.Revision &&
		fact.RunSequence > 0 && fact.TaskID != "" && fact.OperationID != "" && fact.CallID != "" &&
		fact.CommandSHA256 == commandDigest && fact.CheckSHA256 == checkDigest && fact.CoverageSHA256 == coverageDigest &&
		fact.RequestSHA256 != "" && validSyncDigest(fact.RequestSHA256) &&
		fact.WorkspaceID != "" && validRuntimeWorkspaceID(fact.WorkspaceID) && fact.WorkspaceBindingVersion > 0 &&
		fact.WorkspaceCheckpointRevision >= 0 && taskWorkspaceViewIDPattern.MatchString(fact.WorkspaceViewID) &&
		validSyncDigest(fact.WorkspaceManifestSHA256Before) && validSyncDigest(fact.WorkspaceManifestSHA256After)
}
