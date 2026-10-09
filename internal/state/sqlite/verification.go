package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

const (
	PurposeVerificationStdout = "verification_stdout"
	PurposeVerificationStderr = "verification_stderr"
)

// StartVerificationInput binds and starts the one fixed host verification action
// atomically. Its argument bytes contain only the criteria digest, never argv or env.
type StartVerificationInput struct {
	ActionID, AttemptID string
	PolicyRevision      string
	CriteriaRevision    string
}

// StartVerificationAttempt is allowed only while the Run is persisting its terminal
// result. Binding and dispatch-start share one transaction so a crash cannot leave a
// verifier Action that might later be dispatched a second time.
func (s *Store) StartVerificationAttempt(ctx context.Context, f Fence, in StartVerificationInput) (ToolStarted, error) {
	var out ToolStarted
	if !isSHA256(in.PolicyRevision) || !isSHA256(in.CriteriaRevision) || in.ActionID == "" || in.AttemptID == "" {
		return out, protocol.NewError(protocol.CodeInvalidParams, "verification action identifiers or revisions are invalid")
	}
	err := s.fencedWrite(ctx, f, true, func(tx *sql.Tx, now time.Time, th threadRow, run runRow, start startRecord) error {
		if run.phase != "PersistingResult" {
			return ErrPhaseConflict
		}
		if !now.Before(run.deadline) {
			return ErrDeadlinePassed
		}
		var policyRevision string
		if err := tx.QueryRowContext(ctx, `SELECT policy_revision FROM threads WHERE thread_id=?`, f.ThreadID).Scan(&policyRevision); err != nil {
			return fmt.Errorf("sqlite: read policy revision for verification: %w", mapSQLiteError(err))
		}
		if policyRevision != in.PolicyRevision {
			return ErrPolicyChanged
		}
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT action_id FROM actions WHERE run_id=? AND kind='verification'`, f.RunID).Scan(&existing)
		switch {
		case err == nil:
			return ErrAttemptState
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("sqlite: check existing verification action: %w", mapSQLiteError(err))
		}
		at := protocol.FormatTimestamp(now)
		args, err := canonicalJSON(struct {
			CriteriaRevision string `json:"criteria_revision"`
		}{in.CriteriaRevision})
		if err != nil {
			return err
		}
		argsHash := sha256.Sum256([]byte(args))
		argsDigest := hex.EncodeToString(argsHash[:])
		initial, err := canonicalJSON(verificationAttemptRecord{Status: "unknown", CriteriaRevision: in.CriteriaRevision})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO actions(action_id,run_id,kind,name,args_bytes,args_hash,status,current_attempt_id,created_at)
			VALUES(?,?,?,?,?,?,?,?,?)`, in.ActionID, f.RunID, "verification", "process.exec", []byte(args), argsDigest, ToolPrepared, in.AttemptID, at); err != nil {
			return fmt.Errorf("sqlite: prepare verification action: %w", mapSQLiteError(err))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempts(attempt_id,action_id,ordinal,state,started_at,result_json)
			VALUES(?,?,0,?,?,?)`, in.AttemptID, in.ActionID, ToolPrepared, at, initial); err != nil {
			return fmt.Errorf("sqlite: prepare verification attempt: %w", mapSQLiteError(err))
		}
		seq := &eventSeq{base: th.eventSeq}
		prepared, err := appendEvent(ctx, tx, seq, eventRecord{common: runCommon(now, f.ThreadID, run, start, nil), payload: protocol.ActionPreparedPayload{
			ActionID: in.ActionID, AttemptID: in.AttemptID, Kind: "verification", Name: "process.exec", ArgsHash: argsDigest, PolicyRevision: policyRevision}})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET state=? WHERE attempt_id=? AND state=?`, ToolDispatched, in.AttemptID, ToolPrepared); err != nil {
			return fmt.Errorf("sqlite: start verification attempt: %w", mapSQLiteError(err))
		}
		if _, err := tx.ExecContext(ctx, `UPDATE actions SET status=? WHERE action_id=?`, toolActionRunning, in.ActionID); err != nil {
			return fmt.Errorf("sqlite: start verification action: %w", mapSQLiteError(err))
		}
		dispatched, err := appendEvent(ctx, tx, seq, eventRecord{common: runCommon(now, f.ThreadID, run, start, nil), payload: protocol.ActionDispatchStartedPayload{
			ActionID: in.ActionID, AttemptID: in.AttemptID, WriterEpoch: th.writerEpoch, ControlRevision: th.controlRev}})
		if err != nil {
			return err
		}
		out = ToolStarted{Events: []protocol.Event{prepared, dispatched}, WriterEpoch: th.writerEpoch, ControlRevision: th.controlRev}
		return finishThread(ctx, tx, th, f.ThreadID, seq, th.contextRev, false)
	})
	return out, err
}

