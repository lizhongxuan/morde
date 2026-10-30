package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func (s *Store) ReserveTaskWorkspacePublish(ctx context.Context, value app.TaskWorkspacePublishCommand) (app.TaskWorkspacePublishReceipt, error) {
	if err := app.ValidateTaskWorkspacePublishCommand(value); err != nil {
		return app.TaskWorkspacePublishReceipt{}, err
	}
	var receipt app.TaskWorkspacePublishReceipt
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		gate, err := requireOwnerExecutionEnabled(ctx, tx, value.OwnerID, nil)
		if err != nil {
			return err
		}
		var ownerID, hostID, status, leaseOwner string
		var leaseVersion, executionEpoch int64
		var leaseActive bool
		var workspaceID pgtype.Text
		var inputRevision, checkpointRevision pgtype.Int8
		if err := tx.QueryRow(ctx, `SELECT owner_id, host_id, status, lease_owner, lease_version,
COALESCE(lease_expires_at > clock_timestamp(), false), execution_epoch, workspace_id, input_revision, workspace_checkpoint_revision
FROM tasks WHERE id=$1 FOR UPDATE`, value.TaskID).Scan(
			&ownerID, &hostID, &status, &leaseOwner, &leaseVersion, &leaseActive, &executionEpoch,
			&workspaceID, &inputRevision, &checkpointRevision); err != nil {
			return storeError("lock Task before Workspace publish reservation", "task", err)
		}
		existing, err := getTaskWorkspacePublishReceiptTx(ctx, tx, value, false)
		if err == nil {
			receipt = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if ownerID != value.OwnerID || hostID != value.HostID || executionEpoch != gate.epoch {
			return missingTaskError()
		}
		if status != string(task.StatusRunning) || leaseOwner != value.LeaseOwner || leaseVersion != value.LeaseVersion || !leaseActive {
			return taskLeaseConflict()
		}
		if !workspaceID.Valid || workspaceID.String != value.WorkspaceID || !inputRevision.Valid || inputRevision.Int64 != value.InputRevision ||
			!checkpointRevision.Valid || checkpointRevision.Int64 != value.BaseRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace checkpoint changed before publish reservation")
		}
		if err := ensureNoPendingTaskWorkspacePublishOperation(ctx, tx, value.TaskID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_workspace_publish_receipts
(task_id, owner_id, workspace_id, operation_key, request_hash, base_revision, state)
VALUES ($1,$2,$3,$4,$5,$6,'pending') ON CONFLICT (task_id, operation_key) DO NOTHING`,
			value.TaskID, value.OwnerID, value.WorkspaceID, value.OperationKey, value.RequestHash, value.BaseRevision); err != nil {
			return fmt.Errorf("reserve Task Workspace publish receipt: %w", err)
		}
		receipt, err = getTaskWorkspacePublishReceiptTx(ctx, tx, value, false)
		return err
	})
	return receipt, err
}

func (s *Store) GetTaskWorkspacePublishReceipt(ctx context.Context, value app.TaskWorkspacePublishCommand) (app.TaskWorkspacePublishReceipt, error) {
	if err := app.ValidateTaskWorkspacePublishCommand(value); err != nil {
		return app.TaskWorkspacePublishReceipt{}, err
	}
	var receipt app.TaskWorkspacePublishReceipt
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		receipt, err = getTaskWorkspacePublishReceiptTx(ctx, tx, value, false)
		return err
	})
	return receipt, err
}

func (s *Store) RejectTaskWorkspacePublishQuota(ctx context.Context, value app.TaskWorkspacePublishCommand) error {
	if err := app.ValidateTaskWorkspacePublishCommand(value); err != nil {
		return err
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := requireOwnerExecutionEnabled(ctx, tx, value.OwnerID, nil); err != nil {
			return err
		}
		var ownerID string
		if err := tx.QueryRow(ctx, `SELECT owner_id FROM tasks WHERE id=$1 FOR UPDATE`, value.TaskID).Scan(&ownerID); err != nil {
			return storeError("lock Task before rejecting Workspace publish quota", "task", err)
		}
		if ownerID != value.OwnerID {
			return missingTaskError()
		}
		receipt, err := getTaskWorkspacePublishReceiptTx(ctx, tx, value, true)
		if err != nil {
			return err
		}
		if receipt.State != app.TaskWorkspacePublishPending {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE task_workspace_publish_receipts SET state='quota_rejected', updated_at=clock_timestamp()
WHERE task_id=$1 AND operation_key=$2 AND state='pending'`, value.TaskID, value.OperationKey); err != nil {
			return fmt.Errorf("reject Task Workspace publish for physical quota: %w", err)
		}
		return nil
	})
}

