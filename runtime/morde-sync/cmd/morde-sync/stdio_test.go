package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"morde-sync/client"
)

const (
	testWorkspaceID = "wabc1234"
	testSessionID   = "pabc1234"
	testBearerToken = "secret-bearer-token-never-print"
)

func TestStdioSyncEmitsProgressAndResultWithoutLeakingSecrets(t *testing.T) {
	directory := t.TempDir()
	content := []byte("private source contents")
	if err := os.WriteFile(filepath.Join(directory, "main.go"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256(content)
	digest := hex.EncodeToString(digestBytes[:])
	var uploaded atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testBearerToken {
			t.Errorf("Authorization header = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
			writeJSON(t, w, http.StatusOK, map[string]any{
				"workspace_id": testWorkspaceID, "revision": 0, "files": []any{}, "next_cursor": nil,
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions"):
			writeJSON(t, w, http.StatusCreated, sessionFixture("planned", "upload", []transferFixture{
				{Chunk: chunkFixture{SHA256: digest, SizeBytes: int64(len(content))}, Role: "upload"},
			}, int64(len(content)), 0))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/blocks/"+digest):
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read upload: %v", err)
			}
			if !bytes.Equal(body, content) {
				t.Errorf("uploaded content = %q", body)
			}
			uploaded.Store(true)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSessionID+"/commit"):
			writeJSON(t, w, http.StatusOK, sessionFixture("completed", "upload", []transferFixture{
				{Chunk: chunkFixture{SHA256: digest, SizeBytes: int64(len(content))}, Role: "upload", Received: true},
			}, int64(len(content)), int64(len(content))))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	request := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "sync",
		"params": map[string]any{
			"base_url": server.URL, "bearer_token": testBearerToken, "workspace_id": testWorkspaceID,
			"directory": directory, "direction": "upload",
		},
	}
	input := marshalLine(t, request)
	reader, writer := io.Pipe()
	output := &lineSignalWriter{lines: make(chan string, 64)}
	var diagnostics bytes.Buffer
	runDone := make(chan error, 1)
	go func() { runDone <- run(reader, output, &diagnostics) }()
	if _, err := fmt.Fprintln(writer, string(input)); err != nil {
		t.Fatal(err)
	}
	response := waitForResponseID(t, output.lines, "1")
	if response.Error != nil {
		t.Fatalf("sync response = %+v", response)
	}
	_ = writer.Close()
	if err := <-runDone; err != nil {
		t.Fatalf("run() error = %v; stderr=%s", err, diagnostics.String())
	}
	if !uploaded.Load() {
		t.Fatal("sync did not upload the workspace file")
	}
	if strings.Contains(output.String(), testBearerToken) || strings.Contains(output.String(), string(content)) {
		t.Fatalf("protocol output leaked a credential or file contents: %s", output.String())
	}
	messages := decodeMessages(t, output.String())
	progressCount := 0
	gotResult := false
	gotPlan := false
	for _, message := range messages {
		if message.Method == "progress" {
			progressCount++
			var params progressParams
			if err := json.Unmarshal(message.Params, &params); err != nil {
				t.Fatal(err)
			}
			if string(params.RequestID) != "1" {
				t.Fatalf("progress params = %+v", params)
			}
			if params.Stage == "inventory" || params.Stage == "hash" || params.Stage == "planning" {
				if params.SessionID != "" {
					t.Fatalf("scan progress unexpectedly has a session ID: %+v", params)
				}
				if params.Stage == "planning" && (params.FileCount != 1 || params.TotalBytes != int64(len(content)) || params.FilesProcessed != 1 || params.BytesProcessed != int64(len(content))) {
					t.Fatalf("planning progress params = %+v", params)
				}
				gotPlan = gotPlan || params.Stage == "planning"
			} else if params.SessionID != testSessionID {
				t.Fatalf("transfer progress session ID = %q", params.SessionID)
			}
			continue
		}
		if string(message.ID) == "1" && len(message.Result) > 0 {
			var result syncResult
			if err := json.Unmarshal(message.Result, &result); err != nil {
				t.Fatal(err)
			}
			if result.Session.ID != testSessionID || result.Session.State != "completed" || result.Conflicts == nil {
				t.Fatalf("sync result = %+v", result)
			}
			gotResult = true
		}
	}
	if progressCount < 3 || !gotPlan || !gotResult {
		t.Fatalf("progress count=%d plan=%t result=%t output=%s", progressCount, gotPlan, gotResult, output.String())
	}
}

func TestStdioRejectsInvalidSyncParametersWithoutEchoingToken(t *testing.T) {
	const secret = "do-not-echo-this-token"
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"unknown","method":"sync","params":{"base_url":"http://127.0.0.1:1","bearer_token":"` + secret + `","workspace_id":"wabc1234","directory":"/tmp","direction":"upload","unexpected":true}}`,
		`{"jsonrpc":"2.0","id":"missing","method":"sync","params":{"base_url":"http://127.0.0.1:1","bearer_token":"` + secret + `","workspace_id":"wabc1234","direction":"upload"}}`,
	}, "\n") + "\n"
	var output, diagnostics bytes.Buffer
	if err := run(strings.NewReader(input), &output, &diagnostics); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if strings.Contains(output.String(), secret) || strings.Contains(diagnostics.String(), secret) {
		t.Fatalf("invalid request echoed bearer token: stdout=%s stderr=%s", output.String(), diagnostics.String())
	}
	messages := decodeMessages(t, output.String())
	if len(messages) != 2 {
		t.Fatalf("responses = %d; output=%s", len(messages), output.String())
	}
	for _, message := range messages {
		if message.Error == nil || message.Error.Code != -32602 {
			t.Fatalf("invalid input response = %+v", message)
		}
	}
}

func TestStdioListsAndRestoresPreservedCopy(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "workspace")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	const preservedID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const originalContent = "previous local version"
	entry := filepath.Join(parent, ".morde-preserved", testWorkspaceID, preservedID)
	if err := os.MkdirAll(entry, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "content"), []byte(originalContent), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := struct {
		ID          string    `json:"id"`
		WorkspaceID string    `json:"workspace_id"`
		Path        string    `json:"path"`
		CreatedAt   time.Time `json:"created_at"`
	}{ID: preservedID, WorkspaceID: testWorkspaceID, Path: "notes.txt", CreatedAt: time.Now().UTC()}
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "metadata.json"), metadataBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	listed := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "list", "method": "list_preserved",
		"params": map[string]any{"directory": directory, "workspace_id": testWorkspaceID},
	})
	if listed.Error != nil {
		t.Fatalf("list response = %+v", listed)
	}
	var items []client.PreservedFile
	if err := json.Unmarshal(listed.Result, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != preservedID || items[0].Path != "notes.txt" || items[0].Size != int64(len(originalContent)) {
		t.Fatalf("preserved copies = %#v", items)
	}

	restored := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "restore", "method": "restore_preserved",
		"params": map[string]any{"directory": directory, "workspace_id": testWorkspaceID, "id": preservedID},
	})
	if restored.Error != nil {
		t.Fatalf("restore response = %+v", restored)
	}
	var result struct {
		Restored bool `json:"restored"`
	}
	if err := json.Unmarshal(restored.Result, &result); err != nil || !result.Restored {
		t.Fatalf("restore result = %s, err=%v", restored.Result, err)
	}
	content, err := os.ReadFile(filepath.Join(directory, "notes.txt"))
	if err != nil || string(content) != originalContent {
		t.Fatalf("restored file = %q, err=%v", content, err)
	}
	if _, err := os.Stat(filepath.Join(entry, "content")); err != nil {
		t.Fatalf("recovery copy was removed after restore: %v", err)
	}

	conflict := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "conflict", "method": "restore_preserved",
		"params": map[string]any{"directory": directory, "workspace_id": testWorkspaceID, "id": preservedID},
	})
	if conflict.Error == nil || conflict.Error.Code != -32021 {
		t.Fatalf("restore over an existing destination = %+v", conflict)
	}
	contentAfterConflict, err := os.ReadFile(filepath.Join(directory, "notes.txt"))
	if err != nil || string(contentAfterConflict) != originalContent {
		t.Fatalf("existing destination changed: %q, err=%v", contentAfterConflict, err)
	}

	deleted := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "delete", "method": "delete_preserved",
		"params": map[string]any{"directory": directory, "workspace_id": testWorkspaceID, "id": preservedID},
	})
	if deleted.Error != nil {
		t.Fatalf("delete response = %+v", deleted)
	}
	var deleteResult struct {
		Deleted bool `json:"deleted"`
	}
	if err := json.Unmarshal(deleted.Result, &deleteResult); err != nil || !deleteResult.Deleted {
		t.Fatalf("delete result = %s, err=%v", deleted.Result, err)
	}
	items, err = client.ListPreserved(context.Background(), directory, testWorkspaceID)
	if err != nil || len(items) != 0 {
		t.Fatalf("recovery copies after deletion = %#v, %v", items, err)
	}
	missing := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "delete-missing", "method": "delete_preserved",
		"params": map[string]any{"directory": directory, "workspace_id": testWorkspaceID, "id": preservedID},
	})
	if missing.Error == nil || missing.Error.Code != -32022 {
		t.Fatalf("delete missing copy = %+v", missing)
	}
}

func TestStdioListsRestoresAndDeletesSyncthingPreservedCopy(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "workspace")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	const folderID = "morde-0123456789abcdef0123456789abcdef"
	const entryID = "AbcdefghijklmnopQRST"
	const relativePath = "docs/notes.txt"
	const content = "from the F02 node"
	folderHash := sha256.Sum256([]byte(folderID))
	folderRoot := filepath.Join(parent, ".morde-preserved", hex.EncodeToString(folderHash[:]))
	entryRoot := filepath.Join(folderRoot, entryID)
	if err := os.MkdirAll(filepath.Join(entryRoot, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entryRoot, "docs", "notes.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := map[string]any{
		"version": 1, "entry_id": entryID, "folder_id": folderID, "path": relativePath,
		"created_at": time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC),
	}
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folderRoot, entryID+".morde.json"), metadataBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(folderID + "\x00" + entryID + "\x00" + relativePath))
	preservedID := hex.EncodeToString(digest[:16])
	params := map[string]any{"directory": directory, "workspace_id": testWorkspaceID, "folder_id": folderID}
	listed := invokeRPC(t, map[string]any{"jsonrpc": "2.0", "id": "list-f02", "method": "list_preserved", "params": params})
	if listed.Error != nil {
		t.Fatalf("F02 list = %+v", listed)
	}
	var items []client.PreservedFile
	if err := json.Unmarshal(listed.Result, &items); err != nil || len(items) != 1 || items[0].ID != preservedID || items[0].Path != relativePath || items[0].Size != int64(len(content)) {
		t.Fatalf("F02 preserved copies = %#v, err=%v", items, err)
	}
	params["id"] = preservedID
	restored := invokeRPC(t, map[string]any{"jsonrpc": "2.0", "id": "restore-f02", "method": "restore_preserved", "params": params})
	if restored.Error != nil {
		t.Fatalf("F02 restore = %+v", restored)
	}
	if value, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(relativePath))); err != nil || string(value) != content {
		t.Fatalf("restored F02 file = %q, %v", value, err)
	}
	deleted := invokeRPC(t, map[string]any{"jsonrpc": "2.0", "id": "delete-f02", "method": "delete_preserved", "params": params})
	if deleted.Error != nil {
		t.Fatalf("F02 delete = %+v", deleted)
	}
	if _, err := os.Stat(entryRoot); !os.IsNotExist(err) {
		t.Fatalf("F02 archive still exists after delete: %v", err)
	}
}

func TestStdioRejectsInvalidPreservedRequestsWithoutEchoingLocalData(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private-local-directory")
	const privateMarker = "private-content-marker"
	const secret = "bearer-token-must-not-be-accepted"
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"bad-id","method":"restore_preserved","params":{"directory":"` + directory + `","workspace_id":"` + testWorkspaceID + `","id":"../notes.txt"}}`,
		`{"jsonrpc":"2.0","id":"bad-folder","method":"list_preserved","params":{"directory":"` + directory + `","workspace_id":"` + testWorkspaceID + `","folder_id":"../private"}}`,
		`{"jsonrpc":"2.0","id":"extra","method":"list_preserved","params":{"directory":"` + directory + `","workspace_id":"` + testWorkspaceID + `","bearer_token":"` + secret + `"}}`,
		`{"jsonrpc":"2.0","id":"delete-bad-id","method":"delete_preserved","params":{"directory":"` + directory + `","workspace_id":"` + testWorkspaceID + `","id":"../notes.txt"}}`,
	}, "\n") + "\n"
	var output, diagnostics bytes.Buffer
	if err := run(strings.NewReader(input), &output, &diagnostics); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if strings.Contains(output.String(), directory) || strings.Contains(output.String(), secret) ||
		strings.Contains(output.String(), privateMarker) || strings.Contains(diagnostics.String(), secret) {
		t.Fatalf("invalid local recovery request echoed data: stdout=%s stderr=%s", output.String(), diagnostics.String())
	}
	messages := decodeMessages(t, output.String())
	if len(messages) != 4 {
		t.Fatalf("responses = %d; output=%s", len(messages), output.String())
	}
	for _, message := range messages {
		if message.Error == nil || message.Error.Code != -32602 {
			t.Fatalf("invalid recovery request response = %+v", message)
		}
	}
}