// CompleteVerificationInput records the verifier's single process result and its two
// sealed captures. No Tool-result Thread item is created.
type CompleteVerificationInput struct {
	ActionID, AttemptID string
	CriteriaRevision    string
	Status              string
	EffectState         string
	ExitCode            *int64
	CaptureComplete     bool
	Captures            []SealCapture
}

// CompleteVerificationAttempt closes the host Action and seals the process captures in
// one transaction. A completed Action is never dispatched again.
func (s *Store) CompleteVerificationAttempt(ctx context.Context, f Fence, in CompleteVerificationInput) (CompletedTool, error) {
	var out CompletedTool
	state, err := attemptStateOf(in.EffectState)
	if err != nil {
		return out, err
	}
	if !isSHA256(in.CriteriaRevision) || (in.Status != "passed" && in.Status != "failed" && in.Status != "unknown") || len(in.Captures) != 2 ||
		in.Captures[0].EvidenceID == "" || in.Captures[1].EvidenceID == "" || in.Captures[0].EvidenceID == in.Captures[1].EvidenceID ||
		in.Captures[0].Purpose != PurposeVerificationStdout || in.Captures[1].Purpose != PurposeVerificationStderr {
		return out, protocol.NewError(protocol.CodeInvalidParams, "verification result or capture set is invalid")
	}
	if (in.Status == "passed" && in.EffectState != "completed") || (in.Status == "unknown" && in.EffectState != "unknown") ||
		(in.Status == "failed" && in.EffectState != "failed" && in.EffectState != "cancelled") ||
		(in.Status == "passed" && (!in.CaptureComplete || in.ExitCode == nil || *in.ExitCode != 0)) {
		return out, protocol.NewError(protocol.CodeInvalidParams, "verification status does not match its process outcome")
	}
	err = s.fencedWrite(ctx, f, false, func(tx *sql.Tx, now time.Time, th threadRow, run runRow, start startRecord) error {
		var cur, kind, initial string
		if err := tx.QueryRowContext(ctx, `SELECT a.state,ac.kind,a.result_json FROM attempts a JOIN actions ac ON ac.action_id=a.action_id
			WHERE a.attempt_id=? AND ac.action_id=? AND ac.run_id=?`, in.AttemptID, in.ActionID, f.RunID).Scan(&cur, &kind, &initial); errors.Is(err, sql.ErrNoRows) {
			return protocol.NewError(protocol.CodeIntegrityBlocked, "a verification attempt does not belong to this run")
		} else if err != nil {
			return fmt.Errorf("sqlite: load verification attempt: %w", mapSQLiteError(err))
		}
		if kind != "verification" || (cur != ToolDispatched && cur != ToolRunning) {
			return ErrAttemptEnded
		}
		var previous verificationAttemptRecord
		if err := decodeVerificationRecord([]byte(initial), &previous); err != nil || previous.CriteriaRevision != in.CriteriaRevision {
			return protocol.NewError(protocol.CodeIntegrityBlocked, "verification criteria changed during its process attempt")
		}
		for _, c := range in.Captures {
			if c.Created {
				if err := sealBuildingTx(ctx, tx, c.EvidenceID, in.CaptureComplete, true); err != nil {
					return err
				}
			} else if _, err := writeEvidenceSpec(ctx, tx, evidenceSpec{ID: c.EvidenceID, Principal: th.owner,
				Meta: evidenceMeta{Purpose: c.Purpose}, RunID: protocol.Str(f.RunID), AttemptID: protocol.Str(in.AttemptID),
				MediaType: "text/plain; charset=utf-8", Text: true, Partial: !in.CaptureComplete}, nil); err != nil {
				return err
			}
		}
		result, err := canonicalJSON(verificationAttemptRecord{Status: in.Status, CriteriaRevision: in.CriteriaRevision, ExitCode: in.ExitCode,
			CaptureComplete: in.CaptureComplete})
		if err != nil {
			return err
		}
		if r, err := tx.ExecContext(ctx, `UPDATE attempts SET state=?,ended_at=?,result_json=? WHERE attempt_id=? AND state IN (?,?)`,
			state, protocol.FormatTimestamp(now), result, in.AttemptID, ToolDispatched, ToolRunning); err != nil {
			return fmt.Errorf("sqlite: end verification attempt: %w", mapSQLiteError(err))
		} else if n, _ := r.RowsAffected(); n != 1 {
			return ErrAttemptEnded
		}
		if _, err := tx.ExecContext(ctx, `UPDATE actions SET status=? WHERE action_id=?`, state, in.ActionID); err != nil {
			return fmt.Errorf("sqlite: end verification action: %w", mapSQLiteError(err))
		}
		seq := &eventSeq{base: th.eventSeq}
		ids := []string{in.Captures[0].EvidenceID, in.Captures[1].EvidenceID}
		ev, err := appendEvent(ctx, tx, seq, eventRecord{common: runCommon(now, f.ThreadID, run, start, protocol.Str(ids[0])), payload: protocol.ActionCompletedPayload{
			ActionID: in.ActionID, AttemptID: in.AttemptID, EffectState: in.EffectState, ExitCode: in.ExitCode, ResultEvidenceIDs: ids, CaptureComplete: in.CaptureComplete}})
		if err != nil {
			return err
		}
		out = CompletedTool{Events: []protocol.Event{ev}, ResultEvidenceIDs: ids}
		return finishThread(ctx, tx, th, f.ThreadID, seq, th.contextRev, false)
	})
	return out, err
}

