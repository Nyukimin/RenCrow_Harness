//go:build windows

package fsperm

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type fileAttributeTagInfo struct {
	fileAttributes uint32
	reparseTag     uint32
}

func CheckOwnerOnlyDir(path string) error {
	return check(path, true, false)
}

func CheckOwnerOnlyDirForChildren(path string) error {
	return check(path, true, true)
}

func CheckOwnerOnlyFile(path string) error {
	return check(path, false, false)
}

func check(path string, wantDir, requireChildInheritance bool) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("%w: path cannot be opened", ErrUnverifiable)
	}
	handle, err := windows.CreateFile(
		name,
		windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return fmt.Errorf("fsperm: open: %w", WithoutPath(err))
	}
	defer windows.CloseHandle(handle)

	var tagInfo fileAttributeTagInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&tagInfo)),
		uint32(unsafe.Sizeof(tagInfo)),
	); err != nil {
		return unverifiable("file kind cannot be verified", err)
	}
	fileType, err := windows.GetFileType(handle)
	if err != nil {
		return unverifiable("file type cannot be verified", err)
	}
	if err := checkPlainWindowsKind(
		wantDir,
		tagInfo.fileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0,
		tagInfo.fileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0,
		fileType == windows.FILE_TYPE_DISK,
	); err != nil {
		return err
	}

	sd, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return unverifiable("security descriptor cannot be read", err)
	}
	if sd == nil {
		return fmt.Errorf("%w: security descriptor is missing", ErrUnverifiable)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.IsValid() {
		return unverifiable("owner SID cannot be verified", err)
	}
	processUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || processUser == nil || processUser.User.Sid == nil || !processUser.User.Sid.IsValid() {
		return unverifiable("current process user SID cannot be verified", err)
	}
	ownerSID := owner.String()
	processSID := processUser.User.Sid.String()
	if ownerSID != processSID {
		return checkOwnerOnlyACL(ownerSID, processSID, false, false, nil)
	}

	dacl, _, err := sd.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
		return checkOwnerOnlyACL(ownerSID, processSID, false, false, nil)
	}
	if err != nil {
		return unverifiable("DACL cannot be verified", err)
	}
	if dacl == nil {
		return checkOwnerOnlyACL(owner.String(), processUser.User.Sid.String(), true, true, nil)
	}

	entries := make([]aclEntry, 0, dacl.AceCount)
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil || ace == nil {
			return unverifiable("DACL ACE cannot be read", err)
		}
		if ace.Header.AceFlags&^uint8(0x1f) != 0 {
			return fmt.Errorf("%w: ACL ACE flags cannot be verified", ErrUnverifiable)
		}
		kind := aclACEUnknown
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			kind = aclACEAllowed
		case windows.ACCESS_DENIED_ACE_TYPE:
			kind = aclACEDenied
		default:
			entries = append(entries, aclEntry{kind: aclACEUnknown})
			continue
		}

		const sidOffset = uint32(unsafe.Offsetof(windows.ACCESS_ALLOWED_ACE{}.SidStart))
		if uint32(ace.Header.AceSize) < sidOffset+8 {
			return fmt.Errorf("%w: ACL ACE is truncated", ErrUnverifiable)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || sidOffset+windows.GetLengthSid(sid) > uint32(ace.Header.AceSize) {
			return fmt.Errorf("%w: ACL ACE SID cannot be verified", ErrUnverifiable)
		}
		entries = append(entries, aclEntry{
			kind:       kind,
			trusteeSID: sid.String(),
			accessMask: uint32(ace.Mask),
			aceFlags:   ace.Header.AceFlags,
		})
	}

	if err := checkOwnerOnlyACL(ownerSID, processSID, true, false, entries); err != nil {
		return err
	}
	if requireChildInheritance {
		control, _, err := sd.Control()
		if err != nil {
			return unverifiable("DACL protection cannot be verified", err)
		}
		return checkOwnerOnlyDirInheritance(ownerSID, entries, control&windows.SE_DACL_PROTECTED != 0)
	}
	return nil
}

func unverifiable(detail string, err error) error {
	if err == nil {
		return fmt.Errorf("%w: %s", ErrUnverifiable, detail)
	}
	return fmt.Errorf("%w: %s: %v", ErrUnverifiable, detail, WithoutPath(err))
}

// CreatePrivateDir creates one new directory with a protected owner-only DACL
// that grants its owner full control and passes that access to files and dirs.
func CreatePrivateDir(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("%w: directory path cannot be opened", ErrUnverifiable)
	}
	_, attributes, err := privateSecurityAttributes(true)
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(name, attributes); err != nil {
		if isAlreadyExists(err) {
			return fmt.Errorf("fsperm: create directory: %w", os.ErrExist)
		}
		return fmt.Errorf("fsperm: create directory: %w", WithoutPath(err))
	}
	return nil
}

// CreatePrivateFile atomically creates one new file with a protected owner-only
// DACL. It never opens or changes an existing path.
func CreatePrivateFile(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("%w: file path cannot be opened", ErrUnverifiable)
	}
	_, attributes, err := privateSecurityAttributes(false)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		attributes,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if isAlreadyExists(err) {
			return nil, fmt.Errorf("fsperm: create file: %w", os.ErrExist)
		}
		return nil, fmt.Errorf("fsperm: create file: %w", WithoutPath(err))
	}
	f := os.NewFile(uintptr(handle), path)
	if f == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("%w: created file handle cannot be wrapped", ErrUnverifiable)
	}
	return f, nil
}

func privateSecurityAttributes(directory bool) (*windows.SECURITY_DESCRIPTOR, *windows.SecurityAttributes, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, nil, unverifiable("current process user SID cannot be verified", err)
	}
	sid := user.User.Sid.String()
	sddl := privateFileSDDL(sid)
	if directory {
		sddl = privateDirectorySDDL(sid)
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, nil, unverifiable("private security descriptor cannot be built", err)
	}
	attributes := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	return sd, attributes, nil
}

func isAlreadyExists(err error) bool {
	return errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS)
}
