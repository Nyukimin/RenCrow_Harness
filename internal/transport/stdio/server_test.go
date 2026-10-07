package stdio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/service"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func rid(n int) string { return fmt.Sprintf("req_00000000-0000-7000-8000-%012x", n) }

func capsRequest(n int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"service/capabilities","params":{}}`, rid(n))
}

// fakeBackend answers every known method with {"ok":true} unless told otherwise.
type fakeBackend struct {
	mu       sync.Mutex
	handled  int
	handle   func(ctx context.Context, req protocol.Request) (service.Result, error)
	notifier service.Notifier
}

func (f *fakeBackend) HasMethod(m string) bool { return service.HasMethod(m) }

func (f *fakeBackend) NewConn(n service.Notifier) *service.Conn {
	f.mu.Lock()
	f.notifier = n
	f.mu.Unlock()
	return (*service.Service)(nil).NewConn(n)
}

// Handle validates the params first, as the Service does; only a request whose
// params are valid counts as handled.
func (f *fakeBackend) Handle(ctx context.Context, _ *service.Conn, req protocol.Request) (service.Result, error) {
	if _, err := req.DecodeParams(); err != nil {
		return service.Result{}, err
	}
	f.mu.Lock()
	f.handled++
	h := f.handle
	f.mu.Unlock()
	if h != nil {
		return h(ctx, req)
	}
	return service.Result{Body: []byte(`{"ok":true}`)}, nil
}

func (f *fakeBackend) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handled
}

func (f *fakeBackend) progress() service.Notifier {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notifier
}

// peer is a client wired to a running Serve through pipes.
type peer struct {
	t      *testing.T
	be     *fakeBackend
	in     *io.PipeWriter
	outR   *io.PipeReader
	lines  chan string
	done   chan error
	cancel context.CancelFunc
	diag   *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func startPeer(t *testing.T, be *fakeBackend, opts Options) *peer {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	p := &peer{t: t, be: be, in: inW, outR: outR, lines: make(chan string, 4096), done: make(chan error, 1), cancel: cancel, diag: &syncBuffer{}}
	opts.Diag = p.diag
	go func() {
		err := Serve(ctx, be, inR, outW, opts)
		_ = outW.Close()
		p.done <- err
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<26)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
	}()
	t.Cleanup(func() { cancel(); _ = inW.Close(); _ = outR.Close() })
	return p
}

func (p *peer) send(frame string) {
	p.t.Helper()
	if _, err := io.WriteString(p.in, frame+"\n"); err != nil {
		p.t.Fatalf("send: %v", err)
	}
}

func (p *peer) next() string {
	p.t.Helper()
	select {
	case l, ok := <-p.lines:
		if !ok {
			p.t.Fatal("the server closed its output")
		}
		assertProtocolLine(p.t, l)
		return l
	case <-time.After(5 * time.Second):
		p.t.Fatal("no output from the server")
	}
	return ""
}

func (p *peer) nextObject() map[string]any {
	p.t.Helper()
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(p.next()))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		p.t.Fatal(err)
	}
	return m
}

// assertProtocolLine checks that stdout carries only JSON-RPC objects.
func assertProtocolLine(t *testing.T, line string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil || m["jsonrpc"] != "2.0" {
		t.Fatalf("stdout carried something that is not a JSON-RPC object: %q (%v)", line, err)
	}
}

func (p *peer) wantNoLine() {
	p.t.Helper()
	select {
	case l, ok := <-p.lines:
		if ok {
			p.t.Fatalf("unexpected output %q", l)
		}
	case <-time.After(150 * time.Millisecond):
	}
}

func errorOf(t *testing.T, m map[string]any) (code int, data map[string]any) {
	t.Helper()
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("not an error response: %v", m)
	}
	n, _ := e["code"].(json.Number).Int64()
	d, _ := e["data"].(map[string]any)
	return int(n), d
}

func TestServeAnswersWithTheRequestsOwnID(t *testing.T) {
	p := startPeer(t, &fakeBackend{}, Options{})
	p.send(capsRequest(1))
	m := p.nextObject()
	if m["id"] != rid(1) || m["jsonrpc"] != "2.0" || m["error"] != nil {
		t.Fatalf("%v", m)
	}
	if r, ok := m["result"].(map[string]any); !ok || r["ok"] != true {
		t.Fatalf("%v", m)
	}
	// A resend is a new communication: it gets its own id back.
	p.send(capsRequest(2))
	if m := p.nextObject(); m["id"] != rid(2) {
		t.Fatalf("%v", m)
	}
}

func TestUnknownMethodIsMinus32601AndNeverReachesTheService(t *testing.T) {
	be := &fakeBackend{}
	p := startPeer(t, be, Options{})
	for _, method := range []string{"turn/stop", "Initialize", "session/open ", "", "rpc.discover"} {
		p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":%q,"params":{}}`, rid(1), method))
		m := p.nextObject()
		code, _ := errorOf(t, m)
		if code != -32601 || m["id"] != rid(1) {
			t.Fatalf("%q: %v", method, m)
		}
	}
	if be.count() != 0 {
		t.Fatal("an unknown method reached the service")
	}
}

