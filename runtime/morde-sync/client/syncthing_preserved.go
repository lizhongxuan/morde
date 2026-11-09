package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const syncthingPreservedMetadataSuffix = ".morde.json"

var (
	syncthingFolderIDPattern = regexp.MustCompile(`^morde-[a-f0-9]{32}$`)
	syncthingEntryIDPattern  = regexp.MustCompile(`^[A-Za-z0-9]{20}$`)
)

type syncthingPreservedMetadata struct {
	Version   int       `json:"version"`
	EntryID   string    `json:"entry_id"`
	FolderID  string    `json:"folder_id"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
}

type syncthingPreservedItem struct {
	metadata syncthingPreservedMetadata
	record   PreservedFile
}

// ListSyncthingPreserved returns F02 node recovery copies for one registered folder.
func ListSyncthingPreserved(ctx context.Context, directory, workspaceID, folderID string) ([]PreservedFile, error) {
	if ctx == nil || !workspaceIDPattern.MatchString(workspaceID) || !syncthingFolderIDPattern.MatchString(folderID) {
		return nil, errors.New("context and valid Workspace and folder IDs are required")
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
	parentRoot, archiveRoot, err := openSyncthingPreservedRoots(workspaceRoot, canonical, folderID)
	if err != nil {
		return nil, err
	}
	if archiveRoot == nil {
		return []PreservedFile{}, nil
	}
	defer parentRoot.Close()
	defer archiveRoot.Close()
	items, err := listSyncthingPreservedRoot(ctx, archiveRoot, workspaceID, folderID)
	if err != nil {
		return nil, err
	}
	result := make([]PreservedFile, 0, len(items))
	for _, item := range items {
		result = append(result, item.record)
	}
	return result, nil
}

// RestoreSyncthingPreserved copies one F02 archive to its original path only when that path is empty.
func RestoreSyncthingPreserved(ctx context.Context, directory, workspaceID, folderID, id string) error {
	if ctx == nil || !workspaceIDPattern.MatchString(workspaceID) || !syncthingFolderIDPattern.MatchString(folderID) || !preservedIDPattern.MatchString(id) {
		return errors.New("context and valid Workspace, folder, and preservation IDs are required")
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
	parentRoot, archiveRoot, err := openSyncthingPreservedRoots(workspaceRoot, canonical, folderID)
	if err != nil {
		return err
	}
	if archiveRoot == nil {
		return fs.ErrNotExist
	}
	defer parentRoot.Close()
	defer archiveRoot.Close()
	items, err := listSyncthingPreservedRoot(ctx, archiveRoot, workspaceID, folderID)
	if err != nil {
		return err
	}
	var selected *syncthingPreservedItem
	for index := range items {
		if items[index].record.ID == id {
			selected = &items[index]
			break
		}
	}
	if selected == nil {
		return fs.ErrNotExist
	}
	sourceParent, sourceName, sourceInfo, closeSource, err := openSyncthingPreservedItem(archiveRoot, selected.metadata)
	if err != nil {
		return err
	}
	defer closeSource()
	destinationParent, destinationName, closeDestination, err := openClientParentForRestore(workspaceRoot, selected.metadata.Path)
	if err != nil {
		return err
	}
	defer closeDestination()
	if _, err := destinationParent.Lstat(destinationName); err == nil {
		return ErrPreservedDestinationExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := copySyncthingPreservedItem(ctx, sourceParent, sourceName, sourceInfo, destinationParent, destinationName); err != nil {
		return err
	}
	return syncClientDirectory(destinationParent)
}

// DeleteSyncthingPreserved removes exactly one selected F02 archive entry.
func DeleteSyncthingPreserved(ctx context.Context, directory, workspaceID, folderID, id string) error {
	if ctx == nil || !workspaceIDPattern.MatchString(workspaceID) || !syncthingFolderIDPattern.MatchString(folderID) || !preservedIDPattern.MatchString(id) {
		return errors.New("context and valid Workspace, folder, and preservation IDs are required")
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
	parentRoot, archiveRoot, err := openSyncthingPreservedRoots(workspaceRoot, canonical, folderID)
	if err != nil {
		return err
	}
	if archiveRoot == nil {
		return fs.ErrNotExist
	}
	defer parentRoot.Close()
	defer archiveRoot.Close()
	items, err := listSyncthingPreservedRoot(ctx, archiveRoot, workspaceID, folderID)
	if err != nil {
		return err
	}
	var selected *syncthingPreservedItem
	for index := range items {
		if items[index].record.ID == id {
			selected = &items[index]
			break
		}
	}
	if selected == nil {
		return fs.ErrNotExist
	}
	entryInfo, err := archiveRoot.Lstat(selected.metadata.EntryID)
	if err != nil || entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.IsDir() {
		return errors.New("Syncthing preservation entry changed before deletion")
	}
	if _, err := readSyncthingPreservedMetadata(archiveRoot, selected.metadata.EntryID+syncthingPreservedMetadataSuffix, folderID, selected.metadata.EntryID); err != nil {
		return errors.New("Syncthing preservation metadata changed before deletion")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := archiveRoot.RemoveAll(selected.metadata.EntryID); err != nil {
		return fmt.Errorf("remove Syncthing preservation entry: %w", err)
	}
	if err := archiveRoot.Remove(selected.metadata.EntryID + syncthingPreservedMetadataSuffix); err != nil {
		return fmt.Errorf("remove Syncthing preservation metadata: %w", err)
	}
	return syncClientDirectory(archiveRoot)
}

func openSyncthingPreservedRoots(workspaceRoot *os.Root, directory, folderID string) (*os.Root, *os.Root, error) {
	if workspaceRoot == nil || !syncthingFolderIDPattern.MatchString(folderID) {
		return nil, nil, errors.New("invalid Syncthing preservation root")
	}
	parentRoot, err := os.OpenRoot(filepath.Dir(directory))
	if err != nil {
		return nil, nil, fmt.Errorf("open parent of Workspace directory: %w", err)
	}
	workspaceInfo, err := workspaceRoot.Stat(".")
	if err != nil {
		_ = parentRoot.Close()
		return nil, nil, err
	}
	currentInfo, err := parentRoot.Lstat(filepath.Base(directory))
	if err != nil || !os.SameFile(workspaceInfo, currentInfo) {
		_ = parentRoot.Close()
		return nil, nil, errors.New("Workspace directory changed while opening preservation root")
	}
	preservedRoot, err := openOrCreateClientRoot(parentRoot, preservedDirectory, false)
	if errors.Is(err, fs.ErrNotExist) {
		_ = parentRoot.Close()
		return nil, nil, nil
	}
	if err != nil {
		_ = parentRoot.Close()
		return nil, nil, fmt.Errorf("open local preservation directory: %w", err)
	}
	folderKey := sha256.Sum256([]byte(folderID))
	archiveRoot, err := openOrCreateClientRoot(preservedRoot, hex.EncodeToString(folderKey[:]), false)
	_ = preservedRoot.Close()
	if errors.Is(err, fs.ErrNotExist) {
		_ = parentRoot.Close()
		return nil, nil, nil
	}
	if err != nil {
		_ = parentRoot.Close()
		return nil, nil, fmt.Errorf("open Syncthing preservation directory: %w", err)
	}
	return parentRoot, archiveRoot, nil
}

func listSyncthingPreservedRoot(ctx context.Context, archiveRoot *os.Root, workspaceID, folderID string) ([]syncthingPreservedItem, error) {
	entries, err := readClientRootDirectory(archiveRoot)
	if err != nil {
		return nil, err
	}
	result := make([]syncthingPreservedItem, 0, len(entries)/2)
	seen := make(map[string]struct{})
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if !strings.HasSuffix(name, syncthingPreservedMetadataSuffix) {
			info, err := archiveRoot.Lstat(name)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !syncthingEntryIDPattern.MatchString(name) {
				return nil, errors.New("Syncthing preservation directory contains an unrecognized entry")
			}
			if _, err := archiveRoot.Lstat(name + syncthingPreservedMetadataSuffix); errors.Is(err, fs.ErrNotExist) {
				return nil, errors.New("Syncthing preservation entry has no Morde metadata; keep it for manual recovery")
			} else if err != nil {
				return nil, err
			}
			continue
		}
		entryID := strings.TrimSuffix(name, syncthingPreservedMetadataSuffix)
		if !syncthingEntryIDPattern.MatchString(entryID) {
			return nil, errors.New("Syncthing preservation metadata name is invalid")
		}
		metadata, err := readSyncthingPreservedMetadata(archiveRoot, name, folderID, entryID)
		if err != nil {
			return nil, fmt.Errorf("read Syncthing preservation metadata: %w", err)
		}
		entryInfo, err := archiveRoot.Lstat(entryID)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.IsDir() {
			return nil, errors.New("Syncthing preservation entry is not a real directory")
		}
		sourceParent, sourceName, info, closeSource, err := openSyncthingPreservedItem(archiveRoot, metadata)
		if err != nil {
			return nil, err
		}
		size, sizeErr := syncthingPreservedSize(ctx, sourceParent, sourceName)
		closeSource()
		if sizeErr != nil {
			return nil, sizeErr
		}
		if !supportedPreservedItem(info) || size < 0 {
			return nil, errors.New("Syncthing preservation item is unsupported")
		}
		id := syncthingPreservedID(folderID, entryID, metadata.Path)
		if _, duplicate := seen[id]; duplicate {
			return nil, errors.New("Syncthing preservation IDs are not unique")
		}
		seen[id] = struct{}{}
		result = append(result, syncthingPreservedItem{metadata, PreservedFile{
			ID: id, WorkspaceID: workspaceID, Path: metadata.Path, CreatedAt: metadata.CreatedAt, Size: size,
		}})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].record.CreatedAt.After(result[j].record.CreatedAt) })
	return result, nil
}

func readSyncthingPreservedMetadata(root *os.Root, name, folderID, entryID string) (syncthingPreservedMetadata, error) {
	info, err := root.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return syncthingPreservedMetadata{}, errors.New("Syncthing preservation metadata file is invalid")
	}
	file, err := root.Open(name)
	if err != nil {
		return syncthingPreservedMetadata{}, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() || openedInfo.Size() > 16<<10 {
		return syncthingPreservedMetadata{}, errors.New("Syncthing preservation metadata changed while opening")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 16<<10))
	decoder.DisallowUnknownFields()
	var metadata syncthingPreservedMetadata
	if err := decoder.Decode(&metadata); err != nil {
		return syncthingPreservedMetadata{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return syncthingPreservedMetadata{}, errors.New("Syncthing preservation metadata has trailing content")
	}
	if metadata.Version != 1 || metadata.EntryID != entryID || metadata.FolderID != folderID || !validPreservedPath(metadata.Path) || metadata.CreatedAt.IsZero() {
		return syncthingPreservedMetadata{}, errors.New("Syncthing preservation metadata is invalid")
	}
	return metadata, nil
}

func openSyncthingPreservedItem(archiveRoot *os.Root, metadata syncthingPreservedMetadata) (*os.Root, string, os.FileInfo, func(), error) {
	entryInfo, err := archiveRoot.Lstat(metadata.EntryID)
	if err != nil || entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.IsDir() {
		return nil, "", nil, func() {}, errors.New("Syncthing preservation entry is unavailable")
	}
	entryRoot, err := archiveRoot.OpenRoot(metadata.EntryID)
	if err != nil {
		return nil, "", nil, func() {}, err
	}
	openedInfo, err := entryRoot.Stat(".")
	if err != nil || !os.SameFile(entryInfo, openedInfo) {
		_ = entryRoot.Close()
		return nil, "", nil, func() {}, errors.New("Syncthing preservation entry changed while opening")
	}
	parent, name, closeParent, err := openExistingClientParent(entryRoot, metadata.Path)
	if err != nil {
		_ = entryRoot.Close()
		return nil, "", nil, func() {}, err
	}
	info, err := parent.Lstat(name)
	if err != nil || !supportedPreservedItem(info) {
		closeParent()
		_ = entryRoot.Close()
		return nil, "", nil, func() {}, errors.New("Syncthing preservation item is unavailable or unsupported")
	}
	return parent, name, info, func() { closeParent(); _ = entryRoot.Close() }, nil
}

func openExistingClientParent(root *os.Root, relative string) (*os.Root, string, func(), error) {
	if !validPreservedPath(relative) {
		return nil, "", func() {}, errors.New("invalid Syncthing preservation path")
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
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			closeOwned()
			return nil, "", func() {}, errors.New("Syncthing preservation parent is unavailable or unsafe")
		}
		child, err := current.OpenRoot(part)
		if err != nil {
			closeOwned()
			return nil, "", func() {}, err
		}
		childInfo, err := child.Stat(".")
		if err != nil || !childInfo.IsDir() || !os.SameFile(info, childInfo) {
			_ = child.Close()
			closeOwned()
			return nil, "", func() {}, errors.New("Syncthing preservation parent changed while opening")
		}
		owned = append(owned, child)
		current = child
	}
	return current, parts[len(parts)-1], closeOwned, nil
}

func readClientRootDirectory(root *os.Root) ([]os.DirEntry, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	return directory.ReadDir(-1)
}

func supportedPreservedItem(info os.FileInfo) bool {
	return info != nil && (info.Mode().IsRegular() || info.IsDir() || info.Mode()&os.ModeSymlink != 0)
}

func syncthingPreservedID(folderID, entryID, relative string) string {
	digest := sha256.Sum256([]byte(folderID + "\x00" + entryID + "\x00" + relative))
	return hex.EncodeToString(digest[:16])
}

func syncthingPreservedSize(ctx context.Context, root *os.Root, name string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	info, err := root.Lstat(name)
	if err != nil {
		return 0, err
	}
	if info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return info.Size(), nil
	}
	if !info.IsDir() {
		return 0, errors.New("Syncthing preservation item is unsupported")
	}
	directory, err := root.OpenRoot(name)
	if err != nil {
		return 0, err
	}
	defer directory.Close()
	openedInfo, err := directory.Stat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		return 0, errors.New("Syncthing preservation directory changed while opening")
	}
	entries, err := readClientRootDirectory(directory)
	if err != nil {
		return 0, err
	}
	var size int64
	for _, entry := range entries {
		childSize, err := syncthingPreservedSize(ctx, directory, entry.Name())
		if err != nil {
			return 0, err
		}
		if childSize > int64(^uint64(0)>>1)-size {
			return 0, errors.New("Syncthing preservation size exceeds the supported range")
		}
		size += childSize
	}
	return size, nil
}

func copySyncthingPreservedItem(ctx context.Context, source *os.Root, sourceName string, sourceInfo os.FileInfo, destination *os.Root, destinationName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	currentInfo, err := source.Lstat(sourceName)
	if err != nil || !os.SameFile(sourceInfo, currentInfo) || !supportedPreservedItem(currentInfo) {
		return errors.New("Syncthing preservation item changed before restore")
	}
	if currentInfo.Mode().IsRegular() {
		return copySyncthingPreservedFile(ctx, source, sourceName, currentInfo, destination, destinationName)
	}
	if currentInfo.Mode()&os.ModeSymlink != 0 {
		target, err := source.Readlink(sourceName)
		if err != nil {
			return err
		}
		latest, err := source.Lstat(sourceName)
		if err != nil || !os.SameFile(currentInfo, latest) {
			return errors.New("Syncthing preservation symlink changed during restore")
		}
		return destination.Symlink(target, destinationName)
	}
	if err := destination.Mkdir(destinationName, currentInfo.Mode().Perm()); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrPreservedDestinationExists
		}
		return err
	}
	createdInfo, err := destination.Lstat(destinationName)
	if err != nil || createdInfo.Mode()&os.ModeSymlink != 0 || !createdInfo.IsDir() {
		return errors.New("restore destination changed while creating directory")
	}
	cleanup := func() {
		latest, latestErr := destination.Lstat(destinationName)
		if latestErr == nil && os.SameFile(createdInfo, latest) {
			_ = destination.RemoveAll(destinationName)
		}
	}
	sourceRoot, err := source.OpenRoot(sourceName)
	if err != nil {
		cleanup()
		return err
	}
	defer sourceRoot.Close()
	openedSource, err := sourceRoot.Stat(".")
	if err != nil || !os.SameFile(currentInfo, openedSource) {
		cleanup()
		return errors.New("Syncthing preservation directory changed while restoring")
	}
	destinationRoot, err := destination.OpenRoot(destinationName)
	if err != nil {
		cleanup()
		return err
	}
	defer destinationRoot.Close()
	openedDestination, err := destinationRoot.Stat(".")
	if err != nil || !os.SameFile(createdInfo, openedDestination) {
		cleanup()
		return errors.New("restore destination directory changed while opening")
	}
	entries, err := readClientRootDirectory(sourceRoot)
	if err != nil {
		cleanup()
		return err
	}
	for _, entry := range entries {
		childInfo, err := sourceRoot.Lstat(entry.Name())
		if err != nil {
			cleanup()
			return err
		}
		if err := copySyncthingPreservedItem(ctx, sourceRoot, entry.Name(), childInfo, destinationRoot, entry.Name()); err != nil {
			cleanup()
			return err
		}
	}
	if err := syncClientDirectory(destinationRoot); err != nil {
		cleanup()
		return err
	}
	return nil
}

func copySyncthingPreservedFile(ctx context.Context, source *os.Root, sourceName string, sourceInfo os.FileInfo, destination *os.Root, destinationName string) error {
	input, err := source.Open(sourceName)
	if err != nil {
		return err
	}
	defer input.Close()
	openedInfo, err := input.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, openedInfo) {
		return errors.New("Syncthing preservation file changed while opening")
	}
	output, err := destination.OpenFile(destinationName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, sourceInfo.Mode().Perm())
	if errors.Is(err, fs.ErrExist) {
		return ErrPreservedDestinationExists
	}
	if err != nil {
		return err
	}
	createdInfo, err := output.Stat()
	if err != nil {
		_ = output.Close()
		_ = destination.Remove(destinationName)
		return err
	}
	_, copyErr := io.Copy(output, clientContextReader{ctx: ctx, reader: input})
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	latest, statErr := input.Stat()
	if copyErr == nil && closeErr != nil {
		copyErr = closeErr
	}
	if copyErr == nil && (statErr != nil || !os.SameFile(sourceInfo, latest) || latest.Size() != sourceInfo.Size()) {
		copyErr = errors.New("Syncthing preservation file changed during restore")
	}
	if copyErr != nil {
		current, currentErr := destination.Lstat(destinationName)
		if currentErr == nil && os.SameFile(createdInfo, current) {
			_ = destination.Remove(destinationName)
		}
		return copyErr
	}
	return nil
}
