package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

func (s *Store) GetOperation(ctx context.Context, ownerID, taskID, key string) (task.Operation, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || key == "" || len(key) > 255 {
		return task.Operation{}, domain.InvalidArgument("operation", "operation lookup fields are invalid")
	}
	var result task.Operation
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		row, err := loadOwnedOperationByKey(ctx, tx, ownerID, taskID, key)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "task operation not found")
		}
		if err != nil {
			return fmt.Errorf("get task operation: %w", err)
		}
		result = row
		return nil
	})
	return result, err
}

// ListUnknownOperations returns owned operations that need reconciliation before a
// recovered task may safely repeat their external actions.
func (s *Store) ListUnknownOperations(ctx context.Context, ownerID, taskID string) ([]task.Operation, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) {
		return nil, domain.InvalidArgument("task_id", "task lookup fields are invalid")
	}
	var result []task.Operation
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND owner_id=$2)`, taskID, ownerID).Scan(&exists); err != nil {
			return fmt.Errorf("check task ownership for unknown operations: %w", err)
		}
		if !exists {
			return missingTaskError()
		}
		rows, err := tx.Query(ctx, `SELECT o.id, o.task_id, o.operation_key, o.request_hash, o.host_id, o.workspace_id, o.binding_version, o.input_revision,
o.runtime_generation, o.runtime_instance_id, o.control_version, o.lease_version, o.runtime_lease_version, o.status, o.external_id, o.result, o.created_at, o.updated_at
FROM task_operations o JOIN tasks t ON t.id=o.task_id
WHERE o.task_id=$1 AND t.owner_id=$2 AND o.status='unknown'
ORDER BY o.created_at, o.operation_key`, taskID, ownerID)
		if err != nil {
			return fmt.Errorf("list unknown task operations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var row sqlcgen.TaskOperation
			if err := rows.Scan(&row.ID, &row.TaskID, &row.OperationKey, &row.RequestHash, &row.HostID,
				&row.WorkspaceID, &row.BindingVersion, &row.InputRevision, &row.RuntimeGeneration, &row.RuntimeInstanceID, &row.ControlVersion, &row.LeaseVersion,
				&row.RuntimeLeaseVersion, &row.Status, &row.ExternalID,
				&row.Result, &row.CreatedAt, &row.UpdatedAt); err != nil {
				return fmt.Errorf("read unknown task operation: %w", err)
			}
			result = append(result, taskOperationDomain(row))
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate unknown task operations: %w", err)
		}
		return nil
	})
	return result, err
}

func (s *Store) UpdateBatchItem(ctx context.Context, ownerID string, item task.BatchItem, leaseOwner string, leaseVersion int64) (task.BatchItem, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidBatchItemID(item.ID) || !domain.ValidTaskID(item.TaskID) ||
		item.Key == "" || len(item.Key) > 128 || leaseOwner == "" || leaseVersion < 1 || !validBatchItemStatus(item.Status) {
		return task.BatchItem{}, domain.InvalidArgument("batch_item", "batch item update fields are invalid")
	}
	var result task.BatchItem
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		taskRow, err := loadOwnedTaskForUpdate(ctx, tx, ownerID, item.TaskID)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("lock task for batch item update: %w", err)
		}
		if err := requireCurrentLease(ctx, tx, taskRow, leaseOwner, leaseVersion); err != nil {
			return err
		}

		var current sqlcgen.TaskBatchItem
		err = tx.QueryRow(ctx, `SELECT id, task_id, item_key, input, status, result, error, created_at, updated_at, attempt
