package postgres

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"

	"github.com/jackc/pgx/v5/pgxpool"
)

const leaderLockKey int64 = 0x43485245434f4e43           // "CHRECONC"
const workspaceStorageLockKey int64 = 0x4d4f52444553544f // "MORDESTO"

type advisoryLock struct {
	key    int64
	shared bool
}

// TryLeaderLock holds a PostgreSQL session advisory lock on a dedicated pool
// connection for the full lifetime of the returned handle.
func (s *Store) TryLeaderLock(ctx context.Context) (app.LeaderLock, bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire reconciler lock connection: %w", err)
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::bigint)`, leaderLockKey).Scan(&acquired); err != nil {
		_ = discardLeaderConnection(conn)
		return nil, false, fmt.Errorf("try reconciler advisory lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return &sessionLeaderLock{conn: conn, locks: []advisoryLock{{key: leaderLockKey}}}, true, nil
}

func (s *Store) TryHostCreateLock(ctx context.Context, hostID string) (app.LeaderLock, bool, error) {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte("cloudhost:create:" + hostID))
	key := int64(hasher.Sum64())
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire host create lock connection: %w", err)
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::bigint)`, key).Scan(&acquired); err != nil {
		_ = discardLeaderConnection(conn)
		return nil, false, fmt.Errorf("try host create advisory lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return &sessionLeaderLock{conn: conn, locks: []advisoryLock{{key: key}}}, true, nil
}

// TryWorkspaceStorageReconciliationLock excludes block-store writers and
// deleters while startup repairs the physical usage ledger.
func (s *Store) TryWorkspaceStorageReconciliationLock(ctx context.Context) (app.LeaderLock, bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire workspace reconciliation lock connection: %w", err)
	}
	acquired, err := tryAdvisoryLock(ctx, conn, workspaceStorageLockKey, false)
	if err != nil {
		_ = discardLeaderConnection(conn)
		return nil, false, fmt.Errorf("try workspace reconciliation advisory lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return &sessionLeaderLock{conn: conn, locks: []advisoryLock{{key: workspaceStorageLockKey}}}, true, nil
}

func (s *Store) TryWorkspaceBlockStoreLock(ctx context.Context, workspaceID string) (app.LeaderLock, bool, error) {
	return s.tryWorkspaceBlockStoreLock(ctx, workspaceID, false)
}

func (s *Store) TryWorkspaceBlockStoreReadLock(ctx context.Context, workspaceID string) (app.LeaderLock, bool, error) {
	return s.tryWorkspaceBlockStoreLock(ctx, workspaceID, true)
}

func (s *Store) tryWorkspaceBlockStoreLock(ctx context.Context, workspaceID string, shared bool) (app.LeaderLock, bool, error) {
	if !domain.ValidWorkspaceID(workspaceID) {
		return nil, false, domain.InvalidArgument("workspace_id", "workspace id is invalid")
	}
	if s.pool.Config().MaxConns < 2 {
		return nil, false, fmt.Errorf("workspace block store lock requires at least two database connections")
	}
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte("morde:workspace-block-store:" + workspaceID))
	key := int64(hasher.Sum64())
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire workspace block store lock connection: %w", err)
	}
	globalAcquired, err := tryAdvisoryLock(ctx, conn, workspaceStorageLockKey, true)
	if err != nil {
		_ = discardLeaderConnection(conn)
		return nil, false, fmt.Errorf("try global workspace storage advisory lock: %w", err)
	}
	if !globalAcquired {
		conn.Release()
		return nil, false, nil
	}
	workspaceAcquired, err := tryAdvisoryLock(ctx, conn, key, shared)
	if err != nil {
		_ = discardLeaderConnection(conn)
		return nil, false, fmt.Errorf("try workspace block store advisory lock: %w", err)
	}
	if !workspaceAcquired {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock_shared($1::bigint)`, workspaceStorageLockKey).Scan(&unlocked); err != nil || !unlocked {
			_ = discardLeaderConnection(conn)
			if err != nil {
				return nil, false, fmt.Errorf("release global workspace storage advisory lock: %w", err)
			}
			return nil, false, fmt.Errorf("release global workspace storage advisory lock: lock was not held")
		}
		conn.Release()
		return nil, false, nil
	}
	return &sessionLeaderLock{conn: conn, locks: []advisoryLock{{key: workspaceStorageLockKey, shared: true}, {key: key, shared: shared}}}, true, nil
}

type sessionLeaderLock struct {
	conn  *pgxpool.Conn
	locks []advisoryLock

	mu         sync.Mutex
	released   bool
	releaseErr error
}

var _ app.LeaderLockStore = (*Store)(nil)
var _ app.HostCreateLockStore = (*Store)(nil)

func (lock *sessionLeaderLock) Release(ctx context.Context) error {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.released {
		return lock.releaseErr
	}
	lock.released = true

	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for i := len(lock.locks) - 1; i >= 0; i-- {
		item := lock.locks[i]
		unlockQuery := `SELECT pg_advisory_unlock($1::bigint)`
		if item.shared {
			unlockQuery = `SELECT pg_advisory_unlock_shared($1::bigint)`
		}
		var unlocked bool
		if err := lock.conn.QueryRow(unlockCtx, unlockQuery, item.key).Scan(&unlocked); err != nil {
			_ = discardLeaderConnection(lock.conn)
			lock.releaseErr = fmt.Errorf("release session advisory lock: %w", err)
			return lock.releaseErr
		}
		if !unlocked {
			_ = discardLeaderConnection(lock.conn)
			lock.releaseErr = fmt.Errorf("release session advisory lock: lock was not held")
			return lock.releaseErr
		}
	}
	lock.conn.Release()
	return lock.releaseErr
}

func tryAdvisoryLock(ctx context.Context, conn *pgxpool.Conn, key int64, shared bool) (bool, error) {
	query := `SELECT pg_try_advisory_lock($1::bigint)`
	if shared {
		query = `SELECT pg_try_advisory_lock_shared($1::bigint)`
	}
	var acquired bool
	if err := conn.QueryRow(ctx, query, key).Scan(&acquired); err != nil {
		return false, err
	}
	return acquired, nil
}

func discardLeaderConnection(conn *pgxpool.Conn) error {
	pgConn := conn.Hijack()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return pgConn.Close(ctx)
}
