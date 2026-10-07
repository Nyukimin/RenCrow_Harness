// Package identity defines the Canonical IDs the Harness issues.
//
// The names, prefixes and meanings are owned by the RenCrow Identity Canonical
// document; this package only fixes their Go types and exact text form. An ID is
// "<prefix>" + a lowercase hyphenated UUID of version 7 (new) or 5 (migration).
// Entropy failure is fatal: New panics and never falls back to another format.
package identity

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// MigrationNamespace is the fixed UUIDv5 namespace for deterministic migration
// IDs. It is the DNS-namespace UUIDv5 of "rencrow.identity.migration.v1" and is
// used as a constant, never re-derived at runtime.
const MigrationNamespace = "6570d821-e63e-592d-a51f-8cf4b43cdba5"

// ErrInvalid is wrapped by every parse and validation failure.
var ErrInvalid = errors.New("invalid canonical id")

var migrationNamespace = uuid.MustParse(MigrationNamespace)

// Kind is the sealed set of ID kinds. Each kind fixes the text prefix and the
// type name used in the migration derivation.
type Kind interface {
	Prefix() string
	TypeName() string
	sealed()
}

// ID is a Canonical ID of kind K. The zero value is invalid. Values produced by
// New, Parse and Migrate are valid; Validate re-checks an unchecked conversion.
type ID[K Kind] string

// String returns the exact text form.
func (id ID[K]) String() string { return string(id) }

// IsZero reports whether the ID is unset.
func (id ID[K]) IsZero() bool { return id == "" }

// Validate checks the exact text form for kind K.
func (id ID[K]) Validate() error {
	_, err := Parse[K](string(id))
	return err
}

// New issues a new UUIDv7 ID. It panics if UUIDv7 generation fails; there is no
// fallback to UUIDv4, timestamps, counters or other random text.
func New[K Kind]() ID[K] {
	var k K
	u, err := uuid.NewV7()
	if err != nil {
		panic(fmt.Errorf("identity: UUIDv7 generation failed, failing closed: %w", err))
	}
	if u.Version() != 7 {
		panic(fmt.Errorf("identity: generator returned UUIDv%d, want UUIDv7", u.Version()))
	}
	return ID[K](k.Prefix() + u.String())
}

// Parse accepts only the exact canonical text: the kind's prefix, then a
// lowercase 8-4-4-4-12 UUID whose version is 5 or 7 and whose variant is RFC 4122.
// It matches the ID patterns in schemas/protocol.schema.json.
func Parse[K Kind](raw string) (ID[K], error) {
	var k K
	rest, ok := strings.CutPrefix(raw, k.Prefix())
	if !ok {
		return "", fmt.Errorf("%w: %s must start with %q", ErrInvalid, k.TypeName(), k.Prefix())
	}
	if err := checkUUIDText(rest); err != nil {
		return "", fmt.Errorf("%w: %s: %s", ErrInvalid, k.TypeName(), err)
	}
	return ID[K](raw), nil
}

// Migrate deterministically derives a UUIDv5 ID for one legacy field value. It is
// for migration processes only, not runtime lookup. The name is
// TypeName NUL table NUL field NUL value, so one legacy string maps to different
// IDs for different target kinds.
func Migrate[K Kind](sourceTable, sourceField, sourceValue string) (ID[K], error) {
	var k K
	if strings.TrimSpace(sourceTable) == "" {
		return "", fmt.Errorf("%w: source table is required", ErrInvalid)
	}
	if strings.TrimSpace(sourceField) == "" {
		return "", fmt.Errorf("%w: source field is required", ErrInvalid)
	}
	if sourceValue == "" {
		return "", fmt.Errorf("%w: source value is required", ErrInvalid)
	}
	name := k.TypeName() + "\x00" + sourceTable + "\x00" + sourceField + "\x00" + sourceValue
	return ID[K](k.Prefix() + uuid.NewSHA1(migrationNamespace, []byte(name)).String()), nil
}

func checkUUIDText(s string) error {
	if len(s) != 36 {
		return errors.New("uuid text must be 36 bytes")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return errors.New("uuid text must be 8-4-4-4-12")
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return errors.New("uuid text must be lowercase hex")
			}
		}
	}
	if v := s[14]; v != '5' && v != '7' {
		return errors.New("uuid version must be 5 or 7")
	}
	if v := s[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
		return errors.New("uuid variant must be RFC 4122")
	}
	return nil
}

// Kinds. Each marker is exported so callers can name Migrate[K].
type (
	TraceKind      struct{}
	EventKind      struct{}
	SessionKind    struct{}
	ThreadKind     struct{}
	TurnKind       struct{}
	MessageKind    struct{}
	TaskKind       struct{}
	RunKind        struct{}
	ActionKind     struct{}
	AttemptKind    struct{}
	RequestKind    struct{}
	ResponseKind   struct{}
	EvidenceKind   struct{}
	CheckpointKind struct{}
	ReceiptKind    struct{}
	QueueItemKind  struct{}
)

