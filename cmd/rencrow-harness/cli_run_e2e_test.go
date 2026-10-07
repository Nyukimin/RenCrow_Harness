package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

// The commands that drive a Run from the command line (exec, resume, chat), as the real
// binary, over the real client and the strict wire, against the Gateway double.

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// cliProc is one running command with its pipes.
type cliProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *syncBuf
	stderr *syncBuf
	done   chan error
}

func startCLI(t *testing.T, args ...string) *cliProc {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	cmd.Dir = t.TempDir()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	p := &cliProc{t: t, cmd: cmd, stdin: stdin, stdout: &syncBuf{}, stderr: &syncBuf{}, done: make(chan error, 1)}
	cmd.Stdout, cmd.Stderr = p.stdout, p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return p
}

func (p *cliProc) send(s string) {
	p.t.Helper()
	if _, err := io.WriteString(p.stdin, s); err != nil {
		p.t.Fatalf("write to the command: %v\nstderr: %s", err, p.stderr.String())
	}
}

func (p *cliProc) signal(sig os.Signal) {
	p.t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		p.t.Fatal(err)
	}
}

// until waits for a buffer to match the pattern and returns the first match.
func (p *cliProc) until(buf *syncBuf, pattern string) []string {
	p.t.Helper()
	re := regexp.MustCompile(pattern)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if m := re.FindStringSubmatch(buf.String()); m != nil {
			return m
		}
		select {
		case err := <-p.done:
			p.done <- err
			if m := re.FindStringSubmatch(buf.String()); m != nil {
				return m
			}
			p.t.Fatalf("the command ended (%v) before %q appeared\nstdout: %s\nstderr: %s", err, pattern, p.stdout.String(), p.stderr.String())
		case <-time.After(15 * time.Millisecond):
		}
	}
	p.t.Fatalf("%q did not appear\nstdout: %s\nstderr: %s", pattern, p.stdout.String(), p.stderr.String())
	return nil
}

func (p *cliProc) wait() int {
	p.t.Helper()
	select {
	case err := <-p.done:
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		if err != nil {
			p.t.Fatal(err)
		}
		return 0
	case <-time.After(60 * time.Second):
		p.t.Fatalf("the command did not end\nstdout: %s\nstderr: %s", p.stdout.String(), p.stderr.String())
	}
	return -1
}

// jsonLines decodes standard output as JSON lines, and fails if a line is not one.
func jsonLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if l == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(l))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("standard output has a line that is not JSON: %q (%v)", l, err)
		}
		lines = append(lines, m)
	}
	return lines
}

// checkStream holds the JSON lines of an exec to their promise: events and one closing result,
// nothing else, the events in the order they were committed. It returns the events and the result.
func checkStream(t *testing.T, out string) (events []map[string]any, result map[string]any) {
	t.Helper()
	lines := jsonLines(t, out)
	if len(lines) < 2 {
		t.Fatalf("%d lines: %q", len(lines), out)
	}
	last := int64(0)
	for i, l := range lines {
		switch l["type"] {
		case "event":
			ev := l["event"].(map[string]any)
			seq, _ := ev["event_seq"].(json.Number).Int64()
			if seq <= last {
				t.Fatalf("events are not in the order they were committed: %d after %d", seq, last)
			}
			last = seq
			events = append(events, ev)
		case "result":
			if i != len(lines)-1 {
				t.Fatalf("the result is not the last line")
			}
			result = l["result"].(map[string]any)
		default:
			t.Fatalf("a line that is neither an event nor the result: %v", l)
		}
		if len(l) != 2 {
			t.Fatalf("a line with more than its type and its value: %v", l)
		}
	}
	if result == nil {
		t.Fatalf("no result line: %q", out)
	}
	return events, result
}

