package app

import (
	"context"
	"errors"
	"time"

	"morde/server/internal/domain"
)

type TaskMediaCleanupService struct {
	objects TaskMediaStagedObjectStore
	content TaskMediaContentStore
}

func NewTaskMediaCleanupService(objects TaskMediaStagedObjectStore, content TaskMediaContentStore) (*TaskMediaCleanupService, error) {
	if objects == nil || content == nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "task media cleanup dependencies are required")
	}
	return &TaskMediaCleanupService{objects: objects, content: content}, nil
}

func (s *TaskMediaCleanupService) RunOnce(ctx context.Context, now time.Time, batchSize int32) error {
	if now.IsZero() || batchSize < 1 || batchSize > 1000 {
		return domain.InvalidArgument("task_media_cleanup", "cleanup time and batch size are invalid")
	}
	objects, err := s.objects.ListTaskMediaObjectsForCleanup(ctx, now, batchSize)
	if err != nil {
		return err
	}
	var failures []error
	for _, object := range objects {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if err := s.cleanupObject(ctx, now, object); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *TaskMediaCleanupService) cleanupObject(ctx context.Context, now time.Time, object TaskMediaStagedObject) error {
	if !domain.ValidUserID(object.OwnerID) || !domain.ValidTaskID(object.TaskID) || !domain.ValidOperationID(object.OperationID) {
		return domain.NewError(domain.CodeInternal, "task media cleanup metadata is invalid")
	}
	if object.State == TaskMediaPreviewDiscard {
		ready, err := s.objects.CanCleanupTaskMediaStagedObject(ctx, object.TaskID, object.OperationID)
		if err != nil {
			return err
		}
		if !ready {
			return nil
		}
	}
	if object.CleanupState == TaskMediaCleanupComplete {
		if object.State != TaskMediaPreviewSaved && object.State != TaskMediaPreviewDiscard && object.State != TaskMediaPreviewExpired {
			return domain.NewError(domain.CodeInternal, "completed task media cleanup state is not terminal")
		}
		if object.State == TaskMediaPreviewExpired {
			if err := s.objects.ExpireTaskMediaPreviewApproval(ctx, object.TaskID, object.OperationID); err != nil {
				return err
			}
		}
		return s.objects.DeleteCleanedTaskMediaStagedObject(ctx, object.TaskID, object.OperationID)
	}
	if object.CleanupState != TaskMediaCleanupPending {
		return domain.NewError(domain.CodeInternal, "task media cleanup state is invalid")
	}
	if object.State == TaskMediaPreviewPending {
		if object.ExpiresAt.After(now) {
			return domain.NewError(domain.CodeInternal, "unexpired task media preview was selected for cleanup")
		}
		updated, err := s.objects.SetTaskMediaStagedObjectState(ctx, object.TaskID, object.OperationID, TaskMediaPreviewPending, TaskMediaPreviewExpired)
		if err != nil {
			return err
		}
		if updated.TaskID != object.TaskID || updated.OwnerID != object.OwnerID || updated.OperationID != object.OperationID || updated.State != TaskMediaPreviewExpired {
			return domain.NewError(domain.CodeInternal, "expired task media preview transition was not confirmed")
		}
		if err := s.objects.ExpireTaskMediaPreviewApproval(ctx, object.TaskID, object.OperationID); err != nil {
			return err
		}
	} else if object.State != TaskMediaPreviewSaved && object.State != TaskMediaPreviewDiscard && object.State != TaskMediaPreviewExpired {
		return domain.NewError(domain.CodeInternal, "task media cleanup state is not terminal")
	} else if object.State == TaskMediaPreviewExpired {
		if err := s.objects.ExpireTaskMediaPreviewApproval(ctx, object.TaskID, object.OperationID); err != nil {
			return err
		}
	}
	if err := s.content.Delete(ctx, object.OwnerID, object.TaskID, object.OperationID); err != nil {
		return err
	}
	if err := s.objects.MarkTaskMediaStagedObjectCleanupComplete(ctx, object.TaskID, object.OperationID); err != nil {
		return err
	}
	return s.objects.DeleteCleanedTaskMediaStagedObject(ctx, object.TaskID, object.OperationID)
}
