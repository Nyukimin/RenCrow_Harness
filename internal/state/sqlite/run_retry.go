package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// ErrBindingChanged: the Thread's binding is no longer the one the Run was admitted
// with, so a retry is not sent on it (RETRY_CONTRACT section 3: the control revision,
// the binding and the policy are unchanged).
var ErrBindingChanged = errors.New("sqlite: the thread's binding revision changed")

// ErrProfileNotAllowed: the recovery profile of a retry is not one the Run's frozen
// recovery policy lists. The driver chooses the profile from that policy; the store
// holds it to it again, so a retry never uses a profile the Run was not admitted with.
var ErrProfileNotAllowed = errors.New("sqlite: the run's recovery policy does not allow the profile")

// checkProfileAllowed reads the Run's frozen recovery policy and checks the profile
// against its allowed profiles.
func checkProfileAllowed(ctx context.Context, tx *sql.Tx, runID, profile string) error {
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT recovery_policy_json FROM runs WHERE run_id=?`, runID).Scan(&raw); err != nil {
		return fmt.Errorf("sqlite: read the run's recovery policy: %w", mapSQLiteError(err))
	}
	policy, err := protocol.Decode[protocol.RecoveryPolicy]([]byte(raw))
	if err != nil {
		return protocol.NewError(protocol.CodeIntegrityBlocked, "a stored recovery policy does not satisfy the contract").Wrap(err)
	}
	if !slices.Contains(policy.AllowedProfiles, profile) {
		return ErrProfileNotAllowed
	}
	return nil
}

// retryFailed is what a retry is scheduled from: the first Attempt of an act Action,
// ended as a failure whose generation is known to have not started or to have ended.
type retryFailed struct {
	actionID, attemptID, requestID, state string
	requestDigest                         string
	receiptEvidenceID                     string
	modelCompletedEvent                   sql.NullString
}

// loadRetryFailed reads the Attempt a retry follows and refuses everything that is not
// a first act Attempt that ended as a known failure: an Attempt of another Run, one
// that has not ended, one that ended unknown (an unknown generation is never retried),
// one that was itself a retry, and one that already has its retry.
func loadRetryFailed(ctx context.Context, tx *sql.Tx, runID, attemptID string) (retryFailed, error) {
	var f retryFailed
	var stage, genState, attemptState string
	var ordinal int64
	var result sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT a.action_id, a.state, a.ordinal, a.result_json, m.request_id, m.stage, m.generation_state, m.request_digest
		FROM attempts a JOIN actions ac ON ac.action_id=a.action_id JOIN model_calls m ON m.attempt_id=a.attempt_id
		WHERE a.attempt_id=? AND ac.run_id=? AND ac.kind='model'`, attemptID, runID).
		Scan(&f.actionID, &attemptState, &ordinal, &result, &f.requestID, &stage, &genState, &f.requestDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return f, protocol.NewError(protocol.CodeIntegrityBlocked, "a retry names an attempt that is not a generation of the run")
	}
	if err != nil {
		return f, fmt.Errorf("sqlite: load the attempt to retry: %w", mapSQLiteError(err))
	}
	f.state = genState
	if stage != "act" || ordinal != 0 || attemptState != "failed" || (genState != "not_started" && genState != "terminal") || !result.Valid {
		return f, protocol.NewError(protocol.CodeIntegrityBlocked, "a retry names an attempt that cannot be retried")
	}
	var res struct {
		Receipt string `json:"receipt_evidence_id"`
	}
	if err := json.Unmarshal([]byte(result.String), &res); err != nil || res.Receipt == "" {
		return f, protocol.NewError(protocol.CodeIntegrityBlocked, "an ended attempt has no receipt").Wrap(err)
	}
	f.receiptEvidenceID = res.Receipt
	var retried int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE action_id=? AND ordinal=1`, f.actionID).Scan(&retried); err != nil {
		return f, fmt.Errorf("sqlite: count the action's attempts: %w", mapSQLiteError(err))
	}
	if retried != 0 {
		return f, protocol.NewError(protocol.CodeIntegrityBlocked, "an action has no more than one retry")
	}
	err = tx.QueryRowContext(ctx, `SELECT event_id FROM events WHERE run_id=? AND type='model.completed' AND json_extract(payload_json,'$.attempt_id')=?
		ORDER BY event_seq DESC LIMIT 1`, runID, attemptID).Scan(&f.modelCompletedEvent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return f, fmt.Errorf("sqlite: find the end of the attempt: %w", mapSQLiteError(err))
	}
	return f, nil
}

// RetrySchedule is what a retry decision is recorded as: the profile chosen and why, the
// wait, and the failure that triggered it. FallbackFrom and FallbackReason are empty
// when the preferred profile was used.
type RetrySchedule struct {
	FailedAttemptID string
	TriggerCode     string
	DelayMS         int64
	Profile         string
	ProfileRevision string
	FallbackFrom    string
	FallbackReason  string
	// AttemptsUsed and MaxAttempts are the Run's generation attempts when it was decided.
	AttemptsUsed, MaxAttempts int64
}

// RetryScheduled is the record a scheduled retry left.
type RetryScheduled struct {
	// EvidenceID is the retry receipt: the decision, with the ID of the failed Attempt's
	// ModelAttemptReceipt, which model.retry_scheduled names.
	EvidenceID string
	Events     []protocol.Event
}

type retryReceipt struct {
	Kind                     string  `json:"kind"`
	ContractVersion          string  `json:"contract_version"`
	ActionID                 string  `json:"action_id"`
	FailedAttemptID          string  `json:"failed_attempt_id"`
	FailedRequestID          string  `json:"failed_request_id"`
	AttemptReceiptEvidenceID string  `json:"attempt_receipt_evidence_id"`
	TriggerCode              string  `json:"trigger_code"`
	FailureGenerationState   string  `json:"failure_generation_state"`
	NextOrdinal              int64   `json:"next_ordinal"`
	RecoveryProfile          string  `json:"recovery_profile"`
	RecoveryProfileRevision  string  `json:"recovery_profile_revision"`
	FallbackFrom             *string `json:"fallback_from"`
	FallbackReason           *string `json:"fallback_reason"`
	DelayMS                  int64   `json:"delay_ms"`
	GenerationAttemptsUsed   int64   `json:"generation_attempts_used"`
	MaxGenerationAttempts    int64   `json:"max_generation_attempts"`
}

// ScheduleRetry records that the Run will retry its failed Attempt, before it waits: the
// retry receipt as private Evidence and model.retry_scheduled, in one transaction. The
// writer epoch and the control revision are checked, so a stop recorded before this
// commits means no retry is scheduled, and the Run must be in RetryWaiting with time
// left. It dispatches nothing and spends nothing: the attempt budget is spent by
// ReserveRetry, when the retry is actually sent.
func (s *Store) ScheduleRetry(ctx context.Context, f Fence, in RetrySchedule) (RetryScheduled, error) {
	var out RetryScheduled
	err := s.fencedWrite(ctx, f, true, func(tx *sql.Tx, now time.Time, th threadRow, run runRow, start startRecord) error {
		if run.phase != "RetryWaiting" {
			return ErrPhaseConflict
		}
		if !now.Before(run.deadline) {
			return ErrDeadlinePassed
		}
		if err := checkProfileAllowed(ctx, tx, f.RunID, in.Profile); err != nil {
			return err
		}
		failed, err := loadRetryFailed(ctx, tx, f.RunID, in.FailedAttemptID)
		if err != nil {
			return err
		}
		receipt := retryReceipt{
			Kind: "model_retry_receipt", ContractVersion: "act-recovery/v1", ActionID: failed.actionID, FailedAttemptID: in.FailedAttemptID,
			FailedRequestID: failed.requestID, AttemptReceiptEvidenceID: failed.receiptEvidenceID, TriggerCode: in.TriggerCode,
			FailureGenerationState: failed.state, NextOrdinal: 1, RecoveryProfile: in.Profile, RecoveryProfileRevision: in.ProfileRevision,
			DelayMS: in.DelayMS, GenerationAttemptsUsed: in.AttemptsUsed, MaxGenerationAttempts: in.MaxAttempts,
		}
		if in.FallbackFrom != "" {
			receipt.FallbackFrom, receipt.FallbackReason = &in.FallbackFrom, &in.FallbackReason
		}
		raw, err := canonicalJSON(receipt)
		if err != nil {
			return err
		}
		evidenceID, err := writeEvidenceSpec(ctx, tx, evidenceSpec{Principal: th.owner, Meta: evidenceMeta{Purpose: "model_retry_receipt"},
			RunID: protocol.Str(f.RunID), AttemptID: protocol.Str(in.FailedAttemptID), MediaType: "application/json"}, []byte(raw))
		if err != nil {
			return err
		}
		seq := &eventSeq{base: th.eventSeq}
		rec := eventRecord{
			common: runCommon(now, f.ThreadID, run, start, protocol.Str(evidenceID)),
			payload: protocol.ModelRetryScheduledPayload{ActionID: failed.actionID, FailedAttemptID: in.FailedAttemptID, NextOrdinal: 1, TriggerCode: in.TriggerCode,
				DelayMS: in.DelayMS, RecoveryProfile: in.Profile, RecoveryProfileRevision: in.ProfileRevision},
		}
		if failed.modelCompletedEvent.Valid {
			rec.causation, rec.dependencies = protocol.Str(failed.modelCompletedEvent.String), []string{failed.modelCompletedEvent.String}
		}
		ev, err := appendEvent(ctx, tx, seq, rec)
		if err != nil {
			return err
		}
		if err := finishThread(ctx, tx, th, f.ThreadID, seq, th.contextRev, false); err != nil {
			return err
		}
		out = RetryScheduled{EvidenceID: evidenceID, Events: []protocol.Event{ev}}
		return nil
	})
	return out, err
}

// RetryReserve describes the retry about to be sent: the same Action's second Attempt.
type RetryReserve struct {
	FailedAttemptID string
	RequestID       string // issued by the caller; the request being sent carries it
	// RequestBytes is the request as sent, kept as private Evidence.
	RequestBytes       []byte
	InputDigest        string
	RequestDigest      string
	BindingFingerprint string
	ProfileID          string
	ProfileRevision    string
	Stream             bool
	// BindingRevision and PolicyRevision are those the Run was admitted with: a Thread
	// that no longer has them is not sent on.
	BindingRevision, PolicyRevision string
}

// ReserveRetry is the transaction right before a retry is sent, as ReserveGeneration is
// for the first Attempt, and it differs from it in exactly what a retry is: the same
// Action gets a second Attempt (ordinal 1) and no model step is counted, while the Run's
// generation attempt budget is spent for good. In one BEGIN IMMEDIATE it checks the
// writer epoch and that no stop was recorded since the control revision was read, that
// the Run is in Generating with time and an attempt left, that the Thread's binding and
// policy revisions are the Run's, and that the first Attempt ended as a known failure
// and has no retry yet; then it records the Attempt as dispatched, the request as
// Evidence, the model call (generation state unknown until it ends, base request digest
// the first Attempt's), the consumed attempt, and action.dispatch_started,
// model.attempt_started and model.requested. The Action goes back to in flight.
func (s *Store) ReserveRetry(ctx context.Context, f Fence, in RetryReserve) (Reservation, error) {
	var res Reservation
	err := s.fencedWrite(ctx, f, true, func(tx *sql.Tx, now time.Time, th threadRow, run runRow, start startRecord) error {
		if run.phase != "Generating" {
			return ErrPhaseConflict
		}
		if !now.Before(run.deadline) {
			return ErrDeadlinePassed
		}
		if run.attemptsUsed >= run.limits.MaxGenerationAttempts {
			return ErrGenerationBudget
		}
		var policyRevision, bindingRevision string
		if err := tx.QueryRowContext(ctx, `SELECT policy_revision, binding_revision FROM threads WHERE thread_id=?`, f.ThreadID).Scan(&policyRevision, &bindingRevision); err != nil {
			return fmt.Errorf("sqlite: read policy and binding revisions: %w", mapSQLiteError(err))
		}
		if policyRevision != in.PolicyRevision {
			return ErrPolicyChanged
		}
		if bindingRevision != in.BindingRevision {
			return ErrBindingChanged
		}
		if err := checkProfileAllowed(ctx, tx, f.RunID, in.ProfileID); err != nil {
			return err
		}
		failed, err := loadRetryFailed(ctx, tx, f.RunID, in.FailedAttemptID)
		if err != nil {
			return err
		}

		at := protocol.FormatTimestamp(now)
		attemptID := identity.NewAttemptID().String()
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempts(attempt_id, action_id, ordinal, state, started_at) VALUES(?,?,1,'dispatch_started',?)`,
			attemptID, failed.actionID, at); err != nil {
			return fmt.Errorf("sqlite: reserve retry (attempt): %w", mapSQLiteError(err))
		}
		if _, err := tx.ExecContext(ctx, `UPDATE actions SET status='in_flight', current_attempt_id=? WHERE action_id=?`, attemptID, failed.actionID); err != nil {
			return fmt.Errorf("sqlite: reserve retry (action): %w", mapSQLiteError(err))
		}
		requestEvidence, err := writeEvidenceSpec(ctx, tx, evidenceSpec{
			Principal: th.owner, Meta: evidenceMeta{Purpose: "model_request", Stage: "act", ContextRevision: &th.contextRev}, RunID: protocol.Str(f.RunID), AttemptID: protocol.Str(attemptID),
			MediaType: "application/json",
		}, in.RequestBytes)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO model_calls(request_id, run_id, action_id, attempt_id, stage, request_digest, binding_fingerprint,
			request_evidence_id, logical_requests, backend_attempts, generation_state, attempt_ordinal, recovery_profile, recovery_profile_revision,
			base_request_digest, applied_transformations_json) VALUES(?,?,?,?,'act',?,?,?,1,NULL,'unknown',1,?,?,?,'[]')`,
			in.RequestID, f.RunID, failed.actionID, attemptID, in.RequestDigest, in.BindingFingerprint, requestEvidence, in.ProfileID, in.ProfileRevision,
			failed.requestDigest); err != nil {
			return fmt.Errorf("sqlite: reserve retry (model call): %w", mapSQLiteError(err))
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
			} else {
				var scheduled sql.NullString
				err := tx.QueryRowContext(ctx, `SELECT event_id FROM events WHERE run_id=? AND type='model.retry_scheduled' AND json_extract(payload_json,'$.failed_attempt_id')=?
					ORDER BY event_seq DESC LIMIT 1`, f.RunID, in.FailedAttemptID).Scan(&scheduled)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("sqlite: find the scheduled retry: %w", mapSQLiteError(err))
				}
				if scheduled.Valid {
					rec.causation, rec.dependencies = protocol.Str(scheduled.String), []string{scheduled.String}
				}
			}
			ev, err := appendEvent(ctx, tx, seq, rec)
			if err != nil {
				return err
			}
			prev, events = &ev, append(events, ev)
			return nil
		}
		for _, p := range []protocol.EventPayload{
			protocol.ActionDispatchStartedPayload{ActionID: failed.actionID, AttemptID: attemptID, WriterEpoch: th.writerEpoch, ControlRevision: th.controlRev},
			protocol.ModelAttemptStartedPayload{ActionID: failed.actionID, AttemptID: attemptID, Ordinal: 1, RequestID: in.RequestID, GenerationAttemptsUsed: used,
				DeadlineAt: protocol.FormatTimestamp(run.deadline)},
			protocol.ModelRequestedPayload{ActionID: failed.actionID, AttemptID: attemptID, RequestID: in.RequestID, Stage: "act", Ordinal: 1,
				InputDigest: in.InputDigest, RequestDigest: in.RequestDigest, BindingFingerprint: in.BindingFingerprint, Stream: in.Stream},
		} {
			if err := add(p); err != nil {
				return err
			}
		}
		if err := finishThread(ctx, tx, th, f.ThreadID, seq, th.contextRev, false); err != nil {
			return err
		}
		res = Reservation{ActionID: failed.actionID, AttemptID: attemptID, RequestID: in.RequestID, RequestEvidenceID: requestEvidence, Ordinal: 1, AttemptsUsed: used, Events: events}
		return nil
	})
	return res, err
}
