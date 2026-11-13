package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func assignTaskStepSequences(ctx context.Context, tx pgx.Tx, taskID string, steps []task.Step) error {
	if len(steps) == 0 {
		return nil
	}
	var last int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0) FROM task_steps WHERE task_id=$1`, taskID).Scan(&last); err != nil {
		return fmt.Errorf("read TaskStep sequence head: %w", err)
	}
	for index := range steps {
		var existingTaskID string
		var existingSequence int64
		err := tx.QueryRow(ctx, `SELECT task_id,sequence FROM task_steps WHERE id=$1 FOR UPDATE`, steps[index].ID).
			Scan(&existingTaskID, &existingSequence)
		if err == nil {
			if existingTaskID != taskID {
				return domain.NewError(domain.CodeInvalidState, "task step identity changed")
			}
			steps[index].Sequence = existingSequence
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read TaskStep identity: %w", err)
		}
		if steps[index].Sequence <= last {
			if last == math.MaxInt64 {
				return domain.NewError(domain.CodeInvalidState, "TaskStep sequence is exhausted")
			}
			last++
			steps[index].Sequence = last
		} else {
			last = steps[index].Sequence
		}
	}
	return nil
}

func alignTaskStepEventSequence(payload []byte, steps []task.Step) ([]byte, error) {
	var event map[string]json.RawMessage
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, fmt.Errorf("decode TaskStep event: %w", err)
	}
	var stepID string
	if raw := event["step_id"]; len(raw) == 0 || json.Unmarshal(raw, &stepID) != nil {
		return payload, nil
	}
	for _, step := range steps {
		if step.ID == stepID {
			sequence, err := json.Marshal(step.Sequence)
			if err != nil {
				return nil, fmt.Errorf("encode TaskStep event sequence: %w", err)
			}
			event["sequence"] = sequence
			result, err := json.Marshal(event)
			if err != nil {
				return nil, fmt.Errorf("encode TaskStep event: %w", err)
			}
			return result, nil
		}
	}
	return payload, nil
}
