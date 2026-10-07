package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// TerminalInput is the determined end of a Run, ready to be stored. The IDs the
// result lists for itself (the final message, the Evidence of the final text and
// the Evidence that holds the result) are chosen by the caller before the
// transaction, so the result can name them.
type TerminalInput struct {
	Result protocol.RunResult
	// FinalText is the text of the adopted final message: non-empty exactly when
	// Result.FinalMessageID is set. FinalEvidenceID is the Evidence it is sealed as
	// and must be one of Result.EvidenceIDs.
	FinalText       string
	FinalEvidenceID string
	// ResultEvidenceID is the Evidence that holds the Result itself (run.terminal's
	// result_evidence_id).
	ResultEvidenceID string
	// CompactResult is what a manual compaction's receipt holds once its Run has ended. It is
	// required for the Run of a compaction Task and nothing else's: the receipt is made
	// terminal with it in the transaction that stores the Run's end, so the operation never
	// reads as finished before its Run is, nor as running after.
	CompactResult *protocol.CompactResult
}

// PersistTerminal stores the end of a Run. In one transaction: the final text as
// sealed Evidence and as the Thread's next applied message (when there is one), the
// result as sealed Evidence and on the Run, the Run's phase Terminal and status, the
// Task and Turn marked ended, the Thread freed of its active Run, and run.terminal.
// The event is written here and nowhere else, once, in the transaction that stores
// the result.
//
// A Run that completed adopts its final text, so completing it is a state change like
// dispatching a call, and it is linearized against a stop the same way: the control
// revision is compared, and a stop recorded first (ErrControlChanged) means the final
// text is not adopted and the Run must end as the stopped Run it is. A stop recorded
// after finds the Run ended. The other ends adopt nothing, so a stop changes only their
// meaning, which the driver already decided.
func (s *Store) PersistTerminal(ctx context.Context, f Fence, in TerminalInput) ([]protocol.Event, error) {
	var events []protocol.Event
	// A final answer is adopted into the context, which a stop recorded first must be able to
	// refuse. A system Run (a manual compaction) adopts nothing: its checkpoint is committed
	// already, and a stop recorded after that changes nothing of what it did.
	err := s.fencedWrite(ctx, f, in.Result.Status == "completed" && in.Result.FinalMessageID != nil, func(tx *sql.Tx, now time.Time, th threadRow, run runRow, start startRecord) error {
		if run.phase != "PersistingResult" {
			return ErrPhaseConflict
		}
		seq := &eventSeq{base: th.eventSeq}
		ctxRev := th.contextRev
		var err error
		if events, err = terminalTx(ctx, tx, now, th, f.ThreadID, run, start, seq, &ctxRev, in); err != nil {
			return err
		}
		return finishThread(ctx, tx, th, f.ThreadID, seq, ctxRev, true)
	})
	return events, err
}