func TestParseFailuresAreMinus32700WithANullIDAndTheConnectionSurvives(t *testing.T) {
	be := &fakeBackend{}
	p := startPeer(t, be, Options{})
	deep := strings.Repeat(`{"a":`, 70) + "1" + strings.Repeat("}", 70)
	frames := map[string]string{
		"invalid UTF-8":      "{\"jsonrpc\":\"2.0\",\"id\":\"x\xff\",\"method\":\"initialize\",\"params\":{}}",
		"duplicate key":      fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"service/capabilities","params":{},"params":{}}`, rid(1)),
		"trailing value":     capsRequest(1) + ` {}`,
		"trailing garbage":   capsRequest(1) + `x`,
		"depth over 64":      deep,
		"truncated":          `{"jsonrpc":"2.0","id":`,
		"not JSON":           `hello`,
		"BOM":                "\xef\xbb\xbf" + capsRequest(1),
		"unpaired surrogate": `{"jsonrpc":"2.0","id":"\ud800","method":"initialize","params":{}}`,
		"huge number":        `{"jsonrpc":"2.0","id":"x","method":"initialize","params":{"n":1e999999}}`,
	}
	for name, frame := range frames {
		p.send(frame)
		m := p.nextObject()
		code, _ := errorOf(t, m)
		if code != -32700 || m["id"] != nil {
			t.Errorf("%s: %v", name, m)
		}
	}
	if be.count() != 0 {
		t.Fatal("a frame that does not parse reached the service")
	}
	p.send(capsRequest(9))
	if m := p.nextObject(); m["id"] != rid(9) || m["error"] != nil {
		t.Fatalf("the connection must survive a bad frame: %v", m)
	}
}

func TestNestingLimitIs64LevelsIncludingTheRequestObject(t *testing.T) {
	be := &fakeBackend{}
	p := startPeer(t, be, Options{})
	nested := func(levels int) string { // params is the first of the levels
		return strings.Repeat(`{"a":`, levels-1) + `{}` + strings.Repeat("}", levels-1)
	}
	frame := func(paramsLevels int) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"service/capabilities","params":%s}`, rid(1), nested(paramsLevels))
	}
	// The request object is level 1, so params may nest 63 levels more: 64 in total.
	p.send(frame(63))
	m := p.nextObject()
	if code, data := errorOf(t, m); code != -32602 || data["code"] != protocol.CodeInvalidParams || m["id"] != rid(1) {
		t.Fatalf("64 levels parse and are then refused as params: %v", m)
	}
	p.send(frame(64))
	m = p.nextObject()
	if code, _ := errorOf(t, m); code != -32700 || m["id"] != nil {
		t.Fatalf("65 levels do not parse: %v", m)
	}
}

