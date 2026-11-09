package runtimefiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"
)

func TestVerifyMatchesChangedAndMissingFilesWithoutReturningContent(t *testing.T) {
	workspace := openTestWorkspace(t)
	root := workspace.root.Name()
	if err := os.WriteFile(filepath.Join(root, "same.txt"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "changed.txt"), []byte("mutated!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "size.txt"), []byte("larger"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := handlerFor(t, workspace, "verify")
	if handler.Capability() != "file.verify" {
		t.Fatalf("verify capability = %q", handler.Capability())
	}
	args := fmt.Sprintf(`{"files":[{"path":"same.txt","size":8,"sha256":%q},{"path":"changed.txt","size":8,"sha256":%q},{"path":"size.txt","size":5,"sha256":%q},{"path":"missing.txt","size":2,"sha256":%q}]}`,
		testSHA256("original"), testSHA256("original"), testSHA256("larger"), testSHA256("no"))
	result, err := handler.Execute(context.Background(), requestFor("overi001", "file.verify", "verify", args))
	if err != nil {
		t.Fatalf("verify error = %v", err)
	}
	var response struct {
		Files []verifyFileResult `json:"files"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Files) != 4 {
		t.Fatalf("verify files = %#v", response.Files)
	}
	if got := response.Files[0]; got.Path != "same.txt" || got.Size != 8 || got.SHA256 != testSHA256("original") || !got.Matches {
		t.Errorf("matching file result = %#v", got)
	}
	if got := response.Files[1]; got.Path != "changed.txt" || got.Size != 8 || got.SHA256 != testSHA256("mutated!") || got.Matches {
		t.Errorf("changed file result = %#v", got)
	}
	if got := response.Files[2]; got.Path != "size.txt" || got.Size != 6 || got.SHA256 != testSHA256("larger") || got.Matches {
		t.Errorf("size-mismatched file result = %#v", got)
	}
	if got := response.Files[3]; got.Path != "missing.txt" || got.Size != 0 || got.SHA256 != "" || got.Matches {
		t.Errorf("missing file result = %#v", got)
	}
	if strings.Contains(string(result), "original") || strings.Contains(string(result), "mutated!") {
		t.Fatalf("verification response leaked file content: %s", result)
	}
}

func TestVerifyRejectsUnsafeAndSymlinkPaths(t *testing.T) {
	workspace := openTestWorkspace(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace.root.Name(), "escape")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	handler := handlerFor(t, workspace, "verify")
	for index, path := range []string{"../outside", "escape/secret"} {
		arguments := fmt.Sprintf(`{"files":[{"path":%q,"size":6,"sha256":%q}]}`, path, testSHA256("secret"))
		_, err := handler.Execute(context.Background(), requestFor(fmt.Sprintf("overi%03d", index+2), "file.verify", "verify", arguments))
		if !errors.Is(err, ErrUnsafePath) {
			t.Errorf("verify path %q error = %v, want ErrUnsafePath", path, err)
		}
	}
}

func TestVerifyRejectsInvalidJSONAndWorkLimits(t *testing.T) {
	workspace := openTestWorkspace(t)
	handler := handlerFor(t, workspace, "verify")
	validHash := testSHA256("")
	tooMany := make([]verifyFileArgument, maxVerifyFiles+1)
	for index := range tooMany {
		tooMany[index] = verifyFileArgument{Path: "missing", Size: 0, SHA256: validHash}
	}
	tooManyJSON, _ := json.Marshal(verifyArguments{Files: tooMany})
	tooMuchTotal := make([]verifyFileArgument, 9)
	for index := range tooMuchTotal {
		tooMuchTotal[index] = verifyFileArgument{Path: "missing", Size: maxVerifyFileBytes, SHA256: validHash}
	}
	tooMuchTotalJSON, _ := json.Marshal(verifyArguments{Files: tooMuchTotal})
	tests := []struct {
		name string
		body string
	}{
		{name: "too many files", body: string(tooManyJSON)},
		{name: "per file size", body: fmt.Sprintf(`{"files":[{"path":"missing","size":%d,"sha256":%q}]}`, maxVerifyFileBytes+1, validHash)},
		{name: "total size", body: string(tooMuchTotalJSON)},
		{name: "uppercase hash", body: fmt.Sprintf(`{"files":[{"path":"missing","size":0,"sha256":%q}]}`, strings.ToUpper(validHash))},
		{name: "unknown field", body: `{"files":[],"extra":true}`},
		{name: "duplicate key", body: fmt.Sprintf(`{"files":[{"path":"missing","path":"other","size":0,"sha256":%q}]}`, validHash)},
		{name: "trailing JSON", body: fmt.Sprintf(`{"files":[{"path":"missing","size":0,"sha256":%q}]} {}`, validHash)},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := handler.Execute(context.Background(), requestFor(fmt.Sprintf("overi%03d", index+4), "file.verify", "verify", test.body))
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("verify error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestVerifyAcceptsFileCountAndTotalSizeLimits(t *testing.T) {
	workspace := openTestWorkspace(t)
	handler := handlerFor(t, workspace, "verify")
	files := make([]verifyFileArgument, maxVerifyFiles)
	for index := range files {
		size := int64(0)
		if index < 8 {
			size = maxVerifyFileBytes
		}
		files[index] = verifyFileArgument{Path: "missing", Size: size, SHA256: testSHA256("")}
	}
	arguments, err := json.Marshal(verifyArguments{Files: files})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handler.Execute(context.Background(), requestFor("overi019", "file.verify", "verify", string(arguments)))
	if err != nil {
		t.Fatalf("verify at limits error = %v", err)
	}
	var response struct {
		Files []verifyFileResult `json:"files"`
	}
	if err := json.Unmarshal(result, &response); err != nil || len(response.Files) != maxVerifyFiles {
		t.Fatalf("verify at limits returned %d files, err=%v", len(response.Files), err)
	}
}

func TestVerifyHonorsCancellationAndReadStop(t *testing.T) {
	workspace := openTestWorkspace(t)
	handler := handlerFor(t, workspace, "verify")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	arguments := fmt.Sprintf(`{"files":[{"path":"missing","size":0,"sha256":%q}]}`, testSHA256(""))
	operation := requestFor("overi020", "file.verify", "verify", arguments)
	_, err := handler.Execute(ctx, operation)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled verify error = %v", err)
	}
	stopped, err := handler.Stop(context.Background(), contract.Operation{
		OperationID: operation.OperationID, Capability: operation.Capability, Action: operation.Action,
	})
	if err != nil || !stopped.VerifiedStopped {
		t.Fatalf("Stop after read verification = %#v err=%v, want verified stopped", stopped, err)
	}
}

func testSHA256(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func TestWorkspaceFileActionsReadWriteListFind(t *testing.T) {
	workspace := openTestWorkspace(t)
	write := handlerFor(t, workspace, "write")
	read := handlerFor(t, workspace, "read")
	list := handlerFor(t, workspace, "list")
	find := handlerFor(t, workspace, "find")

	writeResult, err := write.Execute(context.Background(), requestFor("owrite01", "file.write", "write", `{"path":"notes/todo.md","content":"first\nsecond\nthird\n"}`))
	if err == nil || !errors.Is(err, ErrUnsafePath) {
		// Writes do not create implicit directories; this prevents path typos
		// from quietly changing Workspace structure.
		t.Fatalf("write into missing parent error = %v result=%s", err, writeResult)
	}
	if err := os.Mkdir(filepath.Join(workspace.root.Name(), "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeResult, err = write.Execute(context.Background(), requestFor("owrite02", "file.write", "write", `{"path":"notes/todo.md","content":"first\nsecond\nthird\n"}`))
	if err != nil {
		t.Fatalf("write error = %v", err)
	}
	writtenInfo, err := os.Stat(filepath.Join(workspace.root.Name(), "notes/todo.md"))
	if err != nil {
		t.Fatal(err)
	}
	if writtenInfo.Mode().Perm() != 0o660 {
		t.Fatalf("written file mode = %v; want group-writable and private from other users", writtenInfo.Mode().Perm())
	}
	var written struct {
		Path         string `json:"path"`
		BytesWritten int    `json:"bytes_written"`
		SHA256       string `json:"sha256"`
	}
	if err := json.Unmarshal(writeResult, &written); err != nil || written.Path != "notes/todo.md" || written.BytesWritten != len("first\nsecond\nthird\n") {
		t.Fatalf("write result = %s, err=%v", writeResult, err)
	}
	if written.SHA256 != testSHA256("first\nsecond\nthird\n") {
		t.Fatalf("write result SHA-256 = %q", written.SHA256)
	}

	readResult, err := read.Execute(context.Background(), requestFor("oread001", "file.read", "read", `{"path":"notes/todo.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	var readValue struct {
		Path string `json:"path"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(readResult, &readValue); err != nil || readValue.Path != "notes/todo.md" || readValue.Text != "first\nsecond\nthird\n" {
		t.Fatalf("read result = %s, err=%v", readResult, err)
	}

	lineResult, err := read.Execute(context.Background(), requestFor("oread002", "file.read", "read", `{"path":"notes/todo.md","start_line":2,"end_line":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(lineResult, &readValue); err != nil || readValue.Text != "second\n" {
		t.Fatalf("line-range read = %s, err=%v", lineResult, err)
	}

	listResult, err := list.Execute(context.Background(), requestFor("olist001", "file.list", "list", `{"path":"notes"}`))
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Path    string          `json:"path"`
		Entries []entryMetadata `json:"entries"`
	}
	if err := json.Unmarshal(listResult, &listed); err != nil || listed.Path != "notes" || len(listed.Entries) != 1 || listed.Entries[0].Name != "todo.md" || listed.Entries[0].Kind != "file" {
		t.Fatalf("list result = %s, err=%v", listResult, err)
	}

	findResult, err := find.Execute(context.Background(), requestFor("ofind001", "file.find", "find", `{"path":".","query":"todo"}`))
	if err != nil {
		t.Fatal(err)
	}
	var found struct {
		Matches []entryMetadata `json:"matches"`
	}
	if err := json.Unmarshal(findResult, &found); err != nil || len(found.Matches) != 1 || found.Matches[0].Name != "todo.md" {
		t.Fatalf("find result = %s, err=%v", findResult, err)
	}
}

func TestRejectTraversalAndAbsolutePaths(t *testing.T) {
	workspace := openTestWorkspace(t)
	if err := os.WriteFile(filepath.Join(workspace.root.Name(), "safe.txt"), []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := handlerFor(t, workspace, "read")
	write := handlerFor(t, workspace, "write")
	for index, unsafe := range []string{"../outside", "nested/../../outside", "/etc/passwd", `C:\Windows\win.ini`, `notes\..\outside`} {
		operationID := fmt.Sprintf("oread00%d", index+1)
		_, err := read.Execute(context.Background(), requestFor(operationID, "file.read", "read", `{"path":`+mustJSON(unsafe)+`}`))
		if !errors.Is(err, ErrUnsafePath) {
			t.Errorf("read path %q error = %v, want ErrUnsafePath", unsafe, err)
		}
		writeID := fmt.Sprintf("owrite%02d", index+1)
		_, err = write.Execute(context.Background(), requestFor(writeID, "file.write", "write", `{"path":`+mustJSON(unsafe)+`,"content":"bad"}`))
		if !errors.Is(err, ErrUnsafePath) {
			t.Errorf("write path %q error = %v, want ErrUnsafePath", unsafe, err)
		}
	}
}

func TestRejectSymlinkEscape(t *testing.T) {
	workspace := openTestWorkspace(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace.root.Name(), "escape")); err != nil {
		t.Fatalf("create test symlink: %v", err)
	}
	read := handlerFor(t, workspace, "read")
	write := handlerFor(t, workspace, "write")
	for _, test := range []struct {
		id         string
		path       string
		wantAction string
	}{
		{id: "oread010", path: "escape/secret.txt", wantAction: "read"},
		{id: "oread011", path: "escape", wantAction: "read"},
	} {
		_, err := read.Execute(context.Background(), requestFor(test.id, "file.read", test.wantAction, `{"path":`+mustJSON(test.path)+`}`))
		if !errors.Is(err, ErrUnsafePath) && !errors.Is(err, ErrFileUnavailable) {
			t.Errorf("read symlink path %q error = %v", test.path, err)
		}
	}
	_, err := write.Execute(context.Background(), requestFor("owrite20", "file.write", "write", `{"path":"escape/changed.txt","content":"owned"}`))
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("write through symlink error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "changed.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside target exists after rejected write: %v", err)
	}
	data, err := os.ReadFile(secret)
	if err != nil || string(data) != "external" {
		t.Fatalf("outside secret changed: %q err=%v", data, err)
	}
}

func TestWorkspaceRootRejectsDirectoryReplacementBetweenCheckAndOpen(t *testing.T) {
	workspace := openTestWorkspace(t)
	rootPath := workspace.root.Name()
	workspaceID := "w1234567"
	workspacePath := filepath.Join(rootPath, workspaceID)
	siblingPath := filepath.Join(rootPath, "sibling")
	replacedPath := filepath.Join(rootPath, "replaced")
	for _, path := range []string{workspacePath, siblingPath} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := workspace.root.Lstat(workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(workspacePath, replacedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(siblingPath, workspacePath); err != nil {
		t.Fatal(err)
	}
	if _, err := openWorkspaceRoot(workspace.root, workspaceID, expected); !errors.Is(err, ErrFileUnavailable) {
		t.Fatalf("opening a replacement directory error=%v, want ErrFileUnavailable", err)
	}
}

func TestOversizeReadPersistsResultTooLargeCode(t *testing.T) {
	workspace := openTestWorkspace(t)
	workspacePath := filepath.Join(workspace.root.Name(), "w1234567")
	if err := os.Mkdir(workspacePath, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspacePath, "large.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", runtimecore.MaxFileReadResultBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, fence, viewID, _ := openAIFileManager(t, workspace)
	operation, err := manager.Submit(context.Background(), contract.ExecuteRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: "qabc1234", OperationID: "oreadb01",
		WorkspaceID: "w1234567", BindingVersion: 1, WorkspaceViewID: viewID, WorkspaceCheckpointRevision: 0,
		LeaseVersion: 1, Capability: "file.read", Action: "read", Arguments: json.RawMessage(`{"path":"large.txt"}`),
	})
	if err != nil {
		t.Fatalf("Submit error = %v", err)
	}
	query := contract.OperationQuery{HostFence: fence, TaskID: "qabc1234", LeaseVersion: 1}
	deadline := time.Now().Add(time.Second)
	for operation.Status == contract.OperationRunning && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		operation, err = manager.QueryOperation(contract.ProtocolVersion, "oreadb01", query)
		if err != nil {
			t.Fatal(err)
		}
	}
	var result struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(operation.Result, &result); err != nil || operation.Status != contract.OperationFailed || result.Code != runtimecore.ErrResultTooLarge.Code {
		t.Fatalf("oversize operation status=%q result=%s err=%v", operation.Status, operation.Result, err)
	}
}

func TestRuntimeManagerRestoresTaskWorkspaceViewAfterRestart(t *testing.T) {
	rootPath, taskRootPath := t.TempDir(), filepath.Join(t.TempDir(), "task-views")
	sourcePath := filepath.Join(rootPath, "w1234567")
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "input.txt"), []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := OpenTaskWorkspace(rootPath, taskRootPath, uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workspace.Close() })
	manager, fence, viewID, storeDir := openAIFileManager(t, workspace)
	write, err := manager.Submit(context.Background(), contract.ExecuteRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: "qabc1234", OperationID: "owrit001",
		WorkspaceID: "w1234567", BindingVersion: 1, WorkspaceViewID: viewID, WorkspaceCheckpointRevision: 0,
		LeaseVersion: 1, Capability: "file.write", Action: "write", Arguments: json.RawMessage(`{"path":"result.txt","content":"published"}`),
	})
	if err != nil {
		t.Fatalf("submit private Task file write: %v", err)
	}
	query := contract.OperationQuery{HostFence: fence, TaskID: "qabc1234", LeaseVersion: 1}
	deadline := time.Now().Add(3 * time.Second)
	for write.Status == contract.OperationRunning && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		write, err = manager.QueryOperation(contract.ProtocolVersion, write.OperationID, query)
		if err != nil {
			t.Fatalf("query private Task file write: %v", err)
		}
	}
	if write.Status != contract.OperationSucceeded {
		t.Fatalf("private Task file write status = %q, result=%s", write.Status, write.Result)
	}
	_, inputManifestSHA, err := scanTaskWorkspace(context.Background(), sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	viewRequest := contract.SetTaskWorkspaceViewRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: "qabc1234", WorkspaceID: "w1234567",
		BindingVersion: 1, InputRevision: 0, WorkspaceCheckpointRevision: 1, ManifestSHA256: inputManifestSHA,
	}
	// Model a lost checkpoint response: the Task bytes persist while Runtime metadata remains at revision 0.
	if err := manager.Close(); err != nil {
		t.Fatalf("stop Runtime before restart: %v", err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatalf("close Task Workspace before restart: %v", err)
	}

	workspace, err = OpenTaskWorkspace(rootPath, taskRootPath, uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatalf("reopen Task Workspace: %v", err)
	}
	t.Cleanup(func() { _ = workspace.Close() })
	manager, err = runtimecore.Open(runtimecore.Options{
		StoreDir: storeDir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: "habc1234", Generation: 2,
		Handlers: workspace.Handlers(), TaskWorkspaceViews: workspace,
	})
	if err != nil {
		t.Fatalf("restart Runtime manager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	capabilities, err := manager.Capabilities(contract.CapabilitiesRequest{
		ProtocolVersion: contract.ProtocolVersion, HostID: "habc1234", RuntimeGeneration: 2,
	})
	if err != nil {
		t.Fatalf("read restarted Runtime capabilities: %v", err)
	}
	newFence := contract.HostFence{
		HostID: capabilities.HostID, RuntimeGeneration: capabilities.RuntimeGeneration,
		RuntimeInstanceID: capabilities.RuntimeInstanceID, ControlVersion: capabilities.ControlVersion,
	}
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: newFence, Mode: contract.ModeAI}); err != nil {
		t.Fatalf("enable restarted Runtime: %v", err)
	}
	if _, err := manager.SetWorkspaceBindings(context.Background(), contract.SetWorkspaceBindingsRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: newFence,
		Bindings: []contract.WorkspaceBinding{{WorkspaceID: "w1234567", BindingVersion: 1, Directory: "w1234567"}},
	}); err != nil {
		t.Fatalf("restore Workspace binding after Runtime restart: %v", err)
	}
	viewRequest.HostFence = newFence
	view, err := manager.SetTaskWorkspaceView(context.Background(), "qabc1234", viewRequest)
	if err != nil || view.ViewID != viewID {
		t.Fatalf("restore Task view after Runtime restart = %#v, %v; want same view %s", view, err, viewID)
	}
	if _, err := manager.SetTaskLease("qabc1234", contract.SetTaskLeaseRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: newFence, LeaseVersion: 2, ExpiresAt: time.Now().UTC().Add(20 * time.Second),
	}); err != nil {
		t.Fatalf("reissue Task lease after Runtime restart: %v", err)
	}
	read, err := manager.Submit(context.Background(), contract.ExecuteRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: newFence, TaskID: "qabc1234", OperationID: "oread001",
		WorkspaceID: "w1234567", BindingVersion: 1, WorkspaceViewID: viewID, WorkspaceCheckpointRevision: 1,
		LeaseVersion: 2, Capability: "file.read", Action: "read", Arguments: json.RawMessage(`{"path":"result.txt"}`),
	})
	if err != nil {
		t.Fatalf("continue Task file read after Runtime restart: %v", err)
	}
	query = contract.OperationQuery{HostFence: newFence, TaskID: "qabc1234", LeaseVersion: 2}
	deadline = time.Now().Add(3 * time.Second)
	for read.Status == contract.OperationRunning && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		read, err = manager.QueryOperation(contract.ProtocolVersion, read.OperationID, query)
		if err != nil {
			t.Fatalf("query Task file read after Runtime restart: %v", err)
		}
	}
	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(read.Result, &result); err != nil || read.Status != contract.OperationSucceeded || result.Text != "published" {
		t.Fatalf("Task read after Runtime restart status=%q result=%s err=%v", read.Status, read.Result, err)
	}
}

