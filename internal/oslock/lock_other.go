//go:build !windows && !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package oslock

import "os"

// lockFile fails closed: an operating system without an implemented advisory lock
// cannot hold a writer lock, and no substitute (a PID file, a lease) is offered.
func lockFile(*os.File, Mode) error { return ErrUnsupported }

func unlockFile(*os.File) error { return nil }
