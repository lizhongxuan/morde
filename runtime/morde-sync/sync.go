// Package mordesync provides the filesystem-only core shared by Morde clients.
// It deliberately contains no transport, persistence, or credential handling.
package mordesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	// BlockSize is the fixed chunk size used by manifests and block transfers.
	BlockSize = 4 << 20
	// Parallelism bounds active file/block reads and their buffers.
	Parallelism = 4
	// MaxManifestEntries caps files and explicit empty directories combined.
	MaxManifestEntries        = 100_000
	reservedTempPrefix        = ".morde-sync-tmp-"
	reservedRecoveryDirectory = ".morde-preserved"
)

var (
	ErrUnsafePath          = errors.New("unsafe workspace path")
	ErrUnsupportedFile     = errors.New("workspace contains a non-regular file")
	ErrHardLinkedFile      = errors.New("workspace contains a hard-linked regular file") // Deprecated: internal hard-link groups are represented in manifests.
	ErrHardlinkAnchor      = errors.New("workspace hard-link anchor is missing or incompatible")
	ErrUnsupportedMetadata = errors.New("workspace contains ACL or extended attribute metadata that sync cannot preserve")
	ErrRescanRequired      = errors.New("source changed; rescan required")
	ErrDestinationChange   = errors.New("destination changed during apply")
	ErrPathConflict        = errors.New("destination parent is not a directory")
	ErrInvalidManifest     = errors.New("invalid workspace manifest")
)

