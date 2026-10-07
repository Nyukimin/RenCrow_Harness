//go:build darwin

package process

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// hostIncarnation is the boot session UUID, which changes at every boot. Where the
// kernel does not offer it (or a restricted environment refuses the query) the boot
// time stands in, and failing that the start time of launchd, PID 1, which is as
// old as the boot and never restarts within it.
func hostIncarnation() (string, error) {
	if s, err := unix.Sysctl("kern.bootsessionuuid"); err == nil && s != "" {
		return "darwin-boot:" + s, nil
	}
	if tv, err := unix.SysctlTimeval("kern.boottime"); err == nil {
		return fmt.Sprintf("darwin-boottime:%d.%06d", tv.Sec, tv.Usec), nil
	}
	if tok, err := startToken(1); err == nil {
		return "darwin-init:" + tok, nil
	}
	return "", ErrUnsupported
}

// startToken is the process's start time as the kernel records it.
func startToken(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// The kernel answers a PID that is not there with no data at all, which the
		// library reports as EIO; ESRCH and ENOENT are the other ways of saying the same.
		if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EIO) {
			return "", ErrNoSuchProcess
		}
		return "", ErrUnsupported
	}
	if int(kp.Proc.P_pid) != pid {
		return "", ErrNoSuchProcess
	}
	t := kp.Proc.P_starttime
	return fmt.Sprintf("darwin-start:%d.%06d", t.Sec, t.Usec), nil
}
