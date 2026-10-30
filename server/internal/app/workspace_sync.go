package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

const (
	manifestPageLimit    int32 = 1000
	changesDefaultLimit  int32 = 200
	changesMaxLimit      int32 = 500
	manifestDefaultLimit int32 = 500
	manifestMaxLimit     int32 = 1000
	maxSyncWait                = 25 * time.Second
	syncPollInterval           = 300 * time.Millisecond
)

var syncIdempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

type WorkspaceBlockReader interface {
	Open(workspaceID, digest string) (io.ReadCloser, error)
}

type WorkspaceBlockStore interface {
	WorkspaceBlockReader
	PutBlock(workspaceID, digest string, src io.Reader) (int64, error)
	ReservationBytes(size int64) (int64, error)
	BlockAllocatedBytes(workspaceID, digest string) (int64, error)
}

type WorkspaceSyncService struct {
	store            WorkspaceSyncStore
	blocks           WorkspaceBlockStore
	maxManifestBytes int64
	maxOwnerBytes    int64
}

func NewWorkspaceSyncService(store WorkspaceSyncStore, blocks WorkspaceBlockStore, maxManifestBytes, maxOwnerBytes int64) (*WorkspaceSyncService, error) {
	if store == nil || blocks == nil || maxOwnerBytes <= 0 {
		return nil, errors.New("workspace synchronization dependencies and owner quota are required")
	}
	if maxManifestBytes <= 0 || maxManifestBytes > workspace.MaxSyncManifestBytes {
		maxManifestBytes = workspace.MaxSyncManifestBytes
	}
	return &WorkspaceSyncService{store: store, blocks: blocks, maxManifestBytes: maxManifestBytes, maxOwnerBytes: maxOwnerBytes}, nil
}

func (s *WorkspaceSyncService) GetManifest(ctx context.Context, principal identity.Principal, workspaceID string, revision int64, prefix, cursor string, limit int32) (workspace.ManifestPage, error) {
	value, err := s.authorize(ctx, principal, workspaceID)
	if err != nil {
		return workspace.ManifestPage{}, err
	}
	if revision < 0 || len(prefix) > 1024 {
		return workspace.ManifestPage{}, domain.InvalidArgument("manifest", "manifest revision or prefix is invalid")
	}
	if prefix != "" {
		prefix = strings.TrimSuffix(prefix, "/")
		if err := workspace.ValidateRelativePath(prefix); err != nil {
			return workspace.ManifestPage{}, domain.InvalidArgument("prefix", "manifest prefix must be a normalized relative path")
		}
	}
	if limit == 0 {
		limit = manifestDefaultLimit
	}
	if limit < 1 || limit > manifestMaxLimit {
		return workspace.ManifestPage{}, domain.InvalidArgument("limit", "manifest page limit is out of range")
	}
	if cursor != "" {
		parsed, err := workspace.DecodeManifestCursor(cursor)
		if err != nil {
			return workspace.ManifestPage{}, domain.InvalidArgument("cursor", "manifest cursor is invalid")
		}
		if revision == 0 {
			revision = parsed.Revision
		}
		if parsed.Revision != revision || parsed.Prefix != prefix {
			return workspace.ManifestPage{}, domain.InvalidArgument("cursor", "manifest cursor does not match the requested snapshot")
		}
		if principal.Kind == identity.KindHostReplica {
			binding, err := s.store.GetWorkspaceBinding(ctx, principal.HostScope, workspaceID)
			if err != nil {
				return workspace.ManifestPage{}, err
			}
			if binding.DesiredRevision != value.Revision || revision != binding.DesiredRevision {
				return workspace.ManifestPage{}, domain.NewError(domain.CodeWorkspaceConflict, "Host binding does not match the requested workspace revision")
			}
		}
		return s.store.GetWorkspaceManifestPage(ctx, workspaceID, revision, prefix, parsed.After, limit)
	}
	if revision == 0 {
		revision = value.Revision
	}
	if principal.Kind == identity.KindHostReplica {
		binding, err := s.store.GetWorkspaceBinding(ctx, principal.HostScope, workspaceID)
		if err != nil {
			return workspace.ManifestPage{}, err
		}
		if binding.DesiredRevision != value.Revision || revision != binding.DesiredRevision {
			return workspace.ManifestPage{}, domain.NewError(domain.CodeWorkspaceConflict, "Host binding does not match the requested workspace revision")
		}
	}
	if revision > value.Revision {
		return workspace.ManifestPage{}, domain.NewError(domain.CodeWorkspaceConflict, "requested workspace revision is not available")
	}
	return s.store.GetWorkspaceManifestPage(ctx, workspaceID, revision, prefix, "", limit)
}

