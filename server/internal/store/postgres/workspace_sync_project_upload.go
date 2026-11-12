package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) GetWorkspaceSyncProjectUploadBaseline(ctx context.Context, principal identity.Principal,
	workspaceID, folderID, sourceBindingID string) (*workspace.SyncSession, error) {
	if principal.Kind != identity.KindDevice || !domain.ValidClientInstallationID(principal.ClientInstallationID) ||
		!domain.ValidUserID(principal.UserID) || !domain.ValidWorkspaceID(workspaceID) ||
		!workspace.ValidSyncFolderID(folderID) || !workspace.ValidProjectSourceBindingID(sourceBindingID) {
		return nil, domain.NewError(domain.CodeForbidden, "project upload requires an installation-bound source")
	}
	var baseline *workspace.SyncSession
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID, installationID, sourceWorkspaceID, sourceFolderID, hostID, sourceState string
		var bindingVersion int64
		var lastSessionID pgtype.Text
		var lastTargetRevision pgtype.Int8
		var lastDigest []byte
		err := tx.QueryRow(ctx, `SELECT owner_id,client_installation_id,workspace_id,folder_id,host_id,binding_version,
state,last_applied_session_id,last_applied_target_revision,last_applied_manifest_sha256
FROM workspace_sync_project_sources WHERE source_binding_id=$1`, sourceBindingID).Scan(
			&ownerID, &installationID, &sourceWorkspaceID, &sourceFolderID, &hostID, &bindingVersion,
			&sourceState, &lastSessionID, &lastTargetRevision, &lastDigest,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read Workspace project source binding: %w", err)
		}
		if ownerID != principal.UserID || installationID != principal.ClientInstallationID ||
			sourceWorkspaceID != workspaceID || sourceFolderID != folderID || sourceState == "revoked" {
			return domain.NewError(domain.CodeWorkspaceConflict, "project source identity is already bound to another installation or target")
		}
		if sourceState != "pending" && sourceState != "active" {
			return domain.NewError(domain.CodeWorkspaceUnavailable, "project source binding is unavailable")
		}
		var currentBindingVersion int64
		var currentBindingState string
		var hostState string
		var hostDeleted bool
		if err := tx.QueryRow(ctx, `SELECT b.binding_version,b.state,h.state,h.deleted_at IS NOT NULL
FROM workspace_bindings b JOIN hosts h ON h.id=b.host_id AND h.owner_id=b.owner_id
WHERE b.owner_id=$1 AND b.workspace_id=$2 AND b.host_id=$3`, ownerID, workspaceID, hostID).
			Scan(&currentBindingVersion, &currentBindingState, &hostState, &hostDeleted); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeWorkspaceUnavailable, "project source Host binding is unavailable")
			}
			return fmt.Errorf("verify Workspace project source binding: %w", err)
		}
		if currentBindingVersion != bindingVersion || currentBindingState != string(workspace.BindingActive) ||
			hostDeleted || hostState == string(host.StateDeleting) || hostState == string(host.StateDeleted) {
			return domain.NewError(domain.CodeWorkspaceConflict, "project source Host binding changed; explicit rebind is required")
		}
		if !lastSessionID.Valid {
			if lastTargetRevision.Valid || len(lastDigest) != 0 {
				return domain.NewError(domain.CodeInternal, "stored project source baseline is incomplete")
			}
			return nil
		}
		if sourceState != "active" || !lastTargetRevision.Valid || len(lastDigest) != 32 {
			return domain.NewError(domain.CodeInternal, "stored project source baseline is invalid")
		}
		q := sqlcgen.New(tx)
		row, err := q.GetWorkspaceSyncSession(ctx, sqlcgen.GetWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: lastSessionID.String})
		if err != nil {
			return storeError("read Host-confirmed project source snapshot", "workspace sync session", err)
		}
		value, err := syncSessionDomain(ctx, tx, q, row)
		if err != nil {
			return err
		}
		value.SourceBindingID = sourceBindingID
		if row.OwnerID != ownerID || !row.FolderID.Valid || row.FolderID.String != folderID || row.TargetRevision != lastTargetRevision.Int64 ||
			value.State != workspace.SyncCompleted || value.Direction != workspace.SyncUpload || value.Policy != workspace.SyncCompareMerge {
			return domain.NewError(domain.CodeInternal, "Host-confirmed project source snapshot does not match its receipt")
		}
		if len(value.Scope) != 1 {
			return domain.NewError(domain.CodeInternal, "Host-confirmed project source scope is invalid")
		}
		digest, err := workspace.ProjectManifestSHA256(value.Scope[0], value.Files, value.EmptyDirectories, value.Directories)
		if err != nil {
			return domain.NewError(domain.CodeInternal, "Host-confirmed project source manifest is invalid")
		}
		decodedDigest, err := hex.DecodeString(digest)
		if err != nil || !bytes.Equal(decodedDigest, lastDigest) {
			return domain.NewError(domain.CodeInternal, "Host-confirmed project source manifest digest is invalid")
		}
		baseline = &value
		return nil
	})
	if err != nil {
		return nil, err
	}
	return baseline, nil
}

