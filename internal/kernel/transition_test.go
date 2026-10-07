package kernel_test

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/kernel"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

var (
	t0       = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	deadline = t0.Add(30 * time.Minute)
	limits   = protocol.Limits{MaxModelSteps: 10, MaxToolCallsPerStep: 8, DeadlineSeconds: 1800, MaxCaptureBytes: 1 << 20, MaxGenerationAttempts: 32}
)

func at(d time.Duration) time.Time { return t0.Add(d) }

func state(p kernel.Phase) kernel.State {
	s := kernel.NewState(limits, deadline, 0, 0)
	s.Phase = p
	if p == kernel.PhasePreparingAction || p == kernel.PhaseExecuting {
		s.ToolTotal = 2 // a response with two calls, the first being acted on
	}
	return s
}

var allEvents = []kernel.EventKind{
	kernel.EvStart, kernel.EvLoaded, kernel.EvAssembled, kernel.EvMeasured, kernel.EvGenerated, kernel.EvFinalValid, kernel.EvFailed, kernel.EvPersisted,
	kernel.EvPrepared, kernel.EvToolSettled, kernel.EvObserved, kernel.EvRetryDue,
	kernel.EvCompactReady, kernel.EvCommitted, kernel.EvCommitStale, kernel.EvCompactSkipped,
}

func event(k kernel.EventKind) kernel.Event {
	ev := kernel.Event{Kind: k, At: at(time.Second)}
	switch k {
	case kernel.EvMeasured:
		ev.Fit = contextplan.VerdictFit
	case kernel.EvGenerated:
		ev.Gen = kernel.Generated{Kind: modelport.KindFinal, GenerationState: modelport.StateTerminal}
	case kernel.EvFailed:
		ev.Failure = kernel.Failure{Code: modelport.CodeUpstreamTransient, GenerationState: modelport.StateTerminal}
	case kernel.EvToolSettled:
		ev.Tool = kernel.ToolEnd{Effect: "completed"}
	}
	return ev
}

// TestEveryPhaseAgainstEveryEvent is the transition table. For each of the sixteen
// phases and each event, either the one transition it has or the refusal the phase
// gets: a Terminal Run is terminal, a phase this build does not run is refused, and
// an event that does not belong to a phase is refused rather than ignored.
func TestEveryPhaseAgainstEveryEvent(t *testing.T) {
	next := map[kernel.Phase]map[kernel.EventKind]kernel.Phase{
		kernel.PhaseAdmitting:             {kernel.EvStart: kernel.PhaseLoading, kernel.EvFailed: kernel.PhasePersistingResult},
		kernel.PhaseLoading:               {kernel.EvLoaded: kernel.PhaseAssembling, kernel.EvFailed: kernel.PhasePersistingResult},
		kernel.PhaseAssembling:            {kernel.EvAssembled: kernel.PhaseMeasuring, kernel.EvFailed: kernel.PhasePersistingResult},
		kernel.PhaseMeasuring:             {kernel.EvMeasured: kernel.PhaseGenerating, kernel.EvFailed: kernel.PhasePersistingResult},
		kernel.PhaseGenerating:            {kernel.EvGenerated: kernel.PhaseValidatingFinal, kernel.EvFailed: kernel.PhasePersistingResult},
		kernel.PhaseRetryWaiting:          {kernel.EvRetryDue: kernel.PhaseMeasuring, kernel.EvFailed: kernel.PhasePersistingResult},
		kernel.PhasePreparingAction:       {kernel.EvPrepared: kernel.PhaseExecuting, kernel.EvFailed: kernel.PhasePersistingResult},
		kernel.PhaseExecuting:             {kernel.EvToolSettled: kernel.PhaseExecuting, kernel.EvFailed: kernel.PhasePersistingResult},
		kernel.PhasePersistingObservation: {kernel.EvObserved: kernel.PhaseAssembling, kernel.EvFailed: kernel.PhasePersistingResult},
		kernel.PhaseCompacting:            {kernel.EvCompactReady: kernel.PhaseCommittingCheckpoint, kernel.EvFailed: kernel.PhasePersistingResult},
		kernel.PhaseCommittingCheckpoint:  {kernel.EvCommitted: kernel.PhaseAssembling, kernel.EvCommitStale: kernel.PhaseAssembling, kernel.EvFailed: kernel.PhasePersistingResult},
		kernel.PhaseValidatingFinal:       {kernel.EvFinalValid: kernel.PhasePersistingResult, kernel.EvFailed: kernel.PhasePersistingResult},
		kernel.PhasePersistingResult:      {kernel.EvPersisted: kernel.PhaseTerminal},
	}
	if len(kernel.Phases()) != 16 {
		t.Fatalf("%d phases", len(kernel.Phases()))
	}
	for _, p := range kernel.Phases() {
		for _, k := range allEvents {
			got, effects, err := kernel.Transition(state(p), event(k))
			switch {
			case p == kernel.PhaseTerminal:
				if !errors.Is(err, kernel.ErrTerminal) || len(effects) != 0 || got.Phase != p {
					t.Errorf("%s/%s: a terminal run moved or did not say so: %v", p, k, err)
				}
			case !p.Reachable():
				if !errors.Is(err, kernel.ErrUnreachablePhase) || len(effects) != 0 || got.Phase != p {
					t.Errorf("%s/%s: a phase this build does not run was acted on: %v", p, k, err)
				}
			default:
				want, ok := next[p][k]
				if !ok {
					if !errors.Is(err, kernel.ErrInvalidTransition) || len(effects) != 0 || got.Phase != p {
						t.Errorf("%s/%s: accepted (%s, %v), want ErrInvalidTransition", p, k, got.Phase, err)
					}
					continue
				}
				if err != nil || got.Phase != want {
					t.Errorf("%s/%s: phase %s (%v), want %s", p, k, got.Phase, err, want)
				}
			}
		}
	}
}

