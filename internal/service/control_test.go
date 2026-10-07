package service_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/service"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func (r *rig) interrupt(runID string, expected int64, key string) protocol.InterruptReceipt {
	r.t.Helper()
	return decode[protocol.InterruptReceipt](r.t, r.mustCall("turn/interrupt", protocol.InterruptInput{RunID: runID, ExpectedControlRevision: expected, IdempotencyKey: key}))
}

func (r *rig) appendInput(thread, runID, text, disposition string, expected int64, key string) protocol.InputReceipt {
	r.t.Helper()
	return decode[protocol.InputReceipt](r.t, r.mustCall("input/append", protocol.InputAppendInput{
		ThreadID: thread, RunID: runID, Input: protocol.InputMessage{Text: text}, Disposition: disposition, ExpectedControlRevision: expected, IdempotencyKey: key}))
}

func resumeInput(taskID, lastRun, key string) protocol.ResumeInput {
	return protocol.ResumeInput{TaskID: taskID, ExpectedLastRunID: lastRun, ExpectedControlRevision: 0, IdempotencyKey: key, Limits: startLimits}
}

// withControlPoll replaces the rig's Service with one whose drivers look for a stop another
// process recorded every d (the Service it replaces has driven nothing yet).
func (r *rig) withControlPoll(d time.Duration) {
	r.t.Helper()
	_ = r.writers.Close()
	r.controlPoll = d
	r.store, r.svc = r.process()
	r.conn = r.svc.NewConn(nil)
}

// otherProcess is a second Service over the same data root with no model port: another
// process that can record but not drive. It does not disturb the rig's own.
func (r *rig) otherProcess() (*service.Service, *service.Conn) {
	r.t.Helper()
	model, writers := r.model, r.writers
	r.model = nil
	_, svc := r.process()
	r.model, r.writers = model, writers
	conn := svc.NewConn(nil)
	mustHandle(r.t, svc, conn, "initialize", protocol.InitializeInput{ClientName: "t", ClientVersion: "0", ProtocolVersion: protocol.ProtocolVersion})
	return svc, conn
}

func (r *rig) startedGrandchild(file string) int {
	r.t.Helper()
	r.waitFor("the grandchild to start", func() bool { _, ok := r.read(file); return ok })
	text, _ := r.read(file)
	pid, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		r.t.Fatalf("%q", text)
	}
	return pid
}

// TestAnInterruptStopsARunningProcessAndEveryProcessItStarted is A12 after the dispatch
// start, through the whole path: the call is running a process that has a grandchild;
// turn/interrupt records the stop; the process tree is stopped (not at the call's own
// timeout); the call is recorded as cancelled, with its effect shown to be over; and the
// Run ends cancelled without a second generation. The same request again, and another
// request after the Run ended, record nothing.
func TestAnInterruptStopsARunningProcessAndEveryProcessItStarted(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Skip("PID probes are unix-only")
	}
	fake := harnesstest.NewFake()
	r, _ := toolRig(t, fake, toolConfig{process: true})
	file := "grandchild.pid"
	fake.SetScript(calls(tc("c1", "process.exec", execArgs([]string{"tree", filepath.Join(r.layout.Work, file)}, "helper", 60))), harnesstest.Final("never"))
	info := r.openSession("intr.open.0000000000000001")
	start := r.startRun(info.ThreadID, "intr.start.000000000000001", "go")
	pid := r.startedGrandchild(file)
	t.Cleanup(func() { killProcess(pid) })
	if !processAlive(pid) {
		t.Fatal("the grandchild is not running")
	}
	if r.val("SELECT a.state FROM attempts a JOIN actions ac ON ac.action_id=a.action_id WHERE ac.kind='tool'") != "running" {
		t.Fatal("the call is not the one running")
	}

	begin := time.Now()
	receipt := r.interrupt(start.RunID, 0, "intr.key.00000000000000001")
	if receipt.Code != "CANCEL_REQUESTED" || !receipt.SignalRecorded || receipt.ControlRevision != 1 || receipt.RunID != start.RunID {
		t.Fatalf("%+v", receipt)
	}
	run := r.waitTerminal(start.RunID)
	if time.Since(begin) > 15*time.Second {
		t.Fatalf("the stop took %v: it waited for the call's own timeout", time.Since(begin))
	}
	res := run.Result
	if res.Status != "cancelled" || res.Code != "CANCELLED" || !res.Resumable || len(res.UnresolvedActionIDs) != 0 || res.FinalText != "" || len(fake.Generates()) != 1 {
		t.Fatalf("%+v", res)
	}
	r.waitFor("the grandchild to be gone", func() bool { return !processAlive(pid) })

	// The call started, ran, and was stopped: its end is cancelled with its output sealed
	// as it was; it is not unknown (the tree was shown to be gone) and not "not started".
	if got := r.val("SELECT ac.status||'/'||a.state FROM attempts a JOIN actions ac ON ac.action_id=a.action_id WHERE ac.kind='tool'"); got != "cancelled/cancelled" {
		t.Fatal(got)
	}
	events := r.threadEvents(info.ThreadID)
	var done protocol.ActionCompletedPayload
	for _, e := range ofType(events, "action.completed") {
		if p := payloadOf[protocol.ActionCompletedPayload](t, e); r.val("SELECT kind FROM actions WHERE action_id=?", p.ActionID) == "tool" {
			done = p
		}
	}
	if done.EffectState != "cancelled" || done.ExitCode != nil {
		t.Fatalf("%+v", done)
	}
	// One stop, recorded before the Run's end, naming this Run and its caller.
	cancels := ofType(events, "control.cancel_requested")
	if len(cancels) != 1 || cancels[0].EventSeq >= ofType(events, "run.terminal")[0].EventSeq {
		t.Fatalf("%s", eventTypes(events))
	}
	if p := payloadOf[protocol.ControlCancelRequestedPayload](t, cancels[0]); p.RunID != start.RunID || p.ControlRevision != 1 || p.Reason != "USER_INTERRUPT" || p.Principal != r.dep.Caller.Principal {
		t.Fatalf("%+v", p)
	}
	if term := payloadOf[protocol.RunTerminalPayload](t, ofType(events, "run.terminal")[0]); term.Status != "cancelled" || term.Code != "CANCELLED" {
		t.Fatalf("%+v", term)
	}

	// The same request again: the same receipt, nothing recorded. Another request, now
	// that the Run has ended: ALREADY_TERMINAL, nothing recorded.
	n := len(events)
	again := r.mustCall("turn/interrupt", protocol.InterruptInput{RunID: start.RunID, ExpectedControlRevision: 0, IdempotencyKey: "intr.key.00000000000000001"})
	if got := decode[protocol.InterruptReceipt](t, again); got != receipt || len(again.Events) != 0 {
		t.Fatalf("%+v %d events", got, len(again.Events))
	}
	late := r.mustCall("turn/interrupt", protocol.InterruptInput{RunID: start.RunID, ExpectedControlRevision: 1, IdempotencyKey: "intr.key.00000000000000002"})
	if got := decode[protocol.InterruptReceipt](t, late); got.Code != "ALREADY_TERMINAL" || got.SignalRecorded || got.ControlRevision != 1 || len(late.Events) != 0 {
		t.Fatalf("%+v", got)
	}
	if len(r.threadEvents(info.ThreadID)) != n {
		t.Fatal("a repeated or late stop recorded something")
	}
	if rep, err := r.store.VerifyClosure(t.Context()); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}

