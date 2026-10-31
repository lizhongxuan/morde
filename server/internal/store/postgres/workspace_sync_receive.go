package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

type receiveBlockReservation struct {
	reservedBytes int64
	sizeBytes     int64
}

func (s *Store) ReserveWorkspaceSyncReceive(ctx context.Context, request app.WorkspaceSyncReceiveRequest, reservationID string, requestHash []byte) (app.WorkspaceSyncReceiveReservation, error) {
	if !domain.ValidUserID(request.OwnerID) || !domain.ValidWorkspaceID(request.WorkspaceID) || !workspace.ValidSyncFolderID(request.FolderID) ||
		!domain.ValidPendingRequestID(reservationID) || !validPostgresSHA256(request.OperationID) || len(requestHash) != 32 ||
		workspace.ValidateRelativePath(request.Path) != nil || request.TargetBytes < 0 || request.TargetBytes > workspace.MaxSyncFileBytes ||
		(request.Operation != app.WorkspaceSyncReceiveUpsert && request.Operation != app.WorkspaceSyncReceiveDelete &&
			request.Operation != app.WorkspaceSyncReceiveDirectoryUpsert && request.Operation != app.WorkspaceSyncReceiveDirectoryDelete) ||
		(request.Operation != app.WorkspaceSyncReceiveUpsert && request.TargetBytes != 0) {
		return app.WorkspaceSyncReceiveReservation{}, domain.InvalidArgument("workspace_sync_receive", "Workspace receive reservation is invalid")
	}
	var reservation app.WorkspaceSyncReceiveReservation
	err := s.withWorkspaceSyncOwnerTx(ctx, request.OwnerID, func(tx pgx.Tx) error {
		var ownerID, state string
		var revision, usedBytes, quotaBytes, revisionBytes int64
		if err := tx.QueryRow(ctx, `SELECT owner_id, state, revision, used_bytes, quota_bytes FROM workspaces WHERE id=$1 FOR UPDATE`, request.WorkspaceID).
			Scan(&ownerID, &state, &revision, &usedBytes, &quotaBytes); err != nil {
			return storeError("lock Workspace for receive reservation", "workspace", err)
		}
		if ownerID != request.OwnerID {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if state != string(workspace.StateReady) {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace is not available for synchronization")
		}
		var pendingLegacyImport bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_legacy_volumes WHERE workspace_id=$1 AND import_state='pending'
)`, request.WorkspaceID).Scan(&pendingLegacyImport); err != nil {
			return fmt.Errorf("check pending legacy Workspace import before receive reservation: %w", err)
		}
		if pendingLegacyImport {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has a pending legacy import")
		}
		if err := requireActiveWorkspaceSyncReceive(ctx, tx, request.OwnerID, request.WorkspaceID, request.FolderID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT total_bytes FROM workspace_revisions WHERE workspace_id=$1 AND revision=$2`, request.WorkspaceID, revision).Scan(&revisionBytes); err != nil {
			return storeError("read Workspace revision for receive reservation", "workspace revision", err)
		}
		if revisionBytes != usedBytes {
			return domain.NewError(domain.CodeInternal, "Workspace storage usage does not match its latest manifest")
		}
		stored, found, err := getWorkspaceSyncReceiveReservation(ctx, tx, request.OwnerID, request.WorkspaceID, request.OperationID, false)
		if err != nil {
			return err
		}
		if found {
			if !bytes.Equal(stored.RequestHash, requestHash) || stored.Path != request.Path || stored.Operation != request.Operation || stored.TargetBytes != request.TargetBytes {
				return idempotencyConflict()
			}
			reservation = stored
			return nil
		}
		if err := ensureNoWorkspaceSyncReceive(ctx, tx, request.WorkspaceID, ""); err != nil {
			return err
		}
		if err := ensureNoPendingWorkspaceSyncCommit(ctx, tx, request.WorkspaceID); err != nil {
			return err
		}
		if err := ensureNoPendingWorkspaceTaskPublish(ctx, tx, request.WorkspaceID); err != nil {
			return err
		}
		if err := ensureNoActiveWorkspaceTaskOperations(ctx, tx, request.WorkspaceID); err != nil {
			return err
		}
		if err := ensureNoActiveWorkspaceSyncSessions(ctx, tx, request.WorkspaceID); err != nil {
			return err
		}
		var desiredRevision, appliedRevision int64
		if err := tx.QueryRow(ctx, `SELECT desired_revision, applied_revision FROM workspace_current_materializations WHERE workspace_id=$1 FOR UPDATE`, request.WorkspaceID).
			Scan(&desiredRevision, &appliedRevision); err != nil {
			return storeError("lock Workspace current materialization before receive reservation", "Workspace current materialization", err)
		}
		if desiredRevision != revision || appliedRevision != revision {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace current directory is not confirmed at its latest revision")
		}
		baseFiles, err := loadWorkspaceRevisionFiles(ctx, tx, request.WorkspaceID, revision, []string{request.Path})
		if err != nil {
			return err
		}
		baseDirectories, err := loadWorkspaceRevisionEmptyDirectories(ctx, tx, request.WorkspaceID, revision, []string{request.Path})
		if err != nil {
			return err
		}
		var baseSize *int64
		var baseSHA *string
		var baseDirectory *string
		if len(baseFiles) > 1 || len(baseDirectories) > 1 || len(baseFiles) > 0 && len(baseDirectories) > 0 {
			return domain.NewError(domain.CodeInternal, "Workspace contains duplicate case-folded paths")
		}
		if request.Operation == app.WorkspaceSyncReceiveDirectoryDelete && len(baseFiles) != 0 {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace directory path is occupied by a file")
		}
		if request.Operation == app.WorkspaceSyncReceiveDirectoryUpsert {
			hasDescendants, err := workspaceRevisionPathHasDescendants(ctx, tx, request.WorkspaceID, revision, request.Path)
			if err != nil {
				return err
			}
			if hasDescendants {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace directory path still contains manifest entries")
			}
		}
		if len(baseFiles) == 1 {
			size, digest := baseFiles[0].SizeBytes, baseFiles[0].SHA256
			baseSize, baseSHA = &size, &digest
		}
		if len(baseDirectories) == 1 {
			baseDirectory = &baseDirectories[0]
		}
		oldSize := int64(0)
		if baseSize != nil {
			oldSize = *baseSize
		}
		if oldSize > usedBytes {
			return domain.NewError(domain.CodeInternal, "Workspace file size exceeds committed usage")
		}
		projectedBytes := usedBytes - oldSize
		if request.Operation == app.WorkspaceSyncReceiveUpsert {
			if projectedBytes > int64(^uint64(0)>>1)-request.TargetBytes {
				return domain.NewError(domain.CodeQuotaExceeded, "Workspace storage quota would be exceeded")
			}
			projectedBytes += request.TargetBytes
		}
		if projectedBytes > quotaBytes && (usedBytes <= quotaBytes || projectedBytes >= usedBytes) {
			return domain.NewError(domain.CodeQuotaExceeded, "Workspace storage quota would be exceeded")
		}
		_, err = tx.Exec(ctx, `INSERT INTO workspace_sync_receive_reservations
(id, owner_id, workspace_id, operation_id, request_hash, file_path, folded_path, operation, target_size,
 base_revision, base_file_size, base_file_sha256, base_empty_directory_path, state)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'reserved')`, reservationID, request.OwnerID, request.WorkspaceID,
			request.OperationID, requestHash, request.Path, strings.ToLower(request.Path), string(request.Operation), request.TargetBytes,
			revision, baseSize, baseSHA, baseDirectory)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.NewError(domain.CodeWorkspaceBusy, "Workspace already has an active receive reservation")
			}
			return fmt.Errorf("create Workspace receive reservation: %w", err)
		}
		updated, err := tx.Exec(ctx, `UPDATE workspace_current_materializations SET applied_revision=-1, updated_at=now()
WHERE workspace_id=$1 AND desired_revision=$2 AND applied_revision=$2`, request.WorkspaceID, revision)
		if err != nil {
			return fmt.Errorf("invalidate Workspace current revision for receive reservation: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace current directory changed before receive reservation")
		}
		reservation = app.WorkspaceSyncReceiveReservation{
			ID: reservationID, OwnerID: request.OwnerID, WorkspaceID: request.WorkspaceID, OperationID: request.OperationID,
			Path: request.Path, Operation: request.Operation, TargetBytes: request.TargetBytes, BaseRevision: revision,
			BaseFileSize: baseSize, BaseFileSHA256: baseSHA, BaseDirectoryPath: baseDirectory,
			State: "reserved", RequestHash: append([]byte(nil), requestHash...),
		}
		return nil
	}, nil, nil)
	return reservation, err
}

