//go:build windows

package sqlite

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
	"golang.org/x/sys/windows"
)

func TestInitRejectsPrivateRootWithoutChildInheritanceBeforeWriting(t *testing.T) {
	if err := fsperm.InitializeProcessOwner(); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		t.Fatalf("read current user SID: %v", err)
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + "D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatalf("build fixture security descriptor: %v", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatalf("read fixture owner: %v", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("read fixture DACL: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(
		root,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner,
		nil,
		dacl,
		nil,
	); err != nil {
		t.Fatalf("set non-inheriting fixture DACL: %v", err)
	}
	before, err := windows.GetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || before == nil {
		t.Fatalf("capture fixture security descriptor: %v", err)
	}

	err = Init(context.Background(), root)
	if !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		t.Fatalf("Init error = %v, want ErrNotOwnerOnly", err)
	}
	for _, path := range []string{filepath.Join(root, "locks"), filepath.Join(root, "staging"), filepath.Join(root, DatabaseFile)} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("Init wrote %q before refusing root inheritance: %v", filepath.Base(path), statErr)
		}
	}
	after, err := windows.GetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || after == nil {
		t.Fatalf("read fixture security descriptor after Init: %v", err)
	}
	if before.String() != after.String() {
		t.Fatal("Init changed the existing root security descriptor")
	}
}

func TestSQLiteURIWindowsDriveAndUNCPathsHaveNoAuthority(t *testing.T) {
	extra := []string{"mode=ro", "immutable=1"}
	wantQuery := "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&mode=ro&immutable=1"
	drivePath := filepath.Join(t.TempDir(), "日本語 space #percent%.db")
	driveURI := dsn(drivePath, extra...)
	driveURL, err := url.Parse(driveURI)
	if err != nil {
		t.Fatalf("parse drive file URI: %v", err)
	}
	wantDrivePath := "/" + filepath.ToSlash(drivePath)
	if driveURL.Scheme != "file" || driveURL.Host != "" || driveURL.Fragment != "" || driveURL.Path != wantDrivePath {
		t.Fatalf("drive file URI parsed as scheme=%q host=%q path=%q fragment=%q, want path %q (%s)",
			driveURL.Scheme, driveURL.Host, driveURL.Path, driveURL.Fragment, wantDrivePath, driveURI)
	}
	if driveURL.RawQuery != wantQuery {
		t.Fatalf("drive URI query = %q, want %q", driveURL.RawQuery, wantQuery)
	}
	for _, escaped := range []string{"%20", "%23", "%25", "%E6%97%A5"} {
		if !strings.Contains(strings.ToUpper(driveURI), escaped) {
			t.Fatalf("drive URI %q does not escape path character as %s", driveURI, escaped)
		}
	}

	uncPath := `\\server\share\日本語 space #percent%.db`
	uncURI := dsn(uncPath, extra...)
	uncURL, err := url.Parse(uncURI)
	if err != nil {
		t.Fatalf("parse UNC file URI: %v", err)
	}
	wantUNCPath := filepath.ToSlash(uncPath)
	if uncURL.Scheme != "file" || uncURL.Host != "" || uncURL.Fragment != "" || uncURL.Path != wantUNCPath {
		t.Fatalf("UNC file URI parsed as scheme=%q host=%q path=%q fragment=%q, want path %q (%s)",
			uncURL.Scheme, uncURL.Host, uncURL.Path, uncURL.Fragment, wantUNCPath, uncURI)
	}
	if uncURL.RawQuery != wantQuery {
		t.Fatalf("UNC URI query = %q, want %q", uncURL.RawQuery, wantQuery)
	}
}
