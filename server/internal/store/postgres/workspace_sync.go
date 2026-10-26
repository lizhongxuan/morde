package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const maxWorkspaceSyncPage int32 = 1000

func (s *Store) GetWorkspaceManifestPage(ctx context.Context, workspaceID string, revision int64, prefix, afterPath string, limit int32) (workspace.ManifestPage, error) {
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
	if _, err := s.q.GetWorkspaceRevision(ctx, sqlcgen.GetWorkspaceRevisionParams{WorkspaceID: workspaceID, Revision: revision}); err != nil {
		return workspace.ManifestPage{}, storeError("get workspace manifest revision", "workspace revision", err)
	}
	rows, err := s.q.ListWorkspaceRevisionFilesPage(ctx, sqlcgen.ListWorkspaceRevisionFilesPageParams{
		WorkspaceID: workspaceID, ValidFromRevision: revision, Column3: prefix,
		FilePath: afterPath, Limit: limit + 1,
	})
	if err != nil {
		return workspace.ManifestPage{}, fmt.Errorf("list workspace manifest page: %w", err)
	}
	hasMore := int32(len(rows)) > limit
	if hasMore {
		rows = rows[:limit]
	}
	files := make([]workspace.File, 0, len(rows))
	for _, row := range rows {
		file, err := workspaceFileFromRow(row.FilePath, row.SizeBytes, row.Sha256, row.Mode, row.Chunks)
		if err != nil {
			return workspace.ManifestPage{}, err
		}
		files = append(files, file)
	}
	page := workspace.ManifestPage{WorkspaceID: workspaceID, Revision: revision, Files: files}
	if hasMore && len(files) > 0 {
		cursor := workspace.EncodeManifestCursor(workspace.ManifestCursor{Revision: revision, Prefix: prefix, After: files[len(files)-1].Path})
		page.NextCursor = &cursor
	}
	return page, nil
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
	if !domain.ValidWorkspaceID(workspaceID) || afterRevision < 0 || limit < 1 || limit > 500 {
		return workspace.ChangePage{}, domain.InvalidArgument("changes", "workspace change query is invalid")
	}
	if afterPath != "" {
		if err := workspace.ValidateRelativePath(afterPath); err != nil {
			return workspace.ChangePage{}, domain.InvalidArgument("after", "workspace changes cursor path is invalid")
		}
	}
	if _, err := s.q.GetWorkspace(ctx, workspaceID); err != nil {
		return workspace.ChangePage{}, storeError("get workspace changes", "workspace", err)
	}
	rows, err := s.q.ListWorkspaceChangesPage(ctx, sqlcgen.ListWorkspaceChangesPageParams{
		WorkspaceID: workspaceID, Revision: afterRevision, FilePath: afterPath, Limit: limit + 1,
	})
	if err != nil {
		return workspace.ChangePage{}, fmt.Errorf("list workspace changes: %w", err)
	}
	hasMore := int32(len(rows)) > limit
	if hasMore {
		rows = rows[:limit]
	}
	items := make([]workspace.Change, 0, len(rows))
	for _, row := range rows {
		item := workspace.Change{Revision: row.Revision, Path: row.FilePath, Action: workspace.ChangeAction(row.Action)}
		if row.Action == string(workspace.ChangeUpsert) {
			var file workspace.File
			if err := json.Unmarshal(row.File, &file); err != nil {
				return workspace.ChangePage{}, fmt.Errorf("decode workspace change file: %w", err)
			}
			if err := workspace.ValidateFile(file); err != nil || file.Path != row.FilePath {
				return workspace.ChangePage{}, domain.NewError(domain.CodeInternal, "stored workspace change is invalid")
			}
			item.File = &file
		} else if row.Action != string(workspace.ChangeDelete) {
			return workspace.ChangePage{}, domain.NewError(domain.CodeInternal, "stored workspace change action is invalid")
		}
		items = append(items, item)
	}
	cursor := workspace.ChangeCursor{Revision: afterRevision, Path: afterPath}
	if len(items) > 0 {
		last := items[len(items)-1]
		cursor = workspace.ChangeCursor{Revision: last.Revision, Path: last.Path}
	}
	return workspace.ChangePage{Items: items, NextCursor: workspace.EncodeChangeCursor(cursor)}, nil
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
	var created workspace.SyncSession
	replayed := false
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
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
		var revision int64
		if err := tx.QueryRow(ctx, `SELECT owner_id, state, revision FROM workspaces WHERE id=$1 FOR UPDATE`, value.WorkspaceID).Scan(&ownerID, &state, &revision); err != nil {
			return storeError("lock workspace for sync session", "workspace", err)
		}
		if ownerID != value.OwnerID || state != string(workspace.StateReady) {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		stored, wasReplay, err := reserveIdempotency(ctx, tx, idempotency, value.ID, "")
		if err != nil {
			return err
		}
		if wasReplay {
			row, err := sqlcgen.New(tx).GetWorkspaceSyncSession(ctx, sqlcgen.GetWorkspaceSyncSessionParams{WorkspaceID: value.WorkspaceID, ID: stored.ResourceID})
			if err != nil {
				return storeError("replay workspace sync session", "workspace sync session", err)
			}
			if row.OwnerID != value.OwnerID || row.ReplicaID != value.ReplicaID {
				return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key is bound to another sync session")
			}
			created, err = syncSessionDomain(ctx, sqlcgen.New(tx), row)
			if err != nil {
				return err
			}
			replayed = true
			return nil
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
		}
		if revision != value.PinnedRevision || value.TargetRevision != revision {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace changed while the sync plan was being created")
		}
		hasUploadBlocks := false
		for _, block := range blocks {
			if block.Role == workspace.BlockUpload {
				hasUploadBlocks = true
				break
			}
		}
		if hasUploadBlocks {
			if err := reserveWorkspaceBlockUsage(ctx, tx, value.WorkspaceID, ownerID, reservations, maxOwnerBytes); err != nil {
				return err
			}
		}
		q := sqlcgen.New(tx)
		row, err := q.CreateWorkspaceSyncSession(ctx, params)
		if err != nil {
			return fmt.Errorf("create workspace sync session: %w", err)
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
		created, err = syncSessionDomain(ctx, q, row)
		return err
	})
	if err != nil {
		return workspace.SyncSession{}, false, err
	}
	return created, replayed, nil
}

