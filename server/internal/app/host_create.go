package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/template"
	"morde/server/internal/domain/workspace"
)

// HostServiceOptions are the app-layer dependencies and config values needed
// by the shared Portal API and MCP HostService implementation.
type HostServiceOptions struct {
	Hosts        HostStore
	Workspaces   WorkspaceStore
	Tokens       TokenStore
	Sandboxes    SandboxRouter
	Volumes      VolumeDeleter
	DiskCapacity DiskCapacityChecker
	Probe        HostProbe
	Runtime      RuntimeHandshaker
	Audit        AuditWriter
	CreateLocks  HostCreateLockStore
	Sealer       *Sealer
	Templates    map[string]template.Template
	Capacity     Capacity
	Instance     string

	Scheme            string
	PortalURL         string
	HostSuffix        string
	TicketTTL         time.Duration
	ReadyTimeout      time.Duration
	ReadyPollInterval time.Duration
	NotifyCreate      func(context.Context, string) error
	ObserveHostReady  func(result string, duration time.Duration)
	CountAuditFailure func()
	Logger            *zap.Logger
}

type hostService struct {
	hosts        HostStore
	workspaces   WorkspaceStore
	tokens       TokenStore
	sandboxes    SandboxRouter
	volumes      VolumeDeleter
	diskCapacity DiskCapacityChecker
	probe        HostProbe
	runtime      RuntimeHandshaker
	audit        AuditWriter
	createLocks  HostCreateLockStore
	sealer       *Sealer
	templates    map[string]template.Template
	capacity     Capacity
	instance     string
	scheme       string
	portalURL    string
	hostSuffix   string
	ticketTTL    time.Duration
	readyTimeout time.Duration
	pollInterval time.Duration
	notifyCreate func(context.Context, string) error
	observeReady func(string, time.Duration)
	countAudit   func()
	logger       *zap.Logger
}

// NewHostService constructs the one HostService implementation shared by
// HTTP and MCP adapters. Worker creation progress is exposed by AdvanceCreate.
func NewHostService(options HostServiceOptions) (*hostService, error) {
	if options.Hosts == nil || options.Workspaces == nil || options.Tokens == nil ||
		options.Sandboxes == nil || options.Volumes == nil || options.DiskCapacity == nil || options.Probe == nil || options.Runtime == nil ||
		options.Audit == nil || options.CreateLocks == nil || options.Sealer == nil || options.NotifyCreate == nil ||
		options.ObserveHostReady == nil || options.CountAuditFailure == nil {
		return nil, fmt.Errorf("host service dependencies are required")
	}
	if options.Logger == nil {
		return nil, fmt.Errorf("host service logger is required")
	}
	if options.Instance == "" || options.Capacity.CPUMilli <= 0 || options.Capacity.MemMiB <= 0 ||
		(options.Scheme != "http" && options.Scheme != "https") || options.HostSuffix == "" || options.PortalURL == "" {
		return nil, fmt.Errorf("host service instance, capacity, and domains are required")
	}
	portalURL, err := url.Parse(options.PortalURL)
	if err != nil || portalURL.Scheme != options.Scheme || portalURL.Host == "" || portalURL.User != nil ||
		portalURL.Path != "" || portalURL.RawQuery != "" || portalURL.Fragment != "" {
		return nil, fmt.Errorf("host service Portal URL is invalid")
	}
	if options.TicketTTL <= 0 || options.ReadyTimeout <= 0 || options.ReadyPollInterval <= 0 {
		return nil, fmt.Errorf("host service durations are invalid")
	}
	if len(options.Templates) == 0 {
		return nil, fmt.Errorf("host service templates are required")
	}

	profiles := options.Sandboxes.Profiles()
	templates := make(map[string]template.Template, len(options.Templates))
	for id, value := range options.Templates {
		if id != value.ID {
			return nil, fmt.Errorf("host service template key does not match template id")
		}
		if err := value.Validate(profiles); err != nil {
			return nil, fmt.Errorf("validate host template %q: %w", id, err)
		}
		templates[id] = value
	}
	return &hostService{
		hosts: options.Hosts, workspaces: options.Workspaces, tokens: options.Tokens,
		sandboxes: options.Sandboxes, volumes: options.Volumes, diskCapacity: options.DiskCapacity,
		probe: options.Probe, runtime: options.Runtime, audit: options.Audit, createLocks: options.CreateLocks, sealer: options.Sealer,
		templates: templates, capacity: options.Capacity, instance: options.Instance,
		scheme: options.Scheme, portalURL: options.PortalURL, hostSuffix: options.HostSuffix,
		ticketTTL: options.TicketTTL, readyTimeout: options.ReadyTimeout,
		pollInterval: options.ReadyPollInterval, notifyCreate: options.NotifyCreate,
		observeReady: options.ObserveHostReady, countAudit: options.CountAuditFailure,
		logger: options.Logger,
	}, nil
}