func (s *Store) CommitWorkspaceSyncReceive(ctx context.Context, ownerID, workspaceID, reservationID string, file *workspace.File, directory *string) (app.WorkspaceSyncReceiveReceipt, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || !domain.ValidPendingRequestID(reservationID) {
		return app.WorkspaceSyncReceiveReceipt{}, domain.InvalidArgument("workspace_sync_receive", "Workspace receive settlement is invalid")
	}
	if file != nil && directory != nil {
		return app.WorkspaceSyncReceiveReceipt{}, domain.InvalidArgument("workspace_sync_receive", "Workspace receive settlement cannot contain file and directory content")
	}
	if file != nil {
		if err := workspace.ValidateFile(*file); err != nil || file.SizeBytes > workspace.MaxSyncFileBytes {
			return app.WorkspaceSyncReceiveReceipt{}, domain.InvalidArgument("file", "Workspace receive file is invalid")
		}
	}
	if directory != nil && workspace.ValidateRelativePath(*directory) != nil {
		return app.WorkspaceSyncReceiveReceipt{}, domain.InvalidArgument("directory", "Workspace receive directory is invalid")
	}
	var receipt app.WorkspaceSyncReceiveReceipt
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var workspaceOwner, workspaceState string
		var currentRevision, currentBytes, quotaBytes, currentFileCount, revisionBytes int64
		if err := tx.QueryRow(ctx, `SELECT owner_id, state, revision, used_bytes, quota_bytes FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).
			Scan(&workspaceOwner, &workspaceState, &currentRevision, &currentBytes, &quotaBytes); err != nil {
			return storeError("lock Workspace for receive settlement", "workspace", err)
		}
		if workspaceOwner != ownerID {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if workspaceState != string(workspace.StateReady) {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace is not available for synchronization")
		}
		reservation, found, err := getWorkspaceSyncReceiveReservationByID(ctx, tx, ownerID, workspaceID, reservationID, true)
		if err != nil {
			return err
		}
		if !found {
			return domain.NewError(domain.CodeNotFound, "Workspace receive reservation not found")
		}
		if reservation.State == "settled" {
			if reservation.ResultRevision == nil || !workspaceSyncReceiveSettlementMatches(reservation, file, directory) {
				return idempotencyConflict()
			}
			revision := *reservation.ResultRevision
			digest := ""
			if reservation.ResultSHA256 != nil {
				digest = *reservation.ResultSHA256
			}
			receipt = app.WorkspaceSyncReceiveReceipt{ReservationID: reservation.ID, WorkspaceID: workspaceID, State: reservation.State, Revision: revision, SHA256: digest}
			return nil
		}
		if reservation.State == "aborting" || reservation.State == "aborted" {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace receive reservation is being aborted or was aborted")
		}
		if reservation.State != "reserved" || reservation.BaseRevision != currentRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed while the receive reservation was active")
		}
		if err := ensureNoWorkspaceSyncReceive(ctx, tx, workspaceID, reservationID); err != nil {
			return err
		}
		if err := ensureNoPendingWorkspaceSyncCommit(ctx, tx, workspaceID); err != nil {
			return err
		}
		if err := ensureNoPendingWorkspaceTaskPublish(ctx, tx, workspaceID); err != nil {
			return err
		}
		if file != nil && (reservation.Operation != app.WorkspaceSyncReceiveUpsert || file.Path != reservation.Path || file.SizeBytes != reservation.TargetBytes) {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace receive file does not match its reservation")
		}
		if directory != nil && ((reservation.Operation != app.WorkspaceSyncReceiveDirectoryUpsert && reservation.Operation != app.WorkspaceSyncReceiveDirectoryDelete) || *directory != reservation.Path) {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace receive directory does not match its reservation")
		}
		if file == nil && directory == nil && reservation.Operation != app.WorkspaceSyncReceiveDelete {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace receive operation requires directory content")
		}
		if err := tx.QueryRow(ctx, `SELECT file_count, total_bytes FROM workspace_revisions WHERE workspace_id=$1 AND revision=$2`, workspaceID, currentRevision).
			Scan(&currentFileCount, &revisionBytes); err != nil {
			return storeError("read Workspace revision for receive settlement", "workspace revision", err)
		}
		if currentBytes != revisionBytes {
			return domain.NewError(domain.CodeInternal, "Workspace storage usage does not match its latest manifest")
		}
		currentFiles, err := loadWorkspaceRevisionFiles(ctx, tx, workspaceID, currentRevision, []string{reservation.Path})
		if err != nil {
			return err
		}
		currentDirectories, err := loadWorkspaceRevisionEmptyDirectories(ctx, tx, workspaceID, currentRevision, []string{reservation.Path})
		if err != nil {
			return err
		}
		if len(currentFiles) > 1 || len(currentDirectories) > 1 || len(currentFiles) > 0 && len(currentDirectories) > 0 ||
			!workspaceSyncReceiveBaseMatches(reservation, currentFiles, currentDirectories) {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace path changed after its receive reservation")
		}
		var fileMutations []syncMutation
		var directoryMutations []syncDirectoryMutation
		switch reservation.Operation {
		case app.WorkspaceSyncReceiveUpsert:
			if len(currentFiles) != 1 || !sameWorkspaceFile(currentFiles[0], *file) {
				copy := *file
				copy.Chunks = append([]workspace.Chunk(nil), file.Chunks...)
				fileMutations = []syncMutation{{path: file.Path, file: &copy}}
			}
		case app.WorkspaceSyncReceiveDelete:
			if len(currentFiles) == 1 {
				fileMutations = []syncMutation{{path: currentFiles[0].Path}}
			}
		case app.WorkspaceSyncReceiveDirectoryUpsert:
			if len(currentDirectories) != 1 || currentDirectories[0] != *directory {
				directoryMutations = []syncDirectoryMutation{{path: *directory}}
			}
		case app.WorkspaceSyncReceiveDirectoryDelete:
			if len(currentFiles) != 0 {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace directory path is occupied by a file")
			}
			if len(currentDirectories) == 1 {
				directoryMutations = []syncDirectoryMutation{{path: currentDirectories[0], delete: true}}
			}
		}
		if err := ensureNoActiveWorkspaceTaskOperations(ctx, tx, workspaceID); err != nil {
			return err
		}
		if len(fileMutations)+len(directoryMutations) > 0 {
			if err := ensureWorkspaceMutationsBlockUsageStored(ctx, tx, workspaceID, fileMutations); err != nil {
				return err
			}
			if err := applyWorkspaceSyncTreeMutations(ctx, tx, workspaceID, currentRevision, currentFileCount, currentBytes, quotaBytes, fileMutations, directoryMutations); err != nil {
				return err
			}
			currentRevision++
		}
		if err := enqueueWorkspaceCurrentMaterialization(ctx, tx, workspaceID, currentRevision); err != nil {
			return err
		}
		if err := releaseWorkspaceSyncReceiveBlocks(ctx, tx, ownerID, workspaceID, reservationID); err != nil {
			return err
		}
		var resultSHA *string
		var resultMode *uint32
		if file != nil {
			digest := file.SHA256
			resultSHA = &digest
			mode := file.Mode
			resultMode = &mode
		}
		updated, err := tx.Exec(ctx, `UPDATE workspace_sync_receive_reservations
SET state='settled', result_revision=$2, result_sha256=$3, result_mode=$4, updated_at=clock_timestamp()
WHERE id=$1 AND workspace_id=$5 AND owner_id=$6 AND state='reserved'`, reservationID, currentRevision, resultSHA, resultMode, workspaceID, ownerID)
		if err != nil {
			return fmt.Errorf("settle Workspace receive reservation: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace receive reservation changed during settlement")
		}
		digest := ""
		if resultSHA != nil {
			digest = *resultSHA
		}
		receipt = app.WorkspaceSyncReceiveReceipt{ReservationID: reservationID, WorkspaceID: workspaceID, State: "settled", Revision: currentRevision, SHA256: digest}
		return nil
	})
	return receipt, err
}

func (s *Store) ReserveWorkspaceSyncReceiveBlockUsage(ctx context.Context, ownerID, workspaceID, reservationID string, reservations []app.WorkspaceBlockReservation, maxOwnerBytes int64) error {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || !domain.ValidPendingRequestID(reservationID) || maxOwnerBytes <= 0 {
		return domain.InvalidArgument("workspace_sync_receive", "Workspace receive block reservation identity is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var storedOwner, workspaceState string
		if err := tx.QueryRow(ctx, `SELECT owner_id, state FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).
			Scan(&storedOwner, &workspaceState); err != nil {
			return storeError("lock Workspace before receive block reservation", "workspace", err)
		}
		if storedOwner != ownerID {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if workspaceState != string(workspace.StateReady) {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace is not available for receive block reservation")
		}
		reservation, found, err := getWorkspaceSyncReceiveReservationByID(ctx, tx, ownerID, workspaceID, reservationID, true)
		if err != nil {
			return err
		}
		if !found {
			return domain.NewError(domain.CodeNotFound, "Workspace receive reservation not found")
		}
		if reservation.State != "reserved" {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace receive reservation is no longer active")
		}
		if err := reserveWorkspaceBlockUsage(ctx, tx, workspaceID, ownerID, reservations, maxOwnerBytes); err != nil {
			return err
		}
		byDigest := make(map[string]receiveBlockReservation, len(reservations))
		for _, item := range reservations {
			if item.WorkspaceID != workspaceID || !validPostgresSHA256(item.Digest) || item.SizeBytes < 1 ||
				item.SizeBytes > workspace.MaxSyncBlockBytes || item.ReservedBytes < item.SizeBytes {
				return domain.InvalidArgument("workspace_sync_receive", "Workspace receive block reservation is invalid")
			}
			if prior, exists := byDigest[item.Digest]; exists && (prior.reservedBytes != item.ReservedBytes || prior.sizeBytes != item.SizeBytes) {
				return domain.InvalidArgument("workspace_sync_receive", "duplicate Workspace receive block reservations disagree")
			}
			byDigest[item.Digest] = receiveBlockReservation{reservedBytes: item.ReservedBytes, sizeBytes: item.SizeBytes}
		}
		digests := make([]string, 0, len(byDigest))
		for digest := range byDigest {
			digests = append(digests, digest)
		}
		sort.Strings(digests)
		for _, digest := range digests {
			item := byDigest[digest]
			updated, err := tx.Exec(ctx, `INSERT INTO workspace_sync_receive_blocks (reservation_id, workspace_id, sha256, size_bytes, reserved_bytes)
VALUES ($1,$2,$3,$4,$5) ON CONFLICT (reservation_id, sha256)
DO UPDATE SET reserved_bytes=EXCLUDED.reserved_bytes
WHERE workspace_sync_receive_blocks.workspace_id=EXCLUDED.workspace_id
  AND workspace_sync_receive_blocks.size_bytes=EXCLUDED.size_bytes`, reservationID, workspaceID, digest, item.sizeBytes, item.reservedBytes)
			if err != nil {
				return fmt.Errorf("associate receive reservation with Workspace block: %w", err)
			}
			if updated.RowsAffected() != 1 {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace receive block association disagrees with its earlier reservation")
			}
		}
		return nil
	})
}

