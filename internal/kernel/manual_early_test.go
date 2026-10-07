package kernel_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/kernel"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

func measuredAt(fit contextplan.Verdict, early bool) kernel.Event {
	ev := event(kernel.EvMeasured)
	ev.Fit, ev.Early = fit, early
	return ev
}

// TestACompactionStartedByTheTriggerRatioIsOptionalAndTriedOncePerRun: a verified fit that reached
// the ratio is compacted before it is generated on, once for the step; a compaction that comes
// to no checkpoint lets the Run generate on the prompt it measured, spending only what the
// stages spent; and the trigger is not tried again in that Run.
func TestACompactionStartedByTheTriggerRatioIsOptionalAndTriedOncePerRun(t *testing.T) {
	measuring := func() kernel.State { s := state(kernel.PhaseMeasuring); s.Compaction = true; return s }

	t.Run("a fit that reached the ratio is compacted", func(t *testing.T) {
		s, effects := run(t, measuring(), measuredAt(contextplan.VerdictFit, true))
		if s.Phase != kernel.PhaseCompacting || !s.Early || !slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffCompact}) || s.ModelStepsUsed != 0 || s.GenerationAttemptsUsed != 0 {
			t.Fatalf("%+v %v: nothing is generated or consumed before the compaction", s, kinds(effects))
		}
	})
	t.Run("a fit that did not is generated on", func(t *testing.T) {
		s, effects := run(t, measuring(), measuredAt(contextplan.VerdictFit, false))
		if s.Phase != kernel.PhaseGenerating || s.Early || !slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffGenerate}) {
			t.Fatalf("%+v %v", s, kinds(effects))
		}
	})
	t.Run("only where compaction is enabled, for a first Attempt, once for the step", func(t *testing.T) {
		off := state(kernel.PhaseMeasuring) // Compaction false
		retry := measuring()
		retry.Attempt = 1
		done := measuring()
		done.Compacted = true
		for name, s := range map[string]kernel.State{"disabled": off, "a retried Attempt": retry, "already compacted for the step": done} {
			n, _, err := kernel.Transition(s, measuredAt(contextplan.VerdictFit, true))
			if err != nil || n.Phase != kernel.PhaseGenerating || n.Early {
				t.Errorf("%s: %+v %v", name, n, err)
			}
		}
	})
	t.Run("a count that is not a verified fit never starts one early", func(t *testing.T) {
		for _, v := range []contextplan.Verdict{contextplan.VerdictStraddle, contextplan.VerdictUnverified} {
			n, _, err := kernel.Transition(measuring(), measuredAt(v, true))
			if err != nil || n.Phase != kernel.PhasePersistingResult || n.Pending == nil || n.Pending.Code != modelport.CodeBudgetUnverified {
				t.Errorf("%s: %+v %v", v, n, err)
			}
		}
	})
	t.Run("a compaction without a checkpoint is skipped past, and not tried again", func(t *testing.T) {
		s, effects := run(t, measuring(), measuredAt(contextplan.VerdictFit, true), kernel.Event{Kind: kernel.EvCompactSkipped, At: at(2 * time.Second), Compact: kernel.CompactReady{Generations: 1}})
		if s.Phase != kernel.PhaseGenerating || s.Early || !s.EarlyTried || s.GenerationAttemptsUsed != 2 || s.ModelStepsUsed != 1 ||
			!slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffCompact, kernel.EffGenerate}) {
			t.Fatalf("%+v %v: one attempt for the stage, one for the act, one step", s, kinds(effects))
		}
		// Later in the Run: the trigger is not tried again, and a prompt that does not fit is
		// compacted as ever.
		s.Phase, s.Compacted = kernel.PhaseMeasuring, false
		if n, _, err := kernel.Transition(s, measuredAt(contextplan.VerdictFit, true)); err != nil || n.Phase != kernel.PhaseGenerating {
			t.Fatalf("%+v %v", n, err)
		}
		if n, _, err := kernel.Transition(s, measuredAt(contextplan.VerdictNoFit, false)); err != nil || n.Phase != kernel.PhaseCompacting || n.Early {
			t.Fatalf("%+v %v", n, err)
		}
	})
	t.Run("what the skipped compaction spent counts against the Run's budgets and deadline", func(t *testing.T) {
		s, _ := run(t, measuring(), measuredAt(contextplan.VerdictFit, true))
		tight := s
		tight.Limits.MaxGenerationAttempts = 1
		n, _, err := kernel.Transition(tight, kernel.Event{Kind: kernel.EvCompactSkipped, At: at(2 * time.Second), Compact: kernel.CompactReady{Generations: 1}})
		if err != nil || n.Phase != kernel.PhasePersistingResult || n.Pending.Code != kernel.CodeGenerationBudgetExhausted {
			t.Fatalf("%+v %v", n, err)
		}
		n, _, err = kernel.Transition(s, kernel.Event{Kind: kernel.EvCompactSkipped, At: deadline.Add(time.Second)})
		if err != nil || n.Phase != kernel.PhasePersistingResult || n.Pending.Code != kernel.CodeDeadlineExceeded {
			t.Fatalf("%+v %v", n, err)
		}
	})
	t.Run("only an early compaction may be skipped", func(t *testing.T) {
		_, _, err := kernel.Transition(enabled(state(kernel.PhaseCompacting)), kernel.Event{Kind: kernel.EvCompactSkipped, At: at(time.Second)})
		if !errors.Is(err, kernel.ErrInvalidTransition) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a candidate that goes stale lets an early compaction go, and ends any other Run", func(t *testing.T) {
		early, _ := run(t, measuring(), measuredAt(contextplan.VerdictFit, true), event(kernel.EvCompactReady))
		n, _, err := kernel.Transition(early, event(kernel.EvCommitStale))
		if err != nil || n.Phase != kernel.PhaseAssembling || n.Early || !n.EarlyTried || n.CompactStale != 0 {
			t.Fatalf("%+v %v", n, err)
		}
		hard, _ := run(t, measuring(), measuredAt(contextplan.VerdictNoFit, false), event(kernel.EvCompactReady))
		first, _, err := kernel.Transition(hard, event(kernel.EvCommitStale))
		if err != nil || first.Phase != kernel.PhaseAssembling || first.CompactStale != 1 {
			t.Fatalf("%+v %v", first, err)
		}
		first.Phase = kernel.PhaseCommittingCheckpoint
		second, _, err := kernel.Transition(first, event(kernel.EvCommitStale))
		if err != nil || second.Phase != kernel.PhasePersistingResult || second.Pending.Code != kernel.CodeCompactionStale {
			t.Fatalf("%+v %v", second, err)
		}
	})
	t.Run("a committed early compaction goes on to assemble the step again", func(t *testing.T) {
		s, _ := run(t, measuring(), measuredAt(contextplan.VerdictFit, true), event(kernel.EvCompactReady))
		n, _, err := kernel.Transition(s, event(kernel.EvCommitted))
		if err != nil || n.Phase != kernel.PhaseAssembling || n.Early || !n.Compacted {
			t.Fatalf("%+v %v", n, err)
		}
	})
}

