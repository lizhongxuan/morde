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
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

const maxLegacyWorkspaceEntries = 2 * workspace.MaxSyncFiles

// LegacyWorkspaceVolume is the importable legacy volume's locked workspace state.
type LegacyWorkspaceVolume struct {
	WorkspaceID string
	VolumeName  string
	Revision    int64
	UsedBytes   int64
	QuotaBytes  int64
}

// WorkspaceLegacyImportStore commits the imported manifest and workspace state atomically.
type WorkspaceLegacyImportStore interface {
	TryWorkspaceBlockStoreLock(ctx context.Context, workspaceID string) (LeaderLock, bool, error)
	GetPendingWorkspaceLegacyVolume(ctx context.Context, workspaceID string) (LegacyWorkspaceVolume, error)
	ReserveWorkspaceBlockUsage(context.Context, string, []WorkspaceBlockReservation, int64) error
	FinalizeWorkspaceBlockUsage(context.Context, string, string, int64, int64) error
	CommitWorkspaceLegacyVolumeImport(ctx context.Context, value LegacyWorkspaceVolume, files []workspace.File) (workspace.Workspace, error)
}

// LegacyWorkspaceVolumeSource opens an unattached legacy volume as a read-only filesystem.
type LegacyWorkspaceVolumeSource interface {
	OpenLegacyVolume(ctx context.Context, name string) (fs.FS, io.Closer, error)
}

type WorkspaceLegacyImportBlockStore interface {
	PutBlock(workspaceID, digest string, src io.Reader) (int64, error)
	ReservationBytes(size int64) (int64, error)
	BlockAllocatedBytes(workspaceID, digest string) (int64, error)
	ListBlockDigests(workspaceID string) ([]string, error)
	RemoveStaleTempBlocks(workspaceID string) error
}

type WorkspaceLegacyImportService struct {
	store         WorkspaceLegacyImportStore
	source        LegacyWorkspaceVolumeSource
	blocks        WorkspaceLegacyImportBlockStore
	maxOwnerBytes int64
}

func NewWorkspaceLegacyImportService(store WorkspaceLegacyImportStore, source LegacyWorkspaceVolumeSource, blocks WorkspaceLegacyImportBlockStore, maxOwnerBytes int64) (*WorkspaceLegacyImportService, error) {
	if store == nil || source == nil || blocks == nil || maxOwnerBytes <= 0 {
		return nil, errors.New("legacy workspace import dependencies and owner quota are required")
	}
	return &WorkspaceLegacyImportService{store: store, source: source, blocks: blocks, maxOwnerBytes: maxOwnerBytes}, nil
}

func (s *WorkspaceLegacyImportService) Import(ctx context.Context, workspaceID string) (result workspace.Workspace, retErr error) {
	if !domain.ValidWorkspaceID(workspaceID) {
		return workspace.Workspace{}, domain.InvalidArgument("workspace_id", "workspace id is invalid")
	}
	if err := ctx.Err(); err != nil {
		return workspace.Workspace{}, err
	}
	lock, acquired, err := s.store.TryWorkspaceBlockStoreLock(ctx, workspaceID)
	if err != nil {
		return workspace.Workspace{}, fmt.Errorf("lock legacy workspace import: %w", err)
	}
	if !acquired {
		return workspace.Workspace{}, domain.NewError(domain.CodeWorkspaceBusy, "legacy workspace import is already in progress")
	}
	defer func() {
		if err := lock.Release(ctx); retErr == nil && err != nil {
			retErr = fmt.Errorf("release legacy workspace import lock: %w", err)
		}
	}()

	legacy, err := s.store.GetPendingWorkspaceLegacyVolume(ctx, workspaceID)
	if err != nil {
		return workspace.Workspace{}, fmt.Errorf("get pending legacy workspace volume: %w", err)
	}
	if legacy.WorkspaceID != workspaceID || legacy.VolumeName == "" || legacy.Revision != 0 || legacy.UsedBytes != 0 || legacy.QuotaBytes <= 0 {
		return workspace.Workspace{}, errors.New("legacy workspace volume is not in an importable initial state")
	}
	// Portal refuses to serve while legacy imports are pending; the advisory lock
	// serializes import commands, so only abandoned temporary blocks can exist here.
	if err := s.blocks.RemoveStaleTempBlocks(workspaceID); err != nil {
		return workspace.Workspace{}, fmt.Errorf("recover temporary workspace blocks: %w", err)
	}

	volumeFS, closer, err := s.source.OpenLegacyVolume(ctx, legacy.VolumeName)
	if err != nil {
		return workspace.Workspace{}, fmt.Errorf("open legacy workspace volume: %w", err)
	}
	if volumeFS == nil || closer == nil {
		if closer != nil {
			_ = closer.Close()
		}
		return workspace.Workspace{}, errors.New("legacy workspace volume source returned an incomplete filesystem")
	}

	files, sources, existing, importErr := s.prepareLegacyImport(ctx, legacy, volumeFS)
	if importErr != nil {
		_ = closer.Close()
		return workspace.Workspace{}, importErr
	}
	reservations, err := s.legacyBlockReservations(workspaceID, files, existing)
	if err != nil {
		_ = closer.Close()
		return workspace.Workspace{}, err
	}
	if err := s.store.ReserveWorkspaceBlockUsage(ctx, workspaceID, reservations, s.maxOwnerBytes); err != nil {
		_ = closer.Close()
		return workspace.Workspace{}, err
	}
	writeErr := s.writeLegacyBlocks(ctx, legacy, volumeFS, sources, files, existing)
	closeErr := closer.Close()
	if writeErr != nil {
		return workspace.Workspace{}, writeErr
	}
	if closeErr != nil {
		return workspace.Workspace{}, fmt.Errorf("close legacy workspace volume: %w", closeErr)
	}
	result, err = s.store.CommitWorkspaceLegacyVolumeImport(ctx, legacy, files)
	if err != nil {
		return workspace.Workspace{}, fmt.Errorf("commit legacy workspace volume import: %w", err)
	}
	return result, nil
}

