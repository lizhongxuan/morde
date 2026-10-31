package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

type WorkspaceSyncReceiveOperation string

const (
	WorkspaceSyncReceiveUpsert          WorkspaceSyncReceiveOperation = "upsert"
	WorkspaceSyncReceiveDelete          WorkspaceSyncReceiveOperation = "delete"
	WorkspaceSyncReceiveDirectoryUpsert WorkspaceSyncReceiveOperation = "directory_upsert"
	WorkspaceSyncReceiveDirectoryDelete WorkspaceSyncReceiveOperation = "directory_delete"
)

type WorkspaceSyncReceiveRequest struct {
	OwnerID, WorkspaceID, FolderID, OperationID string
	Path                                        string
	Operation                                   WorkspaceSyncReceiveOperation
	TargetBytes                                 int64
}

type WorkspaceSyncReceiveReservation struct {
	ID, OwnerID, WorkspaceID, OperationID string
	Path                                  string
	Operation                             WorkspaceSyncReceiveOperation
	TargetBytes, BaseRevision             int64
	BaseFileSize                          *int64
	BaseFileSHA256                        *string
	BaseDirectoryPath                     *string
	State                                 string
	ResultRevision                        *int64
	ResultSHA256                          *string
	ResultMode                            *uint32
	RequestHash                           []byte
}

type WorkspaceSyncReceiveReceipt struct {
	ReservationID, WorkspaceID string
	State                      string
	Revision                   int64
	SHA256                     string
}

type WorkspaceSyncReceiveStore interface {
	ReserveWorkspaceSyncReceive(context.Context, WorkspaceSyncReceiveRequest, string, []byte) (WorkspaceSyncReceiveReservation, error)
	ReserveWorkspaceSyncReceiveBlockUsage(context.Context, string, string, string, []WorkspaceBlockReservation, int64) error
	GetWorkspaceSyncReceiveReservation(context.Context, string, string) (WorkspaceSyncReceiveReservation, error)
	CommitWorkspaceSyncReceive(context.Context, string, string, string, *workspace.File, *string) (WorkspaceSyncReceiveReceipt, error)
	ListActiveWorkspaceSyncReceiveReservations(context.Context) ([]WorkspaceSyncReceiveReservation, error)
	ListActiveWorkspaceSyncReceiveOwners(context.Context) ([]string, error)
	BeginAbortWorkspaceSyncReceive(context.Context, string, string, string) (WorkspaceSyncReceiveReservation, error)
	CompleteAbortWorkspaceSyncReceive(context.Context, string, string, string) error
}

var workspaceSyncReceiveOperationIDPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ReserveWorkspaceSyncReceive gates one direct Syncthing write per Workspace
// and persists its quota/base-revision contract before the node changes disk.
func (s *WorkspaceSyncService) ReserveWorkspaceSyncReceive(ctx context.Context, request WorkspaceSyncReceiveRequest) (WorkspaceSyncReceiveReservation, error) {
	store, err := s.workspaceSyncReceiveStore()
	if err != nil {
		return WorkspaceSyncReceiveReservation{}, err
	}
	if ctx == nil || !domain.ValidUserID(request.OwnerID) || !domain.ValidWorkspaceID(request.WorkspaceID) || !workspace.ValidSyncFolderID(request.FolderID) ||
		!workspaceSyncReceiveOperationIDPattern.MatchString(request.OperationID) || workspace.ValidateRelativePath(request.Path) != nil ||
		request.TargetBytes < 0 || request.TargetBytes > workspace.MaxSyncFileBytes ||
		(request.Operation != WorkspaceSyncReceiveUpsert && request.Operation != WorkspaceSyncReceiveDelete &&
			request.Operation != WorkspaceSyncReceiveDirectoryUpsert && request.Operation != WorkspaceSyncReceiveDirectoryDelete) ||
		(request.Operation != WorkspaceSyncReceiveUpsert && request.TargetBytes != 0) {
		return WorkspaceSyncReceiveReservation{}, domain.InvalidArgument("workspace_sync_receive", "Workspace receive reservation is invalid")
	}
	if strings.EqualFold(request.Path, ".stfolder") || strings.EqualFold(request.Path, ".stignore") {
		return WorkspaceSyncReceiveReservation{}, domain.InvalidArgument("path", "Syncthing control files are not Workspace content")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return WorkspaceSyncReceiveReservation{}, domain.InvalidArgument("workspace_sync_receive", "Workspace receive reservation cannot be encoded")
	}
	requestHash := sha256.Sum256(encoded)
	reservationID, err := domain.NewPendingRequestID()
	if err != nil {
		return WorkspaceSyncReceiveReservation{}, err
	}
	return store.ReserveWorkspaceSyncReceive(ctx, request, reservationID, requestHash[:])
}

