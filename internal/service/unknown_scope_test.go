package service_test

import (
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// TestAnUnknownGenerationBlocksItsTaskAndNotAnotherTaskOfTheThread pins the scope the
// build has today, which is a question for the design and not a decision of it: an
// unknown generation is carried by the Task (a resume of that Task generates nothing, and
// the Task's accumulated unknowns are never cleared), and a new turn on the same Thread is
// a new Task that is admitted and generates. ERROR_MAPPING says no act generation is made
// "while an unfinished generation remains" without saying whether that is the Task, the
// Thread or the binding; the contracts that name the unknown (RETRY_CONTRACT sections 2
// and 7, STORAGE) keep it with the Task.
func TestAnUnknownGenerationBlocksItsTaskAndNotAnotherTaskOfTheThread(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(harnesstest.Reply{Kind: harnesstest.KindNoTerminal, Text: "partial"}, harnesstest.Final("the next task answers"))
	r, _ := modelRig(t, fake)
	info := r.openSession("scope.open.00000000000001")
	first := r.startRun(info.ThreadID, "scope.start.0000000000001", "one")
	run := r.waitTerminal(first.RunID)
	if resultKey(run.Result) != "blocked/MODEL_GENERATION_OUTCOME_UNKNOWN/resumable/unresolved" {
		t.Fatalf("%+v", run.Result)
	}

	// The Task itself: resumed, it generates nothing.
	generates := len(fake.Generates())
	rr := decode[protocol.ResumeResult](t, r.mustCall("run/resume", resumeInput(first.TaskID, first.RunID, "scope.resume.000000000001")))
	if next := r.waitTerminal(rr.RunID); resultKey(next.Result) != "blocked/MODEL_GENERATION_OUTCOME_UNKNOWN/resumable/unresolved" || len(fake.Generates()) != generates {
		t.Fatalf("%+v", next.Result)
	}

	// Another Task of the same Thread: admitted, and it generates.
	second := r.startRun(info.ThreadID, "scope.start.0000000000002", "two", func(in *protocol.StartInput) { in.ExpectedContextRevision = r.contextRevision(info.ThreadID) })
	if second.TaskID == first.TaskID {
		t.Fatal("a new turn is a new Task")
	}
	if got := r.waitTerminal(second.RunID); got.Result.Status != "completed" || got.Result.FinalText != "the next task answers" || len(fake.Generates()) != generates+1 {
		t.Fatalf("%+v", got.Result)
	}
}

func (r *rig) contextRevision(thread string) int64 {
	r.t.Helper()
	return decode[protocol.SessionInfo](r.t, r.mustCall("session/get", protocol.SessionGetInput{ThreadID: thread})).ContextRevision
}
