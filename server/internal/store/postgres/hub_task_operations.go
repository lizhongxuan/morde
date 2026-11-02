package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) PrepareTaskHubOperation(ctx context.Context, value app.TaskHubOperation, leaseOwner string) (app.TaskHubOperation, bool, error) {
	if !validTaskHubOperation(value) || value.Status != task.OperationPrepared || len(value.ResultHash) != 0 || !validTaskHubLeaseOwner(leaseOwner) {
		return app.TaskHubOperation{}, false, domain.InvalidArgument("operation", "Hub Task operation fields are invalid")
	}
	var result app.TaskHubOperation
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockTaskHubOperationLease(ctx, tx, value, leaseOwner); err != nil {
			return err
		}
		if err := lockTaskHubTarget(ctx, tx, value); err != nil {
			return err
		}
		stored, err := getTaskHubOperationForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.Key)
		if err == nil {
			if !sameTaskHubOperationRequest(stored, value) {
				return idempotencyConflict()
			}
			if stored.Status == task.OperationPrepared && stored.LeaseVersion != value.LeaseVersion {
				if err := tx.QueryRow(ctx, `UPDATE task_hub_operations SET lease_version=$4, updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 AND status='prepared' RETURNING lease_version`,
					value.OwnerID, value.TaskID, value.Key, value.LeaseVersion).Scan(&stored.LeaseVersion); err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						return domain.NewError(domain.CodeConflict, "prepared Hub Task operation changed before lease adoption")
					}
					return fmt.Errorf("adopt prepared Hub Task operation lease: %w", err)
				}
			}
			result, replayed = stored, true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read Hub Task operation before prepare: %w", err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO task_hub_operations (
