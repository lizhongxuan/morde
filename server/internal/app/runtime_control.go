package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/control"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/runtimeclient"
)

const (
	runtimeControlPort     = 8090
	runtimeTaskLeaseWindow = 25 * time.Second
)

// RuntimeSession is a verified, per-call runtime connection. Its protocol
// client is private so callers can only use methods that retain the verified
// Host fence.
type RuntimeSession struct {
	Fence        runtimeclient.HostFence
	Mode         runtimeclient.Mode
	Capabilities []string
	client       *runtimeclient.Client
	hosts        HostStore
	closeIdle    func()
}

// RuntimeControl owns the only Morde connection path to a Host runtime.
type RuntimeControl struct {
	hosts     HostStore
	sandboxes SandboxRouter
	sealer    *Sealer
}

func NewRuntimeControl(hosts HostStore, sandboxes SandboxRouter, sealer *Sealer) (*RuntimeControl, error) {
	if hosts == nil || sandboxes == nil || sealer == nil {
		return nil, errors.New("runtime control dependencies are required")
	}
	return &RuntimeControl{hosts: hosts, sandboxes: sandboxes, sealer: sealer}, nil
}

// ReadTaskFence returns the current Host generation and AI-control gate for an
// owned, live Host. Callers still recheck it before every runtime operation.
func (r *RuntimeControl) ReadTaskFence(ctx context.Context, ownerID, hostID string) (AgentHostFence, error) {
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidHostID(hostID) {
		return AgentHostFence{}, domain.NewError(domain.CodeNotFound, "Host not found")
	}
	value, err := r.hosts.GetHost(ctx, hostID)
	if err != nil {
		return AgentHostFence{}, err
	}
	if value.OwnerID != ownerID || value.State != host.StateRunning || value.DeletedAt != nil {
		return AgentHostFence{}, domain.NewError(domain.CodeNotFound, "Host not found")
	}
	return AgentHostFence{
		RuntimeGeneration: value.RuntimeGeneration,
		RuntimeInstanceID: value.RuntimeInstanceID,
		ControlVersion:    value.ControlVersion,
		AIAllowed:         value.ControlState == host.ControlAI,
	}, nil
}

// Connect loads the current Host fence, performs a TLS-pinned capabilities
// handshake, stores the observed instance under a generation/control CAS, and
// reconciles only the runtime mode that the Host record already authorizes.
func (r *RuntimeControl) Connect(ctx context.Context, hostID string, expected *AgentHostFence) (*RuntimeSession, error) {
	return r.connect(ctx, hostID, expected, false)
}

func (r *RuntimeControl) connect(ctx context.Context, hostID string, expected *AgentHostFence, allowCreating bool) (*RuntimeSession, error) {
	if ctx == nil || !domain.ValidHostID(hostID) {
		return nil, domain.NewError(domain.CodeInvalidArgument, "runtime Host is invalid")
	}
	value, err := r.hosts.GetHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	return r.connectHost(ctx, value, expected, allowCreating, nil)
}

// ConnectTransition is the narrowly-scoped recovery handshake for a durable
// control handoff. A runtime-ahead fence is accepted only when the persisted
// transition proves that its exact target mode was already authorized.
func (r *RuntimeControl) ConnectTransition(ctx context.Context, transition control.Transition) (*RuntimeSession, error) {
	if ctx == nil || !domain.ValidHostID(transition.HostID) || !domain.ValidUserID(transition.OwnerID) ||
		!control.ValidRequestID(transition.RequestID) || !control.ValidPhase(transition.Phase) || transition.Phase == control.PhaseCompleted ||
		transition.ExpectedControlVersion < 0 || transition.BlockedControlVersion != transition.ExpectedControlVersion+1 ||
		transition.TargetControlVersion != transition.BlockedControlVersion+1 {
		return nil, domain.NewError(domain.CodeInvalidArgument, "runtime control transition is invalid")
	}
	value, err := r.hosts.GetHost(ctx, transition.HostID)
	if err != nil {
		return nil, err
	}
	expectedState := transitionStateForAction(transition.Action)
	if expectedState == "" || value.OwnerID != transition.OwnerID || value.State != host.StateRunning || value.DeletedAt != nil ||
		value.ControlState != expectedState || value.ControlVersion != transition.BlockedControlVersion {
		return nil, domain.NewError(domain.CodeInvalidState, "Host no longer matches the persisted control transition")
	}
	return r.connectHost(ctx, value, nil, false, &transition)
}

