package mordesync

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"
)

var ErrContentMismatch = errors.New("transferred content does not match manifest")

// ApplyMissing installs a file only when its destination path is absent. It
// validates every block and the whole-file hash before an atomic no-replace
// link; an existing different file is returned as a conflict and never changed.
func ApplyMissing(ctx context.Context, directory string, file File, source io.Reader) (ApplyResult, error) {
	if ctx == nil || source == nil && file.Kind != FileKindSymlink && file.HardlinkTo == "" {
		return ApplyResult{}, errors.New("context and source are required")
	}
	if err := acquireOperation(ctx); err != nil {
		return ApplyResult{}, err
	}
	defer func() { <-operationSlots }()
	if err := validateFile(file); err != nil {
		return ApplyResult{}, err
	}
	root, err := openDirectoryRoot(directory)
	if err != nil {
		return ApplyResult{}, err
	}
	defer root.Close()
	if err := checkUnsupportedMetadataParents(root, file.Path); err != nil {
		return ApplyResult{}, err
	}

	status, existingHash, exists, err := classifyExisting(ctx, root, file)
	if err != nil {
		return ApplyResult{}, err
	}
	if exists {
		if file.Kind == FileKindSymlink && status == ApplyPresent {
			return applyExistingSymlinkMetadata(root, file.Path, file)
		}
		return ApplyResult{Status: status, ExistingSHA256: existingHash}, nil
	}
	if file.HardlinkTo != "" {
		return applyMissingHardlink(ctx, root, file)
	}

	parent, name, closeParent, err := openParent(root, file.Path, true)
	if errors.Is(err, ErrPathConflict) {
		return ApplyResult{Status: ApplyConflict}, nil
	}
	if err != nil {
		return ApplyResult{}, fmt.Errorf("prepare destination %q: %w", file.Path, err)
	}
	defer closeParent()
	status, existingHash, exists, err = classifyAt(ctx, root, parent, name, file)
	if err != nil {
		return ApplyResult{}, err
	}
	if exists {
		if file.Kind == FileKindSymlink && status == ApplyPresent {
			return applyExistingSymlinkMetadata(root, file.Path, file)
		}
		return ApplyResult{Status: status, ExistingSHA256: existingHash}, nil
	}
	if file.Kind == FileKindSymlink {
		if err := parent.Symlink(file.SymlinkTarget, name); err != nil {
			if errors.Is(err, fs.ErrExist) {
				status, existingHash, exists, classifyErr := classifyAt(ctx, root, parent, name, file)
				if classifyErr == nil && exists {
					if file.Kind == FileKindSymlink && status == ApplyPresent {
						return applyExistingSymlinkMetadata(root, file.Path, file)
					}
					return ApplyResult{Status: status, ExistingSHA256: existingHash}, nil
				}
				if classifyErr != nil {
					return ApplyResult{}, classifyErr
				}
			}
			return ApplyResult{}, fmt.Errorf("install symlink without replacement: %w", err)
		}
		created, err := parent.Lstat(name)
		if err != nil || created.Mode()&os.ModeSymlink == 0 {
			return ApplyResult{Status: ApplyWritten}, errors.Join(errors.New("installed Workspace symlink changed"), err)
		}
		if err := applySymlinkMetadataPath(root, file); err != nil {
			current, statErr := parent.Lstat(name)
			var cleanupErr error
			if statErr == nil && os.SameFile(created, current) {
				cleanupErr = parent.Remove(name)
			} else if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
				cleanupErr = statErr
			} else if statErr == nil {
				cleanupErr = ErrDestinationChange
			}
			return ApplyResult{}, errors.Join(err, cleanupErr)
		}
		if file.ModTimeKnown {
			modified := time.Unix(0, file.ModTimeNS)
			if err := setSymlinkTimes(parent, name, modified); err != nil {
				current, statErr := parent.Lstat(name)
				var cleanupErr error
				switch {
				case statErr == nil && os.SameFile(created, current):
					cleanupErr = parent.Remove(name)
				case errors.Is(statErr, fs.ErrNotExist):
				case statErr != nil:
					cleanupErr = statErr
				default:
					cleanupErr = ErrDestinationChange
				}
				if cleanupErr != nil {
					return ApplyResult{Status: ApplyWritten}, errors.Join(fmt.Errorf("set symlink modification time: %w", err), cleanupErr)
				}
				return ApplyResult{}, fmt.Errorf("set symlink modification time: %w", err)
			}
		}
		target, readErr := parent.Readlink(name)
		current, statErr := parent.Lstat(name)
		if readErr != nil || statErr != nil || !os.SameFile(created, current) || target != file.SymlinkTarget ||
			file.ModTimeKnown && current.ModTime().UnixNano() != file.ModTimeNS {
			return ApplyResult{Status: ApplyWritten}, errors.Join(ErrDestinationChange, readErr, statErr)
		}
		if err := syncDirectory(parent); err != nil {
			return ApplyResult{Status: ApplyWritten}, fmt.Errorf("sync destination directory: %w", err)
		}
		return ApplyResult{Status: ApplyWritten}, nil
	}

	tempName, err := temporaryName()
	if err != nil {
		return ApplyResult{}, err
	}
	temporary, err := parent.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("create staging file: %w", err)
	}
	tempExists := true
	defer func() {
		_ = temporary.Close()
		if tempExists {
			_ = parent.Remove(tempName)
		}
	}()

	if err := writeAndVerify(ctx, temporary, file, source); err != nil {
		return ApplyResult{}, err
	}
	if err := temporary.Chmod(os.FileMode(file.Mode)); err != nil {
		return ApplyResult{}, fmt.Errorf("set staged file mode: %w", err)
	}
	if err := applyMetadataFD(temporary, file.Path, file.Xattrs, file.ACL, file.MetadataKnown); err != nil {
		return ApplyResult{}, err
	}
	if file.ModTimeKnown {
		modified := time.Unix(0, file.ModTimeNS)
		if err := parent.Chtimes(tempName, modified, modified); err != nil {
			return ApplyResult{}, fmt.Errorf("set staged file modification time: %w", err)
		}
	}
	if err := temporary.Sync(); err != nil {
		return ApplyResult{}, fmt.Errorf("sync staged file: %w", err)
	}
	if err := checkUnsupportedMetadataFD(temporary, file.Path); err != nil {
		return ApplyResult{}, err
	}
	if err := temporary.Close(); err != nil {
		return ApplyResult{}, fmt.Errorf("close staged file: %w", err)
	}
	if err := parent.Link(tempName, name); err != nil {
		if errors.Is(err, fs.ErrExist) {
			status, existingHash, exists, classifyErr := classifyAt(ctx, root, parent, name, file)
			if classifyErr == nil && exists {
				return ApplyResult{Status: status, ExistingSHA256: existingHash}, nil
			}
			if classifyErr != nil {
				return ApplyResult{}, classifyErr
			}
		}
		return ApplyResult{}, fmt.Errorf("install staged file without replacement: %w", err)
	}
	if err := parent.Remove(tempName); err != nil {
		tempExists = false
		return ApplyResult{Status: ApplyWritten}, fmt.Errorf("remove staging link: %w", err)
	}
	tempExists = false
	if err := syncDirectory(parent); err != nil {
		return ApplyResult{Status: ApplyWritten}, fmt.Errorf("sync destination directory: %w", err)
	}
	return ApplyResult{Status: ApplyWritten}, nil
}