func (s *hostService) Create(ctx context.Context, principal identity.Principal, command CreateHostCommand) (host.Host, error) {
	if err := validatePrincipal(principal); err != nil {
		return host.Host{}, err
	}
	if err := rejectHostCredential(principal); err != nil {
		return host.Host{}, err
	}
	if principal.Kind == identity.KindAPIKey {
		if !principal.HasScope(identity.ScopeHostCreate) {
			return host.Host{}, domain.NewError(domain.CodeForbidden, "API key scope is insufficient")
		}
		if len(command.WorkspaceIDs) != 0 {
			return host.Host{}, domain.NewError(domain.CodeForbidden, "API keys can only create an unbound host")
		}
	}
	tmpl, ok := s.templates[command.TemplateID]
	if !ok {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "template not found")
	}
	if command.Name != "" && len(command.Name) > 100 {
		return host.Host{}, domain.InvalidArgument("name", "host name is too long")
	}
	name := command.Name
	if name == "" {
		name = tmpl.Name
	}

	if len(command.WorkspaceIDs) > workspace.MaxBindingsPerHost {
		return host.Host{}, domain.InvalidArgument("workspace_ids", "at most 64 workspaces can be bound to a host")
	}
	sortedWorkspaceIDs := append([]string(nil), command.WorkspaceIDs...)
	seenWorkspaceIDs := make(map[string]struct{}, len(sortedWorkspaceIDs))
	for _, workspaceID := range sortedWorkspaceIDs {
		if !domain.ValidWorkspaceID(workspaceID) {
			return host.Host{}, domain.InvalidArgument("workspace_ids", "workspace id is invalid")
		}
		if _, exists := seenWorkspaceIDs[workspaceID]; exists {
			return host.Host{}, domain.InvalidArgument("workspace_ids", "workspace ids must be unique")
		}
		seenWorkspaceIDs[workspaceID] = struct{}{}
	}
	sort.Strings(sortedWorkspaceIDs)
	var idempotency IdempotencyRecord
	if command.IdempotencyKey != "" {
		if err := validateIdempotencyKey(command.IdempotencyKey); err != nil {
			return host.Host{}, err
		}
		record, err := idempotencyRecord(principal.UserID, "host.create", command.IdempotencyKey, struct {
			TemplateID   string   `json:"template_id"`
			Name         string   `json:"name"`
			WorkspaceIDs []string `json:"workspace_ids"`
		}{tmpl.ID, name, sortedWorkspaceIDs}, "", "")
		if err != nil {
			return host.Host{}, err
		}
		idempotency = record
		replay, found, err := s.hosts.GetHostCreateReplay(ctx, idempotency)
		if err != nil {
			return host.Host{}, err
		}
		if found {
			if replay.State == host.StateCreating {
				if err := s.notifyCreate(ctx, replay.ID); err != nil {
					s.logger.Error("notify host creation retry failed", zap.String("host_id", replay.ID), zap.Error(err))
				}
			}
			return replay, nil
		}
	}
	for _, workspaceID := range sortedWorkspaceIDs {
		value, err := s.workspaces.GetWorkspace(ctx, workspaceID)
		if err != nil || value.OwnerID != principal.UserID {
			return host.Host{}, domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if value.State != workspace.StateReady {
			return host.Host{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace is not ready")
		}
	}

	requestCPU, err := template.CPUMilli(tmpl.Resources.Requests.CPU)
	if err != nil || requestCPU > math.MaxInt32 {
		return host.Host{}, domain.InvalidArgument("template", "template cpu request is invalid")
	}
	requestMemory, err := template.MemMiB(tmpl.Resources.Requests.Memory)
	if err != nil || requestMemory > math.MaxInt32 {
		return host.Host{}, domain.InvalidArgument("template", "template memory request is invalid")
	}
	limitCPU, err := template.CPUMilli(tmpl.Resources.Limits.CPU)
	if err != nil || limitCPU > math.MaxInt32 {
		return host.Host{}, domain.InvalidArgument("template", "template cpu limit is invalid")
	}
	limitMemory, err := template.MemMiB(tmpl.Resources.Limits.Memory)
	if err != nil || limitMemory > math.MaxInt32 {
		return host.Host{}, domain.InvalidArgument("template", "template memory limit is invalid")
	}
	if err := s.diskCapacity.CheckCapacity(ctx); err != nil {
		s.logger.Warn("host create blocked by storage capacity check", zap.Error(err))
		if domain.CodeOf(err) == domain.CodeCapacityExceeded {
			return host.Host{}, err
		}
		return host.Host{}, domain.NewError(domain.CodeUpstreamError, "host storage capacity is unavailable")
	}

	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return host.Host{}, fmt.Errorf("generate host api key: %w", err)
	}
	key := base64.RawURLEncoding.EncodeToString(keyBytes)
	clear(keyBytes)
	sealedKey, err := s.sealer.Seal([]byte(key))
	if err != nil {
		return host.Host{}, fmt.Errorf("seal host api key: %w", err)
	}
	runtimeCredentialBytes := make([]byte, 32)
	if _, err := rand.Read(runtimeCredentialBytes); err != nil {
		return host.Host{}, fmt.Errorf("generate host runtime credential: %w", err)
	}
	runtimeCredential := base64.RawURLEncoding.EncodeToString(runtimeCredentialBytes)
	clear(runtimeCredentialBytes)
	sealedRuntimeCredential, err := s.sealer.Seal([]byte(runtimeCredential))
	if err != nil {
		return host.Host{}, fmt.Errorf("seal host runtime credential: %w", err)
	}
	now := time.Now().UTC()
	expires := now.Add(tmpl.Lifecycle.TTL)
	value := host.Host{
		OwnerID: principal.UserID, Name: name,
		TemplateID: tmpl.ID, RuntimeProfile: tmpl.Isolation,
		State: host.StateCreating, CPUMilli: int(requestCPU), MemMiB: int(requestMemory),
		CPULimitMilli: int(limitCPU), MemLimitMiB: int(limitMemory),
		AIOKeyEnc: sealedKey, RuntimeCredentialEnc: sealedRuntimeCredential,
		RuntimeGeneration: 1, ExpiresAt: &expires,
	}
	var created host.Host
	var replayed bool
	var createErr error
	for attempt := 0; attempt < 5; attempt++ {
		value.ID, createErr = domain.NewHostID()
		if createErr != nil {
			return host.Host{}, fmt.Errorf("generate host id: %w", createErr)
		}
		grantTokenID := ""
		if principal.Kind == identity.KindAPIKey {
			grantTokenID = principal.TokenID
		}
		createRecord := idempotency
		createRecord.ResourceID = value.ID
		created, replayed, createErr = s.hosts.CreateHost(ctx, CreateHostParams{
			Host: value, WorkspaceIDs: sortedWorkspaceIDs, Capacity: s.capacity, GrantTokenID: grantTokenID, Idempotency: createRecord,
		})
		if createErr == nil || !isUniqueViolation(createErr) {
			break
		}
	}
	if createErr != nil {
		if isUniqueViolation(createErr) {
			return host.Host{}, fmt.Errorf("%w: %w", domain.ErrIDAttemptsExhausted, createErr)
		}
		return host.Host{}, createErr
	}
	if !replayed {
		s.writeAudit(ctx, audit.Entry{
			ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind),
			Action: audit.ActionHostCreate, Target: created.ID,
			Detail: map[string]any{"template_id": created.TemplateID, "workspace_ids": workspaceIDs(created.Workspaces)},
		})
	}
	if !replayed || created.State == host.StateCreating {
		if err := s.notifyCreate(ctx, created.ID); err != nil {
			s.logger.Error("notify host creation worker failed", zap.String("host_id", created.ID), zap.Error(err))
		}
	}
	return created, nil
}

