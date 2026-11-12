package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type HostCopyCommand struct {
	Name                    string
	ExpectedCurrentRevision int64
}

type HostCopyResult struct {
	SourceHostID      string
	SourceWorkspaceID string
	Host              host.Host
	Workspace         DeviceWorkspaceInfo
	SourceRevision    int64
	State             workspace.CopyState
	Replayed          bool
}

type hostCopyHostService interface {
	Get(context.Context, identity.Principal, string) (host.Host, error)
	Create(context.Context, identity.Principal, CreateHostCommand) (host.Host, error)
}

type hostCopyWorkspaceService interface {
	ResumeOrReplayHostCopy(context.Context, identity.Principal, string, WorkspaceCopyCommand, string) (WorkspaceCopyResult, bool, error)
	CopyFromHost(context.Context, identity.Principal, string, int64, string, WorkspaceCopyCommand, string) (WorkspaceCopyResult, error)
}

type HostCopyService struct {
	hosts      hostCopyHostService
	workspaces hostCopyWorkspaceService
}

func NewHostCopyService(hosts hostCopyHostService, workspaces hostCopyWorkspaceService) (*HostCopyService, error) {
	if hosts == nil || workspaces == nil {
		return nil, domain.NewError(domain.CodeInvalidState, "Host copy dependencies are required")
	}
	return &HostCopyService{hosts: hosts, workspaces: workspaces}, nil
}

func (s *HostCopyService) Copy(ctx context.Context, principal identity.Principal, sourceHostID string,
	command HostCopyCommand, idempotencyKey string) (HostCopyResult, error) {
	if s == nil || s.hosts == nil || s.workspaces == nil {
		return HostCopyResult{}, domain.NewError(domain.CodeWorkspaceUnavailable, "Host copy service is unavailable")
	}
	if principal.Kind != identity.KindSession {
		return HostCopyResult{}, domain.NewError(domain.CodeForbidden, "Host copies require a Portal session")
	}
	if err := validatePrincipal(principal); err != nil {
		return HostCopyResult{}, err
	}
	command.Name = strings.TrimSpace(command.Name)
	if !domain.ValidHostID(sourceHostID) || command.Name == "" || utf8.RuneCountInString(command.Name) > 100 || !utf8.ValidString(command.Name) ||
		command.ExpectedCurrentRevision < 0 || validateIdempotencyKey(idempotencyKey) != nil {
		return HostCopyResult{}, domain.InvalidArgument("host_copy", "Host copy request is invalid")
	}
	copyCommand := WorkspaceCopyCommand{Name: command.Name, ExpectedCurrentRevision: command.ExpectedCurrentRevision}
	copyResult, replayed, err := s.workspaces.ResumeOrReplayHostCopy(ctx, principal, sourceHostID, copyCommand, idempotencyKey)
	if err != nil {
		return HostCopyResult{}, err
	}
	if !replayed {
		source, err := s.hosts.Get(ctx, principal, sourceHostID)
		if err != nil {
			return HostCopyResult{}, err
		}
		if source.OwnerID != principal.UserID {
			return HostCopyResult{}, domain.NewError(domain.CodeNotFound, "host not found")
		}
		if source.State != host.StateStopped || source.SandboxID != "" || source.RuntimeInstanceID != "" || source.ControlState != host.ControlAI {
			return HostCopyResult{}, domain.NewError(domain.CodeWorkspaceBusy, "stop the source Host before copying it")
		}
		var binding workspace.Binding
		activeBindings := 0
		for _, value := range source.Workspaces {
			if value.State == workspace.BindingDetached {
				continue
			}
			activeBindings++
			binding = value
		}
		if activeBindings != 1 || binding.HostID != source.ID || binding.OwnerID != principal.UserID ||
			binding.Workspace.OwnerID != principal.UserID || !domain.ValidWorkspaceID(binding.Workspace.ID) || binding.BindingVersion < 1 {
			return HostCopyResult{}, domain.NewError(domain.CodeWorkspaceConflict, "source Host must have exactly one owned Workspace binding")
		}
		if binding.State != workspace.BindingActive || binding.Workspace.State != workspace.StateReady || binding.Workspace.DeletedAt != nil ||
			binding.Workspace.Revision != command.ExpectedCurrentRevision || binding.DesiredRevision != command.ExpectedCurrentRevision ||
			binding.AppliedRevision != command.ExpectedCurrentRevision || binding.SyncState != workspace.BindingSyncHealthy || len(binding.SyncConflicts) != 0 {
			return HostCopyResult{}, domain.NewError(domain.CodeWorkspaceConflict, "source Host Workspace is not at a confirmed revision")
		}

		copyResult, err = s.workspaces.CopyFromHost(ctx, principal, source.ID, binding.BindingVersion, binding.Workspace.ID, copyCommand, idempotencyKey)
		if err != nil {
			return HostCopyResult{}, err
		}
	}
	created, err := s.hosts.Create(ctx, principal, CreateHostCommand{
		Name: command.Name, WorkspaceIDs: []string{copyResult.Workspace.ID}, IdempotencyKey: hostCopyCreateKey(idempotencyKey),
		workspaceCopyReservationID: copyResult.reservationID,
	})
	if err != nil {
		return HostCopyResult{}, err
	}
	return HostCopyResult{
		SourceHostID: sourceHostID, SourceWorkspaceID: copyResult.SourceWorkspaceID, Host: created,
		Workspace: copyResult.Workspace, SourceRevision: copyResult.SourceRevision,
		State: copyResult.State, Replayed: copyResult.Replayed,
	}, nil
}

func hostCopyCreateKey(key string) string {
	digest := sha256.Sum256([]byte(key))
	return "host-copy-" + hex.EncodeToString(digest[:])
}
