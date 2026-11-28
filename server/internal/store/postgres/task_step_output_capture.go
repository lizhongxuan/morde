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
	"morde/server/internal/domain/project"
	"morde/server/internal/store/postgres/sqlcgen"
)

const (
	taskStepOutputCaptureRetention           = 24 * time.Hour
	taskStepOutputCaptureMaxRowsPerOwner     = 10000
	taskStepOutputCaptureStatusOwnerLockSeed = 280
)

func (s *Store) SaveTaskStepOutputCapture(ctx context.Context, value app.TaskStepOutputCaptureRecord) error {
	if s == nil || s.pool == nil || ctx == nil || !app.ValidTaskStepOutputCaptureRecordForStore(value) {
		return domain.InvalidArgument("task_step_output_capture", "supplemental output capture is invalid")
	}
	return withTx(ctx, s.pool.Begin, func(tx pgx.Tx) error {
		var lockedStepID string
		if err := tx.QueryRow(ctx, `SELECT id FROM task_steps WHERE task_id=$1 AND id=$2 FOR UPDATE`, value.Identity.TaskID, value.Identity.StepID).Scan(&lockedStepID); err != nil {
			return domain.NewError(domain.CodeInvalidState, "supplemental output capture step is unavailable")
		}
		// Serialize the per-owner capacity check and receipt write. The advisory
		// lock key is a hash only; no output-derived value participates in it.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 279))`, value.Identity.OwnerID); err != nil {
			return fmt.Errorf("lock supplemental output capture quota: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM task_step_output_captures WHERE owner_id=$1 AND expires_at<=clock_timestamp()`, value.Identity.OwnerID); err != nil {
			return fmt.Errorf("expire supplemental output captures: %w", err)
		}
		var ownerBytes int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(storage_bytes),0) FROM task_step_output_captures
WHERE owner_id=$1 AND expires_at>clock_timestamp()`, value.Identity.OwnerID).Scan(&ownerBytes); err != nil {
			return fmt.Errorf("check supplemental output capture quota: %w", err)
		}
		var ownerRows int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_step_output_captures
WHERE owner_id=$1 AND expires_at>clock_timestamp()`, value.Identity.OwnerID).Scan(&ownerRows); err != nil {
			return fmt.Errorf("check supplemental output capture row quota: %w", err)
		}
		storageBytes := taskStepOutputCaptureStorageBytes(value)
		if storageBytes < 0 || ownerBytes > int64(app.TaskStepOutputCaptureOwnerQuotaBytes-storageBytes) ||
			ownerRows >= taskStepOutputCaptureMaxRowsPerOwner {
			return domain.NewError(domain.CodeQuotaExceeded, "supplemental output storage quota is full")
		}
		if err := verifyTaskStepOutputCaptureFence(ctx, tx, value.Identity); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM task_step_output_capture_statuses WHERE task_id=$1 AND step_id=$2`,
			value.Identity.TaskID, value.Identity.StepID); err != nil {
			return fmt.Errorf("clear incomplete supplemental output status: %w", err)
		}
		stdout, stderr := value.Stdout, value.Stderr
		// Use one stable statement clock for both timestamps; two independent
		// clock_timestamp calls can cross the exact 24-hour database constraint.
		_, err := tx.Exec(ctx, `INSERT INTO task_step_output_captures
(task_id,owner_id,step_id,operation_id,call_id,task_revision,authorization_version,workspace_id,
 workspace_authorization_version,workspace_binding_version,workspace_view_id,workspace_checkpoint_revision,
 stdout_source_bytes,stdout_total_bytes,stdout_sha256,stderr_source_bytes,stderr_total_bytes,stderr_sha256,
 stored_bytes,storage_bytes,page_count,state,created_at,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,'complete',statement_timestamp(),statement_timestamp()+interval '24 hours')`,
			value.Identity.TaskID, value.Identity.OwnerID, value.Identity.StepID, value.Identity.OperationID, value.Identity.CallID,
			value.Identity.TaskRevision, value.Identity.AuthorizationVersion, value.Identity.WorkspaceID,
			value.Identity.WorkspaceAuthorizationVersion, value.Identity.WorkspaceBindingVersion, value.Identity.WorkspaceViewID,
			value.Identity.WorkspaceCheckpointRevision, stdout.SourceBytes, stdout.TotalBytes, stdout.SHA256,
			stderr.SourceBytes, stderr.TotalBytes, stderr.SHA256, stdout.TotalBytes+stderr.TotalBytes,
			storageBytes, len(stdout.Pages)+len(stderr.Pages))
		if err != nil {
			return storeError("save supplemental output capture receipt", "task_step_output_capture", err)
		}
		for _, stream := range []app.TaskStepOutputCaptureStreamRecord{stdout, stderr} {
			for _, page := range stream.Pages {
				if _, err := tx.Exec(ctx, `INSERT INTO task_step_output_capture_pages
(task_id,step_id,stream,page_index,page_offset,plaintext_bytes,page_sha256,payload_ciphertext)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, value.Identity.TaskID, value.Identity.StepID, stream.Stream,
					page.PageIndex, page.Offset, page.PlaintextBytes, page.PageSHA256, page.Ciphertext); err != nil {
					return storeError("save supplemental output capture page", "task_step_output_capture", err)
				}
			}
		}
		return nil
	})
}