func ensureNoPendingTaskWorkspacePublish(ctx context.Context, tx pgx.Tx, taskID string) error {
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM task_workspace_publish_receipts WHERE task_id=$1 AND state='pending'
)`, taskID).Scan(&pending); err != nil {
		return fmt.Errorf("check pending Task Workspace publish: %w", err)
	}
	if pending {
		return domain.NewError(domain.CodeWorkspaceBusy, "Task Workspace publish is pending; retry after it settles")
	}
	return nil
}

func (s *Store) FindTaskWorkspacePublishReceipt(ctx context.Context, ownerID, taskID, workspaceID, operationKey string) (app.TaskWorkspacePublishReceipt, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidWorkspaceID(workspaceID) || operationKey == "" || len(operationKey) > 255 {
		return app.TaskWorkspacePublishReceipt{}, domain.InvalidArgument("task_workspace_publish", "Task Workspace publish receipt identity is invalid")
	}
	var receipt app.TaskWorkspacePublishReceipt
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var storedOwner, storedWorkspace string
		var state string
		var baseRevision int64
		var manifestSHA pgtype.Text
		var publishedRevision pgtype.Int8
		err := tx.QueryRow(ctx, `SELECT owner_id, workspace_id, base_revision, state, manifest_sha256, published_revision
FROM task_workspace_publish_receipts WHERE task_id=$1 AND operation_key=$2`, taskID, operationKey).
			Scan(&storedOwner, &storedWorkspace, &baseRevision, &state, &manifestSHA, &publishedRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			return storeError("find Task Workspace publish receipt", "Task Workspace publish receipt", err)
		}
		if err != nil {
			return fmt.Errorf("find Task Workspace publish receipt: %w", err)
		}
		if storedOwner != ownerID || storedWorkspace != workspaceID {
			return domain.NewError(domain.CodeNotFound, "Task Workspace publish receipt not found")
		}
		receipt = app.TaskWorkspacePublishReceipt{State: app.TaskWorkspacePublishState(state), WorkspaceID: storedWorkspace, BaseRevision: baseRevision}
		if manifestSHA.Valid {
			receipt.ManifestSHA256 = manifestSHA.String
		}
		if publishedRevision.Valid {
			receipt.PublishedRevision = publishedRevision.Int64
		}
		return nil
	})
	return receipt, err
}

func getTaskWorkspacePublishReceiptTx(ctx context.Context, tx pgx.Tx, value app.TaskWorkspacePublishCommand, forUpdate bool) (app.TaskWorkspacePublishReceipt, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	var ownerID, workspaceID, state string
	var requestHash []byte
	var baseRevision int64
	var manifestSHA pgtype.Text
	var publishedRevision pgtype.Int8
	err := tx.QueryRow(ctx, `SELECT owner_id, workspace_id, request_hash, base_revision, state, manifest_sha256, published_revision
FROM task_workspace_publish_receipts WHERE task_id=$1 AND operation_key=$2`+lock, value.TaskID, value.OperationKey).
		Scan(&ownerID, &workspaceID, &requestHash, &baseRevision, &state, &manifestSHA, &publishedRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.TaskWorkspacePublishReceipt{}, pgx.ErrNoRows
	}
	if err != nil {
		return app.TaskWorkspacePublishReceipt{}, fmt.Errorf("read Task Workspace publish receipt: %w", err)
	}
	if ownerID != value.OwnerID || workspaceID != value.WorkspaceID || baseRevision != value.BaseRevision || !equalOperationHash(requestHash, value.RequestHash) {
		return app.TaskWorkspacePublishReceipt{}, idempotencyConflict()
	}
	receipt := app.TaskWorkspacePublishReceipt{State: app.TaskWorkspacePublishState(state), WorkspaceID: workspaceID, BaseRevision: baseRevision}
	if manifestSHA.Valid {
		receipt.ManifestSHA256 = manifestSHA.String
	}
	if publishedRevision.Valid {
		receipt.PublishedRevision = publishedRevision.Int64
	}
	return receipt, nil
}

func (s *Store) CommitTaskWorkspacePublish(ctx context.Context, value app.TaskWorkspacePublishCommand, files []workspace.File, manifestSHA string) (app.TaskWorkspacePublishReceipt, error) {
	if err := app.ValidateTaskWorkspacePublishCommand(value); err != nil {
		return app.TaskWorkspacePublishReceipt{}, err
	}
	actualManifestSHA, err := workspace.ManifestSHA256(files)
	if err != nil || actualManifestSHA != manifestSHA {
		return app.TaskWorkspacePublishReceipt{}, domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace output manifest digest changed")
	}
	var receipt app.TaskWorkspacePublishReceipt
	var rejected error
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		gate, err := requireOwnerExecutionEnabled(ctx, tx, value.OwnerID, nil)
		if err != nil {
			return err
		}
		var ownerID, hostID, status, leaseOwner string
		var leaseVersion, executionEpoch int64
		var leaseActive bool
		var workspaceID pgtype.Text
		var inputRevision, checkpointRevision pgtype.Int8
		if err := tx.QueryRow(ctx, `SELECT owner_id, host_id, status, lease_owner, lease_version,
