//go:build unix

package fsperm

// InitializeProcessOwner is unnecessary on Unix, where file ownership follows
// the process UID.
func InitializeProcessOwner() error { return nil }

// CheckProcessOwner verifies the process default owner matches its Unix user.
func CheckProcessOwner() error { return nil }
