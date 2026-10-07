//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package files

import (
	"errors"
	"os"
	"syscall"
)

// openNoFollow opens a file for reading without following a link in its last
// component, and without blocking on a device or pipe that was swapped in.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// isReparsePoint is Windows' notion; unix has links, which isLink sees by mode.
func isReparsePoint(os.FileInfo) bool { return false }

// syncDir makes a rename or a new name in dir durable.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
