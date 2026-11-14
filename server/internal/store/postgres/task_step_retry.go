package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"
)

const taskStepRetryIntentLifetime = time.Hour

func (s *Store) RequestTaskStepRetry(ctx context.Context, record app.IdempotencyRecord,
	request app.TaskStepRetryRequest) (app.TaskStepRetryRecord, error) {
	if err := validateIdempotency(record); err != nil {
		return app.TaskStepRetryRecord{}, err
	}
	if !domain.ValidTaskID(request.TaskID) || !domain.ValidStepID(request.StepID) ||
		!domain.ValidStepID(request.AttemptStepID) || record.RelatedID != request.StepID ||
		request.ExpectedTaskRevision < 0 || request.ExpectedAttempt < 1 || len(request.ExpectedRequestDigest) != 32 ||
		request.RetryCallID != "morde_step_retry_"+request.AttemptStepID {
		return app.TaskStepRetryRecord{}, domain.InvalidArgument("retry", "task step retry identity is invalid")
	}
	var result app.TaskStepRetryRecord
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		stored, replayed, err := findIdempotency(ctx, tx, record)
		if err != nil {
			return err
		}
		if replayed {
			intent, err := readTaskStepRetryIntent(ctx, tx, stored.ResourceID)
			if err != nil {
				return err
			}
			if intent.TaskID != request.TaskID || intent.SourceStepID != request.StepID ||
				intent.TaskRevision != request.ExpectedTaskRevision || intent.Attempt != request.ExpectedAttempt+1 ||
				!bytes.Equal(intent.RequestDigest, request.ExpectedRequestDigest) {
				return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was used for a different step retry")
			}
			if !intent.Consumed && !intent.ExpiresAt.After(time.Now()) {
				return domain.NewError(domain.CodeConflict, "step retry authorization expired")
			}
			result = app.TaskStepRetryRecord{Intent: intent, Replayed: true}
			return nil
		}

		projectFence, err := lockProjectTaskWorkspaceFence(ctx, tx, request.TaskID, false)
		if err != nil {
			return err
		}
		actorTaskOwnerID, err := taskOwnerIDForActor(ctx, tx, request.TaskID, record.OwnerID, true, false)
		if err != nil {
			return err
		}
		queries := sqlcgen.New(tx)
		initial, err := queries.GetTask(ctx, sqlcgen.GetTaskParams{ID: request.TaskID, OwnerID: actorTaskOwnerID})
		if err != nil {
			return storeError("read task for safe step retry", "task", err)
		}
		if !initial.HostID.Valid || initial.ExecutionLocation != "server" {
			return domain.NewError(domain.CodeConflict, "task target does not support safe step retry")
		}
		if err := lockTaskTargetHost(ctx, tx, initial.HostID.String, initial.InitiatorID); err != nil {
			return err
		}
		if err := lockAIHost(ctx, tx, initial.HostID.String, initial.InitiatorID); err != nil {
			return err
		}
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, request.TaskID, initial.OwnerID).Scan(&locked); err != nil {
			return storeError("lock task for safe step retry", "task", err)
		}
		current, err := queries.GetTask(ctx, sqlcgen.GetTaskParams{ID: request.TaskID, OwnerID: initial.OwnerID})
		if err != nil {
			return storeError("read locked task for safe step retry", "task", err)
		}
		if err := verifyProjectTaskWorkspaceFence(ctx, tx, request.TaskID, projectFence); err != nil {
			return err
		}
		if current.Status != string(task.StatusCompleted) || !current.FinishedAt.Valid || current.CurrentRevision != request.ExpectedTaskRevision ||
			current.DesiredRevision != request.ExpectedTaskRevision || current.CheckpointThroughSeq < 0 || len(current.CheckpointDigest) != 32 ||
			current.AgentSessionID == "" || current.AgentGeneration < 1 || current.AuthVersion < 1 || current.ExecutionEpoch < 1 {
			return domain.NewError(domain.CodeConflict, "task checkpoint or revision is no longer retryable")
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, current.InitiatorID, &current.ExecutionEpoch)
		if err != nil || gate.state != "enabled" || gate.epoch != current.ExecutionEpoch {
			if err != nil {
				return err
			}
			return domain.NewError(domain.CodeConflict, "task execution authorization changed")
		}
		if current.WorkspaceID.Valid {
			if err := lockTaskWorkspaceForExecution(ctx, tx, current.WorkspaceID.String, current.InitiatorID); err != nil {
				return err
			}
			if err := lockTaskWorkspaceInput(ctx, tx, taskDomain(current)); err != nil {
				return err
			}
		}
		authorized, err := requireCurrentTaskAuthorization(ctx, tx, current)
		if err != nil {
			return err
		}
		if !authorized {
			return domain.NewError(domain.CodeForbidden, "task authorization changed")
		}
		if err := verifyStepRetryDependencies(ctx, tx, current.ID, current.OwnerID, current.InitiatorID, current.ProjectID); err != nil {
			return err
		}

		var source task.Step
		var sourceRevision pgtype.Int8
		var sourceDigest []byte
		var sourceRetryable bool
		if err := tx.QueryRow(ctx, `SELECT id,task_id,sequence,attempt_no,COALESCE(retry_of_step_id,''),kind,status,title,summary,
COALESCE(failure_reason,''),retryable,COALESCE(agent_session_id,''),task_revision,COALESCE(tool_call_key,''),COALESCE(tool_name,''),
request_digest,COALESCE(phase,''),detail_state,created_at
FROM task_steps WHERE task_id=$1 AND id=$2 FOR UPDATE`, request.TaskID, request.StepID).Scan(
			&source.ID, &source.TaskID, &source.Sequence, &source.Attempt, &source.RetryOfStepID, &source.Kind, &source.Status,
			&source.Title, &source.Summary, &source.FailureReason, &sourceRetryable, &source.AgentSessionID, &sourceRevision,
			&source.ToolCallKey, &source.ToolName, &sourceDigest, &source.Phase, &source.DetailState, &source.CreatedAt); err != nil {
			return storeError("read source task step for retry", "task step", err)
		}
		if sourceRevision.Valid {
			value := sourceRevision.Int64
			source.TaskRevision = &value
		}
		source.RequestDigest = sourceDigest
		source.Retryable = sourceRetryable
		if source.Status != task.StepFailed || !source.Retryable || source.FailureReason != "not_dispatched" || source.Phase != "prepare" ||
			source.Kind != "tool" || source.Attempt != request.ExpectedAttempt || source.TaskRevision == nil ||
			*source.TaskRevision != request.ExpectedTaskRevision || source.AgentSessionID != current.AgentSessionID ||
			!bytes.Equal(source.RequestDigest, request.ExpectedRequestDigest) || source.ToolCallKey == "" || source.ToolName == "" {
			return domain.NewError(domain.CodeConflict, "step failure is not safe to retry")
		}
		if !app.SupportsSafeTaskStepRetryTool(source.ToolName) {
			return domain.NewError(domain.CodeConflict, "step tool does not support safe retry")
		}
		sourceOperationKey := app.AgentOperationKey(source.ToolCallKey)
		var sourceOperation, unsettledOperation, unknownStep bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_operations WHERE task_id=$1 AND operation_key=$2),
