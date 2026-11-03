package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	mordesync "morde-sync"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

const legacyImportWorkspaceID = "w1234567"

var legacyImportTime = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

type legacyImportStoreFake struct {
	volume           LegacyWorkspaceVolume
	pending          bool
	lockBusy         bool
	importReadyErr   error
	importReadyCalls int
	lockReleaseCalls int
	commitCall       int
	cleanupReason    string
	reservations     []WorkspaceBlockReservation
	finalized        []WorkspaceBlockAllocation
	files            []workspace.File
	emptyDirectories []string
	directories      []workspace.Directory
}

type legacyImportLockFake struct {
	store *legacyImportStoreFake
}

func (lock *legacyImportLockFake) Release(context.Context) error {
	lock.store.lockReleaseCalls++
	return nil
}

func (s *legacyImportStoreFake) TryWorkspaceBlockStoreLock(ctx context.Context, workspaceID string) (LeaderLock, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if s.lockBusy {
		return nil, false, nil
	}
	return &legacyImportLockFake{store: s}, true, nil
}

func (s *legacyImportStoreFake) GetPendingWorkspaceLegacyVolume(ctx context.Context, id string) (LegacyWorkspaceVolume, error) {
	if err := ctx.Err(); err != nil {
		return LegacyWorkspaceVolume{}, err
	}
	if !s.pending || id != s.volume.WorkspaceID {
		return LegacyWorkspaceVolume{}, errors.New("legacy volume is not pending")
	}
	return s.volume, nil
}

func (s *legacyImportStoreFake) EnsureWorkspaceLegacyImportReady(ctx context.Context, id string) error {
	s.importReadyCalls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if id != s.volume.WorkspaceID || !s.pending {
		return errors.New("legacy volume is not pending")
	}
	return s.importReadyErr
}

func (s *legacyImportStoreFake) MarkWorkspaceLegacyVolumeNeedsCleanup(ctx context.Context, id, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.pending || id != s.volume.WorkspaceID {
		return errors.New("legacy workspace is not pending")
	}
	s.cleanupReason = reason
	return nil
}

func (s *legacyImportStoreFake) ReserveWorkspaceBlockUsage(ctx context.Context, workspaceID string, reservations []WorkspaceBlockReservation, _ int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.pending || workspaceID != s.volume.WorkspaceID {
		return errors.New("legacy workspace is not pending")
	}
	s.reservations = append(s.reservations, reservations...)
	return nil
}

func (s *legacyImportStoreFake) FinalizeWorkspaceBlockUsage(ctx context.Context, workspaceID, digest string, allocatedBytes, _ int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.finalized = append(s.finalized, WorkspaceBlockAllocation{WorkspaceID: workspaceID, Digest: digest, AllocatedBytes: allocatedBytes})
	return nil
}

func (s *legacyImportStoreFake) CommitWorkspaceLegacyVolumeImport(ctx context.Context, value LegacyWorkspaceVolume, files []workspace.File, emptyDirectories []string, directories []workspace.Directory) (workspace.Workspace, error) {
	if err := ctx.Err(); err != nil {
		return workspace.Workspace{}, err
	}
	if !s.pending || value != s.volume {
		return workspace.Workspace{}, errors.New("legacy volume changed before commit")
	}
	s.pending = false
	s.commitCall++
	s.files = append([]workspace.File{}, files...)
	s.emptyDirectories = append([]string{}, emptyDirectories...)
	s.directories = append([]workspace.Directory{}, directories...)
	revision := int64(1)
	if len(files)+len(emptyDirectories)+len(directories) == 0 {
		revision = 0
	}
	return workspace.Workspace{ID: value.WorkspaceID, Revision: revision, UsedBytes: legacyFilesSize(files)}, nil
}

func legacyFilesSize(files []workspace.File) int64 {
	var total int64
	for _, file := range files {
		total += file.SizeBytes
	}
	return total
}

type legacyImportBlocksFake struct {
	data            map[string][]byte
	putCalls        int
	putErr          error
	removeTempCalls int
	removeTempErr   error
	onPut           func()
	listErr         error
	wrongSize       bool
}

func newLegacyImportBlocksFake() *legacyImportBlocksFake {
	return &legacyImportBlocksFake{data: make(map[string][]byte)}
}

func (s *legacyImportBlocksFake) PutBlock(_ string, digest string, source io.Reader) (int64, error) {
	s.putCalls++
	if s.onPut != nil {
		s.onPut()
	}
	if s.putErr != nil {
		return 0, s.putErr
	}
	body, err := io.ReadAll(io.LimitReader(source, workspace.MaxSyncBlockBytes+1))
	if err != nil {
		return 0, err
	}
	if int64(len(body)) > workspace.MaxSyncBlockBytes {
		return 0, errors.New("block exceeds limit")
	}
	actual := sha256.Sum256(body)
	if hex.EncodeToString(actual[:]) != digest {
		return 0, errors.New("block digest mismatch")
	}
	s.data[digest] = append([]byte(nil), body...)
	if s.wrongSize {
		return int64(len(body)) + 1, nil
	}
	return int64(len(body)), nil
}

func (s *legacyImportBlocksFake) ReservationBytes(size int64) (int64, error) { return size, nil }

func (s *legacyImportBlocksFake) BlockAllocatedBytes(_ string, digest string) (int64, error) {
	body, ok := s.data[digest]
	if !ok {
		return 0, os.ErrNotExist
	}
	return int64(len(body)), nil
}