func (s *Store) ListPendingWorkspaceSyncProjectUploadApplications(ctx context.Context, hostID, workspaceID string,
	bindingVersion int64) ([]workspace.PendingProjectUploadApplication, error) {
	if !domain.ValidHostID(hostID) || !domain.ValidWorkspaceID(workspaceID) || bindingVersion < 1 {
		return nil, domain.InvalidArgument("project_upload_application", "Host binding identity is invalid")
	}
	items := make([]workspace.PendingProjectUploadApplication, 0)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT a.session_id,a.source_binding_id,f.relative_path,a.target_revision,a.manifest_sha256
FROM workspace_sync_project_upload_applications a
JOIN workspace_sync_folders f ON f.workspace_id=a.workspace_id AND f.owner_id=a.owner_id AND f.folder_id=a.folder_id
WHERE a.host_id=$1 AND a.workspace_id=$2 AND a.workspace_binding_version=$3 AND a.state='pending_apply'
  AND f.folder_kind='project' AND f.relative_path='project/'||f.project_name
ORDER BY a.created_at,a.session_id LIMIT 1000`, hostID, workspaceID, bindingVersion)
		if err != nil {
			return fmt.Errorf("list pending Host project upload applications: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var item workspace.PendingProjectUploadApplication
			var digest []byte
			if err := rows.Scan(&item.SessionID, &item.SourceBindingID, &item.ProjectPath, &item.TargetRevision, &digest); err != nil {
				return fmt.Errorf("scan pending Host project upload application: %w", err)
			}
			if !workspace.ValidSessionID(item.SessionID) || !workspace.ValidProjectSourceBindingID(item.SourceBindingID) ||
				workspace.ValidateRelativePath(item.ProjectPath) != nil || !strings.HasPrefix(item.ProjectPath, "project/") ||
				item.TargetRevision < 0 || len(digest) != 32 {
				return domain.NewError(domain.CodeInternal, "pending Host project upload application is inconsistent")
			}
			item.ManifestSHA256 = hex.EncodeToString(digest)
			items = append(items, item)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) RecordWorkspaceSyncProjectUploadApplication(ctx context.Context, principal identity.Principal,
	workspaceID string, bindingVersion int64, sessionID string, report workspace.HostProjectUploadApplicationReport) (workspace.SyncHostApplication, error) {
	if principal.Kind != identity.KindHostReplica || !domain.ValidHostID(principal.HostScope) ||
		!domain.ValidUserID(principal.UserID) || principal.HostGeneration < 1 || !domain.ValidWorkspaceID(workspaceID) ||
		bindingVersion < 1 || !workspace.ValidSessionID(sessionID) || workspace.ValidateHostProjectUploadApplicationReport(report) != nil {
		return workspace.SyncHostApplication{}, domain.NewError(domain.CodeUnauthenticated, "Host project upload receipt identity is invalid")
	}
	var result workspace.SyncHostApplication
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var hostOwner, hostState string
		var generation int64
		var deletedAt pgtype.Timestamptz
		if err := tx.QueryRow(ctx, `SELECT owner_id,state,runtime_generation,deleted_at FROM hosts WHERE id=$1 FOR UPDATE`, principal.HostScope).
			Scan(&hostOwner, &hostState, &generation, &deletedAt); err != nil {
			return storeError("lock Host for project upload receipt", "host", err)
		}
		if hostOwner != principal.UserID || generation != principal.HostGeneration || hostState != string(host.StateRunning) || deletedAt.Valid {
			return domain.NewError(domain.CodeUnauthenticated, "Host project upload receipt generation or authorization changed")
		}
		var workspaceOwner, workspaceState, bindingOwner, bindingState, bindingSyncState string
		var workspaceRevision, desiredRevision, appliedRevision, currentBindingVersion int64
		if err := tx.QueryRow(ctx, `SELECT w.owner_id,w.state,w.revision,b.owner_id,b.binding_version,b.state,
b.desired_revision,b.applied_revision,b.sync_state
FROM workspaces w JOIN workspace_bindings b ON b.workspace_id=w.id
WHERE w.id=$1 AND b.host_id=$2
FOR UPDATE OF w,b`, workspaceID, principal.HostScope).Scan(
			&workspaceOwner, &workspaceState, &workspaceRevision, &bindingOwner, &currentBindingVersion, &bindingState,
			&desiredRevision, &appliedRevision, &bindingSyncState,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "Host workspace binding not found")
			}
			return fmt.Errorf("lock Host workspace binding for project upload receipt: %w", err)
		}
		if workspaceOwner != principal.UserID || bindingOwner != principal.UserID || workspaceState != string(workspace.StateReady) ||
			bindingState != string(workspace.BindingActive) || currentBindingVersion != bindingVersion ||
			principal.ReplicaWorkspaceID != workspaceID || principal.BindingVersion != bindingVersion {
			return domain.NewError(domain.CodeWorkspaceConflict, "Host workspace binding changed before project upload receipt")
		}
		if report.Status == "applied" && (bindingSyncState != string(workspace.BindingSyncHealthy) ||
			report.AppliedRevision == nil || *report.AppliedRevision > appliedRevision || *report.AppliedRevision > workspaceRevision ||
			*report.AppliedRevision > desiredRevision) {
			return domain.NewError(domain.CodeWorkspaceConflict, "Host has not durably applied the reported project upload revision")
		}

		q := sqlcgen.New(tx)
		sessionRow, err := q.LockWorkspaceSyncSession(ctx, sqlcgen.LockWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if err != nil {
			return storeError("lock project upload session for Host receipt", "workspace sync session", err)
		}
		if sessionRow.OwnerID != principal.UserID || !sessionRow.FolderID.Valid || !sessionRow.SourceBindingID.Valid ||
			sessionRow.Direction != string(workspace.SyncUpload) || sessionRow.Policy != string(workspace.SyncCompareMerge) ||
			sessionRow.State != string(workspace.SyncCompleted) || sessionRow.TargetRevision != report.TargetRevision ||
			sessionRow.SourceBindingID.String != report.SourceBindingID {
			return domain.NewError(domain.CodeWorkspaceConflict, "Host receipt does not match a completed project upload session")
		}
		var folder workspace.SyncFolder
		folder, err = lockWorkspaceSyncProjectUploadFolder(ctx, tx, principal.UserID, workspaceID, sessionRow.FolderID.String)
		if err != nil {
			return err
		}
		if len(sessionRow.Scope) != 1 || sessionRow.Scope[0] != folder.RelativePath {
			return domain.NewError(domain.CodeWorkspaceConflict, "Host receipt project scope does not match its registered folder")
		}

		var appOwner, folderID, sourceBindingID, hostID, state string
		var sourceBindingVersion, appBindingVersion, targetRevision int64
		var baselineSessionID pgtype.Text
		var manifestSHA256, receiptSHA256 []byte
		err = tx.QueryRow(ctx, `SELECT owner_id,folder_id,source_binding_id,source_binding_version,source_baseline_session_id,
host_id,workspace_binding_version,target_revision,state,manifest_sha256,receipt_sha256
FROM workspace_sync_project_upload_applications WHERE workspace_id=$1 AND session_id=$2 FOR UPDATE`, workspaceID, sessionID).Scan(
			&appOwner, &folderID, &sourceBindingID, &sourceBindingVersion, &baselineSessionID,
			&hostID, &appBindingVersion, &targetRevision, &state, &manifestSHA256, &receiptSHA256,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "Host project upload application not found")
			}
			return fmt.Errorf("lock Host project upload application: %w", err)
		}
		reportManifestSHA256, decodeErr := hex.DecodeString(report.ManifestSHA256)
		if decodeErr != nil || appOwner != principal.UserID || folderID != folder.FolderID || sourceBindingID != report.SourceBindingID ||
			hostID != principal.HostScope || appBindingVersion != bindingVersion || targetRevision != report.TargetRevision ||
			baselineSessionID.Valid != sessionRow.SourceBaselineSessionID.Valid ||
			baselineSessionID.Valid && baselineSessionID.String != sessionRow.SourceBaselineSessionID.String ||
			len(manifestSHA256) != 32 || !bytes.Equal(manifestSHA256, reportManifestSHA256) {
			return domain.NewError(domain.CodeWorkspaceConflict, "Host receipt does not match its pending project application")
		}
		session, err := syncSessionDomain(ctx, tx, q, sessionRow)
		if err != nil {
			return err
		}
		digest, err := workspace.ProjectManifestSHA256(folder.RelativePath, session.Files, session.EmptyDirectories, session.Directories)
		if err != nil {
			return domain.NewError(domain.CodeInternal, "stored project upload manifest digest does not match its receipt")
		}
		storedDigest, decodeErr := hex.DecodeString(digest)
		if decodeErr != nil || !bytes.Equal(manifestSHA256, storedDigest) {
			return domain.NewError(domain.CodeInternal, "stored project upload manifest digest does not match its receipt")
		}
		receiptHash, err := hostProjectUploadReceiptHash(report)
		if err != nil {
			return fmt.Errorf("hash Host project upload receipt: %w", err)
		}
		if len(receiptSHA256) != 0 {
			if bytes.Equal(receiptSHA256, receiptHash) {
				if session.HostApplication == nil {
					return domain.NewError(domain.CodeInternal, "stored Host project upload application projection is missing")
				}
				result = *session.HostApplication
				return nil
			}
			return domain.NewError(domain.CodeWorkspaceConflict, "Host project upload application already has a different receipt")
		}
		if state != "pending_apply" {
			return domain.NewError(domain.CodeWorkspaceConflict, "Host project upload application is no longer pending")
		}
		var sourceState string
		var sourceBaselineSessionID pgtype.Text
		var lastAppliedTargetRevision pgtype.Int8
		var lastAppliedManifestSHA256 []byte
		err = tx.QueryRow(ctx, `SELECT state,last_applied_session_id,last_applied_target_revision,last_applied_manifest_sha256
FROM workspace_sync_project_sources WHERE source_binding_id=$1 AND owner_id=$2 AND workspace_id=$3 AND folder_id=$4
  AND host_id=$5 AND binding_version=$6 FOR UPDATE`, sourceBindingID, principal.UserID, workspaceID, folderID,
			hostID, sourceBindingVersion).Scan(&sourceState, &sourceBaselineSessionID, &lastAppliedTargetRevision, &lastAppliedManifestSHA256)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeWorkspaceConflict, "Host project upload source binding changed")
		}
		if err != nil {
			return fmt.Errorf("lock Host project upload source baseline: %w", err)
		}
		if sourceState != "pending" && sourceState != "active" ||
			sourceBaselineSessionID.Valid != baselineSessionID.Valid ||
			sourceBaselineSessionID.Valid && sourceBaselineSessionID.String != baselineSessionID.String {
			return domain.NewError(domain.CodeWorkspaceConflict, "Host project upload source baseline changed")
		}

		effectiveStatus := report.Status
		impacts := append([]workspace.SyncHostApplicationImpact{}, report.Impacts...)
		if report.Status == "applied" {
			if lastAppliedTargetRevision.Valid && (targetRevision < lastAppliedTargetRevision.Int64 ||
				targetRevision == lastAppliedTargetRevision.Int64 && !bytes.Equal(lastAppliedManifestSHA256, manifestSHA256)) {
				effectiveStatus = "conflict"
				impacts = []workspace.SyncHostApplicationImpact{{Path: ".", State: "conflict", Reason: "source_baseline_changed"}}
			} else if *report.AppliedRevision > targetRevision {
				lineageImpacts, err := verifyWorkspaceSyncProjectApplicationLineage(ctx, tx, workspaceID, folder.RelativePath,
					targetRevision, *report.AppliedRevision, session)
				if err != nil {
					return err
				}
				if len(lineageImpacts) > 0 {
					effectiveStatus = "conflict"
					for _, impact := range lineageImpacts {
						if impact.State == "unknown" {
							effectiveStatus = "unknown"
							break
						}
					}
					impacts = lineageImpacts
				}
			}
		}
		encodedImpacts, err := json.Marshal(impacts)
		if err != nil {
			return fmt.Errorf("encode Host project upload impacts: %w", err)
		}
		if effectiveStatus == "applied" && sourceState == "pending" {
			updated, err := tx.Exec(ctx, `UPDATE workspace_sync_project_sources SET state='active',updated_at=now()
WHERE source_binding_id=$1 AND state='pending'`, sourceBindingID)
			if err != nil {
				return fmt.Errorf("activate verified project source binding: %w", err)
			}
			if updated.RowsAffected() != 1 {
				return domain.NewError(domain.CodeWorkspaceConflict, "Host project upload source binding changed")
			}
		}
		var appliedRevisionValue pgtype.Int8
		if report.AppliedRevision != nil {
			appliedRevisionValue = pgtype.Int8{Int64: *report.AppliedRevision, Valid: true}
		}
		var reportSyncState pgtype.Text
		if report.SyncState != nil {
			reportSyncState = pgtype.Text{String: string(*report.SyncState), Valid: true}
		}
		updated, err := tx.Exec(ctx, `UPDATE workspace_sync_project_upload_applications
SET state=$3,runtime_generation=$4,applied_revision=$5,sync_state=$6,impacts=$7,receipt_sha256=$8,updated_at=now()
WHERE workspace_id=$1 AND session_id=$2 AND state='pending_apply' AND receipt_sha256 IS NULL`,
			workspaceID, sessionID, effectiveStatus, principal.HostGeneration, appliedRevisionValue, reportSyncState, encodedImpacts, receiptHash)
		if err != nil {
			return fmt.Errorf("persist Host project upload application receipt: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeWorkspaceConflict, "Host project upload application changed before receipt")
		}
		if effectiveStatus == "applied" && (!lastAppliedTargetRevision.Valid || targetRevision > lastAppliedTargetRevision.Int64) {
			updated, err := tx.Exec(ctx, `UPDATE workspace_sync_project_sources
SET last_applied_session_id=$7,last_applied_target_revision=$8,last_applied_manifest_sha256=$9,updated_at=now()
WHERE source_binding_id=$1 AND owner_id=$2 AND workspace_id=$3 AND folder_id=$4 AND host_id=$5 AND binding_version=$6
  AND state='active' AND last_applied_session_id IS NOT DISTINCT FROM $10`, sourceBindingID, principal.UserID,
				workspaceID, folderID, hostID, sourceBindingVersion, sessionID, targetRevision, manifestSHA256, baselineSessionID)
			if err != nil {
				return fmt.Errorf("advance Host-confirmed project source baseline: %w", err)
			}
			if updated.RowsAffected() != 1 {
				return domain.NewError(domain.CodeWorkspaceConflict, "Host project upload source baseline changed before receipt")
			}
		}
		finalRow, err := q.GetWorkspaceSyncSession(ctx, sqlcgen.GetWorkspaceSyncSessionParams{WorkspaceID: workspaceID, ID: sessionID})
		if err != nil {
			return fmt.Errorf("read Host-confirmed project upload session: %w", err)
		}
		finalSession, err := syncSessionDomain(ctx, tx, q, finalRow)
		if err != nil {
			return err
		}
		if finalSession.HostApplication == nil {
			return domain.NewError(domain.CodeInternal, "Host project upload receipt projection is missing")
		}
		result = *finalSession.HostApplication
		return nil
	})
	if err != nil {
		return workspace.SyncHostApplication{}, err
	}
	return result, nil
}

func hostProjectUploadReceiptHash(value workspace.HostProjectUploadApplicationReport) ([]byte, error) {
	canonical := value
	canonical.Impacts = append([]workspace.SyncHostApplicationImpact{}, value.Impacts...)
	sort.Slice(canonical.Impacts, func(i, j int) bool {
		if canonical.Impacts[i].Path != canonical.Impacts[j].Path {
			return canonical.Impacts[i].Path < canonical.Impacts[j].Path
		}
		if canonical.Impacts[i].State != canonical.Impacts[j].State {
			return canonical.Impacts[i].State < canonical.Impacts[j].State
		}
		return canonical.Impacts[i].Reason < canonical.Impacts[j].Reason
	})
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	return digest[:], nil
}

func verifyWorkspaceSyncProjectApplicationLineage(ctx context.Context, tx pgx.Tx, workspaceID, projectRoot string,
	targetRevision, appliedRevision int64, session workspace.SyncSession) ([]workspace.SyncHostApplicationImpact, error) {
	if targetRevision < 0 || appliedRevision <= targetRevision {
		return nil, domain.NewError(domain.CodeWorkspaceConflict, "Host project application revision lineage is invalid")
	}
	var revisionCount int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM workspace_revisions
WHERE workspace_id=$1 AND revision >= $2 AND revision <= $3`, workspaceID, targetRevision, appliedRevision).Scan(&revisionCount); err != nil {
		return nil, fmt.Errorf("verify Host project application revision lineage: %w", err)
	}
	if revisionCount < 1 || revisionCount-1 != appliedRevision-targetRevision {
		return []workspace.SyncHostApplicationImpact{{Path: ".", State: "unknown", Reason: "revision_lineage_unavailable"}}, nil
	}
	paths := syncMutationPaths(session.UploadFiles, session.UploadDeletedPaths)
	paths = append(paths, syncDirectoryMutationPaths(session.UploadEmptyDirectories, session.UploadDeletedDirectories)...)
	metadataPaths := make([]string, 0, len(session.UploadDirectories)+len(session.UploadDeletedDirectories))
	for _, directory := range session.UploadDirectories {
		metadataPaths = append(metadataPaths, directory.Path)
	}
	metadataPaths = append(metadataPaths, session.UploadDeletedDirectories...)
	paths = uniqueWorkspacePaths(append(paths, metadataPaths...))
	if len(paths) == 0 {
		return nil, nil
	}
	targetFiles, err := loadWorkspaceRevisionFiles(ctx, tx, workspaceID, targetRevision, paths)
	if err != nil {
		return nil, err
	}
	currentFiles, err := loadWorkspaceRevisionFiles(ctx, tx, workspaceID, appliedRevision, paths)
	if err != nil {
		return nil, err
	}
	targetDirs, err := loadWorkspaceRevisionEmptyDirectories(ctx, tx, workspaceID, targetRevision, paths)
	if err != nil {
		return nil, err
	}
	currentDirs, err := loadWorkspaceRevisionEmptyDirectories(ctx, tx, workspaceID, appliedRevision, paths)
	if err != nil {
		return nil, err
	}
	targetMetadata, err := loadWorkspaceRevisionDirectories(ctx, tx, workspaceID, targetRevision, metadataPaths)
	if err != nil {
		return nil, err
	}
	currentMetadata, err := loadWorkspaceRevisionDirectories(ctx, tx, workspaceID, appliedRevision, metadataPaths)
	if err != nil {
		return nil, err
	}
	targetFileMap, currentFileMap := fileMapByFoldedPath(targetFiles), fileMapByFoldedPath(currentFiles)
	targetDirMap, currentDirMap := directoryMapByFoldedPath(targetDirs), directoryMapByFoldedPath(currentDirs)
	targetMetadataMap, currentMetadataMap := workspaceDirectoryMapByFoldedPath(targetMetadata), workspaceDirectoryMapByFoldedPath(currentMetadata)
	impacts := make([]workspace.SyncHostApplicationImpact, 0)
	for _, path := range paths {
		key := strings.ToLower(path)
		targetFile, targetFileOK := targetFileMap[key]
		currentFile, currentFileOK := currentFileMap[key]
		targetDir, targetDirOK := targetDirMap[key]
		currentDir, currentDirOK := currentDirMap[key]
		if targetFileOK != currentFileOK || targetFileOK && !sameWorkspaceFile(targetFile, currentFile) ||
			targetDirOK != currentDirOK || targetDirOK && targetDir != currentDir {
			impacts = append(impacts, workspace.SyncHostApplicationImpact{Path: projectRelativeImpactPath(projectRoot, path), State: "conflict", Reason: "changed_after_target"})
			continue
		}
		if _, isMetadataPath := targetMetadataMap[key]; isMetadataPath {
			targetDirectory, targetOK := targetMetadataMap[key]
			currentDirectory, currentOK := currentMetadataMap[key]
			if targetOK != currentOK || targetOK && !workspace.SameDirectoryMetadata(targetDirectory, currentDirectory) {
				impacts = append(impacts, workspace.SyncHostApplicationImpact{Path: projectRelativeImpactPath(projectRoot, path), State: "conflict", Reason: "changed_after_target"})
			}
		} else if _, currentMetadataPath := currentMetadataMap[key]; currentMetadataPath {
			impacts = append(impacts, workspace.SyncHostApplicationImpact{Path: projectRelativeImpactPath(projectRoot, path), State: "conflict", Reason: "changed_after_target"})
		}
	}
	if len(impacts) > 100 {
		impacts = impacts[:100]
	}
	return impacts, nil
}

