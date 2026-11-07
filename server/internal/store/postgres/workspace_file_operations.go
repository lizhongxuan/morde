package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func (s *Store) ApplyWorkspaceFileOperation(ctx context.Context, ownerID, workspaceID string, operation app.WorkspaceFileOperationCommand,
	idempotency app.IdempotencyRecord) (app.WorkspaceFileOperationResult, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || !validWorkspaceFileOperationForStore(operation) ||
		idempotency.OwnerID != ownerID || idempotency.Operation != "workspace.file_operation" || idempotency.ResourceID != workspaceID {
		return app.WorkspaceFileOperationResult{}, domain.InvalidArgument("file_operation", "Workspace file operation is invalid")
	}
	if err := validateIdempotency(idempotency); err != nil {
		return app.WorkspaceFileOperationResult{}, err
	}
	result := app.WorkspaceFileOperationResult{
		WorkspaceID: workspaceID, Action: operation.Action, SourcePath: operation.SourcePath, TargetPath: operation.TargetPath,
		ExpectedCurrentRevision: operation.ExpectedCurrentRevision,
	}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceForSyncMutation(ctx, tx, workspaceID); err != nil {
			return err
		}
		var storedOwner, state string
		var readOnly bool
		var currentRevision, currentBytes, quotaBytes int64
		var deleted bool
		if err := tx.QueryRow(ctx, `SELECT owner_id,state,revision,used_bytes,quota_bytes,deleted_at IS NOT NULL,is_read_only
FROM workspaces WHERE id=$1`, workspaceID).Scan(&storedOwner, &state, &currentRevision, &currentBytes, &quotaBytes, &deleted, &readOnly); err != nil {
			return storeError("read Workspace before file operation", "workspace", err)
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
				return domain.NewError(domain.CodeIdempotencyConflict, "Workspace file operation key is bound to another Workspace")
			}
			revision, err := strconv.ParseInt(stored.RelatedID, 10, 64)
			if err != nil || revision < 0 {
				return domain.NewError(domain.CodeInternal, "Workspace file operation receipt is invalid")
			}
			result.Revision, result.Replayed = revision, true
			return nil
		}
		if state != string(workspace.StateReady) || currentRevision != operation.ExpectedCurrentRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed; refresh before editing")
		}
		var desiredRevision, appliedRevision int64
		if err := tx.QueryRow(ctx, `SELECT desired_revision,applied_revision FROM workspace_current_materializations WHERE workspace_id=$1`, workspaceID).
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
EXISTS (SELECT 1 FROM tasks WHERE workspace_id=$1 AND status IN ('queued','running','waiting_user','pausing','cancelling')) OR
EXISTS (SELECT 1 FROM task_operations WHERE workspace_id=$1 AND status IN ('prepared','dispatched','unknown')) OR
EXISTS (SELECT 1 FROM task_mcp_connector_operations o JOIN tasks t ON t.id=o.task_id
  WHERE t.workspace_id=$1 AND o.status IN ('prepared','dispatched','unknown')) OR
