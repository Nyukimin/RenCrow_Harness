package contextplan

import (
	"fmt"
	"math"
	"math/bits"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Verdict is what a measure answer allows the Run to conclude about capacity.
type Verdict string

const (
	// VerdictFit: the verified upper bound fits the usable prompt budget.
	VerdictFit Verdict = "fit"
	// VerdictNoFit: even the verified lower bound exceeds it.
	VerdictNoFit Verdict = "no_fit"
	// VerdictStraddle: the verified interval crosses the budget; only an exact
	// recount can decide.
	VerdictStraddle Verdict = "straddle"
	// VerdictUnverified: the count is an estimate or absent. It is never taken for
	// a fit or a no-fit.
	VerdictUnverified Verdict = "unverified"
)

// BudgetInput is a measure answer and what it must be about.
type BudgetInput struct {
	Measure modelport.MeasureResult
	// InputDigest is the logical input digest the Harness computed itself.
	InputDigest string
	// BindingFingerprint is the fingerprint the binding's descriptor published.
	BindingFingerprint string
	// ReservedOutputTokens is the request's max_tokens, SafetyMarginTokens the
	// margin the Harness asked the count to carry.
	ReservedOutputTokens int64
	SafetyMarginTokens   int64
}

// BudgetEstimate is the checked report and its verdict.
type BudgetEstimate struct {
	Report  protocol.BudgetReport
	Verdict Verdict
	// Usable is effective_context_limit - reserved_output_tokens - safety_margin.
	// It is meaningful only when the report has a context limit.
	Usable int64
}

// MismatchError is a measure answer that is not about the request that was
// asked, or not coherent. Code is the normalized code the kernel classifies:
// INPUT_DIGEST_MISMATCH, BINDING_CHANGED or MODEL_CONTRACT_FAILED.
type MismatchError struct {
	Code   string
	Reason string
}

func (e *MismatchError) Error() string { return fmt.Sprintf("contextplan: %s: %s", e.Code, e.Reason) }

// ReportOf is the native BudgetReport of a measure answer: the effective context limit is
// its context_limit and the binding fingerprint its normalization_fingerprint. It checks
// nothing; EstimateBudget does, against the request that was asked about.
func ReportOf(m modelport.MeasureResult) protocol.BudgetReport {
	return protocol.BudgetReport{
		State: m.State, PromptLower: m.PromptLower, PromptUpper: m.PromptUpper, ContextLimit: m.EffectiveContextLimit,
		ReservedOutputTokens: m.ReservedOutputTokens, SafetyMarginTokens: m.SafetyMarginTokens,
		RequestDigest: m.RequestDigest, NormalizationFingerprint: m.BindingFingerprint, EvidenceRef: m.EvidenceRef,
	}
}

// EstimateBudget (F05) checks a measure answer against the request it was asked
// about and judges capacity:
//
//	usable = effective_context_limit - reserved_output_tokens - safety_margin
//	fit    = prompt upper <= usable      no-fit = prompt lower > usable
//
// The prompt count already includes the template and Tool overhead, so nothing is
// subtracted for them. An interval that crosses the budget is a straddle, an
// estimated or unverified count is unverified; neither is ever called a fit or a
// no-fit. The report carries the effective context limit as context_limit and the
// binding fingerprint as normalization_fingerprint, and never presents an estimate
// as a measured token count.
func EstimateBudget(in BudgetInput) (BudgetEstimate, error) {
	m := in.Measure
	mismatch := func(code, reason string) (BudgetEstimate, error) {
		return BudgetEstimate{}, &MismatchError{Code: code, Reason: reason}
	}
	if m.InputDigest != in.InputDigest {
		return mismatch(modelport.CodeInputDigestMismatch, "the count is about another logical input")
	}
	if m.BindingFingerprint != in.BindingFingerprint {
		return mismatch(modelport.CodeBindingChanged, "the count was taken for another binding")
	}
	if m.ReservedOutputTokens != in.ReservedOutputTokens || m.SafetyMarginTokens != in.SafetyMarginTokens {
		return mismatch(modelport.CodeContractFailed, "the count reserves other output or margin tokens than asked")
	}
	report := ReportOf(m)
	switch m.State {
	case "estimated", "unverified":
		return BudgetEstimate{Report: report, Verdict: VerdictUnverified}, nil
	case "verified_exact", "verified_bound":
	default:
		return mismatch(modelport.CodeContractFailed, "the count has an unknown state")
	}
	if m.PromptLower == nil || m.PromptUpper == nil || m.EffectiveContextLimit == nil {
		return mismatch(modelport.CodeContractFailed, "a verified count has no bounds or no context limit")
	}
	lower, upper := *m.PromptLower, *m.PromptUpper
	if lower > upper || (m.State == "verified_exact" && lower != upper) {
		return mismatch(modelport.CodeContractFailed, "the count's bounds are not coherent")
	}
	usable := *m.EffectiveContextLimit - m.ReservedOutputTokens - m.SafetyMarginTokens
	est := BudgetEstimate{Report: report, Usable: usable}
	switch {
	case upper <= usable:
		est.Verdict = VerdictFit
	case lower > usable:
		est.Verdict = VerdictNoFit
	default:
		est.Verdict = VerdictStraddle
	}
	return est, nil
}

// triggerScale is the resolution of a trigger ratio: parts per million. A ratio is
// compared as an integer ratio, never as a float product, so a count that is exactly on the
// boundary is on the same side of it on every platform.
const triggerScale = 1_000_000

// ReachesTrigger (IMPLEMENTATION_SPEC section 8) reports whether the verified count of a
// prompt that FITS has reached the trigger ratio of the usable prompt budget: the point at
// which a compaction is started before the prompt is full, while the request of its Summary
// still fits. It is the trigger's own decision and no more: the prompt it speaks of is
// sent or compacted only on the verified count, and an estimate, a count without bounds or
// an interval that crosses the budget never starts a compaction early (those are not a fit,
// and the machine answers them as before).
//
// The boundary is inclusive: a prompt whose upper bound is exactly ratio * usable has
// reached the trigger. A ratio outside (0, 1) disables the trigger (the configuration
// validator refuses such a ratio, so this is the zero value of a caller that sets none).
func ReachesTrigger(est BudgetEstimate, ratio float64) bool {
	if est.Verdict != VerdictFit || est.Report.PromptUpper == nil || est.Usable <= 0 || !(ratio > 0 && ratio < 1) {
		return false
	}
	ppm := int64(math.Round(ratio * triggerScale))
	if ppm < 1 || ppm >= triggerScale {
		return false
	}
	upper := *est.Report.PromptUpper
	if upper < 0 {
		return false
	}
	// upper * 1e6 >= ppm * usable, in 128 bits.
	lhsHi, lhsLo := bits.Mul64(uint64(upper), triggerScale)
	rhsHi, rhsLo := bits.Mul64(uint64(ppm), uint64(est.Usable))
	return lhsHi > rhsHi || (lhsHi == rhsHi && lhsLo >= rhsLo)
}
