package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
)

// TaskEventChainFacts returns the validation and receipt facts for one
// task's current revision: the reviewed validation state (empty when no
// reviewed validation binds the current result) and whether the result
// receipt was published. The validation predicates mirror the task
// detail projection: the review must bind the current revision, the
// current result bytes and the task.result event digest, and its artifact
// evidence must still be current.
func (s *Store) TaskEventChainFacts(ctx context.Context, ownerID, taskID string) (app.TaskEventChainFacts, error) {
	var facts app.TaskEventChainFacts
	err := s.withTaskSnapshot(ctx, func(tx pgx.Tx) error {
		taskOwnerID, err := taskOwnerIDForActor(ctx, tx, taskID, ownerID, false, false)
		if err != nil {
			return err
		}
		var revision int64
		if err := tx.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1 AND owner_id=$2`, taskID, taskOwnerID).Scan(&revision); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return missingTaskError()
			}
			return fmt.Errorf("read task revision for event chain facts: %w", err)
		}
		state, current, err := currentTaskValidationVerdictInTx(ctx, tx, taskID, taskOwnerID, revision, 0, nil)
		if err != nil {
			return fmt.Errorf("read task validation state for event chain facts: %w", err)
		}
		if current {
			facts.ValidationState = state
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM conversation_result_batches batch
  JOIN conversation_result_sources source ON source.batch_message_id=batch.message_id
 WHERE source.task_id=$1 AND source.task_revision=$2 AND batch.owner_id=$3 AND batch.state='published')`,
			taskID, revision, taskOwnerID).Scan(&facts.Delivered); err != nil {
			return fmt.Errorf("read task result receipt for event chain facts: %w", err)
		}
		return nil
	})
	if err != nil {
		return app.TaskEventChainFacts{}, err
	}
	return facts, nil
}