COALESCE(lease_expires_at > clock_timestamp(), false), execution_epoch, workspace_id, input_revision, workspace_checkpoint_revision
FROM tasks WHERE id=$1 FOR UPDATE`, value.TaskID).Scan(
			&ownerID, &hostID, &status, &leaseOwner, &leaseVersion, &leaseActive, &executionEpoch,
			&workspaceID, &inputRevision, &checkpointRevision); err != nil {
			return storeError("lock Task before Workspace publish", "task", err)
		}
		stored, err := getTaskWorkspacePublishReceiptTx(ctx, tx, value, true)
		if err != nil {
			return err
		}
		receipt = stored
		switch stored.State {
		case app.TaskWorkspacePublishSucceeded:
			return nil
		case app.TaskWorkspacePublishConflict:
			rejected = domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed after this task checkpoint was confirmed")
			return nil
		case app.TaskWorkspacePublishQuotaRejected:
			rejected = domain.NewError(domain.CodeQuotaExceeded, "Workspace storage quota would be exceeded")
			return nil
		case app.TaskWorkspacePublishPending:
		default:
			return domain.NewError(domain.CodeInternal, "Task Workspace publish receipt state is invalid")
		}
		if ownerID != value.OwnerID || hostID != value.HostID || executionEpoch != gate.epoch {
			return missingTaskError()
		}
		if status != string(task.StatusRunning) || leaseOwner != value.LeaseOwner || leaseVersion != value.LeaseVersion || !leaseActive {
			return taskLeaseConflict()
		}
		if !workspaceID.Valid || workspaceID.String != value.WorkspaceID || !inputRevision.Valid || inputRevision.Int64 != value.InputRevision ||
			!checkpointRevision.Valid || checkpointRevision.Int64 != value.BaseRevision {
			return markTaskWorkspacePublishRejected(ctx, tx, value, &receipt, &rejected,
				domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace checkpoint changed before publish"), app.TaskWorkspacePublishConflict)
		}
		if err := ensureNoPendingTaskWorkspacePublishOperation(ctx, tx, value.TaskID); err != nil {
			return err
		}
		var hostOwner, hostState, hostControl string
		var generation, controlVersion int64
		var instanceID string
		var deleted bool
		if err := tx.QueryRow(ctx, `SELECT owner_id, state, control_state, runtime_generation, COALESCE(runtime_instance_id,''), control_version, deleted_at IS NOT NULL
