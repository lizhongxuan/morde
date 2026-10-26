package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/template"
	"morde/server/internal/orchestrator"
	"morde/server/internal/worker"
)

type authAdapter struct{ service *app.CredentialService }

func (a authAdapter) Login(ctx context.Context, name, password, ip string) (api.LoginResult, error) {
	value, err := a.service.Login(ctx, name, password, ip)
	if err != nil {
		return api.LoginResult{}, err
	}
	return api.LoginResult{
		Credential: value.Credential,
		ExpiresAt:  value.ExpiresAt,
		User:       userView(value.User),
	}, nil
}

func (a authAdapter) Logout(ctx context.Context, principal identity.Principal) error {
	return a.service.Logout(ctx, principal)
}

func (a authAdapter) CurrentUser(ctx context.Context, principal identity.Principal) (api.UserView, error) {
	value, err := a.service.CurrentUser(ctx, principal)
	if err != nil {
		return api.UserView{}, err
	}
	return userView(value), nil
}

func userView(value app.UserSummary) api.UserView {
	return api.UserView{
		ID: value.ID, Name: value.Name, Role: value.Role,
		Quota: totals(value.Quota), Usage: totals(value.Usage),
	}
}

type tokenAdapter struct{ service *app.TokenService }

func (a tokenAdapter) ListAPIKeys(ctx context.Context, principal identity.Principal) ([]identity.Token, error) {
	return a.service.ListAPIKeys(ctx, principal)
}

func (a tokenAdapter) CreateAPIKey(ctx context.Context, principal identity.Principal, name string, hostIDs []string, scopes []identity.Scope, ttl time.Duration) (api.CreatedAPIKey, error) {
	value, err := a.service.CreateAPIKey(ctx, principal, name, hostIDs, scopes, ttl)
	if err != nil {
		return api.CreatedAPIKey{}, err
	}
	return api.CreatedAPIKey{ID: value.ID, Key: value.Key, HostIDs: value.HostIDs, Scopes: value.Scopes, ExpiresAt: value.ExpiresAt}, nil
}

func (a tokenAdapter) DeleteAPIKey(ctx context.Context, principal identity.Principal, id string) error {
	return a.service.DeleteAPIKey(ctx, principal, id)
}

type templateAdapter struct{ values map[string]template.Template }

func (a templateAdapter) ListTemplates(context.Context) ([]api.TemplateRecord, error) {
	ids := make([]string, 0, len(a.values))
	for id := range a.values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]api.TemplateRecord, 0, len(ids))
	for _, id := range ids {
		value := a.values[id]
		cpu, err := template.CPUMilli(value.Resources.Requests.CPU)
		if err != nil {
			return nil, fmt.Errorf("read template %q cpu request: %w", id, err)
		}
		memory, err := template.MemMiB(value.Resources.Requests.Memory)
		if err != nil {
			return nil, fmt.Errorf("read template %q memory request: %w", id, err)
		}
		cpuLimit, err := template.CPUMilli(value.Resources.Limits.CPU)
		if err != nil {
			return nil, fmt.Errorf("read template %q cpu limit: %w", id, err)
		}
		memoryLimit, err := template.MemMiB(value.Resources.Limits.Memory)
		if err != nil {
			return nil, fmt.Errorf("read template %q memory limit: %w", id, err)
		}
		workspaceSize, err := template.MemMiB(value.Workspace.Size)
		if err != nil {
			return nil, fmt.Errorf("read template %q workspace size: %w", id, err)
		}
		for _, field := range []int64{cpu, memory, cpuLimit, memoryLimit, workspaceSize} {
			if field > math.MaxInt {
				return nil, fmt.Errorf("template %q resource value exceeds platform integer range", id)
			}
		}
		result = append(result, api.TemplateRecord{
			ID: id, Name: value.Name, Description: value.Description, Isolation: value.Isolation,
			Resources: api.ResourceRecord{
				CPUMilli: int(cpu), MemMiB: int(memory), CPULimitMilli: int(cpuLimit), MemLimitMiB: int(memoryLimit),
			},
			WorkspaceSizeMiB: int(workspaceSize), TTLSeconds: int64(value.Lifecycle.TTL / time.Second),
			IdleTimeoutSeconds: int64(value.Lifecycle.IdleTimeout / time.Second),
			OnIdle:             value.Lifecycle.OnIdle, KeepWorkspaceDefault: value.KeepWorkspaceDefault,
		})
	}
	return result, nil
}

