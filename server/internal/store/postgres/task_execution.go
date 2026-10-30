package postgres

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/execution"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func validPendingText(value string) bool {
	return strings.TrimSpace(value) != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= 512
}

func validClarificationText(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.ContainsRune(value, '\x00') && utf8.ValidString(value) && utf8.RuneCountInString(value) <= 16384
}

func validClarificationQuestion(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.ContainsRune(value, '\x00') && utf8.ValidString(value) && utf8.RuneCountInString(value) <= 4096
}

func (s *Store) withTaskSnapshot(ctx context.Context, fn func(pgx.Tx) error) error {
	return withTx(ctx, func(ctx context.Context) (pgx.Tx, error) {
		return s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	}, fn)
}

func (s *Store) GetTask(ctx context.Context, id, ownerID string) (app.TaskDetails, error) {
	var result app.TaskDetails
	err := s.withTaskSnapshot(ctx, func(tx pgx.Tx) error {
		queries := sqlcgen.New(tx)
		row, err := queries.GetTask(ctx, sqlcgen.GetTaskParams{ID: id, OwnerID: ownerID})
		if err != nil {
			return storeError("get task", "task", err)
		}
		var observationJSON []byte
		err = tx.QueryRow(ctx, `SELECT observation_spec FROM schedule_runs WHERE task_id=$1 AND observation_spec IS NOT NULL`, id).Scan(&observationJSON)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read scheduled task observation: %w", err)
		}
		var scheduleObservation *schedule.ObservationSpec
		if err == nil {
			var observation schedule.ObservationSpec
			if json.Unmarshal(observationJSON, &observation) != nil || observation.Validate() != nil {
				return domain.NewError(domain.CodeInvalidState, "scheduled task observation is invalid")
			}
			scheduleObservation = &observation
		}
		steps, err := queries.ListTaskSteps(ctx, id)
		if err != nil {
			return fmt.Errorf("list task steps: %w", err)
		}
		items, err := queries.ListTaskBatchItems(ctx, id)
		if err != nil {
			return fmt.Errorf("list task batch items: %w", err)
		}
		artifacts, err := queries.ListTaskArtifacts(ctx, id)
		if err != nil {
			return fmt.Errorf("list task artifacts: %w", err)
		}
		var pending task.PendingRequest
		var requestHash []byte
		var respondedAt pgtype.Timestamptz
		err = tx.QueryRow(ctx, `SELECT id, task_id, version, kind, title, object, content, impact, call_id, tool_name,
request_hash, runtime_generation, runtime_instance_id, control_version, status, decision, created_at, responded_at
FROM task_pending_requests WHERE task_id=$1 ORDER BY version DESC LIMIT 1`, id).Scan(
			&pending.ID, &pending.TaskID, &pending.Version, &pending.Kind, &pending.Title, &pending.Object, &pending.Content,
			&pending.Impact, &pending.CallID, &pending.ToolName, &requestHash, &pending.RuntimeGeneration,
			&pending.RuntimeInstanceID, &pending.ControlVersion, &pending.Status, &pending.Decision, &pending.CreatedAt, &respondedAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read latest task pending request: %w", err)
		}
		var pendingRequest *task.PendingRequest
		if err == nil {
			pending.RequestHash = append([]byte(nil), requestHash...)
			if respondedAt.Valid {
				responded := respondedAt.Time
				pending.RespondedAt = &responded
			}
			pendingRequest = &pending
		}
		clarificationRows, err := tx.Query(ctx, `SELECT id, task_id, version, model_step_id, call_id, question, status, answer, created_at, responded_at
FROM task_clarification_requests WHERE task_id=$1 ORDER BY version`, id)
		if err != nil {
			return fmt.Errorf("list task clarification requests: %w", err)
		}
		clarifications := make([]task.ClarificationRequest, 0)
		for clarificationRows.Next() {
			var clarification task.ClarificationRequest
			var respondedAt pgtype.Timestamptz
			if err := clarificationRows.Scan(&clarification.ID, &clarification.TaskID, &clarification.Version,
				&clarification.ModelStepID, &clarification.CallID, &clarification.Question, &clarification.Status, &clarification.Answer,
				&clarification.CreatedAt, &respondedAt); err != nil {
				clarificationRows.Close()
				return fmt.Errorf("scan task clarification request: %w", err)
			}
			if respondedAt.Valid {
				responded := respondedAt.Time
				clarification.RespondedAt = &responded
			}
			clarifications = append(clarifications, clarification)
		}
		if err := clarificationRows.Err(); err != nil {
			clarificationRows.Close()
			return fmt.Errorf("iterate task clarification requests: %w", err)
		}
		clarificationRows.Close()
		stopRequest, stopOperationIDs, err := loadTaskStopState(ctx, tx, id)
		if err != nil {
			return err
		}
		result = app.TaskDetails{
			Task: rowTaskDomain(row), ScheduleObservation: scheduleObservation, Steps: mapRows(steps, taskStepDomain),
			BatchItems: mapRows(items, taskBatchItemDomain), Artifacts: mapRows(artifacts, taskArtifactDomain),
			PendingRequest: pendingRequest, ClarificationRequests: clarifications,
			StopRequest: stopRequest, StopOperationIDs: stopOperationIDs,
		}
		return nil
	})
	return result, err
}

func (s *Store) ListTasks(ctx context.Context, ownerID string, filter app.TaskListRequest) (app.TaskPage, error) {
	limit, cursorAt, cursorID, err := pagination(filter.Page.Limit, filter.Page.Cursor)
	if err != nil {
		return app.TaskPage{}, err
	}
	for _, hostID := range filter.HostIDs {
		if !domain.ValidHostID(hostID) {
			return app.TaskPage{}, domain.InvalidArgument("host_ids", "host id is invalid")
		}
	}
	var status string
	if filter.Status != "" {
		if !validTaskStatus(filter.Status) {
			return app.TaskPage{}, domain.InvalidArgument("status", "task status is invalid")
		}
		status = string(filter.Status)
	}
	rows, err := s.q.ListTasks(ctx, sqlcgen.ListTasksParams{
		OwnerID: ownerID, Column2: filter.HostIDs, Column3: status, Column4: cursorAt, ID: cursorID, Limit: limit + 1,
	})
	if err != nil {
		return app.TaskPage{}, fmt.Errorf("list tasks: %w", err)
	}
	items := mapRows(rows, rowTaskDomain)
	page := app.TaskPage{Items: items}
	page.NextCursor = nextPageCursor(items, limit, func(value task.Task) (string, time.Time) { return value.ID, value.CreatedAt })
	if int32(len(items)) > limit {
		page.Items = items[:limit]
	}
	return page, nil
}

func (s *Store) ListTaskEvents(ctx context.Context, ownerID, taskID string, after int64, limit int32) ([]task.Event, error) {
	page, err := s.taskEventPage(ctx, ownerID, taskID, after, limit)
	return page.Items, err
}

func (s *Store) GetTaskEventPage(ctx context.Context, ownerID, taskID string, after int64, limit int32) (app.TaskEventPage, error) {
	return s.taskEventPage(ctx, ownerID, taskID, after, limit)
}

func (s *Store) taskEventPage(ctx context.Context, ownerID, taskID string, after int64, limit int32) (app.TaskEventPage, error) {
	if after < 0 {
		return app.TaskEventPage{}, domain.InvalidArgument("after", "event cursor must be non-negative")
	}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 {
		return app.TaskEventPage{}, domain.InvalidArgument("limit", "event limit is out of range")
	}
	var result app.TaskEventPage
	err := s.withTaskSnapshot(ctx, func(tx pgx.Tx) error {
		var current int64
		var status string
		err := tx.QueryRow(ctx, `SELECT COALESCE(host_id, ''), status, event_seq FROM tasks WHERE id=$1 AND owner_id=$2`, taskID, ownerID).
			Scan(&result.HostID, &status, &current)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("read task event state: %w", err)
		}
		result.Status = task.Status(status)
		result.CurrentSequence = current
		if after > current {
			return domain.InvalidArgument("after", "event cursor is ahead of the task")
		}
		var first int64
		err = tx.QueryRow(ctx, `SELECT COALESCE(min(sequence),0) FROM task_events WHERE task_id=$1`, taskID).Scan(&first)
		if err != nil {
			return fmt.Errorf("read earliest task event: %w", err)
		}
		if current > after && (first == 0 || after < first-1) {
			return domain.NewError(domain.CodeEventCursorExpired, "event cursor has expired")
		}
		rows, err := sqlcgen.New(tx).ListTaskEvents(ctx, sqlcgen.ListTaskEventsParams{TaskID: taskID, Sequence: after, Limit: limit})
		if err != nil {
			return fmt.Errorf("list task events: %w", err)
		}
		result.Items = mapRows(rows, taskEventDomain)
		return nil
	})
	return result, err
}

