package workspace

import (
	"strings"
	"testing"
)

func TestPrepareProjectUploadSnapshotPrefixesServerOwnedRoot(t *testing.T) {
	folder := SyncFolder{FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: SyncFolderKindProject,
		ProjectName: "blog", RelativePath: "project/blog", State: "pending_node"}
	mode := uint32(0o755)
	modTime := int64(123)
	request := CreateSyncSession{Direction: SyncUpload, Policy: SyncCompareMerge, Files: []File{
		file("src/base.txt", strings.Repeat("a", 64), 1),
		{Path: "src/link", Kind: FileKindRegular, HardlinkTo: "src/base.txt", SizeBytes: 1,
			SHA256: strings.Repeat("a", 64), Mode: 0o644, Chunks: []Chunk{{SHA256: strings.Repeat("a", 64), SizeBytes: 1}}},
	}, EmptyDirectories: []string{"empty"}, Directories: []Directory{
		{Path: ".", Mode: &mode, ModTimeNS: &modTime},
		{Path: "src", Mode: &mode, ModTimeNS: &modTime},
		{Path: "empty", Mode: &mode, ModTimeNS: &modTime},
	}}
	prepared, err := PrepareProjectUploadSnapshot(folder, request)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.FolderID != folder.FolderID || len(prepared.Scope) != 1 || prepared.Scope[0] != folder.RelativePath {
		t.Fatalf("server folder scope = %#v, folder=%q", prepared.Scope, prepared.FolderID)
	}
	if prepared.Files[0].Path != "project/blog/src/base.txt" || prepared.Files[1].HardlinkTo != "project/blog/src/base.txt" ||
		prepared.EmptyDirectories[0] != "project/blog/empty" || prepared.Directories[0].Path != folder.RelativePath {
		t.Fatalf("project snapshot was not rooted beneath the registered path: %#v", prepared)
	}
	if request.Files[0].Path != "src/base.txt" || request.Directories[0].Path != "." {
		t.Fatal("preparing a project snapshot mutated the caller's manifest")
	}
}

func TestPrepareProjectUploadSnapshotRejectsClientScopeAndTraversal(t *testing.T) {
	folder := SyncFolder{FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: SyncFolderKindProject,
		ProjectName: "blog", RelativePath: "project/blog", State: "active"}
	valid := CreateSyncSession{Direction: SyncUpload, Policy: SyncCompareMerge}
	for name, request := range map[string]CreateSyncSession{
		"client scope":    {Direction: SyncUpload, Policy: SyncCompareMerge, Scope: []string{"project/other"}},
		"client delete":   {Direction: SyncUpload, Policy: SyncCompareMerge, DeletedPaths: []string{"secret.txt"}},
		"traversal":       {Direction: SyncUpload, Policy: SyncCompareMerge, Files: []File{file("../secret", strings.Repeat("a", 64), 1)}},
		"absolute":        {Direction: SyncUpload, Policy: SyncCompareMerge, Files: []File{file("/secret", strings.Repeat("a", 64), 1)}},
		"wrong direction": {Direction: SyncBidirectional, Policy: SyncCompareMerge},
		"client revision": {Direction: SyncUpload, Policy: SyncCompareMerge, BaseRevision: 7},
	} {
		if _, err := PrepareProjectUploadSnapshot(folder, request); err == nil {
			t.Errorf("%s request was accepted", name)
		}
	}
	if _, err := PrepareProjectUploadSnapshot(SyncFolder{FolderID: folder.FolderID, Kind: SyncFolderKindProject,
		ProjectName: "blog", RelativePath: "project/elsewhere", State: "active"}, valid); err == nil {
		t.Fatal("folder path inconsistent with registered project name was accepted")
	}
}

func TestPrepareProjectUploadSnapshotKeepsSymlinksInsideProject(t *testing.T) {
	folder := SyncFolder{FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: SyncFolderKindProject,
		ProjectName: "blog", RelativePath: "project/blog", State: "active"}
	for name, target := range map[string]string{
		"workspace sibling": "../../other/README.md",
		"workspace root":    "../../../README.md",
	} {
		_, err := PrepareProjectUploadSnapshot(folder, CreateSyncSession{
			Direction: SyncUpload, Policy: SyncCompareMerge, Files: []File{symlinkFile("src/current", target)},
		})
		if err == nil {
			t.Errorf("%s symlink target %q escaped the project folder", name, target)
		}
	}
	prepared, err := PrepareProjectUploadSnapshot(folder, CreateSyncSession{
		Direction: SyncUpload, Policy: SyncCompareMerge, Files: []File{symlinkFile("src/current", "../assets/current.svg")},
	})
	if err != nil || prepared.Files[0].Path != "project/blog/src/current" {
		t.Fatalf("in-project symlink snapshot = %#v, err=%v", prepared, err)
	}
}

func TestProjectSnapshotDeletionsUseOnlySavedBaseline(t *testing.T) {
	folderRoot := "project/blog"
	folderID := "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	request, err := PrepareProjectUploadSnapshot(SyncFolder{FolderID: folderID, Kind: SyncFolderKindProject,
		ProjectName: "blog", RelativePath: folderRoot, State: "active"}, CreateSyncSession{
		Direction: SyncUpload, Policy: SyncCompareMerge,
		Files:            []File{file("keep.txt", strings.Repeat("b", 64), 1)},
		EmptyDirectories: []string{"empty-kept"},
	})
	if err != nil {
		t.Fatal(err)
	}
	deletedFiles, deletedDirectories, err := ProjectSnapshotDeletions(folderRoot,
		[]File{file("project/blog/keep.txt", strings.Repeat("a", 64), 1), file("project/blog/removed.txt", strings.Repeat("c", 64), 1)},
		[]string{"project/blog/empty-removed"},
		[]Directory{{Path: folderRoot}, {Path: "project/blog/old-dir"}}, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(deletedFiles) != 1 || deletedFiles[0] != "project/blog/removed.txt" {
		t.Fatalf("deleted files = %#v", deletedFiles)
	}
	if len(deletedDirectories) != 2 || deletedDirectories[0] != "project/blog/empty-removed" || deletedDirectories[1] != "project/blog/old-dir" {
		t.Fatalf("deleted directories = %#v", deletedDirectories)
	}
	if _, _, err := ProjectSnapshotDeletions(folderRoot, []File{file("project/other/secret", strings.Repeat("a", 64), 1)}, nil, nil, request); err == nil {
		t.Fatal("baseline entry outside project root was accepted")
	}
}