type legacySourceFile struct {
	path string
	info legacyFileInfo
}

type legacyFileInfo struct {
	name    string
	size    int64
	mode    fs.FileMode
	modTime time.Time
	sys     any
	raw     fs.FileInfo
}

func snapshotLegacyFileInfo(info fs.FileInfo) legacyFileInfo {
	return legacyFileInfo{name: info.Name(), size: info.Size(), mode: info.Mode(), modTime: info.ModTime(), sys: info.Sys(), raw: info}
}

func (i legacyFileInfo) Name() string       { return i.name }
func (i legacyFileInfo) Size() int64        { return i.size }
func (i legacyFileInfo) Mode() fs.FileMode  { return i.mode }
func (i legacyFileInfo) ModTime() time.Time { return i.modTime }
func (i legacyFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i legacyFileInfo) Sys() any           { return i.sys }

func (s *WorkspaceLegacyImportService) prepareLegacyImport(ctx context.Context, legacy LegacyWorkspaceVolume, volumeFS fs.FS) ([]workspace.File, []legacySourceFile, map[string]struct{}, error) {
	sources, err := scanLegacyWorkspace(ctx, volumeFS, legacy.QuotaBytes)
	if err != nil {
		return nil, nil, nil, err
	}

	files := make([]workspace.File, 0, len(sources))
	manifestBytes := int64(2) // JSON array delimiters.
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		file, err := readLegacyFile(ctx, volumeFS, source, nil)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("preflight legacy workspace file %q: %w", source.path, err)
		}
		if len(files) > 0 {
			manifestBytes++
		}
		encodedSize, err := legacyFileManifestSize(source.path, source.info.Size(), uint32(source.info.Mode().Perm()&0777))
		if err != nil || manifestBytes > workspace.MaxSyncManifestBytes-encodedSize {
			return nil, nil, nil, errors.New("legacy workspace manifest exceeds 16 MiB")
		}
		manifestBytes += encodedSize
		files = append(files, file)
	}
	if manifestBytes > workspace.MaxSyncManifestBytes {
		return nil, nil, nil, errors.New("legacy workspace manifest exceeds 16 MiB")
	}
	encodedManifest, err := json.Marshal(files)
	if err != nil || int64(len(encodedManifest)) > workspace.MaxSyncManifestBytes {
		return nil, nil, nil, errors.New("legacy workspace manifest exceeds 16 MiB")
	}

	// Refuse unrelated pre-existing blocks so a retry can only reuse bytes from this source.
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	knownBlocks := make(map[string]struct{})
	for _, file := range files {
		for _, chunk := range file.Chunks {
			knownBlocks[chunk.SHA256] = struct{}{}
		}
	}
	existingBlocks, err := s.blocks.ListBlockDigests(legacy.WorkspaceID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list existing workspace blocks: %w", err)
	}
	existing := make(map[string]struct{}, len(existingBlocks))
	for _, digest := range existingBlocks {
		if _, ok := knownBlocks[digest]; !ok {
			return nil, nil, nil, fmt.Errorf("legacy workspace import found an unrelated block %q", digest)
		}
		existing[digest] = struct{}{}
	}
	return files, sources, existing, nil
}

