package app

import (
	"context"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type workspaceSyncCheckpointRecoveryStore interface {
	ResolveWorkspaceSyncCreate(context.Context, string, string, string, string) (workspace.SyncSession, bool, error)
}

func (s *WorkspaceSyncService) CheckpointRecoveryEnabled() bool {
	_, ok := s.store.(workspaceSyncCheckpointRecoveryStore)
	return ok
}

func (s *WorkspaceSyncService) RecoverSyncSession(ctx context.Context, principal identity.Principal,
	workspaceID, idempotencyKey string) (workspace.SyncSession, error) {
	if principal.Kind == identity.KindHostReplica {
		return workspace.SyncSession{}, domain.NewError(domain.CodeForbidden, "Host replicas cannot recover owner sync checkpoints")
	}
	if !domain.ValidWorkspaceID(workspaceID) {
		return workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	if err := validateSyncIdempotencyKey(idempotencyKey); err != nil {
		return workspace.SyncSession{}, err
	}
	switch principal.Kind {
	case identity.KindSession:
		if _, err := s.authorize(ctx, principal, workspaceID); err != nil {
			return workspace.SyncSession{}, err
		}
	case identity.KindDevice:
		if principal.Role != identity.RoleUser || !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
			!domain.ValidClientInstallationID(principal.ClientInstallationID) || principal.HostScope != "" || len(principal.HostIDs) != 0 ||
			!principal.HasScope(identity.ScopeWorkspaceSync) || !principal.AllowsWorkspace(workspaceID) {
			return workspace.SyncSession{}, domain.NewError(domain.CodeForbidden, "workspace checkpoint recovery is not allowed for this credential")
		}
	default:
		return workspace.SyncSession{}, domain.NewError(domain.CodeForbidden, "workspace checkpoint recovery requires an owner or device credential")
	}
	recoveryStore, ok := s.store.(workspaceSyncCheckpointRecoveryStore)
	if !ok {
		return workspace.SyncSession{}, domain.NewError(domain.CodeWorkspaceUnavailable, "workspace checkpoint recovery is unavailable")
	}
	value, found, err := recoveryStore.ResolveWorkspaceSyncCreate(ctx, principal.UserID, workspaceID,
		syncReplicaID(principal), idempotencyKey)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	if !found {
		return workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	_, value, err = s.authorizeSyncSession(ctx, principal, workspaceID, value.ID)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	return value, nil
}
