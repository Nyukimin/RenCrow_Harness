// Package oslock takes operating-system advisory locks on files: the writer lock
// of a Thread and the migration lock of a data root.
//
// Ownership is exactly "this open file holds the OS lock". The content of the
// lock file is never read and never decides anything: a PID, a timestamp or a
// heartbeat in it cannot make a process the owner and cannot keep a dead
// process's lock alive (the OS drops the lock when the process ends), and
// expiry alone never lets a second process take the lock over. The OS-specific
// system calls live in the lock_*.go adapters selected by build tags.
//
// Locks are advisory: they bind only the processes that ask for them.
package oslock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
)

// Mode is the kind of lock to take.
type Mode int

const (
	// Exclusive excludes every other holder, shared or exclusive.
	Exclusive Mode = iota
	// Shared coexists with other shared holders and excludes exclusive ones.
	Shared
)

var (
	// ErrLocked is returned when another holder (another process, or another open
	// of the same file in this process) holds a conflicting lock.
	ErrLocked = errors.New("oslock: the lock is held by another owner")
	// ErrUnsupported is returned on an operating system without an implemented
	// advisory lock. It fails closed: no lock is simulated.
	ErrUnsupported = errors.New("oslock: advisory file locks are not available on this OS")
	// ErrBadPath is returned for a lock path that is not an absolute path to a
	// regular file (a directory, a symbolic link, a device).
	ErrBadPath = errors.New("oslock: the lock path is not an absolute path to a regular file")
)

// Lock is a held lock. Release it exactly when the protected work ends; closing
// the process releases it too.
type Lock struct {
	f    *os.File
	once sync.Once
	err  error
}

// TryLock opens (creating it owner-only if needed) the lock file at path and takes
// the lock without waiting. The file is never replaced or removed by this package:
// removing a lock file while others may hold or open it would split the lock.
func TryLock(path string, mode Mode) (*Lock, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrBadPath
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, ErrBadPath
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("oslock: %w", fsperm.WithoutPath(err))
	}
	f, err := fsperm.CreatePrivateFile(path)
	if errors.Is(err, os.ErrExist) {
		before, statErr := os.Lstat(path)
		if statErr != nil {
			return nil, fmt.Errorf("oslock: %w", fsperm.WithoutPath(statErr))
		}
		if !before.Mode().IsRegular() {
			return nil, ErrBadPath
		}
		f, err = os.OpenFile(path, os.O_RDWR, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("oslock: %w", fsperm.WithoutPath(err))
	}
	// What was opened must be what was inspected: a link swapped in between the
	// check and the open is refused.
	after, err1 := f.Stat()
	before, err2 := os.Lstat(path)
	if err1 != nil || err2 != nil || !before.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = f.Close()
		return nil, ErrBadPath
	}
	if err := fsperm.CheckOwnerOnlyFile(path); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("oslock: private lock file: %w", fsperm.WithoutPath(err))
	}
	if err := lockFile(f, mode); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Release drops the lock. It is idempotent; the first call's result is returned
// by every call.
func (l *Lock) Release() error {
	l.once.Do(func() {
		uerr := unlockFile(l.f)
		cerr := l.f.Close()
		l.err = errors.Join(uerr, cerr)
	})
	return l.err
}
