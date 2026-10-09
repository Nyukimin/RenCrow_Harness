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
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolview"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The ways a Tool write is refused. Like the others they are values the kernel
// branches on.
var (
	// ErrToolIntentConflict: the same Tool call (Run, response, call ID, position) was
	// bound before with other arguments.
	ErrToolIntentConflict = errors.New("sqlite: this tool call was already bound with other arguments")
	// ErrToolCallLimit: the response holds no Tool call, or more than the Run's limit.
	ErrToolCallLimit = errors.New("sqlite: the number of tool calls is not within the run's limit")
	// ErrPolicyChanged: the Thread's policy revision is no longer the one the Run froze.
	ErrPolicyChanged = errors.New("sqlite: the policy revision changed")
	// ErrAttemptState: the Attempt is not in the state this write needs.
	ErrAttemptState = errors.New("sqlite: the attempt is not in the state this write needs")
)

// Attempt and Action states of a Tool call.
const (
	ToolPrepared      = "prepared"
	ToolDispatched    = "dispatch_started"
	ToolRunning       = "running"
	ToolCompleted     = "completed"
	ToolFailed        = "failed"
	ToolCancelled     = "cancelled"
	ToolUnknown       = "unknown"
	toolActionRunning = "in_flight"
	toolActionRejects = "rejected"
)

// Purposes of the Evidence a Tool call writes.
const (
	PurposeToolResult = "tool_result"
	PurposeToolStdout = "tool_stdout"
	PurposeToolStderr = "tool_stderr"
	purposeToolCalls  = "tool_calls"
)

// Reasons a call is closed without having run (the text the model reads).
var closeReasons = map[string]string{
	"earlier_call_failed": "an earlier call of the same response did not succeed, so this call was not run",
	"run_ended":           "the run ended before this call was run",
	"cancelled":           "the run was cancelled before this call was run",
	"deadline":            "the run's deadline passed before this call was run",
	"policy_changed":      "the policy changed before this call was run",
	"driver_stopped":      "the run was stopped before this call was run",
	"hook_denied":         "a host hook did not allow this call, so it was not run",
	"hook_failed":         "a host hook did not answer in time, so this call was not run",
}

// ToolRejection is a call the policy refused before anything was attempted, with the
// answer the model is given.
type ToolRejection struct {
	Code string
	// ResultText is the rendered view (toolview) of the refusal.
	ResultText       string
	ResultEvidenceID string
	MessageID        string
}

// ToolCall is one call of a response to bind. The IDs are chosen by the caller, so the
// result a rejection is rendered with can name them.
type ToolCall struct {
	ActionID, AttemptID string
	ProviderToolCallID  string
	Ordinal             int64
	Name                string
	// ArgsBytes is the CJ1 text of the validated arguments object.
	ArgsBytes []byte
	Rejection *ToolRejection
}

// BindToolBatchInput is a whole response's Tool calls: the assistant message that
// asked for them and each call, in the response's order.
type BindToolBatchInput struct {
	// ResponseID is the Harness's ID of the accepted model response (rsp_...).
	ResponseID string
	// AssistantRecord is the stored text of the assistant message
	// (contextplan.ToolCallsRecord); AssistantMessageID names its item.
	AssistantRecord    []byte
	AssistantMessageID string
	Calls              []ToolCall
}

// BoundCall is a call as it stands in the store.
type BoundCall struct {
	ActionID, AttemptID string
	ArgsHash            string
	// State is the Attempt's state: prepared for a call that may still be dispatched,
	// failed for one the policy refused, anything else for one that was dispatched.
	State string
	// Existing is set when the call was bound before this call to BindToolBatch.
	Existing bool
}

// BoundBatch is what BindToolBatch left.
type BoundBatch struct {
	AssistantMessageID string
	Calls              []BoundCall
	Events             []protocol.Event
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func nextSequence(ctx context.Context, tx *sql.Tx, threadID string) (int64, error) {
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM items WHERE thread_id=?`, threadID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("sqlite: allocate sequence: %w", mapSQLiteError(err))
	}
	return seq, nil
}

type toolItemMeta struct {
	Kind        string `json:"kind"`
	RunID       string `json:"run_id"`
	ResponseID  string `json:"response_id"`
	ActionID    string `json:"action_id,omitempty"`
	ToolCallID  string `json:"tool_call_id,omitempty"`
	CallOrdinal *int64 `json:"call_ordinal,omitempty"`
}

// insertToolItem records an item of a Tool exchange. It is raw history, appended in the
// order it was accepted; it takes part in the context only once the batch is applied.
func insertToolItem(ctx context.Context, tx *sql.Tx, threadID, runID, messageID, historyKind, origin, evidenceID string, meta toolItemMeta) error {
	seq, err := nextSequence(ctx, tx, threadID)
	if err != nil {
		return err
	}
	m, err := canonicalJSON(meta)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO items(message_id, thread_id, run_id, sequence, history_kind, origin, evidence_id, metadata_json) VALUES(?,?,?,?,?,?,?,?)`,
		messageID, threadID, runID, seq, historyKind, origin, evidenceID, m); err != nil {
		return fmt.Errorf("sqlite: record tool item: %w", mapSQLiteError(err))
	}
	return nil
}

