package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// ReserveInput describes the one generation attempt about to be sent. Everything in
// it was fixed before the attempt: the logical input, the measured digests and the
// request exactly as it will be sent.
type ReserveInput struct {
	Stage     string
	RequestID string // issued by the caller; the request being sent carries it
	// ArgsBytes is the Action's immutable snapshot of the logical request: the CJ1
	// text that the input digest is taken over.
	ArgsBytes []byte
	// RequestBytes is the request as sent, kept as private Evidence.
	RequestBytes       []byte
	InputDigest        string
	RequestDigest      string
	BindingFingerprint string
	ProfileID          string
	ProfileRevision    string
	Stream             bool
}

// Reservation is a recorded, dispatched generation attempt. The attempt budget it
// consumed is not given back whatever happens next.
type Reservation struct {
	ActionID          string
	AttemptID         string
	RequestID         string
	RequestEvidenceID string
	Ordinal           int64
	AttemptsUsed      int64
	Events            []protocol.Event
}

// ReserveGeneration is the transaction right before a generation is sent. In one
// BEGIN IMMEDIATE it checks that the driver still holds the Thread (writer epoch)
// and that nothing was cancelled since it read the control revision, that the Run is
// in phase Generating (Compacting for a compaction stage) with time and budget left
// (deadline, model steps for act, generation attempts), and then records the model Action, its Attempt as dispatched, the
// request as Evidence, the model call (generation state unknown until it ends), the
// consumed attempt, and the events action.prepared, action.dispatch_started,
// model.attempt_started and model.requested.
//
// The attempt is recorded as dispatched, not merely prepared: once this commits the
// request may be sent at any moment, and from then on only a recorded end settles
// what the generation did. A cancellation recorded before this transaction makes it
// fail with ErrControlChanged and nothing is dispatched; one recorded after it finds
// the Attempt already started.
func (s *Store) ReserveGeneration(ctx context.Context, f Fence, in ReserveInput) (Reservation, error) {
	var res Reservation
	err := s.fencedWrite(ctx, f, true, func(tx *sql.Tx, now time.Time, th threadRow, run runRow, start startRecord) error {
		// An act generation is sent from Generating and is a model step. A compaction stage
		// (Selection, Summary) is sent from Compacting and is not: it spends a generation
		// attempt and nothing else, so that the model steps stay the act's alone.
		wantPhase := "Generating"
		if in.Stage != "act" {
			wantPhase = "Compacting"
		}
		if run.phase != wantPhase {
			return ErrPhaseConflict
		}
		if !now.Before(run.deadline) {
			return ErrDeadlinePassed
		}
		if in.Stage == "act" {
			var steps int64
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions WHERE run_id=? AND kind='model' AND name='act'`, f.RunID).Scan(&steps); err != nil {
				return fmt.Errorf("sqlite: count model steps: %w", mapSQLiteError(err))
			}
			if steps >= run.limits.MaxModelSteps {
				return ErrStepBudget
			}
		}
		if run.attemptsUsed >= run.limits.MaxGenerationAttempts {
			return ErrGenerationBudget
		}
		var policyRevision string
		if err := tx.QueryRowContext(ctx, `SELECT policy_revision FROM threads WHERE thread_id=?`, f.ThreadID).Scan(&policyRevision); err != nil {
			return fmt.Errorf("sqlite: read policy revision: %w", mapSQLiteError(err))
		}

		at := protocol.FormatTimestamp(now)
		actionID, attemptID := identity.NewActionID().String(), identity.NewAttemptID().String()
		argsSum := sha256.Sum256(in.ArgsBytes)
		argsHash := hex.EncodeToString(argsSum[:])
		for _, stmt := range []struct {
			what string
			q    string
			args []any
		}{
			{"action", `INSERT INTO actions(action_id, run_id, kind, name, args_bytes, args_hash, status, current_attempt_id, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
				[]any{actionID, f.RunID, "model", in.Stage, in.ArgsBytes, argsHash, "in_flight", attemptID, at}},
			{"attempt", `INSERT INTO attempts(attempt_id, action_id, ordinal, state, started_at) VALUES(?,?,0,'dispatch_started',?)`,
				[]any{attemptID, actionID, at}},
		} {
			if _, err := tx.ExecContext(ctx, stmt.q, stmt.args...); err != nil {
				return fmt.Errorf("sqlite: reserve generation (%s): %w", stmt.what, mapSQLiteError(err))
			}
		}
		requestEvidence, err := writeEvidenceSpec(ctx, tx, evidenceSpec{
			Principal: th.owner, Meta: evidenceMeta{Purpose: "model_request", Stage: in.Stage, ContextRevision: &th.contextRev}, RunID: protocol.Str(f.RunID), AttemptID: protocol.Str(attemptID),
			MediaType: "application/json",
		}, in.RequestBytes)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO model_calls(request_id, run_id, action_id, attempt_id, stage, request_digest, binding_fingerprint,
			request_evidence_id, logical_requests, backend_attempts, generation_state, attempt_ordinal, recovery_profile, recovery_profile_revision,
			base_request_digest, applied_transformations_json) VALUES(?,?,?,?,?,?,?,?,1,NULL,'unknown',0,?,?,?,'[]')`,
			in.RequestID, f.RunID, actionID, attemptID, in.Stage, in.RequestDigest, in.BindingFingerprint, requestEvidence, in.ProfileID, in.ProfileRevision, in.RequestDigest); err != nil {
			return fmt.Errorf("sqlite: reserve generation (model call): %w", mapSQLiteError(err))
		}
		used := run.attemptsUsed + 1
		if r, err := tx.ExecContext(ctx, `UPDATE runs SET generation_attempts_used=? WHERE run_id=? AND generation_attempts_used=?`, used, f.RunID, run.attemptsUsed); err != nil {
			return fmt.Errorf("sqlite: consume generation attempt: %w", mapSQLiteError(err))
		} else if n, _ := r.RowsAffected(); n != 1 {
			return protocol.NewError(protocol.CodeRevisionConflict, "the run's attempt budget changed during a reservation")
		}

		seq := &eventSeq{base: th.eventSeq}
		var prev *protocol.Event
		var events []protocol.Event
		add := func(p protocol.EventPayload) error {
			rec := eventRecord{common: runCommon(now, f.ThreadID, run, start, protocol.Str(requestEvidence)), payload: p}
			if prev != nil {
				rec.causation, rec.dependencies = protocol.Str(prev.EventID), []string{prev.EventID}
			}
			ev, err := appendEvent(ctx, tx, seq, rec)
			if err != nil {
				return err
			}
			prev, events = &ev, append(events, ev)
			return nil
		}
		for _, p := range []protocol.EventPayload{
			protocol.ActionPreparedPayload{ActionID: actionID, AttemptID: attemptID, Kind: "model", Name: in.Stage, ArgsHash: argsHash, PolicyRevision: policyRevision},
			protocol.ActionDispatchStartedPayload{ActionID: actionID, AttemptID: attemptID, WriterEpoch: th.writerEpoch, ControlRevision: th.controlRev},
			protocol.ModelAttemptStartedPayload{ActionID: actionID, AttemptID: attemptID, Ordinal: 0, RequestID: in.RequestID, GenerationAttemptsUsed: used,
				DeadlineAt: protocol.FormatTimestamp(run.deadline)},
			protocol.ModelRequestedPayload{ActionID: actionID, AttemptID: attemptID, RequestID: in.RequestID, Stage: in.Stage, Ordinal: 0,
				InputDigest: in.InputDigest, RequestDigest: in.RequestDigest, BindingFingerprint: in.BindingFingerprint, Stream: in.Stream},
		} {
			if err := add(p); err != nil {
				return err
			}
		}
		if err := finishThread(ctx, tx, th, f.ThreadID, seq, th.contextRev, false); err != nil {
			return err
		}
		res = Reservation{ActionID: actionID, AttemptID: attemptID, RequestID: in.RequestID, RequestEvidenceID: requestEvidence, Ordinal: 0, AttemptsUsed: used, Events: events}
		return nil
	})
	return res, err
}

// AttemptEnd is the recorded end of one generation attempt.
type AttemptEnd struct {
	AttemptID string
	// State is the Attempt's state: completed (a valid output), failed (a known end
	// that gave no usable output) or unknown (the end is not known).
	State string
	// Outcome is the model.completed outcome: completed, incomplete, refused or error.
	Outcome         string
	GenerationState string // not_started, terminal or unknown
	BackendAttempts *int64 // 0, 1 or nil exactly as GenerationState says
	FailureCode     *string
	ResponseID      *string // the Harness's rsp_ ID, when a response arrived
	RawResponse     []byte  // the response as received, when there is one
	// RawResponseMediaType is the media type of RawResponse: a strict event stream when
	// empty, otherwise what the response was (a non-2xx refusal or a non-stream answer is
	// application/json).
	RawResponseMediaType string
	// RawResponsePartial says RawResponse is only the first part of the response (its
	// bound was reached): its Evidence is sealed as an incomplete capture.
	RawResponsePartial bool
	// AppliedTransformations are the transformations the model side says it applied to the
	// request (the receipt's), empty when none or when no receipt was observed.
	AppliedTransformations []string
	// Diagnosis says, without any model text, what in the response broke the contract, and
	// SourceCode is the code the model side stated for a refusal (a plain code, or
	// HTTP_<status>). Either one makes a private diagnosis Evidence of the Attempt, so that
	// why a generation ended as unknown or as a contract failure can be read afterwards.
	Diagnosis     string
	SourceCode    *string
	UsageJSON     []byte // CJ1 of the usage, when known
	UsageComplete bool
}

// AttemptEndResult is what recording an end produced.
type AttemptEndResult struct {
	ReceiptEvidenceID  string
	ResponseEvidenceID *string
	Events             []protocol.Event
}

// RecordAttemptEnd records how one generation attempt ended, in one transaction: the
// response as private Evidence (when there was one), the ModelAttemptReceipt as
// Evidence, the model call, the Attempt and the Action, the unknown counter when the
// end is unknown, and model.completed. An Attempt ends once: a second call is
// ErrAttemptEnded and changes nothing. The writer epoch is checked: a driver that
// lost the Thread records nothing, and what it was waiting for stays an unknown
// attempt for the next driver to settle.
func (s *Store) RecordAttemptEnd(ctx context.Context, f Fence, in AttemptEnd) (AttemptEndResult, error) {
	var out AttemptEndResult
	err := s.fencedWrite(ctx, f, false, func(tx *sql.Tx, now time.Time, th threadRow, run runRow, start startRecord) error {
		seq := &eventSeq{base: th.eventSeq}
		var err error
		out, err = endAttemptTx(ctx, tx, now, th, f.ThreadID, run, start, seq, in)
		if err != nil {
			return err
		}
		return finishThread(ctx, tx, th, f.ThreadID, seq, th.contextRev, false)
	})
	return out, err
}

type attemptFacts struct {
	actionID, requestID, stage                          string
	ordinal                                             int64
	inputDigest, requestDigest, baseDigest, fingerprint string
	profile, profileRevision, requestEvidence           string
}

func loadAttemptFacts(ctx context.Context, tx *sql.Tx, runID, attemptID string) (attemptFacts, string, error) {
	var a attemptFacts
	var state string
	err := tx.QueryRowContext(ctx, `SELECT a.state, a.action_id, a.ordinal, m.request_id, m.stage, m.request_digest, m.base_request_digest, m.binding_fingerprint,
		m.recovery_profile, m.recovery_profile_revision, m.request_evidence_id
		FROM attempts a JOIN actions ac ON ac.action_id=a.action_id JOIN model_calls m ON m.attempt_id=a.attempt_id
		WHERE a.attempt_id=? AND ac.run_id=?`, attemptID, runID).
		Scan(&state, &a.actionID, &a.ordinal, &a.requestID, &a.stage, &a.requestDigest, &a.baseDigest, &a.fingerprint, &a.profile, &a.profileRevision, &a.requestEvidence)
	if errors.Is(err, sql.ErrNoRows) {
		return a, "", protocol.NewError(protocol.CodeIntegrityBlocked, "an attempt has no model call")
	}
	if err != nil {
		return a, "", fmt.Errorf("sqlite: load attempt: %w", mapSQLiteError(err))
	}
	// The logical input digest is a fact of the model.requested event of this attempt.
	rows, err := tx.QueryContext(ctx, `SELECT payload_json FROM events WHERE run_id=? AND type='model.requested' ORDER BY event_seq`, runID)
	if err != nil {
		return a, "", fmt.Errorf("sqlite: load attempt request: %w", mapSQLiteError(err))
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return a, "", err
		}
		p, err := protocol.Event{Type: protocol.EventModelRequested, Payload: []byte(payload)}.TypedPayload()
		if err != nil {
			return a, "", protocol.NewError(protocol.CodeIntegrityBlocked, "a stored model.requested event is not valid").Wrap(err)
		}
		if mr := p.(*protocol.ModelRequestedPayload); mr.AttemptID == attemptID {
			a.inputDigest = mr.InputDigest
		}
	}
	if err := rows.Err(); err != nil {
		return a, "", err
	}
	if a.inputDigest == "" {
		return a, "", protocol.NewError(protocol.CodeIntegrityBlocked, "an attempt has no recorded request event")
	}
	return a, state, nil
}

func endAttemptTx(ctx context.Context, tx *sql.Tx, now time.Time, th threadRow, threadID string, run runRow, start startRecord, seq *eventSeq, in AttemptEnd) (AttemptEndResult, error) {
	facts, state, err := loadAttemptFacts(ctx, tx, run.runID, in.AttemptID)
	if err != nil {
		return AttemptEndResult{}, err
	}
	if state != "dispatch_started" && state != "prepared" && state != "running" {
		return AttemptEndResult{}, ErrAttemptEnded
	}
	var out AttemptEndResult
	runIDp, attemptp := protocol.Str(run.runID), protocol.Str(in.AttemptID)
	if len(in.RawResponse) > 0 {
		media := in.RawResponseMediaType
		if media == "" {
			media = "text/event-stream; charset=utf-8"
		}
		if !utf8.Valid(in.RawResponse) {
			media = "application/octet-stream"
		}
		id, err := writeEvidenceSpec(ctx, tx, evidenceSpec{Principal: th.owner, Meta: evidenceMeta{Purpose: "model_response"}, RunID: runIDp, AttemptID: attemptp, MediaType: media,
			Partial: in.RawResponsePartial}, in.RawResponse)
		if err != nil {
			return out, err
		}
		out.ResponseEvidenceID = &id
	}
	if in.Diagnosis != "" || in.SourceCode != nil {
		var violation *string
		if in.Diagnosis != "" {
			violation = &in.Diagnosis
		}
		raw, err := canonicalJSON(struct {
			Kind            string  `json:"kind"`
			AttemptID       string  `json:"attempt_id"`
			FailureCode     *string `json:"failure_code"`
			GenerationState string  `json:"generation_state"`
			Violation       *string `json:"violation"`
			SourceCode      *string `json:"source_code"`
		}{"model_attempt_diagnosis", in.AttemptID, in.FailureCode, in.GenerationState, violation, in.SourceCode})
		if err != nil {
			return out, err
		}
		if _, err := writeEvidenceSpec(ctx, tx, evidenceSpec{Principal: th.owner, Meta: evidenceMeta{Purpose: "model_attempt_diagnosis"}, RunID: runIDp, AttemptID: attemptp,
			MediaType: "application/json"}, []byte(raw)); err != nil {
			return out, err
		}
	}
	applied := append([]string{}, in.AppliedTransformations...)
	appliedJSON, err := canonicalJSON(applied)
	if err != nil {
		return out, err
	}
	receipt := protocol.ModelAttemptReceipt{
		ActionID: facts.actionID, AttemptID: in.AttemptID, Ordinal: facts.ordinal, RequestID: facts.requestID, ResponseID: in.ResponseID, Stage: facts.stage,
		BaseRequestDigest: facts.baseDigest, RequestDigest: facts.requestDigest, BindingFingerprint: facts.fingerprint,
		RecoveryProfile: facts.profile, RecoveryProfileRevision: facts.profileRevision, AppliedTransformations: applied,
		FailureCode: in.FailureCode, GenerationState: in.GenerationState, BackendAttempts: in.BackendAttempts, UsageComplete: in.UsageComplete,
		RequestEvidenceID: facts.requestEvidence, ResponseEvidenceID: out.ResponseEvidenceID, InputDigest: facts.inputDigest,
	}
	receiptJSON, err := protocol.Encode(receipt)
	if err != nil {
		return out, protocol.NewError(protocol.CodeInternal, "the attempt receipt is not valid").Wrap(err)
	}
	if out.ReceiptEvidenceID, err = writeEvidenceSpec(ctx, tx, evidenceSpec{Principal: th.owner, Meta: evidenceMeta{Purpose: "model_attempt_receipt"},
		RunID: runIDp, AttemptID: attemptp, MediaType: "application/json"}, receiptJSON); err != nil {
		return out, err
	}
	var usage any
	if in.UsageJSON != nil {
		usage = string(in.UsageJSON)
	}
	var responseEvidence any
	if out.ResponseEvidenceID != nil {
		responseEvidence = *out.ResponseEvidenceID
	}
	var backend any
	if in.BackendAttempts != nil {
		backend = *in.BackendAttempts
	}
	if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET response_id=?, response_evidence_id=?, backend_attempts=?, generation_state=?, receipt_json=?, usage_json=?,
		applied_transformations_json=? WHERE attempt_id=?`, in.ResponseID, responseEvidence, backend, in.GenerationState, string(receiptJSON), usage, appliedJSON, in.AttemptID); err != nil {
		return out, fmt.Errorf("sqlite: record model call: %w", mapSQLiteError(err))
	}
	result, err := canonicalJSON(struct {
		GenerationState string  `json:"generation_state"`
		FailureCode     *string `json:"failure_code"`
		ReceiptEvidence string  `json:"receipt_evidence_id"`
	}{in.GenerationState, in.FailureCode, out.ReceiptEvidenceID})
	if err != nil {
		return out, err
	}
	at := protocol.FormatTimestamp(now)
	if r, err := tx.ExecContext(ctx, `UPDATE attempts SET state=?, ended_at=?, result_json=? WHERE attempt_id=? AND state IN ('prepared','dispatch_started','running')`,
		in.State, at, result, in.AttemptID); err != nil {
		return out, fmt.Errorf("sqlite: end attempt: %w", mapSQLiteError(err))
	} else if n, _ := r.RowsAffected(); n != 1 {
		return out, ErrAttemptEnded
	}
	actionStatus := map[string]string{"completed": "completed", "failed": "failed", "unknown": "unknown"}[in.State]
	if actionStatus == "" {
		return out, protocol.NewError(protocol.CodeInternal, "an attempt end has an unknown state")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE actions SET status=? WHERE action_id=?`, actionStatus, facts.actionID); err != nil {
		return out, fmt.Errorf("sqlite: end action: %w", mapSQLiteError(err))
	}
	if in.GenerationState == "unknown" {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET generation_attempts_unknown=generation_attempts_unknown+1 WHERE run_id=?`, run.runID); err != nil {
			return out, fmt.Errorf("sqlite: count unknown attempt: %w", mapSQLiteError(err))
		}
	}
	ev, err := appendEvent(ctx, tx, seq, eventRecord{
		common: runCommon(now, threadID, run, start, protocol.Str(out.ReceiptEvidenceID)),
		payload: protocol.ModelCompletedPayload{ActionID: facts.actionID, AttemptID: in.AttemptID, Outcome: in.Outcome, GenerationState: in.GenerationState,
			FailureCode: in.FailureCode, AttemptReceiptEvidenceID: out.ReceiptEvidenceID},
	})
	if err != nil {
		return out, err
	}
	out.Events = []protocol.Event{ev}
	return out, nil
}
