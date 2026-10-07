package modelport_test

import (
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

func ptr[T any](v T) *T { return &v }

func receiptOf(state string, attempts *int64) *modelport.GatewayAttemptReceipt {
	return &modelport.GatewayAttemptReceipt{
		ContractVersion: modelport.ContractVersion, Stage: modelport.StageAct, BindingFingerprint: ptr("bfp-v1:" + strings.Repeat("a", 64)),
		RequestDigest: ptr(strings.Repeat("b", 64)), LogicalRequests: 1, BackendAttempts: attempts, GenerationState: state,
		RecoveryProfile: modelport.ProfileSameRequest, RecoveryProfileRevision: "builtin-v1", AppliedTransformations: []string{}, InputDigest: ptr(strings.Repeat("c", 64)),
	}
}

func expectedOf() modelport.Expected {
	return modelport.Expected{
		Stage: modelport.StageAct, InputDigest: strings.Repeat("c", 64), RequestDigest: strings.Repeat("b", 64), BindingFingerprint: "bfp-v1:" + strings.Repeat("a", 64),
		ProfileID: modelport.ProfileSameRequest, ProfileRevision: "builtin-v1",
	}
}

func TestVerifyTransformationsAllowsOnlyWhatTheProfileLists(t *testing.T) {
	for _, tc := range []struct {
		name    string
		applied []string
		allowed []string
		ok      bool
	}{
		{"nothing applied", nil, nil, true},
		{"nothing applied, something allowed", []string{}, []string{"format_suffix"}, true},
		{"a listed one", []string{"format_suffix"}, []string{"format_suffix", "thinking_disabled"}, true},
		{"both listed", []string{"thinking_disabled", "format_suffix"}, []string{"format_suffix", "thinking_disabled"}, true},
		{"one that is not listed", []string{"thinking_disabled"}, []string{"format_suffix"}, false},
		{"the same request lists none", []string{"format_suffix"}, nil, false},
		{"an unknown name", []string{"rewrite_everything"}, []string{"format_suffix"}, false},
	} {
		if err := modelport.VerifyTransformations(tc.applied, tc.allowed); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func TestEveryCodeOfTheMappingSaysWhichStatesItComesWith(t *testing.T) {
	for _, tc := range []struct {
		code   string
		states string // n: not_started, t: terminal, u: unknown
	}{
		{modelport.CodeRateLimited, "n"}, {modelport.CodeQueueTimeout, "n"}, {modelport.CodeConnectFailed, "n"}, {modelport.CodeUpstreamTransient, "t"},
		{modelport.CodeReasoningOnly, "t"}, {modelport.CodeEmptyFinalContent, "t"}, {modelport.CodeRawToolMarkup, "tu"}, {modelport.CodeOutputSchemaInvalid, "tu"},
		{modelport.CodeOutputDegenerate, "tu"}, {modelport.CodeModelUnavailable, "nu"}, {modelport.CodeOutcomeUnknown, "u"}, {modelport.CodeContractFailed, "ntu"},
		{modelport.CodeContextLimit, "nt"}, {modelport.CodeBudgetUnverified, "n"}, {modelport.CodeBindingChanged, "n"}, {modelport.CodeAuthFailed, "n"},
		{modelport.CodeLength, "t"}, {modelport.CodeIncomplete, "tu"}, {modelport.CodeRefused, "t"},
	} {
		f, ok := modelport.FactOf(tc.code)
		if !ok {
			t.Errorf("%s is not in the mapping", tc.code)
			continue
		}
		for state, letter := range map[string]string{modelport.StateNotStarted: "n", modelport.StateTerminal: "t", modelport.StateUnknown: "u"} {
			if got, want := f.States.Has(state), strings.Contains(tc.states, letter); got != want {
				t.Errorf("%s with %s: %t, want %t", tc.code, state, got, want)
			}
		}
		if f.States.Has("") || f.States.Has("finished") {
			t.Errorf("%s: a state that is none of the three is in its set", tc.code)
		}
	}
	for _, own := range []string{modelport.CodeCancelled, modelport.CodePermitRevoked, "SOMETHING_ELSE", ""} {
		if _, ok := modelport.FactOf(own); ok {
			t.Errorf("%q is not a code a Gateway states", own)
		}
	}
}

func TestCheckCompletionReceiptHoldsEveryTerminalToTheRequest(t *testing.T) {
	base := func(kind, code string, r *modelport.GatewayAttemptReceipt) modelport.Completion {
		return modelport.Completion{
			TerminalKind: kind, FailureCode: code, FinalText: "x", ContentText: "y", Receipt: r, GenerationState: r.GenerationState, BackendAttempts: r.BackendAttempts,
			ToolIntents: []modelport.ToolIntent{{ProviderToolCallID: "c", Name: "file.read", ArgumentsJSON: "{}"}},
		}
	}
	one, zero := ptr(int64(1)), ptr(int64(0))

	t.Run("a receipt that matches passes untouched", func(t *testing.T) {
		in := base(modelport.KindError, modelport.CodeUpstreamTransient, receiptOf("terminal", one))
		out := modelport.CheckCompletionReceipt(in, expectedOf(), nil)
		if out.TerminalKind != modelport.KindError || out.FailureCode != modelport.CodeUpstreamTransient || out.GenerationState != "terminal" || out.FinalText != "x" || len(out.ToolIntents) != 1 {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("no receipt, nothing to hold", func(t *testing.T) {
		in := modelport.Completion{TerminalKind: modelport.KindError, FailureCode: modelport.CodeOutcomeUnknown, GenerationState: modelport.StateUnknown}
		if out := modelport.CheckCompletionReceipt(in, expectedOf(), nil); out.FailureCode != modelport.CodeOutcomeUnknown || out.GenerationState != modelport.StateUnknown {
			t.Fatalf("%+v", out)
		}
	})
	for name, edit := range map[string]func(r *modelport.GatewayAttemptReceipt){
		"another stage":          func(r *modelport.GatewayAttemptReceipt) { r.Stage = modelport.StageSummary },
		"another request digest": func(r *modelport.GatewayAttemptReceipt) { r.RequestDigest = ptr(strings.Repeat("0", 64)) },
		"another input digest":   func(r *modelport.GatewayAttemptReceipt) { r.InputDigest = ptr(strings.Repeat("0", 64)) },
		"another binding": func(r *modelport.GatewayAttemptReceipt) {
			r.BindingFingerprint = ptr("bfp-v1:" + strings.Repeat("0", 64))
		},
		"another profile":          func(r *modelport.GatewayAttemptReceipt) { r.RecoveryProfile = modelport.ProfileTerminalOutputOnce },
		"another profile revision": func(r *modelport.GatewayAttemptReceipt) { r.RecoveryProfileRevision = "other" },
		"a transformation the profile does not allow": func(r *modelport.GatewayAttemptReceipt) {
			r.AppliedTransformations = []string{"format_suffix"}
		},
		"two logical requests": func(r *modelport.GatewayAttemptReceipt) { r.LogicalRequests = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			r := receiptOf("terminal", one)
			edit(r)
			for _, kind := range []struct{ kind, code string }{
				{modelport.KindFinal, ""}, {modelport.KindToolCalls, ""}, {modelport.KindError, modelport.CodeUpstreamTransient},
				{modelport.KindIncomplete, modelport.CodeLength}, {modelport.KindRefused, modelport.CodeRefused},
			} {
				out := modelport.CheckCompletionReceipt(base(kind.kind, kind.code, r), expectedOf(), nil)
				if out.TerminalKind != modelport.KindError || out.FailureCode != modelport.CodeContractFailed || out.FinalText != "" || len(out.ToolIntents) != 0 || out.ContentText != "" {
					t.Fatalf("%s: %+v", kind.kind, out)
				}
				// The state the receipt stated is kept: a contradiction of the request is not a
				// word about whether the generation ended.
				if out.GenerationState != "terminal" || out.BackendAttempts == nil || *out.BackendAttempts != 1 {
					t.Fatalf("%s: %+v", kind.kind, out)
				}
			}
		})
	}
	t.Run("a hidden retry is a contradiction too", func(t *testing.T) {
		r := receiptOf("terminal", one)
		r.HiddenRetry = true
		if out := modelport.CheckCompletionReceipt(base(modelport.KindError, modelport.CodeUpstreamTransient, r), expectedOf(), nil); out.FailureCode != modelport.CodeContractFailed {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("an allowed transformation is not a contradiction", func(t *testing.T) {
		r := receiptOf("terminal", one)
		r.RecoveryProfile, r.RecoveryProfileRevision, r.AppliedTransformations = modelport.ProfileTerminalOutputOnce, "tooo-1", []string{"format_suffix"}
		exp := expectedOf()
		exp.ProfileID, exp.ProfileRevision = modelport.ProfileTerminalOutputOnce, "tooo-1"
		if out := modelport.CheckCompletionReceipt(base(modelport.KindFinal, "", r), exp, []string{"format_suffix"}); out.TerminalKind != modelport.KindFinal {
			t.Fatalf("%+v", out)
		}
	})

	// A code the mapping lists that cannot come with the state of the receipt is a frame that
	// contradicts itself: its state cannot be read from it.
	for _, tc := range []struct {
		code  string
		state string
		n     *int64
		bad   bool
	}{
		{modelport.CodeRateLimited, "terminal", one, true},
		{modelport.CodeRateLimited, "not_started", zero, false},
		{modelport.CodeConnectFailed, "terminal", one, true},
		{modelport.CodeUpstreamTransient, "not_started", zero, true},
		{modelport.CodeUpstreamTransient, "unknown", nil, true},
		{modelport.CodeOutcomeUnknown, "terminal", one, true},
		{modelport.CodeOutcomeUnknown, "unknown", nil, false},
		{modelport.CodeLength, "unknown", nil, true},
		{modelport.CodeRawToolMarkup, "unknown", nil, false},
		{modelport.CodeContractFailed, "not_started", zero, false},
		{"A_CODE_THE_MAPPING_DOES_NOT_LIST", "terminal", one, false},
	} {
		in := base(modelport.KindError, tc.code, receiptOf(tc.state, tc.n))
		out := modelport.CheckCompletionReceipt(in, expectedOf(), nil)
		if tc.bad {
			if out.FailureCode != modelport.CodeContractFailed || out.GenerationState != modelport.StateUnknown || out.BackendAttempts != nil || out.FinalText != "" || len(out.ToolIntents) != 0 {
				t.Errorf("%s/%s: %+v", tc.code, tc.state, out)
			}
		} else if out.FailureCode != tc.code || out.GenerationState != tc.state {
			t.Errorf("%s/%s: %+v", tc.code, tc.state, out)
		}
	}
}
