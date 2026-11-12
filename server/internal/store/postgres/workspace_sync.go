package postgres

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const workspaceSyncCreateQuotaRejectedResource = "workspace_sync_create_quota_rejected"

const maxWorkspaceSyncPage int32 = 1000

type workspaceManifestItem struct {
	path string
	file *workspace.File
}

func completedHostWorkspaceCheckpoint(ctx context.Context, tx pgx.Tx, workspaceID, replicaID string, revision int64, excludeSessionID string) (bool, error) {
	var completed bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
	SELECT 1 FROM workspace_sync_sessions
	WHERE workspace_id=$1 AND replica_id=$2 AND state='completed' AND direction='bidirectional' AND policy='live'
	AND target_revision=$3 AND conflicts='[]'::jsonb AND task_id IS NULL AND ($4='' OR id<>$4)
)`, workspaceID, replicaID, revision, excludeSessionID).Scan(&completed)
	return completed, err
}

func (s *Store) GetWorkspaceManifestPage(ctx context.Context, workspaceID string, revision int64, prefix, afterPath string, limit int32) (workspace.ManifestPage, error) {
	return s.getWorkspaceManifestPage(ctx, "", workspaceID, revision, prefix, afterPath, limit)
}

func (s *Store) GetWorkspaceManifestPageForOwner(ctx context.Context, ownerID, workspaceID string, revision int64, prefix, afterPath string, limit int32) (workspace.ManifestPage, error) {
	if !domain.ValidUserID(ownerID) {
		return workspace.ManifestPage{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return s.getWorkspaceManifestPage(ctx, ownerID, workspaceID, revision, prefix, afterPath, limit)
}

func (s *Store) GetWorkspaceManifestPageForOwnershipTransfer(ctx context.Context, ownerID, workspaceID, transferID string,
	revision int64, prefix, afterPath string, limit int32) (workspace.ManifestPage, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidOwnershipTransferID(transferID) ||
		revision < 0 || limit < 1 || limit > maxWorkspaceSyncPage {
		return workspace.ManifestPage{}, domain.NewError(domain.CodeNotFound, "workspace ownership transfer not found")
	}
	if afterPath != "" {
		if err := workspace.ValidateRelativePath(afterPath); err != nil {
			return workspace.ManifestPage{}, domain.InvalidArgument("cursor", "workspace manifest cursor path is invalid")
		}
	}
	if prefix != "" {
		if err := workspace.ValidateRelativePath(prefix); err != nil {
			return workspace.ManifestPage{}, domain.InvalidArgument("prefix", "workspace manifest prefix is invalid")
		}
	}
	var page workspace.ManifestPage
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var currentOwner, workspaceState, transferOwner, transferState string
		var currentRevision, transferRevision int64
		var deleted bool
		err := tx.QueryRow(ctx, `SELECT w.owner_id,w.state,w.revision,w.deleted_at IS NOT NULL,
t.from_owner_id,t.state,t.expected_revision
FROM workspaces w JOIN workspace_ownership_transfers t ON t.workspace_id=w.id
WHERE w.id=$1 AND t.id=$2 FOR SHARE OF w,t`, workspaceID, transferID).Scan(
			&currentOwner, &workspaceState, &currentRevision, &deleted, &transferOwner, &transferState, &transferRevision,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace ownership transfer not found")
		}
		if err != nil {
			return fmt.Errorf("lock Workspace transfer manifest scope: %w", err)
		}
		if (transferState != string(workspace.OwnershipTransferPending) && transferState != string(workspace.OwnershipTransferAccepting)) ||
			transferOwner != ownerID || currentOwner != ownerID || transferRevision != revision || currentRevision != revision ||
			workspaceState != string(workspace.StateReady) || deleted {
			return domain.NewError(domain.CodeNotFound, "workspace ownership transfer not found")
		}
		page, err = getWorkspaceManifestPageTx(ctx, tx, workspaceID, revision, prefix, afterPath, limit)
		return err
	})
	if err != nil {
		return workspace.ManifestPage{}, err
	}
	return page, nil
}

func (s *Store) getWorkspaceManifestPage(ctx context.Context, ownerID, workspaceID string, revision int64, prefix, afterPath string, limit int32) (workspace.ManifestPage, error) {
	if !domain.ValidWorkspaceID(workspaceID) || revision < 0 || limit < 1 || limit > maxWorkspaceSyncPage {
		return workspace.ManifestPage{}, domain.InvalidArgument("manifest", "workspace manifest query is invalid")
	}
	if afterPath != "" {
		if err := workspace.ValidateRelativePath(afterPath); err != nil {
			return workspace.ManifestPage{}, domain.InvalidArgument("cursor", "workspace manifest cursor path is invalid")
		}
	}
	if prefix != "" {
		if err := workspace.ValidateRelativePath(prefix); err != nil {
			return workspace.ManifestPage{}, domain.InvalidArgument("prefix", "workspace manifest prefix is invalid")
		}
	}
	var page workspace.ManifestPage
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		currentOwner, err := lockWorkspaceOwnershipForRead(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if ownerID != "" && ownerID != currentOwner {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		page, err = getWorkspaceManifestPageTx(ctx, tx, workspaceID, revision, prefix, afterPath, limit)
		return err
	})
	if err != nil {
		return workspace.ManifestPage{}, err
	}
	return page, nil
}

func getWorkspaceManifestPageTx(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64, prefix, afterPath string, limit int32) (workspace.ManifestPage, error) {
	queries := sqlcgen.New(tx)
	if _, err := queries.GetWorkspaceRevision(ctx, sqlcgen.GetWorkspaceRevisionParams{WorkspaceID: workspaceID, Revision: revision}); err != nil {
		return workspace.ManifestPage{}, storeError("get workspace manifest revision", "workspace revision", err)
	}
	fileRows, err := queries.ListWorkspaceRevisionFilesPage(ctx, sqlcgen.ListWorkspaceRevisionFilesPageParams{
		WorkspaceID: workspaceID, ValidFromRevision: revision, Column3: prefix,
		FilePath: afterPath, Limit: limit + 1,
	})
	if err != nil {
		return workspace.ManifestPage{}, fmt.Errorf("list workspace manifest page: %w", err)
	}
	directoryRows, err := queries.ListWorkspaceRevisionEmptyDirectoriesPage(ctx, sqlcgen.ListWorkspaceRevisionEmptyDirectoriesPageParams{
		WorkspaceID: workspaceID, ValidFromRevision: revision, Column3: prefix,
		DirectoryPath: afterPath, Limit: limit + 1,
	})
	if err != nil {
		return workspace.ManifestPage{}, fmt.Errorf("list workspace empty-directory manifest page: %w", err)
	}
	items := make([]workspaceManifestItem, 0, len(fileRows)+len(directoryRows))
	for _, row := range fileRows {
		file, err := workspaceFileFromRow(row.FilePath, row.SizeBytes, row.Sha256, row.Mode, row.Kind, row.SymlinkTarget, row.ModTimeNs, row.HardlinkTo, row.Chunks, row.Xattrs, row.Acl, row.MetadataKnown)
		if err != nil {
			return workspace.ManifestPage{}, err
		}
		items = append(items, workspaceManifestItem{path: file.Path, file: &file})
	}
	for _, directory := range directoryRows {
		if err := workspace.ValidateRelativePath(directory); err != nil {
			return workspace.ManifestPage{}, domain.NewError(domain.CodeInternal, "stored workspace empty-directory path is invalid")
		}
		items = append(items, workspaceManifestItem{path: directory})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].path < items[j].path })
	for i := 1; i < len(items); i++ {
		if items[i].path == items[i-1].path || strings.EqualFold(items[i].path, items[i-1].path) {
			return workspace.ManifestPage{}, domain.NewError(domain.CodeInternal, "workspace snapshot contains a file and directory path collision")
		}
	}
	hasMore := int32(len(items)) > limit
	if hasMore {
		items = items[:limit]
	}
	page := workspace.ManifestPage{
		WorkspaceID: workspaceID, Revision: revision,
		Files: make([]workspace.File, 0, len(fileRows)), EmptyDirectories: make([]string, 0, len(directoryRows)),
	}
	for _, item := range items {
		if item.file != nil {
			page.Files = append(page.Files, *item.file)
		} else {
			page.EmptyDirectories = append(page.EmptyDirectories, item.path)
		}
	}
	directoryPaths := workspaceDirectoryAncestors(items)
	if prefix == "" && afterPath == "" {
		directoryPaths = append(directoryPaths, ".")
		sort.Strings(directoryPaths)
	}
	page.Directories, err = loadWorkspaceRevisionDirectories(ctx, tx, workspaceID, revision, directoryPaths)
	if err != nil {
		return workspace.ManifestPage{}, err
	}
	if hasMore && len(items) > 0 {
		cursor := workspace.EncodeManifestCursor(workspace.ManifestCursor{Revision: revision, Prefix: prefix, After: items[len(items)-1].path})
		page.NextCursor = &cursor
	}
	return page, nil
}

func workspaceDirectoryAncestors(items []workspaceManifestItem) []string {
	seen := make(map[string]string)
	for _, item := range items {
		parts := strings.Split(item.path, "/")
		prefix := ""
		limit := len(parts)
		if item.file != nil {
			limit--
		}
		for index := 0; index < limit; index++ {
			if index > 0 {
				prefix += "/"
			}
			prefix += parts[index]
			seen[strings.ToLower(prefix)] = prefix
		}
	}
	result := make([]string, 0, len(seen))
	for _, path := range seen {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

// ListWorkspaceBlockReferences returns unique blocks referenced by ready
// workspace revisions, including historical snapshots needed for restore.
func (s *Store) ListWorkspaceBlockReferences(ctx context.Context) ([]workspace.BlockReference, error) {
	rows, err := s.q.ListWorkspaceBlockReferences(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workspace block references: %w", err)
	}
	result := make([]workspace.BlockReference, 0, len(rows))
	for _, row := range rows {
		result = append(result, workspace.BlockReference{
			WorkspaceID: row.WorkspaceID,
			SHA256:      row.Sha256,
			SizeBytes:   row.SizeBytes,
		})
	}
	return result, nil
}

func (s *Store) ListWorkspaceIDsForBlockGC(ctx context.Context) ([]string, error) {
	workspaceIDs, err := s.q.ListWorkspaceIDsForBlockGC(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workspaces for block garbage collection: %w", err)
	}
	if workspaceIDs == nil {
		workspaceIDs = []string{}
	}
	return workspaceIDs, nil
}

func (s *Store) ExpireIdleWorkspaceSyncSessions(ctx context.Context, workspaceID string, cutoff time.Time) (int64, error) {
	if !domain.ValidWorkspaceID(workspaceID) || cutoff.IsZero() {
		return 0, domain.InvalidArgument("workspace_gc", "workspace sync expiry fields are invalid")
	}
	var expired int64
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceForSyncMutation(ctx, tx, workspaceID); err != nil {
			return err
		}
		count, err := sqlcgen.New(tx).ExpireIdleWorkspaceSyncSessions(ctx, sqlcgen.ExpireIdleWorkspaceSyncSessionsParams{
			WorkspaceID: workspaceID, Cutoff: pgtype.Timestamptz{Time: cutoff, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("expire idle workspace sync sessions: %w", err)
		}
		expired = count
		return nil
	})
	if err != nil {
		return 0, err
	}
	return expired, nil
}

// WithWorkspaceBlockGC holds the workspace row lock until the callback finishes
// so commits and upload-session creation cannot race block deletion. Successful
// filesystem removals are committed to the usage ledger even when a later
// removal fails.
func (s *Store) WithWorkspaceBlockGC(ctx context.Context, workspaceID string, collect func([]workspace.BlockReference) ([]app.WorkspaceBlockRemoval, error)) error {
	if !domain.ValidWorkspaceID(workspaceID) || collect == nil {
		return domain.InvalidArgument("workspace_gc", "workspace block collection fields are invalid")
	}
	var collectErr error
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var state string
		var deletedAt pgtype.Timestamptz
		if err := tx.QueryRow(ctx, `SELECT state, deleted_at FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&state, &deletedAt); err != nil {
			return storeError("lock workspace for block garbage collection", "workspace", err)
		}
		if state != string(workspace.StateReady) || deletedAt.Valid {
			return domain.NewError(domain.CodeWorkspaceBusy, "workspace is not available for block garbage collection")
		}
		rows, err := sqlcgen.New(tx).ListWorkspaceBlockGCReferences(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("list protected workspace blocks: %w", err)
		}
		references := make([]workspace.BlockReference, 0, len(rows))
		for _, row := range rows {
			references = append(references, workspace.BlockReference{
				WorkspaceID: row.WorkspaceID, SHA256: row.Sha256, SizeBytes: row.SizeBytes,
			})
		}
		receiveRows, err := tx.Query(ctx, `SELECT b.workspace_id, b.sha256, b.size_bytes
FROM workspace_sync_receive_blocks b
JOIN workspace_sync_receive_reservations r ON r.id=b.reservation_id
WHERE b.workspace_id=$1 AND r.state IN ('reserved','aborting')
ORDER BY b.sha256, b.size_bytes`, workspaceID)
		if err != nil {
			return fmt.Errorf("list active receive Workspace block references: %w", err)
		}
		for receiveRows.Next() {
			var reference workspace.BlockReference
			if err := receiveRows.Scan(&reference.WorkspaceID, &reference.SHA256, &reference.SizeBytes); err != nil {
				receiveRows.Close()
				return fmt.Errorf("scan active receive Workspace block reference: %w", err)
			}
			references = append(references, reference)
		}
		if err := receiveRows.Err(); err != nil {
			receiveRows.Close()
			return fmt.Errorf("read active receive Workspace block references: %w", err)
		}
		receiveRows.Close()
		removed, err := collect(references)
		collectErr = err
		if err := applyWorkspaceBlockRemovals(ctx, tx, workspaceID, removed); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return collectErr
}

func (s *Store) GetWorkspaceChangePage(ctx context.Context, workspaceID string, afterRevision int64, afterPath string, limit int32) (workspace.ChangePage, error) {
	return s.getWorkspaceChangePage(ctx, "", workspaceID, afterRevision, afterPath, limit)
}

