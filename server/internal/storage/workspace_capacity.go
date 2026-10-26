package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"morde/server/internal/domain"
)

// WorkspaceCapacity checks the filesystem used by independent Workspace data.
type WorkspaceCapacity struct {
	rootDir      string
	minFreeBytes int64
}

func NewWorkspaceCapacity(rootDir string, minFreeBytes int64) (*WorkspaceCapacity, error) {
	if !filepath.IsAbs(rootDir) || filepath.Clean(rootDir) == string(filepath.Separator) || minFreeBytes <= 0 {
		return nil, fmt.Errorf("workspace storage directory and minimum free bytes are required")
	}
	return &WorkspaceCapacity{rootDir: filepath.Clean(rootDir), minFreeBytes: minFreeBytes}, nil
}

func (c *WorkspaceCapacity) CheckCapacity(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(c.rootDir)
	if err != nil {
		return fmt.Errorf("resolve workspace storage root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("inspect workspace storage root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace storage root is not a directory")
	}
	var stats syscall.Statfs_t
	if err := syscall.Statfs(root, &stats); err != nil {
		return fmt.Errorf("inspect workspace storage capacity: %w", err)
	}
	if stats.Bsize <= 0 {
		return fmt.Errorf("inspect workspace storage capacity: invalid filesystem block size")
	}
	blockSize := uint64(stats.Bsize)
	available := uint64(stats.Bavail)
	if available > ^uint64(0)/blockSize {
		available = ^uint64(0)
	} else {
		available *= blockSize
	}
	if available < uint64(c.minFreeBytes) {
		return domain.NewError(domain.CodeCapacityExceeded, "workspace filesystem is below its free-space reserve")
	}
	return nil
}

var _ interface{ CheckCapacity(context.Context) error } = (*WorkspaceCapacity)(nil)