type Block struct {
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// File describes one regular file or Workspace-relative symlink.
type File struct {
	Path          string              `json:"path"`
	Kind          string              `json:"kind,omitempty"`
	SymlinkTarget string              `json:"symlink_target,omitempty"`
	HardlinkTo    string              `json:"hardlink_to,omitempty"`
	Xattrs        []ExtendedAttribute `json:"xattrs"`
	ACL           *NativeACL          `json:"acl,omitempty"`
	MetadataKnown bool                `json:"metadata_known,omitempty"`
	Size          int64               `json:"size"`
	Mode          uint32              `json:"mode"`
	ModTimeNS     int64               `json:"mod_time_ns"`
	ModTimeKnown  bool                `json:"mod_time_known,omitempty"`
	SHA256        string              `json:"sha256"`
	Blocks        []Block             `json:"blocks"`
}

const (
	FileKindRegular = "file"
	FileKindSymlink = "symlink"
)

// Directory captures ordinary directory metadata relative to the workspace
// root. Known flags keep older baselines explicitly unknown.
type Directory struct {
	Path          string              `json:"path"`
	Mode          uint32              `json:"mode"`
	ModeKnown     bool                `json:"mode_known,omitempty"`
	ModTimeNS     int64               `json:"mod_time_ns"`
	ModTimeKnown  bool                `json:"mod_time_known,omitempty"`
	Xattrs        []ExtendedAttribute `json:"xattrs"`
	ACL           *NativeACL          `json:"acl,omitempty"`
	MetadataKnown bool                `json:"metadata_known,omitempty"`
}

type ExtendedAttribute struct {
	Name  string `json:"name"`
	Value []byte `json:"value"`
}

// NativeACL carries an OS-native ACL payload. It is only restorable by the
// same platform and format; the sync core never translates ACL semantics.
type NativeACL struct {
	Platform string `json:"platform"`
	Format   string `json:"format"`
	Data     []byte `json:"data"`
}

type Manifest struct {
	FileCount        int64       `json:"file_count"`
	TotalBytes       int64       `json:"total_bytes"`
	Files            []File      `json:"files"`
	EmptyDirectories []string    `json:"empty_directories"`
	Directories      []Directory `json:"directories,omitempty"`
}

type ScanPhase string

const (
	PhaseInventory ScanPhase = "inventory"
	PhaseHash      ScanPhase = "hash"
)

// ScanProgress is a snapshot; callback invocations are serialized.
type ScanProgress struct {
	Phase       ScanPhase
	FilesFound  int64
	BytesFound  int64
	FilesHashed int64
	BytesHashed int64
	CurrentPath string
}

type BlockRef struct {
	File  File
	Index int
}

// BlockConsumer receives a borrowed buffer valid only until it returns.
type BlockConsumer func(file File, index int, block Block, data []byte) error

type ApplyStatus string

const (
	ApplyWritten  ApplyStatus = "written"
	ApplyPresent  ApplyStatus = "already_present"
	ApplyConflict ApplyStatus = "conflict"
)

type ApplyResult struct {
	Status         ApplyStatus `json:"status"`
	ExistingSHA256 string      `json:"existing_sha256,omitempty"`
}

type scannedFile struct {
	file File
	info os.FileInfo
}

type scannedDirectory struct {
	directory Directory
	info      os.FileInfo
}

var blockSlots = make(chan struct{}, Parallelism)
var operationSlots = make(chan struct{}, Parallelism)
var blockBuffers = make(chan []byte, Parallelism)

func acquireOperation(ctx context.Context) error {
	select {
	case operationSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func withBlockBuffer(ctx context.Context, use func([]byte) error) error {
	select {
	case blockSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-blockSlots }()
	var buffer []byte
	select {
	case buffer = <-blockBuffers:
	default:
		buffer = make([]byte, BlockSize)
	}
	defer func() { blockBuffers <- buffer }()
	return use(buffer)
}

// Scan inventories a directory and hashes regular file contents. Unsafe links,
// special files, unsupported ACL/xattr metadata, invalid paths, or changing
// entries fail closed.
func Scan(ctx context.Context, directory string, report func(ScanProgress)) (Manifest, error) {
	if ctx == nil {
		return Manifest{}, errors.New("context is required")
	}
	if err := acquireOperation(ctx); err != nil {
		return Manifest{}, err
	}
	defer func() { <-operationSlots }()
	root, err := openDirectoryRoot(directory)
	if err != nil {
		return Manifest{}, err
	}
	defer root.Close()

	var progressMu sync.Mutex
	progress := ScanProgress{Phase: PhaseInventory}
	reportProgress := func() {
		if report == nil {
			return
		}
		progressMu.Lock()
		current := progress
		report(current)
		progressMu.Unlock()
	}

	var files []scannedFile
	var emptyDirectories []string
	var directories []scannedDirectory
	if err := inventory(ctx, root, "", &files, &emptyDirectories, &directories, &progress, reportProgress); err != nil {
		return Manifest{}, err
	}
	sortScanned(files)
	if err := setHardlinkAnchors(files); err != nil {
		return Manifest{}, err
	}
	sort.Strings(emptyDirectories)
	sortScannedDirectories(directories)
	progressMu.Lock()
	progress.Phase = PhaseHash
	progressMu.Unlock()
	reportProgress()

	if len(files) == 0 {
		if err := verifyInventory(ctx, root, files, emptyDirectories, directories); err != nil {
			return Manifest{}, err
		}
		return Manifest{Files: []File{}, EmptyDirectories: emptyDirectories, Directories: directoryValues(directories)}, nil
	}
	results := make([]File, len(files))
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var firstErr error
	var errOnce sync.Once
	setErr := func(err error) {
		if err == nil {
			return
		}
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}
	workerCount := min(Parallelism, len(files))
	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range jobs {
				if workerCtx.Err() != nil {
					return
				}
				file, err := hashScannedFile(workerCtx, root, files[index], func(bytesHashed int64, completed bool) {
					progressMu.Lock()
					progress.BytesHashed += bytesHashed
					if completed {
						progress.FilesHashed++
					}
					progress.CurrentPath = files[index].file.Path
					current := progress
					if report != nil {
						report(current)
					}
					progressMu.Unlock()
				})
				if err != nil {
					setErr(err)
					return
				}
				results[index] = file
			}
		}()
	}
sendJobs:
	for index := range files {
		select {
		case jobs <- index:
		case <-workerCtx.Done():
			break sendJobs
		}
	}
	close(jobs)
	workers.Wait()
	if firstErr != nil {
		return Manifest{}, firstErr
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if err := verifyInventory(ctx, root, files, emptyDirectories, directories); err != nil {
		return Manifest{}, err
	}
	var totalBytes int64
	for _, file := range results {
		if file.Size > int64(^uint64(0)>>1)-totalBytes {
			return Manifest{}, errors.New("workspace size overflows int64")
		}
		totalBytes += file.Size
	}
	return Manifest{FileCount: int64(len(results)), TotalBytes: totalBytes, Files: results, EmptyDirectories: emptyDirectories, Directories: directoryValues(directories)}, nil
}

func inventory(ctx context.Context, root *os.Root, prefix string, files *[]scannedFile, emptyDirectories *[]string, directories *[]scannedDirectory, progress *ScanProgress, report func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open workspace directory: %w", err)
	}
	directoryInfo, err := directory.Stat()
	if err != nil || !directoryInfo.IsDir() {
		_ = directory.Close()
		return errors.Join(ErrRescanRequired, err)
	}
	xattrs, acl, err := captureMetadataFD(directory, displayMetadataPath(prefix))
	if err != nil {
		_ = directory.Close()
		return err
	}
	metadataPath := prefix
	if metadataPath == "" {
		metadataPath = "."
	}
	if len(*files)+len(*directories) >= MaxManifestEntries {
		_ = directory.Close()
		return fmt.Errorf("workspace manifest exceeds the %d item limit: %w", MaxManifestEntries, ErrInvalidManifest)
	}
	*directories = append(*directories, scannedDirectory{directory: Directory{
		Path: metadataPath, Mode: uint32(directoryInfo.Mode().Perm()), ModeKnown: true,
		ModTimeNS: directoryInfo.ModTime().UnixNano(), ModTimeKnown: true, Xattrs: xattrs, ACL: acl, MetadataKnown: true,
	}, info: directoryInfo})
	entries, err := directory.ReadDir(-1)
	_ = directory.Close()
	if err != nil {
		return fmt.Errorf("read workspace directory: %w", err)
	}
	visibleEntries := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		rel := name
		if prefix != "" {
			rel = path.Join(prefix, name)
		}
		info, err := root.Lstat(name)
		if err != nil {
			return fmt.Errorf("stat %q: %w", rel, ErrRescanRequired)
		}
		if strings.HasPrefix(name, reservedTempPrefix) {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("reserved staging path %q is not a regular file: %w", rel, ErrUnsafePath)
			}
			continue
		}
		if name == reservedRecoveryDirectory && info.IsDir() {
			continue
		}
		if err := validRelativePath(rel); err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			xattrs, acl, err := captureSymlinkMetadata(root, name, rel)
			if err != nil {
				return err
			}
			if len(*files)+len(*directories) >= MaxManifestEntries {
				return fmt.Errorf("workspace manifest exceeds the %d item limit: %w", MaxManifestEntries, ErrInvalidManifest)
			}
			target, err := readSymlink(root, name, info)
			if err != nil {
				return fmt.Errorf("read symlink %q: %w", rel, err)
			}
			if err := ValidateSymlinkTarget(rel, target); err != nil {
				return err
			}
			targetBytes := []byte(target)
			if int64(len(targetBytes)) > int64(^uint64(0)>>1)-progress.BytesFound {
				return errors.New("workspace size overflows int64")
			}
			visibleEntries++
			*files = append(*files, scannedFile{file: File{
				Path: rel, Kind: FileKindSymlink, SymlinkTarget: target, Size: int64(len(targetBytes)), Mode: 0,
				ModTimeNS: info.ModTime().UnixNano(), ModTimeKnown: true, SHA256: sumHex(targetBytes), Xattrs: xattrs, ACL: acl, MetadataKnown: true,
			}, info: info})
			progress.FilesFound++
			progress.BytesFound += int64(len(targetBytes))
			progress.CurrentPath = rel
			report()
			continue
		}
		if info.IsDir() {
			visibleEntries++
			if len(*files)+len(*directories) >= MaxManifestEntries {
				return fmt.Errorf("workspace manifest exceeds the %d item limit: %w", MaxManifestEntries, ErrInvalidManifest)
			}
			child, err := root.OpenRoot(name)
			if err != nil {
				return fmt.Errorf("open directory %q: %w", rel, ErrRescanRequired)
			}
			openedInfo, statErr := child.Stat(".")
			if statErr != nil || !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
				_ = child.Close()
				return fmt.Errorf("directory %q changed: %w", rel, ErrRescanRequired)
			}
			childDirectory, openErr := child.Open(".")
			if openErr != nil {
				_ = child.Close()
				return fmt.Errorf("open directory %q metadata: %w", rel, ErrRescanRequired)
			}
			metadataErr := func() error {
				_, _, metadataErr := captureMetadataFD(childDirectory, rel)
				return metadataErr
			}()
			_ = childDirectory.Close()
			if metadataErr != nil {
				_ = child.Close()
				return metadataErr
			}
			err = inventory(ctx, child, rel, files, emptyDirectories, directories, progress, report)
			_ = child.Close()
			if err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%q: %w", rel, ErrUnsupportedFile)
		}
		xattrs, acl, err := captureRegularFileMetadata(root, name, info, rel)
		if err != nil {
			return err
		}
		if len(*files)+len(*directories) >= MaxManifestEntries {
			return fmt.Errorf("workspace manifest exceeds the %d item limit: %w", MaxManifestEntries, ErrInvalidManifest)
		}
		visibleEntries++
		if info.Size() < 0 || info.Size() > int64(^uint64(0)>>1)-progress.BytesFound {
			return errors.New("workspace size overflows int64")
		}
		*files = append(*files, scannedFile{file: File{
			Path: rel, Kind: FileKindRegular, Size: info.Size(), Mode: uint32(info.Mode().Perm()), ModTimeNS: info.ModTime().UnixNano(), ModTimeKnown: true,
			Xattrs: xattrs, ACL: acl, MetadataKnown: true,
		}, info: info})
		progress.FilesFound++
		progress.BytesFound += info.Size()
		progress.CurrentPath = rel
		report()
	}
	if prefix != "" && visibleEntries == 0 {
		*emptyDirectories = append(*emptyDirectories, prefix)
	}
	return nil
}

