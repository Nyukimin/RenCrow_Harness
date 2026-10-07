package kernel

import (
	"errors"
	"fmt"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The ways Transition refuses.
var (
	// ErrTerminal: the Run is Terminal; nothing moves it again.
	ErrTerminal = errors.New("kernel: the run is terminal")
	// ErrUnreachablePhase: the Run is in a phase this build never enters.
	ErrUnreachablePhase = errors.New("kernel: the run is in a phase this build does not run")
	// ErrInvalidTransition: the event does not belong to the phase.
	ErrInvalidTransition = errors.New("kernel: the event is not valid in this phase")
)

// State is everything the machine knows about a Run. It holds no handle, no time and
// nothing the driver owns: it is a value, copied on every step.
type State struct {
	Phase Phase
	// Limits and Deadline are the Run's, fixed at its admission.
	Limits   protocol.Limits
	Deadline time.Time
	// ModelStepsUsed and GenerationAttemptsUsed are what the Run has consumed. They
	// count a step or an attempt as soon as the machine sends the Run to generate.
	ModelStepsUsed         int64
	GenerationAttemptsUsed int64
	// ToolTotal is how many Tool calls the response being acted on has, and ToolNext
	// the one being run (both zero outside the Tool exchange).
	ToolTotal int
	ToolNext  int
	// Attempt is the ordinal of the Attempt of the act Action being generated: 0, or 1
	// once the first failed and was scheduled for a retry. A retry spends a generation
	// attempt and no model step, and the second Attempt is never retried.
	Attempt int64
	// Retry is the scheduled retry while the Run waits for it (RetryWaiting).
	Retry *RetryDecision
	// Compaction says this Run may compact a prompt that does not fit: the deployment
	// enabled it. Without it a prompt that does not fit ends the Run, as it always did.
	Compaction bool
	// Compacted is set once a checkpoint was committed for the act step being prepared: a
	// prompt that still does not fit after one is a capacity block, not another compaction.
	Compacted bool
	// CompactStale counts the candidates found stale at their commit in this step; one new
	// candidate may follow the first.
	CompactStale int
	// CompactUnknown: a stage generation of the compaction ended with its outcome unknown.
	// The checkpoint may still be committed, and the Run then ends: it does not generate
	// again while a generation of its own is unresolved.
	CompactUnknown bool
	// Early says the compaction being run was started by the trigger ratio while the prompt
	// still fits, not because it does not. Such a compaction is an optimization: when it
	// cannot give a checkpoint (the smallest candidate does not fit, a count cannot be
	// verified, the candidate went stale) the Run goes on with the prompt it has, which
	// fits. EarlyTried is set when one ended without giving one, and the trigger is then
	// not tried again in this Run: a prompt that really does not fit is still compacted
	// (the way the Run always compacted).
	Early      bool
	EarlyTried bool
	// Manual marks a manual compaction: a system Run that exists to compact the Thread's
	// context and end. It counts the live prompt, compacts it whatever the count says (a
	// prompt that fits is compacted too: the operator asked), commits, and ends completed. It
	// generates no answer and applies no input.
	Manual bool
	// Pending is how the Run ends, set when it enters PersistingResult.
	Pending *Outcome
}

// NewState is the state of a Run that has just been admitted, with whatever it has
// already consumed.
func NewState(limits protocol.Limits, deadline time.Time, modelSteps, attempts int64) State {
	return State{Phase: PhaseAdmitting, Limits: limits, Deadline: deadline, ModelStepsUsed: modelSteps, GenerationAttemptsUsed: attempts}
}

// EventKind is what happened.
type EventKind string

// The events of the path. Each is the result of the effect the machine asked for,
// except EvStart, which opens the Run.
const (
	EvStart      EventKind = "start"
	EvLoaded     EventKind = "loaded"
	EvAssembled  EventKind = "assembled"
	EvMeasured   EventKind = "measured"
	EvGenerated  EventKind = "generated"
	EvFinalValid EventKind = "final_valid"
	EvFailed     EventKind = "failed"
	EvPersisted  EventKind = "persisted"
	// EvPrepared: the response's Tool calls are bound to Actions. EvToolSettled: the call
	// being run ended. EvObserved: the Tool exchange is part of the context.
	EvPrepared    EventKind = "prepared"
	EvToolSettled EventKind = "tool_settled"
	EvObserved    EventKind = "observed"
	// EvRetryDue: the retry's wait ended and the retry is to be measured and sent.
	EvRetryDue EventKind = "retry_due"
	// EvCompactReady: a validated candidate is ready to be committed. EvCommitted: it was
	// committed. EvCommitStale: it was not, because what it was made against moved.
	EvCompactReady EventKind = "compact_ready"
	EvCommitted    EventKind = "committed"
	EvCommitStale  EventKind = "commit_stale"
	// EvCompactSkipped: an early compaction ended without a candidate that could be
	// committed, and the Run goes on with the prompt it measured.
	EvCompactSkipped EventKind = "compact_skipped"
)

// ToolEnd is how one Tool call ended: its effect state (not_started, completed, failed,
// cancelled or unknown), and the failure that ends the Run when it must end there.
type ToolEnd struct {
	Effect  string
	Failure *Failure
}

// Event is one thing that happened, with the instant the driver observed it.
type Event struct {
	Kind EventKind
	// At is the driver's clock reading. The machine reads no clock of its own.
	At time.Time
	// Fit is the verdict of the capacity check (EvMeasured).
	Fit contextplan.Verdict
	// Early says the count is a verified fit that has reached the trigger ratio
	// (EvMeasured): a compaction may be started before the prompt is full.
	Early bool
	// Gen describes the finished generation (EvGenerated).
	Gen Generated
	// Failure is what went wrong (EvFailed).
	Failure Failure
	// Tool describes the call that ended (EvToolSettled).
	Tool ToolEnd
	// Compact says what the compaction spent (EvCompactReady).
	Compact CompactReady
}

// CompactReady is what a compaction that produced a candidate cost: the generation
// attempts its stage requests spent, and whether one of them ended with its outcome unknown.
type CompactReady struct {
	Generations int64
	Unknown     bool
}

// EffectKind is what the driver is asked to do.
type EffectKind string

// The effects. Each ends in exactly one event.
const (
	EffLoad          EffectKind = "load"
	EffAssemble      EffectKind = "assemble"
	EffMeasure       EffectKind = "measure"
	EffGenerate      EffectKind = "generate"
	EffValidateFinal EffectKind = "validate_final"
	EffPersistResult EffectKind = "persist_result"
	// EffPrepareBatch binds the response's Tool calls to Actions; EffExecuteTool runs
	// the call State.ToolNext; EffPersistObservation applies the exchange to the context.
	EffPrepareBatch       EffectKind = "prepare_batch"
	EffExecuteTool        EffectKind = "execute_tool"
	EffPersistObservation EffectKind = "persist_observation"
	// EffWaitRetry records the scheduled retry and waits out its backoff; the wait ends
	// at once when the Run is stopped.
	EffWaitRetry EffectKind = "wait_retry"
	// EffCompact makes the candidate of a compaction; EffCommitCheckpoint stores it.
	EffCompact          EffectKind = "compact"
	EffCommitCheckpoint EffectKind = "commit_checkpoint"
)

// ReasonEarlierCallFailed is why the calls after the one that did not succeed are not
// run (Effect.Reason of EffPersistObservation).
const ReasonEarlierCallFailed = "earlier_call_failed"

// Effect is one request to the driver. PersistResult carries the Outcome to store;
// PersistObservation carries, when it follows a call that did not succeed, why the rest
// of the response is not run. WaitRetry carries the retry that was scheduled.
type Effect struct {
	Kind    EffectKind
	Outcome *Outcome
	Reason  string
	// Retry is the decision EffWaitRetry carries out.
	Retry *RetryDecision
}

// Transition is the machine: given the Run's state and an event it returns the next
// state and the effects the driver must carry out. It is total over the path and
// returns an error, never a guess, for anything else: an event that does not belong
// to the phase, a Run that is Terminal, a phase this build does not run.
//
// A failure from any phase of the path does not end the Run on the spot: it sends the
// Run through PersistingResult, so that a Run ends in exactly one place, where its
// result and its run.terminal event are stored together.
func Transition(s State, ev Event) (State, []Effect, error) {
	switch {
	case !s.Phase.Valid():
		return s, nil, fmt.Errorf("%w: unknown phase", ErrUnreachablePhase)
	case s.Phase == PhaseTerminal:
		return s, nil, ErrTerminal
	case !s.Phase.Reachable():
		return s, nil, fmt.Errorf("%w: %s", ErrUnreachablePhase, s.Phase)
	}

	if ev.Kind == EvFailed {
		if s.Phase == PhasePersistingResult {
			// The result could not be stored: the machine cannot end the Run by
			// itself, and says so instead of pretending to.
			return s, nil, fmt.Errorf("%w: a failure while the result is being stored", ErrInvalidTransition)
		}
		return failTo(s, ev.Failure), []Effect{{Kind: EffPersistResult}}, nil
	}

	switch s.Phase {
	case PhaseAdmitting:
		if ev.Kind == EvStart {
			return advance(s, ev, PhaseLoading, EffLoad)
		}
	case PhaseLoading:
		if ev.Kind == EvLoaded {
			return advance(s, ev, PhaseAssembling, EffAssemble)
		}
	case PhaseAssembling:
		if ev.Kind == EvAssembled {
			return advance(s, ev, PhaseMeasuring, EffMeasure)
		}
	case PhaseMeasuring:
		if ev.Kind == EvMeasured {
			return afterMeasure(s, ev)
		}
	case PhaseGenerating:
		if ev.Kind == EvGenerated {
			return afterGeneration(s, ev)
		}
	case PhaseRetryWaiting:
		if ev.Kind == EvRetryDue {
			s.Retry = nil
			return advance(s, ev, PhaseMeasuring, EffMeasure)
		}
	case PhaseCompacting:
		switch ev.Kind {
		case EvCompactReady:
			s.GenerationAttemptsUsed += ev.Compact.Generations
			s.CompactUnknown = ev.Compact.Unknown
			return advance(s, ev, PhaseCommittingCheckpoint, EffCommitCheckpoint)
		case EvCompactSkipped:
			if !s.Early {
				// Only a compaction that was started early may be skipped: a prompt that does not
				// fit, and a manual compaction, have no prompt to go on with.
				break
			}
			// The prompt still fits (it was counted), and no checkpoint came of the compaction:
			// the Run goes on with it. What the stages spent is counted, and the trigger is not
			// tried again in this Run.
			s.GenerationAttemptsUsed += ev.Compact.Generations
			s.Early, s.EarlyTried = false, true
			return startGeneration(s, ev)
		}
	case PhaseCommittingCheckpoint:
		switch ev.Kind {
		case EvCommitted:
			if s.CompactUnknown {
				// The checkpoint is stored. A generation of this Run is unresolved, so the Run
				// does not generate again: it ends with that outcome unknown, resumable.
				return failTo(s, Failure{Code: modelport.CodeOutcomeUnknown, GenerationState: modelport.StateUnknown}), []Effect{{Kind: EffPersistResult}}, nil
			}
			if s.Manual {
				// A manual compaction is done: it ends completed, with the checkpoint it
				// committed named by the result. Completed says only that the compaction was
				// made; it is not a Task's completion (a system Run has no Task meaning).
				done := Outcome{Status: StatusCompleted, Code: CodeCompactionCommitted, Resumable: false}
				s.Phase, s.Pending = PhasePersistingResult, &done
				return s, []Effect{{Kind: EffPersistResult}}, nil
			}
			s.Compacted, s.CompactStale, s.Attempt, s.Early = true, 0, 0, false
			return advance(s, ev, PhaseAssembling, EffAssemble)
		case EvCommitStale:
			// What the candidate was made against moved. It is not committed. The step is
			// prepared again from the new context, with its prompt counted again, and one
			// more candidate may be made from that; a second stale one ends the Run, unless
			// the compaction was an early one, which a prompt that fits can do without.
			if s.Early {
				s.Early, s.EarlyTried, s.Compacted, s.Attempt = false, true, false, 0
				return advance(s, ev, PhaseAssembling, EffAssemble)
			}
			if s.CompactStale++; s.CompactStale > 1 {
				return failTo(s, Failure{Code: CodeCompactionStale}), []Effect{{Kind: EffPersistResult}}, nil
			}
			s.Compacted, s.Attempt = false, 0
			return advance(s, ev, PhaseAssembling, EffAssemble)
		}
	case PhasePreparingAction:
		if ev.Kind == EvPrepared {
			return advance(s, ev, PhaseExecuting, EffExecuteTool)
		}
	case PhaseExecuting:
		if ev.Kind == EvToolSettled {
			return afterTool(s, ev)
		}
	case PhasePersistingObservation:
		if ev.Kind == EvObserved {
			// The exchange is part of the context: what comes next is a new act Action,
			// and its first Attempt.
			s.ToolTotal, s.ToolNext, s.Attempt = 0, 0, 0
			s.Compacted, s.CompactStale = false, 0
			return advance(s, ev, PhaseAssembling, EffAssemble)
		}
	case PhaseValidatingFinal:
		if ev.Kind == EvFinalValid {
			done := Outcome{Status: StatusCompleted, Code: CodeFinalAccepted, Resumable: Resumable(StatusCompleted, CodeFinalAccepted)}
			s.Phase, s.Pending = PhasePersistingResult, &done
			return s, []Effect{{Kind: EffPersistResult}}, nil
		}
	case PhasePersistingResult:
		if ev.Kind == EvPersisted {
			s.Phase = PhaseTerminal
			return s, nil, nil
		}
	}
	return s, nil, fmt.Errorf("%w: %s in %s", ErrInvalidTransition, ev.Kind, s.Phase)
}

// failTo sends the Run to PersistingResult with the Outcome the failure classifies as.
func failTo(s State, f Failure) State {
	o := ClassifyFailure(f)
	s.Phase, s.Pending = PhasePersistingResult, &o
	return s
}

// advance moves to the next phase of the path, unless the Run's deadline has already
// passed, which ends it instead.
func advance(s State, ev Event, next Phase, eff EffectKind) (State, []Effect, error) {
	if !ev.At.Before(s.Deadline) {
		return failTo(s, Failure{Code: CodeDeadlineExceeded}), []Effect{{Kind: EffPersistResult}}, nil
	}
	s.Phase = next
	return s, []Effect{{Kind: eff}}, nil
}

// afterMeasure decides, from the capacity verdict, the deadline and the Run's
// budgets, whether a generation may be sent. Only a verified fit may; a no-fit ends
// the Run blocked for capacity, and a count that is not verified (an estimate, or an
// interval across the budget with no exact recount) ends it blocked as unverified:
// never a guess in either direction. The same holds for a retry, whose final request
// was counted again: it is sent only if that count fits, and it spends the Run's
// generation attempts but not a model step.
func afterMeasure(s State, ev Event) (State, []Effect, error) {
	if !ev.At.Before(s.Deadline) {
		return failTo(s, Failure{Code: CodeDeadlineExceeded}), []Effect{{Kind: EffPersistResult}}, nil
	}
	switch ev.Fit {
	case contextplan.VerdictFit:
		if s.Manual {
			// The operator asked for the compaction: a prompt that fits is compacted too.
			return advance(s, ev, PhaseCompacting, EffCompact)
		}
		// A prompt that fits and has reached the trigger ratio is compacted before it is
		// full, once for the step and until one ended without a checkpoint. A retried Attempt
		// is never compacted (its request is the one that failed, counted again).
		if s.Compaction && ev.Early && s.Attempt == 0 && !s.Compacted && !s.EarlyTried {
			s.Early = true
			return advance(s, ev, PhaseCompacting, EffCompact)
		}
	case contextplan.VerdictNoFit:
		// A prompt that does not fit is first compacted, once for the step. A retried
		// Attempt is never compacted: its request is the one that failed, counted again.
		if s.Manual || (s.Compaction && s.Attempt == 0 && !s.Compacted) {
			s.Early = false
			return advance(s, ev, PhaseCompacting, EffCompact)
		}
		return failTo(s, Failure{Code: CodeCapacityBlocked}), []Effect{{Kind: EffPersistResult}}, nil
	case contextplan.VerdictStraddle, contextplan.VerdictUnverified:
		return failTo(s, Failure{Code: modelport.CodeBudgetUnverified}), []Effect{{Kind: EffPersistResult}}, nil
	default:
		return s, nil, fmt.Errorf("%w: a capacity verdict this machine does not know", ErrInvalidTransition)
	}
	return startGeneration(s, ev)
}

// startGeneration is the step from a measured prompt that may be sent to its generation:
// the Run's budgets and deadline are looked at once more, and the step and the attempt are
// counted as the machine sends the Run to generate.
func startGeneration(s State, ev Event) (State, []Effect, error) {
	if !ev.At.Before(s.Deadline) {
		return failTo(s, Failure{Code: CodeDeadlineExceeded}), []Effect{{Kind: EffPersistResult}}, nil
	}
	if s.Attempt == 0 && s.ModelStepsUsed >= s.Limits.MaxModelSteps {
		return failTo(s, Failure{Code: CodeStepBudgetExhausted}), []Effect{{Kind: EffPersistResult}}, nil
	}
	if s.GenerationAttemptsUsed >= s.Limits.MaxGenerationAttempts {
		return failTo(s, Failure{Code: CodeGenerationBudgetExhausted}), []Effect{{Kind: EffPersistResult}}, nil
	}
	s.Phase = PhaseGenerating
	s.Compacted = false
	if s.Attempt == 0 {
		s.ModelStepsUsed++ // a retry is the same step
	}
	s.GenerationAttemptsUsed++
	return s, []Effect{{Kind: EffGenerate}}, nil
}

func afterGeneration(s State, ev Event) (State, []Effect, error) {
	j := Judge(ev.Gen)
	if !j.Usable {
		// F31: a failed first Attempt of an act Action may be retried once, with the
		// recovery profile the table names, after the backoff it names. Anything else, and
		// the second Attempt's own failure, ends the Run as the failure classifies.
		if s.Attempt == 0 {
			d := PlanModelRetry(RetryInput{
				Stage: modelport.StageAct, Failure: j.Failure, Ordinal: s.Attempt, AttemptsUsed: s.GenerationAttemptsUsed,
				MaxAttempts: s.Limits.MaxGenerationAttempts, Now: ev.At, Deadline: s.Deadline, RetryAfterMS: ev.Gen.RetryAfterMS, Offers: ev.Gen.Offers,
			})
			if d.Allow {
				s.Phase, s.Attempt, s.Retry = PhaseRetryWaiting, s.Attempt+1, &d
				return s, []Effect{{Kind: EffWaitRetry, Retry: &d}}, nil
			}
		}
		return failTo(s, j.Failure), []Effect{{Kind: EffPersistResult}}, nil
	}
	if j.Tools {
		// The response asks for Tool calls: they are bound to Actions, then run one at a
		// time in the order the model gave them.
		s.Phase, s.ToolTotal, s.ToolNext = PhasePreparingAction, ev.Gen.ToolCalls, 0
		return s, []Effect{{Kind: EffPrepareBatch}}, nil
	}
	s.Phase = PhaseValidatingFinal
	return s, []Effect{{Kind: EffValidateFinal}}, nil
}

// afterTool decides what follows one call. A failure the driver attached ends the Run.
// A call that completed is followed by the next call of the response, or, after the
// last, by the observation. A call that did not (it failed, or the policy refused it)
// stops the rest of the response: the later calls were written before the model knew
// this one would not do what it asked, so they are not run, and the exchange is closed
// with the reason. A call that was cancelled or whose effect is unknown ends the Run.
func afterTool(s State, ev Event) (State, []Effect, error) {
	t := ev.Tool
	if t.Failure != nil {
		return failTo(s, *t.Failure), []Effect{{Kind: EffPersistResult}}, nil
	}
	switch t.Effect {
	case "completed":
		if s.ToolNext+1 < s.ToolTotal {
			s.ToolNext++
			return advance(s, ev, PhaseExecuting, EffExecuteTool)
		}
		return advance(s, ev, PhasePersistingObservation, EffPersistObservation)
	case "failed", "not_started":
		next, effects, err := advance(s, ev, PhasePersistingObservation, EffPersistObservation)
		if err == nil && s.ToolNext+1 < s.ToolTotal && len(effects) == 1 && effects[0].Kind == EffPersistObservation {
			effects[0].Reason = ReasonEarlierCallFailed
		}
		return next, effects, err
	case "cancelled":
		return failTo(s, Failure{Code: CodeDriverStopped}), []Effect{{Kind: EffPersistResult}}, nil
	case "unknown":
		return failTo(s, Failure{Code: CodeEffectOutcomeUnknown}), []Effect{{Kind: EffPersistResult}}, nil
	}
	return s, nil, fmt.Errorf("%w: a Tool call that ended in a state this machine does not know", ErrInvalidTransition)
}
