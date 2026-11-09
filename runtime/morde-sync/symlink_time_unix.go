//go:build darwin || linux

package mordesync

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func setSymlinkTimes(parent *os.Root, name string, modified time.Time) error {
	directory, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	stamp := unix.NsecToTimespec(modified.UnixNano())
	return unix.UtimesNanoAt(int(directory.Fd()), name, []unix.Timespec{stamp, stamp}, unix.AT_SYMLINK_NOFOLLOW)
}
