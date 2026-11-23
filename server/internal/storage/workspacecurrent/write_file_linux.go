//go:build linux

package workspacecurrent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func writeWorkspaceCurrentFileFrom(ctx context.Context, root, relativePath string, content io.Reader, size int64, digest string, mode uint32,
	expectedSHA256 string) (retErr error) {
	parts := strings.Split(relativePath, "/")
	if len(parts) < 3 {
		return ErrUnsafePath
	}
	workspaceDir, err := openWorkspaceCurrentFile(root, strings.Split(relativePath, "/")[0]+"/"+strings.Split(relativePath, "/")[1])
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, workspaceDir.Close()) }()
	rootFD := int(workspaceDir.Fd())
	parentFD, err := unix.Dup(rootFD)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, unix.Close(parentFD)) }()
	for _, component := range parts[2 : len(parts)-1] {
		if component == "" || component == "." || component == ".." {
			return ErrUnsafePath
		}
		nextFD, err := unix.Openat(parentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(err, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(parentFD, component, 0750); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				return mkdirErr
			}
			if syncErr := unix.Fsync(parentFD); syncErr != nil {
				return syncErr
			}
			nextFD, err = unix.Openat(parentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if err != nil {
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
				return ErrUnsafePath
			}
			return err
		}
		if err := unix.Close(parentFD); err != nil {
			_ = unix.Close(nextFD)
			return err
		}
		parentFD = nextFD
	}
	name := parts[len(parts)-1]
	if err := verifyExpectedWorkspaceFile(parentFD, name, expectedSHA256); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := ".morde-write-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(parentFD, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			retErr = errors.Join(retErr, unix.Unlinkat(parentFD, temporary, 0))
		}
	}()
	file := os.NewFile(uintptr(fd), temporary)
	hasher := sha256.New()
	source := workspaceCurrentContextReader{ctx: ctx, source: content}
	written, err := io.CopyN(io.MultiWriter(file, hasher), source, size)
	if err != nil || written != size {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return errors.Join(err, file.Close())
	}
	var extra [1]byte
	if n, err := source.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("Workspace current file content exceeds its declared size")
		}
		return errors.Join(err, file.Close())
	}
	if hex.EncodeToString(hasher.Sum(nil)) != digest {
		return errors.Join(errors.New("Workspace current file content digest changed"), file.Close())
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Chmod(os.FileMode(mode & 0777)); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	preserveTemporary, err := commitWorkspaceCurrentWriteAt(parentFD, temporary, name, expectedSHA256, digest)
	if preserveTemporary {
		removeTemporary = false
	}
	if err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

type workspaceCurrentContextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r workspaceCurrentContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(buffer)
}

func verifyExpectedWorkspaceFile(parentFD int, name, expectedSHA256 string) error {
	var stat unix.Stat_t
	err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		if expectedSHA256 == "" {
			return nil
		}
		return ErrWriteConflict
	}
	if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrUnsafePath
	}
	if expectedSHA256 == "" {
		return ErrWriteConflict
	}
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	var openedBefore unix.Stat_t
	if err := unix.Fstat(fd, &openedBefore); err != nil {
		return errors.Join(err, file.Close())
	}
	if !sameWorkspaceCurrentFileVersion(stat, openedBefore) {
		return errors.Join(ErrWriteConflict, file.Close())
	}
	hasher := sha256.New()
	_, readErr := io.Copy(hasher, file)
	var openedAfter unix.Stat_t
	statErr := unix.Fstat(fd, &openedAfter)
	closeErr := file.Close()
	if readErr != nil || statErr != nil || closeErr != nil {
		return errors.Join(readErr, statErr, closeErr)
	}
	var current unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return ErrWriteConflict
		}
		return err
	}
	if hex.EncodeToString(hasher.Sum(nil)) != expectedSHA256 ||
		!sameWorkspaceCurrentFileVersion(openedBefore, openedAfter) ||
		!sameWorkspaceCurrentFileVersion(openedAfter, current) {
		return ErrWriteConflict
	}
	return nil
}

// commitWorkspaceCurrentWriteAt closes the directory-entry replacement race
// left by a separate digest check followed by rename. It is still not a CAS
// against a writer that already holds an fd to the old inode; that requires
// such writers to cooperate with the Workspace lock.
func commitWorkspaceCurrentWriteAt(parentFD int, temporary, name, expectedSHA256, candidateSHA256 string) (preserveTemporary bool, retErr error) {
	if expectedSHA256 == "" {
		if err := unix.Renameat2(parentFD, temporary, parentFD, name, unix.RENAME_NOREPLACE); err != nil {
			if errors.Is(err, unix.EEXIST) {
				var current unix.Stat_t
				if statErr := unix.Fstatat(parentFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); statErr == nil && current.Mode&unix.S_IFMT != unix.S_IFREG {
					return false, ErrUnsafePath
				}
				return false, ErrWriteConflict
			}
			if errors.Is(err, unix.ENOENT) {
				return false, ErrWriteConflict
			}
			return false, err
		}
		return false, unix.Fsync(parentFD)
	}

	if err := unix.Renameat2(parentFD, temporary, parentFD, name, unix.RENAME_EXCHANGE); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, ErrWriteConflict
		}
		return false, err
	}

	priorErr := verifyExpectedWorkspaceFile(parentFD, temporary, expectedSHA256)
	candidateErr := verifyExpectedWorkspaceFile(parentFD, name, candidateSHA256)
	if priorErr == nil && candidateErr == nil {
		if err := unix.Unlinkat(parentFD, temporary, 0); err != nil {
			return true, fmt.Errorf("preserved prior Workspace file at %q after replacement cleanup failed: %w", temporary, err)
		}
		return false, unix.Fsync(parentFD)
	}

	if err := unix.Renameat2(parentFD, temporary, parentFD, name, unix.RENAME_EXCHANGE); err != nil {
		return true, errors.Join(ErrWriteConflict, priorErr, candidateErr,
			fmt.Errorf("could not restore exchanged Workspace file; conflicting entry is preserved at %q: %w", temporary, err))
	}
	if err := unix.Fsync(parentFD); err != nil {
		return false, errors.Join(ErrWriteConflict, priorErr, candidateErr, err)
	}
	if candidateErr := verifyExpectedWorkspaceFile(parentFD, temporary, candidateSHA256); candidateErr != nil {
		return true, errors.Join(ErrWriteConflict, priorErr, candidateErr,
			fmt.Errorf("preserved changed write candidate at %q", temporary))
	}
	return false, errors.Join(ErrWriteConflict, priorErr, candidateErr)
}

func sameWorkspaceCurrentFileVersion(left, right unix.Stat_t) bool {
	return left.Mode&unix.S_IFMT == unix.S_IFREG && right.Mode&unix.S_IFMT == unix.S_IFREG &&
		left.Dev == right.Dev && left.Ino == right.Ino && left.Mode == right.Mode && left.Size == right.Size &&
		left.Mtim == right.Mtim && left.Ctim == right.Ctim
}

func sameWorkspaceCurrentFileIdentity(left, right unix.Stat_t) bool {
	return left.Mode&unix.S_IFMT == unix.S_IFREG && right.Mode&unix.S_IFMT == unix.S_IFREG &&
		left.Dev == right.Dev && left.Ino == right.Ino
}

func workspaceCurrentTemporaryName(prefix string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(nonce[:]), nil
}
