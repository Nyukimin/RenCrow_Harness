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

const (
	forkOperation = "session/fork"
	// ForkCode is the code of the system Run that makes a fork: its own success, which says
	// nothing of any Task's work.
	ForkCode = "SESSION_FORKED"
)

// ForkOutcome is the result of one session/fork.
type ForkOutcome struct {
	Result protocol.ForkResult
	// Replayed is true when the key had already been accepted with the same payload: the
	// original result is returned, whatever has happened to either Thread since.
	Replayed bool
	// Events are the events this call committed, in the new Thread's order; a replay has none.
	Events []protocol.Event
}

// ForkSource is what a fork reads of its source before anything is built: the Thread's
// session and the revisions the fork checkpoint will be fenced to, and the stored checkpoint
// to copy, held to its hash.
type ForkSource struct {
	SessionID                       string
	BindingJSON                     string
	PolicyRevision, BindingRevision string
	PolicyRef                       string
	Checkpoint                      CheckpointRow
}

// LoadForkSource reads the source Thread and the checkpoint a fork is asked to copy. The
// caller must be able to control the Thread (a fork writes a new Thread beside it, which
// no one who may only look at it is allowed to), and the checkpoint must be a checkpoint of
// that very Thread: anything else is refused, and a checkpoint that exists and does not read
// back is INTEGRITY_BLOCKED. Whatever the Thread is doing now (it may have an active Run) is
// not the fork's concern: a checkpoint is immutable, and nothing of a Run in flight is
// copied.
func (s *Store) LoadForkSource(ctx context.Context, caller intake.Caller, threadID, checkpointID string) (ForkSource, error) {
	acc, err := s.ThreadAccess(ctx, caller, threadID)
	if err != nil {
		return ForkSource{}, err
	}
	if !acc.CanControl {
		return ForkSource{}, forbidden()
	}
	rev, err := s.ThreadRevisions(ctx, threadID)
	if err != nil {
		return ForkSource{}, err
	}
	// Whose the checkpoint is is decided before its bytes are read, so that nothing is said of
	// the state of a checkpoint that is not this Thread's (not even that it exists).
	var owner string
	if err := s.db.QueryRowContext(ctx, `SELECT thread_id FROM checkpoints WHERE checkpoint_id=?`, checkpointID).Scan(&owner); errors.Is(err, sql.ErrNoRows) || err == nil && owner != threadID {
		return ForkSource{}, protocol.NewError(protocol.CodeInvalidRequest, "the checkpoint is not a checkpoint of this thread")
	} else if err != nil {
		return ForkSource{}, wrapRead("read source checkpoint", err)
	}
	row, err := loadCheckpoint(ctx, s.db, checkpointID)
	if err != nil {
		return ForkSource{}, err
	}
	return ForkSource{SessionID: rev.SessionID, BindingJSON: rev.BindingJSON, PolicyRevision: rev.PolicyRevision, BindingRevision: rev.BindingRevision,
		PolicyRef: rev.PolicyRef, Checkpoint: row}, nil
}

// ForkCommit is the new Thread a fork makes, ready to be stored: the IDs it was issued, the
// fork checkpoint's exact bytes, and everything the transaction holds the source to.
type ForkCommit struct {
	ThreadID, TaskID, RunID, CheckpointID string
	// SourceThreadID and SourceCheckpointID are what was forked; SourceBindingJSON,
	// SourcePolicyRevision and SourceBindingRevision are the source Thread's binding and
	// revisions the new Thread inherits and the checkpoint's expected values were made from:
	// the transaction finds them as they were.
	SourceThreadID, SourceCheckpointID          string
	SourceBindingJSON                           string
	SourcePolicyRevision, SourceBindingRevision string
	Blob                                        []byte
	Hash                                        string
	SemanticBoundary, DurableBoundary           int64
	BeforeCountJSON, AfterCountJSON, CountJSON  string
	// Sources are every source the checkpoint names, found again inside the transaction in
	// the source Thread's own provenance; Imports are the same sources, imported by the new
	// Thread.
	Sources []SourceCheck
	Imports []ImportedSource
}

