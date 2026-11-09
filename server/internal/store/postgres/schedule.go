package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/execution"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

const scheduleColumns = `id, owner_id, version, status, execution_epoch, host_id, model_id, title, objective, timing,
budget_active_seconds, budget_tool_calls, budget_model_tokens, auth_kind, auth_token_id, auth_scopes, auth_client_installation_id,
misfire_policy, notification_events, next_occurrence_key, next_run_at, next_check_at, observation_spec,
observation_config_hash, baseline_fingerprint, created_at, updated_at, execution_policy`

func storedScheduleExecutionPolicy(value schedule.ExecutionPolicy) schedule.ExecutionPolicy {
	if value == "" {
		return schedule.ExecutionPolicyStandard
	}
	return value
}

type scheduleRowScanner interface {
	Scan(...any) error
}

func (s *Store) CreateSchedule(ctx context.Context, value schedule.Schedule, record app.IdempotencyRecord) (schedule.Schedule, bool, error) {
	if record.OwnerID != value.OwnerID || record.ResourceID != value.ID || !schedule.ValidID(value.ID) || !domain.ValidUserID(value.OwnerID) {
		return schedule.Schedule{}, false, domain.InvalidArgument("schedule", "schedule owner or idempotency fields are invalid")
	}
	var result schedule.Schedule
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		stored, replay, err := reserveIdempotency(ctx, tx, record, value.ID, "")
		if err != nil {
			return err
		}
		if replay {
			result, err = loadSchedule(ctx, tx, record.OwnerID, stored.ResourceID)
			if err == nil {
				replayed = true
			}
			return err
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, value.OwnerID, nil)
		if err != nil {
			return err
		}
		valid, err := validateScheduleAuthorizationInTx(ctx, tx, value.OwnerID, value.Authorization)
		if err != nil {
			return err
		}
		if !valid {
			return domain.NewError(domain.CodeUnauthenticated, "schedule authorization is unavailable")
		}
		value.ExecutionPolicy = storedScheduleExecutionPolicy(value.ExecutionPolicy)
		value.ExecutionEpoch = gate.epoch
		if err := schedule.Validate(value); err != nil {
			return domain.InvalidArgument("schedule", err.Error())
		}
		if err := validateScheduleTaskFields(value); err != nil {
			return err
		}
		timing, err := json.Marshal(value.Timing)
		if err != nil {
			return fmt.Errorf("encode schedule timing: %w", err)
		}
		observation, err := marshalObservation(value.Observation)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO schedules (
id, owner_id, version, status, execution_epoch, host_id, model_id, title, objective, timing,
budget_active_seconds, budget_tool_calls, budget_model_tokens, auth_kind, auth_token_id, auth_scopes,
auth_client_installation_id, misfire_policy, notification_events, next_occurrence_key, next_run_at, next_check_at, observation_spec,
observation_config_hash, baseline_fingerprint, execution_policy
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26)`,
			value.ID, value.OwnerID, value.Version, string(value.Status), value.ExecutionEpoch, nullableText(value.HostID), value.ModelID, value.Title, value.Objective, timing,
			value.Budget.ActiveSeconds, value.Budget.ToolCalls, value.Budget.ModelTokens, value.Authorization.Kind,
			value.Authorization.TokenID, nonNilStrings(value.Authorization.Scopes), nullableText(value.Authorization.ClientInstallationID), string(value.MisfirePolicy),
			notificationEventStrings(value.NotificationEvents), value.NextOccurrenceKey,
			nullableTimestamp(value.NextRunAt), nullableTimestamp(value.NextCheckAt), observation,
			value.ObservationConfigHash, value.BaselineFingerprint, string(value.ExecutionPolicy))
		if err != nil {
			return fmt.Errorf("create schedule: %w", err)
		}
		if err := recordScheduleDefinitionChange(ctx, tx, value.OwnerID, value.ID, value.Version); err != nil {
			return err
		}
		contentBytes, err := personalScheduleContentBytesInTx(ctx, tx, value.OwnerID, value.ID)
		if err != nil {
			return err
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := applyPersonalObjectUsageDeltaInTx(ctx, tx, instanceID, value.OwnerID, contentBytes); err != nil {
			return err
		}
		result, err = loadSchedule(ctx, tx, value.OwnerID, value.ID)
		return err
	})
	return result, replayed, err
}

func (s *Store) ListSchedules(ctx context.Context, ownerID string, hostIDs []string, status schedule.Status, page app.PageRequest) (app.SchedulePage, error) {
	if !domain.ValidUserID(ownerID) || (status != "" && !status.Valid()) {
		return app.SchedulePage{}, domain.InvalidArgument("schedule_filter", "schedule filter is invalid")
	}
	limit, cursorAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return app.SchedulePage{}, err
	}
	for _, hostID := range hostIDs {
		if !domain.ValidHostID(hostID) {
			return app.SchedulePage{}, domain.InvalidArgument("host_ids", "host id is invalid")
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT `+prefixScheduleColumns("s")+`
FROM schedules s
WHERE s.owner_id=$1 AND ($2='' OR s.status=$2) AND ($3::text[] IS NULL OR s.host_id=ANY($3))
  AND (NOT $4::boolean OR (s.created_at,s.id)<($5,$6))
ORDER BY s.created_at DESC,s.id DESC LIMIT $7`, ownerID, string(status), hostIDs, cursorAt.Valid, cursorAt, cursorID, limit+1)
	if err != nil {
		return app.SchedulePage{}, fmt.Errorf("list schedules: %w", err)
	}
	values := make([]schedule.Schedule, 0)
	for rows.Next() {
		value, err := scanSchedule(rows)
		if err != nil {
			rows.Close()
			return app.SchedulePage{}, fmt.Errorf("scan schedule: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return app.SchedulePage{}, fmt.Errorf("iterate schedules: %w", err)
	}
	rows.Close()
	pageResult := app.SchedulePage{Items: values}
	pageResult.NextCursor = nextPageCursor(values, limit, func(value schedule.Schedule) (string, time.Time) { return value.ID, value.CreatedAt })
	if int32(len(values)) > limit {
		pageResult.Items = values[:limit]
	}
	for i := range pageResult.Items {
		lastRun, err := loadLastScheduleRun(ctx, s.pool, pageResult.Items[i].ID, ownerID)
		if err != nil {
			return app.SchedulePage{}, err
		}
		pageResult.Items[i].LastRun = lastRun
	}
	return pageResult, nil
}

func (s *Store) GetSchedule(ctx context.Context, ownerID, id string) (schedule.Schedule, error) {
	if !domain.ValidUserID(ownerID) || !schedule.ValidID(id) {
		return schedule.Schedule{}, domain.NewError(domain.CodeNotFound, "schedule not found")
	}
	return loadSchedule(ctx, s.pool, ownerID, id)
}

func (s *Store) UpdateSchedule(ctx context.Context, ownerID string, record app.IdempotencyRecord, value schedule.Schedule, expectedVersion int64) (schedule.Schedule, error) {
	if !domain.ValidUserID(ownerID) || value.OwnerID != ownerID || record.OwnerID != ownerID || record.Operation != "schedule.update" ||
		record.ResourceID != value.ID || expectedVersion < 1 || !schedule.ValidID(value.ID) {
		return schedule.Schedule{}, domain.InvalidArgument("schedule", "schedule update fields are invalid")
	}
	var result schedule.Schedule
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		stored, replay, err := reserveIdempotency(ctx, tx, record, value.ID, "")
		if err != nil {
			return err
		}
		if replay {
			if stored.ResourceID != value.ID {
				return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was used for a different schedule")
			}
			result, err = loadSchedule(ctx, tx, ownerID, stored.ResourceID)
			return err
		}
		gate, err := lockOwnerExecutionControl(ctx, tx, ownerID, false)
		if err != nil {
			return err
		}
		current, err := loadScheduleForUpdate(ctx, tx, ownerID, value.ID, true)
		if err != nil {
			return err
		}
		if current.Version != expectedVersion {
			return scheduleVersionConflictError(expectedVersion, current.Version)
		}
		valid, err := validateScheduleAuthorizationInTx(ctx, tx, ownerID, value.Authorization)
		if err != nil {
			return err
		}
		if !valid {
			return domain.NewError(domain.CodeUnauthenticated, "schedule authorization is unavailable")
		}
		value.Version = current.Version + 1
		value.ExecutionEpoch = gate.epoch
		if value.Status == schedule.StatusEnabled && gate.state != execution.StateEnabled {
			return ownerExecutionDisabledError(gate)
		}
		if err := schedule.Validate(value); err != nil {
			return domain.InvalidArgument("schedule", err.Error())
		}
		if err := validateScheduleTaskFields(value); err != nil {
			return err
		}
		previousContentBytes, err := personalScheduleContentBytesInTx(ctx, tx, ownerID, value.ID)
		if err != nil {
			return err
		}
		if err := updateScheduleRow(ctx, tx, ownerID, value); err != nil {
			return err
		}
		if err := recordScheduleDefinitionChange(ctx, tx, ownerID, value.ID, value.Version); err != nil {
			return err
		}
		nextContentBytes, err := personalScheduleContentBytesInTx(ctx, tx, ownerID, value.ID)
		if err != nil {
			return err
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := applyPersonalObjectUsageDeltaInTx(ctx, tx, instanceID, ownerID, nextContentBytes-previousContentBytes); err != nil {
			return err
		}
		result, err = loadSchedule(ctx, tx, ownerID, value.ID)
		return err
	})
	return result, err
}

