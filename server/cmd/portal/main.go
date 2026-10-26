package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"morde/server/internal/aio"
	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/gateway"
	"morde/server/internal/httpserver"
	"morde/server/internal/mcp"
	"morde/server/internal/model"
	"morde/server/internal/orchestrator"
	"morde/server/internal/storage"
	"morde/server/internal/storage/sitefs"
	"morde/server/internal/storage/workspacefs"
	"morde/server/internal/store/postgres"
	"morde/server/internal/worker"
	"morde/server/migrations"
)

const defaultConfigPath = "/etc/cloudhost/portal.yaml"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "serve":
		path, err := parseConfigFlag("serve", args[1:], stderr)
		if err != nil {
			return err
		}
		return serve(ctx, path)
	case "migrate":
		if len(args) < 2 || (args[1] != "up" && args[1] != "status") {
			return fmt.Errorf("usage: portal migrate up|status [--config path]")
		}
		path, err := parseConfigFlag("migrate", args[2:], stderr)
		if err != nil {
			return err
		}
		return migrate(ctx, path, args[1], stdout)
	case "user":
		if len(args) < 2 || args[1] != "add" {
			return fmt.Errorf("usage: portal user add --name name [--role admin|user] [--config path]")
		}
		return addUser(ctx, args[2:], stdin, stdout, stderr)
	case "workspace":
		if len(args) < 2 {
			return fmt.Errorf("usage: portal workspace verify [--config path] | import-legacy --workspace id [--config path]")
		}
		switch args[1] {
		case "verify":
			path, err := parseConfigFlag("workspace verify", args[2:], stderr)
			if err != nil {
				return err
			}
			return verifyWorkspaceContent(ctx, path, stdout)
		case "import-legacy":
			workspaceID, path, err := parseWorkspaceImportFlags(args[2:], stderr)
			if err != nil {
				return err
			}
			return importLegacyWorkspace(ctx, path, workspaceID, stdout)
		default:
			return fmt.Errorf("usage: portal workspace verify [--config path] | import-legacy --workspace id [--config path]")
		}
	default:
		return usageError()
	}
}

func usageError() error {
	return fmt.Errorf("usage: portal serve|migrate up|migrate status|user add|workspace verify|workspace import-legacy")
}