// AdvanceCreate performs one host's recoverable create workflow. It is
// idempotent across worker retries: create_attempted_at gates every non-idempotent
// OpenSandbox Create call, and remote metadata is checked before retrying.
func (s *hostService) AdvanceCreate(ctx context.Context, hostID string) (result error) {
	value, err := s.hosts.GetHost(ctx, hostID)
	if err != nil {
		return err
	}
	if value.State != host.StateCreating {
		if value.State == host.StateRunning {
			return nil
		}
		return hostStateError(value.State)
	}
	lock, acquired, err := s.createLocks.TryHostCreateLock(ctx, hostID)
	if err != nil {
		return fmt.Errorf("acquire host create lock: %w", err)
	}
	if !acquired {
		return nil
	}
	if lock == nil {
		return fmt.Errorf("host create lock was acquired without a release handle")
	}
	defer func() { result = errors.Join(result, lock.Release(ctx)) }()
	value, err = s.hosts.GetHost(ctx, hostID)
	if err != nil {
		return err
	}
	if value.State != host.StateCreating {
		if value.State == host.StateRunning {
			return nil
		}
		return hostStateError(value.State)
	}
	startedAt := value.CreatedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	deadline := startedAt.Add(s.readyTimeout)
	if value.CreateAttemptedAt != nil {
		deadline = value.CreateAttemptedAt.Add(s.readyTimeout)
	}

	client, err := s.sandboxes.For(value.RuntimeProfile)
	if err != nil {
		return sandboxOperationError("select sandbox profile", err)
	}
	matches, err := s.findHostSandboxes(ctx, client, value)
	if err != nil {
		return err
	}
	if len(matches) > 0 {
		selected := selectSandbox(matches, value.SandboxID)
		if value.CreateAttemptedAt == nil {
			if err := s.hosts.MarkCreateAttempt(ctx, hostID); err != nil {
				return err
			}
			attemptedAt := time.Now()
			value.CreateAttemptedAt = &attemptedAt
			deadline = attemptedAt.Add(s.readyTimeout)
		}
		if value.SandboxID != selected.ID {
			if err := s.hosts.SetSandboxID(ctx, hostID, host.StateCreating, &selected.ID); err != nil {
				return err
			}
			value.SandboxID = selected.ID
		}
		for _, duplicate := range matches {
			if duplicate.ID == selected.ID {
				continue
			}
			if err := deleteAndConfirm(ctx, client, duplicate.ID, deadline, s.pollInterval); err != nil {
				return fmt.Errorf("delete duplicate sandbox for host %s: %w", hostID, err)
			}
		}
		return s.waitHostReady(ctx, client, value, deadline)
	}
	if value.RuntimeGeneration > 1 && value.StateReason == "restore_unknown" {
		return nil
	}

	if value.CreateAttemptedAt != nil {
		if time.Now().Before(value.CreateAttemptedAt.Add(s.readyTimeout)) {
			return nil
		}
		if value.RuntimeGeneration > 1 {
			return s.hosts.SetHostStateReason(ctx, value.ID, host.StateCreating, value.RuntimeGeneration, "restore_unknown")
		}
		return s.failCreate(ctx, client, value, "ready_timeout", "")
	}
	if err := s.hosts.MarkCreateAttempt(ctx, hostID); err != nil {
		return err
	}
	attemptedAt := time.Now()
	value.CreateAttemptedAt = &attemptedAt
	deadline = attemptedAt.Add(s.readyTimeout)
	request, err := s.createRequest(ctx, value)
	if err != nil {
		return s.failCreate(ctx, client, value, "orchestrator_error", "")
	}
	sandbox, createErr := client.Create(ctx, request)
	if createErr != nil {
		return sandboxOperationError("create sandbox", createErr)
	}
	if sandbox.ID == "" {
		return fmt.Errorf("create sandbox returned an empty id")
	}
	value.SandboxID = sandbox.ID
	if err := s.hosts.SetSandboxID(ctx, hostID, host.StateCreating, &sandbox.ID); err != nil {
		return s.failCreate(ctx, client, value, "orchestrator_error", sandbox.ID)
	}
	return s.waitHostReady(ctx, client, value, deadline)
}

