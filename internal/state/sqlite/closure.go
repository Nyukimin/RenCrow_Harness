package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// ClosureReport is the result of the restore-closure check: whether everything a
// recovery point refers to is present in that same recovery point, and intact.
type ClosureReport struct {
	// OK is true when no issue was found.
	OK bool
	// Issues lists each failed check, one line per finding, never a stored value.
	Issues []string
	// Counts are row counts of the main tables, and "building_evidence" for the
	// evidence that was never sealed (an incomplete capture, reported and kept).
	Counts map[string]int64
}

const maxIssuesPerCheck = 5

type closureChecker struct {
	ctx    context.Context
	q      queryer
	report ClosureReport
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (c *closureChecker) issue(format string, args ...any) {
	c.report.Issues = append(c.report.Issues, fmt.Sprintf(format, args...))
}

// ids runs a query that selects the IDs of offending rows and reports them.
func (c *closureChecker) ids(check, query string, args ...any) error {
	rows, err := c.q.QueryContext(c.ctx, query, args...)
	if err != nil {
		return fmt.Errorf("sqlite: closure check %s: %w", check, mapSQLiteError(err))
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if n++; n <= maxIssuesPerCheck {
			c.issue("%s: %s", check, id)
		}
	}
	if n > maxIssuesPerCheck {
		c.issue("%s: %d more", check, n-maxIssuesPerCheck)
	}
	return rows.Err()
}

// verifyClosure runs every check against one database. It only reads.
func verifyClosure(ctx context.Context, q queryer) (ClosureReport, error) {
	c := &closureChecker{ctx: ctx, q: q, report: ClosureReport{Counts: map[string]int64{}}}
	steps := []func() error{
		c.checkIntegrity, c.checkForeignKeys, c.checkSchemaVersion, c.checkEvidence, c.checkCheckpoints,
		c.checkPointers, c.checkEvents, c.checkReceipts, c.checkInputs, c.checkImports, c.countRows,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return c.report, err
		}
	}
	sort.Strings(c.report.Issues)
	c.report.OK = len(c.report.Issues) == 0
	return c.report, nil
}

func (c *closureChecker) checkIntegrity() error {
	rows, err := c.q.QueryContext(c.ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("sqlite: integrity_check: %w", mapSQLiteError(err))
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		if line != "ok" {
			if n++; n <= maxIssuesPerCheck {
				c.issue("integrity_check: %s", line)
			}
		}
	}
	return rows.Err()
}

func (c *closureChecker) checkForeignKeys() error {
	rows, err := c.q.QueryContext(c.ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("sqlite: foreign_key_check: %w", mapSQLiteError(err))
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return err
		}
		if n++; n <= maxIssuesPerCheck {
			c.issue("foreign key: a row of %s refers to a missing row of %s", table, parent)
		}
	}
	if n > maxIssuesPerCheck {
		c.issue("foreign key: %d more", n-maxIssuesPerCheck)
	}
	return rows.Err()
}

func (c *closureChecker) checkSchemaVersion() error {
	rows, err := c.q.QueryContext(c.ctx, "SELECT version FROM schema_version")
	if err != nil {
		c.issue("schema_version: unreadable")
		return nil
	}
	defer rows.Close()
	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return err
		}
		versions = append(versions, v)
	}
	if len(versions) != 1 || versions[0] != SchemaVersion {
		c.issue("schema_version: not exactly version %d", SchemaVersion)
	}
	return rows.Err()
}

