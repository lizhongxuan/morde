package postgres

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

func TestWorkspaceProjectUploadUsesInstallationCredentialWithoutSyncthingBinding(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "project-upload-manual")
	hostValue := workspaceSyncHostForFolderTest(t, ctx, store, owner.ID, workspaceValue.ID)
	portal := workspaceSyncPortalPrincipal(t, store, owner.ID)
	folderService, err := app.NewWorkspaceSyncDeviceService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := folderService.CreateFolder(ctx, portal, hostValue.ID, "demo", "project-upload-folder-key")
	if err != nil {
		t.Fatalf("register project folder metadata: %v", err)
	}

	installationID := createTestClientInstallation(t, pool, owner.ID, nil)
	syncToken := clientTestToken(t, owner.ID, installationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, syncToken); err != nil {
		t.Fatal(err)
	}
	device := identity.Principal{
		UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindDevice, TokenID: syncToken.ID,
		ClientInstallationID: installationID, Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	var nodeBindings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_device_bindings WHERE workspace_id=$1 AND folder_id=$2`,
		workspaceValue.ID, folder.FolderID).Scan(&nodeBindings); err != nil || nodeBindings != 0 {
		t.Fatalf("project Syncthing bindings before upload = %d, err=%v; want zero", nodeBindings, err)
	}
	if err := store.CheckWorkspaceSyncProjectFolderAccess(ctx, device, workspaceValue.ID, folder.FolderID); err != nil {
		t.Fatalf("authorize installation-bound project upload without a Syncthing binding: %v", err)
	}

	syncService, err := app.NewWorkspaceSyncServiceWithOptions(store, workspaceTransferTestBlocks{}, workspace.MaxSyncManifestBytes, 1<<30,
		app.WorkspaceSyncOptions{ProjectUploadEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	request := workspace.CreateSyncSession{
		FolderID: folder.FolderID, SourceBindingID: "12345678-1234-4234-8234-123456789abc", Direction: workspace.SyncUpload, Policy: workspace.SyncCompareMerge,
		Files: []workspace.File{}, EmptyDirectories: []string{"empty"},
	}
	first, err := syncService.CreateSyncSession(ctx, device, workspaceValue.ID, request, "project-upload-create-key")
	if err != nil {
		t.Fatalf("create manual project upload session: %v", err)
	}
	if first.FolderBaselineRevision != nil || first.BaseRevision != workspaceValue.Revision || len(first.TransferBlocks) != 0 {
		t.Fatalf("first project upload snapshot = %#v", first)
	}
	_, err = syncService.CreateSyncSession(ctx, device, workspaceValue.ID, request, "project-upload-overlap-create-key")
	if domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("create while the source has a planned session = %v; want workspace busy", err)
	}
	committed, err := syncService.CommitSyncSession(ctx, device, workspaceValue.ID, first.ID, "project-upload-commit-key")
	if err != nil {
		t.Fatalf("commit metadata-only project upload: %v", err)
	}
	if committed.State != workspace.SyncCompleted || committed.TargetRevision <= workspaceValue.Revision {
		t.Fatalf("committed project upload = %#v", committed)
	}
	folderAfter, err := store.GetWorkspaceSyncFolder(ctx, device, workspaceValue.ID, folder.FolderID)
	if err != nil || folderAfter.LastUploadRevision != nil {
		t.Fatalf("project upload changed the legacy revision marker = %#v, err=%v; want it to remain unset", folderAfter.LastUploadRevision, err)
	}
	cloudOnlyFile := workspace.File{
		Path: "project/demo/cloud-only.txt", Kind: workspace.FileKindRegular, SizeBytes: 0,
		SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		Mode:   0o644, Chunks: []workspace.Chunk{},
	}
	cloudOnlyRevision := applySyntheticProjectMutationForTest(t, store, workspaceValue.ID, cloudOnlyFile.Path, &cloudOnlyFile, nil)
	if cloudOnlyRevision <= committed.TargetRevision {
		t.Fatalf("synthetic cloud-only file revision = %d; want after project upload revision %d", cloudOnlyRevision, committed.TargetRevision)
	}
	confirmWorkspaceProjectApplicationForTest(t, store, committed, cloudOnlyRevision, "applied")

	secondRequest := request
	secondRequest.EmptyDirectories = []string{}
	second, err := syncService.CreateSyncSession(ctx, device, workspaceValue.ID, secondRequest, "project-upload-second-create-key")
	if err != nil {
		t.Fatalf("create second project upload after Host receipt: %v", err)
	}
	if second.SourceBaselineSessionID != committed.ID || len(second.UploadDeletedDirectories) != 1 ||
		second.UploadDeletedDirectories[0] != "project/demo/empty" {
		t.Fatalf("second upload baseline/deletion = %#v; want Host-confirmed session %q and its owned directory deletion", second, committed.ID)
	}
	secondCommitted, err := syncService.CommitSyncSession(ctx, device, workspaceValue.ID, second.ID, "project-upload-second-commit-key")
	if err != nil || secondCommitted.State != workspace.SyncCompleted || secondCommitted.TargetRevision <= committed.TargetRevision {
		t.Fatalf("commit second project upload = %#v, err=%v; want completed after advancing server revision marker", secondCommitted, err)
	}
	_, err = syncService.CreateSyncSession(ctx, device, workspaceValue.ID, secondRequest, "project-upload-pending-apply-create-key")
	if domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("create destructive session while Host application is pending = %v; want workspace busy", err)
	}
	rejectWorkspaceProjectApplicationAfterBaselineMovesForTest(t, store, secondCommitted, committed.ID,
		committed.TargetRevision, committed.HostApplication.ManifestSHA256)
	manifestAfterSecond, err := store.GetWorkspaceManifestPage(ctx, workspaceValue.ID, secondCommitted.TargetRevision, "project/demo", "", 100)
	if err != nil {
		t.Fatalf("read project tree after second upload: %v", err)
	}
	cloudOnlyPreserved := false
	for _, file := range manifestAfterSecond.Files {
		if file.Path == cloudOnlyFile.Path {
			cloudOnlyPreserved = true
			break
		}
	}
	if !cloudOnlyPreserved {
		t.Fatalf("cloud-only file was deleted by the second PC upload: %#v", manifestAfterSecond.Files)
	}
	cloudRecreateRevision := applySyntheticProjectMutationForTest(t, store, workspaceValue.ID, "project/demo/empty", nil, nil)
	if cloudRecreateRevision <= secondCommitted.TargetRevision {
		t.Fatalf("synthetic affected-path revision = %d; want after project upload revision %d", cloudRecreateRevision, secondCommitted.TargetRevision)
	}
	confirmWorkspaceProjectApplicationForTest(t, store, secondCommitted, cloudRecreateRevision, "conflict")
	verifiedSession, err := syncService.GetSyncSession(ctx, device, workspaceValue.ID, secondCommitted.ID)
	if err != nil || verifiedSession.HostApplication == nil || verifiedSession.HostApplication.Status != "conflict" ||
		len(verifiedSession.HostApplication.Impacts) != 1 || verifiedSession.HostApplication.Impacts[0].Path != "empty" {
		t.Fatalf("higher Host revision with changed affected path = %#v, err=%v", verifiedSession.HostApplication, err)
	}
	var lastAppliedSession string
	if err := store.pool.QueryRow(ctx, `SELECT last_applied_session_id FROM workspace_sync_project_sources WHERE source_binding_id=$1`,
		second.SourceBindingID).Scan(&lastAppliedSession); err != nil || lastAppliedSession != committed.ID {
		t.Fatalf("source baseline after affected-path conflict = %q, err=%v; want previous confirmed session %q", lastAppliedSession, err, committed.ID)
	}
	folderAfterSecond, err := store.GetWorkspaceSyncFolder(ctx, device, workspaceValue.ID, folder.FolderID)
	if err != nil || folderAfterSecond.LastUploadRevision != nil {
		t.Fatalf("second project upload changed the legacy revision marker = %#v, err=%v; want it to remain unset",
			folderAfterSecond.LastUploadRevision, err)
	}
	var applicationState string
	if err := pool.QueryRow(ctx, `SELECT state FROM workspace_sync_project_upload_applications WHERE workspace_id=$1 AND session_id=$2`,
		workspaceValue.ID, secondCommitted.ID).Scan(&applicationState); err != nil || applicationState != "conflict" {
		t.Fatalf("second project application state = %q, err=%v; want conflict", applicationState, err)
	}

	_, err = syncService.CreateSyncSession(ctx, device, workspaceValue.ID, secondRequest, "project-upload-pending-receipt-create-key")
	if domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("create destructive session while Host receipt is conflicted = %v; want workspace busy", err)
	}
	var sessionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_sessions WHERE workspace_id=$1 AND folder_id=$2`,
		workspaceValue.ID, folder.FolderID).Scan(&sessionCount); err != nil || sessionCount != 2 {
		t.Fatalf("project upload sessions while receipt is conflicted = %d, err=%v; want only the two committed sessions", sessionCount, err)
	}
	replayed, err := syncService.CreateSyncSession(ctx, device, workspaceValue.ID, request, "project-upload-create-key")
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("same project upload request after baseline advance = session %q, err=%v; want original %q", replayed.ID, err, first.ID)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_device_bindings WHERE workspace_id=$1 AND folder_id=$2`,
		workspaceValue.ID, folder.FolderID).Scan(&nodeBindings); err != nil || nodeBindings != 0 {
		t.Fatalf("project Syncthing bindings after upload = %d, err=%v; want zero", nodeBindings, err)
	}
}

func TestWorkspaceProjectUploadReceiptRejectsConcurrentBaselineAdvance(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "project-upload-receipt-baseline-race")
	hostValue := workspaceSyncHostForFolderTest(t, ctx, store, owner.ID, workspaceValue.ID)
	folder, err := app.NewWorkspaceSyncDeviceService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	registeredFolder, err := folder.CreateFolder(ctx, workspaceSyncPortalPrincipal(t, store, owner.ID), hostValue.ID,
		"demo", "project-upload-receipt-baseline-race-folder")
	if err != nil {
		t.Fatalf("register project folder metadata: %v", err)
	}
	installationID := createTestClientInstallation(t, pool, owner.ID, nil)
	syncToken := clientTestToken(t, owner.ID, installationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, syncToken); err != nil {
		t.Fatal(err)
	}
	device := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindDevice, TokenID: syncToken.ID,
		ClientInstallationID: installationID, Scopes: []identity.Scope{identity.ScopeWorkspaceSync}}
	syncService, err := app.NewWorkspaceSyncServiceWithOptions(store, workspaceTransferTestBlocks{}, workspace.MaxSyncManifestBytes, 1<<30,
		app.WorkspaceSyncOptions{ProjectUploadEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	request := workspace.CreateSyncSession{FolderID: registeredFolder.FolderID,
		SourceBindingID: "12345678-1234-4234-8234-123456789abc", Direction: workspace.SyncUpload,
		Policy: workspace.SyncCompareMerge, EmptyDirectories: []string{"baseline"}}
	first, err := syncService.CreateSyncSession(ctx, device, workspaceValue.ID, request, "project-upload-race-first-create")
	if err != nil {
		t.Fatalf("create first project upload: %v", err)
	}
	first, err = syncService.CommitSyncSession(ctx, device, workspaceValue.ID, first.ID, "project-upload-race-first-commit")
	if err != nil || first.State != workspace.SyncCompleted {
		t.Fatalf("commit first project upload: session=%#v err=%v", first, err)
	}
	confirmWorkspaceProjectApplicationForTest(t, store, first, first.TargetRevision, "applied")

	request.EmptyDirectories = []string{"next"}
	second, err := syncService.CreateSyncSession(ctx, device, workspaceValue.ID, request, "project-upload-race-second-create")
	if err != nil || second.SourceBaselineSessionID != first.ID {
		t.Fatalf("create second project upload with prior Host baseline: session=%#v err=%v", second, err)
	}
	second, err = syncService.CommitSyncSession(ctx, device, workspaceValue.ID, second.ID, "project-upload-race-second-commit")
	if err != nil || second.State != workspace.SyncCompleted {
		t.Fatalf("commit second project upload: session=%#v err=%v", second, err)
	}

	var hostID string
	var bindingVersion, targetRevision, generation int64
	var manifestDigest []byte
	if err := pool.QueryRow(ctx, `SELECT host_id,workspace_binding_version,target_revision,manifest_sha256
