//go:build linux

package workspacecurrent

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func openWorkspaceCurrentParent(root, relativePath string, createParents bool) (*os.File, string, error) {
	parts := strings.Split(filepath.ToSlash(relativePath), "/")
	if len(parts) < 3 {
		return nil, "", ErrUnsafePath
	}
	workspaceDir, err := openWorkspaceCurrentFile(root, filepath.Join(parts[0], parts[1]))
	if err != nil {
		return nil, "", err
	}
	parentFD, err := unix.Dup(int(workspaceDir.Fd()))
	closeErr := workspaceDir.Close()
	if err != nil || closeErr != nil {
		if err == nil {
			err = closeErr
		}
		return nil, "", err
	}
	for _, component := range parts[2 : len(parts)-1] {
		if component == "" || component == "." || component == ".." {
			_ = unix.Close(parentFD)
			return nil, "", ErrUnsafePath
		}
		nextFD, openErr := unix.Openat(parentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) && createParents {
			if mkdirErr := unix.Mkdirat(parentFD, component, 0750); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(parentFD)
				return nil, "", mkdirErr
			}
			if syncErr := unix.Fsync(parentFD); syncErr != nil {
				_ = unix.Close(parentFD)
				return nil, "", syncErr
			}
			nextFD, openErr = unix.Openat(parentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			_ = unix.Close(parentFD)
			if errors.Is(openErr, unix.ELOOP) || errors.Is(openErr, unix.ENOTDIR) {
				return nil, "", ErrUnsafePath
			}
			return nil, "", openErr
		}
		if err := unix.Close(parentFD); err != nil {
			_ = unix.Close(nextFD)
			return nil, "", err
		}
		parentFD = nextFD
	}
	return os.NewFile(uintptr(parentFD), parts[len(parts)-2]), parts[len(parts)-1], nil
}

func createWorkspaceCurrentDirectory(root, relativePath string, mode uint32) error {
	if mode > 0777 {
		return ErrUnsafePath
	}
	parent, name, err := openWorkspaceCurrentParent(root, relativePath, true)
	if err != nil {
		return err
	}
	defer parent.Close()
	err = unix.Mkdirat(int(parent.Fd()), name, mode&0777)
	if errors.Is(err, unix.EEXIST) {
		var stat unix.Stat_t
		if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			return ErrWriteConflict
		}
		return nil
	}
	if err != nil {
		return err
	}
	return unix.Fsync(int(parent.Fd()))
}

func removeWorkspaceCurrentEmptyDirectory(root, relativePath string) error {
	parent, name, err := openWorkspaceCurrentParent(root, relativePath, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrUnsafePath
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
			return ErrWriteConflict
		}
		return err
	}
	return unix.Fsync(int(parent.Fd()))
}

