package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/oslock"
)

func TestLockPathsStayInsideTheLocksDirectory(t *testing.T) {
	root := privateDir(t, "data")
	if got := MigrationLockPath(root); got != filepath.Join(root, "locks", "migration.lock") {
		t.Fatalf("migration lock path %s", got)
	}
	thread := "thr_00000000-0000-7000-8000-000000000001"
	got, err := ThreadLockPath(root, thread)
	if err != nil || got != filepath.Join(root, "locks", "thread-"+thread+".lock") {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"", "../x", "thr_x", "thr_00000000-0000-7000-8000-000000000001/../../x", "run_00000000-0000-7000-8000-000000000001", "thr_00000000-0000-7000-8000-000000000001\x00"} {
		if p, err := ThreadLockPath(root, bad); err == nil {
			t.Errorf("ThreadLockPath(%q) = %q, want an error", bad, p)
		}
	}
}

func TestInitTakesTheMigrationLockAndReleasesIt(t *testing.T) {
	ctx := context.Background()
	root := privateDir(t, "data")
	if err := os.Mkdir(filepath.Join(root, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Another process holds the lock (a running driver, or another initialization).
	held, err := oslock.TryLock(MigrationLockPath(root), oslock.Shared)
	if err != nil {
		t.Fatal(err)
	}
	err = Init(ctx, root)
	if !errors.Is(err, ErrMigrationBusy) {
		t.Fatalf("Init under a held migration lock: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, DatabaseFile)); statErr == nil {
		t.Fatal("Init created a store without the migration lock")
	}
	_ = held.Release()
	if err := Init(ctx, root); err != nil {
		t.Fatalf("Init after the lock was released: %v", err)
	}
	// Init has released the lock again: a driver can hold it shared.
	l, err := LockMigrationShared(root)
	if err != nil {
		t.Fatalf("the migration lock was left held by Init: %v", err)
	}
	defer l.Release()
}

func TestADriverHoldsTheMigrationLockSharedAndBlocksMigration(t *testing.T) {
	_, root := newStore(t)
	a, err := LockMigrationShared(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LockMigrationShared(root)
	if err != nil {
		t.Fatalf("two drivers must coexist: %v", err)
	}
	if _, err := oslock.TryLock(MigrationLockPath(root), oslock.Exclusive); !errors.Is(err, oslock.ErrLocked) {
		t.Fatalf("a migration while drivers run: %v", err)
	}
	_ = a.Release()
	_ = b.Release()
	m, err := oslock.TryLock(MigrationLockPath(root), oslock.Exclusive)
	if err != nil {
		t.Fatalf("migration lock after the drivers stopped: %v", err)
	}
	_ = m.Release()
	// A data root without a locks directory is not an initialized one: no lock is made up.
	other := privateDir(t, "empty")
	if _, err := LockMigrationShared(other); err == nil || strings.Contains(err.Error(), other) {
		t.Fatalf("lock in an uninitialized root: %v", err)
	}
}