func TestStdioDeletePreservedRejectsUnsafeDirectoryWithoutFollowingIt(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "workspace")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	const preservedID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	target := t.TempDir()
	entry := filepath.Join(target, testWorkspaceID, preservedID)
	if err := os.MkdirAll(entry, 0o700); err != nil {
		t.Fatal(err)
	}
	const outsideContent = "outside recovery data"
	if err := os.WriteFile(filepath.Join(entry, "content"), []byte(outsideContent), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := struct {
		ID          string    `json:"id"`
		WorkspaceID string    `json:"workspace_id"`
		Path        string    `json:"path"`
		CreatedAt   time.Time `json:"created_at"`
	}{ID: preservedID, WorkspaceID: testWorkspaceID, Path: "notes.txt", CreatedAt: time.Now().UTC()}
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "metadata.json"), metadataBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(parent, ".morde-preserved")); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}

	response := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "delete-unsafe", "method": "delete_preserved",
		"params": map[string]any{"directory": directory, "workspace_id": testWorkspaceID, "id": preservedID},
	})
	if response.Error == nil || response.Error.Code != -32022 {
		t.Fatalf("delete through unsafe directory = %+v", response)
	}
	if strings.Contains(string(response.Error.Message), parent) || strings.Contains(string(response.Error.Message), outsideContent) {
		t.Fatalf("error exposed local directory or contents: %+v", response.Error)
	}
	if content, err := os.ReadFile(filepath.Join(entry, "content")); err != nil || string(content) != outsideContent {
		t.Fatalf("outside recovery copy changed: %q, %v", content, err)
	}
}

