package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	mordesync "morde-sync"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func verifyWorkspaceCopyShareClaimTx(ctx context.Context, tx pgx.Tx, request app.WorkspaceCopyReservationRequest,
	targetWorkspaceID string) error {
	if request.ShareID == "" {
		if request.SourceOwnerID != request.TargetOwnerID || request.ReadOnly {
			return domain.InvalidArgument("workspace_copy", "ordinary Workspace copies must retain their owner")
		}
		return nil
	}
	var ownerID, sourceWorkspaceID, name, permission, state, claimantID, storedTarget string
	var sourceRevision int64
	err := tx.QueryRow(ctx, `SELECT owner_id,source_workspace_id,source_revision,workspace_name,permission,state,
COALESCE(claimant_id,''),COALESCE(target_workspace_id,'')
FROM workspace_share_links WHERE id=$1 FOR UPDATE`, request.ShareID).Scan(
		&ownerID, &sourceWorkspaceID, &sourceRevision, &name, &permission, &state, &claimantID, &storedTarget)
	if err != nil {
		return storeError("lock Workspace share copy claim", "Workspace share link", err)
	}
	wantPermission := "edit"
	if request.ReadOnly {
		wantPermission = "view"
	}
	if ownerID != request.SourceOwnerID || sourceWorkspaceID != request.SourceWorkspaceID || sourceRevision != request.SourceRevision ||
		name != request.TargetName || permission != wantPermission || state != string(workspace.ShareClaiming) || claimantID != request.TargetOwnerID {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace share claim does not match the copy request")
	}
	if storedTarget != targetWorkspaceID {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace share target does not match its copy reservation")
	}
	return nil
}

