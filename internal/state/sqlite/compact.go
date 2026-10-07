package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The operation name of a manual compaction's receipt.
const compactOperation = "context/compact"

// CompactOutcome is the result of one context/compact admission.
type CompactOutcome struct {
	Result protocol.OperationAccepted
	// Replayed is true when the key had already been accepted with the same payload: the
	// original receipt is returned, whatever has happened to the Thread since.
	Replayed bool
	// DryRun is true for an accepted dry run: nothing was admitted to run.
	DryRun bool
	// ThreadID, TaskID and RunID name the system Task and Run a new (not dry, not replayed)
	// admission created: the Run the caller is to drive.
	ThreadID, TaskID, RunID string
	// Events are the events this call committed; a replay and a dry run commit none.
	Events []protocol.Event
}

func decodeCompact(adm Admission, paramsJSON []byte) (protocol.CompactInput, string, error) {
	in, err := protocol.Decode[protocol.CompactInput](paramsJSON)
	if err != nil {
		return protocol.CompactInput{}, "", err
	}
	hash, err := protocol.MutationPayloadHash(adm.Caller.Principal, compactOperation, paramsJSON)
	if err != nil {
		return protocol.CompactInput{}, "", protocol.NewError(protocol.CodeInvalidParams, "params cannot be identified").Wrap(err)
	}
	return in, hash, nil
}

// lookupReceiptRef finds the receipt of an earlier operation with this key, whether or not
// it holds its result yet (a manual compaction's receipt is accepted long before it has
// one). The same payload returns it; another payload or operation is IDEMPOTENCY_CONFLICT.
func lookupReceiptRef(ctx context.Context, q queryer, principal, key, operation, payloadHash string) (string, bool, error) {
	var id, op, hash string
	err := q.QueryRowContext(ctx, `SELECT receipt_id, operation, payload_hash FROM receipts WHERE principal=? AND idempotency_key=?`, principal, key).Scan(&id, &op, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("sqlite: look up receipt: %w", mapSQLiteError(err))
	}
	if op != operation || hash != payloadHash {
		return "", true, protocol.NewError(protocol.CodeIdempotencyConflict, "this idempotency key was already used for a different request")
	}
	return id, true, nil
}

// LookupCompact answers only the replay half of AdmitCompact: whether this exact request was
// already accepted, and its receipt if so. It takes no writer role and writes nothing.
func (s *Store) LookupCompact(ctx context.Context, adm Admission, paramsJSON []byte) (CompactOutcome, bool, error) {
	in, hash, err := decodeCompact(adm, paramsJSON)
	if err != nil {
		return CompactOutcome{}, false, err
	}
	th, found, err := loadThread(ctx, s.db, in.ThreadID)
	if err != nil {
		return CompactOutcome{}, false, err
	}
	if !found || !adm.Caller.CanRead(th.owner) {
		return CompactOutcome{}, false, forbidden()
	}
	id, hit, err := lookupReceiptRef(ctx, s.db, adm.Caller.Principal, in.IdempotencyKey, compactOperation, hash)
	if err != nil || !hit {
		return CompactOutcome{}, false, err
	}
	return CompactOutcome{Result: protocol.OperationAccepted{ReceiptID: id, Accepted: true}, Replayed: true, DryRun: in.DryRun}, true, nil
}

// systemLimits are the limits of a system Run: its own and fixed, because nobody asked for
// them (CompactInput has no limits). The deadline and the capture bound are the host's
// own, so a manual compaction is bounded as much as any Run and no more; the generation
// attempts are the two stage requests a compaction may make (Selection and Summary), or
// fewer where the host allows fewer.
func systemLimits(caps protocol.Limits) protocol.Limits {
	return protocol.Limits{MaxModelSteps: 1, MaxToolCallsPerStep: 1, DeadlineSeconds: caps.DeadlineSeconds, MaxCaptureBytes: caps.MaxCaptureBytes,
		MaxGenerationAttempts: min(2, caps.MaxGenerationAttempts)}
}