func (s *Store) ListActiveWorkspaceSyncReceiveReservations(ctx context.Context) ([]app.WorkspaceSyncReceiveReservation, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, owner_id, workspace_id, operation_id, request_hash, file_path,
operation, target_size, base_revision, base_file_size, base_file_sha256, base_empty_directory_path, state, result_revision, result_sha256, result_mode
FROM workspace_sync_receive_reservations WHERE state IN ('reserved','aborting') ORDER BY workspace_id`)
	if err != nil {
		return nil, fmt.Errorf("list active Workspace receive reservations: %w", err)
	}
	defer rows.Close()
	items := make([]app.WorkspaceSyncReceiveReservation, 0)
	for rows.Next() {
		item, found, err := scanWorkspaceSyncReceiveReservation(rows)
		if err != nil {
			return nil, err
		}
		if found {
			items = append(items, item)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read active Workspace receive reservations: %w", err)
	}
	return items, nil
}

func (s *Store) GetWorkspaceSyncReceiveReservation(ctx context.Context, ownerID, reservationID string) (app.WorkspaceSyncReceiveReservation, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidPendingRequestID(reservationID) {
		return app.WorkspaceSyncReceiveReservation{}, domain.InvalidArgument("workspace_sync_receive", "Workspace receive reservation identity is invalid")
	}
	var result app.WorkspaceSyncReceiveReservation
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		reservation, found, err := scanWorkspaceSyncReceiveReservation(tx.QueryRow(ctx, `SELECT id, owner_id, workspace_id, operation_id, request_hash, file_path,
operation, target_size, base_revision, base_file_size, base_file_sha256, base_empty_directory_path, state, result_revision, result_sha256, result_mode
FROM workspace_sync_receive_reservations WHERE owner_id=$1 AND id=$2`, ownerID, reservationID))
		if err != nil {
			return err
		}
		if !found {
			return domain.NewError(domain.CodeNotFound, "Workspace receive reservation not found")
		}
		result = reservation
		return nil
	})
	return result, err
}

func (s *Store) ListActiveWorkspaceSyncReceiveOwners(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT owner_id FROM workspace_sync_receive_reservations
WHERE state IN ('reserved','aborting') ORDER BY owner_id`)
	if err != nil {
		return nil, fmt.Errorf("list owners with active Workspace receive reservations: %w", err)
	}
	defer rows.Close()
	owners := make([]string, 0)
	for rows.Next() {
		var ownerID string
		if err := rows.Scan(&ownerID); err != nil {
			return nil, fmt.Errorf("scan owner with active Workspace receive reservation: %w", err)
		}
		owners = append(owners, ownerID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read owners with active Workspace receive reservations: %w", err)
	}
	return owners, nil
}

