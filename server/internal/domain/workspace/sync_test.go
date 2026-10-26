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
