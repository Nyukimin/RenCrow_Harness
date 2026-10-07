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
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

const (
	// storeOwner is the logical store that owns every row this package creates.
	storeOwner = "harness.execution"
	// evidenceChunkSize bounds one stored chunk of an Evidence's raw bytes.
	evidenceChunkSize = 1 << 20
)

// Admission is what the caller (the Service) fixes about one admission: who is
// calling and through which entrypoint, which writer epoch it holds for the Thread,
// the recovery policy a new Run freezes, and how to find the limit caps of a
// session's policy. None of it comes from the request payload.
type Admission struct {
	Caller         intake.Caller
	Entrypoint     string // one of the five protocol entrypoints
	DeclaredOrigin string // the operator's explicit declaration at a CLI entrypoint, or ""
	// WriterEpoch is the epoch the driver holds for the Thread. It must equal the
	// Thread's stored epoch, or another driver has taken over and nothing is written.
	WriterEpoch int64
	Recovery    protocol.RecoveryPolicy
	// Caps returns the most a Run under policyRef may ask for (host limits and the
	// policy's, whichever is smaller). It is called inside the transaction, with
	// the policy_ref stored in the session, so there is no gap between reading the
	// session and resolving its limits.
	Caps func(policyRef string) (protocol.Limits, error)
	// HostAssets, when set, are what the host gives the new Run beyond the blocks its caller
	// passed (turn/start only): the AGENTS.md and skill-catalog blocks, the sealed skill files
	// the catalog names, and the record of their hashes. They are written in the admission's
	// own transaction, so a Run never exists without the assets it was admitted with.
	HostAssets *HostAssets
}

// HostAssets is the host's contribution to one Run (see Admission.HostAssets).
type HostAssets struct {
	// Blocks are context blocks of the host: a kind the caller may pass, no source (their
	// origin is "host"), and the revision of their kind and text.
	Blocks []protocol.ContextBlock
	// Files are sealed as Evidence of the Run, each under the ID the blocks name it by.
	Files []HostAssetFile
	// Manifest is the canonical JSON record of the hashes of everything above (Evidence of the
	// Run, private).
	Manifest []byte
}

// HostAssetFile is one file sealed as Evidence of the Run.
type HostAssetFile struct {
	EvidenceID string
	Purpose    string
	MediaType  string
	Data       []byte
}

// validate holds the assets to what the store relies on.
func (h *HostAssets) validate() error {
	bad := func(what string) error {
		return protocol.NewError(protocol.CodeInternal, "the host assets are not valid: %s", what)
	}
	for _, b := range h.Blocks {
		if b.Source != nil {
			return bad("a host block has a source")
		}
		switch b.Kind {
		case protocol.KindCharacterSystemPrompt, protocol.KindStableRuntimeContext, protocol.KindRecallPack, protocol.KindVariableRuntimeContext:
		default:
			return bad("a host block of a kind the caller may not pass")
		}
		if rev, err := protocol.ContextRevision(b.Kind, b.Text, nil); err != nil || rev != b.Revision {
			return bad("a host block's revision is not that of its kind and text")
		}
	}
	for _, f := range h.Files {
		if _, err := identity.ParseEvidenceID(f.EvidenceID); err != nil || f.Purpose == "" || f.MediaType == "" {
			return bad("a host file has no valid ID, purpose or media type")
		}
	}
	return nil
}

// StartOutcome is the result of one turn/start admission.
type StartOutcome struct {
	Result protocol.StartResult
	// Replayed is true when the key had already been accepted with the same
	// payload and the original result is returned unchanged.
	Replayed bool
	// Events are the events this call committed, exactly as events/read returns
	// them, so the caller can announce them once the commit is known. A replay
	// commits nothing and has none.
	Events []protocol.Event
}

type threadRow struct {
	sessionID, owner, policyRef string
	writerEpoch                 int64
	contextRev, controlRev      int64
	queueRev, eventSeq          int64
	activeRun                   sql.NullString
}

func forbidden() error {
	return protocol.NewError(protocol.CodeForbidden, "the thread is not accessible to this caller")
}

