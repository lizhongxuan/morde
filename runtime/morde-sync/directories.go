package mordesync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"
)

// ValidateRelativePath exposes the scanner's portable relative-path rules to
// transport adapters that validate directory metadata before filesystem use.
func ValidateRelativePath(value string) error { return validRelativePath(value) }

// InDirectoryScope reports whether path is in, or is an ancestor of, a sync
// scope. Ancestors are included so clients can safely traverse scoped paths.
func InDirectoryScope(path string, scope []string) bool {
	if len(scope) == 0 {
		return true
	}
	path = strings.ToLower(path)
	for _, candidate := range scope {
		candidate = strings.ToLower(candidate)
		if path == candidate || strings.HasPrefix(path, candidate+"/") || strings.HasPrefix(candidate, path+"/") {
			return true
		}
	}
	return false
}

// EnsureEmptyDirectory creates a path only if it is absent or already empty.
func EnsureEmptyDirectory(ctx context.Context, workspacePath, relative string) (ApplyResult, error) {
	if ctx == nil {
		return ApplyResult{}, errors.New("context is required")
	}
	if err := acquireOperation(ctx); err != nil {
		return ApplyResult{}, err
	}
	defer func() { <-operationSlots }()
	if err := validRelativePath(relative); err != nil {
		return ApplyResult{}, err
	}
	root, err := openDirectoryRoot(workspacePath)
	if err != nil {
		return ApplyResult{}, err
	}
	defer root.Close()
	if err := checkUnsupportedMetadataParents(root, relative); err != nil {
		return ApplyResult{}, err
	}
	parent, name, closeParent, err := openParent(root, relative, true)
	if err != nil {
		return ApplyResult{}, err
	}
	defer closeParent()
	info, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if err := ctx.Err(); err != nil {
			return ApplyResult{}, err
		}
		if err := parent.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return ApplyResult{}, err
		}
		child, openErr := parent.OpenRoot(name)
		if openErr != nil {
			return ApplyResult{}, openErr
		}
		opened, openErr := child.Open(".")
		if openErr != nil {
			_ = child.Close()
			return ApplyResult{}, openErr
		}
		metadataErr := checkUnsupportedMetadataFD(opened, relative)
		_ = opened.Close()
		_ = child.Close()
		if metadataErr != nil {
			return ApplyResult{}, metadataErr
		}
		if err := syncDirectory(parent); err != nil {
			return ApplyResult{Status: ApplyWritten}, err
		}
		return ApplyResult{Status: ApplyWritten}, nil
	}
	if err != nil {
		return ApplyResult{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ApplyResult{Status: ApplyConflict}, nil
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return ApplyResult{}, err
	}
	defer child.Close()
	opened, err := child.Open(".")
	if err != nil {
		return ApplyResult{}, err
	}
	defer opened.Close()
	openedInfo, err := opened.Stat()
	if err != nil || !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
		return ApplyResult{}, ErrDestinationChange
	}
	if err := checkUnsupportedMetadataFD(opened, relative); err != nil {
		return ApplyResult{}, err
	}
	entries, err := opened.ReadDir(-1)
	if err != nil {
		return ApplyResult{}, err
	}
	if len(entries) != 0 {
		return ApplyResult{Status: ApplyConflict}, nil
	}
	return ApplyResult{Status: ApplyPresent}, nil
}

