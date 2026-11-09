package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type WorkspaceCopyCommand struct {
	Name                    string
	ExpectedCurrentRevision int64
}

type WorkspaceCopyResult struct {
	SourceWorkspaceID string              `json:"source_workspace_id"`
	Workspace         DeviceWorkspaceInfo `json:"workspace"`
	SourceRevision    int64               `json:"source_revision"`
	State             workspace.CopyState `json:"state"`
	Replayed          bool                `json:"replayed"`
}

// WorkspaceCopyReservationRequest records source and destination ownership
// independently. A non-empty ShareID is accepted only for a live claim by
// TargetOwnerID and binds its target atomically with this reservation.
type WorkspaceCopyReservationRequest struct {
	ID                string
	SourceOwnerID     string
	TargetOwnerID     string
	ShareID           string
	SourceWorkspaceID string
	TargetWorkspaceID string
	TargetName        string
	ServiceInstanceID string
	SourceRevision    int64
	ReservedBytes     int64
	MaxOwnerBytes     int64
	TargetQuotaBytes  int64
	ReadOnly          bool
	Idempotency       IdempotencyRecord
}

type WorkspaceCopyStore interface {
	CreateWorkspaceCopyReservation(context.Context, WorkspaceCopyReservationRequest) (workspace.CopyReservation, bool, error)
	GetWorkspaceCopyReservationByIdempotency(context.Context, IdempotencyRecord, string, string, int64) (workspace.CopyReservation, bool, error)
	GetWorkspaceManifestPageForCopy(context.Context, string, string, int64, string, string, int32) (workspace.ManifestPage, error)
	TryWorkspaceCopyLock(context.Context, string) (LeaderLock, bool, error)
	SetWorkspaceCopyReservationState(context.Context, string, workspace.CopyState, workspace.CopyState) error
	FailWorkspaceCopyReservationBeforeSideEffects(context.Context, string, string) error
	CheckWorkspaceVolumeCopyReservation(context.Context, WorkspaceVolumeCopyRequest) error
	StageWorkspaceCopy(context.Context, string, string, string, []workspace.File, []string, []workspace.Directory, []WorkspaceBlockAllocation, int64) error
	CompleteWorkspaceCopy(context.Context, string, string, int64) error
}

type WorkspaceCopyMaterializer func(context.Context, string, string, int64, []workspace.File, []string, []workspace.Directory) error

type WorkspaceCopyService struct {
	store               WorkspaceCopyStore
	sync                *WorkspaceSyncService
	blocks              WorkspaceBlockStore
	volumes             WorkspaceVolumeProvisioner
	capacity            WorkspaceVolumeCopyCapacityChecker
	preflight           WorkspaceVolumeCopyPreflight
	copier              WorkspaceVolumeCopier
	materialize         WorkspaceCopyMaterializer
	instanceID          string
	maxOwnerBytes       int64
	workspaceQuotaBytes int64
}

func NewWorkspaceCopyService(store WorkspaceCopyStore, syncService *WorkspaceSyncService, blocks WorkspaceBlockStore,
	volumes WorkspaceVolumeProvisioner, copier WorkspaceVolumeCopier, instanceID string, maxOwnerBytes, workspaceQuotaBytes int64) (*WorkspaceCopyService, error) {
	if store == nil || syncService == nil || blocks == nil || volumes == nil || copier == nil ||
		strings.TrimSpace(instanceID) == "" || len(instanceID) > 128 || maxOwnerBytes <= 0 || workspaceQuotaBytes <= 0 {
		return nil, errors.New("workspace copy dependencies, instance, and quotas are required")
	}
	capacity, ok := volumes.(WorkspaceVolumeCopyCapacityChecker)
	if !ok {
		return nil, errors.New("workspace volume copy capacity checker is required")
	}
	preflight, ok := copier.(WorkspaceVolumeCopyPreflight)
	if !ok {
		return nil, errors.New("workspace volume copy preflight is required")
	}
	return &WorkspaceCopyService{
		store: store, sync: syncService, blocks: blocks, volumes: volumes, capacity: capacity, preflight: preflight, copier: copier,
		instanceID: instanceID, maxOwnerBytes: maxOwnerBytes, workspaceQuotaBytes: workspaceQuotaBytes,
	}, nil
}