func (r *RuntimeControl) connectHost(ctx context.Context, value host.Host, expected *AgentHostFence, allowCreating bool, transition *control.Transition) (*RuntimeSession, error) {
	validState := value.State == host.StateRunning || (allowCreating && value.State == host.StateCreating)
	if !validState || value.DeletedAt != nil || value.SandboxID == "" || value.RuntimeGeneration < 1 || value.ControlVersion < 0 {
		return nil, domain.NewError(domain.CodeInvalidState, "Host runtime is not ready")
	}
	if expected != nil && (value.RuntimeGeneration != expected.RuntimeGeneration || value.ControlVersion != expected.ControlVersion || !expected.AIAllowed) {
		return nil, domain.NewError(domain.CodeInvalidState, "Host runtime fence changed")
	}
	if len(value.RuntimeCredentialEnc) == 0 {
		return nil, domain.NewError(domain.CodeInvalidState, "Host runtime credential is unavailable")
	}
	credentialBytes, err := r.sealer.Open(value.RuntimeCredentialEnc)
	if err != nil {
		return nil, domain.NewError(domain.CodeInvalidState, "Host runtime credential is unavailable")
	}
	credential := string(credentialBytes)
	clear(credentialBytes)

	client, err := r.sandboxes.For(value.RuntimeProfile)
	if err != nil {
		return nil, fmt.Errorf("resolve runtime profile: %w", ErrSandboxUnavailable)
	}
	upstream, err := client.Endpoint(ctx, value.SandboxID, runtimeControlPort)
	if err != nil {
		return nil, fmt.Errorf("resolve runtime endpoint: %w", ErrSandboxUnavailable)
	}
	baseURL, err := runtimeHTTPSURL(upstream.BaseURL)
	if err != nil {
		return nil, domain.NewError(domain.CodeUpstreamError, "Host runtime endpoint is invalid")
	}
	httpClient, err := runtimeclient.NewHostHTTPClient(credential, value.ID, upstream.Headers)
	if err != nil {
		return nil, domain.NewError(domain.CodeUpstreamError, "Host runtime TLS identity is invalid")
	}
	protocolClient, err := runtimeclient.New(baseURL, credential, httpClient)
	if err != nil {
		httpClient.CloseIdleConnections()
		return nil, domain.NewError(domain.CodeUpstreamError, "Host runtime endpoint is invalid")
	}
	capabilities, err := protocolClient.Capabilities(ctx, runtimeclient.CapabilitiesRequest{
		HostID: value.ID, RuntimeGeneration: value.RuntimeGeneration, ControlVersion: value.ControlVersion,
	})
	if err != nil {
		httpClient.CloseIdleConnections()
		return nil, fmt.Errorf("runtime capabilities handshake: %w", err)
	}
	if !capabilities.OperationStoreReady || !hasRequiredRuntimeCapabilities(capabilities.Capabilities) {
		httpClient.CloseIdleConnections()
		return nil, domain.NewError(domain.CodeUnsupported, "Host runtime lacks required operation controls")
	}
	if capabilities.ControlVersion > value.ControlVersion {
		mode, recovered := recoveredTransitionMode(value, transition, capabilities)
		if !recovered || value.RuntimeInstanceID == "" || value.RuntimeInstanceID != capabilities.RuntimeInstanceID {
			httpClient.CloseIdleConnections()
			return nil, domain.NewError(domain.CodeInvalidState, "Host runtime control fence is ahead of Morde")
		}
		fence := runtimeclient.HostFence{
			HostID: value.ID, RuntimeGeneration: value.RuntimeGeneration,
			RuntimeInstanceID: capabilities.RuntimeInstanceID, ControlVersion: capabilities.ControlVersion,
		}
		return &RuntimeSession{Fence: fence, Mode: mode, Capabilities: capabilities.Capabilities, client: protocolClient, hosts: r.hosts, closeIdle: httpClient.CloseIdleConnections}, nil
	}
	if capabilities.ControlVersion < 0 {
		httpClient.CloseIdleConnections()
		return nil, domain.NewError(domain.CodeInvalidState, "Host runtime control fence is invalid")
	}
	if value.RuntimeInstanceID != capabilities.RuntimeInstanceID {
		if err := r.hosts.SetRuntimeInstanceID(ctx, value.ID, value.RuntimeGeneration, value.ControlVersion, value.RuntimeInstanceID, capabilities.RuntimeInstanceID); err != nil {
			// An identical handshake may have persisted this instance first. Accept
			// that race only after rereading the Host; a delayed response from an old
			// runtime must not overwrite or outlive a newer instance identity.
			current, readErr := r.hosts.GetHost(ctx, value.ID)
			if readErr != nil || current.RuntimeGeneration != value.RuntimeGeneration || current.ControlVersion != value.ControlVersion || current.RuntimeInstanceID != capabilities.RuntimeInstanceID {
				httpClient.CloseIdleConnections()
				return nil, fmt.Errorf("persist runtime instance fence: %w", err)
			}
			value = current
		} else {
			value.RuntimeInstanceID = capabilities.RuntimeInstanceID
		}
	}
	fence := runtimeclient.HostFence{
		HostID: value.ID, RuntimeGeneration: value.RuntimeGeneration,
		RuntimeInstanceID: capabilities.RuntimeInstanceID, ControlVersion: capabilities.ControlVersion,
	}
	mode, ok := hostRuntimeMode(value.ControlState)
	if !ok {
		httpClient.CloseIdleConnections()
		return nil, domain.NewError(domain.CodeInvalidState, "Host control state is invalid")
	}
	if capabilities.ControlVersion < value.ControlVersion {
		if mode == runtimeclient.ModeHuman {
			httpClient.CloseIdleConnections()
			return nil, domain.NewError(domain.CodeInvalidState, "human control must be explicitly reacquired")
		}
		blocked := fence
		blocked.ControlVersion = value.ControlVersion
		if _, err := protocolClient.SetControl(ctx, runtimeclient.SetControlRequest{HostFence: blocked, Mode: runtimeclient.ModeBlocked}); err != nil {
			httpClient.CloseIdleConnections()
			return nil, fmt.Errorf("block stale runtime control: %w", err)
		}
		fence.ControlVersion = value.ControlVersion
		if mode != runtimeclient.ModeBlocked {
			if _, err := protocolClient.SetControl(ctx, runtimeclient.SetControlRequest{HostFence: fence, Mode: mode}); err != nil {
				httpClient.CloseIdleConnections()
				return nil, fmt.Errorf("reconcile runtime control: %w", err)
			}
		}
		capabilities.ControlVersion = value.ControlVersion
		capabilities.Mode = mode
	} else if capabilities.Mode != mode {
		if capabilities.Mode == runtimeclient.ModeBlocked && mode == runtimeclient.ModeAI {
			if _, err := protocolClient.SetControl(ctx, runtimeclient.SetControlRequest{HostFence: fence, Mode: mode}); err != nil {
				httpClient.CloseIdleConnections()
				return nil, fmt.Errorf("activate reconciled AI control: %w", err)
			}
			capabilities.Mode = mode
		} else {
			httpClient.CloseIdleConnections()
			return nil, domain.NewError(domain.CodeInvalidState, "Host runtime control requires explicit reconciliation")
		}
	}
	fence.ControlVersion = value.ControlVersion
	return &RuntimeSession{Fence: fence, Mode: mode, Capabilities: capabilities.Capabilities, client: protocolClient, hosts: r.hosts, closeIdle: httpClient.CloseIdleConnections}, nil
}