func (s *WorkspaceSyncService) GetChanges(ctx context.Context, principal identity.Principal, workspaceID, after string, wait time.Duration, limit int32) (workspace.ChangePage, error) {
	value, err := s.authorize(ctx, principal, workspaceID)
	if err != nil {
		return workspace.ChangePage{}, err
	}
	if principal.Kind == identity.KindHostReplica {
		binding, err := s.store.GetWorkspaceBinding(ctx, principal.HostScope, workspaceID)
		if err != nil {
			return workspace.ChangePage{}, err
		}
		if (binding.State != workspace.BindingActive && binding.State != workspace.BindingUnbinding) || binding.DesiredRevision != value.Revision {
			return workspace.ChangePage{}, domain.NewError(domain.CodeWorkspaceBusy, "Host workspace binding is not active at the current revision")
		}
	}
	if after == "" || len(after) > 512 {
		return workspace.ChangePage{}, domain.InvalidArgument("after", "workspace changes cursor is required")
	}
	cursor, err := workspace.DecodeChangeCursor(after)
	if err != nil {
		return workspace.ChangePage{}, domain.InvalidArgument("after", "workspace changes cursor is invalid")
	}
	if cursor.Revision > value.Revision {
		return workspace.ChangePage{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace changes cursor is ahead of the current revision")
	}
	if wait < 0 || wait > maxSyncWait {
		return workspace.ChangePage{}, domain.InvalidArgument("wait_seconds", "workspace changes wait is out of range")
	}
	if limit == 0 {
		limit = changesDefaultLimit
	}
	if limit < 1 || limit > changesMaxLimit {
		return workspace.ChangePage{}, domain.InvalidArgument("limit", "workspace changes page limit is out of range")
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		if principal.Kind == identity.KindDevice {
			if err := s.store.CheckWorkspaceSyncDeviceAccess(ctx, principal, workspaceID); err != nil {
				return workspace.ChangePage{}, err
			}
		}
		page, err := s.store.GetWorkspaceChangePage(ctx, workspaceID, cursor.Revision, cursor.Path, limit)
		if err != nil {
			return workspace.ChangePage{}, err
		}
		if len(page.Items) > 0 || wait == 0 {
			return page, nil
		}
		ticker := time.NewTimer(syncPollInterval)
		select {
		case <-ctx.Done():
			ticker.Stop()
			return workspace.ChangePage{}, ctx.Err()
		case <-deadline.C:
			ticker.Stop()
			return page, nil
		case <-ticker.C:
			current, err := s.store.GetWorkspace(ctx, workspaceID)
			if err != nil {
				return workspace.ChangePage{}, err
			}
			if current.State != workspace.StateReady || current.OwnerID != value.OwnerID {
				return workspace.ChangePage{}, domain.NewError(domain.CodeNotFound, "workspace not found")
			}
		}
	}
}

func (s *WorkspaceSyncService) CreateSyncSession(ctx context.Context, principal identity.Principal, workspaceID string, command workspace.CreateSyncSession, idempotencyKey string) (result workspace.SyncSession, retErr error) {
	value, err := s.authorize(ctx, principal, workspaceID)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	if value.State != workspace.StateReady {
		return workspace.SyncSession{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace is not available for synchronization")
	}
	if principal.Kind == identity.KindHostReplica {
		binding, err := s.store.GetWorkspaceBinding(ctx, principal.HostScope, workspaceID)
		if err != nil {
			return workspace.SyncSession{}, err
		}
		if binding.DesiredRevision != value.Revision {
			return workspace.SyncSession{}, domain.NewError(domain.CodeWorkspaceConflict, "Host binding has not observed the current workspace revision")
		}
		switch binding.State {
		case workspace.BindingPreparing:
			if command.Direction != workspace.SyncDownload || command.Policy != workspace.SyncFillMissing || command.BaseRevision != 0 ||
				len(command.DeletedPaths) != 0 || command.AckCursor != nil || len(command.Scope) != 0 {
				return workspace.SyncSession{}, domain.NewError(domain.CodeForbidden, "preparing Host replicas may only download from their last applied revision")
			}
		case workspace.BindingActive:
			safeDownload := command.Direction == workspace.SyncDownload && command.Policy == workspace.SyncFillMissing && command.BaseRevision == 0 &&
				len(command.DeletedPaths) == 0 && command.AckCursor == nil && len(command.Scope) == 0
			liveSync := command.Direction == workspace.SyncBidirectional &&
				(command.Policy == workspace.SyncLive || command.Policy == workspace.SyncFillMissing) &&
				(len(command.DeletedPaths) == 0 || command.Policy == workspace.SyncLive)
			if !safeDownload && !liveSync {
				return workspace.SyncSession{}, domain.NewError(domain.CodeForbidden, "active Host replicas require live bidirectional synchronization")
			}
		case workspace.BindingUnbinding:
			liveCheckpoint := command.Direction == workspace.SyncBidirectional && command.Policy == workspace.SyncLive
			initialCheckpoint := command.Direction == workspace.SyncBidirectional && command.Policy == workspace.SyncFillMissing && len(command.DeletedPaths) == 0
			if !liveCheckpoint && !initialCheckpoint {
				return workspace.SyncSession{}, domain.NewError(domain.CodeForbidden, "unbinding Host replicas require a live or non-destructive initial checkpoint")
			}
		default:
			return workspace.SyncSession{}, domain.NewError(domain.CodeWorkspaceBusy, "Host workspace binding is not available for synchronization")
		}
	}
	if err := validateSyncIdempotencyKey(idempotencyKey); err != nil {
		return workspace.SyncSession{}, err
	}
	if err := workspace.ValidateCreateSyncSession(command, 0); err != nil {
		message := err.Error()
		if strings.Contains(message, "16 MiB") || strings.Contains(message, "item limits") {
			return workspace.SyncSession{}, domain.NewError(domain.CodeRequestTooLarge, "workspace sync manifest exceeds its limits")
		}
		return workspace.SyncSession{}, domain.InvalidArgument("sync_session", message)
	}
	encoded, err := json.Marshal(command)
	if err != nil {
		return workspace.SyncSession{}, domain.InvalidArgument("sync_session", "workspace sync manifest cannot be encoded")
	}
	if int64(len(encoded)) > s.maxManifestBytes {
		return workspace.SyncSession{}, domain.NewError(domain.CodeRequestTooLarge, "workspace sync manifest exceeds its byte limit")
	}
	if command.BaseRevision > value.Revision {
		return workspace.SyncSession{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace base revision is ahead of the current revision")
	}
	if command.AckCursor != nil {
		ack, err := workspace.DecodeChangeCursor(*command.AckCursor)
		if err != nil || ack.Revision > value.Revision {
			return workspace.SyncSession{}, domain.InvalidArgument("ack_cursor", "workspace acknowledgement cursor is invalid or ahead of the current revision")
		}
	}
	baseFiles, err := s.readManifest(ctx, workspaceID, command.BaseRevision, command.Scope)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	currentFiles, err := s.readManifest(ctx, workspaceID, value.Revision, command.Scope)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	plan := workspace.PlanSync(baseFiles, currentFiles, command)
	blockLock, err := s.lockWorkspaceBlockStore(ctx, workspaceID, false)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	defer func() {
		if err := blockLock.Release(ctx); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release workspace block store lock: %w", err))
		}
	}()
	transferBlocks, plannedBytes, err := s.prepareTransferPlan(ctx, workspaceID, plan)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	sessionID, err := workspace.NewSyncSessionID()
	if err != nil {
		return workspace.SyncSession{}, fmt.Errorf("generate workspace sync session: %w", err)
	}
	replicaID := syncReplicaID(principal)
	session := workspace.SyncSession{
		ID: sessionID, WorkspaceID: workspaceID, OwnerID: value.OwnerID, ReplicaID: replicaID,
		Direction: command.Direction, Policy: command.Policy, State: workspace.SyncPlanned,
		BaseRevision: command.BaseRevision, TargetRevision: value.Revision, PinnedRevision: value.Revision,
		Stage: workspace.SyncStageCompare, PlannedBytes: plannedBytes, ConfirmedBytes: 0,
		Conflicts: plan.Conflicts,
		Files:     append([]workspace.File(nil), command.Files...), UploadFiles: plan.UploadFiles,
		DownloadFiles: plan.DownloadFiles, Scope: append([]string(nil), command.Scope...),
		DeletedPaths:       append([]string(nil), command.DeletedPaths...),
		UploadDeletedPaths: plan.UploadDeletedPaths, DownloadDeletedPaths: plan.DownloadDeletedPaths,
		DownloadActions: workspace.DownloadActionsForPlan(plan.DownloadFiles, plan.DownloadDeletedPaths),
		AckCursor:       command.AckCursor, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	requestForHash := struct {
		WorkspaceID string                      `json:"workspace_id"`
		Request     workspace.CreateSyncSession `json:"request"`
	}{WorkspaceID: workspaceID, Request: command}
	record, err := idempotencyRecord(value.OwnerID, "workspace.sync.create", idempotencyKey, requestForHash, sessionID, "")
	if err != nil {
		return workspace.SyncSession{}, err
	}
	reservations, err := workspaceBlockReservations(s.blocks, workspaceID, transferBlocks)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	created, _, err := s.store.CreateWorkspaceSyncSession(ctx, session, transferBlocks, reservations, s.maxOwnerBytes, record)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	return created, nil
}

func (s *WorkspaceSyncService) GetSyncSession(ctx context.Context, principal identity.Principal, workspaceID, sessionID string) (workspace.SyncSession, error) {
	value, err := s.authorize(ctx, principal, workspaceID)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	if !workspace.ValidSessionID(sessionID) {
		return workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	session, err := s.store.GetWorkspaceSyncSession(ctx, workspaceID, sessionID)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	if session.OwnerID != value.OwnerID || session.ReplicaID != syncReplicaID(principal) {
		return workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	return session, nil
}

func (s *WorkspaceSyncService) ListDeviceSyncProgress(ctx context.Context, principal identity.Principal, workspaceID string) ([]workspace.DeviceSyncProgress, error) {
	if err := requireSession(principal); err != nil {
		return nil, err
	}
	value, err := s.authorize(ctx, principal, workspaceID)
	if err != nil {
		return nil, err
	}
	return s.store.ListDeviceWorkspaceSyncProgress(ctx, value.OwnerID, workspaceID)
}

func (s *WorkspaceSyncService) CommitSyncSession(ctx context.Context, principal identity.Principal, workspaceID, sessionID, idempotencyKey string) (workspace.SyncSession, error) {
	value, err := s.authorize(ctx, principal, workspaceID)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	if !workspace.ValidSessionID(sessionID) {
		return workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	if err := validateSyncIdempotencyKey(idempotencyKey); err != nil {
		return workspace.SyncSession{}, err
	}
	session, err := s.store.GetWorkspaceSyncSession(ctx, workspaceID, sessionID)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	if session.OwnerID != value.OwnerID || session.ReplicaID != syncReplicaID(principal) {
		return workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	record, err := idempotencyRecord(value.OwnerID, "workspace.sync.commit", idempotencyKey, struct {
		WorkspaceID string `json:"workspace_id"`
		SessionID   string `json:"session_id"`
	}{workspaceID, sessionID}, sessionID, "")
	if err != nil {
		return workspace.SyncSession{}, err
	}
	receipt, err := s.store.ReserveWorkspaceSyncCommit(ctx, workspaceID, sessionID, record)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	switch receipt.State {
	case WorkspaceSyncCommitQuotaRejected:
		return workspace.SyncSession{}, domain.NewError(domain.CodeQuotaExceeded, "workspace storage quota would be exceeded")
	case WorkspaceSyncCommitSucceeded:
		return receipt.Result, nil
	case WorkspaceSyncCommitPending:
	default:
		return workspace.SyncSession{}, domain.NewError(domain.CodeInternal, "workspace sync commit receipt state is invalid")
	}
	if session.State == workspace.SyncCompleted || session.State == workspace.SyncConflict || session.State == workspace.SyncCancelled ||
		session.State == workspace.SyncExpired || session.State == workspace.SyncFailed {
		updated, _, err := s.store.CommitWorkspaceSyncSession(ctx, workspaceID, sessionID, record)
		if err != nil {
			return workspace.SyncSession{}, err
		}
		if updated.State == workspace.SyncExpired || updated.State == workspace.SyncFailed {
			return workspace.SyncSession{}, domain.NewError(domain.CodeTransferExpired, "workspace sync session is no longer active")
		}
		return updated, nil
	}
	if err := s.verifyUploadedFiles(ctx, workspaceID, session); err != nil {
		receipt, lookupErr := s.store.InspectWorkspaceSyncCommit(ctx, workspaceID, sessionID, record)
		if lookupErr != nil {
			return workspace.SyncSession{}, lookupErr
		}
		switch receipt.State {
		case WorkspaceSyncCommitQuotaRejected:
			return workspace.SyncSession{}, domain.NewError(domain.CodeQuotaExceeded, "workspace storage quota would be exceeded")
		case WorkspaceSyncCommitSucceeded:
			return receipt.Result, nil
		case WorkspaceSyncCommitPending:
		default:
			return workspace.SyncSession{}, domain.NewError(domain.CodeInternal, "workspace sync commit receipt state is invalid")
		}
		if domain.CodeOf(err) == domain.CodeWorkspaceConflict {
			_ = s.store.FailWorkspaceSyncSession(ctx, workspaceID, sessionID)
			return workspace.SyncSession{}, err
		}
		if ctx.Err() != nil {
			return workspace.SyncSession{}, ctx.Err()
		}
		if errors.Is(err, errIncompleteWorkspaceSync) || domain.CodeOf(err) == domain.CodeInternal {
			return workspace.SyncSession{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace sync commit is still pending; retry with the same idempotency key")
		}
		return workspace.SyncSession{}, err
	}
	updated, _, err := s.store.CommitWorkspaceSyncSession(ctx, workspaceID, sessionID, record)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	return updated, nil
}

func (s *WorkspaceSyncService) CancelSyncSession(ctx context.Context, principal identity.Principal, workspaceID, sessionID, idempotencyKey string) (workspace.SyncSession, error) {
	value, err := s.authorize(ctx, principal, workspaceID)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	if !workspace.ValidSessionID(sessionID) {
		return workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	if err := validateSyncIdempotencyKey(idempotencyKey); err != nil {
		return workspace.SyncSession{}, err
	}
	session, err := s.store.GetWorkspaceSyncSession(ctx, workspaceID, sessionID)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	if session.OwnerID != value.OwnerID || session.ReplicaID != syncReplicaID(principal) {
		return workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	record, err := idempotencyRecord(value.OwnerID, "workspace.sync.cancel", idempotencyKey, struct {
		WorkspaceID string `json:"workspace_id"`
		SessionID   string `json:"session_id"`
	}{workspaceID, sessionID}, sessionID, "")
	if err != nil {
		return workspace.SyncSession{}, err
	}
	updated, _, err := s.store.CancelWorkspaceSyncSession(ctx, workspaceID, sessionID, record)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	return updated, nil
}

func (s *WorkspaceSyncService) PutSyncBlock(ctx context.Context, principal identity.Principal, workspaceID, sessionID, digest string, body io.Reader) (written int64, retErr error) {
	value, err := s.authorize(ctx, principal, workspaceID)
	if err != nil {
		return 0, err
	}
	if !validSyncDigest(digest) || body == nil {
		return 0, domain.InvalidArgument("block", "workspace sync block is invalid")
	}
	blockLock, err := s.lockWorkspaceBlockStore(ctx, workspaceID, true)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := blockLock.Release(ctx); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release workspace block store read lock: %w", err))
		}
	}()
	session, err := s.authorizeSession(ctx, principal, value, workspaceID, sessionID)
	if err != nil {
		return 0, err
	}
	if session.State != workspace.SyncPlanned && session.State != workspace.SyncTransferring &&
		session.State != workspace.SyncVerifying && session.State != workspace.SyncApplying {
		return 0, domain.NewError(domain.CodeInvalidState, "workspace sync session does not accept uploads")
	}
	blocks, err := s.store.ListWorkspaceSyncTransferBlocks(ctx, workspaceID, sessionID)
	if err != nil {
		return 0, err
	}
	var expected *workspace.TransferBlock
	for i := range blocks {
		if blocks[i].SHA256 == digest && blocks[i].Role == workspace.BlockUpload {
			expected = &blocks[i]
			break
		}
	}
	if expected == nil {
		return 0, domain.NewError(domain.CodeNotFound, "workspace sync block is not required by this session")
	}
	reader := body
	if principal.Kind == identity.KindDevice {
		reader = reauthorizeSyncReader{reader: body, authorize: func() error {
			return s.store.CheckWorkspaceSyncDeviceAccess(ctx, principal, workspaceID)
		}}
	}
	n, err := s.blocks.PutBlock(workspaceID, digest, reader)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "exceeds 4 mib") {
			return 0, domain.NewError(domain.CodeRequestTooLarge, "workspace sync block exceeds 4 MiB")
		}
		return 0, fmt.Errorf("store verified workspace block: %w", err)
	}
	allocatedBytes, err := s.blocks.BlockAllocatedBytes(workspaceID, digest)
	if err != nil {
		return 0, fmt.Errorf("inspect allocated workspace block bytes: %w", err)
	}
	if n != expected.SizeBytes {
		// The block file is already durable. Settle its reservation before
		// rejecting the transfer so disk usage can never become unaccounted.
		if err := s.store.FinalizeWorkspaceBlockUsage(ctx, workspaceID, digest, allocatedBytes, s.maxOwnerBytes); err != nil {
			return 0, err
		}
		return 0, domain.NewError(domain.CodeWorkspaceConflict, "workspace sync block size does not match the transfer plan")
	}
	if err := s.store.AckWorkspaceSyncBlock(ctx, workspaceID, sessionID, digest, allocatedBytes, s.maxOwnerBytes); err != nil {
		if domain.CodeOf(err) == domain.CodeQuotaExceeded {
			// ACK persists an over-quota settlement before returning this error.
			return 0, err
		}
		// A failed ACK can leave a durable file behind. Retry settlement so the
		// owner ledger still accounts for it even when the session cannot advance.
		if settleErr := s.store.FinalizeWorkspaceBlockUsage(ctx, workspaceID, digest, allocatedBytes, s.maxOwnerBytes); settleErr != nil {
			return 0, errors.Join(err, fmt.Errorf("settle unacknowledged workspace block: %w", settleErr))
		}
		return 0, err
	}
	return n, nil
}

func workspaceBlockReservations(blocks WorkspaceBlockStore, workspaceID string, transfers []workspace.TransferBlock) ([]WorkspaceBlockReservation, error) {
	reservations := make([]WorkspaceBlockReservation, 0, len(transfers))
	for _, transfer := range transfers {
		if transfer.Role != workspace.BlockUpload || transfer.Received {
			continue
		}
		reservedBytes, err := blocks.ReservationBytes(transfer.SizeBytes)
		if err != nil {
			return nil, fmt.Errorf("estimate workspace block reservation: %w", err)
		}
		if reservedBytes <= 0 {
			return nil, domain.NewError(domain.CodeInternal, "workspace block reservation is invalid")
		}
		reservations = append(reservations, WorkspaceBlockReservation{WorkspaceID: workspaceID, Digest: transfer.SHA256, ReservedBytes: reservedBytes})
	}
	return reservations, nil
}

func (s *WorkspaceSyncService) lockWorkspaceBlockStore(ctx context.Context, workspaceID string, shared bool) (LeaderLock, error) {
	var lock LeaderLock
	var acquired bool
	var err error
	if shared {
		lock, acquired, err = s.store.TryWorkspaceBlockStoreReadLock(ctx, workspaceID)
	} else {
		lock, acquired, err = s.store.TryWorkspaceBlockStoreLock(ctx, workspaceID)
	}
	if err != nil {
		return nil, fmt.Errorf("lock workspace block store: %w", err)
	}
	if !acquired {
		return nil, domain.NewError(domain.CodeWorkspaceBusy, "workspace block store is busy")
	}
	if lock == nil {
		return nil, errors.New("workspace block store lock was not returned")
	}
	return lock, nil
}

// AckDownloadedBlock records receipt of a verified download block. The client
// acknowledges each destination file or deletion separately after applying it
// with AckDownloadAction; GET alone cannot establish either guarantee.
func (s *WorkspaceSyncService) AckDownloadedBlock(ctx context.Context, principal identity.Principal, workspaceID, sessionID, digest string) error {
	value, err := s.authorize(ctx, principal, workspaceID)
	if err != nil {
		return err
	}
	session, err := s.authorizeSession(ctx, principal, value, workspaceID, sessionID)
	if err != nil {
		return err
	}
	if !validSyncDigest(digest) {
		return domain.NewError(domain.CodeNotFound, "workspace sync block not found")
	}
	if session.State != workspace.SyncPlanned && session.State != workspace.SyncTransferring &&
		session.State != workspace.SyncVerifying && session.State != workspace.SyncApplying &&
		session.State != workspace.SyncCompleted && session.State != workspace.SyncConflict {
		return domain.NewError(domain.CodeInvalidState, "workspace sync session does not accept block acknowledgements")
	}
	blocks, err := s.store.ListWorkspaceSyncTransferBlocks(ctx, workspaceID, sessionID)
	if err != nil {
		return err
	}
	for _, block := range blocks {
		if block.SHA256 == digest && block.Role == workspace.BlockDownload {
			return s.store.AckWorkspaceSyncDownloadBlock(ctx, workspaceID, sessionID, digest)
		}
	}
	return domain.NewError(domain.CodeNotFound, "workspace sync block is not required by this session")
}

func (s *WorkspaceSyncService) AckDownloadAction(ctx context.Context, principal identity.Principal, workspaceID, sessionID, path string, action workspace.ChangeAction, sha256 string, sizeBytes int64) error {
	value, err := s.authorize(ctx, principal, workspaceID)
	if err != nil {
		return err
	}
	session, err := s.authorizeSession(ctx, principal, value, workspaceID, sessionID)
	if err != nil {
		return err
	}
	var digest *string
	if sha256 != "" {
		digest = &sha256
	}
	if err := workspace.ValidateDownloadAction(workspace.DownloadAction{
		Path: path, Action: action, SHA256: digest, SizeBytes: sizeBytes,
	}); err != nil {
		return domain.InvalidArgument("download_action", "workspace download action acknowledgement is invalid")
	}
	if session.State != workspace.SyncPlanned && session.State != workspace.SyncTransferring &&
		session.State != workspace.SyncVerifying && session.State != workspace.SyncApplying &&
		session.State != workspace.SyncCompleted && session.State != workspace.SyncConflict {
		return domain.NewError(domain.CodeInvalidState, "workspace sync session does not accept action acknowledgements")
	}
	return s.store.AckWorkspaceSyncDownloadAction(ctx, workspaceID, sessionID, path, action, sha256, sizeBytes)
}

func (s *WorkspaceSyncService) GetSyncBlock(ctx context.Context, principal identity.Principal, workspaceID, sessionID, digest string) (io.ReadCloser, error) {
	value, err := s.authorize(ctx, principal, workspaceID)
	if err != nil {
		return nil, err
	}
	session, err := s.authorizeSession(ctx, principal, value, workspaceID, sessionID)
	if err != nil {
		return nil, err
	}
	if !validSyncDigest(digest) {
		return nil, domain.NewError(domain.CodeNotFound, "workspace sync block not found")
	}
	if session.State == workspace.SyncCancelled || session.State == workspace.SyncExpired || session.State == workspace.SyncFailed {
		return nil, domain.NewError(domain.CodeTransferExpired, "workspace sync session is no longer active")
	}
	blocks, err := s.store.ListWorkspaceSyncTransferBlocks(ctx, workspaceID, sessionID)
	if err != nil {
		return nil, err
	}
	var expected *workspace.TransferBlock
	for i := range blocks {
		if blocks[i].SHA256 == digest && blocks[i].Role == workspace.BlockDownload {
			expected = &blocks[i]
			break
		}
	}
	if expected == nil {
		return nil, domain.NewError(domain.CodeNotFound, "workspace sync block is not required by this session")
	}
	data, err := readVerifiedBlock(s.blocks, workspaceID, workspace.Chunk{SHA256: expected.SHA256, SizeBytes: expected.SizeBytes})
	if err != nil {
		return nil, err
	}
	reader := io.NopCloser(bytes.NewReader(data))
	if principal.Kind == identity.KindDevice {
		return reauthorizeSyncReadCloser{
			ReadCloser: reader,
			authorize:  func() error { return s.store.CheckWorkspaceSyncDeviceAccess(ctx, principal, workspaceID) },
		}, nil
	}
	return reader, nil
}

func (s *WorkspaceSyncService) authorize(ctx context.Context, principal identity.Principal, workspaceID string) (workspace.Workspace, error) {
	if !domain.ValidWorkspaceID(workspaceID) {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	if !domain.ValidUserID(principal.UserID) {
		return workspace.Workspace{}, domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	switch principal.Kind {
	case identity.KindSession:
		if err := validatePrincipal(principal); err != nil {
			return workspace.Workspace{}, err
		}
	case identity.KindDevice:
		if principal.Role != identity.RoleUser || !domain.ValidTokenID(principal.TokenID) || principal.HostScope != "" || len(principal.HostIDs) != 0 {
			return workspace.Workspace{}, domain.NewError(domain.CodeUnauthenticated, "authentication required")
		}
		if !principal.HasScope(identity.ScopeWorkspaceSync) {
			return workspace.Workspace{}, domain.NewError(domain.CodeForbidden, "workspace sync scope is required")
		}
		if !principal.AllowsWorkspace(workspaceID) {
			return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if err := s.store.CheckWorkspaceSyncDeviceAccess(ctx, principal, workspaceID); err != nil {
			return workspace.Workspace{}, err
		}
	case identity.KindHostReplica:
		if !domain.ValidUserID(principal.UserID) || !domain.ValidHostID(principal.HostScope) || principal.HostGeneration < 1 ||
			principal.ReplicaWorkspaceID != workspaceID || principal.BindingVersion < 1 || principal.TokenID != "" ||
			len(principal.HostIDs) != 0 || len(principal.WorkspaceIDs) != 0 || len(principal.Scopes) != 0 {
			return workspace.Workspace{}, domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
		}
		binding, err := s.store.GetWorkspaceBinding(ctx, principal.HostScope, workspaceID)
		if err != nil {
			return workspace.Workspace{}, err
		}
		if binding.OwnerID != principal.UserID || binding.BindingVersion != principal.BindingVersion ||
			(binding.State != workspace.BindingPreparing && binding.State != workspace.BindingActive && binding.State != workspace.BindingUnbinding) {
			return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace binding not found")
		}
	default:
		return workspace.Workspace{}, domain.NewError(domain.CodeForbidden, "workspace synchronization is not allowed for this credential")
	}
	value, err := s.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return workspace.Workspace{}, err
	}
	if value.OwnerID != principal.UserID || value.State != workspace.StateReady || value.DeletedAt != nil {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return value, nil
}

type reauthorizeSyncReader struct {
	reader    io.Reader
	authorize func() error
}

func (r reauthorizeSyncReader) Read(p []byte) (int, error) {
	if err := r.authorize(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type reauthorizeSyncReadCloser struct {
	io.ReadCloser
	authorize func() error
}

func (r reauthorizeSyncReadCloser) Read(p []byte) (int, error) {
	if err := r.authorize(); err != nil {
		return 0, err
	}
	return r.ReadCloser.Read(p)
}

func (s *WorkspaceSyncService) authorizeSession(ctx context.Context, principal identity.Principal, value workspace.Workspace, workspaceID, sessionID string) (workspace.SyncSession, error) {
	if !workspace.ValidSessionID(sessionID) {
		return workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	session, err := s.store.GetWorkspaceSyncSession(ctx, workspaceID, sessionID)
	if err != nil {
		return workspace.SyncSession{}, err
	}
	if session.OwnerID != value.OwnerID || session.ReplicaID != syncReplicaID(principal) {
		return workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	return session, nil
}

func (s *WorkspaceSyncService) readManifest(ctx context.Context, workspaceID string, revision int64, scopes []string) ([]workspace.File, error) {
	files := make([]workspace.File, 0)
	prefixes := scopes
	if len(prefixes) == 0 {
		prefixes = []string{""}
	}
	manifestBytes := int64(2) // JSON array brackets.
	for _, prefix := range prefixes {
		after := ""
		for {
			page, err := s.store.GetWorkspaceManifestPage(ctx, workspaceID, revision, prefix, after, manifestPageLimit)
			if err != nil {
				return nil, err
			}
			for _, file := range page.Files {
				if len(files) >= workspace.MaxSyncFiles {
					return nil, domain.NewError(domain.CodeRequestTooLarge, "workspace manifest exceeds the file limit")
				}
				encoded, err := json.Marshal(file)
				if err != nil {
					return nil, domain.NewError(domain.CodeInternal, "workspace manifest file cannot be encoded")
				}
				if len(files) > 0 {
					manifestBytes++ // comma between array entries.
				}
				if manifestBytes > s.maxManifestBytes-int64(len(encoded)) {
					return nil, domain.NewError(domain.CodeRequestTooLarge, "workspace manifest exceeds its byte limit; narrow the sync scope")
				}
				manifestBytes += int64(len(encoded))
				files = append(files, file)
			}
			if page.NextCursor == nil || *page.NextCursor == "" {
				break
			}
			cursor, err := workspace.DecodeManifestCursor(*page.NextCursor)
			if err != nil || cursor.Revision != revision || cursor.Prefix != prefix {
				return nil, domain.NewError(domain.CodeInternal, "workspace manifest cursor is invalid")
			}
			if cursor.After == after {
				return nil, domain.NewError(domain.CodeInternal, "workspace manifest pagination did not advance")
			}
			after = cursor.After
		}
	}
	return files, nil
}

func (s *WorkspaceSyncService) prepareTransferPlan(ctx context.Context, workspaceID string, plan workspace.SyncPlan) ([]workspace.TransferBlock, int64, error) {
	byRole := make(map[string]workspace.TransferBlock, len(plan.UploadBlocks)+len(plan.DownloadBlocks))
	uploadDigests := make(map[string]struct{}, len(plan.UploadBlocks))
	var plannedBytes int64
	for _, chunk := range plan.UploadBlocks {
		uploadDigests[chunk.SHA256] = struct{}{}
		exists, err := verifyExistingBlock(s.blocks, workspaceID, chunk)
		if err != nil {
			return nil, 0, err
		}
		byRole[string(workspace.BlockUpload)+":"+chunk.SHA256] = workspace.TransferBlock{Chunk: chunk, Role: workspace.BlockUpload, Received: exists}
		if !exists {
			if plannedBytes > int64(^uint64(0)>>1)-chunk.SizeBytes {
				return nil, 0, domain.NewError(domain.CodeRequestTooLarge, "workspace transfer plan is too large")
			}
			plannedBytes += chunk.SizeBytes
		}
	}
	for _, chunk := range plan.DownloadBlocks {
		exists, err := verifyExistingBlock(s.blocks, workspaceID, chunk)
		if err != nil {
			return nil, 0, err
		}
		if !exists {
			if _, canUpload := uploadDigests[chunk.SHA256]; !canUpload {
				return nil, 0, domain.NewError(domain.CodeNotFound, "workspace download block is missing from storage")
			}
		}
		// A block already present on the server is still pending on this replica.
		// The client must confirm it after verification and durable local write.
		byRole[string(workspace.BlockDownload)+":"+chunk.SHA256] = workspace.TransferBlock{Chunk: chunk, Role: workspace.BlockDownload, Received: false}
		if plannedBytes > int64(^uint64(0)>>1)-chunk.SizeBytes {
			return nil, 0, domain.NewError(domain.CodeRequestTooLarge, "workspace transfer plan is too large")
		}
		plannedBytes += chunk.SizeBytes
	}
	blocks := make([]workspace.TransferBlock, 0, len(byRole))
	for _, block := range byRole {
		blocks = append(blocks, block)
	}
	sort.Slice(blocks, func(i, j int) bool {
		if blocks[i].Role == blocks[j].Role {
			return blocks[i].SHA256 < blocks[j].SHA256
		}
		return blocks[i].Role < blocks[j].Role
	})
	return blocks, plannedBytes, nil
}

func (s *WorkspaceSyncService) verifyUploadedFiles(ctx context.Context, workspaceID string, session workspace.SyncSession) error {
	transferBlocks, err := s.store.ListWorkspaceSyncTransferBlocks(ctx, workspaceID, session.ID)
	if err != nil {
		return err
	}
	received := make(map[string]bool, len(transferBlocks))
	for _, block := range transferBlocks {
		if block.Role == workspace.BlockUpload {
			received[block.SHA256] = block.Received
		}
	}
	for _, file := range session.UploadFiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		hasher := sha256.New()
		var total int64
		for _, chunk := range file.Chunks {
			if !received[chunk.SHA256] {
				return errIncompleteWorkspaceSync
			}
			data, err := readVerifiedBlock(s.blocks, workspaceID, chunk)
			if err != nil {
				return err
			}
			_, _ = hasher.Write(data)
			total += int64(len(data))
		}
		if total != file.SizeBytes || hex.EncodeToString(hasher.Sum(nil)) != file.SHA256 {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace sync file digest does not match its manifest")
		}
	}
	return nil
}

var errIncompleteWorkspaceSync = errors.New("workspace sync transfer is incomplete")

func readVerifiedBlock(blocks WorkspaceBlockReader, workspaceID string, chunk workspace.Chunk) ([]byte, error) {
	reader, err := blocks.Open(workspaceID, chunk.SHA256)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, domain.NewError(domain.CodeNotFound, "workspace sync block not found")
		}
		return nil, fmt.Errorf("open workspace block: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, workspace.MaxSyncBlockBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read workspace block: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close workspace block: %w", closeErr)
	}
	if int64(len(data)) != chunk.SizeBytes || int64(len(data)) > workspace.MaxSyncBlockBytes {
		return nil, domain.NewError(domain.CodeWorkspaceConflict, "workspace sync block size does not match its manifest")
	}
	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != chunk.SHA256 {
		return nil, domain.NewError(domain.CodeWorkspaceConflict, "workspace sync block failed sha-256 verification")
	}
	return data, nil
}

func verifyExistingBlock(blocks WorkspaceBlockReader, workspaceID string, chunk workspace.Chunk) (bool, error) {
	_, err := readVerifiedBlock(blocks, workspaceID, chunk)
	if err == nil {
		return true, nil
	}
	if domain.CodeOf(err) == domain.CodeNotFound {
		return false, nil
	}
	return false, err
}

func syncReplicaID(principal identity.Principal) string {
	if principal.Kind == identity.KindDevice {
		return "device:" + principal.TokenID
	}
	if principal.Kind == identity.KindHostReplica {
		return fmt.Sprintf("host:%s:g%d:%s:b%d", principal.HostScope, principal.HostGeneration, principal.ReplicaWorkspaceID, principal.BindingVersion)
	}
	return "session:" + principal.UserID
}

func validateSyncIdempotencyKey(value string) error {
	if !syncIdempotencyKeyPattern.MatchString(value) {
		return domain.InvalidArgument("idempotency_key", "idempotency key is invalid")
	}
	return nil
}

func validSyncDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

// compile-time assertions keep the HTTP adapter and command wiring honest.
var _ interface {
	GetManifest(context.Context, identity.Principal, string, int64, string, string, int32) (workspace.ManifestPage, error)
	GetChanges(context.Context, identity.Principal, string, string, time.Duration, int32) (workspace.ChangePage, error)
	CreateSyncSession(context.Context, identity.Principal, string, workspace.CreateSyncSession, string) (workspace.SyncSession, error)
	GetSyncSession(context.Context, identity.Principal, string, string) (workspace.SyncSession, error)
	ListDeviceSyncProgress(context.Context, identity.Principal, string) ([]workspace.DeviceSyncProgress, error)
	CommitSyncSession(context.Context, identity.Principal, string, string, string) (workspace.SyncSession, error)
	CancelSyncSession(context.Context, identity.Principal, string, string, string) (workspace.SyncSession, error)
	PutSyncBlock(context.Context, identity.Principal, string, string, string, io.Reader) (int64, error)
	AckDownloadedBlock(context.Context, identity.Principal, string, string, string) error
	AckDownloadAction(context.Context, identity.Principal, string, string, string, workspace.ChangeAction, string, int64) error
	GetSyncBlock(context.Context, identity.Principal, string, string, string) (io.ReadCloser, error)
} = (*WorkspaceSyncService)(nil)