func parseWorkspaceImportFlags(args []string, stderr io.Writer) (string, string, error) {
	flags := flag.NewFlagSet("workspace import-legacy", flag.ContinueOnError)
	flags.SetOutput(stderr)
	workspaceID := flags.String("workspace", "", "workspace ID whose legacy Docker volume should be imported")
	configPath := flags.String("config", defaultConfigPath, "path to portal.yaml")
	if err := flags.Parse(args); err != nil {
		return "", "", err
	}
	if flags.NArg() != 0 {
		return "", "", fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if !domain.ValidWorkspaceID(*workspaceID) {
		return "", "", fmt.Errorf("--workspace must be a valid workspace ID")
	}
	return *workspaceID, *configPath, nil
}

func parseConfigFlag(name string, args []string, stderr io.Writer) (string, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath, "path to portal.yaml")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if flags.NArg() != 0 {
		return "", fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	return *path, nil
}

func serve(ctx context.Context, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := checkNetworkPolicies(ctx, cfg.Orchestrator.Profiles); err != nil {
		return err
	}
	logger, err := newLogger(cfg.Log)
	if err != nil {
		return err
	}
	defer func() { _ = logger.Sync() }()
	if err := requireLoopbackMetrics(cfg.Listen.Metrics); err != nil {
		return err
	}
	pool, err := openPool(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	currentVersion, migrationVersion, err := schemaVersions(ctx, pool)
	if err != nil {
		return err
	}
	if currentVersion != migrationVersion {
		return fmt.Errorf("database schema version %d does not match latest migration version %d; run portal migrate up", currentVersion, migrationVersion)
	}
	if err := postgres.CheckSchema(ctx, pool); err != nil {
		return err
	}
	store := postgres.New(pool)
	workspaceBlocks, err := workspacefs.Open(cfg.WorkspaceStorage.RootDir)
	if err != nil {
		return fmt.Errorf("open workspace block storage: %w", err)
	}
	defer func() { _ = workspaceBlocks.Close() }()
	if err := reconcileWorkspaceBlockUsage(ctx, store, workspaceBlocks, cfg.WorkspaceStorage.MaxOwnerBytes); err != nil {
		return fmt.Errorf("reconcile workspace physical usage before serving: %w", err)
	}
	pendingImports, err := store.ListPendingWorkspaceLegacyVolumes(ctx)
	if err != nil {
		return fmt.Errorf("check legacy workspace imports: %w", err)
	}
	if len(pendingImports) > 0 {
		ids := make([]string, 0, len(pendingImports))
		for _, pending := range pendingImports {
			ids = append(ids, pending.WorkspaceID)
		}
		return fmt.Errorf("legacy workspace volumes must be imported before serving (pending workspaces: %s); run portal workspace import-legacy --workspace <id>", strings.Join(ids, ", "))
	}
	workspaceCapacity, err := storage.NewWorkspaceCapacity(cfg.WorkspaceStorage.RootDir, cfg.WorkspaceStorage.MinFreeBytes)
	if err != nil {
		return fmt.Errorf("initialize workspace storage capacity: %w", err)
	}
	siteArtifacts, err := sitefs.Open(cfg.SiteStorage.RootDir, cfg.SiteStorage.MaxReleaseBytes, cfg.SiteStorage.MinFreeBytes)
	if err != nil {
		return fmt.Errorf("open site artifact storage: %w", err)
	}
	defer func() { _ = siteArtifacts.Close() }()
	registry := prometheus.NewRegistry()
	clients := make(map[string]app.SandboxClient, len(cfg.Orchestrator.Profiles))
	for name, profile := range cfg.Orchestrator.Profiles {
		profile.Name = name
		clients[name] = orchestrator.NewClient(profile, &http.Client{Timeout: profile.RequestTimeout}, registry)
	}
	router := orchestrator.NewRouter(clients)
	templates, err := config.LoadTemplates(cfg.Templates.Dir, cfg.Orchestrator.Profiles)
	if err != nil {
		return err
	}
	if _, ok := templates[cfg.Templates.Default]; !ok {
		return fmt.Errorf("default template %q is not configured", cfg.Templates.Default)
	}
	dataKey, err := readKey(cfg.Security.DataKeyFile)
	if err != nil {
		return fmt.Errorf("read security.data_key_file: %w", err)
	}
	sealer, err := app.NewSealer(dataKey)
	if err != nil {
		return err
	}
	modelClient, err := model.NewResponsesClient(cfg.Models.AllowedHosts)
	if err != nil {
		return fmt.Errorf("initialize model client: %w", err)
	}
	modelConnections, err := app.NewModelConnectionService(store, sealer, modelClient, cfg.Models.AllowedHosts)
	if err != nil {
		return fmt.Errorf("initialize model connections: %w", err)
	}
	aioClient := aio.New(nil)
	volumes, err := newVolumeDeleter(cfg.Storage)
	if err != nil {
		return err
	}
	auditService, err := app.NewAuditService(store)
	if err != nil {
		return err
	}
	auditFailures, err := registerCounter(registry, prometheus.CounterOpts{
		Name: "cloudhost_audit_write_failures_total", Help: "Audit write failures.",
	})
	if err != nil {
		return err
	}
	countedAudit := countedAuditWriter{writer: auditService, failures: auditFailures}
	readyMetric, err := registerHistogram(registry, prometheus.HistogramOpts{
		Name: "cloudhost_host_ready_duration_seconds", Help: "Host readiness duration.",
	}, "result")
	if err != nil {
		return err
	}
	queue := worker.NewCreateQueue()
	runtime, err := app.NewRuntime(store, router, sealer, cfg.Gateway.UpstreamCacheTTL)
	if err != nil {
		return err
	}
	runtimeControl, err := app.NewRuntimeControl(store, router, sealer)
	if err != nil {
		return err
	}
	hosts, err := app.NewHostService(app.HostServiceOptions{
		Hosts: store, Workspaces: store, Tokens: store,
		Sandboxes: router, Probe: aioClient, Runtime: runtimeControl, Audit: auditService, CreateLocks: store, Sealer: sealer, Volumes: volumes, DiskCapacity: volumes,
		Templates: templates, Capacity: app.Capacity{CPUMilli: cfg.Capacity.CPUMilli, MemMiB: cfg.Capacity.MemMiB},
		Instance: cfg.Instance, Scheme: cfg.Domains.Scheme, PortalURL: cfg.Domains.Scheme + "://" + cfg.Domains.Portal, HostSuffix: cfg.Domains.HostSuffix,
		TicketTTL: cfg.Security.TicketTTL, ReadyTimeout: cfg.Hosts.ReadyTimeout,
		ReadyPollInterval: cfg.Hosts.ReadyPollInterval, NotifyCreate: queue.Notify,
		ObserveHostReady: func(result string, duration time.Duration) {
			readyMetric.WithLabelValues(result).Observe(duration.Seconds())
		},
		CountAuditFailure: auditFailures.Inc, Logger: logger,
	})
	if err != nil {
		return err
	}
	hostSharing, err := app.NewHostSharingService(store, store, store, store, auditService,
		cfg.Domains.Scheme, cfg.Domains.HostSuffix, cfg.Security.TicketTTL, logger, auditFailures.Inc)
	if err != nil {
		return fmt.Errorf("initialize Host sharing: %w", err)
	}
	mcpHosts, err := app.NewMCPComputerHostAuthorizer(store, hostSharing)
	if err != nil {
		return fmt.Errorf("initialize MCP Host authorization: %w", err)
	}
	credentials, err := app.NewCredentialService(store, store, store, countedAudit, app.CredentialOptions{
		SessionTTL: cfg.Security.SessionTTL, TicketTTL: cfg.Security.TicketTTL,
		LoginLimit: loginLimit(cfg.Security.LoginRateLimit), LoginWindow: time.Minute,
	})
	if err != nil {
		return err
	}
	users, err := app.NewUserService(store, store, countedAudit)
	if err != nil {
		return err
	}
	workspaces, err := app.NewWorkspaceService(
		store, workspaceBlocks, workspaceCapacity, countedAudit,
		cfg.WorkspaceStorage.MaxWorkspaceBytes,
	)
	if err != nil {
		return err
	}
	adminWorkspaces, err := app.NewAdminWorkspaceService(store)
	if err != nil {
		return err
	}
	workspaceSync, err := app.NewWorkspaceSyncService(store, workspaceBlocks, workspace.MaxSyncManifestBytes, cfg.WorkspaceStorage.MaxOwnerBytes)
	if err != nil {
		return err
	}
	siteService, err := app.NewSiteService(store, store, workspaceBlocks, siteArtifacts, cfg.SiteStorage.MaxReleaseBytes, cfg.SiteStorage.MaxOwnerBytes)
	if err != nil {
		return fmt.Errorf("initialize Site service: %w", err)
	}
	hostReplicas, err := app.NewHostReplicaService(store, store, store, sealer)
	if err != nil {
		return err
	}
	tokens, err := app.NewTokenService(store, store, countedAudit, 30*24*time.Hour, 365*24*time.Hour, hostSharing)
	if err != nil {
		return err
	}
	clientAuthorizations, err := app.NewClientAuthorizationService(store, store)
	if err != nil {
		return err
	}
	taskService, err := app.NewTaskService(store, credentials, modelConnections)
	if err != nil {
		return err
	}
	scheduleService, err := app.NewScheduleService(store, credentials, modelConnections)
	if err != nil {
		return err
	}
	notificationService, err := app.NewNotificationService(store)
	if err != nil {
		return err
	}
	onboardingService, err := app.NewOnboardingService(store, hosts, modelConnections, cfg.Templates.Default)
	if err != nil {
		return fmt.Errorf("initialize onboarding service: %w", err)
	}
	executionControlService, err := app.NewExecutionControlService(store, auditService)
	if err != nil {
		return err
	}
	controlService, err := app.NewControlService(store, credentials, store, store, store, runtimeControl, auditService, store)
	if err != nil {
		return err
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl)
	if err != nil {
		return err
	}
	agentTools, err := app.NewScheduleAgentToolExecutor(runtimeTools, scheduleService)
	if err != nil {
		return err
	}
	agent, err := app.NewAgent(store, modelConnections, modelClient, credentials, runtimeControl, agentTools)
	if err != nil {
		return err
	}
	mcpRuntimeTools, err := app.NewMCPRuntimeToolExecutor(store, mcpHosts, credentials, store, runtimeControl)
	if err != nil {
		return err
	}
	workerSuffix, err := domain.NewOperationID()
	if err != nil {
		return fmt.Errorf("generate task worker identity: %w", err)
	}
	taskWorker, err := worker.NewTaskWorker(worker.TaskWorkerOptions{
		Store: store, Executor: agent, WorkerID: workerSuffix,
		LeaseDuration: 30 * time.Second, PollInterval: 500 * time.Millisecond, Logger: logger,
	})
	if err != nil {
		return err
	}
	taskStopWorker, err := worker.NewTaskStopWorker(worker.TaskStopWorkerOptions{
		Store: store, Executor: runtimeControl, WorkerID: workerSuffix + "-stop",
		LeaseDuration: 30 * time.Second, PollInterval: 500 * time.Millisecond,
		OperationTimeout: 10 * time.Second, RetryAfter: time.Second, MCPRuntimeStopper: mcpRuntimeTools, Logger: logger,
	})
	if err != nil {
		return err
	}
	conversationService, err := app.NewConversationService(store, credentials)
	if err != nil {
		return err
	}
	mcpHandler, err := mcp.New(mcp.Deps{
		Config: cfg.MCP, Domains: cfg.Domains, Hosts: hosts, Applications: mcpRuntimeTools, RuntimeTools: mcpRuntimeTools,
		Tasks: taskService, Schedules: scheduleService, Auth: credentials, Audit: auditService, Logger: logger, Metrics: registry,
		PreviewPortMin: cfg.Gateway.PreviewPortMin, PreviewPortDeny: cfg.Gateway.PreviewPortDeny,
	})
	if err != nil {
		return err
	}
	gatewayHandler, err := gateway.New(gateway.Deps{
		Config: cfg.Gateway, Domains: cfg.Domains, Security: cfg.Security,
		Hosts: hosts, HostSharing: hostSharing, Control: controlService, Runtime: runtime, ActivityLeases: store, InputPermits: store,
		TerminalSessions: store, TerminalAIO: aioClient, Auth: credentials, Audit: auditService,
		Logger: logger, Metrics: registry,
	})
	if err != nil {
		return err
	}
	admin := adminAdapter{users: users, audit: auditService, capacity: hosts}
	apiHandler, err := api.New(api.Deps{
		Hosts: hosts, HostSharing: hostSharing, HostReplicas: hostReplicas, TLSAuthorizer: hosts, Verifier: credentials, Auth: authAdapter{service: credentials},
		WorkspaceOps: workspaces, WorkspaceSync: workspaceSync, Sites: siteService, Templates: templateAdapter{values: templates},
		Tokens: tokenAdapter{service: tokens}, ClientAuthorizations: clientAuthorizations,
		Tasks: taskService, Schedules: scheduleService, Notifications: notificationService, ExecutionControl: executionControlService,
		Control: controlService, Conversations: conversationService, ModelConnections: modelConnections, Onboarding: onboardingService, Admin: admin, AdminWorkspaces: adminWorkspaces,
		Readiness: readiness{pool: pool, clients: router, profiles: cfg.Orchestrator.Profiles, instance: cfg.Instance}, MCP: mcpHandler,
		MCPDisabled: !cfg.MCP.Enabled,
	}, api.Options{
		Logger: logger, Metrics: registry, APIMaxBodyBytes: cfg.HTTP.APIMaxBodyBytes,
		MCPMaxBodyBytes: cfg.HTTP.MCPMaxBodyBytes, CookieSecure: cfg.Domains.CookieSecure,
		SessionTTL: cfg.Security.SessionTTL,
		HostScheme: cfg.Domains.Scheme, PortalDomain: cfg.Domains.Portal, HostSuffix: cfg.Domains.HostSuffix,
		PreviewPortMin: cfg.Gateway.PreviewPortMin, PreviewPortDeny: cfg.Gateway.PreviewPortDeny,
		PortalFiles: os.DirFS(cfg.PortalWeb.Dir),
	})
	if err != nil {
		return err
	}
	workerApp, err := app.NewWorkerApp(hosts, runtime, store, store, cfg.Reconciler.RenewBefore)
	if err != nil {
		return err
	}
	workspaceBlockGC, err := app.NewWorkspaceBlockGarbageCollector(store, workspaceBlocks)
	if err != nil {
		return fmt.Errorf("initialize workspace block garbage collection: %w", err)
	}
	background, err := worker.New(worker.Options{
		Application: workerApplicationAdapter{WorkerApp: workerApp, workspaceBlockGC: workspaceBlockGC, logger: logger}, CreateQueue: queue, Interval: cfg.Reconciler.Interval,
		ActivityFlushInterval: cfg.Reconciler.ActivityFlushInterval, Logger: logger, Registerer: registry,
	})
	if err != nil {
		return err
	}
	scheduleWorker, err := worker.NewScheduleWorker(scheduleService, time.Second, logger)
	if err != nil {
		return err
	}
	notificationWorker, err := worker.NewNotificationWorker(worker.NotificationWorkerOptions{
		Service: notificationService, PollInterval: time.Second, BatchSize: 100, Logger: logger,
	})
	if err != nil {
		return err
	}
	apiServer := httpserver.NewServer(cfg.Listen.API, apiHandler, cfg.HTTP)
	gatewayServer := httpserver.NewServer(cfg.Listen.Gateway, gatewayHandler, cfg.HTTP)
	metricsServer := httpserver.NewServer(cfg.Listen.Metrics, metricsHandler(registry), cfg.HTTP)
	logger.Info("portal starting", zap.String("instance", cfg.Instance), zap.Int64("migration_version", migrationVersion), zap.String("api", cfg.Listen.API), zap.String("gateway", cfg.Listen.Gateway), zap.String("metrics", cfg.Listen.Metrics))
	return runServers(ctx, logger, cfg.HTTP.ShutdownTimeout, background, taskWorker, taskStopWorker, scheduleWorker, notificationWorker, runtime, gatewayHandler, apiServer, gatewayServer, metricsServer)
}

func migrate(ctx context.Context, configPath, action string, stdout io.Writer) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	pool, err := openPool(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	if action == "up" {
		if err := postgres.Migrate(ctx, pool); err != nil {
			return err
		}
	}
	current, latest, err := schemaVersions(ctx, pool)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "database migration version: current=%d latest=%d\n", current, latest); err != nil {
		return fmt.Errorf("write migration status: %w", err)
	}
	if action == "up" && current != latest {
		return fmt.Errorf("database schema version %d does not match latest migration version %d", current, latest)
	}
	return nil
}

func verifyWorkspaceContent(ctx context.Context, configPath string, stdout io.Writer) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	pool, err := openPool(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	current, latest, err := schemaVersions(ctx, pool)
	if err != nil {
		return err
	}
	if current != latest {
		return fmt.Errorf("database schema version %d does not match latest migration version %d", current, latest)
	}
	if err := postgres.CheckSchema(ctx, pool); err != nil {
		return err
	}
	store := postgres.New(pool)
	pendingImports, err := store.ListPendingWorkspaceLegacyVolumes(ctx)
	if err != nil {
		return err
	}
	references, err := store.ListWorkspaceBlockReferences(ctx)
	if err != nil {
		return err
	}
	blocks, err := workspacefs.OpenExisting(cfg.WorkspaceStorage.RootDir)
	if err != nil {
		return fmt.Errorf("open workspace block storage: %w", err)
	}
	defer func() { _ = blocks.Close() }()
	var referencedBytes int64
	for _, reference := range references {
		if err := blocks.VerifyBlock(reference.WorkspaceID, reference.SHA256, reference.SizeBytes); err != nil {
			return fmt.Errorf("workspace %s block %s failed verification: %w", reference.WorkspaceID, reference.SHA256, err)
		}
		if reference.SizeBytes > int64(^uint64(0)>>1)-referencedBytes {
			return errors.New("workspace referenced block bytes overflow int64")
		}
		referencedBytes += reference.SizeBytes
	}
	stored, err := blocks.VerifyAll()
	if err != nil {
		return fmt.Errorf("verify workspace block store: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "workspace content verified: references=%d referenced_bytes=%d stored_blocks=%d stored_logical_bytes=%d stored_allocated_bytes=%d pending_legacy_imports=%d\n", len(references), referencedBytes, stored.BlockCount, stored.LogicalBytes, stored.AllocatedBytes, len(pendingImports)); err != nil {
		return fmt.Errorf("write workspace verification result: %w", err)
	}
	if len(pendingImports) > 0 {
		return fmt.Errorf("%d legacy workspace volume(s) are pending import; run portal workspace import-legacy --workspace <id>", len(pendingImports))
	}
	return nil
}

func reconcileWorkspaceBlockUsage(ctx context.Context, store *postgres.Store, blocks *workspacefs.Store, maxOwnerBytes int64) (retErr error) {
	lock, acquired, err := store.TryWorkspaceStorageReconciliationLock(ctx)
	if err != nil {
		return fmt.Errorf("lock workspace storage for reconciliation: %w", err)
	}
	if !acquired || lock == nil {
		return errors.New("workspace storage is busy; retry reconciliation when block operations finish")
	}
	defer func() {
		if err := lock.Release(ctx); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release workspace storage reconciliation lock: %w", err))
		}
	}()
	workspaceIDs, err := blocks.ListWorkspaceIDs()
	if err != nil {
		return fmt.Errorf("list workspace block directories: %w", err)
	}
	for _, workspaceID := range workspaceIDs {
		if err := blocks.RemoveStaleTempBlocks(workspaceID); err != nil {
			return fmt.Errorf("remove stale workspace upload files: %w", err)
		}
	}
	fsAllocations, err := blocks.ScanBlockAllocations()
	if err != nil {
		return fmt.Errorf("scan allocated workspace block bytes: %w", err)
	}
	allocations := make([]app.WorkspaceBlockAllocation, 0, len(fsAllocations))
	allocated := make(map[string]struct{}, len(fsAllocations))
	for _, item := range fsAllocations {
		allocations = append(allocations, app.WorkspaceBlockAllocation{
			WorkspaceID: item.WorkspaceID, Digest: item.Digest, AllocatedBytes: item.AllocatedBytes,
		})
		allocated[item.WorkspaceID+":"+item.Digest] = struct{}{}
	}
	targets, err := store.ListWorkspaceBlocksNeedingReservation(ctx)
	if err != nil {
		return fmt.Errorf("list active upload blocks needing reservations: %w", err)
	}
	reservations := make([]app.WorkspaceBlockReservation, 0, len(targets))
	reserved := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		key := target.WorkspaceID + ":" + target.Digest
		if _, exists := allocated[key]; exists {
			continue
		}
		if _, exists := reserved[key]; exists {
			continue
		}
		reservedBytes, err := blocks.ReservationBytes(target.SizeBytes)
		if err != nil {
			return fmt.Errorf("calculate recovered workspace block reservation: %w", err)
		}
		if reservedBytes <= 0 {
			return errors.New("recovered workspace block reservation must be positive")
		}
		reservations = append(reservations, app.WorkspaceBlockReservation{
			WorkspaceID: target.WorkspaceID, Digest: target.Digest, ReservedBytes: reservedBytes,
		})
		reserved[key] = struct{}{}
	}
	if err := store.ReconcileWorkspaceBlockUsage(ctx, allocations, reservations, maxOwnerBytes); err != nil {
		return fmt.Errorf("rebuild workspace physical usage ledger: %w", err)
	}
	return nil
}

