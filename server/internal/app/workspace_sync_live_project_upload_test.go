package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"morde/server/internal/domain/workspace"
)

type recordingWorkspaceCurrentDirectory struct {
	cloudDSHLiveWorkspaceDirectory
	moves int
}

type failOnceLiveWorkspaceDirectory struct {
	cloudDSHLiveWorkspaceDirectory
	failPath string
	failErr  error
	failed   bool
	writes   int
}

func (d *failOnceLiveWorkspaceDirectory) WriteRegularFileFrom(ctx context.Context, ownerID, workspaceID, relative string,
	source io.Reader, size int64, digest string, mode uint32, expectedSHA string) error {
	d.writes++
	if relative == d.failPath && !d.failed {
		d.failed = true
		return d.failErr
	}
	return d.cloudDSHLiveWorkspaceDirectory.WriteRegularFileFrom(ctx, ownerID, workspaceID, relative, source, size, digest, mode, expectedSHA)
}

func (r *recordingWorkspaceCurrentDirectory) MoveRegularFile(ctx context.Context, ownerID, workspaceID, sourcePath, targetPath, expectedSHA256 string, expectedMode uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.moves++
	source := filepath.Join(r.root, filepath.FromSlash(sourcePath))
	target := filepath.Join(r.root, filepath.FromSlash(targetPath))
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != expectedSHA256 {
		return errors.New("source file changed")
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if uint32(info.Mode().Perm()) != expectedMode {
		return errors.New("source file mode changed")
	}
	if _, err := os.Lstat(target); err == nil {
		return errors.New("target file already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(source, target)
}

func TestApplyLiveProjectUploadWritesSharedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "project", "demo"), 0o750); err != nil {
		t.Fatal(err)
	}
	blocks := newSyncBlockMemory()
	content := []byte("package main\n")
	digest := sha256.Sum256(content)
	digestText := hex.EncodeToString(digest[:])
	chunk := workspace.Chunk{SHA256: digestText, SizeBytes: int64(len(content))}
	if _, err := blocks.PutBlock("w1234567", digestText, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	service := &WorkspaceSyncService{blocks: blocks, currentDirectory: cloudDSHLiveWorkspaceDirectory{root: root}, maxOwnerBytes: 1 << 20}
	session := workspace.SyncSession{
		OwnerID: "u1234567", WorkspaceID: "w1234567", FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Direction: workspace.SyncUpload, Scope: []string{"project/demo"},
		UploadFiles: []workspace.File{{Path: "project/demo/main.go", SizeBytes: int64(len(content)), SHA256: digestText,
			Mode: 0o644, Chunks: []workspace.Chunk{chunk}}},
	}
	if conflicts, err := service.applyLiveProjectUpload(context.Background(), WorkspaceSyncDirectCommitPlan{Session: session}); err != nil || len(conflicts) != 0 {
		t.Fatalf("applyLiveProjectUpload() = conflicts %#v, err %v", conflicts, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "project", "demo", "main.go")); err != nil || string(got) != string(content) {
		t.Fatalf("shared file = %q, err %v; want %q", got, err, content)
	}
}

func TestApplyLiveProjectUploadRetriesAfterPartialFilesystemApply(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "project", "demo")
	if err := os.MkdirAll(projectPath, 0o750); err != nil {
		t.Fatal(err)
	}
	oldContent := []byte("previous file")
	oldFile := syncServiceFile("project/demo/a.txt", oldContent)
	oldFile.Kind = workspace.FileKindRegular
	if err := os.WriteFile(filepath.Join(projectPath, "a.txt"), oldContent, 0o644); err != nil {
		t.Fatal(err)
	}
	blocks := newSyncBlockMemory()
	makeFile := func(path string, content []byte) workspace.File {
		t.Helper()
		file := syncServiceFile(path, content)
		file.Kind = workspace.FileKindRegular
		if len(file.Chunks) != 0 {
			if _, err := blocks.PutBlock("w1234567", file.Chunks[0].SHA256, bytes.NewReader(content)); err != nil {
				t.Fatalf("store upload block for %s: %v", path, err)
			}
		}
		return file
	}
	first := makeFile("project/demo/a.txt", []byte("first file"))
	second := makeFile("project/demo/b.txt", []byte("second file"))
	priorSession := workspace.SyncSession{Scope: []string{"project/demo"}, Files: []workspace.File{oldFile}}
	injected := errors.New("temporary write interruption")
	directory := &failOnceLiveWorkspaceDirectory{
		cloudDSHLiveWorkspaceDirectory: cloudDSHLiveWorkspaceDirectory{root: root},
		failPath:                       second.Path, failErr: injected,
	}
	service := &WorkspaceSyncService{blocks: blocks, currentDirectory: directory, maxOwnerBytes: 1 << 20}
	priorFiles, _, priorDirectories, err := service.readLiveProjectManifest(context.Background(), "u1234567", "w1234567", "project/demo")
	if err != nil {
		t.Fatal(err)
	}
	baseline, ok := projectLiveBaselineForSource(priorSession, priorFiles, priorDirectories)
	if !ok {
		t.Fatal("could not create previous applied project baseline")
	}
	session := workspace.SyncSession{
		OwnerID: "u1234567", WorkspaceID: "w1234567", FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Direction: workspace.SyncUpload, Scope: []string{"project/demo"}, SourceBaselineSessionID: "s1234567",
		Files:       []workspace.File{first, second},
		UploadFiles: []workspace.File{first, second},
	}
	baselineRecords := 0
	var recorded WorkspaceSyncProjectLiveBaseline
	plan := WorkspaceSyncDirectCommitPlan{Session: session, PinnedFiles: []workspace.File{oldFile}, ProjectLiveBaseline: &baseline,
		RecordProjectLiveBaseline: func(value WorkspaceSyncProjectLiveBaseline) error {
			baselineRecords++
			recorded = value
			return nil
		}}
	if conflicts, err := service.applyLiveProjectUpload(context.Background(), plan); !errors.Is(err, injected) || len(conflicts) != 0 {
		t.Fatalf("first apply = conflicts %#v, err %v; want injected interruption", conflicts, err)
	}
	if got, err := os.ReadFile(filepath.Join(projectPath, "a.txt")); err != nil || string(got) != "first file" {
		t.Fatalf("first file after partial apply = %q, err %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(projectPath, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("second file after partial apply stat = %v; want not-exist", err)
	}
	if baselineRecords != 0 {
		t.Fatalf("baseline records after partial apply = %d; want none", baselineRecords)
	}

	if conflicts, err := service.applyLiveProjectUpload(context.Background(), plan); err != nil || len(conflicts) != 0 {
		t.Fatalf("retry after partial apply = conflicts %#v, err %v", conflicts, err)
	}
	if baselineRecords != 1 || directory.writes != 3 {
		t.Fatalf("successful retry baseline records=%d writes=%d; want one baseline and only the failed path retried", baselineRecords, directory.writes)
	}
	if len(recorded.Files) != 2 || recorded.Files[0].Path != first.Path || recorded.Files[1].Path != second.Path {
		t.Fatalf("recorded applied baseline = %#v; want both source files", recorded.Files)
	}
	if got, err := os.ReadFile(filepath.Join(projectPath, "a.txt")); err != nil || string(got) != "first file" {
		t.Fatalf("first file after retry = %q, err %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(projectPath, "b.txt")); err != nil || string(got) != "second file" {
		t.Fatalf("second file after retry = %q, err %v", got, err)
	}
}

func TestProjectLiveBaselinePendingUploadKeepsUnrelatedCloudChangesAsConflicts(t *testing.T) {
	oldA := syncServiceFile("project/demo/a.txt", []byte("old a"))
	oldA.Kind = workspace.FileKindRegular
	oldB := syncServiceFile("project/demo/b.txt", []byte("old b"))
	oldB.Kind = workspace.FileKindRegular
	oldC := syncServiceFile("project/demo/c.txt", []byte("same c"))
	oldC.Kind, oldC.Mode, oldC.MetadataKnown = workspace.FileKindRegular, 0o644, true
	oldC.Xattrs = []workspace.ExtendedAttribute{{Name: "user.sync", Value: []byte("baseline")}}
	desiredA := syncServiceFile("project/demo/a.txt", []byte("desired a"))
	desiredA.Kind = workspace.FileKindRegular
	desiredB := syncServiceFile("project/demo/b.txt", []byte("desired b"))
	desiredB.Kind = workspace.FileKindRegular
	desiredC := oldC
	remoteB := syncServiceFile("project/demo/b.txt", []byte("remote b"))
	remoteB.Kind = workspace.FileKindRegular
	remoteC := oldC
	remoteC.Xattrs = []workspace.ExtendedAttribute{{Name: "user.sync", Value: []byte("remote change")}}
	mode, before, after := uint32(0o750), int64(10), int64(11)
	baseline := WorkspaceSyncProjectLiveBaseline{
		Files:       []workspace.File{oldA, oldB, oldC},
		Directories: []workspace.Directory{{Path: "project/demo", Mode: &mode, ModTimeNS: &before, MetadataKnown: true}},
	}
	liveDirectories := []workspace.Directory{{Path: "project/demo", Mode: &mode, ModTimeNS: &after, MetadataKnown: true}}
	session := workspace.SyncSession{
		UploadFiles: []workspace.File{desiredA, desiredB, desiredC},
	}
	conflicts := projectLiveBaselineConflictsForPendingUpload(baseline, []workspace.File{desiredA, remoteB, remoteC}, liveDirectories, session)
	if len(conflicts) != 2 || conflicts[0].Path != oldB.Path || conflicts[0].Reason != "cloud_changed" ||
		conflicts[1].Path != oldC.Path || conflicts[1].Reason != "cloud_changed" {
		t.Fatalf("pending upload baseline conflicts = %#v; want unrelated content and metadata changes at %q and %q",
			conflicts, oldB.Path, oldC.Path)
	}
}

func TestApplyLiveProjectUploadMovesUnchangedRenamedFileInSharedDirectory(t *testing.T) {
	ownerID, workspaceID := "u1234567", "w1234567"
	root := t.TempDir()
	projectDir := filepath.Join(root, "project", "demo")
	if err := os.MkdirAll(projectDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "old.txt"), []byte("unchanged content"), 0o644); err != nil {
		t.Fatal(err)
	}
	content := []byte("unchanged content")
	digest := sha256.Sum256(content)
	digestText := hex.EncodeToString(digest[:])
	blocks := newSyncBlockMemory()
	if _, err := blocks.PutBlock(workspaceID, digestText, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	directory := &recordingWorkspaceCurrentDirectory{cloudDSHLiveWorkspaceDirectory: cloudDSHLiveWorkspaceDirectory{root: root}}
	service := &WorkspaceSyncService{blocks: blocks, currentDirectory: directory, maxOwnerBytes: 1 << 20}
	file := workspace.File{Kind: workspace.FileKindRegular, Path: "project/demo/old.txt", SizeBytes: int64(len(content)),
		SHA256: digestText, Mode: 0o644, MetadataKnown: true, Chunks: []workspace.Chunk{{SHA256: digestText, SizeBytes: int64(len(content))}}}
	desired := file
	desired.Path = "project/demo/new.txt"
	session := workspace.SyncSession{OwnerID: ownerID, WorkspaceID: workspaceID, FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Direction: workspace.SyncUpload, Scope: []string{"project/demo"}, UploadFiles: []workspace.File{desired},
		UploadDeletedPaths: []string{file.Path}}
	plan := WorkspaceSyncDirectCommitPlan{Session: session, PinnedFiles: []workspace.File{file}}
	if conflicts, err := service.applyLiveProjectUpload(context.Background(), plan); err != nil || len(conflicts) != 0 {
		t.Fatalf("rename project upload = conflicts %#v, err %v", conflicts, err)
	}
	if directory.moves != 1 {
		t.Fatalf("file moves = %d; want exactly one atomic move", directory.moves)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old path after move = %v; want not-exist", err)
	}
	got, err := os.ReadFile(filepath.Join(projectDir, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("moved file content = %q; want %q", got, content)
	}
}

func TestApplyLiveProjectUploadPreservesConcurrentChanges(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "project", "demo")
	if err := os.MkdirAll(projectPath, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projectPath, "main.go")
	remote := []byte("cloud changed")
	if err := os.WriteFile(path, remote, 0o644); err != nil {
		t.Fatal(err)
	}
	local := []byte("pc changed")
	localDigest := sha256.Sum256(local)
	localDigestText := hex.EncodeToString(localDigest[:])
	blocks := newSyncBlockMemory()
	if _, err := blocks.PutBlock("w1234567", localDigestText, bytes.NewReader(local)); err != nil {
		t.Fatal(err)
	}
	base := []byte("original")
	baseDigest := sha256.Sum256(base)
	service := &WorkspaceSyncService{blocks: blocks, currentDirectory: cloudDSHLiveWorkspaceDirectory{root: root}, maxOwnerBytes: 1 << 20}
	session := workspace.SyncSession{
		OwnerID: "u1234567", WorkspaceID: "w1234567", FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Direction: workspace.SyncUpload, Scope: []string{"project/demo"},
		UploadFiles: []workspace.File{{Path: "project/demo/main.go", SizeBytes: int64(len(local)), SHA256: localDigestText,
			Mode: 0o644, Chunks: []workspace.Chunk{{SHA256: localDigestText, SizeBytes: int64(len(local))}}}},
	}
	plan := WorkspaceSyncDirectCommitPlan{Session: session, PinnedFiles: []workspace.File{{Path: "project/demo/main.go", SizeBytes: int64(len(base)),
		SHA256: hex.EncodeToString(baseDigest[:]), Mode: 0o644}}}
	conflicts, err := service.applyLiveProjectUpload(context.Background(), plan)
	if err != nil || len(conflicts) != 1 || conflicts[0].Path != "project/demo/main.go" {
		t.Fatalf("concurrent upload result = conflicts %#v, err %v", conflicts, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(remote) {
		t.Fatalf("concurrent file = %q, err %v; cloud change must be preserved", got, err)
	}
}

func TestApplyLiveProjectUploadDeletesNestedTreeBottomUp(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "project", "demo", "sub", "nested")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	content := []byte("remove me")
	filePath := filepath.Join(nested, "file.txt")
	if err := os.WriteFile(filePath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	service := &WorkspaceSyncService{currentDirectory: cloudDSHLiveWorkspaceDirectory{root: root}, maxOwnerBytes: 1 << 20}
	session := workspace.SyncSession{
		OwnerID: "u1234567", WorkspaceID: "w1234567", FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Direction: workspace.SyncUpload, Scope: []string{"project/demo"},
		UploadDeletedPaths:       []string{"project/demo/sub/nested/file.txt"},
		UploadDeletedDirectories: []string{"project/demo/sub", "project/demo/sub/nested"},
	}
	plan := WorkspaceSyncDirectCommitPlan{Session: session,
		PinnedFiles:             []workspace.File{{Path: "project/demo/sub/nested/file.txt", SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]), Mode: 0o644}},
		PinnedDirectoryMetadata: []workspace.Directory{{Path: "project/demo/sub"}, {Path: "project/demo/sub/nested"}},
	}
	if conflicts, err := service.applyLiveProjectUpload(context.Background(), plan); err != nil || len(conflicts) != 0 {
		t.Fatalf("nested deletion = conflicts %#v, err %v", conflicts, err)
	}
	if _, err := os.Stat(filepath.Join(root, "project", "demo", "sub")); !os.IsNotExist(err) {
		t.Fatalf("deleted tree remains: stat err %v", err)
	}
}

func TestProjectLiveBaselineIncludesImplicitAndEmptyDirectories(t *testing.T) {
	mode, originalTime, changedTime := uint32(0o755), int64(10), int64(11)
	file := workspace.File{Kind: workspace.FileKindRegular, Path: "project/demo/nested/file.txt",
		SizeBytes: 0, SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", Mode: 0o644}
	source := workspace.SyncSession{Scope: []string{"project/demo"}, Files: []workspace.File{file},
		EmptyDirectories: []string{"project/demo/empty"}}
	directories := []workspace.Directory{{Path: "project/demo/nested", Mode: &mode, ModTimeNS: &originalTime, MetadataKnown: true},
		{Path: "project/demo/empty", Mode: &mode, ModTimeNS: &originalTime, MetadataKnown: true}}
	baseline, ok := projectLiveBaselineForSource(source, []workspace.File{file}, directories)
	if !ok || len(baseline.Directories) != 2 || !projectLiveBaselineMatchesSource(baseline, source) {
		t.Fatalf("incomplete live baseline: %#v, ok=%v", baseline, ok)
	}
	directories[1].ModTimeNS = &changedTime
	conflicts := projectLiveBaselineConflicts(baseline, []workspace.File{file}, directories)
	if len(conflicts) != 1 || conflicts[0].Path != "project/demo/empty" {
		t.Fatalf("empty directory cloud timestamp change was not protected: %#v", conflicts)
	}
}