FROM workspace_sync_project_upload_applications WHERE workspace_id=$1 AND session_id=$2 AND state='pending_apply'`,
		second.WorkspaceID, second.ID).Scan(&hostID, &bindingVersion, &targetRevision, &manifestDigest); err != nil {
		t.Fatalf("read pending application for race: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT runtime_generation FROM hosts WHERE id=$1`, hostID).Scan(&generation); err != nil {
		t.Fatalf("read Host generation for race receipt: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_bindings
SET desired_revision=GREATEST(desired_revision,$4),applied_revision=$4,sync_state='healthy',sync_conflicts=ARRAY[]::text[],updated_at=now()
WHERE host_id=$1 AND workspace_id=$2 AND binding_version=$3 AND state='active'`,
		hostID, second.WorkspaceID, bindingVersion, targetRevision); err != nil {
		t.Fatalf("prepare healthy Host binding for race receipt: %v", err)
	}

	raceCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	competingTx, err := pool.Begin(raceCtx)
	if err != nil {
		t.Fatalf("begin competing source-baseline transaction: %v", err)
	}
	defer func() { _ = competingTx.Rollback(context.Background()) }()
	var oldBaselineSession string
	if err := pool.QueryRow(raceCtx, `SELECT last_applied_session_id FROM workspace_sync_project_sources WHERE source_binding_id=$1`,
		second.SourceBindingID).Scan(&oldBaselineSession); err != nil || oldBaselineSession != first.ID {
		t.Fatalf("source baseline before race = %q err=%v; want %q", oldBaselineSession, err, first.ID)
	}
	updated, err := competingTx.Exec(raceCtx, `UPDATE workspace_sync_project_sources
SET last_applied_session_id=NULL,last_applied_target_revision=NULL,last_applied_manifest_sha256=NULL,updated_at=now()
	WHERE source_binding_id=$1 AND last_applied_session_id=$2`, second.SourceBindingID, first.ID)
	if err != nil {
		t.Fatalf("move source baseline in competing transaction: %v", err)
	}
	if updated.RowsAffected() != 1 {
		t.Fatalf("competing source-baseline update affected %d rows; want 1", updated.RowsAffected())
	}

	healthy := workspace.BindingSyncHealthy
	report := workspace.HostProjectUploadApplicationReport{SourceBindingID: second.SourceBindingID, TargetRevision: targetRevision,
		ManifestSHA256: hex.EncodeToString(manifestDigest), Status: "applied", AppliedRevision: &targetRevision,
		SyncState: &healthy, Impacts: []workspace.SyncHostApplicationImpact{}}
	principal := identity.Principal{UserID: second.OwnerID, Kind: identity.KindHostReplica, HostScope: hostID,
		HostGeneration: generation, ReplicaWorkspaceID: second.WorkspaceID, BindingVersion: bindingVersion}
	receiptResult := make(chan error, 1)
	go func() {
		_, err := store.RecordWorkspaceSyncProjectUploadApplication(raceCtx, principal, second.WorkspaceID, bindingVersion, second.ID, report)
		receiptResult <- err
	}()

	// Wait until the receipt transaction is visibly blocked on the source row held by competingTx.
	// This uses PostgreSQL's lock wait state rather than timing assumptions or fixed sleeps.
	waitForPostgresLockWait(t, raceCtx, pool, "%workspace_sync_project_sources%FOR UPDATE%", 1)
	if err := competingTx.Commit(raceCtx); err != nil {
		t.Fatalf("commit competing source-baseline transaction: %v", err)
	}
	select {
	case err := <-receiptResult:
		if domain.CodeOf(err) != domain.CodeWorkspaceConflict {
			t.Fatalf("receipt after concurrent baseline advance = %v; want workspace conflict", err)
		}
	case <-raceCtx.Done():
		t.Fatalf("receipt remained blocked after competing baseline commit: %v", raceCtx.Err())
	}

	var state string
	var receiptDigest []byte
	if err := pool.QueryRow(ctx, `SELECT state,receipt_sha256 FROM workspace_sync_project_upload_applications
WHERE workspace_id=$1 AND session_id=$2`, second.WorkspaceID, second.ID).Scan(&state, &receiptDigest); err != nil ||
		state != "pending_apply" || len(receiptDigest) != 0 {
		t.Fatalf("application after concurrent stale receipt = state %q receipt %x err %v; want pending with no receipt", state, receiptDigest, err)
	}
	var baselineSession string
	var baselineRevisionIsNull, baselineManifestIsNull bool
	if err := pool.QueryRow(ctx, `SELECT COALESCE(last_applied_session_id,''),last_applied_target_revision IS NULL,
last_applied_manifest_sha256 IS NULL FROM workspace_sync_project_sources WHERE source_binding_id=$1`,
		second.SourceBindingID).Scan(&baselineSession, &baselineRevisionIsNull, &baselineManifestIsNull); err != nil {
		t.Fatalf("read source baseline after concurrent receipt: %v", err)
	}
	if baselineSession != "" || !baselineRevisionIsNull || !baselineManifestIsNull {
		t.Fatalf("source baseline after stale receipt = session %q revision NULL=%t manifest NULL=%t; want competing transaction's empty baseline",
			baselineSession, baselineRevisionIsNull, baselineManifestIsNull)
	}
}

func applySyntheticProjectMutationForTest(t *testing.T, store *Store, workspaceID, path string, file *workspace.File,
	metadata *workspace.Directory) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin synthetic Workspace revision: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var revision, quotaBytes, fileCount, totalBytes int64
	if err := tx.QueryRow(ctx, `SELECT w.revision,w.quota_bytes,r.file_count,r.total_bytes
FROM workspaces w JOIN workspace_revisions r ON r.workspace_id=w.id AND r.revision=w.revision
WHERE w.id=$1 FOR UPDATE OF w`, workspaceID).Scan(&revision, &quotaBytes, &fileCount, &totalBytes); err != nil {
		t.Fatalf("lock synthetic Workspace revision: %v", err)
	}
	var files []syncMutation
	var directories []syncDirectoryMutation
	var directoryMetadata []syncDirectoryMetadataMutation
	if file != nil {
		files = []syncMutation{{path: path, file: file}}
	} else if metadata == nil {
		directories = []syncDirectoryMutation{{path: path}}
	} else {
		directoryMetadata = []syncDirectoryMetadataMutation{{path: path, directory: metadata}}
	}
	if err := applyWorkspaceSyncTreeMutations(ctx, tx, workspaceID, revision, fileCount, totalBytes, quotaBytes,
		files, directories, nil, directoryMetadata); err != nil {
		t.Fatalf("apply synthetic Workspace tree change: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit synthetic Workspace revision: %v", err)
	}
	return revision + 1
}

func TestProjectUploadConflictDoesNotApplyNonconflictingMutations(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "project-upload-mixed-conflict")
	hostValue := workspaceSyncHostForFolderTest(t, ctx, store, owner.ID, workspaceValue.ID)
	portal := workspaceSyncPortalPrincipal(t, store, owner.ID)
	folderService, err := app.NewWorkspaceSyncDeviceService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := folderService.CreateFolder(ctx, portal, hostValue.ID, "demo", "project-upload-mixed-conflict-folder")
	if err != nil {
		t.Fatal(err)
	}
	installationID := createTestClientInstallation(t, store.pool, owner.ID, nil)
	syncToken := clientTestToken(t, owner.ID, installationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, syncToken); err != nil {
		t.Fatal(err)
	}
	device := identity.Principal{
		UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindDevice, TokenID: syncToken.ID,
		ClientInstallationID: installationID, Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	syncService, err := app.NewWorkspaceSyncServiceWithOptions(store, workspaceTransferTestBlocks{}, workspace.MaxSyncManifestBytes, 1<<30,
		app.WorkspaceSyncOptions{ProjectUploadEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	baseMode, baseTime := uint32(0o755), int64(100)
	first, err := syncService.CreateSyncSession(ctx, device, workspaceValue.ID, workspace.CreateSyncSession{
		FolderID: folder.FolderID, SourceBindingID: "12345678-1234-4234-8234-123456789abc",
		Direction: workspace.SyncUpload, Policy: workspace.SyncCompareMerge,
		EmptyDirectories: []string{"owned"}, Directories: []workspace.Directory{
			{Path: ".", Mode: &baseMode, ModTimeNS: &baseTime, MetadataKnown: true},
			{Path: "owned", Mode: &baseMode, ModTimeNS: &baseTime, MetadataKnown: true},
		},
	}, "project-upload-mixed-conflict-baseline")
	if err != nil || len(first.TransferBlocks) != 0 {
		t.Fatalf("create metadata-only project baseline = %#v, err=%v", first, err)
	}
	committed, err := syncService.CommitSyncSession(ctx, device, workspaceValue.ID, first.ID, "project-upload-mixed-conflict-baseline-commit")
	if err != nil || committed.State != workspace.SyncCompleted {
		t.Fatalf("commit project baseline = %#v, err=%v", committed, err)
	}
	confirmWorkspaceProjectApplicationForTest(t, store, committed, committed.TargetRevision, "applied")

	remoteMode, remoteTime := uint32(0o700), int64(200)
	remoteDirectory := workspace.Directory{Path: "project/demo", Mode: &remoteMode, ModTimeNS: &remoteTime, MetadataKnown: true}
	remoteRevision := applySyntheticProjectMutationForTest(t, store, workspaceValue.ID, remoteDirectory.Path, nil, &remoteDirectory)
	localMode, localTime := uint32(0o711), int64(300)
	second, err := syncService.CreateSyncSession(ctx, device, workspaceValue.ID, workspace.CreateSyncSession{
		FolderID: folder.FolderID, SourceBindingID: "12345678-1234-4234-8234-123456789abc",
		Direction: workspace.SyncUpload, Policy: workspace.SyncCompareMerge,
		EmptyDirectories: []string{"owned", "new"},
		Directories: []workspace.Directory{
			{Path: ".", Mode: &localMode, ModTimeNS: &localTime, MetadataKnown: true},
			{Path: "owned", Mode: &baseMode, ModTimeNS: &baseTime, MetadataKnown: true},
			{Path: "new", Mode: &baseMode, ModTimeNS: &baseTime, MetadataKnown: true},
		},
	}, "project-upload-mixed-conflict-second")
	if err != nil || second.SourceBaselineSessionID != first.ID || len(second.Conflicts) != 1 ||
		second.Conflicts[0].Path != "project/demo" || second.Conflicts[0].Reason != "metadata_changed" ||
		len(second.UploadEmptyDirectories) != 1 || second.UploadEmptyDirectories[0] != "project/demo/new" {
		t.Fatalf("mixed-conflict plan = %#v, err=%v; want one metadata conflict plus one independent upload", second, err)
	}
	conflicted, err := syncService.CommitSyncSession(ctx, device, workspaceValue.ID, second.ID, "project-upload-mixed-conflict-second-commit")
	if err != nil || conflicted.State != workspace.SyncConflict {
		t.Fatalf("commit mixed-conflict project upload = %#v, err=%v; want conflict", conflicted, err)
	}
	workspaceAfter, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || workspaceAfter.Revision != remoteRevision {
		t.Fatalf("Workspace revision after mixed conflict = %d, err=%v; want unchanged revision %d", workspaceAfter.Revision, err, remoteRevision)
	}
	manifest, err := store.GetWorkspaceManifestPage(ctx, workspaceValue.ID, remoteRevision, "project/demo", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range manifest.EmptyDirectories {
		if path == "project/demo/new" {
			t.Fatalf("nonconflicting directory was partially applied: %#v", manifest.EmptyDirectories)
		}
	}
	for _, directory := range manifest.Directories {
		if directory.Path == "project/demo" && (directory.Mode == nil || *directory.Mode != remoteMode || directory.ModTimeNS == nil || *directory.ModTimeNS != remoteTime) {
			t.Fatalf("remote conflicting directory metadata was overwritten: %#v", directory)
		}
	}
	var lastAppliedSession string
	if err := store.pool.QueryRow(ctx, `SELECT last_applied_session_id FROM workspace_sync_project_sources WHERE source_binding_id=$1`,
		first.SourceBindingID).Scan(&lastAppliedSession); err != nil || lastAppliedSession != first.ID {
		t.Fatalf("source baseline after mixed conflict = %q, err=%v; want baseline session %q", lastAppliedSession, err, first.ID)
	}
}

func TestProjectUploadConflictsWhenBindingVersionChanges(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "project-upload-binding-change")
	hostValue := workspaceSyncHostForFolderTest(t, ctx, store, owner.ID, workspaceValue.ID)
	portal := workspaceSyncPortalPrincipal(t, store, owner.ID)
	folderService, err := app.NewWorkspaceSyncDeviceService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := folderService.CreateFolder(ctx, portal, hostValue.ID, "demo", "project-upload-binding-change-folder")
	if err != nil {
		t.Fatal(err)
	}
	installationID := createTestClientInstallation(t, pool, owner.ID, nil)
	syncToken := clientTestToken(t, owner.ID, installationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, syncToken); err != nil {
		t.Fatal(err)
	}
	device := identity.Principal{
		UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindDevice, TokenID: syncToken.ID,
		ClientInstallationID: installationID, Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	syncService, err := app.NewWorkspaceSyncServiceWithOptions(store, workspaceTransferTestBlocks{}, workspace.MaxSyncManifestBytes, 1<<30,
		app.WorkspaceSyncOptions{ProjectUploadEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	planned, err := syncService.CreateSyncSession(ctx, device, workspaceValue.ID, workspace.CreateSyncSession{
		FolderID: folder.FolderID, SourceBindingID: "12345678-1234-4234-8234-123456789abc",
		Direction: workspace.SyncUpload, Policy: workspace.SyncCompareMerge, EmptyDirectories: []string{"owned"},
	}, "project-upload-binding-change-create")
	if err != nil {
		t.Fatalf("create project session for binding change: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_bindings SET binding_version=binding_version+1
WHERE host_id=$1 AND workspace_id=$2 AND state='active'`, hostValue.ID, workspaceValue.ID); err != nil {
		t.Fatalf("change active Host binding version: %v", err)
	}
	result, err := syncService.CommitSyncSession(ctx, device, workspaceValue.ID, planned.ID, "project-upload-binding-change-commit")
	if err != nil || result.State != workspace.SyncConflict {
		t.Fatalf("commit after Host binding change = %#v, err=%v; want conflict", result, err)
	}
	workspaceAfter, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || workspaceAfter.Revision != workspaceValue.Revision {
		t.Fatalf("Workspace revision after Host binding change = %d, err=%v; want unchanged %d", workspaceAfter.Revision, err, workspaceValue.Revision)
	}
	var sourceBaselineSession string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(last_applied_session_id,'') FROM workspace_sync_project_sources WHERE source_binding_id=$1`,
		planned.SourceBindingID).Scan(&sourceBaselineSession); err != nil || sourceBaselineSession != "" {
		t.Fatalf("source baseline after Host binding change = %q, err=%v; want unset", sourceBaselineSession, err)
	}
}

func confirmWorkspaceProjectApplicationForTest(t *testing.T, store *Store, session workspace.SyncSession, appliedRevision int64, expectedStatus string) {
	t.Helper()
	ctx := context.Background()
	var hostID string
	var bindingVersion, targetRevision, generation int64
	var manifestDigest []byte
	if err := store.pool.QueryRow(ctx, `SELECT host_id,workspace_binding_version,target_revision,manifest_sha256
FROM workspace_sync_project_upload_applications WHERE workspace_id=$1 AND session_id=$2 AND state='pending_apply'`,
		session.WorkspaceID, session.ID).Scan(&hostID, &bindingVersion, &targetRevision, &manifestDigest); err != nil {
		t.Fatalf("read pending test Host application receipt: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT runtime_generation FROM hosts WHERE id=$1`, hostID).Scan(&generation); err != nil {
		t.Fatalf("read test Host generation: %v", err)
	}
	// This fixture stands in for the preceding metadata-only Workspace-level applied receipt.
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_bindings
SET desired_revision=GREATEST(desired_revision,$4),applied_revision=$4,sync_state='healthy',sync_conflicts=ARRAY[]::text[],updated_at=now()
WHERE host_id=$1 AND workspace_id=$2 AND binding_version=$3 AND state='active'`,
		hostID, session.WorkspaceID, bindingVersion, appliedRevision); err != nil {
		t.Fatalf("advance test Host Workspace receipt: %v", err)
	}
	healthy := workspace.BindingSyncHealthy
	report := workspace.HostProjectUploadApplicationReport{
		SourceBindingID: session.SourceBindingID, TargetRevision: targetRevision, ManifestSHA256: hex.EncodeToString(manifestDigest),
		Status: "applied", AppliedRevision: &appliedRevision, SyncState: &healthy,
		Impacts: []workspace.SyncHostApplicationImpact{},
	}
	principal := identity.Principal{UserID: session.OwnerID, Kind: identity.KindHostReplica, HostScope: hostID,
		HostGeneration: generation, ReplicaWorkspaceID: session.WorkspaceID, BindingVersion: bindingVersion}
	application, err := store.RecordWorkspaceSyncProjectUploadApplication(ctx, principal, session.WorkspaceID,
		bindingVersion, session.ID, report)
	if err != nil || application.Status != expectedStatus || application.TargetRevision != targetRevision {
		t.Fatalf("confirm test Host application receipt = %#v, err=%v", application, err)
	}
	replayed, err := store.RecordWorkspaceSyncProjectUploadApplication(ctx, principal, session.WorkspaceID,
		bindingVersion, session.ID, report)
	if err != nil || replayed.Status != expectedStatus {
		t.Fatalf("replay identical test Host application receipt = %#v, err=%v", replayed, err)
	}
	conflicted := workspace.BindingSyncConflict
	if _, err := store.RecordWorkspaceSyncProjectUploadApplication(ctx, principal, session.WorkspaceID,
		bindingVersion, session.ID, workspace.HostProjectUploadApplicationReport{
			SourceBindingID: session.SourceBindingID, TargetRevision: targetRevision, ManifestSHA256: report.ManifestSHA256,
			Status: "unknown", SyncState: &conflicted,
			Impacts: []workspace.SyncHostApplicationImpact{},
		}); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("accept different receipt for applied session: %v; want conflict", err)
	}
}

func rejectWorkspaceProjectApplicationAfterBaselineMovesForTest(t *testing.T, store *Store, session workspace.SyncSession,
	baselineSessionID string, baselineTargetRevision int64, baselineManifestSHA256 string) {
	t.Helper()
	ctx := context.Background()
	if session.SourceBaselineSessionID != baselineSessionID {
		t.Fatalf("pending application baseline session = %q; want %q", session.SourceBaselineSessionID, baselineSessionID)
	}
	var hostID string
	var bindingVersion, targetRevision, generation int64
	var manifestDigest []byte
	if err := store.pool.QueryRow(ctx, `SELECT host_id,workspace_binding_version,target_revision,manifest_sha256
