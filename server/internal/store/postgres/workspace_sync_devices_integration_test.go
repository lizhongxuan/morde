package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

func TestWorkspaceSyncDeviceRegistrationIsInstallationBoundAndIdempotent(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	otherOwner := createTestUser(t, store, 3, 10_000, 20_000)
	workspaceOne := createTestWorkspace(t, ctx, store, owner.ID, "sync-device-one")
	workspaceTwo := createTestWorkspace(t, ctx, store, owner.ID, "sync-device-two")
	otherWorkspace := createTestWorkspace(t, ctx, store, otherOwner.ID, "sync-device-other")
	principal := syncDeviceTestPrincipal(t, store, owner.ID)
	otherPrincipal := syncDeviceTestPrincipal(t, store, otherOwner.ID)
	certificateDER := []byte("public syncthing certificate")
	certificateHash := sha256.Sum256(certificateDER)
	deviceID := "AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA"

	firstChallenge := newSyncDeviceTestChallenge(t, owner.ID, workspaceOne.ID, principal.ClientInstallationID, deviceID, certificateDER, certificateHash[:])
	if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, principal, firstChallenge); err != nil {
		t.Fatal(err)
	}
	keyHash, requestHash := syncDeviceTestHashes("registration-key-001", workspaceOne.ID, principal.ClientInstallationID, deviceID, certificateHash[:])
	firstBindingID, err := domain.NewWorkspaceSyncDeviceBindingID()
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.RegisterWorkspaceSyncDevice(ctx, principal, workspaceOne.ID, firstChallenge.ID, keyHash, requestHash, firstBindingID, "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil || first.State != workspace.SyncDeviceBindingPendingNode || first.FolderID != "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || first.DeviceID != deviceID {
		t.Fatalf("register first Workspace device = %#v, %v", first, err)
	}
	replayedID, err := domain.NewWorkspaceSyncDeviceBindingID()
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.RegisterWorkspaceSyncDevice(ctx, principal, workspaceOne.ID, firstChallenge.ID, keyHash, requestHash, replayedID, "morde-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil || replayed.ID != first.ID || replayed.FolderID != first.FolderID {
		t.Fatalf("replay device registration = %#v, %v; original=%#v", replayed, err, first)
	}
	conflictingHash := sha256.Sum256([]byte("different request"))
	if _, err := store.RegisterWorkspaceSyncDevice(ctx, principal, workspaceOne.ID, firstChallenge.ID, keyHash, conflictingHash[:], replayedID, "morde-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("idempotency key reused for another request = %v", err)
	}

	secondChallenge := newSyncDeviceTestChallenge(t, owner.ID, workspaceTwo.ID, principal.ClientInstallationID, deviceID, certificateDER, certificateHash[:])
	if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, principal, secondChallenge); err != nil {
		t.Fatal(err)
	}
	secondKeyHash, secondRequestHash := syncDeviceTestHashes("registration-key-002", workspaceTwo.ID, principal.ClientInstallationID, deviceID, certificateHash[:])
	secondBindingID, err := domain.NewWorkspaceSyncDeviceBindingID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.RegisterWorkspaceSyncDevice(ctx, principal, workspaceTwo.ID, secondChallenge.ID, secondKeyHash, secondRequestHash, secondBindingID, "morde-cccccccccccccccccccccccccccccccc")
	if err != nil || second.WorkspaceID != workspaceTwo.ID {
		t.Fatalf("same installation registers second Workspace = %#v, %v", second, err)
	}
	portalToken := clientTestToken(t, owner.ID, "", identity.KindSession, nil)
	if _, err := store.CreateToken(ctx, portalToken); err != nil {
		t.Fatal(err)
	}
	portalPrincipal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: portalToken.ID}
	if err := store.RevokeWorkspaceSyncDevice(ctx, portalPrincipal, workspaceOne.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	var remainingBindingState, revokedFolderState, remainingFolderState string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM workspace_sync_device_bindings WHERE id=$1`, second.ID).Scan(&remainingBindingState); err != nil || remainingBindingState != string(workspace.SyncDeviceBindingPendingNode) {
		t.Fatalf("other Workspace binding after revoke = %q, want pending_node: %v", remainingBindingState, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT state FROM workspace_sync_folders WHERE workspace_id=$1`, workspaceOne.ID).Scan(&revokedFolderState); err != nil || revokedFolderState != "revoking" {
		t.Fatalf("revoked Workspace folder state = %q, want revoking: %v", revokedFolderState, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT state FROM workspace_sync_folders WHERE workspace_id=$1`, workspaceTwo.ID).Scan(&remainingFolderState); err != nil || remainingFolderState != "pending_node" {
		t.Fatalf("other Workspace folder state = %q, want pending_node: %v", remainingFolderState, err)
	}
	desired := applyWorkspaceSyncNodeState(t, store, owner.ID)
	if desired.OwnerID != owner.ID || len(desired.Folders) != 1 || desired.Folders[0].WorkspaceID != workspaceTwo.ID ||
		desired.Folders[0].FolderID != second.FolderID || len(desired.Folders[0].DeviceIDs) != 1 || desired.Folders[0].DeviceIDs[0] != deviceID {
		t.Fatalf("desired node snapshot after one Workspace binding revoke = %#v", desired)
	}
	if err := store.pool.QueryRow(ctx, `SELECT state FROM workspace_sync_device_bindings WHERE id=$1`, first.ID).Scan(&revokedFolderState); err != nil || revokedFolderState != string(workspace.SyncDeviceBindingRevoked) {
		t.Fatalf("revoked Workspace binding after reconcile = %q, want revoked: %v", revokedFolderState, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT state FROM workspace_sync_device_bindings WHERE id=$1`, second.ID).Scan(&remainingBindingState); err != nil || remainingBindingState != string(workspace.SyncDeviceBindingActive) {
		t.Fatalf("other Workspace binding after reconcile = %q, want active: %v", remainingBindingState, err)
	}

	otherChallenge := newSyncDeviceTestChallenge(t, otherOwner.ID, otherWorkspace.ID, otherPrincipal.ClientInstallationID, deviceID, certificateDER, certificateHash[:])
	if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, otherPrincipal, otherChallenge); err != nil {
		t.Fatal(err)
	}
	otherKeyHash, otherRequestHash := syncDeviceTestHashes("registration-key-003", otherWorkspace.ID, otherPrincipal.ClientInstallationID, deviceID, certificateHash[:])
	otherBindingID, err := domain.NewWorkspaceSyncDeviceBindingID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterWorkspaceSyncDevice(ctx, otherPrincipal, otherWorkspace.ID, otherChallenge.ID, otherKeyHash, otherRequestHash, otherBindingID, "morde-dddddddddddddddddddddddddddddddd"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("cross-account device ID reuse = %v, want conflict", err)
	}
}

