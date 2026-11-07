package postgres

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

func (s *Store) RestoreWorkspaceRevision(ctx context.Context, ownerID, workspaceID string, targetRevision, expectedCurrentRevision int64, paths []string, idempotency app.IdempotencyRecord) (app.WorkspaceRestoreResult, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || targetRevision < 0 || expectedCurrentRevision < 0 ||
		idempotency.OwnerID != ownerID || idempotency.Operation != "workspace.restore" || len(paths) == 0 || len(paths) > 1000 {
		return app.WorkspaceRestoreResult{}, domain.InvalidArgument("restore", "Workspace restore request is invalid")
	}
	for i, path := range paths {
		if workspace.ValidateRelativePath(path) != nil || (i > 0 && strings.ToLower(paths[i-1]) >= strings.ToLower(path)) ||
			(i > 0 && strings.HasPrefix(strings.ToLower(path), strings.ToLower(paths[i-1])+"/")) {
			return app.WorkspaceRestoreResult{}, domain.InvalidArgument("paths", "Workspace restore paths are invalid or overlap")
		}
	}
	if err := validateIdempotency(idempotency); err != nil {
		return app.WorkspaceRestoreResult{}, err
	}

	result := app.WorkspaceRestoreResult{
		WorkspaceID: workspaceID, TargetRevision: targetRevision, ExpectedCurrentRevision: expectedCurrentRevision,
	}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceForSyncMutation(ctx, tx, workspaceID); err != nil {
			return err
		}
		var state, storedOwner string
		var currentRevision, currentFileCount, currentBytes, quotaBytes int64
		var deleted, readOnly bool
		if err := tx.QueryRow(ctx, `SELECT owner_id, state, revision, used_bytes, quota_bytes, deleted_at IS NOT NULL, is_read_only
FROM workspaces WHERE id=$1`, workspaceID).Scan(&storedOwner, &state, &currentRevision, &currentBytes, &quotaBytes, &deleted, &readOnly); err != nil {
			return storeError("read Workspace before restore", "workspace", err)
		}
		if storedOwner != ownerID || deleted {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if readOnly {
			return domain.NewError(domain.CodeForbidden, "workspace is shared as read-only")
		}

		stored, replayed, err := reserveIdempotency(ctx, tx, idempotency, workspaceID, "")
		if err != nil {
			return err
		}
		if replayed {
			if stored.ResourceID != workspaceID {
				return domain.NewError(domain.CodeIdempotencyConflict, "Workspace restore key is bound to another workspace")
			}
			resultRevision, err := strconv.ParseInt(stored.RelatedID, 10, 64)
			if err != nil || resultRevision < 0 {
				return domain.NewError(domain.CodeInternal, "Workspace restore receipt is invalid")
			}
			result.Revision, result.Replayed = resultRevision, true
			return nil
		}
		if state != string(workspace.StateReady) || currentRevision != expectedCurrentRevision || targetRevision > currentRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed or requested restore revision is unavailable")
		}
		var desiredRevision, appliedRevision int64
		if err := tx.QueryRow(ctx, `SELECT desired_revision, applied_revision FROM workspace_current_materializations WHERE workspace_id=$1`, workspaceID).
			Scan(&desiredRevision, &appliedRevision); err != nil {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace current revision is not confirmed")
		}
		if desiredRevision != currentRevision || appliedRevision != currentRevision {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has an unmaterialized revision")
		}
		var blocked bool
		if err := tx.QueryRow(ctx, `SELECT
EXISTS (SELECT 1 FROM workspace_bindings WHERE workspace_id=$1 AND state <> 'detached') OR
EXISTS (SELECT 1 FROM workspace_sync_sessions WHERE workspace_id=$1 AND state IN ('planned','transferring','verifying','applying')) OR
EXISTS (SELECT 1 FROM task_operations WHERE workspace_id=$1 AND status IN ('prepared','dispatched','unknown')) OR
EXISTS (SELECT 1 FROM task_mcp_connector_operations o JOIN tasks t ON t.id=o.task_id
			WHERE t.workspace_id=$1 AND o.status IN ('prepared','dispatched','unknown')) OR
EXISTS (SELECT 1 FROM task_workspace_publish_receipts WHERE workspace_id=$1 AND state='pending') OR
EXISTS (SELECT 1 FROM task_hub_operations o WHERE o.workspace_id=$1 AND o.action='file.put'
  AND o.status IN ('prepared','dispatched','unknown') AND o.sync_stage IN ('pending','local_applied','sync_pending'))`, workspaceID).Scan(&blocked); err != nil {
			return fmt.Errorf("check Workspace writers before restore: %w", err)
		}
		if blocked {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has a binding or in-flight write; stop and settle it before restore")
		}
		if err := ensureNoWorkspaceSyncReceive(ctx, tx, workspaceID, ""); err != nil {
			return err
		}

		currentFiles, currentDirectories, currentDirectoryMetadata, err := loadWorkspaceRestoreSnapshot(ctx, tx, workspaceID, currentRevision)
		if err != nil {
			return err
		}
		targetFiles, targetDirectories, targetDirectoryMetadata, err := loadWorkspaceRestoreSnapshot(ctx, tx, workspaceID, targetRevision)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT file_count FROM workspace_revisions WHERE workspace_id=$1 AND revision=$2`, workspaceID, currentRevision).Scan(&currentFileCount); err != nil {
			return storeError("read current Workspace revision size", "workspace revision", err)
		}
		var manifestBytes int64
		for _, file := range currentFiles {
			manifestBytes += file.SizeBytes
		}
		if currentFileCount != int64(len(currentFiles)) || manifestBytes != currentBytes {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace current totals do not match its committed manifest")
		}
		currentFileMap := workspaceFilesByFoldedPath(currentFiles)
		targetFileMap := workspaceFilesByFoldedPath(targetFiles)
		currentDirectoryMap := workspaceDirectoriesByFoldedPath(currentDirectories)
		targetDirectoryMap := workspaceDirectoriesByFoldedPath(targetDirectories)
		currentDirectoryMetadataMap := workspaceDirectoryMetadataByFoldedPath(currentDirectoryMetadata)
		targetDirectoryMetadataMap := workspaceDirectoryMetadataByFoldedPath(targetDirectoryMetadata)
		keys := make(map[string]struct{}, len(currentFileMap)+len(targetFileMap)+len(currentDirectoryMap)+len(targetDirectoryMap)+
			len(currentDirectoryMetadataMap)+len(targetDirectoryMetadataMap))
		for key := range currentFileMap {
			keys[key] = struct{}{}
		}
		for key := range targetFileMap {
			keys[key] = struct{}{}
		}
		for key := range currentDirectoryMap {
			keys[key] = struct{}{}
		}
		for key := range targetDirectoryMap {
			keys[key] = struct{}{}
		}
		for key := range currentDirectoryMetadataMap {
			keys[key] = struct{}{}
		}
		for key := range targetDirectoryMetadataMap {
			keys[key] = struct{}{}
		}
		ordered := make([]string, 0, len(keys))
		for key := range keys {
			ordered = append(ordered, key)
		}
		sort.Strings(ordered)
		fileMutations := make([]syncMutation, 0)
		directoryMutations := make([]syncDirectoryMutation, 0)
		directoryMetadataMutations := make([]syncDirectoryMetadataMutation, 0)
		selectedAny := make([]bool, len(paths))
		for _, key := range ordered {
			currentFile, hasCurrentFile := currentFileMap[key]
			targetFile, hasTargetFile := targetFileMap[key]
			currentDirectory, hasCurrentDirectory := currentDirectoryMap[key]
			targetDirectory, hasTargetDirectory := targetDirectoryMap[key]
			currentMetadata, hasCurrentMetadata := currentDirectoryMetadataMap[key]
			targetMetadata, hasTargetMetadata := targetDirectoryMetadataMap[key]
			path := workspaceRestoreEntryPath(currentFile, hasCurrentFile, targetFile, hasTargetFile,
				currentDirectory, hasCurrentDirectory, targetDirectory, hasTargetDirectory, currentMetadata, hasCurrentMetadata, targetMetadata, hasTargetMetadata)
			selected := false
			for i, root := range paths {
				if workspaceRestorePathSelected(path, root) {
					selected, selectedAny[i] = true, true
				}
			}
			if !selected || workspaceRestoreEntriesEqual(currentFile, hasCurrentFile, targetFile, hasTargetFile,
				currentDirectory, hasCurrentDirectory, targetDirectory, hasTargetDirectory,
				currentMetadata, hasCurrentMetadata, targetMetadata, hasTargetMetadata) {
				continue
			}
			if hasTargetFile {
				copy := targetFile
				fileMutations = append(fileMutations, syncMutation{path: targetFile.Path, file: &copy})
			} else if hasCurrentFile {
				fileMutations = append(fileMutations, syncMutation{path: currentFile.Path})
			}
			if hasTargetDirectory {
				directoryMutations = append(directoryMutations, syncDirectoryMutation{path: targetDirectory})
			} else if hasCurrentDirectory {
				directoryMutations = append(directoryMutations, syncDirectoryMutation{path: currentDirectory, delete: true})
			}
			if hasTargetMetadata {
				copy := targetMetadata
				directoryMetadataMutations = append(directoryMetadataMutations, syncDirectoryMetadataMutation{path: targetMetadata.Path, directory: &copy})
			} else if hasCurrentMetadata {
				directoryMetadataMutations = append(directoryMetadataMutations, syncDirectoryMetadataMutation{path: currentMetadata.Path, delete: true})
			}
		}
		for _, selected := range selectedAny {
			if !selected {
				return domain.NewError(domain.CodeNotFound, "Workspace restore path is absent from both revisions")
			}
		}
		if err := applyWorkspaceSyncTreeMutations(ctx, tx, workspaceID, currentRevision, currentFileCount, currentBytes, quotaBytes,
			fileMutations, directoryMutations, targetDirectoryMetadata, directoryMetadataMutations); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT revision FROM workspaces WHERE id=$1`, workspaceID).Scan(&result.Revision); err != nil {
			return fmt.Errorf("read Workspace revision after restore: %w", err)
		}
		if result.Revision != currentRevision {
			if err := enqueueWorkspaceCurrentMaterialization(ctx, tx, workspaceID, result.Revision); err != nil {
				return err
			}
		}
		updated, err := tx.Exec(ctx, `UPDATE idempotency_keys SET related_id=$4
WHERE owner_id=$1 AND operation=$2 AND key=$3 AND request_hash=$5 AND resource_id=$6`,
			idempotency.OwnerID, idempotency.Operation, idempotency.Key, strconv.FormatInt(result.Revision, 10), idempotency.RequestHash, workspaceID)
		if err != nil {
			return fmt.Errorf("record Workspace restore result: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace restore receipt changed")
		}
		return nil
	})
	if err != nil {
		return app.WorkspaceRestoreResult{}, err
	}
	return result, nil
}

