package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

func TestWorkspaceSyncDeviceRevocationFencesHTTPAccessAndCancelsItsSession(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	firstWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "sync-device-auth-first")
	secondWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "sync-device-auth-second")
	client := syncDeviceTestPrincipal(t, store, owner.ID)
	deviceToken := clientTestToken(t, owner.ID, client.ClientInstallationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, deviceToken); err != nil {
		t.Fatal(err)
	}
	device := identity.Principal{
		UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindDevice,
		TokenID: deviceToken.ID, ClientInstallationID: client.ClientInstallationID,
		Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	deviceID := "AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA"
	firstBinding := registerTestWorkspaceSyncDevice(t, store, client, firstWorkspace.ID, deviceID, "shared-device-certificate")
	secondBinding := registerTestWorkspaceSyncDevice(t, store, client, secondWorkspace.ID, deviceID, "shared-device-certificate")
	applyWorkspaceSyncNodeState(t, store, owner.ID)
	for _, workspaceID := range []string{firstWorkspace.ID, secondWorkspace.ID} {
		if err := store.CheckWorkspaceSyncDeviceAccess(ctx, device, workspaceID); err != nil {
			t.Fatalf("active device access to %s: %v", workspaceID, err)
		}
	}

	session := newTestSyncSession(t, firstWorkspace, owner.ID, []workspace.File{}, []workspace.File{}, nil)
	session.ReplicaID = "device:" + deviceToken.ID
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, nil,
		syncTestIdempotency(owner.ID, "workspace.sync.create", "device-revoke-session", session.ID)); err != nil {
		t.Fatal(err)
	}
	portalToken := clientTestToken(t, owner.ID, "", identity.KindSession, nil)
	if _, err := store.CreateToken(ctx, portalToken); err != nil {
		t.Fatal(err)
	}
	portal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: portalToken.ID}
	if err := store.RevokeWorkspaceSyncDevice(ctx, portal, firstWorkspace.ID, firstBinding.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckWorkspaceSyncDeviceAccess(ctx, device, firstWorkspace.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked Workspace access = %v; want not found", err)
	}
	streamCalled := false
	if err := store.WithWorkspaceSyncDeviceSessionAccess(ctx, device, firstWorkspace.ID, session.ID, func() error {
		streamCalled = true
		return nil
	}); err == nil || streamCalled {
		t.Fatalf("revoked Workspace stream = %v, callback called %t; want denied before callback", err, streamCalled)
	}
	if err := store.CheckWorkspaceSyncDeviceAccess(ctx, device, secondWorkspace.ID); err != nil {
		t.Fatalf("remaining Workspace access = %v", err)
	}
	cancelled, err := store.GetWorkspaceSyncSession(ctx, firstWorkspace.ID, session.ID)
	if err != nil || cancelled.State != workspace.SyncCancelled {
		t.Fatalf("session after single Workspace revoke = %q, %v; want cancelled", cancelled.State, err)
	}
	desired := applyWorkspaceSyncNodeState(t, store, owner.ID)
	if len(desired.Folders) != 1 || desired.Folders[0].WorkspaceID != secondWorkspace.ID {
		t.Fatalf("desired folders after one Workspace revoke = %#v; second binding %s", desired.Folders, secondBinding.ID)
	}
}

func TestWorkspaceSyncDeviceRevocationWaitsForActiveBlockChunk(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-device-stream-revoke")
	client := syncDeviceTestPrincipal(t, store, owner.ID)
	deviceToken := clientTestToken(t, owner.ID, client.ClientInstallationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, deviceToken); err != nil {
		t.Fatal(err)
	}
	device := identity.Principal{
		UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindDevice,
		TokenID: deviceToken.ID, ClientInstallationID: client.ClientInstallationID,
		Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	binding := registerTestWorkspaceSyncDevice(t, store, client, value.ID,
		"CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC", "active-stream-certificate")
	applyWorkspaceSyncNodeState(t, store, owner.ID)
	session := newTestSyncSession(t, value, owner.ID, []workspace.File{}, []workspace.File{}, nil)
	session.ReplicaID = "device:" + deviceToken.ID
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, nil,
		syncTestIdempotency(owner.ID, "workspace.sync.create", "device-stream-revoke", session.ID)); err != nil {
		t.Fatal(err)
	}
	portalToken := clientTestToken(t, owner.ID, "", identity.KindSession, nil)
	if _, err := store.CreateToken(ctx, portalToken); err != nil {
		t.Fatal(err)
	}
	portal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: portalToken.ID}

	entered := make(chan struct{})
	release := make(chan struct{})
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- store.WithWorkspaceSyncDeviceSessionAccess(ctx, device, value.ID, session.ID, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("authorized block stream did not start")
	}

	revokeStarted := make(chan struct{})
	revokeDone := make(chan error, 1)
	go func() {
		close(revokeStarted)
		revokeDone <- store.RevokeWorkspaceSyncDevice(ctx, portal, value.ID, binding.ID)
	}()
	<-revokeStarted
	select {
	case err := <-revokeDone:
		t.Fatalf("revocation completed while block stream was active: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-streamDone; err != nil {
		t.Fatalf("active block stream: %v", err)
	}
	if err := <-revokeDone; err != nil {
		t.Fatalf("device revocation after block stream: %v", err)
	}
	if err := store.CheckWorkspaceSyncDeviceAccess(ctx, device, value.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("device access after stream and revocation = %v; want not found", err)
	}
	secondChunkCalled := false
	if err := store.WithWorkspaceSyncDeviceSessionAccess(ctx, device, value.ID, session.ID, func() error {
		secondChunkCalled = true
		return nil
	}); err == nil || secondChunkCalled {
		t.Fatalf("next block chunk after revocation = %v, callback called %t; want denied", err, secondChunkCalled)
	}
}

func activateTestWorkspaceSyncDeviceBinding(t *testing.T, store *Store, ownerID, workspaceID, installationID, deviceTokenID string, deviceIDChar byte) {
	t.Helper()
	clientToken := clientTestToken(t, ownerID, installationID, identity.KindClient, nil)
	if _, err := store.CreateToken(context.Background(), clientToken); err != nil {
		t.Fatal(err)
	}
	client := identity.Principal{
		UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: clientToken.ID, ClientInstallationID: installationID,
	}
	deviceID := strings.Repeat(string(deviceIDChar), 7)
	deviceID = strings.Repeat(deviceID+"-", 7) + deviceID
	registerTestWorkspaceSyncDevice(t, store, client, workspaceID, deviceID, "HTTP sync device "+deviceTokenID)
	applyWorkspaceSyncNodeState(t, store, ownerID)
}
