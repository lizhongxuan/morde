package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

func (s *hostService) List(ctx context.Context, principal identity.Principal) ([]host.Host, error) {
	if err := validatePrincipal(principal); err != nil {
		return nil, err
	}
	if principal.Kind == identity.KindAPIKey && !principal.HasScope(identity.ScopeHostRead) {
		return nil, domain.NewError(domain.CodeForbidden, "API key scope is insufficient")
	}
	var values []host.Host
	var err error
	if principal.Role == identity.RoleAdmin {
		values, err = s.hosts.ListHosts(ctx)
	} else {
		values, err = s.hosts.ListHostsByOwner(ctx, principal.UserID)
	}
	if err != nil {
		return nil, err
	}
	result := make([]host.Host, 0, len(values))
	for _, value := range values {
		if value.State == host.StateDeleted || value.DeletedAt != nil {
			continue
		}
		if principal.Kind == identity.KindAPIKey && !principal.AllowsHost(value.ID) {
			continue
		}
		result = append(result, value)
	}
	return result, nil
}

type CapacitySnapshot struct {
	Limit        Capacity
	Allocated    ResourceUsage
	HostsByState map[host.State]int
}

func (s *hostService) Capacity(ctx context.Context, principal identity.Principal) (CapacitySnapshot, error) {
	if err := authorizeAdmin(principal); err != nil {
		return CapacitySnapshot{}, err
	}
	values, err := s.hosts.ListHosts(ctx)
	if err != nil {
		return CapacitySnapshot{}, err
	}
	allocated := allocatedUsage(values)
	byState := make(map[host.State]int)
	for _, value := range values {
		byState[value.State]++
	}
	return CapacitySnapshot{
		Limit: s.capacity, Allocated: allocated, HostsByState: byState,
	}, nil
}

func (s *hostService) Get(ctx context.Context, principal identity.Principal, hostID string) (host.Host, error) {
	if err := authorizeHostScope(principal, hostID, identity.ScopeHostRead); err != nil {
		return host.Host{}, err
	}
	value, err := s.getAuthorizedHost(ctx, principal, hostID)
	if err != nil {
		return host.Host{}, err
	}
	return value, nil
}

func (s *hostService) BindWorkspace(ctx context.Context, principal identity.Principal, hostID, workspaceID string, expectedRevision *int64, expectedBindingVersion int64, idempotencyKey string) (workspace.Binding, error) {
	if principal.Kind != identity.KindSession {
		return workspace.Binding{}, domain.NewError(domain.CodeForbidden, "portal session required")
	}
	if err := authorizeHostScope(principal, hostID, identity.ScopeHostManage); err != nil {
		return workspace.Binding{}, err
	}
	if !domain.ValidWorkspaceID(workspaceID) || expectedBindingVersion < 0 {
		return workspace.Binding{}, domain.InvalidArgument("workspace_id", "workspace binding request is invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return workspace.Binding{}, err
	}
	hostValue, err := s.getAuthorizedHost(ctx, principal, hostID)
	if err != nil {
		return workspace.Binding{}, err
	}
	if hostValue.State != host.StateRunning {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceBusy, "host must be running to bind a workspace")
	}
	value, err := s.workspaces.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return workspace.Binding{}, err
	}
	if value.OwnerID != principal.UserID || value.State != workspace.StateReady || value.DeletedAt != nil {
		return workspace.Binding{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	if expectedRevision != nil && (*expectedRevision < 0 || *expectedRevision != value.Revision) {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace changed before binding")
	}
	current, err := s.workspaces.GetWorkspaceBinding(ctx, hostID, workspaceID)
	if err == nil {
		if current.OwnerID != principal.UserID {
			return workspace.Binding{}, domain.NewError(domain.CodeNotFound, "workspace binding not found")
		}
		if current.BindingVersion < expectedBindingVersion {
			return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace binding version changed; refresh and retry")
		}
		delta := current.BindingVersion - expectedBindingVersion
		switch current.State {
		case workspace.BindingActive, workspace.BindingPreparing:
			if delta > 2 || (current.State == workspace.BindingPreparing && delta > 1) {
				return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace binding version changed; refresh and retry")
			}
			if current.DesiredRevision == value.Revision {
				return current, nil
			}
			return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace binding is still catching up")
		case workspace.BindingUnbinding:
			return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace unbinding is still in progress")
		case workspace.BindingDetached, workspace.BindingFailed:
			if delta != 0 {
				return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace binding version changed; refresh and retry")
			}
			return s.workspaces.TransitionWorkspaceBinding(ctx, hostID, workspaceID, current.BindingVersion,
				current.State, workspace.BindingPreparing, current.DesiredRevision, current.AppliedRevision)
		default:
			return workspace.Binding{}, domain.NewError(domain.CodeInvalidState, "workspace binding state is invalid")
		}
	}
	if domain.CodeOf(err) != domain.CodeNotFound {
		return workspace.Binding{}, err
	}
	if expectedBindingVersion != 0 {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace binding version changed; refresh and retry")
	}
	created, err := s.workspaces.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: principal.UserID, HostID: hostID, Workspace: value, Directory: workspaceID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: value.Revision,
	})
	if domain.CodeOf(err) == domain.CodeWorkspaceBusy {
		current, lookupErr := s.workspaces.GetWorkspaceBinding(ctx, hostID, workspaceID)
		if lookupErr == nil && current.OwnerID == principal.UserID && current.DesiredRevision == value.Revision {
			delta := current.BindingVersion
			if current.State == workspace.BindingPreparing && delta == 1 || current.State == workspace.BindingActive && delta <= 2 {
				return current, nil
			}
		}
	}
	return created, err
}