// AdmitCompact (context/compact, dry_run=false) accepts one manual compaction in a single
// BEGIN IMMEDIATE transaction, or accepts nothing: a system Task (kind compaction, no Turn,
// no input) and its Run, the receipt that will hold the CompactResult, task.created and
// run.started. The Thread must be idle (BUSY: a manual compaction is never queued behind a
// Run), the driver's writer epoch and the expected revisions must be the Thread's, and the
// Run's limits are the system's own (systemLimits). The order is that of AdmitStart: a key
// already accepted answers from its receipt first, whatever has moved since (a retried
// request is not refused for the old revisions the commit itself moved).
func (s *Store) AdmitCompact(ctx context.Context, adm Admission, paramsJSON []byte) (CompactOutcome, error) {
	if adm.WriterEpoch < 1 {
		return CompactOutcome{}, protocol.NewError(protocol.CodeInvalidRequest, "a driver must hold a writer epoch of at least 1")
	}
	if adm.Caps == nil {
		return CompactOutcome{}, protocol.NewError(protocol.CodeInternal, "admission has no limit resolver")
	}
	in, hash, err := decodeCompact(adm, paramsJSON)
	if err != nil {
		return CompactOutcome{}, err
	}
	if in.DryRun {
		return CompactOutcome{}, protocol.NewError(protocol.CodeInternal, "a dry run is recorded by AdmitCompactDryRun")
	}
	recoveryRev, err := protocol.RecoveryPolicyRevision(adm.Recovery.ContractVersion, int(adm.Recovery.MaxAttemptsPerAct), adm.Recovery.AllowedProfiles)
	if err != nil {
		return CompactOutcome{}, protocol.NewError(protocol.CodeInternal, "recovery policy is invalid").Wrap(err)
	}
	recoveryJSON, err := protocol.Encode(adm.Recovery)
	if err != nil {
		return CompactOutcome{}, protocol.NewError(protocol.CodeInternal, "recovery policy is invalid").Wrap(err)
	}
	var out CompactOutcome
	err = s.write(ctx, func(tx *sql.Tx, now time.Time) error {
		var err error
		out, err = admitCompactTx(ctx, tx, now, adm, in, hash, recoveryJSON, recoveryRev)
		return err
	})
	return out, err
}