func eventTypes(events []map[string]any) []string {
	var out []string
	for _, e := range events {
		out = append(out, e["type"].(string))
	}
	return out
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func execArgsOf(f *fixture, input string, extra ...string) []string {
	return append([]string{"exec", "--config", f.l.Config, "--workspace", f.l.Work, "--binding", "fixture-local", "--input-file", input}, extra...)
}

// holdFirstMeasure holds the first count the Gateway is asked for until its context ends (the
// Run stays in Measuring until it is stopped), and tells held. Later counts are answered.
func holdFirstMeasure(fake *harnesstest.Fake) <-chan struct{} {
	held := make(chan struct{}, 4)
	var n atomic.Int32
	fake.OnMeasure = func(ctx context.Context) {
		if n.Add(1) == 1 {
			held <- struct{}{}
			<-ctx.Done()
		}
	}
	return held
}

func waitHeld(t *testing.T, held <-chan struct{}) {
	t.Helper()
	select {
	case <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("the Run did not reach the Gateway")
	}
}

// TestExecDrivesARunToItsEndAndTheExitCodeSaysHow: one Run per outcome the model side can give
// without help, each with its PROTOCOL exit code, and each with a stream on standard output
// that is JSON lines and nothing else.
func TestExecDrivesARunToItsEndAndTheExitCodeSaysHow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  harnesstest.Reply
		code   int
		status string
		why    string
	}{
		{"completed", harnesstest.Final("all done"), 0, "completed", "FINAL_RESPONSE_ACCEPTED"},
		{"incomplete", harnesstest.Reply{Kind: harnesstest.KindLength, Text: "cut off"}, 2, "incomplete", "MODEL_OUTPUT_TRUNCATED"},
		{"rejected", harnesstest.Reply{Kind: harnesstest.KindRefusal, Text: "no"}, 3, "rejected", "MODEL_REFUSED"},
		{"failed", harnesstest.Reply{Kind: harnesstest.KindReasoningOnly, Reasoning: "thinking"}, 5, "failed", "MODEL_OUTPUT_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			fake.SetReply(tc.reply)
			f := newGatewayFixture(t, fake, false)
			input := writeFile(t, f.l.Dir, "in.txt", "please do the thing\n")
			code, out, errs := runCLI(t, execArgsOf(f, input, "--json")...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, out, errs)
			}
			events, res := checkStream(t, out)
			if res["status"] != tc.status || res["code"] != tc.why {
				t.Fatalf("%v", res)
			}
			types := eventTypes(events)
			if types[0] != "session.created" || types[len(types)-1] != "run.terminal" {
				t.Fatalf("%v", types)
			}
			for _, want := range []string{"input.accepted", "task.created", "run.started", "input.applied", "model.requested"} {
				found := false
				for _, ty := range types {
					found = found || ty == want
				}
				if !found {
					t.Errorf("no %s event in %v", want, types)
				}
			}
			if tc.code == 0 && res["final_text"] != "all done" {
				t.Fatalf("%v", res)
			}
			if !strings.Contains(errs, tc.status) {
				t.Fatalf("stderr does not say how the Run ended: %s", errs)
			}
		})
	}

	// A Gateway that is down: the Run is blocked, with nothing generated.
	t.Run("blocked", func(t *testing.T) {
		f := newFixture(t)
		input := writeFile(t, f.l.Dir, "in.txt", "please\n")
		code, out, errs := runCLI(t, execArgsOf(f, input, "--json")...)
		if code != 3 {
			t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errs)
		}
		if _, res := checkStream(t, out); res["status"] != "blocked" || res["code"] != "MODEL_UNAVAILABLE" || res["resumable"] != true {
			t.Fatalf("%v", res)
		}
	})
}

func TestExecWithoutJSONWritesTheFinalTextAndNothingElseToStandardOutput(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Final("the final answer"))
	f := newGatewayFixture(t, fake, false)
	input := writeFile(t, f.l.Dir, "in.txt", "question\n")
	code, out, errs := runCLI(t, execArgsOf(f, input)...)
	if code != 0 || out != "the final answer\n" {
		t.Fatalf("exit %d\nstdout: %q\nstderr: %s", code, out, errs)
	}
	if !strings.Contains(errs, "completed FINAL_RESPONSE_ACCEPTED") || !strings.Contains(errs, "verification: not_run") {
		t.Fatalf("%s", errs)
	}
}

