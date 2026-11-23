package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

const taskAgentStallNoticeKind = "agent"

func (s *Store) CheckTaskAgentStall(ctx context.Context, value task.Task, staleAfter time.Duration) error {
	if ctx == nil || !domain.ValidTaskID(value.ID) || !domain.ValidUserID(value.OwnerID) ||
		value.Status != task.StatusRunning || value.Lease.Owner == "" || value.Lease.Version < 1 || staleAfter <= 0 || staleAfter.Milliseconds() < 1 ||
		value.AgentInstanceID == "" || value.AgentGeneration < 1 || value.AgentSessionID == "" || value.CurrentRevision == nil || *value.CurrentRevision < 0 {
		return domain.InvalidArgument("task_agent_stall", "Task agent stall check identity or threshold is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var currentRevision, agentGeneration int64
		err := tx.QueryRow(ctx, `SELECT t.current_revision,t.agent_generation
FROM tasks t JOIN task_agent_instances ai ON ai.task_id=t.id AND ai.owner_id=t.owner_id
WHERE t.id=$1 AND t.owner_id=$2 AND t.status='running'
  AND t.lease_owner=$3 AND t.lease_version=$4 AND t.lease_expires_at>clock_timestamp()
  AND t.current_revision=$5 AND t.agent_instance_id=$6 AND t.agent_generation=$7 AND t.agent_session_id=$8
  AND t.agent_engine_version=$9 AND t.agent_profile_version=$10 AND t.auth_version=$11
  AND ai.agent_instance_id=t.agent_instance_id AND ai.agent_generation=t.agent_generation
  AND ai.agent_session_id=t.agent_session_id AND ai.engine_version=t.agent_engine_version AND ai.profile_version=t.agent_profile_version
  AND ai.lease_owner=t.lease_owner AND ai.lease_version=t.lease_version AND ai.auth_version=t.auth_version
  AND ai.state='active' AND ai.lease_expires_at>clock_timestamp()
  AND (t.auth_kind='schedule_task' OR t.auth_expires_at>clock_timestamp())
FOR UPDATE OF t,ai`, value.ID, value.OwnerID, value.Lease.Owner, value.Lease.Version, *value.CurrentRevision,
			value.AgentInstanceID, value.AgentGeneration, value.AgentSessionID, value.AgentEngineVersion,
			value.AgentProfileVersion, value.Authorization.Version).Scan(&currentRevision, &agentGeneration)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("validate Task and Agent fences for liveness check: %w", err)
		}
		if currentRevision != *value.CurrentRevision || agentGeneration != value.AgentGeneration {
			return nil
		}

		pendingMessages, err := taskAgentPendingMessages(ctx, tx, value.ID, value.OwnerID, value.ConversationID)
		if err != nil {
			return err
		}
		hasPendingReconciliation, err := taskAgentHasPendingRequestReconciliation(ctx, tx, value.ID, value.OwnerID, *value.CurrentRevision)
		if err != nil {
			return err
		}
		known, stalled, err := taskAgentActivityStalled(ctx, tx, value, staleAfter)
		if err != nil {
			return err
		}
		pending := make(map[string]struct{}, len(pendingMessages))
		for _, messageID := range pendingMessages {
			pending[messageID] = struct{}{}
		}

		type notice struct {
			messageID string
			stepID    string
			state     string
			kind      string
			revision  int64
		}
		rows, err := tx.Query(ctx, `SELECT message_id,step_id,state,notice_kind,task_revision FROM task_agent_stall_notices
WHERE task_id=$1 AND owner_id=$2 FOR UPDATE`, value.ID, value.OwnerID)
		if err != nil {
			return fmt.Errorf("lock Task agent stall notices: %w", err)
		}
		notices := make([]notice, 0)
		existing := make(map[string]struct{})
		for rows.Next() {
			var item notice
			if err := rows.Scan(&item.messageID, &item.stepID, &item.state, &item.kind, &item.revision); err != nil {
				rows.Close()
				return fmt.Errorf("scan Task agent stall notice: %w", err)
			}
			existing[item.messageID] = struct{}{}
			if item.state == "active" {
				notices = append(notices, item)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read Task agent stall notices: %w", err)
		}
		rows.Close()

		for _, item := range notices {
			_, stillPending := pending[item.messageID]
			if item.kind == taskAgentRequestReconciliationNoticeKind {
				hasUnresolved, err := taskAgentHasPendingRequestReconciliation(ctx, tx, value.ID, value.OwnerID, item.revision)
				if err != nil {
					return err
				}
				if hasUnresolved {
					continue
				}
				summary := taskAgentRequestReconciliationClearedSummary
				if !stillPending {
					summary = taskAgentReconciliationResolvedMessageSummary
				}
				if err := clearTaskAgentStallNoticeWithSummary(ctx, tx, value.ID, item.messageID, item.stepID, summary); err != nil {
					return err
				}
				continue
			}
			if stillPending && (!known || stalled) {
				continue
			}
			if err := clearTaskAgentStallNotice(ctx, tx, value.ID, item.messageID, item.stepID, stillPending); err != nil {
				return err
			}
		}
		for _, messageID := range pendingMessages {
			if hasPendingReconciliation {
				if err := createOrPromoteTaskAgentRequestReconciliationNotice(ctx, tx, value.ID,
					value.OwnerID, messageID, *value.CurrentRevision, value.AgentGeneration); err != nil {
					return err
				}
				continue
			}
			if !known || !stalled {
				continue
			}
			if _, exists := existing[messageID]; exists {
				continue
			}
			if err := createTaskAgentStallNotice(ctx, tx, value, messageID, currentRevision, agentGeneration); err != nil {
				return err
			}
		}
		return nil
	})
}

