package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) UpsertHostMember(ctx context.Context, value host.Member) (host.Member, error) {
	if !domain.ValidHostID(value.HostID) || !domain.ValidUserID(value.OwnerID) || !domain.ValidUserID(value.UserID) ||
		value.OwnerID == value.UserID || len(value.Permissions) == 0 {
		return host.Member{}, domain.InvalidArgument("member", "host member fields are invalid")
	}
	permissions := make([]string, 0, len(value.Permissions))
	for _, permission := range value.Permissions {
		if !permission.Valid() {
			return host.Member{}, domain.InvalidArgument("permissions", "host member permission is invalid")
		}
		permissions = append(permissions, string(permission))
	}
	var result host.Member
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var controlActor string
		var controlVersion int64
		err := tx.QueryRow(ctx, `SELECT COALESCE(control_actor_id,''), control_version FROM hosts WHERE id=$1 AND owner_id=$2 AND deleted_at IS NULL FOR UPDATE`,
			value.HostID, value.OwnerID).Scan(&controlActor, &controlVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		if err != nil {
			return fmt.Errorf("lock Host before member permission update: %w", err)
		}
		row, err := sqlcgen.New(tx).UpsertHostMember(ctx, sqlcgen.UpsertHostMemberParams{
			HostID: value.HostID, OwnerID: value.OwnerID, UserID: value.UserID, Permissions: permissions,
		})
		if err != nil {
			return fmt.Errorf("upsert host member: %w", err)
		}
		result = hostMemberDomain(row.HostID, row.OwnerID, row.UserID, row.Permissions,
			row.CreatedAt.Time, row.UpdatedAt.Time, timePointer(row.RevokedAt))
		if controlActor == value.UserID {
			allowed, err := hasControlPermission(ctx, tx, value.HostID, value.OwnerID, value.UserID, "")
			if err != nil {
				return fmt.Errorf("revalidate Host control actor after permission update: %w", err)
			}
			if !allowed {
				if controlVersion == int64(^uint64(0)>>1) {
					return domain.NewError(domain.CodeInvalidState, "Host control version cannot be advanced")
				}
				if _, err := tx.Exec(ctx, `UPDATE hosts SET control_actor_id=NULL, control_lease_expires_at=NULL,
control_version=control_version+1, updated_at=clock_timestamp() WHERE id=$1 AND control_actor_id=$2 AND control_version=$3`,
					value.HostID, value.UserID, controlVersion); err != nil {
					return fmt.Errorf("clear Host control lease after member permission update: %w", err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return host.Member{}, err
	}
	return result, nil
}

func (s *Store) ListHostMembers(ctx context.Context, hostID, ownerID string) ([]host.Member, error) {
	rows, err := s.q.ListHostMembers(ctx, sqlcgen.ListHostMembersParams{HostID: hostID, OwnerID: ownerID})
	if err != nil {
		return nil, fmt.Errorf("list host members: %w", err)
	}
	values := make([]host.Member, 0, len(rows))
	for _, row := range rows {
		values = append(values, hostMemberDomain(row.HostID, row.OwnerID, row.UserID, row.Permissions,
			row.CreatedAt.Time, row.UpdatedAt.Time, timePointer(row.RevokedAt)))
	}
	return values, nil
}

func (s *Store) RevokeHostMember(ctx context.Context, hostID, ownerID, userID string) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockOwnerExecutionControl(ctx, tx, ownerID, true); err != nil {
			return err
		}
		var hostOwnerID, controlActor string
		var controlVersion int64
		err := tx.QueryRow(ctx, `SELECT owner_id, COALESCE(control_actor_id,''), control_version FROM hosts WHERE id=$1 AND owner_id=$2 FOR UPDATE`, hostID, ownerID).Scan(&hostOwnerID, &controlActor, &controlVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "host member not found")
		}
		if err != nil {
			return fmt.Errorf("lock host for member revocation: %w", err)
		}
		var transitionActive bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM host_control_transitions WHERE host_id=$1 AND phase <> 'completed')`, hostID).Scan(&transitionActive); err != nil {
			return fmt.Errorf("check active Host control transition before member revocation: %w", err)
		}
		if transitionActive {
			return domain.NewError(domain.CodeConflict, "Host control transition must finish before member revocation")
		}
		if err := requestMCPRuntimeCancellationsForHostMember(ctx, tx, hostOwnerID, userID, hostID); err != nil {
			return err
		}
		rows, err := sqlcgen.New(tx).RevokeHostMember(ctx, sqlcgen.RevokeHostMemberParams{HostID: hostID, OwnerID: ownerID, UserID: userID})
		if err != nil {
			return fmt.Errorf("revoke host member: %w", err)
		}
		if rows == 0 {
			return domain.NewError(domain.CodeNotFound, "host member not found")
		}
		if controlActor == userID {
			if controlVersion == int64(^uint64(0)>>1) {
				return domain.NewError(domain.CodeInvalidState, "Host control version cannot be advanced")
			}
			if _, err := tx.Exec(ctx, `UPDATE hosts SET control_actor_id=NULL, control_lease_expires_at=NULL,
control_version=control_version+1, updated_at=clock_timestamp() WHERE id=$1 AND control_actor_id=$2 AND control_version=$3`,
				hostID, userID, controlVersion); err != nil {
				return fmt.Errorf("revoke Host control lease: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) ListSharedHosts(ctx context.Context, userID string) ([]host.SharedHost, error) {
	rows, err := s.q.ListSharedHosts(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list shared hosts: %w", err)
	}
	values := make([]host.SharedHost, 0, len(rows))
	for _, row := range rows {
		values = append(values, host.SharedHost{
			HostID: row.HostID, Name: row.Name, State: host.State(row.State), Permissions: hostPermissions(row.Permissions),
		})
	}
	return values, nil
}

func (s *Store) GetHostActorAccess(ctx context.Context, hostID, actorID string) (host.ActorAccess, error) {
	row, err := s.q.GetHostActorAccess(ctx, sqlcgen.GetHostActorAccessParams{ID: hostID, ID_2: actorID})
	if err != nil {
		return host.ActorAccess{}, storeError("get host actor access", "host", err)
	}
	return host.ActorAccess{OwnerID: row.OwnerID, Disabled: row.Disabled, Permissions: hostPermissions(row.Permissions)}, nil
}

func hostMemberDomain(hostID, ownerID, userID string, permissions []string, createdAt, updatedAt time.Time, revokedAt *time.Time) host.Member {
	return host.Member{
		HostID: hostID, OwnerID: ownerID, UserID: userID, Permissions: hostPermissions(permissions),
		CreatedAt: createdAt, UpdatedAt: updatedAt, RevokedAt: revokedAt,
	}
}

func hostPermissions(values []string) []host.Permission {
	permissions := make([]host.Permission, 0, len(values))
	for _, value := range values {
		permissions = append(permissions, host.Permission(value))
	}
	return permissions
}

var _ app.HostSharingStore = (*Store)(nil)
