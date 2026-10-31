package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

const hostReplicaCredentialContext = "morde-host-workspace-replica-v1"

type hostReplicaService struct {
	hosts      HostStore
	workspaces WorkspaceStore
	users      UserStore
	sealer     *Sealer
}

func NewHostReplicaService(hosts HostStore, workspaces WorkspaceStore, users UserStore, sealer *Sealer) (HostReplicaService, error) {
	if hosts == nil || workspaces == nil || users == nil || sealer == nil {
		return nil, errors.New("host replica dependencies are required")
	}
	return &hostReplicaService{hosts: hosts, workspaces: workspaces, users: users, sealer: sealer}, nil
}

func (s *hostReplicaService) Authenticate(ctx context.Context, hostID string, generation int64, raw string) (identity.Principal, error) {
	if !domain.ValidHostID(hostID) || generation < 1 || len(raw) != 43 || strings.ContainsAny(raw, "\r\n") {
		return identity.Principal{}, domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
	}
	principal, value, err := s.loadPrincipal(ctx, hostID, generation)
	if err != nil {
		return identity.Principal{}, err
	}
	credential, err := s.sealer.Open(value.RuntimeCredentialEnc)
	if err != nil {
		return identity.Principal{}, fmt.Errorf("open Host replica verifier: %w", err)
	}
	expected := hostReplicaCredential(credential)
	clear(credential)
	if !hmac.Equal([]byte(raw), []byte(expected)) {
		return identity.Principal{}, domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
	}
	return principal, nil
}

func (s *hostReplicaService) AuthenticateWorkspace(ctx context.Context, raw string) (identity.Principal, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 6 || parts[0] != "morde-replica-v1" {
		return identity.Principal{}, domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
	}
	generation, generationErr := strconv.ParseInt(parts[2], 10, 64)
	bindingVersion, versionErr := strconv.ParseInt(parts[4], 10, 64)
	if generationErr != nil || versionErr != nil || generation < 1 || bindingVersion < 1 ||
		!domain.ValidHostID(parts[1]) || !domain.ValidWorkspaceID(parts[3]) {
		return identity.Principal{}, domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
	}
	principal, value, err := s.loadPrincipal(ctx, parts[1], generation)
	if err != nil {
		return identity.Principal{}, err
	}
	credential, err := s.sealer.Open(value.RuntimeCredentialEnc)
	if err != nil {
		return identity.Principal{}, fmt.Errorf("open Host replica verifier: %w", err)
	}
	macKey, err := base64.RawURLEncoding.DecodeString(hostReplicaCredential(credential))
	clear(credential)
	if err != nil {
		return identity.Principal{}, fmt.Errorf("derive Host replica verifier: %w", err)
	}
	mac := hmac.New(sha256.New, macKey)
	clear(macKey)
	_, _ = mac.Write([]byte(strings.Join(parts[:5], ".")))
	provided, decodeErr := base64.RawURLEncoding.DecodeString(parts[5])
	if decodeErr != nil || !hmac.Equal(provided, mac.Sum(nil)) {
		return identity.Principal{}, domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
	}
	principal, err = s.ScopeWorkspace(ctx, principal, parts[3], bindingVersion)
	return principal, err
}

