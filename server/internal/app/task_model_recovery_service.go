package app

import (
	"context"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

// RetryTaskModelRecovery queues the one server-verified no-dispatch recovery
// attached to the exact revision and event the caller observed.
func (s *TaskService) RetryTaskModelRecovery(ctx context.Context, principal identity.Principal, taskID string,
	expectedRevision, expectedEventSequence int64, idempotencyKey string) (TaskDetails, error) {
	if err := validateTaskReadPrincipal(principal); err != nil {
		return TaskDetails{}, err
	}
	if !domain.ValidTaskID(taskID) || expectedRevision < 0 || expectedEventSequence < 0 {
		return TaskDetails{}, domain.InvalidArgument("task_model_recovery", "retry request is invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return TaskDetails{}, err
	}
	store, ok := s.store.(TaskModelRecoveryStore)
	if !ok {
		return TaskDetails{}, domain.NewError(domain.CodeUnsupported, "task recovery is unavailable")
	}
	details, err := s.store.GetTask(ctx, taskID, principal.UserID)
	if err != nil {
		return TaskDetails{}, err
	}
	value := details.Task
	if value.InternalTool || value.InitiatorID != principal.UserID ||
		(value.ProjectID == "" && value.OwnerID != principal.UserID) {
		return TaskDetails{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	if _, err := s.authorizeTask(ctx, principal, value.HostID, taskControlScope); err != nil {
		return TaskDetails{}, hideTaskAccessError(err)
	}
	if value.CurrentRevision == nil || value.DesiredRevision == nil || *value.CurrentRevision != expectedRevision ||
		*value.DesiredRevision != expectedRevision {
		return TaskDetails{}, domain.NewError(domain.CodeConflict, "task revision changed; reload before retrying")
	}
	request, err := idempotencyRecord(principal.UserID, "task.model_recovery.retry", idempotencyKey, struct {
		TaskID               string `json:"task_id"`
		TaskRevision         int64  `json:"task_revision"`
		AuthorizationVersion int64  `json:"authorization_version"`
		EventSequence        int64  `json:"event_sequence"`
	}{taskID, expectedRevision, value.Authorization.Version, expectedEventSequence}, taskID, "")
	if err != nil {
		return TaskDetails{}, err
	}
	result, err := store.RequestTaskModelRecoveryRetry(ctx, request, TaskModelRecoveryRetryCommand{
		TaskID: taskID, OwnerID: value.OwnerID, InitiatorID: value.InitiatorID, TaskRevision: expectedRevision,
		AuthorizationVersion: value.Authorization.Version, EventSequence: expectedEventSequence,
	})
	if err != nil {
		return TaskDetails{}, err
	}
	if !ValidTaskModelRecoveryIntent(result.Intent) || result.Intent.TaskID != taskID || result.Intent.OwnerID != value.OwnerID ||
		result.Intent.InitiatorID != value.InitiatorID || result.Intent.TaskRevision != expectedRevision ||
		result.Intent.AuthorizationVersion != value.Authorization.Version ||
		(result.Intent.State != task.ModelRecoveryScheduled && result.Intent.State != task.ModelRecoveryAttempting &&
			result.Intent.State != task.ModelRecoverySpent && result.Intent.State != task.ModelRecoveryCompleted) {
		return TaskDetails{}, domain.NewError(domain.CodeInternal, "task recovery request returned an invalid state")
	}
	return s.Get(ctx, principal, taskID)
}
