package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"
)

const (
	taskModelRecoveryScheduledReason  = "task_model_recovery_scheduled"
	taskModelRecoveryAvailableReason  = "task_model_recovery_retry_available"
	taskModelRecoveryUnresolvedReason = "dsh_recovery_unresolved"
)

func (s *Store) ScheduleTaskModelRecovery(ctx context.Context, command app.TaskModelRecoveryCommand) (task.ModelRecoveryIntent, bool, error) {
	if ctx == nil {
		return task.ModelRecoveryIntent{}, false, domain.InvalidArgument("task_model_recovery", "recovery context is required")
	}
	if err := app.ValidateTaskModelRecoveryCommand(command); err != nil {
		return task.ModelRecoveryIntent{}, false, err
	}
	evidence := command.Failure.TaskModelRecoveryFailureEvidence()
	fence := modelRecoveryAgentFence(evidence.TaskFence)
	intent, err := taskModelRecoveryIntentForCommand(command)
	if err != nil {
		return task.ModelRecoveryIntent{}, false, err
	}
	var result task.ModelRecoveryIntent
	var accepted bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		projectFence, err := lockProjectTaskWorkspaceFence(ctx, tx, command.Task.ID, false)
		if err != nil {
			return err
		}
		ownerID, err := taskOwnerIDForActor(ctx, tx, command.Task.ID, command.Task.InitiatorID, true, false)
		if err != nil {
			return err
		}
		if ownerID != command.Task.OwnerID {
			return domain.NewError(domain.CodeUnauthenticated, "task owner changed before recovery")
		}
		if command.Task.HostID != "" {
			if _, err := lockAgentHostTarget(ctx, tx, command.Task.ID, ownerID); err != nil {
				return err
			}
		}
		if err := verifyProjectTaskWorkspaceFence(ctx, tx, command.Task.ID, projectFence); err != nil {
			return err
		}
		locked, err := lockAuthorizedAgentTask(ctx, tx, fence)
		if err != nil {
			if replayedIntent, found, replayErr := loadTaskModelRecoveryIntent(ctx, tx, command.Task.ID, ownerID, intent.TaskRevision, true); replayErr != nil {
				return errors.Join(err, replayErr)
			} else if found && matchesTaskModelRecoveryCommand(replayedIntent, command, intent.RecoveryCallID) &&
				validTaskModelRecoveryReplayState(ctx, tx, replayedIntent) {
				result, accepted = replayedIntent, true
				return nil
			}
			return err
		}
		if locked.InitiatorID != command.Task.InitiatorID || locked.ProjectID != command.Task.ProjectID ||
			locked.Generation != command.Task.AgentGeneration || locked.SessionID != command.Task.AgentSessionID ||
			locked.CheckpointThrough != command.CheckpointThroughSequence || !bytes.Equal(locked.CheckpointDigest, command.CheckpointDigest) {
			return domain.NewError(domain.CodeConflict, "task checkpoint changed before recovery")
		}
		current, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: command.Task.ID, OwnerID: ownerID})
		if err != nil {
			return fmt.Errorf("read current Task before model recovery: %w", err)
		}
		if err := verifyTaskModelRecoveryCurrentTask(current, command, evidence.TaskFence); err != nil {
			return err
		}
		if err := verifyTaskModelRecoveryFailureLedger(ctx, tx, evidence); err != nil {
			return err
		}
		previous, err := lockAgentInstance(ctx, tx, command.Task.ID, ownerID, command.Task.AgentGeneration)
		if err != nil {
			return err
		}
		if previous.State != "stopped" || !validAgentStopEvidence(previous) || previous.Fence.AgentInstanceID != fence.AgentInstanceID ||
			previous.Fence.AgentSessionID != fence.AgentSessionID || previous.Fence.LeaseOwner != fence.LeaseOwner ||
			previous.Fence.LeaseVersion != fence.LeaseVersion || previous.Fence.AuthorizationVersion != fence.AuthorizationVersion ||
			previous.Fence.EngineVersion != fence.EngineVersion || previous.Fence.ProfileVersion != fence.ProfileVersion {
			return domain.NewError(domain.CodeConflict, "task Agent stop is unconfirmed")
		}
		if err := verifyTaskModelRecoveryCheckpoint(ctx, tx, command.Task, command.CheckpointThroughSequence, command.CheckpointDigest); err != nil {
			return err
		}
		if existing, found, err := loadTaskModelRecoveryIntent(ctx, tx, command.Task.ID, ownerID, intent.TaskRevision, true); err != nil {
			return err
		} else if found {
			if !matchesTaskModelRecoveryCommand(existing, command, intent.RecoveryCallID) {
				return domain.NewError(domain.CodeConflict, "a different recovery is already recorded for this Task revision")
			}
			return domain.NewError(domain.CodeConflict, "Task recovery has already been scheduled")
		}
		status, reason, eventType := task.StatusQueued, taskModelRecoveryScheduledReason, "task.status"
		if !command.Auto {
			status, reason, eventType = task.StatusWaitingUser, taskModelRecoveryAvailableReason, "task.waiting_user"
			intent.State = task.ModelRecoveryRetryAvailable
		} else {
			intent.State = task.ModelRecoveryScheduled
		}
		sequence, err := transitionTaskToModelRecoveryState(ctx, tx, command.Task.ID, ownerID, fence, status, reason)
		if err != nil {
			return err
		}
		intent.EventSequence = sequence
		if err := insertTaskModelRecoveryIntent(ctx, tx, intent); err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]any{"status": string(status), "reason": reason,
			"revision": intent.TaskRevision, "recovery_state": string(intent.State)})
		if err != nil {
			return fmt.Errorf("encode task model recovery event: %w", err)
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: command.Task.ID, Sequence: sequence, Type: eventType, Payload: payload}); err != nil {
			return err
		}
		intent.CreatedAt = time.Now().UTC()
		intent.UpdatedAt = intent.CreatedAt
		result = intent
		accepted = true
		return nil
	})
	return result, accepted, err
}