// AdmitStart (F22 AdmitTask with F34 PersistIntake and F35 ResolveLimits) accepts
// one turn/start in a single BEGIN IMMEDIATE transaction, or accepts nothing.
//
// paramsJSON is the exact params text of the request. It is decoded strictly and
// validated against the schema once, and the same bytes give the mutation payload
// hash, so what is checked and what is identified cannot differ.
//
// Order inside the transaction, each step before anything is written:
//
//  1. the Thread must exist and be readable by the caller (a missing and a
//     forbidden Thread look the same);
//  2. the (principal, idempotency_key) receipt is looked up first. The same
//     payload returns the original result, whatever has happened since: the
//     Thread may have an active Run, the revisions may have moved, the proof may
//     have expired or its nonce been used. Another payload is IDEMPOTENCY_CONFLICT;
//  3. only a new request needs control of the Thread, no active Run (BUSY), the
//     driver's writer epoch and the expected revisions;
//  4. the limits are resolved against the caps and refused, never clamped;
//  5. the origin is decided and any relay proof fully verified, including that its
//     nonce was never used.
//
// Then the raw input and each context block become sealed Evidence and items, the
// Turn, Task and Run are created, the receipt is recorded, and the input.accepted,
// task.created and run.started events are appended, all together. A failure at any
// point leaves nothing behind. The context is not applied here: input.applied and
// the revision change belong to the Kernel's Load step.
func (s *Store) AdmitStart(ctx context.Context, adm Admission, paramsJSON []byte) (StartOutcome, error) {
	if adm.WriterEpoch < 1 {
		return StartOutcome{}, protocol.NewError(protocol.CodeInvalidRequest, "a driver must hold a writer epoch of at least 1")
	}
	if adm.Caps == nil {
		return StartOutcome{}, protocol.NewError(protocol.CodeInternal, "admission has no limit resolver")
	}
	in, hash, err := decodeStart(adm, paramsJSON)
	if err != nil {
		return StartOutcome{}, err
	}
	recoveryRev, err := protocol.RecoveryPolicyRevision(adm.Recovery.ContractVersion, int(adm.Recovery.MaxAttemptsPerAct), adm.Recovery.AllowedProfiles)
	if err != nil {
		return StartOutcome{}, protocol.NewError(protocol.CodeInternal, "recovery policy is invalid").Wrap(err)
	}
	recoveryJSON, err := protocol.Encode(adm.Recovery)
	if err != nil {
		return StartOutcome{}, protocol.NewError(protocol.CodeInternal, "recovery policy is invalid").Wrap(err)
	}

	var out StartOutcome
	err = s.write(ctx, func(tx *sql.Tx, now time.Time) error {
		var err error
		out, err = admitTx(ctx, tx, now, adm, in, hash, recoveryJSON, recoveryRev)
		return err
	})
	return out, err
}

// decodeStart decodes turn/start params strictly and computes their mutation
// payload hash from the same bytes.
func decodeStart(adm Admission, paramsJSON []byte) (protocol.StartInput, string, error) {
	in, err := protocol.Decode[protocol.StartInput](paramsJSON)
	if err != nil {
		return protocol.StartInput{}, "", err
	}
	hash, err := protocol.MutationPayloadHash(adm.Caller.Principal, "turn/start", paramsJSON)
	if err != nil {
		return protocol.StartInput{}, "", protocol.NewError(protocol.CodeInvalidParams, "params cannot be identified").Wrap(err)
	}
	return in, hash, nil
}

// LookupStart answers only the replay half of AdmitStart: whether this exact
// request was already accepted, and the original result if so. It takes no
// writer role and writes nothing, so a process that does not (or cannot) drive
// the Thread can still tell a client what became of its request. The order is the
// one AdmitStart uses: the Thread must be readable, then the receipt decides.
// A miss is (zero, false, nil); the caller then goes on to AdmitStart.
func (s *Store) LookupStart(ctx context.Context, adm Admission, paramsJSON []byte) (StartOutcome, bool, error) {
	in, hash, err := decodeStart(adm, paramsJSON)
	if err != nil {
		return StartOutcome{}, false, err
	}
	th, found, err := loadThread(ctx, s.db, in.ThreadID)
	if err != nil {
		return StartOutcome{}, false, err
	}
	if !found || !adm.Caller.CanRead(th.owner) {
		return StartOutcome{}, false, forbidden()
	}
	payload, hit, err := lookupReceipt(ctx, s.db, adm.Caller.Principal, in.IdempotencyKey, "turn/start", hash)
	if err != nil || !hit {
		return StartOutcome{}, false, err
	}
	sr, err := payload.StartResult()
	if err != nil {
		return StartOutcome{}, false, protocol.NewError(protocol.CodeIntegrityBlocked, "the stored receipt is not a start result").Wrap(err)
	}
	return StartOutcome{Result: sr, Replayed: true}, true, nil
}

