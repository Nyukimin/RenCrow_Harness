// Package kernel is the Harness's work kernel: a pure state machine that decides
// what a Run does next, and the driver that carries those decisions out.
//
// The machine (Transition, ClassifyFailure, BuildRunResult and the helpers beside
// them) is a function of its arguments. It reads no clock, store, network or random
// source and issues no ID: the driver puts the time into every event it hands over,
// executes the effects the machine asks for, and feeds what happened back as the
// next event. That split is what makes every decision about a Run testable without
// a store or a model, and an architecture test holds the pure files to it.
//
// This build runs the path Admitting, Loading, Assembling, Measuring, Generating,
// ValidatingFinal, PersistingResult, Terminal, with the Tool exchange (PreparingAction,
// Executing, PersistingObservation), the retry's RetryWaiting and the compaction of a
// prompt that does not fit (Compacting, CommittingCheckpoint), and ends a failed Run
// through PersistingResult. Cancelling and Recovering are part of the type (a Run's phase
// is a public value) but nothing reaches them yet; the machine refuses to leave one.
package kernel

// Phase is the phase of a Run (RunInfo.phase). It is not the Run's status: a phase
// says what the Run is doing, a status how it ended.
type Phase string

// The sixteen phases (IMPLEMENTATION_SPEC section 18).
const (
	PhaseAdmitting             Phase = "Admitting"
	PhaseLoading               Phase = "Loading"
	PhaseAssembling            Phase = "Assembling"
	PhaseMeasuring             Phase = "Measuring"
	PhaseGenerating            Phase = "Generating"
	PhasePreparingAction       Phase = "PreparingAction"
	PhaseExecuting             Phase = "Executing"
	PhasePersistingObservation Phase = "PersistingObservation"
	PhaseCompacting            Phase = "Compacting"
	PhaseCommittingCheckpoint  Phase = "CommittingCheckpoint"
	PhaseValidatingFinal       Phase = "ValidatingFinal"
	PhasePersistingResult      Phase = "PersistingResult"
	PhaseCancelling            Phase = "Cancelling"
	PhaseRecovering            Phase = "Recovering"
	PhaseRetryWaiting          Phase = "RetryWaiting"
	PhaseTerminal              Phase = "Terminal"
)

// Phases returns all sixteen phases in the order of the specification.
func Phases() []Phase {
	return []Phase{
		PhaseAdmitting, PhaseLoading, PhaseAssembling, PhaseMeasuring, PhaseGenerating, PhasePreparingAction, PhaseExecuting,
		PhasePersistingObservation, PhaseCompacting, PhaseCommittingCheckpoint, PhaseValidatingFinal, PhasePersistingResult,
		PhaseCancelling, PhaseRecovering, PhaseRetryWaiting, PhaseTerminal,
	}
}

// Valid reports whether p is one of the sixteen phases.
func (p Phase) Valid() bool {
	for _, q := range Phases() {
		if p == q {
			return true
		}
	}
	return false
}

// Reachable reports whether this build can be in the phase: the fourteen of the path
// above (the eight of a Run that only answers, the three of the Tool exchange,
// RetryWaiting, the backoff before a retried Attempt, and the two of a compaction). A Run
// is never in any other phase here and Transition refuses to act on one.
func (p Phase) Reachable() bool {
	switch p {
	case PhaseAdmitting, PhaseLoading, PhaseAssembling, PhaseMeasuring, PhaseGenerating, PhasePreparingAction, PhaseExecuting,
		PhasePersistingObservation, PhaseRetryWaiting, PhaseCompacting, PhaseCommittingCheckpoint, PhaseValidatingFinal, PhasePersistingResult, PhaseTerminal:
		return true
	}
	return false
}

// Run statuses (RunResult.status): the seven ways a Run ends, and running.
const (
	StatusRunning         = "running"
	StatusCompleted       = "completed"
	StatusIncomplete      = "incomplete"
	StatusRejected        = "rejected"
	StatusBlocked         = "blocked"
	StatusCancelled       = "cancelled"
	StatusFailed          = "failed"
	StatusRestartRequired = "restart_required"
)

// Statuses returns the seven terminal statuses.
func Statuses() []string {
	return []string{StatusCompleted, StatusIncomplete, StatusRejected, StatusBlocked, StatusCancelled, StatusFailed, StatusRestartRequired}
}
