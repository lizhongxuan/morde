package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type workspaceSyncProjectFolderStore interface {
	GetWorkspaceSyncFolder(context.Context, identity.Principal, string, string) (workspace.SyncFolder, error)
	CheckWorkspaceSyncProjectFolderAccess(context.Context, identity.Principal, string, string) error
}

func (s *WorkspaceSyncService) authorizeSyncSession(ctx context.Context, principal identity.Principal, workspaceID, sessionID string) (workspace.Workspace, workspace.SyncSession, error) {
	if !workspace.ValidSessionID(sessionID) {
		return workspace.Workspace{}, workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	session, err := s.store.GetWorkspaceSyncSession(ctx, workspaceID, sessionID)
	if err != nil {
		return workspace.Workspace{}, workspace.SyncSession{}, err
	}
	if session.OwnerID != principal.UserID || session.ReplicaID != syncReplicaID(principal) {
		return workspace.Workspace{}, workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	if session.FolderID == "" {
		value, err := s.authorize(ctx, principal, workspaceID)
		return value, session, err
	}
	folders, ok := s.store.(workspaceSyncProjectFolderStore)
	if !ok {
		return workspace.Workspace{}, workspace.SyncSession{}, domain.NewError(domain.CodeWorkspaceUnavailable, "project upload is unavailable")
	}
	value, err := s.authorizeProjectUpload(ctx, principal, workspaceID, session.FolderID, folders)
	if err != nil {
		return workspace.Workspace{}, workspace.SyncSession{}, err
	}
	folder, err := folders.GetWorkspaceSyncFolder(ctx, principal, workspaceID, session.FolderID)
	if err != nil {
		return workspace.Workspace{}, workspace.SyncSession{}, err
	}
	if folder.Kind != workspace.SyncFolderKindProject || folder.State != "pending_node" && folder.State != "active" ||
		len(session.Scope) != 1 || session.Scope[0] != folder.RelativePath || session.Direction != workspace.SyncUpload || session.Policy != workspace.SyncCompareMerge {
		return workspace.Workspace{}, workspace.SyncSession{}, domain.NewError(domain.CodeWorkspaceUnavailable, "project upload session target is no longer available")
	}
	return value, session, nil
}

func (s *WorkspaceSyncService) createProjectUploadSession(ctx context.Context, principal identity.Principal, workspaceID string,
	request workspace.CreateSyncSession, idempotencyKey string) (workspace.SyncSession, error) {
	if !s.projectUploadEnabled {
		return workspace.SyncSession{}, domain.NewError(domain.CodeWorkspaceUnavailable, "project upload is unavailable")
	}
	originalRequest := request
	folders, ok := s.store.(workspaceSyncProjectFolderStore)
	if !ok {
		return workspace.SyncSession{}, domain.NewError(domain.CodeWorkspaceUnavailable, "project upload is unavailable")
	}
	value, err := s.authorizeProjectUpload(ctx, principal, workspaceID, request.FolderID, folders)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	if value.ReadOnly {
		return workspace.SyncSession{}, domain.NewError(domain.CodeForbidden, "workspace is shared as read-only")
	}
	folder, err := folders.GetWorkspaceSyncFolder(ctx, principal, workspaceID, request.FolderID)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	if folder.OwnerID != value.OwnerID || folder.WorkspaceID != workspaceID {
		return workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync folder not found")
	}
	command, err := workspace.PrepareProjectUploadSnapshot(folder, request)
	if err != nil {
		return workspace.SyncSession{}, domain.InvalidArgument("sync_session", err.Error())
	}
	if err := validateSyncIdempotencyKey(idempotencyKey); err != nil {
		return workspace.SyncSession{}, err
	}
	encoded, err := json.Marshal(command)
	if err != nil {
		return workspace.SyncSession{}, domain.InvalidArgument("sync_session", "workspace sync manifest cannot be encoded")
	}
	if int64(len(encoded)) > s.maxManifestBytes {
		return workspace.SyncSession{}, domain.NewError(domain.CodeRequestTooLarge, "workspace sync manifest exceeds its byte limit")
	}
	if value.State != workspace.StateReady {
		return workspace.SyncSession{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace is not available for synchronization")
	}
	if folder.LastUploadRevision != nil && *folder.LastUploadRevision > value.Revision {
		return workspace.SyncSession{}, domain.NewError(domain.CodeWorkspaceConflict, "project folder baseline is ahead of the workspace")
	}
	baseRevision := value.Revision
	if folder.LastUploadRevision != nil {
		baseRevision = *folder.LastUploadRevision
	}
	command.BaseRevision = baseRevision
	baseFiles, baseEmptyDirectories, baseDirectories, err := s.readManifest(ctx, value.OwnerID, workspaceID, baseRevision, command.Scope)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	currentFiles, currentEmptyDirectories, currentDirectories, err := s.readManifest(ctx, value.OwnerID, workspaceID, value.Revision, command.Scope)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	plan := workspace.SyncPlan{}
	if folder.LastUploadRevision == nil && hasProjectDestinationContents(folder.RelativePath, currentFiles, currentEmptyDirectories, currentDirectories) {
		plan = projectDestinationConflictPlan(command, currentFiles, currentEmptyDirectories, currentDirectories)
	} else {
		if folder.LastUploadRevision != nil {
			command.DeletedPaths, command.DeletedDirectories, err = workspace.ProjectSnapshotDeletions(folder.RelativePath,
				baseFiles, baseEmptyDirectories, baseDirectories, command)
			if err != nil {
				return workspace.SyncSession{}, domain.NewError(domain.CodeWorkspaceUnavailable, "saved project upload baseline is invalid")
			}
		}
		if err := workspace.ValidateCreateSyncSession(command, 0); err != nil {
			message := err.Error()
			if strings.Contains(message, "16 MiB") || strings.Contains(message, "item limits") {
				return workspace.SyncSession{}, domain.NewError(domain.CodeRequestTooLarge, "workspace sync manifest exceeds its limits")
			}
			return workspace.SyncSession{}, domain.InvalidArgument("sync_session", message)
		}
		plan = workspace.PlanSyncTreeWithDirectories(baseFiles, currentFiles, baseEmptyDirectories, currentEmptyDirectories,
			baseDirectories, currentDirectories, command)
	}
	for _, file := range plan.UploadFiles {
		if file.SizeBytes > workspace.MaxSyncFileBytes {
			return workspace.SyncSession{}, domain.NewError(domain.CodeRequestTooLarge, "workspace sync file exceeds 300 MiB")
		}
	}
	blockLock, err := s.lockWorkspaceBlockStore(ctx, workspaceID, false)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	defer func() { _ = blockLock.Release(ctx) }()
	transferBlocks, plannedBytes, err := s.prepareTransferPlan(ctx, workspaceID, plan)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	sessionID, err := workspace.NewSyncSessionID()
	if err != nil {
		return workspace.SyncSession{}, fmt.Errorf("generate workspace sync session: %w", err)
	}
	session := workspace.SyncSession{
		ID: sessionID, WorkspaceID: workspaceID, FolderID: folder.FolderID, FolderBaselineRevision: cloneInt64Pointer(folder.LastUploadRevision),
		OwnerID: value.OwnerID, ReplicaID: syncReplicaID(principal), Direction: workspace.SyncUpload, Policy: workspace.SyncCompareMerge,
		State: workspace.SyncPlanned, BaseRevision: baseRevision, TargetRevision: value.Revision, PinnedRevision: value.Revision,
		Stage: workspace.SyncStageCompare, PlannedBytes: plannedBytes, Conflicts: plan.Conflicts,
		Files: append([]workspace.File(nil), command.Files...), Directories: append([]workspace.Directory(nil), command.Directories...),
		UploadFiles: plan.UploadFiles, UploadDirectories: plan.UploadDirectories,
		Scope: append([]string(nil), command.Scope...), DeletedPaths: append([]string(nil), command.DeletedPaths...),
		EmptyDirectories: append([]string(nil), command.EmptyDirectories...), UploadEmptyDirectories: plan.UploadEmptyDirectories,
		DeletedDirectories: append([]string(nil), command.DeletedDirectories...), UploadDeletedDirectories: plan.UploadDeletedDirectories,
		UploadDeletedPaths: plan.UploadDeletedPaths, TransferBlocks: transferBlocks,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	requestForHash := struct {
		WorkspaceID string                      `json:"workspace_id"`
		Request     workspace.CreateSyncSession `json:"request"`
	}{WorkspaceID: workspaceID, Request: originalRequest}
	record, err := idempotencyRecord(value.OwnerID, "workspace.sync.create", idempotencyKey, requestForHash, sessionID, "")
	if err != nil {
		return workspace.SyncSession{}, err
	}
	reservations, err := workspaceBlockReservations(s.blocks, workspaceID, transferBlocks)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	created, _, err := s.store.CreateWorkspaceSyncSession(ctx, session, transferBlocks, reservations, s.maxOwnerBytes, record)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	return created, nil
}

func (s *WorkspaceSyncService) authorizeProjectUpload(ctx context.Context, principal identity.Principal, workspaceID, folderID string,
	folders workspaceSyncProjectFolderStore) (workspace.Workspace, error) {
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSyncFolderID(folderID) || !domain.ValidUserID(principal.UserID) {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace sync folder not found")
	}
	switch principal.Kind {
	case identity.KindSession:
		if err := validatePrincipal(principal); err != nil {
			return workspace.Workspace{}, err
		}
	case identity.KindDevice:
		if principal.Role != identity.RoleUser || !domain.ValidTokenID(principal.TokenID) || principal.HostScope != "" ||
			len(principal.HostIDs) != 0 || !principal.HasScope(identity.ScopeWorkspaceSync) || !principal.AllowsWorkspace(workspaceID) {
			return workspace.Workspace{}, domain.NewError(domain.CodeForbidden, "project upload is not allowed for this credential")
		}
		if err := folders.CheckWorkspaceSyncProjectFolderAccess(ctx, principal, workspaceID, folderID); err != nil {
			return workspace.Workspace{}, err
		}
	default:
		return workspace.Workspace{}, domain.NewError(domain.CodeForbidden, "project upload is not allowed for this credential")
	}
	value, err := s.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return workspace.Workspace{}, err
	}
	if value.OwnerID != principal.UserID || value.State != workspace.StateReady || value.DeletedAt != nil {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return value, nil
}

func projectDestinationConflictPlan(command workspace.CreateSyncSession, remoteFiles []workspace.File,
	remoteEmptyDirectories []string, remoteDirectories []workspace.Directory) workspace.SyncPlan {
	root := command.Scope[0]
	localFiles := make(map[string]workspace.File, len(command.Files))
	for _, file := range command.Files {
		localFiles[strings.ToLower(file.Path)] = file
	}
	conflicts := make(map[string]workspace.Conflict)
	add := func(path string, local, remote *workspace.File) {
		key := strings.ToLower(path)
		conflict := workspace.Conflict{Path: path, Reason: "destination_exists"}
		if local != nil {
			conflict.LocalSHA256 = stringPointer(local.SHA256)
		}
		if remote != nil {
			conflict.RemoteSHA256 = stringPointer(remote.SHA256)
		}
		if existing, ok := conflicts[key]; ok {
			if existing.LocalSHA256 == nil && conflict.LocalSHA256 != nil {
				existing.LocalSHA256 = conflict.LocalSHA256
			}
			if existing.RemoteSHA256 == nil && conflict.RemoteSHA256 != nil {
				existing.RemoteSHA256 = conflict.RemoteSHA256
			}
			conflicts[key] = existing
			return
		}
		conflicts[key] = conflict
	}
	for _, file := range remoteFiles {
		if !projectPathWithinRoot(file.Path, root) {
			continue
		}
		var local *workspace.File
		if candidate, ok := localFiles[strings.ToLower(file.Path)]; ok {
			candidateCopy := candidate
			local = &candidateCopy
		}
		add(file.Path, local, &file)
	}
	for _, path := range remoteEmptyDirectories {
		if !projectPathWithinRoot(path, root) {
			continue
		}
		add(path, nil, nil)
	}
	for _, directory := range remoteDirectories {
		if strings.EqualFold(directory.Path, root) || !projectPathWithinRoot(directory.Path, root) {
			continue
		}
		add(directory.Path, nil, nil)
	}
	for _, file := range command.Files {
		if _, exists := conflicts[strings.ToLower(file.Path)]; !exists {
			add(file.Path, &file, nil)
		}
	}
	for _, path := range command.EmptyDirectories {
		if _, exists := conflicts[strings.ToLower(path)]; !exists {
			add(path, nil, nil)
		}
	}
	for _, directory := range command.Directories {
		if _, exists := conflicts[strings.ToLower(directory.Path)]; !exists {
			add(directory.Path, nil, nil)
		}
	}
	ordered := make([]string, 0, len(conflicts))
	for path := range conflicts {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	plan := emptyWorkspaceSyncPlan()
	for _, path := range ordered {
		plan.Conflicts = append(plan.Conflicts, conflicts[path])
	}
	return plan
}

func hasProjectDestinationContents(root string, files []workspace.File, emptyDirectories []string, directories []workspace.Directory) bool {
	for _, file := range files {
		if projectPathWithinRoot(file.Path, root) {
			return true
		}
	}
	for _, directory := range emptyDirectories {
		if projectPathWithinRoot(directory, root) {
			return true
		}
	}
	for _, directory := range directories {
		if projectPathWithinRoot(directory.Path, root) && !strings.EqualFold(directory.Path, root) {
			return true
		}
	}
	return false
}

func projectPathWithinRoot(value, root string) bool {
	value, root = strings.ToLower(strings.TrimSuffix(value, "/")), strings.ToLower(strings.TrimSuffix(root, "/"))
	return value == root || strings.HasPrefix(value, root+"/")
}

func emptyWorkspaceSyncPlan() workspace.SyncPlan {
	return workspace.SyncPlan{
		UploadFiles: []workspace.File{}, DownloadFiles: []workspace.File{},
		UploadDirectories: []workspace.Directory{}, DownloadDirectories: []workspace.Directory{},
		UploadDeletedPaths: []string{}, DownloadDeletedPaths: []string{},
		UploadEmptyDirectories: []string{}, DownloadEmptyDirectories: []string{},
		UploadDeletedDirectories: []string{}, DownloadDeletedDirectories: []string{},
		Conflicts: []workspace.Conflict{}, UploadBlocks: []workspace.Chunk{},
		DownloadBlocks: []workspace.Chunk{}, RequiredBlocks: []workspace.Chunk{},
	}
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func stringPointer(value string) *string { return &value }
