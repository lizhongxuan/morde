package postgres

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"morde/server/internal/app"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkspaceSyncFolderTargetsMigrationUpgradeDowngradeAndGuards(t *testing.T) {
	t.Run("multi-folder downgrade is guarded", func(t *testing.T) {
		store, pool := newTemporaryStore(t)
		ctx := context.Background()
		owner := createTestUser(t, store, 3, 10_000, 20_000)
		value := createTestWorkspace(t, ctx, store, owner.ID, "multi-folder-down-guard")
		folderID := "morde-cccccccccccccccccccccccccccccccc"
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_folders
(workspace_id,owner_id,folder_id,folder_kind,project_name,project_name_folded,relative_path)
VALUES ($1,$2,$3,'project','demo','demo','project/demo')`, value.ID, owner.ID, folderID); err != nil {
			t.Fatalf("insert project folder for multi-folder rollback guard: %v", err)
		}
		provider, closeProvider, err := newMigrationProvider(pool)
		if err != nil {
			t.Fatal(err)
		}
		defer closeProvider()
		if _, err := provider.DownTo(ctx, 185); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot roll back multi-folder workspace sync data") {
			t.Fatalf("multi-folder migration rollback error = %v", err)
		}
		var projectName, foldedName string
		if err := pool.QueryRow(ctx, `SELECT project_name,project_name_folded FROM workspace_sync_folders WHERE workspace_id=$1`, value.ID).
			Scan(&projectName, &foldedName); err != nil || projectName != "demo" || foldedName != "demo" {
			t.Fatalf("project folder after rejected multi-folder rollback: name=%q folded=%q err=%v", projectName, foldedName, err)
		}
	})

	t.Run("preserves legacy root rows across upgrade and rollback", func(t *testing.T) {
		store, pool := newTemporaryStore(t)
		ctx := context.Background()
		owner := createTestUser(t, store, 3, 10_000, 20_000)
		value := createTestWorkspace(t, ctx, store, owner.ID, "folder-target-migration")
		principal := syncDeviceTestPrincipal(t, store, owner.ID)
		certificate := []byte("migration test certificate")
		certificateHash := sha256.Sum256(certificate)
		challenge := newSyncDeviceTestChallenge(t, owner.ID, value.ID, principal.ClientInstallationID,
			"AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA", certificate, certificateHash[:])
		folderID := "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

		provider, closeProvider, err := newMigrationProvider(pool)
		if err != nil {
			t.Fatal(err)
		}
		defer closeProvider()
		if _, err := provider.DownTo(ctx, 184); err != nil {
			t.Fatalf("roll back empty folder target migration: %v", err)
		}
		assertWorkspaceSyncFolderTargetsSchema(t, ctx, pool, false)
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_folders (workspace_id,owner_id,folder_id,state)
VALUES ($1,$2,$3,'active')`, value.ID, owner.ID, folderID); err != nil {
			t.Fatalf("insert legacy Workspace sync folder: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_device_challenges
(id,owner_id,workspace_id,client_installation_id,device_id,certificate_der,certificate_sha256,nonce,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, challenge.ID, owner.ID, value.ID, principal.ClientInstallationID,
			challenge.DeviceID, challenge.CertificateDER, challenge.CertificateSHA256, challenge.Nonce, challenge.ExpiresAt); err != nil {
			t.Fatalf("insert legacy Workspace sync device challenge: %v", err)
		}
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("upgrade Workspace sync folder targets: %v", err)
		}
		assertWorkspaceSyncFolderTargetsSchema(t, ctx, pool, true)
		assertWorkspaceSyncFolderTargetDefaults(t, ctx, pool, value.ID, challenge.ID, folderID, "active")

		if _, err := provider.DownTo(ctx, 184); err != nil {
			t.Fatalf("roll back legacy root-only folder target data: %v", err)
		}
		assertWorkspaceSyncFolderTargetsSchema(t, ctx, pool, false)
		var folderExists, challengeExists bool
		if err := pool.QueryRow(ctx, `SELECT
EXISTS (SELECT 1 FROM workspace_sync_folders WHERE workspace_id=$1 AND folder_id=$2),
EXISTS (SELECT 1 FROM workspace_sync_device_challenges WHERE id=$3)`, value.ID, folderID, challenge.ID).
			Scan(&folderExists, &challengeExists); err != nil || !folderExists || !challengeExists {
			t.Fatalf("legacy rows after rollback: folder=%t challenge=%t err=%v", folderExists, challengeExists, err)
		}
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("reapply Workspace sync folder targets: %v", err)
		}
		assertWorkspaceSyncFolderTargetsSchema(t, ctx, pool, true)
		assertWorkspaceSyncFolderTargetDefaults(t, ctx, pool, value.ID, challenge.ID, folderID, "active")
	})

	t.Run("constraints and data block unsafe rollback", func(t *testing.T) {
		store, pool := newTemporaryStore(t)
		ctx := context.Background()
		owner := createTestUser(t, store, 3, 10_000, 20_000)
		value := createTestWorkspace(t, ctx, store, owner.ID, "folder-target-guards")
		principal := syncDeviceTestPrincipal(t, store, owner.ID)
		certificate := []byte("migration guard certificate")
		certificateHash := sha256.Sum256(certificate)
		challenge := newSyncDeviceTestChallenge(t, owner.ID, value.ID, principal.ClientInstallationID,
			"BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB", certificate, certificateHash[:])
		hostValue := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, nil)
		provider, closeProvider, err := newMigrationProvider(pool)
		if err != nil {
			t.Fatal(err)
		}
		defer closeProvider()
		if _, err := provider.DownTo(ctx, 185); err != nil {
			t.Fatalf("roll back empty multi-folder migration for target guards: %v", err)
		}
		folderID := "morde-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_folders (workspace_id,owner_id,folder_id)
VALUES ($1,$2,$3)`, value.ID, owner.ID, folderID); err != nil {
			t.Fatalf("insert root Workspace sync folder: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_device_challenges
(id,owner_id,workspace_id,folder_id,folder_kind,relative_path,client_installation_id,device_id,certificate_der,certificate_sha256,nonce,expires_at)
VALUES ($1,$2,$3,NULL,'workspace_root','.', $4,$5,$6,$7,$8,$9)`, challenge.ID, owner.ID, value.ID,
			principal.ClientInstallationID, challenge.DeviceID, challenge.CertificateDER, challenge.CertificateSHA256, challenge.Nonce, challenge.ExpiresAt); err != nil {
			t.Fatalf("insert root Workspace sync device challenge: %v", err)
		}

		if _, err := pool.Exec(ctx, `UPDATE workspace_sync_folders SET folder_kind='project',project_name='demo',relative_path='project/other'
WHERE workspace_id=$1`, value.ID); err == nil {
			t.Fatal("accepted a project folder whose path does not match its name")
		}
		if _, err := pool.Exec(ctx, `UPDATE workspace_sync_device_challenges
SET folder_id='morde-cccccccccccccccccccccccccccccccc',folder_kind='project',relative_path='project/missing'
WHERE id=$1`, challenge.ID); err == nil {
			t.Fatal("accepted a challenge referring to a missing folder target")
		}
		assertWorkspaceSyncFolderTargetDefaults(t, ctx, pool, value.ID, challenge.ID, folderID, "pending_node")

		keyHash := sha256.Sum256([]byte("folder-target-registration-key"))
		requestHash := sha256.Sum256([]byte("folder-target-registration-request"))
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_folder_registration_requests
(owner_id,host_id,workspace_id,idempotency_key_hash,request_hash,folder_id)
VALUES ($1,$2,$3,$4,$5,$6)`, owner.ID, hostValue.ID, value.ID, keyHash[:], requestHash[:], folderID); err != nil {
			t.Fatalf("insert folder registration request rollback guard: %v", err)
		}
		assertWorkspaceSyncFolderTargetsMigrationDownRejected(t, ctx, pool)
		if _, err := pool.Exec(ctx, `DELETE FROM workspace_sync_folder_registration_requests WHERE owner_id=$1 AND host_id=$2`, owner.ID, hostValue.ID); err != nil {
			t.Fatal(err)
		}

		if _, err := pool.Exec(ctx, `UPDATE workspace_sync_folders SET folder_kind='project',project_name='demo',relative_path='project/demo'