// TestAManualCompactionCompactsWhatEverTheCountSaysAndEndsCompleted: a system Run counts the live
// prompt, compacts it whether it fits or not, and ends completed with the code that says it only
// compacted; it never generates, and what a compaction cannot do ends it as it ends any Run.
func TestAManualCompactionCompactsWhatEverTheCountSaysAndEndsCompleted(t *testing.T) {
	manual := func(p kernel.Phase) kernel.State { s := state(p); s.Manual, s.Compaction = true, true; return s }

	for _, v := range []contextplan.Verdict{contextplan.VerdictFit, contextplan.VerdictNoFit} {
		s, effects := run(t, manual(kernel.PhaseMeasuring), measuredAt(v, false))
		if s.Phase != kernel.PhaseCompacting || !slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffCompact}) || s.ModelStepsUsed != 0 || s.GenerationAttemptsUsed != 0 {
			t.Errorf("%s: %+v %v", v, s, kinds(effects))
		}
	}
	for _, v := range []contextplan.Verdict{contextplan.VerdictStraddle, contextplan.VerdictUnverified} {
		s, _ := run(t, manual(kernel.PhaseMeasuring), measuredAt(v, false))
		if s.Phase != kernel.PhasePersistingResult || s.Pending.Code != modelport.CodeBudgetUnverified || s.Pending.Status != kernel.StatusBlocked {
			t.Errorf("%s: %+v", v, s.Pending)
		}
	}
	// The whole path: load, assemble, measure, compact, commit, persist; no generation, ever.
	s, effects := run(t, manual(kernel.PhaseAdmitting), event(kernel.EvStart), event(kernel.EvLoaded), event(kernel.EvAssembled), measuredAt(contextplan.VerdictFit, false),
		kernel.Event{Kind: kernel.EvCompactReady, At: at(time.Second), Compact: kernel.CompactReady{Generations: 1}}, event(kernel.EvCommitted))
	want := []kernel.EffectKind{kernel.EffLoad, kernel.EffAssemble, kernel.EffMeasure, kernel.EffCompact, kernel.EffCommitCheckpoint, kernel.EffPersistResult}
	if !slices.Equal(kinds(effects), want) || s.Phase != kernel.PhasePersistingResult || s.ModelStepsUsed != 0 || s.GenerationAttemptsUsed != 1 {
		t.Fatalf("%v %+v", kinds(effects), s)
	}
	if o := s.Pending; o == nil || o.Status != kernel.StatusCompleted || o.Code != kernel.CodeCompactionCommitted || o.Resumable || o.UnresolvedModelAction {
		t.Fatalf("%+v", o)
	}
	// A stage generation of unknown outcome ends the Run unknown even when the checkpoint was made.
	u, _ := run(t, manual(kernel.PhaseMeasuring), measuredAt(contextplan.VerdictFit, false), kernel.Event{Kind: kernel.EvCompactReady, At: at(time.Second), Compact: kernel.CompactReady{Generations: 1, Unknown: true}}, event(kernel.EvCommitted))
	if u.Pending == nil || u.Pending.Status != kernel.StatusBlocked || u.Pending.Code != modelport.CodeOutcomeUnknown {
		t.Fatalf("%+v", u.Pending)
	}
	// Stale twice ends it, as any Run.
	st, _ := run(t, manual(kernel.PhaseMeasuring), measuredAt(contextplan.VerdictFit, false), event(kernel.EvCompactReady), event(kernel.EvCommitStale))
	if st.Phase != kernel.PhaseAssembling || st.CompactStale != 1 {
		t.Fatalf("%+v", st)
	}
	st, _ = run(t, st, event(kernel.EvAssembled), measuredAt(contextplan.VerdictFit, false), event(kernel.EvCompactReady), event(kernel.EvCommitStale))
	if st.Phase != kernel.PhasePersistingResult || st.Pending.Code != kernel.CodeCompactionStale {
		t.Fatalf("%+v", st.Pending)
	}
	// A system Run never skips: there is no prompt to go on with.
	if _, _, err := kernel.Transition(compactingManual(manual), kernel.Event{Kind: kernel.EvCompactSkipped, At: at(time.Second)}); !errors.Is(err, kernel.ErrInvalidTransition) {
		t.Fatalf("%v", err)
	}
}

func compactingManual(manual func(kernel.Phase) kernel.State) kernel.State {
	return manual(kernel.PhaseCompacting)
}

func enabled(s kernel.State) kernel.State {
	s.Compaction = true
	return s
}
