package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacecurrent"
	"morde/server/internal/syncnodequota"
)

type WorkspaceSyncQuotaService struct {
	sync    *WorkspaceSyncService
	current *workspacecurrent.Store
}

func NewWorkspaceSyncQuotaService(syncService *WorkspaceSyncService, current *workspacecurrent.Store) (*WorkspaceSyncQuotaService, error) {
	if syncService == nil || current == nil {
		return nil, errors.New("workspace sync quota service requires the sync service and current-file store")
	}
	return &WorkspaceSyncQuotaService{sync: syncService, current: current}, nil
}

func (s *WorkspaceSyncQuotaService) Reserve(ctx context.Context, ownerID string, request syncnodequota.ReserveRequest) (syncnodequota.Receipt, error) {
	reservation, err := s.sync.ReserveWorkspaceSyncReceive(ctx, WorkspaceSyncReceiveRequest{
		OwnerID: ownerID, WorkspaceID: request.WorkspaceID, FolderID: request.FolderID, OperationID: request.OperationID,
		Path: request.Path, Operation: WorkspaceSyncReceiveOperation(request.Operation), TargetBytes: request.SizeBytes,
	})
	if err != nil {
		return syncnodequota.Receipt{}, err
	}
	return workspaceSyncQuotaReceipt(reservation), nil
}

func (s *WorkspaceSyncQuotaService) Settle(ctx context.Context, ownerID, reservationID string) (syncnodequota.Receipt, error) {
	reservation, err := s.sync.GetWorkspaceSyncReceiveReservation(ctx, ownerID, reservationID)
	if err != nil {
		return syncnodequota.Receipt{}, err
	}
	switch reservation.State {
	case "settled", "aborted":
		return workspaceSyncQuotaReceipt(reservation), nil
	case "aborting":
		return syncnodequota.Receipt{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace receive abort is still in progress")
	case "reserved":
	default:
		return syncnodequota.Receipt{}, domain.NewError(domain.CodeInvalidState, "workspace receive reservation state is invalid")
	}
	var file *workspace.File
	var directory *string
	switch reservation.Operation {
	case WorkspaceSyncReceiveUpsert:
		file, err = s.importReceivedFile(ctx, reservation)
	case WorkspaceSyncReceiveDelete:
		_, err = s.current.StatPath(reservation.OwnerID, reservation.WorkspaceID, reservation.Path)
		err = workspaceSyncFileDeleteError(err)
	case WorkspaceSyncReceiveDirectoryUpsert:
		var empty bool
		empty, err = s.current.IsEmptyDirectory(reservation.OwnerID, reservation.WorkspaceID, reservation.Path)
		if err == nil && !empty {
			err = domain.NewError(domain.CodeWorkspaceConflict, "workspace receive directory is not empty")
		} else if errors.Is(err, os.ErrNotExist) {
			err = domain.NewError(domain.CodeWorkspaceBusy, "workspace receive directory is not installed yet")
		} else if errors.Is(err, workspacecurrent.ErrUnsafePath) {
			err = domain.NewError(domain.CodeWorkspaceConflict, "workspace receive path is not a safe directory")
		}
		directory = &reservation.Path
	case WorkspaceSyncReceiveDirectoryDelete:
		_, err = s.current.StatPath(reservation.OwnerID, reservation.WorkspaceID, reservation.Path)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		} else if err == nil {
			err = domain.NewError(domain.CodeWorkspaceConflict, "workspace directory receive did not remove the directory")
		} else if errors.Is(err, workspacecurrent.ErrUnsafePath) {
			err = domain.NewError(domain.CodeWorkspaceConflict, "workspace receive path is not a safe directory")
		}
		directory = &reservation.Path
	default:
		err = domain.NewError(domain.CodeInvalidState, "workspace receive operation is invalid")
	}
	if err != nil {
		if domain.CodeOf(err) == domain.CodeQuotaExceeded || domain.CodeOf(err) == domain.CodeWorkspaceConflict {
			if abortErr := s.sync.AbortWorkspaceSyncReceive(ctx, ownerID, reservationID); abortErr != nil {
				return syncnodequota.Receipt{}, errors.Join(err, fmt.Errorf("restore Workspace after rejected receive: %w", abortErr))
			}
		}
		return syncnodequota.Receipt{}, err
	}
	result, err := s.sync.CommitWorkspaceSyncReceive(ctx, ownerID, reservation.WorkspaceID, reservationID, file, directory)
	if err != nil {
		return syncnodequota.Receipt{}, err
	}
	if result.ReservationID != reservationID || result.State != "settled" {
		return syncnodequota.Receipt{}, domain.NewError(domain.CodeInternal, "workspace receive did not produce a settled receipt")
	}
	revision := result.Revision
	return syncnodequota.Receipt{
		ReservationID: result.ReservationID, State: result.State, BaseRevision: reservation.BaseRevision,
		ResultRevision: &revision,
	}, nil
}

