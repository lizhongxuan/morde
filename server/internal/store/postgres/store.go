package postgres

import (
	"context"
	"errors"
	"fmt"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: sqlcgen.New(pool)}
}

func (s *Store) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return withTx(ctx, s.pool.Begin, fn)
}

func withTx(ctx context.Context, begin func(context.Context) (pgx.Tx, error), fn func(pgx.Tx) error) error {
	tx, err := begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func (s *Store) GetHost(ctx context.Context, id string) (host.Host, error) {
	row, err := s.q.GetHost(ctx, id)
	if err != nil {
		return host.Host{}, storeError("get host", "host", err)
	}
	return hostDomainWithBindings(ctx, s.q, row)
}

func hostDomainWithBindings(ctx context.Context, queries *sqlcgen.Queries, row sqlcgen.Host) (host.Host, error) {
	value := hostDomain(row)
	bindings, err := queries.ListWorkspaceBindingsByHost(ctx, row.ID)
	if err != nil {
		return host.Host{}, fmt.Errorf("list host workspace bindings: %w", err)
	}
	value.Workspaces = mapRows(bindings, workspaceBindingDomainFromList)
	return value, nil
}

func (s *Store) ListHosts(ctx context.Context) ([]host.Host, error) {
	rows, err := s.q.ListHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	values := mapRows(rows, hostDomain)
	if err := s.populateHostWorkspaceBindings(ctx, values); err != nil {
		return nil, err
	}
	return values, nil
}

func (s *Store) ListHostsByOwner(ctx context.Context, ownerID string) ([]host.Host, error) {
	rows, err := s.q.ListHostsByOwner(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	values := mapRows(rows, hostDomain)
	if err := s.populateHostWorkspaceBindings(ctx, values); err != nil {
		return nil, err
	}
	return values, nil
}

func (s *Store) populateHostWorkspaceBindings(ctx context.Context, hosts []host.Host) error {
	if len(hosts) == 0 {
		return nil
	}
	ids := make([]string, 0, len(hosts))
	byID := make(map[string]int, len(hosts))
	for i := range hosts {
		ids = append(ids, hosts[i].ID)
		byID[hosts[i].ID] = i
	}
	rows, err := s.q.ListWorkspaceBindingsByHostIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("list host workspace bindings: %w", err)
	}
	for _, row := range rows {
		if i, ok := byID[row.HostID]; ok {
			hosts[i].Workspaces = append(hosts[i].Workspaces, workspaceBindingDomainFromHostIDs(row))
		}
	}
	return nil
}

func (s *Store) GetWorkspace(ctx context.Context, id string) (workspace.Workspace, error) {
	row, err := s.q.GetWorkspace(ctx, id)
	if err != nil {
		return workspace.Workspace{}, storeError("get workspace", "workspace", err)
	}
	return workspaceDomain(row), nil
}

func (s *Store) ListWorkspacesByOwner(ctx context.Context, ownerID string) ([]workspace.Workspace, error) {
	rows, err := s.q.ListWorkspacesByOwner(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	return mapRows(rows, workspaceDomain), nil
}

func (s *Store) CreateWorkspace(ctx context.Context, value workspace.Workspace, idempotency app.IdempotencyRecord) (workspace.Workspace, bool, error) {
	if !domain.ValidWorkspaceID(value.ID) || !domain.ValidUserID(value.OwnerID) ||
		value.State != workspace.StateReady || value.Revision != 0 || value.UsedBytes != 0 ||
		value.QuotaBytes <= 0 || idempotency.OwnerID != value.OwnerID || idempotency.Operation != "workspace.create" {
		return workspace.Workspace{}, false, domain.InvalidArgument("workspace", "workspace creation fields are invalid")
	}
	var created workspace.Workspace
	replayed := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		stored, replay, err := reserveIdempotency(ctx, tx, idempotency, value.ID, "")
		if err != nil {
			return err
		}
		if replay {
			row, err := sqlcgen.New(tx).GetWorkspace(ctx, stored.ResourceID)
			if err != nil {
				return storeError("replay workspace creation", "workspace", err)
			}
			if row.OwnerID != value.OwnerID {
				return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key is bound to another workspace")
			}
			created = workspaceDomain(row)
			replayed = true
			return nil
		}
		row, err := sqlcgen.New(tx).CreateWorkspace(ctx, sqlcgen.CreateWorkspaceParams{
			ID: value.ID, OwnerID: value.OwnerID, Name: value.Name, State: string(value.State),
			Revision: value.Revision, UsedBytes: value.UsedBytes, QuotaBytes: value.QuotaBytes, LastError: value.LastError,
		})
		if err != nil {
			return fmt.Errorf("create workspace: %w", err)
		}
		if err := sqlcgen.New(tx).CreateWorkspaceRevision(ctx, sqlcgen.CreateWorkspaceRevisionParams{
			WorkspaceID: value.ID, Revision: 0, FileCount: 0, TotalBytes: 0,
		}); err != nil {
			return fmt.Errorf("create initial workspace revision: %w", err)
		}
		created = workspaceDomain(row)
		return nil
	})
	if err != nil {
		return workspace.Workspace{}, false, err
	}
	return created, replayed, nil
}

