package main_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The binary is built once for the whole run, from the module under test, into a
// private temporary directory.
var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
	binDir    string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		binDir, buildErr = os.MkdirTemp("", "rencrow-harness-e2e-")
		if buildErr != nil {
			return
		}
		name := "rencrow-harness"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		binPath = filepath.Join(binDir, name)
		goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
		if runtime.GOOS == "windows" {
			goTool += ".exe"
		}
		cmd := exec.Command(goTool, "build", "-o", binPath, "./cmd/rencrow-harness")
		cmd.Dir = harnesstest.ModuleRoot(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build failed: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

// runCLI runs one command to completion in a directory that is not the repository.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	cmd.Dir = t.TempDir()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), out.String(), errb.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, out.String(), errb.String()
}

// server is one `serve --stdio` child process with its pipes.
type server struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	stderr *bytes.Buffer
	nextID int
	exited chan error
}

func startServe(t *testing.T, config string) *server {
	t.Helper()
	cmd := exec.Command(binary(t), "serve", "--stdio", "--config", config)
	cmd.Dir = t.TempDir()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	s := &server{t: t, cmd: cmd, stdin: stdin, lines: make(chan string, 1024), stderr: &bytes.Buffer{}, exited: make(chan error, 1)}
	cmd.Stderr = s.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		br := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				s.lines <- strings.TrimSuffix(line, "\n")
			}
			if err != nil {
				close(s.lines)
				return
			}
		}
	}()
	go func() { s.exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return s
}

func (s *server) rid() string {
	s.nextID++
	return fmt.Sprintf("req_00000000-0000-7000-8000-%012x", s.nextID)
}

func (s *server) sendRaw(frame string) {
	s.t.Helper()
	if _, err := io.WriteString(s.stdin, frame+"\n"); err != nil {
		s.t.Fatalf("write to the server: %v", err)
	}
}

func (s *server) readLine() map[string]any {
	s.t.Helper()
	select {
	case l, ok := <-s.lines:
		if !ok {
			s.t.Fatalf("the server closed stdout; stderr: %s", s.stderr.String())
		}
		var m map[string]any
		dec := json.NewDecoder(strings.NewReader(l))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil || m["jsonrpc"] != "2.0" {
			s.t.Fatalf("stdout carried something that is not a JSON-RPC object: %q (%v)", l, err)
		}
		return m
	case <-time.After(20 * time.Second):
		s.t.Fatalf("no output from the server; stderr: %s", s.stderr.String())
	}
	return nil
}

// reply is one response together with the notifications that arrived before it.
type reply struct {
	m     map[string]any
	notes []map[string]any
}

func (r reply) result() map[string]any { return r.m["result"].(map[string]any) }

// errCode returns the JSON-RPC code and the ErrorInfo code of an error response.
func (r reply) errCode(t *testing.T) (int, string) {
	t.Helper()
	e, ok := r.m["error"].(map[string]any)
	if !ok {
		t.Fatalf("not an error: %v", r.m)
	}
	n, _ := e["code"].(json.Number).Int64()
	info, _ := e["data"].(map[string]any)
	code, _ := info["code"].(string)
	return int(n), code
}

// await reads until the response with this id, collecting notifications on the way.
func (s *server) await(id string) reply {
	s.t.Helper()
	var r reply
	for {
		m := s.readLine()
		if m["id"] == id {
			r.m = m
			return r
		}
		if m["id"] != nil {
			s.t.Fatalf("a response for another request: %v", m)
		}
		r.notes = append(r.notes, m)
	}
}

func (s *server) call(method string, params any) reply {
	s.t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		s.t.Fatal(err)
	}
	id := s.rid()
	s.sendRaw(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":%q,"params":%s}`, id, method, raw))
	return s.await(id)
}

func (s *server) mustCall(method string, params any) reply {
	s.t.Helper()
	r := s.call(method, params)
	if r.m["error"] != nil {
		s.t.Fatalf("%s: %v", method, r.m["error"])
	}
	return r
}

