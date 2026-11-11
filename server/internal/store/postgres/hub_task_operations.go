package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) PrepareTaskHubOperation(ctx context.Context, value app.TaskHubOperation, leaseOwner string) (app.TaskHubOperation, bool, error) {
	if !validTaskHubOperationState(value) || value.Status != task.OperationPrepared || len(value.ResultHash) != 0 ||
		value.Action == hub.ActionFilePut && (value.SyncStage != app.TaskHubWriteStagePending || len(value.LocalReceiptHash) != 0 || value.SyncSessionID != "" || value.SyncRevision != 0) ||
		!validTaskHubLeaseOwner(leaseOwner) {
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
			if stored.Status == task.OperationUnknown && stored.Action == hub.ActionFileFetch {
				var fetchStatus string
				err := tx.QueryRow(ctx, `SELECT status FROM task_file_fetches
WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 FOR UPDATE`, value.OwnerID, value.TaskID, value.Key).Scan(&fetchStatus)
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.NewError(domain.CodeConflict, "unknown file.fetch operation has no durable fetch record")
				}
				if err != nil {
					return fmt.Errorf("lock file.fetch record for safe retry: %w", err)
				}
				if fetchStatus != string(task.FileFetchQueued) && fetchStatus != string(task.FileFetchTransferring) && fetchStatus != string(task.FileFetchVerifying) {
					result, replayed = stored, true
					return nil
				}
				if err := tx.QueryRow(ctx, `UPDATE task_hub_operations SET status='prepared',lease_version=$4,updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 AND action='file.fetch' AND status='unknown'
RETURNING status,lease_version`, value.OwnerID, value.TaskID, value.Key, value.LeaseVersion).
					Scan(&stored.Status, &stored.LeaseVersion); err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						return domain.NewError(domain.CodeConflict, "unknown file.fetch operation changed before safe retry")
					}
					return fmt.Errorf("prepare safe file.fetch retry: %w", err)
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
task_authorization_version, target_grant_version, task_revision, lease_version, request_hash,
workspace_id, workspace_path, workspace_size_bytes, workspace_sha256, source_checkpoint_revision, sync_stage, status
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,'prepared')
ON CONFLICT (owner_id, task_id, operation_key) DO NOTHING`,
			value.TaskID, value.OwnerID, value.Key, value.ToolName, value.TargetInstallationID, value.Action,
			value.TaskAuthorizationVersion, value.TargetGrantVersion, value.TaskRevision, value.LeaseVersion, value.RequestHash,
			nullableString(value.WorkspaceID), nullableString(value.WorkspacePath), nullableInt64(value.Action == hub.ActionFilePut, value.WorkspaceSizeBytes),
			nullableString(value.WorkspaceSHA256), nullableInt64(value.Action == hub.ActionFilePut, value.SourceCheckpointRevision), value.SyncStage)
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
	if !validTaskHubOperationState(value) || !validTaskHubLeaseOwner(leaseOwner) {
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
	if !validTaskHubOperationState(value) || !validTaskHubLeaseOwner(leaseOwner) ||
		(status != task.OperationSucceeded && status != task.OperationFailed) || len(resultHash) != 32 {
		return domain.InvalidArgument("operation", "Hub Task operation result is invalid")
	}
	if value.Action == hub.ActionFilePut && status == task.OperationSucceeded {
		return domain.NewError(domain.CodeConflict, "file.put requires a verified Workspace sync commit")
	}
	if value.Action == hub.ActionFileFetch && status == task.OperationSucceeded {
		return s.completeTaskFileFetchHubOperation(ctx, value, status, resultHash)
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
		if stored.Action == hub.ActionFilePut && stored.SyncStage != app.TaskHubWriteStagePending {
			return domain.NewError(domain.CodeConflict, "file.put cannot fail after its local receipt was recorded")
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

func (s *Store) completeTaskFileFetchHubOperation(ctx context.Context, value app.TaskHubOperation,
	status task.OperationStatus, resultHash []byte) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockTaskHubOperationTask(ctx, tx, value.TaskID, value.OwnerID); err != nil {
			return err
		}
		stored, err := getTaskHubOperationForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.Key)
		if err != nil {
			return fmt.Errorf("read file.fetch operation before durable completion: %w", err)
		}
		if !sameTaskHubOperationRequest(stored, value) || stored.Action != hub.ActionFileFetch {
			return idempotencyConflict()
		}
		if stored.Status == status && bytes.Equal(stored.ResultHash, resultHash) {
			return nil
		}
		if stored.Status != task.OperationPrepared && stored.Status != task.OperationDispatched && stored.Status != task.OperationUnknown {
			return domain.NewError(domain.CodeConflict, "file.fetch operation cannot accept a durable completion")
		}
		var fetchStatus, contentState string
		if err := tx.QueryRow(ctx, `SELECT status,content_state FROM task_file_fetches
WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 FOR UPDATE`, value.OwnerID, value.TaskID, value.Key).
			Scan(&fetchStatus, &contentState); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeConflict, "file.fetch completion has no durable fetch record")
			}
			return fmt.Errorf("lock file.fetch record before durable completion: %w", err)
		}
		if fetchStatus != string(task.FileFetchCompleted) && fetchStatus != string(task.FileFetchWaitingDevice) {
			return domain.NewError(domain.CodeConflict, "file.fetch completion is not backed by a completed or offline fetch")
		}
		if fetchStatus == string(task.FileFetchCompleted) && contentState == string(task.FileFetchContentNone) {
			return domain.NewError(domain.CodeConflict, "completed file.fetch has no retained content receipt")
		}
		// A prepared operation can be completed only from an already committed
		// fetch row; this is recovery of a durable result, not a Hub replay.
		command, err := tx.Exec(ctx, `UPDATE task_hub_operations SET status='succeeded',result_hash=$4,updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 AND status=$5`, value.OwnerID, value.TaskID, value.Key,
			resultHash, string(stored.Status))
		if err != nil {
			return fmt.Errorf("persist durable file.fetch operation result: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "file.fetch operation changed before durable completion")
		}
		return nil
	})
}

func (s *Store) RecordTaskHubOperationLocalReceipt(ctx context.Context, value app.TaskHubOperation, leaseOwner string, receiptHash []byte) error {
	if !validTaskHubOperationState(value) || value.Action != hub.ActionFilePut || !validTaskHubLeaseOwner(leaseOwner) || len(receiptHash) != 32 {
		return domain.InvalidArgument("operation", "file.put local receipt is invalid")
	}
	boundReceiptHash, err := taskHubFilePutLocalReceiptHash(value, receiptHash)
	if err != nil {
		return fmt.Errorf("hash bound file.put local receipt: %w", err)
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		row, err := lockTaskHubOperationLease(ctx, tx, value, leaseOwner)
		if err != nil {
			return err
		}
		stored, err := getTaskHubOperationForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.Key)
		if err != nil {
			return fmt.Errorf("read file.put operation before local receipt: %w", err)
		}
		if !sameTaskHubOperationRequest(stored, value) || stored.LeaseVersion != value.LeaseVersion || row.LeaseVersion != value.LeaseVersion {
			return idempotencyConflict()
		}
		if stored.SyncStage != app.TaskHubWriteStagePending {
			if (stored.SyncStage == app.TaskHubWriteStageLocal || stored.SyncStage == app.TaskHubWriteStageSyncPending ||
				stored.SyncStage == app.TaskHubWriteStageCloudSynced) && bytes.Equal(stored.LocalReceiptHash, boundReceiptHash[:]) {
				return nil
			}
			return domain.NewError(domain.CodeConflict, "file.put local receipt does not match the recorded receipt")
		}
		if stored.Status != task.OperationDispatched && stored.Status != task.OperationUnknown {
			return domain.NewError(domain.CodeConflict, "file.put operation must be dispatched before recording a local receipt")
		}
		command, err := tx.Exec(ctx, `UPDATE task_hub_operations
SET local_receipt_hash=$4, sync_stage='local_applied', updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 AND status IN ('dispatched','unknown') AND sync_stage='pending'`,
			value.OwnerID, value.TaskID, value.Key, boundReceiptHash[:])
		if err != nil {
			return fmt.Errorf("persist file.put local receipt hash: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "file.put operation changed before local receipt")
		}
		return nil
	})
}

// lockTaskHubOperationWorkspaceSyncCreateTx establishes the device -> Task ->
// Task operation -> Workspace lock order for a Task-linked upload session.
func lockTaskHubOperationWorkspaceSyncCreateTx(ctx context.Context, tx pgx.Tx, ownerID, taskID, operationKey,
	workspaceID, targetInstallationID string, baseRevision int64, scope []string) error {
	if tx == nil || !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !validTaskHubOperationKey(operationKey) ||
		!domain.ValidWorkspaceID(workspaceID) || !domain.ValidClientInstallationID(targetInstallationID) || baseRevision < 0 || len(scope) != 1 {
		return domain.InvalidArgument("operation", "Task file.put sync request is invalid")
	}
	if err := lockTaskHubOperationTask(ctx, tx, taskID, ownerID); err != nil {
		return err
	}
	var op app.TaskHubOperation
	var localReceiptHash []byte
	var syncSessionID pgtype.Text
	err := tx.QueryRow(ctx, `SELECT target_installation_id, action, workspace_id, workspace_path, workspace_size_bytes,
workspace_sha256, source_checkpoint_revision, local_receipt_hash, sync_stage, sync_session_id, status, task_authorization_version
FROM task_hub_operations WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 FOR UPDATE`, ownerID, taskID, operationKey).
		Scan(&op.TargetInstallationID, &op.Action, &op.WorkspaceID, &op.WorkspacePath, &op.WorkspaceSizeBytes,
			&op.WorkspaceSHA256, &op.SourceCheckpointRevision, &localReceiptHash, &op.SyncStage, &syncSessionID, &op.Status, &op.TaskAuthorizationVersion)
	if err != nil {
		return storeError("lock Task file.put operation before Workspace sync create", "Hub Task operation", err)
	}
	op.LocalReceiptHash = append([]byte(nil), localReceiptHash...)
	if syncSessionID.Valid {
		op.SyncSessionID = syncSessionID.String
	}
	writeStateReady := (op.SyncStage == app.TaskHubWriteStageLocal || op.SyncStage == app.TaskHubWriteStageSyncPending) &&
		(op.Status == task.OperationDispatched || op.Status == task.OperationUnknown)
	writeStateSettled := op.SyncStage == app.TaskHubWriteStageCloudSynced && op.Status == task.OperationSucceeded
	if op.Action != hub.ActionFilePut || op.WorkspaceID != workspaceID || op.TargetInstallationID != targetInstallationID ||
		op.SourceCheckpointRevision != baseRevision || op.WorkspacePath != scope[0] || len(op.LocalReceiptHash) != 32 ||
		(!writeStateReady && !writeStateSettled) {
		return domain.NewError(domain.CodeWorkspaceConflict, "Task file.put operation does not match the Workspace sync request")
	}
	var taskWorkspaceID pgtype.Text
	var checkpointRevision pgtype.Int8
	if err := tx.QueryRow(ctx, `SELECT workspace_id, workspace_checkpoint_revision FROM tasks WHERE id=$1 AND owner_id=$2`, taskID, ownerID).
		Scan(&taskWorkspaceID, &checkpointRevision); err != nil {
		return fmt.Errorf("verify Task checkpoint before Workspace sync create: %w", err)
	}
	if !taskWorkspaceID.Valid || !checkpointRevision.Valid || taskWorkspaceID.String != workspaceID || checkpointRevision.Int64 != baseRevision {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace sync request does not use the Task source checkpoint")
	}
	if writeStateSettled {
		return nil
	}
	if err := requireTaskHubWorkspaceWriteAuthorization(ctx, tx, ownerID, taskID, op.TaskAuthorizationVersion); err != nil {
		return err
	}
	return nil
}

// lockTaskHubOperationWorkspaceSyncCommitTx locks the explicitly linked write
// operation before the caller acquires the Workspace row lock.
func lockTaskHubOperationWorkspaceSyncCommitTx(ctx context.Context, tx pgx.Tx, workspaceID, sessionID, targetInstallationID string) error {
	if tx == nil || !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) ||
		!domain.ValidClientInstallationID(targetInstallationID) {
		return domain.InvalidArgument("sync_session", "Task file.put commit identity is invalid")
	}
	var ownerID, replicaID string
	var taskID, operationKey pgtype.Text
	var baseRevision int64
	var scope []string
	if err := tx.QueryRow(ctx, `SELECT owner_id, replica_id, task_id, task_hub_operation_key, base_revision, scope
FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2`, workspaceID, sessionID).
		Scan(&ownerID, &replicaID, &taskID, &operationKey, &baseRevision, &scope); err != nil {
		return storeError("read Task link before Workspace sync commit lock", "workspace sync session", err)
	}
	if !taskID.Valid && !operationKey.Valid {
		return nil
	}
	if !taskID.Valid || !operationKey.Valid || !strings.HasPrefix(replicaID, "device:") || len(scope) != 1 {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace sync Task operation link is invalid")
	}
	if err := lockTaskHubOperationTask(ctx, tx, taskID.String, ownerID); err != nil {
		return err
	}
	var op app.TaskHubOperation
	var localReceiptHash []byte
	var syncSessionID pgtype.Text
	err := tx.QueryRow(ctx, `SELECT target_installation_id, action, workspace_id, workspace_path, workspace_size_bytes,
workspace_sha256, source_checkpoint_revision, local_receipt_hash, sync_stage, sync_session_id, status, task_authorization_version
FROM task_hub_operations WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 FOR UPDATE`, ownerID, taskID.String, operationKey.String).
		Scan(&op.TargetInstallationID, &op.Action, &op.WorkspaceID, &op.WorkspacePath, &op.WorkspaceSizeBytes,
			&op.WorkspaceSHA256, &op.SourceCheckpointRevision, &localReceiptHash, &op.SyncStage, &syncSessionID, &op.Status, &op.TaskAuthorizationVersion)
	if err != nil {
		return storeError("lock Task file.put operation before Workspace sync commit", "Hub Task operation", err)
	}
	op.LocalReceiptHash = append([]byte(nil), localReceiptHash...)
	if syncSessionID.Valid {
		op.SyncSessionID = syncSessionID.String
	}
	if op.Action != hub.ActionFilePut || op.WorkspaceID != workspaceID || op.TargetInstallationID != targetInstallationID ||
		op.WorkspacePath != scope[0] || op.SourceCheckpointRevision != baseRevision || op.SyncSessionID != sessionID ||
		len(op.LocalReceiptHash) != 32 ||
		(op.SyncStage != app.TaskHubWriteStageSyncPending && op.SyncStage != app.TaskHubWriteStageCloudSynced) {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace sync session does not match its Task file.put operation")
	}
	if op.SyncStage == app.TaskHubWriteStageCloudSynced && op.Status == task.OperationSucceeded {
		return nil
	}
	var taskWorkspaceID pgtype.Text
	var checkpointRevision pgtype.Int8
	if err := tx.QueryRow(ctx, `SELECT workspace_id, workspace_checkpoint_revision FROM tasks WHERE id=$1 AND owner_id=$2`, taskID.String, ownerID).
		Scan(&taskWorkspaceID, &checkpointRevision); err != nil {
		return fmt.Errorf("verify Task checkpoint before Workspace sync commit: %w", err)
	}
	if !taskWorkspaceID.Valid || !checkpointRevision.Valid || taskWorkspaceID.String != workspaceID || checkpointRevision.Int64 != baseRevision {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace sync session does not use the Task source checkpoint")
	}
	if err := requireTaskHubWorkspaceWriteAuthorization(ctx, tx, ownerID, taskID.String, op.TaskAuthorizationVersion); err != nil {
		return err
	}
	return nil
}

func requireTaskHubWorkspaceWriteAuthorization(ctx context.Context, tx pgx.Tx, ownerID, taskID string, authorizationVersion int64) error {
	row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: taskID, OwnerID: ownerID})
	if err != nil {
		return fmt.Errorf("read Task authorization before file.put sync: %w", err)
	}
	if row.ExecutionLocation != "server" || row.AuthVersion != authorizationVersion {
		return domain.NewError(domain.CodeUnauthenticated, "file.put Task authorization changed")
	}
	current, err := requireCurrentTaskAuthorization(ctx, tx, row)
	if err != nil {
		return err
	}
	if !current {
		return domain.NewError(domain.CodeUnauthenticated, "file.put Task authorization is unavailable")
	}
	return nil
}

// associateTaskHubOperationWorkspaceSyncSessionTx links an explicitly named
// file.put operation to the device upload session that carries its exact file.
// Call it after the session row and upload manifest are inserted, in that same tx.
func associateTaskHubOperationWorkspaceSyncSessionTx(ctx context.Context, tx pgx.Tx, ownerID, taskID, operationKey,
	workspaceID, targetInstallationID, sessionID string) error {
	if tx == nil || !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !validTaskHubOperationKey(operationKey) ||
		!domain.ValidWorkspaceID(workspaceID) || !domain.ValidClientInstallationID(targetInstallationID) || !workspace.ValidSessionID(sessionID) {
		return domain.InvalidArgument("operation", "file.put Workspace sync association is invalid")
	}
	var sessionOwner, replicaID, state string
	var direction, policy string
	var linkedTaskID, linkedOperationKey pgtype.Text
	var sessionScope []string
	var sessionBaseRevision int64
	if err := tx.QueryRow(ctx, `SELECT owner_id, replica_id, state, task_id, task_hub_operation_key, base_revision, scope, direction, policy
FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, sessionID).
		Scan(&sessionOwner, &replicaID, &state, &linkedTaskID, &linkedOperationKey, &sessionBaseRevision, &sessionScope, &direction, &policy); err != nil {
		return storeError("lock Workspace sync session for Task write association", "workspace sync session", err)
	}
	if sessionOwner != ownerID || !strings.HasPrefix(replicaID, "device:") ||
		state == string(workspace.SyncCancelled) || state == string(workspace.SyncFailed) || state == string(workspace.SyncExpired) {
		return domain.NewError(domain.CodeNotFound, "device Workspace sync session not found")
	}
	deviceAccess, err := prepareWorkspaceSyncDeviceSessionMutation(ctx, tx, workspaceID, sessionID)
	if err != nil {
		return err
	}
	if err := requireActiveWorkspaceSyncDeviceSession(ctx, tx, deviceAccess, sessionOwner, workspaceID, replicaID); err != nil {
		return err
	}
	if deviceAccess == nil || deviceAccess.installationID != targetInstallationID {
		return domain.NewError(domain.CodeNotFound, "Workspace sync device does not match the Task target")
	}
	var op app.TaskHubOperation
	var localReceiptHash []byte
	var syncSessionID pgtype.Text
	err = tx.QueryRow(ctx, `SELECT target_installation_id, action, workspace_id, workspace_path, workspace_size_bytes,
workspace_sha256, source_checkpoint_revision, local_receipt_hash, sync_stage, sync_session_id, status
FROM task_hub_operations WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3`, ownerID, taskID, operationKey).
		Scan(&op.TargetInstallationID, &op.Action, &op.WorkspaceID, &op.WorkspacePath, &op.WorkspaceSizeBytes,
			&op.WorkspaceSHA256, &op.SourceCheckpointRevision, &localReceiptHash, &op.SyncStage, &syncSessionID, &op.Status)
	if err != nil {
		return storeError("lock Task Hub operation for Workspace sync association", "Hub Task operation", err)
	}
	op.LocalReceiptHash = append([]byte(nil), localReceiptHash...)
	if syncSessionID.Valid {
		op.SyncSessionID = syncSessionID.String
	}
	if op.Action != hub.ActionFilePut || op.WorkspaceID != workspaceID || op.TargetInstallationID != targetInstallationID ||
		len(op.LocalReceiptHash) != 32 || (op.Status != task.OperationDispatched && op.Status != task.OperationUnknown) {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace sync session does not match the Task file.put operation")
	}
	exactUpload, err := taskHubWorkspaceSyncHasExactUpload(ctx, tx, workspaceID, sessionID, op.WorkspacePath,
		op.WorkspaceSizeBytes, op.WorkspaceSHA256)
	if err != nil {
		return fmt.Errorf("verify Task file.put upload plan: %w", err)
	}
	if !exactUpload {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace sync session must upload only the exact Task file.put path")
	}
	if op.SyncStage != app.TaskHubWriteStageLocal && op.SyncStage != app.TaskHubWriteStageSyncPending && op.SyncStage != app.TaskHubWriteStageCloudSynced {
		return domain.NewError(domain.CodeConflict, "Task file.put operation has no local-applied receipt")
	}
	if sessionBaseRevision != op.SourceCheckpointRevision || len(sessionScope) != 1 || sessionScope[0] != op.WorkspacePath ||
		direction != string(workspace.SyncUpload) || policy != string(workspace.SyncCompareMerge) {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace sync scope or base revision does not match the Task file.put")
	}
	if linkedTaskID.Valid && (linkedTaskID.String != taskID || !linkedOperationKey.Valid || linkedOperationKey.String != operationKey) ||
		!linkedTaskID.Valid && linkedOperationKey.Valid {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace sync session is already linked to another Task operation")
	}
	if op.SyncSessionID != "" && op.SyncSessionID != sessionID {
		return domain.NewError(domain.CodeWorkspaceConflict, "Task file.put operation is linked to another Workspace sync session")
	}
	var taskWorkspaceID pgtype.Text
	var checkpointRevision pgtype.Int8
	if err := tx.QueryRow(ctx, `SELECT workspace_id, workspace_checkpoint_revision FROM tasks WHERE id=$1 AND owner_id=$2`, taskID, ownerID).
		Scan(&taskWorkspaceID, &checkpointRevision); err != nil {
		return fmt.Errorf("verify Task Workspace checkpoint for file.put association: %w", err)
	}
	if !taskWorkspaceID.Valid || !checkpointRevision.Valid || taskWorkspaceID.String != workspaceID || checkpointRevision.Int64 != op.SourceCheckpointRevision {
		return domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace checkpoint changed before file.put synchronization")
	}
	if op.SyncStage == app.TaskHubWriteStageCloudSynced {
		if op.SyncSessionID != sessionID || !linkedTaskID.Valid || linkedTaskID.String != taskID ||
			!linkedOperationKey.Valid || linkedOperationKey.String != operationKey {
			return domain.NewError(domain.CodeWorkspaceConflict, "completed Task file.put operation is missing its Workspace sync link")
		}
		return nil
	}
	linked, err := tx.Exec(ctx, `UPDATE workspace_sync_sessions
SET task_id=$3, task_hub_operation_key=$4, updated_at=clock_timestamp()
WHERE workspace_id=$1 AND id=$2 AND (task_id IS NULL OR task_id=$3)
	AND (task_hub_operation_key IS NULL OR task_hub_operation_key=$4)`, workspaceID, sessionID, taskID, operationKey)
	if err != nil {
		return fmt.Errorf("link Workspace sync session to Task file.put operation: %w", err)
	}
	if linked.RowsAffected() != 1 {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace sync session changed before Task file.put association")
	}
	command, err := tx.Exec(ctx, `UPDATE task_hub_operations
SET sync_stage='sync_pending', sync_session_id=$4, updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 AND sync_stage IN ('local_applied','sync_pending')
  AND (sync_session_id IS NULL OR sync_session_id=$4)`, ownerID, taskID, operationKey, sessionID)
	if err != nil {
		return fmt.Errorf("advance Task file.put to sync pending: %w", err)
	}
	if command.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "Task file.put operation changed before Workspace sync association")
	}
	return nil
}

