package kernel_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/kernel"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

var bothOffered = kernel.RecoveryOffers{SameRequest: kernel.OfferAllowed, TerminalOutputOnce: kernel.OfferAllowed}
var sameOnly = kernel.RecoveryOffers{SameRequest: kernel.OfferAllowed, TerminalOutputOnce: kernel.OfferNotSupported}

func retryInput(code, state string, offers kernel.RecoveryOffers) kernel.RetryInput {
	return kernel.RetryInput{
		Stage: modelport.StageAct, Failure: kernel.Failure{Code: code, GenerationState: state}, Ordinal: 0,
		AttemptsUsed: 1, MaxAttempts: 32, Now: t0, Deadline: deadline, Offers: offers,
	}
}

// TestPlanModelRetryIsTheTableOfRetryContractSection3 is RETRY_CONTRACT section 3, row
// by row: which failures are retried, with which profile, after how long, and which are
// not retried at all.
func TestPlanModelRetryIsTheTableOfRetryContractSection3(t *testing.T) {
	const (
		ns, term, unk = modelport.StateNotStarted, modelport.StateTerminal, modelport.StateUnknown
		same, once    = modelport.ProfileSameRequest, modelport.ProfileTerminalOutputOnce
	)
	type row struct {
		name    string
		code    string
		state   string
		offers  kernel.RecoveryOffers
		retry   bool
		profile string
		delayMS int64
		denied  string
	}
	rows := []row{
		{"reasoning only uses terminal_output_once when it is offered", modelport.CodeReasoningOnly, term, bothOffered, true, once, 0, ""},
		{"empty final content", modelport.CodeEmptyFinalContent, term, bothOffered, true, once, 0, ""},
		{"raw tool markup", modelport.CodeRawToolMarkup, term, bothOffered, true, once, 0, ""},
		{"an output that does not satisfy the schema", modelport.CodeOutputSchemaInvalid, term, bothOffered, true, once, 0, ""},
		{"reasoning only falls back to the same request", modelport.CodeReasoningOnly, term, sameOnly, true, same, 0, ""},
		{"a format failure of an unknown generation is not retried", modelport.CodeRawToolMarkup, unk, bothOffered, false, "", 0, kernel.DeniedState},
		{"a format failure with no profile offered", modelport.CodeReasoningOnly, term, kernel.RecoveryOffers{}, false, "", 0, kernel.DeniedNoProfile},
		{"connect failed", modelport.CodeConnectFailed, ns, bothOffered, true, same, 1000, ""},
		{"connect failed after a finished generation is accepted too", modelport.CodeConnectFailed, term, bothOffered, true, same, 1000, ""},
		{"upstream transient", modelport.CodeUpstreamTransient, term, bothOffered, true, same, 1000, ""},
		{"upstream transient of an unknown generation", modelport.CodeUpstreamTransient, unk, bothOffered, false, "", 0, kernel.DeniedState},
		{"rate limited", modelport.CodeRateLimited, ns, bothOffered, true, same, 1000, ""},
		{"queue timeout", modelport.CodeQueueTimeout, ns, bothOffered, true, same, 1000, ""},
		{"rate limited needs a generation that never started", modelport.CodeRateLimited, term, bothOffered, false, "", 0, kernel.DeniedState},
		{"a transient code with the same request not offered", modelport.CodeConnectFailed, ns, kernel.RecoveryOffers{TerminalOutputOnce: kernel.OfferAllowed}, false, "", 0, kernel.DeniedNoProfile},
		{"the context limit is never retried with the same payload", modelport.CodeContextLimit, ns, bothOffered, false, "", 0, kernel.DeniedCode},
		{"length", modelport.CodeLength, term, bothOffered, false, "", 0, kernel.DeniedCode},
		{"incomplete", modelport.CodeIncomplete, term, bothOffered, false, "", 0, kernel.DeniedCode},
		{"a refusal", modelport.CodeRefused, term, bothOffered, false, "", 0, kernel.DeniedCode},
		{"the binding changed", modelport.CodeBindingChanged, ns, bothOffered, false, "", 0, kernel.DeniedCode},
		{"an unsupported contract", modelport.CodeUnsupportedContract, ns, bothOffered, false, "", 0, kernel.DeniedCode},
		{"authentication", modelport.CodeAuthFailed, ns, bothOffered, false, "", 0, kernel.DeniedCode},
		{"an unknown outcome", modelport.CodeOutcomeUnknown, unk, bothOffered, false, "", 0, kernel.DeniedState},
		{"a degenerate output", modelport.CodeOutputDegenerate, term, bothOffered, false, "", 0, kernel.DeniedCode},
		{"a contract failure", modelport.CodeContractFailed, term, bothOffered, false, "", 0, kernel.DeniedCode},
		{"a model that is not available", modelport.CodeModelUnavailable, ns, bothOffered, false, "", 0, kernel.DeniedCode},
		{"a cancellation", modelport.CodeCancelled, "", bothOffered, false, "", 0, kernel.DeniedCode},
		{"a revoked permit", modelport.CodePermitRevoked, "", bothOffered, false, "", 0, kernel.DeniedCode},
		{"a code the table does not list", "SOMETHING_ELSE", term, bothOffered, false, "", 0, kernel.DeniedCode},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			d := kernel.PlanModelRetry(retryInput(r.code, r.state, r.offers))
			if d.Allow != r.retry || d.Profile != r.profile || d.DelayMS != r.delayMS || d.Denied != r.denied {
				t.Fatalf("%+v", d)
			}
			if d.Allow && d.TriggerCode != r.code {
				t.Fatalf("the retry names another trigger: %+v", d)
			}
		})
	}
}