func (s *Store) SetScheduleStatus(ctx context.Context, record app.IdempotencyRecord, id string, target schedule.Status, expectedVersion int64, authorization task.Authorization) (schedule.Schedule, error) {
	if !schedule.ValidID(id) || !domain.ValidUserID(record.OwnerID) || record.ResourceID != id || expectedVersion < 1 ||
		(target != schedule.StatusEnabled && target != schedule.StatusDisabled) {
		return schedule.Schedule{}, domain.InvalidArgument("schedule_action", "schedule action fields are invalid")
	}
	var result schedule.Schedule
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		gate, err := lockOwnerExecutionControl(ctx, tx, record.OwnerID, false)
		if err != nil {
			return err
		}
		stored, replay, err := reserveIdempotency(ctx, tx, record, id, "")
		if err != nil {
			return err
		}
		if replay {
			result, err = loadSchedule(ctx, tx, record.OwnerID, stored.ResourceID)
			return err
		}
		value, err := loadScheduleForUpdate(ctx, tx, record.OwnerID, id, true)
		if err != nil {
			return err
		}
		if value.Version != expectedVersion {
			return scheduleVersionConflictError(expectedVersion, value.Version)
		}
		if target == schedule.StatusEnabled && gate.state != execution.StateEnabled {
			return ownerExecutionDisabledError(gate)
		}
		if value.Status == schedule.StatusCompleted && target == schedule.StatusEnabled {
			return domain.NewError(domain.CodeInvalidState, "completed schedule cannot be enabled")
		}
		statusChanged := value.Status != target
		changed := statusChanged
		if target == schedule.StatusEnabled {
			previousAuthorization := value.Authorization
			value.Authorization = authorization
			changed = changed || !sameTaskAuthorization(previousAuthorization, value.Authorization)
			valid, err := validateScheduleAuthorizationInTx(ctx, tx, record.OwnerID, value.Authorization)
			if err != nil {
				return err
			}
			if !valid {
				return domain.NewError(domain.CodeUnauthenticated, "schedule authorization is unavailable")
			}
		}
		if changed {
			if statusChanged {
				value.Status = target
			}
			value.Version++
			value.ExecutionEpoch = gate.epoch
			if statusChanged && target == schedule.StatusEnabled {
				next, err := schedule.FirstOccurrence(value.Timing, time.Now().UTC())
				if err != nil {
					return fmt.Errorf("calculate enabled schedule occurrence: %w", err)
				}
				value.NextOccurrenceKey, value.NextRunAt, value.NextCheckAt = next.Key, next.ScheduledAt, &next.CheckAt
			} else if statusChanged {
				value.NextOccurrenceKey, value.NextRunAt, value.NextCheckAt = "", nil, nil
			}
			if err := schedule.Validate(value); err != nil {
				return domain.InvalidArgument("schedule", err.Error())
			}
			if err := validateScheduleTaskFields(value); err != nil {
				return err
			}
			if err := updateScheduleRow(ctx, tx, record.OwnerID, value); err != nil {
				return err
			}
			if err := recordScheduleDefinitionChange(ctx, tx, record.OwnerID, value.ID, value.Version); err != nil {
				return err
			}
		}
		result, err = loadSchedule(ctx, tx, record.OwnerID, id)
		return err
	})
	return result, err
}