func writeAndVerify(ctx context.Context, output *os.File, file File, source io.Reader) error {
	whole := sha256.New()
	reader := contextReader{ctx: ctx, reader: source}
	for index, block := range file.Blocks {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := withBlockBuffer(ctx, func(buffer []byte) error {
			chunk := buffer[:int(block.Size)]
			if _, err := readFullContext(ctx, reader, chunk); err != nil {
				if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
					return fmt.Errorf("read transferred block %d: %w", index, ErrContentMismatch)
				}
				return fmt.Errorf("read transferred block %d: %w", index, err)
			}
			if sumHex(chunk) != block.SHA256 {
				return fmt.Errorf("transferred block %d: %w", index, ErrContentMismatch)
			}
			_, _ = whole.Write(chunk)
			return writeAll(output, chunk)
		})
		if err != nil {
			return err
		}
	}
	var extra [1]byte
	n, err := readFullContext(ctx, reader, extra[:])
	if n != 0 {
		return ErrContentMismatch
	}
	if !errors.Is(err, io.EOF) {
		if err == nil {
			return ErrContentMismatch
		}
		return fmt.Errorf("check transferred file boundary: %w", err)
	}
	if hex.EncodeToString(whole.Sum(nil)) != file.SHA256 {
		return ErrContentMismatch
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func classifyExisting(ctx context.Context, root *os.Root, file File) (ApplyStatus, string, bool, error) {
	parent, name, closeParent, err := openParent(root, file.Path, false)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", false, nil
	}
	if errors.Is(err, ErrPathConflict) {
		return ApplyConflict, "", true, nil
	}
	if err != nil {
		return "", "", false, err
	}
	defer closeParent()
	return classifyAt(ctx, root, parent, name, file)
}