WHERE workspace_id=$1`, value.ID); err != nil {
			t.Fatalf("set project folder rollback guard: %v", err)
		}
		assertWorkspaceSyncFolderTargetsMigrationDownRejected(t, ctx, pool)
		if _, err := pool.Exec(ctx, `UPDATE workspace_sync_folders SET folder_kind='workspace_root',project_name=NULL,relative_path='.'
WHERE workspace_id=$1`, value.ID); err != nil {
			t.Fatalf("restore root folder target: %v", err)
		}

		if _, err := pool.Exec(ctx, `UPDATE workspace_sync_device_challenges
SET folder_id=$2,folder_kind='project',relative_path='project/demo' WHERE id=$1`, challenge.ID, folderID); err != nil {
			t.Fatalf("set folder-bound challenge rollback guard: %v", err)
		}
		assertWorkspaceSyncFolderTargetsMigrationDownRejected(t, ctx, pool)
	})
}

func assertWorkspaceSyncFolderTargetsSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool, expected bool) {
	t.Helper()
	var folderKind, projectName, relativePath, challengeFolder bool
	var registrationTable bool
	if err := pool.QueryRow(ctx, `SELECT
EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='workspace_sync_folders' AND column_name='folder_kind'),
EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='workspace_sync_folders' AND column_name='project_name'),
EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='workspace_sync_folders' AND column_name='relative_path'),
EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='workspace_sync_device_challenges' AND column_name='folder_id'),
to_regclass('workspace_sync_folder_registration_requests') IS NOT NULL`).
		Scan(&folderKind, &projectName, &relativePath, &challengeFolder, &registrationTable); err != nil {
		t.Fatal(err)
	}
	if folderKind != expected || projectName != expected || relativePath != expected || challengeFolder != expected || registrationTable != expected {
		t.Fatalf("Workspace sync folder target schema: kind=%t project=%t path=%t challenge_folder=%t requests=%t; want %t",
			folderKind, projectName, relativePath, challengeFolder, registrationTable, expected)
	}
}

func assertWorkspaceSyncFolderTargetDefaults(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, challengeID, folderID, expectedState string) {
	t.Helper()
	var folderKind, relativePath, folderState string
	var projectNameNull bool
	if err := pool.QueryRow(ctx, `SELECT folder_kind,project_name IS NULL,relative_path,state
