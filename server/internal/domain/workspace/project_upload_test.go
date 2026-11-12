package workspace

import (
	"strings"
	"testing"
)

const testProjectSourceBindingID = "12345678-1234-4234-8234-123456789abc"

func TestPrepareProjectUploadSnapshotPrefixesServerOwnedRoot(t *testing.T) {
	folder := SyncFolder{FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: SyncFolderKindProject,
		ProjectName: "blog", RelativePath: "project/blog", State: "pending_node"}
	mode := uint32(0o755)
	modTime := int64(123)
	request := CreateSyncSession{Direction: SyncUpload, Policy: SyncCompareMerge, Files: []File{
		file("src/base.txt", strings.Repeat("a", 64), 1),
		{Path: "src/link", Kind: FileKindRegular, HardlinkTo: "src/base.txt", SizeBytes: 1,
			SHA256: strings.Repeat("a", 64), Mode: 0o644, Chunks: []Chunk{{SHA256: strings.Repeat("a", 64), SizeBytes: 1}}},
	}, SourceBindingID: testProjectSourceBindingID, EmptyDirectories: []string{"empty"}, Directories: []Directory{
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
	valid := CreateSyncSession{Direction: SyncUpload, Policy: SyncCompareMerge, SourceBindingID: testProjectSourceBindingID}
	for name, request := range map[string]CreateSyncSession{
		"client scope":    {Direction: SyncUpload, Policy: SyncCompareMerge, SourceBindingID: testProjectSourceBindingID, Scope: []string{"project/other"}},
		"client delete":   {Direction: SyncUpload, Policy: SyncCompareMerge, SourceBindingID: testProjectSourceBindingID, DeletedPaths: []string{"secret.txt"}},
		"traversal":       {Direction: SyncUpload, Policy: SyncCompareMerge, SourceBindingID: testProjectSourceBindingID, Files: []File{file("../secret", strings.Repeat("a", 64), 1)}},
		"absolute":        {Direction: SyncUpload, Policy: SyncCompareMerge, SourceBindingID: testProjectSourceBindingID, Files: []File{file("/secret", strings.Repeat("a", 64), 1)}},
		"wrong direction": {Direction: SyncBidirectional, Policy: SyncCompareMerge, SourceBindingID: testProjectSourceBindingID},
		"client revision": {Direction: SyncUpload, Policy: SyncCompareMerge, SourceBindingID: testProjectSourceBindingID, BaseRevision: 7},
	} {
		if _, err := PrepareProjectUploadSnapshot(folder, request); err == nil {
			t.Errorf("%s request was accepted", name)
		}
	}
	for name, sourceID := range map[string]string{"missing": "", "uppercase": "12345678-1234-4234-8234-123456789ABC", "malformed": "not-a-uuid"} {
		if _, err := PrepareProjectUploadSnapshot(folder, CreateSyncSession{Direction: SyncUpload, Policy: SyncCompareMerge, SourceBindingID: sourceID}); err == nil {
			t.Errorf("%s project source binding was accepted", name)
		}
	}
	if err := ValidateCreateSyncSession(CreateSyncSession{Direction: SyncUpload, Policy: SyncCompareMerge, SourceBindingID: testProjectSourceBindingID}, 0); err == nil {
		t.Fatal("non-project sync accepted a project source binding")
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
			Direction: SyncUpload, Policy: SyncCompareMerge, SourceBindingID: testProjectSourceBindingID,
			Files: []File{symlinkFile("src/current", target)},
		})
		if err == nil {
			t.Errorf("%s symlink target %q escaped the project folder", name, target)
		}
	}
	prepared, err := PrepareProjectUploadSnapshot(folder, CreateSyncSession{
		Direction: SyncUpload, Policy: SyncCompareMerge, SourceBindingID: testProjectSourceBindingID,
		Files: []File{symlinkFile("src/current", "../assets/current.svg")},
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
		Direction: SyncUpload, Policy: SyncCompareMerge, SourceBindingID: testProjectSourceBindingID,
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

func TestProjectManifestSHA256IsCanonicalAndRootScoped(t *testing.T) {
	root := "project/blog"
	a := file(root+"/a.txt", strings.Repeat("a", 64), 1)
	b := file(root+"/b.txt", strings.Repeat("b", 64), 2)
	mode := uint32(0o755)
	modTime := int64(123)
	directories := []Directory{{Path: root, Mode: &mode, ModTimeNS: &modTime},
		{Path: root + "/src", Mode: &mode, ModTimeNS: &modTime}}
	manifestA, err := ProjectManifestSHA256(root, []File{a, b}, []string{root + "/empty"}, directories)
	if err != nil {
		t.Fatal(err)
	}
	manifestB, err := ProjectManifestSHA256(root, []File{b, a}, []string{root + "/empty"}, []Directory{directories[1], directories[0]})
	if err != nil || manifestB != manifestA {
		t.Fatalf("manifest digest changed with item order: %q, %v (want %q)", manifestB, err, manifestA)
	}
	if _, err := ProjectManifestSHA256(root, []File{file("project/other/secret.txt", strings.Repeat("a", 64), 1)}, nil, nil); err == nil {
		t.Fatal("manifest digest accepted a path outside the registered root")
	}
}

func TestHostProjectUploadReceiptRequiresCompleteAppliedEvidence(t *testing.T) {
	target, syncState := int64(9), BindingSyncHealthy
	valid := HostProjectUploadApplicationReport{SourceBindingID: testProjectSourceBindingID, TargetRevision: 9,
		ManifestSHA256: strings.Repeat("a", 64), Status: "applied", AppliedRevision: &target, SyncState: &syncState,
		Impacts: []SyncHostApplicationImpact{}}
	if err := ValidateHostProjectUploadApplicationReport(valid); err != nil {
		t.Fatalf("valid applied receipt rejected: %v", err)
	}
	for name, report := range map[string]HostProjectUploadApplicationReport{
		"below target": {SourceBindingID: testProjectSourceBindingID, TargetRevision: 9, ManifestSHA256: strings.Repeat("a", 64),
			Status: "applied", AppliedRevision: int64Pointer(8), SyncState: &syncState, Impacts: []SyncHostApplicationImpact{}},
		"unhealthy": {SourceBindingID: testProjectSourceBindingID, TargetRevision: 9, ManifestSHA256: strings.Repeat("a", 64),
			Status: "applied", AppliedRevision: &target, SyncState: func() *BindingSyncState { state := BindingSyncConflict; return &state }(), Impacts: []SyncHostApplicationImpact{}},
		"impact on applied": {SourceBindingID: testProjectSourceBindingID, TargetRevision: 9, ManifestSHA256: strings.Repeat("a", 64),
			Status: "applied", AppliedRevision: &target, SyncState: &syncState, Impacts: []SyncHostApplicationImpact{{Path: "src/a", State: "unknown"}}},
		"omitted impacts": {SourceBindingID: testProjectSourceBindingID, TargetRevision: 9, ManifestSHA256: strings.Repeat("a", 64),
			Status: "unknown"},
		"absolute impact": {SourceBindingID: testProjectSourceBindingID, TargetRevision: 9, ManifestSHA256: strings.Repeat("a", 64),
			Status: "conflict", Impacts: []SyncHostApplicationImpact{{Path: "/tmp/private", State: "conflict"}}},
	} {
		if err := ValidateHostProjectUploadApplicationReport(report); err == nil {
			t.Errorf("%s receipt was accepted", name)
		}
	}
}

func TestProjectUploadReproducesWorkspaceBaselineCloudOnlyDeleteRisk(t *testing.T) {
	root := "project/blog"
	folder := SyncFolder{FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: SyncFolderKindProject,
		ProjectName: "blog", RelativePath: root, State: "active"}
	oldA := file(root+"/a.txt", strings.Repeat("a", 64), 1)
	newA := file(root+"/a.txt", strings.Repeat("b", 64), 1)
	cloudOnly := file(root+"/cloud-only.txt", strings.Repeat("c", 64), 1)

	local, err := PrepareProjectUploadSnapshot(folder, CreateSyncSession{
		Direction: SyncUpload, Policy: SyncCompareMerge, SourceBindingID: testProjectSourceBindingID,
		Files: []File{file("a.txt", strings.Repeat("b", 64), 1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstPlan := PlanSync([]File{oldA}, []File{oldA, cloudOnly}, local)
	if len(firstPlan.Conflicts) != 0 || len(firstPlan.UploadFiles) != 1 {
		t.Fatalf("synthetic cloud change plan = %+v; want local a.txt update without conflict", firstPlan)
	}

	// The current implementation advances last_upload_revision to the merged
	// Workspace revision, which now contains cloudOnly even though PC never sent it.
	deleted, _, err := ProjectSnapshotDeletions(root, []File{newA, cloudOnly}, nil, nil, local)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != cloudOnly.Path {
		t.Fatalf("synthetic merged Workspace baseline deletes = %#v; want to reproduce cloud-only candidate %q", deleted, cloudOnly.Path)
	}
}
