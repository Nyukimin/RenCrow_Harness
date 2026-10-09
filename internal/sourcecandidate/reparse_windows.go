//go:build windows

package sourcecandidate

import (
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

func isReparsePoint(path string, info fs.FileInfo) (bool, error) {
	if info.Mode()&fs.ModeSymlink != 0 {
		return true, nil
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes, err := windows.GetFileAttributes(name)
	if err != nil {
		return false, err
	}
	return attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}

func portableDirectoryMode(_ fs.FileMode) string { return "0755" }

func directoryModeFromPortable(_ string) fs.FileMode { return 0o755 }

func portableFileModeForOS(_ fs.FileMode) string { return "0644" }

func chmodPortable(path string, mode fs.FileMode) error { return os.Chmod(path, mode) }
