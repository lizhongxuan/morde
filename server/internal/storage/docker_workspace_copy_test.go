package storage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mordesync "morde-sync"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

const (
	workspaceCopyOwnerID  = "u1234567"
	workspaceCopySourceID = "w1234567"
	workspaceCopyTargetID = "w7654321"
	workspaceCopyInstance = "instance-a"
)

type workspaceCopyFenceFunc func(context.Context, WorkspaceVolumeCopyRequest) error

func (f workspaceCopyFenceFunc) CheckWorkspaceVolumeCopyReservation(ctx context.Context, request WorkspaceVolumeCopyRequest) error {
	return f(ctx, request)
}

func TestCopyWorkspaceVolumePreservesTreeAndIndependence(t *testing.T) {
	client, _, sourcePath, targetPath := newWorkspaceCopyDockerForTest(t, nil, "")
	contents := []byte("hidden source content")
	sourceFile := filepath.Join(sourcePath, ".private.txt")
	if err := os.WriteFile(sourceFile, contents, 0600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2024, time.June, 1, 2, 3, 4, 0, time.UTC)
	if err := os.Chmod(sourceFile, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sourceFile, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	directoryPath := filepath.Join(sourcePath, "preserved", "nested")
	if err := os.MkdirAll(directoryPath, 0750); err != nil {
		t.Fatal(err)
	}
	directoryMode := uint32(0710)
	directoryTime := time.Date(2023, time.May, 2, 1, 2, 3, 0, time.UTC)
	if err := os.Chmod(directoryPath, os.FileMode(directoryMode)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(directoryPath, directoryTime, directoryTime); err != nil {
		t.Fatal(err)
	}

	files, directories, err := client.CopyWorkspaceVolume(context.Background(), workspaceCopyRequest(t, sourcePath), workspaceCopyFenceFunc(func(context.Context, WorkspaceVolumeCopyRequest) error { return nil }))
	if err != nil {
		t.Fatalf("CopyWorkspaceVolume() = %v", err)
	}
	directoryByPath := make(map[string]workspace.Directory, len(directories))
	for _, directory := range directories {
		directoryByPath[directory.Path] = directory
	}
	rootDirectory, preservedDirectory, nestedDirectory := directoryByPath["."], directoryByPath["preserved"], directoryByPath["preserved/nested"]
	if len(directories) != 3 || !rootDirectory.MetadataKnown || preservedDirectory.Mode == nil || nestedDirectory.Mode == nil || nestedDirectory.ModTimeNS == nil ||
		*nestedDirectory.Mode != directoryMode || *nestedDirectory.ModTimeNS != directoryTime.UnixNano() {
		t.Fatalf("copied directory metadata = %#v", directories)
	}
	if len(files) != 1 || files[0].ModTimeNS == nil || *files[0].ModTimeNS != mtime.UnixNano() {
		t.Fatalf("copied file metadata = %#v", files)
	}
	targetFile := filepath.Join(targetPath, ".private.txt")
	info, err := os.Stat(targetFile)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(targetFile)
	if err != nil || string(got) != string(contents) || info.Mode().Perm() != 0640 || !info.ModTime().Equal(mtime) {
		t.Fatalf("copied file = %q, mode %o, mtime %v, err %v", got, info.Mode().Perm(), info.ModTime(), err)
	}
	if err := os.WriteFile(targetFile, []byte("target only"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(sourceFile)
	if err != nil || string(got) != string(contents) {
		t.Fatalf("source changed after destination edit: %q, %v", got, err)
	}
}

func TestCopyWorkspaceVolumeReadsFileTimestampForUnknownCheckpoint(t *testing.T) {
	client, _, sourcePath, _ := newWorkspaceCopyDockerForTest(t, nil, "")
	file := filepath.Join(sourcePath, "legacy.txt")
	if err := os.WriteFile(file, []byte("old checkpoint"), 0600); err != nil {
		t.Fatal(err)
	}
	modified := time.Date(2022, time.January, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(file, modified, modified); err != nil {
		t.Fatal(err)
	}
	request := workspaceCopyRequest(t, sourcePath)
	request.Files[0].ModTimeNS = nil
	copiedFiles, _, err := client.CopyWorkspaceVolume(context.Background(), request,
		workspaceCopyFenceFunc(func(context.Context, WorkspaceVolumeCopyRequest) error { return nil }))
	if err != nil || len(copiedFiles) != 1 || copiedFiles[0].ModTimeNS == nil || *copiedFiles[0].ModTimeNS != modified.UnixNano() {
		t.Fatalf("copied file metadata = %#v, err=%v; want verified physical timestamp", copiedFiles, err)
	}
}

func TestCopyWorkspaceVolumeRequiresCommittedReservationBeforeDockerAccess(t *testing.T) {
	client, requests, sourcePath, _ := newWorkspaceCopyDockerForTest(t, nil, "")
	wantErr := errors.New("copy is not reserved")
	err := copyWorkspaceVolumeForTest(t, client, sourcePath, workspaceCopyFenceFunc(func(context.Context, WorkspaceVolumeCopyRequest) error {
		return wantErr
	}))
	if !errors.Is(err, wantErr) || *requests != 0 {
		t.Fatalf("CopyWorkspaceVolume() = %v, Docker requests = %d; want reservation error and no Docker access", err, *requests)
	}
}

func TestCopyWorkspaceVolumeRejectsInsufficientCapacityWithoutChangingSource(t *testing.T) {
	client, _, sourcePath, targetPath := newWorkspaceCopyDockerForTest(t, nil, "")
	client.minFreeBytes = int64(^uint64(0) >> 1)
	if err := os.WriteFile(filepath.Join(sourcePath, "source.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	err := copyWorkspaceVolumeForTest(t, client, sourcePath,
		workspaceCopyFenceFunc(func(context.Context, WorkspaceVolumeCopyRequest) error { return nil }))
	if domain.CodeOf(err) != domain.CodeCapacityExceeded {
		t.Fatalf("CopyWorkspaceVolume() error = %v, want capacity_exceeded", err)
	}
	if got, err := os.ReadFile(filepath.Join(sourcePath, "source.txt")); err != nil || string(got) != "keep" {
		t.Fatalf("source after capacity rejection = %q, %v", got, err)
	}
	entries, err := os.ReadDir(targetPath)
	if err != nil || len(entries) != 0 {
		t.Fatalf("destination after capacity rejection = %#v, %v; want empty", entries, err)
	}
}

func TestCopyWorkspaceVolumeRejectsUnsafeVolumes(t *testing.T) {
	for _, test := range []struct {
		name         string
		mutate       func(string, *dockerWorkspaceCopyVolume)
		attachedName string
		wantText     string
	}{
		{
			name: "wrong owner label",
			mutate: func(name string, volume *dockerWorkspaceCopyVolume) {
				if name == "workspace-"+workspaceCopySourceID {
					volume.Labels["morde.owner_id"] = "u8765432"
				}
			},
			wantText: "labels do not match",
		},
		{
			name: "mountpoint outside data root",
			mutate: func(name string, volume *dockerWorkspaceCopyVolume) {
				if name == "workspace-"+workspaceCopyTargetID {
					volume.Mountpoint = t.TempDir()
				}
			},
			wantText: "mountpoint is outside",
		},
		{name: "source attached", attachedName: "workspace-" + workspaceCopySourceID, wantText: "source Workspace volume must be detached"},
		{name: "destination attached", attachedName: "workspace-" + workspaceCopyTargetID, wantText: "destination Workspace volume must be detached"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, _, sourcePath, targetPath := newWorkspaceCopyDockerForTest(t, test.mutate, test.attachedName)
			err := copyWorkspaceVolumeForTest(t, client, sourcePath, workspaceCopyFenceFunc(func(context.Context, WorkspaceVolumeCopyRequest) error { return nil }))
			if err == nil || !containsText(err.Error(), test.wantText) {
				t.Fatalf("CopyWorkspaceVolume() = %v; want error containing %q", err, test.wantText)
			}
			entries, readErr := os.ReadDir(targetPath)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("rejected copy changed destination: entries=%v err=%v", entries, readErr)
			}
		})
	}
}

func TestCopyWorkspaceVolumeRejectsDataRootMismatchAndNonEmptyDestination(t *testing.T) {
	t.Run("data root mismatch", func(t *testing.T) {
		client, _, sourcePath, _ := newWorkspaceCopyDockerForTest(t, nil, "")
		client.dataDir = t.TempDir()
		err := copyWorkspaceVolumeForTest(t, client, sourcePath, workspaceCopyFenceFunc(func(context.Context, WorkspaceVolumeCopyRequest) error { return nil }))
		if err == nil || !containsText(err.Error(), "does not match") {
			t.Fatalf("CopyWorkspaceVolume() = %v; want Docker data root mismatch", err)
		}
	})
	t.Run("incomplete destination is safely cleared for retry", func(t *testing.T) {
		client, _, sourcePath, targetPath := newWorkspaceCopyDockerForTest(t, nil, "")
		marker := filepath.Join(targetPath, "keep.txt")
		if err := os.WriteFile(marker, []byte("existing"), 0600); err != nil {
			t.Fatal(err)
		}
		err := copyWorkspaceVolumeForTest(t, client, sourcePath, workspaceCopyFenceFunc(func(context.Context, WorkspaceVolumeCopyRequest) error { return nil }))
		if err != nil {
			t.Fatalf("CopyWorkspaceVolume() = %v; want partial destination recovery", err)
		}
		if _, statErr := os.Lstat(marker); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("incomplete destination marker survived recovery: %v", statErr)
		}
	})
}

func TestCopyWorkspaceVolumePreservesHardlinkIdentity(t *testing.T) {
	client, _, sourcePath, targetPath := newWorkspaceCopyDockerForTest(t, nil, "")
	first := filepath.Join(sourcePath, "a-first.txt")
	second := filepath.Join(sourcePath, "z-hardlink.txt")
	if err := os.WriteFile(first, []byte("linked"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Fatal(err)
	}
	request := workspaceCopyRequest(t, sourcePath)
	copiedFiles, _, err := client.CopyWorkspaceVolume(context.Background(), request, workspaceCopyFenceFunc(func(context.Context, WorkspaceVolumeCopyRequest) error { return nil }))
	if err != nil || len(copiedFiles) != 2 || copiedFiles[1].HardlinkTo != copiedFiles[0].Path {
		t.Fatalf("CopyWorkspaceVolume() files = %#v, err=%v; want preserved hardlink identity", copiedFiles, err)
	}
	targetFirst, firstErr := os.Stat(filepath.Join(targetPath, "a-first.txt"))
	targetSecond, secondErr := os.Stat(filepath.Join(targetPath, "z-hardlink.txt"))
	if firstErr != nil || secondErr != nil || !os.SameFile(targetFirst, targetSecond) {
		t.Fatalf("copied hardlink identity = %v/%v, errors=%v/%v", targetFirst, targetSecond, firstErr, secondErr)
	}
}

func TestValidateWorkspaceVolumeCopySourceAllowsHardLinksWithoutDestinationSideEffects(t *testing.T) {
	client, _, sourcePath, targetPath := newWorkspaceCopyDockerForTest(t, nil, "")
	first := filepath.Join(sourcePath, "a-first.txt")
	second := filepath.Join(sourcePath, "z-hardlink.txt")
	if err := os.WriteFile(first, []byte("linked"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Fatal(err)
	}
	request := workspaceCopyRequest(t, sourcePath)
	err := client.ValidateWorkspaceVolumeCopySource(context.Background(), request,
		workspaceCopyFenceFunc(func(context.Context, WorkspaceVolumeCopyRequest) error { return nil }))
	if err != nil {
		t.Fatalf("ValidateWorkspaceVolumeCopySource() = %v; want supported hardlink preflight", err)
	}
	if entries, readErr := os.ReadDir(targetPath); readErr != nil || len(entries) != 0 {
		t.Fatalf("preflight changed destination: entries=%v err=%v", entries, readErr)
	}
	left, leftErr := os.Lstat(first)
	right, rightErr := os.Lstat(second)
	if leftErr != nil || rightErr != nil || !os.SameFile(left, right) {
		t.Fatalf("preflight changed source links: left=%v right=%v errors=%v/%v", left, right, leftErr, rightErr)
	}
}

func TestCopyWorkspaceVolumeCleansWhenReservationExpiresAfterCopy(t *testing.T) {
	client, _, sourcePath, targetPath := newWorkspaceCopyDockerForTest(t, nil, "")
	if err := os.WriteFile(filepath.Join(sourcePath, "file.txt"), []byte("copy me"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	err := copyWorkspaceVolumeForTest(t, client, sourcePath, workspaceCopyFenceFunc(func(context.Context, WorkspaceVolumeCopyRequest) error {
		calls++
		if calls == 3 {
			return errors.New("reservation expired")
		}
		return nil
	}))
	if err == nil || !containsText(err.Error(), "reservation changed during copy") {
		t.Fatalf("CopyWorkspaceVolume() = %v; want expired reservation error", err)
	}
	entries, readErr := os.ReadDir(targetPath)
	if calls != 3 || readErr != nil || len(entries) != 0 {
		t.Fatalf("expired copy state = calls:%d entries:%v err:%v", calls, entries, readErr)
	}
}

func copyWorkspaceVolumeForTest(t *testing.T, client *Docker, sourcePath string, reservation WorkspaceVolumeCopyReservation) error {
	t.Helper()
	_, _, err := client.CopyWorkspaceVolume(context.Background(), workspaceCopyRequest(t, sourcePath), reservation)
	return err
}

func workspaceCopyRequest(t *testing.T, sourcePath string) WorkspaceVolumeCopyRequest {
	t.Helper()
	manifest, err := mordesync.Scan(context.Background(), sourcePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	files := make([]workspace.File, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		modified := file.ModTimeNS
		chunks := make([]workspace.Chunk, 0, len(file.Blocks))
		for _, block := range file.Blocks {
			chunks = append(chunks, workspace.Chunk{SHA256: block.SHA256, SizeBytes: block.Size})
		}
		files = append(files, workspace.File{
			Path: file.Path, Kind: file.Kind, SymlinkTarget: file.SymlinkTarget, HardlinkTo: file.HardlinkTo, SizeBytes: file.Size, SHA256: file.SHA256,
			Mode: file.Mode, ModTimeNS: &modified, MetadataKnown: file.MetadataKnown,
			Xattrs: workspaceCopyXattrs(file.Xattrs), ACL: workspaceCopyACL(file.ACL), Chunks: chunks,
		})
	}
	directories := make([]workspace.Directory, 0, len(manifest.Directories))
	for _, directory := range manifest.Directories {
		mode, modified := directory.Mode, directory.ModTimeNS
		directories = append(directories, workspace.Directory{Path: directory.Path, Mode: &mode, ModTimeNS: &modified,
			MetadataKnown: directory.MetadataKnown, Xattrs: workspaceCopyXattrs(directory.Xattrs), ACL: workspaceCopyACL(directory.ACL)})
	}
	return WorkspaceVolumeCopyRequest{
		ReservationID: "copy-reservation-01", ServiceInstanceID: workspaceCopyInstance,
		OwnerID: workspaceCopyOwnerID, SourceWorkspaceID: workspaceCopySourceID, TargetWorkspaceID: workspaceCopyTargetID,
		Files: files, EmptyDirectories: append([]string{}, manifest.EmptyDirectories...), Directories: directories,
	}
}

func newWorkspaceCopyDockerForTest(t *testing.T, mutate func(string, *dockerWorkspaceCopyVolume), attachedName string) (*Docker, *int, string, string) {
	t.Helper()
	dockerRoot := t.TempDir()
	var err error
	dockerRoot, err = filepath.EvalSymlinks(dockerRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dockerRoot, "volumes", "workspace-"+workspaceCopySourceID, "_data"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dockerRoot, "volumes", "workspace-"+workspaceCopyTargetID, "_data"), 0700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(dockerRoot, "volumes", "workspace-"+workspaceCopySourceID, "_data")
	targetPath := filepath.Join(dockerRoot, "volumes", "workspace-"+workspaceCopyTargetID, "_data")
	var requests int
	client := newUnixDockerForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.44/info":
			_ = json.NewEncoder(w).Encode(map[string]string{"DockerRootDir": dockerRoot})
		case r.Method == http.MethodGet && (r.URL.Path == "/v1.44/volumes/workspace-"+workspaceCopySourceID || r.URL.Path == "/v1.44/volumes/workspace-"+workspaceCopyTargetID):
			name := filepath.Base(r.URL.Path)
			workspaceID := workspaceCopySourceID
			if name == "workspace-"+workspaceCopyTargetID {
				workspaceID = workspaceCopyTargetID
			}
			labels, _ := workspaceVolumeLabels(name, workspaceCopyInstance, workspaceCopyOwnerID, workspaceID)
			volume := dockerWorkspaceCopyVolume{Name: name, Driver: "local", Labels: labels, Mountpoint: filepath.Join(dockerRoot, "volumes", name, "_data")}
			if mutate != nil {
				mutate(name, &volume)
			}
			_ = json.NewEncoder(w).Encode(volume)
		case r.Method == http.MethodGet && r.URL.Path == "/v1.44/containers/json":
			if r.URL.Query().Get("all") != "1" {
				http.Error(w, "all containers required", http.StatusBadRequest)
				return
			}
			if attachedName == "" {
				_ = json.NewEncoder(w).Encode([]any{})
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]string{{"Id": "container-1"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1.44/containers/container-1/json":
			_ = json.NewEncoder(w).Encode(map[string]any{"Mounts": []any{map[string]string{"Type": "volume", "Name": attachedName}}})
		default:
			http.NotFound(w, r)
		}
	}))
	client.dataDir = dockerRoot
	return client, &requests, sourcePath, targetPath
}

func containsText(value, needle string) bool { return strings.Contains(value, needle) }
