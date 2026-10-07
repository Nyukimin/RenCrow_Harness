package protocol

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

// Message is the constraint of Decode and Encode: every DTO that has a definition
// in schemas/protocol.schema.json. The method is unexported on purpose, so the set
// is sealed: a new wire type is a new contract and is added in this package, next
// to its schema definition.
type Message interface {
	// schemaDef is the "#/$defs/<name>" the type is validated against; an empty
	// name validates against the whole document (the JSON-RPC request envelope).
	schemaDef() string
}

// semantic is implemented by DTOs with conditions the schema cannot express.
type semantic interface {
	Validate() error
}

// preChecker is implemented by DTOs that must refuse a value with a specific
// error code before the generic schema check would report it as INVALID_PARAMS.
type preChecker interface {
	preCheck(decoded any) error
}

// Decode parses one JSON value into the DTO T and returns it only if the value
// is a well-formed T. It applies, in order:
//
//  1. the strict decoder: duplicate keys, invalid UTF-8, unpaired surrogates, a BOM,
//     trailing values, over-deep nesting and out-of-range numbers are errors;
//  2. the JSON Schema of the type (unknown fields, wrong types, missing fields,
//     patterns, ranges);
//  3. the type's semantic conditions (Validate), for what the schema cannot say.
//
// Errors are *Error: INVALID_PARAMS for 1 and 2, INVALID_REQUEST for 3 and for the
// few conditions named in PROTOCOL (a user_message ContextBlock). All wrap
// ErrInvalidInput, and no message repeats the offending value.
func Decode[T Message](data []byte) (T, error) {
	var zero T
	v, err := strictjson.Decode(data)
	if err != nil {
		return zero, paramsError(invalidWrap(err, "malformed JSON"))
	}
	return decodeValue[T](v)
}

func decodeValue[T Message](v any) (T, error) {
	var out T
	if pre, ok := any(out).(preChecker); ok {
		if err := pre.preCheck(v); err != nil {
			return out, err
		}
	}
	if err := schemacheck.Unmarshal(schemacheck.Protocol, out.schemaDef(), v, &out); err != nil {
		if errors.Is(err, schemacheck.ErrViolation) {
			return out, paramsError(invalidWrap(err, "schema"))
		}
		if errors.Is(err, schemacheck.ErrDecode) {
			return out, paramsError(invalidWrap(err, "typed decode"))
		}
		return out, NewError(CodeInternal, "schema unavailable").Wrap(err)
	}
	if s, ok := any(out).(semantic); ok {
		if err := s.Validate(); err != nil {
			return out, asRequestError(err)
		}
	}
	return out, nil
}

// Encode returns the canonical (CJ1) JSON of v after checking that v itself is a
// well-formed T: the same schema and semantic conditions Decode applies. It is
// what a server should use to produce results and what a store should use to
// persist them, so nothing malformed can be written.
func Encode[T Message](v T) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, NewError(CodeInternal, "encode failed").Wrap(fmt.Errorf("%w: %w", ErrInvalidInput, err))
	}
	decoded, err := strictjson.Decode(raw)
	if err != nil {
		return nil, NewError(CodeInternal, "encode failed").Wrap(fmt.Errorf("%w: %w", ErrInvalidInput, err))
	}
	if _, err := decodeValue[T](decoded); err != nil {
		return nil, err
	}
	out, err := canon.Encode(decoded)
	if err != nil {
		return nil, NewError(CodeInternal, "encode failed").Wrap(fmt.Errorf("%w: %w", ErrInvalidInput, err))
	}
	return out, nil
}

func paramsError(cause error) *Error {
	return NewError(CodeInvalidParams, "params do not satisfy the protocol schema").Wrap(cause)
}

func requestError(format string, args ...any) *Error {
	return NewError(CodeInvalidRequest, format, args...).Wrap(invalid(format, args...))
}

// asRequestError keeps an *Error as it is and turns anything else from a semantic
// check into INVALID_REQUEST, still wrapping ErrInvalidInput.
func asRequestError(err error) error {
	var pe *Error
	if errors.As(err, &pe) {
		return err
	}
	return NewError(CodeInvalidRequest, "request violates a protocol condition").Wrap(err)
}

// Request is the JSON-RPC 2.0 request envelope of the native API. Params holds
// the canonical JSON of the params object; DecodeParams types it by Method.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (Request) schemaDef() string { return "" }

// DecodeRequest parses and validates one request envelope. The schema accepts only
// the 16 native methods and requires each method's params to satisfy its own
// type, so an accepted Request is known to carry well-formed params; the typed
// value is obtained with DecodeParams.
func DecodeRequest(data []byte) (Request, error) { return Decode[Request](data) }

