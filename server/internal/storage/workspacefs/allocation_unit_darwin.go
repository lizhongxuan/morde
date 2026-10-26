//go:build darwin

package workspacefs

import (
	"fmt"
	"syscall"
)

func filesystemAllocationUnit(path string) (int64, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return 0, fmt.Errorf("inspect workspace storage filesystem: %w", err)
	}
	if stats.Bsize <= 0 {
		return 0, fmt.Errorf("invalid workspace filesystem allocation unit")
	}
	return int64(stats.Bsize), nil
}