// TestUnreachablePhasesAreNeverReached explores every state the machine can get to
// from a new Run, over every event and every payload that matters, and finds exactly
// the fourteen phases of the path (the eight of an answer, the three of the Tool
// exchange, the backoff before a retry, and the two of a compaction) when the Run may
// compact, and the twelve without the two of a compaction when it may not. Cancelling and
// Recovering exist as values and are never entered.
func TestUnreachablePhasesAreNeverReached(t *testing.T) {
	variants := func(k kernel.EventKind) []kernel.Event {
		base := event(k)
		switch k {
		case kernel.EvMeasured:
			var out []kernel.Event
			for _, v := range []contextplan.Verdict{contextplan.VerdictFit, contextplan.VerdictNoFit, contextplan.VerdictStraddle, contextplan.VerdictUnverified} {
				e := base
				e.Fit = v
				out = append(out, e)
			}
			return out
		case kernel.EvGenerated:
			var out []kernel.Event
			for _, kind := range []string{modelport.KindFinal, modelport.KindToolCalls, modelport.KindIncomplete, modelport.KindRefused, modelport.KindError, "other"} {
				for _, st := range []string{modelport.StateTerminal, modelport.StateUnknown, modelport.StateNotStarted} {
					e := base
					e.Gen = kernel.Generated{Kind: kind, FailureCode: modelport.CodeLength, GenerationState: st}
					out = append(out, e)
					e.Gen.ToolsOffered, e.Gen.ToolCalls = true, 2
					out = append(out, e)
				}
			}
			// A failure F31 retries: the way into RetryWaiting.
			retried := base
			retried.Gen = kernel.Generated{Kind: modelport.KindError, FailureCode: modelport.CodeReasoningOnly, GenerationState: modelport.StateTerminal,
				Offers: kernel.RecoveryOffers{SameRequest: kernel.OfferAllowed}}
			out = append(out, retried)
			return out
		case kernel.EvToolSettled:
			var out []kernel.Event
			for _, eff := range []string{"completed", "failed", "not_started", "cancelled", "unknown", "other"} {
				e := base
				e.Tool = kernel.ToolEnd{Effect: eff}
				out = append(out, e)
				stop := kernel.Failure{Code: kernel.CodeEffectOutcomeUnknown}
				e.Tool.Failure = &stop
				out = append(out, e)
			}
			return out
		case kernel.EvFailed:
			var out []kernel.Event
			for _, c := range kernel.ClassifiedCodes() {
				e := base
				e.Failure = kernel.Failure{Code: c}
				out = append(out, e)
			}
			return out
		case kernel.EvStart, kernel.EvLoaded, kernel.EvAssembled, kernel.EvPrepared, kernel.EvObserved:
			late := base
			late.At = deadline
			return []kernel.Event{base, late}
		}
		return []kernel.Event{base}
	}
	for _, compacts := range []bool{true, false} {
		explore(t, variants, compacts)
	}
}

