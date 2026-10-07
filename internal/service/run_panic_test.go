package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// TestARunDriverThatPanicsDoesNotTakeTheServiceDown: the Run is left as it was, the
// Service keeps answering, and the Run is ended by its deadline like any Run that has
// no driver.
func TestARunDriverThatPanicsDoesNotTakeTheServiceDown(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.OnMeasure = func(context.Context) { panic("a bug in a model port") }
	clock := &testClock{now: testNow}
	r := newModelRig(t, fake, clock, nil)
	r.conn = r.svc.NewConn(&recorder{})
	info := r.openSession("panic.open.00000000001")
	start := r.startRun(info.ThreadID, "panic.start.0000000001", "x")

	deadline := time.Now().Add(10 * time.Second)
	for r.val("SELECT phase FROM runs WHERE run_id=?", start.RunID) != "Measuring" {
		if time.Now().After(deadline) {
			t.Fatal("the run never reached the model port")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // the driver has panicked and been recovered
	run := decode[protocol.RunInfo](t, r.mustCall("run/get", protocol.RunGetInput{RunID: start.RunID}))
	if run.Terminal || run.Phase != "Measuring" {
		t.Fatalf("the Run is left as it was: %+v", run)
	}
	_, err := r.call("turn/start", startParamsAt(info.ThreadID, "panic.start.0000000002", "y", 1))
	wantCode(t, err, protocol.CodeBusy)

	fake.OnMeasure = nil
	clock.Advance(31 * time.Minute)
	res := r.mustCall("turn/start", startParamsAt(info.ThreadID, "panic.start.0000000003", "z", 1))
	res.Done()
	old := decode[protocol.RunInfo](t, r.mustCall("run/get", protocol.RunGetInput{RunID: start.RunID}))
	if !old.Terminal || resultKey(old.Result) != "incomplete/DEADLINE_EXCEEDED/resumable/none" {
		t.Fatalf("%+v", old)
	}
	newRun := decode[protocol.StartResult](t, res)
	if r.waitTerminal(newRun.RunID).Result.Status != "completed" {
		t.Fatal("the Service did not go on to run the next request")
	}
}
