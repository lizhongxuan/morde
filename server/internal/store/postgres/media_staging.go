package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) CreateTaskMediaPreviewForApproval(ctx context.Context, value app.CreateTaskMediaStagedObjectCommand) error {
	if !validTaskMediaStagingCommand(value) {
		return domain.InvalidArgument("task_media_preview", "Task media preview fields are invalid")
	}
	return withTx(ctx, s.pool.Begin, func(tx pgx.Tx) error {
		queries := s.q.WithTx(tx)
		if _, err := queries.CreateTaskMediaStagedObject(ctx, sqlcgen.CreateTaskMediaStagedObjectParams{
			OperationID: value.OperationID, TaskID: value.TaskID, OwnerID: value.OwnerID,
			RuntimeCallID: value.RuntimeCallID, MediaType: value.MediaType, SizeBytes: value.SizeBytes,
			Sha256: value.SHA256, ExpiresAt: timestamp(value.ExpiresAt), State: string(app.TaskMediaPreviewPending),
		}); err != nil {
			return fmt.Errorf("create task media staged object: %w", err)
		}
		linked, err := queries.LinkTaskMediaStagedObjectToPendingApproval(ctx, sqlcgen.LinkTaskMediaStagedObjectToPendingApprovalParams{
			OperationID: nullableText(value.OperationID), PendingRequestID: value.PendingRequestID,
			TaskID: value.TaskID, PendingRequestVersion: value.PendingRequestVersion,
		})
		if err != nil {
			return fmt.Errorf("link task media staged object to pending approval: %w", err)
		}
		if linked != 1 {
			return domain.NewError(domain.CodeConflict, "task approval changed before media preview was linked")
		}
		return nil
	})
}

func (s *Store) GetTaskMediaOperationDetails(ctx context.Context, taskID, operationID string) (app.TaskMediaStagedObject, error) {
	if !domain.ValidTaskID(taskID) || !domain.ValidOperationID(operationID) {
		return app.TaskMediaStagedObject{}, domain.InvalidArgument("task_media_preview", "task media operation identity is invalid")
	}
	row, err := s.q.GetTaskMediaOperationDetails(ctx, sqlcgen.GetTaskMediaOperationDetailsParams{TaskID: taskID, OperationID: operationID})
	if err != nil {
		return app.TaskMediaStagedObject{}, storeError("get task media operation", "task media operation", err)
	}
	return taskMediaStagedObjectDomain(row), nil
}

func (s *Store) GetTaskMediaPreviewForOwner(ctx context.Context, ownerID, taskID, operationID string, now time.Time) (app.TaskMediaStagedObject, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidOperationID(operationID) || now.IsZero() {
		return app.TaskMediaStagedObject{}, domain.InvalidArgument("task_media_preview", "task media preview identity is invalid")
	}
	row, err := s.q.GetTaskMediaPreviewForOwner(ctx, sqlcgen.GetTaskMediaPreviewForOwnerParams{
		OwnerID: ownerID, TaskID: taskID, OperationID: operationID, Now: timestamp(now),
	})
	if err != nil {
		return app.TaskMediaStagedObject{}, storeError("get task media preview", "task media preview", err)
	}
	return taskMediaStagedObjectDomain(row), nil
}

func (s *Store) GetTaskMediaForApprovedSave(ctx context.Context, ownerID, taskID, pendingRequestID string, pendingVersion int64, operationID string) (app.TaskMediaStagedObject, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidPendingRequestID(pendingRequestID) ||
		pendingVersion < 1 || !domain.ValidOperationID(operationID) {
		return app.TaskMediaStagedObject{}, domain.InvalidArgument("task_media_save", "approved Task media identity is invalid")
	}
	row, err := s.q.GetTaskMediaForApprovedSave(ctx, sqlcgen.GetTaskMediaForApprovedSaveParams{
		OwnerID: ownerID, TaskID: taskID, PendingRequestID: pendingRequestID,
		PendingRequestVersion: pendingVersion, OperationID: operationID,
	})
	if err != nil {
		return app.TaskMediaStagedObject{}, storeError("get approved task media", "task media operation", err)
	}
	return taskMediaStagedObjectDomain(row), nil
}

func (s *Store) ListTaskMediaObjectsForCleanup(ctx context.Context, now time.Time, batchSize int32) ([]app.TaskMediaStagedObject, error) {
	if now.IsZero() || batchSize < 1 || batchSize > 1000 {
		return nil, domain.InvalidArgument("task_media_preview", "task media cleanup query is invalid")
	}
	rows, err := s.q.ListTaskMediaObjectsForCleanup(ctx, sqlcgen.ListTaskMediaObjectsForCleanupParams{Now: timestamp(now), BatchSize: batchSize})
	if err != nil {
		return nil, fmt.Errorf("list task media objects for cleanup: %w", err)
	}
	result := make([]app.TaskMediaStagedObject, 0, len(rows))
	for _, row := range rows {
		result = append(result, taskMediaStagedObjectDomain(row))
	}
	return result, nil
}

func (s *Store) SetTaskMediaStagedObjectState(ctx context.Context, taskID, operationID string, expected, next app.TaskMediaStagedObjectState) (app.TaskMediaStagedObject, error) {
	if !domain.ValidTaskID(taskID) || !domain.ValidOperationID(operationID) || !validTaskMediaTransition(expected, next) {
		return app.TaskMediaStagedObject{}, domain.InvalidArgument("task_media_preview", "task media state transition is invalid")
	}
	row, err := s.q.SetTaskMediaStagedObjectState(ctx, sqlcgen.SetTaskMediaStagedObjectStateParams{
		TaskID: taskID, OperationID: operationID, ExpectedState: string(expected), NextState: string(next),
	})
	if err != nil {
		return app.TaskMediaStagedObject{}, storeError("update task media staged object", "task media staged object", err)
	}
	return taskMediaStagedObjectRowDomain(row), nil
}

