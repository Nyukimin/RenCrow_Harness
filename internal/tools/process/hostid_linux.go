//go:build linux

package process

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// hostIncarnation is the kernel's boot ID: it changes at every boot.
func hostIncarnation() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", ErrUnsupported
	}
	return "linux-boot:" + strings.TrimSpace(string(b)), nil
}

// startToken is field 22 of /proc/<pid>/stat, the process's start time in clock ticks
// after boot. The command name (field 2) may hold spaces and parentheses, so fields
// are counted from the last closing parenthesis.
func startToken(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoSuchProcess
	}
	if err != nil {
		return "", ErrUnsupported
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return "", ErrUnsupported
	}
	fields := strings.Fields(s[i+1:])
	// fields[0] is field 3 (state); start time is field 22.
	if len(fields) < 20 {
		return "", ErrUnsupported
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", ErrUnsupported
	}
	return "linux-start:" + fields[19], nil
}
