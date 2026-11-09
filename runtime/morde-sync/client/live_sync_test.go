package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	mordesync "morde-sync"
)

func TestSameManifestContentIncludesModificationTime(t *testing.T) {
	oldTime, newTime := int64(1_700_000_000_000_000_000), int64(1_700_000_001_000_000_000)
	left := mordesync.Manifest{Files: []mordesync.File{{Path: "notes.txt", Size: 1, Mode: 0o600, ModTimeNS: oldTime, ModTimeKnown: true}}}
	right := mordesync.Manifest{Files: []mordesync.File{{Path: "notes.txt", Size: 1, Mode: 0o600, ModTimeNS: oldTime, ModTimeKnown: true}}}
	if !sameManifestContent(left, right) {
		t.Fatal("equal known modification times differ")
	}
	right.Files[0].ModTimeNS = newTime
	if sameManifestContent(left, right) {
		t.Fatal("different modification times were treated as equal")
	}
	right.Files[0].ModTimeKnown = false
	if !sameManifestContent(left, right) {
		t.Fatal("an unknown remote modification time was treated as a mismatch")
	}
	if sameManifestContentExact(left, right) {
		t.Fatal("a snapshot changing known file time to unknown was treated as unchanged")
	}
	left.Files[0].HardlinkTo = "anchor.txt"
	right.Files[0].HardlinkTo = "anchor.txt"
	if !sameManifestContent(left, right) {
		t.Fatal("equal hard-link relationships differ")
	}
	right.Files[0].HardlinkTo = "different-anchor.txt"
	if sameManifestContent(left, right) {
		t.Fatal("different hard-link relationships were treated as equal")
	}

	localDirectory := mordesync.Directory{Path: "src", Mode: 0o750, ModeKnown: true, ModTimeNS: oldTime, ModTimeKnown: true}
	remoteUnknown := mordesync.Directory{Path: "src"}
	if !sameManifestContent(mordesync.Manifest{Directories: []mordesync.Directory{localDirectory}}, mordesync.Manifest{Directories: []mordesync.Directory{remoteUnknown}}) {
		t.Fatal("unknown remote directory metadata was treated as a mismatch")
	}
	if sameManifestContentExact(mordesync.Manifest{Directories: []mordesync.Directory{localDirectory}}, mordesync.Manifest{Directories: []mordesync.Directory{remoteUnknown}}) {
		t.Fatal("a snapshot gaining directory metadata was treated as unchanged")
	}
	remoteChanged := localDirectory
	remoteChanged.Mode = 0o700
	if sameManifestContent(mordesync.Manifest{Directories: []mordesync.Directory{localDirectory}}, mordesync.Manifest{Directories: []mordesync.Directory{remoteChanged}}) {
		t.Fatal("different known directory metadata was treated as equal")
	}
	localWithEmptyXattrs := mordesync.Directory{Path: ".", MetadataKnown: true, Xattrs: []mordesync.ExtendedAttribute{}}
	remoteWithNilXattrs := mordesync.Directory{Path: ".", MetadataKnown: true}
	if !sameManifestContent(mordesync.Manifest{Directories: []mordesync.Directory{localWithEmptyXattrs}}, mordesync.Manifest{Directories: []mordesync.Directory{remoteWithNilXattrs}}) ||
		!sameManifestContentExact(mordesync.Manifest{Directories: []mordesync.Directory{localWithEmptyXattrs}}, mordesync.Manifest{Directories: []mordesync.Directory{remoteWithNilXattrs}}) {
		t.Fatal("empty and nil xattr collections were treated as different metadata")
	}
}

func TestRetryableErrorIncludesChangedSource(t *testing.T) {
	if !retryableError(fmt.Errorf("scan local live workspace: %w", mordesync.ErrRescanRequired)) {
		t.Fatal("a changing local file should trigger another live-sync scan")
	}
	busy := &HTTPError{Status: http.StatusConflict, Code: "workspace_busy"}
	if !retryableError(busy) || isStaleCursorError(busy) {
		t.Fatal("transient workspace busy response should retry without resetting the changes cursor")
	}
	if !isStaleCursorError(&HTTPError{Status: http.StatusConflict, Code: "workspace_conflict"}) {
		t.Fatal("workspace conflict from the changes feed should require a safe cursor reset")
	}
}

func TestParentDirectoryPathsIncludesWorkspaceRoot(t *testing.T) {
	for _, test := range []struct {
		path string
		want []string
	}{
		{path: "README.md", want: []string{"."}},
		{path: "src/main.go", want: []string{".", "src"}},
		{path: "src/nested/main.go", want: []string{".", "src", "src/nested"}},
	} {
		if got := parentDirectoryPaths(test.path); !equalStrings(got, test.want) {
			t.Fatalf("parent directories for %q = %#v; want %#v", test.path, got, test.want)
		}
	}
}

func TestValidateLiveBaselineRequiresInitializedMatchingCheckpoint(t *testing.T) {
	directory, err := canonicalDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{}
	if err := client.ValidateLiveBaseline(context.Background(), testWorkspace, directory, nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing baseline error = %v", err)
	}
	state := liveState{
		Version: 1, Workspace: testWorkspace, Directory: directory, Initialized: false,
		Cursor: encodeLiveCursor(liveCursor{Revision: 1}), Revision: 1,
		Baseline: mordesync.Manifest{Files: []mordesync.File{}},
	}
	if err := client.writeLiveState(directory, state); err != nil {
		t.Fatal(err)
	}
	if err := client.ValidateLiveBaseline(context.Background(), testWorkspace, directory, nil); err == nil {
		t.Fatal("uninitialized baseline was accepted")
	}
	state.Initialized = true
	if err := client.writeLiveState(directory, state); err != nil {
		t.Fatal(err)
	}
	if err := client.ValidateLiveBaseline(context.Background(), testWorkspace, directory, nil); err != nil {
		t.Fatalf("valid baseline rejected: %v", err)
	}
	if err := client.ValidateLiveBaseline(context.Background(), testWorkspace, directory, []string{"src"}); err == nil {
		t.Fatal("baseline with a different scope was accepted")
	}
}

