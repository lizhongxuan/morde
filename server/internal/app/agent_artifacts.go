package app

import (
	"context"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func (a *Agent) persistVerifiedArtifacts(ctx context.Context, claimed task.Task, step task.Step, batchScope *agentBatchScope, artifacts []AgentVerifiedArtifact) ([]task.Artifact, error) {
	if len(artifacts) == 0 {
		return nil, nil
	}
	if len(artifacts) > maxAgentVerifiedArtifacts || step.TaskID != claimed.ID || !domain.ValidStepID(step.ID) {
		return nil, domain.NewError(domain.CodeInvalidArgument, "verified artifact metadata is invalid")
	}
	details, err := a.refreshTask(ctx, claimed)
	if err != nil {
		return nil, err
	}
	current := details.Task
	batchItemID := ""
	batchAttempt := int32(0)
	prepared := make([]task.Artifact, 0, len(artifacts))
	if batchScope != nil {
		item, exists := agentBatchItem(details.BatchItems, batchScope.Item.ID)
		if !exists || item.Status != task.BatchItemRunning || !batchItemMatchesScope(current, details.BatchItems, *batchScope) {
			return nil, domain.NewError(domain.CodeInvalidState, "verified artifact batch attempt changed")
		}
		batchItemID = item.ID
		batchAttempt = item.Attempt
	}
	for _, verified := range artifacts {
		if err := validateAgentVerificationArtifacts(AgentVerificationResult{Status: AgentVerified, Artifacts: []AgentVerifiedArtifact{verified}}); err != nil {
			return nil, domain.NewError(domain.CodeInvalidArgument, "verified artifact metadata is invalid")
		}
		if verified.WorkspaceID != current.WorkspaceID {
			return nil, domain.NewError(domain.CodeInvalidState, "verified artifact is outside the task workspace")
		}
		var existing *task.Artifact
		for index := range details.Artifacts {
			candidate := &details.Artifacts[index]
			if candidate.TaskID == current.ID && candidate.BatchItemID == batchItemID && candidate.BatchAttempt == batchAttempt && candidate.Path == verified.Path {
				existing = candidate
				break
			}
		}
		if existing != nil {
			if existing.Label != verified.Label || existing.MediaType != verified.MediaType || existing.WorkspaceID != verified.WorkspaceID ||
				existing.Revision != verified.Revision || existing.SizeBytes != verified.SizeBytes || existing.SHA256 != verified.SHA256 {
				return nil, domain.NewError(domain.CodeInvalidState, "verified artifact metadata changed during recovery")
			}
			if batchScope == nil {
				prepared = append(prepared, *existing)
			}
			continue
		}
		artifactID, err := domain.NewArtifactID()
		if err != nil {
			return nil, err
		}
		artifact := task.Artifact{
			ID: artifactID, TaskID: current.ID, StepID: step.ID, Label: verified.Label, Path: verified.Path,
			WorkspaceID: verified.WorkspaceID, Revision: verified.Revision, SizeBytes: verified.SizeBytes, SHA256: verified.SHA256,
			MediaType: verified.MediaType, BatchItemID: batchItemID, BatchAttempt: batchAttempt,
		}
		if batchScope != nil {
			if _, err = a.store.RecordValidatedArtifact(ctx, current.OwnerID, artifact, current.Lease.Owner, current.Lease.Version); err != nil {
				return nil, err
			}
		} else {
			prepared = append(prepared, artifact)
		}
	}
	return prepared, nil
}