// TestExecMakesItsInputAutomationUnlessTheOperatorDeclaresOtherwise: the origin of the input is
// the entrypoint's and the operator's declaration (capped by the profile), never a guess from
// the terminal.
func TestExecMakesItsInputAutomationUnlessTheOperatorDeclaresOtherwise(t *testing.T) {
	for _, tc := range []struct {
		name             string
		extra            []string
		declared, actual string
		basis            string
	}{
		{"nothing declared", nil, "automation", "automation", "automation"},
		{"human declared", []string{"--origin", "human"}, "human", "human", "declared_local"},
		{"unknown declared", []string{"--origin", "unknown"}, "unknown", "unknown", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGatewayFixture(t, harnesstest.NewFake(), false)
			input := writeFile(t, f.l.Dir, "in.txt", "go\n")
			code, out, errs := runCLI(t, execArgsOf(f, input, append([]string{"--json"}, tc.extra...)...)...)
			if code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errs)
			}
			events, _ := checkStream(t, out)
			var intake map[string]any
			for _, e := range events {
				if e["type"] == "input.accepted" {
					intake = e["payload"].(map[string]any)["intake"].(map[string]any)
				}
			}
			if intake["entrypoint"] != "cli_exec" || intake["declared_origin"] != tc.declared || intake["effective_origin"] != tc.actual || intake["proof_basis"] != tc.basis {
				t.Fatalf("%v", intake)
			}
		})
	}
}

// TestExecRunTwiceWithOneKeyIsOneRun: the same key and the same input is the first Run's
// result, with nothing generated again.
func TestExecRunTwiceWithOneKeyIsOneRun(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Final("once"))
	f := newGatewayFixture(t, fake, false)
	input := writeFile(t, f.l.Dir, "in.txt", "go\n")
	args := execArgsOf(f, input, "--json", "--idempotency-key", "e2e.exec.same.key.0001")
	code, out, errs := runCLI(t, args...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	_, first := checkStream(t, out)
	generated := f.gw.Hits("/chat/completions")
	code, out2, errs2 := runCLI(t, args...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out2, errs2)
	}
	lines := jsonLines(t, out2)
	again := lines[len(lines)-1]["result"].(map[string]any)
	if again["run_id"] != first["run_id"] || again["final_text"] != "once" || f.gw.Hits("/chat/completions") != generated {
		t.Fatalf("%v (generations %d -> %d)", again, generated, f.gw.Hits("/chat/completions"))
	}
}

// TestExecStoppedBySIGINTRecordsOneStopAndEndsWithHowTheRunEnded: Ctrl-C records the stop and
// waits; the Run ends cancelled; the stream still ends with the result and the exit code is 4.
func TestExecStoppedBySIGINTRecordsOneStopAndEndsWithHowTheRunEnded(t *testing.T) {
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			fake := harnesstest.NewFake()
			held := holdFirstMeasure(fake)
			f := newGatewayFixture(t, fake, false)
			input := writeFile(t, f.l.Dir, "in.txt", "go\n")
			p := startCLI(t, execArgsOf(f, input, "--json")...)
			waitHeld(t, held)
			p.signal(sig)
			if code := p.wait(); code != 4 {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, p.stdout.String(), p.stderr.String())
			}
			events, res := checkStream(t, p.stdout.String())
			if res["status"] != "cancelled" || res["code"] != "CANCELLED" || res["resumable"] != true {
				t.Fatalf("%v", res)
			}
			types := eventTypes(events)
			stops := 0
			for _, ty := range types {
				if ty == "control.cancel_requested" {
					stops++
				}
			}
			if stops != 1 || types[len(types)-1] != "run.terminal" {
				t.Fatalf("%v", types)
			}
			if !strings.Contains(p.stderr.String(), "stop recorded for the run") {
				t.Fatalf("%s", p.stderr.String())
			}
		})
	}
}

