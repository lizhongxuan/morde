package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	mordesync "morde-sync"
)

func TestHandoffReconcilesOnlyDirectoryModificationTimes(t *testing.T) {
	directory := t.TempDir()
	local, err := mordesync.Scan(context.Background(), directory, nil)
	if err != nil || len(local.Directories) != 1 || !local.Directories[0].ModTimeKnown {
		t.Fatalf("scan local root metadata = %#v, %v", local.Directories, err)
	}
	remote := local
	remote.Directories = append([]mordesync.Directory(nil), local.Directories...)
	remote.Directories[0].ModTimeNS -= 1_000_000_000
	if !sameManifestContentIgnoringDirectoryTimes(local, remote) {
		t.Fatal("directory timestamp was not the only manifest difference")
	}
	if err := applyHandoffDirectoryTimes(context.Background(), directory, local, remote); err != nil {
		t.Fatalf("reconcile root directory timestamp: %v", err)
	}
	if info, err := os.Stat(directory); err != nil || info.ModTime().UnixNano() != remote.Directories[0].ModTimeNS {
		t.Fatalf("root modification time = %d, %v; want %d", info.ModTime().UnixNano(), err, remote.Directories[0].ModTimeNS)
	}
	updated, err := mordesync.Scan(context.Background(), directory, nil)
	if err != nil || !sameManifestContent(updated, remote) {
		t.Fatalf("root metadata after reconciliation = %#v, %v", updated.Directories, err)
	}
}

func TestConfirmDownloadedLiveBaselineRejectsUnexpectedLocalFile(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	portal.setRemote("server.txt", []byte("server version"))
	client := portal.client(t)
	revision := portal.currentRevision()
	writeTestFile(t, directory, "server.txt", "server version")
	writeTestFile(t, directory, "local-only.txt", "keep me")

	err := client.ConfirmDownloadedLiveBaseline(context.Background(), testWorkspace, directory, revision)
	var conflict *LiveConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("unexpected local file did not block Host baseline: %v", err)
	}
	if portal.currentRevision() != revision || portal.fileText("local-only.txt") != "" {
		t.Fatal("baseline check uploaded an unexpected Host file")
	}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.readLiveState(canonical, testWorkspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conflict created a live baseline: %v", err)
	}
	assertLocalFile(t, directory, "local-only.txt", "keep me")
	if err := os.Remove(filepath.Join(directory, "local-only.txt")); err != nil {
		t.Fatal(err)
	}
	if err := client.ConfirmDownloadedLiveBaseline(context.Background(), testWorkspace, directory, revision); err != nil {
		t.Fatalf("matching downloaded tree did not establish baseline: %v", err)
	}
	if err := client.ValidateLiveBaseline(context.Background(), testWorkspace, directory, nil); err != nil {
		t.Fatalf("baseline was not durable: %v", err)
	}
}

func TestHandoffCheckpointReturnsVerifiedRevision(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	portal.setRemote("main.go", []byte("v1"))
	client := portal.client(t)
	seedHandoffBaseline(t, client, directory)
	writeTestFile(t, directory, "main.go", "v2")

	revision, err := client.HandoffCheckpoint(context.Background(), LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory})
	if err != nil {
		t.Fatalf("HandoffCheckpoint: %v", err)
	}
	if revision != portal.currentRevision() || portal.fileText("main.go") != "v2" {
		t.Fatalf("revision=%d server revision=%d remote main.go=%q", revision, portal.currentRevision(), portal.fileText("main.go"))
	}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.readLiveState(canonical, testWorkspace)
	if err != nil || state.Revision != revision || !sameManifestContent(state.Baseline, portal.currentManifest()) {
		t.Fatalf("persisted handoff state=%+v err=%v", state, err)
	}
}

