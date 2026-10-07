package kernel

import (
	"sort"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

// Codes the kernel itself gives a Run's result. The model's normalized codes are in
// package modelport; a result takes one of these or, for a model contract refusal
// that is its own contract code, that code.
const (
	CodeFinalAccepted             = "FINAL_RESPONSE_ACCEPTED"
	CodeDeadlineExceeded          = "DEADLINE_EXCEEDED"
	CodeStepBudgetExhausted       = "STEP_BUDGET_EXHAUSTED"
	CodeGenerationBudgetExhausted = "GENERATION_BUDGET_EXHAUSTED"
	CodeDriverStopped             = "DRIVER_STOPPED"
	CodeCapacityBlocked           = "CAPACITY_BLOCKED"
	CodeIntegrityBlocked          = "INTEGRITY_BLOCKED"
	CodePersistenceUncertain      = "PERSISTENCE_UNCERTAIN"
	// CodeCompactionStale: a compaction's candidate was found stale twice in one step.
	CodeCompactionStale = "COMPACTION_STALE"
	// CodeCompactionCommitted is the code of a manual compaction's Run that committed its
	// checkpoint: a system Run's own success, which says nothing of any Task's work.
	CodeCompactionCommitted  = "COMPACTION_COMMITTED"
	CodeInternalError        = "INTERNAL_ERROR"
	CodeModelOutputInvalid   = "MODEL_OUTPUT_INVALID"
	CodeModelTransportFailed = "MODEL_TRANSPORT_FAILED"
	CodeModelTemporarilyDown = "MODEL_TEMPORARILY_UNAVAILABLE"
	CodeModelOutputTruncated = "MODEL_OUTPUT_TRUNCATED"
	CodeModelRefused         = "MODEL_REFUSED"
	// The Tool runtime's own ends of a Run.
	CodeEffectOutcomeUnknown = "EFFECT_OUTCOME_UNKNOWN"
	CodePolicyAmbiguous      = "POLICY_AMBIGUOUS"
	CodePolicyChanged        = "POLICY_CHANGED"
	CodeWorkspaceBusy        = "WORKSPACE_BUSY"
	// The host's fixed hook before a call denied it, or did not answer validly in time
	// (HOST_ASSETS section 4). internal/extensions spells them the same.
	CodeHostHookDenied = "HOST_HOOK_DENIED"
	CodeHostHookFailed = "HOST_HOOK_FAILED"
)

// Failure is something that went wrong, in the terms the classification needs: a
// normalized code, and what is known about the generation it concerns. GenerationState
// is "" when no generation is involved (a failure before one was dispatched), or one of
// the three states of ERROR_MAPPING.
type Failure struct {
	Code            string
	GenerationState string
	// HiddenRetry is set when the model side admitted a retry the contract forbids.
	HiddenRetry bool
}

// Outcome is how a failed or finished Run ends: its status and code, whether a new
// Run on the same Task may continue it, and whether a generation of this Run is
// left unresolved (its action is then listed in the result).
type Outcome struct {
	Status                string
	Code                  string
	Resumable             bool
	UnresolvedModelAction bool
}

// rule is one row of the classification: the status and code a normalized code gives
// a Run that cannot go on.
type rule struct{ status, code string }

// classification is ERROR_MAPPING section 3 and RETRY_CONTRACT section 3 ("the Run's
// result when it cannot be retried"), with the codes of this build's own limits. A row is
// where a Run ends when F31 does not retry its failure (see PlanModelRetry) and when the
// retry, the second Attempt of the Action, failed too: a failure is classified the same
// whichever Attempt it was.
var classification = map[string]rule{
	// Output the model gave that cannot be used: the Run failed, nothing was adopted.
	modelport.CodeReasoningOnly:       {StatusFailed, CodeModelOutputInvalid},
	modelport.CodeEmptyFinalContent:   {StatusFailed, CodeModelOutputInvalid},
	modelport.CodeRawToolMarkup:       {StatusFailed, CodeModelOutputInvalid},
	modelport.CodeOutputSchemaInvalid: {StatusFailed, CodeModelOutputInvalid},
	modelport.CodeOutputDegenerate:    {StatusFailed, modelport.CodeOutputDegenerate},
	// The model side was not reachable or not ready.
	modelport.CodeConnectFailed:     {StatusFailed, CodeModelTransportFailed},
	modelport.CodeUpstreamTransient: {StatusFailed, CodeModelTransportFailed},
	modelport.CodeRateLimited:       {StatusIncomplete, CodeModelTemporarilyDown},
	modelport.CodeQueueTimeout:      {StatusIncomplete, CodeModelTemporarilyDown},
	modelport.CodeModelUnavailable:  {StatusBlocked, modelport.CodeModelUnavailable},
	// The request did not fit, or its fit could not be established.
	modelport.CodeContextLimit:     {StatusBlocked, CodeCapacityBlocked},
	CodeCapacityBlocked:            {StatusBlocked, CodeCapacityBlocked},
	modelport.CodeBudgetUnverified: {StatusBlocked, modelport.CodeBudgetUnverified},
	// The output stopped early.
	modelport.CodeLength:     {StatusIncomplete, CodeModelOutputTruncated},
	modelport.CodeIncomplete: {StatusIncomplete, CodeModelOutputTruncated},
	// A refusal is final: no retry is built to get around it.
	modelport.CodeRefused: {StatusRejected, CodeModelRefused},
	// The contract cannot be met: stop, never work around it.
	modelport.CodeBindingChanged:        {StatusBlocked, modelport.CodeBindingChanged},
	modelport.CodeUnsupportedContract:   {StatusBlocked, modelport.CodeUnsupportedContract},
	modelport.CodeUnsupportedRecovery:   {StatusBlocked, modelport.CodeUnsupportedRecovery},
	modelport.CodeAuthFailed:            {StatusBlocked, modelport.CodeAuthFailed},
	modelport.CodeInputDigestMismatch:   {StatusBlocked, modelport.CodeInputDigestMismatch},
	modelport.CodeRequestDigestMismatch: {StatusBlocked, modelport.CodeRequestDigestMismatch},
	modelport.CodeContractFailed:        {StatusFailed, modelport.CodeContractFailed},
	// Control and limits of the Run itself.
	modelport.CodeCancelled:       {StatusCancelled, modelport.CodeCancelled},
	modelport.CodePermitRevoked:   {StatusCancelled, modelport.CodePermitRevoked},
	CodeDeadlineExceeded:          {StatusIncomplete, CodeDeadlineExceeded},
	CodeStepBudgetExhausted:       {StatusIncomplete, CodeStepBudgetExhausted},
	CodeGenerationBudgetExhausted: {StatusIncomplete, CodeGenerationBudgetExhausted},
	CodeDriverStopped:             {StatusIncomplete, CodeDriverStopped},
	// The Tool runtime: a call whose effect cannot be established, a policy that is not
	// one a Run can use (ambiguous, or no longer the one it froze), a workspace another
	// Run holds. Each stops the Run where it stands; none is worked around.
	CodeEffectOutcomeUnknown: {StatusBlocked, CodeEffectOutcomeUnknown},
	CodePolicyAmbiguous:      {StatusBlocked, CodePolicyAmbiguous},
	CodePolicyChanged:        {StatusBlocked, CodePolicyChanged},
	CodeWorkspaceBusy:        {StatusBlocked, CodeWorkspaceBusy},
	// The host's fixed hook before a call denied it, or did not answer validly in time
	// (HOST_ASSETS section 4): a host decision, blocked and not a failure of the work.
	CodeHostHookDenied: {StatusBlocked, CodeHostHookDenied},
	CodeHostHookFailed: {StatusBlocked, CodeHostHookFailed},
	// The Harness's own state.
	CodeIntegrityBlocked:     {StatusBlocked, CodeIntegrityBlocked},
	CodePersistenceUncertain: {StatusRestartRequired, CodePersistenceUncertain},
	CodeInternalError:        {StatusFailed, CodeInternalError},
	CodeCompactionStale:      {StatusIncomplete, CodeCompactionStale},
}

// ClassifiedCodes lists the codes ClassifyFailure has a row for.
func ClassifiedCodes() []string {
	out := make([]string, 0, len(classification))
	for c := range classification {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// ClassifyFailure (F19) is the one place a failure becomes the end of a Run. It is
// pure and has a fixed order of precedence:
//
//  1. a cancellation is a cancellation (a generation still unresolved is listed);
//  2. a hidden retry means the generation count cannot be trusted: blocked, as a
//     contract failure;
//  3. a generation whose end is unknown makes the Run blocked with
//     MODEL_GENERATION_OUTCOME_UNKNOWN, whatever else went wrong: it is never
//     reported as the failure it might have been, never retried, and its action is
//     left unresolved for the next Run to settle;
//  4. otherwise the code's row. A code with no row is a model contract failure: the
//     text of an error is never used to find a more flattering code.
func ClassifyFailure(f Failure) Outcome {
	unknown := f.GenerationState == modelport.StateUnknown
	out := func(status, code string) Outcome {
		return Outcome{Status: status, Code: code, Resumable: Resumable(status, code), UnresolvedModelAction: unknown}
	}
	switch {
	case f.Code == modelport.CodeCancelled || f.Code == modelport.CodePermitRevoked:
		return out(StatusCancelled, f.Code)
	case f.HiddenRetry:
		return out(StatusBlocked, modelport.CodeContractFailed)
	case unknown:
		return out(StatusBlocked, modelport.CodeOutcomeUnknown)
	}
	if r, ok := classification[f.Code]; ok {
		return out(r.status, r.code)
	}
	return out(StatusFailed, modelport.CodeContractFailed)
}

// Resumable says whether a new Run on the same Task may carry on from where this one
// ended. It is a statement about the Run's end, not a promise that the next Run
// succeeds: a Run that completed has nothing to resume; a refusal is not resumed
// around; a broken integrity needs repair first. Everything else (a limit reached, a
// failure, a block that someone can lift, a cancellation, a restart) leaves a Task
// that a new Run can continue, under new limits and, where the block was the
// binding, an explicit new binding.
func Resumable(status, code string) bool {
	switch status {
	case StatusCompleted, StatusRejected:
		return false
	case StatusBlocked:
		return code != CodeIntegrityBlocked
	}
	return true
}

// Generated is what the kernel is told about a finished generation attempt.
type Generated struct {
	// Kind is the completion's terminal kind (modelport.Kind*).
	Kind            string
	FailureCode     string
	GenerationState string
	HiddenRetry     bool
	// ToolsOffered says the request declared Tools, and ToolCalls is how many valid
	// calls a tool_calls completion carries. Without offered Tools a Tool call is a
	// contract failure; calls that did not validate are not counted here, the driver
	// having turned their completion into the error it is.
	ToolsOffered bool
	ToolCalls    int
	// Offers and RetryAfterMS are what the driver knows for F31: which recovery profiles
	// may be used for this failure, and the wait the model side asked for (0: none).
	Offers       RecoveryOffers
	RetryAfterMS int64
}

// Judgement is what a finished generation amounts to for the Run, and how the
// attempt is recorded.
type Judgement struct {
	// Usable: the generation gave something the Run may act on: a final answer or Tool
	// calls to run (Tools).
	Usable bool
	// Tools: what the generation gave is Tool calls, not an answer.
	Tools bool
	// Outcome and FailureCode are the attempt's model.completed outcome and code.
	Outcome     string
	FailureCode string
	// Failure is how the Run ends when the generation is not usable.
	Failure Failure
}

// Judge decides what a completed generation means. A response that asks for Tools is
// usable only when the request offered Tools and the calls were valid; where it offered
// none, the model broke the request's contract, nothing is dispatched and the Run
// fails. The same rule records the attempt, so the stored attempt and the Run agree.
func Judge(g Generated) Judgement {
	fail := func(outcome, code string) Judgement {
		return Judgement{Outcome: outcome, FailureCode: code, Failure: Failure{Code: code, GenerationState: g.GenerationState, HiddenRetry: g.HiddenRetry}}
	}
	switch g.Kind {
	case modelport.KindFinal:
		if g.GenerationState != modelport.StateTerminal || g.HiddenRetry {
			return fail("error", modelport.CodeContractFailed)
		}
		return Judgement{Usable: true, Outcome: "completed"}
	case modelport.KindToolCalls:
		if !g.ToolsOffered || g.ToolCalls < 1 || g.GenerationState != modelport.StateTerminal || g.HiddenRetry {
			return fail("error", modelport.CodeContractFailed)
		}
		return Judgement{Usable: true, Tools: true, Outcome: "completed"}
	case modelport.KindIncomplete:
		return fail("incomplete", g.FailureCode)
	case modelport.KindRefused:
		return fail("refused", modelport.CodeRefused)
	case modelport.KindError:
		return fail("error", g.FailureCode)
	}
	return fail("error", modelport.CodeContractFailed)
}

// OrphanOutcome is how a Run ends that its driver left behind (the process stopped, or
// another driver took the Thread over), decided from the facts that survived: how many
// of its generation attempts were dispatched and never recorded an end, the instant of
// the decision and the Run's deadline. It is the same classification a driver applies
// to itself when it is told to stop, so the two ways a Run can lose its driver end the
// same way:
//
//   - a generation that may have run (the end was never recorded) leaves the Run
//     blocked with the generation's outcome unknown, however late the deadline is;
//   - a Tool call that was dispatched and never recorded an end leaves it blocked with
//     the call's effect unknown (EFFECT_OUTCOME_UNKNOWN): the call is not run again to
//     find out;
//   - otherwise nothing was dispatched that is unaccounted for: the Run ended at its
//     deadline if that has passed, and was stopped with its driver if not.
//
// Either way the Run is incomplete or blocked, never completed and never failed, and
// a new Run on the same Task may continue it.
func OrphanOutcome(unresolvedAttempts, unresolvedTools int, now, deadline time.Time) Outcome {
	return OrphanOutcomeCancelled(unresolvedAttempts, unresolvedTools, false, now, deadline)
}

// OrphanOutcomeCancelled is OrphanOutcome for a Run whose stop may have been requested
// (cancelRequested: a control.cancel_requested names it) while nobody was driving it.
// The request is a request and no more: nobody stopped anything, so what the Run left
// unknown still decides its end (a generation or a Tool call that may have taken effect
// is never reported as a cancellation), and only a Run with nothing unresolved ends as
// the cancelled Run the caller asked for. A request outranks the deadline and a stopped
// driver, which are not why the Run ended.
func OrphanOutcomeCancelled(unresolvedAttempts, unresolvedTools int, cancelRequested bool, now, deadline time.Time) Outcome {
	switch {
	case unresolvedAttempts > 0:
		return ClassifyFailure(Failure{Code: CodeDriverStopped, GenerationState: modelport.StateUnknown})
	case unresolvedTools > 0:
		return ClassifyFailure(Failure{Code: CodeEffectOutcomeUnknown})
	case cancelRequested:
		return ClassifyFailure(Failure{Code: modelport.CodeCancelled})
	case !now.Before(deadline):
		return ClassifyFailure(Failure{Code: CodeDeadlineExceeded})
	}
	return ClassifyFailure(Failure{Code: CodeDriverStopped})
}