// DecodeParams returns the typed params of the request: a pointer-free value of
// the DTO the method takes (InitializeInput, StartInput, ...).
func (r Request) DecodeParams() (any, error) {
	switch r.Method {
	case "initialize":
		return Decode[InitializeInput](r.Params)
	case "service/capabilities":
		return Decode[EmptyInput](r.Params)
	case "session/open":
		return Decode[SessionOpenInput](r.Params)
	case "session/list":
		return Decode[SessionListInput](r.Params)
	case "session/get":
		return Decode[SessionGetInput](r.Params)
	case "session/fork":
		return Decode[SessionForkInput](r.Params)
	case "turn/start":
		return Decode[StartInput](r.Params)
	case "input/append":
		return Decode[InputAppendInput](r.Params)
	case "turn/interrupt":
		return Decode[InterruptInput](r.Params)
	case "run/get":
		return Decode[RunGetInput](r.Params)
	case "run/resume":
		return Decode[ResumeInput](r.Params)
	case "context/compact":
		return Decode[CompactInput](r.Params)
	case "receipt/get":
		return Decode[ReceiptGetInput](r.Params)
	case "events/read":
		return Decode[EventsReadInput](r.Params)
	case "evidence/read":
		return Decode[EvidenceReadInput](r.Params)
	case "service/shutdown":
		return Decode[ShutdownInput](r.Params)
	}
	return nil, NewError(CodeInvalidParams, "unknown method").Wrap(invalid("method is not part of the native API"))
}

// MutationPayloadHash is the idempotency identity of this request for the
// authenticated principal. It fails with ErrNotMutationMethod for methods that
// carry no idempotency_key.
func (r Request) MutationPayloadHash(principal string) (string, error) {
	return MutationPayloadHash(principal, r.Method, r.Params)
}

func (InitializeInput) schemaDef() string     { return "InitializeInput" }
func (EmptyInput) schemaDef() string          { return "EmptyInput" }
func (SessionOpenInput) schemaDef() string    { return "SessionOpenInput" }
func (SessionOpenResult) schemaDef() string   { return "SessionOpenResult" }
func (SessionListInput) schemaDef() string    { return "SessionListInput" }
func (SessionListResult) schemaDef() string   { return "SessionListResult" }
func (SessionGetInput) schemaDef() string     { return "SessionGetInput" }
func (SessionInfo) schemaDef() string         { return "SessionInfo" }
func (SessionForkInput) schemaDef() string    { return "SessionForkInput" }
func (ForkResult) schemaDef() string          { return "ForkResult" }
func (StartInput) schemaDef() string          { return "StartInput" }
func (StartResult) schemaDef() string         { return "StartResult" }
func (InputAppendInput) schemaDef() string    { return "InputAppendInput" }
func (InputReceipt) schemaDef() string        { return "InputReceipt" }
func (InterruptInput) schemaDef() string      { return "InterruptInput" }
func (InterruptReceipt) schemaDef() string    { return "InterruptReceipt" }
func (RunGetInput) schemaDef() string         { return "RunGetInput" }
func (RunInfo) schemaDef() string             { return "RunInfo" }
func (RunResult) schemaDef() string           { return "RunResult" }
func (Verification) schemaDef() string        { return "Verification" }
func (ResumeInput) schemaDef() string         { return "ResumeInput" }
func (ResumeResult) schemaDef() string        { return "ResumeResult" }
func (CompactInput) schemaDef() string        { return "CompactInput" }
func (CompactResult) schemaDef() string       { return "CompactResult" }
func (BudgetReport) schemaDef() string        { return "BudgetReport" }
func (OperationAccepted) schemaDef() string   { return "OperationAccepted" }
func (ReceiptGetInput) schemaDef() string     { return "ReceiptGetInput" }
func (ReceiptPayload) schemaDef() string      { return "ReceiptPayload" }
func (ReceiptRecord) schemaDef() string       { return "ReceiptRecord" }
func (EventsReadInput) schemaDef() string     { return "EventsReadInput" }
func (EventsReadResult) schemaDef() string    { return "EventsReadResult" }
func (Event) schemaDef() string               { return "Event" }
func (EvidenceReadInput) schemaDef() string   { return "EvidenceReadInput" }
func (EvidenceReadResult) schemaDef() string  { return "EvidenceReadResult" }
func (ShutdownInput) schemaDef() string       { return "ShutdownInput" }
func (ShutdownResult) schemaDef() string      { return "ShutdownResult" }
func (CapabilitiesResult) schemaDef() string  { return "CapabilitiesResult" }
func (Capability) schemaDef() string          { return "Capability" }
func (RecoveryPolicy) schemaDef() string      { return "RecoveryPolicy" }
func (IntakeReceipt) schemaDef() string       { return "IntakeReceipt" }
func (ModelAttemptReceipt) schemaDef() string { return "ModelAttemptReceipt" }
func (ProgressDelta) schemaDef() string       { return "ProgressDelta" }
func (ProgressReset) schemaDef() string       { return "ProgressReset" }
func (ProgressGap) schemaDef() string         { return "ProgressGap" }
func (ErrorInfo) schemaDef() string           { return "ErrorInfo" }
func (ContextBlock) schemaDef() string        { return "ContextBlock" }
func (Binding) schemaDef() string             { return "Binding" }
func (Upstream) schemaDef() string            { return "Upstream" }
func (InputMessage) schemaDef() string        { return "InputMessage" }
func (Limits) schemaDef() string              { return "Limits" }
func (OriginProof) schemaDef() string         { return "OriginProof" }
func (SourceRef) schemaDef() string           { return "SourceRef" }
func (ByteRange) schemaDef() string           { return "ByteRange" }