func classifyAt(ctx context.Context, root, parent *os.Root, name string, file File) (ApplyStatus, string, bool, error) {
	info, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	if file.Kind == FileKindSymlink {
		if info.Mode()&os.ModeSymlink == 0 {
			return ApplyConflict, "", true, nil
		}
		if err := checkSymlinkMetadata(root, file.Path, file.Path); err != nil {
			return "", "", false, err
		}
		target, err := parent.Readlink(name)
		current, statErr := parent.Lstat(name)
		if err != nil || statErr != nil || !os.SameFile(info, current) || current.Mode()&os.ModeSymlink == 0 ||
			current.Size() != info.Size() || current.ModTime() != info.ModTime() {
			return "", "", false, errors.Join(ErrDestinationChange, err, statErr)
		}
		metadataMatches, metadataErr := metadataMatchesSymlinkRoot(parent, name, file.Path, file.Xattrs, file.ACL, file.MetadataKnown)
		if metadataErr != nil {
			return "", "", false, metadataErr
		}
		if !metadataMatches {
			return ApplyConflict, sumHex([]byte(target)), true, nil
		}
		latestTarget, latestTargetErr := parent.Readlink(name)
		latest, latestErr := parent.Lstat(name)
		if latestTargetErr != nil || latestErr != nil || !os.SameFile(info, latest) || latestTarget != target || latest.ModTime() != current.ModTime() {
			return "", "", false, errors.Join(ErrDestinationChange, latestTargetErr, latestErr)
		}
		digest := sumHex([]byte(target))
		if target == file.SymlinkTarget && digest == file.SHA256 {
			return ApplyPresent, digest, true, nil
		}
		return ApplyConflict, digest, true, nil
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ApplyConflict, "", true, nil
	}
	if file.HardlinkTo != "" {
		anchor, err := lstatPath(root, file.HardlinkTo)
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", false, ErrHardlinkAnchor
		}
		if err != nil {
			return "", "", false, fmt.Errorf("inspect hard-link anchor %q: %w", file.HardlinkTo, err)
		}
		if !anchor.Mode().IsRegular() || !os.SameFile(info, anchor) || !hardlinkMetadataMatches(info, file) {
			return ApplyConflict, "", true, nil
		}
	}
	opened, err := parent.Open(name)
	if err != nil {
		return "", "", false, fmt.Errorf("open existing destination: %w", err)
	}
	defer opened.Close()
	openedInfo, err := opened.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return "", "", false, ErrDestinationChange
	}
	if err := checkUnsupportedMetadataFD(opened, file.Path); err != nil {
		return "", "", false, err
	}
	metadataMatches, metadataErr := metadataMatchesFD(opened, file.Path, file.Xattrs, file.ACL, file.MetadataKnown)
	if metadataErr != nil {
		return "", "", false, metadataErr
	}
	if !metadataMatches {
		return ApplyConflict, "", true, nil
	}
	hasher := sha256.New()
	err = withBlockBuffer(ctx, func(buffer []byte) error {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, readErr := opened.Read(buffer)
			if n > 0 {
				_, _ = hasher.Write(buffer[:n])
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return readErr
			}
			if n == 0 {
				return io.ErrNoProgress
			}
		}
	})
	if err != nil {
		return "", "", false, fmt.Errorf("hash existing destination: %w", err)
	}
	after, statErr := opened.Stat()
	current, pathErr := lstatPath(root, file.Path)
	if statErr != nil || pathErr != nil || !os.SameFile(info, after) || !os.SameFile(info, current) ||
		after.Size() != info.Size() || after.ModTime() != info.ModTime() || current.Size() != info.Size() || current.ModTime() != info.ModTime() {
		return "", "", false, ErrDestinationChange
	}
	if err := checkUnsupportedMetadataFD(opened, file.Path); err != nil {
		return "", "", false, err
	}
	metadataMatches, metadataErr = metadataMatchesFD(opened, file.Path, file.Xattrs, file.ACL, file.MetadataKnown)
	if metadataErr != nil {
		return "", "", false, metadataErr
	}
	if !metadataMatches {
		return ApplyConflict, "", true, nil
	}
	if file.HardlinkTo != "" {
		anchor, anchorErr := lstatPath(root, file.HardlinkTo)
		if anchorErr != nil || !os.SameFile(info, anchor) || !hardlinkMetadataMatches(current, file) {
			return "", "", false, errors.Join(ErrDestinationChange, anchorErr)
		}
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if digest == file.SHA256 {
		return ApplyPresent, digest, true, nil
	}
	return ApplyConflict, digest, true, nil
}