func hashScannedFile(ctx context.Context, root *os.Root, input scannedFile, progress func(int64, bool)) (File, error) {
	file := input.file
	if file.Kind == FileKindSymlink {
		if err := ctx.Err(); err != nil {
			return File{}, err
		}
		target, info, err := readSymlinkPath(root, file.Path)
		if err != nil || target != file.SymlinkTarget || !sameSnapshot(input.info, file, info) || sumHex([]byte(target)) != file.SHA256 {
			return File{}, fmt.Errorf("symlink %q changed during scan: %w", file.Path, ErrRescanRequired)
		}
		xattrs, acl, err := captureSymlinkMetadataPath(root, file.Path, file.Path)
		if err != nil || !sameMetadata(file.Xattrs, file.ACL, xattrs, acl) {
			return File{}, errors.Join(fmt.Errorf("symlink %q metadata changed during scan: %w", file.Path, ErrRescanRequired), err)
		}
		progress(file.Size, true)
		return file, nil
	}
	var blocks []Block
	var fileHash [32]byte
	err := withBlockBuffer(ctx, func(buffer []byte) error {
		opened, err := openFile(root, file.Path, &file, input.info)
		if err != nil {
			return err
		}
		defer opened.Close()
		xattrs, acl, err := captureMetadataFD(opened, file.Path)
		if err != nil || !sameMetadata(file.Xattrs, file.ACL, xattrs, acl) {
			return errors.Join(fmt.Errorf("file %q metadata changed during scan: %w", file.Path, ErrRescanRequired), err)
		}
		hasher := newSHA256()
		remaining, offset := file.Size, int64(0)
		for remaining > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			blockSize := min(int64(BlockSize), remaining)
			chunk := buffer[:int(blockSize)]
			n, readErr := io.ReadFull(opened, chunk)
			if readErr != nil || n != len(chunk) {
				return fmt.Errorf("read %q: %w", file.Path, ErrRescanRequired)
			}
			_, _ = hasher.Write(chunk)
			blocks = append(blocks, Block{Offset: offset, Size: blockSize, SHA256: sumHex(chunk)})
			offset += blockSize
			remaining -= blockSize
			progress(blockSize, false)
		}
		var extra [1]byte
		n, readErr := io.ReadFull(opened, extra[:])
		if n != 0 || !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("file %q grew during scan: %w", file.Path, ErrRescanRequired)
		}
		openedInfo, statErr := opened.Stat()
		if statErr != nil || !sameSnapshot(input.info, file, openedInfo) {
			return fmt.Errorf("file %q changed during scan: %w", file.Path, ErrRescanRequired)
		}
		xattrs, acl, err = captureMetadataFD(opened, file.Path)
		if err != nil || !sameMetadata(file.Xattrs, file.ACL, xattrs, acl) {
			return errors.Join(fmt.Errorf("file %q metadata changed during scan: %w", file.Path, ErrRescanRequired), err)
		}
		current, statErr := lstatPath(root, file.Path)
		if statErr != nil || !os.SameFile(input.info, current) || !sameSnapshot(input.info, file, current) {
			return fmt.Errorf("file %q changed during scan: %w", file.Path, ErrRescanRequired)
		}
		copy(fileHash[:], hasher.Sum(nil))
		return nil
	})
	if err != nil {
		return File{}, err
	}
	file.Blocks = blocks
	file.SHA256 = encodeHex(fileHash[:])
	progress(0, true)
	return file, nil
}

