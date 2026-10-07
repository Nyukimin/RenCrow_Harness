package kernel_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/extensions"
	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/kernel"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// TestEveryDocumentedCodeHasAClassification keeps the classification complete: every
// normalized code of ERROR_MAPPING and every code the kernel gives out has a row, and
// there is no row that nothing documents.
func TestEveryDocumentedCodeHasAClassification(t *testing.T) {
	documented := []string{
		// ERROR_MAPPING section 2, the normalized column.
		"QUEUE_TIMEOUT", "CONNECT_FAILED", "UPSTREAM_TRANSIENT", "MODEL_GENERATION_OUTCOME_UNKNOWN", "MODEL_OUTPUT_SCHEMA_INVALID",
		"MODEL_CONTRACT_FAILED", "MODEL_UNAVAILABLE", "MODEL_OUTPUT_DEGENERATE", "EMPTY_FINAL_CONTENT", "REASONING_ONLY", "RAW_TOOL_MARKUP",
		"CONTEXT_LIMIT_EXCEEDED", "BINDING_CHANGED", "INPUT_DIGEST_MISMATCH", "REQUEST_DIGEST_MISMATCH", "UNSUPPORTED_CONTRACT",
		"UNSUPPORTED_RECOVERY_PROFILE", "AUTH_FAILED", "RATE_LIMITED", "LENGTH", "INCOMPLETE", "REFUSED", "CANCELLED", "PERMIT_REVOKED", "BUDGET_UNVERIFIED",
		// The Run's own limits and the Harness's own state.
		"DEADLINE_EXCEEDED", "STEP_BUDGET_EXHAUSTED", "GENERATION_BUDGET_EXHAUSTED", "DRIVER_STOPPED", "CAPACITY_BLOCKED",
		"INTEGRITY_BLOCKED", "PERSISTENCE_UNCERTAIN", "INTERNAL_ERROR",
		// The Tool runtime's own ends of a Run (IMPLEMENTATION_SPEC sections 9 and 13, HOST_ASSETS section 1, STORAGE section 3).
		"EFFECT_OUTCOME_UNKNOWN", "POLICY_AMBIGUOUS", "POLICY_CHANGED", "WORKSPACE_BUSY",
		// The host's fixed hook before a call (HOST_ASSETS section 4): denied, or no valid answer in time.
		"HOST_HOOK_DENIED", "HOST_HOOK_FAILED",
		// A compaction whose candidate was found stale twice in one step (IMPLEMENTATION_SPEC section 11).
		"COMPACTION_STALE",
	}
	have := kernel.ClassifiedCodes()
	for _, c := range documented {
		// MODEL_GENERATION_OUTCOME_UNKNOWN is not a row: it is what an unknown generation state gives.
		if c == "MODEL_GENERATION_OUTCOME_UNKNOWN" {
			continue
		}
		if !slices.Contains(have, c) {
			t.Errorf("%s has no classification", c)
		}
	}
	for _, c := range have {
		if !slices.Contains(documented, c) {
			t.Errorf("%s is classified but documented nowhere", c)
		}
	}
}