// write runs f in one BEGIN IMMEDIATE transaction (the pool's _txlock mode) and
// commits it. A failure before the commit rolls back and is returned as it is
// (mapped for BUSY and a full store). A commit that fails is PERSISTENCE_UNCERTAIN:
// whether it took effect is not known, and the answer is to ask again with the same
// idempotency key, never to build a second operation.
func (s *Store) write(ctx context.Context, f func(tx *sql.Tx, now time.Time) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return mapSQLiteError(err)
	}
	if err := f(tx, s.now()); err != nil {
		_ = tx.Rollback()
		return err
	}
	// A failed COMMIT includes the case where the context ended and the transaction
	// was already rolled back: it is still reported as uncertain, the safe answer,
	// because only the receipt lookup can tell which happened.
	if err := tx.Commit(); err != nil {
		return protocol.NewError(protocol.CodePersistenceUncertain,
			"the commit did not complete; retry with the same idempotency key to learn the outcome").AsRetryable().Wrap(err)
	}
	return nil
}

func loadThread(ctx context.Context, tx queryer, threadID string) (threadRow, bool, error) {
	var t threadRow
	err := tx.QueryRowContext(ctx, `SELECT t.session_id, s.principal, s.policy_ref, t.writer_epoch, t.context_revision, t.control_revision,
		t.queue_revision, t.event_seq, t.active_run_id FROM threads t JOIN sessions s ON s.session_id=t.session_id WHERE t.thread_id=?`, threadID).
		Scan(&t.sessionID, &t.owner, &t.policyRef, &t.writerEpoch, &t.contextRev, &t.controlRev, &t.queueRev, &t.eventSeq, &t.activeRun)
	if errors.Is(err, sql.ErrNoRows) {
		return t, false, nil
	}
	if err != nil {
		return t, false, fmt.Errorf("sqlite: load thread: %w", mapSQLiteError(err))
	}
	return t, true, nil
}