func (TraceKind) Prefix() string        { return "trc_" }
func (TraceKind) TypeName() string      { return "TraceID" }
func (TraceKind) sealed()               {}
func (EventKind) Prefix() string        { return "evt_" }
func (EventKind) TypeName() string      { return "EventID" }
func (EventKind) sealed()               {}
func (SessionKind) Prefix() string      { return "ses_" }
func (SessionKind) TypeName() string    { return "SessionID" }
func (SessionKind) sealed()             {}
func (ThreadKind) Prefix() string       { return "thr_" }
func (ThreadKind) TypeName() string     { return "ThreadID" }
func (ThreadKind) sealed()              {}
func (TurnKind) Prefix() string         { return "turn_" }
func (TurnKind) TypeName() string       { return "TurnID" }
func (TurnKind) sealed()                {}
func (MessageKind) Prefix() string      { return "msg_" }
func (MessageKind) TypeName() string    { return "MessageID" }
func (MessageKind) sealed()             {}
func (TaskKind) Prefix() string         { return "tsk_" }
func (TaskKind) TypeName() string       { return "TaskID" }
func (TaskKind) sealed()                {}
func (RunKind) Prefix() string          { return "run_" }
func (RunKind) TypeName() string        { return "RunID" }
func (RunKind) sealed()                 {}
func (ActionKind) Prefix() string       { return "act_" }
func (ActionKind) TypeName() string     { return "ActionID" }
func (ActionKind) sealed()              {}
func (AttemptKind) Prefix() string      { return "att_" }
func (AttemptKind) TypeName() string    { return "AttemptID" }
func (AttemptKind) sealed()             {}
func (RequestKind) Prefix() string      { return "req_" }
func (RequestKind) TypeName() string    { return "RequestID" }
func (RequestKind) sealed()             {}
func (ResponseKind) Prefix() string     { return "rsp_" }
func (ResponseKind) TypeName() string   { return "ResponseID" }
func (ResponseKind) sealed()            {}
func (EvidenceKind) Prefix() string     { return "evd_" }
func (EvidenceKind) TypeName() string   { return "EvidenceID" }
func (EvidenceKind) sealed()            {}
func (CheckpointKind) Prefix() string   { return "ckp_" }
func (CheckpointKind) TypeName() string { return "CheckpointID" }
func (CheckpointKind) sealed()          {}
func (ReceiptKind) Prefix() string      { return "rcp_" }
func (ReceiptKind) TypeName() string    { return "ReceiptID" }
func (ReceiptKind) sealed()             {}
func (QueueItemKind) Prefix() string    { return "qit_" }
func (QueueItemKind) TypeName() string  { return "QueueItemID" }
func (QueueItemKind) sealed()           {}

// Distinct ID types. Mixing them is a compile error.
type (
	TraceID      = ID[TraceKind]
	EventID      = ID[EventKind]
	SessionID    = ID[SessionKind]
	ThreadID     = ID[ThreadKind]
	TurnID       = ID[TurnKind]
	MessageID    = ID[MessageKind]
	TaskID       = ID[TaskKind]
	RunID        = ID[RunKind]
	ActionID     = ID[ActionKind]
	AttemptID    = ID[AttemptKind]
	RequestID    = ID[RequestKind]
	ResponseID   = ID[ResponseKind]
	EvidenceID   = ID[EvidenceKind]
	CheckpointID = ID[CheckpointKind]
	ReceiptID    = ID[ReceiptKind]
	QueueItemID  = ID[QueueItemKind]
)

func NewTraceID() TraceID           { return New[TraceKind]() }
func NewEventID() EventID           { return New[EventKind]() }
func NewSessionID() SessionID       { return New[SessionKind]() }
func NewThreadID() ThreadID         { return New[ThreadKind]() }
func NewTurnID() TurnID             { return New[TurnKind]() }
func NewMessageID() MessageID       { return New[MessageKind]() }
func NewTaskID() TaskID             { return New[TaskKind]() }
func NewRunID() RunID               { return New[RunKind]() }
func NewActionID() ActionID         { return New[ActionKind]() }
func NewAttemptID() AttemptID       { return New[AttemptKind]() }
func NewRequestID() RequestID       { return New[RequestKind]() }
func NewResponseID() ResponseID     { return New[ResponseKind]() }
func NewEvidenceID() EvidenceID     { return New[EvidenceKind]() }
func NewCheckpointID() CheckpointID { return New[CheckpointKind]() }
func NewReceiptID() ReceiptID       { return New[ReceiptKind]() }
func NewQueueItemID() QueueItemID   { return New[QueueItemKind]() }

func ParseTraceID(raw string) (TraceID, error)           { return Parse[TraceKind](raw) }
func ParseEventID(raw string) (EventID, error)           { return Parse[EventKind](raw) }
func ParseSessionID(raw string) (SessionID, error)       { return Parse[SessionKind](raw) }
func ParseThreadID(raw string) (ThreadID, error)         { return Parse[ThreadKind](raw) }
func ParseTurnID(raw string) (TurnID, error)             { return Parse[TurnKind](raw) }
func ParseMessageID(raw string) (MessageID, error)       { return Parse[MessageKind](raw) }
func ParseTaskID(raw string) (TaskID, error)             { return Parse[TaskKind](raw) }
func ParseRunID(raw string) (RunID, error)               { return Parse[RunKind](raw) }
func ParseActionID(raw string) (ActionID, error)         { return Parse[ActionKind](raw) }
func ParseAttemptID(raw string) (AttemptID, error)       { return Parse[AttemptKind](raw) }
func ParseRequestID(raw string) (RequestID, error)       { return Parse[RequestKind](raw) }
func ParseResponseID(raw string) (ResponseID, error)     { return Parse[ResponseKind](raw) }
func ParseEvidenceID(raw string) (EvidenceID, error)     { return Parse[EvidenceKind](raw) }
func ParseCheckpointID(raw string) (CheckpointID, error) { return Parse[CheckpointKind](raw) }
func ParseReceiptID(raw string) (ReceiptID, error)       { return Parse[ReceiptKind](raw) }
func ParseQueueItemID(raw string) (QueueItemID, error)   { return Parse[QueueItemKind](raw) }
