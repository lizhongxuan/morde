package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type CreateWorkspaceCommand struct {
	Name string
}

type DeviceWorkspaceInfo struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Revision int64           `json:"revision"`
	State    workspace.State `json:"state"`
}

type WorkspaceContentDeleter interface {
	DeleteWorkspace(string) error
}

type WorkspaceCurrentDeleter interface {
	Delete(context.Context, string, string) error
}

type WorkspaceCurrentInitializer func(context.Context, string) error

type WorkspaceCapacityChecker interface {
	CheckCapacity(context.Context) error
}

type WorkspaceService struct {
	store                WorkspaceStore
	content              WorkspaceContentDeleter
	current              WorkspaceCurrentDeleter
	initializeCurrent    WorkspaceCurrentInitializer
	capacity             WorkspaceCapacityChecker
	audit                AuditWriter
	maxWorkspaceBytes    int64
	hostWorkspaceVolumes *WorkspaceVolumeService
}

func NewWorkspaceService(store WorkspaceStore, content WorkspaceContentDeleter, current WorkspaceCurrentDeleter, capacity WorkspaceCapacityChecker, writer AuditWriter, maxWorkspaceBytes int64) (*WorkspaceService, error) {
	if store == nil || content == nil || current == nil || capacity == nil || writer == nil || maxWorkspaceBytes <= 0 {
		return nil, errors.New("workspace service dependencies and quotas are required")
	}
	return &WorkspaceService{
		store: store, content: content, current: current, capacity: capacity, audit: writer,
		maxWorkspaceBytes: maxWorkspaceBytes,
	}, nil
}

func (s *WorkspaceService) SetWorkspaceCurrentInitializer(initializer WorkspaceCurrentInitializer) error {
	if s == nil || initializer == nil {
		return errors.New("workspace current initializer is required")
	}
	s.initializeCurrent = initializer
	return nil
}

func (s *WorkspaceService) SetHostWorkspaceVolumes(volumes *WorkspaceVolumeService) error {
	if s == nil || volumes == nil {
		return errors.New("Host workspace volume service is required")
	}
	s.hostWorkspaceVolumes = volumes
	return nil
}

func (s *WorkspaceService) CreateWorkspace(ctx context.Context, principal identity.Principal, command CreateWorkspaceCommand, idempotencyKey string) (workspace.Workspace, error) {
	if principal.Kind != identity.KindSession {
		return workspace.Workspace{}, domain.NewError(domain.CodeForbidden, "portal session required")
	}
	if err := validatePrincipal(principal); err != nil {
		return workspace.Workspace{}, err
	}
	command.Name = strings.TrimSpace(command.Name)
	if command.Name == "" || utf8.RuneCountInString(command.Name) > 120 || !utf8.ValidString(command.Name) {
		return workspace.Workspace{}, domain.InvalidArgument("name", "workspace name must contain 1 to 120 valid characters")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return workspace.Workspace{}, err
	}
	if err := s.capacity.CheckCapacity(ctx); err != nil {
		return workspace.Workspace{}, err
	}
	id, err := domain.NewWorkspaceID()
	if err != nil {
		return workspace.Workspace{}, fmt.Errorf("generate workspace id: %w", err)
	}
	value := workspace.Workspace{ID: id, OwnerID: principal.UserID, Name: command.Name, State: workspace.StateReady, QuotaBytes: s.maxWorkspaceBytes}
	record, err := idempotencyRecord(principal.UserID, "workspace.create", idempotencyKey, struct {
		Name string `json:"name"`
	}{command.Name}, id, "")
	if err != nil {
		return workspace.Workspace{}, err
	}
	created, _, err := s.store.CreateWorkspace(ctx, value, record)
	if err != nil {
		return workspace.Workspace{}, err
	}
	if s.initializeCurrent == nil {
		return workspace.Workspace{}, errors.New("workspace current initializer is unavailable")
	}
	if err := s.initializeCurrent(ctx, created.ID); err != nil {
		return workspace.Workspace{}, fmt.Errorf("initialize Workspace current directory: %w", err)
	}
	return created, nil
}