func sortScanned(files []scannedFile) {
	sort.Slice(files, func(i, j int) bool { return files[i].file.Path < files[j].file.Path })
}

func verifyInventory(ctx context.Context, root *os.Root, original []scannedFile, originalEmptyDirectories []string, originalDirectories []scannedDirectory) error {
	current := make([]scannedFile, 0, len(original))
	currentEmptyDirectories := make([]string, 0, len(originalEmptyDirectories))
	currentDirectories := make([]scannedDirectory, 0, len(originalDirectories))
	progress := ScanProgress{Phase: PhaseInventory}
	if err := inventory(ctx, root, "", &current, &currentEmptyDirectories, &currentDirectories, &progress, func() {}); err != nil {
		return err
	}
	sortScanned(current)
	if err := setHardlinkAnchors(current); err != nil {
		return err
	}
	sort.Strings(currentEmptyDirectories)
	sortScannedDirectories(currentDirectories)
	if len(current) != len(original) || !slices.Equal(currentEmptyDirectories, originalEmptyDirectories) || len(currentDirectories) != len(originalDirectories) {
		return ErrRescanRequired
	}
	for index := range original {
		before := original[index]
		after := current[index]
		if before.file.Path != after.file.Path || before.file.Kind != after.file.Kind || before.file.SymlinkTarget != after.file.SymlinkTarget ||
			before.file.HardlinkTo != after.file.HardlinkTo ||
			before.file.MetadataKnown != after.file.MetadataKnown ||
			!sameMetadata(before.file.Xattrs, before.file.ACL, after.file.Xattrs, after.file.ACL) ||
			!sameSnapshot(before.info, before.file, after.info) {
			return fmt.Errorf("file %q changed during scan: %w", before.file.Path, ErrRescanRequired)
		}
	}
	for index := range originalDirectories {
		before, after := originalDirectories[index], currentDirectories[index]
		if before.directory.Path != after.directory.Path || !os.SameFile(before.info, after.info) ||
			before.directory.Mode != after.directory.Mode || before.directory.ModTimeNS != after.directory.ModTimeNS ||
			before.directory.MetadataKnown != after.directory.MetadataKnown ||
			!sameMetadata(before.directory.Xattrs, before.directory.ACL, after.directory.Xattrs, after.directory.ACL) {
			return fmt.Errorf("directory %q changed during scan: %w", before.directory.Path, ErrRescanRequired)
		}
	}
	return ctx.Err()
}