// checkEvidence verifies every sealed Evidence against its own digest: the chunks
// are contiguous from byte 0, add up to total_bytes and hash to raw_hash. Evidence
// still "building" is counted, not failed: it is an incomplete capture that
// recovery keeps and never presents as complete.
func (c *closureChecker) checkEvidence() error {
	rows, err := c.q.QueryContext(c.ctx, `SELECT e.evidence_id, e.total_bytes, e.raw_hash, c.ordinal, c.byte_start, c.data
		FROM evidence e LEFT JOIN evidence_chunks c ON c.evidence_id=e.evidence_id
		WHERE e.state='sealed' ORDER BY e.evidence_id, c.ordinal`)
	if err != nil {
		return fmt.Errorf("sqlite: closure check evidence: %w", mapSQLiteError(err))
	}
	defer rows.Close()

	var (
		cur     string
		total   int64
		want    string
		next    int64
		ordinal int64
		h       = sha256.New()
		bad     bool
		damaged int
	)
	// fail records the first problem of the current evidence, within the cap.
	fail := func(format string, args ...any) {
		bad = true
		if damaged++; damaged <= maxIssuesPerCheck {
			c.issue("evidence %s: "+format, append([]any{cur}, args...)...)
		} else if damaged == maxIssuesPerCheck+1 {
			c.issue("evidence: more damaged evidence follows")
		}
	}
	finishCur := func() {
		if cur == "" || bad {
			return
		}
		if next != total {
			fail("chunks hold %d bytes but total_bytes is %d", next, total)
		} else if hex.EncodeToString(h.Sum(nil)) != want {
			fail("chunk bytes do not match raw_hash")
		}
	}
	for rows.Next() {
		var id, hash string
		var tb int64
		var ord, start sql.NullInt64
		var data []byte
		if err := rows.Scan(&id, &tb, &hash, &ord, &start, &data); err != nil {
			return err
		}
		if id != cur {
			finishCur()
			cur, total, want, next, ordinal, bad = id, tb, hash, 0, 0, false
			h.Reset()
		}
		if !ord.Valid || bad {
			continue // no chunk (an empty capture) or already failed
		}
		if ord.Int64 != ordinal || start.Int64 != next {
			fail("chunks are not contiguous from byte 0")
			continue
		}
		ordinal++
		next += int64(len(data))
		h.Write(data)
	}
	finishCur()
	return rows.Err()
}

func (c *closureChecker) checkCheckpoints() error {
	rows, err := c.q.QueryContext(c.ctx, "SELECT checkpoint_id, candidate_bytes, candidate_hash FROM checkpoints")
	if err != nil {
		return fmt.Errorf("sqlite: closure check checkpoints: %w", mapSQLiteError(err))
	}
	defer rows.Close()
	for rows.Next() {
		var id, hash string
		var b []byte
		if err := rows.Scan(&id, &b, &hash); err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != hash {
			c.issue("checkpoint %s: candidate bytes do not match candidate_hash", id)
		}
	}
	return rows.Err()
}

// checkPointers validates the columns the DDL leaves as plain references (they
// would form creation-order cycles as foreign keys): an existing target of the
// right Thread, never just a non-empty string.
func (c *closureChecker) checkPointers() error {
	checks := []struct{ name, query string }{
		{"threads.current_checkpoint_id", `SELECT t.thread_id FROM threads t LEFT JOIN checkpoints k ON k.checkpoint_id=t.current_checkpoint_id
			WHERE t.current_checkpoint_id IS NOT NULL AND (k.checkpoint_id IS NULL OR k.thread_id<>t.thread_id)`},
		{"threads.active_run_id", `SELECT t.thread_id FROM threads t LEFT JOIN runs r ON r.run_id=t.active_run_id LEFT JOIN tasks k ON k.task_id=r.task_id
			WHERE t.active_run_id IS NOT NULL AND (r.run_id IS NULL OR k.thread_id<>t.thread_id OR r.status<>'running')`},
		{"tasks.last_run_id", `SELECT k.task_id FROM tasks k LEFT JOIN runs r ON r.run_id=k.last_run_id
			WHERE k.last_run_id IS NOT NULL AND (r.run_id IS NULL OR r.task_id<>k.task_id)`},
		{"turns.root_task_id", `SELECT u.turn_id FROM turns u LEFT JOIN tasks k ON k.task_id=u.root_task_id
			WHERE u.root_task_id IS NOT NULL AND (k.task_id IS NULL OR k.thread_id<>u.thread_id)`},
		{"turns.source_message_id", `SELECT u.turn_id FROM turns u LEFT JOIN items i ON i.message_id=u.source_message_id
			WHERE u.source_message_id IS NOT NULL AND (i.message_id IS NULL OR i.thread_id<>u.thread_id)`},
		{"context_entries thread", `SELECT e.message_id FROM context_entries e JOIN items i ON i.message_id=e.message_id WHERE i.thread_id<>e.thread_id`},
		{"queue_inputs thread", `SELECT q.queue_item_id FROM queue_inputs q JOIN items i ON i.message_id=q.message_id WHERE i.thread_id<>q.thread_id`},
		{"items evidence not sealed", `SELECT i.message_id FROM items i JOIN evidence e ON e.evidence_id=i.evidence_id WHERE e.state<>'sealed'`},
		{"events evidence not sealed", `SELECT v.event_id FROM events v JOIN evidence e ON e.evidence_id=v.evidence_id WHERE e.state<>'sealed'`},
	}
	for _, ck := range checks {
		if err := c.ids(ck.name, ck.query); err != nil {
			return err
		}
	}
	return nil
}

