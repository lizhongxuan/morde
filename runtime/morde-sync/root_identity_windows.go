//go:build windows

package mordesync

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func matchesSelectedRootIdentity(root *os.Root, device, inode string) bool {
	file, err := root.Open(".")
	if err != nil {
		return false
	}
	defer file.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return false
	}
	fileIndex := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	return fileIndex != 0 && fmt.Sprint(info.VolumeSerialNumber) == device && fmt.Sprint(fileIndex) == inode
}