func projectRelativeImpactPath(root, value string) string {
	if value == root {
		return "."
	}
	return strings.TrimPrefix(value, root+"/")
}

func ensureWorkspaceSyncProjectSourceTx(ctx context.Context, tx pgx.Tx, session workspace.SyncSession,
	deviceAccess *workspaceSyncDeviceAccess, folder workspace.SyncFolder) error {
	if deviceAccess == nil || deviceAccess.installationID == "" ||
		!workspace.ValidProjectSourceBindingID(session.SourceBindingID) || session.FolderID != folder.FolderID {
		return domain.NewError(domain.CodeWorkspaceConflict, "project source binding or baseline is invalid")
	}
	var hostID string
	var bindingVersion int64
	err := tx.QueryRow(ctx, `SELECT r.host_id,b.binding_version
FROM workspace_sync_folder_registration_requests r
JOIN workspace_bindings b ON b.host_id=r.host_id AND b.workspace_id=r.workspace_id AND b.owner_id=r.owner_id AND b.state='active'
WHERE r.owner_id=$1 AND r.workspace_id=$2 AND r.folder_id=$3
ORDER BY r.created_at DESC LIMIT 1`, folder.OwnerID, folder.WorkspaceID, folder.FolderID).
		Scan(&hostID, &bindingVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeWorkspaceUnavailable, "project source Host binding is unavailable")
	}
	if err != nil {
		return fmt.Errorf("resolve current Host binding for project source: %w", err)
	}
	var ownerID, installationID, sourceWorkspaceID, sourceFolderID, savedHostID, sourceState string
	var savedBindingVersion int64
	var lastSessionID pgtype.Text
	err = tx.QueryRow(ctx, `SELECT owner_id,client_installation_id,workspace_id,folder_id,host_id,binding_version,state,last_applied_session_id
FROM workspace_sync_project_sources WHERE source_binding_id=$1 FOR UPDATE`, session.SourceBindingID).Scan(
		&ownerID, &installationID, &sourceWorkspaceID, &sourceFolderID, &savedHostID, &savedBindingVersion, &sourceState, &lastSessionID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		var anotherSource string
		err = tx.QueryRow(ctx, `SELECT source_binding_id FROM workspace_sync_project_sources
WHERE workspace_id=$1 AND folder_id=$2 AND state<>'revoked' FOR UPDATE`, folder.WorkspaceID, folder.FolderID).Scan(&anotherSource)
		if err == nil {
			return domain.NewError(domain.CodeWorkspaceConflict, "project folder is already bound to another local source")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check existing project source: %w", err)
		}
		if session.SourceBaselineSessionID != "" {
			return domain.NewError(domain.CodeWorkspaceConflict, "project source baseline changed before session creation")
		}
		_, err = tx.Exec(ctx, `INSERT INTO workspace_sync_project_sources
(source_binding_id,owner_id,client_installation_id,workspace_id,folder_id,host_id,binding_version,state)
VALUES ($1,$2,$3,$4,$5,$6,$7,'pending')`, session.SourceBindingID, folder.OwnerID, deviceAccess.installationID,
			folder.WorkspaceID, folder.FolderID, hostID, bindingVersion)
		if err != nil {
			return fmt.Errorf("bind project source to installation and Host target: %w", err)
		}
	} else {
		if err != nil {
			return fmt.Errorf("read project source binding for session creation: %w", err)
		}
		if ownerID != folder.OwnerID || installationID != deviceAccess.installationID || sourceWorkspaceID != folder.WorkspaceID ||
			sourceFolderID != folder.FolderID || savedHostID != hostID || savedBindingVersion != bindingVersion || sourceState == "revoked" {
			return domain.NewError(domain.CodeWorkspaceConflict, "project source identity is already bound to another installation or target")
		}
		if sourceState != "pending" && sourceState != "active" || lastSessionID.Valid != (session.SourceBaselineSessionID != "") ||
			lastSessionID.Valid && lastSessionID.String != session.SourceBaselineSessionID {
			return domain.NewError(domain.CodeWorkspaceConflict, "project source baseline changed before session creation")
		}
	}
	// Serialize the unresolved check behind the source row lock. A concurrent
	// create that committed while this transaction waited must be visible here.
	var unresolved bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_sync_project_upload_applications