func TestOpenClientWorkspaceRootRejectsSymlink(t *testing.T) {
	workspace := t.TempDir()
	root, err := openClientWorkspaceRoot(workspace)
	if err != nil {
		t.Fatalf("open real workspace: %v", err)
	}
	_ = root.Close()

	link := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(workspace, link); err != nil {
		t.Skipf("symlink is unavailable: %v", err)
	}
	if _, err := openClientWorkspaceRoot(link); err == nil {
		t.Fatal("workspace root symlink was accepted")
	}
}

func TestLiveSyncInitialComplementsDifferentPathsAndPersistsBaseline(t *testing.T) {
	directory := t.TempDir()
	writeTestFile(t, directory, "local.txt", "local data")
	portal := newLiveTestPortal(t)
	portal.setRemote("remote.txt", []byte("remote data"))
	portal.setRemote("same.txt", []byte("same data"))
	writeTestFile(t, directory, "same.txt", "same data")
	client := portal.client(t)
	ctx, cancel := context.WithCancel(context.Background())
	watching := make(chan struct{}, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.LiveSync(ctx, LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory,
			RescanInterval: 5 * time.Second, Progress: func(progress Progress) {
				if progress.Stage == "watch" {
					select {
					case watching <- struct{}{}:
					default:
					}
				}
			}})
	}()
	select {
	case <-watching:
	case err := <-errCh:
		t.Fatalf("initial live sync failed before watching: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for initial live sync")
	}
	cancel()
	if err := <-errCh; err == nil {
		t.Fatal("LiveSync returned nil after cancellation")
	}
	assertLocalFile(t, directory, "remote.txt", "remote data")
	assertLocalFile(t, directory, "local.txt", "local data")
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.readLiveState(canonical, testWorkspace)
	if err != nil || !state.Initialized || state.Revision != portal.currentRevision() {
		t.Fatalf("live baseline state=%+v err=%v remote revision=%d", state, err, portal.currentRevision())
	}
	if !sameManifestContent(state.Baseline, portal.currentManifest()) {
		t.Fatalf("persisted baseline differs from remote snapshot: %+v", state.Baseline)
	}
}

func TestLiveSyncKeepsSameCursorPollAcrossPeriodicRescan(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	portal.changeWait = 15 * time.Second
	client := portal.client(t)
	ctx, cancel := context.WithCancel(context.Background())
	watching := make(chan struct{}, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.LiveSync(ctx, LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory,
			RescanInterval: 5 * time.Second, Progress: func(progress Progress) {
				if progress.Stage == "watch" {
					select {
					case watching <- struct{}{}:
					default:
					}
				}
			}})
	}()
	select {
	case <-watching:
	case err := <-errCh:
		cancel()
		t.Fatalf("initial live sync failed before watching: %v", err)
	case <-time.After(8 * time.Second):
		cancel()
		t.Fatal("timed out waiting for initial live sync")
	}
	select {
	case <-time.After(6 * time.Second):
	case err := <-errCh:
		cancel()
		t.Fatalf("live sync stopped during periodic rescan: %v", err)
	}
	if calls := portal.changeCallCount(); calls != 1 {
		cancel()
		if err := <-errCh; err == nil {
			t.Fatal("LiveSync returned nil after cancellation")
		}
		t.Fatalf("changes poll requests after a same-cursor rescan = %d, want 1", calls)
	}
	cancel()
	if err := <-errCh; err == nil {
		t.Fatal("LiveSync returned nil after cancellation")
	}
}

func TestLiveSyncReplaysCompletedSessionAfterReceiptIsInterrupted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	directory := t.TempDir()
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	portal := newLiveTestPortal(t)
	client := portal.client(t)
	ctx, cancel := context.WithCancel(context.Background())
	var firstID string
	err = client.LiveSync(ctx, LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory,
		OnResult: func(result Result) {
			if result.Session.State == "completed" {
				firstID = result.Session.ID
				cancel()
			}
		}})
	if !errors.Is(err, context.Canceled) || firstID == "" {
		t.Fatalf("interrupted receipt: session=%q error=%v", firstID, err)
	}
	metadata, err := readResumeMetadata(canonical, testWorkspace)
	if err != nil || metadata.SessionID != firstID {
		t.Fatalf("recoverable completed session = %+v, %v", metadata, err)
	}
	created := portal.createCount()
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	restarted := portal.client(t)
	replayed := make(chan string, 1)
	watching := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- restarted.LiveSync(ctx, LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory,
			OnResult: func(result Result) { replayed <- result.Session.ID },
			Progress: func(progress Progress) {
				if progress.Stage == "watch" {
					select {
					case watching <- struct{}{}:
					default:
					}
				}
			},
		})
	}()
	select {
	case got := <-replayed:
		if got != firstID {
			t.Fatalf("replayed session=%q, want %q", got, firstID)
		}
	case err := <-done:
		t.Fatalf("replay stopped before receipt: %v; metadata state=%q policy=%q", err, metadata.State, metadata.Policy)
	case <-time.After(5 * time.Second):
		t.Fatal("completed session receipt was not replayed")
	}
	select {
	case <-watching:
	case err := <-done:
		t.Fatalf("replay stopped before watching: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("replayed live sync did not resume watching")
	}
	if _, err := readResumeMetadata(canonical, testWorkspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed session metadata remains after receipt: %v", err)
	}
	if got := portal.createCount(); got != created {
		t.Fatalf("replay created %d new sessions", got-created)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("stop replay: %v", err)
	}
}

func TestLiveSyncInitialSamePathConflictPreservesBothAndRestartDoesNotOverwrite(t *testing.T) {
	directory := t.TempDir()
	writeTestFile(t, directory, "shared.txt", "local version")
	portal := newLiveTestPortal(t)
	portal.setRemote("shared.txt", []byte("remote version"))
	client := portal.client(t)
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		err := client.LiveSync(ctx, LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory, RescanInterval: 5 * time.Second})
		cancel()
		var conflict *LiveConflictError
		if !errors.As(err, &conflict) || len(conflict.Conflicts) == 0 {
			t.Fatalf("attempt %d expected initial conflict, got %v", attempt+1, err)
		}
		canonical, canonicalErr := canonicalDirectory(directory)
		if canonicalErr != nil {
			t.Fatal(canonicalErr)
		}
		state, stateErr := client.readLiveState(canonical, testWorkspace)
		if stateErr != nil || state.Initialized {
			t.Fatalf("attempt %d unexpectedly established baseline: state=%+v err=%v", attempt+1, state, stateErr)
		}
		assertLocalFile(t, directory, "shared.txt", "local version")
		if got := portal.fileText("shared.txt"); got != "remote version" {
			t.Fatalf("attempt %d remote version = %q", attempt+1, got)
		}
	}
}