func (s *WorkspaceService) CreateHostWorkspace(ctx context.Context, ownerID, idempotencyKey string) (workspace.Workspace, error) {
	if !domain.ValidUserID(ownerID) {
		return workspace.Workspace{}, domain.InvalidArgument("owner_id", "computer owner is invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return workspace.Workspace{}, err
	}
	digest := sha256.Sum256([]byte(ownerID + "\x00" + idempotencyKey))
	return s.CreateWorkspace(ctx, identity.Principal{UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindSession},
		CreateWorkspaceCommand{Name: "Computer storage"}, "host-workspace-"+hex.EncodeToString(digest[:]))
}

func (s *WorkspaceService) ListWorkspaces(ctx context.Context, principal identity.Principal) ([]workspace.Workspace, error) {
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return nil, domain.NewError(domain.CodeForbidden, "portal session required")
	}
	if err := validatePrincipal(principal); err != nil {
		return nil, err
	}
	values, err := s.store.ListWorkspacesByOwner(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	result := make([]workspace.Workspace, 0, len(values))
	for _, value := range values {
		if value.State != workspace.StateDeleted && value.State != workspace.StateCopying && value.DeletedAt == nil {
			result = append(result, value)
		}
	}
	return result, nil
}

func (s *WorkspaceService) EnsureDefaultWorkspace(ctx context.Context, ownerID string) (workspace.Workspace, error) {
	if !domain.ValidUserID(ownerID) {
		return workspace.Workspace{}, domain.InvalidArgument("user_id", "user id is invalid")
	}
	key, err := domain.NewWorkspaceID()
	if err != nil {
		return workspace.Workspace{}, fmt.Errorf("generate default Workspace idempotency key: %w", err)
	}
	return s.CreateWorkspace(ctx, identity.Principal{UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindSession},
		CreateWorkspaceCommand{Name: "Computer storage"}, "default-host-workspace-"+key)
}

func (s *WorkspaceService) ValidateHostWorkspace(ctx context.Context, ownerID, hostID, workspaceID string) error {
	if !domain.ValidUserID(ownerID) || !domain.ValidHostID(hostID) || !domain.ValidWorkspaceID(workspaceID) {
		return domain.InvalidArgument("host_workspace", "computer storage identity is invalid")
	}
	bindings, err := s.store.GetWorkspaceBindingsByHost(ctx, hostID)
	if err != nil {
		return fmt.Errorf("read computer storage binding before deletion: %w", err)
	}
	if len(bindings) != 1 || bindings[0].HostID != hostID || bindings[0].OwnerID != ownerID || bindings[0].Workspace.ID != workspaceID {
		return domain.NewError(domain.CodeWorkspaceConflict, "computer storage ownership is ambiguous")
	}
	allBindings, err := s.store.GetWorkspaceBindingsByWorkspace(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("read computer storage references before deletion: %w", err)
	}
	if len(allBindings) != 1 || allBindings[0].HostID != hostID || allBindings[0].OwnerID != ownerID {
		return domain.NewError(domain.CodeWorkspaceConflict, "computer storage is shared or has ambiguous references")
	}
	value, err := s.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	if value.OwnerID != ownerID {
		return domain.NewError(domain.CodeWorkspaceConflict, "computer storage owner changed")
	}
	if value.State != workspace.StateReady && value.State != workspace.StateDeleting && value.State != workspace.StateDeleted {
		return domain.NewError(domain.CodeWorkspaceBusy, "computer storage is not ready for deletion")
	}
	if s.hostWorkspaceVolumes == nil {
		return domain.NewError(domain.CodeInvalidState, "computer storage cleanup is unavailable")
	}
	return s.hostWorkspaceVolumes.ValidateHostWorkspaceVolume(ctx, ownerID, hostID, workspaceID)
}

func (s *WorkspaceService) DeleteHostWorkspace(ctx context.Context, ownerID, hostID string) error {
	if !domain.ValidUserID(ownerID) || !domain.ValidHostID(hostID) {
		return domain.InvalidArgument("host_workspace", "computer storage identity is invalid")
	}
	bindings, err := s.store.GetWorkspaceBindingsByHost(ctx, hostID)
	if err != nil {
		return fmt.Errorf("read computer storage binding before deletion: %w", err)
	}
	if len(bindings) != 1 || bindings[0].HostID != hostID || bindings[0].OwnerID != ownerID || !domain.ValidWorkspaceID(bindings[0].Workspace.ID) {
		return domain.NewError(domain.CodeWorkspaceConflict, "computer storage binding is missing or ambiguous")
	}
	workspaceID := bindings[0].Workspace.ID
	if err := s.ValidateHostWorkspace(ctx, ownerID, hostID, workspaceID); err != nil {
		return err
	}
	if bindings[0].State != workspace.BindingDetached {
		return domain.NewError(domain.CodeWorkspaceBusy, "computer storage is still attached")
	}
	principal := identity.Principal{UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindSession}
	_, err = s.deleteWorkspace(ctx, principal, workspaceID, hostID)
	return err
}

func (s *WorkspaceService) ListDeviceWorkspaces(ctx context.Context, principal identity.Principal) ([]DeviceWorkspaceInfo, error) {
	if principal.Kind != identity.KindDevice {
		return nil, domain.NewError(domain.CodeForbidden, "device credential required")
	}
	if principal.Role != identity.RoleUser || !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		principal.HostScope != "" || len(principal.HostIDs) != 0 {
		return nil, domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if !principal.HasScope(identity.ScopeWorkspaceSync) {
		return nil, domain.NewError(domain.CodeForbidden, "workspace sync scope is required")
	}
	values, err := s.store.ListWorkspacesByOwner(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	result := make([]DeviceWorkspaceInfo, 0, len(values))
	for _, value := range values {
		if value.OwnerID == principal.UserID && principal.AllowsWorkspace(value.ID) && value.State == workspace.StateReady && value.DeletedAt == nil {
			result = append(result, DeviceWorkspaceInfo{ID: value.ID, Name: value.Name, Revision: value.Revision, State: value.State})
		}
	}
	return result, nil
}

func (s *WorkspaceService) GetWorkspace(ctx context.Context, principal identity.Principal, id string) (workspace.Workspace, error) {
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return workspace.Workspace{}, domain.NewError(domain.CodeForbidden, "portal session required")
	}
	if err := validatePrincipal(principal); err != nil {
		return workspace.Workspace{}, err
	}
	if !domain.ValidWorkspaceID(id) {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	value, err := s.store.GetWorkspace(ctx, id)
	if err != nil {
		return workspace.Workspace{}, err
	}
	if value.OwnerID != principal.UserID || value.State == workspace.StateDeleted || value.DeletedAt != nil {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return value, nil
}

func (s *WorkspaceService) DeleteWorkspace(ctx context.Context, principal identity.Principal, id string) (workspace.Workspace, error) {
	return s.deleteWorkspace(ctx, principal, id, "")
}

func (s *WorkspaceService) deleteWorkspace(ctx context.Context, principal identity.Principal, id, allowedHostID string) (workspace.Workspace, error) {
	if principal.Kind != identity.KindSession {
		return workspace.Workspace{}, domain.NewError(domain.CodeForbidden, "portal session required")
	}
	if err := validatePrincipal(principal); err != nil {
		return workspace.Workspace{}, err
	}
	if !domain.ValidWorkspaceID(id) {
		return workspace.Workspace{}, domain.InvalidArgument("workspace_id", "workspace id is invalid")
	}
	value, err := s.store.GetWorkspace(ctx, id)
	if err != nil {
		return workspace.Workspace{}, err
	}
	if value.OwnerID != principal.UserID {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	bindings, err := s.store.GetWorkspaceBindingsByWorkspace(ctx, id)
	if err != nil {
		return workspace.Workspace{}, fmt.Errorf("read computer storage bindings before deletion: %w", err)
	}
	for _, binding := range bindings {
		if allowedHostID == "" || binding.HostID != allowedHostID || binding.State != workspace.BindingDetached {
			return workspace.Workspace{}, domain.NewError(domain.CodeWorkspaceBusy, "computer storage is assigned to a cloud computer")
		}
	}
	lock, acquired, err := s.store.TryWorkspaceBlockStoreLock(ctx, id)
	if err != nil {
		return workspace.Workspace{}, fmt.Errorf("lock workspace before deletion: %w", err)
	}
	if !acquired {
		return workspace.Workspace{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace is busy")
	}
	if lock == nil {
		return workspace.Workspace{}, errors.New("workspace deletion lock was not returned")
	}
	defer func() { _ = lock.Release(ctx) }()
	value, err = s.store.GetWorkspace(ctx, id)
	if err != nil {
		return workspace.Workspace{}, err
	}
	if value.OwnerID != principal.UserID {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	bindings, err = s.store.GetWorkspaceBindingsByWorkspace(ctx, id)
	if err != nil {
		return workspace.Workspace{}, fmt.Errorf("recheck computer storage bindings before deletion: %w", err)
	}
	for _, binding := range bindings {
		if allowedHostID == "" || binding.HostID != allowedHostID || binding.State != workspace.BindingDetached {
			return workspace.Workspace{}, domain.NewError(domain.CodeWorkspaceBusy, "computer storage is assigned to a cloud computer")
		}
	}
	if value.State == workspace.StateDeleted || value.DeletedAt != nil {
		if allowedHostID != "" {
			if err := s.hostWorkspaceVolumes.DeleteHostWorkspaceVolume(ctx, principal.UserID, allowedHostID, id); err != nil {
				return workspace.Workspace{}, err
			}
		}
		return value, nil
	}
	wasReady := value.State == workspace.StateReady
	if value.State == workspace.StateReady {
		value, err = s.store.ClaimWorkspaceForDelete(ctx, id, value.Revision)
		if err != nil {
			return workspace.Workspace{}, err
		}
	} else if value.State != workspace.StateDeleting {
		return workspace.Workspace{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace cannot be deleted in its current state")
	}
	if wasReady {
		if err := s.audit.Write(ctx, audit.Entry{
			ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind),
			Action: audit.ActionWorkspaceDelete, Target: id,
		}); err != nil {
			return workspace.Workspace{}, fmt.Errorf("write workspace delete audit: %w", err)
		}
	}
	if err := s.content.DeleteWorkspace(id); err != nil {
		return workspace.Workspace{}, fmt.Errorf("delete workspace content: %w", err)
	}
	if err := s.current.Delete(ctx, value.OwnerID, id); err != nil {
		return workspace.Workspace{}, fmt.Errorf("delete workspace current directory: %w", err)
	}
	if allowedHostID != "" {
		if err := s.hostWorkspaceVolumes.DeleteHostWorkspaceVolume(ctx, principal.UserID, allowedHostID, id); err != nil {
			return workspace.Workspace{}, err
		}
	}
	deleted, err := s.store.CompleteWorkspaceDelete(ctx, id, value.Revision)
	if err != nil {
		current, getErr := s.store.GetWorkspace(ctx, id)
		if getErr == nil && (current.State == workspace.StateDeleted || current.DeletedAt != nil) {
			return current, nil
		}
		return workspace.Workspace{}, err
	}
	return deleted, nil
}

var _ interface {
	CreateWorkspace(context.Context, identity.Principal, CreateWorkspaceCommand, string) (workspace.Workspace, error)
	ListWorkspaces(context.Context, identity.Principal) ([]workspace.Workspace, error)
	ListDeviceWorkspaces(context.Context, identity.Principal) ([]DeviceWorkspaceInfo, error)
	GetWorkspace(context.Context, identity.Principal, string) (workspace.Workspace, error)
	DeleteWorkspace(context.Context, identity.Principal, string) (workspace.Workspace, error)
} = (*WorkspaceService)(nil)