func TestHandoffCheckpointResumesCommittedSessionAfterCheckpointWriteFailure(t *testing.T) {
	cacheHome := t.TempDir()
	t.Setenv("HOME", cacheHome)
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	portal.setRemote("main.go", []byte("v1"))
	first := portal.client(t)
	seedHandoffBaseline(t, first, directory)
	writeTestFile(t, directory, "main.go", "v2")
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	liveStateFile, err := first.liveStateFilename(canonical, testWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	previousState, err := os.ReadFile(liveStateFile)
	if err != nil {
		t.Fatal(err)
	}
	installHandoffCommitHook(t, first, func() {
		if err := os.Remove(liveStateFile); err != nil {
			t.Errorf("remove live checkpoint for failure injection: %v", err)
			return
		}
		if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), liveStateFile); err != nil {
			t.Errorf("install live checkpoint failure: %v", err)
		}
	})
	if _, err := first.HandoffCheckpoint(context.Background(), LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory}); err == nil || !strings.Contains(err.Error(), "baseline checkpoint failed") {
		t.Fatalf("expected durable checkpoint failure after commit, got %v", err)
	}
	assertLocalFile(t, directory, "main.go", "v2")
	if got := portal.fileText("main.go"); got != "v2" {
		t.Fatalf("remote committed file = %q, want v2", got)
	}
	metadata, err := readResumeMetadata(canonical, testWorkspace)
	if err != nil || metadata.SessionID == "" {
		t.Fatalf("completed session recovery metadata=%+v err=%v", metadata, err)
	}
	metadata.State = "completed"
	if err := writeResumeMetadata(canonical, metadata); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(liveStateFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(liveStateFile, previousState, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, directory, "after-commit.txt", "new local work after the committed sync")
	createdBeforeResume := portal.createCount()
	revision, err := portal.client(t).HandoffCheckpoint(context.Background(), LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory})
	if err != nil || revision != portal.currentRevision() {
		t.Fatalf("resume committed handoff revision=%d current=%d err=%v", revision, portal.currentRevision(), err)
	}
	if got := portal.fileText("after-commit.txt"); got != "new local work after the committed sync" {
		t.Fatalf("post-commit local change was not included in the resumed handoff: %q", got)
	}
	if got := portal.createCount() - createdBeforeResume; got > 2 {
		t.Fatalf("resume created more than the local-edit and cursor-ack sessions: %d", got)
	}
	state, err := first.readLiveState(canonical, testWorkspace)
	if err != nil || state.Revision != revision || !sameManifestContent(state.Baseline, portal.currentManifest()) {
		t.Fatalf("recovered common baseline=%+v err=%v", state, err)
	}
}

func TestHandoffCheckpointRestartReusesServerCreateIdentity(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	portal.setRemote("main.go", []byte("base"))
	first := portal.client(t)
	seedHandoffBaseline(t, first, directory)
	initialCreates := len(portal.createdKeys())
	writeTestFile(t, directory, "main.go", "edited")
	const handoffID = "9fc6eec8-3fe8-4cde-881d-05e5f82cf6b4"
	dropped := false
	first.http = &http.Client{Transport: handoffHookTransport{base: http.DefaultTransport, hook: func(request *http.Request, response *http.Response) {
		if !dropped && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/sync-sessions") {
			dropped = true
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			response.Body = io.NopCloser(strings.NewReader(""))
		}
	}}}
	if _, err := first.HandoffCheckpoint(context.Background(), LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory, IdempotencyKey: handoffID}); err == nil || !dropped {
		t.Fatalf("lost create response: error=%v dropped=%t", err, dropped)
	}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	resume, err := readResumeMetadata(canonical, testWorkspace)
	if err != nil || resume.SessionID != "" || !strings.HasPrefix(resume.IdempotencyKey, handoffID+".") {
		t.Fatalf("ambiguous create metadata=%+v err=%v", resume, err)
	}
	createCount := portal.createCount()
	if _, err := portal.client(t).HandoffCheckpoint(context.Background(), LiveSyncOptions{
		WorkspaceID: testWorkspace, Directory: directory, IdempotencyKey: "c16c17f6-0eb5-4df6-a819-d79a95a46d24",
	}); err == nil || portal.createCount() != createCount {
		t.Fatalf("another handoff adopted an unknown create: error=%v creates=%d want=%d", err, portal.createCount(), createCount)
	}
	otherURL := strings.Replace(portal.server.URL, "127.0.0.1", "localhost", 1)
	if otherURL == portal.server.URL {
		t.Fatal("test Portal has no alternate loopback name")
	}
	otherPortal, err := New(otherURL, "test-device-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherPortal.HandoffCheckpoint(context.Background(), LiveSyncOptions{
		WorkspaceID: testWorkspace, Directory: directory, IdempotencyKey: handoffID,
	}); err == nil || portal.createCount() != createCount {
		t.Fatalf("another Portal adopted an unknown create: error=%v creates=%d want=%d", err, portal.createCount(), createCount)
	}

	// A fresh client represents the Desktop's restarted sync subprocess. The
	// server must replay the same session for the saved create key.
	second := portal.client(t)
	revision, err := second.HandoffCheckpoint(context.Background(), LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory, IdempotencyKey: handoffID})
	if err != nil || revision != portal.currentRevision() || portal.fileText("main.go") != "edited" {
		t.Fatalf("restarted handoff revision=%d server=%d file=%q err=%v", revision, portal.currentRevision(), portal.fileText("main.go"), err)
	}
	keys := portal.createdKeys()
	if len(keys) < initialCreates+2 || keys[initialCreates] != resume.IdempotencyKey || keys[initialCreates+1] != resume.IdempotencyKey {
		t.Fatalf("create keys across restart=%v; want saved key %q twice", keys, resume.IdempotencyKey)
	}
	if len(keys) > initialCreates+2 && keys[initialCreates+2] == keys[initialCreates+1] {
		t.Fatalf("distinct handoff sync requests reused a create key: %v", keys)
	}
}

