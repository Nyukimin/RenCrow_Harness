//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package process

import "testing"

// The liveness probe is unix-only; the tests that need it skip elsewhere.
func alive(int) bool { return false }

func waitDead(*testing.T, int) {}

func escapeGroup() {}