func TestCompareManifestConflictsReportsCaseOnlyRename(t *testing.T) {
	local := mordesync.Manifest{FileCount: 1, TotalBytes: 4, Files: []mordesync.File{makeFile("foo.txt", []byte("same"))}}
	remote := mordesync.Manifest{FileCount: 1, TotalBytes: 4, Files: []mordesync.File{makeFile("Foo.txt", []byte("same"))}}
	conflicts := compareManifestConflicts(local, remote)
	if len(conflicts) != 1 || conflicts[0].Reason != "case_collision" || conflicts[0].Path != "Foo.txt" {
		t.Fatalf("case-only rename conflicts = %#v", conflicts)
	}
}

func TestLiveSyncPreservesLocalFileOnRemoteCaseOnlyRename(t *testing.T) {
	directory := t.TempDir()
	writeTestFile(t, directory, "foo.txt", "same")
	portal := newLiveTestPortal(t)
	portal.setRemote("foo.txt", []byte("same"))
	client := portal.client(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := runLiveUntilWatching(t, ctx, client, directory)
	waitSignal(t, run.watching, "initial live sync")
	portal.renameRemote("foo.txt", "Foo.txt")

	select {
	case err := <-run.done:
		var conflict *LiveConflictError
		if !errors.As(err, &conflict) || len(conflict.Conflicts) != 1 ||
			conflict.Conflicts[0].Reason != "case_collision" || conflict.Conflicts[0].Path != "Foo.txt" {
			t.Fatalf("remote case-only rename error = %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("LiveSync did not stop on the remote case-only rename")
	}
	manifest, err := mordesync.Scan(context.Background(), directory, nil)
	if err != nil || len(manifest.Files) != 1 || manifest.Files[0].Path != "foo.txt" {
		t.Fatalf("local file after remote case-only rename = %#v err=%v", manifest.Files, err)
	}
	assertLocalFile(t, directory, "foo.txt", "same")
	remote, ok := portal.file("foo.txt")
	if !ok || remote.Path != "Foo.txt" {
		t.Fatalf("remote file after case-only rename = %#v, exists=%t", remote, ok)
	}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.readLiveState(canonical, testWorkspace)
	if err != nil || state.Revision != 1 || state.Baseline.Files[0].Path != "foo.txt" {
		t.Fatalf("live baseline advanced across unresolved rename: state=%+v err=%v", state, err)
	}
}

func TestLiveSyncPreventsAnotherSyncOperationOnTheSameDirectory(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	client := portal.client(t)
	ctx, cancel := context.WithCancel(context.Background())
	run := runLiveUntilWatching(t, ctx, client, directory)
	waitSignal(t, run.watching, "initial live sync")
	if _, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory}); err == nil || err.Error() != "another Morde sync operation is active for this directory" {
		t.Fatalf("sync during live session error = %v", err)
	}
	cancel()
	waitLiveDone(t, run.done)
	if _, err := client.Sync(context.Background(), SyncOptions{WorkspaceID: testWorkspace, Directory: directory}); err != nil {
		t.Fatalf("sync after live session stopped: %v", err)
	}
}

func TestLiveSyncUploadsOneSidedEditAndDelete(t *testing.T) {
	t.Run("edit", func(t *testing.T) {
		directory := t.TempDir()
		portal := newLiveTestPortal(t)
		portal.setRemote("main.go", []byte("v1"))
		client := portal.client(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		run := runLiveUntilWatching(t, ctx, client, directory)
		waitSignal(t, run.watching, "initial live sync")
		writeTestFile(t, directory, "main.go", "v2 local")
		waitUntil(t, 6*time.Second, func() bool { return portal.fileText("main.go") == "v2 local" })
		cancel()
		waitLiveDone(t, run.done)
	})
	t.Run("delete", func(t *testing.T) {
		directory := t.TempDir()
		portal := newLiveTestPortal(t)
		portal.setRemote("remove.txt", []byte("old value"))
		client := portal.client(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		run := runLiveUntilWatching(t, ctx, client, directory)
		waitSignal(t, run.watching, "initial live sync")
		if err := os.Remove(filepath.Join(directory, "remove.txt")); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, 6*time.Second, func() bool { _, exists := portal.file("remove.txt"); return !exists })
		cancel()
		waitLiveDone(t, run.done)
	})
}

func TestLiveSyncAppliesRemoteUpdateWithChangedSize(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	portal.setRemote("main.go", []byte("v1"))
	client := portal.client(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := runLiveUntilWatching(t, ctx, client, directory)
	waitSignal(t, run.watching, "initial live sync")
	portal.setRemote("main.go", []byte("a longer remote version"))
	waitUntil(t, 6*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(directory, "main.go"))
		return err == nil && string(data) == "a longer remote version"
	})
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 6*time.Second, func() bool {
		select {
		case err := <-run.done:
			t.Fatalf("live sync stopped before the remote update was committed: %v", err)
		default:
		}
		state, err := client.readLiveState(canonical, testWorkspace)
		return err == nil && state.Revision == portal.currentRevision() && sameManifestContent(state.Baseline, portal.currentManifest())
	})
	cancel()
	waitLiveDone(t, run.done)
}

func TestLiveSyncAppliesRemoteDeletionOnlyAgainstBaseline(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	portal.setRemote("remote-delete.txt", []byte("baseline"))
	client := portal.client(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := runLiveUntilWatching(t, ctx, client, directory)
	waitSignal(t, run.watching, "initial live sync")
	portal.deleteRemote("remote-delete.txt")
	waitUntil(t, 6*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(directory, "remote-delete.txt"))
		return os.IsNotExist(err)
	})
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 6*time.Second, func() bool {
		select {
		case err := <-run.done:
			t.Fatalf("live sync stopped before the deletion was committed: %v", err)
		default:
		}
		state, err := client.readLiveState(canonical, testWorkspace)
		return err == nil && state.Revision == portal.currentRevision() && len(state.Baseline.Files) == 0
	})
	cancel()
	waitLiveDone(t, run.done)
}