func taskAgentPendingMessages(ctx context.Context, tx pgx.Tx, taskID, ownerID, conversationID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM conversation_messages
WHERE task_id=$1 AND owner_id=$2 AND conversation_id=$3 AND role='user' AND delivery_status='pending'
ORDER BY sequence,id`, taskID, ownerID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list pending Task input messages: %w", err)
	}
	defer rows.Close()
	messages := make([]string, 0)
	for rows.Next() {
		var messageID string
		if err := rows.Scan(&messageID); err != nil {
			return nil, fmt.Errorf("scan pending Task input message: %w", err)
		}
		messages = append(messages, messageID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending Task input messages: %w", err)
	}
	return messages, nil
}

func taskAgentActivityStalled(ctx context.Context, tx pgx.Tx, value task.Task, staleAfter time.Duration) (known, stalled bool, err error) {
	rows, err := tx.Query(ctx, `SELECT session_id,COALESCE(parent_session_id,''),activity_status,
activity_generation,activity_revision,activity_phase,activity_state,activity_progressed_at,activity_heartbeat_at,
COALESCE(activity_heartbeat_at<=clock_timestamp()-($4::bigint*interval '1 millisecond'),false),
COALESCE(activity_phase='model' AND activity_state='started' AND
  activity_progressed_at<=clock_timestamp()-($5::bigint*interval '1 millisecond'),false)
FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2
  AND (parent_session_id IS NULL OR task_revision=$3)
ORDER BY (parent_session_id IS NULL) DESC,created_at,session_id`, value.ID, value.OwnerID, *value.CurrentRevision,
		staleAfter.Milliseconds(), app.ModelCallTimeout.Milliseconds())
	if err != nil {
		return false, false, fmt.Errorf("read current Task agent activity: %w", err)
	}
	defer rows.Close()
	rootFound := false
	unknown := false
	for rows.Next() {
		var sessionID, parentSessionID string
		var status pgtype.Text
		var generation, revision pgtype.Int8
		var phase, state pgtype.Text
		var progressedAt, heartbeat pgtype.Timestamptz
		var heartbeatStale, modelCallStale bool
		if err := rows.Scan(&sessionID, &parentSessionID, &status, &generation, &revision, &phase, &state,
			&progressedAt, &heartbeat, &heartbeatStale, &modelCallStale); err != nil {
			return false, false, fmt.Errorf("scan current Task agent activity: %w", err)
		}
		if parentSessionID == "" {
			if sessionID != value.AgentSessionID || rootFound {
				unknown = true
				continue
			}
			rootFound = true
			if !generation.Valid || generation.Int64 != value.AgentGeneration || !revision.Valid || revision.Int64 != *value.CurrentRevision ||
				!status.Valid || (status.String != "idle" && status.String != "running") || !heartbeat.Valid {
				unknown = true
				continue
			}
			if state.Valid && state.String == "started" && status.String != "running" {
				unknown = true
				continue
			}
			stalled = stalled || heartbeatStale
			if phase.Valid && phase.String == "model" {
				if !state.Valid || state.String == "started" && !progressedAt.Valid {
					unknown = true
					continue
				}
				stalled = stalled || modelCallStale
			}
			continue
		}
		if !generation.Valid || generation.Int64 != value.AgentGeneration {
			continue
		}
		if !revision.Valid || revision.Int64 != *value.CurrentRevision || !status.Valid {
			unknown = true
			continue
		}
		if state.Valid && state.String == "started" && status.String != "running" {
			unknown = true
			continue
		}
		if phase.Valid && phase.String == "model" {
			if !state.Valid || state.String == "started" && !progressedAt.Valid {
				unknown = true
				continue
			}
			stalled = stalled || modelCallStale
		}
		if status.String == "idle" {
			continue
		}
		if status.String != "running" || !heartbeat.Valid {
			unknown = true
			continue
		}
		stalled = stalled || heartbeatStale
	}
	if err := rows.Err(); err != nil {
		return false, false, fmt.Errorf("read current Task agent activity: %w", err)
	}
	if !rootFound || unknown {
		return false, false, nil
	}
	return true, stalled, nil
}

func createTaskAgentStallNotice(ctx context.Context, tx pgx.Tx, value task.Task, messageID string, revision, generation int64) error {
	stepID, err := domain.NewStepID()
	if err != nil {
		return fmt.Errorf("generate Task activity notice step ID: %w", err)
	}
	var stepSequence int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_steps WHERE task_id=$1`, value.ID).Scan(&stepSequence); err != nil {
		return fmt.Errorf("allocate Task activity notice step sequence: %w", err)
	}
	const title = "任务仍在运行"
	const summary = "检测到待处理消息，Agent 活动状态已超时未更新；任务未被自动取消。"
	if _, err := tx.Exec(ctx, `INSERT INTO task_steps (id,task_id,sequence,kind,status,title,summary)
VALUES ($1,$2,$3,$4,'running',$5,$6)`, stepID, value.ID, stepSequence, taskAgentStallNoticeKind, title, summary); err != nil {
		return fmt.Errorf("persist Task activity notice step: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_agent_stall_notices
(task_id,owner_id,message_id,step_id,task_revision,agent_generation,state,notice_kind)
VALUES ($1,$2,$3,$4,$5,$6,'active','activity_stall')`, value.ID, value.OwnerID, messageID, stepID, revision, generation); err != nil {
		return fmt.Errorf("persist Task activity notice identity: %w", err)
	}
	return insertTaskAgentStallStepEvent(ctx, tx, value.ID, stepID, stepSequence, taskAgentStallNoticeKind, "running", title, summary)
}

func clearTaskAgentStallNotice(ctx context.Context, tx pgx.Tx, taskID, messageID, stepID string, stillPending bool) error {
	const summaryResumed = "Agent 已恢复活动观察；任务仍按原状态继续。"
	const summaryResolved = "该消息已不在待处理队列中；任务仍按原状态继续。"
	summary := summaryResumed
	if !stillPending {
		summary = summaryResolved
	}
	return clearTaskAgentStallNoticeWithSummary(ctx, tx, taskID, messageID, stepID, summary)
}

func clearTaskAgentStallNoticeWithSummary(ctx context.Context, tx pgx.Tx, taskID, messageID, stepID, summary string) error {
	result, err := tx.Exec(ctx, `UPDATE task_agent_stall_notices SET state='cleared',cleared_at=clock_timestamp()
WHERE task_id=$1 AND message_id=$2 AND state='active'`, taskID, messageID)
	if err != nil {
		return fmt.Errorf("clear Task activity notice state: %w", err)
	}
	if result.RowsAffected() == 0 {
		return nil
	}
	var stepSequence int64
	var kind, title string
	if err := tx.QueryRow(ctx, `UPDATE task_steps SET status='succeeded',summary=$3
WHERE task_id=$1 AND id=$2 AND status='running' RETURNING sequence,kind,title`, taskID, stepID, summary).
		Scan(&stepSequence, &kind, &title); err != nil {
		return fmt.Errorf("complete Task activity notice step: %w", err)
	}
	return insertTaskAgentStallStepEvent(ctx, tx, taskID, stepID, stepSequence, kind, "succeeded", title, summary)
}

func clearTaskAgentStallNoticesForProgress(ctx context.Context, tx pgx.Tx, taskID, ownerID string) error {
	rows, err := tx.Query(ctx, `SELECT n.message_id,n.step_id,n.notice_kind,n.task_revision,
EXISTS (SELECT 1 FROM conversation_messages m WHERE m.id=n.message_id AND m.task_id=n.task_id AND m.owner_id=n.owner_id
  AND m.role='user' AND m.delivery_status='pending')
FROM task_agent_stall_notices n
WHERE n.task_id=$1 AND n.owner_id=$2 AND n.state='active' FOR UPDATE OF n`, taskID, ownerID)
	if err != nil {
		return fmt.Errorf("lock Task activity notices before clearing on progress: %w", err)
	}
	type item struct {
		messageID, stepID, kind string
		revision                int64
		stillPending            bool
	}
	items := make([]item, 0)
	for rows.Next() {
		var notice item
		if err := rows.Scan(&notice.messageID, &notice.stepID, &notice.kind, &notice.revision, &notice.stillPending); err != nil {
			rows.Close()
			return fmt.Errorf("scan Task activity notice before clearing on progress: %w", err)
		}
		items = append(items, notice)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read Task activity notices before clearing on progress: %w", err)
	}
	rows.Close()
	for _, notice := range items {
		if notice.kind == taskAgentRequestReconciliationNoticeKind {
			hasUnresolved, err := taskAgentHasPendingRequestReconciliation(ctx, tx, taskID, ownerID, notice.revision)
			if err != nil {
				return err
			}
			if hasUnresolved {
				continue
			}
			summary := taskAgentRequestReconciliationClearedSummary
			if !notice.stillPending {
				summary = taskAgentReconciliationResolvedMessageSummary
			}
			if err := clearTaskAgentStallNoticeWithSummary(ctx, tx, taskID, notice.messageID, notice.stepID,
				summary); err != nil {
				return err
			}
			continue
		}
		if err := clearTaskAgentStallNotice(ctx, tx, taskID, notice.messageID, notice.stepID, notice.stillPending); err != nil {
			return err
		}
	}
	return nil
}

const (
	taskAgentRequestReconciliationNoticeKind      = "request_reconciliation"
	taskAgentRequestReconciliationNoticeTitle     = "操作结果待核对"
	taskAgentRequestReconciliationNoticeSummary   = "Agent 的请求响应期限已到，操作结果仍待对账；不会自动重试或视为完成。"
	taskAgentRequestReconciliationClearedSummary  = "已取得该请求的精确对账结果；消息仍待处理，任务按原状态继续。"
	taskAgentReconciliationResolvedMessageSummary = "已取得该请求的精确对账结果；该消息已不在待处理队列中，任务按原状态继续。"
)

func taskAgentHasPendingRequestReconciliation(ctx context.Context, tx pgx.Tx, taskID, ownerID string, revision int64) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM task_agent_request_reconciliations
  WHERE task_id=$1 AND owner_id=$2 AND task_revision=$3 AND state='pending'
)`, taskID, ownerID, revision).Scan(&exists); err != nil {
		return false, fmt.Errorf("check unresolved Task agent request reconciliation: %w", err)
	}
	return exists, nil
}

