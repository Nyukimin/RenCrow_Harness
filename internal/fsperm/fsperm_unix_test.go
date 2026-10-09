//go:build unix

package fsperm_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
)

func TestPrivateCreationIsExclusiveAndOwnerOnly(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "private")
	if err := fsperm.CreatePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("private directory mode grants group/other access: %04o", info.Mode().Perm())
	}
	if err := fsperm.CheckOwnerOnlyDir(dir); err != nil {
		t.Fatalf("created directory is not owner-only: %v", err)
	}
	modeBefore := info.Mode().Perm()
	if err := fsperm.CreatePrivateDir(dir); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing directory creation error = %v, want os.ErrExist", err)
	}
	info, err = os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != modeBefore {
		t.Fatalf("existing directory mode changed from %04o to %04o", modeBefore, info.Mode().Perm())
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
	info, err = os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("private file mode grants group/other access: %04o", info.Mode().Perm())
	}
	if err := fsperm.CheckOwnerOnlyFile(file); err != nil {
		t.Fatalf("created file is not owner-only: %v", err)
	}
	if _, err := fsperm.CreatePrivateFile(file); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing file creation error = %v, want os.ErrExist", err)
	}
	contents, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "unchanged" {
		t.Fatalf("existing file contents changed: %q", contents)
	}
}
