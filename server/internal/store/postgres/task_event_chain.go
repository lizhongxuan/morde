package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
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
		var state pgtype.Text
		err = tx.QueryRow(ctx, `SELECT review.validation_state
FROM task_validation_reviews review
JOIN tasks task ON task.id=review.task_id AND task.status='completed'
  AND task.current_revision=review.task_revision AND task.desired_revision=review.task_revision
  AND task.result<>'' AND sha256(convert_to(task.result,'UTF8'))=review.result_sha256
JOIN task_events event ON event.task_id=review.task_id AND event.sequence=review.result_event_sequence
  AND event.task_revision=review.task_revision AND event.event_type='task.result'
  AND event.result_content_sha256=review.result_sha256
WHERE review.task_id=$1 AND review.owner_id=$2 AND review.task_revision=$3 AND review.state='reviewed'
  AND task_validation_artifact_evidence_current(review.task_id, review.owner_id, review.artifact_content_evidence)`,
			taskID, taskOwnerID, revision).Scan(&state)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read task validation state for event chain facts: %w", err)
		}
		if state.Valid {
			facts.ValidationState = app.TaskValidationState(state.String)
			if !app.ValidTaskValidationState(facts.ValidationState) {
				return domain.NewError(domain.CodeInvalidState, "task validation state is invalid")
			}
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