func (s *Store) CreateWorkspaceCopyReservation(ctx context.Context, request app.WorkspaceCopyReservationRequest) (workspace.CopyReservation, bool, error) {
	if !domain.ValidUserID(request.SourceOwnerID) || !domain.ValidUserID(request.TargetOwnerID) ||
		!domain.ValidWorkspaceID(request.SourceWorkspaceID) || !domain.ValidWorkspaceID(request.TargetWorkspaceID) ||
		request.SourceWorkspaceID == request.TargetWorkspaceID || !domain.ValidPendingRequestID(request.ID) || request.TargetName == "" || len(request.TargetName) > 120 ||
		request.ServiceInstanceID == "" || len(request.ServiceInstanceID) > 128 || request.SourceRevision < 0 || request.ReservedBytes < 0 ||
		request.MaxOwnerBytes <= 0 || request.TargetQuotaBytes <= 0 || request.Idempotency.OwnerID != request.TargetOwnerID ||
		request.Idempotency.ResourceID != request.SourceWorkspaceID {
		return workspace.CopyReservation{}, false, domain.InvalidArgument("workspace_copy", "Workspace copy reservation fields are invalid")
	}
	if request.ShareID == "" {
		if request.SourceOwnerID != request.TargetOwnerID || request.ReadOnly || request.Idempotency.Operation != "workspace.copy" {
			return workspace.CopyReservation{}, false, domain.InvalidArgument("workspace_copy", "ordinary Workspace copies must retain their owner")
		}
	} else if !domain.ValidPendingRequestID(request.ShareID) || request.SourceOwnerID == request.TargetOwnerID ||
		request.Idempotency.Operation != "workspace.share.copy" {
		return workspace.CopyReservation{}, false, domain.InvalidArgument("workspace_copy", "Workspace share copy ownership is invalid")
	}
	record := request.Idempotency
	if err := validateIdempotency(record); err != nil {
		return workspace.CopyReservation{}, false, err
	}

	var reservation workspace.CopyReservation
	replayed := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceMutationFence(ctx, tx, request.SourceWorkspaceID, true); err != nil {
			return err
		}
		stored, replay, err := reserveIdempotency(ctx, tx, record, request.SourceWorkspaceID, request.ID)
		if err != nil {
			return err
		}
		if replay {
			if stored.ResourceID != request.SourceWorkspaceID {
				return domain.NewError(domain.CodeIdempotencyConflict, "Workspace copy key is bound to another source")
			}
			reservation, err = getWorkspaceCopyReservationTx(ctx, tx, stored.RelatedID)
			if err != nil {
				return err
			}
			if reservation.SourceOwnerID != request.SourceOwnerID || reservation.TargetOwnerID != request.TargetOwnerID ||
				reservation.ShareID != request.ShareID || reservation.ReadOnly != request.ReadOnly ||
				reservation.SourceWorkspaceID != request.SourceWorkspaceID || reservation.ServiceInstanceID != request.ServiceInstanceID ||
				reservation.SourceRevision != request.SourceRevision || reservation.ReservedBytes != request.ReservedBytes {
				return domain.NewError(domain.CodeIdempotencyConflict, "Workspace copy reservation does not match the original request")
			}
			var storedName string
			var storedQuota int64
			var storedOwner string
			var storedReadOnly bool
			if err := tx.QueryRow(ctx, `SELECT owner_id,name,quota_bytes,is_read_only FROM workspaces WHERE id=$1`, reservation.TargetWorkspaceID).
				Scan(&storedOwner, &storedName, &storedQuota, &storedReadOnly); err != nil || storedOwner != request.TargetOwnerID ||
				storedName != request.TargetName || storedQuota != request.TargetQuotaBytes || storedReadOnly != request.ReadOnly {
				return domain.NewError(domain.CodeIdempotencyConflict, "Workspace copy target does not match the original request")
			}
			if err := verifyWorkspaceCopyShareClaimTx(ctx, tx, request, reservation.TargetWorkspaceID); err != nil {
				return err
			}
			replayed = true
			return nil
		}
		if err := verifyWorkspaceCopyShareClaimTx(ctx, tx, request, ""); err != nil {
			return err
		}

		if err := lockWorkspaceForSyncMutation(ctx, tx, request.SourceWorkspaceID); err != nil {
			return err
		}
		var currentOwner, state string
		var readOnly bool
		var currentRevision int64
		var deleted bool
		if err := tx.QueryRow(ctx, `SELECT owner_id,state,revision,deleted_at IS NOT NULL,is_read_only
FROM workspaces WHERE id=$1`, request.SourceWorkspaceID).Scan(&currentOwner, &state, &currentRevision, &deleted, &readOnly); err != nil {
			return storeError("read source Workspace for copy", "workspace", err)
		}
		if currentOwner != request.SourceOwnerID || deleted {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if readOnly {
			return domain.NewError(domain.CodeForbidden, "workspace is shared as read-only")
		}
		if state != string(workspace.StateReady) || currentRevision != request.SourceRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed before copy reservation")
		}
		var desiredRevision, appliedRevision int64
		if err := tx.QueryRow(ctx, `SELECT desired_revision,applied_revision FROM workspace_current_materializations WHERE workspace_id=$1`, request.SourceWorkspaceID).
			Scan(&desiredRevision, &appliedRevision); err != nil || desiredRevision != currentRevision || appliedRevision != currentRevision {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace current revision is not confirmed")
		}
		var volumeReady bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_volumes WHERE workspace_id=$1 AND owner_id=$2 AND provisioned_owner_id=$2
 AND state='ready' AND volume_name='workspace-' || $1
		)`, request.SourceWorkspaceID, request.SourceOwnerID).Scan(&volumeReady); err != nil {
			return fmt.Errorf("check source Workspace volume before copy: %w", err)
		}
		if !volumeReady {
			return domain.NewError(domain.CodeWorkspaceUnavailable, "source Workspace volume is not ready for a complete copy")
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
		  AND o.status IN ('prepared','dispatched','unknown') AND o.sync_stage IN ('pending','local_applied','sync_pending'))`, request.SourceWorkspaceID).Scan(&blocked); err != nil {
			return fmt.Errorf("check Workspace writers before copy: %w", err)
		}
		if blocked {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has a Host binding or in-flight write; stop and settle it before copy")
		}
		if err := ensureNoWorkspaceSyncReceive(ctx, tx, request.SourceWorkspaceID, ""); err != nil {
			return err
		}

		allocatedBytes, ownerReservedBytes, err := lockWorkspaceOwnerUsage(ctx, tx, request.TargetOwnerID)
		if err != nil {
			return err
		}
		if allocatedBytes > request.MaxOwnerBytes || ownerReservedBytes > request.MaxOwnerBytes-allocatedBytes || request.ReservedBytes > request.MaxOwnerBytes-allocatedBytes-ownerReservedBytes {
			return domain.NewError(domain.CodeQuotaExceeded, "Workspace copy exceeds the owner's remaining physical storage quota")
		}
		if request.ReservedBytes > math.MaxInt64-ownerReservedBytes {
			return domain.NewError(domain.CodeQuotaExceeded, "Workspace copy reservation exceeds the storage quota")
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_owner_block_usage SET reserved_bytes=reserved_bytes+$2,updated_at=clock_timestamp() WHERE owner_id=$1`, request.TargetOwnerID, request.ReservedBytes); err != nil {
			return fmt.Errorf("reserve owner Workspace copy capacity: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspaces (id,owner_id,name,state,revision,used_bytes,quota_bytes,is_read_only)
VALUES ($1,$2,$3,'copying',0,0,$4,$5)`, request.TargetWorkspaceID, request.TargetOwnerID, request.TargetName, request.TargetQuotaBytes, request.ReadOnly); err != nil {
			return fmt.Errorf("create reserved Workspace copy target: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_ownership_history (workspace_id,owner_id,transfer_id) VALUES ($1,$2,NULL)`, request.TargetWorkspaceID, request.TargetOwnerID); err != nil {
			return fmt.Errorf("record reserved Workspace copy owner: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_revisions (workspace_id,revision,file_count,total_bytes,directory_count) VALUES ($1,0,0,0,0)`, request.TargetWorkspaceID); err != nil {
			return fmt.Errorf("create reserved Workspace copy revision: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_current_materializations (workspace_id,desired_revision,applied_revision) VALUES ($1,0,-1)`, request.TargetWorkspaceID); err != nil {
			return fmt.Errorf("create reserved Workspace copy materialization: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_volumes (workspace_id,owner_id,provisioned_owner_id,state,volume_name)
		VALUES ($1,$2,$2,'pending',$3)`, request.TargetWorkspaceID, request.TargetOwnerID, app.WorkspaceNamedVolumeName(request.TargetWorkspaceID)); err != nil {
			return fmt.Errorf("register reserved Workspace copy volume: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_copy_reservations
(id,owner_id,source_owner_id,target_owner_id,share_id,read_only,source_workspace_id,target_workspace_id,service_instance_id,source_revision,reserved_bytes,state)
VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,$8,$9,$10,$11,'reserved')`, request.ID, request.SourceOwnerID, request.SourceOwnerID,
			request.TargetOwnerID, request.ShareID, request.ReadOnly, request.SourceWorkspaceID, request.TargetWorkspaceID, request.ServiceInstanceID,
			request.SourceRevision, request.ReservedBytes); err != nil {
			return fmt.Errorf("create Workspace copy reservation: %w", err)
		}
		if request.ShareID != "" {
			updated, err := tx.Exec(ctx, `UPDATE workspace_share_links SET target_workspace_id=$2
WHERE id=$1 AND state='claiming' AND claimant_id=$3 AND target_workspace_id IS NULL`, request.ShareID, request.TargetWorkspaceID, request.TargetOwnerID)
			if err != nil || updated.RowsAffected() != 1 {
				return errors.Join(domain.NewError(domain.CodeWorkspaceConflict, "Workspace share claim changed before its copy was reserved"), err)
			}
		}
		reservation = workspace.CopyReservation{
			ID: request.ID, SourceOwnerID: request.SourceOwnerID, TargetOwnerID: request.TargetOwnerID, ShareID: request.ShareID,
			SourceWorkspaceID: request.SourceWorkspaceID, TargetWorkspaceID: request.TargetWorkspaceID,
			ServiceInstanceID: request.ServiceInstanceID, SourceRevision: request.SourceRevision, ReservedBytes: request.ReservedBytes,
			ReadOnly: request.ReadOnly,
			State:    workspace.CopyReserved, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		return nil
	})
	if err != nil {
		return workspace.CopyReservation{}, false, err
	}
	return reservation, replayed, nil
}

func (s *Store) GetWorkspaceCopyReservationByIdempotency(ctx context.Context, record app.IdempotencyRecord, name, serviceInstanceID string,
	quotaBytes int64) (workspace.CopyReservation, bool, error) {
	if err := validateIdempotency(record); err != nil {
		return workspace.CopyReservation{}, false, err
	}
	if (record.Operation != "workspace.copy" && record.Operation != "workspace.share.copy") ||
		!domain.ValidWorkspaceID(record.ResourceID) || name == "" || len(name) > 120 ||
		strings.TrimSpace(serviceInstanceID) == "" || len(serviceInstanceID) > 128 || quotaBytes <= 0 {
		return workspace.CopyReservation{}, false, domain.InvalidArgument("workspace_copy", "Workspace copy idempotency lookup is invalid")
	}
	var reservation workspace.CopyReservation
	found := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		stored, ok, err := findIdempotency(ctx, tx, record)
		if err != nil || !ok {
			return err
		}
		if stored.ResourceID != record.ResourceID || !domain.ValidPendingRequestID(stored.RelatedID) {
			return domain.NewError(domain.CodeIdempotencyConflict, "Workspace copy key is bound to another source")
		}
		reservation, err = getWorkspaceCopyReservationTx(ctx, tx, stored.RelatedID)
		if err != nil {
			return err
		}
		var storedName string
		var storedQuota int64
		if reservation.TargetOwnerID != record.OwnerID || reservation.SourceWorkspaceID != record.ResourceID ||
			reservation.ServiceInstanceID != serviceInstanceID {
			return domain.NewError(domain.CodeIdempotencyConflict, "Workspace copy reservation does not match the original request")
		}
		if (record.Operation == "workspace.copy") != (reservation.ShareID == "") {
			return domain.NewError(domain.CodeIdempotencyConflict, "Workspace copy reservation operation does not match the original request")
		}
		if err := tx.QueryRow(ctx, `SELECT name,quota_bytes FROM workspaces WHERE id=$1`, reservation.TargetWorkspaceID).
			Scan(&storedName, &storedQuota); err != nil || storedName != name || storedQuota != quotaBytes {
			return domain.NewError(domain.CodeIdempotencyConflict, "Workspace copy target does not match the original request")
		}
		if reservation.ShareID != "" {
			if err := verifyWorkspaceCopyShareClaimTx(ctx, tx, app.WorkspaceCopyReservationRequest{
				SourceOwnerID: reservation.SourceOwnerID, TargetOwnerID: reservation.TargetOwnerID, ShareID: reservation.ShareID,
				SourceWorkspaceID: reservation.SourceWorkspaceID, TargetWorkspaceID: reservation.TargetWorkspaceID,
				TargetName: name, SourceRevision: reservation.SourceRevision, ReadOnly: reservation.ReadOnly,
			}, reservation.TargetWorkspaceID); err != nil {
				return err
			}
		}
		found = true
		return nil
	})
	if err != nil {
		return workspace.CopyReservation{}, false, err
	}
	return reservation, found, nil
}

