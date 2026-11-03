package app

import (
	"context"
	"time"

	"morde/server/internal/domain/host"
)

const HostControlLeaseTTL = 30 * time.Second

type HostControlLease struct {
	HostID         string
	ActorUserID    string
	ControlVersion int64
	ExpiresAt      *time.Time
	Active         bool
}

// HostControlLeaseStore serializes control renewal and user-originated writes
// with Host mode transitions.
type HostControlLeaseStore interface {
	GetHostControlLease(context.Context, string) (HostControlLease, error)
	RenewHostControlLease(context.Context, string, string, int64) (HostControlLease, error)
	CheckHostControlLease(context.Context, string, string, int64, int64) error
	WithHostControlPermit(context.Context, host.Ref, string, host.Permission, func() error) error
}