func taskHubWorkspaceSyncHasExactUpload(ctx context.Context, tx pgx.Tx, workspaceID, sessionID, path string, sizeBytes int64, sha256 string) (bool, error) {
	if tx == nil || !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) ||
		workspace.ValidateRelativePath(path) != nil || sizeBytes < 0 || !validPostgresSHA256(sha256) {
		return false, domain.InvalidArgument("sync_session", "Task file.put upload manifest identity is invalid")
	}
	var matches bool
	err := tx.QueryRow(ctx, `SELECT jsonb_array_length(files)=1
  AND jsonb_array_length(upload_files)=1 AND jsonb_array_length(download_files)=0
  AND cardinality(deleted_paths)=0 AND cardinality(upload_deleted_paths)=0 AND cardinality(download_deleted_paths)=0
  AND cardinality(empty_directories)=0 AND cardinality(upload_empty_directories)=0 AND cardinality(download_empty_directories)=0
  AND cardinality(deleted_directories)=0 AND cardinality(upload_deleted_directories)=0 AND cardinality(download_deleted_directories)=0
  AND ack_cursor IS NULL AND direction='upload' AND policy='compare_merge' AND cardinality(scope)=1 AND scope[1]=$3
  AND files->0->>'path'=$3 AND files->0->>'size_bytes'=$4 AND files->0->>'sha256'=$5
  AND EXISTS (
  SELECT 1 FROM workspace_sync_sessions s
  CROSS JOIN LATERAL jsonb_array_elements(s.upload_files) value
  WHERE s.workspace_id=$1 AND s.id=$2
    AND value->>'path'=$3 AND value->>'size_bytes'=$4 AND value->>'sha256'=$5
  ) FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2`, workspaceID, sessionID,
		path, fmt.Sprint(sizeBytes), sha256).Scan(&matches)
	return matches, err
}