// TestResumeContinuesATaskThatWasStoppedAndASecondResumeIsTheFirst: the Task that exec left
// cancelled gets a new Run of the same Task, driven to its end by the same stream; the same key
// again is that Run; a Task that completed is not resumed.
func TestResumeContinuesATaskThatWasStoppedAndASecondResumeIsTheFirst(t *testing.T) {
	fake := harnesstest.NewFake()
	held := holdFirstMeasure(fake)
	fake.SetReply(harnesstest.Final("resumed and done"))
	f := newGatewayFixture(t, fake, false)
	input := writeFile(t, f.l.Dir, "in.txt", "go\n")
	firstArgs := execArgsOf(f, input, "--json", "--idempotency-key", "e2e.resume.first.key.0001")
	p := startCLI(t, firstArgs...)
	waitHeld(t, held)
	p.signal(os.Interrupt)
	if code := p.wait(); code != 4 {
		t.Fatalf("exit %d\n%s", code, p.stderr.String())
	}
	_, stopped := checkStream(t, p.stdout.String())
	taskID, runID := stopped["task_id"].(string), stopped["run_id"].(string)

	args := []string{"resume", "--config", f.l.Config, "--task", taskID, "--last-run", runID, "--json", "--idempotency-key", "e2e.resume.key.000001"}
	code, out, errs := runCLI(t, args...)
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	events, res := checkStream(t, out)
	if res["status"] != "completed" || res["final_text"] != "resumed and done" || res["task_id"] != taskID || res["run_id"] == runID {
		t.Fatalf("%v", res)
	}
	var started map[string]any
	for _, e := range events {
		if e["type"] == "run.started" {
			started = e["payload"].(map[string]any)
		}
	}
	if started == nil || started["previous_run_id"] != runID {
		t.Fatalf("%v", started)
	}
	generated := f.gw.Hits("/chat/completions")
	code, out2, errs2 := runCLI(t, args...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out2, errs2)
	}
	lines := jsonLines(t, out2)
	if again := lines[len(lines)-1]["result"].(map[string]any); again["run_id"] != res["run_id"] || f.gw.Hits("/chat/completions") != generated {
		t.Fatalf("the same key was not the same resume: %v", again)
	}

	// The first exec again, by its key, is the first Run's stream and result: the Run that was
	// resumed since is not part of it.
	code, again1, errs1 := runCLI(t, firstArgs...)
	if code != 4 {
		t.Fatalf("exit %d\n%s\n%s", code, again1, errs1)
	}
	replayEvents, replayed := checkStream(t, again1)
	if replayed["run_id"] != runID || replayed["status"] != "cancelled" {
		t.Fatalf("%v", replayed)
	}
	for _, ev := range replayEvents {
		if id, _ := ev["run_id"].(string); id != "" && id != runID {
			t.Fatalf("an event of another Run is in the stream: %v", ev)
		}
	}

	// The Task completed: there is nothing to resume, and no Run outcome is reported for it.
	code, out3, errs3 := runCLI(t, "resume", "--config", f.l.Config, "--task", taskID, "--last-run", res["run_id"].(string), "--json", "--idempotency-key", "e2e.resume.key.000002")
	if code == 0 || (code >= 2 && code <= 6) || out3 != "" {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out3, errs3)
	}
	// A last run that is not the Task's last Run is refused.
	code, out4, _ := runCLI(t, "resume", "--config", f.l.Config, "--task", taskID, "--last-run", runID, "--json", "--idempotency-key", "e2e.resume.key.000003")
	if code == 0 || (code >= 2 && code <= 6) || out4 != "" {
		t.Fatalf("exit %d\nstdout: %s", code, out4)
	}
}

