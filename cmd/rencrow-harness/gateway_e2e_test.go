package main_test

import (
	"encoding/json"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The production composition end to end: the real binary, the real RenCrow_LLM client
// and the strict wire over loopback HTTP, against a Gateway double that serves the fake
// model in the Gateway's own forms. Nothing here is a port the process was handed: the
// process builds its client from the configuration, as `serve --stdio` does.

// untilTerminal reads notifications until the run.terminal event of the Run and returns
// every notification up to and including it, in the order they arrived.
func (s *server) untilTerminal(runID string) []map[string]any {
	s.t.Helper()
	var out []map[string]any
	for {
		m := s.readLine()
		out = append(out, m)
		if m["method"] != "event/recorded" {
			continue
		}
		ev, ok := m["params"].(map[string]any)
		if ok && ev["type"] == protocol.EventRunTerminal && ev["run_id"] == runID {
			return out
		}
	}
}

// names is the method of each notification, and for an event its type too.
func names(notes []map[string]any) []string {
	var out []string
	for _, m := range notes {
		name, _ := m["method"].(string)
		if p, ok := m["params"].(map[string]any); ok && name == "event/recorded" {
			name += ":" + p["type"].(string)
		}
		out = append(out, name)
	}
	return out
}

func (f *fixture) startRun(t *testing.T, s *server, key, text string) (thread, runID string) {
	t.Helper()
	f.initialize(s)
	open := s.mustCall("session/open", f.openParams(key+".open"))
	thread = open.result()["session"].(map[string]any)["thread_id"].(string)
	s.events(1)
	start := s.mustCall("turn/start", startParams(t, thread, key+".start", text))
	return thread, start.result()["run_id"].(string)
}

func TestARunIsRetriedOverTheRealClientAndTheStrictWire(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.OfferTerminalOutputOnce = true
	fake.SetScript(
		harnesstest.Reply{Kind: harnesstest.KindErrorOutcome, Code: "RAW_TOOL_MARKUP", Text: "<tool_call>broken"},
		harnesstest.Final("fixed"))
	f := newGatewayFixture(t, fake, false)
	s := startServe(t, f.l.Config)
	_, runID := f.startRun(t, s, "wire.retry.0000000001", "fix it")
	notes := s.untilTerminal(runID)

	// The Run is executed to its end, and the retry shows on the wire in its order: the
	// failed Attempt's text, the discard of it, the retry's own text.
	run := s.mustCall("run/get", map[string]any{"run_id": runID}).result()
	res := run["result"].(map[string]any)
	if run["terminal"] != true || res["status"] != "completed" || res["final_text"] != "fixed" || run["generation_attempts_used"].(json.Number).String() != "2" {
		t.Fatalf("%v", run)
	}
	var kinds []string
	for _, n := range names(notes) {
		switch n {
		case "progress/delta", "progress/reset", "event/recorded:model.retry_scheduled", "event/recorded:model.attempt_started", "event/recorded:run.terminal":
			kinds = append(kinds, n)
		}
	}
	want := []string{"event/recorded:model.attempt_started", "progress/delta", "event/recorded:model.retry_scheduled", "event/recorded:model.attempt_started", "progress/reset", "progress/delta", "event/recorded:run.terminal"}
	if len(kinds) != len(want) {
		t.Fatalf("%v", names(notes))
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("%v", names(notes))
		}
	}
	var reset map[string]any
	for _, m := range notes {
		if m["method"] == "progress/reset" {
			reset = m["params"].(map[string]any)
		}
	}
	if reset["run_id"] != runID || reset["reason"] != "RAW_TOOL_MARKUP" || reset["provisional"] != true || reset["old_attempt_id"] == reset["new_attempt_id"] {
		t.Fatalf("%v", reset)
	}
	// What the Gateway saw: the binding described once, each Attempt counted and then
	// generated, nothing more.
	if f.gw.Hits("/status") != 1 || f.gw.Hits("/context/measure") != 2 || f.gw.Hits("/chat/completions") != 2 {
		t.Fatalf("status=%d measure=%d generate=%d", f.gw.Hits("/status"), f.gw.Hits("/context/measure"), f.gw.Hits("/chat/completions"))
	}
	if g := fake.Generates(); len(g) != 2 || g[1].Rencrow.Harness.Recovery.ProfileID != "terminal_output_once" || g[1].Rencrow.Harness.Recovery.RetryOfRequestID == nil {
		t.Fatalf("the retry did not reach the Gateway as a terminal_output_once retry")
	}
	_ = s.mustCall("service/shutdown", map[string]any{"mode": "drain", "deadline_seconds": 1})
	if err := s.wait(); err != nil {
		t.Fatalf("exit: %v\nstderr: %s", err, s.stderr.String())
	}
}

