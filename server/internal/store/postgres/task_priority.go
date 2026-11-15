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
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) SetTaskPriority(ctx context.Context, record app.IdempotencyRecord, taskID string,
	priority task.QueuePriority, expectedEventSequence int64, requestID string) (task.Task, error) {
	if !domain.ValidTaskID(taskID) || !domain.ValidTaskActionID(requestID) || !priority.Valid() || expectedEventSequence < 0 {
		return task.Task{}, domain.InvalidArgument("priority", "task priority fields are invalid")
	}
	if err := validateIdempotency(record); err != nil {
		return task.Task{}, err
	}
	if record.Operation != "task.priority.set" || record.ResourceID != requestID || record.RelatedID != taskID || record.OwnerID == "" {
		return task.Task{}, domain.InvalidArgument("idempotency", "task priority idempotency identity is invalid")
	}

	var result task.Task
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		taskOwnerID, err := taskOwnerIDForActor(ctx, tx, taskID, record.OwnerID, true, true)
		if err != nil {
			return err
		}
		var status string
		var eventSequence int64
		var currentPriority task.QueuePriority
		err = tx.QueryRow(ctx, `SELECT status, event_seq, queue_priority FROM tasks WHERE id=$1 AND owner_id=$2`,
			taskID, taskOwnerID).Scan(&status, &eventSequence, &currentPriority)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("read task priority fence: %w", err)
		}
		stored, replayed, err := findIdempotency(ctx, tx, record)
		if err != nil {
			return err
		}
		if replayed {
			if !domain.ValidTaskActionID(stored.ResourceID) || stored.RelatedID != taskID {
				return idempotencyConflict()
			}
		} else {
			if status != string(task.StatusQueued) {
				return domain.NewError(domain.CodeConflict, "task priority can only change while a task is queued")
			}
			if eventSequence != expectedEventSequence {
				return domain.NewError(domain.CodeConflict, "task event sequence does not match expected_event_sequence")
			}
			stored, replayed, err = reserveIdempotency(ctx, tx, record, requestID, taskID)
			if err != nil {
				return err
			}
			if replayed && (!domain.ValidTaskActionID(stored.ResourceID) || stored.RelatedID != taskID) {
				return idempotencyConflict()
			}
			if !replayed && currentPriority != priority {
				var nextSequence int64
				err = tx.QueryRow(ctx, `UPDATE tasks SET queue_priority=$3, event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND status='queued' AND event_seq=$4 RETURNING event_seq`,
					taskID, taskOwnerID, string(priority), expectedEventSequence).Scan(&nextSequence)
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.NewError(domain.CodeConflict, "task state changed while setting priority")
				}
				if err != nil {
					return fmt.Errorf("set task queue priority: %w", err)
				}
				payload, err := json.Marshal(struct {
					Priority         task.QueuePriority `json:"priority"`
					PreviousPriority task.QueuePriority `json:"previous_priority"`
				}{priority, currentPriority})
				if err != nil {
					return fmt.Errorf("encode task priority event: %w", err)
				}
				if err := insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: nextSequence, Type: "task.priority", Payload: payload}); err != nil {
					return err
				}
			}
		}
		row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: taskID, OwnerID: taskOwnerID})
		if err != nil {
			return fmt.Errorf("read task after priority update: %w", err)
		}
		result = rowTaskDomain(row)
		return nil
	})
	return result, err
}