func TestInvalidRequestsAreMinus32600(t *testing.T) {
	be := &fakeBackend{}
	p := startPeer(t, be, Options{})
	good := capsRequest(1)
	cases := []struct {
		name, frame string
		wantID      any
	}{
		{"batch", "[" + good + "," + capsRequest(2) + "]", nil},
		{"empty batch", `[]`, nil},
		{"number", `1`, nil},
		{"string", `"x"`, nil},
		{"null", `null`, nil},
		{"empty object", `{}`, nil},
		{"notification (no id)", `{"jsonrpc":"2.0","method":"service/capabilities","params":{}}`, nil},
		{"numeric id", `{"jsonrpc":"2.0","id":1,"method":"service/capabilities","params":{}}`, nil},
		{"null id", `{"jsonrpc":"2.0","id":null,"method":"service/capabilities","params":{}}`, nil},
		{"id that is not a request id", `{"jsonrpc":"2.0","id":"abc","method":"service/capabilities","params":{}}`, nil},
		{"a thread id as the id", `{"jsonrpc":"2.0","id":"thr_00000000-0000-7000-8000-000000000001","method":"service/capabilities","params":{}}`, nil},
		{"wrong version", fmt.Sprintf(`{"jsonrpc":"1.0","id":%q,"method":"service/capabilities","params":{}}`, rid(3)), rid(3)},
		{"no version", fmt.Sprintf(`{"id":%q,"method":"service/capabilities","params":{}}`, rid(3)), rid(3)},
		{"no method", fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"params":{}}`, rid(3)), rid(3)},
		{"method is a number", fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":1,"params":{}}`, rid(3)), rid(3)},
		{"extra member", fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"service/capabilities","params":{},"extra":1}`, rid(3)), rid(3)},
	}
	for _, c := range cases {
		p.send(c.frame)
		m := p.nextObject()
		code, _ := errorOf(t, m)
		if code != -32600 || m["id"] != c.wantID {
			t.Errorf("%s: %v", c.name, m)
		}
	}
	if be.count() != 0 {
		t.Fatal("an invalid request reached the service")
	}
}

func TestInvalidParamsAreMinus32602(t *testing.T) {
	be := &fakeBackend{}
	p := startPeer(t, be, Options{})
	for name, frame := range map[string]string{
		"params missing":      fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"service/capabilities"}`, rid(1)),
		"params not object":   fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"service/capabilities","params":[]}`, rid(1)),
		"unknown field":       fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"service/capabilities","params":{"x":1}}`, rid(1)),
		"wrong type":          fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"session/get","params":{"thread_id":7}}`, rid(1)),
		"unsupported version": fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"initialize","params":{"client_name":"c","client_version":"1","protocol_version":"rencrow-harness/v2"}}`, rid(1)),
		"missing field":       fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"events/read","params":{"thread_id":"thr_00000000-0000-7000-8000-000000000001"}}`, rid(1)),
	} {
		p.send(frame)
		m := p.nextObject()
		code, data := errorOf(t, m)
		if code != -32602 || m["id"] != rid(1) || data["code"] != protocol.CodeInvalidParams {
			t.Errorf("%s: %v", name, m)
		}
	}
	// A user_message block is a refused request, not a malformed one.
	p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"turn/start","params":{"thread_id":"thr_00000000-0000-7000-8000-000000000001","input":{"text":"x","origin_proof":null},`+
		`"context_blocks":[{"kind":"user_message","text":"x","revision":"r","source":null}],"upstream":null,"expected_context_revision":0,"expected_control_revision":0,`+
		`"idempotency_key":"key.0123456789abcdef","limits":{"max_model_steps":1,"max_tool_calls_per_step":1,"deadline_seconds":1,"max_capture_bytes":1,"max_generation_attempts":1}}}`, rid(2)))
	m := p.nextObject()
	if code, data := errorOf(t, m); code != -32600 || data["code"] != protocol.CodeInvalidRequest {
		t.Fatalf("%v", m)
	}
	if be.count() != 0 {
		t.Fatal("invalid params reached the service")
	}
}