// checkEvents requires the Thread's event_seq to be exactly 1..N with the counter
// at N, and every stored event to be a well-formed typed Event: the stored payload
// is the canonical JSON of its type and agrees with the common fields.
func (c *closureChecker) checkEvents() error {
	if err := c.ids("event_seq", `SELECT t.thread_id FROM threads t
		LEFT JOIN (SELECT thread_id, COUNT(*) n, MIN(event_seq) lo, MAX(event_seq) hi FROM events GROUP BY thread_id) e ON e.thread_id=t.thread_id
		WHERE (e.n IS NULL AND t.event_seq<>0) OR (e.n IS NOT NULL AND (e.lo<>1 OR e.hi<>e.n OR e.hi<>t.event_seq))`); err != nil {
		return err
	}
	rows, err := c.q.QueryContext(c.ctx, `SELECT event_id, event_seq, thread_id, task_id, run_id, type, receipt_id, evidence_id, payload_json, recorded_at
		FROM events ORDER BY thread_id, event_seq`)
	if err != nil {
		return fmt.Errorf("sqlite: closure check events: %w", mapSQLiteError(err))
	}
	defer rows.Close()
	bad := 0
	for rows.Next() {
		var ev protocol.Event
		var task, run, receipt, evidence sql.NullString
		var payload string
		if err := rows.Scan(&ev.EventID, &ev.EventSeq, &ev.ThreadID, &task, &run, &ev.Type, &receipt, &evidence, &payload, &ev.RecordedAt); err != nil {
			return err
		}
		ev.TaskID, ev.RunID, ev.ReceiptID, ev.EvidenceID = nullStr(task), nullStr(run), nullStr(receipt), nullStr(evidence)
		ev.Payload = []byte(payload)
		// Message and code are not columns of their own: they live in the payload and
		// the typed check below re-derives what it needs from there.
		if err := checkStoredEvent(ev); err != nil {
			if bad++; bad <= maxIssuesPerCheck {
				c.issue("event %s: %v", ev.EventID, err)
			}
		}
	}
	return rows.Err()
}

func nullStr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