// CommitWorkspaceSyncReceive accepts already-written content after the caller
// has verified files and accounted for every immutable block they reference.
func (s *WorkspaceSyncService) CommitWorkspaceSyncReceive(ctx context.Context, ownerID, workspaceID, reservationID string, file *workspace.File, directory *string) (WorkspaceSyncReceiveReceipt, error) {
	store, err := s.workspaceSyncReceiveStore()
	if err != nil {
		return WorkspaceSyncReceiveReceipt{}, err
	}
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || !domain.ValidPendingRequestID(reservationID) {
		return WorkspaceSyncReceiveReceipt{}, domain.InvalidArgument("workspace_sync_receive", "Workspace receive settlement is invalid")
	}
	if file != nil && directory != nil {
		return WorkspaceSyncReceiveReceipt{}, domain.InvalidArgument("workspace_sync_receive", "Workspace receive settlement cannot contain both file and directory content")
	}
	result, err := store.CommitWorkspaceSyncReceive(ctx, ownerID, workspaceID, reservationID, file, directory)
	if err != nil {
		return WorkspaceSyncReceiveReceipt{}, err
	}
	// shortcut: rebuild the full Workspace after each received file; optimize to verified path updates if Ubuntu measurements show this dominates sync time.
	if err := s.materializeWorkspaceCurrent(ctx, workspaceID); err != nil {
		if ctx.Err() != nil {
			return WorkspaceSyncReceiveReceipt{}, ctx.Err()
		}
		return WorkspaceSyncReceiveReceipt{}, domain.NewError(domain.CodeWorkspaceBusy, "Workspace receive commit is durable but current-directory materialization is pending; retry the same reservation")
	}
	return result, nil
}

func (s *WorkspaceSyncService) GetWorkspaceSyncReceiveReservation(ctx context.Context, ownerID, reservationID string) (WorkspaceSyncReceiveReservation, error) {
	store, err := s.workspaceSyncReceiveStore()
	if err != nil {
		return WorkspaceSyncReceiveReservation{}, err
	}
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidPendingRequestID(reservationID) {
		return WorkspaceSyncReceiveReservation{}, domain.InvalidArgument("workspace_sync_receive", "workspace receive reservation identity is invalid")
	}
	reservation, err := store.GetWorkspaceSyncReceiveReservation(ctx, ownerID, reservationID)
	if err != nil {
		return WorkspaceSyncReceiveReservation{}, err
	}
	if reservation.State == "settled" {
		if err := s.materializeWorkspaceCurrent(ctx, reservation.WorkspaceID); err != nil {
			if ctx.Err() != nil {
				return WorkspaceSyncReceiveReservation{}, ctx.Err()
			}
			return WorkspaceSyncReceiveReservation{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace receive is settled but current-directory materialization is pending; retry the same reservation")
		}
	}
	return reservation, nil
}

func (s *WorkspaceSyncService) ReserveWorkspaceSyncReceiveBlockUsage(ctx context.Context, ownerID, workspaceID, reservationID string, reservations []WorkspaceBlockReservation, maxOwnerBytes int64) error {
	store, err := s.workspaceSyncReceiveStore()
	if err != nil {
		return err
	}
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) ||
		!domain.ValidPendingRequestID(reservationID) || maxOwnerBytes <= 0 {
		return domain.InvalidArgument("workspace_sync_receive", "workspace receive block reservation identity is invalid")
	}
	for _, reservation := range reservations {
		if reservation.WorkspaceID != workspaceID || !validSyncDigest(reservation.Digest) ||
			reservation.SizeBytes < 1 || reservation.SizeBytes > workspace.MaxSyncBlockBytes || reservation.ReservedBytes < reservation.SizeBytes {
			return domain.InvalidArgument("workspace_sync_receive", "workspace receive block reservation is invalid")
		}
	}
	return store.ReserveWorkspaceSyncReceiveBlockUsage(ctx, ownerID, workspaceID, reservationID, reservations, maxOwnerBytes)
}

