//go:build windows

package client

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func hubDirectoryFileIdentity(root *os.Root) (string, error) {
	directory, err := root.Open(".")
	if err != nil {
		return "", errHubInspect
	}
	defer directory.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(directory.Fd()), &info); err != nil ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.VolumeSerialNumber == 0 ||
		(info.FileIndexHigh == 0 && info.FileIndexLow == 0) {
		return "", errHubInspect
	}
	return fmt.Sprintf("windows:%08x:%08x%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}