func TestLiveSyncRemoteDeleteConflictsWithLocalEditAndPreservesIt(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	portal.setRemote("delete-race.txt", []byte("baseline"))
	client := portal.client(t)
	ctx, cancel := context.WithCancel(context.Background())
	run := runLiveUntilWatching(t, ctx, client, directory)
	waitSignal(t, run.watching, "initial live sync")
	writeTestFile(t, directory, "delete-race.txt", "local edit")
	portal.deleteRemote("delete-race.txt")
	var conflict *LiveConflictError
	select {
	case err := <-run.done:
		if !errors.As(err, &conflict) || len(conflict.Conflicts) == 0 {
			t.Fatalf("expected remote-delete conflict, got %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for remote-delete conflict")
	}
	cancel()
	assertLocalFile(t, directory, "delete-race.txt", "local edit")
	if _, exists := portal.file("delete-race.txt"); exists {
		t.Fatal("remote delete did not remain applied")
	}
}

func TestInstallLiveReplacementPreservesLeafChangedAtInstallBoundary(t *testing.T) {
	directory := t.TempDir()
	writeTestFile(t, directory, "main.go", "baseline")
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	expectedInfo, _, exists, err := hashClientPath(context.Background(), root, "main.go")
	if err != nil || !exists {
		t.Fatalf("hash baseline: exists=%t err=%v", exists, err)
	}
	expected := makeFile("main.go", []byte("baseline"))
	tempName, err := clientTemporaryName()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, tempName), []byte("remote update"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = installLiveReplacementAtBoundary(context.Background(), root, "main.go", tempName, expectedInfo, expected, func() {
		replacement := filepath.Join(directory, ".editor-save")
		if err := os.WriteFile(replacement, []byte("editor replacement"), 0o644); err != nil {
			t.Errorf("create replacement at install boundary: %v", err)
			return
		}
		if err := os.Rename(replacement, filepath.Join(directory, "main.go")); err != nil {
			t.Errorf("atomically install replacement at install boundary: %v", err)
		}
	})
	if !errors.Is(err, mordesync.ErrDestinationChange) {
		t.Fatalf("install error = %v, want destination change", err)
	}
	assertLocalFile(t, directory, "main.go", "editor replacement")
	if _, err := os.Stat(filepath.Join(directory, tempName)); err != nil {
		t.Fatalf("staged update should remain available after failed install: %v", err)
	}
}

func TestInstallLiveReplacementRestoresBaselineWhenNoClobberInstallFails(t *testing.T) {
	directory := t.TempDir()
	writeTestFile(t, directory, "main.go", "baseline")
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	expectedInfo, _, exists, err := hashClientPath(context.Background(), root, "main.go")
	if err != nil || !exists {
		t.Fatalf("hash baseline: exists=%t err=%v", exists, err)
	}
	expected := makeFile("main.go", []byte("baseline"))
	_, err = installLiveReplacement(context.Background(), root, "main.go", "missing-stage-file", expectedInfo, expected)
	if err == nil {
		t.Fatal("install unexpectedly succeeded without a staged file")
	}
	assertLocalFile(t, directory, "main.go", "baseline")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".morde-sync-tmp-preserved-") {
			t.Fatalf("rollback left an unnecessary preservation file: %q", entry.Name())
		}
	}
}

func TestRemoveLiveBaselinePreservesLeafChangedAtDeleteBoundary(t *testing.T) {
	directory := t.TempDir()
	writeTestFile(t, directory, "main.go", "baseline")
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	expectedInfo, _, exists, err := hashClientPath(context.Background(), root, "main.go")
	if err != nil || !exists {
		t.Fatalf("hash baseline: exists=%t err=%v", exists, err)
	}
	expected := makeFile("main.go", []byte("baseline"))
	_, removed, err := removeLiveBaselineAtBoundary(context.Background(), root, "main.go", expectedInfo, expected, func() {
		replacement := filepath.Join(directory, ".editor-save")
		if err := os.WriteFile(replacement, []byte("editor replacement"), 0o644); err != nil {
			t.Errorf("create replacement at delete boundary: %v", err)
			return
		}
		if err := os.Rename(replacement, filepath.Join(directory, "main.go")); err != nil {
			t.Errorf("atomically install replacement at delete boundary: %v", err)
		}
	})
	if removed || !errors.Is(err, mordesync.ErrDestinationChange) {
		t.Fatalf("remove result removed=%t err=%v, want preserved destination change", removed, err)
	}
	assertLocalFile(t, directory, "main.go", "editor replacement")
}

func TestLiveSyncPausesOnConcurrentChangeAndPreservesBothSides(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	portal.setRemote("conflict.txt", []byte("base"))
	client := portal.client(t)
	ctx, cancel := context.WithCancel(context.Background())
	run := runLiveUntilWatching(t, ctx, client, directory)
	waitSignal(t, run.watching, "initial live sync")
	writeTestFile(t, directory, "conflict.txt", "local edit")
	portal.setRemote("conflict.txt", []byte("remote edit"))
	var conflict *LiveConflictError
	select {
	case err := <-run.done:
		if !errors.As(err, &conflict) || len(conflict.Conflicts) == 0 {
			t.Fatalf("expected live conflict, got %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for live conflict")
	}
	cancel()
	assertLocalFile(t, directory, "conflict.txt", "local edit")
	if got := portal.fileText("conflict.txt"); got != "remote edit" {
		t.Fatalf("remote conflict copy = %q", got)
	}
}

func TestLiveSyncRestartsFromPersistedBaselineAndReconnects(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	portal.setRemote("restart.txt", []byte("base"))
	client := portal.client(t)
	ctx1, cancel1 := context.WithCancel(context.Background())
	run1 := runLiveUntilWatching(t, ctx1, client, directory)
	waitSignal(t, run1.watching, "first live sync")
	cancel1()
	waitLiveDone(t, run1.done)
	createCount := portal.createCount()
	portal.failNextChanges(1)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	run2 := runLiveUntilWatching(t, ctx2, client, directory)
	waitSignal(t, run2.watching, "restarted live sync")
	if portal.createCount() != createCount {
		t.Fatalf("restart created a duplicate sync session: before=%d after=%d", createCount, portal.createCount())
	}
	waitUntil(t, 3*time.Second, func() bool { return portal.changeFailureCount() == 0 })
	portal.setRemote("later.txt", []byte("remote after reconnect"))
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-run2.done:
			t.Fatalf("restarted LiveSync stopped before applying remote change: %v", err)
		default:
		}
		data, err := os.ReadFile(filepath.Join(directory, "later.txt"))
		if err == nil && string(data) == "remote after reconnect" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, err := os.ReadFile(filepath.Join(directory, "later.txt"))
	if err != nil || string(data) != "remote after reconnect" {
		t.Fatalf("remote file was not applied after reconnect: data=%q err=%v", data, err)
	}
	cancel2()
	waitLiveDone(t, run2.done)
}

