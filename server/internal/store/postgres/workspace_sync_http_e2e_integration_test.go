package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"morde-sync/client"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	mordesync "morde-sync"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/storage/workspacefs"
	"morde/server/internal/store/postgres/sqlcgen"
)

func TestWorkspaceSyncClientThroughHTTPAndPostgres(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "http-sync-e2e")
	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "blocks"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocks.Close() })
	faultableBlocks := &workspaceSyncFaultableBlocks{store: blocks}
	syncService, err := app.NewWorkspaceSyncService(store, faultableBlocks, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}

	deviceCredentials := make([]string, 2)
	deviceIDs := make([]string, 2)
	for i := range deviceCredentials {
		installationID := createTestClientInstallation(t, pool, owner.ID, []string{value.ID})
		tokenID, err := domain.NewTokenID()
		if err != nil {
			t.Fatal(err)
		}
		credential, secretHash, err := identity.NewCredential(identity.KindDevice, tokenID)
		if err != nil {
			t.Fatal(err)
		}
		deviceCredentials[i] = credential
		deviceIDs[i] = tokenID
		if _, err := store.CreateToken(ctx, identity.Token{
			ID: tokenID, OwnerID: owner.ID, ClientInstallationID: installationID, Kind: identity.KindDevice, Name: fmt.Sprintf("sync device %d", i),
			WorkspaceIDs: []string{value.ID}, Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
			SecretHash: secretHash, ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	auditService, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentialService, err := app.NewCredentialService(store, store, store, auditService, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 2, LoginWindow: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	router, err := api.New(api.Deps{
		Hosts:         unusedWorkspaceSyncHostService{},
		TLSAuthorizer: unusedWorkspaceSyncTLSAuthorizer{},
		Verifier:      credentialService,
		Auth:          unusedWorkspaceSyncAuth{},
		WorkspaceSync: syncService,
		Tasks:         unusedWorkspaceSyncTasks{},
		Conversations: unusedWorkspaceSyncConversations{},
		MCPDisabled:   true,
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 16 << 20,
		MCPMaxBodyBytes: 16 << 20, SessionTTL: time.Hour, HostScheme: "https",
		PortalDomain: "portal.example.test", HostSuffix: "example-hosts.test", PreviewPortMin: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(router)
	t.Cleanup(func() { httpServer.Close() })
	httpClient := httpServer.Client()
	dropper := &workspaceSyncResponseDropper{base: httpClient.Transport, putAttempts: make(map[string]int)}
	httpClient.Transport = dropper
	syncClient, err := client.New(httpServer.URL, deviceCredentials[0], httpClient)
	if err != nil {
		t.Fatal(err)
	}

	source := t.TempDir()
	writeWorkspaceSyncE2EFile(t, source, "src/main.go", []byte("package main\n"))
	remoteReadme := bytes.Repeat([]byte("r"), mordesync.BlockSize+128)
	writeWorkspaceSyncE2EFile(t, source, "README.md", remoteReadme)
	zeroRevision := int64(0)
	interrupted, err := syncClient.Sync(ctx, client.SyncOptions{
		WorkspaceID: value.ID, Directory: source, Direction: "upload", BaseRevision: &zeroRevision,
	})
	if err == nil || interrupted.Session.ID == "" {
		t.Fatalf("injected transfer interruption = %#v, %v", interrupted.Session, err)
	}
	resumedClient, err := client.New(httpServer.URL, deviceCredentials[0], httpClient)
	if err != nil {
		t.Fatal(err)
	}
	uploaded, err := resumedClient.Sync(ctx, client.SyncOptions{
		WorkspaceID: value.ID, Directory: source, SessionID: interrupted.Session.ID,
	})
	if err != nil || uploaded.Session.State != "completed" || uploaded.Session.TargetRevision != 1 {
		t.Fatalf("resumed upload through Portal API = %#v, %v", uploaded.Session, err)
	}
	dropper.mu.Lock()
	droppedDigest := dropper.droppedDigest
	droppedAttempts := dropper.putAttempts[droppedDigest]
	dropper.mu.Unlock()
	if droppedDigest == "" || droppedAttempts != 1 {
		t.Fatalf("durably acknowledged block was uploaded again: digest=%q attempts=%d", droppedDigest, droppedAttempts)
	}
	manifest, err := store.GetWorkspaceManifestPage(ctx, value.ID, 1, "", "", 100)
	if err != nil || len(manifest.Files) != 2 {
		t.Fatalf("persisted upload manifest = %#v, %v", manifest, err)
	}
	revisionOne := int64(1)
	zeroIncrement, err := syncClient.Sync(ctx, client.SyncOptions{
		WorkspaceID: value.ID, Directory: source, Direction: "upload", BaseRevision: &revisionOne,
	})
	if err != nil || zeroIncrement.Session.State != "completed" || zeroIncrement.Session.PlannedBytes != 0 || zeroIncrement.Session.TargetRevision != 1 {
		t.Fatalf("zero-increment upload = %#v, %v", zeroIncrement.Session, err)
	}
	unchangedWorkspace, err := store.GetWorkspace(ctx, value.ID)
	if err != nil || unchangedWorkspace.Revision != 1 {
		t.Fatalf("zero-increment upload advanced workspace revision to %d: %v", unchangedWorkspace.Revision, err)
	}

	destination := t.TempDir()
	localOnly := []byte("keep local edits\n")
	writeWorkspaceSyncE2EFile(t, destination, "src/main.go", localOnly)
	partialBlockSHA := sha256.Sum256(remoteReadme[mordesync.BlockSize:])
	dropper.mu.Lock()
	dropper.failPartialDownload = true
	dropper.partialDownloadDigest = hex.EncodeToString(partialBlockSHA[:])
	dropper.mu.Unlock()
	interruptedDownload, err := syncClient.Sync(ctx, client.SyncOptions{
		WorkspaceID: value.ID, Directory: destination, Direction: "download", BaseRevision: &revisionOne,
	})
	if err == nil || interruptedDownload.Session.ID == "" {
		t.Fatalf("injected download interruption = %#v, %v", interruptedDownload.Session, err)
	}
	dropper.mu.Lock()
	didFailPartialDownload := dropper.droppedDownload
	dropper.mu.Unlock()
	if !didFailPartialDownload {
		t.Fatal("the targeted second README block was not interrupted")
	}
	if _, err := os.Stat(filepath.Join(destination, "README.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted download left a partial file: %v", err)
	}
	stagedFiles, err := os.ReadDir(destination)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range stagedFiles {
		if strings.HasPrefix(entry.Name(), ".morde-sync-tmp-") {
			t.Fatalf("interrupted download left staging file %q", entry.Name())
		}
	}
	if data, err := os.ReadFile(filepath.Join(destination, "src/main.go")); err != nil || !bytes.Equal(data, localOnly) {
		t.Fatalf("interrupted download changed existing local file = %q, %v", data, err)
	}
	resumedDownloadClient, err := client.New(httpServer.URL, deviceCredentials[0], httpClient)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := resumedDownloadClient.Sync(ctx, client.SyncOptions{
		WorkspaceID: value.ID, Directory: destination, SessionID: interruptedDownload.Session.ID,
	})
	if err != nil || downloaded.Session.State != "completed" {
		t.Fatalf("resumed download through Portal API = %#v, %v", downloaded.Session, err)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "src/main.go")); err != nil || string(data) != string(localOnly) {
		t.Fatalf("existing different file = %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "README.md")); err != nil || !bytes.Equal(data, remoteReadme) {
		t.Fatalf("missing file was not completed: bytes=%d, %v", len(data), err)
	}

	secondClient, err := client.New(httpServer.URL, deviceCredentials[1], httpClient)
	if err != nil {
		t.Fatal(err)
	}
	liveOne := t.TempDir()
	liveRunOne := startWorkspaceSyncLive(t, "first device", syncClient, value.ID, liveOne)
	waitWorkspaceSyncWatch(t, liveRunOne)
	writeWorkspaceSyncE2EFile(t, liveOne, "new.txt", []byte("live from first device\n"))
	waitWorkspaceSyncReplicaCursor(t, ctx, store, value.ID, "device:"+deviceIDs[0], 2)
	waitWorkspaceSyncWatch(t, liveRunOne)
	liveTwo := t.TempDir()
	liveRunTwo := startWorkspaceSyncLive(t, "second device", secondClient, value.ID, liveTwo)
	waitWorkspaceSyncWatch(t, liveRunTwo)
	waitWorkspaceSyncFile(t, liveTwo, "new.txt", "live from first device\n", liveRunTwo)
	waitWorkspaceSyncReplicaCursor(t, ctx, store, value.ID, "device:"+deviceIDs[1], 2)
	writeWorkspaceSyncE2EFile(t, liveTwo, "README.md", []byte("live edit from second device\n"))
	waitWorkspaceSyncRevision(t, ctx, store, value.ID, 3, liveRunTwo)
	waitWorkspaceSyncFile(t, liveOne, "README.md", "live edit from second device\n", liveRunOne)
	waitWorkspaceSyncReplicaCursor(t, ctx, store, value.ID, "device:"+deviceIDs[0], 3)
	waitWorkspaceSyncReplicaCursor(t, ctx, store, value.ID, "device:"+deviceIDs[1], 3)
	latest, err := store.GetWorkspace(ctx, value.ID)
	if err != nil || latest.Revision < 3 {
		t.Fatalf("live sync workspace revision = %d, %v", latest.Revision, err)
	}
	if err := liveRunOne.stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("first live sync shutdown = %v", err)
	}
	if err := liveRunTwo.stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("second live sync shutdown = %v", err)
	}
	writeWorkspaceSyncE2EFile(t, source, "disk-full.txt", []byte("resume after storage recovery\n"))
	revisionThree := int64(3)
	faultableBlocks.failPut(syscall.ENOSPC)
	interruptedByFullStorage, err := syncClient.Sync(ctx, client.SyncOptions{
		WorkspaceID: value.ID, Directory: source, Direction: "upload", BaseRevision: &revisionThree,
	})
	var storageError *client.HTTPError
	if !errors.As(err, &storageError) || storageError.Status != http.StatusInternalServerError || interruptedByFullStorage.Session.ID == "" {
		t.Fatalf("full-storage upload = %#v, %v", interruptedByFullStorage.Session, err)
	}
	unchangedWorkspace, err = store.GetWorkspace(ctx, value.ID)
	if err != nil || unchangedWorkspace.Revision != 3 {
		t.Fatalf("storage error advanced workspace revision to %d: %v", unchangedWorkspace.Revision, err)
	}
	faultableBlocks.failPut(nil)
	recoveredClient, err := client.New(httpServer.URL, deviceCredentials[0], httpClient)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := recoveredClient.Sync(ctx, client.SyncOptions{
		WorkspaceID: value.ID, Directory: source, SessionID: interruptedByFullStorage.Session.ID,
	})
	if err != nil || recovered.Session.State != "completed" || recovered.Session.TargetRevision != 4 {
		t.Fatalf("storage-recovered upload = %#v, %v", recovered.Session, err)
	}
	writeWorkspaceSyncE2EFile(t, liveOne, "handoff-local.txt", []byte("local work before leaving\n"))
	handoffRevision, err := recoveredClient.HandoffCheckpoint(ctx, client.LiveSyncOptions{WorkspaceID: value.ID, Directory: liveOne})
	if err != nil || handoffRevision != 5 {
		t.Fatalf("local-to-cloud handoff checkpoint = revision %d, %v", handoffRevision, err)
	}
	assertWorkspaceSyncFile(t, liveOne, "disk-full.txt", "resume after storage recovery\n")
	assertWorkspaceSyncFile(t, liveOne, "handoff-local.txt", "local work before leaving\n")
	handoffManifest, err := store.GetWorkspaceManifestPage(ctx, value.ID, handoffRevision, "", "", 100)
	if err != nil || len(handoffManifest.Files) != 5 {
		t.Fatalf("local-to-cloud handoff manifest = %#v, %v", handoffManifest, err)
	}

	remoteWorkspace := t.TempDir()
	remoteDownload, err := secondClient.Sync(ctx, client.SyncOptions{
		WorkspaceID: value.ID, Directory: remoteWorkspace, Direction: "download", BaseRevision: &handoffRevision,
	})
	if err != nil || remoteDownload.Session.State != "completed" {
		t.Fatalf("download handoff checkpoint to second device = %#v, %v", remoteDownload.Session, err)
	}
	writeWorkspaceSyncE2EFile(t, remoteWorkspace, "src/main.go", []byte("package main\n// continued remotely\n"))
	remoteEdit, err := secondClient.Sync(ctx, client.SyncOptions{
		WorkspaceID: value.ID, Directory: remoteWorkspace, Direction: "upload", Policy: "compare_merge", BaseRevision: &handoffRevision,
	})
	if err != nil || remoteEdit.Session.State != "completed" || remoteEdit.Session.TargetRevision != handoffRevision+1 {
		t.Fatalf("remote-to-local handoff source update = %#v, %v", remoteEdit.Session, err)
	}
	returnedRevision, err := recoveredClient.HandoffCheckpoint(ctx, client.LiveSyncOptions{WorkspaceID: value.ID, Directory: liveOne})
	if err != nil || returnedRevision != handoffRevision+1 {
		t.Fatalf("cloud-to-local handoff checkpoint = revision %d, %v", returnedRevision, err)
	}
	assertWorkspaceSyncFile(t, liveOne, "src/main.go", "package main\n// continued remotely\n")
	if latest, err := store.GetWorkspace(ctx, value.ID); err != nil || latest.Revision != returnedRevision {
		t.Fatalf("handoff final workspace revision = %d, %v", latest.Revision, err)
	}
	if err := store.RevokeToken(ctx, deviceIDs[0], owner.ID); err != nil {
		t.Fatal(err)
	}
	_, err = syncClient.Sync(ctx, client.SyncOptions{
		WorkspaceID: value.ID, Directory: source, Direction: "upload", BaseRevision: &revisionThree,
	})
	var httpErr *client.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusUnauthorized {
		t.Fatalf("revoked device API access = %v, want HTTP 401", err)
	}
}

// TestWorkspaceSyncClientThroughHTTPAndPostgresENOSPCRetryAfterRecovery is an
// opt-in end-to-end check against a dedicated, small ext4 mount. It exercises
// the actual block store, HTTP API, and PostgreSQL acknowledgement state.
func TestWorkspaceSyncClientThroughHTTPAndPostgresENOSPCRetryAfterRecovery(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ext4 ENOSPC integration test is Linux-only")
	}
	if strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL")) == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; PostgreSQL ENOSPC integration test was not run")
	}
	volumeRoot := strings.TrimSpace(os.Getenv("WORKSPACEFS_ENOSPC_DIR"))
	if volumeRoot == "" {
		t.Skip("set WORKSPACEFS_ENOSPC_DIR to a dedicated ext4 mount")
	}
	volumeRoot, err := filepath.Abs(volumeRoot)
	if err != nil || filepath.Clean(volumeRoot) == string(filepath.Separator) {
		t.Fatalf("WORKSPACEFS_ENOSPC_DIR must name a dedicated ext4 mount, got %q", volumeRoot)
	}
	volumeInfo, err := os.Lstat(volumeRoot)
	if err != nil || !volumeInfo.IsDir() || volumeInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("WORKSPACEFS_ENOSPC_DIR must be a real mount directory: %q", volumeRoot)
	}
	mountInfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("read mount table: %v", err)
	}
	volumeDevice, volumeFilesystem, volumeMounted := workspaceSyncENOSPCMountInfo(string(mountInfo), volumeRoot)
	rootDevice, _, rootMounted := workspaceSyncENOSPCMountInfo(string(mountInfo), string(filepath.Separator))
	if !volumeMounted || !rootMounted || volumeDevice == rootDevice {
		t.Fatalf("WORKSPACEFS_ENOSPC_DIR must be a separate mount from the system root: %q", volumeRoot)
	}
	if volumeFilesystem != "ext4" {
		t.Fatalf("WORKSPACEFS_ENOSPC_DIR filesystem = %q, want ext4", volumeFilesystem)
	}
	var initial syscall.Statfs_t
	if err := syscall.Statfs(volumeRoot, &initial); err != nil {
		t.Fatalf("stat ENOSPC test volume: %v", err)
	}
	const (
		ext4SuperMagic        = 0xef53
		enospcFreeSpaceBytes  = 2 << 20
		enospcTestVolumeLimit = 256 << 20
	)
	if uint64(initial.Type) != ext4SuperMagic {
		t.Fatalf("ENOSPC test volume type = %#x, want ext4 %#x", uint64(initial.Type), ext4SuperMagic)
	}
	if initial.Bsize <= 0 || uint64(initial.Blocks) > uint64(enospcTestVolumeLimit)/uint64(initial.Bsize) {
		t.Fatalf("ENOSPC test volume must be at most %d bytes", enospcTestVolumeLimit)
	}
	canUseReservedBlocks := os.Geteuid() == 0
	freeBytes := workspaceSyncENOSPCFreeBytes(initial, canUseReservedBlocks)
	if freeBytes <= enospcFreeSpaceBytes+workspacefs.MaxBlockBytes {
		t.Fatalf("ext4 test volume has only %d available bytes before setup", freeBytes)
	}

	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "http-sync-enospc-e2e")
	scratch, err := os.MkdirTemp(volumeRoot, "workspace-sync-enospc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(scratch); err != nil {
			t.Errorf("remove ENOSPC test directory: %v", err)
		}
	})
	blocks, err := workspacefs.Open(filepath.Join(scratch, "blocks"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close ENOSPC test block store: %v", err)
		}
	})
	if err := os.Mkdir(filepath.Join(scratch, "blocks", value.ID), 0700); err != nil {
		t.Fatalf("prepare workspace block directory before filling volume: %v", err)
	}
	var beforeFill syscall.Statfs_t
	if err := syscall.Statfs(volumeRoot, &beforeFill); err != nil {
		t.Fatalf("stat ENOSPC test volume before fill: %v", err)
	}
	freeBytes = workspaceSyncENOSPCFreeBytes(beforeFill, canUseReservedBlocks)
	if freeBytes <= enospcFreeSpaceBytes+workspacefs.MaxBlockBytes {
		t.Fatalf("ext4 test volume has only %d available bytes after setup", freeBytes)
	}

	tokenID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	credential, secretHash, err := identity.NewCredential(identity.KindDevice, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	installationID := createTestClientInstallation(t, pool, owner.ID, []string{value.ID})
	if _, err := store.CreateToken(ctx, identity.Token{
		ID: tokenID, OwnerID: owner.ID, ClientInstallationID: installationID, Kind: identity.KindDevice, Name: "ext4 ENOSPC test device",
		WorkspaceIDs: []string{value.ID}, Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
		SecretHash: secretHash, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	auditService, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentialService, err := app.NewCredentialService(store, store, store, auditService, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 2, LoginWindow: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	syncService, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	router, err := api.New(api.Deps{
		Hosts:         unusedWorkspaceSyncHostService{},
		TLSAuthorizer: unusedWorkspaceSyncTLSAuthorizer{},
		Verifier:      credentialService,
		Auth:          unusedWorkspaceSyncAuth{},
		WorkspaceSync: syncService,
		Tasks:         unusedWorkspaceSyncTasks{},
		Conversations: unusedWorkspaceSyncConversations{},
		MCPDisabled:   true,
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 16 << 20,
		MCPMaxBodyBytes: 16 << 20, SessionTTL: time.Hour, HostScheme: "https",
		PortalDomain: "portal.example.test", HostSuffix: "example-hosts.test", PreviewPortMin: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(router)
	t.Cleanup(httpServer.Close)
	httpClient := httpServer.Client()
	syncClient, err := client.New(httpServer.URL, credential, httpClient)
	if err != nil {
		t.Fatal(err)
	}

	data := bytes.Repeat([]byte{'x'}, int(workspacefs.MaxBlockBytes))
	expectedDigestBytes := sha256.Sum256(data)
	expectedDigest := hex.EncodeToString(expectedDigestBytes[:])
	source := t.TempDir()
	writeWorkspaceSyncE2EFile(t, source, "payload.bin", data)

	fillerPath := filepath.Join(scratch, "filler")
	filler, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filler.Close()
		if err := os.Remove(fillerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove ENOSPC filler: %v", err)
		}
	})
	var beforeFilling syscall.Statfs_t
	if err := syscall.Statfs(volumeRoot, &beforeFilling); err != nil {
		t.Fatalf("stat ENOSPC test volume before filler write: %v", err)
	}
	freeBytes = workspaceSyncENOSPCFreeBytes(beforeFilling, canUseReservedBlocks)
	if freeBytes <= enospcFreeSpaceBytes+workspacefs.MaxBlockBytes {
		t.Fatalf("ext4 test volume has only %d available bytes before filler write", freeBytes)
	}
	targetBytes := freeBytes - enospcFreeSpaceBytes
	zeroes := make([]byte, 1<<20)
	var filled int64
	for filled < targetBytes {
		want := int64(len(zeroes))
		if remaining := targetBytes - filled; remaining < want {
			want = remaining
		}
		n, writeErr := filler.Write(zeroes[:want])
		filled += int64(n)
		if writeErr != nil {
			if !errors.Is(writeErr, syscall.ENOSPC) {
				t.Fatalf("fill ext4 test volume: %v", writeErr)
			}
			break
		}
		if int64(n) != want {
			t.Fatalf("short write filling ext4 test volume: wrote %d of %d", n, want)
		}
	}
	if err := filler.Sync(); err != nil && !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("sync ext4 filler: %v", err)
	}
	if err := filler.Close(); err != nil && !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("close ext4 filler: %v", err)
	}
	var full syscall.Statfs_t
	if err := syscall.Statfs(volumeRoot, &full); err != nil {
		t.Fatalf("stat full ext4 test volume: %v", err)
	}
	if available := workspaceSyncENOSPCFreeBytes(full, canUseReservedBlocks); available >= workspacefs.MaxBlockBytes {
		t.Fatalf("ext4 test volume still has %d bytes; expected less than one %d-byte block", available, workspacefs.MaxBlockBytes)
	}

	zeroRevision := int64(0)
	interrupted, err := syncClient.Sync(ctx, client.SyncOptions{
		WorkspaceID: value.ID, Directory: source, Direction: "upload", BaseRevision: &zeroRevision,
	})
	var storageError *client.HTTPError
	if !errors.As(err, &storageError) || storageError.Status != http.StatusInternalServerError || interrupted.Session.ID == "" {
		t.Fatalf("full ext4 upload through HTTP = %#v, %v", interrupted.Session, err)
	}
	sessionID := interrupted.Session.ID
	session, err := store.GetWorkspaceSyncSession(ctx, value.ID, sessionID)
	if err != nil || session.ConfirmedBytes != 0 || string(session.State) == "completed" {
		t.Fatalf("PostgreSQL session after real ext4 ENOSPC = %#v, %v", session, err)
	}
	transfers, err := store.ListWorkspaceSyncTransferBlocks(ctx, value.ID, sessionID)
	if err != nil || len(transfers) != 1 || transfers[0].Received || transfers[0].SHA256 != expectedDigest || transfers[0].SizeBytes != workspacefs.MaxBlockBytes {
		t.Fatalf("PostgreSQL transfer acknowledgement after real ext4 ENOSPC = %#v, %v", transfers, err)
	}
	unchanged, err := store.GetWorkspace(ctx, value.ID)
	if err != nil || unchanged.Revision != 0 {
		t.Fatalf("real ext4 ENOSPC advanced workspace revision to %d: %v", unchanged.Revision, err)
	}
	blockEntries, err := os.ReadDir(filepath.Join(scratch, "blocks", value.ID))
	if err != nil || len(blockEntries) != 0 {
		t.Fatalf("workspace block directory after real ext4 ENOSPC contains %#v: %v", blockEntries, err)
	}
	commitRequest, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/workspaces/%s/sync-sessions/%s/commit", httpServer.URL, value.ID, sessionID), nil)
	if err != nil {
		t.Fatal(err)
	}
	commitRequest.Header.Set("Authorization", "Bearer "+credential)
	commitRequest.Header.Set("Idempotency-Key", "enospc-commit-before-recovery")
	commitResponse, err := httpClient.Do(commitRequest)
	if err != nil {
		t.Fatalf("commit incomplete ENOSPC session: %v", err)
	}
	_ = commitResponse.Body.Close()
	if commitResponse.StatusCode != http.StatusConflict {
		t.Fatalf("commit incomplete ENOSPC session status = %d, want %d", commitResponse.StatusCode, http.StatusConflict)
	}
	unchanged, err = store.GetWorkspace(ctx, value.ID)
	if err != nil || unchanged.Revision != 0 {
		t.Fatalf("blocked commit advanced workspace revision to %d: %v", unchanged.Revision, err)
	}
	session, err = store.GetWorkspaceSyncSession(ctx, value.ID, sessionID)
	if err != nil || session.TargetRevision != 0 || string(session.State) == "completed" {
		t.Fatalf("incomplete session changed after blocked commit = %#v, %v", session, err)
	}

	if err := os.Remove(fillerPath); err != nil {
		t.Fatalf("free ext4 test volume: %v", err)
	}
	scratchDir, err := os.Open(scratch)
	if err != nil {
		t.Fatalf("open ext4 test directory after freeing space: %v", err)
	}
	if err := scratchDir.Sync(); err != nil {
		_ = scratchDir.Close()
		t.Fatalf("sync ext4 test directory after freeing space: %v", err)
	}
	if err := scratchDir.Close(); err != nil {
		t.Fatalf("close ext4 test directory: %v", err)
	}
	recoveredClient, err := client.New(httpServer.URL, credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := recoveredClient.Sync(ctx, client.SyncOptions{
		WorkspaceID: value.ID, Directory: source, SessionID: sessionID,
	})
	if err != nil || recovered.Session.ID != sessionID || recovered.Session.State != "completed" || recovered.Session.TargetRevision != 1 {
		t.Fatalf("space-recovered upload through new client = %#v, %v", recovered.Session, err)
	}
	session, err = store.GetWorkspaceSyncSession(ctx, value.ID, sessionID)
	if err != nil || session.State != "completed" || session.TargetRevision != 1 || session.ConfirmedBytes != workspacefs.MaxBlockBytes {
		t.Fatalf("PostgreSQL recovered session = %#v, %v", session, err)
	}
	transfers, err = store.ListWorkspaceSyncTransferBlocks(ctx, value.ID, sessionID)
	if err != nil || len(transfers) != 1 || !transfers[0].Received || transfers[0].SHA256 != expectedDigest {
		t.Fatalf("PostgreSQL recovered transfer block = %#v, %v", transfers, err)
	}
	unchanged, err = store.GetWorkspace(ctx, value.ID)
	if err != nil || unchanged.Revision != 1 {
		t.Fatalf("recovered upload workspace revision = %d: %v", unchanged.Revision, err)
	}
	if err := blocks.VerifyBlock(value.ID, expectedDigest, workspacefs.MaxBlockBytes); err != nil {
		t.Fatalf("verify recovered ext4 block digest: %v", err)
	}
}

func workspaceSyncENOSPCMountInfo(mountInfo, mountPoint string) (device, filesystem string, ok bool) {
	encoded := strings.NewReplacer("\\", "\\134", "\t", "\\011", "\n", "\\012", " ", "\\040").Replace(filepath.Clean(mountPoint))
	var matchedDevice, matchedFilesystem string
	matched := false
	for _, line := range strings.Split(mountInfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || fields[4] != encoded {
			continue
		}
		for i := 6; i+2 < len(fields); i++ {
			if fields[i] == "-" {
				if matched {
					return "", "", false
				}
				matchedDevice, matchedFilesystem, matched = fields[2], fields[i+1], true
				break
			}
		}
	}
	return matchedDevice, matchedFilesystem, matched
}

func workspaceSyncENOSPCFreeBytes(stat syscall.Statfs_t, canUseReservedBlocks bool) int64 {
	freeBlocks := stat.Bavail
	if canUseReservedBlocks {
		freeBlocks = stat.Bfree
	}
	return int64(freeBlocks) * int64(stat.Bsize)
}

func TestWorkspaceSyncENOSPCMountInfoMatchesExactMountAndFilesystem(t *testing.T) {
	mountInfo := "22 1 0:41 / / rw - overlay overlay rw\n23 22 7:0 / /tmp/ext4\\040test rw - ext4 /dev/loop0 rw\n"
	if device, filesystem, ok := workspaceSyncENOSPCMountInfo(mountInfo, "/tmp/ext4 test"); !ok || device != "7:0" || filesystem != "ext4" {
		t.Fatalf("workspaceSyncENOSPCMountInfo() = (%q, %q, %t), want (7:0, ext4, true)", device, filesystem, ok)
	}
	if _, _, ok := workspaceSyncENOSPCMountInfo(mountInfo, "/tmp/ext4"); ok {
		t.Fatal("workspaceSyncENOSPCMountInfo matched a mount path prefix")
	}
	stackedMountInfo := mountInfo + "24 23 8:1 / /tmp/ext4\\040test rw - ext4 /dev/loop1 rw\n"
	if _, _, ok := workspaceSyncENOSPCMountInfo(stackedMountInfo, "/tmp/ext4 test"); ok {
		t.Fatal("workspaceSyncENOSPCMountInfo accepted an ambiguous stacked mount")
	}
	stat := syscall.Statfs_t{Bavail: 2, Bfree: 5, Bsize: 4096}
	if got := workspaceSyncENOSPCFreeBytes(stat, false); got != 2*4096 {
		t.Fatalf("unprivileged ext4 free bytes = %d, want %d", got, 2*4096)
	}
	if got := workspaceSyncENOSPCFreeBytes(stat, true); got != 5*4096 {
		t.Fatalf("root ext4 free bytes = %d, want %d", got, 5*4096)
	}
}

type workspaceSyncResponseDropper struct {
	base                  http.RoundTripper
	mu                    sync.Mutex
	dropped               bool
	droppedDigest         string
	putAttempts           map[string]int
	failPartialDownload   bool
	partialDownloadDigest string
	droppedDownload       bool
}

type workspaceSyncFaultableBlocks struct {
	store *workspacefs.Store
	mu    sync.RWMutex
	err   error
}

func (s *workspaceSyncFaultableBlocks) failPut(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

func (s *workspaceSyncFaultableBlocks) PutBlock(workspaceID, digest string, source io.Reader) (int64, error) {
	s.mu.RLock()
	err := s.err
	s.mu.RUnlock()
	if err != nil {
		return 0, err
	}
	return s.store.PutBlock(workspaceID, digest, source)
}

func (s *workspaceSyncFaultableBlocks) Open(workspaceID, digest string) (io.ReadCloser, error) {
	return s.store.Open(workspaceID, digest)
}

func (s *workspaceSyncFaultableBlocks) ReservationBytes(size int64) (int64, error) {
	return s.store.ReservationBytes(size)
}

func (s *workspaceSyncFaultableBlocks) BlockAllocatedBytes(workspaceID, digest string) (int64, error) {
	return s.store.BlockAllocatedBytes(workspaceID, digest)
}

func (d *workspaceSyncResponseDropper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := d.base.RoundTrip(request)
	if err != nil || (request.Method != http.MethodPut && request.Method != http.MethodGet) || !strings.Contains(request.URL.Path, "/blocks/") {
		return response, err
	}
	d.mu.Lock()
	drop := false
	partialDownload := false
	if request.Method == http.MethodPut {
		digest := request.URL.Path[strings.LastIndex(request.URL.Path, "/")+1:]
		d.putAttempts[digest]++
		drop = !d.dropped
		if drop {
			d.dropped = true
			d.droppedDigest = digest
		}
	} else if request.Method == http.MethodGet && d.failPartialDownload && !d.droppedDownload {
		digest := request.URL.Path[strings.LastIndex(request.URL.Path, "/")+1:]
		partialDownload = digest == d.partialDownloadDigest
		d.droppedDownload = partialDownload
	}
	d.mu.Unlock()
	if partialDownload {
		response.Body = &workspaceSyncPartialReadError{ReadCloser: response.Body, remaining: 64}
		return response, nil
	}
	if !drop {
		return response, nil
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	return nil, errors.New("injected block response loss")
}

type workspaceSyncPartialReadError struct {
	io.ReadCloser
	remaining int
}

func (r *workspaceSyncPartialReadError) Read(data []byte) (int, error) {
	if r.remaining == 0 {
		return 0, errors.New("injected partial block body failure")
	}
	if len(data) > r.remaining {
		data = data[:r.remaining]
	}
	n, err := r.ReadCloser.Read(data)
	r.remaining -= n
	if r.remaining == 0 && err == nil {
		return n, errors.New("injected partial block body failure")
	}
	return n, err
}

func writeWorkspaceSyncE2EFile(t *testing.T, root, name string, data []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertWorkspaceSyncFile(t *testing.T, root, name, expected string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil || string(data) != expected {
		t.Fatalf("workspace file %q = %q, %v; want %q", name, data, err, expected)
	}
}

type unusedWorkspaceSyncHostService struct{ app.HostService }
type unusedWorkspaceSyncTLSAuthorizer struct{ app.HostTLSAuthorizer }
type unusedWorkspaceSyncAuth struct{ api.AuthService }
type unusedWorkspaceSyncTasks struct{ api.TaskService }
type unusedWorkspaceSyncConversations struct{ api.ConversationService }

type workspaceSyncLiveRun struct {
	name       string
	watching   chan struct{}
	finished   chan struct{}
	err        error
	resultMu   sync.Mutex
	lastResult client.Result
	stopOnce   sync.Once
	stopError  error
	stop       func() error
}

func startWorkspaceSyncLive(t *testing.T, name string, syncClient *client.Client, workspaceID, directory string) *workspaceSyncLiveRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	run := &workspaceSyncLiveRun{name: name, watching: make(chan struct{}, 1), finished: make(chan struct{})}
	go func() {
		run.err = syncClient.LiveSync(ctx, client.LiveSyncOptions{WorkspaceID: workspaceID, Directory: directory, RescanInterval: 5 * time.Second,
			OnResult: func(result client.Result) {
				run.resultMu.Lock()
				run.lastResult = result
				run.resultMu.Unlock()
			},
			Progress: func(progress client.Progress) {
				if progress.Stage == "watch" {
					select {
					case run.watching <- struct{}{}:
					default:
					}
				}
			}})
		close(run.finished)
	}()
	run.stop = func() error {
		run.stopOnce.Do(func() {
			cancel()
			<-run.finished
			run.stopError = run.err
		})
		return run.stopError
	}
	t.Cleanup(func() { _ = run.stop() })
	return run
}

func (run *workspaceSyncLiveRun) resultSummary() string {
	run.resultMu.Lock()
	defer run.resultMu.Unlock()
	conflicts := make([]string, 0, len(run.lastResult.Conflicts))
	for _, conflict := range run.lastResult.Conflicts {
		conflicts = append(conflicts, fmt.Sprintf("%s:%s local=%v remote=%v", conflict.Path, conflict.Reason, conflict.LocalSHA256, conflict.RemoteSHA256))
	}
	return fmt.Sprintf("session=%s state=%s direction=%s policy=%s base=%d target=%d conflicts=%v",
		run.lastResult.Session.ID, run.lastResult.Session.State, run.lastResult.Session.Direction, run.lastResult.Session.Policy,
		run.lastResult.Session.BaseRevision, run.lastResult.Session.TargetRevision, conflicts)
}

func waitWorkspaceSyncWatch(t *testing.T, run *workspaceSyncLiveRun) {
	t.Helper()
	select {
	case <-run.watching:
	case <-run.finished:
		t.Fatalf("live sync %s stopped before watch state: %v (%s)", run.name, run.err, run.resultSummary())
	case <-time.After(15 * time.Second):
		t.Fatal("live sync did not reach watch state")
	}
}

func waitWorkspaceSyncFile(t *testing.T, root, name, expected string, runs ...*workspaceSyncLiveRun) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, run := range runs {
			select {
			case <-run.finished:
				data, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
				t.Fatalf("live sync %s stopped before applying %q: %v (%s); local=%q read_error=%v", run.name, name, run.err, run.resultSummary(), data, readErr)
			default:
			}
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err == nil && string(data) == expected {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, run := range runs {
		t.Logf("live sync %s last result: %s", run.name, run.resultSummary())
	}
	t.Fatalf("live sync did not apply %q with expected content", name)
}

func waitWorkspaceSyncReplicaCursor(t *testing.T, ctx context.Context, store *Store, workspaceID, replicaID string, revision int64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		cursor, err := store.q.GetWorkspaceSyncCursor(ctx, sqlcgen.GetWorkspaceSyncCursorParams{WorkspaceID: workspaceID, ReplicaID: replicaID})
		if err == nil && cursor.Revision >= revision {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("replica %q did not acknowledge workspace revision %d", replicaID, revision)
}

func waitWorkspaceSyncRevision(t *testing.T, ctx context.Context, store *Store, workspaceID string, revision int64, runs ...*workspaceSyncLiveRun) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, run := range runs {
			select {
			case <-run.finished:
				t.Fatalf("live sync %s stopped before committing workspace revision %d: %v (%s)", run.name, revision, run.err, run.resultSummary())
			default:
			}
		}
		value, err := store.GetWorkspace(ctx, workspaceID)
		if err == nil && value.Revision >= revision {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("workspace did not reach revision %d", revision)
}
