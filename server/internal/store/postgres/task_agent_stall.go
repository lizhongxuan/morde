package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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
		}
		rows, err := tx.Query(ctx, `SELECT message_id,step_id,state FROM task_agent_stall_notices
WHERE task_id=$1 AND owner_id=$2 FOR UPDATE`, value.ID, value.OwnerID)
		if err != nil {
			return fmt.Errorf("lock Task agent stall notices: %w", err)
		}
		notices := make([]notice, 0)
		existing := make(map[string]struct{})
		for rows.Next() {
			var item notice
			if err := rows.Scan(&item.messageID, &item.stepID, &item.state); err != nil {
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
			if stillPending && (!known || stalled) {
				continue
			}
			if err := clearTaskAgentStallNotice(ctx, tx, value.ID, item.messageID, item.stepID, stillPending); err != nil {
				return err
			}
		}
		if !known || !stalled {
			return nil
		}
		for _, messageID := range pendingMessages {
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
activity_generation,activity_revision,activity_heartbeat_at,
COALESCE(activity_heartbeat_at<=clock_timestamp()-($4::bigint*interval '1 millisecond'),false)
FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2
  AND (parent_session_id IS NULL OR task_revision=$3)
ORDER BY (parent_session_id IS NULL) DESC,created_at,session_id`, value.ID, value.OwnerID, *value.CurrentRevision,
		staleAfter.Milliseconds())
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
		var heartbeat pgtype.Timestamptz
		var heartbeatStale bool
		if err := rows.Scan(&sessionID, &parentSessionID, &status, &generation, &revision, &heartbeat, &heartbeatStale); err != nil {
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
			stalled = stalled || heartbeatStale
			continue
		}
		if !generation.Valid || generation.Int64 != value.AgentGeneration {
			continue
		}
		if !revision.Valid || revision.Int64 != *value.CurrentRevision || !status.Valid {
			unknown = true
			continue
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
(task_id,owner_id,message_id,step_id,task_revision,agent_generation,state)
VALUES ($1,$2,$3,$4,$5,$6,'active')`, value.ID, value.OwnerID, messageID, stepID, revision, generation); err != nil {
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
	rows, err := tx.Query(ctx, `SELECT message_id,step_id FROM task_agent_stall_notices
WHERE task_id=$1 AND owner_id=$2 AND state='active' FOR UPDATE`, taskID, ownerID)
	if err != nil {
		return fmt.Errorf("lock Task activity notices before clearing on progress: %w", err)
	}
	type item struct{ messageID, stepID string }
	items := make([]item, 0)
	for rows.Next() {
		var notice item
		if err := rows.Scan(&notice.messageID, &notice.stepID); err != nil {
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
		if err := clearTaskAgentStallNotice(ctx, tx, taskID, notice.messageID, notice.stepID, true); err != nil {
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
