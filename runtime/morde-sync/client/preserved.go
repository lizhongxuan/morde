package client

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	mordesync "morde-sync"
)

const preservedDirectory = ".morde-preserved"

var preservedIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

var ErrPreservedDestinationExists = errors.New("restore destination already exists")

// PreservedFile describes a local-only copy made before live sync replaces or deletes a file.
type PreservedFile struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Path        string    `json:"path"`
	CreatedAt   time.Time `json:"created_at"`
	Size        int64     `json:"size"`
}

type preservedFileMetadata struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Path        string    `json:"path"`
	CreatedAt   time.Time `json:"created_at"`
}

func preserveClientFile(ctx context.Context, workspaceRoot *os.Root, sourceParent *os.Root, directory, workspaceID, name string,
	expectedInfo os.FileInfo, expected mordesync.File) (PreservedFile, string, error) {
	if ctx == nil || workspaceRoot == nil || sourceParent == nil || !workspaceIDPattern.MatchString(workspaceID) ||
		!validPreservedPath(expected.Path) || name != filepath.Base(filepath.FromSlash(expected.Path)) || expected.Size < 0 || !digestPattern.MatchString(expected.SHA256) {
		return PreservedFile{}, "", errors.New("invalid Workspace preservation request")
	}
	currentInfo, currentDigest, currentTarget, exists, err := inspectClientFile(ctx, sourceParent, name, expected.Path)
	if err != nil || !exists || expectedInfo == nil || !os.SameFile(expectedInfo, currentInfo) || !sameClientFile(currentInfo, currentDigest, currentTarget, expected) {
		return PreservedFile{}, "", mordesync.ErrDestinationChange
	}
	if err := ctx.Err(); err != nil {
		return PreservedFile{}, "", err
	}
	directory, err = canonicalDirectory(directory)
	if err != nil {
		return PreservedFile{}, "", fmt.Errorf("resolve Workspace preservation root: %w", err)
	}
	parentRoot, archiveRoot, err := openPreservedRoots(workspaceRoot, directory, workspaceID, true)
	if err != nil {
		return PreservedFile{}, "", err
	}
	defer parentRoot.Close()
	defer archiveRoot.Close()

	for attempt := 0; attempt < 3; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return PreservedFile{}, "", fmt.Errorf("generate preservation ID: %w", err)
		}
		id := hex.EncodeToString(random[:])
		if err := archiveRoot.Mkdir(id, 0o700); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return PreservedFile{}, "", fmt.Errorf("create Workspace preservation entry: %w", err)
		}
		entryRoot, err := archiveRoot.OpenRoot(id)
		if err != nil {
			_ = archiveRoot.Remove(id)
			return PreservedFile{}, "", fmt.Errorf("open Workspace preservation entry: %w", err)
		}
		created := time.Now().UTC()
		metadata := preservedFileMetadata{ID: id, WorkspaceID: workspaceID, Path: expected.Path, CreatedAt: created}
		metadataFile, err := entryRoot.OpenFile("metadata.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			err = json.NewEncoder(metadataFile).Encode(metadata)
		}
		if err == nil {
			err = metadataFile.Sync()
		}
		if metadataFile != nil {
			if closeErr := metadataFile.Close(); err == nil {
				err = closeErr
			}
		}
		if err == nil {
			err = syncClientDirectory(entryRoot)
		}
		if err == nil {
			err = syncClientDirectory(archiveRoot)
		}
		if err != nil {
			_ = entryRoot.Close()
			_ = removeIncompletePreservedEntry(archiveRoot, id)
			return PreservedFile{}, "", fmt.Errorf("save Workspace preservation metadata: %w", err)
		}

		archivePath := filepath.Join(preservedDirectory, workspaceID, id, "content")
		sourcePath := filepath.Join(filepath.Base(directory), filepath.FromSlash(expected.Path))
		if err := ctx.Err(); err != nil {
			_ = entryRoot.Close()
			_ = removeIncompletePreservedEntry(archiveRoot, id)
			return PreservedFile{}, "", err
		}
		if err := parentRoot.Rename(sourcePath, archivePath); err != nil {
			_ = entryRoot.Close()
			_ = removeIncompletePreservedEntry(archiveRoot, id)
			return PreservedFile{}, "", fmt.Errorf("preserve local file before sync: %w", err)
		}
		record := PreservedFile{ID: id, WorkspaceID: workspaceID, Path: expected.Path, CreatedAt: created, Size: expected.Size}
		if err := syncClientDirectory(sourceParent); err != nil {
			_ = entryRoot.Close()
			return record, archivePath, fmt.Errorf("sync local directory after preserving %q: %w", expected.Path, err)
		}
		if err := syncClientDirectory(entryRoot); err != nil {
			_ = entryRoot.Close()
			return record, archivePath, fmt.Errorf("sync local preservation entry for %q: %w", expected.Path, err)
		}
		_ = entryRoot.Close()

		info, digest, target, exists, err := inspectClientFile(ctx, parentRoot, archivePath, expected.Path)
		if err == nil && exists && sameClientFile(info, digest, target, expected) && os.SameFile(expectedInfo, info) {
			return record, archivePath, nil
		}
		cause := err
		if cause == nil {
			cause = mordesync.ErrDestinationChange
		}
		if restoreErr := parentRoot.Link(archivePath, sourcePath); restoreErr != nil {
			return record, archivePath, fmt.Errorf("local file changed while it was preserved; recovery copy remains at %q: %v: %w", archivePath, cause, mordesync.ErrDestinationChange)
		}
		_ = syncClientDirectory(sourceParent)
		return record, archivePath, fmt.Errorf("local file changed while it was preserved; recovery copy remains at %q: %v: %w", archivePath, cause, mordesync.ErrDestinationChange)
	}
	return PreservedFile{}, "", errors.New("could not allocate a unique Workspace preservation ID")
}

