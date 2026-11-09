//go:build aix || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package mordesync

import (
	"os"

	"golang.org/x/sys/unix"
)

// removeEmptyDirectoryAt asks the kernel to remove a directory specifically.
// AT_REMOVEDIR makes a file swapped into the path fail instead of deleting it.
func removeEmptyDirectoryAt(parent *os.Root, name string) error {
	directory, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return unix.Unlinkat(int(directory.Fd()), name, unix.AT_REMOVEDIR)
}