func (s *Store) RequestTaskModelRecoveryRetry(ctx context.Context, idempotency app.IdempotencyRecord,
	command app.TaskModelRecoveryRetryCommand) (app.TaskModelRecoveryRetryResult, error) {
	if ctx == nil || !domain.ValidTaskID(command.TaskID) || !domain.ValidUserID(command.OwnerID) ||
		!domain.ValidUserID(command.InitiatorID) || command.TaskRevision < 0 || command.AuthorizationVersion < 1 ||
		command.EventSequence < 1 || idempotency.Operation != "task.model_recovery.retry" ||
		idempotency.OwnerID != command.InitiatorID || idempotency.ResourceID != command.TaskID {
		return app.TaskModelRecoveryRetryResult{}, domain.InvalidArgument("task_model_recovery_retry", "retry identity is invalid")
	}
	if err := validateIdempotency(idempotency); err != nil {
		return app.TaskModelRecoveryRetryResult{}, err
	}
	var result app.TaskModelRecoveryRetryResult
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		projectFence, err := lockProjectTaskWorkspaceFence(ctx, tx, command.TaskID, false)
		if err != nil {
			return err
		}
		ownerID, err := taskOwnerIDForActor(ctx, tx, command.TaskID, command.InitiatorID, true, false)
		if err != nil {
			return err
		}
		if ownerID != command.OwnerID {
			return domain.NewError(domain.CodeNotFound, "task not found")
		}
		initial, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: command.TaskID, OwnerID: ownerID})
		if err != nil {
			return storeError("read Task for model recovery retry", "task", err)
		}
		if initial.HostID.Valid {
			if _, err := lockAgentHostTarget(ctx, tx, command.TaskID, ownerID); err != nil {
				return err
			}
		}
		if err := verifyProjectTaskWorkspaceFence(ctx, tx, command.TaskID, projectFence); err != nil {
			return err
		}
		var lockedTaskID string
		if err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, command.TaskID, ownerID).Scan(&lockedTaskID); err != nil {
			return fmt.Errorf("lock Task for model recovery retry: %w", err)
		}
		current, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: command.TaskID, OwnerID: ownerID})
		if err != nil {
			return storeError("read locked Task for model recovery retry", "task", err)
		}
		authorized, err := requireCurrentTaskAuthorization(ctx, tx, current)
		if err != nil {
			return err
		}
		if !authorized {
			return domain.NewError(domain.CodeForbidden, "task authorization changed")
		}
		if replay, found, err := findIdempotency(ctx, tx, idempotency); err != nil {
			return err
		} else if found {
			if replay.ResourceID != command.TaskID {
				return domain.NewError(domain.CodeIdempotencyConflict, "retry key belongs to another Task")
			}
			intent, exists, err := loadTaskModelRecoveryIntent(ctx, tx, command.TaskID, ownerID, command.TaskRevision, false)
			if err != nil {
				return err
			}
			if !exists || replay.RelatedID != intent.RecoveryCallID ||
				current.InitiatorID != command.InitiatorID || current.CurrentRevision != command.TaskRevision ||
				current.DesiredRevision != command.TaskRevision || current.AuthVersion != command.AuthorizationVersion ||
				intent.AuthorizationVersion != command.AuthorizationVersion {
				return domain.NewError(domain.CodeConflict, "Task recovery is no longer available")
			}
			result = app.TaskModelRecoveryRetryResult{Intent: intent, Replayed: true}
			return nil
		}
		if err := verifyTaskModelRecoveryRetryTask(current, command); err != nil {
			return err
		}
		intent, found, err := loadTaskModelRecoveryIntent(ctx, tx, command.TaskID, ownerID, command.TaskRevision, true)
		if err != nil {
			return err
		}
		if !found || intent.State != task.ModelRecoveryRetryAvailable || intent.Automatic ||
			intent.AuthorizationVersion != command.AuthorizationVersion || intent.InitiatorID != command.InitiatorID ||
			intent.TaskRevision != command.TaskRevision || current.AgentSessionID != intent.AgentSessionID ||
			current.AgentInstanceID != intent.AgentInstanceID || current.AgentGeneration != intent.AgentGeneration ||
			current.CheckpointThroughSeq != intent.CheckpointThroughSequence ||
			!bytes.Equal(current.CheckpointDigest, intent.CheckpointDigest) {
			return domain.NewError(domain.CodeConflict, "Task recovery is no longer available")
		}
		checkpointTask := taskDomain(current)
		checkpointTask.Lease.Version = intent.FailedLeaseVersion
		if err := verifyTaskModelRecoveryCheckpoint(ctx, tx, checkpointTask, intent.CheckpointThroughSequence, intent.CheckpointDigest); err != nil {
			return err
		}
		previous, err := lockAgentInstance(ctx, tx, command.TaskID, ownerID, intent.AgentGeneration)
		if err != nil {
			return err
		}
		if previous.State != "stopped" || !validAgentStopEvidence(previous) || previous.Fence.AgentInstanceID != intent.AgentInstanceID ||
			previous.Fence.AgentSessionID != intent.AgentSessionID || previous.Fence.LeaseOwner != intent.FailedLeaseOwner ||
			previous.Fence.LeaseVersion != intent.FailedLeaseVersion || previous.Fence.AuthorizationVersion != intent.AuthorizationVersion ||
			previous.Fence.EngineVersion != intent.EngineVersion || previous.Fence.ProfileVersion != intent.ProfileVersion {
			return domain.NewError(domain.CodeConflict, "Task Agent stop is unconfirmed")
		}
		if idempotency.RelatedID != "" && idempotency.RelatedID != intent.RecoveryCallID {
			return domain.NewError(domain.CodeIdempotencyConflict, "retry key targets another recovery attempt")
		}
		reserved, replayed, err := reserveIdempotency(ctx, tx, idempotency, command.TaskID, intent.RecoveryCallID)
		if err != nil {
			return err
		}
		if replayed {
			if reserved.ResourceID != command.TaskID || reserved.RelatedID != intent.RecoveryCallID {
				return domain.NewError(domain.CodeIdempotencyConflict, "retry key targets another recovery attempt")
			}
			result = app.TaskModelRecoveryRetryResult{Intent: intent, Replayed: true}
			return nil
		}
		sequence, err := queueTaskForModelRecoveryRetry(ctx, tx, command)
		if err != nil {
			return err
		}
		intent.State, intent.EventSequence, intent.UpdatedAt = task.ModelRecoveryScheduled, sequence, time.Now().UTC()
		updated, err := tx.Exec(ctx, `UPDATE task_model_recovery_schedules SET state='scheduled',event_sequence=$3,updated_at=clock_timestamp()
WHERE task_id=$1 AND task_revision=$2 AND state='retry_available'`, intent.TaskID, intent.TaskRevision, sequence)
		if err != nil {
			return fmt.Errorf("queue Task model recovery retry: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "Task recovery was already retried")
		}
		payload, err := json.Marshal(map[string]any{"status": string(task.StatusQueued), "reason": taskModelRecoveryScheduledReason,
			"revision": intent.TaskRevision, "recovery_state": string(intent.State)})
		if err != nil {
			return fmt.Errorf("encode task model recovery retry event: %w", err)
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: command.TaskID, Sequence: sequence, Type: "task.status", Payload: payload}); err != nil {
			return err
		}
		result = app.TaskModelRecoveryRetryResult{Intent: intent}
		return nil
	})
	return result, err
}