func sortScannedDirectories(values []scannedDirectory) {
	sort.Slice(values, func(i, j int) bool { return values[i].directory.Path < values[j].directory.Path })
}

func directoryValues(values []scannedDirectory) []Directory {
	result := make([]Directory, 0, len(values))
	for _, value := range values {
		result = append(result, value.directory)
	}
	return result
}

func min[T ~int | ~int64](a, b T) T {
	if a < b {
		return a
	}
	return b
}

func newSHA256() hash.Hash { return sha256.New() }

func sumHex(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}

func encodeHex(value []byte) string { return hex.EncodeToString(value) }

func openDirectoryRoot(directory string) (*os.Root, error) {
	if directory == "" || strings.IndexByte(directory, 0) >= 0 {
		return nil, errors.New("workspace directory is invalid")
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace directory: %w", err)
	}
	abs = filepath.Clean(abs)
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("workspace root must be a real directory")
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	openedInfo, err := root.Stat(".")
	if err != nil || !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
		_ = root.Close()
		return nil, errors.New("workspace root changed while opening")
	}
	return root, nil
}

func validRelativePath(value string) error {
	if value == "." || !fs.ValidPath(value) || path.IsAbs(value) || path.Clean(value) != value || strings.ContainsRune(value, '\\') {
		return fmt.Errorf("%q: %w", value, ErrUnsafePath)
	}
	for _, part := range strings.Split(value, "/") {
		if part == reservedRecoveryDirectory || strings.HasPrefix(part, reservedTempPrefix) || strings.ContainsAny(part, `<>:"|?*`) || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return fmt.Errorf("%q: %w", value, ErrUnsafePath)
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
			len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
			return fmt.Errorf("%q: %w", value, ErrUnsafePath)
		}
	}
	return nil
}

