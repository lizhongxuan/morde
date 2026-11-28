package app

import (
	"context"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

// SetTaskModelRecoveryAutoEnabled independently controls automatic recovery.
// It is false by default; the validated manual path remains available.
func (b *CloudDSHToolBroker) SetTaskModelRecoveryAutoEnabled(enabled bool) error {
	if b == nil {
		return domain.NewError(domain.CodeInvalidArgument, "task model recovery broker is required")
	}
	b.taskModelRecoveryAuto = enabled
	return nil
}

// ScheduleTaskModelRecovery persists only a gateway-confirmed failure with
// known zero usage at the exact safe checkpoint and read-only toolset.
func (b *CloudDSHToolBroker) ScheduleTaskModelRecovery(ctx context.Context, claimed task.Task,
	failure TaskModelRecoveryFailure, checkpointThroughSequence int64, checkpointDigest, toolsetDigest []byte) (bool, error) {
	if ctx == nil || b == nil || failure == nil {
		return false, domain.InvalidArgument("task_model_recovery", "recovery evidence is required")
	}
	store, ok := b.store.(TaskModelRecoveryStore)
	if !ok {
		return false, domain.NewError(domain.CodeUnsupported, "task model recovery storage is unavailable")
	}
	command := TaskModelRecoveryCommand{Task: claimed, Failure: failure,
		CheckpointThroughSequence: checkpointThroughSequence, CheckpointDigest: append([]byte(nil), checkpointDigest...),
		ToolsetDigest: append([]byte(nil), toolsetDigest...), Auto: b.taskModelRecoveryAuto}
	if err := ValidateTaskModelRecoveryCommand(command); err != nil {
		return false, nil
	}
	intent, accepted, err := store.ScheduleTaskModelRecovery(ctx, command)
	if err != nil || !accepted {
		return accepted, err
	}
	wantState := task.ModelRecoveryRetryAvailable
	if command.Auto {
		wantState = task.ModelRecoveryScheduled
	}
	evidence := failure.TaskModelRecoveryFailureEvidence()
	if !ValidTaskModelRecoveryIntent(intent) || intent.TaskID != claimed.ID || intent.OwnerID != claimed.OwnerID ||
		intent.InitiatorID != claimed.InitiatorID || intent.TaskRevision != *claimed.CurrentRevision ||
		intent.AuthorizationVersion != claimed.Authorization.Version || intent.FailedCallID != evidence.CallID ||
		intent.FailedThroughSequence != checkpointThroughSequence || intent.CheckpointThroughSequence != checkpointThroughSequence ||
		!equalTaskModelRecoveryDigest(intent.CheckpointDigest, checkpointDigest) ||
		!equalTaskModelRecoveryDigest(intent.ToolsetDigest, toolsetDigest) || intent.State != wantState {
		return false, domain.NewError(domain.CodeInternal, "task recovery storage returned an invalid schedule")
	}
	return true, nil
}

// BeginTaskModelRecoveryAttempt consumes the only scheduled recovery attempt
// before the model gateway can contact the provider.
func (b *CloudDSHToolBroker) BeginTaskModelRecoveryAttempt(ctx context.Context, intent task.ModelRecoveryIntent,
	fence AgentInstanceFence) error {
	if ctx == nil || b == nil || !ValidTaskModelRecoveryIntent(intent) || intent.State != task.ModelRecoveryScheduled ||
		intent.TaskID != fence.TaskID || intent.OwnerID != fence.OwnerID || intent.InitiatorID != fence.InitiatorID ||
		intent.AgentSessionID != fence.AgentSessionID || intent.AuthorizationVersion != fence.AuthorizationVersion ||
		fence.AgentGeneration <= intent.AgentGeneration || fence.AgentInstanceID == intent.AgentInstanceID ||
		intent.EngineVersion != fence.EngineVersion || intent.ProfileVersion != fence.ProfileVersion {
		return domain.NewError(domain.CodeConflict, "task model recovery is no longer scheduled")
	}
	store, ok := b.store.(TaskModelRecoveryStore)
	if !ok {
		return domain.NewError(domain.CodeUnsupported, "task model recovery storage is unavailable")
	}
	return store.BeginTaskModelRecoveryAttempt(ctx, intent, fence)
}