func (s *WorkspaceCopyService) SetCurrentMaterializer(materializer WorkspaceCopyMaterializer) error {
	if s == nil || materializer == nil {
		return errors.New("workspace copy materializer is required")
	}
	s.materialize = materializer
	return nil
}

func (s *WorkspaceCopyService) Copy(ctx context.Context, principal identity.Principal, sourceWorkspaceID string,
	command WorkspaceCopyCommand, idempotencyKey string) (WorkspaceCopyResult, error) {
	return s.copy(ctx, principal, principal.UserID, principal.UserID, "", false, sourceWorkspaceID, command, idempotencyKey)
}

// copyForWorkspaceShare creates a recipient-owned copy from the immutable
// source checkpoint represented by a validated, in-progress share claim.
func (s *WorkspaceCopyService) copyForWorkspaceShare(ctx context.Context, principal identity.Principal,
	share workspace.ShareLink) (WorkspaceCopyResult, error) {
	if err := requireWorkspaceShareSession(principal); err != nil {
		return WorkspaceCopyResult{}, err
	}
	if err := share.Validate(); err != nil || share.State != workspace.ShareClaiming || share.ClaimantID != principal.UserID ||
		share.OwnerID == principal.UserID || share.TargetWorkspaceID != "" && !domain.ValidWorkspaceID(share.TargetWorkspaceID) {
		return WorkspaceCopyResult{}, domain.NewError(domain.CodeNotFound, "Workspace share link not found")
	}
	if share.Permission != workspace.ShareView && share.Permission != workspace.ShareEdit {
		return WorkspaceCopyResult{}, domain.NewError(domain.CodeInternal, "Workspace share permission is invalid")
	}
	return s.copy(ctx, principal, share.OwnerID, principal.UserID, share.ID, share.Permission == workspace.ShareView,
		share.SourceWorkspaceID, WorkspaceCopyCommand{Name: share.WorkspaceName, ExpectedCurrentRevision: share.SourceRevision},
		"ws-share-copy-"+share.ID)
}