// TestClassifyFailure is F19 over the table of ERROR_MAPPING section 3 and
// RETRY_CONTRACT section 3 for a Run that cannot retry.
func TestClassifyFailure(t *testing.T) {
	T, N, U := modelport.StateTerminal, modelport.StateNotStarted, modelport.StateUnknown
	type want struct {
		status, code string
		resumable    bool
		unresolved   bool
	}
	for _, tc := range []struct {
		f Failure
		w want
	}{
		{Failure{"REASONING_ONLY", T, false}, want{"failed", "MODEL_OUTPUT_INVALID", true, false}},
		{Failure{"EMPTY_FINAL_CONTENT", T, false}, want{"failed", "MODEL_OUTPUT_INVALID", true, false}},
		{Failure{"RAW_TOOL_MARKUP", T, false}, want{"failed", "MODEL_OUTPUT_INVALID", true, false}},
		{Failure{"MODEL_OUTPUT_SCHEMA_INVALID", T, false}, want{"failed", "MODEL_OUTPUT_INVALID", true, false}},
		{Failure{"MODEL_OUTPUT_DEGENERATE", T, false}, want{"failed", "MODEL_OUTPUT_DEGENERATE", true, false}},
		{Failure{"CONNECT_FAILED", N, false}, want{"failed", "MODEL_TRANSPORT_FAILED", true, false}},
		{Failure{"UPSTREAM_TRANSIENT", T, false}, want{"failed", "MODEL_TRANSPORT_FAILED", true, false}},
		{Failure{"RATE_LIMITED", N, false}, want{"incomplete", "MODEL_TEMPORARILY_UNAVAILABLE", true, false}},
		{Failure{"QUEUE_TIMEOUT", N, false}, want{"incomplete", "MODEL_TEMPORARILY_UNAVAILABLE", true, false}},
		{Failure{"MODEL_UNAVAILABLE", N, false}, want{"blocked", "MODEL_UNAVAILABLE", true, false}},
		{Failure{"CONTEXT_LIMIT_EXCEEDED", N, false}, want{"blocked", "CAPACITY_BLOCKED", true, false}},
		{Failure{"LENGTH", T, false}, want{"incomplete", "MODEL_OUTPUT_TRUNCATED", true, false}},
		{Failure{"INCOMPLETE", T, false}, want{"incomplete", "MODEL_OUTPUT_TRUNCATED", true, false}},
		{Failure{"REFUSED", T, false}, want{"rejected", "MODEL_REFUSED", false, false}},
		{Failure{"BINDING_CHANGED", N, false}, want{"blocked", "BINDING_CHANGED", true, false}},
		{Failure{"UNSUPPORTED_CONTRACT", N, false}, want{"blocked", "UNSUPPORTED_CONTRACT", true, false}},
		{Failure{"UNSUPPORTED_RECOVERY_PROFILE", N, false}, want{"blocked", "UNSUPPORTED_RECOVERY_PROFILE", true, false}},
		{Failure{"AUTH_FAILED", N, false}, want{"blocked", "AUTH_FAILED", true, false}},
		{Failure{"INPUT_DIGEST_MISMATCH", N, false}, want{"blocked", "INPUT_DIGEST_MISMATCH", true, false}},
		{Failure{"REQUEST_DIGEST_MISMATCH", N, false}, want{"blocked", "REQUEST_DIGEST_MISMATCH", true, false}},
		{Failure{"MODEL_CONTRACT_FAILED", T, false}, want{"failed", "MODEL_CONTRACT_FAILED", true, false}},
		{Failure{"BUDGET_UNVERIFIED", "", false}, want{"blocked", "BUDGET_UNVERIFIED", true, false}},
		{Failure{"CAPACITY_BLOCKED", "", false}, want{"blocked", "CAPACITY_BLOCKED", true, false}},
		{Failure{"CANCELLED", T, false}, want{"cancelled", "CANCELLED", true, false}},
		{Failure{"PERMIT_REVOKED", N, false}, want{"cancelled", "PERMIT_REVOKED", true, false}},
		{Failure{"DEADLINE_EXCEEDED", "", false}, want{"incomplete", "DEADLINE_EXCEEDED", true, false}},
		{Failure{"STEP_BUDGET_EXHAUSTED", "", false}, want{"incomplete", "STEP_BUDGET_EXHAUSTED", true, false}},
		{Failure{"GENERATION_BUDGET_EXHAUSTED", "", false}, want{"incomplete", "GENERATION_BUDGET_EXHAUSTED", true, false}},
		{Failure{"DRIVER_STOPPED", "", false}, want{"incomplete", "DRIVER_STOPPED", true, false}},
		{Failure{"INTEGRITY_BLOCKED", "", false}, want{"blocked", "INTEGRITY_BLOCKED", false, false}},
		{Failure{"PERSISTENCE_UNCERTAIN", "", false}, want{"restart_required", "PERSISTENCE_UNCERTAIN", true, false}},
		{Failure{"INTERNAL_ERROR", "", false}, want{"failed", "INTERNAL_ERROR", true, false}},
		// Whatever is not in the table is a model contract failure, not a guess.
		{Failure{"SOMETHING_NEW", T, false}, want{"failed", "MODEL_CONTRACT_FAILED", true, false}},
		{Failure{"", "", false}, want{"failed", "MODEL_CONTRACT_FAILED", true, false}},
		// An unknown generation outranks every code but a cancellation, and is never
		// retried, never reported as the failure it might have been, and left unresolved.
		{Failure{"REASONING_ONLY", U, false}, want{"blocked", "MODEL_GENERATION_OUTCOME_UNKNOWN", true, true}},
		{Failure{"RATE_LIMITED", U, false}, want{"blocked", "MODEL_GENERATION_OUTCOME_UNKNOWN", true, true}},
		{Failure{"DEADLINE_EXCEEDED", U, false}, want{"blocked", "MODEL_GENERATION_OUTCOME_UNKNOWN", true, true}},
		{Failure{"DRIVER_STOPPED", U, false}, want{"blocked", "MODEL_GENERATION_OUTCOME_UNKNOWN", true, true}},
		{Failure{"MODEL_CONTRACT_FAILED", U, false}, want{"blocked", "MODEL_GENERATION_OUTCOME_UNKNOWN", true, true}},
		{Failure{"CANCELLED", U, false}, want{"cancelled", "CANCELLED", true, true}},
		// A hidden retry is a contract failure that blocks.
		{Failure{"UPSTREAM_TRANSIENT", T, true}, want{"blocked", "MODEL_CONTRACT_FAILED", true, false}},
		{Failure{"MODEL_CONTRACT_FAILED", U, true}, want{"blocked", "MODEL_CONTRACT_FAILED", true, true}},
	} {
		t.Run(tc.f.Code+"/"+tc.f.GenerationState, func(t *testing.T) {
			got := kernel.ClassifyFailure(tc.f)
			if got.Status != tc.w.status || got.Code != tc.w.code || got.Resumable != tc.w.resumable || got.UnresolvedModelAction != tc.w.unresolved {
				t.Fatalf("%+v, want %+v", got, tc.w)
			}
			if !slices.Contains(kernel.Statuses(), got.Status) {
				t.Fatalf("%s is not a terminal status", got.Status)
			}
		})
	}
}

