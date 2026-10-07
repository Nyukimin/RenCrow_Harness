package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// SessionPlan is what session/open resolves from the host configuration: the real
// workspace root, the policy and mode the session is bound to, and the policy
// revision those resolve to. The store records it and decides none of it.
type SessionPlan struct {
	WorkspacePath  string
	PolicyRef      string
	ExecutionMode  string
	PolicyRevision string
}

// SessionAdmission is what the Service fixes about one session/open. Resolve is
// called inside the transaction, only for a request that is not a replay, so an
// answer already given stays the answer whatever the configuration says today.
type SessionAdmission struct {
	Caller  intake.Caller
	Resolve func(in protocol.SessionOpenInput) (SessionPlan, error)
}

// SessionOutcome is the result of one session/open.
type SessionOutcome struct {
	Result   protocol.SessionOpenResult
	Replayed bool
	// Events are the events this call committed (session.created); a replay has none.
	Events []protocol.Event
}

// OpenSession (session/open) creates a Session and its first Thread, the
// session.created event and the terminal receipt in one BEGIN IMMEDIATE
// transaction, or nothing. The caller becomes the session owner. Model generation
// is not involved. The order is that of AdmitStart: the key's receipt is looked up
// first (same payload: the original result; another payload or operation:
// IDEMPOTENCY_CONFLICT), and only a new request resolves the plan.
func (s *Store) OpenSession(ctx context.Context, adm SessionAdmission, paramsJSON []byte) (SessionOutcome, error) {
	if adm.Resolve == nil {
		return SessionOutcome{}, protocol.NewError(protocol.CodeInternal, "session admission has no plan resolver")
	}
	in, err := protocol.Decode[protocol.SessionOpenInput](paramsJSON)
	if err != nil {
		return SessionOutcome{}, err
	}
	hash, err := protocol.MutationPayloadHash(adm.Caller.Principal, "session/open", paramsJSON)
	if err != nil {
		return SessionOutcome{}, protocol.NewError(protocol.CodeInvalidParams, "params cannot be identified").Wrap(err)
	}
	var out SessionOutcome
	err = s.write(ctx, func(tx *sql.Tx, now time.Time) error {
		var err error
		out, err = openSessionTx(ctx, tx, now, adm, in, hash)
		return err
	})
	return out, err
}