func (s *legacyImportBlocksFake) ListBlockDigests(_ string) ([]string, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	digests := make([]string, 0, len(s.data))
	for digest := range s.data {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	return digests, nil
}

func (s *legacyImportBlocksFake) ListBlockNames(_ string) ([]string, error) {
	return s.ListBlockDigests("")
}

func (s *legacyImportBlocksFake) DeleteBlock(_ string, digest string) (int64, error) {
	body, ok := s.data[digest]
	if !ok {
		return 0, nil
	}
	delete(s.data, digest)
	return int64(len(body)), nil
}

func (s *legacyImportBlocksFake) RemoveStaleTempBlocks(string) error {
	s.removeTempCalls++
	return s.removeTempErr
}

type legacyImportSourceFake struct {
	fs             fs.FS
	openErr        error
	validateErr    error
	closer         *legacyImportCloser
	openName       string
	validateNames  []string
	validateRoots  []string
	hardlinkScans  []map[string]string
	mutateManifest func(int, *mordesync.Manifest)
	validateCall   int
}

func (s *legacyImportSourceFake) OpenLegacyVolume(_ context.Context, name string) (fs.FS, io.Closer, error) {
	s.openName = name
	if s.openErr != nil {
		return nil, nil, s.openErr
	}
	return s.fs, s.closer, nil
}

func (s *legacyImportSourceFake) ValidateLegacyWorkspaceTree(ctx context.Context, name, root string) (mordesync.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return mordesync.Manifest{}, err
	}
	s.validateNames = append(s.validateNames, name)
	s.validateRoots = append(s.validateRoots, root)
	if s.validateErr != nil {
		return mordesync.Manifest{}, s.validateErr
	}
	var value map[string]string
	if len(s.hardlinkScans) > 0 {
		index := min(s.validateCall, len(s.hardlinkScans)-1)
		value = s.hardlinkScans[index]
	}
	call := s.validateCall
	s.validateCall++
	copy := make(map[string]string, len(value))
	for path, anchor := range value {
		copy[path] = anchor
	}
	tree := s.fs
	if root != "" {
		var err error
		tree, err = fs.Sub(s.fs, root)
		if err != nil {
			return mordesync.Manifest{}, err
		}
	}
	manifest, err := legacyImportManifestForFS(ctx, tree, copy)
	if err == nil && s.mutateManifest != nil {
		s.mutateManifest(call, &manifest)
	}
	return manifest, err
}

func legacyImportManifestForFS(ctx context.Context, volumeFS fs.FS, hardlinks map[string]string) (mordesync.Manifest, error) {
	sources, emptyDirectories, directories, err := scanLegacyWorkspace(ctx, volumeFS, 1<<60)
	if err != nil {
		return mordesync.Manifest{}, err
	}
	manifest := mordesync.Manifest{EmptyDirectories: emptyDirectories, FileCount: int64(len(sources))}
	for _, source := range sources {
		modTimeNS := source.info.ModTime().UnixNano()
		source.metadata = workspace.File{
			Path: source.path, SizeBytes: source.info.Size(), Mode: uint32(source.info.Mode().Perm() & 0777),
			ModTimeNS: &modTimeNS, HardlinkTo: hardlinks[source.path], MetadataKnown: true,
		}
		file, err := readLegacyFile(ctx, volumeFS, source, nil)
		if err != nil {
			return mordesync.Manifest{}, err
		}
		manifest.TotalBytes += file.SizeBytes
		observed := mordesync.File{
			Path: file.Path, Kind: file.Kind, SymlinkTarget: file.SymlinkTarget, HardlinkTo: file.HardlinkTo,
			MetadataKnown: file.MetadataKnown, Mode: file.Mode, ModTimeNS: *file.ModTimeNS, ModTimeKnown: true,
			Size: file.SizeBytes, SHA256: file.SHA256,
		}
		var offset int64
		for _, chunk := range file.Chunks {
			observed.Blocks = append(observed.Blocks, mordesync.Block{Offset: offset, Size: chunk.SizeBytes, SHA256: chunk.SHA256})
			offset += chunk.SizeBytes
		}
		manifest.Files = append(manifest.Files, observed)
	}
	rootInfo, err := fs.Stat(volumeFS, ".")
	if err != nil {
		return mordesync.Manifest{}, err
	}
	allDirectories := append([]workspace.Directory(nil), directories...)
	mode, modified := uint32(rootInfo.Mode().Perm()&0777), rootInfo.ModTime().UnixNano()
	allDirectories = append(allDirectories, workspace.Directory{Path: ".", Mode: &mode, ModTimeNS: &modified, MetadataKnown: true})
	for _, directory := range allDirectories {
		observed := mordesync.Directory{Path: directory.Path, ModeKnown: true, ModTimeKnown: true, MetadataKnown: true}
		if directory.Mode != nil {
			observed.Mode = *directory.Mode
		}
		if directory.ModTimeNS != nil {
			observed.ModTimeNS = *directory.ModTimeNS
		}
		manifest.Directories = append(manifest.Directories, observed)
	}
	return manifest, nil
}

type legacyImportCloser struct {
	closeErr error
	closed   bool
}

func (c *legacyImportCloser) Close() error {
	c.closed = true
	return c.closeErr
}

func newLegacyImportHarness(volumeFS fs.FS, quota int64) (*WorkspaceLegacyImportService, *legacyImportStoreFake, *legacyImportBlocksFake, *legacyImportSourceFake) {
	store := &legacyImportStoreFake{
		volume:  LegacyWorkspaceVolume{WorkspaceID: legacyImportWorkspaceID, VolumeName: "legacy-volume", QuotaBytes: quota},
		pending: true,
	}
	blocks := newLegacyImportBlocksFake()
	source := &legacyImportSourceFake{fs: volumeFS, closer: &legacyImportCloser{}}
	service, err := NewWorkspaceLegacyImportService(store, source, blocks, 1<<40)
	if err != nil {
		panic(err)
	}
	return service, store, blocks, source
}

func legacyMapFile(data []byte, mode fs.FileMode) *fstest.MapFile {
	return &fstest.MapFile{Data: append([]byte(nil), data...), Mode: mode, ModTime: legacyImportTime}
}