// checkReceipts follows each stored result to what it names: the run, task, turn
// and thread exist, and the intake's message and raw Evidence are present and sealed.
func (c *closureChecker) checkReceipts() error {
	rows, err := c.q.QueryContext(c.ctx, "SELECT receipt_id, result_json FROM receipts WHERE result_json IS NOT NULL")
	if err != nil {
		return fmt.Errorf("sqlite: closure check receipts: %w", mapSQLiteError(err))
	}
	type ref struct{ receipt, table, col, id string }
	var refs []ref
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		payload, err := protocol.Decode[protocol.ReceiptPayload]([]byte(raw))
		if err != nil {
			c.issue("receipt %s: result is not a ReceiptPayload", id)
			continue
		}
		switch payload.Type {
		case protocol.ReceiptStartResult:
			sr, err := payload.StartResult()
			if err != nil {
				c.issue("receipt %s: result is not a StartResult", id)
				continue
			}
			refs = append(refs,
				ref{id, "runs", "run_id", sr.RunID}, ref{id, "tasks", "task_id", sr.TaskID},
				ref{id, "turns", "turn_id", sr.TurnID}, ref{id, "threads", "thread_id", sr.ThreadID},
				ref{id, "items", "message_id", sr.Intake.MessageID},
				ref{id, "evidence", "evidence_id", sr.Intake.EvidenceID})
		case protocol.ReceiptResumeResult:
			rr, err := protocol.Decode[protocol.ResumeResult](payload.Value)
			if err != nil {
				c.issue("receipt %s: result is not a ResumeResult", id)
				continue
			}
			refs = append(refs,
				ref{id, "runs", "run_id", rr.RunID}, ref{id, "runs", "run_id", rr.PreviousRunID},
				ref{id, "tasks", "task_id", rr.TaskID}, ref{id, "threads", "thread_id", rr.ThreadID})
		case protocol.ReceiptInputReceipt:
			ir, err := protocol.Decode[protocol.InputReceipt](payload.Value)
			if err != nil {
				c.issue("receipt %s: result is not an InputReceipt", id)
				continue
			}
			refs = append(refs,
				ref{id, "items", "message_id", ir.MessageID}, ref{id, "evidence", "evidence_id", ir.Intake.EvidenceID},
				ref{id, "queue_inputs", "queue_item_id", ir.QueueItemID}, ref{id, "threads", "thread_id", ir.Intake.ThreadID})
		case protocol.ReceiptInterruptReceipt:
			ir, err := protocol.Decode[protocol.InterruptReceipt](payload.Value)
			if err != nil {
				c.issue("receipt %s: result is not an InterruptReceipt", id)
				continue
			}
			refs = append(refs, ref{id, "runs", "run_id", ir.RunID})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close() // one connection: finish the scan before the lookups below
	for _, r := range refs {
		var n int
		q := "SELECT COUNT(*) FROM " + r.table + " WHERE " + r.col + "=?"
		if r.table == "evidence" {
			q += " AND state='sealed'"
		}
		if err := c.q.QueryRowContext(c.ctx, q, r.id).Scan(&n); err != nil {
			return fmt.Errorf("sqlite: closure check receipts: %w", mapSQLiteError(err))
		}
		if n == 0 {
			c.issue("receipt %s: names a missing or unsealed %s", r.receipt, strings.TrimSuffix(r.table, "s"))
		}
	}
	return nil
}

// checkInputs reports queued inputs whose original is not durable: pending inputs
// must survive a restore with their raw text.
func (c *closureChecker) checkInputs() error {
	return c.ids("queue_inputs raw", `SELECT q.queue_item_id FROM queue_inputs q JOIN items i ON i.message_id=q.message_id
		LEFT JOIN evidence e ON e.evidence_id=i.evidence_id WHERE e.evidence_id IS NULL OR e.state<>'sealed'`)
}

// checkImports reports provenance a fork imported that is not in the recovery point: an
// Evidence that is missing or not sealed, a checkpoint that is missing, and an import whose fork
// checkpoint is not a fork checkpoint of the Thread that imported it.
func (c *closureChecker) checkImports() error {
	for _, chk := range []struct{ name, query string }{
		{"source_imports evidence", `SELECT i.thread_id||'/'||i.source_id FROM source_imports i WHERE i.source_kind='evidence'
			AND NOT EXISTS (SELECT 1 FROM evidence e WHERE e.evidence_id=i.source_id AND e.state='sealed')`},
		{"source_imports checkpoint", `SELECT i.thread_id||'/'||i.source_id FROM source_imports i WHERE i.source_kind='checkpoint'
			AND NOT EXISTS (SELECT 1 FROM checkpoints k WHERE k.checkpoint_id=i.source_id)`},
		{"source_imports fork checkpoint", `SELECT i.thread_id||'/'||i.source_id FROM source_imports i
			WHERE NOT EXISTS (SELECT 1 FROM checkpoints k WHERE k.checkpoint_id=i.fork_checkpoint_id AND k.thread_id=i.thread_id AND k.mode='fork')`},
	} {
		if err := c.ids(chk.name, chk.query); err != nil {
			return err
		}
	}
	return nil
}

func (c *closureChecker) countRows() error {
	for _, table := range []string{"sessions", "threads", "turns", "tasks", "runs", "evidence", "evidence_chunks", "items",
		"context_entries", "checkpoints", "receipts", "queue_inputs", "events", "relay_nonces", "source_imports"} {
		var n int64
		if err := c.q.QueryRowContext(c.ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			c.issue("table %s is unreadable", table)
			continue
		}
		c.report.Counts[table] = n
	}
	var building int64
	if err := c.q.QueryRowContext(c.ctx, "SELECT COUNT(*) FROM evidence WHERE state='building'").Scan(&building); err == nil {
		c.report.Counts["building_evidence"] = building
	}
	return nil
}

// VerifyClosure checks the live store's restore closure. The check runs on its own
// read-only connection inside one read transaction, so every one of its queries sees
// the same committed snapshot even while writers keep committing, and it does not hold
// the store's write path while it runs.
func (s *Store) VerifyClosure(ctx context.Context) (ClosureReport, error) {
	db, err := openDB(dsn(s.path, "_pragma=query_only(1)"))
	if err != nil {
		return ClosureReport{}, fmt.Errorf("sqlite: %w", err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return ClosureReport{}, fmt.Errorf("sqlite: %w", mapSQLiteError(err))
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil { // deferred: the first read pins the snapshot
		return ClosureReport{}, fmt.Errorf("sqlite: %w", mapSQLiteError(err))
	}
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK") }()
	return verifyClosure(ctx, conn)
}

// VerifyBackupFile checks a database file at rest, without touching it: it is
// opened read-only and immutable, so no journal or WAL file is created beside it.
func VerifyBackupFile(ctx context.Context, path string) (ClosureReport, error) {
	db, err := openDB(sqliteFileURI(path, "mode=ro&immutable=1&_pragma=foreign_keys(1)"))
	if err != nil {
		return ClosureReport{}, fmt.Errorf("sqlite: %w", err)
	}
	defer db.Close()
	return verifyClosure(ctx, db)
}
