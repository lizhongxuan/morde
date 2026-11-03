package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
)

func (s *Store) GetHostControlLease(ctx context.Context, hostID string) (app.HostControlLease, error) {
	if !domain.ValidHostID(hostID) {
		return app.HostControlLease{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	var value app.HostControlLease
	var actorID pgtype.Text
	var expiresAt pgtype.Timestamptz
	err := s.pool.QueryRow(ctx, `
SELECT id, control_version,
       CASE WHEN lease.active THEN control_actor_id ELSE NULL END, control_lease_expires_at, lease.active
FROM hosts
CROSS JOIN LATERAL (
  SELECT state='running' AND control_state='user' AND control_actor_id IS NOT NULL
    AND control_lease_expires_at > clock_timestamp()
    AND EXISTS (SELECT 1 FROM users WHERE id=control_actor_id AND status='approved')
    AND (control_actor_id=owner_id OR EXISTS (
      SELECT 1 FROM host_members m WHERE m.host_id=hosts.id AND m.owner_id=hosts.owner_id
        AND m.user_id=control_actor_id AND m.revoked_at IS NULL
        AND m.permissions && ARRAY['desktop:control','terminal:control','mcp:control']::text[]
    )) AS active
) lease
WHERE id=$1 AND deleted_at IS NULL`, hostID).Scan(
		&value.HostID, &value.ControlVersion, &actorID, &expiresAt, &value.Active,
	)
	if err != nil {
		return app.HostControlLease{}, storeError("get host control lease", "host", err)
	}
	if actorID.Valid {
		value.ActorUserID = actorID.String
	}
	if expiresAt.Valid {
		expires := expiresAt.Time.UTC()
		value.ExpiresAt = &expires
	}
	return value, nil
}

func (s *Store) RenewHostControlLease(ctx context.Context, hostID, actorID string, controlVersion int64) (app.HostControlLease, error) {
	if !domain.ValidHostID(hostID) || !domain.ValidUserID(actorID) || controlVersion < 0 {
		return app.HostControlLease{}, domain.InvalidArgument("control_lease", "Host control lease identity is invalid")
	}
	var result app.HostControlLease
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID, currentActor string
		var runtimeVersion int64
		err := tx.QueryRow(ctx, `SELECT owner_id, COALESCE(control_actor_id,''), control_version
FROM hosts WHERE id=$1 AND state='running' AND control_state='user' AND deleted_at IS NULL
  AND control_version=$2 AND control_actor_id=$3
  AND control_lease_expires_at > clock_timestamp()
FOR UPDATE`, hostID, controlVersion, actorID).Scan(&ownerID, &currentActor, &runtimeVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return controlLeaseConflict()
		}
		if err != nil {
			return fmt.Errorf("lock Host control lease for renewal: %w", err)
		}
		allowed, err := hasControlPermission(ctx, tx, hostID, ownerID, actorID, "")
		if err != nil {
			return fmt.Errorf("authorize Host control lease renewal: %w", err)
		}
		if !allowed {
			return controlLeaseConflict()
		}
		var expires time.Time
		err = tx.QueryRow(ctx, `UPDATE hosts SET control_lease_expires_at=clock_timestamp()+($4 * interval '1 second'), updated_at=clock_timestamp()
WHERE id=$1 AND control_state='user' AND control_version=$2 AND control_actor_id=$3
  AND control_lease_expires_at > clock_timestamp()
RETURNING control_lease_expires_at`, hostID, controlVersion, currentActor, app.HostControlLeaseTTL.Seconds()).Scan(&expires)
		if err != nil {
			return fmt.Errorf("renew Host control lease: %w", err)
		}
		result = app.HostControlLease{HostID: hostID, ActorUserID: actorID, ControlVersion: runtimeVersion, ExpiresAt: &expires, Active: true}
		return nil
	})
	if err != nil {
		return app.HostControlLease{}, err
	}
	return result, nil
}

func (s *Store) CheckHostControlLease(ctx context.Context, hostID, actorID string, runtimeGeneration, controlVersion int64) error {
	if !domain.ValidHostID(hostID) || !domain.ValidUserID(actorID) || runtimeGeneration <= 0 || controlVersion < 0 {
		return domain.InvalidArgument("control_lease", "Host control lease fence is invalid")
	}
	var ownerID string
	err := s.pool.QueryRow(ctx, `SELECT owner_id FROM hosts WHERE id=$1 AND state='running' AND control_state='user' AND deleted_at IS NULL
  AND runtime_generation=$3 AND control_version=$4 AND control_actor_id=$2 AND control_lease_expires_at > clock_timestamp()`,
		hostID, actorID, runtimeGeneration, controlVersion).Scan(&ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return controlLeaseConflict()
	}
	if err != nil {
		return fmt.Errorf("check Host control lease: %w", err)
	}
	allowed, err := hasControlPermission(ctx, s.pool, hostID, ownerID, actorID, "")
	if err != nil {
		return fmt.Errorf("authorize Host control lease: %w", err)
	}
	if !allowed {
		return controlLeaseConflict()
	}
	return nil
}

func (s *Store) WithHostControlPermit(ctx context.Context, ref host.Ref, actorID string, permission host.Permission, forward func() error) error {
	if !domain.ValidHostID(ref.ID) || !domain.ValidUserID(actorID) || ref.RuntimeGeneration <= 0 || ref.ControlVersion < 0 || forward == nil ||
		(permission != host.PermissionDesktopControl && permission != host.PermissionTerminalControl && permission != host.PermissionMCPControl) {
		return domain.InvalidArgument("control_lease", "Host control permit is invalid")
	}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID string
		err := tx.QueryRow(ctx, `SELECT owner_id FROM hosts WHERE id=$1 AND state='running' AND control_state='user' AND deleted_at IS NULL
  AND runtime_generation=$2 AND control_version=$3 AND control_actor_id=$4 AND control_lease_expires_at > clock_timestamp()
FOR SHARE`, ref.ID, ref.RuntimeGeneration, ref.ControlVersion, actorID).Scan(&ownerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return controlLeaseConflict()
		}
		if err != nil {
			return fmt.Errorf("lock Host control permit: %w", err)
		}
		allowed, err := hasControlPermission(ctx, tx, ref.ID, ownerID, actorID, string(permission))
		if err != nil {
			return fmt.Errorf("authorize Host control permit: %w", err)
		}
		if !allowed {
			return controlLeaseConflict()
		}
		if err := forward(); err != nil {
			return fmt.Errorf("forward Host control input: %w", err)
		}
		return nil
	})
	return err
}

type hostControlPermissionQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func hasControlPermission(ctx context.Context, queryer hostControlPermissionQuerier, hostID, ownerID, actorID, permission string) (bool, error) {
	var allowed bool
	err := queryer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$3 AND status='approved') AND
  ($2=$3 OR EXISTS (
    SELECT 1 FROM host_members m WHERE m.host_id=$1 AND m.owner_id=$2 AND m.user_id=$3 AND m.revoked_at IS NULL
      AND CASE WHEN $4='' THEN m.permissions && ARRAY['desktop:control','terminal:control','mcp:control']::text[]
               ELSE $4=ANY(m.permissions) END
  ))`, hostID, ownerID, actorID, permission).Scan(&allowed)
	return allowed, err
}

func controlLeaseConflict() error {
	return domain.NewError(domain.CodeConflict, "Host control lease is absent, expired, or no longer authorized")
}

var _ app.HostControlLeaseStore = (*Store)(nil)
