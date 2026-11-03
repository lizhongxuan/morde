package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/control"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/runtimeclient"
)

const (
	controlObserveScope                identity.Scope = identity.ScopeComputerObserve
	controlOperateScope                identity.Scope = identity.ScopeComputerOperate
	maxControlTaskIDs                                 = 10_000
	controlConnectionDrainTimeout                     = 50 * time.Second
	controlConnectionDrainPollInterval                = 500 * time.Millisecond
)

type ControlTransitionStore interface {
	BeginControlTransition(context.Context, control.BeginRequest) (control.Transition, error)
	GetActiveControlTransition(context.Context, string) (control.Transition, error)
	SaveControlTaskIDs(context.Context, string, string, []string) (control.Transition, error)
	SetControlTransitionPhase(context.Context, string, string, control.Phase) (control.Transition, error)
	SaveControlDrainedReceipt(context.Context, string, string, json.RawMessage) (control.Transition, error)
	CompleteControlTransition(context.Context, string, string) (control.Transition, error)
	RecordControlTransitionFailure(context.Context, string, string, string) error
}

type MCPRuntimeTaskIDReader interface {
	ListActiveMCPRuntimeTaskIDs(context.Context, string, string, AgentHostFence) ([]string, error)
}

// ControlStatus is the authoritative Host control fence plus the durable
// transition, if a handoff is still in progress.
type ControlStatus struct {
	HostID                string              `json:"host_id"`
	ControlState          host.ControlState   `json:"control_state"`
	ControlVersion        int64               `json:"control_version"`
	ControlActorUserID    string              `json:"control_actor_user_id"`
	ControlLeaseExpiresAt *time.Time          `json:"control_lease_expires_at"`
	ControlLeaseActive    bool                `json:"control_lease_active"`
	ControlLeaseHeldByMe  bool                `json:"control_lease_held_by_me"`
	ActiveTransition      *control.Transition `json:"active_transition,omitempty"`
}

type ApplicationsStatus struct {
	HostID            string
	RuntimeGeneration int64
	RuntimeInstanceID string
	ControlVersion    int64
	CheckedAt         time.Time
	Items             []runtimeclient.ApplicationStatusItem
}

type ApplicationInstallResult struct {
	HostID            string
	RuntimeGeneration int64
	RuntimeInstanceID string
	ControlVersion    int64
	Operation         runtimeclient.ApplicationInstallOperation
}

type ControlActionCommand struct {
	Action                 control.Action
	RequestID              string
	ExpectedControlVersion int64
}

type ControlActionResult struct {
	Status              ControlStatus `json:"status"`
	PendingOperationIDs []string      `json:"pending_operation_ids"`
}

// ControlService owns the only user-facing Host handoff workflow. It keeps
// the Host fence blocked until the runtime confirms quiescence and the final
// runtime mode is durably reconciled.
type ControlService struct {
	hosts             HostStore
	leases            HostControlLeaseStore
	authorizer        TaskAuthorizer
	tasks             TaskStore
	transitions       ControlTransitionStore
	activity          HostActivityLeaseDrainer
	mcpTaskIDs        MCPRuntimeTaskIDReader
	runtime           *RuntimeControl
	audit             AuditWriter
	drainTimeout      time.Duration
	drainPollInterval time.Duration
}

func NewControlService(hosts HostStore, authorizer TaskAuthorizer, tasks TaskStore, transitions ControlTransitionStore, activity HostActivityLeaseDrainer, runtime *RuntimeControl, writer AuditWriter, mcpTaskIDs MCPRuntimeTaskIDReader) (*ControlService, error) {
	if hosts == nil || authorizer == nil || tasks == nil || transitions == nil || activity == nil || runtime == nil || writer == nil || mcpTaskIDs == nil {
		return nil, errors.New("control service dependencies are required")
	}
	leases, ok := hosts.(HostControlLeaseStore)
	if !ok {
		return nil, errors.New("host control lease store is required")
	}
	return &ControlService{
		hosts: hosts, leases: leases, authorizer: authorizer, tasks: tasks, transitions: transitions,
		activity: activity, mcpTaskIDs: mcpTaskIDs, runtime: runtime, audit: writer,
		drainTimeout: controlConnectionDrainTimeout, drainPollInterval: controlConnectionDrainPollInterval,
	}, nil
}

