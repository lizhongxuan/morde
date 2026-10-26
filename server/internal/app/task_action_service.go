package app

import (
	"context"
	"fmt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

// RequestAction applies a versioned, idempotent pause, resume, or cancel
// request. The store compares the task's current status atomically so a stale
// client cannot undo a newer transition.
func (s *TaskService) RequestAction(
	ctx context.Context,
	principal identity.Principal,
	taskID string,
	action task.Action,
	expectedStatus task.Status,
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
	if _, err := authorizeTaskHost(ctx, s.authorizer, principal, details.Task.HostID, taskControlScope); err != nil {
		return TaskActionResult{}, hideTaskAccessError(err)
	}
	requestID, err := domain.NewTaskActionID()
	if err != nil {
		return TaskActionResult{}, fmt.Errorf("generate task action id: %w", err)
	}
	request, err := idempotencyRecord(principal.UserID, "task.action."+string(action), idempotencyKey, struct {
		Action         task.Action `json:"action"`
		TaskID         string      `json:"task_id"`
		ExpectedStatus task.Status `json:"expected_status"`
	}{action, taskID, expectedStatus}, requestID, taskID)
	if err != nil {
		return TaskActionResult{}, err
	}
	result, err := s.actions.RequestTaskAction(ctx, request, taskID, action, expectedStatus, requestID)
	if err != nil {
		return TaskActionResult{}, err
	}
	if result.Task.ID != taskID || result.Task.OwnerID != principal.UserID || result.Action != action {
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