// lookupReceipt returns the stored result of an earlier operation with this key.
func lookupReceipt(ctx context.Context, tx queryer, principal, key, operation, payloadHash string) (protocol.ReceiptPayload, bool, error) {
	var op, hash string
	var result sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT operation, payload_hash, result_json FROM receipts WHERE principal=? AND idempotency_key=?`, principal, key).
		Scan(&op, &hash, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.ReceiptPayload{}, false, nil
	}
	if err != nil {
		return protocol.ReceiptPayload{}, false, fmt.Errorf("sqlite: look up receipt: %w", mapSQLiteError(err))
	}
	if op != operation || hash != payloadHash {
		return protocol.ReceiptPayload{}, true, protocol.NewError(protocol.CodeIdempotencyConflict,
			"this idempotency key was already used for a different request")
	}
	if !result.Valid {
		return protocol.ReceiptPayload{}, true, protocol.NewError(protocol.CodeIntegrityBlocked, "the receipt of this key holds no result")
	}
	p, err := protocol.Decode[protocol.ReceiptPayload]([]byte(result.String))
	if err != nil {
		return protocol.ReceiptPayload{}, true, protocol.NewError(protocol.CodeIntegrityBlocked, "the stored receipt is not valid").Wrap(err)
	}
	return p, true, nil
}

func admitTx(ctx context.Context, tx *sql.Tx, now time.Time, adm Admission, in protocol.StartInput, payloadHash string, recoveryJSON []byte, recoveryRev string) (StartOutcome, error) {
	caller := adm.Caller
	th, found, err := loadThread(ctx, tx, in.ThreadID)
	if err != nil {
		return StartOutcome{}, err
	}
	if !found || !caller.CanRead(th.owner) {
		return StartOutcome{}, forbidden()
	}

	// An existing receipt answers before anything about the present is examined.
	if payload, hit, err := lookupReceipt(ctx, tx, caller.Principal, in.IdempotencyKey, "turn/start", payloadHash); err != nil {
		return StartOutcome{}, err
	} else if hit {
		sr, err := payload.StartResult()
		if err != nil {
			return StartOutcome{}, protocol.NewError(protocol.CodeIntegrityBlocked, "the stored receipt is not a start result").Wrap(err)
		}
		return StartOutcome{Result: sr, Replayed: true}, nil
	}

	if !caller.CanControl(th.owner) {
		return StartOutcome{}, forbidden()
	}
	if th.activeRun.Valid {
		return StartOutcome{}, protocol.NewError(protocol.CodeBusy, "the thread has an active run; use input/append").AsRetryable()
	}
	if th.writerEpoch != adm.WriterEpoch {
		return StartOutcome{}, protocol.NewError(protocol.CodeRevisionConflict, "the thread's writer epoch is not the driver's")
	}
	if th.contextRev != in.ExpectedContextRevision || th.controlRev != in.ExpectedControlRevision {
		return StartOutcome{}, protocol.NewError(protocol.CodeRevisionConflict, "the expected revisions are not the thread's current revisions")
	}

	caps, err := adm.Caps(th.policyRef)
	if err != nil {
		if errors.Is(err, intake.ErrPolicyUnavailable) {
			return StartOutcome{}, protocol.NewError(protocol.CodeForbidden, "the session's policy is not available").Wrap(err)
		}
		return StartOutcome{}, protocol.NewError(protocol.CodeInternal, "the session's limits could not be resolved").Wrap(err)
	}
	budget, err := intake.ResolveLimits(in.Limits, caps, now)
	if err != nil {
		return StartOutcome{}, err
	}

	decision, err := caller.Decide(intake.Claim{
		Entrypoint: adm.Entrypoint, DeclaredOrigin: adm.DeclaredOrigin, Method: "turn/start",
		IdempotencyKey: in.IdempotencyKey, DestinationThreadID: in.ThreadID, Text: in.Input.Text, Proof: in.Input.OriginProof,
	}, now)
	if err != nil {
		return StartOutcome{}, err
	}
	if decision.Proof != nil {
		var used int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM relay_nonces WHERE issuer=? AND key_id=? AND nonce=?`,
			decision.Proof.Issuer, decision.Proof.KeyID, decision.Proof.Nonce).Scan(&used); err != nil {
			return StartOutcome{}, fmt.Errorf("sqlite: check nonce: %w", mapSQLiteError(err))
		}
		if used != 0 {
			return StartOutcome{}, protocol.NewError(protocol.CodeInvalidOriginProof, "origin proof rejected: nonce was already used")
		}
	}

	// All checks passed. From here on only effects.
	acceptedAt := protocol.FormatTimestamp(now)
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM items WHERE thread_id=?`, in.ThreadID).Scan(&sequence); err != nil {
		return StartOutcome{}, fmt.Errorf("sqlite: allocate sequence: %w", mapSQLiteError(err))
	}

	receiptID := identity.NewReceiptID().String()
	messageID := identity.NewMessageID().String()
	turnID := identity.NewTurnID().String()
	taskID := identity.NewTaskID().String()
	runID := identity.NewRunID().String()
	traceID := identity.NewTraceID().String()

	inputEvidence, err := writeEvidence(ctx, tx, caller.Principal, evidenceMeta{Purpose: "intake_input"}, []byte(in.Input.Text))
	if err != nil {
		return StartOutcome{}, err
	}
	intakeReceipt := protocol.IntakeReceipt{
		ReceiptID: receiptID, MessageID: messageID, ThreadID: in.ThreadID, EvidenceID: inputEvidence,
		Principal: caller.Principal, Entrypoint: adm.Entrypoint, CallerProfileDigest: caller.ProfileDigest,
		DeclaredOrigin: decision.DeclaredOrigin, EffectiveOrigin: decision.EffectiveOrigin,
		ProofBasis: decision.ProofBasis, ProofDigest: decision.ProofDigest, AcceptedSequence: sequence, AcceptedAt: acceptedAt,
	}
	result := protocol.StartResult{
		ReceiptID: receiptID, Accepted: true, SessionID: th.sessionID, ThreadID: in.ThreadID, TurnID: turnID, TaskID: taskID,
		RunID: runID, TraceID: traceID, EffectiveLimits: budget.Limits, DeadlineAt: budget.DeadlineAt,
		RecoveryPolicyRevision: recoveryRev, Intake: intakeReceipt,
	}
	// The result is validated before it is stored, so a malformed receipt cannot exist.
	receiptPayload, err := protocol.NewReceiptPayload(result)
	if err != nil {
		return StartOutcome{}, protocol.NewError(protocol.CodeInternal, "the start result is not valid").Wrap(err)
	}
	receiptJSON, err := protocol.Encode(receiptPayload)
	if err != nil {
		return StartOutcome{}, protocol.NewError(protocol.CodeInternal, "the start result is not valid").Wrap(err)
	}

	record := intakeRecord{Kind: "intake_record", Intake: intakeReceipt, RawEvidenceID: inputEvidence, OriginProof: decision.Proof, MutationPayloadHash: payloadHash}
	if err := insertItem(ctx, tx, itemRow{
		messageID: messageID, threadID: in.ThreadID, sequence: sequence, historyKind: historyKind(decision.EffectiveOrigin),
		origin: decision.EffectiveOrigin, evidenceID: inputEvidence, metadata: record,
	}); err != nil {
		return StartOutcome{}, err
	}

	var upstreamJSON, parentTask, parentOwner any
	if in.Upstream != nil {
		raw, err := protocol.Encode(*in.Upstream)
		if err != nil {
			return StartOutcome{}, err
		}
		upstreamJSON, parentTask, parentOwner = string(raw), in.Upstream.TaskID, in.Upstream.Owner
	}
	limitsJSON, err := protocol.Encode(budget.Limits)
	if err != nil {
		return StartOutcome{}, err
	}
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO turns(turn_id, thread_id, source_message_id, root_task_id, status, created_at) VALUES(?,?,?,?,?,?)`,
			[]any{turnID, in.ThreadID, messageID, taskID, "active", acceptedAt}},
		{`INSERT INTO tasks(task_id, thread_id, origin_turn_id, parent_task_id, parent_owner, kind, upstream_json, status, last_run_id, created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, []any{taskID, in.ThreadID, turnID, parentTask, parentOwner, "work", upstreamJSON, "running", runID, acceptedAt}},
		{`INSERT INTO runs(run_id, task_id, trace_id, resume_source_run_id, phase, status, writer_epoch, started_at, ended_at, result_json,
			limits_json, deadline_at, recovery_policy_json, recovery_policy_revision, generation_attempts_used, generation_attempts_unknown)
			VALUES(?,?,?,NULL,?,?,?,?,NULL,NULL,?,?,?,?,0,0)`,
			[]any{runID, taskID, traceID, "Admitting", "running", th.writerEpoch, acceptedAt, string(limitsJSON), budget.DeadlineAt, string(recoveryJSON), recoveryRev}},
	} {
		if _, err := tx.ExecContext(ctx, stmt.q, stmt.args...); err != nil {
			return StartOutcome{}, fmt.Errorf("sqlite: admit: %w", mapSQLiteError(err))
		}
	}

	// The four typed context blocks are part of what the Run was started with, so
	// they are kept as sealed Evidence and items of their own, after the input.
	for i, b := range in.ContextBlocks {
		if err := storeContextBlock(ctx, tx, caller.Principal, in.ThreadID, sequence+1+int64(i), receiptID, b); err != nil {
			return StartOutcome{}, err
		}
	}
	// The host's own blocks follow the caller's (the same kind of block comes after the
	// caller's of that kind), and the files and the record of hashes belong to the Run.
	if h := adm.HostAssets; h != nil {
		if err := h.validate(); err != nil {
			return StartOutcome{}, err
		}
		for i, b := range h.Blocks {
			if err := storeContextBlock(ctx, tx, caller.Principal, in.ThreadID, sequence+1+int64(len(in.ContextBlocks))+int64(i), receiptID, b); err != nil {
				return StartOutcome{}, err
			}
		}
		for _, f := range h.Files {
			if _, err := writeEvidenceSpec(ctx, tx, evidenceSpec{ID: f.EvidenceID, Principal: caller.Principal, Meta: evidenceMeta{Purpose: f.Purpose}, RunID: protocol.Str(runID),
				MediaType: f.MediaType, Text: true}, f.Data); err != nil {
				return StartOutcome{}, err
			}
		}
		if len(h.Manifest) > 0 {
			if _, err := writeEvidenceSpec(ctx, tx, evidenceSpec{Principal: caller.Principal, Meta: evidenceMeta{Purpose: "host_assets_manifest"}, RunID: protocol.Str(runID),
				MediaType: "application/json"}, h.Manifest); err != nil {
				return StartOutcome{}, err
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO receipts(receipt_id, principal, idempotency_key, operation, payload_hash, stage, result_json, error_json, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,NULL,?,?)`, receiptID, caller.Principal, in.IdempotencyKey, "turn/start", payloadHash, "accepted", string(receiptJSON), acceptedAt, acceptedAt); err != nil {
		return StartOutcome{}, fmt.Errorf("sqlite: record receipt: %w", mapSQLiteError(err))
	}
	if decision.Proof != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO relay_nonces(issuer, key_id, nonce, payload_hash, receipt_id, accepted_at) VALUES(?,?,?,?,?,?)`,
			decision.Proof.Issuer, decision.Proof.KeyID, decision.Proof.Nonce, payloadHash, receiptID, acceptedAt); err != nil {
			// The check above ran under the write lock, so a duplicate here would mean
			// the lock was not held. Only that is a replayed nonce; a full store or an
			// I/O failure is a storage fault and must not tell the caller its proof is bad.
			if isPrimaryKeyViolation(err) {
				return StartOutcome{}, protocol.NewError(protocol.CodeInvalidOriginProof, "origin proof rejected: nonce was already used").Wrap(err)
			}
			return StartOutcome{}, fmt.Errorf("sqlite: record nonce: %w", mapSQLiteError(err))
		}
	}

	seq := &eventSeq{base: th.eventSeq}
	common := func(task, run *string) protocol.EventCommon {
		return protocol.EventCommon{
			EventID: identity.NewEventID().String(), ThreadID: in.ThreadID, TaskID: task, RunID: run,
			ReceiptID: protocol.Str(receiptID), EvidenceID: protocol.Str(inputEvidence), RecordedAt: acceptedAt,
		}
	}
	accepted := common(protocol.Str(taskID), protocol.Str(runID))
	accepted.MessageID = protocol.Str(messageID)
	e1, err := appendEvent(ctx, tx, seq, eventRecord{
		common: accepted,
		payload: protocol.InputAcceptedPayload{Intake: intakeReceipt, QueueItemID: nil, Disposition: "initial",
			QueueRevision: th.queueRev, ControlRevision: th.controlRev},
	})
	if err != nil {
		return StartOutcome{}, err
	}
	var parent *string
	if in.Upstream != nil {
		parent = protocol.Str(in.Upstream.TaskID)
	}
	e2, err := appendEvent(ctx, tx, seq, eventRecord{
		common:    common(protocol.Str(taskID), nil),
		payload:   protocol.TaskCreatedPayload{TaskID: taskID, TurnID: protocol.Str(turnID), ParentTaskID: parent, Kind: "work"},
		causation: protocol.Str(e1.EventID), dependencies: []string{e1.EventID},
	})
	if err != nil {
		return StartOutcome{}, err
	}
	e3, err := appendEvent(ctx, tx, seq, eventRecord{
		common: common(protocol.Str(taskID), protocol.Str(runID)),
		payload: protocol.RunStartedPayload{RunID: runID, PreviousRunID: nil, TraceID: traceID, WriterEpoch: th.writerEpoch,
			EffectiveLimits: budget.Limits, DeadlineAt: budget.DeadlineAt, RecoveryPolicyRevision: recoveryRev},
		causation: protocol.Str(e2.EventID), dependencies: []string{e2.EventID},
	})
	if err != nil {
		return StartOutcome{}, err
	}

	// Compare-and-set on everything the checks above relied on. Inside an immediate
	// transaction nobody else can have changed these, so a miss is an invariant
	// failure, reported as a conflict and never as a success.
	res, err := tx.ExecContext(ctx, `UPDATE threads SET active_run_id=?, event_seq=? WHERE thread_id=? AND writer_epoch=? AND context_revision=?
		AND control_revision=? AND queue_revision=? AND event_seq=? AND active_run_id IS NULL`,
		runID, seq.last(), in.ThreadID, th.writerEpoch, th.contextRev, th.controlRev, th.queueRev, th.eventSeq)
	if err != nil {
		return StartOutcome{}, fmt.Errorf("sqlite: update thread: %w", mapSQLiteError(err))
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return StartOutcome{}, protocol.NewError(protocol.CodeRevisionConflict, "the thread changed during admission")
	}
	return StartOutcome{Result: result, Events: []protocol.Event{e1, e2, e3}}, nil
}

func historyKind(effectiveOrigin string) string {
	switch effectiveOrigin {
	case protocol.OriginHuman:
		return "HumanInstruction"
	case protocol.OriginAutomation:
		return "AutomationInstruction"
	}
	return "Protected"
}

// intakeRecord is the IntakeRecord of INTERNAL_CONTRACTS: the receipt, the raw
// Evidence, the proof that was verified (or null) and the mutation payload hash,
// stored in items.metadata_json next to the original they describe.
type intakeRecord struct {
	Kind                string                 `json:"kind"`
	Intake              protocol.IntakeReceipt `json:"intake"`
	RawEvidenceID       string                 `json:"raw_evidence_id"`
	OriginProof         *protocol.OriginProof  `json:"origin_proof"`
	MutationPayloadHash string                 `json:"mutation_payload_hash"`
}

// contextBlockRecord is the PreparedContextBlock of INTERNAL_CONTRACTS: the public
// block fields minus its text (which is the Evidence), plus what the Host computed.
type contextBlockRecord struct {
	Kind               string              `json:"kind"`
	BlockKind          string              `json:"block_kind"`
	Revision           string              `json:"revision"`
	ComputedTextDigest string              `json:"computed_text_digest"`
	ResolvedOrigin     string              `json:"resolved_origin"`
	ClaimedSource      *protocol.SourceRef `json:"claimed_source"`
	IntakeReceiptID    string              `json:"intake_receipt_ref"`
}

type itemRow struct {
	messageID, threadID string
	sequence            int64
	historyKind, origin string
	evidenceID          string
	metadata            any
}

func insertItem(ctx context.Context, tx *sql.Tx, it itemRow) error {
	meta, err := canonicalJSON(it.metadata)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO items(message_id, thread_id, run_id, sequence, history_kind, origin, evidence_id, metadata_json)
		VALUES(?,?,NULL,?,?,?,?,?)`, it.messageID, it.threadID, it.sequence, it.historyKind, it.origin, it.evidenceID, meta); err != nil {
		return fmt.Errorf("sqlite: insert item: %w", mapSQLiteError(err))
	}
	return nil
}