// ListActiveWorkspaceSyncReceiveOwners supports fail-closed owner relay gating
// while a receive is unresolved and must not be expired automatically.
func (s *WorkspaceSyncService) ListActiveWorkspaceSyncReceiveOwners(ctx context.Context) ([]string, error) {
	store, err := s.workspaceSyncReceiveStore()
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, domain.InvalidArgument("context", "workspace receive context is required")
	}
	return store.ListActiveWorkspaceSyncReceiveOwners(ctx)
}

func (s *WorkspaceSyncService) ListActiveWorkspaceSyncReceiveReservations(ctx context.Context) ([]WorkspaceSyncReceiveReservation, error) {
	store, err := s.workspaceSyncReceiveStore()
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, domain.InvalidArgument("context", "workspace receive context is required")
	}
	return store.ListActiveWorkspaceSyncReceiveReservations(ctx)
}

// AbortWorkspaceSyncReceive restores the current directory from the committed
// manifest before making the reservation terminal.
func (s *WorkspaceSyncService) AbortWorkspaceSyncReceive(ctx context.Context, ownerID, reservationID string) (retErr error) {
	store, err := s.workspaceSyncReceiveStore()
	if err != nil {
		return err
	}
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidPendingRequestID(reservationID) {
		return domain.InvalidArgument("workspace_sync_receive", "workspace receive abort identity is invalid")
	}
	current, err := store.GetWorkspaceSyncReceiveReservation(ctx, ownerID, reservationID)
	if err != nil {
		return err
	}
	workspaceID := current.WorkspaceID
	if current.State == "aborted" {
		return nil
	}
	blockLock, err := s.lockWorkspaceBlockStore(ctx, workspaceID, false)
	if err != nil {
		return err
	}
	defer func() {
		if err := blockLock.Release(ctx); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release Workspace block store lock after receive abort: %w", err))
		}
	}()

	reservation, err := store.BeginAbortWorkspaceSyncReceive(ctx, ownerID, workspaceID, reservationID)
	if err != nil {
		return err
	}
	if reservation.State == "aborted" {
		return nil
	}
	if reservation.State != "aborting" {
		return domain.NewError(domain.CodeWorkspaceConflict, "workspace receive reservation is not abortable")
	}
	if err := s.materializeWorkspaceCurrentForReceive(ctx, workspaceID, reservationID); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return domain.NewError(domain.CodeWorkspaceBusy, "workspace receive abort is pending current-directory recovery; retry the same reservation")
	}
	return store.CompleteAbortWorkspaceSyncReceive(ctx, ownerID, workspaceID, reservationID)
}

func (s *WorkspaceSyncService) workspaceSyncReceiveStore() (WorkspaceSyncReceiveStore, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("workspace receive store is unavailable")
	}
	store, ok := s.store.(WorkspaceSyncReceiveStore)
	if !ok {
		return nil, errors.New("workspace receive store is unavailable")
	}
	return store, nil
}