func TestHandoffCheckpointRejectsWrongIdentityForKnownPendingSession(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	portal.setRemote("main.go", []byte("base"))
	first := portal.client(t)
	seedHandoffBaseline(t, first, directory)
	writeTestFile(t, directory, "main.go", "edited")
	const handoffID = "9fc6eec8-3fe8-4cde-881d-05e5f82cf6b4"
	dropped := false
	first.http = &http.Client{Transport: handoffHookTransport{base: http.DefaultTransport, hook: func(request *http.Request, response *http.Response) {
		if !dropped && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/commit") {
			dropped = true
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			response.Body = io.NopCloser(strings.NewReader(""))
		}
	}}}
	if _, err := first.HandoffCheckpoint(context.Background(), LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory, IdempotencyKey: handoffID}); err == nil || !dropped {
		t.Fatalf("lost commit response: error=%v dropped=%t", err, dropped)
	}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := readResumeMetadata(canonical, testWorkspace)
	if err != nil || pending.SessionID == "" || !strings.HasPrefix(pending.IdempotencyKey, handoffID+".") {
		t.Fatalf("known pending session=%+v err=%v", pending, err)
	}
	createCount := portal.createCount()
	if _, err := portal.client(t).HandoffCheckpoint(context.Background(), LiveSyncOptions{
		WorkspaceID: testWorkspace, Directory: directory, IdempotencyKey: "c16c17f6-0eb5-4df6-a819-d79a95a46d24",
	}); err == nil || portal.createCount() != createCount {
		t.Fatalf("another handoff adopted a pending session: error=%v creates=%d want=%d", err, portal.createCount(), createCount)
	}
	otherURL := strings.Replace(portal.server.URL, "127.0.0.1", "localhost", 1)
	if otherURL == portal.server.URL {
		t.Fatal("test Portal has no alternate loopback name")
	}
	otherPortal, err := New(otherURL, "test-device-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherPortal.HandoffCheckpoint(context.Background(), LiveSyncOptions{
		WorkspaceID: testWorkspace, Directory: directory, IdempotencyKey: handoffID,
	}); err == nil || portal.createCount() != createCount {
		t.Fatalf("another Portal adopted a pending session: error=%v creates=%d want=%d", err, portal.createCount(), createCount)
	}
	revision, err := portal.client(t).HandoffCheckpoint(context.Background(), LiveSyncOptions{
		WorkspaceID: testWorkspace, Directory: directory, IdempotencyKey: handoffID,
	})
	if err != nil || revision != portal.currentRevision() || portal.fileText("main.go") != "edited" {
		t.Fatalf("same handoff did not recover the pending session: revision=%d current=%d error=%v", revision, portal.currentRevision(), err)
	}
}

