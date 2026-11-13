package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func (s *Store) GetTaskStepDetail(ctx context.Context, actorID, taskID, stepID string) (app.TaskStepDetailRecord, error) {
	var result app.TaskStepDetailRecord
	err := s.withTaskSnapshot(ctx, func(tx pgx.Tx) error {
		ownerID, err := taskOwnerIDForActor(ctx, tx, taskID, actorID, false, false)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT t.id,t.owner_id,st.id,t.event_seq,d.task_revision,d.payload_ciphertext,d.payload_sha256,d.payload_size_bytes,d.truncated
FROM tasks t JOIN task_steps st ON st.task_id=t.id AND st.id=$3
JOIN task_step_details d ON d.task_id=st.task_id AND d.step_id=st.id
WHERE t.id=$1 AND t.owner_id=$2`, taskID, ownerID, stepID).Scan(
			&result.TaskID, &result.OwnerID, &result.StepID, &result.EventSequence, &result.TaskRevision,
			&result.Ciphertext, &result.SHA256, &result.SizeBytes, &result.Truncated)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "task step detail not found")
		}
		if err != nil {
			return fmt.Errorf("read task step detail: %w", err)
		}
		return nil
	})
	return result, err
}
