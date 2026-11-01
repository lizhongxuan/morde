package runtimefiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mordesync "morde-sync"
)

func TestTaskWorkspaceManifestPageAndBlockExport(t *testing.T) {
	workspace, taskID, viewID, inputDigest, viewPath := openTaskExportWorkspace(t, map[string]string{
		"a.txt": "alpha", "b.txt": "beta",
	})
	if err := os.WriteFile(filepath.Join(viewPath, "b.txt"), []byte("output-beta"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantManifest, wantDigest, err := scanTaskWorkspace(context.Background(), viewPath)
	if err != nil {
		t.Fatal(err)
	}
	first, err := workspace.TaskWorkspaceManifestPage(context.Background(), taskID, viewID, "", "", 1)
	if err != nil {
		t.Fatalf("first manifest page: %v", err)
	}
	if first.TaskID != taskID || first.ViewID != viewID || first.WorkspaceID != "wabc1234" ||
		first.BindingVersion != 3 || first.InputRevision != 4 || first.WorkspaceCheckpointRevision != 9 ||
		first.InputManifestSHA256 != inputDigest || first.ManifestSHA256 != wantDigest || len(first.Files) != 1 ||
		first.Files[0].Path != "a.txt" || first.NextCursor != "a.txt" {
		t.Fatalf("unexpected first page: %+v", first)
	}
	if got, err := taskManifestSHA256(wantManifest); err != nil || got != first.ManifestSHA256 {
		t.Fatalf("manifest digest = %q, %v; want %q", got, err, first.ManifestSHA256)
	}
	second, err := workspace.TaskWorkspaceManifestPage(context.Background(), taskID, viewID, first.ManifestSHA256, first.NextCursor, 1)
	if err != nil {
		t.Fatalf("second manifest page: %v", err)
	}
	if len(second.Files) != 1 || second.Files[0].Path != "b.txt" || second.NextCursor != "" ||
		second.Files[0].SizeBytes != int64(len("output-beta")) || len(second.Files[0].Chunks) != 1 ||
		second.Files[0].Chunks[0].SHA256 != testSHA256("output-beta") {
		t.Fatalf("unexpected second page: %+v", second)
	}
	block, err := workspace.ReadTaskWorkspaceBlock(context.Background(), taskID, viewID, first.ManifestSHA256, testSHA256("output-beta"))
	if err != nil || string(block) != "output-beta" {
		t.Fatalf("read block = %q, %v", block, err)
	}
}

func TestTaskWorkspaceCheckpointAdvancePreservesPrivateFiles(t *testing.T) {
	workspace, taskID, viewID, inputDigest, viewPath := openTaskExportWorkspace(t, map[string]string{
		"a.txt": "alpha", "b.txt": "beta",
	})
	want := []byte("task-private-output")
	if err := os.WriteFile(filepath.Join(viewPath, "b.txt"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := workspace.PrepareTaskWorkspaceView(context.Background(), "habc1234", taskID, "wabc1234", viewID, 3, 4, 10, inputDigest); err != nil {
		t.Fatalf("advance Task Workspace checkpoint: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(viewPath, "b.txt"))
	if err != nil || string(got) != string(want) {
		t.Fatalf("private Task file after checkpoint advance = %q, %v", got, err)
	}
	page, err := workspace.TaskWorkspaceManifestPage(context.Background(), taskID, viewID, "", "", 10)
	if err != nil || page.WorkspaceCheckpointRevision != 10 || page.InputManifestSHA256 != inputDigest || len(page.Files) != 2 {
		t.Fatalf("manifest after checkpoint advance = %#v, %v", page, err)
	}
}

func TestTaskWorkspaceViewSurvivesRuntimeFilesystemReopen(t *testing.T) {
	workspace, taskID, viewID, inputDigest, viewPath := openTaskExportWorkspace(t, map[string]string{"result.txt": "before"})
	if err := os.WriteFile(filepath.Join(viewPath, "result.txt"), []byte("published"), 0o600); err != nil {
		t.Fatal(err)
	}
	rootPath, taskRootPath := workspace.rootPath, workspace.taskRootPath
	if err := workspace.PrepareTaskWorkspaceView(context.Background(), "habc1234", taskID, "wabc1234", viewID, 3, 4, 10, inputDigest); err != nil {
		t.Fatalf("advance checkpoint before Runtime restart: %v", err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatalf("close Task Workspace before Runtime restart: %v", err)
	}

	reopened, err := OpenTaskWorkspace(rootPath, taskRootPath, uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatalf("reopen Task Workspace after Runtime restart: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.PrepareTaskWorkspaceView(context.Background(), "habc1234", taskID, "wabc1234", viewID, 3, 4, 9, inputDigest); !errors.Is(err, ErrTaskWorkspaceView) {
		t.Fatalf("stale checkpoint after Runtime restart = %v; want fail-closed", err)
	}
	if err := reopened.PrepareTaskWorkspaceView(context.Background(), "habc1234", taskID, "wabc1234", viewID, 3, 4, 10, inputDigest); err != nil {
		t.Fatalf("restore persisted Task Workspace view: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(viewPath, "result.txt")); err != nil || string(got) != "published" {
		t.Fatalf("private file after Runtime restart = %q, %v", got, err)
	}
	page, err := reopened.TaskWorkspaceManifestPage(context.Background(), taskID, viewID, "", "", 10)
	if err != nil || page.WorkspaceCheckpointRevision != 10 || len(page.Files) != 1 || page.Files[0].Path != "result.txt" {
		t.Fatalf("restored Task Workspace checkpoint = %#v, %v", page, err)
	}
}

func TestTaskWorkspaceCopiesSynchronizedReadableModes(t *testing.T) {
	root := t.TempDir()
	workspaceID, taskID := "wabc1234", "qabc1234"
	source := filepath.Join(root, workspaceID)
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "notes", "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := OpenTaskWorkspace(root, filepath.Join(root, ".morde-task-views"), uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workspace.Close() })
	_, digest, err := scanTaskWorkspace(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	viewID := "v" + strings.Repeat("a", 64)
	if err := workspace.PrepareTaskWorkspaceView(context.Background(), "habc1234", taskID, workspaceID, viewID, 1, 0, 0, digest); err != nil {
		t.Fatalf("prepare readable mode Task Workspace: %v", err)
	}
}

func TestTaskWorkspaceCopiesAndExportsEmptyDirectories(t *testing.T) {
	root := t.TempDir()
	workspaceID, taskID := "wabc1234", "qabc1234"
	source := filepath.Join(root, workspaceID)
	if err := os.MkdirAll(filepath.Join(source, "docs", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "empty-root"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "readme.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := OpenTaskWorkspace(root, filepath.Join(root, ".morde-task-views"), uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workspace.Close() })
	manifest, digest, err := scanTaskWorkspace(context.Background(), source)
	if err != nil || len(manifest.EmptyDirectories) != 2 {
		t.Fatalf("source manifest empty directories = %v, %v", manifest.EmptyDirectories, err)
	}
	viewID := "v" + strings.Repeat("a", 64)
	if err := workspace.PrepareTaskWorkspaceView(context.Background(), "habc1234", taskID, workspaceID, viewID, 1, 0, 0, digest); err != nil {
		t.Fatalf("prepare Task view with empty directories: %v", err)
	}
	viewPath := filepath.Join(workspace.taskRootPath, taskID, viewID)
	for _, directory := range manifest.EmptyDirectories {
		info, err := os.Stat(filepath.Join(viewPath, filepath.FromSlash(directory)))
		if err != nil || !info.IsDir() {
			t.Fatalf("copied empty directory %q = %v, %v", directory, info, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(viewPath, "output", "empty"), 0o770); err != nil {
		t.Fatal(err)
	}
	first, err := workspace.TaskWorkspaceManifestPage(context.Background(), taskID, viewID, "", "", 2)
	if err != nil || len(first.EmptyDirectories) != 2 || len(first.Files) != 0 || first.NextCursor == "" {
		t.Fatalf("first output page = %#v, %v", first, err)
	}
	second, err := workspace.TaskWorkspaceManifestPage(context.Background(), taskID, viewID, first.ManifestSHA256, first.NextCursor, 2)
	if err != nil || second.NextCursor != "" || len(second.Files) != 1 || second.Files[0].Path != "readme.txt" ||
		len(second.EmptyDirectories) != 1 || second.EmptyDirectories[0] != "output/empty" {
		t.Fatalf("second output page = %#v, %v", second, err)
	}
}

func TestTaskManifestTreeSHA256CanonicalizesFileAndDirectoryOrder(t *testing.T) {
	first := mordesync.Manifest{
		FileCount: 2, TotalBytes: 0,
		Files: []mordesync.File{
			{Path: "z.txt", Mode: 0o600, SHA256: testSHA256("")},
			{Path: "a.txt", Mode: 0o600, SHA256: testSHA256("")},
		},
		EmptyDirectories: []string{"b/empty", "a/empty"},
	}
	second := mordesync.Manifest{
		FileCount: 2, TotalBytes: 0,
		Files: []mordesync.File{
			{Path: "a.txt", Mode: 0o600, SHA256: testSHA256("")},
			{Path: "z.txt", Mode: 0o600, SHA256: testSHA256("")},
		},
		EmptyDirectories: []string{"a/empty", "b/empty"},
	}
	firstDigest, err := taskManifestSHA256(first)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := taskManifestSHA256(second)
	if err != nil || firstDigest != secondDigest {
		t.Fatalf("tree digest depends on directory order: %q, %q, %v", firstDigest, secondDigest, err)
	}
}

func TestTaskManifestSHA256FileOnlyKeepsV1Digest(t *testing.T) {
	manifest := mordesync.Manifest{
		FileCount: 1,
		Files:     []mordesync.File{{Path: "a.txt", Mode: 0o600, SHA256: testSHA256("")}},
	}
	digest, err := taskManifestSHA256(manifest)
	if err != nil || digest != "411449d004e66ac3f2a4ae4f408ebc3b187b651078e43d098da6704c3ecf0bfe" {
		t.Fatalf("file-only manifest digest = %q, %v; want existing V1 digest", digest, err)
	}
}

func TestTaskManifestSHA256RejectsUnsafeEmptyDirectoryPaths(t *testing.T) {
	for _, path := range []string{"../escape", "a/../escape", "a\\escape", "same"} {
		t.Run(path, func(t *testing.T) {
			manifest := mordesync.Manifest{EmptyDirectories: []string{path}}
			if path == "same" {
				manifest.FileCount = 1
				manifest.Files = []mordesync.File{{Path: "same", Mode: 0o600, SHA256: testSHA256("")}}
			}
			if _, err := taskManifestSHA256(manifest); err == nil {
				t.Fatalf("taskManifestSHA256 accepted unsafe/colliding directory path %q", path)
			}
		})
	}
	if _, err := taskManifestSHA256(mordesync.Manifest{EmptyDirectories: []string{"same", "same"}}); err == nil {
		t.Fatal("taskManifestSHA256 accepted duplicate empty directory paths")
	}
}

func TestTaskWorkspaceBlockExportFailsClosedWhenBytesChangeOrDisappear(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(string) error
	}{
		{name: "mutated", change: func(path string) error { return os.WriteFile(path, []byte("changed"), 0o600) }},
		{name: "missing", change: os.Remove},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace, taskID, viewID, _, viewPath := openTaskExportWorkspace(t, map[string]string{"file.txt": "original"})
			page, err := workspace.TaskWorkspaceManifestPage(context.Background(), taskID, viewID, "", "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if err := test.change(filepath.Join(viewPath, "file.txt")); err != nil {
				t.Fatal(err)
			}
			if _, err := workspace.ReadTaskWorkspaceBlock(context.Background(), taskID, viewID, page.ManifestSHA256, testSHA256("original")); err == nil {
				t.Fatal("ReadTaskWorkspaceBlock() succeeded after the referenced bytes changed")
			}
		})
	}
}

func TestTaskWorkspaceExportRejectsBoundsAndTraversal(t *testing.T) {
	workspace, taskID, viewID, _, viewPath := openTaskExportWorkspace(t, map[string]string{"file.txt": "inside"})
	if _, err := workspace.TaskWorkspaceManifestPage(context.Background(), taskID, viewID, "", "", 0); err == nil {
		t.Fatal("manifest page accepted a zero limit")
	}
	if _, err := workspace.TaskWorkspaceManifestPage(context.Background(), taskID, viewID, "", "../outside", 1); err == nil {
		t.Fatal("manifest page accepted a traversal cursor")
	}
	if _, err := workspace.TaskWorkspaceManifestPage(context.Background(), taskID, viewID, "", "", TaskWorkspaceManifestPageFilesMax+1); err == nil {
		t.Fatal("manifest page accepted a limit above its bound")
	}
	page, err := workspace.TaskWorkspaceManifestPage(context.Background(), taskID, viewID, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.ReadTaskWorkspaceBlock(context.Background(), taskID, viewID, page.ManifestSHA256, strings.Repeat("f", 64)); err == nil {
		t.Fatal("block export returned bytes for a digest absent from the manifest")
	}
	if err := os.Remove(filepath.Join(viewPath, "file.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(viewPath), "outside.txt"), filepath.Join(viewPath, "file.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(viewPath), "outside.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.ReadTaskWorkspaceBlock(context.Background(), taskID, viewID, page.ManifestSHA256, testSHA256("inside")); err == nil {
		t.Fatal("block export followed a symlink outside the manifest file")
	}
}

func TestTaskWorkspaceManifestEnforcesFiveGiBBound(t *testing.T) {
	chunks := make([]taskManifestChunk, 0, int(maxTaskViewBytes/mordesync.BlockSize)+1)
	remaining := maxTaskViewBytes + 1
	for remaining > 0 {
		size := min(int64(mordesync.BlockSize), remaining)
		chunks = append(chunks, taskManifestChunk{SHA256: strings.Repeat("a", 64), SizeBytes: size})
		remaining -= size
	}
	manifest := mordesync.Manifest{
		FileCount:  1,
		TotalBytes: maxTaskViewBytes + 1,
		Files: []mordesync.File{{
			Path: "large.bin", Size: maxTaskViewBytes + 1, Mode: 0o600, SHA256: strings.Repeat("b", 64),
			Blocks: make([]mordesync.Block, len(chunks)),
		}},
	}
	for index, chunk := range chunks {
		manifest.Files[0].Blocks[index] = mordesync.Block{
			Offset: int64(index) * mordesync.BlockSize, Size: chunk.SizeBytes, SHA256: chunk.SHA256,
		}
	}
	if _, err := taskManifestSHA256(manifest); err == nil {
		t.Fatal("task manifest accepted more than 5 GiB")
	}
}

func TestTaskWorkspaceManifestPageHonorsEncodedSizeBound(t *testing.T) {
	manifest := mordesync.Manifest{Files: make([]mordesync.File, TaskWorkspaceManifestPageFilesMax)}
	sharedPath := strings.TrimSuffix(strings.Repeat(strings.Repeat("a", 255)+"/", 14), "/")
	for index := range manifest.Files {
		path := fmt.Sprintf("%s/%s/%s", sharedPath, strings.Repeat("b", 250), fmt.Sprintf("f%06d%s", index, strings.Repeat("x", 249)))
		manifest.Files[index] = mordesync.File{Path: path, Mode: 0o600, SHA256: testSHA256("")}
	}
	manifest.FileCount = int64(len(manifest.Files))
	digest, err := taskManifestSHA256(manifest)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := taskWorkspaceManifestSnapshot{
		Metadata: persistedTaskView{
			HostID: "habc1234", TaskID: "qabc1234", WorkspaceID: "wabc1234", BindingVersion: 1,
			WorkspaceCheckpointRevision: 0, ManifestSHA256: strings.Repeat("0", 64), ViewID: "v" + strings.Repeat("a", 64),
		},
		ManifestSHA256: digest,
		Files:          taskManifestFiles(manifest),
	}
	page, err := pageTaskWorkspaceManifest(snapshot, "", TaskWorkspaceManifestPageFilesMax)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxTaskWorkspaceManifestPageBytes || len(page.Files) >= TaskWorkspaceManifestPageFilesMax || page.NextCursor == "" {
		t.Fatalf("page size/count/cursor = %d/%d/%q", len(encoded), len(page.Files), page.NextCursor)
	}
}

func TestTaskWorkspaceManifestSnapshotRejectsFileCountBound(t *testing.T) {
	snapshot := taskWorkspaceManifestSnapshot{
		Metadata: persistedTaskView{
			HostID: "habc1234", TaskID: "qabc1234", WorkspaceID: "wabc1234", BindingVersion: 1,
			WorkspaceCheckpointRevision: 0, ManifestSHA256: strings.Repeat("0", 64), ViewID: "v" + strings.Repeat("a", 64),
		},
		ManifestSHA256: strings.Repeat("0", 64),
		Files:          make([]taskManifestFile, maxTaskViewFiles+1),
	}
	if err := validateTaskWorkspaceManifestSnapshot(&snapshot); err == nil {
		t.Fatal("Task Workspace manifest accepted too many files")
	}
}

func TestOpenTaskWorkspaceAcceptsStickyGroupWritableHostRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(root, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, os.ModeSetgid|os.ModeSticky|0o770); err != nil {
		t.Fatal(err)
	}
	workspace, err := OpenTaskWorkspace(root, filepath.Join(root, ".morde-task-views"), uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatalf("open Task Workspace beneath sticky Host root: %v", err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenTaskWorkspaceRejectsNonStickyGroupWritableHostRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(root, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTaskWorkspace(root, filepath.Join(root, ".morde-task-views"), uint32(os.Getuid()), uint32(os.Getgid())); !errors.Is(err, ErrInvalidWorkspace) {
		t.Fatalf("open Task Workspace beneath non-sticky group-writable Host root = %v; want ErrInvalidWorkspace", err)
	}
}

func openTaskExportWorkspace(t *testing.T, files map[string]string) (*Workspace, string, string, string, string) {
	t.Helper()
	root := t.TempDir()
	workspaceID := "wabc1234"
	sourcePath := filepath.Join(root, workspaceID)
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(sourcePath, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	taskRootParent := t.TempDir()
	taskRootPath := filepath.Join(taskRootParent, "task-views")
	workspace, err := OpenTaskWorkspace(root, taskRootPath, uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatalf("open task workspace: %v", err)
	}
	t.Cleanup(func() { _ = workspace.Close() })
	inputManifest, inputDigest, err := scanTaskWorkspace(context.Background(), sourcePath)
	if err != nil {
		t.Fatalf("scan source workspace: %v", err)
	}
	_ = inputManifest
	taskID := "qabc1234"
	viewID := "v" + strings.Repeat("a", 64)
	if err := workspace.PrepareTaskWorkspaceView(context.Background(), "habc1234", taskID, workspaceID, viewID, 3, 4, 9, inputDigest); err != nil {
		t.Fatalf("prepare task workspace view: %v", err)
	}
	viewPath := filepath.Join(taskRootPath, taskID, viewID)
	return workspace, taskID, viewID, inputDigest, viewPath
}
