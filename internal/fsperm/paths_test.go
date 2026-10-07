package fsperm_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
)

func TestWithinAndOverlaps(t *testing.T) {
	sep := string(filepath.Separator)
	root := sep + "a" + sep + "b"
	cases := []struct {
		child, parent string
		within        bool
	}{
		{root, root, true},
		{root + sep + "c", root, true},
		{root + sep + "c" + sep + "d", root, true},
		{sep + "a", root, false},
		{root + "x", root, false}, // a sibling that shares a prefix is not inside
		{sep + "a" + sep + "bb", root, false},
		{sep + "z", root, false},
	}
	for _, c := range cases {
		if got := fsperm.Within(c.child, c.parent); got != c.within {
			t.Errorf("Within(%q, %q) = %v", c.child, c.parent, got)
		}
	}
	if !fsperm.Overlaps(root, root+sep+"c") || !fsperm.Overlaps(root+sep+"c", root) || fsperm.Overlaps(root, sep+"a"+sep+"bb") {
		t.Error("Overlaps must hold both ways and not for prefix siblings")
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		if !fsperm.Within(sep+"A"+sep+"B"+sep+"c", root) {
			t.Error("a path differing only in case is inside on a case-insensitive default file system")
		}
	}
}

func TestResolveAheadResolvesLinksWithoutCreatingAnything(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got, err := fsperm.ResolveAhead(filepath.Join(link, "x", "y"))
	if err != nil || got != filepath.Join(real, "x", "y") {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(real, "x")); err == nil {
		t.Fatal("ResolveAhead created a directory")
	}
	if got, err := fsperm.ResolveAhead(real); err != nil || got != real {
		t.Fatalf("an existing path resolves to itself: %q %v", got, err)
	}
}
