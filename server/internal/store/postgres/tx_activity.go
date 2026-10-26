package postgres

import (
	"context"
	"fmt"
	"time"

	"morde/server/internal/domain"
)

// TouchHosts persists activity for a batch with one UPDATE, keeping the
// worker's in-memory activity flush out of per-host round trips.
func (s *Store) TouchHosts(ctx context.Context, hostIDs []string, at time.Time) error {
	if len(hostIDs) == 0 {
		return nil
	}
	if at.IsZero() {
		return domain.InvalidArgument("at", "activity timestamp is required")
	}
	if _, err := s.pool.Exec(ctx, `
UPDATE hosts
SET last_active_at = $2, updated_at = $2
WHERE id = ANY($1::text[]) AND deleted_at IS NULL`, hostIDs, at); err != nil {
		return fmt.Errorf("touch hosts: %w", err)
	}
	return nil
}
