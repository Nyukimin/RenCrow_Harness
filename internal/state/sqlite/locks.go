package sqlite

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/oslock"
)

const (
	locksDirName  = "locks"
	migrationLock = "migration.lock"
)

// ErrMigrationBusy is returned when the data root's migration lock is held by
// another process: a running driver (which holds it shared) or another
// initialization or migration (which holds it exclusively).
var ErrMigrationBusy = errors.New("sqlite: the data root's migration lock is held by another process")

// MigrationLockPath is the lock every driver holds shared for as long as it runs
// and that schema initialization and migration hold exclusively, so a migration
// never runs under a live driver. root is the real data root.
func MigrationLockPath(root string) string {
	return filepath.Join(root, locksDirName, migrationLock)
}

// ThreadLockPath is the writer lock of one Thread. The Thread ID is parsed
// strictly, so the file name can only be thread-<canonical ID>.lock inside the
// locks directory, whatever the caller passes.
func ThreadLockPath(root, threadID string) (string, error) {
	id, err := identity.ParseThreadID(threadID)
	if err != nil {
		return "", fmt.Errorf("sqlite: thread lock: %w", err)
	}
	return filepath.Join(root, locksDirName, "thread-"+id.String()+".lock"), nil
}

// LockMigrationShared takes the migration lock shared, without waiting. The
// caller keeps it until the process stops driving the store. The locks directory
// must already exist and be owner-only (Init makes it): nothing is created here.
func LockMigrationShared(root string) (*oslock.Lock, error) {
	if err := fsperm.CheckOwnerOnlyDir(filepath.Join(root, locksDirName)); err != nil {
		return nil, fmt.Errorf("sqlite: locks directory: %w", fsperm.WithoutPath(err))
	}
	l, err := oslock.TryLock(MigrationLockPath(root), oslock.Shared)
	if errors.Is(err, oslock.ErrLocked) {
		return nil, ErrMigrationBusy
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: migration lock: %w", err)
	}
	return l, nil
}

// WorkspaceLockPath is the lock a Run that may change a workspace holds exclusively
// for as long as it runs. It is named by the digest of the workspace's real root, so
// two Threads (or two processes) that work in the same directory meet at one file
// whatever way they spell it. root is the real data root and workspaceRoot the real
// workspace root.
func WorkspaceLockPath(root, workspaceRoot string) (string, error) {
	if workspaceRoot == "" || !filepath.IsAbs(workspaceRoot) {
		return "", errors.New("sqlite: workspace lock: the workspace root is not an absolute path")
	}
	sum := sha256.Sum256([]byte(filepath.Clean(workspaceRoot)))
	return filepath.Join(root, locksDirName, "workspace-"+hex.EncodeToString(sum[:])+".lock"), nil
}