func (s *Store) GetWorkspaceSyncSession(ctx context.Context, workspaceID, sessionID string) (workspace.SyncSession, error) {
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) {
		return workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	row, err := s.q.GetWorkspaceSyncSession(ctx, sqlcgen.GetWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
	if err != nil {
		return workspace.SyncSession{}, storeError("get workspace sync session", "workspace sync session", err)
	}
	return syncSessionDomain(ctx, s.q, row)
}

func (s *Store) ListDeviceWorkspaceSyncProgress(ctx context.Context, ownerID, workspaceID string) ([]workspace.DeviceSyncProgress, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) {
		return nil, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	rows, err := s.q.ListDeviceWorkspaceSyncProgress(ctx, sqlcgen.ListDeviceWorkspaceSyncProgressParams{ID: workspaceID, OwnerID: ownerID})
	if err != nil {
		return nil, fmt.Errorf("list device workspace sync progress: %w", err)
	}
	items := make([]workspace.DeviceSyncProgress, 0, len(rows))
	for _, row := range rows {
		items = append(items, workspace.DeviceSyncProgress{
			WorkspaceID: row.WorkspaceID, DeviceID: row.DeviceID, SessionID: row.SessionID,
			Direction: workspace.SyncDirection(row.Direction), State: workspace.SyncState(row.State), Stage: workspace.SyncStage(row.Stage),
			PlannedBytes: row.PlannedBytes, ConfirmedBytes: row.ConfirmedBytes,
			DownloadActionsTotal: row.DownloadActionsTotal, DownloadActionsDone: row.DownloadActionsDone,
			UpdatedAt: row.UpdatedAt.Time.UTC(),
		})
	}
	return items, nil
}

func (s *Store) ListWorkspaceSyncTransferBlocks(ctx context.Context, workspaceID, sessionID string) ([]workspace.TransferBlock, error) {
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) {
		return nil, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	rows, err := s.q.ListWorkspaceSyncTransferBlocks(ctx, sqlcgen.ListWorkspaceSyncTransferBlocksParams{WorkspaceID: workspaceID, SessionID: sessionID})
	if err != nil {
		return nil, fmt.Errorf("list workspace sync transfer blocks: %w", err)
	}
	blocks := make([]workspace.TransferBlock, 0, len(rows))
	for _, row := range rows {
		blocks = append(blocks, workspace.TransferBlock{
			Chunk: workspace.Chunk{SHA256: row.Sha256, SizeBytes: row.SizeBytes}, Role: workspace.BlockRole(row.Role), Received: row.Received,
		})
	}
	return blocks, nil
}

func lockWorkspaceForSyncMutation(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	var lockedID string
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&lockedID); err != nil {
		return storeError("lock workspace before sync session mutation", "workspace", err)
	}
	return nil
}