type verificationAttemptRecord struct {
	Status           string `json:"status"`
	CriteriaRevision string `json:"criteria_revision"`
	ExitCode         *int64 `json:"exit_code,omitempty"`
	CaptureComplete  bool   `json:"capture_complete,omitempty"`
	Reconcile        string `json:"reconcile,omitempty"`
}

func decodeVerificationRecord(raw []byte, out any) error {
	if _, err := strictjson.Decode(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out)
}

func reconcileVerificationTx(ctx context.Context, tx *sql.Tx, now time.Time, th threadRow, threadID string, run runRow,
	start startRecord, seq *eventSeq, reconciled map[string]string) (*protocol.Verification, string, []protocol.Event, error) {
	var actionID, attemptID, state, recordJSON string
	err := tx.QueryRowContext(ctx, `SELECT ac.action_id,a.attempt_id,a.state,COALESCE(a.result_json,'')
		FROM actions ac JOIN attempts a ON a.action_id=ac.action_id AND a.ordinal=0
		WHERE ac.run_id=? AND ac.kind='verification'`, run.runID).Scan(&actionID, &attemptID, &state, &recordJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil, nil
	}
	if err != nil {
		return nil, "", nil, fmt.Errorf("sqlite: read verifier action: %w", mapSQLiteError(err))
	}
	var record verificationAttemptRecord
	if err := decodeVerificationRecord([]byte(recordJSON), &record); err != nil || !isSHA256(record.CriteriaRevision) {
		return nil, "", nil, protocol.NewError(protocol.CodeIntegrityBlocked, "verification criteria evidence is invalid")
	}
	unknownAction := ""
	var events []protocol.Event
	switch state {
	case ToolPrepared:
		record.Status, record.ExitCode, record.CaptureComplete = "not_run", nil, true
		closed, err := canonicalJSON(record)
		if err != nil {
			return nil, "", nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET state=?,ended_at=?,result_json=? WHERE attempt_id=? AND state=?`,
			ToolCancelled, protocol.FormatTimestamp(now), closed, attemptID, ToolPrepared); err != nil {
			return nil, "", nil, fmt.Errorf("sqlite: close unstarted verification: %w", mapSQLiteError(err))
		}
		if _, err := tx.ExecContext(ctx, `UPDATE actions SET status=? WHERE action_id=?`, ToolCancelled, actionID); err != nil {
			return nil, "", nil, fmt.Errorf("sqlite: close unstarted verification action: %w", mapSQLiteError(err))
		}
		ev, err := appendEvent(ctx, tx, seq, eventRecord{common: runCommon(now, threadID, run, start, nil), payload: protocol.ActionCompletedPayload{
			ActionID: actionID, AttemptID: attemptID, EffectState: "not_started", ResultEvidenceIDs: []string{}, CaptureComplete: true}})
		if err != nil {
			return nil, "", nil, err
		}
		events = append(events, ev)
	case ToolDispatched, ToolRunning:
		rows, err := tx.QueryContext(ctx, `SELECT evidence_id FROM evidence WHERE run_id=? AND attempt_id=? AND state='building'
			AND json_extract(metadata_json,'$.purpose') IN (?,?) ORDER BY evidence_id`, run.runID, attemptID, PurposeVerificationStdout, PurposeVerificationStderr)
		if err != nil {
			return nil, "", nil, fmt.Errorf("sqlite: find unsealed verification capture: %w", mapSQLiteError(err))
		}
		var building []string
		for rows.Next() {
			var evidenceID string
			if err := rows.Scan(&evidenceID); err != nil {
				_ = rows.Close()
				return nil, "", nil, err
			}
			building = append(building, evidenceID)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, "", nil, err
		}
		_ = rows.Close()
		for _, evidenceID := range building {
			if err := sealBuildingTx(ctx, tx, evidenceID, false, false); err != nil {
				return nil, "", nil, err
			}
		}
		ids, err := verificationEvidenceIDs(ctx, tx, run.runID, attemptID)
		if err != nil {
			return nil, "", nil, err
		}
		record.Status, record.ExitCode, record.CaptureComplete, record.Reconcile = "unknown", nil, false, reconciled[attemptID]
		closed, err := canonicalJSON(record)
		if err != nil {
			return nil, "", nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET state=?,ended_at=?,result_json=? WHERE attempt_id=? AND state IN (?,?)`,
			ToolUnknown, protocol.FormatTimestamp(now), closed, attemptID, ToolDispatched, ToolRunning); err != nil {
			return nil, "", nil, fmt.Errorf("sqlite: reconcile verification attempt: %w", mapSQLiteError(err))
		}
		if _, err := tx.ExecContext(ctx, `UPDATE actions SET status=? WHERE action_id=?`, ToolUnknown, actionID); err != nil {
			return nil, "", nil, fmt.Errorf("sqlite: reconcile verification action: %w", mapSQLiteError(err))
		}
		var evidenceRef *string
		if len(ids) > 0 {
			evidenceRef = protocol.Str(ids[0])
		}
		closedEvent, err := appendEvent(ctx, tx, seq, eventRecord{common: runCommon(now, threadID, run, start, evidenceRef), payload: protocol.ActionCompletedPayload{
			ActionID: actionID, AttemptID: attemptID, EffectState: "unknown", ResultEvidenceIDs: ids, CaptureComplete: false}})
		if err != nil {
			return nil, "", nil, err
		}
		events = append(events, closedEvent)
		unknownAction = actionID
	case ToolCompleted, ToolFailed, ToolCancelled, ToolUnknown:
	default:
		return nil, "", nil, protocol.NewError(protocol.CodeIntegrityBlocked, "verification attempt has an invalid state")
	}
	ids, err := verificationEvidenceIDs(ctx, tx, run.runID, attemptID)
	if err != nil {
		return nil, "", nil, err
	}
	var stored string
	if err := tx.QueryRowContext(ctx, `SELECT result_json FROM attempts WHERE attempt_id=?`, attemptID).Scan(&stored); err != nil {
		return nil, "", nil, fmt.Errorf("sqlite: reload verification result: %w", mapSQLiteError(err))
	}
	if err := decodeVerificationRecord([]byte(stored), &record); err != nil {
		return nil, "", nil, protocol.NewError(protocol.CodeIntegrityBlocked, "verification result evidence is invalid")
	}
	if record.Status != "passed" && record.Status != "failed" && record.Status != "unknown" && record.Status != "not_run" {
		return nil, "", nil, protocol.NewError(protocol.CodeIntegrityBlocked, "verification result status is invalid")
	}
	if record.Status == "passed" && (record.ExitCode == nil || *record.ExitCode != 0 || !record.CaptureComplete || len(ids) == 0) {
		record.Status = "unknown"
		unknownAction = actionID
	}
	if record.Status == "unknown" {
		unknownAction = actionID
	}
	criteria := record.CriteriaRevision
	if record.Status == "not_run" {
		ids = []string{}
	}
	return &protocol.Verification{Status: record.Status, EvidenceIDs: ids, CriteriaRevision: &criteria}, unknownAction, events, nil
}

func verificationEvidenceIDs(ctx context.Context, q queryer, runID, attemptID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT evidence_id FROM evidence WHERE run_id=? AND attempt_id=? AND state='sealed'
		AND json_extract(metadata_json,'$.purpose') IN (?,?)
		ORDER BY CASE json_extract(metadata_json,'$.purpose') WHEN ? THEN 0 ELSE 1 END`,
		runID, attemptID, PurposeVerificationStdout, PurposeVerificationStderr, PurposeVerificationStdout)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list verification Evidence: %w", mapSQLiteError(err))
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func isSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
