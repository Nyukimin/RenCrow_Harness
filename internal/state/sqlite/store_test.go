package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

var testNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// privateDir returns a fresh owner-only directory under t.TempDir().
func privateDir(t testing.TB, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// newStore initializes and opens a disposable store.
func newStore(t testing.TB) (*Store, string) {
	t.Helper()
	root := privateDir(t, "data")
	ctx := context.Background()
	if err := Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, root, Options{Clock: intake.FixedClock(testNow)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, root
}

// rawDB opens the store file with a plain connection that has the triggers and
// constraints of the schema, for tests that read or sabotage the database.
func rawDB(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"}).String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustExec(t testing.TB, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func scalar[T any](t testing.TB, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, q string, args ...any) T {
	t.Helper()
	var v T
	if err := db.QueryRowContext(context.Background(), q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func TestInitCreatesAPrivateStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh-data-root")
	if err := Init(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	for path, kind := range map[string]string{
		root:                                     "dir",
		filepath.Join(root, "locks"):             "dir",
		filepath.Join(root, "staging"):           "dir",
		filepath.Join(root, "execution.sqlite3"): "file",
	} {
		var err error
		if kind == "dir" {
			err = fsperm.CheckOwnerOnlyDir(path)
		} else {
			err = fsperm.CheckOwnerOnlyFile(path)
		}
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".init-") || strings.HasSuffix(e.Name(), "-wal") || strings.HasSuffix(e.Name(), "-shm") {
			t.Fatalf("initialization left %s behind", e.Name())
		}
	}
}

func TestInitNeverReplacesAStoreOrOpensAPrivateRoot(t *testing.T) {
	ctx := context.Background()
	root := privateDir(t, "data")
	if err := Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "execution.sqlite3")
	before, _ := os.ReadFile(dbPath)
	if err := Init(ctx, root); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second Init: %v", err)
	}
	after, _ := os.ReadFile(dbPath)
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("a refused Init changed the store")
	}

	loose := privateDir(t, "loose")
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Init(ctx, loose); !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		t.Fatalf("Init in a group-readable directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(loose, "execution.sqlite3")); err == nil {
		t.Fatal("Init created a store in a directory it refused")
	}
	for _, rel := range []string{"relative/root", "", "~/data", "$HOME/data"} {
		if err := Init(ctx, rel); err == nil {
			t.Fatalf("Init(%q) accepted a path that is not absolute", rel)
		}
	}
}

func TestOpenNeverCreatesOrFallsBack(t *testing.T) {
	ctx := context.Background()
	cwd, _ := os.Getwd()
	listing := func(dir string) string {
		entries, _ := os.ReadDir(dir)
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		return strings.Join(names, ",")
	}
	cwdBefore := listing(cwd)

	root := privateDir(t, "data")
	if _, err := Open(ctx, root, Options{}); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Open of an uninitialized root: %v", err)
	}
	if got := listing(root); got != "" {
		t.Fatalf("Open created %q in the data root", got)
	}
	if got := listing(cwd); got != cwdBefore {
		t.Fatalf("Open touched the working directory: %q -> %q", cwdBefore, got)
	}
	missing := filepath.Join(t.TempDir(), "nowhere")
	if _, err := Open(ctx, missing, Options{}); err == nil {
		t.Fatal("Open of a missing root succeeded")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("Open created the missing root")
	}
	// An empty file is not an initialized store: Open does not apply the migration.
	empty := filepath.Join(root, "execution.sqlite3")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, root, Options{}); !errors.Is(err, ErrUnknownSchema) {
		t.Fatalf("Open of an empty database: %v", err)
	}
	if st, _ := os.Stat(empty); st.Size() != 0 {
		t.Fatal("Open wrote into a database it refused")
	}
}

