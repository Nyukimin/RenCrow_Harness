package protocol

import (
	"errors"
	"fmt"
)

// Domain error codes of the native protocol (PROTOCOL section 8) and the codes the
// request decoder and the admission path return. A code is an API value, not a
// diagnostic: never put a payload, a key, a credential or a full path in a message.
const (
	CodeBusy                 = "BUSY"
	CodeForbidden            = "FORBIDDEN"
	CodeRevisionConflict     = "REVISION_CONFLICT"
	CodeIdempotencyConflict  = "IDEMPOTENCY_CONFLICT"
	CodeUnsupportedContract  = "UNSUPPORTED_CONTRACT"
	CodeBudgetUnverified     = "BUDGET_UNVERIFIED"
	CodeInvalidRange         = "INVALID_RANGE"
	CodeIntegrityBlocked     = "INTEGRITY_BLOCKED"
	CodePersistenceUncertain = "PERSISTENCE_UNCERTAIN"
	CodeInvalidParams        = "INVALID_PARAMS"  // the value does not satisfy the schema
	CodeInvalidRequest       = "INVALID_REQUEST" // schema-valid but semantically refused
	CodeInvalidOriginProof   = "INVALID_ORIGIN_PROOF"
	CodeInvalidLimits        = "INVALID_LIMITS"
	// CodeContextAssetTooLarge: an AGENTS.md, a SKILL.md or the set of them is over a limit
	// of HOST_ASSETS; the Run is not admitted and nothing was cut to fit.
	CodeContextAssetTooLarge = "CONTEXT_ASSET_TOO_LARGE"
	// CodeInvalidExtension: an AGENTS.md or SKILL.md, or the place they are looked for, is not
	// acceptable (a link, a header outside the subset, a name that is not unique).
	CodeInvalidExtension = "INVALID_EXTENSION"
	CodeInternal         = "INTERNAL"
)

// Error is a typed domain failure. It carries the stable Code and a short message
// that is safe to return to a client; Cause keeps the underlying error for
// errors.Is / errors.As and is never serialized.
type Error struct {
	Code      string
	Message   string
	Retryable bool
	Cause     error
}

// NewError builds an Error with a formatted, client-safe message.
func NewError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap attaches the underlying cause.
func (e *Error) Wrap(cause error) *Error {
	e.Cause = cause
	return e
}

// AsRetryable marks the failure as safe to retry with the same idempotency key.
func (e *Error) AsRetryable() *Error {
	e.Retryable = true
	return e
}

// Error implements error.
func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Unwrap returns the underlying cause, if any.
func (e *Error) Unwrap() error { return e.Cause }

// Info is the wire form of the failure. evidence_id is always null here; a failure
// that has private Evidence attaches it where it is produced.
func (e *Error) Info() ErrorInfo {
	return ErrorInfo{Code: e.Code, Message: e.Message, Retryable: e.Retryable}
}

// CodeOf returns the Code of the first *Error in err's chain, or "" if there is none.
func CodeOf(err error) string {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}
