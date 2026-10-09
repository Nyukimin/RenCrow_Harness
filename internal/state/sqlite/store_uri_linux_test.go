//go:build linux

package sqlite

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
)

func TestSQLiteURIStoreLifecycleOnSpecialNativePaths(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "データ space #percent% question?")
	if err := Init(ctx, root); err != nil {
		t.Fatalf("initialize store at special path: %v", err)
	}

	store, err := Open(ctx, root, Options{Clock: intake.FixedClock(testNow)})
	if err != nil {
		t.Fatalf("open store at special path: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store before reopen: %v", err)
	}
	store, err = Open(ctx, root, Options{Clock: intake.FixedClock(testNow)})
	if err != nil {
		t.Fatalf("reopen store at special path: %v", err)
	}
	defer store.Close()

	backupRoot := filepath.Join(t.TempDir(), "backup 日本語 space #percent% question?")
	receipt, err := store.Backup(ctx, backupRoot)
	if err != nil {
		t.Fatalf("back up store to special path: %v", err)
	}
	report, err := VerifyBackupFile(ctx, receipt.Path)
	if err != nil {
		t.Fatalf("verify backup at special path: %v", err)
	}
	if !report.OK {
		t.Fatalf("backup closure failed: %+v", report)
	}
}

func TestSQLiteURIPreservesPOSIXLiteralBackslash(t *testing.T) {
	path := filepath.Join(t.TempDir(), `literal\backslash.db`)
	uri := sqliteFileURI(path, "_pragma=foreign_keys(1)")
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse file URI: %v", err)
	}
	if parsed.Host != "" || parsed.Path != path {
		t.Fatalf("URI path = %q with host %q, want literal POSIX path %q (%s)", parsed.Path, parsed.Host, path, uri)
	}
	if !strings.Contains(strings.ToUpper(uri), "%5C") {
		t.Fatalf("URI %q does not encode the literal backslash as a path byte", uri)
	}
}