FROM workspace_sync_folders WHERE workspace_id=$1 AND owner_id=(SELECT owner_id FROM workspaces WHERE id=$1)`, workspaceID).
		Scan(&folderKind, &projectNameNull, &relativePath, &folderState); err != nil {
		t.Fatal(err)
	}
	if folderKind != "workspace_root" || !projectNameNull || relativePath != "." || folderState != expectedState {
		t.Fatalf("Workspace sync folder defaults: kind=%q project_null=%t path=%q state=%q", folderKind, projectNameNull, relativePath, folderState)
	}
	var challengeFolderNull bool
	var challengeKind, challengePath string
	if err := pool.QueryRow(ctx, `SELECT folder_id IS NULL,folder_kind,relative_path
FROM workspace_sync_device_challenges WHERE id=$1`, challengeID).
		Scan(&challengeFolderNull, &challengeKind, &challengePath); err != nil {
		t.Fatal(err)
	}
	if !challengeFolderNull || challengeKind != "workspace_root" || challengePath != "." {
		t.Fatalf("Workspace sync challenge defaults: folder_null=%t kind=%q path=%q", challengeFolderNull, challengeKind, challengePath)
	}
	var storedFolderID string
	if err := pool.QueryRow(ctx, `SELECT folder_id FROM workspace_sync_folders WHERE workspace_id=$1`, workspaceID).Scan(&storedFolderID); err != nil || storedFolderID != folderID {
		t.Fatalf("legacy folder ID = %q, err=%v; want %q", storedFolderID, err, folderID)
	}
}

func assertWorkspaceSyncFolderTargetsMigrationDownRejected(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 184); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot roll back workspace project folder registrations") {
		t.Fatalf("Workspace project folder registration rollback error = %v", err)
	}
	assertWorkspaceSyncFolderTargetsSchema(t, ctx, pool, true)
}
