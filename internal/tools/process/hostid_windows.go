//go:build windows

package process

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code Windows reports for a process that has not ended.
const stillActive = 259

// systemTimeOfDay is SYSTEM_TIMEOFDAY_INFORMATION.
type systemTimeOfDay struct {
	BootTime      int64
	CurrentTime   int64
	TimeZoneBias  int64
	TimeZoneID    uint32
	Reserved      uint32
	BootTimeBias  uint64
	SleepTimeBias uint64
}

// hostIncarnation identifies this boot. The System process (PID 4) is created at boot
// and its creation time is stored when it is created, so it does not move when the
// clock is set; where it cannot be read, the kernel's boot time stands in, which does
// move when the clock is adjusted (a later check then finds another incarnation and
// leaves a process alone, never the other way round).
func hostIncarnation() (string, error) {
	if h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, 4); err == nil {
		defer windows.CloseHandle(h)
		if tok, _, err := handleStart(h); err == nil {
			return "windows-system:" + tok, nil
		}
	}
	var info systemTimeOfDay
	var n uint32
	if err := windows.NtQuerySystemInformation(windows.SystemTimeOfDayInformation, unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), &n); err != nil {
		return "", ErrUnsupported
	}
	return fmt.Sprintf("windows-boot:%d", info.BootTime), nil
}

// handleStart is the creation time of the process behind a handle, and whether that
// process is still running.
func handleStart(h windows.Handle) (string, bool, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", false, ErrUnsupported
	}
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return "", false, ErrUnsupported
	}
	return fmt.Sprintf("windows-start:%d", created.Nanoseconds()), code == stillActive, nil
}

// startToken is the process's creation time. A process that has ended, whose handle
// is still held by someone, is not there.
func startToken(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return "", ErrNoSuchProcess
		}
		return "", ErrUnsupported
	}
	defer windows.CloseHandle(h)
	tok, active, err := handleStart(h)
	if err != nil {
		return "", err
	}
	if !active {
		return "", ErrNoSuchProcess
	}
	return tok, nil
}
