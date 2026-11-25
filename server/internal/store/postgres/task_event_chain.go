package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

// GetTaskEventChain is the E08 read-side facts reader: it loads the full
// stored event list of one task plus the two non-event facts the nine-kind
// projection needs (the reviewed validation verdict for the current revision
// and whether its result receipt has been published), then projects the
// chain. Nothing is written here; the write side keeps its eleven `task.*`
// event types untouched. The read runs in a repeatable-read snapshot so the
// events, the verdict and the receipt are from one consistent view.
func (s *Store) GetTaskEventChain(ctx context.Context, ownerID, taskID string) (app.TaskEventChain, error) {
	var chain app.TaskEventChain
	err := s.withTaskSnapshot(ctx, func(tx pgx.Tx) error {
		taskOwnerID, err := taskOwnerIDForActor(ctx, tx, taskID, ownerID, false, false)
		if err != nil {
			return err
		}
		var currentRevision int64
		if err := tx.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1 AND owner_id=$2`,
			taskID, taskOwnerID).Scan(&currentRevision); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return missingTaskError()
			}
			return fmt.Errorf("read task revision for event chain: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT task_id,sequence,event_type,payload,created_at
FROM task_events WHERE task_id=$1 ORDER BY sequence`, taskID)
		if err != nil {
			return fmt.Errorf("list task events for event chain: %w", err)
		}
		events := make([]task.Event, 0)
		for rows.Next() {
			var event task.Event
			if err := rows.Scan(&event.TaskID, &event.Sequence, &event.Type, &event.Payload, &event.CreatedAt); err != nil {
				rows.Close()
				return fmt.Errorf("read task event for event chain: %w", err)
			}
			events = append(events, event)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate task events for event chain: %w", err)
		}
		rows.Close()
		facts, err := loadTaskEventChainFacts(ctx, tx, taskID, taskOwnerID, currentRevision)
		if err != nil {
			return err
		}
		projected, err := app.ProjectTaskEventChain(events, facts)
		if err != nil {
			return err
		}
		chain = projected
		return nil
	})
	return chain, err
}

// loadTaskEventChainFacts reads the two non-event facts for one task revision
// in the caller's snapshot: the reviewed validation verdict (only a
// `reviewed` row counts; a pending or processing review leaves the verdict
// empty so no verification node is projected) and the published result
// receipt, if any. A verdict without a receipt, or a receipt without a
// passing verdict, never yields a delivery node; the projection enforces
// that, this loader only reports the facts.
func loadTaskEventChainFacts(ctx context.Context, tx pgx.Tx, taskID, ownerID string, revision int64) (app.TaskEventChainFacts, error) {
	facts := app.TaskEventChainFacts{}
	var state string
	var resultSequence int64
	err := tx.QueryRow(ctx, `SELECT COALESCE(validation_state,''),result_event_sequence
FROM task_validation_reviews WHERE task_id=$1 AND owner_id=$2 AND task_revision=$3 AND state='reviewed'`,
		taskID, ownerID, revision).Scan(&state, &resultSequence)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return facts, fmt.Errorf("read task validation verdict for event chain: %w", err)
	}
	if err == nil {
		facts.ValidationState = app.TaskValidationState(state)
		facts.ResultEventSequence = resultSequence
	}
	var receiptSequence int64
	err = tx.QueryRow(ctx, `SELECT source.event_sequence
FROM conversation_result_sources source
JOIN conversation_result_batches batch ON batch.message_id=source.batch_message_id
WHERE source.task_id=$1 AND source.task_revision=$2 AND source.owner_id=$3 AND source.state='published' AND batch.state='published'
ORDER BY source.event_sequence LIMIT 1`, taskID, revision, ownerID).Scan(&receiptSequence)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return facts, fmt.Errorf("read task result receipt for event chain: %w", err)
	}
	if err == nil {
		facts.ReceiptPublished = true
		facts.ReceiptEventSequence = receiptSequence
	}
	return facts, nil
}
