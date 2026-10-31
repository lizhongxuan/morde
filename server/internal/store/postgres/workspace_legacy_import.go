package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"strings"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

func (s *Store) GetPendingWorkspaceLegacyVolume(ctx context.Context, workspaceID string) (app.LegacyWorkspaceVolume, error) {
	if !domain.ValidWorkspaceID(workspaceID) {
		return app.LegacyWorkspaceVolume{}, domain.NewError(domain.CodeNotFound, "workspace legacy volume not found")
	}
	row, err := s.q.GetPendingWorkspaceLegacyVolume(ctx, workspaceID)
	if err != nil {
		return app.LegacyWorkspaceVolume{}, storeError("get pending workspace legacy volume", "workspace legacy volume", err)
	}
	return app.LegacyWorkspaceVolume{
		WorkspaceID: row.WorkspaceID, VolumeName: row.VolumeName,
		Revision: row.Revision, UsedBytes: row.UsedBytes, QuotaBytes: row.QuotaBytes,
	}, nil
}

func (s *Store) EnsureWorkspaceLegacyImportReady(ctx context.Context, workspaceID string) error {
	if !domain.ValidWorkspaceID(workspaceID) {
		return domain.InvalidArgument("workspace_id", "workspace id is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var workspaceState, importState string
		var revision, usedBytes int64
		if err := tx.QueryRow(ctx, `
SELECT w.state, w.revision, w.used_bytes, v.import_state
FROM workspaces w
JOIN workspace_legacy_volumes v ON v.workspace_id=w.id
WHERE w.id=$1
FOR UPDATE OF w, v`, workspaceID).Scan(&workspaceState, &revision, &usedBytes, &importState); err != nil {
			return storeError("lock Workspace before legacy import cleanup", "workspace legacy volume", err)
		}
		if workspaceState != string(workspace.StateReady) {
			return domain.NewError(domain.CodeWorkspaceBusy, "workspace is not ready for legacy import")
		}
		if importState != "pending" || revision != 0 || usedBytes != 0 {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace is no longer an empty pending legacy import")
		}
		return ensureWorkspaceLegacyImportHasNoActiveWork(ctx, tx, workspaceID)
	})
}

func ensureWorkspaceLegacyImportHasNoActiveWork(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	for _, check := range []func(context.Context, pgx.Tx, string) error{
		ensureNoPendingWorkspaceSyncCommit,
		ensureNoPendingWorkspaceTaskPublish,
		ensureNoActiveWorkspaceSyncSessions,
		ensureNoActiveWorkspaceTaskOperations,
	} {
		if err := check(ctx, tx, workspaceID); err != nil {
			return err
		}
	}
	return ensureNoWorkspaceSyncReceive(ctx, tx, workspaceID, "")
}

func requireNoPendingWorkspaceLegacyImport(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_legacy_volumes WHERE workspace_id=$1 AND import_state='pending'
)`, workspaceID).Scan(&pending); err != nil {
		return fmt.Errorf("check pending legacy Workspace import: %w", err)
	}
	if pending {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has a pending legacy import")
	}
	return nil
}

func (s *Store) ListPendingWorkspaceLegacyVolumes(ctx context.Context) ([]app.LegacyWorkspaceVolume, error) {
	rows, err := s.pool.Query(ctx, `