FROM hosts WHERE id=$1 FOR SHARE`, value.HostID).Scan(&hostOwner, &hostState, &hostControl, &generation, &instanceID, &controlVersion, &deleted); err != nil {
			return storeError("lock Host before Task Workspace publish", "host", err)
		}
		if hostOwner != value.OwnerID || hostState != string(host.StateRunning) || hostControl != "ai" || deleted ||
			generation != value.HostFence.RuntimeGeneration || instanceID != value.HostFence.RuntimeInstanceID || controlVersion != value.HostFence.ControlVersion {
			return domain.NewError(domain.CodeInvalidState, "Host runtime or control state changed before Task Workspace publish")
		}
		var workspaceOwner, workspaceState string
		var currentRevision, currentBytes, quotaBytes, currentFileCount, revisionBytes int64
		if err := tx.QueryRow(ctx, `SELECT owner_id, state, revision, used_bytes, quota_bytes FROM workspaces WHERE id=$1 FOR UPDATE`, value.WorkspaceID).
			Scan(&workspaceOwner, &workspaceState, &currentRevision, &currentBytes, &quotaBytes); err != nil {
			return storeError("lock Workspace for Task publish", "workspace", err)
		}
		if workspaceOwner != value.OwnerID || workspaceState != string(workspace.StateReady) {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace is not available for Task publish")
		}
		if currentRevision != value.BaseRevision {
			return markTaskWorkspacePublishRejected(ctx, tx, value, &receipt, &rejected,
				domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed after this task checkpoint was confirmed"), app.TaskWorkspacePublishConflict)
		}
		if err := tx.QueryRow(ctx, `SELECT file_count, total_bytes FROM workspace_revisions WHERE workspace_id=$1 AND revision=$2`,
			value.WorkspaceID, currentRevision).Scan(&currentFileCount, &revisionBytes); err != nil {
			return storeError("read locked Workspace revision for Task publish", "workspace revision", err)
		}
		if currentBytes != revisionBytes {
			return domain.NewError(domain.CodeInternal, "Workspace storage usage does not match its latest manifest")
		}
		var bindingOwner, bindingState, syncState string
		var bindingVersion, desiredRevision, appliedRevision int64
		if err := tx.QueryRow(ctx, `SELECT owner_id, state, sync_state, binding_version, desired_revision, applied_revision
FROM workspace_bindings WHERE host_id=$1 AND workspace_id=$2 FOR UPDATE`, value.HostID, value.WorkspaceID).
			Scan(&bindingOwner, &bindingState, &syncState, &bindingVersion, &desiredRevision, &appliedRevision); err != nil {
			return storeError("lock Host Workspace binding before Task publish", "workspace binding", err)
		}
		if bindingOwner != value.OwnerID || bindingState != string(workspace.BindingActive) || syncState != string(workspace.BindingSyncHealthy) ||
			bindingVersion != value.BindingVersion || desiredRevision != value.BaseRevision || appliedRevision != value.BaseRevision {
			return markTaskWorkspacePublishRejected(ctx, tx, value, &receipt, &rejected,
				domain.NewError(domain.CodeWorkspaceConflict, "Host Workspace binding changed before Task publish"), app.TaskWorkspacePublishConflict)
		}
		mutations, err := taskWorkspacePublishMutations(ctx, tx, value.WorkspaceID, currentRevision, files)
		if err != nil {
			return err
		}
		if err := ensureWorkspaceMutationsBlockUsageStored(ctx, tx, value.WorkspaceID, mutations); err != nil {
			return err
		}
		if len(mutations) > 0 {
			if _, err := tx.Exec(ctx, `SAVEPOINT task_workspace_publish_apply`); err != nil {
				return fmt.Errorf("start Task Workspace publish savepoint: %w", err)
			}
			if err := applyWorkspaceSyncMutations(ctx, tx, value.WorkspaceID, currentRevision, currentFileCount, currentBytes, quotaBytes, mutations); err != nil {
				if domain.CodeOf(err) != domain.CodeQuotaExceeded {
					return err
				}
				if _, rollbackErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT task_workspace_publish_apply`); rollbackErr != nil {
					return fmt.Errorf("rollback over-quota Task Workspace publish: %w", rollbackErr)
				}
				return markTaskWorkspacePublishRejected(ctx, tx, value, &receipt, &rejected, err, app.TaskWorkspacePublishQuotaRejected)
			}
		}
		publishedRevision := currentRevision
		if len(mutations) > 0 {
			publishedRevision++
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET workspace_checkpoint_revision=$2, updated_at=clock_timestamp() WHERE id=$1`, value.TaskID, publishedRevision); err != nil {
			return fmt.Errorf("advance Task Workspace checkpoint: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE task_workspace_publish_receipts
SET state='succeeded', manifest_sha256=$3, published_revision=$4, updated_at=clock_timestamp()
WHERE task_id=$1 AND operation_key=$2 AND state='pending'`, value.TaskID, value.OperationKey, manifestSHA, publishedRevision); err != nil {
			return fmt.Errorf("commit Task Workspace publish receipt: %w", err)
		}
		receipt.State, receipt.ManifestSHA256, receipt.PublishedRevision = app.TaskWorkspacePublishSucceeded, manifestSHA, publishedRevision
		return nil
	})
	if err != nil {
		return app.TaskWorkspacePublishReceipt{}, err
	}
	if rejected != nil {
		return receipt, rejected
	}
	return receipt, nil
}