func (s *Store) BeginAbortWorkspaceSyncReceive(ctx context.Context, ownerID, workspaceID, reservationID string) (app.WorkspaceSyncReceiveReservation, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || !domain.ValidPendingRequestID(reservationID) {
		return app.WorkspaceSyncReceiveReservation{}, domain.InvalidArgument("workspace_sync_receive", "Workspace receive abort identity is invalid")
	}
	var result app.WorkspaceSyncReceiveReservation
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var workspaceOwner, workspaceState string
		var currentRevision int64
		if err := tx.QueryRow(ctx, `SELECT owner_id, state, revision FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).
			Scan(&workspaceOwner, &workspaceState, &currentRevision); err != nil {
			return storeError("lock Workspace before receive abort", "workspace", err)
		}
		if workspaceOwner != ownerID {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if workspaceState != string(workspace.StateReady) {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace is not available for receive recovery")
		}
		reservation, found, err := getWorkspaceSyncReceiveReservationByID(ctx, tx, ownerID, workspaceID, reservationID, true)
		if err != nil {
			return err
		}
		if !found {
			return domain.NewError(domain.CodeNotFound, "Workspace receive reservation not found")
		}
		switch reservation.State {
		case "aborted", "settled":
			result = reservation
			return nil
		case "reserved":
			var desiredRevision int64
			if err := tx.QueryRow(ctx, `SELECT desired_revision FROM workspace_current_materializations WHERE workspace_id=$1 FOR UPDATE`, workspaceID).
				Scan(&desiredRevision); err != nil {
				return storeError("lock Workspace current materialization before receive abort", "Workspace current materialization", err)
			}
			if desiredRevision != currentRevision {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace current materialization target changed during receive")
			}
			updated, err := tx.Exec(ctx, `UPDATE workspace_sync_receive_reservations SET state='aborting', updated_at=clock_timestamp()
WHERE id=$1 AND workspace_id=$2 AND owner_id=$3 AND state='reserved'`, reservationID, workspaceID, ownerID)
			if err != nil {
				return fmt.Errorf("begin Workspace receive abort: %w", err)
			}
			if updated.RowsAffected() != 1 {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace receive reservation changed before abort")
			}
			updated, err = tx.Exec(ctx, `UPDATE workspace_current_materializations SET applied_revision=-1, updated_at=now()
WHERE workspace_id=$1 AND desired_revision=$2`, workspaceID, currentRevision)
			if err != nil {
				return fmt.Errorf("invalidate Workspace current revision for receive abort: %w", err)
			}
			if updated.RowsAffected() != 1 {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace current materialization target changed before receive abort")
			}
			reservation.State = "aborting"
			result = reservation
			return nil
		case "aborting":
			result = reservation
			return nil
		default:
			return domain.NewError(domain.CodeInternal, "Workspace receive reservation state is invalid")
		}
	})
	return result, err
}

func (s *Store) CompleteAbortWorkspaceSyncReceive(ctx context.Context, ownerID, workspaceID, reservationID string) error {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || !domain.ValidPendingRequestID(reservationID) {
		return domain.InvalidArgument("workspace_sync_receive", "Workspace receive abort identity is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var workspaceOwner, workspaceState string
		var currentRevision int64
		if err := tx.QueryRow(ctx, `SELECT owner_id, state, revision FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).
			Scan(&workspaceOwner, &workspaceState, &currentRevision); err != nil {
			return storeError("lock Workspace before completing receive abort", "workspace", err)
		}
		if workspaceOwner != ownerID {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		reservation, found, err := getWorkspaceSyncReceiveReservationByID(ctx, tx, ownerID, workspaceID, reservationID, true)
		if err != nil {
			return err
		}
		if !found {
			return domain.NewError(domain.CodeNotFound, "Workspace receive reservation not found")
		}
		if reservation.State == "aborted" {
			return nil
		}
		if reservation.State != "aborting" {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace receive reservation is not aborting")
		}
		var desiredRevision, appliedRevision int64
		if err := tx.QueryRow(ctx, `SELECT desired_revision, applied_revision FROM workspace_current_materializations WHERE workspace_id=$1 FOR UPDATE`, workspaceID).
			Scan(&desiredRevision, &appliedRevision); err != nil {
			return storeError("read recovered Workspace current materialization", "Workspace current materialization", err)
		}
		if workspaceState != string(workspace.StateReady) || desiredRevision != currentRevision || appliedRevision != currentRevision {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace current directory recovery is not complete")
		}
		if err := releaseWorkspaceSyncReceiveBlocks(ctx, tx, ownerID, workspaceID, reservationID); err != nil {
			return err
		}
		updated, err := tx.Exec(ctx, `UPDATE workspace_sync_receive_reservations SET state='aborted', updated_at=clock_timestamp()
WHERE id=$1 AND workspace_id=$2 AND owner_id=$3 AND state='aborting'`, reservationID, workspaceID, ownerID)
		if err != nil {
			return fmt.Errorf("complete Workspace receive abort: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace receive reservation changed during abort")
		}
		return nil
	})
}