func TestOversizeFrameIsRefusedWithoutReachingTheServiceAndTheNextFrameWorks(t *testing.T) {
	be := &fakeBackend{}
	p := startPeer(t, be, Options{MaxFrameBytes: 2048})
	big := fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"service/capabilities","params":{},"pad":%q}`, rid(1), strings.Repeat("x", 3000))
	p.send(big)
	m := p.nextObject()
	if code, _ := errorOf(t, m); code != -32700 || m["id"] != nil {
		t.Fatalf("%v", m)
	}
	if !strings.Contains(m["error"].(map[string]any)["message"].(string), "too large") {
		t.Fatalf("the refusal must say why: %v", m)
	}
	p.send(capsRequest(2))
	if m := p.nextObject(); m["id"] != rid(2) || m["error"] != nil {
		t.Fatalf("%v", m)
	}
	if be.count() != 1 {
		t.Fatalf("the oversize frame reached the service (%d calls)", be.count())
	}
	if strings.Contains(p.diag.String(), "xxxx") {
		t.Fatal("the diagnostic repeats frame content")
	}
}

func TestFramesAtTheLimitCrlfBlankLinesAndAFinalFrameWithoutNewline(t *testing.T) {
	be := &fakeBackend{}
	exact := capsRequest(1)
	p := startPeer(t, be, Options{MaxFrameBytes: len(exact)})
	p.send(exact)
	if m := p.nextObject(); m["id"] != rid(1) || m["error"] != nil {
		t.Fatalf("a frame exactly at the limit is valid: %v", m)
	}
	p.send("")
	p.send("   ")
	_, _ = io.WriteString(p.in, capsRequest(2)+"\r\n")
	if m := p.nextObject(); m["id"] != rid(2) || m["error"] != nil {
		t.Fatalf("CRLF framing: %v", m)
	}
	p.wantNoLine() // blank lines produce no output
	_, _ = io.WriteString(p.in, capsRequest(3))
	_ = p.in.Close()
	if m := p.nextObject(); m["id"] != rid(3) {
		t.Fatalf("a final frame without a newline is still a frame: %v", m)
	}
	if err := <-p.done; err != nil {
		t.Fatalf("EOF ends the server cleanly: %v", err)
	}
}

func TestErrorsAreMappedToJSONRPCCodesAndDataCarriesErrorInfo(t *testing.T) {
	be := &fakeBackend{}
	p := startPeer(t, be, Options{})
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantInfo string
	}{
		{"busy", protocol.NewError(protocol.CodeBusy, "another process drives this thread").AsRetryable(), -32000, protocol.CodeBusy},
		{"forbidden", protocol.NewError(protocol.CodeForbidden, "no"), -32000, protocol.CodeForbidden},
		{"idempotency", protocol.NewError(protocol.CodeIdempotencyConflict, "no"), -32000, protocol.CodeIdempotencyConflict},
		{"unsupported", protocol.NewError(protocol.CodeUnsupportedContract, "no"), -32000, protocol.CodeUnsupportedContract},
		{"invalid range", protocol.NewError(protocol.CodeInvalidRange, "no"), -32000, protocol.CodeInvalidRange},
		{"invalid limits", protocol.NewError(protocol.CodeInvalidLimits, "no"), -32000, protocol.CodeInvalidLimits},
		{"invalid origin proof", protocol.NewError(protocol.CodeInvalidOriginProof, "no"), -32000, protocol.CodeInvalidOriginProof},
		{"persistence uncertain", protocol.NewError(protocol.CodePersistenceUncertain, "no").AsRetryable(), -32000, protocol.CodePersistenceUncertain},
		{"invalid request", protocol.NewError(protocol.CodeInvalidRequest, "no"), -32600, protocol.CodeInvalidRequest},
		{"invalid params", protocol.NewError(protocol.CodeInvalidParams, "no"), -32602, protocol.CodeInvalidParams},
		{"internal", protocol.NewError(protocol.CodeInternal, "inconsistent"), -32603, protocol.CodeInternal},
		{"wrapped", fmt.Errorf("context: %w", protocol.NewError(protocol.CodeBusy, "x")), -32000, protocol.CodeBusy},
	}
	for i, c := range cases {
		c := c
		be.mu.Lock()
		be.handle = func(context.Context, protocol.Request) (service.Result, error) { return service.Result{}, c.err }
		be.mu.Unlock()
		p.send(capsRequest(i + 1))
		m := p.nextObject()
		code, data := errorOf(t, m)
		if code != c.wantCode || data["code"] != c.wantInfo || m["id"] != rid(i+1) {
			t.Errorf("%s: %v", c.name, m)
		}
		var pe *protocol.Error
		if errors.As(c.err, &pe) && data["retryable"] != pe.Retryable {
			t.Errorf("%s: retryable %v", c.name, data["retryable"])
		}
		if data["evidence_id"] != nil {
			t.Errorf("%s: evidence_id %v", c.name, data["evidence_id"])
		}
	}
}

func TestInternalFailuresAndPanicsDoNotLeakAndDoNotStopTheServer(t *testing.T) {
	be := &fakeBackend{}
	p := startPeer(t, be, Options{})
	secret := "password=hunter2 token=abcdef0123456789"
	be.mu.Lock()
	be.handle = func(context.Context, protocol.Request) (service.Result, error) {
		return service.Result{}, errors.New(secret)
	}
	be.mu.Unlock()
	p.send(capsRequest(1))
	m := p.nextObject()
	code, data := errorOf(t, m)
	if code != -32603 || data["code"] != protocol.CodeInternal || strings.Contains(m["error"].(map[string]any)["message"].(string), "hunter2") {
		t.Fatalf("%v", m)
	}
	be.mu.Lock()
	be.handle = func(context.Context, protocol.Request) (service.Result, error) { panic(secret) }
	be.mu.Unlock()
	p.send(capsRequest(2))
	m = p.nextObject()
	if code, _ := errorOf(t, m); code != -32603 {
		t.Fatalf("%v", m)
	}
	be.mu.Lock()
	be.handle = nil
	be.mu.Unlock()
	p.send(capsRequest(3))
	if m := p.nextObject(); m["id"] != rid(3) || m["error"] != nil {
		t.Fatalf("the server must survive: %v", m)
	}
	if strings.Contains(p.diag.String(), "hunter2") || strings.Contains(p.diag.String(), "abcdef0123456789") {
		t.Fatalf("stderr must carry no secret: %q", p.diag.String())
	}
}

func testEvents(t *testing.T) []protocol.Event {
	t.Helper()
	thread := identity.NewThreadID().String()
	var out []protocol.Event
	for i := int64(1); i <= 2; i++ {
		ev, err := protocol.BuildTypedEvent(protocol.EventCommon{EventID: identity.NewEventID().String(), EventSeq: i, ThreadID: thread, RecordedAt: "2026-10-07T12:00:00Z"},
			protocol.SessionCreatedPayload{SessionID: identity.NewSessionID().String(), Binding: protocol.Binding{Kind: "model_route", Selector: "s", ProfileRevision: "r"}, Mode: "structured_only"})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

func TestCommittedEventsAreAnnouncedAfterTheResponseInOrder(t *testing.T) {
	be := &fakeBackend{}
	events := testEvents(t)
	be.handle = func(context.Context, protocol.Request) (service.Result, error) {
		return service.Result{Body: []byte(`{"ok":true}`), Events: events}, nil
	}
	p := startPeer(t, be, Options{})
	p.send(capsRequest(1))
	if m := p.nextObject(); m["id"] != rid(1) || m["result"] == nil {
		t.Fatalf("the response comes first: %v", m)
	}
	for i, want := range events {
		n := p.next()
		var note struct {
			Method string         `json:"method"`
			ID     any            `json:"id"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(n), &note); err != nil || note.Method != "event/recorded" || note.ID != nil {
			t.Fatalf("event %d: %s", i, n)
		}
		raw, _ := json.Marshal(note.Params)
		got, err := protocol.Decode[protocol.Event](raw)
		if err != nil || got.EventID != want.EventID || got.EventSeq != want.EventSeq {
			t.Fatalf("event %d: %v %s", i, err, n)
		}
	}
}

