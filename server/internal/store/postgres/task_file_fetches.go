package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

const taskFileFetchProjectionColumns = `f.fetch_id,f.owner_id,f.task_id,f.operation_key,f.source_installation_id,
i.display_name,f.source_directory_name,f.target_grant_version,f.task_authorization_version,f.task_revision,f.relative_path,f.status,
f.waiting_reason,f.failure_reason,f.source_version,f.source_sha256,f.size_bytes,f.confirmed_bytes,f.content_id,f.expires_at,
f.media_type,CASE WHEN f.content_state='retained' AND f.expires_at<=clock_timestamp() THEN 'expired' ELSE f.content_state END,
f.parse_status,f.parse_error,f.deleted_at,f.staging_expires_at,f.created_at,f.updated_at`

func (s *Store) CreateTaskFileFetch(ctx context.Context, command app.TaskFileFetchCreate, leaseOwner string) (app.TaskFileFetch, bool, error) {
	value, operation := command.Fetch, command.Operation
	if ctx == nil || !validTaskFileFetchCreate(value, operation) || !validTaskHubLeaseOwner(leaseOwner) {
		return app.TaskFileFetch{}, false, domain.InvalidArgument("file_fetch", "Task file fetch fields are invalid")
	}
	var result app.TaskFileFetch
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockTaskHubOperationLease(ctx, tx, operation, leaseOwner); err != nil {
			return err
		}
		if err := lockTaskHubTarget(ctx, tx, operation); err != nil {
			return err
		}
		storedOperation, err := getTaskHubOperationForUpdate(ctx, tx, operation.OwnerID, operation.TaskID, operation.Key)
		if err != nil {
			return fmt.Errorf("read file.fetch operation before creating fetch record: %w", err)
		}
		if !sameTaskHubOperationRequest(storedOperation, operation) {
			return idempotencyConflict()
		}
		storedFetch, err := getTaskFileFetchForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.OperationKey)
		if err == nil {
			if !sameTaskFileFetchRequest(storedFetch, value, operation) {
				return idempotencyConflict()
			}
			result, replayed = storedFetch, true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read file.fetch record before create: %w", err)
		}
		var activeCount int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_file_fetches