func explore(t *testing.T, variants func(k kernel.EventKind) []kernel.Event, compacts bool) {
	t.Helper()
	seen := map[kernel.Phase]bool{kernel.PhaseAdmitting: true}
	first := state(kernel.PhaseAdmitting)
	first.Compaction = compacts
	queue := []kernel.State{first}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		for _, k := range allEvents {
			for _, ev := range variants(k) {
				n, _, err := kernel.Transition(s, ev)
				if err != nil {
					continue
				}
				if !seen[n.Phase] {
					seen[n.Phase] = true
					queue = append(queue, n)
				}
			}
		}
	}
	for _, p := range kernel.Phases() {
		compactionPhase := p == kernel.PhaseCompacting || p == kernel.PhaseCommittingCheckpoint
		if want := p.Reachable() && (compacts || !compactionPhase); seen[p] != want {
			t.Errorf("compaction %t, %s: reached=%t, want %t", compacts, p, seen[p], want)
		}
	}
	if want := map[bool]int{true: 14, false: 12}[compacts]; len(seen) != want {
		t.Fatalf("compaction %t: reached %v", compacts, seen)
	}
}

func run(t *testing.T, s kernel.State, evs ...kernel.Event) (kernel.State, []kernel.Effect) {
	t.Helper()
	var effects []kernel.Effect
	for _, ev := range evs {
		var err error
		var e []kernel.Effect
		if s, e, err = kernel.Transition(s, ev); err != nil {
			t.Fatalf("%s: %v", ev.Kind, err)
		}
		effects = append(effects, e...)
	}
	return s, effects
}

func kinds(effects []kernel.Effect) []kernel.EffectKind {
	var out []kernel.EffectKind
	for _, e := range effects {
		out = append(out, e.Kind)
	}
	return out
}

func TestHappyPath(t *testing.T) {
	s, effects := run(t, state(kernel.PhaseAdmitting),
		event(kernel.EvStart), event(kernel.EvLoaded), event(kernel.EvAssembled), event(kernel.EvMeasured),
		event(kernel.EvGenerated), event(kernel.EvFinalValid))
	want := []kernel.EffectKind{kernel.EffLoad, kernel.EffAssemble, kernel.EffMeasure, kernel.EffGenerate, kernel.EffValidateFinal, kernel.EffPersistResult}
	if !slices.Equal(kinds(effects), want) {
		t.Fatalf("effects %v", kinds(effects))
	}
	if s.Phase != kernel.PhasePersistingResult || s.Pending == nil || s.Pending.Status != kernel.StatusCompleted || s.Pending.Code != kernel.CodeFinalAccepted || s.Pending.Resumable {
		t.Fatalf("%+v %+v", s, s.Pending)
	}
	if s.ModelStepsUsed != 1 || s.GenerationAttemptsUsed != 1 {
		t.Fatalf("one step and one attempt were consumed: %+v", s)
	}
	s, effects = run(t, s, event(kernel.EvPersisted))
	if s.Phase != kernel.PhaseTerminal || len(effects) != 0 {
		t.Fatalf("%+v %v", s, effects)
	}
}