func (s *Store) AppendAssistantTextDelta(ctx context.Context, delta app.AssistantTextDelta) (task.Event, error) {
	if !domain.ValidTaskID(delta.TaskID) || delta.LeaseOwner == "" || len(delta.LeaseOwner) > 255 || delta.LeaseVersion < 1 ||
		!domain.ValidMessageID(delta.MessageID) || !domain.ValidStepID(delta.TurnID) || delta.Delta == "" ||
		len(delta.Delta) > app.MaxAssistantMessageBytes || !utf8.ValidString(delta.Delta) || strings.ContainsRune(delta.Delta, '\x00') {
		return task.Event{}, domain.InvalidArgument("text_delta", "assistant text delta is invalid")
	}
	payload, err := json.Marshal(map[string]string{
		"role": "assistant", "message_id": delta.MessageID, "turn_id": delta.TurnID, "delta": delta.Delta,
	})
	if err != nil {
		return task.Event{}, fmt.Errorf("encode assistant text delta: %w", err)
	}
	var result task.Event
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID string
		err := tx.QueryRow(ctx, `SELECT owner_id FROM tasks WHERE id=$1 AND status='running'
AND lease_owner=$2 AND lease_version=$3 AND lease_expires_at > clock_timestamp() FOR UPDATE`,
			delta.TaskID, delta.LeaseOwner, delta.LeaseVersion).Scan(&ownerID)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if readErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`, delta.TaskID).Scan(&exists); readErr != nil {
				return fmt.Errorf("check task before appending assistant text: %w", readErr)
			}
			if !exists {
				return missingTaskError()
			}
			return taskLeaseConflict()
		}
		if err != nil {
			return fmt.Errorf("lock task before appending assistant text: %w", err)
		}
		var sequence int64
		err = tx.QueryRow(ctx, `UPDATE tasks SET event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND status='running' AND lease_owner=$3 AND lease_version=$4
AND lease_expires_at > clock_timestamp() RETURNING event_seq`,
			delta.TaskID, ownerID, delta.LeaseOwner, delta.LeaseVersion).Scan(&sequence)
		if errors.Is(err, pgx.ErrNoRows) {
			return taskLeaseConflict()
		}
		if err != nil {
			return fmt.Errorf("advance task event sequence for assistant text: %w", err)
		}
		event := task.Event{TaskID: delta.TaskID, Sequence: sequence, Type: "task.message", Payload: payload}
		if err := insertTaskEvent(ctx, tx, event); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT created_at FROM task_events WHERE task_id=$1 AND sequence=$2`, delta.TaskID, sequence).Scan(&event.CreatedAt); err != nil {
			return fmt.Errorf("read assistant text event time: %w", err)
		}
		result = event
		return nil
	})
	return result, err
}

func (s *Store) ClaimNextTask(ctx context.Context, worker string, leaseDuration time.Duration) (task.Task, bool, error) {
	return s.claimNextTask(ctx, worker, leaseDuration, 0)
}

// ClaimNextTaskWithOwnerLimit reserves one of the account's active execution
// slots in the same transaction as the Task lease. The owner gate serializes
// claims across Store instances and Portal processes.
func (s *Store) ClaimNextTaskWithOwnerLimit(ctx context.Context, worker string, leaseDuration time.Duration, maxActivePerOwner int) (task.Task, bool, error) {
	if maxActivePerOwner < 1 {
		return task.Task{}, false, domain.InvalidArgument("owner_task_limit", "a positive per-owner task limit is required")
	}
	return s.claimNextTask(ctx, worker, leaseDuration, maxActivePerOwner)
}

func (s *Store) claimNextTask(ctx context.Context, worker string, leaseDuration time.Duration, maxActivePerOwner int) (task.Task, bool, error) {
	if worker == "" || len(worker) > 255 || leaseDuration <= 0 {
		return task.Task{}, false, domain.InvalidArgument("lease", "worker and positive lease duration are required")
	}
	leaseMillis := leaseDuration.Milliseconds()
	if leaseMillis < 1 {
		leaseMillis = 1
	}
	var claimed task.Task
	var found bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID string
		var taskEpoch int64
		err := tx.QueryRow(ctx, `
SELECT t.owner_id, t.execution_epoch FROM tasks AS t
WHERE (t.status='queued' OR (t.status='running' AND t.lease_expires_at <= clock_timestamp()))
  AND t.execution_location='server'
  AND ($1::integer = 0 OR (
    SELECT count(*) FROM tasks AS active_task
    WHERE active_task.owner_id=t.owner_id AND active_task.status='running'
      AND active_task.id<>t.id
      AND (active_task.lease_expires_at > clock_timestamp() OR EXISTS (
        SELECT 1 FROM task_agent_instances AS agent
        WHERE agent.task_id=active_task.id AND agent.state='active'
      ))
  ) < $1)
  AND EXISTS (
    SELECT 1 FROM owner_execution_controls AS ec
    WHERE ec.owner_id=t.owner_id AND ec.state='enabled' AND ec.epoch=t.execution_epoch
  )
  AND (t.host_id IS NULL OR EXISTS (
    SELECT 1 FROM hosts AS h
    WHERE h.id=t.host_id AND h.owner_id=t.owner_id
      AND h.state<>'deleted' AND h.deleted_at IS NULL
  ))
ORDER BY t.created_at, t.id
LIMIT 1`, maxActivePerOwner).Scan(&ownerID, &taskEpoch)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim next task: %w", err)
		}
		var gate executionGate
		if maxActivePerOwner > 0 {
			gate, err = lockOwnerExecutionControl(ctx, tx, ownerID, true)
			if err != nil {
				return err
			}
			if gate.state != execution.StateEnabled || gate.epoch != taskEpoch {
				return nil
			}
		} else {
			gate, err = requireOwnerExecutionEnabled(ctx, tx, ownerID, &taskEpoch)
			if err != nil {
				if domain.CodeOf(err) == domain.CodeConflict {
					return nil
				}
				return err
			}
		}
		var id string
		var hostID string
		var oldVersion int64
		err = tx.QueryRow(ctx, `
SELECT t.id, COALESCE(t.host_id,''), t.lease_version FROM tasks AS t
WHERE t.owner_id=$1 AND t.execution_epoch=$2
  AND (t.status='queued' OR (t.status='running' AND t.lease_expires_at <= clock_timestamp()))
  AND t.execution_location='server'
  AND EXISTS (
    SELECT 1 FROM owner_execution_controls AS ec
    WHERE ec.owner_id=t.owner_id AND ec.state='enabled' AND ec.epoch=t.execution_epoch
  )
  AND (t.host_id IS NULL OR EXISTS (
    SELECT 1 FROM hosts AS h
    WHERE h.id=t.host_id AND h.owner_id=t.owner_id
      AND h.state<>'deleted' AND h.deleted_at IS NULL
  ))
ORDER BY t.created_at, t.id
LIMIT 1 FOR UPDATE SKIP LOCKED`, ownerID, gate.epoch).Scan(&id, &hostID, &oldVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("select and lock candidate task for claim: %w", err)
		}
		if maxActivePerOwner > 0 {
			var active int64
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks AS active_task WHERE owner_id=$1 AND id<>$2 AND status='running'
AND (lease_expires_at > clock_timestamp() OR EXISTS (
  SELECT 1 FROM task_agent_instances AS agent WHERE agent.task_id=active_task.id AND agent.state='active'
))`, ownerID, id).Scan(&active); err != nil {
				return fmt.Errorf("count active owner tasks before claim: %w", err)
			}
			if active >= int64(maxActivePerOwner) {
				return nil
			}
		}
		if hostID != "" {
			if err := lockTaskTargetHost(ctx, tx, hostID, ownerID); err != nil {
				if domain.CodeOf(err) == domain.CodeNotFound {
					return nil
				}
				return err
			}
		}
		taskRow, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: id, OwnerID: ownerID})
		if err != nil {
			return fmt.Errorf("read task authorization before claim: %w", err)
		}
		authorized, err := requireCurrentTaskAuthorization(ctx, tx, taskRow)
		if err != nil {
			return err
		}
		if !authorized {
			if err := requestTaskCancellationForAuthorization(ctx, tx, id, ownerID); err != nil {
				return err
			}
			return nil
		}
		found = true
		if oldVersion == int64(^uint64(0)>>1) {
			return domain.NewError(domain.CodeInvalidState, "task lease version is exhausted")
		}
		newVersion := oldVersion + 1
		if _, err := tx.Exec(ctx, `UPDATE task_operations SET status='unknown', lease_version=$2, updated_at=clock_timestamp() WHERE task_id=$1 AND status='dispatched'`, id, newVersion); err != nil {
			return fmt.Errorf("mark prior dispatched operations unknown: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE task_operations SET lease_version=$2, updated_at=clock_timestamp() WHERE task_id=$1 AND status='unknown'`, id, newVersion); err != nil {
			return fmt.Errorf("adopt unknown operations: %w", err)
		}
		var operationKeys []string
		rows, err := tx.Query(ctx, `SELECT operation_key FROM task_operations WHERE task_id=$1 AND status='unknown' AND lease_version=$2 ORDER BY operation_key`, id, newVersion)
		if err != nil {
			return fmt.Errorf("list outstanding unknown operations: %w", err)
		}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return fmt.Errorf("read recovered operation: %w", err)
			}
			operationKeys = append(operationKeys, key)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read recovered operations: %w", err)
		}
		rows.Close()
		var eventSequence int64
		err = tx.QueryRow(ctx, `UPDATE tasks SET status='running', lease_owner=$2, lease_version=$3,
lease_expires_at=clock_timestamp()+($4::bigint * interval '1 millisecond'),
runtime_lease_safe_after=GREATEST(COALESCE(runtime_lease_safe_after, '-infinity'::timestamptz), clock_timestamp()+($4::bigint * interval '1 millisecond')),
updated_at=clock_timestamp(),
event_seq=event_seq+1
WHERE id=$1 RETURNING event_seq`, id, worker, newVersion, leaseMillis).Scan(&eventSequence)
		if err != nil {
			return fmt.Errorf("set task lease: %w", err)
		}
		statusPayload, err := json.Marshal(map[string]any{"status": string(task.StatusRunning), "lease_version": newVersion})
		if err != nil {
			return fmt.Errorf("encode task claim event: %w", err)
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: id, Sequence: eventSequence, Type: "task.status", Payload: statusPayload}); err != nil {
			return err
		}
		if len(operationKeys) > 0 {
			payload, err := json.Marshal(map[string]any{"status": "unknown", "operation_keys": operationKeys, "requires_reconciliation": true})
			if err != nil {
				return fmt.Errorf("encode operation recovery event: %w", err)
			}
			eventSequence, err = incrementTaskEventSequence(ctx, tx, id)
			if err != nil {
				return err
			}
			if err := insertTaskEvent(ctx, tx, task.Event{
				TaskID: id, Sequence: eventSequence, Type: "task.operation", Payload: payload,
			}); err != nil {
				return err
			}
		}
		row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: id, OwnerID: ownerID})
		if err != nil {
			return fmt.Errorf("read claimed task: %w", err)
		}
		claimed = rowTaskDomain(row)
		return nil
	})
	return claimed, found, err
}

