package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func (s *Store) RecordHostReplicaHeartbeat(ctx context.Context, hostID string, runtimeGeneration int64) error {
	if !domain.ValidHostID(hostID) || runtimeGeneration < 1 {
		return domain.InvalidArgument("host_id", "Host Runtime heartbeat is invalid")
	}
	var ownerID string
	if err := s.pool.QueryRow(ctx, `SELECT owner_id FROM hosts WHERE id=$1`, hostID).Scan(&ownerID); err != nil {
		return storeError("read Host owner before heartbeat", "host", err)
	}
	if !domain.ValidUserID(ownerID) {
		return domain.NewError(domain.CodeInternal, "Host owner identity is invalid")
	}
	return s.withApprovedWorkspaceSyncOwnerTx(ctx, ownerID, func(tx pgx.Tx) error {
		result, err := tx.Exec(ctx, `
INSERT INTO host_runtime_heartbeats (host_id, runtime_generation, last_seen_at)
SELECT h.id, h.runtime_generation, clock_timestamp()
FROM hosts AS h
WHERE h.id=$1 AND h.owner_id=$3 AND h.runtime_generation=$2 AND h.state IN ('creating','running') AND h.deleted_at IS NULL
ON CONFLICT (host_id) DO UPDATE
SET runtime_generation=EXCLUDED.runtime_generation,
    last_seen_at=CASE
      WHEN host_runtime_heartbeats.runtime_generation=EXCLUDED.runtime_generation
        THEN GREATEST(host_runtime_heartbeats.last_seen_at, EXCLUDED.last_seen_at)
      ELSE EXCLUDED.last_seen_at
    END
WHERE host_runtime_heartbeats.runtime_generation <= EXCLUDED.runtime_generation`, hostID, runtimeGeneration, ownerID)
		if err != nil {
			return fmt.Errorf("record Host Runtime heartbeat: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeUnauthenticated, "Host Runtime generation is no longer active")
		}
		return nil
	})
}

var _ app.HostReplicaLivenessStore = (*Store)(nil)