func TestDeadlineEndsTheRunBeforeEachStepOfThePath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase kernel.Phase
		ev    kernel.EventKind
	}{
		{"start", kernel.PhaseAdmitting, kernel.EvStart},
		{"loaded", kernel.PhaseLoading, kernel.EvLoaded},
		{"assembled", kernel.PhaseAssembling, kernel.EvAssembled},
		{"measured", kernel.PhaseMeasuring, kernel.EvMeasured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := event(tc.ev)
			ev.At = deadline.Add(-time.Second)
			if s, _, err := kernel.Transition(state(tc.phase), ev); err != nil || s.Phase == kernel.PhasePersistingResult {
				t.Fatalf("one second before the deadline the run goes on: %v %+v", err, s)
			}
			ev.At = deadline // the deadline is not a time the run may still start something at
			s, effects, err := kernel.Transition(state(tc.phase), ev)
			if err != nil || s.Phase != kernel.PhasePersistingResult || s.Pending.Status != kernel.StatusIncomplete || s.Pending.Code != kernel.CodeDeadlineExceeded ||
				!s.Pending.Resumable || !slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffPersistResult}) {
				t.Fatalf("%v %+v %+v", err, s, s.Pending)
			}
		})
	}
}

func TestBudgetsEndTheRunBeforeAGenerationIsSent(t *testing.T) {
	s := state(kernel.PhaseMeasuring)
	s.ModelStepsUsed = limits.MaxModelSteps
	n, effects, err := kernel.Transition(s, event(kernel.EvMeasured))
	if err != nil || n.Pending == nil || n.Pending.Code != kernel.CodeStepBudgetExhausted || n.Pending.Status != kernel.StatusIncomplete || kinds(effects)[0] != kernel.EffPersistResult {
		t.Fatalf("%v %+v", err, n.Pending)
	}
	s = state(kernel.PhaseMeasuring)
	s.GenerationAttemptsUsed = limits.MaxGenerationAttempts
	n, _, err = kernel.Transition(s, event(kernel.EvMeasured))
	if err != nil || n.Pending == nil || n.Pending.Code != kernel.CodeGenerationBudgetExhausted || n.Pending.Status != kernel.StatusIncomplete {
		t.Fatalf("%v %+v", err, n.Pending)
	}
	// One short of the budget still generates, and uses the last attempt.
	s = state(kernel.PhaseMeasuring)
	s.GenerationAttemptsUsed = limits.MaxGenerationAttempts - 1
	n, effects, err = kernel.Transition(s, event(kernel.EvMeasured))
	if err != nil || n.Phase != kernel.PhaseGenerating || n.GenerationAttemptsUsed != limits.MaxGenerationAttempts || kinds(effects)[0] != kernel.EffGenerate {
		t.Fatalf("%v %+v", err, n)
	}
}

// TestOnlyAVerifiedFitGenerates is the capacity rule of the machine: every verdict
// that is not a verified fit ends the Run without a generation effect.
func TestOnlyAVerifiedFitGenerates(t *testing.T) {
	for v, want := range map[contextplan.Verdict]struct{ status, code string }{
		contextplan.VerdictNoFit:      {kernel.StatusBlocked, kernel.CodeCapacityBlocked},
		contextplan.VerdictStraddle:   {kernel.StatusBlocked, modelport.CodeBudgetUnverified},
		contextplan.VerdictUnverified: {kernel.StatusBlocked, modelport.CodeBudgetUnverified},
	} {
		ev := event(kernel.EvMeasured)
		ev.Fit = v
		n, effects, err := kernel.Transition(state(kernel.PhaseMeasuring), ev)
		if err != nil || n.Phase != kernel.PhasePersistingResult || n.Pending.Status != want.status || n.Pending.Code != want.code ||
			slices.Contains(kinds(effects), kernel.EffGenerate) || n.GenerationAttemptsUsed != 0 {
			t.Errorf("%s: %v %+v %v", v, err, n.Pending, kinds(effects))
		}
	}
	ev := event(kernel.EvMeasured)
	ev.Fit = "guess"
	if _, _, err := kernel.Transition(state(kernel.PhaseMeasuring), ev); !errors.Is(err, kernel.ErrInvalidTransition) {
		t.Fatalf("an unknown verdict was accepted: %v", err)
	}
}