func removeWorkspaceCurrentRegularFile(root, relativePath, expectedSHA256 string) error {
	parent, name, err := openWorkspaceCurrentParent(root, relativePath, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	return removeWorkspaceCurrentRegularFileAt(int(parent.Fd()), name, expectedSHA256)
}

func removeWorkspaceCurrentRegularFileAt(parentFD int, name, expectedSHA256 string) error {
	temporary, err := workspaceCurrentTemporaryName(".morde-delete-")
	if err != nil {
		return err
	}
	if err := unix.Renameat2(parentFD, name, parentFD, temporary, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	verifyErr := verifyExpectedWorkspaceFile(parentFD, temporary, expectedSHA256)
	if verifyErr != nil {
		restoreErr := unix.Renameat2(parentFD, temporary, parentFD, name, unix.RENAME_NOREPLACE)
		if restoreErr != nil {
			return errors.Join(ErrWriteConflict, verifyErr,
				fmt.Errorf("changed Workspace file is preserved at %q because restoration failed: %w", temporary, restoreErr))
		}
		if err := unix.Fsync(parentFD); err != nil {
			return errors.Join(ErrWriteConflict, verifyErr, err)
		}
		return verifyErr
	}
	if err := unix.Unlinkat(parentFD, temporary, 0); err != nil {
		return fmt.Errorf("verified Workspace file is preserved at %q because deletion failed: %w", temporary, err)
	}
	return unix.Fsync(parentFD)
}

func moveWorkspaceCurrentRegularFile(root, sourcePath, targetPath, expectedSHA256 string, expectedMode uint32) error {
	sourceParent, sourceName, err := openWorkspaceCurrentParent(root, sourcePath, false)
	if errors.Is(err, unix.ENOENT) {
		return verifyMovedWorkspaceCurrentRegularFile(root, targetPath, expectedSHA256, expectedMode)
	}
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	targetParent, targetName, err := openWorkspaceCurrentParent(root, targetPath, false)
	if err != nil {
		return err
	}
	defer targetParent.Close()
	sourceFD, targetFD := int(sourceParent.Fd()), int(targetParent.Fd())
	var sourceEntry unix.Stat_t
	if err := unix.Fstatat(sourceFD, sourceName, &sourceEntry, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		return verifyMovedWorkspaceCurrentRegularFileAt(targetFD, targetName, expectedSHA256, expectedMode)
	} else if err != nil {
		return err
	} else if sourceEntry.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrUnsafePath
	}
	var targetStat unix.Stat_t
	if err := unix.Fstatat(targetFD, targetName, &targetStat, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if targetStat.Mode&unix.S_IFMT != unix.S_IFREG {
			return ErrUnsafePath
		}
		return ErrWriteConflict
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	sourceStat, err := verifyWorkspaceCurrentRegularFileAt(sourceFD, sourceName, expectedSHA256, expectedMode)
	if err != nil {
		return err
	}
	var currentSource unix.Stat_t
	if err := unix.Fstatat(sourceFD, sourceName, &currentSource, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if !sameWorkspaceCurrentFileIdentity(currentSource, sourceStat) {
		return ErrWriteConflict
	}
	return moveWorkspaceCurrentRegularFileAt(sourceFD, sourceName, targetFD, targetName, expectedSHA256, expectedMode, sourceStat)
}

func moveWorkspaceCurrentRegularFileAt(sourceFD int, sourceName string, targetFD int, targetName, expectedSHA256 string, expectedMode uint32, expectedStat unix.Stat_t) error {
	temporary, err := workspaceCurrentTemporaryName(".morde-move-")
	if err != nil {
		return err
	}
	if err := unix.Renameat2(sourceFD, sourceName, targetFD, temporary, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return ErrWriteConflict
		}
		return err
	}

	stagedStat, verifyErr := verifyWorkspaceCurrentRegularFileAt(targetFD, temporary, expectedSHA256, expectedMode)
	if verifyErr == nil && !sameWorkspaceCurrentFileIdentity(stagedStat, expectedStat) {
		verifyErr = ErrWriteConflict
	}
	if verifyErr != nil {
		return errors.Join(ErrWriteConflict, verifyErr, restoreStagedWorkspaceCurrentFile(targetFD, temporary, sourceFD, sourceName))
	}

	backup, err := workspaceCurrentTemporaryName(".morde-move-recovery-")
	if err != nil {
		return errors.Join(err, restoreStagedWorkspaceCurrentFile(targetFD, temporary, sourceFD, sourceName))
	}
	if err := unix.Linkat(targetFD, temporary, targetFD, backup, 0); err != nil {
		return errors.Join(fmt.Errorf("prepare move recovery link: %w", err), restoreStagedWorkspaceCurrentFile(targetFD, temporary, sourceFD, sourceName))
	}
	if err := unix.Fsync(sourceFD); err != nil {
		return errors.Join(err, restoreStagedWorkspaceCurrentFileWithBackup(targetFD, temporary, backup, sourceFD, sourceName))
	}
	if err := unix.Fsync(targetFD); err != nil {
		return errors.Join(err, restoreStagedWorkspaceCurrentFileWithBackup(targetFD, temporary, backup, sourceFD, sourceName))
	}

	if err := unix.Renameat2(targetFD, temporary, targetFD, targetName, unix.RENAME_NOREPLACE); err != nil {
		restoreErr := restoreStagedWorkspaceCurrentFileWithBackup(targetFD, temporary, backup, sourceFD, sourceName)
		if errors.Is(err, unix.EEXIST) {
			var current unix.Stat_t
			if statErr := unix.Fstatat(targetFD, targetName, &current, unix.AT_SYMLINK_NOFOLLOW); statErr == nil && current.Mode&unix.S_IFMT != unix.S_IFREG {
				return errors.Join(ErrUnsafePath, restoreErr)
			}
			return errors.Join(ErrWriteConflict, restoreErr)
		}
		return errors.Join(err, restoreErr)
	}
	if err := unix.Fsync(sourceFD); err != nil {
		return fmt.Errorf("move committed with recovery copy at %q; source directory sync failed: %w", backup, err)
	}
	if err := unix.Fsync(targetFD); err != nil {
		return fmt.Errorf("move committed with recovery copy at %q; target directory sync failed: %w", backup, err)
	}

	movedStat, verifyErr := verifyWorkspaceCurrentRegularFileAt(targetFD, targetName, expectedSHA256, expectedMode)
	if verifyErr == nil && !sameWorkspaceCurrentFileIdentity(movedStat, expectedStat) {
		verifyErr = ErrWriteConflict
	}
	if verifyErr != nil {
		return errors.Join(ErrWriteConflict, verifyErr, restoreMovedWorkspaceCurrentFile(targetFD, targetName, backup, sourceFD, sourceName, expectedStat))
	}
	if err := unix.Unlinkat(targetFD, backup, 0); err != nil {
		return fmt.Errorf("move completed; recovery copy is preserved at %q because cleanup failed: %w", backup, err)
	}
	return unix.Fsync(targetFD)
}

func restoreStagedWorkspaceCurrentFile(targetFD int, temporary string, sourceFD int, sourceName string) error {
	return restoreStagedWorkspaceCurrentFileWithBackup(targetFD, temporary, "", sourceFD, sourceName)
}

func restoreStagedWorkspaceCurrentFileWithBackup(targetFD int, temporary, backup string, sourceFD int, sourceName string) error {
	if err := unix.Renameat2(targetFD, temporary, sourceFD, sourceName, unix.RENAME_NOREPLACE); err != nil {
		preserved := temporary
		if backup != "" {
			preserved = backup
		}
		return fmt.Errorf("staged Workspace file is preserved at %q because source restoration failed: %w", preserved, err)
	}
	if backup != "" {
		if err := unix.Unlinkat(targetFD, backup, 0); err != nil {
			return fmt.Errorf("restored Workspace source; recovery link is preserved at %q: %w", backup, err)
		}
	}
	return errors.Join(unix.Fsync(targetFD), unix.Fsync(sourceFD))
}

func restoreMovedWorkspaceCurrentFile(targetFD int, targetName, backup string, sourceFD int, sourceName string, expectedStat unix.Stat_t) error {
	var current unix.Stat_t
	statErr := unix.Fstatat(targetFD, targetName, &current, unix.AT_SYMLINK_NOFOLLOW)
	if statErr == nil && sameWorkspaceCurrentFileIdentity(current, expectedStat) {
		if err := unix.Renameat2(targetFD, targetName, sourceFD, sourceName, unix.RENAME_NOREPLACE); err == nil {
			if err := unix.Unlinkat(targetFD, backup, 0); err != nil {
				return fmt.Errorf("restored moved file; recovery link is preserved at %q: %w", backup, err)
			}
			return errors.Join(unix.Fsync(targetFD), unix.Fsync(sourceFD))
		} else if !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("could not restore moved file from target; recovery copy is preserved at %q: %w", backup, err)
		}
	}
	if statErr != nil && !errors.Is(statErr, unix.ENOENT) {
		return fmt.Errorf("could not inspect moved target; recovery copy is preserved at %q: %w", backup, statErr)
	}
	if err := unix.Renameat2(targetFD, backup, sourceFD, sourceName, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("moved file is preserved at recovery path %q because source restoration failed: %w", backup, err)
	}
	return errors.Join(unix.Fsync(targetFD), unix.Fsync(sourceFD))
}

func verifyMovedWorkspaceCurrentRegularFile(root, relativePath, expectedSHA256 string, expectedMode uint32) error {
	parent, name, err := openWorkspaceCurrentParent(root, relativePath, false)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return ErrWriteConflict
		}
		return err
	}
	defer parent.Close()
	return verifyMovedWorkspaceCurrentRegularFileAt(int(parent.Fd()), name, expectedSHA256, expectedMode)
}