WHERE workspace_id=$1 AND folder_id=$2 AND state IN ('pending_apply','conflict','unknown')
) OR EXISTS (
SELECT 1 FROM workspace_sync_sessions
WHERE workspace_id=$1 AND folder_id=$2 AND state IN ('planned','transferring','verifying','applying')
)`, folder.WorkspaceID, folder.FolderID).Scan(&unresolved); err != nil {
		return fmt.Errorf("check unresolved project upload after source lock: %w", err)
	}
	if unresolved {
		return domain.NewError(domain.CodeWorkspaceBusy, "a previous project upload still needs recovery")
	}
	return nil
}

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
		session.Direction != workspace.SyncUpload || session.Policy != workspace.SyncCompareMerge ||
		!workspace.ValidProjectSourceBindingID(session.SourceBindingID) || session.FolderBaselineRevision != nil ||
		session.BaseRevision != session.PinnedRevision {
		return domain.NewError(domain.CodeWorkspaceUnavailable, "project upload session is outside its registered folder")
	}
	return nil
}

func workspaceSyncSessionBaselineMatches(ctx context.Context, tx pgx.Tx, row sqlcgen.WorkspaceSyncSession,
	folder workspace.SyncFolder) (bool, error) {
	// Deletion ownership is pinned to the Host-confirmed source session. The
	// legacy LastUploadRevision only rejects a server snapshot older than the
	// latest successful project commit; it never supplies PC-owned deletions.
	if !row.FolderID.Valid || row.FolderID.String != folder.FolderID || row.FolderBaselineRevision.Valid ||
		row.BaseRevision != row.PinnedRevision ||
		folder.LastUploadRevision != nil && row.BaseRevision < *folder.LastUploadRevision {
		return false, nil
	}
	var sourceBindingID, plannedBaselineSessionID, currentBaselineSessionID pgtype.Text
	var sourceState, sourceHostID, bindingState string
	var sourceBindingVersion, bindingVersion int64
	err := tx.QueryRow(ctx, `SELECT s.source_binding_id,s.source_baseline_session_id,
       source.last_applied_session_id,source.state,source.host_id,source.binding_version,b.binding_version,b.state