// TestAnInterruptStopsAGenerationInFlightAndItsOutcomeStaysUnknown: the model is asked
// to generate and the Run is stopped. The Run is cancelled, as the caller asked; what the
// generation did is not known (the stream was cut, nothing shows the backend stopped), so
// the attempt is unknown, counted so, and listed in the result: a stop does not turn an
// unknown generation into a known one.
func TestAnInterruptStopsAGenerationInFlightAndItsOutcomeStaysUnknown(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Reply{Kind: harnesstest.KindHang})
	reached := make(chan struct{})
	var once sync.Once
	fake.OnGenerate = func(context.Context, modelport.ChatRequest) { once.Do(func() { close(reached) }) }
	r, _ := modelRig(t, fake)
	info := r.openSession("intr.open.0000000000000002")
	start := r.startRun(info.ThreadID, "intr.start.000000000000002", "go")
	<-reached
	if receipt := r.interrupt(start.RunID, 0, "intr.key.00000000000000003"); receipt.Code != "CANCEL_REQUESTED" {
		t.Fatalf("%+v", receipt)
	}
	run := r.waitTerminal(start.RunID)
	res := run.Result
	if res.Status != "cancelled" || res.Code != "CANCELLED" || !res.Resumable || len(res.UnresolvedActionIDs) != 1 || run.GenerationAttemptsUsed != 1 || run.GenerationAttemptsUnknown != 1 {
		t.Fatalf("%+v %+v", res, run)
	}
	if got := r.val("SELECT state FROM attempts WHERE action_id=?", res.UnresolvedActionIDs[0]); got != "unknown" {
		t.Fatalf("an unknown generation is recorded as %s", got)
	}
	done := payloadOf[protocol.ModelCompletedPayload](t, ofType(r.threadEvents(info.ThreadID), "model.completed")[0])
	if done.Outcome != "error" || done.GenerationState != "unknown" || done.FailureCode == nil || *done.FailureCode != "CANCELLED" {
		t.Fatalf("%+v", done)
	}
	if len(fake.Generates()) != 1 {
		t.Fatalf("%d generations", len(fake.Generates()))
	}
}

// TestAFinalAnswerThatArrivesAfterAStopIsNotAdopted is STORAGE section 6 for a result: a
// stop that was recorded (by another process, so the driver was not woken and the answer
// was read) before the Run could complete means the answer is not adopted. The Run ends
// cancelled; the generation that produced the answer stays recorded as it was.
func TestAFinalAnswerThatArrivesAfterAStopIsNotAdopted(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Final("the answer"))
	r, _ := modelRig(t, fake)
	r.withControlPoll(time.Hour) // the answer must not be cut off by the driver's own look at the revision
	other, otherConn := r.otherProcess()
	info := r.openSession("intr.open.0000000000000003")
	var runID string
	var once sync.Once
	fake.OnGenerate = func(context.Context, modelport.ChatRequest) {
		once.Do(func() {
			// Recorded by the other process while the generation is in flight.
			res, err := other.Handle(context.Background(), otherConn, request(t, "turn/interrupt", protocol.InterruptInput{RunID: runID, ExpectedControlRevision: 0, IdempotencyKey: "intr.key.00000000000000004"}))
			if err != nil {
				t.Errorf("%v", err)
				return
			}
			if got := decode[protocol.InterruptReceipt](t, res); got.Code != "CANCEL_REQUESTED" {
				t.Errorf("%+v", got)
			}
		})
	}
	start := r.startRunWith(info.ThreadID, "intr.start.000000000000003", "go", func(id string) { runID = id })
	run := r.waitTerminal(start.RunID)
	res := run.Result
	if res.Status != "cancelled" || res.Code != "CANCELLED" || res.FinalText != "" || res.FinalMessageID != nil || len(res.UnresolvedActionIDs) != 0 || !res.Resumable {
		t.Fatalf("%+v", res)
	}
	// The answer was produced, and that is on record; it is not part of the Thread's history.
	if got := r.val("SELECT a.state FROM attempts a JOIN actions ac ON ac.action_id=a.action_id WHERE ac.kind='model'"); got != "completed" {
		t.Fatalf("the generation's own end was rewritten: %s", got)
	}
	if r.count("context_entries") != 1 {
		t.Fatalf("%d entries: the answer of a stopped Run was applied", r.count("context_entries"))
	}
}

// startRunWith is startRun for a test that needs the Run's ID before the Run is driven
// (its model hook acts on the Run): the ID is handed to ready once turn/start answered.
func (r *rig) startRunWith(thread, key, text string, ready func(runID string)) protocol.StartResult {
	r.t.Helper()
	res := r.mustCall("turn/start", startParams(thread, key, text))
	out := decode[protocol.StartResult](r.t, res)
	ready(out.RunID)
	res.Done()
	return out
}