func (s *Store) RenewTaskLease(ctx context.Context, taskID, worker string, version int64, leaseDuration time.Duration) (task.Task, error) {
	if taskID == "" || worker == "" || version < 1 || leaseDuration <= 0 {
		return task.Task{}, domain.InvalidArgument("lease", "task, worker, version and positive lease duration are required")
	}
	leaseMillis := leaseDuration.Milliseconds()
	if leaseMillis < 1 {
		leaseMillis = 1
	}
	var row sqlcgen.Task
	var authorizationDenied bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID string
		if err := tx.QueryRow(ctx, `SELECT owner_id FROM tasks WHERE id=$1`, taskID).Scan(&ownerID); errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		} else if err != nil {
			return fmt.Errorf("read task owner before lease renewal: %w", err)
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, ownerID, nil)
		if err != nil {
			return err
		}
		taskRow, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: taskID, OwnerID: ownerID})
		if err != nil {
			return fmt.Errorf("read task authorization before lease renewal: %w", err)
		}
		authorized, err := requireCurrentTaskAuthorization(ctx, tx, taskRow)
		if err != nil {
			return err
		}
		if !authorized {
			if err := requestTaskCancellationForAuthorization(ctx, tx, taskID, ownerID); err != nil {
				return err
			}
			authorizationDenied = true
			return nil
		}
		var id string
		err = tx.QueryRow(ctx, `UPDATE tasks SET
lease_expires_at=clock_timestamp()+($4::bigint * interval '1 millisecond'),
runtime_lease_safe_after=GREATEST(COALESCE(runtime_lease_safe_after, '-infinity'::timestamptz), clock_timestamp()+($4::bigint * interval '1 millisecond')),
updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$5 AND execution_epoch=$6 AND status='running' AND lease_owner=$2 AND lease_version=$3 AND lease_expires_at > clock_timestamp()
RETURNING id, owner_id`, taskID, worker, version, leaseMillis, ownerID, gate.epoch).Scan(&id, &row.OwnerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return taskLeaseConflict()
		}
		if err != nil {
			return fmt.Errorf("renew task lease: %w", err)
		}
		row, err = sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: id, OwnerID: row.OwnerID})
		if err != nil {
			return fmt.Errorf("read renewed task: %w", err)
		}
		return nil
	})
	if err == nil && authorizationDenied {
		return task.Task{}, taskLeaseConflict()
	}
	if err != nil {
		if domain.CodeOf(err) == domain.CodeInvalidState {
			var exists bool
			if readErr := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`, taskID).Scan(&exists); readErr == nil && !exists {
				return task.Task{}, missingTaskError()
			}
		}
		return task.Task{}, err
	}
	return rowTaskDomain(row), nil
}

func requireCurrentTaskAuthorization(ctx context.Context, tx pgx.Tx, value sqlcgen.Task) (bool, error) {
	if value.AuthTokenID == "" || value.AuthVersion < 1 {
		return false, nil
	}
	var approved bool
	err := tx.QueryRow(ctx, `SELECT status='approved' FROM users WHERE id=$1 FOR SHARE`, value.OwnerID).Scan(&approved)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !approved {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check task authorization account: %w", err)
	}
	var kind string
	var installationID pgtype.Text
	var expiresAt time.Time
	var revokedAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `SELECT kind, client_installation_id, expires_at, revoked_at
FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`, value.AuthTokenID, value.OwnerID).
		Scan(&kind, &installationID, &expiresAt, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check current task authorization token: %w", err)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return false, fmt.Errorf("read database time for task authorization: %w", err)
	}
	if value.AuthExpiresAt.Valid && !value.AuthExpiresAt.Time.After(now) || kind != value.AuthKind || !expiresAt.After(now) ||
		kind == string(identity.KindClient) && !installationID.Valid ||
		installationID.Valid != (value.AuthClientInstallationID.String != "") ||
		installationID.Valid && installationID.String != value.AuthClientInstallationID.String {
		return false, nil
	}
	if revokedAt.Valid && (!installationID.Valid || !value.CreatedAt.Time.Before(revokedAt.Time)) {
		return false, nil
	}
	if installationID.Valid {
		var expiresAt pgtype.Timestamptz
		var revokedAt pgtype.Timestamptz
		err := tx.QueryRow(ctx, `SELECT expires_at, revoked_at FROM client_installations
WHERE id=$1 AND owner_id=$2 FOR SHARE`, installationID.String, value.OwnerID).Scan(&expiresAt, &revokedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("check current task client installation: %w", err)
		}
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return false, fmt.Errorf("read database time for client installation authorization: %w", err)
		}
		if revokedAt.Valid || !expiresAt.Valid || !expiresAt.Time.After(now) {
			return false, nil
		}
	}
	return true, nil
}

func requestTaskCancellationForAuthorization(ctx context.Context, tx pgx.Tx, taskID, ownerID string) error {
	var status task.Status
	var leaseVersion int64
	var safeAfter time.Time
	err := tx.QueryRow(ctx, `SELECT status, lease_version,
GREATEST(COALESCE(runtime_lease_safe_after, '-infinity'::timestamptz),
         COALESCE(lease_expires_at, '-infinity'::timestamptz),
         COALESCE((SELECT max(safe_after) FROM task_stop_requests WHERE task_id=$1 AND status='pending'), '-infinity'::timestamptz),
         clock_timestamp())
FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, taskID, ownerID).Scan(&status, &leaseVersion, &safeAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		return missingTaskError()
	}
	if err != nil {
		return fmt.Errorf("lock task for authorization cancellation: %w", err)
	}
	if status == task.StatusCompleted || status == task.StatusFailed || status == task.StatusCancelled {
		return nil
	}
	var pendingCancelID string
	err = tx.QueryRow(ctx, `SELECT id FROM task_stop_requests WHERE task_id=$1 AND action='cancel' AND status='pending' FOR UPDATE`, taskID).Scan(&pendingCancelID)
	if err == nil && status == task.StatusCancelling {
		return cancelPendingTaskRequest(ctx, tx, taskID)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("find pending task authorization cancellation: %w", err)
	}
	if leaseVersion == int64(^uint64(0)>>1) {
		return domain.NewError(domain.CodeInvalidState, "task lease version is exhausted")
	}
	if _, err := tx.Exec(ctx, `UPDATE task_stop_requests SET status='completed', lease_owner='', lease_expires_at=NULL,
completed_at=clock_timestamp(), updated_at=clock_timestamp()
WHERE task_id=$1 AND action='pause' AND status='pending'`, taskID); err != nil {
		return fmt.Errorf("replace task pause with authorization cancellation: %w", err)
	}
	if err := cancelPendingTaskRequest(ctx, tx, taskID); err != nil {
		return err
	}
	requestID, err := domain.NewTaskActionID()
	if err != nil {
		return fmt.Errorf("generate task authorization stop id: %w", err)
	}
	var sequence int64
	err = tx.QueryRow(ctx, `UPDATE tasks SET status='cancelling', lease_version=lease_version+1,
lease_owner='', lease_expires_at=NULL, waiting_reason='', finished_at=NULL, event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 RETURNING event_seq`, taskID, ownerID).Scan(&sequence)
	if err != nil {
		return fmt.Errorf("fence task for authorization cancellation: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_stop_requests (id, task_id, action, status, safe_after)
VALUES ($1,$2,'cancel','pending',$3)`, requestID, taskID, safeAfter); err != nil {
		return fmt.Errorf("persist task authorization cancellation: %w", err)
	}
	return insertTaskStatusEvent(ctx, tx, taskID, sequence, task.StatusCancelling, task.ActionCancel, requestID)
}

func stopTasksForAuthorization(ctx context.Context, tx pgx.Tx, ownerID, tokenID, installationID string) (int, error) {
	rows, err := tx.Query(ctx, `SELECT t.id FROM tasks t
WHERE t.owner_id=$1 AND t.status IN ('queued','running','waiting_user','pausing','paused','cancelling')
  AND (($2::text <> '' AND t.auth_token_id=$2) OR ($3::text <> '' AND t.auth_client_installation_id=$3))
ORDER BY t.id FOR UPDATE OF t`, ownerID, tokenID, installationID)
	if err != nil {
		return 0, fmt.Errorf("lock tasks for authorization revocation: %w", err)
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("read task for authorization revocation: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate tasks for authorization revocation: %w", err)
	}
	rows.Close()
	for _, id := range ids {
		if err := requestTaskCancellationForAuthorization(ctx, tx, id, ownerID); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

func (s *Store) CommitTaskProgress(ctx context.Context, progress app.TaskProgress) (task.Event, error) {
	if progress.TaskID == "" || progress.LeaseOwner == "" || progress.LeaseVersion < 1 || !validTaskStatus(progress.Status) {
		return task.Event{}, domain.InvalidArgument("progress", "task progress fields are invalid")
	}
	if progress.Usage.ActiveSeconds < 0 || progress.Usage.ToolCalls < 0 ||
		(progress.Usage.ModelTokens != nil && *progress.Usage.ModelTokens < 0) {
		return task.Event{}, domain.InvalidArgument("usage", "task usage must be non-negative")
	}
	if strings.TrimSpace(progress.Event.Type) == "" {
		return task.Event{}, domain.InvalidArgument("event.type", "task event type is required")
	}
	if len(progress.Artifacts) > 0 && (progress.Status != task.StatusCompleted || progress.Event.Type != "task.result") {
		return task.Event{}, domain.InvalidArgument("artifacts", "task artifacts may only be committed with task completion")
	}
	if pending := progress.PendingRequest; pending != nil {
		if !domain.ValidPendingRequestID(pending.ID) || pending.TaskID != progress.TaskID || pending.Version < 1 || pending.Kind != "approval" ||
			pending.Status != task.PendingRequestPending || pending.Decision != "" || len(pending.RequestHash) != 32 ||
			pending.RuntimeGeneration < 0 || pending.RuntimeInstanceID == "" || pending.ControlVersion < 0 || pending.CallID == "" || len(pending.CallID) > 256 ||
			pending.ToolName == "" || len(pending.ToolName) > 128 || !validPendingText(pending.Title) || !validPendingText(pending.Object) ||
			!validPendingText(pending.Content) || !validPendingText(pending.Impact) || progress.Status != task.StatusWaitingUser || progress.WaitingReason != "approval_required" {
			return task.Event{}, domain.InvalidArgument("pending_request", "pending approval request fields are invalid")
		}
	}
	if clarification := progress.ClarificationRequest; clarification != nil {
		if progress.PendingRequest != nil || !domain.ValidPendingRequestID(clarification.ID) || clarification.TaskID != progress.TaskID ||
			clarification.Version < 1 || !domain.ValidStepID(clarification.ModelStepID) || clarification.CallID == "" || len(clarification.CallID) > 256 ||
			!validClarificationQuestion(clarification.Question) || clarification.Status != task.ClarificationPending ||
			clarification.Answer != "" || clarification.RespondedAt != nil || progress.Status != task.StatusWaitingUser ||
			progress.WaitingReason != "clarification_required" {
			return task.Event{}, domain.InvalidArgument("clarification_request", "task clarification request fields are invalid")
		}
	}
	eventPayload, err := validateInitialTaskEvent(progress.Event.Type, progress.Event.Payload)
	if err != nil {
		return task.Event{}, err
	}
	if message := progress.AssistantMessage; message != nil {
		if message.Role != conversation.RoleAssistant || message.TaskID != progress.TaskID || message.OwnerID == "" ||
			message.ConversationID == "" || progress.Status != task.StatusRunning || progress.Event.Type != "task.step" ||
			len(message.Content) > app.MaxAssistantMessageBytes || !utf8.ValidString(message.Content) || strings.ContainsRune(message.Content, '\x00') {
			return task.Event{}, domain.InvalidArgument("assistant_message", "assistant message fields are invalid")
		}
		if err := validateMessage(*message); err != nil {
			return task.Event{}, err
		}
	}
	var result task.Event
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if message := progress.AssistantMessage; message != nil {
			// AppendMessage locks the conversation first; use the same order so
			// concurrent user and assistant messages cannot deadlock.
			var locked string
			err := tx.QueryRow(ctx, `SELECT c.id FROM conversations c JOIN tasks t
ON t.conversation_id=c.id AND t.owner_id=c.owner_id
WHERE t.id=$1 AND c.id=$2 AND c.owner_id=$3 FOR UPDATE OF c`,
				progress.TaskID, message.ConversationID, message.OwnerID).Scan(&locked)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.InvalidArgument("assistant_message", "assistant message does not belong to the task")
			}
			if err != nil {
				return fmt.Errorf("lock assistant message conversation: %w", err)
			}
		}
		var current sqlcgen.Task
		err := tx.QueryRow(ctx, `SELECT id, owner_id, conversation_id, host_id, model_id, objective, status, auth_kind, auth_token_id, auth_scopes, auth_client_installation_id,
budget_active_seconds, budget_tool_calls, budget_model_tokens, usage_active_seconds, usage_tool_calls, usage_model_tokens, lease_owner,
lease_version, lease_expires_at, event_seq, current_step, result, waiting_reason, created_at, updated_at, finished_at, application_id,
execution_location, agent_session_id, auth_version, auth_expires_at, workspace_id, input_revision, workspace_checkpoint_revision
FROM tasks WHERE id=$1 FOR UPDATE`, progress.TaskID).Scan(
			&current.ID, &current.OwnerID, &current.ConversationID, &current.HostID, &current.ModelID, &current.Objective, &current.Status,
			&current.AuthKind, &current.AuthTokenID, &current.AuthScopes, &current.AuthClientInstallationID, &current.BudgetActiveSeconds, &current.BudgetToolCalls,
			&current.BudgetModelTokens, &current.UsageActiveSeconds, &current.UsageToolCalls, &current.UsageModelTokens,
			&current.LeaseOwner, &current.LeaseVersion, &current.LeaseExpiresAt, &current.EventSeq, &current.CurrentStep,
			&current.Result, &current.WaitingReason, &current.CreatedAt, &current.UpdatedAt, &current.FinishedAt, &current.ApplicationID,
			&current.ExecutionLocation, &current.AgentSessionID, &current.AuthVersion, &current.AuthExpiresAt, &current.WorkspaceID, &current.InputRevision, &current.WorkspaceCheckpointRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("lock task for progress: %w", err)
		}
		var leaseValid bool
		if err := tx.QueryRow(ctx, `SELECT lease_expires_at > clock_timestamp() FROM tasks WHERE id=$1`, progress.TaskID).Scan(&leaseValid); err != nil {
			return fmt.Errorf("check task lease: %w", err)
		}
		if current.LeaseOwner != progress.LeaseOwner || current.LeaseVersion != progress.LeaseVersion || !leaseValid || current.Status != string(task.StatusRunning) {
			return taskLeaseConflict()
		}
		currentTask := rowTaskDomain(current)
		if skillRead := progress.CloudDSHSkillRead; skillRead != nil {
			if current.ID != skillRead.TaskID || current.OwnerID != skillRead.OwnerID || currentTask.ExecutionLocation != "server" ||
				currentTask.AgentSessionID != skillRead.AgentSessionID ||
				currentTask.Authorization.Version != skillRead.AuthorizationVersion || progress.Status != task.StatusRunning ||
				progress.Event.Type != "task.step" || !validCloudDSHSkillReadCommit(*skillRead) {
				return domain.NewError(domain.CodeInvalidState, "cloud dsh skill authorization or identity changed")
			}
			var authorizationValid bool
			if err := tx.QueryRow(ctx, `SELECT auth_expires_at > clock_timestamp() FROM tasks WHERE id=$1`, progress.TaskID).Scan(&authorizationValid); err != nil {
				return fmt.Errorf("check cloud dsh skill authorization expiry: %w", err)
			}
			if !authorizationValid {
				return domain.NewError(domain.CodeUnauthenticated, "cloud dsh skill authorization expired")
			}
			if err := validateCloudDSHSkillReadEvent(progress.Event.Payload, *skillRead); err != nil {
				return err
			}
			existing, found, err := findCloudDSHSkillReadEvent(ctx, tx, progress.TaskID, skillRead.AgentSessionID, skillRead.CallID)
			if err != nil {
				return err
			}
			if found {
				if existing.Type != "task.step" || validateCloudDSHSkillReadEvent(existing.Payload, *skillRead) != nil {
					return domain.NewError(domain.CodeInvalidState, "cloud dsh skill replay fingerprint changed")
				}
				result = existing
				return nil
			}
		}
		if currentTask.Status != progress.Status && !task.CanTransition(currentTask.Status, progress.Status) {
			conflict := domain.NewError(domain.CodeInvalidState, "task status transition is invalid")
			conflict.Details["status"] = string(currentTask.Status)
			return conflict
		}
		if progress.Usage.ActiveSeconds > currentTask.Budget.ActiveSeconds || progress.Usage.ToolCalls > currentTask.Budget.ToolCalls ||
			(progress.Usage.ModelTokens != nil && *progress.Usage.ModelTokens > currentTask.Budget.ModelTokens) {
			return domain.NewError(domain.CodeQuotaExceeded, "task budget exceeded")
		}
		observationOutcome, observationConfigured, err := commitScheduleObservation(ctx, tx, progress)
		if err != nil {
			return err
		}
		if observationConfigured && progress.Status == task.StatusCompleted && progress.Event.Type == "task.result" {
			eventPayload, err = addObservationOutcomeToTaskResult(eventPayload, observationOutcome)
			if err != nil {
				return err
			}
		}
		stepIDs := make(map[string]struct{}, len(progress.Steps))
		stepSequences := make(map[int64]struct{}, len(progress.Steps))
		for _, step := range progress.Steps {
			if !domain.ValidStepID(step.ID) || (step.TaskID != "" && step.TaskID != progress.TaskID) || step.Sequence < 1 || step.Kind == "" || !validStepStatus(step.Status) {
				return domain.InvalidArgument("steps", "task step fields are invalid")
			}
			if _, exists := stepIDs[step.ID]; exists {
				return domain.InvalidArgument("steps", "task step ids must be unique")
			}
			if _, exists := stepSequences[step.Sequence]; exists {
				return domain.InvalidArgument("steps", "task step sequences must be unique")
			}
			stepIDs[step.ID] = struct{}{}
			stepSequences[step.Sequence] = struct{}{}
			var childSessionStep bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM task_agent_sessions WHERE task_id=$1 AND step_id=$2 AND parent_session_id IS NOT NULL
)`, progress.TaskID, step.ID).Scan(&childSessionStep); err != nil {
				return fmt.Errorf("check child Agent Session TaskStep ownership: %w", err)
			}
			if childSessionStep {
				return domain.NewError(domain.CodeForbidden, "task progress cannot change a child Agent Session step")
			}
			result, err := tx.Exec(ctx, `INSERT INTO task_steps (id, task_id, sequence, kind, status, title, summary) VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (id) DO UPDATE SET status=EXCLUDED.status, title=EXCLUDED.title, summary=EXCLUDED.summary
WHERE task_steps.task_id=EXCLUDED.task_id AND task_steps.sequence=EXCLUDED.sequence`,
				step.ID, progress.TaskID, step.Sequence, step.Kind, string(step.Status), step.Title, step.Summary)
			if err != nil {
				return fmt.Errorf("persist task step: %w", err)
			}
			if result.RowsAffected() == 0 {
				return domain.NewError(domain.CodeInvalidState, "task step identity changed")
			}
		}
		if len(progress.Artifacts) > 0 {
			if err := persistTaskProgressArtifacts(ctx, tx, current.OwnerID, progress.TaskID, progress.Artifacts); err != nil {
				return err
			}
		}
		if progress.Status == task.StatusCompleted {
			if current.WorkspaceID.Valid {
				if !current.WorkspaceCheckpointRevision.Valid {
					return domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace checkpoint is unavailable")
				}
				var revision int64
				err := tx.QueryRow(ctx, `SELECT revision FROM workspaces WHERE id=$1 FOR SHARE`, current.WorkspaceID.String).Scan(&revision)
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.NewError(domain.CodeWorkspaceConflict, "Workspace was removed after this task input was confirmed")
				}
				if err != nil {
					return fmt.Errorf("read Workspace before task completion: %w", err)
				}
				if revision != current.WorkspaceCheckpointRevision.Int64 {
					return domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed after this task checkpoint was confirmed")
				}
			}
			var unsettledChild bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM task_agent_sessions s JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.parent_session_id IS NOT NULL AND st.status IN ('queued','running','waiting_user','unknown')
)`, progress.TaskID).Scan(&unsettledChild); err != nil {
				return fmt.Errorf("check child Agent Sessions before task completion: %w", err)
			}
			if unsettledChild {
				return domain.NewError(domain.CodeInvalidState, "task has unsettled child Agent Sessions")
			}
		}
		if progress.Status == task.StatusCompleted {
			if err := validateCompletedTaskArtifacts(ctx, tx, current.OwnerID, progress.TaskID); err != nil {
				return err
			}
		}
		finished := progress.Status == task.StatusCompleted || progress.Status == task.StatusFailed || progress.Status == task.StatusCancelled
		var sequence int64
		err = tx.QueryRow(ctx, `UPDATE tasks SET status=$4, current_step=$5, result=$6, waiting_reason=$7,
