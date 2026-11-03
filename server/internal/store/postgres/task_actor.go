package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func taskOwnerIDForActor(ctx context.Context, tx pgx.Tx, taskID, actorID string, write, lock bool) (string, error) {
	lockClause := ""
	if lock {
		lockClause = " FOR UPDATE OF t"
	}
	var ownerID string
	err := tx.QueryRow(ctx, `SELECT t.owner_id FROM tasks t
WHERE t.id=$1 AND (
  (t.project_id IS NULL AND t.owner_id=$2)
  OR (t.project_id IS NOT NULL AND EXISTS (
    SELECT 1 FROM project_members pm JOIN users u ON u.id=pm.user_id AND u.status='approved'
    WHERE pm.project_id=t.project_id AND pm.user_id=$2
      AND (NOT $3::boolean OR (pm.role IN ('member','owner') AND t.initiator_id=$2))
  ))
)`+lockClause, taskID, actorID, write).Scan(&ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", missingTaskError()
	}
	if err != nil {
		return "", err
	}
	return ownerID, nil
}