WHERE owner_id=$1 AND task_id=$2 AND (status<>'completed' OR expires_at>clock_timestamp())`, value.OwnerID, value.TaskID).Scan(&activeCount); err != nil {
			return fmt.Errorf("count Task file fetches: %w", err)
		}
		if activeCount >= app.TaskFileFetchMaxPerTask {
			return domain.NewError(domain.CodeQuotaExceeded, "Task file fetch limit has been reached")
		}
		_, err = tx.Exec(ctx, `INSERT INTO task_file_fetches (
fetch_id,task_id,owner_id,operation_key,source_installation_id,target_grant_version,
task_authorization_version,task_revision,relative_path,request_hash,status
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'queued')
ON CONFLICT (owner_id,task_id,operation_key) DO NOTHING`, value.ID, value.TaskID, value.OwnerID, value.OperationKey,
			value.SourceInstallationID, value.TargetGrantVersion, value.TaskAuthorizationVersion, value.TaskRevision,
			value.RelativePath, operation.RequestHash)
		if err != nil {
			return fmt.Errorf("create Task file fetch record: %w", err)
		}
		result, err = getTaskFileFetchForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.OperationKey)
		if err != nil {
			return fmt.Errorf("read created Task file fetch record: %w", err)
		}
		if !sameTaskFileFetchRequest(result, value, operation) {
			return idempotencyConflict()
		}
		replayed = result.ID != value.ID
		return nil
	})
	return result, replayed, err
}

func (s *Store) DispatchTaskFileFetch(ctx context.Context, operation app.TaskHubOperation, fetchID string, ticketHash []byte,
	ticketExpiresAt *time.Time, targetOnline bool, leaseOwner string) (bool, error) {
	if ctx == nil || operation.Action != hub.ActionFileFetch || !validTaskHubOperationState(operation) || !domain.ValidOperationID(fetchID) ||
		!validTaskHubLeaseOwner(leaseOwner) || (targetOnline && (len(ticketHash) != 32 || ticketExpiresAt == nil ||
		!ticketExpiresAt.After(time.Now()) || ticketExpiresAt.After(time.Now().Add(app.TaskFileFetchTicketLifetime+time.Second)))) ||
		(!targetOnline && (len(ticketHash) != 0 || ticketExpiresAt != nil)) {
		return false, domain.InvalidArgument("file_fetch", "Task file fetch dispatch fields are invalid")
	}
	var dispatched bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockTaskHubOperationLease(ctx, tx, operation, leaseOwner); err != nil {
			return err
		}
		if err := lockTaskHubTarget(ctx, tx, operation); err != nil {
			return err
		}
		storedOperation, err := getTaskHubOperationForUpdate(ctx, tx, operation.OwnerID, operation.TaskID, operation.Key)
		if err != nil {
			return fmt.Errorf("read file.fetch operation before dispatch: %w", err)
		}
		if !sameTaskHubOperationRequest(storedOperation, operation) {
			return idempotencyConflict()
		}
		if storedOperation.Status != task.OperationPrepared {
			return nil
		}
		storedFetch, err := getTaskFileFetchByIDForUpdate(ctx, tx, operation.OwnerID, operation.TaskID, fetchID)
		if err != nil {
			return fmt.Errorf("read file.fetch record before dispatch: %w", err)
		}
		if storedFetch.OperationKey != operation.Key ||
			storedFetch.TaskRevision != operation.TaskRevision || storedFetch.TargetGrantVersion != operation.TargetGrantVersion {
			return domain.NewError(domain.CodeConflict, "Task file fetch changed before Hub dispatch")
		}
		if targetOnline {
			if storedFetch.Status != task.FileFetchQueued && storedFetch.Status != task.FileFetchTransferring && storedFetch.Status != task.FileFetchVerifying {
				return domain.NewError(domain.CodeConflict, "Task file fetch cannot resume in its current state")
			}
			if storedFetch.Status == task.FileFetchTransferring || storedFetch.Status == task.FileFetchVerifying {
				var stagingLive bool
				if err := tx.QueryRow(ctx, `SELECT COALESCE(staging_expires_at>clock_timestamp(),false)
FROM task_file_fetches WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3`, operation.OwnerID, operation.TaskID, fetchID).Scan(&stagingLive); err != nil {
					return fmt.Errorf("check Task file fetch staging before resume: %w", err)
				}
				if !stagingLive {
					return domain.NewError(domain.CodeTransferExpired, "Task file fetch staging has expired")
				}
			}
			command, err := tx.Exec(ctx, `UPDATE task_file_fetches SET ticket_hash=$4,ticket_expires_at=$5,updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 AND status IN ('queued','transferring','verifying')`, operation.OwnerID, operation.TaskID, fetchID, ticketHash, ticketExpiresAt)
			if err != nil {
				return fmt.Errorf("persist Task file fetch dispatch authorization: %w", err)
			}
			if command.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "Task file fetch changed before authorization was stored")
			}
		} else {
			if storedFetch.Status != task.FileFetchQueued {
				return domain.NewError(domain.CodeConflict, "Task file fetch cannot be marked offline after transfer started")
			}
			command, err := tx.Exec(ctx, `UPDATE task_file_fetches SET status='waiting_device',waiting_reason='device_offline',
