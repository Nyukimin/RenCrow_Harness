//go:build windows

package sourcecandidate

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openRegularNoFollow(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_SEQUENTIAL_SCAN,
		0,
	)
	if err != nil {
		return nil, err
	}
	var tagInfo struct {
		fileAttributes uint32
		reparseTag     uint32
	}
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&tagInfo)),
		uint32(unsafe.Sizeof(tagInfo)),
	); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if tagInfo.fileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		_ = windows.CloseHandle(handle)
		return nil, os.ErrInvalid
	}
	fileType, err := windows.GetFileType(handle)
	if err != nil || fileType != windows.FILE_TYPE_DISK {
		_ = windows.CloseHandle(handle)
		if err != nil {
			return nil, err
		}
		return nil, os.ErrInvalid
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, os.ErrInvalid
	}
	return file, nil
}
