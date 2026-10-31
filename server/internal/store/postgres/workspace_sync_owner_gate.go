package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
)

func (s *Store) withWorkspaceSyncOwnerTx(ctx context.Context, ownerID string, apply func(pgx.Tx) error, afterCommit func(context.Context) error, disableAfterCommitFailure func(context.Context) error) (retErr error) {
	if ctx == nil || !domain.ValidUserID(ownerID) || apply == nil {
		return domain.InvalidArgument("owner_id", "Workspace sync owner transaction is invalid")
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire Workspace sync owner connection: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		unlockErr := conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended('morde:workspace-sync-owner:' || $1, 0))`, ownerID).Scan(&unlocked)
		if unlockErr != nil || !unlocked {
			if unlockErr == nil {
				unlockErr = errors.New("owner advisory lock was not held")
			}
			retErr = errors.Join(retErr, fmt.Errorf("release Workspace sync owner advisory lock: %w", unlockErr))
			_ = conn.Hijack().Close(unlockCtx)
			return
		}
		conn.Release()
	}()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('morde:workspace-sync-owner:' || $1, 0))`, ownerID); err != nil {
		return fmt.Errorf("lock Workspace sync owner relay lifecycle: %w", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin Workspace sync owner transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := apply(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit Workspace sync owner transaction: %w", err)
	}
	if afterCommit == nil {
		return nil
	}
	if err := afterCommit(ctx); err != nil {
		result := fmt.Errorf("apply Workspace sync relay gate after owner commit: %w", err)
		if disableAfterCommitFailure != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if gateErr := disableAfterCommitFailure(cleanupCtx); gateErr != nil {
				result = errors.Join(result, fmt.Errorf("close Workspace sync relay gate after failed activation: %w", gateErr))
			}
		}
		return result
	}
	return nil
}