func TestShutdownFlushesTheResponseThenStopsReading(t *testing.T) {
	be := &fakeBackend{}
	be.handle = func(context.Context, protocol.Request) (service.Result, error) {
		return service.Result{Body: []byte(`{"accepted":true}`), Shutdown: &service.ShutdownRequest{Mode: "drain", Deadline: 2 * time.Second}}, nil
	}
	p := startPeer(t, be, Options{})
	p.send(capsRequest(1))
	if m := p.nextObject(); m["id"] != rid(1) || m["result"] == nil {
		t.Fatalf("%v", m)
	}
	if err := <-p.done; err != nil {
		t.Fatalf("a shutdown that flushed ends cleanly: %v", err)
	}
	// Nothing after the shutdown is handled.
	go func() { _, _ = io.WriteString(p.in, capsRequest(2)+"\n") }()
	time.Sleep(100 * time.Millisecond)
	if be.count() != 1 {
		t.Fatalf("the service handled a request after shutdown (%d)", be.count())
	}
}

func TestShutdownGivesUpAtItsDeadlineWhenTheClientDoesNotRead(t *testing.T) {
	be := &fakeBackend{}
	be.handle = func(context.Context, protocol.Request) (service.Result, error) {
		return service.Result{Body: []byte(`{"accepted":true}`), Shutdown: &service.ShutdownRequest{Mode: "cancel", Deadline: 200 * time.Millisecond}}, nil
	}
	inR, inW := io.Pipe()
	g := newGated() // the client never reads
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- Serve(context.Background(), be, inR, g, Options{}) }()
	_, _ = io.WriteString(inW, capsRequest(1)+"\n")
	select {
	case err := <-done:
		if !errors.Is(err, ErrDrainTimeout) {
			t.Fatalf("%v", err)
		}
		if time.Since(start) > 3*time.Second {
			t.Fatal("the deadline was not honoured")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return at the shutdown deadline")
	}
	g.open()
}

