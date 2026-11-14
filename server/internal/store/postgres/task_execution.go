package postgres

import (
	"bytes"
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
	"morde/server/internal/domain/host"
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

const taskHubWorkspaceSyncClaimPredicate = `(t.status='waiting_user' AND t.waiting_reason='task_hub_workspace_sync' AND EXISTS (
  SELECT 1 FROM task_hub_operations settled
  JOIN task_events wait_event ON wait_event.task_id=t.id AND wait_event.sequence=t.event_seq
  JOIN workspace_sync_sessions sync_session ON sync_session.workspace_id=settled.workspace_id AND sync_session.id=settled.sync_session_id
  WHERE settled.owner_id=t.owner_id AND settled.task_id=t.id AND settled.action='file.put'
    AND settled.status='succeeded' AND settled.sync_stage='cloud_synced'
    AND wait_event.event_type='task.waiting_user'
    AND wait_event.payload#>>'{detail,operation_key}'=settled.operation_key
    AND sync_session.task_id=settled.task_id AND sync_session.task_hub_operation_key=settled.operation_key
    AND sync_session.state='completed' AND sync_session.target_revision=settled.sync_revision
    AND sync_session.replica_id='device:'||settled.target_installation_id
) AND NOT EXISTS (
  SELECT 1 FROM task_hub_operations active
  WHERE active.owner_id=t.owner_id AND active.task_id=t.id AND active.action='file.put'
    AND active.status IN ('prepared','dispatched','unknown')
    AND active.sync_stage IN ('pending','local_applied','sync_pending')
))`

func (s *Store) withTaskSnapshot(ctx context.Context, fn func(pgx.Tx) error) error {
	return withTx(ctx, func(ctx context.Context) (pgx.Tx, error) {
		return s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	}, fn)
}

func (s *Store) GetTask(ctx context.Context, id, ownerID string) (app.TaskDetails, error) {
	var result app.TaskDetails
	err := s.withTaskSnapshot(ctx, func(tx pgx.Tx) error {
		queries := sqlcgen.New(tx)
		taskOwnerID, err := taskOwnerIDForActor(ctx, tx, id, ownerID, false, false)
		if err != nil {
			return err
		}
		row, err := queries.GetTask(ctx, sqlcgen.GetTaskParams{ID: id, OwnerID: taskOwnerID})
		if err != nil {
			return storeError("get task", "task", err)
		}
		if !row.ProjectID.Valid && row.InitiatorID != row.OwnerID {
			return domain.NewError(domain.CodeUnauthenticated, "personal task authorization is unavailable")
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
		steps, err := listTaskStepsWithActivity(ctx, tx, id)
		if err != nil {
			return fmt.Errorf("list task steps: %w", err)
		}
		agents, err := listTaskAgentExecutions(ctx, tx, row.ID, row.OwnerID)
		if err != nil {
			return fmt.Errorf("list task Agent executions: %w", err)
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
		var expiresAt, respondedAt pgtype.Timestamptz
		err = tx.QueryRow(ctx, `SELECT id, task_id, version, kind, title, object, content, impact, call_id, COALESCE(media_preview_operation_id,''), tool_name,
request_hash, runtime_generation, runtime_instance_id, control_version, status, decision, created_at, expires_at, responded_at
FROM task_pending_requests WHERE task_id=$1 ORDER BY version DESC LIMIT 1`, id).Scan(
			&pending.ID, &pending.TaskID, &pending.Version, &pending.Kind, &pending.Title, &pending.Object, &pending.Content,
			&pending.Impact, &pending.CallID, &pending.MediaPreviewOperationID, &pending.ToolName, &requestHash, &pending.RuntimeGeneration,
			&pending.RuntimeInstanceID, &pending.ControlVersion, &pending.Status, &pending.Decision, &pending.CreatedAt, &expiresAt, &respondedAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read latest task pending request: %w", err)
		}
		var pendingRequest *task.PendingRequest
		if err == nil {
			pending.RequestHash = append([]byte(nil), requestHash...)
			if expiresAt.Valid {
				expires := expiresAt.Time
				pending.ExpiresAt = &expires
			}
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
		assistantMessage, err := readAssistantMessageSnapshot(ctx, tx, row)
		if err != nil {
			return err
		}
		projected := []task.Task{rowTaskDomain(row)}
		if err := populateTaskRevisionProjection(ctx, tx, projected); err != nil {
			return err
		}
		var resultMessagePending, resultMessagePublished, currentRevisionStarted bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM conversation_result_sources source
 JOIN conversation_result_batches batch ON batch.message_id=source.batch_message_id
 JOIN service_identity instance ON instance.singleton=true AND instance.instance_id=batch.service_instance_id
 WHERE source.task_id=$1 AND source.task_revision=$2 AND source.state='pending' AND batch.state IN ('pending','processing')
), EXISTS (
 SELECT 1 FROM conversation_result_sources source
 JOIN conversation_result_batches batch ON batch.message_id=source.batch_message_id
 JOIN service_identity instance ON instance.singleton=true AND instance.instance_id=batch.service_instance_id
 WHERE source.task_id=$1 AND source.task_revision=$2 AND source.state='published' AND batch.state='published'
), EXISTS (SELECT 1 FROM task_revision_starts started
 WHERE started.task_id=$1 AND started.revision=$2)
`, row.ID, *projected[0].CurrentRevision).Scan(&resultMessagePending, &resultMessagePublished, &currentRevisionStarted); err != nil {
			return fmt.Errorf("read pending Task result message: %w", err)
		}
		dependencies, childJobs, resultReview, err := loadTaskK2Projection(ctx, tx, row.ID, *projected[0].CurrentRevision)
		if err != nil {
			return err
		}
		result = app.TaskDetails{
			Task: projected[0], AssistantMessage: assistantMessage, CurrentRevisionStarted: &currentRevisionStarted,
			Dependencies: dependencies, ChildJobs: childJobs, ResultReview: resultReview,
			ResultMessagePending: resultMessagePending, ResultMessagePublished: resultMessagePublished,
			ScheduleObservation: scheduleObservation, Steps: steps, Agents: agents,
			BatchItems: mapRows(items, taskBatchItemDomain), Artifacts: mapRows(artifacts, taskArtifactDomain),
			PendingRequest: pendingRequest, ClarificationRequests: clarifications,
			StopRequest: stopRequest, StopOperationIDs: stopOperationIDs,
		}
		return nil
	})
	return result, err
}

func listTaskStepsWithActivity(ctx context.Context, tx pgx.Tx, taskID string) ([]task.Step, error) {
	rows, err := tx.Query(ctx, `SELECT id,task_id,sequence,attempt_no,COALESCE(retry_of_step_id,''),kind,status,title,summary,
COALESCE(failure_reason,''),retryable,created_at,COALESCE(agent_session_id,''),task_revision,COALESCE(tool_call_key,''),
COALESCE(tool_name,''),request_digest,COALESCE(phase,''),detail_state
FROM task_steps WHERE task_id=$1 ORDER BY sequence`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	steps := make([]task.Step, 0)
	for rows.Next() {
		var step task.Step
		var revision pgtype.Int8
		if err := rows.Scan(&step.ID, &step.TaskID, &step.Sequence, &step.Attempt, &step.RetryOfStepID, &step.Kind, &step.Status,
			&step.Title, &step.Summary, &step.FailureReason, &step.Retryable, &step.CreatedAt, &step.AgentSessionID, &revision,
			&step.ToolCallKey, &step.ToolName, &step.RequestDigest, &step.Phase, &step.DetailState); err != nil {
			return nil, err
		}
		if revision.Valid {
			value := revision.Int64
			step.TaskRevision = &value
		}
		steps = append(steps, step)
	}
	return steps, rows.Err()
}

func listTaskAgentExecutions(ctx context.Context, tx pgx.Tx, taskID, ownerID string) ([]app.TaskAgentExecution, error) {
	rows, err := tx.Query(ctx, `SELECT s.session_id,COALESCE(s.parent_session_id,''),COALESCE(s.step_id,''),
CASE WHEN s.parent_session_id IS NULL THEN t.current_revision ELSE s.task_revision END,
CASE WHEN s.parent_session_id IS NULL THEN 'main' ELSE 'subagent' END,
CASE WHEN live.valid AND s.activity_generation=t.agent_generation AND s.activity_sequence IS NOT NULL
  AND (s.parent_session_id IS NULL OR s.task_revision=t.current_revision) THEN s.activity_status ELSE 'unknown' END,
CASE WHEN live.valid AND s.activity_generation=t.agent_generation AND s.activity_sequence IS NOT NULL
  AND (s.parent_session_id IS NULL OR s.task_revision=t.current_revision) THEN s.activity_observed_at ELSE NULL END
FROM task_agent_sessions s JOIN tasks t ON t.id=s.task_id AND t.owner_id=s.owner_id
LEFT JOIN LATERAL (SELECT true AS valid FROM task_agent_instances ai
  WHERE ai.task_id=t.id AND ai.owner_id=t.owner_id AND ai.agent_generation=t.agent_generation
    AND ai.agent_instance_id=t.agent_instance_id AND ai.agent_session_id=t.agent_session_id
    AND ai.lease_owner=t.lease_owner AND ai.lease_version=t.lease_version AND ai.auth_version=t.auth_version
    AND ai.state='active' AND ai.lease_expires_at > clock_timestamp()
    AND t.status='running' AND t.lease_expires_at > clock_timestamp()
    AND (t.auth_kind='schedule_task' OR t.auth_expires_at > clock_timestamp())
  LIMIT 1) live ON true
WHERE s.task_id=$1 AND s.owner_id=$2
ORDER BY (s.parent_session_id IS NULL) DESC,s.created_at,s.session_id`, taskID, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := make([]app.TaskAgentExecution, 0)
	for rows.Next() {
		var agent app.TaskAgentExecution
		var status string
		var observedAt pgtype.Timestamptz
		if err := rows.Scan(&agent.ID, &agent.ParentID, &agent.StepID, &agent.TaskRevision, &agent.Role, &status, &observedAt); err != nil {
			return nil, err
		}
		agent.Status = status
		if observedAt.Valid {
			value := observedAt.Time
			agent.ObservedAt = &value
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
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
	var rows []sqlcgen.Task
	visibleRows := make([]sqlcgen.Task, 0)
	var items []task.Task
	var k2Projections map[string]app.TaskK2Projection
	err = s.withTaskSnapshot(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlcgen.New(tx).ListTasks(ctx, sqlcgen.ListTasksParams{
			OwnerID: ownerID, Column2: filter.HostIDs, Column3: status, Column4: cursorAt, ID: cursorID, Limit: limit + 1,
		})
		if err != nil {
			return fmt.Errorf("list tasks: %w", err)
		}
		for _, row := range rows {
			if !row.ProjectID.Valid && row.InitiatorID != row.OwnerID {
				continue
			}
			visibleRows = append(visibleRows, row)
		}
		items = mapRows(visibleRows, rowTaskDomain)
		if err := populateTaskStepCounts(ctx, tx, items); err != nil {
			return err
		}
		if err := populateTaskRevisionProjection(ctx, tx, items); err != nil {
			return err
		}
		projectionItems := items
		if int32(len(projectionItems)) > limit {
			projectionItems = projectionItems[:limit]
		}
		k2Projections, err = loadTaskK2Projections(ctx, tx, projectionItems)
		return err
	})
	if err != nil {
		return app.TaskPage{}, err
	}
	page := app.TaskPage{Items: items, K2Projections: k2Projections}
	page.NextCursor = nextPageCursor(items, limit, func(value task.Task) (string, time.Time) { return value.ID, value.CreatedAt })
	if int32(len(items)) > limit {
		page.Items = items[:limit]
	}
	return page, nil
}

func populateTaskStepCounts(ctx context.Context, tx pgx.Tx, items []task.Task) error {
	if len(items) == 0 {
		return nil
	}
	taskIDs := make([]string, 0, len(items))
	for _, item := range items {
		taskIDs = append(taskIDs, item.ID)
	}
	rows, err := tx.Query(ctx, `
SELECT task_id,
       count(*) FILTER (WHERE kind <> 'budget')::bigint AS step_count,
       count(*) FILTER (WHERE kind <> 'budget' AND status = 'succeeded')::bigint AS completed_step_count
FROM task_steps
WHERE task_id = ANY($1::text[])
GROUP BY task_id`, taskIDs)
	if err != nil {
		return fmt.Errorf("list task step counts: %w", err)
	}
	defer rows.Close()
	type stepCounts struct {
		total     int64
		completed int64
	}
	countsByTask := make(map[string]stepCounts, len(items))
	for rows.Next() {
		var taskID string
		var counts stepCounts
		if err := rows.Scan(&taskID, &counts.total, &counts.completed); err != nil {
			return fmt.Errorf("scan task step counts: %w", err)
		}
		countsByTask[taskID] = counts
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read task step counts: %w", err)
	}
	for index := range items {
		counts := countsByTask[items[index].ID]
		items[index].StepCount = counts.total
		items[index].CompletedStepCount = counts.completed
	}
	return nil
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
		var taskOwnerID, initiatorID string
		var projectID pgtype.Text
		var err error
		taskOwnerID, err = taskOwnerIDForActor(ctx, tx, taskID, ownerID, false, false)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT COALESCE(host_id, ''), status, event_seq,project_id,initiator_id FROM tasks WHERE id=$1 AND owner_id=$2`, taskID, taskOwnerID).
			Scan(&result.HostID, &status, &current, &projectID, &initiatorID)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("read task event state: %w", err)
		}
		if !projectID.Valid && initiatorID != ownerID {
			return domain.NewError(domain.CodeUnauthenticated, "personal task authorization is unavailable")
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
		var ownerID, initiatorID string
		err := tx.QueryRow(ctx, `SELECT owner_id,initiator_id FROM tasks WHERE id=$1 AND status='running'
AND lease_owner=$2 AND lease_version=$3 AND lease_expires_at > clock_timestamp()
AND current_revision=desired_revision FOR UPDATE`,
			delta.TaskID, delta.LeaseOwner, delta.LeaseVersion).Scan(&ownerID, &initiatorID)
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
		taskRow, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: delta.TaskID, OwnerID: ownerID})
		if err != nil {
			return fmt.Errorf("read task authorization before appending assistant text: %w", err)
		}
		if taskRow.InitiatorID != initiatorID {
			return domain.NewError(domain.CodeUnauthenticated, "task initiator is unavailable")
		}
		if taskRow.WorkspaceID.Valid {
			if err := lockTaskWorkspaceForExecution(ctx, tx, taskRow.WorkspaceID.String, taskRow.InitiatorID); err != nil {
				return err
			}
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, initiatorID, &taskRow.ExecutionEpoch)
		if err != nil {
			return err
		}
		if authorized, err := requireCurrentTaskAuthorization(ctx, tx, taskRow); err != nil {
			return err
		} else if !authorized {
			return domain.NewError(domain.CodeUnauthenticated, "task text authorization is unavailable")
		}
		if gate.state != execution.StateEnabled {
			return ownerExecutionDisabledError(gate)
		}
		var sequence int64
		err = tx.QueryRow(ctx, `UPDATE tasks SET event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND status='running' AND lease_owner=$3 AND lease_version=$4
AND lease_expires_at > clock_timestamp() AND current_revision=desired_revision RETURNING event_seq`,
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
		if err := appendAssistantMessageSnapshot(ctx, tx, delta); err != nil {
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

func readAssistantMessageSnapshot(ctx context.Context, tx pgx.Tx, taskRow sqlcgen.Task) (*app.AssistantMessageSnapshot, error) {
	var messageID, turnID, content, rawStatus string
	var sequence, taskRevision, resultEventSequence pgtype.Int8
	var createdAt time.Time
	err := tx.QueryRow(ctx, `SELECT snapshot.message_id, snapshot.turn_id, snapshot.content, snapshot.status,
       COALESCE(snapshot.message_sequence, message.sequence), snapshot.created_at,
       snapshot.task_revision, snapshot.result_event_sequence
FROM task_assistant_message_snapshots snapshot
LEFT JOIN conversation_messages message ON message.id=snapshot.message_id AND message.task_id=snapshot.task_id
  AND message.conversation_id=$2 AND message.owner_id=$3 AND message.role='assistant'
WHERE snapshot.task_id=$1`, taskRow.ID, taskRow.ConversationID, taskRow.OwnerID).
		Scan(&messageID, &turnID, &content, &rawStatus, &sequence, &createdAt, &taskRevision, &resultEventSequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read assistant message snapshot: %w", err)
	}
	status := app.AssistantMessageStatus(rawStatus)
	switch status {
	case app.AssistantMessageStreaming, app.AssistantMessageCompleted, app.AssistantMessageInterrupted:
	default:
		return nil, domain.NewError(domain.CodeInvalidState, "assistant message snapshot status is invalid")
	}
	// A task may be stopped outside CommitTaskProgress (for example, a user
	// cancellation), or its worker lease may expire before another worker
	// claims it. Never expose unfinished text as live in either case.
	if status == app.AssistantMessageStreaming {
		var activeLease bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE(status='running' AND lease_expires_at > clock_timestamp(), false)
FROM tasks WHERE id=$1`, taskRow.ID).Scan(&activeLease); err != nil {
			return nil, fmt.Errorf("check assistant message task lease: %w", err)
		}
		if taskRow.Status != string(task.StatusRunning) || !activeLease {
			status = app.AssistantMessageInterrupted
		}
	}
	message := conversation.Message{
		ID: messageID, ConversationID: taskRow.ConversationID, OwnerID: taskRow.OwnerID,
		Role: conversation.RoleAssistant, Content: content, TaskID: taskRow.ID,
		ModelID: taskRow.ModelID, SourceKind: "task", CreatedAt: createdAt,
	}
	if sequence.Valid {
		message.Sequence = sequence.Int64
	}
	return &app.AssistantMessageSnapshot{Message: message, Status: status,
		TaskRevision: int64Pointer(taskRevision), ResultEventSequence: int64Pointer(resultEventSequence)}, nil
}

func appendAssistantMessageSnapshot(ctx context.Context, tx pgx.Tx, delta app.AssistantTextDelta) error {
	var messageID, turnID, content, status string
	var snapshotRevision pgtype.Int8
	var currentRevision int64
	if err := tx.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1`, delta.TaskID).Scan(&currentRevision); err != nil {
		return fmt.Errorf("read streaming Task revision: %w", err)
	}
	err := tx.QueryRow(ctx, `SELECT message_id, turn_id, content, status, task_revision
FROM task_assistant_message_snapshots WHERE task_id=$1 FOR UPDATE`, delta.TaskID).
		Scan(&messageID, &turnID, &content, &status, &snapshotRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err := tx.Exec(ctx, `INSERT INTO task_assistant_message_snapshots
(task_id, message_id, turn_id, content, status, task_revision)
VALUES ($1,$2,$3,$4,'streaming',$5)`, delta.TaskID, delta.MessageID, delta.TurnID, delta.Delta, currentRevision)
		if err != nil {
			return fmt.Errorf("create assistant message snapshot: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock assistant message snapshot: %w", err)
	}
	if messageID == delta.MessageID {
		if turnID != delta.TurnID || status != string(app.AssistantMessageStreaming) ||
			(snapshotRevision.Valid && snapshotRevision.Int64 != currentRevision) || (!snapshotRevision.Valid && currentRevision != 0) {
			return domain.NewError(domain.CodeInvalidState, "assistant message snapshot identity is already finalized")
		}
		if len(content)+len(delta.Delta) > app.MaxAssistantMessageBytes {
			return domain.InvalidArgument("text_delta", "assistant message exceeds the size limit")
		}
		if _, err := tx.Exec(ctx, `UPDATE task_assistant_message_snapshots
SET content=content || $2,task_revision=COALESCE(task_revision,$5),updated_at=clock_timestamp()
WHERE task_id=$1 AND message_id=$3 AND turn_id=$4 AND status='streaming'`,
			delta.TaskID, delta.Delta, delta.MessageID, delta.TurnID, currentRevision); err != nil {
			return fmt.Errorf("append assistant message snapshot: %w", err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE task_assistant_message_snapshots
SET message_id=$2, turn_id=$3, content=$4, status='streaming', message_sequence=NULL,task_revision=$5,result_event_sequence=NULL,
created_at=clock_timestamp(), updated_at=clock_timestamp()
WHERE task_id=$1`, delta.TaskID, delta.MessageID, delta.TurnID, delta.Delta, currentRevision); err != nil {
		return fmt.Errorf("replace assistant message snapshot: %w", err)
	}
	return nil
}

func completeAssistantMessageSnapshot(ctx context.Context, tx pgx.Tx, taskID, turnID string, message conversation.Message) error {
	var sequence int64
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `SELECT sequence, created_at FROM conversation_messages
WHERE id=$1 AND conversation_id=$2 AND owner_id=$3 AND task_id=$4`,
		message.ID, message.ConversationID, message.OwnerID, taskID).Scan(&sequence, &createdAt); err != nil {
		return fmt.Errorf("read completed assistant message metadata: %w", err)
	}
	// A different turn cannot replace text that was already shown. Keep that
	// snapshot interrupted while the independently committed message remains
	// available from the conversation history.
	if _, err := tx.Exec(ctx, `UPDATE task_assistant_message_snapshots SET status='interrupted',updated_at=clock_timestamp()
WHERE task_id=$1 AND status='streaming' AND content<>'' AND
 (message_id<>$2 OR turn_id<>$3 OR left($4,char_length(content))<>content)`,
		taskID, message.ID, turnID, message.Content); err != nil {
		return fmt.Errorf("preserve interrupted assistant message: %w", err)
	}
	_, err := tx.Exec(ctx, `INSERT INTO task_assistant_message_snapshots
(task_id, message_id, turn_id, content, status, message_sequence, created_at, updated_at,task_revision,result_event_sequence)
VALUES ($1,$2,$3,$4,'completed',$5,$6,clock_timestamp(),(SELECT current_revision FROM tasks WHERE id=$1),NULL)
ON CONFLICT (task_id) DO UPDATE SET message_id=EXCLUDED.message_id, turn_id=EXCLUDED.turn_id,
content=EXCLUDED.content, status='completed', message_sequence=EXCLUDED.message_sequence,
created_at=EXCLUDED.created_at, updated_at=clock_timestamp(),task_revision=EXCLUDED.task_revision,result_event_sequence=NULL
WHERE NOT (task_assistant_message_snapshots.status='interrupted' AND task_assistant_message_snapshots.content<>'' AND
 (task_assistant_message_snapshots.message_id<>EXCLUDED.message_id OR task_assistant_message_snapshots.turn_id<>EXCLUDED.turn_id OR
  left(EXCLUDED.content,char_length(task_assistant_message_snapshots.content))<>task_assistant_message_snapshots.content))
 OR EXISTS (SELECT 1 FROM conversation_messages archived
   WHERE archived.id=task_assistant_message_snapshots.message_id AND archived.task_id=task_assistant_message_snapshots.task_id
     AND archived.output_status='interrupted' AND archived.output_task_revision=task_assistant_message_snapshots.task_revision
     AND archived.content=task_assistant_message_snapshots.content)`,
		taskID, message.ID, turnID, message.Content, sequence, createdAt)
	if err != nil {
		return fmt.Errorf("complete assistant message snapshot: %w", err)
	}
	return nil
}

func interruptAssistantMessageSnapshot(ctx context.Context, tx pgx.Tx, taskID, turnID string, allStreaming bool) error {
	query := `UPDATE task_assistant_message_snapshots SET status='interrupted', updated_at=clock_timestamp()
WHERE task_id=$1 AND status='streaming'`
	args := []any{taskID}
	if !allStreaming {
		query += ` AND turn_id=$2`
		args = append(args, turnID)
	}
	query += ` RETURNING message_id,task_revision`
	var messageID string
	var revision pgtype.Int8
	if err := tx.QueryRow(ctx, query, args...).Scan(&messageID, &revision); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return fmt.Errorf("interrupt assistant message snapshot: %w", err)
	}
	// Older snapshots may predate durable stream messages. If a matching row
	// exists, persist the interruption without changing its identity or text.
	var ownerID, conversationID string
	err := tx.QueryRow(ctx, `UPDATE conversation_messages SET output_status='interrupted'
WHERE id=$1 AND task_id=$2 AND output_status='streaming' AND output_task_revision=$3
RETURNING owner_id,conversation_id`, messageID, taskID, revision).Scan(&ownerID, &conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("interrupt assistant stream message: %w", err)
	}
	if err := recordAccountObjectChanges(ctx, tx,
		accountObjectChange{OwnerID: ownerID, ObjectType: "conversation", ObjectID: conversationID, Operation: "upsert"},
		accountObjectChange{OwnerID: ownerID, ObjectType: "message", ObjectID: messageID, Operation: "upsert"},
	); err != nil {
		return err
	}
	return nil
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
		var initiatorID string
		var taskEpoch int64
		err := tx.QueryRow(ctx, `
SELECT t.initiator_id, t.execution_epoch FROM tasks AS t
WHERE (t.status='queued' OR (t.status='running' AND t.lease_expires_at <= clock_timestamp()) OR
  `+taskHubWorkspaceSyncClaimPredicate+` OR
  (t.status='waiting_user' AND t.waiting_reason='workspace_host_sync' AND EXISTS (
    SELECT 1 FROM workspace_bindings wb JOIN workspaces w ON w.id=wb.workspace_id
    WHERE wb.host_id=t.host_id AND wb.workspace_id=t.workspace_id
      AND wb.state='active'
      AND wb.sync_state='healthy'
      AND wb.desired_revision>=t.workspace_checkpoint_revision AND wb.applied_revision>=t.workspace_checkpoint_revision
      AND w.state='ready' AND w.deleted_at IS NULL AND w.revision=t.workspace_checkpoint_revision
      AND EXISTS (SELECT 1 FROM hosts h WHERE h.id=t.host_id AND h.state='running' AND h.deleted_at IS NULL
        AND (h.owner_id=t.initiator_id OR EXISTS (SELECT 1 FROM host_members hm
          WHERE hm.host_id=h.id AND hm.owner_id=h.owner_id AND hm.user_id=t.initiator_id
            AND hm.revoked_at IS NULL AND 'desktop:control'=ANY(hm.permissions)))
        AND EXISTS (SELECT 1 FROM host_runtime_heartbeats hb WHERE hb.host_id=h.id AND hb.runtime_generation=h.runtime_generation
          AND hb.last_seen_at > clock_timestamp() - $2::double precision * interval '1 second'))
  )))
  AND t.execution_location='server' AND t.current_revision=t.desired_revision
	  AND NOT EXISTS (SELECT 1 FROM task_dependencies dependency JOIN tasks prerequisite
	    ON prerequisite.id=dependency.dependency_task_id AND prerequisite.owner_id=dependency.owner_id
	      AND prerequisite.conversation_id=dependency.conversation_id AND prerequisite.initiator_id=t.initiator_id
	      AND prerequisite.project_id IS NOT DISTINCT FROM t.project_id
	    WHERE dependency.task_id=t.id AND (prerequisite.status<>'completed'
	      OR prerequisite.current_revision<>dependency.required_revision OR prerequisite.desired_revision<>dependency.required_revision OR prerequisite.result=''))
	  AND ((t.project_id IS NULL AND t.initiator_id=t.owner_id) OR (t.project_id IS NOT NULL AND (t.checkpoint_through_seq>=0 OR EXISTS (
	    SELECT 1 FROM project_members pm JOIN users pu ON pu.id=pm.user_id AND pu.status='approved'
	    WHERE pm.project_id=t.project_id AND pm.user_id=t.initiator_id AND pm.role IN ('member','owner')
	  ))))
  AND ($1::integer = 0 OR (
    SELECT count(*) FROM tasks AS active_task
    WHERE active_task.initiator_id=t.initiator_id AND active_task.status='running'
      AND active_task.id<>t.id
      AND (active_task.lease_expires_at > clock_timestamp() OR EXISTS (
        SELECT 1 FROM task_agent_instances AS agent
        WHERE agent.task_id=active_task.id AND agent.state='active'
      ))
  ) < $1)
  AND EXISTS (
    SELECT 1 FROM owner_execution_controls AS ec
    WHERE ec.owner_id=t.initiator_id AND ec.state='enabled' AND ec.epoch=t.execution_epoch
  )
  AND (t.host_id IS NULL OR (t.project_id IS NOT NULL AND t.checkpoint_through_seq>=0) OR EXISTS (
    SELECT 1 FROM hosts AS h
    WHERE h.id=t.host_id AND h.state<>'deleted' AND h.deleted_at IS NULL
      AND (h.owner_id=t.initiator_id OR EXISTS (SELECT 1 FROM host_members hm
        WHERE hm.host_id=h.id AND hm.owner_id=h.owner_id AND hm.user_id=t.initiator_id
          AND hm.revoked_at IS NULL AND 'desktop:control'=ANY(hm.permissions)))
  ))
  AND (t.workspace_id IS NULL OR (t.project_id IS NOT NULL AND t.checkpoint_through_seq>=0) OR NOT EXISTS (
    SELECT 1 FROM workspace_ownership_transfers transfer
    WHERE transfer.workspace_id=t.workspace_id AND transfer.state IN ('pending','accepting','unknown')
  ))
ORDER BY t.created_at, t.id
LIMIT 1`, maxActivePerOwner, host.RuntimeHeartbeatFreshness.Seconds()).Scan(&initiatorID, &taskEpoch)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim next task: %w", err)
		}
		var gate executionGate
		if maxActivePerOwner > 0 {
			gate, err = lockOwnerExecutionControl(ctx, tx, initiatorID, true)
			if err != nil {
				return err
			}
			if gate.state != execution.StateEnabled || gate.epoch != taskEpoch {
				return nil
			}
		} else {
			gate, err = requireOwnerExecutionEnabled(ctx, tx, initiatorID, &taskEpoch)
			if err != nil {
				if domain.CodeOf(err) == domain.CodeConflict {
					return nil
				}
				return err
			}
		}
		var id, taskOwnerID string
		var hostID string
		var projectCheckpointRecovery bool
		var oldVersion int64
		err = tx.QueryRow(ctx, `
SELECT t.id, t.owner_id, COALESCE(t.host_id,''), t.lease_version,
  (t.project_id IS NOT NULL AND t.checkpoint_through_seq>=0) FROM tasks AS t
WHERE t.initiator_id=$1 AND t.execution_epoch=$2
  AND (t.status='queued' OR (t.status='running' AND t.lease_expires_at <= clock_timestamp()) OR
    `+taskHubWorkspaceSyncClaimPredicate+` OR
    (t.status='waiting_user' AND t.waiting_reason='workspace_host_sync' AND EXISTS (
      SELECT 1 FROM workspace_bindings wb JOIN workspaces w ON w.id=wb.workspace_id
      WHERE wb.host_id=t.host_id AND wb.workspace_id=t.workspace_id
        AND wb.state='active'
        AND wb.sync_state='healthy'
        AND wb.desired_revision>=t.workspace_checkpoint_revision AND wb.applied_revision>=t.workspace_checkpoint_revision
        AND w.state='ready' AND w.deleted_at IS NULL AND w.revision=t.workspace_checkpoint_revision
        AND EXISTS (SELECT 1 FROM hosts h WHERE h.id=t.host_id AND h.state='running' AND h.deleted_at IS NULL
          AND (h.owner_id=t.initiator_id OR EXISTS (SELECT 1 FROM host_members hm
            WHERE hm.host_id=h.id AND hm.owner_id=h.owner_id AND hm.user_id=t.initiator_id
              AND hm.revoked_at IS NULL AND 'desktop:control'=ANY(hm.permissions)))
          AND EXISTS (SELECT 1 FROM host_runtime_heartbeats hb WHERE hb.host_id=h.id AND hb.runtime_generation=h.runtime_generation
            AND hb.last_seen_at > clock_timestamp() - $3::double precision * interval '1 second'))
    )))
  AND t.execution_location='server' AND t.current_revision=t.desired_revision
	  AND NOT EXISTS (SELECT 1 FROM task_dependencies dependency JOIN tasks prerequisite
	    ON prerequisite.id=dependency.dependency_task_id AND prerequisite.owner_id=dependency.owner_id
	      AND prerequisite.conversation_id=dependency.conversation_id AND prerequisite.initiator_id=t.initiator_id
	      AND prerequisite.project_id IS NOT DISTINCT FROM t.project_id
	    WHERE dependency.task_id=t.id AND (prerequisite.status<>'completed'
	      OR prerequisite.current_revision<>dependency.required_revision OR prerequisite.desired_revision<>dependency.required_revision OR prerequisite.result=''))
	AND ((t.project_id IS NULL AND t.initiator_id=t.owner_id) OR (t.project_id IS NOT NULL AND (t.checkpoint_through_seq>=0 OR EXISTS (
	  SELECT 1 FROM project_members pm JOIN users pu ON pu.id=pm.user_id AND pu.status='approved'
	  WHERE pm.project_id=t.project_id AND pm.user_id=t.initiator_id AND pm.role IN ('member','owner')
	))))
  AND EXISTS (
    SELECT 1 FROM owner_execution_controls AS ec
    WHERE ec.owner_id=t.initiator_id AND ec.state='enabled' AND ec.epoch=t.execution_epoch
  )
  AND (t.host_id IS NULL OR (t.project_id IS NOT NULL AND t.checkpoint_through_seq>=0) OR EXISTS (
    SELECT 1 FROM hosts AS h
    WHERE h.id=t.host_id AND h.state<>'deleted' AND h.deleted_at IS NULL
      AND (h.owner_id=t.initiator_id OR EXISTS (SELECT 1 FROM host_members hm
        WHERE hm.host_id=h.id AND hm.owner_id=h.owner_id AND hm.user_id=t.initiator_id
          AND hm.revoked_at IS NULL AND 'desktop:control'=ANY(hm.permissions)))
  ))
  AND (t.workspace_id IS NULL OR (t.project_id IS NOT NULL AND t.checkpoint_through_seq>=0) OR NOT EXISTS (
    SELECT 1 FROM workspace_ownership_transfers transfer
    WHERE transfer.workspace_id=t.workspace_id AND transfer.state IN ('pending','accepting','unknown')
  ))
ORDER BY t.created_at, t.id
LIMIT 1 FOR UPDATE SKIP LOCKED`, initiatorID, gate.epoch, host.RuntimeHeartbeatFreshness.Seconds()).Scan(&id, &taskOwnerID, &hostID, &oldVersion, &projectCheckpointRecovery)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("select and lock candidate task for claim: %w", err)
		}
		if maxActivePerOwner > 0 {
			var active int64
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks AS active_task WHERE initiator_id=$1 AND id<>$2 AND status='running'
AND (lease_expires_at > clock_timestamp() OR EXISTS (
  SELECT 1 FROM task_agent_instances AS agent WHERE agent.task_id=active_task.id AND agent.state='active'
				))`, initiatorID, id).Scan(&active); err != nil {
				return fmt.Errorf("count active owner tasks before claim: %w", err)
			}
			if active >= int64(maxActivePerOwner) {
				return nil
			}
		}
		if hostID != "" && !projectCheckpointRecovery {
			if err := lockTaskTargetHost(ctx, tx, hostID, initiatorID); err != nil {
				if domain.CodeOf(err) == domain.CodeNotFound {
					return nil
				}
				return err
			}
		}
		taskRow, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: id, OwnerID: taskOwnerID})
		if err != nil {
			return fmt.Errorf("read task authorization before claim: %w", err)
		}
		if taskRow.WorkspaceID.Valid && !projectCheckpointRecovery {
			if err := lockTaskWorkspaceForExecution(ctx, tx, taskRow.WorkspaceID.String, taskRow.InitiatorID); err != nil {
				if domain.CodeOf(err) == domain.CodeWorkspaceBusy || domain.CodeOf(err) == domain.CodeNotFound {
					return nil
				}
				return err
			}
		}
		// A recovered project Task is only claimed here. Its membership,
		// credentials and fixed conversation snapshot are checked together in a
		// second transaction before the executor may start a Runtime. Locking
		// those rows here after the Task would invert the project -> Task order.
		if !taskRow.ProjectID.Valid || taskRow.CheckpointThroughSeq < 0 {
			authorized, err := requireCurrentTaskAuthorization(ctx, tx, taskRow)
			if err != nil {
				return err
			}
			if !authorized {
				if err := requestTaskCancellationForAuthorization(ctx, tx, id, taskOwnerID); err != nil {
					return err
				}
				return nil
			}
		}
		if taskRow.Status == string(task.StatusWaitingUser) && taskRow.WaitingReason == task.WaitingReasonWorkspaceHostSync && !projectCheckpointRecovery {
			ready, err := workspaceTaskHostSyncReady(ctx, tx, taskRow)
			if err != nil {
				return err
			}
			if !ready {
				return nil
			}
		}
		if taskRow.Status == string(task.StatusWaitingUser) && taskRow.WaitingReason == "task_hub_workspace_sync" {
			var ready bool
			if err := tx.QueryRow(ctx, `SELECT `+taskHubWorkspaceSyncClaimPredicate+`
				FROM tasks AS t WHERE t.id=$1 AND t.owner_id=$2`, id, taskOwnerID).Scan(&ready); err != nil {
				return fmt.Errorf("recheck Task Hub Workspace sync before claim: %w", err)
			}
			if !ready {
				return nil
			}
		}
		var conversationContext *task.ConversationContext
		// A personal checkpoint may contain an earlier personal-data proof. The
		// original fixed context must also be restored on lease recovery so the
		// runtime cannot silently skip its privacy validation.
		if taskRow.CheckpointThroughSeq < 0 || (!taskRow.ProjectID.Valid && taskRow.InitiatorID == taskRow.OwnerID) {
			conversationContext, err = readTaskConversationContext(ctx, tx, rowTaskDomain(taskRow))
			if errors.Is(err, errTaskContextSnapshotUnavailable) {
				const explanation = "此任务的历史上下文无法恢复，请重新发送消息。"
				if _, err := tx.Exec(ctx, `UPDATE tasks SET result=CASE WHEN result='' THEN $3 ELSE result || E'\n\n' || $3 END
WHERE id=$1 AND owner_id=$2`, id, taskOwnerID, explanation); err != nil {
					return fmt.Errorf("record unavailable conversation snapshot: %w", err)
				}
				// Reuse the fenced stop path so old processes and uncertain operations settle safely.
				if err := requestTaskCancellationForAuthorization(ctx, tx, id, taskOwnerID); err != nil {
					return err
				}
				if err := markPendingTaskConversationMessagesUnknown(ctx, tx, id, taskOwnerID, ""); err != nil {
					return fmt.Errorf("mark unresolved task messages unknown during snapshot recovery: %w", err)
				}
				sequence, err := incrementTaskEventSequence(ctx, tx, id)
				if err != nil {
					return err
				}
				payload, err := json.Marshal(map[string]string{"status": "cancelling", "reason": "conversation_snapshot_unavailable", "message": explanation})
				if err != nil {
					return fmt.Errorf("encode unavailable conversation snapshot event: %w", err)
				}
				return insertTaskEvent(ctx, tx, task.Event{TaskID: id, Sequence: sequence, Type: "task.status", Payload: payload})
			}
			if err != nil {
				return err
			}
		}
		dependencyResults, err := loadTaskDependencyResultsInTx(ctx, tx, id, taskOwnerID)
		if err != nil {
			return err
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
		if oldVersion > 0 {
			if err := interruptAssistantMessageSnapshot(ctx, tx, id, "", true); err != nil {
				return err
			}
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
		err = tx.QueryRow(ctx, `UPDATE tasks SET status='running', waiting_reason='', lease_owner=$2, lease_version=$3,
lease_expires_at=clock_timestamp()+($4::bigint * interval '1 millisecond'),
runtime_lease_safe_after=GREATEST(COALESCE(runtime_lease_safe_after, '-infinity'::timestamptz), clock_timestamp()+($4::bigint * interval '1 millisecond')),
updated_at=clock_timestamp(),
event_seq=event_seq+1
WHERE id=$1 AND current_revision=desired_revision RETURNING event_seq`, id, worker, newVersion, leaseMillis).Scan(&eventSequence)
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
		row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: id, OwnerID: taskOwnerID})
		if err != nil {
			return fmt.Errorf("read claimed task: %w", err)
		}
		claimed = rowTaskDomain(row)
		claimed.ConversationContext = conversationContext
		claimed.DependencyResults = dependencyResults
		stepRetry, err := loadClaimedStepRetryIntent(ctx, tx, claimed.ID, claimed.CurrentRevision)
		if err != nil {
			return err
		}
		claimed.StepRetry = stepRetry
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
		var ownerID, initiatorID string
		if err := tx.QueryRow(ctx, `SELECT owner_id,initiator_id FROM tasks WHERE id=$1`, taskID).Scan(&ownerID, &initiatorID); errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		} else if err != nil {
			return fmt.Errorf("read task owner before lease renewal: %w", err)
		}
		projectWorkspaceFence, err := lockProjectTaskWorkspaceFence(ctx, tx, taskID, false)
		if err != nil {
			return err
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, initiatorID, nil)
		if err != nil {
			return err
		}
		taskRow, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: taskID, OwnerID: ownerID})
		if err != nil {
			return fmt.Errorf("read task authorization before lease renewal: %w", err)
		}
		if taskRow.WorkspaceID.Valid && !taskRow.ProjectID.Valid {
			if err := lockTaskWorkspaceForExecution(ctx, tx, taskRow.WorkspaceID.String, taskRow.InitiatorID); err != nil {
				return err
			}
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
		if err := verifyProjectTaskWorkspaceFence(ctx, tx, taskID, projectWorkspaceFence); err != nil {
			return err
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
	if !domain.ValidUserID(value.InitiatorID) {
		return false, nil
	}
	if projectWorkspaceAuthorizationPermit(ctx, value) {
		return requireCurrentTaskCredentialAuthorization(ctx, tx, value, false)
	}
	projectID, err := lockTaskInitiatorInTx(ctx, tx, value.ID, value.OwnerID, value.InitiatorID)
	if err != nil {
		switch domain.CodeOf(err) {
		case domain.CodeUnauthenticated, domain.CodeForbidden, domain.CodeNotFound:
			return false, nil
		default:
			return false, err
		}
	}
	if value.ProjectID.Valid != (projectID != "") || value.ProjectID.Valid && value.ProjectID.String != projectID {
		return false, nil
	}
	return requireCurrentTaskCredentialAuthorization(ctx, tx, value, true)
}

// A verified project Workspace permit already holds these rows in the outer
// transaction; ordinary callers lock them here as before.
func requireCurrentTaskCredentialAuthorization(ctx context.Context, tx pgx.Tx, value sqlcgen.Task, lockRows bool) (bool, error) {
	lock := ""
	if lockRows {
		lock = " FOR SHARE"
	}
	var approved bool
	err := tx.QueryRow(ctx, `SELECT status='approved' FROM users WHERE id=$1`+lock, value.InitiatorID).Scan(&approved)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !approved {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check task authorization account: %w", err)
	}
	if value.AuthKind == string(identity.KindScheduleTask) {
		if !scheduleTaskScopeStringsValidForHost(value.AuthScopes, value.HostID.Valid) || !schedule.ValidRunID(value.AuthTokenID) ||
			value.AuthClientInstallationID.Valid {
			return false, nil
		}
		var taskHostID any
		if value.HostID.Valid {
			taskHostID = value.HostID.String
		}
		var linked bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM schedule_runs r
  JOIN schedules s ON s.id=r.schedule_id AND s.owner_id=r.owner_id
  JOIN tasks t ON t.id=r.task_id AND t.owner_id=r.owner_id AND t.initiator_id=r.owner_id
  WHERE r.id=$1 AND r.owner_id=$2 AND r.task_id=$3 AND t.auth_kind='schedule_task' AND t.auth_token_id=r.id
    AND t.execution_location='server' AND s.host_id IS NOT DISTINCT FROM t.host_id
    AND t.host_id IS NOT DISTINCT FROM $4::text
)`, value.AuthTokenID, value.InitiatorID, value.ID, taskHostID).Scan(&linked)
		if err != nil {
			return false, fmt.Errorf("check scheduled Task run link: %w", err)
		}
		return linked, nil
	}
	var kind string
	var installationID pgtype.Text
	var expiresAt time.Time
	var revokedAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `SELECT kind, client_installation_id, expires_at, revoked_at
FROM tokens WHERE id=$1 AND owner_id=$2`+lock, value.AuthTokenID, value.InitiatorID).
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
WHERE id=$1 AND owner_id=$2`+lock, installationID.String, value.InitiatorID).Scan(&expiresAt, &revokedAt)
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

func workspaceTaskHostSyncReady(ctx context.Context, tx pgx.Tx, value sqlcgen.Task) (bool, error) {
	if !value.HostID.Valid || !value.WorkspaceID.Valid || !value.WorkspaceCheckpointRevision.Valid {
		return false, nil
	}
	expectedOwner := value.InitiatorID
	if value.ProjectID.Valid {
		if !value.WorkspaceOwnerID.Valid || value.WorkspaceOwnerID.String == "" {
			return false, nil
		}
		expectedOwner = value.WorkspaceOwnerID.String
	}
	var hostRunning bool
	var runtimeGeneration int64
	if err := tx.QueryRow(ctx, `SELECT state='running' AND deleted_at IS NULL, runtime_generation FROM hosts WHERE id=$1 AND owner_id=$2 FOR SHARE`, value.HostID.String, expectedOwner).
		Scan(&hostRunning, &runtimeGeneration); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("check target Host before resuming Workspace Task: %w", err)
	}
	if !hostRunning {
		return false, nil
	}
	var workspaceOwner, workspaceState string
	var workspaceRevision int64
	if err := tx.QueryRow(ctx, `SELECT owner_id,state,revision FROM workspaces WHERE id=$1 FOR SHARE`, value.WorkspaceID.String).
		Scan(&workspaceOwner, &workspaceState, &workspaceRevision); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("lock Workspace before resuming Workspace Task: %w", err)
	}
	if workspaceOwner != expectedOwner {
		return false, nil
	}
	if err := requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, value.WorkspaceID.String); err != nil {
		if domain.CodeOf(err) == domain.CodeWorkspaceBusy {
			return false, nil
		}
		return false, err
	}
	var bindingState, syncState string
	var desiredRevision, appliedRevision int64
	if err := tx.QueryRow(ctx, `SELECT state, sync_state, desired_revision, applied_revision FROM workspace_bindings
WHERE host_id=$1 AND workspace_id=$2 FOR SHARE`, value.HostID.String, value.WorkspaceID.String).
		Scan(&bindingState, &syncState, &desiredRevision, &appliedRevision); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("lock Host Workspace binding before resuming Task: %w", err)
	}
	var heartbeatFresh bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM host_runtime_heartbeats hb
  WHERE hb.host_id=$1 AND hb.runtime_generation=$2
    AND hb.last_seen_at > clock_timestamp() - $3::double precision * interval '1 second'
)`, value.HostID.String, runtimeGeneration, host.RuntimeHeartbeatFreshness.Seconds()).Scan(&heartbeatFresh); err != nil {
		return false, fmt.Errorf("check Host Runtime heartbeat before resuming Workspace Task: %w", err)
	}
	return workspaceOwner == expectedOwner && workspaceState == string(workspace.StateReady) &&
		workspaceRevision == value.WorkspaceCheckpointRevision.Int64 && bindingState == string(workspace.BindingActive) &&
		syncState == string(workspace.BindingSyncHealthy) &&
		desiredRevision >= value.WorkspaceCheckpointRevision.Int64 && appliedRevision >= value.WorkspaceCheckpointRevision.Int64 && heartbeatFresh, nil
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
		if err := interruptAssistantMessageSnapshot(ctx, tx, taskID, "", true); err != nil {
			return err
		}
		if err := requestActiveTaskAgentChildCancellations(ctx, tx, taskID, ownerID); err != nil {
			return err
		}
		if err := settleStoppedCurrentTaskAgentChildrenIfProven(ctx, tx, taskID, ownerID); err != nil {
			return err
		}
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
	if err := interruptAssistantMessageSnapshot(ctx, tx, taskID, "", true); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_stop_requests (id, task_id, action, status, safe_after)
VALUES ($1,$2,'cancel','pending',$3)`, requestID, taskID, safeAfter); err != nil {
		return fmt.Errorf("persist task authorization cancellation: %w", err)
	}
	if err := insertTaskStatusEvent(ctx, tx, taskID, sequence, task.StatusCancelling, task.ActionCancel, requestID); err != nil {
		return err
	}
	if err := requestActiveTaskAgentChildCancellations(ctx, tx, taskID, ownerID); err != nil {
		return err
	}
	return settleStoppedCurrentTaskAgentChildrenIfProven(ctx, tx, taskID, ownerID)
}