func TestTaskWorkspaceViewDeletionIsIdempotentAndTombstoneBlocksRecreation(t *testing.T) {
	rootPath := t.TempDir()
	taskRootPath := filepath.Join(t.TempDir(), "task-views")
	sourcePath := filepath.Join(rootPath, "w1234567")
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	_, manifestSHA, err := scanTaskWorkspace(context.Background(), sourcePath)
	if err != nil {
		t.Fatalf("scan empty source workspace: %v", err)
	}
	workspace, err := OpenTaskWorkspace(rootPath, taskRootPath, uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workspace.Close() })
	const hostID, taskID, requestID = "h1234567", "q1234567", "z1234567"
	viewID := "v" + strings.Repeat("0", 64)
	if err := workspace.PrepareTaskWorkspaceView(context.Background(), hostID, taskID, "w1234567", viewID, 1, 0, 0, manifestSHA); err != nil {
		t.Fatalf("prepare private Task view: %v", err)
	}
	if err := os.WriteFile(filepath.Join(taskRootPath, taskID, viewID, "result.txt"), []byte("private output"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := workspace.DeleteTaskWorkspaceView(context.Background(), hostID, taskID, requestID); err != nil {
		t.Fatalf("delete private Task view: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(taskRootPath, taskID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Task view directory remains after deletion: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(taskRootPath, taskWorkspaceDeletionTombstoneName(taskID))); err != nil {
		t.Fatalf("durable deletion tombstone missing: %v", err)
	}
	if err := workspace.DeleteTaskWorkspaceView(context.Background(), hostID, taskID, requestID); err != nil {
		t.Fatalf("same deletion request was not idempotent: %v", err)
	}
	if err := workspace.DeleteTaskWorkspaceView(context.Background(), hostID, taskID, "z7654321"); !errors.Is(err, ErrTaskWorkspaceView) {
		t.Fatalf("different request ID reused a completed Task tombstone: %v", err)
	}
	if err := workspace.PrepareTaskWorkspaceView(context.Background(), hostID, taskID, "w1234567", viewID, 1, 0, 0, manifestSHA); !errors.Is(err, ErrTaskWorkspaceView) {
		t.Fatalf("deleted Task view was recreated in the same process: %v", err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	workspace, err = OpenTaskWorkspace(rootPath, taskRootPath, uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatalf("reopen Task Workspace after deletion: %v", err)
	}
	t.Cleanup(func() { _ = workspace.Close() })
	if err := workspace.PrepareTaskWorkspaceView(context.Background(), hostID, taskID, "w1234567", viewID, 1, 0, 0, manifestSHA); !errors.Is(err, ErrTaskWorkspaceView) {
		t.Fatalf("deleted Task view was recreated after Runtime restart: %v", err)
	}
}

func TestCoreCancellationOfQueuedWriteRequiresVerifiedStop(t *testing.T) {
	workspace := openTestWorkspace(t)
	workspacePath := filepath.Join(workspace.root.Name(), "w1234567")
	if err := os.Mkdir(workspacePath, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspacePath, "atomic.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, fence, viewID, _ := openAIFileManager(t, workspace)
	workspace.writeGate <- struct{}{}
	request := contract.ExecuteRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: "qabc1234", OperationID: "ocanc001",
		WorkspaceID: "w1234567", BindingVersion: 1, WorkspaceViewID: viewID, WorkspaceCheckpointRevision: 0,
		LeaseVersion: 1, Capability: "file.write", Action: "write",
		Arguments: json.RawMessage(`{"path":"atomic.txt","content":"new"}`),
	}
	if _, err := manager.Submit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		workspace.registry.mu.Lock()
		_, started := workspace.registry.runs[request.OperationID]
		workspace.registry.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("file write handler did not start")
		}
		time.Sleep(time.Millisecond)
	}
	operation, err := manager.CancelOperation(context.Background(), contract.ProtocolVersion, request.OperationID, contract.CancelOperationRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence,
	})
	<-workspace.writeGate
	if err != nil || operation.Status != contract.OperationCancelled {
		t.Fatalf("cancel result=%#v err=%v", operation, err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "old" {
		t.Fatalf("target after verified cancellation = %q err=%v", data, err)
	}
}

func TestCancelledWriteBeforeCommitLeavesTargetUntouched(t *testing.T) {
	workspace := openTestWorkspace(t)
	target := filepath.Join(workspace.root.Name(), "atomic.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	write := handlerFor(t, workspace, "write")
	workspace.writeGate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := write.Execute(ctx, requestFor("owrite30", "file.write", "write", `{"path":"atomic.txt","content":"new complete value"}`))
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write error = %v", err)
	}
	<-workspace.writeGate
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "old" {
		t.Fatalf("target after cancellation = %q err=%v", data, err)
	}
	entries, err := os.ReadDir(workspace.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".morde-tmp-") {
			t.Fatalf("temporary file left after cancelled write: %s", entry.Name())
		}
	}
}

func TestWriteIsAtomicAndStopDoesNotHideCommittedSideEffect(t *testing.T) {
	workspace := openTestWorkspace(t)
	write := handlerFor(t, workspace, "write")
	if err := os.WriteFile(filepath.Join(workspace.root.Name(), "atomic.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	operation := requestFor("owrite40", "file.write", "write", `{"path":"atomic.txt","content":"new complete value"}`)
	if _, err := write.Execute(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workspace.root.Name(), "atomic.txt"))
	if err != nil || string(data) != "new complete value" {
		t.Fatalf("atomic target = %q err=%v", data, err)
	}
	stop, err := write.Stop(context.Background(), contract.Operation{
		OperationID: operation.OperationID, Capability: "file.write", Action: "write",
	})
	if err != nil || stop.VerifiedStopped {
		t.Fatalf("Stop after a committed write = %#v err=%v, want unverified", stop, err)
	}
}

func TestListAndFindOutputAreBounded(t *testing.T) {
	workspace := openTestWorkspace(t)
	for index := 0; index < maxListEntries+5; index++ {
		name := filepath.Join(workspace.root.Name(), fmt.Sprintf("match-%04d", index))
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	list := handlerFor(t, workspace, "list")
	listResult, err := list.Execute(context.Background(), requestFor("olist009", "file.list", "list", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Entries   []entryMetadata `json:"entries"`
		Truncated bool            `json:"truncated"`
	}
	if err := json.Unmarshal(listResult, &listed); err != nil || len(listed.Entries) != maxListEntries || !listed.Truncated {
		t.Fatalf("bounded list len=%d truncated=%v err=%v", len(listed.Entries), listed.Truncated, err)
	}
	find := handlerFor(t, workspace, "find")
	findResult, err := find.Execute(context.Background(), requestFor("ofind009", "file.find", "find", `{"query":"match-"}`))
	if err != nil {
		t.Fatal(err)
	}
	var found struct {
		Matches   []entryMetadata `json:"matches"`
		Truncated bool            `json:"truncated"`
	}
	if err := json.Unmarshal(findResult, &found); err != nil || len(found.Matches) != maxFindMatches || !found.Truncated {
		t.Fatalf("find len=%d truncated=%v err=%v", len(found.Matches), found.Truncated, err)
	}
}

func TestListAndFindBoundScannedTransientEntries(t *testing.T) {
	workspace := openTestWorkspace(t)
	rootPath := workspace.root.Name()
	seed := filepath.Join(rootPath, tempFilePrefix+"seed")
	if err := os.WriteFile(seed, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= maxListScanned; index++ {
		name := filepath.Join(rootPath, fmt.Sprintf("%s%05d", tempFilePrefix, index))
		if err := os.Link(seed, name); err != nil {
			t.Fatalf("create transient entry %d: %v", index, err)
		}
	}

	list := handlerFor(t, workspace, "list")
	listResult, err := list.Execute(context.Background(), requestFor("olist010", "file.list", "list", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Entries   []entryMetadata `json:"entries"`
		Truncated bool            `json:"truncated"`
	}
	if err := json.Unmarshal(listResult, &listed); err != nil || len(listed.Entries) != 0 || !listed.Truncated {
		t.Fatalf("list hidden-entry scan len=%d truncated=%v err=%v", len(listed.Entries), listed.Truncated, err)
	}

	find := handlerFor(t, workspace, "find")
	findResult, err := find.Execute(context.Background(), requestFor("ofind010", "file.find", "find", `{"query":"no-match"}`))
	if err != nil {
		t.Fatal(err)
	}
	var found struct {
		Matches   []entryMetadata `json:"matches"`
		Truncated bool            `json:"truncated"`
	}
	if err := json.Unmarshal(findResult, &found); err != nil || len(found.Matches) != 0 || !found.Truncated {
		t.Fatalf("find hidden-entry scan len=%d truncated=%v err=%v", len(found.Matches), found.Truncated, err)
	}
}

func openTestWorkspace(t *testing.T) *Workspace {
	t.Helper()
	root := t.TempDir()
	taskRootParent := t.TempDir()
	workspace, err := OpenTaskWorkspace(root, filepath.Join(taskRootParent, "task-views"), uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workspace.Close() })
	return workspace
}

func openAIFileManager(t *testing.T, workspace *Workspace) (*runtimecore.Manager, contract.HostFence, string, string) {
	t.Helper()
	storeDir := t.TempDir()
	if err := os.Chmod(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := runtimecore.Open(runtimecore.Options{
		StoreDir: storeDir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: "habc1234", Generation: 1,
		Handlers: workspace.Handlers(), TaskWorkspaceViews: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	capabilities, err := manager.Capabilities(contract.CapabilitiesRequest{
		ProtocolVersion: contract.ProtocolVersion, HostID: "habc1234", RuntimeGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	fence := contract.HostFence{
		HostID: capabilities.HostID, RuntimeGeneration: capabilities.RuntimeGeneration,
		RuntimeInstanceID: capabilities.RuntimeInstanceID, ControlVersion: capabilities.ControlVersion,
	}
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetWorkspaceBindings(context.Background(), contract.SetWorkspaceBindingsRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence,
		Bindings: []contract.WorkspaceBinding{{WorkspaceID: "w1234567", BindingVersion: 1, Directory: "w1234567"}},
	}); err != nil {
		t.Fatal(err)
	}
	manifestSHA256 := strings.Repeat("0", 64)
	if _, sourceSHA256, scanErr := scanTaskWorkspace(context.Background(), filepath.Join(workspace.rootPath, "w1234567")); scanErr == nil {
		manifestSHA256 = sourceSHA256
	} else {
		t.Fatalf("scan Task Workspace source: %v", scanErr)
	}
	view, err := manager.SetTaskWorkspaceView(context.Background(), "qabc1234", contract.SetTaskWorkspaceViewRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: "qabc1234", WorkspaceID: "w1234567",
		BindingVersion: 1, InputRevision: 0, WorkspaceCheckpointRevision: 0, ManifestSHA256: manifestSHA256,
	})
	if err != nil {
		t.Fatalf("register Task Workspace view: %v", err)
	}
	if _, err := manager.SetTaskLease("qabc1234", contract.SetTaskLeaseRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, LeaseVersion: 1, ExpiresAt: time.Now().UTC().Add(20 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	return manager, fence, view.ViewID, storeDir
}

func handlerFor(t *testing.T, workspace *Workspace, action string) runtimecore.ActionHandler {
	t.Helper()
	for _, handler := range workspace.Handlers() {
		if handler.Action() == action {
			return handler
		}
	}
	t.Fatalf("handler %q not found", action)
	return nil
}

func requestFor(id, capability, action, arguments string) contract.ExecuteRequest {
	return contract.ExecuteRequest{
		OperationID: id, WorkspaceID: "w1234567", BindingVersion: 1,
		Capability: capability, Action: action, Arguments: json.RawMessage(arguments),
	}
}

func mustJSON(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
