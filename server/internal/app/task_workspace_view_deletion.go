package app

import (
	"context"
	"fmt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func (s *TaskService) RequestWorkspaceViewDeletion(
	ctx context.Context,
	principal identity.Principal,
	taskID string,
	confirmDelete bool,
	idempotencyKey string,
) (TaskWorkspaceViewDeletion, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return TaskWorkspaceViewDeletion{}, err
	}
	if !domain.ValidTaskID(taskID) {
		return TaskWorkspaceViewDeletion{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	if !confirmDelete {
		return TaskWorkspaceViewDeletion{}, domain.InvalidArgument("confirm_delete", "explicit deletion confirmation is required")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return TaskWorkspaceViewDeletion{}, err
	}
	if s.viewDeletions == nil {
		return TaskWorkspaceViewDeletion{}, domain.NewError(domain.CodeUnsupported, "task Workspace view deletion is unavailable")
	}
	details, err := s.store.GetTask(ctx, taskID, principal.UserID)
	if err != nil {
		return TaskWorkspaceViewDeletion{}, err
	}
	if details.Task.WorkspaceID == "" || !domain.ValidHostID(details.Task.HostID) {
		return TaskWorkspaceViewDeletion{}, domain.NewError(domain.CodeInvalidState, "task has no private Workspace view")
	}
	if details.Task.Status != task.StatusCompleted && details.Task.Status != task.StatusFailed && details.Task.Status != task.StatusCancelled {
		return TaskWorkspaceViewDeletion{}, domain.NewError(domain.CodeInvalidState, "task must be stopped before deleting its Workspace view")
	}
	if _, err := s.authorizeTask(ctx, principal, details.Task.HostID, taskControlScope); err != nil {
		return TaskWorkspaceViewDeletion{}, hideTaskAccessError(err)
	}
	requestID, err := domain.NewTaskWorkspaceViewDeletionID()
	if err != nil {
		return TaskWorkspaceViewDeletion{}, fmt.Errorf("generate Task Workspace view deletion id: %w", err)
	}
	record, err := idempotencyRecord(principal.UserID, "task.workspace_view.delete", idempotencyKey, struct {
		TaskID        string `json:"task_id"`
		ConfirmDelete bool   `json:"confirm_delete"`
	}{TaskID: taskID, ConfirmDelete: true}, requestID, taskID)
	if err != nil {
		return TaskWorkspaceViewDeletion{}, err
	}
	deletion, err := s.viewDeletions.RequestTaskWorkspaceViewDeletion(ctx, record, taskID)
	if err != nil {
		return TaskWorkspaceViewDeletion{}, err
	}
	if !domain.ValidTaskWorkspaceViewDeletionID(deletion.ID) || deletion.TaskID != taskID || deletion.HostID != details.Task.HostID ||
		(deletion.Status != "pending" && deletion.Status != "applied") {
		return TaskWorkspaceViewDeletion{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return deletion, nil
}

func (s *TaskService) ListPendingWorkspaceViewDeletions(ctx context.Context, principal identity.Principal) ([]TaskWorkspaceViewDeletion, error) {
	if principal.Kind != identity.KindHostReplica || !domain.ValidUserID(principal.UserID) || !domain.ValidHostID(principal.HostScope) || principal.HostGeneration < 1 {
		return nil, domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
	}
	if s.viewDeletions == nil {
		return nil, domain.NewError(domain.CodeUnsupported, "task Workspace view deletion is unavailable")
	}
	items, err := s.viewDeletions.ListPendingTaskWorkspaceViewDeletions(ctx, principal.HostScope, principal.UserID, principal.HostGeneration)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if !domain.ValidTaskWorkspaceViewDeletionID(item.ID) || !domain.ValidTaskID(item.TaskID) || item.HostID != principal.HostScope || item.Status != "pending" {
			return nil, domain.NewError(domain.CodeInternal, "task Workspace view deletion is inconsistent")
		}
	}
	if items == nil {
		items = []TaskWorkspaceViewDeletion{}
	}
	return items, nil
}

func (s *TaskService) GetWorkspaceViewDeletion(ctx context.Context, principal identity.Principal, taskID string) (TaskWorkspaceViewDeletion, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return TaskWorkspaceViewDeletion{}, err
	}
	if !domain.ValidTaskID(taskID) {
		return TaskWorkspaceViewDeletion{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	if s.viewDeletions == nil {
		return TaskWorkspaceViewDeletion{}, domain.NewError(domain.CodeUnsupported, "task Workspace view deletion is unavailable")
	}
	details, err := s.store.GetTask(ctx, taskID, principal.UserID)
	if err != nil {
		return TaskWorkspaceViewDeletion{}, err
	}
	if _, err := s.authorizeTask(ctx, principal, details.Task.HostID, taskControlScope); err != nil {
		return TaskWorkspaceViewDeletion{}, hideTaskAccessError(err)
	}
	deletion, err := s.viewDeletions.GetTaskWorkspaceViewDeletion(ctx, principal.UserID, taskID)
	if err != nil {
		return TaskWorkspaceViewDeletion{}, err
	}
	if !domain.ValidTaskWorkspaceViewDeletionID(deletion.ID) || deletion.TaskID != taskID || deletion.HostID != details.Task.HostID ||
		(deletion.Status != "pending" && deletion.Status != "applied") {
		return TaskWorkspaceViewDeletion{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return deletion, nil
}

func (s *TaskService) CompleteWorkspaceViewDeletion(ctx context.Context, principal identity.Principal, taskID, requestID string) (TaskWorkspaceViewDeletion, error) {
	if principal.Kind != identity.KindHostReplica || !domain.ValidUserID(principal.UserID) || !domain.ValidHostID(principal.HostScope) || principal.HostGeneration < 1 {
		return TaskWorkspaceViewDeletion{}, domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
	}
	if !domain.ValidTaskID(taskID) || !domain.ValidTaskWorkspaceViewDeletionID(requestID) {
		return TaskWorkspaceViewDeletion{}, domain.NewError(domain.CodeNotFound, "task Workspace view deletion not found")
	}
	if s.viewDeletions == nil {
		return TaskWorkspaceViewDeletion{}, domain.NewError(domain.CodeUnsupported, "task Workspace view deletion is unavailable")
	}
	return s.viewDeletions.CompleteTaskWorkspaceViewDeletion(ctx, principal.HostScope, principal.UserID, principal.HostGeneration, taskID, requestID)
}