func openSessionTx(ctx context.Context, tx *sql.Tx, now time.Time, adm SessionAdmission, in protocol.SessionOpenInput, payloadHash string) (SessionOutcome, error) {
	principal := adm.Caller.Principal
	if payload, hit, err := lookupReceipt(ctx, tx, principal, in.IdempotencyKey, "session/open", payloadHash); err != nil {
		return SessionOutcome{}, err
	} else if hit {
		if payload.Type != protocol.ReceiptSessionOpenResult {
			return SessionOutcome{}, protocol.NewError(protocol.CodeIntegrityBlocked, "the stored receipt is not a session/open result")
		}
		res, err := protocol.Decode[protocol.SessionOpenResult](payload.Value)
		if err != nil {
			return SessionOutcome{}, protocol.NewError(protocol.CodeIntegrityBlocked, "the stored receipt is not valid").Wrap(err)
		}
		return SessionOutcome{Result: res, Replayed: true}, nil
	}

	plan, err := adm.Resolve(in)
	if err != nil {
		return SessionOutcome{}, err
	}
	at := protocol.FormatTimestamp(now)
	sessionID := identity.NewSessionID().String()
	threadID := identity.NewThreadID().String()
	receiptID := identity.NewReceiptID().String()
	bindingJSON, err := canonicalJSON(in.Binding)
	if err != nil {
		return SessionOutcome{}, err
	}
	info := protocol.SessionInfo{
		SessionID: sessionID, ThreadID: threadID, WorkspacePath: plan.WorkspacePath, Binding: in.Binding, PolicyRef: plan.PolicyRef,
		ExecutionMode: plan.ExecutionMode, ContextRevision: 0, ControlRevision: 0, ActiveRunID: nil,
	}
	result := protocol.SessionOpenResult{ReceiptID: receiptID, Session: info}
	receiptPayload, err := protocol.NewReceiptPayload(result)
	if err != nil {
		return SessionOutcome{}, protocol.NewError(protocol.CodeInternal, "the session result is not valid").Wrap(err)
	}
	receiptJSON, err := protocol.Encode(receiptPayload)
	if err != nil {
		return SessionOutcome{}, protocol.NewError(protocol.CodeInternal, "the session result is not valid").Wrap(err)
	}

	for _, stmt := range []struct {
		what string
		q    string
		args []any
	}{
		{"session", `INSERT INTO sessions(session_id, principal, created_at, workspace_path, policy_ref, execution_mode) VALUES(?,?,?,?,?,?)`,
			[]any{sessionID, principal, at, plan.WorkspacePath, plan.PolicyRef, plan.ExecutionMode}},
		{"thread", `INSERT INTO threads(thread_id, session_id, binding_json, policy_revision, binding_revision, event_seq) VALUES(?,?,?,?,?,1)`,
			[]any{threadID, sessionID, bindingJSON, plan.PolicyRevision, in.Binding.ProfileRevision}},
		{"receipt", `INSERT INTO receipts(receipt_id, principal, idempotency_key, operation, payload_hash, stage, result_json, error_json, created_at, updated_at)
			VALUES(?,?,?,?,?,?,?,NULL,?,?)`, []any{receiptID, principal, in.IdempotencyKey, "session/open", payloadHash, "terminal", string(receiptJSON), at, at}},
	} {
		if _, err := tx.ExecContext(ctx, stmt.q, stmt.args...); err != nil {
			return SessionOutcome{}, fmt.Errorf("sqlite: open session (%s): %w", stmt.what, mapSQLiteError(err))
		}
	}
	ev, err := appendEvent(ctx, tx, &eventSeq{}, eventRecord{
		common:  protocol.EventCommon{EventID: identity.NewEventID().String(), ThreadID: threadID, ReceiptID: protocol.Str(receiptID), RecordedAt: at},
		payload: protocol.SessionCreatedPayload{SessionID: sessionID, Binding: in.Binding, Mode: plan.ExecutionMode},
	})
	if err != nil {
		return SessionOutcome{}, err
	}
	return SessionOutcome{Result: result, Events: []protocol.Event{ev}}, nil
}

// ThreadAccess is what a caller may do with a Thread.
type ThreadAccess struct {
	Owner      string
	CanControl bool
}

// ThreadAccess is the check that comes before any role is taken: the Thread must
// exist and be readable by the caller. A missing and an unreadable Thread give the
// same FORBIDDEN, so the answer says nothing about what exists.
func (s *Store) ThreadAccess(ctx context.Context, caller intake.Caller, threadID string) (ThreadAccess, error) {
	th, found, err := loadThread(ctx, s.db, threadID)
	if err != nil {
		return ThreadAccess{}, err
	}
	if !found || !caller.CanRead(th.owner) {
		return ThreadAccess{}, forbidden()
	}
	return ThreadAccess{Owner: th.owner, CanControl: caller.CanControl(th.owner)}, nil
}

// BumpWriterEpoch makes the caller the Thread's driver: it adds one to
// writer_epoch and returns the new value. Every later write of that driver
// compares this value, so a driver that was taken over finds out at its next
// write. The OS writer lock is taken before this call; this call does not take it.
func (s *Store) BumpWriterEpoch(ctx context.Context, threadID string) (int64, error) {
	var epoch int64
	err := s.write(ctx, func(tx *sql.Tx, _ time.Time) error {
		err := tx.QueryRowContext(ctx, `UPDATE threads SET writer_epoch=writer_epoch+1 WHERE thread_id=? RETURNING writer_epoch`, threadID).Scan(&epoch)
		if errors.Is(err, sql.ErrNoRows) {
			return forbidden()
		}
		if err != nil {
			return fmt.Errorf("sqlite: bump writer epoch: %w", mapSQLiteError(err))
		}
		return nil
	})
	return epoch, err
}

const sessionColumns = `s.session_id, t.thread_id, s.workspace_path, t.binding_json, s.policy_ref, s.execution_mode,
	t.context_revision, t.control_revision, t.active_run_id, s.principal`

type rowScanner interface{ Scan(dest ...any) error }

