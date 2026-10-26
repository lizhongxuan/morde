package postgres

import (
	"context"
	"fmt"

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
	row, err := s.q.UpsertHostMember(ctx, sqlcgen.UpsertHostMemberParams{
		HostID: value.HostID, OwnerID: value.OwnerID, UserID: value.UserID, Permissions: permissions,
	})
	if err != nil {
		return host.Member{}, fmt.Errorf("upsert host member: %w", err)
	}
	return hostMemberDomain(row), nil
}

func (s *Store) ListHostMembers(ctx context.Context, hostID, ownerID string) ([]host.Member, error) {
	rows, err := s.q.ListHostMembers(ctx, sqlcgen.ListHostMembersParams{HostID: hostID, OwnerID: ownerID})
	if err != nil {
		return nil, fmt.Errorf("list host members: %w", err)
	}
	values := make([]host.Member, 0, len(rows))
	for _, row := range rows {
		values = append(values, hostMemberDomain(row))
	}
	return values, nil
}

func (s *Store) RevokeHostMember(ctx context.Context, hostID, ownerID, userID string) error {
	rows, err := s.q.RevokeHostMember(ctx, sqlcgen.RevokeHostMemberParams{HostID: hostID, OwnerID: ownerID, UserID: userID})
	if err != nil {
		return fmt.Errorf("revoke host member: %w", err)
	}
	if rows == 0 {
		return domain.NewError(domain.CodeNotFound, "host member not found")
	}
	return nil
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

func hostMemberDomain(row sqlcgen.HostMember) host.Member {
	return host.Member{
		HostID: row.HostID, OwnerID: row.OwnerID, UserID: row.UserID, Permissions: hostPermissions(row.Permissions),
		CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time, RevokedAt: timePointer(row.RevokedAt),
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