func openPreservedRoots(workspaceRoot *os.Root, directory, workspaceID string, create bool) (*os.Root, *os.Root, error) {
	if workspaceRoot == nil || !workspaceIDPattern.MatchString(workspaceID) {
		return nil, nil, errors.New("invalid Workspace preservation root")
	}
	parentRoot, err := os.OpenRoot(filepath.Dir(directory))
	if err != nil {
		return nil, nil, fmt.Errorf("open parent of Workspace directory: %w", err)
	}
	workspaceInfo, err := workspaceRoot.Stat(".")
	if err != nil {
		_ = parentRoot.Close()
		return nil, nil, fmt.Errorf("inspect Workspace directory: %w", err)
	}
	currentInfo, err := parentRoot.Lstat(filepath.Base(directory))
	if err != nil || !os.SameFile(workspaceInfo, currentInfo) {
		_ = parentRoot.Close()
		return nil, nil, mordesync.ErrDestinationChange
	}
	preservedRoot, err := openOrCreateClientRoot(parentRoot, preservedDirectory, create)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !create {
			_ = parentRoot.Close()
			return nil, nil, nil
		}
		_ = parentRoot.Close()
		return nil, nil, fmt.Errorf("open local preservation directory: %w", err)
	}
	workspaceArchiveRoot, err := openOrCreateClientRoot(preservedRoot, workspaceID, create)
	_ = preservedRoot.Close()
	if err != nil {
		_ = parentRoot.Close()
		if errors.Is(err, fs.ErrNotExist) && !create {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("open Workspace preservation directory: %w", err)
	}
	return parentRoot, workspaceArchiveRoot, nil
}

func openOrCreateClientRoot(parent *os.Root, name string, create bool) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) && create {
		if err := parent.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		info, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("preservation path is not a real directory")
	}
	if create {
		if err := parent.Chmod(name, 0o700); err != nil {
			return nil, err
		}
		updatedInfo, statErr := parent.Lstat(name)
		if statErr != nil || updatedInfo.Mode()&os.ModeSymlink != 0 || !updatedInfo.IsDir() || !os.SameFile(info, updatedInfo) {
			return nil, errors.New("preservation directory changed while setting permissions")
		}
		info = updatedInfo
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	openedInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		_ = root.Close()
		return nil, errors.New("preservation directory changed while opening")
	}
	return root, nil
}