func (s *ControlService) Status(ctx context.Context, principal identity.Principal, hostID string) (ControlStatus, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return ControlStatus{}, err
	}
	value, err := s.authorizeControlHost(ctx, principal, hostID, false)
	if err != nil || value.State != host.StateRunning {
		return ControlStatus{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	status := controlStatusForHost(value)
	if err := s.populateControlLeaseStatus(ctx, &status, principal.UserID); err != nil {
		return ControlStatus{}, err
	}
	transition, err := s.transitions.GetActiveControlTransition(ctx, hostID)
	if err == nil {
		if transition.OwnerID != value.OwnerID {
			return ControlStatus{}, domain.NewError(domain.CodeNotFound, "host not found")
		}
		status.ActiveTransition = &transition
	} else if domain.CodeOf(err) != domain.CodeNotFound {
		return ControlStatus{}, err
	}
	return status, nil
}

func (s *ControlService) Applications(ctx context.Context, principal identity.Principal, hostID string) (ApplicationsStatus, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return ApplicationsStatus{}, err
	}
	if _, err := authorizeTaskHost(ctx, s.authorizer, principal, hostID, controlObserveScope); err != nil {
		return ApplicationsStatus{}, hideTaskAccessError(err)
	}
	value, err := s.hosts.GetHost(ctx, hostID)
	if err != nil || value.OwnerID != principal.UserID || value.State != host.StateRunning || value.DeletedAt != nil {
		return ApplicationsStatus{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	session, err := s.runtime.Connect(ctx, hostID, nil)
	if err != nil {
		return ApplicationsStatus{}, err
	}
	defer session.Close()
	applications, err := session.Applications(ctx)
	if err != nil {
		return ApplicationsStatus{}, err
	}
	installStore, ok := s.hosts.(ApplicationInstallStore)
	if !ok {
		return ApplicationsStatus{}, errors.New("durable application install store is unavailable")
	}
	pending, err := installStore.GetPendingApplicationInstall(ctx, hostID, principal.UserID)
	if err != nil && domain.CodeOf(err) != domain.CodeNotFound {
		return ApplicationsStatus{}, err
	}
	if err == nil && pending.RuntimeGeneration == applications.RuntimeGeneration {
		for _, item := range applications.Items {
			if item.ID == pending.ApplicationID && item.InstallRequestID == pending.RequestID && item.InstallOperation != nil &&
				(pending.Operation == nil || item.InstallOperation.ID == pending.Operation.ID &&
					item.InstallOperation.CreatedAt.Equal(pending.Operation.CreatedAt)) {
				if _, err := installStore.RecordApplicationInstallOperation(ctx, hostID, principal.UserID, pending.RequestID, *item.InstallOperation); err != nil {
					return ApplicationsStatus{}, err
				}
				break
			}
		}
	}
	return ApplicationsStatus{
		HostID: applications.HostID, RuntimeGeneration: applications.RuntimeGeneration,
		RuntimeInstanceID: applications.RuntimeInstanceID, ControlVersion: applications.ControlVersion,
		CheckedAt: applications.CheckedAt, Items: append([]runtimeclient.ApplicationStatusItem{}, applications.Items...),
	}, nil
}

func (s *ControlService) InstallApplication(ctx context.Context, principal identity.Principal, hostID, appID, requestID string) (ApplicationInstallResult, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return ApplicationInstallResult{}, err
	}
	if !ValidHostApplicationID(appID) {
		return ApplicationInstallResult{}, domain.InvalidArgument("app_id", "application is not installable")
	}
	if !control.ValidRequestID(requestID) {
		return ApplicationInstallResult{}, domain.InvalidArgument("request_id", "request id is invalid")
	}
	if _, err := authorizeTaskHost(ctx, s.authorizer, principal, hostID, controlOperateScope); err != nil {
		return ApplicationInstallResult{}, hideTaskAccessError(err)
	}
	value, err := s.hosts.GetHost(ctx, hostID)
	if err != nil || value.OwnerID != principal.UserID || value.State != host.StateRunning || value.DeletedAt != nil {
		return ApplicationInstallResult{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	session, err := s.runtime.Connect(ctx, hostID, nil)
	if err != nil {
		return ApplicationInstallResult{}, err
	}
	defer session.Close()
	value, err = s.ownedRunningApplicationHost(ctx, principal, hostID)
	if err != nil {
		return ApplicationInstallResult{}, err
	}
	if session.Fence.RuntimeGeneration != value.RuntimeGeneration || session.Fence.RuntimeInstanceID != value.RuntimeInstanceID ||
		session.Fence.ControlVersion != value.ControlVersion {
		return ApplicationInstallResult{}, domain.NewError(domain.CodeConflict, "Host runtime fence changed")
	}
	releaseLease, err := s.acquireApplicationOperationLease(ctx, session.Fence)
	if err != nil {
		return ApplicationInstallResult{}, err
	}
	defer releaseLease()
	catalog, err := session.Applications(ctx)
	if err != nil {
		return ApplicationInstallResult{}, err
	}
	installable := false
	for _, item := range catalog.Items {
		if item.ID == appID {
			installable = item.Installable
			break
		}
	}
	if !installable {
		return ApplicationInstallResult{}, domain.NewError(domain.CodeUnsupported, "application is unavailable on this Host")
	}
	if err := s.audit.Write(ctx, audit.Entry{
		ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind), Action: audit.ActionHostApplicationInstall, Target: hostID,
		Detail: map[string]any{"application_id": appID, "request_id": requestID},
	}); err != nil {
		return ApplicationInstallResult{}, fmt.Errorf("write application install audit: %w", err)
	}
	installStore, ok := s.hosts.(ApplicationInstallStore)
	if !ok {
		return ApplicationInstallResult{}, errors.New("durable application install store is unavailable")
	}
	reserved, _, err := installStore.ReserveApplicationInstall(ctx, ApplicationInstall{
		HostID: hostID, OwnerID: principal.UserID, RequestID: requestID, ApplicationID: appID,
		RuntimeGeneration: session.Fence.RuntimeGeneration, RuntimeInstanceID: session.Fence.RuntimeInstanceID,
		ControlVersion: session.Fence.ControlVersion, Status: ApplicationInstallUnknown,
	})
	if err != nil {
		return ApplicationInstallResult{}, err
	}
	if reserved.Terminal() && reserved.Operation != nil {
		return ApplicationInstallResult{
			HostID: hostID, RuntimeGeneration: reserved.RuntimeGeneration,
			RuntimeInstanceID: reserved.RuntimeInstanceID, ControlVersion: reserved.ControlVersion,
			Operation: *reserved.Operation,
		}, nil
	}
	operation, err := session.InstallApplication(ctx, appID, requestID)
	if err != nil {
		return ApplicationInstallResult{}, err
	}
	if _, err := installStore.RecordApplicationInstallOperation(ctx, hostID, principal.UserID, requestID, operation); err != nil {
		return ApplicationInstallResult{}, err
	}
	return ApplicationInstallResult{
		HostID: session.Fence.HostID, RuntimeGeneration: session.Fence.RuntimeGeneration,
		RuntimeInstanceID: session.Fence.RuntimeInstanceID, ControlVersion: session.Fence.ControlVersion,
		Operation: operation,
	}, nil
}

func (s *ControlService) Act(ctx context.Context, principal identity.Principal, hostID string, command ControlActionCommand) (ControlActionResult, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return ControlActionResult{}, err
	}
	hostValue, err := s.authorizeControlHost(ctx, principal, hostID, true)
	if err != nil {
		return ControlActionResult{}, hideTaskAccessError(err)
	}
	request := control.BeginRequest{
		HostID: hostID, OwnerID: hostValue.OwnerID, ActorUserID: principal.UserID, RequestID: command.RequestID,
		Action: command.Action, ExpectedControlVersion: command.ExpectedControlVersion,
	}
	transition, err := s.transitions.BeginControlTransition(ctx, request)
	if err != nil {
		return ControlActionResult{}, err
	}
	if err := s.audit.Write(ctx, audit.Entry{
		ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind), Action: audit.ActionHostControl, Target: hostID,
		Detail: map[string]any{"action": command.Action, "request_id": command.RequestID, "control_version": command.ExpectedControlVersion},
	}); err != nil {
		_ = s.transitions.RecordControlTransitionFailure(ctx, hostID, transition.RequestID, "audit_unavailable")
		return ControlActionResult{}, fmt.Errorf("write control transition audit: %w", err)
	}
	return s.resume(ctx, transition, principal.UserID)
}