func (s *Store) BeginTaskModelRecoveryAttempt(ctx context.Context, intent task.ModelRecoveryIntent, fence app.AgentInstanceFence) error {
	if ctx == nil || !app.ValidTaskModelRecoveryIntent(intent) {
		return domain.InvalidArgument("task_model_recovery", "recovery intent is invalid")
	}
	if intent.State != task.ModelRecoveryScheduled {
		return domain.NewError(domain.CodeConflict, "Task recovery attempt has already been consumed")
	}
	if fence.Validate() != nil || fence.TaskID != intent.TaskID || fence.OwnerID != intent.OwnerID ||
		fence.InitiatorID != intent.InitiatorID || fence.AgentSessionID != intent.AgentSessionID ||
		fence.AuthorizationVersion != intent.AuthorizationVersion || fence.EngineVersion != intent.EngineVersion ||
		fence.ProfileVersion != intent.ProfileVersion || fence.AgentGeneration <= intent.AgentGeneration ||
		fence.AgentInstanceID == intent.AgentInstanceID {
		return domain.InvalidArgument("task_model_recovery", "attempt fence is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		projectFence, err := lockProjectTaskWorkspaceFence(ctx, tx, intent.TaskID, false)
		if err != nil {
			return err
		}
		ownerID, err := taskOwnerIDForActor(ctx, tx, intent.TaskID, intent.InitiatorID, true, false)
		if err != nil {
			return err
		}
		if ownerID != intent.OwnerID {
			return domain.NewError(domain.CodeUnauthenticated, "task owner changed before recovery attempt")
		}
		if current, err := lockAuthorizedAgentTask(ctx, tx, fence); err != nil {
			return err
		} else if current.InitiatorID != intent.InitiatorID || current.ProjectID != projectFence.projectID ||
			current.InstanceID != fence.AgentInstanceID || current.Generation != fence.AgentGeneration || current.SessionID != intent.AgentSessionID ||
			current.EngineVersion != intent.EngineVersion || current.ProfileVersion != intent.ProfileVersion ||
			current.CheckpointThrough != intent.CheckpointThroughSequence || !bytes.Equal(current.CheckpointDigest, intent.CheckpointDigest) {
			return domain.NewError(domain.CodeConflict, "Task recovery checkpoint or authorization changed")
		}
		currentTask, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: intent.TaskID, OwnerID: ownerID})
		if err != nil {
			return storeError("read current Task model recovery attempt", "task", err)
		}
		if currentTask.CurrentRevision != intent.TaskRevision || currentTask.DesiredRevision != intent.TaskRevision ||
			currentTask.AuthVersion != intent.AuthorizationVersion {
			return domain.NewError(domain.CodeConflict, "Task revision or authorization changed before recovery attempt")
		}
		if err := verifyProjectTaskWorkspaceFence(ctx, tx, intent.TaskID, projectFence); err != nil {
			return err
		}
		currentAgent, err := lockAgentInstance(ctx, tx, intent.TaskID, ownerID, fence.AgentGeneration)
		if err != nil {
			return err
		}
		if currentAgent.State != "active" || currentAgent.Fence.AgentInstanceID != fence.AgentInstanceID ||
			currentAgent.Fence.AgentSessionID != fence.AgentSessionID || currentAgent.Fence.LeaseOwner != fence.LeaseOwner ||
			currentAgent.Fence.LeaseVersion != fence.LeaseVersion || currentAgent.Fence.AuthorizationVersion != fence.AuthorizationVersion ||
			!currentAgent.ExpiresAt.After(time.Now()) {
			return domain.NewError(domain.CodeConflict, "Task recovery Agent lease is no longer current")
		}
		if err := verifyTaskModelRecoveryCheckpoint(ctx, tx, task.Task{ID: intent.TaskID, OwnerID: intent.OwnerID,
			AgentInstanceID: intent.AgentInstanceID, AgentGeneration: intent.AgentGeneration, AgentSessionID: intent.AgentSessionID,
			AgentEngineVersion: intent.EngineVersion, AgentProfileVersion: intent.ProfileVersion,
			CheckpointThroughSequence: intent.CheckpointThroughSequence, CheckpointDigest: append([]byte(nil), intent.CheckpointDigest...),
			Lease:         task.Lease{Version: intent.FailedLeaseVersion},
			Authorization: task.Authorization{Version: intent.AuthorizationVersion}}, intent.CheckpointThroughSequence, intent.CheckpointDigest); err != nil {
			return err
		}
		stored, found, err := loadTaskModelRecoveryIntent(ctx, tx, intent.TaskID, ownerID, intent.TaskRevision, true)
		if err != nil {
			return err
		}
		if !found || !sameTaskModelRecoveryIntent(stored, intent) || stored.State != task.ModelRecoveryScheduled {
			return domain.NewError(domain.CodeConflict, "Task recovery attempt has already been consumed")
		}
		result, err := tx.Exec(ctx, `UPDATE task_model_recovery_schedules SET state='attempting',attempt_started_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE task_id=$1 AND task_revision=$2 AND state='scheduled' AND recovery_call_id=$3 AND checkpoint_digest=$4 AND toolset_digest=$5`,
			intent.TaskID, intent.TaskRevision, intent.RecoveryCallID, intent.CheckpointDigest, intent.ToolsetDigest)
		if err != nil {
			return fmt.Errorf("consume Task model recovery attempt: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "Task recovery attempt has already been consumed")
		}
		return nil
	})
}

