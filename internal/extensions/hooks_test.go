package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

func validInput(p HookPoint) HookInput {
	act := identity.NewActionID().String()
	return HookInput{
		Hook: p, ThreadID: identity.NewThreadID().String(), RunID: identity.NewRunID().String(), ActionID: &act,
		ContextRevision: 3, ControlRevision: 0, EvidenceIDs: []string{identity.NewEvidenceID().String()},
	}
}

func decode(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	out, err := strictjson.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestHookInputAndResultAreTheSchemasTypes: what the Go types accept and refuse is what
// host_assets.schema.json accepts and refuses, in both directions.
func TestHookInputAndResultAreTheSchemasTypes(t *testing.T) {
	for _, p := range HookPoints() {
		in := validInput(p)
		if err := in.Validate(); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if err := schemacheck.Validate(schemacheck.HostAssets, "HookInput", decode(t, in)); err != nil {
			t.Fatalf("%s: the schema refuses a valid input: %v", p, err)
		}
	}
	stage, code := "act", "completed"
	full := validInput(AfterModel)
	full.Stage, full.Code = &stage, &code
	if err := full.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := schemacheck.Validate(schemacheck.HostAssets, "HookInput", decode(t, full)); err != nil {
		t.Fatal(err)
	}

	empty, tooLong, badStage, badID := "", strings.Repeat("x", 129), "summary", "evd_nope"
	negCtx := validInput(BeforeTool)
	negCtx.ContextRevision = -1
	nilEvidence := validInput(BeforeTool)
	nilEvidence.EvidenceIDs = nil
	manyEvidence := validInput(BeforeTool)
	for i := 0; i < 129; i++ {
		manyEvidence.EvidenceIDs = append(manyEvidence.EvidenceIDs, identity.NewEvidenceID().String())
	}
	for name, in := range map[string]HookInput{
		"unknown point":               {Hook: "after_everything", ThreadID: validInput(BeforeTool).ThreadID, RunID: validInput(BeforeTool).RunID, EvidenceIDs: []string{}},
		"empty code":                  func() HookInput { x := validInput(BeforeTool); x.Code = &empty; return x }(),
		"code too long":               func() HookInput { x := validInput(BeforeTool); x.Code = &tooLong; return x }(),
		"stage that is not one":       func() HookInput { x := validInput(BeforeTool); x.Stage = &badStage; return x }(),
		"evidence id that is not one": func() HookInput { x := validInput(BeforeTool); x.EvidenceIDs = []string{badID}; return x }(),
		"negative revision":           negCtx,
		"null evidence list":          nilEvidence,
		"129 evidence ids":            manyEvidence,
		"action that is not one":      func() HookInput { x := validInput(BeforeTool); a := "act_x"; x.ActionID = &a; return x }(),
	} {
		if err := in.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if name != "null evidence list" { // a nil slice encodes as null, which the schema refuses too
			if err := schemacheck.Validate(schemacheck.HostAssets, "HookInput", decode(t, in)); err == nil {
				t.Errorf("%s: the schema accepted it", name)
			}
		}
	}

	for name, r := range map[string]HookResult{"continue": Continue(), "deny": Deny("POLICY_X")} {
		if err := schemacheck.Validate(schemacheck.HostAssets, "HookResult", decode(t, r)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, r := range map[string]HookResult{"a deny without a code": {Decision: DecisionDeny}, "a decision of none": {Decision: "retry"}} {
		if err := schemacheck.Validate(schemacheck.HostAssets, "HookResult", decode(t, r)); err == nil {
			t.Errorf("%s: the schema accepted it", name)
		}
		if err := r.validate(BeforeTool); err == nil {
			t.Errorf("%s: the Go check accepted it", name)
		}
	}
}

func TestOnlyThePointsBeforeSomethingMayDeny(t *testing.T) {
	want := map[HookPoint]bool{BeforeModel: true, BeforeTool: true, BeforeCompact: true}
	for _, p := range HookPoints() {
		if p.CanDeny() != want[p] {
			t.Errorf("%s: CanDeny=%t", p, p.CanDeny())
		}
		err := Deny("X").validate(p)
		if (err == nil) != want[p] {
			t.Errorf("%s: a deny is %v", p, err)
		}
		if err := Continue().validate(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestTheConfigurationNamesOnlyHooksThisProductHas(t *testing.T) {
	for _, names := range [][]string{nil, {}, {AuditMetadata}} {
		if _, err := NewRunner(names, Options{}); err != nil {
			t.Errorf("%v: %v", names, err)
		}
	}
	for name, names := range map[string][]string{
		"an unknown hook":  {"audit_everything"},
		"a path":           {"/usr/bin/true"},
		"a hook twice":     {AuditMetadata, AuditMetadata},
		"an empty name":    {""},
		"a different case": {"Audit_Metadata"},
	} {
		if _, err := NewRunner(names, Options{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if r, _ := NewRunner(nil, Options{}); r.Active() {
		t.Error("an empty list has something to call")
	}
	var none *Runner
	if none.Active() || none.Call(context.Background(), validInput(BeforeTool), nil).Failed {
		t.Error("a nil runner is not a no-op")
	}
}

func runnerWith(t *testing.T, hard time.Duration, cbs ...Callback) *Runner {
	t.Helper()
	var extra []Named
	for i, cb := range cbs {
		extra = append(extra, Named{Name: string(rune('a' + i)), Call: cb})
	}
	r, err := NewRunner(nil, Options{Extra: extra, HardLimit: hard})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAHookThatContinuesLetsTheRunGoOn(t *testing.T) {
	var calls atomic.Int32
	r := runnerWith(t, time.Second, func(context.Context, HookInput, Recorder) (HookResult, error) {
		calls.Add(1)
		return Continue(), nil
	})
	v := r.Call(context.Background(), validInput(BeforeTool), nil)
	if v.Deny || v.Failed || v.Ended || v.Fatal != nil || calls.Load() != 1 {
		t.Fatalf("%+v", v)
	}
}

func TestADenyBeforeSomethingStopsItAndTheLaterHooksAreNotCalled(t *testing.T) {
	var second atomic.Int32
	r := runnerWith(t, time.Second,
		func(context.Context, HookInput, Recorder) (HookResult, error) { return Deny("NOT_NOW"), nil },
		func(context.Context, HookInput, Recorder) (HookResult, error) { second.Add(1); return Continue(), nil })
	v := r.Call(context.Background(), validInput(BeforeTool), nil)
	if !v.Deny || v.Code != "NOT_NOW" || v.Failed || second.Load() != 0 {
		t.Fatalf("%+v second=%d", v, second.Load())
	}
}

func TestADenyAfterSomethingIsAFailureOfTheHookAndChangesNothing(t *testing.T) {
	r := runnerWith(t, time.Second, func(context.Context, HookInput, Recorder) (HookResult, error) { return Deny("TOO_LATE"), nil })
	for _, p := range []HookPoint{AfterModel, AfterTool, AfterCompact, RunTerminal} {
		v := r.Call(context.Background(), validInput(p), nil)
		if v.Deny || !v.Failed || v.Reason != "invalid_result" {
			t.Errorf("%s: %+v", p, v)
		}
	}
}

func TestAHookThatDoesNotAnswerValidlyFails(t *testing.T) {
	for name, cb := range map[string]Callback{
		"an error": func(context.Context, HookInput, Recorder) (HookResult, error) { return HookResult{}, errors.New("no") },
		"a decision of none": func(context.Context, HookInput, Recorder) (HookResult, error) {
			return HookResult{Decision: "retry"}, nil
		},
		"a deny without a code": func(context.Context, HookInput, Recorder) (HookResult, error) {
			return HookResult{Decision: DecisionDeny}, nil
		},
		"a zero result": func(context.Context, HookInput, Recorder) (HookResult, error) { return HookResult{}, nil },
		"a panic":       func(context.Context, HookInput, Recorder) (HookResult, error) { panic("boom") },
	} {
		r := runnerWith(t, time.Second, cb)
		v := r.Call(context.Background(), validInput(BeforeTool), nil)
		if !v.Failed || v.Deny || v.Ended {
			t.Errorf("%s: %+v", name, v)
		}
	}
}

// TestAHookThatDoesNotReturnIsGivenUpOnAndNotWaitedFor: the runner returns at the hard limit
// with a failure; it does not claim to have stopped the callback, which is told by its
// context and ends when it looks at it.
func TestAHookThatDoesNotReturnIsGivenUpOnAndNotWaitedFor(t *testing.T) {
	release := make(chan struct{})
	stopped := make(chan struct{})
	r := runnerWith(t, 30*time.Millisecond, func(ctx context.Context, _ HookInput, _ Recorder) (HookResult, error) {
		<-release
		close(stopped)
		return Continue(), nil
	})
	begin := time.Now()
	v := r.Call(context.Background(), validInput(BeforeTool), nil)
	took := time.Since(begin)
	if !v.Failed || v.Reason != "timeout" || v.Ended || took > 2*time.Second {
		t.Fatalf("%+v after %v", v, took)
	}
	select {
	case <-stopped:
		t.Fatal("the callback was said to be stopped; nothing stops a goroutine")
	default:
	}
	close(release)
	<-stopped // it ends by itself; its late answer is dropped
}

func TestARecorderWriteThatTheHardLimitEndsIsCancelledThroughItsContext(t *testing.T) {
	sawEnd := make(chan error, 1)
	r := runnerWith(t, 30*time.Millisecond, func(ctx context.Context, in HookInput, rec Recorder) (HookResult, error) {
		_, err := rec(ctx, "x", []byte("{}"))
		sawEnd <- err
		return Continue(), err
	})
	rec := func(ctx context.Context, _ string, _ []byte) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if v := r.Call(context.Background(), validInput(BeforeTool), rec); !v.Failed || v.Reason != "timeout" {
		t.Fatalf("%+v", v)
	}
	if err := <-sawEnd; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the write was not told to stop: %v", err)
	}
}

func TestAStoppedRunIsNotTheHooksFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := runnerWith(t, time.Second, func(c context.Context, _ HookInput, _ Recorder) (HookResult, error) {
		<-c.Done()
		return HookResult{}, c.Err()
	})
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	v := r.Call(ctx, validInput(BeforeTool), nil)
	if !v.Failed || !v.Ended || v.Reason != "run_stopped" {
		t.Fatalf("%+v", v)
	}
	// And a context that had already ended calls nothing.
	var calls atomic.Int32
	r2 := runnerWith(t, time.Second, func(context.Context, HookInput, Recorder) (HookResult, error) { calls.Add(1); return Continue(), nil })
	if v := r2.Call(ctx, validInput(BeforeTool), nil); !v.Ended || calls.Load() != 0 {
		t.Fatalf("%+v calls=%d", v, calls.Load())
	}
}

func TestAStoreErrorOfTheRecorderIsNotHiddenAsTheHooksFailure(t *testing.T) {
	lost := errors.New("writer lost")
	r, err := NewRunner([]string{AuditMetadata}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rec := func(context.Context, string, []byte) (string, error) { return "", Fatal(lost) }
	v := r.Call(context.Background(), validInput(BeforeTool), rec)
	if v.Fatal == nil || !errors.Is(v.Fatal, lost) || !errors.Is(v.Fatal, ErrFatal) || v.Deny {
		t.Fatalf("%+v", v)
	}
	// An ordinary error of the write is the hook's.
	rec = func(context.Context, string, []byte) (string, error) { return "", errors.New("disk full") }
	if v := r.Call(context.Background(), validInput(BeforeTool), rec); !v.Failed || v.Fatal != nil || v.Reason != "error" {
		t.Fatalf("%+v", v)
	}
}

func TestAnInputThatIsNotValidIsNeverGivenToAHook(t *testing.T) {
	var calls atomic.Int32
	r := runnerWith(t, time.Second, func(context.Context, HookInput, Recorder) (HookResult, error) { calls.Add(1); return Continue(), nil })
	in := validInput(BeforeTool)
	in.RunID = "run_x"
	if v := r.Call(context.Background(), in, nil); !v.Failed || v.Reason != "invalid_input" || calls.Load() != 0 {
		t.Fatalf("%+v", v)
	}
}

func TestTheDesignBudgetIsCountedAndNotEnforced(t *testing.T) {
	now := time.Unix(1000, 0)
	tick := 0
	clock := func() time.Time { tick++; return now.Add(time.Duration(tick) * 40 * time.Millisecond) }
	r, err := NewRunner(nil, Options{HardLimit: time.Second, Now: clock, Extra: []Named{{Name: "slow", Call: func(context.Context, HookInput, Recorder) (HookResult, error) { return Continue(), nil }}}})
	if err != nil {
		t.Fatal(err)
	}
	v := r.Call(context.Background(), validInput(BeforeTool), nil)
	if v.Failed || v.Deny || !v.Over || r.OverBudget() != 1 {
		t.Fatalf("%+v over=%d", v, r.OverBudget())
	}
}

// TestAuditMetadataContinuesEverywhereAndRecordsOnlyIdentifiers: the one hook of this
// product continues at all seven points and leaves, for each, a private record of the
// input's identifiers and revisions: nothing else is in it.
func TestAuditMetadataContinuesEverywhereAndRecordsOnlyIdentifiers(t *testing.T) {
	r, err := NewRunner([]string{AuditMetadata}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range HookPoints() {
		var purposes []string
		var bodies [][]byte
		rec := func(_ context.Context, purpose string, data []byte) (string, error) {
			purposes, bodies = append(purposes, purpose), append(bodies, data)
			return identity.NewEvidenceID().String(), nil
		}
		in := validInput(p)
		v := r.Call(context.Background(), in, rec)
		if v.Deny || v.Failed || v.Fatal != nil {
			t.Fatalf("%s: %+v", p, v)
		}
		if len(purposes) != 1 || purposes[0] != PurposeHookAudit {
			t.Fatalf("%s: %v", p, purposes)
		}
		var got map[string]any
		if err := json.Unmarshal(bodies[0], &got); err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{"format": true, "hook": true, "thread_id": true, "run_id": true, "action_id": true, "stage": true, "context_revision": true,
			"control_revision": true, "evidence_ids": true, "code": true, "decision": true}
		for k := range got {
			if !want[k] {
				t.Errorf("%s: the record has a field %q", p, k)
			}
		}
		if got["hook"] != string(p) || got["decision"] != "continue" || got["run_id"] != in.RunID {
			t.Errorf("%s: %v", p, got)
		}
	}
	// Without anywhere to write, it cannot audit, and says so.
	if v := r.Call(context.Background(), validInput(BeforeTool), nil); !v.Failed {
		t.Fatalf("%+v", v)
	}
}

// afterRecorder is a Recorder that keeps what a hook leaves.
type afterRecorder struct {
	purposes []string
	bodies   [][]byte
	err      error
}

func (a *afterRecorder) record(_ context.Context, purpose string, data []byte) (string, error) {
	if a.err != nil {
		return "", a.err
	}
	a.purposes, a.bodies = append(a.purposes, purpose), append(a.bodies, data)
	return identity.NewEvidenceID().String(), nil
}

// TestCallAfterContinuesWithNoDiagnosisWhenTheHooksAnswerValidly: the points after something
// ran their hooks and leave no diagnosis for a valid answer.
func TestCallAfterContinuesWithNoDiagnosisWhenTheHooksAnswerValidly(t *testing.T) {
	var calls atomic.Int32
	r := runnerWith(t, time.Second, func(context.Context, HookInput, Recorder) (HookResult, error) { calls.Add(1); return Continue(), nil })
	for _, p := range []HookPoint{AfterModel, AfterTool, AfterCompact, RunTerminal} {
		rec := &afterRecorder{}
		if err := r.CallAfter(context.Background(), validInput(p), rec.record, nil); err != nil || len(rec.purposes) != 0 {
			t.Fatalf("%s: %v %v", p, err, rec.purposes)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("%d calls", calls.Load())
	}
	// A runner with nothing to call, and no runner, call nothing and leave nothing.
	empty, _ := NewRunner(nil, Options{})
	var none *Runner
	for _, x := range []*Runner{empty, none} {
		rec := &afterRecorder{}
		if err := x.CallAfter(context.Background(), validInput(AfterModel), rec.record, nil); err != nil || len(rec.purposes) != 0 {
			t.Fatalf("%v %v", err, rec.purposes)
		}
	}
}

// TestCallAfterLeavesADiagnosisForAHookThatDoesNotAnswerValidly: an error, a deny (which the
// point does not allow), a panic and a callback that does not return before the hard limit
// each leave one diagnosis naming the point and the reason, with no content, and the call
// itself returns no error.
func TestCallAfterLeavesADiagnosisForAHookThatDoesNotAnswerValidly(t *testing.T) {
	for name, tc := range map[string]struct {
		cb     Callback
		reason string
	}{
		"an error": {func(context.Context, HookInput, Recorder) (HookResult, error) {
			return HookResult{}, errors.New("secret detail")
		}, "error"},
		"a deny":  {func(context.Context, HookInput, Recorder) (HookResult, error) { return Deny("TOO_LATE"), nil }, "invalid_result"},
		"a panic": {func(context.Context, HookInput, Recorder) (HookResult, error) { panic("boom") }, "error"},
		"no return in time": {func(ctx context.Context, _ HookInput, _ Recorder) (HookResult, error) {
			<-ctx.Done()
			return Continue(), nil
		}, "timeout"},
	} {
		t.Run(name, func(t *testing.T) {
			r := runnerWith(t, 30*time.Millisecond, tc.cb)
			in := validInput(AfterModel)
			rec := &afterRecorder{}
			var told []string
			diag := func(format string, args ...any) { told = append(told, format) }
			if err := r.CallAfter(context.Background(), in, rec.record, diag); err != nil {
				t.Fatal(err)
			}
			if len(rec.purposes) != 1 || rec.purposes[0] != PurposeHookDiagnosis || len(told) == 0 {
				t.Fatalf("%v %v", rec.purposes, told)
			}
			var got map[string]any
			if err := json.Unmarshal(rec.bodies[0], &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != 4 || got["format"] != "rencrow-hook-diagnosis/v1" || got["hook"] != "after_model" || got["action_id"] != *in.ActionID || got["reason"] != tc.reason {
				t.Fatalf("%v", got)
			}
			if strings.Contains(string(rec.bodies[0]), "secret detail") {
				t.Fatal("the diagnosis carries what the hook said")
			}
		})
	}
}

// TestCallAfterRunsWhenTheRunIsBeingStoppedAndSurfacesOnlyAStoreError: the thing happened, so
// the hook is called under a context that the Run's stop does not cancel; a diagnosis that
// cannot be written is not an error, except when the writer is gone, which the caller sees.
func TestCallAfterRunsWhenTheRunIsBeingStoppedAndSurfacesOnlyAStoreError(t *testing.T) {
	var calls atomic.Int32
	r := runnerWith(t, time.Second, func(ctx context.Context, _ HookInput, _ Recorder) (HookResult, error) {
		if ctx.Err() != nil {
			return HookResult{}, ctx.Err()
		}
		calls.Add(1)
		return Continue(), nil
	})
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	rec := &afterRecorder{}
	if err := r.CallAfter(stopped, validInput(RunTerminal), rec.record, nil); err != nil || calls.Load() != 1 || len(rec.purposes) != 0 {
		t.Fatalf("%v calls=%d %v", err, calls.Load(), rec.purposes)
	}

	failing := runnerWith(t, time.Second, func(context.Context, HookInput, Recorder) (HookResult, error) { return HookResult{}, errors.New("no") })
	if err := failing.CallAfter(context.Background(), validInput(AfterTool), (&afterRecorder{err: errors.New("disk full")}).record, nil); err != nil {
		t.Fatalf("a diagnosis that was not written is not an error: %v", err)
	}
	lost := errors.New("writer lost")
	if err := failing.CallAfter(context.Background(), validInput(AfterTool), (&afterRecorder{err: Fatal(lost)}).record, nil); !errors.Is(err, ErrFatal) || !errors.Is(err, lost) {
		t.Fatalf("the writer being gone is the caller's to see: %v", err)
	}
	audit, _ := NewRunner([]string{AuditMetadata}, Options{})
	if err := audit.CallAfter(context.Background(), validInput(AfterTool), (&afterRecorder{err: Fatal(lost)}).record, nil); !errors.Is(err, ErrFatal) {
		t.Fatalf("%v", err)
	}
}

func TestTheDiagnosisOfAPointWithNoActionNamesNone(t *testing.T) {
	raw, err := EncodeDiagnosis(RunTerminal, nil, "timeout")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["hook"] != "run_terminal" || got["action_id"] != nil || got["reason"] != "timeout" || got["format"] != "rencrow-hook-diagnosis/v1" {
		t.Fatalf("%v", got)
	}
}