func (r *RuntimeControl) Handshake(ctx context.Context, hostID string) error {
	session, err := r.connect(ctx, hostID, nil, true)
	if err == nil {
		session.Close()
	}
	return err
}

func (s *RuntimeSession) Close() {
	if s != nil && s.closeIdle != nil {
		s.closeIdle()
	}
}

func (s *RuntimeSession) Supports(capability string) bool {
	if s == nil {
		return false
	}
	for _, candidate := range s.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

func (s *RuntimeSession) Applications(ctx context.Context) (runtimeclient.ApplicationsResponse, error) {
	if s == nil || s.client == nil {
		return runtimeclient.ApplicationsResponse{}, domain.NewError(domain.CodeInvalidState, "runtime session is unavailable")
	}
	return s.client.Applications(ctx, s.Fence)
}

func (s *RuntimeSession) InstallApplication(ctx context.Context, appID, requestID string) (runtimeclient.ApplicationInstallOperation, error) {
	if s == nil || s.client == nil {
		return runtimeclient.ApplicationInstallOperation{}, domain.NewError(domain.CodeInvalidState, "runtime session is unavailable")
	}
	response, err := s.client.InstallApplication(ctx, appID, runtimeclient.ApplicationInstallRequest{
		HostFence: s.Fence, RequestID: requestID,
	})
	if err != nil {
		return runtimeclient.ApplicationInstallOperation{}, err
	}
	return response.Operation, nil
}

func (s *RuntimeSession) LaunchApplication(ctx context.Context, appID, launchID string) (runtimeclient.ApplicationLaunchOperation, error) {
	if s == nil || s.client == nil {
		return runtimeclient.ApplicationLaunchOperation{}, domain.NewError(domain.CodeInvalidState, "runtime session is unavailable")
	}
	return s.client.LaunchApplication(ctx, appID, s.Fence, launchID)
}

func (s *RuntimeSession) GetApplicationLaunch(ctx context.Context, appID, launchID string) (runtimeclient.ApplicationLaunchOperation, error) {
	if s == nil || s.client == nil {
		return runtimeclient.ApplicationLaunchOperation{}, domain.NewError(domain.CodeInvalidState, "runtime session is unavailable")
	}
	return s.client.GetApplicationLaunch(ctx, appID, s.Fence, launchID)
}

func (s *RuntimeSession) StopApplication(ctx context.Context, appID, launchID string) (runtimeclient.ApplicationLaunchOperation, error) {
	if s == nil || s.client == nil {
		return runtimeclient.ApplicationLaunchOperation{}, domain.NewError(domain.CodeInvalidState, "runtime session is unavailable")
	}
	return s.client.StopApplication(ctx, appID, s.Fence, launchID)
}

func (s *RuntimeSession) SetTaskLease(ctx context.Context, taskID string, leaseVersion int64, expiresAt time.Time) error {
	if s == nil || s.client == nil {
		return domain.NewError(domain.CodeInvalidState, "runtime session is unavailable")
	}
	expiresAt = boundedRuntimeLeaseExpiry(time.Now().UTC(), expiresAt.UTC())
	_, err := s.client.SetTaskLease(ctx, taskID, runtimeclient.SetTaskLeaseRequest{
		HostFence: s.Fence, LeaseVersion: leaseVersion, ExpiresAt: expiresAt,
	})
	return err
}

func boundedRuntimeLeaseExpiry(now, taskExpiry time.Time) time.Time {
	limit := now.Add(runtimeTaskLeaseWindow)
	if taskExpiry.After(limit) {
		return limit
	}
	return taskExpiry
}

func (s *RuntimeSession) Execute(ctx context.Context, taskID, operationID string, leaseVersion int64, capability, action string, arguments json.RawMessage, workspaceID string, bindingVersion int64, inputRevision *int64) (runtimeclient.Operation, error) {
	if s == nil || s.client == nil {
		return runtimeclient.Operation{}, domain.NewError(domain.CodeInvalidState, "runtime session is unavailable")
	}
	if runtimeclient.WorkspaceScopedCapability(capability) {
		if _, err := s.syncWorkspaceBindings(ctx, workspaceID, bindingVersion, inputRevision); err != nil {
			return runtimeclient.Operation{}, err
		}
	} else if workspaceID != "" || bindingVersion != 0 || inputRevision != nil {
		return runtimeclient.Operation{}, domain.NewError(domain.CodeInvalidArgument, "runtime Workspace binding is invalid")
	}
	return s.client.Execute(ctx, runtimeclient.ExecuteRequest{
		HostFence: s.Fence, TaskID: taskID, OperationID: operationID,
		WorkspaceID: workspaceID, BindingVersion: bindingVersion,
		LeaseVersion: leaseVersion, Capability: capability, Action: action, Arguments: arguments,
	})
}

// SyncWorkspaceBindings refreshes Runtime's in-memory binding registry from
// the latest HostStore row. expectedVersion==0 captures the current active
// version; a positive version enforces an operation's persisted binding fence.
func (s *RuntimeSession) SyncWorkspaceBindings(ctx context.Context, workspaceID string, expectedVersion int64) (int64, error) {
	return s.syncWorkspaceBindings(ctx, workspaceID, expectedVersion, nil)
}

func (s *RuntimeSession) SyncTaskWorkspaceBindings(ctx context.Context, current task.Task, workspaceID string, expectedVersion int64) (int64, error) {
	if current.WorkspaceID != workspaceID || current.InputRevision == nil {
		return 0, domain.NewError(domain.CodeWorkspaceConflict, "task has no matching pinned Workspace input")
	}
	return s.syncWorkspaceBindings(ctx, workspaceID, expectedVersion, current.InputRevision)
}

func (s *RuntimeSession) syncWorkspaceBindings(ctx context.Context, workspaceID string, expectedVersion int64, expectedRevision *int64) (int64, error) {
	if s == nil || s.client == nil || s.hosts == nil || !domain.ValidWorkspaceID(workspaceID) || expectedVersion < 0 {
		return 0, domain.NewError(domain.CodeInvalidArgument, "runtime Workspace binding is invalid")
	}
	current, err := s.hosts.GetHost(ctx, s.Fence.HostID)
	if err != nil {
		return 0, err
	}
	if current.ID != s.Fence.HostID || current.State != host.StateRunning || current.DeletedAt != nil ||
		current.RuntimeGeneration != s.Fence.RuntimeGeneration || current.RuntimeInstanceID != s.Fence.RuntimeInstanceID || current.ControlVersion != s.Fence.ControlVersion || current.ControlState != host.ControlAI {
		return 0, domain.NewError(domain.CodeInvalidState, "Host runtime fence changed")
	}
	bindings := make([]runtimeclient.WorkspaceBinding, 0, len(current.Workspaces))
	var selected int64
	for _, binding := range current.Workspaces {
		if binding.State != workspace.BindingActive {
			continue
		}
		if !domain.ValidWorkspaceID(binding.Workspace.ID) || binding.BindingVersion < 1 || binding.Directory != binding.Workspace.ID {
			return 0, domain.NewError(domain.CodeInvalidState, "Host Workspace binding is invalid")
		}
		bindings = append(bindings, runtimeclient.WorkspaceBinding{
			WorkspaceID: binding.Workspace.ID, BindingVersion: binding.BindingVersion, Directory: binding.Directory,
		})
		if binding.Workspace.ID == workspaceID {
			selected = binding.BindingVersion
			if expectedRevision != nil && (binding.SyncState != workspace.BindingSyncHealthy ||
				binding.DesiredRevision != *expectedRevision || binding.AppliedRevision != *expectedRevision) {
				return 0, domain.NewError(domain.CodeWorkspaceConflict, "Host Workspace no longer matches the task input revision")
			}
		}
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].WorkspaceID < bindings[j].WorkspaceID })
	if selected < 1 || (expectedVersion > 0 && selected != expectedVersion) {
		return 0, domain.NewError(domain.CodeInvalidState, "Workspace binding is missing, inactive, or stale")
	}
	if _, err := s.client.SetWorkspaceBindings(ctx, runtimeclient.SetWorkspaceBindingsRequest{
		HostFence: s.Fence, Bindings: bindings,
	}); err != nil {
		return 0, err
	}
	return selected, nil
}

