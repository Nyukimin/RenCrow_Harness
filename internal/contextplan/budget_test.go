package contextplan_test

import (
	"errors"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func fixtureMeasure(t testing.TB) (modelport.MeasureResult, contextplan.BudgetInput) {
	t.Helper()
	m := decode[modelport.MeasureResult](t, wire(t, "measure_result.json"))
	return m, contextplan.BudgetInput{
		Measure: m, InputDigest: m.InputDigest, BindingFingerprint: m.BindingFingerprint,
		ReservedOutputTokens: m.ReservedOutputTokens, SafetyMarginTokens: m.SafetyMarginTokens,
	}
}

func i64(n int64) *int64 { return &n }

func TestEstimateBudgetOnTheFixtureCount(t *testing.T) {
	m, in := fixtureMeasure(t)
	if err := modelport.ValidateMeasureResult(m); err != nil {
		t.Fatal(err)
	}
	est, err := contextplan.EstimateBudget(in)
	if err != nil {
		t.Fatal(err)
	}
	// 32768 - 4096 - 256: the overhead is already in prompt_tokens and is not
	// subtracted again.
	if est.Verdict != contextplan.VerdictFit || est.Usable != 28416 {
		t.Fatalf("%+v", est)
	}
	r := est.Report
	if r.State != "verified_exact" || *r.PromptLower != 8000 || *r.PromptUpper != 8000 || *r.ContextLimit != 32768 ||
		r.NormalizationFingerprint != m.BindingFingerprint || r.RequestDigest != m.RequestDigest || r.EvidenceRef == nil {
		t.Fatalf("report %+v", r)
	}
	if b, err := protocol.Encode(r); err != nil {
		t.Fatalf("the report is not a valid BudgetReport: %v (%s)", err, b)
	}
}

// TestBudgetBoundaries is A04: the four states and the edges of fit and no-fit.
func TestBudgetBoundaries(t *testing.T) {
	const usable = 28416
	for _, tc := range []struct {
		name         string
		state        string
		lower, upper *int64
		limit        *int64
		want         contextplan.Verdict
	}{
		{"exact on the edge fits", "verified_exact", i64(usable), i64(usable), i64(32768), contextplan.VerdictFit},
		{"exact one over does not fit", "verified_exact", i64(usable + 1), i64(usable + 1), i64(32768), contextplan.VerdictNoFit},
		{"bound whose upper is on the edge fits", "verified_bound", i64(usable - 100), i64(usable), i64(32768), contextplan.VerdictFit},
		{"bound wholly over is no-fit", "verified_bound", i64(usable + 1), i64(usable + 50), i64(32768), contextplan.VerdictNoFit},
		{"bound that crosses the edge is a straddle", "verified_bound", i64(usable - 10), i64(usable + 10), i64(32768), contextplan.VerdictStraddle},
		{"bound whose lower is on the edge is a straddle", "verified_bound", i64(usable), i64(usable + 1), i64(32768), contextplan.VerdictStraddle},
		{"an estimate is unverified, however small", "estimated", i64(10), i64(20), i64(32768), contextplan.VerdictUnverified},
		{"an unverified count is unverified", "unverified", nil, nil, nil, contextplan.VerdictUnverified},
		{"a limit below the reservation leaves nothing usable", "verified_exact", i64(1), i64(1), i64(4000), contextplan.VerdictNoFit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, in := fixtureMeasure(t)
			m.State, m.PromptLower, m.PromptUpper, m.EffectiveContextLimit = tc.state, tc.lower, tc.upper, tc.limit
			in.Measure = m
			est, err := contextplan.EstimateBudget(in)
			if err != nil {
				t.Fatal(err)
			}
			if est.Verdict != tc.want {
				t.Fatalf("verdict %s, want %s (usable %d)", est.Verdict, tc.want, est.Usable)
			}
			if est.Report.State != tc.state {
				t.Fatalf("an estimate must keep its state: %+v", est.Report)
			}
		})
	}
}