func removeIncompletePreservedEntry(parent *os.Root, id string) error {
	entry, err := parent.OpenRoot(id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = entry.Remove("metadata.json")
	_ = entry.Close()
	return parent.Remove(id)
}

func validPreservedPath(value string) bool {
	return value != "" && fs.ValidPath(value) && path.Clean(value) == value && !strings.ContainsRune(value, '\\')
}

func readPreservedMetadata(entry *os.Root, id, workspaceID string) (preservedFileMetadata, error) {
	file, err := entry.Open("metadata.json")
	if err != nil {
		return preservedFileMetadata{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return preservedFileMetadata{}, errors.New("preserved metadata file is invalid")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 16<<10))
	decoder.DisallowUnknownFields()
	var metadata preservedFileMetadata
	if err := decoder.Decode(&metadata); err != nil {
		return preservedFileMetadata{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return preservedFileMetadata{}, errors.New("preserved metadata has trailing content")
	}
	if metadata.ID != id || metadata.WorkspaceID != workspaceID || !validPreservedPath(metadata.Path) || metadata.CreatedAt.IsZero() {
		return preservedFileMetadata{}, errors.New("preserved metadata is invalid")
	}
	return metadata, nil
}

// ListPreserved returns recovery copies stored outside the selected Workspace directory.
func ListPreserved(ctx context.Context, directory, workspaceID string) ([]PreservedFile, error) {
	if ctx == nil || !workspaceIDPattern.MatchString(workspaceID) {
		return nil, errors.New("context and valid Workspace ID are required")
	}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		return nil, err
	}
	releaseOperation, err := acquireOperationLock(ctx, canonical)
	if err != nil {
		return nil, err
	}
	defer releaseOperation()
	workspaceRoot, err := openClientWorkspaceRoot(canonical)
	if err != nil {
		return nil, err
	}
	defer workspaceRoot.Close()
	parentRoot, archiveRoot, err := openPreservedRoots(workspaceRoot, canonical, workspaceID, false)
	if err != nil {
		return nil, err
	}
	if parentRoot != nil {
		defer parentRoot.Close()
	}
	if archiveRoot == nil {
		return []PreservedFile{}, nil
	}
	defer archiveRoot.Close()
	dir, err := archiveRoot.Open(".")
	if err != nil {
		return nil, err
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return nil, err
	}
	result := make([]PreservedFile, 0, len(entries))
	for _, entry := range entries {
		if !preservedIDPattern.MatchString(entry.Name()) {
			continue
		}
		entryInfo, err := archiveRoot.Lstat(entry.Name())
		if err != nil {
			return nil, err
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.IsDir() {
			return nil, errors.New("preserved Workspace entry is not a real directory")
		}
		entryRoot, err := archiveRoot.OpenRoot(entry.Name())
		if err != nil {
			return nil, err
		}
		openedInfo, err := entryRoot.Stat(".")
		if err != nil || !os.SameFile(entryInfo, openedInfo) {
			_ = entryRoot.Close()
			return nil, errors.New("preserved Workspace entry changed while opening")
		}
		metadata, metadataErr := readPreservedMetadata(entryRoot, entry.Name(), workspaceID)
		info, statErr := entryRoot.Lstat("content")
		if errors.Is(statErr, fs.ErrNotExist) {
			_ = entryRoot.Close()
			continue
		}
		if metadataErr != nil {
			_ = entryRoot.Close()
			return nil, fmt.Errorf("read preserved Workspace metadata: %w", metadataErr)
		}
		if statErr != nil {
			_ = entryRoot.Close()
			return nil, errors.New("preserved Workspace content is unavailable")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, readErr := entryRoot.Readlink("content")
			latest, latestErr := entryRoot.Lstat("content")
			_ = entryRoot.Close()
			if readErr != nil || latestErr != nil || !os.SameFile(info, latest) || latest.Mode()&os.ModeSymlink == 0 ||
				mordesync.ValidateSymlinkTarget(metadata.Path, target) != nil {
				return nil, errors.Join(errors.New("preserved Workspace symlink changed"), readErr, latestErr)
			}
			result = append(result, PreservedFile{ID: metadata.ID, WorkspaceID: metadata.WorkspaceID,
				Path: metadata.Path, CreatedAt: metadata.CreatedAt, Size: int64(len([]byte(target)))})
			continue
		}
		_ = entryRoot.Close()
		if !info.Mode().IsRegular() {
			return nil, errors.New("preserved Workspace content is not a regular file")
		}
		result = append(result, PreservedFile{ID: metadata.ID, WorkspaceID: metadata.WorkspaceID,
			Path: metadata.Path, CreatedAt: metadata.CreatedAt, Size: info.Size()})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result, nil
}

// DeletePreserved removes one verified recovery copy from the local recovery area.
// Callers must obtain explicit user confirmation before invoking this operation.
func DeletePreserved(ctx context.Context, directory, workspaceID, id string) error {
	if ctx == nil || !workspaceIDPattern.MatchString(workspaceID) || !preservedIDPattern.MatchString(id) {
		return errors.New("context, valid Workspace ID, and preservation ID are required")
	}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		return err
	}
	releaseOperation, err := acquireOperationLock(ctx, canonical)
	if err != nil {
		return err
	}
	defer releaseOperation()
	workspaceRoot, err := openClientWorkspaceRoot(canonical)
	if err != nil {
		return err
	}
	defer workspaceRoot.Close()
	parentRoot, archiveRoot, err := openPreservedRoots(workspaceRoot, canonical, workspaceID, false)
	if err != nil {
		return err
	}
	if parentRoot != nil {
		defer parentRoot.Close()
	}
	if archiveRoot == nil {
		return fs.ErrNotExist
	}
	defer archiveRoot.Close()

	entryInfo, err := archiveRoot.Lstat(id)
	if err != nil {
		return err
	}
	if entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.IsDir() {
		return errors.New("preserved Workspace entry is not a real directory")
	}
	entryRoot, err := archiveRoot.OpenRoot(id)
	if err != nil {
		return err
	}
	defer entryRoot.Close()
	openedInfo, err := entryRoot.Stat(".")
	if err != nil || !os.SameFile(entryInfo, openedInfo) {
		return errors.New("preserved Workspace entry changed while opening")
	}
	metadata, err := readPreservedMetadata(entryRoot, id, workspaceID)
	if err != nil {
		return fmt.Errorf("read preserved Workspace metadata: %w", err)
	}
	contentInfo, err := entryRoot.Lstat("content")
	if err != nil {
		return errors.New("preserved Workspace content is unavailable")
	}
	if contentInfo.Mode()&os.ModeSymlink != 0 {
		target, readErr := entryRoot.Readlink("content")
		latest, latestErr := entryRoot.Lstat("content")
		if readErr != nil || latestErr != nil || !os.SameFile(contentInfo, latest) || latest.Mode()&os.ModeSymlink == 0 ||
			mordesync.ValidateSymlinkTarget(metadata.Path, target) != nil {
			return errors.Join(errors.New("preserved Workspace symlink is unsafe or changed"), readErr, latestErr)
		}
	} else if !contentInfo.Mode().IsRegular() {
		return errors.New("preserved Workspace content is unavailable")
	}
	entryDir, err := entryRoot.Open(".")
	if err != nil {
		return err
	}
	entries, err := entryDir.ReadDir(-1)
	_ = entryDir.Close()
	if err != nil {
		return err
	}
	if len(entries) != 2 || entries[0].Name() == entries[1].Name() ||
		!((entries[0].Name() == "content" && entries[1].Name() == "metadata.json") ||
			(entries[0].Name() == "metadata.json" && entries[1].Name() == "content")) {
		return errors.New("preserved Workspace entry contains unexpected files")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	currentEntryInfo, err := archiveRoot.Lstat(id)
	if err != nil || !os.SameFile(entryInfo, currentEntryInfo) {
		return mordesync.ErrDestinationChange
	}
	currentContentInfo, err := entryRoot.Lstat("content")
	if err != nil || !os.SameFile(contentInfo, currentContentInfo) || contentInfo.Mode() != currentContentInfo.Mode() {
		return mordesync.ErrDestinationChange
	}
	if _, err := readPreservedMetadata(entryRoot, id, workspaceID); err != nil {
		return mordesync.ErrDestinationChange
	}
	if err := archiveRoot.RemoveAll(id); err != nil {
		return fmt.Errorf("remove preserved Workspace entry: %w", err)
	}
	if err := syncClientDirectory(archiveRoot); err != nil {
		return fmt.Errorf("sync Workspace preservation directory after deletion: %w", err)
	}
	return nil
}

// hasPreservedBaseline reports whether the local recovery area contains the
// exact file that was displaced for a file-to-directory live transition.
// It intentionally does not take the workspace operation lock; callers run
// inside the sync lock already.
func hasPreservedBaseline(ctx context.Context, directory, workspaceID string, expected mordesync.File) (bool, error) {
	if ctx == nil || !workspaceIDPattern.MatchString(workspaceID) || expected.Path == "" || expected.Size < 0 || !digestPattern.MatchString(expected.SHA256) {
		return false, errors.New("invalid preserved baseline lookup")
	}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		return false, err
	}
	workspaceRoot, err := openClientWorkspaceRoot(canonical)
	if err != nil {
		return false, err
	}
	defer workspaceRoot.Close()
	parentRoot, archiveRoot, err := openPreservedRoots(workspaceRoot, canonical, workspaceID, false)
	if err != nil {
		return false, err
	}
	if parentRoot != nil {
		defer parentRoot.Close()
	}
	if archiveRoot == nil {
		return false, nil
	}
	defer archiveRoot.Close()
	directoryFile, err := archiveRoot.Open(".")
	if err != nil {
		return false, err
	}
	entries, err := directoryFile.ReadDir(-1)
	_ = directoryFile.Close()
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !preservedIDPattern.MatchString(entry.Name()) {
			continue
		}
		info, err := archiveRoot.Lstat(entry.Name())
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false, errors.New("preserved Workspace entry is not a real directory")
		}
		entryRoot, err := archiveRoot.OpenRoot(entry.Name())
		if err != nil {
			return false, err
		}
		openedInfo, err := entryRoot.Stat(".")
		if err != nil || !os.SameFile(info, openedInfo) {
			_ = entryRoot.Close()
			return false, errors.New("preserved Workspace entry changed while opening")
		}
		metadata, err := readPreservedMetadata(entryRoot, entry.Name(), workspaceID)
		if err != nil {
			_ = entryRoot.Close()
			return false, err
		}
		if metadata.Path != expected.Path {
			_ = entryRoot.Close()
			continue
		}
		contentInfo, digest, target, exists, err := inspectClientFile(ctx, entryRoot, "content", expected.Path)
		_ = entryRoot.Close()
		if err != nil {
			return false, err
		}
		if exists && sameClientFile(contentInfo, digest, target, expected) {
			return true, nil
		}
	}
	return false, nil
}