func invokeRPC(t *testing.T, request map[string]any) decodedMessage {
	t.Helper()
	input := append(marshalLine(t, request), '\n')
	var output, diagnostics bytes.Buffer
	if err := run(bytes.NewReader(input), &output, &diagnostics); err != nil {
		t.Fatalf("run() error = %v; stderr=%s", err, diagnostics.String())
	}
	messages := decodeMessages(t, output.String())
	if len(messages) != 1 {
		t.Fatalf("expected one response; output=%s", output.String())
	}
	return messages[0]
}

func TestStdioHubCommandRejectsInvalidBoundsAndStaleWorkspace(t *testing.T) {
	command := strings.Repeat("x", (8<<10)+1)
	invalid := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "invalid-command", "method": "hub_command",
		"params": map[string]any{
			"directory": t.TempDir(), "identity": strings.Repeat("a", 64), "command": command, "timeout_ms": 1000,
		},
	})
	if invalid.Error == nil || invalid.Error.Code != -32602 || strings.Contains(string(invalid.Error.Message), command) {
		t.Fatalf("invalid command response = %+v", invalid)
	}

	directory := t.TempDir()
	selection, err := client.SelectHubRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	stale := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "stale-command", "method": "hub_command",
		"params": map[string]any{
			"directory": selection.Directory, "identity": strings.Repeat("0", 64), "command": "true", "timeout_ms": 1000,
		},
	})
	if stale.Error == nil || stale.Error.Code != -32030 || strings.Contains(string(stale.Error.Message), directory) {
		t.Fatalf("stale command workspace response = %+v", stale)
	}
}

func TestStdioHubCommandProbeReturnsOnlyAvailability(t *testing.T) {
	response := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "command-probe", "method": "hub_command_probe", "params": map[string]any{},
	})
	if response.Error != nil {
		t.Fatalf("command probe response = %+v", response)
	}
	var result hubCommandProbeResult
	if err := json.Unmarshal(response.Result, &result); err != nil || len(response.Result) == 0 {
		t.Fatalf("command probe result = %s, err=%v", response.Result, err)
	}
	if bytes.Contains(response.Result, []byte("command")) {
		t.Fatalf("command probe exposed unexpected data: %s", response.Result)
	}
}

func TestStdioHubInspectionRoutesAcrossFreshProcesses(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "docs", "readme.txt"), []byte("local-only text"), 0o600); err != nil {
		t.Fatal(err)
	}
	selected := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "root", "method": "hub_root",
		"params": map[string]any{"directory": directory},
	})
	if selected.Error != nil {
		t.Fatalf("hub_root response = %+v", selected)
	}
	var root client.HubRootSelection
	if err := json.Unmarshal(selected.Result, &root); err != nil || root.Identity == "" {
		t.Fatalf("hub_root result = %+v, err=%v", root, err)
	}
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil || root.Directory != canonical {
		t.Fatalf("hub_root path = %q, want canonical %q; err=%v", root.Directory, canonical, err)
	}
	listed := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "files", "method": "hub_files",
		"params": map[string]any{"directory": root.Directory, "identity": root.Identity, "path": "docs"},
	})
	if listed.Error != nil {
		t.Fatalf("hub_files response = %+v", listed)
	}
	var files hubFilesResult
	if err := json.Unmarshal(listed.Result, &files); err != nil || len(files.Entries) != 1 || files.Entries[0].Path != "docs/readme.txt" {
		t.Fatalf("hub_files result = %+v, err=%v", files, err)
	}
	read := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "read", "method": "hub_file_get",
		"params": map[string]any{"directory": root.Directory, "identity": root.Identity, "path": "docs/readme.txt", "max_bytes": 32},
	})
	if read.Error != nil {
		t.Fatalf("hub_file_get response = %+v", read)
	}
	var content client.HubFileContent
	if err := json.Unmarshal(read.Result, &content); err != nil || content.Text != "local-only text" || content.Truncated {
		t.Fatalf("hub_file_get result = %+v, err=%v", content, err)
	}
}

func TestStdioHubInspectionRejectsTraversalUnknownFieldsAndRootReplacement(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "selected")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	selected := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "root", "method": "hub_root",
		"params": map[string]any{"directory": directory},
	})
	if selected.Error != nil {
		t.Fatalf("hub_root response = %+v", selected)
	}
	var root client.HubRootSelection
	if err := json.Unmarshal(selected.Result, &root); err != nil {
		t.Fatal(err)
	}
	unsafe := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "traversal", "method": "hub_file_get",
		"params": map[string]any{"directory": root.Directory, "identity": root.Identity, "path": "../outside", "max_bytes": 8},
	})
	if unsafe.Error == nil {
		t.Fatal("hub_file_get accepted a traversal path")
	}
	unknown := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "unknown", "method": "hub_files",
		"params": map[string]any{"directory": root.Directory, "identity": root.Identity, "unexpected": true},
	})
	if unknown.Error == nil || unknown.Error.Code != -32602 {
		t.Fatalf("hub_files accepted an unknown field: %+v", unknown)
	}
	if err := os.Rename(directory, filepath.Join(parent, "old-selected")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "replacement-secret"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	replaced := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "replaced", "method": "hub_files",
		"params": map[string]any{"directory": directory, "identity": root.Identity},
	})
	if replaced.Error == nil || strings.Contains(replaced.Error.Message, "replacement-secret") || strings.Contains(replaced.Error.Message, parent) {
		t.Fatalf("hub_files accepted/revealed a replaced root: %+v", replaced)
	}
}