func admitCompactTx(ctx context.Context, tx *sql.Tx, now time.Time, adm Admission, in protocol.CompactInput, hash string, recoveryJSON []byte, recoveryRev string) (CompactOutcome, error) {
	caller := adm.Caller
	th, found, err := loadThread(ctx, tx, in.ThreadID)
	if err != nil {
		return CompactOutcome{}, err
	}
	if !found || !caller.CanRead(th.owner) {
		return CompactOutcome{}, forbidden()
	}
	if id, hit, err := lookupReceiptRef(ctx, tx, caller.Principal, in.IdempotencyKey, compactOperation, hash); err != nil {
		return CompactOutcome{}, err
	} else if hit {
		return CompactOutcome{Result: protocol.OperationAccepted{ReceiptID: id, Accepted: true}, Replayed: true}, nil
	}
	if !caller.CanControl(th.owner) {
		return CompactOutcome{}, forbidden()
	}
	if th.activeRun.Valid {
		return CompactOutcome{}, protocol.NewError(protocol.CodeBusy, "the thread has an active run; a manual compaction needs an idle thread").AsRetryable()
	}
	if th.writerEpoch != adm.WriterEpoch {
		return CompactOutcome{}, protocol.NewError(protocol.CodeRevisionConflict, "the thread's writer epoch is not the driver's")
	}
	if th.contextRev != in.ExpectedContextRevision || th.controlRev != in.ExpectedControlRevision {
		return CompactOutcome{}, protocol.NewError(protocol.CodeRevisionConflict, "the expected revisions are not the thread's current revisions")
	}
	caps, err := adm.Caps(th.policyRef)
	if err != nil {
		if errors.Is(err, intake.ErrPolicyUnavailable) {
			return CompactOutcome{}, protocol.NewError(protocol.CodeForbidden, "the session's policy is not available").Wrap(err)
		}
		return CompactOutcome{}, protocol.NewError(protocol.CodeInternal, "the session's limits could not be resolved").Wrap(err)
	}
	budget, err := intake.ResolveLimits(systemLimits(caps), caps, now)
	if err != nil {
		return CompactOutcome{}, err
	}

	at := protocol.FormatTimestamp(now)
	receiptID := identity.NewReceiptID().String()
	taskID := identity.NewTaskID().String()
	runID := identity.NewRunID().String()
	traceID := identity.NewTraceID().String()
	limitsJSON, err := protocol.Encode(budget.Limits)
	if err != nil {
		return CompactOutcome{}, err
	}
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO tasks(task_id, thread_id, origin_turn_id, parent_task_id, parent_owner, kind, upstream_json, status, last_run_id, created_at)
			VALUES(?,?,NULL,NULL,NULL,'compaction',NULL,'running',?,?)`, []any{taskID, in.ThreadID, runID, at}},
		{`INSERT INTO runs(run_id, task_id, trace_id, resume_source_run_id, phase, status, writer_epoch, started_at, ended_at, result_json,
			limits_json, deadline_at, recovery_policy_json, recovery_policy_revision, generation_attempts_used, generation_attempts_unknown)
			VALUES(?,?,?,NULL,'Admitting','running',?,?,NULL,NULL,?,?,?,?,0,0)`,
			[]any{runID, taskID, traceID, th.writerEpoch, at, string(limitsJSON), budget.DeadlineAt, string(recoveryJSON), recoveryRev}},
		// The receipt is accepted and holds no result until the Run ends: the CompactResult
		// is written with run.terminal, in the same transaction.
		{`INSERT INTO receipts(receipt_id, principal, idempotency_key, operation, payload_hash, stage, result_json, error_json, created_at, updated_at)
			VALUES(?,?,?,?,?,'accepted',NULL,NULL,?,?)`, []any{receiptID, caller.Principal, in.IdempotencyKey, compactOperation, hash, at, at}},
	} {
		if _, err := tx.ExecContext(ctx, stmt.q, stmt.args...); err != nil {
			return CompactOutcome{}, fmt.Errorf("sqlite: admit compaction: %w", mapSQLiteError(err))
		}
	}
	seq := &eventSeq{base: th.eventSeq}
	common := func(run *string) protocol.EventCommon {
		return protocol.EventCommon{EventID: identity.NewEventID().String(), ThreadID: in.ThreadID, TaskID: protocol.Str(taskID), RunID: run,
			ReceiptID: protocol.Str(receiptID), RecordedAt: at}
	}
	created, err := appendEvent(ctx, tx, seq, eventRecord{common: common(nil), payload: protocol.TaskCreatedPayload{TaskID: taskID, TurnID: nil, ParentTaskID: nil, Kind: "compaction"}})
	if err != nil {
		return CompactOutcome{}, err
	}
	started, err := appendEvent(ctx, tx, seq, eventRecord{
		common: common(protocol.Str(runID)),
		payload: protocol.RunStartedPayload{RunID: runID, PreviousRunID: nil, TraceID: traceID, WriterEpoch: th.writerEpoch, EffectiveLimits: budget.Limits,
			DeadlineAt: budget.DeadlineAt, RecoveryPolicyRevision: recoveryRev},
		causation: protocol.Str(created.EventID), dependencies: []string{created.EventID},
	})
	if err != nil {
		return CompactOutcome{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE threads SET active_run_id=?, event_seq=? WHERE thread_id=? AND writer_epoch=? AND context_revision=?
		AND control_revision=? AND queue_revision=? AND event_seq=? AND active_run_id IS NULL`,
		runID, seq.last(), in.ThreadID, th.writerEpoch, th.contextRev, th.controlRev, th.queueRev, th.eventSeq)
	if err != nil {
		return CompactOutcome{}, fmt.Errorf("sqlite: update thread: %w", mapSQLiteError(err))
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return CompactOutcome{}, protocol.NewError(protocol.CodeRevisionConflict, "the thread changed during admission")
	}
	return CompactOutcome{Result: protocol.OperationAccepted{ReceiptID: receiptID, Accepted: true}, ThreadID: in.ThreadID, TaskID: taskID, RunID: runID,
		Events: []protocol.Event{created, started}}, nil
}