func (s *Store) SaveTaskStepOutputCaptureStatus(ctx context.Context, value app.TaskStepOutputCaptureStatusRecord) error {
	if s == nil || s.pool == nil || ctx == nil || !app.ValidTaskStepOutputCaptureStatusRecordForStore(value) {
		return domain.InvalidArgument("task_step_output_capture_status", "supplemental output status is invalid")
	}
	return withTx(ctx, s.pool.Begin, func(tx pgx.Tx) error {
		var lockedStepID string
		if err := tx.QueryRow(ctx, `SELECT id FROM task_steps WHERE task_id=$1 AND id=$2 FOR UPDATE`, value.Identity.TaskID, value.Identity.StepID).Scan(&lockedStepID); err != nil {
			return domain.NewError(domain.CodeInvalidState, "supplemental output capture step is unavailable")
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,$2))`,
			value.Identity.OwnerID, taskStepOutputCaptureStatusOwnerLockSeed); err != nil {
			return fmt.Errorf("lock supplemental output status quota: %w", err)
		}
		if err := verifyTaskStepOutputCaptureFence(ctx, tx, value.Identity); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM task_step_output_captures WHERE task_id=$1 AND step_id=$2 AND expires_at<=clock_timestamp()`,
			value.Identity.TaskID, value.Identity.StepID); err != nil {
			return fmt.Errorf("expire supplemental output receipt before status save: %w", err)
		}
		var complete bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_step_output_captures WHERE task_id=$1 AND step_id=$2 AND state='complete' AND expires_at>clock_timestamp())`,
			value.Identity.TaskID, value.Identity.StepID).Scan(&complete); err != nil {
			return fmt.Errorf("check supplemental output receipt before status save: %w", err)
		}
		if complete {
			return domain.NewError(domain.CodeInvalidState, "complete supplemental output receipt already exists")
		}
		var existing bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_step_output_capture_statuses
WHERE task_id=$1 AND owner_id=$2 AND step_id=$3)`, value.Identity.TaskID, value.Identity.OwnerID, value.Identity.StepID).Scan(&existing); err != nil {
			return fmt.Errorf("check supplemental output status identity: %w", err)
		}
		if !existing {
			// Status rows contain no output bytes. If the owner has reached the
			// bounded row cap, evict the oldest metadata row so the latest status
			// remains observable; the 24-hour expiry remains an upper bound.
			var ownerRows int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_step_output_capture_statuses
WHERE owner_id=$1 AND expires_at>clock_timestamp()`, value.Identity.OwnerID).Scan(&ownerRows); err != nil {
				return fmt.Errorf("check supplemental output status row quota: %w", err)
			}
			if ownerRows >= taskStepOutputCaptureMaxRowsPerOwner {
				if _, err := tx.Exec(ctx, `DELETE FROM task_step_output_capture_statuses
WHERE (task_id,step_id) IN (
 SELECT task_id,step_id FROM task_step_output_capture_statuses
 WHERE owner_id=$1 AND expires_at>clock_timestamp() ORDER BY created_at,task_id,step_id LIMIT 1
)`, value.Identity.OwnerID); err != nil {
					return fmt.Errorf("evict oldest supplemental output status: %w", err)
				}
			}
		}
		// Keep both timestamps on one stable statement clock to satisfy the
		// table's exact 24-hour retention bound under concurrent writes.
		_, err := tx.Exec(ctx, `INSERT INTO task_step_output_capture_statuses
(task_id,owner_id,step_id,operation_id,call_id,task_revision,authorization_version,workspace_id,
 workspace_authorization_version,workspace_binding_version,workspace_view_id,workspace_checkpoint_revision,state,created_at,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,statement_timestamp(),statement_timestamp()+interval '24 hours')
ON CONFLICT (task_id,step_id) DO UPDATE SET
 owner_id=EXCLUDED.owner_id,operation_id=EXCLUDED.operation_id,call_id=EXCLUDED.call_id,task_revision=EXCLUDED.task_revision,
 authorization_version=EXCLUDED.authorization_version,workspace_id=EXCLUDED.workspace_id,
 workspace_authorization_version=EXCLUDED.workspace_authorization_version,workspace_binding_version=EXCLUDED.workspace_binding_version,
 workspace_view_id=EXCLUDED.workspace_view_id,workspace_checkpoint_revision=EXCLUDED.workspace_checkpoint_revision,
 state=EXCLUDED.state,created_at=EXCLUDED.created_at,expires_at=EXCLUDED.expires_at`,
			value.Identity.TaskID, value.Identity.OwnerID, value.Identity.StepID, value.Identity.OperationID, value.Identity.CallID,
			value.Identity.TaskRevision, value.Identity.AuthorizationVersion, value.Identity.WorkspaceID,
			value.Identity.WorkspaceAuthorizationVersion, value.Identity.WorkspaceBindingVersion,
			value.Identity.WorkspaceViewID, value.Identity.WorkspaceCheckpointRevision, value.State)
		if err != nil {
			return storeError("save supplemental output capture status", "task_step_output_capture_status", err)
		}
		return nil
	})
}

func verifyTaskStepOutputCaptureFence(ctx context.Context, tx pgx.Tx, identity app.TaskStepOutputCaptureIdentity) error {
	var ownerID, initiatorID, taskWorkspaceID, taskWorkspaceOwnerID, projectID string
	var authVersion, workspaceAuthVersion, taskRevision, desiredRevision, checkpointRevision int64
	var currentRevision, desired pgtype.Int8
	var currentWorkspace pgtype.Text
	var currentWorkspaceOwner pgtype.Text
	var currentCheckpoint pgtype.Int8
	var taskProjectResourceVersion, projectResourceVersion pgtype.Int8
	var memberVersion pgtype.Int8
	var memberRole pgtype.Text
	var stepRevision pgtype.Int8
	var stepCallID, stepToolName, stepPhase, stepStatus pgtype.Text
	var operationRevision pgtype.Int8
	var operationCallID, operationToolName, operationWorkspaceID, operationStatus pgtype.Text
	var operationBindingVersion int64
	var operationCheckpoint pgtype.Int8
	var workspaceOwner, workspaceState pgtype.Text
	var workspaceDeleted pgtype.Timestamptz
	err := tx.QueryRow(ctx, `SELECT t.owner_id,t.initiator_id,t.auth_version,t.current_revision,t.desired_revision,t.workspace_id,
	 t.workspace_owner_id,t.workspace_checkpoint_revision,COALESCE(t.project_id,''),t.workspace_project_resource_version,pr.version,
 pm.version,pm.role,st.task_revision,st.tool_call_key,st.tool_name,st.phase,st.status,
 op.task_revision,op.call_id,op.tool_name,op.workspace_id,op.binding_version,op.workspace_checkpoint_revision,op.status,
 w.owner_id,w.state,w.deleted_at
FROM tasks t
JOIN task_steps st ON st.task_id=t.id AND st.id=$3
JOIN task_operations op ON op.task_id=t.id AND op.id=$4
LEFT JOIN project_members pm ON pm.project_id=t.project_id AND pm.user_id=t.initiator_id
  AND EXISTS (SELECT 1 FROM users u WHERE u.id=pm.user_id AND u.status='approved')
LEFT JOIN project_resources pr ON pr.project_id=t.project_id AND pr.kind='workspace'
  AND pr.resource_id=t.workspace_id AND pr.workspace_id=t.workspace_id AND pr.active
LEFT JOIN workspaces w ON w.id=t.workspace_id
WHERE t.id=$1 AND t.owner_id=$2`, identity.TaskID, identity.OwnerID, identity.StepID, identity.OperationID).Scan(
		&ownerID, &initiatorID, &authVersion, &currentRevision, &desired, &currentWorkspace, &currentWorkspaceOwner, &currentCheckpoint, &projectID,
		&taskProjectResourceVersion, &projectResourceVersion,
		&memberVersion, &memberRole, &stepRevision, &stepCallID, &stepToolName, &stepPhase, &stepStatus,
		&operationRevision, &operationCallID, &operationToolName, &operationWorkspaceID, &operationBindingVersion, &operationCheckpoint, &operationStatus,
		&workspaceOwner, &workspaceState, &workspaceDeleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeInvalidState, "supplemental output capture fence changed")
	}
	if err != nil {
		return fmt.Errorf("verify supplemental output capture fence: %w", err)
	}
	if currentRevision.Valid {
		taskRevision = currentRevision.Int64
	}
	if desired.Valid {
		desiredRevision = desired.Int64
	}
	if currentWorkspaceOwner.Valid {
		taskWorkspaceOwnerID = currentWorkspaceOwner.String
	}
	if currentWorkspace.Valid {
		taskWorkspaceID = currentWorkspace.String
	}
	if currentCheckpoint.Valid {
		checkpointRevision = currentCheckpoint.Int64
	}
	workspaceAuthVersion = 0
	if projectID != "" {
		if !memberVersion.Valid || !memberRole.Valid || (memberRole.String != "owner" && memberRole.String != "member") ||
			!taskProjectResourceVersion.Valid || !projectResourceVersion.Valid || taskProjectResourceVersion.Int64 != projectResourceVersion.Int64 {
			return domain.NewError(domain.CodeInvalidState, "supplemental output capture authorization changed")
		}
		workspaceAuthVersion = memberVersion.Int64
	}
	if ownerID != identity.OwnerID || authVersion != identity.AuthorizationVersion || !currentRevision.Valid || !desired.Valid ||
		taskRevision != identity.TaskRevision || desiredRevision != identity.TaskRevision || taskWorkspaceID != identity.WorkspaceID ||
		workspaceAuthVersion != identity.WorkspaceAuthorizationVersion || checkpointRevision != identity.WorkspaceCheckpointRevision ||
		stepRevision.Int64 != identity.TaskRevision || !stepCallID.Valid || stepCallID.String != identity.CallID ||
		!stepToolName.Valid || stepToolName.String != "shell_exec" || !stepPhase.Valid || stepPhase.String != "execute" ||
		!stepStatus.Valid || stepStatus.String != "succeeded" && stepStatus.String != "failed" ||
		!operationRevision.Valid || operationRevision.Int64 != identity.TaskRevision || !operationCallID.Valid || operationCallID.String != identity.CallID ||
		!operationToolName.Valid || operationToolName.String != "shell_exec" || !operationWorkspaceID.Valid || operationWorkspaceID.String != identity.WorkspaceID ||
		operationBindingVersion != identity.WorkspaceBindingVersion || !operationCheckpoint.Valid || operationCheckpoint.Int64 != identity.WorkspaceCheckpointRevision ||
		!operationStatus.Valid || operationStatus.String != "succeeded" && operationStatus.String != "failed" ||
		!workspaceOwner.Valid || workspaceOwner.String != taskWorkspaceOwnerID || !workspaceState.Valid || workspaceState.String != "ready" || workspaceDeleted.Valid {
		return domain.NewError(domain.CodeInvalidState, "supplemental output capture fence changed")
	}
	if projectID != "" {
		owner, version, err := lockProjectTaskWorkspaceAccess(ctx, tx, projectID, initiatorID, identity.WorkspaceID, false)
		if err != nil || owner != taskWorkspaceOwnerID || version != taskProjectResourceVersion.Int64 {
			return domain.NewError(domain.CodeInvalidState, "supplemental output capture authorization changed")
		}
		if err := verifyProjectTaskWorkspaceGrant(ctx, tx, identity.TaskID, projectID, initiatorID, owner, identity.WorkspaceID); err != nil {
			return domain.NewError(domain.CodeInvalidState, "supplemental output capture authorization changed")
		}
	}
	return nil
}

func taskStepOutputCaptureStorageBytes(value app.TaskStepOutputCaptureRecord) int {
	total := 0
	for _, stream := range []app.TaskStepOutputCaptureStreamRecord{value.Stdout, value.Stderr} {
		for _, page := range stream.Pages {
			total += len(page.Ciphertext)
		}
	}
	return total
}

func (s *Store) GetTaskStepOutputCaptureWorkspaceAuthorizationVersion(ctx context.Context, actorID, taskID, ownerID string) (int64, error) {
	if s == nil || s.pool == nil || ctx == nil || !domain.ValidUserID(actorID) || !domain.ValidTaskID(taskID) || !domain.ValidUserID(ownerID) {
		return 0, domain.NewError(domain.CodeNotFound, "supplemental output authorization is unavailable")
	}
	var version int64
	err := withTx(ctx, s.pool.Begin, func(tx pgx.Tx) error {
		visibleOwner, err := taskOwnerIDForActor(ctx, tx, taskID, actorID, false, false)
		if err != nil || visibleOwner != ownerID {
			return domain.NewError(domain.CodeNotFound, "supplemental output authorization is unavailable")
		}
		row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: taskID, OwnerID: ownerID})
		if err != nil {
			return domain.NewError(domain.CodeNotFound, "supplemental output authorization is unavailable")
		}
		current := rowTaskDomain(row)
		if current.ID != taskID || current.OwnerID != ownerID || current.InitiatorID != actorID || current.WorkspaceID == "" || current.WorkspaceOwnerID == "" {
			return domain.NewError(domain.CodeNotFound, "supplemental output authorization is unavailable")
		}
		if current.ProjectID == "" {
			if current.WorkspaceProjectResourceVersion != nil {
				return domain.NewError(domain.CodeNotFound, "supplemental output authorization is unavailable")
			}
			var workspaceOwner, workspaceState string
			var deletedAt pgtype.Timestamptz
			if err := tx.QueryRow(ctx, `SELECT owner_id,state,deleted_at FROM workspaces WHERE id=$1`, current.WorkspaceID).
				Scan(&workspaceOwner, &workspaceState, &deletedAt); err != nil || workspaceOwner != current.WorkspaceOwnerID ||
				workspaceState != "ready" || deletedAt.Valid {
				return domain.NewError(domain.CodeNotFound, "supplemental output authorization is unavailable")
			}
			version = 0
			return nil
		}
		if current.WorkspaceProjectResourceVersion == nil {
			return domain.NewError(domain.CodeNotFound, "supplemental output authorization is unavailable")
		}
		workspaceOwner, resourceVersion, err := lockProjectTaskWorkspaceAccess(ctx, tx, current.ProjectID, actorID, current.WorkspaceID, false)
		if err != nil || workspaceOwner != current.WorkspaceOwnerID || resourceVersion != *current.WorkspaceProjectResourceVersion {
			return domain.NewError(domain.CodeNotFound, "supplemental output authorization is unavailable")
		}
		if err := verifyProjectTaskWorkspaceGrant(ctx, tx, current.ID, current.ProjectID, current.InitiatorID,
			current.WorkspaceOwnerID, current.WorkspaceID); err != nil {
			return domain.NewError(domain.CodeNotFound, "supplemental output authorization is unavailable")
		}
		var role string
		if err := tx.QueryRow(ctx, `SELECT pm.version,pm.role FROM project_members pm JOIN users u ON u.id=pm.user_id
WHERE pm.project_id=$1 AND pm.user_id=$2 AND u.status='approved'`, current.ProjectID, actorID).Scan(&version, &role); err != nil ||
			version < 1 || role != "owner" && role != "member" {
			return domain.NewError(domain.CodeNotFound, "supplemental output authorization is unavailable")
		}
		return nil
	})
	return version, err
}

// GetTaskStepOutputCaptureStatus is deliberately metadata-only. It returns
// complete only for an atomic encrypted receipt, and rechecks the actor's
// current Task/Workspace fence before returning any persisted state.
func (s *Store) GetTaskStepOutputCaptureStatus(ctx context.Context, actorID, taskID, stepID string) (app.TaskStepOutputCaptureStatusRecord, error) {
	if s == nil || s.pool == nil || ctx == nil || !domain.ValidUserID(actorID) || !domain.ValidTaskID(taskID) || !domain.ValidStepID(stepID) {
		return app.TaskStepOutputCaptureStatusRecord{}, domain.NewError(domain.CodeNotFound, "supplemental output status not found")
	}
	var result app.TaskStepOutputCaptureStatusRecord
	err := withTx(ctx, s.pool.Begin, func(tx pgx.Tx) error {
		ownerID, err := taskOwnerIDForActor(ctx, tx, taskID, actorID, false, false)
		if err != nil {
			return err
		}
		var lockedTaskID string
		if err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id=$1 AND owner_id=$2 FOR SHARE`, taskID, ownerID).Scan(&lockedTaskID); err != nil {
			return domain.NewError(domain.CodeNotFound, "supplemental output status not found")
		}
		readIdentity := func(query string, args ...any) (bool, error) {
			var state string
			err := tx.QueryRow(ctx, query, args...).Scan(
				&result.Identity.OwnerID, &result.Identity.TaskID, &result.Identity.AuthorizationVersion,
				&result.Identity.TaskRevision, &result.Identity.WorkspaceID, &result.Identity.WorkspaceAuthorizationVersion,
				&result.Identity.WorkspaceBindingVersion, &result.Identity.WorkspaceViewID,
				&result.Identity.WorkspaceCheckpointRevision, &result.Identity.StepID, &result.Identity.CallID,
				&result.Identity.OperationID, &state, &result.ExpiresAt)
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			result.State = app.TaskStepOutputCaptureState(state)
			return true, nil
		}
		found, err := readIdentity(`SELECT owner_id,task_id,authorization_version,task_revision,workspace_id,workspace_authorization_version,
 workspace_binding_version,workspace_view_id,workspace_checkpoint_revision,step_id,call_id,operation_id,'complete',expires_at
FROM task_step_output_captures WHERE task_id=$1 AND owner_id=$2 AND step_id=$3 AND state='complete' AND expires_at>clock_timestamp()`,
			taskID, ownerID, stepID)
		if err != nil {
			return fmt.Errorf("read supplemental output receipt status: %w", err)
		}
		if !found {
			found, err = readIdentity(`SELECT owner_id,task_id,authorization_version,task_revision,workspace_id,workspace_authorization_version,
 workspace_binding_version,workspace_view_id,workspace_checkpoint_revision,step_id,call_id,operation_id,state,expires_at
FROM task_step_output_capture_statuses WHERE task_id=$1 AND owner_id=$2 AND step_id=$3 AND expires_at>clock_timestamp()`,
				taskID, ownerID, stepID)
			if err != nil {
				return fmt.Errorf("read supplemental output failure status: %w", err)
			}
		}
		if !found {
			if err := verifyTaskStepOutputCaptureNotRecordedRead(ctx, tx, actorID, ownerID, taskID, stepID); err != nil {
				return err
			}
			result = app.TaskStepOutputCaptureStatusRecord{State: app.TaskStepOutputCaptureStateNotRecorded}
			return nil
		}
		if !app.ValidTaskStepOutputCaptureStatusRecordForStore(result) && result.State != app.TaskStepOutputCaptureStateComplete {
			return domain.NewError(domain.CodeNotFound, "supplemental output status not found")
		}
		if err := verifyTaskStepOutputCaptureReadAccess(ctx, tx, actorID, ownerID, result.Identity); err != nil {
			if domain.CodeOf(err) == domain.CodeForbidden || domain.CodeOf(err) == domain.CodeNotFound || domain.CodeOf(err) == domain.CodeInvalidState {
				return domain.NewError(domain.CodeNotFound, "supplemental output status not found")
			}
			return err
		}
		if err := verifyTaskStepOutputCaptureOperationFence(ctx, tx, result.Identity); err != nil {
			return err
		}
		result.ExpiresAt = result.ExpiresAt.UTC()
		return nil
	})
	return result, err
}