func TestWorkspaceLegacyImportPreservesFilesAndPermissions(t *testing.T) {
	volumeFS := fstest.MapFS{
		"README.md": legacyMapFile([]byte("welcome"), 0644),
		"bin/tool":  legacyMapFile([]byte("#!/bin/sh\n"), 0755),
		"bin":       {Mode: fs.ModeDir | 0750, ModTime: legacyImportTime},
		"src":       {Mode: fs.ModeDir | 0755, ModTime: legacyImportTime},
		"src/empty": legacyMapFile(nil, 0600),
	}
	service, store, blocks, source := newLegacyImportHarness(volumeFS, 1<<20)

	result, err := service.Import(context.Background(), legacyImportWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != legacyImportWorkspaceID || result.Revision != 1 || result.UsedBytes != int64(len("welcome")+len("#!/bin/sh\n")) {
		t.Fatalf("import result = %+v", result)
	}
	if source.openName != "legacy-volume" || len(source.validateRoots) != 2 || source.validateNames[0] != "legacy-volume" || source.validateNames[1] != "legacy-volume" || !source.closer.closed || store.commitCall != 1 || store.lockReleaseCalls != 1 || blocks.removeTempCalls != 1 {
		t.Fatalf("open=%q validation=%q roots=%q closed=%v commits=%d lock releases=%d temp recoveries=%d", source.openName, source.validateNames, source.validateRoots, source.closer.closed, store.commitCall, store.lockReleaseCalls, blocks.removeTempCalls)
	}
	if len(store.files) != 3 || store.files[0].Path != "README.md" || store.files[1].Path != "bin/tool" || store.files[2].Path != "src/empty" {
		t.Fatalf("manifest files = %+v", store.files)
	}
	if store.files[1].Mode != 0755 || store.files[2].Mode != 0600 || store.files[2].SizeBytes != 0 || len(store.files[2].Chunks) != 0 || !store.files[0].MetadataKnown {
		t.Fatalf("file permissions or empty file metadata = %+v", store.files)
	}
	wantBinMode, wantMode, wantModTime := uint32(0750), uint32(0755), legacyImportTime.UnixNano()
	if len(store.directories) != 3 || store.directories[0].Path != "." || !store.directories[0].MetadataKnown ||
		!workspace.SameDirectoryMetadata(store.directories[1], workspace.Directory{Path: "bin", Mode: &wantBinMode, ModTimeNS: &wantModTime, MetadataKnown: true}) ||
		!workspace.SameDirectoryMetadata(store.directories[2], workspace.Directory{Path: "src", Mode: &wantMode, ModTimeNS: &wantModTime, MetadataKnown: true}) {
		t.Fatalf("directory metadata = %+v", store.directories)
	}
	emptyDigest := sha256.Sum256(nil)
	if store.files[2].SHA256 != hex.EncodeToString(emptyDigest[:]) {
		t.Fatalf("empty file digest = %s", store.files[2].SHA256)
	}
	if len(blocks.data) != 2 {
		t.Fatalf("stored blocks = %d, want 2", len(blocks.data))
	}
	for _, file := range store.files[:2] {
		if len(file.Chunks) != 1 || !bytes.Equal(blocks.data[file.Chunks[0].SHA256], volumeFS[file.Path].Data) {
			t.Fatalf("stored content for %q does not match source", file.Path)
		}
	}

	if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil {
		t.Fatal("repeat import succeeded after the store marked the volume imported")
	}
	if store.commitCall != 1 {
		t.Fatalf("repeat import committed %d times", store.commitCall)
	}
}

func TestWorkspaceLegacyImportPreservesNativeMetadata(t *testing.T) {
	volumeFS := fstest.MapFS{"README.md": legacyMapFile([]byte("welcome"), 0644)}
	service, store, _, source := newLegacyImportHarness(volumeFS, 1<<20)
	source.mutateManifest = func(_ int, manifest *mordesync.Manifest) {
		manifest.Files[0].Xattrs = []mordesync.ExtendedAttribute{{Name: "user.note", Value: []byte("file-value")}}
		manifest.Files[0].ACL = &mordesync.NativeACL{Platform: "linux", Format: "linux-posix-acl-v1", Data: []byte{1, 2}}
		for i := range manifest.Directories {
			if manifest.Directories[i].Path == "." {
				manifest.Directories[i].Xattrs = []mordesync.ExtendedAttribute{{Name: "user.note", Value: []byte("root-value")}}
			}
		}
	}

	if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if len(store.files) != 1 || !store.files[0].MetadataKnown || len(store.files[0].Xattrs) != 1 ||
		store.files[0].Xattrs[0].Name != "user.note" || string(store.files[0].Xattrs[0].Value) != "file-value" ||
		store.files[0].ACL == nil || store.files[0].ACL.Format != "linux-posix-acl-v1" {
		t.Fatalf("imported file metadata = %+v", store.files)
	}
	if len(store.directories) != 1 || store.directories[0].Path != "." || !store.directories[0].MetadataKnown ||
		len(store.directories[0].Xattrs) != 1 || string(store.directories[0].Xattrs[0].Value) != "root-value" {
		t.Fatalf("imported root metadata = %+v", store.directories)
	}
}

func TestWorkspaceLegacyImportPreservesHardlinkManifest(t *testing.T) {
	body := []byte("shared data")
	volumeFS := fstest.MapFS{
		"a.txt": legacyMapFile(body, 0640),
		"b.txt": legacyMapFile(body, 0640),
	}
	service, store, blocks, source := newLegacyImportHarness(volumeFS, 1<<20)
	source.hardlinkScans = []map[string]string{{"b.txt": "a.txt"}}

	if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if len(store.files) != 2 || store.files[0].Path != "a.txt" || store.files[1].Path != "b.txt" || store.files[1].HardlinkTo != "a.txt" {
		t.Fatalf("imported hard-link manifest = %+v", store.files)
	}
	if len(blocks.data) != 1 {
		t.Fatalf("stored blocks = %d, want shared content block", len(blocks.data))
	}
}

func TestWorkspaceLegacyImportRejectsHardlinkTopologyChange(t *testing.T) {
	body := []byte("shared data")
	volumeFS := fstest.MapFS{
		"a.txt": legacyMapFile(body, 0640),
		"b.txt": legacyMapFile(body, 0640),
	}
	service, store, _, source := newLegacyImportHarness(volumeFS, 1<<20)
	source.hardlinkScans = []map[string]string{{"b.txt": "a.txt"}, {}}

	if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil || !strings.Contains(err.Error(), "hard-link topology changed") {
		t.Fatalf("import with changed hard-link topology error = %v", err)
	}
	if store.commitCall != 0 {
		t.Fatalf("committed imports = %d, want no commit", store.commitCall)
	}
}

func TestWorkspaceLegacyImportRejectsConcurrentRunAndReleasesLockOnFailure(t *testing.T) {
	service, store, blocks, source := newLegacyImportHarness(fstest.MapFS{}, 1<<20)
	store.lockBusy = true
	if _, err := service.Import(context.Background(), legacyImportWorkspaceID); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("concurrent import error = %v, want workspace busy", err)
	}
	if blocks.removeTempCalls != 0 || source.openName != "" {
		t.Fatalf("busy import touched storage or source: recoveries=%d source=%q", blocks.removeTempCalls, source.openName)
	}

	store.lockBusy = false
	blocks.removeTempErr = errors.New("unknown residue")
	if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil || !strings.Contains(err.Error(), "recover temporary workspace blocks") {
		t.Fatalf("import with recovery error = %v", err)
	}
	if store.lockReleaseCalls != 1 || source.openName != "" {
		t.Fatalf("failed recovery lock releases=%d source=%q", store.lockReleaseCalls, source.openName)
	}
}