func (s *hostReplicaService) ScopeWorkspace(ctx context.Context, principal identity.Principal, workspaceID string, bindingVersion int64) (identity.Principal, error) {
	if principal.Kind != identity.KindHostReplica || !domain.ValidHostID(principal.HostScope) || !domain.ValidUserID(principal.UserID) ||
		principal.HostGeneration < 1 || !domain.ValidWorkspaceID(workspaceID) || bindingVersion < 1 {
		return identity.Principal{}, domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
	}
	if err := s.verifyPrincipal(ctx, principal); err != nil {
		return identity.Principal{}, err
	}
	binding, err := s.workspaces.GetWorkspaceBinding(ctx, principal.HostScope, workspaceID)
	if err != nil {
		return identity.Principal{}, err
	}
	if binding.OwnerID != principal.UserID {
		return identity.Principal{}, domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	if binding.BindingVersion != bindingVersion {
		return identity.Principal{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace binding version changed; refresh and retry")
	}
	if binding.State != workspace.BindingPreparing && binding.State != workspace.BindingActive && binding.State != workspace.BindingUnbinding {
		return identity.Principal{}, domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	principal.ReplicaWorkspaceID = workspaceID
	principal.BindingVersion = bindingVersion
	return principal, nil
}

func (s *hostReplicaService) ListBindings(ctx context.Context, principal identity.Principal) ([]workspace.Binding, error) {
	if err := s.verifyPrincipal(ctx, principal); err != nil {
		return nil, err
	}
	bindings, err := s.workspaces.GetWorkspaceBindingsByHost(ctx, principal.HostScope)
	if err != nil {
		return nil, err
	}
	for _, binding := range bindings {
		if binding.OwnerID != principal.UserID || binding.HostID != principal.HostScope {
			return nil, domain.NewError(domain.CodeInternal, "Host workspace binding is inconsistent")
		}
	}
	return bindings, nil
}

func (s *hostReplicaService) CredentialForBinding(ctx context.Context, principal identity.Principal, workspaceID string, bindingVersion int64) (string, error) {
	principal, err := s.ScopeWorkspace(ctx, principal, workspaceID, bindingVersion)
	if err != nil {
		return "", err
	}
	value, err := s.hosts.GetHost(ctx, principal.HostScope)
	if err != nil {
		return "", err
	}
	credential, err := s.sealer.Open(value.RuntimeCredentialEnc)
	if err != nil {
		return "", fmt.Errorf("open Host replica verifier: %w", err)
	}
	macKey, err := base64.RawURLEncoding.DecodeString(hostReplicaCredential(credential))
	clear(credential)
	if err != nil {
		return "", fmt.Errorf("derive Host replica verifier: %w", err)
	}
	payload := fmt.Sprintf("morde-replica-v1.%s.%d.%s.%d", principal.HostScope, principal.HostGeneration, workspaceID, bindingVersion)
	mac := hmac.New(sha256.New, macKey)
	clear(macKey)
	_, _ = mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *hostReplicaService) RecordAppliedRevision(ctx context.Context, principal identity.Principal, workspaceID string, bindingVersion int64, sessionID string, revision int64) (workspace.Binding, error) {
	if principal.ReplicaWorkspaceID != workspaceID || principal.BindingVersion != bindingVersion || !workspace.ValidSessionID(sessionID) || revision < 0 {
		return workspace.Binding{}, domain.NewError(domain.CodeUnauthenticated, "host replica binding authorization is unavailable")
	}
	if err := s.verifyPrincipal(ctx, principal); err != nil {
		return workspace.Binding{}, err
	}
	binding, err := s.workspaces.AdvanceWorkspaceBindingAppliedRevision(ctx, principal.HostScope, principal.HostGeneration,
		workspaceID, bindingVersion, sessionID, revision)
	if err != nil {
		return workspace.Binding{}, err
	}
	if binding.OwnerID != principal.UserID {
		return workspace.Binding{}, domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	return binding, nil
}

func (s *hostReplicaService) ReportWorkspaceSyncStatus(ctx context.Context, principal identity.Principal, state workspace.BindingSyncState, conflicts []string) (workspace.Binding, error) {
	if !domain.ValidWorkspaceID(principal.ReplicaWorkspaceID) || principal.BindingVersion < 1 ||
		workspace.ValidateBindingSyncStatus(state, conflicts) != nil {
		return workspace.Binding{}, domain.InvalidArgument("sync_status", "workspace sync status is invalid")
	}
	if err := s.verifyPrincipal(ctx, principal); err != nil {
		return workspace.Binding{}, err
	}
	binding, err := s.workspaces.GetWorkspaceBinding(ctx, principal.HostScope, principal.ReplicaWorkspaceID)
	if err != nil {
		return workspace.Binding{}, err
	}
	if binding.OwnerID != principal.UserID || binding.BindingVersion != principal.BindingVersion || binding.State != workspace.BindingActive {
		return workspace.Binding{}, domain.NewError(domain.CodeNotFound, "active workspace binding not found")
	}
	store, ok := s.workspaces.(HostReplicaWorkspaceStore)
	if !ok {
		return workspace.Binding{}, domain.NewError(domain.CodeUnsupported, "workspace sync status is unavailable")
	}
	return store.SetWorkspaceBindingSyncStatusForHost(ctx, principal.HostScope, principal.HostGeneration,
		principal.ReplicaWorkspaceID, principal.BindingVersion, state, conflicts)
}

func (s *hostReplicaService) ActivateBinding(ctx context.Context, principal identity.Principal, workspaceID string, bindingVersion int64) (workspace.Binding, error) {
	if principal.ReplicaWorkspaceID != workspaceID || principal.BindingVersion != bindingVersion {
		return workspace.Binding{}, domain.NewError(domain.CodeUnauthenticated, "host replica binding authorization is unavailable")
	}
	if err := s.verifyPrincipal(ctx, principal); err != nil {
		return workspace.Binding{}, err
	}
	binding, err := s.workspaces.GetWorkspaceBinding(ctx, principal.HostScope, workspaceID)
	if err != nil {
		return workspace.Binding{}, err
	}
	if binding.OwnerID != principal.UserID || binding.BindingVersion != bindingVersion {
		return workspace.Binding{}, domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	if binding.State == workspace.BindingActive && binding.AppliedRevision == binding.DesiredRevision && binding.DesiredRevision == binding.Workspace.Revision {
		return binding, nil
	}
	if binding.State != workspace.BindingPreparing {
		return workspace.Binding{}, domain.NewError(domain.CodeInvalidState, "workspace binding is not preparing")
	}
	store, ok := s.workspaces.(HostReplicaWorkspaceStore)
	if !ok {
		return workspace.Binding{}, domain.NewError(domain.CodeUnsupported, "workspace binding activation is unavailable")
	}
	return store.ActivateWorkspaceBindingForHost(ctx, principal.HostScope, principal.HostGeneration, workspaceID,
		bindingVersion, binding.DesiredRevision, binding.AppliedRevision)
}

func (s *hostReplicaService) CompleteUnbinding(ctx context.Context, principal identity.Principal, workspaceID string, bindingVersion, checkpointRevision int64) (workspace.Binding, error) {
	if principal.ReplicaWorkspaceID != workspaceID || principal.BindingVersion != bindingVersion || checkpointRevision < 0 {
		return workspace.Binding{}, domain.NewError(domain.CodeUnauthenticated, "host replica binding authorization is unavailable")
	}
	if err := s.verifyPrincipal(ctx, principal); err != nil {
		return workspace.Binding{}, err
	}
	binding, err := s.workspaces.GetWorkspaceBinding(ctx, principal.HostScope, workspaceID)
	if err != nil {
		return workspace.Binding{}, err
	}
	if binding.OwnerID != principal.UserID || binding.BindingVersion != bindingVersion || binding.State != workspace.BindingUnbinding {
		return workspace.Binding{}, domain.NewError(domain.CodeNotFound, "workspace binding is not unbinding")
	}
	if binding.Workspace.Revision != checkpointRevision || binding.DesiredRevision != checkpointRevision || binding.AppliedRevision != checkpointRevision {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace changed after the Host checkpoint")
	}
	return s.workspaces.CompleteWorkspaceBindingUnbind(ctx, principal.HostScope, workspaceID, bindingVersion, principal.HostGeneration, checkpointRevision)
}

func (s *hostReplicaService) verifyPrincipal(ctx context.Context, principal identity.Principal) error {
	if principal.Kind != identity.KindHostReplica || !domain.ValidHostID(principal.HostScope) || !domain.ValidUserID(principal.UserID) || principal.HostGeneration < 1 {
		return domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
	}
	hostValue, err := s.hosts.GetHost(ctx, principal.HostScope)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
		}
		return fmt.Errorf("load Host replica identity: %w", err)
	}
	if hostValue.OwnerID != principal.UserID || hostValue.RuntimeGeneration != principal.HostGeneration || !replicaHostState(hostValue.State) || hostValue.DeletedAt != nil {
		return domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
	}
	user, err := s.users.GetUserByID(ctx, principal.UserID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return domain.NewError(domain.CodeUnauthenticated, "host replica owner is unavailable")
		}
		return fmt.Errorf("load Host replica owner: %w", err)
	}
	if user.Status != identity.StatusApproved {
		return domain.NewError(domain.CodeUnauthenticated, "host replica owner is unavailable")
	}
	return nil
}

func (s *hostReplicaService) loadPrincipal(ctx context.Context, hostID string, generation int64) (identity.Principal, host.Host, error) {
	value, err := s.hosts.GetHost(ctx, hostID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return identity.Principal{}, host.Host{}, domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
		}
		return identity.Principal{}, host.Host{}, fmt.Errorf("load Host replica identity: %w", err)
	}
	if value.RuntimeGeneration != generation || !replicaHostState(value.State) || value.DeletedAt != nil || len(value.RuntimeCredentialEnc) == 0 {
		return identity.Principal{}, host.Host{}, domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
	}
	user, err := s.users.GetUserByID(ctx, value.OwnerID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return identity.Principal{}, host.Host{}, domain.NewError(domain.CodeUnauthenticated, "host replica owner is unavailable")
		}
		return identity.Principal{}, host.Host{}, fmt.Errorf("load Host replica owner: %w", err)
	}
	if user.Status != identity.StatusApproved {
		return identity.Principal{}, host.Host{}, domain.NewError(domain.CodeUnauthenticated, "host replica owner is unavailable")
	}
	return identity.Principal{
		UserID: value.OwnerID, Role: identity.RoleUser, Kind: identity.KindHostReplica,
		HostScope: value.ID, HostGeneration: value.RuntimeGeneration,
	}, value, nil
}

func hostReplicaCredential(runtimeCredential []byte) string {
	mac := hmac.New(sha256.New, runtimeCredential)
	_, _ = mac.Write([]byte(hostReplicaCredentialContext))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func replicaHostState(state host.State) bool {
	return state == host.StateCreating || state == host.StateRunning
}

var _ HostReplicaService = (*hostReplicaService)(nil)
