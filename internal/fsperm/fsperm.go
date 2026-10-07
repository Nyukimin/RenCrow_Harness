// Package fsperm checks that a private directory or file is reachable by its
// owner only. Private stores and key files are checked, never silently repaired:
// a directory that is too open is an error for the operator to fix.
package fsperm

import "errors"

var (
	// ErrNotOwnerOnly is wrapped when the path is readable or writable by anyone
	// but its owner, or is not owned by the current user.
	ErrNotOwnerOnly = errors.New("fsperm: path is not owner-only")
	// ErrUnverifiable is wrapped when this operating system has no implemented
	// check. The caller must treat it as a failure: an unchecked secret location
	// is not an owner-only one.
	ErrUnverifiable = errors.New("fsperm: owner-only access cannot be verified on this OS")
)