func TestWorkspaceSyncNodeRevisionChangesTransactionallyWithRegistration(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-node-revision-transaction")
	principal := syncDeviceTestPrincipal(t, store, owner.ID)
	deviceID := "FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF"
	certificateDER := []byte("transactional revision certificate")
	certificateHash := sha256.Sum256(certificateDER)
	challenge := newSyncDeviceTestChallenge(t, owner.ID, value.ID, principal.ClientInstallationID, deviceID, certificateDER, certificateHash[:])
	if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, principal, challenge); err != nil {
		t.Fatal(err)
	}
	keyHash, requestHash := syncDeviceTestHashes("sync-node-transactional-registration", value.ID, principal.ClientInstallationID, deviceID, certificateHash[:])
	bindingID, err := domain.NewWorkspaceSyncDeviceBindingID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `ALTER TABLE workspace_sync_device_registration_requests
ADD CONSTRAINT f02_test_reject_workspace_sync_registration CHECK (false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	_, registrationErr := store.RegisterWorkspaceSyncDevice(ctx, principal, value.ID, challenge.ID, keyHash, requestHash, bindingID, "morde-12345678901234567890123456789012")
	if registrationErr == nil {
		t.Fatal("registration unexpectedly passed the temporary database constraint")
	}
	if _, err := store.pool.Exec(ctx, `ALTER TABLE workspace_sync_device_registration_requests