func (s *WorkspaceLegacyImportService) legacyBlockReservations(workspaceID string, files []workspace.File, existing map[string]struct{}) ([]WorkspaceBlockReservation, error) {
	reservations := make([]WorkspaceBlockReservation, 0)
	seen := make(map[string]struct{})
	for _, file := range files {
		for _, chunk := range file.Chunks {
			if _, ok := existing[chunk.SHA256]; ok {
				continue
			}
			if _, ok := seen[chunk.SHA256]; ok {
				continue
			}
			reservedBytes, err := s.blocks.ReservationBytes(chunk.SizeBytes)
			if err != nil {
				return nil, fmt.Errorf("estimate legacy workspace block reservation: %w", err)
			}
			if reservedBytes <= 0 {
				return nil, domain.NewError(domain.CodeInternal, "legacy workspace block reservation is invalid")
			}
			seen[chunk.SHA256] = struct{}{}
			reservations = append(reservations, WorkspaceBlockReservation{WorkspaceID: workspaceID, Digest: chunk.SHA256, ReservedBytes: reservedBytes})
		}
	}
	return reservations, nil
}

func (s *WorkspaceLegacyImportService) writeLegacyBlocks(ctx context.Context, legacy LegacyWorkspaceVolume, volumeFS fs.FS, sources []legacySourceFile, files []workspace.File, existing map[string]struct{}) error {
	for i, source := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		writer := &legacyBlockWriter{store: s.blocks, usageStore: s.store, workspaceID: legacy.WorkspaceID, expected: files[i], existing: existing, maxOwnerBytes: s.maxOwnerBytes}
		if _, err := readLegacyFile(ctx, volumeFS, source, writer); err != nil {
			return fmt.Errorf("store legacy workspace file %q: %w", source.path, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyLegacyWorkspaceTree(ctx, volumeFS, sources, legacy.QuotaBytes); err != nil {
		return err
	}
	return ctx.Err()
}

func scanLegacyWorkspace(ctx context.Context, volumeFS fs.FS, quotaBytes int64) ([]legacySourceFile, error) {
	root, err := fs.Stat(volumeFS, ".")
	if err != nil || !root.IsDir() {
		return nil, errors.New("legacy workspace volume root is unavailable")
	}
	sources := make([]legacySourceFile, 0)
	pathCase := make(map[string]string)
	files := make(map[string]string)
	ancestorDirs := make(map[string]struct{})
	dirChildren := map[string]int{".": 0}
	var totalBytes int64
	manifestBytes := int64(2)
	fileCount := 0
	entryCount := 0
	err = fs.WalkDir(volumeFS, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return fmt.Errorf("walk legacy workspace volume: %w", walkErr)
		}
		if name == "." {
			if !entry.IsDir() {
				return errors.New("legacy workspace volume root is not a directory")
			}
			return nil
		}
		if err := workspace.ValidateRelativePath(name); err != nil {
			return fmt.Errorf("legacy workspace path %q is invalid: %w", name, err)
		}
		entryCount++
		if entryCount > maxLegacyWorkspaceEntries {
			return errors.New("legacy workspace exceeds path entry limit")
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect legacy workspace path %q: %w", name, err)
		}
		mode := info.Mode()
		if mode&fs.ModeSymlink != 0 || (!mode.IsDir() && !mode.IsRegular()) {
			return fmt.Errorf("legacy workspace path %q is not a regular file or directory", name)
		}
		folded := strings.ToLower(name)
		if previous, ok := pathCase[folded]; ok && previous != name {
			return fmt.Errorf("legacy workspace paths %q and %q collide by case", previous, name)
		}
		pathCase[folded] = name
		for parent := path.Dir(folded); parent != "."; parent = path.Dir(parent) {
			if previous, ok := files[parent]; ok {
				return fmt.Errorf("legacy workspace file %q is a parent of %q", previous, name)
			}
			ancestorDirs[parent] = struct{}{}
		}
		parent := path.Dir(name)
		parentKey := strings.ToLower(parent)
		dirChildren[parentKey]++
		if mode.IsDir() {
			if _, isFile := files[folded]; isFile {
				return fmt.Errorf("legacy workspace path %q is both a file and directory", name)
			}
			dirChildren[folded] = 0
			return nil
		}
		if _, isDir := dirChildren[folded]; isDir {
			return fmt.Errorf("legacy workspace path %q is both a file and directory", name)
		}
		if _, isParent := ancestorDirs[folded]; isParent {
			return fmt.Errorf("legacy workspace file %q conflicts with a child path", name)
		}
		if fileCount >= workspace.MaxSyncFiles {
			return errors.New("legacy workspace exceeds file count limit")
		}
		if info.Size() < 0 || totalBytes > int64(^uint64(0)>>1)-info.Size() {
			return errors.New("legacy workspace file sizes overflow int64")
		}
		if totalBytes > quotaBytes-info.Size() {
			return errors.New("legacy workspace exceeds its quota")
		}
		if fileCount > 0 {
			manifestBytes++
		}
		encodedSize, err := legacyFileManifestSize(name, info.Size(), uint32(mode.Perm()&0777))
		if err != nil || manifestBytes > workspace.MaxSyncManifestBytes-encodedSize {
			return errors.New("legacy workspace manifest exceeds 16 MiB")
		}
		manifestBytes += encodedSize
		if manifestBytes > workspace.MaxSyncManifestBytes {
			return errors.New("legacy workspace manifest exceeds 16 MiB")
		}
		totalBytes += info.Size()
		files[folded] = name
		sources = append(sources, legacySourceFile{path: name, info: snapshotLegacyFileInfo(info)})
		fileCount++
		return nil
	})
	if err != nil {
		return nil, err
	}
	for dir, children := range dirChildren {
		if dir != "." && children == 0 {
			return nil, fmt.Errorf("legacy workspace contains empty directory %q", dir)
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].path < sources[j].path })
	return sources, nil
}