EXISTS (SELECT 1 FROM task_workspace_publish_receipts WHERE workspace_id=$1 AND state='pending') OR
EXISTS (SELECT 1 FROM task_hub_operations o WHERE o.workspace_id=$1 AND o.action='file.put'
  AND o.status IN ('prepared','dispatched','unknown') AND o.sync_stage IN ('pending','local_applied','sync_pending'))`, workspaceID).Scan(&blocked); err != nil {
			return fmt.Errorf("check Workspace writers before file operation: %w", err)
		}
		if blocked {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has an active Host binding or in-flight write; stop and settle it before editing")
		}
		if err := ensureNoWorkspaceSyncReceive(ctx, tx, workspaceID, ""); err != nil {
			return err
		}

		currentFiles, currentDirectories, _, err := loadWorkspaceRestoreSnapshot(ctx, tx, workspaceID, currentRevision)
		if err != nil {
			return err
		}
		var fileCount int64
		if err := tx.QueryRow(ctx, `SELECT file_count FROM workspace_revisions WHERE workspace_id=$1 AND revision=$2`, workspaceID, currentRevision).Scan(&fileCount); err != nil {
			return storeError("read Workspace file count before operation", "workspace revision", err)
		}
		var manifestBytes int64
		for _, file := range currentFiles {
			manifestBytes += file.SizeBytes
		}
		if fileCount != int64(len(currentFiles)) || manifestBytes != currentBytes {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace current totals do not match its committed manifest")
		}
		if workspaceFileOperationTouchesSensitiveSnapshot(operation, currentFiles, currentDirectories) {
			return domain.NewError(domain.CodeForbidden,
				"file edits are disabled for credential paths (.ssh, .gnupg, .aws, .azure, .kube, .docker, .config/gcloud, .config/gh, .netrc, .git-credentials, and .env*); review these files on the Host")
		}
		fileMutations, directoryMutations, err := workspaceFileOperationMutations(operation, currentFiles, currentDirectories)
		if err != nil {
			return err
		}
		if err := applyWorkspaceSyncTreeMutations(ctx, tx, workspaceID, currentRevision, fileCount, currentBytes, quotaBytes, fileMutations, directoryMutations, nil, nil); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT revision FROM workspaces WHERE id=$1`, workspaceID).Scan(&result.Revision); err != nil {
			return fmt.Errorf("read Workspace revision after file operation: %w", err)
		}
		if result.Revision == currentRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace file operation did not change the committed tree")
		}
		if err := enqueueWorkspaceCurrentMaterialization(ctx, tx, workspaceID, result.Revision); err != nil {
			return err
		}
		updated, err := tx.Exec(ctx, `UPDATE idempotency_keys SET related_id=$4
WHERE owner_id=$1 AND operation=$2 AND key=$3 AND request_hash=$5 AND resource_id=$6`,
			idempotency.OwnerID, idempotency.Operation, idempotency.Key, strconv.FormatInt(result.Revision, 10), idempotency.RequestHash, workspaceID)
		if err != nil {
			return fmt.Errorf("record Workspace file operation result: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace file operation receipt changed")
		}
		return nil
	})
	if err != nil {
		return app.WorkspaceFileOperationResult{}, err
	}
	return result, nil
}

func workspaceFileOperationTouchesSensitiveSnapshot(operation app.WorkspaceFileOperationCommand, files []workspace.File, directories []string) bool {
	if app.WorkspaceFileOperationPathIsSensitive(operation.SourcePath) ||
		operation.TargetPath != "" && app.WorkspaceFileOperationPathIsSensitive(operation.TargetPath) {
		return true
	}
	source := strings.ToLower(operation.SourcePath)
	selected := func(pathValue string) bool {
		pathValue = strings.ToLower(pathValue)
		return pathValue == source || strings.HasPrefix(pathValue, source+"/")
	}
	for _, file := range files {
		if selected(file.Path) && app.WorkspaceFileOperationPathIsSensitive(file.Path) {
			return true
		}
	}
	for _, directory := range directories {
		if selected(directory) && app.WorkspaceFileOperationPathIsSensitive(directory) {
			return true
		}
	}
	return false
}

