package sqlite

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// BackupReceipt describes one finished, verified backup.
type BackupReceipt struct {
	Path          string
	Bytes         int64
	SHA256        string
	SchemaVersion int
	CreatedAt     string
	Closure       ClosureReport
}

// Backup (F28) writes a consistent copy of the store into backupRoot and verifies
// it before reporting success.
//
// The copy is made by SQLite itself (VACUUM INTO), which reads one consistent
// snapshot of the committed state: the live files are never copied, so a backup
// taken while writers run is still a recovery point, not a torn file. The target
// is a new owner-only file in an owner-only directory outside the data root, and
// is never an existing file. The finished file is then opened read-only and put
// through the restore-closure check; a backup that fails it is removed and
// reported, never kept as a recovery point.
func (s *Store) Backup(ctx context.Context, backupRoot string) (BackupReceipt, error) {
	if err := fsperm.CheckProcessOwner(); err != nil {
		return BackupReceipt{}, fmt.Errorf("sqlite: process owner: %w", err)
	}
	if backupRoot == "" || !filepath.IsAbs(backupRoot) || filepath.Clean(backupRoot) != backupRoot || strings.ContainsRune(backupRoot, 0) {
		return BackupReceipt{}, errors.New("sqlite: backup root must be an explicit absolute path")
	}
	// Judge where the directory would land before anything is created, following any
	// symbolic link among its ancestors: a link into the data root must be refused
	// without having made a directory there.
	ahead, err := fsperm.ResolveAhead(backupRoot)
	if err != nil {
		return BackupReceipt{}, fmt.Errorf("sqlite: backup root: %w", fsperm.WithoutPath(err))
	}
	if fsperm.Overlaps(ahead, s.root) {
		return BackupReceipt{}, ErrBackupLocation
	}
	if _, err := os.Lstat(backupRoot); errors.Is(err, os.ErrNotExist) {
		if err := createPrivateDirAll(backupRoot); err != nil {
			return BackupReceipt{}, fmt.Errorf("sqlite: backup root: %w", fsperm.WithoutPath(err))
		}
	} else if err != nil {
		return BackupReceipt{}, fmt.Errorf("sqlite: backup root: %w", fsperm.WithoutPath(err))
	}
	real, err := filepath.EvalSymlinks(backupRoot)
	if err != nil {
		return BackupReceipt{}, fmt.Errorf("sqlite: backup root: %w", fsperm.WithoutPath(err))
	}
	if fsperm.Overlaps(real, s.root) {
		return BackupReceipt{}, ErrBackupLocation
	}
	if err := fsperm.CheckOwnerOnlyDirForChildren(real); err != nil {
		return BackupReceipt{}, fmt.Errorf("sqlite: backup root: %w", err)
	}

	now := s.now()
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return BackupReceipt{}, fmt.Errorf("sqlite: random: %w", err)
	}
	name := fmt.Sprintf("execution-%s-%s.sqlite3", now.Format("20060102T150405Z"), hex.EncodeToString(suffix[:]))
	target := filepath.Join(real, name)

	// VACUUM INTO accepts a target only if it is empty or missing. Creating it first
	// with the private creator means it is never visible with wider rights.
	f, err := fsperm.CreatePrivateFile(target)
	if err != nil {
		return BackupReceipt{}, fmt.Errorf("sqlite: backup target: %w", fsperm.WithoutPath(err))
	}
	if err := f.Close(); err != nil {
		return BackupReceipt{}, fmt.Errorf("sqlite: backup target: %w", fsperm.WithoutPath(err))
	}
	if err := fsperm.CheckOwnerOnlyFile(target); err != nil {
		return BackupReceipt{}, fmt.Errorf("sqlite: backup target: %w", fsperm.WithoutPath(err))
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(target)
		}
	}()

	if err := checkSQLiteSidecars(target); err != nil {
		return BackupReceipt{}, err
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", target); err != nil {
		return BackupReceipt{}, fmt.Errorf("sqlite: backup: %w", mapSQLiteError(err))
	}
	if err := fsperm.CheckOwnerOnlyFile(target); err != nil {
		return BackupReceipt{}, fmt.Errorf("sqlite: backup target: %w", fsperm.WithoutPath(err))
	}
	if err := checkSQLiteSidecars(target); err != nil {
		return BackupReceipt{}, err
	}
	if err := makeWAL(ctx, target); err != nil {
		return BackupReceipt{}, err
	}
	report, err := VerifyBackupFile(ctx, target)
	if err != nil {
		return BackupReceipt{}, err
	}
	if !report.OK {
		return BackupReceipt{Closure: report}, fmt.Errorf("%w: %s", ErrBackupInvalid, strings.Join(report.Issues, "; "))
	}
	sum, size, err := hashAndSync(target)
	if err != nil {
		return BackupReceipt{}, err
	}
	keep = true
	return BackupReceipt{
		Path: target, Bytes: size, SHA256: sum, SchemaVersion: SchemaVersion,
		CreatedAt: protocol.FormatTimestamp(now), Closure: report,
	}, nil
}

// makeWAL gives the finished copy the journal mode of a store. VACUUM INTO writes a
// rollback-journal database; Open requires WAL and never converts, so the copy is
// converted here, once, and checkpointed so it is a single self-contained file.
func makeWAL(ctx context.Context, path string) error {
	if err := fsperm.CheckOwnerOnlyFile(path); err != nil {
		return fmt.Errorf("sqlite: backup target: %w", fsperm.WithoutPath(err))
	}
	if err := checkSQLiteSidecars(path); err != nil {
		return err
	}
	db, err := openDB(dsn(path))
	if err != nil {
		return fmt.Errorf("sqlite: backup: %w", err)
	}
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil || !strings.EqualFold(mode, "wal") {
		_ = db.Close()
		return fmt.Errorf("sqlite: backup: could not set WAL mode (%v)", mapSQLiteError(err))
	}
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		_ = db.Close()
		return fmt.Errorf("sqlite: backup: %w", mapSQLiteError(err))
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("sqlite: backup: %w", err)
	}
	if err := checkSQLiteSidecars(path); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if st, err := os.Stat(path + suffix); err == nil {
			if st.Size() > 0 && suffix == "-wal" {
				return fmt.Errorf("sqlite: backup: the copy kept uncheckpointed data")
			}
			_ = os.Remove(path + suffix)
		}
	}
	return nil
}

// hashAndSync returns the SHA-256 and size of a file and flushes it to stable storage.
func hashAndSync(path string) (string, int64, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return "", 0, fmt.Errorf("sqlite: backup: %w", fsperm.WithoutPath(err))
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("sqlite: backup: %w", fsperm.WithoutPath(err))
	}
	if err := f.Sync(); err != nil {
		return "", 0, fmt.Errorf("sqlite: backup: %w", fsperm.WithoutPath(err))
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