// RemoveEmptyDirectory removes a directory only when it is still empty.
func RemoveEmptyDirectory(ctx context.Context, workspacePath, relative string) (ApplyResult, error) {
	if ctx == nil {
		return ApplyResult{}, errors.New("context is required")
	}
	if err := acquireOperation(ctx); err != nil {
		return ApplyResult{}, err
	}
	defer func() { <-operationSlots }()
	if err := validRelativePath(relative); err != nil {
		return ApplyResult{}, err
	}
	root, err := openDirectoryRoot(workspacePath)
	if err != nil {
		return ApplyResult{}, err
	}
	defer root.Close()
	if err := checkUnsupportedMetadataParents(root, relative); err != nil {
		return ApplyResult{}, err
	}
	parent, name, closeParent, err := openParent(root, relative, false)
	if errors.Is(err, fs.ErrNotExist) {
		return ApplyResult{Status: ApplyPresent}, nil
	}
	if errors.Is(err, ErrPathConflict) {
		return ApplyResult{Status: ApplyConflict}, nil
	}
	if err != nil {
		return ApplyResult{}, err
	}
	defer closeParent()
	info, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return ApplyResult{Status: ApplyPresent}, nil
	}
	if err != nil {
		return ApplyResult{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ApplyResult{Status: ApplyConflict}, nil
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return ApplyResult{}, err
	}
	defer child.Close()
	opened, err := child.Open(".")
	if err != nil {
		return ApplyResult{}, err
	}
	defer opened.Close()
	openedInfo, err := opened.Stat()
	if err != nil || !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
		return ApplyResult{}, ErrDestinationChange
	}
	if err := checkUnsupportedMetadataFD(opened, relative); err != nil {
		return ApplyResult{}, err
	}
	entries, err := opened.ReadDir(-1)
	if err != nil {
		return ApplyResult{}, err
	}
	if len(entries) != 0 {
		return ApplyResult{Status: ApplyConflict}, nil
	}
	if err := ctx.Err(); err != nil {
		return ApplyResult{}, err
	}
	current, err := parent.Lstat(name)
	if err != nil || !os.SameFile(info, current) {
		return ApplyResult{}, ErrDestinationChange
	}
	if err := parent.Remove(name); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ApplyResult{Status: ApplyPresent}, nil
		}
		return ApplyResult{}, err
	}
	if err := syncDirectory(parent); err != nil {
		return ApplyResult{Status: ApplyWritten}, err
	}
	return ApplyResult{Status: ApplyWritten}, nil
}