func TestLiveSyncKeepsLocalEditDuringNetworkOutageAndUploadsAfterReconnect(t *testing.T) {
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	var offline, refused atomic.Bool
	client, err := New(portal.server.URL, "test-device-token", &http.Client{Transport: networkGateTransport{
		base: http.DefaultTransport, offline: &offline, refused: &refused,
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := runLiveUntilWatching(t, ctx, client, directory)
	waitSignal(t, run.watching, "initial live sync")
	before := portal.createCount()
	offline.Store(true)
	writeTestFile(t, directory, "offline.txt", "saved while offline")
	waitUntil(t, 3*time.Second, refused.Load)
	if got := portal.createCount(); got != before {
		t.Fatalf("offline sync created %d sessions, want %d", got, before)
	}
	if got := portal.fileText("offline.txt"); got != "" {
		t.Fatalf("offline edit reached Portal before reconnect: %q", got)
	}
	select {
	case err := <-run.done:
		t.Fatalf("LiveSync stopped during network outage: %v", err)
	default:
	}
	offline.Store(false)
	waitUntil(t, 8*time.Second, func() bool { return portal.fileText("offline.txt") == "saved while offline" })
	data, err := os.ReadFile(filepath.Join(directory, "offline.txt"))
	if err != nil || string(data) != "saved while offline" {
		t.Fatalf("local edit after reconnect = %q, %v", data, err)
	}
	cancel()
	waitLiveDone(t, run.done)
}

func TestLiveSyncResumesUncommittedFileAfterProcessRestart(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("XDG_CACHE_HOME", t.TempDir())
	}
	directory := t.TempDir()
	portal := newLiveTestPortal(t)
	var offline, refused atomic.Bool
	firstClient, err := New(portal.server.URL, "test-device-token", &http.Client{Transport: networkGateTransport{
		base: http.DefaultTransport, offline: &offline, refused: &refused,
	}})
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, stopFirst := context.WithCancel(context.Background())
	firstRun := runLiveUntilWatching(t, firstCtx, firstClient, directory)
	waitSignal(t, firstRun.watching, "initial live sync before process restart")
	offline.Store(true)
	writeTestFile(t, directory, "release-pending.txt", "keep through Host release")
	waitUntil(t, 3*time.Second, refused.Load)
	if got := portal.fileText("release-pending.txt"); got != "" {
		t.Fatalf("uncommitted file reached Portal before restart: %q", got)
	}
	stopFirst()
	waitLiveDone(t, firstRun.done)
	assertLocalFile(t, directory, "release-pending.txt", "keep through Host release")

	secondClient := portal.client(t)
	if runtime.GOOS == "linux" {
		t.Setenv("HOME", t.TempDir()) // A restored container has a fresh non-persistent home.
		if err := secondClient.ValidateLiveBaseline(context.Background(), testWorkspace, directory, nil); err != nil {
			t.Fatalf("persistent cache baseline rejected after home changed: %v", err)
		}
	}
	secondCtx, stopSecond := context.WithCancel(context.Background())
	secondRun := runLiveUntilWatching(t, secondCtx, secondClient, directory)
	waitSignal(t, secondRun.watching, "resumed live sync after process restart")
	waitUntil(t, 8*time.Second, func() bool { return portal.fileText("release-pending.txt") == "keep through Host release" })
	assertLocalFile(t, directory, "release-pending.txt", "keep through Host release")
	stopSecond()
	waitLiveDone(t, secondRun.done)
}

type networkGateTransport struct {
	base    http.RoundTripper
	offline *atomic.Bool
	refused *atomic.Bool
}

func (transport networkGateTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.offline.Load() {
		transport.refused.Store(true)
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	return transport.base.RoundTrip(request)
}

type liveRun struct {
	watching <-chan struct{}
	done     <-chan error
}

func runLiveUntilWatching(t *testing.T, ctx context.Context, client *Client, directory string) liveRun {
	t.Helper()
	watching := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- client.LiveSync(ctx, LiveSyncOptions{WorkspaceID: testWorkspace, Directory: directory, RescanInterval: 5 * time.Second,
			Progress: func(progress Progress) {
				if progress.Stage == "watch" {
					select {
					case watching <- struct{}{}:
					default:
					}
				}
			}})
	}()
	return liveRun{watching: watching, done: done}
}

func waitLiveDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("LiveSync cancellation error = %v", err)
		}
		return err
	case <-time.After(8 * time.Second):
		t.Fatal("LiveSync did not stop")
		return nil
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(8 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func waitUntil(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}

func writeTestFile(t *testing.T, directory, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// liveTestPortal is a small in-memory implementation of the existing Portal
// workspace manifest, changes and sync-session contracts. It is test-only.
type liveTestPortal struct {
	t                       *testing.T
	server                  *httptest.Server
	mu                      sync.Mutex
	revision                int64
	nextID                  int
	current                 map[string]mordesync.File
	snaps                   map[int64]map[string]mordesync.File
	contents                map[string][]byte
	changes                 []workspaceChange
	sessions                map[string]*liveTestSession
	idempotency             map[string]liveCreateReplay
	createKeys              []string
	notify                  chan struct{}
	failChanges             int
	remainingChangeFailures int
	createCalls             int
	changeWait              time.Duration
	changeCalls             int
}

type liveTestSession struct {
	value         SyncSession
	request       createSessionRequest
	blocks        map[string][]byte
	files         map[string]mordesync.File
	uploadDeletes []string
}

type liveCreateReplay struct {
	request   string
	sessionID string
}

func newLiveTestPortal(t *testing.T) *liveTestPortal {
	t.Helper()
	portal := &liveTestPortal{t: t, revision: 0, current: map[string]mordesync.File{}, snaps: map[int64]map[string]mordesync.File{0: {}},
		contents: map[string][]byte{}, sessions: map[string]*liveTestSession{}, idempotency: map[string]liveCreateReplay{}, notify: make(chan struct{})}
	portal.server = httptest.NewServer(http.HandlerFunc(portal.serveHTTP))
	t.Cleanup(portal.server.Close)
	return portal
}

func (p *liveTestPortal) client(t *testing.T) *Client {
	t.Helper()
	value, err := New(p.server.URL, "test-device-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func (p *liveTestPortal) setRemote(name string, data []byte) {
	p.mu.Lock()
	file := makeFile(name, data)
	p.contents[file.SHA256] = append([]byte(nil), data...)
	p.current[strings.ToLower(name)] = file
	p.bumpRevisionLocked([]workspaceChange{{Revision: p.revision + 1, Path: name, Action: "upsert", File: ptrWorkspaceFile(toWorkspaceFile(file))}})
	p.mu.Unlock()
}

func (p *liveTestPortal) renameRemote(oldName, newName string) {
	p.mu.Lock()
	key := strings.ToLower(oldName)
	file, exists := p.current[key]
	if !exists {
		p.mu.Unlock()
		p.t.Fatalf("remote file %q does not exist", oldName)
	}
	file.Path = newName
	p.current[key] = file
	p.bumpRevisionLocked([]workspaceChange{
		{Path: oldName, Action: "delete"},
		{Path: newName, Action: "upsert", File: ptrWorkspaceFile(toWorkspaceFile(file))},
	})
	p.mu.Unlock()
}

func (p *liveTestPortal) deleteRemote(name string) {
	p.mu.Lock()
	delete(p.current, strings.ToLower(name))
	p.bumpRevisionLocked([]workspaceChange{{Revision: p.revision + 1, Path: name, Action: "delete"}})
	p.mu.Unlock()
}

func (p *liveTestPortal) bumpRevisionLocked(changes []workspaceChange) {
	p.revision++
	for i := range changes {
		changes[i].Revision = p.revision
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	p.changes = append(p.changes, changes...)
	p.snaps[p.revision] = cloneLiveFiles(p.current)
	close(p.notify)
	p.notify = make(chan struct{})
}

func (p *liveTestPortal) currentRevision() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.revision
}

func (p *liveTestPortal) currentManifest() mordesync.Manifest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return liveManifest(p.current)
}

func (p *liveTestPortal) file(name string) (mordesync.File, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	file, ok := p.current[strings.ToLower(name)]
	return file, ok
}

func (p *liveTestPortal) fileText(name string) string {
	file, ok := p.file(name)
	if !ok {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return string(p.contents[file.SHA256])
}

func (p *liveTestPortal) createCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.createCalls
}

func (p *liveTestPortal) createdKeys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.createKeys...)
}

func (p *liveTestPortal) failNextChanges(count int) {
	p.mu.Lock()
	p.failChanges = count
	p.remainingChangeFailures = count
	p.mu.Unlock()
}

func (p *liveTestPortal) changeFailureCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.remainingChangeFailures
}