usage_active_seconds=$8, usage_tool_calls=$9,
usage_model_tokens=CASE WHEN execution_location='server' THEN usage_model_tokens ELSE $10 END,
lease_owner=CASE WHEN $4='running' THEN lease_owner ELSE '' END,
lease_expires_at=CASE WHEN $4='running' THEN lease_expires_at ELSE NULL END,
finished_at=CASE WHEN $11 THEN clock_timestamp() WHEN $4='running' THEN NULL ELSE finished_at END,
event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 AND lease_owner=$2 AND lease_version=$3 RETURNING event_seq`,
			progress.TaskID, progress.LeaseOwner, progress.LeaseVersion, string(progress.Status), progress.CurrentStep,
			progress.Result, progress.WaitingReason, progress.Usage.ActiveSeconds, progress.Usage.ToolCalls,
			taskUsageTokens(progress.Usage.ModelTokens), finished).Scan(&sequence)
		if err != nil {
			return fmt.Errorf("commit task progress: %w", err)
		}
		if pending := progress.PendingRequest; pending != nil {
			_, err = tx.Exec(ctx, `INSERT INTO task_pending_requests
(id, task_id, version, kind, title, object, content, impact, call_id, tool_name, request_hash,
 runtime_generation, runtime_instance_id, control_version, status)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'pending')`,
				pending.ID, pending.TaskID, pending.Version, pending.Kind, pending.Title, pending.Object, pending.Content, pending.Impact,
				pending.CallID, pending.ToolName, pending.RequestHash, pending.RuntimeGeneration, pending.RuntimeInstanceID, pending.ControlVersion)
			if err != nil {
				return fmt.Errorf("persist task pending request: %w", err)
			}
		}
		if clarification := progress.ClarificationRequest; clarification != nil {
			_, err = tx.Exec(ctx, `INSERT INTO task_clarification_requests