func (s *Store) GetUserByID(ctx context.Context, id string) (identity.User, error) {
	row, err := s.q.GetUserByID(ctx, id)
	if err != nil {
		return identity.User{}, storeError("get user", "user", err)
	}
	return userDomain(row), nil
}

func (s *Store) GetUserByName(ctx context.Context, name string) (identity.User, error) {
	row, err := s.q.GetUserByName(ctx, name)
	if err != nil {
		return identity.User{}, storeError("get user", "user", err)
	}
	return userDomain(row), nil
}

func (s *Store) ListUsers(ctx context.Context) ([]identity.User, error) {
	rows, err := s.q.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return mapRows(rows, userDomain), nil
}

func (s *Store) CreateUser(ctx context.Context, value identity.User) (identity.User, error) {
	row, err := s.q.CreateUser(ctx, sqlcgen.CreateUserParams{
		ID: value.ID, Name: value.Name, PasswordHash: value.PasswordHash, Role: string(value.Role),
		QuotaHosts: int32(value.QuotaHosts), QuotaCpuMilli: int32(value.QuotaCPUMilli),
		QuotaMemMib: int32(value.QuotaMemMiB), Disabled: value.Disabled,
	})
	if err != nil {
		return identity.User{}, fmt.Errorf("create user: %w", err)
	}
	return userDomain(row), nil
}

func (s *Store) UpdateUser(ctx context.Context, actorID string, value identity.User) (identity.User, error) {
	var updated identity.User
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('morde:admin-user-update', 0))`); err != nil {
			return fmt.Errorf("lock admin user updates: %w", err)
		}
		var currentRole string
		var currentDisabled bool
		if err := tx.QueryRow(ctx, `SELECT role, disabled FROM users WHERE id=$1 FOR UPDATE`, value.ID).Scan(&currentRole, &currentDisabled); err != nil {
			return storeError("update user", "user", err)
		}
		wasActiveAdmin := currentRole == string(identity.RoleAdmin) && !currentDisabled
		willBeActiveAdmin := value.Role == identity.RoleAdmin && !value.Disabled
		if actorID == value.ID && !willBeActiveAdmin {
			return domain.NewError(domain.CodeForbidden, "administrators cannot disable or demote themselves")
		}
		if wasActiveAdmin && !willBeActiveAdmin {
			var activeAdmins int64
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin' AND disabled=false`).Scan(&activeAdmins); err != nil {
				return fmt.Errorf("count active administrators: %w", err)
			}
			if activeAdmins <= 1 {
				return domain.NewError(domain.CodeInvalidState, "cannot disable or demote the last active administrator")
			}
		}
		row, err := sqlcgen.New(tx).UpdateUser(ctx, sqlcgen.UpdateUserParams{
			ID: value.ID, Name: value.Name, PasswordHash: value.PasswordHash, Role: string(value.Role),
			QuotaHosts: int32(value.QuotaHosts), QuotaCpuMilli: int32(value.QuotaCPUMilli),
			QuotaMemMib: int32(value.QuotaMemMiB), Disabled: value.Disabled,
		})
		if err != nil {
			return storeError("update user", "user", err)
		}
		updated = userDomain(row)
		return nil
	})
	if err != nil {
		return identity.User{}, err
	}
	return updated, nil
}