func (p *liveTestPortal) changeCallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changeCalls
}

func (p *liveTestPortal) serveHTTP(w http.ResponseWriter, r *http.Request) {
	p.t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer test-device-token" {
		p.t.Errorf("unexpected authorization %q", got)
	}
	const prefix = "/api/workspaces/" + testWorkspace
	switch {
	case r.Method == http.MethodGet && r.URL.Path == prefix+"/manifest":
		p.handleManifest(w, r)
	case r.Method == http.MethodGet && r.URL.Path == prefix+"/changes":
		p.handleChanges(w, r)
	case r.Method == http.MethodPost && r.URL.Path == prefix+"/sync-sessions":
		p.handleCreate(w, r)
	case strings.HasPrefix(r.URL.Path, prefix+"/sync-sessions/"):
		p.handleSession(w, r)
	default:
		p.t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		http.NotFound(w, r)
	}
}

func (p *liveTestPortal) handleManifest(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	revision := p.revision
	if value := r.URL.Query().Get("revision"); value != "" {
		parsed, _ := strconv.ParseInt(value, 10, 64)
		revision = parsed
	}
	files, exists := p.snaps[revision]
	p.mu.Unlock()
	if !exists {
		http.Error(w, "revision missing", http.StatusConflict)
		return
	}
	pageFiles := workspaceFiles(files)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit > 0 && len(pageFiles) > limit {
		pageFiles = pageFiles[:limit]
	}
	page := map[string]any{"workspace_id": testWorkspace, "revision": revision, "files": pageFiles, "next_cursor": nil}
	writeLiveJSON(p.t, w, http.StatusOK, page)
}

func (p *liveTestPortal) handleChanges(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.changeCalls++
	if p.failChanges > 0 {
		p.failChanges--
		p.remainingChangeFailures = p.failChanges
		p.mu.Unlock()
		http.Error(w, "temporary", http.StatusServiceUnavailable)
		return
	}
	wait := p.changeWait
	p.mu.Unlock()
	if wait <= 0 {
		wait = 80 * time.Millisecond
	}
	cursor, err := decodeLiveCursor(r.URL.Query().Get("after"))
	if err != nil {
		http.Error(w, "bad cursor", http.StatusBadRequest)
		return
	}
	deadline := time.Now().Add(wait)
	var items []workspaceChange
	var next string
	for {
		p.mu.Lock()
		for _, item := range p.changes {
			if item.Revision > cursor.Revision || (item.Revision == cursor.Revision && item.Path > cursor.Path) {
				items = append(items, item)
			}
		}
		if len(items) > 0 {
			last := items[len(items)-1]
			next = encodeLiveCursor(liveCursor{Revision: last.Revision, Path: last.Path})
			p.mu.Unlock()
			break
		}
		if next == "" {
			next = r.URL.Query().Get("after")
		}
		changed := p.notify
		p.mu.Unlock()
		if time.Until(deadline) <= 0 {
			break
		}
		select {
		case <-r.Context().Done():
			return
		case <-changed:
			items = nil
		case <-time.After(time.Until(deadline)):
			break
		}
		if time.Until(deadline) <= 0 {
			break
		}
	}
	if items == nil {
		items = []workspaceChange{}
	}
	writeLiveJSON(p.t, w, http.StatusOK, workspaceChangePage{Items: items, NextCursor: next})
}