// TestPlanModelRetryAgreesWithTheDesignsErrorCases holds the decision to the design's own
// cases (examples/wire/error_cases.json): for each normalized code and generation
// state of the table, whether it is retried.
func TestPlanModelRetryAgreesWithTheDesignsErrorCases(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contract", "examples", "wire", "error_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []struct {
			SourceCode    string `json:"source_code"`
			Proof         string `json:"proof"`
			ExpectedCode  string `json:"expected_code"`
			ExpectedState string `json:"expected_generation_state"`
			ExpectedRetry bool   `json:"expected_retry"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) < 10 {
		t.Fatalf("%d cases", len(doc.Cases))
	}
	for _, c := range doc.Cases {
		t.Run(c.SourceCode+"/"+c.Proof, func(t *testing.T) {
			d := kernel.PlanModelRetry(retryInput(c.ExpectedCode, c.ExpectedState, bothOffered))
			if d.Allow != c.ExpectedRetry {
				t.Fatalf("%s %s: retry=%t (%s), the design says %t", c.ExpectedCode, c.ExpectedState, d.Allow, d.Denied, c.ExpectedRetry)
			}
		})
	}
}

// TestRetryIsRefusedWhenItsPreconditionsFail: the second Attempt is never retried, a
// hidden retry is a contract failure, and a retry needs an attempt left in the budget and
// a backoff that ends before the deadline.
func TestRetryIsRefusedWhenItsPreconditionsFail(t *testing.T) {
	base := retryInput(modelport.CodeReasoningOnly, modelport.StateTerminal, bothOffered)
	if d := kernel.PlanModelRetry(base); !d.Allow {
		t.Fatalf("the baseline is retried: %+v", d)
	}
	for _, tc := range []struct {
		name   string
		edit   func(*kernel.RetryInput)
		denied string
	}{
		{"the second attempt", func(in *kernel.RetryInput) { in.Ordinal = 1 }, kernel.DeniedAttemptLimit},
		{"another stage", func(in *kernel.RetryInput) { in.Stage = modelport.StageSummary }, kernel.DeniedStage},
		{"a selection stage", func(in *kernel.RetryInput) { in.Stage = modelport.StageSelection }, kernel.DeniedStage},
		{"a hidden retry", func(in *kernel.RetryInput) { in.Failure.HiddenRetry = true }, kernel.DeniedHiddenRetry},
		{"the last attempt of the budget is spent", func(in *kernel.RetryInput) { in.AttemptsUsed = 32 }, kernel.DeniedAttemptBudget},
		{"a budget of one", func(in *kernel.RetryInput) { in.MaxAttempts = 1 }, kernel.DeniedAttemptBudget},
		{"the deadline has passed", func(in *kernel.RetryInput) { in.Now = deadline }, kernel.DeniedDeadline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.edit(&in)
			if d := kernel.PlanModelRetry(in); d.Allow || d.Denied != tc.denied || d.Profile != "" {
				t.Fatalf("%+v", d)
			}
		})
	}

	// The attempt just before the cap is retried, the cap itself is not.
	in := base
	in.AttemptsUsed, in.MaxAttempts = 31, 32
	if !kernel.PlanModelRetry(in).Allow {
		t.Fatal("an attempt is left")
	}
	in.AttemptsUsed = 32
	if kernel.PlanModelRetry(in).Allow {
		t.Fatal("no attempt is left")
	}

	// A backoff that would end at the deadline is not waited for; one that ends before is.
	transient := retryInput(modelport.CodeUpstreamTransient, modelport.StateTerminal, bothOffered)
	transient.Now = deadline.Add(-1001 * time.Millisecond)
	if !kernel.PlanModelRetry(transient).Allow {
		t.Fatal("the wait ends before the deadline")
	}
	transient.Now = deadline.Add(-1000 * time.Millisecond)
	if d := kernel.PlanModelRetry(transient); d.Allow || d.Denied != kernel.DeniedDeadline {
		t.Fatalf("the wait ends at the deadline: %+v", d)
	}
	// A format failure waits for nothing, so it is retried up to the deadline itself.
	format := retryInput(modelport.CodeRawToolMarkup, modelport.StateTerminal, bothOffered)
	format.Now = deadline.Add(-time.Millisecond)
	if !kernel.PlanModelRetry(format).Allow {
		t.Fatal("no wait, and the deadline is still ahead")
	}
}

// TestTheBackoffOfRateLimitingFollowsWhatTheModelSideAsked: at least one second, what the
// model side asked for when it asked for more, and no retry when that is over ten seconds.
func TestTheBackoffOfRateLimitingFollowsWhatTheModelSideAsked(t *testing.T) {
	for _, tc := range []struct {
		afterMS int64
		retry   bool
		delayMS int64
	}{
		{0, true, 1000}, {400, true, 1000}, {1000, true, 1000}, {2500, true, 2500}, {10000, true, 10000}, {10001, false, 0}, {60000, false, 0},
	} {
		for _, code := range []string{modelport.CodeRateLimited, modelport.CodeQueueTimeout} {
			in := retryInput(code, modelport.StateNotStarted, bothOffered)
			in.RetryAfterMS = tc.afterMS
			d := kernel.PlanModelRetry(in)
			if d.Allow != tc.retry || d.DelayMS != tc.delayMS {
				t.Fatalf("%s retry_after=%d: %+v", code, tc.afterMS, d)
			}
			if !tc.retry && d.Denied != kernel.DeniedBackoffTooLong {
				t.Fatalf("%+v", d)
			}
		}
	}
}

// TestTheProfileOfAFormatRetryAndWhyTheOtherWasNotUsed: terminal_output_once when the
// host, the Run's policy and the binding all offer it; otherwise the same request, with
// the reason the first was not used.
func TestTheProfileOfAFormatRetryAndWhyTheOtherWasNotUsed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		once    kernel.Offer
		profile string
		from    string
		why     kernel.Offer
	}{
		{"offered", kernel.OfferAllowed, modelport.ProfileTerminalOutputOnce, "", ""},
		{"the policy does not allow it", kernel.OfferNotAllowed, modelport.ProfileSameRequest, modelport.ProfileTerminalOutputOnce, kernel.OfferNotAllowed},
		{"the binding does not publish it", kernel.OfferNotSupported, modelport.ProfileSameRequest, modelport.ProfileTerminalOutputOnce, kernel.OfferNotSupported},
		{"it does not answer this code", kernel.OfferCodeNotCovered, modelport.ProfileSameRequest, modelport.ProfileTerminalOutputOnce, kernel.OfferCodeNotCovered},
		{"nothing was said of it", "", modelport.ProfileSameRequest, modelport.ProfileTerminalOutputOnce, kernel.OfferNotSupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := kernel.PlanModelRetry(retryInput(modelport.CodeReasoningOnly, modelport.StateTerminal,
				kernel.RecoveryOffers{SameRequest: kernel.OfferAllowed, TerminalOutputOnce: tc.once}))
			if !d.Allow || d.Profile != tc.profile || d.FallbackFrom != tc.from || d.FallbackReason != tc.why {
				t.Fatalf("%+v", d)
			}
		})
	}
	// A transient failure never uses terminal_output_once, whatever is offered.
	d := kernel.PlanModelRetry(retryInput(modelport.CodeConnectFailed, modelport.StateNotStarted, bothOffered))
	if d.Profile != modelport.ProfileSameRequest || d.FallbackFrom != "" {
		t.Fatalf("%+v", d)
	}
}

// TestNoCombinationOfFactsRetriesWhatMustNotBeRetried walks every code, state, ordinal
// and offer the table has to do with, and checks the rules that hold whatever the
// rest says: an unknown generation, a hidden retry, a second Attempt and a failure
// outside the table are never retried, and a retry never asks for a profile that was not
// offered.
func TestNoCombinationOfFactsRetriesWhatMustNotBeRetried(t *testing.T) {
	codes := append(kernel.ClassifiedCodes(), "NOT_A_CODE")
	states := []string{"", modelport.StateNotStarted, modelport.StateTerminal, modelport.StateUnknown}
	offers := []kernel.Offer{"", kernel.OfferAllowed, kernel.OfferNotAllowed, kernel.OfferNotSupported, kernel.OfferCodeNotCovered}
	retriable := []string{
		modelport.CodeReasoningOnly, modelport.CodeEmptyFinalContent, modelport.CodeRawToolMarkup, modelport.CodeOutputSchemaInvalid,
		modelport.CodeConnectFailed, modelport.CodeUpstreamTransient, modelport.CodeRateLimited, modelport.CodeQueueTimeout,
	}
	for _, code := range codes {
		for _, state := range states {
			for _, same := range offers {
				for _, once := range offers {
					for _, ordinal := range []int64{0, 1} {
						for _, hidden := range []bool{false, true} {
							in := retryInput(code, state, kernel.RecoveryOffers{SameRequest: same, TerminalOutputOnce: once})
							in.Ordinal, in.Failure.HiddenRetry = ordinal, hidden
							d := kernel.PlanModelRetry(in)
							if !d.Allow {
								continue
							}
							switch {
							case state == modelport.StateUnknown || state == "":
								t.Fatalf("an unresolved generation was retried: %s %q", code, state)
							case hidden || ordinal != 0:
								t.Fatalf("hidden=%t ordinal=%d was retried: %s", hidden, ordinal, code)
							case !slices.Contains(retriable, code):
								t.Fatalf("%s is not in the table and was retried", code)
							case d.Profile == modelport.ProfileSameRequest && same != kernel.OfferAllowed,
								d.Profile == modelport.ProfileTerminalOutputOnce && once != kernel.OfferAllowed:
								t.Fatalf("a profile that was not offered was chosen: %+v", d)
							case d.Profile != modelport.ProfileSameRequest && d.Profile != modelport.ProfileTerminalOutputOnce:
								t.Fatalf("%+v", d)
							}
						}
					}
				}
			}
		}
	}
}

func generated(code, state string, offers kernel.RecoveryOffers) kernel.Event {
	ev := event(kernel.EvGenerated)
	ev.Gen = kernel.Generated{Kind: modelport.KindError, FailureCode: code, GenerationState: state, Offers: offers}
	return ev
}

// TestAFailedFirstAttemptWaitsThenIsMeasuredAndSentAgain is the machine's path of one
// retry: Generating, RetryWaiting (with the decision), Measuring, Generating. The retry
// spends a generation attempt and no model step, and it is the second and last Attempt.
func TestAFailedFirstAttemptWaitsThenIsMeasuredAndSentAgain(t *testing.T) {
	s := state(kernel.PhaseMeasuring)
	s.ModelStepsUsed, s.GenerationAttemptsUsed = 0, 0
	s, _ = run(t, s, event(kernel.EvMeasured))
	if s.Phase != kernel.PhaseGenerating || s.ModelStepsUsed != 1 || s.GenerationAttemptsUsed != 1 || s.Attempt != 0 {
		t.Fatalf("%+v", s)
	}
	s, effects := run(t, s, generated(modelport.CodeReasoningOnly, modelport.StateTerminal, bothOffered))
	if s.Phase != kernel.PhaseRetryWaiting || s.Attempt != 1 || s.Retry == nil || s.Retry.Profile != modelport.ProfileTerminalOutputOnce ||
		len(effects) != 1 || effects[0].Kind != kernel.EffWaitRetry || effects[0].Retry == nil || effects[0].Retry.TriggerCode != modelport.CodeReasoningOnly {
		t.Fatalf("%+v %+v", s, effects)
	}
	if s.ModelStepsUsed != 1 || s.GenerationAttemptsUsed != 1 {
		t.Fatalf("scheduling a retry spends nothing: %+v", s)
	}

	s, effects = run(t, s, event(kernel.EvRetryDue))
	if s.Phase != kernel.PhaseMeasuring || s.Retry != nil || !slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffMeasure}) {
		t.Fatalf("%+v %+v", s, effects)
	}
	s, effects = run(t, s, event(kernel.EvMeasured))
	if s.Phase != kernel.PhaseGenerating || s.ModelStepsUsed != 1 || s.GenerationAttemptsUsed != 2 || !slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffGenerate}) {
		t.Fatalf("a retry spends an attempt and not a step: %+v", s)
	}

	// The second Attempt that fails the same way is not retried: the Run ends as the
	// failure classifies.
	s, effects = run(t, s, generated(modelport.CodeReasoningOnly, modelport.StateTerminal, bothOffered))
	if s.Phase != kernel.PhasePersistingResult || s.Pending == nil || s.Pending.Status != kernel.StatusFailed || s.Pending.Code != kernel.CodeModelOutputInvalid ||
		!slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffPersistResult}) {
		t.Fatalf("%+v %+v", s, effects)
	}
}

// TestTheSecondAttemptsOtherFailuresEndTheRunAsTheyClassify: the retry's own failure is
// classified like any other (an unknown generation is blocked, a refusal rejected), and
// success after a retry is an ordinary final answer.
func TestTheSecondAttemptsOtherFailuresEndTheRunAsTheyClassify(t *testing.T) {
	retrying := func() kernel.State {
		s := state(kernel.PhaseGenerating)
		s.ModelStepsUsed, s.GenerationAttemptsUsed, s.Attempt = 1, 2, 1
		return s
	}
	for _, tc := range []struct {
		name   string
		ev     kernel.Event
		status string
		code   string
	}{
		{"unknown", generated(modelport.CodeOutcomeUnknown, modelport.StateUnknown, bothOffered), kernel.StatusBlocked, modelport.CodeOutcomeUnknown},
		{"transient again", generated(modelport.CodeUpstreamTransient, modelport.StateTerminal, bothOffered), kernel.StatusFailed, kernel.CodeModelTransportFailed},
		{"rate limited again", generated(modelport.CodeRateLimited, modelport.StateNotStarted, bothOffered), kernel.StatusIncomplete, kernel.CodeModelTemporarilyDown},
		{"length", generated(modelport.CodeLength, modelport.StateTerminal, bothOffered), kernel.StatusIncomplete, kernel.CodeModelOutputTruncated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := run(t, retrying(), tc.ev)
			if s.Phase != kernel.PhasePersistingResult || s.Pending.Status != tc.status || s.Pending.Code != tc.code {
				t.Fatalf("%+v", s.Pending)
			}
		})
	}
	s, _ := run(t, retrying(), event(kernel.EvGenerated))
	if s.Phase != kernel.PhaseValidatingFinal {
		t.Fatalf("a retry that answered is an answer: %+v", s)
	}
}

// TestARetryNeedsNoModelStepButNeedsAnAttempt: a Run on its last model step is retried
// (the retry is the same step), and a Run whose generation attempts end with the first
// Attempt is not (and ends as the failure classifies, not as an exhausted budget).
func TestARetryNeedsNoModelStepButNeedsAnAttempt(t *testing.T) {
	s := state(kernel.PhaseMeasuring)
	s.ModelStepsUsed, s.GenerationAttemptsUsed = limits.MaxModelSteps-1, 5
	s, _ = run(t, s, event(kernel.EvMeasured))
	if s.ModelStepsUsed != limits.MaxModelSteps {
		t.Fatalf("%+v", s)
	}
	s, _ = run(t, s, generated(modelport.CodeRawToolMarkup, modelport.StateTerminal, bothOffered))
	if s.Phase != kernel.PhaseRetryWaiting {
		t.Fatalf("the last step is retried: %+v", s)
	}
	s, _ = run(t, s, event(kernel.EvRetryDue), event(kernel.EvMeasured))
	if s.Phase != kernel.PhaseGenerating || s.ModelStepsUsed != limits.MaxModelSteps || s.GenerationAttemptsUsed != 7 {
		t.Fatalf("%+v", s)
	}

	// The cap is reached by the first Attempt: no retry, and the failure's own end.
	tight := kernel.NewState(protocolLimits(2), deadline, 0, 0)
	tight.Phase = kernel.PhaseMeasuring
	tight, _ = run(t, tight, event(kernel.EvMeasured))
	tight, _ = run(t, tight, generated(modelport.CodeReasoningOnly, modelport.StateTerminal, bothOffered))
	if tight.Phase != kernel.PhaseRetryWaiting {
		t.Fatalf("one attempt is left: %+v", tight)
	}
	one := kernel.NewState(protocolLimits(1), deadline, 0, 0)
	one.Phase = kernel.PhaseMeasuring
	one, _ = run(t, one, event(kernel.EvMeasured))
	one, _ = run(t, one, generated(modelport.CodeReasoningOnly, modelport.StateTerminal, bothOffered))
	if one.Phase != kernel.PhasePersistingResult || one.Pending.Code != kernel.CodeModelOutputInvalid {
		t.Fatalf("the budget is spent: %+v", one)
	}

	// A retry that finds the budget spent when it is measured ends the Run for it.
	late := state(kernel.PhaseMeasuring)
	late.Attempt, late.GenerationAttemptsUsed = 1, limits.MaxGenerationAttempts
	late, _ = run(t, late, event(kernel.EvMeasured))
	if late.Phase != kernel.PhasePersistingResult || late.Pending.Code != kernel.CodeGenerationBudgetExhausted {
		t.Fatalf("%+v", late.Pending)
	}
}

func protocolLimits(maxAttempts int64) protocol.Limits {
	l := limits
	l.MaxGenerationAttempts = maxAttempts
	return l
}

// TestARetryThatDoesNotFitIsNotSent: the retry's own count decides, as the first
// Attempt's did.
func TestARetryThatDoesNotFitIsNotSent(t *testing.T) {
	for _, tc := range []struct {
		verdict contextplan.Verdict
		code    string
	}{
		{contextplan.VerdictNoFit, kernel.CodeCapacityBlocked},
		{contextplan.VerdictStraddle, modelport.CodeBudgetUnverified},
		{contextplan.VerdictUnverified, modelport.CodeBudgetUnverified},
	} {
		s := state(kernel.PhaseRetryWaiting)
		s.Attempt = 1
		s, _ = run(t, s, event(kernel.EvRetryDue))
		ev := event(kernel.EvMeasured)
		ev.Fit = tc.verdict
		s, _ = run(t, s, ev)
		if s.Phase != kernel.PhasePersistingResult || s.Pending.Status != kernel.StatusBlocked || s.Pending.Code != tc.code {
			t.Fatalf("%s: %+v", tc.verdict, s.Pending)
		}
	}
}

// TestAStopOrADeadlineWhileWaitingEndsTheRunAtOnce: nothing is sent after the wait is
// over because the Run was stopped.
func TestAStopOrADeadlineWhileWaitingEndsTheRunAtOnce(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status string
	}{
		{modelport.CodeCancelled, kernel.StatusCancelled},
		{kernel.CodeDeadlineExceeded, kernel.StatusIncomplete},
		{kernel.CodeDriverStopped, kernel.StatusIncomplete},
		{modelport.CodePermitRevoked, kernel.StatusCancelled},
	} {
		s := state(kernel.PhaseRetryWaiting)
		s.Attempt = 1
		ev := event(kernel.EvFailed)
		ev.Failure = kernel.Failure{Code: tc.code}
		s, effects := run(t, s, ev)
		if s.Phase != kernel.PhasePersistingResult || s.Pending.Status != tc.status || s.Pending.UnresolvedModelAction ||
			!slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffPersistResult}) {
			t.Fatalf("%s: %+v", tc.code, s.Pending)
		}
	}
	// A wait that is over after the deadline ends the Run as late.
	s := state(kernel.PhaseRetryWaiting)
	s.Attempt = 1
	due := event(kernel.EvRetryDue)
	due.At = deadline
	s, _ = run(t, s, due)
	if s.Phase != kernel.PhasePersistingResult || s.Pending.Code != kernel.CodeDeadlineExceeded {
		t.Fatalf("%+v", s.Pending)
	}
}

// TestEveryNewActionStartsOverAtItsFirstAttempt: after a Tool exchange the next
// generation is a new Action and has its own retry.
func TestEveryNewActionStartsOverAtItsFirstAttempt(t *testing.T) {
	s := state(kernel.PhasePersistingObservation)
	s.Attempt = 1
	s, _ = run(t, s, event(kernel.EvObserved))
	if s.Phase != kernel.PhaseAssembling || s.Attempt != 0 {
		t.Fatalf("%+v", s)
	}
}