// storeContextBlock keeps one context block. A block without a source is Host
// context supplied by the authenticated caller profile and is origin "host". A
// block that names a source keeps that claim as data only: its origin is "unknown",
// because the caller attaching a source does not authenticate it.
func storeContextBlock(ctx context.Context, tx *sql.Tx, principal, threadID string, sequence int64, receiptID string, b protocol.ContextBlock) error {
	digest, err := protocol.TextDigest(b.Text)
	if err != nil {
		return protocol.NewError(protocol.CodeInvalidRequest, "context block text is not valid UTF-8").Wrap(err)
	}
	origin := protocol.OriginUnknown
	if b.Source == nil {
		origin = "host"
	}
	evidenceID, err := writeEvidence(ctx, tx, principal, evidenceMeta{Purpose: "context_block", BlockKind: b.Kind}, []byte(b.Text))
	if err != nil {
		return err
	}
	return insertItem(ctx, tx, itemRow{
		messageID: identity.NewMessageID().String(), threadID: threadID, sequence: sequence, historyKind: "HostContext", origin: origin, evidenceID: evidenceID,
		metadata: contextBlockRecord{Kind: "context_block", BlockKind: b.Kind, Revision: b.Revision, ComputedTextDigest: digest,
			ResolvedOrigin: origin, ClaimedSource: b.Source, IntakeReceiptID: receiptID},
	})
}

