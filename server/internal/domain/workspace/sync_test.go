package workspace

import (
	"strings"
	"testing"
)

func file(path, digest string, size int64) File {
	chunks := []Chunk{}
	if size > 0 {
		chunks = append(chunks, Chunk{SHA256: digest, SizeBytes: size})
	}
	return File{Path: path, SizeBytes: size, SHA256: digest, Mode: 0644, Chunks: chunks}
}

func TestValidSyncFolderID(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
	}{
		{value: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", want: true},
		{value: "morde-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{value: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{value: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/"},
	} {
		if got := ValidSyncFolderID(test.value); got != test.want {
			t.Errorf("ValidSyncFolderID(%q) = %t; want %t", test.value, got, test.want)
		}
	}
}

func TestValidateCreateSyncSessionEnforcesManifestLimitsAndPaths(t *testing.T) {
	valid := CreateSyncSession{Direction: SyncUpload, Policy: SyncCompareMerge, Files: []File{file("src/main.go", strings.Repeat("a", 64), 3)}}
	if err := ValidateCreateSyncSession(valid, 1024); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/etc/passwd", "../secret", "src/../secret", "src\\secret", "", "."} {
		invalid := valid
		invalid.Files = []File{file(path, strings.Repeat("a", 64), 3)}
		if err := ValidateCreateSyncSession(invalid, 1024); err == nil {
			t.Errorf("accepted unsafe path %q", path)
		}
	}
	invalid := valid
	invalid.Files = []File{file("A.txt", strings.Repeat("a", 64), 3), file("a.TXT", strings.Repeat("b", 64), 3)}
	if err := ValidateCreateSyncSession(invalid, 1024); err == nil {
		t.Fatal("accepted case-folded path collision")
	}
	invalid.Files = []File{file("Workspace/a.txt", strings.Repeat("a", 64), 3), file("workspace/b.txt", strings.Repeat("b", 64), 3)}
	if err := ValidateCreateSyncSession(invalid, 1024); err == nil {
		t.Fatal("accepted case-folded directory collision")
	}
	invalid = valid
	invalid.Files = []File{file("src/main.go", strings.Repeat("a", 64), 5)}
	if err := ValidateCreateSyncSession(invalid, 4); err == nil {
		t.Fatal("accepted manifest over quota")
	}
}

func TestValidateSyncManifestIncludesEmptyDirectoriesWithoutChargingFileQuota(t *testing.T) {
	value := CreateSyncSession{
		Direction: SyncUpload, Policy: SyncLive, Files: []File{file("notes.txt", strings.Repeat("a", 64), 3)},
		EmptyDirectories: []string{"empty/nested"},
	}
	if err := ValidateCreateSyncSession(value, 3); err != nil {
		t.Fatalf("valid file plus empty directory rejected: %v", err)
	}
	for _, invalid := range []CreateSyncSession{
		{Direction: SyncUpload, Policy: SyncLive, Files: []File{file("a/b.txt", strings.Repeat("a", 64), 1)}, EmptyDirectories: []string{"a"}},
		{Direction: SyncUpload, Policy: SyncLive, EmptyDirectories: []string{"A", "a"}},
		{Direction: SyncUpload, Policy: SyncLive, EmptyDirectories: []string{"empty", "empty/child"}},
		{Direction: SyncUpload, Policy: SyncLive, Files: []File{file("same", strings.Repeat("a", 64), 1)}, EmptyDirectories: []string{"same"}},
	} {
		if err := ValidateCreateSyncSession(invalid, 0); err == nil {
			t.Errorf("accepted conflicting tree manifest: %#v", invalid)
		}
	}
	invalid := CreateSyncSession{Direction: SyncUpload, Policy: SyncLive, EmptyDirectories: []string{"empty"}, DeletedDirectories: []string{"empty"}}
	if err := ValidateCreateSyncSession(invalid, 0); err == nil {
		t.Fatal("accepted a directory as both present and deleted")
	}
}