// Failure is spelled positionally above; this keeps the literals short.
type Failure = kernel.Failure

func TestJudgeAndTheAttemptRecordAgree(t *testing.T) {
	for _, tc := range []struct {
		g       kernel.Generated
		usable  bool
		outcome string
		code    string
	}{
		{kernel.Generated{Kind: modelport.KindFinal, GenerationState: modelport.StateTerminal}, true, "completed", ""},
		{kernel.Generated{Kind: modelport.KindFinal, GenerationState: modelport.StateUnknown}, false, "error", "MODEL_CONTRACT_FAILED"},
		{kernel.Generated{Kind: modelport.KindFinal, GenerationState: modelport.StateTerminal, HiddenRetry: true}, false, "error", "MODEL_CONTRACT_FAILED"},
		{kernel.Generated{Kind: modelport.KindToolCalls, GenerationState: modelport.StateTerminal}, false, "error", "MODEL_CONTRACT_FAILED"},
		{kernel.Generated{Kind: modelport.KindToolCalls, GenerationState: modelport.StateTerminal, ToolsOffered: true}, false, "error", "MODEL_CONTRACT_FAILED"},
		{kernel.Generated{Kind: modelport.KindToolCalls, GenerationState: modelport.StateTerminal, ToolCalls: 2}, false, "error", "MODEL_CONTRACT_FAILED"},
		{kernel.Generated{Kind: modelport.KindToolCalls, GenerationState: modelport.StateUnknown, ToolsOffered: true, ToolCalls: 1}, false, "error", "MODEL_CONTRACT_FAILED"},
		{kernel.Generated{Kind: modelport.KindToolCalls, GenerationState: modelport.StateTerminal, ToolsOffered: true, ToolCalls: 1}, true, "completed", ""},
		{kernel.Generated{Kind: modelport.KindIncomplete, FailureCode: "LENGTH", GenerationState: modelport.StateTerminal}, false, "incomplete", "LENGTH"},
		{kernel.Generated{Kind: modelport.KindRefused, GenerationState: modelport.StateTerminal}, false, "refused", "REFUSED"},
		{kernel.Generated{Kind: modelport.KindError, FailureCode: "RAW_TOOL_MARKUP", GenerationState: modelport.StateTerminal}, false, "error", "RAW_TOOL_MARKUP"},
		{kernel.Generated{Kind: "other", GenerationState: modelport.StateTerminal}, false, "error", "MODEL_CONTRACT_FAILED"},
	} {
		j := kernel.Judge(tc.g)
		if j.Usable != tc.usable || j.Outcome != tc.outcome || j.FailureCode != tc.code {
			t.Errorf("%+v: %+v", tc.g, j)
		}
		if !j.Usable && (j.Failure.Code != tc.code || j.Failure.GenerationState != tc.g.GenerationState) {
			t.Errorf("%+v: the failure %+v does not carry the attempt's code and state", tc.g, j.Failure)
		}
	}
}

