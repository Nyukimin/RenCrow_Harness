package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The fixed codes of control.cancel_requested.reason (EVENT_CONTRACT: "reason(固定code)").
// They say which operation recorded the stop signal, never why the caller wanted it.
const (
	// CancelReasonInterrupt: a turn/interrupt.
	CancelReasonInterrupt = "USER_INTERRUPT"
	// CancelReasonInputInterrupt: an input/append with disposition interrupt_current.
	CancelReasonInputInterrupt = "INPUT_INTERRUPT_CURRENT"
)

// InterruptOutcome is the result of one turn/interrupt.
type InterruptOutcome struct {
	// ThreadID is the Thread of the Run: where the stop was recorded.
	ThreadID string
	Result   protocol.InterruptReceipt
	// Replayed is true when the key had already been accepted with the same payload and
	// the original result is returned unchanged.
	Replayed bool
	// Events are the events this call committed (control.cancel_requested); a replay, a
	// Run that had already ended and a Run whose stop was already requested commit none.
	Events []protocol.Event
}

// RecordInterrupt (turn/interrupt) records the stop signal of a Run. It is only a record:
// the signal is a control revision and one control.cancel_requested event, committed
// together with the receipt in one BEGIN IMMEDIATE transaction, and it says nothing
// about whether anything has stopped. The driver of the Run (this process or another)
// finds the new revision at its next fenced write and, where it is running a call, is
// told to stop that call; the Run ends cancelled, or blocked with an unknown effect, only
// when the driver records it.
//
// No writer role is needed: stopping is how a process that does not drive the Thread
// can still ask the one that does. The order is that of AdmitStart. The Run's Thread
// must be readable (a missing and a forbidden Run look the same); a key already used
// answers from its receipt whatever has happened since; only a new request needs
// control of the Thread. Then:
//
//   - a Run that has ended gets ALREADY_TERMINAL and nothing is recorded or changed;
//   - otherwise the expected control revision must be the Thread's (lost-update
//     protection: a caller that has not seen the Thread's latest control change is
//     refused, never silently answered);
//   - a Run whose stop was already requested then gets CANCEL_REQUESTED at the current
//     revision and nothing is recorded again (a second key does not make a second
//     signal);
//   - otherwise the revision moves by one and the signal is recorded.
func (s *Store) RecordInterrupt(ctx context.Context, caller intake.Caller, paramsJSON []byte) (InterruptOutcome, error) {
	in, err := protocol.Decode[protocol.InterruptInput](paramsJSON)
	if err != nil {
		return InterruptOutcome{}, err
	}
	hash, err := protocol.MutationPayloadHash(caller.Principal, "turn/interrupt", paramsJSON)
	if err != nil {
		return InterruptOutcome{}, protocol.NewError(protocol.CodeInvalidParams, "params cannot be identified").Wrap(err)
	}
	var out InterruptOutcome
	err = s.write(ctx, func(tx *sql.Tx, now time.Time) error {
		var err error
		out, err = interruptTx(ctx, tx, now, caller, in, hash)
		return err
	})
	return out, err
}

func interruptTx(ctx context.Context, tx *sql.Tx, now time.Time, caller intake.Caller, in protocol.InterruptInput, hash string) (InterruptOutcome, error) {
	run, found, err := loadRunRow(ctx, tx, in.RunID)
	if err != nil {
		return InterruptOutcome{}, err
	}
	if !found {
		return InterruptOutcome{}, forbidden()
	}
	th, found, err := loadThread(ctx, tx, run.threadID)
	if err != nil {
		return InterruptOutcome{}, err
	}
	if !found || !caller.CanRead(th.owner) {
		return InterruptOutcome{}, forbidden()
	}

	// An existing receipt answers before anything about the present is examined.
	if payload, hit, err := lookupReceipt(ctx, tx, caller.Principal, in.IdempotencyKey, "turn/interrupt", hash); err != nil {
		return InterruptOutcome{}, err
	} else if hit {
		if payload.Type != protocol.ReceiptInterruptReceipt {
			return InterruptOutcome{}, protocol.NewError(protocol.CodeIntegrityBlocked, "the stored receipt is not an interrupt receipt")
		}
		res, err := protocol.Decode[protocol.InterruptReceipt](payload.Value)
		if err != nil {
			return InterruptOutcome{}, protocol.NewError(protocol.CodeIntegrityBlocked, "the stored receipt is not valid").Wrap(err)
		}
		return InterruptOutcome{ThreadID: run.threadID, Result: res, Replayed: true}, nil
	}
	if !caller.CanControl(th.owner) {
		return InterruptOutcome{}, forbidden()
	}

	receiptID := identity.NewReceiptID().String()
	at := protocol.FormatTimestamp(now)
	record := func(res protocol.InterruptReceipt) error {
		return insertReceipt(ctx, tx, receiptRow{id: receiptID, principal: caller.Principal, key: in.IdempotencyKey, operation: "turn/interrupt",
			hash: hash, stage: "terminal", at: at}, res)
	}

	if run.status != "running" {
		res := protocol.InterruptReceipt{ReceiptID: receiptID, RunID: in.RunID, SignalRecorded: false, ControlRevision: th.controlRev, Code: protocol.InterruptAlreadyTerminal}
		if err := record(res); err != nil {
			return InterruptOutcome{}, err
		}
		return InterruptOutcome{ThreadID: run.threadID, Result: res}, nil
	}
	if th.controlRev != in.ExpectedControlRevision {
		return InterruptOutcome{}, protocol.NewError(protocol.CodeRevisionConflict, "the expected control revision is not the thread's current revision")
	}
	pending, err := cancelPending(ctx, tx, in.RunID)
	if err != nil {
		return InterruptOutcome{}, err
	}
	if pending {
		res := protocol.InterruptReceipt{ReceiptID: receiptID, RunID: in.RunID, SignalRecorded: true, ControlRevision: th.controlRev, Code: protocol.InterruptCancelRequested}
		if err := record(res); err != nil {
			return InterruptOutcome{}, err
		}
		return InterruptOutcome{ThreadID: run.threadID, Result: res}, nil
	}

	newRev := th.controlRev + 1
	res := protocol.InterruptReceipt{ReceiptID: receiptID, RunID: in.RunID, SignalRecorded: true, ControlRevision: newRev, Code: protocol.InterruptCancelRequested}
	if err := record(res); err != nil {
		return InterruptOutcome{}, err
	}
	seq := &eventSeq{base: th.eventSeq}
	ev, err := appendEvent(ctx, tx, seq, eventRecord{
		common: protocol.EventCommon{
			EventID: identity.NewEventID().String(), ThreadID: run.threadID, TaskID: protocol.Str(run.taskID), RunID: protocol.Str(run.runID),
			ReceiptID: protocol.Str(receiptID), RecordedAt: at,
		},
		payload: protocol.ControlCancelRequestedPayload{RunID: run.runID, ControlRevision: newRev, Reason: CancelReasonInterrupt, Principal: caller.Principal},
	})
	if err != nil {
		return InterruptOutcome{}, err
	}
	if err := moveControl(ctx, tx, th, run.threadID, seq, newRev, th.queueRev); err != nil {
		return InterruptOutcome{}, err
	}
	return InterruptOutcome{ThreadID: run.threadID, Result: res, Events: []protocol.Event{ev}}, nil
}