SELECT w.id AS workspace_id, v.volume_name, w.revision, w.used_bytes, w.quota_bytes, COALESCE(v.import_error, '')
FROM workspace_legacy_volumes v
JOIN workspaces w ON w.id = v.workspace_id
WHERE v.import_state = 'pending' AND w.state = 'ready'
ORDER BY v.created_at, v.workspace_id`)
	if err != nil {
		return nil, fmt.Errorf("list pending workspace legacy volumes: %w", err)
	}
	defer rows.Close()
	values := make([]app.LegacyWorkspaceVolume, 0)
	for rows.Next() {
		var value app.LegacyWorkspaceVolume
		if err := rows.Scan(&value.WorkspaceID, &value.VolumeName, &value.Revision, &value.UsedBytes, &value.QuotaBytes, &value.ImportError); err != nil {
			return nil, fmt.Errorf("read pending workspace legacy volume: %w", err)
		}
		values = append(values, app.LegacyWorkspaceVolume{
			WorkspaceID: value.WorkspaceID, VolumeName: value.VolumeName,
			Revision: value.Revision, UsedBytes: value.UsedBytes, QuotaBytes: value.QuotaBytes,
			ImportError: value.ImportError,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending workspace legacy volumes: %w", err)
	}
	return values, nil
}

func (s *Store) MarkWorkspaceLegacyVolumeNeedsCleanup(ctx context.Context, workspaceID, reason string) error {
	if !domain.ValidWorkspaceID(workspaceID) || strings.TrimSpace(reason) == "" {
		return domain.InvalidArgument("legacy_volume", "workspace cleanup record is invalid")
	}
	result, err := s.pool.Exec(ctx, `
UPDATE workspace_legacy_volumes SET import_error = $2
WHERE workspace_id = $1 AND import_state = 'pending'`, workspaceID, reason)
	if err != nil {
		return fmt.Errorf("mark legacy workspace cleanup required: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeWorkspaceConflict, "legacy workspace import state changed before cleanup could be recorded")
	}
	return nil
}

func (s *Store) CommitWorkspaceLegacyVolumeImport(ctx context.Context, value app.LegacyWorkspaceVolume, files []workspace.File, emptyDirectories []string) (workspace.Workspace, error) {
	totalBytes, err := validateLegacyWorkspaceImport(value, files, emptyDirectories)
	if err != nil {
		return workspace.Workspace{}, err
	}
	var imported workspace.Workspace
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		// Lock both records so import state, workspace state, and the revision snapshot cannot change before commit.
		var volumeName, importState, workspaceState string
		var revision, usedBytes, quotaBytes int64
		if err := tx.QueryRow(ctx, `
SELECT v.volume_name, v.import_state, w.state, w.revision, w.used_bytes, w.quota_bytes
FROM workspace_legacy_volumes v
JOIN workspaces w ON w.id = v.workspace_id
WHERE v.workspace_id = $1
FOR UPDATE OF v, w`, value.WorkspaceID).Scan(
			&volumeName, &importState, &workspaceState, &revision, &usedBytes, &quotaBytes,
		); err != nil {
			return storeError("lock workspace legacy volume for import", "workspace legacy volume", err)
		}
		if workspaceState != string(workspace.StateReady) {
			return domain.NewError(domain.CodeWorkspaceBusy, "workspace is not ready for legacy import")
		}
		if importState != "pending" || volumeName != value.VolumeName || revision != value.Revision || usedBytes != value.UsedBytes || quotaBytes != value.QuotaBytes {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace legacy volume changed after inspection")
		}
		if revision != 0 || usedBytes != 0 {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace already contains synchronized data")
		}
		if totalBytes > quotaBytes {
			return domain.NewError(domain.CodeQuotaExceeded, "legacy workspace content exceeds workspace quota")
		}
		q := sqlcgen.New(tx)
		if err := ensureWorkspaceLegacyImportHasNoActiveWork(ctx, tx, value.WorkspaceID); err != nil {
			return err
		}
		encodedFiles, err := json.Marshal(files)
		if err != nil {
			return fmt.Errorf("encode legacy workspace files for block usage check: %w", err)
		}
		if err := ensureWorkspaceFilesBlockUsageStored(ctx, tx, value.WorkspaceID, encodedFiles); err != nil {
			return err
		}

		mutations := make([]syncMutation, 0, len(files))
		for i := range files {
			file := files[i]
			mutations = append(mutations, syncMutation{path: file.Path, file: &file})
		}
		directoryMutations := make([]syncDirectoryMutation, 0, len(emptyDirectories))
		for _, directory := range emptyDirectories {
			directoryMutations = append(directoryMutations, syncDirectoryMutation{path: directory})
		}
		if len(mutations)+len(directoryMutations) > 0 {
			if err := applyWorkspaceSyncTreeMutations(ctx, tx, value.WorkspaceID, 0, 0, 0, quotaBytes, mutations, directoryMutations); err != nil {
				return err
			}
		}
		targetRevision := int64(0)
		if len(mutations)+len(directoryMutations) > 0 {
			targetRevision++
		}
		if err := enqueueWorkspaceCurrentMaterialization(ctx, tx, value.WorkspaceID, targetRevision); err != nil {
			return err
		}

		result, err := tx.Exec(ctx, `