// RestorePreserved copies a recovery file back only when its original path is absent. The archive remains until explicitly deleted.
func RestorePreserved(ctx context.Context, directory, workspaceID, id string) error {
	if ctx == nil || !workspaceIDPattern.MatchString(workspaceID) || !preservedIDPattern.MatchString(id) {
		return errors.New("context, valid Workspace ID, and preservation ID are required")
	}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		return err
	}
	releaseOperation, err := acquireOperationLock(ctx, canonical)
	if err != nil {
		return err
	}
	defer releaseOperation()
	workspaceRoot, err := openClientWorkspaceRoot(canonical)
	if err != nil {
		return err
	}
	defer workspaceRoot.Close()
	parentRoot, archiveRoot, err := openPreservedRoots(workspaceRoot, canonical, workspaceID, false)
	if err != nil {
		return err
	}
	if parentRoot != nil {
		defer parentRoot.Close()
	}
	if archiveRoot == nil {
		return fs.ErrNotExist
	}
	defer archiveRoot.Close()
	entryInfo, err := archiveRoot.Lstat(id)
	if err != nil || entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.IsDir() {
		return errors.New("preserved Workspace entry is unavailable")
	}
	entryRoot, err := archiveRoot.OpenRoot(id)
	if err != nil {
		return err
	}
	defer entryRoot.Close()
	openedInfo, err := entryRoot.Stat(".")
	if err != nil || !os.SameFile(entryInfo, openedInfo) {
		return errors.New("preserved Workspace entry changed while opening")
	}
	metadata, err := readPreservedMetadata(entryRoot, id, workspaceID)
	if err != nil {
		return fmt.Errorf("read preserved Workspace metadata: %w", err)
	}
	sourceInfo, err := entryRoot.Lstat("content")
	if err != nil || sourceInfo.Mode()&os.ModeSymlink == 0 && !sourceInfo.Mode().IsRegular() {
		return errors.New("preserved Workspace content is unavailable")
	}
	parent, name, closeParent, err := openClientParentForRestore(workspaceRoot, metadata.Path)
	if err != nil {
		return err
	}
	defer closeParent()
	if _, err := parent.Lstat(name); err == nil {
		return ErrPreservedDestinationExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 {
		target, err := entryRoot.Readlink("content")
		latest, statErr := entryRoot.Lstat("content")
		if err != nil || statErr != nil || !os.SameFile(sourceInfo, latest) || mordesync.ValidateSymlinkTarget(metadata.Path, target) != nil {
			return errors.Join(errors.New("preserved Workspace symlink changed"), err, statErr)
		}
		if err := parent.Symlink(target, name); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return ErrPreservedDestinationExists
			}
			return fmt.Errorf("restore preserved Workspace symlink: %w", err)
		}
		file := mordesync.File{Path: metadata.Path, Kind: mordesync.FileKindSymlink, SymlinkTarget: target,
			Size: int64(len([]byte(target))), Mode: 0, SHA256: sumSHA256([]byte(target)), ModTimeNS: sourceInfo.ModTime().UnixNano(), ModTimeKnown: true}
		if err := mordesync.VerifyExistingSymlink(ctx, canonical, file); err != nil {
			if info, statErr := parent.Lstat(name); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
				actual, readErr := parent.Readlink(name)
				if readErr == nil && actual == target {
					_ = parent.Remove(name)
				}
			}
			return err
		}
		return nil
	}
	tempName, err := clientTemporaryName()
	if err != nil {
		return err
	}
	temporary, err := parent.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create restore staging file: %w", err)
	}
	tempExists := true
	defer func() {
		_ = temporary.Close()
		if tempExists {
			_ = parent.Remove(tempName)
		}
	}()
	source, err := entryRoot.Open("content")
	if err != nil {
		return err
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(temporary, hasher), clientContextReader{ctx: ctx, reader: source})
	closeErr := source.Close()
	if copyErr != nil {
		return fmt.Errorf("copy preserved Workspace file: %w", copyErr)
	}
	if closeErr != nil {
		return closeErr
	}
	afterInfo, err := entryRoot.Lstat("content")
	if err != nil || !os.SameFile(sourceInfo, afterInfo) || sourceInfo.Size() != afterInfo.Size() || sourceInfo.ModTime() != afterInfo.ModTime() {
		return mordesync.ErrDestinationChange
	}
	_, digest, exists, err := hashClientPath(ctx, entryRoot, "content")
	if err != nil || !exists || digest != hex.EncodeToString(hasher.Sum(nil)) {
		return mordesync.ErrDestinationChange
	}
	if err := temporary.Chmod(sourceInfo.Mode().Perm()); err != nil {
		return fmt.Errorf("set restored Workspace file mode: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync restored Workspace file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close restored Workspace file: %w", err)
	}
	if err := parent.Link(tempName, name); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrPreservedDestinationExists
		}
		return fmt.Errorf("install restored Workspace file without replacement: %w", err)
	}
	if err := parent.Remove(tempName); err != nil {
		tempExists = false
		return fmt.Errorf("remove restored Workspace staging link: %w", err)
	}
	tempExists = false
	if err := syncClientDirectory(parent); err != nil {
		return fmt.Errorf("sync restored Workspace directory: %w", err)
	}
	return nil
}

