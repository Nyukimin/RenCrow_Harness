package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func TestBackupIsAVerifiedConsistentCopy(t *testing.T) {
	e := newEnv(t)
	e.mustAdmit(e.thread, "backup.key.00000000001", nil)
	_, other := e.newThread("core:local")
	e.mustAdmit(other, "backup.key.00000000002", nil)
	backups := privateDir(t, "backups")

	r, err := e.s.Backup(context.Background(), backups)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(r.Path) != backups || strings.HasPrefix(r.Path, e.root) {
		t.Fatalf("backup at %s", r.Path)
	}
	if err := fsperm.CheckOwnerOnlyFile(r.Path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if int64(len(raw)) != r.Bytes || hex.EncodeToString(sum[:]) != r.SHA256 || r.SchemaVersion != 1 {
		t.Fatalf("receipt %+v does not describe the file", r)
	}
	if !r.Closure.OK || r.Closure.Counts["runs"] != 2 || r.Closure.Counts["events"] != 6 || r.Closure.Counts["items"] != 10 {
		t.Fatalf("closure of the backup: %+v", r.Closure)
	}
	// The same check is available on any file at rest, and leaves nothing beside it.
	before, _ := os.ReadDir(backups)
	report, err := VerifyBackupFile(context.Background(), r.Path)
	after, _ := os.ReadDir(backups)
	if err != nil || !report.OK || len(before) != len(after) {
		t.Fatalf("%v %+v (%d -> %d files)", err, report, len(before), len(after))
	}
	// A second backup is a second file: nothing is overwritten.
	r2, err := e.s.Backup(context.Background(), backups)
	if err != nil || r2.Path == r.Path {
		t.Fatalf("%v %s", err, r2.Path)
	}
	if again, _ := os.ReadFile(r.Path); string(again) != string(raw) {
		t.Fatal("a later backup changed an earlier one")
	}
}

func TestBackupCreatesItsPrivateRootAndRefusesOpenOnes(t *testing.T) {
	e := newEnv(t)
	base := t.TempDir()
	fresh := filepath.Join(base, "a", "b", "backups")
	if _, err := e.s.Backup(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckOwnerOnlyDir(fresh); err != nil {
		t.Fatalf("the created backup root is not private: %v", err)
	}
	loose := privateDir(t, "loose")
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Backup(context.Background(), loose); !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		t.Fatalf("a group-readable backup root: %v", err)
	}
	entries, _ := os.ReadDir(loose)
	if len(entries) != 0 {
		t.Fatal("a backup was written into a directory that was refused")
	}
}

func TestCheckSQLiteSidecarsRejectsUnsafeExistingFileWithoutMutation(t *testing.T) {
	root := privateDir(t, "sidecars")
	database := filepath.Join(root, DatabaseFile)
	sidecar := database + "-wal"
	contents := []byte("keep this existing sidecar unchanged")
	if err := os.WriteFile(sidecar, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sidecar, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	err = checkSQLiteSidecars(database)
	if !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		t.Fatalf("sidecar check error = %v, want ErrNotOwnerOnly", err)
	}
	after, err := os.Stat(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode().Perm() != after.Mode().Perm() || string(got) != string(contents) {
		t.Fatal("sidecar check changed the existing file")
	}
}

func TestBackupNeverWritesInsideTheDataRoot(t *testing.T) {
	e := newEnv(t)
	for name, dir := range map[string]string{
		"the data root":        e.root,
		"inside the data root": filepath.Join(e.root, "backups"),
		"above the data root":  filepath.Dir(e.root),
	} {
		t.Run(name, func(t *testing.T) {
			before, _ := os.ReadDir(dir)
			if _, err := e.s.Backup(context.Background(), dir); !errors.Is(err, ErrBackupLocation) {
				t.Fatalf("%v", err)
			}
			after, _ := os.ReadDir(dir)
			if len(before) != len(after) {
				t.Fatal("something was created while refusing")
			}
		})
	}
	for _, rel := range []string{"", "backups", "~/backups"} {
		if _, err := e.s.Backup(context.Background(), rel); err == nil {
			t.Fatalf("Backup(%q) accepted a path that is not absolute", rel)
		}
	}
}

func TestBackupDuringConcurrentWritesIsStillClosed(t *testing.T) {
	e := newEnv(t)
	writer, err := Open(context.Background(), e.root, Options{Clock: intake.FixedClock(testNow)})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	const n = 30
	threads := make([]string, n)
	for i := range threads {
		_, threads[i] = e.newThread("core:local")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i, thread := range threads {
			if _, err := writer.AdmitStart(context.Background(), e.adm, e.params(thread, fmt.Sprintf("bk.key.%018d", i), nil)); err != nil {
				t.Errorf("admission %d: %v", i, err)
				return
			}
		}
	}()
	backups := privateDir(t, "backups")
	for i := 0; i < 5; i++ {
		r, err := e.s.Backup(context.Background(), backups)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Closure.OK {
			t.Fatalf("backup %d is not a closed recovery point: %+v", i, r.Closure)
		}
	}
	wg.Wait()
}

// sabotage damages a disposable store the way a bad disk, a bad restore or a bug
// could, and the closure check must say so. Triggers that would refuse the damage
// are dropped first: the point is what the check notices, not whether SQL allows it.
func TestRestoreClosureNoticesDamage(t *testing.T) {
	type damage struct {
		name  string
		stmts func(e *env, r protocol.StartResult) []string
		want  string
	}
	cases := []damage{
		{"a flipped byte in raw input", func(e *env, r protocol.StartResult) []string {
			return []string{"DROP TRIGGER chunks_no_update", fmt.Sprintf("UPDATE evidence_chunks SET data=zeroblob(length(data)) WHERE evidence_id='%s'", r.Intake.EvidenceID)}
		}, "do not match raw_hash"},
		{"a missing chunk", func(e *env, r protocol.StartResult) []string {
			return []string{"DROP TRIGGER chunks_no_delete", fmt.Sprintf("DELETE FROM evidence_chunks WHERE evidence_id='%s'", r.Intake.EvidenceID)}
		}, "chunks hold 0 bytes"},
		{"raw hash changed", func(e *env, r protocol.StartResult) []string {
			return []string{"DROP TRIGGER evidence_sealed_no_update", fmt.Sprintf("UPDATE evidence SET raw_hash='%s' WHERE evidence_id='%s'", strings.Repeat("0", 64), r.Intake.EvidenceID)}
		}, "do not match raw_hash"},
		{"a checkpoint whose bytes do not match its hash", func(e *env, r protocol.StartResult) []string {
			return []string{fmt.Sprintf("INSERT INTO checkpoints VALUES('ckp_bad','%s','%s',NULL,'normal',1,0,0,0,x'01','%s','{}','t')", e.thread, r.RunID, strings.Repeat("e", 64))}
		}, "candidate bytes do not match"},
		{"a current checkpoint that does not exist", func(e *env, r protocol.StartResult) []string {
			return []string{fmt.Sprintf("UPDATE threads SET current_checkpoint_id='ckp_ghost' WHERE thread_id='%s'", e.thread)}
		}, "threads.current_checkpoint_id"},
		{"a current checkpoint of another thread", func(e *env, r protocol.StartResult) []string {
			sess, thr := "ses_other", "thr_other"
			return []string{
				fmt.Sprintf("INSERT INTO sessions VALUES('%s','core:local','t','/w','p','trusted_host')", sess),
				fmt.Sprintf("INSERT INTO threads(thread_id,session_id,binding_json,policy_revision,binding_revision) VALUES('%s','%s','{}','p','b')", thr, sess),
				fmt.Sprintf("INSERT INTO checkpoints VALUES('ckp_x','%s','%s',NULL,'normal',1,0,0,0,x'01','%x','{}','t')", e.thread, r.RunID, sha256.Sum256([]byte{1})),
				fmt.Sprintf("UPDATE threads SET current_checkpoint_id='ckp_x' WHERE thread_id='%s'", thr),
			}
		}, "threads.current_checkpoint_id"},
		{"an active run that does not exist", func(e *env, r protocol.StartResult) []string {
			return []string{fmt.Sprintf("UPDATE threads SET active_run_id='run_ghost' WHERE thread_id='%s'", e.thread)}
		}, "threads.active_run_id"},
		{"an active run that already ended", func(e *env, r protocol.StartResult) []string {
			return []string{fmt.Sprintf("UPDATE runs SET status='completed' WHERE run_id='%s'", r.RunID)}
		}, "threads.active_run_id"},
		{"a task whose last run is missing", func(e *env, r protocol.StartResult) []string {
			return []string{fmt.Sprintf("UPDATE tasks SET last_run_id='run_ghost' WHERE task_id='%s'", r.TaskID)}
		}, "tasks.last_run_id"},
		{"a gap in event_seq", func(e *env, r protocol.StartResult) []string {
			return []string{"DROP TRIGGER events_no_delete", fmt.Sprintf("DELETE FROM events WHERE thread_id='%s' AND event_seq=2", e.thread)}
		}, "event_seq:"},
		{"a thread counter that disagrees with its events", func(e *env, r protocol.StartResult) []string {
			return []string{fmt.Sprintf("UPDATE threads SET event_seq=9 WHERE thread_id='%s'", e.thread)}
		}, "event_seq:"},
		{"an event payload that is not canonical", func(e *env, r protocol.StartResult) []string {
			return []string{"DROP TRIGGER events_no_update", "UPDATE events SET payload_json=replace(payload_json, ',\"', ', \"') WHERE type='run.started'"}
		}, "not canonical"},
		{"an event payload with another run than its common run", func(e *env, r protocol.StartResult) []string {
			return []string{"DROP TRIGGER events_no_update", fmt.Sprintf("UPDATE events SET payload_json=replace(payload_json, '%s', 'run_00000000-0000-7000-8000-00000000dead') WHERE type='run.started'", r.RunID)}
		}, "disagree"},
		{"a receipt that names a missing run", func(e *env, r protocol.StartResult) []string {
			return []string{fmt.Sprintf("UPDATE receipts SET result_json=replace(result_json, '%s', 'run_00000000-0000-7000-8000-00000000dead')", r.RunID)}
		}, "missing or unsealed run"},
		{"a receipt that names unsealed raw evidence", func(e *env, r protocol.StartResult) []string {
			return []string{"DROP TRIGGER evidence_sealed_no_update", fmt.Sprintf("UPDATE evidence SET state='building' WHERE evidence_id='%s'", r.Intake.EvidenceID)}
		}, "missing or unsealed evidence"},
		{"a row that refers to nothing", func(e *env, r protocol.StartResult) []string {
			return []string{"PRAGMA foreign_keys=OFF", "INSERT INTO items VALUES('msg_orphan','" + e.thread + "',NULL,99,'x','y','evd_ghost','{}')", "PRAGMA foreign_keys=ON"}
		}, "foreign key"},
		{"an item whose original was never sealed", func(e *env, r protocol.StartResult) []string {
			return []string{
				"INSERT INTO evidence(evidence_id,principal,owner,state,media_type,metadata_json) VALUES('evd_open','p','o','building','text/plain','{}')",
				"INSERT INTO items VALUES('msg_open','" + e.thread + "',NULL,98,'HumanInstruction','human','evd_open','{}')",
			}
		}, "items evidence not sealed"},
		{"a pending input without its raw text", func(e *env, r protocol.StartResult) []string {
			return []string{
				"INSERT INTO evidence(evidence_id,principal,owner,state,media_type,metadata_json) VALUES('evd_open','p','o','building','text/plain','{}')",
				"INSERT INTO items VALUES('msg_open','" + e.thread + "',NULL,98,'HumanInstruction','human','evd_open','{}')",
				"INSERT INTO queue_inputs VALUES('qit_1','" + e.thread + "',NULL,'msg_open','" + r.ReceiptID + "','next_step','queued',1,NULL,'t')",
			}
		}, "queue_inputs raw"},
		{"another schema version", func(e *env, r protocol.StartResult) []string { return []string{"UPDATE schema_version SET version=2"} }, "schema_version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			out := e.mustAdmit(e.thread, "damage.key.0000000001", nil)
			if report, err := e.s.VerifyClosure(context.Background()); err != nil || !report.OK {
				t.Fatalf("the undamaged store must be closed first: %v %+v", err, report)
			}
			for _, stmt := range c.stmts(e, out.Result) {
				mustExec(t, e.s.db, stmt)
			}
			report, err := e.s.VerifyClosure(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if report.OK || !strings.Contains(strings.Join(report.Issues, "\n"), c.want) {
				t.Fatalf("damage not reported (want %q): %+v", c.want, report)
			}
			// A damaged store is never turned into a recovery point.
			backups := privateDir(t, "backups")
			if _, err := e.s.Backup(context.Background(), backups); !errors.Is(err, ErrBackupInvalid) {
				t.Fatalf("Backup of a damaged store: %v", err)
			}
			if entries, _ := os.ReadDir(backups); len(entries) != 0 {
				t.Fatalf("a failed backup left %d file(s) behind", len(entries))
			}
		})
	}
}

func TestIncompleteCaptureIsCountedNotFailed(t *testing.T) {
	e := newEnv(t)
	e.mustAdmit(e.thread, "capture.key.000000001", nil)
	mustExec(t, e.s.db, "INSERT INTO evidence(evidence_id,principal,owner,state,media_type,metadata_json) VALUES('evd_partial','p','o','building','application/octet-stream','{}')")
	mustExec(t, e.s.db, "INSERT INTO evidence_chunks VALUES('evd_partial',0,0,x'0102')")
	report, err := e.s.VerifyClosure(context.Background())
	if err != nil || !report.OK || report.Counts["building_evidence"] != 1 {
		t.Fatalf("a never-sealed capture is kept and reported, not a failure: %v %+v", err, report)
	}
}

// A backup is only a recovery point if a store can be opened from it. Placing the
// backup file as the store file of a fresh data root must give a store that Open
// accepts (WAL, schema version 1) and that holds the same data.
func TestBackupCanBePlacedAndOpenedAsAStore(t *testing.T) {
	e := newEnv(t)
	out := e.mustAdmit(e.thread, "restore.key.0000000001", nil)
	r, err := e.s.Backup(context.Background(), privateDir(t, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	restored := privateDir(t, "restored")
	if err := os.WriteFile(filepath.Join(restored, DatabaseFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), restored, Options{Clock: intake.FixedClock(testNow)})
	if err != nil {
		t.Fatalf("a verified backup that Open refuses is not a recovery point: %v", err)
	}
	defer s.Close()
	if v := scalar[string](t, s.db, "SELECT run_id FROM runs"); v != out.Result.RunID {
		t.Fatalf("restored run %s", v)
	}
	// The restored store still answers the original request from its receipt.
	adm := e.adm
	again, err := s.AdmitStart(context.Background(), adm, e.params(e.thread, "restore.key.0000000001", nil))
	if err != nil || !again.Replayed || !reflect.DeepEqual(again.Result, out.Result) {
		t.Fatalf("%v %+v", err, again)
	}
}

func TestBackupRefusesALinkIntoTheDataRootBeforeCreatingAnything(t *testing.T) {
	e := newEnv(t)
	base := t.TempDir()
	link := filepath.Join(base, "via-link")
	if err := os.Symlink(e.root, link); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(link, "backups")
	before, _ := os.ReadDir(e.root)
	if _, err := e.s.Backup(context.Background(), target); !errors.Is(err, ErrBackupLocation) {
		t.Fatalf("%v", err)
	}
	after, _ := os.ReadDir(e.root)
	if len(before) != len(after) {
		t.Fatal("a directory was created inside the data root before the refusal")
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		upper := filepath.Join(strings.ToUpper(e.root), "BACKUPS")
		if _, err := e.s.Backup(context.Background(), upper); !errors.Is(err, ErrBackupLocation) && err == nil {
			t.Fatal("a data root spelled in another case was not recognized")
		}
	}
}

// The check runs over many tables while admissions keep committing. Each query of an
// unsnapshotted check sees a different state; one snapshot never reports damage that
// is not there.
func TestLiveClosureCheckSeesOneSnapshotWhileWritersCommit(t *testing.T) {
	e := newEnv(t)
	writer, err := Open(context.Background(), e.root, Options{Clock: intake.FixedClock(testNow)})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	const n = 60
	threads := make([]string, n)
	for i := range threads {
		_, threads[i] = e.newThread("core:local")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i, thread := range threads {
			if _, err := writer.AdmitStart(context.Background(), e.adm, e.params(thread, fmt.Sprintf("snap.key.%018d", i), nil)); err != nil {
				t.Errorf("admission %d: %v", i, err)
				return
			}
		}
	}()
	for i := 0; i < 25; i++ {
		report, err := e.s.VerifyClosure(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !report.OK {
			t.Fatalf("check %d reported damage that is not there: %v", i, report.Issues)
		}
	}
	wg.Wait()
}
