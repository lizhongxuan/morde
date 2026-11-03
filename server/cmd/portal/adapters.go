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
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/template"
	"morde/server/internal/orchestrator"
	"morde/server/internal/worker"
)

type cloudDSHAdapters struct {
	tasks             api.TaskService
	schedules         api.ScheduleService
	instantChat       api.InstantChatService
	scheduleProcessor worker.ScheduleProcessor
}

func cloudDSHAdaptersForRuntime(enabled bool, tasks api.TaskService, schedules api.ScheduleService,
	instantChat api.InstantChatService, scheduleProcessor worker.ScheduleProcessor) cloudDSHAdapters {
	if enabled {
		return cloudDSHAdapters{tasks: tasks, schedules: schedules, instantChat: instantChat, scheduleProcessor: scheduleProcessor}
	}
	return cloudDSHAdapters{
		tasks:       runtimeUnavailableTaskService{TaskService: tasks},
		schedules:   runtimeUnavailableScheduleService{ScheduleService: schedules},
		instantChat: runtimeUnavailableInstantChatService{InstantChatService: instantChat},
	}
}

func taskRuntimeUnavailable() error {
	return domain.NewError(domain.CodeHostNotReady, "task execution runtime is disabled")
}

type projectConversationTaskAppender struct{ tasks api.TaskService }

func (a projectConversationTaskAppender) AppendProjectMessage(ctx context.Context, principal identity.Principal, projectID, conversationID string,
	command app.AppendMessageCommand) (app.TaskSubmissionResult, error) {
	command.ProjectID = projectID
	return a.tasks.AppendMessage(ctx, principal, conversationID, command)
}

type projectWorkItemTaskLinkAuthorizer struct{ tasks api.TaskService }

func (a projectWorkItemTaskLinkAuthorizer) AuthorizeProjectTaskLink(ctx context.Context, principal identity.Principal, projectID, taskID string) error {
	if a.tasks == nil {
		return domain.NewError(domain.CodeUnsupported, "project Task authorization is unavailable")
	}
	details, err := a.tasks.Get(ctx, principal, taskID)
	if err != nil {
		return err
	}
	if details.Task.ID != taskID || details.Task.ProjectID != projectID {
		return domain.NewError(domain.CodeForbidden, "Task is not available to this project member")
	}
	return nil
}

type projectWorkspaceManifestReader interface {
	GetWorkspaceManifest(context.Context, identity.Principal, string, string, int64, string, string, int32) (app.ProjectWorkspaceFilePage, error)
}

type projectWorkItemDeliveryAuthorizer struct {
	tasks      api.TaskService
	projects   projectWorkspaceManifestReader
	savedProof app.ProjectDeliveryAuthorizer
}

func (a projectWorkItemDeliveryAuthorizer) AuthorizeSavedProjectDelivery(ctx context.Context, principal identity.Principal, projectID string,
	evidence project.WorkItemDelivery) error {
	if a.tasks == nil || a.projects == nil || a.savedProof == nil {
		return domain.NewError(domain.CodeUnsupported, "project delivery authorization is unavailable")
	}
	details, err := a.tasks.Get(ctx, principal, evidence.TaskID)
	if err != nil {
		return err
	}
	if details.Task.ID != evidence.TaskID || details.Task.ProjectID != projectID || details.Task.WorkspaceID != evidence.WorkspaceID ||
		details.Task.WorkspaceCheckpointRevision == nil || *details.Task.WorkspaceCheckpointRevision != evidence.Revision {
		return domain.NewError(domain.CodeWorkspaceConflict, "delivery evidence does not match the readable project Task checkpoint")
	}
	manifest, err := a.projects.GetWorkspaceManifest(ctx, principal, projectID, evidence.WorkspaceID, evidence.Revision, "", "", 1)
	if err != nil {
		return err
	}
	if manifest.ProjectID != projectID || manifest.WorkspaceID != evidence.WorkspaceID || manifest.Revision != evidence.Revision {
		return domain.NewError(domain.CodeWorkspaceConflict, "delivery Workspace revision is not readable by this project member")
	}
	return a.savedProof.AuthorizeSavedProjectDelivery(ctx, principal, projectID, evidence)
}

// Keep reads and stop/cleanup actions available without queuing work when no task worker runs.
type runtimeUnavailableTaskService struct{ api.TaskService }

func (s runtimeUnavailableTaskService) Submit(context.Context, identity.Principal, app.SubmitTaskCommand) (app.TaskSubmissionResult, error) {
	return app.TaskSubmissionResult{}, taskRuntimeUnavailable()
}

func (s runtimeUnavailableTaskService) AppendMessage(context.Context, identity.Principal, string, app.AppendMessageCommand) (app.TaskSubmissionResult, error) {
	return app.TaskSubmissionResult{}, taskRuntimeUnavailable()
}

func (s runtimeUnavailableTaskService) RequestAction(ctx context.Context, principal identity.Principal, taskID string,
	action task.Action, expectedStatus task.Status, idempotencyKey string) (app.TaskActionResult, error) {
	if action == task.ActionResume {
		return app.TaskActionResult{}, taskRuntimeUnavailable()
	}
	return s.TaskService.RequestAction(ctx, principal, taskID, action, expectedStatus, idempotencyKey)
}

