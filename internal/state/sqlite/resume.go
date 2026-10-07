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

// ResumeAdmission is what the Service fixes about one run/resume: who is calling, which
// writer epoch it holds for the Thread, the recovery policy and limit caps (as in
// Admission), what the host allows, and what the host said about the processes of the
// Task's unknown Tool calls.
type ResumeAdmission struct {
	Admission
	// BindingAllowed says whether a binding the request names is one the host
	// configured. A request without a binding keeps the Thread's.
	BindingAllowed func(protocol.Binding) bool
	// Reconciled is the host's verdict (F29), by Attempt ID, on each unknown process
	// Attempt of the Task. It was made before the transaction: an OS call never happens
	// inside one. An unknown Attempt it has no verdict for is recorded as unchecked.
	Reconciled map[string]string
	// EndsOnUnknownGeneration decides the end of a Run that is admitted while a generation
	// of its Task has an unknown end: such a Run is never driven, because no generation
	// may start while an earlier one is not known to have ended and nothing here can ask
	// the model side. It is called inside the transaction with the Run as it stands (its
	// Unresolved are the Task's unknown generations) and gives the result the Run is
	// ended with. Without it a Task with an unknown generation cannot be resumed at all:
	// the admission fails and writes nothing.
	EndsOnUnknownGeneration func(StaleRun) (TerminalInput, error)
	// Standing is what the Service verified, before the admission, about the checkpoint the
	// Task's Thread stands on (kernel.VerifyStanding): its ID, nil for none. The admission
	// only fences it: the Thread must still stand on exactly that checkpoint, or nothing is
	// admitted. Nil means nothing was verified, which a store-level caller may say.
	Standing *Standing
}

// Standing is the checkpoint a Thread was verified to stand on.
type Standing struct{ CheckpointID *string }

// ResumeOutcome is the result of one run/resume.
type ResumeOutcome struct {
	Result protocol.ResumeResult
	// Replayed is true when the key had already been accepted with the same payload and
	// the original result is returned unchanged.
	Replayed bool
	// Prior is what the Task's earlier Runs consumed. It is carried beside the new Run's
	// own zeros, never merged into them, and the earlier Runs' records are not touched.
	Prior intake.PriorUsage
	// Events are the events this call committed (run.started of the new Run, and its
	// run.terminal when Blocked); a replay commits none.
	Events []protocol.Event
	// Blocked: the new Run was admitted already ended, blocked by an unknown generation of
	// its Task (see ResumeAdmission.EndsOnUnknownGeneration). Nothing is to be driven.
	Blocked bool
}

// TaskAccess is what a caller may do with the Thread of a Task.
type TaskAccess struct {
	ThreadID string
	ThreadAccess
}

// TaskAccess finds the Thread of a Task and what the caller may do with it. A Task that
// does not exist and one the caller cannot read are the same FORBIDDEN.
func (s *Store) TaskAccess(ctx context.Context, caller intake.Caller, taskID string) (TaskAccess, error) {
	var threadID string
	err := s.db.QueryRowContext(ctx, `SELECT thread_id FROM tasks WHERE task_id=?`, taskID).Scan(&threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskAccess{}, forbidden()
	}
	if err != nil {
		return TaskAccess{}, wrapRead("read task", err)
	}
	acc, err := s.ThreadAccess(ctx, caller, threadID)
	if err != nil {
		return TaskAccess{}, err
	}
	return TaskAccess{ThreadID: threadID, ThreadAccess: acc}, nil
}

func decodeResume(adm Admission, paramsJSON []byte) (protocol.ResumeInput, string, error) {
	in, err := protocol.Decode[protocol.ResumeInput](paramsJSON)
	if err != nil {
		return protocol.ResumeInput{}, "", err
	}
	hash, err := protocol.MutationPayloadHash(adm.Caller.Principal, "run/resume", paramsJSON)
	if err != nil {
		return protocol.ResumeInput{}, "", protocol.NewError(protocol.CodeInvalidParams, "params cannot be identified").Wrap(err)
	}
	return in, hash, nil
}