func (s *WorkspaceCopyService) copy(ctx context.Context, principal identity.Principal, sourceOwnerID, targetOwnerID, shareID string,
	readOnly bool, sourceWorkspaceID string, command WorkspaceCopyCommand, idempotencyKey string) (WorkspaceCopyResult, error) {
	if s == nil || s.store == nil || s.sync == nil || s.blocks == nil || s.materialize == nil {
		return WorkspaceCopyResult{}, domain.NewError(domain.CodeWorkspaceUnavailable, "Workspace copy service is unavailable")
	}
	if principal.Kind != identity.KindSession {
		return WorkspaceCopyResult{}, domain.NewError(domain.CodeForbidden, "Workspace copies require a Portal session")
	}
	if err := validatePrincipal(principal); err != nil {
		return WorkspaceCopyResult{}, err
	}
	if !domain.ValidUserID(sourceOwnerID) || !domain.ValidUserID(targetOwnerID) || targetOwnerID != principal.UserID ||
		(shareID == "" && (sourceOwnerID != targetOwnerID || readOnly)) || (shareID != "" && sourceOwnerID == targetOwnerID) {
		return WorkspaceCopyResult{}, domain.InvalidArgument("workspace_copy", "Workspace copy ownership is invalid")
	}
	sourcePrincipal := principal
	if sourceOwnerID != principal.UserID {
		sourcePrincipal = identity.Principal{UserID: sourceOwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	}
	command.Name = strings.TrimSpace(command.Name)
	if !domain.ValidWorkspaceID(sourceWorkspaceID) || command.Name == "" || utf8.RuneCountInString(command.Name) > 120 ||
		!utf8.ValidString(command.Name) || command.ExpectedCurrentRevision < 0 || validateIdempotencyKey(idempotencyKey) != nil {
		return WorkspaceCopyResult{}, domain.InvalidArgument("workspace_copy", "Workspace copy request is invalid")
	}
	var err error
	var requestHash []byte
	if shareID == "" {
		requestHash, err = json.Marshal(struct {
			SourceWorkspaceID string
			Name              string
			ExpectedRevision  int64
		}{sourceWorkspaceID, command.Name, command.ExpectedCurrentRevision})
	} else {
		requestHash, err = json.Marshal(struct {
			ShareID           string
			SourceOwnerID     string
			TargetOwnerID     string
			SourceWorkspaceID string
			Name              string
			ExpectedRevision  int64
			ReadOnly          bool
		}{shareID, sourceOwnerID, targetOwnerID, sourceWorkspaceID, command.Name, command.ExpectedCurrentRevision, readOnly})
	}
	if err != nil {
		return WorkspaceCopyResult{}, domain.NewError(domain.CodeInternal, "Workspace copy request cannot be encoded")
	}
	digest := sha256.Sum256(requestHash)
	operation := "workspace.copy"
	if shareID != "" {
		operation = "workspace.share.copy"
	}
	record := IdempotencyRecord{OwnerID: targetOwnerID, Operation: operation, Key: idempotencyKey,
		RequestHash: digest[:], ResourceID: sourceWorkspaceID}
	reservation, found, err := s.store.GetWorkspaceCopyReservationByIdempotency(ctx, record, command.Name, s.instanceID, s.workspaceQuotaBytes)
	if err != nil {
		return WorkspaceCopyResult{}, err
	}
	if found && (reservation.SourceOwnerID != sourceOwnerID || reservation.TargetOwnerID != targetOwnerID ||
		reservation.ShareID != shareID || reservation.ReadOnly != readOnly) {
		return WorkspaceCopyResult{}, domain.NewError(domain.CodeIdempotencyConflict, "Workspace copy reservation ownership does not match the original request")
	}
	if found && reservation.State == workspace.CopyCompleted {
		return workspaceCopyResult(sourceWorkspaceID, command.Name, reservation, true), nil
	}
	if found && reservation.State == workspace.CopyFailed {
		return WorkspaceCopyResult{}, domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy previously failed and must use a new idempotency key")
	}
	var source workspace.Workspace
	var files []workspace.File
	var directories []string
	var directoryMetadata []workspace.Directory
	var blockSizes map[string]int64
	var replayed bool
	if found {
		if reservation.SourceRevision != command.ExpectedCurrentRevision {
			return WorkspaceCopyResult{}, domain.NewError(domain.CodeIdempotencyConflict, "Workspace copy reservation does not match the requested revision")
		}
		lock, acquired, err := s.store.TryWorkspaceCopyLock(ctx, reservation.ID)
		if err != nil {
			return WorkspaceCopyResult{}, err
		}
		if !acquired {
			return WorkspaceCopyResult{}, domain.NewError(domain.CodeWorkspaceBusy, "Workspace copy is already running")
		}
		defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()
		source, err = s.sync.authorize(ctx, sourcePrincipal, sourceWorkspaceID)
		if err != nil {
			return WorkspaceCopyResult{}, err
		}
		if source.ReadOnly {
			return WorkspaceCopyResult{}, domain.NewError(domain.CodeForbidden, "workspace is shared as read-only")
		}
		if source.Revision != reservation.SourceRevision || source.State != workspace.StateReady {
			return WorkspaceCopyResult{}, domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy source checkpoint changed")
		}
		files, directories, directoryMetadata, err = s.copyReservationManifest(ctx, reservation, sourceOwnerID)
		if err != nil {
			return WorkspaceCopyResult{}, err
		}
		reservedBytes, sizes, err := s.copyBlockReservations(files)
		if err != nil {
			return WorkspaceCopyResult{}, err
		}
		if reservedBytes != reservation.ReservedBytes {
			return WorkspaceCopyResult{}, domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy block reservation does not match its checkpoint")
		}
		blockSizes, replayed = sizes, true
	} else {
		source, err = s.sync.authorize(ctx, sourcePrincipal, sourceWorkspaceID)
		if err != nil {
			return WorkspaceCopyResult{}, err
		}
		if source.ReadOnly {
			return WorkspaceCopyResult{}, domain.NewError(domain.CodeForbidden, "workspace is shared as read-only")
		}
		if source.Revision != command.ExpectedCurrentRevision || source.State != workspace.StateReady {
			return WorkspaceCopyResult{}, domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed before copy; refresh and retry")
		}
		files, directories, directoryMetadata, err = s.copyManifest(ctx, sourcePrincipal, sourceWorkspaceID, command.ExpectedCurrentRevision)
		if err != nil {
			return WorkspaceCopyResult{}, err
		}
		reservedBytes, sizes, err := s.copyBlockReservations(files)
		if err != nil {
			return WorkspaceCopyResult{}, err
		}
		blockSizes = sizes
		targetWorkspaceID, err := domain.NewWorkspaceID()
		if err != nil {
			return WorkspaceCopyResult{}, fmt.Errorf("generate copied Workspace ID: %w", err)
		}
		reservationID, err := domain.NewPendingRequestID()
		if err != nil {
			return WorkspaceCopyResult{}, fmt.Errorf("generate Workspace copy reservation ID: %w", err)
		}
		reservation, replayed, err = s.store.CreateWorkspaceCopyReservation(ctx, WorkspaceCopyReservationRequest{
			ID: reservationID, SourceOwnerID: sourceOwnerID, TargetOwnerID: targetOwnerID, ShareID: shareID,
			SourceWorkspaceID: sourceWorkspaceID, TargetWorkspaceID: targetWorkspaceID, TargetName: command.Name,
			ServiceInstanceID: s.instanceID, SourceRevision: command.ExpectedCurrentRevision, ReservedBytes: reservedBytes,
			MaxOwnerBytes: s.maxOwnerBytes, TargetQuotaBytes: s.workspaceQuotaBytes, ReadOnly: readOnly, Idempotency: record,
		})
		if err != nil {
			return WorkspaceCopyResult{}, err
		}
		if reservation.State == workspace.CopyCompleted {
			return workspaceCopyResult(sourceWorkspaceID, command.Name, reservation, true), nil
		}
		if reservation.State == workspace.CopyFailed {
			return WorkspaceCopyResult{}, domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy previously failed and must use a new idempotency key")
		}
		if replayed && reservation.State != workspace.CopyReserved {
			lock, acquired, err := s.store.TryWorkspaceCopyLock(ctx, reservation.ID)
			if err != nil {
				return WorkspaceCopyResult{}, err
			}
			if !acquired {
				return WorkspaceCopyResult{}, domain.NewError(domain.CodeWorkspaceBusy, "Workspace copy is already running")
			}
			defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()
			files, directories, directoryMetadata, err = s.copyReservationManifest(ctx, reservation, sourceOwnerID)
			if err != nil {
				return WorkspaceCopyResult{}, err
			}
			reservedBytes, sizes, err = s.copyBlockReservations(files)
			if err != nil {
				return WorkspaceCopyResult{}, err
			}
			if reservedBytes != reservation.ReservedBytes {
				return WorkspaceCopyResult{}, domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy block reservation does not match its checkpoint")
			}
			blockSizes = sizes
		} else {
			lock, acquired, err := s.store.TryWorkspaceCopyLock(ctx, reservation.ID)
			if err != nil {
				return WorkspaceCopyResult{}, err
			}
			if !acquired {
				return WorkspaceCopyResult{}, domain.NewError(domain.CodeWorkspaceBusy, "Workspace copy is already running")
			}
			defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()
		}
	}
	wasReserved := reservation.State == workspace.CopyReserved
	volumeBytes, err := workspaceCopyFileBytes(files)
	if err != nil {
		if wasReserved {
			err = errors.Join(err, s.store.FailWorkspaceCopyReservationBeforeSideEffects(ctx, targetOwnerID, reservation.ID))
		}
		return WorkspaceCopyResult{}, err
	}
	if volumeBytes != source.UsedBytes || volumeBytes > s.workspaceQuotaBytes {
		var err error = domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy manifest totals do not match its current checkpoint")
		if wasReserved {
			err = errors.Join(err, s.store.FailWorkspaceCopyReservationBeforeSideEffects(ctx, targetOwnerID, reservation.ID))
		}
		return WorkspaceCopyResult{}, err
	}
	if s.sync.verifyCurrent == nil {
		if wasReserved {
			if failErr := s.store.FailWorkspaceCopyReservationBeforeSideEffects(ctx, targetOwnerID, reservation.ID); failErr != nil {
				return WorkspaceCopyResult{}, errors.Join(domain.NewError(domain.CodeWorkspaceUnavailable, "Workspace current-tree verifier is unavailable"), failErr)
			}
		} else if reservation.State == workspace.Copying {
			return WorkspaceCopyResult{}, s.copyUnknown(ctx, reservation, domain.NewError(domain.CodeWorkspaceUnavailable, "Workspace current-tree verifier is unavailable"))
		}
		return WorkspaceCopyResult{}, domain.NewError(domain.CodeWorkspaceUnavailable, "Workspace current-tree verifier is unavailable")
	}
	matches, verifyErr := s.sync.verifyCurrent(ctx, sourceOwnerID, sourceWorkspaceID, files, directories, directoryMetadata)
	if verifyErr != nil || !matches {
		cause := errors.Join(domain.NewError(domain.CodeWorkspaceConflict, "Workspace current tree does not match the confirmed checkpoint"), verifyErr)
		if wasReserved {
			if failErr := s.store.FailWorkspaceCopyReservationBeforeSideEffects(ctx, targetOwnerID, reservation.ID); failErr != nil {
				return WorkspaceCopyResult{}, errors.Join(cause, failErr)
			}
		} else if reservation.State == workspace.Copying {
			return WorkspaceCopyResult{}, s.copyUnknown(ctx, reservation, cause)
		}
		return WorkspaceCopyResult{}, cause
	}
	if wasReserved {
		if err := s.capacity.CheckCapacityForWrite(ctx, volumeBytes); err != nil {
			if failErr := s.store.FailWorkspaceCopyReservationBeforeSideEffects(ctx, targetOwnerID, reservation.ID); failErr != nil {
				return WorkspaceCopyResult{}, errors.Join(err, failErr)
			}
			return WorkspaceCopyResult{}, err
		}
	}
	request := WorkspaceVolumeCopyRequest{
		ReservationID: reservation.ID, ServiceInstanceID: s.instanceID, SourceOwnerID: sourceOwnerID, TargetOwnerID: targetOwnerID,
		SourceWorkspaceID: sourceWorkspaceID, TargetWorkspaceID: reservation.TargetWorkspaceID,
		SourceRevision: reservation.SourceRevision, Files: files, EmptyDirectories: directories, Directories: directoryMetadata,
	}
	if wasReserved {
		if err := s.preflight.ValidateWorkspaceVolumeCopySource(ctx, request, s.store); err != nil {
			if failErr := s.store.FailWorkspaceCopyReservationBeforeSideEffects(ctx, targetOwnerID, reservation.ID); failErr != nil {
				return WorkspaceCopyResult{}, errors.Join(fmt.Errorf("validate Workspace copy source before side effects: %w", err), failErr)
			}
			return WorkspaceCopyResult{}, fmt.Errorf("validate Workspace copy source before side effects: %w", err)
		}
	}
	if wasReserved {
		if err := s.store.SetWorkspaceCopyReservationState(ctx, reservation.ID, workspace.CopyReserved, workspace.Copying); err != nil {
			return WorkspaceCopyResult{}, err
		}
		reservation.State = workspace.Copying
	} else if reservation.State == workspace.CopyUnknown {
		if err := s.store.SetWorkspaceCopyReservationState(ctx, reservation.ID, workspace.CopyUnknown, workspace.Copying); err != nil {
			return WorkspaceCopyResult{}, err
		}
		reservation.State = workspace.Copying
	}
	if err := s.volumes.EnsureWorkspaceVolume(ctx, WorkspaceNamedVolumeName(request.TargetWorkspaceID), s.instanceID, targetOwnerID, request.TargetWorkspaceID); err != nil {
		return WorkspaceCopyResult{}, s.copyUnknown(ctx, reservation, fmt.Errorf("provision copied Workspace volume: %w", err))
	}
	copyFiles, copyDirectories, err := s.copier.CopyWorkspaceVolume(ctx, request, s.store)
	if err != nil {
		return WorkspaceCopyResult{}, s.copyUnknown(ctx, reservation, err)
	}
	files, err = mergeWorkspaceCopyFileMetadata(files, copyFiles)
	if err != nil {
		return WorkspaceCopyResult{}, s.copyUnknown(ctx, reservation, err)
	}
	if err := validateWorkspaceCopyDirectoryMetadata(files, directories, directoryMetadata, copyDirectories); err != nil {
		return WorkspaceCopyResult{}, s.copyUnknown(ctx, reservation, err)
	}
	allocations, err := s.copyBlocks(ctx, reservation, blockSizes)
	if err != nil {
		return WorkspaceCopyResult{}, s.copyUnknown(ctx, reservation, err)
	}
	if err := s.store.StageWorkspaceCopy(ctx, reservation.ID, targetOwnerID, command.Name, files, directories, copyDirectories, allocations, s.workspaceQuotaBytes); err != nil {
		return WorkspaceCopyResult{}, s.copyUnknown(ctx, reservation, err)
	}
	if err := s.materialize(ctx, targetOwnerID, reservation.TargetWorkspaceID, 1, files, directories, copyDirectories); err != nil {
		return WorkspaceCopyResult{}, s.copyUnknown(ctx, reservation, fmt.Errorf("materialize copied Workspace current tree: %w", err))
	}
	if err := s.store.CompleteWorkspaceCopy(ctx, reservation.ID, targetOwnerID, s.maxOwnerBytes); err != nil {
		return WorkspaceCopyResult{}, s.copyUnknown(ctx, reservation, err)
	}
	return WorkspaceCopyResult{
		SourceWorkspaceID: sourceWorkspaceID,
		Workspace:         DeviceWorkspaceInfo{ID: reservation.TargetWorkspaceID, Name: command.Name, Revision: 1, State: workspace.StateReady},
		SourceRevision:    reservation.SourceRevision, State: workspace.CopyCompleted, Replayed: replayed,
	}, nil
}

func workspaceCopyResult(sourceWorkspaceID, name string, reservation workspace.CopyReservation, replayed bool) WorkspaceCopyResult {
	return WorkspaceCopyResult{
		SourceWorkspaceID: sourceWorkspaceID,
		Workspace:         DeviceWorkspaceInfo{ID: reservation.TargetWorkspaceID, Name: name, Revision: 1, State: workspace.StateReady},
		SourceRevision:    reservation.SourceRevision, State: reservation.State, Replayed: replayed,
	}
}

func workspaceCopyFileBytes(files []workspace.File) (int64, error) {
	var total int64
	for _, file := range files {
		if err := workspace.ValidateFile(file); err != nil || total > math.MaxInt64-file.SizeBytes {
			return 0, domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy manifest contains an invalid size")
		}
		total += file.SizeBytes
	}
	return total, nil
}

func (s *WorkspaceCopyService) copyManifest(ctx context.Context, principal identity.Principal, workspaceID string, revision int64) ([]workspace.File, []string, []workspace.Directory, error) {
	return s.sync.readManifest(ctx, principal.UserID, workspaceID, revision, nil)
}

func (s *WorkspaceCopyService) copyReservationManifest(ctx context.Context, reservation workspace.CopyReservation, ownerID string) ([]workspace.File, []string, []workspace.Directory, error) {
	return s.sync.readManifestUsing(ctx, ownerID, reservation.SourceWorkspaceID, reservation.SourceRevision, nil,
		func(ctx context.Context, ownerID, workspaceID string, revision int64, prefix, after string, limit int32) (workspace.ManifestPage, error) {
			return s.store.GetWorkspaceManifestPageForCopy(ctx, ownerID, reservation.ID, revision, prefix, after, limit)
		})
}

func validateWorkspaceCopyDirectoryMetadata(files []workspace.File, emptyDirectories []string, expectedMetadata, directories []workspace.Directory) error {
	expected := make(map[string]string)
	expected["."] = "."
	add := func(path string, includeSelf bool) {
		parts := strings.Split(path, "/")
		limit := len(parts)
		if !includeSelf {
			limit--
		}
		prefix := ""
		for i := 0; i < limit; i++ {
			if i > 0 {
				prefix += "/"
			}
			prefix += parts[i]
			expected[strings.ToLower(prefix)] = prefix
		}
	}
	for _, file := range files {
		if err := workspace.ValidateFile(file); err != nil {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy contains an invalid file")
		}
		add(file.Path, false)
	}
	for _, directory := range emptyDirectories {
		if err := workspace.ValidateRelativePath(directory); err != nil {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy contains an invalid directory")
		}
		add(directory, true)
	}
	wantedMetadata := make(map[string]workspace.Directory, len(expectedMetadata))
	for _, directory := range expectedMetadata {
		if workspace.ValidateDirectory(directory, false) != nil || !directory.MetadataKnown {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy source metadata is incomplete")
		}
		key := strings.ToLower(directory.Path)
		if _, exists := wantedMetadata[key]; exists {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy source metadata contains duplicate directories")
		}
		wantedMetadata[key] = directory
	}
	if len(wantedMetadata) != len(expected) {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy source metadata is incomplete")
	}
	seen := make(map[string]struct{}, len(directories))
	for _, directory := range directories {
		if err := workspace.ValidateDirectory(directory, true); err != nil || !directory.MetadataKnown {
			return domain.NewError(domain.CodeWorkspaceConflict, "copied Workspace directory metadata is incomplete")
		}
		key := strings.ToLower(directory.Path)
		if _, exists := seen[key]; exists {
			return domain.NewError(domain.CodeWorkspaceConflict, "copied Workspace contains duplicate directory metadata")
		}
		if expected[key] != directory.Path {
			return domain.NewError(domain.CodeWorkspaceConflict, "copied Workspace directory metadata does not match its tree")
		}
		wanted, exists := wantedMetadata[key]
		if !exists || !workspace.SameNativeMetadata(directory.Xattrs, wanted.Xattrs, directory.ACL, wanted.ACL) ||
			wanted.Mode != nil && *wanted.Mode != *directory.Mode || wanted.ModTimeNS != nil && *wanted.ModTimeNS != *directory.ModTimeNS {
			return domain.NewError(domain.CodeWorkspaceConflict, "copied Workspace directory metadata does not match its checkpoint")
		}
		seen[key] = struct{}{}
	}
	if len(seen) != len(expected) {
		return domain.NewError(domain.CodeWorkspaceConflict, "copied Workspace directory metadata is incomplete")
	}
	return nil
}

func mergeWorkspaceCopyFileMetadata(expected, copied []workspace.File) ([]workspace.File, error) {
	if len(expected) != len(copied) {
		return nil, domain.NewError(domain.CodeWorkspaceConflict, "copied Workspace file metadata is incomplete")
	}
	result := append([]workspace.File(nil), expected...)
	for index, source := range expected {
		actual := copied[index]
		if workspace.ValidateFile(actual) != nil || !source.MetadataKnown || !actual.MetadataKnown ||
			!workspace.SameNativeMetadata(source.Xattrs, actual.Xattrs, source.ACL, actual.ACL) || actual.Path != source.Path || actual.SizeBytes != source.SizeBytes ||
			actual.SHA256 != source.SHA256 || actual.Mode != source.Mode || workspaceCopyFileKind(actual) != workspaceCopyFileKind(source) ||
			actual.SymlinkTarget != source.SymlinkTarget || actual.HardlinkTo != source.HardlinkTo ||
			len(actual.Chunks) != len(source.Chunks) || actual.ModTimeNS == nil {
			return nil, domain.NewError(domain.CodeWorkspaceConflict, "copied Workspace file metadata does not match its tree")
		}
		for chunkIndex, chunk := range source.Chunks {
			if actual.Chunks[chunkIndex] != chunk {
				return nil, domain.NewError(domain.CodeWorkspaceConflict, "copied Workspace file metadata does not match its tree")
			}
		}
		if source.ModTimeNS != nil && *source.ModTimeNS != *actual.ModTimeNS {
			return nil, domain.NewError(domain.CodeWorkspaceConflict, "copied Workspace file timestamp changed")
		}
		modified := *actual.ModTimeNS
		result[index].ModTimeNS = &modified
	}
	return result, nil
}

func workspaceCopyFileKind(file workspace.File) string {
	if file.Kind == "" {
		return "file"
	}
	return file.Kind
}

func (s *WorkspaceCopyService) copyBlockReservations(files []workspace.File) (int64, map[string]int64, error) {
	blockSizes := make(map[string]int64)
	for _, file := range files {
		if err := workspace.ValidateFile(file); err != nil {
			return 0, nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy manifest contains an invalid file")
		}
		for _, chunk := range file.Chunks {
			if prior, exists := blockSizes[chunk.SHA256]; exists && prior != chunk.SizeBytes {
				return 0, nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy manifest contains conflicting blocks")
			}
			blockSizes[chunk.SHA256] = chunk.SizeBytes
		}
	}
	digests := make([]string, 0, len(blockSizes))
	for digest := range blockSizes {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	var reservedBytes int64
	for _, digest := range digests {
		reserved, err := s.blocks.ReservationBytes(blockSizes[digest])
		if err != nil || reserved < blockSizes[digest] {
			return 0, nil, errors.Join(domain.NewError(domain.CodeWorkspaceUnavailable, "Workspace copy storage capacity cannot be reserved"), err)
		}
		if reservedBytes > math.MaxInt64-reserved {
			return 0, nil, domain.NewError(domain.CodeQuotaExceeded, "Workspace copy capacity exceeds storage limits")
		}
		reservedBytes += reserved
	}
	return reservedBytes, blockSizes, nil
}

func (s *WorkspaceCopyService) copyBlocks(ctx context.Context, reservation workspace.CopyReservation, blockSizes map[string]int64) ([]WorkspaceBlockAllocation, error) {
	digests := make([]string, 0, len(blockSizes))
	for digest := range blockSizes {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	allocations := make([]WorkspaceBlockAllocation, 0, len(digests))
	for _, digest := range digests {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reader, err := s.blocks.Open(reservation.SourceWorkspaceID, digest)
		if err != nil {
			return nil, fmt.Errorf("open source Workspace block %q: %w", digest, err)
		}
		written, writeErr := s.blocks.PutBlock(reservation.TargetWorkspaceID, digest, reader)
		closeErr := reader.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return nil, fmt.Errorf("copy Workspace block %q: %w", digest, err)
		}
		if written != blockSizes[digest] {
			return nil, domain.NewError(domain.CodeWorkspaceConflict, "copied Workspace block size does not match its manifest")
		}
		allocated, err := s.blocks.BlockAllocatedBytes(reservation.TargetWorkspaceID, digest)
		if err != nil {
			return nil, fmt.Errorf("measure copied Workspace block %q: %w", digest, err)
		}
		allocations = append(allocations, WorkspaceBlockAllocation{WorkspaceID: reservation.TargetWorkspaceID, Digest: digest, AllocatedBytes: allocated})
	}
	return allocations, nil
}

func (s *WorkspaceCopyService) copyUnknown(ctx context.Context, reservation workspace.CopyReservation, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.store.SetWorkspaceCopyReservationState(cleanupCtx, reservation.ID, workspace.Copying, workspace.CopyUnknown); err != nil {
		return errors.Join(cause, fmt.Errorf("keep Workspace copy fenced after an uncertain result: %w", err))
	}
	return cause
}
