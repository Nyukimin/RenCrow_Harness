package kernel_test

import (
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/kernel"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func exact(n int64) modelport.MeasureResult {
	limit := int64(8000 + 4096 + 2048)
	return modelport.MeasureResult{ContractVersion: modelport.ContractVersion, State: "verified_exact", PromptLower: &n, PromptUpper: &n, EffectiveContextLimit: &limit,
		ReservedOutputTokens: 4096, SafetyMarginTokens: 2048, RequestDigest: strings.Repeat("c", 64), BindingFingerprint: "bfp-v1:" + strings.Repeat("d", 64), InputDigest: strings.Repeat("e", 64)}
}

// TestACompactResultSaysWhatHappenedAndNothingMore is the mapping of PROTOCOL sections 6 and 9 and
// IMPLEMENTATION_SPEC section 11: each end of a manual compaction's Run is one CompactResult that
// satisfies the protocol (the schema and its conditions), an executed result is one of the five
// outcomes only, a result that was not executed claims no outcome and no checkpoint, and a
// checkpoint is named only when one was committed.
func TestACompactResultSaysWhatHappenedAndNothingMore(t *testing.T) {
	committed := func(mode string) *kernel.CommittedCheckpoint {
		return &kernel.CommittedCheckpoint{ID: "ckp_00000000-0000-7000-8000-000000000001", Mode: mode, SemanticBoundary: 2, DurableBoundary: 3, Before: exact(9000), After: exact(3000)}
	}
	required, available := int64(9000), int64(8000)
	capacity := &compaction.Outcome{Before: compaction.Count{Result: exact(9000)}, Required: &required, Available: &available}
	for _, tc := range []struct {
		name                  string
		facts                 kernel.CompactFacts
		status, outcome, code string
		checkpoint            bool
	}{
		{"a Normal checkpoint", kernel.CompactFacts{RunStatus: kernel.StatusCompleted, RunCode: kernel.CodeCompactionCommitted, Checkpoint: committed("normal")}, "executed", "NormalCompacted", "", true},
		{"an Emergency checkpoint", kernel.CompactFacts{RunStatus: kernel.StatusCompleted, RunCode: kernel.CodeCompactionCommitted, Checkpoint: committed("emergency")}, "executed", "EmergencyCompacted", "", true},
		{"a checkpoint and an unresolved generation", kernel.CompactFacts{RunStatus: kernel.StatusBlocked, RunCode: modelport.CodeOutcomeUnknown, Checkpoint: committed("normal")}, "executed", "NormalCompacted", modelport.CodeOutcomeUnknown, true},
		{"a checkpoint and a context that no longer reads back", kernel.CompactFacts{RunStatus: kernel.StatusBlocked, RunCode: kernel.CodeIntegrityBlocked, Checkpoint: committed("normal")}, "executed", "NormalCompacted", kernel.CodeIntegrityBlocked, true},
		{"a checkpoint and a driver that stopped after it", kernel.CompactFacts{RunStatus: kernel.StatusIncomplete, RunCode: kernel.CodeDriverStopped, Checkpoint: committed("normal")}, "executed", "NormalCompacted", "", true},
		{"capacity", kernel.CompactFacts{RunStatus: kernel.StatusBlocked, RunCode: kernel.CodeCapacityBlocked, Outcome: capacity}, "executed", "CapacityBlocked", kernel.CodeCapacityBlocked, false},
		{"capacity without a shortfall that is known", kernel.CompactFacts{RunStatus: kernel.StatusBlocked, RunCode: kernel.CodeCapacityBlocked, Outcome: &compaction.Outcome{Required: &available, Available: &required}}, "executed", "CapacityBlocked", kernel.CodeCapacityBlocked, false},
		{"integrity", kernel.CompactFacts{RunStatus: kernel.StatusBlocked, RunCode: kernel.CodeIntegrityBlocked}, "executed", "IntegrityBlocked", kernel.CodeIntegrityBlocked, false},
		{"a commit that is not known", kernel.CompactFacts{RunStatus: kernel.StatusRestartRequired, RunCode: kernel.CodePersistenceUncertain}, "executed", "RestartRequired", kernel.CodePersistenceUncertain, false},
		{"a stop", kernel.CompactFacts{RunStatus: kernel.StatusCancelled, RunCode: modelport.CodeCancelled}, "cancelled", "", modelport.CodeCancelled, false},
		{"a deadline", kernel.CompactFacts{RunStatus: kernel.StatusIncomplete, RunCode: kernel.CodeDeadlineExceeded}, "cancelled", "", kernel.CodeDeadlineExceeded, false},
		{"a driver that stopped", kernel.CompactFacts{RunStatus: kernel.StatusIncomplete, RunCode: kernel.CodeDriverStopped}, "cancelled", "", kernel.CodeDriverStopped, false},
		{"a candidate stale twice", kernel.CompactFacts{RunStatus: kernel.StatusIncomplete, RunCode: kernel.CodeCompactionStale}, "stale", "", kernel.CodeCompactionStale, false},
		{"a count that cannot be verified", kernel.CompactFacts{RunStatus: kernel.StatusBlocked, RunCode: modelport.CodeBudgetUnverified}, "unavailable", "", modelport.CodeBudgetUnverified, false},
		{"a model side that is not there", kernel.CompactFacts{RunStatus: kernel.StatusBlocked, RunCode: modelport.CodeModelUnavailable}, "unavailable", "", modelport.CodeModelUnavailable, false},
		{"an unresolved generation and no checkpoint", kernel.CompactFacts{RunStatus: kernel.StatusBlocked, RunCode: modelport.CodeOutcomeUnknown}, "unavailable", "", modelport.CodeOutcomeUnknown, false},
		{"a completed Run with no checkpoint is a contradiction, not a success", kernel.CompactFacts{RunStatus: kernel.StatusCompleted, RunCode: kernel.CodeCompactionCommitted}, "unavailable", "", kernel.CodeInternalError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := kernel.BuildCompactResult(tc.facts)
			if _, err := protocol.NewReceiptPayload(res); err != nil {
				t.Fatalf("not a valid CompactResult: %v\n%+v", err, res)
			}
			outcome := ""
			if res.Outcome != nil {
				outcome = *res.Outcome
			}
			code := ""
			if res.Error != nil {
				code = res.Error.Code
			}
			if res.Status != tc.status || outcome != tc.outcome || code != tc.code || (res.CheckpointID != nil) != tc.checkpoint {
				t.Fatalf("%+v outcome %q error %q", res, outcome, code)
			}
			if res.Status != "executed" && (res.Outcome != nil || res.CheckpointID != nil) {
				t.Fatal("a result that was not executed claims no outcome and no checkpoint")
			}
			if tc.checkpoint && (res.Before == nil || res.After == nil || *res.After.PromptUpper >= *res.Before.PromptLower || *res.SemanticBoundary != 2 || *res.DurableBoundary != 3) {
				t.Fatalf("%+v", res)
			}
		})
	}
	// The capacity numbers are a pair and only for a shortfall that exists.
	if r := kernel.BuildCompactResult(kernel.CompactFacts{RunStatus: kernel.StatusBlocked, RunCode: kernel.CodeCapacityBlocked, Outcome: capacity}); r.RequiredMinimumTokens == nil || *r.RequiredMinimumTokens != 9000 || *r.AvailableTokens != 8000 || r.Before == nil {
		t.Fatalf("%+v", r)
	}
	if r := kernel.BuildCompactResult(kernel.CompactFacts{RunStatus: kernel.StatusBlocked, RunCode: kernel.CodeCapacityBlocked, Outcome: &compaction.Outcome{Required: &available, Available: &required}}); r.RequiredMinimumTokens != nil || r.AvailableTokens != nil {
		t.Fatalf("%+v: no shortfall, no numbers", r)
	}
}