ticket_hash=NULL,ticket_expires_at=NULL,updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 AND status='queued'`, operation.OwnerID, operation.TaskID, fetchID)
			if err != nil {
				return fmt.Errorf("persist offline Task file fetch wait: %w", err)
			}
			if command.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "Task file fetch changed before offline wait was stored")
			}
		}
		command, err := tx.Exec(ctx, `UPDATE task_hub_operations SET status='dispatched',updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 AND status='prepared'`, operation.OwnerID, operation.TaskID, operation.Key)
		if err != nil {
			return fmt.Errorf("mark Task file fetch dispatched: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "Task file fetch operation changed before dispatch")
		}
		dispatched = true
		return nil
	})
	return dispatched, err
}

func (s *Store) MarkTaskFileFetchWaiting(ctx context.Context, operation app.TaskHubOperation, fetchID, reason string) (app.TaskFileFetch, error) {
	if ctx == nil || operation.Action != hub.ActionFileFetch || !validTaskHubOperationState(operation) ||
		!domain.ValidOperationID(fetchID) || reason != "device_offline" {
		return app.TaskFileFetch{}, domain.InvalidArgument("file_fetch", "Task file fetch wait reason is invalid")
	}
	var result app.TaskFileFetch
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockTaskHubOperationTask(ctx, tx, operation.TaskID, operation.OwnerID); err != nil {
			return err
		}
		storedOperation, err := getTaskHubOperationForUpdate(ctx, tx, operation.OwnerID, operation.TaskID, operation.Key)
		if err != nil {
			return fmt.Errorf("read file.fetch operation before wait: %w", err)
		}
		if !sameTaskHubOperationRequest(storedOperation, operation) || storedOperation.Status != task.OperationDispatched {
			return domain.NewError(domain.CodeConflict, "Task file fetch is not awaiting a device")
		}
		fetch, err := getTaskFileFetchByIDForUpdate(ctx, tx, operation.OwnerID, operation.TaskID, fetchID)
		if err != nil {
			return fmt.Errorf("read file.fetch record before wait: %w", err)
		}
		if fetch.OperationKey != operation.Key {
			return idempotencyConflict()
		}
		if fetch.Status != task.FileFetchQueued {
			result = fetch
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE task_file_fetches SET status='waiting_device',waiting_reason=$4,
ticket_hash=NULL,ticket_expires_at=NULL,updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 AND status='queued'`,
			operation.OwnerID, operation.TaskID, fetchID, reason); err != nil {
			return fmt.Errorf("persist Task file fetch waiting state: %w", err)
		}
		result, err = getTaskFileFetchByIDForUpdate(ctx, tx, operation.OwnerID, operation.TaskID, fetchID)
		return err
	})
	return result, err
}