func TestGenerationResultsDecideTheRunsEnd(t *testing.T) {
	gen := func(kind, code, st string) kernel.Event {
		return kernel.Event{Kind: kernel.EvGenerated, At: at(time.Second), Gen: kernel.Generated{Kind: kind, FailureCode: code, GenerationState: st}}
	}
	for _, tc := range []struct {
		name       string
		ev         kernel.Event
		phase      kernel.Phase
		status     string
		code       string
		unresolved bool
	}{
		{"final", gen(modelport.KindFinal, "", modelport.StateTerminal), kernel.PhaseValidatingFinal, "", "", false},
		{"final from a generation that is not shown to have ended", gen(modelport.KindFinal, "", modelport.StateUnknown), kernel.PhasePersistingResult, kernel.StatusBlocked, modelport.CodeOutcomeUnknown, true},
		{"tool calls where none were offered", gen(modelport.KindToolCalls, "", modelport.StateTerminal), kernel.PhasePersistingResult, kernel.StatusFailed, modelport.CodeContractFailed, false},
		{"length", gen(modelport.KindIncomplete, modelport.CodeLength, modelport.StateTerminal), kernel.PhasePersistingResult, kernel.StatusIncomplete, kernel.CodeModelOutputTruncated, false},
		{"refusal", gen(modelport.KindRefused, modelport.CodeRefused, modelport.StateTerminal), kernel.PhasePersistingResult, kernel.StatusRejected, kernel.CodeModelRefused, false},
		{"reasoning only", gen(modelport.KindError, modelport.CodeReasoningOnly, modelport.StateTerminal), kernel.PhasePersistingResult, kernel.StatusFailed, kernel.CodeModelOutputInvalid, false},
		{"EOF before the terminal", gen(modelport.KindError, modelport.CodeOutcomeUnknown, modelport.StateUnknown), kernel.PhasePersistingResult, kernel.StatusBlocked, modelport.CodeOutcomeUnknown, true},
		{"a refusal whose end is unknown stays unknown", gen(modelport.KindRefused, modelport.CodeRefused, modelport.StateUnknown), kernel.PhasePersistingResult, kernel.StatusBlocked, modelport.CodeOutcomeUnknown, true},
		{"an unknown kind", gen("surprise", "", modelport.StateTerminal), kernel.PhasePersistingResult, kernel.StatusFailed, modelport.CodeContractFailed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, effects, err := kernel.Transition(state(kernel.PhaseGenerating), tc.ev)
			if err != nil || n.Phase != tc.phase {
				t.Fatalf("%v %s", err, n.Phase)
			}
			if tc.phase == kernel.PhaseValidatingFinal {
				if !slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffValidateFinal}) {
					t.Fatalf("%v", kinds(effects))
				}
				return
			}
			if n.Pending.Status != tc.status || n.Pending.Code != tc.code || n.Pending.UnresolvedModelAction != tc.unresolved {
				t.Fatalf("%+v", n.Pending)
			}
			if !slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffPersistResult}) {
				t.Fatalf("a failed generation must go to the result and nowhere else: %v", kinds(effects))
			}
		})
	}
}

func TestTransitionIsPureAndLeavesItsInputAlone(t *testing.T) {
	s := state(kernel.PhaseMeasuring)
	before := s
	a, ea, err1 := kernel.Transition(s, event(kernel.EvMeasured))
	b, eb, err2 := kernel.Transition(s, event(kernel.EvMeasured))
	if err1 != nil || err2 != nil || !reflect.DeepEqual(a, b) || !reflect.DeepEqual(ea, eb) {
		t.Fatal("the same state and event gave different results")
	}
	if !reflect.DeepEqual(s, before) {
		t.Fatal("Transition changed the state it was given")
	}
	// A failure's outcome is a fresh value: changing it does not reach back.
	f, _, _ := kernel.Transition(s, kernel.Event{Kind: kernel.EvFailed, Failure: kernel.Failure{Code: modelport.CodeRefused}})
	f.Pending.Code = "changed"
	g, _, _ := kernel.Transition(s, kernel.Event{Kind: kernel.EvFailed, Failure: kernel.Failure{Code: modelport.CodeRefused}})
	if g.Pending.Code != kernel.CodeModelRefused {
		t.Fatal("the outcome was shared between transitions")
	}
}