EXISTS(SELECT 1 FROM task_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown')),
EXISTS(SELECT 1 FROM task_steps WHERE task_id=$1 AND status='unknown')`, request.TaskID, sourceOperationKey).
			Scan(&sourceOperation, &unsettledOperation, &unknownStep); err != nil {
			return fmt.Errorf("verify prior tool side effects before retry: %w", err)
		}
		if sourceOperation || unsettledOperation || unknownStep {
			return domain.NewError(domain.CodeConflict, "task has an operation whose side effects are not confirmed")
		}
		if err := verifyNoOtherUnledgeredFailures(ctx, tx, request.TaskID, source.ID); err != nil {
			return err
		}
		var hasNewerAttempt bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_steps WHERE task_id=$1 AND retry_of_step_id=$2)`,
			request.TaskID, source.ID).Scan(&hasNewerAttempt); err != nil {
			return fmt.Errorf("verify task step retry chain: %w", err)
		}
		if hasNewerAttempt {
			return domain.NewError(domain.CodeConflict, "a newer attempt already exists for this step")
		}

		var runtimeGeneration int64
		if err := tx.QueryRow(ctx, `SELECT h.runtime_generation FROM hosts h WHERE h.id=$1 AND h.state='running' AND h.control_state='ai'
AND h.deleted_at IS NULL AND EXISTS (SELECT 1 FROM host_runtime_heartbeats hb WHERE hb.host_id=h.id
  AND hb.runtime_generation=h.runtime_generation AND hb.last_seen_at > clock_timestamp()-$2::double precision*interval '1 second')`,
			current.HostID.String, host.RuntimeHeartbeatFreshness.Seconds()).Scan(&runtimeGeneration); err != nil || runtimeGeneration < 1 {
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read Host retry fence: %w", err)
			}
			return domain.NewError(domain.CodeConflict, "Host is not ready for a safe step retry")
		}
		workspaceBindingVersion := int64(0)
		var workspaceRevision *int64
		if current.WorkspaceID.Valid {
			if !current.WorkspaceCheckpointRevision.Valid || !current.InputRevision.Valid {
				return domain.NewError(domain.CodeConflict, "task Workspace input fence is incomplete")
			}
			var bindingState string
			var appliedRevision, revision int64
			if err := tx.QueryRow(ctx, `SELECT wb.binding_version,wb.state,wb.applied_revision,w.revision
FROM workspace_bindings wb JOIN workspaces w ON w.id=wb.workspace_id
WHERE wb.host_id=$1 AND wb.workspace_id=$2`, current.HostID.String, current.WorkspaceID.String).
				Scan(&workspaceBindingVersion, &bindingState, &appliedRevision, &revision); err != nil {
				return fmt.Errorf("read Workspace retry fence: %w", err)
			}
			if bindingState != "active" || revision != current.WorkspaceCheckpointRevision.Int64 || appliedRevision != revision {
				return domain.NewError(domain.CodeWorkspaceConflict, "task Workspace target changed before retry")
			}
			value := revision
			workspaceRevision = &value
		}

		sequence := int64(0)
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_steps WHERE task_id=$1`, request.TaskID).Scan(&sequence); err != nil {
			return fmt.Errorf("allocate task step retry sequence: %w", err)
		}
		if sequence < 1 || source.Attempt == int64(^uint64(0)>>1) {
			return domain.NewError(domain.CodeInvalidState, "task step retry count is exhausted")
		}
		intent := task.StepRetryIntent{TaskID: current.ID, OwnerID: current.OwnerID, SourceStepID: source.ID,
			AttemptStepID: request.AttemptStepID, Attempt: source.Attempt + 1, SourceCallID: source.ToolCallKey,
			RetryCallID: request.RetryCallID, AgentSessionID: current.AgentSessionID, TaskRevision: request.ExpectedTaskRevision,
			ToolName: source.ToolName, RequestDigest: append([]byte(nil), source.RequestDigest...),
			AuthorizationVersion: current.AuthVersion, HostID: current.HostID.String, HostRuntimeGeneration: runtimeGeneration,
			WorkspaceID: current.WorkspaceID.String, InputRevision: int64Pointer(current.InputRevision),
			WorkspaceCheckpointRevision: int64Pointer(current.WorkspaceCheckpointRevision), WorkspaceBindingVersion: workspaceBindingVersion,
			WorkspaceRevision: workspaceRevision, CheckpointThroughSequence: current.CheckpointThroughSeq,
			CheckpointDigest: append([]byte(nil), current.CheckpointDigest...),
			ExpiresAt:        time.Now().UTC().Truncate(time.Microsecond).Add(taskStepRetryIntentLifetime)}
		stored, replayed, err = reserveIdempotency(ctx, tx, record, request.AttemptStepID, request.StepID)
		if err != nil {
			return err
		}
		if replayed {
			intent, err := readTaskStepRetryIntent(ctx, tx, stored.ResourceID)
			if err != nil {
				return err
			}
			result = app.TaskStepRetryRecord{Intent: intent, Replayed: true}
			return nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_steps
(id,task_id,sequence,kind,status,title,summary,agent_session_id,task_revision,tool_call_key,tool_name,request_digest,phase,detail_state,
attempt_no,retry_of_step_id)
VALUES ($1,$2,$3,'tool','queued',$4,$5,$6,$7,$8,$9,$10,'retry','not_recorded',$11,$12)`,
			intent.AttemptStepID, intent.TaskID, sequence, "Retry tool: "+source.ToolName, "A verified step retry is queued.",
			intent.AgentSessionID, intent.TaskRevision, intent.RetryCallID, intent.ToolName, intent.RequestDigest, intent.Attempt, intent.SourceStepID); err != nil {
			return fmt.Errorf("create task step retry attempt: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_step_retry_intents
(attempt_step_id,task_id,owner_id,source_step_id,attempt_no,task_revision,agent_session_id,source_call_id,retry_call_id,
tool_name,request_digest,authorization_version,host_id,host_runtime_generation,workspace_id,input_revision,
workspace_checkpoint_revision,workspace_binding_version,workspace_revision,checkpoint_through_seq,checkpoint_digest,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,NULLIF($15,''),$16,$17,$18,$19,$20,$21,$22)`,
			intent.AttemptStepID, intent.TaskID, intent.OwnerID, intent.SourceStepID, intent.Attempt, intent.TaskRevision,
			intent.AgentSessionID, intent.SourceCallID, intent.RetryCallID, intent.ToolName, intent.RequestDigest,
			intent.AuthorizationVersion, intent.HostID, intent.HostRuntimeGeneration, intent.WorkspaceID,
			intent.InputRevision, intent.WorkspaceCheckpointRevision, intent.WorkspaceBindingVersion, intent.WorkspaceRevision,
			intent.CheckpointThroughSequence, intent.CheckpointDigest, intent.ExpiresAt); err != nil {
			return fmt.Errorf("persist task step retry intent: %w", err)
		}
		if err := updateTaskForStepRetry(ctx, tx, intent.TaskID, intent.AttemptStepID); err != nil {
			return err
		}
		result = app.TaskStepRetryRecord{Intent: intent}
		return nil
	})
	return result, err
}

func (s *Store) ConsumeTaskStepRetryIntent(ctx context.Context, intent task.StepRetryIntent, fence app.AgentInstanceFence) error {
	if !validStepRetryIntent(intent) || fence.TaskID != intent.TaskID || fence.OwnerID != intent.OwnerID ||
		fence.AgentSessionID != intent.AgentSessionID || fence.AgentInstanceID == "" || fence.AgentGeneration < 1 ||
		fence.AuthorizationVersion != intent.AuthorizationVersion || fence.LeaseOwner == "" || fence.LeaseVersion < 1 {
		return domain.InvalidArgument("step_retry", "step retry intent is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		projectFence, err := lockProjectTaskWorkspaceFence(ctx, tx, intent.TaskID, false)
		if err != nil {
			return err
		}
		var ownerID, initiatorID, hostID string
		if err := tx.QueryRow(ctx, `SELECT owner_id,initiator_id,COALESCE(host_id,'') FROM tasks WHERE id=$1`, intent.TaskID).
			Scan(&ownerID, &initiatorID, &hostID); err != nil {
			return storeError("read task for retry intent consumption", "task", err)
		}
		if ownerID != intent.OwnerID || hostID != intent.HostID || initiatorID != fence.InitiatorID || !domain.ValidHostID(hostID) {
			return domain.NewError(domain.CodeConflict, "task retry target changed")
		}
		if err := lockTaskTargetHost(ctx, tx, hostID, initiatorID); err != nil {
			return err
		}
		if err := lockAIHost(ctx, tx, hostID, initiatorID); err != nil {
			return err
		}
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, intent.TaskID, ownerID).Scan(&locked); err != nil {
			return storeError("lock task for retry intent consumption", "task", err)
		}
		row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: intent.TaskID, OwnerID: ownerID})
		if err != nil {
			return storeError("read locked task for retry intent consumption", "task", err)
		}
		if err := verifyProjectTaskWorkspaceFence(ctx, tx, intent.TaskID, projectFence); err != nil {
			return err
		}
		if row.Status != string(task.StatusRunning) || row.LeaseOwner != fence.LeaseOwner || row.LeaseVersion != fence.LeaseVersion ||
			row.LeaseExpiresAt.Time.Before(time.Now()) || row.CurrentRevision != intent.TaskRevision || row.DesiredRevision != intent.TaskRevision ||
			row.AgentSessionID != intent.AgentSessionID || row.AuthVersion != intent.AuthorizationVersion || row.HostID.String != intent.HostID ||
			row.AgentInstanceID != fence.AgentInstanceID || row.AgentGeneration != fence.AgentGeneration ||
			row.AgentEngineVersion != fence.EngineVersion || row.AgentProfileVersion != fence.ProfileVersion ||
			row.CheckpointThroughSeq != intent.CheckpointThroughSequence || !bytes.Equal(row.CheckpointDigest, intent.CheckpointDigest) {
			return domain.NewError(domain.CodeConflict, "task retry execution fence changed")
		}
		var liveAgentInstance bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_agent_instances ai WHERE ai.task_id=$1 AND ai.owner_id=$2
AND ai.agent_instance_id=$3 AND ai.agent_generation=$4 AND ai.agent_session_id=$5 AND ai.lease_owner=$6
AND ai.lease_version=$7 AND ai.auth_version=$8 AND ai.state='active' AND ai.lease_expires_at>clock_timestamp())`,
			fence.TaskID, fence.OwnerID, fence.AgentInstanceID, fence.AgentGeneration, fence.AgentSessionID,
			fence.LeaseOwner, fence.LeaseVersion, fence.AuthorizationVersion).Scan(&liveAgentInstance); err != nil {
			return fmt.Errorf("verify live Agent instance before retry: %w", err)
		}
		if !liveAgentInstance {
			return domain.NewError(domain.CodeConflict, "task retry Agent instance is no longer active")
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, row.InitiatorID, &row.ExecutionEpoch)
		if err != nil || gate.state != "enabled" || gate.epoch != row.ExecutionEpoch {
			if err != nil {
				return err
			}
			return domain.NewError(domain.CodeConflict, "task execution authorization changed")
		}
		if row.WorkspaceID.Valid {
			if !sameNullableInt64(row.InputRevision, intent.InputRevision) || !sameNullableInt64(row.WorkspaceCheckpointRevision, intent.WorkspaceCheckpointRevision) {
				return domain.NewError(domain.CodeWorkspaceConflict, "task Workspace input fence changed")
			}
			if err := lockTaskWorkspaceForExecution(ctx, tx, row.WorkspaceID.String, row.InitiatorID); err != nil {
				return err
			}
			if err := lockTaskWorkspaceInput(ctx, tx, taskDomain(row)); err != nil {
				return err
			}
			var bindingVersion, appliedRevision, workspaceRevision int64
			var bindingState string
			if err := tx.QueryRow(ctx, `SELECT wb.binding_version,wb.applied_revision,w.revision,wb.state
FROM workspace_bindings wb JOIN workspaces w ON w.id=wb.workspace_id WHERE wb.host_id=$1 AND wb.workspace_id=$2`,
				intent.HostID, row.WorkspaceID.String).Scan(&bindingVersion, &appliedRevision, &workspaceRevision, &bindingState); err != nil {
				return fmt.Errorf("read current retry Workspace fence: %w", err)
			}
			if intent.WorkspaceBindingVersion != bindingVersion || intent.WorkspaceRevision == nil ||
				*intent.WorkspaceRevision != workspaceRevision || appliedRevision != workspaceRevision || bindingState != "active" {
				return domain.NewError(domain.CodeWorkspaceConflict, "task Workspace target changed before retry dispatch")
			}
		} else if row.WorkspaceID.Valid || intent.WorkspaceID != "" || intent.WorkspaceBindingVersion != 0 {
			return domain.NewError(domain.CodeWorkspaceConflict, "task Workspace target changed before retry dispatch")
		}
		authorized, err := requireCurrentTaskAuthorization(ctx, tx, row)
		if err != nil {
			return err
		}
		if !authorized {
			return domain.NewError(domain.CodeForbidden, "task authorization changed")
		}
		if err := verifyStepRetryDependencies(ctx, tx, row.ID, row.OwnerID, row.InitiatorID, row.ProjectID); err != nil {
			return err
		}
		var runtimeGeneration int64
		if err := tx.QueryRow(ctx, `SELECT runtime_generation FROM hosts WHERE id=$1 AND state='running' AND control_state='ai'
AND deleted_at IS NULL AND EXISTS (SELECT 1 FROM host_runtime_heartbeats hb WHERE hb.host_id=hosts.id
AND hb.runtime_generation=hosts.runtime_generation AND hb.last_seen_at>clock_timestamp()-$2::double precision*interval '1 second')`,
			intent.HostID, host.RuntimeHeartbeatFreshness.Seconds()).Scan(&runtimeGeneration); err != nil {
			return fmt.Errorf("recheck Host retry fence: %w", err)
		}
		if runtimeGeneration != intent.HostRuntimeGeneration {
			return domain.NewError(domain.CodeConflict, "Host runtime changed after retry authorization")
		}
		var sourceStatus, sourcePhase, sourceFailureReason, sourceToolName string
		var sourceRetryable bool
		var sourceDigest []byte
		var sourceCallID string
		var sourceRevision pgtype.Int8
		if err := tx.QueryRow(ctx, `SELECT status,COALESCE(phase,''),COALESCE(failure_reason,''),retryable,
COALESCE(tool_call_key,''),COALESCE(tool_name,''),request_digest,task_revision
FROM task_steps WHERE task_id=$1 AND id=$2 FOR UPDATE`, intent.TaskID, intent.SourceStepID).Scan(
			&sourceStatus, &sourcePhase, &sourceFailureReason, &sourceRetryable, &sourceCallID, &sourceToolName, &sourceDigest, &sourceRevision); err != nil {
			return domain.NewError(domain.CodeConflict, "step retry source is unavailable")
		}
		if sourceStatus != string(task.StepFailed) || sourcePhase != "prepare" || sourceFailureReason != "not_dispatched" || !sourceRetryable ||
			sourceCallID != intent.SourceCallID || sourceToolName != intent.ToolName || !sourceRevision.Valid || sourceRevision.Int64 != intent.TaskRevision ||
			!bytes.Equal(sourceDigest, intent.RequestDigest) || !app.SupportsSafeTaskStepRetryTool(sourceToolName) {
			return domain.NewError(domain.CodeConflict, "step retry source evidence changed")
		}
		var sourceOperation, unsettledOperation, unknownStep bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_operations WHERE task_id=$1 AND operation_key=$2),
