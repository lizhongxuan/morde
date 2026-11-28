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

const taskStepOutputCaptureRetention = 24 * time.Hour

func (s *Store) SaveTaskStepOutputCapture(ctx context.Context, value app.TaskStepOutputCaptureRecord) error {
	if s == nil || s.pool == nil || ctx == nil || !app.ValidTaskStepOutputCaptureRecordForStore(value) {
		return domain.InvalidArgument("task_step_output_capture", "supplemental output capture is invalid")
	}
	return withTx(ctx, s.pool.Begin, func(tx pgx.Tx) error {
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
		storageBytes := taskStepOutputCaptureStorageBytes(value)
		if storageBytes < 0 || ownerBytes > int64(app.TaskStepOutputCaptureOwnerQuotaBytes-storageBytes) {
			return domain.NewError(domain.CodeQuotaExceeded, "supplemental output storage quota is full")
		}
		if err := verifyTaskStepOutputCaptureFence(ctx, tx, value.Identity); err != nil {
			return err
		}
		stdout, stderr := value.Stdout, value.Stderr
		_, err := tx.Exec(ctx, `INSERT INTO task_step_output_captures
(task_id,owner_id,step_id,operation_id,call_id,task_revision,authorization_version,workspace_id,
 workspace_authorization_version,workspace_binding_version,workspace_view_id,workspace_checkpoint_revision,
 stdout_source_bytes,stdout_total_bytes,stdout_sha256,stderr_source_bytes,stderr_total_bytes,stderr_sha256,
 stored_bytes,storage_bytes,page_count,state,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,'complete',clock_timestamp()+interval '24 hours')`,
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
	 t.workspace_owner_id,t.workspace_checkpoint_revision,COALESCE(t.project_id,''),t.workspace_project_resource_version,p.version,
 pm.version,pm.role,st.task_revision,st.tool_call_key,st.tool_name,st.phase,st.status,
 op.task_revision,op.call_id,op.tool_name,op.workspace_id,op.binding_version,op.workspace_checkpoint_revision,op.status,
 w.owner_id,w.state,w.deleted_at
FROM tasks t
JOIN task_steps st ON st.task_id=t.id AND st.id=$3
JOIN task_operations op ON op.task_id=t.id AND op.id=$4
LEFT JOIN project_members pm ON pm.project_id=t.project_id AND pm.user_id=t.initiator_id
  AND EXISTS (SELECT 1 FROM users u WHERE u.id=pm.user_id AND u.status='approved')
LEFT JOIN projects p ON p.id=t.project_id
LEFT JOIN workspaces w ON w.id=t.workspace_id
WHERE t.id=$1`, identity.TaskID, identity.OwnerID, identity.StepID, identity.OperationID).Scan(
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
		return nil
	})
	return removed, err
}