func resumeReplay(payload protocol.ReceiptPayload) (ResumeOutcome, error) {
	if payload.Type != protocol.ReceiptResumeResult {
		return ResumeOutcome{}, protocol.NewError(protocol.CodeIntegrityBlocked, "the stored receipt is not a resume result")
	}
	res, err := protocol.Decode[protocol.ResumeResult](payload.Value)
	if err != nil {
		return ResumeOutcome{}, protocol.NewError(protocol.CodeIntegrityBlocked, "the stored receipt is not valid").Wrap(err)
	}
	return ResumeOutcome{Result: res, Replayed: true}, nil
}

// LookupResume answers only the replay half of AdmitResume: whether this exact request
// was already accepted, and the original result if so. It takes no writer role and
// writes nothing. A miss is (zero, false, nil).
func (s *Store) LookupResume(ctx context.Context, adm Admission, paramsJSON []byte) (ResumeOutcome, bool, error) {
	in, hash, err := decodeResume(adm, paramsJSON)
	if err != nil {
		return ResumeOutcome{}, false, err
	}
	if _, err := s.TaskAccess(ctx, adm.Caller, in.TaskID); err != nil {
		return ResumeOutcome{}, false, err
	}
	payload, hit, err := lookupReceipt(ctx, s.db, adm.Caller.Principal, in.IdempotencyKey, "run/resume", hash)
	if err != nil || !hit {
		return ResumeOutcome{}, false, err
	}
	out, err := resumeReplay(payload)
	return out, true, err
}

// resumeFacts are what a resume is judged from.
type resumeFacts struct {
	th       threadRow
	threadID string
	turnID   sql.NullString
	last     runRow
	// current is the checkpoint the Thread stands on (nil for none).
	current *string
}

// checkResumable is the part of the admission that only reads: the Task can be resumed
// by this caller, now. It is run before the host is asked anything (Preflight) and again
// inside the admission's transaction, which is the one that decides. In order:
//
//   - the Task's Thread is readable and controllable by the caller (FORBIDDEN);
//   - the Thread has no active Run (BUSY: the old driver has not let go);
//   - the Task is a work Task whose last Run is the one the request named
//     (REVISION_CONFLICT) and has ended (BUSY otherwise);
//   - that Run's result is resumable (INVALID_REQUEST: a completed Task is never run
//     again automatically, and a refusal or a broken integrity is not resumed around);
//   - the expected control revision is the Thread's (REVISION_CONFLICT);
//   - a checkpoint is not asked for (UNSUPPORTED_CONTRACT: this build has none).
func checkResumable(ctx context.Context, q queryer, caller intake.Caller, in protocol.ResumeInput) (resumeFacts, error) {
	var f resumeFacts
	var kind string
	var lastRun sql.NullString
	err := q.QueryRowContext(ctx, `SELECT thread_id, kind, last_run_id, origin_turn_id FROM tasks WHERE task_id=?`, in.TaskID).Scan(&f.threadID, &kind, &lastRun, &f.turnID)
	if errors.Is(err, sql.ErrNoRows) {
		return f, forbidden()
	}
	if err != nil {
		return f, wrapRead("read task", err)
	}
	th, found, err := loadThread(ctx, q, f.threadID)
	if err != nil {
		return f, err
	}
	if !found || !caller.CanControl(th.owner) {
		return f, forbidden()
	}
	f.th = th
	if th.activeRun.Valid {
		return f, protocol.NewError(protocol.CodeBusy, "the thread has an active run; resume a task once its run has ended").AsRetryable()
	}
	if kind != "work" {
		return f, protocol.NewError(protocol.CodeInvalidRequest, "only a work task can be resumed")
	}
	if !lastRun.Valid || lastRun.String != in.ExpectedLastRunID {
		return f, protocol.NewError(protocol.CodeRevisionConflict, "the task's last run is not the one the request expected")
	}
	last, found, err := loadRunRow(ctx, q, lastRun.String)
	if err != nil {
		return f, err
	}
	if !found {
		return f, protocol.NewError(protocol.CodeIntegrityBlocked, "a task names a last run that does not exist")
	}
	f.last = last
	if last.status == "running" {
		return f, protocol.NewError(protocol.CodeBusy, "the task's last run has not ended").AsRetryable()
	}
	var resultJSON sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT result_json FROM runs WHERE run_id=?`, last.runID).Scan(&resultJSON); err != nil {
		return f, wrapRead("read run result", err)
	}
	if !resultJSON.Valid {
		return f, protocol.NewError(protocol.CodeIntegrityBlocked, "an ended run has no stored result")
	}
	res, err := protocol.Decode[protocol.RunResult]([]byte(resultJSON.String))
	if err != nil {
		return f, protocol.NewError(protocol.CodeIntegrityBlocked, "a stored run result does not satisfy the contract").Wrap(err)
	}
	if !res.Resumable {
		return f, protocol.NewError(protocol.CodeInvalidRequest, "the task's last run is not resumable")
	}
	if th.controlRev != in.ExpectedControlRevision {
		return f, protocol.NewError(protocol.CodeRevisionConflict, "the expected control revision is not the thread's current revision")
	}
	var current sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT current_checkpoint_id FROM threads WHERE thread_id=?`, f.threadID).Scan(&current); err != nil {
		return f, wrapRead("read current checkpoint", err)
	}
	f.current = nullStr(current)
	// A resume continues the Thread from where it stands, so a checkpoint the request names is
	// the one the Thread stands on, and no other: an older one is never resumed on (the
	// snapshot is not rewound silently, and a Thread's context is not rebuilt from a
	// checkpoint it has moved past). To work from an older checkpoint is to fork it.
	if in.CheckpointID != nil && (f.current == nil || *f.current != *in.CheckpointID) {
		return f, protocol.NewError(protocol.CodeRevisionConflict, "the thread does not stand on the checkpoint the request named; a resume does not rewind (fork the checkpoint instead)")
	}
	return f, nil
}

