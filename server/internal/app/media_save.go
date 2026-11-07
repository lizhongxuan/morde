package app

import (
	"context"
	"crypto/sha256"
	"errors"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

type TaskMediaSaveService struct {
	objects TaskMediaApprovedSaveStore
	content TaskMediaVerifiedContentStore
}

func NewTaskMediaSaveService(objects TaskMediaApprovedSaveStore, content TaskMediaVerifiedContentStore) (*TaskMediaSaveService, error) {
	if objects == nil || content == nil {
		return nil, errors.New("task media save dependencies are required")
	}
	return &TaskMediaSaveService{objects: objects, content: content}, nil
}

func (s *TaskMediaSaveService) OpenApproved(ctx context.Context, ownerID, taskID, pendingRequestID string, pendingVersion int64, operationID string) (TaskMediaSaveInput, error) {
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidPendingRequestID(pendingRequestID) ||
		pendingVersion < 1 || !domain.ValidOperationID(operationID) {
		return TaskMediaSaveInput{}, domain.InvalidArgument("task_media_save", "approved Task media identity is invalid")
	}
	object, err := s.objects.GetTaskMediaForApprovedSave(ctx, ownerID, taskID, pendingRequestID, pendingVersion, operationID)
	if err != nil {
		return TaskMediaSaveInput{}, err
	}
	if object.OwnerID != ownerID || object.TaskID != taskID || object.OperationID != operationID ||
		object.State != TaskMediaPreviewSaving || object.CleanupState != TaskMediaCleanupPending ||
		object.OperationStatus != "succeeded" || object.TokenState != "settled" ||
		object.InputTokens == nil || object.OutputTokens == nil || object.ReservedTokens <= 0 ||
		*object.InputTokens < 0 || *object.OutputTokens < 0 || *object.InputTokens > object.ReservedTokens ||
		*object.OutputTokens > object.ReservedTokens-*object.InputTokens || !taskMediaPreviewTypeAllowed(object.MediaType) ||
		object.SizeBytes < 1 || object.SizeBytes > workspace.MaxSyncFileBytes || len(object.SHA256) != sha256.Size {
		return TaskMediaSaveInput{}, domain.NewError(domain.CodeInvalidState, "Task media is not ready for an approved save")
	}
	body, err := s.content.OpenVerified(ctx, object.OwnerID, object.TaskID, object.OperationID, TaskMediaStagedContent{
		MediaType: object.MediaType, SizeBytes: object.SizeBytes, SHA256: object.SHA256,
	})
	if err != nil {
		return TaskMediaSaveInput{}, err
	}
	if body == nil {
		return TaskMediaSaveInput{}, domain.NewError(domain.CodeInternal, "approved Task media content is unavailable")
	}
	return TaskMediaSaveInput{Body: body, MediaType: object.MediaType, SizeBytes: object.SizeBytes, SHA256: append([]byte(nil), object.SHA256...)}, nil
}