func (s *hostService) createRequest(ctx context.Context, value host.Host) (SandboxCreateRequest, error) {
	tmpl, ok := s.templates[value.TemplateID]
	if !ok {
		return SandboxCreateRequest{}, domain.NewError(domain.CodeNotFound, "template not found")
	}
	key, err := s.sealer.Open(value.AIOKeyEnc)
	if err != nil {
		return SandboxCreateRequest{}, fmt.Errorf("open host api key: %w", err)
	}
	runtimeCredential, err := s.sealer.Open(value.RuntimeCredentialEnc)
	if err != nil {
		return SandboxCreateRequest{}, fmt.Errorf("open host runtime credential: %w", err)
	}
	runtimeCredentialString := string(runtimeCredential)
	replicaCredential := hostReplicaCredential(runtimeCredential)
	clear(runtimeCredential)
	return SandboxCreateRequest{
		Image: tmpl.Image, Entrypoint: append([]string(nil), tmpl.Entrypoint...),
		Timeout:  tmpl.Lifecycle.TTL,
		Requests: SandboxResources{CPU: tmpl.Resources.Requests.CPU, Memory: tmpl.Resources.Requests.Memory},
		Limits:   SandboxResources{CPU: tmpl.Resources.Limits.CPU, Memory: tmpl.Resources.Limits.Memory},
		Env: map[string]string{
			"SANDBOX_API_KEY": string(key), "MORDE_RUNTIME_BOOTSTRAP_KEY": runtimeCredentialString,
			"MORDE_HOST_REPLICA_KEY": replicaCredential, "MORDE_PORTAL_URL": s.portalURL,
			"MORDE_HOST_ID": value.ID, "MORDE_RUNTIME_GENERATION": strconv.FormatInt(value.RuntimeGeneration, 10),
			"WORKSPACE": tmpl.Workspace.MountPath, "TZ": "Asia/Shanghai",
		},
		Metadata: map[string]string{
			"ch-env": s.instance, "ch-host": value.ID, "ch-owner": value.OwnerID,
			"ch-generation": strconv.FormatInt(value.RuntimeGeneration, 10),
		},
		Volumes: []SandboxVolume{
			{
				Name: "profile-data", MountPath: ProfileVolumeMountPath,
				ClaimName: domain.ProfileVolumeName(value.ID), Storage: ProfileVolumeStorage,
			},
			{
				Name: "workspace-root", MountPath: tmpl.Workspace.MountPath,
				ClaimName: domain.WorkspaceRootVolumeName(value.ID), Storage: tmpl.Workspace.Size,
			},
			{
				Name: "runtime-data", MountPath: RuntimeDataVolumeMountPath,
				ClaimName: domain.RuntimeVolumeName(value.ID), Storage: RuntimeDataVolumeStorage,
			},
			{
				Name: "host-apps", MountPath: HostAppsVolumeMountPath,
				ClaimName: domain.HostAppsVolumeName(value.ID), Storage: HostAppsVolumeStorage,
			},
		},
	}, nil
}