func (s *hostService) UnbindWorkspace(ctx context.Context, principal identity.Principal, hostID, workspaceID string, expectedBindingVersion int64, writersStopped bool, idempotencyKey string) (workspace.Binding, error) {
	if principal.Kind != identity.KindSession {
		return workspace.Binding{}, domain.NewError(domain.CodeForbidden, "portal session required")
	}
	if err := authorizeHostScope(principal, hostID, identity.ScopeHostManage); err != nil {
		return workspace.Binding{}, err
	}
	if !domain.ValidWorkspaceID(workspaceID) || expectedBindingVersion < 1 {
		return workspace.Binding{}, domain.InvalidArgument("workspace_id", "workspace binding request is invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return workspace.Binding{}, err
	}
	if !writersStopped {
		return workspace.Binding{}, domain.InvalidArgument("writers_stopped", "stop AI tools, IDEs, and other workspace writers before confirming unbinding")
	}
	hostValue, err := s.getAuthorizedHost(ctx, principal, hostID)
	if err != nil {
		return workspace.Binding{}, err
	}
	if hostValue.State != host.StateRunning {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceBusy, "host must be running to safely unbind a workspace")
	}
	current, err := s.workspaces.GetWorkspaceBinding(ctx, hostID, workspaceID)
	if err != nil {
		return workspace.Binding{}, err
	}
	if current.OwnerID != principal.UserID {
		return workspace.Binding{}, domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	if current.BindingVersion < expectedBindingVersion {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace binding version changed; refresh and retry")
	}
	delta := current.BindingVersion - expectedBindingVersion
	switch current.State {
	case workspace.BindingDetached, workspace.BindingUnbinding:
		if delta > 2 {
			return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace binding version changed; refresh and retry")
		}
		return current, nil
	case workspace.BindingPreparing, workspace.BindingActive, workspace.BindingFailed:
		if delta != 0 {
			return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace binding version changed; refresh and retry")
		}
		return s.workspaces.TransitionWorkspaceBinding(ctx, hostID, workspaceID, current.BindingVersion,
			current.State, workspace.BindingUnbinding, current.DesiredRevision, current.AppliedRevision)
	default:
		return workspace.Binding{}, domain.NewError(domain.CodeInvalidState, "workspace binding state is invalid")
	}
}

func (s *hostService) RetryWorkspaceSync(ctx context.Context, principal identity.Principal, hostID, workspaceID string, expectedBindingVersion int64) (workspace.Binding, error) {
	if principal.Kind != identity.KindSession {
		return workspace.Binding{}, domain.NewError(domain.CodeForbidden, "portal session required")
	}
	if err := authorizeHostScope(principal, hostID, identity.ScopeHostManage); err != nil {
		return workspace.Binding{}, err
	}
	if !domain.ValidWorkspaceID(workspaceID) || expectedBindingVersion < 1 {
		return workspace.Binding{}, domain.InvalidArgument("expected_binding_version", "workspace sync retry request is invalid")
	}
	hostValue, err := s.getAuthorizedHost(ctx, principal, hostID)
	if err != nil {
		return workspace.Binding{}, err
	}
	if hostValue.State != host.StateRunning {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceBusy, "host must be running to retry workspace sync")
	}
	binding, err := s.workspaces.GetWorkspaceBinding(ctx, hostID, workspaceID)
	if err != nil {
		return workspace.Binding{}, err
	}
	if binding.OwnerID != principal.UserID {
		return workspace.Binding{}, domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	if binding.BindingVersion != expectedBindingVersion {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace binding version changed; refresh and retry")
	}
	if binding.State != workspace.BindingActive {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace binding must be active to retry sync")
	}
	if binding.SyncState == workspace.BindingSyncHealthy {
		return binding, nil
	}
	if binding.SyncState != workspace.BindingSyncConflict {
		return workspace.Binding{}, domain.NewError(domain.CodeInvalidState, "workspace sync status cannot be retried")
	}
	store, ok := s.workspaces.(workspaceBindingSyncStatusStore)
	if !ok {
		return workspace.Binding{}, domain.NewError(domain.CodeUnsupported, "workspace sync retry is unavailable")
	}
	return store.SetWorkspaceBindingSyncStatus(ctx, hostID, workspaceID, expectedBindingVersion, workspace.BindingSyncHealthy, nil)
}

// GetForComputer authorizes a computer action against its own narrow scope.
// Computer observation/operation must not imply the separate host:read scope.
func (s *hostService) GetForComputer(ctx context.Context, principal identity.Principal, hostID string, scope identity.Scope) (host.Host, error) {
	if principal.Kind != identity.KindAPIKey || (scope != identity.ScopeComputerObserve && scope != identity.ScopeComputerOperate) {
		return host.Host{}, domain.NewError(domain.CodeForbidden, "computer scope is insufficient")
	}
	if !principal.HasScope(scope) {
		return host.Host{}, domain.NewError(domain.CodeForbidden, "API key scope is insufficient")
	}
	if !principal.AllowsHost(hostID) {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	value, err := s.getAuthorizedHost(ctx, principal, hostID)
	if err != nil {
		return host.Host{}, err
	}
	return value, nil
}

type mcpComputerHostAuthorizer struct {
	hosts   HostStore
	sharing HostPermissionAuthorizer
}

func NewMCPComputerHostAuthorizer(hosts HostStore, sharing HostPermissionAuthorizer) (MCPComputerHostAuthorizer, error) {
	if hosts == nil || sharing == nil {
		return nil, fmt.Errorf("MCP Host authorization dependencies are required")
	}
	return &mcpComputerHostAuthorizer{hosts: hosts, sharing: sharing}, nil
}

func (s *mcpComputerHostAuthorizer) GetForComputer(ctx context.Context, principal identity.Principal, hostID string, scope identity.Scope) (host.Host, error) {
	if principal.Kind != identity.KindAPIKey || (scope != identity.ScopeComputerObserve && scope != identity.ScopeComputerOperate) {
		return host.Host{}, domain.NewError(domain.CodeForbidden, "computer scope is insufficient")
	}
	if !principal.HasScope(scope) {
		return host.Host{}, domain.NewError(domain.CodeForbidden, "API key scope is insufficient")
	}
	if !principal.AllowsHost(hostID) {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err := validatePrincipal(principal); err != nil {
		return host.Host{}, err
	}
	if !domain.ValidHostID(hostID) {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	value, err := s.hosts.GetHost(ctx, hostID)
	if err != nil {
		return host.Host{}, err
	}
	if value.State == host.StateDeleted || value.DeletedAt != nil {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if value.OwnerID != principal.UserID {
		if _, err := s.sharing.Authorize(ctx, hostID, principal.UserID, host.PermissionMCPControl); err != nil {
			return host.Host{}, err
		}
	}
	return value, nil
}

func (s *hostService) AuthorizeTLS(ctx context.Context, hostID string) bool {
	if !domain.ValidHostID(hostID) {
		return false
	}
	value, err := s.hosts.GetHost(ctx, hostID)
	if err != nil {
		if domain.CodeOf(err) != domain.CodeNotFound {
			s.logger.Warn("authorize host certificate lookup failed", zap.String("host_id", hostID), zap.Error(err))
		}
		return false
	}
	return value.State == host.StateRunning
}

func (s *hostService) Delete(ctx context.Context, principal identity.Principal, command DeleteHostCommand) (host.Host, error) {
	if err := authorizeHostScope(principal, command.HostID, identity.ScopeHostManage); err != nil {
		return host.Host{}, err
	}
	value, err := s.getAuthorizedHost(ctx, principal, command.HostID)
	if err != nil {
		return host.Host{}, err
	}
	if err := rejectHostCredential(principal); err != nil {
		return host.Host{}, err
	}
	if value.RuntimeGeneration > 1 && (value.StateReason == "restore_unknown" ||
		(value.State == host.StateCreating && value.CreateAttemptedAt != nil)) {
		return host.Host{}, domain.NewError(domain.CodeConflict, "restore create outcome is uncertain; contact an operator before deleting this host")
	}
	bindings, err := s.workspaces.GetWorkspaceBindingsByHost(ctx, value.ID)
	if err != nil {
		return host.Host{}, err
	}
	for _, binding := range bindings {
		if binding.State != workspace.BindingDetached {
			return host.Host{}, domain.NewError(domain.CodeWorkspaceBusy, "unbind and verify all workspaces before deleting this host")
		}
	}
	if value.State == host.StateDeleting {
		return value, nil
	}
	if err := host.ValidateTransition(value.State, host.StateDeleting); err != nil {
		return host.Host{}, err
	}
	updated, err := s.hosts.TransitionHost(ctx, value.ID, value.State, host.StateDeleting, "")
	if err != nil {
		return host.Host{}, err
	}
	s.writeAudit(ctx, audit.Entry{
		ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind),
		Action: audit.ActionHostDelete, Target: value.ID,
		Detail: map[string]any{"workspace_ids": workspaceIDs(value.Workspaces)},
	})
	return updated, nil
}

// Release stops only the Host's compute. The Host identity, Workspace
// attachment, and deterministic runtime data volume remain available to restore.
func (s *hostService) Release(ctx context.Context, principal identity.Principal, hostID string, confirmProcessTermination bool) (host.Host, error) {
	if err := authorizeHostScope(principal, hostID, identity.ScopeHostManage); err != nil {
		return host.Host{}, err
	}
	value, err := s.getAuthorizedHost(ctx, principal, hostID)
	if err != nil {
		return host.Host{}, err
	}
	if err := rejectHostCredential(principal); err != nil {
		return host.Host{}, err
	}
	updated, changed, err := s.hosts.BeginHostRelease(ctx, value.ID, confirmProcessTermination)
	if err != nil {
		return host.Host{}, err
	}
	if !changed && updated.State == host.StateRunning {
		return host.Host{}, domain.NewError(domain.CodeConflict, "host has active tasks, sessions, or is under user control")
	}
	if changed {
		s.writeAudit(ctx, audit.Entry{
			ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind),
			Action: audit.ActionHostRelease, Target: updated.ID,
			Detail: map[string]any{"runtime_generation": updated.RuntimeGeneration},
		})
	}
	return updated, nil
}

// Restore reuses the stable Host and data volumes, advancing exactly one
// runtime generation under the store's transaction lock.
func (s *hostService) Restore(ctx context.Context, principal identity.Principal, hostID string) (host.Host, error) {
	if err := authorizeHostScope(principal, hostID, identity.ScopeHostManage); err != nil {
		return host.Host{}, err
	}
	value, err := s.getAuthorizedHost(ctx, principal, hostID)
	if err != nil {
		return host.Host{}, err
	}
	if err := rejectHostCredential(principal); err != nil {
		return host.Host{}, err
	}
	if value.State == host.StateRunning {
		return value, nil
	}
	if value.State == host.StateCreating && value.RuntimeGeneration > 1 &&
		(value.StateReason == "restore_pending" || value.StateReason == "restore_unknown") {
		if value.StateReason == "restore_pending" {
			if err := s.notifyCreate(ctx, value.ID); err != nil {
				s.logger.Error("notify host restore worker failed", zap.String("host_id", value.ID), zap.Error(err))
			}
		}
		return value, nil
	}
	if value.State != host.StateStopped {
		return host.Host{}, invalidHostState(value.State)
	}
	tmpl, ok := s.templates[value.TemplateID]
	if !ok {
		return host.Host{}, domain.NewError(domain.CodeInternal, "host template is unavailable")
	}
	if err := s.diskCapacity.CheckCapacity(ctx); err != nil {
		s.logger.Warn("host restore blocked by storage capacity check", zap.String("host_id", value.ID), zap.Error(err))
		if domain.CodeOf(err) == domain.CodeCapacityExceeded {
			return host.Host{}, err
		}
		return host.Host{}, domain.NewError(domain.CodeUpstreamError, "host storage capacity is unavailable")
	}
	credentialBytes := make([]byte, 32)
	if _, err := rand.Read(credentialBytes); err != nil {
		return host.Host{}, fmt.Errorf("generate host runtime credential: %w", err)
	}
	credential := base64.RawURLEncoding.EncodeToString(credentialBytes)
	clear(credentialBytes)
	sealedCredential, err := s.sealer.Seal([]byte(credential))
	if err != nil {
		return host.Host{}, fmt.Errorf("seal host runtime credential: %w", err)
	}
	updated, changed, err := s.hosts.BeginHostRestore(ctx, RestoreHostParams{
		HostID: value.ID, RuntimeCredentialEnc: sealedCredential,
		ExpiresAt: time.Now().UTC().Add(tmpl.Lifecycle.TTL), Capacity: s.capacity,
	})
	if err != nil {
		return host.Host{}, err
	}
	if changed {
		s.writeAudit(ctx, audit.Entry{
			ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind),
			Action: audit.ActionHostRestore, Target: updated.ID,
			Detail: map[string]any{"runtime_generation": updated.RuntimeGeneration},
		})
	}
	if updated.State == host.StateCreating && updated.StateReason == "restore_pending" {
		if err := s.notifyCreate(ctx, updated.ID); err != nil {
			s.logger.Error("notify host restore worker failed", zap.String("host_id", updated.ID), zap.Error(err))
		}
	}
	return updated, nil
}

func (s *hostService) Renew(ctx context.Context, principal identity.Principal, hostID string) (host.Host, error) {
	if err := authorizeHostScope(principal, hostID, identity.ScopeHostManage); err != nil {
		return host.Host{}, err
	}
	value, err := s.getAuthorizedHost(ctx, principal, hostID)
	if err != nil {
		return host.Host{}, err
	}
	if err := rejectHostCredential(principal); err != nil {
		return host.Host{}, err
	}
	if value.State != host.StateRunning {
		return host.Host{}, invalidHostState(value.State)
	}
	tmpl, ok := s.templates[value.TemplateID]
	if !ok {
		return host.Host{}, domain.NewError(domain.CodeInternal, "host template is unavailable")
	}
	client, err := s.sandboxes.For(value.RuntimeProfile)
	if err != nil {
		return host.Host{}, sandboxOperationError("select sandbox profile", err)
	}
	expectedExpiry := time.Now().UTC().Add(tmpl.Lifecycle.TTL)
	expiresAt, err := client.Renew(ctx, value.SandboxID, expectedExpiry)
	if err != nil {
		return host.Host{}, sandboxOperationError("renew sandbox", err)
	}
	updated, err := s.hosts.SetHostExpiry(ctx, value.ID, value.State, expiresAt)
	if err != nil {
		return host.Host{}, err
	}
	s.writeAudit(ctx, audit.Entry{
		ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind),
		Action: audit.ActionHostRenew, Target: value.ID,
		Detail: map[string]any{"expires_at": expiresAt.UTC().Format(time.RFC3339Nano)},
	})
	return updated, nil
}

func (s *hostService) IssueTicket(ctx context.Context, principal identity.Principal, hostID string) (EnterInfo, error) {
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return EnterInfo{}, domain.NewError(domain.CodeForbidden, "portal session required")
	}
	value, err := s.getAuthorizedHost(ctx, principal, hostID)
	if err != nil {
		return EnterInfo{}, err
	}
	if value.OwnerID != principal.UserID {
		return EnterInfo{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if value.State != host.StateRunning {
		return EnterInfo{}, invalidHostState(value.State)
	}
	expiresAt := time.Now().UTC().Add(s.ticketTTL)
	var credential string
	if principal.Kind == identity.KindClient {
		_, credential, err = issueClientTicket(ctx, s.tokens, principal.UserID, principal.UserID, value.ID, principal.ClientInstallationID, expiresAt)
	} else {
		_, credential, err = issueCredential(ctx, s.tokens, principal.UserID, identity.KindTicket, value.ID, "", expiresAt)
	}
	if err != nil {
		return EnterInfo{}, err
	}
	query := url.Values{"t": []string{credential}}
	enterURL := url.URL{
		Scheme: s.scheme, Host: hostURLHost(value.ID, s.hostSuffix),
		Path: "/__ch/auth", RawQuery: query.Encode(),
	}
	s.writeAudit(ctx, audit.Entry{
		ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind),
		Action: audit.ActionHostEnter, Target: value.ID,
	})
	return EnterInfo{URL: enterURL.String(), ExpiresAt: expiresAt}, nil
}

func (s *hostService) IssuePreviewTicket(ctx context.Context, principal identity.Principal, hostID string, port int) (EnterInfo, error) {
	if principal.Kind != identity.KindSession {
		return EnterInfo{}, domain.NewError(domain.CodeForbidden, "portal session required")
	}
	if !domain.ValidPreviewTargetPort(port) {
		return EnterInfo{}, domain.InvalidArgument("port", "preview port is invalid")
	}
	value, err := s.getAuthorizedHost(ctx, principal, hostID)
	if err != nil {
		return EnterInfo{}, err
	}
	if value.OwnerID != principal.UserID {
		return EnterInfo{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if value.State != host.StateRunning {
		return EnterInfo{}, invalidHostState(value.State)
	}
	expiresAt := time.Now().UTC().Add(s.ticketTTL)
	_, credential, err := issueCredentialWithPreviewPort(ctx, s.tokens, principal.UserID, identity.KindTicket, value.ID, "", expiresAt, nil, nil, port)
	if err != nil {
		return EnterInfo{}, err
	}
	query := url.Values{"t": []string{credential}}
	enterURL := url.URL{Scheme: s.scheme, Host: fmt.Sprintf("%d-%s", port, hostURLHost(value.ID, s.hostSuffix)), Path: "/__ch/auth", RawQuery: query.Encode()}
	s.writeAudit(ctx, audit.Entry{ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind), Action: audit.ActionHostEnter,
		Target: hostID, Detail: map[string]any{"mode": "preview", "preview_port": port}})
	return EnterInfo{URL: enterURL.String(), ExpiresAt: expiresAt}, nil
}

func (s *hostService) getAuthorizedHost(ctx context.Context, principal identity.Principal, hostID string) (host.Host, error) {
	if err := validatePrincipal(principal); err != nil {
		return host.Host{}, err
	}
	if !domain.ValidHostID(hostID) {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	value, err := s.hosts.GetHost(ctx, hostID)
	if err != nil {
		return host.Host{}, err
	}
	if value.State == host.StateDeleted || value.DeletedAt != nil ||
		(principal.Role != identity.RoleAdmin && value.OwnerID != principal.UserID) ||
		(principal.HostScope != "" && principal.HostScope != value.ID) ||
		(principal.Kind == identity.KindAPIKey && !principal.AllowsHost(value.ID)) {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	return value, nil
}

func authorizeHostScope(principal identity.Principal, hostID string, scope identity.Scope) error {
	if principal.Kind != identity.KindAPIKey {
		return nil
	}
	if !principal.HasScope(scope) {
		return domain.NewError(domain.CodeForbidden, "API key scope is insufficient")
	}
	if !principal.AllowsHost(hostID) {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	return nil
}

func (s *hostService) detachHostWorkspaces(ctx context.Context, hostID string) error {
	bindings, err := s.workspaces.GetWorkspaceBindingsByHost(ctx, hostID)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		switch binding.State {
		case workspace.BindingDetached:
			continue
		case workspace.BindingPreparing, workspace.BindingActive, workspace.BindingFailed:
			binding, err = s.workspaces.TransitionWorkspaceBinding(ctx, hostID, binding.Workspace.ID, binding.BindingVersion, binding.State, workspace.BindingUnbinding, binding.DesiredRevision, binding.AppliedRevision)
			if err != nil {
				return fmt.Errorf("begin workspace unbinding %s: %w", binding.Workspace.ID, err)
			}
		case workspace.BindingUnbinding:
		default:
			return domain.NewError(domain.CodeInvalidState, "workspace binding state is invalid")
		}
		if binding.State == workspace.BindingUnbinding {
			if _, err := s.workspaces.TransitionWorkspaceBinding(ctx, hostID, binding.Workspace.ID, binding.BindingVersion, workspace.BindingUnbinding, workspace.BindingDetached, binding.DesiredRevision, binding.AppliedRevision); err != nil {
				return fmt.Errorf("complete workspace unbinding %s: %w", binding.Workspace.ID, err)
			}
		}
	}
	return nil
}

func workspaceIDs(bindings []workspace.Binding) []string {
	ids := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		if binding.State != workspace.BindingDetached {
			ids = append(ids, binding.Workspace.ID)
		}
	}
	return ids
}

func (s *hostService) hostURL(hostID string) string {
	return s.scheme + "://" + hostURLHost(hostID, s.hostSuffix)
}

func hostURLHost(hostID, suffix string) string {
	return hostID + "." + strings.TrimSuffix(suffix, ".")
}

func invalidHostState(state host.State) error {
	err := domain.NewError(domain.CodeInvalidState, "host state does not allow this operation")
	err.Details["state"] = string(state)
	return err
}

func sandboxOperationError(operation string, err error) error {
	switch {
	case errors.Is(err, ErrSandboxUnsupported):
		return domain.NewError(domain.CodeUnsupported, "sandbox operation unsupported")
	case errors.Is(err, ErrSandboxNotFound):
		return domain.NewError(domain.CodeNotFound, "sandbox not found")
	case errors.Is(err, ErrSandboxConflict):
		return domain.NewError(domain.CodeInvalidState, "sandbox state changed")
	default:
		return fmt.Errorf("%s: %w", operation, domain.NewError(domain.CodeUpstreamError, "sandbox service unavailable"))
	}
}

func hostStateError(state host.State) error {
	return invalidHostState(state)
}

var _ HostService = (*hostService)(nil)