func terminalTx(ctx context.Context, tx *sql.Tx, now time.Time, th threadRow, threadID string, run runRow, start startRecord, seq *eventSeq, ctxRev *int64, in TerminalInput) ([]protocol.Event, error) {
	r := in.Result
	if r.RunID != run.runID || r.TaskID != run.taskID {
		return nil, protocol.NewError(protocol.CodeInternal, "a result is for another run")
	}
	if (r.FinalMessageID != nil) != (in.FinalText != "") || (r.FinalMessageID != nil) != (in.FinalEvidenceID != "") {
		return nil, protocol.NewError(protocol.CodeInternal, "a result names a final message exactly when it has a final text")
	}
	if in.FinalEvidenceID != "" {
		listed := false
		for _, id := range r.EvidenceIDs {
			listed = listed || id == in.FinalEvidenceID
		}
		if !listed {
			return nil, protocol.NewError(protocol.CodeInternal, "a result does not list the evidence of its final text")
		}
	}
	resultJSON, err := protocol.Encode(r)
	if err != nil {
		return nil, protocol.NewError(protocol.CodeInternal, "the run result is not valid").Wrap(err)
	}
	at := protocol.FormatTimestamp(now)
	runIDp := protocol.Str(run.runID)

	if r.FinalMessageID != nil {
		if _, err := writeEvidenceSpec(ctx, tx, evidenceSpec{ID: in.FinalEvidenceID, Principal: th.owner, Meta: evidenceMeta{Purpose: "final_text"},
			RunID: runIDp, MediaType: "text/plain; charset=utf-8", Text: true}, []byte(in.FinalText)); err != nil {
			return nil, err
		}
		var sequence int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM items WHERE thread_id=?`, threadID).Scan(&sequence); err != nil {
			return nil, fmt.Errorf("sqlite: allocate sequence: %w", mapSQLiteError(err))
		}
		meta, err := canonicalJSON(struct {
			Kind  string `json:"kind"`
			RunID string `json:"run_id"`
		}{"final_message", run.runID})
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO items(message_id, thread_id, run_id, sequence, history_kind, origin, evidence_id, metadata_json) VALUES(?,?,?,?,'Work','agent',?,?)`,
			*r.FinalMessageID, threadID, run.runID, sequence, in.FinalEvidenceID, meta); err != nil {
			return nil, fmt.Errorf("sqlite: record final message: %w", mapSQLiteError(err))
		}
		*ctxRev++
		if err := insertContextEntry(ctx, tx, threadID, *r.FinalMessageID, *ctxRev); err != nil {
			return nil, fmt.Errorf("sqlite: apply final message: %w", err)
		}
	}
	if _, err := writeEvidenceSpec(ctx, tx, evidenceSpec{ID: in.ResultEvidenceID, Principal: th.owner, Meta: evidenceMeta{Purpose: "run_result"},
		RunID: runIDp, MediaType: "application/json"}, resultJSON); err != nil {
		return nil, err
	}
	if res, err := tx.ExecContext(ctx, `UPDATE runs SET phase='Terminal', status=?, ended_at=?, result_json=? WHERE run_id=? AND status='running'`,
		r.Status, at, string(resultJSON), run.runID); err != nil {
		return nil, fmt.Errorf("sqlite: end run: %w", mapSQLiteError(err))
	} else if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrRunNotRunning
	}
	// The Run's end is not the Task's meaning: the Task is only marked as having had
	// its Run end, whatever the Run's status says.
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='run_ended' WHERE task_id=?`, run.taskID); err != nil {
		return nil, fmt.Errorf("sqlite: end task: %w", mapSQLiteError(err))
	}
	if run.turnID.Valid {
		if _, err := tx.ExecContext(ctx, `UPDATE turns SET status='ended' WHERE turn_id=? AND status='active'`, run.turnID.String); err != nil {
			return nil, fmt.Errorf("sqlite: end turn: %w", mapSQLiteError(err))
		}
	}
	if start.system {
		if err := finishSystemReceipt(ctx, tx, now, run.taskID, start.runReceiptID, in.CompactResult); err != nil {
			return nil, err
		}
	} else if in.CompactResult != nil {
		return nil, protocol.NewError(protocol.CodeInternal, "a compaction result is given for a run that is not a system run")
	}
	common := runCommon(now, threadID, run, start, protocol.Str(in.ResultEvidenceID))
	common.Code = protocol.Str(r.Code)
	ev, err := appendEvent(ctx, tx, seq, eventRecord{common: common, payload: protocol.RunTerminalPayload{
		Status: r.Status, Code: r.Code, ResultEvidenceID: in.ResultEvidenceID, LastCheckpointID: r.LastCheckpointID}})
	if err != nil {
		return nil, err
	}
	return []protocol.Event{ev}, nil
}

// finishSystemReceipt makes the receipt of a system Run's operation terminal with its
// result. Only a manual compaction's receipt has one to write.
func finishSystemReceipt(ctx context.Context, tx *sql.Tx, now time.Time, taskID, receiptID string, result *protocol.CompactResult) error {
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT kind FROM tasks WHERE task_id=?`, taskID).Scan(&kind); err != nil {
		return wrapRead("read task kind", err)
	}
	if kind != "compaction" {
		if result != nil {
			return protocol.NewError(protocol.CodeInternal, "a compaction result is given for a run that does not compact")
		}
		return nil
	}
	if result == nil {
		return protocol.NewError(protocol.CodeInternal, "a manual compaction ends with its result")
	}
	payload, err := protocol.NewReceiptPayload(*result)
	if err != nil {
		return protocol.NewError(protocol.CodeInternal, "a compaction result is not valid").Wrap(err)
	}
	raw, err := protocol.Encode(payload)
	if err != nil {
		return protocol.NewError(protocol.CodeInternal, "a compaction result is not valid").Wrap(err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE receipts SET stage='terminal', result_json=?, updated_at=? WHERE receipt_id=? AND operation=? AND stage<>'terminal'`,
		string(raw), protocol.FormatTimestamp(now), receiptID, compactOperation)
	if err != nil {
		return fmt.Errorf("sqlite: finish receipt: %w", mapSQLiteError(err))
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return protocol.NewError(protocol.CodeIntegrityBlocked, "a manual compaction's receipt is not open")
	}
	return nil
}

// UnresolvedAttempt is a generation attempt that was dispatched and has no recorded
// end.
type UnresolvedAttempt struct {
	ActionID  string
	AttemptID string
}

// StaleRun is what a Run left behind when its driver stopped, in the facts a pure
// decision needs.
type StaleRun struct {
	RunID, TaskID, Phase string
	Now                  time.Time
	DeadlineAt           time.Time
	// Unresolved are the attempts that were dispatched and never recorded an end.
	// Each has already been settled as unknown when decide is called, and its
	// receipt Evidence is among EvidenceIDs.
	Unresolved []UnresolvedAttempt
	// UnresolvedTools are the Actions of Tool calls that were dispatched and never
	// recorded an end: each is unknown now, with its answer applied to the context.
	UnresolvedTools []string
	EvidenceIDs     []string
	Limits          protocol.Limits
	// CancelRequested: a stop of the Run was requested (control.cancel_requested) and the
	// Run's driver never acted on it.
	CancelRequested bool
	// LastCheckpointID is the last checkpoint the Run committed, if it committed one: a
	// checkpoint is one transaction, so one that exists is whole whatever became of the driver.
	LastCheckpointID *string
	// System is set for the Run of a system Task, and Compaction for the one of a manual
	// compaction: its result is the CompactResult its receipt holds. LastCheckpoint is what the
	// Run's last checkpoint says of itself (a compaction's result is made of it).
	System, Compaction bool
	LastCheckpoint     *StaleCheckpoint
}

// StaleCheckpoint is the part of a committed checkpoint a CompactResult is made of: its
// mode, its boundaries and the two counts, which a settled compaction takes from the row
// because nothing else of the Run's memory survived.
type StaleCheckpoint struct {
	CheckpointID                      string
	Mode                              string
	SemanticBoundary, DurableBoundary int64
	CountJSON                         string
}

// TerminalizeStale ends, deterministically, every Run of the Thread that an earlier
// writer epoch left running. It is called by the driver that has just taken a new
// epoch, in one transaction: for each such Run, its undecided generation attempts
// become unknown (the response may have been produced, so nothing is assumed), and
// decide, a pure function of the Run's facts, gives the result that is stored with
// run.terminal. A Run of the current epoch is never touched. Nothing is written when
// there is nothing stale.
func (s *Store) TerminalizeStale(ctx context.Context, threadID string, epoch int64, reconciled map[string]string, decide func(StaleRun) (TerminalInput, error)) ([]protocol.Event, error) {
	return s.terminalizeRuns(ctx, threadID, epoch, SelectOlderEpochs, nil, reconciled, decide)
}

// TerminalizeExpired ends the Runs of the Thread, of the driver's own epoch, whose
// deadline has passed and that nobody is driving (driving says which are). A Run that
// is being driven ends itself when its deadline passes; this is for the one that is
// not (its driver stopped without ending it, or this process has no driver at all), so
// that a Run without a driver does not keep the Thread busy past its deadline.
func (s *Store) TerminalizeExpired(ctx context.Context, threadID string, epoch int64, driving func(runID string) bool, reconciled map[string]string, decide func(StaleRun) (TerminalInput, error)) ([]protocol.Event, error) {
	return s.terminalizeRuns(ctx, threadID, epoch, SelectExpired, driving, reconciled, decide)
}

// TerminalizeCancelled ends the Runs of the Thread, of the driver's own epoch, whose stop
// was requested and that nobody is driving (driving says which are). A Run that is being
// driven ends itself when its driver acts on the request; this is for the one that is
// not (this process has no driver at all, or its driver stopped without ending it), so
// that a stop recorded for a Run nobody works on ends it instead of waiting for the
// next time someone takes the Thread. The request is a request: what the Run left
// unknown still decides how it ends (see the decide of the caller).
func (s *Store) TerminalizeCancelled(ctx context.Context, threadID string, epoch int64, driving func(runID string) bool, reconciled map[string]string, decide func(StaleRun) (TerminalInput, error)) ([]protocol.Event, error) {
	return s.terminalizeRuns(ctx, threadID, epoch, SelectCancelled, driving, reconciled, decide)
}

// terminalizeRuns settles the Runs selected by selectStaleRuns. reconciled maps an
// Attempt to the verdict of the host check of its process (made before the
// transaction); it is kept in the record of an Attempt that ends unknown.
func (s *Store) terminalizeRuns(ctx context.Context, threadID string, epoch int64, sel StaleSelect, driving func(string) bool, reconciled map[string]string, decide func(StaleRun) (TerminalInput, error)) ([]protocol.Event, error) {
	var events []protocol.Event
	err := s.write(ctx, func(tx *sql.Tx, now time.Time) error {
		th, found, err := loadThread(ctx, tx, threadID)
		if err != nil {
			return err
		}
		if !found {
			return forbidden()
		}
		if th.writerEpoch != epoch {
			return ErrWriterLost
		}
		runIDs, err := selectStaleRuns(ctx, tx, now, threadID, epoch, sel, driving)
		if err != nil {
			return err
		}
		if len(runIDs) == 0 {
			return nil
		}
		seq := &eventSeq{base: th.eventSeq}
		ctxRev := th.contextRev
		freed := false
		for _, id := range runIDs {
			run, found, err := loadRunRow(ctx, tx, id)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			start, err := loadStartRecord(ctx, tx, id)
			if err != nil {
				return err
			}
			arows, err := tx.QueryContext(ctx, `SELECT a.attempt_id, a.action_id FROM attempts a JOIN actions ac ON ac.action_id=a.action_id
				WHERE ac.run_id=? AND ac.kind='model' AND a.state IN ('prepared','dispatch_started','running') ORDER BY a.started_at, a.attempt_id`, id)
			if err != nil {
				return fmt.Errorf("sqlite: list unresolved attempts: %w", mapSQLiteError(err))
			}
			var unresolved []UnresolvedAttempt
			for arows.Next() {
				var u UnresolvedAttempt
				if err := arows.Scan(&u.AttemptID, &u.ActionID); err != nil {
					_ = arows.Close()
					return err
				}
				unresolved = append(unresolved, u)
			}
			err = arows.Err()
			_ = arows.Close()
			if err != nil {
				return err
			}
			code := "MODEL_GENERATION_OUTCOME_UNKNOWN"
			for _, u := range unresolved {
				end, err := endAttemptTx(ctx, tx, now, th, threadID, run, start, seq, AttemptEnd{
					AttemptID: u.AttemptID, State: "unknown", Outcome: "error", GenerationState: "unknown", BackendAttempts: nil, FailureCode: &code,
				})
				if err != nil {
					return err
				}
				events = append(events, end.Events...)
			}
			// The Tool exchange the Run left half done: every call that is not over is
			// closed (never dispatched: not run; dispatched: unknown) and the exchange is
			// applied to the context, so what was done is in front of the next Run.
			batches, err := unappliedToolResponses(ctx, tx, id)
			if err != nil {
				return err
			}
			var unknownTools []string
			for _, responseID := range batches {
				evs, unknown, err := applyBatchTx(ctx, tx, now, th, threadID, run, start, seq, &ctxRev, responseID, "driver_stopped", reconciled)
				if err != nil {
					return err
				}
				events = append(events, evs...)
				unknownTools = append(unknownTools, unknown...)
			}
			evidence, err := runEvidenceIDs(ctx, tx, id)
			if err != nil {
				return err
			}
			cancelled, err := cancelPending(ctx, tx, id)
			if err != nil {
				return err
			}
			var lastCheckpoint sql.NullString
			var last *StaleCheckpoint
			var cp StaleCheckpoint
			switch err := tx.QueryRowContext(ctx, `SELECT checkpoint_id, mode, semantic_boundary, durable_boundary, count_json FROM checkpoints WHERE run_id=? ORDER BY rowid DESC LIMIT 1`, id).
				Scan(&cp.CheckpointID, &cp.Mode, &cp.SemanticBoundary, &cp.DurableBoundary, &cp.CountJSON); {
			case err == nil:
				lastCheckpoint, last = sql.NullString{String: cp.CheckpointID, Valid: true}, &cp
			case !errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("sqlite: read the run's last checkpoint: %w", mapSQLiteError(err))
			}
			compaction := false
			if start.system {
				var kind string
				if err := tx.QueryRowContext(ctx, `SELECT kind FROM tasks WHERE task_id=?`, run.taskID).Scan(&kind); err != nil {
					return wrapRead("read task kind", err)
				}
				compaction = kind == "compaction"
			}
			in, err := decide(StaleRun{RunID: id, TaskID: run.taskID, Phase: run.phase, Now: now, DeadlineAt: run.deadline, Unresolved: unresolved,
				UnresolvedTools: unknownTools, EvidenceIDs: evidence, Limits: run.limits, CancelRequested: cancelled, LastCheckpointID: nullStr(lastCheckpoint),
				System: start.system, Compaction: compaction, LastCheckpoint: last})
			if err != nil {
				return err
			}
			evs, err := terminalTx(ctx, tx, now, th, threadID, run, start, seq, &ctxRev, in)
			if err != nil {
				return err
			}
			events = append(events, evs...)
			freed = freed || (th.activeRun.Valid && th.activeRun.String == id)
		}
		return finishThread(ctx, tx, th, threadID, seq, ctxRev, freed)
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

// unappliedToolResponses lists the responses of a Run whose Tool exchange was bound and
// never applied to the context, in the order they were made.
func unappliedToolResponses(ctx context.Context, tx *sql.Tx, runID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT json_extract(i.metadata_json,'$.response_id') FROM items i
		WHERE i.run_id=? AND json_extract(i.metadata_json,'$.kind')='tool_calls'
		AND NOT EXISTS (SELECT 1 FROM context_entries ce WHERE ce.message_id=i.message_id) ORDER BY i.sequence`, runID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list unapplied tool exchanges: %w", mapSQLiteError(err))
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
