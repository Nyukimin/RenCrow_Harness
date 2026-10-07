package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// AppendOutcome is the result of one input/append.
type AppendOutcome struct {
	Result protocol.InputReceipt
	// Replayed is true when the key had already been accepted with the same payload and
	// the original result is returned unchanged.
	Replayed bool
	// Events are the events this call committed: input.accepted, and for
	// interrupt_current also control.cancel_requested. A replay commits none.
	Events []protocol.Event
	// StopRequested: the input is an interrupt_current, so a stop is recorded for the Run
	// (by this call, by an earlier one, or by this call's replay). Whoever acts on a stop
	// (see Service.afterStop) acts on it whenever this is set.
	StopRequested bool
}

// AppendInput (input/append, F23 ApplyQueuedInput's acceptance half) stores one more
// input of a Run that is still going, durably and with its classification, in one
// BEGIN IMMEDIATE transaction. Accepting an input is not applying it: the raw text is
// sealed as Evidence and an item, the queue gets a row and its revision moves, and the
// context (and its revision) is untouched. The three dispositions differ only in what
// happens after:
//
//   - next_step: queued; the driver applies it to the context before the Run's next
//     prompt (ApplyNextSteps), or, if the Run ends first, the next Run's Load does;
//   - next_turn: queued; applied by the Load of the next Run of the Thread;
//   - interrupt_current: stored deferred, and in the same transaction the Thread's
//     control revision moves and control.cancel_requested is recorded (the stop signal
//     is a control change, never an entry of the ordinary queue). The input is not
//     acted on by the Run it interrupted: it waits, deferred, for the next Run of the
//     Thread (a turn/start or a run/resume), which applies it once. Nothing starts
//     that Run on its own.
//
// Like turn/interrupt it needs no writer role: it only adds to the queue (and, for
// interrupt_current, signals), and the driver is the one that applies. The order is
// that of AdmitStart: the Thread must be readable; a key already used answers from its
// receipt whatever has happened since; only a new request needs control of the Thread,
// a Run of this Thread that is still the active one, the expected control revision,
// and a decided origin (a relay proof is verified in full, its nonce never used twice).
func (s *Store) AppendInput(ctx context.Context, adm Admission, paramsJSON []byte) (AppendOutcome, error) {
	in, err := protocol.Decode[protocol.InputAppendInput](paramsJSON)
	if err != nil {
		return AppendOutcome{}, err
	}
	hash, err := protocol.MutationPayloadHash(adm.Caller.Principal, "input/append", paramsJSON)
	if err != nil {
		return AppendOutcome{}, protocol.NewError(protocol.CodeInvalidParams, "params cannot be identified").Wrap(err)
	}
	var out AppendOutcome
	err = s.write(ctx, func(tx *sql.Tx, now time.Time) error {
		var err error
		out, err = appendTx(ctx, tx, now, adm, in, hash)
		return err
	})
	return out, err
}