func (s *Store) GetWorkspaceManifestPageForCopy(ctx context.Context, ownerID, reservationID string, revision int64,
	prefix, afterPath string, limit int32) (workspace.ManifestPage, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidPendingRequestID(reservationID) || revision < 0 || limit < 1 || limit > maxWorkspaceSyncPage {
		return workspace.ManifestPage{}, domain.NewError(domain.CodeNotFound, "Workspace copy not found")
	}
	if prefix != "" {
		if err := workspace.ValidateRelativePath(prefix); err != nil {
			return workspace.ManifestPage{}, domain.InvalidArgument("prefix", "workspace manifest prefix is invalid")
		}
	}
	if afterPath != "" {
		if err := workspace.ValidateRelativePath(afterPath); err != nil {
			return workspace.ManifestPage{}, domain.InvalidArgument("cursor", "workspace manifest cursor path is invalid")
		}
	}
	var page workspace.ManifestPage
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var sourceWorkspaceID, state, workspaceState string
		var currentOwner string
		var sourceRevision, currentRevision int64
		var deleted bool
		if err := tx.QueryRow(ctx, `SELECT c.source_workspace_id,c.source_revision,c.state,w.owner_id,w.state,w.revision,w.deleted_at IS NOT NULL
FROM workspace_copy_reservations c JOIN workspaces w ON w.id=c.source_workspace_id
WHERE c.id=$1 AND c.source_owner_id=$2 FOR SHARE OF c,w`, reservationID, ownerID).Scan(
			&sourceWorkspaceID, &sourceRevision, &state, &currentOwner, &workspaceState, &currentRevision, &deleted); err != nil {
			return storeError("verify Workspace copy manifest scope", "Workspace copy", err)
		}
		if currentOwner != ownerID || workspaceState != string(workspace.StateReady) || deleted || sourceRevision != revision || currentRevision != revision ||
			(state != string(workspace.CopyReserved) && state != string(workspace.Copying) && state != string(workspace.CopyUnknown)) {
			return domain.NewError(domain.CodeNotFound, "Workspace copy not found")
		}
		var err error
		page, err = getWorkspaceManifestPageTx(ctx, tx, sourceWorkspaceID, revision, prefix, afterPath, limit)
		return err
	})
	if err != nil {
		return workspace.ManifestPage{}, err
	}
	return page, nil
}

func (s *Store) TryWorkspaceCopyLock(ctx context.Context, reservationID string) (app.LeaderLock, bool, error) {
	if !domain.ValidPendingRequestID(reservationID) {
		return nil, false, domain.InvalidArgument("workspace_copy", "Workspace copy reservation ID is invalid")
	}
	if s.pool.Config().MaxConns < 2 {
		return nil, false, fmt.Errorf("workspace copy lock requires at least two database connections")
	}
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte("morde:workspace-copy:" + reservationID))
	key := int64(hasher.Sum64())
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire Workspace copy lock connection: %w", err)
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::bigint)`, key).Scan(&acquired); err != nil {
		_ = discardLeaderConnection(conn)
		return nil, false, fmt.Errorf("try Workspace copy advisory lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	capacityHasher := fnv.New64a()
	_, _ = capacityHasher.Write([]byte("morde:workspace-copy-capacity"))
	capacityKey := int64(capacityHasher.Sum64())
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::bigint)`, capacityKey).Scan(&acquired); err != nil {
		_ = discardLeaderConnection(conn)
		return nil, false, fmt.Errorf("try Workspace copy capacity advisory lock: %w", err)
	}
	if !acquired {
		var unlocked bool
		if err := conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1::bigint)`, key).Scan(&unlocked); err != nil {
			_ = discardLeaderConnection(conn)
			return nil, false, fmt.Errorf("release Workspace copy advisory lock: %w", err)
		}
		if !unlocked {
			_ = discardLeaderConnection(conn)
			return nil, false, errors.New("workspace copy advisory lock was not held")
		}
		conn.Release()
		return nil, false, nil
	}
	return &sessionLeaderLock{conn: conn, locks: []advisoryLock{{key: key}, {key: capacityKey}}}, true, nil
}

func (s *Store) GetWorkspaceCopyReservationByID(ctx context.Context, id string) (workspace.CopyReservation, error) {
	if !domain.ValidPendingRequestID(id) {
		return workspace.CopyReservation{}, domain.NewError(domain.CodeNotFound, "Workspace copy not found")
	}
	return s.getWorkspaceCopyReservation(ctx, `SELECT id,source_owner_id,target_owner_id,COALESCE(share_id,''),source_workspace_id,target_workspace_id,