func TestWorkspaceLegacyImportChecksActivityBeforeTempCleanup(t *testing.T) {
	service, store, blocks, source := newLegacyImportHarness(fstest.MapFS{}, 1<<20)
	store.importReadyErr = domain.NewError(domain.CodeWorkspaceBusy, "workspace has active task work")

	if _, err := service.Import(context.Background(), legacyImportWorkspaceID); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("import with active work error = %v; want workspace busy", err)
	}
	if store.importReadyCalls != 1 || blocks.removeTempCalls != 0 || source.openName != "" || store.lockReleaseCalls != 1 {
		t.Fatalf("preflight calls=%d cleanup=%d source=%q lock releases=%d; want check before any cleanup", store.importReadyCalls, blocks.removeTempCalls, source.openName, store.lockReleaseCalls)
	}
}

func TestWorkspaceLegacyImportAllowsEmptyVolume(t *testing.T) {
	service, store, _, source := newLegacyImportHarness(fstest.MapFS{}, 100)
	result, err := service.Import(context.Background(), legacyImportWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != 1 || store.commitCall != 1 || store.files == nil || len(store.files) != 0 || len(store.directories) != 1 ||
		store.directories[0].Path != "." || !store.directories[0].MetadataKnown || !source.closer.closed {
		t.Fatalf("empty-volume import = result %+v, files %#v, commits %d, closed %v", result, store.files, store.commitCall, source.closer.closed)
	}
}

func TestWorkspaceLegacyImportPreservesEmptyDirectories(t *testing.T) {
	service, store, _, _ := newLegacyImportHarness(fstest.MapFS{
		"empty-dir":    {Mode: fs.ModeDir | 0755, ModTime: legacyImportTime},
		"nested":       {Mode: fs.ModeDir | 0755, ModTime: legacyImportTime},
		"nested/empty": {Mode: fs.ModeDir | 0755, ModTime: legacyImportTime},
		"nested/file":  legacyMapFile([]byte("content"), 0644),
	}, 100)
	result, err := service.Import(context.Background(), legacyImportWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != 1 || len(store.emptyDirectories) != 2 || store.emptyDirectories[0] != "empty-dir" || store.emptyDirectories[1] != "nested/empty" {
		t.Fatalf("legacy empty directories = %#v, result %+v", store.emptyDirectories, result)
	}
}

func TestWorkspaceLegacyImportSelectsMatchingRootInSharedLegacyVolume(t *testing.T) {
	volumeFS := fstest.MapFS{
		"w1234567/README.md":  legacyMapFile([]byte("selected"), 0644),
		"w7654321/secret.txt": legacyMapFile([]byte("other workspace"), 0600),
	}
	service, store, blocks, source := newLegacyImportHarness(volumeFS, 1<<20)
	if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if len(store.files) != 1 || store.files[0].Path != "README.md" || string(blocks.data[store.files[0].Chunks[0].SHA256]) != "selected" {
		t.Fatalf("imported files = %+v, blocks = %#v", store.files, blocks.data)
	}
	if len(source.validateRoots) != 2 || source.validateRoots[0] != legacyImportWorkspaceID || source.validateRoots[1] != legacyImportWorkspaceID {
		t.Fatalf("validated shared volume roots = %#v", source.validateRoots)
	}
	if got, err := fs.ReadFile(volumeFS, "w7654321/secret.txt"); err != nil || string(got) != "other workspace" {
		t.Fatalf("unselected Workspace source = %q, err=%v", got, err)
	}
}

func TestWorkspaceLegacyImportRejectsUnsupportedMetadataBeforeWritingBlocks(t *testing.T) {
	service, store, blocks, source := newLegacyImportHarness(fstest.MapFS{"file.txt": legacyMapFile([]byte("content"), 0644)}, 1<<20)
	source.validateErr = errors.New("workspace tree copy cannot preserve extended attributes or ACLs")
	if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil || !strings.Contains(err.Error(), "extended attributes or ACLs") {
		t.Fatalf("unsupported metadata import error = %v", err)
	}
	if blocks.putCalls != 0 || store.commitCall != 0 || len(source.validateRoots) != 1 || !source.closer.closed {
		t.Fatalf("unsupported metadata changed state: writes=%d commits=%d validations=%q closed=%v", blocks.putCalls, store.commitCall, source.validateRoots, source.closer.closed)
	}
}

func TestWorkspaceLegacyImportRejectsDirectoryChangeDuringBlockWrite(t *testing.T) {
	volumeFS := fstest.MapFS{
		"src":      {Mode: fs.ModeDir | 0755, ModTime: legacyImportTime},
		"src/file": legacyMapFile([]byte("content"), 0644),
	}
	service, store, blocks, source := newLegacyImportHarness(volumeFS, 1<<20)
	blocks.onPut = func() {
		directory := volumeFS["src"]
		directory.ModTime = legacyImportTime.Add(time.Second)
		volumeFS["src"] = directory
	}
	if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil || !strings.Contains(err.Error(), "changed during import") {
		t.Fatalf("directory change import error = %v", err)
	}
	if blocks.putCalls != 1 || store.commitCall != 0 || !source.closer.closed {
		t.Fatalf("directory change changed state: writes=%d commits=%d closed=%v", blocks.putCalls, store.commitCall, source.closer.closed)
	}
}

func TestWorkspaceLegacyImportRejectsUnmappedOrMixedSharedVolumeRoots(t *testing.T) {
	tests := []struct {
		name string
		fs   fstest.MapFS
	}{
		{
			name: "requested Workspace root is absent",
			fs: fstest.MapFS{
				"w7654321/README.md": legacyMapFile([]byte("other"), 0644),
			},
		},
		{
			name: "invalid Workspace root ID",
			fs: fstest.MapFS{
				"w123456A/README.md": legacyMapFile([]byte("invalid"), 0644),
			},
		},
		{
			name: "Workspace roots mixed with unclassified files",
			fs: fstest.MapFS{
				"w1234567/README.md": legacyMapFile([]byte("workspace"), 0644),
				"README.md":          legacyMapFile([]byte("ambiguous"), 0644),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, store, blocks, source := newLegacyImportHarness(test.fs, 1<<20)
			if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil || !strings.Contains(err.Error(), "cleanup is required") {
				t.Fatalf("ambiguous legacy volume import error = %v", err)
			}
			if store.cleanupReason == "" || store.commitCall != 0 || len(blocks.data) != 0 || !source.closer.closed {
				t.Fatalf("ambiguous import changed state: cleanup=%q commits=%d blocks=%d closed=%v", store.cleanupReason, store.commitCall, len(blocks.data), source.closer.closed)
			}
		})
	}
}

func TestWorkspaceLegacyImportRejectsUnsafeVolumesBeforeWritingBlocks(t *testing.T) {
	tests := []struct {
		name  string
		fs    fs.FS
		quota int64
		want  string
	}{
		{
			name:  "invalid path",
			fs:    legacyInvalidPathFS{},
			quota: 10,
			want:  "cleanup is required",
		},
		{
			name: "symlink",
			fs: fstest.MapFS{
				"ok.txt":   legacyMapFile([]byte("ok"), 0644),
				"link.txt": {Data: []byte("ok.txt"), Mode: fs.ModeSymlink | 0777, ModTime: legacyImportTime},
			},
			quota: 10,
			want:  "cleanup is required",
		},
		{
			name: "special item",
			fs: fstest.MapFS{
				"ok.txt": legacyMapFile([]byte("ok"), 0644),
				"pipe":   {Mode: fs.ModeNamedPipe | 0600, ModTime: legacyImportTime},
			},
			quota: 10,
			want:  "cleanup is required",
		},
		{
			name: "case collision",
			fs: fstest.MapFS{
				"A.txt": legacyMapFile([]byte("one"), 0644),
				"a.txt": legacyMapFile([]byte("two"), 0644),
			},
			quota: 10,
			want:  "cleanup is required",
		},
		{
			name:  "quota exceeded",
			fs:    fstest.MapFS{"large": legacyMapFile([]byte("12345"), 0644)},
			quota: 4,
			want:  "large",
		},
		{
			name:  "single file above sync limit",
			fs:    legacyOversizedFS{},
			quota: workspace.MaxSyncFileBytes + 1,
			want:  "cleanup is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, store, blocks, source := newLegacyImportHarness(test.fs, test.quota)
			if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil || (test.want != "" && !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("unsafe volume import error = %v", err)
			}
			if store.commitCall != 0 || blocks.putCalls != 0 || !source.closer.closed {
				t.Fatalf("commits=%d puts=%d closed=%v", store.commitCall, blocks.putCalls, source.closer.closed)
			}
			if test.want != "" && !strings.Contains(store.cleanupReason, test.want) {
				t.Fatalf("cleanup-required record = %q", store.cleanupReason)
			}
		})
	}
}

func TestWorkspaceLegacyImportRejectsManifestTooLarge(t *testing.T) {
	if _, err := legacyFileManifestSize("big.bin", (workspace.MaxSyncManifestBytes*workspace.MaxSyncBlockBytes)/70, 0644); err == nil {
		t.Fatal("oversized manifest file estimate was accepted")
	}
}

func TestWorkspaceLegacyImportChecksExistingBlocksAndSourceStability(t *testing.T) {
	t.Run("orphan block", func(t *testing.T) {
		volumeFS := fstest.MapFS{"file": legacyMapFile([]byte("current"), 0644)}
		service, store, blocks, _ := newLegacyImportHarness(volumeFS, 100)
		orphan := sha256.Sum256([]byte("not in this source"))
		blocks.data[hex.EncodeToString(orphan[:])] = []byte("not in this source")
		if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil {
			t.Fatal("import accepted an unrelated existing block")
		}
		if store.commitCall != 0 || blocks.putCalls != 0 {
			t.Fatalf("commits=%d puts=%d", store.commitCall, blocks.putCalls)
		}
	})

	t.Run("source changed after preflight", func(t *testing.T) {
		base := fstest.MapFS{"file": legacyMapFile([]byte("initial"), 0644)}
		volumeFS := &legacyChangingFS{base: base, name: "file", changeAt: 5, replacement: []byte("changed")}
		service, store, blocks, _ := newLegacyImportHarness(volumeFS, 100)
		if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil {
			t.Fatal("import accepted source changes between preflight and block transfer")
		}
		if store.commitCall != 0 || blocks.putCalls != 0 || volumeFS.opens != 5 {
			t.Fatalf("commits=%d puts=%d source opens=%d", store.commitCall, blocks.putCalls, volumeFS.opens)
		}
	})
}

func TestWorkspaceLegacyImportRetryAfterSourceChangesRecoversPartialBlock(t *testing.T) {
	oldBody := []byte("old source bytes")
	newBody := []byte("updated source bytes")
	oldDigest := sha256.Sum256(oldBody)
	oldDigestText := hex.EncodeToString(oldDigest[:])
	service, _, blocks, source := newLegacyImportHarness(fstest.MapFS{
		"a.txt": legacyMapFile(oldBody, 0644),
		"b.txt": legacyMapFile([]byte("second file"), 0644),
	}, 1<<20)
	blocks.onPut = func() {
		if blocks.putCalls == 2 {
			blocks.putErr = errors.New("injected failure after first final block")
		}
	}
	if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("first import error = %v, want injected failure", err)
	}
	if _, ok := blocks.data[oldDigestText]; !ok {
		t.Fatal("first failed import did not leave its expected partial final block")
	}

	// The source changes after the failed run. Without targeted cleanup the old
	// digest is rejected as unrelated during preflight and the pending import
	// cannot make progress.
	source.fs = fstest.MapFS{"a.txt": legacyMapFile(newBody, 0644)}
	blocks.putErr = nil
	gcStore := &blockGCTestStore{}
	gc, err := NewWorkspaceBlockGarbageCollector(gcStore, blocks)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := gc.CollectPendingLegacyImport(context.Background(), legacyImportWorkspaceID)
	if err != nil {
		t.Fatalf("recover partial import blocks: %v", err)
	}
	if stats.BlocksDeleted != 1 || stats.BytesReclaimed != int64(len(oldBody)) {
		t.Fatalf("partial import recovery stats = %#v", stats)
	}
	if _, ok := blocks.data[oldDigestText]; ok {
		t.Fatal("recovery retained the block from the changed source")
	}

	result, err := service.Import(context.Background(), legacyImportWorkspaceID)
	if err != nil {
		t.Fatalf("retry import after source changed: %v", err)
	}
	newDigest := sha256.Sum256(newBody)
	if result.Revision != 1 || len(blocks.data) != 1 || !bytes.Equal(blocks.data[hex.EncodeToString(newDigest[:])], newBody) {
		t.Fatalf("retry result=%+v stored blocks=%v", result, blocks.data)
	}
}

