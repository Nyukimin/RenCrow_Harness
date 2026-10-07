// Package toolview is the one form a Tool's outcome takes in front of the model: the
// content of the role=tool message that answers a Tool call. It is a leaf package so
// the dispatcher, which renders the outcome of a call that ran, and the store, which
// renders the outcome of a call that did not (or whose outcome is not known), write
// one format.
//
// A view is stored as Evidence and applied to the Thread's context as the call's
// answer, so every later prompt carries the same bytes.
package toolview

import (
	"encoding/json"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Codes of a call that has no Tool result of its own.
const (
	// CodeNotExecuted: the call was never run (an earlier call of the response did not
	// succeed, or the Run ended first).
	CodeNotExecuted = "NOT_EXECUTED"
	// CodeEffectUnknown: the call may have run and its outcome was not recorded.
	CodeEffectUnknown = "EFFECT_OUTCOME_UNKNOWN"
)

// Error is the failure part of a view.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// View is the answer to one Tool call. Result is the Tool's own result object, or null
// when the call produced none.
type View struct {
	Tool            string          `json:"tool"`
	ActionID        string          `json:"action_id"`
	EffectState     string          `json:"effect_state"`
	ExitCode        *int64          `json:"exit_code"`
	CaptureComplete bool            `json:"capture_complete"`
	EvidenceIDs     []string        `json:"evidence_ids"`
	Error           *Error          `json:"error"`
	Result          json.RawMessage `json:"result"`
}

// Encode is the canonical JSON text of the view.
func Encode(v View) ([]byte, error) {
	if v.EvidenceIDs == nil {
		v.EvidenceIDs = []string{}
	}
	if len(v.Result) == 0 {
		v.Result = json.RawMessage("null")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return protocol.EncodeCanonicalContract(raw)
}

// NotExecuted is the answer to a call that was not run.
func NotExecuted(tool, actionID, message string) View {
	return View{Tool: tool, ActionID: actionID, EffectState: "not_started", CaptureComplete: true,
		Error: &Error{Code: CodeNotExecuted, Message: message}}
}

// Unknown is the answer to a call whose outcome is not known.
func Unknown(tool, actionID, message string, evidence []string) View {
	return View{Tool: tool, ActionID: actionID, EffectState: "unknown", CaptureComplete: false, EvidenceIDs: evidence,
		Error: &Error{Code: CodeEffectUnknown, Message: message}}
}