func (s *Store) GetTokenByID(ctx context.Context, id string) (identity.Token, error) {
	row, err := s.q.GetToken(ctx, id)
	if err != nil {
		return identity.Token{}, storeError("get token", "token", err)
	}
	return tokenDomain(row), nil
}

func (s *Store) ListTokensByOwner(ctx context.Context, ownerID string) ([]identity.Token, error) {
	rows, err := s.q.ListTokensByOwner(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	return mapRows(rows, tokenDomain), nil
}

func (s *Store) CreateToken(ctx context.Context, value identity.Token) (identity.Token, error) {
	return createToken(ctx, s.q, value)
}

func createToken(ctx context.Context, q *sqlcgen.Queries, value identity.Token) (identity.Token, error) {
	if value.ActorUserID != "" && (value.Kind != identity.KindTicket || !domain.ValidUserID(value.ActorUserID) || value.ActorUserID == value.OwnerID) {
		return identity.Token{}, domain.InvalidArgument("actor_user_id", "ticket actor is invalid")
	}
	if value.PreviewPort != 0 && (value.Kind != identity.KindTicket || !domain.ValidPreviewTargetPort(value.PreviewPort)) {
		return identity.Token{}, domain.InvalidArgument("preview_port", "preview port is invalid")
	}
	scopes := make([]string, 0, len(value.Scopes))
	for _, scope := range value.Scopes {
		scopes = append(scopes, string(scope))
	}
	row, err := q.CreateToken(ctx, sqlcgen.CreateTokenParams{
		ID: value.ID, OwnerID: value.OwnerID, Kind: string(value.Kind), HostID: nullableText(value.HostID),
		PreviewPort: pgtype.Int4{Int32: int32(value.PreviewPort), Valid: value.PreviewPort > 0},
		HostIds:     append([]string{}, value.HostIDs...), WorkspaceIds: append([]string{}, value.WorkspaceIDs...), Scopes: scopes,
		Name: value.Name, SecretHash: value.SecretHash, ExpiresAt: timestamp(value.ExpiresAt),
		UsedAt: nullableTimestamp(value.UsedAt), LastUsedAt: nullableTimestamp(value.LastUsedAt),
		RevokedAt: nullableTimestamp(value.RevokedAt), ActorUserID: nullableText(value.ActorUserID),
		ClientInstallationID: nullableText(value.ClientInstallationID),
	})
	if err != nil {
		return identity.Token{}, fmt.Errorf("create token: %w", err)
	}
	return tokenDomain(row), nil
}

func (s *Store) RevokeToken(ctx context.Context, id, ownerID string) error {
	rowsAffected, err := s.q.RevokeToken(ctx, sqlcgen.RevokeTokenParams{ID: id, OwnerID: ownerID})
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	if rowsAffected == 0 {
		return domain.NewError(domain.CodeNotFound, "token not found")
	}
	return nil
}

func (s *Store) WriteAudit(ctx context.Context, entry audit.Entry) error {
	params, err := auditParams(entry)
	if err != nil {
		return err
	}
	_, err = s.q.CreateAuditLog(ctx, params)
	if err != nil {
		return fmt.Errorf("write audit log: %w", err)
	}
	return nil
}

func (s *Store) ListAuditLogs(ctx context.Context, filter app.AuditFilter) ([]app.AuditLog, error) {
	rows, err := s.q.ListAuditLogs(ctx, sqlcgen.ListAuditLogsParams{
		Actor: filter.Actor, Action: filter.Action, Target: filter.Target,
		Since: nullableTimestamp(filter.Since), AuditLimit: filter.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list audit logs: %w", err)
	}
	return mapRowsWithError(rows, auditDomain)
}

func storeError(operation, resource string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, resource+" not found")
	}
	return fmt.Errorf("%s: %w", operation, err)
}

var (
	_ app.HostStore            = (*Store)(nil)
	_ app.WorkspaceStore       = (*Store)(nil)
	_ app.UserStore            = (*Store)(nil)
	_ app.TokenStore           = (*Store)(nil)
	_ app.AuditStore           = (*Store)(nil)
	_ app.ModelConnectionStore = (*Store)(nil)
)
