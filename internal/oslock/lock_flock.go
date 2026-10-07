//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package oslock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockFile takes a BSD flock(2) lock. flock is tied to the open file description,
// so unrelated code in this process that opens and closes the same file cannot
// drop it (unlike POSIX fcntl record locks), and another open of the file, in this
// or another process, conflicts as it should.
func lockFile(f *os.File, mode Mode) error {
	how := syscall.LOCK_EX
	if mode == Shared {
		how = syscall.LOCK_SH
	}
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return ErrLocked
		}
		return fmt.Errorf("oslock: flock: %w", err)
	}
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