type evidenceMeta struct {
	Purpose   string `json:"purpose"`
	BlockKind string `json:"block_kind,omitempty"`
	// Stage and ContextRevision describe a model request: the stage it was for and the
	// Thread's context revision it was sent at (what the model was shown is what was applied
	// at or before it).
	Stage           string `json:"stage,omitempty"`
	ContextRevision *int64 `json:"context_revision,omitempty"`
}

// evidenceSpec describes one Evidence to write. ID is optional: a caller that must
// name the Evidence before the transaction (a result that lists its own Evidence)
// chooses it. Text marks bytes that are themselves the text projection (UTF-8 text);
// anything else is raw only.
type evidenceSpec struct {
	ID        string
	Principal string
	Meta      evidenceMeta
	RunID     *string
	AttemptID *string
	MediaType string
	Text      bool
	// Partial marks bytes that are only the first part of what there was (a capture
	// that hit its bound): the Evidence is sealed with capture_complete=false, so what
	// is missing is never read as "was never there".
	Partial bool
}

// writeEvidence stores raw bytes as sealed Evidence: a building row, ordered
// chunks, then one transition to sealed with the byte count and the digest. The
// bytes are stored exactly as received (no trimming, no newline or Unicode
// normalization); the text projection of a single text part is those same bytes.
// Evidence of an intake belongs to the thread, not yet to a Run, so run_id stays null.
func writeEvidence(ctx context.Context, tx *sql.Tx, principal string, meta evidenceMeta, data []byte) (string, error) {
	return writeEvidenceSpec(ctx, tx, evidenceSpec{Principal: principal, Meta: meta, MediaType: "text/plain; charset=utf-8", Text: true}, data)
}