// TestAStopRecordedByAnotherProcessStopsTheDriversWork: the process that drives the Run
// is not the one that recorded the stop. The driver finds the new control revision on its
// own, tells the call to stop, and the Run ends cancelled; the other process, which does
// not hold the Thread, only recorded.
func TestAStopRecordedByAnotherProcessStopsTheDriversWork(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Skip("PID probes are unix-only")
	}
	fake := harnesstest.NewFake()
	fake.SetScript(calls(tc("c1", "process.exec", execArgs([]string{"sleep"}, "helper", 60))), harnesstest.Final("never"))
	r, _ := toolRig(t, fake, toolConfig{process: true})
	r.withControlPoll(25 * time.Millisecond)
	other, otherConn := r.otherProcess()
	info := r.openSession("intr.open.0000000000000004")
	start := r.startRun(info.ThreadID, "intr.start.000000000000004", "go")
	r.waitFor("the process to run", func() bool {
		return r.val("SELECT COUNT(*) FROM attempts a JOIN actions ac ON ac.action_id=a.action_id WHERE ac.kind='tool' AND a.state='running'") == "1"
	})
	begin := time.Now()
	res, err := other.Handle(context.Background(), otherConn, request(t, "turn/interrupt", protocol.InterruptInput{RunID: start.RunID, ExpectedControlRevision: 0, IdempotencyKey: "intr.key.00000000000000005"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := decode[protocol.InterruptReceipt](t, res); got.Code != "CANCEL_REQUESTED" || len(res.Events) != 1 || res.Events[0].Type != "control.cancel_requested" {
		t.Fatalf("the other process records the signal and nothing else: %+v %s", got, eventTypes(res.Events))
	}
	run := r.waitTerminal(start.RunID)
	if time.Since(begin) > 15*time.Second || run.Result.Status != "cancelled" || run.Result.Code != "CANCELLED" || len(fake.Generates()) != 1 {
		t.Fatalf("%v %+v", time.Since(begin), run.Result)
	}
}

// TestAStopForARunNobodyDrivesEndsItAtOnce: a Run that this process admitted and never
// drove (no model port) is ended by the stop, as the cancelled Run it is, and the Thread
// is free; the events say so in order.
func TestAStopForARunNobodyDrivesEndsItAtOnce(t *testing.T) {
	r := newRig(t, nil)
	info := r.openSession("intr.open.0000000000000005")
	start := decode[protocol.StartResult](t, r.mustCall("turn/start", startParams(info.ThreadID, "intr.start.000000000000005", "work")))
	res := r.mustCall("turn/interrupt", protocol.InterruptInput{RunID: start.RunID, ExpectedControlRevision: 0, IdempotencyKey: "intr.key.00000000000000006"})
	if got := decode[protocol.InterruptReceipt](t, res); got.Code != "CANCEL_REQUESTED" || got.ControlRevision != 1 {
		t.Fatalf("%+v", got)
	}
	if eventTypes(res.Events) != "control.cancel_requested,run.terminal" {
		t.Fatalf("%s", eventTypes(res.Events))
	}
	run := decode[protocol.RunInfo](t, r.mustCall("run/get", protocol.RunGetInput{RunID: start.RunID}))
	if !run.Terminal || run.Result.Status != "cancelled" || run.Result.Code != "CANCELLED" || !run.Result.Resumable || len(run.Result.UnresolvedActionIDs) != 0 {
		t.Fatalf("%+v", run)
	}
	if sess := decode[protocol.SessionInfo](t, r.mustCall("session/get", protocol.SessionGetInput{ThreadID: info.ThreadID})); sess.ActiveRunID != nil || sess.ControlRevision != 1 {
		t.Fatalf("%+v", sess)
	}
	// The same request again changes nothing.
	again := r.mustCall("turn/interrupt", protocol.InterruptInput{RunID: start.RunID, ExpectedControlRevision: 0, IdempotencyKey: "intr.key.00000000000000006"})
	if len(again.Events) != 0 || decode[protocol.InterruptReceipt](t, again).Code != "CANCEL_REQUESTED" {
		t.Fatal("a repeated stop recorded something")
	}
	if r.count("events") != 6 {
		t.Fatalf("%d events", r.count("events"))
	}
}

// TestAStopForARunAProcessLeftEndsLikeAnyOtherStopOfARunNobodyDrives: the Run's driver is
// gone (its process ended) and another process is asked to stop the Run. Taking the Thread
// settles the Run, now knowing the stop was asked: nothing dispatched ends cancelled; a
// generation that may have run keeps the Run blocked with its outcome unknown.
func TestAStopForARunAProcessLeftEndsLikeAnyOtherStopOfARunNobodyDrives(t *testing.T) {
	for name, tc := range map[string]struct {
		kind     harnesstest.Kind
		inFlight bool
		want     string
	}{
		"nothing dispatched":     {harnesstest.KindFinal, false, "cancelled/CANCELLED/resumable/none"},
		"a generation in flight": {harnesstest.KindHang, true, "blocked/MODEL_GENERATION_OUTCOME_UNKNOWN/resumable/unresolved"},
	} {
		t.Run(name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			fake.SetReply(harnesstest.Reply{Kind: tc.kind})
			reached := make(chan struct{})
			var once sync.Once
			if tc.inFlight {
				fake.OnGenerate = func(context.Context, modelport.ChatRequest) { once.Do(func() { close(reached) }) }
			} else {
				fake.OnMeasure = func(ctx context.Context) { once.Do(func() { close(reached) }); <-ctx.Done() }
			}
			r, _ := modelRig(t, fake)
			info := r.openSession("intr.open.0000000000000006")
			start := r.startRun(info.ThreadID, "intr.start.000000000000006", "x")
			<-reached
			_ = r.writers.Close() // the driver's process ends: the OS drops its locks
			r.model = nil
			_, svc2 := r.process()
			conn2 := svc2.NewConn(nil)
			mustHandle(t, svc2, conn2, "initialize", protocol.InitializeInput{ClientName: "t", ClientVersion: "0", ProtocolVersion: protocol.ProtocolVersion})
			res := mustHandle(t, svc2, conn2, "turn/interrupt", protocol.InterruptInput{RunID: start.RunID, ExpectedControlRevision: 0, IdempotencyKey: "intr.key.00000000000000007"})
			if got := decode[protocol.InterruptReceipt](t, res); got.Code != "CANCEL_REQUESTED" {
				t.Fatalf("%+v", got)
			}
			got := resultKey(decode[protocol.RunInfo](t, mustHandle(t, svc2, conn2, "run/get", protocol.RunGetInput{RunID: start.RunID})).Result)
			r.mustCall("service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
			r.svc.Quiesce()
			if got != tc.want {
				t.Fatalf("%s, want %s", got, tc.want)
			}
			if got := eventTypes(res.Events); !strings.HasPrefix(got, "control.cancel_requested,") || !strings.HasSuffix(got, ",run.terminal") {
				t.Fatalf("the stop is recorded, then the Run it ended: %s", got)
			}
		})
	}
}

// TestAnInputAppendedForTheNextStepIsInThatPromptOnce is F23 through a Run: the input
// arrives while the model is generating the first step; it is stored at once and applied
// to the context before the second step's prompt, after what the first step did, exactly
// once; the first prompt does not have it.
func TestAnInputAppendedForTheNextStepIsInThatPromptOnce(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(calls(tc("c1", "file.read", readArgs("demo.txt", 0, 10, 10))), harnesstest.Final("done"))
	r, _ := toolRig(t, fake, toolConfig{})
	r.write("demo.txt", "x")
	info := r.openSession("app.open.00000000000000001")
	var runID string
	var receipt protocol.InputReceipt
	var once sync.Once
	fake.OnGenerate = func(context.Context, modelport.ChatRequest) {
		once.Do(func() {
			receipt = decode[protocol.InputReceipt](t, mustHandle(t, r.svc, r.conn, "input/append", protocol.InputAppendInput{
				ThreadID: info.ThreadID, RunID: runID, Input: protocol.InputMessage{Text: "also check Y"}, Disposition: "next_step", ExpectedControlRevision: 0,
				IdempotencyKey: "app.key.0000000000000000001"}))
		})
	}
	start := r.startRunWith(info.ThreadID, "app.start.000000000000001", "check X", func(id string) { runID = id })
	run := r.waitTerminal(start.RunID)
	if run.Result.Status != "completed" || len(fake.Generates()) != 2 {
		t.Fatalf("%+v", run.Result)
	}
	if receipt.DeliveryState != "queued" || receipt.Disposition != "next_step" || receipt.QueueRevision != 1 || receipt.ControlRevision != 0 {
		t.Fatalf("%+v", receipt)
	}
	// Not in the first prompt; last in the second, after the exchange of the first step.
	first, second := fake.Generates()[0].Messages, fake.Generates()[1].Messages
	for _, m := range first {
		if strings.Contains(m.Text(), "also check Y") {
			t.Fatal("the first prompt carries an input that arrived after it was built")
		}
	}
	var roles []string
	for _, m := range second[1:] {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "user,assistant,tool,user" || second[len(second)-1].Text() != envelope("also check Y") {
		t.Fatalf("%v %q", roles, second[len(second)-1].Text())
	}
	// The events: accepted (during the generation), then applied before the next step.
	events := r.threadEvents(info.ThreadID)
	accepted, applied := ofType(events, "input.accepted"), ofType(events, "input.applied")
	if len(accepted) != 2 || len(applied) != 2 {
		t.Fatalf("%s", eventTypes(events))
	}
	p := payloadOf[protocol.InputAppliedPayload](t, applied[1])
	if p.Disposition != "next_step" || p.MessageID != receipt.MessageID || p.QueueItemID == nil || *p.QueueItemID != receipt.QueueItemID || p.ContextRevision != 4 ||
		applied[1].ReceiptID == nil || *applied[1].ReceiptID != receipt.ReceiptID {
		t.Fatalf("%+v", p)
	}
	secondStep := ofType(events, "model.requested")[1]
	if applied[1].EventSeq >= secondStep.EventSeq || accepted[1].EventSeq >= applied[1].EventSeq {
		t.Fatalf("%s", eventTypes(events))
	}
	if r.val("SELECT delivery_state||'/'||applied_context_revision FROM queue_inputs") != "applied/4" {
		t.Fatal("the queue row is not marked applied")
	}
	// The input, the first step's call and answer, the appended input and the final answer.
	if run.ContextRevision != 5 {
		t.Fatalf("context revision %d", run.ContextRevision)
	}
	if rep, err := r.store.VerifyClosure(t.Context()); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}

// TestInputsLeftWaitingAreAppliedByTheNextRunOnceAndInOrder: while a Run generates, a
// next_turn input and an interrupt_current input arrive. The second stops the Run
// (cancelled) and neither is applied to it. The Thread's next Run applies both, in the
// order they arrived and before its own input; a Run after that does not apply them again.
func TestInputsLeftWaitingAreAppliedByTheNextRunOnceAndInOrder(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Reply{Kind: harnesstest.KindHang})
	reached := make(chan struct{})
	var once sync.Once
	fake.OnGenerate = func(context.Context, modelport.ChatRequest) { once.Do(func() { close(reached) }) }
	r, _ := modelRig(t, fake)
	info := r.openSession("app.open.00000000000000002")
	first := r.startRun(info.ThreadID, "app.start.000000000000002", "first")
	<-reached
	turn := r.appendInput(info.ThreadID, first.RunID, "A for later", "next_turn", 0, "app.key.0000000000000000002")
	stop := r.appendInput(info.ThreadID, first.RunID, "B instead", "interrupt_current", 0, "app.key.0000000000000000003")
	if turn.DeliveryState != "queued" || turn.ControlRevision != 0 || stop.DeliveryState != "deferred" || stop.ControlRevision != 1 || stop.QueueRevision != 2 {
		t.Fatalf("%+v %+v", turn, stop)
	}
	run := r.waitTerminal(first.RunID)
	if run.Result.Status != "cancelled" || run.Result.Code != "CANCELLED" || len(run.Result.UnresolvedActionIDs) != 1 {
		t.Fatalf("%+v", run.Result)
	}
	// Neither input reached the Run they were sent to.
	if r.val("SELECT COUNT(*) FROM queue_inputs WHERE delivery_state='applied'") != "0" || r.count("context_entries") != 1 {
		t.Fatal("an input was applied to the Run that was stopped")
	}
	// An input for the Run that ended is refused, not lost into the queue.
	_, err := r.call("input/append", protocol.InputAppendInput{ThreadID: info.ThreadID, RunID: first.RunID, Input: protocol.InputMessage{Text: "late"}, Disposition: "next_step",
		ExpectedControlRevision: 1, IdempotencyKey: "app.key.0000000000000000004"})
	wantCode(t, err, protocol.CodeInvalidRequest)

	fake.SetReply(harnesstest.Final("ok"))
	cur := decode[protocol.SessionInfo](t, r.mustCall("session/get", protocol.SessionGetInput{ThreadID: info.ThreadID}))
	second := r.startRun(info.ThreadID, "app.start.000000000000003", "second", func(in *protocol.StartInput) {
		in.ExpectedContextRevision, in.ExpectedControlRevision = cur.ContextRevision, cur.ControlRevision
	})
	if r.waitTerminal(second.RunID).Result.Status != "completed" {
		t.Fatal("the next Run did not complete")
	}
	g := fake.Generates()[len(fake.Generates())-1]
	var texts []string
	for _, m := range g.Messages[1:] {
		texts = append(texts, m.Text())
	}
	if strings.Join(texts, "|") != strings.Join([]string{envelope("first"), envelope("A for later"), envelope("B instead"), envelope("second")}, "|") {
		t.Fatalf("%q", texts)
	}
	var order []string
	for _, e := range ofType(r.threadEvents(info.ThreadID), "input.applied") {
		if e.RunID != nil && *e.RunID == second.RunID {
			order = append(order, payloadOf[protocol.InputAppliedPayload](t, e).Disposition)
		}
	}
	if strings.Join(order, ",") != "next_turn,interrupt_current,initial" {
		t.Fatalf("%v", order)
	}
	// A third Run does not apply them again.
	cur = decode[protocol.SessionInfo](t, r.mustCall("session/get", protocol.SessionGetInput{ThreadID: info.ThreadID}))
	third := r.startRun(info.ThreadID, "app.start.000000000000004", "third", func(in *protocol.StartInput) {
		in.ExpectedContextRevision, in.ExpectedControlRevision = cur.ContextRevision, cur.ControlRevision
	})
	if r.waitTerminal(third.RunID).Result.Status != "completed" {
		t.Fatal("the third Run did not complete")
	}
	g = fake.Generates()[len(fake.Generates())-1]
	count := 0
	for _, m := range g.Messages {
		if m.Text() == envelope("A for later") || m.Text() == envelope("B instead") {
			count++
		}
	}
	if count != 2 || r.val("SELECT COUNT(*) FROM context_entries ce JOIN items i ON i.message_id=ce.message_id WHERE i.message_id IN (?,?)", turn.MessageID, stop.MessageID) != "2" {
		t.Fatalf("%d", count)
	}
	if rep, err := r.store.VerifyClosure(t.Context()); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}

// TestResumeContinuesATaskWhoseCallWasKilledAndNeverRunsTheCallAgain is run/resume
// through the whole path, with the kill of A10: the process died between the call's effect
// and its record. The resume takes the Thread (which settles the old Run: blocked, the
// call's outcome unknown), asks the host about the call's process, and starts a new Run of
// the same Task with a new Trace. The new Run is told, in its prompt, that the call's
// outcome is unknown; the command is not run again, by anyone; the old Run, the call's
// one end and the unknown outcome are exactly what they were.
func TestResumeContinuesATaskWhoseCallWasKilledAndNeverRunsTheCallAgain(t *testing.T) {
	hit := make(chan string, 1)
	fake := harnesstest.NewFake()
	r, _ := hookRig(t, fake, toolConfig{process: true}, killAt(tools.PointAfterExecute, hit))
	counter := filepath.Join(r.layout.Work, "count.log")
	fake.SetScript(calls(tc("c1", "process.exec", execArgs([]string{"append", counter}, "helper", 20))), harnesstest.Final("carried on"))
	info := r.openSession("res.open.00000000000000001")
	start := r.startRun(info.ThreadID, "res.start.000000000000001", "go")
	<-hit
	r.waitFor("the driver to stop", func() bool { return len(fake.Generates()) == 1 })
	time.Sleep(50 * time.Millisecond)
	if lines(counter) != 1 {
		t.Fatalf("the program ran %d times", lines(counter))
	}

	svc2, conn2, _ := r.restart()
	in := resumeInput(start.TaskID, start.RunID, "res.key.00000000000000001")
	res := mustHandle(t, svc2, conn2, "run/resume", in)
	res.Done()
	rr := decode[protocol.ResumeResult](t, res)
	if rr.TaskID != start.TaskID || rr.ThreadID != info.ThreadID || rr.PreviousRunID != start.RunID || rr.RunID == start.RunID || rr.TraceID == start.TraceID || rr.CheckpointID != nil ||
		rr.EffectiveLimits != startLimits || rr.RecoveryPolicyRevision != start.RecoveryPolicyRevision {
		t.Fatalf("%+v", rr)
	}
	// The resume's own events, as the client was told them: the dead Run's call and end,
	// then the new Run.
	if got := eventTypes(res.Events); got != "action.completed,run.terminal,run.started" {
		t.Fatalf("%s", got)
	}
	unknown := payloadOf[protocol.ActionCompletedPayload](t, res.Events[0])
	started := res.Events[2]
	if unknown.EffectState != "unknown" || started.EvidenceID == nil || started.ReceiptID == nil || *started.ReceiptID != rr.ReceiptID {
		t.Fatalf("%+v %+v", unknown, started)
	}

	next := r.waitTerminalOn(svc2, conn2, rr.RunID)
	if next.Result.Status != "completed" || next.Result.FinalText != "carried on" {
		t.Fatalf("%+v", next.Result)
	}
	old := resultOfRun(t, svc2, conn2, start.RunID)
	if old.Result.Status != "blocked" || old.Result.Code != "EFFECT_OUTCOME_UNKNOWN" || len(old.Result.UnresolvedActionIDs) != 1 || old.Result.UnresolvedActionIDs[0] != unknown.ActionID || !old.Result.Resumable {
		t.Fatalf("the old Run changed: %+v", old.Result)
	}
	// The command ran once, ever; the call is unknown with its one end.
	if lines(counter) != 1 {
		t.Fatalf("the program ran %d times", lines(counter))
	}
	if r.val("SELECT state FROM attempts WHERE action_id=?", unknown.ActionID) != "unknown" ||
		r.val("SELECT COUNT(*) FROM events WHERE type='action.completed' AND json_extract(payload_json,'$.action_id')=?", unknown.ActionID) != "1" {
		t.Fatal("the unknown call was changed or ended twice")
	}
	// The new Run's prompt carries the unknown outcome of the call.
	var sawUnknown bool
	for _, v := range toolMessages(t, fake.Generates()[len(fake.Generates())-1]) {
		sawUnknown = sawUnknown || (v.Error != nil && v.Error.Code == "EFFECT_OUTCOME_UNKNOWN")
	}
	if !sawUnknown {
		t.Fatal("the resumed Run's prompt does not carry the unknown outcome")
	}
	// What the resume found is Evidence of the new Run, named by its run.started.
	var rec struct {
		Tools []struct {
			AttemptID string `json:"attempt_id"`
			Effect    string `json:"effect"`
			Verdict   string `json:"verdict"`
		} `json:"tool_attempts"`
	}
	if err := json.Unmarshal([]byte(r.evidence(*started.EvidenceID)), &rec); err != nil || len(rec.Tools) != 1 || rec.Tools[0].Effect != "unknown" || rec.Tools[0].Verdict != "gone" ||
		r.val("SELECT attempt_id FROM attempts WHERE action_id=?", unknown.ActionID) != rec.Tools[0].AttemptID {
		t.Fatalf("%v %+v", err, rec)
	}
	// The Task is the same, the Run is new: one Task, two Runs, and the Thread is free.
	if r.count("tasks") != 1 || r.count("runs") != 2 || r.val("SELECT status FROM tasks") != "run_ended" ||
		r.val("SELECT COALESCE(active_run_id,'none') FROM threads") != "none" {
		t.Fatal("the Task and its Runs are not recorded as one Task with two Runs")
	}

	// The same request again is the same answer, and starts nothing.
	again := mustHandle(t, svc2, conn2, "run/resume", in)
	again.Done()
	if got := decode[protocol.ResumeResult](t, again); got != rr || len(again.Events) != 0 || r.count("runs") != 2 {
		t.Fatalf("%+v", got)
	}
	// A Task whose last Run completed is not run again, automatically or otherwise.
	_, err := svc2.Handle(context.Background(), conn2, request(t, "run/resume", resumeInput(start.TaskID, rr.RunID, "res.key.00000000000000002")))
	wantCode(t, err, protocol.CodeInvalidRequest)
	if r.count("runs") != 2 {
		t.Fatal("a completed Task was resumed")
	}
	if rep, err := r.store.VerifyClosure(t.Context()); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}

// stubReconciler is the host's check of the processes of unknown calls: it says what the
// test tells it to.
type stubReconciler struct {
	mu      sync.Mutex
	verdict string
	calls   int
}

func (s *stubReconciler) Reconcile(_ context.Context, attempts []sqlite.ToolAttemptRef) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	out := map[string]string{}
	for _, a := range attempts {
		out[a.AttemptID] = s.verdict
	}
	return out
}

func (s *stubReconciler) set(v string) { s.mu.Lock(); s.verdict = v; s.mu.Unlock() }

func (s *stubReconciler) asked() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

// TestAResumeIsRefusedWhileAnUnknownCallsProcessIsStillRunning: the host finds the
// process of a call whose outcome is unknown still running and cannot stop it. The
// resume is refused (BUSY, retryable) and writes nothing; once the host can stop it, the
// same request is accepted.
func TestAResumeIsRefusedWhileAnUnknownCallsProcessIsStillRunning(t *testing.T) {
	hit := make(chan string, 1)
	fake := harnesstest.NewFake()
	stub := &stubReconciler{verdict: "stop_failed"}
	r, _ := hookRig(t, fake, toolConfig{process: true}, killAt(tools.PointAfterExecute, hit))
	counter := filepath.Join(r.layout.Work, "count.log")
	fake.SetScript(calls(tc("c1", "process.exec", execArgs([]string{"append", counter}, "helper", 20))), harnesstest.Final("carried on"))
	info := r.openSession("res.open.00000000000000002")
	start := r.startRun(info.ThreadID, "res.start.000000000000002", "go")
	<-hit
	r.waitFor("the driver to stop", func() bool { return len(fake.Generates()) == 1 })
	time.Sleep(50 * time.Millisecond)

	r.reconciler = stub
	svc2, conn2, _ := r.restart()
	// The take-over inside the resume settles the dead Run (the call is unknown whatever
	// the host finds); the resume's own look at the call's process then finds one that will
	// not stop.
	// What the host can refuse is refused before the host is asked: a limit above the host's
	// is INVALID_LIMITS and the processes of the unknown calls are not looked at (the one look
	// there was is the take-over's).
	tooLong := resumeInput(start.TaskID, start.RunID, "res.key.00000000000000099")
	tooLong.Limits.DeadlineSeconds = 3600
	_, err := svc2.Handle(context.Background(), conn2, request(t, "run/resume", tooLong))
	wantCode(t, err, protocol.CodeInvalidLimits)
	if stub.asked() != 1 {
		t.Fatalf("the host was asked %d times; a resume that is refused for its limits must not ask", stub.asked())
	}
	_, err = svc2.Handle(context.Background(), conn2, request(t, "run/resume", resumeInput(start.TaskID, start.RunID, "res.key.00000000000000003")))
	wantCode(t, err, protocol.CodeBusy)
	var pe *protocol.Error
	if !asError(err, &pe) || !pe.Retryable {
		t.Fatalf("%v", err)
	}
	if r.count("runs") != 1 || r.count("receipts") != 2 || r.val("SELECT COALESCE(active_run_id,'none') FROM threads") != "none" || lines(counter) != 1 {
		t.Fatalf("a refused resume wrote something: runs=%d receipts=%d", r.count("runs"), r.count("receipts"))
	}
	stub.set("stopped")
	res := mustHandle(t, svc2, conn2, "run/resume", resumeInput(start.TaskID, start.RunID, "res.key.00000000000000003"))
	res.Done()
	rr := decode[protocol.ResumeResult](t, res)
	if r.waitTerminalOn(svc2, conn2, rr.RunID).Result.Status != "completed" || lines(counter) != 1 {
		t.Fatal("the resume after the process was stopped did not complete, or ran the call again")
	}
}

// TestAResumeWithAnExplicitBindingUsesItFromTheFirstStep: a binding the host configured
// replaces the Thread's for the new Run (the model is described and counted anew); one it
// did not configure is refused; no binding keeps the Thread's.
func TestAResumeWithAnExplicitBindingUsesItFromTheFirstStep(t *testing.T) {
	alt := protocol.Binding{Kind: "model_route", Selector: "fixture-alt", ProfileRevision: "fixture-v2"}
	fake := harnesstest.NewFake()
	fake.SetReply(calls(tc("c1", "file.read", readArgs("demo.txt", 0, 10, 10))))
	r := newModelRig(t, fake, nil, func(l *harnesstest.Layout) {
		toolConfig{}.edit(l)
		l.Cfg["bindings"] = append(l.Cfg["bindings"].([]any), map[string]any{"profile_name": "fixture-alt", "binding": map[string]any{
			"kind": alt.Kind, "selector": alt.Selector, "profile_revision": alt.ProfileRevision, "agent_id": nil, "execution_role": nil}})
	})
	r.conn = r.svc.NewConn(&recorder{})
	r.write("demo.txt", "x")
	info := r.openSession("res.open.00000000000000003")
	first := r.waitTerminal(r.startRun(info.ThreadID, "res.start.000000000000003", "go", func(in *protocol.StartInput) { in.Limits.MaxModelSteps = 1 }).RunID)
	if first.Result.Status != "incomplete" || first.Result.Code != "STEP_BUDGET_EXHAUSTED" || !first.Result.Resumable {
		t.Fatalf("%+v", first.Result)
	}
	task := first.TaskID

	// Not configured: refused, nothing written.
	n := r.count("receipts")
	bad := resumeInput(task, first.RunID, "res.key.00000000000000004")
	bad.Binding = &protocol.Binding{Kind: "model_route", Selector: "elsewhere", ProfileRevision: "v1"}
	_, err := r.call("run/resume", bad)
	wantCode(t, err, protocol.CodeForbidden)
	if r.count("runs") != 1 || r.count("receipts") != n {
		t.Fatal("a refused resume wrote something")
	}

	fake.SetReply(harnesstest.Final("done on the other model"))
	ok := resumeInput(task, first.RunID, "res.key.00000000000000005")
	ok.Binding = &alt
	res := r.mustCall("run/resume", ok)
	res.Done()
	rr := decode[protocol.ResumeResult](t, res)
	if r.waitTerminal(rr.RunID).Result.Status != "completed" {
		t.Fatal("the resumed Run did not complete")
	}
	last := fake.Generates()[len(fake.Generates())-1]
	if last.Model != "fixture-alt" {
		t.Fatalf("the new Run was generated on %q", last.Model)
	}
	if sess := decode[protocol.SessionInfo](t, r.mustCall("session/get", protocol.SessionGetInput{ThreadID: info.ThreadID})); sess.Binding.Selector != "fixture-alt" || sess.Binding.ProfileRevision != "fixture-v2" {
		t.Fatalf("%+v", sess.Binding)
	}
	// The old Run is as it was: its own generation was on the first binding.
	if first2 := decode[protocol.RunInfo](t, r.mustCall("run/get", protocol.RunGetInput{RunID: first.RunID})); first2.Result.Code != "STEP_BUDGET_EXHAUSTED" || fake.Generates()[0].Model != r.binding().Selector {
		t.Fatalf("%+v", first2.Result)
	}
}

// TestAnInterruptBeforeAnyGenerationStopsTheRunAndLeavesNothingUnknown: the stop arrives
// while the count of the prompt is in flight. Nothing was generated, so nothing is unknown:
// the Run is cancelled, no generation was sent and none is listed.
func TestAnInterruptBeforeAnyGenerationStopsTheRunAndLeavesNothingUnknown(t *testing.T) {
	fake := harnesstest.NewFake()
	reached := make(chan struct{})
	var once sync.Once
	fake.OnMeasure = func(ctx context.Context) { once.Do(func() { close(reached) }); <-ctx.Done() }
	r, _ := modelRig(t, fake)
	info := r.openSession("intr.open.0000000000000007")
	start := r.startRun(info.ThreadID, "intr.start.000000000000007", "x")
	<-reached
	if receipt := r.interrupt(start.RunID, 0, "intr.key.00000000000000008"); receipt.Code != "CANCEL_REQUESTED" {
		t.Fatalf("%+v", receipt)
	}
	run := r.waitTerminal(start.RunID)
	if resultKey(run.Result) != "cancelled/CANCELLED/resumable/none" || run.GenerationAttemptsUsed != 0 || len(fake.Generates()) != 0 || r.count("attempts") != 0 {
		t.Fatalf("%+v %+v", run.Result, run)
	}
}

// TestAnInterruptBetweenToolCallsStopsTheCallsThatHaveNotStarted is A12 before the
// dispatch-start, with a real stop: the first call of a response ran; the stop is recorded
// while it is being recorded; the second call is never dispatched (not started, answered
// "not run: cancelled"), and the Run is cancelled.
func TestAnInterruptBetweenToolCallsStopsTheCallsThatHaveNotStarted(t *testing.T) {
	fake := harnesstest.NewFake()
	var runID string
	var r *rig
	var once sync.Once
	r, _ = hookRig(t, fake, toolConfig{}, func(p tools.Point, _ string) error {
		if p == tools.PointAfterExecute {
			once.Do(func() {
				res, err := r.svc.Handle(context.Background(), r.conn, request(t, "turn/interrupt", protocol.InterruptInput{RunID: runID, ExpectedControlRevision: 0, IdempotencyKey: "intr.key.00000000000000009"}))
				if err != nil {
					t.Errorf("%v", err)
					return
				}
				if got := decode[protocol.InterruptReceipt](t, res); got.Code != "CANCEL_REQUESTED" {
					t.Errorf("%+v", got)
				}
			})
		}
		return nil
	})
	fake.SetScript(calls(tc("c1", "file.create", createArgs("a.txt", "x")), tc("c2", "file.create", createArgs("b.txt", "y"))), harnesstest.Final("never"))
	info := r.openSession("intr.open.0000000000000008")
	start := r.startRunWith(info.ThreadID, "intr.start.000000000000008", "go", func(id string) { runID = id })
	run := r.waitTerminal(start.RunID)
	res := run.Result
	if res.Status != "cancelled" || res.Code != "CANCELLED" || len(res.UnresolvedActionIDs) != 0 || len(fake.Generates()) != 1 {
		t.Fatalf("%+v", res)
	}
	if c, ok := r.read("a.txt"); !ok || c != "x" {
		t.Fatal("the call that had started did not run")
	}
	if _, ok := r.read("b.txt"); ok {
		t.Fatal("a call was dispatched after the stop was recorded")
	}
	if got := r.val("SELECT group_concat(a.state, ',') FROM (SELECT a.state FROM attempts a JOIN actions ac ON ac.action_id=a.action_id WHERE ac.kind='tool' ORDER BY a.started_at, a.attempt_id) a"); got != "completed,cancelled" {
		t.Fatalf("%s", got)
	}
	// The exchange is complete in the context: the next Run is told the second call was not run.
	cur := decode[protocol.SessionInfo](t, r.mustCall("session/get", protocol.SessionGetInput{ThreadID: info.ThreadID}))
	next := r.startRun(info.ThreadID, "intr.start.000000000000009", "after", func(in *protocol.StartInput) {
		in.ExpectedContextRevision, in.ExpectedControlRevision = cur.ContextRevision, cur.ControlRevision
	})
	_ = r.waitTerminal(next.RunID)
	views := toolMessages(t, fake.Generates()[1])
	if len(views) != 2 || views[0].EffectState != "completed" || views[1].Error == nil || views[1].Error.Code != "NOT_EXECUTED" || !strings.Contains(views[1].Error.Message, "cancelled") {
		t.Fatalf("%+v", views)
	}
}

// TestResumeAfterAKillAtTheDispatchStartNeverRunsTheCall: the process died right after the
// dispatch-start was recorded, before anything ran. Nothing proves the call did not run, so
// it is unknown, and the resume does not run it: the file the call would have made does
// not exist when the new Run ends, and the call is a call that has one end, unknown.
func TestResumeAfterAKillAtTheDispatchStartNeverRunsTheCall(t *testing.T) {
	hit := make(chan string, 1)
	fake := harnesstest.NewFake()
	r, _ := hookRig(t, fake, toolConfig{}, killAt(tools.PointAfterDispatchStart, hit))
	fake.SetScript(calls(tc("c1", "file.create", createArgs("never.txt", "x"))), harnesstest.Final("moved on"))
	info := r.openSession("res.open.00000000000000004")
	start := r.startRun(info.ThreadID, "res.start.000000000000004", "go")
	<-hit
	time.Sleep(50 * time.Millisecond)
	if _, ok := r.read("never.txt"); ok {
		t.Fatal("the call ran")
	}
	svc2, conn2, _ := r.restart()
	res := mustHandle(t, svc2, conn2, "run/resume", resumeInput(start.TaskID, start.RunID, "res.key.00000000000000006"))
	res.Done()
	rr := decode[protocol.ResumeResult](t, res)
	if got := r.waitTerminalOn(svc2, conn2, rr.RunID).Result; got.Status != "completed" || got.FinalText != "moved on" {
		t.Fatalf("%+v", got)
	}
	if _, ok := r.read("never.txt"); ok {
		t.Fatal("the resume ran the call")
	}
	old := resultOfRun(t, svc2, conn2, start.RunID).Result
	if old.Status != "blocked" || old.Code != "EFFECT_OUTCOME_UNKNOWN" || len(old.UnresolvedActionIDs) != 1 || r.val("SELECT state FROM attempts WHERE action_id=?", old.UnresolvedActionIDs[0]) != "unknown" {
		t.Fatalf("%+v", old)
	}
	// A file tool has no process to ask about: the look says so, and the call stays unknown.
	started := ofType(r.threadEvents(info.ThreadID), "run.started")
	var rec struct {
		Tools []struct {
			Tool    string `json:"tool"`
			Effect  string `json:"effect"`
			Verdict string `json:"verdict"`
		} `json:"tool_attempts"`
	}
	last := started[len(started)-1]
	if last.EvidenceID == nil {
		t.Fatal("the resumed Run's start names no reconciliation")
	}
	if err := json.Unmarshal([]byte(r.evidence(*last.EvidenceID)), &rec); err != nil || len(rec.Tools) != 1 || rec.Tools[0].Tool != "file.create" || rec.Tools[0].Effect != "unknown" || rec.Tools[0].Verdict != "not_applicable" {
		t.Fatalf("%v %+v", err, rec)
	}
}

// TestARunWhoseStopWasRecordedButNeverActedOnIsEndedByTheNextTurn: a stop was recorded for a
// Run nobody drives, and nothing acted on it (the process that was to could not take the
// Thread then). The next request that takes the Thread ends that Run as the cancelled Run
// it is, instead of leaving the Thread busy until the Run's deadline.
func TestARunWhoseStopWasRecordedButNeverActedOnIsEndedByTheNextTurn(t *testing.T) {
	r := newRig(t, nil)
	info := r.openSession("intr.open.0000000000000009")
	first := decode[protocol.StartResult](t, r.mustCall("turn/start", startParams(info.ThreadID, "intr.start.000000000000010", "work")))
	// Only the record, as the store does it: nothing acts on it.
	params, err := json.Marshal(protocol.InterruptInput{RunID: first.RunID, ExpectedControlRevision: 0, IdempotencyKey: "intr.key.00000000000000010"})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := r.store.RecordInterrupt(context.Background(), r.dep.Caller, params); err != nil || out.Result.Code != "CANCEL_REQUESTED" {
		t.Fatalf("%+v %v", out, err)
	}
	if run := decode[protocol.RunInfo](t, r.mustCall("run/get", protocol.RunGetInput{RunID: first.RunID})); run.Terminal {
		t.Fatal("nothing should have ended the Run yet")
	}
	// The next request that takes the Thread ends the Run first; it is refused for its own
	// revisions (the stop moved the control revision), and the Run is ended all the same.
	_, err = r.call("turn/start", startParamsAt(info.ThreadID, "intr.start.000000000000011", "next", 0))
	wantCode(t, err, protocol.CodeRevisionConflict)
	if got := resultKey(decode[protocol.RunInfo](t, r.mustCall("run/get", protocol.RunGetInput{RunID: first.RunID})).Result); got != "cancelled/CANCELLED/resumable/none" {
		t.Fatal(got)
	}
	if sess := decode[protocol.SessionInfo](t, r.mustCall("session/get", protocol.SessionGetInput{ThreadID: info.ThreadID})); sess.ActiveRunID != nil {
		t.Fatalf("the Thread is still busy: %+v", sess)
	}
	in := startParamsAt(info.ThreadID, "intr.start.000000000000012", "next", 0)
	in.ExpectedControlRevision = 1
	second := r.mustCall("turn/start", in)
	if got := eventTypes(second.Events); got != "input.accepted,task.created,run.started" {
		t.Fatalf("%s", got)
	}
}

// TestResumeOfATaskWithAnUnknownGenerationStartsNoGeneration is fail closed through the
// whole path: a generation of the Task was in flight when its process died, so its end is
// unknown, and nothing here can ask the model side. The resume creates its Run (same Task,
// new Trace, the resume's receipt) and ends it at once as blocked with
// MODEL_GENERATION_OUTCOME_UNKNOWN, listing the unknown generation. The model port is not
// called at all, by the resume or by anything it started: no describe, no count, no
// generation. A second resume is the same, and the Thread is free throughout.
func TestResumeOfATaskWithAnUnknownGenerationStartsNoGeneration(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Reply{Kind: harnesstest.KindHang})
	reached := make(chan struct{})
	var once sync.Once
	fake.OnGenerate = func(context.Context, modelport.ChatRequest) { once.Do(func() { close(reached) }) }
	r, _ := modelRig(t, fake)
	info := r.openSession("res.open.00000000000000005")
	start := r.startRun(info.ThreadID, "res.start.000000000000005", "go")
	<-reached
	// The driver's process ends (its locks go); another process, with the same model port,
	// is asked to resume.
	_ = r.writers.Close()
	svc2, conn2, _ := r.restart()
	describes, measures, generates := fake.Describes(), len(fake.Measures()), len(fake.Generates())
	if describes != 1 || measures != 1 || generates != 1 {
		t.Fatalf("setup: %d %d %d", describes, measures, generates)
	}

	res := mustHandle(t, svc2, conn2, "run/resume", resumeInput(start.TaskID, start.RunID, "res.key.00000000000000010"))
	res.Done()
	rr := decode[protocol.ResumeResult](t, res)
	// The take-over settled the old Run (its generation unknown); the new Run is created and ended.
	if got := eventTypes(res.Events); got != "model.completed,run.terminal,run.started,run.terminal" {
		t.Fatalf("%s", got)
	}
	old := resultOfRun(t, svc2, conn2, start.RunID).Result
	next := resultOfRun(t, svc2, conn2, rr.RunID)
	if resultKey(old) != "blocked/MODEL_GENERATION_OUTCOME_UNKNOWN/resumable/unresolved" || !next.Terminal || resultKey(next.Result) != "blocked/MODEL_GENERATION_OUTCOME_UNKNOWN/resumable/unresolved" ||
		len(next.Result.UnresolvedActionIDs) != 1 || next.Result.UnresolvedActionIDs[0] != old.UnresolvedActionIDs[0] || next.Result.FinalText != "" ||
		rr.TaskID != start.TaskID || rr.PreviousRunID != start.RunID || rr.RunID == start.RunID || rr.TraceID == start.TraceID {
		t.Fatalf("%+v %+v", old, next)
	}
	if next.GenerationAttemptsUsed != 0 || r.val("SELECT COUNT(*) FROM actions WHERE run_id=?", rr.RunID) != "0" {
		t.Fatal("the blocked Run reserved a generation")
	}
	if sess := decode[protocol.SessionInfo](t, mustHandle(t, svc2, conn2, "session/get", protocol.SessionGetInput{ThreadID: info.ThreadID})); sess.ActiveRunID != nil {
		t.Fatalf("the Thread is busy with a Run nobody drives: %+v", sess)
	}
	// The evidence of the resume says the generation was not queried.
	started := ofType(res.Events, "run.started")[0]
	if started.EvidenceID == nil || !strings.Contains(r.evidence(*started.EvidenceID), `"queried":false`) {
		t.Fatal("the resume does not say that the generation was not queried")
	}

	// A second resume of the Task: another blocked Run, and still no call to the model port.
	again := mustHandle(t, svc2, conn2, "run/resume", resumeInput(start.TaskID, rr.RunID, "res.key.00000000000000011"))
	again.Done()
	rr2 := decode[protocol.ResumeResult](t, again)
	if rr2.RunID == rr.RunID || rr2.PreviousRunID != rr.RunID || resultKey(resultOfRun(t, svc2, conn2, rr2.RunID).Result) != "blocked/MODEL_GENERATION_OUTCOME_UNKNOWN/resumable/unresolved" {
		t.Fatalf("%+v", rr2)
	}
	// The same request again answers from its receipt.
	replay := mustHandle(t, svc2, conn2, "run/resume", resumeInput(start.TaskID, start.RunID, "res.key.00000000000000010"))
	if decode[protocol.ResumeResult](t, replay) != rr || len(replay.Events) != 0 {
		t.Fatal("a replay of the resume answered something else")
	}

	svc2.Quiesce()
	r.mustCall("service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
	r.svc.Quiesce()
	if fake.Describes() != describes || len(fake.Measures()) != measures || len(fake.Generates()) != generates {
		t.Fatalf("the model port was called by a resume of a Task with an unknown generation: describes %d, counts %d, generations %d",
			fake.Describes()-describes, len(fake.Measures())-measures, len(fake.Generates())-generates)
	}
	if rep, err := r.store.VerifyClosure(t.Context()); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}