DROP CONSTRAINT f02_test_reject_workspace_sync_registration`); err != nil {
		t.Fatal(err)
	}
	var revisionRows, bindings int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_node_revisions WHERE owner_id=$1`, owner.ID).Scan(&revisionRows); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_device_bindings WHERE owner_id=$1`, owner.ID).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if revisionRows != 0 || bindings != 0 {
		t.Fatalf("failed registration left revision rows=%d bindings=%d", revisionRows, bindings)
	}

	registered, err := store.RegisterWorkspaceSyncDevice(ctx, principal, value.ID, challenge.ID, keyHash, requestHash, bindingID, "morde-12345678901234567890123456789012")
	if err != nil {
		t.Fatal(err)
	}
	var desiredRevision, appliedRevision int64
	if err := store.pool.QueryRow(ctx, `SELECT desired_revision,applied_revision FROM workspace_sync_node_revisions WHERE owner_id=$1`, owner.ID).Scan(&desiredRevision, &appliedRevision); err != nil {
		t.Fatal(err)
	}
	if desiredRevision != 1 || appliedRevision != 0 {
		t.Fatalf("committed registration revisions = desired:%d applied:%d, want 1 and 0", desiredRevision, appliedRevision)
	}
	replayedID, err := domain.NewWorkspaceSyncDeviceBindingID()
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.RegisterWorkspaceSyncDevice(ctx, principal, value.ID, challenge.ID, keyHash, requestHash, replayedID, "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil || replayed.ID != registered.ID {
		t.Fatalf("registration replay = %#v, %v; original=%#v", replayed, err, registered)
	}
	if err := store.pool.QueryRow(ctx, `SELECT desired_revision,applied_revision FROM workspace_sync_node_revisions WHERE owner_id=$1`, owner.ID).Scan(&desiredRevision, &appliedRevision); err != nil {
		t.Fatal(err)
	}
	if desiredRevision != 1 || appliedRevision != 0 {
		t.Fatalf("replayed registration changed revisions = desired:%d applied:%d", desiredRevision, appliedRevision)
	}
}

func TestWorkspaceSyncNodeReconcileFailureCanRetryAndAdvancesState(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-node-reconcile-retry")
	principal := syncDeviceTestPrincipal(t, store, owner.ID)
	binding := registerTestWorkspaceSyncDevice(t, store, principal, value.ID,
		"GGGGGGG-GGGGGGG-GGGGGGG-GGGGGGG-GGGGGGG-GGGGGGG-GGGGGGG-GGGGGGG", "sync-node-reconcile-retry-certificate")
	applyFailure := errors.New("node unavailable")
	var firstAttempt workspace.SyncNodeDesiredState
	err := store.ReconcileWorkspaceSyncNode(ctx, owner.ID, func(_ context.Context, state workspace.SyncNodeDesiredState) error {
		firstAttempt = state
		return applyFailure
	})
	if !errors.Is(err, applyFailure) {
		t.Fatalf("failed apply error = %v, want wrapped node error", err)
	}
	assertWorkspaceSyncDeviceRevocationPendingRegistration(t, store, binding.ID)
	owners, err := store.ListWorkspaceSyncNodeOwnersToReconcile(ctx)
	if err != nil || !reflect.DeepEqual(owners, []string{owner.ID}) {
		t.Fatalf("owners after failed apply = %v, %v", owners, err)
	}
	var retryAttempt workspace.SyncNodeDesiredState
	if err := store.ReconcileWorkspaceSyncNode(ctx, owner.ID, func(_ context.Context, state workspace.SyncNodeDesiredState) error {
		retryAttempt = state
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(firstAttempt, retryAttempt) {
		t.Fatalf("retry snapshot = %#v, initial snapshot = %#v", retryAttempt, firstAttempt)
	}
	var desiredRevision, appliedRevision int64
	if err := store.pool.QueryRow(ctx, `SELECT desired_revision,applied_revision FROM workspace_sync_node_revisions WHERE owner_id=$1`, owner.ID).Scan(&desiredRevision, &appliedRevision); err != nil {
		t.Fatal(err)
	}
	if desiredRevision != firstAttempt.Revision || appliedRevision != desiredRevision {
		t.Fatalf("reconciled revisions = desired:%d applied:%d, snapshot:%d", desiredRevision, appliedRevision, firstAttempt.Revision)
	}
	var bindingState, folderState string
	if err := store.pool.QueryRow(ctx, `SELECT b.state,f.state FROM workspace_sync_device_bindings b