// events reads n event/recorded notifications that follow a response.
func (s *server) events(n int) []protocol.Event {
	s.t.Helper()
	var out []protocol.Event
	for i := 0; i < n; i++ {
		m := s.readLine()
		if m["method"] != "event/recorded" || m["id"] != nil {
			s.t.Fatalf("expected an event notification: %v", m)
		}
		raw, _ := json.Marshal(m["params"])
		ev, err := protocol.Decode[protocol.Event](raw)
		if err != nil {
			s.t.Fatalf("event notification is not a valid Event: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

func (s *server) wait() error {
	s.t.Helper()
	select {
	case err := <-s.exited:
		return err
	case <-time.After(20 * time.Second):
		s.t.Fatal("the server did not exit")
	}
	return nil
}

type fixture struct {
	l *harnesstest.Layout
	// fake and gw are the model the Gateway double serves and the double itself; measuring
	// is signalled each time a count reaches it (a held fixture).
	fake      *harnesstest.Fake
	gw        *harnesstest.Gateway
	measuring chan struct{}
}

// newFixture is a deployment whose Gateway is nothing at all: its address is a loopback
// port that nothing listens on, which is what a Gateway that is down looks like.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	l := harnesstest.NewLayout(t, harnesstest.Options{})
	code, out, errs := runCLI(t, "init", "--config", l.Config, "--data-root", l.Data)
	if code != 0 {
		t.Fatalf("init exited %d\n%s\n%s", code, out, errs)
	}
	return &fixture{l: l}
}

// newGatewayFixture is a deployment whose configuration names a Gateway double that
// serves the fake model over HTTP: the real binary, the real client and the strict wire,
// in a loopback test. With hold, the double never answers a count: a Run it admits stays
// in Measuring until it is stopped, which is what the tests of a Run that is in progress
// need.
func newGatewayFixture(t *testing.T, fake *harnesstest.Fake, hold bool) *fixture {
	t.Helper()
	return newGatewayFixtureWith(t, fake, hold, nil)
}

// newGatewayFixtureWith is newGatewayFixture with the deployment's documents edited before
// they are written (before `init`).
func newGatewayFixtureWith(t *testing.T, fake *harnesstest.Fake, hold bool, edit func(l *harnesstest.Layout)) *fixture {
	t.Helper()
	l := harnesstest.NewLayout(t, harnesstest.Options{})
	if edit != nil {
		edit(l)
	}
	f := &fixture{l: l, fake: fake, measuring: make(chan struct{}, 16)}
	if hold {
		fake.OnMeasure = func(ctx context.Context) {
			select {
			case f.measuring <- struct{}{}:
			default:
			}
			<-ctx.Done()
		}
	}
	binding := protocol.Binding{Kind: "model_route", Selector: "example-local-model--example-host", ProfileRevision: "fixture-profile-1"}
	b := l.Binding()
	binding.Kind, binding.Selector, binding.ProfileRevision = b["kind"].(string), b["selector"].(string), b["profile_revision"].(string)
	f.gw = harnesstest.NewGateway(t, fake, binding)
	l.Cfg["gateway"].(map[string]any)["base_url"] = f.gw.BaseURL()
	l.Write()
	if code, out, errs := runCLI(t, "init", "--config", l.Config, "--data-root", l.Data); code != 0 {
		t.Fatalf("init exited %d\n%s\n%s", code, out, errs)
	}
	return f
}

// waitMeasuring waits until a count reached the held Gateway.
func (f *fixture) waitMeasuring(t *testing.T) {
	t.Helper()
	select {
	case <-f.measuring:
	case <-time.After(20 * time.Second):
		t.Fatal("no count reached the Gateway")
	}
}

func (f *fixture) initialize(s *server) {
	s.t.Helper()
	r := s.mustCall("initialize", protocol.InitializeInput{ClientName: "e2e", ClientVersion: "1", ProtocolVersion: protocol.ProtocolVersion})
	if r.result()["protocol_version"] != protocol.ProtocolVersion {
		s.t.Fatalf("%v", r.m)
	}
}

func (f *fixture) openParams(key string) map[string]any {
	return map[string]any{
		"workspace_path": f.l.Work, "binding": f.l.Binding(), "policy_ref": f.l.WorkspacePolicy(),
		"execution_mode": "trusted_host", "idempotency_key": key,
	}
}

func startParams(t *testing.T, thread, key, text string) map[string]any {
	t.Helper()
	const blockText = "host policy decides what may run"
	rev, err := protocol.ContextRevision(protocol.KindStableRuntimeContext, blockText, nil)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"thread_id": thread, "input": map[string]any{"text": text, "origin_proof": nil},
		"context_blocks": []any{map[string]any{"kind": "stable_runtime_context", "text": blockText, "revision": rev, "source": nil}},
		"upstream":       nil, "expected_context_revision": 0, "expected_control_revision": 0, "idempotency_key": key,
		"limits": map[string]any{"max_model_steps": 10, "max_tool_calls_per_step": 8, "deadline_seconds": 1800, "max_capture_bytes": 67108864, "max_generation_attempts": 32},
	}
}

func TestEndToEndOverStdio(t *testing.T) {
	// The Run this test admits is held in Measuring by the Gateway double, so that it is
	// the active Run of its Thread for as long as the test needs one.
	f := newGatewayFixture(t, harnesstest.NewFake(), true)
	s := startServe(t, f.l.Config)
	f.initialize(s)

	caps := s.mustCall("service/capabilities", map[string]any{})
	status := map[string]string{}
	for _, c := range caps.result()["capabilities"].([]any) {
		c := c.(map[string]any)
		status[c["name"].(string)] = c["status"].(string)
	}
	if status["turn/start"] != "ready" || status["turn.execution"] != "ready" || status["model.generation"] != "ready" || status["input/append"] != "ready" || status["turn/interrupt"] != "ready" || status["run/resume"] != "ready" ||
		status["session/fork"] != "ready" || status["context/compact"] != "ready" {
		t.Fatalf("capabilities %v", status)
	}

	// session/open: the response, then the committed event.
	open := s.mustCall("session/open", f.openParams("e2e.open.0000000000001"))
	session := open.result()["session"].(map[string]any)
	thread := session["thread_id"].(string)
	opened := s.events(1)
	if opened[0].Type != protocol.EventSessionCreated || opened[0].ThreadID != thread || opened[0].EventSeq != 1 {
		t.Fatalf("%+v", opened)
	}

	// turn/start: the acceptance, then the Run is driven: its input is applied, and it
	// stops where the Gateway double holds it, in the count.
	const text = "この隔離workspaceのテストを修正する"
	params := startParams(t, thread, "e2e.start.000000000001", text)
	start := s.mustCall("turn/start", params)
	started := s.events(4)
	var types []string
	for _, e := range started {
		types = append(types, e.Type)
	}
	if strings.Join(types, ",") != "input.accepted,task.created,run.started,input.applied" || started[0].EventSeq != 2 || started[2].EventSeq != 4 || started[3].EventSeq != 5 {
		t.Fatalf("events %v", started)
	}
	f.waitMeasuring(t)
	sr := start.result()
	runID, receiptID := sr["run_id"].(string), sr["receipt_id"].(string)
	intake := sr["intake"].(map[string]any)
	if sr["accepted"] != true || intake["entrypoint"] != "stdio_automation" || intake["effective_origin"] != "automation" || intake["proof_basis"] != "automation" {
		t.Fatalf("%v", sr)
	}

	// The same key and the same payload: the original answer, and nothing announced.
	again := s.mustCall("turn/start", params)
	if len(again.notes) != 0 {
		t.Fatalf("a replay announced events: %v", again.notes)
	}
	if !jsonEqual(t, again.result(), sr) {
		t.Fatalf("a replay must return the original result:\n%v\n%v", again.result(), sr)
	}
	// The same key with another payload is a conflict, and creates nothing.
	changed := startParams(t, thread, "e2e.start.000000000001", text+" さらに")
	conflict := s.call("turn/start", changed)
	if code, info := conflict.errCode(t); code != -32000 || info != protocol.CodeIdempotencyConflict {
		t.Fatalf("%v", conflict.m)
	}
	// A new key while the Run is active: BUSY.
	busy := s.call("turn/start", startParams(t, thread, "e2e.start.000000000002", "next"))
	if code, info := busy.errCode(t); code != -32000 || info != protocol.CodeBusy {
		t.Fatalf("%v", busy.m)
	}
	if len(conflict.notes)+len(busy.notes) != 0 {
		t.Fatal("a refusal announced events")
	}

	// receipt/get returns the same operation result.
	rec := s.mustCall("receipt/get", map[string]any{"receipt_id": receiptID}).result()
	if rec["operation"] != "turn/start" || rec["stage"] != "accepted" {
		t.Fatalf("%v", rec)
	}
	if got := rec["result"].(map[string]any); got["type"] != "StartResult" || !jsonEqual(t, got["value"], sr) {
		t.Fatalf("%v", got)
	}

	// events/read returns exactly the events that were announced.
	read := s.mustCall("events/read", map[string]any{"thread_id": thread, "after_seq": 0, "limit": 100}).result()
	evs := read["events"].([]any)
	if len(evs) != 5 || read["has_more"] != false || read["next_after_seq"].(json.Number).String() != "5" {
		t.Fatalf("%v", read)
	}
	announced := append(append([]protocol.Event{}, opened...), started...)
	for i, e := range evs {
		if e.(map[string]any)["event_id"] != announced[i].EventID {
			t.Fatalf("event %d differs between the notification and events/read", i)
		}
	}
	page := s.mustCall("events/read", map[string]any{"thread_id": thread, "after_seq": 2, "limit": 1}).result()
	if len(page["events"].([]any)) != 1 || page["has_more"] != true || page["next_after_seq"].(json.Number).String() != "3" {
		t.Fatalf("%v", page)
	}

	// run/get: the Run is being driven and is held in the count.
	run := s.mustCall("run/get", map[string]any{"run_id": runID}).result()
	if run["phase"] != "Measuring" || run["terminal"] != false || run["result"] != nil || run["thread_id"] != thread {
		t.Fatalf("%v", run)
	}

	// evidence/read: stored bytes, hashes of the whole.
	evidenceID := intake["evidence_id"].(string)
	ev := s.mustCall("evidence/read", map[string]any{"evidence_id": evidenceID, "projection_version": "text/v1", "range": map[string]any{"start": 0, "end": len(text)}}).result()
	data, err := base64.StdEncoding.DecodeString(ev["data_base64"].(string))
	if err != nil || string(data) != text || ev["partial"] != false || ev["capture_complete"] != true {
		t.Fatalf("%v %v", err, ev)
	}
	cut := s.call("evidence/read", map[string]any{"evidence_id": evidenceID, "projection_version": "text/v1", "range": map[string]any{"start": 1, "end": 3}})
	if code, info := cut.errCode(t); code != -32000 || info != protocol.CodeInvalidRange {
		t.Fatalf("%v", cut.m)
	}

	// input/append stores one more input of the running Run, classified and durable; the Run
	// is held in its count, so nothing applies it before the Run is stopped.
	appendParams := map[string]any{"thread_id": thread, "run_id": runID, "input": map[string]any{"text": "more", "origin_proof": nil},
		"disposition": "next_step", "expected_control_revision": 0, "idempotency_key": "e2e.append.00000000001"}
	appended := s.mustCall("input/append", appendParams).result()
	if appended["disposition"] != "next_step" || appended["delivery_state"] != "queued" || appended["origin"] != "automation" ||
		appended["queue_revision"].(json.Number).String() != "1" || appended["control_revision"].(json.Number).String() != "0" ||
		!strings.HasPrefix(appended["queue_item_id"].(string), "qit_") {
		t.Fatalf("%v", appended)
	}
	if again := s.mustCall("input/append", appendParams).result(); !jsonEqual(t, again, appended) {
		t.Fatal("a replay of input/append must answer from its receipt")
	}
	if got := s.mustCall("events/read", map[string]any{"thread_id": thread, "after_seq": 0, "limit": 100}).result()["events"].([]any); len(got) != 6 ||
		got[5].(map[string]any)["type"] != "input.accepted" {
		t.Fatalf("an append is one input.accepted event, once: %v", got)
	}

	// session/fork is a method of this build: a checkpoint the Thread does not have is the
	// caller's mistake, said as such, and nothing is written.
	missing := s.call("session/fork", map[string]any{"thread_id": thread, "checkpoint_id": "ckp_00000000-0000-7000-8000-000000000001", "idempotency_key": "e2e.fork.000000000001"})
	if code, info := missing.errCode(t); code != -32600 || info != protocol.CodeInvalidRequest {
		t.Fatalf("%v", missing.m)
	}

	// service/shutdown ends the process cleanly after the response; the Run that was held
	// is stopped with its driver and records how it ended.
	down := s.mustCall("service/shutdown", map[string]any{"mode": "drain", "deadline_seconds": 1})
	if down.result()["accepted"] != true {
		t.Fatalf("%v", down.m)
	}
	if err := s.wait(); err != nil {
		t.Fatalf("exit: %v\nstderr: %s", err, s.stderr.String())
	}
	if _, ok := <-s.lines; ok {
		t.Fatal("output after the shutdown response")
	}
	stderr := s.stderr.String()
	if strings.Contains(stderr, f.l.Dir) || strings.Contains(stderr, "テスト") || strings.Contains(stderr, "e2e.start") {
		t.Fatalf("stderr carries a path or request content: %q", stderr)
	}

	// Everything is durable and readable by the CLI afterwards, from another process.
	code, out, errs := runCLI(t, "sessions", "list", "--config", f.l.Config)
	if code != 0 || !strings.Contains(out, thread) || errs != "" {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	code, out, _ = runCLI(t, "inspect", "--config", f.l.Config, "--run", runID, "--json")
	if code != 0 || !strings.Contains(out, `"phase":"Terminal"`) || !strings.Contains(out, `"code":"DRIVER_STOPPED"`) {
		t.Fatalf("%d %q", code, out)
	}
	code, out, _ = runCLI(t, "evidence", "--config", f.l.Config, "--id", evidenceID, "--start", "0", "--end", fmt.Sprint(len(text)))
	if code != 0 || out != text {
		t.Fatalf("%d %q", code, out)
	}
	// The locks were released with the process: a new server starts and answers.
	s2 := startServe(t, f.l.Config)
	f.initialize(s2)
	if again := s2.mustCall("turn/start", params); !jsonEqual(t, again.result(), sr) {
		t.Fatal("a restarted server must answer an accepted request from its receipt")
	}
	_ = s2.stdin.Close()
	if err := s2.wait(); err != nil {
		t.Fatalf("EOF must end the server cleanly: %v", err)
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	var va, vb any
	if json.Unmarshal(x, &va) != nil || json.Unmarshal(y, &vb) != nil {
		return false
	}
	nx, _ := json.Marshal(va)
	ny, _ := json.Marshal(vb)
	return bytes.Equal(nx, ny)
}

func TestHandshakeAndMalformedFramesOverStdio(t *testing.T) {
	f := newFixture(t)
	s := startServe(t, f.l.Config)

	// Nothing works before initialize.
	pre := s.call("session/list", map[string]any{"cursor": nil, "limit": 10})
	if code, info := pre.errCode(t); code != -32600 || info != protocol.CodeInvalidRequest {
		t.Fatalf("%v", pre.m)
	}
	// An unsupported protocol version is refused and the handshake stays open.
	bad := s.call("initialize", map[string]any{"client_name": "e2e", "client_version": "1", "protocol_version": "rencrow-harness/v2"})
	if code, _ := bad.errCode(t); code != -32602 {
		t.Fatalf("%v", bad.m)
	}
	f.initialize(s)
	twice := s.call("initialize", map[string]any{"client_name": "e2e", "client_version": "1", "protocol_version": protocol.ProtocolVersion})
	if code, info := twice.errCode(t); code != -32600 || info != protocol.CodeInvalidRequest {
		t.Fatalf("%v", twice.m)
	}

	rid := s.rid()
	good := fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"service/capabilities","params":{}}`, rid)
	expect := func(name, frame string, code int, wantID any) {
		t.Helper()
		s.sendRaw(frame)
		m := s.readLine()
		e, ok := m["error"].(map[string]any)
		if !ok {
			t.Fatalf("%s: %v", name, m)
		}
		if n, _ := e["code"].(json.Number).Int64(); int(n) != code || m["id"] != wantID {
			t.Fatalf("%s: %v", name, m)
		}
	}
	expect("invalid UTF-8", "{\"jsonrpc\":\"2.0\",\"id\":\"req_\xff\",\"method\":\"initialize\",\"params\":{}}", -32700, nil)
	expect("duplicate key", fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"service/capabilities","params":{},"params":{}}`, rid), -32700, nil)
	expect("trailing value", good+` {}`, -32700, nil)
	expect("depth over 64", strings.Repeat(`{"a":`, 70)+"1"+strings.Repeat("}", 70), -32700, nil)
	expect("batch", "["+good+"]", -32600, nil)
	expect("unknown method", fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"turn/stop","params":{}}`, rid), -32601, rid)
	expect("unknown field in params", fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"service/capabilities","params":{"x":1}}`, rid), -32602, rid)

	// The frame limit is 16 MiB: the line itself, without its newline. A frame
	// exactly at the limit is read (and, being padded, refused as a request); one
	// byte more is refused unread.
	pad := func(total int) string {
		head := fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"service/capabilities","params":{},"pad":"`, rid)
		return head + strings.Repeat("x", total-len(head)-2) + `"}`
	}
	const limit = 16 << 20
	exact := pad(limit)
	if len(exact) != limit {
		t.Fatalf("test frame is %d bytes", len(exact))
	}
	expect("frame at the limit", exact, -32600, rid)
	expect("frame one byte over the limit", pad(limit+1), -32700, nil)

	// None of that reached the Service: the store holds nothing, and the server still answers.
	if r := s.mustCall("session/list", map[string]any{"cursor": nil, "limit": 10}); len(r.result()["sessions"].([]any)) != 0 {
		t.Fatalf("%v", r.m)
	}
	if code, out, _ := runCLI(t, "sessions", "list", "--config", f.l.Config); code != 0 || out != "" {
		t.Fatalf("a rejected frame left something in the store: %d %q", code, out)
	}
	_ = s.stdin.Close()
	if err := s.wait(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.stderr.String(), "xxxxxxxx") || strings.Contains(s.stderr.String(), f.l.Dir) {
		t.Fatal("stderr repeats frame content or a path")
	}
}