func verifyTaskStepOutputCaptureOperationFence(ctx context.Context, tx pgx.Tx, identity app.TaskStepOutputCaptureIdentity) error {
	var stepRevision, operationRevision, operationBinding, operationCheckpoint pgtype.Int8
	var stepCallID, stepToolName, stepPhase, stepStatus pgtype.Text
	var operationCallID, operationToolName, operationWorkspace, operationStatus pgtype.Text
	err := tx.QueryRow(ctx, `SELECT st.task_revision,st.tool_call_key,st.tool_name,st.phase,st.status,
 op.task_revision,op.call_id,op.tool_name,op.workspace_id,op.binding_version,op.workspace_checkpoint_revision,op.status
FROM task_steps st JOIN task_operations op ON op.task_id=st.task_id
WHERE st.task_id=$1 AND st.id=$2 AND op.id=$3`, identity.TaskID, identity.StepID, identity.OperationID).Scan(
		&stepRevision, &stepCallID, &stepToolName, &stepPhase, &stepStatus,
		&operationRevision, &operationCallID, &operationToolName, &operationWorkspace, &operationBinding, &operationCheckpoint, &operationStatus)
	if err != nil {
		return domain.NewError(domain.CodeNotFound, "supplemental output status not found")
	}
	if !stepRevision.Valid || stepRevision.Int64 != identity.TaskRevision || !stepCallID.Valid || stepCallID.String != identity.CallID ||
		!stepToolName.Valid || stepToolName.String != "shell_exec" || !stepPhase.Valid || stepPhase.String != "execute" ||
		!stepStatus.Valid || stepStatus.String != "succeeded" && stepStatus.String != "failed" ||
		!operationRevision.Valid || operationRevision.Int64 != identity.TaskRevision || !operationCallID.Valid || operationCallID.String != identity.CallID ||
		!operationToolName.Valid || operationToolName.String != "shell_exec" || !operationWorkspace.Valid || operationWorkspace.String != identity.WorkspaceID ||
		!operationBinding.Valid || operationBinding.Int64 != identity.WorkspaceBindingVersion ||
		!operationCheckpoint.Valid || operationCheckpoint.Int64 != identity.WorkspaceCheckpointRevision ||
		!operationStatus.Valid || operationStatus.String != "succeeded" && operationStatus.String != "failed" {
		return domain.NewError(domain.CodeNotFound, "supplemental output status not found")
	}
	return nil
}

