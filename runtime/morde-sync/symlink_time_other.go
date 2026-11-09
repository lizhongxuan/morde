//go:build !darwin && !linux

package mordesync

import (
	"errors"
	"os"
	"time"
)

func setSymlinkTimes(*os.Root, string, time.Time) error {
	return errors.New("setting symlink modification time is unsupported on this platform")
}
