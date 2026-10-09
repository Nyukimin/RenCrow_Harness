//go:build !windows && !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package files

import "os"

func openNoFollow(path string) (*os.File, error) { return os.OpenFile(path, os.O_RDONLY, 0) }

func stageCreatePerm(perm os.FileMode, _ bool) os.FileMode { return perm }

func restoreStagePerm(*os.File, os.FileMode) error { return nil }

func isReparsePoint(os.FileInfo) bool { return false }

func syncDir(string) error { return nil }