func (s *Store) AckWorkspaceSyncBlock(ctx context.Context, workspaceID, sessionID, digest string, allocatedBytes, maxOwnerBytes int64) error {
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) || !validPostgresSHA256(digest) {
		return domain.NewError(domain.CodeNotFound, "workspace sync block not found")
	}
	if allocatedBytes < 0 || maxOwnerBytes <= 0 {
		return domain.InvalidArgument("workspace_block_usage", "workspace block allocation is invalid")
	}
	var settlementErr error
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceForSyncMutation(ctx, tx, workspaceID); err != nil {
			return err
		}
		q := sqlcgen.New(tx)
		row, err := q.LockWorkspaceSyncSession(ctx, sqlcgen.LockWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if err != nil {
			return storeError("lock workspace sync session for block acknowledgement", "workspace sync session", err)
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
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceForSyncMutation(ctx, tx, workspaceID); err != nil {
			return err
		}
		q := sqlcgen.New(tx)
		row, err := q.LockWorkspaceSyncSession(ctx, sqlcgen.LockWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if err != nil {
			return storeError("lock workspace sync session for download acknowledgement", "workspace sync session", err)
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
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceForSyncMutation(ctx, tx, workspaceID); err != nil {
			return err
		}
		q := sqlcgen.New(tx)
		session, err := q.LockWorkspaceSyncSession(ctx, sqlcgen.LockWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if err != nil {
			return storeError("lock workspace sync session for download action acknowledgement", "workspace sync session", err)
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

func (s *Store) CommitWorkspaceSyncSession(ctx context.Context, workspaceID, sessionID string, idempotency app.IdempotencyRecord) (workspace.SyncSession, bool, error) {
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) || idempotency.Operation != "workspace.sync.commit" || idempotency.ResourceID != sessionID {
		return workspace.SyncSession{}, false, domain.InvalidArgument("sync_session", "workspace sync commit fields are invalid")
	}
	var result workspace.SyncSession
	replayed := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		// Host authorization is revalidated in the binding transition lock order
		// so detach or rebind cannot race a revision commit.
		var replicaID string
		if err := tx.QueryRow(ctx, `SELECT replica_id FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2`, workspaceID, sessionID).Scan(&replicaID); err != nil {
			return storeError("read workspace sync replica before commit", "workspace sync session", err)
		}
		if !validReplicaID(replicaID) {
			return domain.NewError(domain.CodeInternal, "stored workspace sync replica is invalid")
		}
		hostReplica, hostScoped := parseHostSyncReplicaID(replicaID)
		if hostScoped {
			if hostReplica.workspaceID != workspaceID {
				return domain.NewError(domain.CodeWorkspaceConflict, "Host workspace sync replica does not match its session")
			}
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
		var currentRevision, currentBytes, quotaBytes, currentFileCount, revisionBytes int64
		if err := tx.QueryRow(ctx, `
SELECT w.owner_id, w.state, w.revision, w.used_bytes, w.quota_bytes,
       r.file_count, r.total_bytes
FROM workspaces w
JOIN workspace_revisions r ON r.workspace_id=w.id AND r.revision=w.revision
WHERE w.id=$1 FOR UPDATE OF w`, workspaceID).Scan(&workspaceOwner, &workspaceState, &currentRevision, &currentBytes, &quotaBytes, &currentFileCount, &revisionBytes); err != nil {
			return storeError("lock workspace for sync commit", "workspace", err)
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
		if row.ReplicaID != replicaID {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace sync replica changed before commit")
		}
		stored, wasReplay, err := reserveIdempotency(ctx, tx, idempotency, sessionID, "")
		if err != nil {
			return err
		}
		if wasReplay {
			if stored.ResourceID != sessionID {
				return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key is bound to another sync session")
			}
			result, err = syncSessionDomain(ctx, q, row)
			replayed = err == nil
			return err
		}
		if isTerminalSyncState(workspace.SyncState(row.State)) {
			result, err = syncSessionDomain(ctx, q, row)
			return err
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
		mutationPaths := syncMutationPaths(uploadFiles, deletions)
		pinnedFiles, err := loadWorkspaceRevisionFiles(ctx, tx, workspaceID, row.PinnedRevision, mutationPaths)
		if err != nil {
			return err
		}
		currentFiles, err := loadWorkspaceRevisionFiles(ctx, tx, workspaceID, currentRevision, mutationPaths)
		if err != nil {
			return err
		}
		pinnedByPath := fileMapByFoldedPath(pinnedFiles)
		currentByPath := fileMapByFoldedPath(currentFiles)
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
				continue
			}
			if currentOK != pinnedOK || (currentOK && !sameWorkspaceFile(current, pinned)) {
				reason := "content_changed"
				conflictPath := file.Path
				if currentOK && pinnedOK && current.Path != pinned.Path && strings.EqualFold(current.Path, pinned.Path) {
					reason = "case_collision"
					conflictPath = current.Path
				}
				conflicts = append(conflicts, workspace.Conflict{Path: conflictPath, Reason: reason, LocalSHA256: stringPointer(file.SHA256), RemoteSHA256: fileHashPointer(current, currentOK)})
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
			if !currentOK {
				continue
			}
			if !pinnedOK || !sameWorkspaceFile(current, pinned) {
				conflicts = append(conflicts, workspace.Conflict{Path: path, Reason: "delete_changed", RemoteSHA256: fileHashPointer(current, currentOK)})
				continue
			}
			mutations = append(mutations, syncMutation{path: current.Path})
		}
		if err := ensureWorkspaceMutationsBlockUsageStored(ctx, tx, workspaceID, mutations); err != nil {
			return err
		}
		if err := applyWorkspaceSyncMutations(ctx, tx, workspaceID, currentRevision, currentFileCount, currentBytes, quotaBytes, mutations); err != nil {
			return err
		}
		targetRevision := currentRevision
		if len(mutations) > 0 {
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
		result, err = syncSessionDomain(ctx, q, finalRow)
		return err
	})
	if err != nil {
		return workspace.SyncSession{}, false, err
	}
	return result, replayed, nil
}

func (s *Store) CancelWorkspaceSyncSession(ctx context.Context, workspaceID, sessionID string, idempotency app.IdempotencyRecord) (workspace.SyncSession, bool, error) {
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) || idempotency.Operation != "workspace.sync.cancel" || idempotency.ResourceID != sessionID {
		return workspace.SyncSession{}, false, domain.InvalidArgument("sync_session", "workspace sync cancel fields are invalid")
	}
	var result workspace.SyncSession
	replayed := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceForSyncMutation(ctx, tx, workspaceID); err != nil {
			return err
		}
		q := sqlcgen.New(tx)
		row, err := q.LockWorkspaceSyncSession(ctx, sqlcgen.LockWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if err != nil {
			return storeError("lock workspace sync session for cancellation", "workspace sync session", err)
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
			result, err = syncSessionDomain(ctx, q, row)
			replayed = err == nil
			return err
		}
		if isTerminalSyncState(workspace.SyncState(row.State)) {
			result, err = syncSessionDomain(ctx, q, row)
			return err
		}
		cancelled, err := q.CancelWorkspaceSyncSession(ctx, sqlcgen.CancelWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace sync session changed during cancellation")
		}
		if err != nil {
			return fmt.Errorf("cancel workspace sync session: %w", err)
		}
		result, err = syncSessionDomain(ctx, q, cancelled)
		return err
	})
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
	expectedActions := workspace.DownloadActionsForPlan(value.DownloadFiles, value.DownloadDeletedPaths)
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
		DownloadDeletedPaths: nonNilStrings(value.DownloadDeletedPaths),
		AckCursor:            ack, Conflicts: conflicts,
	}, nil
}

func syncSessionDomain(ctx context.Context, q *sqlcgen.Queries, row sqlcgen.WorkspaceSyncSession) (workspace.SyncSession, error) {
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
	value.Direction, value.Policy, value.State, value.Stage = workspace.SyncDirection(row.Direction), workspace.SyncPolicy(row.Policy), workspace.SyncState(row.State), workspace.SyncStage(row.Stage)
	value.BaseRevision, value.TargetRevision, value.PinnedRevision = row.BaseRevision, row.TargetRevision, row.PinnedRevision
	value.PlannedBytes, value.ConfirmedBytes = row.PlannedBytes, row.ConfirmedBytes
	value.Scope, value.DeletedPaths, value.UploadDeletedPaths = nonNilStrings(row.Scope), nonNilStrings(row.DeletedPaths), nonNilStrings(row.UploadDeletedPaths)
	value.DownloadDeletedPaths = nonNilStrings(row.DownloadDeletedPaths)
	if row.AckCursor.Valid {
		value.AckCursor = &row.AckCursor.String
	}
	value.CreatedAt, value.UpdatedAt = row.CreatedAt.Time, row.UpdatedAt.Time
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

func workspaceFileFromRow(path string, size int64, digest string, mode int32, encodedChunks []byte) (workspace.File, error) {
	var chunks []workspace.Chunk
	if err := json.Unmarshal(encodedChunks, &chunks); err != nil {
		return workspace.File{}, fmt.Errorf("decode workspace file chunks: %w", err)
	}
	if chunks == nil {
		chunks = []workspace.Chunk{}
	}
	file := workspace.File{Path: path, SizeBytes: size, SHA256: digest, Mode: uint32(mode), Chunks: chunks}
	if err := workspace.ValidateFile(file); err != nil {
		return workspace.File{}, domain.NewError(domain.CodeInternal, "stored workspace manifest file is invalid")
	}
	return file, nil
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

func loadWorkspaceRevisionFiles(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64, paths []string) ([]workspace.File, error) {
	if len(paths) == 0 {
		return []workspace.File{}, nil
	}
	rows, err := tx.Query(ctx, `
SELECT file_path, size_bytes, sha256, mode, chunks
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
		var path, digest string
		var size int64
		var mode int32
		var chunks []byte
		if err := rows.Scan(&path, &size, &digest, &mode, &chunks); err != nil {
			return nil, fmt.Errorf("scan workspace revision file: %w", err)
		}
		file, err := workspaceFileFromRow(path, size, digest, mode, chunks)
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

func sameWorkspaceFile(left, right workspace.File) bool {
	return left.Path == right.Path && left.SizeBytes == right.SizeBytes && left.SHA256 == right.SHA256 && left.Mode == right.Mode
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

func applyWorkspaceSyncMutations(ctx context.Context, tx pgx.Tx, workspaceID string, revision, currentFileCount, currentBytes, quotaBytes int64, mutations []syncMutation) error {
	if len(mutations) == 0 {
		return nil
	}
	q := sqlcgen.New(tx)
	nextRevision := revision + 1
	var deltaBytes, deltaFiles int64
	currentRows := make(map[string]workspace.File, len(mutations))
	paths := make([]string, 0, len(mutations))
	for _, mutation := range mutations {
		paths = append(paths, mutation.path)
	}
	var rows []struct {
		FilePath  string `json:"file_path"`
		SizeBytes int64  `json:"size_bytes"`
		Sha256    string `json:"sha256"`
		Mode      int32  `json:"mode"`
		Chunks    []byte `json:"chunks"`
	}
	foldedPaths := lowerWorkspacePaths(paths)
	encoded, err := tx.Query(ctx, `
SELECT file_path, size_bytes, sha256, mode, chunks
FROM workspace_revision_files
WHERE workspace_id=$1 AND valid_to_revision IS NULL AND folded_path = ANY($2::text[])`, workspaceID, foldedPaths)
	if err != nil {
		return fmt.Errorf("read workspace files for sync apply: %w", err)
	}
	for encoded.Next() {
		var row struct {
			FilePath  string `json:"file_path"`
			SizeBytes int64  `json:"size_bytes"`
			Sha256    string `json:"sha256"`
			Mode      int32  `json:"mode"`
			Chunks    []byte `json:"chunks"`
		}
		if err := encoded.Scan(&row.FilePath, &row.SizeBytes, &row.Sha256, &row.Mode, &row.Chunks); err != nil {
			encoded.Close()
			return fmt.Errorf("scan workspace files for sync apply: %w", err)
		}
		rows = append(rows, row)
	}
	queryErr := encoded.Err()
	encoded.Close()
	if queryErr != nil {
		return fmt.Errorf("iterate workspace files for sync apply: %w", queryErr)
	}
	for _, row := range rows {
		file, err := workspaceFileFromRow(row.FilePath, row.SizeBytes, row.Sha256, row.Mode, row.Chunks)
		if err != nil {
			return err
		}
		currentRows[strings.ToLower(file.Path)] = file
	}
	for _, mutation := range mutations {
		oldFile, oldOK := currentRows[strings.ToLower(mutation.path)]
		if oldOK {
			deltaBytes -= oldFile.SizeBytes
			deltaFiles--
		}
		if mutation.file != nil {
			deltaBytes += mutation.file.SizeBytes
			deltaFiles++
		}
	}
	if currentBytes < 0 || deltaBytes > 0 && currentBytes > int64(^uint64(0)>>1)-deltaBytes || deltaBytes < 0 && deltaBytes < -currentBytes || currentFileCount+deltaFiles < 0 || currentFileCount+deltaFiles > workspace.MaxSyncFiles {
		return domain.NewError(domain.CodeQuotaExceeded, "workspace storage quota would be exceeded")
	}
	nextBytes := currentBytes + deltaBytes
	if nextBytes > quotaBytes {
		return domain.NewError(domain.CodeQuotaExceeded, "workspace storage quota would be exceeded")
	}
	if revision == int64(^uint64(0)>>1) {
		return domain.NewError(domain.CodeWorkspaceConflict, "workspace revision is exhausted")
	}
	if err := q.CreateWorkspaceRevision(ctx, sqlcgen.CreateWorkspaceRevisionParams{
		WorkspaceID: workspaceID, Revision: nextRevision,
		FileCount: currentFileCount + deltaFiles, TotalBytes: nextBytes,
	}); err != nil {
		return fmt.Errorf("create workspace revision: %w", err)
	}
	for _, mutation := range mutations {
		oldFile, oldOK := currentRows[strings.ToLower(mutation.path)]
		if oldOK {
			affected, err := q.CloseWorkspaceRevisionFile(ctx, sqlcgen.CloseWorkspaceRevisionFileParams{
				WorkspaceID: workspaceID, FilePath: oldFile.Path, ValidToRevision: pgtype.Int8{Int64: nextRevision, Valid: true},
			})
			if err != nil {
				return fmt.Errorf("close prior workspace file revision: %w", err)
			}
			if affected != 1 {
				return domain.NewError(domain.CodeWorkspaceConflict, "workspace file changed during sync apply")
			}
		}
		if mutation.file == nil {
			if oldOK {
				if err := q.InsertWorkspaceChange(ctx, sqlcgen.InsertWorkspaceChangeParams{
					WorkspaceID: workspaceID, Revision: nextRevision, FilePath: oldFile.Path, Action: string(workspace.ChangeDelete), File: nil,
				}); err != nil {
					return fmt.Errorf("record workspace file deletion: %w", err)
				}
			}
			continue
		}
		if oldOK && oldFile.Path != mutation.file.Path && strings.EqualFold(oldFile.Path, mutation.file.Path) {
			if err := q.InsertWorkspaceChange(ctx, sqlcgen.InsertWorkspaceChangeParams{
				WorkspaceID: workspaceID, Revision: nextRevision, FilePath: oldFile.Path, Action: string(workspace.ChangeDelete), File: nil,
			}); err != nil {
				return fmt.Errorf("record prior workspace path for case-only rename: %w", err)
			}
		}
		fileJSON, err := json.Marshal(mutation.file)
		if err != nil {
			return fmt.Errorf("encode workspace revision file: %w", err)
		}
		chunksJSON, err := json.Marshal(nonNilChunks(mutation.file.Chunks))
		if err != nil {
			return fmt.Errorf("encode workspace file chunks: %w", err)
		}
		if err := q.InsertWorkspaceRevisionFile(ctx, sqlcgen.InsertWorkspaceRevisionFileParams{
			WorkspaceID: workspaceID, ValidFromRevision: nextRevision,
			FilePath: mutation.file.Path, FoldedPath: strings.ToLower(mutation.file.Path),
			SizeBytes: mutation.file.SizeBytes, Sha256: mutation.file.SHA256, Mode: int32(mutation.file.Mode), Chunks: chunksJSON,
		}); err != nil {
			return fmt.Errorf("insert workspace file revision: %w", err)
		}
		if err := q.InsertWorkspaceChange(ctx, sqlcgen.InsertWorkspaceChangeParams{
			WorkspaceID: workspaceID, Revision: nextRevision, FilePath: mutation.file.Path,
			Action: string(workspace.ChangeUpsert), File: fileJSON,
		}); err != nil {
			return fmt.Errorf("record workspace file update: %w", err)
		}
	}
	command, err := tx.Exec(ctx, `
UPDATE workspaces SET revision=$2, used_bytes=$3, updated_at=now()
	WHERE id=$1 AND state='ready' AND revision=$4 AND quota_bytes >= $3`, workspaceID, nextRevision, nextBytes, revision)
	if err != nil {
		return fmt.Errorf("update workspace revision after sync: %w", err)
	}
	if command.RowsAffected() != 1 {
		return domain.NewError(domain.CodeWorkspaceConflict, "workspace changed during sync apply")
	}
	if _, err := tx.Exec(ctx, `
UPDATE workspace_bindings SET desired_revision=$2, updated_at=now()
WHERE workspace_id=$1 AND state IN ('preparing','active','unbinding') AND desired_revision < $2`, workspaceID, nextRevision); err != nil {
		return fmt.Errorf("advance bound host workspace revisions: %w", err)
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