func createOrPromoteTaskAgentRequestReconciliationNotice(ctx context.Context, tx pgx.Tx, taskID, ownerID,
	messageID string, revision, generation int64) error {
	var stepID, state, noticeKind string
	err := tx.QueryRow(ctx, `SELECT step_id,state,notice_kind FROM task_agent_stall_notices
WHERE task_id=$1 AND owner_id=$2 AND message_id=$3 FOR UPDATE`, taskID, ownerID, messageID).
		Scan(&stepID, &state, &noticeKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return createTaskAgentRequestReconciliationNotice(ctx, tx, taskID, ownerID, messageID, revision, generation)
	}
	if err != nil {
		return fmt.Errorf("lock Task agent request reconciliation notice: %w", err)
	}
	if state == "active" && noticeKind == taskAgentRequestReconciliationNoticeKind {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE task_agent_stall_notices SET state='active',cleared_at=NULL,
task_revision=$4,agent_generation=$5,notice_kind=$6
WHERE task_id=$1 AND owner_id=$2 AND message_id=$3`, taskID, ownerID, messageID, revision, generation,
		taskAgentRequestReconciliationNoticeKind); err != nil {
		return fmt.Errorf("promote Task agent request reconciliation notice: %w", err)
	}
	var stepSequence int64
	var kind, title string
	if err := tx.QueryRow(ctx, `UPDATE task_steps SET status='running',title=$3,summary=$4
WHERE task_id=$1 AND id=$2 RETURNING sequence,kind,title`, taskID, stepID,
		taskAgentRequestReconciliationNoticeTitle, taskAgentRequestReconciliationNoticeSummary).
		Scan(&stepSequence, &kind, &title); err != nil {
		return fmt.Errorf("reopen Task agent request reconciliation step: %w", err)
	}
	return insertTaskAgentStallStepEvent(ctx, tx, taskID, stepID, stepSequence, kind, "running", title,
		taskAgentRequestReconciliationNoticeSummary)
}

func createTaskAgentRequestReconciliationNotice(ctx context.Context, tx pgx.Tx, taskID, ownerID, messageID string,
	revision, generation int64) error {
	stepID, err := domain.NewStepID()
	if err != nil {
		return fmt.Errorf("generate Task request reconciliation notice step ID: %w", err)
	}
	var stepSequence int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_steps WHERE task_id=$1`, taskID).Scan(&stepSequence); err != nil {
		return fmt.Errorf("allocate Task request reconciliation step sequence: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_steps (id,task_id,sequence,kind,status,title,summary)
VALUES ($1,$2,$3,$4,'running',$5,$6)`, stepID, taskID, stepSequence, taskAgentStallNoticeKind,
		taskAgentRequestReconciliationNoticeTitle, taskAgentRequestReconciliationNoticeSummary); err != nil {
		return fmt.Errorf("persist Task request reconciliation step: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_agent_stall_notices
(task_id,owner_id,message_id,step_id,task_revision,agent_generation,state,notice_kind)
VALUES ($1,$2,$3,$4,$5,$6,'active',$7)`, taskID, ownerID, messageID, stepID, revision, generation,
		taskAgentRequestReconciliationNoticeKind); err != nil {
		return fmt.Errorf("persist Task request reconciliation notice identity: %w", err)
	}
	return insertTaskAgentStallStepEvent(ctx, tx, taskID, stepID, stepSequence, taskAgentStallNoticeKind,
		"running", taskAgentRequestReconciliationNoticeTitle, taskAgentRequestReconciliationNoticeSummary)
}

func clearTaskAgentRequestReconciliationNotices(ctx context.Context, tx pgx.Tx, taskID, ownerID, conversationID string) error {
	rows, err := tx.Query(ctx, `SELECT n.message_id,n.step_id,n.task_revision
FROM task_agent_stall_notices n
WHERE n.task_id=$1 AND n.owner_id=$2 AND n.state='active' AND n.notice_kind='request_reconciliation'
FOR UPDATE`, taskID, ownerID)
	if err != nil {
		return fmt.Errorf("lock Task request reconciliation notices for resolution: %w", err)
	}
	type notice struct {
		messageID string
		stepID    string
		revision  int64
	}
	notices := make([]notice, 0)
	for rows.Next() {
		var item notice
		if err := rows.Scan(&item.messageID, &item.stepID, &item.revision); err != nil {
			rows.Close()
			return fmt.Errorf("scan Task request reconciliation notice: %w", err)
		}
		notices = append(notices, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read Task request reconciliation notices: %w", err)
	}
	rows.Close()
	for _, item := range notices {
		var stillPending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM conversation_messages
  WHERE id=$1 AND task_id=$2 AND owner_id=$3 AND conversation_id=$4
    AND role='user' AND delivery_status='pending'
)`, item.messageID, taskID, ownerID, conversationID).Scan(&stillPending); err != nil {
			return fmt.Errorf("check Task input state for request reconciliation notice: %w", err)
		}
		hasUnresolved, err := taskAgentHasPendingRequestReconciliation(ctx, tx, taskID, ownerID, item.revision)
		if err != nil {
			return err
		}
		if hasUnresolved {
			continue
		}
		summary := taskAgentRequestReconciliationClearedSummary
		if !stillPending {
			summary = taskAgentReconciliationResolvedMessageSummary
		}
		if err := clearTaskAgentStallNoticeWithSummary(ctx, tx, taskID, item.messageID, item.stepID, summary); err != nil {
			return err
		}
	}
	return nil
}

func insertTaskAgentStallStepEvent(ctx context.Context, tx pgx.Tx, taskID, stepID string, stepSequence int64, kind, status, title, summary string) error {
	eventSequence, err := incrementTaskEventSequence(ctx, tx, taskID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"step_id": stepID, "sequence": stepSequence, "kind": kind,
		"status": status, "title": title, "summary": summary, "usage_model_tokens_known": false,
		"usage_active_seconds": 0, "usage_tool_calls": 0})
	if err != nil {
		return fmt.Errorf("encode Task activity notice event: %w", err)
	}
	return insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: eventSequence, Type: "task.step", Payload: payload})
}