JOIN workspace_sync_folders f ON f.workspace_id=b.workspace_id WHERE b.id=$1`, binding.ID).Scan(&bindingState, &folderState); err != nil ||
		bindingState != string(workspace.SyncDeviceBindingActive) || folderState != "active" {
		t.Fatalf("successful node reconcile states = binding:%q folder:%q err:%v", bindingState, folderState, err)
	}
	owners, err = store.ListWorkspaceSyncNodeOwnersToReconcile(ctx)
	if err != nil || len(owners) != 0 {
		t.Fatalf("owners after successful apply = %v, %v", owners, err)
	}
}

func TestWorkspaceSyncNodeReconcileFencesConcurrentRegistration(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	workspaceOne := createTestWorkspace(t, ctx, store, owner.ID, "sync-node-reconcile-fence-one")
	workspaceTwo := createTestWorkspace(t, ctx, store, owner.ID, "sync-node-reconcile-fence-two")
	principal := syncDeviceTestPrincipal(t, store, owner.ID)
	first := registerTestWorkspaceSyncDevice(t, store, principal, workspaceOne.ID,
		"HHHHHHH-HHHHHHH-HHHHHHH-HHHHHHH-HHHHHHH-HHHHHHH-HHHHHHH-HHHHHHH", "sync-node-reconcile-fence-first")
	secondDeviceID := "IIIIIII-IIIIIII-IIIIIII-IIIIIII-IIIIIII-IIIIIII-IIIIIII-IIIIIII"
	secondCertificate := []byte("sync-node-reconcile-fence-second")
	secondCertificateHash := sha256.Sum256(secondCertificate)
	secondChallenge := newSyncDeviceTestChallenge(t, owner.ID, workspaceTwo.ID, principal.ClientInstallationID, secondDeviceID, secondCertificate, secondCertificateHash[:])
	if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, principal, secondChallenge); err != nil {
		t.Fatal(err)
	}
	secondKeyHash, secondRequestHash := syncDeviceTestHashes("sync-node-reconcile-fence-second-registration", workspaceTwo.ID, principal.ClientInstallationID, secondDeviceID, secondCertificateHash[:])
	secondBindingID, err := domain.NewWorkspaceSyncDeviceBindingID()
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan workspace.SyncNodeDesiredState, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	letCallbackReturn := func() { releaseOnce.Do(func() { close(release) }) }
	defer letCallbackReturn()
	reconcileDone := make(chan error, 1)
	go func() {
		reconcileDone <- store.ReconcileWorkspaceSyncNode(ctx, owner.ID, func(_ context.Context, state workspace.SyncNodeDesiredState) error {
			entered <- state
			<-release
			return nil
		})
	}()
	var initialSnapshot workspace.SyncNodeDesiredState
	select {
	case initialSnapshot = <-entered:
	case err := <-reconcileDone:
		t.Fatalf("reconcile returned before callback: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("reconcile callback was not entered")
	}

	registrationStarted := make(chan struct{})
	registrationDone := make(chan error, 1)
	go func() {
		close(registrationStarted)
		_, err := store.RegisterWorkspaceSyncDevice(ctx, principal, workspaceTwo.ID, secondChallenge.ID, secondKeyHash,
			secondRequestHash, secondBindingID, "morde-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		registrationDone <- err
	}()
	<-registrationStarted
	select {
	case err := <-registrationDone:
		t.Fatalf("registration completed while reconcile callback held the owner lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	letCallbackReturn()
	if err := <-reconcileDone; err != nil {
		t.Fatalf("reconcile during concurrent registration: %v", err)
	}
	if err := <-registrationDone; err != nil {
		t.Fatalf("registration after reconcile: %v", err)
	}
	if len(initialSnapshot.Folders) != 1 || initialSnapshot.Folders[0].WorkspaceID != workspaceOne.ID ||
		len(initialSnapshot.Folders[0].DeviceIDs) != 1 || initialSnapshot.Folders[0].DeviceIDs[0] != first.DeviceID {
		t.Fatalf("reconcile snapshot included concurrent registration: %#v", initialSnapshot)
	}
	var desiredRevision, appliedRevision int64
	if err := store.pool.QueryRow(ctx, `SELECT desired_revision,applied_revision FROM workspace_sync_node_revisions WHERE owner_id=$1`, owner.ID).Scan(&desiredRevision, &appliedRevision); err != nil {
		t.Fatal(err)
	}
	if desiredRevision != initialSnapshot.Revision+1 || appliedRevision != initialSnapshot.Revision {
		t.Fatalf("revision fence after registration = desired:%d applied:%d snapshot:%d", desiredRevision, appliedRevision, initialSnapshot.Revision)
	}
}

func TestWorkspaceSyncDeviceRevocationClosesInstallationBindings(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-device-revoke")
	principal := syncDeviceTestPrincipal(t, store, owner.ID)
	certificateDER := []byte("public syncthing certificate")
	certificateHash := sha256.Sum256(certificateDER)
	deviceID := "BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB"
	challenge := newSyncDeviceTestChallenge(t, owner.ID, value.ID, principal.ClientInstallationID, deviceID, certificateDER, certificateHash[:])
	if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, principal, challenge); err != nil {
		t.Fatal(err)
	}
	keyHash, requestHash := syncDeviceTestHashes("revoke-registration-001", value.ID, principal.ClientInstallationID, deviceID, certificateHash[:])
	bindingID, err := domain.NewWorkspaceSyncDeviceBindingID()
	if err != nil {
		t.Fatal(err)
	}
	binding, err := store.RegisterWorkspaceSyncDevice(ctx, principal, value.ID, challenge.ID, keyHash, requestHash, bindingID, "morde-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	if err != nil {
		t.Fatal(err)
	}
	staleChallenge := newSyncDeviceTestChallenge(t, owner.ID, value.ID, principal.ClientInstallationID, deviceID, certificateDER, certificateHash[:])
	if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, principal, staleChallenge); err != nil {
		t.Fatal(err)
	}

	portal := clientTestToken(t, owner.ID, "", identity.KindSession, nil)
	if _, err := store.CreateToken(ctx, portal); err != nil {
		t.Fatal(err)
	}
	portalPrincipal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: portal.ID}
	if err := store.RevokeWorkspaceSyncDevice(ctx, portalPrincipal, value.ID, binding.ID); err != nil {
		t.Fatal(err)
	}
	staleKeyHash, staleRequestHash := syncDeviceTestHashes("revoked-stale-challenge", value.ID, principal.ClientInstallationID, deviceID, certificateHash[:])
	staleBindingID, err := domain.NewWorkspaceSyncDeviceBindingID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterWorkspaceSyncDevice(ctx, principal, value.ID, staleChallenge.ID, staleKeyHash, staleRequestHash, staleBindingID, "morde-gggggggggggggggggggggggggggggggg"); err == nil {
		t.Fatal("pre-revocation challenge recreated a revoked Workspace binding")
	}
	var state string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM workspace_sync_device_bindings WHERE id=$1`, binding.ID).Scan(&state); err != nil || state != string(workspace.SyncDeviceBindingRevoking) {
		t.Fatalf("binding state after explicit revoke = %q, want revoking: %v", state, err)
	}
	secondDeviceID := "CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC"
	secondCertificateDER := []byte("second public syncthing certificate")
	secondCertificateHash := sha256.Sum256(secondCertificateDER)
	secondChallenge := newSyncDeviceTestChallenge(t, owner.ID, value.ID, principal.ClientInstallationID, secondDeviceID, secondCertificateDER, secondCertificateHash[:])
	if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, principal, secondChallenge); err != nil {
		t.Fatal(err)
	}
	secondKeyHash, secondRequestHash := syncDeviceTestHashes("revoke-registration-002", value.ID, principal.ClientInstallationID, secondDeviceID, secondCertificateHash[:])
	secondBindingID, err := domain.NewWorkspaceSyncDeviceBindingID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.RegisterWorkspaceSyncDevice(ctx, principal, value.ID, secondChallenge.ID, secondKeyHash, secondRequestHash, secondBindingID, "morde-ffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatal(err)
	}
	var reopenedFolderState string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM workspace_sync_folders WHERE workspace_id=$1`, value.ID).Scan(&reopenedFolderState); err != nil || reopenedFolderState != "pending_node" {
		t.Fatalf("folder state after pairing a replacement device = %q, want pending_node: %v", reopenedFolderState, err)
	}
	if err := store.RevokeClientInstallation(ctx, principal.ClientInstallationID, owner.ID, "", audit.ActorSession); err != nil {
		t.Fatal(err)
	}
	var identityRevoked bool
	var folderState string
	if err := store.pool.QueryRow(ctx, `SELECT i.revoked_at IS NOT NULL,f.state,b.state