// cancelPending reports whether a stop was already requested for the Run: a
// control.cancel_requested event names it.
func cancelPending(ctx context.Context, q queryer, runID string) (bool, error) {
	var n int
	// Through the Thread's events (the index's leading column), not the whole table.
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE thread_id=(SELECT t.thread_id FROM runs r JOIN tasks t ON t.task_id=r.task_id WHERE r.run_id=?)
		AND run_id=? AND type='control.cancel_requested'`, runID, runID).Scan(&n); err != nil {
		return false, fmt.Errorf("sqlite: check pending cancellation: %w", mapSQLiteError(err))
	}
	return n > 0, nil
}

// moveControl writes the Thread's new control and queue revisions and event counter
// back with a compare-and-set on everything the transaction relied on. Inside an
// immediate transaction nobody else can have changed these, so a miss is an invariant
// failure, reported as a conflict and never as a success.
func moveControl(ctx context.Context, tx *sql.Tx, th threadRow, threadID string, seq *eventSeq, newControlRev, newQueueRev int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE threads SET control_revision=?, queue_revision=?, event_seq=? WHERE thread_id=? AND writer_epoch=?
		AND context_revision=? AND control_revision=? AND queue_revision=? AND event_seq=?`,
		newControlRev, newQueueRev, seq.last(), threadID, th.writerEpoch, th.contextRev, th.controlRev, th.queueRev, th.eventSeq)
	if err != nil {
		return fmt.Errorf("sqlite: update thread: %w", mapSQLiteError(err))
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return protocol.NewError(protocol.CodeRevisionConflict, "the thread changed during a control write")
	}
	return nil
}

// receiptRow is the identity of a receipt to store.
type receiptRow struct {
	id, principal, key, operation, hash, stage, at string
}

// insertReceipt stores one receipt whose result is one of the receipt result types.
// The result is validated on the way in, so a malformed receipt cannot exist.
func insertReceipt(ctx context.Context, tx *sql.Tx, r receiptRow, result any) error {
	payload, err := protocol.NewReceiptPayload(result)
	if err != nil {
		return protocol.NewError(protocol.CodeInternal, "a receipt result is not valid").Wrap(err)
	}
	raw, err := protocol.Encode(payload)
	if err != nil {
		return protocol.NewError(protocol.CodeInternal, "a receipt result is not valid").Wrap(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO receipts(receipt_id, principal, idempotency_key, operation, payload_hash, stage, result_json, error_json, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,NULL,?,?)`, r.id, r.principal, r.key, r.operation, r.hash, r.stage, string(raw), r.at, r.at); err != nil {
		return fmt.Errorf("sqlite: record receipt: %w", mapSQLiteError(err))
	}
	return nil
}

// ControlRevision reads the Thread's current control revision: what a driver compares
// with the one it holds to learn that a stop was recorded by someone else.
func (s *Store) ControlRevision(ctx context.Context, threadID string) (int64, error) {
	var rev int64
	err := s.db.QueryRowContext(ctx, `SELECT control_revision FROM threads WHERE thread_id=?`, threadID).Scan(&rev)
	if err != nil {
		return 0, wrapRead("read control revision", err)
	}
	return rev, nil
}

// CancelRequested reports whether a stop was requested for the Run (a
// control.cancel_requested event names it), whether or not anything has acted on it.
func (s *Store) CancelRequested(ctx context.Context, runID string) (bool, error) {
	return cancelPending(ctx, s.db, runID)
}
