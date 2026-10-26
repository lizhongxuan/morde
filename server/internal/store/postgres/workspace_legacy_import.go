package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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

func (s *Store) ListPendingWorkspaceLegacyVolumes(ctx context.Context) ([]app.LegacyWorkspaceVolume, error) {
	rows, err := s.q.ListPendingWorkspaceLegacyVolumes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pending workspace legacy volumes: %w", err)
	}
	values := make([]app.LegacyWorkspaceVolume, 0, len(rows))
	for _, row := range rows {
		values = append(values, app.LegacyWorkspaceVolume{
			WorkspaceID: row.WorkspaceID, VolumeName: row.VolumeName,
			Revision: row.Revision, UsedBytes: row.UsedBytes, QuotaBytes: row.QuotaBytes,
		})
	}
	return values, nil
}

func (s *Store) CommitWorkspaceLegacyVolumeImport(ctx context.Context, value app.LegacyWorkspaceVolume, files []workspace.File) (workspace.Workspace, error) {
	totalBytes, err := validateLegacyWorkspaceImport(value, files)
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
		active, err := q.WorkspaceHasActiveSyncSession(ctx, value.WorkspaceID)
		if err != nil {
			return fmt.Errorf("check active workspace sync before legacy import: %w", err)
		}
		if active {
			return domain.NewError(domain.CodeWorkspaceBusy, "workspace has an active sync session")
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
		if len(mutations) > 0 {
			if err := applyWorkspaceSyncMutations(ctx, tx, value.WorkspaceID, 0, 0, 0, quotaBytes, mutations); err != nil {
				return err
			}
		}

		result, err := tx.Exec(ctx, `
UPDATE workspace_legacy_volumes SET import_state = 'imported', imported_at = now()
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

func validateLegacyWorkspaceImport(value app.LegacyWorkspaceVolume, files []workspace.File) (int64, error) {
	if !domain.ValidWorkspaceID(value.WorkspaceID) || value.VolumeName == "" || value.Revision < 0 || value.UsedBytes < 0 || value.QuotaBytes <= 0 {
		return 0, domain.InvalidArgument("legacy_volume", "workspace legacy volume snapshot is invalid")
	}
	if len(files) > workspace.MaxSyncFiles {
		return 0, domain.NewError(domain.CodeQuotaExceeded, "legacy workspace content exceeds file count limit")
	}
	seen := make(map[string]string, len(files))
	var totalBytes int64
	for _, file := range files {
		if err := workspace.ValidateFile(file); err != nil {
			return 0, domain.InvalidArgument("files", "legacy workspace file metadata is invalid")
		}
		folded := strings.ToLower(file.Path)
		if previous, ok := seen[folded]; ok {
			return 0, domain.InvalidArgument("files", fmt.Sprintf("legacy workspace paths %q and %q collide case-insensitively", previous, file.Path))
		}
		seen[folded] = file.Path
		if totalBytes > math.MaxInt64-file.SizeBytes {
			return 0, domain.InvalidArgument("files", "legacy workspace file sizes overflow")
		}
		totalBytes += file.SizeBytes
	}
	return totalBytes, nil
}

var _ app.WorkspaceLegacyImportStore = (*Store)(nil)