FROM task_batch_items WHERE id=$1 AND task_id=$2 FOR UPDATE`, item.ID, item.TaskID).Scan(
			&current.ID, &current.TaskID, &current.ItemKey, &current.Input, &current.Status,
			&current.Result, &current.Error, &current.CreatedAt, &current.UpdatedAt, &current.Attempt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "task batch item not found")
		}
		if err != nil {
			return fmt.Errorf("read batch item for update: %w", err)
		}
		if current.ItemKey != item.Key || current.Input != item.Input || current.Attempt != item.Attempt {
			return domain.NewError(domain.CodeInvalidState, "batch item identity cannot be changed")
		}
		if current.Status == string(item.Status) {
			if current.Result == item.Result && current.Error == item.Error {
				result = taskBatchItemDomain(current)
				return nil
			}
			return domain.NewError(domain.CodeInvalidState, "batch item result changed after status was committed")
		}
		if !canTransitionBatchItem(task.BatchItemStatus(current.Status), item.Status) {
			conflict := domain.NewError(domain.CodeInvalidState, "batch item status transition is invalid")
			conflict.Details["status"] = current.Status
			return conflict
		}
		err = tx.QueryRow(ctx, `UPDATE task_batch_items SET status=$3, result=$4, error=$5, updated_at=clock_timestamp()
WHERE id=$1 AND task_id=$2
RETURNING id, task_id, item_key, input, status, result, error, created_at, updated_at, attempt`,
			item.ID, item.TaskID, string(item.Status), item.Result, item.Error).Scan(
			&current.ID, &current.TaskID, &current.ItemKey, &current.Input, &current.Status,
			&current.Result, &current.Error, &current.CreatedAt, &current.UpdatedAt, &current.Attempt)
		if err != nil {
			return fmt.Errorf("persist batch item update: %w", err)
		}
		payload, err := json.Marshal(map[string]any{
			"batch_item_id": item.ID,
			"key":           item.Key,
			"status":        string(item.Status),
			"result":        item.Result,
			"error":         item.Error,
			"attempt":       item.Attempt,
		})
		if err != nil {
			return fmt.Errorf("encode batch item event: %w", err)
		}
		sequence, err := incrementTaskEventSequence(ctx, tx, item.TaskID)
		if err != nil {
			return err
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: item.TaskID, Sequence: sequence, Type: "task.batch_item", Payload: payload}); err != nil {
			return err
		}
		result = taskBatchItemDomain(current)
		return nil
	})
	return result, err
}

// RetryFailedBatchItem queues a failed batch item for a new attempt. The task
// row lock serializes retries with different idempotency keys; the idempotency
// record, task reset, item reset, and both events commit as one transaction.
func (s *Store) RetryFailedBatchItem(ctx context.Context, idempotency app.IdempotencyRecord, taskID, batchItemID string) error {
	if !domain.ValidTaskID(taskID) || !domain.ValidBatchItemID(batchItemID) {
		return domain.InvalidArgument("batch_item", "task and batch item ids are invalid")
	}
	if err := validateIdempotency(idempotency); err != nil {
		return err
	}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		stored, replay, err := findIdempotency(ctx, tx, idempotency)
		if err != nil {
			return err
		}
		if replay {
			if stored.ResourceID != taskID || stored.RelatedID != batchItemID {
				return idempotencyConflict()
			}
			return nil
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, idempotency.OwnerID, nil)
		if err != nil {
			return err
		}

		taskRow, err := loadOwnedTaskForUpdate(ctx, tx, idempotency.OwnerID, taskID)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("lock task for batch item retry: %w", err)
		}
		var taskEpoch int64
		if err := tx.QueryRow(ctx, `SELECT execution_epoch FROM tasks WHERE id=$1 AND owner_id=$2`, taskID, idempotency.OwnerID).Scan(&taskEpoch); err != nil {
			return fmt.Errorf("read task execution epoch before batch retry: %w", err)
		}
		if taskEpoch != gate.epoch {
			return ownerExecutionDisabledError(gate)
		}
		if _, replay, err := reserveIdempotency(ctx, tx, idempotency, taskID, batchItemID); err != nil {
			return err
		} else if replay {
			return nil
		}
		if taskRow.Status != string(task.StatusFailed) {
			return domain.NewError(domain.CodeInvalidState, "only a failed task can retry a batch item")
		}
		if taskRow.LeaseVersion == int64(^uint64(0)>>1) {
			return domain.NewError(domain.CodeInvalidState, "task lease version is exhausted")
		}

		var item sqlcgen.TaskBatchItem
		err = tx.QueryRow(ctx, `SELECT id, task_id, item_key, input, status, result, error, created_at, updated_at, attempt