FROM workspace_sync_project_upload_applications WHERE workspace_id=$1 AND session_id=$2 AND state='pending_apply'`,
		session.WorkspaceID, session.ID).Scan(&hostID, &bindingVersion, &targetRevision, &manifestDigest); err != nil {
		t.Fatalf("read pending application before stale-baseline receipt: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT runtime_generation FROM hosts WHERE id=$1`, hostID).Scan(&generation); err != nil {
		t.Fatalf("read Host generation for stale-baseline receipt: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_sync_project_sources
SET last_applied_session_id=NULL,last_applied_target_revision=NULL,last_applied_manifest_sha256=NULL,updated_at=now()
WHERE source_binding_id=$1 AND last_applied_session_id=$2`, session.SourceBindingID, baselineSessionID); err != nil {
		t.Fatalf("move project source baseline before receipt: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_bindings
SET desired_revision=GREATEST(desired_revision,$4),applied_revision=$4,sync_state='healthy',sync_conflicts=ARRAY[]::text[],updated_at=now()
WHERE host_id=$1 AND workspace_id=$2 AND binding_version=$3 AND state='active'`,
		hostID, session.WorkspaceID, bindingVersion, targetRevision); err != nil {
		t.Fatalf("advance binding before stale-baseline receipt: %v", err)
	}
	healthy := workspace.BindingSyncHealthy
	report := workspace.HostProjectUploadApplicationReport{
		SourceBindingID: session.SourceBindingID, TargetRevision: targetRevision, ManifestSHA256: hex.EncodeToString(manifestDigest),
		Status: "applied", AppliedRevision: &targetRevision, SyncState: &healthy, Impacts: []workspace.SyncHostApplicationImpact{},
	}
	principal := identity.Principal{UserID: session.OwnerID, Kind: identity.KindHostReplica, HostScope: hostID,
		HostGeneration: generation, ReplicaWorkspaceID: session.WorkspaceID, BindingVersion: bindingVersion}
	if _, err := store.RecordWorkspaceSyncProjectUploadApplication(ctx, principal, session.WorkspaceID,
		bindingVersion, session.ID, report); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("accept receipt after source baseline moved: %v; want workspace conflict", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_sync_project_sources
SET last_applied_session_id=$2,last_applied_target_revision=$3,last_applied_manifest_sha256=$4,updated_at=now()
WHERE source_binding_id=$1`, session.SourceBindingID, baselineSessionID, baselineTargetRevision,
		mustDecodeSHA256ForTest(t, baselineManifestSHA256)); err != nil {
		t.Fatalf("restore project source baseline after stale-baseline receipt: %v", err)
	}
	var state string
	var receiptDigest []byte
	if err := store.pool.QueryRow(ctx, `SELECT state,receipt_sha256 FROM workspace_sync_project_upload_applications
WHERE workspace_id=$1 AND session_id=$2`, session.WorkspaceID, session.ID).Scan(&state, &receiptDigest); err != nil ||
		state != "pending_apply" || len(receiptDigest) != 0 {
		t.Fatalf("stale-baseline application state = %q, receipt=%x, err=%v; want pending with no receipt", state, receiptDigest, err)
	}
}

func mustDecodeSHA256ForTest(t *testing.T, value string) []byte {
	t.Helper()
	digest, err := hex.DecodeString(value)
	if err != nil || len(digest) != 32 {
		t.Fatalf("invalid SHA-256 in test fixture: %v", err)
	}
	return digest
}

func TestFirstProjectUploadConflictsWithExistingDestinationWithoutTransfer(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "project-upload-existing-destination")
	hostValue := workspaceSyncHostForFolderTest(t, ctx, store, owner.ID, workspaceValue.ID)
	portal := workspaceSyncPortalPrincipal(t, store, owner.ID)

	seedService, err := app.NewWorkspaceSyncService(store, workspaceTransferTestBlocks{}, workspace.MaxSyncManifestBytes, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := seedService.CreateSyncSession(ctx, portal, workspaceValue.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncUpload, Policy: workspace.SyncCompareMerge,
		EmptyDirectories: []string{"project/demo/existing"},
	}, "project-upload-existing-destination-seed")
	if err != nil {
		t.Fatalf("create metadata-only destination seed: %v", err)
	}
	if len(seed.TransferBlocks) != 0 {
		t.Fatalf("destination seed unexpectedly planned content blocks: %#v", seed.TransferBlocks)
	}
	if _, err := seedService.CommitSyncSession(ctx, portal, workspaceValue.ID, seed.ID, "project-upload-existing-destination-seed-commit"); err != nil {
		t.Fatalf("commit metadata-only destination seed: %v", err)
	}
	workspaceBefore, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil {
		t.Fatal(err)
	}

	folderService, err := app.NewWorkspaceSyncDeviceService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := folderService.CreateFolder(ctx, portal, hostValue.ID, "demo", "project-upload-existing-destination-folder")
	if err != nil {
		t.Fatalf("register project folder over existing destination: %v", err)
	}
	installationID := createTestClientInstallation(t, store.pool, owner.ID, nil)
	syncToken := clientTestToken(t, owner.ID, installationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, syncToken); err != nil {
		t.Fatal(err)
	}
	device := identity.Principal{
		UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindDevice, TokenID: syncToken.ID,
		ClientInstallationID: installationID, Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	uploadService, err := app.NewWorkspaceSyncServiceWithOptions(store, workspaceTransferTestBlocks{}, workspace.MaxSyncManifestBytes, 1<<30,
		app.WorkspaceSyncOptions{ProjectUploadEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	planned, err := uploadService.CreateSyncSession(ctx, device, workspaceValue.ID, workspace.CreateSyncSession{
		FolderID: folder.FolderID, SourceBindingID: "12345678-1234-4234-8234-123456789abc", Direction: workspace.SyncUpload, Policy: workspace.SyncCompareMerge,
	}, "project-upload-existing-destination-create")
	if err != nil {
		t.Fatalf("create first upload against existing destination: %v", err)
	}
	if planned.FolderBaselineRevision != nil || len(planned.TransferBlocks) != 0 || planned.PlannedBytes != 0 ||
		len(planned.Conflicts) != 1 || planned.Conflicts[0].Path != "project/demo/existing" || planned.Conflicts[0].Reason != "destination_exists" {
		t.Fatalf("first upload plan for existing destination = %#v; want metadata-only destination conflict", planned)
	}
	result, err := uploadService.CommitSyncSession(ctx, device, workspaceValue.ID, planned.ID, "project-upload-existing-destination-commit")
	if err != nil || result.State != workspace.SyncConflict {
		t.Fatalf("first upload result = %#v, err=%v; want conflict", result, err)
	}
	workspaceAfter, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || workspaceAfter.Revision != workspaceBefore.Revision {
		t.Fatalf("workspace revision after first-upload conflict = %d, err=%v; want unchanged revision %d", workspaceAfter.Revision, err, workspaceBefore.Revision)
	}
	folderAfter, err := store.GetWorkspaceSyncFolder(ctx, device, workspaceValue.ID, folder.FolderID)
	if err != nil || folderAfter.LastUploadRevision != nil {
		t.Fatalf("project baseline after first-upload conflict = %#v, err=%v; want unset", folderAfter.LastUploadRevision, err)
	}
}