func TestAnUnknownGenerationOverTheWireIsBlockedAndNotRetried(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(harnesstest.Reply{Kind: harnesstest.KindTransportError}, harnesstest.Final("never asked for"))
	f := newGatewayFixture(t, fake, false)
	s := startServe(t, f.l.Config)
	_, runID := f.startRun(t, s, "wire.unknown.000000001", "go")
	s.untilTerminal(runID)
	run := s.mustCall("run/get", map[string]any{"run_id": runID}).result()
	res := run["result"].(map[string]any)
	if res["status"] != "blocked" || res["code"] != "MODEL_GENERATION_OUTCOME_UNKNOWN" || len(res["unresolved_action_ids"].([]any)) != 1 ||
		run["generation_attempts_used"].(json.Number).String() != "1" || run["generation_attempts_unknown"].(json.Number).String() != "1" {
		t.Fatalf("%v", run)
	}
	if f.gw.Hits("/chat/completions") != 1 || f.gw.Hits("/context/measure") != 1 {
		t.Fatalf("measure=%d generate=%d: an unknown generation was sent again", f.gw.Hits("/context/measure"), f.gw.Hits("/chat/completions"))
	}
	_ = s.mustCall("service/shutdown", map[string]any{"mode": "drain", "deadline_seconds": 1})
	_ = s.wait()
}

// TestAGatewayThatIsDownEndsTheRunBlockedAndNotTheProcess: the process starts without
// contacting the Gateway (its capabilities say what it can do, not that the Gateway is
// up), and the Run that needs the Gateway ends blocked, with nothing generated.
func TestAGatewayThatIsDownEndsTheRunBlockedAndNotTheProcess(t *testing.T) {
	f := newFixture(t) // the configuration names a loopback port nothing listens on
	s := startServe(t, f.l.Config)
	f.initialize(s)
	caps := s.mustCall("service/capabilities", map[string]any{})
	status := map[string]string{}
	for _, c := range caps.result()["capabilities"].([]any) {
		c := c.(map[string]any)
		status[c["name"].(string)] = c["status"].(string)
	}
	if status["turn.execution"] != "ready" || status["model.generation"] != "ready" || status["context/compact"] != "ready" || status["session/fork"] != "ready" {
		t.Fatalf("%v", status)
	}
	open := s.mustCall("session/open", f.openParams("down.open.000000000001"))
	thread := open.result()["session"].(map[string]any)["thread_id"].(string)
	s.events(1)
	start := s.mustCall("turn/start", startParams(t, thread, "down.start.00000000001", "go"))
	runID := start.result()["run_id"].(string)
	s.untilTerminal(runID)
	res := s.mustCall("run/get", map[string]any{"run_id": runID}).result()["result"].(map[string]any)
	if res["status"] != "blocked" || res["code"] != "MODEL_UNAVAILABLE" || res["resumable"] != true || len(res["unresolved_action_ids"].([]any)) != 0 {
		t.Fatalf("%v", res)
	}
	// The process is still serving.
	if again := s.mustCall("session/get", map[string]any{"thread_id": thread}).result(); again["active_run_id"] != nil {
		t.Fatalf("%v", again)
	}
	_ = s.mustCall("service/shutdown", map[string]any{"mode": "drain", "deadline_seconds": 1})
	_ = s.wait()
}
