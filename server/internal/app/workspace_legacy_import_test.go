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
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

const legacyImportWorkspaceID = "w1234567"

var legacyImportTime = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

type legacyImportStoreFake struct {
	volume           LegacyWorkspaceVolume
	pending          bool
	lockBusy         bool
	lockReleaseCalls int
	commitCall       int
	reservations     []WorkspaceBlockReservation
	finalized        []WorkspaceBlockAllocation
	files            []workspace.File
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

func (s *legacyImportStoreFake) CommitWorkspaceLegacyVolumeImport(ctx context.Context, value LegacyWorkspaceVolume, files []workspace.File) (workspace.Workspace, error) {
	if err := ctx.Err(); err != nil {
		return workspace.Workspace{}, err
	}
	if !s.pending || value != s.volume {
		return workspace.Workspace{}, errors.New("legacy volume changed before commit")
	}
	s.pending = false
	s.commitCall++
	s.files = append([]workspace.File{}, files...)
	revision := int64(1)
	if len(files) == 0 {
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

func (s *legacyImportBlocksFake) RemoveStaleTempBlocks(string) error {
	s.removeTempCalls++
	return s.removeTempErr
}

type legacyImportSourceFake struct {
	fs       fs.FS
	openErr  error
	closer   *legacyImportCloser
	openName string
}

func (s *legacyImportSourceFake) OpenLegacyVolume(_ context.Context, name string) (fs.FS, io.Closer, error) {
	s.openName = name
	if s.openErr != nil {
		return nil, nil, s.openErr
	}
	return s.fs, s.closer, nil
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
	if source.openName != "legacy-volume" || !source.closer.closed || store.commitCall != 1 || store.lockReleaseCalls != 1 || blocks.removeTempCalls != 1 {
		t.Fatalf("open=%q closed=%v commits=%d lock releases=%d temp recoveries=%d", source.openName, source.closer.closed, store.commitCall, store.lockReleaseCalls, blocks.removeTempCalls)
	}
	if len(store.files) != 3 || store.files[0].Path != "README.md" || store.files[1].Path != "bin/tool" || store.files[2].Path != "src/empty" {
		t.Fatalf("manifest files = %+v", store.files)
	}
	if store.files[1].Mode != 0755 || store.files[2].Mode != 0600 || store.files[2].SizeBytes != 0 || len(store.files[2].Chunks) != 0 {
		t.Fatalf("file permissions or empty file metadata = %+v", store.files)
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

func TestWorkspaceLegacyImportAllowsEmptyVolume(t *testing.T) {
	service, store, _, source := newLegacyImportHarness(fstest.MapFS{}, 100)
	result, err := service.Import(context.Background(), legacyImportWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != 0 || store.commitCall != 1 || store.files == nil || len(store.files) != 0 || !source.closer.closed {
		t.Fatalf("empty-volume import = result %+v, files %#v, commits %d, closed %v", result, store.files, store.commitCall, source.closer.closed)
	}
}

func TestWorkspaceLegacyImportRejectsUnsafeVolumesBeforeWritingBlocks(t *testing.T) {
	tests := []struct {
		name  string
		fs    fs.FS
		quota int64
	}{
		{
			name:  "invalid path",
			fs:    legacyInvalidPathFS{},
			quota: 10,
		},
		{
			name: "symlink",
			fs: fstest.MapFS{
				"ok.txt":   legacyMapFile([]byte("ok"), 0644),
				"link.txt": {Data: []byte("ok.txt"), Mode: fs.ModeSymlink | 0777, ModTime: legacyImportTime},
			},
			quota: 10,
		},
		{
			name: "special item",
			fs: fstest.MapFS{
				"ok.txt": legacyMapFile([]byte("ok"), 0644),
				"pipe":   {Mode: fs.ModeNamedPipe | 0600, ModTime: legacyImportTime},
			},
			quota: 10,
		},
		{
			name: "empty directory",
			fs: fstest.MapFS{
				"ok.txt":    legacyMapFile([]byte("ok"), 0644),
				"empty-dir": {Mode: fs.ModeDir | 0755, ModTime: legacyImportTime},
			},
			quota: 10,
		},
		{
			name: "case collision",
			fs: fstest.MapFS{
				"A.txt": legacyMapFile([]byte("one"), 0644),
				"a.txt": legacyMapFile([]byte("two"), 0644),
			},
			quota: 10,
		},
		{
			name:  "quota exceeded",
			fs:    fstest.MapFS{"large": legacyMapFile([]byte("12345"), 0644)},
			quota: 4,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, store, blocks, source := newLegacyImportHarness(test.fs, test.quota)
			if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil {
				t.Fatal("unsafe volume import succeeded")
			}
			if store.commitCall != 0 || blocks.putCalls != 0 || !source.closer.closed {
				t.Fatalf("commits=%d puts=%d closed=%v", store.commitCall, blocks.putCalls, source.closer.closed)
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
		volumeFS := &legacyChangingFS{base: base, name: "file", changeAt: 3, replacement: []byte("changed")}
		service, store, blocks, _ := newLegacyImportHarness(volumeFS, 100)
		if _, err := service.Import(context.Background(), legacyImportWorkspaceID); err == nil {
			t.Fatal("import accepted source changes between preflight and block transfer")
		}
		if store.commitCall != 0 || blocks.putCalls != 0 || volumeFS.opens != 3 {
			t.Fatalf("commits=%d puts=%d source opens=%d", store.commitCall, blocks.putCalls, volumeFS.opens)
		}
	})
}

func TestWorkspaceLegacyImportPropagatesBlockAndContextErrors(t *testing.T) {
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
	if store.commitCall != 0 || blocks.putCalls != 0 {
		t.Fatalf("commits=%d puts=%d", store.commitCall, blocks.putCalls)
	}
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