func TestResumable(t *testing.T) {
	for _, tc := range []struct {
		status, code string
		want         bool
	}{
		{"completed", "FINAL_RESPONSE_ACCEPTED", false},
		{"rejected", "MODEL_REFUSED", false},
		{"blocked", "INTEGRITY_BLOCKED", false},
		{"blocked", "BINDING_CHANGED", true},
		{"incomplete", "DEADLINE_EXCEEDED", true},
		{"failed", "MODEL_OUTPUT_INVALID", true},
		{"cancelled", "CANCELLED", true},
		{"restart_required", "PERSISTENCE_UNCERTAIN", true},
	} {
		if got := kernel.Resumable(tc.status, tc.code); got != tc.want {
			t.Errorf("%s/%s: %t", tc.status, tc.code, got)
		}
	}
}

func facts(o kernel.Outcome) kernel.ResultFacts {
	return kernel.ResultFacts{RunID: identity.NewRunID().String(), TaskID: identity.NewTaskID().String(), Outcome: o}
}

func completed() kernel.ResultFacts {
	f := facts(kernel.Outcome{Status: kernel.StatusCompleted, Code: kernel.CodeFinalAccepted})
	f.FinalMessageID, f.FinalText = protocol.Str(identity.NewMessageID().String()), "done"
	f.EvidenceIDs = []string{identity.NewEvidenceID().String()}
	return f
}

