package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

const (
	hostWorkspaceReadOwner  = "u1234567"
	hostWorkspaceReadMember = "u7654321"
	hostWorkspaceReadHost   = "h1234567"
)

type hostWorkspaceReadStore struct {
	*syncServiceStore
	bindings         []workspace.Binding
	returnUnfiltered bool
	listErr          error
	bindingErr       error
	manifestErr      error
}

func (s *hostWorkspaceReadStore) GetWorkspaceBindingsByHost(ctx context.Context, hostID string) ([]workspace.Binding, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bindings := make([]workspace.Binding, 0, len(s.bindings))
	for _, binding := range s.bindings {
		if s.returnUnfiltered || binding.HostID == hostID {
			bindings = append(bindings, binding)
		}
	}
	return bindings, nil
}

func (s *hostWorkspaceReadStore) GetWorkspaceBinding(ctx context.Context, hostID, workspaceID string) (workspace.Binding, error) {
	if s.bindingErr != nil {
		return workspace.Binding{}, s.bindingErr
	}
	return s.syncServiceStore.GetWorkspaceBinding(ctx, hostID, workspaceID)
}

func (s *hostWorkspaceReadStore) GetWorkspaceManifestPage(ctx context.Context, workspaceID string, revision int64, prefix, after string, limit int32) (workspace.ManifestPage, error) {
	if s.manifestErr != nil {
		return workspace.ManifestPage{}, s.manifestErr
	}
	return s.syncServiceStore.GetWorkspaceManifestPage(ctx, workspaceID, revision, prefix, after, limit)
}

func newHostWorkspaceReadFixture(t *testing.T) (*hostWorkspaceReadStore, *syncBlockMemory, *WorkspaceSyncService) {
	t.Helper()
	base := newSyncServiceStore()
	base.workspace.OwnerID = hostWorkspaceReadOwner
	base.workspace.Revision = 4
	base.binding = workspace.Binding{
		ID: "b1234567", HostID: hostWorkspaceReadHost, OwnerID: hostWorkspaceReadOwner,
		Workspace: base.workspace, Directory: "project", State: workspace.BindingActive, BindingVersion: 3,
		DesiredRevision: 4, AppliedRevision: 4, SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	}
	base.snapshots[4] = []workspace.File{}
	store := &hostWorkspaceReadStore{syncServiceStore: base, bindings: []workspace.Binding{base.binding}}
	blocks := newSyncBlockMemory()
	service, err := NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	return store, blocks, service
}

func hostWorkspaceReadFile(t *testing.T, blocks *syncBlockMemory, filePath string, parts ...[]byte) workspace.File {
	t.Helper()
	file := workspace.File{Path: filePath, Mode: 0644, Chunks: []workspace.Chunk{}}
	hasher := sha256.New()
	for _, part := range parts {
		digest := sha256.Sum256(part)
		chunk := workspace.Chunk{SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(part))}
		if _, err := blocks.PutBlock("w1234567", chunk.SHA256, bytes.NewReader(part)); err != nil {
			t.Fatal(err)
		}
		file.Chunks = append(file.Chunks, chunk)
		file.SizeBytes += int64(len(part))
		_, _ = hasher.Write(part)
	}
	file.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	return file
}

func TestListHostWorkspaceBindingsValidatesIdentityAndOwnership(t *testing.T) {
	store, _, service := newHostWorkspaceReadFixture(t)
	store.binding.State = workspace.BindingPreparing
	store.bindings = []workspace.Binding{store.binding}

	items, err := service.ListHostWorkspaceBindings(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost)
	if err != nil || len(items) != 1 || items[0].State != workspace.BindingPreparing {
		t.Fatalf("member metadata listing = %#v, %v; want inactive binding metadata", items, err)
	}
	if _, err := service.ListHostWorkspaceBindings(context.Background(), "", hostWorkspaceReadHost); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("empty actor error = %v, want unauthenticated", err)
	}
	if _, err := service.ListHostWorkspaceBindings(context.Background(), hostWorkspaceReadMember, "bad"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("invalid Host error = %v, want not_found", err)
	}

	store.returnUnfiltered = true
	store.bindings[0].HostID = "h8765432"
	if _, err := service.ListHostWorkspaceBindings(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("foreign Host binding error = %v, want internal", err)
	}
	store.returnUnfiltered = false
	store.bindings[0] = store.binding
	store.bindings[0].Workspace.OwnerID = "u7654321"
	if _, err := service.ListHostWorkspaceBindings(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("mismatched binding owner error = %v, want internal", err)
	}
}

