//go:build unix

package fsperm

import (
	"fmt"
	"os"
	"syscall"
)

// CheckOwnerOnlyDir requires path to be a real directory (not a symbolic link)
// owned by the current user with no permission bit for group or others.
func CheckOwnerOnlyDir(path string) error {
	return check(path, true)
}

// CheckOwnerOnlyDirForChildren has no ACL inheritance requirement on Unix: new
// children receive their own explicit mode when created.
func CheckOwnerOnlyDirForChildren(path string) error {
	return CheckOwnerOnlyDir(path)
}

// CheckOwnerOnlyFile requires path to be a regular file (not a symbolic link)
// owned by the current user with no permission bit for group or others.
func CheckOwnerOnlyFile(path string) error {
	return check(path, false)
}

// CreatePrivateDir creates one new owner-only directory. Existing paths are not
// changed; callers inspect a collision with CheckOwnerOnlyDir.
func CreatePrivateDir(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil {
		return fmt.Errorf("fsperm: create directory: %w", WithoutPath(err))
	}
	return nil
}

// CreatePrivateFile atomically creates one new owner-only file. It never opens
// or changes an existing path.
func CreatePrivateFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("fsperm: create file: %w", WithoutPath(err))
	}
	return f, nil
}

func check(path string, wantDir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("fsperm: %w", WithoutPath(err))
	}
	kind := "file"
	ok := info.Mode().IsRegular()
	if wantDir {
		kind, ok = "directory", info.IsDir()
	}
	if !ok {
		return fmt.Errorf("%w: not a plain %s", ErrNotOwnerOnly, kind)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: mode %04o gives access to group or others", ErrNotOwnerOnly, perm)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: owner cannot be read", ErrUnverifiable)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: owned by another user", ErrNotOwnerOnly)
	}
	return nil
}