func (s *Store) GetTaskModelRecovery(ctx context.Context, taskID, actorID string, revision int64) (task.ModelRecoveryIntent, bool, error) {
	if ctx == nil || !domain.ValidTaskID(taskID) || !domain.ValidUserID(actorID) || revision < 0 {
		return task.ModelRecoveryIntent{}, false, domain.InvalidArgument("task_model_recovery", "recovery identity is invalid")
	}
	var result task.ModelRecoveryIntent
	var found bool
	err := s.withTaskSnapshot(ctx, func(tx pgx.Tx) error {
		ownerID, err := taskOwnerIDForActor(ctx, tx, taskID, actorID, false, false)
		if err != nil {
			return err
		}
		result, found, err = loadTaskModelRecoveryIntent(ctx, tx, taskID, ownerID, revision, false)
		return err
	})
	return result, found, err
}

func loadTaskModelRecoveryIntent(ctx context.Context, queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, taskID, ownerID string, revision int64, lock bool) (task.ModelRecoveryIntent, bool, error) {
	query := `SELECT task_id,owner_id,initiator_id,task_revision,authorization_version,agent_instance_id,agent_generation,
agent_session_id,failed_lease_owner,failed_lease_version,engine_version,profile_version,failed_call_id,failed_model_id,failed_request_digest,failed_through_seq,checkpoint_through_seq,checkpoint_digest,
toolset_digest,recovery_call_id,state,automatic,event_sequence,attempt_started_at,created_at,updated_at
FROM task_model_recovery_schedules WHERE task_id=$1 AND owner_id=$2 AND task_revision=$3`
	if lock {
		query += ` FOR UPDATE`
	}
	var value task.ModelRecoveryIntent
	var state string
	var attemptStarted pgtype.Timestamptz
	err := queryer.QueryRow(ctx, query, taskID, ownerID, revision).Scan(
		&value.TaskID, &value.OwnerID, &value.InitiatorID, &value.TaskRevision, &value.AuthorizationVersion,
		&value.AgentInstanceID, &value.AgentGeneration, &value.AgentSessionID, &value.FailedLeaseOwner, &value.FailedLeaseVersion,
		&value.EngineVersion, &value.ProfileVersion,
		&value.FailedCallID, &value.FailedModelID, &value.FailedRequestDigest,
		&value.FailedThroughSequence, &value.CheckpointThroughSequence, &value.CheckpointDigest,
		&value.ToolsetDigest, &value.RecoveryCallID, &state, &value.Automatic, &value.EventSequence, &attemptStarted,
		&value.CreatedAt, &value.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return task.ModelRecoveryIntent{}, false, nil
	}
	if err != nil {
		return task.ModelRecoveryIntent{}, false, fmt.Errorf("read Task model recovery schedule: %w", err)
	}
	value.State = task.ModelRecoveryState(state)
	if attemptStarted.Valid {
		value.AttemptStartedAt = &attemptStarted.Time
	}
	if !app.ValidTaskModelRecoveryIntent(value) {
		return task.ModelRecoveryIntent{}, false, domain.NewError(domain.CodeInvalidState, "stored Task model recovery schedule is invalid")
	}
	return value, true, nil
}