func scanSession(row rowScanner) (protocol.SessionInfo, string, error) {
	var info protocol.SessionInfo
	var bindingJSON, owner string
	var active sql.NullString
	if err := row.Scan(&info.SessionID, &info.ThreadID, &info.WorkspacePath, &bindingJSON, &info.PolicyRef, &info.ExecutionMode,
		&info.ContextRevision, &info.ControlRevision, &active, &owner); err != nil {
		return info, "", err
	}
	b, err := protocol.Decode[protocol.Binding]([]byte(bindingJSON))
	if err != nil {
		return info, "", protocol.NewError(protocol.CodeIntegrityBlocked, "a stored binding does not satisfy the contract").Wrap(err)
	}
	info.Binding, info.ActiveRunID = b, nullStr(active)
	return info, owner, nil
}

// GetSession (session/get) returns the current snapshot of a Thread the caller can read.
func (s *Store) GetSession(ctx context.Context, caller intake.Caller, threadID string) (protocol.SessionInfo, error) {
	info, owner, err := scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM threads t JOIN sessions s ON s.session_id=t.session_id WHERE t.thread_id=?`, threadID))
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.SessionInfo{}, forbidden()
	}
	if err != nil {
		return protocol.SessionInfo{}, wrapRead("read session", err)
	}
	if !caller.CanRead(owner) {
		return protocol.SessionInfo{}, forbidden()
	}
	return info, nil
}

// wrapRead keeps a typed error and adds context to anything else.
func wrapRead(what string, err error) error {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return err
	}
	return fmt.Errorf("sqlite: %s: %w", what, mapSQLiteError(err))
}

const (
	listBatch         = 200
	cursorDomain      = "rencrow-session-cursor/v1\x00"
	maxListPageLength = 100
)

func cursorTag(caller intake.Caller) string {
	sum := sha256.Sum256([]byte(cursorDomain + caller.ProfileDigest))
	return hex.EncodeToString(sum[:8])
}

func encodeCursor(caller intake.Caller, threadID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(threadID + "|" + cursorTag(caller)))
}

func decodeCursor(caller intake.Caller, cursor string) (string, error) {
	bad := protocol.NewError(protocol.CodeInvalidParams, "cursor is not valid for this caller")
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", bad
	}
	id, tag, ok := strings.Cut(string(raw), "|")
	if !ok || tag != cursorTag(caller) {
		return "", bad
	}
	if _, err := identity.ParseThreadID(id); err != nil {
		return "", bad
	}
	return id, nil
}

// ListSessions (session/list) returns the Threads the caller can read, ordered by
// thread ID (which is creation order). The cursor is opaque and bound to the
// caller profile that received it. A page has a next cursor only if another
// readable Thread follows, so unreadable Threads are neither shown nor hinted at.
func (s *Store) ListSessions(ctx context.Context, caller intake.Caller, cursor *string, limit int) (protocol.SessionListResult, error) {
	if limit < 1 || limit > maxListPageLength {
		return protocol.SessionListResult{}, protocol.NewError(protocol.CodeInvalidParams, "limit must be 1 to %d", maxListPageLength)
	}
	after := ""
	if cursor != nil {
		var err error
		if after, err = decodeCursor(caller, *cursor); err != nil {
			return protocol.SessionListResult{}, err
		}
	}
	res := protocol.SessionListResult{Sessions: []protocol.SessionInfo{}}
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT `+sessionColumns+` FROM threads t JOIN sessions s ON s.session_id=t.session_id
			WHERE t.thread_id>? ORDER BY t.thread_id LIMIT ?`, after, listBatch)
		if err != nil {
			return protocol.SessionListResult{}, wrapRead("list sessions", err)
		}
		n := 0
		more := false
		for rows.Next() {
			info, owner, err := scanSession(rows)
			if err != nil {
				_ = rows.Close()
				return protocol.SessionListResult{}, wrapRead("list sessions", err)
			}
			n++
			after = info.ThreadID
			if !caller.CanRead(owner) {
				continue
			}
			if len(res.Sessions) == limit {
				more = true
				break
			}
			res.Sessions = append(res.Sessions, info)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return protocol.SessionListResult{}, wrapRead("list sessions", err)
		}
		if more {
			next := encodeCursor(caller, res.Sessions[len(res.Sessions)-1].ThreadID)
			res.NextCursor = &next
			return res, nil
		}
		if n < listBatch {
			return res, nil
		}
	}
}