func importLegacyWorkspace(ctx context.Context, configPath, workspaceID string, stdout io.Writer) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	pool, err := openPool(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	current, latest, err := schemaVersions(ctx, pool)
	if err != nil {
		return err
	}
	if current != latest {
		return fmt.Errorf("database schema version %d does not match latest migration version %d", current, latest)
	}
	if err := postgres.CheckSchema(ctx, pool); err != nil {
		return err
	}
	blocks, err := workspacefs.Open(cfg.WorkspaceStorage.RootDir)
	if err != nil {
		return fmt.Errorf("open workspace block storage: %w", err)
	}
	defer func() { _ = blocks.Close() }()
	store := postgres.New(pool)
	if err := reconcileWorkspaceBlockUsage(ctx, store, blocks, cfg.WorkspaceStorage.MaxOwnerBytes); err != nil {
		return fmt.Errorf("reconcile workspace physical usage before legacy import: %w", err)
	}
	volumeSource, err := newVolumeDeleter(cfg.Storage)
	if err != nil {
		return err
	}
	importer, err := app.NewWorkspaceLegacyImportService(store, volumeSource, blocks, cfg.WorkspaceStorage.MaxOwnerBytes)
	if err != nil {
		return err
	}
	imported, err := importer.Import(ctx, workspaceID)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "legacy workspace imported: workspace=%s revision=%d bytes=%d; source Docker volume retained\n", imported.ID, imported.Revision, imported.UsedBytes); err != nil {
		return fmt.Errorf("write legacy workspace import result: %w", err)
	}
	return nil
}

