package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

// Event types (EVENT_CONTRACT section 1). The set is closed: a new type needs a
// schema entry, a payload struct here and a row in the contract table.
const (
	EventSessionCreated        = "session.created"
	EventInputAccepted         = "input.accepted"
	EventInputApplied          = "input.applied"
	EventTaskCreated           = "task.created"
	EventRunStarted            = "run.started"
	EventModelRequested        = "model.requested"
	EventModelCompleted        = "model.completed"
	EventActionPrepared        = "action.prepared"
	EventActionDispatchStarted = "action.dispatch_started"
	EventActionCompleted       = "action.completed"
	EventCheckpointCommitted   = "checkpoint.committed"
	EventControlCancelRequest  = "control.cancel_requested"
	EventRunTerminal           = "run.terminal"
	EventModelRetryScheduled   = "model.retry_scheduled"
	EventModelAttemptStarted   = "model.attempt_started"
)

// EventPayload is the type-specific payload of one Event type. Every field of a
// payload is required and every struct is closed: the JSON of a payload has
// exactly the schema's keys, with an explicit null where the contract says null.
type EventPayload interface {
	EventType() string
}

// Payload structs, one per type, in the order of EVENT_CONTRACT.

type SessionCreatedPayload struct {
	SessionID string  `json:"session_id"`
	Binding   Binding `json:"binding"`
	Mode      string  `json:"mode"`
}

type InputAcceptedPayload struct {
	Intake          IntakeReceipt `json:"intake"`
	QueueItemID     *string       `json:"queue_item_id"`
	Disposition     string        `json:"disposition"`
	QueueRevision   int64         `json:"queue_revision"`
	ControlRevision int64         `json:"control_revision"`
}

type InputAppliedPayload struct {
	MessageID       string  `json:"message_id"`
	QueueItemID     *string `json:"queue_item_id"`
	ContextRevision int64   `json:"context_revision"`
	Disposition     string  `json:"disposition"`
}

type TaskCreatedPayload struct {
	TaskID       string  `json:"task_id"`
	TurnID       *string `json:"turn_id"`
	ParentTaskID *string `json:"parent_task_id"`
	Kind         string  `json:"kind"`
}

type RunStartedPayload struct {
	RunID                  string  `json:"run_id"`
	PreviousRunID          *string `json:"previous_run_id"`
	TraceID                string  `json:"trace_id"`
	WriterEpoch            int64   `json:"writer_epoch"`
	EffectiveLimits        Limits  `json:"effective_limits"`
	DeadlineAt             string  `json:"deadline_at"`
	RecoveryPolicyRevision string  `json:"recovery_policy_revision"`
}

type ModelRequestedPayload struct {
	ActionID           string `json:"action_id"`
	AttemptID          string `json:"attempt_id"`
	RequestID          string `json:"request_id"`
	Stage              string `json:"stage"`
	Ordinal            int64  `json:"ordinal"`
	InputDigest        string `json:"input_digest"`
	RequestDigest      string `json:"request_digest"`
	BindingFingerprint string `json:"binding_fingerprint"`
	Stream             bool   `json:"stream"`
}

type ModelCompletedPayload struct {
	ActionID                 string  `json:"action_id"`
	AttemptID                string  `json:"attempt_id"`
	Outcome                  string  `json:"outcome"`
	GenerationState          string  `json:"generation_state"`
	FailureCode              *string `json:"failure_code"`
	AttemptReceiptEvidenceID string  `json:"attempt_receipt_evidence_id"`
}

type ActionPreparedPayload struct {
	ActionID       string `json:"action_id"`
	AttemptID      string `json:"attempt_id"`
	Kind           string `json:"kind"`
	Name           string `json:"name"`
	ArgsHash       string `json:"args_hash"`
	PolicyRevision string `json:"policy_revision"`
}

type ActionDispatchStartedPayload struct {
	ActionID        string `json:"action_id"`
	AttemptID       string `json:"attempt_id"`
	WriterEpoch     int64  `json:"writer_epoch"`
	ControlRevision int64  `json:"control_revision"`
}