FROM workspace_sync_device_identities i JOIN workspace_sync_folders f ON f.workspace_id=$2
JOIN workspace_sync_device_bindings b ON b.workspace_id=f.workspace_id AND b.device_identity_id=i.id
WHERE i.device_id=$1`, secondDeviceID, value.ID).Scan(&identityRevoked, &folderState, &state); err != nil || !identityRevoked || folderState != "revoking" || state != string(workspace.SyncDeviceBindingRevoking) {
		t.Fatalf("device identity/binding/folder after installation revoke = revoked:%t binding:%q folder:%q err:%v", identityRevoked, state, folderState, err)
	}
	desired := applyWorkspaceSyncNodeState(t, store, owner.ID)
	if len(desired.Folders) != 0 {
		t.Fatalf("installation revoke left desired folders: %#v", desired.Folders)
	}
	assertWorkspaceSyncDeviceRevoked(t, store, binding.ID)
}

func TestWorkspaceSyncDeviceListIsScopedToPortalOwnerAndWorkspace(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	otherOwner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-device-list")
	otherWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "sync-device-list-other")
	client := syncDeviceTestPrincipal(t, store, owner.ID)
	first := registerTestWorkspaceSyncDevice(t, store, client, value.ID,
		"AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA", "list-first-certificate")
	second := registerTestWorkspaceSyncDevice(t, store, client, value.ID,
		"BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB", "list-second-certificate")
	registerTestWorkspaceSyncDevice(t, store, client, otherWorkspace.ID,
		"CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC", "list-other-workspace-certificate")

	portal := clientTestToken(t, owner.ID, "", identity.KindSession, nil)
	if _, err := store.CreateToken(ctx, portal); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: portal.ID}
	if err := store.RevokeWorkspaceSyncDevice(ctx, principal, value.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListWorkspaceSyncDeviceBindings(ctx, principal, value.ID)
	if err != nil || len(items) != 2 {
		t.Fatalf("ListWorkspaceSyncDeviceBindings() = %d items, %v; want two Workspace-local bindings", len(items), err)
	}
	states := map[string]workspace.SyncDeviceBindingState{}
	for _, item := range items {
		if item.WorkspaceID != value.ID || item.OwnerID != owner.ID {
			t.Fatalf("device list leaked a different scope: %+v", item)
		}
		states[item.ID] = item.State
	}
	if states[first.ID] != workspace.SyncDeviceBindingRevoking || states[second.ID] != workspace.SyncDeviceBindingPendingNode {
		t.Fatalf("device states = %v", states)
	}
	if _, err := store.ListWorkspaceSyncDeviceBindings(ctx, client, value.ID); err == nil {
		t.Fatal("client credential listed Workspace sync devices")
	}
	otherPortal := clientTestToken(t, otherOwner.ID, "", identity.KindSession, nil)
	if _, err := store.CreateToken(ctx, otherPortal); err != nil {
		t.Fatal(err)
	}
	otherPrincipal := identity.Principal{UserID: otherOwner.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: otherPortal.ID}
	if _, err := store.ListWorkspaceSyncDeviceBindings(ctx, otherPrincipal, value.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner device list error = %v, want not found", err)
	}
}

func TestWorkspaceSyncDevicePendingChallengesAreBoundedAndExpiredRowsAreCollected(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-device-challenge-limit")
	principal := syncDeviceTestPrincipal(t, store, owner.ID)
	certificateDER := []byte("bounded challenge certificate")
	certificateHash := sha256.Sum256(certificateDER)
	deviceID := "EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE"
	var firstChallenge workspace.SyncDeviceChallenge
	for index := 0; index < app.MaxPendingWorkspaceSyncDeviceChallengesPerInstallation; index++ {
		challenge := newSyncDeviceTestChallenge(t, owner.ID, value.ID, principal.ClientInstallationID, deviceID, certificateDER, certificateHash[:])
		if index == 0 {
			firstChallenge = challenge
		}
		if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, principal, challenge); err != nil {
			t.Fatalf("create pending challenge %d: %v", index, err)
		}
	}
	overflow := newSyncDeviceTestChallenge(t, owner.ID, value.ID, principal.ClientInstallationID, deviceID, certificateDER, certificateHash[:])
	if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, principal, overflow); domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("challenge beyond per-installation limit = %v, want rate limited", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_sync_device_challenges SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, firstChallenge.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, principal, overflow); err != nil {
		t.Fatalf("create challenge after expired row cleanup: %v", err)
	}
	var expiredRows, pendingRows int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE id=$1),count(*) FROM workspace_sync_device_challenges WHERE client_installation_id=$2`,
		firstChallenge.ID, principal.ClientInstallationID).Scan(&expiredRows, &pendingRows); err != nil || expiredRows != 0 || pendingRows != app.MaxPendingWorkspaceSyncDeviceChallengesPerInstallation {
		t.Fatalf("challenge rows after cleanup = expired:%d pending:%d, err=%v", expiredRows, pendingRows, err)
	}
}