func schemaVersions(ctx context.Context, pool *pgxpool.Pool) (int64, int64, error) {
	db := stdlib.OpenDBFromPool(pool)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		_ = db.Close()
		return 0, 0, fmt.Errorf("initialize migration status: %w", err)
	}
	defer func() { _ = provider.Close() }()
	var versionTableExists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('goose_db_version') IS NOT NULL").Scan(&versionTableExists); err != nil {
		return 0, 0, fmt.Errorf("check migration version table: %w", err)
	}
	if !versionTableExists {
		sources := provider.ListSources()
		if len(sources) == 0 {
			return 0, 0, errors.New("no database migrations are embedded")
		}
		return 0, sources[len(sources)-1].Version, nil
	}
	current, latest, err := provider.GetVersions(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("read migration status: %w", err)
	}
	return current, latest, nil
}

func addUser(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("user add", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath, "path to portal.yaml")
	name := flags.String("name", "", "user name")
	roleValue := flags.String("role", string(identity.RoleAdmin), "user role: admin or user")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*name) == "" {
		return fmt.Errorf("usage: portal user add --name name [--role admin|user] [--config path]")
	}
	role := identity.Role(*roleValue)
	if role != identity.RoleAdmin && role != identity.RoleUser {
		return fmt.Errorf("role must be admin or user")
	}
	password, err := readPassword(stdin)
	if err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	pool, err := openPool(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := postgres.CheckSchema(ctx, pool); err != nil {
		return err
	}
	store := postgres.New(pool)
	auditService, err := app.NewAuditService(store)
	if err != nil {
		return err
	}
	users, err := app.NewUserService(store, store, auditService)
	if err != nil {
		return err
	}
	created, err := users.Create(ctx, identity.Principal{Kind: identity.KindSystem, Role: identity.RoleAdmin}, app.CreateUserCommand{
		Name: *name, Password: password, Role: role,
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "created user %s (%s) with role %s\n", created.Name, created.ID, created.Role)
	return err
}

func readPassword(reader io.Reader) (string, error) {
	if reader == nil {
		return "", errors.New("password must be provided on stdin")
	}
	value, err := io.ReadAll(io.LimitReader(bufio.NewReader(reader), 75))
	if err != nil {
		return "", fmt.Errorf("read password from stdin: %w", err)
	}
	value = []byte(strings.TrimSuffix(strings.TrimSuffix(string(value), "\n"), "\r"))
	if len(value) == 0 || len(value) > 72 {
		return "", errors.New("password from stdin must contain 1 to 72 bytes")
	}
	return string(value), nil
}

func openPool(ctx context.Context, cfg config.Database) (*pgxpool.Pool, error) {
	if strings.TrimSpace(cfg.DSN) == "" {
		return nil, errors.New("config.database.dsn is required")
	}
	poolConfig, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse config.database.dsn: %w", err)
	}
	poolConfig.MaxConns = cfg.MaxConns
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}