func restorePreservedLink(workspaceRoot *os.Root, directory, workspaceID, archivePath, relativePath string) error {
	if !validPreservedPath(relativePath) {
		return errors.New("invalid preserved Workspace path")
	}
	parentRoot, archiveRoot, err := openPreservedRoots(workspaceRoot, directory, workspaceID, false)
	if err != nil {
		return err
	}
	if parentRoot == nil || archiveRoot == nil {
		return fs.ErrNotExist
	}
	defer parentRoot.Close()
	defer archiveRoot.Close()
	archiveInfo, err := parentRoot.Lstat(archivePath)
	if err != nil {
		return errors.New("preserved Workspace content is unavailable")
	}
	sourcePath := filepath.Join(filepath.Base(directory), filepath.FromSlash(relativePath))
	if archiveInfo.Mode()&os.ModeSymlink != 0 {
		target, err := parentRoot.Readlink(archivePath)
		latest, statErr := parentRoot.Lstat(archivePath)
		if err != nil || statErr != nil || !os.SameFile(archiveInfo, latest) || latest.Mode()&os.ModeSymlink == 0 ||
			mordesync.ValidateSymlinkTarget(relativePath, target) != nil {
			return errors.Join(errors.New("preserved Workspace symlink changed"), err, statErr)
		}
		if err := parentRoot.Symlink(target, sourcePath); err != nil {
			return err
		}
		file := mordesync.File{Path: relativePath, Kind: mordesync.FileKindSymlink, SymlinkTarget: target,
			Size: int64(len([]byte(target))), Mode: 0, SHA256: sumSHA256([]byte(target)), ModTimeNS: archiveInfo.ModTime().UnixNano(), ModTimeKnown: true}
		if err := mordesync.VerifyExistingSymlink(context.Background(), directory, file); err != nil {
			if info, statErr := parentRoot.Lstat(sourcePath); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
				actual, readErr := parentRoot.Readlink(sourcePath)
				if readErr == nil && actual == target {
					_ = parentRoot.Remove(sourcePath)
				}
			}
			return err
		}
		return nil
	}
	if !archiveInfo.Mode().IsRegular() {
		return errors.New("preserved Workspace content is unavailable")
	}
	if err := parentRoot.Link(archivePath, sourcePath); err != nil {
		return err
	}
	return nil
}

