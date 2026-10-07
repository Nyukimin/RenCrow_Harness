package sqlite

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func appendParams(thread, run, text, disposition string, expected int64, key string, mod func(m map[string]any)) []byte {
	m := map[string]any{
		"thread_id": thread, "run_id": run, "input": map[string]any{"text": text, "origin_proof": nil},
		"disposition": disposition, "expected_control_revision": expected, "idempotency_key": key,
	}
	if mod != nil {
		mod(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

func (e *env) appendInput(r *run, text, disposition string, expected int64, key string) AppendOutcome {
	e.t.Helper()
	out, err := e.s.AppendInput(bg, e.adm, appendParams(e.thread, r.start.RunID, text, disposition, expected, key, nil))
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// TestAppendInputStoresEachDispositionAndAppliesNone: accepting an input is not applying
// it. Each of the three dispositions saves the raw text as sealed Evidence and an item,
// adds a queue row and moves the queue revision; none touches the context or its
// revision. next_step and next_turn are queued and leave the control revision alone;
// interrupt_current is deferred and moves the control revision in the same transaction,
// with the stop recorded as its own event, not as an entry of the ordinary queue.
func TestAppendInputStoresEachDispositionAndAppliesNone(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("app.run.0000000000000001")
	before := e.counts()

	step := e.appendInput(r, "next step please", protocol.DispositionNextStep, 0, "app.key.0000000000000001")
	rc := step.Result
	if step.StopRequested {
		t.Fatal("a next_step input is not a stop")
	}
	if step.Replayed || rc.Disposition != "next_step" || rc.DeliveryState != "queued" || rc.QueueRevision != 1 || rc.ControlRevision != 0 || rc.Origin != "automation" ||
		!strings.HasPrefix(rc.QueueItemID, "qit_") || rc.Intake.AcceptedSequence != 6 || rc.Intake.ThreadID != e.thread || rc.Intake.Principal != "core:local" ||
		rc.Intake.ReceiptID != rc.ReceiptID || rc.Intake.MessageID != rc.MessageID || len(step.Events) != 1 || step.Events[0].Type != "input.accepted" {
		t.Fatalf("%+v", step)
	}
	ev := payloadOf[protocol.InputAcceptedPayload](t, step.Events[0])
	if ev.Disposition != "next_step" || ev.QueueItemID == nil || *ev.QueueItemID != rc.QueueItemID || ev.QueueRevision != 1 || ev.ControlRevision != 0 || ev.Intake.MessageID != rc.MessageID ||
		step.Events[0].RunID == nil || *step.Events[0].RunID != r.start.RunID || step.Events[0].MessageID == nil || *step.Events[0].MessageID != rc.MessageID ||
		step.Events[0].EvidenceID == nil || *step.Events[0].EvidenceID != rc.Intake.EvidenceID {
		t.Fatalf("%+v %+v", ev, step.Events[0])
	}
	if got := queryString(t, e.s.db, "SELECT disposition||'|'||delivery_state||'|'||queue_revision||'|'||run_id||'|'||COALESCE(applied_context_revision,'none') FROM queue_inputs WHERE queue_item_id=?", rc.QueueItemID); got != "next_step|queued|1|"+r.start.RunID+"|none" {
		t.Fatalf("%s", got)
	}
	// The original is sealed Evidence, kept exactly.
	if got := queryString(t, e.s.db, "SELECT state||'|'||total_bytes FROM evidence WHERE evidence_id=?", rc.Intake.EvidenceID); got != "sealed|16" {
		t.Fatalf("%s", got)
	}
	if got := queryString(t, e.s.db, "SELECT history_kind||'|'||origin||'|'||sequence FROM items WHERE message_id=?", rc.MessageID); got != "AutomationInstruction|automation|6" {
		t.Fatalf("%s", got)
	}

	turn := e.appendInput(r, "after this turn", protocol.DispositionNextTurn, 0, "app.key.0000000000000002")
	if turn.Result.DeliveryState != "queued" || turn.Result.QueueRevision != 2 || turn.Result.ControlRevision != 0 || len(turn.Events) != 1 {
		t.Fatalf("%+v", turn)
	}

	stop := e.appendInput(r, "stop and do this", protocol.DispositionInterruptCurrent, 0, "app.key.0000000000000003")
	sr := stop.Result
	if !stop.StopRequested {
		t.Fatal("an interrupt_current input is a stop")
	}
	if sr.DeliveryState != "deferred" || sr.QueueRevision != 3 || sr.ControlRevision != 1 || len(stop.Events) != 2 || stop.Events[0].Type != "input.accepted" || stop.Events[1].Type != "control.cancel_requested" {
		t.Fatalf("%+v", stop)
	}
	cancel := payloadOf[protocol.ControlCancelRequestedPayload](t, stop.Events[1])
	acc := payloadOf[protocol.InputAcceptedPayload](t, stop.Events[0])
	if cancel.ControlRevision != 1 || cancel.Reason != CancelReasonInputInterrupt || cancel.Principal != "core:local" || cancel.RunID != r.start.RunID || acc.ControlRevision != 1 || acc.QueueRevision != 3 ||
		stop.Events[1].ReceiptID == nil || *stop.Events[1].ReceiptID != sr.ReceiptID {
		t.Fatalf("%+v %+v", cancel, acc)
	}

	// Nothing was applied: the context and its revision are as the Run's start left them.
	if got := scalar[string](t, e.s.db, "SELECT context_revision||'/'||control_revision||'/'||queue_revision||'/'||event_seq FROM threads"); got != "0/1/3/7" {
		t.Fatalf("%s", got)
	}
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM context_entries") != 0 || scalar[int](t, e.s.db, "SELECT COUNT(*) FROM queue_inputs WHERE delivery_state='applied'") != 0 {
		t.Fatal("accepting an input applied it")
	}
	after := e.counts()
	if after["items"] != before["items"]+3 || after["receipts"] != before["receipts"]+3 || after["events"] != before["events"]+4 || after["runs"] != before["runs"] || after["evidence"] != before["evidence"]+3 {
		t.Fatalf("%v -> %v", before, after)
	}
	// The stop is the control change, the queue is the queue: the cancel event is not
	// a queue entry and the queue has exactly the three inputs.
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM queue_inputs") != 3 || scalar[int](t, e.s.db, "SELECT COUNT(*) FROM events WHERE type='control.cancel_requested'") != 1 {
		t.Fatal("the stop is not separate from the queue")
	}

	// A second interrupt_current finds the stop already requested: queued deferred, no
	// second stop and no second revision.
	again := e.appendInput(r, "and this too", protocol.DispositionInterruptCurrent, 1, "app.key.0000000000000004")
	if !again.StopRequested {
		t.Fatal("a second interrupt_current is a stop too: whoever acts on stops acts on it")
	}
	if again.Result.DeliveryState != "deferred" || again.Result.ControlRevision != 1 || again.Result.QueueRevision != 4 || len(again.Events) != 1 || again.Events[0].Type != "input.accepted" {
		t.Fatalf("%+v", again)
	}
	replay, err := e.s.AppendInput(bg, e.adm, appendParams(e.thread, r.start.RunID, "stop and do this", "interrupt_current", 0, "app.key.0000000000000003", nil))
	if err != nil || !replay.Replayed || !replay.StopRequested || len(replay.Events) != 0 {
		t.Fatalf("%+v %v", replay, err)
	}
	if rep, err := e.s.VerifyClosure(bg); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}

// TestAppendInputIsIdempotentAndRefusesWhatItShould: the same request is the same
// answer and writes nothing; and nothing is stored for an input that cannot be taken.
func TestAppendInputIsIdempotentAndRefusesWhatItShould(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("app.run.0000000000000002")
	const key = "app.key.0000000000000011"
	first := e.appendInput(r, "once", protocol.DispositionNextStep, 0, key)
	n := e.counts()

	// A replay, even after the Thread moved on (a stop moved the revision).
	if _, err := e.s.RecordInterrupt(bg, e.caller, interruptParams(r.start.RunID, 0, "app.int.0000000000000011")); err != nil {
		t.Fatal(err)
	}
	n = e.counts()
	again, err := e.s.AppendInput(bg, e.adm, appendParams(e.thread, r.start.RunID, "once", "next_step", 0, key, nil))
	if err != nil || !again.Replayed || again.Result != first.Result || len(again.Events) != 0 {
		t.Fatalf("%+v %v", again, err)
	}
	e.wantNoChange(n)
	// The same key for another request is a conflict.
	if _, err := e.s.AppendInput(bg, e.adm, appendParams(e.thread, r.start.RunID, "twice", "next_step", 0, key, nil)); protocol.CodeOf(err) != protocol.CodeIdempotencyConflict {
		t.Fatalf("%v", err)
	}
	e.wantNoChange(n)

	for name, tc := range map[string]struct {
		params []byte
		caller func() Admission
		want   string
	}{
		"a stale control revision": {appendParams(e.thread, r.start.RunID, "x", "next_step", 0, "app.key.0000000000000012", nil), nil, protocol.CodeRevisionConflict},
		"a run of no such thread":  {appendParams(e.thread, "run_00000000-0000-7000-8000-000000000001", "x", "next_step", 1, "app.key.0000000000000013", nil), nil, protocol.CodeForbidden},
		"no such thread":           {appendParams("thr_00000000-0000-7000-8000-000000000001", r.start.RunID, "x", "next_step", 1, "app.key.0000000000000014", nil), nil, protocol.CodeForbidden},
		"an unknown disposition":   {appendParams(e.thread, r.start.RunID, "x", "right_now", 1, "app.key.0000000000000015", nil), nil, protocol.CodeInvalidParams},
		"a stranger": {appendParams(e.thread, r.start.RunID, "x", "next_step", 1, "app.key.0000000000000016", nil), func() Admission {
			a := e.adm
			a.Caller = e.newCaller(func(c *intake.CallerConfig) { c.Principal, c.Relays = "user:stranger", nil })
			return a
		}, protocol.CodeForbidden},
	} {
		adm := e.adm
		if tc.caller != nil {
			adm = tc.caller()
		}
		if _, err := e.s.AppendInput(bg, adm, tc.params); protocol.CodeOf(err) != tc.want {
			t.Errorf("%s: %v", name, err)
		}
	}
	e.wantNoChange(n)

	// An ended Run takes no more input: the Thread is free, so a new turn is the way.
	e.endRun(r, "incomplete", "DRIVER_STOPPED", true)
	n = e.counts()
	if _, err := e.s.AppendInput(bg, e.adm, appendParams(e.thread, r.start.RunID, "late", "next_step", 1, "app.key.0000000000000017", nil)); protocol.CodeOf(err) != protocol.CodeInvalidRequest {
		t.Fatalf("%v", err)
	}
	e.wantNoChange(n)
}

// TestAppendInputVerifiesAProofAndNeverUsesANonceTwice: an appended input has its origin
// decided exactly as a turn's is. A proof is bound to its thread, key and text; a nonce
// that was used is refused for any other operation; a replay of the accepted request is
// answered from the receipt after the proof has expired.
func TestAppendInputVerifiesAProofAndNeverUsesANonceTwice(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("app.run.0000000000000003")
	const key = "app.key.0000000000000021"
	signed := e.proof(e.thread, key, "from a person", "nonce-cccccccccccccccc", "human", testNow.Add(-time.Minute), 5*time.Minute)
	withProof := func(p map[string]any) func(m map[string]any) {
		return func(m map[string]any) { m["input"].(map[string]any)["origin_proof"] = p }
	}
	out, err := e.s.AppendInput(bg, e.adm, appendParams(e.thread, r.start.RunID, "from a person", "next_step", 0, key, withProof(signed)))
	if err != nil {
		t.Fatal(err)
	}
	if out.Result.Origin != "human" || out.Result.Intake.ProofBasis != "verified_relay" || out.Result.Intake.ProofDigest == nil {
		t.Fatalf("%+v", out.Result)
	}
	if got := queryString(t, e.s.db, "SELECT history_kind FROM items WHERE message_id=?", out.Result.MessageID); got != "HumanInstruction" {
		t.Fatal(got)
	}
	if got := queryString(t, e.s.db, "SELECT nonce||'|'||receipt_id FROM relay_nonces"); got != "nonce-cccccccccccccccc|"+out.Result.ReceiptID {
		t.Fatal(got)
	}
	n := e.counts()
	// Another key with the same nonce: refused. A proof for another text: refused.
	reuse := e.proof(e.thread, "app.key.0000000000000022", "second", "nonce-cccccccccccccccc", "human", testNow.Add(-time.Minute), 5*time.Minute)
	if _, err := e.s.AppendInput(bg, e.adm, appendParams(e.thread, r.start.RunID, "second", "next_step", 0, "app.key.0000000000000022", withProof(reuse))); protocol.CodeOf(err) != protocol.CodeInvalidOriginProof {
		t.Fatalf("%v", err)
	}
	other := e.proof(e.thread, "app.key.0000000000000023", "signed text", "nonce-dddddddddddddddd", "human", testNow.Add(-time.Minute), 5*time.Minute)
	if _, err := e.s.AppendInput(bg, e.adm, appendParams(e.thread, r.start.RunID, "different text", "next_step", 0, "app.key.0000000000000023", withProof(other))); protocol.CodeOf(err) != protocol.CodeInvalidOriginProof {
		t.Fatalf("%v", err)
	}
	e.wantNoChange(n)
	// The accepted request again, with its proof long expired: the receipt answers.
	e.s.clock = intake.FixedClock(testNow.Add(time.Hour))
	again, err := e.s.AppendInput(bg, e.adm, appendParams(e.thread, r.start.RunID, "from a person", "next_step", 0, key, withProof(signed)))
	if err != nil || !again.Replayed || !reflect.DeepEqual(again.Result, out.Result) {
		t.Fatalf("%+v %v", again, err)
	}
}

// TestQueuedInputsAreAppliedOnceInTheOrderTheyWereAccepted is F23: a next_step input is
// applied at the step boundary of its own Run (and not when a stop is recorded); inputs a
// Run leaves unapplied (next_turn, a deferred interrupt_current, a next_step that came
// after the last step) are applied by the Load of the Thread's next Run, before that
// Run's own input, in the order they were accepted; and nothing is applied twice.
func TestQueuedInputsAreAppliedOnceInTheOrderTheyWereAccepted(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("app.run.0000000000000004")
	r.phase("Admitting", "Loading")
	if _, err := e.s.ApplyInput(bg, r.fence); err != nil {
		t.Fatal(err)
	}
	// Nothing queued: nothing to apply, nothing written.
	if evs, n, err := e.s.ApplyNextSteps(bg, r.fence); err != nil || n != 0 || len(evs) != 0 {
		t.Fatalf("%v %d %v", evs, n, err)
	}
	a := e.appendInput(r, "step A", protocol.DispositionNextStep, 0, "app.key.0000000000000031")
	b := e.appendInput(r, "later B", protocol.DispositionNextTurn, 0, "app.key.0000000000000032")
	c := e.appendInput(r, "step C", protocol.DispositionNextStep, 0, "app.key.0000000000000033")

	evs, n, err := e.s.ApplyNextSteps(bg, r.fence)
	if err != nil || n != 2 || len(evs) != 2 {
		t.Fatalf("%v %d %v", evs, n, err)
	}
	for i, want := range []struct {
		msg, item string
		rev       int64
	}{{a.Result.MessageID, a.Result.QueueItemID, 2}, {c.Result.MessageID, c.Result.QueueItemID, 3}} {
		p := payloadOf[protocol.InputAppliedPayload](t, evs[i])
		if p.MessageID != want.msg || p.QueueItemID == nil || *p.QueueItemID != want.item || p.ContextRevision != want.rev || p.Disposition != "next_step" ||
			evs[i].MessageID == nil || *evs[i].MessageID != want.msg || evs[i].RunID == nil || *evs[i].RunID != r.start.RunID {
			t.Fatalf("%d: %+v", i, p)
		}
	}
	if got := queryString(t, e.s.db, "SELECT group_concat(delivery_state||':'||COALESCE(applied_context_revision,'-'),',') FROM (SELECT * FROM queue_inputs ORDER BY queue_revision)"); got != "applied:2,queued:-,applied:3" {
		t.Fatalf("%s", got)
	}
	// Applied in the order they were accepted, after the Run's own input.
	snap, err := e.s.LoadSnapshot(bg, r.start.RunID)
	if err != nil || len(snap.Applied) != 3 || snap.Applied[1].MessageID != a.Result.MessageID || snap.Applied[2].MessageID != c.Result.MessageID ||
		snap.Applied[1].Text != "step A" || snap.Applied[2].Text != "step C" || snap.ContextRevision != 3 || snap.PendingInputs != 1 {
		t.Fatalf("%+v %v", snap, err)
	}
	// Once: a second call applies nothing.
	if evs, n, err := e.s.ApplyNextSteps(bg, r.fence); err != nil || n != 0 || len(evs) != 0 {
		t.Fatalf("%v %d %v", evs, n, err)
	}

	// A stop recorded: the Run is not fed more input.
	late := e.appendInput(r, "too late", protocol.DispositionNextStep, 0, "app.key.0000000000000034")
	if _, err := e.s.RecordInterrupt(bg, e.caller, interruptParams(r.start.RunID, 0, "app.int.0000000000000031")); err != nil {
		t.Fatal(err)
	}
	n0 := e.counts()
	if _, _, err := e.s.ApplyNextSteps(bg, r.fence); !errors.Is(err, ErrControlChanged) {
		t.Fatalf("%v", err)
	}
	e.wantNoChange(n0)
	e.appendInput(r, "instead do this", protocol.DispositionInterruptCurrent, 1, "app.key.0000000000000035")

	// The Run ends. The next Run of the Thread starts: its Load applies what is waiting,
	// in acceptance order, before its own input, each once.
	e.endRun(r, "cancelled", "CANCELLED", true)
	adm := e.adm
	adm.WriterEpoch = 2
	next, err := e.s.AdmitStart(bg, adm, e.params(e.thread, "app.start.00000000000001", func(m map[string]any) {
		m["expected_context_revision"], m["expected_control_revision"] = 3, 1
	}))
	if err != nil {
		t.Fatal(err)
	}
	fence := Fence{ThreadID: e.thread, RunID: next.Result.RunID, Epoch: 2, ControlRevision: 1}
	if err := e.s.SetPhase(bg, fence, "Admitting", "Loading"); err != nil {
		t.Fatal(err)
	}
	applied, err := e.s.ApplyInput(bg, fence)
	if err != nil || len(applied) != 4 {
		t.Fatalf("%v %v", applied, err)
	}
	var order []string
	for _, ev := range applied {
		p := payloadOf[protocol.InputAppliedPayload](t, ev)
		order = append(order, p.Disposition)
		if ev.RunID == nil || *ev.RunID != next.Result.RunID {
			t.Fatalf("an input is applied for the Run that applies it: %+v", ev)
		}
	}
	if strings.Join(order, ",") != "next_turn,next_step,interrupt_current,initial" {
		t.Fatalf("%v", order)
	}
	if applied[0].MessageID == nil || *applied[0].MessageID != b.Result.MessageID || applied[1].MessageID == nil || *applied[1].MessageID != late.Result.MessageID ||
		applied[3].MessageID == nil || *applied[3].MessageID != next.Result.Intake.MessageID {
		t.Fatalf("the order of application is the order of acceptance")
	}
	if got := scalar[string](t, e.s.db, "SELECT COUNT(*)||'/'||SUM(delivery_state='applied') FROM queue_inputs"); got != "5/5" {
		t.Fatalf("%s", got)
	}
	// Once more: nothing.
	if again, err := e.s.ApplyInput(bg, fence); err != nil || len(again) != 0 {
		t.Fatalf("%v %v", again, err)
	}
	if rep, err := e.s.VerifyClosure(bg); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}
