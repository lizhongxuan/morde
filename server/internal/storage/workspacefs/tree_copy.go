//go:build darwin || linux

package workspacefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain/workspace"
)

var (
	ErrTreeCopyDestinationNotEmpty = errors.New("workspace tree copy destination must be an empty directory")
	ErrTreeCopyOverlappingRoots    = errors.New("workspace tree copy source and destination must not overlap")
	ErrTreeCopyUnsupportedMetadata = errors.New("workspace tree copy cannot preserve extended attributes or ACLs")
)

type treeCopyCreatedPath struct {
	name string
	info os.FileInfo
}

type treeCopyHardlink struct {
	sourceInfo      os.FileInfo
	destinationPath string
	destinationInfo os.FileInfo
	xattrs          []workspace.ExtendedAttribute
	acl             *workspace.NativeACL
}

// ValidateTreeCopy rejects metadata and file types that CopyTree cannot preserve.
func ValidateTreeCopy(ctx context.Context, sourcePath string) error {
	if ctx == nil {
		return errors.New("workspace tree copy context is required")
	}
	sourcePath, sourceInfo, err := resolveTreeCopyRoot(sourcePath)
	if err != nil {
		return fmt.Errorf("inspect workspace tree copy source: %w", err)
	}
	sourceRoot, err := os.OpenRoot(sourcePath)
	if err != nil {
		return fmt.Errorf("open workspace tree copy source: %w", err)
	}
	defer func() { _ = sourceRoot.Close() }()
	if opened, err := sourceRoot.Stat("."); err != nil || !os.SameFile(sourceInfo, opened) {
		return errors.Join(errors.New("workspace tree copy source changed before preflight"), err)
	}
	_, err = preflightTreeCopy(ctx, sourcePath, sourceRoot, sourceInfo)
	return err
}