func (s *RuntimeSession) GetOperation(ctx context.Context, taskID, operationID string, leaseVersion int64) (runtimeclient.Operation, error) {
	if s == nil || s.client == nil {
		return runtimeclient.Operation{}, domain.NewError(domain.CodeInvalidState, "runtime session is unavailable")
	}
	return s.client.GetOperation(ctx, operationID, runtimeclient.OperationQuery{
		HostFence: s.Fence, TaskID: taskID, LeaseVersion: leaseVersion,
	})
}

func (s *RuntimeSession) CancelOperation(ctx context.Context, operationID string) (runtimeclient.Operation, error) {
	if s == nil || s.client == nil {
		return runtimeclient.Operation{}, domain.NewError(domain.CodeInvalidState, "runtime session is unavailable")
	}
	return s.client.CancelOperation(ctx, operationID, runtimeclient.CancelOperationRequest{HostFence: s.Fence})
}

func (s *RuntimeSession) Quiesce(ctx context.Context, targetControlVersion int64, taskIDs []string) (runtimeclient.QuiesceResponse, error) {
	if s == nil || s.client == nil {
		return runtimeclient.QuiesceResponse{}, domain.NewError(domain.CodeInvalidState, "runtime session is unavailable")
	}
	return s.client.Quiesce(ctx, runtimeclient.QuiesceRequest{
		HostFence: s.Fence, TargetControlVersion: targetControlVersion, TaskIDs: taskIDs,
	})
}

