package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

type WorkspaceVolumeState string

const (
	WorkspaceVolumeLegacy  WorkspaceVolumeState = "legacy"
	WorkspaceVolumePending WorkspaceVolumeState = "pending"
	WorkspaceVolumeReady   WorkspaceVolumeState = "ready"
)

type WorkspaceVolumeRegistry struct {
	WorkspaceID        string
	OwnerID            string
	ProvisionedOwnerID string
	State              WorkspaceVolumeState
	VolumeName         string
	LegacyHostID       string
}

type WorkspaceVolumeRegistryStore interface {
	GetWorkspaceVolumeRegistry(context.Context, string) (WorkspaceVolumeRegistry, error)
	MarkWorkspaceVolumeReady(context.Context, string, string, string) error
}

type WorkspaceVolumeProvisioner interface {
	EnsureWorkspaceVolume(context.Context, string, string, string, string) error
	VerifyWorkspaceVolume(context.Context, string, string, string, string) error
}

type WorkspaceVolumeCopyCapacityChecker interface {
	CheckCapacityForWrite(context.Context, int64) error
}

type WorkspaceVolumeCopyRequest struct {
	ReservationID     string
	ServiceInstanceID string
	OwnerID           string
	SourceWorkspaceID string
	TargetWorkspaceID string
	SourceRevision    int64
	Files             []workspace.File
	EmptyDirectories  []string
	Directories       []workspace.Directory
}

type WorkspaceVolumeCopyReservationChecker interface {
	CheckWorkspaceVolumeCopyReservation(context.Context, WorkspaceVolumeCopyRequest) error
}

type WorkspaceVolumeCopyPreflight interface {
	ValidateWorkspaceVolumeCopySource(context.Context, WorkspaceVolumeCopyRequest, WorkspaceVolumeCopyReservationChecker) error
}

type WorkspaceVolumeCopier interface {
	CopyWorkspaceVolume(context.Context, WorkspaceVolumeCopyRequest, WorkspaceVolumeCopyReservationChecker) ([]workspace.File, []workspace.Directory, error)
}

type HostWorkspaceVolumeResolver interface {
	PrepareHostWorkspaceVolume(context.Context, string, string, string, string, string) (SandboxVolume, error)
}

type WorkspaceVolumeService struct {
	store      WorkspaceVolumeRegistryStore
	volumes    WorkspaceVolumeProvisioner
	instanceID string
}

func NewWorkspaceVolumeService(store WorkspaceVolumeRegistryStore, volumes WorkspaceVolumeProvisioner, instanceID string) (*WorkspaceVolumeService, error) {
	if store == nil || volumes == nil || strings.TrimSpace(instanceID) == "" || len(instanceID) > 128 {
		return nil, errors.New("workspace volume registry, provisioner, and service instance are required")
	}
	return &WorkspaceVolumeService{store: store, volumes: volumes, instanceID: instanceID}, nil
}

func WorkspaceNamedVolumeName(workspaceID string) string {
	return "workspace-" + workspaceID
}

func (s *WorkspaceVolumeService) PrepareHostWorkspaceVolume(ctx context.Context, ownerID, hostID, workspaceID, mountPath, storageSize string) (SandboxVolume, error) {
	if s == nil || !domain.ValidUserID(ownerID) || !domain.ValidHostID(hostID) || !domain.ValidWorkspaceID(workspaceID) ||
		!filepath.IsAbs(mountPath) || filepath.Clean(mountPath) != mountPath || strings.TrimSpace(storageSize) == "" {
		return SandboxVolume{}, domain.InvalidArgument("workspace_volume", "workspace volume request is invalid")
	}
	registered, err := s.store.GetWorkspaceVolumeRegistry(ctx, workspaceID)
	if err != nil {
		return SandboxVolume{}, err
	}
	if registered.WorkspaceID != workspaceID || registered.OwnerID != ownerID {
		return SandboxVolume{}, domain.NewError(domain.CodeNotFound, "workspace volume not found")
	}
	name := WorkspaceNamedVolumeName(workspaceID)
	switch registered.State {
	case WorkspaceVolumeLegacy:
		if registered.LegacyHostID != hostID || registered.VolumeName != "" {
			return SandboxVolume{}, domain.NewError(domain.CodeWorkspaceConflict, "legacy workspace volume is not mapped to this Host")
		}
		return SandboxVolume{
			Name: "workspace-root", MountPath: mountPath, ClaimName: domain.WorkspaceRootVolumeName(hostID),
			Storage: storageSize, RequireExisting: true,
		}, nil
	case WorkspaceVolumePending:
		if registered.VolumeName != name || !domain.ValidUserID(registered.ProvisionedOwnerID) {
			return SandboxVolume{}, domain.NewError(domain.CodeInvalidState, "pending workspace volume identity is invalid")
		}
		if err := s.volumes.EnsureWorkspaceVolume(ctx, name, s.instanceID, registered.ProvisionedOwnerID, workspaceID); err != nil {
			return SandboxVolume{}, fmt.Errorf("provision workspace volume: %w", err)
		}
		if err := s.store.MarkWorkspaceVolumeReady(ctx, ownerID, workspaceID, name); err != nil {
			return SandboxVolume{}, fmt.Errorf("register workspace volume: %w", err)
		}
	case WorkspaceVolumeReady:
		if registered.VolumeName != name || !domain.ValidUserID(registered.ProvisionedOwnerID) {
			return SandboxVolume{}, domain.NewError(domain.CodeInvalidState, "registered workspace volume identity is invalid")
		}
		if err := s.volumes.VerifyWorkspaceVolume(ctx, name, s.instanceID, registered.ProvisionedOwnerID, workspaceID); err != nil {
			return SandboxVolume{}, fmt.Errorf("verify registered workspace volume: %w", err)
		}
	default:
		return SandboxVolume{}, domain.NewError(domain.CodeInvalidState, "workspace volume state is invalid")
	}
	return SandboxVolume{
		Name: "workspace", MountPath: mountPath, ClaimName: name, Storage: storageSize, RequireExisting: true,
	}, nil
}