func TestExecInATrustedWorkspaceGivesTheRunItsAgentsFileAndAnAssetOverALimitStopsItBeforeTheRun(t *testing.T) {
	fake := harnesstest.NewFake()
	f := newGatewayFixtureWith(t, fake, false, func(l *harnesstest.Layout) {
		l.Cfg["extensions"] = map[string]any{"enabled": true, "trusted_workspace_roots": []any{l.Work}, "skill_roots": []any{}, "hooks": []any{}}
	})
	writeFile(t, f.l.Work, "AGENTS.md", "Prefer small commits.\n")
	input := writeFile(t, f.l.Dir, "in.txt", "go\n")
	code, out, errs := runCLI(t, execArgsOf(f, input, "--json")...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	checkStream(t, out)
	gen := fake.Generates()
	if len(gen) != 1 {
		t.Fatalf("%d generations", len(gen))
	}
	seen := false
	for _, m := range gen[0].Messages {
		seen = seen || strings.Contains(m.Text(), "Prefer small commits.") && strings.Contains(m.Text(), "rencrow-host-agents/v1")
	}
	if !seen {
		t.Fatal("the AGENTS.md did not reach the model")
	}

	// Over 32 KiB: refused before the Run, with a code that is not a Run outcome, and a stream
	// that does not pretend there was a result.
	writeFile(t, f.l.Work, "AGENTS.md", strings.Repeat("x", 32*1024+1))
	code, out, errs = runCLI(t, execArgsOf(f, input, "--json")...)
	if code != 1 || !strings.Contains(errs, "CONTEXT_ASSET_TOO_LARGE") || strings.Contains(out, `"type":"result"`) {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	if len(fake.Generates()) != 1 {
		t.Fatal("a refused Run reached the model")
	}
}

// ---- chat ----

func chatArgs(f *fixture, extra ...string) []string {
	return append([]string{"chat", "--config", f.l.Config, "--workspace", f.l.Work, "--binding", "fixture-local"}, extra...)
}

func inspectStatus(t *testing.T, f *fixture, runID string) map[string]any {
	t.Helper()
	code, out, errs := runCLI(t, "inspect", "--config", f.l.Config, "--run", runID, "--json")
	if code != 0 {
		t.Fatalf("inspect: %d %s", code, errs)
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatal(err)
	}
	return info["result"].(map[string]any)
}

var runIDRE = `(run_[0-9a-f-]+)`

// TestChatStopAndCtrlCEndTheRunThatIsGoingOnAndTheChatGoesOn: /stop and SIGINT are turn/interrupt;
// the chat says the Run ended when it did and is ready for the next message; a Ctrl-C at the
// prompt leaves.
func TestChatStopAndCtrlCEndTheRunThatIsGoingOnAndTheChatGoesOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(p *cliProc)
	}{
		{"/stop", func(p *cliProc) { p.send("/stop\n") }},
		{"SIGINT", func(p *cliProc) { p.signal(os.Interrupt) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			fake.SetReply(harnesstest.Final("the second answer"))
			held := holdFirstMeasure(fake)
			f := newGatewayFixture(t, fake, false)
			p := startCLI(t, chatArgs(f)...)
			p.until(p.stderr, `chat: session ses_[0-9a-f-]+, thread thr_[0-9a-f-]+, mode structured_only`)
			p.send("hello\n")
			runID := p.until(p.stderr, `\[run `+runIDRE+` started\]`)[1]
			waitHeld(t, held)
			tc.stop(p)
			p.until(p.stderr, `stop recorded for the run \(CANCEL_REQUESTED\)`)
			p.until(p.stderr, `\[run `+runID+` ended cancelled CANCELLED`)
			res := inspectStatus(t, f, runID)
			if res["status"] != "cancelled" || res["code"] != "CANCELLED" {
				t.Fatalf("%v", res)
			}
			// The chat is ready for the next message.
			p.send("second\n")
			p.until(p.stdout, `the second answer`)
			p.until(p.stderr, `ended completed FINAL_RESPONSE_ACCEPTED`)
			p.send("/exit\n")
			if code := p.wait(); code != 0 {
				t.Fatalf("exit %d\n%s", code, p.stderr.String())
			}
		})
	}
	t.Run("SIGINT at the prompt leaves", func(t *testing.T) {
		f := newGatewayFixture(t, harnesstest.NewFake(), false)
		p := startCLI(t, chatArgs(f)...)
		p.until(p.stderr, `chat: session`)
		p.send("/status\n")
		p.until(p.stderr, `no run is active`)
		p.signal(os.Interrupt)
		if code := p.wait(); code != 0 {
			t.Fatalf("exit %d\n%s", code, p.stderr.String())
		}
	})
}

// TestChatInterruptHoldsItsTextForTheNextMessageAndLaterHoldsItWithoutStopping: /interrupt
// stops the Run and keeps the text for the next turn (the chat does not start the next Run on
// its own), /later keeps it without stopping; both are applied, in the order they were
// accepted, before the next message.
func TestChatInterruptHoldsItsTextForTheNextMessageAndLaterHoldsItWithoutStopping(t *testing.T) {
	for _, tc := range []struct {
		name, command, text, saysSoon string
		stopsRun                      bool
	}{
		{"/interrupt", "/interrupt", "change of plan: use the other file", `interrupt recorded: the run is stopping`, true},
		{"/later", "/later", "remember this for afterwards", `held for the next turn`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			fake.SetReply(harnesstest.Final("done after the hold"))
			held := holdFirstMeasure(fake)
			f := newGatewayFixture(t, fake, false)
			p := startCLI(t, chatArgs(f)...)
			p.until(p.stderr, `chat: session`)
			p.send("first request\n")
			runID := p.until(p.stderr, `\[run `+runIDRE+` started\]`)[1]
			waitHeld(t, held)
			p.send(tc.command + " " + tc.text + "\n")
			p.until(p.stderr, tc.saysSoon)
			if tc.stopsRun {
				p.until(p.stderr, `\[run `+runID+` ended cancelled CANCELLED`)
			} else {
				// The Run is still going on: it is stopped by the person, not by /later.
				p.send("/stop\n")
				p.until(p.stderr, `\[run `+runID+` ended cancelled CANCELLED`)
			}
			if len(fake.Generates()) != 0 {
				t.Fatal("a stopped Run generated")
			}
			p.send("the next message\n")
			p.until(p.stdout, `done after the hold`)
			p.until(p.stderr, `ended completed FINAL_RESPONSE_ACCEPTED`)
			p.send("/exit\n")
			if code := p.wait(); code != 0 {
				t.Fatalf("exit %d\n%s", code, p.stderr.String())
			}
			gen := fake.Generates()
			if len(gen) != 1 {
				t.Fatalf("%d generations", len(gen))
			}
			heldAt, nextAt := -1, -1
			for i, m := range gen[0].Messages {
				switch {
				case strings.Contains(m.Text(), tc.text):
					heldAt = i
				case strings.Contains(m.Text(), "the next message"):
					nextAt = i
				}
			}
			if heldAt < 0 || nextAt < 0 || heldAt > nextAt {
				t.Fatalf("the held text is not applied before the next message (%d, %d)", heldAt, nextAt)
			}
		})
	}
}