// ResumePlan is what the Service needs before it asks the host about the Task's unknown
// processes: the Thread to take the writer role of, the Run that was last, and the Tool
// Attempts of the Task whose outcome is unknown.
type ResumePlan struct {
	ThreadID  string
	LastRunID string
	Unknown   []ToolAttemptRef
}

// PreflightResume applies the read-only conditions of a resume to the Task as it is now
// (see checkResumable, and the binding and the limits the admission will judge) and lists the Task's unknown Tool Attempts, so the host can be
// asked about their processes before the admission's transaction. It writes nothing and
// decides nothing: AdmitResume checks again.
func (s *Store) PreflightResume(ctx context.Context, adm ResumeAdmission, paramsJSON []byte) (ResumePlan, error) {
	in, _, err := decodeResume(adm.Admission, paramsJSON)
	if err != nil {
		return ResumePlan{}, err
	}
	f, err := checkResumable(ctx, s.db, adm.Caller, in)
	if err != nil {
		return ResumePlan{}, err
	}
	// What the host can be asked to refuse is refused before the host is asked anything:
	// the binding and the limits are judged as the admission will judge them.
	if _, err := resolveResumeTarget(adm, in); err != nil {
		return ResumePlan{}, err
	}
	if _, _, err := resumeBudget(ctx, s.db, adm, f.th.policyRef, in, s.now()); err != nil {
		return ResumePlan{}, err
	}
	unknown, err := unknownToolAttempts(ctx, s.db, in.TaskID)
	if err != nil {
		return ResumePlan{}, err
	}
	return ResumePlan{ThreadID: f.threadID, LastRunID: f.last.runID, Unknown: unknown}, nil
}