type ActionCompletedPayload struct {
	ActionID          string   `json:"action_id"`
	AttemptID         string   `json:"attempt_id"`
	EffectState       string   `json:"effect_state"`
	ExitCode          *int64   `json:"exit_code"`
	ResultEvidenceIDs []string `json:"result_evidence_ids"`
	CaptureComplete   bool     `json:"capture_complete"`
}

type CheckpointCommittedPayload struct {
	CheckpointID          string `json:"checkpoint_id"`
	Mode                  string `json:"mode"`
	CandidateHash         string `json:"candidate_hash"`
	ContextRevision       int64  `json:"context_revision"`
	SemanticBoundary      int64  `json:"semantic_boundary"`
	DurableBoundary       int64  `json:"durable_boundary"`
	BeforeCountEvidenceID string `json:"before_count_evidence_id"`
	AfterCountEvidenceID  string `json:"after_count_evidence_id"`
}

type ControlCancelRequestedPayload struct {
	RunID           string `json:"run_id"`
	ControlRevision int64  `json:"control_revision"`
	Reason          string `json:"reason"`
	Principal       string `json:"principal"`
}

type RunTerminalPayload struct {
	Status           string  `json:"status"`
	Code             string  `json:"code"`
	ResultEvidenceID string  `json:"result_evidence_id"`
	LastCheckpointID *string `json:"last_checkpoint_id"`
}

type ModelRetryScheduledPayload struct {
	ActionID                string `json:"action_id"`
	FailedAttemptID         string `json:"failed_attempt_id"`
	NextOrdinal             int64  `json:"next_ordinal"`
	TriggerCode             string `json:"trigger_code"`
	DelayMS                 int64  `json:"delay_ms"`
	RecoveryProfile         string `json:"recovery_profile"`
	RecoveryProfileRevision string `json:"recovery_profile_revision"`
}

type ModelAttemptStartedPayload struct {
	ActionID               string `json:"action_id"`
	AttemptID              string `json:"attempt_id"`
	Ordinal                int64  `json:"ordinal"`
	RequestID              string `json:"request_id"`
	GenerationAttemptsUsed int64  `json:"generation_attempts_used"`
	DeadlineAt             string `json:"deadline_at"`
}

func (SessionCreatedPayload) EventType() string        { return EventSessionCreated }
func (InputAcceptedPayload) EventType() string         { return EventInputAccepted }
func (InputAppliedPayload) EventType() string          { return EventInputApplied }
func (TaskCreatedPayload) EventType() string           { return EventTaskCreated }
func (RunStartedPayload) EventType() string            { return EventRunStarted }
func (ModelRequestedPayload) EventType() string        { return EventModelRequested }
func (ModelCompletedPayload) EventType() string        { return EventModelCompleted }
func (ActionPreparedPayload) EventType() string        { return EventActionPrepared }
func (ActionDispatchStartedPayload) EventType() string { return EventActionDispatchStarted }
func (ActionCompletedPayload) EventType() string       { return EventActionCompleted }
func (CheckpointCommittedPayload) EventType() string   { return EventCheckpointCommitted }
func (ControlCancelRequestedPayload) EventType() string {
	return EventControlCancelRequest
}
func (RunTerminalPayload) EventType() string         { return EventRunTerminal }
func (ModelRetryScheduledPayload) EventType() string { return EventModelRetryScheduled }
func (ModelAttemptStartedPayload) EventType() string { return EventModelAttemptStarted }

