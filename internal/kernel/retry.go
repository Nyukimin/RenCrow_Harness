package kernel

import (
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

// The numbers of RETRY_CONTRACT section 2 and 3.
const (
	// MaxAttemptsPerAct is the most Attempts one act Action has: the first and one
	// retry. The caller cannot raise it (RecoveryPolicy.max_attempts_per_act is fixed).
	MaxAttemptsPerAct = 2
	// RetryDelayTransientMS is the wait before the same request is sent again after a
	// transient connection or upstream failure.
	RetryDelayTransientMS = 1000
	// RetryDelayMinRateMS and RetryDelayMaxRateMS bound the wait after RATE_LIMITED or
	// QUEUE_TIMEOUT: at least the first, and no retry when the wait would be over the
	// second.
	RetryDelayMinRateMS = 1000
	RetryDelayMaxRateMS = 10000
)

// Offer says whether a recovery profile may be used for the failure at hand. The driver
// works it out from the three things RETRY_CONTRACT section 4 intersects: the host's
// and the Run's frozen policy, and what the binding's descriptor publishes for the act
// stage and for this code. The machine only reads the answer.
type Offer string

const (
	// OfferAllowed: the policy allows the profile, the binding offers it for the act
	// stage, and the profile's codes include the failure's code.
	OfferAllowed Offer = "offered"
	// OfferNotAllowed: the Run's frozen recovery policy does not list the profile.
	OfferNotAllowed Offer = "not_allowed_by_policy"
	// OfferNotSupported: the binding publishes no such profile for the act stage.
	OfferNotSupported Offer = "not_supported_by_binding"
	// OfferCodeNotCovered: the binding publishes the profile, but not for this code.
	OfferCodeNotCovered Offer = "code_not_covered_by_profile"
)

// RecoveryOffers are the answers for the two public profiles. A zero value offers
// nothing.
type RecoveryOffers struct {
	SameRequest        Offer
	TerminalOutputOnce Offer
}

// RetryInput is everything F31 decides from. The instant and the time left come in as
// values; the function reads no clock.
type RetryInput struct {
	// Stage is the stage of the failed request. Only act is ever retried.
	Stage string
	// Failure is the failed Attempt as the classification sees it: its normalized code
	// and what is known of the generation.
	Failure Failure
	// Ordinal is the failed Attempt's ordinal within its Action (0 for the first).
	Ordinal int64
	// AttemptsUsed and MaxAttempts are the Run's generation attempts: consumed, and the
	// cap of its limits. A retry spends one.
	AttemptsUsed, MaxAttempts int64
	// Now and Deadline are the instant of the decision and the Run's deadline.
	Now, Deadline time.Time
	// RetryAfterMS is the wait the model side asked for, 0 when it asked for none. The
	// strict contract carries no such value today, so it is 0 in this build.
	RetryAfterMS int64
	Offers       RecoveryOffers
}

// Reasons a retry is refused (RetryDecision.Denied). They say why the decision is no;
// the Run then ends as the original failure classifies.
const (
	DeniedStage          = "not_act_stage"
	DeniedAttemptLimit   = "attempt_limit_of_the_action"
	DeniedHiddenRetry    = "hidden_retry"
	DeniedState          = "generation_state_not_retryable"
	DeniedCode           = "code_not_retryable"
	DeniedNoProfile      = "no_recovery_profile_offered"
	DeniedAttemptBudget  = "generation_attempt_budget"
	DeniedBackoffTooLong = "backoff_over_the_limit"
	DeniedDeadline       = "backoff_reaches_the_deadline"
)

// RetryDecision is the answer of F31 (INTERNAL_CONTRACTS RetryDecision). Allow=false
// means the Run ends as ClassifyFailure says of the failure; the terminal status and
// code are never chosen here, which keeps the table of "when it cannot be retried" in
// one place.
type RetryDecision struct {
	Allow bool
	// Denied is the reason of a refusal, empty when Allow.
	Denied string
	// Profile is the recovery profile of the retry; TriggerCode the failure that
	// scheduled it; DelayMS the wait before it.
	Profile     string
	TriggerCode string
	DelayMS     int64
	// FallbackFrom, when set, is the profile RETRY_CONTRACT prefers for this failure and
	// that was not used, and FallbackReason says why (terminal_output_once ->
	// same_request when the host, the Run's policy or the binding does not offer it).
	FallbackFrom   string
	FallbackReason Offer
}

// PlanModelRetry (F31) decides, from RETRY_CONTRACT section 3 alone, whether a failed
// act Attempt is retried, with which recovery profile and after how long. It is pure:
// no clock, no store, no network.
//
// A retry needs all of these, in this order of refusal: the act stage; a first Attempt
// (the second one ends as its own failure classifies, and a new Action is never made to
// get around the limit); no hidden retry on the model side; a generation state the table
// allows for the code (a generation that is unknown is never retried and never taken for
// not started); a code the table lists; a profile offered; an attempt left in the Run's
// budget; and a wait that ends before the deadline. The caller also holds the facts a
// pure function cannot see: that no Tool of the response was dispatched (a failed
// completion carries no dispatchable call) and that the control revision, the binding and
// the policy are those of the first Attempt (checked in the reservation transaction).
func PlanModelRetry(in RetryInput) RetryDecision {
	deny := func(why string) RetryDecision { return RetryDecision{Denied: why, TriggerCode: in.Failure.Code} }
	switch {
	case in.Stage != modelport.StageAct:
		return deny(DeniedStage)
	case in.Ordinal+1 >= MaxAttemptsPerAct:
		return deny(DeniedAttemptLimit)
	case in.Failure.HiddenRetry:
		return deny(DeniedHiddenRetry)
	}
	state := in.Failure.GenerationState
	if state == modelport.StateUnknown {
		// Whatever the code says, a generation that may still be running is not sent
		// again, and is never taken for one that did not start.
		return deny(DeniedState)
	}
	var d RetryDecision
	switch in.Failure.Code {
	case modelport.CodeReasoningOnly, modelport.CodeEmptyFinalContent, modelport.CodeRawToolMarkup, modelport.CodeOutputSchemaInvalid:
		// An output of the wrong form from a generation that is shown to have ended.
		if state != modelport.StateTerminal {
			return deny(DeniedState)
		}
		profile, from, why, ok := formatProfile(in.Offers)
		if !ok {
			return deny(DeniedNoProfile)
		}
		d = RetryDecision{Profile: profile, FallbackFrom: from, FallbackReason: why}
	case modelport.CodeConnectFailed, modelport.CodeUpstreamTransient:
		if state != modelport.StateNotStarted && state != modelport.StateTerminal {
			return deny(DeniedState)
		}
		if in.Offers.SameRequest != OfferAllowed {
			return deny(DeniedNoProfile)
		}
		d = RetryDecision{Profile: modelport.ProfileSameRequest, DelayMS: RetryDelayTransientMS}
	case modelport.CodeRateLimited, modelport.CodeQueueTimeout:
		// Only a request that was never dispatched may be sent again after being
		// turned away.
		if state != modelport.StateNotStarted {
			return deny(DeniedState)
		}
		if in.Offers.SameRequest != OfferAllowed {
			return deny(DeniedNoProfile)
		}
		wait := max(RetryDelayMinRateMS, in.RetryAfterMS)
		if wait > RetryDelayMaxRateMS {
			return deny(DeniedBackoffTooLong)
		}
		d = RetryDecision{Profile: modelport.ProfileSameRequest, DelayMS: wait}
	default:
		// Context limit (the same payload is never sent again), a truncated output, a
		// refusal, a contract or binding failure, an unknown outcome, a cancellation:
		// none is retried.
		return deny(DeniedCode)
	}
	d.TriggerCode = in.Failure.Code
	switch {
	case in.AttemptsUsed >= in.MaxAttempts:
		return deny(DeniedAttemptBudget)
	case !in.Now.Add(time.Duration(d.DelayMS) * time.Millisecond).Before(in.Deadline):
		return deny(DeniedDeadline)
	}
	d.Allow = true
	return d
}

// formatProfile chooses the profile for an output of the wrong form: terminal_output_once
// when it is offered, else the same request again (and why the first was not used).
func formatProfile(o RecoveryOffers) (profile, fallbackFrom string, why Offer, ok bool) {
	switch {
	case o.TerminalOutputOnce == OfferAllowed:
		return modelport.ProfileTerminalOutputOnce, "", "", true
	case o.SameRequest == OfferAllowed:
		why = o.TerminalOutputOnce
		if why == "" {
			why = OfferNotSupported
		}
		return modelport.ProfileSameRequest, modelport.ProfileTerminalOutputOnce, why, true
	}
	return "", "", "", false
}