func preflightTreeCopy(ctx context.Context, sourcePath string, sourceRoot *os.Root, rootInfo os.FileInfo) (map[string]os.FileInfo, error) {
	snapshot := map[string]os.FileInfo{".": rootInfo}
	if err := validateTreeCopyDirectory(ctx, sourcePath, sourceRoot, ".", rootInfo, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func validateTreeCopyDirectory(ctx context.Context, sourcePath string, sourceRoot *os.Root, name string,
	expectedInfo os.FileInfo, snapshot map[string]os.FileInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := sourceRoot.Open(name)
	if err != nil {
		return fmt.Errorf("open workspace directory %q: %w", name, err)
	}
	defer func() { _ = directory.Close() }()
	openedInfo, err := directory.Stat()
	if err != nil || !openedInfo.IsDir() || !treeCopyStableInfo(expectedInfo, openedInfo) {
		return errors.Join(fmt.Errorf("workspace directory %q changed during preflight", name), err)
	}
	beforeXattrs, beforeACL, err := ReadTreeCopyMetadata(sourceRoot, sourcePath, name, false)
	if err != nil {
		return err
	}
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("read workspace directory %q: %w", name, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		child := entry.Name()
		if name != "." {
			child = path.Join(name, child)
		}
		info, err := sourceRoot.Lstat(child)
		if err != nil {
			return fmt.Errorf("inspect workspace path %q: %w", child, err)
		}
		snapshot[child] = info
		switch {
		case info.IsDir():
			if err := validateTreeCopyDirectory(ctx, sourcePath, sourceRoot, child, info, snapshot); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := validateTreeCopyMetadata(sourceRoot, sourcePath, child, false); err != nil {
				return err
			}
			file, err := sourceRoot.Open(child)
			if err != nil {
				return fmt.Errorf("open workspace file %q: %w", child, err)
			}
			fileInfo, statErr := file.Stat()
			closeErr := file.Close()
			if statErr != nil || closeErr != nil || !fileInfo.Mode().IsRegular() || !treeCopyStableInfo(info, fileInfo) {
				return errors.Join(fmt.Errorf("workspace file %q changed during preflight", child), statErr, closeErr)
			}
		case info.Mode()&os.ModeSymlink != 0:
			if err := validateTreeCopyMetadata(sourceRoot, sourcePath, child, true); err != nil {
				return err
			}
			target, err := sourceRoot.Readlink(child)
			if err != nil {
				return fmt.Errorf("read workspace symlink %q: %w", child, err)
			}
			currentInfo, err := sourceRoot.Lstat(child)
			if err != nil || !treeCopyStableInfo(info, currentInfo) {
				return errors.Join(fmt.Errorf("workspace symlink %q changed during preflight", child), err)
			}
			if err := validateTreeCopySymlink(child, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("workspace tree copy does not support special file %q (%s)", child, info.Mode().Type())
		}
	}
	currentXattrs, currentACL, err := ReadTreeCopyMetadata(sourceRoot, sourcePath, name, false)
	if err != nil {
		return err
	}
	if !SameTreeCopyMetadata(beforeXattrs, beforeACL, currentXattrs, currentACL) {
		return fmt.Errorf("workspace directory %q metadata changed during preflight", name)
	}
	currentInfo, err := directory.Stat()
	if err != nil || !treeCopyStableInfo(expectedInfo, currentInfo) {
		return errors.Join(fmt.Errorf("workspace directory %q changed during preflight", name), err)
	}
	return nil
}

func treeCopyStableInfo(expected, current os.FileInfo) bool {
	return expected != nil && current != nil && os.SameFile(expected, current) && expected.Mode() == current.Mode() &&
		expected.Size() == current.Size() && expected.ModTime().Equal(current.ModTime())
}

func validateTreeCopyMetadata(root *os.Root, rootPath, name string, symlink bool) error {
	_, _, err := ReadTreeCopyMetadata(root, rootPath, name, symlink)
	return err
}

// CopyTree copies an already-quiesced directory tree into an existing empty directory.
func CopyTree(sourcePath, destinationPath string) error {
	sourcePath, sourceInfo, err := resolveTreeCopyRoot(sourcePath)
	if err != nil {
		return fmt.Errorf("inspect workspace tree copy source: %w", err)
	}
	destinationPath, destinationInfo, err := resolveTreeCopyRoot(destinationPath)
	if err != nil {
		return fmt.Errorf("inspect workspace tree copy destination: %w", err)
	}
	if treeCopyPathsOverlap(sourcePath, destinationPath) {
		return ErrTreeCopyOverlappingRoots
	}
	if os.SameFile(sourceInfo, destinationInfo) {
		return ErrTreeCopyOverlappingRoots
	}
	sourceRoot, err := os.OpenRoot(sourcePath)
	if err != nil {
		return fmt.Errorf("open workspace tree copy source: %w", err)
	}
	defer func() { _ = sourceRoot.Close() }()
	destinationRoot, err := os.OpenRoot(destinationPath)
	if err != nil {
		return fmt.Errorf("open workspace tree copy destination: %w", err)
	}
	defer func() { _ = destinationRoot.Close() }()
	destinationRootFile, err := destinationRoot.Open(".")
	if err != nil {
		return fmt.Errorf("open workspace tree copy destination root: %w", err)
	}
	defer func() { _ = destinationRootFile.Close() }()
	if opened, err := sourceRoot.Stat("."); err != nil || !os.SameFile(sourceInfo, opened) {
		return fmt.Errorf("workspace tree copy source changed before copy")
	}
	if opened, err := destinationRoot.Stat("."); err != nil || !os.SameFile(destinationInfo, opened) {
		return fmt.Errorf("workspace tree copy destination changed before copy")
	}
	destinationXattrs, destinationACL, err := ReadTreeCopyMetadata(destinationRoot, destinationPath, ".", false)
	if err != nil {
		return err
	}
	entries, err := readTreeCopyDirectory(destinationRoot, ".")
	if err != nil {
		return fmt.Errorf("inspect workspace tree copy destination: %w", err)
	}
	if len(entries) != 0 {
		return ErrTreeCopyDestinationNotEmpty
	}
	// Inspect the complete source before creating any destination entry. The
	// copy pass repeats metadata reads so a tree that changes after preflight
	// still fails and is rolled back.
	sourceSnapshot, err := preflightTreeCopy(context.Background(), sourcePath, sourceRoot, sourceInfo)
	if err != nil {
		return err
	}

	created := make([]treeCopyCreatedPath, 0)
	hardlinks := make([]treeCopyHardlink, 0)
	copyErr := copyTreeDirectory(sourcePath, destinationPath, sourceRoot, destinationRoot, destinationRootFile, sourceSnapshot, ".", sourceInfo, destinationInfo, &created, &hardlinks)
	if copyErr != nil {
		cleanupErr := error(nil)
		for i := len(created) - 1; i >= 0; i-- {
			current, statErr := destinationRoot.Lstat(created[i].name)
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			if statErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("inspect workspace copy rollback path %q: %w", created[i].name, statErr))
				continue
			}
			if !os.SameFile(created[i].info, current) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("workspace copy rollback skipped changed path %q", created[i].name))
				continue
			}
			if err := destinationRoot.Remove(created[i].name); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove workspace copy rollback path %q: %w", created[i].name, err))
			}
		}
		if err := restoreTreeCopyDirectoryMetadata(destinationRoot, destinationRootFile, destinationPath, destinationInfo, destinationXattrs, destinationACL); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("restore workspace copy destination root metadata: %w", err))
		}
		return errors.Join(copyErr, cleanupErr)
	}
	return nil
}

