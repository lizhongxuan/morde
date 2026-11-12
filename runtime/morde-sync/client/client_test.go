package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mordesync "morde-sync"
)

const (
	testWorkspace = "wabc1234"
	testSession   = "pabc1234"
)

func TestLocalManifestThrottlesScanProgress(t *testing.T) {
	directory := t.TempDir()
	for index := range 200 {
		name := filepath.Join(directory, fmt.Sprintf("file-%03d.txt", index))
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var scanEvents int
	manifest, _, err := (&Client{}).localManifest(context.Background(), SyncOptions{WorkspaceID: testWorkspace}, directory, func(value Progress) {
		if value.Stage == string(mordesync.PhaseInventory) || value.Stage == string(mordesync.PhaseHash) {
			scanEvents++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.FileCount != 200 || scanEvents >= int(manifest.FileCount) {
		t.Fatalf("manifest=%d files, scan notifications=%d; progress should be throttled", manifest.FileCount, scanEvents)
	}
}

func TestTaskLinkedSyncRequiresAndPreservesItsFence(t *testing.T) {
	baseRevision := int64(3)
	options := SyncOptions{WorkspaceID: testWorkspace, Directory: t.TempDir(), Direction: "upload", Policy: "compare_merge",
		BaseRevision: &baseRevision, Scope: []string{"reports/out.txt"}, TaskID: "qabc1234",
		TaskHubOperationKey: "agent:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if err := (&Client{}).validateOptions(context.Background(), options); err != nil {
		t.Fatalf("valid Task sync options rejected: %v", err)
	}
	data, err := createSessionPayload("", options.Direction, options.Policy, baseRevision, options.Scope,
		mordesync.Manifest{Files: []mordesync.File{}, EmptyDirectories: []string{}}, nil, nil, nil,
		options.TaskID, options.TaskHubOperationKey)
	if err != nil {
		t.Fatal(err)
	}
	var request createSessionRequest
	if err := json.Unmarshal(data, &request); err != nil || request.TaskID != options.TaskID || request.TaskHubOperationKey != options.TaskHubOperationKey {
		t.Fatalf("Task sync create request = %+v, err=%v", request, err)
	}
	options.Scope = nil
	if err := (&Client{}).validateOptions(context.Background(), options); err == nil {
		t.Fatal("Task sync accepted a request without one scoped file")
	}
	options.SessionID, options.Direction, options.Policy, options.BaseRevision = testSession, "upload", "", nil
	if err := (&Client{}).validateOptions(context.Background(), options); err != nil {
		t.Fatalf("valid saved Task session could not be resumed without repeating its manifest scope: %v", err)
	}
}

func TestProjectFolderSyncUsesUnscopedUploadSession(t *testing.T) {
	const folderID = "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baseRevision := int64(0)
	created := false
	writeSession := func(w http.ResponseWriter, status int, session SyncSession) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(session); err != nil {
			t.Errorf("encode sync session metadata: %v", err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions") {
			var request createSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode project upload metadata: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if request.FolderID != folderID || request.Direction != "upload" || request.Policy != "compare_merge" || request.BaseRevision != 0 ||
				len(request.Scope) != 0 || len(request.Files) != 0 || len(request.EmptyDirectories) != 0 || len(request.DeletedPaths) != 0 || len(request.DeletedDirectories) != 0 {
				t.Errorf("project upload request must contain only an empty local manifest and folder identity: %+v", request)
			}
			created = true
			writeSession(w, http.StatusCreated, SyncSession{ID: testSession, WorkspaceID: testWorkspace, FolderID: folderID,
				Direction: "upload", Policy: "compare_merge", State: "planned", BaseRevision: 0, TargetRevision: 1,
				Stage: "compare", TransferBlocks: []TransferBlock{}, DownloadActions: []DownloadAction{}, Conflicts: []Conflict{}})
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSession+"/commit") {
			writeSession(w, http.StatusOK, SyncSession{ID: testSession, WorkspaceID: testWorkspace, FolderID: folderID,
				Direction: "upload", Policy: "compare_merge", State: "completed", BaseRevision: 0, TargetRevision: 1,
				Stage: "complete", TransferBlocks: []TransferBlock{}, DownloadActions: []DownloadAction{}, Conflicts: []Conflict{}})
			return
		}
		t.Errorf("unexpected project upload request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := New(server.URL, "local-metadata-only-test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, FolderID: folderID, Directory: t.TempDir(),
		Direction: "upload", Policy: "compare_merge", BaseRevision: &baseRevision, Scope: []string{}})
	if err != nil || !created || result.Session.State != "completed" || result.Session.FolderID != folderID {
		t.Fatalf("project upload result = %+v, created=%v, err=%v", result, created, err)
	}

	invalid := []SyncOptions{
		{WorkspaceID: testWorkspace, FolderID: folderID, Directory: t.TempDir(), Direction: "bidirectional", Policy: "compare_merge", BaseRevision: &baseRevision},
		{WorkspaceID: testWorkspace, FolderID: folderID, Directory: t.TempDir(), Direction: "upload", Policy: "live", BaseRevision: &baseRevision},
		{WorkspaceID: testWorkspace, FolderID: folderID, Directory: t.TempDir(), Direction: "upload", Policy: "compare_merge"},
		{WorkspaceID: testWorkspace, FolderID: folderID, Directory: t.TempDir(), Direction: "upload", Policy: "compare_merge", BaseRevision: &baseRevision, Scope: []string{"src"}},
		{WorkspaceID: testWorkspace, FolderID: folderID, Directory: t.TempDir(), Direction: "upload", Policy: "compare_merge", BaseRevision: &baseRevision, DeletedPaths: []string{"old.txt"}},
		{WorkspaceID: testWorkspace, FolderID: folderID, Directory: t.TempDir(), Direction: "upload", Policy: "compare_merge", BaseRevision: &baseRevision, DeletedDirectories: []string{"old-directory"}},
	}
	for _, options := range invalid {
		if err := client.validateOptions(context.Background(), options); err == nil {
			t.Fatalf("project folder sync accepted invalid options: %+v", options)
		}
	}
}

func TestProjectUploadRejectsCloudToLocalSessionActions(t *testing.T) {
	const folderID = "morde-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	baseRevision := int64(0)
	sha := strings.Repeat("a", 64)
	tests := []struct {
		name           string
		actions        []DownloadAction
		transferBlocks []TransferBlock
	}{
		{name: "download action", actions: []DownloadAction{{Path: "cloud-only.txt", Action: "upsert", SHA256: &sha, SizeBytes: 0}}, transferBlocks: []TransferBlock{}},
		{name: "download block", actions: []DownloadAction{}, transferBlocks: []TransferBlock{{Chunk: Chunk{SHA256: sha, SizeBytes: 1}, Role: "download"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.Path)
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/sync-sessions") {
					t.Errorf("unexpected project upload request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				var request createSessionRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode project upload metadata: %v", err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				if request.FolderID != folderID || request.Direction != "upload" || request.Policy != "compare_merge" || request.BaseRevision != 0 ||
					len(request.Scope) != 0 || len(request.Files) != 0 || len(request.EmptyDirectories) != 0 || len(request.DeletedPaths) != 0 || len(request.DeletedDirectories) != 0 {
					t.Errorf("project upload request must contain only an empty local manifest and folder identity: %+v", request)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(SyncSession{ID: testSession, WorkspaceID: testWorkspace, FolderID: folderID,
					Direction: "upload", Policy: "compare_merge", State: "planned", BaseRevision: 0, TargetRevision: 1,
					Stage: "compare", TransferBlocks: test.transferBlocks, DownloadActions: test.actions, Conflicts: []Conflict{}})
			}))
			defer server.Close()

			client, err := New(server.URL, "local-metadata-only-test-token", nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, FolderID: folderID, Directory: t.TempDir(),
				Direction: "upload", Policy: "compare_merge", BaseRevision: &baseRevision, Scope: []string{}})
			if err == nil || !strings.Contains(err.Error(), "project upload session unexpectedly contains") {
				t.Fatalf("project upload accepted cloud-to-local session actions: %v", err)
			}
			if len(requests) != 1 || !strings.HasSuffix(requests[0], "/sync-sessions") {
				t.Fatalf("project upload made requests beyond session metadata: %v", requests)
			}
		})
	}
}

func TestTaskLinkedSyncManifestContainsOnlyExactFile(t *testing.T) {
	options := SyncOptions{TaskID: "qabc1234", TaskHubOperationKey: "agent:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Scope: []string{"reports/result.txt"}}
	file := mordesync.File{Path: "reports/result.txt"}
	if err := validateTaskLinkedManifest(options, mordesync.Manifest{Files: []mordesync.File{file}}); err != nil {
		t.Fatalf("exact Task file manifest rejected: %v", err)
	}
	if err := validateTaskLinkedManifest(options, mordesync.Manifest{Files: []mordesync.File{file, {Path: "reports/extra.txt"}}}); err == nil {
		t.Fatal("Task file manifest accepted another file under a recursive scope")
	}
	if err := validateTaskLinkedManifest(options, mordesync.Manifest{Files: []mordesync.File{file}, EmptyDirectories: []string{"reports/empty"}}); err == nil {
		t.Fatal("Task file manifest accepted an empty directory")
	}
	options.Scope = []string{"reports"}
	if err := validateTaskLinkedManifest(options, mordesync.Manifest{Files: []mordesync.File{file}}); err == nil {
		t.Fatal("Task file manifest accepted a recursive scope")
	}
}

func TestWorkspaceFileMappingPreservesKnownModificationTime(t *testing.T) {
	modified := int64(1_700_000_000_123_456_789)
	local := mordesync.File{Path: "notes.txt", HardlinkTo: "anchor.txt", Size: 1, Mode: 0o640, ModTimeNS: modified, ModTimeKnown: true,
		SHA256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881",
		Blocks: []mordesync.Block{{Offset: 0, Size: 1, SHA256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}}}
	wire := toWorkspaceFile(local)
	if wire.ModTimeNS == nil || *wire.ModTimeNS != modified {
		t.Fatalf("wire modification time = %#v", wire.ModTimeNS)
	}
	decoded, err := toCoreFile(wire)
	if err != nil || !decoded.ModTimeKnown || decoded.ModTimeNS != modified || decoded.HardlinkTo != local.HardlinkTo {
		t.Fatalf("decoded modification time = %#v, %v", decoded, err)
	}
	if legacy, err := toCoreFile(workspaceFile{Path: "old.txt", SizeBytes: 0, SHA256: strings.Repeat("b", 64), Mode: 0o600, Chunks: []workspaceChunk{}}); err != nil || legacy.ModTimeKnown {
		t.Fatalf("legacy modification time was treated as known: %#v, %v", legacy, err)
	}
}

func TestSyncVerifiedExistingAppliesMetadataOnlyLiveUpdate(t *testing.T) {
	content := []byte("same content")
	initialTime := time.Unix(1_700_000_000, 123_456_789)
	updatedTime := initialTime.Add(time.Second)
	create := func(name string) (string, mordesync.File) {
		t.Helper()
		directory := t.TempDir()
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, content, 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, initialTime, initialTime); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		baseline := makeFile(name, content)
		baseline.Mode = uint32(info.Mode().Perm())
		baseline.ModTimeNS, baseline.ModTimeKnown = info.ModTime().UnixNano(), true
		return directory, baseline
	}

	t.Run("apply remote mode and time from the unchanged baseline", func(t *testing.T) {
		directory, baseline := create("notes.txt")
		expected := baseline
		expected.Mode = 0o600
		expected.ModTimeNS = updatedTime.UnixNano()
		if err := syncVerifiedExisting(context.Background(), directory, expected, &baseline); err != nil {
			t.Fatalf("syncVerifiedExisting() error = %v", err)
		}
		info, err := os.Stat(filepath.Join(directory, "notes.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if uint32(info.Mode().Perm()) != expected.Mode || info.ModTime().UnixNano() != expected.ModTimeNS {
			t.Fatalf("updated destination metadata = mode %o, time %d, err %v", info.Mode().Perm(), info.ModTime().UnixNano(), err)
		}
	})

	t.Run("preserve metadata changed after the live plan", func(t *testing.T) {
		directory, baseline := create("notes.txt")
		expected := baseline
		expected.ModTimeNS = updatedTime.UnixNano()
		localTime := updatedTime.Add(time.Second)
		path := filepath.Join(directory, "notes.txt")
		if err := os.Chtimes(path, localTime, localTime); err != nil {
			t.Fatal(err)
		}
		if err := syncVerifiedExisting(context.Background(), directory, expected, &baseline); !errors.Is(err, mordesync.ErrDestinationChange) {
			t.Fatalf("syncVerifiedExisting() error = %v; want local metadata change conflict", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.ModTime().UnixNano() != localTime.UnixNano() {
			t.Fatalf("concurrent local timestamp = %d, %v; want %d", info.ModTime().UnixNano(), err, localTime.UnixNano())
		}
	})
}

func TestWorkspaceFileMappingPreservesSymlinkTargetWithoutBlocks(t *testing.T) {
	target := "../content.txt"
	local := mordesync.File{Path: "nested/link", Kind: mordesync.FileKindSymlink, SymlinkTarget: target,
		Size: int64(len([]byte(target))), SHA256: sumSHA256([]byte(target)), Mode: 0}
	wire := toWorkspaceFile(local)
	if wire.Kind != mordesync.FileKindSymlink || wire.SymlinkTarget != target || wire.SizeBytes != local.Size || wire.Mode != 0 || len(wire.Chunks) != 0 {
		t.Fatalf("symlink wire file = %#v", wire)
	}
	decoded, err := toCoreFile(wire)
	if err != nil || decoded.Kind != mordesync.FileKindSymlink || decoded.SymlinkTarget != target || decoded.Size != local.Size ||
		decoded.SHA256 != local.SHA256 || decoded.Mode != 0 || len(decoded.Blocks) != 0 {
		t.Fatalf("decoded symlink = %#v, %v", decoded, err)
	}
}

func TestClientEmptyDirectoryProtocolAndRecovery(t *testing.T) {
	t.Run("create payload and action shape", func(t *testing.T) {
		data, err := createSessionPayload("", "bidirectional", "live", 7, nil,
			mordesync.Manifest{Files: []mordesync.File{}, EmptyDirectories: []string{"reports/empty"}},
			[]string{"old.txt"}, []string{"old-directory"}, nil, "", "")
		if err != nil {
			t.Fatal(err)
		}
		var request createSessionRequest
		if err := json.Unmarshal(data, &request); err != nil {
			t.Fatal(err)
		}
		if len(request.Files) != 0 || len(request.EmptyDirectories) != 1 || request.EmptyDirectories[0] != "reports/empty" ||
			len(request.DeletedPaths) != 1 || request.DeletedPaths[0] != "old.txt" ||
			len(request.DeletedDirectories) != 1 || request.DeletedDirectories[0] != "old-directory" {
			t.Fatalf("directory sync payload = %+v", request)
		}
		values := []DownloadAction{{Path: ".", Action: "directory_upsert"}, {Path: "old-empty", Action: "directory_delete"}}
		if _, err := downloadActionMap(values, "live"); err != nil {
			t.Fatalf("downloadActionMap(directory actions) error = %v", err)
		}
		values[0].SizeBytes = 1
		if _, err := downloadActionMap(values, "live"); err == nil {
			t.Fatal("downloadActionMap accepted file size on a directory action")
		}
		values[0].SizeBytes = 0
		if _, err := downloadActionMap(values, "fill_missing"); err == nil {
			t.Fatal("downloadActionMap accepted a directory delete under fill_missing")
		}
	})
	t.Run("manifest pagination and changelog", func(t *testing.T) {
		cursor := "next-page"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/workspaces/"+testWorkspace+"/manifest" {
				if r.URL.Query().Get("cursor") == "" {
					_ = json.NewEncoder(w).Encode(workspaceManifest{WorkspaceID: testWorkspace, Revision: 9,
						Files: []workspaceFile{}, EmptyDirectories: []string{"first"}, NextCursor: &cursor})
					return
				}
				_ = json.NewEncoder(w).Encode(workspaceManifest{WorkspaceID: testWorkspace, Revision: 9,
					Files: []workspaceFile{}, EmptyDirectories: []string{"nested/second"}})
				return
			}
			next := encodeLiveCursor(liveCursor{Revision: 1, Path: "."})
			_ = json.NewEncoder(w).Encode(workspaceChangePage{Items: []workspaceChange{{Revision: 1, Path: ".", Action: "directory_upsert"}}, NextCursor: next})
		}))
		defer server.Close()
		client, err := New(server.URL, "test-token", server.Client())
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := client.manifest(context.Background(), testWorkspace, 9)
		if err != nil {
			t.Fatal(err)
		}
		if manifest.FileCount != 0 || manifest.TotalBytes != 0 || len(manifest.Files) != 0 ||
			len(manifest.EmptyDirectories) != 2 || manifest.EmptyDirectories[0] != "first" || manifest.EmptyDirectories[1] != "nested/second" {
			t.Fatalf("manifest = %#v", manifest)
		}
		page, err := client.getChanges(context.Background(), testWorkspace, encodeLiveCursor(liveCursor{}), 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Items[0].Path != "." || page.Items[0].Action != "directory_upsert" || page.Items[0].File != nil {
			t.Fatalf("directory-only change page = %#v", page)
		}
	})
	t.Run("live directory diff and preserved replacement recovery", func(t *testing.T) {
		baseline := mordesync.Manifest{Files: []mordesync.File{}, EmptyDirectories: []string{"old-empty"}}
		local := mordesync.Manifest{Files: []mordesync.File{}, EmptyDirectories: []string{"new-empty"}}
		if sameManifestContent(local, baseline) {
			t.Fatal("empty-directory changes were omitted from live manifest equality")
		}
		if deleted := missingBaselineDirectories(baseline, mordesync.Manifest{Files: []mordesync.File{}, EmptyDirectories: []string{}}, nil); !equalStrings(deleted, []string{"old-empty"}) {
			t.Fatalf("deleted empty directories = %#v", deleted)
		}
		withDescendant := mordesync.Manifest{Files: []mordesync.File{makeFile("old-empty/child.txt", []byte("child"))}}
		if deleted := missingBaselineDirectories(baseline, withDescendant, nil); len(deleted) != 0 {
			t.Fatalf("directory that remains structurally present was marked deleted: %#v", deleted)
		}
		if conflicts := compareManifestConflicts(baseline, local); len(conflicts) != 2 {
			t.Fatalf("directory changes should report both paths, got %#v", conflicts)
		}
		directory := t.TempDir()
		writeTestFile(t, directory, "legacy", "baseline")
		expected := makeFile("legacy", []byte("baseline"))
		_, removed, err := removeIfHash(context.Background(), directory, testWorkspace, expected)
		if err != nil || !removed {
			t.Fatalf("preserve baseline file = removed %t, err %v", removed, err)
		}
		preserved, err := hasPreservedBaseline(context.Background(), directory, testWorkspace, expected)
		if err != nil || !preserved {
			t.Fatalf("hasPreservedBaseline() = %t, %v", preserved, err)
		}
	})
}

func TestSyncUploadsDownloadsAndPagesManifest(t *testing.T) {
	directory := t.TempDir()
	localData := []byte("local source")
	if err := os.WriteFile(filepath.Join(directory, "local.txt"), localData, 0o644); err != nil {
		t.Fatal(err)
	}
	remoteA := makeFile("remote-a.txt", []byte("remote A"))
	remoteB := makeFile("remote-b.txt", []byte("remote B"))
	remoteEmpty := makeFile("empty.txt", nil)
	local := makeFile("local.txt", localData)
	var requestsMu sync.Mutex
	var uploaded, blockAcknowledged, actionAcknowledged []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-device-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if !strings.HasPrefix(r.URL.Path, "/prefix/api/") {
			t.Errorf("unexpected base path: %s", r.URL.Path)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/prefix/api/workspaces/"+testWorkspace+"/sync-sessions":
			if r.Header.Get("Idempotency-Key") == "" {
				t.Error("create omitted Idempotency-Key")
			}
			var request createSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Direction != "bidirectional" || request.Policy != "fill_missing" || len(request.Files) != 1 || request.Files[0].Path != "local.txt" {
				t.Errorf("unexpected create request: %+v", request)
			}
			session := sessionFixture([]TransferBlock{
				{Chunk: Chunk{SHA256: local.SHA256, SizeBytes: local.Size}, Role: "upload"},
				{Chunk: Chunk{SHA256: remoteA.SHA256, SizeBytes: remoteA.Size}, Role: "download"},
				{Chunk: Chunk{SHA256: remoteB.SHA256, SizeBytes: remoteB.Size}, Role: "download"},
			}, local.Size+remoteA.Size+remoteB.Size, upsertAction(remoteA), upsertAction(remoteB), upsertAction(remoteEmpty))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(session)
		case r.Method == http.MethodGet && r.URL.Path == "/prefix/api/workspaces/"+testWorkspace+"/manifest":
			if r.URL.Query().Get("revision") != "2" {
				t.Errorf("manifest must pin target revision 2: %s", r.URL.RawQuery)
			}
			var page workspaceManifest
			switch r.URL.Query().Get("cursor") {
			case "":
				cursor := "page-two"
				page = workspaceManifest{WorkspaceID: testWorkspace, Revision: 2, Files: []workspaceFile{toWorkspaceFile(remoteA)}, NextCursor: &cursor}
			case "page-two":
				page = workspaceManifest{WorkspaceID: testWorkspace, Revision: 2, Files: []workspaceFile{toWorkspaceFile(remoteB), toWorkspaceFile(remoteEmpty)}}
			default:
				t.Errorf("unexpected manifest cursor: %q", r.URL.Query().Get("cursor"))
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(page)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/blocks/"+local.SHA256):
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != string(localData) {
				t.Errorf("uploaded block body = %q", body)
			}
			requestsMu.Lock()
			uploaded = append(uploaded, r.URL.Path)
			requestsMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blocks/"+remoteA.SHA256):
			_, _ = w.Write([]byte("remote A"))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blocks/"+remoteB.SHA256):
			_, _ = w.Write([]byte("remote B"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blocks/"+remoteA.SHA256+"/ack"):
			requestsMu.Lock()
			blockAcknowledged = append(blockAcknowledged, remoteA.SHA256)
			requestsMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blocks/"+remoteB.SHA256+"/ack"):
			requestsMu.Lock()
			blockAcknowledged = append(blockAcknowledged, remoteB.SHA256)
			requestsMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/download-actions/ack"):
			var ack struct {
				Path      string  `json:"path"`
				Action    string  `json:"action"`
				SHA256    *string `json:"sha256"`
				SizeBytes int64   `json:"size_bytes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&ack); err != nil {
				t.Fatal(err)
			}
			if ack.Action != "upsert" {
				t.Errorf("unexpected action ACK: %+v", ack)
			}
			switch ack.Path {
			case "remote-a.txt":
				assertLocalFile(t, directory, ack.Path, "remote A")
			case "remote-b.txt":
				assertLocalFile(t, directory, ack.Path, "remote B")
			case "empty.txt":
				data, err := os.ReadFile(filepath.Join(directory, ack.Path))
				if err != nil || len(data) != 0 {
					t.Fatalf("zero-byte action was not applied: data=%q err=%v", data, err)
				}
			default:
				t.Errorf("unexpected action ACK: %+v", ack)
			}
			requestsMu.Lock()
			actionAcknowledged = append(actionAcknowledged, ack.Path)
			requestsMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSession+"/commit"):
			session := sessionFixture([]TransferBlock{
				{Chunk: Chunk{SHA256: local.SHA256, SizeBytes: local.Size}, Role: "upload", Received: true},
				{Chunk: Chunk{SHA256: remoteA.SHA256, SizeBytes: remoteA.Size}, Role: "download", Received: true},
				{Chunk: Chunk{SHA256: remoteB.SHA256, SizeBytes: remoteB.Size}, Role: "download", Received: true},
			}, local.Size+remoteA.Size+remoteB.Size, appliedAction(remoteA), appliedAction(remoteB), appliedAction(remoteEmpty))
			session.State = "completed"
			session.ConfirmedBytes = session.PlannedBytes
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(session)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL+"/prefix", "test-device-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	baseRevision := int64(1)
	var progressMu sync.Mutex
	var progress []Progress
	result, err := client.Sync(context.Background(), SyncOptions{
		WorkspaceID: testWorkspace, Directory: directory, BaseRevision: &baseRevision,
		Progress: func(value Progress) { progressMu.Lock(); progress = append(progress, value); progressMu.Unlock() },
	})
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if result.Session.ID != testSession || result.Session.State != "completed" {
		t.Fatalf("unexpected final session: %+v", result.Session)
	}
	if len(uploaded) != 1 || len(blockAcknowledged) != 2 || len(actionAcknowledged) != 3 {
		t.Fatalf("uploaded=%v block ACKs=%v action ACKs=%v", uploaded, blockAcknowledged, actionAcknowledged)
	}
	assertLocalFile(t, directory, "remote-a.txt", "remote A")
	assertLocalFile(t, directory, "remote-b.txt", "remote B")
	if data, err := os.ReadFile(filepath.Join(directory, "empty.txt")); err != nil || len(data) != 0 {
		t.Fatalf("zero-byte file missing: data=%q err=%v", data, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("workspace contains unexpected client metadata or files: %v", entries)
	}
	progressMu.Lock()
	defer progressMu.Unlock()
	if len(progress) < 4 || progress[len(progress)-1].ConfirmedBytes != result.Session.PlannedBytes {
		t.Fatalf("progress did not reach completed bytes: %+v", progress)
	}
	var planned *Progress
	var sawUploadPath bool
	for index := range progress {
		if progress[index].Stage == "planning" {
			planned = &progress[index]
		}
		if progress[index].Stage == "transfer" && progress[index].CurrentPath == "local.txt" {
			sawUploadPath = true
		}
	}
	if planned == nil || planned.FileCount != 1 || planned.TotalBytes != int64(len(localData)) || planned.CurrentPath != "" {
		t.Fatalf("pre-transfer manifest totals were not reported: %+v", planned)
	}
	if !sawUploadPath {
		t.Fatalf("upload progress did not identify the current file: %+v", progress)
	}
}

func TestSyncDownloadPreservesHardlinkIdentity(t *testing.T) {
	directory := t.TempDir()
	content := []byte("downloaded hardlink group")
	anchor := makeFile("a/anchor.txt", content)
	peer := makeFile("z/peer.txt", content)
	peer.HardlinkTo = anchor.Path
	var blockGets, blockAcks int
	var actionAcks []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions"):
			session := sessionFixture([]TransferBlock{{Chunk: Chunk{SHA256: anchor.SHA256, SizeBytes: anchor.Size}, Role: "download"}}, anchor.Size,
				upsertAction(anchor), upsertAction(peer))
			session.Direction = "download"
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(session)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
			_ = json.NewEncoder(w).Encode(workspaceManifest{WorkspaceID: testWorkspace, Revision: 2,
				Files: []workspaceFile{toWorkspaceFile(anchor), toWorkspaceFile(peer)}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blocks/"+anchor.SHA256):
			blockGets++
			_, _ = w.Write(content)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blocks/"+anchor.SHA256+"/ack"):
			blockAcks++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/download-actions/ack"):
			var ack struct {
				Path string `json:"path"`
			}
			if err := json.NewDecoder(r.Body).Decode(&ack); err != nil {
				t.Fatal(err)
			}
			actionAcks = append(actionAcks, ack.Path)
			if ack.Path == peer.Path {
				anchorInfo, anchorErr := os.Stat(filepath.Join(directory, anchor.Path))
				peerInfo, peerErr := os.Stat(filepath.Join(directory, peer.Path))
				if anchorErr != nil || peerErr != nil || !os.SameFile(anchorInfo, peerInfo) {
					t.Errorf("downloaded hardlink group is not one inode: %v / %v, %v / %v", anchorInfo, peerInfo, anchorErr, peerErr)
				}
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSession+"/commit"):
			session := sessionFixture([]TransferBlock{{Chunk: Chunk{SHA256: anchor.SHA256, SizeBytes: anchor.Size}, Role: "download", Received: true}}, anchor.Size,
				appliedAction(anchor), appliedAction(peer))
			session.Direction = "download"
			session.State = "completed"
			session.ConfirmedBytes = session.PlannedBytes
			_ = json.NewEncoder(w).Encode(session)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory,
		Direction: "download", BaseRevision: int64Ptr(1)})
	if err != nil || result.Session.State != "completed" {
		t.Fatalf("Sync() result=%+v, error=%v", result, err)
	}
	anchorInfo, err := os.Stat(filepath.Join(directory, anchor.Path))
	if err != nil {
		t.Fatal(err)
	}
	peerInfo, err := os.Stat(filepath.Join(directory, peer.Path))
	if err != nil || !os.SameFile(anchorInfo, peerInfo) {
		t.Fatalf("downloaded files do not share an inode: %v / %v, %v", anchorInfo, peerInfo, err)
	}
	if blockGets != 1 || blockAcks != 1 || len(actionAcks) != 2 || actionAcks[0] != anchor.Path || actionAcks[1] != peer.Path {
		t.Fatalf("block gets=%d block acks=%d action acks=%v", blockGets, blockAcks, actionAcks)
	}
}

func TestSyncCanResumeAndSkipServerAcknowledgedUpload(t *testing.T) {
	directory := t.TempDir()
	data := []byte("source")
	if err := os.WriteFile(filepath.Join(directory, "source.txt"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	file := makeFile("source.txt", data)
	var mu sync.Mutex
	var uploads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions"):
			session := sessionFixture([]TransferBlock{{Chunk: Chunk{SHA256: file.SHA256, SizeBytes: file.Size}, Role: "upload"}}, file.Size)
			session.Direction = "upload"
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(session)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSession):
			block := TransferBlock{Chunk: Chunk{SHA256: file.SHA256, SizeBytes: file.Size}, Role: "upload", Received: true}
			session := sessionFixture([]TransferBlock{block}, file.Size)
			session.Direction = "upload"
			_ = json.NewEncoder(w).Encode(session)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/blocks/"+file.SHA256):
			mu.Lock()
			uploads++
			mu.Unlock()
			// Model a lost response after the server durably records the block.
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSession+"/commit"):
			session := sessionFixture([]TransferBlock{{Chunk: Chunk{SHA256: file.SHA256, SizeBytes: file.Size}, Role: "upload", Received: true}}, file.Size)
			session.Direction = "upload"
			session.State = "completed"
			session.ConfirmedBytes = session.PlannedBytes
			_ = json.NewEncoder(w).Encode(session)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory, Direction: "upload", BaseRevision: int64Ptr(1)})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusInternalServerError {
		t.Fatalf("first Sync() error = %v", err)
	}
	if first.Session.ID != testSession {
		t.Fatalf("failed transfer did not preserve resumable session: %+v", first.Session)
	}
	second, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory, SessionID: testSession})
	if err != nil {
		t.Fatalf("resume Sync() error = %v", err)
	}
	if second.Session.State != "completed" || uploads != 1 {
		t.Fatalf("resume state=%s upload attempts=%d", second.Session.State, uploads)
	}
}

func TestSyncRetriesQuotaRejectedCommitWithSavedKey(t *testing.T) {
	directory := t.TempDir()
	data := []byte("persist the commit key")
	if err := os.WriteFile(filepath.Join(directory, "source.txt"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	file := makeFile("source.txt", data)
	session := sessionFixture([]TransferBlock{{Chunk: Chunk{SHA256: file.SHA256, SizeBytes: file.Size}, Role: "upload", Received: true}}, file.Size)
	session.Direction = "upload"
	session.BaseRevision = 0
	session.State = "verifying"
	session.ConfirmedBytes = file.Size
	var mu sync.Mutex
	var commitKeys []string
	quotaRejectedKeys := make(map[string]bool)
	allowNewCommit := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(session)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSession):
			_ = json.NewEncoder(w).Encode(session)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSession+"/commit"):
			key := r.Header.Get("Idempotency-Key")
			mu.Lock()
			commitKeys = append(commitKeys, key)
			quotaRejected := quotaRejectedKeys[key]
			if !quotaRejected && !allowNewCommit {
				quotaRejectedKeys[key] = true
				quotaRejected = true
			}
			mu.Unlock()
			if !quotaRejected {
				committed := session
				committed.State = "completed"
				committed.Stage = "complete"
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(committed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":{"code":"quota_exceeded","message":"must not be retained"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory, Direction: "upload", BaseRevision: int64Ptr(0)})
	var firstError *HTTPError
	if !errors.As(err, &firstError) || firstError.Code != "quota_exceeded" {
		t.Fatalf("first commit error = %v, want quota_exceeded", err)
	}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := readResumeMetadata(canonical, testWorkspace)
	if err != nil || metadata.SessionID != testSession || metadata.CommitIdempotencyKey == "" || !metadata.CommitQuotaRejected {
		t.Fatalf("saved commit retry metadata = %#v, err=%v", metadata, err)
	}
	metadata.CommitQuotaRejected = false
	if err := writeResumeMetadata(canonical, metadata); err != nil {
		t.Fatal(err)
	}
	_, err = client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory,
		SessionID: testSession, RetryCommitAfterQuota: true})
	if err == nil || !strings.Contains(err.Error(), "saved quota rejection") {
		t.Fatalf("new commit key without a saved quota rejection returned %v", err)
	}
	metadata.CommitQuotaRejected = true
	if err := writeResumeMetadata(canonical, metadata); err != nil {
		t.Fatal(err)
	}
	_, err = client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory, SessionID: testSession})
	var retryError *HTTPError
	if !errors.As(err, &retryError) || retryError.Code != "quota_exceeded" {
		t.Fatalf("retried commit error = %v, want quota_exceeded", err)
	}
	mu.Lock()
	allowNewCommit = true
	mu.Unlock()
	completed, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory,
		SessionID: testSession, RetryCommitAfterQuota: true})
	if err != nil || completed.Session.State != "completed" {
		t.Fatalf("explicit quota retry result = %+v, error = %v", completed, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(commitKeys) != 3 || commitKeys[0] == "" || commitKeys[0] != commitKeys[1] || commitKeys[0] != metadata.CommitIdempotencyKey || commitKeys[2] == "" || commitKeys[2] == commitKeys[0] {
		t.Fatalf("commit retry keys = %v, saved key = %q", commitKeys, metadata.CommitIdempotencyKey)
	}
}

func TestSyncAllowsShrunkRetryAfterDefiniteCreateQuotaRejection(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "source.txt"), []byte("too large"), 0o600); err != nil {
		t.Fatal(err)
	}
	var keys []string
	var accepted SyncSession
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions"):
			var request createSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			if request.Files[0].SizeBytes > 5 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":{"code":"quota_exceeded","message":"workspace quota"}}`)
				return
			}
			file := makeFile(request.Files[0].Path, []byte("small"))
			accepted = sessionFixture([]TransferBlock{{Chunk: Chunk{SHA256: file.SHA256, SizeBytes: file.Size}, Role: "upload", Received: true}}, file.Size)
			accepted.Direction, accepted.Policy, accepted.BaseRevision, accepted.TargetRevision = "upload", "live", 0, 1
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(accepted)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSession+"/commit"):
			completed := accepted
			completed.State, completed.Stage = "completed", "complete"
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(completed)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	options := SyncOptions{WorkspaceID: testWorkspace, Directory: directory, Direction: "upload", Policy: "live",
		BaseRevision: int64Ptr(0), IdempotencyNamespace: "f02quota"}
	if _, err := client.Sync(context.Background(), options); !isDefiniteQuotaRejection(err) {
		t.Fatalf("first create error = %v, want definite quota_exceeded", err)
	}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := readResumeMetadata(canonical, testWorkspace)
	if err != nil || !rejected.CreateQuotaRejected || len(rejected.Manifest.Files) != 0 {
		t.Fatalf("metadata after definite create rejection = %#v, %v; want a minimal rejection marker", rejected, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "source.txt"), []byte("small"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := client.Sync(context.Background(), options)
	if err != nil || result.Session.State != "completed" {
		t.Fatalf("shrunk retry result = %+v, error = %v", result, err)
	}
	if len(keys) != 2 || keys[0] == "" || keys[1] == "" || keys[0] == keys[1] {
		t.Fatalf("create idempotency keys after shrink = %v, want two distinct non-empty keys", keys)
	}
}

func TestSyncRetriesQuotaRejectedCreateWithFreshKeyAfterQuotaChanges(t *testing.T) {
	directory := t.TempDir()
	contents := []byte("larger")
	if err := os.WriteFile(filepath.Join(directory, "source.txt"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	var keys []string
	quotaAvailable := false
	var accepted SyncSession
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions"):
			var request createSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode create request: %v", err)
				return
			}
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			if !quotaAvailable {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":{"code":"quota_exceeded"}}`)
				return
			}
			file := makeFile(request.Files[0].Path, contents)
			accepted = sessionFixture([]TransferBlock{{Chunk: Chunk{SHA256: file.SHA256, SizeBytes: file.Size}, Role: "upload", Received: true}}, file.Size)
			accepted.Direction, accepted.Policy, accepted.BaseRevision, accepted.TargetRevision = "upload", "live", 0, 1
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(accepted)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSession+"/commit"):
			accepted.State, accepted.Stage = "completed", "complete"
			_ = json.NewEncoder(w).Encode(accepted)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	options := SyncOptions{WorkspaceID: testWorkspace, Directory: directory, Direction: "upload", Policy: "live",
		BaseRevision: int64Ptr(0), IdempotencyNamespace: "f02quota"}
	if _, err := client.Sync(context.Background(), options); !isDefiniteQuotaRejection(err) {
		t.Fatalf("first create error = %v, want quota_exceeded", err)
	}
	quotaAvailable = true
	result, err := client.Sync(context.Background(), options)
	if err != nil || result.Session.State != "completed" {
		t.Fatalf("same-manifest retry = %+v, error = %v", result, err)
	}
	if len(keys) != 2 || keys[0] == "" || keys[1] == "" || keys[0] == keys[1] {
		t.Fatalf("create idempotency keys after quota change = %v, want a fresh key", keys)
	}
}

func TestSyncResumesAppliedFileAfterActionAckFailure(t *testing.T) {
	directory := t.TempDir()
	remote := makeFile("remote.txt", []byte("download once"))
	var mu sync.Mutex
	var blockGets, blockAcks, actionAcks int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions"):
			session := sessionFixture([]TransferBlock{{Chunk: Chunk{SHA256: remote.SHA256, SizeBytes: remote.Size}, Role: "download"}}, remote.Size, upsertAction(remote))
			session.Direction = "download"
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(session)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
			_ = json.NewEncoder(w).Encode(workspaceManifest{WorkspaceID: testWorkspace, Revision: 2, Files: []workspaceFile{toWorkspaceFile(remote)}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSession):
			block := TransferBlock{Chunk: Chunk{SHA256: remote.SHA256, SizeBytes: remote.Size}, Role: "download", Received: true}
			session := sessionFixture([]TransferBlock{block}, remote.Size, upsertAction(remote))
			session.Direction = "download"
			_ = json.NewEncoder(w).Encode(session)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blocks/"+remote.SHA256):
			mu.Lock()
			blockGets++
			mu.Unlock()
			_, _ = w.Write([]byte("download once"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blocks/"+remote.SHA256+"/ack"):
			mu.Lock()
			blockAcks++
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/download-actions/ack"):
			mu.Lock()
			actionAcks++
			current := actionAcks
			mu.Unlock()
			if current == 1 {
				http.Error(w, "ack response lost", http.StatusServiceUnavailable)
				return
			}
			assertLocalFile(t, directory, "remote.txt", "download once")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSession+"/commit"):
			action := appliedAction(remote)
			session := sessionFixture([]TransferBlock{{Chunk: Chunk{SHA256: remote.SHA256, SizeBytes: remote.Size}, Role: "download", Received: true}}, remote.Size, action)
			session.Direction = "download"
			session.State = "completed"
			session.ConfirmedBytes = session.PlannedBytes
			_ = json.NewEncoder(w).Encode(session)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory, Direction: "download", BaseRevision: int64Ptr(1)})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusServiceUnavailable || first.Session.ID != testSession {
		t.Fatalf("first Sync() result=%+v error=%v", first, err)
	}
	assertLocalFile(t, directory, "remote.txt", "download once")
	second, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory, SessionID: testSession})
	if err != nil || second.Session.State != "completed" {
		t.Fatalf("resume Sync() result=%+v error=%v", second, err)
	}
	if blockGets != 1 || blockAcks != 1 || actionAcks != 2 {
		t.Fatalf("block gets=%d block ACKs=%d action ACKs=%d", blockGets, blockAcks, actionAcks)
	}
}

func TestSyncRestoresMissingFileAfterAppliedActionOnResume(t *testing.T) {
	directory := t.TempDir()
	remote := makeFile("remote.txt", []byte("download once"))
	var actionApplied bool
	var blockReceived bool
	var actionAcks, blockGets, commits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions"):
			session := sessionFixture([]TransferBlock{{Chunk: Chunk{SHA256: remote.SHA256, SizeBytes: remote.Size}, Role: "download", Received: blockReceived}}, remote.Size, upsertAction(remote))
			session.Direction = "download"
			session.BaseRevision = 0
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(session)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSession):
			action := upsertAction(remote)
			action.Applied = actionApplied
			session := sessionFixture([]TransferBlock{{Chunk: Chunk{SHA256: remote.SHA256, SizeBytes: remote.Size}, Role: "download", Received: blockReceived}}, remote.Size, action)
			session.Direction = "download"
			session.BaseRevision = 0
			_ = json.NewEncoder(w).Encode(session)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
			_ = json.NewEncoder(w).Encode(workspaceManifest{WorkspaceID: testWorkspace, Revision: 2, Files: []workspaceFile{toWorkspaceFile(remote)}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blocks/"+remote.SHA256):
			blockGets++
			_, _ = w.Write([]byte("download once"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blocks/"+remote.SHA256+"/ack"):
			blockReceived = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/download-actions/ack"):
			actionApplied = true
			actionAcks++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSession+"/commit"):
			commits++
			if commits == 1 {
				http.Error(w, "commit response lost", http.StatusServiceUnavailable)
				return
			}
			session := sessionFixture([]TransferBlock{{Chunk: Chunk{SHA256: remote.SHA256, SizeBytes: remote.Size}, Role: "download", Received: true}}, remote.Size, appliedAction(remote))
			session.Direction = "download"
			session.State = "completed"
			session.ConfirmedBytes = session.PlannedBytes
			_ = json.NewEncoder(w).Encode(session)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory, Direction: "download", BaseRevision: int64Ptr(0)})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusServiceUnavailable || first.Session.ID != testSession {
		t.Fatalf("first Sync() result=%+v error=%v", first, err)
	}
	if err := os.Remove(filepath.Join(directory, remote.Path)); err != nil {
		t.Fatal(err)
	}

	resumed, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory, SessionID: testSession})
	if err != nil || resumed.Session.State != "completed" {
		t.Fatalf("resume Sync() result=%+v error=%v", resumed, err)
	}
	assertLocalFile(t, directory, remote.Path, "download once")
	if blockGets != 2 || actionAcks != 2 || commits != 2 {
		t.Fatalf("block gets=%d action ACKs=%d commits=%d", blockGets, actionAcks, commits)
	}
}

func TestSyncNeverOverwritesDestinationConflict(t *testing.T) {
	directory := t.TempDir()
	remote := makeFile("existing.txt", []byte("remote contents"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions"):
			// This file appears after Scan but before the transfer begins.
			if err := os.WriteFile(filepath.Join(directory, "existing.txt"), []byte("keep local"), 0o644); err != nil {
				t.Fatal(err)
			}
			session := sessionFixture([]TransferBlock{{Chunk: Chunk{SHA256: remote.SHA256, SizeBytes: remote.Size}, Role: "download"}}, remote.Size, upsertAction(remote))
			session.Direction = "download"
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(session)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
			_ = json.NewEncoder(w).Encode(workspaceManifest{WorkspaceID: testWorkspace, Revision: 2, Files: []workspaceFile{toWorkspaceFile(remote)}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSession+"/commit"):
			t.Error("must not commit an incomplete transfer after a concurrent conflict")
			http.Error(w, "unexpected", http.StatusInternalServerError)
		case strings.Contains(r.URL.Path, "/blocks/"):
			t.Errorf("must not transfer or acknowledge a conflicting file: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory, BaseRevision: int64Ptr(0), Direction: "download"})
	if err == nil || result.Session.ID != testSession || len(result.Conflicts) != 1 {
		t.Fatalf("Sync() result=%+v error=%v", result, err)
	}
	assertLocalFile(t, directory, "existing.txt", "keep local")
}

func TestSyncContinuesIndependentDownloadsAfterApplyConflict(t *testing.T) {
	directory := t.TempDir()
	conflicted := makeFile("race.txt", []byte("remote version"))
	safe := makeFile("safe.txt", []byte("safe remote version"))
	var safeActionAcknowledged bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions"):
			session := sessionFixture([]TransferBlock{
				{Chunk: Chunk{SHA256: conflicted.SHA256, SizeBytes: conflicted.Size}, Role: "download"},
				{Chunk: Chunk{SHA256: safe.SHA256, SizeBytes: safe.Size}, Role: "download"},
			}, conflicted.Size+safe.Size, upsertAction(conflicted), upsertAction(safe))
			session.Direction = "download"
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(session)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
			_ = json.NewEncoder(w).Encode(workspaceManifest{WorkspaceID: testWorkspace, Revision: 2,
				Files: []workspaceFile{toWorkspaceFile(conflicted), toWorkspaceFile(safe)}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blocks/"+conflicted.SHA256):
			// Race a local write after ApplyMissing has created its staging file.
			if err := os.WriteFile(filepath.Join(directory, conflicted.Path), []byte("keep local"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte("remote version"))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blocks/"+safe.SHA256):
			_, _ = w.Write([]byte("safe remote version"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blocks/"+safe.SHA256+"/ack"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/download-actions/ack"):
			var ack struct {
				Path string `json:"path"`
			}
			if err := json.NewDecoder(r.Body).Decode(&ack); err != nil {
				t.Fatal(err)
			}
			if ack.Path != safe.Path {
				t.Errorf("unexpected action acknowledgement for conflicted file %q", ack.Path)
			}
			safeActionAcknowledged = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSession+"/commit"):
			t.Error("must not commit while a file conflict remains")
			http.Error(w, "unexpected commit", http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory,
		BaseRevision: int64Ptr(0), Direction: "download"})
	if err == nil || !strings.Contains(err.Error(), conflicted.Path) || result.Session.ID != testSession || len(result.Conflicts) != 1 || result.Conflicts[0].Path != conflicted.Path {
		t.Fatalf("Sync() result=%+v error=%v", result, err)
	}
	if !safeActionAcknowledged {
		t.Fatal("independent file was not acknowledged after the earlier conflict")
	}
	assertLocalFile(t, directory, conflicted.Path, "keep local")
	assertLocalFile(t, directory, safe.Path, "safe remote version")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".morde-sync-tmp-") {
			t.Fatalf("failed atomic apply left a staging file %q", entry.Name())
		}
	}
}

func TestSyncStopsIndependentDownloadsOnAuthorizationFailure(t *testing.T) {
	directory := t.TempDir()
	first := makeFile("first.txt", []byte("first remote"))
	second := makeFile("second.txt", []byte("second remote"))
	secondBlockRequested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions"):
			session := sessionFixture([]TransferBlock{
				{Chunk: Chunk{SHA256: first.SHA256, SizeBytes: first.Size}, Role: "download"},
				{Chunk: Chunk{SHA256: second.SHA256, SizeBytes: second.Size}, Role: "download"},
			}, first.Size+second.Size, upsertAction(first), upsertAction(second))
			session.Direction = "download"
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(session)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
			_ = json.NewEncoder(w).Encode(workspaceManifest{WorkspaceID: testWorkspace, Revision: 2,
				Files: []workspaceFile{toWorkspaceFile(first), toWorkspaceFile(second)}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blocks/"+first.SHA256):
			http.Error(w, "private authorization detail", http.StatusUnauthorized)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blocks/"+second.SHA256):
			secondBlockRequested = true
			_, _ = w.Write([]byte("second remote"))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory,
		BaseRevision: int64Ptr(0), Direction: "download"})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusUnauthorized || result.Session.ID != testSession {
		t.Fatalf("Sync() result=%+v error=%v", result, err)
	}
	if secondBlockRequested {
		t.Fatal("download continued after an authorization failure")
	}
	if strings.Contains(err.Error(), "private authorization detail") {
		t.Fatalf("error leaked response body: %v", err)
	}
}

func TestMustStopDownloadPreservesCancellationAndRemoteErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !mustStopDownload(ctx, errors.New("local filesystem error")) {
		t.Fatal("cancelled sync must stop even when the current operation reports a local error")
	}
	remoteErr := &remoteDownloadError{err: &HTTPError{Method: http.MethodGet, Status: http.StatusUnauthorized}}
	if !mustStopDownload(context.Background(), remoteErr) {
		t.Fatal("remote authorization failures must stop the download")
	}
	if mustStopDownload(context.Background(), errors.New("local filesystem error")) {
		t.Fatal("a path-local error must be eligible for independent-path recovery")
	}
}

func TestRequestJSONPreservesOnlyStructuredAPIErrorCode(t *testing.T) {
	tests := []struct {
		name  string
		code  string
		large bool
		want  string
	}{
		{name: "quota code", code: "quota_exceeded", want: "quota_exceeded"},
		{name: "invalid code", code: "private server detail"},
		{name: "oversized error", code: "quota_exceeded", large: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				body := fmt.Sprintf(`{"error":{"code":%q,"message":"private server detail"}}`, test.code)
				if test.large {
					body = `{"error":{"code":"quota_exceeded","message":"` + strings.Repeat("x", maxErrorJSONBytes) + `"}}`
				}
				_, _ = fmt.Fprint(w, body)
			}))
			defer server.Close()
			client, err := New(server.URL, "test-token", nil)
			if err != nil {
				t.Fatal(err)
			}
			err = client.requestJSON(context.Background(), http.MethodPost, "/sync-sessions/test/commit", "key", nil, http.StatusOK, &struct{}{})
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) || httpErr.Status != http.StatusConflict || httpErr.Code != test.want {
				t.Fatalf("request error = %#v; want status %d and code %q", err, http.StatusConflict, test.want)
			}
			if test.want != "" && !strings.Contains(err.Error(), test.want) {
				t.Fatalf("request error omitted structured code %q: %v", test.want, err)
			}
			if strings.Contains(err.Error(), "private server detail") {
				t.Fatalf("request error leaked response message: %v", err)
			}
		})
	}
}