func newLogger(cfg config.Log) (*zap.Logger, error) {
	var level zapcore.Level
	switch cfg.Level {
	case "debug":
		level = zapcore.DebugLevel
	case "info":
		level = zapcore.InfoLevel
	case "warn":
		level = zapcore.WarnLevel
	case "error":
		level = zapcore.ErrorLevel
	default:
		return nil, fmt.Errorf("config.log.level is invalid")
	}
	options := zap.NewProductionConfig()
	options.Level = zap.NewAtomicLevelAt(level)
	options.Encoding = cfg.Format
	return options.Build()
}

func readKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key file must contain exactly 32 bytes")
	}
	return key, nil
}

func loginLimit(value string) int {
	count, err := strconv.Atoi(strings.TrimSuffix(value, "/min"))
	if err != nil {
		return 0
	}
	return count
}

func newVolumeDeleter(cfg config.Storage) (*storage.Docker, error) {
	if cfg.Driver != "docker" {
		return nil, fmt.Errorf("storage.driver %q is unsupported", cfg.Driver)
	}
	return storage.NewDocker(cfg.DockerHost, cfg.DockerDataDir, cfg.MinFreeBytes)
}

func requireLoopbackMetrics(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" {
		return errors.New("listen.metrics must bind to 127.0.0.1")
	}
	return nil
}