// recordScheduleDefinitionChange publishes the definition's business version
// to personal sync without storing a second copy of the schedule body. The
// schedule row is the source of truth for both its definition and revision.
func recordScheduleDefinitionChange(ctx context.Context, tx pgx.Tx, ownerID, scheduleID string, revision int64) error {
	if !domain.ValidUserID(ownerID) || !schedule.ValidID(scheduleID) || revision < 1 {
		return domain.InvalidArgument("schedule", "schedule sync fields are invalid")
	}
	instanceID, err := serviceInstanceIDInTx(ctx, tx)
	if err != nil {
		return err
	}
	lastSequence, err := lockAccountChangeCounter(ctx, tx, instanceID, ownerID)
	if err != nil {
		return err
	}
	if lastSequence == math.MaxInt64 {
		return domain.NewError(domain.CodeInvalidState, "account change sequence is exhausted")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_object_versions
(service_instance_id, owner_id, object_type, object_id, revision, is_deleted)
VALUES ($1::uuid,$2,'schedule',$3,$4,false)
ON CONFLICT (service_instance_id, owner_id, object_type, object_id) DO UPDATE
SET revision=EXCLUDED.revision, is_deleted=false, updated_at=clock_timestamp()`,
		instanceID, ownerID, scheduleID, revision); err != nil {
		return fmt.Errorf("advance schedule sync version: %w", err)
	}
	lastSequence++
	if _, err := tx.Exec(ctx, `INSERT INTO account_change_log
(service_instance_id, owner_id, change_sequence, object_type, object_id, revision, operation)
VALUES ($1::uuid,$2,$3,'schedule',$4,$5,'upsert')`, instanceID, ownerID, lastSequence, scheduleID, revision); err != nil {
		return fmt.Errorf("append schedule account change: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE account_change_counters SET last_sequence=$3, updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2`, instanceID, ownerID, lastSequence); err != nil {
		return fmt.Errorf("advance schedule account change counter: %w", err)
	}
	return nil
}

