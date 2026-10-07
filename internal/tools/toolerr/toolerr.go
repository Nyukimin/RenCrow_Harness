// Package toolerr is the error every built-in Tool returns for a failure it can name:
// a stable code, a short message safe to show the model, and how the failure left the
// world (nothing was attempted, the attempt changed nothing, or the effect is not known).
//
// It is a leaf package so the file and process Tools and the dispatcher that runs them
// agree on one vocabulary without importing each other.
package toolerr

import (
	"errors"
	"fmt"
)

// Class says what a failure means for the Action's effect.
type Class int

const (
	// Rejected: the policy or the request was refused before anything was attempted.
	// The effect state is not_started.
	Rejected Class = iota
	// Failed: the Tool attempted the work and it failed with a known end that changed
	// nothing. The effect state is failed.
	Failed
	// Unknown: the Tool may have changed the world and cannot say. The effect state is
	// unknown, and nothing is retried on the strength of it.
	Unknown
)

// Codes of Tool failures. They are data the model and the kernel read, never prose.
const (
	CodePolicyRejected  = "POLICY_REJECTED"
	CodePolicyAmbiguous = "POLICY_AMBIGUOUS"
	CodeToolNotAllowed  = "TOOL_NOT_ALLOWED"
	CodeModeUnavailable = "MODE_UNAVAILABLE"
	CodePathInvalid     = "PATH_INVALID"
	CodePathOutside     = "PATH_OUTSIDE_SCOPE"
	CodePathEscape      = "PATH_ESCAPE"
	CodeNotFound        = "NOT_FOUND"
	CodeNotFile         = "NOT_A_FILE"
	CodeNotDirectory    = "NOT_A_DIRECTORY"
	CodeParentMissing   = "PARENT_MISSING"
	CodeAlreadyExists   = "ALREADY_EXISTS"
	CodeHashMismatch    = "HASH_MISMATCH"
	CodeOldTextMissing  = "OLD_TEXT_NOT_FOUND"
	CodeOldTextAmbig    = "OLD_TEXT_NOT_UNIQUE"
	CodeInvalidRange    = "INVALID_RANGE"
	CodeFileTooLarge    = "FILE_TOO_LARGE"
	CodeRegexInvalid    = "REGEX_INVALID"
	CodeIO              = "IO_ERROR"
	CodeDurability      = "DURABILITY_UNKNOWN"
	CodeEnvUnknown      = "ENV_PROFILE_UNKNOWN"
	CodeStartFailed     = "START_FAILED"
	CodeTimeout         = "TIMEOUT"
	CodeCaptureLimit    = "CAPTURE_LIMIT"
	CodeCancelled       = "CANCELLED"
	CodeEvidenceDenied  = "EVIDENCE_NOT_READABLE"
	CodeProjection      = "PROJECTION_UNAVAILABLE"
	CodeInternal        = "TOOL_INTERNAL"
)

// Error is one named failure of a Tool.
type Error struct {
	Code    string
	Message string
	Class   Class
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// New builds an Error.
func New(class Class, code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Class: class}
}

// Reject, Fail and Uncertain build the three classes.
func Reject(code, format string, args ...any) *Error { return New(Rejected, code, format, args...) }

func Fail(code, format string, args ...any) *Error { return New(Failed, code, format, args...) }

func Uncertain(code, format string, args ...any) *Error { return New(Unknown, code, format, args...) }

// As returns the *Error in err's chain.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// EffectState is the effect state (INTERNAL_CONTRACTS ToolResult) a class stands for.
func (c Class) EffectState() string {
	switch c {
	case Rejected:
		return "not_started"
	case Failed:
		return "failed"
	}
	return "unknown"
}
