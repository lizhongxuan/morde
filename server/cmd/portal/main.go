package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
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
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/gateway"
	"morde/server/internal/httpserver"
	"morde/server/internal/mcp"
	"morde/server/internal/model"
	"morde/server/internal/orchestrator"
	"morde/server/internal/storage"
	"morde/server/internal/storage/sitefs"
	"morde/server/internal/storage/workspacecurrent"
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
	return fmt.Errorf("usage: portal serve|migrate up|migrate status|workspace verify|workspace import-legacy")
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
	if configuredRoot := os.Getenv("CH_WORKSPACE_SYNC_DATA_ROOT_DIR"); configuredRoot != "" && configuredRoot != cfg.WorkspaceSyncNode.DataRoot {
		return fmt.Errorf("config.workspace_sync_node.data_root must match CH_WORKSPACE_SYNC_DATA_ROOT_DIR")
	}
	if configuredRoot := os.Getenv("CH_WORKSPACE_SYNC_CURRENT_FILES_DIR"); configuredRoot != "" && configuredRoot != cfg.WorkspaceSyncNode.WorkspaceRoot {
		return fmt.Errorf("config.workspace_sync_node.workspace_root must match CH_WORKSPACE_SYNC_CURRENT_FILES_DIR")
	}
	if cfg.WorkspaceSyncNode.Enabled {
		if err := storage.CheckWorkspaceSyncStorageIsolation(
			cfg.WorkspaceSyncNode.DataRoot, cfg.WorkspaceStorage.RootDir, cfg.WorkspaceSyncNode.WorkspaceRoot, cfg.Storage.DockerDataDir, cfg.SiteStorage.RootDir,
			cfg.WorkspaceSyncNode.StateRoot, cfg.WorkspaceSyncNode.RelayStateRoot, cfg.WorkspaceSyncNode.ProtectedStorageRoots,
		); err != nil {
			return fmt.Errorf("validate workspace sync filesystem isolation: %w", err)
		}
	} else if err := storage.CheckWorkspaceCurrentStorageIsolation(
		cfg.WorkspaceSyncNode.DataRoot, cfg.WorkspaceStorage.RootDir, cfg.WorkspaceSyncNode.WorkspaceRoot, cfg.Storage.DockerDataDir, cfg.SiteStorage.RootDir,
		cfg.WorkspaceSyncNode.ProtectedStorageRoots,
	); err != nil {
		return fmt.Errorf("validate Workspace current-filesystem isolation: %w", err)
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
	serviceInstanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		return fmt.Errorf("load service instance identity: %w", err)
	}
	if err := app.EnsureInitialAdmin(ctx, store); err != nil {
		return fmt.Errorf("initialize administrator: %w", err)
	}
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
		items := make([]string, 0, len(pendingImports))
		for _, pending := range pendingImports {
			item := pending.WorkspaceID
			if pending.ImportError != "" {
				item += " (needs cleanup: " + pending.ImportError + ")"
			}
			items = append(items, item)
		}
		return fmt.Errorf("legacy workspace volumes must be imported or cleaned up before serving (pending workspaces: %s); run portal workspace import-legacy --workspace <id>", strings.Join(items, ", "))
	}
	workspaceCurrent, err := workspacecurrent.Open(cfg.WorkspaceSyncNode.WorkspaceRoot)
	if err != nil {
		return fmt.Errorf("open Workspace current-file storage: %w", err)
	}
	defer func() { _ = workspaceCurrent.Close() }()
	workspaceSync, err := app.NewWorkspaceSyncServiceWithOptions(
		store, workspaceBlocks, workspace.MaxSyncManifestBytes, cfg.WorkspaceStorage.MaxOwnerBytes,
		app.WorkspaceSyncOptions{ProjectUploadEnabled: cfg.WorkspaceSyncNode.ProjectUploadEnabled},
	)
	if err != nil {
		return err
	}
	if err := workspaceSync.SetWorkspaceCurrentMaterializer(func(ctx context.Context, ownerID, workspaceID string, revision int64,
		files []workspace.File, emptyDirectories []string, directories []workspace.Directory) error {
		return workspaceCurrent.Materialize(ctx, ownerID, workspaceID, revision, files, emptyDirectories, directories, workspaceBlocks)
	}); err != nil {
		return fmt.Errorf("configure Workspace current-file materializer: %w", err)
	}
	if err := workspaceSync.SetWorkspaceCurrentRevisionReader(workspaceCurrent.CurrentRevision); err != nil {
		return fmt.Errorf("configure Workspace current revision reader: %w", err)
	}
	if err := workspaceSync.SetWorkspaceCurrentManifestVerifier(workspaceCurrent.MatchesManifest); err != nil {
		return fmt.Errorf("configure Workspace current manifest verifier: %w", err)
	}
	workspaceOwnershipTransfers, err := app.NewWorkspaceTransferService(store, cfg.WorkspaceStorage.MaxOwnerBytes)
	if err != nil {
		return fmt.Errorf("initialize Workspace ownership transfer service: %w", err)
	}
	if err := workspaceOwnershipTransfers.SetWorkspaceOwnershipTransferProjection(app.WorkspaceOwnershipTransferProjection{
		PrepareRecipient: workspaceSync.PrepareWorkspaceOwnershipTransferProjection,
		DiscardRecipient: func(ctx context.Context, transfer workspace.OwnershipTransfer) error {
			return workspaceCurrent.Delete(ctx, transfer.ToOwnerID, transfer.WorkspaceID)
		},
		RetireFormer: func(ctx context.Context, transfer workspace.OwnershipTransfer) error {
			return workspaceCurrent.Delete(ctx, transfer.FromOwnerID, transfer.WorkspaceID)
		},
	}); err != nil {
		return fmt.Errorf("configure Workspace ownership transfer projection: %w", err)
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
	modelClient, err := model.NewResponsesClient(cfg.Models.AllowedHosts, cfg.Models.ProxyURL)
	if err != nil {
		return fmt.Errorf("initialize model client: %w", err)
	}
	modelConnections, err := app.NewModelConnectionService(store, sealer, modelClient, cfg.Models.AllowedHosts)
	if err != nil {
		return fmt.Errorf("initialize model connections: %w", err)
	}
	projects, err := app.NewProjectService(store)
	if err != nil {
		return fmt.Errorf("initialize project service: %w", err)
	}
	projectMCPConnectors, err := app.NewProjectMCPConnectorService(store, projects)
	if err != nil {
		return fmt.Errorf("initialize project MCP connectors: %w", err)
	}
	projectRates := make([]project.RateSnapshot, 0, len(cfg.Models.ProjectRates))
	for _, rate := range cfg.Models.ProjectRates {
		projectRates = append(projectRates, rate.Snapshot())
	}
	configuredProjectRates, err := app.NewConfiguredProjectRates(projectRates)
	if err != nil {
		return fmt.Errorf("initialize project model rates: %w", err)
	}
	projectBudgets, err := app.NewProjectBudgetService(store, projects, configuredProjectRates)
	if err != nil {
		return fmt.Errorf("initialize project budget service: %w", err)
	}
	var connectorOAuthRedirect string
	if cfg.Domains.Scheme == "https" {
		connectorOAuthRedirect = "https://" + cfg.Domains.Portal + "/api/mcp/oauth/callback"
	}
	mcpConnectors, err := app.NewMCPConnectorService(store, sealer, connectorOAuthRedirect)
	if err != nil {
		return fmt.Errorf("initialize MCP connectors: %w", err)
	}
	modelGateway, err := app.NewModelGatewayService(store, modelConnections, modelClient, projectBudgets)
	if err != nil {
		return fmt.Errorf("initialize model gateway: %w", err)
	}
	modelGateway.SetGPT6LunaMaxReasoning(cfg.Models.GPT6LunaMaxReasoning)
	if err := modelGateway.SetModelResponseSealer(sealer); err != nil {
		return fmt.Errorf("initialize model response encryption: %w", err)
	}
	modelUsageAdmin, err := app.NewModelUsageAdminService(store)
	if err != nil {
		return fmt.Errorf("initialize model usage admin: %w", err)
	}
	adminConsole, err := app.NewAdminConsoleService(store)
	if err != nil {
		return fmt.Errorf("initialize admin console: %w", err)
	}
	adminControls, err := app.NewAdminControlsService(store)
	if err != nil {
		return fmt.Errorf("initialize admin controls: %w", err)
	}
	adminOperations, err := app.NewAdminOperationsService(store)
	if err != nil {
		return fmt.Errorf("initialize admin operations: %w", err)
	}
	adminModelBudgetDefaults, err := app.NewAdminModelBudgetDefaultService(store)
	if err != nil {
		return fmt.Errorf("initialize admin model budget defaults: %w", err)
	}
	adminModelDefaults, err := app.NewAdminModelDefaultService(store)
	if err != nil {
		return fmt.Errorf("initialize admin model defaults: %w", err)
	}
	recordConsents, err := app.NewRecordConsentService(store)
	if err != nil {
		return fmt.Errorf("initialize record consents: %w", err)
	}
	recordAccess, err := app.NewRecordAccessService(store)
	if err != nil {
		return fmt.Errorf("initialize record access: %w", err)
	}
	recordCapture, err := app.NewRecordCaptureService(store)
	if err != nil {
		return fmt.Errorf("initialize record capture: %w", err)
	}
	adminQualityActivity, err := app.NewAdminQualityActivityService(store)
	if err != nil {
		return fmt.Errorf("initialize admin quality and activity: %w", err)
	}
	aioClient := aio.New(nil)
	volumes, err := newVolumeDeleter(cfg.Storage)
	if err != nil {
		return err
	}
	hostUsageService, err := app.NewHostUsageService(store, store, volumes, projects, cfg.Instance, nil)
	if err != nil {
		return fmt.Errorf("initialize Host usage service: %w", err)
	}
	workspaceVolumes, err := app.NewWorkspaceVolumeService(store, volumes, serviceInstanceID)
	if err != nil {
		return fmt.Errorf("initialize Workspace volume service: %w", err)
	}
	workspaceCopies, err := app.NewWorkspaceCopyService(store, workspaceSync, workspaceBlocks, volumes, volumes,
		serviceInstanceID, cfg.WorkspaceStorage.MaxOwnerBytes, cfg.WorkspaceStorage.MaxWorkspaceBytes)
	if err != nil {
		return fmt.Errorf("initialize Workspace copy service: %w", err)
	}
	if err := workspaceCopies.SetCurrentMaterializer(func(ctx context.Context, ownerID, workspaceID string, revision int64,
		files []workspace.File, emptyDirectories []string, directories []workspace.Directory) error {
		return workspaceCurrent.Materialize(ctx, ownerID, workspaceID, revision, files, emptyDirectories, directories, workspaceBlocks)
	}); err != nil {
		return fmt.Errorf("configure Workspace copy materializer: %w", err)
	}
	workspaceShares, err := app.NewWorkspaceShareService(store, workspaceCopies)
	if err != nil {
		return fmt.Errorf("initialize Workspace share service: %w", err)
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
	workspaces, err := app.NewWorkspaceService(
		store, workspaceBlocks, workspaceCurrent, workspaceCapacity, countedAudit,
		cfg.WorkspaceStorage.MaxWorkspaceBytes,
	)
	if err != nil {
		return err
	}
	if err := workspaces.SetWorkspaceCurrentInitializer(workspaceSync.InitializeWorkspaceCurrent); err != nil {
		return fmt.Errorf("configure Workspace current initializer: %w", err)
	}
	hosts, err := app.NewHostService(app.HostServiceOptions{
		Hosts: store, Users: store, Workspaces: store, DefaultWorkspaces: workspaces, WorkspaceVolumes: workspaceVolumes, Tokens: store,
		Sandboxes: router, Probe: aioClient, Runtime: runtimeControl, Audit: auditService, CreateLocks: store, Sealer: sealer, Volumes: volumes, DiskCapacity: volumes,
		Templates: templates, DefaultTemplateID: cfg.Templates.Default, Capacity: app.Capacity{CPUMilli: cfg.Capacity.CPUMilli, MemMiB: cfg.Capacity.MemMiB},
		Instance: cfg.Instance, Scheme: cfg.Domains.Scheme, PortalURL: cfg.Domains.Scheme + "://" + cfg.Domains.Portal,
		RuntimePortalURL: cfg.Hosts.RuntimePortalURL, HostSuffix: cfg.Domains.HostSuffix,
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
	hostCopies, err := app.NewHostCopyService(hosts, workspaceCopies)
	if err != nil {
		return fmt.Errorf("initialize Host copy service: %w", err)
	}
	adminHostResources, err := app.NewAdminHostResourcesService(store, app.Capacity{CPUMilli: cfg.Capacity.CPUMilli, MemMiB: cfg.Capacity.MemMiB}, router)
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
	var workspaceSyncNodeEnsurer app.WorkspaceSyncNodeEnsurer
	var workspaceSyncNodeOwnerReconciler app.WorkspaceSyncNodeOwnerReconciler
	var workspaceSyncNodeProvisioner *worker.WorkspaceSyncNodeProvisioner
	if cfg.WorkspaceSyncNode.Enabled {
		quotaService, err := app.NewWorkspaceSyncQuotaService(workspaceSync, workspaceCurrent, workspaceCapacity)
		if err != nil {
			return fmt.Errorf("initialize Workspace sync quota service: %w", err)
		}
		quotaSockets, err := worker.NewWorkspaceSyncNodeQuotaSockets(cfg.WorkspaceSyncNode.QuotaSocketRoot, quotaService)
		if err != nil {
			return fmt.Errorf("initialize Workspace sync quota sockets: %w", err)
		}
		defer func() { _ = quotaSockets.Close() }()
		dockerSocket, err := workspaceSyncDockerSocketPath(cfg.Storage.DockerHost)
		if err != nil {
			return err
		}
		nodeSupervisor, err := worker.NewDockerWorkspaceSyncNodeSupervisor(worker.DockerWorkspaceSyncNodeSupervisorOptions{
			ImageDigest: cfg.WorkspaceSyncNode.ImageDigest, RelayImageDigest: cfg.WorkspaceSyncNode.RelayImageDigest,
			RelayPublicAddress: cfg.WorkspaceSyncNode.RelayPublicAddress, RelayBindAddress: cfg.WorkspaceSyncNode.RelayBindAddress,
			RelayToken:   cfg.WorkspaceSyncNode.RelayToken,
			DockerSocket: dockerSocket, RelayStateRoot: cfg.WorkspaceSyncNode.RelayStateRoot, StateRoot: cfg.WorkspaceSyncNode.StateRoot,
			WorkspaceRoot: cfg.WorkspaceSyncNode.WorkspaceRoot, QuotaSocketRoot: cfg.WorkspaceSyncNode.QuotaSocketRoot,
			RelayContainerName: cfg.WorkspaceSyncNode.RelayContainerName,
			Resources:          worker.DockerWorkspaceSyncNodeResourceLimits{MemoryBytes: 256 << 20, NanoCPUs: 250_000_000, PIDsLimit: 64},
		})
		if err != nil {
			return fmt.Errorf("initialize Workspace sync node supervisor: %w", err)
		}
		startGuard := worker.WorkspaceSyncNodeStartGuardFunc(func(ctx context.Context, ownerID string) error {
			owners, err := workspaceSync.ListActiveWorkspaceSyncReceiveOwners(ctx)
			if err != nil {
				return err
			}
			for _, activeOwner := range owners {
				if activeOwner == ownerID {
					return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has an unresolved file receive")
				}
			}
			return nil
		})
		workspaceSyncNodeProvisioner, err = worker.NewWorkspaceSyncNodeProvisioner(worker.WorkspaceSyncNodeProvisionerOptions{
			Supervisor: nodeSupervisor, Store: store, RelayGate: worker.NewCommandWorkspaceSyncRelayGate(),
			QuotaSockets: quotaSockets, StartGuard: startGuard, DataKey: dataKey,
		})
		if err != nil {
			return fmt.Errorf("initialize Workspace sync node provisioner: %w", err)
		}
		if err := workspaceSyncNodeProvisioner.StopAllWorkspaceSyncNodesForStartup(ctx); err != nil {
			return fmt.Errorf("stop Workspace sync nodes before startup recovery: %w", err)
		}
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = workspaceSyncNodeProvisioner.StopAllWorkspaceSyncNodesForStartup(stopCtx)
		}()
		workspaceSyncNodeEnsurer = workspaceSyncNodeProvisioner
		workspaceSyncNodeOwnerReconciler = workspaceSyncNodeProvisioner
		if err := workspaceSync.SetWorkspaceSyncNodeObserver(workspaceSyncNodeProvisioner); err != nil {
			return fmt.Errorf("configure Workspace sync node observer: %w", err)
		}
	}
	hubService, err := app.NewHubService(store)
	if err != nil {
		return err
	}
	defer hubService.Close()
	credentials, err := app.NewCredentialServiceWithHub(store, store, store, countedAudit, app.CredentialOptions{
		SessionTTL: cfg.Security.SessionTTL, TicketTTL: cfg.Security.TicketTTL,
		LoginLimit: loginLimit(cfg.Security.LoginRateLimit), LoginWindow: time.Minute,
	}, hosts, hubService, workspaceSyncNodeOwnerReconciler)
	if err != nil {
		return err
	}
	tasks, err := app.NewTaskService(store, credentials, modelConnections, projects)
	if err != nil {
		return fmt.Errorf("initialize task service: %w", err)
	}
	schedules, err := app.NewScheduleService(store, credentials, modelConnections)
	if err != nil {
		return fmt.Errorf("initialize schedule service: %w", err)
	}
	conversations, err := app.NewConversationService(store, credentials)
	if err != nil {
		return fmt.Errorf("initialize conversation service: %w", err)
	}
	executionAdapters := cloudDSHAdaptersForRuntime(cfg.CloudDSH.Enabled, tasks, schedules, schedules)
	projectConversations, err := app.NewProjectConversationService(store, projects, projectConversationTaskAppender{tasks: executionAdapters.tasks})
	if err != nil {
		return fmt.Errorf("initialize project conversation service: %w", err)
	}
	projectWorkItems, err := app.NewProjectWorkItemService(store, projects, projectWorkItemTaskLinkAuthorizer{tasks: tasks},
		projectWorkItemDeliveryAuthorizer{tasks: tasks, projects: projects, savedProof: store})
	if err != nil {
		return fmt.Errorf("initialize project work item service: %w", err)
	}
	adminWorkspaces, err := app.NewAdminWorkspaceService(store)
	if err != nil {
		return err
	}
	if err := workspaceSync.ReconcilePendingWorkspaceCurrentMaterializations(ctx); err != nil {
		return fmt.Errorf("reconcile Workspace current-file materializations before serving: %w", err)
	}
	users, err := app.NewUserServiceWithHub(store, store, hubService, workspaceSyncNodeOwnerReconciler)
	if err != nil {
		return err
	}
	workspaceSyncDevices, err := app.NewWorkspaceSyncDeviceService(store, workspaceSyncNodeEnsurer)
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
	tokens, err := app.NewTokenService(store, countedAudit, 30*24*time.Hour, 365*24*time.Hour)
	if err != nil {
		return err
	}
	clientLogin, err := app.NewClientLoginService(store, credentials, workspaceSyncNodeOwnerReconciler, hubService)
	if err != nil {
		return err
	}
	deviceService, err := app.NewDeviceService(clientLogin, store, hubService)
	if err != nil {
		return err
	}
	personalDataService, err := app.NewPersonalMutationService(store)
	if err != nil {
		return err
	}
	goalService, err := app.NewGoalService(personalDataService, store)
	if err != nil {
		return fmt.Errorf("initialize goal service: %w", err)
	}
	personalSyncService, err := app.NewPersonalSyncService(store, dataKey)
	if err != nil {
		return err
	}
	personalDataQuotaService, err := app.NewPersonalDataQuotaService(store)
	if err != nil {
		return fmt.Errorf("initialize personal data quota service: %w", err)
	}
	personalDataQuotaUserService, err := app.NewPersonalDataQuotaUserService(store)
	if err != nil {
		return fmt.Errorf("initialize personal data quota user service: %w", err)
	}
	personalAttachmentService, err := app.NewPersonalAttachmentService(store, personalDataQuotaService)
	if err != nil {
		return fmt.Errorf("initialize personal attachment service: %w", err)
	}
	projectConversationAttachments, err := app.NewProjectConversationAttachmentService(store)
	if err != nil {
		return fmt.Errorf("initialize project conversation attachment service: %w", err)
	}
	personalHistoryImportService, err := app.NewPersonalHistoryImportService(store)
	if err != nil {
		return fmt.Errorf("initialize personal history import service: %w", err)
	}
	notificationService, err := app.NewNotificationService(store)
	if err != nil {
		return err
	}
	onboardingService, err := app.NewOnboardingService(store, hosts, modelConnections, cfg.Templates.Default)
	if err != nil {
		return fmt.Errorf("initialize onboarding service: %w", err)
	}
	controlService, err := app.NewControlService(store, credentials, store, store, store, runtimeControl, auditService, store)
	if err != nil {
		return err
	}
	workerSuffix, err := domain.NewOperationID()
	if err != nil {
		return fmt.Errorf("generate task worker identity: %w", err)
	}
	recordWorker, err := worker.NewRecordCaptureWorker(worker.RecordCaptureWorkerOptions{
		Capture: recordCapture, Access: recordAccess, WorkerID: workerSuffix + "-record",
		PollInterval: time.Second, BatchSize: 50, Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("initialize record worker: %w", err)
	}
	resultWorker, err := worker.NewConversationResultReviewWorker(store, 100*time.Millisecond, logger)
	if err != nil {
		return fmt.Errorf("initialize conversation result worker: %w", err)
	}
	var executionControlService api.ExecutionControlService
	var taskWorker *worker.TaskWorker
	var routeWorker *worker.ConversationRouteWorker
	var taskAgentStopReconciler worker.TaskAgentStopReconciler
	if cfg.CloudDSH.Enabled {
		executionControlService, err = app.NewExecutionControlService(store, countedAudit)
		if err != nil {
			return err
		}
		cloudDSHWorkers, workerErr := newCloudDSHTaskWorker(cfg.CloudDSH, store, credentials, runtimeControl, workspaceSync,
			modelGateway, sealer, mcpConnectors, recordCapture, personalDataService, personalSyncService,
			personalAttachmentService, goalService, schedules, projects,
			hubService, workerSuffix, serviceInstanceID, dataKey, logger)
		if workerErr != nil {
			return fmt.Errorf("initialize cloud dsh task worker: %w", workerErr)
		}
		taskWorker = cloudDSHWorkers.taskWorker
		taskAgentStopReconciler = cloudDSHWorkers.accountRuntimes
		coordinatorCheckpoints, err := app.NewConversationCoordinatorCheckpointService(store, sealer)
		if err != nil {
			return err
		}
		coordinator, err := worker.NewCloudDSHConversationCoordinator(worker.CloudDSHConversationCoordinatorOptions{
			AccountRuntimes: cloudDSHWorkers.accountRuntimes, Models: modelGateway, Checkpoints: coordinatorCheckpoints, SummaryStore: store,
		})
		if err != nil {
			return err
		}
		routeWorker, err = worker.NewConversationRouteWorker(worker.ConversationRouteWorkerOptions{
			Queue: store, Actions: store, Models: coordinator, WorkerID: workerSuffix + "-route",
			SummaryStore: store, SummaryInputs: store, SummarySealer: sealer,
			LeaseDuration: 2 * time.Minute, PollInterval: 500 * time.Millisecond,
			MaxConcurrent: min(cfg.CloudDSH.MaxConcurrentTasks, 16), Logger: logger,
		})
		if err != nil {
			return fmt.Errorf("initialize conversation routing worker: %w", err)
		}
	}
	mcpRuntimeTools, err := app.NewMCPRuntimeToolExecutor(store, mcpHosts, credentials, store, runtimeControl)
	if err != nil {
		return err
	}
	taskStopWorker, err := worker.NewTaskStopWorker(worker.TaskStopWorkerOptions{
		Store: store, Executor: runtimeControl, WorkerID: workerSuffix + "-stop",
		LeaseDuration: 30 * time.Second, PollInterval: 500 * time.Millisecond,
		OperationTimeout: 10 * time.Second, RetryAfter: time.Second, MCPRuntimeStopper: mcpRuntimeTools,
		TaskAgentStopper: taskAgentStopReconciler, Logger: logger,
	})
	if err != nil {
		return err
	}
	mcpHandler, err := mcp.New(mcp.Deps{
		Config: cfg.MCP, Domains: cfg.Domains, Hosts: hosts, Applications: mcpRuntimeTools, RuntimeTools: mcpRuntimeTools,
		Auth: credentials, Audit: auditService, Logger: logger, Metrics: registry,
		PreviewPortMin: cfg.Gateway.PreviewPortMin, PreviewPortDeny: cfg.Gateway.PreviewPortDeny,
	})
	if err != nil {
		return err
	}
	embedTicketExchanger, ok := hostSharing.(app.HostEmbedTicketExchanger)
	if !ok {
		return fmt.Errorf("host embed ticket exchange is unavailable")
	}
	gatewayHandler, err := gateway.New(gateway.Deps{
		Config: cfg.Gateway, Domains: cfg.Domains, Security: cfg.Security,
		Hosts: hosts, HostSharing: hostSharing, HostEmbedTickets: embedTicketExchanger, ServiceInstanceID: serviceInstanceID,
		WorkspaceFiles: workspaceSync, TaskObservation: tasks, Control: controlService, Runtime: runtime, ActivityLeases: store, InputPermits: store,
		TerminalSessions: store, TerminalAIO: aioClient, Auth: credentials, Audit: auditService,
		Logger: logger, Metrics: registry,
	})
	if err != nil {
		return err
	}
	admin := adminAdapter{users: users, audit: auditService, capacity: hosts, workspaceSyncReconciler: workspaceSyncNodeOwnerReconciler}
	apiHandler, err := api.New(api.Deps{
		Hosts: hosts, HostSharing: hostSharing, HostReplicas: hostReplicas, HostUsage: hostUsageService, TLSAuthorizer: hosts, Verifier: credentials, Auth: authAdapter{service: credentials, users: users},
		WorkspaceOps: workspaces, WorkspaceSync: workspaceSync, WorkspaceSyncDevices: workspaceSyncDevices, WorkspaceDiff: workspaceSync,
		WorkspaceCopies: workspaceCopies, HostCopies: hostCopies, WorkspaceTransfers: workspaceOwnershipTransfers, WorkspaceShares: workspaceShares,
		Sites: siteService, Templates: templateAdapter{values: templates},
		Tokens: tokenAdapter{service: tokens}, ClientLogin: clientLogin,
		Notifications: notificationService,
		Control:       controlService, ExecutionControl: executionControlService, Tasks: executionAdapters.tasks, TaskFileFetch: tasks,
		TaskObservationTickets: tasks, Conversations: conversations,
		ProjectConversations:           projectConversations,
		ProjectConversationAttachments: projectConversationAttachments,
		ProjectWorkItems:               projectWorkItems,
		ProjectBudget:                  projectBudgets,
		ProjectMCPConnectors:           projectMCPConnectors,
		ProjectBudgetExecutionReady:    len(cfg.Models.ProjectRates) > 0,
		ConversationAutoRoutingReady: routeWorker != nil && taskWorker != nil && executionControlService != nil &&
			executionAdapters.tasks != nil && modelGateway != nil,
		RuntimeOperationLedger: store,
		Projects:               projects, ProjectContent: projects,
		ModelConnections: modelConnections, MCPConnectors: mcpConnectors, ModelGateway: modelGateway, ModelUsageAdmin: modelUsageAdmin,
		Hub: hubService, Devices: deviceService, PersonalData: personalDataService, PersonalSync: personalSyncService,
		Goals: goalService, Schedules: executionAdapters.schedules,
		PersonalDataQuota: personalDataQuotaService, PersonalDataQuotaUser: personalDataQuotaUserService,
		PersonalAttachments: personalAttachmentService, PersonalHistoryImport: personalHistoryImportService,
		Onboarding: onboardingService, Admin: admin, AdminConsole: adminConsole, AdminQualityActivity: adminQualityActivity, AdminControls: adminControls,
		AdminOperations:          adminOperations,
		AdminModelBudgetDefaults: adminModelBudgetDefaults,
		AdminModelDefaults:       adminModelDefaults,
		RecordConsents:           recordConsents, RecordAccess: recordAccess, RecordCaptureStatus: recordCapture,
		AdminHostResources: adminHostResources, AdminHostRecovery: hosts, AdminWorkspaces: adminWorkspaces,
		AdminAudit: auditService,
		Readiness:  readiness{pool: pool, clients: router, profiles: cfg.Orchestrator.Profiles, instance: cfg.Instance}, MCP: mcpHandler,
		MCPDisabled: !cfg.MCP.Enabled,
	}, api.Options{
		Logger: logger, Metrics: registry, ServiceInstanceID: serviceInstanceID, APIMaxBodyBytes: cfg.HTTP.APIMaxBodyBytes,
		MCPMaxBodyBytes: cfg.HTTP.MCPMaxBodyBytes, CookieSecure: cfg.Domains.CookieSecure,
		SessionTTL: cfg.Security.SessionTTL,
		HostScheme: cfg.Domains.Scheme, PortalDomain: cfg.Domains.Portal, HostSuffix: cfg.Domains.HostSuffix,
		PreviewPortMin: cfg.Gateway.PreviewPortMin, PreviewPortDeny: cfg.Gateway.PreviewPortDeny,
		PortalFiles: os.DirFS(cfg.PortalWeb.Dir),
	})
	if err != nil {
		return err
	}
	workerApp, err := app.NewWorkerApp(hosts, runtime, store, store, store, cfg.Reconciler.RenewBefore)
	if err != nil {
		return err
	}
	workspaceBlockGC, err := app.NewWorkspaceBlockGarbageCollector(store, workspaceBlocks)
	if err != nil {
		return fmt.Errorf("initialize workspace block garbage collection: %w", err)
	}
	background, err := worker.New(worker.Options{
		Application: workerApplicationAdapter{WorkerApp: workerApp, workspaceBlockGC: workspaceBlockGC,
			workspaceSyncNodeProvisioner: workspaceSyncNodeProvisioner, hostUsage: hostUsageService, logger: logger}, CreateQueue: queue, Interval: cfg.Reconciler.Interval,
		ActivityFlushInterval: cfg.Reconciler.ActivityFlushInterval, HostUsageInterval: worker.HostUsageSampleInterval,
		Logger: logger, Registerer: registry,
	})
	if err != nil {
		return err
	}
	notificationWorker, err := worker.NewNotificationWorker(worker.NotificationWorkerOptions{
		Service: notificationService, PollInterval: time.Second, BatchSize: 100, Logger: logger,
	})
	if err != nil {
		return err
	}
	var scheduleWorker *worker.ScheduleWorker
	if executionAdapters.scheduleProcessor != nil {
		scheduleWorker, err = worker.NewScheduleWorker(worker.ScheduleWorkerOptions{
			Service: executionAdapters.scheduleProcessor, PollInterval: time.Second, BatchSize: 100, Logger: logger,
		})
		if err != nil {
			return err
		}
	}
	goalAgentTriggerService, err := app.NewGoalAgentTriggerService(store, store, schedules, workerSuffix+"-goal-trigger", 2*time.Minute)
	if err != nil {
		return fmt.Errorf("initialize Goal agent trigger service: %w", err)
	}
	goalAgentTriggerWorker, err := worker.NewGoalAgentTriggerWorker(worker.GoalAgentTriggerWorkerOptions{
		Service: goalAgentTriggerService, PollInterval: time.Second, BatchSize: 100, Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("initialize Goal agent trigger worker: %w", err)
	}
	if workspaceSyncNodeProvisioner != nil {
		if err := workspaceSyncNodeProvisioner.ReconcileWorkspaceSyncNodesOnStartup(ctx); err != nil {
			logger.Warn("Workspace sync startup reconciliation incomplete; affected relay gates remain closed", zap.Error(err))
		}
	}
	apiServer := httpserver.NewServer(cfg.Listen.API, apiHandler, cfg.HTTP)
	gatewayServer := httpserver.NewServer(cfg.Listen.Gateway, gatewayHandler, cfg.HTTP)
	metricsServer := httpserver.NewServer(cfg.Listen.Metrics, metricsHandler(registry), cfg.HTTP)
	logger.Info("portal starting", zap.String("instance", cfg.Instance), zap.Int64("migration_version", migrationVersion), zap.String("api", cfg.Listen.API), zap.String("gateway", cfg.Listen.Gateway), zap.String("metrics", cfg.Listen.Metrics))
	personalCleanupCtx, stopPersonalCleanup := context.WithCancel(ctx)
	defer stopPersonalCleanup()
	go runPersonalDataCleanup(personalCleanupCtx, personalAttachmentService, personalDataQuotaService, tasks, logger)
	fileFetchCleanupCtx, stopFileFetchCleanup := context.WithCancel(ctx)
	defer stopFileFetchCleanup()
	go runTaskFileFetchCleanup(fileFetchCleanupCtx, tasks, logger)
	return runServers(ctx, logger, cfg.HTTP.ShutdownTimeout, background, taskStopWorker, notificationWorker, taskWorker, routeWorker, resultWorker, recordWorker, scheduleWorker, goalAgentTriggerWorker, runtime, gatewayHandler, apiServer, gatewayServer, metricsServer)
}

func runTaskFileFetchCleanup(ctx context.Context, tasks *app.TaskService, logger *zap.Logger) {
	ticker := time.NewTicker(app.TaskFileFetchCleanupInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if count, err := tasks.CleanupExpiredTaskFileFetches(cleanupCtx, time.Now().UTC(), app.TaskFileFetchCleanupBatch); err != nil {
			if logger != nil {
				logger.Warn("Task file fetch cleanup failed", zap.Error(err))
			}
		} else if count > 0 && logger != nil {
			logger.Info("Task file fetch cleanup completed", zap.Int32("records", count))
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runPersonalDataCleanup(ctx context.Context, attachments *app.PersonalAttachmentService, quota *app.PersonalDataQuotaService,
	tasks *app.TaskService, logger *zap.Logger) {
	const (
		batchLimit int32 = 1000
		interval         = time.Minute
		timeout          = 30 * time.Second
	)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(ctx, timeout)
		if _, err := attachments.CleanupExpiredPersonalAttachmentUploads(cleanupCtx, time.Now().UTC(), batchLimit); err != nil && logger != nil {
			logger.Warn("personal attachment cleanup failed", zap.Error(err))
		}
		if _, err := quota.CleanupExpiredPersonalDataQuotaReservations(cleanupCtx, time.Now().UTC(), batchLimit); err != nil && logger != nil {
			logger.Warn("orphan personal data quota reservation cleanup failed", zap.Error(err))
		}
		if _, err := tasks.CleanupExpiredConversationMessageUploads(cleanupCtx, time.Now().UTC(), batchLimit); err != nil && logger != nil {
			logger.Warn("expired conversation message upload cleanup failed", zap.Error(err))
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type cloudDSHTaskWorkerBundle struct {
	taskWorker      *worker.TaskWorker
	accountRuntimes *worker.CloudDSHAccountRuntimeManager
}

func newCloudDSHTaskWorker(cfg config.CloudDSH, store *postgres.Store, authorizer app.TaskAuthorizer,
	runtimeControl *app.RuntimeControl, workspaceSync *app.WorkspaceSyncService, models *app.ModelGatewayService,
	sealer *app.Sealer, mcpConnectors *app.MCPConnectorService, recordCapture *app.RecordCaptureService,
	personalData *app.PersonalMutationService, personalSync *app.PersonalSyncService,
	personalFiles *app.PersonalAttachmentService, goals *app.GoalService, schedules *app.ScheduleService, projects *app.ProjectService, hubService *app.HubService,
	workerID, serviceInstanceID string, historyCursorKey []byte, logger *zap.Logger) (*cloudDSHTaskWorkerBundle, error) {
	supervisorKey, err := readProtectedKey(cfg.SupervisorKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read cloud_dsh.supervisor_key_file: %w", err)
	}
	guardianPublicKey, err := readKey(cfg.GuardianPublicKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read cloud_dsh.guardian_public_key_file: %w", err)
	}
	supervisorAuth, err := app.NewAgentStopAuthenticator(cfg.SupervisorID, supervisorKey)
	if err != nil {
		return nil, err
	}
	guardianKey := ed25519.PublicKey(guardianPublicKey)
	guardianAuth, err := app.NewAgentGuardianStopAuthenticator(cfg.SupervisorID, guardianKey)
	if err != nil {
		return nil, err
	}
	taskGuardianAuth, err := app.NewAccountAgentTaskGuardianStopAuthenticator(guardianKey)
	if err != nil {
		return nil, err
	}
	instances, err := app.NewAgentInstanceServiceWithGuardianStops(store, supervisorAuth, guardianAuth, taskGuardianAuth)
	if err != nil {
		return nil, err
	}
	runtimeControlService, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		return nil, err
	}
	runtimeStopAuth, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(guardianKey)
	if err != nil {
		return nil, err
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		return nil, err
	}
	toolRuntime, err := app.NewRuntimeToolExecutor(store, authorizer, runtimeControl, runtimeControl, workspaceSync)
	if err != nil {
		return nil, err
	}
	tools, err := app.NewCloudDSHToolBrokerWithPersonalDataAndSchedules(store, toolRuntime, mcpConnectors, newMCPConnectorToolClient(),
		recordCapture, personalData, personalSync, goals, personalFiles, projects, schedules, hubService)
	if err != nil {
		return nil, err
	}
	if err := tools.SetHistorySearchCursorSigning(serviceInstanceID, historyCursorKey); err != nil {
		return nil, err
	}
	if cfg.BuiltinSearch.Enabled {
		search, err := newBuiltinSearchToolClient(cfg.BuiltinSearch)
		if err != nil {
			return nil, fmt.Errorf("configure built-in search: %w", err)
		}
		if err := tools.SetBuiltinSearch(search); err != nil {
			return nil, err
		}
	}
	childSessions, err := app.NewAgentChildSessionService(store)
	if err != nil {
		return nil, err
	}
	guardian, err := worker.NewUnixSocketDockerCloudDSHGuardian(cfg.GuardianSocket)
	if err != nil {
		return nil, err
	}
	supervisor, err := worker.NewDockerCloudDSHSupervisor(worker.DockerCloudDSHSupervisorOptions{
		DockerHost: cfg.DockerHost, Image: cfg.Image, NodePath: cfg.NodePath, ScriptPath: cfg.ScriptPath,
		Guardian: guardian,
	})
	if err != nil {
		return nil, err
	}
	accountRuntimeSupervisor, err := worker.NewDockerCloudDSHAccountRuntimeSupervisor(supervisor)
	if err != nil {
		return nil, err
	}
	accountTaskGuardian, ok := guardian.(worker.CloudDSHAccountRuntimeTaskGuardian)
	if !ok {
		return nil, errors.New("account Agent Runtime Guardian operations are unavailable")
	}
	accountRuntimes, err := worker.NewCloudDSHAccountRuntimeManager(worker.CloudDSHAccountRuntimeManagerOptions{
		Control: runtimeControlService, Supervisor: accountRuntimeSupervisor, Guardian: accountTaskGuardian,
		StopVerifier: runtimeStopAuth, TaskStopper: instances, LeaseOwner: workerID + "-runtime",
		MaxTasks: cfg.MaxConcurrentPerOwner,
	})
	if err != nil {
		return nil, err
	}
	executor, err := worker.NewCloudDSHExecutor(worker.CloudDSHExecutorOptions{
		SupervisorID: cfg.SupervisorID, SummarySealer: sealer, AccountRuntimes: accountRuntimes,
		Instances: instances, Checkpoints: checkpoints, Models: models, Tools: tools,
		Children: childSessions, Completion: tools, RevisionStops: store, TextStreams: store, ProjectContexts: store,
	})
	if err != nil {
		return nil, err
	}
	taskWorker, err := worker.NewTaskWorker(worker.TaskWorkerOptions{
		Store: store, Executor: executor, Lifecycle: accountRuntimes.Run, WorkerID: workerID + "-task",
		LeaseDuration: 30 * time.Second, PollInterval: 500 * time.Millisecond,
		MaxConcurrentTasks: cfg.MaxConcurrentTasks, MaxConcurrentTasksPerOwner: cfg.MaxConcurrentPerOwner, Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	return &cloudDSHTaskWorkerBundle{taskWorker: taskWorker, accountRuntimes: accountRuntimes}, nil
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
		items := make([]string, 0, len(pendingImports))
		for _, pending := range pendingImports {
			item := pending.WorkspaceID
			if pending.ImportError != "" {
				item += " (needs cleanup: " + pending.ImportError + ")"
			}
			items = append(items, item)
		}
		return fmt.Errorf("%d legacy workspace volume(s) are pending import or cleanup (%s); run portal workspace import-legacy --workspace <id>", len(pendingImports), strings.Join(items, ", "))
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
	blockGC, err := app.NewWorkspaceBlockGarbageCollector(store, blocks)
	if err != nil {
		return fmt.Errorf("initialize legacy Workspace block recovery: %w", err)
	}
	if _, err := blockGC.CollectPendingLegacyImport(ctx, workspaceID); err != nil {
		return fmt.Errorf("recover blocks from a previous legacy import attempt: %w", err)
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

func readProtectedKey(path string) ([]byte, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("key file must be a regular file with no group or other permissions")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !fileInfo.Mode().IsRegular() || !os.SameFile(pathInfo, fileInfo) {
		return nil, fmt.Errorf("key file changed while opening")
	}
	key, err := io.ReadAll(io.LimitReader(file, 33))
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

func runServers(ctx context.Context, logger *zap.Logger, shutdownTimeout time.Duration, background *worker.Worker, taskStopWorker *worker.TaskStopWorker, notificationWorker *worker.NotificationWorker, taskWorker *worker.TaskWorker, routeWorker *worker.ConversationRouteWorker, resultWorker *worker.ConversationResultReviewWorker, recordWorker *worker.RecordCaptureWorker, scheduleWorker *worker.ScheduleWorker, goalAgentTriggerWorker *worker.GoalAgentTriggerWorker, runtime *app.Runtime, gatewayHandler http.Handler, servers ...*http.Server) error {
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
	if err := ctx.Err(); err != nil {
		for _, listener := range listeners {
			_ = listener.Close()
		}
		return err
	}
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- background.Run(workerCtx) }()
	taskStopWorkerDone := make(chan error, 1)
	go func() { taskStopWorkerDone <- taskStopWorker.Run(workerCtx) }()
	notificationWorkerDone := make(chan error, 1)
	go func() { notificationWorkerDone <- notificationWorker.Run(workerCtx) }()
	recordWorkerDone := make(chan error, 1)
	go func() { recordWorkerDone <- recordWorker.Run(workerCtx) }()
	resultWorkerDone := make(chan error, 1)
	go func() { resultWorkerDone <- resultWorker.Run(workerCtx) }()
	var scheduleWorkerDone chan error
	if scheduleWorker != nil {
		scheduleWorkerDone = make(chan error, 1)
		go func() { scheduleWorkerDone <- scheduleWorker.Run(workerCtx) }()
	}
	goalAgentTriggerWorkerDone := make(chan error, 1)
	go func() { goalAgentTriggerWorkerDone <- goalAgentTriggerWorker.Run(workerCtx) }()
	var taskWorkerDone chan error
	var cancelTaskWorker context.CancelFunc
	if taskWorker != nil {
		taskWorkerCtx, cancel := context.WithCancel(context.Background())
		cancelTaskWorker = cancel
		taskWorkerDone = make(chan error, 1)
		go func() { taskWorkerDone <- taskWorker.Run(taskWorkerCtx) }()
	}
	var routeWorkerDone chan error
	var cancelRouteWorker context.CancelFunc
	if routeWorker != nil {
		routeWorkerCtx, cancel := context.WithCancel(context.Background())
		cancelRouteWorker = cancel
		routeWorkerDone = make(chan error, 1)
		go func() { routeWorkerDone <- routeWorker.Run(routeWorkerCtx) }()
	}
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
	taskWorkerExited := false
	routeWorkerExited := false
	resultWorkerExited := false
	select {
	case <-ctx.Done():
	case runErr = <-serverErrors:
	case err := <-taskWorkerDone:
		taskWorkerExited = true
		if err == nil {
			runErr = errors.New("cloud task worker stopped unexpectedly")
		} else {
			runErr = fmt.Errorf("cloud task worker stopped unexpectedly: %w", err)
		}
	case err := <-routeWorkerDone:
		routeWorkerExited = true
		if err == nil {
			runErr = errors.New("conversation routing worker stopped unexpectedly")
		} else {
			runErr = fmt.Errorf("conversation routing worker stopped unexpectedly: %w", err)
		}
	case err := <-resultWorkerDone:
		resultWorkerExited = true
		if err == nil {
			runErr = errors.New("conversation result worker stopped unexpectedly")
		} else {
			runErr = fmt.Errorf("conversation result worker stopped unexpectedly: %w", err)
		}
	case err := <-scheduleWorkerDone:
		if err == nil {
			runErr = errors.New("schedule worker stopped unexpectedly")
		} else {
			runErr = fmt.Errorf("schedule worker stopped unexpectedly: %w", err)
		}
	case err := <-goalAgentTriggerWorkerDone:
		if err == nil {
			runErr = errors.New("goal agent trigger worker stopped unexpectedly")
		} else {
			runErr = fmt.Errorf("goal agent trigger worker stopped unexpectedly: %w", err)
		}
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
	if cancelRouteWorker != nil {
		cancelRouteWorker()
		if !routeWorkerExited {
			select {
			case err := <-routeWorkerDone:
				if err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
					runErr = fmt.Errorf("stop conversation routing worker: %w", err)
				}
			case <-shutdownCtx.Done():
				if runErr == nil {
					runErr = errors.New("conversation routing worker did not stop before shutdown timeout")
				}
			}
		}
	}
	if cancelTaskWorker != nil {
		cancelTaskWorker()
		if !taskWorkerExited {
			drainCtx, cancelDrain := context.WithTimeout(context.Background(), shutdownTimeout)
			select {
			case err := <-taskWorkerDone:
				if err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
					runErr = fmt.Errorf("stop cloud task worker: %w", err)
				}
			case <-drainCtx.Done():
				if runErr == nil {
					runErr = fmt.Errorf("cloud task worker did not stop before shutdown timeout; Guardian lease expiry remains the final stop boundary")
				}
			}
			cancelDrain()
		}
	}
	cancelWorker()
	if !resultWorkerExited {
		if err := <-resultWorkerDone; err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
			runErr = fmt.Errorf("stop conversation result worker: %w", err)
		}
	}
	if err := <-workerDone; err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
		runErr = fmt.Errorf("stop worker: %w", err)
	}
	if err := <-taskStopWorkerDone; err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
		runErr = fmt.Errorf("stop task action worker: %w", err)
	}
	if err := <-notificationWorkerDone; err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
		runErr = fmt.Errorf("stop notification worker: %w", err)
	}
	if err := <-recordWorkerDone; err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
		runErr = fmt.Errorf("stop record worker: %w", err)
	}
	if scheduleWorkerDone != nil {
		if err := <-scheduleWorkerDone; err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
			runErr = fmt.Errorf("stop schedule worker: %w", err)
		}
	}
	if err := <-goalAgentTriggerWorkerDone; err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
		runErr = fmt.Errorf("stop Goal agent trigger worker: %w", err)
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

func workspaceSyncDockerSocketPath(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "unix" || parsed.Host != "" || parsed.User != nil || parsed.Opaque != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || !filepath.IsAbs(parsed.Path) || filepath.Clean(parsed.Path) != parsed.Path || parsed.Path == string(filepath.Separator) {
		return "", errors.New("workspace sync node requires a local Unix Docker socket")
	}
	return parsed.Path, nil
}