// reconcileTaskHubOperationWorkspaceSyncCommitTx is called after the matching
// device session reaches completed, before its commit receipt is finalized.
func reconcileTaskHubOperationWorkspaceSyncCommitTx(ctx context.Context, tx pgx.Tx, workspaceID, sessionID string, committedRevision int64) error {
	if tx == nil || !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) || committedRevision < 0 {
		return domain.InvalidArgument("sync_session", "Task file.put sync commit identity is invalid")
	}
	var ownerID, replicaID, state string
	var targetRevision, sessionBaseRevision int64
	var taskID, operationKey pgtype.Text
	var sessionScope []string
	if err := tx.QueryRow(ctx, `SELECT owner_id, replica_id, state, target_revision, task_id, task_hub_operation_key, base_revision, scope
	FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2`, workspaceID, sessionID).
		Scan(&ownerID, &replicaID, &state, &targetRevision, &taskID, &operationKey, &sessionBaseRevision, &sessionScope); err != nil {
		return storeError("lock Workspace sync session for Task file.put settlement", "workspace sync session", err)
	}
	if !taskID.Valid && !operationKey.Valid {
		return nil
	}
	if !taskID.Valid || !operationKey.Valid || state != string(workspace.SyncCompleted) || targetRevision != committedRevision ||
		!strings.HasPrefix(replicaID, "device:") {
		return nil
	}
	deviceAccess, err := prepareWorkspaceSyncDeviceSessionMutation(ctx, tx, workspaceID, sessionID)
	if err != nil {
		return err
	}
	if err := requireActiveWorkspaceSyncDeviceSession(ctx, tx, deviceAccess, ownerID, workspaceID, replicaID); err != nil {
		return err
	}
	if deviceAccess == nil {
		return domain.NewError(domain.CodeNotFound, "Workspace sync session is not bound to a device")
	}
	var op app.TaskHubOperation
	var localReceiptHash []byte
	var syncRevision pgtype.Int8
	if err := tx.QueryRow(ctx, `SELECT target_installation_id, action, workspace_id, workspace_path, workspace_size_bytes,
target_grant_version, workspace_sha256, source_checkpoint_revision, local_receipt_hash, sync_stage, sync_session_id, sync_revision, status, result_hash
FROM task_hub_operations WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3`, ownerID, taskID.String, operationKey.String).
		Scan(&op.TargetInstallationID, &op.Action, &op.WorkspaceID, &op.WorkspacePath, &op.WorkspaceSizeBytes,
			&op.TargetGrantVersion, &op.WorkspaceSHA256, &op.SourceCheckpointRevision, &localReceiptHash,
			&op.SyncStage, &op.SyncSessionID, &syncRevision, &op.Status, &op.ResultHash); err != nil {
		return storeError("lock Task file.put operation for sync commit", "Hub Task operation", err)
	}
	op.OwnerID, op.TaskID, op.Key = ownerID, taskID.String, operationKey.String
	op.LocalReceiptHash = append([]byte(nil), localReceiptHash...)
	if syncRevision.Valid {
		op.SyncRevision = syncRevision.Int64
	}
	if op.Action != hub.ActionFilePut || op.WorkspaceID != workspaceID || op.TargetInstallationID != deviceAccess.installationID ||
		op.SyncSessionID != sessionID || len(op.LocalReceiptHash) != 32 || op.SourceCheckpointRevision < 0 ||
		op.SourceCheckpointRevision != sessionBaseRevision || len(sessionScope) != 1 || sessionScope[0] != op.WorkspacePath {
		return domain.NewError(domain.CodeWorkspaceConflict, "completed Workspace sync does not match the Task file.put operation")
	}
	if op.SyncStage == app.TaskHubWriteStageCloudSynced && op.Status == task.OperationSucceeded && op.SyncRevision == committedRevision {
		return nil
	}
	if op.SyncStage != app.TaskHubWriteStageSyncPending ||
		(op.Status != task.OperationDispatched && op.Status != task.OperationUnknown) {
		return domain.NewError(domain.CodeConflict, "Task file.put operation is not waiting for this Workspace sync commit")
	}
	var hasUpload bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM workspace_sync_sessions s
  CROSS JOIN LATERAL jsonb_array_elements(s.upload_files) value
  WHERE s.workspace_id=$1 AND s.id=$2
    AND value->>'path'=$3 AND value->>'size_bytes'=$4 AND value->>'sha256'=$5
)`, workspaceID, sessionID, op.WorkspacePath, fmt.Sprint(op.WorkspaceSizeBytes), op.WorkspaceSHA256).Scan(&hasUpload); err != nil {
		return fmt.Errorf("verify Task file.put upload in completed Workspace sync: %w", err)
	}
	if !hasUpload {
		return domain.NewError(domain.CodeWorkspaceConflict, "completed Workspace sync did not upload the Task file.put path")
	}
	var actualSize int64
	var actualSHA256 string
	if err := tx.QueryRow(ctx, `SELECT size_bytes, sha256 FROM workspace_revision_files
WHERE workspace_id=$1 AND file_path=$2 AND valid_from_revision <= $3
  AND (valid_to_revision IS NULL OR valid_to_revision > $3) AND kind='file'`, workspaceID, op.WorkspacePath, committedRevision).
		Scan(&actualSize, &actualSHA256); err != nil {
		return storeError("verify Task file.put in committed Workspace revision", "workspace file", err)
	}
	if actualSize != op.WorkspaceSizeBytes || actualSHA256 != op.WorkspaceSHA256 {
		return domain.NewError(domain.CodeWorkspaceConflict, "committed Workspace file does not match the Task file.put digest")
	}
	var taskWorkspaceID pgtype.Text
	var checkpointRevision pgtype.Int8
	if err := tx.QueryRow(ctx, `SELECT workspace_id, workspace_checkpoint_revision FROM tasks WHERE id=$1 AND owner_id=$2`, taskID.String, ownerID).
		Scan(&taskWorkspaceID, &checkpointRevision); err != nil {
		return fmt.Errorf("verify Task checkpoint during file.put settlement: %w", err)
	}
	if !taskWorkspaceID.Valid || !checkpointRevision.Valid || taskWorkspaceID.String != workspaceID || checkpointRevision.Int64 != op.SourceCheckpointRevision {
		return domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace checkpoint does not match the file.put source checkpoint")
	}
	if err := advanceTaskHubWorkspaceCheckpointTx(ctx, tx, ownerID, taskID.String, workspaceID,
		op.SourceCheckpointRevision, committedRevision); err != nil {
		return err
	}
	op.SyncStage, op.SyncSessionID, op.SyncRevision, op.Status = app.TaskHubWriteStageCloudSynced, sessionID, committedRevision, task.OperationSucceeded
	resultHash, err := app.TaskHubFilePutCloudReceiptHash(op)
	if err != nil {
		return fmt.Errorf("hash Task file.put cloud receipt: %w", err)
	}
	command, err := tx.Exec(ctx, `UPDATE task_hub_operations
SET sync_stage='cloud_synced', sync_revision=$4, result_hash=$5, status='succeeded', updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 AND status IN ('dispatched','unknown')
  AND sync_stage='sync_pending' AND sync_session_id=$6`, ownerID, taskID.String, operationKey.String, committedRevision, resultHash[:], sessionID)
	if err != nil {
		return fmt.Errorf("settle Task file.put after Workspace sync commit: %w", err)
	}
	if command.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "Task file.put operation changed during Workspace sync settlement")
	}
	return nil
}

func advanceTaskHubWorkspaceCheckpointTx(ctx context.Context, tx pgx.Tx, ownerID, taskID, workspaceID string,
	sourceRevision, committedRevision int64) error {
	if tx == nil || !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidWorkspaceID(workspaceID) ||
		sourceRevision < 0 || committedRevision < sourceRevision {
		return domain.InvalidArgument("workspace_checkpoint", "Task file.put checkpoint identity is invalid")
	}
	command, err := tx.Exec(ctx, `UPDATE tasks
