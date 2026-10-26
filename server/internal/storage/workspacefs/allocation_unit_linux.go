//go:build linux

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
	unit := stats.Frsize
	if unit <= 0 {
		unit = stats.Bsize
	}
	if unit <= 0 {
		return 0, fmt.Errorf("invalid workspace filesystem allocation unit")
	}
	return unit, nil
}
