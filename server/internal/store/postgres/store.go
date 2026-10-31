package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_current_materializations
(workspace_id, desired_revision, applied_revision) VALUES ($1, 0, -1)`, value.ID); err != nil {
			return fmt.Errorf("create initial Workspace current materialization: %w", err)
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

func (s *Store) ListUsersByStatus(ctx context.Context, status *identity.AccountStatus) ([]identity.User, error) {
	if status == nil {
		return s.ListUsers(ctx)
	}
	if !status.Valid() {
		return nil, domain.InvalidArgument("status", "invalid account status")
	}
	rows, err := s.q.ListUsersByStatus(ctx, string(*status))
	if err != nil {
		return nil, fmt.Errorf("list users by status: %w", err)
	}
	return mapRows(rows, userDomain), nil
}

func (s *Store) CreateUser(ctx context.Context, value identity.User) (identity.User, error) {
	if !domain.ValidUserID(value.ID) || strings.TrimSpace(value.Name) == "" || value.Name != strings.TrimSpace(value.Name) ||
		strings.EqualFold(value.Name, "admin") || value.PasswordHash == "" || value.Role != identity.RoleUser ||
		value.Status != identity.StatusPending || value.MustChangePassword || value.QuotaHosts < 0 ||
		value.QuotaCPUMilli < 0 || value.QuotaMemMiB < 0 || value.DefaultHostCPUMilli <= 0 || value.DefaultHostMemMiB <= 0 {
		return identity.User{}, domain.InvalidArgument("user", "new users must be pending standard accounts with valid fields")
	}
	row, err := s.q.CreateUser(ctx, sqlcgen.CreateUserParams{
		ID: value.ID, Name: value.Name, PasswordHash: value.PasswordHash, Role: string(value.Role),
		Status: string(value.Status), MustChangePassword: value.MustChangePassword,
		QuotaHosts: int32(value.QuotaHosts), QuotaCpuMilli: int32(value.QuotaCPUMilli),
		QuotaMemMib: int32(value.QuotaMemMiB), DefaultHostCpuMilli: int32(value.DefaultHostCPUMilli), DefaultHostMemMib: int32(value.DefaultHostMemMiB),
	})
	if err != nil {
		return identity.User{}, fmt.Errorf("create user: %w", err)
	}
	return userDomain(row), nil
}

func (s *Store) UpdateUser(ctx context.Context, actorID string, expectedStatus identity.AccountStatus, value identity.User) (identity.User, error) {
	return s.updateUser(ctx, actorID, expectedStatus, value, nil, nil)
}

func (s *Store) UpdateUserWithAudit(ctx context.Context, actorID string, expectedStatus identity.AccountStatus, value identity.User, entry audit.Entry, disableRelay func(context.Context) error) (identity.User, error) {
	params, err := auditParams(entry)
	if err != nil {
		return identity.User{}, err
	}
	return s.updateUser(ctx, actorID, expectedStatus, value, &params, disableRelay)
}

func (s *Store) updateUser(ctx context.Context, actorID string, expectedStatus identity.AccountStatus, value identity.User, auditParams *sqlcgen.CreateAuditLogParams, disableRelay func(context.Context) error) (identity.User, error) {
	if !domain.ValidUserID(value.ID) || !expectedStatus.Valid() || !value.Status.Valid() ||
		(value.Role != identity.RoleUser && value.Role != identity.RoleAdmin && value.Role != identity.RoleSuperAdmin) ||
		value.QuotaHosts < 0 || value.QuotaCPUMilli < 0 || value.QuotaMemMiB < 0 || value.DefaultHostCPUMilli <= 0 || value.DefaultHostMemMiB <= 0 {
		return identity.User{}, domain.InvalidArgument("user", "user fields or expected status are invalid")
	}
	var updated identity.User
	err := s.withWorkspaceSyncOwnerTx(ctx, value.ID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('morde:admin-user-update', 0))`); err != nil {
			return fmt.Errorf("lock admin user updates: %w", err)
		}
		if _, err := lockOwnerExecutionControl(ctx, tx, value.ID, true); err != nil {
			return err
		}
		var currentName, currentPasswordHash, currentRole, currentStatus string
		var currentMustChangePassword bool
		var currentQuotaHosts, currentQuotaCPUMilli, currentQuotaMemMiB, currentDefaultCPU, currentDefaultMem int
		if err := tx.QueryRow(ctx, `SELECT name, password_hash, role, status, must_change_password,
quota_hosts, quota_cpu_milli, quota_mem_mib, default_host_cpu_milli, default_host_mem_mib FROM users WHERE id=$1 FOR UPDATE`, value.ID).Scan(
			&currentName, &currentPasswordHash, &currentRole, &currentStatus, &currentMustChangePassword,
			&currentQuotaHosts, &currentQuotaCPUMilli, &currentQuotaMemMiB, &currentDefaultCPU, &currentDefaultMem); err != nil {
			return storeError("update user", "user", err)
		}
		current := identity.User{
			Name: currentName, PasswordHash: currentPasswordHash, Role: identity.Role(currentRole),
			Status: identity.AccountStatus(currentStatus), MustChangePassword: currentMustChangePassword,
			QuotaHosts: currentQuotaHosts, QuotaCPUMilli: currentQuotaCPUMilli, QuotaMemMiB: currentQuotaMemMiB,
			DefaultHostCPUMilli: currentDefaultCPU, DefaultHostMemMiB: currentDefaultMem,
		}
		if current.Status != expectedStatus {
			return domain.NewError(domain.CodeConflict, "account status changed; refresh the user list")
		}
		if value.Status != current.Status && !current.Status.CanTransitionTo(value.Status) {
			return domain.NewError(domain.CodeConflict, "account status transition is not allowed")
		}
		if value.Role != current.Role && (current.Status != identity.StatusApproved || value.Status != identity.StatusApproved) {
			return domain.NewError(domain.CodeConflict, "only approved accounts may change role")
		}
		if value.Role == identity.RoleSuperAdmin && current.Role != identity.RoleSuperAdmin {
			return domain.NewError(domain.CodeForbidden, "the super administrator role is reserved")
		}
		if value.MustChangePassword != current.MustChangePassword && value.Role != identity.RoleSuperAdmin {
			return domain.NewError(domain.CodeForbidden, "only the super administrator may change this setting")
		}
		if actorID == value.ID && current.Role.IsAdmin() &&
			(current.Role != value.Role || current.Status != value.Status) {
			return domain.NewError(domain.CodeForbidden, "administrators cannot change their own role or status")
		}
		if current.Name == "admin" || current.Role == identity.RoleSuperAdmin {
			if current.Name != "admin" || current.Role != identity.RoleSuperAdmin || actorID != value.ID ||
				value.Name != current.Name || value.Role != current.Role || value.Status != current.Status ||
				value.QuotaHosts != current.QuotaHosts || value.QuotaCPUMilli != current.QuotaCPUMilli ||
				value.QuotaMemMiB != current.QuotaMemMiB || value.DefaultHostCPUMilli != current.DefaultHostCPUMilli || value.DefaultHostMemMiB != current.DefaultHostMemMiB || value.MustChangePassword {
				return domain.NewError(domain.CodeForbidden, "the super administrator account is protected")
			}
		}
		if value.PasswordHash != current.PasswordHash && actorID != value.ID {
			return domain.NewError(domain.CodeForbidden, "users must change their own password")
		}
		identityChanged := current.Name != value.Name || current.PasswordHash != value.PasswordHash || current.Role != value.Role ||
			current.Status != value.Status || current.MustChangePassword != value.MustChangePassword
		if identityChanged && disableRelay != nil {
			if err := disableRelay(ctx); err != nil {
				return fmt.Errorf("disable Workspace sync relay before account authorization change: %w", err)
			}
		}
		row, err := sqlcgen.New(tx).UpdateUser(ctx, sqlcgen.UpdateUserParams{
			ID: value.ID, Name: value.Name, PasswordHash: value.PasswordHash, Role: string(value.Role),
			Status: string(value.Status), MustChangePassword: value.MustChangePassword,
			QuotaHosts: int32(value.QuotaHosts), QuotaCpuMilli: int32(value.QuotaCPUMilli),
			QuotaMemMib: int32(value.QuotaMemMiB), DefaultHostCpuMilli: int32(value.DefaultHostCPUMilli), DefaultHostMemMib: int32(value.DefaultHostMemMiB), ExpectedStatus: string(expectedStatus),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeConflict, "account status changed; refresh the user list")
			}
			return storeError("update user", "user", err)
		}
		updated = userDomain(row)
		if identityChanged {
			if err := stopAllOwnerTasks(ctx, tx, value.ID); err != nil {
				return err
			}
			if err := requestMCPRuntimeCancellationsForActor(ctx, tx, value.ID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE tokens SET revoked_at=COALESCE(revoked_at, now()) WHERE owner_id=$1 AND revoked_at IS NULL`, value.ID); err != nil {
				return fmt.Errorf("revoke account credentials: %w", err)
			}
			if err := revokeWorkspaceSyncDevicesForOwner(ctx, tx, value.ID); err != nil {
				return err
			}
		}
		if auditParams != nil {
			if _, err := sqlcgen.New(tx).CreateAuditLog(ctx, *auditParams); err != nil {
				return fmt.Errorf("write user update audit: %w", err)
			}
		}
		return nil
	}, nil, nil)
	if err != nil {
		return identity.User{}, err
	}
	return updated, nil
}

func (s *Store) EnsureInitialAdmin(ctx context.Context, value identity.User) error {
	if !domain.ValidUserID(value.ID) || value.Name != "admin" || value.PasswordHash == "" ||
		value.Role != identity.RoleSuperAdmin || value.Status != identity.StatusApproved || !value.MustChangePassword ||
		value.QuotaHosts < 0 || value.QuotaCPUMilli < 0 || value.QuotaMemMiB < 0 {
		return domain.InvalidArgument("user", "initial super administrator fields are invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('morde:admin-user-update', 0))`); err != nil {
			return fmt.Errorf("lock initial administrator creation: %w", err)
		}
		var id, name, role, status string
		err := tx.QueryRow(ctx, `SELECT id, name, role, status FROM users
WHERE role='super_admin' OR lower(btrim(name))='admin' FOR UPDATE`).Scan(&id, &name, &role, &status)
		if err == nil {
			if role != string(identity.RoleSuperAdmin) || status != string(identity.StatusApproved) {
				return domain.NewError(domain.CodeInvalidState, "administrator account conflicts with the initial super administrator")
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check initial administrator: %w", err)
		}
		var users int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
			return fmt.Errorf("count existing users: %w", err)
		}
		if users != 0 {
			return domain.NewError(domain.CodeInvalidState, "existing users require an administrator account migration")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, name, password_hash, role, status, must_change_password,
quota_hosts, quota_cpu_milli, quota_mem_mib) VALUES ($1, 'admin', $2, 'super_admin', 'approved', true, $3, $4, $5)`,
			value.ID, value.PasswordHash, value.QuotaHosts, value.QuotaCPUMilli, value.QuotaMemMiB); err != nil {
			return fmt.Errorf("create initial super administrator: %w", err)
		}
		return nil
	})
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
	return s.withWorkspaceSyncOwnerTx(ctx, ownerID, func(tx pgx.Tx) error {
		if _, err := lockOwnerExecutionControl(ctx, tx, ownerID, true); err != nil {
			return err
		}
		var tokenKind, installationID string
		err := tx.QueryRow(ctx, `SELECT kind,COALESCE(client_installation_id,'') FROM tokens WHERE id=$1 AND owner_id=$2 FOR UPDATE`, id, ownerID).Scan(&tokenKind, &installationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "token not found")
		}
		if err != nil {
			return fmt.Errorf("lock token for revocation: %w", err)
		}
		if _, err := stopTasksForAuthorization(ctx, tx, ownerID, id, ""); err != nil {
			return err
		}
		if tokenKind == string(identity.KindAPIKey) {
			if err := requestMCPRuntimeCancellationsForToken(ctx, tx, ownerID, id); err != nil {
				return err
			}
		}
		if _, err := sqlcgen.New(tx).RevokeToken(ctx, sqlcgen.RevokeTokenParams{ID: id, OwnerID: ownerID}); err != nil {
			return fmt.Errorf("revoke token: %w", err)
		}
		if (tokenKind == string(identity.KindClient) || tokenKind == string(identity.KindDevice)) && installationID != "" {
			if err := revokeWorkspaceSyncDevicesForInstallation(ctx, tx, ownerID, installationID); err != nil {
				return err
			}
		}
		return nil
	}, nil, nil)
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