EXISTS(SELECT 1 FROM task_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown')),
EXISTS(SELECT 1 FROM task_steps WHERE task_id=$1 AND status='unknown')`, row.ID, app.AgentOperationKey(intent.SourceCallID)).
			Scan(&sourceOperation, &unsettledOperation, &unknownStep); err != nil {
			return fmt.Errorf("recheck prior tool side effects before retry dispatch: %w", err)
		}
		if sourceOperation || unsettledOperation || unknownStep {
			return domain.NewError(domain.CodeConflict, "task has an operation whose side effects are not confirmed")
		}
		var state string
		var expiresAt time.Time
		err = tx.QueryRow(ctx, `SELECT state,expires_at FROM task_step_retry_intents WHERE attempt_step_id=$1 AND task_id=$2
AND source_step_id=$3 AND retry_call_id=$4 AND task_revision=$5 AND agent_session_id=$6 AND request_digest=$7 FOR UPDATE`,
			intent.AttemptStepID, intent.TaskID, intent.SourceStepID, intent.RetryCallID, intent.TaskRevision,
			intent.AgentSessionID, intent.RequestDigest).Scan(&state, &expiresAt)
		if err != nil {
			return domain.NewError(domain.CodeConflict, "step retry intent is unavailable")
		}
		if state != "pending" || !expiresAt.After(time.Now()) {
			return domain.NewError(domain.CodeConflict, "step retry intent is expired or already consumed")
		}
		storedIntent, err := readTaskStepRetryIntent(ctx, tx, intent.AttemptStepID)
		if err != nil {
			return err
		}
		if !sameStepRetryIntent(storedIntent, intent) {
			return domain.NewError(domain.CodeConflict, "step retry intent binding changed")
		}
		result, err := tx.Exec(ctx, `UPDATE task_step_retry_intents SET state='consumed',consumed_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE attempt_step_id=$1 AND state='pending'`, intent.AttemptStepID)
		if err != nil {
			return fmt.Errorf("consume task step retry intent: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "step retry intent was already consumed")
		}
		return nil
	})
}

func sameStepRetryIntent(left, right task.StepRetryIntent) bool {
	return left.TaskID == right.TaskID && left.OwnerID == right.OwnerID && left.SourceStepID == right.SourceStepID &&
		left.AttemptStepID == right.AttemptStepID && left.Attempt == right.Attempt && left.SourceCallID == right.SourceCallID &&
		left.RetryCallID == right.RetryCallID && left.AgentSessionID == right.AgentSessionID && left.TaskRevision == right.TaskRevision &&
		left.ToolName == right.ToolName && bytes.Equal(left.RequestDigest, right.RequestDigest) &&
		left.AuthorizationVersion == right.AuthorizationVersion && left.HostID == right.HostID &&
		left.HostRuntimeGeneration == right.HostRuntimeGeneration && left.WorkspaceID == right.WorkspaceID &&
		sameInt64Pointer(left.InputRevision, right.InputRevision) &&
		sameInt64Pointer(left.WorkspaceCheckpointRevision, right.WorkspaceCheckpointRevision) &&
		left.WorkspaceBindingVersion == right.WorkspaceBindingVersion && sameInt64Pointer(left.WorkspaceRevision, right.WorkspaceRevision) &&
		left.CheckpointThroughSequence == right.CheckpointThroughSequence && bytes.Equal(left.CheckpointDigest, right.CheckpointDigest) &&
		left.Consumed == right.Consumed && left.ExpiresAt.Equal(right.ExpiresAt)
}

func sameInt64Pointer(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func loadClaimedStepRetryIntent(ctx context.Context, tx pgx.Tx, taskID string, revision *int64) (*task.StepRetryIntent, error) {
	if revision == nil {
		return nil, nil
	}
	var attemptStepID string
	err := tx.QueryRow(ctx, `SELECT attempt_step_id FROM task_step_retry_intents
WHERE task_id=$1 AND task_revision=$2 ORDER BY created_at DESC,attempt_step_id DESC LIMIT 1`, taskID, *revision).Scan(&attemptStepID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load task step retry intent for claim: %w", err)
	}
	intent, err := readTaskStepRetryIntent(ctx, tx, attemptStepID)
	if err != nil {
		return nil, err
	}
	return &intent, nil
}

func updateTaskForStepRetry(ctx context.Context, tx pgx.Tx, taskID, stepID string) error {
	var sequence int64
	err := tx.QueryRow(ctx, `UPDATE tasks SET status='queued',current_step=$2,waiting_reason='',lease_owner='',
lease_expires_at=NULL,finished_at=NULL,updated_at=clock_timestamp(),event_seq=event_seq+1 WHERE id=$1 AND status='completed'
RETURNING event_seq`, taskID, stepID).Scan(&sequence)
	if err != nil {
		return fmt.Errorf("queue task for step retry: %w", err)
	}
	statusPayload, err := json.Marshal(map[string]any{"status": string(task.StatusQueued), "reason": "step_retry", "step_id": stepID})
	if err != nil {
		return fmt.Errorf("encode task step retry status: %w", err)
	}
	if err := insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: sequence, Type: "task.status", Payload: statusPayload}); err != nil {
		return err
	}
	var stepSequence, attempt int64
	var retryOf string
	if err := tx.QueryRow(ctx, `SELECT sequence,attempt_no,COALESCE(retry_of_step_id,'') FROM task_steps WHERE task_id=$1 AND id=$2`, taskID, stepID).
		Scan(&stepSequence, &attempt, &retryOf); err != nil {
		return fmt.Errorf("read queued retry step: %w", err)
	}
	stepPayload, err := json.Marshal(map[string]any{"step_id": stepID, "sequence": stepSequence, "attempt": attempt,
		"retry_of_step_id": retryOf, "status": string(task.StepQueued), "summary": "A verified step retry is queued."})
	if err != nil {
		return fmt.Errorf("encode task step retry event: %w", err)
	}
	sequence, err = incrementTaskEventSequence(ctx, tx, taskID)
	if err != nil {
		return err
	}
	return insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: sequence, Type: "task.step", Payload: stepPayload})
}

func readTaskStepRetryIntent(ctx context.Context, tx pgx.Tx, attemptStepID string) (task.StepRetryIntent, error) {
	var intent task.StepRetryIntent
	var workspaceID pgtype.Text
	var workspaceRevision pgtype.Int8
	var inputRevision, workspaceCheckpointRevision pgtype.Int8
	var checkpointDigest, requestDigest []byte
	var state string
	err := tx.QueryRow(ctx, `SELECT task_id,owner_id,source_step_id,attempt_step_id,attempt_no,source_call_id,retry_call_id,
agent_session_id,task_revision,tool_name,request_digest,authorization_version,host_id,host_runtime_generation,
workspace_id,input_revision,workspace_checkpoint_revision,workspace_binding_version,workspace_revision,
checkpoint_through_seq,checkpoint_digest,state,expires_at
FROM task_step_retry_intents WHERE attempt_step_id=$1`, attemptStepID).Scan(
		&intent.TaskID, &intent.OwnerID, &intent.SourceStepID, &intent.AttemptStepID, &intent.Attempt,
		&intent.SourceCallID, &intent.RetryCallID, &intent.AgentSessionID, &intent.TaskRevision, &intent.ToolName,
		&requestDigest, &intent.AuthorizationVersion, &intent.HostID, &intent.HostRuntimeGeneration, &workspaceID,
		&inputRevision, &workspaceCheckpointRevision, &intent.WorkspaceBindingVersion, &workspaceRevision,
		&intent.CheckpointThroughSequence, &checkpointDigest, &state, &intent.ExpiresAt)
	if err != nil {
		return task.StepRetryIntent{}, storeError("read task step retry intent", "task step retry", err)
	}
	intent.RequestDigest = append([]byte(nil), requestDigest...)
	intent.CheckpointDigest = append([]byte(nil), checkpointDigest...)
	if workspaceID.Valid {
		intent.WorkspaceID = workspaceID.String
	}
	intent.InputRevision = int64Pointer(inputRevision)
	intent.WorkspaceCheckpointRevision = int64Pointer(workspaceCheckpointRevision)
	intent.WorkspaceRevision = int64Pointer(workspaceRevision)
	intent.Consumed = state == "consumed"
	if !validStepRetryIntent(intent) {
		return task.StepRetryIntent{}, domain.NewError(domain.CodeInvalidState, "task step retry intent is invalid")
	}
	return intent, nil
}

func verifyStepRetryDependencies(ctx context.Context, tx pgx.Tx, taskID, ownerID, initiatorID string, projectID pgtype.Text) error {
	var blocked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM task_dependencies dependency JOIN tasks prerequisite
ON prerequisite.id=dependency.dependency_task_id AND prerequisite.owner_id=dependency.owner_id
AND prerequisite.conversation_id=dependency.conversation_id AND prerequisite.initiator_id=$3
AND prerequisite.project_id IS NOT DISTINCT FROM $4
WHERE dependency.task_id=$1 AND dependency.owner_id=$2 AND (prerequisite.status<>'completed'
OR prerequisite.current_revision<>dependency.required_revision OR prerequisite.desired_revision<>dependency.required_revision
OR prerequisite.result=''))`, taskID, ownerID, initiatorID, projectID).Scan(&blocked); err != nil {
		return fmt.Errorf("verify task step retry dependencies: %w", err)
	}
	if blocked {
		return domain.NewError(domain.CodeConflict, "task prerequisites are no longer verified")
	}
	return nil
}

