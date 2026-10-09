package fsperm

import "fmt"

type aclACEKind uint8

const (
	aclACEUnknown aclACEKind = iota
	aclACEAllowed
	aclACEDenied
)

type aclEntry struct {
	kind       aclACEKind
	trusteeSID string
	accessMask uint32
	aceFlags   uint8
}

const (
	aceObjectInherit    = 0x01       // OBJECT_INHERIT_ACE
	aceContainerInherit = 0x02       // CONTAINER_INHERIT_ACE
	aceNoPropagate      = 0x04       // NO_PROPAGATE_INHERIT_ACE
	fileAllAccess       = 0x001f01ff // FILE_ALL_ACCESS
	genericAll          = 0x10000000 // GENERIC_ALL
)

func checkProcessOwnerDefault(userSID, ownerSID string) error {
	if userSID == "" || ownerSID == "" {
		return fmt.Errorf("%w: process owner SID cannot be verified", ErrUnverifiable)
	}
	if ownerSID != userSID {
		return fmt.Errorf("%w: process default owner does not match the process user", ErrNotOwnerOnly)
	}
	return nil
}

// checkOwnerOnlyACL evaluates the Windows ACL facts read from one object handle.
// A missing or null DACL grants broad access, so both fail closed.
func checkOwnerOnlyACL(ownerSID, processSID string, daclPresent, daclNull bool, entries []aclEntry) error {
	if ownerSID == "" || processSID == "" {
		return fmt.Errorf("%w: owner SID cannot be verified", ErrUnverifiable)
	}
	if ownerSID != processSID {
		return fmt.Errorf("%w: owned by another user", ErrNotOwnerOnly)
	}
	if !daclPresent || daclNull {
		return fmt.Errorf("%w: DACL is missing or null", ErrNotOwnerOnly)
	}
	for _, entry := range entries {
		switch entry.kind {
		case aclACEAllowed:
			if entry.trusteeSID == "" {
				return fmt.Errorf("%w: allow ACE trustee cannot be verified", ErrUnverifiable)
			}
			if entry.accessMask != 0 && entry.trusteeSID != ownerSID {
				return fmt.Errorf("%w: allow ACE grants access to a non-owner", ErrNotOwnerOnly)
			}
		case aclACEDenied:
			if entry.trusteeSID == "" {
				return fmt.Errorf("%w: deny ACE trustee cannot be verified", ErrUnverifiable)
			}
		default:
			return fmt.Errorf("%w: ACL contains an unknown ACE", ErrUnverifiable)
		}
	}
	return nil
}

func checkOwnerOnlyDirInheritance(ownerSID string, entries []aclEntry, daclProtected bool) error {
	if !daclProtected {
		return fmt.Errorf("%w: directory DACL is not protected from parent inheritance", ErrNotOwnerOnly)
	}
	if ownerSID == "" {
		return fmt.Errorf("%w: directory owner SID cannot be verified", ErrUnverifiable)
	}
	var inheritsFullControl bool
	for _, entry := range entries {
		inheritFlags := entry.aceFlags & (aceObjectInherit | aceContainerInherit)
		if inheritFlags == 0 || entry.accessMask == 0 {
			continue
		}
		if entry.aceFlags&aceNoPropagate != 0 {
			return fmt.Errorf("%w: inherited ACE stops propagating to descendants", ErrNotOwnerOnly)
		}
		switch entry.kind {
		case aclACEDenied:
			return fmt.Errorf("%w: propagating deny ACE can conflict with child access", ErrNotOwnerOnly)
		case aclACEAllowed:
			if entry.trusteeSID != ownerSID {
				return fmt.Errorf("%w: inherited allow ACE grants access to a non-owner", ErrNotOwnerOnly)
			}
			if inheritFlags == aceObjectInherit|aceContainerInherit &&
				(entry.accessMask&fileAllAccess == fileAllAccess || entry.accessMask&genericAll != 0) {
				inheritsFullControl = true
			}
		default:
			return fmt.Errorf("%w: inherited ACL ACE cannot be verified", ErrUnverifiable)
		}
	}
	if !inheritsFullControl {
		return fmt.Errorf("%w: owner-only DACL does not pass full control to files and directories", ErrNotOwnerOnly)
	}
	return nil
}

func privateDirectorySDDL(ownerSID string) string {
	return "O:" + ownerSID + "D:P(A;OICI;FA;;;" + ownerSID + ")"
}

func privateFileSDDL(ownerSID string) string {
	return "O:" + ownerSID + "D:P(A;;FA;;;" + ownerSID + ")"
}

func checkPlainWindowsKind(wantDir, isDir, isReparse, isDisk bool) error {
	kind := "file"
	if wantDir {
		kind = "directory"
	}
	if isReparse || !isDisk || wantDir != isDir {
		return fmt.Errorf("%w: not a plain %s", ErrNotOwnerOnly, kind)
	}
	return nil
}
