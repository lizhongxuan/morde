package postgres

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func TestWorkspaceCopyOwnerSplitMigrationPreservesLegacyTransferredCopy(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	sourceOwner := createTestUser(t, store, 8, 20_000, 40_000)
	recipient := createTestUser(t, store, 8, 20_000, 40_000)
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 161); err != nil {
		t.Fatalf("roll back empty Workspace copy owner-split migration: %v", err)
	}

	source := createTransferReadyWorkspace(t, ctx, store, sourceOwner.ID, "legacy share source")
	legacyTarget := createTransferReadyWorkspace(t, ctx, store, sourceOwner.ID, "legacy shared copy")
	transfers := newTestWorkspaceOwnershipTransferService(t, store, 100)
	transfer, replayed, err := transfers.RequestWorkspaceTransfer(ctx, workspaceTransferPrincipal(sourceOwner.ID), legacyTarget.ID,
		recipient.ID, legacyTarget.Revision, "legacy-share-copy-transfer")
	if err != nil || replayed {
		t.Fatalf("request legacy copy ownership transfer = %#v, replayed=%t, err=%v", transfer, replayed, err)
	}
	accepted, err := transfers.AcceptWorkspaceTransferReadOnly(ctx, workspaceTransferPrincipal(recipient.ID), legacyTarget.ID,
		transfer.ID, "legacy-share-copy-transfer-accept")
	if err != nil || accepted.State != workspace.OwnershipTransferAccepted {
		t.Fatalf("accept legacy copy ownership transfer = %#v, err=%v", accepted, err)
	}

	legacyReservationID, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_copy_reservations
(id,owner_id,source_workspace_id,target_workspace_id,service_instance_id,source_revision,reserved_bytes,state)
VALUES ($1,$2,$3,$4,'legacy-share-copy',0,0,'completed')`, legacyReservationID, sourceOwner.ID, source.ID, legacyTarget.ID); err != nil {
		t.Fatalf("insert pre-migration transferred copy reservation: %v", err)
	}
	var historyOwner string
	var historyTransferID *string
	if err := pool.QueryRow(ctx, `SELECT owner_id,transfer_id FROM workspace_ownership_history
WHERE workspace_id=$1 AND ended_at IS NULL`, legacyTarget.ID).Scan(&historyOwner, &historyTransferID); err != nil ||
		historyOwner != recipient.ID || historyTransferID == nil || *historyTransferID != transfer.ID {
		t.Fatalf("legacy target ownership history = owner=%q transfer=%v err=%v", historyOwner, historyTransferID, err)
	}

	if _, err := provider.UpTo(ctx, 162); err != nil {
		t.Fatalf("apply Workspace copy owner-split migration to legacy row: %v", err)
	}
	var migratedSourceOwner, migratedTargetOwner, migratedShareID, migratedState string
	var migratedReadOnly bool
	if err := pool.QueryRow(ctx, `SELECT source_owner_id,target_owner_id,COALESCE(share_id,''),read_only,state
FROM workspace_copy_reservations WHERE id=$1`, legacyReservationID).Scan(
		&migratedSourceOwner, &migratedTargetOwner, &migratedShareID, &migratedReadOnly, &migratedState); err != nil ||
		migratedSourceOwner != sourceOwner.ID || migratedTargetOwner != recipient.ID || migratedShareID != "" ||
		!migratedReadOnly || migratedState != string(workspace.CopyCompleted) {
		t.Fatalf("migrated legacy copy reservation = %q/%q/%q/%t/%q, err=%v", migratedSourceOwner,
			migratedTargetOwner, migratedShareID, migratedReadOnly, migratedState, err)
	}

	if _, err := provider.DownTo(ctx, 161); err != nil {
		t.Fatalf("roll back migration with a legacy completed copy: %v", err)
	}
	var storedOwner, targetOwner string
	var targetReadOnly bool
	if err := pool.QueryRow(ctx, `SELECT c.owner_id,w.owner_id,w.is_read_only
FROM workspace_copy_reservations c JOIN workspaces w ON w.id=c.target_workspace_id WHERE c.id=$1`, legacyReservationID).
		Scan(&storedOwner, &targetOwner, &targetReadOnly); err != nil || storedOwner != sourceOwner.ID || targetOwner != recipient.ID || !targetReadOnly {
		t.Fatalf("legacy copy after safe rollback = source owner %q, target owner %q, read-only %t, err=%v",
			storedOwner, targetOwner, targetReadOnly, err)
	}
	if _, err := provider.UpTo(ctx, 162); err != nil {
		t.Fatalf("reapply Workspace copy owner-split migration: %v", err)
	}

	shareID, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	tokenHash := sha256.Sum256([]byte("new direct copy migration guard"))
	_, err = store.CreateWorkspaceShare(ctx, workspace.ShareLink{
		ID: shareID, OwnerID: sourceOwner.ID, SourceWorkspaceID: source.ID, SourceRevision: source.Revision,
		Permission: workspace.ShareView, TokenHash: tokenHash[:], ExpiresAt: time.Now().Add(7 * 24 * time.Hour), State: workspace.SharePending,
	})
	if err != nil {
		t.Fatalf("create direct Workspace share for down guard: %v", err)
	}
	if _, err := store.StartWorkspaceShareClaim(ctx, tokenHash[:], recipient.ID); err != nil {
		t.Fatalf("claim direct Workspace share for down guard: %v", err)
	}
	target := createTransferReadyWorkspace(t, ctx, store, recipient.ID, "direct share copy migration guard")
	reservationID, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_copy_reservations
(id,owner_id,source_owner_id,target_owner_id,share_id,read_only,source_workspace_id,target_workspace_id,
 service_instance_id,source_revision,reserved_bytes,state)
VALUES ($1,$2,$3,$4,$5,true,$6,$7,'direct-share-migration-test',$8,0,'reserved')`, reservationID, sourceOwner.ID,
		sourceOwner.ID, recipient.ID, shareID, source.ID, target.ID, source.Revision); err != nil {
		t.Fatalf("insert direct recipient-owned copy for down guard: %v", err)
	}
	if _, err := provider.DownTo(ctx, 161); err == nil || !strings.Contains(err.Error(), "cannot roll back recipient-owned Workspace share copies") {
		t.Fatalf("down migration with a new direct share copy = %v, want recipient-owned copy guard", err)
	}
}