func openClientParentForRestore(root *os.Root, relative string) (*os.Root, string, func(), error) {
	if !validPreservedPath(relative) {
		return nil, "", func() {}, errors.New("invalid preserved Workspace path")
	}
	parts := strings.Split(relative, "/")
	current := root
	owned := make([]*os.Root, 0, len(parts)-1)
	closeOwned := func() {
		for index := len(owned) - 1; index >= 0; index-- {
			_ = owned[index].Close()
		}
	}
	for _, part := range parts[:len(parts)-1] {
		info, err := current.Lstat(part)
		if errors.Is(err, fs.ErrNotExist) {
			mkdirErr := current.Mkdir(part, 0o755)
			if mkdirErr != nil && !errors.Is(mkdirErr, fs.ErrExist) {
				closeOwned()
				return nil, "", func() {}, mkdirErr
			}
			if mkdirErr == nil {
				if err := syncClientDirectory(current); err != nil {
					closeOwned()
					return nil, "", func() {}, err
				}
			}
			info, err = current.Lstat(part)
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			closeOwned()
			return nil, "", func() {}, errors.New("preserved Workspace parent is unavailable or unsafe")
		}
		child, err := current.OpenRoot(part)
		if err != nil {
			closeOwned()
			return nil, "", func() {}, errors.New("open preserved Workspace parent")
		}
		childInfo, err := child.Stat(".")
		if err != nil || !childInfo.IsDir() || !os.SameFile(info, childInfo) {
			_ = child.Close()
			closeOwned()
			return nil, "", func() {}, errors.New("preserved Workspace parent changed while opening")
		}
		owned = append(owned, child)
		current = child
	}
	return current, parts[len(parts)-1], closeOwned, nil
}