type legacyBlockWriter struct {
	store         WorkspaceLegacyImportBlockStore
	usageStore    WorkspaceLegacyImportStore
	workspaceID   string
	expected      workspace.File
	existing      map[string]struct{}
	maxOwnerBytes int64
}

func readLegacyFile(ctx context.Context, volumeFS fs.FS, source legacySourceFile, writer *legacyBlockWriter) (workspace.File, error) {
	file, err := volumeFS.Open(source.path)
	if err != nil {
		return workspace.File{}, err
	}
	before, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return workspace.File{}, err
	}
	if !sameLegacyFileInfo(source.info, before) || !before.Mode().IsRegular() {
		_ = file.Close()
		return workspace.File{}, errors.New("source file changed after scan")
	}

	wholeHash := sha256.New()
	chunks := make([]workspace.Chunk, 0, (before.Size()+workspace.MaxSyncBlockBytes-1)/workspace.MaxSyncBlockBytes)
	remaining := before.Size()
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return workspace.File{}, err
		}
		size := remaining
		if size > workspace.MaxSyncBlockBytes {
			size = workspace.MaxSyncBlockBytes
		}
		body := make([]byte, int(size))
		if _, err := io.ReadFull(file, body); err != nil {
			_ = file.Close()
			return workspace.File{}, fmt.Errorf("read source file block: %w", err)
		}
		_, _ = wholeHash.Write(body)
		chunkHash := sha256.Sum256(body)
		chunk := workspace.Chunk{SHA256: hex.EncodeToString(chunkHash[:]), SizeBytes: size}
		if writer != nil {
			if len(chunks) >= len(writer.expected.Chunks) || writer.expected.Chunks[len(chunks)] != chunk {
				_ = file.Close()
				return workspace.File{}, errors.New("source file content changed after preflight")
			}
			if _, exists := writer.existing[chunk.SHA256]; !exists {
				stored, err := writer.store.PutBlock(writer.workspaceID, chunk.SHA256, bytes.NewReader(body))
				if err != nil {
					_ = file.Close()
					return workspace.File{}, fmt.Errorf("put content block: %w", err)
				}
				if stored != size {
					_ = file.Close()
					return workspace.File{}, errors.New("content block store accepted an unexpected byte count")
				}
				allocatedBytes, err := writer.store.BlockAllocatedBytes(writer.workspaceID, chunk.SHA256)
				if err != nil {
					_ = file.Close()
					return workspace.File{}, fmt.Errorf("inspect allocated content block bytes: %w", err)
				}
				if err := writer.usageStore.FinalizeWorkspaceBlockUsage(ctx, writer.workspaceID, chunk.SHA256, allocatedBytes, writer.maxOwnerBytes); err != nil {
					_ = file.Close()
					return workspace.File{}, fmt.Errorf("settle allocated content block usage: %w", err)
				}
			}
		}
		chunks = append(chunks, chunk)
		remaining -= size
	}
	var extra [1]byte
	n, readErr := file.Read(extra[:])
	if n != 0 || !errors.Is(readErr, io.EOF) {
		_ = file.Close()
		if readErr == nil || errors.Is(readErr, io.EOF) {
			return workspace.File{}, errors.New("source file size changed while reading")
		}
		return workspace.File{}, fmt.Errorf("check source file end: %w", readErr)
	}
	after, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return workspace.File{}, fmt.Errorf("inspect source file after read: %w", statErr)
	}
	if closeErr != nil {
		return workspace.File{}, fmt.Errorf("close source file: %w", closeErr)
	}
	pathInfo, pathErr := fs.Stat(volumeFS, source.path)
	if pathErr != nil || !sameLegacyFileInfo(source.info, after) || !sameLegacyFileInfo(source.info, pathInfo) {
		return workspace.File{}, errors.New("source file changed while reading")
	}
	digest := hex.EncodeToString(wholeHash.Sum(nil))
	result := workspace.File{Path: source.path, SizeBytes: before.Size(), SHA256: digest, Mode: uint32(before.Mode().Perm() & 0777), Chunks: chunks}
	if writer != nil && (result.SHA256 != writer.expected.SHA256 || len(chunks) != len(writer.expected.Chunks)) {
		return workspace.File{}, errors.New("source file content changed after preflight")
	}
	if err := workspace.ValidateFile(result); err != nil {
		return workspace.File{}, fmt.Errorf("validate legacy file manifest: %w", err)
	}
	return result, nil
}

