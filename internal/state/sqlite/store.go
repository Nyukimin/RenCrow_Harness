// Package sqlite is the Harness-owned execution store: one local SQLite database
// under an explicit, private data root.
//
// The store never decides where it lives. Init creates a new store only in the
// data root it is given and Open opens an existing one; neither falls back to the
// repository, the home directory or a temporary directory, and Open never creates
// or converts anything. A store whose schema version this build does not know is
// refused, not migrated at startup.
//
// Every write that must be atomic runs in one BEGIN IMMEDIATE transaction
// (WAL, synchronous=FULL, foreign keys on), so a writer takes the database write
// lock before it reads, and what it read is still true when it writes. Tests that
// need a database use a throw-away store in t.TempDir().
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	msqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	harness "github.com/Nyukimin/RenCrow_Harness"
	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/internal/oslock"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

const (
	// DatabaseFile is the store's file name under the data root.
	DatabaseFile = "execution.sqlite3"
	// SchemaVersion is the only schema version this build reads and writes.
	SchemaVersion = 1

	migrationFile = "migrations/001_initial.sql"
	busyTimeoutMS = 5000
)

var (
	// ErrNotInitialized is returned by Open when the data root holds no store.
	ErrNotInitialized = errors.New("sqlite: the data root holds no initialized store")
	// ErrAlreadyInitialized is returned by Init when a store file already exists.
	ErrAlreadyInitialized = errors.New("sqlite: the data root already holds a store")
	// ErrUnknownSchema is returned when the file is not a store of schema version 1.
	ErrUnknownSchema = errors.New("sqlite: unknown or missing schema version")
	// ErrQuotaExceeded is returned when the store reached its size limit. Nothing is
	// deleted to make room.
	ErrQuotaExceeded = errors.New("sqlite: the store reached its size quota")
	// ErrBackupLocation is returned when a backup would be written inside the data root.
	ErrBackupLocation = errors.New("sqlite: the backup location overlaps the data root")
	// ErrBackupInvalid is returned when a finished backup fails its restore-closure check.
	ErrBackupInvalid = errors.New("sqlite: the backup fails the restore-closure check")
)

// Options configure Open.
type Options struct {
	// Clock supplies acceptance time; the zero value uses the system clock.
	Clock intake.Clock
	// MaxDatabaseBytes, when positive, is the store's size quota, enforced by
	// SQLite itself (max_page_count): a write that would exceed it fails and no
	// data is removed.
	MaxDatabaseBytes int64
	// Fault is a test seam and is nil in every production composition. It is asked at a
	// few named points of a checkpoint commit; an error it returns makes the commit fail
	// there ("checkpoint.before_commit": the transaction is rolled back) or lose its
	// answer ("checkpoint.after_commit": the transaction took effect and the caller is
	// told it is uncertain).
	Fault func(point string) error
}

// Store is an open execution store. It is safe for concurrent use; writes are
// serialized by SQLite's write lock.
type Store struct {
	db    *sql.DB
	root  string
	path  string
	clock intake.Clock
	fault func(point string) error
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Root returns the real data root path.
func (s *Store) Root() string { return s.root }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func dsn(path string, extra ...string) string {
	q := append([]string{"_pragma=foreign_keys(1)", "_pragma=busy_timeout(" + fmt.Sprint(busyTimeoutMS) + ")"}, extra...)
	return (&url.URL{Scheme: "file", Path: path, RawQuery: strings.Join(q, "&")}).String()
}

// openDB opens a one-connection pool: SQLite serializes writers anyway, and one
// connection keeps per-connection settings (foreign keys, quota) uniform.
func openDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	return db, nil
}

func resolveDataRoot(dataRoot string) (string, error) {
	if dataRoot == "" || !filepath.IsAbs(dataRoot) || strings.ContainsRune(dataRoot, 0) {
		return "", fmt.Errorf("sqlite: data root must be an explicit absolute path")
	}
	if filepath.Clean(dataRoot) != dataRoot {
		return "", fmt.Errorf("sqlite: data root must be written in clean form")
	}
	return dataRoot, nil
}