func TestHandoffCreateKeyDistinguishesRequests(t *testing.T) {
	const handoffID = "9fc6eec8-3fe8-4cde-881d-05e5f82cf6b4"
	const portalURL = "https://morde.example"
	const directory = "/work/project"
	manifest := mustScan(context.Background(), t.TempDir(), nil)
	first, err := derivedCreateKey(handoffID, portalURL, directory, testWorkspace, "", "", "bidirectional", "live", 1, nil, manifest, nil, nil, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	again, err := derivedCreateKey(handoffID, portalURL, directory, testWorkspace, "", "", "bidirectional", "live", 1, nil, manifest, nil, nil, nil, "", "")
	if err != nil || again != first {
		t.Fatalf("same create request keys=%q and %q, error=%v", first, again, err)
	}
	changed, err := derivedCreateKey(handoffID, portalURL, directory, testWorkspace, "", "", "bidirectional", "live", 2, nil, manifest, nil, nil, nil, "", "")
	if err != nil || changed == first || len(first) > 128 {
		t.Fatalf("changed create request keys=%q and %q, error=%v", first, changed, err)
	}
	otherDirectory, err := derivedCreateKey(handoffID, portalURL, "/work/other", testWorkspace, "", "", "bidirectional", "live", 1, nil, manifest, nil, nil, nil, "", "")
	if err != nil || otherDirectory == first {
		t.Fatalf("another directory reused key %q, error=%v", otherDirectory, err)
	}
	otherPortal, err := derivedCreateKey(handoffID, "https://other.example", directory, testWorkspace, "", "", "bidirectional", "live", 1, nil, manifest, nil, nil, nil, "", "")
	if err != nil || otherPortal == first {
		t.Fatalf("another Portal reused key %q, error=%v", otherPortal, err)
	}
}

func TestHandoffSameIdentityDoesNotReplayAnotherDirectory(t *testing.T) {
	portal := newLiveTestPortal(t)
	const handoffID = "9fc6eec8-3fe8-4cde-881d-05e5f82cf6b4"
	for _, directory := range []string{t.TempDir(), t.TempDir()} {
		if _, err := portal.client(t).HandoffCheckpoint(context.Background(), LiveSyncOptions{
			WorkspaceID: testWorkspace, Directory: directory, IdempotencyKey: handoffID,
		}); err != nil {
			t.Fatalf("handoff directory %q: %v", directory, err)
		}
	}
	keys := portal.createdKeys()
	if len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("separate directories reused a server create key: %v", keys)
	}
}

func TestHandoffCheckpointInitializesBaselineBeforeDetaching(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	portal.setRemote("main.go", []byte("workspace snapshot"))
	client := portal.client(t)

	revision, err := client.HandoffCheckpoint(context.Background(), LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory})
	if err != nil {
		t.Fatalf("HandoffCheckpoint without prior live baseline: %v", err)
	}
	if revision != portal.currentRevision() {
		t.Fatalf("handoff revision=%d, server revision=%d", revision, portal.currentRevision())
	}
	assertLocalFile(t, directory, "main.go", "workspace snapshot")
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.readLiveState(canonical, testWorkspace)
	if err != nil || !state.Initialized || state.Revision != revision {
		t.Fatalf("handoff baseline = %+v, err=%v", state, err)
	}
}

func TestHandoffCheckpointReturnsRemoteOnlyChangesToLocal(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	portal.setRemote("updated.txt", []byte("v1"))
	portal.setRemote("deleted.txt", []byte("remove me"))
	client := portal.client(t)
	seedHandoffBaseline(t, client, directory)

	portal.setRemote("updated.txt", []byte("v2"))
	portal.setRemote("added.txt", []byte("new remote file"))
	portal.deleteRemote("deleted.txt")

	revision, err := client.HandoffCheckpoint(context.Background(), LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory})
	if err != nil {
		t.Fatalf("HandoffCheckpoint: %v", err)
	}
	if got := portal.currentRevision(); revision != got {
		t.Fatalf("revision=%d server revision=%d", revision, got)
	}
	assertLocalFile(t, directory, "updated.txt", "v2")
	assertLocalFile(t, directory, "added.txt", "new remote file")
	local, err := scanHandoffWorkspace(context.Background(), directory, nil)
	if err != nil {
		t.Fatalf("scan local workspace: %v", err)
	}
	remote := portal.currentManifest()
	if !sameManifestContent(local, remote) {
		t.Fatalf("local manifest does not match remote after handoff: local=%+v remote=%+v", local, remote)
	}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.readLiveState(canonical, testWorkspace)
	if err != nil || state.Revision != revision || !sameManifestContent(state.Baseline, remote) {
		t.Fatalf("persisted handoff state=%+v err=%v remote revision=%d", state, err, revision)
	}
	cursor, err := decodeLiveCursor(state.Cursor)
	if err != nil || cursor.Revision != revision {
		t.Fatalf("persisted handoff cursor=%+v err=%v revision=%d", cursor, err, revision)
	}
}