func sameSnapshot(original os.FileInfo, expected File, actual os.FileInfo) bool {
	if original == nil || actual == nil || !os.SameFile(original, actual) || actual.Size() != expected.Size ||
		(expected.ModTimeKnown && actual.ModTime().UnixNano() != expected.ModTimeNS) {
		return false
	}
	if expected.Kind == FileKindSymlink {
		return actual.Mode()&os.ModeSymlink != 0 && expected.Mode == 0
	}
	return (expected.Kind == "" || expected.Kind == FileKindRegular) && actual.Mode().IsRegular() && uint32(actual.Mode().Perm()) == expected.Mode
}

func readSymlink(root *os.Root, name string, expected os.FileInfo) (string, error) {
	info, err := root.Lstat(name)
	if err != nil || expected == nil || info.Mode()&os.ModeSymlink == 0 || !os.SameFile(expected, info) {
		return "", ErrRescanRequired
	}
	target, err := root.Readlink(name)
	if err != nil {
		return "", err
	}
	latest, err := root.Lstat(name)
	if err != nil || !os.SameFile(info, latest) || latest.Mode()&os.ModeSymlink == 0 || latest.Size() != info.Size() || latest.ModTime() != info.ModTime() {
		return "", ErrRescanRequired
	}
	return target, nil
}

func readSymlinkPath(root *os.Root, relative string) (string, os.FileInfo, error) {
	if err := validRelativePath(relative); err != nil {
		return "", nil, err
	}
	parent, name, closeParent, err := openParent(root, relative, false)
	if err != nil {
		return "", nil, err
	}
	defer closeParent()
	info, err := parent.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", nil, ErrRescanRequired
	}
	target, err := parent.Readlink(name)
	if err != nil {
		return "", nil, err
	}
	latest, err := parent.Lstat(name)
	if err != nil || !os.SameFile(info, latest) || latest.Mode()&os.ModeSymlink == 0 || latest.Size() != info.Size() || latest.ModTime() != info.ModTime() {
		return "", nil, ErrRescanRequired
	}
	if err := ValidateSymlinkTarget(relative, target); err != nil {
		return "", nil, err
	}
	return target, latest, nil
}

