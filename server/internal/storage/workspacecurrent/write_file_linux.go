//go:build linux

package workspacecurrent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	if err := verifyExpectedWorkspaceFile(parentFD, name, expectedSHA256); err != nil {
		return err
	}
	if err := unix.Renameat2(parentFD, temporary, parentFD, name, 0); err != nil {
		return err
	}
	removeTemporary = false
	return unix.Fsync(parentFD)
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
	hasher := sha256.New()
	_, readErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != expectedSHA256 {
		return ErrWriteConflict
	}
	return nil
}