func (s *Store) ListScheduleRuns(ctx context.Context, ownerID, scheduleID string, hostIDs []string, page app.PageRequest) (app.ScheduleRunPage, error) {
	if !domain.ValidUserID(ownerID) || !schedule.ValidID(scheduleID) {
		return app.ScheduleRunPage{}, domain.NewError(domain.CodeNotFound, "schedule not found")
	}
	limit, cursorAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return app.ScheduleRunPage{}, err
	}
	for _, hostID := range hostIDs {
		if !domain.ValidHostID(hostID) {
			return app.ScheduleRunPage{}, domain.InvalidArgument("host_ids", "host id is invalid")
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT r.id,r.schedule_id,r.owner_id,r.schedule_version,r.occurrence_key,r.scheduled_at,
r.coalesced_count,r.status,r.skip_reason,r.task_id,r.notification_events,r.observation_spec,r.observation_config_hash,
r.observation_outcome,r.observation_fingerprint,r.created_at,r.updated_at,r.finished_at,
t.status,t.updated_at,t.finished_at
FROM schedule_runs r JOIN schedules s ON s.id=r.schedule_id AND s.owner_id=r.owner_id
LEFT JOIN tasks t ON t.id=r.task_id
WHERE r.owner_id=$1 AND r.schedule_id=$2 AND ($3::text[] IS NULL OR s.host_id=ANY($3))
  AND (NOT $4::boolean OR (r.created_at,r.id)<($5,$6))
ORDER BY r.created_at DESC,r.id DESC LIMIT $7`, ownerID, scheduleID, hostIDs, cursorAt.Valid, cursorAt, cursorID, limit+1)
	if err != nil {
		return app.ScheduleRunPage{}, fmt.Errorf("list schedule runs: %w", err)
	}
	values := make([]schedule.Run, 0)
	for rows.Next() {
		value, err := scanRunWithTask(rows)
		if err != nil {
			rows.Close()
			return app.ScheduleRunPage{}, fmt.Errorf("scan schedule run: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return app.ScheduleRunPage{}, fmt.Errorf("iterate schedule runs: %w", err)
	}
	rows.Close()
	pageResult := app.ScheduleRunPage{Items: values}
	pageResult.NextCursor = nextPageCursor(values, limit, func(value schedule.Run) (string, time.Time) { return value.ID, value.CreatedAt })
	if int32(len(values)) > limit {
		pageResult.Items = values[:limit]
	}
	return pageResult, nil
}

func (s *Store) ListDueSchedules(ctx context.Context, now time.Time, limit int32) ([]schedule.Schedule, error) {
	if now.IsZero() || limit < 1 || limit > 100 {
		return nil, domain.InvalidArgument("schedule_poll", "schedule poll bounds are invalid")
	}
	rows, err := s.pool.Query(ctx, `SELECT `+prefixScheduleColumns("s")+`
FROM schedules s WHERE s.status='enabled' AND s.next_check_at <= $1
ORDER BY s.next_check_at,s.id LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list due schedules: %w", err)
	}
	values := make([]schedule.Schedule, 0)
	for rows.Next() {
		value, err := scanSchedule(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan due schedule: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate due schedules: %w", err)
	}
	rows.Close()
	return values, nil
}

func (s *Store) CommitScheduleOccurrence(ctx context.Context, commit app.ScheduleOccurrenceCommit) (schedule.Run, bool, error) {
	if !domain.ValidUserID(commit.OwnerID) || !schedule.ValidID(commit.ScheduleID) || commit.ExpectedVersion < 1 || commit.ExpectedEpoch < 1 ||
		commit.Occurrence.Key == "" || commit.Occurrence.CheckAt.IsZero() || !commit.Occurrence.Status.Valid() ||
		(commit.Occurrence.Status == schedule.RunSkipped && commit.Occurrence.SkipReason == nil) ||
		(commit.Occurrence.Status != schedule.RunSkipped && commit.Occurrence.SkipReason != nil) {
		return schedule.Run{}, false, domain.InvalidArgument("occurrence", "scheduled occurrence fields are invalid")
	}
	if commit.Occurrence.ScheduledAt == nil && (commit.Occurrence.SkipReason == nil || *commit.Occurrence.SkipReason != schedule.SkipDSTGap) {
		return schedule.Run{}, false, domain.InvalidArgument("occurrence", "only a DST gap may have no scheduled instant")
	}
	if err := validateScheduleNotificationEvents(commit.NotificationEvents); err != nil {
		return schedule.Run{}, false, err
	}
	runID, err := schedule.NewRunID()
	if err != nil {
		return schedule.Run{}, false, fmt.Errorf("generate schedule run id: %w", err)
	}
	var taskID, conversationID, messageID string
	if commit.Occurrence.Status == schedule.RunQueued {
		if commit.Occurrence.ScheduledAt == nil {
			return schedule.Run{}, false, domain.InvalidArgument("occurrence", "queued occurrence requires a scheduled instant")
		}
		if taskID, err = domain.NewTaskID(); err != nil {
			return schedule.Run{}, false, fmt.Errorf("generate scheduled task id: %w", err)
		}
		if conversationID, err = domain.NewConversationID(); err != nil {
			return schedule.Run{}, false, fmt.Errorf("generate scheduled conversation id: %w", err)
		}
		if messageID, err = domain.NewMessageID(); err != nil {
			return schedule.Run{}, false, fmt.Errorf("generate scheduled message id: %w", err)
		}
	}
	var result schedule.Run
	var committed bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		gate, err := requireOwnerExecutionEnabled(ctx, tx, commit.OwnerID, &commit.ExpectedEpoch)
		if err != nil {
			return err
		}
		value, err := loadScheduleForOccurrence(ctx, tx, commit.OwnerID, commit.ScheduleID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if value.Status != schedule.StatusEnabled || value.Version != commit.ExpectedVersion || value.ExecutionEpoch != gate.epoch ||
			value.NextOccurrenceKey == "" || value.NextOccurrenceKey != commit.Occurrence.Key || value.NextCheckAt == nil ||
			!value.NextCheckAt.Equal(commit.Occurrence.CheckAt) || !sameTime(value.NextRunAt, commit.Occurrence.ScheduledAt) {
			return nil
		}
		var due bool
		if err := tx.QueryRow(ctx, `SELECT next_check_at <= clock_timestamp() FROM schedules WHERE id=$1 AND owner_id=$2`, value.ID, value.OwnerID).Scan(&due); err != nil {
			return fmt.Errorf("recheck scheduled occurrence due time: %w", err)
		}
		if !due {
			return nil
		}
		validAuthorization, err := validateScheduleAuthorizationInTx(ctx, tx, value.OwnerID, value.Authorization)
		if err != nil {
			return err
		}
		occurrence := commit.Occurrence
		if !validAuthorization {
			reason := schedule.SkipAuthorizationEnded
			occurrence.Status, occurrence.SkipReason, occurrence.CoalescedCount = schedule.RunSkipped, &reason, 0
			taskID = ""
		}
		next, err := schedule.NextOccurrence(value.Timing, commit.Occurrence.Key)
		if value.Timing.Kind == schedule.TimingOnce {
			if commit.NextOccurrence != nil {
				return domain.InvalidArgument("next_occurrence", "one-time schedule cannot have a next occurrence")
			}
		} else if err != nil || commit.NextOccurrence == nil || next.Key != commit.NextOccurrence.Key ||
			!next.CheckAt.Equal(commit.NextOccurrence.CheckAt) || !sameTime(next.ScheduledAt, commit.NextOccurrence.ScheduledAt) {
			return domain.InvalidArgument("next_occurrence", "next scheduled occurrence does not match timing")
		}
		var existing string
		err = tx.QueryRow(ctx, `SELECT id FROM schedule_runs WHERE schedule_id=$1 AND occurrence_key=$2`, value.ID, commit.Occurrence.Key).Scan(&existing)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check duplicate schedule occurrence: %w", err)
		}

		if occurrence.Status == schedule.RunQueued {
			var priorTask string
			err := tx.QueryRow(ctx, `SELECT t.id FROM schedule_runs r JOIN tasks t ON t.id=r.task_id
WHERE r.schedule_id=$1 AND t.status IN ('queued','running','waiting_user','pausing','paused','cancelling')
ORDER BY r.created_at DESC,r.id DESC LIMIT 1 FOR UPDATE OF t`, value.ID).Scan(&priorTask)
			if err == nil {
				reason := schedule.SkipOverlap
				occurrence.Status, occurrence.SkipReason = schedule.RunSkipped, &reason
				taskID = ""
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("check overlapping schedule run: %w", err)
			}
		}
		if occurrence.Status == schedule.RunQueued {
			if value.HostID != "" {
				if err := lockAIHost(ctx, tx, value.HostID, value.OwnerID); err != nil {
					return err
				}
			}
			if err := validateScheduleTaskFields(value); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO conversations (id,owner_id,host_id,title) VALUES ($1,$2,$3,$4)`,
				conversationID, value.OwnerID, nullableText(value.HostID), truncateScheduleTitle(value.Title)); err != nil {
				return fmt.Errorf("create scheduled conversation: %w", err)
			}
			scheduledScopes := identity.ScheduleTaskExecutionScopes()
			if value.HostID == "" {
				scheduledScopes = identity.HostlessScheduleTaskExecutionScopes()
			}
			if value.ExecutionPolicy == schedule.ExecutionPolicyGoalFollowupReadOnly {
				if value.HostID != "" {
					return domain.NewError(domain.CodeInvalidState, "read-only Goal follow-up schedule cannot target a Host")
				}
				scheduledScopes = identity.GoalFollowupScheduleTaskExecutionScopes()
			}
			internalAuthorization := task.Authorization{Kind: string(identity.KindScheduleTask), TokenID: runID, Scopes: make([]string, 0, len(scheduledScopes)), Version: 1}
			for _, scope := range scheduledScopes {
				internalAuthorization.Scopes = append(internalAuthorization.Scopes, string(scope))
			}
			taskValue := normalizeNewTask(task.Task{
				ID: taskID, OwnerID: value.OwnerID, InitiatorID: value.OwnerID, ConversationID: conversationID, HostID: value.HostID,
				ExecutionLocation: "server", ModelID: value.ModelID, Objective: value.Objective, Status: task.StatusQueued,
				Authorization: internalAuthorization, Budget: value.Budget,
			})
			if err := validateTask(taskValue); err != nil {
				return err
			}
			_, contextManifest, err := captureTaskConversationContext(ctx, tx, value.OwnerID, conversationID)
			if err != nil {
				return err
			}
			initialEventPayload, err := attachTaskConversationContextManifest([]byte(`{"status":"queued"}`), contextManifest)
			if err != nil {
				return err
			}
			if err := insertTask(ctx, tx, taskValue, 1, gate.epoch); err != nil {
				return err
			}
			message := conversation.Message{
				ID: messageID, ConversationID: conversationID, OwnerID: value.OwnerID, Role: conversation.RoleUser,
				Content: value.Objective, TaskID: taskID,
			}
			if err := validateMessage(message); err != nil {
				return err
			}
			if err := insertMessage(ctx, tx, message); err != nil {
				return err
			}
			if err := insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: 1, Type: "task.status", Payload: initialEventPayload}); err != nil {
				return err
			}
		}
		var taskIDValue any
		if taskID != "" {
			taskIDValue = taskID
		}
		var scheduledAt any
		if occurrence.ScheduledAt != nil {
			scheduledAt = *occurrence.ScheduledAt
		}
		var skipReason any
		if occurrence.SkipReason != nil {
			skipReason = string(*occurrence.SkipReason)
		}
		observation, err := marshalObservation(value.Observation)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO schedule_runs (
id,schedule_id,owner_id,schedule_version,occurrence_key,scheduled_at,coalesced_count,status,skip_reason,task_id,notification_events,
observation_spec,observation_config_hash,observation_outcome,observation_fingerprint,finished_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'unknown','',CASE WHEN $8 IN ('skipped','missed') THEN clock_timestamp() ELSE NULL END)`, runID, value.ID, value.OwnerID, value.Version,
			occurrence.Key, scheduledAt, occurrence.CoalescedCount, string(occurrence.Status), skipReason,
			taskIDValue, notificationEventStrings(commit.NotificationEvents), observation, value.ObservationConfigHash)
		if err != nil {
			return fmt.Errorf("record schedule occurrence: %w", err)
		}
		var completedDefinitionVersion int64
		if value.Timing.Kind == schedule.TimingOnce {
			_, err = tx.Exec(ctx, `UPDATE schedules SET status='completed',version=version+1,next_occurrence_key='',next_run_at=NULL,next_check_at=NULL,updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2`, value.ID, value.OwnerID)
			completedDefinitionVersion = value.Version + 1
		} else {
			_, err = tx.Exec(ctx, `UPDATE schedules SET next_occurrence_key=$3,next_run_at=$4,next_check_at=$5,updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2`, value.ID, value.OwnerID, commit.NextOccurrence.Key,
				nullableTimestamp(commit.NextOccurrence.ScheduledAt), commit.NextOccurrence.CheckAt)
		}
		if err != nil {
			return fmt.Errorf("advance schedule occurrence: %w", err)
		}
		if taskID != "" {
			if err := recordAccountObjectChanges(ctx, tx,
				accountObjectChange{OwnerID: value.OwnerID, ObjectType: "conversation", ObjectID: conversationID, Operation: "upsert"},
				accountObjectChange{OwnerID: value.OwnerID, ObjectType: "message", ObjectID: messageID, Operation: "upsert"},
			); err != nil {
				return err
			}
		}
		if completedDefinitionVersion > 0 {
			if err := recordScheduleDefinitionChange(ctx, tx, value.OwnerID, value.ID, completedDefinitionVersion); err != nil {
				return err
			}
		}
		result = schedule.Run{
			ID: runID, ScheduleID: value.ID, ScheduleVersion: value.Version, OccurrenceKey: occurrence.Key,
			ScheduledAt: occurrence.ScheduledAt, CoalescedCount: occurrence.CoalescedCount, Status: occurrence.Status,
			SkipReason: occurrence.SkipReason, NotificationEvents: append([]schedule.NotificationEvent(nil), commit.NotificationEvents...),
			Observation: value.Observation, ObservationConfigHash: value.ObservationConfigHash, ObservationOutcome: schedule.ObservationUnknown,
		}
		if taskID != "" {
			result.TaskID = &taskID
		}
		var finishedAt pgtype.Timestamptz
		if err := tx.QueryRow(ctx, `SELECT created_at,updated_at,finished_at FROM schedule_runs WHERE id=$1`, runID).
			Scan(&result.CreatedAt, &result.UpdatedAt, &finishedAt); err != nil {
			return fmt.Errorf("read created schedule run: %w", err)
		}
		if finishedAt.Valid {
			result.FinishedAt = &finishedAt.Time
		}
		committed = true
		return nil
	})
	return result, committed, err
}

