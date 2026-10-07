package main_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

// qaSummary is a Summary of a question and its answer: it cites the answer (the one piece of Work).
const qaSummary = `{"current_work":[{"text":"質問に答えた。","source_handles":["work-0"]}],"decisions":[],"verification":[],"open_items":[],"next_steps":[],"important_observation_handles":[]}`

// TestTheCompactCommandCompactsAThreadOverTheRealBinaryAndTheStrictWire: a thread made by a
// `serve` process, then `compact` run as a command of its own, against the Gateway double. The
// dry run answers without a checkpoint; the compaction makes one (exit 0, the CompactResult
// as one canonical JSON object on standard output, a line for a person on standard error);
// the same key is not a second compaction; a thread that does not exist or a command line that
// is wrong fails without being mistaken for a Run's outcome.
func TestTheCompactCommandCompactsAThreadOverTheRealBinaryAndTheStrictWire(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Reply{Dynamic: func(req modelport.ChatRequest) harnesstest.Reply {
		if req.Rencrow.Harness.Stage != modelport.StageAct {
			return harnesstest.Final(qaSummary)
		}
		// A long answer: a compaction that keeps less than it replaces is one that shrinks.
		return harnesstest.Final(strings.Repeat("答えました。", 1200))
	}})
	f := newGatewayFixture(t, fake, false)
	s := startServe(t, f.l.Config)
	thread, runID := f.startRun(t, s, "cli.compact.0000000001", "質問です。")
	s.untilTerminal(runID)
	if res := s.mustCall("run/get", map[string]any{"run_id": runID}).result()["result"].(map[string]any); res["status"] != "completed" {
		t.Fatalf("%v", res)
	}
	s.mustCall("service/shutdown", map[string]any{"mode": "drain", "deadline_seconds": 1})
	if err := s.wait(); err != nil {
		t.Fatal(err)
	}

	decode := func(out string) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
			t.Fatalf("standard output is one JSON object: %q: %v", out, err)
		}
		return m
	}
	// The dry run: counted, nothing made.
	code, out, errs := runCLI(t, "compact", "--config", f.l.Config, "--thread", thread, "--dry-run")
	if code != 0 || !strings.Contains(errs, "compaction: dry_run") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	dry := decode(out)
	if dry["status"] != "dry_run" || dry["outcome"] != nil || dry["checkpoint_id"] != nil || dry["before"] == nil || dry["after"] != nil {
		t.Fatalf("%v", dry)
	}
	if got := len(fake.Generates()); got != 1 {
		t.Fatalf("a dry run generated: %d generations", got)
	}

	// The compaction: exit 0, a checkpoint named, the thread stands on it.
	code, out, errs = runCLI(t, "compact", "--config", f.l.Config, "--thread", thread, "--idempotency-key", "cli.compact.key.00000001")
	if code != 0 || !strings.Contains(errs, "compaction: executed NormalCompacted") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	res := decode(out)
	cp, _ := res["checkpoint_id"].(string)
	if res["status"] != "executed" || res["outcome"] != "NormalCompacted" || !strings.HasPrefix(cp, "ckp_") || res["error"] != nil || res["before"] == nil || res["after"] == nil {
		t.Fatalf("%v", res)
	}
	if code, out, _ := runCLI(t, "sessions", "list", "--config", f.l.Config); code != 0 || !strings.Contains(out, thread) {
		t.Fatalf("%d %q", code, out)
	}
	if got := len(fake.Generates()); got != 2 {
		t.Fatalf("one Summary generation: %d generations", got)
	}

	// The same key again, with the thread moved by the compaction itself, is not a second
	// compaction: the request is another request (the revisions it is made at are the thread's now),
	// and it is refused for it, not run.
	code, out, errs = runCLI(t, "compact", "--config", f.l.Config, "--thread", thread, "--idempotency-key", "cli.compact.key.00000001")
	if code != 1 || out != "" || !strings.Contains(errs, "IDEMPOTENCY_CONFLICT") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if got := len(fake.Generates()); got != 2 {
		t.Fatalf("a refused request generated: %d", got)
	}

	// What is not a compaction: a thread that does not exist (an operation failure, never a Run
	// outcome code), a thread id that is not one, no thread at all.
	for name, args := range map[string]struct {
		args []string
		code int
	}{
		"a thread that does not exist": {[]string{"compact", "--config", f.l.Config, "--thread", "thr_00000000-0000-7000-8000-000000000001"}, 1},
		"not a thread id":              {[]string{"compact", "--config", f.l.Config, "--thread", "x"}, 64},
		"no thread":                    {[]string{"compact", "--config", f.l.Config}, 64},
	} {
		code, out, errs := runCLI(t, args.args...)
		if code != args.code || out != "" || errs == "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, out, errs)
		}
	}
}