func sameLegacyFileInfo(left legacyFileInfo, right fs.FileInfo) bool {
	if right == nil || left.Name() != right.Name() || left.Size() != right.Size() || left.Mode() != right.Mode() || !left.ModTime().Equal(right.ModTime()) {
		return false
	}
	if left.Sys() == nil || right.Sys() == nil {
		return true
	}
	return os.SameFile(left.raw, right)
}

func legacyFileManifestSize(filePath string, sizeBytes int64, mode uint32) (int64, error) {
	base, err := json.Marshal(workspace.File{Path: filePath, SizeBytes: sizeBytes, SHA256: strings.Repeat("a", 64), Mode: mode, Chunks: []workspace.Chunk{}})
	if err != nil {
		return 0, fmt.Errorf("encode legacy workspace manifest file: %w", err)
	}
	added := int64(len(base))
	chunkCount := sizeBytes / workspace.MaxSyncBlockBytes
	if sizeBytes%workspace.MaxSyncBlockBytes != 0 {
		chunkCount++
	}
	if chunkCount == 0 {
		return added, nil
	}
	fullChunk, err := json.Marshal(workspace.Chunk{SHA256: strings.Repeat("a", 64), SizeBytes: workspace.MaxSyncBlockBytes})
	if err != nil {
		return 0, fmt.Errorf("encode legacy workspace block manifest: %w", err)
	}
	lastSize := sizeBytes - (chunkCount-1)*workspace.MaxSyncBlockBytes
	lastChunk, err := json.Marshal(workspace.Chunk{SHA256: strings.Repeat("a", 64), SizeBytes: lastSize})
	if err != nil {
		return 0, fmt.Errorf("encode legacy workspace block manifest: %w", err)
	}
	fullCount := chunkCount - 1
	if fullCount > (workspace.MaxSyncManifestBytes-added-int64(len(lastChunk)))/(int64(len(fullChunk))+1) {
		return 0, errors.New("legacy workspace manifest exceeds 16 MiB")
	}
	added += fullCount*(int64(len(fullChunk))+1) + int64(len(lastChunk))
	return added, nil
}

func verifyLegacyWorkspaceTree(ctx context.Context, volumeFS fs.FS, sources []legacySourceFile, quotaBytes int64) error {
	current, err := scanLegacyWorkspace(ctx, volumeFS, quotaBytes)
	if err != nil {
		return fmt.Errorf("recheck legacy workspace volume: %w", err)
	}
	if len(current) != len(sources) {
		return errors.New("legacy workspace volume changed during import")
	}
	for i := range sources {
		if current[i].path != sources[i].path || !sameLegacyFileInfo(current[i].info, sources[i].info) {
			return errors.New("legacy workspace volume changed during import")
		}
	}
	return nil
}