func TestWorkspaceSyncDeviceConcurrentChallengesRespectInstallationLimit(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-device-challenge-race")
	principal := syncDeviceTestPrincipal(t, store, owner.ID)
	certificateDER := []byte("challenge race certificate")
	certificateHash := sha256.Sum256(certificateDER)
	const requests = app.MaxPendingWorkspaceSyncDeviceChallengesPerInstallation * 2
	start := make(chan struct{})
	results := make(chan error, requests)
	var wait sync.WaitGroup
	for index := 0; index < requests; index++ {
		challenge := newSyncDeviceTestChallenge(t, owner.ID, value.ID, principal.ClientInstallationID,
			"EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE", certificateDER, certificateHash[:])
		wait.Add(1)
		go func(challenge workspace.SyncDeviceChallenge) {
			defer wait.Done()
			<-start
			results <- store.CreateWorkspaceSyncDeviceChallenge(ctx, principal, challenge)
		}(challenge)
	}
	close(start)
	wait.Wait()
	close(results)

	var created, limited int
	for err := range results {
		switch {
		case err == nil:
			created++
		case domain.CodeOf(err) == domain.CodeRateLimited:
			limited++
		default:
			t.Fatalf("concurrent challenge creation failed unexpectedly: %v", err)
		}
	}
	if created != app.MaxPendingWorkspaceSyncDeviceChallengesPerInstallation ||
		limited != requests-app.MaxPendingWorkspaceSyncDeviceChallengesPerInstallation {
		t.Fatalf("concurrent challenge results = created:%d rate_limited:%d; want created:%d rate_limited:%d",
			created, limited, app.MaxPendingWorkspaceSyncDeviceChallengesPerInstallation,
			requests-app.MaxPendingWorkspaceSyncDeviceChallengesPerInstallation)
	}
	var pending int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_device_challenges
WHERE owner_id=$1 AND client_installation_id=$2 AND expires_at>clock_timestamp()`, owner.ID, principal.ClientInstallationID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != app.MaxPendingWorkspaceSyncDeviceChallengesPerInstallation {
		t.Fatalf("pending challenges after concurrent requests = %d; want %d", pending, app.MaxPendingWorkspaceSyncDeviceChallengesPerInstallation)
	}
}

func TestWorkspaceSyncDeviceConcurrentWorkspaceRegistrationReusesIdentity(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	workspaceOne := createTestWorkspace(t, ctx, store, owner.ID, "sync-device-race-one")
	workspaceTwo := createTestWorkspace(t, ctx, store, owner.ID, "sync-device-race-two")
	principal := syncDeviceTestPrincipal(t, store, owner.ID)
	certificateDER := []byte("concurrent public syncthing certificate")
	certificateHash := sha256.Sum256(certificateDER)
	deviceID := "DDDDDDD-DDDDDDD-DDDDDDD-DDDDDDD-DDDDDDD-DDDDDDD-DDDDDDD-DDDDDDD"
	type registration struct {
		challenge workspace.SyncDeviceChallenge
		key       string
		folderID  string
	}
	registrations := make([]registration, 0, 2)
	for index, workspaceValue := range []workspace.Workspace{workspaceOne, workspaceTwo} {
		challenge := newSyncDeviceTestChallenge(t, owner.ID, workspaceValue.ID, principal.ClientInstallationID, deviceID, certificateDER, certificateHash[:])
		if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, principal, challenge); err != nil {
			t.Fatal(err)
		}
		registrations = append(registrations, registration{challenge: challenge, key: "race-registration-00" + string(rune('1'+index)), folderID: "morde-" + strings.Repeat(string(rune('a'+index)), 32)})
	}
	var wait sync.WaitGroup
	errors := make(chan error, len(registrations))
	for _, value := range registrations {
		wait.Add(1)
		go func(value registration) {
			defer wait.Done()
			keyHash, requestHash := syncDeviceTestHashes(value.key, value.challenge.WorkspaceID, principal.ClientInstallationID, deviceID, certificateHash[:])
			bindingID, err := domain.NewWorkspaceSyncDeviceBindingID()
			if err != nil {
				errors <- err
				return
			}
			_, err = store.RegisterWorkspaceSyncDevice(ctx, principal, value.challenge.WorkspaceID, value.challenge.ID, keyHash, requestHash, bindingID, value.folderID)
			errors <- err
		}(value)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent device registration: %v", err)
		}
	}
	var identities, bindings int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_device_identities WHERE device_id=$1 AND revoked_at IS NULL`, deviceID).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_device_bindings WHERE client_installation_id=$1 AND state<>'revoked'`, principal.ClientInstallationID).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if identities != 1 || bindings != 2 {
		t.Fatalf("concurrent registrations created %d identities and %d bindings, want 1 and 2", identities, bindings)
	}
}

func TestWorkspaceSyncDeviceClientTokenRevocationClosesBinding(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-device-token-revoke")
	principal := syncDeviceTestPrincipal(t, store, owner.ID)
	binding := registerTestWorkspaceSyncDevice(t, store, principal, value.ID,
		"AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA", "token-revoke-certificate")
	applyWorkspaceSyncNodeState(t, store, owner.ID)
	deviceToken := clientTestToken(t, owner.ID, principal.ClientInstallationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, deviceToken); err != nil {
		t.Fatal(err)
	}
	session := newTestSyncSession(t, value, owner.ID, []workspace.File{}, []workspace.File{}, nil)
	session.ReplicaID = "device:" + deviceToken.ID
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, nil,
		syncTestIdempotency(owner.ID, "workspace.sync.create", "token-revoke-session", session.ID)); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeToken(ctx, deviceToken.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceSyncDeviceRevocationQueued(t, store, binding.ID)
	if err := store.CheckWorkspaceSyncDeviceAccess(ctx, identity.Principal{
		UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindDevice,
		TokenID: deviceToken.ID, ClientInstallationID: principal.ClientInstallationID,
	}, value.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked device token access = %v; want not found", err)
	}
	cancelled, err := store.GetWorkspaceSyncSession(ctx, value.ID, session.ID)
	if err != nil || cancelled.State != workspace.SyncCancelled {
		t.Fatalf("session after device token revoke = %q, %v; want cancelled", cancelled.State, err)
	}
	if got := applyWorkspaceSyncNodeState(t, store, owner.ID); len(got.Folders) != 0 {
		t.Fatalf("token revoke left desired folders: %#v", got.Folders)
	}
	assertWorkspaceSyncDeviceRevoked(t, store, binding.ID)
}

func TestWorkspaceSyncDeviceAccountDisableClosesBinding(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	admin := createTestSuperAdmin(t, store)
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-device-account-revoke")
	principal := syncDeviceTestPrincipal(t, store, owner.ID)
	binding := registerTestWorkspaceSyncDevice(t, store, principal, value.ID,
		"BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB", "account-revoke-certificate")
	owner.Status = identity.StatusDisabled
	if _, err := store.UpdateUser(ctx, admin.ID, identity.StatusApproved, owner); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceSyncDeviceRevocationQueued(t, store, binding.ID)
	if got := applyWorkspaceSyncNodeState(t, store, owner.ID); len(got.Folders) != 0 {
		t.Fatalf("account revoke left desired folders: %#v", got.Folders)
	}
	assertWorkspaceSyncDeviceRevoked(t, store, binding.ID)
}

func registerTestWorkspaceSyncDevice(t *testing.T, store *Store, principal identity.Principal, workspaceID, deviceID, certificate string) workspace.SyncDeviceBinding {
	t.Helper()
	ctx := context.Background()
	certificateDER := []byte(certificate)
	certificateHash := sha256.Sum256(certificateDER)
	challenge := newSyncDeviceTestChallenge(t, principal.UserID, workspaceID, principal.ClientInstallationID, deviceID, certificateDER, certificateHash[:])
	if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, principal, challenge); err != nil {
		t.Fatal(err)
	}
	keyHash, requestHash := syncDeviceTestHashes("test-registration-key:"+certificate, workspaceID, principal.ClientInstallationID, deviceID, certificateHash[:])
	bindingID, err := domain.NewWorkspaceSyncDeviceBindingID()
	if err != nil {
		t.Fatal(err)
	}
	folderHash := sha256.Sum256([]byte(workspaceID + "\x00" + string(certificateDER)))
	folderID := "morde-" + hex.EncodeToString(folderHash[:16])
	binding, err := store.RegisterWorkspaceSyncDevice(ctx, principal, workspaceID, challenge.ID, keyHash, requestHash, bindingID, folderID)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func assertWorkspaceSyncDeviceRevoked(t *testing.T, store *Store, bindingID string) {
	t.Helper()
	var state, folderState string
	var identityRevoked bool
	if err := store.pool.QueryRow(context.Background(), `SELECT b.state,i.revoked_at IS NOT NULL,f.state