func decodeFork(adm Admission, paramsJSON []byte) (protocol.SessionForkInput, string, error) {
	in, err := protocol.Decode[protocol.SessionForkInput](paramsJSON)
	if err != nil {
		return protocol.SessionForkInput{}, "", err
	}
	hash, err := protocol.MutationPayloadHash(adm.Caller.Principal, forkOperation, paramsJSON)
	if err != nil {
		return protocol.SessionForkInput{}, "", protocol.NewError(protocol.CodeInvalidParams, "params cannot be identified").Wrap(err)
	}
	return in, hash, nil
}

func forkReplay(payload protocol.ReceiptPayload) (ForkOutcome, error) {
	if payload.Type != protocol.ReceiptForkResult {
		return ForkOutcome{}, protocol.NewError(protocol.CodeIntegrityBlocked, "the stored receipt is not a fork result")
	}
	res, err := protocol.Decode[protocol.ForkResult](payload.Value)
	if err != nil {
		return ForkOutcome{}, protocol.NewError(protocol.CodeIntegrityBlocked, "the stored receipt is not valid").Wrap(err)
	}
	return ForkOutcome{Result: res, Replayed: true}, nil
}

// LookupFork answers only the replay half of AdmitFork: whether this exact request was
// already accepted, and the original result if so. It takes nothing and writes nothing.
func (s *Store) LookupFork(ctx context.Context, adm Admission, paramsJSON []byte) (ForkOutcome, bool, error) {
	in, hash, err := decodeFork(adm, paramsJSON)
	if err != nil {
		return ForkOutcome{}, false, err
	}
	th, found, err := loadThread(ctx, s.db, in.ThreadID)
	if err != nil {
		return ForkOutcome{}, false, err
	}
	if !found || !adm.Caller.CanRead(th.owner) {
		return ForkOutcome{}, false, forbidden()
	}
	payload, hit, err := lookupReceipt(ctx, s.db, adm.Caller.Principal, in.IdempotencyKey, forkOperation, hash)
	if err != nil || !hit {
		return ForkOutcome{}, false, err
	}
	out, err := forkReplay(payload)
	return out, true, err
}

// AdmitFork (session/fork, F25) makes the new Thread in one BEGIN IMMEDIATE transaction, or
// nothing: the Thread (in the source's Session, so no one who could not see the source sees
// it), the Harness's own session_maintenance Task and Run (which end at once and mean no
// work), the fork checkpoint, the provenance the Thread imports, the receipt, and the
// events. It is the explicit, separate transaction CHECKPOINT_FORMAT section 2 asks for: the
// copied checkpoint is not committed onto the source Thread, whose snapshot is not moved.
//
// The order is that of AdmitStart: a key already accepted answers from its receipt first (the
// same payload: the original result, whatever has happened since; another: IDEMPOTENCY_CONFLICT);
// only a new request needs control of the source Thread, and its binding and revisions as they
// were when the checkpoint was made. The sources the checkpoint names are found again, in the
// source Thread's own provenance, inside the transaction.
//
// Nothing a Run in flight holds is copied: the new Thread has no active Run, no Tool state,
// no queued input and no file state; the workspace is the Session's, as it is, and nothing is
// restored to what it was.
func (s *Store) AdmitFork(ctx context.Context, adm Admission, paramsJSON []byte, c ForkCommit) (ForkOutcome, error) {
	if adm.Caps == nil {
		return ForkOutcome{}, protocol.NewError(protocol.CodeInternal, "admission has no limit resolver")
	}
	in, hash, err := decodeFork(adm, paramsJSON)
	if err != nil {
		return ForkOutcome{}, err
	}
	recoveryRev, err := protocol.RecoveryPolicyRevision(adm.Recovery.ContractVersion, int(adm.Recovery.MaxAttemptsPerAct), adm.Recovery.AllowedProfiles)
	if err != nil {
		return ForkOutcome{}, protocol.NewError(protocol.CodeInternal, "recovery policy is invalid").Wrap(err)
	}
	recoveryJSON, err := protocol.Encode(adm.Recovery)
	if err != nil {
		return ForkOutcome{}, protocol.NewError(protocol.CodeInternal, "recovery policy is invalid").Wrap(err)
	}
	var out ForkOutcome
	err = s.write(ctx, func(tx *sql.Tx, now time.Time) error {
		var err error
		out, err = forkTx(ctx, tx, now, adm, in, hash, recoveryJSON, recoveryRev, c)
		return err
	})
	return out, err
}

