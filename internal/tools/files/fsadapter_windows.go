//go:build windows

package files

import (
	"os"
	"syscall"
)

// openNoFollow opens a file for reading. Windows has no O_NOFOLLOW: the caller
// refuses a reparse point by looking at the attributes before and after the open.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY, 0)
}

func stageCreatePerm(perm os.FileMode, _ bool) os.FileMode { return perm }

func restoreStagePerm(*os.File, os.FileMode) error { return nil }

// isReparsePoint reports a junction, a symbolic link or any other reparse point.
func isReparsePoint(fi os.FileInfo) bool {
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	return ok && d.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// syncDir has no equivalent on Windows (a directory cannot be opened for sync); the
// rename itself is the durability point the OS offers.
func syncDir(string) error { return nil }