func TestPlanSyncTreeCoalescesFileDirectoryReplacement(t *testing.T) {
	digest := strings.Repeat("a", 64)
	oldFile := file("folder", digest, 1)
	request := CreateSyncSession{
		Direction: SyncBidirectional, Policy: SyncLive, Files: []File{},
		EmptyDirectories: []string{"folder"}, DeletedPaths: []string{"folder"},
	}
	plan := PlanSyncTree([]File{oldFile}, []File{oldFile}, nil, nil, request)
	if len(plan.UploadFiles) != 0 || len(plan.UploadDeletedPaths) != 0 || len(plan.UploadEmptyDirectories) != 1 || plan.UploadEmptyDirectories[0] != "folder" || len(plan.Conflicts) != 0 {
		t.Fatalf("file-to-directory upload plan = %#v", plan)
	}
	actions := DownloadActionsForTreePlan(nil, nil, []string{"folder"}, nil)
	if len(actions) != 1 || actions[0].Path != "folder" || actions[0].Action != ChangeDirectoryUpsert {
		t.Fatalf("directory download action = %#v", actions)
	}
	if err := ValidateDownloadAction(actions[0]); err != nil {
		t.Fatalf("directory download action rejected: %v", err)
	}

	request = CreateSyncSession{
		Direction: SyncBidirectional, Policy: SyncLive,
		Files: []File{file("folder", strings.Repeat("b", 64), 1)}, DeletedDirectories: []string{"folder"},
	}
	plan = PlanSyncTree(nil, nil, []string{"folder"}, []string{"folder"}, request)
	if len(plan.UploadFiles) != 1 || plan.UploadFiles[0].Path != "folder" || len(plan.UploadDeletedDirectories) != 0 || len(plan.Conflicts) != 0 {
		t.Fatalf("directory-to-file upload plan = %#v", plan)
	}

	request = CreateSyncSession{Direction: SyncDownload, Policy: SyncCompareMerge, Files: []File{oldFile}}
	plan = PlanSyncTree([]File{oldFile}, nil, nil, []string{"folder"}, request)
	if len(plan.DownloadFiles) != 0 || len(plan.DownloadDeletedPaths) != 0 || len(plan.DownloadEmptyDirectories) != 1 || len(plan.DownloadDeletedDirectories) != 0 {
		t.Fatalf("file-to-directory download plan was not coalesced: %#v", plan)
	}

	request = CreateSyncSession{Direction: SyncDownload, Policy: SyncCompareMerge, Files: []File{}}
	// A remote file is represented by a file snapshot in the current tree.
	remoteFile := file("folder", strings.Repeat("c", 64), 1)
	plan = PlanSyncTree(nil, []File{remoteFile}, []string{"folder"}, nil, request)
	if len(plan.DownloadFiles) != 1 || plan.DownloadFiles[0].Path != "folder" || len(plan.DownloadDeletedDirectories) != 0 {
		t.Fatalf("directory-to-file download plan was not coalesced: %#v", plan)
	}
}

func TestSyncQuotaStatusForUsage(t *testing.T) {
	for _, test := range []struct {
		used, quota int64
		want        SyncQuotaStatus
	}{
		{used: 9, quota: 10, want: SyncQuotaAvailable},
		{used: 10, quota: 10, want: SyncQuotaFull},
		{used: 11, quota: 10, want: SyncQuotaOver},
	} {
		if got := SyncQuotaStatusForUsage(test.used, test.quota); got != test.want {
			t.Errorf("SyncQuotaStatusForUsage(%d, %d) = %q, want %q", test.used, test.quota, got, test.want)
		}
	}
}

func TestValidateSyncScopeIsNonOverlappingCaseInsensitively(t *testing.T) {
	base := CreateSyncSession{Direction: SyncUpload, Policy: SyncLive, Files: []File{file("src/main.go", strings.Repeat("a", 64), 3)}}
	base.Scope = []string{"src", "SRC/vendor"}
	if err := ValidateCreateSyncSession(base, 1024); err == nil {
		t.Fatal("accepted overlapping case-insensitive scopes")
	}
	base.Scope = []string{"docs"}
	if err := ValidateCreateSyncSession(base, 1024); err == nil {
		t.Fatal("accepted manifest file outside requested scope")
	}
	base.Files = []File{}
	base.DeletedPaths = []string{"src/file.go"}
	if err := ValidateCreateSyncSession(base, 1024); err == nil {
		t.Fatal("accepted deleted path outside requested scope")
	}
}

func TestSyncPlanDoesNotInferDeletesAndMergeDetectsChangedPaths(t *testing.T) {
	base := []File{file("keep.txt", strings.Repeat("a", 64), 1), file("change.txt", strings.Repeat("b", 64), 1)}
	remote := []File{file("keep.txt", strings.Repeat("a", 64), 1), file("change.txt", strings.Repeat("c", 64), 1)}
	request := CreateSyncSession{Direction: SyncBidirectional, Policy: SyncCompareMerge, Files: []File{file("change.txt", strings.Repeat("d", 64), 1)}}
	plan := PlanSync(base, remote, request)
	if len(plan.UploadFiles) != 0 || len(plan.Conflicts) != 1 || plan.Conflicts[0].Path != "change.txt" {
		t.Fatalf("changed path plan = %#v", plan)
	}
	if len(plan.UploadDeletedPaths) != 0 {
		t.Fatalf("omitted keep.txt implied deletion: %#v", plan.UploadDeletedPaths)
	}
	request.DeletedPaths = []string{"keep.txt"}
	plan = PlanSync(base, remote, request)
	if len(plan.UploadDeletedPaths) != 1 || plan.UploadDeletedPaths[0] != "keep.txt" {
		t.Fatalf("explicit deletion plan = %#v", plan.UploadDeletedPaths)
	}
}