func TestEndOfInputAndContextCancelStopTheServer(t *testing.T) {
	p := startPeer(t, &fakeBackend{}, Options{})
	p.send(capsRequest(1))
	_ = p.nextObject()
	_ = p.in.Close()
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatalf("%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EOF did not stop the server")
	}

	q := startPeer(t, &fakeBackend{}, Options{})
	q.cancel()
	select {
	case err := <-q.done:
		if err != nil {
			t.Fatalf("%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop the server")
	}
}

func TestASlowClientOverflowsTheQueueAndTheConnectionIsClosed(t *testing.T) {
	be := &fakeBackend{}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe() // nobody reads outR: the client is stuck
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), be, inR, outW, Options{PriorityMessages: 4, PriorityBytes: 1 << 20, ProgressMessages: 4})
	}()
	go func() {
		for i := 1; i <= 50; i++ {
			if _, err := io.WriteString(inW, capsRequest(i)+"\n"); err != nil {
				return
			}
		}
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrBackpressure) {
			t.Fatalf("%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server never gave up on a client that does not read")
	}
	// The connection is closed, not left half-open: the blocked output is released.
	readDone := make(chan error, 1)
	go func() { _, err := outR.Read(make([]byte, 1)); readDone <- err }()
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the output was not closed")
	}
	_ = inW.Close()
}

func TestProgressNotificationsAreSentAndLateOnesBecomeAGapWhileControlStillGoesFirst(t *testing.T) {
	be := &fakeBackend{}
	inR, inW := io.Pipe()
	g := newGated()
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), be, inR, g, Options{PriorityMessages: 16, PriorityBytes: 1 << 20, ProgressMessages: 3})
	}()
	send := func(n int) { _, _ = io.WriteString(inW, capsRequest(n)+"\n") }

	send(1) // its response is in the writer's hands, which is stalled
	time.Sleep(100 * time.Millisecond)
	n := be.progress()
	if n == nil {
		t.Fatal("the service was not given a notifier")
	}
	runID, attemptID := "run_00000000-0000-7000-8000-000000000001", "att_00000000-0000-7000-8000-000000000001"
	for i := int64(0); i < 8; i++ {
		n.ProgressDelta(protocol.ProgressDelta{RunID: runID, AttemptID: attemptID, Ordinal: i, Text: "x", Provisional: true})
	}
	send(2) // a control response while progress is saturated
	send(3)
	time.Sleep(100 * time.Millisecond)
	g.open()
	lines := waitForLines(t, g, 1+2+3+1)
	ids := []string{}
	for _, l := range lines {
		assertProtocolLine(t, l)
		var m struct {
			ID     string `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal([]byte(l), &m)
		if m.ID != "" {
			ids = append(ids, m.ID)
		} else {
			ids = append(ids, m.Method)
		}
	}
	want := []string{rid(1), rid(2), rid(3), "progress/delta", "progress/delta", "progress/delta", "progress/gap"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("order\n got %v\nwant %v", ids, want)
	}
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
