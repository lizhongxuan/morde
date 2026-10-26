package postgres

import (
	"context"
	"errors"
	"fmt"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

func (s *Store) AcquireHostActivityLease(ctx context.Context, lease app.HostActivityLease) error {
	if err := lease.ValidateForAcquireOrRenew(); err != nil {
		return err
	}
	rows, err := s.q.AcquireHostActivityLease(ctx, sqlcgen.AcquireHostActivityLeaseParams{
		HostID: lease.HostID, LeaseID: lease.LeaseID, RuntimeGeneration: lease.RuntimeGeneration,
		ControlVersion: lease.ControlVersion, Kind: lease.Kind, TtlSeconds: lease.TTL.Seconds(),
	})
	if err != nil {
		return fmt.Errorf("acquire host activity lease: %w", err)
	}
	if rows != 1 {
		return hostActivityLeaseConflict()
	}
	return nil
}

func (s *Store) RenewHostActivityLease(ctx context.Context, lease app.HostActivityLease) error {
	if err := lease.ValidateForAcquireOrRenew(); err != nil {
		return err
	}
	rows, err := s.q.RenewHostActivityLease(ctx, sqlcgen.RenewHostActivityLeaseParams{
		HostID: lease.HostID, LeaseID: lease.LeaseID, RuntimeGeneration: lease.RuntimeGeneration,
		ControlVersion: lease.ControlVersion, Kind: lease.Kind, TtlSeconds: lease.TTL.Seconds(),
	})
	if err != nil {
		return fmt.Errorf("renew host activity lease: %w", err)
	}
	if rows != 1 {
		return hostActivityLeaseConflict()
	}
	return nil
}

func (s *Store) ReleaseHostActivityLease(ctx context.Context, lease app.HostActivityLease) error {
	if err := app.ValidateHostActivityLease(lease); err != nil {
		return err
	}
	if err := s.q.ReleaseHostActivityLease(ctx, sqlcgen.ReleaseHostActivityLeaseParams{
		HostID: lease.HostID, LeaseID: lease.LeaseID, RuntimeGeneration: lease.RuntimeGeneration,
		ControlVersion: lease.ControlVersion, Kind: lease.Kind,
	}); err != nil {
		return fmt.Errorf("release host activity lease: %w", err)
	}
	return nil
}

func (s *Store) HasActiveHostActivityLease(ctx context.Context, hostID string, runtimeGeneration int64) (bool, error) {
	if !domain.ValidHostID(hostID) || runtimeGeneration <= 0 {
		return false, domain.InvalidArgument("host_id", "host activity lease query is invalid")
	}
	active, err := s.q.HasActiveHostActivityLease(ctx, sqlcgen.HasActiveHostActivityLeaseParams{
		HostID: hostID, RuntimeGeneration: runtimeGeneration,
	})
	if err != nil {
		return false, fmt.Errorf("check active host activity lease: %w", err)
	}
	return active, nil
}

func (s *Store) HasOutstandingControlTransitionLeases(ctx context.Context, hostID string, expectedControlVersion int64) (bool, error) {
	if !domain.ValidHostID(hostID) || expectedControlVersion < 0 {
		return false, domain.InvalidArgument("host_id", "control transition lease query is invalid")
	}
	pending, err := s.q.HasOutstandingControlTransitionLeases(ctx, sqlcgen.HasOutstandingControlTransitionLeasesParams{
		HostID: hostID, ExpectedControlVersion: expectedControlVersion,
	})
	if err != nil {
		return false, fmt.Errorf("check outstanding control transition leases: %w", err)
	}
	return pending, nil
}

func (s *Store) WithHostInputPermit(ctx context.Context, ref host.Ref, forward func() error) error {
	if !domain.ValidHostID(ref.ID) || ref.RuntimeGeneration <= 0 || ref.ControlVersion < 0 || forward == nil {
		return domain.InvalidArgument("host_id", "user input permit is invalid")
	}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var lockedHostID string
		err := tx.QueryRow(ctx, `
SELECT id
FROM hosts
WHERE id=$1 AND state='running' AND deleted_at IS NULL
  AND runtime_generation=$2 AND control_version=$3 AND control_state='user'
FOR SHARE`, ref.ID, ref.RuntimeGeneration, ref.ControlVersion).Scan(&lockedHostID)
		if errors.Is(err, pgx.ErrNoRows) {
			return hostActivityLeaseConflict()
		}
		if err != nil {
			return fmt.Errorf("lock Host for user input permit: %w", err)
		}
		if err := forward(); err != nil {
			return fmt.Errorf("forward user input: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

func hostActivityLeaseConflict() error {
	return domain.NewError(domain.CodeConflict, "host activity lease fence is stale, expired, or already in use")
}

var _ app.HostActivityLeaseStore = (*Store)(nil)
var _ app.HostActivityLeaseDrainer = (*Store)(nil)
var _ app.HostInputPermitStore = (*Store)(nil)