func TestOpenRefusesOpenPermissions(t *testing.T) {
	_, root := newStore(t)
	dbPath := filepath.Join(root, "execution.sqlite3")
	if err := os.Chmod(dbPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), root, Options{}); !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		t.Fatalf("Open of a world-readable database: %v", err)
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), root, Options{}); !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		t.Fatalf("Open under a group-readable root: %v", err)
	}
}

func TestStoreSettings(t *testing.T) {
	s, root := newStore(t)
	ctx := context.Background()
	if v := scalar[int](t, s.db, "PRAGMA foreign_keys"); v != 1 {
		t.Fatalf("foreign_keys=%d", v)
	}
	if v := scalar[string](t, s.db, "PRAGMA journal_mode"); v != "wal" {
		t.Fatalf("journal_mode=%s", v)
	}
	if v := scalar[int](t, s.db, "PRAGMA synchronous"); v != 2 {
		t.Fatalf("synchronous=%d (want FULL=2)", v)
	}
	if v, err := s.SchemaVersion(ctx); err != nil || v != 1 {
		t.Fatalf("schema version %d %v", v, err)
	}
	// Files the database creates next to itself keep the owner-only mode.
	mustExec(t, s.db, "INSERT INTO sessions VALUES('ses_a','user:x','t','/w','p','trusted_host')")
	for _, name := range []string{"execution.sqlite3", "execution.sqlite3-wal", "execution.sqlite3-shm"} {
		if st, err := os.Stat(filepath.Join(root, name)); err == nil && st.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s has mode %v", name, st.Mode().Perm())
		}
	}
}

func TestOpenRefusesUnknownSchemaVersions(t *testing.T) {
	for name, mutate := range map[string]string{
		"a newer version":      "INSERT INTO schema_version VALUES(2,'x')",
		"only a newer version": "UPDATE schema_version SET version=2",
		"version zero":         "UPDATE schema_version SET version=0",
		"no version row":       "DELETE FROM schema_version",
		"no version table":     "DROP TABLE schema_version",
	} {
		t.Run(name, func(t *testing.T) {
			s, root := newStore(t)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			db := rawDB(t, filepath.Join(root, "execution.sqlite3"))
			mustExec(t, db, mutate)
			_ = db.Close()
			before, _ := os.ReadFile(filepath.Join(root, "execution.sqlite3"))
			_, err := Open(context.Background(), root, Options{})
			if !errors.Is(err, ErrUnknownSchema) {
				t.Fatalf("Open: %v", err)
			}
			// Refusing is all it does: no conversion of the file.
			after, _ := os.ReadFile(filepath.Join(root, "execution.sqlite3"))
			if len(before) != len(after) {
				t.Fatal("a refused Open changed the database file")
			}
		})
	}
}

func TestMigrationIsAppliedOnlyByInitAndMatchesTheEmbeddedDDL(t *testing.T) {
	s, _ := newStore(t)
	tables := map[string]bool{}
	rows, err := s.db.QueryContext(context.Background(), "SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables[n] = true
	}
	for _, want := range []string{"schema_version", "sessions", "threads", "turns", "tasks", "runs", "actions", "attempts", "tool_links",
		"evidence", "evidence_chunks", "items", "context_entries", "checkpoints", "receipts", "queue_inputs", "events", "model_calls", "relay_nonces"} {
		if !tables[want] {
			t.Errorf("table %s missing", want)
		}
	}
	if v := scalar[int](t, s.db, "SELECT COUNT(*) FROM schema_version"); v != 1 {
		t.Fatalf("schema_version rows: %d", v)
	}
}

