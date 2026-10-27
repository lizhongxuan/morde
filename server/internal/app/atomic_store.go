package app

import (
	"context"
	"time"

	"morde/server/internal/domain/host"
)

// Capacity is the configured cluster-wide limit used during host creation.
type Capacity struct {
	CPUMilli int64
	MemMiB   int64
}

// CreateHostParams keeps host insertion, workspace bindings, and allocation
// checks in one store operation.
type CreateHostParams struct {
	Host         host.Host
	WorkspaceIDs []string
	Capacity     Capacity
	Idempotency  IdempotencyRecord
}

// RestoreHostParams are intentionally kept out of CreateHostParams: restoring
// a stopped Host must retain its identity and both deterministic data volumes.
type RestoreHostParams struct {
	HostID               string
	RuntimeCredentialEnc []byte
	ExpiresAt            time.Time
	Capacity             Capacity
}

// LeaderLock is held until Release is called. The PostgreSQL implementation
// keeps the session that owns the advisory lock checked out until then.
type LeaderLock interface {
	Release(context.Context) error
}

type LeaderLockStore interface {
	TryLeaderLock(context.Context) (LeaderLock, bool, error)
}

type HostCreateLockStore interface {
	TryHostCreateLock(context.Context, string) (LeaderLock, bool, error)
}