func TestStdioHubInspectionBoundsTextAndRejectsInvalidUTF8(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "text.txt"), []byte("ab界tail"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "binary.txt"), []byte{'a', 0xff, 'b'}, 0o600); err != nil {
		t.Fatal(err)
	}
	selected := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "root", "method": "hub_root",
		"params": map[string]any{"directory": directory},
	})
	if selected.Error != nil {
		t.Fatalf("hub_root response = %+v", selected)
	}
	var root client.HubRootSelection
	if err := json.Unmarshal(selected.Result, &root); err != nil {
		t.Fatal(err)
	}
	truncated := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "text", "method": "hub_file_get",
		"params": map[string]any{"directory": root.Directory, "identity": root.Identity, "path": "text.txt", "max_bytes": 4},
	})
	if truncated.Error != nil {
		t.Fatalf("hub_file_get response = %+v", truncated)
	}
	var textValue client.HubFileContent
	if err := json.Unmarshal(truncated.Result, &textValue); err != nil || textValue.Text != "ab" || !textValue.Truncated {
		t.Fatalf("truncated result = %+v, err=%v", textValue, err)
	}
	binary := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "binary", "method": "hub_file_get",
		"params": map[string]any{"directory": root.Directory, "identity": root.Identity, "path": "binary.txt", "max_bytes": 8},
	})
	if binary.Error == nil {
		t.Fatal("hub_file_get accepted invalid UTF-8")
	}
}

func TestStdioHubFileSnapshotAndChunkTransfersLargeBinaryFiles(t *testing.T) {
	directory := t.TempDir()
	content := make([]byte, client.HubInspectMaxBytes+73)
	for index := range content {
		content[index] = byte(index % 251)
	}
	if err := os.WriteFile(filepath.Join(directory, "document.pdf"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	selected := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "root", "method": "hub_root",
		"params": map[string]any{"directory": directory},
	})
	if selected.Error != nil {
		t.Fatalf("hub_root response = %+v", selected)
	}
	var root client.HubRootSelection
	if err := json.Unmarshal(selected.Result, &root); err != nil {
		t.Fatal(err)
	}
	snapshotResponse := invokeRPC(t, map[string]any{
		"jsonrpc": "2.0", "id": "snapshot", "method": "hub_file_snapshot",
		"params": map[string]any{"directory": root.Directory, "identity": root.Identity, "path": "document.pdf"},
	})
	if snapshotResponse.Error != nil || bytes.Contains(snapshotResponse.Result, []byte(directory)) {
		t.Fatalf("hub_file_snapshot response = %+v", snapshotResponse)
	}
	var snapshot client.HubFileSnapshot
	if err := json.Unmarshal(snapshotResponse.Result, &snapshot); err != nil || snapshot.SizeBytes != int64(len(content)) {
		t.Fatalf("hub_file_snapshot result = %+v, err=%v", snapshot, err)
	}

	var reconstructed []byte
	for offset := int64(0); offset < snapshot.SizeBytes; {
		chunkResponse := invokeRPC(t, map[string]any{
			"jsonrpc": "2.0", "id": fmt.Sprintf("chunk-%d", offset), "method": "hub_file_chunk",
			"params": map[string]any{"directory": root.Directory, "identity": root.Identity, "path": snapshot.Path,
				"version": snapshot.Version, "offset": offset, "max_bytes": 8 << 10},
		})
		if chunkResponse.Error != nil || bytes.Contains(chunkResponse.Result, []byte(directory)) {
			t.Fatalf("hub_file_chunk response exposed data/path or failed: %+v", chunkResponse)
		}
		var chunk client.HubFileChunk
		if err := json.Unmarshal(chunkResponse.Result, &chunk); err != nil || chunk.Offset != offset || len(chunk.Data) == 0 {
			t.Fatalf("hub_file_chunk result = %+v, err=%v", chunk, err)
		}
		if !bytes.Contains(chunkResponse.Result, []byte(`"data":"`+base64.StdEncoding.EncodeToString(chunk.Data)+`"`)) {
			t.Fatalf("hub_file_chunk data was not JSON base64: %s", chunkResponse.Result)
		}
		chunkDigest := sha256.Sum256(chunk.Data)
		if chunk.SHA256 != hex.EncodeToString(chunkDigest[:]) {
			t.Fatalf("chunk digest = %q, want %s", chunk.SHA256, hex.EncodeToString(chunkDigest[:]))
		}
		reconstructed = append(reconstructed, chunk.Data...)
		offset += int64(len(chunk.Data))
	}
	wholeDigest := sha256.Sum256(reconstructed)
	if !bytes.Equal(reconstructed, content) || hex.EncodeToString(wholeDigest[:]) != snapshot.SHA256 {
		t.Fatal("stdio file chunks did not reconstruct the snapshotted bytes")
	}
}

func TestStdioQuotaRetryRequiresSessionID(t *testing.T) {
	params := syncParams{BaseURL: "http://127.0.0.1", BearerToken: testBearerToken,
		WorkspaceID: testWorkspaceID, Directory: "/tmp/workspace", Direction: "upload", RetryCommitAfterQuota: true}
	if err := validateSyncParams(params); err == nil {
		t.Fatal("quota retry without a session ID was accepted")
	}
	params.SessionID = testSessionID
	if err := validateSyncParams(params); err != nil {
		t.Fatalf("quota retry with a session ID was rejected: %v", err)
	}
}

func TestStdioTaskSyncRequiresBoundUpload(t *testing.T) {
	baseRevision := int64(3)
	params := syncParams{BaseURL: "http://127.0.0.1", BearerToken: testBearerToken,
		WorkspaceID: testWorkspaceID, Directory: "/tmp/workspace", Direction: "upload", Policy: "compare_merge",
		BaseRevision: &baseRevision, Scope: []string{"reports/out.txt"}, TaskID: "qabc1234",
		TaskHubOperationKey: "agent:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if err := validateSyncParams(params); err != nil {
		t.Fatalf("valid Task sync parameters rejected: %v", err)
	}
	params.TaskHubOperationKey = ""
	if err := validateSyncParams(params); err == nil {
		t.Fatal("Task sync accepted a missing operation key")
	}
	params.TaskHubOperationKey = "agent:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	params.Policy = "fill_missing"
	if err := validateSyncParams(params); err == nil {
		t.Fatal("Task sync accepted a non-conflict-detecting policy")
	}
	params.SessionID, params.Policy, params.BaseRevision, params.Scope = testSessionID, "", nil, nil
	if err := validateSyncParams(params); err != nil {
		t.Fatalf("saved Task sync could not be resumed: %v", err)
	}
}