// TestThePureFilesTouchNothingOutside holds the pure part of the kernel to what its
// package comment says: no clock, no randomness, no store, no file, no network, no
// ID issuer. The files are read as source: a call to time.Now, or an import that
// could reach the outside, fails here.
func TestThePureFilesTouchNothingOutside(t *testing.T) {
	allowed := map[string]bool{
		"errors": true, "fmt": true, "slices": true, "sort": true, "time": true,
		"github.com/Nyukimin/RenCrow_Harness/internal/contextplan": true,
		"github.com/Nyukimin/RenCrow_Harness/internal/modelport":   true,
		"github.com/Nyukimin/RenCrow_Harness/pkg/protocol":         true,
	}
	forbiddenTime := map[string]bool{"Now": true, "Since": true, "Until": true, "Sleep": true, "After": true, "AfterFunc": true, "Tick": true, "NewTimer": true, "NewTicker": true}
	fset := token.NewFileSet()
	for _, name := range []string{"phase.go", "transition.go", "failure.go", "result.go", "retry.go"} {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if !allowed[path] {
				t.Errorf("%s imports %s, which the pure kernel may not", name, path)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "time" && forbiddenTime[sel.Sel.Name] {
					t.Errorf("%s calls time.%s: the driver supplies the time", name, sel.Sel.Name)
				}
			}
			return true
		})
		if strings.Contains(name, "_test") {
			t.Fatal("test files are not pure files")
		}
	}
}

func toolGen(n int) kernel.Event {
	ev := event(kernel.EvGenerated)
	ev.Gen = kernel.Generated{Kind: modelport.KindToolCalls, GenerationState: modelport.StateTerminal, ToolsOffered: true, ToolCalls: n}
	return ev
}

func settled(effect string) kernel.Event {
	ev := event(kernel.EvToolSettled)
	ev.Tool = kernel.ToolEnd{Effect: effect}
	return ev
}

// TestAResponseWithToolCallsIsActedOnOneCallAtATimeAndThenAnsweredAgain is the Tool
// exchange: bind all the calls, run them in order, apply the exchange, and generate
// again as a new step.
func TestAResponseWithToolCallsIsActedOnOneCallAtATimeAndThenAnsweredAgain(t *testing.T) {
	s, effects := run(t, state(kernel.PhaseGenerating), toolGen(3))
	if s.Phase != kernel.PhasePreparingAction || s.ToolTotal != 3 || s.ToolNext != 0 || !slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffPrepareBatch}) {
		t.Fatalf("%+v %v", s, kinds(effects))
	}
	s, effects = run(t, s, event(kernel.EvPrepared))
	if s.Phase != kernel.PhaseExecuting || s.ToolNext != 0 || !slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffExecuteTool}) {
		t.Fatalf("%+v %v", s, kinds(effects))
	}
	for i := 1; i < 3; i++ {
		s, effects = run(t, s, settled("completed"))
		if s.Phase != kernel.PhaseExecuting || s.ToolNext != i || !slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffExecuteTool}) {
			t.Fatalf("call %d: %+v %v", i, s, kinds(effects))
		}
	}
	s, effects = run(t, s, settled("completed"))
	if s.Phase != kernel.PhasePersistingObservation || effects[0].Kind != kernel.EffPersistObservation || effects[0].Reason != "" {
		t.Fatalf("%+v %+v", s, effects)
	}
	s, effects = run(t, s, event(kernel.EvObserved))
	if s.Phase != kernel.PhaseAssembling || s.ToolTotal != 0 || s.ToolNext != 0 || !slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffAssemble}) {
		t.Fatalf("%+v %v", s, kinds(effects))
	}
	// The next generation is a new step under the same budgets.
	s, _ = run(t, s, event(kernel.EvAssembled), event(kernel.EvMeasured))
	if s.Phase != kernel.PhaseGenerating || s.ModelStepsUsed != 1 {
		t.Fatalf("%+v", s)
	}
}

