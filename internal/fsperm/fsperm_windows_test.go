//go:build windows

package fsperm_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
	"golang.org/x/sys/windows"
)

func TestWindowsOwnerOnlyChecksAreReadOnlyAndCheckKind(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "key")
	const contents = "secret fixture"
	if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeSecurity := securityDescriptorText(t, file)
	beforeContents, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	err = fsperm.CheckOwnerOnlyFile(file)
	if err != nil && !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		t.Fatalf("check returned an unexpected error: %v", err)
	}
	if err := fsperm.CheckOwnerOnlyDir(file); !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		t.Fatalf("a file passed as a directory: %v", err)
	}
	if err := fsperm.CheckOwnerOnlyFile(root); !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		t.Fatalf("a directory passed as a file: %v", err)
	}

	afterContents, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeContents) != contents || string(afterContents) != contents {
		t.Fatal("owner-only checks changed the file contents")
	}
	if afterSecurity := securityDescriptorText(t, file); afterSecurity != beforeSecurity {
		t.Fatal("owner-only checks changed the file security descriptor")
	}
}

func TestWindowsOwnerOnlyCheckRejectsReparsePoint(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	if err := fsperm.CheckOwnerOnlyFile(link); !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		t.Fatalf("a reparse point was accepted: %v", err)
	}
}

func TestWindowsOwnerOnlyCheckRedactsMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	err := fsperm.CheckOwnerOnlyFile(missing)
	if err == nil {
		t.Fatal("a missing file was accepted")
	}
	if strings.Contains(err.Error(), missing) {
		t.Fatalf("error contains the private path: %v", err)
	}
}

func TestWindowsPrivateCreationUsesOwnerOnlyACLAndNeverRepairsCollision(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "private")
	if err := fsperm.CreatePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckOwnerOnlyDirForChildren(dir); err != nil {
		t.Fatalf("created private directory has no verified owner-only inheritance: %v", err)
	}
	dirSecurity := securityDescriptorText(t, dir)
	if err := fsperm.CreatePrivateDir(dir); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing directory creation error = %v, want os.ErrExist", err)
	}
	if after := securityDescriptorText(t, dir); after != dirSecurity {
		t.Fatal("directory collision changed its security descriptor")
	}

	file := filepath.Join(dir, "key")
	f, err := fsperm.CreatePrivateFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("unchanged")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckOwnerOnlyFile(file); err != nil {
		t.Fatalf("created private file is not owner-only: %v", err)
	}
	fileSecurity := securityDescriptorText(t, file)
	if _, err := fsperm.CreatePrivateFile(file); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing file creation error = %v, want os.ErrExist", err)
	}
	if after := securityDescriptorText(t, file); after != fileSecurity {
		t.Fatal("file collision changed its security descriptor")
	}
	contents, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "unchanged" {
		t.Fatalf("existing file contents changed: %q", contents)
	}
}

func TestWindowsChildInheritanceRequiresProtectedDACL(t *testing.T) {
	if err := fsperm.InitializeProcessOwner(); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	parent := filepath.Join(base, "private-parent")
	if err := fsperm.CreatePrivateDir(parent); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckOwnerOnlyDirForChildren(parent); err != nil {
		t.Fatalf("protected owner-only parent rejected: %v", err)
	}
	child := filepath.Join(parent, "inherited-child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	if windowsDACLProtected(t, child) {
		t.Fatal("test child unexpectedly has a protected DACL")
	}
	if err := fsperm.CheckOwnerOnlyDirForChildren(child); !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		t.Fatalf("an unprotected inherited DACL was accepted: %v", err)
	}
}

func TestWindowsProcessOwnerGuardIsReadOnly(t *testing.T) {
	before := processDefaultOwnerSID(t)
	err := fsperm.CheckProcessOwner()
	if err != nil && !errors.Is(err, fsperm.ErrNotOwnerOnly) && !errors.Is(err, fsperm.ErrUnverifiable) {
		t.Fatalf("process owner check returned an unexpected error: %v", err)
	}
	if after := processDefaultOwnerSID(t); after != before {
		t.Fatalf("read-only process owner check changed the default owner from %s to %s", before, after)
	}
}

func securityDescriptorText(t *testing.T, path string) string {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		name,
		windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	sd, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil || sd == nil {
		t.Fatalf("read security descriptor: %v", err)
	}
	if text := sd.String(); text != "" {
		return text
	}
	t.Fatal("security descriptor could not be rendered")
	return ""
}

type testTokenOwner struct {
	owner *windows.SID
}

func processDefaultOwnerSID(t *testing.T) string {
	t.Helper()
	buffer := make([]uintptr, 64)
	var returned uint32
	length := uint32(len(buffer) * int(unsafe.Sizeof(buffer[0])))
	if err := windows.GetTokenInformation(
		windows.GetCurrentProcessToken(),
		windows.TokenOwner,
		(*byte)(unsafe.Pointer(&buffer[0])),
		length,
		&returned,
	); err != nil {
		t.Fatalf("read process default owner: %v", err)
	}
	if returned < uint32(unsafe.Sizeof(testTokenOwner{})) || returned > length {
		t.Fatalf("invalid process default owner information length: %d", returned)
	}
	owner := (*testTokenOwner)(unsafe.Pointer(&buffer[0])).owner
	if owner == nil || !owner.IsValid() {
		t.Fatal("process default owner SID is invalid")
	}
	return owner.String()
}

func windowsDACLProtected(t *testing.T, path string) bool {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		name,
		windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil || sd == nil {
		t.Fatalf("read DACL control: %v", err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("read security descriptor control: %v", err)
	}
	return control&windows.SE_DACL_PROTECTED != 0
}
