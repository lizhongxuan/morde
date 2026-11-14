package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func (s *Store) GetCloudDSHTaskPlanReceipt(ctx context.Context, taskID, ownerID, sessionID, callID string) (task.Plan, error) {
	var result task.Plan
	err := s.withTaskSnapshot(ctx, func(tx pgx.Tx) error {
		_, err := taskOwnerIDForActor(ctx, tx, taskID, ownerID, false, false)
		if err != nil {
			return err
		}
		var payload []byte
		err = tx.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id=$1 AND event_type='task.plan'
AND payload #>> '{detail,call_id}'=$2 ORDER BY sequence DESC LIMIT 1`, taskID, callID).Scan(&payload)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "task plan receipt not found")
		}
		if err != nil {
			return fmt.Errorf("read task plan receipt: %w", err)
		}
		result, err = app.ReadCloudDSHTaskPlanReceipt(payload, taskID, ownerID, sessionID, callID)
		if err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func latestTaskPlan(ctx context.Context, tx pgx.Tx, taskID string, taskRevision int64) (*task.Plan, error) {
	var payload []byte
	err := tx.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id=$1 AND event_type='task.plan'
AND payload #>> '{plan,task_revision}'=$2 ORDER BY sequence DESC LIMIT 1`, taskID, fmt.Sprint(taskRevision)).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read latest task plan: %w", err)
	}
	var event appTaskPlanEvent
	if len(payload) > 64<<10 || json.Unmarshal(payload, &event) != nil || event.Plan.Validate() != nil ||
		event.Plan.TaskRevision != taskRevision || event.PlanRevision != event.Plan.Revision || event.TaskRevision != taskRevision {
		return nil, domain.NewError(domain.CodeInvalidState, "stored task plan is invalid")
	}
	return &event.Plan, nil
}

type appTaskPlanEvent struct {
	TaskRevision int64     `json:"task_revision"`
	PlanRevision int64     `json:"plan_revision"`
	Plan         task.Plan `json:"plan"`
	Detail       struct {
		CallID         string `json:"call_id"`
		AgentSessionID string `json:"agent_session_id"`
	} `json:"detail"`
}

func findCloudDSHTaskPlanEvent(ctx context.Context, tx pgx.Tx, taskID, callID string) ([]byte, bool, error) {
	var payload []byte
	err := tx.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id=$1 AND event_type='task.plan'
AND payload #>> '{detail,call_id}'=$2 ORDER BY sequence LIMIT 1`, taskID, callID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("find previous task plan call: %w", err)
	}
	return payload, true, nil
}

func validateCloudDSHTaskPlanProgress(ctx context.Context, tx pgx.Tx, current task.Task, progress app.TaskProgress, commit app.CloudDSHTaskPlanCommit) (task.Event, bool, error) {
	if current.ID != commit.TaskID || current.OwnerID != commit.OwnerID || current.ExecutionLocation != "server" ||
		current.AgentSessionID != commit.AgentSessionID || current.Authorization.Version != commit.AuthorizationVersion ||
		current.CurrentRevision == nil || *current.CurrentRevision != commit.Plan.TaskRevision {
		return task.Event{}, false, domain.NewError(domain.CodeInvalidState, "task plan revision or authorization changed")
	}
	var authorizationValid bool
	if err := tx.QueryRow(ctx, `SELECT auth_kind='schedule_task' OR auth_expires_at > clock_timestamp() FROM tasks WHERE id=$1`, current.ID).Scan(&authorizationValid); err != nil {
		return task.Event{}, false, fmt.Errorf("check task plan authorization expiry: %w", err)
	}
	if !authorizationValid {
		return task.Event{}, false, domain.NewError(domain.CodeUnauthenticated, "task authorization is unavailable")
	}
	if err := app.ValidateCloudDSHTaskPlanEvent(progress.Event.Payload, commit); err != nil {
		return task.Event{}, false, err
	}
	if priorPayload, found, err := findCloudDSHTaskPlanEvent(ctx, tx, current.ID, commit.CallID); err != nil {
		return task.Event{}, false, err
	} else if found {
		if app.ValidateCloudDSHTaskPlanEvent(priorPayload, commit) != nil {
			return task.Event{}, false, domain.NewError(domain.CodeIdempotencyConflict, "task plan call identity was reused")
		}
		var prior task.Event
		if err := tx.QueryRow(ctx, `SELECT task_id,sequence,event_type,payload,created_at FROM task_events
WHERE task_id=$1 AND event_type='task.plan' AND payload #>> '{detail,call_id}'=$2 ORDER BY sequence LIMIT 1`, current.ID, commit.CallID).
			Scan(&prior.TaskID, &prior.Sequence, &prior.Type, &prior.Payload, &prior.CreatedAt); err != nil {
			return task.Event{}, false, fmt.Errorf("read replayed task plan event: %w", err)
		}
		return prior, true, nil
	}
	var previousRevision int64
	err := tx.QueryRow(ctx, `SELECT COALESCE(MAX((payload #>> '{plan,revision}')::bigint),0) FROM task_events
WHERE task_id=$1 AND event_type='task.plan' AND payload #>> '{plan,task_revision}'=$2`, current.ID, fmt.Sprint(commit.Plan.TaskRevision)).Scan(&previousRevision)
	if err != nil {
		return task.Event{}, false, fmt.Errorf("read prior task plan revision: %w", err)
	}
	if commit.Plan.Revision != previousRevision+1 {
		return task.Event{}, false, domain.NewError(domain.CodeConflict, "task plan revision is stale")
	}
	return task.Event{}, false, nil
}