FROM task_batch_items WHERE task_id=$1 AND id=$2 FOR UPDATE`, taskID, batchItemID).Scan(
			&item.ID, &item.TaskID, &item.ItemKey, &item.Input, &item.Status, &item.Result,
			&item.Error, &item.CreatedAt, &item.UpdatedAt, &item.Attempt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "task batch item not found")
		}
		if err != nil {
			return fmt.Errorf("read batch item for retry: %w", err)
		}
		if item.Status != string(task.BatchItemFailed) {
			return domain.NewError(domain.CodeInvalidState, "only a failed batch item can be retried")
		}
		if item.Attempt < 1 || item.Attempt == int32(^uint32(0)>>1) {
			return domain.NewError(domain.CodeInvalidState, "batch item attempt is exhausted or invalid")
		}

		var totalItems, nonTerminalItems int64
		if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status NOT IN ('succeeded','failed'))
FROM task_batch_items WHERE task_id=$1`, taskID).Scan(&totalItems, &nonTerminalItems); err != nil {
			return fmt.Errorf("check batch item retry eligibility: %w", err)
		}
		if totalItems == 0 || nonTerminalItems != 0 {
			return domain.NewError(domain.CodeInvalidState, "all other batch items must be terminal before retry")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM task_artifacts WHERE task_id=$1 AND batch_item_id=$2`, taskID, batchItemID); err != nil {
			return fmt.Errorf("discard artifacts from failed batch attempt: %w", err)
		}
		var unresolvedOperations bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM task_operations WHERE task_id=$1 AND status IN ('dispatched','unknown')
)`, taskID).Scan(&unresolvedOperations); err != nil {
			return fmt.Errorf("check unresolved operations before batch retry: %w", err)
		}
		if unresolvedOperations {
			return domain.NewError(domain.CodeInvalidState, "task has an unresolved operation and cannot retry a batch item")
		}

		_, err = tx.Exec(ctx, `UPDATE task_batch_items
SET status='queued', attempt=attempt+1, result='', error='', updated_at=clock_timestamp()
WHERE task_id=$1 AND id=$2`, taskID, batchItemID)
		if err != nil {
			return fmt.Errorf("reset failed batch item for retry: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET status='queued', current_step='', result='', waiting_reason='',
lease_owner='', lease_version=lease_version+1, lease_expires_at=NULL, finished_at=NULL, updated_at=clock_timestamp()
WHERE id=$1`, taskID); err != nil {
			return fmt.Errorf("queue task for batch retry: %w", err)
		}

		statusPayload, err := json.Marshal(map[string]any{
			"status": string(task.StatusQueued), "reason": "batch_item_retry",
			"batch_item_id": batchItemID, "batch_attempt": item.Attempt + 1,
		})
		if err != nil {
			return fmt.Errorf("encode batch retry status event: %w", err)
		}
		sequence, err := incrementTaskEventSequence(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: sequence, Type: "task.status", Payload: statusPayload}); err != nil {
			return err
		}
		batchPayload, err := json.Marshal(map[string]any{
			"batch_item_id": batchItemID, "key": item.ItemKey, "status": string(task.BatchItemQueued),
			"attempt": item.Attempt + 1, "result": "", "error": "",
		})
		if err != nil {
			return fmt.Errorf("encode batch retry item event: %w", err)
		}
		sequence, err = incrementTaskEventSequence(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: sequence, Type: "task.batch_item", Payload: batchPayload}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

// RecordValidatedArtifact persists a batch artifact with a proof of its
// committed Workspace revision. Runtime and block readback occur in the app;
// this transaction rechecks the immutable manifest before accepting the row.
func (s *Store) RecordValidatedArtifact(ctx context.Context, ownerID string, artifact task.Artifact, leaseOwner string, leaseVersion int64) (task.Artifact, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidArtifactID(artifact.ID) || !domain.ValidTaskID(artifact.TaskID) || !domain.ValidStepID(artifact.StepID) ||
		strings.TrimSpace(artifact.Label) == "" || !validWorkspaceRelativeArtifactPath(artifact.Path) || leaseOwner == "" || leaseVersion < 1 ||
		!validTaskArtifactProof(artifact) ||
		(artifact.BatchItemID == "" && artifact.BatchAttempt != 0) ||
		(artifact.BatchItemID != "" && (!domain.ValidBatchItemID(artifact.BatchItemID) || artifact.BatchAttempt < 1)) {
		return task.Artifact{}, domain.InvalidArgument("artifact", "validated artifact metadata is invalid")
	}
	var result task.Artifact
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		taskRow, err := loadOwnedTaskForUpdate(ctx, tx, ownerID, artifact.TaskID)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("lock task for artifact record: %w", err)
		}
		if err := requireCurrentLease(ctx, tx, taskRow, leaseOwner, leaseVersion); err != nil {
			return err
		}
		var stepExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_steps WHERE task_id=$1 AND id=$2)`, artifact.TaskID, artifact.StepID).Scan(&stepExists); err != nil {
			return fmt.Errorf("check artifact source step: %w", err)
		}
		if !stepExists {
			return domain.NewError(domain.CodeNotFound, "artifact source step not found")
		}
		if artifact.BatchItemID != "" {
			var attempt int32
			err := tx.QueryRow(ctx, `SELECT attempt FROM task_batch_items WHERE task_id=$1 AND id=$2`, artifact.TaskID, artifact.BatchItemID).Scan(&attempt)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "artifact batch item not found")
			}
			if err != nil {
				return fmt.Errorf("check artifact batch item: %w", err)
			}
			if attempt != artifact.BatchAttempt {
				return domain.NewError(domain.CodeInvalidState, "artifact batch attempt does not match the current item attempt")
			}
		}
		if err := validateTaskArtifactWorkspaceProof(ctx, tx, ownerID, artifact); err != nil {
			return err
		}
		result, err = recordTaskArtifactTx(ctx, tx, artifact)
		if err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func validTaskArtifactProof(artifact task.Artifact) bool {
	return domain.ValidWorkspaceID(artifact.WorkspaceID) && artifact.Revision >= 0 && artifact.SizeBytes >= 0 && validPostgresSHA256(artifact.SHA256)
}