func insertTaskModelRecoveryIntent(ctx context.Context, tx pgx.Tx, value task.ModelRecoveryIntent) error {
	_, err := tx.Exec(ctx, `INSERT INTO task_model_recovery_schedules
(task_id,owner_id,initiator_id,task_revision,authorization_version,agent_instance_id,agent_generation,agent_session_id,
failed_lease_owner,failed_lease_version,engine_version,profile_version,failed_call_id,failed_model_id,failed_request_digest,failed_through_seq,checkpoint_through_seq,checkpoint_digest,toolset_digest,
recovery_call_id,state,automatic,event_sequence)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
		value.TaskID, value.OwnerID, value.InitiatorID, value.TaskRevision, value.AuthorizationVersion, value.AgentInstanceID,
		value.AgentGeneration, value.AgentSessionID, value.FailedLeaseOwner, value.FailedLeaseVersion, value.EngineVersion, value.ProfileVersion, value.FailedCallID,
		value.FailedModelID, value.FailedRequestDigest, value.FailedThroughSequence, value.CheckpointThroughSequence, value.CheckpointDigest, value.ToolsetDigest,
		value.RecoveryCallID, string(value.State), value.Automatic, value.EventSequence)
	if err != nil {
		return fmt.Errorf("persist Task model recovery schedule: %w", err)
	}
	return nil
}

func taskModelRecoveryIntentForCommand(command app.TaskModelRecoveryCommand) (task.ModelRecoveryIntent, error) {
	evidence := command.Failure.TaskModelRecoveryFailureEvidence()
	fence := evidence.TaskFence
	revision := *command.Task.CurrentRevision
	recoveryCallID, err := app.TaskModelRecoveryCallID(command.Task.ID, revision, evidence.CallID, evidence.TaskFence.ThroughSequence)
	if err != nil {
		return task.ModelRecoveryIntent{}, err
	}
	state := task.ModelRecoveryRetryAvailable
	if command.Auto {
		state = task.ModelRecoveryScheduled
	}
	return task.ModelRecoveryIntent{TaskID: command.Task.ID, OwnerID: command.Task.OwnerID, InitiatorID: command.Task.InitiatorID,
		TaskRevision: revision, AuthorizationVersion: fence.AuthorizationVersion, AgentInstanceID: fence.AgentInstanceID,
		AgentGeneration: fence.AgentGeneration, AgentSessionID: fence.AgentSessionID, FailedLeaseOwner: fence.LeaseOwner,
		FailedLeaseVersion: fence.LeaseVersion, EngineVersion: fence.EngineVersion,
		ProfileVersion: fence.ProfileVersion, FailedCallID: evidence.CallID, FailedModelID: evidence.ModelID,
		FailedRequestDigest: append([]byte(nil), evidence.RequestDigest...), FailedThroughSequence: fence.ThroughSequence,
		CheckpointThroughSequence: command.CheckpointThroughSequence, CheckpointDigest: append([]byte(nil), command.CheckpointDigest...),
		ToolsetDigest: append([]byte(nil), command.ToolsetDigest...), RecoveryCallID: recoveryCallID, State: state, Automatic: command.Auto}, nil
}

func modelRecoveryAgentFence(value app.ModelTaskFence) app.AgentInstanceFence {
	return app.AgentInstanceFence{TaskID: value.TaskID, OwnerID: value.OwnerID, InitiatorID: value.InitiatorID,
		AgentInstanceID: value.AgentInstanceID, AgentGeneration: value.AgentGeneration, AgentSessionID: value.AgentSessionID,
		LeaseOwner: value.LeaseOwner, LeaseVersion: value.LeaseVersion, AuthorizationVersion: value.AuthorizationVersion,
		EngineVersion: value.EngineVersion, ProfileVersion: value.ProfileVersion}
}

func verifyTaskModelRecoveryCurrentTask(current sqlcgen.Task, command app.TaskModelRecoveryCommand, fence app.ModelTaskFence) error {
	if current.Status != string(task.StatusRunning) || current.ExecutionLocation != "server" || current.ID != command.Task.ID ||
		current.OwnerID != command.Task.OwnerID || current.InitiatorID != command.Task.InitiatorID ||
		current.CurrentRevision != *command.Task.CurrentRevision || current.DesiredRevision != *command.Task.DesiredRevision ||
		current.AuthVersion != fence.AuthorizationVersion || current.AgentInstanceID != fence.AgentInstanceID ||
		current.AgentGeneration != fence.AgentGeneration || current.AgentSessionID != fence.AgentSessionID ||
		current.AgentEngineVersion != fence.EngineVersion || current.AgentProfileVersion != fence.ProfileVersion ||
		current.LeaseOwner != fence.LeaseOwner || current.LeaseVersion != fence.LeaseVersion ||
		current.CheckpointThroughSeq != command.CheckpointThroughSequence || !bytes.Equal(current.CheckpointDigest, command.CheckpointDigest) {
		return domain.NewError(domain.CodeConflict, "Task model recovery fence changed")
	}
	return nil
}

func verifyTaskModelRecoveryCheckpoint(ctx context.Context, tx pgx.Tx, value task.Task, through int64, digest []byte) error {
	if through < 0 || len(digest) != 32 || value.AgentSessionID == "" || value.AgentInstanceID == "" ||
		value.AgentGeneration < 1 || value.Authorization.Version < 1 || value.Lease.Version < 1 ||
		value.AgentEngineVersion == "" || value.AgentProfileVersion == "" {
		return domain.NewError(domain.CodeConflict, "Task recovery checkpoint is invalid")
	}
	var matches bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM task_agent_sessions root
 JOIN task_agent_checkpoints checkpoint ON checkpoint.task_id=root.task_id AND checkpoint.owner_id=root.owner_id
   AND checkpoint.agent_session_id=root.session_id AND checkpoint.agent_root_session_id=root.session_id
 JOIN task_agent_instances writer ON writer.task_id=checkpoint.task_id AND writer.owner_id=checkpoint.owner_id
   AND writer.agent_instance_id=checkpoint.agent_instance_id AND writer.agent_generation=checkpoint.agent_generation
   AND writer.agent_session_id=checkpoint.agent_session_id AND writer.engine_version=checkpoint.engine_version
   AND writer.profile_version=checkpoint.profile_version AND writer.auth_version=checkpoint.auth_version
   AND writer.lease_version=checkpoint.lease_version
 WHERE root.task_id=$1 AND root.owner_id=$2 AND root.session_id=$3 AND root.parent_session_id IS NULL
   AND root.checkpoint_through_seq=$4 AND root.checkpoint_digest=$5
   AND root.root_checkpoint_through_seq=$4 AND root.root_checkpoint_baseline_known
   AND checkpoint.agent_generation<=$6 AND checkpoint.lease_version<=$7
   AND checkpoint.through_seq=$4 AND checkpoint.digest=$5 AND checkpoint.engine_version=$8
   AND checkpoint.profile_version=$9 AND checkpoint.auth_version=$10
   AND writer.state='stopped' AND ((writer.stop_receipt IS NOT NULL AND writer.stop_receipt_digest=sha256(writer.stop_receipt))
     OR (writer.guardian_stop_attestation IS NOT NULL AND writer.guardian_stop_attestation_digest=sha256(writer.guardian_stop_attestation)))
)`, value.ID, value.OwnerID, value.AgentSessionID, through, digest, value.AgentGeneration, value.Lease.Version,
		value.AgentEngineVersion, value.AgentProfileVersion, value.Authorization.Version).Scan(&matches); err != nil {
		return fmt.Errorf("verify Task model recovery checkpoint: %w", err)
	}
	if !matches || value.CheckpointThroughSequence != through || !bytes.Equal(value.CheckpointDigest, digest) {
		return domain.NewError(domain.CodeConflict, "Task recovery checkpoint is no longer current")
	}
	return nil
}

func transitionTaskToModelRecoveryState(ctx context.Context, tx pgx.Tx, taskID, ownerID string, fence app.AgentInstanceFence,
	status task.Status, reason string) (int64, error) {
	if status != task.StatusQueued && status != task.StatusWaitingUser {
		return 0, domain.InvalidArgument("task_model_recovery", "recovery Task status is invalid")
	}
	var sequence int64
	err := tx.QueryRow(ctx, `UPDATE tasks SET status=$6,waiting_reason=$7,lease_owner='',lease_version=lease_version+1,
lease_expires_at=NULL,finished_at=NULL,updated_at=clock_timestamp(),event_seq=event_seq+1
WHERE id=$1 AND owner_id=$2 AND status='running' AND lease_owner=$3 AND lease_version=$4
 AND current_revision=desired_revision AND auth_version=$5 AND lease_expires_at>clock_timestamp()
 AND lease_version<$8
RETURNING event_seq`, taskID, ownerID, fence.LeaseOwner, fence.LeaseVersion, fence.AuthorizationVersion,
		string(status), reason, int64(math.MaxInt64)).Scan(&sequence)
	if err != nil {
		return 0, domain.NewError(domain.CodeConflict, "Task is no longer eligible for model recovery")
	}
	return sequence, nil
}

func queueTaskForModelRecoveryRetry(ctx context.Context, tx pgx.Tx, command app.TaskModelRecoveryRetryCommand) (int64, error) {
	var sequence int64
	err := tx.QueryRow(ctx, `UPDATE tasks SET status='queued',waiting_reason=$6,lease_owner='',lease_version=lease_version+1,
lease_expires_at=NULL,finished_at=NULL,updated_at=clock_timestamp(),event_seq=event_seq+1
WHERE id=$1 AND owner_id=$2 AND initiator_id=$3 AND status='waiting_user' AND waiting_reason=$4
 AND current_revision=$5 AND desired_revision=$5 AND auth_version=$7 AND event_seq=$8
 AND lease_owner='' AND lease_expires_at IS NULL AND lease_version<9223372036854775807
RETURNING event_seq`, command.TaskID, command.OwnerID, command.InitiatorID, taskModelRecoveryAvailableReason,
		command.TaskRevision, taskModelRecoveryScheduledReason, command.AuthorizationVersion, command.EventSequence).Scan(&sequence)
	if err != nil {
		return 0, domain.NewError(domain.CodeConflict, "Task is no longer eligible for model recovery retry")
	}
	return sequence, nil
}