service_instance_id,source_revision,reserved_bytes,read_only,state,created_at,updated_at FROM workspace_copy_reservations WHERE id=$1`, id)
}

func (s *Store) getWorkspaceCopyReservation(ctx context.Context, query string, args ...any) (workspace.CopyReservation, error) {
	var reservation workspace.CopyReservation
	err := s.pool.QueryRow(ctx, query, args...).Scan(
		&reservation.ID, &reservation.SourceOwnerID, &reservation.TargetOwnerID, &reservation.ShareID, &reservation.SourceWorkspaceID,
		&reservation.TargetWorkspaceID, &reservation.ServiceInstanceID, &reservation.SourceRevision, &reservation.ReservedBytes,
		&reservation.ReadOnly, &reservation.State, &reservation.CreatedAt, &reservation.UpdatedAt)
	if err != nil {
		return workspace.CopyReservation{}, storeError("get Workspace copy reservation", "Workspace copy", err)
	}
	if err := reservation.Validate(); err != nil {
		return workspace.CopyReservation{}, domain.NewError(domain.CodeInternal, "stored Workspace copy reservation is invalid")
	}
	return reservation, nil
}

func (s *Store) SetWorkspaceCopyReservationState(ctx context.Context, id string, from, to workspace.CopyState) error {
	if !domain.ValidPendingRequestID(id) || !from.Valid() || !to.Valid() || !validWorkspaceCopyTransition(from, to) {
		return domain.InvalidArgument("workspace_copy", "Workspace copy state transition is invalid")
	}
	result, err := s.pool.Exec(ctx, `UPDATE workspace_copy_reservations SET state=$3,updated_at=clock_timestamp()
WHERE id=$1 AND state=$2`, id, string(from), string(to))
	if err != nil {
		return fmt.Errorf("update Workspace copy reservation state: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy reservation changed")
	}
	return nil
}

func (s *Store) FailWorkspaceCopyReservationBeforeSideEffects(ctx context.Context, ownerID, id string) error {
	if !domain.ValidUserID(ownerID) || !domain.ValidPendingRequestID(id) {
		return domain.InvalidArgument("workspace_copy", "Workspace copy failure request is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var storedTargetOwner, sourceOwnerID, sourceWorkspaceID, targetWorkspaceID, shareID, state string
		var reservedBytes int64
		var readOnly bool
		if err := tx.QueryRow(ctx, `SELECT target_owner_id,source_owner_id,source_workspace_id,target_workspace_id,
COALESCE(share_id,''),reserved_bytes,read_only,state
FROM workspace_copy_reservations WHERE id=$1 FOR UPDATE`, id).Scan(
			&storedTargetOwner, &sourceOwnerID, &sourceWorkspaceID, &targetWorkspaceID, &shareID, &reservedBytes, &readOnly, &state); err != nil {
			return storeError("lock Workspace copy before safe failure", "Workspace copy", err)
		}
		if storedTargetOwner != ownerID {
			return domain.NewError(domain.CodeNotFound, "Workspace copy not found")
		}
		if shareID != "" {
			if state != string(workspace.CopyReserved) {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace share copy may have side effects and cannot be safely released")
			}

			var claimState, claimantID, linkedTargetID string
			if err := tx.QueryRow(ctx, `SELECT state,COALESCE(claimant_id,''),COALESCE(target_workspace_id,'')
FROM workspace_share_links WHERE id=$1 FOR UPDATE`, shareID).Scan(&claimState, &claimantID, &linkedTargetID); err != nil {
				return storeError("lock Workspace share before releasing safe copy", "Workspace share link", err)
			}
			if claimState != string(workspace.ShareClaiming) || claimantID != ownerID || linkedTargetID != targetWorkspaceID {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace share copy claim changed before safe release")
			}

			var targetOwner, targetState, volumeOwner, provisionedOwner, volumeState, volumeName string
			var targetRevision, desiredRevision, appliedRevision int64
			var targetReadOnly bool
			if err := tx.QueryRow(ctx, `SELECT w.owner_id,w.state,w.revision,w.is_read_only,
v.owner_id,v.provisioned_owner_id,v.state,v.volume_name,m.desired_revision,m.applied_revision
FROM workspaces w
JOIN workspace_volumes v ON v.workspace_id=w.id
JOIN workspace_current_materializations m ON m.workspace_id=w.id
WHERE w.id=$1 FOR UPDATE OF w,v,m`, targetWorkspaceID).Scan(
				&targetOwner, &targetState, &targetRevision, &targetReadOnly, &volumeOwner, &provisionedOwner,
				&volumeState, &volumeName, &desiredRevision, &appliedRevision); err != nil {
				return storeError("verify Workspace share copy has no side effects", "workspace", err)
			}
			if targetOwner != ownerID || targetState != string(workspace.StateCopying) || targetRevision != 0 || targetReadOnly != readOnly ||
				volumeOwner != ownerID || provisionedOwner != ownerID || volumeState != "pending" ||
				volumeName != app.WorkspaceNamedVolumeName(targetWorkspaceID) || desiredRevision != 0 || appliedRevision != -1 {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace share copy target may have side effects and cannot be safely released")
			}

			_, ownerReserved, err := lockWorkspaceOwnerUsage(ctx, tx, ownerID)
			if err != nil {
				return err
			}
			if ownerReserved < reservedBytes {
				return domain.NewError(domain.CodeInternal, "Workspace copy capacity reservation is missing")
			}
			if _, err := tx.Exec(ctx, `UPDATE workspace_owner_block_usage SET reserved_bytes=reserved_bytes-$2,updated_at=clock_timestamp() WHERE owner_id=$1`, ownerID, reservedBytes); err != nil {
				return fmt.Errorf("release recipient Workspace copy capacity reservation: %w", err)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM workspace_volumes WHERE workspace_id=$1`, targetWorkspaceID); err != nil {
				return fmt.Errorf("remove unused recipient Workspace copy volume registration: %w", err)
			}
			updated, err := tx.Exec(ctx, `UPDATE workspaces SET state='deleted',deleted_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND state='copying' AND revision=0`, targetWorkspaceID, ownerID)
			if err != nil || updated.RowsAffected() != 1 {
				return errors.Join(domain.NewError(domain.CodeWorkspaceConflict, "unused Workspace share copy target changed"), err)
			}
			updated, err = tx.Exec(ctx, `UPDATE workspace_share_links SET target_workspace_id=NULL
WHERE id=$1 AND state='claiming' AND claimant_id=$2 AND target_workspace_id=$3`, shareID, ownerID, targetWorkspaceID)
			if err != nil || updated.RowsAffected() != 1 {
				return errors.Join(domain.NewError(domain.CodeWorkspaceConflict, "Workspace share target changed before safe release"), err)
			}
			idempotency, err := tx.Exec(ctx, `DELETE FROM idempotency_keys
WHERE owner_id=$1 AND operation='workspace.share.copy' AND key=$2 AND resource_id=$3 AND related_id=$4`,
				ownerID, "ws-share-copy-"+shareID, sourceWorkspaceID, id)
			if err != nil || idempotency.RowsAffected() != 1 {
				return errors.Join(domain.NewError(domain.CodeWorkspaceConflict, "Workspace share copy idempotency receipt changed before safe release"), err)
			}
			deleted, err := tx.Exec(ctx, `DELETE FROM workspace_copy_reservations WHERE id=$1 AND share_id=$2 AND state='reserved'`, id, shareID)
			if err != nil || deleted.RowsAffected() != 1 {
				return errors.Join(domain.NewError(domain.CodeWorkspaceConflict, "Workspace share copy reservation changed before safe release"), err)
			}
			return nil
		}
		if state == string(workspace.CopyFailed) {
			return nil
		}
		if state != string(workspace.CopyReserved) {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy may have side effects and cannot be safely released")
		}
		_, ownerReserved, err := lockWorkspaceOwnerUsage(ctx, tx, ownerID)
		if err != nil {
			return err
		}
		if ownerReserved < reservedBytes {
			return domain.NewError(domain.CodeInternal, "Workspace copy capacity reservation is missing")
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_owner_block_usage SET reserved_bytes=reserved_bytes-$2,updated_at=clock_timestamp() WHERE owner_id=$1`, ownerID, reservedBytes); err != nil {
			return fmt.Errorf("release safe Workspace copy capacity reservation: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_volumes WHERE workspace_id=$1`, targetWorkspaceID); err != nil {
			return fmt.Errorf("remove unused Workspace copy volume registration: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE workspaces SET state='deleted',deleted_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE id=$1 AND state='copying' AND revision=0`, targetWorkspaceID); err != nil {
			return fmt.Errorf("retire unused Workspace copy target: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_copy_reservations SET state='failed',updated_at=clock_timestamp() WHERE id=$1 AND state='reserved'`, id); err != nil {
			return fmt.Errorf("mark unused Workspace copy failed: %w", err)
		}
		return nil
	})
}

