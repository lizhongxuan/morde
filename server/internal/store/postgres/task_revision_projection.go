package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

// The SQLC task projection predates conversation revisions. Load these fields
// in the same read-only snapshot without changing the generated query contract.
func populateTaskRevisionProjection(ctx context.Context, tx pgx.Tx, items []task.Task) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, 0, len(items))
	positions := make(map[string]int, len(items))
	for index, item := range items {
		ids = append(ids, item.ID)
		positions[item.ID] = index
	}
	rows, err := tx.Query(ctx, `SELECT id,current_revision,desired_revision FROM tasks WHERE id=ANY($1::text[])`, ids)
	if err != nil {
		return fmt.Errorf("read Task revisions: %w", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id string
		var current, desired int64
		if err := rows.Scan(&id, &current, &desired); err != nil {
			return fmt.Errorf("scan Task revisions: %w", err)
		}
		position, exists := positions[id]
		if !exists || current < 0 || desired < current {
			return domain.NewError(domain.CodeInvalidState, "Task revision projection is invalid")
		}
		items[position].CurrentRevision, items[position].DesiredRevision = &current, &desired
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read Task revisions: %w", err)
	}
	if count != len(items) {
		return domain.NewError(domain.CodeInvalidState, "Task revision projection is incomplete")
	}
	return nil
}