func TestSyncFillMissingOnlyAddsAbsentFiles(t *testing.T) {
	base := []File{file("same.txt", strings.Repeat("a", 64), 1)}
	remote := []File{file("same.txt", strings.Repeat("b", 64), 1)}
	request := CreateSyncSession{Direction: SyncUpload, Policy: SyncFillMissing, Files: []File{file("same.txt", strings.Repeat("c", 64), 1), file("new.txt", strings.Repeat("d", 64), 1)}}
	plan := PlanSync(base, remote, request)
	if len(plan.UploadFiles) != 1 || plan.UploadFiles[0].Path != "new.txt" || len(plan.Conflicts) != 0 {
		t.Fatalf("fill missing plan = %#v", plan)
	}
}

func TestSyncLiveUploadsOneSidedLocalChangeWithoutDownloadingOldRemoteFile(t *testing.T) {
	base := []File{file("main.go", strings.Repeat("a", 64), 1)}
	request := CreateSyncSession{
		Direction: SyncBidirectional, Policy: SyncLive,
		Files: []File{file("main.go", strings.Repeat("b", 64), 1)},
	}
	plan := PlanSync(base, base, request)
	if len(plan.UploadFiles) != 1 || plan.UploadFiles[0].SHA256 != request.Files[0].SHA256 || len(plan.DownloadFiles) != 0 || len(plan.Conflicts) != 0 {
		t.Fatalf("one-sided local live edit plan = %#v", plan)
	}
}

func TestSyncLiveCaseOnlyRenameUploadsLocallyAndConflictsRemotely(t *testing.T) {
	base := []File{file("foo.txt", strings.Repeat("a", 64), 1)}
	localRename := file("Foo.txt", strings.Repeat("a", 64), 1)
	request := CreateSyncSession{Direction: SyncBidirectional, Policy: SyncLive, Files: []File{localRename}}
	plan := PlanSync(base, base, request)
	if len(plan.UploadFiles) != 1 || plan.UploadFiles[0].Path != "Foo.txt" || len(plan.DownloadFiles) != 0 || len(plan.Conflicts) != 0 {
		t.Fatalf("local case-only rename plan = %#v", plan)
	}

	remote := []File{file("Foo.txt", strings.Repeat("a", 64), 1)}
	request.Files = append([]File(nil), base...)
	plan = PlanSync(base, remote, request)
	if len(plan.UploadFiles) != 0 || len(plan.DownloadFiles) != 0 || len(plan.Conflicts) != 1 ||
		plan.Conflicts[0].Path != "Foo.txt" || plan.Conflicts[0].Reason != "case_collision" {
		t.Fatalf("remote case-only rename plan = %#v", plan)
	}

	request.Policy = SyncFillMissing
	plan = PlanSync(nil, remote, request)
	if len(plan.UploadFiles) != 0 || len(plan.DownloadFiles) != 0 || len(plan.Conflicts) != 0 {
		t.Fatalf("initial fill-missing case mismatch should preserve both spellings: %#v", plan)
	}
}

func TestManifestAndChangeCursorsAreBoundedAndValidated(t *testing.T) {
	manifest := ManifestCursor{Revision: 12, Prefix: "src", After: "src/file.go"}
	encoded := EncodeManifestCursor(manifest)
	decoded, err := DecodeManifestCursor(encoded)
	if err != nil || decoded != manifest {
		t.Fatalf("manifest cursor round trip = %#v, %v", decoded, err)
	}
	if _, err := DecodeManifestCursor("../bad"); err == nil {
		t.Fatal("accepted invalid manifest cursor")
	}
	change := ChangeCursor{Revision: 12, Path: "src/file.go"}
	encoded = EncodeChangeCursor(change)
	decodedChange, err := DecodeChangeCursor(encoded)
	if err != nil || decodedChange != change {
		t.Fatalf("change cursor round trip = %#v, %v", decodedChange, err)
	}
	if _, err := DecodeChangeCursor(EncodeChangeCursor(ChangeCursor{Revision: -1})); err == nil {
		t.Fatal("accepted negative change cursor")
	}
}