func (p *liveTestPortal) handleCreate(w http.ResponseWriter, r *http.Request) {
	var request createSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		p.t.Errorf("decode create sync session: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.createCalls++
	key := r.Header.Get("Idempotency-Key")
	p.createKeys = append(p.createKeys, key)
	encoded, err := json.Marshal(request)
	if err != nil {
		p.t.Errorf("encode create sync session: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if replay, exists := p.idempotency[key]; exists {
		if replay.request != string(encoded) {
			http.Error(w, "idempotency conflict", http.StatusConflict)
			return
		}
		writeLiveJSON(p.t, w, http.StatusCreated, p.sessions[replay.sessionID].value)
		return
	}
	base := p.snaps[request.BaseRevision]
	if base == nil {
		http.Error(w, "base missing", http.StatusConflict)
		return
	}
	local := make(map[string]mordesync.File, len(request.Files))
	for _, file := range request.Files {
		core, err := toCoreFile(file)
		if err != nil {
			p.t.Errorf("bad client file: %v", err)
		}
		local[strings.ToLower(core.Path)] = core
	}
	uploadFiles, downloadFiles, uploadDeletes, downloadDeletes, conflicts := livePlan(request.Policy, base, p.current, local, request.DeletedPaths)
	blocks := make([]TransferBlock, 0)
	seen := make(map[string]bool)
	for _, group := range []struct {
		role  string
		files []mordesync.File
	}{{role: "upload", files: uploadFiles}, {role: "download", files: downloadFiles}} {
		role, files := group.role, group.files
		for _, file := range files {
			for _, block := range file.Blocks {
				key := blockKey(role, block.SHA256)
				if !seen[key] {
					blocks = append(blocks, TransferBlock{Chunk: Chunk{SHA256: block.SHA256, SizeBytes: block.Size}, Role: role})
					seen[key] = true
				}
			}
		}
	}
	var planned int64
	for _, block := range blocks {
		planned += block.Chunk.SizeBytes
	}
	actions := make([]DownloadAction, 0, len(downloadFiles)+len(downloadDeletes))
	for _, file := range downloadFiles {
		digest := file.SHA256
		actions = append(actions, DownloadAction{Path: file.Path, Action: "upsert", SHA256: &digest, SizeBytes: file.Size})
	}
	for _, name := range downloadDeletes {
		actions = append(actions, DownloadAction{Path: name, Action: "delete", SizeBytes: 0})
	}
	sort.Slice(actions, func(i, j int) bool { return actions[i].Path < actions[j].Path })
	p.nextID++
	id := fmt.Sprintf("p%07d", p.nextID)
	value := SyncSession{ID: id, WorkspaceID: testWorkspace, Direction: request.Direction, Policy: request.Policy, State: "planned",
		BaseRevision: request.BaseRevision, TargetRevision: p.revision, Stage: "compare", PlannedBytes: planned,
		TransferBlocks: blocks, DownloadActions: actions, Conflicts: conflicts}
	fileMap := make(map[string]mordesync.File, len(uploadFiles))
	for _, file := range uploadFiles {
		fileMap[strings.ToLower(file.Path)] = file
	}
	p.sessions[id] = &liveTestSession{value: value, request: request, blocks: map[string][]byte{}, files: fileMap, uploadDeletes: uploadDeletes}
	p.idempotency[key] = liveCreateReplay{request: string(encoded), sessionID: id}
	writeLiveJSON(p.t, w, http.StatusCreated, value)
}

func (p *liveTestPortal) handleSession(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/workspaces/"+testWorkspace+"/sync-sessions/"), "/")
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	p.mu.Lock()
	session := p.sessions[parts[0]]
	if session == nil {
		p.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		value := session.value
		p.mu.Unlock()
		writeLiveJSON(p.t, w, http.StatusOK, value)
		return
	}
	if len(parts) == 3 && parts[1] == "blocks" && r.Method == http.MethodPut {
		data, _ := io.ReadAll(r.Body)
		session.blocks[parts[2]] = data
		for i := range session.value.TransferBlocks {
			if session.value.TransferBlocks[i].Role == "upload" && session.value.TransferBlocks[i].Chunk.SHA256 == parts[2] {
				session.value.TransferBlocks[i].Received = true
			}
		}
		p.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) == 3 && parts[1] == "blocks" && r.Method == http.MethodGet {
		data := append([]byte(nil), p.contents[parts[2]]...)
		if data == nil {
			for _, block := range session.blocks {
				if sumSHA256(block) == parts[2] {
					data = block
					break
				}
			}
		}
		p.mu.Unlock()
		if data == nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
		return
	}
	if len(parts) == 4 && parts[1] == "blocks" && parts[3] == "ack" && r.Method == http.MethodPost {
		for i := range session.value.TransferBlocks {
			if session.value.TransferBlocks[i].Role == "download" && session.value.TransferBlocks[i].Chunk.SHA256 == parts[2] {
				session.value.TransferBlocks[i].Received = true
			}
		}
		p.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) == 3 && parts[1] == "download-actions" && parts[2] == "ack" && r.Method == http.MethodPost {
		var ack DownloadAction
		_ = json.NewDecoder(r.Body).Decode(&ack)
		for i := range session.value.DownloadActions {
			if session.value.DownloadActions[i].Path == ack.Path && session.value.DownloadActions[i].Action == ack.Action {
				session.value.DownloadActions[i].Applied = true
			}
		}
		p.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) == 2 && parts[1] == "commit" && r.Method == http.MethodPost {
		for _, transfer := range session.value.TransferBlocks {
			if !transfer.Received {
				p.mu.Unlock()
				http.Error(w, "missing transfer", http.StatusConflict)
				return
			}
		}
		for _, action := range session.value.DownloadActions {
			if !action.Applied {
				p.mu.Unlock()
				http.Error(w, "missing action", http.StatusConflict)
				return
			}
		}
		if len(session.value.Conflicts) == 0 {
			changed := false
			for _, file := range session.request.Files {
				if containsFile(session.filesSlice("upload"), file.Path) {
					var data []byte
					for _, block := range file.Chunks {
						data = append(data, session.blocks[block.SHA256]...)
					}
					core, _ := toCoreFile(file)
					p.current[strings.ToLower(file.Path)] = core
					p.contents[core.SHA256] = data
					changed = true
				}
			}
			for _, deleted := range session.uploadDeletes {
				if p.current[strings.ToLower(deleted)].Path != "" {
					delete(p.current, strings.ToLower(deleted))
					changed = true
				}
			}
			if changed {
				changeValues := make([]workspaceChange, 0)
				for _, file := range session.filesSlice("upload") {
					changeValues = append(changeValues, workspaceChange{Path: file.Path, Action: "upsert", File: ptrWorkspaceFile(toWorkspaceFile(file))})
				}
				for _, deleted := range session.uploadDeletes {
					changeValues = append(changeValues, workspaceChange{Path: deleted, Action: "delete"})
				}
				p.bumpRevisionLocked(changeValues)
			}
		}
		if len(session.value.Conflicts) > 0 {
			session.value.State = "conflict"
		} else {
			session.value.State = "completed"
		}
		session.value.Stage = "complete"
		session.value.TargetRevision = p.revision
		for i := range session.value.TransferBlocks {
			session.value.TransferBlocks[i].Received = true
		}
		value := session.value
		p.mu.Unlock()
		writeLiveJSON(p.t, w, http.StatusOK, value)
		return
	}
	p.mu.Unlock()
	p.t.Errorf("unexpected sync session request %s %s", r.Method, r.URL.Path)
	http.NotFound(w, r)
}

func (s *liveTestSession) filesSlice(role string) []mordesync.File {
	var result []mordesync.File
	for _, file := range s.files {
		result = append(result, file)
	}
	return result
}

func livePlan(policy string, base, current, local map[string]mordesync.File, deleted []string) ([]mordesync.File, []mordesync.File, []string, []string, []Conflict) {
	var uploads, downloads []mordesync.File
	var uploadDeletes, downloadDeletes []string
	var conflicts []Conflict
	deletedSet := make(map[string]bool)
	for _, name := range deleted {
		deletedSet[strings.ToLower(name)] = true
	}
	paths := map[string]bool{}
	for name := range base {
		paths[name] = true
	}
	for name := range current {
		paths[name] = true
	}
	for name := range local {
		paths[name] = true
	}
	for name := range deletedSet {
		paths[name] = true
	}
	ordered := make([]string, 0, len(paths))
	for name := range paths {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		b, bok := base[name]
		r, rok := current[name]
		l, lok := local[name]
		ld := deletedSet[name]
		if policy != "fill_missing" && lok && rok && l.Path != r.Path && strings.EqualFold(l.Path, r.Path) {
			conflicts = append(conflicts, Conflict{Path: r.Path, Reason: "case_collision", LocalSHA256: hashPtr(l, true), RemoteSHA256: hashPtr(r, true)})
			continue
		}
		if policy == "fill_missing" {
			if lok && !rok {
				uploads = append(uploads, l)
			}
			if rok && !lok {
				downloads = append(downloads, r)
			}
			continue
		}
		lc := (ld && bok) || (lok && (!bok || !sameManifestEntry(l, b)))
		rc := bok != rok || (bok && rok && !sameManifestEntry(b, r))
		same := lok == rok && (!lok || sameManifestEntry(l, r))
		if lc && rc && !same {
			conflicts = append(conflicts, Conflict{Path: firstPath(b, bok, l, lok, r, rok), Reason: "content_changed", LocalSHA256: hashPtr(l, lok), RemoteSHA256: hashPtr(r, rok)})
		}
		if lc && !rc {
			if ld {
				if rok {
					uploadDeletes = append(uploadDeletes, firstPath(b, bok, l, lok, r, rok))
				}
			} else if lok && !same {
				uploads = append(uploads, l)
			}
		}
		if rc && !lc {
			if rok {
				downloads = append(downloads, r)
			} else if lok {
				downloadDeletes = append(downloadDeletes, firstPath(b, bok, l, lok, r, rok))
			}
		}
		if lc && rc && same {
			continue
		}
	}
	return uploads, downloads, uploadDeletes, downloadDeletes, conflicts
}

func containsFile(files []mordesync.File, path string) bool {
	for _, file := range files {
		if strings.EqualFold(file.Path, path) {
			return true
		}
	}
	return false
}
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func sameFileContent(a, b mordesync.File) bool {
	return a.SHA256 == b.SHA256 && a.Size == b.Size && a.Mode == b.Mode && (!a.ModTimeKnown || !b.ModTimeKnown || a.ModTimeNS == b.ModTimeNS)
}
func sameManifestEntry(a, b mordesync.File) bool {
	return a.Path == b.Path && a.HardlinkTo == b.HardlinkTo && sameFileContent(a, b)
}
func firstPath(b mordesync.File, bok bool, l mordesync.File, lok bool, r mordesync.File, rok bool) string {
	if bok {
		return b.Path
	}
	if lok {
		return l.Path
	}
	return r.Path
}
func hashPtr(file mordesync.File, ok bool) *string {
	if !ok {
		return nil
	}
	return &file.SHA256
}
func ptrWorkspaceFile(value workspaceFile) *workspaceFile { return &value }
func cloneLiveFiles(values map[string]mordesync.File) map[string]mordesync.File {
	result := make(map[string]mordesync.File, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
func liveManifest(values map[string]mordesync.File) mordesync.Manifest {
	files := make([]mordesync.File, 0, len(values))
	var total int64
	for _, file := range values {
		files = append(files, file)
		total += file.Size
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return mordesync.Manifest{FileCount: int64(len(files)), TotalBytes: total, Files: files}
}
func workspaceFiles(values map[string]mordesync.File) []workspaceFile {
	files := make([]workspaceFile, 0, len(values))
	for _, file := range values {
		files = append(files, toWorkspaceFile(file))
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}
func writeLiveJSON(t *testing.T, writer http.ResponseWriter, status int, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("write test response: %v", err)
	}
}