(id, task_id, version, model_step_id, call_id, question, status)
VALUES ($1,$2,$3,$4,$5,$6,'pending')`, clarification.ID, clarification.TaskID, clarification.Version,
				clarification.ModelStepID, clarification.CallID, clarification.Question)
			if err != nil {
				return fmt.Errorf("persist task clarification request: %w", err)
			}
		}
		progress.Event.TaskID = progress.TaskID
		progress.Event.Sequence = sequence
		progress.Event.Payload = eventPayload
		if err := insertTaskEvent(ctx, tx, progress.Event); err != nil {
			return err
		}
		if message := progress.AssistantMessage; message != nil {
			if message.OwnerID != current.OwnerID || message.ConversationID != current.ConversationID {
				return domain.InvalidArgument("assistant_message", "assistant message does not belong to the task")
			}
			if err := insertMessage(ctx, tx, *message); err != nil {
				return err
			}
			messagePayload, err := json.Marshal(map[string]any{
				"role": "assistant", "message_id": message.ID, "turn_id": progress.CurrentStep,
				"content": message.Content, "done": true,
			})
			if err != nil {
				return fmt.Errorf("encode completed assistant message event: %w", err)
			}
			var messageSequence int64
			err = tx.QueryRow(ctx, `UPDATE tasks SET event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND status='running' AND lease_owner=$3 AND lease_version=$4
AND lease_expires_at > clock_timestamp() RETURNING event_seq`,
				progress.TaskID, current.OwnerID, progress.LeaseOwner, progress.LeaseVersion).Scan(&messageSequence)
			if err != nil {
				return fmt.Errorf("advance task event sequence for assistant message: %w", err)
			}
			if err := insertTaskEvent(ctx, tx, task.Event{
				TaskID: progress.TaskID, Sequence: messageSequence, Type: "task.message", Payload: messagePayload,
			}); err != nil {
				return err
			}
		}
		var createdAt time.Time
		if err := tx.QueryRow(ctx, `SELECT created_at FROM task_events WHERE task_id=$1 AND sequence=$2`, progress.TaskID, sequence).Scan(&createdAt); err != nil {
			return fmt.Errorf("read committed task event time: %w", err)
		}
		result = task.Event{TaskID: progress.TaskID, Sequence: sequence, Type: progress.Event.Type, Payload: eventPayload, CreatedAt: createdAt}
		return nil
	})
	return result, err
}

func commitScheduleObservation(ctx context.Context, tx pgx.Tx, progress app.TaskProgress) (schedule.ObservationOutcome, bool, error) {
	if progress.Status != task.StatusCompleted || progress.Event.Type != "task.result" {
		return "", false, nil
	}
	var runID, scheduleID, runConfigHash, currentConfigHash, baseline string
	var observationJSON []byte
	err := tx.QueryRow(ctx, `SELECT r.id,r.schedule_id,r.observation_spec,r.observation_config_hash,s.observation_config_hash,s.baseline_fingerprint