func (s *RuntimeSession) SetControl(ctx context.Context, controlVersion int64, mode runtimeclient.Mode, receipt *runtimeclient.DrainedReceipt) error {
	if s == nil || s.client == nil {
		return domain.NewError(domain.CodeInvalidState, "runtime session is unavailable")
	}
	fence := s.Fence
	fence.ControlVersion = controlVersion
	_, err := s.client.SetControl(ctx, runtimeclient.SetControlRequest{HostFence: fence, Mode: mode, DrainedReceipt: receipt})
	return err
}

func runtimeHTTPSURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("invalid runtime endpoint")
	}
	parsed.Scheme = "https"
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

func hostRuntimeMode(state host.ControlState) (runtimeclient.Mode, bool) {
	switch state {
	case host.ControlAI:
		return runtimeclient.ModeAI, true
	case host.ControlUser:
		return runtimeclient.ModeHuman, true
	case host.ControlTakingOver, host.ControlReturning:
		return runtimeclient.ModeBlocked, true
	default:
		return "", false
	}
}

func transitionStateForAction(action control.Action) host.ControlState {
	switch action {
	case control.ActionTakeOver:
		return host.ControlTakingOver
	case control.ActionRelease:
		return host.ControlReturning
	default:
		return ""
	}
}

func recoveredTransitionMode(value host.Host, transition *control.Transition, capabilities runtimeclient.CapabilitiesResponse) (runtimeclient.Mode, bool) {
	if transition == nil || transition.Phase != control.PhaseDrained ||
		capabilities.RuntimeGeneration != value.RuntimeGeneration || capabilities.ControlVersion != transition.TargetControlVersion {
		return "", false
	}
	switch transition.Action {
	case control.ActionTakeOver:
		var receipt runtimeclient.DrainedReceipt
		if json.Unmarshal(transition.DrainedReceipt, &receipt) != nil || receipt.ReceiptID == "" ||
			receipt.HostID != value.ID || receipt.RuntimeGeneration != value.RuntimeGeneration ||
			receipt.RuntimeInstanceID == "" || receipt.RuntimeInstanceID != capabilities.RuntimeInstanceID ||
			receipt.BlockedControlVersion != transition.BlockedControlVersion || receipt.TargetControlVersion != transition.TargetControlVersion ||
			!sameControlTaskIDs(receipt.TaskIDs, transition.TaskIDs) || receipt.IssuedAt.IsZero() {
			return "", false
		}
		if capabilities.Mode == runtimeclient.ModeHuman {
			return runtimeclient.ModeHuman, true
		}
	case control.ActionRelease:
		if len(transition.DrainedReceipt) == 0 && capabilities.Mode == runtimeclient.ModeAI {
			return runtimeclient.ModeAI, true
		}
	}
	return "", false
}

func sameControlTaskIDs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, id := range left {
		counts[id]++
	}
	for _, id := range right {
		if counts[id] == 0 {
			return false
		}
		counts[id]--
	}
	return true
}

func hasRequiredRuntimeCapabilities(capabilities []string) bool {
	required := [...]string{"operation.execute", "operation.query", "operation.cancel", "control.fence", "control.quiesce", "task_lease.fence"}
	set := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		set[capability] = struct{}{}
	}
	for _, capability := range required {
		if _, exists := set[capability]; !exists {
			return false
		}
	}
	return true
}