func TestHandoffCheckpointConflictKeepsCommonBaseline(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	portal.setRemote("main.go", []byte("v1"))
	client := portal.client(t)
	before := seedHandoffBaseline(t, client, directory)
	writeTestFile(t, directory, "main.go", "local edit")
	portal.setRemote("main.go", []byte("remote edit"))

	_, err := client.HandoffCheckpoint(context.Background(), LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory})
	var conflict *LiveConflictError
	if !errors.As(err, &conflict) || len(conflict.Conflicts) == 0 {
		t.Fatalf("expected retained sync conflict, got %v", err)
	}
	assertHandoffBaselineUnchanged(t, client, directory, before)
	assertLocalFile(t, directory, "main.go", "local edit")
	if got := portal.fileText("main.go"); got != "remote edit" {
		t.Fatalf("remote edit was lost: %q", got)
	}
}

func TestHandoffCheckpointRejectsConcurrentLocalChange(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	portal.setRemote("main.go", []byte("v1"))
	client := portal.client(t)
	before := seedHandoffBaseline(t, client, directory)
	writeTestFile(t, directory, "main.go", "planned edit")
	installHandoffCommitHook(t, client, func() {
		writeTestFile(t, directory, "main.go", "concurrent edit")
	})

	_, err := client.HandoffCheckpoint(context.Background(), LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory})
	var conflict *LiveConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected concurrent local change to fail closed, got %v", err)
	}
	assertHandoffBaselineUnchanged(t, client, directory, before)
	assertLocalFile(t, directory, "main.go", "concurrent edit")
	if got := portal.fileText("main.go"); got != "planned edit" {
		t.Fatalf("committed server snapshot unexpectedly changed: %q", got)
	}
}

func TestHandoffCheckpointRejectsServerRevisionAdvance(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	portal.setRemote("main.go", []byte("v1"))
	client := portal.client(t)
	seedHandoffBaseline(t, client, directory)
	writeTestFile(t, directory, "main.go", "planned edit")
	installHandoffCommitHook(t, client, func() {
		portal.setRemote("other.txt", []byte("concurrent server edit"))
	})

	revision, err := client.HandoffCheckpoint(context.Background(), LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory})
	if err == nil || !strings.Contains(err.Error(), "revision advanced") {
		t.Fatalf("expected revision advance to fail closed, revision=%d err=%v", revision, err)
	}
	if revision != 0 || portal.fileText("other.txt") != "concurrent server edit" {
		t.Fatalf("revision=%d remote concurrent change=%q", revision, portal.fileText("other.txt"))
	}
}

func TestHandoffCheckpointHonorsCancellation(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	client := portal.client(t)
	seedHandoffBaseline(t, client, directory)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := client.HandoffCheckpoint(ctx, LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func seedHandoffBaseline(t *testing.T, client *Client, directory string) liveState {
	t.Helper()
	options := LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.loadOrCreateLiveState(context.Background(), options, canonical)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, result, ran, err := client.liveCycle(ctx, options, canonical, state, nil)
	if err != nil || !ran || result.Session.State != "completed" {
		t.Fatalf("seed live baseline: ran=%t session=%+v err=%v", ran, result.Session, err)
	}
	if err := client.reportLiveResult(ctx, options, canonical, result, ran, nil); err != nil {
		t.Fatalf("finish seeded live baseline: %v", err)
	}
	state, err = client.readLiveState(canonical, testWorkspace)
	if err != nil || !state.Initialized {
		t.Fatalf("read seeded baseline: state=%+v err=%v", state, err)
	}
	return state
}

func assertHandoffBaselineUnchanged(t *testing.T, client *Client, directory string, before liveState) {
	t.Helper()
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	after, err := client.readLiveState(canonical, testWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if before.Revision != after.Revision || before.Cursor != after.Cursor || !sameManifestContent(before.Baseline, after.Baseline) {
		t.Fatalf("common baseline changed after failed handoff: before=%+v after=%+v", before, after)
	}
}

func installHandoffCommitHook(t *testing.T, client *Client, hook func()) {
	t.Helper()
	var once sync.Once
	client.http = &http.Client{Transport: handoffHookTransport{base: http.DefaultTransport, hook: func(request *http.Request, response *http.Response) {
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/commit") && response.StatusCode == http.StatusOK {
			once.Do(hook)
		}
	}}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

type handoffHookTransport struct {
	base http.RoundTripper
	hook func(*http.Request, *http.Response)
}

func (transport handoffHookTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err == nil && transport.hook != nil {
		transport.hook(request, response)
	}
	return response, err
}