FROM workspace_sync_device_bindings b
JOIN workspace_sync_device_identities i ON i.id=b.device_identity_id
JOIN workspace_sync_folders f ON f.workspace_id=b.workspace_id WHERE b.id=$1`, bindingID).
		Scan(&state, &identityRevoked, &folderState); err != nil || state != string(workspace.SyncDeviceBindingRevoked) || !identityRevoked || folderState != "revoked" {
		t.Fatalf("binding revocation state = binding:%q identity_revoked:%t folder:%q err:%v", state, identityRevoked, folderState, err)
	}
}

func assertWorkspaceSyncDeviceRevocationQueued(t *testing.T, store *Store, bindingID string) {
	t.Helper()
	var state string
	var desiredRevision, appliedRevision int64
	if err := store.pool.QueryRow(context.Background(), `SELECT b.state,r.desired_revision,r.applied_revision
FROM workspace_sync_device_bindings b JOIN workspace_sync_node_revisions r ON r.owner_id=b.owner_id
WHERE b.id=$1`, bindingID).Scan(&state, &desiredRevision, &appliedRevision); err != nil ||
		state != string(workspace.SyncDeviceBindingRevoking) || desiredRevision <= appliedRevision {
		t.Fatalf("revocation queue state = binding:%q desired:%d applied:%d err:%v", state, desiredRevision, appliedRevision, err)
	}
}

func assertWorkspaceSyncDeviceRevocationPendingRegistration(t *testing.T, store *Store, bindingID string) {
	t.Helper()
	var state string
	var desiredRevision, appliedRevision int64
	if err := store.pool.QueryRow(context.Background(), `SELECT b.state,r.desired_revision,r.applied_revision