func loadSchedule(ctx context.Context, queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, ownerID, id string) (schedule.Schedule, error) {
	value, err := scanSchedule(queryer.QueryRow(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE owner_id=$1 AND id=$2`, ownerID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return schedule.Schedule{}, domain.NewError(domain.CodeNotFound, "schedule not found")
	}
	if err != nil {
		return schedule.Schedule{}, fmt.Errorf("load schedule: %w", err)
	}
	value.LastRun, err = loadLastScheduleRun(ctx, queryer, id, ownerID)
	if err != nil {
		return schedule.Schedule{}, err
	}
	value.CurrentRun, err = loadCurrentScheduleRun(ctx, queryer, id, ownerID)
	if err != nil {
		return schedule.Schedule{}, err
	}
	return value, nil
}

func loadScheduleForUpdate(ctx context.Context, tx pgx.Tx, ownerID, id string, lock bool) (schedule.Schedule, error) {
	query := `SELECT ` + scheduleColumns + ` FROM schedules WHERE owner_id=$1 AND id=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	value, err := scanSchedule(tx.QueryRow(ctx, query, ownerID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return schedule.Schedule{}, domain.NewError(domain.CodeNotFound, "schedule not found")
	}
	if err != nil {
		return schedule.Schedule{}, fmt.Errorf("load schedule for update: %w", err)
	}
	value.LastRun, err = loadLastScheduleRun(ctx, tx, id, ownerID)
	if err != nil {
		return schedule.Schedule{}, err
	}
	value.CurrentRun, err = loadCurrentScheduleRun(ctx, tx, id, ownerID)
	return value, err
}

func loadScheduleForOccurrence(ctx context.Context, tx pgx.Tx, ownerID, id string) (schedule.Schedule, error) {
	return scanSchedule(tx.QueryRow(ctx, `SELECT `+scheduleColumns+`
FROM schedules WHERE owner_id=$1 AND id=$2 FOR UPDATE SKIP LOCKED`, ownerID, id))
}

func loadLastScheduleRun(ctx context.Context, queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id, ownerID string) (*schedule.Run, error) {
	row, err := scanRunWithTask(queryer.QueryRow(ctx, `SELECT r.id,r.schedule_id,r.owner_id,r.schedule_version,r.occurrence_key,r.scheduled_at,
r.coalesced_count,r.status,r.skip_reason,r.task_id,r.notification_events,r.observation_spec,r.observation_config_hash,
r.observation_outcome,r.observation_fingerprint,r.created_at,r.updated_at,r.finished_at,
t.status,t.updated_at,t.finished_at
FROM schedule_runs r LEFT JOIN tasks t ON t.id=r.task_id
WHERE r.schedule_id=$1 AND r.owner_id=$2 ORDER BY r.created_at DESC,r.id DESC LIMIT 1`, id, ownerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load latest schedule run: %w", err)
	}
	return &row, nil
}

func loadCurrentScheduleRun(ctx context.Context, queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id, ownerID string) (*schedule.Run, error) {
	row, err := scanRunWithTask(queryer.QueryRow(ctx, `SELECT r.id,r.schedule_id,r.owner_id,r.schedule_version,r.occurrence_key,r.scheduled_at,
r.coalesced_count,r.status,r.skip_reason,r.task_id,r.notification_events,r.observation_spec,r.observation_config_hash,
r.observation_outcome,r.observation_fingerprint,r.created_at,r.updated_at,r.finished_at,
t.status,t.updated_at,t.finished_at
FROM schedule_runs r JOIN tasks t ON t.id=r.task_id
WHERE r.schedule_id=$1 AND r.owner_id=$2 AND t.status IN ('queued','running','waiting_user','pausing','paused','cancelling')
ORDER BY r.created_at DESC,r.id DESC LIMIT 1`, id, ownerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load current schedule run: %w", err)
	}
	return &row, nil
}

func scanSchedule(row scheduleRowScanner) (schedule.Schedule, error) {
	var value schedule.Schedule
	var status, authKind, misfirePolicy, executionPolicy string
	var authClientInstallationID pgtype.Text
	var timingJSON, observationJSON []byte
	var authScopes, notificationEvents []string
	var nextRunAt, nextCheckAt, createdAt, updatedAt pgtype.Timestamptz
	var hostID pgtype.Text
	err := row.Scan(&value.ID, &value.OwnerID, &value.Version, &status, &value.ExecutionEpoch, &hostID, &value.ModelID,
		&value.Title, &value.Objective, &timingJSON, &value.Budget.ActiveSeconds, &value.Budget.ToolCalls, &value.Budget.ModelTokens,
		&authKind, &value.Authorization.TokenID, &authScopes, &authClientInstallationID, &misfirePolicy, &notificationEvents, &value.NextOccurrenceKey,
		&nextRunAt, &nextCheckAt, &observationJSON, &value.ObservationConfigHash, &value.BaselineFingerprint, &createdAt, &updatedAt, &executionPolicy)
	if err != nil {
		return schedule.Schedule{}, err
	}
	if err := json.Unmarshal(timingJSON, &value.Timing); err != nil {
		return schedule.Schedule{}, fmt.Errorf("decode schedule timing: %w", err)
	}
	if len(observationJSON) != 0 {
		var observation schedule.ObservationSpec
		if err := json.Unmarshal(observationJSON, &observation); err != nil {
			return schedule.Schedule{}, fmt.Errorf("decode schedule observation: %w", err)
		}
		value.Observation = &observation
	}
	value.Status = schedule.Status(status)
	value.HostID = hostID.String
	value.MisfirePolicy = schedule.MisfirePolicy(misfirePolicy)
	value.ExecutionPolicy = schedule.ExecutionPolicy(executionPolicy)
	value.Authorization.Kind = authKind
	value.Authorization.Scopes = append([]string(nil), authScopes...)
	value.Authorization.ClientInstallationID = authClientInstallationID.String
	value.NotificationEvents = scheduleNotificationEvents(notificationEvents)
	value.NextRunAt, value.NextCheckAt = timePointer(nextRunAt), timePointer(nextCheckAt)
	value.CreatedAt, value.UpdatedAt = createdAt.Time, updatedAt.Time
	return value, nil
}

func scanRunWithTask(row scheduleRowScanner) (schedule.Run, error) {
	var value schedule.Run
	var ownerID, status string
	var skipReason, taskID, taskStatus pgtype.Text
	var scheduledAt, finishedAt, taskUpdatedAt, taskFinishedAt pgtype.Timestamptz
	var notificationEvents []string
	var observationJSON []byte
	err := row.Scan(&value.ID, &value.ScheduleID, &ownerID, &value.ScheduleVersion, &value.OccurrenceKey, &scheduledAt,
		&value.CoalescedCount, &status, &skipReason, &taskID, &notificationEvents, &observationJSON, &value.ObservationConfigHash,
		&value.ObservationOutcome, &value.ObservationFingerprint, &value.CreatedAt, &value.UpdatedAt, &finishedAt,
		&taskStatus, &taskUpdatedAt, &taskFinishedAt)
	if err != nil {
		return schedule.Run{}, err
	}
	value.ScheduledAt = timePointer(scheduledAt)
	value.Status = schedule.RunStatus(status)
	value.NotificationEvents = scheduleNotificationEvents(notificationEvents)
	if len(observationJSON) != 0 {
		var observation schedule.ObservationSpec
		if err := json.Unmarshal(observationJSON, &observation); err != nil {
			return schedule.Run{}, fmt.Errorf("decode schedule run observation: %w", err)
		}
		value.Observation = &observation
	}
	if skipReason.Valid {
		reason := schedule.SkipReason(skipReason.String)
		value.SkipReason = &reason
	}
	if taskID.Valid {
		value.TaskID = &taskID.String
	}
	if finishedAt.Valid {
		value.FinishedAt = &finishedAt.Time
	}
	if taskStatus.Valid {
		value.Status = scheduleRunStatus(task.Status(taskStatus.String))
	}
	if taskUpdatedAt.Valid && taskUpdatedAt.Time.After(value.UpdatedAt) {
		value.UpdatedAt = taskUpdatedAt.Time
	}
	if taskFinishedAt.Valid {
		value.FinishedAt = &taskFinishedAt.Time
	}
	return value, nil
}

func scheduleRunStatus(status task.Status) schedule.RunStatus {
	if status == task.StatusCancelling {
		return schedule.RunStopping
	}
	return schedule.RunStatus(status)
}

func updateScheduleRow(ctx context.Context, tx pgx.Tx, ownerID string, value schedule.Schedule) error {
	timing, err := json.Marshal(value.Timing)
	if err != nil {
		return fmt.Errorf("encode schedule timing: %w", err)
	}
	observation, err := marshalObservation(value.Observation)
	if err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE schedules SET version=$3,status=$4,execution_epoch=$5,host_id=$6,model_id=$7,title=$8,objective=$9,timing=$10,
budget_active_seconds=$11,budget_tool_calls=$12,budget_model_tokens=$13,auth_kind=$14,auth_token_id=$15,auth_scopes=$16,
auth_client_installation_id=$17,misfire_policy=$18,notification_events=$19,next_occurrence_key=$20,next_run_at=$21,next_check_at=$22,observation_spec=$23,
observation_config_hash=$24,baseline_fingerprint=$25,execution_policy=$26,updated_at=clock_timestamp()
		WHERE id=$1 AND owner_id=$2`, value.ID, ownerID, value.Version, string(value.Status), value.ExecutionEpoch, nullableText(value.HostID), value.ModelID,
		value.Title, value.Objective, timing, value.Budget.ActiveSeconds, value.Budget.ToolCalls, value.Budget.ModelTokens,
		value.Authorization.Kind, value.Authorization.TokenID, nonNilStrings(value.Authorization.Scopes), nullableText(value.Authorization.ClientInstallationID), string(value.MisfirePolicy),
		notificationEventStrings(value.NotificationEvents), value.NextOccurrenceKey, nullableTimestamp(value.NextRunAt), nullableTimestamp(value.NextCheckAt),
		observation, value.ObservationConfigHash, value.BaselineFingerprint, string(storedScheduleExecutionPolicy(value.ExecutionPolicy)))
	if err != nil {
		return fmt.Errorf("update schedule: %w", err)
	}
	if command.RowsAffected() != 1 {
		return domain.NewError(domain.CodeNotFound, "schedule not found")
	}
	return nil
}

func personalScheduleContentBytesInTx(ctx context.Context, tx pgx.Tx, ownerID, scheduleID string) (int64, error) {
	var contentBytes int64
	err := tx.QueryRow(ctx, `SELECT octet_length(jsonb_build_object(
  'title',title,'objective',objective,'model',model_id,'host',host_id,'timing',timing,
  'budgets',jsonb_build_object('active_seconds',budget_active_seconds,'tool_calls',budget_tool_calls,'model_tokens',budget_model_tokens),
  'misfire',misfire_policy,'notification',to_jsonb(notification_events),'observation',observation_spec,'execution_policy',execution_policy
)::text) FROM schedules WHERE id=$1 AND owner_id=$2`, scheduleID, ownerID).Scan(&contentBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, domain.NewError(domain.CodeNotFound, "schedule not found")
	}
	if err != nil {
		return 0, fmt.Errorf("measure schedule personal data content: %w", err)
	}
	if contentBytes < 1 {
		return 0, domain.NewError(domain.CodeInternal, "schedule personal data size is invalid")
	}
	return contentBytes, nil
}

func marshalObservation(value *schedule.ObservationSpec) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode schedule observation: %w", err)
	}
	return encoded, nil
}

func prefixScheduleColumns(alias string) string {
	columns := strings.Split(scheduleColumns, ",")
	for i := range columns {
		columns[i] = alias + "." + strings.TrimSpace(columns[i])
	}
	return strings.Join(columns, ",")
}

func validateScheduleTaskFields(value schedule.Schedule) error {
	if value.Authorization.Kind == "" || strings.TrimSpace(value.Objective) == "" || value.ModelID == "" ||
		(value.HostID != "" && !domain.ValidHostID(value.HostID)) || !domain.ValidUserID(value.OwnerID) ||
		(value.HostID == "" && (value.Observation != nil || app.ClassifyFileDeliveryIntent(value.Objective, false) != task.FileDeliveryNone)) {
		return domain.InvalidArgument("schedule", "scheduled task fields are invalid")
	}
	if err := value.Budget.Validate(); err != nil {
		return domain.InvalidArgument("budget", err.Error())
	}
	return nil
}

func validateScheduleNotificationEvents(values []schedule.NotificationEvent) error {
	seen := make(map[schedule.NotificationEvent]struct{}, len(values))
	for _, value := range values {
		if !value.Valid() {
			return domain.InvalidArgument("notification_events", "notification event is invalid")
		}
		if _, ok := seen[value]; ok {
			return domain.InvalidArgument("notification_events", "notification events must be unique")
		}
		seen[value] = struct{}{}
	}
	return nil
}

func notificationEventStrings(values []schedule.NotificationEvent) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, string(value))
	}
	return result
}

func scheduleNotificationEvents(values []string) []schedule.NotificationEvent {
	result := make([]schedule.NotificationEvent, 0, len(values))
	for _, value := range values {
		result = append(result, schedule.NotificationEvent(value))
	}
	return result
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func sameTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func truncateScheduleTitle(value string) string {
	runes := []rune(value)
	if len(runes) > 120 {
		runes = runes[:120]
	}
	return string(runes)
}

func scheduleVersionConflictError(expected, actual int64) error {
	conflict := domain.NewError(domain.CodeConflict, "schedule version changed")
	conflict.Details["expected_version"] = expected
	conflict.Details["actual_version"] = actual
	return conflict
}

var _ app.ScheduleStore = (*Store)(nil)