SET workspace_checkpoint_revision=$3, updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND workspace_id=$4 AND workspace_checkpoint_revision=$5`,
		taskID, ownerID, committedRevision, workspaceID, sourceRevision)
	if err != nil {
		return fmt.Errorf("advance Task Workspace checkpoint after file.put sync: %w", err)
	}
	if command.RowsAffected() != 1 {
		return domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace checkpoint changed before file.put sync settlement")
	}
	return nil
}

func taskHubFilePutLocalReceiptHash(operation app.TaskHubOperation, receiptHash []byte) ([32]byte, error) {
	canonical, err := json.Marshal(struct {
		Version                  string `json:"version"`
		OwnerID                  string `json:"owner_id"`
		TaskID                   string `json:"task_id"`
		OperationKey             string `json:"operation_key"`
		TargetInstallationID     string `json:"target_installation_id"`
		TargetGrantVersion       int64  `json:"target_grant_version"`
		WorkspaceID              string `json:"workspace_id"`
		Path                     string `json:"path"`
		SizeBytes                int64  `json:"size_bytes"`
		SHA256                   string `json:"sha256"`
		SourceCheckpointRevision int64  `json:"source_checkpoint_revision"`
		ReceiptHash              []byte `json:"receipt_hash"`
	}{"morde.task-hub.file-put.local-receipt.v1", operation.OwnerID, operation.TaskID, operation.Key,
		operation.TargetInstallationID, operation.TargetGrantVersion, operation.WorkspaceID, operation.WorkspacePath,
		operation.WorkspaceSizeBytes, operation.WorkspaceSHA256, operation.SourceCheckpointRevision, receiptHash})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

func (s *Store) MarkTaskHubOperationUnknown(ctx context.Context, value app.TaskHubOperation, leaseOwner string) error {
	if !validTaskHubOperationState(value) || !validTaskHubLeaseOwner(leaseOwner) {
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
	result, err := scanTaskHubOperation(s.pool.QueryRow(ctx, `SELECT task_id, owner_id, operation_key, tool_name, target_installation_id, action,
task_authorization_version, target_grant_version, task_revision, lease_version, request_hash, result_hash, status,
workspace_id, workspace_path, workspace_size_bytes, workspace_sha256, source_checkpoint_revision,
local_receipt_hash, sync_stage, sync_session_id, sync_revision
FROM task_hub_operations WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3`, ownerID, taskID, key))
	if err != nil {
		return app.TaskHubOperation{}, storeError("get Hub Task operation", "Hub Task operation", err)
	}
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
	if value.Action == hub.ActionFileFetch {
		var currentRevision, desiredRevision pgtype.Int8
		var status string
		if err := tx.QueryRow(ctx, `SELECT current_revision,desired_revision,status FROM tasks WHERE id=$1 AND owner_id=$2`,
			value.TaskID, value.OwnerID).Scan(&currentRevision, &desiredRevision, &status); err != nil {
			return sqlcgen.Task{}, fmt.Errorf("read Task revision for file fetch: %w", err)
		}
		if !currentRevision.Valid || !desiredRevision.Valid || currentRevision.Int64 != value.TaskRevision ||
			desiredRevision.Int64 != value.TaskRevision || status != string(task.StatusRunning) {
			return sqlcgen.Task{}, domain.NewError(domain.CodeConflict, "file.fetch Task revision is no longer current")
		}
	}
	if value.Action == hub.ActionFilePut {
		var workspaceID pgtype.Text
		var checkpointRevision pgtype.Int8
		if err := tx.QueryRow(ctx, `SELECT workspace_id, workspace_checkpoint_revision FROM tasks WHERE id=$1 AND owner_id=$2`,
			value.TaskID, value.OwnerID).Scan(&workspaceID, &checkpointRevision); err != nil {
			return sqlcgen.Task{}, fmt.Errorf("read file.put Task Workspace checkpoint: %w", err)
		}
		if !workspaceID.Valid || !checkpointRevision.Valid || workspaceID.String != value.WorkspaceID ||
			checkpointRevision.Int64 != value.SourceCheckpointRevision {
			return sqlcgen.Task{}, domain.NewError(domain.CodeWorkspaceConflict, "file.put does not match the Task Workspace checkpoint")
		}
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
	return scanTaskHubOperation(tx.QueryRow(ctx, `SELECT task_id, owner_id, operation_key, tool_name, target_installation_id, action,