func quarantineInvalidTaskModelRecoveryClaim(ctx context.Context, tx pgx.Tx, current sqlcgen.Task,
	intent task.ModelRecoveryIntent, hasIntent bool) error {
	recoveryState := task.ModelRecoverySuperseded
	if hasIntent && intent.State == task.ModelRecoveryAttempting {
		recoveryState = task.ModelRecoverySpent
	}
	if _, err := tx.Exec(ctx, `UPDATE task_model_recovery_schedules SET state=CASE WHEN state='attempting' THEN 'spent' ELSE 'superseded' END,
updated_at=clock_timestamp() WHERE task_id=$1 AND state IN ('retry_available','scheduled','attempting')`, current.ID); err != nil {
		return fmt.Errorf("quarantine invalid Task model recovery schedule: %w", err)
	}
	var sequence int64
	err := tx.QueryRow(ctx, `UPDATE tasks SET status='waiting_user',waiting_reason=$3,lease_owner='',lease_version=lease_version+1,
lease_expires_at=NULL,finished_at=NULL,updated_at=clock_timestamp(),event_seq=event_seq+1
WHERE id=$1 AND owner_id=$2 AND event_seq=$4 AND status=$5 AND lease_version<9223372036854775807
 AND (status='queued' OR (status='running' AND lease_expires_at<=clock_timestamp()))
RETURNING event_seq`, current.ID, current.OwnerID, taskModelRecoveryUnresolvedReason, current.EventSeq, current.Status).Scan(&sequence)
	if err != nil {
		return domain.NewError(domain.CodeConflict, "invalid Task model recovery claim changed before quarantine")
	}
	payload, err := json.Marshal(map[string]any{"status": string(task.StatusWaitingUser), "reason": taskModelRecoveryUnresolvedReason,
		"recovery_state": string(recoveryState)})
	if err != nil {
		return fmt.Errorf("encode invalid Task model recovery event: %w", err)
	}
	if err := insertTaskEvent(ctx, tx, task.Event{TaskID: current.ID, Sequence: sequence, Type: "task.waiting_user", Payload: payload}); err != nil {
		return err
	}
	return nil
}

func matchesTaskModelRecoveryCommand(intent task.ModelRecoveryIntent, command app.TaskModelRecoveryCommand, recoveryCallID string) bool {
	evidence := command.Failure.TaskModelRecoveryFailureEvidence()
	return intent.TaskID == command.Task.ID && intent.OwnerID == command.Task.OwnerID && intent.InitiatorID == command.Task.InitiatorID &&
		intent.TaskRevision == *command.Task.CurrentRevision && intent.AuthorizationVersion == evidence.TaskFence.AuthorizationVersion &&
		intent.AgentInstanceID == evidence.TaskFence.AgentInstanceID && intent.AgentGeneration == evidence.TaskFence.AgentGeneration &&
		intent.AgentSessionID == evidence.TaskFence.AgentSessionID && intent.FailedLeaseOwner == evidence.TaskFence.LeaseOwner &&
		intent.FailedLeaseVersion == evidence.TaskFence.LeaseVersion && intent.EngineVersion == evidence.TaskFence.EngineVersion &&
		intent.ProfileVersion == evidence.TaskFence.ProfileVersion && intent.FailedCallID == evidence.CallID &&
		intent.FailedModelID == evidence.ModelID && bytes.Equal(intent.FailedRequestDigest, evidence.RequestDigest) &&
		intent.FailedThroughSequence == evidence.TaskFence.ThroughSequence && intent.CheckpointThroughSequence == command.CheckpointThroughSequence &&
		bytes.Equal(intent.CheckpointDigest, command.CheckpointDigest) && bytes.Equal(intent.ToolsetDigest, command.ToolsetDigest) &&
		intent.RecoveryCallID == recoveryCallID && intent.Automatic == command.Auto
}