FROM workspace_sync_device_bindings b JOIN workspace_sync_node_revisions r ON r.owner_id=b.owner_id
WHERE b.id=$1`, bindingID).Scan(&state, &desiredRevision, &appliedRevision); err != nil ||
		state != string(workspace.SyncDeviceBindingPendingNode) || desiredRevision <= appliedRevision {
		t.Fatalf("pending registration state = binding:%q desired:%d applied:%d err:%v", state, desiredRevision, appliedRevision, err)
	}
}

func applyWorkspaceSyncNodeState(t *testing.T, store *Store, ownerID string) workspace.SyncNodeDesiredState {
	t.Helper()
	var applied workspace.SyncNodeDesiredState
	called := false
	if err := store.ReconcileWorkspaceSyncNode(context.Background(), ownerID, func(_ context.Context, state workspace.SyncNodeDesiredState) error {
		applied = state
		called = true
		return nil
	}); err != nil {
		t.Fatalf("reconcile Workspace sync node: %v", err)
	}
	if !called {
		t.Fatalf("Workspace sync node reconcile for %s did not call apply", ownerID)
	}
	return applied
}

func syncDeviceTestPrincipal(t *testing.T, store *Store, ownerID string) identity.Principal {
	t.Helper()
	installationID := createTestClientInstallation(t, store.pool, ownerID, nil)
	token := clientTestToken(t, ownerID, installationID, identity.KindClient, nil)
	if _, err := store.CreateToken(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	return identity.Principal{UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindClient, TokenID: token.ID, ClientInstallationID: installationID}
}

func newSyncDeviceTestChallenge(t *testing.T, ownerID, workspaceID, installationID, deviceID string, certificateDER, certificateHash []byte) workspace.SyncDeviceChallenge {
	t.Helper()
	id, err := domain.NewWorkspaceSyncDeviceChallengeID()
	if err != nil {
		t.Fatal(err)
	}
	return workspace.SyncDeviceChallenge{ID: id, OwnerID: ownerID, WorkspaceID: workspaceID,
		ClientInstallationID: installationID, DeviceID: deviceID, CertificateDER: certificateDER,
		CertificateSHA256: certificateHash, Nonce: make([]byte, 32), ExpiresAt: time.Now().Add(5 * time.Minute)}
}

func syncDeviceTestHashes(key, workspaceID, installationID, deviceID string, certificateHash []byte) ([]byte, []byte) {
	keyHash := sha256.Sum256([]byte(key))
	requestHash := sha256.Sum256([]byte(workspaceID + "\x00" + installationID + "\x00" + deviceID + "\x00" + string(certificateHash)))
	return keyHash[:], requestHash[:]
}