func TestWorkspaceLegacyImportPropagatesBlockAndContextErrors(t *testing.T) {
	t.Run("nil context", func(t *testing.T) {
		service, store, blocks, _ := newLegacyImportHarness(fstest.MapFS{}, 100)
		var nilContext context.Context
		if _, err := service.Import(nilContext, legacyImportWorkspaceID); err == nil {
			t.Fatal("import accepted a nil context")
		}
		if store.commitCall != 0 || blocks.putCalls != 0 {
			t.Fatalf("commits=%d puts=%d", store.commitCall, blocks.putCalls)
		}
	})

	t.Run("block digest mismatch", func(t *testing.T) {
		service, store, blocks, _ := newLegacyImportHarness(fstest.MapFS{"file": legacyMapFile([]byte("body"), 0644)}, 100)
		blocks.putErr = errors.New("digest mismatch")
		if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
			t.Fatalf("import error = %v", err)
		}
		if store.commitCall != 0 || blocks.putCalls != 1 {
			t.Fatalf("commits=%d puts=%d", store.commitCall, blocks.putCalls)
		}
	})

	t.Run("unexpected stored size", func(t *testing.T) {
		service, store, blocks, _ := newLegacyImportHarness(fstest.MapFS{"file": legacyMapFile([]byte("body"), 0644)}, 100)
		blocks.wrongSize = true
		if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil {
			t.Fatal("import accepted a mismatched stored block size")
		}
		if store.commitCall != 0 {
			t.Fatalf("commits=%d", store.commitCall)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		service, store, blocks, source := newLegacyImportHarness(fstest.MapFS{"file": legacyMapFile([]byte("body"), 0644)}, 100)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := service.Import(ctx, legacyImportWorkspaceID); !errors.Is(err, context.Canceled) {
			t.Fatalf("import error = %v", err)
		}
		if store.commitCall != 0 || blocks.putCalls != 0 || source.openName != "" {
			t.Fatalf("commits=%d puts=%d opened=%q", store.commitCall, blocks.putCalls, source.openName)
		}
	})

	t.Run("cancelled during block write", func(t *testing.T) {
		service, store, blocks, _ := newLegacyImportHarness(fstest.MapFS{"file": legacyMapFile([]byte("body"), 0644)}, 100)
		ctx, cancel := context.WithCancel(context.Background())
		blocks.onPut = cancel
		if _, err := service.Import(ctx, legacyImportWorkspaceID); !errors.Is(err, context.Canceled) {
			t.Fatalf("import error = %v", err)
		}
		if store.commitCall != 0 || blocks.putCalls != 1 {
			t.Fatalf("commits=%d puts=%d", store.commitCall, blocks.putCalls)
		}
	})
}