func validTaskModelRecoveryReplayState(ctx context.Context, tx pgx.Tx, intent task.ModelRecoveryIntent) bool {
	var matches bool
	reason := taskModelRecoveryScheduledReason
	status := string(task.StatusQueued)
	if intent.State == task.ModelRecoveryRetryAvailable {
		reason, status = taskModelRecoveryAvailableReason, string(task.StatusWaitingUser)
	} else if intent.State != task.ModelRecoveryScheduled {
		return false
	}
	err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM tasks t
 WHERE t.id=$1 AND t.owner_id=$2 AND t.initiator_id=$3 AND t.status=$4 AND t.waiting_reason=$5
   AND t.current_revision=$6 AND t.desired_revision=$6 AND t.auth_version=$7 AND t.event_seq=$8
   AND t.agent_instance_id=$9 AND t.agent_generation=$10 AND t.agent_session_id=$11
   AND t.agent_engine_version=$12 AND t.agent_profile_version=$13
   AND t.checkpoint_through_seq=$14 AND t.checkpoint_digest=$15
   AND t.lease_owner='' AND t.lease_expires_at IS NULL AND t.lease_version=$16
   AND EXISTS (SELECT 1 FROM task_agent_instances stopped
     WHERE stopped.task_id=$1 AND stopped.owner_id=$2 AND stopped.agent_instance_id=$9
       AND stopped.agent_generation=$10 AND stopped.agent_session_id=$11 AND stopped.state='stopped'
       AND stopped.lease_owner=$17 AND stopped.lease_version=$18 AND stopped.auth_version=$7
       AND stopped.engine_version=$12 AND stopped.profile_version=$13
       AND ((stopped.stop_receipt IS NOT NULL AND stopped.stop_receipt_digest=sha256(stopped.stop_receipt))
         OR (stopped.guardian_stop_attestation IS NOT NULL AND stopped.guardian_stop_attestation_digest=sha256(stopped.guardian_stop_attestation))))
)`, intent.TaskID, intent.OwnerID, intent.InitiatorID, status, reason, intent.TaskRevision, intent.AuthorizationVersion,
		intent.EventSequence, intent.AgentInstanceID, intent.AgentGeneration, intent.AgentSessionID, intent.EngineVersion,
		intent.ProfileVersion, intent.CheckpointThroughSequence, intent.CheckpointDigest, intent.FailedLeaseVersion+1,
		intent.FailedLeaseOwner, intent.FailedLeaseVersion).Scan(&matches)
	return err == nil && matches
}

func verifyTaskModelRecoveryRetryTask(current sqlcgen.Task, command app.TaskModelRecoveryRetryCommand) error {
	if current.ID != command.TaskID || current.OwnerID != command.OwnerID || current.InitiatorID != command.InitiatorID ||
		current.Status != string(task.StatusWaitingUser) || current.WaitingReason != taskModelRecoveryAvailableReason ||
		current.CurrentRevision != command.TaskRevision || current.DesiredRevision != command.TaskRevision ||
		current.AuthVersion != command.AuthorizationVersion || current.EventSeq != command.EventSequence ||
		current.LeaseOwner != "" || current.LeaseExpiresAt.Valid {
		return domain.NewError(domain.CodeConflict, "Task state changed before model recovery retry")
	}
	return nil
}

func sameTaskModelRecoveryIntent(left, right task.ModelRecoveryIntent) bool {
	return left.TaskID == right.TaskID && left.OwnerID == right.OwnerID && left.InitiatorID == right.InitiatorID &&
		left.TaskRevision == right.TaskRevision && left.AuthorizationVersion == right.AuthorizationVersion &&
		left.AgentInstanceID == right.AgentInstanceID && left.AgentGeneration == right.AgentGeneration &&
		left.AgentSessionID == right.AgentSessionID && left.FailedLeaseOwner == right.FailedLeaseOwner &&
		left.FailedLeaseVersion == right.FailedLeaseVersion && left.EngineVersion == right.EngineVersion && left.ProfileVersion == right.ProfileVersion &&
		left.FailedCallID == right.FailedCallID && left.FailedModelID == right.FailedModelID &&
		bytes.Equal(left.FailedRequestDigest, right.FailedRequestDigest) && left.FailedThroughSequence == right.FailedThroughSequence &&
		left.CheckpointThroughSequence == right.CheckpointThroughSequence && bytes.Equal(left.CheckpointDigest, right.CheckpointDigest) &&
		bytes.Equal(left.ToolsetDigest, right.ToolsetDigest) && left.RecoveryCallID == right.RecoveryCallID
}

func intentMatchesCurrentTaskForClaim(intent task.ModelRecoveryIntent, claimed task.Task) bool {
	return app.ValidTaskModelRecoveryIntent(intent) &&
		(intent.State == task.ModelRecoveryScheduled || intent.State == task.ModelRecoveryAttempting) &&
		claimed.ID == intent.TaskID && claimed.OwnerID == intent.OwnerID && claimed.InitiatorID == intent.InitiatorID &&
		claimed.CurrentRevision != nil && *claimed.CurrentRevision == intent.TaskRevision &&
		claimed.DesiredRevision != nil && *claimed.DesiredRevision == intent.TaskRevision &&
		claimed.Authorization.Version == intent.AuthorizationVersion && claimed.AgentSessionID == intent.AgentSessionID &&
		(claimed.AgentGeneration > intent.AgentGeneration || claimed.AgentInstanceID == intent.AgentInstanceID) &&
		claimed.AgentGeneration >= intent.AgentGeneration && claimed.AgentEngineVersion == intent.EngineVersion &&
		claimed.AgentProfileVersion == intent.ProfileVersion && claimed.CheckpointThroughSequence == intent.CheckpointThroughSequence &&
		bytes.Equal(claimed.CheckpointDigest, intent.CheckpointDigest)
}

func loadTaskModelRecoveryIntents(ctx context.Context, tx pgx.Tx, values []task.Task) error {
	if len(values) == 0 {
		return nil
	}
	ids, revisions := make([]string, 0, len(values)), make([]int64, 0, len(values))
	byID := make(map[string]*task.Task, len(values))
	for i := range values {
		if values[i].CurrentRevision == nil || *values[i].CurrentRevision < 0 {
			continue
		}
		ids = append(ids, values[i].ID)
		revisions = append(revisions, *values[i].CurrentRevision)
		byID[values[i].ID] = &values[i]
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT recovery.task_id,recovery.owner_id,recovery.initiator_id,recovery.task_revision,
recovery.authorization_version,recovery.agent_instance_id,recovery.agent_generation,recovery.agent_session_id,
recovery.failed_lease_owner,recovery.failed_lease_version,
recovery.engine_version,recovery.profile_version,recovery.failed_call_id,recovery.failed_model_id,recovery.failed_request_digest,recovery.failed_through_seq,
recovery.checkpoint_through_seq,recovery.checkpoint_digest,recovery.toolset_digest,recovery.recovery_call_id,
recovery.state,recovery.automatic,recovery.event_sequence,recovery.attempt_started_at,recovery.created_at,recovery.updated_at
FROM task_model_recovery_schedules recovery
JOIN unnest($1::text[],$2::bigint[]) requested(task_id,task_revision)
  ON requested.task_id=recovery.task_id AND requested.task_revision=recovery.task_revision`, ids, revisions)
	if err != nil {
		return fmt.Errorf("list Task model recovery schedules: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		value, err := scanTaskModelRecoveryIntent(rows)
		if err != nil {
			return err
		}
		projected := byID[value.TaskID]
		if projected == nil || !app.ValidTaskModelRecoveryIntent(value) || !intentMatchesCurrentTaskForProjection(value, *projected) {
			return domain.NewError(domain.CodeInvalidState, "stored Task model recovery projection is invalid")
		}
		projected.ModelRecovery = &value
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate Task model recovery schedules: %w", err)
	}
	return nil
}