func (s *ControlService) RenewLease(ctx context.Context, principal identity.Principal, hostID string, expectedControlVersion int64) (ControlStatus, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return ControlStatus{}, err
	}
	_, err := s.authorizeControlHost(ctx, principal, hostID, true)
	if err != nil {
		return ControlStatus{}, hideTaskAccessError(err)
	}
	if _, err := s.leases.RenewHostControlLease(ctx, hostID, principal.UserID, expectedControlVersion); err != nil {
		return ControlStatus{}, err
	}
	return s.Status(ctx, principal, hostID)
}

func (s *ControlService) authorizeControlHost(ctx context.Context, principal identity.Principal, hostID string, operate bool) (host.Host, error) {
	if !domain.ValidHostID(hostID) {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	value, err := s.hosts.GetHost(ctx, hostID)
	if err != nil || value.OwnerID == "" || value.DeletedAt != nil {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if principal.UserID == value.OwnerID {
		scope := controlObserveScope
		if operate {
			scope = controlOperateScope
		}
		if _, err := authorizeTaskHost(ctx, s.authorizer, principal, hostID, scope); err != nil {
			return host.Host{}, hideTaskAccessError(err)
		}
		return value, nil
	}
	if principal.Kind != identity.KindSession {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	sharing, ok := s.hosts.(HostSharingStore)
	if !ok {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	access, err := sharing.GetHostActorAccess(ctx, hostID, principal.UserID)
	if err != nil || access.OwnerID != value.OwnerID || access.Disabled {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	for _, permission := range access.Permissions {
		if permission == host.PermissionDesktopView || permission == host.PermissionDesktopControl ||
			permission == host.PermissionTerminalControl || permission == host.PermissionMCPControl {
			if !operate || permission != host.PermissionDesktopView {
				return value, nil
			}
		}
	}
	return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
}

func (s *ControlService) resume(ctx context.Context, transition control.Transition, viewerUserID string) (ControlActionResult, error) {
	if transition.Phase == control.PhaseCompleted {
		status, err := s.statusForTransition(ctx, transition, viewerUserID)
		return ControlActionResult{Status: status, PendingOperationIDs: []string{}}, err
	}
	if !transition.TaskIDsSaved {
		if transition.Phase != control.PhaseBlocking {
			return ControlActionResult{}, domain.NewError(domain.CodeInvalidState, "control task snapshot is unavailable")
		}
		taskIDs, err := s.snapshotActiveTaskIDs(ctx, transition)
		if err != nil {
			_ = s.transitions.RecordControlTransitionFailure(ctx, transition.HostID, transition.RequestID, "task_snapshot_failed")
			return ControlActionResult{}, err
		}
		transition, err = s.transitions.SaveControlTaskIDs(ctx, transition.HostID, transition.RequestID, taskIDs)
		if err != nil {
			return ControlActionResult{}, err
		}
	}
	if transition.Phase == control.PhaseBlocking {
		var err error
		transition, err = s.transitions.SetControlTransitionPhase(ctx, transition.HostID, transition.RequestID, control.PhaseDraining)
		if err != nil {
			return ControlActionResult{}, err
		}
	}
	if err := s.waitForGatewayConnectionDrain(ctx, transition); err != nil {
		return ControlActionResult{}, err
	}

	session, err := s.runtime.ConnectTransition(ctx, transition)
	if err != nil {
		_ = s.transitions.RecordControlTransitionFailure(ctx, transition.HostID, transition.RequestID, "runtime_unavailable")
		return ControlActionResult{}, err
	}
	defer session.Close()

	if transition.Phase == control.PhaseDrained {
		if s.runtimeAlreadyReachedTarget(session, transition) {
			completed, completeErr := s.transitions.CompleteControlTransition(ctx, transition.HostID, transition.RequestID)
			if completeErr != nil {
				_ = s.transitions.RecordControlTransitionFailure(ctx, transition.HostID, transition.RequestID, "transition_persistence_failed")
				return ControlActionResult{}, completeErr
			}
			status, statusErr := s.statusForTransition(ctx, completed, viewerUserID)
			return ControlActionResult{Status: status, PendingOperationIDs: []string{}}, statusErr
		}
		if session.Mode != runtimeclient.ModeBlocked || session.Fence.ControlVersion != transition.BlockedControlVersion {
			return s.failTransition(ctx, transition, "runtime_fence_mismatch", domain.NewError(domain.CodeInvalidState, "runtime control fence does not match transition"))
		}
	} else if session.Mode != runtimeclient.ModeBlocked || session.Fence.ControlVersion != transition.BlockedControlVersion {
		return s.failTransition(ctx, transition, "runtime_fence_mismatch", domain.NewError(domain.CodeInvalidState, "runtime is not blocked at the transition fence"))
	}

	if transition.Phase != control.PhaseDrained {
		quiesced, err := session.Quiesce(ctx, transition.TargetControlVersion, transition.TaskIDs)
		if err != nil {
			return s.failTransition(ctx, transition, "runtime_quiesce_failed", err)
		}
		if quiesced.Status == runtimeclient.QuiesceFailed {
			return s.failTransition(ctx, transition, "runtime_quiesce_failed", domain.NewError(domain.CodeUpstreamError, "runtime could not confirm that operations stopped"))
		}
		if quiesced.Status == runtimeclient.QuiesceDraining || len(quiesced.PendingOperationIDs) != 0 {
			_ = s.transitions.RecordControlTransitionFailure(ctx, transition.HostID, transition.RequestID, "operations_pending")
			transition.FailureReason = "operations_pending"
			status, statusErr := s.statusForTransition(ctx, transition, viewerUserID)
			return ControlActionResult{Status: status, PendingOperationIDs: append([]string{}, quiesced.PendingOperationIDs...)}, statusErr
		}
		if quiesced.Status != runtimeclient.QuiesceDrained || quiesced.DrainedReceipt == nil {
			return s.failTransition(ctx, transition, "receipt_invalid", domain.NewError(domain.CodeUpstreamError, "runtime returned an invalid drained receipt"))
		}
		if transition.Action == control.ActionTakeOver {
			receipt, marshalErr := json.Marshal(quiesced.DrainedReceipt)
			if marshalErr != nil {
				return s.failTransition(ctx, transition, "receipt_invalid", marshalErr)
			}
			transition, err = s.transitions.SaveControlDrainedReceipt(ctx, transition.HostID, transition.RequestID, receipt)
		} else {
			transition, err = s.transitions.SetControlTransitionPhase(ctx, transition.HostID, transition.RequestID, control.PhaseDrained)
		}
		if err != nil {
			return ControlActionResult{}, err
		}
	}

	switch transition.Action {
	case control.ActionTakeOver:
		var receipt runtimeclient.DrainedReceipt
		if err := json.Unmarshal(transition.DrainedReceipt, &receipt); err != nil {
			return s.failTransition(ctx, transition, "receipt_invalid", domain.NewError(domain.CodeInvalidState, "persisted drained receipt is invalid"))
		}
		if err := session.SetControl(ctx, transition.TargetControlVersion, runtimeclient.ModeHuman, &receipt); err != nil {
			return s.failTransition(ctx, transition, "runtime_control_failed", err)
		}
	case control.ActionRelease:
		if err := session.SetControl(ctx, transition.TargetControlVersion, runtimeclient.ModeAI, nil); err != nil {
			return s.failTransition(ctx, transition, "runtime_control_failed", err)
		}
	default:
		return s.failTransition(ctx, transition, "invalid_action", domain.NewError(domain.CodeInvalidState, "control transition action is invalid"))
	}
	completed, err := s.transitions.CompleteControlTransition(ctx, transition.HostID, transition.RequestID)
	if err != nil {
		_ = s.transitions.RecordControlTransitionFailure(ctx, transition.HostID, transition.RequestID, "transition_persistence_failed")
		return ControlActionResult{}, err
	}
	status, err := s.statusForTransition(ctx, completed, viewerUserID)
	return ControlActionResult{Status: status, PendingOperationIDs: []string{}}, err
}

func (s *ControlService) waitForGatewayConnectionDrain(ctx context.Context, transition control.Transition) error {
	timeout := s.drainTimeout
	if timeout <= 0 {
		timeout = controlConnectionDrainTimeout
	}
	pollInterval := s.drainPollInterval
	if pollInterval <= 0 {
		pollInterval = controlConnectionDrainPollInterval
	}
	drainCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		pending, err := s.activity.HasOutstandingControlTransitionLeases(drainCtx, transition.HostID, transition.ExpectedControlVersion)
		if err != nil {
			return fmt.Errorf("check pre-transition Host connections: %w", err)
		}
		if !pending {
			return nil
		}
		select {
		case <-drainCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			err := domain.NewError(domain.CodeHostNotReady, "existing Host connections are still draining")
			err.RetryAfter = time.Second
			return err
		case <-ticker.C:
		}
	}
}

func (s *ControlService) snapshotActiveTaskIDs(ctx context.Context, transition control.Transition) ([]string, error) {
	ownerID, hostID := transition.OwnerID, transition.HostID
	statuses := []task.Status{task.StatusQueued, task.StatusRunning, task.StatusWaitingUser, task.StatusPausing, task.StatusCancelling}
	seen := make(map[string]struct{})
	for _, status := range statuses {
		cursor := ""
		for {
			page, err := s.tasks.ListTasks(ctx, ownerID, TaskListRequest{
				HostIDs: []string{hostID}, Status: status, Page: PageRequest{Limit: 100, Cursor: cursor},
			})
			if err != nil {
				return nil, fmt.Errorf("snapshot active Host tasks: %w", err)
			}
			for _, value := range page.Items {
				if value.HostID != hostID || value.OwnerID != ownerID || value.Status != status || !domain.ValidTaskID(value.ID) {
					return nil, domain.NewError(domain.CodeInternal, "stored Host task is invalid")
				}
				seen[value.ID] = struct{}{}
				if len(seen) > maxControlTaskIDs {
					return nil, domain.NewError(domain.CodeInvalidState, "Host has too many active tasks for a safe control transition")
				}
			}
			if page.NextCursor == "" {
				break
			}
			if page.NextCursor == cursor {
				return nil, domain.NewError(domain.CodeInternal, "task snapshot cursor did not advance")
			}
			cursor = page.NextCursor
		}
	}
	hostValue, err := s.hosts.GetHost(ctx, hostID)
	if err != nil || hostValue.OwnerID != ownerID || hostValue.State != host.StateRunning || hostValue.DeletedAt != nil ||
		hostValue.RuntimeGeneration < 1 || hostValue.RuntimeInstanceID == "" || hostValue.ControlVersion != transition.BlockedControlVersion {
		return nil, domain.NewError(domain.CodeInvalidState, "Host runtime fence is unavailable for control transition")
	}
	fence := AgentHostFence{
		RuntimeGeneration: hostValue.RuntimeGeneration,
		RuntimeInstanceID: hostValue.RuntimeInstanceID,
		ControlVersion:    transition.ExpectedControlVersion,
		AIAllowed:         true,
	}
	if fence.ControlVersion < 0 {
		return nil, domain.NewError(domain.CodeInvalidState, "Host runtime fence is invalid for control transition")
	}
	mcpTaskIDs, err := s.mcpTaskIDs.ListActiveMCPRuntimeTaskIDs(ctx, ownerID, hostID, fence)
	if err != nil {
		return nil, fmt.Errorf("snapshot active MCP runtime operations: %w", err)
	}
	for _, id := range mcpTaskIDs {
		if !domain.ValidTaskID(id) {
			return nil, domain.NewError(domain.CodeInternal, "stored MCP runtime task is invalid")
		}
		seen[id] = struct{}{}
		if len(seen) > maxControlTaskIDs {
			return nil, domain.NewError(domain.CodeInvalidState, "Host has too many active operations for a safe control transition")
		}
	}
	result := make([]string, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

func (s *ControlService) runtimeAlreadyReachedTarget(session *RuntimeSession, transition control.Transition) bool {
	if session == nil || session.Fence.ControlVersion != transition.TargetControlVersion {
		return false
	}
	switch transition.Action {
	case control.ActionTakeOver:
		return session.Mode == runtimeclient.ModeHuman
	case control.ActionRelease:
		return session.Mode == runtimeclient.ModeAI
	default:
		return false
	}
}

func (s *ControlService) failTransition(ctx context.Context, transition control.Transition, reason string, cause error) (ControlActionResult, error) {
	if err := s.transitions.RecordControlTransitionFailure(ctx, transition.HostID, transition.RequestID, reason); err != nil {
		return ControlActionResult{}, fmt.Errorf("control transition failed: %w (record failure: %w)", cause, err)
	}
	return ControlActionResult{}, cause
}

func (s *ControlService) statusForTransition(ctx context.Context, transition control.Transition, viewerUserID string) (ControlStatus, error) {
	value, err := s.hosts.GetHost(ctx, transition.HostID)
	if err != nil || value.OwnerID != transition.OwnerID || value.State != host.StateRunning || value.DeletedAt != nil {
		return ControlStatus{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	status := controlStatusForHost(value)
	if err := s.populateControlLeaseStatus(ctx, &status, viewerUserID); err != nil {
		return ControlStatus{}, err
	}
	if transition.Phase != control.PhaseCompleted {
		status.ActiveTransition = &transition
	} else {
		status.ActiveTransition = nil
	}
	return status, nil
}

func (s *ControlService) populateControlLeaseStatus(ctx context.Context, status *ControlStatus, viewerUserID string) error {
	lease, err := s.leases.GetHostControlLease(ctx, status.HostID)
	if err != nil {
		return err
	}
	status.ControlActorUserID = lease.ActorUserID
	status.ControlLeaseExpiresAt = lease.ExpiresAt
	status.ControlLeaseActive = lease.Active
	status.ControlLeaseHeldByMe = lease.Active && lease.ActorUserID == viewerUserID
	return nil
}

func controlStatusForHost(value host.Host) ControlStatus {
	return ControlStatus{HostID: value.ID, ControlState: value.ControlState, ControlVersion: value.ControlVersion}
}
