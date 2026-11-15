package app

import (
	"context"
	"fmt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func (s *TaskService) SetPriority(ctx context.Context, principal identity.Principal, taskID string,
	priority task.QueuePriority, expectedEventSequence int64, idempotencyKey string) (task.Task, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return task.Task{}, err
	}
	if !domain.ValidTaskID(taskID) {
		return task.Task{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	if !priority.Valid() || expectedEventSequence < 0 {
		return task.Task{}, domain.InvalidArgument("priority", "queue priority and event sequence are invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return task.Task{}, err
	}
	requester, ok := s.store.(TaskPriorityRequester)
	if !ok {
		return task.Task{}, domain.NewError(domain.CodeUnsupported, "task priority updates are unavailable")
	}
	details, err := s.store.GetTask(ctx, taskID, principal.UserID)
	if err != nil {
		return task.Task{}, err
	}
	if _, err := s.authorizeTask(ctx, principal, details.Task.HostID, taskControlScope); err != nil {
		return task.Task{}, hideTaskAccessError(err)
	}
	requestID, err := domain.NewTaskActionID()
	if err != nil {
		return task.Task{}, fmt.Errorf("generate task priority action id: %w", err)
	}
	record, err := idempotencyRecord(principal.UserID, "task.priority.set", idempotencyKey, struct {
		TaskID                string             `json:"task_id"`
		Priority              task.QueuePriority `json:"priority"`
		ExpectedEventSequence int64              `json:"expected_event_sequence"`
	}{taskID, priority, expectedEventSequence}, requestID, taskID)
	if err != nil {
		return task.Task{}, err
	}
	result, err := requester.SetTaskPriority(ctx, record, taskID, priority, expectedEventSequence, requestID)
	if err != nil {
		return task.Task{}, err
	}
	initiatorID := result.InitiatorID
	if initiatorID == "" {
		initiatorID = result.OwnerID
	}
	if result.ID != taskID || initiatorID != principal.UserID || !result.QueuePriority.Valid() {
		return task.Task{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return result, nil
}
