//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris && !windows

package mordesync

import (
	"errors"
	"os"
)

func removeEmptyDirectoryAt(_ *os.Root, _ string) error {
	return errors.New("safe empty-directory removal is unsupported on this platform")
}
