//go:build darwin || linux

package workspacecurrent

import (
	"time"

	"golang.org/x/sys/unix"
)

func setWorkspaceCurrentSymlinkModTime(path string, modified time.Time) error {
	timestamp := unix.NsecToTimespec(modified.UnixNano())
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{timestamp, timestamp}, unix.AT_SYMLINK_NOFOLLOW)
}