UPDATE workspace_legacy_volumes SET import_state = 'imported', imported_at = now(), import_error = NULL
WHERE workspace_id = $1 AND import_state = 'pending'`, value.WorkspaceID)
		if err != nil {
			return fmt.Errorf("mark workspace legacy volume imported: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace legacy volume import state changed")
		}
		row, err := q.GetWorkspace(ctx, value.WorkspaceID)
		if err != nil {
			return fmt.Errorf("read imported workspace: %w", err)
		}
		imported = workspaceDomain(row)
		return nil
	})
	if err != nil {
		return workspace.Workspace{}, err
	}
	return imported, nil
}

func validateLegacyWorkspaceImport(value app.LegacyWorkspaceVolume, files []workspace.File, emptyDirectories []string) (int64, error) {
	if !domain.ValidWorkspaceID(value.WorkspaceID) || value.VolumeName == "" || value.Revision < 0 || value.UsedBytes < 0 || value.QuotaBytes <= 0 {
		return 0, domain.InvalidArgument("legacy_volume", "workspace legacy volume snapshot is invalid")
	}
	if len(files)+len(emptyDirectories) > workspace.MaxSyncFiles {
		return 0, domain.NewError(domain.CodeQuotaExceeded, "legacy workspace content exceeds file count limit")
	}
	seen := make(map[string]string, len(files)+len(emptyDirectories))
	paths := make([]string, 0, len(files)+len(emptyDirectories))
	var totalBytes int64
	for _, file := range files {
		if err := workspace.ValidateFile(file); err != nil {
			return 0, domain.InvalidArgument("files", "legacy workspace file metadata is invalid")
		}
		if file.SizeBytes > workspace.MaxSyncFileBytes {
			return 0, domain.NewError(domain.CodeQuotaExceeded, fmt.Sprintf("legacy workspace file %q exceeds the 300 MiB file limit; cleanup is required", file.Path))
		}
		folded := strings.ToLower(file.Path)
		if previous, ok := seen[folded]; ok {
			return 0, domain.InvalidArgument("files", fmt.Sprintf("legacy workspace paths %q and %q collide case-insensitively", previous, file.Path))
		}
		seen[folded] = file.Path
		paths = append(paths, file.Path)
		if totalBytes > math.MaxInt64-file.SizeBytes {
			return 0, domain.InvalidArgument("files", "legacy workspace file sizes overflow")
		}
		totalBytes += file.SizeBytes
	}
	for _, directory := range emptyDirectories {
		if err := workspace.ValidateRelativePath(directory); err != nil {
			return 0, domain.InvalidArgument("empty_directories", "legacy workspace directory path is invalid")
		}
		folded := strings.ToLower(directory)
		if previous, ok := seen[folded]; ok {
			return 0, domain.InvalidArgument("empty_directories", fmt.Sprintf("legacy workspace paths %q and %q collide case-insensitively", previous, directory))
		}
		seen[folded] = directory
		paths = append(paths, directory)
	}
	for _, pathValue := range paths {
		for parent := path.Dir(strings.ToLower(pathValue)); parent != "."; parent = path.Dir(parent) {
			if ancestor, ok := seen[parent]; ok {
				return 0, domain.InvalidArgument("files", fmt.Sprintf("legacy workspace path %q conflicts with child %q", ancestor, pathValue))
			}
		}
	}
	return totalBytes, nil
}

var _ app.WorkspaceLegacyImportStore = (*Store)(nil)