func loadWorkspaceRestoreSnapshot(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64) ([]workspace.File, []string, []workspace.Directory, error) {
	queries := sqlcgen.New(tx)
	if _, err := queries.GetWorkspaceRevision(ctx, sqlcgen.GetWorkspaceRevisionParams{WorkspaceID: workspaceID, Revision: revision}); err != nil {
		return nil, nil, nil, storeError("read Workspace restore revision", "workspace revision", err)
	}
	rows, err := queries.ListWorkspaceRevisionFiles(ctx, sqlcgen.ListWorkspaceRevisionFilesParams{WorkspaceID: workspaceID, ValidFromRevision: revision})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list Workspace restore files: %w", err)
	}
	files := make([]workspace.File, 0, len(rows))
	for _, row := range rows {
		file, err := workspaceFileFromRow(row.FilePath, row.SizeBytes, row.Sha256, row.Mode, row.Kind, row.SymlinkTarget, row.ModTimeNs, row.HardlinkTo, row.Chunks, row.Xattrs, row.Acl, row.MetadataKnown)
		if err != nil {
			return nil, nil, nil, err
		}
		files = append(files, file)
	}
	directories, err := queries.ListWorkspaceRevisionEmptyDirectories(ctx, sqlcgen.ListWorkspaceRevisionEmptyDirectoriesParams{WorkspaceID: workspaceID, ValidFromRevision: revision})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list Workspace restore directories: %w", err)
	}
	if directories == nil {
		directories = []string{}
	}
	items := make([]workspaceManifestItem, 0, len(files)+len(directories))
	for index := range files {
		file := files[index]
		items = append(items, workspaceManifestItem{path: file.Path, file: &file})
	}
	for _, directory := range directories {
		items = append(items, workspaceManifestItem{path: directory})
	}
	directoryPaths := workspaceDirectoryAncestors(items)
	directoryPaths = append(directoryPaths, ".")
	directoryMetadata, err := loadWorkspaceRevisionDirectories(ctx, tx, workspaceID, revision, uniqueWorkspacePaths(directoryPaths))
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err := workspace.ManifestTreeWithMetadataSHA256(files, directories, directoryMetadata); err != nil {
		return nil, nil, nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace restore revision contains an invalid manifest")
	}
	var storedFileCount, storedBytes, storedDirectories int64
	if err := tx.QueryRow(ctx, `SELECT file_count,total_bytes,directory_count FROM workspace_revisions WHERE workspace_id=$1 AND revision=$2`, workspaceID, revision).
		Scan(&storedFileCount, &storedBytes, &storedDirectories); err != nil {
		return nil, nil, nil, storeError("read Workspace restore revision totals", "workspace revision", err)
	}
	var fileBytes int64
	for _, file := range files {
		fileBytes += file.SizeBytes
	}
	if storedFileCount != int64(len(files)) || storedBytes != fileBytes || storedDirectories != int64(len(directories)) {
		return nil, nil, nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace restore revision totals do not match its manifest")
	}
	return files, directories, directoryMetadata, nil
}

