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

// TestAProcessThatDiesWhileTheRetryIsInFlightLeavesTheRetryUnknown: the first Attempt
// ended as a known failure, the retry was dispatched, and the process died with its
// generation in flight. The driver that takes the Thread over settles the Run as blocked
// with the retry's generation unknown (not the first's, which ended), counts it once, and
// the Action stays one Action with two Attempts.
func TestAProcessThatDiesWhileTheRetryIsInFlightLeavesTheRetryUnknown(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(reasoningOnly(), harnesstest.Reply{Kind: harnesstest.KindHang})
	reached := make(chan struct{})
	var once sync.Once
	var calls int
	var mu sync.Mutex
	fake.OnGenerate = func(context.Context, modelport.ChatRequest) {
		mu.Lock()
		calls++
		second := calls == 2
		mu.Unlock()
		if second {
			once.Do(func() { close(reached) })
		}
	}
	r, _ := modelRig(t, fake)
	info := r.openSession("crash.open.00000000000001")
	start := r.startRun(info.ThreadID, "crash.start.0000000000001", "go")
	<-reached
	if got := r.val(`SELECT group_concat(a.ordinal||':'||a.state, ',') FROM (SELECT * FROM attempts ORDER BY ordinal) a`); got != "0:failed,1:dispatch_started" {
		t.Fatal(got)
	}

	// The driver's process ends; another one takes the Thread over.
	svc2, conn2, rec2 := r.restart()
	settled, started := r.takeOver(svc2, conn2, rec2, info.ThreadID, "crash.again.000000000001")
	_ = started
	var terminal *protocol.Event
	for i := range settled {
		if settled[i].Type == "run.terminal" {
			terminal = &settled[i]
		}
	}
	if terminal == nil {
		t.Fatalf("the take-over announced no run.terminal: %d events", len(settled))
	}
	old := resultOfRun(t, svc2, conn2, start.RunID)
	if resultKey(old.Result) != "blocked/MODEL_GENERATION_OUTCOME_UNKNOWN/resumable/unresolved" || old.GenerationAttemptsUsed != 2 || old.GenerationAttemptsUnknown != 1 {
		t.Fatalf("%+v used=%d unknown=%d", old.Result, old.GenerationAttemptsUsed, old.GenerationAttemptsUnknown)
	}
	if got := r.val(`SELECT group_concat(a.ordinal||':'||a.state, ',') FROM (SELECT * FROM attempts WHERE action_id IN (SELECT action_id FROM actions ORDER BY created_at LIMIT 1) ORDER BY ordinal) a`); got != "0:failed,1:unknown" {
		t.Fatal(got)
	}
	if got := r.val(`SELECT COUNT(*) FROM model_calls WHERE generation_state='unknown' AND backend_attempts IS NULL AND attempt_ordinal=1`); got != "1" {
		t.Fatal("the retry's unknown generation is not recorded as unknown")
	}
	mustHandle(t, svc2, conn2, "service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
	svc2.Quiesce()
	r.mustCall("service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
	r.svc.Quiesce()
}

// TestAProcessThatDiesWhileTheRetryWaitsLeavesNothingUnknown: the process dies in the
// backoff. The failed Attempt had a known end, so the Run is settled as stopped with its
// driver (resumable), not as an unknown generation.
func TestAProcessThatDiesWhileTheRetryWaitsLeavesNothingUnknown(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(transient(), harnesstest.Final("never asked for"))
	r, _ := modelRig(t, fake)
	waiting := make(chan struct{}, 1)
	r.waitHook = func(ctx context.Context, _ time.Duration) error {
		waiting <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	info := r.openSession("crash2.open.0000000000001")
	start := r.startRun(info.ThreadID, "crash2.start.000000000001", "go")
	<-waiting
	svc2, conn2, rec2 := r.restart()
	r.takeOver(svc2, conn2, rec2, info.ThreadID, "crash2.again.00000000001")
	old := resultOfRun(t, svc2, conn2, start.RunID)
	if resultKey(old.Result) != "incomplete/DRIVER_STOPPED/resumable/none" || old.GenerationAttemptsUnknown != 0 || old.GenerationAttemptsUsed != 1 {
		t.Fatalf("%+v", old.Result)
	}
	if got := r.val(`SELECT group_concat(state, ',') FROM attempts`); got != "failed" {
		t.Fatal(got)
	}
	mustHandle(t, svc2, conn2, "service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
	svc2.Quiesce()
	r.mustCall("service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
	r.svc.Quiesce()
}