task_id, owner_id, operation_key, tool_name, target_installation_id, action,
task_authorization_version, target_grant_version, lease_version, request_hash, status
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'prepared')
ON CONFLICT (owner_id, task_id, operation_key) DO NOTHING`,
			value.TaskID, value.OwnerID, value.Key, value.ToolName, value.TargetInstallationID, value.Action,
			value.TaskAuthorizationVersion, value.TargetGrantVersion, value.LeaseVersion, value.RequestHash)
		if err != nil {
			return fmt.Errorf("prepare Hub Task operation: %w", err)
		}
		stored, err = getTaskHubOperationForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.Key)
		if err != nil {
			return fmt.Errorf("read prepared Hub Task operation: %w", err)
		}
		if !sameTaskHubOperationRequest(stored, value) {
			return idempotencyConflict()
		}
		result, replayed = stored, stored.Status != task.OperationPrepared || stored.LeaseVersion != value.LeaseVersion
		return nil
	})
	return result, replayed, err
}

func (s *Store) MarkTaskHubOperationDispatched(ctx context.Context, value app.TaskHubOperation, leaseOwner string) error {
	if !validTaskHubOperation(value) || !validTaskHubLeaseOwner(leaseOwner) {
		return domain.InvalidArgument("operation", "Hub Task operation fields are invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		row, err := lockTaskHubOperationLease(ctx, tx, value, leaseOwner)
		if err != nil {
			return err
		}
		if err := lockTaskHubTarget(ctx, tx, value); err != nil {
			return err
		}
		stored, err := getTaskHubOperationForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.Key)
		if err != nil {
			return fmt.Errorf("read Hub Task operation before dispatch: %w", err)
		}
		if !sameTaskHubOperationRequest(stored, value) || stored.LeaseVersion != value.LeaseVersion || row.LeaseVersion != value.LeaseVersion {
			return idempotencyConflict()
		}
		if stored.Status != task.OperationPrepared {
			return domain.NewError(domain.CodeConflict, "Hub Task operation is no longer prepared")
		}
		command, err := tx.Exec(ctx, `UPDATE task_hub_operations SET status='dispatched', updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 AND lease_version=$4 AND status='prepared'`,
			value.OwnerID, value.TaskID, value.Key, value.LeaseVersion)
		if err != nil {
			return fmt.Errorf("mark Hub Task operation dispatched: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "Hub Task operation changed before dispatch")
		}
		return nil
	})
}

func (s *Store) CompleteTaskHubOperation(ctx context.Context, value app.TaskHubOperation, leaseOwner string,
	status task.OperationStatus, resultHash []byte) error {
	if !validTaskHubOperation(value) || !validTaskHubLeaseOwner(leaseOwner) ||
		(status != task.OperationSucceeded && status != task.OperationFailed) || len(resultHash) != 32 {
		return domain.InvalidArgument("operation", "Hub Task operation result is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		row, err := lockTaskHubOperationLease(ctx, tx, value, leaseOwner)
		if err != nil {
			return err
		}
		if err := lockTaskHubTarget(ctx, tx, value); err != nil {
			return err
		}
		stored, err := getTaskHubOperationForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.Key)
		if err != nil {
			return fmt.Errorf("read Hub Task operation before completion: %w", err)
		}
		if !sameTaskHubOperationRequest(stored, value) || stored.LeaseVersion != value.LeaseVersion || row.LeaseVersion != value.LeaseVersion {
			return idempotencyConflict()
		}
		if stored.Status == status && bytes.Equal(stored.ResultHash, resultHash) {
			return nil
		}
		beforeDispatchFailure := stored.Status == task.OperationPrepared && status == task.OperationFailed
		if (!beforeDispatchFailure && stored.Status != task.OperationDispatched) || !task.CanTransitionOperation(stored.Status, status) {
			return domain.NewError(domain.CodeConflict, "Hub Task operation cannot accept this result")
		}
		command, err := tx.Exec(ctx, `UPDATE task_hub_operations SET status=$4, result_hash=$5, updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 AND lease_version=$6 AND status=$7`,
			value.OwnerID, value.TaskID, value.Key, string(status), resultHash, value.LeaseVersion, string(stored.Status))
		if err != nil {
			return fmt.Errorf("persist Hub Task operation result hash: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "Hub Task operation changed before completion")
		}
		return nil
	})
}

func (s *Store) MarkTaskHubOperationUnknown(ctx context.Context, value app.TaskHubOperation, leaseOwner string) error {
	if !validTaskHubOperation(value) || !validTaskHubLeaseOwner(leaseOwner) {
		return domain.InvalidArgument("operation", "Hub Task operation fields are invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockTaskHubOperationTask(ctx, tx, value.TaskID, value.OwnerID); err != nil {
			return err
		}
		stored, err := getTaskHubOperationForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.Key)
		if err != nil {
			return fmt.Errorf("read Hub Task operation before unknown settlement: %w", err)
		}
		if !sameTaskHubOperationRequest(stored, value) {
			return idempotencyConflict()
		}
		if stored.Status == task.OperationUnknown {
			return nil
		}
		if stored.Status != task.OperationDispatched || !task.CanTransitionOperation(stored.Status, task.OperationUnknown) {
			return domain.NewError(domain.CodeConflict, "Hub Task operation is not dispatched")
		}
		command, err := tx.Exec(ctx, `UPDATE task_hub_operations SET status='unknown', updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 AND status='dispatched'`, value.OwnerID, value.TaskID, value.Key)
		if err != nil {
			return fmt.Errorf("persist unknown Hub Task operation: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "Hub Task operation changed before unknown settlement")
		}
		return nil
	})
}

func (s *Store) GetTaskHubOperation(ctx context.Context, ownerID, taskID, key string) (app.TaskHubOperation, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !validTaskHubOperationKey(key) {
		return app.TaskHubOperation{}, domain.InvalidArgument("operation", "Hub Task operation identity is invalid")
	}
	var result app.TaskHubOperation
	err := s.pool.QueryRow(ctx, `SELECT task_id, owner_id, operation_key, tool_name, target_installation_id, action,
