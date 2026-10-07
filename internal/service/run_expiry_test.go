package service_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// TestARunWithoutADriverDoesNotKeepTheThreadBusyPastItsDeadline: a process with no model
// port admits Runs and drives none; such a Run holds the Thread until its own deadline
// and no longer, whether the process restarted or not.
func TestARunWithoutADriverDoesNotKeepTheThreadBusyPastItsDeadline(t *testing.T) {
	clock := &testClock{now: testNow}
	r := newModelRig(t, nil, clock, nil)
	info := r.openSession("expire.open.00000000001")
	first := decode[protocol.StartResult](t, r.mustCall("turn/start", startParams(info.ThreadID, "expire.start.0000000001", "first")))

	// Before the deadline the Run is active and a new request is BUSY, as it always was.
	clock.Advance(29 * time.Minute)
	_, err := r.call("turn/start", startParams(info.ThreadID, "expire.start.0000000002", "second"))
	wantCode(t, err, protocol.CodeBusy)

	clock.Advance(2 * time.Minute) // 31 minutes: past the 30 minute deadline
	res := r.mustCall("turn/start", startParams(info.ThreadID, "expire.start.0000000003", "third"))
	if got := eventTypes(res.Events); got != "run.terminal,input.accepted,task.created,run.started" {
		t.Fatalf("%s", got)
	}
	old := decode[protocol.RunInfo](t, r.mustCall("run/get", protocol.RunGetInput{RunID: first.RunID}))
	if !old.Terminal || resultKey(old.Result) != "incomplete/DEADLINE_EXCEEDED/resumable/none" {
		t.Fatalf("%+v", old)
	}
}

// TestARunThatIsBeingDrivenIsNotSettledFromOutside: a Run with a live driver past its
// deadline belongs to that driver, whatever else asks.
func TestARunThatIsBeingDrivenIsNotSettledFromOutside(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Reply{Kind: harnesstest.KindHang})
	inFlight := make(chan struct{})
	var once sync.Once
	fake.OnGenerate = func(context.Context, modelport.ChatRequest) { once.Do(func() { close(inFlight) }) }
	clock := &testClock{now: testNow}
	r := newModelRig(t, fake, clock, nil)
	r.conn = r.svc.NewConn(&recorder{})
	info := r.openSession("driven.open.0000000001")
	start := r.startRun(info.ThreadID, "driven.start.000000001", "x")
	<-inFlight
	clock.Advance(time.Hour) // the store's clock says the deadline has passed
	_, err := r.call("turn/start", startParamsAt(info.ThreadID, "driven.start.000000002", "y", 1))
	wantCode(t, err, protocol.CodeBusy)
	if run := decode[protocol.RunInfo](t, r.mustCall("run/get", protocol.RunGetInput{RunID: start.RunID})); run.Terminal {
		t.Fatalf("a driven Run was settled from outside: %+v", run)
	}
	r.mustCall("service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
	r.svc.Quiesce()
	if res := r.waitTerminal(start.RunID).Result; res.Status != "blocked" {
		t.Fatalf("%+v", res)
	}
}