func markTaskWorkspacePublishRejected(ctx context.Context, tx pgx.Tx, value app.TaskWorkspacePublishCommand, receipt *app.TaskWorkspacePublishReceipt, rejected *error, cause error, state app.TaskWorkspacePublishState) error {
	if _, err := tx.Exec(ctx, `UPDATE task_workspace_publish_receipts SET state=$3, updated_at=clock_timestamp()
WHERE task_id=$1 AND operation_key=$2 AND state='pending'`, value.TaskID, value.OperationKey, string(state)); err != nil {
		return fmt.Errorf("persist Task Workspace publish rejection: %w", err)
	}
	receipt.State = state
	*rejected = cause
	return nil
}

func ensureNoPendingTaskWorkspacePublishOperation(ctx context.Context, tx pgx.Tx, taskID string) error {
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT
EXISTS (SELECT 1 FROM task_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown')) OR
EXISTS (SELECT 1 FROM task_mcp_connector_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown')) OR
EXISTS (SELECT 1 FROM task_stop_requests WHERE task_id=$1 AND status='pending')`, taskID).Scan(&pending); err != nil {
		return fmt.Errorf("check Task operations before Workspace publish: %w", err)
	}
	if pending {
		return domain.NewError(domain.CodeWorkspaceBusy, "Task has an in-flight operation; retry Workspace publish after it settles")
	}
	return nil
}

func (s *Store) ReserveTaskWorkspaceBlockUsage(ctx context.Context, workspaceID string, reservations []app.WorkspaceBlockReservation, maxOwnerBytes int64) error {
	if !domain.ValidWorkspaceID(workspaceID) || maxOwnerBytes <= 0 {
		return domain.InvalidArgument("workspace_block_reservation", "workspace block reservation fields are invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID, state string
		if err := tx.QueryRow(ctx, `SELECT owner_id, state FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&ownerID, &state); err != nil {
			return storeError("lock Workspace before reserving Task output blocks", "workspace", err)
		}
		if state != string(workspace.StateReady) {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace is not available for Task publish")
		}
		return reserveWorkspaceBlockUsage(ctx, tx, workspaceID, ownerID, reservations, maxOwnerBytes)
	})
}

func taskWorkspacePublishMutations(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64, files []workspace.File) ([]syncMutation, error) {
	rows, err := tx.Query(ctx, `SELECT file_path, size_bytes, sha256, mode, chunks FROM workspace_revision_files
WHERE workspace_id=$1 AND valid_from_revision <= $2 AND (valid_to_revision IS NULL OR valid_to_revision > $2)
ORDER BY file_path`, workspaceID, revision)
	if err != nil {
		return nil, fmt.Errorf("read current Workspace files for Task publish: %w", err)
	}
	currentFiles := make([]workspace.File, 0)
	for rows.Next() {
		var filePath, digest string
		var size int64
		var mode int32
		var chunks []byte
		if err := rows.Scan(&filePath, &size, &digest, &mode, &chunks); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan current Workspace file for Task publish: %w", err)
		}
		file, err := workspaceFileFromRow(filePath, size, digest, mode, chunks)
		if err != nil {
			rows.Close()
			return nil, err
		}
		currentFiles = append(currentFiles, file)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate current Workspace files for Task publish: %w", err)
	}
	rows.Close()
	currentByPath := fileMapByFoldedPath(currentFiles)
	outputByPath := fileMapByFoldedPath(files)
	mutations := make([]syncMutation, 0, len(currentFiles)+len(files))
	for _, current := range currentFiles {
		output, exists := outputByPath[strings.ToLower(current.Path)]
		if !exists {
			mutations = append(mutations, syncMutation{path: current.Path})
			continue
		}
		if !sameWorkspaceFile(current, output) {
			file := output
			mutations = append(mutations, syncMutation{path: output.Path, file: &file})
		}
	}
	for _, output := range files {
		if _, exists := currentByPath[strings.ToLower(output.Path)]; !exists {
			file := output
			mutations = append(mutations, syncMutation{path: output.Path, file: &file})
		}
	}
	return mutations, nil
}