func stopTasksForAuthorization(ctx context.Context, tx pgx.Tx, actorID, tokenID, installationID string) (int, error) {
	rows, err := tx.Query(ctx, `SELECT t.id,t.owner_id FROM tasks t
WHERE t.initiator_id=$1 AND t.status IN ('queued','running','waiting_user','pausing','paused','cancelling')
  AND (($2::text <> '' AND t.auth_token_id=$2) OR ($3::text <> '' AND t.auth_client_installation_id=$3))
ORDER BY t.id FOR UPDATE OF t`, actorID, tokenID, installationID)
	if err != nil {
		return 0, fmt.Errorf("lock tasks for authorization revocation: %w", err)
	}
	type taskOwner struct{ id, ownerID string }
	ids := make([]taskOwner, 0)
	for rows.Next() {
		var value taskOwner
		if err := rows.Scan(&value.id, &value.ownerID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("read task for authorization revocation: %w", err)
		}
		ids = append(ids, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate tasks for authorization revocation: %w", err)
	}
	rows.Close()
	for _, value := range ids {
		if err := requestTaskCancellationForAuthorization(ctx, tx, value.id, value.ownerID); err != nil {
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
	if progress.ToolCall != nil {
		call := progress.ToolCall
		if len(progress.Steps) != 1 || call.AgentSessionID == "" || call.AgentInstanceID == "" || call.AgentGeneration < 1 ||
			call.AuthorizationVersion < 1 || call.TaskRevision < 0 || call.CallKey == "" || len(call.CallKey) > 256 ||
			!utf8.ValidString(call.CallKey) || strings.ContainsRune(call.CallKey, '\x00') || call.ToolName == "" || len(call.ToolName) > 128 ||
			!utf8.ValidString(call.ToolName) || strings.ContainsRune(call.ToolName, '\x00') || len(call.RequestDigest) != 32 ||
			(call.DetailState != "not_recorded" && call.DetailState != "available") || call.Phase == "" || len(call.Phase) > 64 ||
			!utf8.ValidString(call.Phase) || strings.ContainsRune(call.Phase, '\x00') {
			return task.Event{}, domain.InvalidArgument("tool_call", "task tool call progress is invalid")
		}
		if (call.FailureReason != "" && call.FailureReason != "not_dispatched") ||
			(call.Retryable && (call.FailureReason != "not_dispatched" || call.Phase != "prepare" || progress.Steps[0].Status != task.StepFailed)) ||
			(call.FailureReason == "not_dispatched" && (call.Phase != "prepare" || progress.Steps[0].Status != task.StepFailed)) {
			return task.Event{}, domain.InvalidArgument("tool_call.failure", "task tool failure evidence is invalid")
		}
		if call.DetailState == "available" {
			if len(call.DetailCiphertext) < 29 || len(call.DetailCiphertext) > 65564 || len(call.DetailSHA256) != 32 ||
				call.DetailSizeBytes < 1 || call.DetailSizeBytes > 65536 || len(progress.Steps[0].ID) == 0 {
				return task.Event{}, domain.InvalidArgument("tool_call.detail", "task tool detail is invalid")
			}
		} else if len(call.DetailCiphertext) != 0 || len(call.DetailSHA256) != 0 || call.DetailSizeBytes != 0 {
			return task.Event{}, domain.InvalidArgument("tool_call.detail", "unavailable task tool detail must not include a payload")
		}
		step := progress.Steps[0]
		taskRevision := call.TaskRevision
		step.AgentSessionID, step.TaskRevision = call.AgentSessionID, &taskRevision
		step.ToolCallKey, step.ToolName = call.CallKey, call.ToolName
		step.RequestDigest, step.Phase, step.DetailState = append([]byte(nil), call.RequestDigest...), call.Phase, call.DetailState
		step.FailureReason, step.Retryable = call.FailureReason, call.Retryable
		if step.Attempt < 1 {
			step.Attempt = 1
		}
		progress.Steps[0] = step
	}
	if pending := progress.PendingRequest; pending != nil {
		hostlessWorkspaceApproval := app.IsHostlessWorkspaceWriteApproval(*pending)
		if !domain.ValidPendingRequestID(pending.ID) || pending.TaskID != progress.TaskID || pending.Version < 1 || pending.Kind != "approval" ||
			pending.Status != task.PendingRequestPending || pending.Decision != "" || len(pending.RequestHash) != 32 ||
			pending.RuntimeGeneration < 0 || (!hostlessWorkspaceApproval && pending.RuntimeInstanceID == "") || pending.ControlVersion < 0 || pending.CallID == "" || len(pending.CallID) > 256 ||
			pending.ToolName == "" || len(pending.ToolName) > 128 || !validPendingText(pending.Title) || !validPendingText(pending.Object) ||
			!validPendingText(pending.Content) || !validPendingText(pending.Impact) || progress.Status != task.StatusWaitingUser || progress.WaitingReason != "approval_required" {
			return task.Event{}, domain.InvalidArgument("pending_request", "pending approval request fields are invalid")
		}
	}
	if media := progress.TaskMediaApproval; media != nil {
		pending := progress.PendingRequest
		if pending == nil || pending.MediaPreviewOperationID != media.OperationID || pending.TaskID != progress.TaskID ||
			!domain.ValidOperationID(media.OperationID) || len(media.RuntimeCallID) < 8 || len(media.RuntimeCallID) > 128 ||
			!utf8.ValidString(media.RuntimeCallID) || strings.ContainsRune(media.RuntimeCallID, '\x00') ||
			len(media.ProviderOperationID) > 256 || !utf8.ValidString(media.ProviderOperationID) ||
			strings.ContainsRune(media.ProviderOperationID, '\x00') || len(media.OperationResult) < 2 || len(media.OperationResult) > 4096 ||
			!utf8.ValidString(media.OperationResult) || !json.Valid([]byte(media.OperationResult)) || !validTaskMediaType(media.MediaType) ||
			media.SizeBytes < 1 || media.SizeBytes > workspace.MaxSyncFileBytes || len(media.SHA256) != 32 ||
			media.ExpiresAt.IsZero() || !media.ExpiresAt.After(time.Now()) {
			return task.Event{}, domain.InvalidArgument("task_media_approval", "task media approval commit fields are invalid")
		}
	} else if progress.PendingRequest != nil && progress.PendingRequest.MediaPreviewOperationID != "" {
		return task.Event{}, domain.InvalidArgument("task_media_approval", "media approval requires its staged object commit")
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
	if progress.PublishResultMessage && (progress.Status != task.StatusCompleted || progress.Event.Type != "task.result" ||
		progress.AssistantMessage != nil || !domain.ValidStepID(progress.CurrentStep) || strings.TrimSpace(progress.Result) == "" ||
		len(progress.Result) > app.MaxAssistantMessageBytes || !utf8.ValidString(progress.Result) || strings.ContainsRune(progress.Result, '\x00')) {
		return task.Event{}, domain.InvalidArgument("assistant_message", "completed task result is invalid")
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
	if personalMutation := progress.CloudDSHPersonalMutation; personalMutation != nil {
		if progress.CloudDSHSkillRead != nil || progress.Status != task.StatusRunning || progress.Event.Type != "task.step" ||
			!app.ValidCloudDSHPersonalMutationCommit(*personalMutation) ||
			app.ValidateCloudDSHPersonalMutationEvent(progress.Event.Payload, *personalMutation) != nil {
			return task.Event{}, domain.InvalidArgument("personal_mutation", "personal mutation progress is invalid")
		}
	}
	var result task.Event
	var authorizationDenied bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		projectWorkspaceFence, err := lockProjectTaskWorkspaceFence(ctx, tx, progress.TaskID, false)
		if err != nil && projectWorkspaceFence.projectID != "" &&
			(domain.CodeOf(err) == domain.CodeNotFound || domain.CodeOf(err) == domain.CodeForbidden) {
			// Serialize against a concurrent re-invite before deciding that the
			// project Task has permanently lost its execution authorization.
			var projectID string
			lockErr := tx.QueryRow(ctx, `SELECT id FROM projects WHERE id=$1 FOR SHARE`, projectWorkspaceFence.projectID).Scan(&projectID)
			if lockErr != nil && !errors.Is(lockErr, pgx.ErrNoRows) {
				return fmt.Errorf("lock project before cancelling unauthorized task: %w", lockErr)
			}
			if lockErr == nil {
				projectWorkspaceFence, err = lockProjectTaskWorkspaceFence(ctx, tx, progress.TaskID, false)
			}
			if errors.Is(lockErr, pgx.ErrNoRows) || err != nil &&
				(domain.CodeOf(err) == domain.CodeNotFound || domain.CodeOf(err) == domain.CodeForbidden) {
				var ownerID string
				if err := tx.QueryRow(ctx, `SELECT owner_id FROM tasks WHERE id=$1`, progress.TaskID).Scan(&ownerID); err != nil {
					return fmt.Errorf("read owner for unauthorized project Task: %w", err)
				}
				if err := requestTaskCancellationForAuthorization(ctx, tx, progress.TaskID, ownerID); err != nil {
					return err
				}
				authorizationDenied = true
				return nil
			}
		}
		if err != nil {
			return err
		}
		if message := progress.AssistantMessage; message != nil {
			// Message acceptance locks the conversation first; use the same order so
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
		} else if progress.PublishResultMessage {
			var locked string
			err := tx.QueryRow(ctx, `SELECT c.id FROM conversations c JOIN tasks t
ON t.conversation_id=c.id AND t.owner_id=c.owner_id
WHERE t.id=$1 FOR UPDATE OF c`, progress.TaskID).Scan(&locked)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "task conversation not found")
			}
			if err != nil {
				return fmt.Errorf("lock completed task conversation: %w", err)
			}
		}
		var current sqlcgen.Task
		err = tx.QueryRow(ctx, `SELECT id, owner_id, conversation_id, project_id, initiator_id, host_id, model_id, objective, status, auth_kind, auth_token_id, auth_scopes, auth_client_installation_id,
budget_active_seconds, budget_tool_calls, budget_model_tokens, usage_active_seconds, usage_tool_calls, usage_model_tokens, lease_owner,
lease_version, lease_expires_at, event_seq, current_step, result, waiting_reason, created_at, updated_at, finished_at, application_id,
execution_location, agent_session_id, auth_version, auth_expires_at, workspace_id, input_revision, workspace_checkpoint_revision,
agent_instance_id,agent_generation,
workspace_owner_id,workspace_project_resource_version
FROM tasks WHERE id=$1 FOR UPDATE`, progress.TaskID).Scan(
			&current.ID, &current.OwnerID, &current.ConversationID, &current.ProjectID, &current.InitiatorID, &current.HostID, &current.ModelID, &current.Objective, &current.Status,
			&current.AuthKind, &current.AuthTokenID, &current.AuthScopes, &current.AuthClientInstallationID, &current.BudgetActiveSeconds, &current.BudgetToolCalls,
			&current.BudgetModelTokens, &current.UsageActiveSeconds, &current.UsageToolCalls, &current.UsageModelTokens,
			&current.LeaseOwner, &current.LeaseVersion, &current.LeaseExpiresAt, &current.EventSeq, &current.CurrentStep,
			&current.Result, &current.WaitingReason, &current.CreatedAt, &current.UpdatedAt, &current.FinishedAt, &current.ApplicationID,
			&current.ExecutionLocation, &current.AgentSessionID, &current.AuthVersion, &current.AuthExpiresAt, &current.WorkspaceID, &current.InputRevision, &current.WorkspaceCheckpointRevision,
			&current.AgentInstanceID, &current.AgentGeneration,
			&current.WorkspaceOwnerID, &current.WorkspaceProjectResourceVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("lock task for progress: %w", err)
		}
		if err := verifyProjectTaskWorkspaceFence(ctx, tx, progress.TaskID, projectWorkspaceFence); err != nil {
			return err
		}
		if pending := progress.PendingRequest; pending != nil && app.IsHostlessWorkspaceWriteApproval(*pending) &&
			(current.HostID.Valid || current.ExecutionLocation != "server" || !current.WorkspaceID.Valid) {
			return domain.InvalidArgument("pending_request", "Hostless Workspace approval requires a server Task with a Workspace and no Host")
		}
		var leaseValid, revisionReady bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE(lease_expires_at > clock_timestamp(), false),current_revision=desired_revision FROM tasks WHERE id=$1`, progress.TaskID).Scan(&leaseValid, &revisionReady); err != nil {
			return fmt.Errorf("check task lease: %w", err)
		}
		if current.LeaseOwner != progress.LeaseOwner || current.LeaseVersion != progress.LeaseVersion || !leaseValid || !revisionReady || current.Status != string(task.StatusRunning) {
			return taskLeaseConflict()
		}
		currentTask := rowTaskDomain(current)
		if call := progress.ToolCall; call != nil {
			var currentRevision int64
			if err := tx.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1`, current.ID).Scan(&currentRevision); err != nil {
				return fmt.Errorf("read task revision for tool call: %w", err)
			}
			if currentRevision != call.TaskRevision || current.AgentInstanceID != call.AgentInstanceID || current.AgentGeneration != call.AgentGeneration ||
				current.AgentGeneration < 1 || current.AgentSessionID == "" || current.AuthVersion != call.AuthorizationVersion {
				return domain.NewError(domain.CodeUnauthenticated, "task tool call fence changed")
			}
			var liveInstance bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM task_agent_instances ai
WHERE ai.task_id=$1 AND ai.owner_id=$2 AND ai.agent_instance_id=$3 AND ai.agent_generation=$4 AND ai.agent_session_id=$5
  AND ai.lease_owner=$6 AND ai.lease_version=$7 AND ai.auth_version=$8 AND ai.state='active'
  AND ai.lease_expires_at > clock_timestamp())`, current.ID, current.OwnerID, call.AgentInstanceID, call.AgentGeneration,
				current.AgentSessionID, progress.LeaseOwner, progress.LeaseVersion, call.AuthorizationVersion).Scan(&liveInstance); err != nil {
				return fmt.Errorf("check live Agent instance for tool call: %w", err)
			}
			if !liveInstance {
				return taskLeaseConflict()
			}
			underRoot, err := agentSessionDescendsFrom(ctx, tx, current.ID, current.OwnerID, call.AgentSessionID, current.AgentSessionID)
			if err != nil {
				return err
			}
			if !underRoot {
				return domain.NewError(domain.CodeUnauthenticated, "tool call Agent session is outside the task execution tree")
			}
			var sessionRevision int64
			if err := tx.QueryRow(ctx, `SELECT task_revision FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2 AND session_id=$3`,
				current.ID, current.OwnerID, call.AgentSessionID).Scan(&sessionRevision); err != nil {
				return domain.NewError(domain.CodeUnauthenticated, "tool call Agent session is unavailable")
			}
			if call.AgentSessionID != current.AgentSessionID && sessionRevision != currentRevision {
				return domain.NewError(domain.CodeUnauthenticated, "tool call Agent session revision changed")
			}
			var existingStepID string
			var existingToolName string
			var existingRevision int64
			var existingDigest []byte
			var existingAttempt int64
			var existingRetryOf string
			err = tx.QueryRow(ctx, `SELECT id,tool_name,task_revision,request_digest,attempt_no,COALESCE(retry_of_step_id,'') FROM task_steps
WHERE task_id=$1 AND agent_session_id=$2 AND tool_call_key=$3 FOR UPDATE`, current.ID, call.AgentSessionID, call.CallKey).
				Scan(&existingStepID, &existingToolName, &existingRevision, &existingDigest, &existingAttempt, &existingRetryOf)
			if err == nil {
				if existingStepID != progress.Steps[0].ID || existingToolName != call.ToolName || existingRevision != call.TaskRevision ||
					!bytes.Equal(existingDigest, call.RequestDigest) {
					return domain.NewError(domain.CodeIdempotencyConflict, "tool call identity was reused with different inputs")
				}
				progress.Steps[0].Attempt, progress.Steps[0].RetryOfStepID = existingAttempt, existingRetryOf
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read existing task tool call step: %w", err)
			} else if progress.Steps[0].Attempt < 1 {
				progress.Steps[0].Attempt = 1
			}
		}
		if current.WorkspaceID.Valid && !current.ProjectID.Valid {
			if err := lockTaskWorkspaceForExecution(ctx, tx, current.WorkspaceID.String, current.InitiatorID); err != nil {
				return err
			}
		}
		var executionEnabled bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM owner_execution_controls ec JOIN tasks t ON t.initiator_id=ec.owner_id
WHERE t.id=$1 AND t.owner_id=$2 AND ec.state='enabled' AND ec.epoch=t.execution_epoch
)`, current.ID, current.OwnerID).Scan(&executionEnabled); err != nil {
			return fmt.Errorf("check task initiator execution gate before progress: %w", err)
		}
		if !executionEnabled {
			return taskLeaseConflict()
		}
		authorized, err := requireCurrentTaskAuthorization(ctx, tx, current)
		if err != nil {
			return err
		}
		if !authorized {
			if err := requestTaskCancellationForAuthorization(ctx, tx, current.ID, current.OwnerID); err != nil {
				return err
			}
			authorizationDenied = true
			return nil
		}
		if media := progress.TaskMediaApproval; media != nil {
			if err := persistTaskMediaApprovalCommit(ctx, tx, current, progress, *media); err != nil {
				return err
			}
		}
		if personalMutation := progress.CloudDSHPersonalMutation; personalMutation != nil {
			existing, found, err := validateCloudDSHPersonalMutationFence(ctx, tx, currentTask, progress, *personalMutation)
			if err != nil {
				return err
			}
			if found {
				result = existing
				return nil
			}
		}
		if skillRead := progress.CloudDSHSkillRead; skillRead != nil {
			if current.ID != skillRead.TaskID || current.OwnerID != skillRead.OwnerID || currentTask.ExecutionLocation != "server" ||
				currentTask.AgentSessionID != skillRead.AgentSessionID ||
				currentTask.Authorization.Version != skillRead.AuthorizationVersion || progress.Status != task.StatusRunning ||
				progress.Event.Type != "task.step" || !validCloudDSHSkillReadCommit(*skillRead) {
				return domain.NewError(domain.CodeInvalidState, "cloud dsh skill authorization or identity changed")
			}
			var authorizationValid bool
			if err := tx.QueryRow(ctx, `SELECT auth_kind='schedule_task' OR auth_expires_at > clock_timestamp() FROM tasks WHERE id=$1`, progress.TaskID).Scan(&authorizationValid); err != nil {
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
		var workspaceRuntimeGeneration int64
		if progress.Status == task.StatusCompleted && current.WorkspaceID.Valid {
			authorized, err := requireCurrentTaskAuthorization(ctx, tx, current)
			if err != nil {
				return err
			}
			if !authorized {
				if err := requestTaskCancellationForAuthorization(ctx, tx, progress.TaskID, current.OwnerID); err != nil {
					return err
				}
				authorizationDenied = true
				return nil
			}
			if !current.WorkspaceCheckpointRevision.Valid || (!current.HostID.Valid && !current.ProjectID.Valid) {
				return domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace checkpoint is unavailable")
			}
			expectedWorkspaceOwner := current.InitiatorID
			if current.ProjectID.Valid {
				if !current.WorkspaceOwnerID.Valid {
					return domain.NewError(domain.CodeWorkspaceConflict, "project Workspace owner is unavailable")
				}
				expectedWorkspaceOwner = current.WorkspaceOwnerID.String
			}
			var hostReady bool
			if current.HostID.Valid {
				err = tx.QueryRow(ctx, `SELECT h.state='running' AND h.deleted_at IS NULL AND EXISTS (
  SELECT 1 FROM host_runtime_heartbeats hb WHERE hb.host_id=h.id AND hb.runtime_generation=h.runtime_generation
    AND hb.last_seen_at > clock_timestamp() - $3::double precision * interval '1 second'
), h.runtime_generation FROM hosts AS h WHERE h.id=$1 AND h.owner_id=$2 FOR SHARE`,
					current.HostID.String, expectedWorkspaceOwner, host.RuntimeHeartbeatFreshness.Seconds()).Scan(&hostReady, &workspaceRuntimeGeneration)
				if errors.Is(err, pgx.ErrNoRows) {
					err = nil
				}
				if err != nil {
					return fmt.Errorf("lock target Host before task completion: %w", err)
				}
			}
			var workspaceOwner string
			var revision int64
			err = tx.QueryRow(ctx, `SELECT owner_id,revision FROM workspaces WHERE id=$1 FOR SHARE`, current.WorkspaceID.String).Scan(&workspaceOwner, &revision)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace was removed after this task input was confirmed")
			}
			if err != nil {
				return fmt.Errorf("read Workspace before task completion: %w", err)
			}
			if workspaceOwner != expectedWorkspaceOwner || revision != current.WorkspaceCheckpointRevision.Int64 {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed after this task checkpoint was confirmed")
			}
			if current.HostID.Valid {
				appliedRevision, desiredRevision := int64(-1), int64(-1)
				var bindingOwner, bindingState, syncState string
				err = tx.QueryRow(ctx, `SELECT owner_id,applied_revision, desired_revision,state,sync_state FROM workspace_bindings
WHERE host_id=$1 AND workspace_id=$2 FOR SHARE`, current.HostID.String, current.WorkspaceID.String).
					Scan(&bindingOwner, &appliedRevision, &desiredRevision, &bindingState, &syncState)
				if errors.Is(err, pgx.ErrNoRows) {
					err = nil
				}
				if err != nil {
					return fmt.Errorf("read target Host Workspace binding receipt: %w", err)
				}
				var heartbeatFresh bool
				err = tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM host_runtime_heartbeats hb
  WHERE hb.host_id=$1 AND hb.runtime_generation=$2
    AND hb.last_seen_at > clock_timestamp() - $3::double precision * interval '1 second'
)`, current.HostID.String, workspaceRuntimeGeneration, host.RuntimeHeartbeatFreshness.Seconds()).Scan(&heartbeatFresh)
				if err != nil {
					return fmt.Errorf("recheck Host Runtime heartbeat before task completion: %w", err)
				}
				if !hostReady || !heartbeatFresh || bindingOwner != expectedWorkspaceOwner || bindingState != string(workspace.BindingActive) ||
					syncState != string(workspace.BindingSyncHealthy) || desiredRevision < current.WorkspaceCheckpointRevision.Int64 || appliedRevision < current.WorkspaceCheckpointRevision.Int64 {
					progress.Status = task.StatusWaitingUser
					progress.WaitingReason = task.WaitingReasonWorkspaceHostSync
					progress.Result = current.Result
					progress.Artifacts = nil
					progress.Event.Type = "task.waiting_user"
					for i := range progress.Steps {
						if progress.Steps[i].ID == progress.CurrentStep {
							progress.Steps[i].Status = task.StepWaitingUser
							progress.Steps[i].Summary = "Waiting for the target Host to apply the saved Workspace revision."
						}
					}
					eventPayload, err = json.Marshal(map[string]any{
						"status": string(task.StatusWaitingUser), "reason": task.WaitingReasonWorkspaceHostSync,
						"required_revision": current.WorkspaceCheckpointRevision.Int64, "applied_revision": appliedRevision,
					})
					if err != nil {
						return fmt.Errorf("encode Workspace Host sync wait event: %w", err)
					}
				}
			}
		}
		switch progress.Status {
		case task.StatusCompleted:
			var pendingMessages bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM conversation_messages
WHERE task_id=$1 AND owner_id=$2 AND conversation_id=$3 AND role='user' AND delivery_status='pending'
)`, current.ID, current.OwnerID, current.ConversationID).Scan(&pendingMessages); err != nil {
				return fmt.Errorf("check pending user messages before task completion: %w", err)
			}
			if pendingMessages {
				return domain.NewError(domain.CodeConflict, "task has pending user messages")
			}
		case task.StatusFailed, task.StatusCancelled:
			if err := markPendingTaskConversationMessagesUnknown(ctx, tx, current.ID, current.OwnerID, current.ConversationID); err != nil {
				return fmt.Errorf("mark undelivered user messages unknown on Task stop: %w", err)
			}
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
		if personalMutation := progress.CloudDSHPersonalMutation; personalMutation != nil {
			if err := applyCloudDSHPersonalMutationInTx(ctx, tx, currentTask, *personalMutation, eventPayload); err != nil {
				return err
			}
		}
		stepIDs := make(map[string]struct{}, len(progress.Steps))
		stepSequences := make(map[int64]struct{}, len(progress.Steps))
		if err := assignTaskStepSequences(ctx, tx, progress.TaskID, progress.Steps); err != nil {
			return err
		}
		eventPayload, err = alignTaskStepEventSequence(eventPayload, progress.Steps)
		if err != nil {
			return err
		}
		for index := range progress.Steps {
			step := progress.Steps[index]
			if step.DetailState == "" {
				step.DetailState = "not_recorded"
				progress.Steps[index].DetailState = step.DetailState
			}
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
			if step.Attempt < 1 {
				step.Attempt = 1
			}
			result, err := tx.Exec(ctx, `INSERT INTO task_steps
(id,task_id,sequence,kind,status,title,summary,agent_session_id,task_revision,tool_call_key,tool_name,request_digest,phase,detail_state,
 attempt_no,retry_of_step_id,failure_reason,retryable)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
ON CONFLICT (id) DO UPDATE SET status=EXCLUDED.status, title=EXCLUDED.title, summary=EXCLUDED.summary,
 agent_session_id=COALESCE(task_steps.agent_session_id,EXCLUDED.agent_session_id),
 task_revision=COALESCE(task_steps.task_revision,EXCLUDED.task_revision),
 tool_call_key=COALESCE(task_steps.tool_call_key,EXCLUDED.tool_call_key),
 tool_name=COALESCE(task_steps.tool_name,EXCLUDED.tool_name),
 request_digest=COALESCE(task_steps.request_digest,EXCLUDED.request_digest),
phase=CASE WHEN EXCLUDED.agent_session_id IS NOT NULL THEN EXCLUDED.phase ELSE task_steps.phase END,
failure_reason=EXCLUDED.failure_reason,
retryable=EXCLUDED.retryable,
detail_state=CASE WHEN EXCLUDED.agent_session_id IS NOT NULL
  THEN CASE WHEN EXCLUDED.detail_state='not_recorded' AND task_steps.detail_state='available' THEN 'available' ELSE EXCLUDED.detail_state END
  ELSE task_steps.detail_state END
WHERE task_steps.task_id=EXCLUDED.task_id AND task_steps.sequence=EXCLUDED.sequence
 AND (task_steps.agent_session_id IS NULL OR EXCLUDED.agent_session_id IS NULL OR
   (task_steps.agent_session_id=EXCLUDED.agent_session_id AND task_steps.task_revision=EXCLUDED.task_revision
    AND task_steps.tool_call_key=EXCLUDED.tool_call_key AND task_steps.tool_name=EXCLUDED.tool_name
		    AND task_steps.request_digest=EXCLUDED.request_digest AND task_steps.attempt_no=EXCLUDED.attempt_no
		    AND task_steps.retry_of_step_id IS NOT DISTINCT FROM EXCLUDED.retry_of_step_id))`,
				step.ID, progress.TaskID, step.Sequence, step.Kind, string(step.Status), step.Title, step.Summary,
				nullableText(step.AgentSessionID), step.TaskRevision, nullableText(step.ToolCallKey), nullableText(step.ToolName),
				step.RequestDigest, nullableText(step.Phase), step.DetailState, step.Attempt, nullableText(step.RetryOfStepID),
				nullableText(step.FailureReason), step.Retryable)
			if err != nil {
				return fmt.Errorf("persist task step: %w", err)
			}
			if result.RowsAffected() == 0 {
				return domain.NewError(domain.CodeInvalidState, "task step identity changed")
			}
		}
		if call := progress.ToolCall; call != nil && call.DetailState == "available" {
			step := progress.Steps[0]
			if _, err := tx.Exec(ctx, `SAVEPOINT task_step_detail_write`); err != nil {
				return fmt.Errorf("start encrypted Task step detail savepoint: %w", err)
			}
			_, detailErr := tx.Exec(ctx, `INSERT INTO task_step_details
(task_id,owner_id,step_id,task_revision,payload_ciphertext,payload_sha256,payload_size_bytes,truncated)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (task_id,step_id) DO UPDATE SET task_revision=EXCLUDED.task_revision,
 payload_ciphertext=EXCLUDED.payload_ciphertext,payload_sha256=EXCLUDED.payload_sha256,
 payload_size_bytes=EXCLUDED.payload_size_bytes,truncated=EXCLUDED.truncated,updated_at=clock_timestamp()`,
				progress.TaskID, current.OwnerID, step.ID, call.TaskRevision, call.DetailCiphertext, call.DetailSHA256, call.DetailSizeBytes, call.Truncated)
			if detailErr != nil {
				if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT task_step_detail_write`); err != nil {
					return fmt.Errorf("recover from encrypted Task step detail failure: %w", errors.Join(detailErr, err))
				}
				if _, err := tx.Exec(ctx, `UPDATE task_steps SET detail_state='not_recorded' WHERE task_id=$1 AND id=$2`, progress.TaskID, step.ID); err != nil {
					return fmt.Errorf("mark unavailable encrypted Task step detail: %w", err)
				}
				step.DetailState = "not_recorded"
				progress.Steps[0] = step
				call.DetailState, call.DetailCiphertext, call.DetailSHA256, call.DetailSizeBytes = "not_recorded", nil, nil, 0
			} else if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT task_step_detail_write`); err != nil {
				return fmt.Errorf("release encrypted Task step detail savepoint: %w", err)
			}
		}
		if len(progress.Artifacts) > 0 {
			if err := persistTaskProgressArtifacts(ctx, tx, current.OwnerID, progress.TaskID, progress.Artifacts); err != nil {
				return err
			}
		}
		if progress.Status == task.StatusCompleted {
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
		workspaceCompletionHeartbeatRequired := progress.Status == task.StatusCompleted && current.WorkspaceID.Valid && current.HostID.Valid
		completionHostOwner := current.InitiatorID
		if current.ProjectID.Valid && current.WorkspaceOwnerID.Valid {
			completionHostOwner = current.WorkspaceOwnerID.String
		}
		var sequence int64
		err = tx.QueryRow(ctx, `WITH completion_clock AS MATERIALIZED (
  -- Align the final Host freshness check with the completion timestamp.
  SELECT clock_timestamp() AS checked_at
)
UPDATE tasks SET status=$4, current_step=$5, result=$6, waiting_reason=$7,
usage_active_seconds=$8, usage_tool_calls=$9,
usage_model_tokens=CASE WHEN execution_location='server' THEN usage_model_tokens ELSE $10 END,
lease_owner=CASE WHEN $4='running' THEN lease_owner ELSE '' END,
lease_expires_at=CASE WHEN $4='running' THEN lease_expires_at ELSE NULL END,
finished_at=CASE WHEN $11 AND $12::boolean THEN completion_clock.checked_at WHEN $11 THEN clock_timestamp() WHEN $4='running' THEN NULL ELSE finished_at END,
event_seq=event_seq+1, updated_at=clock_timestamp()

FROM completion_clock
WHERE id=$1 AND lease_owner=$2 AND lease_version=$3
  AND (NOT $12::boolean OR EXISTS (
    SELECT 1 FROM hosts h JOIN host_runtime_heartbeats hb
      ON hb.host_id=h.id AND hb.runtime_generation=h.runtime_generation
    WHERE h.id=$13::text AND h.owner_id=$14::text AND h.runtime_generation=$15::bigint
      AND h.state='running' AND h.deleted_at IS NULL
      AND hb.last_seen_at > completion_clock.checked_at - $16::double precision * interval '1 second'
  ))
RETURNING event_seq`,
			progress.TaskID, progress.LeaseOwner, progress.LeaseVersion, string(progress.Status), progress.CurrentStep,
			progress.Result, progress.WaitingReason, progress.Usage.ActiveSeconds, progress.Usage.ToolCalls,
			taskUsageTokens(progress.Usage.ModelTokens), finished, workspaceCompletionHeartbeatRequired,
			current.HostID.String, completionHostOwner, workspaceRuntimeGeneration, host.RuntimeHeartbeatFreshness.Seconds()).Scan(&sequence)
		if errors.Is(err, pgx.ErrNoRows) && workspaceCompletionHeartbeatRequired {
			return domain.NewError(domain.CodeWorkspaceConflict, "Host Runtime heartbeat expired before Workspace Task completion")
		}
		if err != nil {
			return fmt.Errorf("commit task progress: %w", err)
		}
		if pending := progress.PendingRequest; pending != nil {
			mediaOperationID := pgtype.Text{}
			if pending.MediaPreviewOperationID != "" {
				mediaOperationID = pgtype.Text{String: pending.MediaPreviewOperationID, Valid: true}
			}
			_, err = tx.Exec(ctx, `INSERT INTO task_pending_requests
(id, task_id, version, kind, title, object, content, impact, call_id, media_preview_operation_id, tool_name, request_hash,
 runtime_generation, runtime_instance_id, control_version, status, expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'pending',$16)`,
				pending.ID, pending.TaskID, pending.Version, pending.Kind, pending.Title, pending.Object, pending.Content, pending.Impact,
				pending.CallID, mediaOperationID, pending.ToolName, pending.RequestHash, pending.RuntimeGeneration, pending.RuntimeInstanceID, pending.ControlVersion, pending.ExpiresAt)
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
		if progress.AssistantMessage == nil {
			if progress.Status != task.StatusRunning {
				if err := interruptAssistantMessageSnapshot(ctx, tx, progress.TaskID, "", true); err != nil {
					return err
				}
			} else {
				for _, step := range progress.Steps {
					if step.ID == progress.CurrentStep && (step.Status == task.StepFailed || step.Status == task.StepUnknown || step.Status == task.StepWaitingUser) {
						if err := interruptAssistantMessageSnapshot(ctx, tx, progress.TaskID, step.ID, false); err != nil {
							return err
						}
						break
					}
				}
			}
		}
		if message := progress.AssistantMessage; message != nil {
			if message.OwnerID != current.OwnerID || message.ActorID != current.InitiatorID || message.ConversationID != current.ConversationID {
				return domain.InvalidArgument("assistant_message", "assistant message does not belong to the task")
			}
			if err := insertMessage(ctx, tx, *message); err != nil {
				return err
			}
			if err := completeAssistantMessageSnapshot(ctx, tx, progress.TaskID, progress.CurrentStep, *message); err != nil {
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
		if progress.PublishResultMessage && progress.Status == task.StatusCompleted {
			if current.ExecutionLocation != "server" {
				return domain.NewError(domain.CodeInvalidState, "completed assistant result requires server execution")
			}
			var taskRevision int64
			var automaticallyRouted bool
			if err := tx.QueryRow(ctx, `SELECT current_revision,EXISTS (
 SELECT 1 FROM conversation_messages m JOIN conversation_message_routes r ON r.message_id=m.id
 WHERE m.task_id=$1 AND m.role='user' AND r.state='routed'
) FROM tasks WHERE id=$1`, progress.TaskID).Scan(&taskRevision, &automaticallyRouted); err != nil {
				return fmt.Errorf("read completed Task revision: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE task_events SET task_revision=$3,result_content_sha256=sha256(convert_to($4::text,'UTF8'))
WHERE task_id=$1 AND sequence=$2 AND event_type='task.result'`, progress.TaskID, sequence, taskRevision, progress.Result); err != nil {
				return fmt.Errorf("record completed Task revision: %w", err)
			}
			queued := false
			if automaticallyRouted {
				var err error
				queued, err = queueConversationResultInTx(ctx, tx, progress.TaskID, taskRevision, sequence, progress.Result, maxConversationResultWindow)
				if err != nil {
					return err
				}
			}
			if !queued {
				var snapshotID, snapshotTurn, snapshotContent, snapshotStatus string
				var snapshotRevision pgtype.Int8
				snapshotErr := tx.QueryRow(ctx, `SELECT message_id,turn_id,content,status,task_revision FROM task_assistant_message_snapshots
WHERE task_id=$1 FOR UPDATE`, progress.TaskID).Scan(&snapshotID, &snapshotTurn, &snapshotContent, &snapshotStatus, &snapshotRevision)
				if snapshotErr != nil && !errors.Is(snapshotErr, pgx.ErrNoRows) {
					return fmt.Errorf("check completed assistant result: %w", snapshotErr)
				}
				sameRevision := snapshotRevision.Valid && snapshotRevision.Int64 == taskRevision || !snapshotRevision.Valid && taskRevision == 0
				alreadySaved := snapshotErr == nil && sameRevision && snapshotStatus == string(app.AssistantMessageCompleted) &&
					snapshotTurn == progress.CurrentStep && snapshotContent == progress.Result
				messageID := ""
				if snapshotErr == nil && sameRevision && snapshotStatus == string(app.AssistantMessageInterrupted) &&
					snapshotTurn == progress.CurrentStep && snapshotContent != "" && strings.HasPrefix(progress.Result, snapshotContent) && domain.ValidMessageID(snapshotID) {
					messageID = snapshotID
				}
				if alreadySaved {
					messageID = snapshotID
				} else if messageID == "" {
					var err error
					messageID, err = domain.NewMessageID()
					if err != nil {
						return err
					}
				}
				message := conversation.Message{ID: messageID, ConversationID: current.ConversationID, OwnerID: current.OwnerID,
					ActorID: current.InitiatorID, Role: conversation.RoleAssistant, Content: progress.Result, TaskID: current.ID}
				if !alreadySaved {
					if messageID == snapshotID && snapshotID != "" {
						updated, err := tx.Exec(ctx, `UPDATE conversation_messages SET content=$1,output_status='completed',output_result_event_sequence=$2
WHERE id=$3 AND task_id=$4 AND conversation_id=$5 AND owner_id=$6 AND actor_id=$7
 AND output_status='interrupted' AND output_task_revision=$8 AND content=$9 AND left($1,char_length(content))=content`,
							progress.Result, sequence, messageID, current.ID, current.ConversationID, current.OwnerID,
							current.InitiatorID, taskRevision, snapshotContent)
						if err != nil {
							return fmt.Errorf("complete assistant stream message: %w", err)
						}
						if updated.RowsAffected() != 1 {
							var exists bool
							if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM conversation_messages WHERE id=$1)`, messageID).Scan(&exists); err != nil {
								return fmt.Errorf("check legacy assistant snapshot: %w", err)
							}
							if exists {
								return domain.NewError(domain.CodeInvalidState, "assistant stream message changed before completion")
							}
							// A snapshot written before durable stream messages has no
							// conversation row yet; preserve its original identity.
							if err := insertMessage(ctx, tx, message); err != nil {
								return err
							}
						}
					} else if err := insertMessage(ctx, tx, message); err != nil {
						return err
					}
					if err := completeAssistantMessageSnapshot(ctx, tx, progress.TaskID, progress.CurrentStep, message); err != nil {
						return err
					}
				}
				if _, err := tx.Exec(ctx, `UPDATE task_assistant_message_snapshots SET task_revision=$2,result_event_sequence=$3
WHERE task_id=$1 AND message_id=$4 AND status='completed'`, progress.TaskID, taskRevision, sequence, messageID); err != nil {
					return fmt.Errorf("record completed assistant result evidence: %w", err)
				}
				if err := recordAccountObjectChanges(ctx, tx,
					accountObjectChange{OwnerID: current.OwnerID, ObjectType: "conversation", ObjectID: current.ConversationID, Operation: "upsert"},
					accountObjectChange{OwnerID: current.OwnerID, ObjectType: "message", ObjectID: messageID, Operation: "upsert"},
				); err != nil {
					return err
				}
			}
		}
		var createdAt time.Time
		if err := tx.QueryRow(ctx, `SELECT created_at FROM task_events WHERE task_id=$1 AND sequence=$2`, progress.TaskID, sequence).Scan(&createdAt); err != nil {
			return fmt.Errorf("read committed task event time: %w", err)
		}
		if message := progress.AssistantMessage; message != nil {
			if err := recordAccountObjectChanges(ctx, tx,
				accountObjectChange{OwnerID: current.OwnerID, ObjectType: "conversation", ObjectID: current.ConversationID, Operation: "upsert"},
				accountObjectChange{OwnerID: current.OwnerID, ObjectType: "message", ObjectID: message.ID, Operation: "upsert"},
			); err != nil {
				return err
			}
		}
		result = task.Event{TaskID: progress.TaskID, Sequence: sequence, Type: progress.Event.Type, Payload: eventPayload, CreatedAt: createdAt}
		return nil
	})
	if err == nil && authorizationDenied {
		if progress.CloudDSHPersonalMutation != nil || progress.CloudDSHSkillRead != nil {
			return task.Event{}, domain.NewError(domain.CodeUnauthenticated, "task authorization is unavailable")
		}
		return task.Event{}, taskLeaseConflict()
	}
	return result, err
}