func restoreTreeCopyDirectoryMetadata(root *os.Root, file *os.File, rootPath string, info os.FileInfo, xattrs []workspace.ExtendedAttribute,
	acl *workspace.NativeACL) error {
	if err := ApplyOpenedTreeCopyMetadata(file, rootPath, ".", xattrs, acl); err != nil {
		return err
	}
	if err := file.Chmod(treeCopyPermissionMode(info.Mode())); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	atime, ok := treeCopyAccessTime(info)
	if !ok {
		atime = info.ModTime()
	}
	return root.Chtimes(".", atime, info.ModTime())
}

func resolveTreeCopyRoot(name string) (string, os.FileInfo, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", nil, fmt.Errorf("resolve path: %w", err)
	}
	absolute = filepath.Clean(absolute)
	if absolute == string(filepath.Separator) || filepath.VolumeName(absolute)+string(filepath.Separator) == absolute {
		return "", nil, errors.New("workspace tree copy root cannot be a filesystem root")
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", nil, errors.New("workspace tree copy root must be a real directory")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", nil, err
	}
	resolved = filepath.Clean(resolved)
	info, err = os.Lstat(resolved)
	if err != nil {
		return "", nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", nil, errors.New("workspace tree copy root must be a real directory")
	}
	return resolved, info, nil
}

func treeCopyPathsOverlap(first, second string) bool {
	return treeCopyPathContains(first, second) || treeCopyPathContains(second, first)
}

func treeCopyPathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && (relative == "." || (relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

func copyTreeDirectory(sourcePath, destinationPath string, sourceRoot, destinationRoot *os.Root, destinationRootFile *os.File,
	sourceSnapshot map[string]os.FileInfo, name string, expectedInfo, destinationInfo os.FileInfo, created *[]treeCopyCreatedPath,
	hardlinks *[]treeCopyHardlink) error {
	sourceDirectory, err := sourceRoot.Open(name)
	if err != nil {
		return fmt.Errorf("open workspace directory %q: %w", name, err)
	}
	defer func() { _ = sourceDirectory.Close() }()
	openedInfo, err := sourceDirectory.Stat()
	if err != nil {
		return fmt.Errorf("stat workspace directory %q: %w", name, err)
	}
	if !openedInfo.IsDir() || !treeCopyStableInfo(expectedInfo, openedInfo) {
		return fmt.Errorf("workspace directory %q changed during copy", name)
	}
	metadataXattrs, metadataACL, err := ReadTreeCopyMetadata(sourceRoot, sourcePath, name, false)
	if err != nil {
		return err
	}
	entries, err := sourceDirectory.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("read workspace directory %q: %w", name, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		child := entry.Name()
		if name != "." {
			child = path.Join(name, child)
		}
		info, err := sourceRoot.Lstat(child)
		if err != nil {
			return fmt.Errorf("inspect workspace path %q: %w", child, err)
		}
		snapshotInfo := sourceSnapshot[child]
		if !treeCopyStableInfo(snapshotInfo, info) {
			return fmt.Errorf("workspace path %q changed after preflight", child)
		}
		mode := snapshotInfo.Mode()
		switch {
		case mode.IsDir():
			if err := destinationRoot.Mkdir(child, 0700); err != nil {
				return fmt.Errorf("create workspace directory %q: %w", child, err)
			}
			createdInfo, err := destinationRoot.Lstat(child)
			if err != nil {
				cleanupErr := destinationRoot.Remove(child)
				return errors.Join(fmt.Errorf("inspect created workspace directory %q: %w", child, err), treeCopyCleanupError(child, cleanupErr))
			}
			*created = append(*created, treeCopyCreatedPath{name: child, info: createdInfo})
			if err := copyTreeDirectory(sourcePath, destinationPath, sourceRoot, destinationRoot, destinationRootFile, sourceSnapshot,
				child, snapshotInfo, createdInfo, created, hardlinks); err != nil {
				return err
			}
		case mode.IsRegular():
			if err := copyTreeRegularFile(sourcePath, destinationPath, sourceRoot, destinationRoot, child, snapshotInfo, created, hardlinks); err != nil {
				return err
			}
		case mode&os.ModeSymlink != 0:
			xattrs, acl, err := ReadTreeCopyMetadata(sourceRoot, sourcePath, child, true)
			if err != nil {
				return err
			}
			target, err := sourceRoot.Readlink(child)
			if err != nil {
				return fmt.Errorf("read workspace symlink %q: %w", child, err)
			}
			currentInfo, err := sourceRoot.Lstat(child)
			if err != nil || !treeCopyStableInfo(snapshotInfo, currentInfo) {
				return fmt.Errorf("workspace symlink %q changed during copy", child)
			}
			currentXattrs, currentACL, err := ReadTreeCopyMetadata(sourceRoot, sourcePath, child, true)
			if err != nil {
				return err
			}
			if !SameTreeCopyMetadata(xattrs, acl, currentXattrs, currentACL) {
				return fmt.Errorf("workspace symlink %q metadata changed during copy", child)
			}
			if err := validateTreeCopySymlink(child, target); err != nil {
				return err
			}
			if err := destinationRoot.Symlink(target, child); err != nil {
				return fmt.Errorf("create workspace symlink %q: %w", child, err)
			}
			createdInfo, err := destinationRoot.Lstat(child)
			if err != nil {
				cleanupErr := destinationRoot.Remove(child)
				return errors.Join(fmt.Errorf("inspect created workspace symlink %q: %w", child, err), treeCopyCleanupError(child, cleanupErr))
			}
			*created = append(*created, treeCopyCreatedPath{name: child, info: createdInfo})
			if err := ensureTreeCopyPathInfo(destinationRoot, child, createdInfo); err != nil {
				return err
			}
			if err := ApplyTreeCopyMetadata(destinationRoot, destinationPath, child, true, xattrs, acl); err != nil {
				return err
			}
			if err := setTreeCopySymlinkTimes(destinationPath, child, snapshotInfo); err != nil {
				return fmt.Errorf("set workspace symlink time %q: %w", child, err)
			}
			if err := ensureTreeCopyPathInfo(destinationRoot, child, createdInfo); err != nil {
				return err
			}
		default:
			return fmt.Errorf("workspace tree copy does not support special file %q (%s)", child, mode.Type())
		}
	}
	currentInfo, err := sourceRoot.Lstat(name)
	if err != nil || !treeCopyStableInfo(expectedInfo, currentInfo) {
		return errors.Join(fmt.Errorf("workspace directory %q changed during copy", name), err)
	}
	currentXattrs, currentACL, err := ReadTreeCopyMetadata(sourceRoot, sourcePath, name, false)
	if err != nil {
		return err
	}
	if !SameTreeCopyMetadata(metadataXattrs, metadataACL, currentXattrs, currentACL) {
		return fmt.Errorf("workspace directory %q metadata changed during copy", name)
	}
	var rootFile *os.File
	if name == "." {
		rootFile = destinationRootFile
	}
	return finishTreeCopyDirectory(sourcePath, destinationPath, sourceRoot, destinationRoot, rootFile, name, destinationInfo,
		expectedInfo, metadataXattrs, metadataACL)
}

func copyTreeRegularFile(sourcePath, destinationPath string, sourceRoot, destinationRoot *os.Root, name string, expectedInfo os.FileInfo, created *[]treeCopyCreatedPath, hardlinks *[]treeCopyHardlink) error {
	xattrs, acl, err := ReadTreeCopyMetadata(sourceRoot, sourcePath, name, false)
	if err != nil {
		return err
	}
	for _, link := range *hardlinks {
		if !os.SameFile(link.sourceInfo, expectedInfo) {
			continue
		}
		if !SameTreeCopyMetadata(link.xattrs, link.acl, xattrs, acl) {
			return fmt.Errorf("workspace hard-linked file %q metadata changed during copy", name)
		}
		if err := ensureTreeCopyPathInfo(destinationRoot, link.destinationPath, link.destinationInfo); err != nil {
			return err
		}
		source, err := sourceRoot.Open(name)
		if err != nil {
			return fmt.Errorf("open workspace hard-linked file %q: %w", name, err)
		}
		openedSourceInfo, statErr := source.Stat()
		closeErr := source.Close()
		if statErr != nil || closeErr != nil || !openedSourceInfo.Mode().IsRegular() || !os.SameFile(expectedInfo, openedSourceInfo) ||
			openedSourceInfo.Size() != link.sourceInfo.Size() || !openedSourceInfo.ModTime().Equal(link.sourceInfo.ModTime()) {
			return errors.Join(fmt.Errorf("workspace hard-linked file %q changed during copy", name), statErr, closeErr)
		}
		if err := destinationRoot.Link(link.destinationPath, name); err != nil {
			return fmt.Errorf("create workspace hard link %q: %w", name, err)
		}
		createdInfo, err := destinationRoot.Lstat(name)
		if err != nil {
			return errors.Join(fmt.Errorf("inspect created workspace hard link %q: %w", name, err), treeCopyCleanupError(name, destinationRoot.Remove(name)))
		}
		*created = append(*created, treeCopyCreatedPath{name: name, info: createdInfo})
		if !createdInfo.Mode().IsRegular() || !os.SameFile(link.destinationInfo, createdInfo) {
			return fmt.Errorf("workspace hard link %q did not preserve file identity", name)
		}
		return ensureTreeCopyPathInfo(destinationRoot, link.destinationPath, link.destinationInfo)
	}
	source, err := sourceRoot.Open(name)
	if err != nil {
		return fmt.Errorf("open workspace file %q: %w", name, err)
	}
	defer func() { _ = source.Close() }()
	openedSourceInfo, err := source.Stat()
	if err != nil {
		return fmt.Errorf("stat workspace file %q: %w", name, err)
	}
	if !openedSourceInfo.Mode().IsRegular() || !os.SameFile(expectedInfo, openedSourceInfo) {
		return fmt.Errorf("workspace file %q changed during copy", name)
	}
	destination, err := destinationRoot.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("create workspace file %q: %w", name, err)
	}
	createdInfo, err := destination.Stat()
	if err != nil {
		closeErr := destination.Close()
		removeErr := destinationRoot.Remove(name)
		return errors.Join(fmt.Errorf("inspect created workspace file %q: %w", name, err), treeCopyCleanupError(name, closeErr), treeCopyCleanupError(name, removeErr))
	}
	*created = append(*created, treeCopyCreatedPath{name: name, info: createdInfo})
	written, err := io.Copy(destination, source)
	if err != nil {
		_ = destination.Close()
		return fmt.Errorf("copy workspace file %q: %w", name, err)
	}
	if written != expectedInfo.Size() {
		_ = destination.Close()
		return fmt.Errorf("workspace file %q changed during copy", name)
	}
	afterSourceInfo, err := source.Stat()
	if err != nil {
		_ = destination.Close()
		return fmt.Errorf("recheck workspace file %q: %w", name, err)
	}
	if !afterSourceInfo.Mode().IsRegular() || !os.SameFile(expectedInfo, afterSourceInfo) || afterSourceInfo.Size() != expectedInfo.Size() || !afterSourceInfo.ModTime().Equal(expectedInfo.ModTime()) {
		_ = destination.Close()
		return fmt.Errorf("workspace file %q changed during copy", name)
	}
	currentXattrs, currentACL, err := ReadTreeCopyMetadata(sourceRoot, sourcePath, name, false)
	if err != nil {
		_ = destination.Close()
		return err
	}
	if !SameTreeCopyMetadata(xattrs, acl, currentXattrs, currentACL) {
		_ = destination.Close()
		return fmt.Errorf("workspace file %q metadata changed during copy", name)
	}
	if err := ApplyOpenedTreeCopyMetadata(destination, filepath.Join(destinationPath, filepath.FromSlash(name)), name, xattrs, acl); err != nil {
		_ = destination.Close()
		return err
	}
	if err := destination.Chmod(treeCopyPermissionMode(expectedInfo.Mode())); err != nil {
		_ = destination.Close()
		return fmt.Errorf("set workspace file mode %q: %w", name, err)
	}
	if err := destination.Sync(); err != nil {
		_ = destination.Close()
		return fmt.Errorf("sync workspace file %q: %w", name, err)
	}
	if err := destination.Close(); err != nil {
		return fmt.Errorf("close workspace file %q: %w", name, err)
	}
	if err := ensureTreeCopyPathInfo(destinationRoot, name, createdInfo); err != nil {
		return err
	}
	mtime := expectedInfo.ModTime()
	atime, ok := treeCopyAccessTime(expectedInfo)
	if !ok {
		atime = mtime
	}
	if err := destinationRoot.Chtimes(name, atime, mtime); err != nil {
		return fmt.Errorf("set workspace file time %q: %w", name, err)
	}
	if err := ensureTreeCopyPathInfo(destinationRoot, name, createdInfo); err != nil {
		return err
	}
	*hardlinks = append(*hardlinks, treeCopyHardlink{sourceInfo: expectedInfo, destinationPath: name, destinationInfo: createdInfo,
		xattrs: xattrs, acl: acl})
	return nil
}

func finishTreeCopyDirectory(sourcePath, destinationPath string, sourceRoot, destinationRoot *os.Root, rootFile *os.File, name string,
	expectedInfo, sourceInfo os.FileInfo, xattrs []workspace.ExtendedAttribute, acl *workspace.NativeACL) error {
	destinationDirectory, err := destinationRoot.OpenRoot(name)
	if err != nil {
		return fmt.Errorf("open created workspace directory %q: %w", name, err)
	}
	defer func() { _ = destinationDirectory.Close() }()
	openedInfo, err := destinationDirectory.Stat(".")
	if err != nil || !os.SameFile(expectedInfo, openedInfo) {
		return fmt.Errorf("workspace destination directory %q changed during copy", name)
	}
	file := rootFile
	closeFile := false
	if file == nil {
		file, err = destinationDirectory.Open(".")
		if err != nil {
			return fmt.Errorf("open created workspace directory metadata %q: %w", name, err)
		}
		closeFile = true
	}
	if closeFile {
		defer func() { _ = file.Close() }()
	}
	atime, ok := treeCopyAccessTime(sourceInfo)
	if !ok {
		atime = sourceInfo.ModTime()
	}
	if err := destinationDirectory.Chtimes(".", atime, sourceInfo.ModTime()); err != nil {
		return fmt.Errorf("set workspace directory time %q: %w", name, err)
	}
	if err := ApplyOpenedTreeCopyMetadata(file, filepath.Join(destinationPath, filepath.FromSlash(name)), name, xattrs, acl); err != nil {
		return err
	}
	if err := file.Chmod(treeCopyPermissionMode(sourceInfo.Mode())); err != nil {
		return fmt.Errorf("set workspace directory mode %q: %w", name, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync workspace directory metadata %q: %w", name, err)
	}
	currentXattrs, currentACL, err := ReadTreeCopyMetadata(sourceRoot, sourcePath, name, false)
	if err != nil || !SameTreeCopyMetadata(xattrs, acl, currentXattrs, currentACL) {
		return errors.Join(fmt.Errorf("workspace directory %q metadata changed during copy", name), err)
	}
	return nil
}

func treeCopyPermissionMode(mode os.FileMode) os.FileMode {
	return mode.Perm()
}

func treeCopyCleanupError(name string, err error) error {
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("clean up workspace copy path %q: %w", name, err)
}

func treeCopyAccessTime(info os.FileInfo) (time.Time, bool) {
	value := reflect.ValueOf(info.Sys())
	if value.IsValid() && value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return time.Time{}, false
	}
	for _, name := range []string{"Atim", "Atimespec"} {
		timespec := value.FieldByName(name)
		if !timespec.IsValid() || timespec.Kind() != reflect.Struct {
			continue
		}
		seconds := timespec.FieldByName("Sec")
		nanoseconds := timespec.FieldByName("Nsec")
		if !seconds.IsValid() || !nanoseconds.IsValid() {
			continue
		}
		var sec, nsec int64
		if seconds.CanInt() && nanoseconds.CanInt() {
			sec, nsec = seconds.Int(), nanoseconds.Int()
		} else if seconds.CanUint() && nanoseconds.CanUint() {
			sec, nsec = int64(seconds.Uint()), int64(nanoseconds.Uint())
		} else {
			continue
		}
		return time.Unix(sec, nsec), true
	}
	return time.Time{}, false
}

func validateTreeCopySymlink(name, target string) error {
	if target == "" || len(target) > 4096 || !utf8.ValidString(target) || strings.ContainsAny(target, "\x00\\:") ||
		strings.HasPrefix(target, "/") || path.IsAbs(target) || filepath.VolumeName(target) != "" {
		return fmt.Errorf("workspace tree copy rejects absolute or escaping symlink %q", name)
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	if resolved == ".." || path.IsAbs(resolved) || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("workspace tree copy rejects absolute or escaping symlink %q", name)
	}
	return nil
}

func ensureTreeCopyPathInfo(root *os.Root, name string, expected os.FileInfo) error {
	current, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("inspect copied workspace path %q: %w", name, err)
	}
	if !os.SameFile(expected, current) {
		return fmt.Errorf("workspace destination path %q changed during copy", name)
	}
	return nil
}

func readTreeCopyDirectory(root *os.Root, name string) ([]os.DirEntry, error) {
	directory, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = directory.Close() }()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	return entries, nil
}
