package kernel

import (
	"encoding/json"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// CommittedCheckpoint is the part of a committed checkpoint a CompactResult is made of.
type CommittedCheckpoint struct {
	ID, Mode                          string
	SemanticBoundary, DurableBoundary int64
	Before, After                     modelport.MeasureResult
}

// CompactFacts are what a manual compaction's CompactResult is made of: how its system Run
// ended, what the compaction came to (nil when the Run ended before it came to anything, or
// in another process), and the checkpoint it committed (nil when it committed none).
type CompactFacts struct {
	RunStatus, RunCode string
	Outcome            *compaction.Outcome
	Checkpoint         *CommittedCheckpoint
}

// The fixed text of the short diagnostic a CompactResult carries for a code. It names what
// happened and never what was in the context, a key or a path.
func compactError(code string) *protocol.ErrorInfo {
	msg := "the manual compaction did not complete"
	switch code {
	case CodeCapacityBlocked:
		msg = "the context cannot be made to fit by a compaction"
	case CodeIntegrityBlocked:
		msg = "what is stored for the context contradicts itself"
	case CodePersistenceUncertain:
		msg = "whether the checkpoint was stored is not known; restart the service"
	case CodeCompactionStale:
		msg = "the context changed under the candidate twice"
	case modelport.CodeBudgetUnverified:
		msg = "the size of the context could not be verified"
	case modelport.CodeCancelled, modelport.CodePermitRevoked:
		msg = "the compaction was stopped"
	case CodeDeadlineExceeded, CodeDriverStopped:
		msg = "the compaction ran out of time or its driver stopped"
	}
	retry := false
	switch code {
	case CodeDeadlineExceeded, CodeDriverStopped, modelport.CodeModelUnavailable, CodeModelTemporarilyDown:
		retry = true
	}
	return &protocol.ErrorInfo{Code: code, Message: msg, Retryable: retry}
}

// BuildCompactResult (PROTOCOL sections 6 and 9, IMPLEMENTATION_SPEC section 11) makes the
// CompactResult of a manual compaction from how its Run ended. It is pure, and it never
// makes an operation look like another: executed is only a compaction that came to one of
// the five outcomes; one that was stopped, that went stale, or that could not run is
// cancelled, stale or unavailable, with no outcome, and no checkpoint is named unless one
// was committed.
func BuildCompactResult(f CompactFacts) protocol.CompactResult {
	r := protocol.CompactResult{Status: compaction.StatusExecuted}
	if c := f.Checkpoint; c != nil {
		before, after := contextplan.ReportOf(c.Before), contextplan.ReportOf(c.After)
		outcome := compaction.OutcomeNormal
		if c.Mode == compaction.ModeEmergency {
			outcome = compaction.OutcomeEmergency
		}
		r.Outcome, r.CheckpointID, r.Before, r.After = protocol.Str(outcome), protocol.Str(c.ID), &before, &after
		r.SemanticBoundary, r.DurableBoundary = &c.SemanticBoundary, &c.DurableBoundary
		switch f.RunCode {
		case CodeCompactionCommitted, CodeDriverStopped, CodeDeadlineExceeded, modelport.CodeCancelled:
			// The checkpoint is the result: a Run whose driver merely stopped, or whose stop
			// came, after the commit says nothing more.
		default:
			// The checkpoint is stored and the Run ended on something else (a stage generation
			// of the same compaction that is unresolved, a context that no longer reads back):
			// the result says both, and an anomaly is not hidden behind the checkpoint.
			r.Error = compactError(f.RunCode)
		}
		return r
	}
	switch {
	case f.RunStatus == StatusCompleted:
		// A manual compaction that completed committed a checkpoint. Without one the records
		// contradict each other, and the result does not pretend otherwise.
		return protocol.CompactResult{Status: compaction.StatusUnavailable, Error: compactError(CodeInternalError)}
	case f.RunCode == CodeCapacityBlocked:
		r.Outcome, r.Error = protocol.Str(compaction.OutcomeCapacity), compactError(f.RunCode)
		if o := f.Outcome; o != nil {
			if o.Required != nil && o.Available != nil && *o.Required > *o.Available {
				// The numbers come as a pair and only for a real shortfall.
				r.RequiredMinimumTokens, r.AvailableTokens = o.Required, o.Available
			}
			if verified := o.Before.Result; verified.State == "verified_exact" || verified.State == "verified_bound" {
				b := contextplan.ReportOf(verified)
				r.Before = &b
			}
		}
	case f.RunCode == CodeIntegrityBlocked:
		r.Outcome, r.Error = protocol.Str(compaction.OutcomeIntegrity), compactError(f.RunCode)
	case f.RunCode == CodePersistenceUncertain:
		r.Outcome, r.Error = protocol.Str(compaction.OutcomeRestart), compactError(f.RunCode)
	case f.RunCode == CodeCompactionStale:
		r.Status, r.Error = compaction.StatusStale, compactError(f.RunCode)
	case f.RunStatus == StatusCancelled || f.RunCode == CodeDeadlineExceeded || f.RunCode == CodeDriverStopped:
		r.Status, r.Error = compaction.StatusCancelled, compactError(f.RunCode)
	default:
		r.Status, r.Error = compaction.StatusUnavailable, compactError(f.RunCode)
	}
	return r
}

// compactResultOfStale is the CompactResult of a manual compaction whose Run was settled by
// another driver than its own (the process that ran it stopped): all that is known of it is
// the Run's end and the checkpoint its last transaction left, if it left one (a checkpoint
// is one transaction, so one that exists is whole).
func compactResultOfStale(st sqlite.StaleRun, res protocol.RunResult) protocol.CompactResult {
	f := CompactFacts{RunStatus: res.Status, RunCode: res.Code}
	if cp := st.LastCheckpoint; cp != nil {
		var counts struct {
			Before modelport.MeasureResult `json:"before"`
			After  modelport.MeasureResult `json:"after"`
		}
		if err := json.Unmarshal([]byte(cp.CountJSON), &counts); err == nil {
			f.Checkpoint = &CommittedCheckpoint{ID: cp.CheckpointID, Mode: cp.Mode, SemanticBoundary: cp.SemanticBoundary, DurableBoundary: cp.DurableBoundary,
				Before: counts.Before, After: counts.After}
		} else {
			// A stored checkpoint whose counts cannot be read is a contradiction in what is
			// stored, which no result may paper over.
			f.RunStatus, f.RunCode = StatusBlocked, CodeIntegrityBlocked
		}
	}
	return BuildCompactResult(f)
}