// GetReceipt (receipt/get) returns the stored record of an operation. A receipt
// belongs to the principal whose key made it: another principal, and a receipt
// that does not exist, both get the same FORBIDDEN.
func (s *Store) GetReceipt(ctx context.Context, caller intake.Caller, receiptID string) (protocol.ReceiptRecord, error) {
	var op, stage string
	var result, errJSON sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT operation, stage, result_json, error_json FROM receipts WHERE receipt_id=? AND principal=?`,
		receiptID, caller.Principal).Scan(&op, &stage, &result, &errJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.ReceiptRecord{}, forbidden()
	}
	if err != nil {
		return protocol.ReceiptRecord{}, wrapRead("read receipt", err)
	}
	rec := protocol.ReceiptRecord{ReceiptID: receiptID, Operation: op, Stage: stage}
	if result.Valid {
		p, err := protocol.Decode[protocol.ReceiptPayload]([]byte(result.String))
		if err != nil {
			return protocol.ReceiptRecord{}, protocol.NewError(protocol.CodeIntegrityBlocked, "the stored receipt is not valid").Wrap(err)
		}
		rec.Result = &p
	}
	if errJSON.Valid {
		info, err := protocol.Decode[protocol.ErrorInfo]([]byte(errJSON.String))
		if err != nil {
			return protocol.ReceiptRecord{}, protocol.NewError(protocol.CodeIntegrityBlocked, "the stored receipt error is not valid").Wrap(err)
		}
		rec.Error = &info
	}
	return rec, nil
}

// GetRun (run/get) returns the current phase of a Run, or its determined result
// once it is terminal. Both a Run of a Thread the caller cannot read and a Run
// that does not exist give FORBIDDEN.
func (s *Store) GetRun(ctx context.Context, caller intake.Caller, runID string) (protocol.RunInfo, error) {
	var info protocol.RunInfo
	var owner, limitsJSON string
	var result sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT r.run_id, r.task_id, t.thread_id, r.phase, r.result_json, r.limits_json, r.deadline_at,
		r.recovery_policy_revision, r.generation_attempts_used, r.generation_attempts_unknown, th.context_revision, th.control_revision, s.principal,
		COALESCE((SELECT MAX(e.event_seq) FROM events e WHERE e.run_id=r.run_id), 0)
		FROM runs r JOIN tasks t ON t.task_id=r.task_id JOIN threads th ON th.thread_id=t.thread_id JOIN sessions s ON s.session_id=th.session_id
		WHERE r.run_id=?`, runID).Scan(&info.RunID, &info.TaskID, &info.ThreadID, &info.Phase, &result, &limitsJSON, &info.DeadlineAt,
		&info.RecoveryPolicyRevision, &info.GenerationAttemptsUsed, &info.GenerationAttemptsUnknown, &info.ContextRevision, &info.ControlRevision, &owner, &info.LastEventSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.RunInfo{}, forbidden()
	}
	if err != nil {
		return protocol.RunInfo{}, wrapRead("read run", err)
	}
	if !caller.CanRead(owner) {
		return protocol.RunInfo{}, forbidden()
	}
	if info.EffectiveLimits, err = protocol.Decode[protocol.Limits]([]byte(limitsJSON)); err != nil {
		return protocol.RunInfo{}, protocol.NewError(protocol.CodeIntegrityBlocked, "a stored run does not satisfy the contract").Wrap(err)
	}
	info.Terminal = info.Phase == "Terminal"
	if info.Terminal {
		if !result.Valid {
			return protocol.RunInfo{}, protocol.NewError(protocol.CodeIntegrityBlocked, "a terminal run has no stored result")
		}
		rr, err := protocol.Decode[protocol.RunResult]([]byte(result.String))
		if err != nil {
			return protocol.RunInfo{}, protocol.NewError(protocol.CodeIntegrityBlocked, "a stored run result does not satisfy the contract").Wrap(err)
		}
		info.Result = &rr
	}
	return info, nil
}