func TestSyncRejectsDeleteActionsWithoutChangingLocalFile(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "keep.txt"), []byte("local version"), 0o644); err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := mordesync.Scan(context.Background(), directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeResumeMetadata(canonical, resumeMetadata{Version: 1, WorkspaceID: testWorkspace, Directory: canonical,
		SessionID: testSession, Direction: "download", BaseRevision: 1, IdempotencyKey: "delete-test", Scope: []string{}, Manifest: manifest}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSession) {
			t.Errorf("unexpected request %s %s; delete action must fail before transfer", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		session := sessionFixture(nil, 0, DownloadAction{Path: "keep.txt", Action: "delete", SizeBytes: 0})
		session.Direction = "download"
		_ = json.NewEncoder(w).Encode(session)
	}))
	defer server.Close()
	client, err := New(server.URL, "test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory, SessionID: testSession})
	if err == nil || !strings.Contains(err.Error(), "unexpectedly contains a delete action") || result.Session.ID != testSession {
		t.Fatalf("Sync() result=%+v error=%v", result, err)
	}
	assertLocalFile(t, directory, "keep.txt", "local version")
}

func TestSyncRejectsMalformedDigestAndReportsHTTPStatus(t *testing.T) {
	directory := t.TempDir()
	t.Run("malformed transfer digest", func(t *testing.T) {
		manifest, err := mordesync.Scan(context.Background(), directory, nil)
		if err != nil {
			t.Fatal(err)
		}
		stateDirectory, err := canonicalDirectory(directory)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeResumeMetadata(stateDirectory, resumeMetadata{Version: 1, WorkspaceID: testWorkspace, Directory: stateDirectory,
			SessionID: testSession, Direction: "upload", BaseRevision: 1, IdempotencyKey: "malformed-key", Scope: []string{}, Manifest: manifest}); err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
				return
			}
			session := sessionFixture([]TransferBlock{{Chunk: Chunk{SHA256: "BAD", SizeBytes: 3}, Role: "upload"}}, 3)
			session.Direction = "upload"
			_ = json.NewEncoder(w).Encode(session)
		}))
		defer server.Close()
		client, _ := New(server.URL, "test-token", nil)
		result, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory, SessionID: testSession})
		if err == nil || result.Session.ID != testSession || !strings.Contains(err.Error(), "invalid transfer block") {
			t.Fatalf("Sync() result=%+v error=%v", result, err)
		}
	})
	t.Run("server error", func(t *testing.T) {
		directory := t.TempDir()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/sync-sessions") {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			http.Error(w, "private server detail", http.StatusServiceUnavailable)
		}))
		defer server.Close()
		client, _ := New(server.URL, "do-not-leak-this-token", nil)
		result, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory, BaseRevision: int64Ptr(0)})
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.Status != http.StatusServiceUnavailable || result.Session.ID != "" {
			t.Fatalf("Sync() result=%+v error=%v", result, err)
		}
		if strings.Contains(err.Error(), "do-not-leak-this-token") || strings.Contains(err.Error(), "private server detail") {
			t.Fatalf("error leaked sensitive data: %v", err)
		}
	})
}