func TestASecondProcessCannotDriveAThreadAndTheLockEndsWithItsHolder(t *testing.T) {
	f := newGatewayFixture(t, harnesstest.NewFake(), true)
	a := startServe(t, f.l.Config)
	f.initialize(a)
	open := a.mustCall("session/open", f.openParams("e2e.dual.open.0000001"))
	a.events(1)
	thread := open.result()["session"].(map[string]any)["thread_id"].(string)
	params := startParams(t, thread, "e2e.dual.start.000001", "first")
	first := a.mustCall("turn/start", params)
	a.events(4)
	f.waitMeasuring(t)

	b := startServe(t, f.l.Config)
	f.initialize(b)
	// B cannot take the writer role while A holds it: BUSY, said as such, nothing written.
	refused := b.call("turn/start", startParams(t, thread, "e2e.dual.start.000002", "second"))
	if code, info := refused.errCode(t); code != -32000 || info != protocol.CodeBusy {
		t.Fatalf("%v", refused.m)
	}
	if msg := refused.m["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "another process") {
		t.Fatalf("the refusal must name the cause: %s", msg)
	}
	// B can still tell the client what became of a request A already accepted.
	if replay := b.mustCall("turn/start", params); !jsonEqual(t, replay.result(), first.result()) {
		t.Fatal("a replay must be answered from the receipt without the writer role")
	}
	if sess := b.mustCall("session/get", map[string]any{"thread_id": thread}).result(); sess["active_run_id"] != first.result()["run_id"] {
		t.Fatalf("%v", sess)
	}

	// A dies without a word. The OS drops its lock; nothing else does, and nothing
	// takes the thread over by itself. Whoever takes the writer role next settles the
	// Run A left running (it was being driven, held in its count: nothing was dispatched),
	// in the transaction that raises the epoch, and only then admits the new request,
	// so the Thread is not busy for ever.
	if err := a.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = a.wait()
	deadline := time.Now().Add(10 * time.Second)
	var third reply
	// The first Run's input was applied to the Thread's context while it was driven, so the
	// next request expects the revision the Thread has now.
	thirdParams := startParams(t, thread, "e2e.dual.start.000003", "third")
	thirdParams["expected_context_revision"] = b.mustCall("session/get", map[string]any{"thread_id": thread}).result()["context_revision"]
	for {
		r := b.call("turn/start", thirdParams)
		if r.m["error"] == nil {
			third = r
			break
		}
		code, info := r.errCode(t)
		msg := r.m["error"].(map[string]any)["message"].(string)
		if code != -32000 || info != protocol.CodeBusy || !strings.Contains(msg, "another process") {
			t.Fatalf("expected BUSY while the dead holder's lock is still held, got %v", r.m)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the dead holder's lock never ended: %v", r.m)
		}
		time.Sleep(50 * time.Millisecond)
	}
	evs := b.events(4)
	if evs[0].Type != protocol.EventRunTerminal || evs[1].Type != protocol.EventInputAccepted || evs[2].Type != protocol.EventTaskCreated || evs[3].Type != protocol.EventRunStarted {
		t.Fatalf("the settled Run's terminal event must precede the new admission: %v", []string{evs[0].Type, evs[1].Type, evs[2].Type, evs[3].Type})
	}
	if evs[0].RunID == nil || *evs[0].RunID != first.result()["run_id"] {
		t.Fatalf("run.terminal is not the first Run's: %+v", evs[0])
	}
	old := b.mustCall("run/get", map[string]any{"run_id": first.result()["run_id"]}).result()
	res, _ := old["result"].(map[string]any)
	if old["phase"] != "Terminal" || old["terminal"] != true || res == nil || res["status"] != "incomplete" || res["code"] != "DRIVER_STOPPED" ||
		res["resumable"] != true || res["final_text"] != "" || res["final_message_id"] != nil {
		t.Fatalf("the Run A left was not settled as stopped with its driver: %v", old)
	}
	if sess := b.mustCall("session/get", map[string]any{"thread_id": thread}).result(); sess["active_run_id"] != third.result()["run_id"] {
		t.Fatalf("the Thread's active Run is not the new one: %v", sess)
	}
	_ = b.stdin.Close()
	_ = b.wait()
}