func TestStdioSyncSurfacesStructuredQuotaErrorCode(t *testing.T) {
	directory := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprint(w, `{"error":{"code":"quota_exceeded","message":"private server detail"}}`)
	}))
	defer server.Close()
	input := marshalLine(t, map[string]any{
		"jsonrpc": "2.0", "id": "quota-sync", "method": "sync",
		"params": map[string]any{
			"base_url": server.URL, "bearer_token": testBearerToken, "workspace_id": testWorkspaceID,
			"directory": directory, "direction": "upload", "base_revision": 0,
		},
	})
	reader, writer := io.Pipe()
	output := &lineSignalWriter{lines: make(chan string, 16)}
	var diagnostics bytes.Buffer
	runDone := make(chan error, 1)
	go func() { runDone <- run(reader, output, &diagnostics) }()
	if _, err := fmt.Fprintln(writer, string(input)); err != nil {
		t.Fatal(err)
	}
	response := waitForResponseID(t, output.lines, `"quota-sync"`)
	if response.Error == nil || !strings.Contains(response.Error.Message, "quota_exceeded") {
		t.Fatalf("quota sync response = %+v; want structured quota code", response)
	}
	if strings.Contains(response.Error.Message, "private server detail") || strings.Contains(output.String(), testBearerToken) {
		t.Fatalf("quota sync response leaked private data: %s", output.String())
	}
	_ = writer.Close()
	if err := <-runDone; err != nil {
		t.Fatalf("run() error = %v; stderr=%s", err, diagnostics.String())
	}
}

func TestStdioHandoffCheckpointVerifiesAndEmitsSafeProgress(t *testing.T) {
	directory := t.TempDir()
	createKeys := make(chan string, 2)
	server := newStdioLiveSyncServer(t, func(key string) { createKeys <- key })
	defer server.Close()

	input := marshalLine(t, map[string]any{"jsonrpc": "2.0", "id": "handoff-1", "method": "handoff_checkpoint",
		"params": map[string]any{"base_url": server.URL, "bearer_token": testBearerToken, "workspace_id": testWorkspaceID, "directory": directory,
			"idempotency_key": "9fc6eec8-3fe8-4cde-881d-05e5f82cf6b4"}})
	reader, writer := io.Pipe()
	output := &lineSignalWriter{lines: make(chan string, 64)}
	var diagnostics bytes.Buffer
	runDone := make(chan error, 1)
	go func() { runDone <- run(reader, output, &diagnostics) }()
	if _, err := fmt.Fprintln(writer, string(input)); err != nil {
		t.Fatal(err)
	}
	response := waitForResponseID(t, output.lines, `"handoff-1"`)
	if response.Error != nil {
		t.Fatalf("handoff response = %+v", response)
	}
	_ = writer.Close()
	if err := <-runDone; err != nil {
		t.Fatalf("run() error = %v; stderr=%s", err, diagnostics.String())
	}
	if strings.Contains(output.String(), testBearerToken) || strings.Contains(output.String(), directory) {
		t.Fatalf("handoff protocol output leaked a token or local path: %s", output.String())
	}
	var gotProgress, gotResult bool
	for _, message := range decodeMessages(t, output.String()) {
		if message.Method == "progress" {
			var params progressParams
			if err := json.Unmarshal(message.Params, &params); err != nil {
				t.Fatal(err)
			}
			if string(params.RequestID) == `"handoff-1"` {
				if params.CurrentPath != "" {
					t.Fatalf("handoff progress exposed a path: %+v", params)
				}
				gotProgress = true
			}
			continue
		}
		if string(message.ID) == `"handoff-1"` && len(message.Result) > 0 {
			var result handoffCheckpointResult
			if err := json.Unmarshal(message.Result, &result); err != nil {
				t.Fatal(err)
			}
			if result.Revision != 0 {
				t.Fatalf("handoff revision = %d, want 0", result.Revision)
			}
			gotResult = true
		}
	}
	if !gotProgress || !gotResult {
		t.Fatalf("handoff progress=%t result=%t output=%s", gotProgress, gotResult, output.String())
	}
	select {
	case key := <-createKeys:
		if !strings.HasPrefix(key, "9fc6eec8-3fe8-4cde-881d-05e5f82cf6b4.") {
			t.Fatalf("server create key %q does not carry the handoff identity", key)
		}
	default:
		t.Fatal("handoff did not create a server sync session")
	}
}