FROM workspace_sync_sessions s
JOIN workspace_sync_project_sources source
  ON source.source_binding_id=s.source_binding_id
 AND source.owner_id=s.owner_id
 AND source.workspace_id=s.workspace_id
 AND source.folder_id=s.folder_id
JOIN workspace_sync_folder_registration_requests r
  ON r.owner_id=source.owner_id AND r.workspace_id=source.workspace_id AND r.folder_id=source.folder_id AND r.host_id=source.host_id
JOIN workspace_bindings b
  ON b.host_id=source.host_id AND b.workspace_id=source.workspace_id AND b.owner_id=source.owner_id
WHERE s.workspace_id=$1 AND s.owner_id=$2 AND s.id=$3 AND s.folder_id=$4
ORDER BY r.created_at DESC LIMIT 1
FOR UPDATE OF source,b`, row.WorkspaceID, row.OwnerID, row.ID, folder.FolderID).Scan(
		&sourceBindingID, &plannedBaselineSessionID, &currentBaselineSessionID, &sourceState,
		&sourceHostID, &sourceBindingVersion, &bindingVersion, &bindingState,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock Workspace project source baseline for sync commit: %w", err)
	}
	if !sourceBindingID.Valid || !row.SourceBindingID.Valid || sourceBindingID.String != row.SourceBindingID.String ||
		!workspace.ValidProjectSourceBindingID(sourceBindingID.String) || sourceHostID == "" ||
		sourceBindingVersion != bindingVersion || bindingState != string(workspace.BindingActive) ||
		(sourceState != "pending" && sourceState != "active") ||
		plannedBaselineSessionID.Valid != currentBaselineSessionID.Valid {
		return false, nil
	}
	if plannedBaselineSessionID.Valid && plannedBaselineSessionID.String != currentBaselineSessionID.String {
		return false, nil
	}
	return true, nil
}

func createWorkspaceSyncProjectUploadApplication(ctx context.Context, tx pgx.Tx, row sqlcgen.WorkspaceSyncSession,
	folder workspace.SyncFolder, targetRevision int64, session workspace.SyncSession) error {
	if !row.FolderID.Valid || row.FolderID.String != folder.FolderID || !row.SourceBindingID.Valid ||
		!workspace.ValidProjectSourceBindingID(row.SourceBindingID.String) || targetRevision < 0 ||
		len(row.Scope) != 1 || row.Scope[0] != folder.RelativePath {
		return domain.NewError(domain.CodeWorkspaceConflict, "project upload source receipt fields are invalid")
	}
	var hostID, sourceState string
	var sourceBindingVersion, workspaceBindingVersion int64
	var savedBaselineSessionID pgtype.Text
	err := tx.QueryRow(ctx, `SELECT source.host_id,source.binding_version,source.state,source.last_applied_session_id,b.binding_version