func sessionFixture(blocks []TransferBlock, plannedBytes int64, actions ...DownloadAction) SyncSession {
	return SyncSession{
		ID: testSession, WorkspaceID: testWorkspace, Direction: "bidirectional", Policy: "fill_missing", State: "planned",
		BaseRevision: 1, TargetRevision: 2, Stage: "compare", PlannedBytes: plannedBytes, TransferBlocks: blocks,
		DownloadActions: append([]DownloadAction{}, actions...), Conflicts: []Conflict{},
	}
}

func upsertAction(file mordesync.File) DownloadAction {
	digest := file.SHA256
	return DownloadAction{Path: file.Path, Action: "upsert", SHA256: &digest, SizeBytes: file.Size}
}

func appliedAction(file mordesync.File) DownloadAction {
	action := upsertAction(file)
	action.Applied = true
	return action
}

func makeFile(name string, data []byte) mordesync.File {
	digest := sha256.Sum256(data)
	file := mordesync.File{Path: name, Size: int64(len(data)), Mode: 0o644, SHA256: hex.EncodeToString(digest[:])}
	if len(data) > 0 {
		file.Blocks = []mordesync.Block{{Offset: 0, Size: int64(len(data)), SHA256: file.SHA256}}
	}
	return file
}

func assertLocalFile(t *testing.T, directory, name, expected string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != expected {
		t.Fatalf("%s = %q, want %q", name, data, expected)
	}
}

func int64Ptr(value int64) *int64 { return &value }