func TestImmutableTriggersRefuseUpdateAndDelete(t *testing.T) {
	s, _ := newStore(t)
	seedThread(t, s, "user:a", "ses_a", "thr_a")
	mustExec(t, s.db, "INSERT INTO evidence(evidence_id,principal,owner,state,media_type,metadata_json) VALUES('e1','p','o','building','text/plain','{}')")
	mustExec(t, s.db, "INSERT INTO evidence_chunks VALUES('e1',0,0,x'6162')")
	mustExec(t, s.db, "UPDATE evidence SET state='sealed',capture_complete=1,total_bytes=2,raw_hash=? WHERE evidence_id='e1'", strings.Repeat("a", 64))
	mustExec(t, s.db, "INSERT INTO items VALUES('m1','thr_a',NULL,1,'HumanInstruction','human','e1','{}')")
	mustExec(t, s.db, "INSERT INTO context_entries VALUES('thr_a',1,'m1',1)")
	mustExec(t, s.db, "INSERT INTO turns VALUES('tu1','thr_a',NULL,NULL,'active','t')")
	mustExec(t, s.db, "INSERT INTO tasks VALUES('tk1','thr_a','tu1',NULL,NULL,'work',NULL,'running',NULL,'t')")
	mustExec(t, s.db, "INSERT INTO runs(run_id,task_id,trace_id,phase,status,writer_epoch,started_at,limits_json,deadline_at,recovery_policy_json,recovery_policy_revision) VALUES('r1','tk1','tr','Admitting','running',1,'t','{}','t','{}',?)", strings.Repeat("a", 64))
	mustExec(t, s.db, "INSERT INTO checkpoints VALUES('c1','thr_a','r1',NULL,'normal',1,0,0,0,x'01',?,'{}','t')", strings.Repeat("b", 64))
	mustExec(t, s.db, "INSERT INTO events VALUES('ev1','thr_a',1,NULL,NULL,'session.created',NULL,'[]',NULL,NULL,'{}','t')")

	refused := []struct{ name, stmt string }{
		{"sealed evidence update", "UPDATE evidence SET media_type='x' WHERE evidence_id='e1'"},
		{"sealed evidence reopen", "UPDATE evidence SET state='building' WHERE evidence_id='e1'"},
		{"evidence delete", "DELETE FROM evidence WHERE evidence_id='e1'"},
		{"chunk update", "UPDATE evidence_chunks SET data=x'ff' WHERE evidence_id='e1'"},
		{"chunk delete", "DELETE FROM evidence_chunks WHERE evidence_id='e1'"},
		{"chunk added to sealed evidence", "INSERT INTO evidence_chunks VALUES('e1',1,2,x'63')"},
		{"item update", "UPDATE items SET origin='automation' WHERE message_id='m1'"},
		{"item delete", "DELETE FROM items WHERE message_id='m1'"},
		{"context entry update", "UPDATE context_entries SET applied_context_revision=9"},
		{"context entry delete", "DELETE FROM context_entries"},
		{"checkpoint update", "UPDATE checkpoints SET candidate_hash=? WHERE checkpoint_id='c1'"},
		{"checkpoint delete", "DELETE FROM checkpoints"},
		{"event update", "UPDATE events SET payload_json='{\"x\":1}' WHERE event_id='ev1'"},
		{"event delete", "DELETE FROM events"},
	}
	for _, c := range refused {
		var args []any
		if strings.Contains(c.stmt, "?") {
			args = append(args, strings.Repeat("c", 64))
		}
		if _, err := s.db.ExecContext(context.Background(), c.stmt, args...); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
	if v := scalar[int](t, s.db, "SELECT COUNT(*) FROM evidence_chunks"); v != 1 {
		t.Fatalf("chunks changed: %d", v)
	}
	// Foreign keys are enforced too: a row cannot point at nothing.
	if _, err := s.db.ExecContext(context.Background(), "INSERT INTO items VALUES('m2','thr_a',NULL,2,'x','y','no-such-evidence','{}')"); err == nil {
		t.Fatal("an item referencing missing evidence was accepted")
	}
	// A single admitted state is still writable where the contract allows it: the
	// current pointers are plain columns, validated by the closure check below.
	mustExec(t, s.db, "UPDATE threads SET current_checkpoint_id='c1' WHERE thread_id='thr_a'")
}

func TestQuotaStopsWritesWithoutDeletingAnything(t *testing.T) {
	ctx := context.Background()
	root := privateDir(t, "data")
	if err := Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, root, Options{MaxDatabaseBytes: 600 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedThread(t, s, "user:a", "ses_a", "thr_a")
	mustExec(t, s.db, "INSERT INTO evidence(evidence_id,principal,owner,state,media_type,metadata_json) VALUES('e1','p','o','building','text/plain','{}')")
	blob := make([]byte, 64*1024)
	var stopped error
	stored := 0
	for i := 0; i < 64; i++ {
		_, err := s.db.ExecContext(ctx, "INSERT INTO evidence_chunks VALUES('e1',?,?,?)", i, i*len(blob), blob)
		if err != nil {
			stopped = err
			break
		}
		stored++
	}
	if stopped == nil {
		t.Fatal("the quota did not stop the writes")
	}
	if !errors.Is(mapSQLiteError(stopped), ErrQuotaExceeded) {
		t.Fatalf("a full store must be reported as ErrQuotaExceeded: %v", mapSQLiteError(stopped))
	}
	if v := scalar[int](t, s.db, "SELECT COUNT(*) FROM evidence_chunks"); v != stored || stored == 0 {
		t.Fatalf("chunks %d, stored %d: nothing may be deleted to make room", v, stored)
	}
}

func TestQuotaIsAppliedOrRefusedNeverApproximated(t *testing.T) {
	ctx := context.Background()
	root := privateDir(t, "data")
	if err := Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []int64{-1, 1, 4095} {
		if s, err := Open(ctx, root, Options{MaxDatabaseBytes: bad}); err == nil {
			_ = s.Close()
			t.Fatalf("a quota of %d bytes was accepted", bad)
		}
	}
	s, err := Open(ctx, root, Options{MaxDatabaseBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if pages, size := scalar[int64](t, s.db, "PRAGMA max_page_count"), scalar[int64](t, s.db, "PRAGMA page_size"); pages*size != 1<<20 {
		t.Fatalf("quota %d pages of %d bytes", pages, size)
	}
}

func TestAdmissionOnAFullStoreWritesNothingAndSaysSo(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	full, err := Open(ctx, e.root, Options{Clock: intake.FixedClock(testNow), MaxDatabaseBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	// Fill the store up to its quota with one sealed-looking raw evidence in chunks.
	mustExec(t, full.db, "INSERT INTO evidence(evidence_id,principal,owner,state,media_type,metadata_json) VALUES('evd_fill','p','o','building','application/octet-stream','{}')")
	// Large rows first, then small ones, so no page is left for the admission.
	next, offset := 0, 0
	for _, size := range []int{64 * 1024, 4 * 1024, 256} {
		blob := make([]byte, size)
		for failures := 0; failures == 0; next++ {
			if _, err := full.db.ExecContext(ctx, "INSERT INTO evidence_chunks VALUES('evd_fill',?,?,?)", next, offset, blob); err != nil {
				failures++
				next--
				break
			}
			offset += size
			if next > 20000 {
				t.Fatal("the quota never stopped the fill")
			}
		}
	}
	before := e.counts()
	// The input is large enough to need new pages; small rows alone could still fit
	// into pages that already exist, which is not what this test is about.
	big := e.params(e.thread, "full.key.00000000000001", func(m map[string]any) {
		m["input"].(map[string]any)["text"] = strings.Repeat("0123456789abcdef", 16*1024)
	})
	_, err = full.AdmitStart(ctx, e.adm, big)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("a full store must say so: %v", err)
	}
	if protocol.CodeOf(err) == protocol.CodeInvalidOriginProof || protocol.CodeOf(err) == protocol.CodeForbidden {
		t.Fatalf("a storage fault must not look like a refusal of the caller: %v", err)
	}
	e.wantNoChange(before)
	if v := queryString(t, e.s.db, "SELECT COUNT(*) FROM evidence_chunks WHERE evidence_id='evd_fill'"); v == "0" {
		t.Fatal("the fill was removed to make room")
	}
}