func (s *hostService) waitHostReady(ctx context.Context, client SandboxClient, value host.Host, deadline time.Time) error {
	lastStage := "sandbox_start"
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for host %s readiness: %w", value.ID, err)
		}
		if !time.Now().Before(deadline) {
			s.logger.Warn("host readiness timed out", zap.String("host_id", value.ID), zap.String("stage", lastStage))
			return s.failCreate(ctx, client, value, "ready_timeout", "")
		}
		remote, err := client.Get(ctx, value.SandboxID)
		if err != nil {
			lastStage = "sandbox_status"
			if errors.Is(err, ErrSandboxNotFound) {
				return s.failCreate(ctx, client, value, "orchestrator_error", "")
			}
			if !errors.Is(err, ErrSandboxUnavailable) {
				return s.failCreate(ctx, client, value, "orchestrator_error", "")
			}
		} else {
			switch remote.State {
			case SandboxRunning:
				stage, err := s.probeReady(ctx, client, value)
				if err == nil {
					if _, err := s.hosts.TransitionHost(ctx, value.ID, host.StateCreating, host.StateRunning, ""); err != nil {
						return err
					}
					s.observeHostReady("running", value.CreatedAt)
					return nil
				}
				lastStage = stage
			case SandboxFailed, SandboxTerminated:
				return s.failCreate(ctx, client, value, "orchestrator_error", "")
			default:
				lastStage = "sandbox_start"
			}
		}
		if err := waitContext(ctx, deadline, s.pollInterval); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			return err
		}
	}
}

func (s *hostService) probeReady(ctx context.Context, client SandboxClient, value host.Host) (string, error) {
	upstream, err := client.Endpoint(ctx, value.SandboxID, 8080)
	if err != nil {
		return "aio_endpoint", err
	}
	key, err := s.sealer.Open(value.AIOKeyEnc)
	if err != nil {
		return "aio_credential", err
	}
	upstream.APIKey = string(key)
	if err := s.probe.Ready(ctx, upstream); err != nil {
		return "aio_health", err
	}
	if err := s.runtime.Handshake(ctx, value.ID); err != nil {
		return "runtime_handshake", err
	}
	return "", nil
}