func releaseWorkspaceSyncReceiveBlocks(ctx context.Context, tx pgx.Tx, ownerID, workspaceID, reservationID string) error {
	_, ownerReservedBytes, err := lockWorkspaceOwnerUsage(ctx, tx, ownerID)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT b.sha256, u.state, u.reserved_bytes
FROM workspace_sync_receive_blocks b
JOIN workspace_block_usage u ON u.workspace_id=b.workspace_id AND u.sha256=b.sha256
WHERE b.reservation_id=$1 AND b.workspace_id=$2
ORDER BY b.sha256 FOR UPDATE OF u`, reservationID, workspaceID)
	if err != nil {
		return fmt.Errorf("lock Workspace receive block usage for abort: %w", err)
	}
	type usage struct {
		digest, state string
		reservedBytes int64
	}
	items := make([]usage, 0)
	for rows.Next() {
		var item usage
		if err := rows.Scan(&item.digest, &item.state, &item.reservedBytes); err != nil {
			rows.Close()
			return fmt.Errorf("read Workspace receive block usage for abort: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate Workspace receive block usage for abort: %w", err)
	}
	rows.Close()

	var releasedBytes int64
	for _, item := range items {
		if item.state != "reserved" {
			continue
		}
		var referenced bool
		if err := tx.QueryRow(ctx, `SELECT
EXISTS (
  SELECT 1 FROM workspace_sync_session_blocks b
  JOIN workspace_sync_sessions s ON s.workspace_id=b.workspace_id AND s.id=b.session_id
  WHERE b.workspace_id=$1 AND b.sha256=$2 AND b.role='upload'
    AND s.state IN ('planned','transferring','verifying','applying')
) OR EXISTS (
  SELECT 1 FROM workspace_sync_receive_blocks b
  JOIN workspace_sync_receive_reservations r ON r.id=b.reservation_id
  WHERE b.workspace_id=$1 AND b.sha256=$2 AND b.reservation_id<>$3
    AND r.state IN ('reserved','aborting')
)`, workspaceID, item.digest, reservationID).Scan(&referenced); err != nil {
			return fmt.Errorf("check Workspace receive block references before abort cleanup: %w", err)
		}
		if referenced {
			continue
		}
		var reserved int64
		err := tx.QueryRow(ctx, `DELETE FROM workspace_block_usage
WHERE workspace_id=$1 AND sha256=$2 AND state='reserved' RETURNING reserved_bytes`, workspaceID, item.digest).Scan(&reserved)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("release abandoned Workspace receive block reservation: %w", err)
		}
		if reserved < 0 || releasedBytes > int64(^uint64(0)>>1)-reserved {
			return domain.NewError(domain.CodeInternal, "Workspace receive block reservation total is invalid")
		}
		releasedBytes += reserved
	}
	if releasedBytes > 0 {
		if ownerReservedBytes < releasedBytes {
			return domain.NewError(domain.CodeInternal, "Workspace owner reserved block usage counters are inconsistent")
		}
		updated, err := tx.Exec(ctx, `UPDATE workspace_owner_block_usage
SET reserved_bytes=$2, updated_at=now() WHERE owner_id=$1`, ownerID, ownerReservedBytes-releasedBytes)
		if err != nil {
			return fmt.Errorf("release Workspace owner block reservation bytes: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeInternal, "Workspace owner block usage row is missing during receive abort")
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_sync_receive_blocks WHERE reservation_id=$1 AND workspace_id=$2`, reservationID, workspaceID); err != nil {
		return fmt.Errorf("remove Workspace receive block associations after abort: %w", err)
	}
	return nil
}