func verifyTaskStepOutputCaptureNotRecordedRead(ctx context.Context, tx pgx.Tx, actorID, ownerID, taskID, stepID string) error {
	row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: taskID, OwnerID: ownerID})
	if err != nil {
		return domain.NewError(domain.CodeNotFound, "supplemental output status not found")
	}
	current := rowTaskDomain(row)
	if current.ID != taskID || current.OwnerID != ownerID || current.CurrentRevision == nil || current.DesiredRevision == nil ||
		*current.CurrentRevision != *current.DesiredRevision || current.WorkspaceID == "" || current.WorkspaceCheckpointRevision == nil {
		return domain.NewError(domain.CodeNotFound, "supplemental output status not found")
	}
	var revision pgtype.Int8
	var callID, toolName, phase, status pgtype.Text
	if err := tx.QueryRow(ctx, `SELECT task_revision,tool_call_key,tool_name,phase,status FROM task_steps WHERE task_id=$1 AND id=$2`,
		taskID, stepID).Scan(&revision, &callID, &toolName, &phase, &status); err != nil ||
		!revision.Valid || revision.Int64 != *current.CurrentRevision || !callID.Valid || callID.String == "" ||
		!toolName.Valid || toolName.String != "shell_exec" || !phase.Valid || phase.String != "execute" ||
		!status.Valid || status.String != "succeeded" && status.String != "failed" {
		return domain.NewError(domain.CodeNotFound, "supplemental output status not found")
	}
	if current.ProjectID != "" {
		if current.WorkspaceProjectResourceVersion == nil || current.InitiatorID == "" || current.WorkspaceOwnerID == "" {
			return domain.NewError(domain.CodeNotFound, "supplemental output status not found")
		}
		owner, resourceVersion, err := lockProjectTaskWorkspaceAccess(ctx, tx, current.ProjectID, actorID, current.WorkspaceID, false)
		if err != nil || owner != current.WorkspaceOwnerID || resourceVersion != *current.WorkspaceProjectResourceVersion {
			return domain.NewError(domain.CodeNotFound, "supplemental output status not found")
		}
		if err := verifyProjectTaskWorkspaceGrant(ctx, tx, taskID, current.ProjectID, current.InitiatorID, owner, current.WorkspaceID); err != nil {
			return domain.NewError(domain.CodeNotFound, "supplemental output status not found")
		}
		return nil
	}
	if current.WorkspaceProjectResourceVersion != nil || current.WorkspaceOwnerID == "" {
		return domain.NewError(domain.CodeNotFound, "supplemental output status not found")
	}
	var workspaceOwner, workspaceState string
	var deletedAt pgtype.Timestamptz
	if err := tx.QueryRow(ctx, `SELECT owner_id,state,deleted_at FROM workspaces WHERE id=$1`, current.WorkspaceID).
		Scan(&workspaceOwner, &workspaceState, &deletedAt); err != nil || workspaceOwner != current.WorkspaceOwnerID ||
		workspaceState != "ready" || deletedAt.Valid {
		return domain.NewError(domain.CodeNotFound, "supplemental output status not found")
	}
	return nil
}

