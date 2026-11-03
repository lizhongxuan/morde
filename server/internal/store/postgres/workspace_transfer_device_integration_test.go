package postgres

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

func TestPostgresWorkspaceOwnershipTransferClearsDeviceAuthorizationAtomically(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	oldOwner := createTestUser(t, store, 8, 20_000, 40_000)
	newOwner := createTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTransferReadyWorkspace(t, ctx, store, oldOwner.ID, "transfer-device-cleanup")
	bindingID, identityID, pendingChallengeID, installationID := registerTransferTestDevice(t, ctx, store, oldOwner.ID, workspaceValue.ID)
	deviceToken := clientTestToken(t, oldOwner.ID, installationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, deviceToken); err != nil {
		t.Fatalf("create transfer test device token: %v", err)
	}
	devicePrincipal := identity.Principal{UserID: oldOwner.ID, Role: identity.RoleUser, Kind: identity.KindDevice,
		TokenID: deviceToken.ID, ClientInstallationID: installationID}
	if err := store.CheckWorkspaceSyncDeviceAccess(ctx, devicePrincipal, workspaceValue.ID); err != nil {
		t.Fatalf("device could not access Workspace before ownership transfer: %v", err)
	}

	beforeDesired, beforeApplied := workspaceSyncNodeRevision(t, ctx, store, oldOwner.ID)
	if beforeDesired != beforeApplied {
		t.Fatalf("test fixture node revision = %d/%d before transfer, want reconciled", beforeDesired, beforeApplied)
	}

	service := newTestWorkspaceOwnershipTransferService(t, store, 100)
	created, replayed, err := service.RequestWorkspaceTransfer(ctx, workspaceTransferPrincipal(oldOwner.ID), workspaceValue.ID,
		newOwner.ID, workspaceValue.Revision, "transfer-device-cleanup-create")
	if err != nil || replayed {
		t.Fatalf("request device-bound Workspace transfer = %#v replayed=%t err=%v", created, replayed, err)
	}
	accepted, err := service.AcceptWorkspaceTransfer(ctx, workspaceTransferPrincipal(newOwner.ID), workspaceValue.ID,
		created.ID, "transfer-device-cleanup-accept")
	if err != nil || accepted.State != workspace.OwnershipTransferAccepted {
		t.Fatalf("accept device-bound Workspace transfer = %#v err=%v", accepted, err)
	}

	assertTransferDeviceRows(t, ctx, store, workspaceValue.ID, 0, 0, 0, 0)
	if err := store.CheckWorkspaceSyncDeviceAccess(ctx, devicePrincipal, workspaceValue.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("former owner's device retained access after transfer: %v", err)
	}
	var retainedIdentityCount, activeIdentityCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE revoked_at IS NULL)