// newPayload returns a pointer to an empty payload struct of the type, or nil.
func newPayload(eventType string) EventPayload {
	switch eventType {
	case EventSessionCreated:
		return &SessionCreatedPayload{}
	case EventInputAccepted:
		return &InputAcceptedPayload{}
	case EventInputApplied:
		return &InputAppliedPayload{}
	case EventTaskCreated:
		return &TaskCreatedPayload{}
	case EventRunStarted:
		return &RunStartedPayload{}
	case EventModelRequested:
		return &ModelRequestedPayload{}
	case EventModelCompleted:
		return &ModelCompletedPayload{}
	case EventActionPrepared:
		return &ActionPreparedPayload{}
	case EventActionDispatchStarted:
		return &ActionDispatchStartedPayload{}
	case EventActionCompleted:
		return &ActionCompletedPayload{}
	case EventCheckpointCommitted:
		return &CheckpointCommittedPayload{}
	case EventControlCancelRequest:
		return &ControlCancelRequestedPayload{}
	case EventRunTerminal:
		return &RunTerminalPayload{}
	case EventModelRetryScheduled:
		return &ModelRetryScheduledPayload{}
	case EventModelAttemptStarted:
		return &ModelAttemptStartedPayload{}
	}
	return nil
}

// TypedPayload decodes Payload into the struct of the event's type. Unknown keys
// are an error: the payload is closed.
func (e Event) TypedPayload() (EventPayload, error) {
	p := newPayload(e.Type)
	if p == nil {
		return nil, requestError("unknown event type")
	}
	dec := json.NewDecoder(bytes.NewReader(e.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(p); err != nil {
		return nil, NewError(CodeInvalidRequest, "event payload does not match its type").Wrap(fmt.Errorf("%w: %w", ErrInvalidInput, err))
	}
	return p, nil
}

// DerivedCommon returns the common message_id and code of an event. The store keeps
// neither in a column of its own: for input.accepted and input.applied the message_id
// is the one inside the payload, for run.terminal the code is, and every other event
// type has neither. Reading an event back therefore reproduces what was built.
func DerivedCommon(eventType string, payload json.RawMessage) (messageID, code *string, err error) {
	e := Event{Type: eventType, Payload: payload}
	p, err := e.TypedPayload()
	if err != nil {
		return nil, nil, err
	}
	switch v := p.(type) {
	case *InputAcceptedPayload:
		return Str(v.Intake.MessageID), nil, nil
	case *InputAppliedPayload:
		return Str(v.MessageID), nil, nil
	case *RunTerminalPayload:
		return nil, Str(v.Code), nil
	}
	return nil, nil, nil
}

func eq(a, b *string) bool { return a != nil && b != nil && *a == *b }

// Validate checks what the schema cannot: the payload decodes into the struct of
// its type and is canonical (CJ1) as stored, the common IDs agree with the same
// IDs inside the payload (EVENT_CONTRACT section 2), and each type has the
// common task/run it must have.
func (e Event) Validate() error {
	if _, err := ParseTimestamp(e.RecordedAt); err != nil {
		return requestError("recorded_at is not a valid timestamp")
	}
	canonical, err := EncodeCanonicalContract(e.Payload)
	if err != nil || !bytes.Equal(canonical, e.Payload) {
		return requestError("event payload is not canonical JSON")
	}
	p, err := e.TypedPayload()
	if err != nil {
		return err
	}
	switch e.Type {
	case EventSessionCreated:
		if e.TaskID != nil || e.RunID != nil {
			return requestError("session.created carries no task or run")
		}
	case EventInputAccepted, EventInputApplied:
		// The target task and run may still be undecided.
	case EventTaskCreated:
		if e.TaskID == nil {
			return requestError("task.created needs the task")
		}
	default:
		if e.TaskID == nil || e.RunID == nil {
			return requestError("%s needs the task and the run", e.Type)
		}
	}
	// message_id and code exist only where the payload has the same value, so the
	// store can give them back without columns of their own.
	if e.MessageID != nil && e.Type != EventInputAccepted && e.Type != EventInputApplied {
		return requestError("%s carries no message_id", e.Type)
	}
	if e.Code != nil && e.Type != EventRunTerminal {
		return requestError("%s carries no code", e.Type)
	}
	mismatch := func(what string) error {
		return requestError("%s: payload and common %s disagree", e.Type, what)
	}
	switch v := p.(type) {
	case *InputAcceptedPayload:
		if v.Intake.ThreadID != e.ThreadID {
			return mismatch("thread_id")
		}
		if !eq(&v.Intake.MessageID, e.MessageID) {
			return mismatch("message_id")
		}
		if !eq(&v.Intake.ReceiptID, e.ReceiptID) {
			return mismatch("receipt_id")
		}
		if !eq(&v.Intake.EvidenceID, e.EvidenceID) {
			return mismatch("evidence_id")
		}
		if err := v.Intake.Validate(); err != nil {
			return err
		}
	case *InputAppliedPayload:
		if !eq(&v.MessageID, e.MessageID) {
			return mismatch("message_id")
		}
	case *TaskCreatedPayload:
		if !eq(&v.TaskID, e.TaskID) {
			return mismatch("task_id")
		}
	case *RunStartedPayload:
		if !eq(&v.RunID, e.RunID) {
			return mismatch("run_id")
		}
		if _, err := ParseTimestamp(v.DeadlineAt); err != nil {
			return requestError("run.started deadline_at is not a valid timestamp")
		}
	case *ModelCompletedPayload:
		if !eq(&v.AttemptReceiptEvidenceID, e.EvidenceID) {
			return mismatch("evidence_id")
		}
	case *ControlCancelRequestedPayload:
		if !eq(&v.RunID, e.RunID) {
			return mismatch("run_id")
		}
	case *RunTerminalPayload:
		if !eq(&v.ResultEvidenceID, e.EvidenceID) {
			return mismatch("evidence_id")
		}
		if !eq(&v.Code, e.Code) {
			return mismatch("code")
		}
	case *ModelAttemptStartedPayload:
		if _, err := ParseTimestamp(v.DeadlineAt); err != nil {
			return requestError("model.attempt_started deadline_at is not a valid timestamp")
		}
	}
	return nil
}

// EventCommon is every Event field except the type and the payload.
type EventCommon struct {
	EventID    string
	EventSeq   int64
	ThreadID   string
	TaskID     *string
	RunID      *string
	ReceiptID  *string
	EvidenceID *string
	MessageID  *string
	Code       *string
	RecordedAt string
}

// BuildTypedEvent (F38) turns one durable fact into an Event. The payload is
// encoded as CJ1, the form events.payload_json stores and events/read returns, and
// the finished Event is checked against the schema and the conditions of
// Event.Validate, so an Event with an unknown key, a missing field, a wrong ID
// kind or disagreeing IDs cannot be built.
func BuildTypedEvent(c EventCommon, p EventPayload) (Event, error) {
	if p == nil {
		return Event{}, requestError("event payload is required")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return Event{}, NewError(CodeInternal, "event payload cannot be encoded").Wrap(fmt.Errorf("%w: %w", ErrInvalidInput, err))
	}
	payload, err := EncodeCanonicalContract(raw)
	if err != nil {
		return Event{}, NewError(CodeInternal, "event payload cannot be encoded").Wrap(err)
	}
	ev := Event{
		EventID: c.EventID, EventSeq: c.EventSeq, ThreadID: c.ThreadID,
		TaskID: c.TaskID, RunID: c.RunID, Type: p.EventType(),
		ReceiptID: c.ReceiptID, EvidenceID: c.EvidenceID, MessageID: c.MessageID,
		Code: c.Code, RecordedAt: c.RecordedAt, Payload: payload,
	}
	whole, err := json.Marshal(ev)
	if err != nil {
		return Event{}, NewError(CodeInternal, "event cannot be encoded").Wrap(fmt.Errorf("%w: %w", ErrInvalidInput, err))
	}
	decoded, err := strictjson.Decode(whole)
	if err != nil {
		return Event{}, NewError(CodeInternal, "event cannot be encoded").Wrap(err)
	}
	return decodeValue[Event](decoded)
}