// ApplyDirectoryMetadata applies a server directory snapshot only when the
// local directory still matches the scan taken before the sync session.
// expected=nil means the directory was absent at that scan and may be created.
func ApplyDirectoryMetadata(ctx context.Context, workspacePath string, expected *Directory, desired Directory, allowCurrent bool) (ApplyResult, error) {
	if ctx == nil {
		return ApplyResult{}, errors.New("context is required")
	}
	if desired.Path != "." && validRelativePath(desired.Path) != nil || desired.ModeKnown && desired.Mode&^uint32(0o777) != 0 ||
		validateMetadata(desired.Xattrs, desired.ACL) != nil || !desired.MetadataKnown && (len(desired.Xattrs) != 0 || desired.ACL != nil) {
		return ApplyResult{}, ErrInvalidManifest
	}
	if expected != nil && (expected.Path != desired.Path || validateMetadata(expected.Xattrs, expected.ACL) != nil ||
		!expected.MetadataKnown && (len(expected.Xattrs) != 0 || expected.ACL != nil)) {
		return ApplyResult{}, ErrInvalidManifest
	}
	root, err := openDirectoryRoot(workspacePath)
	if err != nil {
		return ApplyResult{}, err
	}
	defer root.Close()
	if err := checkUnsupportedMetadataParents(root, desired.Path); err != nil {
		return ApplyResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ApplyResult{}, err
	}

	parent, name := root, desired.Path
	closeParent := func() {}
	rootDirectory := desired.Path == "."
	if !rootDirectory {
		parent, name, closeParent, err = openParent(root, desired.Path, true)
		if err != nil {
			if errors.Is(err, ErrPathConflict) || errors.Is(err, ErrUnsafePath) || errors.Is(err, fs.ErrNotExist) {
				return ApplyResult{Status: ApplyConflict}, nil
			}
			return ApplyResult{}, err
		}
	}
	defer closeParent()
	info, err := parent.Lstat(name)
	created := false
	if errors.Is(err, fs.ErrNotExist) {
		if expected != nil || rootDirectory {
			return ApplyResult{Status: ApplyConflict}, nil
		}
		if err := parent.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return ApplyResult{}, err
		} else if err == nil {
			created = true
			if err := syncDirectory(parent); err != nil {
				return ApplyResult{Status: ApplyWritten}, err
			}
		}
		info, err = parent.Lstat(name)
	}
	if err != nil {
		return ApplyResult{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ApplyResult{Status: ApplyConflict}, nil
	}
	child := parent
	closeChild := func() {}
	if !rootDirectory {
		child, err = parent.OpenRoot(name)
		if err != nil {
			return ApplyResult{}, err
		}
		closeChild = func() { _ = child.Close() }
	}
	defer closeChild()
	opened, err := child.Open(".")
	if err != nil {
		return ApplyResult{}, err
	}
	defer opened.Close()
	openedInfo, err := opened.Stat()
	if err != nil || !os.SameFile(info, openedInfo) || !openedInfo.IsDir() {
		return ApplyResult{Status: ApplyConflict}, nil
	}
	if err := checkUnsupportedMetadataFD(opened, desired.Path); err != nil {
		return ApplyResult{}, err
	}
	metadataMatches, err := metadataMatchesFD(opened, displayMetadataPath(desired.Path), desired.Xattrs, desired.ACL, desired.MetadataKnown)
	if err != nil {
		return ApplyResult{}, err
	}
	desiredMatches := (!desired.ModeKnown || info.Mode().Perm() == os.FileMode(desired.Mode)) &&
		(!desired.ModTimeKnown || info.ModTime().UnixNano() == desired.ModTimeNS) && metadataMatches
	if desiredMatches {
		if created {
			return ApplyResult{Status: ApplyWritten}, nil
		}
		return ApplyResult{Status: ApplyPresent}, nil
	}
	if expected == nil && !allowCurrent {
		return ApplyResult{Status: ApplyConflict}, nil
	}
	if expected != nil {
		expectedMetadataMatches, metadataErr := metadataMatchesFD(opened, displayMetadataPath(expected.Path), expected.Xattrs, expected.ACL, expected.MetadataKnown)
		if metadataErr != nil {
			return ApplyResult{}, metadataErr
		}
		if expected.ModeKnown && info.Mode().Perm() != os.FileMode(expected.Mode) ||
			expected.ModTimeKnown && info.ModTime().UnixNano() != expected.ModTimeNS || !expectedMetadataMatches {
			return ApplyResult{Status: ApplyConflict}, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return ApplyResult{}, err
	}
	if desired.ModeKnown {
		if err := opened.Chmod(os.FileMode(desired.Mode)); err != nil {
			return ApplyResult{}, err
		}
	}
	if desired.MetadataKnown {
		if err := applyMetadataFD(opened, displayMetadataPath(desired.Path), desired.Xattrs, desired.ACL, true); err != nil {
			return ApplyResult{}, err
		}
	} else {
		metadataMatches, err = metadataMatchesFD(opened, displayMetadataPath(desired.Path), desired.Xattrs, desired.ACL, false)
		if err != nil {
			return ApplyResult{}, err
		}
		if !metadataMatches {
			return ApplyResult{Status: ApplyConflict}, nil
		}
	}
	if desired.ModTimeKnown {
		modified := time.Unix(0, desired.ModTimeNS)
		if err := child.Chtimes(".", modified, modified); err != nil {
			return ApplyResult{}, err
		}
	}
	if err := opened.Sync(); err != nil {
		return ApplyResult{}, err
	}
	finalInfo, err := opened.Stat()
	if err != nil || !os.SameFile(info, finalInfo) || !finalInfo.IsDir() ||
		desired.ModeKnown && finalInfo.Mode().Perm() != os.FileMode(desired.Mode) ||
		desired.ModTimeKnown && finalInfo.ModTime().UnixNano() != desired.ModTimeNS {
		return ApplyResult{}, errors.Join(ErrDestinationChange, err)
	}
	metadataMatches, err = metadataMatchesFD(opened, displayMetadataPath(desired.Path), desired.Xattrs, desired.ACL, desired.MetadataKnown)
	if err != nil {
		return ApplyResult{}, err
	}
	if !metadataMatches {
		return ApplyResult{}, fmt.Errorf("%w: restored directory metadata on %q did not match", ErrDestinationChange, desired.Path)
	}
	return ApplyResult{Status: ApplyWritten}, nil
}

// DirectoryMetadataMap returns a case-folded index used by sync clients.
func DirectoryMetadataMap(values []Directory) map[string]Directory {
	result := make(map[string]Directory, len(values))
	for _, value := range values {
		result[strings.ToLower(value.Path)] = value
	}
	return result
}

func sortDirectories(values []Directory) {
	sort.Slice(values, func(i, j int) bool {
		if values[i].Path == "." {
			return false
		}
		if values[j].Path == "." {
			return true
		}
		return values[i].Path < values[j].Path
	})
}