func TestWorkspaceLegacyImportSplitsFilesIntoBoundedBlocks(t *testing.T) {
	body := bytes.Repeat([]byte("x"), int(workspace.MaxSyncBlockBytes)+1)
	service, store, blocks, _ := newLegacyImportHarness(fstest.MapFS{"large.bin": legacyMapFile(body, 0644)}, int64(len(body)))
	if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if len(store.files) != 1 || len(store.files[0].Chunks) != 2 || store.files[0].Chunks[0].SizeBytes != workspace.MaxSyncBlockBytes || store.files[0].Chunks[1].SizeBytes != 1 {
		t.Fatalf("large file chunks = %+v", store.files)
	}
	if !bytes.Equal(blocks.data[store.files[0].Chunks[0].SHA256], body[:workspace.MaxSyncBlockBytes]) || !bytes.Equal(blocks.data[store.files[0].Chunks[1].SHA256], body[workspace.MaxSyncBlockBytes:]) {
		t.Fatal("stored chunks differ from source bytes")
	}
}

func TestWorkspaceLegacyImportRejectsFileParentPathCollision(t *testing.T) {
	volumeFS := fstest.MapFS{
		"a/b.txt": legacyMapFile([]byte("child"), 0644),
		"A":       legacyMapFile([]byte("parent"), 0644),
	}
	service, store, blocks, _ := newLegacyImportHarness(volumeFS, 100)
	if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil {
		t.Fatal("import accepted a file path that collides with a child directory")
	}
	if store.commitCall != 0 || blocks.putCalls != 0 {
		t.Fatalf("commits=%d puts=%d", store.commitCall, blocks.putCalls)
	}
}