type adminAdapter struct {
	users    *app.UserService
	audit    *app.AuditService
	capacity interface {
		Capacity(context.Context, identity.Principal) (app.CapacitySnapshot, error)
	}
}

func (a adminAdapter) ListUsers(ctx context.Context, principal identity.Principal) ([]api.AdminUserRecord, error) {
	values, err := a.users.List(ctx, principal)
	if err != nil {
		return nil, err
	}
	result := make([]api.AdminUserRecord, 0, len(values))
	for _, value := range values {
		result = append(result, adminUser(value))
	}
	return result, nil
}

func (a adminAdapter) CreateUser(ctx context.Context, principal identity.Principal, command api.CreateAdminUserCommand) (api.AdminUserRecord, error) {
	var quota *app.ResourceUsage
	if command.Quota != nil {
		value := usage(*command.Quota)
		quota = &value
	}
	created, err := a.users.Create(ctx, principal, app.CreateUserCommand{
		Name: command.Name, Password: command.Password, Role: command.Role, Quota: quota,
	})
	if err != nil {
		return api.AdminUserRecord{}, err
	}
	return adminUser(created), nil
}

func (a adminAdapter) UpdateUser(ctx context.Context, principal identity.Principal, id string, command api.UpdateAdminUserCommand) (api.AdminUserRecord, error) {
	var quota *app.ResourceUsage
	if command.Quota != nil {
		value := usage(*command.Quota)
		quota = &value
	}
	updated, err := a.users.Update(ctx, principal, id, app.UpdateUserCommand{
		Role: command.Role, Password: command.Password, Disabled: command.Disabled, Quota: quota,
	})
	if err != nil {
		return api.AdminUserRecord{}, err
	}
	return adminUser(updated), nil
}

func (a adminAdapter) Capacity(ctx context.Context, principal identity.Principal) (api.CapacityRecord, error) {
	value, err := a.capacity.Capacity(ctx, principal)
	if err != nil {
		return api.CapacityRecord{}, err
	}
	if value.Limit.CPUMilli > math.MaxInt || value.Limit.MemMiB > math.MaxInt {
		return api.CapacityRecord{}, fmt.Errorf("capacity value exceeds platform integer range")
	}
	return api.CapacityRecord{
		Limit:     totals(app.ResourceUsage{CPUMilli: int(value.Limit.CPUMilli), MemMiB: int(value.Limit.MemMiB)}),
		Allocated: totals(value.Allocated), HostsByState: value.HostsByState,
	}, nil
}

func (a adminAdapter) ListAudit(ctx context.Context, principal identity.Principal, query api.AuditQuery) ([]api.AuditRecord, error) {
	values, err := a.audit.List(ctx, principal, app.AuditFilter{
		Actor: query.Actor, Action: query.Action, Target: query.Target, Since: query.Since, Limit: int32(query.Limit),
	})
	if err != nil {
		return nil, err
	}
	result := make([]api.AuditRecord, 0, len(values))
	for _, value := range values {
		result = append(result, api.AuditRecord{ID: value.ID, At: value.At, Entry: value.Entry})
	}
	return result, nil
}

func adminUser(value app.UserSummary) api.AdminUserRecord {
	return api.AdminUserRecord{
		ID: value.ID, Name: value.Name, Role: value.Role, Disabled: value.Disabled,
		Quota: totals(value.Quota), Usage: totals(value.Usage), CreatedAt: value.CreatedAt,
	}
}

