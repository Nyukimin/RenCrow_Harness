//go:build unix

package fsperm_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
)

func TestOwnerOnlyDir(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "good")
	if err := os.Mkdir(good, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckOwnerOnlyDir(good); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o750, 0o705, 0o755, 0o770, 0o777} {
		open := filepath.Join(root, "open"+mode.String())
		if err := os.Mkdir(open, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(open, mode); err != nil {
			t.Fatal(err)
		}
		if err := fsperm.CheckOwnerOnlyDir(open); !errors.Is(err, fsperm.ErrNotOwnerOnly) {
			t.Fatalf("mode %v accepted: %v", mode, err)
		}
	}
	if err := fsperm.CheckOwnerOnlyDir(filepath.Join(root, "missing")); err == nil {
		t.Fatal("a missing directory was accepted")
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckOwnerOnlyDir(file); !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		t.Fatalf("a file passed as a directory: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckOwnerOnlyDir(link); !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		t.Fatalf("a symbolic link was accepted: %v", err)
	}
}

func TestOwnerOnlyFile(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "key")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckOwnerOnlyFile(f); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o666} {
		if err := os.Chmod(f, mode); err != nil {
			t.Fatal(err)
		}
		if err := fsperm.CheckOwnerOnlyFile(f); !errors.Is(err, fsperm.ErrNotOwnerOnly) {
			t.Fatalf("mode %v accepted: %v", mode, err)
		}
	}
	if err := os.Chmod(f, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckOwnerOnlyFile(f); err != nil {
		t.Fatalf("read-only owner file refused: %v", err)
	}
	if err := fsperm.CheckOwnerOnlyFile(root); !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		t.Fatalf("a directory passed as a file: %v", err)
	}
	link := filepath.Join(root, "keylink")
	if err := os.Symlink(f, link); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckOwnerOnlyFile(link); !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		t.Fatalf("a symbolic link was accepted: %v", err)
	}
}
