package service_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// TestProcessExecIsRefusedWhereTheModeOrThePolicyDoesNotGiveIt: structured_only never
// offers process.exec (and a call to it is a response the Harness cannot use); in
// trusted_host a request no profile covers is refused before any process starts, and
// two profiles that both cover it stop the Run, since that is the host's mistake.
func TestProcessExecIsRefusedWhereTheModeOrThePolicyDoesNotGiveIt(t *testing.T) {
	t.Run("structured_only does not offer it, and a call to it is refused whole", func(t *testing.T) {
		fake := harnesstest.NewFake()
		r, _ := toolRig(t, fake, toolConfig{process: true})
		marker := filepath.Join(r.layout.Work, "marker.log")
		// The model asks for it again on the one retry F31 allows: refused whole both times.
		fake.SetReply(calls(tc("c1", "process.exec", execArgs([]string{"append", marker}, "helper", 5))))
		info := r.openMode("so.open.0000000000000001", protocol.ModeStructuredOnly)
		res := r.waitTerminal(r.startRun(info.ThreadID, "so.start.000000000000001", "go").RunID).Result
		if res.Status != "failed" || res.Code != "MODEL_OUTPUT_INVALID" || len(fake.Generates()) != 2 || lines(marker) != 0 || r.count("tool_links") != 0 {
			t.Fatalf("%+v", res)
		}
		for _, d := range fake.Generates()[0].Tools {
			if d.Function.Name == "process.exec" {
				t.Fatal("structured_only offered process.exec")
			}
		}
		if len(fake.Generates()[0].Tools) != 5 {
			t.Fatalf("%d tools", len(fake.Generates()[0].Tools))
		}
	})

	t.Run("no profile covers the request", func(t *testing.T) {
		fake := harnesstest.NewFake()
		r, _ := toolRig(t, fake, toolConfig{process: true})
		marker := filepath.Join(r.layout.Work, "marker.log")
		exe := execArgs([]string{"append", marker}, "helper", 5)
		wrongPrefix := strings.Replace(exe, "-test.run=TestHelperProcess", "-test.run=SomethingElse", 1)
		badEnv := strings.Replace(exe, `"helper"`, `"admin"`, 1)
		badCwd := strings.Replace(exe, `"cwd":"."`, `"cwd":"../outside"`, 1)
		fake.SetScript(calls(tc("c1", "process.exec", wrongPrefix)), calls(tc("c2", "process.exec", badEnv)), calls(tc("c3", "process.exec", badCwd)), harnesstest.Final("gave up"))
		info := r.openSession("np.open.0000000000000001")
		res := r.waitTerminal(r.startRun(info.ThreadID, "np.start.000000000000001", "go").RunID).Result
		if res.Status != "completed" || lines(marker) != 0 {
			t.Fatalf("%+v", res)
		}
		var codes []string
		for _, g := range fake.Generates()[1:] {
			v := toolMessages(t, g)
			last := v[len(v)-1]
			if last.EffectState != "not_started" || last.Error == nil {
				t.Fatalf("%+v", last)
			}
			codes = append(codes, last.Error.Code)
		}
		if strings.Join(codes, ",") != "POLICY_REJECTED,ENV_PROFILE_UNKNOWN,PATH_INVALID" {
			t.Fatalf("%v", codes)
		}
		if r.val("SELECT COUNT(*) FROM actions WHERE kind='tool' AND status='rejected'") != "3" {
			t.Fatal("a refusal is not recorded as one")
		}
	})

	t.Run("two profiles cover the request", func(t *testing.T) {
		fake := harnesstest.NewFake()
		r, _ := toolRig(t, fake, toolConfig{process: true, twin: true})
		marker := filepath.Join(r.layout.Work, "marker.log")
		fake.SetScript(calls(tc("c1", "process.exec", execArgs([]string{"append", marker}, "helper", 5))), harnesstest.Final("never"))
		info := r.openSession("amb.open.000000000000001")
		run := r.waitTerminal(r.startRun(info.ThreadID, "amb.start.00000000000001", "go").RunID)
		res := run.Result
		if res.Status != "blocked" || res.Code != "POLICY_AMBIGUOUS" || !res.Resumable || len(res.UnresolvedActionIDs) != 0 || lines(marker) != 0 || len(fake.Generates()) != 1 {
			t.Fatalf("%+v", res)
		}
		// The refusal is recorded, answered in the context, and nothing was started.
		if r.val("SELECT status FROM actions WHERE kind='tool'") != "rejected" || run.ContextRevision != 3 {
			t.Fatalf("%s %+v", r.val("SELECT status FROM actions WHERE kind='tool'"), run)
		}
	})
}

