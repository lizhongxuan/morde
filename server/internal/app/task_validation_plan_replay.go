package app

import (
	"bytes"
	"encoding/json"
	"strings"

	"morde/server/internal/domain"
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
// passed on one unchanged Task Workspace snapshot and both requirement-set
// completeness and final-snapshot identity are independently verified, and
// PENDING otherwise.
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
	// H07. Keep that uncertainty, but still inspect independently bound check
	// receipts so a verified applicable failure can be reported as FAIL.
	unresolvedPlanEvidence := !equalReplayStepIDs(unresolvedApplicability, evidence.PlanStepsWithoutChecks) || len(unresolvedApplicability) != 0
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

	hasUnknown, hasFailure := len(provided) != len(expected) || unresolvedPlanEvidence, false
	var sharedWorkspaceID, sharedViewID, sharedManifest string
	var sharedBindingVersion, sharedCheckpointRevision int64
	var sharedHostID, sharedHostInstanceID, sharedInputManifest string
	var sharedHostGeneration, sharedControlVersion, sharedInputRevision int64
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
			if (fact.ExitCode != 0 || fact.TimedOut) && validSyncDigest(fact.WorkspaceSnapshotSHA256Before) &&
				fact.WorkspaceSnapshotSHA256Before == fact.WorkspaceSnapshotSHA256After {
				hasFailure = true
			} else {
				hasUnknown = true
			}
		case TaskValidationReplayPass:
			if fact.ExitCode != 0 || fact.TimedOut || !validSyncDigest(fact.WorkspaceSnapshotSHA256Before) ||
				fact.WorkspaceSnapshotSHA256Before != fact.WorkspaceSnapshotSHA256After {
				hasUnknown = true
				continue
			}
			if sharedWorkspaceID == "" {
				sharedWorkspaceID, sharedViewID, sharedManifest = fact.WorkspaceID, fact.WorkspaceViewID, fact.WorkspaceSnapshotSHA256Before
				sharedBindingVersion, sharedCheckpointRevision = fact.WorkspaceBindingVersion, fact.WorkspaceCheckpointRevision
				sharedHostID, sharedHostGeneration, sharedHostInstanceID = fact.HostID, fact.HostRuntimeGeneration, fact.HostRuntimeInstanceID
				sharedControlVersion, sharedInputRevision, sharedInputManifest = fact.HostControlVersion, fact.InputRevision, fact.InputManifestSHA256
			} else if sharedWorkspaceID != fact.WorkspaceID || sharedViewID != fact.WorkspaceViewID ||
				sharedManifest != fact.WorkspaceSnapshotSHA256Before || sharedBindingVersion != fact.WorkspaceBindingVersion ||
				sharedCheckpointRevision != fact.WorkspaceCheckpointRevision || sharedHostID != fact.HostID ||
				sharedHostGeneration != fact.HostRuntimeGeneration || sharedHostInstanceID != fact.HostRuntimeInstanceID ||
				sharedControlVersion != fact.HostControlVersion || sharedInputRevision != fact.InputRevision ||
				sharedInputManifest != fact.InputManifestSHA256 {
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
	if !evidence.RequirementSetVerified || !evidence.FinalWorkspaceSnapshotVerified ||
		!TaskValidationFinalWorkspaceSnapshotProofMatchesChecks(evidence.FinalWorkspaceSnapshotProof, evidence.Checks) {
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
		(fact.WorkspaceManifestSHA256Before == "" || validSyncDigest(fact.WorkspaceManifestSHA256Before)) &&
		(fact.WorkspaceManifestSHA256After == "" || validSyncDigest(fact.WorkspaceManifestSHA256After)) &&
		validSyncDigest(fact.WorkspaceSnapshotSHA256Before) && validSyncDigest(fact.WorkspaceSnapshotSHA256After) &&
		domain.ValidHostID(fact.HostID) && fact.HostRuntimeGeneration > 0 && validReplayRuntimeInstanceID(fact.HostRuntimeInstanceID) &&
		fact.HostControlVersion >= 0 && fact.InputRevision >= 0 && fact.WorkspaceCheckpointRevision >= fact.InputRevision &&
		validSyncDigest(fact.InputManifestSHA256)
}

// ValidTaskValidationFinalWorkspaceSnapshotProof validates the proof's basic
// identity and binds it to the exact active review claim. The Store performs
// the authoritative Task/Host/operation comparisons in the completion tx.
func ValidTaskValidationFinalWorkspaceSnapshotProof(claim TaskValidationReviewClaim,
	proof *TaskValidationFinalWorkspaceSnapshotProof) bool {
	return proof != nil && ValidTaskValidationReviewClaim(claim) && ValidTaskValidationReviewClaim(proof.Claim) &&
		sameTaskValidationReviewClaim(claim, proof.Claim) && proof.TaskID == claim.TaskID &&
		proof.TaskRevision == claim.TaskRevision && domain.ValidHostID(proof.HostID) && proof.RuntimeGeneration > 0 &&
		validReplayRuntimeInstanceID(proof.RuntimeInstanceID) && proof.ControlVersion >= 0 &&
		domain.ValidWorkspaceID(proof.WorkspaceID) && proof.BindingVersion > 0 && proof.InputRevision >= 0 &&
		proof.WorkspaceCheckpointRevision >= proof.InputRevision && validSyncDigest(proof.InputManifestSHA256) &&
		taskWorkspaceViewIDPattern.MatchString(proof.ViewID) && validSyncDigest(proof.SnapshotSHA256)
}

// TaskValidationFinalWorkspaceSnapshotProofMatchesReplay accepts only a
// current, independently read snapshot that matches the database-owned Task,
// Host and every typed check receipt. Runtime response identity by itself is
// never enough to verify the final snapshot.
func TaskValidationFinalWorkspaceSnapshotProofMatchesReplay(claim TaskValidationReviewClaim,
	baseline TaskValidationReplaySnapshotBaseline, checks []TaskValidationReplayCheckEvidence,
	proof *TaskValidationFinalWorkspaceSnapshotProof) bool {
	if !ValidTaskValidationFinalWorkspaceSnapshotProof(claim, proof) || len(checks) == 0 ||
		baseline.OwnerID != claim.OwnerID || baseline.InitiatorID != claim.InitiatorID ||
		baseline.ConversationID != claim.ConversationID || baseline.TaskID != claim.TaskID ||
		baseline.TaskRevision != claim.TaskRevision || baseline.HostState != "running" || baseline.HostControlState != "ai" ||
		baseline.HostID != proof.HostID || baseline.HostRuntimeGeneration != proof.RuntimeGeneration ||
		baseline.HostRuntimeInstanceID != proof.RuntimeInstanceID || baseline.HostControlVersion != proof.ControlVersion ||
		baseline.WorkspaceID != proof.WorkspaceID || baseline.InputRevision != proof.InputRevision ||
		baseline.WorkspaceCheckpointRevision != proof.WorkspaceCheckpointRevision ||
		baseline.InputManifestSHA256 != proof.InputManifestSHA256 {
		return false
	}
	return TaskValidationFinalWorkspaceSnapshotProofMatchesChecks(proof, checks)
}

// TaskValidationFinalWorkspaceSnapshotProofMatchesChecks binds every passed
// typed check to one exact runtime view and final manifest digest.
func TaskValidationFinalWorkspaceSnapshotProofMatchesChecks(proof *TaskValidationFinalWorkspaceSnapshotProof,
	checks []TaskValidationReplayCheckEvidence) bool {
	if proof == nil || !ValidTaskValidationFinalWorkspaceSnapshotProof(proof.Claim, proof) || len(checks) == 0 {
		return false
	}
	for _, check := range checks {
		if check.Status != TaskValidationReplayPass || check.TaskID != proof.Claim.TaskID || check.TaskRevision != proof.Claim.TaskRevision ||
			check.HostID != proof.HostID || check.HostRuntimeGeneration != proof.RuntimeGeneration ||
			check.HostRuntimeInstanceID != proof.RuntimeInstanceID || check.HostControlVersion != proof.ControlVersion ||
			check.WorkspaceID != proof.WorkspaceID || check.WorkspaceBindingVersion != proof.BindingVersion ||
			check.InputRevision != proof.InputRevision || check.InputManifestSHA256 != proof.InputManifestSHA256 ||
			check.WorkspaceCheckpointRevision != proof.WorkspaceCheckpointRevision || check.WorkspaceViewID != proof.ViewID ||
			check.WorkspaceSnapshotSHA256Before != check.WorkspaceSnapshotSHA256After ||
			check.WorkspaceSnapshotSHA256After != proof.SnapshotSHA256 {
			return false
		}
	}
	return true
}

func sameTaskValidationReviewClaim(left, right TaskValidationReviewClaim) bool {
	return left.OwnerID == right.OwnerID && left.InitiatorID == right.InitiatorID &&
		left.ConversationID == right.ConversationID && left.TaskID == right.TaskID &&
		left.TaskRevision == right.TaskRevision && left.ResultEventSequence == right.ResultEventSequence &&
		bytes.Equal(left.ResultSHA256, right.ResultSHA256) && left.ClaimGeneration == right.ClaimGeneration &&
		left.LeaseOwner == right.LeaseOwner && left.LeaseExpiresAt.Equal(right.LeaseExpiresAt)
}

func validReplayRuntimeInstanceID(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}
