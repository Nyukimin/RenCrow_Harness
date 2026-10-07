package sqlite

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func interruptParams(runID string, expected int64, key string) []byte {
	b, err := json.Marshal(map[string]any{"run_id": runID, "expected_control_revision": expected, "idempotency_key": key})
	if err != nil {
		panic(err)
	}
	return b
}

// endRun ends a running Run the way a take-over by another driver does (its Thread's
// epoch is raised and the Run settled), with the given result. It leaves the Thread free.
func (e *env) endRun(_ *run, status, code string, resumable bool) {
	e.t.Helper()
	epoch, err := e.s.BumpWriterEpoch(bg, e.thread)
	if err != nil {
		e.t.Fatal(err)
	}
	_, err = e.s.TerminalizeStale(bg, e.thread, epoch, nil, func(st StaleRun) (TerminalInput, error) {
		res := protocol.RunResult{RunID: st.RunID, TaskID: st.TaskID, Status: status, Code: code, Verification: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}},
			EvidenceIDs: st.EvidenceIDs, Resumable: resumable, UnresolvedActionIDs: []string{}}
		return TerminalInput{Result: res, ResultEvidenceID: identity.NewEvidenceID().String()}, nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// TestInterruptRecordsTheSignalOnceAndOnlyTheSignal: turn/interrupt moves the control
// revision by one and writes control.cancel_requested with its receipt, all together. It
// ends nothing and applies nothing: the Run is as it was, its phase included. A replay
// answers from the receipt, a second key finds the stop already requested and records
// nothing again, and a caller that has not seen the revision is refused whatever else is
// true.
func TestInterruptRecordsTheSignalOnceAndOnlyTheSignal(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("int.run.0000000000000001")
	before := e.counts()
	if scalar[int](t, e.s.db, "SELECT event_seq FROM threads") != 3 {
		t.Fatal("setup")
	}

	out, err := e.s.RecordInterrupt(bg, e.caller, interruptParams(r.start.RunID, 0, "int.key.00000000000001"))
	if err != nil {
		t.Fatal(err)
	}
	res := out.Result
	if out.Replayed || out.ThreadID != e.thread || res.Code != protocol.InterruptCancelRequested || !res.SignalRecorded || res.ControlRevision != 1 || res.RunID != r.start.RunID ||
		len(out.Events) != 1 || out.Events[0].Type != "control.cancel_requested" {
		t.Fatalf("%+v", out)
	}
	ev := out.Events[0]
	p := payloadOf[protocol.ControlCancelRequestedPayload](t, ev)
	if p.RunID != r.start.RunID || p.ControlRevision != 1 || p.Reason != CancelReasonInterrupt || p.Principal != "core:local" ||
		ev.RunID == nil || *ev.RunID != r.start.RunID || ev.TaskID == nil || *ev.TaskID != r.start.TaskID || ev.ReceiptID == nil || *ev.ReceiptID != res.ReceiptID || ev.EventSeq != 4 {
		t.Fatalf("%+v %+v", p, ev)
	}
	if got := scalar[string](t, e.s.db, "SELECT control_revision||'/'||queue_revision||'/'||context_revision||'/'||event_seq||'/'||active_run_id FROM threads"); got != "1/0/0/4/"+r.start.RunID {
		t.Fatalf("%s", got)
	}
	// Only the signal: the Run is as it was.
	if got := scalar[string](t, e.s.db, "SELECT phase||'/'||status FROM runs"); got != "Admitting/running" {
		t.Fatalf("a recorded stop is not an ended Run: %s", got)
	}
	if got := scalar[string](t, e.s.db, "SELECT operation||'/'||stage FROM receipts WHERE receipt_id=?", res.ReceiptID); got != "turn/interrupt/terminal" {
		t.Fatalf("%s", got)
	}
	after := e.counts()
	if after["receipts"] != before["receipts"]+1 || after["events"] != before["events"]+1 || after["runs"] != before["runs"] || after["items"] != before["items"] {
		t.Fatalf("%v -> %v", before, after)
	}
	// The receipt is readable through receipt/get, with the same result.
	rec, err := e.s.GetReceipt(bg, e.caller, res.ReceiptID)
	if err != nil || rec.Result == nil || rec.Result.Type != protocol.ReceiptInterruptReceipt {
		t.Fatalf("%+v %v", rec, err)
	}

	// The same request again: the same answer, nothing written.
	n := e.counts()
	again, err := e.s.RecordInterrupt(bg, e.caller, interruptParams(r.start.RunID, 0, "int.key.00000000000001"))
	if err != nil || !again.Replayed || again.Result != res || len(again.Events) != 0 {
		t.Fatalf("%+v %v", again, err)
	}
	e.wantNoChange(n)
	// The same key for another request is a conflict, not a second stop.
	if _, err := e.s.RecordInterrupt(bg, e.caller, interruptParams(r.start.RunID, 1, "int.key.00000000000001")); protocol.CodeOf(err) != protocol.CodeIdempotencyConflict {
		t.Fatalf("%v", err)
	}
	e.wantNoChange(n)

	// Another key that has not seen the stop is refused (it expected revision 0); one that
	// has gets the existing request back, and nothing changes: a stop is not stacked.
	if _, err := e.s.RecordInterrupt(bg, e.caller, interruptParams(r.start.RunID, 0, "int.key.00000000000002")); protocol.CodeOf(err) != protocol.CodeRevisionConflict {
		t.Fatalf("%v", err)
	}
	second, err := e.s.RecordInterrupt(bg, e.caller, interruptParams(r.start.RunID, 1, "int.key.00000000000003"))
	if err != nil || second.Replayed || second.Result.Code != protocol.InterruptCancelRequested || !second.Result.SignalRecorded || second.Result.ControlRevision != 1 ||
		len(second.Events) != 0 || second.Result.ReceiptID == res.ReceiptID {
		t.Fatalf("%+v %v", second, err)
	}
	if scalar[string](t, e.s.db, "SELECT control_revision||'/'||event_seq FROM threads") != "1/4" || scalar[int](t, e.s.db, "SELECT COUNT(*) FROM events WHERE type='control.cancel_requested'") != 1 {
		t.Fatal("a second request stacked a second stop")
	}
	if rep, err := e.s.VerifyClosure(bg); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}

// TestInterruptOfAnEndedRunRecordsNothing: a Run that has its result cannot be stopped,
// and the answer says so; the receipt is kept so the same request answers the same.
func TestInterruptOfAnEndedRunRecordsNothing(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("int.run.0000000000000002")
	e.endRun(r, "incomplete", "DRIVER_STOPPED", true)
	n := e.counts()
	rev := scalar[string](t, e.s.db, "SELECT control_revision||'/'||event_seq FROM threads")

	out, err := e.s.RecordInterrupt(bg, e.caller, interruptParams(r.start.RunID, 0, "int.key.00000000000011"))
	if err != nil || out.Result.Code != protocol.InterruptAlreadyTerminal || out.Result.SignalRecorded || out.Result.ControlRevision != 0 || len(out.Events) != 0 {
		t.Fatalf("%+v %v", out, err)
	}
	after := e.counts()
	if after["receipts"] != n["receipts"]+1 || after["events"] != n["events"] || scalar[string](t, e.s.db, "SELECT control_revision||'/'||event_seq FROM threads") != rev {
		t.Fatal("a stop of an ended Run changed the Thread")
	}
	// An ended Run is stopped by nobody: whatever revision the caller expects.
	again, err := e.s.RecordInterrupt(bg, e.caller, interruptParams(r.start.RunID, 7, "int.key.00000000000012"))
	if err != nil || again.Result.Code != protocol.InterruptAlreadyTerminal {
		t.Fatalf("%+v %v", again, err)
	}
	// The same key again is the first answer.
	replay, err := e.s.RecordInterrupt(bg, e.caller, interruptParams(r.start.RunID, 0, "int.key.00000000000011"))
	if err != nil || !replay.Replayed || replay.Result != out.Result {
		t.Fatalf("%+v %v", replay, err)
	}
}

// TestInterruptRefusesWhatIsNotTheCallers: a Run of a Thread the caller cannot read is
// the same FORBIDDEN as one that does not exist; a caller that may only read cannot stop.
func TestInterruptRefusesWhatIsNotTheCallers(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("int.run.0000000000000003")
	n := e.counts()
	stranger := e.newCaller(func(c *intake.CallerConfig) { c.Principal, c.Relays = "user:stranger", nil })
	reader := e.newCaller(func(c *intake.CallerConfig) {
		c.Principal, c.Relays, c.ReadableSessionOwners = "user:reader", nil, []string{"core:local"}
	})
	for name, tc := range map[string]struct {
		caller intake.Caller
		run    string
	}{
		"no such run":  {e.caller, "run_00000000-0000-7000-8000-000000000001"},
		"not readable": {stranger, r.start.RunID},
		"read only":    {reader, r.start.RunID},
	} {
		if _, err := e.s.RecordInterrupt(bg, tc.caller, interruptParams(tc.run, 0, "int.key.00000000000021")); protocol.CodeOf(err) != protocol.CodeForbidden {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Params that are not a request are not looked up at all.
	if _, err := e.s.RecordInterrupt(bg, e.caller, []byte(`{"run_id":"x"}`)); protocol.CodeOf(err) != protocol.CodeInvalidParams {
		t.Fatalf("%v", err)
	}
	e.wantNoChange(n)
	if scalar[string](t, e.s.db, "SELECT control_revision FROM threads") != "0" {
		t.Fatal("a refused stop moved the revision")
	}
}

// TestAStopRecordedFirstIsNotOvertakenByTheRunsEnd is STORAGE section 6 for a result: a
// stop that was recorded before a Run could complete means the final text is not adopted
// (ErrControlChanged), and the Run can still be ended as the cancelled Run it is; a stop
// that comes after the Run ended finds an ended Run.
func TestAStopRecordedFirstIsNotOvertakenByTheRunsEnd(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("int.run.0000000000000004")
	r.goTo("PersistingResult")
	if _, err := e.s.RecordInterrupt(bg, e.caller, interruptParams(r.start.RunID, 0, "int.key.00000000000031")); err != nil {
		t.Fatal(err)
	}
	final := identity.NewMessageID().String()
	finalEv, resultEv := identity.NewEvidenceID().String(), identity.NewEvidenceID().String()
	evidence, err := e.s.RunEvidenceIDs(bg, r.start.RunID)
	if err != nil {
		t.Fatal(err)
	}
	completed := TerminalInput{FinalText: "the answer", FinalEvidenceID: finalEv, ResultEvidenceID: resultEv, Result: protocol.RunResult{
		RunID: r.start.RunID, TaskID: r.start.TaskID, Status: "completed", Code: "FINAL_RESPONSE_ACCEPTED", FinalMessageID: &final, FinalText: "the answer",
		Verification: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}}, EvidenceIDs: append(evidence, finalEv), UnresolvedActionIDs: []string{},
	}}
	n := e.counts()
	if _, err := e.s.PersistTerminal(bg, r.fence, completed); !errors.Is(err, ErrControlChanged) {
		t.Fatalf("a Run completed over a recorded stop: %v", err)
	}
	e.wantNoChange(n)
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM context_entries") != 0 {
		t.Fatal("the final text of a stopped Run was applied")
	}
	// The same driver ends the Run as cancelled: no final text, nothing adopted.
	cancelled := TerminalInput{ResultEvidenceID: identity.NewEvidenceID().String(), Result: protocol.RunResult{
		RunID: r.start.RunID, TaskID: r.start.TaskID, Status: "cancelled", Code: "CANCELLED", Verification: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}},
		EvidenceIDs: evidence, UnresolvedActionIDs: []string{}, Resumable: true,
	}}
	events, err := e.s.PersistTerminal(bg, r.fence, cancelled)
	if err != nil || len(events) != 1 || events[0].Type != "run.terminal" {
		t.Fatalf("%v %v", events, err)
	}
	if got := scalar[string](t, e.s.db, "SELECT status||'/'||phase FROM runs"); got != "cancelled/Terminal" {
		t.Fatal(got)
	}

	// A stop that comes after the Run ended finds an ended Run, and records nothing.
	e2 := newEnv(t)
	r2 := e2.newRun("int.run.0000000000000005")
	r2.goTo("PersistingResult")
	final2, finalEv2 := identity.NewMessageID().String(), identity.NewEvidenceID().String()
	ev2, err := e2.s.RunEvidenceIDs(bg, r2.start.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e2.s.PersistTerminal(bg, r2.fence, TerminalInput{FinalText: "the answer", FinalEvidenceID: finalEv2, ResultEvidenceID: identity.NewEvidenceID().String(), Result: protocol.RunResult{
		RunID: r2.start.RunID, TaskID: r2.start.TaskID, Status: "completed", Code: "FINAL_RESPONSE_ACCEPTED", FinalMessageID: &final2, FinalText: "the answer",
		Verification: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}}, EvidenceIDs: append(ev2, finalEv2), UnresolvedActionIDs: []string{},
	}}); err != nil {
		t.Fatal(err)
	}
	out, err := e2.s.RecordInterrupt(bg, e2.caller, interruptParams(r2.start.RunID, 0, "int.key.00000000000032"))
	if err != nil || out.Result.Code != protocol.InterruptAlreadyTerminal || out.Result.SignalRecorded {
		t.Fatalf("%+v %v", out, err)
	}
}

// TestAStopRecordedBeforeADispatchStopsTheDispatch: the control revision a driver holds
// is the one it was admitted at; once a stop is recorded no generation and no Tool call
// can start on it (A12, before). One that started before the stop is a call that has
// started, and its end is still recorded.
func TestAStopRecordedBeforeADispatchStopsTheDispatch(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("int.run.0000000000000006")
	r.goTo("Generating")
	if _, err := e.s.RecordInterrupt(bg, e.caller, interruptParams(r.start.RunID, 0, "int.key.00000000000041")); err != nil {
		t.Fatal(err)
	}
	n := e.counts()
	if _, err := e.s.ReserveGeneration(bg, r.fence, reserveInput()); !errors.Is(err, ErrControlChanged) {
		t.Fatalf("%v", err)
	}
	e.wantNoChange(n)
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM attempts") != 0 {
		t.Fatal("a generation started over a stop")
	}
	// The driver that reads the Thread again sees a stop is recorded.
	got, err := e.s.CancelRequested(bg, r.start.RunID)
	rev, rerr := e.s.ControlRevision(bg, e.thread)
	if err != nil || rerr != nil || !got || rev != 1 {
		t.Fatalf("%v %v %v %d", got, err, rerr, rev)
	}
}