// TestBuildRunResult is F20: the Run's status and the verification stay apart, and
// facts that contradict each other do not become a result.
func TestBuildRunResult(t *testing.T) {
	f := completed()
	r, err := kernel.BuildRunResult(f)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "completed" || r.Code != "FINAL_RESPONSE_ACCEPTED" || r.FinalText != "done" || r.Resumable || r.LastCheckpointID != nil ||
		r.Verification.Status != "not_run" || len(r.Verification.EvidenceIDs) != 0 || r.Verification.CriteriaRevision != nil ||
		r.UnresolvedActionIDs == nil || len(r.UnresolvedActionIDs) != 0 || len(r.EvidenceIDs) != 1 {
		t.Fatalf("a completed run with nothing checked: %+v", r)
	}

	// A failed Run: no final message, resumable, its unresolved action listed.
	u := facts(kernel.ClassifyFailure(kernel.Failure{Code: "ANY", GenerationState: modelport.StateUnknown}))
	act := identity.NewActionID().String()
	u.UnresolvedActionIDs = []string{act, act}
	r, err = kernel.BuildRunResult(u)
	if err != nil || r.Status != "blocked" || r.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" || !r.Resumable || len(r.UnresolvedActionIDs) != 1 || r.FinalMessageID != nil || r.FinalText != "" {
		t.Fatalf("%+v %v", r, err)
	}

	ev := completed().EvidenceIDs[0]
	crit := "criteria-1"
	for name, edit := range map[string]func(f *kernel.ResultFacts){
		"unknown status":                  func(f *kernel.ResultFacts) { f.Outcome.Status = "running" },
		"empty code":                      func(f *kernel.ResultFacts) { f.Outcome.Code = "" },
		"code too long":                   func(f *kernel.ResultFacts) { f.Outcome.Code = strings.Repeat("A", 81) },
		"completed without a message":     func(f *kernel.ResultFacts) { f.FinalMessageID, f.FinalText = nil, "" },
		"text without a message":          func(f *kernel.ResultFacts) { f.FinalMessageID = nil },
		"message without a text":          func(f *kernel.ResultFacts) { f.FinalText = "" },
		"completed with an unresolved":    func(f *kernel.ResultFacts) { f.UnresolvedActionIDs = []string{act} },
		"completed with an unknown model": func(f *kernel.ResultFacts) { f.Outcome.UnresolvedModelAction = true },
		"passed without evidence": func(f *kernel.ResultFacts) {
			f.Verification = &protocol.Verification{Status: "passed", EvidenceIDs: []string{}, CriteriaRevision: &crit}
		},
		"passed without criteria": func(f *kernel.ResultFacts) {
			f.Verification = &protocol.Verification{Status: "passed", EvidenceIDs: []string{ev}}
		},
		"passed on evidence the result lacks": func(f *kernel.ResultFacts) {
			f.Verification = &protocol.Verification{Status: "passed", EvidenceIDs: []string{identity.NewEvidenceID().String()}, CriteriaRevision: &crit}
		},
		"verification of another kind": func(f *kernel.ResultFacts) {
			f.Verification = &protocol.Verification{Status: "verified", EvidenceIDs: []string{}}
		},
		"not an ID": func(f *kernel.ResultFacts) { f.RunID = "run_1" },
	} {
		t.Run(name, func(t *testing.T) {
			f := completed()
			f.EvidenceIDs = []string{ev}
			edit(&f)
			if _, err := kernel.BuildRunResult(f); !errors.Is(err, kernel.ErrInvalidTerminal) {
				t.Fatalf("err=%v", err)
			}
		})
	}

	// A passed verification that names evidence and criteria is kept, beside a status
	// that says nothing about it.
	f = completed()
	f.EvidenceIDs = []string{ev}
	f.Verification = &protocol.Verification{Status: "passed", EvidenceIDs: []string{ev}, CriteriaRevision: &crit}
	if r, err := kernel.BuildRunResult(f); err != nil || r.Verification.Status != "passed" {
		t.Fatalf("%+v %v", r, err)
	}
	f.Outcome = kernel.Outcome{Status: kernel.StatusFailed, Code: "MODEL_OUTPUT_INVALID"}
	f.FinalMessageID, f.FinalText = nil, ""
	if r, err := kernel.BuildRunResult(f); err != nil || r.Status != "failed" || r.Verification.Status != "passed" {
		t.Fatalf("the run's status and the verification are separate: %+v %v", r, err)
	}
}