func TestInitAndUsageOverTheRealBinary(t *testing.T) {
	l := harnesstest.NewLayout(t, harnesstest.Options{})
	code, out, errs := runCLI(t, "init", "--config", l.Config, "--data-root", l.Data)
	if code != 0 || !strings.Contains(out, "initialized") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	if code, _, _ := runCLI(t, "init", "--config", l.Config, "--data-root", l.Data); code != 1 {
		t.Fatalf("a second init exits %d, want 1", code)
	}
	for _, args := range [][]string{{}, {"nonsense"}, {"serve", "--config", l.Config}, {"exec", "--config", l.Config, "--json"}, {"chat"}, {"resume"}, {"compact"}} {
		if code, out, errs := runCLI(t, args...); code != 64 || out != "" || errs == "" {
			t.Errorf("%v: %d %q %q", args, code, out, errs)
		}
	}
	// A server that is told to stop by a signal exits through the same path as EOF.
	s := startServe(t, l.Config)
	f := &fixture{l: l}
	f.initialize(s)
	if err := s.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Skip("signals are not available")
	}
	if err := s.wait(); err != nil {
		t.Fatalf("an interrupted server must flush and exit 0: %v\n%s", err, s.stderr.String())
	}
}

// nativeExamples returns the 16 single-schema example requests of the design package,
// in the order of the API table: one valid request per method.
func nativeExamples(t *testing.T) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(harnesstest.ModuleRoot(t), "testdata", "contract", "examples", "native_requests.json"))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var reqs []map[string]any
	if err := dec.Decode(&reqs); err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 16 {
		t.Fatalf("expected one example per method, got %d", len(reqs))
	}
	return reqs
}