func TestHostWorkspaceStoreFailuresReturnUnavailable(t *testing.T) {
	store, _, service := newHostWorkspaceReadFixture(t)
	store.listErr = errors.New("database connection failed")
	if _, err := service.ListHostWorkspaceBindings(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost); domain.CodeOf(err) != domain.CodeWorkspaceUnavailable {
		t.Fatalf("list failure = %v, want workspace_unavailable", err)
	}

	store.listErr = nil
	store.bindingErr = errors.New("database connection failed")
	if _, err := service.GetHostWorkspaceManifest(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 4, "", "", 10); domain.CodeOf(err) != domain.CodeWorkspaceUnavailable {
		t.Fatalf("binding failure = %v, want workspace_unavailable", err)
	}

	store.bindingErr = nil
	store.manifestErr = domain.NewError(domain.CodeNotFound, "workspace revision not found")
	if _, err := service.GetHostWorkspaceManifest(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 4, "", "", 10); domain.CodeOf(err) != domain.CodeWorkspaceUnavailable {
		t.Fatalf("manifest failure = %v, want workspace_unavailable", err)
	}

	store.manifestErr = errors.New("database connection failed")
	if _, err := service.GetHostWorkspaceManifest(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 4, "", "", 10); domain.CodeOf(err) != domain.CodeWorkspaceUnavailable {
		t.Fatalf("manifest store failure = %v, want workspace_unavailable", err)
	}
}

func TestGetHostWorkspaceManifestPinsAppliedRevisionAndCursor(t *testing.T) {
	store, _, service := newHostWorkspaceReadFixture(t)
	store.snapshots[4] = []workspace.File{
		syncServiceFile("a.txt", []byte("a")), syncServiceFile("b.txt", []byte("b")),
	}
	page, err := service.GetHostWorkspaceManifest(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 4, "", "", 1)
	if err != nil || page.Revision != 4 || len(page.Files) != 1 || page.Files[0].Path != "a.txt" || page.NextCursor == nil {
		t.Fatalf("first manifest page = %#v, %v", page, err)
	}
	page, err = service.GetHostWorkspaceManifest(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 4, "", *page.NextCursor, 1)
	if err != nil || page.Revision != 4 || len(page.Files) != 1 || page.Files[0].Path != "b.txt" || page.NextCursor != nil {
		t.Fatalf("second manifest page = %#v, %v", page, err)
	}
	if _, err := service.GetHostWorkspaceManifest(context.Background(), hostWorkspaceReadOwner, hostWorkspaceReadHost, store.workspace.ID, 4, "", "", 10); err != nil {
		t.Fatalf("workspace owner manifest error = %v", err)
	}
	if _, err := service.GetHostWorkspaceManifest(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 3, "", "", 10); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("stale revision error = %v, want workspace_conflict", err)
	}
	cursor := workspace.EncodeManifestCursor(workspace.ManifestCursor{Revision: 4, Prefix: "other", After: "a.txt"})
	if _, err := service.GetHostWorkspaceManifest(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 4, "", cursor, 10); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("wrong-prefix cursor error = %v, want invalid_argument", err)
	}
}