func workspaceFileOperationMutations(operation app.WorkspaceFileOperationCommand, files []workspace.File, directories []string) ([]syncMutation, []syncDirectoryMutation, error) {
	source := strings.ToLower(operation.SourcePath)
	selected := func(value string) bool {
		value = strings.ToLower(value)
		return value == source || strings.HasPrefix(value, source+"/")
	}
	var sourceFound bool
	for _, file := range files {
		if selected(file.Path) {
			sourceFound = true
		}
	}
	for _, directory := range directories {
		if selected(directory) {
			sourceFound = true
		}
	}
	if !sourceFound {
		return nil, nil, domain.NewError(domain.CodeNotFound, "Workspace source path not found")
	}
	if operation.Action != app.WorkspaceFileDelete {
		if err := workspaceFileOperationDestinationFree(operation, files, directories); err != nil {
			return nil, nil, err
		}
	}
	fileMutations := make([]syncMutation, 0)
	directoryMutations := make([]syncDirectoryMutation, 0)
	mutate := func(pathValue string) (string, bool) {
		if operation.Action == app.WorkspaceFileDelete {
			return "", true
		}
		pathParts, sourceParts := strings.Split(pathValue, "/"), strings.Split(operation.SourcePath, "/")
		if len(pathParts) < len(sourceParts) {
			return "", false
		}
		for i := range sourceParts {
			if !strings.EqualFold(pathParts[i], sourceParts[i]) {
				return "", false
			}
		}
		suffix := strings.Join(pathParts[len(sourceParts):], "/")
		if suffix == "" {
			return operation.TargetPath, true
		}
		return operation.TargetPath + "/" + suffix, true
	}
	for _, file := range files {
		if !selected(file.Path) {
			continue
		}
		newPath, ok := mutate(file.Path)
		if !ok {
			return nil, nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace source path changed case and cannot be moved safely")
		}
		if newPath == "" {
			fileMutations = append(fileMutations, syncMutation{path: file.Path})
			continue
		}
		copy := file
		copy.Path = newPath
		if err := app.ValidateWorkspaceFileOperationMove(file, newPath); err != nil {
			return nil, nil, err
		}
		if strings.EqualFold(file.Path, newPath) {
			fileMutations = append(fileMutations, syncMutation{path: newPath, file: &copy})
		} else {
			fileMutations = append(fileMutations, syncMutation{path: file.Path}, syncMutation{path: newPath, file: &copy})
		}
	}
	for _, directory := range directories {
		if !selected(directory) {
			continue
		}
		newPath, ok := mutate(directory)
		if !ok {
			return nil, nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace source path changed case and cannot be moved safely")
		}
		if newPath == "" {
			directoryMutations = append(directoryMutations, syncDirectoryMutation{path: directory, delete: true})
			continue
		}
		if strings.EqualFold(directory, newPath) {
			directoryMutations = append(directoryMutations, syncDirectoryMutation{path: newPath})
		} else {
			directoryMutations = append(directoryMutations, syncDirectoryMutation{path: directory, delete: true}, syncDirectoryMutation{path: newPath})
		}
	}
	return fileMutations, directoryMutations, nil
}

func workspaceFileOperationDestinationFree(operation app.WorkspaceFileOperationCommand, files []workspace.File, directories []string) error {
	source := strings.ToLower(operation.SourcePath)
	target := strings.ToLower(operation.TargetPath)
	paths := make([]string, 0, len(files)+len(directories))
	filePaths := make(map[string]struct{}, len(files))
	for _, file := range files {
		folded := strings.ToLower(file.Path)
		paths = append(paths, folded)
		filePaths[folded] = struct{}{}
	}
	for _, directory := range directories {
		paths = append(paths, strings.ToLower(directory))
	}
	for _, folded := range paths {
		if folded == source || strings.HasPrefix(folded, source+"/") {
			continue
		}
		if folded == target || strings.HasPrefix(folded, target+"/") {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace destination already exists")
		}
		if strings.HasPrefix(target, folded+"/") {
			if _, isFile := filePaths[folded]; isFile {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace destination is inside a file")
			}
		}
	}
	return nil
}

func validWorkspaceFileOperationForStore(value app.WorkspaceFileOperationCommand) bool {
	if !value.WriteEnabled || value.ExpectedCurrentRevision < 0 || workspace.ValidateRelativePath(value.SourcePath) != nil {
		return false
	}
	if value.Action == app.WorkspaceFileDelete {
		return value.TargetPath == ""
	}
	if value.Action != app.WorkspaceFileRename && value.Action != app.WorkspaceFileMove || workspace.ValidateRelativePath(value.TargetPath) != nil || value.SourcePath == value.TargetPath {
		return false
	}
	source, target := strings.ToLower(value.SourcePath), strings.ToLower(value.TargetPath)
	if source == target {
		return value.SourcePath != value.TargetPath
	}
	return !strings.HasPrefix(source, target+"/") && !strings.HasPrefix(target, source+"/")
}