func (s *WorkspaceSyncQuotaService) Abort(ctx context.Context, ownerID, reservationID string) (syncnodequota.Receipt, error) {
	reservation, err := s.sync.GetWorkspaceSyncReceiveReservation(ctx, ownerID, reservationID)
	if err != nil {
		return syncnodequota.Receipt{}, err
	}
	if reservation.State != "settled" && reservation.State != "aborted" {
		if err := s.sync.AbortWorkspaceSyncReceive(ctx, ownerID, reservationID); err != nil {
			return syncnodequota.Receipt{}, err
		}
		reservation, err = s.sync.GetWorkspaceSyncReceiveReservation(ctx, ownerID, reservationID)
		if err != nil {
			return syncnodequota.Receipt{}, err
		}
	}
	return workspaceSyncQuotaReceipt(reservation), nil
}

func workspaceSyncQuotaReceipt(value WorkspaceSyncReceiveReservation) syncnodequota.Receipt {
	return syncnodequota.Receipt{
		ReservationID: value.ID, State: value.State, BaseRevision: value.BaseRevision,
		ResultRevision: value.ResultRevision,
	}
}

func workspaceSyncFileDeleteError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return domain.NewError(domain.CodeWorkspaceConflict, "workspace delete receive did not remove the file")
}

func (s *WorkspaceSyncQuotaService) importReceivedFile(ctx context.Context, reservation WorkspaceSyncReceiveReservation) (*workspace.File, error) {
	file, info, err := s.current.OpenRegularFile(reservation.OwnerID, reservation.WorkspaceID, reservation.Path)
	if err != nil {
		if errors.Is(err, workspacecurrent.ErrUnsafePath) {
			return nil, domain.NewError(domain.CodeWorkspaceConflict, "workspace receive path is not a regular file")
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, domain.NewError(domain.CodeWorkspaceBusy, "workspace receive file is not installed yet")
		}
		return nil, fmt.Errorf("open received Workspace file: %w", err)
	}
	defer func() { _ = file.Close() }()
	if info.Size() != reservation.TargetBytes || !info.Mode().IsRegular() {
		return nil, domain.NewError(domain.CodeWorkspaceConflict, "received Workspace file size does not match its reservation")
	}
	value, err := describeWorkspaceReceiveFile(file, reservation)
	if err != nil {
		return nil, err
	}
	blockLock, err := s.sync.lockWorkspaceBlockStore(ctx, reservation.WorkspaceID, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = blockLock.Release(ctx) }()
	usageStore, ok := s.sync.store.(interface {
		ReserveWorkspaceSyncReceiveBlockUsage(context.Context, string, string, string, []WorkspaceBlockReservation, int64) error
	})
	if !ok {
		return nil, errors.New("workspace receive physical quota ledger is unavailable")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind received Workspace file: %w", err)
	}
	secondHash := sha256.New()
	seen := make(map[string]struct{}, len(value.Chunks))
	for _, chunk := range value.Chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data := make([]byte, int(chunk.SizeBytes))
		if _, err := io.ReadFull(file, data); err != nil {
			return nil, fmt.Errorf("read received Workspace block: %w", err)
		}
		_, _ = secondHash.Write(data)
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != chunk.SHA256 {
			return nil, domain.NewError(domain.CodeWorkspaceConflict, "received Workspace file changed during block import")
		}
		if _, exists := seen[chunk.SHA256]; exists {
			continue
		}
		seen[chunk.SHA256] = struct{}{}
		reservedBytes, err := s.sync.blocks.ReservationBytes(chunk.SizeBytes)
		if err != nil || reservedBytes <= 0 {
			return nil, domain.NewError(domain.CodeRequestTooLarge, "received Workspace block reservation is invalid")
		}
		blockReservation := WorkspaceBlockReservation{
			WorkspaceID: reservation.WorkspaceID, Digest: chunk.SHA256,
			ReservedBytes: reservedBytes, SizeBytes: chunk.SizeBytes,
		}
		if err := usageStore.ReserveWorkspaceSyncReceiveBlockUsage(ctx, reservation.OwnerID, reservation.WorkspaceID, reservation.ID, []WorkspaceBlockReservation{blockReservation}, s.sync.maxOwnerBytes); err != nil {
			return nil, err
		}
		if _, err := readVerifiedBlock(s.sync.blocks, reservation.WorkspaceID, chunk); err == nil {
			allocatedBytes, err := s.sync.blocks.BlockAllocatedBytes(reservation.WorkspaceID, chunk.SHA256)
			if err != nil {
				return nil, fmt.Errorf("inspect existing received Workspace block: %w", err)
			}
			if err := s.sync.store.FinalizeWorkspaceBlockUsage(ctx, reservation.WorkspaceID, chunk.SHA256, allocatedBytes, s.sync.maxOwnerBytes); err != nil {
				return nil, err
			}
			continue
		} else if domain.CodeOf(err) != domain.CodeNotFound {
			return nil, fmt.Errorf("verify existing received Workspace block: %w", err)
		}
		storedBytes, err := s.sync.blocks.PutBlock(reservation.WorkspaceID, chunk.SHA256, bytes.NewReader(data))
		if err != nil {
			allocatedBytes, statErr := s.sync.blocks.BlockAllocatedBytes(reservation.WorkspaceID, chunk.SHA256)
			if statErr == nil {
				_ = s.sync.store.FinalizeWorkspaceBlockUsage(ctx, reservation.WorkspaceID, chunk.SHA256, allocatedBytes, s.sync.maxOwnerBytes)
			}
			return nil, fmt.Errorf("store received Workspace block: %w", err)
		}
		if storedBytes != chunk.SizeBytes {
			return nil, domain.NewError(domain.CodeWorkspaceConflict, "stored Workspace block size does not match the manifest")
		}
		allocatedBytes, err := s.sync.blocks.BlockAllocatedBytes(reservation.WorkspaceID, chunk.SHA256)
		if err != nil {
			return nil, fmt.Errorf("inspect stored received Workspace block: %w", err)
		}
		if err := s.sync.store.FinalizeWorkspaceBlockUsage(ctx, reservation.WorkspaceID, chunk.SHA256, allocatedBytes, s.sync.maxOwnerBytes); err != nil {
			return nil, err
		}
	}
	if hex.EncodeToString(secondHash.Sum(nil)) != value.SHA256 {
		return nil, domain.NewError(domain.CodeWorkspaceConflict, "received Workspace file changed during block import")
	}
	return &value, nil
}

