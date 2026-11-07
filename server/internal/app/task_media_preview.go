package app

import (
	"context"
	"crypto/sha256"
	"mime"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

type TaskMediaPreviewService struct {
	objects TaskMediaStagedObjectStore
	content TaskMediaVerifiedContentStore
}

func NewTaskMediaPreviewService(objects TaskMediaStagedObjectStore, content TaskMediaVerifiedContentStore) (*TaskMediaPreviewService, error) {
	if objects == nil || content == nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "task media preview dependencies are required")
	}
	return &TaskMediaPreviewService{objects: objects, content: content}, nil
}

func (*TaskMediaPreviewService) GenerationReady() bool { return false }

func (s *TaskMediaPreviewService) OpenPreview(ctx context.Context, principal identity.Principal, taskID, operationID string) (TaskMediaPreview, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return TaskMediaPreview{}, err
	}
	if !domain.ValidTaskID(taskID) || !domain.ValidOperationID(operationID) {
		return TaskMediaPreview{}, domain.NewError(domain.CodeNotFound, "task media preview not found")
	}
	now := time.Now().UTC()
	staged, err := s.objects.GetTaskMediaPreviewForOwner(ctx, principal.UserID, taskID, operationID, now)
	if err != nil {
		return TaskMediaPreview{}, err
	}
	mediaType, params, err := mime.ParseMediaType(staged.MediaType)
	if err != nil || len(params) != 0 || mediaType != strings.ToLower(staged.MediaType) ||
		staged.TaskID != taskID || staged.OwnerID != principal.UserID || staged.OperationID != operationID ||
		staged.OperationStatus != string(task.OperationSucceeded) || staged.TokenState != "settled" ||
		staged.State != TaskMediaPreviewPending || staged.CleanupState != TaskMediaCleanupPending ||
		!staged.ExpiresAt.After(now) || staged.SizeBytes < 1 || staged.SizeBytes > workspace.MaxSyncFileBytes || len(staged.SHA256) != sha256.Size ||
		!taskMediaPreviewTypeAllowed(mediaType) {
		return TaskMediaPreview{}, domain.NewError(domain.CodeInternal, "task media preview metadata is invalid")
	}
	body, err := s.content.OpenVerified(ctx, principal.UserID, taskID, operationID, TaskMediaStagedContent{
		MediaType: mediaType, SizeBytes: staged.SizeBytes, SHA256: append([]byte(nil), staged.SHA256...),
	})
	if err != nil {
		return TaskMediaPreview{}, err
	}
	if body == nil {
		return TaskMediaPreview{}, domain.NewError(domain.CodeInternal, "task media preview content is unavailable")
	}
	return TaskMediaPreview{Body: body, MediaType: mediaType, SizeBytes: staged.SizeBytes}, nil
}

func taskMediaPreviewTypeAllowed(value string) bool {
	switch value {
	case "image/png", "image/jpeg", "image/webp", "video/mp4", "audio/mpeg", "audio/ogg", "audio/opus", "audio/wav", "audio/aac":
		return true
	default:
		return false
	}
}