func appendTx(ctx context.Context, tx *sql.Tx, now time.Time, adm Admission, in protocol.InputAppendInput, hash string) (AppendOutcome, error) {
	caller := adm.Caller
	th, found, err := loadThread(ctx, tx, in.ThreadID)
	if err != nil {
		return AppendOutcome{}, err
	}
	if !found || !caller.CanRead(th.owner) {
		return AppendOutcome{}, forbidden()
	}
	if payload, hit, err := lookupReceipt(ctx, tx, caller.Principal, in.IdempotencyKey, "input/append", hash); err != nil {
		return AppendOutcome{}, err
	} else if hit {
		if payload.Type != protocol.ReceiptInputReceipt {
			return AppendOutcome{}, protocol.NewError(protocol.CodeIntegrityBlocked, "the stored receipt is not an input receipt")
		}
		res, err := protocol.Decode[protocol.InputReceipt](payload.Value)
		if err != nil {
			return AppendOutcome{}, protocol.NewError(protocol.CodeIntegrityBlocked, "the stored receipt is not valid").Wrap(err)
		}
		return AppendOutcome{Result: res, Replayed: true, StopRequested: res.Disposition == protocol.DispositionInterruptCurrent}, nil
	}
	if !caller.CanControl(th.owner) {
		return AppendOutcome{}, forbidden()
	}
	run, found, err := loadRunRow(ctx, tx, in.RunID)
	if err != nil {
		return AppendOutcome{}, err
	}
	if !found || run.threadID != in.ThreadID {
		return AppendOutcome{}, forbidden()
	}
	if run.status != "running" || !th.activeRun.Valid || th.activeRun.String != in.RunID {
		return AppendOutcome{}, protocol.NewError(protocol.CodeInvalidRequest, "the run is not the thread's active run; start a turn instead")
	}
	if th.controlRev != in.ExpectedControlRevision {
		return AppendOutcome{}, protocol.NewError(protocol.CodeRevisionConflict, "the expected control revision is not the thread's current revision")
	}
	decision, err := caller.Decide(intake.Claim{
		Entrypoint: adm.Entrypoint, DeclaredOrigin: adm.DeclaredOrigin, Method: "input/append",
		IdempotencyKey: in.IdempotencyKey, DestinationThreadID: in.ThreadID, Text: in.Input.Text, Proof: in.Input.OriginProof,
	}, now)
	if err != nil {
		return AppendOutcome{}, err
	}
	if decision.Proof != nil {
		var used int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM relay_nonces WHERE issuer=? AND key_id=? AND nonce=?`,
			decision.Proof.Issuer, decision.Proof.KeyID, decision.Proof.Nonce).Scan(&used); err != nil {
			return AppendOutcome{}, fmt.Errorf("sqlite: check nonce: %w", mapSQLiteError(err))
		}
		if used != 0 {
			return AppendOutcome{}, protocol.NewError(protocol.CodeInvalidOriginProof, "origin proof rejected: nonce was already used")
		}
	}
	interrupting := in.Disposition == protocol.DispositionInterruptCurrent
	newControl := th.controlRev
	if interrupting {
		pending, err := cancelPending(ctx, tx, in.RunID)
		if err != nil {
			return AppendOutcome{}, err
		}
		if !pending {
			newControl++
		}
	}

	// All checks passed. From here on only effects.
	at := protocol.FormatTimestamp(now)
	sequence, err := nextSequence(ctx, tx, in.ThreadID)
	if err != nil {
		return AppendOutcome{}, err
	}
	receiptID := identity.NewReceiptID().String()
	messageID := identity.NewMessageID().String()
	queueItemID := identity.NewQueueItemID().String()
	newQueue := th.queueRev + 1
	delivery := protocol.DeliveryQueued
	if interrupting {
		delivery = protocol.DeliveryDeferred
	}

	evidenceID, err := writeEvidence(ctx, tx, caller.Principal, evidenceMeta{Purpose: "intake_input"}, []byte(in.Input.Text))
	if err != nil {
		return AppendOutcome{}, err
	}
	intakeReceipt := protocol.IntakeReceipt{
		ReceiptID: receiptID, MessageID: messageID, ThreadID: in.ThreadID, EvidenceID: evidenceID,
		Principal: caller.Principal, Entrypoint: adm.Entrypoint, CallerProfileDigest: caller.ProfileDigest,
		DeclaredOrigin: decision.DeclaredOrigin, EffectiveOrigin: decision.EffectiveOrigin,
		ProofBasis: decision.ProofBasis, ProofDigest: decision.ProofDigest, AcceptedSequence: sequence, AcceptedAt: at,
	}
	result := protocol.InputReceipt{
		ReceiptID: receiptID, QueueItemID: queueItemID, MessageID: messageID, Origin: decision.EffectiveOrigin, Disposition: in.Disposition,
		DeliveryState: delivery, QueueRevision: newQueue, ControlRevision: newControl, Intake: intakeReceipt,
	}
	if err := insertItem(ctx, tx, itemRow{
		messageID: messageID, threadID: in.ThreadID, sequence: sequence, historyKind: historyKind(decision.EffectiveOrigin),
		origin: decision.EffectiveOrigin, evidenceID: evidenceID,
		metadata: intakeRecord{Kind: "intake_record", Intake: intakeReceipt, RawEvidenceID: evidenceID, OriginProof: decision.Proof, MutationPayloadHash: hash},
	}); err != nil {
		return AppendOutcome{}, err
	}
	if err := insertReceipt(ctx, tx, receiptRow{id: receiptID, principal: caller.Principal, key: in.IdempotencyKey, operation: "input/append",
		hash: hash, stage: "accepted", at: at}, result); err != nil {
		return AppendOutcome{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO queue_inputs(queue_item_id, thread_id, run_id, message_id, receipt_id, disposition, delivery_state, queue_revision,
		applied_context_revision, created_at) VALUES(?,?,?,?,?,?,?,?,NULL,?)`,
		queueItemID, in.ThreadID, in.RunID, messageID, receiptID, in.Disposition, delivery, newQueue, at); err != nil {
		return AppendOutcome{}, fmt.Errorf("sqlite: queue input: %w", mapSQLiteError(err))
	}
	if decision.Proof != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO relay_nonces(issuer, key_id, nonce, payload_hash, receipt_id, accepted_at) VALUES(?,?,?,?,?,?)`,
			decision.Proof.Issuer, decision.Proof.KeyID, decision.Proof.Nonce, hash, receiptID, at); err != nil {
			if isPrimaryKeyViolation(err) {
				return AppendOutcome{}, protocol.NewError(protocol.CodeInvalidOriginProof, "origin proof rejected: nonce was already used").Wrap(err)
			}
			return AppendOutcome{}, fmt.Errorf("sqlite: record nonce: %w", mapSQLiteError(err))
		}
	}

	seq := &eventSeq{base: th.eventSeq}
	common := protocol.EventCommon{
		EventID: identity.NewEventID().String(), ThreadID: in.ThreadID, TaskID: protocol.Str(run.taskID), RunID: protocol.Str(run.runID),
		ReceiptID: protocol.Str(receiptID), EvidenceID: protocol.Str(evidenceID), MessageID: protocol.Str(messageID), RecordedAt: at,
	}
	accepted, err := appendEvent(ctx, tx, seq, eventRecord{common: common, payload: protocol.InputAcceptedPayload{
		Intake: intakeReceipt, QueueItemID: protocol.Str(queueItemID), Disposition: in.Disposition, QueueRevision: newQueue, ControlRevision: newControl}})
	if err != nil {
		return AppendOutcome{}, err
	}
	events := []protocol.Event{accepted}
	if newControl != th.controlRev {
		cancel, err := appendEvent(ctx, tx, seq, eventRecord{
			common: protocol.EventCommon{
				EventID: identity.NewEventID().String(), ThreadID: in.ThreadID, TaskID: protocol.Str(run.taskID), RunID: protocol.Str(run.runID),
				ReceiptID: protocol.Str(receiptID), RecordedAt: at,
			},
			payload:   protocol.ControlCancelRequestedPayload{RunID: run.runID, ControlRevision: newControl, Reason: CancelReasonInputInterrupt, Principal: caller.Principal},
			causation: protocol.Str(accepted.EventID), dependencies: []string{accepted.EventID},
		})
		if err != nil {
			return AppendOutcome{}, err
		}
		events = append(events, cancel)
	}
	if err := moveControl(ctx, tx, th, in.ThreadID, seq, newControl, newQueue); err != nil {
		return AppendOutcome{}, err
	}
	return AppendOutcome{Result: result, Events: events, StopRequested: interrupting}, nil
}

// pendingInput is one input waiting to be applied to a Thread's context.
type pendingInput struct {
	queueItemID *string // nil for the input a Run was started with
	messageID   string
	receiptID   string
	evidenceID  string
	disposition string // initial, or the queue row's
	sequence    int64  // the raw acceptance order (items.sequence)
}

// loadCarryOver lists the inputs of the Thread that were accepted into its queue and
// never applied (queued or deferred) and that were not aimed at the Run that is applying
// now, in the order they were accepted. They are what an earlier Run left: a next_turn
// input, a deferred interrupt_current, a next_step that arrived after the last step.
func loadCarryOver(ctx context.Context, q queryer, threadID, runID string) ([]pendingInput, error) {
	rows, err := q.QueryContext(ctx, `SELECT q.queue_item_id, q.message_id, q.receipt_id, i.evidence_id, q.disposition, i.sequence
		FROM queue_inputs q JOIN items i ON i.message_id=q.message_id
		WHERE q.thread_id=? AND q.delivery_state IN ('queued','deferred') AND COALESCE(q.run_id,'')<>? ORDER BY i.sequence`, threadID, runID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list carried-over inputs: %w", mapSQLiteError(err))
	}
	defer rows.Close()
	return scanPending(rows)
}

// loadNextSteps lists the next_step inputs aimed at the Run that are still queued.
func loadNextSteps(ctx context.Context, q queryer, threadID, runID string) ([]pendingInput, error) {
	rows, err := q.QueryContext(ctx, `SELECT q.queue_item_id, q.message_id, q.receipt_id, i.evidence_id, q.disposition, i.sequence
		FROM queue_inputs q JOIN items i ON i.message_id=q.message_id
		WHERE q.thread_id=? AND q.run_id=? AND q.disposition='next_step' AND q.delivery_state='queued' ORDER BY i.sequence`, threadID, runID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list next-step inputs: %w", mapSQLiteError(err))
	}
	defer rows.Close()
	return scanPending(rows)
}

func scanPending(rows *sql.Rows) ([]pendingInput, error) {
	var out []pendingInput
	for rows.Next() {
		var p pendingInput
		var id string
		if err := rows.Scan(&id, &p.messageID, &p.receiptID, &p.evidenceID, &p.disposition, &p.sequence); err != nil {
			return nil, err
		}
		p.queueItemID = &id
		out = append(out, p)
	}
	return out, rows.Err()
}

// applyPending applies inputs to the Thread's context in the order given, each moving
// the context revision by one and writing input.applied for the Run that applies it. An
// input that came from the queue is marked applied with the revision that took it, so
// it is applied once and never again. The caller writes the Thread row back.
func applyPending(ctx context.Context, tx *sql.Tx, now time.Time, threadID string, run runRow, items []pendingInput, seq *eventSeq, ctxRev *int64) ([]protocol.Event, error) {
	at := protocol.FormatTimestamp(now)
	var events []protocol.Event
	for _, p := range items {
		*ctxRev++
		if err := insertContextEntry(ctx, tx, threadID, p.messageID, *ctxRev); err != nil {
			return nil, fmt.Errorf("sqlite: apply input: %w", err)
		}
		if p.queueItemID != nil {
			res, err := tx.ExecContext(ctx, `UPDATE queue_inputs SET delivery_state='applied', applied_context_revision=?
				WHERE queue_item_id=? AND delivery_state IN ('queued','deferred')`, *ctxRev, *p.queueItemID)
			if err != nil {
				return nil, fmt.Errorf("sqlite: mark input applied: %w", mapSQLiteError(err))
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return nil, protocol.NewError(protocol.CodeIntegrityBlocked, "a queued input was applied twice")
			}
		}
		ev, err := appendEvent(ctx, tx, seq, eventRecord{
			common: protocol.EventCommon{
				EventID: identity.NewEventID().String(), ThreadID: threadID, TaskID: protocol.Str(run.taskID), RunID: protocol.Str(run.runID),
				ReceiptID: protocol.Str(p.receiptID), EvidenceID: protocol.Str(p.evidenceID), MessageID: protocol.Str(p.messageID), RecordedAt: at,
			},
			payload: protocol.InputAppliedPayload{MessageID: p.messageID, QueueItemID: p.queueItemID, ContextRevision: *ctxRev, Disposition: p.disposition},
		})
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return events, nil
}

// ApplyNextSteps is the step-boundary half of F23: the next_step inputs aimed at the
// Run that arrived while it was working are applied to the Thread's context, in the
// order they were accepted, so the prompt of the Run's next step carries them. It is a
// fenced write that also checks the control revision: a Run that is being stopped is
// not fed more input (ErrControlChanged). It returns the events it committed and how
// many inputs it applied; with nothing queued it writes nothing.
func (s *Store) ApplyNextSteps(ctx context.Context, f Fence) ([]protocol.Event, int, error) {
	// Most steps have nothing waiting: look before taking the write lock. An input that
	// arrives right after the look is applied at the next boundary, as one that arrived
	// right after the transaction would be.
	var waiting int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM queue_inputs WHERE thread_id=? AND run_id=? AND disposition='next_step' AND delivery_state='queued'`,
		f.ThreadID, f.RunID).Scan(&waiting); err != nil {
		return nil, 0, wrapRead("count next-step inputs", err)
	}
	if waiting == 0 {
		return nil, 0, nil
	}
	var events []protocol.Event
	applied := 0
	err := s.fencedWrite(ctx, f, true, func(tx *sql.Tx, now time.Time, th threadRow, run runRow, start startRecord) error {
		if start.system {
			return nil // a system Run takes no input; what was aimed at it waits for the next work Run
		}
		items, err := loadNextSteps(ctx, tx, f.ThreadID, f.RunID)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}
		seq := &eventSeq{base: th.eventSeq}
		ctxRev := th.contextRev
		if events, err = applyPending(ctx, tx, now, f.ThreadID, run, items, seq, &ctxRev); err != nil {
			return err
		}
		applied = len(items)
		return finishThread(ctx, tx, th, f.ThreadID, seq, ctxRev, false)
	})
	return events, applied, err
}

func sortPending(items []pendingInput) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].sequence < items[j].sequence })
}

// insertContextEntry applies one item to the Thread's context at the next application
// coordinate (context_seq): one after the last entry the Thread applied, and, for a Thread
// that stands on a checkpoint whose durable boundary is further than that (a forked Thread
// has no entry of its own and starts from the boundary of the checkpoint it was forked
// into), one after that boundary. Nothing is ever applied at or before the boundary of the
// checkpoint a Thread stands on: the snapshot reads what came after it.
func insertContextEntry(ctx context.Context, tx *sql.Tx, threadID, messageID string, contextRev int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO context_entries(thread_id, context_seq, message_id, applied_context_revision)
		VALUES(?, (SELECT MAX(COALESCE((SELECT MAX(context_seq) FROM context_entries WHERE thread_id=?), 0),
			COALESCE((SELECT c.durable_boundary FROM threads t JOIN checkpoints c ON c.checkpoint_id=t.current_checkpoint_id WHERE t.thread_id=?), 0)) + 1), ?, ?)`,
		threadID, threadID, threadID, messageID, contextRev)
	return mapSQLiteError(err)
}
