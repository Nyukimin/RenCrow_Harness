package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The ways a driver's write can be refused. They are values the kernel branches on,
// not messages for a client.
var (
	// ErrWriterLost: the Thread's writer epoch is no longer the driver's. Another
	// process took the Thread over; this driver writes nothing more.
	ErrWriterLost = errors.New("sqlite: the thread's writer epoch is no longer this driver's")
	// ErrControlChanged: the Thread's control revision moved since the driver read it
	// (a cancellation was recorded), so nothing may be dispatched on the old reading.
	ErrControlChanged = errors.New("sqlite: the thread's control revision changed")
	// ErrRunNotRunning: the Run already has its result.
	ErrRunNotRunning = errors.New("sqlite: the run is not running")
	// ErrPhaseConflict: the Run is not in the phase the write expected.
	ErrPhaseConflict = errors.New("sqlite: the run is not in the expected phase")
	// ErrGenerationBudget: no generation attempt is left in the Run's budget.
	ErrGenerationBudget = errors.New("sqlite: the run has no generation attempt left")
	// ErrStepBudget: the Run has used its model steps.
	ErrStepBudget = errors.New("sqlite: the run has used its model steps")
	// ErrDeadlinePassed: the Run's deadline has passed.
	ErrDeadlinePassed = errors.New("sqlite: the run's deadline has passed")
	// ErrAttemptEnded: the Attempt already has its end recorded.
	ErrAttemptEnded = errors.New("sqlite: the attempt already ended")
)

// Fence is a driver's right to write for one Run: the Thread, the Run, the writer
// epoch the driver holds and the control revision it last read. A write checks the
// epoch inside its own transaction; dispatching checks the control revision too.
type Fence struct {
	ThreadID        string
	RunID           string
	Epoch           int64
	ControlRevision int64
}

// Now is the store's clock, the one instant source of everything it records.
func (s *Store) Now() time.Time { return s.now() }

type runRow struct {
	runID, taskID, threadID string
	turnID                  sql.NullString
	phase, status           string
	writerEpoch             int64
	limits                  protocol.Limits
	deadline                time.Time
	attemptsUsed            int64
	attemptsUnknown         int64
}

func loadRunRow(ctx context.Context, q queryer, runID string) (runRow, bool, error) {
	var r runRow
	var limitsJSON, deadline string
	err := q.QueryRowContext(ctx, `SELECT r.run_id, r.task_id, t.thread_id, t.origin_turn_id, r.phase, r.status, r.writer_epoch, r.limits_json,
		r.deadline_at, r.generation_attempts_used, r.generation_attempts_unknown
		FROM runs r JOIN tasks t ON t.task_id=r.task_id WHERE r.run_id=?`, runID).
		Scan(&r.runID, &r.taskID, &r.threadID, &r.turnID, &r.phase, &r.status, &r.writerEpoch, &limitsJSON, &deadline, &r.attemptsUsed, &r.attemptsUnknown)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("sqlite: load run: %w", mapSQLiteError(err))
	}
	if r.limits, err = protocol.Decode[protocol.Limits]([]byte(limitsJSON)); err != nil {
		return r, false, protocol.NewError(protocol.CodeIntegrityBlocked, "a stored run does not satisfy the contract").Wrap(err)
	}
	if r.deadline, err = protocol.ParseTimestamp(deadline); err != nil {
		return r, false, protocol.NewError(protocol.CodeIntegrityBlocked, "a stored run deadline is not valid").Wrap(err)
	}
	return r, true, nil
}

// startRecord is what the Run's admission left in its first item. messageID and intake
// are the Task's start (a resumed Run has the Task's, since it is the same Task);
// runReceiptID is the receipt of the operation that admitted this very Run: the
// turn/start for a Run that started a Task, the run/resume for one that resumed it, the
// context/compact or session/fork for a system Run.
//
// A system Run (a manual compaction, a fork's maintenance Run) has no input: no Turn, no
// message, no intake. Its receipt is the one its run.started event names, and the typed
// context blocks it counts with are those of the Thread's latest turn/start (blocksReceipt):
// a system Run is given no blocks of its own, and what the next Run will carry is not known
// before it is started.
type startRecord struct {
	messageID    string
	intake       intakeRecord
	runReceiptID string
	// system is set for a Run whose Task is not a work Task.
	system bool
	// blocksReceipt is the intake receipt whose context blocks the Run's prompts carry.
	blocksReceipt string
}