func (s *Store) FailTaskFileFetch(ctx context.Context, operation app.TaskHubOperation, fetchID, failureReason string) error {
	if ctx == nil || operation.Action != hub.ActionFileFetch || !validTaskHubOperationState(operation) || !domain.ValidOperationID(fetchID) ||
		!task.ValidFileFetchFailureReason(failureReason) {
		return domain.InvalidArgument("file_fetch", "Task file fetch identity is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockTaskHubOperationTask(ctx, tx, operation.TaskID, operation.OwnerID); err != nil {
			return err
		}
		storedOperation, err := getTaskHubOperationForUpdate(ctx, tx, operation.OwnerID, operation.TaskID, operation.Key)
		if err != nil {
			return fmt.Errorf("read file.fetch operation before failure: %w", err)
		}
		if !sameTaskHubOperationRequest(storedOperation, operation) {
			return idempotencyConflict()
		}
		command, err := tx.Exec(ctx, `UPDATE task_file_fetches SET status='failed',waiting_reason=NULL,failure_reason=$5,
ticket_hash=NULL,ticket_expires_at=NULL,staging_expires_at=NULL,confirmed_bytes=0,updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 AND operation_key=$4 AND content_state='none'
AND status IN ('queued','waiting_device','transferring','verifying')`,
			operation.OwnerID, operation.TaskID, fetchID, operation.Key, failureReason)
		if err != nil {
			return fmt.Errorf("persist failed Task file fetch: %w", err)
		}
		if command.RowsAffected() == 1 {
			if _, err := tx.Exec(ctx, `DELETE FROM task_file_fetch_chunks WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3`,
				operation.OwnerID, operation.TaskID, fetchID); err != nil {
				return fmt.Errorf("delete failed Task file fetch staging chunks: %w", err)
			}
		} else {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_file_fetches WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 AND operation_key=$4 AND status='failed' AND failure_reason=$5)`,
				operation.OwnerID, operation.TaskID, fetchID, operation.Key, failureReason).Scan(&exists); err != nil {
				return fmt.Errorf("check failed Task file fetch replay: %w", err)
			}
			if !exists {
				return domain.NewError(domain.CodeConflict, "Task file fetch cannot fail in its current state")
			}
		}
		return nil
	})
}

func (s *Store) GetTaskFileFetch(ctx context.Context, ownerID, taskID, fetchID string) (app.TaskFileFetch, error) {
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidOperationID(fetchID) {
		return app.TaskFileFetch{}, domain.InvalidArgument("file_fetch", "Task file fetch identity is invalid")
	}
	value, err := getTaskFileFetch(ctx, s.pool, ownerID, taskID, fetchID)
	if err != nil {
		return app.TaskFileFetch{}, storeError("get Task file fetch", "Task file fetch", err)
	}
	return value, nil
}

func (s *Store) GetTaskFileFetchByOperation(ctx context.Context, ownerID, taskID, operationKey string) (app.TaskFileFetch, error) {
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !validTaskHubOperationKey(operationKey) {
		return app.TaskFileFetch{}, domain.InvalidArgument("file_fetch", "Task file fetch operation identity is invalid")
	}
	value, err := scanTaskFileFetch(s.pool.QueryRow(ctx, `SELECT `+taskFileFetchProjectionColumns+`
FROM task_file_fetches f JOIN client_installations i ON i.id=f.source_installation_id AND i.owner_id=f.owner_id
WHERE f.owner_id=$1 AND f.task_id=$2 AND f.operation_key=$3`, ownerID, taskID, operationKey))
	if err != nil {
		return app.TaskFileFetch{}, storeError("get Task file fetch by operation", "Task file fetch", err)
	}
	return value, nil
}

func getTaskFileFetch(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, ownerID, taskID, fetchID string) (app.TaskFileFetch, error) {
	return scanTaskFileFetch(q.QueryRow(ctx, `SELECT `+taskFileFetchProjectionColumns+`
FROM task_file_fetches f JOIN client_installations i ON i.id=f.source_installation_id AND i.owner_id=f.owner_id
WHERE f.owner_id=$1 AND f.task_id=$2 AND f.fetch_id=$3`, ownerID, taskID, fetchID))
}

func getTaskFileFetchForUpdate(ctx context.Context, tx pgx.Tx, ownerID, taskID, operationKey string) (app.TaskFileFetch, error) {
	return scanTaskFileFetch(tx.QueryRow(ctx, `SELECT `+taskFileFetchProjectionColumns+`
FROM task_file_fetches f JOIN client_installations i ON i.id=f.source_installation_id AND i.owner_id=f.owner_id
WHERE f.owner_id=$1 AND f.task_id=$2 AND f.operation_key=$3 FOR UPDATE OF f`, ownerID, taskID, operationKey))
}

func getTaskFileFetchByIDForUpdate(ctx context.Context, tx pgx.Tx, ownerID, taskID, fetchID string) (app.TaskFileFetch, error) {
	return scanTaskFileFetch(tx.QueryRow(ctx, `SELECT `+taskFileFetchProjectionColumns+`
FROM task_file_fetches f JOIN client_installations i ON i.id=f.source_installation_id AND i.owner_id=f.owner_id
WHERE f.owner_id=$1 AND f.task_id=$2 AND f.fetch_id=$3 FOR UPDATE OF f`, ownerID, taskID, fetchID))
}

func scanTaskFileFetch(row pgx.Row) (app.TaskFileFetch, error) {
	var value app.TaskFileFetch
	var waitingReason, failureReason, sourceVersion, sourceSHA256, contentID, mediaType, contentState, parseError pgtype.Text
	var sizeBytes pgtype.Int8
	var expiresAt, deletedAt, stagingExpiresAt pgtype.Timestamptz
	var status, parseStatus string
	err := row.Scan(&value.ID, &value.OwnerID, &value.TaskID, &value.OperationKey, &value.SourceInstallationID,
		&value.SourceDeviceName, &value.SourceDirectoryName, &value.TargetGrantVersion, &value.TaskAuthorizationVersion, &value.TaskRevision, &value.RelativePath,
		&status, &waitingReason, &failureReason, &sourceVersion, &sourceSHA256, &sizeBytes, &value.ConfirmedBytes, &contentID, &expiresAt,
		&mediaType, &contentState, &parseStatus, &parseError, &deletedAt, &stagingExpiresAt, &value.CreatedAt, &value.UpdatedAt)
	if err != nil {
		return app.TaskFileFetch{}, err
	}
	value.Status = task.FileFetchStatus(status)
	if waitingReason.Valid {
		value.WaitingReason = waitingReason.String
	}
	if failureReason.Valid {
		value.FailureReason = failureReason.String
	}
	if sourceVersion.Valid {
		value.SourceVersion = sourceVersion.String
	}
	if sourceSHA256.Valid {
		value.SourceSHA256 = sourceSHA256.String
	}
	if sizeBytes.Valid {
		value.SizeBytes = sizeBytes.Int64
	}
	if contentID.Valid {
		value.ContentID = contentID.String
	}
	if expiresAt.Valid {
		expiry := expiresAt.Time.UTC()
		value.ExpiresAt = &expiry
	}
	if mediaType.Valid {
		value.MediaType = mediaType.String
	}
	if contentState.Valid {
		value.ContentState = task.FileFetchContentState(contentState.String)
	}
	value.ParseStatus = task.FileFetchParseStatus(parseStatus)
	if parseError.Valid {
		value.ParseError = parseError.String
	}
	if deletedAt.Valid {
		deleted := deletedAt.Time.UTC()
		value.DeletedAt = &deleted
	}
	if stagingExpiresAt.Valid {
		expiry := stagingExpiresAt.Time.UTC()
		value.StagingExpiresAt = &expiry
	}
	return value, nil
}

func validTaskFileFetchCreate(value app.TaskFileFetch, operation app.TaskHubOperation) bool {
	return domain.ValidOperationID(value.ID) && domain.ValidUserID(value.OwnerID) && domain.ValidTaskID(value.TaskID) &&
		value.OperationKey == operation.Key && operation.Action == hub.ActionFileFetch && value.OwnerID == operation.OwnerID &&
		value.TaskID == operation.TaskID && value.SourceInstallationID == operation.TargetInstallationID &&
		value.TargetGrantVersion == operation.TargetGrantVersion && value.TaskAuthorizationVersion == operation.TaskAuthorizationVersion &&
		value.TaskRevision == operation.TaskRevision && value.TaskRevision > 0 && workspace.ValidateRelativePath(value.RelativePath) == nil &&
		value.Status == task.FileFetchQueued && value.WaitingReason == "" && value.SourceVersion == "" && value.SourceSHA256 == "" &&
		value.SizeBytes == 0 && value.ConfirmedBytes == 0 && value.ContentID == "" && value.ExpiresAt == nil
}

func sameTaskFileFetchRequest(stored, requested app.TaskFileFetch, operation app.TaskHubOperation) bool {
	return stored.ID != "" && stored.OwnerID == requested.OwnerID && stored.TaskID == requested.TaskID &&
		stored.OperationKey == requested.OperationKey && stored.SourceInstallationID == requested.SourceInstallationID &&
		stored.TargetGrantVersion == requested.TargetGrantVersion && stored.TaskAuthorizationVersion == requested.TaskAuthorizationVersion &&
		stored.TaskRevision == requested.TaskRevision && stored.RelativePath == requested.RelativePath &&
		operation.Action == hub.ActionFileFetch && operation.TaskRevision == requested.TaskRevision
}
