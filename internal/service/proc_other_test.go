//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package service_test

// The tests that probe a PID skip on these systems.
func processAlive(int) bool { return false }

func killProcess(int) {}

func reapProcess(int) {}