func TestMeasureAnswersThatAreNotAboutTheRequestAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(in *contextplan.BudgetInput)
		code string
	}{
		{"another input", func(in *contextplan.BudgetInput) { in.InputDigest = "0000" }, modelport.CodeInputDigestMismatch},
		{"another binding", func(in *contextplan.BudgetInput) { in.BindingFingerprint = "bfp-v1:changed" }, modelport.CodeBindingChanged},
		{"another reservation", func(in *contextplan.BudgetInput) { in.ReservedOutputTokens++ }, modelport.CodeContractFailed},
		{"another margin", func(in *contextplan.BudgetInput) { in.SafetyMarginTokens++ }, modelport.CodeContractFailed},
		{"exact with two bounds", func(in *contextplan.BudgetInput) { in.Measure.PromptUpper = i64(8001) }, modelport.CodeContractFailed},
		{"bounds the wrong way round", func(in *contextplan.BudgetInput) {
			in.Measure.State, in.Measure.PromptLower, in.Measure.PromptUpper = "verified_bound", i64(9), i64(8)
		}, modelport.CodeContractFailed},
		{"verified without a limit", func(in *contextplan.BudgetInput) { in.Measure.EffectiveContextLimit = nil }, modelport.CodeContractFailed},
		{"unknown state", func(in *contextplan.BudgetInput) { in.Measure.State = "guessed" }, modelport.CodeContractFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, in := fixtureMeasure(t)
			tc.edit(&in)
			_, err := contextplan.EstimateBudget(in)
			var me *contextplan.MismatchError
			if !errors.As(err, &me) || me.Code != tc.code {
				t.Fatalf("err=%v, want %s", err, tc.code)
			}
		})
	}
}

// TestTheTriggerRatioIsAnInclusiveIntegerBoundaryOfAVerifiedFit: a prompt that fits has
// reached the trigger exactly when its upper bound is at least ratio * usable; one token under
// has not. Nothing that is not a verified fit ever reaches it.
func TestTheTriggerRatioIsAnInclusiveIntegerBoundaryOfAVerifiedFit(t *testing.T) {
	est := func(verdict contextplan.Verdict, upper int64, usable int64) contextplan.BudgetEstimate {
		return contextplan.BudgetEstimate{Verdict: verdict, Usable: usable, Report: protocol.BudgetReport{PromptUpper: i64(upper)}}
	}
	for _, tc := range []struct {
		name  string
		est   contextplan.BudgetEstimate
		ratio float64
		want  bool
	}{
		{"exactly on the boundary (0.85 of 8000)", est(contextplan.VerdictFit, 6800, 8000), 0.85, true},
		{"one token under", est(contextplan.VerdictFit, 6799, 8000), 0.85, false},
		{"one token over", est(contextplan.VerdictFit, 6801, 8000), 0.85, true},
		{"a full prompt that still fits", est(contextplan.VerdictFit, 8000, 8000), 0.85, true},
		{"a float product would be 6799.999999999999 for 0.85 of 8000 on some platforms: the integer ratio is exact", est(contextplan.VerdictFit, 6800, 8000), 0.85, true},
		{"0.1 of 30 is 3 exactly", est(contextplan.VerdictFit, 3, 30), 0.1, true},
		{"no fit never reaches it", est(contextplan.VerdictNoFit, 9000, 8000), 0.85, false},
		{"a straddle never reaches it", est(contextplan.VerdictStraddle, 7000, 8000), 0.85, false},
		{"an unverified count never reaches it", est(contextplan.VerdictUnverified, 7000, 8000), 0.85, false},
		{"no usable budget", est(contextplan.VerdictFit, 1, 0), 0.85, false},
		{"a ratio of zero disables it", est(contextplan.VerdictFit, 8000, 8000), 0, false},
		{"a ratio of one disables it", est(contextplan.VerdictFit, 8000, 8000), 1, false},
		{"a ratio above one disables it", est(contextplan.VerdictFit, 8000, 8000), 1.5, false},
		{"a negative ratio disables it", est(contextplan.VerdictFit, 8000, 8000), -0.5, false},
		{"a count at the top of the exact integer range does not overflow", est(contextplan.VerdictFit, 9007199254740991, 9007199254740991), 0.999999, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := contextplan.ReachesTrigger(tc.est, tc.ratio); got != tc.want {
				t.Fatalf("ReachesTrigger = %v, want %v", got, tc.want)
			}
		})
	}
	noUpper := contextplan.BudgetEstimate{Verdict: contextplan.VerdictFit, Usable: 8000}
	if contextplan.ReachesTrigger(noUpper, 0.85) {
		t.Fatal("a count without an upper bound never reaches the trigger")
	}
}
