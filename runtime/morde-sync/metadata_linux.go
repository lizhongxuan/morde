//go:build linux

package mordesync

import (
	"errors"
	"fmt"
	"os"
)

func captureDarwinACLFD(*os.File, string) (*NativeACL, error) { return nil, nil }

func captureDarwinACLSymlink(*os.File, string, string) (*NativeACL, error) { return nil, nil }

func applyDarwinACLFD(*os.File, string, *NativeACL) error {
	return errors.New("Darwin ACL helper called on Linux")
}

func applyDarwinACLSymlink(*os.File, string, string, *NativeACL) error {
	return errors.New("Darwin ACL helper called on Linux")
}

func darwinFDPath(*os.File) (string, error) {
	return "", fmt.Errorf("Darwin path helper is unavailable on Linux")
}