// AdmitCompactDryRun (context/compact, dry_run=true) records the answer of a dry run. A dry
// run looks at the Thread's context and counts it; it makes no Task, Run, Evidence, event,
// checkpoint or revision change, and the Thread may be busy: the one thing it stores is the
// receipt that holds its CompactResult, so that the accepted operation can be read back by
// its ID (OperationAccepted names nothing else) and a retried request is answered by the same
// receipt. The expected revisions are checked (the answer is about that context, not another),
// and a key already accepted answers from its receipt first.
func (s *Store) AdmitCompactDryRun(ctx context.Context, adm Admission, paramsJSON []byte, result protocol.CompactResult) (CompactOutcome, error) {
	in, hash, err := decodeCompact(adm, paramsJSON)
	if err != nil {
		return CompactOutcome{}, err
	}
	if !in.DryRun || result.Status != "dry_run" && result.Status != "unavailable" {
		return CompactOutcome{}, protocol.NewError(protocol.CodeInternal, "only the answer of a dry run is recorded here")
	}
	var out CompactOutcome
	err = s.write(ctx, func(tx *sql.Tx, now time.Time) error {
		caller := adm.Caller
		th, found, err := loadThread(ctx, tx, in.ThreadID)
		if err != nil {
			return err
		}
		if !found || !caller.CanRead(th.owner) {
			return forbidden()
		}
		if id, hit, err := lookupReceiptRef(ctx, tx, caller.Principal, in.IdempotencyKey, compactOperation, hash); err != nil {
			return err
		} else if hit {
			out = CompactOutcome{Result: protocol.OperationAccepted{ReceiptID: id, Accepted: true}, Replayed: true, DryRun: true}
			return nil
		}
		if !caller.CanControl(th.owner) {
			return forbidden()
		}
		if th.contextRev != in.ExpectedContextRevision || th.controlRev != in.ExpectedControlRevision {
			return protocol.NewError(protocol.CodeRevisionConflict, "the expected revisions are not the thread's current revisions")
		}
		at := protocol.FormatTimestamp(now)
		receiptID := identity.NewReceiptID().String()
		if err := insertReceipt(ctx, tx, receiptRow{id: receiptID, principal: caller.Principal, key: in.IdempotencyKey, operation: compactOperation, hash: hash, stage: "terminal", at: at}, result); err != nil {
			return err
		}
		out = CompactOutcome{Result: protocol.OperationAccepted{ReceiptID: receiptID, Accepted: true}, DryRun: true}
		return nil
	})
	return out, err
}

// ThreadRevisions are the revisions and pointers of a Thread a caller needs to judge a
// request against, read in one look.
type ThreadRevisions struct {
	SessionID                          string
	ContextRevision, ControlRevision   int64
	PolicyRevision, BindingRevision    string
	CurrentCheckpointID, ActiveRunID   *string
	BindingJSON                        string
	PolicyRef, WorkspacePath, ExecMode string
}

// ThreadRevisions reads them. A Thread that does not exist is FORBIDDEN, as everywhere.
func (s *Store) ThreadRevisions(ctx context.Context, threadID string) (ThreadRevisions, error) {
	var r ThreadRevisions
	var current, active sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT t.session_id, t.context_revision, t.control_revision, t.policy_revision, t.binding_revision, t.current_checkpoint_id, t.active_run_id,
		t.binding_json, s.policy_ref, s.workspace_path, s.execution_mode FROM threads t JOIN sessions s ON s.session_id=t.session_id WHERE t.thread_id=?`, threadID).
		Scan(&r.SessionID, &r.ContextRevision, &r.ControlRevision, &r.PolicyRevision, &r.BindingRevision, &current, &active, &r.BindingJSON, &r.PolicyRef, &r.WorkspacePath, &r.ExecMode)
	if errors.Is(err, sql.ErrNoRows) {
		return r, forbidden()
	}
	if err != nil {
		return r, wrapRead("read thread", err)
	}
	r.CurrentCheckpointID, r.ActiveRunID = nullStr(current), nullStr(active)
	return r, nil
}