// TestOrphanOutcomeIsTheClassificationOfAStoppedDriver: the two ways a Run loses its
// driver end the same way, and an unresolved generation outranks the deadline.
func TestOrphanOutcomeIsTheClassificationOfAStoppedDriver(t *testing.T) {
	now, deadline := t0, t0.Add(time.Minute)
	for name, tc := range map[string]struct {
		unresolved, tools int
		now               time.Time
		want              kernel.Outcome
	}{
		"nothing dispatched, before the deadline": {0, 0, now, kernel.ClassifyFailure(kernel.Failure{Code: kernel.CodeDriverStopped})},
		"nothing dispatched, at the deadline":     {0, 0, deadline, kernel.ClassifyFailure(kernel.Failure{Code: kernel.CodeDeadlineExceeded})},
		"an unresolved generation":                {1, 0, now, kernel.ClassifyFailure(kernel.Failure{Code: kernel.CodeDriverStopped, GenerationState: modelport.StateUnknown})},
		"an unresolved generation past deadline":  {2, 0, deadline.Add(time.Hour), kernel.ClassifyFailure(kernel.Failure{Code: kernel.CodeDriverStopped, GenerationState: modelport.StateUnknown})},
		"a Tool call whose effect is unknown":     {0, 1, now, kernel.ClassifyFailure(kernel.Failure{Code: kernel.CodeEffectOutcomeUnknown})},
		"a Tool call unknown past the deadline":   {0, 2, deadline.Add(time.Hour), kernel.ClassifyFailure(kernel.Failure{Code: kernel.CodeEffectOutcomeUnknown})},
		"a generation outranks a Tool call":       {1, 1, now, kernel.ClassifyFailure(kernel.Failure{Code: kernel.CodeDriverStopped, GenerationState: modelport.StateUnknown})},
	} {
		t.Run(name, func(t *testing.T) {
			if got := kernel.OrphanOutcome(tc.unresolved, tc.tools, tc.now, deadline); got != tc.want {
				t.Fatalf("%+v, want %+v", got, tc.want)
			}
		})
	}
	if o := kernel.OrphanOutcome(0, 0, now, deadline); o.Status != "incomplete" || o.Code != "DRIVER_STOPPED" || !o.Resumable || o.UnresolvedModelAction {
		t.Fatalf("%+v", o)
	}
	if o := kernel.OrphanOutcome(1, 0, now, deadline); o.Status != "blocked" || o.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" || !o.Resumable || !o.UnresolvedModelAction {
		t.Fatalf("%+v", o)
	}
	if o := kernel.OrphanOutcome(0, 1, now, deadline); o.Status != "blocked" || o.Code != "EFFECT_OUTCOME_UNKNOWN" || !o.Resumable || o.UnresolvedModelAction {
		t.Fatalf("%+v", o)
	}
}

// TestAStopRequestedForARunNobodyDroveEndsItOnlyWhenNothingIsUnresolved: a request to
// stop a Run is a request. When nobody drove the Run, nothing was stopped, so what the Run
// left unknown (a generation, a Tool call) still decides how it ends, and only a Run with
// nothing unresolved ends as the cancelled Run the caller asked for, ahead of its
// deadline and of a stopped driver.
func TestAStopRequestedForARunNobodyDroveEndsItOnlyWhenNothingIsUnresolved(t *testing.T) {
	now, deadline := t0, t0.Add(time.Minute)
	cancelled := kernel.ClassifyFailure(kernel.Failure{Code: modelport.CodeCancelled})
	for name, tc := range map[string]struct {
		unresolved, tools int
		now               time.Time
		want              kernel.Outcome
	}{
		"nothing unresolved":                  {0, 0, now, cancelled},
		"nothing unresolved, past deadline":   {0, 0, deadline.Add(time.Hour), cancelled},
		"an unresolved generation":            {1, 0, now, kernel.ClassifyFailure(kernel.Failure{Code: kernel.CodeDriverStopped, GenerationState: modelport.StateUnknown})},
		"a Tool call whose effect is unknown": {0, 1, now, kernel.ClassifyFailure(kernel.Failure{Code: kernel.CodeEffectOutcomeUnknown})},
	} {
		t.Run(name, func(t *testing.T) {
			if got := kernel.OrphanOutcomeCancelled(tc.unresolved, tc.tools, true, tc.now, deadline); got != tc.want {
				t.Fatalf("%+v, want %+v", got, tc.want)
			}
			// Without the request the classification is the one it always was.
			if got, want := kernel.OrphanOutcomeCancelled(tc.unresolved, tc.tools, false, tc.now, deadline), kernel.OrphanOutcome(tc.unresolved, tc.tools, tc.now, deadline); got != want {
				t.Fatalf("%+v, want %+v", got, want)
			}
		})
	}
	if cancelled.Status != "cancelled" || cancelled.Code != "CANCELLED" || !cancelled.Resumable || cancelled.UnresolvedModelAction {
		t.Fatalf("%+v", cancelled)
	}
}