func TestWorkspaceLegacyImportLimitsTotalPathEntries(t *testing.T) {
	service, store, blocks, _ := newLegacyImportHarness(legacyLargeDirFS{count: maxLegacyWorkspaceEntries + 1}, 100)
	if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil || !strings.Contains(err.Error(), "path entry limit") {
		t.Fatalf("import error = %v", err)
	}
	if !strings.Contains(store.cleanupReason, "path entry limit") {
		t.Fatalf("path-entry cleanup record = %q", store.cleanupReason)
	}
	if store.commitCall != 0 || blocks.putCalls != 0 {
		t.Fatalf("commits=%d puts=%d", store.commitCall, blocks.putCalls)
	}
}

func TestWorkspaceLegacyImportPersistsFileCountAndManifestCleanupReasons(t *testing.T) {
	t.Run("file count", func(t *testing.T) {
		service, store, blocks, _ := newLegacyImportHarness(legacyLimitFilesFS{count: workspace.MaxSyncFiles + 1}, 1<<30)
		if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil || !strings.Contains(err.Error(), "file count limit") {
			t.Fatalf("import error = %v", err)
		}
		if !strings.Contains(store.cleanupReason, "file count limit") || store.commitCall != 0 || blocks.putCalls != 0 {
			t.Fatalf("cleanup=%q commits=%d puts=%d", store.cleanupReason, store.commitCall, blocks.putCalls)
		}
	})
	t.Run("manifest size", func(t *testing.T) {
		service, store, blocks, _ := newLegacyImportHarness(legacyLimitFilesFS{count: 6000, namePadding: 3000}, 1<<30)
		if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil || !strings.Contains(err.Error(), "manifest exceeds 16 MiB") {
			t.Fatalf("import error = %v", err)
		}
		if !strings.Contains(store.cleanupReason, "manifest exceeds 16 MiB") || store.commitCall != 0 || blocks.putCalls != 0 {
			t.Fatalf("cleanup=%q commits=%d puts=%d", store.cleanupReason, store.commitCall, blocks.putCalls)
		}
	})
}

type legacyChangingFS struct {
	base        fstest.MapFS
	name        string
	opens       int
	changeAt    int
	replacement []byte
}

func (f *legacyChangingFS) Open(name string) (fs.File, error) {
	if name == f.name {
		f.opens++
		if f.opens == f.changeAt {
			f.base[name].Data = append([]byte(nil), f.replacement...)
		}
	}
	return f.base.Open(name)
}

type legacyInvalidPathFS struct{}

func (legacyInvalidPathFS) Open(name string) (fs.File, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	return legacyStaticFile{info: legacyStaticInfo{name: ".", mode: fs.ModeDir | 0755}}, nil
}

func (legacyInvalidPathFS) Stat(name string) (fs.FileInfo, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	return legacyStaticInfo{name: ".", mode: fs.ModeDir | 0755}, nil
}

func (legacyInvalidPathFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	return []fs.DirEntry{legacyStaticEntry{info: legacyStaticInfo{name: "../escape", mode: 0644, size: 1}}}, nil
}

type legacyStaticFile struct {
	info legacyStaticInfo
}

