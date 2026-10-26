package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
)

const (
	HostActivityLeaseKindGateway = "gateway"
	HostActivityLeaseKindPreview = "preview"
	HostActivityLeaseMinTTL      = time.Second
	HostActivityLeaseMaxTTL      = 5 * time.Minute
)

// HostActivityLease identifies one active user-facing connection to a Host.
// RuntimeGeneration and ControlVersion fence leases across Host replacement
// and control handoff. TTL is bounded to keep abandoned connections short-lived.
type HostActivityLease struct {
	HostID            string
	LeaseID           string
	RuntimeGeneration int64
	ControlVersion    int64
	Kind              string
	TTL               time.Duration
}

type HostActivityLeaseStore interface {
	AcquireHostActivityLease(context.Context, HostActivityLease) error
	RenewHostActivityLease(context.Context, HostActivityLease) error
	ReleaseHostActivityLease(context.Context, HostActivityLease) error
	HasActiveHostActivityLease(context.Context, string, int64) (bool, error)
}

// HostActivityLeaseDrainer reports leases from the pre-transition fence even
// after they stop counting as current activity. Control handoffs use it before
// opening the target control mode.
type HostActivityLeaseDrainer interface {
	HasOutstandingControlTransitionLeases(context.Context, string, int64) (bool, error)
}

// HostInputPermitStore linearizes a bounded user input write against a Host
// control transition. The callback must only write a parsed input message.
type HostInputPermitStore interface {
	WithHostInputPermit(context.Context, host.Ref, func() error) error
}

// ValidateHostActivityLease checks the shared lease identity and fence fields.
// TTL is validated separately because release does not use one.
func ValidateHostActivityLease(lease HostActivityLease) error {
	if !domain.ValidHostID(lease.HostID) {
		return domain.InvalidArgument("host_id", "host activity lease host id is invalid")
	}
	if lease.LeaseID == "" || len(lease.LeaseID) > 128 {
		return domain.InvalidArgument("lease_id", "host activity lease id is invalid")
	}
	for _, r := range lease.LeaseID {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_", r) {
			return domain.InvalidArgument("lease_id", "host activity lease id is invalid")
		}
	}
	if lease.RuntimeGeneration <= 0 || lease.ControlVersion < 0 {
		return domain.InvalidArgument("runtime_generation", "host activity lease fence is invalid")
	}
	if !ValidHostActivityLeaseKind(lease.Kind) {
		return domain.InvalidArgument("kind", "host activity lease kind is invalid")
	}
	return nil
}

func ValidHostActivityLeaseKind(kind string) bool {
	return kind == HostActivityLeaseKindGateway || kind == HostActivityLeaseKindPreview
}

func ValidateHostActivityLeaseTTL(ttl time.Duration) error {
	if ttl < HostActivityLeaseMinTTL || ttl > HostActivityLeaseMaxTTL {
		return domain.InvalidArgument("ttl", fmt.Sprintf("host activity lease ttl must be between %s and %s", HostActivityLeaseMinTTL, HostActivityLeaseMaxTTL))
	}
	return nil
}

func (lease HostActivityLease) ValidateForAcquireOrRenew() error {
	if err := ValidateHostActivityLease(lease); err != nil {
		return err
	}
	return ValidateHostActivityLeaseTTL(lease.TTL)
}