func applyMissingHardlink(ctx context.Context, root *os.Root, file File) (ApplyResult, error) {
	anchorFile := file
	anchorFile.Path = file.HardlinkTo
	anchorFile.HardlinkTo = ""
	status, _, exists, err := classifyExisting(ctx, root, anchorFile)
	if err != nil || !exists || status != ApplyPresent {
		return ApplyResult{}, errors.Join(ErrHardlinkAnchor, err)
	}
	anchorInfo, err := lstatPath(root, file.HardlinkTo)
	if err != nil || !anchorInfo.Mode().IsRegular() || !hardlinkMetadataMatches(anchorInfo, file) {
		return ApplyResult{}, errors.Join(ErrHardlinkAnchor, err)
	}

	parent, name, closeParent, err := openParent(root, file.Path, true)
	if errors.Is(err, ErrPathConflict) {
		return ApplyResult{Status: ApplyConflict}, nil
	}
	if err != nil {
		return ApplyResult{}, fmt.Errorf("prepare hard-link destination %q: %w", file.Path, err)
	}
	defer closeParent()
	status, existingHash, exists, err := classifyAt(ctx, root, parent, name, file)
	if err != nil {
		return ApplyResult{}, err
	}
	if exists {
		return ApplyResult{Status: status, ExistingSHA256: existingHash}, nil
	}
	if err := root.Link(file.HardlinkTo, file.Path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			status, existingHash, exists, classifyErr := classifyAt(ctx, root, parent, name, file)
			if classifyErr != nil {
				return ApplyResult{}, classifyErr
			}
			if exists {
				return ApplyResult{Status: status, ExistingSHA256: existingHash}, nil
			}
		}
		return ApplyResult{}, fmt.Errorf("install workspace hard link without replacement: %w", err)
	}
	created, statErr := parent.Lstat(name)
	anchorAfter, anchorErr := lstatPath(root, file.HardlinkTo)
	if statErr != nil || anchorErr != nil || !created.Mode().IsRegular() || !os.SameFile(created, anchorAfter) {
		var cleanupErr error
		if statErr == nil {
			current, currentErr := parent.Lstat(name)
			if currentErr == nil && os.SameFile(created, current) {
				cleanupErr = parent.Remove(name)
			} else if currentErr != nil && !errors.Is(currentErr, fs.ErrNotExist) {
				cleanupErr = currentErr
			} else if currentErr == nil {
				cleanupErr = ErrDestinationChange
			}
		}
		return ApplyResult{}, errors.Join(ErrDestinationChange, statErr, anchorErr, cleanupErr)
	}
	status, _, exists, err = classifyAt(ctx, root, parent, name, file)
	if err != nil || !exists || status != ApplyPresent {
		current, currentErr := parent.Lstat(name)
		var cleanupErr error
		if currentErr == nil && os.SameFile(created, current) {
			cleanupErr = parent.Remove(name)
		} else if currentErr != nil && !errors.Is(currentErr, fs.ErrNotExist) {
			cleanupErr = currentErr
		} else if currentErr == nil {
			cleanupErr = ErrDestinationChange
		}
		return ApplyResult{}, errors.Join(ErrDestinationChange, err, cleanupErr)
	}
	if err := syncDirectory(parent); err != nil {
		return ApplyResult{Status: ApplyWritten}, fmt.Errorf("sync hard-link destination directory: %w", err)
	}
	return ApplyResult{Status: ApplyWritten}, nil
}

func hardlinkMetadataMatches(info os.FileInfo, file File) bool {
	return uint32(info.Mode().Perm()) == file.Mode && (!file.ModTimeKnown || info.ModTime().UnixNano() == file.ModTimeNS)
}