// unknownToolAttempts lists the Tool Attempts of the Task's Runs that ended unknown.
func unknownToolAttempts(ctx context.Context, q queryer, taskID string) ([]ToolAttemptRef, error) {
	rows, err := q.QueryContext(ctx, `SELECT ac.run_id, a.action_id, a.attempt_id, ac.name, a.state, COALESCE(a.host_incarnation,''), COALESCE(a.process_token,'')
		FROM attempts a JOIN actions ac ON ac.action_id=a.action_id JOIN runs r ON r.run_id=ac.run_id
		WHERE r.task_id=? AND ac.kind='tool' AND a.state=? ORDER BY a.started_at, a.attempt_id`, taskID, ToolUnknown)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list unknown tool attempts: %w", mapSQLiteError(err))
	}
	defer rows.Close()
	var out []ToolAttemptRef
	for rows.Next() {
		var r ToolAttemptRef
		if err := rows.Scan(&r.RunID, &r.ActionID, &r.AttemptID, &r.Tool, &r.State, &r.HostIncarnation, &r.ProcessToken); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// unknownModelAttempts lists the generation Attempts of the Task's Runs that ended unknown.
func unknownModelAttempts(ctx context.Context, q queryer, taskID string) ([]UnresolvedAttempt, []string, error) {
	rows, err := q.QueryContext(ctx, `SELECT ac.run_id, a.action_id, a.attempt_id FROM attempts a JOIN actions ac ON ac.action_id=a.action_id
		JOIN runs r ON r.run_id=ac.run_id WHERE r.task_id=? AND ac.kind='model' AND a.state='unknown' ORDER BY a.started_at, a.attempt_id`, taskID)
	if err != nil {
		return nil, nil, fmt.Errorf("sqlite: list unknown generation attempts: %w", mapSQLiteError(err))
	}
	defer rows.Close()
	var out []UnresolvedAttempt
	var runs []string
	for rows.Next() {
		var u UnresolvedAttempt
		var run string
		if err := rows.Scan(&run, &u.ActionID, &u.AttemptID); err != nil {
			return nil, nil, err
		}
		out, runs = append(out, u), append(runs, run)
	}
	return out, runs, rows.Err()
}

// reconciledTool and reconciledModel are the entries of the Evidence a resume keeps of
// its look at the Task's unknown calls.
type reconciledTool struct {
	RunID     string `json:"run_id"`
	ActionID  string `json:"action_id"`
	AttemptID string `json:"attempt_id"`
	Tool      string `json:"tool"`
	// Effect is what is known of the call's effect: always unknown. A verdict says what
	// the host found of the process, never what the process did.
	Effect  string `json:"effect"`
	Verdict string `json:"verdict"`
}

type reconciledModel struct {
	RunID     string `json:"run_id"`
	ActionID  string `json:"action_id"`
	AttemptID string `json:"attempt_id"`
	// State is the generation's: unknown, and not asked about again (the model side
	// cannot be asked from here).
	State   string `json:"generation_state"`
	Queried bool   `json:"queried"`
}

type resumeReconciliation struct {
	Kind          string            `json:"kind"`
	PreviousRunID string            `json:"previous_run_id"`
	ToolAttempts  []reconciledTool  `json:"tool_attempts"`
	ModelAttempts []reconciledModel `json:"model_attempts"`
}

// AdmitResume (run/resume, F22 with F35) accepts one resume in a single BEGIN IMMEDIATE
// transaction, or accepts nothing: a new Run of the same Task, with a new Trace, that
// continues from the Thread's stored context. It does not run the old Run again and
// does not change it: the old Run's result, Actions and Attempts stay as they were, and
// the new Run points back at it (resume_source_run_id, run.started.previous_run_id).
//
// The order is that of AdmitStart: the key's receipt answers first (the same payload:
// the original result, whatever has happened since; another payload, limits included:
// IDEMPOTENCY_CONFLICT); only a new request is judged (checkResumable, then the writer
// epoch, then the binding, then the limits). The limits are explicit and the new
// deadline counts from this acceptance (F35); what the Task's earlier Runs consumed is
// returned beside the new Run's own zeros.
//
// A generation of the Task whose end is unknown stops the resume where it stands (the next
// act generation is never started while one is unresolved, and nothing here can ask the
// model side): the new Run is created, with its Trace, its limits and the receipt of the
// resume, and ended in the same transaction as blocked with MODEL_GENERATION_OUTCOME_UNKNOWN,
// listing those generations as unresolved. It is never driven, so nothing is generated, and
// the Evidence below says the generations were not queried.
//
// What the Task's Runs left unknown stays unknown. A Tool call whose effect is unknown is
// never run again to find out, and nothing here promotes it: the call keeps its end and
// its single action.completed. What the resume adds is the look at it: the host's verdict
// on each such process (made before the transaction) and the generations that are still
// unknown are sealed as one Evidence of the new Run, which the new Run's run.started
// names and whose dependencies are the old Run's own end and the unknown calls' events.
// The model that continues reads the answer "outcome unknown, not run again" that
// settled the call and decides what to do about it.
func (s *Store) AdmitResume(ctx context.Context, adm ResumeAdmission, paramsJSON []byte) (ResumeOutcome, error) {
	if adm.WriterEpoch < 1 {
		return ResumeOutcome{}, protocol.NewError(protocol.CodeInvalidRequest, "a driver must hold a writer epoch of at least 1")
	}
	if adm.Caps == nil {
		return ResumeOutcome{}, protocol.NewError(protocol.CodeInternal, "admission has no limit resolver")
	}
	in, hash, err := decodeResume(adm.Admission, paramsJSON)
	if err != nil {
		return ResumeOutcome{}, err
	}
	recoveryRev, err := protocol.RecoveryPolicyRevision(adm.Recovery.ContractVersion, int(adm.Recovery.MaxAttemptsPerAct), adm.Recovery.AllowedProfiles)
	if err != nil {
		return ResumeOutcome{}, protocol.NewError(protocol.CodeInternal, "recovery policy is invalid").Wrap(err)
	}
	recoveryJSON, err := protocol.Encode(adm.Recovery)
	if err != nil {
		return ResumeOutcome{}, protocol.NewError(protocol.CodeInternal, "recovery policy is invalid").Wrap(err)
	}
	var out ResumeOutcome
	err = s.write(ctx, func(tx *sql.Tx, now time.Time) error {
		var err error
		out, err = resumeTx(ctx, tx, now, adm, in, hash, recoveryJSON, recoveryRev)
		return err
	})
	return out, err
}

func resumeTx(ctx context.Context, tx *sql.Tx, now time.Time, adm ResumeAdmission, in protocol.ResumeInput, hash string, recoveryJSON []byte, recoveryRev string) (ResumeOutcome, error) {
	caller := adm.Caller
	// The Task's Thread must be readable before anything else is said about the request.
	var threadID string
	if err := tx.QueryRowContext(ctx, `SELECT thread_id FROM tasks WHERE task_id=?`, in.TaskID).Scan(&threadID); errors.Is(err, sql.ErrNoRows) {
		return ResumeOutcome{}, forbidden()
	} else if err != nil {
		return ResumeOutcome{}, wrapRead("read task", err)
	}
	th, found, err := loadThread(ctx, tx, threadID)
	if err != nil {
		return ResumeOutcome{}, err
	}
	if !found || !caller.CanRead(th.owner) {
		return ResumeOutcome{}, forbidden()
	}
	if payload, hit, err := lookupReceipt(ctx, tx, caller.Principal, in.IdempotencyKey, "run/resume", hash); err != nil {
		return ResumeOutcome{}, err
	} else if hit {
		return resumeReplay(payload)
	}

	facts, err := checkResumable(ctx, tx, caller, in)
	if err != nil {
		return ResumeOutcome{}, err
	}
	if st := adm.Standing; st != nil && !sameID(st.CheckpointID, facts.current) {
		return ResumeOutcome{}, protocol.NewError(protocol.CodeRevisionConflict, "the thread stands on another checkpoint than the one that was verified")
	}
	th = facts.th
	if th.writerEpoch != adm.WriterEpoch {
		return ResumeOutcome{}, protocol.NewError(protocol.CodeRevisionConflict, "the thread's writer epoch is not the driver's")
	}

	var storedBinding string
	if err := tx.QueryRowContext(ctx, `SELECT binding_json FROM threads WHERE thread_id=?`, threadID).Scan(&storedBinding); err != nil {
		return ResumeOutcome{}, wrapRead("read binding", err)
	}
	newBindingJSON, err := resolveResumeTarget(adm, in)
	if err != nil {
		return ResumeOutcome{}, err
	}
	budget, prior, err := resumeBudget(ctx, tx, adm, th.policyRef, in, now)
	if err != nil {
		return ResumeOutcome{}, err
	}

	// A generation whose end is unknown blocks the Run before it exists: without a decision
	// to end it, nothing is written.
	unknownGenerations, _, err := unknownModelAttempts(ctx, tx, in.TaskID)
	if err != nil {
		return ResumeOutcome{}, err
	}
	blocked := len(unknownGenerations) > 0
	if blocked && adm.EndsOnUnknownGeneration == nil {
		return ResumeOutcome{}, protocol.NewError(protocol.CodeInternal, "a task with an unknown generation cannot be resumed here")
	}

	// All checks passed. From here on only effects.
	at := protocol.FormatTimestamp(now)
	receiptID := identity.NewReceiptID().String()
	runID := identity.NewRunID().String()
	traceID := identity.NewTraceID().String()
	limitsJSON, err := protocol.Encode(budget.Limits)
	if err != nil {
		return ResumeOutcome{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(run_id, task_id, trace_id, resume_source_run_id, phase, status, writer_epoch, started_at, ended_at, result_json,
		limits_json, deadline_at, recovery_policy_json, recovery_policy_revision, generation_attempts_used, generation_attempts_unknown)
		VALUES(?,?,?,?,?,?,?,?,NULL,NULL,?,?,?,?,0,0)`,
		runID, in.TaskID, traceID, facts.last.runID, "Admitting", "running", th.writerEpoch, at, string(limitsJSON), budget.DeadlineAt, string(recoveryJSON), recoveryRev); err != nil {
		return ResumeOutcome{}, fmt.Errorf("sqlite: resume: %w", mapSQLiteError(err))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='running', last_run_id=? WHERE task_id=?`, runID, in.TaskID); err != nil {
		return ResumeOutcome{}, fmt.Errorf("sqlite: resume task: %w", mapSQLiteError(err))
	}
	if facts.turnID.Valid {
		if _, err := tx.ExecContext(ctx, `UPDATE turns SET status='active' WHERE turn_id=?`, facts.turnID.String); err != nil {
			return ResumeOutcome{}, fmt.Errorf("sqlite: resume turn: %w", mapSQLiteError(err))
		}
	}

	evidenceID, previousEnd, dependencies, err := recordResumeReconciliation(ctx, tx, adm, th, threadID, in.TaskID, runID, facts.last.runID)
	if err != nil {
		return ResumeOutcome{}, err
	}

	result := protocol.ResumeResult{
		ReceiptID: receiptID, TaskID: in.TaskID, ThreadID: threadID, PreviousRunID: facts.last.runID, RunID: runID, TraceID: traceID, CheckpointID: facts.current,
		EffectiveLimits: budget.Limits, DeadlineAt: budget.DeadlineAt, RecoveryPolicyRevision: recoveryRev,
	}
	if err := insertReceipt(ctx, tx, receiptRow{id: receiptID, principal: caller.Principal, key: in.IdempotencyKey, operation: "run/resume",
		hash: hash, stage: "accepted", at: at}, result); err != nil {
		return ResumeOutcome{}, err
	}

	seq := &eventSeq{base: th.eventSeq}
	started, err := appendEvent(ctx, tx, seq, eventRecord{
		common: protocol.EventCommon{
			EventID: identity.NewEventID().String(), ThreadID: threadID, TaskID: protocol.Str(in.TaskID), RunID: protocol.Str(runID),
			ReceiptID: protocol.Str(receiptID), EvidenceID: evidenceID, RecordedAt: at,
		},
		payload: protocol.RunStartedPayload{RunID: runID, PreviousRunID: protocol.Str(facts.last.runID), TraceID: traceID, WriterEpoch: th.writerEpoch,
			EffectiveLimits: budget.Limits, DeadlineAt: budget.DeadlineAt, RecoveryPolicyRevision: recoveryRev},
		causation: previousEnd, dependencies: dependencies,
	})
	if err != nil {
		return ResumeOutcome{}, err
	}

	events := []protocol.Event{started}
	if blocked {
		// Ended where it began: the Run is a record of the refusal to generate, and the
		// Thread is never given it as its active Run.
		run, found, err := loadRunRow(ctx, tx, runID)
		if err != nil {
			return ResumeOutcome{}, err
		}
		start, serr := loadStartRecord(ctx, tx, runID)
		if !found || serr != nil {
			return ResumeOutcome{}, protocol.NewError(protocol.CodeIntegrityBlocked, "a resumed run cannot be read back").Wrap(serr)
		}
		evidence, err := runEvidenceIDs(ctx, tx, runID)
		if err != nil {
			return ResumeOutcome{}, err
		}
		in2, err := adm.EndsOnUnknownGeneration(StaleRun{RunID: runID, TaskID: in.TaskID, Phase: "Admitting", Now: now, DeadlineAt: run.deadline,
			Unresolved: unknownGenerations, EvidenceIDs: evidence, Limits: run.limits})
		if err != nil {
			return ResumeOutcome{}, err
		}
		ctxRev := th.contextRev
		ended, err := terminalTx(ctx, tx, now, th, threadID, run, start, seq, &ctxRev, in2)
		if err != nil {
			return ResumeOutcome{}, err
		}
		events = append(events, ended...)
		if err := finishThread(ctx, tx, th, threadID, seq, ctxRev, false); err != nil {
			return ResumeOutcome{}, err
		}
	} else {
		// Compare-and-set on everything the checks above relied on.
		res, err := tx.ExecContext(ctx, `UPDATE threads SET active_run_id=?, event_seq=? WHERE thread_id=? AND writer_epoch=? AND context_revision=?
			AND control_revision=? AND queue_revision=? AND event_seq=? AND active_run_id IS NULL`,
			runID, seq.last(), threadID, th.writerEpoch, th.contextRev, th.controlRev, th.queueRev, th.eventSeq)
		if err != nil {
			return ResumeOutcome{}, fmt.Errorf("sqlite: update thread: %w", mapSQLiteError(err))
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ResumeOutcome{}, protocol.NewError(protocol.CodeRevisionConflict, "the thread changed during admission")
		}
	}
	if newBindingJSON != "" && newBindingJSON != storedBinding {
		// An explicit new binding is applied to the stopped Thread with the new Run, which
		// is counted again from its first step (the driver describes and measures anew).
		if _, err := tx.ExecContext(ctx, `UPDATE threads SET binding_json=?, binding_revision=? WHERE thread_id=? AND writer_epoch=?`,
			newBindingJSON, in.Binding.ProfileRevision, threadID, th.writerEpoch); err != nil {
			return ResumeOutcome{}, fmt.Errorf("sqlite: update binding: %w", mapSQLiteError(err))
		}
	}
	return ResumeOutcome{Result: result, Prior: prior, Events: events, Blocked: blocked}, nil
}

// resolveResumeTarget judges the binding of a resume: none keeps the Thread's (the result
// is ""), one the host configured is returned as the canonical JSON the Thread will store,
// and any other is FORBIDDEN.
func resolveResumeTarget(adm ResumeAdmission, in protocol.ResumeInput) (string, error) {
	if in.Binding == nil {
		return "", nil
	}
	if adm.BindingAllowed == nil || !adm.BindingAllowed(*in.Binding) {
		return "", protocol.NewError(protocol.CodeForbidden, "the binding is not one the host allows")
	}
	return canonicalJSON(*in.Binding)
}

// resumeBudget (F35) resolves the new Run's limits against what the session's policy and
// the host allow, and reads what the Task's earlier Runs consumed.
func resumeBudget(ctx context.Context, q queryer, adm ResumeAdmission, policyRef string, in protocol.ResumeInput, now time.Time) (intake.ResumeBudget, intake.PriorUsage, error) {
	if adm.Caps == nil {
		return intake.ResumeBudget{}, intake.PriorUsage{}, protocol.NewError(protocol.CodeInternal, "admission has no limit resolver")
	}
	caps, err := adm.Caps(policyRef)
	if err != nil {
		if errors.Is(err, intake.ErrPolicyUnavailable) {
			return intake.ResumeBudget{}, intake.PriorUsage{}, protocol.NewError(protocol.CodeForbidden, "the session's policy is not available").Wrap(err)
		}
		return intake.ResumeBudget{}, intake.PriorUsage{}, protocol.NewError(protocol.CodeInternal, "the session's limits could not be resolved").Wrap(err)
	}
	prior, err := priorUsage(ctx, q, in.TaskID)
	if err != nil {
		return intake.ResumeBudget{}, intake.PriorUsage{}, err
	}
	budget, err := intake.ResolveResumeLimits(in.Limits, caps, now, prior)
	return budget, prior, err
}

// priorUsage is what the Task's Runs have consumed so far: model steps, generation
// attempts, and the attempts whose generation is unknown.
func priorUsage(ctx context.Context, q queryer, taskID string) (intake.PriorUsage, error) {
	var p intake.PriorUsage
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions a JOIN runs r ON r.run_id=a.run_id WHERE r.task_id=? AND a.kind='model' AND a.name='act'`, taskID).Scan(&p.ModelStepsUsed); err != nil {
		return p, fmt.Errorf("sqlite: count prior steps: %w", mapSQLiteError(err))
	}
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(generation_attempts_used),0), COALESCE(SUM(generation_attempts_unknown),0) FROM runs WHERE task_id=?`, taskID).
		Scan(&p.GenerationAttemptsUsed, &p.GenerationAttemptsUnknown); err != nil {
		return p, fmt.Errorf("sqlite: sum prior attempts: %w", mapSQLiteError(err))
	}
	return p, nil
}

// recordResumeReconciliation seals the Evidence of a resume's look at the Task's unknown
// calls (when there are any) and finds the events the new Run depends on: first the
// previous Run's run.terminal (also returned on its own, as the event the new Run
// directly follows), then the action.completed of each unknown Tool call. It returns
// the Evidence ID (nil when nothing was unknown), the previous Run's end and the
// dependencies.
func recordResumeReconciliation(ctx context.Context, tx *sql.Tx, adm ResumeAdmission, th threadRow, threadID, taskID, runID, previousRunID string) (*string, *string, []string, error) {
	var deps []string
	var previousEnd *string
	var terminal string
	err := tx.QueryRowContext(ctx, `SELECT event_id FROM events WHERE thread_id=? AND run_id=? AND type='run.terminal'`, threadID, previousRunID).Scan(&terminal)
	switch {
	case err == nil:
		previousEnd = protocol.Str(terminal)
		deps = append(deps, terminal)
	case !errors.Is(err, sql.ErrNoRows):
		return nil, nil, nil, fmt.Errorf("sqlite: find previous run end: %w", mapSQLiteError(err))
	}
	tools, err := unknownToolAttempts(ctx, tx, taskID)
	if err != nil {
		return nil, nil, nil, err
	}
	models, modelRuns, err := unknownModelAttempts(ctx, tx, taskID)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(tools) == 0 && len(models) == 0 {
		return nil, previousEnd, deps, nil
	}
	rec := resumeReconciliation{Kind: "resume_reconciliation", PreviousRunID: previousRunID, ToolAttempts: []reconciledTool{}, ModelAttempts: []reconciledModel{}}
	for _, t := range tools {
		verdict, ok := adm.Reconciled[t.AttemptID]
		if !ok {
			verdict = "unchecked"
		}
		rec.ToolAttempts = append(rec.ToolAttempts, reconciledTool{RunID: t.RunID, ActionID: t.ActionID, AttemptID: t.AttemptID, Tool: t.Tool, Effect: "unknown", Verdict: verdict})
		var completed string
		err := tx.QueryRowContext(ctx, `SELECT event_id FROM events WHERE thread_id=? AND type='action.completed' AND json_extract(payload_json,'$.attempt_id')=?
			ORDER BY event_seq DESC LIMIT 1`, threadID, t.AttemptID).Scan(&completed)
		switch {
		case err == nil:
			deps = append(deps, completed)
		case !errors.Is(err, sql.ErrNoRows):
			return nil, nil, nil, fmt.Errorf("sqlite: find unknown call event: %w", mapSQLiteError(err))
		}
	}
	for i, m := range models {
		rec.ModelAttempts = append(rec.ModelAttempts, reconciledModel{RunID: modelRuns[i], ActionID: m.ActionID, AttemptID: m.AttemptID, State: "unknown", Queried: false})
	}
	raw, err := canonicalJSON(rec)
	if err != nil {
		return nil, nil, nil, err
	}
	id, err := writeEvidenceSpec(ctx, tx, evidenceSpec{Principal: th.owner, Meta: evidenceMeta{Purpose: "resume_reconciliation"}, RunID: protocol.Str(runID),
		MediaType: "application/json"}, []byte(raw))
	if err != nil {
		return nil, nil, nil, err
	}
	return protocol.Str(id), previousEnd, deps, nil
}

// sameID reports whether two optional IDs are the same (both absent, or both the same).
func sameID(a, b *string) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}