func TestGetHostWorkspaceManifestRequiresReadyBindingAndRevision(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*hostWorkspaceReadStore)
		wantCode domain.ErrorCode
	}{
		{name: "missing binding", mutate: func(store *hostWorkspaceReadStore) { store.binding.HostID = "h8765432" }, wantCode: domain.CodeNotFound},
		{name: "preparing binding", mutate: func(store *hostWorkspaceReadStore) { store.binding.State = workspace.BindingPreparing }, wantCode: domain.CodeWorkspaceBusy},
		{name: "failed binding", mutate: func(store *hostWorkspaceReadStore) { store.binding.State = workspace.BindingFailed }, wantCode: domain.CodeWorkspaceConflict},
		{name: "sync conflict", mutate: func(store *hostWorkspaceReadStore) {
			store.binding.SyncState = workspace.BindingSyncConflict
			store.binding.SyncConflicts = []string{"src/main.go"}
		}, wantCode: domain.CodeWorkspaceConflict},
		{name: "not applied", mutate: func(store *hostWorkspaceReadStore) { store.binding.AppliedRevision = 3 }, wantCode: domain.CodeWorkspaceBusy},
		{name: "desired revision stale", mutate: func(store *hostWorkspaceReadStore) { store.binding.DesiredRevision = 3 }, wantCode: domain.CodeWorkspaceBusy},
		{name: "workspace not ready", mutate: func(store *hostWorkspaceReadStore) {
			store.binding.Workspace.State = workspace.StateDeleting
		}, wantCode: domain.CodeWorkspaceBusy},
		{name: "binding owner mismatch", mutate: func(store *hostWorkspaceReadStore) {
			store.binding.Workspace.OwnerID = "u7654321"
		}, wantCode: domain.CodeNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, _, service := newHostWorkspaceReadFixture(t)
			test.mutate(store)
			_, err := service.GetHostWorkspaceManifest(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 0, "", "", 10)
			if domain.CodeOf(err) != test.wantCode {
				t.Fatalf("manifest error = %v, want %s", err, test.wantCode)
			}
		})
	}
}

func TestGetHostWorkspaceManifestPinsRevisionZero(t *testing.T) {
	store, _, service := newHostWorkspaceReadFixture(t)
	store.workspace.Revision = 0
	store.binding.Workspace.Revision = 0
	store.binding.DesiredRevision = 0
	store.binding.AppliedRevision = 0
	store.bindings[0] = store.binding
	store.snapshots[0] = []workspace.File{syncServiceFile("initial.txt", []byte("initial"))}

	page, err := service.GetHostWorkspaceManifest(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 0, "", "", 10)
	if err != nil || page.Revision != 0 || len(page.Files) != 1 || page.Files[0].Path != "initial.txt" {
		t.Fatalf("revision-zero manifest = %#v, %v", page, err)
	}
}

func TestStreamHostWorkspaceFileVerifiesAllChunksBeforeWriting(t *testing.T) {
	store, blocks, service := newHostWorkspaceReadFixture(t)
	parts := [][]byte{[]byte("workspace "), []byte("file body")}
	file := hostWorkspaceReadFile(t, blocks, "docs/readme.txt", parts...)
	store.snapshots[4] = []workspace.File{file}
	var output []byte
	var writes int
	got, err := service.StreamHostWorkspaceFile(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 4, file.Path,
		func(metadata workspace.File, chunk []byte) error {
			writes++
			if metadata.Path != file.Path || metadata.SizeBytes != file.SizeBytes {
				t.Fatalf("stream metadata = %#v", metadata)
			}
			output = append(output, chunk...)
			return nil
		})
	if err != nil || got.Path != file.Path || writes != len(parts) || string(output) != "workspace file body" {
		t.Fatalf("stream result = %#v, writes %d, body %q, error %v", got, writes, output, err)
	}

	store, blocks, service = newHostWorkspaceReadFixture(t)
	file = hostWorkspaceReadFile(t, blocks, "docs/readme.txt", parts...)
	store.snapshots[4] = []workspace.File{file}
	file.SHA256 = strings.Repeat("0", 64)
	store.snapshots[4] = []workspace.File{file}
	writes = 0
	if _, err := service.StreamHostWorkspaceFile(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 4, file.Path,
		func(workspace.File, []byte) error { writes++; return nil }); domain.CodeOf(err) != domain.CodeWorkspaceConflict || writes != 0 {
		t.Fatalf("bad full-file digest stream = writes %d, error %v; want conflict before writes", writes, err)
	}
}