func verifyMovedWorkspaceCurrentRegularFileAt(parentFD int, name, expectedSHA256 string, expectedMode uint32) error {
	_, err := verifyWorkspaceCurrentRegularFileAt(parentFD, name, expectedSHA256, expectedMode)
	if errors.Is(err, unix.ENOENT) {
		return ErrWriteConflict
	}
	return err
}

func verifyWorkspaceCurrentRegularFileAt(parentFD int, name, expectedSHA256 string, expectedMode uint32) (unix.Stat_t, error) {
	var before unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return unix.Stat_t{}, err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return unix.Stat_t{}, ErrUnsafePath
	}
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return unix.Stat_t{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	var openedBefore unix.Stat_t
	if err := unix.Fstat(fd, &openedBefore); err != nil {
		return unix.Stat_t{}, errors.Join(err, file.Close())
	}
	if !sameWorkspaceCurrentFileVersion(before, openedBefore) {
		return unix.Stat_t{}, errors.Join(ErrWriteConflict, file.Close())
	}
	hasher := sha256.New()
	_, readErr := io.Copy(hasher, file)
	var openedAfter unix.Stat_t
	statErr := unix.Fstat(fd, &openedAfter)
	closeErr := file.Close()
	if readErr != nil || statErr != nil || closeErr != nil {
		return unix.Stat_t{}, errors.Join(readErr, statErr, closeErr)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != expectedSHA256 || uint32(openedAfter.Mode&0777) != expectedMode ||
		!sameWorkspaceCurrentFileVersion(openedBefore, openedAfter) {
		return unix.Stat_t{}, ErrWriteConflict
	}
	var current unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return unix.Stat_t{}, err
	}
	if current.Mode&unix.S_IFMT != unix.S_IFREG || uint32(current.Mode&0777) != expectedMode ||
		!sameWorkspaceCurrentFileVersion(openedAfter, current) {
		return unix.Stat_t{}, ErrWriteConflict
	}
	return openedAfter, nil
}