// BindToolBatch (the first half of F16) binds the Tool calls of one accepted response to
// Actions, once. In one transaction it checks the writer epoch, the control revision
// and that the Run is preparing an action within its deadline, records the assistant
// message that asked for the calls, and, for every call, an Action with its Attempt
// (prepared), the link from the call's key (Run, response, call ID, position) and
// action.prepared. A call the policy refused is closed in the same transaction: its
// Attempt failed, its answer sealed, its action.completed written (effect not_started).
//
// The binding is idempotent on the key: the same call again returns the same Action,
// and the same key with other arguments is ErrToolIntentConflict. Binding every call
// of the response at once means a call that is never reached still has an Action to be
// closed, instead of becoming a new one when someone resumes the Run.
func (s *Store) BindToolBatch(ctx context.Context, f Fence, in BindToolBatchInput) (BoundBatch, error) {
	var out BoundBatch
	err := s.fencedWrite(ctx, f, true, func(tx *sql.Tx, now time.Time, th threadRow, run runRow, start startRecord) error {
		if run.phase != "PreparingAction" {
			return ErrPhaseConflict
		}
		if !now.Before(run.deadline) {
			return ErrDeadlinePassed
		}
		if n := int64(len(in.Calls)); n < 1 || n > run.limits.MaxToolCallsPerStep {
			return ErrToolCallLimit
		}
		var policyRevision string
		if err := tx.QueryRowContext(ctx, `SELECT policy_revision FROM threads WHERE thread_id=?`, f.ThreadID).Scan(&policyRevision); err != nil {
			return fmt.Errorf("sqlite: read policy revision: %w", mapSQLiteError(err))
		}
		seq := &eventSeq{base: th.eventSeq}
		at := protocol.FormatTimestamp(now)

		// The assistant message of the response, once.
		var msgID string
		err := tx.QueryRowContext(ctx, `SELECT message_id FROM items WHERE run_id=? AND json_extract(metadata_json,'$.kind')='tool_calls' AND json_extract(metadata_json,'$.response_id')=?`,
			f.RunID, in.ResponseID).Scan(&msgID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			ev, werr := writeEvidenceSpec(ctx, tx, evidenceSpec{Principal: th.owner, Meta: evidenceMeta{Purpose: purposeToolCalls}, RunID: protocol.Str(f.RunID),
				MediaType: "text/plain; charset=utf-8", Text: true}, in.AssistantRecord)
			if werr != nil {
				return werr
			}
			msgID = in.AssistantMessageID
			if err := insertToolItem(ctx, tx, f.ThreadID, f.RunID, msgID, "Work", "agent", ev,
				toolItemMeta{Kind: "tool_calls", RunID: f.RunID, ResponseID: in.ResponseID}); err != nil {
				return err
			}
		case err != nil:
			return fmt.Errorf("sqlite: look up tool message: %w", mapSQLiteError(err))
		}
		out.AssistantMessageID = msgID

		for _, c := range in.Calls {
			argsHash := sha256Hex(c.ArgsBytes)
			var actionID, attemptID, prevHash, state string
			err := tx.QueryRowContext(ctx, `SELECT l.action_id, l.args_hash, a.attempt_id, a.state FROM tool_links l
				JOIN actions ac ON ac.action_id=l.action_id JOIN attempts a ON a.action_id=ac.action_id AND a.ordinal=0
				WHERE l.run_id=? AND l.model_response_id=? AND l.provider_tool_call_id=? AND l.ordinal=?`,
				f.RunID, in.ResponseID, c.ProviderToolCallID, c.Ordinal).Scan(&actionID, &prevHash, &attemptID, &state)
			switch {
			case err == nil:
				if prevHash != argsHash {
					return ErrToolIntentConflict
				}
				out.Calls = append(out.Calls, BoundCall{ActionID: actionID, AttemptID: attemptID, ArgsHash: argsHash, State: state, Existing: true})
				continue
			case !errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("sqlite: look up tool call: %w", mapSQLiteError(err))
			}
			for _, stmt := range []struct {
				what string
				q    string
				args []any
			}{
				{"action", `INSERT INTO actions(action_id, run_id, kind, name, args_bytes, args_hash, status, current_attempt_id, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
					[]any{c.ActionID, f.RunID, "tool", c.Name, c.ArgsBytes, argsHash, ToolPrepared, c.AttemptID, at}},
				{"attempt", `INSERT INTO attempts(attempt_id, action_id, ordinal, state, started_at) VALUES(?,?,0,?,?)`, []any{c.AttemptID, c.ActionID, ToolPrepared, at}},
				{"link", `INSERT INTO tool_links(run_id, model_response_id, provider_tool_call_id, ordinal, action_id, args_hash) VALUES(?,?,?,?,?,?)`,
					[]any{f.RunID, in.ResponseID, c.ProviderToolCallID, c.Ordinal, c.ActionID, argsHash}},
			} {
				if _, err := tx.ExecContext(ctx, stmt.q, stmt.args...); err != nil {
					return fmt.Errorf("sqlite: bind tool call (%s): %w", stmt.what, mapSQLiteError(err))
				}
			}
			ev, err := appendEvent(ctx, tx, seq, eventRecord{common: runCommon(now, f.ThreadID, run, start, nil), payload: protocol.ActionPreparedPayload{
				ActionID: c.ActionID, AttemptID: c.AttemptID, Kind: "tool", Name: c.Name, ArgsHash: argsHash, PolicyRevision: policyRevision}})
			if err != nil {
				return err
			}
			out.Events = append(out.Events, ev)
			bound := BoundCall{ActionID: c.ActionID, AttemptID: c.AttemptID, ArgsHash: argsHash, State: ToolPrepared}
			if c.Rejection != nil {
				done, err := rejectToolCallTx(ctx, tx, now, th, f.ThreadID, run, start, seq, in.ResponseID, c)
				if err != nil {
					return err
				}
				out.Events = append(out.Events, done)
				bound.State = ToolFailed
			}
			out.Calls = append(out.Calls, bound)
		}
		return finishThread(ctx, tx, th, f.ThreadID, seq, th.contextRev, false)
	})
	return out, err
}

// rejectToolCallTx closes a call the policy refused: nothing was attempted.
func rejectToolCallTx(ctx context.Context, tx *sql.Tx, now time.Time, th threadRow, threadID string, run runRow, start startRecord, seq *eventSeq, responseID string, c ToolCall) (protocol.Event, error) {
	r := c.Rejection
	evID, msgID := r.ResultEvidenceID, r.MessageID
	if evID == "" {
		evID = identity.NewEvidenceID().String()
	}
	if msgID == "" {
		msgID = identity.NewMessageID().String()
	}
	if _, err := writeEvidenceSpec(ctx, tx, evidenceSpec{ID: evID, Principal: th.owner, Meta: evidenceMeta{Purpose: PurposeToolResult}, RunID: protocol.Str(run.runID),
		AttemptID: protocol.Str(c.AttemptID), MediaType: "text/plain; charset=utf-8", Text: true}, []byte(r.ResultText)); err != nil {
		return protocol.Event{}, err
	}
	if err := insertToolItem(ctx, tx, threadID, run.runID, msgID, "Observation", "tool", evID,
		toolItemMeta{Kind: "tool_result", RunID: run.runID, ResponseID: responseID, ActionID: c.ActionID, ToolCallID: c.ProviderToolCallID, CallOrdinal: &c.Ordinal}); err != nil {
		return protocol.Event{}, err
	}
	result, err := canonicalJSON(struct {
		EffectState string `json:"effect_state"`
		Code        string `json:"code"`
	}{"not_started", r.Code})
	if err != nil {
		return protocol.Event{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attempts SET state=?, ended_at=?, result_json=? WHERE attempt_id=? AND state=?`,
		ToolFailed, protocol.FormatTimestamp(now), result, c.AttemptID, ToolPrepared); err != nil {
		return protocol.Event{}, fmt.Errorf("sqlite: reject tool attempt: %w", mapSQLiteError(err))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE actions SET status=? WHERE action_id=?`, toolActionRejects, c.ActionID); err != nil {
		return protocol.Event{}, fmt.Errorf("sqlite: reject tool action: %w", mapSQLiteError(err))
	}
	return appendEvent(ctx, tx, seq, eventRecord{common: runCommon(now, threadID, run, start, protocol.Str(evID)), payload: protocol.ActionCompletedPayload{
		ActionID: c.ActionID, AttemptID: c.AttemptID, EffectState: "not_started", ExitCode: nil, ResultEvidenceIDs: []string{evID}, CaptureComplete: true}})
}

// StartToolInput names the Attempt to dispatch and the policy revision the dispatcher
// resolved its authority from.
type StartToolInput struct {
	ActionID, AttemptID string
	PolicyRevision      string
}

// ToolStarted is what recording a dispatch-start produced.
type ToolStarted struct {
	Events          []protocol.Event
	WriterEpoch     int64
	ControlRevision int64
}

// StartToolAttempt (the dispatch-start of F16) is the one step where a Tool call may
// become one that has started. In one BEGIN IMMEDIATE it compares the Thread row's
// writer epoch and control revision with the ones the driver holds, and the Thread's
// policy revision with the dispatcher's, and checks the Run is executing within its
// deadline; only then does it move the Attempt from prepared to dispatch_started and
// write action.dispatch_started. A cancellation recorded first changes the control
// revision, so the compare fails and nothing starts (ErrControlChanged); one recorded
// after finds the Attempt already started, which is then treated as a call that may
// have taken effect.
//
// An Attempt that is not prepared is never started: its call was dispatched or closed
// already (ErrAttemptState), and is not run a second time.
func (s *Store) StartToolAttempt(ctx context.Context, f Fence, in StartToolInput) (ToolStarted, error) {
	var out ToolStarted
	err := s.fencedWrite(ctx, f, true, func(tx *sql.Tx, now time.Time, th threadRow, run runRow, start startRecord) error {
		if run.phase != "Executing" {
			return ErrPhaseConflict
		}
		if !now.Before(run.deadline) {
			return ErrDeadlinePassed
		}
		var policyRevision, state string
		if err := tx.QueryRowContext(ctx, `SELECT policy_revision FROM threads WHERE thread_id=?`, f.ThreadID).Scan(&policyRevision); err != nil {
			return fmt.Errorf("sqlite: read policy revision: %w", mapSQLiteError(err))
		}
		if policyRevision != in.PolicyRevision {
			return ErrPolicyChanged
		}
		err := tx.QueryRowContext(ctx, `SELECT a.state FROM attempts a JOIN actions ac ON ac.action_id=a.action_id
			WHERE a.attempt_id=? AND ac.action_id=? AND ac.run_id=? AND ac.kind='tool'`, in.AttemptID, in.ActionID, f.RunID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return protocol.NewError(protocol.CodeIntegrityBlocked, "a tool attempt does not belong to this run")
		}
		if err != nil {
			return fmt.Errorf("sqlite: load tool attempt: %w", mapSQLiteError(err))
		}
		if state != ToolPrepared {
			return ErrAttemptState
		}
		if r, err := tx.ExecContext(ctx, `UPDATE attempts SET state=? WHERE attempt_id=? AND state=?`, ToolDispatched, in.AttemptID, ToolPrepared); err != nil {
			return fmt.Errorf("sqlite: start tool attempt: %w", mapSQLiteError(err))
		} else if n, _ := r.RowsAffected(); n != 1 {
			return ErrAttemptState
		}
		if _, err := tx.ExecContext(ctx, `UPDATE actions SET status=? WHERE action_id=?`, toolActionRunning, in.ActionID); err != nil {
			return fmt.Errorf("sqlite: start tool action: %w", mapSQLiteError(err))
		}
		seq := &eventSeq{base: th.eventSeq}
		ev, err := appendEvent(ctx, tx, seq, eventRecord{common: runCommon(now, f.ThreadID, run, start, nil), payload: protocol.ActionDispatchStartedPayload{
			ActionID: in.ActionID, AttemptID: in.AttemptID, WriterEpoch: th.writerEpoch, ControlRevision: th.controlRev}})
		if err != nil {
			return err
		}
		out = ToolStarted{Events: []protocol.Event{ev}, WriterEpoch: th.writerEpoch, ControlRevision: th.controlRev}
		return finishThread(ctx, tx, th, f.ThreadID, seq, th.contextRev, false)
	})
	return out, err
}

// RecordProcessStart notes the identity of the process an Attempt started: the host
// incarnation, and the token that holds its PID, start time and nonce. The Attempt moves
// from dispatch_started to running. Until this commits a crash leaves a dispatched
// Attempt with no identity, which cannot be matched to any process and stays unknown.
func (s *Store) RecordProcessStart(ctx context.Context, f Fence, attemptID, hostIncarnation, token string) error {
	return s.fencedWrite(ctx, f, false, func(tx *sql.Tx, _ time.Time, _ threadRow, _ runRow, _ startRecord) error {
		r, err := tx.ExecContext(ctx, `UPDATE attempts SET state=?, host_incarnation=?, process_token=?
			WHERE attempt_id=? AND state=? AND action_id IN (SELECT action_id FROM actions WHERE run_id=? AND kind IN ('tool','verification'))`,
			ToolRunning, hostIncarnation, token, attemptID, ToolDispatched, f.RunID)
		if err != nil {
			return fmt.Errorf("sqlite: record process start: %w", mapSQLiteError(err))
		}
		if n, _ := r.RowsAffected(); n != 1 {
			return ErrAttemptState
		}
		return nil
	})
}

// CaptureChunk is one bounded piece of a process's output. The first piece of an
// Evidence (ordinal 0) creates it as building; the rest extend it.
type CaptureChunk struct {
	EvidenceID string
	AttemptID  string
	Purpose    string
	Ordinal    int64
	ByteStart  int64
	Data       []byte
}

// AppendCaptureChunk stores one chunk of a capture under the writer fence. The Run's
// DB lock is held only for this write, never while the process runs. An Evidence that
// is never sealed (the driver died) stays building: an incomplete capture that is kept
// and never presented as complete.
func (s *Store) AppendCaptureChunk(ctx context.Context, f Fence, c CaptureChunk) error {
	return s.fencedWrite(ctx, f, false, func(tx *sql.Tx, _ time.Time, th threadRow, _ runRow, _ startRecord) error {
		var actionKind string
		if err := tx.QueryRowContext(ctx, `SELECT ac.kind FROM attempts a JOIN actions ac ON ac.action_id=a.action_id
			WHERE a.attempt_id=? AND ac.run_id=?`, c.AttemptID, f.RunID).Scan(&actionKind); errors.Is(err, sql.ErrNoRows) {
			return protocol.NewError(protocol.CodeIntegrityBlocked, "a process capture does not belong to this run")
		} else if err != nil {
			return fmt.Errorf("sqlite: find process capture action: %w", mapSQLiteError(err))
		}
		purposeKind := ""
		switch c.Purpose {
		case PurposeToolStdout, PurposeToolStderr:
			purposeKind = "tool"
		case PurposeVerificationStdout, PurposeVerificationStderr:
			purposeKind = "verification"
		}
		if purposeKind == "" || purposeKind != actionKind {
			return protocol.NewError(protocol.CodeInternal, "a capture chunk has a purpose that is not a capture")
		}
		if c.Ordinal == 0 {
			meta, err := canonicalJSON(evidenceMeta{Purpose: c.Purpose})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO evidence(evidence_id, principal, owner, run_id, attempt_id, state, media_type, metadata_json)
				VALUES(?,?,?,?,?,'building','application/octet-stream',?)`, c.EvidenceID, th.owner, storeOwner, f.RunID, c.AttemptID, meta); err != nil {
				return fmt.Errorf("sqlite: begin capture: %w", mapSQLiteError(err))
			}
		} else {
			var state string
			if err := tx.QueryRowContext(ctx, `SELECT state FROM evidence WHERE evidence_id=? AND run_id=? AND attempt_id=?`, c.EvidenceID, f.RunID, c.AttemptID).Scan(&state); err != nil {
				return fmt.Errorf("sqlite: find capture: %w", mapSQLiteError(err))
			}
			if state != "building" {
				return protocol.NewError(protocol.CodeIntegrityBlocked, "a capture was extended after it was sealed")
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO evidence_chunks(evidence_id, ordinal, byte_start, data) VALUES(?,?,?,?)`, c.EvidenceID, c.Ordinal, c.ByteStart, c.Data); err != nil {
			return fmt.Errorf("sqlite: store capture chunk: %w", mapSQLiteError(err))
		}
		return nil
	})
}

// utf8Stream checks that a sequence of chunks, joined, is valid UTF-8.
type utf8Stream struct {
	carry []byte
	bad   bool
}

func (u *utf8Stream) write(p []byte) {
	if u.bad {
		return
	}
	b := append(u.carry, p...)
	n, tail := len(b), 0
	for i := 1; i <= 3 && i <= n; i++ {
		if utf8.RuneStart(b[n-i]) {
			if !utf8.FullRune(b[n-i:]) {
				tail = i
			}
			break
		}
	}
	if !utf8.Valid(b[:n-tail]) {
		u.bad = true
	}
	u.carry = append([]byte(nil), b[n-tail:]...)
}

func (u *utf8Stream) valid() bool { return !u.bad && len(u.carry) == 0 }

// sealBuildingTx seals an Evidence that was being captured: the chunks must be
// contiguous from byte 0; their size and SHA-256 become the Evidence's, and it is a
// text projection only if the whole is valid UTF-8 and the capture is complete.
func sealBuildingTx(ctx context.Context, tx *sql.Tx, evidenceID string, complete, textIfValid bool) error {
	rows, err := tx.QueryContext(ctx, `SELECT ordinal, byte_start, data FROM evidence_chunks WHERE evidence_id=? ORDER BY ordinal`, evidenceID)
	if err != nil {
		return fmt.Errorf("sqlite: read capture: %w", mapSQLiteError(err))
	}
	h := sha256.New()
	var total int64
	var u utf8Stream
	var next int64
	for i := int64(0); rows.Next(); i++ {
		var ord, startAt int64
		var data []byte
		if err := rows.Scan(&ord, &startAt, &data); err != nil {
			_ = rows.Close()
			return err
		}
		if ord != i || startAt != next {
			_ = rows.Close()
			return protocol.NewError(protocol.CodeIntegrityBlocked, "a capture's chunks are not contiguous")
		}
		h.Write(data)
		u.write(data)
		next += int64(len(data))
	}
	total = next
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return fmt.Errorf("sqlite: read capture: %w", mapSQLiteError(err))
	}
	hash := hex.EncodeToString(h.Sum(nil))
	media, projVersion, projHash := "application/octet-stream", any(nil), any(nil)
	if textIfValid && complete && u.valid() {
		media, projVersion, projHash = "text/plain; charset=utf-8", "text/v1", hash
	}
	capture := 0
	if complete {
		capture = 1
	}
	r, err := tx.ExecContext(ctx, `UPDATE evidence SET state='sealed', capture_complete=?, total_bytes=?, raw_hash=?, media_type=?, projection_version=?, projection_hash=?
		WHERE evidence_id=? AND state='building'`, capture, total, hash, media, projVersion, projHash, evidenceID)
	if err != nil {
		return fmt.Errorf("sqlite: seal capture: %w", mapSQLiteError(err))
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return protocol.NewError(protocol.CodeIntegrityBlocked, "a capture was sealed twice")
	}
	return nil
}

// SealCapture says how one output Evidence of a call is sealed: Created is false when
// the process wrote nothing and no chunk was ever stored (an empty Evidence is made).
type SealCapture struct {
	EvidenceID string
	Created    bool
	Purpose    string
}

// CompleteToolInput is the end of one dispatched Attempt.
type CompleteToolInput struct {
	ActionID, AttemptID string
	// EffectState is completed, failed, cancelled or unknown.
	EffectState     string
	ExitCode        *int64
	CaptureComplete bool
	Captures        []SealCapture
	// ResultText is the rendered view (toolview) of the call, and ResultEvidenceID and
	// MessageID name what it is stored as.
	ResultText       string
	ResultEvidenceID string
	MessageID        string
	ResponseID       string
	ProviderCallID   string
	CallOrdinal      int64
	// ResultJSON is the Attempt's own result record (CJ1), small and without output.
	ResultJSON string
}

// CompletedTool is what recording the end produced.
type CompletedTool struct {
	Events            []protocol.Event
	ResultEvidenceIDs []string
}

func attemptStateOf(effect string) (string, error) {
	switch effect {
	case "completed":
		return ToolCompleted, nil
	case "failed":
		return ToolFailed, nil
	case "cancelled":
		return ToolCancelled, nil
	case "unknown":
		return ToolUnknown, nil
	}
	return "", protocol.NewError(protocol.CodeInternal, "a tool attempt ends in a state that is not an end")
}

// CompleteToolAttempt (the last step of F16) records how a dispatched Attempt ended,
// in one transaction: the output Evidence sealed, the answer sealed as Evidence and
// kept as an item, the Attempt and its Action ended, and action.completed. The writer
// epoch is checked and the control revision is not: a result that arrives after a
// cancellation is still recorded. An Attempt ends once.
func (s *Store) CompleteToolAttempt(ctx context.Context, f Fence, in CompleteToolInput) (CompletedTool, error) {
	var out CompletedTool
	state, err := attemptStateOf(in.EffectState)
	if err != nil {
		return out, err
	}
	err = s.fencedWrite(ctx, f, false, func(tx *sql.Tx, now time.Time, th threadRow, run runRow, start startRecord) error {
		var cur string
		if err := tx.QueryRowContext(ctx, `SELECT a.state FROM attempts a JOIN actions ac ON ac.action_id=a.action_id
			WHERE a.attempt_id=? AND ac.action_id=? AND ac.run_id=? AND ac.kind='tool'`, in.AttemptID, in.ActionID, f.RunID).Scan(&cur); errors.Is(err, sql.ErrNoRows) {
			return protocol.NewError(protocol.CodeIntegrityBlocked, "a tool attempt does not belong to this run")
		} else if err != nil {
			return fmt.Errorf("sqlite: load tool attempt: %w", mapSQLiteError(err))
		}
		if cur != ToolDispatched && cur != ToolRunning {
			return ErrAttemptEnded
		}
		var evidenceIDs []string
		for _, c := range in.Captures {
			if c.Created {
				if err := sealBuildingTx(ctx, tx, c.EvidenceID, in.CaptureComplete, true); err != nil {
					return err
				}
			} else {
				if _, err := writeEvidenceSpec(ctx, tx, evidenceSpec{ID: c.EvidenceID, Principal: th.owner, Meta: evidenceMeta{Purpose: c.Purpose}, RunID: protocol.Str(f.RunID),
					AttemptID: protocol.Str(in.AttemptID), MediaType: "text/plain; charset=utf-8", Text: true}, nil); err != nil {
					return err
				}
			}
			evidenceIDs = append(evidenceIDs, c.EvidenceID)
		}
		resultID := in.ResultEvidenceID
		if resultID == "" {
			resultID = identity.NewEvidenceID().String()
		}
		if _, err := writeEvidenceSpec(ctx, tx, evidenceSpec{ID: resultID, Principal: th.owner, Meta: evidenceMeta{Purpose: PurposeToolResult}, RunID: protocol.Str(f.RunID),
			AttemptID: protocol.Str(in.AttemptID), MediaType: "text/plain; charset=utf-8", Text: true}, []byte(in.ResultText)); err != nil {
			return err
		}
		msgID := in.MessageID
		if msgID == "" {
			msgID = identity.NewMessageID().String()
		}
		if err := insertToolItem(ctx, tx, f.ThreadID, f.RunID, msgID, "Observation", "tool", resultID,
			toolItemMeta{Kind: "tool_result", RunID: f.RunID, ResponseID: in.ResponseID, ActionID: in.ActionID, ToolCallID: in.ProviderCallID, CallOrdinal: &in.CallOrdinal}); err != nil {
			return err
		}
		if r, err := tx.ExecContext(ctx, `UPDATE attempts SET state=?, ended_at=?, result_json=? WHERE attempt_id=? AND state IN (?,?)`,
			state, protocol.FormatTimestamp(now), in.ResultJSON, in.AttemptID, ToolDispatched, ToolRunning); err != nil {
			return fmt.Errorf("sqlite: end tool attempt: %w", mapSQLiteError(err))
		} else if n, _ := r.RowsAffected(); n != 1 {
			return ErrAttemptEnded
		}
		if _, err := tx.ExecContext(ctx, `UPDATE actions SET status=? WHERE action_id=?`, state, in.ActionID); err != nil {
			return fmt.Errorf("sqlite: end tool action: %w", mapSQLiteError(err))
		}
		seq := &eventSeq{base: th.eventSeq}
		ids := append([]string{resultID}, evidenceIDs...)
		ev, err := appendEvent(ctx, tx, seq, eventRecord{common: runCommon(now, f.ThreadID, run, start, protocol.Str(resultID)), payload: protocol.ActionCompletedPayload{
			ActionID: in.ActionID, AttemptID: in.AttemptID, EffectState: in.EffectState, ExitCode: in.ExitCode, ResultEvidenceIDs: ids, CaptureComplete: in.CaptureComplete}})
		if err != nil {
			return err
		}
		out = CompletedTool{Events: []protocol.Event{ev}, ResultEvidenceIDs: ids}
		return finishThread(ctx, tx, th, f.ThreadID, seq, th.contextRev, false)
	})
	return out, err
}

// RunCaptureBytes is how many output bytes the Run's finished calls captured: what the
// Run's capture limit has been used for.
func (s *Store) RunCaptureBytes(ctx context.Context, runID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(total_bytes),0) FROM evidence WHERE run_id=? AND state='sealed'
		AND json_extract(metadata_json,'$.purpose') IN (?,?,?,?)`, runID, PurposeToolStdout, PurposeToolStderr, PurposeVerificationStdout, PurposeVerificationStderr).Scan(&n)
	if err != nil {
		return 0, wrapRead("count captured bytes", err)
	}
	return n, nil
}

// batchCall is one call of a bound response as the store sees it.
type batchCall struct {
	ordinal           int64
	providerID        string
	actionID          string
	attemptID         string
	state             string
	name              string
	resultItemPresent bool
}

func loadBatchCalls(ctx context.Context, tx *sql.Tx, runID, responseID string) ([]batchCall, error) {
	rows, err := tx.QueryContext(ctx, `SELECT l.ordinal, l.provider_tool_call_id, l.action_id, a.attempt_id, a.state, ac.name,
		EXISTS(SELECT 1 FROM items i WHERE i.run_id=l.run_id AND json_extract(i.metadata_json,'$.kind')='tool_result' AND json_extract(i.metadata_json,'$.action_id')=l.action_id)
		FROM tool_links l JOIN actions ac ON ac.action_id=l.action_id JOIN attempts a ON a.action_id=ac.action_id AND a.ordinal=0
		WHERE l.run_id=? AND l.model_response_id=? ORDER BY l.ordinal`, runID, responseID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: load tool batch: %w", mapSQLiteError(err))
	}
	defer rows.Close()
	var out []batchCall
	for rows.Next() {
		var c batchCall
		if err := rows.Scan(&c.ordinal, &c.providerID, &c.actionID, &c.attemptID, &c.state, &c.name, &c.resultItemPresent); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// closeCallTx ends an Attempt that never completed and records its answer. A prepared
// Attempt was never dispatched: it ends cancelled with its effect not_started. A
// dispatched one may have taken effect and ends unknown, with whatever output was
// being captured sealed as an incomplete capture; reconciled is what is known of its
// process (the verdict of the host check), kept in the Attempt's record.
func closeCallTx(ctx context.Context, tx *sql.Tx, now time.Time, th threadRow, threadID string, run runRow, start startRecord, seq *eventSeq, responseID string, c batchCall, reason, reconciled string) (protocol.Event, bool, error) {
	var (
		effect, attemptState, code string
		view                       toolview.View
		captured                   []string
	)
	if c.state == ToolPrepared {
		msg, ok := closeReasons[reason]
		if !ok {
			msg = closeReasons["run_ended"]
		}
		effect, attemptState, code = "not_started", ToolCancelled, toolview.CodeNotExecuted
		view = toolview.NotExecuted(c.name, c.actionID, "This call was not run: "+msg+".")
	} else {
		effect, attemptState, code = "unknown", ToolUnknown, toolview.CodeEffectUnknown
		rows, err := tx.QueryContext(ctx, `SELECT evidence_id FROM evidence WHERE run_id=? AND attempt_id=? AND state='building' ORDER BY evidence_id`, run.runID, c.attemptID)
		if err != nil {
			return protocol.Event{}, false, fmt.Errorf("sqlite: find unsealed capture: %w", mapSQLiteError(err))
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return protocol.Event{}, false, err
			}
			captured = append(captured, id)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return protocol.Event{}, false, err
		}
		for _, id := range captured {
			if err := sealBuildingTx(ctx, tx, id, false, false); err != nil {
				return protocol.Event{}, false, err
			}
		}
		view = toolview.Unknown(c.name, c.actionID, "The outcome of this call is not known: the process that was running it ended before its result was recorded. It was not run again.", captured)
	}
	text, err := toolview.Encode(view)
	if err != nil {
		return protocol.Event{}, false, protocol.NewError(protocol.CodeInternal, "a closed call's answer cannot be built").Wrap(err)
	}
	evID, msgID := identity.NewEvidenceID().String(), identity.NewMessageID().String()
	if _, err := writeEvidenceSpec(ctx, tx, evidenceSpec{ID: evID, Principal: th.owner, Meta: evidenceMeta{Purpose: PurposeToolResult}, RunID: protocol.Str(run.runID),
		AttemptID: protocol.Str(c.attemptID), MediaType: "text/plain; charset=utf-8", Text: true}, text); err != nil {
		return protocol.Event{}, false, err
	}
	if !c.resultItemPresent {
		if err := insertToolItem(ctx, tx, threadID, run.runID, msgID, "Observation", "tool", evID,
			toolItemMeta{Kind: "tool_result", RunID: run.runID, ResponseID: responseID, ActionID: c.actionID, ToolCallID: c.providerID, CallOrdinal: &c.ordinal}); err != nil {
			return protocol.Event{}, false, err
		}
	}
	result, err := canonicalJSON(struct {
		EffectState string `json:"effect_state"`
		Code        string `json:"code"`
		Reconcile   string `json:"reconcile,omitempty"`
	}{effect, code, reconciled})
	if err != nil {
		return protocol.Event{}, false, err
	}
	if r, err := tx.ExecContext(ctx, `UPDATE attempts SET state=?, ended_at=?, result_json=? WHERE attempt_id=? AND state IN (?,?,?)`,
		attemptState, protocol.FormatTimestamp(now), result, c.attemptID, ToolPrepared, ToolDispatched, ToolRunning); err != nil {
		return protocol.Event{}, false, fmt.Errorf("sqlite: close tool attempt: %w", mapSQLiteError(err))
	} else if n, _ := r.RowsAffected(); n != 1 {
		return protocol.Event{}, false, ErrAttemptEnded
	}
	if _, err := tx.ExecContext(ctx, `UPDATE actions SET status=? WHERE action_id=?`, attemptState, c.actionID); err != nil {
		return protocol.Event{}, false, fmt.Errorf("sqlite: close tool action: %w", mapSQLiteError(err))
	}
	ev, err := appendEvent(ctx, tx, seq, eventRecord{common: runCommon(now, threadID, run, start, protocol.Str(evID)), payload: protocol.ActionCompletedPayload{
		ActionID: c.actionID, AttemptID: c.attemptID, EffectState: effect, ExitCode: nil, ResultEvidenceIDs: append([]string{evID}, captured...), CaptureComplete: effect == "not_started"}})
	return ev, effect == "unknown", err
}

// applyBatchTx finishes one response's Tool exchange: every call that is not over is
// closed (see closeCallTx), and the assistant message and the answers, in the order of
// the calls, are applied to the Thread's context together, each moving the context
// revision by one. A batch that is already applied is left alone. It reports the
// calls whose outcome is unknown.
func applyBatchTx(ctx context.Context, tx *sql.Tx, now time.Time, th threadRow, threadID string, run runRow, start startRecord, seq *eventSeq, ctxRev *int64,
	responseID, reason string, reconciled map[string]string) ([]protocol.Event, []string, error) {
	var assistantID string
	if err := tx.QueryRowContext(ctx, `SELECT message_id FROM items WHERE run_id=? AND json_extract(metadata_json,'$.kind')='tool_calls' AND json_extract(metadata_json,'$.response_id')=?`,
		run.runID, responseID).Scan(&assistantID); errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil // nothing was bound for this response
	} else if err != nil {
		return nil, nil, fmt.Errorf("sqlite: find tool message: %w", mapSQLiteError(err))
	}
	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM context_entries WHERE thread_id=? AND message_id=?`, threadID, assistantID).Scan(&applied); err != nil {
		return nil, nil, fmt.Errorf("sqlite: check applied tool message: %w", mapSQLiteError(err))
	}
	if applied != 0 {
		return nil, nil, nil
	}
	calls, err := loadBatchCalls(ctx, tx, run.runID, responseID)
	if err != nil {
		return nil, nil, err
	}
	var events []protocol.Event
	var unknown []string
	for i := range calls {
		c := &calls[i]
		switch c.state {
		case ToolPrepared, ToolDispatched, ToolRunning:
			ev, isUnknown, err := closeCallTx(ctx, tx, now, th, threadID, run, start, seq, responseID, *c, reason, reconciled[c.attemptID])
			if err != nil {
				return nil, nil, err
			}
			events = append(events, ev)
			if isUnknown {
				unknown = append(unknown, c.actionID)
			}
		case ToolUnknown:
			unknown = append(unknown, c.actionID)
		}
	}
	// The answers, now every call has one.
	apply := func(messageID string) error {
		*ctxRev++
		if err := insertContextEntry(ctx, tx, threadID, messageID, *ctxRev); err != nil {
			return fmt.Errorf("sqlite: apply tool exchange: %w", err)
		}
		return nil
	}
	if err := apply(assistantID); err != nil {
		return nil, nil, err
	}
	for _, c := range calls {
		var msgID string
		if err := tx.QueryRowContext(ctx, `SELECT message_id FROM items WHERE run_id=? AND json_extract(metadata_json,'$.kind')='tool_result' AND json_extract(metadata_json,'$.action_id')=?
			ORDER BY sequence DESC LIMIT 1`, run.runID, c.actionID).Scan(&msgID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil, protocol.NewError(protocol.CodeIntegrityBlocked, "a tool call has no answer to apply")
			}
			return nil, nil, fmt.Errorf("sqlite: find tool answer: %w", mapSQLiteError(err))
		}
		if err := apply(msgID); err != nil {
			return nil, nil, err
		}
	}
	return events, unknown, nil
}

// ObservationResult is what applying a batch produced.
type ObservationResult struct {
	Events []protocol.Event
	// Unknown are the calls whose outcome is unknown.
	Unknown []string
}

// PersistToolObservation applies the Tool exchange of one response to the Thread's
// context, in one transaction (see applyBatchTx). reason says why an unreached call was
// not run (a key of closeReasons). Applying twice changes nothing.
func (s *Store) PersistToolObservation(ctx context.Context, f Fence, responseID, reason string) (ObservationResult, error) {
	var out ObservationResult
	err := s.fencedWrite(ctx, f, false, func(tx *sql.Tx, now time.Time, th threadRow, run runRow, start startRecord) error {
		seq := &eventSeq{base: th.eventSeq}
		ctxRev := th.contextRev
		events, unknown, err := applyBatchTx(ctx, tx, now, th, f.ThreadID, run, start, seq, &ctxRev, responseID, reason, nil)
		if err != nil {
			return err
		}
		out = ObservationResult{Events: events, Unknown: unknown}
		if len(events) == 0 && ctxRev == th.contextRev {
			return nil
		}
		return finishThread(ctx, tx, th, f.ThreadID, seq, ctxRev, false)
	})
	return out, err
}

// ToolAttemptRef is a dispatched Tool Attempt that has no recorded end.
type ToolAttemptRef struct {
	RunID, ActionID, AttemptID string
	Tool                       string
	State                      string
	HostIncarnation            string
	ProcessToken               string
}

// StaleSelect says which Runs a settling covers.
type StaleSelect int

const (
	// SelectOlderEpochs: the Runs an earlier writer epoch left running.
	SelectOlderEpochs StaleSelect = iota
	// SelectExpired: the Runs of the driver's own epoch whose deadline has passed.
	SelectExpired
	// SelectCancelled: the Runs of the driver's own epoch whose stop was requested.
	SelectCancelled
)

// selectStaleRuns lists the Runs terminalizeRuns would settle, with its own selection
// rules, so the host-level check of their processes (done before the transaction, never
// inside it) looks at the same Runs. driving, when set, says which Runs have a driver
// (they are left to it).
func selectStaleRuns(ctx context.Context, q queryer, now time.Time, threadID string, epoch int64, sel StaleSelect, driving func(string) bool) ([]string, error) {
	query, args := `SELECT r.run_id FROM runs r JOIN tasks t ON t.task_id=r.task_id
		WHERE t.thread_id=? AND r.status='running' AND r.writer_epoch<? ORDER BY r.started_at, r.run_id`, []any{threadID, epoch}
	switch sel {
	case SelectExpired:
		query, args = `SELECT r.run_id FROM runs r JOIN tasks t ON t.task_id=r.task_id
			WHERE t.thread_id=? AND r.status='running' AND r.writer_epoch=? AND r.deadline_at<=? ORDER BY r.started_at, r.run_id`,
			[]any{threadID, epoch, protocol.FormatTimestamp(now)}
	case SelectCancelled:
		query, args = `SELECT r.run_id FROM runs r JOIN tasks t ON t.task_id=r.task_id
			WHERE t.thread_id=? AND r.status='running' AND r.writer_epoch=?
			AND EXISTS (SELECT 1 FROM events e WHERE e.thread_id=t.thread_id AND e.run_id=r.run_id AND e.type='control.cancel_requested')
			ORDER BY r.started_at, r.run_id`, []any{threadID, epoch}
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list stale runs: %w", mapSQLiteError(err))
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if driving != nil && driving(id) {
			continue
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// StaleToolAttempts lists the dispatched Tool Attempts of the Runs a take-over (or an
// expiry) is about to settle. The caller checks their processes against the host and
// hands the verdicts to TerminalizeStale: an OS call never happens inside a transaction.
func (s *Store) StaleToolAttempts(ctx context.Context, threadID string, epoch int64, expiredOnly bool, driving func(string) bool) ([]ToolAttemptRef, error) {
	sel := SelectOlderEpochs
	if expiredOnly {
		sel = SelectExpired
	}
	return s.StaleToolAttemptsFor(ctx, threadID, epoch, sel, driving)
}

// StaleToolAttemptsFor is StaleToolAttempts for any of the selections a settling can make.
func (s *Store) StaleToolAttemptsFor(ctx context.Context, threadID string, epoch int64, sel StaleSelect, driving func(string) bool) ([]ToolAttemptRef, error) {
	ids, err := selectStaleRuns(ctx, s.db, s.now(), threadID, epoch, sel, driving)
	if err != nil {
		return nil, err
	}
	var out []ToolAttemptRef
	for _, id := range ids {
		rows, err := s.db.QueryContext(ctx, `SELECT a.action_id, a.attempt_id, ac.name, a.state, COALESCE(a.host_incarnation,''), COALESCE(a.process_token,'')
			FROM attempts a JOIN actions ac ON ac.action_id=a.action_id WHERE ac.run_id=? AND ac.kind IN ('tool','verification') AND a.state IN (?,?) ORDER BY a.started_at, a.attempt_id`,
			id, ToolDispatched, ToolRunning)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list stale tool attempts: %w", mapSQLiteError(err))
		}
		for rows.Next() {
			r := ToolAttemptRef{RunID: id}
			if err := rows.Scan(&r.ActionID, &r.AttemptID, &r.Tool, &r.State, &r.HostIncarnation, &r.ProcessToken); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, r)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