func validateTaskArtifactWorkspaceProof(ctx context.Context, tx pgx.Tx, ownerID string, artifact task.Artifact) error {
	if !validTaskArtifactProof(artifact) {
		return domain.InvalidArgument("artifact.proof", "workspace artifact proof fields are invalid")
	}
	var matches bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1
  FROM tasks t
  JOIN workspaces w ON w.id=$3 AND w.owner_id=t.owner_id AND w.deleted_at IS NULL
  JOIN workspace_revision_files f ON f.workspace_id=$3 AND f.file_path=$4
    AND f.valid_from_revision <= $5 AND (f.valid_to_revision IS NULL OR f.valid_to_revision > $5)
    AND f.size_bytes=$6 AND f.sha256=$7
  WHERE t.id=$1 AND t.owner_id=$2 AND t.workspace_id=$3
)`, artifact.TaskID, ownerID, artifact.WorkspaceID, artifact.Path, artifact.Revision, artifact.SizeBytes, artifact.SHA256).Scan(&matches)
	if err != nil {
		return fmt.Errorf("verify task artifact workspace manifest proof: %w", err)
	}
	if !matches {
		return domain.NewError(domain.CodeInvalidState, "artifact proof does not match a committed Task Workspace revision")
	}
	return nil
}

func recordTaskArtifactTx(ctx context.Context, tx pgx.Tx, artifact task.Artifact) (task.Artifact, error) {
	var current sqlcgen.TaskArtifact
	current, err := loadTaskArtifactByPath(ctx, tx, artifact)
	if err == nil {
		if !sameArtifact(current, artifact) {
			return task.Artifact{}, domain.NewError(domain.CodeInvalidState, "artifact path already has different metadata")
		}
		return taskArtifactDomain(current), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return task.Artifact{}, fmt.Errorf("check existing artifact path: %w", err)
	}
	err = scanTaskArtifact(tx.QueryRow(ctx, `SELECT id, task_id, step_id, label, path, media_type, created_at, batch_item_id, batch_attempt,
