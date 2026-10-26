package app

import (
	"context"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func (a *Agent) persistVerifiedArtifacts(ctx context.Context, claimed task.Task, step task.Step, batchScope *agentBatchScope, artifacts []AgentVerifiedArtifact) error {
	if len(artifacts) == 0 {
		return nil
	}
	if len(artifacts) > maxAgentVerifiedArtifacts || step.TaskID != claimed.ID || !domain.ValidStepID(step.ID) {
		return domain.NewError(domain.CodeInvalidArgument, "verified artifact metadata is invalid")
	}
	details, err := a.refreshTask(ctx, claimed)
	if err != nil {
		return err
	}
	current := details.Task
	batchItemID := ""
	batchAttempt := int32(0)
	if batchScope != nil {
		item, exists := agentBatchItem(details.BatchItems, batchScope.Item.ID)
		if !exists || item.Status != task.BatchItemRunning || !batchItemMatchesScope(current, details.BatchItems, *batchScope) {
			return domain.NewError(domain.CodeInvalidState, "verified artifact batch attempt changed")
		}
		batchItemID = item.ID
		batchAttempt = item.Attempt
	}
	for _, verified := range artifacts {
		if err := validateAgentVerificationArtifacts(AgentVerificationResult{Status: AgentVerified, Artifacts: []AgentVerifiedArtifact{verified}}); err != nil {
			return domain.NewError(domain.CodeInvalidArgument, "verified artifact metadata is invalid")
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
			if existing.Label != verified.Label || existing.MediaType != verified.MediaType {
				return domain.NewError(domain.CodeInvalidState, "verified artifact metadata changed during recovery")
			}
			continue
		}
		artifactID, err := domain.NewArtifactID()
		if err != nil {
			return err
		}
		_, err = a.store.RecordValidatedArtifact(ctx, current.OwnerID, task.Artifact{
			ID: artifactID, TaskID: current.ID, StepID: step.ID, Label: verified.Label, Path: verified.Path,
			MediaType: verified.MediaType, BatchItemID: batchItemID, BatchAttempt: batchAttempt,
		}, current.Lease.Owner, current.Lease.Version)
		if err != nil {
			return err
		}
	}
	return nil
}