func describeWorkspaceReceiveFile(file *os.File, reservation WorkspaceSyncReceiveReservation) (workspace.File, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return workspace.File{}, fmt.Errorf("rewind received Workspace file: %w", err)
	}
	wholeHash := sha256.New()
	chunks := make([]workspace.Chunk, 0, (reservation.TargetBytes+workspace.MaxSyncBlockBytes-1)/workspace.MaxSyncBlockBytes)
	remaining := reservation.TargetBytes
	for remaining > 0 {
		size := min(remaining, int64(workspace.MaxSyncBlockBytes))
		data := make([]byte, int(size))
		if _, err := io.ReadFull(file, data); err != nil {
			return workspace.File{}, fmt.Errorf("preflight received Workspace block: %w", err)
		}
		_, _ = wholeHash.Write(data)
		digest := sha256.Sum256(data)
		chunks = append(chunks, workspace.Chunk{SHA256: hex.EncodeToString(digest[:]), SizeBytes: size})
		remaining -= size
	}
	info, err := file.Stat()
	if err != nil {
		return workspace.File{}, fmt.Errorf("inspect received Workspace file after preflight: %w", err)
	}
	if info.Size() != reservation.TargetBytes || !info.Mode().IsRegular() {
		return workspace.File{}, domain.NewError(domain.CodeWorkspaceConflict, "received Workspace file changed during preflight")
	}
	value := workspace.File{
		Path: reservation.Path, SizeBytes: reservation.TargetBytes, SHA256: hex.EncodeToString(wholeHash.Sum(nil)),
		Mode: uint32(info.Mode().Perm() & 0o777), Chunks: chunks,
	}
	if err := workspace.ValidateFile(value); err != nil {
		return workspace.File{}, domain.NewError(domain.CodeWorkspaceConflict, "received Workspace file manifest is invalid")
	}
	return value, nil
}