func loadStartRecord(ctx context.Context, q queryer, runID string) (startRecord, error) {
	var s startRecord
	var kind, threadID string
	var turn sql.NullString
	err := q.QueryRowContext(ctx, `SELECT t.kind, t.thread_id, t.origin_turn_id FROM runs r JOIN tasks t ON t.task_id=r.task_id WHERE r.run_id=?`, runID).Scan(&kind, &threadID, &turn)
	if errors.Is(err, sql.ErrNoRows) {
		return s, protocol.NewError(protocol.CodeIntegrityBlocked, "a run has no task")
	}
	if err != nil {
		return s, fmt.Errorf("sqlite: load start record: %w", mapSQLiteError(err))
	}
	if kind != "work" || !turn.Valid {
		if kind == "work" || turn.Valid {
			return s, protocol.NewError(protocol.CodeIntegrityBlocked, "a run's task is a system task with a turn, or a work task without one")
		}
		s.system = true
		if err := q.QueryRowContext(ctx, `SELECT receipt_id FROM events WHERE run_id=? AND type='run.started' AND receipt_id IS NOT NULL ORDER BY event_seq LIMIT 1`, runID).Scan(&s.runReceiptID); err != nil {
			return s, protocol.NewError(protocol.CodeIntegrityBlocked, "a system run has no operation receipt").Wrap(err)
		}
		var block sql.NullString
		if err := q.QueryRowContext(ctx, `SELECT json_extract(metadata_json,'$.intake_receipt_ref') FROM items WHERE thread_id=? AND history_kind='HostContext'
			ORDER BY sequence DESC LIMIT 1`, threadID).Scan(&block); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return s, fmt.Errorf("sqlite: load context block receipt: %w", mapSQLiteError(err))
		}
		s.blocksReceipt = block.String
		return s, nil
	}
	var meta string
	err = q.QueryRowContext(ctx, `SELECT i.message_id, i.metadata_json FROM runs r JOIN tasks t ON t.task_id=r.task_id
		JOIN turns tu ON tu.turn_id=t.origin_turn_id JOIN items i ON i.message_id=tu.source_message_id WHERE r.run_id=?`, runID).Scan(&s.messageID, &meta)
	if errors.Is(err, sql.ErrNoRows) {
		return s, protocol.NewError(protocol.CodeIntegrityBlocked, "a run has no admitted input")
	}
	if err != nil {
		return s, fmt.Errorf("sqlite: load start record: %w", mapSQLiteError(err))
	}
	if err := json.Unmarshal([]byte(meta), &s.intake); err != nil || s.intake.Kind != "intake_record" {
		return s, protocol.NewError(protocol.CodeIntegrityBlocked, "the admitted input has no intake record").Wrap(err)
	}
	// A Run that started its Task was admitted by the turn/start whose intake this is. A
	// resumed Run was admitted by a run/resume, whose receipt names it.
	s.runReceiptID = s.intake.Intake.ReceiptID
	s.blocksReceipt = s.intake.Intake.ReceiptID
	var resumedFrom sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT resume_source_run_id FROM runs WHERE run_id=?`, runID).Scan(&resumedFrom); err != nil {
		return s, fmt.Errorf("sqlite: load run source: %w", mapSQLiteError(err))
	}
	if resumedFrom.Valid {
		var receipt string
		err := q.QueryRowContext(ctx, `SELECT receipt_id FROM receipts WHERE operation='run/resume' AND json_extract(result_json,'$.value.run_id')=?`, runID).Scan(&receipt)
		if err != nil {
			return s, protocol.NewError(protocol.CodeIntegrityBlocked, "a resumed run has no resume receipt").Wrap(err)
		}
		s.runReceiptID = receipt
	}
	return s, nil
}

// RunRecord is what a driver needs to know to run a Run.
type RunRecord struct {
	RunID, TaskID, ThreadID, SessionID, TurnID, TraceID string
	Owner                                               string
	Phase, Status                                       string
	RunEpoch                                            int64 // the epoch the Run was admitted under
	ThreadEpoch                                         int64
	ControlRevision, ContextRevision                    int64
	Limits                                              protocol.Limits
	DeadlineAt                                          time.Time
	Recovery                                            protocol.RecoveryPolicy
	RecoveryRevision                                    string
	AttemptsUsed, AttemptsUnknown                       int64
	ModelSteps                                          int64
	Binding                                             protocol.Binding
	BindingRevision, PolicyRevision                     string
	// WorkspacePath, PolicyRef and ExecutionMode are the session's: what a Run's Tool
	// authority is resolved from.
	WorkspacePath, PolicyRef, ExecutionMode         string
	StartMessageID, StartReceiptID, InputEvidenceID string
	CurrentCheckpointID                             *string
	// System is set for a Run of a system Task (a manual compaction): it has no input, so
	// StartMessageID and InputEvidenceID are empty, and StartReceiptID is the receipt of the
	// operation that admitted it. BlocksReceiptID is the intake receipt whose context blocks
	// the Run's prompts carry (its own for a work Run, the Thread's latest for a system Run).
	System          bool
	BlocksReceiptID string
}

// LoadRun reads a Run for its driver.
func (s *Store) LoadRun(ctx context.Context, runID string) (RunRecord, error) {
	row, found, err := loadRunRow(ctx, s.db, runID)
	if err != nil {
		return RunRecord{}, err
	}
	if !found {
		return RunRecord{}, forbidden()
	}
	rec := RunRecord{RunID: row.runID, TaskID: row.taskID, ThreadID: row.threadID, Phase: row.phase, Status: row.status, RunEpoch: row.writerEpoch,
		Limits: row.limits, DeadlineAt: row.deadline, AttemptsUsed: row.attemptsUsed, AttemptsUnknown: row.attemptsUnknown}
	if row.turnID.Valid {
		rec.TurnID = row.turnID.String
	}
	var bindingJSON, recoveryJSON string
	var checkpoint sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT s.session_id, s.principal, r.trace_id, t.binding_json, t.binding_revision, t.policy_revision,
		t.writer_epoch, t.control_revision, t.context_revision, t.current_checkpoint_id, r.recovery_policy_json, r.recovery_policy_revision,
		s.workspace_path, s.policy_ref, s.execution_mode
		FROM runs r JOIN tasks tk ON tk.task_id=r.task_id JOIN threads t ON t.thread_id=tk.thread_id JOIN sessions s ON s.session_id=t.session_id
		WHERE r.run_id=?`, runID).Scan(&rec.SessionID, &rec.Owner, &rec.TraceID, &bindingJSON, &rec.BindingRevision, &rec.PolicyRevision,
		&rec.ThreadEpoch, &rec.ControlRevision, &rec.ContextRevision, &checkpoint, &recoveryJSON, &rec.RecoveryRevision,
		&rec.WorkspacePath, &rec.PolicyRef, &rec.ExecutionMode)
	if err != nil {
		return RunRecord{}, wrapRead("read run", err)
	}
	rec.CurrentCheckpointID = nullStr(checkpoint)
	if rec.Binding, err = protocol.Decode[protocol.Binding]([]byte(bindingJSON)); err != nil {
		return RunRecord{}, protocol.NewError(protocol.CodeIntegrityBlocked, "a stored binding does not satisfy the contract").Wrap(err)
	}
	if rec.Recovery, err = protocol.Decode[protocol.RecoveryPolicy]([]byte(recoveryJSON)); err != nil {
		return RunRecord{}, protocol.NewError(protocol.CodeIntegrityBlocked, "a stored recovery policy does not satisfy the contract").Wrap(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions WHERE run_id=? AND kind='model' AND name='act'`, runID).Scan(&rec.ModelSteps); err != nil {
		return RunRecord{}, wrapRead("count model steps", err)
	}
	start, err := loadStartRecord(ctx, s.db, runID)
	if err != nil {
		return RunRecord{}, err
	}
	rec.StartMessageID, rec.StartReceiptID, rec.InputEvidenceID = start.messageID, start.intake.Intake.ReceiptID, start.intake.RawEvidenceID
	rec.System, rec.BlocksReceiptID = start.system, start.blocksReceipt
	if start.system {
		rec.StartReceiptID = start.runReceiptID
	}
	return rec, nil
}

// fencedWrite runs fn in one transaction after checking the driver's right to
// write: the Thread's writer epoch is the fence's, the Run is still running and is
// the Thread's active Run, and (when control is set) the control revision is the
// one the driver read. A refusal is one of the Err values above and nothing is
// written.
func (s *Store) fencedWrite(ctx context.Context, f Fence, control bool, fn func(tx *sql.Tx, now time.Time, th threadRow, run runRow, start startRecord) error) error {
	return s.write(ctx, func(tx *sql.Tx, now time.Time) error {
		th, found, err := loadThread(ctx, tx, f.ThreadID)
		if err != nil {
			return err
		}
		if !found {
			return forbidden()
		}
		if th.writerEpoch != f.Epoch {
			return ErrWriterLost
		}
		run, found, err := loadRunRow(ctx, tx, f.RunID)
		if err != nil {
			return err
		}
		if !found || run.threadID != f.ThreadID {
			return forbidden()
		}
		if run.status != "running" || !th.activeRun.Valid || th.activeRun.String != f.RunID {
			return ErrRunNotRunning
		}
		if control && th.controlRev != f.ControlRevision {
			return ErrControlChanged
		}
		start, err := loadStartRecord(ctx, tx, f.RunID)
		if err != nil {
			return err
		}
		return fn(tx, now, th, run, start)
	})
}

// finishThread writes back the Thread's event counter (and its context revision)
// with a compare-and-set on everything the transaction relied on.
func finishThread(ctx context.Context, tx *sql.Tx, th threadRow, threadID string, seq *eventSeq, newContextRev int64, clearActive bool) error {
	q := `UPDATE threads SET event_seq=?, context_revision=?`
	if clearActive {
		q += `, active_run_id=NULL`
	}
	q += ` WHERE thread_id=? AND writer_epoch=? AND context_revision=? AND control_revision=? AND event_seq=?`
	res, err := tx.ExecContext(ctx, q, seq.last(), newContextRev, threadID, th.writerEpoch, th.contextRev, th.controlRev, th.eventSeq)
	if err != nil {
		return fmt.Errorf("sqlite: update thread: %w", mapSQLiteError(err))
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return protocol.NewError(protocol.CodeRevisionConflict, "the thread changed during a write")
	}
	return nil
}

// runCommon builds the common fields of an event of this Run: the Run's own IDs, the
// receipt of the operation that admitted it (turn/start, or run/resume for a resumed
// Run), and the evidence the event traces.
func runCommon(now time.Time, threadID string, run runRow, start startRecord, evidence *string) protocol.EventCommon {
	return protocol.EventCommon{
		EventID: identity.NewEventID().String(), ThreadID: threadID, TaskID: protocol.Str(run.taskID), RunID: protocol.Str(run.runID),
		ReceiptID: protocol.Str(start.runReceiptID), EvidenceID: evidence, RecordedAt: protocol.FormatTimestamp(now),
	}
}

// SetPhase moves the Run from one phase to the next. It is a compare-and-set on the
// phase and the writer epoch: a Run that is not in `from`, or whose Thread another
// driver took over, is not touched.
func (s *Store) SetPhase(ctx context.Context, f Fence, from, to string) error {
	return s.fencedWrite(ctx, f, false, func(tx *sql.Tx, now time.Time, _ threadRow, run runRow, start startRecord) error {
		if run.phase != from {
			return ErrPhaseConflict
		}
		res, err := tx.ExecContext(ctx, `UPDATE runs SET phase=? WHERE run_id=? AND phase=? AND status='running'`, to, f.RunID, from)
		if err != nil {
			return fmt.Errorf("sqlite: set phase: %w", mapSQLiteError(err))
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrPhaseConflict
		}
		if start.system && from == "Admitting" {
			// The operation of a system Run is running once its Run is driven: what a client
			// reads from the receipt between the two is "running", not "accepted".
			if _, err := tx.ExecContext(ctx, `UPDATE receipts SET stage='running', updated_at=? WHERE receipt_id=? AND operation=? AND stage='accepted'`,
				protocol.FormatTimestamp(now), start.runReceiptID, compactOperation); err != nil {
				return fmt.Errorf("sqlite: set receipt stage: %w", mapSQLiteError(err))
			}
		}
		return nil
	})
}

// RecordEvidence seals bytes as Evidence of the Run. JSON is recorded as
// application/json (raw only); text is the text projection of itself.
func (s *Store) RecordEvidence(ctx context.Context, f Fence, purpose, mediaType string, text bool, data []byte) (string, error) {
	var id string
	err := s.fencedWrite(ctx, f, false, func(tx *sql.Tx, _ time.Time, th threadRow, _ runRow, _ startRecord) error {
		var err error
		id, err = writeEvidenceSpec(ctx, tx, evidenceSpec{Principal: th.owner, Meta: evidenceMeta{Purpose: purpose}, RunID: protocol.Str(f.RunID), MediaType: mediaType, Text: text}, data)
		return err
	})
	return id, err
}

// RunEvidenceIDs lists the Evidence captured for a Run, in the order it was written.
func (s *Store) RunEvidenceIDs(ctx context.Context, runID string) ([]string, error) {
	return runEvidenceIDs(ctx, s.db, runID)
}

func runEvidenceIDs(ctx context.Context, q queryer, runID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT evidence_id FROM evidence WHERE run_id=? ORDER BY rowid`, runID)
	if err != nil {
		return nil, wrapRead("list run evidence", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ApplyInput is the first half of the Kernel's Load step (F02, with the carry-over half
// of F23): the inputs the Thread is waiting to apply are applied to its context, in the
// order they were accepted, each moving the context revision by one and each with its
// own input.applied, in one transaction. Those are the Run's own input message (when the
// Run is the first of its Task; a resumed Run's was applied by the Run before it) and
// whatever an earlier Run left in the Thread's queue and never applied: a next_turn
// input, a deferred interrupt_current, a next_step that came after the last step. Each
// is applied once: an applied queue row is never applied again, and applying a Run's
// input twice changes nothing and returns no event.
func (s *Store) ApplyInput(ctx context.Context, f Fence) ([]protocol.Event, error) {
	var events []protocol.Event
	err := s.fencedWrite(ctx, f, false, func(tx *sql.Tx, now time.Time, th threadRow, run runRow, start startRecord) error {
		if start.system {
			// A system Run has no input to apply, and it applies none of the Thread's queue: what
			// waits there is the next work Run's to take.
			return nil
		}
		var applied int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM context_entries WHERE thread_id=? AND message_id=?`, f.ThreadID, start.messageID).Scan(&applied); err != nil {
			return fmt.Errorf("sqlite: check applied input: %w", mapSQLiteError(err))
		}
		items, err := loadCarryOver(ctx, tx, f.ThreadID, f.RunID)
		if err != nil {
			return err
		}
		if applied == 0 {
			var sequence int64
			if err := tx.QueryRowContext(ctx, `SELECT sequence FROM items WHERE message_id=?`, start.messageID).Scan(&sequence); err != nil {
				return fmt.Errorf("sqlite: read input sequence: %w", mapSQLiteError(err))
			}
			items = append(items, pendingInput{messageID: start.messageID, receiptID: start.intake.Intake.ReceiptID, evidenceID: start.intake.RawEvidenceID,
				disposition: "initial", sequence: sequence})
		}
		if len(items) == 0 {
			return nil
		}
		sortPending(items)
		seq := &eventSeq{base: th.eventSeq}
		ctxRev := th.contextRev
		if events, err = applyPending(ctx, tx, now, f.ThreadID, run, items, seq, &ctxRev); err != nil {
			return err
		}
		return finishThread(ctx, tx, th, f.ThreadID, seq, ctxRev, false)
	})
	return events, err
}

// AppliedEntry is one applied item of a Thread's context with its text.
type AppliedEntry struct {
	ContextSeq  int64
	MessageID   string
	HistoryKind string
	Origin      string
	Text        string
	EvidenceID  string
	Sequence    int64
	// ItemKind and ToolCallID come from the item's record ("kind", "tool_call_id"): the
	// two halves of a Tool exchange are told apart by them.
	ItemKind   string
	ToolCallID string
	// RawHash, TotalBytes, CaptureComplete and TextProjection are the Evidence's own
	// record of the text: what a compaction names as the source of the entry.
	RawHash         string
	TotalBytes      int64
	CaptureComplete bool
	TextProjection  bool
	// AppliedRevision is the context revision the entry was applied at, and Presented says
	// a generation of the act stage was sent at or after that revision: the model has been
	// shown the entry.
	AppliedRevision int64
	Presented       bool
}

// Snapshot (PreparedSnapshot) is what one Load reads in one consistent view: the
// Thread's revisions, the applied context in application order with the exact text
// of each entry, the typed context blocks the Run was started with, and how many
// inputs wait unapplied (never mixed into the applied context).
type Snapshot struct {
	ThreadID                                                     string
	ContextRevision, ControlRevision, QueueRevision, WriterEpoch int64
	BindingRevision, PolicyRevision                              string
	Applied                                                      []AppliedEntry
	Blocks                                                       []protocol.ContextBlock
	PendingInputs                                                int64
	CurrentCheckpointID                                          *string
	// Checkpoint is the Thread's current checkpoint, loaded and checked against its hash,
	// when it has one. Applied is then only what was applied after the checkpoint's durable
	// boundary: the checkpoint stands for the rest.
	Checkpoint *CheckpointRow
}

// LoadSnapshot (F02) reads the Run's snapshot. Every text is read back from its
// sealed Evidence and checked against its recorded hash, and every typed block
// against the digest and revision recorded at its admission; anything that does
// not match is INTEGRITY_BLOCKED, never an approximation.
func (s *Store) LoadSnapshot(ctx context.Context, runID string) (Snapshot, error) {
	rec, err := s.LoadRun(ctx, runID)
	if err != nil {
		return Snapshot{}, err
	}
	return s.loadSnapshot(ctx, rec.ThreadID, rec.BlocksReceiptID)
}

// LoadThreadSnapshot is LoadSnapshot for a Thread that no Run is reading: what a
// compaction that is only asked what it would do (dry_run) sees. Its typed context blocks
// are those of the Thread's latest turn/start (none, when it has had none).
func (s *Store) LoadThreadSnapshot(ctx context.Context, threadID string) (Snapshot, error) {
	var block sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT json_extract(metadata_json,'$.intake_receipt_ref') FROM items WHERE thread_id=? AND history_kind='HostContext'
		ORDER BY sequence DESC LIMIT 1`, threadID).Scan(&block); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, wrapRead("read context block receipt", err)
	}
	return s.loadSnapshot(ctx, threadID, block.String)
}

func (s *Store) loadSnapshot(ctx context.Context, threadID, blocksReceipt string) (Snapshot, error) {
	rec := RunRecord{ThreadID: threadID, StartReceiptID: blocksReceipt}
	var err error
	var snap Snapshot
	var checkpoint sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT thread_id, context_revision, control_revision, queue_revision, writer_epoch, binding_revision, policy_revision, current_checkpoint_id
		FROM threads WHERE thread_id=?`, rec.ThreadID).Scan(&snap.ThreadID, &snap.ContextRevision, &snap.ControlRevision, &snap.QueueRevision, &snap.WriterEpoch,
		&snap.BindingRevision, &snap.PolicyRevision, &checkpoint)
	if err != nil {
		return Snapshot{}, wrapRead("read snapshot", err)
	}
	snap.CurrentCheckpointID = nullStr(checkpoint)
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM queue_inputs WHERE thread_id=? AND delivery_state IN ('queued','deferred')`, rec.ThreadID).Scan(&snap.PendingInputs); err != nil {
		return Snapshot{}, wrapRead("count pending inputs", err)
	}

	after := int64(0)
	if snap.CurrentCheckpointID != nil {
		cp, err := s.LoadCheckpoint(ctx, *snap.CurrentCheckpointID)
		if err != nil {
			return Snapshot{}, err
		}
		if cp.ThreadID != rec.ThreadID {
			return Snapshot{}, protocol.NewError(protocol.CodeIntegrityBlocked, "the current checkpoint is not this Thread's")
		}
		snap.Checkpoint, after = &cp, cp.DurableBoundary
	}
	// What the model has been shown: the entries applied at or before the context revision of
	// the last act request that was actually generated on (one that ended with a backend
	// generation, terminal). A request that was refused before generating, or whose outcome
	// is not known, is not claimed as having shown anything.
	var presented int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(json_extract(e.metadata_json,'$.context_revision')),0) FROM model_calls m
		JOIN evidence e ON e.evidence_id=m.request_evidence_id JOIN runs r ON r.run_id=m.run_id JOIN tasks t ON t.task_id=r.task_id
		WHERE t.thread_id=? AND m.stage='act' AND m.generation_state='terminal' AND m.backend_attempts=1`, rec.ThreadID).Scan(&presented); err != nil {
		return Snapshot{}, wrapRead("read presented revision", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ce.context_seq, i.message_id, i.history_kind, i.origin, i.evidence_id, i.sequence,
		COALESCE(json_extract(i.metadata_json,'$.kind'),''), COALESCE(json_extract(i.metadata_json,'$.tool_call_id'),''),
		COALESCE(ev.raw_hash,''), COALESCE(ev.total_bytes,0), COALESCE(ev.capture_complete,0), COALESCE(ev.projection_version,'')='text/v1', ce.applied_context_revision
		FROM context_entries ce JOIN items i ON i.message_id=ce.message_id JOIN evidence ev ON ev.evidence_id=i.evidence_id
		WHERE ce.thread_id=? AND ce.context_seq>? ORDER BY ce.context_seq`, rec.ThreadID, after)
	if err != nil {
		return Snapshot{}, wrapRead("read applied context", err)
	}
	for rows.Next() {
		var e AppliedEntry
		var complete int64
		if err := rows.Scan(&e.ContextSeq, &e.MessageID, &e.HistoryKind, &e.Origin, &e.EvidenceID, &e.Sequence, &e.ItemKind, &e.ToolCallID,
			&e.RawHash, &e.TotalBytes, &complete, &e.TextProjection, &e.AppliedRevision); err != nil {
			_ = rows.Close()
			return Snapshot{}, wrapRead("read applied context", err)
		}
		e.CaptureComplete, e.Presented = complete == 1, e.AppliedRevision <= presented
		snap.Applied = append(snap.Applied, e)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return Snapshot{}, wrapRead("read applied context", err)
	}
	for i := range snap.Applied {
		e := &snap.Applied[i]
		if e.Text, err = s.readEvidenceText(ctx, e.EvidenceID); err != nil {
			return Snapshot{}, err
		}
	}

	brows, err := s.db.QueryContext(ctx, `SELECT evidence_id, metadata_json FROM items
		WHERE thread_id=? AND history_kind='HostContext' AND json_extract(metadata_json,'$.intake_receipt_ref')=? ORDER BY sequence`, threadID, blocksReceipt)
	if err != nil {
		return Snapshot{}, wrapRead("read context blocks", err)
	}
	type blockRef struct {
		evidenceID string
		meta       contextBlockRecord
	}
	var refs []blockRef
	for brows.Next() {
		var ref blockRef
		var meta string
		if err := brows.Scan(&ref.evidenceID, &meta); err != nil {
			_ = brows.Close()
			return Snapshot{}, wrapRead("read context blocks", err)
		}
		if err := json.Unmarshal([]byte(meta), &ref.meta); err != nil {
			_ = brows.Close()
			return Snapshot{}, protocol.NewError(protocol.CodeIntegrityBlocked, "a stored context block record is not valid").Wrap(err)
		}
		refs = append(refs, ref)
	}
	err = brows.Err()
	_ = brows.Close()
	if err != nil {
		return Snapshot{}, wrapRead("read context blocks", err)
	}
	for _, ref := range refs {
		text, err := s.readEvidenceText(ctx, ref.evidenceID)
		if err != nil {
			return Snapshot{}, err
		}
		digest, err := protocol.TextDigest(text)
		revision, rerr := protocol.ContextRevision(ref.meta.BlockKind, text, ref.meta.ClaimedSource)
		if err != nil || rerr != nil || digest != ref.meta.ComputedTextDigest || revision != ref.meta.Revision {
			return Snapshot{}, protocol.NewError(protocol.CodeIntegrityBlocked, "a stored context block does not match its recorded digest and revision")
		}
		snap.Blocks = append(snap.Blocks, protocol.ContextBlock{Kind: ref.meta.BlockKind, Text: text, Revision: ref.meta.Revision, Source: ref.meta.ClaimedSource})
	}
	return snap, nil
}

// readEvidenceText returns a sealed text Evidence in full. The stored bytes must
// hash to the recorded raw hash and be valid UTF-8.
func (s *Store) readEvidenceText(ctx context.Context, evidenceID string) (string, error) {
	var state string
	var total sql.NullInt64
	var rawHash sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT state, total_bytes, raw_hash FROM evidence WHERE evidence_id=?`, evidenceID).Scan(&state, &total, &rawHash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", protocol.NewError(protocol.CodeIntegrityBlocked, "an item names an evidence that does not exist")
	}
	if err != nil {
		return "", wrapRead("read evidence", err)
	}
	if state != "sealed" || !total.Valid || !rawHash.Valid {
		return "", protocol.NewError(protocol.CodeIntegrityBlocked, "an item names an evidence that is not sealed")
	}
	data, err := s.readBytes(ctx, evidenceID, 0, uint64(total.Int64))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != rawHash.String || !utf8.Valid(data) {
		return "", protocol.NewError(protocol.CodeIntegrityBlocked, "an evidence does not match its recorded hash")
	}
	return string(data), nil
}