// TestARunBlockedByAnUnknownGenerationEndsAsAnyUnknownGenerationDoes: a Run admitted while a
// generation of its Task is unknown ends as that unknown is classified (blocked,
// MODEL_GENERATION_OUTCOME_UNKNOWN, resumable), listing the unknown generations' Actions
// and nothing else, with no final text.
func TestARunBlockedByAnUnknownGenerationEndsAsAnyUnknownGenerationDoes(t *testing.T) {
	in, err := kernel.SettleUnknownGeneration(sqlite.StaleRun{
		RunID: "run_00000000-0000-7000-8000-000000000001", TaskID: "tsk_00000000-0000-7000-8000-000000000001", Phase: "Admitting",
		Unresolved: []sqlite.UnresolvedAttempt{{ActionID: "act_00000000-0000-7000-8000-000000000001", AttemptID: "att_00000000-0000-7000-8000-000000000001"},
			{ActionID: "act_00000000-0000-7000-8000-000000000002", AttemptID: "att_00000000-0000-7000-8000-000000000002"}},
		EvidenceIDs: []string{"evd_00000000-0000-7000-8000-000000000001"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := in.Result
	want := kernel.ClassifyFailure(kernel.Failure{Code: modelport.CodeOutcomeUnknown, GenerationState: modelport.StateUnknown})
	if r.Status != want.Status || r.Code != want.Code || r.Status != "blocked" || r.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" || !r.Resumable || r.FinalText != "" || r.FinalMessageID != nil ||
		len(r.UnresolvedActionIDs) != 2 || r.Verification.Status != "not_run" || in.ResultEvidenceID == "" || in.FinalText != "" {
		t.Fatalf("%+v", in)
	}
}

// TestThePhasesAreTheSchemasPhases keeps the sixteen values of the kernel and of
// RunInfo.phase one list.
func TestThePhasesAreTheSchemasPhases(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "protocol.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range kernel.Phases() {
		got = append(got, string(p))
	}
	if want := doc.Defs["RunInfo"].Properties["phase"].Enum; !slices.Equal(got, want) {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
	var statuses = doc.Defs["RunResult"].Properties["status"].Enum
	if !slices.Equal(kernel.Statuses(), statuses) {
		t.Fatalf("statuses %v, schema %v", kernel.Statuses(), statuses)
	}
}

// TestTheHookCodesAreSpelledAlikeByTheKernelAndTheHookBoundary: the kernel's pure files cannot
// import internal/extensions, so the two spell the codes of a hook's end of a Run each, and
// this is what keeps them one.
func TestTheHookCodesAreSpelledAlikeByTheKernelAndTheHookBoundary(t *testing.T) {
	if kernel.CodeHostHookDenied != extensions.CodeHookDenied || kernel.CodeHostHookFailed != extensions.CodeHookFailed {
		t.Fatal("the kernel and the hook boundary spell the codes differently")
	}
	for _, code := range []string{extensions.CodeHookDenied, extensions.CodeHookFailed} {
		o := kernel.ClassifyFailure(kernel.Failure{Code: code})
		if o.Status != kernel.StatusBlocked || o.Code != code || !o.Resumable || o.UnresolvedModelAction {
			t.Errorf("%s: %+v", code, o)
		}
	}
}