workspace_id, workspace_revision, size_bytes, sha256
FROM task_artifacts WHERE id=$1 FOR UPDATE`, artifact.ID), &current)
	if err == nil {
		if !sameArtifact(current, artifact) {
			return task.Artifact{}, domain.NewError(domain.CodeInvalidState, "artifact identity cannot be changed")
		}
		return taskArtifactDomain(current), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return task.Artifact{}, fmt.Errorf("check existing artifact metadata: %w", err)
	}
	inserted, err := tx.Exec(ctx, `INSERT INTO task_artifacts
(id, task_id, step_id, label, path, media_type, batch_item_id, batch_attempt, workspace_id, workspace_revision, size_bytes, sha256)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT DO NOTHING`,
		artifact.ID, artifact.TaskID, artifact.StepID, artifact.Label, artifact.Path, artifact.MediaType,
		nullableBatchItemID(artifact.BatchItemID), nullableBatchAttempt(artifact.BatchItemID, artifact.BatchAttempt),
		artifact.WorkspaceID, artifact.Revision, artifact.SizeBytes, artifact.SHA256)
	if err != nil {
		return task.Artifact{}, fmt.Errorf("record validated artifact proof: %w", err)
	}
	if inserted.RowsAffected() == 0 {
		current, err = loadTaskArtifactByPath(ctx, tx, artifact)
		if err == nil && sameArtifact(current, artifact) {
			return taskArtifactDomain(current), nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return task.Artifact{}, fmt.Errorf("read artifact path after conflict: %w", err)
		}
		return task.Artifact{}, domain.NewError(domain.CodeInvalidState, "artifact identity is already in use")
	}
	if err := scanTaskArtifact(tx.QueryRow(ctx, `SELECT id, task_id, step_id, label, path, media_type, created_at, batch_item_id, batch_attempt,
workspace_id, workspace_revision, size_bytes, sha256
FROM task_artifacts WHERE id=$1`, artifact.ID), &current); err != nil {
		return task.Artifact{}, fmt.Errorf("read recorded artifact proof: %w", err)
	}
	payloadFields := map[string]any{
		"artifact_id": artifact.ID, "step_id": artifact.StepID, "label": artifact.Label,
		"media_type": artifact.MediaType, "path": artifact.Path, "workspace_id": artifact.WorkspaceID,
		"revision": artifact.Revision, "size_bytes": artifact.SizeBytes, "sha256": artifact.SHA256,
	}
	if artifact.BatchItemID != "" {
		payloadFields["batch_item_id"] = artifact.BatchItemID
		payloadFields["batch_attempt"] = artifact.BatchAttempt
	}
	payload, err := json.Marshal(payloadFields)
	if err != nil {
		return task.Artifact{}, fmt.Errorf("encode artifact event: %w", err)
	}
	sequence, err := incrementTaskEventSequence(ctx, tx, artifact.TaskID)
	if err != nil {
		return task.Artifact{}, err
	}
	if err := insertTaskEvent(ctx, tx, task.Event{TaskID: artifact.TaskID, Sequence: sequence, Type: "task.artifact", Payload: payload}); err != nil {
		return task.Artifact{}, err
	}
	return taskArtifactDomain(current), nil
}

func persistTaskProgressArtifacts(ctx context.Context, tx pgx.Tx, ownerID, taskID string, artifacts []task.Artifact) error {
	if len(artifacts) > 64 {
		return domain.InvalidArgument("artifacts", "task artifact count exceeds the limit")
	}
	seen := make(map[string]struct{}, len(artifacts))
	for _, artifact := range artifacts {
		if artifact.TaskID != taskID || !domain.ValidArtifactID(artifact.ID) || !domain.ValidStepID(artifact.StepID) ||
			strings.TrimSpace(artifact.Label) == "" || !validWorkspaceRelativeArtifactPath(artifact.Path) || !validTaskArtifactProof(artifact) ||
			(artifact.BatchItemID == "" && artifact.BatchAttempt != 0) ||
			(artifact.BatchItemID != "" && (!domain.ValidBatchItemID(artifact.BatchItemID) || artifact.BatchAttempt < 1)) {
			return domain.InvalidArgument("artifacts", "task artifact fields are invalid")
		}
		key := artifact.BatchItemID + "\x00" + fmt.Sprint(artifact.BatchAttempt) + "\x00" + artifact.Path
		if _, duplicate := seen[key]; duplicate {
			return domain.InvalidArgument("artifacts", "task artifact paths must be unique")
		}
		seen[key] = struct{}{}
		if err := validateTaskArtifactWorkspaceProof(ctx, tx, ownerID, artifact); err != nil {
			return err
		}
		if _, err := recordTaskArtifactTx(ctx, tx, artifact); err != nil {
			return err
		}
	}
	return nil
}

func validateCompletedTaskArtifacts(ctx context.Context, tx pgx.Tx, ownerID, taskID string) error {
	var invalid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1
  FROM task_artifacts a
  JOIN tasks t ON t.id=a.task_id AND t.owner_id=$2
  LEFT JOIN workspaces w ON w.id=a.workspace_id AND w.owner_id=t.owner_id AND w.deleted_at IS NULL
  LEFT JOIN workspace_revision_files f ON f.workspace_id=a.workspace_id AND f.file_path=a.path
    AND f.valid_from_revision <= a.workspace_revision
    AND (f.valid_to_revision IS NULL OR f.valid_to_revision > a.workspace_revision)
    AND f.size_bytes=a.size_bytes AND f.sha256=a.sha256
  LEFT JOIN task_batch_items bi ON bi.task_id=a.task_id AND bi.id=a.batch_item_id
  WHERE a.task_id=$1 AND (
    a.workspace_id IS NULL OR a.workspace_revision IS NULL OR a.size_bytes IS NULL OR a.sha256 IS NULL
    OR t.workspace_id IS NULL OR a.workspace_id <> t.workspace_id OR w.id IS NULL OR f.file_path IS NULL
    OR (a.batch_item_id IS NOT NULL AND (bi.id IS NULL OR bi.attempt <> a.batch_attempt OR bi.status <> 'succeeded'))
  )
)`, taskID, ownerID).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("verify completed task artifact proofs: %w", err)
	}
	if invalid {
		return domain.NewError(domain.CodeInvalidState, "task cannot complete with an unproved or stale Workspace artifact")
	}
	return nil
}