// Init creates a new store in dataRoot. The directory is created (owner-only) if
// it is missing; if it exists it must already be owner-only and must not already
// hold a store. The schema is applied to a private temporary file which is only
// linked to its final name once it has been verified, so a failed Init never
// leaves a half-built store, and an existing store is never replaced.
func Init(ctx context.Context, dataRoot string) error {
	root, err := resolveDataRoot(dataRoot)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return fmt.Errorf("sqlite: create data root: %w", fsperm.WithoutPath(err))
		}
		if err := os.Chmod(root, 0o700); err != nil {
			return fmt.Errorf("sqlite: data root permissions: %w", fsperm.WithoutPath(err))
		}
	} else if err != nil {
		return fmt.Errorf("sqlite: data root: %w", fsperm.WithoutPath(err))
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("sqlite: data root: %w", fsperm.WithoutPath(err))
	}
	if err := fsperm.CheckOwnerOnlyDir(real); err != nil {
		return fmt.Errorf("sqlite: data root: %w", fsperm.WithoutPath(err))
	}
	final := filepath.Join(real, DatabaseFile)
	if _, err := os.Lstat(final); err == nil {
		return ErrAlreadyInitialized
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sqlite: %w", fsperm.WithoutPath(err))
	}
	for _, sub := range []string{"locks", "staging"} {
		p := filepath.Join(real, sub)
		if err := os.Mkdir(p, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("sqlite: %w", fsperm.WithoutPath(err))
		}
		if err := fsperm.CheckOwnerOnlyDir(p); err != nil {
			return fmt.Errorf("sqlite: %s: %w", sub, err)
		}
	}

	// Initialization is a migration: it holds the exclusive migration lock, so it
	// can neither run under a live driver nor race another initialization.
	gate, err := oslock.TryLock(MigrationLockPath(real), oslock.Exclusive)
	if errors.Is(err, oslock.ErrLocked) {
		return ErrMigrationBusy
	}
	if err != nil {
		return fmt.Errorf("sqlite: migration lock: %w", err)
	}
	defer gate.Release()

	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("sqlite: random: %w", fsperm.WithoutPath(err))
	}
	tmp := filepath.Join(real, DatabaseFile+".init-"+hex.EncodeToString(suffix[:]))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("sqlite: %w", fsperm.WithoutPath(err))
	}
	_ = f.Close()
	defer func() {
		for _, p := range []string{tmp, tmp + "-wal", tmp + "-shm", tmp + "-journal"} {
			_ = os.Remove(p)
		}
	}()

	ddl, err := harness.Migrations.ReadFile(migrationFile)
	if err != nil {
		return fmt.Errorf("sqlite: embedded migration: %w", err)
	}
	db, err := openDB(dsn(tmp))
	if err != nil {
		return fmt.Errorf("sqlite: %w", fsperm.WithoutPath(err))
	}
	if _, err := db.ExecContext(ctx, string(ddl)); err != nil {
		_ = db.Close()
		return fmt.Errorf("sqlite: apply schema: %w", err)
	}
	s := &Store{db: db, root: real, path: tmp, clock: intake.SystemClock{}}
	if err := s.verifySettings(ctx, 0); err != nil {
		_ = db.Close()
		return err
	}
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		_ = db.Close()
		return fmt.Errorf("sqlite: checkpoint: %w", err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("sqlite: %w", fsperm.WithoutPath(err))
	}
	// Link, not rename: linking fails if the name appeared in the meantime, so a
	// concurrent Init cannot be overwritten.
	if err := os.Link(tmp, final); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrAlreadyInitialized
		}
		return fmt.Errorf("sqlite: install store: %w", fsperm.WithoutPath(err))
	}
	return nil
}