FROM schedule_runs r JOIN schedules s ON s.id=r.schedule_id AND s.owner_id=r.owner_id
WHERE r.task_id=$1 FOR UPDATE OF r,s`, progress.TaskID).
		Scan(&runID, &scheduleID, &observationJSON, &runConfigHash, &currentConfigHash, &baseline)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("lock scheduled observation state: %w", err)
	}
	if len(observationJSON) == 0 {
		return "", false, nil
	}
	var observation schedule.ObservationSpec
	if json.Unmarshal(observationJSON, &observation) != nil || observation.Validate() != nil {
		return "", false, domain.NewError(domain.CodeInvalidState, "scheduled task observation is invalid")
	}
	outcome := schedule.ObservationUnknown
	fingerprint := ""
	if validScheduleObservationFingerprint(progress.ObservationFingerprint) {
		fingerprint = progress.ObservationFingerprint
		if runConfigHash != "" && runConfigHash == currentConfigHash && runConfigHash == observation.ConfigHash() {
			switch baseline {
			case "":
				outcome = schedule.ObservationBaseline
			case fingerprint:
				outcome = schedule.ObservationNoChange
			default:
				outcome = schedule.ObservationChanged
			}
			if _, err := tx.Exec(ctx, `UPDATE schedules SET baseline_fingerprint=$2 WHERE id=$1`, scheduleID, fingerprint); err != nil {
				return "", false, fmt.Errorf("advance scheduled observation baseline: %w", err)
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE schedule_runs SET observation_outcome=$2,observation_fingerprint=$3,updated_at=clock_timestamp() WHERE id=$1`,
		runID, string(outcome), fingerprint); err != nil {
		return "", false, fmt.Errorf("record scheduled observation outcome: %w", err)
	}
	return outcome, true, nil
}

func validScheduleObservationFingerprint(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func addObservationOutcomeToTaskResult(payload []byte, outcome schedule.ObservationOutcome) ([]byte, error) {
	if !outcome.Valid() {
		return nil, domain.NewError(domain.CodeInvalidState, "scheduled observation outcome is invalid")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, fmt.Errorf("decode task result event for observation outcome: %w", err)
	}
	encodedOutcome, err := json.Marshal(outcome)
	if err != nil {
		return nil, fmt.Errorf("encode scheduled observation outcome: %w", err)
	}
	fields["observation_outcome"] = encodedOutcome
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode task result observation outcome: %w", err)
	}
	return encoded, nil
}

func (s *Store) CreateOperation(ctx context.Context, value task.Operation, leaseOwner string) (task.Operation, bool, error) {
	if value.WorkspaceCheckpointRevision == nil && value.InputRevision != nil {
		checkpoint := *value.InputRevision
		value.WorkspaceCheckpointRevision = &checkpoint
	}
	if !domain.ValidOperationID(value.ID) || !domain.ValidTaskID(value.TaskID) || value.Key == "" || len(value.Key) > 255 ||
		len(value.RequestHash) != 32 || !domain.ValidHostID(value.HostID) || value.LeaseVersion < 1 ||
		value.RuntimeLeaseVersion < 1 || value.RuntimeInstanceID == "" || leaseOwner == "" ||
		!validOperationWorkspaceBinding(value.WorkspaceID, value.BindingVersion) ||
		(value.InputRevision != nil && (value.WorkspaceID == "" || *value.InputRevision < 0)) ||
		(value.WorkspaceID == "") != (value.WorkspaceCheckpointRevision == nil) ||
		(value.WorkspaceID != "" && value.InputRevision == nil) ||
		(value.InputRevision != nil && value.WorkspaceCheckpointRevision != nil && *value.WorkspaceCheckpointRevision < *value.InputRevision) {
		return task.Operation{}, false, domain.InvalidArgument("operation", "operation fields are invalid")
	}
	if value.Status != task.OperationPrepared {
		return task.Operation{}, false, domain.InvalidArgument("operation.status", "new operation must be prepared")
	}
	var result task.Operation
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID string
		if err := tx.QueryRow(ctx, `SELECT owner_id FROM tasks WHERE id=$1`, value.TaskID).Scan(&ownerID); errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		} else if err != nil {
			return fmt.Errorf("read task owner before operation registration: %w", err)
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, ownerID, nil)
		if err != nil {
			return err
		}
		var row sqlcgen.Task
		var taskEpoch int64
		err = tx.QueryRow(ctx, `SELECT id, owner_id, conversation_id, host_id, model_id, objective, status, auth_kind, auth_token_id, auth_scopes, auth_client_installation_id,
budget_active_seconds, budget_tool_calls, budget_model_tokens, usage_active_seconds, usage_tool_calls, usage_model_tokens, lease_owner,
lease_version, lease_expires_at, event_seq, current_step, result, waiting_reason, created_at, updated_at, finished_at, execution_epoch, application_id, workspace_id, input_revision, workspace_checkpoint_revision
FROM tasks WHERE id=$1 FOR UPDATE`, value.TaskID).Scan(
			&row.ID, &row.OwnerID, &row.ConversationID, &row.HostID, &row.ModelID, &row.Objective, &row.Status,
			&row.AuthKind, &row.AuthTokenID, &row.AuthScopes, &row.AuthClientInstallationID, &row.BudgetActiveSeconds, &row.BudgetToolCalls,
			&row.BudgetModelTokens, &row.UsageActiveSeconds, &row.UsageToolCalls, &row.UsageModelTokens,
			&row.LeaseOwner, &row.LeaseVersion, &row.LeaseExpiresAt, &row.EventSeq, &row.CurrentStep,
			&row.Result, &row.WaitingReason, &row.CreatedAt, &row.UpdatedAt, &row.FinishedAt, &taskEpoch, &row.ApplicationID,
			&row.WorkspaceID, &row.InputRevision, &row.WorkspaceCheckpointRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("lock task for operation: %w", err)
		}
		ownerID = row.OwnerID
		if taskEpoch != gate.epoch {
			return ownerExecutionDisabledError(gate)
		}
		if err := requireCurrentLease(ctx, tx, row, leaseOwner, value.LeaseVersion); err != nil {
			return err
		}
		if value.HostID != row.HostID.String {
			return domain.InvalidArgument("operation.host_id", "operation host does not match task host")
		}
		existing, err := loadOperationByKey(ctx, tx, value.TaskID, value.Key)
		if err == nil {
			if !equalOperationHash(existing.RequestHash, value.RequestHash) || existing.HostID != value.HostID ||
				existing.WorkspaceID != value.WorkspaceID || existing.BindingVersion != value.BindingVersion ||
				!sameTaskInputRevision(existing.InputRevision, value.InputRevision) ||
				!sameTaskInputRevision(existing.WorkspaceCheckpointRevision, value.WorkspaceCheckpointRevision) {
				return idempotencyConflict()
			}
			result, replayed = existing, true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read existing task operation: %w", err)
		}
		if err := ensureNoPendingTaskWorkspacePublish(ctx, tx, value.TaskID); err != nil {
			return err
		}
		if err := requireOperationHostFence(ctx, tx, ownerID, value); err != nil {
			return err
		}
		if value.WorkspaceID != "" {
			if err := requireTaskWorkspaceInputAdmission(ctx, tx, row, value); err != nil {
				return err
			}
		}
		if err := requireRunningTaskApplication(ctx, tx, row); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO task_operations (id, task_id, operation_key, request_hash, host_id, workspace_id, binding_version, input_revision, workspace_checkpoint_revision, runtime_generation, runtime_instance_id, control_version, lease_version, runtime_lease_version, status, external_id, result)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'prepared',$15,$16)`, value.ID, value.TaskID, value.Key, value.RequestHash,
			value.HostID, value.WorkspaceID, value.BindingVersion, taskUsageTokens(value.InputRevision), taskUsageTokens(value.WorkspaceCheckpointRevision), value.RuntimeGeneration, value.RuntimeInstanceID,
			value.ControlVersion, value.LeaseVersion, value.RuntimeLeaseVersion, value.ExternalID, value.Result)
		if err != nil {
			return fmt.Errorf("create prepared task operation: %w", err)
		}
		payload, err := json.Marshal(map[string]string{"operation_id": value.ID, "operation_key": value.Key, "status": string(task.OperationPrepared)})
		if err != nil {
			return fmt.Errorf("encode operation event: %w", err)
		}
		sequence, err := incrementTaskEventSequence(ctx, tx, value.TaskID)
		if err != nil {
			return err
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: value.TaskID, Sequence: sequence, Type: "task.operation", Payload: payload}); err != nil {
			return err
		}
		result, err = loadOperationByKey(ctx, tx, value.TaskID, value.Key)
		if err != nil {
			return fmt.Errorf("read created task operation: %w", err)
		}
		return nil
	})
	return result, replayed, err
}

func (s *Store) UpdateOperation(ctx context.Context, update app.OperationUpdate) (task.Operation, error) {
	if !domain.ValidTaskID(update.TaskID) || update.OperationKey == "" || update.LeaseOwner == "" || update.LeaseVersion < 1 || !validOperationStatus(update.Status) {
		return task.Operation{}, domain.InvalidArgument("operation", "operation update fields are invalid")
	}
	var result task.Operation
	var authorizationDenied bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID string
		if err := tx.QueryRow(ctx, `SELECT owner_id FROM tasks WHERE id=$1`, update.TaskID).Scan(&ownerID); errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		} else if err != nil {
			return fmt.Errorf("read task owner before operation update: %w", err)
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, ownerID, nil)
		if err != nil {
			return err
		}
		var row sqlcgen.Task
		var taskEpoch int64
		err = tx.QueryRow(ctx, `SELECT id, owner_id, conversation_id, host_id, model_id, objective, status, auth_kind, auth_token_id, auth_scopes, auth_client_installation_id,