func applyExistingSymlinkMetadata(root *os.Root, relative string, file File) (ApplyResult, error) {
	parent, name, closeParent, err := openParent(root, relative, false)
	if err != nil {
		return ApplyResult{}, err
	}
	defer closeParent()
	info, err := parent.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return ApplyResult{}, errors.Join(ErrDestinationChange, err)
	}
	target, err := parent.Readlink(name)
	current, statErr := parent.Lstat(name)
	if err != nil || statErr != nil || !os.SameFile(info, current) || current.Mode()&os.ModeSymlink == 0 || target != file.SymlinkTarget {
		return ApplyResult{}, errors.Join(ErrDestinationChange, err, statErr)
	}
	metadataMatches, metadataErr := metadataMatchesSymlinkRoot(parent, name, relative, file.Xattrs, file.ACL, file.MetadataKnown)
	if metadataErr != nil {
		return ApplyResult{}, metadataErr
	}
	if !metadataMatches {
		return ApplyResult{Status: ApplyConflict}, nil
	}
	if file.ModTimeKnown && current.ModTime().UnixNano() != file.ModTimeNS {
		if err := setSymlinkTimes(parent, name, time.Unix(0, file.ModTimeNS)); err != nil {
			return ApplyResult{}, fmt.Errorf("set existing symlink modification time: %w", err)
		}
		updated, err := parent.Lstat(name)
		latest, targetErr := parent.Readlink(name)
		if err != nil || targetErr != nil || !os.SameFile(info, updated) || latest != file.SymlinkTarget || updated.ModTime().UnixNano() != file.ModTimeNS {
			return ApplyResult{}, errors.Join(ErrDestinationChange, err, targetErr)
		}
		if err := syncDirectory(parent); err != nil {
			return ApplyResult{Status: ApplyWritten}, fmt.Errorf("sync destination directory: %w", err)
		}
		return ApplyResult{Status: ApplyWritten}, nil
	}
	return ApplyResult{Status: ApplyPresent, ExistingSHA256: file.SHA256}, nil
}

// VerifyExistingSymlink verifies and, when requested, timestamps an already
// present Workspace link without resolving its target.
func VerifyExistingSymlink(ctx context.Context, directory string, file File) error {
	if ctx == nil || file.Kind != FileKindSymlink || validateFile(file) != nil {
		return ErrInvalidManifest
	}
	if err := acquireOperation(ctx); err != nil {
		return err
	}
	defer func() { <-operationSlots }()
	root, err := openDirectoryRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	parent, name, closeParent, err := openParent(root, file.Path, false)
	if err != nil {
		return err
	}
	defer closeParent()
	info, err := parent.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return errors.Join(ErrDestinationChange, err)
	}
	target, err := parent.Readlink(name)
	current, statErr := parent.Lstat(name)
	if err != nil || statErr != nil || !os.SameFile(info, current) || target != file.SymlinkTarget || sumHex([]byte(target)) != file.SHA256 ||
		int64(len([]byte(target))) != file.Size {
		return errors.Join(ErrDestinationChange, err, statErr)
	}
	if file.ModTimeKnown && current.ModTime().UnixNano() != file.ModTimeNS {
		if err := setSymlinkTimes(parent, name, time.Unix(0, file.ModTimeNS)); err != nil {
			return fmt.Errorf("set Workspace symlink modification time: %w", err)
		}
		updated, err := parent.Lstat(name)
		latestTarget, readErr := parent.Readlink(name)
		if err != nil || readErr != nil || !os.SameFile(info, updated) || latestTarget != file.SymlinkTarget || updated.ModTime().UnixNano() != file.ModTimeNS {
			return errors.Join(ErrDestinationChange, err, readErr)
		}
	}
	return syncDirectory(parent)
}

func temporaryName() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate staging name: %w", err)
	}
	return reservedTempPrefix + hex.EncodeToString(random[:]), nil
}

func syncDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func readFullContext(ctx context.Context, reader io.Reader, destination []byte) (int, error) {
	read := 0
	withoutProgress := 0
	for read < len(destination) {
		if err := ctx.Err(); err != nil {
			return read, err
		}
		n, err := reader.Read(destination[read:])
		if n < 0 || n > len(destination)-read {
			return read, ErrContentMismatch
		}
		read += n
		if read == len(destination) {
			return read, nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if read == 0 {
					return read, io.EOF
				}
				return read, io.ErrUnexpectedEOF
			}
			return read, err
		}
		if n == 0 {
			withoutProgress++
			if withoutProgress >= 100 {
				return read, io.ErrNoProgress
			}
		} else {
			withoutProgress = 0
		}
	}
	return read, nil
}