// Open opens an existing store. It checks that the data root and the database
// file are owner-only, that the file is a SQLite database of schema version 1 in
// WAL mode, and applies the connection settings. It never creates a file and never
// writes to a database it refuses.
func Open(ctx context.Context, dataRoot string, opts Options) (*Store, error) {
	root, err := resolveDataRoot(dataRoot)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("sqlite: data root: %w", fsperm.WithoutPath(err))
	}
	if err := fsperm.CheckOwnerOnlyDir(real); err != nil {
		return nil, fmt.Errorf("sqlite: data root: %w", fsperm.WithoutPath(err))
	}
	path := filepath.Join(real, DatabaseFile)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotInitialized
	} else if err != nil {
		return nil, fmt.Errorf("sqlite: %w", fsperm.WithoutPath(err))
	}
	if err := fsperm.CheckOwnerOnlyFile(path); err != nil {
		return nil, fmt.Errorf("sqlite: database file: %w", err)
	}
	// Look at the header before connecting: connecting to an empty or foreign file
	// would let SQLite write to it.
	pageSize, err := readPageSize(path)
	if err != nil {
		return nil, err
	}
	extra := []string{"_pragma=synchronous(2)", "_txlock=immediate"}
	var quotaPages int64
	if opts.MaxDatabaseBytes < 0 {
		return nil, errors.New("sqlite: the size quota must not be negative")
	}
	if opts.MaxDatabaseBytes > 0 {
		// A quota that rounds down to no page at all would silently be a different,
		// larger limit; it is refused instead.
		if quotaPages = opts.MaxDatabaseBytes / pageSize; quotaPages < 1 {
			return nil, errors.New("sqlite: the size quota is smaller than one database page")
		}
		extra = append(extra, fmt.Sprintf("_pragma=max_page_count(%d)", quotaPages))
	}
	db, err := openDB(dsn(path, extra...))
	if err != nil {
		return nil, fmt.Errorf("sqlite: %w", fsperm.WithoutPath(err))
	}
	clock := opts.Clock
	if clock == nil {
		clock = intake.SystemClock{}
	}
	s := &Store{db: db, root: real, path: path, clock: clock, fault: opts.Fault}
	if err := s.verifySettings(ctx, quotaPages); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// readPageSize returns the page size recorded in the SQLite file header, and
// refuses anything that is not a SQLite database file.
func readPageSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("sqlite: %w", fsperm.WithoutPath(err))
	}
	defer f.Close()
	var hdr [100]byte
	if n, _ := f.ReadAt(hdr[:], 0); n < len(hdr) || string(hdr[:16]) != "SQLite format 3\x00" {
		return 0, ErrUnknownSchema
	}
	size := int64(binary.BigEndian.Uint16(hdr[16:18]))
	if size == 1 {
		size = 65536
	}
	if size < 512 {
		return 0, ErrUnknownSchema
	}
	return size, nil
}

// verifySettings checks the connection settings the contract requires and the
// schema version, so a store opened under other settings is refused rather than
// quietly weaker.
func (s *Store) verifySettings(ctx context.Context, wantMaxPages int64) error {
	var fk, sync int
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		return fmt.Errorf("sqlite: foreign keys are not enforced (%v)", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("sqlite: the store is not in WAL mode (%v)", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil || sync != 2 {
		return fmt.Errorf("sqlite: synchronous is not FULL (%v)", err)
	}
	if wantMaxPages > 0 {
		var got int64
		if err := s.db.QueryRowContext(ctx, "PRAGMA max_page_count").Scan(&got); err != nil || got != wantMaxPages {
			return fmt.Errorf("sqlite: the size quota was not applied (%v)", err)
		}
	}
	if v, err := s.SchemaVersion(ctx); err != nil {
		return err
	} else if v != SchemaVersion {
		return ErrUnknownSchema
	}
	return s.verifyObjects(ctx)
}

// SchemaVersion returns the store's schema version. A store with no version
// table, no row, or more than one row is ErrUnknownSchema.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT version FROM schema_version")
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnknownSchema, mapSQLiteError(err))
	}
	defer rows.Close()
	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return 0, fmt.Errorf("%w: %v", ErrUnknownSchema, err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil || len(versions) != 1 {
		return 0, ErrUnknownSchema
	}
	return versions[0], nil
}

// now returns the acceptance time from the injected clock, in UTC.
func (s *Store) now() time.Time { return s.clock.Now().UTC() }

// mapSQLiteError turns the SQLite codes the contract cares about into the typed
// errors callers branch on: a busy database is retryable BUSY, a full one is
// ErrQuotaExceeded. Everything else is returned unchanged.
func mapSQLiteError(err error) error {
	if err == nil {
		return nil
	}
	var se *msqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return protocol.NewError(protocol.CodeBusy, "the store is busy").AsRetryable().Wrap(err)
		case sqlite3.SQLITE_FULL:
			// Either the configured quota or the disk itself: in both cases nothing
			// further can be written and nothing is deleted to make room.
			return protocol.NewError(protocol.CodeInternal, "the store is full; nothing was written").Wrap(fmt.Errorf("%w: %w", ErrQuotaExceeded, err))
		case sqlite3.SQLITE_NOTADB:
			return fmt.Errorf("%w: %w", ErrUnknownSchema, err)
		}
	}
	return err
}

// isPrimaryKeyViolation reports a primary key or unique constraint failure.
func isPrimaryKeyViolation(err error) bool {
	var se *msqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	return se.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY || se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
