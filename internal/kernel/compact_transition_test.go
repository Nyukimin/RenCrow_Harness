package kernel_test

import (
	"slices"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/kernel"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

func compacting(p kernel.Phase) kernel.State {
	s := state(p)
	s.Compaction = true
	return s
}

func measured(v contextplan.Verdict) kernel.Event {
	ev := event(kernel.EvMeasured)
	ev.Fit = v
	return ev
}

// TestAPromptThatDoesNotFitIsCompactedOnceAndThenPreparedAgain is the machine's whole use of
// a compaction: a verified no-fit sends the Run to Compacting, a candidate to be committed,
// and, once committed, back to Assembling with what the compaction spent counted; a prompt
// that still does not fit is then a capacity block, not another compaction.
func TestAPromptThatDoesNotFitIsCompactedOnceAndThenPreparedAgain(t *testing.T) {
	s, effects := run(t, compacting(kernel.PhaseMeasuring), measured(contextplan.VerdictNoFit))
	if s.Phase != kernel.PhaseCompacting || !slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffCompact}) || s.GenerationAttemptsUsed != 0 || s.ModelStepsUsed != 0 {
		t.Fatalf("%s %v", s.Phase, kinds(effects))
	}
	ready := event(kernel.EvCompactReady)
	ready.Compact = kernel.CompactReady{Generations: 2}
	s, effects = run(t, s, ready)
	if s.Phase != kernel.PhaseCommittingCheckpoint || !slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffCommitCheckpoint}) || s.GenerationAttemptsUsed != 2 || s.ModelStepsUsed != 0 {
		t.Fatalf("a compaction spends generation attempts and no model step: %+v", s)
	}
	s, effects = run(t, s, event(kernel.EvCommitted))
	if s.Phase != kernel.PhaseAssembling || !slices.Equal(kinds(effects), []kernel.EffectKind{kernel.EffAssemble}) || !s.Compacted {
		t.Fatalf("%s %v", s.Phase, kinds(effects))
	}
	// Prepared again: still no fit is the end, with no second compaction.
	s2, _ := run(t, s, event(kernel.EvAssembled))
	s3, effects := run(t, s2, measured(contextplan.VerdictNoFit))
	if s3.Phase != kernel.PhasePersistingResult || s3.Pending == nil || s3.Pending.Code != kernel.CodeCapacityBlocked || s3.Pending.Status != kernel.StatusBlocked || slices.Contains(kinds(effects), kernel.EffCompact) {
		t.Fatalf("%+v", s3.Pending)
	}
	// A fit generates, and the next step may compact again.
	g, _ := run(t, s2, measured(contextplan.VerdictFit))
	if g.Phase != kernel.PhaseGenerating || g.Compacted || g.ModelStepsUsed != 1 || g.GenerationAttemptsUsed != 3 {
		t.Fatalf("%+v", g)
	}
}

// TestACompactionIsOnlyAnAnswerToAVerifiedNoFitOfAFirstAttempt: nothing else compacts: not a
// Run that may not, not a count that is not verified, not a retried Attempt (its request is
// the one that failed).
func TestACompactionIsOnlyAnAnswerToAVerifiedNoFitOfAFirstAttempt(t *testing.T) {
	for name, tc := range map[string]struct {
		s    kernel.State
		fit  contextplan.Verdict
		code string
	}{
		"a Run that may not compact":  {state(kernel.PhaseMeasuring), contextplan.VerdictNoFit, kernel.CodeCapacityBlocked},
		"an interval across the line": {compacting(kernel.PhaseMeasuring), contextplan.VerdictStraddle, modelport.CodeBudgetUnverified},
		"an unverified count":         {compacting(kernel.PhaseMeasuring), contextplan.VerdictUnverified, modelport.CodeBudgetUnverified},
		"a retried attempt": {func() kernel.State {
			s := compacting(kernel.PhaseMeasuring)
			s.Attempt = 1
			return s
		}(), contextplan.VerdictNoFit, kernel.CodeCapacityBlocked},
	} {
		n, effects, err := kernel.Transition(tc.s, measured(tc.fit))
		if err != nil || n.Phase != kernel.PhasePersistingResult || n.Pending.Code != tc.code || slices.Contains(kinds(effects), kernel.EffCompact) {
			t.Errorf("%s: %v %+v", name, err, n.Pending)
		}
	}
}

