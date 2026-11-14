package postgres

import (
	"context"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

func TestWorkspaceSyncCheckpointRecoveryTombstonesMissingCreateKey(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "sync-recovery-tombstone")
	key := "legacy-checkpoint-tombstone-0001"

	if recovered, found, err := store.ResolveWorkspaceSyncCreate(ctx, owner.ID, workspaceValue.ID, "session:"+owner.ID, key); err != nil || found {
		t.Fatalf("recover missing checkpoint = %#v, found=%t, err=%v; want tombstoned miss", recovered, found, err)
	}

	var operation, resourceID, relatedID string
	var requestHash []byte
	if err := store.pool.QueryRow(ctx, `SELECT operation, request_hash, resource_id, related_id
FROM idempotency_keys WHERE owner_id=$1 AND operation='workspace.sync.create' AND key=$2`, owner.ID, key).
		Scan(&operation, &requestHash, &resourceID, &relatedID); err != nil {
		t.Fatalf("read recovery tombstone: %v", err)
	}
	if operation != "workspace.sync.create" || len(requestHash) != 32 ||
		resourceID != workspaceSyncCreateRecoveryTombstoneResource || relatedID != "workspace:"+workspaceValue.ID {
		t.Fatalf("recovery reservation = operation:%q hash_bytes:%d resource:%q related:%q; want immutable workspace tombstone",
			operation, len(requestHash), resourceID, relatedID)
	}

	lateSession := newTestSyncSession(t, workspaceValue, owner.ID, []workspace.File{}, []workspace.File{}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, lateSession, nil,
		syncTestIdempotency(owner.ID, "workspace.sync.create", key, lateSession.ID)); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("late create using tombstoned key = %v; want idempotency conflict", err)
	}
	var sessionCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2`,
		workspaceValue.ID, lateSession.ID).Scan(&sessionCount); err != nil || sessionCount != 0 {
		t.Fatalf("late session count = %d, err=%v; want zero", sessionCount, err)
	}
}

func TestWorkspaceSyncCheckpointRecoveryScopesWorkspaceAndReplica(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "sync-recovery-scope")
	otherWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "sync-recovery-other-workspace")
	key := "legacy-checkpoint-scope-0001"

	session := newTestSyncSession(t, workspaceValue, owner.ID, []workspace.File{}, []workspace.File{}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, nil,
		syncTestIdempotency(owner.ID, "workspace.sync.create", key, session.ID)); err != nil {
		t.Fatalf("create checkpoint session: %v", err)
	}

	recovered, found, err := store.ResolveWorkspaceSyncCreate(ctx, owner.ID, workspaceValue.ID, "session:"+owner.ID, key)
	if err != nil || !found || recovered.ID != session.ID || recovered.WorkspaceID != workspaceValue.ID || recovered.ReplicaID != session.ReplicaID {
		t.Fatalf("recover matching checkpoint = %#v, found=%t, err=%v", recovered, found, err)
	}

	if leaked, found, err := store.ResolveWorkspaceSyncCreate(ctx, owner.ID, otherWorkspace.ID, "session:"+owner.ID, key); err != nil || found {
		t.Fatalf("recover checkpoint from another Workspace = %#v, found=%t, err=%v; want opaque miss", leaked, found, err)
	}

	client := syncDeviceTestPrincipal(t, store, owner.ID)
	deviceToken := clientTestToken(t, owner.ID, client.ClientInstallationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, deviceToken); err != nil {
		t.Fatalf("create alternate device token: %v", err)
	}
	registerTestWorkspaceSyncDevice(t, store, client, workspaceValue.ID,
		"EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE", "recovery-replica-certificate")
	applyWorkspaceSyncNodeState(t, store, owner.ID)

	if leaked, found, err := store.ResolveWorkspaceSyncCreate(ctx, owner.ID, workspaceValue.ID, "device:"+deviceToken.ID, key); err != nil || found {
		t.Fatalf("recover checkpoint from another replica = %#v, found=%t, err=%v; want opaque miss", leaked, found, err)
	}
}
