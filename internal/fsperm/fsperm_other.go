//go:build !unix && !windows

package fsperm

import "fmt"

// CheckOwnerOnlyDir cannot verify an owner-only ACL on this OS. It fails closed.
func CheckOwnerOnlyDir(path string) error {
	return fmt.Errorf("%w: ACL verification is not implemented", ErrUnverifiable)
}

// CheckOwnerOnlyFile cannot verify an owner-only ACL on this OS. It fails closed.
func CheckOwnerOnlyFile(path string) error {
	return fmt.Errorf("%w: ACL verification is not implemented", ErrUnverifiable)
}

func CheckOwnerOnlyDirForChildren(path string) error {
	return fmt.Errorf("%w: ACL inheritance verification is not implemented", ErrUnverifiable)
}