func ensureNoWorkspaceSyncReceive(ctx context.Context, tx pgx.Tx, workspaceID, allowedReservationID string) error {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_sync_receive_reservations
WHERE workspace_id=$1 AND state IN ('reserved','aborting') AND ($2='' OR id<>$2)
)`, workspaceID, allowedReservationID).Scan(&active); err != nil {
		return fmt.Errorf("check active Workspace receive reservation: %w", err)
	}
	if active {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has a direct receive in progress; retry after it settles")
	}
	return nil
}

func ensureNoWorkspaceSyncReceiveForOwner(ctx context.Context, tx pgx.Tx, ownerID string) error {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_sync_receive_reservations
WHERE owner_id=$1 AND state IN ('reserved','aborting')
)`, ownerID).Scan(&active); err != nil {
		return fmt.Errorf("check active Workspace receive reservation for owner: %w", err)
	}
	if active {
		return domain.NewError(domain.CodeWorkspaceBusy, "account has a Workspace receive in progress; retry after it settles")
	}
	return nil
}

func requireActiveWorkspaceSyncReceive(ctx context.Context, tx pgx.Tx, ownerID, workspaceID, folderID string) error {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM users u
JOIN workspace_sync_folders f ON f.owner_id=u.id AND f.workspace_id=$2 AND f.folder_id=$3 AND f.state='active'
JOIN workspace_sync_device_bindings b ON b.owner_id=f.owner_id AND b.workspace_id=f.workspace_id AND b.state='active'
JOIN client_installations i ON i.id=b.client_installation_id AND i.owner_id=b.owner_id
WHERE u.id=$1 AND u.status='approved' AND i.revoked_at IS NULL AND i.expires_at>clock_timestamp()
)`, ownerID, workspaceID, folderID).Scan(&active); err != nil {
		return fmt.Errorf("check active Workspace sync authorization: %w", err)
	}
	if !active {
		return domain.NewError(domain.CodeForbidden, "Workspace synchronization is not currently authorized")
	}
	return nil
}

func ensureNoPendingWorkspaceSyncCommit(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_sync_commit_receipts WHERE workspace_id=$1 AND state='pending'
)`, workspaceID).Scan(&pending); err != nil {
		return fmt.Errorf("check pending Workspace sync commit before direct receive: %w", err)
	}
	if pending {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has a pending sync commit; retry direct receive after it settles")
	}
	return nil
}

