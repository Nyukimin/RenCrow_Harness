//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package service_test

import "syscall"

func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// killProcess ends a process and its group, whatever state a test left it in.
func killProcess(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// reapProcess collects a child that has died, so it no longer counts as alive.
func reapProcess(pid int) {
	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
}