// writeEvidenceSpec is writeEvidence for Evidence that belongs to a Run or an
// Attempt and may be binary or JSON.
func writeEvidenceSpec(ctx context.Context, tx *sql.Tx, spec evidenceSpec, data []byte) (string, error) {
	id := spec.ID
	if id == "" {
		id = identity.NewEvidenceID().String()
	} else if _, err := identity.ParseEvidenceID(id); err != nil {
		return "", protocol.NewError(protocol.CodeInternal, "an evidence ID is not canonical").Wrap(err)
	}
	if spec.Text && !utf8.Valid(data) {
		return "", protocol.NewError(protocol.CodeInternal, "text evidence is not valid UTF-8")
	}
	metaJSON, err := canonicalJSON(spec.Meta)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO evidence(evidence_id, principal, owner, run_id, attempt_id, state, media_type, metadata_json)
		VALUES(?,?,?,?,?,'building',?,?)`, id, spec.Principal, storeOwner, spec.RunID, spec.AttemptID, spec.MediaType, metaJSON); err != nil {
		return "", fmt.Errorf("sqlite: insert evidence: %w", mapSQLiteError(err))
	}
	for ordinal, start := 0, 0; start < len(data); ordinal++ {
		end := min(start+evidenceChunkSize, len(data))
		if _, err := tx.ExecContext(ctx, `INSERT INTO evidence_chunks(evidence_id, ordinal, byte_start, data) VALUES(?,?,?,?)`, id, ordinal, start, data[start:end]); err != nil {
			return "", fmt.Errorf("sqlite: insert evidence chunk: %w", mapSQLiteError(err))
		}
		start = end
	}
	sum := sha256.Sum256(data)
	rawHash := hex.EncodeToString(sum[:])
	var projVersion, projHash any
	if spec.Text {
		projVersion, projHash = "text/v1", rawHash
	}
	complete := 1
	if spec.Partial {
		complete = 0
	}
	if _, err := tx.ExecContext(ctx, `UPDATE evidence SET state='sealed', capture_complete=?, total_bytes=?, raw_hash=?, projection_version=?, projection_hash=?
		WHERE evidence_id=? AND state='building'`, complete, len(data), rawHash, projVersion, projHash, id); err != nil {
		return "", fmt.Errorf("sqlite: seal evidence: %w", mapSQLiteError(err))
	}
	return id, nil
}

// canonicalJSON is CJ1 of any value that encoding/json can marshal: the stored form
// of every JSON column this package writes, so equal values are equal bytes.
func canonicalJSON(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("sqlite: encode json: %w", err)
	}
	out, err := protocol.EncodeCanonicalContract(raw)
	if err != nil {
		return "", fmt.Errorf("sqlite: canonical json: %w", err)
	}
	return string(out), nil
}
