package postgres

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

func TestWorkspaceSyncSupportsMultipleMetadataFoldersWithScopedRelations(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	projectWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "multi-folder-projects")
	rootWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "multi-folder-root")
	portal := workspaceSyncPortalPrincipal(t, store, owner.ID)
	client := syncDeviceTestPrincipal(t, store, owner.ID)
	projectHost := workspaceSyncHostForFolderTest(t, ctx, store, owner.ID, projectWorkspace.ID)
	rootHost := workspaceSyncHostForFolderTest(t, ctx, store, owner.ID, rootWorkspace.ID)

	service, err := app.NewWorkspaceSyncDeviceService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.CreateFolder(ctx, portal, projectHost.ID, "Café", "project-cafe-key-0001")
	if err != nil {
		t.Fatalf("register first project metadata: %v", err)
	}
	replayed, err := service.CreateFolder(ctx, portal, projectHost.ID, "Café", "project-cafe-key-0001")
	if err != nil || replayed.FolderID != first.FolderID {
		t.Fatalf("idempotent project registration = %#v, %v; want folder %q", replayed, err, first.FolderID)
	}
	second, err := service.CreateFolder(ctx, portal, projectHost.ID, "tools", "project-tools-key-0002")
	if err != nil {
		t.Fatalf("register second project metadata in same Workspace: %v", err)
	}
	if _, err := service.CreateFolder(ctx, portal, projectHost.ID, "CAFÉ", "project-case-key-0003"); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("case-insensitive project collision = %v; want workspace conflict", err)
	}
	if _, err := service.CreateFolder(ctx, portal, projectHost.ID, "Cafe\u0301", "project-nfc-key-0004"); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("NFC-equivalent project collision = %v; want workspace conflict", err)
	}
	folders, err := service.ListFolders(ctx, portal, projectHost.ID)
	if err != nil || len(folders) != 2 || folders[0].FolderID == folders[1].FolderID {
		t.Fatalf("list multiple project folders = %#v, %v", folders, err)
	}
	for index, folder := range []workspace.SyncFolder{first, second} {
		certificate := []byte("folder-bound challenge " + folder.ProjectName)
		certificateHash := sha256.Sum256(certificate)
		deviceID := "EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE"
		if index == 1 {
			deviceID = "FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF"
		}
		challenge := newSyncDeviceTestChallenge(t, owner.ID, projectWorkspace.ID, client.ClientInstallationID, deviceID, certificate, certificateHash[:])
		challenge.FolderID, challenge.FolderKind, challenge.RelativePath = folder.FolderID, workspace.SyncFolderKindProject, folder.RelativePath
		if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, client, challenge); err != nil {
			t.Fatalf("create challenge bound to project folder %s: %v", folder.FolderID, err)
		}
		var storedFolderID, storedPath string
		if err := pool.QueryRow(ctx, `SELECT folder_id,relative_path FROM workspace_sync_device_challenges WHERE id=$1`, challenge.ID).
			Scan(&storedFolderID, &storedPath); err != nil || storedFolderID != folder.FolderID || storedPath != folder.RelativePath {
			t.Fatalf("folder-bound challenge metadata = %q %q, err=%v", storedFolderID, storedPath, err)
		}
	}

	// Root and project metadata are mutually exclusive both in Store behavior
	// and in the database, including a root mapping created by legacy callers.
	rootFolderID := "morde-99999999999999999999999999999999"
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_folders
(workspace_id,owner_id,folder_id,state,folder_kind,project_name,relative_path)
VALUES ($1,$2,$3,'pending_node','workspace_root',NULL,'.')`, rootWorkspace.ID, owner.ID, rootFolderID); err != nil {
		t.Fatalf("insert synthetic legacy root metadata: %v", err)
	}
	if _, err := service.CreateFolder(ctx, portal, rootHost.ID, "blocked-project", "root-project-key-0005"); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("project registration beside legacy root = %v; want workspace conflict", err)
	}
	conflictingRootID := "morde-88888888888888888888888888888888"
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_folders
(workspace_id,owner_id,folder_id,state,folder_kind,project_name,relative_path)
VALUES ($1,$2,$3,'pending_node','workspace_root',NULL,'.')`, projectWorkspace.ID, owner.ID, conflictingRootID); err == nil {
		t.Fatal("database accepted root metadata beside project metadata")
	}
	rootChallengeCertificate := []byte("root challenge")
	rootChallengeHash := sha256.Sum256(rootChallengeCertificate)
	rootChallenge := newSyncDeviceTestChallenge(t, owner.ID, projectWorkspace.ID, client.ClientInstallationID,
		"CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC", rootChallengeCertificate, rootChallengeHash[:])
	if err := store.CreateWorkspaceSyncDeviceChallenge(ctx, client, rootChallenge); domain.CodeOf(err) != domain.CodeWorkspaceUnavailable {
		t.Fatalf("legacy root challenge after project registration = %v; want unavailable", err)
	}

	// Synthetic metadata proves that a single installation/device may be
	// related to different project folder IDs, while composite foreign keys
	// reject a binding or receipt that names another folder.
	identityID, err := domain.NewWorkspaceSyncDeviceIdentityID()
	if err != nil {
		t.Fatal(err)
	}
	deviceID := "DDDDDDD-DDDDDDD-DDDDDDD-DDDDDDD-DDDDDDD-DDDDDDD-DDDDDDD-DDDDDDD"
	certificateHash := sha256.Sum256([]byte("synthetic folder metadata identity"))
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_device_identities
(id,owner_id,client_installation_id,device_id,certificate_sha256)
VALUES ($1,$2,$3,$4,$5)`, identityID, owner.ID, client.ClientInstallationID, deviceID, certificateHash[:]); err != nil {
		t.Fatalf("insert synthetic device identity: %v", err)
	}
	firstBindingID := mustNewSyncBindingID(t)
	secondBindingID := mustNewSyncBindingID(t)
	for _, item := range []struct{ bindingID, folderID string }{
		{firstBindingID, first.FolderID}, {secondBindingID, second.FolderID},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_device_bindings
(id,owner_id,workspace_id,folder_id,client_installation_id,device_identity_id,state)
VALUES ($1,$2,$3,$4,$5,$6,'pending_node')`, item.bindingID, owner.ID, projectWorkspace.ID,
			item.folderID, client.ClientInstallationID, identityID); err != nil {
			t.Fatalf("bind same device to project folder %s: %v", item.folderID, err)
		}
	}
	wrongFolderID := "morde-77777777777777777777777777777777"
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_device_bindings
(id,owner_id,workspace_id,folder_id,client_installation_id,device_identity_id,state)
VALUES ($1,$2,$3,$4,$5,$6,'pending_node')`, mustNewSyncBindingID(t), owner.ID, projectWorkspace.ID,
		wrongFolderID, client.ClientInstallationID, identityID); err == nil {
		t.Fatal("database accepted a binding whose Workspace/folder foreign key does not exist")
	}

	keyHash := sha256.Sum256([]byte("same idempotency key on separate folder scopes"))
	firstRequestHash := sha256.Sum256([]byte("first folder registration"))
	secondRequestHash := sha256.Sum256([]byte("second folder registration"))
	for _, item := range []struct {
		bindingID, folderID string
		requestHash         []byte
	}{
		{firstBindingID, first.FolderID, firstRequestHash[:]}, {secondBindingID, second.FolderID, secondRequestHash[:]},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_device_registration_requests
(owner_id,workspace_id,folder_id,idempotency_key_hash,request_hash,binding_id)
VALUES ($1,$2,$3,$4,$5,$6)`, owner.ID, projectWorkspace.ID, item.folderID, keyHash[:], item.requestHash, item.bindingID); err != nil {
			t.Fatalf("insert folder-scoped registration receipt: %v", err)
		}
	}
	wrongReceiptKeyHash := sha256.Sum256([]byte("wrong-folder-receipt-key"))
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_device_registration_requests
(owner_id,workspace_id,folder_id,idempotency_key_hash,request_hash,binding_id)
VALUES ($1,$2,$3,$4,$5,$6)`, owner.ID, projectWorkspace.ID, first.FolderID,
		wrongReceiptKeyHash[:], firstRequestHash[:], secondBindingID); err == nil {
		t.Fatal("database accepted a registration receipt pointing at a binding from another folder")
	}
	listedBindings, err := store.ListWorkspaceSyncDeviceBindings(ctx, portal, projectWorkspace.ID)
	if err != nil || len(listedBindings) != 2 || listedBindings[0].FolderID == listedBindings[1].FolderID {
		t.Fatalf("list folder-scoped device bindings = %#v, %v", listedBindings, err)
	}
	rootNodeBindings, err := store.ListWorkspaceSyncNodeBindings(ctx, owner.ID, projectWorkspace.ID)
	if err != nil || len(rootNodeBindings) != 0 {
		t.Fatalf("project bindings appeared as legacy root node bindings: %#v, %v", rootNodeBindings, err)
	}

	// Receive reservation and quota identities are also scoped by folder. The
	// application gate keeps project writes unavailable; direct synthetic rows
	// exercise only the database relationship and composite idempotency keys.
	operationID := strings.Repeat("a", 64)
	requestHash := sha256.Sum256([]byte("synthetic receive operation"))
	for _, folderID := range []string{first.FolderID, second.FolderID} {
		reservationID, err := domain.NewPendingRequestID()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_receive_reservations
(id,owner_id,workspace_id,folder_id,operation_id,request_hash,file_path,folded_path,operation,target_size,base_revision,state)
VALUES ($1,$2,$3,$4,$5,$6,'file.txt','file.txt','upsert',1,0,'aborted')`, reservationID, owner.ID, projectWorkspace.ID,
			folderID, operationID, requestHash[:]); err != nil {
			t.Fatalf("insert folder-scoped receive reservation: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_receive_quota_rejections
(owner_id,workspace_id,folder_id,operation_id,request_hash,file_path,folded_path,operation,target_size,reason_code,state)
VALUES ($1,$2,$3,$4,$5,'file.txt','file.txt','upsert',1,'workspace_quota_exceeded','paused')`, owner.ID, projectWorkspace.ID,
			folderID, operationID, requestHash[:]); err != nil {
			t.Fatalf("insert folder-scoped quota rejection: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_receive_reservations
(id,owner_id,workspace_id,folder_id,operation_id,request_hash,file_path,folded_path,operation,target_size,base_revision,state)
VALUES ($1,$2,$3,$4,$5,$6,'bad.txt','bad.txt','upsert',1,0,'aborted')`, mustNewPendingID(t), owner.ID, rootWorkspace.ID,
		first.FolderID, strings.Repeat("b", 64), requestHash[:]); err == nil {
		t.Fatal("database accepted a receive reservation whose Workspace/folder foreign key does not match")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_receive_quota_rejections
(owner_id,workspace_id,folder_id,operation_id,request_hash,file_path,folded_path,operation,target_size,reason_code,state)
VALUES ($1,$2,$3,$4,$5,'bad.txt','bad.txt','upsert',1,'workspace_quota_exceeded','paused')`, owner.ID, rootWorkspace.ID,
		first.FolderID, strings.Repeat("c", 64), requestHash[:]); err == nil {
		t.Fatal("database accepted a quota rejection whose Workspace/folder foreign key does not match")
	}

	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 185); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot roll back multi-folder workspace sync data") {
		t.Fatalf("rollback with project folder metadata = %v; want rollback protection", err)
	}
	var folderRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_folders WHERE workspace_id=$1`, projectWorkspace.ID).Scan(&folderRows); err != nil || folderRows != 2 {
		t.Fatalf("project folder metadata after rejected rollback = %d, err=%v", folderRows, err)
	}
	var rootRows, projectRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_folders WHERE workspace_id=$1 AND folder_id=$2`, rootWorkspace.ID, rootFolderID).Scan(&rootRows); err != nil || rootRows != 1 {
		t.Fatalf("legacy root metadata after rejected rollback = %d, err=%v", rootRows, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_folders WHERE workspace_id=$1 AND folder_kind='project'`, projectWorkspace.ID).Scan(&projectRows); err != nil || projectRows != 2 {
		t.Fatalf("project metadata after rejected rollback = %d, err=%v", projectRows, err)
	}
}