budget_active_seconds, budget_tool_calls, budget_model_tokens, usage_active_seconds, usage_tool_calls, usage_model_tokens, lease_owner,
lease_version, lease_expires_at, event_seq, current_step, result, waiting_reason, created_at, updated_at, finished_at, execution_epoch, application_id,
workspace_id, input_revision, workspace_checkpoint_revision
FROM tasks WHERE id=$1 FOR UPDATE`, update.TaskID).Scan(
			&row.ID, &row.OwnerID, &row.ConversationID, &row.HostID, &row.ModelID, &row.Objective, &row.Status,
			&row.AuthKind, &row.AuthTokenID, &row.AuthScopes, &row.AuthClientInstallationID, &row.BudgetActiveSeconds, &row.BudgetToolCalls,
			&row.BudgetModelTokens, &row.UsageActiveSeconds, &row.UsageToolCalls, &row.UsageModelTokens,
			&row.LeaseOwner, &row.LeaseVersion, &row.LeaseExpiresAt, &row.EventSeq, &row.CurrentStep,
			&row.Result, &row.WaitingReason, &row.CreatedAt, &row.UpdatedAt, &row.FinishedAt, &taskEpoch, &row.ApplicationID,
			&row.WorkspaceID, &row.InputRevision, &row.WorkspaceCheckpointRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("lock task for operation update: %w", err)
		}
		if taskEpoch != gate.epoch {
			return ownerExecutionDisabledError(gate)
		}
		if err := requireCurrentLease(ctx, tx, row, update.LeaseOwner, update.LeaseVersion); err != nil {
			return err
		}
		operation, err := loadOperationByKey(ctx, tx, update.TaskID, update.OperationKey)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "task operation not found")
		}
		if err != nil {
			return fmt.Errorf("read task operation for update: %w", err)
		}
		adoptingPrepared := operation.Status == task.OperationPrepared && update.Status == task.OperationPrepared
		if operation.LeaseVersion != update.LeaseVersion && !adoptingPrepared {
			return taskLeaseConflict()
		}
		dispatching := operation.Status == task.OperationPrepared && update.Status == task.OperationDispatched
		rearming := operation.Status == task.OperationUnknown && update.Status == task.OperationPrepared
		if adoptingPrepared || rearming || update.Status == task.OperationDispatched {
			taskRow, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: update.TaskID, OwnerID: ownerID})
			if err != nil {
				return fmt.Errorf("read task authorization before operation dispatch: %w", err)
			}
			authorized, err := requireCurrentTaskAuthorization(ctx, tx, taskRow)
			if err != nil {
				return err
			}
			if !authorized {
				if err := requestTaskCancellationForAuthorization(ctx, tx, update.TaskID, ownerID); err != nil {
					return err
				}
				authorizationDenied = true
				return nil
			}
		}
		if update.RearmAfterNotFound != rearming || update.AdoptPrepared != adoptingPrepared {
			return domain.NewError(domain.CodeInvalidState, "operation rearm requires an explicit confirmed not-found result")
		}
		if adoptingPrepared {
			if operation.LeaseVersion == update.LeaseVersion || update.RuntimeGeneration < 1 || update.RuntimeInstanceID == "" ||
				update.ControlVersion < 0 || update.RuntimeLeaseVersion != update.LeaseVersion || update.ExternalID != "" || update.Result != "" {
				return domain.InvalidArgument("operation.runtime_fence", "prepared adoption requires the current runtime fence and no result")
			}
			adoptFence := operation
			adoptFence.RuntimeGeneration = update.RuntimeGeneration
			adoptFence.RuntimeInstanceID = update.RuntimeInstanceID
			adoptFence.ControlVersion = update.ControlVersion
			if err := requireOperationHostFence(ctx, tx, row.OwnerID, adoptFence); err != nil {
				return err
			}
		}
		if rearming {
			if update.RuntimeGeneration < 1 || update.RuntimeInstanceID == "" || update.ControlVersion < 0 ||
				update.RuntimeLeaseVersion != update.LeaseVersion || update.ExternalID != "" || update.Result != "" {
				return domain.InvalidArgument("operation.runtime_fence", "rearm requires the current exact runtime fence and a cleared result")
			}
			rearmFence := operation
			rearmFence.RuntimeGeneration = update.RuntimeGeneration
			rearmFence.RuntimeInstanceID = update.RuntimeInstanceID
			rearmFence.ControlVersion = update.ControlVersion
			if err := requireOperationHostFence(ctx, tx, row.OwnerID, rearmFence); err != nil {
				return err
			}
		}
		if update.Status == task.OperationDispatched {
			if update.RuntimeGeneration < 1 || update.RuntimeInstanceID == "" || update.ControlVersion < 0 ||
				update.RuntimeLeaseVersion != update.LeaseVersion {
				return domain.InvalidArgument("operation.runtime_fence", "dispatch requires the current runtime fence")
			}
			dispatchFence := operation
			if dispatching {
				dispatchFence.RuntimeGeneration = update.RuntimeGeneration
				dispatchFence.RuntimeInstanceID = update.RuntimeInstanceID
				dispatchFence.ControlVersion = update.ControlVersion
				dispatchFence.RuntimeLeaseVersion = update.RuntimeLeaseVersion
			} else if operation.RuntimeGeneration != update.RuntimeGeneration || operation.RuntimeInstanceID != update.RuntimeInstanceID ||
				operation.ControlVersion != update.ControlVersion || operation.RuntimeLeaseVersion != update.RuntimeLeaseVersion {
				return domain.NewError(domain.CodeInvalidState, "dispatched runtime fence is immutable")
			}
			if err := requireOperationHostFence(ctx, tx, row.OwnerID, dispatchFence); err != nil {
				return err
			}
			// Prepared operations must recheck the Task snapshot at the side-effect boundary.
			if row.WorkspaceID.Valid {
				if err := requireTaskWorkspaceInputAdmission(ctx, tx, row, operation); err != nil {
					return err
				}
			}
		}
		if adoptingPrepared || rearming || update.Status == task.OperationDispatched {
			if err := requireRunningTaskApplication(ctx, tx, row); err != nil {
				return err
			}
		}
		if operation.Status == update.Status && !adoptingPrepared {
			if operation.ExternalID == update.ExternalID && operation.Result == update.Result {
				result = operation
				return nil
			}
			return domain.NewError(domain.CodeInvalidState, "operation result changed after status was committed")
		}
		if !adoptingPrepared && !task.CanTransitionOperation(operation.Status, update.Status) {
			conflict := domain.NewError(domain.CodeInvalidState, "operation status transition is invalid")
			conflict.Details["status"] = string(operation.Status)
			return conflict
		}
		if adoptingPrepared {
			_, err = tx.Exec(ctx, `UPDATE task_operations SET lease_version=$2, runtime_generation=$3, runtime_instance_id=$4,
control_version=$5, runtime_lease_version=$6, updated_at=clock_timestamp()
WHERE id=$1 AND task_id=$7 AND lease_version=$8 AND status='prepared'`, operation.ID, update.LeaseVersion,
				update.RuntimeGeneration, update.RuntimeInstanceID, update.ControlVersion, update.RuntimeLeaseVersion,
				update.TaskID, operation.LeaseVersion)
		} else if rearming {
			_, err = tx.Exec(ctx, `UPDATE task_operations SET status='prepared', external_id='', result='', lease_version=$2,
runtime_generation=$3, runtime_instance_id=$4, control_version=$5, runtime_lease_version=$6, updated_at=clock_timestamp()
WHERE id=$1 AND task_id=$7 AND lease_version=$2 AND status='unknown'`, operation.ID, update.LeaseVersion,
				update.RuntimeGeneration, update.RuntimeInstanceID, update.ControlVersion, update.RuntimeLeaseVersion, update.TaskID)
		} else if dispatching {
			_, err = tx.Exec(ctx, `UPDATE task_operations SET status=$3, external_id=$4, result=$5, lease_version=$6,
runtime_generation=$7, runtime_instance_id=$8, control_version=$9, runtime_lease_version=$10, updated_at=clock_timestamp()
WHERE id=$1 AND task_id=$2 AND lease_version=$6 AND status='prepared'`, operation.ID, update.TaskID, string(update.Status), update.ExternalID, update.Result,
				update.LeaseVersion, update.RuntimeGeneration, update.RuntimeInstanceID, update.ControlVersion, update.RuntimeLeaseVersion)
		} else {
			_, err = tx.Exec(ctx, `UPDATE task_operations SET status=$3, external_id=$4, result=$5, lease_version=$6, updated_at=clock_timestamp()