func (s runtimeUnavailableTaskService) RetryBatchItem(context.Context, identity.Principal, string, string, string) error {
	return taskRuntimeUnavailable()
}

func (s runtimeUnavailableTaskService) RespondToPendingRequest(ctx context.Context, principal identity.Principal, taskID, pendingID string,
	version int64, decision task.PendingDecision, idempotencyKey string) error {
	if decision == task.PendingDecisionApprove {
		return taskRuntimeUnavailable()
	}
	return s.TaskService.RespondToPendingRequest(ctx, principal, taskID, pendingID, version, decision, idempotencyKey)
}

func (s runtimeUnavailableTaskService) AnswerClarificationRequest(context.Context, identity.Principal, string, string, int64, string, string) error {
	return taskRuntimeUnavailable()
}

func (s runtimeUnavailableTaskService) RequestWorkspaceViewDeletion(ctx context.Context, principal identity.Principal,
	taskID string, confirm bool, idempotencyKey string) (app.TaskWorkspaceViewDeletion, error) {
	service, ok := s.TaskService.(api.TaskWorkspaceViewDeletionService)
	if !ok {
		return app.TaskWorkspaceViewDeletion{}, domain.NewError(domain.CodeUnsupported, "task Workspace view deletion is unavailable")
	}
	return service.RequestWorkspaceViewDeletion(ctx, principal, taskID, confirm, idempotencyKey)
}

func (s runtimeUnavailableTaskService) GetWorkspaceViewDeletion(ctx context.Context, principal identity.Principal,
	taskID string) (app.TaskWorkspaceViewDeletion, error) {
	service, ok := s.TaskService.(api.TaskWorkspaceViewDeletionService)
	if !ok {
		return app.TaskWorkspaceViewDeletion{}, domain.NewError(domain.CodeUnsupported, "task Workspace view deletion is unavailable")
	}
	return service.GetWorkspaceViewDeletion(ctx, principal, taskID)
}

func (s runtimeUnavailableTaskService) RevokeMCPConnectorGrant(ctx context.Context, principal identity.Principal,
	taskID, connectorID string, expectedVersion int64) (app.TaskMCPConnectorGrant, error) {
	service, ok := s.TaskService.(api.TaskMCPConnectorGrantService)
	if !ok {
		return app.TaskMCPConnectorGrant{}, domain.NewError(domain.CodeUnsupported, "task MCP connector grants are unavailable")
	}
	return service.RevokeMCPConnectorGrant(ctx, principal, taskID, connectorID, expectedVersion)
}

type runtimeUnavailableScheduleService struct{ api.ScheduleService }

func (s runtimeUnavailableScheduleService) Create(context.Context, identity.Principal, app.CreateScheduleCommand, string) (schedule.Schedule, error) {
	return schedule.Schedule{}, taskRuntimeUnavailable()
}

func (s runtimeUnavailableScheduleService) Action(ctx context.Context, principal identity.Principal, id string,
	command app.ScheduleActionCommand, idempotencyKey string) (app.ScheduleActionResult, error) {
	if command.Action == schedule.ActionEnable {
		return app.ScheduleActionResult{}, taskRuntimeUnavailable()
	}
	return s.ScheduleService.Action(ctx, principal, id, command, idempotencyKey)
}

type runtimeUnavailableInstantChatService struct{ api.InstantChatService }

func (s runtimeUnavailableInstantChatService) Send(context.Context, identity.Principal, app.InstantChatSend) (app.InstantChatReply, error) {
	return app.InstantChatReply{}, taskRuntimeUnavailable()
}

type authAdapter struct {
	service *app.CredentialService
	users   *app.UserService
}

func (a authAdapter) Register(ctx context.Context, name, password, ip string, inviteCode ...string) (api.UserView, error) {
	value, err := a.users.Register(ctx, name, password, ip, inviteCode...)
	if err != nil {
		return api.UserView{}, err
	}
	return userView(value), nil
}

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

func (a authAdapter) ChangePassword(ctx context.Context, principal identity.Principal, current, next string) error {
	return a.service.ChangePassword(ctx, principal, current, next)
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
		ID: value.ID, Name: value.Name, Role: value.Role, Status: value.Status,
		MustChangePassword: value.MustChangePassword,
		Quota:              totals(value.Quota), Usage: totals(value.Usage),
	}
}

type tokenAdapter struct{ service *app.TokenService }

func (a tokenAdapter) ListAPIKeys(ctx context.Context, principal identity.Principal) ([]identity.Token, error) {
	return a.service.ListAPIKeys(ctx, principal)
}