func totals(value app.ResourceUsage) api.TotalsView {
	return api.TotalsView{Hosts: value.Hosts, CPUMilli: value.CPUMilli, MemMiB: value.MemMiB}
}

func usage(value api.TotalsView) app.ResourceUsage {
	return app.ResourceUsage{Hosts: value.Hosts, CPUMilli: value.CPUMilli, MemMiB: value.MemMiB}
}

type countedAuditWriter struct {
	writer   app.AuditWriter
	failures prometheus.Counter
}

func (w countedAuditWriter) Write(ctx context.Context, entry audit.Entry) error {
	if err := w.writer.Write(ctx, entry); err != nil {
		w.failures.Inc()
		return err
	}
	return nil
}

type readiness struct {
	pool     *pgxpool.Pool
	clients  *orchestrator.Router
	profiles map[string]config.Profile
	instance string
}

type workerApplicationAdapter struct {
	*app.WorkerApp
	workspaceBlockGC *app.WorkspaceBlockGarbageCollector
	logger           *zap.Logger
}

func (a workerApplicationAdapter) Reconcile(ctx context.Context) (map[string]int, error) {
	actions, reconcileErr := a.WorkerApp.Reconcile(ctx)
	if a.workspaceBlockGC == nil {
		return actions, reconcileErr
	}
	stats, gcErr := a.workspaceBlockGC.Collect(ctx)
	if stats.BlocksDeleted > 0 || stats.SessionsExpired > 0 {
		logger := a.logger
		if logger == nil {
			logger = zap.NewNop()
		}
		logger.Info("workspace block garbage collection completed",
			zap.Int64("workspaces_scanned", stats.WorkspacesScanned),
			zap.Int64("sessions_expired", stats.SessionsExpired),
			zap.Int64("blocks_deleted", stats.BlocksDeleted),
			zap.Int64("block_bytes_unlinked", stats.BytesReclaimed),
		)
	}
	return actions, errors.Join(reconcileErr, gcErr)
}

func (a workerApplicationAdapter) TryReconcilerLock(ctx context.Context) (worker.LeaderLock, bool, error) {
	lock, acquired, err := a.WorkerApp.TryReconcilerLock(ctx)
	if err != nil || !acquired || lock == nil {
		return nil, acquired, err
	}
	return leaderLockAdapter{LeaderLock: lock}, true, nil
}

type leaderLockAdapter struct{ app.LeaderLock }

func (r readiness) CheckReady(ctx context.Context) error {
	if err := r.pool.Ping(ctx); err != nil {
		return fmt.Errorf("database is not ready: %w", err)
	}
	if err := checkNetworkPolicies(ctx, r.profiles); err != nil {
		return err
	}
	for _, name := range r.clients.Profiles() {
		client, err := r.clients.For(name)
		if err != nil {
			return fmt.Errorf("sandbox profile %s is unavailable: %w", name, err)
		}
		if _, err := client.List(ctx, map[string]string{"ch-env": r.instance}); err != nil {
			return fmt.Errorf("sandbox profile %s is not ready: %w", name, err)
		}
	}
	return nil
}

func checkNetworkPolicies(ctx context.Context, profiles map[string]config.Profile) error {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := orchestrator.CheckNetworkPolicy(ctx, profiles[name].NetworkGuardPath, profiles[name].BaseURL); err != nil {
			return fmt.Errorf("network policy guard for profile %s is not ready: %w", name, err)
		}
	}
	return nil
}

var _ api.AuthService = authAdapter{}
var _ api.TokenService = tokenAdapter{}
var _ api.TemplateReader = templateAdapter{}
var _ api.AdminService = adminAdapter{}
var _ api.ReadinessChecker = readiness{}
var _ api.WorkspaceService = (*app.WorkspaceService)(nil)
var _ app.AuditWriter = countedAuditWriter{}
var _ worker.Application = workerApplicationAdapter{}
var _ worker.LeaderLock = leaderLockAdapter{}
