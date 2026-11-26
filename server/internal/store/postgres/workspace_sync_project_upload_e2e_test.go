package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	mordesync "morde-sync"
	"morde-sync/client"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacecurrent"
	"morde/server/internal/storage/workspacefs"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

type localProjectUploadTestTransport struct {
	base http.RoundTripper
	t    *testing.T
}

func (transport localProjectUploadTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err == nil && response.StatusCode >= 400 {
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(body))
		transport.t.Logf("synthetic test server response: status=%d body=%s read_error=%v", response.StatusCode, body, readErr)
	}
	return response, err
}

// This test uses only an in-process HTTP server, a test database and temporary directories.
// Linux is required by the production Workspace directory's safe openat2 operations.
func TestManualProjectUploadLocalEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the production Workspace directory requires Linux openat2")
	}
	ctx := context.Background()
	store, pool := newTemporaryStore(t)
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value, _, err := store.CreateAgentWorkspace(ctx, newAgentWorkspaceCandidate(t, owner.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAgentWorkspaceDirectoryInitialized(ctx, owner.ID, value.ID); err != nil {
		t.Fatal(err)
	}
	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "blocks"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocks.Close() })
	currentRoot := filepath.Join(t.TempDir(), "current")
	current, err := workspacecurrent.Open(currentRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = current.Close() })
	if err := current.Materialize(ctx, owner.ID, value.ID, 0, nil, nil,
		[]workspace.Directory{{Path: ".", MetadataKnown: true}}, blocks); err != nil {
		t.Fatal(err)
	}
	ownerDigest := sha256.Sum256([]byte(owner.ID))
	cloudProject := filepath.Join(currentRoot, hex.EncodeToString(ownerDigest[:]), value.ID, "project", "demo")
	if err := os.MkdirAll(cloudProject, 0o750); err != nil {
		t.Fatal(err)
	}
	host := workspaceSyncHostForFolderTest(t, ctx, store, owner.ID, value.ID)
	folders, err := app.NewWorkspaceSyncDeviceService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := folders.CreateFolder(ctx, workspaceSyncPortalPrincipal(t, store, owner.ID), host.ID, "demo", "local-e2e-project-folder")
	if err != nil {
		t.Fatal(err)
	}
	installationID := createTestClientInstallation(t, pool, owner.ID, nil)
	tokenID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	credential, secretHash, err := identity.NewCredential(identity.KindDevice, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateToken(ctx, identity.Token{ID: tokenID, OwnerID: owner.ID, ClientInstallationID: installationID,
		Kind: identity.KindDevice, Name: "local synthetic project sync", Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
		SecretHash: secretHash, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	syncService, err := app.NewWorkspaceSyncServiceWithOptions(store, blocks, 0, 1<<40,
		app.WorkspaceSyncOptions{ProjectUploadEnabled: true, LiveDirectorySourceEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := syncService.SetWorkspaceCurrentDirectory(current); err != nil {
		t.Fatal(err)
	}
	auditService, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := app.NewCredentialService(store, store, store, auditService, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 2, LoginWindow: time.Minute,
	}, credentialDefaultHostNoop{})
	if err != nil {
		t.Fatal(err)
	}
	router, err := api.New(api.Deps{Hosts: unusedWorkspaceSyncHostService{}, TLSAuthorizer: unusedWorkspaceSyncTLSAuthorizer{},
		Verifier: verifier, Auth: unusedWorkspaceSyncAuth{}, WorkspaceSync: syncService, MCPDisabled: true}, api.Options{
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
	httpClient.Transport = localProjectUploadTestTransport{base: httpClient.Transport, t: t}
	syncClient, err := client.New(httpServer.URL, credential, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	writeWorkspaceSyncE2EFile(t, source, "nested/keep.txt", []byte("initial"))
	writeWorkspaceSyncE2EFile(t, source, "old/deep/remove.txt", []byte("delete later"))
	writeWorkspaceSyncE2EFile(t, source, "update.txt", []byte("before"))
	baseRevision := int64(0)
	syncOptions := client.SyncOptions{WorkspaceID: value.ID, FolderID: folder.FolderID,
		SourceBindingID: "12345678-1234-4234-8234-123456789abc", Directory: source,
		Direction: "upload", Policy: "compare_merge", BaseRevision: &baseRevision, Scope: []string{}}
	syncNow := func() client.Result {
		t.Helper()
		result, err := syncClient.Sync(ctx, syncOptions)
		if err != nil {
			t.Fatalf("explicit project sync: %v", err)
		}
		if result.Session.Direction != "upload" || len(result.Session.DownloadActions) != 0 {
			t.Fatalf("project sync unexpectedly requested download actions: %#v", result.Session)
		}
		return result
	}
	assertFile := func(root, path, expected string) {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil || string(content) != expected {
			t.Fatalf("file %s = %q, err %v; want %q", path, content, err, expected)
		}
	}
	assertMissing := func(root, path string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("file %s should be absent: %v", path, err)
		}
	}
	first := syncNow()
	if first.Session.State != "completed" {
		t.Fatalf("first upload = %#v", first)
	}
	localManifest, err := mordesync.Scan(ctx, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	localFingerprint, err := mordesync.ManifestFingerprint(localManifest)
	if err != nil || first.SnapshotFingerprint != localFingerprint {
		t.Fatalf("uploaded snapshot fingerprint does not match the local read-only scan: %v", err)
	}
	firstStored, err := store.GetWorkspaceSyncSession(ctx, value.ID, first.Session.ID)
	if err != nil || firstStored.HostApplication == nil || firstStored.HostApplication.Status != "applied" {
		t.Fatalf("first host receipt = %#v, err %v", firstStored.HostApplication, err)
	}
	assertFile(cloudProject, "nested/keep.txt", "initial")
	assertFile(cloudProject, "old/deep/remove.txt", "delete later")
	assertFile(cloudProject, "update.txt", "before")
	if info, err := os.Stat(cloudProject); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("registered project root mode was changed: info=%v err=%v", info, err)
	}

	writeWorkspaceSyncE2EFile(t, source, "update.txt", []byte("after"))
	initialCloudInfo, err := os.Stat(filepath.Join(cloudProject, "update.txt"))
	if err != nil {
		t.Fatal(err)
	}
	changedDuringTransfer := false
	syncOptions.Progress = func(progress client.Progress) {
		if progress.SessionID == "" || changedDuringTransfer {
			return
		}
		changedDuringTransfer = true
		if err := os.Chtimes(filepath.Join(cloudProject, "update.txt"), time.Now(), initialCloudInfo.ModTime().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	raced := syncNow()
	syncOptions.Progress = nil
	if !changedDuringTransfer || raced.Session.State != "conflict" {
		t.Fatalf("cloud change after planning must stop commit: changed=%v result=%#v", changedDuringTransfer, raced)
	}
	assertFile(cloudProject, "update.txt", "before")
	if err := os.Chtimes(filepath.Join(cloudProject, "update.txt"), time.Now(), initialCloudInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceSyncE2EFile(t, source, "new/added.txt", []byte("added"))
	if err := os.RemoveAll(filepath.Join(source, "old")); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceSyncE2EFile(t, cloudProject, "cloud-only.txt", []byte("cloud"))
	time.Sleep(100 * time.Millisecond)
	assertFile(cloudProject, "update.txt", "before")
	assertFile(cloudProject, "old/deep/remove.txt", "delete later")
	assertMissing(cloudProject, "new/added.txt")
	assertMissing(source, "cloud-only.txt")

	second := syncNow()
	if second.Session.State != "completed" {
		t.Fatalf("second upload = %#v", second)
	}
	assertFile(cloudProject, "update.txt", "after")
	assertFile(cloudProject, "new/added.txt", "added")
	assertFile(cloudProject, "cloud-only.txt", "cloud")
	assertMissing(cloudProject, "old")
	assertMissing(source, "cloud-only.txt")
	if info, err := os.Stat(cloudProject); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("registered project root mode was changed on retry: info=%v err=%v", info, err)
	}
	emptyFolder, err := folders.CreateFolder(ctx, workspaceSyncPortalPrincipal(t, store, owner.ID), host.ID, "empty", "local-e2e-empty-folder")
	if err != nil {
		t.Fatal(err)
	}
	emptyResult, err := syncClient.Sync(ctx, client.SyncOptions{WorkspaceID: value.ID, FolderID: emptyFolder.FolderID,
		SourceBindingID: "12345678-1234-4234-8234-123456789abd", Directory: t.TempDir(),
		Direction: "upload", Policy: "compare_merge", BaseRevision: &baseRevision, Scope: []string{}})
	if err != nil || emptyResult.Session.State != "completed" {
		t.Fatalf("empty project upload = %#v, err=%v", emptyResult, err)
	}
	if info, err := os.Stat(filepath.Join(filepath.Dir(cloudProject), "empty")); err != nil || !info.IsDir() {
		t.Fatalf("empty project root not created: info=%v err=%v", info, err)
	}

	if err := os.Remove(filepath.Join(source, "nested", "keep.txt")); err != nil {
		t.Fatal(err)
	}
	cloudFile := filepath.Join(cloudProject, "nested", "keep.txt")
	cloudInfo, err := os.Stat(cloudFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(cloudFile, time.Now(), cloudInfo.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	timeConflict := syncNow()
	if timeConflict.Session.State != "conflict" || len(timeConflict.Conflicts) == 0 {
		t.Fatalf("local deletion versus cloud timestamp edit must conflict: %#v", timeConflict)
	}
	assertFile(cloudProject, "nested/keep.txt", "initial")
	if err := os.Chtimes(cloudFile, time.Now(), cloudInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cloudFile, 0o600); err != nil {
		t.Fatal(err)
	}
	permissionConflict := syncNow()
	if permissionConflict.Session.State != "conflict" || len(permissionConflict.Conflicts) == 0 {
		t.Fatalf("local deletion versus cloud permission edit must conflict: %#v", permissionConflict)
	}
	assertFile(cloudProject, "nested/keep.txt", "initial")
	if err := os.Chmod(cloudFile, 0o644); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceSyncE2EFile(t, cloudProject, "nested/keep.txt", []byte("independent cloud edit"))
	conflict := syncNow()
	if conflict.Session.State != "conflict" || len(conflict.Conflicts) == 0 {
		t.Fatalf("local deletion versus cloud edit must conflict: %#v", conflict)
	}
	assertFile(cloudProject, "nested/keep.txt", "independent cloud edit")
	assertMissing(source, "nested/keep.txt")
	assertFile(cloudProject, "cloud-only.txt", "cloud")
	if _, err := pool.Exec(ctx, `DELETE FROM workspace_sync_project_live_baselines WHERE workspace_id=$1 AND session_id=$2`, value.ID, second.Session.ID); err != nil {
		t.Fatal(err)
	}
	missingBaseline := syncNow()
	if missingBaseline.Session.State != "conflict" || len(missingBaseline.Conflicts) != 1 || missingBaseline.Conflicts[0].Reason != "cloud_baseline_unavailable" {
		t.Fatalf("a missing old cloud baseline must fail closed: %#v", missingBaseline)
	}
	assertFile(cloudProject, "nested/keep.txt", "independent cloud edit")

	movedSource := source + "-unavailable"
	if err := os.Rename(source, movedSource); err != nil {
		t.Fatal(err)
	}
	_, missingErr := syncClient.Sync(ctx, syncOptions)
	if err := os.Rename(movedSource, source); err != nil {
		t.Fatal(err)
	}
	if missingErr == nil {
		t.Fatal("an inaccessible source root was treated as an empty upload")
	}
	assertFile(cloudProject, "nested/keep.txt", "independent cloud edit")
	assertFile(cloudProject, "update.txt", "after")
	if err := store.RevokeToken(ctx, tokenID, owner.ID); err != nil {
		t.Fatal(err)
	}
	_, revokedErr := syncClient.Sync(ctx, syncOptions)
	var httpErr *client.HTTPError
	if !errors.As(revokedErr, &httpErr) || httpErr.Status != http.StatusUnauthorized {
		t.Fatalf("revoked credential must block a project sync: %v", revokedErr)
	}
	assertFile(cloudProject, "nested/keep.txt", "independent cloud edit")
	assertFile(cloudProject, "update.txt", "after")
}