func (s *hostService) failCreate(ctx context.Context, client SandboxClient, value host.Host, reason, trustedSandboxID string) error {
	if value.RuntimeGeneration > 1 {
		if err := s.cleanupHostGenerationSandboxes(ctx, client, value, trustedSandboxID); err != nil {
			return err
		}
		if value.SandboxID != "" {
			if err := s.hosts.SetSandboxID(ctx, value.ID, host.StateCreating, nil); err != nil {
				return err
			}
		}
		if _, err := s.hosts.TransitionHost(ctx, value.ID, host.StateCreating, host.StateStopped, "restore_failed"); err != nil {
			return err
		}
		s.observeHostReady("failed", value.CreatedAt)
		return nil
	}
	if err := s.cleanupHostSandboxes(ctx, client, value, trustedSandboxID); err != nil {
		return err
	}
	if value.SandboxID != "" {
		if err := s.hosts.SetSandboxID(ctx, value.ID, host.StateCreating, nil); err != nil {
			return err
		}
	}
	if _, err := s.hosts.TransitionHost(ctx, value.ID, host.StateCreating, host.StateFailed, reason); err != nil {
		return err
	}
	s.observeHostReady("failed", value.CreatedAt)
	if err := s.detachHostWorkspaces(ctx, value.ID); err != nil {
		return fmt.Errorf("detach workspaces after failed host creation: %w", err)
	}
	return nil
}

func (s *hostService) cleanupHostSandboxes(ctx context.Context, client SandboxClient, value host.Host, trustedSandboxID string) error {
	return s.cleanupHostGenerationSandboxes(ctx, client, value, trustedSandboxID)
}