func verifyNoOtherUnledgeredFailures(ctx context.Context, tx pgx.Tx, taskID, sourceStepID string) error {
	rows, err := tx.Query(ctx, `SELECT tool_call_key FROM task_steps WHERE task_id=$1 AND status='failed' AND tool_call_key IS NOT NULL
AND id<>$2`, taskID, sourceStepID)
	if err != nil {
		return fmt.Errorf("list prior failed task tool calls: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var callID string
		if err := rows.Scan(&callID); err != nil {
			return fmt.Errorf("read prior failed task tool call: %w", err)
		}
		var hasOperation bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_operations WHERE task_id=$1 AND operation_key=$2)`,
			taskID, app.AgentOperationKey(callID)).Scan(&hasOperation); err != nil {
			return fmt.Errorf("check prior failed task operation: %w", err)
		}
		if !hasOperation {
			return domain.NewError(domain.CodeConflict, "another failed tool call has no confirmed operation ledger")
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate prior failed task tool calls: %w", err)
	}
	return nil
}

func validStepRetryIntent(intent task.StepRetryIntent) bool {
	return domain.ValidTaskID(intent.TaskID) && domain.ValidUserID(intent.OwnerID) && domain.ValidStepID(intent.SourceStepID) &&
		domain.ValidStepID(intent.AttemptStepID) && intent.Attempt > 1 && intent.SourceCallID != "" && len(intent.SourceCallID) <= 256 &&
		intent.RetryCallID == "morde_step_retry_"+intent.AttemptStepID && intent.AgentSessionID != "" && intent.TaskRevision >= 0 &&
		intent.ToolName != "" && len(intent.RequestDigest) == 32 && intent.AuthorizationVersion > 0 && domain.ValidHostID(intent.HostID) &&
		intent.HostRuntimeGeneration > 0 && intent.CheckpointThroughSequence >= 0 && len(intent.CheckpointDigest) == 32 &&
		!intent.ExpiresAt.IsZero() && ((intent.WorkspaceID == "" && intent.InputRevision == nil && intent.WorkspaceCheckpointRevision == nil &&
		intent.WorkspaceBindingVersion == 0 && intent.WorkspaceRevision == nil) ||
		(domain.ValidWorkspaceID(intent.WorkspaceID) && intent.InputRevision != nil && intent.WorkspaceCheckpointRevision != nil &&
			intent.WorkspaceBindingVersion > 0 && intent.WorkspaceRevision != nil))
}

func sameNullableInt64(left pgtype.Int8, right *int64) bool {
	if !left.Valid || right == nil {
		return !left.Valid && right == nil
	}
	return left.Int64 == *right
}