func forkTx(ctx context.Context, tx *sql.Tx, now time.Time, adm Admission, in protocol.SessionForkInput, hash string, recoveryJSON []byte, recoveryRev string, c ForkCommit) (ForkOutcome, error) {
	caller := adm.Caller
	if in.ThreadID != c.SourceThreadID || in.CheckpointID != c.SourceCheckpointID {
		return ForkOutcome{}, protocol.NewError(protocol.CodeInternal, "a fork is made for another request than the one admitted")
	}
	src, found, err := loadThread(ctx, tx, in.ThreadID)
	if err != nil {
		return ForkOutcome{}, err
	}
	if !found || !caller.CanRead(src.owner) {
		return ForkOutcome{}, forbidden()
	}
	if payload, hit, err := lookupReceipt(ctx, tx, caller.Principal, in.IdempotencyKey, forkOperation, hash); err != nil {
		return ForkOutcome{}, err
	} else if hit {
		return forkReplay(payload)
	}
	if !caller.CanControl(src.owner) {
		return ForkOutcome{}, forbidden()
	}

	// The source as the fork checkpoint was made from it: the same Session, binding and
	// revisions, and a checkpoint that is still the one that was read.
	var bindingJSON, policyRev, bindingRev string
	if err := tx.QueryRowContext(ctx, `SELECT binding_json, policy_revision, binding_revision FROM threads WHERE thread_id=?`, in.ThreadID).Scan(&bindingJSON, &policyRev, &bindingRev); err != nil {
		return ForkOutcome{}, wrapRead("read source thread", err)
	}
	if bindingJSON != c.SourceBindingJSON || policyRev != c.SourcePolicyRevision || bindingRev != c.SourceBindingRevision {
		return ForkOutcome{}, protocol.NewError(protocol.CodeRevisionConflict, "the source thread's binding changed while the fork was made").AsRetryable()
	}
	var sourceHash, sourceThread string
	if err := tx.QueryRowContext(ctx, `SELECT candidate_hash, thread_id FROM checkpoints WHERE checkpoint_id=?`, in.CheckpointID).Scan(&sourceHash, &sourceThread); errors.Is(err, sql.ErrNoRows) || err == nil && sourceThread != in.ThreadID {
		return ForkOutcome{}, protocol.NewError(protocol.CodeInvalidRequest, "the checkpoint is not a checkpoint of this thread")
	} else if err != nil {
		return ForkOutcome{}, wrapRead("read source checkpoint", err)
	}
	if err := verifySources(ctx, tx, in.ThreadID, c.Sources); err != nil {
		return ForkOutcome{}, err
	}
	caps, err := adm.Caps(src.policyRef)
	if err != nil {
		if errors.Is(err, intake.ErrPolicyUnavailable) {
			return ForkOutcome{}, protocol.NewError(protocol.CodeForbidden, "the session's policy is not available").Wrap(err)
		}
		return ForkOutcome{}, protocol.NewError(protocol.CodeInternal, "the session's limits could not be resolved").Wrap(err)
	}
	budget, err := intake.ResolveLimits(systemLimits(caps), caps, now)
	if err != nil {
		return ForkOutcome{}, err
	}
	limitsJSON, err := protocol.Encode(budget.Limits)
	if err != nil {
		return ForkOutcome{}, err
	}

	at := protocol.FormatTimestamp(now)
	receiptID := identity.NewReceiptID().String()
	traceID := identity.NewTraceID().String()
	const epoch = 1 // the new Thread's first writer epoch: nobody drives it yet, the next driver takes epoch 2
	const newContextRev = 1

	var binding protocol.Binding
	if binding, err = protocol.Decode[protocol.Binding]([]byte(bindingJSON)); err != nil {
		return ForkOutcome{}, protocol.NewError(protocol.CodeIntegrityBlocked, "a stored binding does not satisfy the contract").Wrap(err)
	}
	var mode, sessionOwner string
	if err := tx.QueryRowContext(ctx, `SELECT s.execution_mode, s.principal FROM threads t JOIN sessions s ON s.session_id=t.session_id WHERE t.thread_id=?`, in.ThreadID).Scan(&mode, &sessionOwner); err != nil {
		return ForkOutcome{}, wrapRead("read session", err)
	}

	for _, stmt := range []struct {
		what string
		q    string
		args []any
	}{
		{"thread", `INSERT INTO threads(thread_id, session_id, source_thread_id, binding_json, policy_revision, binding_revision, writer_epoch, event_seq)
			VALUES(?,?,?,?,?,?,?,0)`, []any{c.ThreadID, src.sessionID, in.ThreadID, bindingJSON, policyRev, bindingRev, epoch}},
		{"task", `INSERT INTO tasks(task_id, thread_id, origin_turn_id, parent_task_id, parent_owner, kind, upstream_json, status, last_run_id, created_at)
			VALUES(?,?,NULL,NULL,NULL,'session_maintenance',NULL,'running',?,?)`, []any{c.TaskID, c.ThreadID, c.RunID, at}},
		{"run", `INSERT INTO runs(run_id, task_id, trace_id, resume_source_run_id, phase, status, writer_epoch, started_at, ended_at, result_json,
			limits_json, deadline_at, recovery_policy_json, recovery_policy_revision, generation_attempts_used, generation_attempts_unknown)
			VALUES(?,?,?,NULL,'PersistingResult','running',?,?,NULL,NULL,?,?,?,?,0,0)`,
			[]any{c.RunID, c.TaskID, traceID, epoch, at, string(limitsJSON), budget.DeadlineAt, string(recoveryJSON), recoveryRev}},
	} {
		if _, err := tx.ExecContext(ctx, stmt.q, stmt.args...); err != nil {
			return ForkOutcome{}, fmt.Errorf("sqlite: fork (%s): %w", stmt.what, mapSQLiteError(err))
		}
	}
	result := protocol.ForkResult{ReceiptID: receiptID, SourceThreadID: in.ThreadID, ThreadID: c.ThreadID, SessionID: src.sessionID, CheckpointID: c.CheckpointID}
	if err := insertReceipt(ctx, tx, receiptRow{id: receiptID, principal: caller.Principal, key: in.IdempotencyKey, operation: forkOperation, hash: hash, stage: "terminal", at: at}, result); err != nil {
		return ForkOutcome{}, err
	}

	// The two counts of the checkpoint are Evidence of the maintenance Run, as a compaction's are.
	runIDp := protocol.Str(c.RunID)
	beforeEv, err := writeEvidenceSpec(ctx, tx, evidenceSpec{Principal: src.owner, Meta: evidenceMeta{Purpose: "measure_result"}, RunID: runIDp, MediaType: "application/json"}, []byte(c.BeforeCountJSON))
	if err != nil {
		return ForkOutcome{}, err
	}
	afterEv, err := writeEvidenceSpec(ctx, tx, evidenceSpec{Principal: src.owner, Meta: evidenceMeta{Purpose: "measure_result"}, RunID: runIDp, MediaType: "application/json"}, []byte(c.AfterCountJSON))
	if err != nil {
		return ForkOutcome{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoints(checkpoint_id, thread_id, run_id, parent_checkpoint_id, mode, context_revision, control_revision,
		semantic_boundary, durable_boundary, candidate_bytes, candidate_hash, count_json, created_at) VALUES(?,?,?,NULL,'fork',?,0,?,?,?,?,?,?)`,
		c.CheckpointID, c.ThreadID, c.RunID, newContextRev, c.SemanticBoundary, c.DurableBoundary, c.Blob, c.Hash, c.CountJSON, at); err != nil {
		return ForkOutcome{}, fmt.Errorf("sqlite: store fork checkpoint: %w", mapSQLiteError(err))
	}
	if err := importSources(ctx, tx, c.ThreadID, in.ThreadID, in.CheckpointID, c.CheckpointID, c.Imports); err != nil {
		return ForkOutcome{}, err
	}

	seq := &eventSeq{base: 0}
	common := func(task, run *string, evidence *string) protocol.EventCommon {
		return protocol.EventCommon{EventID: identity.NewEventID().String(), ThreadID: c.ThreadID, TaskID: task, RunID: run, ReceiptID: protocol.Str(receiptID), EvidenceID: evidence, RecordedAt: at}
	}
	created, err := appendEvent(ctx, tx, seq, eventRecord{
		common:  protocol.EventCommon{EventID: identity.NewEventID().String(), ThreadID: c.ThreadID, ReceiptID: protocol.Str(receiptID), RecordedAt: at},
		payload: protocol.SessionCreatedPayload{SessionID: src.sessionID, Binding: binding, Mode: mode}})
	if err != nil {
		return ForkOutcome{}, err
	}
	taskCreated, err := appendEvent(ctx, tx, seq, eventRecord{common: common(protocol.Str(c.TaskID), nil, nil),
		payload:   protocol.TaskCreatedPayload{TaskID: c.TaskID, TurnID: nil, ParentTaskID: nil, Kind: "session_maintenance"},
		causation: protocol.Str(created.EventID), dependencies: []string{created.EventID}})
	if err != nil {
		return ForkOutcome{}, err
	}
	started, err := appendEvent(ctx, tx, seq, eventRecord{common: common(protocol.Str(c.TaskID), runIDp, nil),
		payload: protocol.RunStartedPayload{RunID: c.RunID, PreviousRunID: nil, TraceID: traceID, WriterEpoch: epoch, EffectiveLimits: budget.Limits,
			DeadlineAt: budget.DeadlineAt, RecoveryPolicyRevision: recoveryRev},
		causation: protocol.Str(taskCreated.EventID), dependencies: []string{taskCreated.EventID}})
	if err != nil {
		return ForkOutcome{}, err
	}
	committed, err := appendEvent(ctx, tx, seq, eventRecord{common: common(protocol.Str(c.TaskID), runIDp, protocol.Str(afterEv)),
		payload: protocol.CheckpointCommittedPayload{CheckpointID: c.CheckpointID, Mode: "fork", CandidateHash: c.Hash, ContextRevision: newContextRev,
			SemanticBoundary: c.SemanticBoundary, DurableBoundary: c.DurableBoundary, BeforeCountEvidenceID: beforeEv, AfterCountEvidenceID: afterEv},
		causation: protocol.Str(started.EventID), dependencies: []string{started.EventID}})
	if err != nil {
		return ForkOutcome{}, err
	}

	// The maintenance Run ends where it began: completed, with the checkpoint it made, and
	// no final message, no Task meaning and nothing to resume.
	res := protocol.RunResult{RunID: c.RunID, TaskID: c.TaskID, Status: "completed", Code: ForkCode, FinalMessageID: nil, FinalText: "",
		Verification: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}, CriteriaRevision: nil},
		EvidenceIDs:  []string{beforeEv, afterEv}, UnresolvedActionIDs: []string{}, LastCheckpointID: protocol.Str(c.CheckpointID), Resumable: false}
	resultEvidence := identity.NewEvidenceID().String()
	res.EvidenceIDs = append(res.EvidenceIDs, resultEvidence)
	th := threadRow{sessionID: src.sessionID, owner: src.owner, policyRef: src.policyRef, writerEpoch: epoch}
	run := runRow{runID: c.RunID, taskID: c.TaskID, threadID: c.ThreadID, phase: "PersistingResult", status: "running", writerEpoch: epoch, limits: budget.Limits}
	ctxRev := int64(newContextRev)
	ended, err := terminalTx(ctx, tx, now, th, c.ThreadID, run, startRecord{system: true, runReceiptID: receiptID}, seq, &ctxRev,
		TerminalInput{Result: res, ResultEvidenceID: resultEvidence})
	if err != nil {
		return ForkOutcome{}, err
	}
	// The Thread stands on its checkpoint, at the context revision the checkpoint's expected
	// value plus one, with no active Run.
	upd, err := tx.ExecContext(ctx, `UPDATE threads SET event_seq=?, context_revision=?, current_checkpoint_id=? WHERE thread_id=? AND writer_epoch=? AND context_revision=0 AND event_seq=0`,
		seq.last(), newContextRev, c.CheckpointID, c.ThreadID, epoch)
	if err != nil {
		return ForkOutcome{}, fmt.Errorf("sqlite: fork thread: %w", mapSQLiteError(err))
	}
	if n, _ := upd.RowsAffected(); n != 1 {
		return ForkOutcome{}, protocol.NewError(protocol.CodeRevisionConflict, "the new thread changed during the fork")
	}
	return ForkOutcome{Result: result, Events: append([]protocol.Event{created, taskCreated, started, committed}, ended...)}, nil
}