func workspaceFilesByFoldedPath(files []workspace.File) map[string]workspace.File {
	result := make(map[string]workspace.File, len(files))
	for _, file := range files {
		result[strings.ToLower(file.Path)] = file
	}
	return result
}

func workspaceDirectoriesByFoldedPath(directories []string) map[string]string {
	result := make(map[string]string, len(directories))
	for _, directory := range directories {
		result[strings.ToLower(directory)] = directory
	}
	return result
}

func workspaceRestoreEntryPath(currentFile workspace.File, hasCurrentFile bool, targetFile workspace.File, hasTargetFile bool,
	currentDirectory string, hasCurrentDirectory bool, targetDirectory string, hasTargetDirectory bool,
	currentMetadata workspace.Directory, hasCurrentMetadata bool, targetMetadata workspace.Directory, hasTargetMetadata bool) string {
	switch {
	case hasTargetFile:
		return targetFile.Path
	case hasCurrentFile:
		return currentFile.Path
	case hasTargetDirectory:
		return targetDirectory
	case hasCurrentDirectory:
		return currentDirectory
	case hasTargetMetadata:
		return targetMetadata.Path
	case hasCurrentMetadata:
		return currentMetadata.Path
	default:
		return ""
	}
}

func workspaceRestorePathSelected(path, root string) bool {
	path, root = strings.ToLower(path), strings.ToLower(root)
	return path == root || strings.HasPrefix(path, root+"/")
}

func workspaceRestoreEntriesEqual(currentFile workspace.File, hasCurrentFile bool, targetFile workspace.File, hasTargetFile bool,
	currentDirectory string, hasCurrentDirectory bool, targetDirectory string, hasTargetDirectory bool,
	currentMetadata workspace.Directory, hasCurrentMetadata bool, targetMetadata workspace.Directory, hasTargetMetadata bool) bool {
	if hasCurrentFile != hasTargetFile || hasCurrentDirectory != hasTargetDirectory || hasCurrentMetadata != hasTargetMetadata {
		return false
	}
	if hasCurrentFile && !sameWorkspaceFile(currentFile, targetFile) {
		return false
	}
	if hasCurrentDirectory && currentDirectory != targetDirectory {
		return false
	}
	return !hasCurrentMetadata || workspace.SameDirectoryMetadata(currentMetadata, targetMetadata)
}

func workspaceDirectoryMetadataByFoldedPath(directories []workspace.Directory) map[string]workspace.Directory {
	result := make(map[string]workspace.Directory, len(directories))
	for _, directory := range directories {
		result[strings.ToLower(directory.Path)] = directory
	}
	return result
}