func persistTaskMediaApprovalCommit(ctx context.Context, tx pgx.Tx, current sqlcgen.Task, progress app.TaskProgress, media app.TaskMediaApprovalCommit) error {
	pending := progress.PendingRequest
	if pending == nil || pending.MediaPreviewOperationID != media.OperationID || pending.TaskID != current.ID ||
		!validTaskMediaStagingCommand(app.CreateTaskMediaStagedObjectCommand{
			TaskID: current.ID, OwnerID: current.OwnerID, OperationID: media.OperationID, RuntimeCallID: media.RuntimeCallID,
			PendingRequestID: pending.ID, PendingRequestVersion: pending.Version, MediaType: media.MediaType,
			SizeBytes: media.SizeBytes, SHA256: media.SHA256, ExpiresAt: media.ExpiresAt,
		}) || len(media.OperationResult) < 2 || len(media.OperationResult) > 4096 ||
		!utf8.ValidString(media.OperationResult) || !json.Valid([]byte(media.OperationResult)) ||
		len(media.ProviderOperationID) > 256 || !utf8.ValidString(media.ProviderOperationID) || strings.ContainsRune(media.ProviderOperationID, '\x00') {
		return domain.InvalidArgument("task_media_approval", "task media approval commit fields are invalid")
	}
	operationKey := app.AgentOperationKey(pending.CallID)
	var operationID, storedKey, status, externalID, operationResult string
	var requestHash []byte
	var leaseVersion int64
	err := tx.QueryRow(ctx, `SELECT id, operation_key, request_hash, lease_version, status, external_id, result
FROM task_operations WHERE task_id=$1 AND operation_key=$2 FOR UPDATE`, current.ID, operationKey).
		Scan(&operationID, &storedKey, &requestHash, &leaseVersion, &status, &externalID, &operationResult)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeInvalidState, "task media operation is unavailable")
	}
	if err != nil {
		return fmt.Errorf("lock task media operation: %w", err)
	}
	if operationID != media.OperationID || storedKey != operationKey || !bytes.Equal(requestHash, pending.RequestHash) ||
		leaseVersion > progress.LeaseVersion || (status != string(task.OperationDispatched) && status != string(task.OperationUnknown) && status != string(task.OperationSucceeded)) ||
		(externalID != "" && externalID != media.ProviderOperationID) || (operationResult != "" && operationResult != media.OperationResult) {
		return domain.NewError(domain.CodeInvalidState, "task media operation binding changed")
	}
	if status != string(task.OperationSucceeded) {
		updated, err := tx.Exec(ctx, `UPDATE task_operations SET status=$3, external_id=$4, result=$5, lease_version=$6, updated_at=clock_timestamp()
		WHERE id=$1 AND task_id=$2 AND status=$7 AND lease_version=$8`, operationID, current.ID, string(task.OperationSucceeded), media.ProviderOperationID,
			media.OperationResult, progress.LeaseVersion, status, leaseVersion)
		if err != nil {
			return fmt.Errorf("complete task media operation: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "task media operation changed before approval commit")
		}
		payload, err := json.Marshal(map[string]string{"operation_id": operationID, "operation_key": operationKey, "status": "succeeded"})
		if err != nil {
			return fmt.Errorf("encode task media operation event: %w", err)
		}
		sequence, err := incrementTaskEventSequence(ctx, tx, current.ID)
		if err != nil {
			return err
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: current.ID, Sequence: sequence, Type: "task.operation", Payload: payload}); err != nil {
			return err
		}
	}
	if _, err := sqlcgen.New(tx).CreateTaskMediaStagedObject(ctx, sqlcgen.CreateTaskMediaStagedObjectParams{
		OperationID: media.OperationID, TaskID: current.ID, OwnerID: current.OwnerID, RuntimeCallID: media.RuntimeCallID,
		MediaType: media.MediaType, SizeBytes: media.SizeBytes, Sha256: media.SHA256, ExpiresAt: timestamp(media.ExpiresAt),
		State: string(app.TaskMediaPreviewPending),
	}); err != nil {
		return fmt.Errorf("create task media staged object: %w", err)
	}
	details, err := sqlcgen.New(tx).GetTaskMediaOperationDetails(ctx, sqlcgen.GetTaskMediaOperationDetailsParams{TaskID: current.ID, OperationID: media.OperationID})
	if err != nil {
		return fmt.Errorf("verify committed task media operation: %w", err)
	}
	if details.OwnerID != current.OwnerID || details.InitiatorID != current.InitiatorID || details.RuntimeCallID != media.RuntimeCallID ||
		details.OperationKey != operationKey || !bytes.Equal(details.RequestHash, pending.RequestHash) ||
		details.ProviderOperationID != media.ProviderOperationID || details.OperationStatus != string(task.OperationSucceeded) ||
		details.TokenState != "settled" || !details.InputTokens.Valid || !details.OutputTokens.Valid ||
		details.InputTokens.Int64 < 0 || details.OutputTokens.Int64 < 0 || details.ReservedTokens < 1 ||
		details.InputTokens.Int64 > details.ReservedTokens || details.OutputTokens.Int64 > details.ReservedTokens-details.InputTokens.Int64 ||
		details.MediaType != media.MediaType || details.SizeBytes != media.SizeBytes || !bytes.Equal(details.Sha256, media.SHA256) ||
		details.State != string(app.TaskMediaPreviewPending) {
		return domain.NewError(domain.CodeInvalidState, "task media operation is not settled or its preview binding changed")
	}
	return nil
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
		projectWorkspaceFence, err := lockProjectTaskWorkspaceFence(ctx, tx, value.TaskID, false)
		if err != nil {
			return err
		}
		var ownerID, initiatorID string
		if err := tx.QueryRow(ctx, `SELECT owner_id,initiator_id FROM tasks WHERE id=$1`, value.TaskID).Scan(&ownerID, &initiatorID); errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		} else if err != nil {
			return fmt.Errorf("read task owner before operation registration: %w", err)
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, initiatorID, nil)
		if err != nil {
			return err
		}
		var row sqlcgen.Task
		var taskEpoch int64
		err = tx.QueryRow(ctx, `SELECT id, owner_id, project_id, initiator_id, conversation_id, host_id, model_id, objective, status, auth_kind, auth_token_id, auth_scopes, auth_client_installation_id,
budget_active_seconds, budget_tool_calls, budget_model_tokens, usage_active_seconds, usage_tool_calls, usage_model_tokens, lease_owner,
lease_version, lease_expires_at, event_seq, current_step, result, waiting_reason, created_at, updated_at, finished_at, execution_epoch, application_id, workspace_id, input_revision, workspace_checkpoint_revision, workspace_owner_id
FROM tasks WHERE id=$1 FOR UPDATE`, value.TaskID).Scan(
			&row.ID, &row.OwnerID, &row.ProjectID, &row.InitiatorID, &row.ConversationID, &row.HostID, &row.ModelID, &row.Objective, &row.Status,
			&row.AuthKind, &row.AuthTokenID, &row.AuthScopes, &row.AuthClientInstallationID, &row.BudgetActiveSeconds, &row.BudgetToolCalls,
			&row.BudgetModelTokens, &row.UsageActiveSeconds, &row.UsageToolCalls, &row.UsageModelTokens,
			&row.LeaseOwner, &row.LeaseVersion, &row.LeaseExpiresAt, &row.EventSeq, &row.CurrentStep,
			&row.Result, &row.WaitingReason, &row.CreatedAt, &row.UpdatedAt, &row.FinishedAt, &taskEpoch, &row.ApplicationID,
			&row.WorkspaceID, &row.InputRevision, &row.WorkspaceCheckpointRevision, &row.WorkspaceOwnerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("lock task for operation: %w", err)
		}
		if err := verifyProjectTaskWorkspaceFence(ctx, tx, value.TaskID, projectWorkspaceFence); err != nil {
			return err
		}
		ownerID = row.OwnerID
		if row.InitiatorID != initiatorID {
			return domain.NewError(domain.CodeUnauthenticated, "task initiator is unavailable")
		}
		if taskEpoch != gate.epoch {
			return ownerExecutionDisabledError(gate)
		}
		if err := requireCurrentLease(ctx, tx, row, leaseOwner, value.LeaseVersion); err != nil {
			return err
		}
		authorizationRow, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: value.TaskID, OwnerID: ownerID})
		if err != nil {
			return fmt.Errorf("read task authorization before operation registration: %w", err)
		}
		authorized, err := requireCurrentTaskAuthorization(ctx, tx, authorizationRow)
		if err != nil {
			return err
		}
		if !authorized {
			return domain.NewError(domain.CodeUnauthenticated, "task operation authorization is unavailable")
		}
		if value.HostID != row.HostID.String {
			return domain.InvalidArgument("operation.host_id", "operation host does not match task host")
		}
		if err := requireOperationHostFence(ctx, tx, ownerID, value); err != nil {
			return err
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
		var projectWorkspaceFence projectTaskWorkspaceFence
		if update.Status != task.OperationUnknown {
			var err error
			projectWorkspaceFence, err = lockProjectTaskWorkspaceFence(ctx, tx, update.TaskID, false)
			if err != nil {
				return err
			}
		}
		var ownerID, initiatorID string
		if err := tx.QueryRow(ctx, `SELECT owner_id,initiator_id FROM tasks WHERE id=$1`, update.TaskID).Scan(&ownerID, &initiatorID); errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		} else if err != nil {
			return fmt.Errorf("read task owner before operation update: %w", err)
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, initiatorID, nil)
		if err != nil {
			return err
		}
		var row sqlcgen.Task
		var taskEpoch int64
		err = tx.QueryRow(ctx, `SELECT id, owner_id, project_id, initiator_id, conversation_id, host_id, model_id, objective, status, auth_kind, auth_token_id, auth_scopes, auth_client_installation_id,
budget_active_seconds, budget_tool_calls, budget_model_tokens, usage_active_seconds, usage_tool_calls, usage_model_tokens, lease_owner,
lease_version, lease_expires_at, event_seq, current_step, result, waiting_reason, created_at, updated_at, finished_at, execution_epoch, application_id,
workspace_id, input_revision, workspace_checkpoint_revision, workspace_owner_id
FROM tasks WHERE id=$1 FOR UPDATE`, update.TaskID).Scan(
			&row.ID, &row.OwnerID, &row.ProjectID, &row.InitiatorID, &row.ConversationID, &row.HostID, &row.ModelID, &row.Objective, &row.Status,
			&row.AuthKind, &row.AuthTokenID, &row.AuthScopes, &row.AuthClientInstallationID, &row.BudgetActiveSeconds, &row.BudgetToolCalls,
			&row.BudgetModelTokens, &row.UsageActiveSeconds, &row.UsageToolCalls, &row.UsageModelTokens,
			&row.LeaseOwner, &row.LeaseVersion, &row.LeaseExpiresAt, &row.EventSeq, &row.CurrentStep,
			&row.Result, &row.WaitingReason, &row.CreatedAt, &row.UpdatedAt, &row.FinishedAt, &taskEpoch, &row.ApplicationID,
			&row.WorkspaceID, &row.InputRevision, &row.WorkspaceCheckpointRevision, &row.WorkspaceOwnerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("lock task for operation update: %w", err)
		}
		if update.Status != task.OperationUnknown {
			if err := verifyProjectTaskWorkspaceFence(ctx, tx, update.TaskID, projectWorkspaceFence); err != nil {
				return err
			}
		}
		if row.InitiatorID != initiatorID {
			return domain.NewError(domain.CodeUnauthenticated, "task initiator is unavailable")
		}
		if taskEpoch != gate.epoch {
			return ownerExecutionDisabledError(gate)
		}
		if err := requireCurrentLease(ctx, tx, row, update.LeaseOwner, update.LeaseVersion); err != nil {
			return err
		}
		authorizationRow, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: update.TaskID, OwnerID: ownerID})
		if err != nil {
			return fmt.Errorf("read task authorization before operation update: %w", err)
		}
		authorized, err := requireCurrentTaskAuthorization(ctx, tx, authorizationRow)
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
	if err := tx.QueryRow(ctx, `SELECT lease_expires_at > clock_timestamp() AND current_revision=desired_revision FROM tasks WHERE id=$1`, row.ID).Scan(&valid); err != nil {
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
	var initiatorID string
	if err := tx.QueryRow(ctx, `SELECT initiator_id FROM tasks WHERE id=$1 AND owner_id=$2 FOR SHARE`, operation.TaskID, ownerID).
		Scan(&initiatorID); err != nil {
		return domain.NewError(domain.CodeUnauthenticated, "task initiator is unavailable")
	}
	if _, err := lockTaskInitiatorInTx(ctx, tx, operation.TaskID, ownerID, initiatorID); err != nil {
		return err
	}
	return requireTaskAgentHostAuthorizationFence(ctx, tx, operation.TaskID, ownerID, operation)
}