// TestACallThatDidNotSucceedStopsTheRestOfItsResponse: the later calls were written
// before the model knew, so none runs; the exchange is closed with the reason.
func TestACallThatDidNotSucceedStopsTheRestOfItsResponse(t *testing.T) {
	for _, effect := range []string{"failed", "not_started"} {
		s, _ := run(t, state(kernel.PhaseGenerating), toolGen(3), event(kernel.EvPrepared))
		s, effects := run(t, s, settled("completed"), settled(effect))
		if s.Phase != kernel.PhasePersistingObservation || len(effects) != 2 || effects[1].Kind != kernel.EffPersistObservation || effects[1].Reason != kernel.ReasonEarlierCallFailed {
			t.Fatalf("%s: %+v %+v", effect, s, effects)
		}
		// The last call failing leaves nothing unrun, so no reason is given.
		s, _ = run(t, state(kernel.PhaseGenerating), toolGen(2), event(kernel.EvPrepared), settled("completed"))
		_, effects = run(t, s, settled(effect))
		if effects[0].Kind != kernel.EffPersistObservation || effects[0].Reason != "" {
			t.Fatalf("%s: %+v", effect, effects)
		}
	}
}

// TestCallsThatEndTheRun: an unknown effect, a cancellation and any failure the driver
// attaches end the Run through PersistingResult, each as its own classification.
func TestCallsThatEndTheRun(t *testing.T) {
	for name, tc := range map[string]struct {
		ev           kernel.Event
		status, code string
	}{
		"an unknown effect": {settled("unknown"), "blocked", "EFFECT_OUTCOME_UNKNOWN"},
		"cancelled":         {settled("cancelled"), "incomplete", "DRIVER_STOPPED"},
		"a stop attached": {func() kernel.Event {
			e := settled("not_started")
			e.Tool.Failure = &kernel.Failure{Code: kernel.CodePolicyAmbiguous}
			return e
		}(), "blocked", "POLICY_AMBIGUOUS"},
		"a cancellation attached": {func() kernel.Event {
			e := settled("not_started")
			e.Tool.Failure = &kernel.Failure{Code: modelport.CodeCancelled}
			return e
		}(), "cancelled", "CANCELLED"},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := run(t, state(kernel.PhaseGenerating), toolGen(2), event(kernel.EvPrepared))
			s, effects := run(t, s, tc.ev)
			if s.Phase != kernel.PhasePersistingResult || s.Pending == nil || s.Pending.Status != tc.status || s.Pending.Code != tc.code ||
				!slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffPersistResult}) {
				t.Fatalf("%+v %+v", s, s.Pending)
			}
		})
	}
	// A state this machine does not know is refused, never guessed.
	s, _ := run(t, state(kernel.PhaseGenerating), toolGen(1), event(kernel.EvPrepared))
	if _, _, err := kernel.Transition(s, settled("running")); !errors.Is(err, kernel.ErrInvalidTransition) {
		t.Fatalf("%v", err)
	}
}

// TestTheDeadlineStopsTheExchangeBetweenCalls: time is checked before each next step.
func TestTheDeadlineStopsTheExchangeBetweenCalls(t *testing.T) {
	s, _ := run(t, state(kernel.PhaseGenerating), toolGen(2), event(kernel.EvPrepared))
	late := settled("completed")
	late.At = deadline
	s, effects, err := kernel.Transition(s, late)
	if err != nil || s.Phase != kernel.PhasePersistingResult || s.Pending.Code != kernel.CodeDeadlineExceeded || !slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffPersistResult}) {
		t.Fatalf("%+v %v %v", s, kinds(effects), err)
	}
}