func metricsHandler(registry *prometheus.Registry) http.Handler {
	metrics := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		metrics.ServeHTTP(w, r)
	})
}

func registerCounter(reg prometheus.Registerer, opts prometheus.CounterOpts) (prometheus.Counter, error) {
	collector := prometheus.NewCounter(opts)
	if err := reg.Register(collector); err != nil {
		var already prometheus.AlreadyRegisteredError
		if !errors.As(err, &already) {
			return nil, err
		}
		counter, ok := already.ExistingCollector.(prometheus.Counter)
		if !ok {
			return nil, fmt.Errorf("metric %s already has an incompatible collector", opts.Name)
		}
		return counter, nil
	}
	return collector, nil
}

func registerHistogram(reg prometheus.Registerer, opts prometheus.HistogramOpts, labels ...string) (*prometheus.HistogramVec, error) {
	collector := prometheus.NewHistogramVec(opts, labels)
	if err := reg.Register(collector); err != nil {
		var already prometheus.AlreadyRegisteredError
		if !errors.As(err, &already) {
			return nil, err
		}
		histogram, ok := already.ExistingCollector.(*prometheus.HistogramVec)
		if !ok {
			return nil, fmt.Errorf("metric %s already has an incompatible collector", opts.Name)
		}
		return histogram, nil
	}
	return collector, nil
}

