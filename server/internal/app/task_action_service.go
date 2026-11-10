package app

import (
	"context"
	"fmt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

// RequestAction applies a legacy status-fenced, idempotent pause, resume, or
// cancel request.
func (s *TaskService) RequestAction(
	ctx context.Context,
	principal identity.Principal,
	taskID string,
	action task.Action,
	expectedStatus task.Status,
	idempotencyKey string,
) (TaskActionResult, error) {
	return s.RequestActionWithRevisionFence(ctx, principal, taskID, action, expectedStatus, nil, idempotencyKey)
}

// RequestActionWithRevisionFence applies an idempotent task action optionally
// fenced to the exact task revision pair observed by the caller. The store
// compares the status and supplied revisions atomically before mutation.
func (s *TaskService) RequestActionWithRevisionFence(
	ctx context.Context,
	principal identity.Principal,
	taskID string,
	action task.Action,
	expectedStatus task.Status,
	revisionFence *TaskActionRevisionFence,
	idempotencyKey string,
) (TaskActionResult, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return TaskActionResult{}, err
	}
	if !domain.ValidTaskID(taskID) {
		return TaskActionResult{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	if !validTaskActionExpectedStatus(action, expectedStatus) {
		return TaskActionResult{}, domain.InvalidArgument("action", "action or expected_status is invalid")
	}
	if revisionFence != nil && (revisionFence.CurrentRevision < 0 || revisionFence.DesiredRevision < 0) {
		return TaskActionResult{}, domain.InvalidArgument("revision", "expected task revisions must be non-negative")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return TaskActionResult{}, err
	}
	if s.actions == nil {
		return TaskActionResult{}, domain.NewError(domain.CodeUnsupported, "task actions are unavailable")
	}
	details, err := s.store.GetTask(ctx, taskID, principal.UserID)
	if err != nil {
		return TaskActionResult{}, err
	}
	if _, err := s.authorizeTask(ctx, principal, details.Task.HostID, taskControlScope); err != nil {
		return TaskActionResult{}, hideTaskAccessError(err)
	}
	requestID, err := domain.NewTaskActionID()
	if err != nil {
		return TaskActionResult{}, fmt.Errorf("generate task action id: %w", err)
	}
	request, err := idempotencyRecord(principal.UserID, "task.action."+string(action), idempotencyKey, struct {
		Action         task.Action              `json:"action"`
		TaskID         string                   `json:"task_id"`
		ExpectedStatus task.Status              `json:"expected_status"`
		RevisionFence  *TaskActionRevisionFence `json:"revision_fence,omitempty"`
	}{action, taskID, expectedStatus, revisionFence}, requestID, taskID)
	if err != nil {
		return TaskActionResult{}, err
	}
	result, err := s.actions.RequestTaskAction(ctx, request, taskID, action, expectedStatus, revisionFence, requestID)
	if err != nil {
		return TaskActionResult{}, err
	}
	initiatorID := result.Task.InitiatorID
	if initiatorID == "" {
		initiatorID = result.Task.OwnerID
	}
	if result.Task.ID != taskID || initiatorID != principal.UserID ||
		(result.Task.ProjectID == "" && result.Task.OwnerID != principal.UserID) || result.Action != action {
		return TaskActionResult{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	for _, operationID := range result.UnsettledOperationIDs {
		if !domain.ValidOperationID(operationID) {
			return TaskActionResult{}, domain.NewError(domain.CodeInternal, "internal error")
		}
	}
	return result, nil
}

func validTaskActionExpectedStatus(action task.Action, status task.Status) bool {
	switch action {
	case task.ActionPause:
		return status == task.StatusQueued || status == task.StatusRunning
	case task.ActionResume:
		return status == task.StatusPaused
	case task.ActionCancel:
		return status == task.StatusQueued || status == task.StatusRunning || status == task.StatusWaitingUser || status == task.StatusPaused
	default:
		return false
	}
}