task_authorization_version, target_grant_version, lease_version, request_hash, result_hash, status
FROM task_hub_operations WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3`, ownerID, taskID, key).Scan(
		&result.TaskID, &result.OwnerID, &result.Key, &result.ToolName, &result.TargetInstallationID, &result.Action,
		&result.TaskAuthorizationVersion, &result.TargetGrantVersion, &result.LeaseVersion, &result.RequestHash,
		&result.ResultHash, &result.Status)
	if err != nil {
		return app.TaskHubOperation{}, storeError("get Hub Task operation", "Hub Task operation", err)
	}
	result.RequestHash = append([]byte(nil), result.RequestHash...)
	result.ResultHash = append([]byte(nil), result.ResultHash...)
	return result, nil
}

func lockTaskHubOperationLease(ctx context.Context, tx pgx.Tx, value app.TaskHubOperation, leaseOwner string) (sqlcgen.Task, error) {
	gate, err := requireOwnerExecutionEnabled(ctx, tx, value.OwnerID, nil)
	if err != nil {
		return sqlcgen.Task{}, err
	}
	if err := lockTaskHubOperationTask(ctx, tx, value.TaskID, value.OwnerID); err != nil {
		return sqlcgen.Task{}, err
	}
	row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: value.TaskID, OwnerID: value.OwnerID})
	if err != nil {
		return sqlcgen.Task{}, fmt.Errorf("read Hub Task operation task: %w", err)
	}
	if row.ExecutionLocation != "server" || row.ExecutionEpoch != gate.epoch || row.AuthVersion != value.TaskAuthorizationVersion {
		return sqlcgen.Task{}, domain.NewError(domain.CodeUnauthenticated, "Hub Task authorization changed")
	}
	if err := requireCurrentLease(ctx, tx, row, leaseOwner, value.LeaseVersion); err != nil {
		return sqlcgen.Task{}, err
	}
	authorized, err := requireCurrentTaskAuthorization(ctx, tx, row)
	if err != nil {
		return sqlcgen.Task{}, err
	}
	if !authorized {
		return sqlcgen.Task{}, domain.NewError(domain.CodeUnauthenticated, "Hub Task authorization is unavailable")
	}
	return row, nil
}

func lockTaskHubOperationTask(ctx context.Context, tx pgx.Tx, taskID, ownerID string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, taskID, ownerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return missingTaskError()
	}
	if err != nil {
		return fmt.Errorf("lock task for Hub operation: %w", err)
	}
	return nil
}

func lockTaskHubTarget(ctx context.Context, tx pgx.Tx, value app.TaskHubOperation) error {
	var ownerID string
	var active bool
	err := tx.QueryRow(ctx, `SELECT owner_id, COALESCE(revoked_at IS NULL AND expires_at > clock_timestamp(), false)
FROM client_installations WHERE id=$1 FOR SHARE`, value.TargetInstallationID).Scan(&ownerID, &active)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (ownerID != value.OwnerID || !active) {
		return domain.NewError(domain.CodeNotFound, "Hub Task target installation is unavailable")
	}
	if err != nil {
		return fmt.Errorf("lock Hub Task target installation: %w", err)
	}
	return nil
}

func getTaskHubOperationForUpdate(ctx context.Context, tx pgx.Tx, ownerID, taskID, key string) (app.TaskHubOperation, error) {
	var result app.TaskHubOperation
	err := tx.QueryRow(ctx, `SELECT task_id, owner_id, operation_key, tool_name, target_installation_id, action,
task_authorization_version, target_grant_version, lease_version, request_hash, result_hash, status
FROM task_hub_operations WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 FOR UPDATE`, ownerID, taskID, key).Scan(
		&result.TaskID, &result.OwnerID, &result.Key, &result.ToolName, &result.TargetInstallationID, &result.Action,
		&result.TaskAuthorizationVersion, &result.TargetGrantVersion, &result.LeaseVersion, &result.RequestHash,
		&result.ResultHash, &result.Status)
	result.RequestHash = append([]byte(nil), result.RequestHash...)
	result.ResultHash = append([]byte(nil), result.ResultHash...)
	return result, err
}

func validTaskHubOperation(value app.TaskHubOperation) bool {
	return domain.ValidUserID(value.OwnerID) && domain.ValidTaskID(value.TaskID) && validTaskHubOperationKey(value.Key) &&
		value.ToolName != "" && len(value.ToolName) <= 128 && utf8.ValidString(value.ToolName) && !strings.ContainsRune(value.ToolName, '\x00') &&
		domain.ValidClientInstallationID(value.TargetInstallationID) &&
		validTaskHubAction(value.Action) && value.TaskAuthorizationVersion > 0 && value.TargetGrantVersion > 0 &&
		value.LeaseVersion > 0 && len(value.RequestHash) == 32
}

func validTaskHubAction(action string) bool {
	switch action {
	case hub.ActionScreenshot, hub.ActionFiles, hub.ActionFileGet, hub.ActionCommand:
		return true
	default:
		return false
	}
}

func validTaskHubOperationKey(value string) bool {
	return value != "" && len(value) <= 512 && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00') && strings.TrimSpace(value) == value
}

func validTaskHubLeaseOwner(value string) bool {
	return value != "" && len(value) <= 255 && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00') && strings.TrimSpace(value) == value
}

func sameTaskHubOperationRequest(stored, requested app.TaskHubOperation) bool {
	return stored.OwnerID == requested.OwnerID && stored.TaskID == requested.TaskID && stored.Key == requested.Key &&
		stored.ToolName == requested.ToolName && stored.TargetInstallationID == requested.TargetInstallationID &&
		stored.Action == requested.Action && stored.TaskAuthorizationVersion == requested.TaskAuthorizationVersion &&
		stored.TargetGrantVersion == requested.TargetGrantVersion && bytes.Equal(stored.RequestHash, requested.RequestHash)
}

var _ app.TaskHubOperationStore = (*Store)(nil)