func loadOwnedOperationByKey(ctx context.Context, tx pgx.Tx, ownerID, taskID, key string) (task.Operation, error) {
	var row sqlcgen.TaskOperation
	err := tx.QueryRow(ctx, `SELECT o.id, o.task_id, o.operation_key, o.request_hash, o.host_id, o.workspace_id, o.binding_version, o.input_revision,
o.runtime_generation, o.runtime_instance_id, o.control_version, o.lease_version, o.runtime_lease_version, o.status, o.external_id, o.result, o.created_at, o.updated_at
FROM task_operations o JOIN tasks t ON t.id=o.task_id
WHERE t.owner_id=$1 AND o.task_id=$2 AND o.operation_key=$3`, ownerID, taskID, key).Scan(
		&row.ID, &row.TaskID, &row.OperationKey, &row.RequestHash, &row.HostID, &row.WorkspaceID, &row.BindingVersion, &row.InputRevision, &row.RuntimeGeneration,
		&row.RuntimeInstanceID, &row.ControlVersion, &row.LeaseVersion, &row.RuntimeLeaseVersion, &row.Status,
		&row.ExternalID, &row.Result, &row.CreatedAt, &row.UpdatedAt)
	if err != nil {
		return task.Operation{}, err
	}
	return taskOperationDomain(row), nil
}

