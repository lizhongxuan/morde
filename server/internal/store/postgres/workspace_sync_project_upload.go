package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"
)

// Lock the Host before callers lock the Workspace. Host binding transitions
// use Host -> Workspace -> binding order; taking this shared Host lock first
// prevents upload authorization from deadlocking while waiting on the binding.
func lockWorkspaceSyncProjectRegistrationHost(ctx context.Context, tx pgx.Tx, ownerID, workspaceID, folderID string) error {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSyncFolderID(folderID) {
		return domain.NewError(domain.CodeWorkspaceUnavailable, "project upload target is unavailable")
	}
	var hostID string
	err := tx.QueryRow(ctx, `SELECT host_id FROM workspace_sync_folder_registration_requests
WHERE owner_id=$1 AND workspace_id=$2 AND folder_id=$3
ORDER BY created_at DESC LIMIT 1`, ownerID, workspaceID, folderID).Scan(&hostID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeWorkspaceUnavailable, "project upload target is unavailable")
	}
	if err != nil {
		return fmt.Errorf("read Host for Workspace project upload: %w", err)
	}
	var hostOwner, hostState string
	var deletedAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `SELECT owner_id,state,deleted_at FROM hosts WHERE id=$1 FOR SHARE`, hostID).
		Scan(&hostOwner, &hostState, &deletedAt)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (hostOwner != ownerID || deletedAt.Valid || hostState == "deleted" || hostState == "deleting") {
		return domain.NewError(domain.CodeWorkspaceUnavailable, "project upload target is unavailable")
	}
	if err != nil {
		return fmt.Errorf("lock Host for Workspace project upload: %w", err)
	}
	return nil
}

func lockWorkspaceSyncProjectUploadFolder(ctx context.Context, tx pgx.Tx, ownerID, workspaceID, folderID string) (workspace.SyncFolder, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSyncFolderID(folderID) {
		return workspace.SyncFolder{}, domain.NewError(domain.CodeNotFound, "workspace sync folder not found")
	}
	folder, err := scanWorkspaceSyncFolder(tx.QueryRow(ctx, workspaceSyncFolderSelect+
		`WHERE f.workspace_id=$1 AND f.owner_id=$2 AND f.folder_id=$3 FOR UPDATE`, workspaceID, ownerID, folderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return workspace.SyncFolder{}, domain.NewError(domain.CodeNotFound, "workspace sync folder not found")
	}
	if err != nil {
		return workspace.SyncFolder{}, err
	}
	if folder.Kind != workspace.SyncFolderKindProject || folder.State != "pending_node" && folder.State != "active" ||
		workspace.ValidateSyncProjectFolderName(folder.ProjectName) != nil || folder.RelativePath != "project/"+folder.ProjectName ||
		workspace.ValidateRelativePath(folder.RelativePath) != nil {
		return workspace.SyncFolder{}, domain.NewError(domain.CodeWorkspaceUnavailable, "project upload target is unavailable")
	}
	if err := requireWorkspaceSyncProjectHostBinding(ctx, tx, ownerID, workspaceID, folderID); err != nil {
		return workspace.SyncFolder{}, err
	}
	return folder, nil
}

func requireWorkspaceSyncProjectHostBinding(ctx context.Context, tx pgx.Tx, ownerID, workspaceID, folderID string) error {
	var hostState, bindingState string
	var deletedAt pgtype.Timestamptz
	err := tx.QueryRow(ctx, `SELECT h.state,b.state,h.deleted_at
FROM workspace_sync_folder_registration_requests r
JOIN hosts h ON h.id=r.host_id AND h.owner_id=r.owner_id
JOIN workspace_bindings b ON b.host_id=h.id AND b.owner_id=h.owner_id AND b.workspace_id=r.workspace_id
WHERE r.owner_id=$1 AND r.workspace_id=$2 AND r.folder_id=$3 AND b.state='active' AND h.deleted_at IS NULL
ORDER BY r.created_at DESC
LIMIT 1
FOR SHARE OF h,b`, ownerID, workspaceID, folderID).Scan(&hostState, &bindingState, &deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeWorkspaceUnavailable, "project folder is no longer attached to an active cloud computer")
	}
	if err != nil {
		return fmt.Errorf("verify Workspace project folder Host binding: %w", err)
	}
	if deletedAt.Valid || bindingState != "active" || (hostState == "deleted" || hostState == "deleting") {
		return domain.NewError(domain.CodeWorkspaceUnavailable, "project folder is no longer attached to an active cloud computer")
	}
	return nil
}

