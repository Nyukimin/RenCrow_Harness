//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package process

import (
	"syscall"
	"testing"
	"time"
)

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		// A killed child of this test process is a zombie until it is reaped; Wait4
		// reaps it where this process is the parent, and fails harmlessly otherwise.
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still alive", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// escapeGroup moves the calling process into its parent's process group.
func escapeGroup() {
	if pg, err := syscall.Getpgid(syscall.Getppid()); err == nil {
		_ = syscall.Setpgid(0, pg)
	}
}