func runServers(ctx context.Context, logger *zap.Logger, shutdownTimeout time.Duration, background *worker.Worker, taskWorker *worker.TaskWorker, taskStopWorker *worker.TaskStopWorker, scheduleWorker *worker.ScheduleWorker, notificationWorker *worker.NotificationWorker, runtime *app.Runtime, gatewayHandler http.Handler, servers ...*http.Server) error {
	listeners := make([]net.Listener, 0, len(servers))
	for _, server := range servers {
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			for _, active := range listeners {
				_ = active.Close()
			}
			return fmt.Errorf("listen on %s: %w", server.Addr, err)
		}
		listeners = append(listeners, listener)
	}
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- background.Run(workerCtx) }()
	taskWorkerDone := make(chan error, 1)
	go func() { taskWorkerDone <- taskWorker.Run(workerCtx) }()
	taskStopWorkerDone := make(chan error, 1)
	go func() { taskStopWorkerDone <- taskStopWorker.Run(workerCtx) }()
	scheduleWorkerDone := make(chan error, 1)
	go func() { scheduleWorkerDone <- scheduleWorker.Run(workerCtx) }()
	notificationWorkerDone := make(chan error, 1)
	go func() { notificationWorkerDone <- notificationWorker.Run(workerCtx) }()
	serverErrors := make(chan error, len(servers))
	var serverWG sync.WaitGroup
	for index, server := range servers {
		server := server
		listener := listeners[index]
		serverWG.Add(1)
		go func() {
			defer serverWG.Done()
			err := server.Serve(listener)
			if !errors.Is(err, http.ErrServerClosed) {
				serverErrors <- fmt.Errorf("listen on %s: %w", server.Addr, err)
			}
		}()
	}
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-serverErrors:
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	for _, server := range servers {
		if err := server.Shutdown(shutdownCtx); err != nil {
			if runErr == nil {
				runErr = fmt.Errorf("shutdown server %s: %w", server.Addr, err)
			}
			_ = server.Close()
		}
	}
	if closer, ok := gatewayHandler.(interface{ CloseConnections() }); ok {
		closer.CloseConnections()
	}
	serverWG.Wait()
	cancelWorker()
	if err := <-workerDone; err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
		runErr = fmt.Errorf("stop worker: %w", err)
	}
	if err := <-taskWorkerDone; err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
		runErr = fmt.Errorf("stop task worker: %w", err)
	}
	if err := <-taskStopWorkerDone; err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
		runErr = fmt.Errorf("stop task action worker: %w", err)
	}
	if err := <-scheduleWorkerDone; err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
		runErr = fmt.Errorf("stop schedule worker: %w", err)
	}
	if err := <-notificationWorkerDone; err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
		runErr = fmt.Errorf("stop notification worker: %w", err)
	}
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 10*time.Second)
	if err := runtime.FlushActivity(flushCtx); err != nil {
		logger.Error("flush activity during shutdown failed", zap.Error(err))
		if runErr == nil {
			runErr = fmt.Errorf("flush activity during shutdown: %w", err)
		}
	}
	cancelFlush()
	logger.Info("portal stopped")
	return runErr
}