func (f legacyStaticFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (legacyStaticFile) Read([]byte) (int, error)     { return 0, io.EOF }
func (legacyStaticFile) Close() error                 { return nil }

type legacyStaticInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (i legacyStaticInfo) Name() string       { return i.name }
func (i legacyStaticInfo) Size() int64        { return i.size }
func (i legacyStaticInfo) Mode() fs.FileMode  { return i.mode }
func (i legacyStaticInfo) ModTime() time.Time { return legacyImportTime }
func (i legacyStaticInfo) IsDir() bool        { return i.mode.IsDir() }
func (i legacyStaticInfo) Sys() any           { return nil }

type legacyStaticEntry struct {
	info legacyStaticInfo
}

func (e legacyStaticEntry) Name() string               { return e.info.Name() }
func (e legacyStaticEntry) IsDir() bool                { return e.info.IsDir() }
func (e legacyStaticEntry) Type() fs.FileMode          { return e.info.Mode().Type() }
func (e legacyStaticEntry) Info() (fs.FileInfo, error) { return e.info, nil }

var _ fs.FS = legacyInvalidPathFS{}
var _ fs.StatFS = legacyInvalidPathFS{}
var _ fs.ReadDirFS = legacyInvalidPathFS{}

type legacyOversizedFS struct{}

func (legacyOversizedFS) Open(name string) (fs.File, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	return legacyStaticFile{info: legacyStaticInfo{name: ".", mode: fs.ModeDir | 0755}}, nil
}

func (legacyOversizedFS) Stat(name string) (fs.FileInfo, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	return legacyStaticInfo{name: ".", mode: fs.ModeDir | 0755}, nil
}

func (legacyOversizedFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	return []fs.DirEntry{legacyStaticEntry{info: legacyStaticInfo{name: "oversized.bin", size: workspace.MaxSyncFileBytes + 1, mode: 0644}}}, nil
}

var _ fs.FS = legacyOversizedFS{}
var _ fs.StatFS = legacyOversizedFS{}
var _ fs.ReadDirFS = legacyOversizedFS{}

type legacyLargeDirFS struct {
	count int
}

func (f legacyLargeDirFS) Open(name string) (fs.File, error) {
	if name == "." || strings.HasPrefix(name, "d") {
		return legacyStaticFile{info: legacyStaticInfo{name: path.Base(name), mode: fs.ModeDir | 0755}}, nil
	}
	return nil, fs.ErrNotExist
}

func (legacyLargeDirFS) Stat(name string) (fs.FileInfo, error) {
	if name == "." {
		return legacyStaticInfo{name: ".", mode: fs.ModeDir | 0755}, nil
	}
	return nil, fs.ErrNotExist
}

func (f legacyLargeDirFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return []fs.DirEntry{}, nil
	}
	entries := make([]fs.DirEntry, f.count)
	for i := range entries {
		entries[i] = legacyStaticEntry{info: legacyStaticInfo{name: fmt.Sprintf("d%06d", i), mode: fs.ModeDir | 0755}}
	}
	return entries, nil
}

var _ fs.FS = legacyLargeDirFS{}
var _ fs.StatFS = legacyLargeDirFS{}
var _ fs.ReadDirFS = legacyLargeDirFS{}

type legacyLimitFilesFS struct {
	count       int
	namePadding int
}

func (f legacyLimitFilesFS) Open(name string) (fs.File, error) {
	if name == "." {
		return legacyStaticFile{info: legacyStaticInfo{name: ".", mode: fs.ModeDir | 0755}}, nil
	}
	if _, ok := f.fileIndex(name); !ok {
		return nil, fs.ErrNotExist
	}
	return legacyStaticFile{info: legacyStaticInfo{name: path.Base(name), mode: 0644}}, nil
}

func (f legacyLimitFilesFS) Stat(name string) (fs.FileInfo, error) {
	if name == "." {
		return legacyStaticInfo{name: ".", mode: fs.ModeDir | 0755}, nil
	}
	if _, ok := f.fileIndex(name); !ok {
		return nil, fs.ErrNotExist
	}
	return legacyStaticInfo{name: path.Base(name), mode: 0644}, nil
}

func (f legacyLimitFilesFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	entries := make([]fs.DirEntry, f.count)
	for i := range entries {
		fileName := f.fileName(i)
		entries[i] = legacyStaticEntry{info: legacyStaticInfo{name: fileName, mode: 0644}}
	}
	return entries, nil
}

func (f legacyLimitFilesFS) fileName(index int) string {
	return fmt.Sprintf("f%05d%s", index, strings.Repeat("x", f.namePadding))
}

func (f legacyLimitFilesFS) fileIndex(name string) (int, bool) {
	if len(name) != 6+f.namePadding || name[0] != 'f' || strings.Trim(name[6:], "x") != "" {
		return 0, false
	}
	index, err := strconv.Atoi(name[1:6])
	return index, err == nil && index >= 0 && index < f.count && f.fileName(index) == name
}

var _ fs.FS = legacyLimitFilesFS{}
var _ fs.StatFS = legacyLimitFilesFS{}
var _ fs.ReadDirFS = legacyLimitFilesFS{}

func TestLegacyManifestSizeMatchesJSONEncoding(t *testing.T) {
	file := workspace.File{
		Path: "src/main.go", SizeBytes: 3, SHA256: strings.Repeat("a", 64), Mode: 0644,
		Chunks: []workspace.Chunk{{SHA256: strings.Repeat("b", 64), SizeBytes: 3}},
	}
	encoded, err := jsonMarshalLegacyFiles([]workspace.File{file})
	if err != nil {
		t.Fatal(err)
	}
	estimate, err := legacyFileManifestSize(file.Path, file.SizeBytes, file.Mode)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(encoded)) != estimate+2 {
		t.Fatalf("estimated file JSON bytes=%d, actual array bytes=%d", estimate, len(encoded))
	}
}

func jsonMarshalLegacyFiles(files []workspace.File) ([]byte, error) {
	return json.Marshal(files)
}

func TestLegacyImportErrorContextIsStable(t *testing.T) {
	store := &legacyImportStoreFake{volume: LegacyWorkspaceVolume{WorkspaceID: legacyImportWorkspaceID, VolumeName: "legacy-volume", QuotaBytes: 100}, pending: true}
	source := &legacyImportSourceFake{openErr: fmt.Errorf("docker unavailable"), closer: &legacyImportCloser{}}
	blocks := newLegacyImportBlocksFake()
	service, err := NewWorkspaceLegacyImportService(store, source, blocks, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil || !strings.Contains(err.Error(), "open legacy workspace volume") {
		t.Fatalf("import error = %v", err)
	}
}
