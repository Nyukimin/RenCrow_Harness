//go:build !unix

package fsperm

import "fmt"

// CheckOwnerOnlyDir cannot verify an owner-only ACL on this OS yet. It fails
// closed; the Windows ACL check is part of the three-OS acceptance.
func CheckOwnerOnlyDir(path string) error {
	return fmt.Errorf("%w: ACL verification is not implemented", ErrUnverifiable)
}

// CheckOwnerOnlyFile cannot verify an owner-only ACL on this OS yet. It fails closed.
func CheckOwnerOnlyFile(path string) error {
	return fmt.Errorf("%w: ACL verification is not implemented", ErrUnverifiable)
}