task_authorization_version, target_grant_version, task_revision, lease_version, request_hash, result_hash, status,
workspace_id, workspace_path, workspace_size_bytes, workspace_sha256, source_checkpoint_revision,
local_receipt_hash, sync_stage, sync_session_id, sync_revision
FROM task_hub_operations WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 FOR UPDATE`, ownerID, taskID, key))
}

func scanTaskHubOperation(row pgx.Row) (app.TaskHubOperation, error) {
	var result app.TaskHubOperation
	var workspaceID, workspacePath, workspaceSHA256, syncSessionID pgtype.Text
	var workspaceSizeBytes, sourceCheckpointRevision, syncRevision pgtype.Int8
	err := row.Scan(
		&result.TaskID, &result.OwnerID, &result.Key, &result.ToolName, &result.TargetInstallationID, &result.Action,
		&result.TaskAuthorizationVersion, &result.TargetGrantVersion, &result.TaskRevision, &result.LeaseVersion, &result.RequestHash,
		&result.ResultHash, &result.Status, &workspaceID, &workspacePath, &workspaceSizeBytes, &workspaceSHA256,
		&sourceCheckpointRevision, &result.LocalReceiptHash, &result.SyncStage, &syncSessionID, &syncRevision)
	if err != nil {
		return app.TaskHubOperation{}, err
	}
	if workspaceID.Valid {
		result.WorkspaceID = workspaceID.String
	}
	if workspacePath.Valid {
		result.WorkspacePath = workspacePath.String
	}
	if workspaceSizeBytes.Valid {
		result.WorkspaceSizeBytes = workspaceSizeBytes.Int64
	}
	if workspaceSHA256.Valid {
		result.WorkspaceSHA256 = workspaceSHA256.String
	}
	if sourceCheckpointRevision.Valid {
		result.SourceCheckpointRevision = sourceCheckpointRevision.Int64
	}
	if syncSessionID.Valid {
		result.SyncSessionID = syncSessionID.String
	}
	if syncRevision.Valid {
		result.SyncRevision = syncRevision.Int64
	}
	result.RequestHash = append([]byte(nil), result.RequestHash...)
	result.ResultHash = append([]byte(nil), result.ResultHash...)
	result.LocalReceiptHash = append([]byte(nil), result.LocalReceiptHash...)
	return result, nil
}

func validTaskHubOperation(value app.TaskHubOperation) bool {
	return value.Action != hub.ActionFilePut && validTaskHubOperationState(value)
}

// The database action constraint remains the production gate for file.put.
func validTaskHubOperationState(value app.TaskHubOperation) bool {
	if !domain.ValidUserID(value.OwnerID) || !domain.ValidTaskID(value.TaskID) || !validTaskHubOperationKey(value.Key) ||
		value.ToolName == "" || len(value.ToolName) > 128 || !utf8.ValidString(value.ToolName) || strings.ContainsRune(value.ToolName, '\x00') ||
		!domain.ValidClientInstallationID(value.TargetInstallationID) ||
		(value.Action != hub.ActionFilePut && !validTaskHubAction(value.Action)) ||
		value.TaskAuthorizationVersion <= 0 || value.TargetGrantVersion <= 0 || value.LeaseVersion <= 0 || len(value.RequestHash) != 32 {
		return false
	}
	if value.Action != hub.ActionFilePut {
		return (value.Action == hub.ActionFileFetch && value.TaskRevision > 0 || value.Action != hub.ActionFileFetch && value.TaskRevision == 0) &&
			value.WorkspaceID == "" && value.WorkspacePath == "" && value.WorkspaceSizeBytes == 0 && value.WorkspaceSHA256 == "" &&
			value.SourceCheckpointRevision == 0 && len(value.LocalReceiptHash) == 0 && value.SyncStage == "" && value.SyncSessionID == "" &&
			value.SyncRevision == 0 && (len(value.ResultHash) == 0 || len(value.ResultHash) == 32)
	}
	return domain.ValidWorkspaceID(value.WorkspaceID) && workspace.ValidateRelativePath(value.WorkspacePath) == nil &&
		len(value.WorkspacePath) <= hub.MaxHubPathBytes && value.WorkspaceSizeBytes >= 0 && value.WorkspaceSizeBytes <= hub.MaxWorkspaceWriteBytes &&
		validPostgresSHA256(value.WorkspaceSHA256) && value.SourceCheckpointRevision >= 0 &&
		validTaskHubWriteState(value)
}

func validTaskHubAction(action string) bool {
	switch action {
	case hub.ActionScreenshot, hub.ActionFiles, hub.ActionFileGet, hub.ActionFileFetch, hub.ActionCommand:
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
		stored.TargetGrantVersion == requested.TargetGrantVersion && stored.TaskRevision == requested.TaskRevision && bytes.Equal(stored.RequestHash, requested.RequestHash) &&
		stored.WorkspaceID == requested.WorkspaceID && stored.WorkspacePath == requested.WorkspacePath &&
		stored.WorkspaceSizeBytes == requested.WorkspaceSizeBytes && stored.WorkspaceSHA256 == requested.WorkspaceSHA256 &&
		stored.SourceCheckpointRevision == requested.SourceCheckpointRevision
}

func validTaskHubWriteState(value app.TaskHubOperation) bool {
	switch value.SyncStage {
	case app.TaskHubWriteStagePending:
		return len(value.ResultHash) == 0 && len(value.LocalReceiptHash) == 0 && value.SyncSessionID == "" && value.SyncRevision == 0 &&
			(value.Status == task.OperationPrepared || value.Status == task.OperationDispatched || value.Status == task.OperationFailed || value.Status == task.OperationUnknown)
	case app.TaskHubWriteStageLocal:
		return len(value.ResultHash) == 0 && len(value.LocalReceiptHash) == 32 && value.SyncSessionID == "" && value.SyncRevision == 0 &&
			(value.Status == task.OperationDispatched || value.Status == task.OperationUnknown)
	case app.TaskHubWriteStageSyncPending:
		return len(value.ResultHash) == 0 && len(value.LocalReceiptHash) == 32 && workspace.ValidSessionID(value.SyncSessionID) && value.SyncRevision == 0 &&
			(value.Status == task.OperationDispatched || value.Status == task.OperationUnknown)
	case app.TaskHubWriteStageCloudSynced:
		return len(value.LocalReceiptHash) == 32 && workspace.ValidSessionID(value.SyncSessionID) && value.SyncRevision >= 0 &&
			value.Status == task.OperationSucceeded && len(value.ResultHash) == 32
	default:
		return false
	}
}

func nullableString(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

func nullableInt64(enabled bool, value int64) pgtype.Int8 {
	return pgtype.Int8{Int64: value, Valid: enabled}
}

var _ app.TaskHubOperationStore = (*Store)(nil)
var _ app.TaskHubWorkspaceWriteStore = (*Store)(nil)