WHERE id=$1 AND task_id=$2 AND lease_version=$6`, operation.ID, update.TaskID, string(update.Status), update.ExternalID, update.Result, update.LeaseVersion)
		}
		if err != nil {
			return fmt.Errorf("update task operation: %w", err)
		}
		payloadValue := map[string]any{"operation_id": operation.ID, "operation_key": update.OperationKey, "status": string(update.Status)}
		if adoptingPrepared {
			payloadValue["adoption_reason"] = "task_lease_reclaimed_before_dispatch"
		} else if rearming {
			payloadValue["rearm_reason"] = "runtime_operation_not_found"
		}
		payload, err := json.Marshal(payloadValue)
		if err != nil {
			return fmt.Errorf("encode operation event: %w", err)
		}
		sequence, err := incrementTaskEventSequence(ctx, tx, update.TaskID)
		if err != nil {
			return err
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: update.TaskID, Sequence: sequence, Type: "task.operation", Payload: payload}); err != nil {
			return err
		}
		result, err = loadOperationByKey(ctx, tx, update.TaskID, update.OperationKey)
		if err != nil {
			return fmt.Errorf("read updated task operation: %w", err)
		}
		return nil
	})
	if err == nil && authorizationDenied {
		return task.Operation{}, taskLeaseConflict()
	}
	return result, err
}

func rowTaskDomain(row sqlcgen.Task) task.Task { return taskDomain(row) }

func requireCurrentLease(ctx context.Context, tx pgx.Tx, row sqlcgen.Task, owner string, version int64) error {
	if row.Status != string(task.StatusRunning) || row.LeaseOwner != owner || row.LeaseVersion != version {
		return taskLeaseConflict()
	}
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT lease_expires_at > clock_timestamp() FROM tasks WHERE id=$1`, row.ID).Scan(&valid); err != nil {
		return fmt.Errorf("check task operation lease: %w", err)
	}
	if !valid {
		return taskLeaseConflict()
	}
	return nil
}

func taskLeaseConflict() error {
	err := domain.NewError(domain.CodeInvalidState, "task lease is stale or expired")
	err.Details["reason"] = "lease_lost"
	return err
}

func requireOperationHostFence(ctx context.Context, tx pgx.Tx, ownerID string, operation task.Operation) error {
	var runtimeGeneration, controlVersion int64
	var runtimeInstanceID string
	var controlState string
	var hostState string
	var deleted bool
	err := tx.QueryRow(ctx, `SELECT runtime_generation, COALESCE(runtime_instance_id,''), control_version, control_state, state, deleted_at IS NOT NULL FROM hosts WHERE id=$1 AND owner_id=$2 FOR SHARE`,
		operation.HostID, ownerID).Scan(&runtimeGeneration, &runtimeInstanceID, &controlVersion, &controlState, &hostState, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return fmt.Errorf("read host operation fence: %w", err)
	}
	if operation.RuntimeGeneration != runtimeGeneration || operation.RuntimeInstanceID != runtimeInstanceID ||
		operation.ControlVersion != controlVersion || controlState != "ai" || hostState != "running" || deleted {
		err := domain.NewError(domain.CodeInvalidState, "host runtime or control state changed")
		err.Details["reason"] = "control_changed"
		return err
	}
	return nil
}

func requireTaskWorkspaceInputAdmission(ctx context.Context, tx pgx.Tx, row sqlcgen.Task, operation task.Operation) error {
	if operation.InputRevision == nil || !row.WorkspaceID.Valid || row.WorkspaceID.String != operation.WorkspaceID ||
		!row.InputRevision.Valid || row.InputRevision.Int64 != *operation.InputRevision ||
		!row.WorkspaceCheckpointRevision.Valid || operation.WorkspaceCheckpointRevision == nil ||
		row.WorkspaceCheckpointRevision.Int64 != *operation.WorkspaceCheckpointRevision {
		return domain.NewError(domain.CodeWorkspaceConflict, "task Workspace input changed before operation admission")
	}
	checkpoint := *operation.WorkspaceCheckpointRevision
	var ownerID, state string
	var revision int64
	err := tx.QueryRow(ctx, `SELECT owner_id, state, revision FROM workspaces WHERE id=$1 FOR SHARE`, operation.WorkspaceID).
		Scan(&ownerID, &state, &revision)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (ownerID != row.OwnerID || state != string(workspace.StateReady) || revision != checkpoint)) {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed after this task input was confirmed")
	}
	if err != nil {
		return fmt.Errorf("lock Workspace for task operation admission: %w", err)
	}
	// A verified Task publish advances its private view before the Host replica catches up.
	var bindingOwner, bindingState, syncState string
	var bindingVersion int64
	err = tx.QueryRow(ctx, `SELECT owner_id, state, sync_state, binding_version
FROM workspace_bindings WHERE host_id=$1 AND workspace_id=$2 FOR SHARE`, operation.HostID, operation.WorkspaceID).
		Scan(&bindingOwner, &bindingState, &syncState, &bindingVersion)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (bindingOwner != row.OwnerID || bindingState != string(workspace.BindingActive) ||
		bindingVersion != operation.BindingVersion || syncState != string(workspace.BindingSyncHealthy))) {
		return domain.NewError(domain.CodeWorkspaceConflict, "Host Workspace no longer matches the task input revision")
	}
	if err != nil {
		return fmt.Errorf("lock Host Workspace binding for task operation admission: %w", err)
	}
	return nil
}

// The Host fence lock is held while checking its managed application state.
func requireRunningTaskApplication(ctx context.Context, tx pgx.Tx, value sqlcgen.Task) error {
	if value.ApplicationID == "" {
		return nil
	}
	var running bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM host_application_launches a JOIN hosts h ON h.id=a.host_id AND h.owner_id=a.owner_id
WHERE a.host_id=$1 AND a.owner_id=$2 AND a.application_id=$3 AND a.status='running'
  AND a.runtime_generation=h.runtime_generation AND a.runtime_instance_id=h.runtime_instance_id
  AND a.control_version=h.control_version)`, value.HostID.String, value.OwnerID, value.ApplicationID).Scan(&running)
	if err != nil {
		return fmt.Errorf("check task application launch: %w", err)
	}
	if !running {
		return domain.NewError(domain.CodeInvalidState, "target application is not running on this Host")
	}
	return nil
}

func incrementTaskEventSequence(ctx context.Context, tx pgx.Tx, taskID string) (int64, error) {
	var sequence int64
	if err := tx.QueryRow(ctx, `UPDATE tasks SET event_seq=event_seq+1, updated_at=clock_timestamp() WHERE id=$1 RETURNING event_seq`, taskID).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("allocate task event sequence: %w", err)
	}
	return sequence, nil
}

func loadOperationByKey(ctx context.Context, tx pgx.Tx, taskID, operationKey string) (task.Operation, error) {
	var row sqlcgen.TaskOperation
	err := tx.QueryRow(ctx, `SELECT id, task_id, operation_key, request_hash, host_id, workspace_id, binding_version, input_revision, workspace_checkpoint_revision, runtime_generation, runtime_instance_id, control_version, lease_version, runtime_lease_version, status, external_id, result, created_at, updated_at
FROM task_operations WHERE task_id=$1 AND operation_key=$2`, taskID, operationKey).Scan(
		&row.ID, &row.TaskID, &row.OperationKey, &row.RequestHash, &row.HostID, &row.WorkspaceID, &row.BindingVersion,
		&row.InputRevision, &row.WorkspaceCheckpointRevision, &row.RuntimeGeneration, &row.RuntimeInstanceID, &row.ControlVersion, &row.LeaseVersion, &row.RuntimeLeaseVersion, &row.Status,
		&row.ExternalID, &row.Result, &row.CreatedAt, &row.UpdatedAt)
	return taskOperationDomain(row), err
}

func validOperationWorkspaceBinding(workspaceID string, bindingVersion int64) bool {
	if workspaceID == "" {
		return bindingVersion == 0
	}
	return domain.ValidWorkspaceID(workspaceID) && bindingVersion > 0
}

func validTaskStatus(value task.Status) bool {
	switch value {
	case task.StatusQueued, task.StatusRunning, task.StatusWaitingUser, task.StatusPausing,
		task.StatusPaused, task.StatusCancelling, task.StatusCompleted, task.StatusFailed, task.StatusCancelled:
		return true
	default:
		return false
	}
}

func validStepStatus(value task.StepStatus) bool {
	switch value {
	case task.StepQueued, task.StepRunning, task.StepWaitingUser, task.StepSucceeded, task.StepFailed, task.StepUnknown:
		return true
	default:
		return false
	}
}

func validOperationStatus(value task.OperationStatus) bool {
	switch value {
	case task.OperationPrepared, task.OperationDispatched, task.OperationSucceeded, task.OperationFailed, task.OperationCancelled, task.OperationUnknown:
		return true
	default:
		return false
	}
}

func equalOperationHash(a, b []byte) bool {
	if len(a) != len(b) || len(a) != 32 {
		return false
	}
	var mismatch byte
	for i := range a {
		mismatch |= a[i] ^ b[i]
	}
	return mismatch == 0
}

var _ app.TaskStore = (*Store)(nil)