func (s *Store) GetTaskStepOutputCapturePage(ctx context.Context, actorID, taskID, stepID, stream string, offset int) (app.TaskStepOutputCapturePageRecord, error) {
	if s == nil || s.pool == nil || ctx == nil || !domain.ValidUserID(actorID) || !domain.ValidTaskID(taskID) ||
		!domain.ValidStepID(stepID) || (stream != "stdout" && stream != "stderr") || offset < 0 || offset > app.TaskStepOutputCaptureMaxBytes {
		return app.TaskStepOutputCapturePageRecord{}, domain.NewError(domain.CodeNotFound, "supplemental output page not found")
	}
	var result app.TaskStepOutputCapturePageRecord
	err := withTx(ctx, s.pool.Begin, func(tx pgx.Tx) error {
		ownerID, err := taskOwnerIDForActor(ctx, tx, taskID, actorID, false, false)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM tasks WHERE id=$1 AND owner_id=$2 FOR SHARE`, taskID, ownerID); err != nil {
			return domain.NewError(domain.CodeNotFound, "supplemental output page not found")
		}
		var pageIndex, pageOffset, pageBytes pgtype.Int4
		var pageDigest, ciphertext []byte
		var expiresAt time.Time
		var stepRevision, operationRevision, operationBinding, operationCheckpoint pgtype.Int8
		var stepCallID, stepToolName, stepPhase, stepStatus pgtype.Text
		var operationCallID, operationToolName, operationWorkspace, operationStatus pgtype.Text
		query := `SELECT c.owner_id,c.task_id,c.authorization_version,c.task_revision,c.workspace_id,c.workspace_authorization_version,
 c.workspace_binding_version,c.workspace_view_id,c.workspace_checkpoint_revision,c.step_id,c.call_id,c.operation_id,
 CASE WHEN $4='stdout' THEN c.stdout_source_bytes ELSE c.stderr_source_bytes END,
 CASE WHEN $4='stdout' THEN c.stdout_total_bytes ELSE c.stderr_total_bytes END,
 CASE WHEN $4='stdout' THEN c.stdout_sha256 ELSE c.stderr_sha256 END,
 p.page_index,p.page_offset,p.plaintext_bytes,p.page_sha256,p.payload_ciphertext,c.expires_at,
 st.task_revision,st.tool_call_key,st.tool_name,st.phase,st.status,
 op.task_revision,op.call_id,op.tool_name,op.workspace_id,op.binding_version,op.workspace_checkpoint_revision,op.status
FROM task_step_output_captures c
JOIN task_steps st ON st.task_id=c.task_id AND st.id=c.step_id
JOIN task_operations op ON op.task_id=c.task_id AND op.id=c.operation_id
LEFT JOIN task_step_output_capture_pages p ON p.task_id=c.task_id AND p.step_id=c.step_id AND p.stream=$4 AND p.page_offset=$5
WHERE c.task_id=$1 AND c.owner_id=$2 AND c.step_id=$3 AND c.state='complete' AND c.expires_at>clock_timestamp()
 AND (($4='stdout' AND c.stdout_total_bytes=0 AND $5=0 AND p.page_offset IS NULL) OR
      ($4='stderr' AND c.stderr_total_bytes=0 AND $5=0 AND p.page_offset IS NULL) OR p.page_offset IS NOT NULL)`
		err = tx.QueryRow(ctx, query, taskID, ownerID, stepID, stream, offset).Scan(
			&result.Identity.OwnerID, &result.Identity.TaskID, &result.Identity.AuthorizationVersion, &result.Identity.TaskRevision,
			&result.Identity.WorkspaceID, &result.Identity.WorkspaceAuthorizationVersion, &result.Identity.WorkspaceBindingVersion,
			&result.Identity.WorkspaceViewID, &result.Identity.WorkspaceCheckpointRevision, &result.Identity.StepID,
			&result.Identity.CallID, &result.Identity.OperationID, &result.SourceBytes, &result.TotalBytes, &result.StreamSHA256,
			&pageIndex, &pageOffset, &pageBytes, &pageDigest, &ciphertext, &expiresAt,
			&stepRevision, &stepCallID, &stepToolName, &stepPhase, &stepStatus,
			&operationRevision, &operationCallID, &operationToolName, &operationWorkspace, &operationBinding, &operationCheckpoint, &operationStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "supplemental output page not found")
		}
		if err != nil {
			return fmt.Errorf("read supplemental output page: %w", err)
		}
		if err := verifyTaskStepOutputCaptureReadAccess(ctx, tx, actorID, ownerID, result.Identity); err != nil {
			if domain.CodeOf(err) == domain.CodeForbidden || domain.CodeOf(err) == domain.CodeNotFound || domain.CodeOf(err) == domain.CodeInvalidState {
				return domain.NewError(domain.CodeNotFound, "supplemental output page not found")
			}
			return err
		}
		if stepRevision.Int64 != result.Identity.TaskRevision || !stepCallID.Valid || stepCallID.String != result.Identity.CallID ||
			!stepToolName.Valid || stepToolName.String != "shell_exec" || !stepPhase.Valid || stepPhase.String != "execute" ||
			!stepStatus.Valid || stepStatus.String != "succeeded" && stepStatus.String != "failed" ||
			!operationRevision.Valid || operationRevision.Int64 != result.Identity.TaskRevision || !operationCallID.Valid || operationCallID.String != result.Identity.CallID ||
			!operationToolName.Valid || operationToolName.String != "shell_exec" || !operationWorkspace.Valid || operationWorkspace.String != result.Identity.WorkspaceID ||
			!operationBinding.Valid || operationBinding.Int64 != result.Identity.WorkspaceBindingVersion ||
			!operationCheckpoint.Valid || operationCheckpoint.Int64 != result.Identity.WorkspaceCheckpointRevision ||
			!operationStatus.Valid || operationStatus.String != "succeeded" && operationStatus.String != "failed" {
			return domain.NewError(domain.CodeNotFound, "supplemental output page not found")
		}
		result.Stream, result.ExpiresAt = stream, expiresAt.UTC()
		if pageOffset.Valid {
			result.PageIndex, result.Offset, result.PlaintextBytes = int(pageIndex.Int32), int(pageOffset.Int32), int(pageBytes.Int32)
			result.PageSHA256, result.Ciphertext = append([]byte(nil), pageDigest...), append([]byte(nil), ciphertext...)
		}
		return nil
	})
	return result, err
}

func verifyTaskStepOutputCaptureReadAccess(ctx context.Context, tx pgx.Tx, actorID, ownerID string,
	identity app.TaskStepOutputCaptureIdentity) error {
	row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: identity.TaskID, OwnerID: ownerID})
	if err != nil {
		return domain.NewError(domain.CodeNotFound, "supplemental output page not found")
	}
	current := rowTaskDomain(row)
	if current.ID != identity.TaskID || current.OwnerID != identity.OwnerID || current.Authorization.Version != identity.AuthorizationVersion ||
		current.CurrentRevision == nil || current.DesiredRevision == nil || *current.CurrentRevision != identity.TaskRevision ||
		*current.DesiredRevision != identity.TaskRevision || current.WorkspaceID != identity.WorkspaceID ||
		current.WorkspaceCheckpointRevision == nil || *current.WorkspaceCheckpointRevision != identity.WorkspaceCheckpointRevision {
		return domain.NewError(domain.CodeNotFound, "supplemental output page not found")
	}
	if current.ProjectID != "" {
		if current.WorkspaceProjectResourceVersion == nil {
			return domain.NewError(domain.CodeNotFound, "supplemental output page not found")
		}
		workspaceOwner, resourceVersion, err := lockProjectTaskWorkspaceAccess(ctx, tx, current.ProjectID, actorID, current.WorkspaceID, false)
		if err != nil || workspaceOwner != current.WorkspaceOwnerID || resourceVersion != *current.WorkspaceProjectResourceVersion {
			return domain.NewError(domain.CodeNotFound, "supplemental output page not found")
		}
		if err := verifyProjectTaskWorkspaceGrant(ctx, tx, current.ID, current.ProjectID, current.InitiatorID,
			current.WorkspaceOwnerID, current.WorkspaceID); err != nil {
			return domain.NewError(domain.CodeNotFound, "supplemental output page not found")
		}
		var memberVersion int64
		var role string
		if err := tx.QueryRow(ctx, `SELECT pm.version,pm.role FROM project_members pm JOIN users u ON u.id=pm.user_id
WHERE pm.project_id=$1 AND pm.user_id=$2 AND u.status='approved'`, current.ProjectID, actorID).Scan(&memberVersion, &role); err != nil ||
			memberVersion != identity.WorkspaceAuthorizationVersion || role != string(project.RoleMember) && role != string(project.RoleOwner) {
			return domain.NewError(domain.CodeNotFound, "supplemental output page not found")
		}
		return nil
	}
	if identity.WorkspaceAuthorizationVersion != 0 || current.WorkspaceProjectResourceVersion != nil {
		return domain.NewError(domain.CodeNotFound, "supplemental output page not found")
	}
	var workspaceOwner, workspaceState string
	var deletedAt pgtype.Timestamptz
	if err := tx.QueryRow(ctx, `SELECT owner_id,state,deleted_at FROM workspaces WHERE id=$1`, current.WorkspaceID).
		Scan(&workspaceOwner, &workspaceState, &deletedAt); err != nil || workspaceOwner != current.WorkspaceOwnerID ||
		workspaceState != "ready" || deletedAt.Valid {
		return domain.NewError(domain.CodeNotFound, "supplemental output page not found")
	}
	return nil
}

// CleanupExpiredTaskStepOutputCaptures permanently deletes expired encrypted
// pages. Task and step deletion also cascades through both capture tables.
func (s *Store) CleanupExpiredTaskStepOutputCaptures(ctx context.Context, limit int32) (int32, error) {
	if s == nil || s.pool == nil || ctx == nil || limit < 1 || limit > 1000 {
		return 0, domain.InvalidArgument("limit", "supplemental output capture cleanup limit is invalid")
	}
	var removed int32
	err := withTx(ctx, s.pool.Begin, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM task_step_output_captures WHERE (task_id,step_id) IN (
 SELECT task_id,step_id FROM task_step_output_captures WHERE expires_at<=clock_timestamp()
 ORDER BY expires_at,task_id,step_id LIMIT $1 FOR UPDATE SKIP LOCKED)`, limit)
		if err != nil {
			return fmt.Errorf("clean expired supplemental output captures: %w", err)
		}
		removed = int32(tag.RowsAffected())
		remaining := limit - removed
		if remaining < 1 {
			return nil
		}
		tag, err = tx.Exec(ctx, `DELETE FROM task_step_output_capture_statuses WHERE (task_id,step_id) IN (
 SELECT task_id,step_id FROM task_step_output_capture_statuses WHERE expires_at<=clock_timestamp()
 ORDER BY expires_at,task_id,step_id LIMIT $1 FOR UPDATE SKIP LOCKED)`, remaining)
		if err != nil {
			return fmt.Errorf("clean expired supplemental output capture statuses: %w", err)
		}
		removed += int32(tag.RowsAffected())
		return nil
	})
	return removed, err
}