func TestStreamHostWorkspaceFileRejectsPathAndBlockFailures(t *testing.T) {
	store, blocks, service := newHostWorkspaceReadFixture(t)
	file := hostWorkspaceReadFile(t, blocks, "docs/readme.txt", []byte("body"))
	store.snapshots[4] = []workspace.File{file}
	for _, path := range []string{"../secret", "/etc/passwd", "docs//readme.txt"} {
		if _, err := service.StreamHostWorkspaceFile(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 4, path,
			func(workspace.File, []byte) error { t.Fatal("invalid path wrote content"); return nil }); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Fatalf("path %q error = %v, want invalid_argument", path, err)
		}
	}
	if _, err := service.StreamHostWorkspaceFile(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 4, "docs",
		func(workspace.File, []byte) error { t.Fatal("directory prefix wrote content"); return nil }); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("directory prefix error = %v, want not_found", err)
	}
	blocks.data[file.Chunks[0].SHA256] = []byte("bad!")
	writes := 0
	if _, err := service.StreamHostWorkspaceFile(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 4, file.Path,
		func(workspace.File, []byte) error { writes++; return nil }); domain.CodeOf(err) != domain.CodeWorkspaceConflict || writes != 0 {
		t.Fatalf("corrupt block stream = writes %d, error %v; want conflict before writes", writes, err)
	}
	blocks.data[file.Chunks[0].SHA256] = []byte("bad")
	if _, err := service.StreamHostWorkspaceFile(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 4, file.Path,
		func(workspace.File, []byte) error { writes++; return nil }); domain.CodeOf(err) != domain.CodeWorkspaceConflict || writes != 0 {
		t.Fatalf("wrong-size block stream = writes %d, error %v; want conflict before writes", writes, err)
	}

	store, _, service = newHostWorkspaceReadFixture(t)
	file = hostWorkspaceReadFile(t, newSyncBlockMemory(), "docs/readme.txt", []byte("missing"))
	store.snapshots[4] = []workspace.File{file}
	if _, err := service.StreamHostWorkspaceFile(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 4, file.Path,
		func(workspace.File, []byte) error { t.Fatal("missing block wrote content"); return nil }); domain.CodeOf(err) != domain.CodeWorkspaceUnavailable {
		t.Fatalf("missing block error = %v, want workspace_unavailable", err)
	}
}

func TestStreamHostWorkspaceFileStopsWhenBindingChangesBetweenBlocks(t *testing.T) {
	store, blocks, service := newHostWorkspaceReadFixture(t)
	file := hostWorkspaceReadFile(t, blocks, "docs/readme.txt", []byte("first"), []byte("second"))
	store.snapshots[4] = []workspace.File{file}
	writes := 0
	_, err := service.StreamHostWorkspaceFile(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 4, file.Path,
		func(_ workspace.File, chunk []byte) error {
			writes++
			if writes == 1 {
				store.binding.AppliedRevision = 3
			}
			if len(chunk) == 0 {
				t.Fatal("non-empty file emitted an empty chunk")
			}
			return nil
		})
	if domain.CodeOf(err) != domain.CodeWorkspaceBusy || writes != 1 {
		t.Fatalf("changed binding stream = writes %d, error %v; want one block then busy", writes, err)
	}
}

func TestStreamHostWorkspaceFileCallsWriterForEmptyFileAndStopsOnWriterError(t *testing.T) {
	store, _, service := newHostWorkspaceReadFixture(t)
	empty := hostWorkspaceReadFile(t, newSyncBlockMemory(), "empty.txt")
	store.snapshots[4] = []workspace.File{empty}
	writes := 0
	if _, err := service.StreamHostWorkspaceFile(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 4, empty.Path,
		func(metadata workspace.File, chunk []byte) error {
			writes++
			if metadata.SizeBytes != 0 || len(chunk) != 0 {
				t.Fatalf("empty file callback = %#v, %q", metadata, chunk)
			}
			return nil
		}); err != nil || writes != 1 {
		t.Fatalf("empty file writes = %d, error %v", writes, err)
	}

	store, blocks, service := newHostWorkspaceReadFixture(t)
	file := hostWorkspaceReadFile(t, blocks, "two-parts.txt", []byte("one"), []byte("two"))
	store.snapshots[4] = []workspace.File{file}
	wantErr := errors.New("writer stopped")
	writes = 0
	if _, err := service.StreamHostWorkspaceFile(context.Background(), hostWorkspaceReadMember, hostWorkspaceReadHost, store.workspace.ID, 4, file.Path,
		func(workspace.File, []byte) error { writes++; return wantErr }); !errors.Is(err, wantErr) || writes != 1 {
		t.Fatalf("writer failure = writes %d, error %v; want one callback and original error", writes, err)
	}
}