func (s *hostService) cleanupHostGenerationSandboxes(ctx context.Context, client SandboxClient, value host.Host, trustedSandboxID string) error {
	matches, err := s.findHostSandboxes(ctx, client, value)
	if err != nil {
		return err
	}
	ids := make(map[string]struct{}, len(matches)+2)
	for _, match := range matches {
		ids[match.ID] = struct{}{}
	}
	for _, id := range []string{value.SandboxID, trustedSandboxID} {
		if id == "" {
			continue
		}
		remote, getErr := client.Get(ctx, id)
		if errors.Is(getErr, ErrSandboxNotFound) {
			continue
		}
		if getErr != nil {
			return fmt.Errorf("inspect sandbox before generation cleanup: %w", getErr)
		}
		if !metadataMatchesGeneration(remote, s.instance, value.ID, value.RuntimeGeneration) {
			return fmt.Errorf("refuse to delete sandbox from another runtime generation")
		}
		ids[id] = struct{}{}
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	deadline := time.Now().Add(s.readyTimeout)
	for _, id := range ordered {
		if err := deleteAndConfirm(ctx, client, id, deadline, s.pollInterval); err != nil {
			return fmt.Errorf("clean sandbox for host %s generation %d: %w", value.ID, value.RuntimeGeneration, err)
		}
	}
	remaining, err := s.findHostSandboxes(ctx, client, value)
	if err != nil {
		return err
	}
	for _, remote := range remaining {
		if remote.State != SandboxTerminated {
			return fmt.Errorf("sandbox cleanup for host %s generation %d is not confirmed", value.ID, value.RuntimeGeneration)
		}
	}
	return nil
}

func (s *hostService) findHostSandboxes(ctx context.Context, client SandboxClient, value host.Host) ([]Sandbox, error) {
	rows, err := client.List(ctx, map[string]string{"ch-env": s.instance, "ch-host": value.ID})
	if err != nil {
		return nil, fmt.Errorf("list sandboxes for host %s: %w", value.ID, err)
	}
	byID := make(map[string]Sandbox, len(rows)+1)
	for _, row := range rows {
		if row.ID != "" && metadataMatchesGeneration(row, s.instance, value.ID, value.RuntimeGeneration) {
			byID[row.ID] = row
		}
	}
	if value.SandboxID != "" {
		if _, ok := byID[value.SandboxID]; !ok {
			remote, getErr := client.Get(ctx, value.SandboxID)
			switch {
			case errors.Is(getErr, ErrSandboxNotFound):
			case getErr != nil:
				return nil, fmt.Errorf("inspect saved sandbox for host %s: %w", value.ID, getErr)
			case metadataMatchesGeneration(remote, s.instance, value.ID, value.RuntimeGeneration):
				byID[remote.ID] = remote
			default:
				return nil, fmt.Errorf("saved sandbox metadata does not match host %s", value.ID)
			}
		}
	}
	result := make([]Sandbox, 0, len(byID))
	for _, row := range byID {
		result = append(result, row)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func selectSandbox(matches []Sandbox, preferredID string) Sandbox {
	for _, match := range matches {
		if match.ID == preferredID {
			return match
		}
	}
	return matches[0]
}

func metadataMatches(value Sandbox, instance, hostID string) bool {
	return value.Metadata["ch-env"] == instance && value.Metadata["ch-host"] == hostID
}

func metadataMatchesGeneration(value Sandbox, instance, hostID string, generation int64) bool {
	return metadataMatches(value, instance, hostID) && value.Metadata["ch-generation"] == strconv.FormatInt(generation, 10)
}

func validSandboxGeneration(value Sandbox) bool {
	generation, err := strconv.ParseInt(value.Metadata["ch-generation"], 10, 64)
	return err == nil && generation > 0
}

func deleteAndConfirm(ctx context.Context, client SandboxClient, sandboxID string, deadline time.Time, interval time.Duration) error {
	if err := client.Delete(ctx, sandboxID); err != nil && !errors.Is(err, ErrSandboxNotFound) {
		return err
	}
	for {
		remote, err := client.Get(ctx, sandboxID)
		if errors.Is(err, ErrSandboxNotFound) || (err == nil && remote.State == SandboxTerminated) {
			return nil
		}
		if err != nil && !errors.Is(err, ErrSandboxUnavailable) {
			return err
		}
		if err := waitContext(ctx, deadline, interval); err != nil {
			return err
		}
	}
}

func waitContext(ctx context.Context, deadline time.Time, interval time.Duration) error {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.DeadlineExceeded
	}
	if interval > remaining {
		interval = remaining
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		if !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
		return nil
	}
}

func (s *hostService) observeHostReady(result string, createdAt time.Time) {
	if s.observeReady == nil {
		return
	}
	duration := time.Since(createdAt)
	if createdAt.IsZero() || duration < 0 {
		duration = 0
	}
	s.observeReady(result, duration)
}

func (s *hostService) writeAudit(ctx context.Context, entry audit.Entry) {
	if err := s.audit.Write(ctx, entry); err != nil {
		s.logger.Error("audit write failed", zap.String("action", string(entry.Action)), zap.Error(err))
		if s.countAudit != nil {
			s.countAudit()
		}
	}
}

func validatePrincipal(principal identity.Principal) error {
	if !domain.ValidUserID(principal.UserID) {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if principal.Role != identity.RoleUser && !principal.Role.IsAdmin() {
		return domain.NewError(domain.CodeForbidden, "access denied")
	}
	switch principal.Kind {
	case identity.KindSession:
		if principal.HostScope != "" || len(principal.HostIDs) != 0 || len(principal.Scopes) != 0 {
			return domain.NewError(domain.CodeUnauthenticated, "authentication required")
		}
	case identity.KindAPIKey:
		if principal.HostScope != "" || principal.Role != identity.RoleUser {
			return domain.NewError(domain.CodeUnauthenticated, "authentication required")
		}
	case identity.KindClient:
		if principal.HostScope != "" || principal.Role != identity.RoleUser ||
			!domain.ValidClientInstallationID(principal.ClientInstallationID) || len(principal.HostIDs) != 0 || len(principal.Scopes) != 0 {
			return domain.NewError(domain.CodeUnauthenticated, "authentication required")
		}
	case identity.KindHostCookie:
		if !domain.ValidHostID(principal.HostScope) {
			return domain.NewError(domain.CodeUnauthenticated, "authentication required")
		}
	default:
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	return nil
}

func rejectHostCredential(principal identity.Principal) error {
	switch principal.Kind {
	case identity.KindHostCookie, identity.KindTicket:
		return domain.NewError(domain.CodeForbidden, "access denied")
	default:
		return nil
	}
}

var _ HostService = (*hostService)(nil)