func TestStdioHandoffCheckpointRejectsInvalidParametersAndActiveLiveSync(t *testing.T) {
	directory := t.TempDir()
	server := newStdioLiveSyncServer(t)
	defer server.Close()

	var invalidOutput, invalidDiagnostics bytes.Buffer
	invalidInput := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"bad-scope","method":"handoff_checkpoint","params":{"base_url":"` + server.URL + `","bearer_token":"` + testBearerToken + `","workspace_id":"` + testWorkspaceID + `","directory":"` + directory + `","scope":["../escape"],"idempotency_key":"handoff-1"}}`,
		`{"jsonrpc":"2.0","id":"bad-field","method":"handoff_checkpoint","params":{"base_url":"` + server.URL + `","bearer_token":"` + testBearerToken + `","workspace_id":"` + testWorkspaceID + `","directory":"` + directory + `","extra":true}}`,
		`{"jsonrpc":"2.0","id":"bad-key","method":"handoff_checkpoint","params":{"base_url":"` + server.URL + `","bearer_token":"` + testBearerToken + `","workspace_id":"` + testWorkspaceID + `","directory":"` + directory + `","idempotency_key":"bad key"}}`,
	}, "\n") + "\n"
	if err := run(strings.NewReader(invalidInput), &invalidOutput, &invalidDiagnostics); err != nil {
		t.Fatalf("invalid parameter run() error = %v", err)
	}
	for _, message := range decodeMessages(t, invalidOutput.String()) {
		if message.Error == nil || message.Error.Code != -32602 {
			t.Fatalf("invalid handoff parameters response = %+v", message)
		}
	}
	if strings.Contains(invalidOutput.String(), testBearerToken) || strings.Contains(invalidOutput.String(), directory) {
		t.Fatalf("invalid handoff request leaked sensitive values: %s", invalidOutput.String())
	}

	reader, writer := io.Pipe()
	output := &lineSignalWriter{lines: make(chan string, 64)}
	var diagnostics bytes.Buffer
	runDone := make(chan error, 1)
	go func() { runDone <- run(reader, output, &diagnostics) }()
	liveParams := map[string]any{"base_url": server.URL, "bearer_token": testBearerToken, "workspace_id": testWorkspaceID,
		"directory": directory, "rescan_seconds": 5}
	if _, err := fmt.Fprintln(writer, string(marshalLine(t, map[string]any{"jsonrpc": "2.0", "id": "live-lock", "method": "live_sync", "params": liveParams}))); err != nil {
		t.Fatal(err)
	}
	waitForProgressStage(t, output.lines, "watching")
	handoffParams := map[string]any{"base_url": server.URL, "bearer_token": testBearerToken, "workspace_id": testWorkspaceID,
		"directory": directory, "idempotency_key": "9fc6eec8-3fe8-4cde-881d-05e5f82cf6b4"}
	if _, err := fmt.Fprintln(writer, string(marshalLine(t, map[string]any{"jsonrpc": "2.0", "id": "handoff-lock", "method": "handoff_checkpoint", "params": handoffParams}))); err != nil {
		t.Fatal(err)
	}
	message := waitForResponseID(t, output.lines, `"handoff-lock"`)
	if message.Error == nil || message.Error.Code != -32001 {
		t.Fatalf("handoff concurrent-operation response = %+v", message)
	}
	if _, err := fmt.Fprintln(writer, string(marshalLine(t, map[string]any{"jsonrpc": "2.0", "id": "cancel-live-lock", "method": "cancel",
		"params": map[string]any{"request_id": "live-lock"}}))); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run() error = %v; stderr=%s", err, diagnostics.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active live sync did not stop after cancel")
	}
}

func TestStdioHandoffCheckpointFailureDoesNotExposeTokenOrPath(t *testing.T) {
	directory := t.TempDir()
	const secret = "handoff-secret-never-echo"
	input := marshalLine(t, map[string]any{"jsonrpc": "2.0", "id": "handoff-fail", "method": "handoff_checkpoint",
		"params": map[string]any{"base_url": "http://127.0.0.1:1", "bearer_token": secret, "workspace_id": testWorkspaceID, "directory": directory,
			"idempotency_key": "9fc6eec8-3fe8-4cde-881d-05e5f82cf6b4"}})
	var output, diagnostics bytes.Buffer
	if err := run(bytes.NewReader(append(input, '\n')), &output, &diagnostics); err != nil {
		t.Fatalf("run() error = %v; stderr=%s", err, diagnostics.String())
	}
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), directory) || strings.Contains(diagnostics.String(), directory) {
		t.Fatalf("handoff failure leaked sensitive values: stdout=%s stderr=%s", output.String(), diagnostics.String())
	}
	messages := decodeMessages(t, output.String())
	if len(messages) < 2 || messages[len(messages)-1].Error == nil || messages[len(messages)-1].Error.Code != -32010 {
		t.Fatalf("handoff failure response = %#v", messages)
	}
}

func newStdioLiveSyncServer(t *testing.T, onCreate ...func(string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testBearerToken {
			t.Errorf("Authorization header = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
			writeJSON(t, w, http.StatusOK, map[string]any{"workspace_id": testWorkspaceID, "revision": 0, "files": []any{}, "next_cursor": nil})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions"):
			if len(onCreate) != 0 {
				onCreate[0](r.Header.Get("Idempotency-Key"))
			}
			writeJSON(t, w, http.StatusCreated, sessionFixture("planned", "bidirectional", []transferFixture{}, 0, 0))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSessionID+"/commit"):
			writeJSON(t, w, http.StatusOK, sessionFixture("completed", "bidirectional", []transferFixture{}, 0, 0))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/changes"):
			writeJSON(t, w, http.StatusOK, map[string]any{"items": []any{}, "next_cursor": r.URL.Query().Get("after")})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
}

func seedStdioHandoffBaseline(t *testing.T, baseURL, directory string) {
	t.Helper()
	reader, writer := io.Pipe()
	output := &lineSignalWriter{lines: make(chan string, 64)}
	var diagnostics bytes.Buffer
	runDone := make(chan error, 1)
	go func() { runDone <- run(reader, output, &diagnostics) }()
	params := map[string]any{"base_url": baseURL, "bearer_token": testBearerToken, "workspace_id": testWorkspaceID, "directory": directory, "rescan_seconds": 5}
	if _, err := fmt.Fprintln(writer, string(marshalLine(t, map[string]any{"jsonrpc": "2.0", "id": "seed-live", "method": "live_sync", "params": params}))); err != nil {
		t.Fatal(err)
	}
	waitForProgressStage(t, output.lines, "watching")
	if _, err := fmt.Fprintln(writer, string(marshalLine(t, map[string]any{"jsonrpc": "2.0", "id": "seed-cancel", "method": "cancel",
		"params": map[string]any{"request_id": "seed-live"}}))); err != nil {
		t.Fatal(err)
	}
	message := waitForResponseID(t, output.lines, `"seed-cancel"`)
	var result cancelResult
	if err := json.Unmarshal(message.Result, &result); err != nil || !result.Cancelled {
		t.Fatalf("seed live cancellation = %+v err=%v", message, err)
	}
	_ = writer.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("seed live run() error = %v; stderr=%s", err, diagnostics.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("seed live sync did not stop")
	}
}

func TestStdioCancelStopsSyncAndConfirmsServerCancellation(t *testing.T) {
	directory := t.TempDir()
	commitStarted := make(chan struct{})
	var commitOnce sync.Once
	var cancelCalled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
			writeJSON(t, w, http.StatusOK, map[string]any{
				"workspace_id": testWorkspaceID, "revision": 0, "files": []any{}, "next_cursor": nil,
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions"):
			writeJSON(t, w, http.StatusCreated, sessionFixture("planned", "upload", []transferFixture{}, 0, 0))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSessionID+"/commit"):
			commitOnce.Do(func() { close(commitStarted) })
			<-r.Context().Done()
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSessionID+"/cancel"):
			cancelCalled.Store(true)
			writeJSON(t, w, http.StatusOK, sessionFixture("cancelled", "upload", []transferFixture{}, 0, 0))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	reader, writer := io.Pipe()
	var output, diagnostics bytes.Buffer
	runDone := make(chan error, 1)
	go func() { runDone <- run(reader, &output, &diagnostics) }()

	params := map[string]any{
		"base_url": server.URL, "bearer_token": testBearerToken, "workspace_id": testWorkspaceID,
		"directory": directory, "direction": "upload",
	}
	if _, err := fmt.Fprintln(writer, string(marshalLine(t, map[string]any{"jsonrpc": "2.0", "id": "sync-1", "method": "sync", "params": params}))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-commitStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("sync did not reach its commit request")
	}
	if _, err := fmt.Fprintln(writer, string(marshalLine(t, map[string]any{"jsonrpc": "2.0", "id": "sync-2", "method": "sync", "params": params}))); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(writer, string(marshalLine(t, map[string]any{
		"jsonrpc": "2.0", "id": "cancel-wrong", "method": "cancel", "params": map[string]any{"request_id": "other-sync"},
	}))); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(writer, string(marshalLine(t, map[string]any{
		"jsonrpc": "2.0", "id": "cancel-1", "method": "cancel", "params": map[string]any{"request_id": "sync-1"},
	}))); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run() error = %v; stderr=%s", err, diagnostics.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not finish")
	}
	if !cancelCalled.Load() {
		t.Fatal("server cancel endpoint was not called")
	}
	if strings.Contains(output.String(), testBearerToken) {
		t.Fatalf("protocol output leaked bearer token: %s", output.String())
	}
	messages := decodeMessages(t, output.String())
	var gotCancel, gotSecondSyncError, gotWrongCancelError bool
	for _, message := range messages {
		switch string(message.ID) {
		case `"cancel-1"`:
			var result cancelResult
			if err := json.Unmarshal(message.Result, &result); err != nil || !result.Cancelled || result.Session.State != "cancelled" {
				t.Fatalf("cancel response = %+v err=%v", message, err)
			}
			gotCancel = true
		case `"sync-2"`:
			if message.Error == nil || message.Error.Code != -32001 {
				t.Fatalf("second sync response = %+v", message)
			}
			gotSecondSyncError = true
		case `"cancel-wrong"`:
			if message.Error == nil || message.Error.Code != -32004 {
				t.Fatalf("wrong cancel response = %+v", message)
			}
			gotWrongCancelError = true
		}
	}
	if !gotCancel || !gotSecondSyncError || !gotWrongCancelError {
		t.Fatalf("cancel=%t second sync rejected=%t wrong cancel rejected=%t messages=%s", gotCancel, gotSecondSyncError, gotWrongCancelError, output.String())
	}
}

func TestNormalizeLiveProgressStage(t *testing.T) {
	for input, want := range map[string]string{
		"watch": "watching", "changes": "watching", "sync_planning": "scan", "planning": "scan",
		"compare": "scan", "transfer": "transfer", "verify": "transfer", "apply": "transfer",
		"rescan_fallback": "reconnect", "retry": "reconnect", "sync": "scan",
	} {
		if got := normalizeLiveProgressStage("running", input); got != want {
			t.Errorf("normalizeLiveProgressStage(%q) = %q, want %q", input, got, want)
		}
	}
	if got := normalizeLiveProgressStage("conflict", "complete"); got != "conflict" {
		t.Fatalf("conflict stage = %q", got)
	}
}

func TestStdioCancelStopsIdleLiveWatcherWithoutServerCancel(t *testing.T) {
	directory := t.TempDir()
	var cancelCalled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
			writeJSON(t, w, http.StatusOK, map[string]any{"workspace_id": testWorkspaceID, "revision": 0, "files": []any{}, "next_cursor": nil})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions"):
			writeJSON(t, w, http.StatusCreated, sessionFixture("planned", "bidirectional", []transferFixture{}, 0, 0))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSessionID+"/commit"):
			writeJSON(t, w, http.StatusOK, sessionFixture("completed", "bidirectional", []transferFixture{}, 0, 0))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/changes"):
			<-r.Context().Done()
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSessionID+"/cancel"):
			cancelCalled.Store(true)
			writeJSON(t, w, http.StatusOK, sessionFixture("cancelled", "bidirectional", []transferFixture{}, 0, 0))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	reader, writer := io.Pipe()
	output := &lineSignalWriter{lines: make(chan string, 64)}
	var diagnostics bytes.Buffer
	runDone := make(chan error, 1)
	go func() { runDone <- run(reader, output, &diagnostics) }()
	params := map[string]any{"base_url": server.URL, "bearer_token": testBearerToken, "workspace_id": testWorkspaceID,
		"directory": directory, "rescan_seconds": 5}
	if _, err := fmt.Fprintln(writer, string(marshalLine(t, map[string]any{"jsonrpc": "2.0", "id": "live-1", "method": "live_sync", "params": params}))); err != nil {
		t.Fatal(err)
	}
	waitForProgressStage(t, output.lines, "watching")
	if _, err := fmt.Fprintln(writer, string(marshalLine(t, map[string]any{"jsonrpc": "2.0", "id": "cancel-1", "method": "cancel",
		"params": map[string]any{"request_id": "live-1"}}))); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run() error = %v; stderr=%s", err, diagnostics.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle live watcher did not stop within the deadline")
	}
	if cancelCalled.Load() {
		t.Fatal("idle watcher attempted to cancel its already completed session")
	}
	var cancelled bool
	for _, message := range decodeMessages(t, output.String()) {
		if string(message.ID) == `"cancel-1"` && len(message.Result) > 0 {
			var result cancelResult
			if err := json.Unmarshal(message.Result, &result); err != nil {
				t.Fatal(err)
			}
			cancelled = result.Cancelled
		}
	}
	if !cancelled {
		t.Fatalf("idle cancel did not confirm local stop: %s", output.String())
	}
}

func TestStdioEOFCancelsActiveLiveSession(t *testing.T) {
	directory := t.TempDir()
	content := []byte("live upload")
	if err := os.WriteFile(filepath.Join(directory, "main.go"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256(content)
	digest := hex.EncodeToString(digestBytes[:])
	commitStarted := make(chan struct{})
	var commitOnce sync.Once
	var cancelCalled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
			writeJSON(t, w, http.StatusOK, map[string]any{"workspace_id": testWorkspaceID, "revision": 0, "files": []any{}, "next_cursor": nil})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions"):
			writeJSON(t, w, http.StatusCreated, sessionFixture("planned", "bidirectional", []transferFixture{
				{Chunk: chunkFixture{SHA256: digest, SizeBytes: int64(len(content))}, Role: "upload"},
			}, int64(len(content)), 0))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/blocks/"+digest):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSessionID+"/commit"):
			commitOnce.Do(func() { close(commitStarted) })
			<-r.Context().Done()
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSessionID+"/cancel"):
			cancelCalled.Store(true)
			writeJSON(t, w, http.StatusOK, sessionFixture("cancelled", "bidirectional", []transferFixture{
				{Chunk: chunkFixture{SHA256: digest, SizeBytes: int64(len(content))}, Role: "upload", Received: true},
			}, int64(len(content)), int64(len(content))))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	reader, writer := io.Pipe()
	var output, diagnostics bytes.Buffer
	runDone := make(chan error, 1)
	go func() { runDone <- run(reader, &output, &diagnostics) }()
	params := map[string]any{"base_url": server.URL, "bearer_token": testBearerToken, "workspace_id": testWorkspaceID,
		"directory": directory, "rescan_seconds": 5}
	if _, err := fmt.Fprintln(writer, string(marshalLine(t, map[string]any{"jsonrpc": "2.0", "id": "live-eof", "method": "live_sync", "params": params}))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-commitStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("live session did not reach commit")
	}
	started := time.Now()
	_ = writer.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run() error = %v; stderr=%s", err, diagnostics.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stdin EOF did not stop and cancel the active live session promptly")
	}
	if time.Since(started) >= 5*time.Second || !cancelCalled.Load() {
		t.Fatalf("EOF shutdown elapsed=%s server_cancel=%t", time.Since(started), cancelCalled.Load())
	}
}

func TestStdioEOFCancelsActiveOneShotSync(t *testing.T) {
	directory := t.TempDir()
	content := []byte("one-shot upload")
	if err := os.WriteFile(filepath.Join(directory, "main.go"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256(content)
	digest := hex.EncodeToString(digestBytes[:])
	blockStarted := make(chan struct{})
	var blockOnce sync.Once
	var cancelCalled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
			writeJSON(t, w, http.StatusOK, map[string]any{"workspace_id": testWorkspaceID, "revision": 0, "files": []any{}, "next_cursor": nil})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions"):
			writeJSON(t, w, http.StatusCreated, sessionFixture("planned", "upload", []transferFixture{
				{Chunk: chunkFixture{SHA256: digest, SizeBytes: int64(len(content))}, Role: "upload"},
			}, int64(len(content)), 0))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/blocks/"+digest):
			blockOnce.Do(func() { close(blockStarted) })
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync-sessions/"+testSessionID+"/cancel"):
			cancelCalled.Store(true)
			writeJSON(t, w, http.StatusOK, sessionFixture("cancelled", "upload", []transferFixture{
				{Chunk: chunkFixture{SHA256: digest, SizeBytes: int64(len(content))}, Role: "upload", Received: true},
			}, int64(len(content)), int64(len(content))))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	reader, writer := io.Pipe()
	output := &lineSignalWriter{lines: make(chan string, 64)}
	var diagnostics bytes.Buffer
	runDone := make(chan error, 1)
	go func() { runDone <- run(reader, output, &diagnostics) }()
	params := map[string]any{"base_url": server.URL, "bearer_token": testBearerToken, "workspace_id": testWorkspaceID,
		"directory": directory, "direction": "upload"}
	if _, err := fmt.Fprintln(writer, string(marshalLine(t, map[string]any{"jsonrpc": "2.0", "id": "sync-eof", "method": "sync", "params": params}))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blockStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("one-shot sync did not start its block transfer")
	}
	started := time.Now()
	_ = writer.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run() error = %v; stderr=%s", err, diagnostics.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stdin EOF did not stop and cancel the active one-shot sync promptly")
	}
	if time.Since(started) >= 5*time.Second || !cancelCalled.Load() {
		t.Fatalf("EOF shutdown elapsed=%s server_cancel=%t", time.Since(started), cancelCalled.Load())
	}
	if strings.Contains(diagnostics.String(), testBearerToken) || strings.Contains(diagnostics.String(), directory) {
		t.Fatalf("EOF shutdown diagnostics leaked sensitive values: %s", diagnostics.String())
	}
}

func TestStdioRejectsOversizedLine(t *testing.T) {
	input := strings.Repeat("x", maxRequestLine+1) + "\n"
	var output, diagnostics bytes.Buffer
	if err := run(strings.NewReader(input), &output, &diagnostics); err == nil {
		t.Fatal("run accepted an oversized line")
	}
	messages := decodeMessages(t, output.String())
	if len(messages) != 1 || messages[0].Error == nil || messages[0].Error.Code != -32700 {
		t.Fatalf("oversized line response = %#v", messages)
	}
}

type transferFixture struct {
	Chunk    chunkFixture `json:"chunk"`
	Role     string       `json:"role"`
	Received bool         `json:"received"`
}

type chunkFixture struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type sessionWireFixture struct {
	ID              string            `json:"id"`
	WorkspaceID     string            `json:"workspace_id"`
	Direction       string            `json:"direction"`
	Policy          string            `json:"policy"`
	State           string            `json:"state"`
	BaseRevision    int64             `json:"base_revision"`
	TargetRevision  int64             `json:"target_revision"`
	Stage           string            `json:"stage"`
	PlannedBytes    int64             `json:"planned_bytes"`
	ConfirmedBytes  int64             `json:"confirmed_bytes"`
	TransferBlocks  []transferFixture `json:"transfer_blocks"`
	DownloadActions []any             `json:"download_actions"`
	Conflicts       []any             `json:"conflicts"`
}

func sessionFixture(state, direction string, blocks []transferFixture, planned, confirmed int64) sessionWireFixture {
	stage := "compare"
	if state == "cancelled" || state == "completed" {
		stage = "complete"
	}
	return sessionWireFixture{
		ID: testSessionID, WorkspaceID: testWorkspaceID, Direction: direction, Policy: "fill_missing",
		State: state, BaseRevision: 0, TargetRevision: 0, Stage: stage, PlannedBytes: planned,
		ConfirmedBytes: confirmed, TransferBlocks: blocks, DownloadActions: []any{}, Conflicts: []any{},
	}
}

type decodedMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type lineSignalWriter struct {
	mu    sync.Mutex
	data  bytes.Buffer
	lines chan string
}

func (w *lineSignalWriter) Write(value []byte) (int, error) {
	w.mu.Lock()
	n, err := w.data.Write(value)
	w.mu.Unlock()
	select {
	case w.lines <- string(value):
	default:
	}
	return n, err
}

func (w *lineSignalWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.String()
}

func waitForProgressStage(t *testing.T, lines <-chan string, stage string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-lines:
			var message decodedMessage
			if json.Unmarshal([]byte(line), &message) != nil || message.Method != "progress" {
				continue
			}
			var params progressParams
			if json.Unmarshal(message.Params, &params) == nil && params.Stage == stage {
				return
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for progress stage %q", stage)
		}
	}
}

func waitForResponseID(t *testing.T, lines <-chan string, id string) decodedMessage {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-lines:
			var message decodedMessage
			if json.Unmarshal([]byte(line), &message) == nil && string(message.ID) == id {
				return message
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for response ID %s", id)
			return decodedMessage{}
		}
	}
}

func decodeMessages(t *testing.T, value string) []decodedMessage {
	t.Helper()
	var result []decodedMessage
	scanner := bufio.NewScanner(strings.NewReader(value))
	for scanner.Scan() {
		var message decodedMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			t.Fatalf("decode protocol line %q: %v", scanner.Text(), err)
		}
		result = append(result, message)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func marshalLine(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode mock response: %v", err)
	}
}
