package postgres

import (
	"context"
	"testing"

	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

func TestRevokeExpiredWorkspaceSyncDevicesRemovesExpiredInstallationAndPreservesRenewedOne(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	expiredWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "expired-sync-device")
	renewedWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "renewed-sync-device")
	expiredClient := syncDeviceTestPrincipal(t, store, owner.ID)
	renewedClient := syncDeviceTestPrincipal(t, store, owner.ID)
	expiredDeviceID := "EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE"
	renewedDeviceID := "FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF"
	expiredBinding := registerTestWorkspaceSyncDevice(t, store, expiredClient, expiredWorkspace.ID,
		expiredDeviceID, "expired installation certificate")
	renewedBinding := registerTestWorkspaceSyncDevice(t, store, renewedClient, renewedWorkspace.ID,
		renewedDeviceID, "renewed installation certificate")
	applyWorkspaceSyncNodeState(t, store, owner.ID)

	deviceToken := clientTestToken(t, owner.ID, expiredClient.ClientInstallationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, deviceToken); err != nil {
		t.Fatal(err)
	}
	session := newTestSyncSession(t, expiredWorkspace, owner.ID, []workspace.File{}, []workspace.File{}, nil)
	session.ReplicaID = "device:" + deviceToken.ID
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, nil,
		syncTestIdempotency(owner.ID, "workspace.sync.create", "expired-install-session", session.ID)); err != nil {
		t.Fatal(err)
	}
	challenge := newSyncDeviceTestChallenge(t, owner.ID, expiredWorkspace.ID, expiredClient.ClientInstallationID,
		expiredDeviceID, []byte("pending expired installation certificate"), make([]byte, 32))
	if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, expiredClient, challenge); err != nil {
		t.Fatal(err)
	}

	if _, err := store.pool.Exec(ctx, `UPDATE client_installations SET expires_at=clock_timestamp()-interval '1 second'
WHERE id=$1`, expiredClient.ClientInstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE client_installations SET expires_at=clock_timestamp()-interval '1 second'
WHERE id=$1`, renewedClient.ClientInstallationID); err != nil {
		t.Fatal(err)
	}
	// Simulate a successful login renewal before the scan.
	if _, err := store.pool.Exec(ctx, `UPDATE client_installations SET expires_at=clock_timestamp()+interval '30 days'
WHERE id=$1`, renewedClient.ClientInstallationID); err != nil {
		t.Fatal(err)
	}

	if err := store.RevokeExpiredWorkspaceSyncDevices(ctx); err != nil {
		t.Fatalf("revoke expired Workspace sync devices: %v", err)
	}
	assertWorkspaceSyncDeviceRevocationQueued(t, store, expiredBinding.ID)
	var challengeCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_device_challenges WHERE id=$1`, challenge.ID).Scan(&challengeCount); err != nil || challengeCount != 0 {
		t.Fatalf("expired installation challenge count = %d, err=%v; want 0", challengeCount, err)
	}
	progress, err := store.GetWorkspaceSyncSession(ctx, expiredWorkspace.ID, session.ID)
	if err != nil || progress.State != workspace.SyncCancelled {
		t.Fatalf("expired installation sync session state = %q, err=%v; want cancelled", progress.State, err)
	}
	var renewedState string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM workspace_sync_device_bindings WHERE id=$1`, renewedBinding.ID).Scan(&renewedState); err != nil || renewedState != string(workspace.SyncDeviceBindingActive) {
		t.Fatalf("renewed installation binding state = %q, err=%v; want active", renewedState, err)
	}

	desired := applyWorkspaceSyncNodeState(t, store, owner.ID)
	if len(desired.Folders) != 1 || desired.Folders[0].WorkspaceID != renewedWorkspace.ID ||
		len(desired.Folders[0].DeviceIDs) != 1 || desired.Folders[0].DeviceIDs[0] != renewedDeviceID {
		t.Fatalf("node desired state after expiry cleanup = %#v; want only renewed installation", desired)
	}
	assertWorkspaceSyncDeviceRevoked(t, store, expiredBinding.ID)
	if err := store.pool.QueryRow(ctx, `SELECT state FROM workspace_sync_device_bindings WHERE id=$1`, renewedBinding.ID).Scan(&renewedState); err != nil || renewedState != string(workspace.SyncDeviceBindingActive) {
		t.Fatalf("renewed installation binding after reconciliation = %q, err=%v; want active", renewedState, err)
	}
}