FROM workspace_sync_device_identities WHERE id=$1 AND owner_id=$2`, identityID, oldOwner.ID).
		Scan(&retainedIdentityCount, &activeIdentityCount); err != nil {
		t.Fatal(err)
	}
	if retainedIdentityCount != 1 || activeIdentityCount != 1 {
		t.Fatalf("installation-scoped device identity was not preserved for other Workspaces: count=%d active=%d", retainedIdentityCount, activeIdentityCount)
	}
	if pendingChallengeID == "" || bindingID == "" {
		t.Fatal("test fixture did not create both device binding and pending challenge")
	}
	desired, applied := workspaceSyncNodeRevision(t, ctx, store, oldOwner.ID)
	if desired != beforeDesired+1 || applied != beforeApplied {
		t.Fatalf("former-owner node revision after transfer = %d/%d, want %d/%d", desired, applied, beforeDesired+1, beforeApplied)
	}

	var observed workspace.SyncNodeDesiredState
	if err := store.ReconcileWorkspaceSyncNode(ctx, oldOwner.ID, func(_ context.Context, value workspace.SyncNodeDesiredState) error {
		observed = value
		return nil
	}); err != nil {
		t.Fatalf("reconcile former-owner node after transfer: %v", err)
	}
	if observed.Revision != desired || len(observed.Folders) != 0 {
		t.Fatalf("former-owner desired device state after transfer = %#v, want no transferred Workspace folders", observed)
	}
	finalDesired, finalApplied := workspaceSyncNodeRevision(t, ctx, store, oldOwner.ID)
	if finalDesired != desired || finalApplied != desired {
		t.Fatalf("former-owner node revision after cleanup apply = %d/%d, want %d/%d", finalDesired, finalApplied, desired, desired)
	}
}

func TestPostgresWorkspaceOwnershipTransferClearsOrphanDeviceFolder(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	oldOwner := createTestUser(t, store, 8, 20_000, 40_000)
	newOwner := createTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTransferReadyWorkspace(t, ctx, store, oldOwner.ID, "transfer-orphan-device-folder")
	folderID := "morde-" + strings.Repeat("a", 32)
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_sync_folders(workspace_id,owner_id,folder_id,state)
VALUES ($1,$2,$3,'revoked')`, workspaceValue.ID, oldOwner.ID, folderID); err != nil {
		t.Fatalf("seed orphan Workspace sync folder: %v", err)
	}
	assertTransferDeviceRows(t, ctx, store, workspaceValue.ID, 1, 0, 0, 0)

	service := newTestWorkspaceOwnershipTransferService(t, store, 100)
	created, replayed, err := service.RequestWorkspaceTransfer(ctx, workspaceTransferPrincipal(oldOwner.ID), workspaceValue.ID,
		newOwner.ID, workspaceValue.Revision, "orphan-device-folder-transfer-create")
	if err != nil || replayed {
		t.Fatalf("request transfer with orphan device folder = %#v replayed=%t err=%v", created, replayed, err)
	}
	accepted, err := service.AcceptWorkspaceTransfer(ctx, workspaceTransferPrincipal(newOwner.ID), workspaceValue.ID,
		created.ID, "orphan-device-folder-transfer-accept")
	if err != nil || accepted.State != workspace.OwnershipTransferAccepted {
		t.Fatalf("accept transfer with orphan device folder = %#v err=%v", accepted, err)
	}
	assertTransferDeviceRows(t, ctx, store, workspaceValue.ID, 0, 0, 0, 0)
	current, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || current.OwnerID != newOwner.ID {
		t.Fatalf("Workspace owner after orphan-folder transfer = %q, %v", current.OwnerID, err)
	}

	desiredRevision, appliedRevision := workspaceSyncNodeRevision(t, ctx, store, oldOwner.ID)
	if desiredRevision != 1 || appliedRevision != 0 {
		t.Fatalf("former-owner node revision = %d/%d, want cleanup pending at 1/0", desiredRevision, appliedRevision)
	}
	var observed workspace.SyncNodeDesiredState
	if err := store.ReconcileWorkspaceSyncNode(ctx, oldOwner.ID, func(_ context.Context, value workspace.SyncNodeDesiredState) error {
		observed = value
		return nil
	}); err != nil {
		t.Fatalf("reconcile former-owner node after orphan-folder transfer: %v", err)
	}
	if observed.Revision != desiredRevision || len(observed.Folders) != 0 {
		t.Fatalf("former-owner desired sync state = %#v, want no Workspace folders", observed)
	}
}