func (a tokenAdapter) CreateAPIKey(ctx context.Context, principal identity.Principal, name string, ttl time.Duration) (api.CreatedAPIKey, error) {
	value, err := a.service.CreateAPIKey(ctx, principal, name, ttl)
	if err != nil {
		return api.CreatedAPIKey{}, err
	}
	return api.CreatedAPIKey{ID: value.ID, Key: value.Key, ExpiresAt: value.ExpiresAt}, nil
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
	users                   *app.UserService
	audit                   *app.AuditService
	workspaceSyncReconciler app.WorkspaceSyncNodeOwnerReconciler
	capacity                interface {
		Capacity(context.Context, identity.Principal) (app.CapacitySnapshot, error)
	}
}

func (a adminAdapter) ListUsers(ctx context.Context, principal identity.Principal, status *identity.AccountStatus) ([]api.AdminUserRecord, error) {
	values, err := a.users.List(ctx, principal, status)
	if err != nil {
		return nil, err
	}
	result := make([]api.AdminUserRecord, 0, len(values))
	for _, value := range values {
		result = append(result, adminUser(value))
	}
	return result, nil
}

func (a adminAdapter) ListUserPage(ctx context.Context, principal identity.Principal, status *identity.AccountStatus, limit, offset int) (app.AdminPage[api.AdminUserRecord], error) {
	page, err := a.users.ListPage(ctx, principal, status, limit, offset)
	if err != nil {
		return app.AdminPage[api.AdminUserRecord]{}, err
	}
	result := app.AdminPage[api.AdminUserRecord]{Items: make([]api.AdminUserRecord, 0, len(page.Items)), Total: page.Total, Limit: page.Limit, Offset: page.Offset}
	for _, value := range page.Items {
		result.Items = append(result.Items, adminUser(value))
	}
	return result, nil
}

func (a adminAdapter) GetUser(ctx context.Context, principal identity.Principal, id string) (api.AdminUserRecord, error) {
	value, err := a.users.Get(ctx, principal, id)
	if err != nil {
		return api.AdminUserRecord{}, err
	}
	return adminUser(value), nil
}

func (a adminAdapter) UpdateUser(ctx context.Context, principal identity.Principal, id string, command api.UpdateAdminUserCommand) (api.AdminUserRecord, error) {
	var quota *app.ResourceUsage
	if command.Quota != nil {
		value := usage(*command.Quota)
		quota = &value
	}
	var defaultHostResources *app.HostResources
	if command.DefaultHostResources != nil {
		defaultHostResources = &app.HostResources{CPUMilli: command.DefaultHostResources.CPUMilli, MemMiB: command.DefaultHostResources.MemMiB}
	}
	updated, err := a.users.Update(ctx, principal, id, app.UpdateUserCommand{
		ExpectedStatus: command.ExpectedStatus, Role: command.Role, Status: command.Status, Quota: quota, DefaultHostResources: defaultHostResources,
	})
	if err != nil {
		return api.AdminUserRecord{}, err
	}
	if (command.Status != nil && *command.Status != identity.StatusApproved || command.Role != nil) && a.workspaceSyncReconciler != nil {
		if err := a.workspaceSyncReconciler.ReconcileWorkspaceSyncNodeOwner(ctx, id); err != nil {
			return api.AdminUserRecord{}, domain.NewError(domain.CodeUpstreamError, "workspace sync node could not confirm account revocation")
		}
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
		ID: value.ID, Name: value.Name, Role: value.Role, Status: value.Status,
		Quota: totals(value.Quota), Usage: totals(value.Usage), CreatedAt: value.CreatedAt,
		DefaultHostResources: api.HostResourcesView{CPUMilli: value.DefaultHostResources.CPUMilli, MemMiB: value.DefaultHostResources.MemMiB},
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
	workspaceBlockGC             *app.WorkspaceBlockGarbageCollector
	workspaceSyncNodeProvisioner *worker.WorkspaceSyncNodeProvisioner
	hostUsage                    *app.HostUsageService
	logger                       *zap.Logger
}

func (a workerApplicationAdapter) CollectHostUsage(ctx context.Context) error {
	if a.hostUsage == nil {
		return errors.New("host usage service is unavailable")
	}
	return a.hostUsage.CollectAll(ctx)
}

func (a workerApplicationAdapter) Reconcile(ctx context.Context) (map[string]int, error) {
	actions, reconcileErr := a.WorkerApp.Reconcile(ctx)
	var gcErr error
	if a.workspaceBlockGC != nil {
		stats, collectErr := a.workspaceBlockGC.Collect(ctx)
		gcErr = collectErr
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
	}
	var syncNodeErr error
	if a.workspaceSyncNodeProvisioner != nil {
		syncNodeErr = a.workspaceSyncNodeProvisioner.ReconcilePendingWorkspaceSyncNodes(ctx)
	}
	return actions, errors.Join(reconcileErr, gcErr, syncNodeErr)
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
var _ api.HostUsageReceiptService = (*app.HostUsageService)(nil)
var _ api.WorkspaceService = (*app.WorkspaceService)(nil)
var _ app.AuditWriter = countedAuditWriter{}
var _ worker.Application = workerApplicationAdapter{}
var _ interface{ CollectHostUsage(context.Context) error } = workerApplicationAdapter{}
var _ worker.LeaderLock = leaderLockAdapter{}