// TestACompactionThatLeftAGenerationUnresolvedEndsTheRunAfterItsCheckpoint: the checkpoint is
// stored, and the Run does not generate again while a generation of its own is unresolved.
func TestACompactionThatLeftAGenerationUnresolvedEndsTheRunAfterItsCheckpoint(t *testing.T) {
	s, _ := run(t, compacting(kernel.PhaseMeasuring), measured(contextplan.VerdictNoFit))
	ready := event(kernel.EvCompactReady)
	ready.Compact = kernel.CompactReady{Generations: 1, Unknown: true}
	s, _ = run(t, s, ready)
	s, effects := run(t, s, event(kernel.EvCommitted))
	if s.Phase != kernel.PhasePersistingResult || s.Pending == nil || s.Pending.Status != kernel.StatusBlocked || s.Pending.Code != modelport.CodeOutcomeUnknown ||
		!s.Pending.UnresolvedModelAction || !s.Pending.Resumable || slices.Contains(kinds(effects), kernel.EffAssemble) {
		t.Fatalf("%+v %v", s.Pending, kinds(effects))
	}
}

// TestAStaleCandidateIsMadeAgainFromTheNewContextOnce: not committed; the step is prepared
// again and counted again (its before count is of the new context); the second stale
// candidate of the step ends the Run.
func TestAStaleCandidateIsMadeAgainFromTheNewContextOnce(t *testing.T) {
	s, _ := run(t, compacting(kernel.PhaseCommittingCheckpoint), event(kernel.EvCommitStale))
	if s.Phase != kernel.PhaseAssembling || s.Compacted || s.CompactStale != 1 {
		t.Fatalf("%+v", s)
	}
	s, _ = run(t, s, event(kernel.EvAssembled), measured(contextplan.VerdictNoFit))
	if s.Phase != kernel.PhaseCompacting {
		t.Fatalf("the new context that still does not fit is compacted again: %s", s.Phase)
	}
	s, _ = run(t, s, event(kernel.EvCompactReady))
	s, effects := run(t, s, event(kernel.EvCommitStale))
	if s.Phase != kernel.PhasePersistingResult || s.Pending.Code != kernel.CodeCompactionStale || s.Pending.Status != kernel.StatusIncomplete || !s.Pending.Resumable || len(effects) != 1 || effects[0].Kind != kernel.EffPersistResult {
		t.Fatalf("%+v", s.Pending)
	}
	// A step that went on resets the count.
	g, _ := run(t, compacting(kernel.PhasePersistingObservation), event(kernel.EvObserved))
	if g.CompactStale != 0 || g.Compacted {
		t.Fatalf("%+v", g)
	}
}

// TestACompactionDoesNotOutlastTheDeadline: the machine will not start or commit a step past
// the Run's deadline.
func TestACompactionDoesNotOutlastTheDeadline(t *testing.T) {
	late := func(k kernel.EventKind) kernel.Event {
		ev := event(k)
		ev.At = deadline
		return ev
	}
	for name, tc := range map[string]struct {
		s  kernel.State
		ev kernel.Event
	}{
		"before compacting":      {compacting(kernel.PhaseMeasuring), func() kernel.Event { e := late(kernel.EvMeasured); e.Fit = contextplan.VerdictNoFit; return e }()},
		"before committing":      {compacting(kernel.PhaseCompacting), late(kernel.EvCompactReady)},
		"before preparing again": {compacting(kernel.PhaseCommittingCheckpoint), late(kernel.EvCommitted)},
	} {
		n, effects, err := kernel.Transition(tc.s, tc.ev)
		if err != nil || n.Phase != kernel.PhasePersistingResult || n.Pending.Code != kernel.CodeDeadlineExceeded || len(effects) != 1 || effects[0].Kind != kernel.EffPersistResult {
			t.Errorf("%s: %v %+v", name, err, n.Pending)
		}
	}
	_ = time.Second
}

// TestAFailureOfACompactionEndsTheRunLikeAnyOther: a compaction is a phase like the others, a
// failure in it goes through PersistingResult, and what the compaction learned (a stop, an
// unresolved generation) decides the code by the same classification.
func TestAFailureOfACompactionEndsTheRunLikeAnyOther(t *testing.T) {
	for _, p := range []kernel.Phase{kernel.PhaseCompacting, kernel.PhaseCommittingCheckpoint} {
		for _, f := range []kernel.Failure{
			{Code: modelport.CodeCancelled}, {Code: kernel.CodeCapacityBlocked}, {Code: kernel.CodeIntegrityBlocked}, {Code: kernel.CodePersistenceUncertain},
			{Code: modelport.CodeBudgetUnverified}, {Code: kernel.CodeDriverStopped, GenerationState: modelport.StateUnknown},
		} {
			ev := event(kernel.EvFailed)
			ev.Failure = f
			n, effects, err := kernel.Transition(compacting(p), ev)
			want := kernel.ClassifyFailure(f)
			if err != nil || n.Phase != kernel.PhasePersistingResult || *n.Pending != want || len(effects) != 1 {
				t.Errorf("%s/%s: %v %+v want %+v", p, f.Code, err, n.Pending, want)
			}
		}
	}
	if o := kernel.ClassifyFailure(kernel.Failure{Code: kernel.CodePersistenceUncertain}); o.Status != kernel.StatusRestartRequired || !o.Resumable {
		t.Fatalf("%+v", o)
	}
}
