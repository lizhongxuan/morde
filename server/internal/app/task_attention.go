package app

import (
	"context"
	"time"

	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type TaskAttentionKind string

const (
	TaskAttentionNeedsUserAction  TaskAttentionKind = "needs_user_action"
	TaskAttentionRecovering       TaskAttentionKind = "recovering"
	TaskAttentionPausedRetryable  TaskAttentionKind = "paused_retryable"
	TaskAttentionInterrupted      TaskAttentionKind = "interrupted"
	TaskAttentionResultUnverified TaskAttentionKind = "result_unverified"
)

type TaskAttentionAction string

const (
	TaskAttentionActionApprove TaskAttentionAction = "approve"
	TaskAttentionActionClarify TaskAttentionAction = "clarify"
	TaskAttentionActionRetry   TaskAttentionAction = "retry"
	TaskAttentionActionDetails TaskAttentionAction = "view_details"
)

// TaskAttention is a safe presentation state derived from the same authorized
// Task snapshot returned to the caller. Action is nil unless the current
// caller can perform a persisted, versioned operation for this Task revision.
type TaskAttention struct {
	Kind          TaskAttentionKind    `json:"kind"`
	Reason        string               `json:"reason"`
	Action        *TaskAttentionAction `json:"action"`
	EventSequence int64                `json:"event_sequence"`
}

type taskModelRecoveryIntentReader interface {
	GetTaskModelRecovery(context.Context, string, string, int64) (task.ModelRecoveryIntent, bool, error)
}

func ValidTaskAttention(value *TaskAttention, eventSequence int64) bool {
	if value == nil {
		return eventSequence >= 0
	}
	if eventSequence < 0 || value.EventSequence != eventSequence || value.Reason == "" {
		return false
	}
	switch value.Kind {
	case TaskAttentionNeedsUserAction:
		return (value.Reason == "approval_required" && value.Action != nil && *value.Action == TaskAttentionActionApprove) ||
			(value.Reason == "clarification_required" && value.Action != nil && *value.Action == TaskAttentionActionClarify)
	case TaskAttentionRecovering:
		return (value.Reason == "recovery_scheduled" || value.Reason == TaskModelBudgetCapacityWaitReason) && value.Action == nil
	case TaskAttentionPausedRetryable:
		return value.Reason == "retry_available" && (value.Action == nil || *value.Action == TaskAttentionActionRetry)
	case TaskAttentionInterrupted:
		return (value.Reason == "execution_failed" || value.Reason == "unknown" ||
			value.Reason == TaskModelBudgetCapacityExpiredReason || value.Reason == TaskModelBudgetCapacityExhaustedReason) && value.Action == nil
	case TaskAttentionResultUnverified:
		return value.Reason == "outcome_unknown" && value.Action == nil
	default:
		return false
	}
}

func (s *TaskService) taskAttention(ctx context.Context, principal identity.Principal, details TaskDetails) *TaskAttention {
	value := details.Task
	if value.Status == task.StatusCancelled {
		return nil
	}
	if attention := s.taskModelRecoveryAttention(ctx, principal, value); attention != nil {
		return attention
	}
	budgetPaused := value.Status == task.StatusPaused && (value.WaitingReason == TaskModelBudgetCapacityExpiredReason ||
		value.WaitingReason == TaskModelBudgetCapacityExhaustedReason)
	if value.Status != task.StatusWaitingUser && value.Status != task.StatusFailed && !budgetPaused {
		return nil
	}
	attention := &TaskAttention{EventSequence: value.EventSequence}
	if value.Status == task.StatusWaitingUser && value.WaitingReason == TaskModelBudgetCapacityWaitReason {
		attention.Kind, attention.Reason = TaskAttentionRecovering, TaskModelBudgetCapacityWaitReason
		return attention
	}
	if budgetPaused {
		attention.Kind, attention.Reason = TaskAttentionInterrupted, value.WaitingReason
		return attention
	}
	if value.Status == task.StatusWaitingUser && value.WaitingReason == "approval_required" &&
		currentPendingApproval(details) && s.taskControlAvailable(ctx, principal, value.HostID) {
		action := TaskAttentionActionApprove
		attention.Kind, attention.Reason, attention.Action = TaskAttentionNeedsUserAction, "approval_required", &action
		return attention
	}
	if value.Status == task.StatusWaitingUser && value.WaitingReason == "clarification_required" &&
		currentPendingClarification(details) && s.taskControlAvailable(ctx, principal, value.HostID) {
		action := TaskAttentionActionClarify
		attention.Kind, attention.Reason, attention.Action = TaskAttentionNeedsUserAction, "clarification_required", &action
		return attention
	}
	if taskHasUnverifiedOutcome(details) || value.Status == task.StatusWaitingUser &&
		(value.WaitingReason == "dsh_recovery_unresolved" || value.WaitingReason == "unknown_outcome" ||
			value.WaitingReason == "reconciliation_required" || value.WaitingReason == "tool_result_unknown" ||
			value.WaitingReason == "mcp_result_unknown" || value.WaitingReason == "hub_action_unknown" ||
			value.WaitingReason == "target_result_unknown" || value.WaitingReason == "workspace_publish_pending") {
		attention.Kind, attention.Reason = TaskAttentionResultUnverified, "outcome_unknown"
		return attention
	}
	if value.Status == task.StatusFailed {
		attention.Kind, attention.Reason = TaskAttentionInterrupted, "execution_failed"
		return attention
	}
	if value.Status == task.StatusCancelled {
		return nil
	}
	attention.Kind, attention.Reason = TaskAttentionInterrupted, "unknown"
	return attention
}

func (s *TaskService) taskModelRecoveryAttention(ctx context.Context, principal identity.Principal, value task.Task) *TaskAttention {
	if value.CurrentRevision == nil || value.DesiredRevision == nil || *value.CurrentRevision != *value.DesiredRevision {
		return nil
	}
	store, ok := s.store.(taskModelRecoveryIntentReader)
	if !ok {
		return nil
	}
	intent, found, err := store.GetTaskModelRecovery(ctx, value.ID, value.OwnerID, *value.CurrentRevision)
	if err != nil || !found || !ValidTaskModelRecoveryIntent(intent) || intent.TaskID != value.ID ||
		intent.OwnerID != value.OwnerID || intent.InitiatorID != value.InitiatorID || intent.TaskRevision != *value.CurrentRevision ||
		intent.AuthorizationVersion != value.Authorization.Version {
		return nil
	}
	attention := &TaskAttention{EventSequence: value.EventSequence}
	switch intent.State {
	case task.ModelRecoveryScheduled, task.ModelRecoveryAttempting:
		if value.Status != task.StatusQueued && value.Status != task.StatusRunning {
			return nil
		}
		attention.Kind, attention.Reason = TaskAttentionRecovering, "recovery_scheduled"
		return attention
	case task.ModelRecoveryRetryAvailable:
		if value.Status != task.StatusWaitingUser || value.EventSequence != intent.EventSequence {
			return nil
		}
		attention.Kind, attention.Reason = TaskAttentionPausedRetryable, "retry_available"
		if principal.UserID == intent.InitiatorID && s.taskControlAvailable(ctx, principal, value.HostID) {
			action := TaskAttentionActionRetry
			attention.Action = &action
		}
		return attention
	default:
		return nil
	}
}

func (s *TaskService) taskControlAvailable(ctx context.Context, principal identity.Principal, hostID string) bool {
	_, err := s.authorizeTask(ctx, principal, hostID, taskControlScope)
	return err == nil
}

func currentPendingApproval(details TaskDetails) bool {
	value := details.Task
	request := details.PendingRequest
	if value.Status != task.StatusWaitingUser || value.WaitingReason != "approval_required" || request == nil ||
		request.TaskID != value.ID || request.Status != task.PendingRequestPending ||
		(request.ExpiresAt != nil && !request.ExpiresAt.After(time.Now())) {
		return false
	}
	if value.CurrentRevision != nil {
		return request.TaskRevision != nil && *request.TaskRevision == *value.CurrentRevision
	}
	return request.TaskRevision == nil
}

func currentPendingClarification(details TaskDetails) bool {
	value := details.Task
	if value.Status != task.StatusWaitingUser || value.WaitingReason != "clarification_required" || value.CurrentStep == "" {
		return false
	}
	var activeStep *task.Step
	for i := range details.Steps {
		step := &details.Steps[i]
		if step.ID == value.CurrentStep && step.TaskID == value.ID && step.Kind == "clarification" && step.Status == task.StepWaitingUser {
			activeStep = step
			break
		}
	}
	if activeStep == nil {
		return false
	}
	for _, request := range details.ClarificationRequests {
		if request.Status != task.ClarificationPending || request.TaskID != value.ID || request.Question == "" || request.RespondedAt != nil {
			continue
		}
		var sourceStep *task.Step
		for i := range details.Steps {
			step := &details.Steps[i]
			if step.ID == request.ModelStepID && step.TaskID == value.ID {
				sourceStep = step
				break
			}
		}
		if sourceStep == nil || sourceStep.Sequence >= activeStep.Sequence {
			continue
		}
		if value.CurrentRevision != nil {
			if activeStep.TaskRevision == nil || sourceStep.TaskRevision == nil ||
				*activeStep.TaskRevision != *value.CurrentRevision || *sourceStep.TaskRevision != *value.CurrentRevision {
				continue
			}
		}
		return true
	}
	return false
}

func taskHasUnverifiedOutcome(details TaskDetails) bool {
	value := details.Task
	for _, step := range details.Steps {
		if value.CurrentRevision != nil && step.TaskRevision != nil && *step.TaskRevision != *value.CurrentRevision {
			continue
		}
		if step.Status == task.StepUnknown || step.FailureReason == "side_effect_unknown" {
			return true
		}
	}
	return false
}
