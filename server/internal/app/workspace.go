package app

import (
	"context"
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

type WorkspaceCapacityChecker interface {
	CheckCapacity(context.Context) error
}

type WorkspaceService struct {
	store             WorkspaceStore
	content           WorkspaceContentDeleter
	capacity          WorkspaceCapacityChecker
	audit             AuditWriter
	maxWorkspaceBytes int64
}

func NewWorkspaceService(store WorkspaceStore, content WorkspaceContentDeleter, capacity WorkspaceCapacityChecker, writer AuditWriter, maxWorkspaceBytes int64) (*WorkspaceService, error) {
	if store == nil || content == nil || capacity == nil || writer == nil || maxWorkspaceBytes <= 0 {
		return nil, errors.New("workspace service dependencies and quotas are required")
	}
	return &WorkspaceService{
		store: store, content: content, capacity: capacity, audit: writer,
		maxWorkspaceBytes: maxWorkspaceBytes,
	}, nil
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
	return created, nil
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
		if value.State != workspace.StateDeleted && value.DeletedAt == nil {
			result = append(result, value)
		}
	}
	return result, nil
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
	result := make([]DeviceWorkspaceInfo, 0, len(principal.WorkspaceIDs))
	for _, value := range values {
		if value.OwnerID == principal.UserID && principal.AllowsWorkspace(value.ID) && value.State != workspace.StateDeleted && value.DeletedAt == nil {
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
	if value.State == workspace.StateDeleted || value.DeletedAt != nil {
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
