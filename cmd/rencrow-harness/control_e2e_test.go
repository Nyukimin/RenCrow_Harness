package main_test

import (
	"encoding/json"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// TestInterruptAppendAndResumeOverStdio: the methods of a Run's control, on the wire,
// against a Run the Gateway double holds in its count. A stop ends it and says how, in the
// order it happened; a replay answers from the receipt and announces nothing; an input for
// an ended Run is refused; the ended Task is resumed into a new Run of the same Task,
// which a second resume then finds busy.
func TestInterruptAppendAndResumeOverStdio(t *testing.T) {
	f := newGatewayFixture(t, harnesstest.NewFake(), true)
	s := startServe(t, f.l.Config)
	f.initialize(s)
	open := s.mustCall("session/open", f.openParams("ctl.open.00000000000001"))
	thread := open.result()["session"].(map[string]any)["thread_id"].(string)
	s.events(1)
	start := s.mustCall("turn/start", startParams(t, thread, "ctl.start.0000000000001", "do the work"))
	s.events(4)
	f.waitMeasuring(t)
	runID, taskID := start.result()["run_id"].(string), start.result()["task_id"].(string)

	interrupt := map[string]any{"run_id": runID, "expected_control_revision": 0, "idempotency_key": "ctl.stop.000000000000001"}
	stopped := s.mustCall("turn/interrupt", interrupt).result()
	if stopped["code"] != "CANCEL_REQUESTED" || stopped["signal_recorded"] != true || stopped["control_revision"].(json.Number).String() != "1" || stopped["run_id"] != runID {
		t.Fatalf("%v", stopped)
	}
	evs := s.events(2)
	if evs[0].Type != protocol.EventControlCancelRequest || evs[1].Type != protocol.EventRunTerminal || evs[1].Code == nil || *evs[1].Code != "CANCELLED" {
		t.Fatalf("%+v", evs)
	}
	if replay := s.mustCall("turn/interrupt", interrupt); len(replay.notes) != 0 || !jsonEqual(t, replay.result(), stopped) {
		t.Fatalf("a replay of a stop announced or changed something: %v", replay.m)
	}
	run := s.mustCall("run/get", map[string]any{"run_id": runID}).result()
	res, _ := run["result"].(map[string]any)
	if run["terminal"] != true || res == nil || res["status"] != "cancelled" || res["code"] != "CANCELLED" || res["resumable"] != true {
		t.Fatalf("%v", run)
	}
	late := s.mustCall("turn/interrupt", map[string]any{"run_id": runID, "expected_control_revision": 1, "idempotency_key": "ctl.stop.000000000000002"}).result()
	if late["code"] != "ALREADY_TERMINAL" || late["signal_recorded"] != false {
		t.Fatalf("%v", late)
	}

	// An input for the Run that ended is refused; nothing was queued.
	refused := s.call("input/append", map[string]any{"thread_id": thread, "run_id": runID, "input": map[string]any{"text": "more", "origin_proof": nil},
		"disposition": "next_step", "expected_control_revision": 1, "idempotency_key": "ctl.append.0000000000001"})
	if code, info := refused.errCode(t); code != -32600 || info != protocol.CodeInvalidRequest {
		t.Fatalf("%v", refused.m)
	}

	limits := map[string]any{"max_model_steps": 5, "max_tool_calls_per_step": 4, "deadline_seconds": 600, "max_capture_bytes": 1048576, "max_generation_attempts": 8}
	resume := map[string]any{"task_id": taskID, "expected_last_run_id": runID, "checkpoint_id": nil, "expected_control_revision": 1, "binding": nil,
		"idempotency_key": "ctl.resume.000000000001", "limits": limits}
	resumed := s.mustCall("run/resume", resume)
	rr := resumed.result()
	started := s.events(1)
	if rr["task_id"] != taskID || rr["thread_id"] != thread || rr["previous_run_id"] != runID || rr["run_id"] == runID || rr["trace_id"] == start.result()["trace_id"] || rr["checkpoint_id"] != nil ||
		started[0].Type != protocol.EventRunStarted || started[0].RunID == nil || *started[0].RunID != rr["run_id"] {
		t.Fatalf("%v %+v", rr, started)
	}
	if again := s.mustCall("run/resume", resume); len(again.notes) != 0 || !jsonEqual(t, again.result(), rr) {
		t.Fatalf("a replay of a resume started something: %v", again.m)
	}
	busy := s.call("run/resume", map[string]any{"task_id": taskID, "expected_last_run_id": rr["run_id"], "checkpoint_id": nil, "expected_control_revision": 1, "binding": nil,
		"idempotency_key": "ctl.resume.000000000002", "limits": limits})
	if code, info := busy.errCode(t); code != -32000 || info != protocol.CodeBusy {
		t.Fatalf("%v", busy.m)
	}
	if got := s.mustCall("run/get", map[string]any{"run_id": rr["run_id"]}).result(); got["terminal"] != false {
		t.Fatalf("%v", got)
	}
	_ = s.mustCall("service/shutdown", map[string]any{"mode": "drain", "deadline_seconds": 1})
	if err := s.wait(); err != nil {
		t.Fatalf("exit: %v\nstderr: %s", err, s.stderr.String())
	}
}