func writeWorkspaceCurrentSymbolicLink(root, relativePath, target, expectedTarget string) error {
	parent, name, err := openWorkspaceCurrentParent(root, relativePath, true)
	if err != nil {
		return err
	}
	defer parent.Close()
	parentFD := int(parent.Fd())
	if current, linkErr := readWorkspaceCurrentLinkAt(parentFD, name); linkErr == nil {
		if current == target {
			return nil
		}
		if expectedTarget == "" || current != expectedTarget {
			return ErrWriteConflict
		}
	} else if !errors.Is(linkErr, unix.ENOENT) {
		return linkErr
	} else if expectedTarget != "" {
		return ErrWriteConflict
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := ".morde-link-" + hex.EncodeToString(nonce[:])
	if err := unix.Symlinkat(target, parentFD, temporary); err != nil {
		return err
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = unix.Unlinkat(parentFD, temporary, 0)
		}
	}()
	if expectedTarget == "" {
		if err := unix.Renameat2(parentFD, temporary, parentFD, name, unix.RENAME_NOREPLACE); err != nil {
			return err
		}
		removeTemporary = false
		return unix.Fsync(parentFD)
	}
	if err := unix.Renameat2(parentFD, temporary, parentFD, name, unix.RENAME_EXCHANGE); err != nil {
		return err
	}
	previous, err := readWorkspaceCurrentLinkAt(parentFD, temporary)
	if err != nil || previous != expectedTarget {
		current, currentErr := readWorkspaceCurrentLinkAt(parentFD, name)
		if currentErr == nil && current == target {
			if restoreErr := unix.Renameat2(parentFD, temporary, parentFD, name, unix.RENAME_EXCHANGE); restoreErr == nil {
				return ErrWriteConflict
			}
		}
		removeTemporary = false
		return errors.Join(ErrWriteConflict, err)
	}
	if err := unix.Unlinkat(parentFD, temporary, 0); err != nil {
		removeTemporary = false
		return err
	}
	removeTemporary = false
	return unix.Fsync(parentFD)
}

func removeWorkspaceCurrentSymbolicLink(root, relativePath, expectedTarget string) error {
	parent, name, err := openWorkspaceCurrentParent(root, relativePath, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	parentFD := int(parent.Fd())
	target, err := readWorkspaceCurrentLinkAt(parentFD, name)
	if err != nil {
		return err
	}
	if target != expectedTarget {
		return ErrWriteConflict
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFLNK {
		return ErrUnsafePath
	}
	if err := unix.Unlinkat(parentFD, name, 0); err != nil {
		return err
	}
	return unix.Fsync(parentFD)
}

func readWorkspaceCurrentLinkAt(parentFD int, name string) (string, error) {
	buffer := make([]byte, 4096)
	count, err := unix.Readlinkat(parentFD, name, buffer)
	if err != nil {
		return "", err
	}
	if count == len(buffer) {
		return "", ErrUnsafePath
	}
	return string(buffer[:count]), nil
}
