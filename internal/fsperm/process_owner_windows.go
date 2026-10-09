//go:build windows

package fsperm

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

type tokenOwnerInfo struct {
	owner *windows.SID
}

// InitializeProcessOwner changes only the current process token's default owner
// to its token user. It refuses to run under thread impersonation.
func InitializeProcessOwner() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := checkNoThreadImpersonation(); err != nil {
		return err
	}

	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_ADJUST_DEFAULT, &token); err != nil {
		return fmt.Errorf("%w: process token cannot be opened: %v", ErrUnverifiable, err)
	}
	defer token.Close()

	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return fmt.Errorf("%w: process user SID cannot be verified: %v", ErrUnverifiable, err)
	}
	userSID := user.User.Sid.String()
	ownerSID, err := tokenOwnerSID(token)
	if err != nil {
		return err
	}
	if ownerSID == userSID {
		return nil
	}

	owner := tokenOwnerInfo{owner: user.User.Sid}
	if err := windows.SetTokenInformation(
		token,
		windows.TokenOwner,
		(*byte)(unsafe.Pointer(&owner)),
		uint32(unsafe.Sizeof(owner)),
	); err != nil {
		return fmt.Errorf("%w: process default owner cannot be set: %v", ErrUnverifiable, err)
	}
	ownerSID, err = tokenOwnerSID(token)
	if err != nil {
		return err
	}
	if ownerSID != userSID {
		return fmt.Errorf("%w: process default owner readback did not match the process user", ErrUnverifiable)
	}
	return nil
}

// CheckProcessOwner verifies the current thread is not impersonating and the
// process token's default owner matches its user. It never changes the token.
func CheckProcessOwner() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := checkNoThreadImpersonation(); err != nil {
		return err
	}
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return fmt.Errorf("%w: process token cannot be opened: %v", ErrUnverifiable, err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return fmt.Errorf("%w: process user SID cannot be verified: %v", ErrUnverifiable, err)
	}
	ownerSID, err := tokenOwnerSID(token)
	if err != nil {
		return err
	}
	return checkProcessOwnerDefault(user.User.Sid.String(), ownerSID)
}

func checkNoThreadImpersonation() error {
	var threadToken windows.Token
	err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &threadToken)
	if err == nil {
		_ = threadToken.Close()
		return fmt.Errorf("%w: thread impersonation is active", ErrUnverifiable)
	}
	if !errors.Is(err, windows.ERROR_NO_TOKEN) {
		return fmt.Errorf("%w: thread impersonation cannot be checked: %v", ErrUnverifiable, err)
	}
	return nil
}

func tokenOwnerSID(token windows.Token) (string, error) {
	// TOKEN_OWNER and its SID fit in this aligned buffer (a SID is at most 68 bytes).
	buffer := make([]uintptr, 64)
	var returned uint32
	bufferSize := uint32(len(buffer) * int(unsafe.Sizeof(buffer[0])))
	if err := windows.GetTokenInformation(
		token,
		windows.TokenOwner,
		(*byte)(unsafe.Pointer(&buffer[0])),
		bufferSize,
		&returned,
	); err != nil {
		return "", fmt.Errorf("%w: process default owner cannot be read: %v", ErrUnverifiable, err)
	}
	if returned < uint32(unsafe.Sizeof(tokenOwnerInfo{})) || returned > bufferSize {
		return "", fmt.Errorf("%w: process default owner result is invalid", ErrUnverifiable)
	}
	base := uintptr(unsafe.Pointer(&buffer[0]))
	end := base + uintptr(returned)
	owner := (*tokenOwnerInfo)(unsafe.Pointer(&buffer[0])).owner
	if owner == nil {
		return "", fmt.Errorf("%w: process default owner SID cannot be verified", ErrUnverifiable)
	}
	ownerAddress := uintptr(unsafe.Pointer(owner))
	if ownerAddress < base || ownerAddress > end || end-ownerAddress < 8 || !owner.IsValid() {
		return "", fmt.Errorf("%w: process default owner SID cannot be verified", ErrUnverifiable)
	}
	if sidLength := uintptr(windows.GetLengthSid(owner)); sidLength < 8 || sidLength > end-ownerAddress {
		return "", fmt.Errorf("%w: process default owner SID is truncated", ErrUnverifiable)
	}
	return owner.String(), nil
}
