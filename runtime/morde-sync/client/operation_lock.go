package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var errOperationLocked = errors.New("another Morde sync operation is active for this directory")

// IsOperationLockHeld reports whether another Morde sync process owns the directory lock.
func IsOperationLockHeld(ctx context.Context, directory string) (bool, error) {
	release, err := acquireOperationLock(ctx, directory)
	if errors.Is(err, errOperationLocked) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	release()
	return false, nil
}

// shortcut: this advisory lock coordinates morde-sync processes only; external
// editors cannot be frozen, so handoff still requires the user's stop-write confirmation.
func acquireOperationLock(ctx context.Context, directory string) (func(), error) {
	if ctx == nil || directory == "" {
		return nil, errors.New("context and workspace directory are required")
	}
	root, err := resumeCacheRoot()
	if err != nil {
		return nil, err
	}
	// Keep the path lock for compatibility with existing processes, then add a lock keyed by the actual directory identity.
	pathLock, err := acquireOperationLockFile(ctx, root, directory)
	if err != nil {
		return nil, err
	}
	identity, err := operationDirectoryIdentity(directory)
	if err != nil {
		releaseOperationLockFile(pathLock)
		return nil, fmt.Errorf("identify workspace operation directory: %w", err)
	}
	identityLock, err := acquireOperationLockFile(ctx, root, "identity:"+identity)
	if err != nil {
		releaseOperationLockFile(pathLock)
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseOperationLockFile(identityLock)
			releaseOperationLockFile(pathLock)
		})
	}, nil
}

func acquireOperationLockFile(ctx context.Context, root, key string) (*os.File, error) {
	digest := sha256.Sum256([]byte(key))
	path := filepath.Join(root, hex.EncodeToString(digest[:])+".lock")
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return nil, errors.New("workspace operation lock is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect workspace operation lock: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open workspace operation lock: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		_ = file.Close()
		return nil, errors.New("workspace operation lock is not a regular file")
	}
	if err := ctx.Err(); err != nil {
		_ = file.Close()
		return nil, err
	}
	locked, err := tryLockOperation(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock workspace operation: %w", err)
	}
	if !locked {
		_ = file.Close()
		return nil, errOperationLocked
	}
	return file, nil
}

func releaseOperationLockFile(file *os.File) {
	_ = unlockOperation(file)
	_ = file.Close()
}
