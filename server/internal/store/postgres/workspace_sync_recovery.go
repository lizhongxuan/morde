package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"
)

const workspaceSyncCreateRecoveryTombstoneResource = "workspace_sync_create_recovery_tombstone"

func (s *Store) ResolveWorkspaceSyncCreate(ctx context.Context, ownerID, workspaceID, replicaID, key string) (workspace.SyncSession, bool, error) {
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) ||
		!validSyncRecoveryKey(key) || !validReplicaID(replicaID) {
		return workspace.SyncSession{}, false, domain.InvalidArgument("workspace_sync_recovery", "workspace sync recovery identity is invalid")
	}
	if strings.HasPrefix(replicaID, "host:") {
		return workspace.SyncSession{}, false, domain.NewError(domain.CodeForbidden, "Host replicas cannot recover owner sync checkpoints")
	}
	sessionReplica := replicaID == "session:"+ownerID
	deviceReplica := strings.HasPrefix(replicaID, "device:")
	if !sessionReplica && !deviceReplica {
		return workspace.SyncSession{}, false, domain.NewError(domain.CodeForbidden, "workspace checkpoint recovery replica is invalid")
	}

	var result workspace.SyncSession
	found := false
	resolve := func(tx pgx.Tx) error {
		var deviceAccess *workspaceSyncDeviceAccess
		if deviceReplica {
			var err error
			deviceAccess, err = prepareWorkspaceSyncDeviceReplica(ctx, tx, ownerID, workspaceID, replicaID)
			if err != nil {
				return err
			}
			if deviceAccess == nil {
				return domain.NewError(domain.CodeNotFound, "workspace not found")
			}
			if err := requireActiveWorkspaceSyncDeviceWorkspaceAccess(ctx, tx, deviceAccess); err != nil {
				return err
			}
		}

		var workspaceOwner, workspaceState string
		var deletedAt pgtype.Timestamptz
		err := tx.QueryRow(ctx, `SELECT owner_id,state,deleted_at FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).
			Scan(&workspaceOwner, &workspaceState, &deletedAt)
		if errors.Is(err, pgx.ErrNoRows) || err == nil &&
			(workspaceOwner != ownerID || workspaceState != string(workspace.StateReady) || deletedAt.Valid) {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if err != nil {
			return fmt.Errorf("lock Workspace for sync checkpoint recovery: %w", err)
		}
		if err := requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, workspaceID); err != nil {
			return err
		}

		tombstoneHash := sha256.Sum256([]byte("morde:workspace.sync.create:recovery-tombstone:v1\x00" + ownerID + "\x00" + workspaceID + "\x00" + key))
		tombstone := app.IdempotencyRecord{
			OwnerID: ownerID, Operation: "workspace.sync.create", Key: key, RequestHash: tombstoneHash[:],
			ResourceID: workspaceSyncCreateRecoveryTombstoneResource, RelatedID: "workspace:" + workspaceID,
		}
		if err := validateIdempotency(tombstone); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO idempotency_keys (owner_id,operation,key,request_hash,resource_id,related_id)
VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (owner_id,operation,key) DO NOTHING`, tombstone.OwnerID,
			tombstone.Operation, tombstone.Key, tombstone.RequestHash, tombstone.ResourceID, tombstone.RelatedID); err != nil {
			return fmt.Errorf("reserve Workspace sync create recovery tombstone: %w", err)
		}

		var resourceID string
		if err := tx.QueryRow(ctx, `SELECT resource_id FROM idempotency_keys
WHERE owner_id=$1 AND operation='workspace.sync.create' AND key=$2 FOR UPDATE`, ownerID, key).Scan(&resourceID); err != nil {
			return fmt.Errorf("read Workspace sync create recovery reservation: %w", err)
		}
		if resourceID == workspaceSyncCreateRecoveryTombstoneResource || resourceID == workspaceSyncCreateQuotaRejectedResource ||
			!workspace.ValidSessionID(resourceID) {
			return nil
		}

		row, err := sqlcgen.New(tx).GetWorkspaceSyncSession(ctx, sqlcgen.GetWorkspaceSyncSessionParams{
			WorkspaceID: workspaceID, ID: resourceID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read Workspace sync checkpoint session: %w", err)
		}
		if row.OwnerID != ownerID || row.ReplicaID != replicaID {
			return nil
		}
		result, err = syncSessionDomain(ctx, tx, sqlcgen.New(tx), row)
		if err != nil {
			return err
		}
		found = true
		return nil
	}

	var err error
	if sessionReplica {
		err = s.withApprovedWorkspaceSyncOwnerReadTx(ctx, ownerID, resolve)
	} else {
		err = s.WithTx(ctx, resolve)
	}
	if err != nil {
		return workspace.SyncSession{}, false, err
	}
	return result, found, nil
}

func validSyncRecoveryKey(value string) bool {
	if len(value) == 0 || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' ||
			char == '.' || char == '_' || char == '~' || char == '-') {
			return false
		}
	}
	return true
}