func (s *Store) MarkTaskMediaStagedObjectCleanupComplete(ctx context.Context, taskID, operationID string) error {
	if !domain.ValidTaskID(taskID) || !domain.ValidOperationID(operationID) {
		return domain.InvalidArgument("task_media_preview", "task media cleanup identity is invalid")
	}
	updated, err := s.q.MarkTaskMediaStagedObjectCleanupComplete(ctx, sqlcgen.MarkTaskMediaStagedObjectCleanupCompleteParams{
		TaskID: taskID, OperationID: operationID,
	})
	if err != nil {
		return fmt.Errorf("mark task media object cleanup complete: %w", err)
	}
	if updated != 1 {
		return domain.NewError(domain.CodeConflict, "task media object is not ready for cleanup")
	}
	return nil
}

func (s *Store) DeleteCleanedTaskMediaStagedObject(ctx context.Context, taskID, operationID string) error {
	if !domain.ValidTaskID(taskID) || !domain.ValidOperationID(operationID) {
		return domain.InvalidArgument("task_media_preview", "task media cleanup identity is invalid")
	}
	if err := s.q.DeleteCleanedTaskMediaStagedObject(ctx, sqlcgen.DeleteCleanedTaskMediaStagedObjectParams{
		TaskID: taskID, OperationID: operationID,
	}); err != nil {
		return fmt.Errorf("delete cleaned task media staged object: %w", err)
	}
	return nil
}

func validTaskMediaStagingCommand(value app.CreateTaskMediaStagedObjectCommand) bool {
	return domain.ValidUserID(value.OwnerID) && domain.ValidTaskID(value.TaskID) && domain.ValidOperationID(value.OperationID) &&
		domain.ValidPendingRequestID(value.PendingRequestID) && value.PendingRequestVersion > 0 &&
		len(value.RuntimeCallID) >= 8 && len(value.RuntimeCallID) <= 128 && utf8.ValidString(value.RuntimeCallID) &&
		validTaskMediaType(value.MediaType) && value.MediaType == strings.ToLower(value.MediaType) && value.SizeBytes > 0 && value.SizeBytes <= workspace.MaxSyncFileBytes &&
		len(value.SHA256) == sha256.Size && !value.ExpiresAt.IsZero() && value.ExpiresAt.After(time.Now())
}

func validTaskMediaType(value string) bool {
	switch strings.ToLower(value) {
	case "image/png", "image/jpeg", "image/webp", "video/mp4", "audio/mpeg", "audio/ogg", "audio/opus", "audio/wav", "audio/aac":
		return true
	default:
		return false
	}
}

func validTaskMediaState(value app.TaskMediaStagedObjectState) bool {
	switch value {
	case app.TaskMediaPreviewPending, app.TaskMediaPreviewSaving, app.TaskMediaPreviewSaved, app.TaskMediaPreviewDiscard, app.TaskMediaPreviewExpired:
		return true
	default:
		return false
	}
}

func validTaskMediaTransition(from, to app.TaskMediaStagedObjectState) bool {
	if !validTaskMediaState(from) || !validTaskMediaState(to) {
		return false
	}
	switch from {
	case app.TaskMediaPreviewPending:
		return to == app.TaskMediaPreviewSaving || to == app.TaskMediaPreviewDiscard || to == app.TaskMediaPreviewExpired
	case app.TaskMediaPreviewSaving:
		return to == app.TaskMediaPreviewSaved || to == app.TaskMediaPreviewDiscard || to == app.TaskMediaPreviewExpired
	default:
		return false
	}
}

func taskMediaStagedObjectDomain(row sqlcgen.TaskMediaOperationDetail) app.TaskMediaStagedObject {
	return app.TaskMediaStagedObject{
		TaskID: row.TaskID, OwnerID: row.OwnerID, InitiatorID: row.InitiatorID, OperationID: row.OperationID, RuntimeCallID: row.RuntimeCallID,
		OperationKey: row.OperationKey, ProviderOperationID: row.ProviderOperationID,
		RequestHash: append([]byte(nil), row.RequestHash...), OperationStatus: row.OperationStatus, TokenState: row.TokenState,
		ReservedTokens: row.ReservedTokens, InputTokens: int64Pointer(row.InputTokens), OutputTokens: int64Pointer(row.OutputTokens),
		MediaType: row.MediaType, SizeBytes: row.SizeBytes, SHA256: append([]byte(nil), row.Sha256...),
		ExpiresAt: row.ExpiresAt.Time, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
		State: app.TaskMediaStagedObjectState(row.State), CleanupState: app.TaskMediaCleanupState(row.CleanupState),
	}
}

func taskMediaStagedObjectRowDomain(row sqlcgen.TaskMediaStagedObject) app.TaskMediaStagedObject {
	return app.TaskMediaStagedObject{
		TaskID: row.TaskID, OwnerID: row.OwnerID, InitiatorID: row.InitiatorID, OperationID: row.OperationID, RuntimeCallID: row.RuntimeCallID,
		MediaType: row.MediaType, SizeBytes: row.SizeBytes, SHA256: append([]byte(nil), row.Sha256...),
		ExpiresAt: row.ExpiresAt.Time, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
		State: app.TaskMediaStagedObjectState(row.State), CleanupState: app.TaskMediaCleanupState(row.CleanupState),
	}
}

var _ app.TaskMediaStagedObjectStore = (*Store)(nil)
var _ app.TaskMediaApprovedSaveStore = (*Store)(nil)