FROM workspace_sync_project_sources source
JOIN workspace_bindings b ON b.host_id=source.host_id AND b.workspace_id=source.workspace_id AND b.owner_id=source.owner_id
WHERE source.source_binding_id=$1 AND source.owner_id=$2 AND source.workspace_id=$3 AND source.folder_id=$4
  AND b.state='active'
FOR UPDATE OF source,b`, row.SourceBindingID.String, row.OwnerID, row.WorkspaceID, folder.FolderID).
		Scan(&hostID, &sourceBindingVersion, &sourceState, &savedBaselineSessionID, &workspaceBindingVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeWorkspaceConflict, "project upload Host binding changed before commit")
	}
	if err != nil {
		return fmt.Errorf("lock project upload source and Host binding: %w", err)
	}
	if (sourceState != "pending" && sourceState != "active") || sourceBindingVersion != workspaceBindingVersion ||
		savedBaselineSessionID.Valid != row.SourceBaselineSessionID.Valid ||
		savedBaselineSessionID.Valid && savedBaselineSessionID.String != row.SourceBaselineSessionID.String {
		return domain.NewError(domain.CodeWorkspaceConflict, "project upload source baseline changed before commit")
	}
	if sourceState == "pending" {
		updated, err := tx.Exec(ctx, `UPDATE workspace_sync_project_sources SET state='active',updated_at=now()
