package service_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolview"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// TestHelperProcess is not a test: it is the program process.exec runs in these tests
// (the test binary itself, so no shell and no system program is involved). It acts only
// where the environment profile the policy grants sets the marker.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("RENCROW_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	switch args[0] {
	case "echo":
		fmt.Fprint(os.Stdout, "hello out")
		fmt.Fprint(os.Stderr, "hello err")
	case "utf8":
		fmt.Fprint(os.Stdout, "日本語です")
	case "env":
		env := os.Environ()
		sort.Strings(env)
		fmt.Fprint(os.Stdout, strings.Join(env, "\n"))
	case "append": // appends one line to a file: the count of lines is the count of runs
		f, err := os.OpenFile(args[1], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			os.Exit(3)
		}
		fmt.Fprintln(f, "ran")
		_ = f.Close()
		fmt.Fprint(os.Stdout, "appended")
	case "big":
		n, _ := strconv.Atoi(args[1])
		chunk := []byte(strings.Repeat("x", 4096))
		for n > 0 {
			k := min(n, len(chunk))
			if _, err := os.Stdout.Write(chunk[:k]); err != nil {
				os.Exit(9)
			}
			n -= k
		}
		time.Sleep(30 * time.Second)
	case "sleep":
		time.Sleep(60 * time.Second)
	case "tree": // a grandchild in the same group: its PID is written to a file, then both wait
		exe, _ := os.Executable()
		c := exec.Command(exe, "-test.run=TestHelperProcess", "--", "sleep")
		c.Env = os.Environ()
		if err := c.Start(); err != nil {
			os.Exit(8)
		}
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(c.Process.Pid)), 0o644); err != nil {
			os.Exit(7)
		}
		time.Sleep(60 * time.Second)
	case "exit":
		n, _ := strconv.Atoi(args[1])
		os.Exit(n)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func j(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

type m = map[string]any

func readArgs(path string, start, end, max int) string {
	return j(m{"path": path, "range": m{"start": start, "end": end}, "max_bytes": max})
}

func createArgs(path, text string) string {
	return j(m{"path": path, "text": text, "expected_absent": true})
}

func execArgs(argv []string, env string, timeout int) string {
	exe, _ := os.Executable()
	return j(m{"executable": exe, "argv": append([]string{"-test.run=TestHelperProcess", "--"}, argv...), "cwd": ".", "env_profile_ref": env, "timeout_seconds": timeout})
}

func evidenceArgs(id, projection string, start, end int) string {
	return j(m{"evidence_id": id, "projection_version": projection, "range": m{"start": start, "end": end}})
}

func calls(cs ...harnesstest.Call) harnesstest.Reply {
	return harnesstest.Reply{Kind: harnesstest.KindToolCall, Calls: cs}
}

func tc(id, name, args string) harnesstest.Call {
	return harnesstest.Call{ID: id, Name: name, Args: args}
}

// toolConfig says what the policy of a Tool rig grants.
type toolConfig struct {
	tools         []string
	readPrefixes  []string
	writePrefixes []string
	// process adds the test binary as a process profile, with a clean and a "helper"
	// environment (which sets the marker the helper acts on).
	process bool
	// twin adds a second profile for the same executable and prefix (an ambiguity).
	twin bool
}

var allTools = []string{"file.read", "file.search", "file.create", "file.edit", "evidence.read"}

func (c toolConfig) edit(l *harnesstest.Layout) {
	pol := l.Reg["policies"].([]any)[0].(map[string]any)
	if c.tools == nil {
		c.tools = allTools
	}
	asAny := func(s []string) []any {
		out := make([]any, len(s))
		for i, v := range s {
			out[i] = v
		}
		return out
	}
	pol["tools"] = asAny(c.tools)
	if c.readPrefixes != nil {
		pol["read_prefixes"] = asAny(c.readPrefixes)
	}
	if c.writePrefixes != nil {
		pol["write_prefixes"] = asAny(c.writePrefixes)
	}
	if !c.process {
		return
	}
	exe, _ := os.Executable()
	profile := func(name string) m {
		return m{"name": name, "executable": exe, "is_shell": false, "argv_prefix": []any{"-test.run=TestHelperProcess", "--"}}
	}
	profiles := []any{profile("helper")}
	names := []any{"helper"}
	if c.twin {
		profiles, names = append(profiles, profile("helper-twin")), append(names, "helper-twin")
	}
	l.Cfg["process_profiles"] = profiles
	pol["process_profiles"] = names
	pol["tools"] = append(pol["tools"].([]any), "process.exec")
	pol["env_profiles"] = []any{"clean", "helper"}
	l.Cfg["env_profiles"] = []any{m{"name": "clean", "values": m{}}, m{"name": "helper", "values": m{"RENCROW_HELPER": "1"}}}
}

// toolRig is a rig whose model is the fake and whose policy is c's.
func toolRig(t *testing.T, fake *harnesstest.Fake, c toolConfig) (*rig, *recorder) {
	t.Helper()
	r := newModelRig(t, fake, nil, c.edit)
	rec := &recorder{}
	r.conn = r.svc.NewConn(rec)
	return r, rec
}

func (r *rig) openMode(key, mode string) protocol.SessionInfo {
	r.t.Helper()
	r.initialize()
	p := r.openParams(key)
	p.ExecutionMode = mode
	return decode[protocol.SessionOpenResult](r.t, r.mustCall("session/open", p)).Session
}

func (r *rig) write(rel, content string) string {
	r.t.Helper()
	p := filepath.Join(r.layout.Work, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	return p
}

func (r *rig) read(rel string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(r.layout.Work, filepath.FromSlash(rel)))
	return string(b), err == nil
}

// toolMessages are the role=tool messages of a request, in order, with the view each
// carries decoded.
func toolMessages(t *testing.T, req modelport.ChatRequest) []toolview.View {
	t.Helper()
	var out []toolview.View
	for _, msg := range req.Messages {
		if msg.Role != "tool" {
			continue
		}
		var v toolview.View
		if err := json.Unmarshal([]byte(msg.Text()), &v); err != nil {
			t.Fatalf("a tool message is not a view: %v\n%s", err, msg.Text())
		}
		out = append(out, v)
	}
	return out
}

func resultOf(t *testing.T, v toolview.View) map[string]any {
	t.Helper()
	var out map[string]any
	if string(v.Result) == "null" || len(v.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(v.Result, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestARunReadsAFileThroughATool is the exchange from end to end: the request declares
// the policy's Tools, the model's call is bound to an Action and run, the answer enters
// the next prompt as role=tool beside the assistant message that asked, and the Run
// ends with the model's answer.
func TestARunReadsAFileThroughATool(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(
		harnesstest.Reply{Kind: harnesstest.KindToolCall, Text: "Let me look.", Calls: []harnesstest.Call{tc("call-1", "file.read", readArgs("demo.txt", 0, 100, 100))}},
		harnesstest.Final("The file says hello."))
	r, rec := toolRig(t, fake, toolConfig{})
	r.write("demo.txt", "hello\n")
	info := r.openSession("tool.open.000000000001")
	start := r.startRun(info.ThreadID, "tool.start.00000000001", "what does demo.txt say?")
	run := r.waitTerminal(start.RunID)
	res := run.Result
	if res.Status != "completed" || res.FinalText != "The file says hello." || res.Verification.Status != "not_run" || len(res.UnresolvedActionIDs) != 0 {
		t.Fatalf("%+v", res)
	}
	// The input, the assistant message, the answer and the final message: four applications.
	if run.GenerationAttemptsUsed != 2 || run.ContextRevision != 4 || fake.Describes() != 2 || len(fake.Measures()) != 2 || len(fake.Generates()) != 2 {
		t.Fatalf("%+v generates=%d", run, len(fake.Generates()))
	}

	// The first request declares the Tools of the policy, in name order, as the schema has them.
	g0 := fake.Generates()[0]
	var names []string
	for _, d := range g0.Tools {
		names = append(names, d.Function.Name)
		if d.Type != "function" || !d.Function.Strict || len(d.Function.Parameters) == 0 {
			t.Fatalf("%+v", d)
		}
	}
	if strings.Join(names, ",") != "evidence.read,file.create,file.edit,file.read,file.search" || g0.ToolChoice != "auto" {
		t.Fatalf("%v %s", names, g0.ToolChoice)
	}
	// The second request carries the exchange: the assistant message (its text, its call
	// exactly as made) and its answer, directly after it.
	g1 := fake.Generates()[1]
	n := len(g1.Messages)
	asst, tool := g1.Messages[n-2], g1.Messages[n-1]
	if asst.Role != "assistant" || asst.Text() != "Let me look." || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call-1" || asst.ToolCalls[0].Function.Name != "file.read" ||
		asst.ToolCalls[0].Function.Arguments != readArgs("demo.txt", 0, 100, 100) || tool.Role != "tool" || tool.ToolCallID != "call-1" {
		t.Fatalf("%+v %+v", asst, tool)
	}
	views := toolMessages(t, g1)
	if len(views) != 1 || views[0].Tool != "file.read" || views[0].EffectState != "completed" || views[0].Error != nil || views[0].ExitCode != nil || !views[0].CaptureComplete {
		t.Fatalf("%+v", views)
	}
	got := resultOf(t, views[0])
	if got["content"] != "hello\n" || got["raw_hash"] != sha("hello\n") || got["total_bytes"] != float64(6) || got["partial"] != false || got["encoding"] != "utf-8" {
		t.Fatalf("%v", got)
	}

	// The store: one model Action per step and one Tool Action, linked by the call's key.
	if r.val("SELECT COUNT(*) FROM actions WHERE kind='model'") != "2" ||
		r.val("SELECT name||'/'||status FROM actions WHERE kind='tool'") != "file.read/completed" ||
		r.val("SELECT l.provider_tool_call_id||'/'||l.ordinal||'/'||(l.model_response_id LIKE 'rsp_%') FROM tool_links l") != "call-1/0/1" ||
		r.val("SELECT state FROM attempts WHERE action_id=(SELECT action_id FROM actions WHERE kind='tool')") != "completed" {
		t.Fatal("the Action of the call is not recorded")
	}
	events := r.threadEvents(info.ThreadID)
	var seq []string
	for _, e := range events {
		switch e.Type {
		case "model.requested", "model.completed", "run.terminal":
			seq = append(seq, e.Type)
		case "action.prepared", "action.dispatch_started", "action.completed":
			// Only the Tool's: the model Actions have their own events.
			var id struct {
				ActionID string `json:"action_id"`
			}
			if err := json.Unmarshal(e.Payload, &id); err != nil {
				t.Fatal(err)
			}
			if r.val("SELECT kind FROM actions WHERE action_id=?", id.ActionID) == "tool" {
				seq = append(seq, e.Type)
			}
		}
	}
	if strings.Join(seq, ",") != "model.requested,model.completed,action.prepared,action.dispatch_started,action.completed,model.requested,model.completed,run.terminal" {
		t.Fatalf("%s", strings.Join(seq, ","))
	}
	toolEvent := func(typ string) protocol.Event {
		for _, e := range ofType(events, typ) {
			var id struct {
				ActionID string `json:"action_id"`
			}
			_ = json.Unmarshal(e.Payload, &id)
			if r.val("SELECT kind FROM actions WHERE action_id=?", id.ActionID) == "tool" {
				return e
			}
		}
		t.Fatalf("no %s of a Tool", typ)
		return protocol.Event{}
	}
	prepared := payloadOf[protocol.ActionPreparedPayload](t, toolEvent("action.prepared"))
	started := payloadOf[protocol.ActionDispatchStartedPayload](t, toolEvent("action.dispatch_started"))
	done := payloadOf[protocol.ActionCompletedPayload](t, toolEvent("action.completed"))
	if prepared.Kind != "tool" || prepared.Name != "file.read" || prepared.ArgsHash != sha(`{"max_bytes":100,"path":"demo.txt","range":{"end":100,"start":0}}`) ||
		started.ActionID != prepared.ActionID || started.WriterEpoch < 1 || done.ActionID != prepared.ActionID || done.EffectState != "completed" || done.ExitCode != nil ||
		!done.CaptureComplete || len(done.ResultEvidenceIDs) != 1 {
		t.Fatalf("%+v %+v %+v", prepared, started, done)
	}
	// The recorder saw the same confirmed events, in order.
	seen, _ := rec.seen()
	if len(ofType(seen, "action.completed")) != 1 || len(ofType(seen, "action.prepared")) != 3 || len(seen) != len(events)-len(ofType(events, "session.created"))-
		len(ofType(events, "input.accepted"))-len(ofType(events, "task.created"))-len(ofType(events, "run.started")) {
		t.Fatalf("%s", eventTypes(seen))
	}
	// The result is sealed Evidence a client can read back, and the view in it is what the model saw.
	if r.evidence(done.ResultEvidenceIDs[0]) != fake.Generates()[1].Messages[n-1].Text() {
		t.Fatal("the stored answer is not the one the model was given")
	}
	// No file was changed by a read.
	if c, _ := r.read("demo.txt"); c != "hello\n" {
		t.Fatal("a read changed the file")
	}
	if rep, err := r.store.VerifyClosure(t.Context()); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}

// TestALaterRunSeesWhatAnEarlierRunDid: the exchange is part of the Thread's context.
func TestALaterRunSeesWhatAnEarlierRunDid(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(
		calls(tc("c1", "file.create", createArgs("made.txt", "created by the model"))),
		harnesstest.Final("done"),
		harnesstest.Final("yes, I made it"))
	r, _ := toolRig(t, fake, toolConfig{})
	info := r.openSession("later.open.0000000000001")
	first := r.waitTerminal(r.startRun(info.ThreadID, "later.start.000000000001", "make a file").RunID)
	if first.Result.Status != "completed" {
		t.Fatalf("%+v", first.Result)
	}
	if c, ok := r.read("made.txt"); !ok || c != "created by the model" {
		t.Fatalf("%q %v", c, ok)
	}
	second := r.startRun(info.ThreadID, "later.start.000000000002", "did you?", func(in *protocol.StartInput) { in.ExpectedContextRevision = first.ContextRevision })
	if r.waitTerminal(second.RunID).Result.Status != "completed" {
		t.Fatal("the second Run did not complete")
	}
	var roles []string
	for _, msg := range fake.Generates()[2].Messages[1:] {
		roles = append(roles, msg.Role)
	}
	// user, assistant (asking), tool (the answer), assistant (the final), user.
	if strings.Join(roles, ",") != "user,assistant,tool,assistant,user" {
		t.Fatalf("%v", roles)
	}
	msgs := fake.Generates()[2].Messages
	if len(msgs[2].ToolCalls) != 1 || msgs[2].ToolCalls[0].ID != "c1" || msgs[3].ToolCallID != "c1" || msgs[4].Text() != "done" {
		t.Fatalf("%+v", msgs[2:])
	}
}

// TestTheRecordedToolStreamIsActedOn: the design package's recorded stream (the call's
// arguments arrive in two fragments), made a valid answer to the request it follows.
func TestTheRecordedToolStreamIsActedOn(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(harnesstest.ModuleRoot(t), "testdata", "contract", "examples", "wire", "act_tool_stream.sse"))
	if err != nil {
		t.Fatal(err)
	}
	recorded := string(raw)
	for _, want := range []string{`\"demo.txt\"}`, `"fixture-read-1"`, "9a0742630d64d3fc648f24a0d92b22362e17d40af325efb2196396481599615c", "fec1d0c41d2ece109a38e21c9c10d4a6598e169292515aaae4eaa12739a5a88a"} {
		if !strings.Contains(recorded, want) {
			t.Fatalf("the recorded stream has changed: %s is missing", want)
		}
	}
	answer := func(complete bool) harnesstest.Reply {
		return harnesstest.Reply{Kind: harnesstest.KindRaw, Stream: func(input, request string) string {
			s := strings.ReplaceAll(recorded, "9a0742630d64d3fc648f24a0d92b22362e17d40af325efb2196396481599615c", input)
			s = strings.ReplaceAll(s, "fec1d0c41d2ece109a38e21c9c10d4a6598e169292515aaae4eaa12739a5a88a", request)
			if complete {
				// The second fragment finishes the arguments the schema requires.
				s = strings.ReplaceAll(s, `\"demo.txt\"}`, `\"demo.txt\",\"range\":{\"start\":0,\"end\":64},\"max_bytes\":64}`)
			}
			return s
		}}
	}

	t.Run("arguments that satisfy the schema", func(t *testing.T) {
		fake := harnesstest.NewFake()
		fake.SetScript(answer(true), harnesstest.Final("read it"))
		r, _ := toolRig(t, fake, toolConfig{})
		r.write("demo.txt", "demo content")
		info := r.openSession("rec.open.00000000000001")
		run := r.waitTerminal(r.startRun(info.ThreadID, "rec.start.0000000000001", "go").RunID)
		if run.Result.Status != "completed" {
			t.Fatalf("%+v", run.Result)
		}
		views := toolMessages(t, fake.Generates()[1])
		if len(views) != 1 || resultOf(t, views[0])["content"] != "demo content" {
			t.Fatalf("%+v", views)
		}
		if msgs := fake.Generates()[1].Messages; msgs[len(msgs)-1].ToolCallID != "fixture-read-1" {
			t.Fatalf("%+v", msgs[len(msgs)-1])
		}
	})
	t.Run("the arguments as recorded do not satisfy the schema", func(t *testing.T) {
		fake := harnesstest.NewFake()
		fake.SetScript(answer(false))
		r, _ := toolRig(t, fake, toolConfig{})
		r.write("demo.txt", "demo content")
		info := r.openSession("rec2.open.0000000000001")
		run := r.waitTerminal(r.startRun(info.ThreadID, "rec2.start.000000000001", "go").RunID)
		res := run.Result
		// The same answer on the one retry F31 allows ends the Run: two generations, one Action.
		if res.Status != "failed" || res.Code != "MODEL_OUTPUT_INVALID" || len(res.UnresolvedActionIDs) != 0 || len(fake.Generates()) != 2 {
			t.Fatalf("%+v", res)
		}
		if r.count("actions") != 1 || r.count("tool_links") != 0 {
			t.Fatal("a call that did not validate became an Action")
		}
		for _, ev := range ofType(r.threadEvents(info.ThreadID), "model.completed") {
			done := payloadOf[protocol.ModelCompletedPayload](t, ev)
			if done.Outcome != "error" || done.GenerationState != "terminal" || done.FailureCode == nil || *done.FailureCode != "MODEL_OUTPUT_SCHEMA_INVALID" {
				t.Fatalf("%+v", done)
			}
		}
	})
}

// TestOneBadCallRefusesTheWholeResponseAndNothingRuns: the first call is valid, the
// second names a Tool the request did not declare; the file the first would have made
// does not exist, and no Action exists for either.
func TestOneBadCallRefusesTheWholeResponseAndNothingRuns(t *testing.T) {
	for name, bad := range map[string]harnesstest.Call{
		"a Tool that was not declared": tc("c2", "process.exec", `{"executable":"/bin/ls","argv":[],"cwd":".","env_profile_ref":"clean","timeout_seconds":1}`),
		"arguments of the wrong shape": tc("c2", "file.read", `{"path":"x"}`),
		"an unknown Tool":              tc("c2", "file.delete", `{"path":"x"}`),
	} {
		t.Run(name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			fake.SetReply(calls(tc("c1", "file.create", createArgs("never.txt", "x")), bad))
			r, _ := toolRig(t, fake, toolConfig{})
			info := r.openSession("bad.open.00000000000001")
			res := r.waitTerminal(r.startRun(info.ThreadID, "bad.start.0000000000001", "go").RunID).Result
			if res.Status != "failed" || res.Code != "MODEL_OUTPUT_INVALID" || len(fake.Generates()) != 2 || r.count("tool_links") != 0 || r.count("actions") != 1 {
				t.Fatalf("%+v", res)
			}
			if _, ok := r.read("never.txt"); ok {
				t.Fatal("a call of a refused response ran")
			}
			// The response is kept as the model gave it, and is not part of the context.
			if r.count("context_entries") != 1 {
				t.Fatalf("%d context entries: a refused response entered the context", r.count("context_entries"))
			}
		})
	}
}

// TestCallsRunInOrderAndACallThatFailsStopsTheRest: the third call was written before
// the model knew the second would fail, so it is not run, and is answered as not run.
func TestCallsRunInOrderAndACallThatFailsStopsTheRest(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(
		calls(tc("c1", "file.create", createArgs("one.txt", "1")), tc("c2", "file.read", readArgs("missing.txt", 0, 10, 10)), tc("c3", "file.create", createArgs("three.txt", "3"))),
		harnesstest.Final("handled it"))
	r, _ := toolRig(t, fake, toolConfig{})
	info := r.openSession("order.open.000000000001")
	run := r.waitTerminal(r.startRun(info.ThreadID, "order.start.00000000001", "go").RunID)
	if run.Result.Status != "completed" || run.Result.FinalText != "handled it" {
		t.Fatalf("%+v", run.Result)
	}
	if c, _ := r.read("one.txt"); c != "1" {
		t.Fatal("the first call did not run")
	}
	if _, ok := r.read("three.txt"); ok {
		t.Fatal("a call after a failed call ran")
	}
	// All three calls are answered, in the order of the calls, directly after the message that asked.
	g1 := fake.Generates()[1]
	n := len(g1.Messages)
	if g1.Messages[n-4].Role != "assistant" || len(g1.Messages[n-4].ToolCalls) != 3 {
		t.Fatalf("%+v", g1.Messages[n-4])
	}
	for i, id := range []string{"c1", "c2", "c3"} {
		if g1.Messages[n-3+i].Role != "tool" || g1.Messages[n-3+i].ToolCallID != id || g1.Messages[n-4].ToolCalls[i].ID != id {
			t.Fatalf("answer %d: %+v", i, g1.Messages[n-3+i])
		}
	}
	views := toolMessages(t, g1)
	if views[0].EffectState != "completed" || views[1].EffectState != "failed" || views[1].Error == nil || views[1].Error.Code != "NOT_FOUND" ||
		views[2].EffectState != "not_started" || views[2].Error == nil || views[2].Error.Code != "NOT_EXECUTED" || !strings.Contains(views[2].Error.Message, "earlier call") {
		t.Fatalf("%+v", views)
	}
	// Each call has its Action; only the two that were reached were dispatched.
	states := r.val(`SELECT group_concat(a.state, ',') FROM (SELECT att.state FROM attempts att JOIN actions ac ON ac.action_id=att.action_id JOIN tool_links l ON l.action_id=ac.action_id
		WHERE ac.kind='tool' ORDER BY l.ordinal) a`)
	if states != "completed,failed,cancelled" {
		t.Fatalf("%s", states)
	}
	events := r.threadEvents(info.ThreadID)
	if n := len(ofType(events, "action.prepared")); n != 5 { // two model Actions and three calls
		t.Fatalf("%d action.prepared", n)
	}
	var tool []string
	for _, e := range ofType(events, "action.completed") {
		tool = append(tool, payloadOf[protocol.ActionCompletedPayload](t, e).EffectState)
	}
	if strings.Join(tool, ",") != "completed,failed,not_started" {
		t.Fatalf("%v", tool)
	}
	dispatched := 0
	for _, e := range ofType(events, "action.dispatch_started") {
		if r.val("SELECT kind FROM actions WHERE action_id=?", payloadOf[protocol.ActionDispatchStartedPayload](t, e).ActionID) == "tool" {
			dispatched++
		}
	}
	if dispatched != 2 {
		t.Fatalf("%d calls were dispatched", dispatched)
	}
}

// TestAPolicyRefusalIsAnAnswerTheModelReads: a path outside the read prefixes is refused
// before anything is attempted, recorded as a refused Action, and the Run goes on.
func TestAPolicyRefusalIsAnAnswerTheModelReads(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(calls(tc("c1", "file.read", readArgs("private/key.txt", 0, 10, 10))), harnesstest.Final("I could not read it"))
	r, _ := toolRig(t, fake, toolConfig{readPrefixes: []string{"public"}})
	r.write("private/key.txt", "secret")
	info := r.openSession("refuse.open.0000000001")
	run := r.waitTerminal(r.startRun(info.ThreadID, "refuse.start.000000001", "go").RunID)
	if run.Result.Status != "completed" {
		t.Fatalf("%+v", run.Result)
	}
	v := toolMessages(t, fake.Generates()[1])[0]
	if v.EffectState != "not_started" || v.Error == nil || v.Error.Code != "PATH_OUTSIDE_SCOPE" || strings.Contains(string(v.Result), "secret") {
		t.Fatalf("%+v", v)
	}
	if r.val("SELECT status FROM actions WHERE kind='tool'") != "rejected" || r.val("SELECT state FROM attempts WHERE action_id=(SELECT action_id FROM actions WHERE kind='tool')") != "failed" {
		t.Fatal("a refused call is not recorded as refused")
	}
	events := r.threadEvents(info.ThreadID)
	for _, e := range ofType(events, "action.dispatch_started") {
		if r.val("SELECT kind FROM actions WHERE action_id=?", payloadOf[protocol.ActionDispatchStartedPayload](t, e).ActionID) == "tool" {
			t.Fatal("a refused call was dispatched")
		}
	}
}

// TestProcessExecRunsUnderOneProfileWithAnExplicitEnvironment covers process.exec through
// a Run: the program runs without a shell, sees only the profile's environment, its
// output is sealed Evidence the model reads back through evidence.read, and the answer
// says whether the capture is complete.
func TestProcessExecRunsUnderOneProfileWithAnExplicitEnvironment(t *testing.T) {
	t.Setenv("RENCROW_SECRET_CANARY", "gateway-token-value")
	fake := harnesstest.NewFake()
	fake.SetScript(
		calls(tc("c1", "process.exec", execArgs([]string{"echo"}, "helper", 20)), tc("c2", "process.exec", execArgs([]string{"env"}, "helper", 20)), tc("c3", "process.exec", execArgs([]string{"utf8"}, "helper", 20))),
		harnesstest.Final("ran them"))
	r, _ := toolRig(t, fake, toolConfig{process: true})
	info := r.openSession("proc.open.00000000000001")
	run := r.waitTerminal(r.startRun(info.ThreadID, "proc.start.0000000000001", "go").RunID)
	if run.Result.Status != "completed" {
		t.Fatalf("%+v", run.Result)
	}
	if g0 := fake.Generates()[0]; len(g0.Tools) != 6 || g0.Tools[5].Function.Name != "process.exec" {
		t.Fatalf("process.exec is not in the catalog of a trusted_host Run with a profile: %d tools", len(g0.Tools))
	}
	views := toolMessages(t, fake.Generates()[1])
	if len(views) != 3 {
		t.Fatalf("%d answers", len(views))
	}
	echo := resultOf(t, views[0])
	stdout := echo["stdout"].(map[string]any)
	if views[0].EffectState != "completed" || views[0].ExitCode == nil || *views[0].ExitCode != 0 || !views[0].CaptureComplete || stdout["preview"] != "hello out" ||
		echo["stderr"].(map[string]any)["preview"] != "hello err" || stdout["total_bytes"] != float64(9) || echo["timed_out"] != false || len(views[0].EvidenceIDs) != 2 {
		t.Fatalf("%+v %v", views[0], echo)
	}
	// Nothing of the host's environment reached the program.
	env := resultOf(t, views[1])["stdout"].(map[string]any)["preview"].(string)
	if strings.Contains(env, "gateway-token-value") || strings.Contains(env, "CANARY") || strings.Contains(env, "PATH=") || !strings.Contains(env, "RENCROW_HELPER=1") {
		t.Fatalf("the environment was not the profile's:\n%s", env)
	}
	// The output is Evidence: sealed, complete, hashed, readable.
	outID := stdout["evidence_id"].(string)
	if r.val("SELECT state||'|'||capture_complete||'|'||total_bytes||'|'||raw_hash||'|'||media_type FROM evidence WHERE evidence_id=?", outID) != "sealed|1|9|"+sha("hello out")+"|text/plain; charset=utf-8" {
		t.Fatalf("%s", r.val("SELECT state||'|'||capture_complete||'|'||total_bytes FROM evidence WHERE evidence_id=?", outID))
	}
	if r.evidence(outID) != "hello out" {
		t.Fatal("the stored output is not what the program printed")
	}
	if r.val("SELECT json_extract(metadata_json,'$.purpose') FROM evidence WHERE evidence_id=?", outID) != "tool_stdout" || r.val("SELECT host_incarnation IS NOT NULL AND process_token IS NOT NULL FROM attempts WHERE action_id=(SELECT action_id FROM actions WHERE name='process.exec' LIMIT 1)") != "1" {
		t.Fatal("the output purpose or the process identity is not recorded")
	}
}

// TestEvidenceReadReadsWhatAProcessPrintedAndRefusesWhatIsNotTheThreads: the model reads
// ranges of earlier output by the same rules as evidence/read, inside its own Thread.
func TestEvidenceReadReadsWhatAProcessPrintedAndRefusesWhatIsNotTheThreads(t *testing.T) {
	fake := harnesstest.NewFake()
	r, _ := toolRig(t, fake, toolConfig{process: true})
	// Another Thread of the same store, with Evidence the model learns the ID of.
	infoB := r.openSession("ev.open.b.0000000000001")
	runB := r.waitTerminal(r.startRun(infoB.ThreadID, "ev.start.b.000000000001", "x").RunID)
	if runB.Result.Status != "completed" {
		t.Fatalf("%+v", runB.Result)
	}
	foreign := r.val("SELECT evidence_id FROM items WHERE thread_id=? LIMIT 1", infoB.ThreadID)
	infoA := decode[protocol.SessionOpenResult](t, r.mustCall("session/open", r.openParams("ev.open.a.0000000000001"))).Session

	fake.SetScript(
		calls(tc("c1", "process.exec", execArgs([]string{"utf8"}, "helper", 20))),
		harnesstest.Reply{Dynamic: func(req modelport.ChatRequest) harnesstest.Reply {
			views := toolMessages(t, req)
			id := resultOf(t, views[len(views)-1])["stdout"].(map[string]any)["evidence_id"].(string)
			return calls(
				tc("r1", "evidence.read", evidenceArgs(id, "text/v1", 0, 6)), // two characters of three bytes
				tc("r2", "evidence.read", evidenceArgs(id, "text/v1", 0, 4)), // cuts a character
				tc("r3", "evidence.read", evidenceArgs(id, "raw/v1", 0, 4)),  // raw reads any bytes
				tc("r4", "evidence.read", evidenceArgs(id, "raw/v1", 0, 99)), // past the end
				tc("r5", "evidence.read", evidenceArgs(foreign, "raw/v1", 0, 4)))
		}},
		harnesstest.Final("read it"))
	run := r.waitTerminal(r.startRun(infoA.ThreadID, "ev.start.a.000000000001", "go").RunID)
	if run.Result.Status != "completed" {
		t.Fatalf("%+v", run.Result)
	}
	// The calls are answered in order; the one after the first failure is not run.
	views := toolMessages(t, fake.Generates()[3])[1:]
	if len(views) != 5 {
		t.Fatalf("%d answers", len(views))
	}
	first := resultOf(t, views[0])
	if views[0].EffectState != "completed" || first["data"] != "日本" || first["encoding"] != "utf-8" || first["total_bytes"] != float64(15) || first["partial"] != true ||
		first["raw_hash"] != sha("日本語です") || first["capture_complete"] != true {
		t.Fatalf("%+v %v", views[0], first)
	}
	if views[1].EffectState != "failed" || views[1].Error == nil || views[1].Error.Code != "INVALID_RANGE" {
		t.Fatalf("a text range through a character: %+v", views[1])
	}
	for i := 2; i < 5; i++ {
		if views[i].EffectState != "not_started" || views[i].Error == nil || views[i].Error.Code != "NOT_EXECUTED" {
			t.Fatalf("answer %d: %+v", i, views[i])
		}
	}
	// Read on its own, each of the remaining cases has the answer the method gives.
	fake2 := harnesstest.NewFake()
	r2, _ := toolRig(t, fake2, toolConfig{process: true})
	infoB2 := r2.openSession("ev2.open.b.000000000001")
	_ = r2.waitTerminal(r2.startRun(infoB2.ThreadID, "ev2.start.b.00000000001", "x").RunID)
	foreign2 := r2.val("SELECT evidence_id FROM items WHERE thread_id=? LIMIT 1", infoB2.ThreadID)
	infoA2 := decode[protocol.SessionOpenResult](t, r2.mustCall("session/open", r2.openParams("ev2.open.a.000000000001"))).Session
	one := func(args func(id string) string) toolview.View {
		fake2.SetScript(
			calls(tc("c1", "process.exec", execArgs([]string{"utf8"}, "helper", 20))),
			harnesstest.Reply{Dynamic: func(req modelport.ChatRequest) harnesstest.Reply {
				views := toolMessages(t, req)
				id := resultOf(t, views[len(views)-1])["stdout"].(map[string]any)["evidence_id"].(string)
				return calls(tc("r1", "evidence.read", args(id)))
			}},
			harnesstest.Final("ok"))
		before := len(fake2.Generates())
		key := fmt.Sprintf("ev2.start.a.%011d", before)
		cur := decode[protocol.SessionInfo](t, r2.mustCall("session/get", protocol.SessionGetInput{ThreadID: infoA2.ThreadID}))
		run := r2.waitTerminal(r2.startRun(infoA2.ThreadID, key, "go", func(in *protocol.StartInput) { in.ExpectedContextRevision = cur.ContextRevision }).RunID)
		if run.Result.Status != "completed" {
			t.Fatalf("%+v", run.Result)
		}
		v := toolMessages(t, fake2.Generates()[len(fake2.Generates())-1])
		return v[len(v)-1]
	}
	if v := one(func(id string) string { return evidenceArgs(id, "raw/v1", 0, 3) }); v.EffectState != "completed" || resultOf(t, v)["data"] != "日" || resultOf(t, v)["encoding"] != "utf-8" {
		t.Fatalf("%s", v.Result)
	}
	// Raw bytes that are not text come back as they are, in base64.
	if v := one(func(id string) string { return evidenceArgs(id, "raw/v1", 0, 4) }); v.EffectState != "completed" || resultOf(t, v)["encoding"] != "base64" {
		t.Fatalf("%s", v.Result)
	}
	if v := one(func(id string) string { return evidenceArgs(id, "raw/v1", 0, 99) }); v.EffectState != "failed" || v.Error.Code != "INVALID_RANGE" {
		t.Fatalf("%+v", v)
	}
	if v := one(func(string) string { return evidenceArgs(foreign2, "raw/v1", 0, 4) }); v.EffectState != "failed" || v.Error.Code != "EVIDENCE_NOT_READABLE" {
		t.Fatalf("another Thread's Evidence: %+v", v)
	}
	if v := one(func(string) string { return evidenceArgs("evd_01a1151c-5a6f-7e06-996b-c21d45447041", "raw/v1", 0, 4) }); v.EffectState != "failed" || v.Error.Code != "EVIDENCE_NOT_READABLE" {
		t.Fatalf("Evidence that does not exist: %+v", v)
	}
}