func loadOwnedTaskForUpdate(ctx context.Context, tx pgx.Tx, ownerID, taskID string) (sqlcgen.Task, error) {
	var row sqlcgen.Task
	err := tx.QueryRow(ctx, `SELECT id, owner_id, conversation_id, host_id, model_id, objective, status, auth_kind, auth_token_id, auth_scopes, auth_client_installation_id,
budget_active_seconds, budget_tool_calls, budget_model_tokens, usage_active_seconds, usage_tool_calls, usage_model_tokens, lease_owner,
lease_version, lease_expires_at, event_seq, current_step, result, waiting_reason, created_at, updated_at, finished_at, application_id
FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, taskID, ownerID).Scan(
		&row.ID, &row.OwnerID, &row.ConversationID, &row.HostID, &row.ModelID, &row.Objective, &row.Status,
		&row.AuthKind, &row.AuthTokenID, &row.AuthScopes, &row.AuthClientInstallationID, &row.BudgetActiveSeconds, &row.BudgetToolCalls,
		&row.BudgetModelTokens, &row.UsageActiveSeconds, &row.UsageToolCalls, &row.UsageModelTokens,
		&row.LeaseOwner, &row.LeaseVersion, &row.LeaseExpiresAt, &row.EventSeq, &row.CurrentStep,
		&row.Result, &row.WaitingReason, &row.CreatedAt, &row.UpdatedAt, &row.FinishedAt, &row.ApplicationID)
	return row, err
}

func validBatchItemStatus(status task.BatchItemStatus) bool {
	switch status {
	case task.BatchItemQueued, task.BatchItemRunning, task.BatchItemSucceeded, task.BatchItemFailed, task.BatchItemUnknown:
		return true
	default:
		return false
	}
}

func canTransitionBatchItem(from, to task.BatchItemStatus) bool {
	switch from {
	case task.BatchItemQueued:
		return to == task.BatchItemRunning || to == task.BatchItemSucceeded || to == task.BatchItemFailed || to == task.BatchItemUnknown
	case task.BatchItemRunning:
		return to == task.BatchItemSucceeded || to == task.BatchItemFailed || to == task.BatchItemUnknown
	case task.BatchItemUnknown:
		return to == task.BatchItemRunning || to == task.BatchItemSucceeded || to == task.BatchItemFailed
	default:
		return false
	}
}

func validWorkspaceRelativeArtifactPath(value string) bool {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, `\`) || path.IsAbs(value) {
		return false
	}
	clean := path.Clean(value)
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, "../") && clean == value
}

func sameArtifact(current sqlcgen.TaskArtifact, candidate task.Artifact) bool {
	batchItemID := ""
	if current.BatchItemID.Valid {
		batchItemID = current.BatchItemID.String
	}
	batchAttempt := int32(0)
	if current.BatchAttempt.Valid {
		batchAttempt = current.BatchAttempt.Int32
	}
	return current.TaskID == candidate.TaskID && current.StepID == candidate.StepID && current.Label == candidate.Label &&
		current.Path == candidate.Path && current.MediaType == candidate.MediaType && batchItemID == candidate.BatchItemID &&
		batchAttempt == candidate.BatchAttempt && current.WorkspaceID.Valid && current.WorkspaceID.String == candidate.WorkspaceID &&
		current.WorkspaceRevision.Valid && current.WorkspaceRevision.Int64 == candidate.Revision &&
		current.SizeBytes.Valid && current.SizeBytes.Int64 == candidate.SizeBytes && current.Sha256.Valid && current.Sha256.String == candidate.SHA256
}

func scanTaskArtifact(row pgx.Row, result *sqlcgen.TaskArtifact) error {
	return row.Scan(&result.ID, &result.TaskID, &result.StepID, &result.Label, &result.Path,
		&result.MediaType, &result.CreatedAt, &result.BatchItemID, &result.BatchAttempt, &result.WorkspaceID,
		&result.WorkspaceRevision, &result.SizeBytes, &result.Sha256)
}

func loadTaskArtifactByPath(ctx context.Context, tx pgx.Tx, candidate task.Artifact) (sqlcgen.TaskArtifact, error) {
	var result sqlcgen.TaskArtifact
	err := scanTaskArtifact(tx.QueryRow(ctx, `SELECT id, task_id, step_id, label, path, media_type, created_at, batch_item_id, batch_attempt,
workspace_id, workspace_revision, size_bytes, sha256
FROM task_artifacts
WHERE task_id=$1 AND path=$2 AND batch_item_id IS NOT DISTINCT FROM $3 AND batch_attempt IS NOT DISTINCT FROM $4
FOR UPDATE`, candidate.TaskID, candidate.Path, nullableBatchItemID(candidate.BatchItemID),
		nullableBatchAttempt(candidate.BatchItemID, candidate.BatchAttempt)), &result)
	return result, err
}

func nullableBatchItemID(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableBatchAttempt(batchItemID string, value int32) any {
	if batchItemID == "" {
		return nil
	}
	return value
}
