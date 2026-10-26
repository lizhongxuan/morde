package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

func (s *Store) EnsureOrphanHostVolumeCleanup(ctx context.Context, instanceID, hostID, profile, sandboxID string, generation int64, active bool) (bool, error) {
	if strings.TrimSpace(instanceID) == "" || len(instanceID) > 128 || !domain.ValidHostID(hostID) ||
		strings.TrimSpace(profile) == "" || len(profile) > 128 || strings.TrimSpace(sandboxID) == "" ||
		len(sandboxID) > 512 || generation <= 0 {
		return false, domain.InvalidArgument("orphan_host_volume_cleanup", "cleanup identity is invalid")
	}
	pending := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockHostVolumeCleanupID(ctx, tx, hostID); err != nil {
			return err
		}
		var state string
		err := tx.QueryRow(ctx, `SELECT state FROM hosts WHERE id=$1 FOR UPDATE`, hostID).Scan(&state)
		if err == nil && state != "deleted" {
			return domain.NewError(domain.CodeConflict, "refuse orphan cleanup while Host is active")
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check Host before orphan volume cleanup: %w", err)
		}
		value, err := sqlcgen.New(tx).EnsureOrphanHostVolumeCleanup(ctx, sqlcgen.EnsureOrphanHostVolumeCleanupParams{
			InstanceID: instanceID, HostID: hostID, RuntimeProfile: profile, SandboxID: sandboxID,
			RuntimeGeneration: generation, Active: active,
		})
		if err != nil {
			return fmt.Errorf("ensure orphan Host volume cleanup: %w", err)
		}
		pending = !value.Valid
		return nil
	})
	if err != nil {
		return false, err
	}
	return pending, nil
}

func (s *Store) ListOrphanHostVolumeCleanups(ctx context.Context, instanceID string) ([]string, error) {
	if strings.TrimSpace(instanceID) == "" || len(instanceID) > 128 {
		return nil, domain.InvalidArgument("orphan_host_volume_cleanup", "cleanup query is invalid")
	}
	rows, err := s.q.ListOrphanHostVolumeCleanups(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("list orphan Host volume cleanups: %w", err)
	}
	return rows, nil
}

func (s *Store) CompleteOrphanHostVolumeCleanup(ctx context.Context, instanceID, hostID string) error {
	if strings.TrimSpace(instanceID) == "" || len(instanceID) > 128 || !domain.ValidHostID(hostID) {
		return domain.InvalidArgument("orphan_host_volume_cleanup", "cleanup identity is invalid")
	}
	if err := s.q.CompleteOrphanHostVolumeCleanup(ctx, sqlcgen.CompleteOrphanHostVolumeCleanupParams{InstanceID: instanceID, HostID: hostID}); err != nil {
		return fmt.Errorf("complete orphan Host volume cleanup: %w", err)
	}
	return nil
}

func lockHostVolumeCleanupID(ctx context.Context, tx pgx.Tx, hostID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('morde:orphan-host-volume-cleanup:' || $1::text, 0))`, hostID); err != nil {
		return fmt.Errorf("lock Host volume cleanup identity: %w", err)
	}
	return nil
}

var _ app.OrphanHostVolumeCleanupStore = (*Store)(nil)
