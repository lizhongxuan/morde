//go:build darwin || linux

package client

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func operationDirectoryIdentity(directory string) (string, error) {
	info, err := os.Stat(directory)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("workspace root is not a directory")
	}
	identity, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("filesystem does not expose a stable directory identity")
	}
	return fmt.Sprintf("%x:%x", uint64(identity.Dev), uint64(identity.Ino)), nil
}

func tryLockOperation(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func unlockOperation(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