func scanTaskModelRecoveryIntent(scanner interface{ Scan(...any) error }) (task.ModelRecoveryIntent, error) {
	var value task.ModelRecoveryIntent
	var state string
	var attemptStarted pgtype.Timestamptz
	if err := scanner.Scan(&value.TaskID, &value.OwnerID, &value.InitiatorID, &value.TaskRevision,
		&value.AuthorizationVersion, &value.AgentInstanceID, &value.AgentGeneration, &value.AgentSessionID,
		&value.FailedLeaseOwner, &value.FailedLeaseVersion,
		&value.EngineVersion, &value.ProfileVersion, &value.FailedCallID, &value.FailedModelID, &value.FailedRequestDigest, &value.FailedThroughSequence,
		&value.CheckpointThroughSequence, &value.CheckpointDigest, &value.ToolsetDigest, &value.RecoveryCallID,
		&state, &value.Automatic, &value.EventSequence, &attemptStarted, &value.CreatedAt, &value.UpdatedAt); err != nil {
		return task.ModelRecoveryIntent{}, fmt.Errorf("scan Task model recovery schedule: %w", err)
	}
	value.State = task.ModelRecoveryState(state)
	if attemptStarted.Valid {
		value.AttemptStartedAt = &attemptStarted.Time
	}
	return value, nil
}

func verifyTaskModelRecoveryFailureLedger(ctx context.Context, tx pgx.Tx, evidence app.TaskModelRecoveryFailureEvidence) error {
	if evidence.Kind == app.TaskModelRecoveryFailureNoDispatch {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2
  UNION ALL
  SELECT 1 FROM task_model_call_responses WHERE owner_id=$1 AND source='task' AND call_id=$2
)`, evidence.TaskFence.InitiatorID, evidence.CallID).Scan(&exists); err != nil {
			return fmt.Errorf("verify undispatched Task model ledger: %w", err)
		}
		if exists {
			return domain.NewError(domain.CodeConflict, "Task model call is not confirmed undispatched")
		}
		return nil
	}
	if evidence.Kind != app.TaskModelRecoveryFailureProviderReject ||
		(evidence.ProviderErrorKind != app.ModelErrorRateLimited && evidence.ProviderErrorKind != app.ModelErrorUnavailable) {
		return domain.NewError(domain.CodeConflict, "Task model failure is not eligible for recovery")
	}
	var state, modelID string
	var taskID *string
	var digest []byte
	var inputTokens, outputTokens *int64
	var responseExists bool
	err := tx.QueryRow(ctx, `SELECT ledger.state,ledger.task_id,ledger.model_id,ledger.request_digest,
ledger.input_tokens,ledger.output_tokens,EXISTS(SELECT 1 FROM task_model_call_responses response
  WHERE response.owner_id=ledger.owner_id AND response.source=ledger.source AND response.call_id=ledger.call_id)
FROM model_call_ledger ledger WHERE ledger.owner_id=$1 AND ledger.source='task' AND ledger.call_id=$2`,
		evidence.TaskFence.InitiatorID, evidence.CallID).Scan(&state, &taskID, &modelID, &digest, &inputTokens, &outputTokens, &responseExists)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeConflict, "Task model rejection is not settled")
	}
	if err != nil {
		return fmt.Errorf("verify rejected Task model ledger: %w", err)
	}
	if taskID == nil || *taskID != evidence.TaskFence.TaskID || state != "settled" || modelID != evidence.ModelID ||
		!bytes.Equal(digest, evidence.RequestDigest) || inputTokens == nil || outputTokens == nil ||
		*inputTokens != 0 || *outputTokens != 0 || responseExists {
		return domain.NewError(domain.CodeConflict, "Task model rejection usage or response is unresolved")
	}
	return nil
}

func verifyTaskModelRecoveryAttemptForReservation(ctx context.Context, tx pgx.Tx, request app.ModelCallReservation) error {
	if request.Task == nil || request.Task.RecoveryCallID == "" {
		return nil
	}
	fence := request.Task
	var eligible bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM task_model_recovery_schedules recovery
 JOIN tasks current ON current.id=recovery.task_id AND current.owner_id=recovery.owner_id AND current.initiator_id=recovery.initiator_id
 WHERE recovery.task_id=$1 AND recovery.owner_id=$2 AND recovery.initiator_id=$3
   AND recovery.task_revision=$4 AND recovery.authorization_version=$5
   AND recovery.agent_session_id=$6 AND recovery.failed_call_id=$7 AND recovery.recovery_call_id=$8
   AND recovery.failed_model_id=$9 AND recovery.failed_request_digest=$10
   AND recovery.failed_through_seq=$11 AND recovery.checkpoint_through_seq=$11
   AND recovery.engine_version=$12 AND recovery.profile_version=$13 AND recovery.state='attempting'
   AND recovery.agent_generation < $14 AND recovery.agent_instance_id <> $15
   AND current.status='running' AND current.current_revision=$4 AND current.desired_revision=$4
   AND current.auth_version=$5 AND current.agent_instance_id=$15 AND current.agent_generation=$14
   AND current.agent_session_id=$6 AND current.agent_engine_version=$12 AND current.agent_profile_version=$13
   AND current.checkpoint_through_seq=$11 AND current.checkpoint_digest=recovery.checkpoint_digest
   AND current.lease_owner=$16 AND current.lease_version=$17
)`, fence.TaskID, fence.OwnerID, fence.InitiatorID, fence.TaskRevision, fence.AuthorizationVersion,
		fence.AgentRootSessionID, request.DSHCallID, request.CallID, request.ModelID, request.RequestDigest,
		fence.ThroughSequence, fence.EngineVersion, fence.ProfileVersion, fence.AgentGeneration, fence.AgentInstanceID,
		fence.LeaseOwner, fence.LeaseVersion).Scan(&eligible)
	if err != nil {
		return fmt.Errorf("verify scheduled Task model recovery attempt: %w", err)
	}
	if !eligible || request.DSHCallID == "" || request.DSHCallID == request.CallID {
		return domain.NewError(domain.CodeConflict, "Task model recovery attempt is no longer current")
	}
	return nil
}

func intentMatchesCurrentTaskForProjection(intent task.ModelRecoveryIntent, current task.Task) bool {
	return current.CurrentRevision != nil && *current.CurrentRevision == intent.TaskRevision &&
		current.DesiredRevision != nil && *current.DesiredRevision == intent.TaskRevision &&
		current.ID == intent.TaskID && current.OwnerID == intent.OwnerID && current.InitiatorID == intent.InitiatorID &&
		current.Authorization.Version == intent.AuthorizationVersion
}

func taskModelRecoveryEventSequence(value task.Task) int64 {
	if value.EventSequence == math.MaxInt64 {
		return 0
	}
	return value.EventSequence + 1
}

var _ app.TaskModelRecoveryStore = (*Store)(nil)
