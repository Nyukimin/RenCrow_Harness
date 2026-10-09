//go:build !windows

package sourcecandidate

import (
	"fmt"
	"io/fs"
	"os"
	"strconv"
)

func isReparsePoint(_ string, info fs.FileInfo) (bool, error) {
	return info.Mode()&fs.ModeSymlink != 0, nil
}

func portableDirectoryMode(mode fs.FileMode) string { return fmt.Sprintf("%04o", mode.Perm()) }

func directoryModeFromPortable(mode string) fs.FileMode {
	parsed, _ := strconv.ParseUint(mode, 8, 32)
	return fs.FileMode(parsed)
}

func portableFileModeForOS(mode fs.FileMode) string {
	if mode.Perm()&0o111 != 0 {
		return "0755"
	}
	return "0644"
}

func chmodPortable(path string, mode fs.FileMode) error { return os.Chmod(path, mode) }