WHERE source_binding_id=$1 AND state='pending'`, row.SourceBindingID.String)
		if err != nil {
			return fmt.Errorf("activate project source for Host application: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeWorkspaceConflict, "project source changed before Host application")
		}
	}
	files, err := decodeWorkspaceFiles(row.Files)
	if err != nil {
		return err
	}
	digest, err := workspace.ProjectManifestSHA256(folder.RelativePath, files, nonNilStrings(row.EmptyDirectories), session.Directories)
	if err != nil {
		return domain.NewError(domain.CodeInternal, "project upload manifest digest is invalid")
	}
	digestBytes, err := hex.DecodeString(digest)
	if err != nil || len(digestBytes) != 32 {
		return domain.NewError(domain.CodeInternal, "project upload manifest digest is invalid")
	}
	_, err = tx.Exec(ctx, `INSERT INTO workspace_sync_project_upload_applications
(workspace_id,session_id,owner_id,folder_id,source_binding_id,source_binding_version,source_baseline_session_id,
 host_id,workspace_binding_version,target_revision,state,manifest_sha256,impacts)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'pending_apply',$11,'[]'::jsonb)`,
		row.WorkspaceID, row.ID, row.OwnerID, folder.FolderID, row.SourceBindingID.String, sourceBindingVersion,
		row.SourceBaselineSessionID, hostID, workspaceBindingVersion, targetRevision, digestBytes)
	if err != nil {
		return fmt.Errorf("persist pending Host project upload application: %w", err)
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
