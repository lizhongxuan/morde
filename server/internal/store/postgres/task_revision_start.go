package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

// The caller has already locked and authorized the current Task and Agent,
// and commits the root checkpoint in the same transaction as this event.
func recordTaskRevisionStartedInTx(ctx context.Context, tx pgx.Tx, taskID string) error {
	var current, desired int64
	if err := tx.QueryRow(ctx, `SELECT current_revision,desired_revision FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&current, &desired); err != nil {
		return fmt.Errorf("read task revision before start: %w", err)
	}
	if current < 0 || current != desired {
		return domain.NewError(domain.CodeInvalidState, "task revision changed before start")
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM task_revision_starts WHERE task_id=$1 AND revision=$2)`, taskID, current).Scan(&exists); err != nil {
		return fmt.Errorf("check task revision start: %w", err)
	}
	if exists {
		return nil
	}
	var previous int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0) FROM task_steps WHERE task_id=$1`, taskID).Scan(&previous); err != nil {
		return fmt.Errorf("read task step sequence: %w", err)
	}
	if previous == math.MaxInt64 {
		return domain.NewError(domain.CodeInvalidState, "task step sequence is exhausted")
	}
	stepID, err := domain.NewStepID()
	if err != nil {
		return fmt.Errorf("create task start step identity: %w", err)
	}
	const title, summary = "开始处理", "正在处理你的请求。"
	if _, err := tx.Exec(ctx, `INSERT INTO task_steps (id,task_id,sequence,kind,status,title,summary)
VALUES ($1,$2,$3,'progress','succeeded',$4,$5)`, stepID, taskID, previous+1, title, summary); err != nil {
		return fmt.Errorf("insert task revision start step: %w", err)
	}
	eventSequence, err := incrementTaskEventSequence(ctx, tx, taskID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"phase": "started", "step_id": stepID, "title": title, "summary": summary, "revision": current})
	if err != nil {
		return fmt.Errorf("encode task revision start event: %w", err)
	}
	if err := insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: eventSequence, Type: "task.step", Payload: payload}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_revision_starts (task_id,revision,step_id,event_sequence) VALUES ($1,$2,$3,$4)`,
		taskID, current, stepID, eventSequence); err != nil {
		return fmt.Errorf("record task revision start: %w", err)
	}
	return nil
}