func TestAll16MethodsAreStrictlyTypedOverStdio(t *testing.T) {
	f := newFixture(t)

	// Valid examples: each is accepted by the envelope and the typed params (no
	// -32700/-32600/-32601/-32602). What they then answer depends on the empty store.
	type outcome struct {
		result bool
		code   string
	}
	want := map[string]outcome{
		"initialize": {result: true}, "service/capabilities": {result: true}, "session/list": {result: true},
		"session/open": {code: protocol.CodeForbidden}, "session/get": {code: protocol.CodeForbidden}, "turn/start": {code: protocol.CodeForbidden},
		"run/get": {code: protocol.CodeForbidden}, "receipt/get": {code: protocol.CodeForbidden}, "events/read": {code: protocol.CodeForbidden},
		"evidence/read": {code: protocol.CodeForbidden},
		// A Thread, Run or Task that does not exist is the same FORBIDDEN as one the caller cannot read.
		"input/append": {code: protocol.CodeForbidden}, "turn/interrupt": {code: protocol.CodeForbidden}, "run/resume": {code: protocol.CodeForbidden},
		"session/fork": {code: protocol.CodeForbidden}, "context/compact": {code: protocol.CodeForbidden}, "service/shutdown": {result: true},
	}
	s := startServe(t, f.l.Config)
	for _, req := range nativeExamples(t) {
		method := req["method"].(string)
		raw, _ := json.Marshal(req)
		s.sendRaw(string(raw))
		r := s.await(req["id"].(string))
		w := want[method]
		if w.result {
			if r.m["result"] == nil {
				t.Errorf("%s: %v", method, r.m)
			}
			continue
		}
		code, info := r.errCode(t)
		if code != -32000 || info != w.code {
			t.Errorf("%s: %v", method, r.m)
		}
	}
	if err := s.wait(); err != nil {
		t.Fatalf("the shutdown example must end the server cleanly: %v", err)
	}

	// Mutated examples: an unknown field, and a string field of the wrong type, are
	// both refused as invalid params, before anything is looked up or written.
	m := startServe(t, f.l.Config)
	examples := nativeExamples(t)
	extra := func(req map[string]any) map[string]any {
		out := cloneRequest(req)
		out["params"].(map[string]any)["unknown_field"] = 1
		return out
	}
	initReq := extra(examples[0])
	m.sendRaw(mustMarshal(t, initReq))
	r := m.await(initReq["id"].(string))
	if code, _ := r.errCode(t); code != -32602 {
		t.Fatalf("initialize with an unknown field: %v", r.m)
	}
	f.initialize(m)
	for _, req := range examples[1 : len(examples)-1] { // everything but initialize and shutdown
		method := req["method"].(string)
		bad := extra(req)
		m.sendRaw(mustMarshal(t, bad))
		if code, _ := m.await(bad["id"].(string)).errCode(t); code != -32602 {
			t.Errorf("%s with an unknown field: code %d", method, code)
		}
		wrong := cloneRequest(req)
		changed := false
		for k, v := range wrong["params"].(map[string]any) {
			if _, ok := v.(string); ok {
				wrong["params"].(map[string]any)[k] = 12345
				changed = true
				break
			}
		}
		if !changed {
			continue
		}
		m.sendRaw(mustMarshal(t, wrong))
		if code, _ := m.await(wrong["id"].(string)).errCode(t); code != -32602 {
			t.Errorf("%s with a field of the wrong type: code %d", method, code)
		}
	}
	// Nothing above reached the store.
	if code, out, _ := runCLI(t, "sessions", "list", "--config", f.l.Config); code != 0 || out != "" {
		t.Fatalf("%d %q", code, out)
	}
}

func cloneRequest(req map[string]any) map[string]any {
	raw, _ := json.Marshal(req)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	_ = dec.Decode(&out)
	return out
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
