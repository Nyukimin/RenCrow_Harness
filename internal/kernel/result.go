package kernel

import (
	"errors"
	"fmt"
	"slices"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// ErrInvalidTerminal is wrapped by every refusal to build a RunResult from facts that
// contradict each other.
var ErrInvalidTerminal = errors.New("kernel: invalid terminal result")

// ResultFacts are the records a Run's end is built from.
type ResultFacts struct {
	RunID   string
	TaskID  string
	Outcome Outcome
	// FinalMessageID and FinalText are the adopted final message, both or neither.
	FinalMessageID *string
	FinalText      string
	// Verification is what was checked and found; nil means nothing was checked.
	Verification *protocol.Verification
	// EvidenceIDs are the Evidence of the Run that the result points at.
	EvidenceIDs         []string
	UnresolvedActionIDs []string
	LastCheckpointID    *string
	// System marks the result of a system Run (a manual compaction): it completes without a
	// final message, because it adopts none, and it never has one.
	System bool
}

// BuildRunResult (F20) builds the result of a Run from the records of its end. It
// keeps two things apart that must stay apart. The Run's status says how the Run
// ended; the verification says what was checked, and is not_run unless something
// was: a Run that completed with a final answer has not thereby shown the Task done,
// and a passed verification names at least one Evidence and the criteria revision it
// was checked against. A result whose parts contradict each other (a completed Run
// with an action left unresolved, a final text without its message, a verification
// that passed on nothing) is not built.
func BuildRunResult(f ResultFacts) (protocol.RunResult, error) {
	bad := func(format string, args ...any) (protocol.RunResult, error) {
		return protocol.RunResult{}, fmt.Errorf("%w: %s", ErrInvalidTerminal, fmt.Sprintf(format, args...))
	}
	o := f.Outcome
	if !slices.Contains(Statuses(), o.Status) {
		return bad("the status is not a terminal status")
	}
	if o.Code == "" || len(o.Code) > 80 {
		return bad("the code is empty or too long")
	}
	if (f.FinalMessageID != nil) != (f.FinalText != "") {
		return bad("a final message and a final text come together")
	}
	if f.System && f.FinalMessageID != nil {
		return bad("a system run adopts no final message")
	}
	if o.Status == StatusCompleted {
		if f.FinalMessageID == nil && !f.System {
			return bad("a completed run has a final message")
		}
		if len(f.UnresolvedActionIDs) > 0 || o.UnresolvedModelAction {
			return bad("a completed run has no unresolved action")
		}
	}

	evidence := dedupe(f.EvidenceIDs)
	v := protocol.Verification{Status: "not_run", EvidenceIDs: []string{}, CriteriaRevision: nil}
	if f.Verification != nil {
		v = *f.Verification
		v.EvidenceIDs = dedupe(v.EvidenceIDs)
		switch v.Status {
		case "passed":
			if len(v.EvidenceIDs) == 0 || v.CriteriaRevision == nil || *v.CriteriaRevision == "" {
				return bad("a passed verification names its evidence and criteria revision")
			}
		case "failed", "not_run", "unknown":
		default:
			return bad("the verification status is unknown")
		}
		for _, id := range v.EvidenceIDs {
			if !slices.Contains(evidence, id) {
				return bad("a verification names evidence the result does not list")
			}
		}
	}
	res := protocol.RunResult{
		RunID: f.RunID, TaskID: f.TaskID, Status: o.Status, Code: o.Code, FinalMessageID: f.FinalMessageID, FinalText: f.FinalText,
		Verification: v, EvidenceIDs: evidence, UnresolvedActionIDs: dedupe(f.UnresolvedActionIDs), LastCheckpointID: f.LastCheckpointID,
		Resumable: Resumable(o.Status, o.Code),
	}
	if _, err := protocol.Encode(res); err != nil {
		return bad("the result does not satisfy the protocol")
	}
	return res, nil
}

func dedupe(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