func TestWorkspaceSyncMultiFolderMigrationBackfillsLegacyReceiveFolder(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "multi-folder-backfill")
	folderID := "morde-66666666666666666666666666666666"
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_folders
(workspace_id,owner_id,folder_id,state,folder_kind,project_name,relative_path)
VALUES ($1,$2,$3,'active','workspace_root',NULL,'.')`, value.ID, owner.ID, folderID); err != nil {
		t.Fatalf("insert legacy root folder metadata: %v", err)
	}
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 185); err != nil {
		t.Fatalf("return to pre-multi-folder schema: %v", err)
	}
	reservationID := mustNewPendingID(t)
	operationID := strings.Repeat("d", 64)
	requestHash := sha256.Sum256([]byte("legacy receive reservation"))
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_receive_reservations
(id,owner_id,workspace_id,operation_id,request_hash,file_path,folded_path,operation,target_size,base_revision,state)
VALUES ($1,$2,$3,$4,$5,'legacy.txt','legacy.txt','upsert',1,0,'aborted')`, reservationID, owner.ID, value.ID,
		operationID, requestHash[:]); err != nil {
		t.Fatalf("insert pre-migration receive reservation: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_receive_quota_rejections
(owner_id,workspace_id,operation_id,request_hash,file_path,folded_path,operation,target_size,reason_code,state)
VALUES ($1,$2,$3,$4,'legacy.txt','legacy.txt','upsert',1,'workspace_quota_exceeded','paused')`, owner.ID, value.ID,
		operationID, requestHash[:]); err != nil {
		t.Fatalf("insert pre-migration quota rejection: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("reapply multi-folder migration: %v", err)
	}
	var reservationFolder, quotaFolder string
	if err := pool.QueryRow(ctx, `SELECT folder_id FROM workspace_sync_receive_reservations WHERE id=$1`, reservationID).Scan(&reservationFolder); err != nil || reservationFolder != folderID {
		t.Fatalf("legacy reservation folder backfill = %q, err=%v; want %q", reservationFolder, err, folderID)
	}
	if err := pool.QueryRow(ctx, `SELECT folder_id FROM workspace_sync_receive_quota_rejections WHERE owner_id=$1 AND workspace_id=$2 AND operation_id=$3`, owner.ID, value.ID, operationID).Scan(&quotaFolder); err != nil || quotaFolder != folderID {
		t.Fatalf("legacy quota folder backfill = %q, err=%v; want %q", quotaFolder, err, folderID)
	}
}

func workspaceSyncPortalPrincipal(t *testing.T, store *Store, ownerID string) identity.Principal {
	t.Helper()
	token := clientTestToken(t, ownerID, "", identity.KindSession, nil)
	if _, err := store.CreateToken(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	return identity.Principal{UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: token.ID}
}

func workspaceSyncHostForFolderTest(t *testing.T, ctx context.Context, store *Store, ownerID, workspaceID string) host.Host {
	t.Helper()
	hostValue := createTestHost(t, ctx, store, ownerID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{workspaceID})
	hostValue = startWorkspaceSyncTestHost(t, ctx, store, hostValue)
	binding, err := store.GetWorkspaceBinding(ctx, hostValue.ID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, 0, 0); err != nil {
		t.Fatal(err)
	}
	return hostValue
}

func mustNewSyncBindingID(t *testing.T) string {
	t.Helper()
	id, err := domain.NewWorkspaceSyncDeviceBindingID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustNewPendingID(t *testing.T) string {
	t.Helper()
	id, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