// TestChatAPlainLineDuringARunIsOneMoreInputForItsNextStep: the line is queued, applied at the
// start of the Run's next step, and shows in the next prompt; nothing is stopped.
func TestChatAPlainLineDuringARunIsOneMoreInputForItsNextStep(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(
		harnesstest.Reply{Kind: harnesstest.KindToolCall, Name: "file.read", Args: `{"path":"note.txt","range":{"start":0,"end":100},"max_bytes":100}`},
		harnesstest.Final("finished with the extra"))
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	var n atomic.Int32
	fake.OnGenerate = func(ctx context.Context, _ modelport.ChatRequest) {
		if n.Add(1) == 1 {
			reached <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	}
	f := newGatewayFixture(t, fake, false)
	writeFile(t, f.l.Work, "note.txt", "a note\n")
	p := startCLI(t, chatArgs(f)...)
	p.until(p.stderr, `chat: session`)
	p.send("read the note\n")
	select {
	case <-reached:
	case <-time.After(30 * time.Second):
		t.Fatal("the generation did not start")
	}
	p.send("and also mention the weather\n")
	p.until(p.stderr, `queued for the next step of the run`)
	close(release)
	p.until(p.stdout, `finished with the extra`)
	p.until(p.stderr, `ended completed FINAL_RESPONSE_ACCEPTED`)
	p.send("/exit\n")
	if code := p.wait(); code != 0 {
		t.Fatalf("exit %d\n%s", code, p.stderr.String())
	}
	gen := fake.Generates()
	if len(gen) != 2 {
		t.Fatalf("%d generations", len(gen))
	}
	seenInFirst, seenInSecond := false, false
	for _, m := range gen[0].Messages {
		seenInFirst = seenInFirst || strings.Contains(m.Text(), "and also mention the weather")
	}
	for _, m := range gen[1].Messages {
		seenInSecond = seenInSecond || strings.Contains(m.Text(), "and also mention the weather")
	}
	if seenInFirst || !seenInSecond {
		t.Fatalf("the queued line was applied at the wrong step (first %t, second %t)", seenInFirst, seenInSecond)
	}
}

// TestChatCommandsThatDoNotApplyAreRefusedAndNothingIsQueued: while a Run is active /compact is
// refused (and is not queued); /interrupt and /later need a Run and a text; an unknown command
// is not sent as a message; // sends a slash.
func TestChatCommandsThatDoNotApplyAreRefusedAndNothingIsQueued(t *testing.T) {
	fake := harnesstest.NewFake()
	held := holdFirstMeasure(fake)
	f := newGatewayFixture(t, fake, false)
	p := startCLI(t, chatArgs(f)...)
	p.until(p.stderr, `chat: session`)
	// Idle: no Run to stop, to interrupt or to hold for.
	p.send("/stop\n")
	p.until(p.stderr, `no run is active`)
	p.send("/interrupt something\n")
	p.until(p.stderr, `no run is active: send the text as a message`)
	p.send("/later something\n")
	p.until(p.stderr, `no run is active: send the text as a message`)
	p.send("/help\n")
	p.until(p.stderr, `/interrupt <text>`)
	p.send("/bogus\n")
	p.until(p.stderr, `unknown command /bogus`)

	p.send("start the run\n")
	runID := p.until(p.stderr, `\[run `+runIDRE+` started\]`)[1]
	waitHeld(t, held)
	p.send("/compact\n")
	p.until(p.stderr, `a run is active: /stop it or wait for it first`)
	p.send("/interrupt\n")
	p.until(p.stderr, `/interrupt needs a text`)
	p.send("/status\n")
	p.until(p.stderr, `run `+runID+` phase Measuring`)
	p.send("//a path like /usr/bin\n")
	p.until(p.stderr, `queued for the next step of the run`)
	if n := strings.Count(p.stderr.String(), "[compaction "); n != 0 {
		t.Fatalf("a compaction was started or queued: %s", p.stderr.String())
	}
	p.send("/exit\n")
	if code := p.wait(); code != 4 {
		t.Fatalf("exit %d\n%s", code, p.stderr.String())
	}
	if res := inspectStatus(t, f, runID); res["status"] != "cancelled" {
		t.Fatalf("%v", res)
	}
}

// TestChatEndOfInputIsAStopOfTheRunThatIsGoingOn: the pipe was cut: the Run is stopped, the
// chat waits for it to end and exits with how it ended; at the prompt the end of input is just
// the end.
func TestChatEndOfInputIsAStopOfTheRunThatIsGoingOn(t *testing.T) {
	fake := harnesstest.NewFake()
	held := holdFirstMeasure(fake)
	f := newGatewayFixture(t, fake, false)
	p := startCLI(t, chatArgs(f)...)
	p.until(p.stderr, `chat: session`)
	p.send("work on it\n")
	runID := p.until(p.stderr, `\[run `+runIDRE+` started\]`)[1]
	waitHeld(t, held)
	_ = p.stdin.Close()
	if code := p.wait(); code != 4 {
		t.Fatalf("exit %d\n%s", code, p.stderr.String())
	}
	if !strings.Contains(p.stderr.String(), "standard input ended: the run is stopped") {
		t.Fatalf("%s", p.stderr.String())
	}
	if res := inspectStatus(t, f, runID); res["status"] != "cancelled" || res["code"] != "CANCELLED" {
		t.Fatalf("%v", res)
	}

	idle := startCLI(t, chatArgs(f)...)
	idle.until(idle.stderr, `chat: session`)
	_ = idle.stdin.Close()
	if code := idle.wait(); code != 0 {
		t.Fatalf("exit %d\n%s", code, idle.stderr.String())
	}
}

func TestChatStreamsTheModelsTextToStandardOutputAndTheRestToStandardError(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Reply{Kind: harnesstest.KindFinal, Chunks: []string{"Hello, ", "streamed ", "world."}})
	f := newGatewayFixture(t, fake, false)
	p := startCLI(t, chatArgs(f)...)
	p.until(p.stderr, `chat: session`)
	p.send("say hi\n")
	p.until(p.stderr, `ended completed FINAL_RESPONSE_ACCEPTED`)
	p.send("/exit\n")
	if code := p.wait(); code != 0 {
		t.Fatalf("exit %d\n%s", code, p.stderr.String())
	}
	if got := p.stdout.String(); strings.TrimSpace(got) != "Hello, streamed world." {
		t.Fatalf("standard output is not the model's text alone: %q", got)
	}
}

// TestChatCompactsAnIdleThreadAndTheResultIsSaid: /compact with no Run active is the manual
// compaction of the thread, and chat says how it came out.
func TestChatCompactsAnIdleThreadAndTheResultIsSaid(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Reply{Dynamic: func(req modelport.ChatRequest) harnesstest.Reply {
		if req.Rencrow.Harness.Stage != modelport.StageAct {
			return harnesstest.Final(qaSummary)
		}
		return harnesstest.Final(strings.Repeat("答えました。", 1200))
	}})
	f := newGatewayFixture(t, fake, false)
	p := startCLI(t, chatArgs(f)...)
	p.until(p.stderr, `chat: session`)
	p.send("質問です。\n")
	p.until(p.stderr, `ended completed FINAL_RESPONSE_ACCEPTED`)
	p.send("/compact\n")
	p.until(p.stderr, `\[compaction executed NormalCompacted\]`)
	p.send("/exit\n")
	if code := p.wait(); code != 0 {
		t.Fatalf("exit %d\n%s", code, p.stderr.String())
	}
}