func validateWorkspaceSyncProjectUploadBaseline(session workspace.SyncSession, folder workspace.SyncFolder) error {
	if session.FolderID != folder.FolderID || len(session.Scope) != 1 || session.Scope[0] != folder.RelativePath ||
		session.Direction != workspace.SyncUpload || session.Policy != workspace.SyncCompareMerge {
		return domain.NewError(domain.CodeWorkspaceUnavailable, "project upload session is outside its registered folder")
	}
	if session.FolderBaselineRevision == nil {
		if folder.LastUploadRevision != nil || session.BaseRevision != session.PinnedRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "project upload baseline changed")
		}
		return nil
	}
	if folder.LastUploadRevision == nil || *folder.LastUploadRevision != *session.FolderBaselineRevision ||
		session.BaseRevision != *session.FolderBaselineRevision {
		return domain.NewError(domain.CodeWorkspaceConflict, "project upload baseline changed")
	}
	return nil
}

func workspaceSyncSessionBaselineMatches(row sqlcgen.WorkspaceSyncSession, folder workspace.SyncFolder) bool {
	if row.FolderBaselineRevision.Valid != (folder.LastUploadRevision != nil) {
		return false
	}
	if row.FolderBaselineRevision.Valid {
		return row.FolderBaselineRevision.Int64 == *folder.LastUploadRevision && row.BaseRevision == row.FolderBaselineRevision.Int64
	}
	return row.BaseRevision == row.PinnedRevision
}

func advanceWorkspaceSyncProjectUploadBaseline(ctx context.Context, tx pgx.Tx, ownerID, workspaceID, folderID string,
	expectedBaseline pgtype.Int8, targetRevision int64) error {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSyncFolderID(folderID) ||
		targetRevision < 0 || expectedBaseline.Valid && expectedBaseline.Int64 < 0 {
		return domain.InvalidArgument("sync_session", "project upload baseline fields are invalid")
	}
	updated, err := tx.Exec(ctx, `UPDATE workspace_sync_folders
SET last_upload_revision=$5, updated_at=now()
WHERE workspace_id=$1 AND owner_id=$2 AND folder_id=$3 AND folder_kind='project'
  AND state IN ('pending_node','active')
  AND last_upload_revision IS NOT DISTINCT FROM $4`, workspaceID, ownerID, folderID, expectedBaseline, targetRevision)
	if err != nil {
		return fmt.Errorf("advance Workspace project upload baseline: %w", err)
	}
	if updated.RowsAffected() != 1 {
		return domain.NewError(domain.CodeWorkspaceConflict, "project upload baseline changed before commit")
	}
	return nil
}

func finishWorkspaceSyncProjectBaselineConflict(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, row sqlcgen.WorkspaceSyncSession,
	folder workspace.SyncFolder, currentRevision int64, idempotency app.IdempotencyRecord, result *workspace.SyncSession) error {
	conflicts, err := decodeWorkspaceConflicts(row.Conflicts)
	if err != nil {
		return err
	}
	found := false
	for _, conflict := range conflicts {
		if strings.EqualFold(conflict.Path, folder.RelativePath) {
			found = true
			break
		}
	}
	if !found {
		conflicts = append(conflicts, workspace.Conflict{Path: folder.RelativePath, Reason: "content_changed"})
	}
	conflictJSON, err := json.Marshal(nonNilConflicts(conflicts))
	if err != nil {
		return fmt.Errorf("encode project baseline conflict: %w", err)
	}
	if err := q.SetWorkspaceSyncSessionState(ctx, sqlcgen.SetWorkspaceSyncSessionStateParams{
		WorkspaceID: row.WorkspaceID, ID: row.ID, State: string(workspace.SyncConflict),
		Stage: string(workspace.SyncStageComplete), Conflicts: conflictJSON, TargetRevision: currentRevision,
	}); err != nil {
		return fmt.Errorf("mark stale project upload baseline conflict: %w", err)
	}
	finalRow, err := q.GetWorkspaceSyncSession(ctx, sqlcgen.GetWorkspaceSyncSessionParams{WorkspaceID: row.WorkspaceID, ID: row.ID})
	if err != nil {
		return fmt.Errorf("read stale project upload session: %w", err)
	}
	*result, err = syncSessionDomain(ctx, tx, q, finalRow)
	if err != nil {
		return err
	}
	return saveWorkspaceSyncCommitSuccess(ctx, tx, idempotency)
}