func requireTaskWorkspaceInputAdmission(ctx context.Context, tx pgx.Tx, row sqlcgen.Task, operation task.Operation) error {
	if operation.InputRevision == nil || !row.WorkspaceID.Valid || row.WorkspaceID.String != operation.WorkspaceID ||
		!row.InputRevision.Valid || row.InputRevision.Int64 != *operation.InputRevision ||
		!row.WorkspaceCheckpointRevision.Valid || operation.WorkspaceCheckpointRevision == nil ||
		row.WorkspaceCheckpointRevision.Int64 != *operation.WorkspaceCheckpointRevision {
		return domain.NewError(domain.CodeWorkspaceConflict, "task Workspace input changed before operation admission")
	}
	checkpoint := *operation.WorkspaceCheckpointRevision
	expectedWorkspaceOwner := row.InitiatorID
	if row.ProjectID.Valid {
		if !row.WorkspaceOwnerID.Valid || row.WorkspaceOwnerID.String == "" {
			return domain.NewError(domain.CodeWorkspaceConflict, "project Workspace owner is unavailable")
		}
		expectedWorkspaceOwner = row.WorkspaceOwnerID.String
	}
	var ownerID, state string
	var revision int64
	err := tx.QueryRow(ctx, `SELECT owner_id, state, revision FROM workspaces WHERE id=$1 FOR SHARE`, operation.WorkspaceID).
		Scan(&ownerID, &state, &revision)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (ownerID != expectedWorkspaceOwner || state != string(workspace.StateReady) || revision != checkpoint)) {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed after this task input was confirmed")
	}
	if err != nil {
		return fmt.Errorf("lock Workspace for task operation admission: %w", err)
	}
	if err := requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, operation.WorkspaceID); err != nil {
		return err
	}
	if err := requireNoPendingWorkspaceLegacyImport(ctx, tx, operation.WorkspaceID); err != nil {
		return err
	}
	// A verified Task publish advances its private view before the Host replica catches up.
	var bindingOwner, bindingState, syncState string
	var bindingVersion int64
	err = tx.QueryRow(ctx, `SELECT owner_id, state, sync_state, binding_version
FROM workspace_bindings WHERE host_id=$1 AND workspace_id=$2 FOR SHARE`, operation.HostID, operation.WorkspaceID).
		Scan(&bindingOwner, &bindingState, &syncState, &bindingVersion)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (bindingOwner != expectedWorkspaceOwner || bindingState != string(workspace.BindingActive) ||
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