// TestProcessOutputIsBoundedAndALongRunIsStopped: past the Run's capture limit or the
// timeout the process tree is stopped and the answer says what was lost.
func TestProcessOutputIsBoundedAndALongRunIsStopped(t *testing.T) {
	t.Run("the capture limit", func(t *testing.T) {
		fake := harnesstest.NewFake()
		r, _ := toolRig(t, fake, toolConfig{process: true})
		fake.SetScript(calls(tc("c1", "process.exec", execArgs([]string{"big", "200000"}, "helper", 30))), harnesstest.Final("noted"))
		info := r.openSession("cap.open.000000000000001")
		begin := time.Now()
		run := r.waitTerminal(r.startRun(info.ThreadID, "cap.start.00000000000001", "go", func(in *protocol.StartInput) { in.Limits.MaxCaptureBytes = 2048 }).RunID)
		if run.Result.Status != "completed" || time.Since(begin) > 15*time.Second {
			t.Fatalf("%+v after %v", run.Result, time.Since(begin))
		}
		v := toolMessages(t, fake.Generates()[1])[0]
		out := resultOf(t, v)["stdout"].(map[string]any)
		if v.EffectState != "failed" || v.Error == nil || v.Error.Code != "CAPTURE_LIMIT" || v.CaptureComplete || v.ExitCode != nil || out["capture_complete"] != false || out["total_bytes"] != float64(2048) {
			t.Fatalf("%+v %v", v, out)
		}
		id := out["evidence_id"].(string)
		if got := r.val("SELECT state||'|'||capture_complete||'|'||total_bytes FROM evidence WHERE evidence_id=?", id); got != "sealed|0|2048" {
			t.Fatalf("an incomplete capture is sealed as one: %s", got)
		}
		// The event says the capture is incomplete, and no full completion is claimed.
		for _, e := range ofType(r.threadEvents(info.ThreadID), "action.completed") {
			p := payloadOf[protocol.ActionCompletedPayload](t, e)
			if p.EffectState == "failed" && (p.CaptureComplete || p.ExitCode != nil) {
				t.Fatalf("%+v", p)
			}
		}
		// Reading it back through the evidence method says partial.
		res := decode[protocol.EvidenceReadResult](t, r.mustCall("evidence/read", protocol.EvidenceReadInput{EvidenceID: id, ProjectionVersion: "raw/v1", Range: protocol.ByteRange{Start: 0, End: 10}}))
		if !res.Partial || res.CaptureComplete {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("the timeout", func(t *testing.T) {
		fake := harnesstest.NewFake()
		r, _ := toolRig(t, fake, toolConfig{process: true})
		fake.SetScript(calls(tc("c1", "process.exec", execArgs([]string{"sleep"}, "helper", 1))), harnesstest.Final("noted"))
		info := r.openSession("to.open.00000000000000001")
		begin := time.Now()
		run := r.waitTerminal(r.startRun(info.ThreadID, "to.start.0000000000000001", "go").RunID)
		if run.Result.Status != "completed" || time.Since(begin) > 20*time.Second {
			t.Fatalf("%+v after %v", run.Result, time.Since(begin))
		}
		v := toolMessages(t, fake.Generates()[1])[0]
		if v.EffectState != "failed" || v.Error == nil || v.Error.Code != "TIMEOUT" || v.ExitCode != nil || resultOf(t, v)["timed_out"] != true || resultOf(t, v)["signaled"] != true {
			t.Fatalf("%+v", v)
		}
	})
}

// TestAToolLoopIsBoundedByTheStepBudget: a model that keeps calling Tools is stopped by
// the Run's model steps, the exchange of the last step is applied, and the Run is
// resumable.
func TestAToolLoopIsBoundedByTheStepBudget(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(calls(tc("c1", "file.read", readArgs("demo.txt", 0, 10, 10))))
	r, _ := toolRig(t, fake, toolConfig{})
	r.write("demo.txt", "x")
	info := r.openSession("loop.open.0000000000001")
	run := r.waitTerminal(r.startRun(info.ThreadID, "loop.start.000000000001", "go", func(in *protocol.StartInput) { in.Limits.MaxModelSteps = 2 }).RunID)
	res := run.Result
	if res.Status != "incomplete" || res.Code != "STEP_BUDGET_EXHAUSTED" || !res.Resumable || len(fake.Generates()) != 2 || run.GenerationAttemptsUsed != 2 {
		t.Fatalf("%+v", res)
	}
	// Both steps' exchanges are in the context: the input and two (call, answer) pairs.
	if run.ContextRevision != 5 || r.val("SELECT COUNT(*) FROM actions WHERE kind='tool' AND status='completed'") != "2" || r.count("context_entries") != 5 {
		t.Fatalf("%+v", run)
	}
	// The same call ID in two responses is two calls: the key includes the response.
	if r.val("SELECT COUNT(DISTINCT model_response_id) FROM tool_links") != "2" || r.val("SELECT COUNT(*) FROM tool_links WHERE provider_tool_call_id='c1'") != "2" {
		t.Fatal("a call ID that two responses share was merged")
	}
}

// TestMoreCallsThanTheRunAllowsIsAStreamThatBrokeItsContract: the stream assembler holds
// a response to the Run's calls per step, as it always did; a response that has more is
// a violation (its generation is not known to have ended), and none of its calls runs.
func TestMoreCallsThanTheRunAllowsIsAStreamThatBrokeItsContract(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(calls(tc("c1", "file.read", readArgs("a", 0, 1, 1)), tc("c2", "file.read", readArgs("b", 0, 1, 1)), tc("c3", "file.read", readArgs("c", 0, 1, 1))))
	r, _ := toolRig(t, fake, toolConfig{})
	info := r.openSession("many.open.0000000000001")
	res := r.waitTerminal(r.startRun(info.ThreadID, "many.start.000000000001", "go", func(in *protocol.StartInput) { in.Limits.MaxToolCallsPerStep = 2 }).RunID).Result
	if res.Status != "blocked" || res.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" || len(fake.Generates()) != 1 || r.count("tool_links") != 0 || r.val("SELECT COUNT(*) FROM actions WHERE kind='tool'") != "0" {
		t.Fatalf("%+v", res)
	}
}