func TestPostgresWorkspaceOwnershipTransferRollbackRestoresDeviceState(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	oldOwner := createTestUser(t, store, 8, 20_000, 40_000)
	newOwner := createTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTransferReadyWorkspace(t, ctx, store, oldOwner.ID, "transfer-device-rollback")
	bindingID, _, _, _ := registerTransferTestDevice(t, ctx, store, oldOwner.ID, workspaceValue.ID)
	desiredBefore, appliedBefore := workspaceSyncNodeRevision(t, ctx, store, oldOwner.ID)

	transferID := mustID(t, workspace.NewOwnershipTransferID)
	created, _, err := store.CreateWorkspaceOwnershipTransfer(ctx, oldOwner.ID, workspaceValue.ID, newOwner.ID,
		workspaceValue.Revision, transferID, transferRecord(oldOwner.ID, "workspace.transfer.create", "transfer-device-rollback-create", transferID))
	if err != nil || created.State != workspace.OwnershipTransferPending {
		t.Fatalf("create transfer for rollback test = %#v, %v", created, err)
	}
	operationID := mustID(t, domain.NewOperationID)
	_, _, err = store.StartWorkspaceOwnershipTransferAcceptance(ctx, newOwner.ID, workspaceValue.ID, transferID, operationID,
		transferRecord(newOwner.ID, "workspace.transfer.accept", "transfer-device-rollback-accept", transferID))
	if err != nil {
		t.Fatalf("start transfer acceptance for rollback test: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `CREATE FUNCTION fail_workspace_owner_transfer_for_test() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.owner_id IS DISTINCT FROM OLD.owner_id THEN RAISE EXCEPTION 'injected owner update failure'; END IF;
  RETURN NEW;
END $$`); err != nil {
		t.Fatalf("create rollback test trigger function: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `CREATE TRIGGER fail_workspace_owner_transfer_for_test
BEFORE UPDATE OF owner_id ON workspaces FOR EACH ROW
EXECUTE FUNCTION fail_workspace_owner_transfer_for_test()`); err != nil {
		t.Fatalf("create rollback test trigger: %v", err)
	}
	if _, err := store.FinishWorkspaceOwnershipTransferAcceptance(ctx, newOwner.ID, workspaceValue.ID, transferID, operationID, 100); err == nil {
		t.Fatal("injected failure after device cleanup unexpectedly accepted the transfer")
	}
	assertTransferDeviceRows(t, ctx, store, workspaceValue.ID, 1, 1, 1, 1)
	desiredAfterFailure, appliedAfterFailure := workspaceSyncNodeRevision(t, ctx, store, oldOwner.ID)
	if desiredAfterFailure != desiredBefore || appliedAfterFailure != appliedBefore {
		t.Fatalf("node revision changed despite transfer rollback: %d/%d, want %d/%d", desiredAfterFailure, appliedAfterFailure, desiredBefore, appliedBefore)
	}
	var ownerID string
	if err := store.pool.QueryRow(ctx, `SELECT owner_id FROM workspaces WHERE id=$1`, workspaceValue.ID).Scan(&ownerID); err != nil || ownerID != oldOwner.ID {
		t.Fatalf("Workspace owner changed despite failed transaction: %q, %v", ownerID, err)
	}
	var storedBindingID string
	if err := store.pool.QueryRow(ctx, `SELECT id FROM workspace_sync_device_bindings WHERE workspace_id=$1`, workspaceValue.ID).Scan(&storedBindingID); err != nil || storedBindingID != bindingID {
		t.Fatalf("device binding history was not restored after failed transaction: %q, %v", storedBindingID, err)
	}

	if _, err := store.pool.Exec(ctx, `DROP TRIGGER fail_workspace_owner_transfer_for_test ON workspaces`); err != nil {
		t.Fatalf("drop rollback test trigger: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `DROP FUNCTION fail_workspace_owner_transfer_for_test()`); err != nil {
		t.Fatalf("drop rollback test trigger function: %v", err)
	}
	accepted, err := store.FinishWorkspaceOwnershipTransferAcceptance(ctx, newOwner.ID, workspaceValue.ID, transferID, operationID, 100)
	if err != nil || accepted.State != workspace.OwnershipTransferAccepted {
		t.Fatalf("retry transfer after rollback = %#v, %v", accepted, err)
	}
	assertTransferDeviceRows(t, ctx, store, workspaceValue.ID, 0, 0, 0, 0)
}

func TestPostgresWorkspaceOwnershipTransferCancelPreservesDeviceState(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	oldOwner := createTestUser(t, store, 8, 20_000, 40_000)
	newOwner := createTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTransferReadyWorkspace(t, ctx, store, oldOwner.ID, "transfer-device-cancel")
	registerTransferTestDevice(t, ctx, store, oldOwner.ID, workspaceValue.ID)

	service := newTestWorkspaceOwnershipTransferService(t, store, 100)
	created, _, err := service.RequestWorkspaceTransfer(ctx, workspaceTransferPrincipal(oldOwner.ID), workspaceValue.ID,
		newOwner.ID, workspaceValue.Revision, "transfer-device-cancel-create")
	if err != nil {
		t.Fatalf("request transfer before cancellation: %v", err)
	}
	cancelled, err := service.CancelWorkspaceTransfer(ctx, workspaceTransferPrincipal(oldOwner.ID), workspaceValue.ID,
		created.ID, "transfer-device-cancel-cancel")
	if err != nil || cancelled.State != workspace.OwnershipTransferCancelled {
		t.Fatalf("cancel device-bound transfer = %#v, %v", cancelled, err)
	}
	assertTransferDeviceRows(t, ctx, store, workspaceValue.ID, 1, 1, 1, 1)
	if _, err := store.GetWorkspace(ctx, workspaceValue.ID); err != nil {
		t.Fatalf("Workspace is unavailable after cancelling transfer: %v", err)
	}
}

func registerTransferTestDevice(t *testing.T, ctx context.Context, store *Store, ownerID, workspaceID string) (string, string, string, string) {
	t.Helper()
	principal := syncDeviceTestPrincipal(t, store, ownerID)
	certificateDER := []byte("workspace transfer test device certificate")
	certificateHash := sha256.Sum256(certificateDER)
	deviceID := "AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG-HHHHHHH"
	challenge := newSyncDeviceTestChallenge(t, ownerID, workspaceID, principal.ClientInstallationID, deviceID, certificateDER, certificateHash[:])
	if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, principal, challenge); err != nil {
		t.Fatalf("create transfer test device challenge: %v", err)
	}
	keyHash, requestHash := syncDeviceTestHashes("transfer-device-registration-key", workspaceID, principal.ClientInstallationID, deviceID, certificateHash[:])
	bindingID, err := domain.NewWorkspaceSyncDeviceBindingID()
	if err != nil {
		t.Fatal(err)
	}
	binding, err := store.RegisterWorkspaceSyncDevice(ctx, principal, workspaceID, challenge.ID, keyHash, requestHash,
		bindingID, "morde-0123456789abcdef0123456789abcdef")
	if err != nil || binding.ID != bindingID {
		t.Fatalf("register transfer test device: %#v, %v", binding, err)
	}
	secondChallenge := newSyncDeviceTestChallenge(t, ownerID, workspaceID, principal.ClientInstallationID,
		"IIIIIII-JJJJJJJ-KKKKKKK-LLLLLLL-MMMMMMM-NNNNNNN-OOOOOOO-PPPPPPP", certificateDER, certificateHash[:])
	if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, principal, secondChallenge); err != nil {
		t.Fatalf("create second transfer test device challenge: %v", err)
	}
	if err := store.ReconcileWorkspaceSyncNode(ctx, ownerID, func(context.Context, workspace.SyncNodeDesiredState) error { return nil }); err != nil {
		t.Fatalf("activate transfer test device on owner node: %v", err)
	}
	var identityID string
	if err := store.pool.QueryRow(ctx, `SELECT device_identity_id FROM workspace_sync_device_bindings WHERE id=$1`, bindingID).Scan(&identityID); err != nil {
		t.Fatalf("read transfer test device identity: %v", err)
	}
	return bindingID, identityID, secondChallenge.ID, principal.ClientInstallationID
}

func assertTransferDeviceRows(t *testing.T, ctx context.Context, store *Store, workspaceID string,
	wantFolders, wantBindings, wantChallenges, wantRegistrations int) {
	t.Helper()
	var folders, bindings, challenges, registrations int
	if err := store.pool.QueryRow(ctx, `SELECT
  (SELECT count(*) FROM workspace_sync_folders WHERE workspace_id=$1),
  (SELECT count(*) FROM workspace_sync_device_bindings WHERE workspace_id=$1),
  (SELECT count(*) FROM workspace_sync_device_challenges WHERE workspace_id=$1),
  (SELECT count(*) FROM workspace_sync_device_registration_requests WHERE workspace_id=$1)`, workspaceID).
		Scan(&folders, &bindings, &challenges, &registrations); err != nil {
		t.Fatal(err)
	}
	if folders != wantFolders || bindings != wantBindings || challenges != wantChallenges || registrations != wantRegistrations {
		t.Fatalf("Workspace device rows folder/binding/challenge/registration = %d/%d/%d/%d, want %d/%d/%d/%d",
			folders, bindings, challenges, registrations, wantFolders, wantBindings, wantChallenges, wantRegistrations)
	}
}

func workspaceSyncNodeRevision(t *testing.T, ctx context.Context, store *Store, ownerID string) (int64, int64) {
	t.Helper()
	var desired, applied int64
	if err := store.pool.QueryRow(ctx, `SELECT desired_revision,applied_revision FROM workspace_sync_node_revisions WHERE owner_id=$1`, ownerID).
		Scan(&desired, &applied); err != nil {
		t.Fatalf("read Workspace sync node revision: %v", err)
	}
	return desired, applied
}