func ensureNoPendingWorkspaceTaskPublish(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM task_workspace_publish_receipts WHERE workspace_id=$1 AND state='pending'
)`, workspaceID).Scan(&pending); err != nil {
		return fmt.Errorf("check pending Task Workspace publish before direct receive: %w", err)
	}
	if pending {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has a pending Task publish; retry direct receive after it settles")
	}
	return nil
}

func ensureNoActiveWorkspaceSyncSessions(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_sync_sessions WHERE workspace_id=$1 AND state IN ('planned','transferring','verifying','applying')
)`, workspaceID).Scan(&active); err != nil {
		return fmt.Errorf("check active Workspace sync sessions: %w", err)
	}
	if active {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has an active sync session; retry direct receive after it settles")
	}
	return nil
}

func ensureNoActiveWorkspaceTaskOperations(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT
EXISTS (SELECT 1 FROM task_operations WHERE workspace_id=$1 AND status IN ('prepared','dispatched','unknown')) OR
EXISTS (SELECT 1 FROM task_mcp_connector_operations o JOIN tasks t ON t.id=o.task_id
  WHERE t.workspace_id=$1 AND o.status IN ('dispatched','unknown'))`, workspaceID).Scan(&active); err != nil {
		return fmt.Errorf("check admitted Task operations before Workspace receive commit: %w", err)
	}
	if active {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has an in-flight task operation; retry receive settlement after it settles")
	}
	return nil
}

func getWorkspaceSyncReceiveReservation(ctx context.Context, tx pgx.Tx, ownerID, workspaceID, operationID string, forUpdate bool) (app.WorkspaceSyncReceiveReservation, bool, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	return scanWorkspaceSyncReceiveReservation(tx.QueryRow(ctx, `SELECT id, owner_id, workspace_id, operation_id, request_hash, file_path,
operation, target_size, base_revision, base_file_size, base_file_sha256, base_empty_directory_path, state, result_revision, result_sha256, result_mode
FROM workspace_sync_receive_reservations WHERE owner_id=$1 AND workspace_id=$2 AND operation_id=$3`+lock, ownerID, workspaceID, operationID))
}

func getWorkspaceSyncReceiveReservationByID(ctx context.Context, tx pgx.Tx, ownerID, workspaceID, reservationID string, forUpdate bool) (app.WorkspaceSyncReceiveReservation, bool, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	return scanWorkspaceSyncReceiveReservation(tx.QueryRow(ctx, `SELECT id, owner_id, workspace_id, operation_id, request_hash, file_path,
operation, target_size, base_revision, base_file_size, base_file_sha256, base_empty_directory_path, state, result_revision, result_sha256, result_mode
FROM workspace_sync_receive_reservations WHERE owner_id=$1 AND workspace_id=$2 AND id=$3`+lock, ownerID, workspaceID, reservationID))
}

type workspaceSyncReceiveScanner interface {
	Scan(...any) error
}

func scanWorkspaceSyncReceiveReservation(row workspaceSyncReceiveScanner) (app.WorkspaceSyncReceiveReservation, bool, error) {
	var value app.WorkspaceSyncReceiveReservation
	var operation string
	var baseSize, resultRevision pgtype.Int8
	var baseSHA, baseDirectory, resultSHA pgtype.Text
	var resultMode pgtype.Int4
	err := row.Scan(&value.ID, &value.OwnerID, &value.WorkspaceID, &value.OperationID, &value.RequestHash, &value.Path,
		&operation, &value.TargetBytes, &value.BaseRevision, &baseSize, &baseSHA, &baseDirectory, &value.State, &resultRevision, &resultSHA, &resultMode)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.WorkspaceSyncReceiveReservation{}, false, nil
	}
	if err != nil {
		return app.WorkspaceSyncReceiveReservation{}, false, fmt.Errorf("read Workspace receive reservation: %w", err)
	}
	value.Operation = app.WorkspaceSyncReceiveOperation(operation)
	if baseSize.Valid {
		value.BaseFileSize = &baseSize.Int64
	}
	if baseSHA.Valid {
		value.BaseFileSHA256 = &baseSHA.String
	}
	if baseDirectory.Valid {
		value.BaseDirectoryPath = &baseDirectory.String
	}
	if resultRevision.Valid {
		value.ResultRevision = &resultRevision.Int64
	}
	if resultSHA.Valid {
		value.ResultSHA256 = &resultSHA.String
	}
	if resultMode.Valid {
		mode := uint32(resultMode.Int32)
		value.ResultMode = &mode
	}
	value.RequestHash = append([]byte(nil), value.RequestHash...)
	return value, true, nil
}

func workspaceSyncReceiveBaseMatches(reservation app.WorkspaceSyncReceiveReservation, files []workspace.File, directories []string) bool {
	if reservation.BaseFileSize != nil && reservation.BaseFileSHA256 != nil {
		return len(files) == 1 && len(directories) == 0 && files[0].SizeBytes == *reservation.BaseFileSize && files[0].SHA256 == *reservation.BaseFileSHA256
	}
	if reservation.BaseDirectoryPath != nil {
		return len(files) == 0 && len(directories) == 1 && directories[0] == *reservation.BaseDirectoryPath
	}
	return len(files) == 0 && len(directories) == 0
}

func workspaceSyncReceiveSettlementMatches(reservation app.WorkspaceSyncReceiveReservation, file *workspace.File, directory *string) bool {
	if reservation.Operation == app.WorkspaceSyncReceiveDelete {
		return file == nil && directory == nil
	}
	if reservation.Operation == app.WorkspaceSyncReceiveDirectoryUpsert || reservation.Operation == app.WorkspaceSyncReceiveDirectoryDelete {
		return file == nil && directory != nil && *directory == reservation.Path && reservation.ResultSHA256 == nil && reservation.ResultMode == nil
	}
	return directory == nil && file != nil && file.Path == reservation.Path && file.SizeBytes == reservation.TargetBytes &&
		reservation.ResultSHA256 != nil && reservation.ResultMode != nil && file.SHA256 == *reservation.ResultSHA256 && file.Mode == *reservation.ResultMode
}

func workspaceRevisionPathHasDescendants(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64, path string) (bool, error) {
	foldedPath := strings.ToLower(path)
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_revision_files
WHERE workspace_id=$1 AND valid_from_revision <= $2 AND (valid_to_revision IS NULL OR valid_to_revision > $2)
  AND left(folded_path, char_length($3) + 1) = $3 || '/'
UNION ALL
SELECT 1 FROM workspace_revision_empty_directories
WHERE workspace_id=$1 AND valid_from_revision <= $2 AND (valid_to_revision IS NULL OR valid_to_revision > $2)
  AND left(folded_path, char_length($3) + 1) = $3 || '/'
)`, workspaceID, revision, foldedPath).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check Workspace directory descendants before receive: %w", err)
	}
	return exists, nil
}

var _ app.WorkspaceSyncReceiveStore = (*Store)(nil)
