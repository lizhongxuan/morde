//go:build windows

package mordesync

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// removeEmptyDirectoryAt opens the final path relative to the already-open
// parent handle, requires a directory, and marks that handle for deletion.
// This prevents a concurrent file replacement from being deleted by path.
func removeEmptyDirectoryAt(parent *os.Root, name string) error {
	directory, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(directory.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	var statusBlock windows.IO_STATUS_BLOCK
	var handle windows.Handle
	err = windows.NtCreateFile(&handle,
		windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes,
		&statusBlock,
		nil,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	if err != nil {
		if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) || errors.Is(err, windows.STATUS_OBJECT_PATH_NOT_FOUND) {
			return fs.ErrNotExist
		}
		if errors.Is(err, windows.STATUS_DIRECTORY_NOT_EMPTY) {
			return syscall.ENOTEMPTY
		}
		if errors.Is(err, windows.STATUS_NOT_A_DIRECTORY) || errors.Is(err, windows.STATUS_FILE_IS_A_DIRECTORY) {
			return syscall.ENOTDIR
		}
		return err
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return syscall.ENOTDIR
	}
	type fileDispositionInformation struct {
		DeleteFile uint8
	}
	disposition := fileDispositionInformation{DeleteFile: 1}
	if err := windows.SetFileInformationByHandle(handle, windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&disposition)), uint32(unsafe.Sizeof(disposition))); err != nil {
		if errors.Is(err, windows.STATUS_DIRECTORY_NOT_EMPTY) {
			return syscall.ENOTEMPTY
		}
		return err
	}
	return nil
}