// ValidateSymlinkTarget validates a link string without resolving or following it.
func ValidateSymlinkTarget(linkPath, target string) error {
	if validRelativePath(linkPath) != nil || target == "" || !utf8.ValidString(target) ||
		len(target) > 4096 || strings.IndexByte(target, 0) >= 0 || strings.ContainsAny(target, `\\:`) || path.IsAbs(target) {
		return fmt.Errorf("%q: %w", linkPath, ErrUnsafePath)
	}
	resolved := path.Clean(path.Join(path.Dir(linkPath), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") || path.IsAbs(resolved) ||
		(resolved != "." && validRelativePath(resolved) != nil) {
		return fmt.Errorf("%q: %w", linkPath, ErrUnsafePath)
	}
	return nil
}

func openFile(root *os.Root, relative string, expected *File, original os.FileInfo) (*os.File, error) {
	if err := validRelativePath(relative); err != nil {
		return nil, err
	}
	parent, name, closeParent, err := openParent(root, relative, false)
	if err != nil {
		return nil, fmt.Errorf("open parent for %q: %w", relative, err)
	}
	defer closeParent()
	info, err := parent.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("file %q is unavailable: %w", relative, ErrRescanRequired)
	}
	if expected != nil && !sameSnapshot(original, *expected, info) {
		return nil, fmt.Errorf("file %q changed: %w", relative, ErrRescanRequired)
	}
	file, err := parent.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open file %q: %w", relative, ErrRescanRequired)
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) ||
		(expected != nil && !sameSnapshot(original, *expected, openedInfo)) {
		_ = file.Close()
		return nil, fmt.Errorf("file %q changed while opening: %w", relative, ErrRescanRequired)
	}
	return file, nil
}

func lstatPath(root *os.Root, relative string) (os.FileInfo, error) {
	if err := validRelativePath(relative); err != nil {
		return nil, err
	}
	parent, name, closeParent, err := openParent(root, relative, false)
	if err != nil {
		return nil, err
	}
	defer closeParent()
	return parent.Lstat(name)
}

func openParent(root *os.Root, relative string, create bool) (*os.Root, string, func(), error) {
	parts := strings.Split(relative, "/")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return nil, "", func() {}, ErrUnsafePath
	}
	current := root
	owned := make([]*os.Root, 0, len(parts)-1)
	closeOwned := func() {
		for index := len(owned) - 1; index >= 0; index-- {
			_ = owned[index].Close()
		}
	}
	for _, part := range parts[:len(parts)-1] {
		info, err := current.Lstat(part)
		if errors.Is(err, fs.ErrNotExist) && create {
			mkdirErr := current.Mkdir(part, 0o755)
			if mkdirErr != nil && !errors.Is(mkdirErr, fs.ErrExist) {
				closeOwned()
				return nil, "", func() {}, mkdirErr
			}
			if mkdirErr == nil {
				if syncErr := syncDirectory(current); syncErr != nil {
					closeOwned()
					return nil, "", func() {}, syncErr
				}
			}
			info, err = current.Lstat(part)
		}
		if err != nil {
			closeOwned()
			return nil, "", func() {}, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			closeOwned()
			return nil, "", func() {}, ErrUnsafePath
		}
		if !info.IsDir() {
			closeOwned()
			return nil, "", func() {}, ErrPathConflict
		}
		child, err := current.OpenRoot(part)
		if err != nil {
			closeOwned()
			return nil, "", func() {}, ErrUnsafePath
		}
		childInfo, err := child.Stat(".")
		if err != nil || !childInfo.IsDir() || !os.SameFile(info, childInfo) {
			_ = child.Close()
			closeOwned()
			return nil, "", func() {}, ErrUnsafePath
		}
		owned = append(owned, child)
		current = child
	}
	return current, parts[len(parts)-1], closeOwned, nil
}