func (s *Store) GetWorkspaceChangePageForOwner(ctx context.Context, ownerID, workspaceID string, afterRevision int64, afterPath string, limit int32) (workspace.ChangePage, error) {
	if !domain.ValidUserID(ownerID) {
		return workspace.ChangePage{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return s.getWorkspaceChangePage(ctx, ownerID, workspaceID, afterRevision, afterPath, limit)
}

func (s *Store) getWorkspaceChangePage(ctx context.Context, ownerID, workspaceID string, afterRevision int64, afterPath string, limit int32) (workspace.ChangePage, error) {
	if !domain.ValidWorkspaceID(workspaceID) || afterRevision < 0 || limit < 1 || limit > 500 {
		return workspace.ChangePage{}, domain.InvalidArgument("changes", "workspace change query is invalid")
	}
	if afterPath != "" {
		if afterPath != "." && workspace.ValidateRelativePath(afterPath) != nil {
			return workspace.ChangePage{}, domain.InvalidArgument("after", "workspace changes cursor path is invalid")
		}
	}
	var page workspace.ChangePage
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		currentOwner, err := lockWorkspaceOwnershipForRead(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if ownerID != "" && ownerID != currentOwner {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		rows, err := sqlcgen.New(tx).ListWorkspaceChangesPage(ctx, sqlcgen.ListWorkspaceChangesPageParams{
			WorkspaceID: workspaceID, Revision: afterRevision, FilePath: afterPath, Limit: limit + 1,
		})
		if err != nil {
			return fmt.Errorf("list workspace changes: %w", err)
		}
		hasMore := int32(len(rows)) > limit
		if hasMore {
			rows = rows[:limit]
		}
		items := make([]workspace.Change, 0, len(rows))
		for _, row := range rows {
			item := workspace.Change{Revision: row.Revision, Path: row.FilePath, Action: workspace.ChangeAction(row.Action)}
			if row.Action == string(workspace.ChangeUpsert) {
				if row.Directory != nil {
					return domain.NewError(domain.CodeInternal, "stored workspace file change includes directory metadata")
				}
				var file workspace.File
				if err := json.Unmarshal(row.File, &file); err != nil {
					return fmt.Errorf("decode workspace change file: %w", err)
				}
				if err := workspace.ValidateFile(file); err != nil || file.Path != row.FilePath {
					return domain.NewError(domain.CodeInternal, "stored workspace change is invalid")
				}
				item.File = &file
			} else if row.Action == string(workspace.ChangeDirectoryUpsert) {
				if row.FilePath != "." && workspace.ValidateRelativePath(row.FilePath) != nil {
					return domain.NewError(domain.CodeInternal, "stored workspace directory change is invalid")
				}
				if row.File != nil {
					return domain.NewError(domain.CodeInternal, "stored workspace directory change includes file metadata")
				}
				if row.Directory != nil {
					var directory workspace.Directory
					if err := json.Unmarshal(row.Directory, &directory); err != nil {
						return fmt.Errorf("decode workspace change directory: %w", err)
					}
					if err := workspace.ValidateDirectory(directory, false); err != nil || directory.Path != row.FilePath {
						return domain.NewError(domain.CodeInternal, "stored workspace directory change is invalid")
					}
					item.Directory = &directory
				}
			} else if row.Action != string(workspace.ChangeDelete) && row.Action != string(workspace.ChangeDirectoryDelete) {
				return domain.NewError(domain.CodeInternal, "stored workspace change action is invalid")
			} else if row.Action == string(workspace.ChangeDelete) || row.Action == string(workspace.ChangeDirectoryDelete) {
				validPath := workspace.ValidateRelativePath(row.FilePath) == nil || row.Action == string(workspace.ChangeDirectoryDelete) && row.FilePath == "."
				if !validPath || row.File != nil || row.Directory != nil {
					return domain.NewError(domain.CodeInternal, "stored workspace directory change is invalid")
				}
			}
			items = append(items, item)
		}
		cursor := workspace.ChangeCursor{Revision: afterRevision, Path: afterPath}
		if len(items) > 0 {
			last := items[len(items)-1]
			cursor = workspace.ChangeCursor{Revision: last.Revision, Path: last.Path}
		}
		page = workspace.ChangePage{Items: items, NextCursor: workspace.EncodeChangeCursor(cursor)}
		return nil
	})
	if err != nil {
		return workspace.ChangePage{}, err
	}
	return page, nil
}

func (s *Store) CheckWorkspaceSyncHostReplicaAccess(ctx context.Context, principal identity.Principal) error {
	if principal.Kind != identity.KindHostReplica || !domain.ValidHostID(principal.HostScope) || !domain.ValidWorkspaceID(principal.ReplicaWorkspaceID) ||
		!domain.ValidUserID(principal.UserID) || principal.HostGeneration < 1 || principal.BindingVersion < 1 {
		return domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
	}
	var ownerID, hostState, bindingOwnerID, bindingState string
	var generation, bindingVersion int64
	var deleted bool
	err := s.pool.QueryRow(ctx, `
SELECT h.owner_id, h.state, h.runtime_generation, h.deleted_at IS NOT NULL,
       wb.owner_id, wb.state, wb.binding_version
FROM hosts h
JOIN workspace_bindings wb ON wb.host_id = h.id
WHERE h.id = $1 AND wb.workspace_id = $2`, principal.HostScope, principal.ReplicaWorkspaceID).
		Scan(&ownerID, &hostState, &generation, &deleted, &bindingOwnerID, &bindingState, &bindingVersion)
	if err != nil {
		return storeError("check Host workspace sync authorization", "workspace binding", err)
	}
	if ownerID != principal.UserID || bindingOwnerID != principal.UserID || generation != principal.HostGeneration || deleted ||
		(hostState != string(host.StateCreating) && hostState != string(host.StateRunning)) || bindingVersion != principal.BindingVersion ||
		(bindingState != string(workspace.BindingPreparing) && bindingState != string(workspace.BindingActive) && bindingState != string(workspace.BindingUnbinding)) {
		return domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	return nil
}

func (s *Store) CreateWorkspaceSyncSession(ctx context.Context, value workspace.SyncSession, blocks []workspace.TransferBlock, reservations []app.WorkspaceBlockReservation, maxOwnerBytes int64, idempotency app.IdempotencyRecord) (workspace.SyncSession, bool, error) {
	if err := validateSyncSession(value); err != nil {
		return workspace.SyncSession{}, false, err
	}
	if err := validateSessionTransferBlocks(value, blocks); err != nil {
		return workspace.SyncSession{}, false, err
	}
	if maxOwnerBytes <= 0 || validateWorkspaceSyncReservationSet(value.WorkspaceID, blocks, reservations) != nil {
		return workspace.SyncSession{}, false, domain.InvalidArgument("sync_session", "workspace sync block reservations are invalid")
	}
	if idempotency.OwnerID != value.OwnerID || idempotency.Operation != "workspace.sync.create" || idempotency.ResourceID != value.ID {
		return workspace.SyncSession{}, false, domain.InvalidArgument("sync_session", "workspace sync idempotency fields are invalid")
	}
	params, err := workspaceSyncSessionParams(value)
	if err != nil {
		return workspace.SyncSession{}, false, err
	}
	hostReplica, hostScoped := parseHostSyncReplicaID(value.ReplicaID)
	if hostScoped && hostReplica.workspaceID != value.WorkspaceID {
		return workspace.SyncSession{}, false, domain.NewError(domain.CodeWorkspaceConflict, "Host workspace sync replica does not match its session")
	}
	sessionScoped := strings.HasPrefix(value.ReplicaID, "session:")
	if sessionScoped && value.ReplicaID != "session:"+value.OwnerID {
		return workspace.SyncSession{}, false, domain.NewError(domain.CodeNotFound, "workspace sync session owner does not match its replica")
	}
	var created workspace.SyncSession
	replayed := false
	quotaRejected := false
	applyCreate := func(tx pgx.Tx) error {
		deviceAccess, err := prepareWorkspaceSyncDeviceReplica(ctx, tx, value.OwnerID, value.WorkspaceID, value.ReplicaID)
		if err != nil {
			return err
		}
		if value.FolderID != "" {
			if hostScoped || value.TaskID != "" {
				return domain.NewError(domain.CodeForbidden, "project upload sessions cannot use Host or Task replicas")
			}
			if err := lockWorkspaceSyncProjectRegistrationHost(ctx, tx, value.OwnerID, value.WorkspaceID, value.FolderID); err != nil {
				return err
			}
		}
		if value.TaskID != "" {
			if deviceAccess == nil {
				return domain.NewError(domain.CodeForbidden, "Task-linked workspace sync requires a device replica")
			}
			if err := lockTaskHubOperationWorkspaceSyncCreateTx(ctx, tx, value.OwnerID, value.TaskID, value.TaskHubOperationKey,
				value.WorkspaceID, deviceAccess.installationID, value.BaseRevision, value.Scope); err != nil {
				return err
			}
		}
		var hostOwner, hostState string
		var hostGeneration int64
		var hostDeletedAt pgtype.Timestamptz
		if hostScoped {
			if err := tx.QueryRow(ctx, `SELECT owner_id, state, runtime_generation, deleted_at FROM hosts WHERE id=$1 FOR UPDATE`, hostReplica.hostID).
				Scan(&hostOwner, &hostState, &hostGeneration, &hostDeletedAt); err != nil {
				return storeError("lock Host for sync session", "host", err)
			}
		}
		var ownerID, state string
		var readOnly bool
		var revision, quotaBytes int64
		if err := tx.QueryRow(ctx, `SELECT owner_id, state, revision, quota_bytes, is_read_only FROM workspaces WHERE id=$1 FOR UPDATE`, value.WorkspaceID).
			Scan(&ownerID, &state, &revision, &quotaBytes, &readOnly); err != nil {
			return storeError("lock workspace for sync session", "workspace", err)
		}
		if err := requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, value.WorkspaceID); err != nil {
			return err
		}
		if ownerID != value.OwnerID || state != string(workspace.StateReady) {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		var projectFolder *workspace.SyncFolder
		if value.FolderID == "" {
			if value.FolderBaselineRevision != nil {
				return domain.InvalidArgument("sync_session", "workspace root session has a project folder baseline")
			}
			if value.SourceBindingID != "" || value.SourceBaselineSessionID != "" {
				return domain.InvalidArgument("sync_session", "workspace root session has a project source binding")
			}
			if err := requireWorkspaceRootSyncTarget(ctx, tx, ownerID, value.WorkspaceID); err != nil {
				return err
			}
		} else {
			folder, err := lockWorkspaceSyncProjectUploadFolder(ctx, tx, ownerID, value.WorkspaceID, value.FolderID)
			if err != nil {
				return err
			}
			if len(value.Scope) != 1 || value.Scope[0] != folder.RelativePath {
				return domain.NewError(domain.CodeWorkspaceUnavailable, "project upload scope does not match its registered folder")
			}
			if err := validateWorkspaceSyncProjectUploadBaseline(value, folder); err != nil {
				return err
			}
			projectFolder = &folder
		}
		if readOnly && value.Direction != workspace.SyncDownload {
			return domain.NewError(domain.CodeForbidden, "workspace is shared as read-only")
		}
		if err := requireNoPendingWorkspaceLegacyImport(ctx, tx, value.WorkspaceID); err != nil {
			return err
		}
		if err := ensureNoWorkspaceSyncReceive(ctx, tx, value.WorkspaceID, ""); err != nil {
			return err
		}
		if err := requireActiveWorkspaceSyncDeviceSession(ctx, tx, deviceAccess, ownerID, value.WorkspaceID, value.ReplicaID, value.FolderID); err != nil {
			return err
		}
		stored, wasReplay, err := reserveIdempotency(ctx, tx, idempotency, value.ID, "")
		if err != nil {
			return err
		}
		if wasReplay {
			if stored.ResourceID == workspaceSyncCreateQuotaRejectedResource {
				if stored.RelatedID != value.ReplicaID {
					return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key is bound to another workspace replica")
				}
				quotaRejected = true
				return nil
			}
			row, err := sqlcgen.New(tx).GetWorkspaceSyncSession(ctx, sqlcgen.GetWorkspaceSyncSessionParams{WorkspaceID: value.WorkspaceID, ID: stored.ResourceID})
			if err != nil {
				return storeError("replay workspace sync session", "workspace sync session", err)
			}
			if row.OwnerID != value.OwnerID || row.ReplicaID != value.ReplicaID {
				return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key is bound to another sync session")
			}
			created, err = syncSessionDomain(ctx, tx, sqlcgen.New(tx), row)
			if err != nil {
				return err
			}
			replayed = true
			return nil
		}
		if projectFolder != nil {
			if err := ensureWorkspaceSyncProjectSourceTx(ctx, tx, value, deviceAccess, *projectFolder); err != nil {
				return err
			}
		}
		if hostScoped {
			if hostOwner != value.OwnerID || hostGeneration != hostReplica.generation ||
				(hostState != string(host.StateCreating) && hostState != string(host.StateRunning)) || hostDeletedAt.Valid {
				return domain.NewError(domain.CodeInvalidState, "Host workspace sync authorization changed before session creation")
			}
			var bindingID string
			if err := tx.QueryRow(ctx, `SELECT id FROM workspace_bindings WHERE host_id=$1 AND workspace_id=$2 FOR UPDATE`,
				hostReplica.hostID, value.WorkspaceID).Scan(&bindingID); err != nil {
				return storeError("lock Host workspace binding for sync session", "workspace binding", err)
			}
			binding, err := sqlcgen.New(tx).GetWorkspaceBinding(ctx, sqlcgen.GetWorkspaceBindingParams{HostID: hostReplica.hostID, WorkspaceID: value.WorkspaceID})
			if err != nil {
				return fmt.Errorf("read locked Host workspace binding for sync session: %w", err)
			}
			if binding.OwnerID != value.OwnerID || binding.BindingVersion != hostReplica.bindingVersion ||
				(binding.BindingState != string(workspace.BindingPreparing) && binding.BindingState != string(workspace.BindingActive) &&
					binding.BindingState != string(workspace.BindingUnbinding)) {
				return bindingStateConflict(ctx, tx, hostReplica.hostID, value.WorkspaceID)
			}
			if (value.Direction == workspace.SyncDownload || value.Direction == workspace.SyncBidirectional) &&
				value.BaseRevision > binding.AppliedRevision {
				// A live Host may verify its own completed checkpoint before reporting it as applied.
				followsOwnCheckpoint := false
				if value.Direction == workspace.SyncBidirectional && value.Policy == workspace.SyncLive && value.BaseRevision == revision {
					followsOwnCheckpoint, err = completedHostWorkspaceCheckpoint(ctx, tx, value.WorkspaceID, value.ReplicaID, value.BaseRevision, "")
					if err != nil {
						return fmt.Errorf("check Host workspace checkpoint before sync: %w", err)
					}
				}
				if !followsOwnCheckpoint {
					return domain.NewError(domain.CodeWorkspaceConflict, "Host workspace sync base revision is ahead of its applied revision")
				}
			}
		}
		if revision != value.PinnedRevision || value.TargetRevision != revision {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace changed while the sync plan was being created")
		}
		var manifestBytes int64
		for _, file := range value.Files {
			if file.SizeBytes < 0 || manifestBytes > int64(^uint64(0)>>1)-file.SizeBytes {
				return domain.InvalidArgument("sync_session", "workspace manifest size is invalid")
			}
			manifestBytes += file.SizeBytes
		}
		if manifestBytes > quotaBytes {
			if err := persistWorkspaceSyncCreateQuotaRejection(ctx, tx, idempotency, value.ReplicaID); err != nil {
				return err
			}
			quotaRejected = true
			return nil
		}
		hasUploadBlocks := false
		for _, block := range blocks {
			if block.Role == workspace.BlockUpload {
				hasUploadBlocks = true
				break
			}
		}
		if hasUploadBlocks {
			reservationTx, err := tx.Begin(ctx)
			if err != nil {
				return fmt.Errorf("start workspace block reservation savepoint: %w", err)
			}
			if err := reserveWorkspaceBlockUsage(ctx, reservationTx, value.WorkspaceID, ownerID, reservations, maxOwnerBytes); err != nil {
				if rollbackErr := reservationTx.Rollback(ctx); rollbackErr != nil {
					return errors.Join(err, fmt.Errorf("rollback workspace block reservation savepoint: %w", rollbackErr))
				}
				if domain.CodeOf(err) == domain.CodeQuotaExceeded {
					if persistErr := persistWorkspaceSyncCreateQuotaRejection(ctx, tx, idempotency, value.ReplicaID); persistErr != nil {
						return errors.Join(err, persistErr)
					}
					quotaRejected = true
					return nil
				}
				return err
			}
			if err := reservationTx.Commit(ctx); err != nil {
				return fmt.Errorf("commit workspace block reservation savepoint: %w", err)
			}
		}
		q := sqlcgen.New(tx)
		row, err := q.CreateWorkspaceSyncSession(ctx, params)
		if err != nil {
			return fmt.Errorf("create workspace sync session: %w", err)
		}
		if err := persistWorkspaceSyncSessionDirectories(ctx, tx, value); err != nil {
			return err
		}
		if value.TaskID != "" {
			if err := associateTaskHubOperationWorkspaceSyncSessionTx(ctx, tx, value.OwnerID, value.TaskID, value.TaskHubOperationKey,
				value.WorkspaceID, deviceAccess.installationID, value.ID); err != nil {
				return err
			}
		}
		for _, block := range blocks {
			if !validTransferBlock(block) {
				return domain.InvalidArgument("sync_session", "workspace sync transfer block is invalid")
			}
			if err := q.InsertWorkspaceSyncTransferBlock(ctx, sqlcgen.InsertWorkspaceSyncTransferBlockParams{
				WorkspaceID: value.WorkspaceID, SessionID: value.ID, Sha256: block.SHA256,
				SizeBytes: block.SizeBytes, Role: string(block.Role), Received: block.Received,
			}); err != nil {
				return fmt.Errorf("persist workspace sync transfer block: %w", err)
			}
		}
		for _, action := range value.DownloadActions {
			var digest pgtype.Text
			if action.SHA256 != nil {
				digest = pgtype.Text{String: *action.SHA256, Valid: true}
			}
			if err := q.InsertWorkspaceSyncDownloadAction(ctx, sqlcgen.InsertWorkspaceSyncDownloadActionParams{
				WorkspaceID: value.WorkspaceID, SessionID: value.ID, FilePath: action.Path,
				Action: string(action.Action), Sha256: digest, SizeBytes: action.SizeBytes,
			}); err != nil {
				return fmt.Errorf("persist workspace sync download action: %w", err)
			}
		}
		created, err = syncSessionDomain(ctx, tx, q, row)
		return err
	}
	if hostScoped {
		err = s.withApprovedWorkspaceSyncOwnerTx(ctx, value.OwnerID, applyCreate)
	} else if sessionScoped {
		err = s.withApprovedWorkspaceSyncOwnerReadTx(ctx, value.OwnerID, applyCreate)
	} else {
		err = s.WithTx(ctx, applyCreate)
	}
	if err != nil {
		return workspace.SyncSession{}, false, err
	}
	if quotaRejected {
		return workspace.SyncSession{}, false, domain.NewError(domain.CodeQuotaExceeded, "workspace storage quota would be exceeded")
	}
	return created, replayed, nil
}

func persistWorkspaceSyncCreateQuotaRejection(ctx context.Context, tx pgx.Tx, idempotency app.IdempotencyRecord, replicaID string) error {
	result, err := tx.Exec(ctx, `UPDATE idempotency_keys
SET resource_id=$4, related_id=$5
WHERE owner_id=$1 AND operation=$2 AND key=$3 AND request_hash=$6`,
		idempotency.OwnerID, idempotency.Operation, idempotency.Key, workspaceSyncCreateQuotaRejectedResource, replicaID, idempotency.RequestHash)
	if err != nil {
		return fmt.Errorf("persist rejected workspace sync create: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeInternal, "workspace sync create rejection lost its idempotency record")
	}
	return nil
}

func (s *Store) GetWorkspaceSyncSession(ctx context.Context, workspaceID, sessionID string) (workspace.SyncSession, error) {
	return s.getWorkspaceSyncSession(ctx, "", workspaceID, sessionID)
}

func (s *Store) GetWorkspaceSyncSessionForOwner(ctx context.Context, ownerID, workspaceID, sessionID string) (workspace.SyncSession, error) {
	if !domain.ValidUserID(ownerID) {
		return workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	return s.getWorkspaceSyncSession(ctx, ownerID, workspaceID, sessionID)
}

func (s *Store) getWorkspaceSyncSession(ctx context.Context, ownerID, workspaceID, sessionID string) (workspace.SyncSession, error) {
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) {
		return workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	var result workspace.SyncSession
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		currentOwner, err := lockWorkspaceOwnershipForRead(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if ownerID != "" && ownerID != currentOwner {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		}
		queries := sqlcgen.New(tx)
		row, err := queries.GetWorkspaceSyncSession(ctx, sqlcgen.GetWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if err != nil {
			return storeError("get workspace sync session", "workspace sync session", err)
		}
		if row.OwnerID != currentOwner {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		}
		result, err = syncSessionDomain(ctx, tx, queries, row)
		return err
	})
	if err != nil {
		return workspace.SyncSession{}, err
	}
	return result, nil
}

func (s *Store) ListDeviceWorkspaceSyncProgress(ctx context.Context, ownerID, workspaceID string) ([]workspace.DeviceSyncProgress, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) {
		return nil, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	items := make([]workspace.DeviceSyncProgress, 0)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		currentOwner, err := lockWorkspaceOwnershipForRead(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if currentOwner != ownerID {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if err := requireWorkspaceRootSyncTarget(ctx, tx, ownerID, workspaceID); err != nil {
			return err
		}
		rows, err := sqlcgen.New(tx).ListDeviceWorkspaceSyncProgress(ctx, sqlcgen.ListDeviceWorkspaceSyncProgressParams{ID: workspaceID, OwnerID: ownerID})
		if err != nil {
			return fmt.Errorf("list device workspace sync progress: %w", err)
		}
		items = make([]workspace.DeviceSyncProgress, 0, len(rows))
		for _, row := range rows {
			items = append(items, workspace.DeviceSyncProgress{
				WorkspaceID: row.WorkspaceID, DeviceID: row.DeviceID, SessionID: row.SessionID,
				Direction: workspace.SyncDirection(row.Direction), State: workspace.SyncState(row.State), Stage: workspace.SyncStage(row.Stage),
				PlannedBytes: row.PlannedBytes, ConfirmedBytes: row.ConfirmedBytes,
				DownloadActionsTotal: row.DownloadActionsTotal, DownloadActionsDone: row.DownloadActionsDone,
				UpdatedAt: row.UpdatedAt.Time.UTC(),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) ListWorkspaceSyncTransferBlocks(ctx context.Context, workspaceID, sessionID string) ([]workspace.TransferBlock, error) {
	return s.listWorkspaceSyncTransferBlocks(ctx, "", workspaceID, sessionID)
}

func (s *Store) ListWorkspaceSyncTransferBlocksForOwner(ctx context.Context, ownerID, workspaceID, sessionID string) ([]workspace.TransferBlock, error) {
	if !domain.ValidUserID(ownerID) {
		return nil, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	return s.listWorkspaceSyncTransferBlocks(ctx, ownerID, workspaceID, sessionID)
}

func (s *Store) listWorkspaceSyncTransferBlocks(ctx context.Context, ownerID, workspaceID, sessionID string) ([]workspace.TransferBlock, error) {
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) {
		return nil, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	blocks := make([]workspace.TransferBlock, 0)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		currentOwner, err := lockWorkspaceOwnershipForRead(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if ownerID != "" && ownerID != currentOwner {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		}
		var sessionOwner string
		if err := tx.QueryRow(ctx, `SELECT owner_id FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2 FOR SHARE`, workspaceID, sessionID).Scan(&sessionOwner); err != nil {
			return storeError("get workspace sync session for transfer blocks", "workspace sync session", err)
		}
		if sessionOwner != currentOwner {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		}
		rows, err := sqlcgen.New(tx).ListWorkspaceSyncTransferBlocks(ctx, sqlcgen.ListWorkspaceSyncTransferBlocksParams{WorkspaceID: workspaceID, SessionID: sessionID})
		if err != nil {
			return fmt.Errorf("list workspace sync transfer blocks: %w", err)
		}
		blocks = make([]workspace.TransferBlock, 0, len(rows))
		for _, row := range rows {
			blocks = append(blocks, workspace.TransferBlock{
				Chunk: workspace.Chunk{SHA256: row.Sha256, SizeBytes: row.SizeBytes}, Role: workspace.BlockRole(row.Role), Received: row.Received,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return blocks, nil
}

func lockWorkspaceForSyncMutation(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	var lockedID string
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&lockedID); err != nil {
		return storeError("lock workspace before sync session mutation", "workspace", err)
	}
	return requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, workspaceID)
}

func (s *Store) withWorkspaceSyncSessionMutationTx(ctx context.Context, workspaceID, sessionID string, apply func(pgx.Tx) error, allowUnbindingVersionAdvance ...bool) error {
	var ownerID, replicaID string
	var folderID pgtype.Text
	if err := s.pool.QueryRow(ctx, `SELECT owner_id, replica_id, folder_id FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2`, workspaceID, sessionID).
		Scan(&ownerID, &replicaID, &folderID); err != nil {
		return storeError("read workspace sync session before mutation", "workspace sync session", err)
	}
	if !strings.HasPrefix(replicaID, "host:") {
		if strings.HasPrefix(replicaID, "session:") {
			if replicaID != "session:"+ownerID {
				return domain.NewError(domain.CodeNotFound, "workspace sync session owner does not match its replica")
			}
			return s.withApprovedWorkspaceSyncOwnerReadTx(ctx, ownerID, func(tx pgx.Tx) error {
				if folderID.Valid {
					if err := lockWorkspaceSyncProjectRegistrationHost(ctx, tx, ownerID, workspaceID, folderID.String); err != nil {
						return err
					}
				}
				return apply(tx)
			})
		}
		return s.WithTx(ctx, func(tx pgx.Tx) error {
			if folderID.Valid {
				if _, err := prepareWorkspaceSyncDeviceSessionMutation(ctx, tx, workspaceID, sessionID); err != nil {
					return err
				}
				if err := lockWorkspaceSyncProjectRegistrationHost(ctx, tx, ownerID, workspaceID, folderID.String); err != nil {
					return err
				}
			}
			return apply(tx)
		})
	}
	replica, ok := parseHostSyncReplicaID(replicaID)
	if !ok {
		return domain.NewError(domain.CodeInternal, "stored workspace sync Host replica is invalid")
	}
	if replica.workspaceID != workspaceID {
		return domain.NewError(domain.CodeWorkspaceConflict, "Host workspace sync replica does not match its session")
	}
	return s.withApprovedWorkspaceSyncOwnerReadTx(ctx, ownerID, func(tx pgx.Tx) error {
		allowStaleBindingCleanup := len(allowUnbindingVersionAdvance) > 0 && allowUnbindingVersionAdvance[0]
		if err := lockWorkspaceSyncHostSessionAccess(ctx, tx, ownerID, workspaceID, sessionID, replica, true, allowStaleBindingCleanup); err != nil {
			return err
		}
		return apply(tx)
	})
}

func (s *Store) AckWorkspaceSyncBlock(ctx context.Context, workspaceID, sessionID, digest string, allocatedBytes, maxOwnerBytes int64) error {
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) || !validPostgresSHA256(digest) {
		return domain.NewError(domain.CodeNotFound, "workspace sync block not found")
	}
	if allocatedBytes < 0 || maxOwnerBytes <= 0 {
		return domain.InvalidArgument("workspace_block_usage", "workspace block allocation is invalid")
	}
	var settlementErr error
	err := s.withWorkspaceSyncSessionMutationTx(ctx, workspaceID, sessionID, func(tx pgx.Tx) error {
		deviceAccess, err := prepareWorkspaceSyncDeviceSessionMutation(ctx, tx, workspaceID, sessionID)
		if err != nil {
			return err
		}
		if err := lockWorkspaceForSyncMutation(ctx, tx, workspaceID); err != nil {
			return err
		}
		q := sqlcgen.New(tx)
		row, err := q.LockWorkspaceSyncSession(ctx, sqlcgen.LockWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if err != nil {
			return storeError("lock workspace sync session for block acknowledgement", "workspace sync session", err)
		}
		if err := requireActiveWorkspaceSyncDeviceSession(ctx, tx, deviceAccess, row.OwnerID, workspaceID, row.ReplicaID, row.FolderID.String); err != nil {
			return err
		}
		block, err := q.GetWorkspaceSyncTransferBlock(ctx, sqlcgen.GetWorkspaceSyncTransferBlockParams{
			WorkspaceID: workspaceID, SessionID: sessionID, Sha256: digest, Role: string(workspace.BlockUpload),
		})
		if err != nil {
			return storeError("get workspace sync transfer block", "workspace sync block", err)
		}
		if block.Received {
			return nil
		}
		if row.State != string(workspace.SyncPlanned) && row.State != string(workspace.SyncTransferring) {
			return domain.NewError(domain.CodeInvalidState, "workspace sync session does not accept block acknowledgements")
		}
		overrun, err := finalizeWorkspaceBlockUsageTx(ctx, tx, workspaceID, digest, allocatedBytes, maxOwnerBytes)
		if err != nil {
			return err
		}
		if overrun {
			// Keep the actual allocation in the ledger even though this block
			// cannot be acknowledged or included in a committed revision.
			settlementErr = domain.NewError(domain.CodeQuotaExceeded, "workspace block allocation exceeded its reserved physical quota")
			return nil
		}
		size, err := q.MarkWorkspaceSyncBlockReceived(ctx, sqlcgen.MarkWorkspaceSyncBlockReceivedParams{
			WorkspaceID: workspaceID, SessionID: sessionID, Sha256: digest,
		})
		if err != nil {
			return fmt.Errorf("acknowledge workspace sync block: %w", err)
		}
		if size != block.SizeBytes {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace sync block changed during acknowledgement")
		}
		if err := q.AddWorkspaceSyncConfirmedBytes(ctx, sqlcgen.AddWorkspaceSyncConfirmedBytesParams{
			WorkspaceID: workspaceID, SessionID: sessionID, ConfirmedBytes: size,
		}); err != nil {
			return fmt.Errorf("update workspace sync progress: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return settlementErr
}

func (s *Store) AckWorkspaceSyncDownloadBlock(ctx context.Context, workspaceID, sessionID, digest string) error {
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) || !validPostgresSHA256(digest) {
		return domain.NewError(domain.CodeNotFound, "workspace sync block not found")
	}
	return s.withWorkspaceSyncSessionMutationTx(ctx, workspaceID, sessionID, func(tx pgx.Tx) error {
		deviceAccess, err := prepareWorkspaceSyncDeviceSessionMutation(ctx, tx, workspaceID, sessionID)
		if err != nil {
			return err
		}
		if err := lockWorkspaceForSyncMutation(ctx, tx, workspaceID); err != nil {
			return err
		}
		q := sqlcgen.New(tx)
		row, err := q.LockWorkspaceSyncSession(ctx, sqlcgen.LockWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if err != nil {
			return storeError("lock workspace sync session for download acknowledgement", "workspace sync session", err)
		}
		if err := requireActiveWorkspaceSyncDeviceSession(ctx, tx, deviceAccess, row.OwnerID, workspaceID, row.ReplicaID, row.FolderID.String); err != nil {
			return err
		}
		var size int64
		var received bool
		if err := tx.QueryRow(ctx, `
SELECT size_bytes, received FROM workspace_sync_session_blocks
WHERE workspace_id=$1 AND session_id=$2 AND sha256=$3 AND role='download'`, workspaceID, sessionID, digest).Scan(&size, &received); err != nil {
			return storeError("get workspace download block", "workspace sync block", err)
		}
		if received {
			return nil
		}
		if row.State != string(workspace.SyncPlanned) && row.State != string(workspace.SyncTransferring) {
			return domain.NewError(domain.CodeInvalidState, "workspace sync session does not accept block acknowledgements")
		}
		var markedSize int64
		if err := tx.QueryRow(ctx, `
UPDATE workspace_sync_session_blocks SET received=true
WHERE workspace_id=$1 AND session_id=$2 AND sha256=$3 AND role='download' AND received=false
RETURNING size_bytes`, workspaceID, sessionID, digest).Scan(&markedSize); err != nil {
			return fmt.Errorf("acknowledge workspace download block: %w", err)
		}
		if markedSize != size {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace sync block changed during acknowledgement")
		}
		if err := q.AddWorkspaceSyncConfirmedBytes(ctx, sqlcgen.AddWorkspaceSyncConfirmedBytesParams{
			WorkspaceID: workspaceID, SessionID: sessionID, ConfirmedBytes: size,
		}); err != nil {
			return fmt.Errorf("update workspace sync download progress: %w", err)
		}
		return nil
	})
}

func (s *Store) AckWorkspaceSyncDownloadAction(ctx context.Context, workspaceID, sessionID, path string, action workspace.ChangeAction, digest string, sizeBytes int64) error {
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) {
		return domain.NewError(domain.CodeNotFound, "workspace download action not found")
	}
	var sha256 *string
	if digest != "" {
		sha256 = &digest
	}
	if err := workspace.ValidateDownloadAction(workspace.DownloadAction{
		Path: path, Action: action, SHA256: sha256, SizeBytes: sizeBytes,
	}); err != nil {
		return domain.InvalidArgument("download_action", "workspace download action acknowledgement is invalid")
	}
	return s.withWorkspaceSyncSessionMutationTx(ctx, workspaceID, sessionID, func(tx pgx.Tx) error {
		deviceAccess, err := prepareWorkspaceSyncDeviceSessionMutation(ctx, tx, workspaceID, sessionID)
		if err != nil {
			return err
		}
		if err := lockWorkspaceForSyncMutation(ctx, tx, workspaceID); err != nil {
			return err
		}
		q := sqlcgen.New(tx)
		session, err := q.LockWorkspaceSyncSession(ctx, sqlcgen.LockWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if err != nil {
			return storeError("lock workspace sync session for download action acknowledgement", "workspace sync session", err)
		}
		if err := requireActiveWorkspaceSyncDeviceSession(ctx, tx, deviceAccess, session.OwnerID, workspaceID, session.ReplicaID, session.FolderID.String); err != nil {
			return err
		}
		var plannedAction string
		var plannedSHA pgtype.Text
		var plannedSize int64
		var applied bool
		if err := tx.QueryRow(ctx, `
SELECT action, sha256, size_bytes, applied
FROM workspace_sync_session_download_actions
WHERE workspace_id=$1 AND session_id=$2 AND file_path=$3
FOR UPDATE`, workspaceID, sessionID, path).Scan(&plannedAction, &plannedSHA, &plannedSize, &applied); err != nil {
			return storeError("get workspace sync download action", "workspace download action", err)
		}
		matchesSHA := !plannedSHA.Valid && digest == "" || plannedSHA.Valid && plannedSHA.String == digest
		if plannedAction != string(action) || !matchesSHA || plannedSize != sizeBytes {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace download action does not match the session plan")
		}
		if applied {
			return nil
		}
		if session.State != string(workspace.SyncPlanned) && session.State != string(workspace.SyncTransferring) &&
			session.State != string(workspace.SyncVerifying) && session.State != string(workspace.SyncApplying) {
			return domain.NewError(domain.CodeInvalidState, "workspace sync session does not accept action acknowledgements")
		}
		command, err := tx.Exec(ctx, `
UPDATE workspace_sync_session_download_actions SET applied=true
WHERE workspace_id=$1 AND session_id=$2 AND file_path=$3 AND applied=false`, workspaceID, sessionID, path)
		if err != nil {
			return fmt.Errorf("acknowledge workspace sync download action: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace download action changed during acknowledgement")
		}
		var blocksPending, actionsPending bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM workspace_sync_session_blocks
  WHERE workspace_id=$1 AND session_id=$2 AND received=false
), EXISTS (
  SELECT 1 FROM workspace_sync_session_download_actions
  WHERE workspace_id=$1 AND session_id=$2 AND applied=false
)`, workspaceID, sessionID).Scan(&blocksPending, &actionsPending); err != nil {
			return fmt.Errorf("check workspace sync download progress: %w", err)
		}
		state, stage := workspace.SyncApplying, workspace.SyncStageApply
		if blocksPending {
			state, stage = workspace.SyncTransferring, workspace.SyncStageTransfer
		} else if actionsPending {
			state, stage = workspace.SyncApplying, workspace.SyncStageApply
		}
		if _, err := tx.Exec(ctx, `
UPDATE workspace_sync_sessions SET state=$3, stage=$4, updated_at=now()
WHERE workspace_id=$1 AND id=$2`, workspaceID, sessionID, string(state), string(stage)); err != nil {
			return fmt.Errorf("advance workspace sync action stage: %w", err)
		}
		return nil
	})
}

func validateWorkspaceSyncCommitReceipt(workspaceID, sessionID string, idempotency app.IdempotencyRecord) error {
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) ||
		idempotency.Operation != "workspace.sync.commit" || idempotency.ResourceID != sessionID {
		return domain.InvalidArgument("sync_session", "workspace sync commit fields are invalid")
	}
	return validateIdempotency(idempotency)
}

func readWorkspaceSyncCommitReceipt(ctx context.Context, tx pgx.Tx, workspaceID, sessionID string, idempotency app.IdempotencyRecord, lock bool) (app.WorkspaceSyncCommitReceipt, error) {
	if err := validateWorkspaceSyncCommitReceipt(workspaceID, sessionID, idempotency); err != nil {
		return app.WorkspaceSyncCommitReceipt{}, err
	}
	query := `SELECT request_hash, workspace_id, session_id, state
FROM workspace_sync_commit_receipts WHERE owner_id=$1 AND operation=$2 AND key=$3`
	if lock {
		query += ` FOR UPDATE`
	}
	var requestHash []byte
	var storedWorkspace, storedSession, state string
	if err := tx.QueryRow(ctx, query, idempotency.OwnerID, idempotency.Operation, idempotency.Key).
		Scan(&requestHash, &storedWorkspace, &storedSession, &state); err != nil {
		return app.WorkspaceSyncCommitReceipt{}, storeError("read workspace sync commit receipt", "workspace sync commit receipt", err)
	}
	if !bytes.Equal(requestHash, idempotency.RequestHash) || storedWorkspace != workspaceID || storedSession != sessionID {
		return app.WorkspaceSyncCommitReceipt{}, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was used for a different request")
	}
	receipt := app.WorkspaceSyncCommitReceipt{State: app.WorkspaceSyncCommitState(state)}
	switch receipt.State {
	case app.WorkspaceSyncCommitPending, app.WorkspaceSyncCommitQuotaRejected:
	case app.WorkspaceSyncCommitSucceeded:
		row, err := sqlcgen.New(tx).GetWorkspaceSyncSession(ctx, sqlcgen.GetWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if err != nil {
			return app.WorkspaceSyncCommitReceipt{}, storeError("read completed workspace sync session", "workspace sync session", err)
		}
		if row.OwnerID != idempotency.OwnerID {
			return app.WorkspaceSyncCommitReceipt{}, domain.NewError(domain.CodeInternal, "workspace sync commit receipt owner does not match")
		}
		receipt.Result, err = syncSessionDomain(ctx, tx, sqlcgen.New(tx), row)
		if err != nil {
			return app.WorkspaceSyncCommitReceipt{}, err
		}
	default:
		return app.WorkspaceSyncCommitReceipt{}, domain.NewError(domain.CodeInternal, "workspace sync commit receipt state is invalid")
	}
	return receipt, nil
}

func (s *Store) ReserveWorkspaceSyncCommit(ctx context.Context, workspaceID, sessionID string, idempotency app.IdempotencyRecord) (app.WorkspaceSyncCommitReceipt, error) {
	if err := validateWorkspaceSyncCommitReceipt(workspaceID, sessionID, idempotency); err != nil {
		return app.WorkspaceSyncCommitReceipt{}, err
	}
	var receipt app.WorkspaceSyncCommitReceipt
	err := s.withWorkspaceSyncSessionMutationTx(ctx, workspaceID, sessionID, func(tx pgx.Tx) error {
		deviceAccess, err := prepareWorkspaceSyncDeviceSessionMutation(ctx, tx, workspaceID, sessionID)
		if err != nil {
			return err
		}
		if err := lockWorkspaceForSyncMutation(ctx, tx, workspaceID); err != nil {
			return err
		}
		if err := ensureNoWorkspaceSyncReceive(ctx, tx, workspaceID, ""); err != nil {
			return err
		}
		var sessionOwner, replicaID, direction string
		var sessionFolderID pgtype.Text
		if err := tx.QueryRow(ctx, `SELECT owner_id,replica_id,direction,folder_id FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2`, workspaceID, sessionID).
			Scan(&sessionOwner, &replicaID, &direction, &sessionFolderID); err != nil {
			return storeError("read workspace sync session before commit reservation", "workspace sync session", err)
		}
		if sessionOwner != idempotency.OwnerID {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		}
		var readOnly bool
		if err := tx.QueryRow(ctx, `SELECT is_read_only FROM workspaces WHERE id=$1`, workspaceID).Scan(&readOnly); err != nil {
			return storeError("read workspace mode before sync commit reservation", "workspace", err)
		}
		if readOnly && direction != string(workspace.SyncDownload) {
			return domain.NewError(domain.CodeForbidden, "workspace is shared as read-only")
		}
		if err := requireActiveWorkspaceSyncDeviceSession(ctx, tx, deviceAccess, sessionOwner, workspaceID, replicaID, sessionFolderID.String); err != nil {
			return err
		}
		// Keep keys written by the previous commit path in the same idempotency namespace.
		stored, exists, err := findIdempotency(ctx, tx, idempotency)
		if err != nil {
			return err
		}
		if exists && stored.ResourceID != sessionID {
			return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key is bound to another sync session")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_sync_commit_receipts
(owner_id, operation, key, request_hash, workspace_id, session_id, state)
VALUES ($1, $2, $3, $4, $5, $6, 'pending')
ON CONFLICT (owner_id, operation, key) DO NOTHING`, idempotency.OwnerID, idempotency.Operation,
			idempotency.Key, idempotency.RequestHash, workspaceID, sessionID); err != nil {
			return fmt.Errorf("reserve workspace sync commit receipt: %w", err)
		}
		receipt, err = readWorkspaceSyncCommitReceipt(ctx, tx, workspaceID, sessionID, idempotency, false)
		return err
	})
	return receipt, err
}

func (s *Store) InspectWorkspaceSyncCommit(ctx context.Context, workspaceID, sessionID string, idempotency app.IdempotencyRecord) (app.WorkspaceSyncCommitReceipt, error) {
	var receipt app.WorkspaceSyncCommitReceipt
	err := s.withWorkspaceSyncSessionMutationTx(ctx, workspaceID, sessionID, func(tx pgx.Tx) error {
		deviceAccess, err := prepareWorkspaceSyncDeviceSessionMutation(ctx, tx, workspaceID, sessionID)
		if err != nil {
			return err
		}
		if err := lockWorkspaceForSyncMutation(ctx, tx, workspaceID); err != nil {
			return err
		}
		var sessionOwner, replicaID string
		var sessionFolderID pgtype.Text
		if err := tx.QueryRow(ctx, `SELECT owner_id,replica_id,folder_id FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2`, workspaceID, sessionID).
			Scan(&sessionOwner, &replicaID, &sessionFolderID); err != nil {
			return storeError("read workspace sync session before commit inspection", "workspace sync session", err)
		}
		if sessionOwner != idempotency.OwnerID {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		}
		if err := requireActiveWorkspaceSyncDeviceSession(ctx, tx, deviceAccess, sessionOwner, workspaceID, replicaID, sessionFolderID.String); err != nil {
			return err
		}
		receipt, err = readWorkspaceSyncCommitReceipt(ctx, tx, workspaceID, sessionID, idempotency, true)
		return err
	})
	return receipt, err
}

func saveWorkspaceSyncCommitSuccess(ctx context.Context, tx pgx.Tx, idempotency app.IdempotencyRecord) error {
	updated, err := tx.Exec(ctx, `UPDATE workspace_sync_commit_receipts SET state='succeeded', updated_at=now()
WHERE owner_id=$1 AND operation=$2 AND key=$3 AND state='pending'`, idempotency.OwnerID, idempotency.Operation, idempotency.Key)
	if err != nil {
		return fmt.Errorf("save workspace sync commit receipt: %w", err)
	}
	if updated.RowsAffected() != 1 {
		return domain.NewError(domain.CodeInternal, "workspace sync commit receipt changed")
	}
	return nil
}

func (s *Store) CommitWorkspaceSyncSession(ctx context.Context, workspaceID, sessionID string, idempotency app.IdempotencyRecord) (workspace.SyncSession, bool, error) {
	if err := validateWorkspaceSyncCommitReceipt(workspaceID, sessionID, idempotency); err != nil {
		return workspace.SyncSession{}, false, err
	}
	var sessionOwnerID, expectedReplicaID, expectedDirection string
	var expectedFolderID pgtype.Text
	if err := s.pool.QueryRow(ctx, `SELECT owner_id, replica_id, direction, folder_id FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2`, workspaceID, sessionID).
		Scan(&sessionOwnerID, &expectedReplicaID, &expectedDirection, &expectedFolderID); err != nil {
		return workspace.SyncSession{}, false, storeError("read workspace sync session before commit", "workspace sync session", err)
	}
	if sessionOwnerID != idempotency.OwnerID {
		return workspace.SyncSession{}, false, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	if !validReplicaID(expectedReplicaID) {
		return workspace.SyncSession{}, false, domain.NewError(domain.CodeInternal, "stored workspace sync replica is invalid")
	}
	hostReplica, hostScoped := parseHostSyncReplicaID(expectedReplicaID)
	sessionScoped := strings.HasPrefix(expectedReplicaID, "session:")
	if sessionScoped && expectedReplicaID != "session:"+sessionOwnerID {
		return workspace.SyncSession{}, false, domain.NewError(domain.CodeInternal, "stored workspace sync session owner does not match its replica")
	}
	if hostScoped && hostReplica.workspaceID != workspaceID {
		return workspace.SyncSession{}, false, domain.NewError(domain.CodeWorkspaceConflict, "Host workspace sync replica does not match its session")
	}
	var result workspace.SyncSession
	replayed := false
	var rejected error
	var err error
	applyCommit := func(tx pgx.Tx) error {
		// Host authorization is revalidated in the binding transition lock order
		// so detach or rebind cannot race a revision commit.
		var replicaID string
		if err := tx.QueryRow(ctx, `SELECT replica_id FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2`, workspaceID, sessionID).Scan(&replicaID); err != nil {
			return storeError("read workspace sync replica before commit", "workspace sync session", err)
		}
		if replicaID != expectedReplicaID {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace sync replica changed before commit")
		}
		deviceAccess, err := prepareWorkspaceSyncDeviceReplica(ctx, tx, idempotency.OwnerID, workspaceID, replicaID)
		if err != nil {
			return err
		}
		if expectedFolderID.Valid {
			if hostScoped {
				return domain.NewError(domain.CodeForbidden, "project uploads cannot use Host replicas")
			}
			if err := lockWorkspaceSyncProjectRegistrationHost(ctx, tx, idempotency.OwnerID, workspaceID, expectedFolderID.String); err != nil {
				return err
			}
		}
		if deviceAccess != nil {
			if err := lockTaskHubOperationWorkspaceSyncCommitTx(ctx, tx, workspaceID, sessionID, deviceAccess.installationID); err != nil {
				return err
			}
		}
		if hostScoped {
			var ownerID, state string
			var generation int64
			var deletedAt pgtype.Timestamptz
			if err := tx.QueryRow(ctx, `SELECT owner_id, state, runtime_generation, deleted_at FROM hosts WHERE id=$1 FOR UPDATE`, hostReplica.hostID).
				Scan(&ownerID, &state, &generation, &deletedAt); err != nil {
				return storeError("lock Host before workspace sync commit", "host", err)
			}
			if ownerID != idempotency.OwnerID || generation != hostReplica.generation ||
				(state != string(host.StateCreating) && state != string(host.StateRunning)) || deletedAt.Valid {
				return domain.NewError(domain.CodeInvalidState, "Host workspace sync authorization changed before commit")
			}
		}
		var workspaceOwner, workspaceState string
		var workspaceReadOnly bool
		var currentRevision, currentBytes, quotaBytes, currentFileCount, revisionBytes int64
		if err := tx.QueryRow(ctx, `
			SELECT owner_id, state, revision, used_bytes, quota_bytes, is_read_only
			FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&workspaceOwner, &workspaceState, &currentRevision, &currentBytes, &quotaBytes, &workspaceReadOnly); err != nil {
			return storeError("lock workspace for sync commit", "workspace", err)
		}
		if workspaceOwner != idempotency.OwnerID {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if workspaceReadOnly && expectedDirection != string(workspace.SyncDownload) {
			return domain.NewError(domain.CodeForbidden, "workspace is shared as read-only")
		}
		if err := ensureNoWorkspaceSyncReceive(ctx, tx, workspaceID, ""); err != nil {
			return err
		}
		// Read after acquiring the Workspace lock so a waiting transaction sees the new revision.
		if err := tx.QueryRow(ctx, `SELECT file_count, total_bytes FROM workspace_revisions WHERE workspace_id=$1 AND revision=$2`,
			workspaceID, currentRevision).Scan(&currentFileCount, &revisionBytes); err != nil {
			return storeError("read locked workspace revision for sync commit", "workspace revision", err)
		}
		if hostScoped {
			var bindingOwner, bindingState string
			var bindingVersion int64
			if err := tx.QueryRow(ctx, `SELECT owner_id, binding_version, state FROM workspace_bindings WHERE host_id=$1 AND workspace_id=$2 FOR UPDATE`,
				hostReplica.hostID, workspaceID).Scan(&bindingOwner, &bindingVersion, &bindingState); err != nil {
				return storeError("lock Host workspace binding before sync commit", "workspace binding", err)
			}
			if bindingOwner != idempotency.OwnerID || bindingVersion != hostReplica.bindingVersion ||
				(bindingState != string(workspace.BindingPreparing) && bindingState != string(workspace.BindingActive) && bindingState != string(workspace.BindingUnbinding)) {
				return bindingStateConflict(ctx, tx, hostReplica.hostID, workspaceID)
			}
		}
		if currentBytes != revisionBytes {
			return domain.NewError(domain.CodeInternal, "workspace storage usage does not match its latest manifest")
		}
		q := sqlcgen.New(tx)
		row, err := q.LockWorkspaceSyncSession(ctx, sqlcgen.LockWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if err != nil {
			return storeError("lock workspace sync session for commit", "workspace sync session", err)
		}
		if workspaceOwner != row.OwnerID {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		}
		if idempotency.OwnerID != row.OwnerID {
			return domain.NewError(domain.CodeForbidden, "workspace sync owner does not match")
		}
		if row.ReplicaID != replicaID || row.FolderID.Valid != expectedFolderID.Valid ||
			row.FolderID.Valid && row.FolderID.String != expectedFolderID.String {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace sync replica changed before commit")
		}
		var projectFolder *workspace.SyncFolder
		if row.FolderID.Valid {
			folder, err := lockWorkspaceSyncProjectUploadFolder(ctx, tx, row.OwnerID, workspaceID, row.FolderID.String)
			if err != nil {
				return err
			}
			if len(row.Scope) != 1 || row.Scope[0] != folder.RelativePath {
				return domain.NewError(domain.CodeWorkspaceUnavailable, "project upload scope does not match its registered folder")
			}
			projectFolder = &folder
		} else if err := requireWorkspaceRootSyncTarget(ctx, tx, workspaceOwner, workspaceID); err != nil {
			return err
		}
		if err := requireActiveWorkspaceSyncDeviceSession(ctx, tx, deviceAccess, row.OwnerID, workspaceID, row.ReplicaID, row.FolderID.String); err != nil {
			return err
		}
		stored, err := readWorkspaceSyncCommitReceipt(ctx, tx, workspaceID, sessionID, idempotency, true)
		if err != nil {
			return err
		}
		if stored.State == app.WorkspaceSyncCommitQuotaRejected {
			rejected = domain.NewError(domain.CodeQuotaExceeded, "workspace storage quota would be exceeded")
			return nil
		}
		if stored.State == app.WorkspaceSyncCommitSucceeded {
			result, replayed = stored.Result, true
			return nil
		}
		projectBaselineMatches := true
		if projectFolder != nil {
			projectBaselineMatches, err = workspaceSyncSessionBaselineMatches(ctx, tx, row, *projectFolder)
			if err != nil {
				return err
			}
		}
		if projectFolder != nil && !projectBaselineMatches {
			if err := finishWorkspaceSyncProjectBaselineConflict(ctx, tx, q, row, *projectFolder, currentRevision, idempotency, &result); err != nil {
				return err
			}
			return nil
		}
		if isTerminalSyncState(workspace.SyncState(row.State)) {
			result, err = syncSessionDomain(ctx, tx, q, row)
			if err != nil {
				return err
			}
			if result.State == workspace.SyncExpired || result.State == workspace.SyncFailed {
				return nil
			}
			if result.State == workspace.SyncCompleted {
				if err := enqueueWorkspaceCurrentMaterialization(ctx, tx, workspaceID, result.TargetRevision); err != nil {
					return err
				}
				if err := reconcileTaskHubOperationWorkspaceSyncCommitTx(ctx, tx, workspaceID, sessionID, result.TargetRevision); err != nil {
					return err
				}
			}
			return saveWorkspaceSyncCommitSuccess(ctx, tx, idempotency)
		}
		if workspaceState != string(workspace.StateReady) {
			return domain.NewError(domain.CodeWorkspaceBusy, "workspace is not available for synchronization")
		}
		blocks, err := q.ListWorkspaceSyncTransferBlocks(ctx, sqlcgen.ListWorkspaceSyncTransferBlocksParams{WorkspaceID: workspaceID, SessionID: sessionID})
		if err != nil {
			return fmt.Errorf("list workspace sync blocks before commit: %w", err)
		}
		for _, block := range blocks {
			if !block.Received {
				return domain.NewError(domain.CodeWorkspaceBusy, "workspace sync is missing acknowledged blocks")
			}
		}
		actions, err := q.ListWorkspaceSyncDownloadActions(ctx, sqlcgen.ListWorkspaceSyncDownloadActionsParams{
			WorkspaceID: workspaceID, SessionID: sessionID,
		})
		if err != nil {
			return fmt.Errorf("list workspace sync download actions before commit: %w", err)
		}
		for _, action := range actions {
			if !action.Applied {
				return domain.NewError(domain.CodeWorkspaceBusy, "workspace sync is missing applied download actions")
			}
		}

		uploadFiles, err := decodeWorkspaceFiles(row.UploadFiles)
		if err != nil {
			return err
		}
		deletions := append([]string(nil), row.UploadDeletedPaths...)
		uploadEmptyDirectories := nonNilStrings(row.UploadEmptyDirectories)
		deletedDirectories := nonNilStrings(row.UploadDeletedDirectories)
		directorySession := workspace.SyncSession{WorkspaceID: workspaceID, ID: sessionID}
		if err := loadWorkspaceSyncSessionDirectories(ctx, tx, workspaceID, sessionID, &directorySession); err != nil {
			return err
		}
		uploadDirectories := directorySession.UploadDirectories
		directoryPaths := syncDirectoryMutationPaths(uploadEmptyDirectories, deletedDirectories)
		metadataPaths := make([]string, 0, len(uploadDirectories)+len(deletedDirectories))
		for _, directory := range uploadDirectories {
			metadataPaths = append(metadataPaths, directory.Path)
		}
		metadataPaths = append(metadataPaths, deletedDirectories...)
		mutationPaths := uniqueWorkspacePaths(append(append(syncMutationPaths(uploadFiles, deletions), directoryPaths...), metadataPaths...))
		pinnedFiles, err := loadWorkspaceRevisionFiles(ctx, tx, workspaceID, row.PinnedRevision, mutationPaths)
		if err != nil {
			return err
		}
		currentFiles, err := loadWorkspaceRevisionFiles(ctx, tx, workspaceID, currentRevision, mutationPaths)
		if err != nil {
			return err
		}
		pinnedDirectories, err := loadWorkspaceRevisionEmptyDirectories(ctx, tx, workspaceID, row.PinnedRevision, mutationPaths)
		if err != nil {
			return err
		}
		currentDirectories, err := loadWorkspaceRevisionEmptyDirectories(ctx, tx, workspaceID, currentRevision, mutationPaths)
		if err != nil {
			return err
		}
		pinnedByPath := fileMapByFoldedPath(pinnedFiles)
		currentByPath := fileMapByFoldedPath(currentFiles)
		pinnedDirectoriesByPath := directoryMapByFoldedPath(pinnedDirectories)
		currentDirectoriesByPath := directoryMapByFoldedPath(currentDirectories)
		pinnedMetadata, err := loadWorkspaceRevisionDirectories(ctx, tx, workspaceID, row.PinnedRevision, metadataPaths)
		if err != nil {
			return err
		}
		currentMetadata, err := loadWorkspaceRevisionDirectories(ctx, tx, workspaceID, currentRevision, metadataPaths)
		if err != nil {
			return err
		}
		pinnedMetadataByPath := workspaceDirectoryMapByFoldedPath(pinnedMetadata)
		currentMetadataByPath := workspaceDirectoryMapByFoldedPath(currentMetadata)
		conflicts, err := decodeWorkspaceConflicts(row.Conflicts)
		if err != nil {
			return err
		}
		conflictPaths := make(map[string]bool, len(conflicts))
		for _, conflict := range conflicts {
			conflictPaths[strings.ToLower(conflict.Path)] = true
		}
		mutations := make([]syncMutation, 0, len(uploadFiles)+len(deletions))
		for _, file := range uploadFiles {
			key := strings.ToLower(file.Path)
			current, currentOK := currentByPath[key]
			pinned, pinnedOK := pinnedByPath[key]
			if conflictPaths[key] {
				continue
			}
			if currentOK && sameWorkspaceFile(current, file) {
				if _, currentDirectory := currentDirectoriesByPath[key]; currentDirectory {
					conflicts = append(conflicts, workspace.Conflict{Path: file.Path, Reason: "type_changed"})
					conflictPaths[key] = true
				}
				continue
			}
			currentDirectory, currentDirectoryOK := currentDirectoriesByPath[key]
			pinnedDirectory, pinnedDirectoryOK := pinnedDirectoriesByPath[key]
			if currentOK != pinnedOK || (currentOK && !sameWorkspaceFile(current, pinned)) || currentDirectoryOK != pinnedDirectoryOK || (currentDirectoryOK && currentDirectory != pinnedDirectory) {
				reason := "content_changed"
				conflictPath := file.Path
				if currentDirectoryOK != pinnedDirectoryOK || (currentDirectoryOK && currentDirectory != pinnedDirectory) {
					reason = "type_changed"
				}
				if currentOK && pinnedOK && current.Path != pinned.Path && strings.EqualFold(current.Path, pinned.Path) {
					reason = "case_collision"
					conflictPath = current.Path
				}
				conflicts = append(conflicts, workspace.Conflict{Path: conflictPath, Reason: reason, LocalSHA256: stringPointer(file.SHA256), RemoteSHA256: fileHashPointer(current, currentOK)})
				conflictPaths[key] = true
				continue
			}
			mutations = append(mutations, syncMutation{path: file.Path, file: &file})
		}
		for _, path := range deletions {
			key := strings.ToLower(path)
			current, currentOK := currentByPath[key]
			pinned, pinnedOK := pinnedByPath[key]
			if conflictPaths[key] {
				continue
			}
			currentDirectory, currentDirectoryOK := currentDirectoriesByPath[key]
			pinnedDirectory, pinnedDirectoryOK := pinnedDirectoriesByPath[key]
			if !currentOK {
				if currentDirectoryOK != pinnedDirectoryOK || (currentDirectoryOK && currentDirectory != pinnedDirectory) {
					conflicts = append(conflicts, workspace.Conflict{Path: path, Reason: "type_changed"})
					conflictPaths[key] = true
				}
				continue
			}
			if !pinnedOK || !sameWorkspaceFile(current, pinned) || currentDirectoryOK != pinnedDirectoryOK || (currentDirectoryOK && currentDirectory != pinnedDirectory) {
				conflicts = append(conflicts, workspace.Conflict{Path: path, Reason: "delete_changed", RemoteSHA256: fileHashPointer(current, currentOK)})
				conflictPaths[key] = true
				conflictPaths[key] = true
				continue
			}
			mutations = append(mutations, syncMutation{path: current.Path})
		}
		directoryMutations := make([]syncDirectoryMutation, 0, len(uploadEmptyDirectories)+len(deletedDirectories))
		for _, directory := range uploadEmptyDirectories {
			key := strings.ToLower(directory)
			current, currentOK := currentDirectoriesByPath[key]
			pinned, pinnedOK := pinnedDirectoriesByPath[key]
			if conflictPaths[key] {
				continue
			}
			currentFile, currentFileOK := currentByPath[key]
			pinnedFile, pinnedFileOK := pinnedByPath[key]
			if currentOK && current == directory && !currentFileOK {
				continue
			}
			if currentOK != pinnedOK || (currentOK && current != pinned) || currentFileOK != pinnedFileOK || (currentFileOK && !sameWorkspaceFile(currentFile, pinnedFile)) {
				conflicts = append(conflicts, workspace.Conflict{Path: directory, Reason: "type_changed"})
				conflictPaths[key] = true
				continue
			}
			directoryMutations = append(directoryMutations, syncDirectoryMutation{path: directory})
		}
		directoryMetadataMutations := make([]syncDirectoryMetadataMutation, 0, len(uploadDirectories)+len(deletedDirectories))
		for _, directory := range uploadDirectories {
			key := strings.ToLower(directory.Path)
			if conflictPaths[key] {
				continue
			}
			if _, currentFile := currentByPath[key]; currentFile {
				conflicts = append(conflicts, workspace.Conflict{Path: directory.Path, Reason: "type_changed"})
				conflictPaths[key] = true
				continue
			}
			current, currentOK := currentMetadataByPath[key]
			pinned, pinnedOK := pinnedMetadataByPath[key]
			if currentOK && workspace.SameDirectoryMetadata(current, directory) {
				continue
			}
			if currentOK != pinnedOK || currentOK && !workspace.SameDirectoryMetadata(current, pinned) {
				conflicts = append(conflicts, workspace.Conflict{Path: directory.Path, Reason: "metadata_changed"})
				conflictPaths[key] = true
				continue
			}
			directoryMetadataMutations = append(directoryMetadataMutations, syncDirectoryMetadataMutation{path: directory.Path, directory: &directory})
		}
		for _, directory := range deletedDirectories {
			key := strings.ToLower(directory)
			if conflictPaths[key] {
				continue
			}
			current, currentStored, err := currentDirectoryMetadataForPath(ctx, tx, workspaceID, currentRevision, directory)
			if err != nil {
				return err
			}
			pinned, pinnedStored, err := currentDirectoryMetadataForPath(ctx, tx, workspaceID, row.PinnedRevision, directory)
			if err != nil {
				return err
			}
			if !currentStored {
				continue
			}
			if !pinnedStored || !workspace.SameDirectoryMetadata(current, pinned) {
				conflicts = append(conflicts, workspace.Conflict{Path: directory, Reason: "metadata_changed"})
				conflictPaths[key] = true
				continue
			}
			directoryMetadataMutations = append(directoryMetadataMutations, syncDirectoryMetadataMutation{path: current.Path, delete: true})
		}
		for _, directory := range deletedDirectories {
			key := strings.ToLower(directory)
			current, currentOK := currentDirectoriesByPath[key]
			pinned, pinnedOK := pinnedDirectoriesByPath[key]
			if conflictPaths[key] {
				continue
			}
			currentFile, currentFileOK := currentByPath[key]
			pinnedFile, pinnedFileOK := pinnedByPath[key]
			if !currentOK {
				if currentFileOK != pinnedFileOK || (currentFileOK && !sameWorkspaceFile(currentFile, pinnedFile)) {
					conflicts = append(conflicts, workspace.Conflict{Path: directory, Reason: "type_changed"})
					conflictPaths[key] = true
				}
				continue
			}
			if !pinnedOK || current != pinned || currentFileOK != pinnedFileOK || (currentFileOK && !sameWorkspaceFile(currentFile, pinnedFile)) {
				conflicts = append(conflicts, workspace.Conflict{Path: directory, Reason: "delete_changed"})
				conflictPaths[key] = true
				continue
			}
			directoryMutations = append(directoryMutations, syncDirectoryMutation{path: current, delete: true})
		}
		if projectFolder != nil && len(conflicts) > 0 {
			conflictJSON, err := json.Marshal(nonNilConflicts(conflicts))
			if err != nil {
				return fmt.Errorf("encode conflicted project upload: %w", err)
			}
			if err := q.SetWorkspaceSyncSessionState(ctx, sqlcgen.SetWorkspaceSyncSessionStateParams{
				WorkspaceID: workspaceID, ID: sessionID, State: string(workspace.SyncConflict),
				Stage: string(workspace.SyncStageComplete), Conflicts: conflictJSON, TargetRevision: currentRevision,
			}); err != nil {
				return fmt.Errorf("finish conflicted project upload without applying mutations: %w", err)
			}
			finalRow, err := q.GetWorkspaceSyncSession(ctx, sqlcgen.GetWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
			if err != nil {
				return fmt.Errorf("read conflicted project upload: %w", err)
			}
			result, err = syncSessionDomain(ctx, tx, q, finalRow)
			if err != nil {
				return err
			}
			return saveWorkspaceSyncCommitSuccess(ctx, tx, idempotency)
		}
		if err := ensureWorkspaceMutationsBlockUsageStored(ctx, tx, workspaceID, mutations); err != nil {
			return err
		}
		if len(mutations) > 0 || len(directoryMutations) > 0 || len(directoryMetadataMutations) > 0 {
			var blockedByTaskFence bool
			linkedTaskID, linkedOperationKey := "", ""
			if row.TaskID.Valid {
				linkedTaskID = row.TaskID.String
			}
			if row.TaskHubOperationKey.Valid {
				linkedOperationKey = row.TaskHubOperationKey.String
			}
			if err := tx.QueryRow(ctx, `SELECT
EXISTS (SELECT 1 FROM task_operations WHERE workspace_id=$1 AND status IN ('prepared','dispatched','unknown')) OR
EXISTS (SELECT 1 FROM task_mcp_connector_operations o JOIN tasks t ON t.id=o.task_id
			WHERE t.workspace_id=$1 AND o.status IN ('dispatched','unknown')) OR
EXISTS (SELECT 1 FROM task_workspace_publish_receipts WHERE workspace_id=$1 AND state='pending') OR
EXISTS (SELECT 1 FROM task_hub_operations o WHERE o.workspace_id=$1 AND o.action='file.put'
  AND o.status IN ('prepared','dispatched','unknown') AND o.sync_stage IN ('pending','local_applied','sync_pending')
  AND NOT ($2::text<>'' AND o.task_id=$2 AND o.operation_key=$3))`, workspaceID, linkedTaskID, linkedOperationKey).Scan(&blockedByTaskFence); err != nil {
				return fmt.Errorf("check Task fences before Workspace revision commit: %w", err)
			}
			if blockedByTaskFence {
				return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has an in-flight Task operation or publish; retry synchronization after it settles")
			}
		}
		if _, err := tx.Exec(ctx, `SAVEPOINT workspace_sync_apply`); err != nil {
			return fmt.Errorf("start workspace sync apply savepoint: %w", err)
		}
		if err := applyWorkspaceSyncTreeMutations(ctx, tx, workspaceID, currentRevision, currentFileCount, currentBytes, quotaBytes,
			mutations, directoryMutations, directorySession.Directories, directoryMetadataMutations); err != nil {
			if domain.CodeOf(err) != domain.CodeQuotaExceeded {
				return err
			}
			if _, rollbackErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT workspace_sync_apply`); rollbackErr != nil {
				return fmt.Errorf("rollback workspace sync apply savepoint: %w", rollbackErr)
			}
			updated, markErr := tx.Exec(ctx, `UPDATE workspace_sync_commit_receipts SET state='quota_rejected', updated_at=now()
WHERE owner_id=$1 AND operation=$2 AND key=$3 AND state='pending'`, idempotency.OwnerID, idempotency.Operation, idempotency.Key)
			if markErr != nil {
				return fmt.Errorf("record workspace sync quota rejection: %w", markErr)
			}
			if updated.RowsAffected() != 1 {
				return domain.NewError(domain.CodeInternal, "workspace sync commit receipt changed")
			}
			rejected = err
			return nil
		}
		targetRevision := currentRevision
		if len(mutations) > 0 || len(directoryMutations) > 0 || len(directoryMetadataMutations) > 0 {
			targetRevision++
		}
		state := workspace.SyncCompleted
		if len(conflicts) > 0 {
			state = workspace.SyncConflict
		}
		conflictJSON, err := json.Marshal(nonNilConflicts(conflicts))
		if err != nil {
			return fmt.Errorf("encode workspace sync conflicts: %w", err)
		}
		if err := q.SetWorkspaceSyncSessionState(ctx, sqlcgen.SetWorkspaceSyncSessionStateParams{
			WorkspaceID: workspaceID, ID: sessionID, State: string(state), Stage: string(workspace.SyncStageComplete),
			Conflicts: conflictJSON, TargetRevision: targetRevision,
		}); err != nil {
			return fmt.Errorf("finish workspace sync session: %w", err)
		}
		if state == workspace.SyncCompleted && projectFolder != nil {
			projectReceiptSession := directorySession
			projectReceiptSession.Scope = nonNilStrings(row.Scope)
			projectReceiptSession.Files, err = decodeWorkspaceFiles(row.Files)
			if err != nil {
				return err
			}
			projectReceiptSession.EmptyDirectories = nonNilStrings(row.EmptyDirectories)
			if err := createWorkspaceSyncProjectUploadApplication(ctx, tx, row, *projectFolder, targetRevision, projectReceiptSession); err != nil {
				return err
			}
		}
		if state == workspace.SyncCompleted && row.AckCursor.Valid {
			cursor, err := workspace.DecodeChangeCursor(row.AckCursor.String)
			if err != nil || cursor.Revision > targetRevision {
				return domain.NewError(domain.CodeWorkspaceConflict, "workspace acknowledgement cursor is invalid")
			}
			if err := q.InsertWorkspaceSyncCursor(ctx, sqlcgen.InsertWorkspaceSyncCursorParams{
				WorkspaceID: workspaceID, OwnerID: row.OwnerID, ReplicaID: row.ReplicaID,
				Revision: cursor.Revision, FilePath: cursor.Path,
			}); err != nil {
				return fmt.Errorf("persist workspace sync cursor: %w", err)
			}
		}
		finalRow, err := q.GetWorkspaceSyncSession(ctx, sqlcgen.GetWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if err != nil {
			return fmt.Errorf("read committed workspace sync session: %w", err)
		}
		result, err = syncSessionDomain(ctx, tx, q, finalRow)
		if err != nil {
			return err
		}
		if result.State == workspace.SyncCompleted {
			if err := enqueueWorkspaceCurrentMaterialization(ctx, tx, workspaceID, targetRevision); err != nil {
				return err
			}
			if err := reconcileTaskHubOperationWorkspaceSyncCommitTx(ctx, tx, workspaceID, sessionID, targetRevision); err != nil {
				return err
			}
		}
		return saveWorkspaceSyncCommitSuccess(ctx, tx, idempotency)
	}
	if hostScoped {
		err = s.withApprovedWorkspaceSyncOwnerTx(ctx, sessionOwnerID, applyCommit)
	} else if sessionScoped {
		err = s.withApprovedWorkspaceSyncOwnerReadTx(ctx, sessionOwnerID, applyCommit)
	} else {
		err = s.WithTx(ctx, applyCommit)
	}
	if err != nil {
		return workspace.SyncSession{}, false, err
	}
	if rejected != nil {
		return workspace.SyncSession{}, false, rejected
	}
	return result, replayed, nil
}

func (s *Store) CancelWorkspaceSyncSession(ctx context.Context, workspaceID, sessionID string, idempotency app.IdempotencyRecord) (workspace.SyncSession, bool, error) {
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) || idempotency.Operation != "workspace.sync.cancel" || idempotency.ResourceID != sessionID {
		return workspace.SyncSession{}, false, domain.InvalidArgument("sync_session", "workspace sync cancel fields are invalid")
	}
	var result workspace.SyncSession
	replayed := false
	// Beginning unbind advances the binding fence. Let cancellation clean up a
	// session from the previous version while the binding is still unbinding.
	err := s.withWorkspaceSyncSessionMutationTx(ctx, workspaceID, sessionID, func(tx pgx.Tx) error {
		deviceAccess, err := prepareWorkspaceSyncDeviceSessionMutation(ctx, tx, workspaceID, sessionID)
		if err != nil {
			return err
		}
		if err := lockWorkspaceForSyncMutation(ctx, tx, workspaceID); err != nil {
			return err
		}
		q := sqlcgen.New(tx)
		row, err := q.LockWorkspaceSyncSession(ctx, sqlcgen.LockWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if err != nil {
			return storeError("lock workspace sync session for cancellation", "workspace sync session", err)
		}
		if err := requireActiveWorkspaceSyncDeviceSession(ctx, tx, deviceAccess, row.OwnerID, workspaceID, row.ReplicaID, row.FolderID.String); err != nil {
			return err
		}
		if idempotency.OwnerID != row.OwnerID {
			return domain.NewError(domain.CodeForbidden, "workspace sync owner does not match")
		}
		stored, wasReplay, err := reserveIdempotency(ctx, tx, idempotency, sessionID, "")
		if err != nil {
			return err
		}
		if wasReplay && stored.ResourceID != sessionID {
			return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key is bound to another sync session")
		}
		if wasReplay {
			result, err = syncSessionDomain(ctx, tx, q, row)
			replayed = err == nil
			return err
		}
		if isTerminalSyncState(workspace.SyncState(row.State)) {
			result, err = syncSessionDomain(ctx, tx, q, row)
			return err
		}
		cancelled, err := q.CancelWorkspaceSyncSession(ctx, sqlcgen.CancelWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace sync session changed during cancellation")
		}
		if err != nil {
			return fmt.Errorf("cancel workspace sync session: %w", err)
		}
		result, err = syncSessionDomain(ctx, tx, q, cancelled)
		return err
	}, true)
	if err != nil {
		return workspace.SyncSession{}, false, err
	}
	return result, replayed, nil
}

func (s *Store) FailWorkspaceSyncSession(ctx context.Context, workspaceID, sessionID string) error {
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) {
		return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceForSyncMutation(ctx, tx, workspaceID); err != nil {
			return err
		}
		q := sqlcgen.New(tx)
		row, err := q.LockWorkspaceSyncSession(ctx, sqlcgen.LockWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if err != nil {
			return storeError("lock workspace sync session for failure", "workspace sync session", err)
		}
		if isTerminalSyncState(workspace.SyncState(row.State)) {
			return nil
		}
		if err := q.SetWorkspaceSyncSessionState(ctx, sqlcgen.SetWorkspaceSyncSessionStateParams{
			WorkspaceID: workspaceID, ID: sessionID, State: string(workspace.SyncFailed), Stage: string(workspace.SyncStageComplete),
			Conflicts: row.Conflicts, TargetRevision: row.TargetRevision,
		}); err != nil {
			return fmt.Errorf("fail workspace sync session: %w", err)
		}
		return nil
	})
}

func validateSyncSession(value workspace.SyncSession) error {
	if !workspace.ValidSessionID(value.ID) || !domain.ValidWorkspaceID(value.WorkspaceID) || !domain.ValidUserID(value.OwnerID) ||
		!validReplicaID(value.ReplicaID) || !value.Direction.Valid() || !value.Policy.Valid() ||
		value.State != workspace.SyncPlanned || value.Stage != workspace.SyncStageCompare ||
		value.BaseRevision < 0 || value.PinnedRevision < 0 || value.TargetRevision != value.PinnedRevision ||
		value.PlannedBytes < 0 || value.ConfirmedBytes < 0 || value.ConfirmedBytes > value.PlannedBytes {
		return domain.InvalidArgument("sync_session", "workspace sync session fields are invalid")
	}
	if value.FolderID == "" {
		if value.FolderBaselineRevision != nil || value.SourceBindingID != "" || value.SourceBaselineSessionID != "" {
			return domain.InvalidArgument("sync_session", "workspace sync folder baseline is invalid")
		}
	} else if !workspace.ValidSyncFolderID(value.FolderID) || value.Direction != workspace.SyncUpload ||
		value.Policy != workspace.SyncCompareMerge || len(value.Scope) != 1 || value.TaskID != "" || value.TaskHubOperationKey != "" ||
		!workspace.ValidProjectSourceBindingID(value.SourceBindingID) || value.FolderBaselineRevision != nil ||
		value.SourceBaselineSessionID != "" && (!workspace.ValidSessionID(value.SourceBaselineSessionID) || value.SourceBaselineSessionID == value.ID) {
		return domain.InvalidArgument("sync_session", "project upload session fields are invalid")
	}
	if value.TaskID != "" && (!domain.ValidTaskID(value.TaskID) || len(value.TaskHubOperationKey) == 0 || len(value.TaskHubOperationKey) > 512 ||
		strings.TrimSpace(value.TaskHubOperationKey) != value.TaskHubOperationKey || strings.ContainsRune(value.TaskHubOperationKey, '\x00') ||
		!strings.HasPrefix(value.ReplicaID, "device:")) {
		return domain.InvalidArgument("sync_session", "Task-linked workspace sync fields are invalid")
	}
	if err := workspace.ValidateCreateSyncSession(workspace.CreateSyncSession{
		Direction: value.Direction, Policy: value.Policy, BaseRevision: value.BaseRevision, FolderID: value.FolderID,
		SourceBindingID: value.SourceBindingID, Scope: value.Scope,
		TaskID: value.TaskID, TaskHubOperationKey: value.TaskHubOperationKey,
		Files: value.Files, EmptyDirectories: value.EmptyDirectories,
		DeletedPaths: value.DeletedPaths, DeletedDirectories: value.DeletedDirectories, AckCursor: value.AckCursor,
	}, 0); err != nil {
		return domain.InvalidArgument("sync_session", "workspace sync manifest is invalid")
	}
	for _, directories := range [][]string{value.UploadEmptyDirectories, value.DownloadEmptyDirectories, value.UploadDeletedDirectories, value.DownloadDeletedDirectories} {
		if err := validateDirectoryList(directories); err != nil {
			return domain.InvalidArgument("sync_session", "workspace sync directory plan is invalid")
		}
	}
	for _, file := range value.Files {
		if err := workspace.ValidateFile(file); err != nil {
			return domain.InvalidArgument("sync_session", "workspace sync file is invalid")
		}
	}
	for _, file := range value.UploadFiles {
		if err := workspace.ValidateFile(file); err != nil {
			return domain.InvalidArgument("sync_session", "workspace sync upload file is invalid")
		}
	}
	for _, file := range value.DownloadFiles {
		if err := workspace.ValidateFile(file); err != nil {
			return domain.InvalidArgument("sync_session", "workspace sync download file is invalid")
		}
	}
	expectedActions := workspace.DownloadActionsForTreePlanWithMetadata(
		value.DownloadFiles, value.DownloadDeletedPaths, value.DownloadEmptyDirectories, value.DownloadDeletedDirectories, value.DownloadDirectories)
	if len(value.DownloadActions) != len(expectedActions) {
		return domain.InvalidArgument("sync_session", "workspace sync download actions do not match its plan")
	}
	for i, action := range value.DownloadActions {
		if err := workspace.ValidateDownloadAction(action); err != nil || action.Applied {
			return domain.InvalidArgument("sync_session", "workspace sync download action is invalid")
		}
		expected := expectedActions[i]
		if action.Path != expected.Path || action.Action != expected.Action || action.SizeBytes != expected.SizeBytes ||
			!sameSHA256Pointer(action.SHA256, expected.SHA256) {
			return domain.InvalidArgument("sync_session", "workspace sync download actions do not match its plan")
		}
	}
	return nil
}

func validateDirectoryList(directories []string) error {
	if len(directories) > workspace.MaxSyncFiles {
		return errors.New("directory list exceeds its item limit")
	}
	seen := make(map[string]struct{}, len(directories))
	for _, directory := range directories {
		if err := workspace.ValidateRelativePath(directory); err != nil {
			return err
		}
		folded := strings.ToLower(directory)
		if _, exists := seen[folded]; exists {
			return errors.New("directory list contains duplicate paths")
		}
		seen[folded] = struct{}{}
	}
	return nil
}

func sameSHA256Pointer(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func validReplicaID(value string) bool {
	if strings.HasPrefix(value, "session:") {
		return domain.ValidUserID(strings.TrimPrefix(value, "session:"))
	}
	if strings.HasPrefix(value, "device:") {
		return domain.ValidTokenID(strings.TrimPrefix(value, "device:"))
	}
	_, ok := parseHostSyncReplicaID(value)
	return ok
}

type hostSyncReplicaID struct {
	hostID         string
	generation     int64
	workspaceID    string
	bindingVersion int64
}

func parseHostSyncReplicaID(value string) (hostSyncReplicaID, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 5 || parts[0] != "host" || !strings.HasPrefix(parts[2], "g") || !strings.HasPrefix(parts[4], "b") {
		return hostSyncReplicaID{}, false
	}
	generationText := strings.TrimPrefix(parts[2], "g")
	bindingVersionText := strings.TrimPrefix(parts[4], "b")
	generation, generationErr := strconv.ParseInt(generationText, 10, 64)
	bindingVersion, bindingVersionErr := strconv.ParseInt(bindingVersionText, 10, 64)
	if !domain.ValidHostID(parts[1]) || !domain.ValidWorkspaceID(parts[3]) || generationErr != nil || bindingVersionErr != nil ||
		generation < 1 || bindingVersion < 1 || strconv.FormatInt(generation, 10) != generationText || strconv.FormatInt(bindingVersion, 10) != bindingVersionText {
		return hostSyncReplicaID{}, false
	}
	return hostSyncReplicaID{hostID: parts[1], generation: generation, workspaceID: parts[3], bindingVersion: bindingVersion}, true
}

func validTransferBlock(value workspace.TransferBlock) bool {
	return validPostgresSHA256(value.SHA256) && value.SizeBytes >= 1 && value.SizeBytes <= workspace.MaxSyncBlockBytes &&
		(value.Role == workspace.BlockUpload || value.Role == workspace.BlockDownload)
}

func validateSessionTransferBlocks(session workspace.SyncSession, blocks []workspace.TransferBlock) error {
	expected := make(map[string]workspace.Chunk)
	var planned int64
	addFiles := func(files []workspace.File, role workspace.BlockRole) error {
		for _, file := range files {
			for _, chunk := range file.Chunks {
				key := string(role) + ":" + chunk.SHA256
				if previous, ok := expected[key]; ok {
					if previous.SizeBytes != chunk.SizeBytes {
						return domain.InvalidArgument("sync_session", "workspace sync block sizes do not match their files")
					}
					continue
				}
				expected[key] = chunk
			}
		}
		return nil
	}
	if err := addFiles(session.UploadFiles, workspace.BlockUpload); err != nil {
		return err
	}
	if err := addFiles(session.DownloadFiles, workspace.BlockDownload); err != nil {
		return err
	}
	actual := make(map[string]struct{}, len(blocks))
	for _, block := range blocks {
		if !validTransferBlock(block) {
			return domain.InvalidArgument("sync_session", "workspace sync transfer block is invalid")
		}
		key := string(block.Role) + ":" + block.SHA256
		chunk, ok := expected[key]
		if !ok || chunk.SizeBytes != block.SizeBytes {
			return domain.InvalidArgument("sync_session", "workspace sync transfer block does not match its files")
		}
		if _, duplicate := actual[key]; duplicate {
			return domain.InvalidArgument("sync_session", "workspace sync transfer block is duplicated")
		}
		if block.Role == workspace.BlockDownload && block.Received {
			return domain.InvalidArgument("sync_session", "download blocks cannot be acknowledged before session creation")
		}
		actual[key] = struct{}{}
		if block.Role == workspace.BlockDownload || !block.Received {
			if planned > int64(^uint64(0)>>1)-block.SizeBytes {
				return domain.NewError(domain.CodeRequestTooLarge, "workspace transfer plan is too large")
			}
			planned += block.SizeBytes
		}
	}
	if len(actual) != len(expected) || planned != session.PlannedBytes {
		return domain.InvalidArgument("sync_session", "workspace sync transfer plan does not match its files or byte count")
	}
	return nil
}

func validateWorkspaceSyncReservationSet(workspaceID string, blocks []workspace.TransferBlock, reservations []app.WorkspaceBlockReservation) error {
	expected := make(map[string]struct{})
	for _, block := range blocks {
		if block.Role == workspace.BlockUpload && !block.Received {
			expected[block.SHA256] = struct{}{}
		}
	}
	actual := make(map[string]struct{}, len(reservations))
	for _, reservation := range reservations {
		if reservation.WorkspaceID != workspaceID || !validPostgresSHA256(reservation.Digest) || reservation.ReservedBytes <= 0 {
			return domain.InvalidArgument("sync_session", "workspace sync block reservation is invalid")
		}
		if _, ok := expected[reservation.Digest]; !ok {
			return domain.InvalidArgument("sync_session", "workspace sync block reservation does not match an upload block")
		}
		if _, duplicate := actual[reservation.Digest]; duplicate {
			return domain.InvalidArgument("sync_session", "workspace sync block reservation is duplicated")
		}
		actual[reservation.Digest] = struct{}{}
	}
	if len(actual) != len(expected) {
		return domain.InvalidArgument("sync_session", "workspace sync block reservations do not cover every upload block")
	}
	return nil
}

func validPostgresSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func workspaceSyncSessionParams(value workspace.SyncSession) (sqlcgen.CreateWorkspaceSyncSessionParams, error) {
	files, err := marshalNonNilJSON(value.Files)
	if err != nil {
		return sqlcgen.CreateWorkspaceSyncSessionParams{}, err
	}
	uploadFiles, err := marshalNonNilJSON(value.UploadFiles)
	if err != nil {
		return sqlcgen.CreateWorkspaceSyncSessionParams{}, err
	}
	downloadFiles, err := marshalNonNilJSON(value.DownloadFiles)
	if err != nil {
		return sqlcgen.CreateWorkspaceSyncSessionParams{}, err
	}
	conflicts, err := marshalNonNilJSON(value.Conflicts)
	if err != nil {
		return sqlcgen.CreateWorkspaceSyncSessionParams{}, err
	}
	ack := pgtype.Text{}
	if value.AckCursor != nil {
		ack = pgtype.Text{String: *value.AckCursor, Valid: true}
	}
	return sqlcgen.CreateWorkspaceSyncSessionParams{
		ID: value.ID, WorkspaceID: value.WorkspaceID, OwnerID: value.OwnerID, ReplicaID: value.ReplicaID,
		Direction: string(value.Direction), Policy: string(value.Policy), State: string(value.State), Stage: string(value.Stage),
		BaseRevision: value.BaseRevision, TargetRevision: value.TargetRevision, PinnedRevision: value.PinnedRevision,
		PlannedBytes: value.PlannedBytes, ConfirmedBytes: value.ConfirmedBytes,
		Files: files, UploadFiles: uploadFiles, DownloadFiles: downloadFiles,
		Scope: nonNilStrings(value.Scope), DeletedPaths: nonNilStrings(value.DeletedPaths), UploadDeletedPaths: nonNilStrings(value.UploadDeletedPaths),
		DownloadDeletedPaths:       nonNilStrings(value.DownloadDeletedPaths),
		EmptyDirectories:           nonNilStrings(value.EmptyDirectories),
		UploadEmptyDirectories:     nonNilStrings(value.UploadEmptyDirectories),
		DownloadEmptyDirectories:   nonNilStrings(value.DownloadEmptyDirectories),
		DeletedDirectories:         nonNilStrings(value.DeletedDirectories),
		UploadDeletedDirectories:   nonNilStrings(value.UploadDeletedDirectories),
		DownloadDeletedDirectories: nonNilStrings(value.DownloadDeletedDirectories),
		AckCursor:                  ack, Conflicts: conflicts,
		TaskID:              pgtype.Text{String: value.TaskID, Valid: value.TaskID != ""},
		TaskHubOperationKey: pgtype.Text{String: value.TaskHubOperationKey, Valid: value.TaskHubOperationKey != ""},
		FolderID:            pgtype.Text{String: value.FolderID, Valid: value.FolderID != ""},
		SourceBindingID:     pgtype.Text{String: value.SourceBindingID, Valid: value.SourceBindingID != ""},
		SourceBaselineSessionID: pgtype.Text{String: value.SourceBaselineSessionID,
			Valid: value.SourceBaselineSessionID != ""},
		FolderBaselineRevision: func() pgtype.Int8 {
			if value.FolderBaselineRevision == nil {
				return pgtype.Int8{}
			}
			return pgtype.Int8{Int64: *value.FolderBaselineRevision, Valid: true}
		}(),
	}, nil
}

func persistWorkspaceSyncSessionDirectories(ctx context.Context, tx pgx.Tx, value workspace.SyncSession) error {
	directories, err := marshalNonNilJSON(value.Directories)
	if err != nil {
		return fmt.Errorf("encode workspace sync directories: %w", err)
	}
	upload, err := marshalNonNilJSON(value.UploadDirectories)
	if err != nil {
		return fmt.Errorf("encode workspace sync upload directories: %w", err)
	}
	download, err := marshalNonNilJSON(value.DownloadDirectories)
	if err != nil {
		return fmt.Errorf("encode workspace sync download directories: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspace_sync_session_directories (workspace_id,session_id,directories,upload_directories,download_directories)
VALUES ($1,$2,$3,$4,$5)`, value.WorkspaceID, value.ID, directories, upload, download); err != nil {
		return fmt.Errorf("persist workspace sync directories: %w", err)
	}
	return nil
}

func loadWorkspaceSyncSessionDirectories(ctx context.Context, tx pgx.Tx, workspaceID, sessionID string, value *workspace.SyncSession) error {
	var directories, upload, download []byte
	err := tx.QueryRow(ctx, `SELECT directories,upload_directories,download_directories
FROM workspace_sync_session_directories WHERE workspace_id=$1 AND session_id=$2`, workspaceID, sessionID).
		Scan(&directories, &upload, &download)
	if errors.Is(err, pgx.ErrNoRows) {
		value.Directories, value.UploadDirectories, value.DownloadDirectories = []workspace.Directory{}, []workspace.Directory{}, []workspace.Directory{}
		return nil
	}
	if err != nil {
		return fmt.Errorf("load workspace sync directory metadata: %w", err)
	}
	for _, entry := range []struct {
		encoded []byte
		target  *[]workspace.Directory
	}{{directories, &value.Directories}, {upload, &value.UploadDirectories}, {download, &value.DownloadDirectories}} {
		if err := json.Unmarshal(entry.encoded, entry.target); err != nil {
			return fmt.Errorf("decode workspace sync directory metadata: %w", err)
		}
		if *entry.target == nil {
			*entry.target = []workspace.Directory{}
		}
	}
	return nil
}

func syncSessionDomain(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, row sqlcgen.WorkspaceSyncSession) (workspace.SyncSession, error) {
	var value workspace.SyncSession
	if err := json.Unmarshal(row.Files, &value.Files); err != nil {
		return workspace.SyncSession{}, fmt.Errorf("decode workspace sync request files: %w", err)
	}
	if err := json.Unmarshal(row.UploadFiles, &value.UploadFiles); err != nil {
		return workspace.SyncSession{}, fmt.Errorf("decode workspace sync upload files: %w", err)
	}
	if err := json.Unmarshal(row.DownloadFiles, &value.DownloadFiles); err != nil {
		return workspace.SyncSession{}, fmt.Errorf("decode workspace sync download files: %w", err)
	}
	if err := json.Unmarshal(row.Conflicts, &value.Conflicts); err != nil {
		return workspace.SyncSession{}, fmt.Errorf("decode workspace sync conflicts: %w", err)
	}
	if value.Files == nil {
		value.Files = []workspace.File{}
	}
	if value.UploadFiles == nil {
		value.UploadFiles = []workspace.File{}
	}
	if value.DownloadFiles == nil {
		value.DownloadFiles = []workspace.File{}
	}
	if value.Conflicts == nil {
		value.Conflicts = []workspace.Conflict{}
	}
	value.ID, value.WorkspaceID, value.OwnerID, value.ReplicaID = row.ID, row.WorkspaceID, row.OwnerID, row.ReplicaID
	if row.FolderID.Valid {
		value.FolderID = row.FolderID.String
	}
	if row.FolderBaselineRevision.Valid {
		value.FolderBaselineRevision = &row.FolderBaselineRevision.Int64
	}
	if row.SourceBindingID.Valid {
		value.SourceBindingID = row.SourceBindingID.String
	}
	if row.SourceBaselineSessionID.Valid {
		value.SourceBaselineSessionID = row.SourceBaselineSessionID.String
	}
	if row.TaskID.Valid {
		value.TaskID = row.TaskID.String
	}
	if row.TaskHubOperationKey.Valid {
		value.TaskHubOperationKey = row.TaskHubOperationKey.String
	}
	value.Direction, value.Policy, value.State, value.Stage = workspace.SyncDirection(row.Direction), workspace.SyncPolicy(row.Policy), workspace.SyncState(row.State), workspace.SyncStage(row.Stage)
	value.BaseRevision, value.TargetRevision, value.PinnedRevision = row.BaseRevision, row.TargetRevision, row.PinnedRevision
	value.PlannedBytes, value.ConfirmedBytes = row.PlannedBytes, row.ConfirmedBytes
	value.Scope, value.DeletedPaths, value.UploadDeletedPaths = nonNilStrings(row.Scope), nonNilStrings(row.DeletedPaths), nonNilStrings(row.UploadDeletedPaths)
	value.DownloadDeletedPaths = nonNilStrings(row.DownloadDeletedPaths)
	value.EmptyDirectories = nonNilStrings(row.EmptyDirectories)
	value.UploadEmptyDirectories = nonNilStrings(row.UploadEmptyDirectories)
	value.DownloadEmptyDirectories = nonNilStrings(row.DownloadEmptyDirectories)
	value.DeletedDirectories = nonNilStrings(row.DeletedDirectories)
	value.UploadDeletedDirectories = nonNilStrings(row.UploadDeletedDirectories)
	value.DownloadDeletedDirectories = nonNilStrings(row.DownloadDeletedDirectories)
	if err := loadWorkspaceSyncSessionDirectories(ctx, tx, row.WorkspaceID, row.ID, &value); err != nil {
		return workspace.SyncSession{}, err
	}
	if row.AckCursor.Valid {
		value.AckCursor = &row.AckCursor.String
	}
	value.CreatedAt, value.UpdatedAt = row.CreatedAt.Time, row.UpdatedAt.Time
	if row.SourceBindingID.Valid {
		if err := loadWorkspaceSyncProjectHostApplication(ctx, tx, row, &value); err != nil {
			return workspace.SyncSession{}, err
		}
	}
	blocks, err := q.ListWorkspaceSyncTransferBlocks(ctx, sqlcgen.ListWorkspaceSyncTransferBlocksParams{WorkspaceID: row.WorkspaceID, SessionID: row.ID})
	if err != nil {
		return workspace.SyncSession{}, fmt.Errorf("list workspace sync session blocks: %w", err)
	}
	value.TransferBlocks = make([]workspace.TransferBlock, 0, len(blocks))
	for _, block := range blocks {
		value.TransferBlocks = append(value.TransferBlocks, workspace.TransferBlock{
			Chunk: workspace.Chunk{SHA256: block.Sha256, SizeBytes: block.SizeBytes},
			Role:  workspace.BlockRole(block.Role), Received: block.Received,
		})
	}
	value.TransferBlocks = nonNilTransferBlocks(value.TransferBlocks)
	actions, err := q.ListWorkspaceSyncDownloadActions(ctx, sqlcgen.ListWorkspaceSyncDownloadActionsParams{
		WorkspaceID: row.WorkspaceID, SessionID: row.ID,
	})
	if err != nil {
		return workspace.SyncSession{}, fmt.Errorf("list workspace sync download actions: %w", err)
	}
	value.DownloadActions = make([]workspace.DownloadAction, 0, len(actions))
	for _, action := range actions {
		var digest *string
		if action.Sha256.Valid {
			value := action.Sha256.String
			digest = &value
		}
		value.DownloadActions = append(value.DownloadActions, workspace.DownloadAction{
			Path: action.FilePath, Action: workspace.ChangeAction(action.Action), SHA256: digest,
			SizeBytes: action.SizeBytes, Applied: action.Applied,
		})
	}
	return value, nil
}

func loadWorkspaceSyncProjectHostApplication(ctx context.Context, tx pgx.Tx, row sqlcgen.WorkspaceSyncSession,
	value *workspace.SyncSession) error {
	var state, hostID string
	var bindingVersion, targetRevision int64
	var manifestSHA256, encodedImpacts []byte
	var generation, appliedRevision pgtype.Int8
	var syncState pgtype.Text
	err := tx.QueryRow(ctx, `SELECT state,host_id,workspace_binding_version,target_revision,manifest_sha256,
runtime_generation,applied_revision,sync_state,impacts
FROM workspace_sync_project_upload_applications WHERE workspace_id=$1 AND session_id=$2`, row.WorkspaceID, row.ID).
		Scan(&state, &hostID, &bindingVersion, &targetRevision, &manifestSHA256, &generation, &appliedRevision, &syncState, &encodedImpacts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load Host project upload application receipt: %w", err)
	}
	if len(manifestSHA256) != 32 {
		return domain.NewError(domain.CodeInternal, "stored Host project upload manifest digest is invalid")
	}
	var impacts []workspace.SyncHostApplicationImpact
	if err := json.Unmarshal(encodedImpacts, &impacts); err != nil {
		return fmt.Errorf("decode Host project upload impacts: %w", err)
	}
	if impacts == nil {
		impacts = []workspace.SyncHostApplicationImpact{}
	}
	application := &workspace.SyncHostApplication{
		Status: state, HostID: hostID, WorkspaceBindingVersion: bindingVersion,
		TargetRevision: targetRevision, ManifestSHA256: hex.EncodeToString(manifestSHA256), Impacts: impacts,
	}
	if generation.Valid {
		value := generation.Int64
		application.RuntimeGeneration = &value
	}
	if appliedRevision.Valid {
		value := appliedRevision.Int64
		application.AppliedRevision = &value
	}
	if syncState.Valid {
		value := workspace.BindingSyncState(syncState.String)
		application.SyncState = &value
	}
	value.HostApplication = application
	return nil
}

func workspaceFileFromRow(path string, size int64, digest string, mode int32, kind string, symlinkTarget pgtype.Text,
	modTimeNS pgtype.Int8, hardlinkTo pgtype.Text, encodedChunks, encodedXattrs, encodedACL []byte, metadataKnown bool) (workspace.File, error) {
	var chunks []workspace.Chunk
	if err := json.Unmarshal(encodedChunks, &chunks); err != nil {
		return workspace.File{}, fmt.Errorf("decode workspace file chunks: %w", err)
	}
	if chunks == nil {
		chunks = []workspace.Chunk{}
	}
	xattrs, acl, err := workspaceNativeMetadataFromRow(encodedXattrs, encodedACL)
	if err != nil {
		return workspace.File{}, fmt.Errorf("decode workspace file native metadata: %w", err)
	}
	file := workspace.File{Path: path, SizeBytes: size, SHA256: digest, Mode: uint32(mode), Kind: kind, Chunks: chunks, Xattrs: xattrs, ACL: acl, MetadataKnown: metadataKnown}
	if symlinkTarget.Valid {
		file.SymlinkTarget = symlinkTarget.String
	}
	if modTimeNS.Valid {
		value := modTimeNS.Int64
		file.ModTimeNS = &value
	}
	if hardlinkTo.Valid {
		file.HardlinkTo = hardlinkTo.String
	}
	if err := workspace.ValidateFile(file); err != nil {
		return workspace.File{}, domain.NewError(domain.CodeInternal, "stored workspace manifest file is invalid")
	}
	return file, nil
}

func workspaceNativeMetadataFromRow(encodedXattrs, encodedACL []byte) ([]workspace.ExtendedAttribute, *workspace.NativeACL, error) {
	xattrs := make([]workspace.ExtendedAttribute, 0)
	if len(encodedXattrs) > 0 {
		if err := json.Unmarshal(encodedXattrs, &xattrs); err != nil {
			return nil, nil, fmt.Errorf("decode workspace extended attributes: %w", err)
		}
	}
	if xattrs == nil {
		xattrs = []workspace.ExtendedAttribute{}
	}
	var acl *workspace.NativeACL
	if len(encodedACL) > 0 && string(encodedACL) != "null" {
		acl = &workspace.NativeACL{}
		if err := json.Unmarshal(encodedACL, acl); err != nil {
			return nil, nil, fmt.Errorf("decode workspace native ACL: %w", err)
		}
	}
	return xattrs, acl, nil
}

func workspaceNativeMetadataJSON(xattrs []workspace.ExtendedAttribute, acl *workspace.NativeACL) ([]byte, []byte, error) {
	if xattrs == nil {
		xattrs = []workspace.ExtendedAttribute{}
	}
	encodedXattrs, err := json.Marshal(xattrs)
	if err != nil {
		return nil, nil, fmt.Errorf("encode workspace extended attributes: %w", err)
	}
	if acl == nil {
		return encodedXattrs, nil, nil
	}
	encodedACL, err := json.Marshal(acl)
	if err != nil {
		return nil, nil, fmt.Errorf("encode workspace native ACL: %w", err)
	}
	return encodedXattrs, encodedACL, nil
}

func workspaceSymlinkTargetText(file workspace.File) pgtype.Text {
	if workspace.CanonicalFileKind(file.Kind) != workspace.FileKindSymlink {
		return pgtype.Text{}
	}
	return pgtype.Text{String: file.SymlinkTarget, Valid: true}
}

func workspaceHardlinkTargetText(file workspace.File) pgtype.Text {
	if file.HardlinkTo == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: file.HardlinkTo, Valid: true}
}

func decodeWorkspaceFiles(encoded []byte) ([]workspace.File, error) {
	var files []workspace.File
	if err := json.Unmarshal(encoded, &files); err != nil {
		return nil, fmt.Errorf("decode workspace sync upload plan: %w", err)
	}
	if files == nil {
		files = []workspace.File{}
	}
	return files, nil
}

func decodeWorkspaceConflicts(encoded []byte) ([]workspace.Conflict, error) {
	var conflicts []workspace.Conflict
	if err := json.Unmarshal(encoded, &conflicts); err != nil {
		return nil, fmt.Errorf("decode workspace sync conflicts: %w", err)
	}
	if conflicts == nil {
		conflicts = []workspace.Conflict{}
	}
	return conflicts, nil
}

func syncMutationPaths(files []workspace.File, deletedPaths []string) []string {
	paths := make([]string, 0, len(files)+len(deletedPaths))
	seen := make(map[string]struct{}, cap(paths))
	for _, file := range files {
		key := strings.ToLower(file.Path)
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			paths = append(paths, file.Path)
		}
	}
	for _, path := range deletedPaths {
		key := strings.ToLower(path)
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			paths = append(paths, path)
		}
	}
	return paths
}

func syncDirectoryMutationPaths(directories, deletedDirectories []string) []string {
	paths := make([]string, 0, len(directories)+len(deletedDirectories))
	paths = append(paths, directories...)
	paths = append(paths, deletedDirectories...)
	return uniqueWorkspacePaths(paths)
}

func loadWorkspaceRevisionFiles(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64, paths []string) ([]workspace.File, error) {
	if len(paths) == 0 {
		return []workspace.File{}, nil
	}
	rows, err := tx.Query(ctx, `
SELECT file_path, size_bytes, sha256, mode, kind, symlink_target, mod_time_ns, hardlink_to, chunks, xattrs, acl, metadata_known
FROM workspace_revision_files
WHERE workspace_id=$1 AND valid_from_revision <= $2
  AND (valid_to_revision IS NULL OR valid_to_revision > $2)
  AND folded_path = ANY($3::text[])`, workspaceID, revision, lowerWorkspacePaths(paths))
	if err != nil {
		return nil, fmt.Errorf("load workspace revision files: %w", err)
	}
	defer rows.Close()
	files := make([]workspace.File, 0, len(paths))
	for rows.Next() {
		var path, digest, kind string
		var size int64
		var mode int32
		var symlinkTarget pgtype.Text
		var modTimeNS pgtype.Int8
		var hardlinkTo pgtype.Text
		var chunks, xattrs, acl []byte
		var metadataKnown bool
		if err := rows.Scan(&path, &size, &digest, &mode, &kind, &symlinkTarget, &modTimeNS, &hardlinkTo, &chunks, &xattrs, &acl, &metadataKnown); err != nil {
			return nil, fmt.Errorf("scan workspace revision file: %w", err)
		}
		file, err := workspaceFileFromRow(path, size, digest, mode, kind, symlinkTarget, modTimeNS, hardlinkTo, chunks, xattrs, acl, metadataKnown)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workspace revision files: %w", err)
	}
	return files, nil
}

func loadWorkspaceRevisionEmptyDirectories(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return []string{}, nil
	}
	rows, err := tx.Query(ctx, `
SELECT directory_path
FROM workspace_revision_empty_directories
WHERE workspace_id=$1 AND valid_from_revision <= $2
  AND (valid_to_revision IS NULL OR valid_to_revision > $2)
  AND folded_path = ANY($3::text[])`, workspaceID, revision, lowerWorkspacePaths(paths))
	if err != nil {
		return nil, fmt.Errorf("load workspace revision empty directories: %w", err)
	}
	defer rows.Close()
	directories := make([]string, 0, len(paths))
	for rows.Next() {
		var directory string
		if err := rows.Scan(&directory); err != nil {
			return nil, fmt.Errorf("scan workspace revision empty directory: %w", err)
		}
		directories = append(directories, directory)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workspace revision empty directories: %w", err)
	}
	return directories, nil
}

func loadWorkspaceRevisionDirectories(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64, paths []string) ([]workspace.Directory, error) {
	if len(paths) == 0 {
		return []workspace.Directory{}, nil
	}
	rows, err := tx.Query(ctx, `
SELECT directory_path, mode, mod_time_ns, xattrs, acl, metadata_known
FROM workspace_revision_directories
WHERE workspace_id=$1 AND valid_from_revision <= $2
  AND (valid_to_revision IS NULL OR valid_to_revision > $2)
  AND folded_path = ANY($3::text[])
ORDER BY directory_path`, workspaceID, revision, lowerWorkspacePaths(paths))
	if err != nil {
		return nil, fmt.Errorf("load workspace revision directories: %w", err)
	}
	defer rows.Close()
	directoriesByPath := make(map[string]workspace.Directory, len(paths))
	for rows.Next() {
		var value workspace.Directory
		var mode pgtype.Int4
		var modified pgtype.Int8
		var xattrs, acl []byte
		if err := rows.Scan(&value.Path, &mode, &modified, &xattrs, &acl, &value.MetadataKnown); err != nil {
			return nil, fmt.Errorf("scan workspace revision directory: %w", err)
		}
		if value.Path != "." && workspace.ValidateRelativePath(value.Path) != nil || mode.Valid && (mode.Int32 < 0 || mode.Int32 > 0o777) {
			return nil, domain.NewError(domain.CodeInternal, "stored workspace directory metadata is invalid")
		}
		value.Xattrs, value.ACL, err = workspaceNativeMetadataFromRow(xattrs, acl)
		if err != nil {
			return nil, domain.NewError(domain.CodeInternal, "stored workspace directory metadata is invalid")
		}
		if mode.Valid {
			permissions := uint32(mode.Int32)
			value.Mode = &permissions
		}
		if modified.Valid {
			value.ModTimeNS = &modified.Int64
		}
		if err := workspace.ValidateDirectory(value, false); err != nil {
			return nil, domain.NewError(domain.CodeInternal, "stored workspace directory metadata is invalid")
		}
		directoriesByPath[strings.ToLower(value.Path)] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workspace revision directories: %w", err)
	}
	orderedPaths := append([]string(nil), paths...)
	sort.Strings(orderedPaths)
	directories := make([]workspace.Directory, 0, len(orderedPaths))
	for _, path := range orderedPaths {
		if path != "." && workspace.ValidateRelativePath(path) != nil {
			return nil, domain.NewError(domain.CodeInternal, "requested workspace directory metadata path is invalid")
		}
		if value, exists := directoriesByPath[strings.ToLower(path)]; exists {
			if value.Path != path {
				return nil, domain.NewError(domain.CodeInternal, "stored workspace directory path differs from the revision tree")
			}
			directories = append(directories, value)
			continue
		}
		directories = append(directories, workspace.Directory{Path: path})
	}
	return directories, nil
}

func lowerWorkspacePaths(paths []string) []string {
	lowered := make([]string, len(paths))
	for i, path := range paths {
		lowered[i] = strings.ToLower(path)
	}
	return lowered
}

func fileMapByFoldedPath(files []workspace.File) map[string]workspace.File {
	result := make(map[string]workspace.File, len(files))
	for _, file := range files {
		result[strings.ToLower(file.Path)] = file
	}
	return result
}

func workspaceDirectoryMapByFoldedPath(directories []workspace.Directory) map[string]workspace.Directory {
	result := make(map[string]workspace.Directory, len(directories))
	for _, directory := range directories {
		result[strings.ToLower(directory.Path)] = directory
	}
	return result
}

func directoryMapByFoldedPath(directories []string) map[string]string {
	result := make(map[string]string, len(directories))
	for _, directory := range directories {
		result[strings.ToLower(directory)] = directory
	}
	return result
}

func sameWorkspaceFile(left, right workspace.File) bool {
	if left.Path != right.Path || left.SizeBytes != right.SizeBytes || left.SHA256 != right.SHA256 || left.Mode != right.Mode ||
		left.HardlinkTo != right.HardlinkTo || workspace.CanonicalFileKind(left.Kind) != workspace.CanonicalFileKind(right.Kind) ||
		left.SymlinkTarget != right.SymlinkTarget || left.MetadataKnown != right.MetadataKnown ||
		!sameWorkspaceNativeMetadata(left.Xattrs, right.Xattrs, left.ACL, right.ACL) {
		return false
	}
	return left.ModTimeNS == nil && right.ModTimeNS == nil || left.ModTimeNS != nil && right.ModTimeNS != nil && *left.ModTimeNS == *right.ModTimeNS
}

func sameWorkspaceNativeMetadata(leftXattrs, rightXattrs []workspace.ExtendedAttribute, leftACL, rightACL *workspace.NativeACL) bool {
	if len(leftXattrs) != len(rightXattrs) || (leftACL == nil) != (rightACL == nil) {
		return false
	}
	for index := range leftXattrs {
		if leftXattrs[index].Name != rightXattrs[index].Name || !bytes.Equal(leftXattrs[index].Value, rightXattrs[index].Value) {
			return false
		}
	}
	return leftACL == nil || leftACL.Platform == rightACL.Platform && leftACL.Format == rightACL.Format && bytes.Equal(leftACL.Data, rightACL.Data)
}

func workspaceFileModTimeNS(value *int64) pgtype.Int8 {
	if value == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *value, Valid: true}
}

func stringPointer(value string) *string { return &value }

func fileHashPointer(value workspace.File, ok bool) *string {
	if !ok {
		return nil
	}
	return &value.SHA256
}

type syncMutation struct {
	path string
	file *workspace.File
}

type syncDirectoryMutation struct {
	path   string
	delete bool
}

type syncDirectoryMetadataMutation struct {
	path      string
	directory *workspace.Directory
	delete    bool
}

func applyWorkspaceSyncTreeMutations(ctx context.Context, tx pgx.Tx, workspaceID string, revision, currentFileCount, currentBytes, quotaBytes int64,
	fileMutations []syncMutation, directoryMutations []syncDirectoryMutation, assertedDirectoryMetadata []workspace.Directory,
	metadataMutationGroups ...[]syncDirectoryMetadataMutation) error {
	var metadataMutations []syncDirectoryMetadataMutation
	for _, group := range metadataMutationGroups {
		metadataMutations = append(metadataMutations, group...)
	}
	if len(fileMutations)+len(directoryMutations)+len(metadataMutations) == 0 {
		return nil
	}
	filePlans := make(map[string]syncMutation, len(fileMutations))
	directoryPlans := make(map[string]syncDirectoryMutation, len(directoryMutations))
	metadataPlans := make(map[string]syncDirectoryMetadataMutation, len(metadataMutations))
	assertedDirectoryTimes := make(map[string]struct{}, len(assertedDirectoryMetadata))
	for _, directory := range assertedDirectoryMetadata {
		if directory.ModTimeNS != nil && (directory.Path == "." || workspace.ValidateRelativePath(directory.Path) == nil) {
			assertedDirectoryTimes[strings.ToLower(directory.Path)] = struct{}{}
		}
	}
	allPaths := make([]string, 0, len(fileMutations)+len(directoryMutations)+len(metadataMutations))
	for _, mutation := range fileMutations {
		if err := workspace.ValidateRelativePath(mutation.path); err != nil {
			return domain.InvalidArgument("sync_session", "workspace file mutation path is invalid")
		}
		if mutation.file != nil {
			if err := workspace.ValidateFile(*mutation.file); err != nil || !strings.EqualFold(mutation.file.Path, mutation.path) {
				return domain.InvalidArgument("sync_session", "workspace file mutation is invalid")
			}
		}
		key := strings.ToLower(mutation.path)
		filePlans[key] = mutation
		allPaths = append(allPaths, mutation.path)
	}
	for _, mutation := range directoryMutations {
		if err := workspace.ValidateRelativePath(mutation.path); err != nil {
			return domain.InvalidArgument("sync_session", "workspace directory mutation path is invalid")
		}
		key := strings.ToLower(mutation.path)
		directoryPlans[key] = mutation
		allPaths = append(allPaths, mutation.path)
	}
	for _, mutation := range metadataMutations {
		if mutation.path != "." && workspace.ValidateRelativePath(mutation.path) != nil {
			return domain.InvalidArgument("sync_session", "workspace directory metadata path is invalid")
		}
		if mutation.directory != nil {
			if err := workspace.ValidateDirectory(*mutation.directory, false); err != nil || mutation.directory.Path != mutation.path {
				return domain.InvalidArgument("sync_session", "workspace directory metadata mutation is invalid")
			}
		}
		key := strings.ToLower(mutation.path)
		metadataPlans[key] = mutation
		allPaths = append(allPaths, mutation.path)
	}
	allPaths = uniqueWorkspacePaths(allPaths)
	currentFiles, err := loadWorkspaceRevisionFiles(ctx, tx, workspaceID, revision, allPaths)
	if err != nil {
		return err
	}
	currentDirectories, err := loadWorkspaceRevisionEmptyDirectories(ctx, tx, workspaceID, revision, allPaths)
	if err != nil {
		return err
	}
	filesByPath := fileMapByFoldedPath(currentFiles)
	directoriesByPath := directoryMapByFoldedPath(currentDirectories)
	for key := range filesByPath {
		if _, exists := directoriesByPath[key]; exists {
			return domain.NewError(domain.CodeInternal, "workspace snapshot contains a file and directory path collision")
		}
	}
	var directoryCount int64
	if err := tx.QueryRow(ctx, `SELECT directory_count FROM workspace_revisions WHERE workspace_id=$1 AND revision=$2`, workspaceID, revision).Scan(&directoryCount); err != nil {
		return fmt.Errorf("read current workspace directory count: %w", err)
	}
	type finalTreeState struct {
		kind string
		path string
		file *workspace.File
	}
	finalStates := make(map[string]finalTreeState, len(allPaths))
	var deltaBytes, deltaFiles, deltaDirectories int64
	for _, pathValue := range allPaths {
		key := strings.ToLower(pathValue)
		oldFile, hadFile := filesByPath[key]
		oldDirectory, hadDirectory := directoriesByPath[key]
		filePlan, hasFilePlan := filePlans[key]
		directoryPlan, hasDirectoryPlan := directoryPlans[key]
		if !hasFilePlan && !hasDirectoryPlan {
			continue
		}
		final := finalTreeState{}
		if hasFilePlan && filePlan.file != nil {
			copied := *filePlan.file
			final = finalTreeState{kind: "file", path: copied.Path, file: &copied}
		} else if hasDirectoryPlan && !directoryPlan.delete {
			final = finalTreeState{kind: "directory", path: directoryPlan.path}
		}
		if final.kind == "file" && hadFile && sameWorkspaceFile(oldFile, *final.file) || final.kind == "directory" && hadDirectory && oldDirectory == final.path || final.kind == "" && !hadFile && !hadDirectory {
			continue
		}
		if hadFile {
			deltaBytes -= oldFile.SizeBytes
			deltaFiles--
		}
		if final.kind == "file" {
			deltaBytes += final.file.SizeBytes
			deltaFiles++
		}
		if hadDirectory {
			deltaDirectories--
		}
		if final.kind == "directory" {
			deltaDirectories++
		}
		finalStates[key] = final
	}
	if len(finalStates) == 0 && len(metadataPlans) == 0 {
		return nil
	}
	if currentBytes < 0 || deltaBytes > 0 && currentBytes > int64(^uint64(0)>>1)-deltaBytes || deltaBytes < 0 && deltaBytes < -currentBytes || currentFileCount+deltaFiles < 0 {
		return domain.NewError(domain.CodeQuotaExceeded, "workspace storage quota would be exceeded")
	}
	nextBytes := currentBytes + deltaBytes
	nextFileCount := currentFileCount + deltaFiles
	nextDirectoryCount := directoryCount + deltaDirectories
	if nextFileCount < 0 || nextDirectoryCount < 0 || nextFileCount+nextDirectoryCount > workspace.MaxSyncFiles {
		return domain.NewError(domain.CodeRequestTooLarge, "workspace manifest exceeds its item limit")
	}
	if nextBytes > quotaBytes && (currentBytes <= quotaBytes || nextBytes >= currentBytes) {
		return domain.NewError(domain.CodeQuotaExceeded, "workspace storage quota would be exceeded")
	}
	if revision == int64(^uint64(0)>>1) {
		return domain.NewError(domain.CodeWorkspaceConflict, "workspace revision is exhausted")
	}
	nextRevision := revision + 1
	q := sqlcgen.New(tx)
	if err := q.CreateWorkspaceTreeRevision(ctx, sqlcgen.CreateWorkspaceTreeRevisionParams{
		WorkspaceID: workspaceID, Revision: nextRevision, FileCount: nextFileCount, TotalBytes: nextBytes, DirectoryCount: nextDirectoryCount,
	}); err != nil {
		return fmt.Errorf("create workspace tree revision: %w", err)
	}
	type changeEvent struct {
		path      string
		action    workspace.ChangeAction
		file      *workspace.File
		directory *workspace.Directory
	}
	events := make([]changeEvent, 0, len(finalStates)+1)
	for key, final := range finalStates {
		oldFile, hadFile := filesByPath[key]
		oldDirectory, hadDirectory := directoriesByPath[key]
		if hadFile {
			affected, err := q.CloseWorkspaceRevisionFile(ctx, sqlcgen.CloseWorkspaceRevisionFileParams{WorkspaceID: workspaceID, FilePath: oldFile.Path, ValidToRevision: pgtype.Int8{Int64: nextRevision, Valid: true}})
			if err != nil {
				return fmt.Errorf("close prior workspace file revision: %w", err)
			}
			if affected != 1 {
				return domain.NewError(domain.CodeWorkspaceConflict, "workspace file changed during tree apply")
			}
		}
		if hadDirectory {
			affected, err := q.CloseWorkspaceRevisionEmptyDirectory(ctx, sqlcgen.CloseWorkspaceRevisionEmptyDirectoryParams{WorkspaceID: workspaceID, DirectoryPath: oldDirectory, ValidToRevision: pgtype.Int8{Int64: nextRevision, Valid: true}})
			if err != nil {
				return fmt.Errorf("close prior workspace empty directory revision: %w", err)
			}
			if affected != 1 {
				return domain.NewError(domain.CodeWorkspaceConflict, "workspace directory changed during tree apply")
			}
		}
		switch final.kind {
		case "file":
			chunksJSON, err := json.Marshal(nonNilChunks(final.file.Chunks))
			if err != nil {
				return fmt.Errorf("encode workspace file chunks: %w", err)
			}
			xattrsJSON, aclJSON, err := workspaceNativeMetadataJSON(final.file.Xattrs, final.file.ACL)
			if err != nil {
				return err
			}
			if err := q.InsertWorkspaceRevisionFile(ctx, sqlcgen.InsertWorkspaceRevisionFileParams{
				WorkspaceID: workspaceID, ValidFromRevision: nextRevision, FilePath: final.file.Path, FoldedPath: strings.ToLower(final.file.Path),
				SizeBytes: final.file.SizeBytes, Sha256: final.file.SHA256, Mode: int32(final.file.Mode),
				Kind: workspace.CanonicalFileKind(final.file.Kind), SymlinkTarget: workspaceSymlinkTargetText(*final.file),
				ModTimeNs: workspaceFileModTimeNS(final.file.ModTimeNS), HardlinkTo: workspaceHardlinkTargetText(*final.file),
				Chunks: chunksJSON, Xattrs: xattrsJSON, Acl: aclJSON, MetadataKnown: final.file.MetadataKnown,
			}); err != nil {
				return fmt.Errorf("insert workspace file revision: %w", err)
			}
		case "directory":
			if err := q.InsertWorkspaceRevisionEmptyDirectory(ctx, sqlcgen.InsertWorkspaceRevisionEmptyDirectoryParams{
				WorkspaceID: workspaceID, ValidFromRevision: nextRevision, DirectoryPath: final.path, FoldedPath: strings.ToLower(final.path),
			}); err != nil {
				return fmt.Errorf("insert workspace empty-directory revision: %w", err)
			}
		}
		if hadFile && final.path != "" && oldFile.Path != final.path {
			events = append(events, changeEvent{path: oldFile.Path, action: workspace.ChangeDelete})
		}
		if hadDirectory && final.path != "" && oldDirectory != final.path {
			events = append(events, changeEvent{path: oldDirectory, action: workspace.ChangeDirectoryDelete})
		}
		switch final.kind {
		case "file":
			copy := *final.file
			events = append(events, changeEvent{path: final.path, action: workspace.ChangeUpsert, file: &copy})
		case "directory":
			events = append(events, changeEvent{path: final.path, action: workspace.ChangeDirectoryUpsert})
		case "":
			if hadFile {
				events = append(events, changeEvent{path: oldFile.Path, action: workspace.ChangeDelete})
			}
			if hadDirectory {
				events = append(events, changeEvent{path: oldDirectory, action: workspace.ChangeDirectoryDelete})
			}
		}
	}
	if err := ensureWorkspaceTreeShape(ctx, tx, workspaceID, nextRevision); err != nil {
		return err
	}
	if err := ensureWorkspaceHardlinkShape(ctx, tx, workspaceID, nextRevision); err != nil {
		return err
	}
	for key, mutation := range metadataPlans {
		_, exists, err := currentDirectoryMetadataForPath(ctx, tx, workspaceID, revision, mutation.path)
		if err != nil {
			return err
		}
		if exists {
			closed, err := tx.Exec(ctx, `UPDATE workspace_revision_directories SET valid_to_revision=$3
WHERE workspace_id=$1 AND folded_path=$2 AND valid_to_revision IS NULL AND valid_from_revision < $3`, workspaceID, key, nextRevision)
			if err != nil {
				return fmt.Errorf("close prior workspace directory metadata: %w", err)
			}
			if closed.RowsAffected() != 1 {
				return domain.NewError(domain.CodeWorkspaceConflict, "workspace directory metadata changed during tree apply")
			}
		}
		if mutation.directory != nil {
			exists, err := workspaceDirectoryExistsAtRevision(ctx, tx, workspaceID, nextRevision, mutation.path)
			if err != nil {
				return err
			}
			if !exists {
				return domain.NewError(domain.CodeWorkspaceConflict, "workspace directory metadata path is absent from the resulting tree")
			}
			xattrsJSON, aclJSON, err := workspaceNativeMetadataJSON(mutation.directory.Xattrs, mutation.directory.ACL)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_revision_directories
(workspace_id,valid_from_revision,directory_path,folded_path,mode,mod_time_ns,xattrs,acl,metadata_known) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				workspaceID, nextRevision, mutation.directory.Path, key, workspaceDirectoryMode(mutation.directory.Mode),
				workspaceDirectoryModTime(mutation.directory.ModTimeNS), xattrsJSON, aclJSON, mutation.directory.MetadataKnown); err != nil {
				return fmt.Errorf("insert workspace directory metadata revision: %w", err)
			}
			events = append(events, changeEvent{path: mutation.path, action: workspace.ChangeDirectoryUpsert})
		} else if mutation.delete {
			events = append(events, changeEvent{path: mutation.path, action: workspace.ChangeDirectoryDelete})
		}
	}
	structuralPaths := make([]string, 0, len(fileMutations)+len(directoryMutations))
	for _, mutation := range fileMutations {
		structuralPaths = append(structuralPaths, mutation.path)
	}
	for _, mutation := range directoryMutations {
		structuralPaths = append(structuralPaths, mutation.path)
	}
	invalidatedEvents, err := invalidateWorkspaceDirectoryTimes(ctx, tx, workspaceID, nextRevision, revision, structuralPaths, allPaths, metadataPlans, assertedDirectoryTimes)
	if err != nil {
		return err
	}
	for _, eventPath := range invalidatedEvents {
		events = append(events, changeEvent{path: eventPath, action: workspace.ChangeDirectoryUpsert})
	}
	seenEvents := make(map[string]struct{}, len(events))
	for _, event := range events {
		if _, exists := seenEvents[event.path]; exists {
			continue
		}
		seenEvents[event.path] = struct{}{}
		var fileJSON, directoryJSON []byte
		if event.file != nil {
			fileJSON, err = json.Marshal(event.file)
			if err != nil {
				return fmt.Errorf("encode workspace revision change: %w", err)
			}
		} else if event.action == workspace.ChangeDirectoryUpsert {
			directory := event.directory
			if directory == nil {
				if value, exists, err := currentDirectoryMetadataForPath(ctx, tx, workspaceID, nextRevision, event.path); err != nil {
					return err
				} else if exists {
					directory = &value
				}
			}
			if directory != nil {
				directoryJSON, err = json.Marshal(directory)
				if err != nil {
					return fmt.Errorf("encode workspace directory revision change: %w", err)
				}
			}
		}
		if err := q.InsertWorkspaceChange(ctx, sqlcgen.InsertWorkspaceChangeParams{
			WorkspaceID: workspaceID, Revision: nextRevision, FilePath: event.path, Action: string(event.action), File: fileJSON, Directory: directoryJSON,
		}); err != nil {
			return fmt.Errorf("record workspace tree change: %w", err)
		}
	}
	command, err := tx.Exec(ctx, `
UPDATE workspaces SET revision=$2, used_bytes=$3, updated_at=now()
WHERE id=$1 AND state='ready' AND revision=$4
  AND (quota_bytes >= $3 OR (used_bytes > quota_bytes AND $3 < used_bytes))`, workspaceID, nextRevision, nextBytes, revision)
	if err != nil {
		return fmt.Errorf("update workspace revision after sync: %w", err)
	}
	if command.RowsAffected() != 1 {
		return domain.NewError(domain.CodeWorkspaceConflict, "workspace changed during sync apply")
	}
	changedPaths := make([]string, 0, len(finalStates))
	for foldedPath := range finalStates {
		changedPaths = append(changedPaths, foldedPath)
	}
	if _, err := tx.Exec(ctx, `UPDATE workspace_sync_receive_quota_rejections
SET state='superseded', updated_at=clock_timestamp()
WHERE workspace_id=$1 AND state='paused' AND folded_path=ANY($2::text[])
  AND folder_id IN (SELECT folder_id FROM workspace_sync_folders WHERE workspace_id=$1 AND folder_kind='workspace_root')`, workspaceID, changedPaths); err != nil {
		return fmt.Errorf("supersede Workspace receive quota pauses for committed paths: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE workspace_bindings SET desired_revision=$2, updated_at=now()
WHERE workspace_id=$1 AND state IN ('preparing','active','unbinding') AND desired_revision < $2`, workspaceID, nextRevision); err != nil {
		return fmt.Errorf("advance bound host workspace revisions: %w", err)
	}
	return nil
}

func workspaceDirectoryMode(value *uint32) pgtype.Int4 {
	if value == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*value), Valid: true}
}

func workspaceDirectoryModTime(value *int64) pgtype.Int8 {
	if value == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *value, Valid: true}
}

func currentDirectoryMetadataForPath(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64, path string) (workspace.Directory, bool, error) {
	var value workspace.Directory
	var mode pgtype.Int4
	var modified pgtype.Int8
	var xattrs, acl []byte
	err := tx.QueryRow(ctx, `SELECT directory_path,mode,mod_time_ns,xattrs,acl,metadata_known FROM workspace_revision_directories
WHERE workspace_id=$1 AND folded_path=$2 AND valid_from_revision <= $3 AND (valid_to_revision IS NULL OR valid_to_revision > $3)`,
		workspaceID, strings.ToLower(path), revision).Scan(&value.Path, &mode, &modified, &xattrs, &acl, &value.MetadataKnown)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspace.Directory{}, false, nil
	}
	if err != nil {
		return workspace.Directory{}, false, fmt.Errorf("read current workspace directory metadata: %w", err)
	}
	if mode.Valid {
		permissions := uint32(mode.Int32)
		value.Mode = &permissions
	}
	if modified.Valid {
		value.ModTimeNS = &modified.Int64
	}
	value.Xattrs, value.ACL, err = workspaceNativeMetadataFromRow(xattrs, acl)
	if err != nil || workspace.ValidateDirectory(value, false) != nil {
		return workspace.Directory{}, false, domain.NewError(domain.CodeInternal, "stored workspace directory metadata is invalid")
	}
	return value, true, nil
}

func workspaceDirectoryExistsAtRevision(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64, path string) (bool, error) {
	if path == "." {
		return true, nil
	}
	var exists bool
	err := tx.QueryRow(ctx, `SELECT
EXISTS (SELECT 1 FROM workspace_revision_files f WHERE f.workspace_id=$1 AND f.valid_from_revision <= $2
  AND (f.valid_to_revision IS NULL OR f.valid_to_revision > $2) AND starts_with(f.folded_path, $3 || '/')) OR
EXISTS (SELECT 1 FROM workspace_revision_empty_directories d WHERE d.workspace_id=$1 AND d.valid_from_revision <= $2
  AND (d.valid_to_revision IS NULL OR d.valid_to_revision > $2) AND (d.folded_path=$3 OR starts_with(d.folded_path, $3 || '/')))`,
		workspaceID, revision, strings.ToLower(path)).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check workspace directory in resulting tree: %w", err)
	}
	return exists, nil
}

func invalidateWorkspaceDirectoryTimes(ctx context.Context, tx pgx.Tx, workspaceID string, nextRevision, currentRevision int64,
	structuralPaths, allPaths []string, metadataPlans map[string]syncDirectoryMetadataMutation, assertedDirectoryTimes map[string]struct{}) ([]string, error) {
	candidates := make(map[string]string)
	for _, path := range directoryAncestorPaths(structuralPaths) {
		candidates[strings.ToLower(path)] = path
	}
	for _, path := range allPaths {
		candidates[strings.ToLower(path)] = path
	}
	paths := make([]string, 0, len(candidates))
	for _, path := range candidates {
		paths = append(paths, path)
	}
	metadata, err := loadWorkspaceRevisionDirectories(ctx, tx, workspaceID, currentRevision, paths)
	if err != nil {
		return nil, err
	}
	byPath := workspaceDirectoryMapByFoldedPath(metadata)
	events := make([]string, 0)
	for key, old := range byPath {
		if _, explicitlyChanged := metadataPlans[key]; explicitlyChanged {
			continue
		}
		if _, asserted := assertedDirectoryTimes[key]; asserted {
			continue
		}
		exists, err := workspaceDirectoryExistsAtRevision(ctx, tx, workspaceID, nextRevision, old.Path)
		if err != nil {
			return nil, err
		}
		if !exists {
			if _, err := tx.Exec(ctx, `UPDATE workspace_revision_directories SET valid_to_revision=$3
WHERE workspace_id=$1 AND folded_path=$2 AND valid_to_revision IS NULL AND valid_from_revision < $3`, workspaceID, key, nextRevision); err != nil {
				return nil, fmt.Errorf("close removed workspace directory metadata: %w", err)
			}
			continue
		}
		if old.ModTimeNS == nil {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_revision_directories SET valid_to_revision=$3
WHERE workspace_id=$1 AND folded_path=$2 AND valid_to_revision IS NULL AND valid_from_revision < $3`, workspaceID, key, nextRevision); err != nil {
			return nil, fmt.Errorf("close stale workspace directory timestamp: %w", err)
		}
		xattrsJSON, aclJSON, err := workspaceNativeMetadataJSON(old.Xattrs, old.ACL)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_revision_directories
(workspace_id,valid_from_revision,directory_path,folded_path,mode,mod_time_ns,xattrs,acl,metadata_known) VALUES ($1,$2,$3,$4,$5,NULL,$6,$7,$8)`,
			workspaceID, nextRevision, old.Path, key, workspaceDirectoryMode(old.Mode), xattrsJSON, aclJSON, old.MetadataKnown); err != nil {
			return nil, fmt.Errorf("record unknown workspace directory timestamp: %w", err)
		}
		events = append(events, old.Path)
	}
	return events, nil
}

func directoryAncestorPaths(paths []string) []string {
	seen := make(map[string]string)
	for _, value := range paths {
		seen["."] = "."
		parts := strings.Split(value, "/")
		prefix := ""
		for index, part := range parts[:len(parts)-1] {
			if index > 0 {
				prefix += "/"
			}
			prefix += part
			seen[strings.ToLower(prefix)] = prefix
		}
	}
	result := make([]string, 0, len(seen))
	for _, path := range seen {
		result = append(result, path)
	}
	return result
}

func uniqueWorkspacePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, value := range paths {
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}

func ensureWorkspaceTreeShape(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64) error {
	var invalid bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM workspace_revision_files f
  WHERE f.workspace_id=$1 AND f.valid_from_revision <= $2 AND (f.valid_to_revision IS NULL OR f.valid_to_revision > $2)
    AND (EXISTS (SELECT 1 FROM workspace_revision_files c WHERE c.workspace_id=$1 AND c.valid_from_revision <= $2 AND (c.valid_to_revision IS NULL OR c.valid_to_revision > $2) AND starts_with(c.folded_path, f.folded_path || '/'))
      OR EXISTS (SELECT 1 FROM workspace_revision_empty_directories d WHERE d.workspace_id=$1 AND d.valid_from_revision <= $2 AND (d.valid_to_revision IS NULL OR d.valid_to_revision > $2) AND starts_with(d.folded_path, f.folded_path || '/')))
  UNION ALL
  SELECT 1 FROM workspace_revision_empty_directories d
  WHERE d.workspace_id=$1 AND d.valid_from_revision <= $2 AND (d.valid_to_revision IS NULL OR d.valid_to_revision > $2)
    AND (EXISTS (SELECT 1 FROM workspace_revision_files f WHERE f.workspace_id=$1 AND f.valid_from_revision <= $2 AND (f.valid_to_revision IS NULL OR f.valid_to_revision > $2) AND f.folded_path=d.folded_path)
      OR EXISTS (SELECT 1 FROM workspace_revision_files f WHERE f.workspace_id=$1 AND f.valid_from_revision <= $2 AND (f.valid_to_revision IS NULL OR f.valid_to_revision > $2) AND starts_with(f.folded_path, d.folded_path || '/'))
      OR EXISTS (SELECT 1 FROM workspace_revision_empty_directories c WHERE c.workspace_id=$1 AND c.valid_from_revision <= $2 AND (c.valid_to_revision IS NULL OR c.valid_to_revision > $2) AND starts_with(c.folded_path, d.folded_path || '/')))
)`, workspaceID, revision).Scan(&invalid); err != nil {
		return fmt.Errorf("validate workspace tree shape: %w", err)
	}
	if invalid {
		return domain.InvalidArgument("manifest", "workspace tree contains a file-prefix conflict or non-empty directory")
	}
	return nil
}

func ensureWorkspaceHardlinkShape(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64) error {
	var invalid bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM workspace_revision_files f
  WHERE f.workspace_id=$1 AND f.valid_from_revision <= $2 AND (f.valid_to_revision IS NULL OR f.valid_to_revision > $2)
    AND f.hardlink_to IS NOT NULL AND (
      f.hardlink_to COLLATE "C" >= f.file_path COLLATE "C" OR
      NOT EXISTS (
        SELECT 1 FROM workspace_revision_files a
        WHERE a.workspace_id=f.workspace_id AND a.file_path=f.hardlink_to
          AND a.valid_from_revision <= $2 AND (a.valid_to_revision IS NULL OR a.valid_to_revision > $2)
          AND a.kind='file' AND a.symlink_target IS NULL AND a.hardlink_to IS NULL
          AND a.size_bytes=f.size_bytes AND a.sha256=f.sha256 AND a.mode=f.mode
          AND a.mod_time_ns IS NOT DISTINCT FROM f.mod_time_ns AND a.chunks=f.chunks
      )
    )
)`, workspaceID, revision).Scan(&invalid); err != nil {
		return fmt.Errorf("validate workspace hardlink relations: %w", err)
	}
	if invalid {
		return domain.InvalidArgument("manifest", "workspace tree contains an invalid hardlink relation")
	}
	return nil
}

func nonNilConflicts(value []workspace.Conflict) []workspace.Conflict {
	if value == nil {
		return []workspace.Conflict{}
	}
	return value
}

func nonNilChunks(value []workspace.Chunk) []workspace.Chunk {
	if value == nil {
		return []workspace.Chunk{}
	}
	return value
}

func nonNilTransferBlocks(value []workspace.TransferBlock) []workspace.TransferBlock {
	if value == nil {
		return []workspace.TransferBlock{}
	}
	for i := 0; i < len(value); i++ {
		for j := i + 1; j < len(value); j++ {
			if value[j].Role < value[i].Role || value[j].Role == value[i].Role && value[j].SHA256 < value[i].SHA256 {
				value[i], value[j] = value[j], value[i]
			}
		}
	}
	return value
}

func marshalNonNilJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode workspace sync data: %w", err)
	}
	if string(data) == "null" {
		data = []byte("[]")
	}
	return data, nil
}

func isTerminalSyncState(value workspace.SyncState) bool {
	return value == workspace.SyncCompleted || value == workspace.SyncConflict || value == workspace.SyncCancelled || value == workspace.SyncFailed || value == workspace.SyncExpired
}

var _ app.WorkspaceSyncStore = (*Store)(nil)
var _ app.WorkspaceBlockGCStore = (*Store)(nil)