func (s *Store) StageWorkspaceCopy(ctx context.Context, reservationID, ownerID, name string, files []workspace.File, emptyDirectories []string,
	directories []workspace.Directory, allocations []app.WorkspaceBlockAllocation, workspaceQuotaBytes int64) error {
	if !domain.ValidPendingRequestID(reservationID) || !domain.ValidUserID(ownerID) || name == "" || len(name) > 120 ||
		len(files)+len(emptyDirectories) > workspace.MaxSyncFiles || len(directories) > workspace.MaxSyncFiles || workspaceQuotaBytes <= 0 {
		return domain.InvalidArgument("workspace_copy", "Workspace copy snapshot is invalid")
	}
	reservation, err := s.GetWorkspaceCopyReservationByID(ctx, reservationID)
	if err != nil {
		return err
	}
	if reservation.TargetOwnerID != ownerID || !reservation.State.Active() {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy reservation is no longer active")
	}
	fileBytes, blockSizes, err := validateWorkspaceCopySnapshot(files, emptyDirectories, directories, false)
	if err != nil {
		return err
	}
	if fileBytes > workspaceQuotaBytes {
		return domain.NewError(domain.CodeQuotaExceeded, "copied Workspace exceeds its file quota")
	}
	allocationByDigest := make(map[string]int64, len(allocations))
	var allocatedBytes int64
	for _, allocation := range allocations {
		if allocation.WorkspaceID != reservation.TargetWorkspaceID || !validPostgresSHA256(allocation.Digest) || allocation.AllocatedBytes < 0 {
			return domain.InvalidArgument("workspace_copy", "Workspace copy block allocation is invalid")
		}
		if _, exists := blockSizes[allocation.Digest]; !exists {
			return domain.InvalidArgument("workspace_copy", "Workspace copy allocation is not referenced by its manifest")
		}
		if _, duplicate := allocationByDigest[allocation.Digest]; duplicate {
			return domain.InvalidArgument("workspace_copy", "Workspace copy allocations contain duplicates")
		}
		allocationByDigest[allocation.Digest] = allocation.AllocatedBytes
		if allocatedBytes > math.MaxInt64-allocation.AllocatedBytes {
			return domain.NewError(domain.CodeQuotaExceeded, "Workspace copy allocation exceeds storage limits")
		}
		allocatedBytes += allocation.AllocatedBytes
	}
	if len(allocationByDigest) != len(blockSizes) || allocatedBytes > reservation.ReservedBytes {
		return domain.NewError(domain.CodeQuotaExceeded, "Workspace copy allocation does not match its capacity reservation")
	}

	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var storedTargetOwner, storedSourceOwner, state string
		var sourceWorkspaceID, targetWorkspaceID string
		var sourceRevision, reservedBytes int64
		var readOnly bool
		if err := tx.QueryRow(ctx, `SELECT source_owner_id,target_owner_id,source_workspace_id,target_workspace_id,source_revision,reserved_bytes,read_only,state
FROM workspace_copy_reservations WHERE id=$1 FOR UPDATE`, reservationID).Scan(
			&storedSourceOwner, &storedTargetOwner, &sourceWorkspaceID, &targetWorkspaceID, &sourceRevision, &reservedBytes, &readOnly, &state); err != nil {
			return storeError("lock Workspace copy before staging", "Workspace copy", err)
		}
		if storedTargetOwner != ownerID || (state != string(workspace.Copying) && state != string(workspace.CopyUnknown)) {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy is not in a staging state")
		}
		var sourceOwner, sourceState string
		var currentRevision int64
		if err := tx.QueryRow(ctx, `SELECT owner_id,state,revision FROM workspaces WHERE id=$1 FOR SHARE`, sourceWorkspaceID).
			Scan(&sourceOwner, &sourceState, &currentRevision); err != nil {
			return storeError("verify Workspace copy source checkpoint", "workspace", err)
		}
		if sourceOwner != storedSourceOwner || sourceState != string(workspace.StateReady) || currentRevision != sourceRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy source checkpoint changed")
		}
		if storedSourceOwner != reservation.SourceOwnerID || storedTargetOwner != reservation.TargetOwnerID ||
			targetWorkspaceID != reservation.TargetWorkspaceID || reservedBytes != reservation.ReservedBytes || readOnly != reservation.ReadOnly {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy reservation changed")
		}
		var stagedOwner, stagedState, stagedName string
		var stagedReadOnly bool
		var stagedRevision, stagedBytes, stagedQuota int64
		err := tx.QueryRow(ctx, `SELECT owner_id,state,name,revision,used_bytes,quota_bytes,is_read_only FROM workspaces WHERE id=$1 FOR UPDATE`, targetWorkspaceID).
			Scan(&stagedOwner, &stagedState, &stagedName, &stagedRevision, &stagedBytes, &stagedQuota, &stagedReadOnly)
		if err == nil {
			if stagedOwner != storedTargetOwner || stagedState != string(workspace.StateCopying) || stagedReadOnly != readOnly {
				return domain.NewError(domain.CodeWorkspaceConflict, "reserved Workspace copy target does not match its reservation")
			}
			if stagedRevision == 1 && stagedBytes == fileBytes && stagedName == name && stagedQuota == workspaceQuotaBytes {
				storedFiles, storedEmptyDirectories, storedDirectories, err := getWorkspaceCopyManifestTx(ctx, tx, targetWorkspaceID)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(storedFiles, files) || !reflect.DeepEqual(storedEmptyDirectories, emptyDirectories) || !reflect.DeepEqual(storedDirectories, directories) {
					return domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy target already contains a different staged checkpoint")
				}
				return nil
			}
			if stagedRevision != 0 || stagedBytes != 0 || stagedName != name || stagedQuota != workspaceQuotaBytes {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy target has unexpected staged data")
			}
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("inspect staged Workspace copy target: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_revisions (workspace_id,revision,file_count,total_bytes,directory_count)
VALUES ($1,1,$2,$3,$4)`, targetWorkspaceID, len(files), fileBytes, len(emptyDirectories)); err != nil {
			return fmt.Errorf("create Workspace copy revisions: %w", err)
		}
		for _, file := range files {
			chunks, err := json.Marshal(nonNilChunks(file.Chunks))
			if err != nil {
				return fmt.Errorf("encode copied Workspace chunks: %w", err)
			}
			xattrsJSON, aclJSON, err := workspaceNativeMetadataJSON(file.Xattrs, file.ACL)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_revision_files
(workspace_id,valid_from_revision,file_path,folded_path,size_bytes,sha256,mode,kind,symlink_target,mod_time_ns,hardlink_to,chunks,xattrs,acl,metadata_known)
VALUES ($1,1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, targetWorkspaceID, file.Path, strings.ToLower(file.Path), file.SizeBytes, file.SHA256, int32(file.Mode),
				workspace.CanonicalFileKind(file.Kind), workspaceSymlinkTargetText(file), workspaceFileModTimeNS(file.ModTimeNS), workspaceHardlinkTargetText(file), chunks, xattrsJSON, aclJSON, file.MetadataKnown); err != nil {
				return fmt.Errorf("insert copied Workspace file %q: %w", file.Path, err)
			}
			encoded, err := json.Marshal(file)
			if err != nil {
				return fmt.Errorf("encode copied Workspace change %q: %w", file.Path, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_changes (workspace_id,revision,file_path,action,file)
VALUES ($1,1,$2,'upsert',$3)`, targetWorkspaceID, file.Path, encoded); err != nil {
				return fmt.Errorf("insert copied Workspace change %q: %w", file.Path, err)
			}
		}
		for _, directory := range emptyDirectories {
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_revision_empty_directories
(workspace_id,valid_from_revision,directory_path,folded_path) VALUES ($1,1,$2,$3)`, targetWorkspaceID, directory, strings.ToLower(directory)); err != nil {
				return fmt.Errorf("insert copied Workspace directory %q: %w", directory, err)
			}
		}
		for _, directory := range directories {
			xattrsJSON, aclJSON, err := workspaceNativeMetadataJSON(directory.Xattrs, directory.ACL)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_revision_directories
(workspace_id,valid_from_revision,directory_path,folded_path,mode,mod_time_ns,xattrs,acl,metadata_known) VALUES ($1,1,$2,$3,$4,$5,$6,$7,$8)`,
				targetWorkspaceID, directory.Path, strings.ToLower(directory.Path), workspaceDirectoryMode(directory.Mode), workspaceDirectoryModTime(directory.ModTimeNS),
				xattrsJSON, aclJSON, directory.MetadataKnown); err != nil {
				return fmt.Errorf("insert copied Workspace directory metadata %q: %w", directory.Path, err)
			}
			encoded, err := json.Marshal(directory)
			if err != nil {
				return fmt.Errorf("encode copied Workspace directory change %q: %w", directory.Path, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_changes (workspace_id,revision,file_path,action,file,directory)
VALUES ($1,1,$2,'directory_upsert',NULL,$3)`, targetWorkspaceID, directory.Path, encoded); err != nil {
				return fmt.Errorf("insert copied Workspace directory metadata change %q: %w", directory.Path, err)
			}
		}
		if err := ensureWorkspaceTreeShape(ctx, tx, targetWorkspaceID, 1); err != nil {
			return err
		}
		if err := ensureWorkspaceHardlinkShape(ctx, tx, targetWorkspaceID, 1); err != nil {
			return err
		}
		updated, err := tx.Exec(ctx, `UPDATE workspace_current_materializations SET desired_revision=1,updated_at=clock_timestamp()
WHERE workspace_id=$1 AND desired_revision=0 AND applied_revision=-1`, targetWorkspaceID)
		if err != nil || updated.RowsAffected() != 1 {
			return errors.Join(domain.NewError(domain.CodeWorkspaceConflict, "reserved Workspace copy materialization changed"), err)
		}
		updated, err = tx.Exec(ctx, `UPDATE workspaces SET name=$2,revision=1,used_bytes=$3,updated_at=clock_timestamp()
WHERE id=$1 AND state='copying' AND revision=0`, targetWorkspaceID, name, fileBytes)
		if err != nil || updated.RowsAffected() != 1 {
			return errors.Join(domain.NewError(domain.CodeWorkspaceConflict, "reserved Workspace copy target changed"), err)
		}
		for digest, allocated := range allocationByDigest {
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_block_usage (workspace_id,sha256,state,allocated_bytes,reserved_bytes)
VALUES ($1,$2,'stored',$3,0) ON CONFLICT (workspace_id,sha256) DO UPDATE
SET state='stored',allocated_bytes=EXCLUDED.allocated_bytes,reserved_bytes=0,updated_at=clock_timestamp()`, targetWorkspaceID, digest, allocated); err != nil {
				return fmt.Errorf("record copied Workspace block allocation: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) CompleteWorkspaceCopy(ctx context.Context, reservationID, ownerID string, maxOwnerBytes int64) error {
	if !domain.ValidPendingRequestID(reservationID) || !domain.ValidUserID(ownerID) || maxOwnerBytes <= 0 {
		return domain.InvalidArgument("workspace_copy", "Workspace copy completion request is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var storedTargetOwner, targetWorkspaceID, state string
		var reservedBytes int64
		var readOnly bool
		if err := tx.QueryRow(ctx, `SELECT target_owner_id,target_workspace_id,reserved_bytes,read_only,state
FROM workspace_copy_reservations WHERE id=$1 FOR UPDATE`, reservationID).Scan(&storedTargetOwner, &targetWorkspaceID, &reservedBytes, &readOnly, &state); err != nil {
			return storeError("lock Workspace copy before completion", "Workspace copy", err)
		}
		if storedTargetOwner != ownerID {
			return domain.NewError(domain.CodeNotFound, "Workspace copy not found")
		}
		if state == string(workspace.CopyCompleted) {
			return nil
		}
		if state != string(workspace.Copying) && state != string(workspace.CopyUnknown) {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy is not ready to complete")
		}
		var targetOwner, targetState string
		var targetRevision, desiredRevision, appliedRevision int64
		var targetReadOnly bool
		if err := tx.QueryRow(ctx, `SELECT w.owner_id,w.state,w.revision,w.is_read_only,m.desired_revision,m.applied_revision
FROM workspaces w JOIN workspace_current_materializations m ON m.workspace_id=w.id
WHERE w.id=$1 FOR UPDATE OF w,m`, targetWorkspaceID).Scan(
			&targetOwner, &targetState, &targetRevision, &targetReadOnly, &desiredRevision, &appliedRevision); err != nil {
			return storeError("verify staged Workspace copy projection", "workspace", err)
		}
		if targetOwner != ownerID || targetReadOnly != readOnly || targetState != string(workspace.StateCopying) || targetRevision != 1 || desiredRevision != 1 ||
			(appliedRevision != -1 && appliedRevision != 1) {
			return domain.NewError(domain.CodeWorkspaceConflict, "staged Workspace copy projection is not ready")
		}
		var volumeState, volumeName string
		if err := tx.QueryRow(ctx, `SELECT state,volume_name FROM workspace_volumes WHERE workspace_id=$1 AND owner_id=$2 FOR UPDATE`, targetWorkspaceID, ownerID).
			Scan(&volumeState, &volumeName); err != nil || volumeName != app.WorkspaceNamedVolumeName(targetWorkspaceID) ||
			(volumeState != "pending" && volumeState != "ready") {
			return domain.NewError(domain.CodeWorkspaceConflict, "copied Workspace volume registry is invalid")
		}
		var copiedAllocated int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(allocated_bytes),0) FROM workspace_block_usage WHERE workspace_id=$1 AND state='stored'`, targetWorkspaceID).
			Scan(&copiedAllocated); err != nil {
			return fmt.Errorf("sum copied Workspace blocks: %w", err)
		}
		if copiedAllocated > reservedBytes {
			return domain.NewError(domain.CodeQuotaExceeded, "copied Workspace blocks exceeded reserved physical capacity")
		}
		_, ownerReserved, err := lockWorkspaceOwnerUsage(ctx, tx, ownerID)
		if err != nil {
			return err
		}
		if ownerReserved < reservedBytes {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy capacity reservation is missing")
		}
		var ownerAllocated int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(u.allocated_bytes),0)
FROM workspace_block_usage u JOIN workspaces w ON w.id=u.workspace_id WHERE w.owner_id=$1`, ownerID).Scan(&ownerAllocated); err != nil {
			return fmt.Errorf("reconcile copied Workspace owner allocation: %w", err)
		}
		if ownerReserved-reservedBytes > maxOwnerBytes || ownerAllocated > maxOwnerBytes-(ownerReserved-reservedBytes) {
			return domain.NewError(domain.CodeQuotaExceeded, "copied Workspace exceeds the owner's physical storage quota")
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_owner_block_usage SET allocated_bytes=$2,
reserved_bytes=reserved_bytes-$3,updated_at=clock_timestamp() WHERE owner_id=$1`, ownerID, ownerAllocated, reservedBytes); err != nil {
			return fmt.Errorf("settle Workspace copy capacity: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_volumes SET state='ready',ready_at=COALESCE(ready_at,clock_timestamp()),updated_at=clock_timestamp()
WHERE workspace_id=$1 AND state IN ('pending','ready')`, targetWorkspaceID); err != nil {
			return fmt.Errorf("mark copied Workspace volume ready: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_current_materializations SET applied_revision=1,updated_at=clock_timestamp()
WHERE workspace_id=$1 AND desired_revision=1 AND applied_revision IN (-1,1)`, targetWorkspaceID); err != nil {
			return fmt.Errorf("confirm copied Workspace current projection: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE workspaces SET state='ready',updated_at=clock_timestamp()
WHERE id=$1 AND state='copying'`, targetWorkspaceID); err != nil {
			return fmt.Errorf("publish copied Workspace: %w", err)
		}
		updated, err := tx.Exec(ctx, `UPDATE workspace_copy_reservations SET state='completed',updated_at=clock_timestamp()
WHERE id=$1 AND state IN ('copying','unknown')`, reservationID)
		if err != nil || updated.RowsAffected() != 1 {
			return errors.Join(domain.NewError(domain.CodeWorkspaceConflict, "Workspace copy reservation changed before completion"), err)
		}
		return nil
	})
}

func validateWorkspaceCopySnapshot(files []workspace.File, emptyDirectories []string, directories []workspace.Directory, requireKnownMetadata bool) (int64, map[string]int64, error) {
	paths := mordesync.ManifestPathValidator{}
	blockSizes := make(map[string]int64)
	var totalBytes int64
	directoryPaths := append([]string(nil), emptyDirectories...)
	for _, directory := range directories {
		directoryPaths = append(directoryPaths, directory.Path)
	}
	if err := workspace.ValidateNoSymlinkDescendants(files, directoryPaths); err != nil {
		return 0, nil, domain.InvalidArgument("workspace_copy", "Workspace copy contains a path below a symlink")
	}
	for _, file := range files {
		if err := workspace.ValidateFile(file); err != nil {
			return 0, nil, domain.InvalidArgument("workspace_copy", "Workspace copy contains an invalid file")
		}
		if requireKnownMetadata && !file.MetadataKnown {
			return 0, nil, domain.InvalidArgument("workspace_copy", "Workspace copy file metadata is incomplete")
		}
		if err := paths.Add(file.Path); err != nil {
			return 0, nil, domain.InvalidArgument("workspace_copy", "Workspace copy contains conflicting paths")
		}
		if totalBytes > math.MaxInt64-file.SizeBytes {
			return 0, nil, domain.NewError(domain.CodeQuotaExceeded, "Workspace copy size exceeds storage limits")
		}
		totalBytes += file.SizeBytes
		for _, chunk := range file.Chunks {
			if size, exists := blockSizes[chunk.SHA256]; exists && size != chunk.SizeBytes {
				return 0, nil, domain.InvalidArgument("workspace_copy", "Workspace copy contains conflicting block sizes")
			}
			blockSizes[chunk.SHA256] = chunk.SizeBytes
		}
	}
	if err := workspace.ValidateHardlinkAnchors(files); err != nil {
		return 0, nil, domain.InvalidArgument("workspace_copy", "Workspace copy contains an invalid hardlink relation")
	}
	for _, directory := range emptyDirectories {
		if err := paths.AddEmptyDirectory(directory); err != nil {
			return 0, nil, domain.InvalidArgument("workspace_copy", "Workspace copy contains conflicting directory paths")
		}
	}
	expectedDirectories := make(map[string]string)
	addDirectory := func(path string, includeSelf bool) {
		parts := strings.Split(path, "/")
		limit := len(parts)
		if !includeSelf {
			limit--
		}
		prefix := ""
		for index := 0; index < limit; index++ {
			if index > 0 {
				prefix += "/"
			}
			prefix += parts[index]
			expectedDirectories[strings.ToLower(prefix)] = prefix
		}
	}
	for _, file := range files {
		addDirectory(file.Path, false)
	}
	for _, directory := range emptyDirectories {
		addDirectory(directory, true)
	}
	seenDirectories := make(map[string]struct{}, len(directories))
	rootSeen := false
	for _, directory := range directories {
		if err := workspace.ValidateDirectory(directory, requireKnownMetadata); err != nil {
			return 0, nil, domain.InvalidArgument("workspace_copy", "Workspace copy directory metadata is invalid")
		}
		if requireKnownMetadata && !directory.MetadataKnown {
			return 0, nil, domain.InvalidArgument("workspace_copy", "Workspace copy directory metadata is incomplete")
		}
		key := strings.ToLower(directory.Path)
		if directory.Path == "." {
			rootSeen = true
		} else if expectedDirectories[key] != directory.Path {
			return 0, nil, domain.InvalidArgument("workspace_copy", "Workspace copy directory metadata conflicts with its tree")
		}
		if _, exists := seenDirectories[key]; exists {
			return 0, nil, domain.InvalidArgument("workspace_copy", "Workspace copy directory metadata is duplicated")
		}
		seenDirectories[key] = struct{}{}
	}
	if !rootSeen || len(seenDirectories) != len(expectedDirectories)+1 {
		return 0, nil, domain.InvalidArgument("workspace_copy", "Workspace copy directory metadata is incomplete")
	}
	return totalBytes, blockSizes, nil
}

func getWorkspaceCopyManifestTx(ctx context.Context, tx pgx.Tx, workspaceID string) ([]workspace.File, []string, []workspace.Directory, error) {
	files := make([]workspace.File, 0)
	emptyDirectories := make([]string, 0)
	directoryByPath := make(map[string]workspace.Directory)
	after := ""
	for {
		page, err := getWorkspaceManifestPageTx(ctx, tx, workspaceID, 1, "", after, maxWorkspaceSyncPage)
		if err != nil {
			return nil, nil, nil, err
		}
		files = append(files, page.Files...)
		emptyDirectories = append(emptyDirectories, page.EmptyDirectories...)
		for _, directory := range page.Directories {
			directoryByPath[strings.ToLower(directory.Path)] = directory
		}
		if page.NextCursor == nil {
			break
		}
		cursor, err := workspace.DecodeManifestCursor(*page.NextCursor)
		if err != nil || cursor.Revision != 1 || cursor.Prefix != "" || cursor.After <= after {
			return nil, nil, nil, domain.NewError(domain.CodeInternal, "staged Workspace copy manifest cursor is invalid")
		}
		after = cursor.After
	}
	directories := make([]workspace.Directory, 0, len(directoryByPath))
	for _, directory := range directoryByPath {
		directories = append(directories, directory)
	}
	sort.Slice(directories, func(i, j int) bool { return directories[i].Path < directories[j].Path })
	return files, emptyDirectories, directories, nil
}

func (s *Store) CheckWorkspaceVolumeCopyReservation(ctx context.Context, request app.WorkspaceVolumeCopyRequest) error {
	if !domain.ValidPendingRequestID(request.ReservationID) || request.ServiceInstanceID == "" || len(request.ServiceInstanceID) > 128 ||
		!domain.ValidUserID(request.SourceOwnerID) || !domain.ValidUserID(request.TargetOwnerID) || !domain.ValidWorkspaceID(request.SourceWorkspaceID) ||
		!domain.ValidWorkspaceID(request.TargetWorkspaceID) || request.SourceWorkspaceID == request.TargetWorkspaceID {
		return domain.InvalidArgument("workspace_copy", "Workspace copy volume request is invalid")
	}
	var active bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_copy_reservations c JOIN workspaces w ON w.id=c.source_workspace_id
JOIN workspaces t ON t.id=c.target_workspace_id
WHERE c.id=$1 AND c.source_owner_id=$2 AND c.target_owner_id=$3 AND c.source_workspace_id=$4 AND c.target_workspace_id=$5
AND c.service_instance_id=$6 AND c.source_revision=$7 AND w.owner_id=c.source_owner_id AND w.revision=c.source_revision
AND t.owner_id=c.target_owner_id AND t.is_read_only=c.read_only
AND c.state IN ('reserved','copying','unknown')
)`, request.ReservationID, request.SourceOwnerID, request.TargetOwnerID, request.SourceWorkspaceID, request.TargetWorkspaceID, request.ServiceInstanceID, request.SourceRevision).Scan(&active)
	if err != nil {
		return fmt.Errorf("check Workspace volume copy reservation: %w", err)
	}
	if !active {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace copy reservation is not active")
	}
	return nil
}

func getWorkspaceCopyReservationTx(ctx context.Context, tx pgx.Tx, id string) (workspace.CopyReservation, error) {
	var reservation workspace.CopyReservation
	err := tx.QueryRow(ctx, `SELECT id,source_owner_id,target_owner_id,COALESCE(share_id,''),source_workspace_id,target_workspace_id,
service_instance_id,source_revision,reserved_bytes,read_only,state,created_at,updated_at FROM workspace_copy_reservations WHERE id=$1`, id).Scan(
		&reservation.ID, &reservation.SourceOwnerID, &reservation.TargetOwnerID, &reservation.ShareID, &reservation.SourceWorkspaceID,
		&reservation.TargetWorkspaceID, &reservation.ServiceInstanceID, &reservation.SourceRevision, &reservation.ReservedBytes,
		&reservation.ReadOnly, &reservation.State, &reservation.CreatedAt, &reservation.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspace.CopyReservation{}, domain.NewError(domain.CodeIdempotencyConflict, "Workspace copy receipt is missing")
	}
	if err != nil {
		return workspace.CopyReservation{}, fmt.Errorf("read Workspace copy reservation: %w", err)
	}
	if err := reservation.Validate(); err != nil {
		return workspace.CopyReservation{}, domain.NewError(domain.CodeInternal, "stored Workspace copy reservation is invalid")
	}
	return reservation, nil
}

func validWorkspaceCopyTransition(from, to workspace.CopyState) bool {
	switch from {
	case workspace.CopyReserved:
		return to == workspace.Copying || to == workspace.CopyFailed
	case workspace.Copying:
		return to == workspace.CopyUnknown || to == workspace.CopyCompleted || to == workspace.CopyFailed
	case workspace.CopyUnknown:
		return to == workspace.Copying || to == workspace.CopyFailed || to == workspace.CopyCompleted
	default:
		return false
	}
}
