package main_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

// TestAPromptThatDoesNotFitIsCompactedOverTheRealClientAndTheStrictWire is the production
// composition for a compaction: the real binary builds its client from the configuration, a
// Tool answer makes the next prompt too large, and the Selection-free Normal compaction runs its
// Summary stage as the strict non-stream request over HTTP (no Tool, one JSON object, the
// stage's own recovery and options), the checkpoint is committed, and the Run is generated
// again from the checkpoint and completes. The Gateway sees each request once.
func TestAPromptThatDoesNotFitIsCompactedOverTheRealClientAndTheStrictWire(t *testing.T) {
	const file = 60000
	fake := harnesstest.NewFake()
	// usable = 8,000 tokens: the limit less the 4,096 output reserve and the 2,048 margin.
	fake.SetMeasure(harnesstest.MeasureConfig{Limit: 8000 + 4096 + 2048})
	summary := `{"current_work":[{"text":"read the big file","source_handles":["work-0","observation-0"]}],"decisions":[],"verification":[],"open_items":[{"text":"write the code","source_handles":["instruction-0"]}],"next_steps":[],"important_observation_handles":["observation-0"]}`
	var stages []modelport.ChatRequest
	fake.SetReply(harnesstest.Reply{Dynamic: func(req modelport.ChatRequest) harnesstest.Reply {
		if st := req.Rencrow.Harness.Stage; st != modelport.StageAct {
			stages = append(stages, req)
			return harnesstest.Final(summary)
		}
		for _, m := range req.Messages {
			if m.Role == "tool" || strings.HasPrefix(m.Text(), "RENCROW_CONTEXT_BOUNDARY_V1") {
				return harnesstest.Final("carried on")
			}
		}
		args, _ := json.Marshal(map[string]any{"path": "big.txt", "range": map[string]any{"start": 0, "end": file}, "max_bytes": file})
		return harnesstest.Reply{Kind: harnesstest.KindToolCall, Calls: []harnesstest.Call{{ID: "call-1", Name: "file.read", Args: string(args)}}}
	}})
	f := newGatewayFixture(t, fake, false)
	if err := os.WriteFile(filepath.Join(f.l.Work, "big.txt"), []byte(strings.Repeat("0123456789abcdef\n", file/17)), 0o644); err != nil {
		t.Fatal(err)
	}
	s := startServe(t, f.l.Config)
	_, runID := f.startRun(t, s, "wire.compact.00000001", "read big.txt and write the code")
	notes := s.untilTerminal(runID)

	run := s.mustCall("run/get", map[string]any{"run_id": runID}).result()
	res := run["result"].(map[string]any)
	if res["status"] != "completed" || res["final_text"] != "carried on" || res["last_checkpoint_id"] == nil {
		t.Fatalf("%v", res)
	}
	var kinds []string
	for _, n := range names(notes) {
		if strings.HasPrefix(n, "event/recorded:") && (strings.Contains(n, "checkpoint") || strings.Contains(n, "model.requested")) {
			kinds = append(kinds, n)
		}
	}
	want := []string{"event/recorded:model.requested", "event/recorded:model.requested", "event/recorded:checkpoint.committed", "event/recorded:model.requested"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("%v", names(notes))
	}
	// What the Gateway saw: every prompt counted before it is sent, and three generations: the
	// act that asked for the Tool, the Summary stage, and the act from the checkpoint.
	if f.gw.Hits("/chat/completions") != 3 || len(stages) != 1 {
		t.Fatalf("generations %d, stage requests %d", f.gw.Hits("/chat/completions"), len(stages))
	}
	sr := stages[0]
	if sr.Stream || sr.ToolChoice != "none" || len(sr.Tools) != 0 || sr.ResponseFormat.Type != "json_object" || sr.Rencrow.Harness.Stage != modelport.StageSummary ||
		sr.Rencrow.Harness.Recovery.ProfileID != "same_request" || sr.Rencrow.Harness.MaxBackendAttempts != 1 || len(sr.Messages) != 2 {
		t.Fatalf("%+v", sr)
	}
	if f.gw.Hits("/context/measure") != 6 {
		t.Fatalf("counts %d: the first prompt, the one that does not fit, the smallest candidate (Preflight), the Summary request, the candidate, the prompt that is sent", f.gw.Hits("/context/measure"))
	}
	_ = s.mustCall("service/shutdown", map[string]any{"mode": "drain", "deadline_seconds": 1})
	if err := s.wait(); err != nil {
		t.Fatalf("exit: %v\nstderr: %s", err, s.stderr.String())
	}
}
