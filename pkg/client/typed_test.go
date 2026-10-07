package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// answer is what a scripted peer says to one request.
type answer struct {
	result []byte
	err    *protocol.ErrorInfo
}

func ok[T protocol.Message](t testing.TB, v T) answer { return answer{result: mustEncode(t, v)} }

func refuse(code, message string) answer {
	return answer{err: &protocol.ErrorInfo{Code: code, Message: message}}
}

// seen is the requests a scripted peer has handled.
type seen struct {
	mu   sync.Mutex
	reqs []protocol.Request
	vals []any
}

func (s *seen) add(r protocol.Request, v any) {
	s.mu.Lock()
	s.reqs, s.vals = append(s.reqs, r), append(s.vals, v)
	s.mu.Unlock()
}

// methods returns the methods seen, in order.
func (s *seen) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.reqs {
		out = append(out, r.Method)
	}
	return out
}

func (s *seen) params(method string) []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []any
	for i, r := range s.reqs {
		if r.Method == method {
			out = append(out, s.vals[i])
		}
	}
	return out
}

// serve runs a scripted Harness in its own goroutine: every request is decoded as the
// native request it must be, handed to handle, and answered. It stops with the pipe.
func (p *peer) serve(handle func(req protocol.Request, params any) answer) *seen {
	rec := &seen{}
	go func() {
		for {
			line, err := p.readLine()
			if err != nil {
				return
			}
			req, err := protocol.DecodeRequest(line)
			if err != nil {
				p.t.Errorf("the client sent a frame that is not a valid native request: %v", err)
				return
			}
			params, err := req.DecodeParams()
			if err != nil {
				p.t.Errorf("params: %v", err)
				return
			}
			rec.add(req, params)
			a := handle(req, params)
			if a.err != nil {
				p.respondErrorInfo(req.ID, *a.err)
			} else {
				p.write(idFrame(req.ID, "result", a.result))
			}
		}
	}()
	return rec
}

func (p *peer) respondErrorInfo(id string, info protocol.ErrorInfo) {
	data, _ := json.Marshal(info)
	body, _ := json.Marshal(map[string]any{"code": -32000, "message": info.Message, "data": json.RawMessage(data)})
	p.write(idFrame(id, "error", body))
}

// ---- the typed API ----------------------------------------------------------

func TestEveryTypedMethodSendsTheNativeRequestOfItsDesignExample(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contract", "examples", "native_requests.json"))
	if err != nil {
		t.Fatal(err)
	}
	var examples []json.RawMessage
	if err := json.Unmarshal(b, &examples); err != nil {
		t.Fatal(err)
	}
	if len(examples) != 16 {
		t.Fatalf("%d examples", len(examples))
	}
	calls := map[string]func(context.Context, *Client, any) error{
		"service/capabilities": func(ctx context.Context, c *Client, _ any) error { _, err := c.ServiceCapabilities(ctx); return err },
		"session/open": func(ctx context.Context, c *Client, v any) error {
			_, err := c.SessionOpen(ctx, v.(protocol.SessionOpenInput))
			return err
		},
		"session/list": func(ctx context.Context, c *Client, v any) error {
			_, err := c.SessionList(ctx, v.(protocol.SessionListInput))
			return err
		},
		"session/get": func(ctx context.Context, c *Client, v any) error {
			_, err := c.SessionGet(ctx, v.(protocol.SessionGetInput))
			return err
		},
		"session/fork": func(ctx context.Context, c *Client, v any) error {
			_, err := c.SessionFork(ctx, v.(protocol.SessionForkInput))
			return err
		},
		"turn/start": func(ctx context.Context, c *Client, v any) error {
			_, err := c.TurnStart(ctx, v.(protocol.StartInput))
			return err
		},
		"input/append": func(ctx context.Context, c *Client, v any) error {
			_, err := c.InputAppend(ctx, v.(protocol.InputAppendInput))
			return err
		},
		"turn/interrupt": func(ctx context.Context, c *Client, v any) error {
			_, err := c.TurnInterrupt(ctx, v.(protocol.InterruptInput))
			return err
		},
		"run/get": func(ctx context.Context, c *Client, v any) error {
			_, err := c.RunGet(ctx, v.(protocol.RunGetInput))
			return err
		},
		"run/resume": func(ctx context.Context, c *Client, v any) error {
			_, err := c.RunResume(ctx, v.(protocol.ResumeInput))
			return err
		},
		"context/compact": func(ctx context.Context, c *Client, v any) error {
			_, err := c.ContextCompact(ctx, v.(protocol.CompactInput))
			return err
		},
		"receipt/get": func(ctx context.Context, c *Client, v any) error {
			_, err := c.ReceiptGet(ctx, v.(protocol.ReceiptGetInput))
			return err
		},
		"events/read": func(ctx context.Context, c *Client, v any) error {
			_, err := c.EventsRead(ctx, v.(protocol.EventsReadInput))
			return err
		},
		"evidence/read": func(ctx context.Context, c *Client, v any) error {
			_, err := c.EvidenceRead(ctx, v.(protocol.EvidenceReadInput))
			return err
		},
	}
	c, p, _ := newPipeClient(t, connOptions{})
	ids := map[string]bool{}
	covered := 0
	for _, ex := range examples {
		req, err := protocol.DecodeRequest(ex)
		if err != nil {
			t.Fatal(err)
		}
		call := calls[req.Method]
		if call == nil {
			if req.Method != "initialize" && req.Method != "service/shutdown" {
				t.Errorf("%s has no typed method", req.Method)
			}
			continue
		}
		params, err := req.DecodeParams()
		if err != nil {
			t.Fatal(err)
		}
		want, err := protocol.EncodeCanonicalContract(req.Params)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- call(context.Background(), c, params) }()
		got := p.readRequest() // a valid native request: envelope, ID and params
		if got.Method != req.Method || string(got.Params) != string(want) {
			t.Errorf("%s\n got params  %s\nwant params  %s", req.Method, got.Params, want)
		}
		if ids[got.ID] {
			t.Errorf("%s: the request ID %s was used twice", req.Method, got.ID)
		}
		ids[got.ID] = true
		p.respondError(got.ID, -32000, protocol.ErrorInfo{Code: protocol.CodeForbidden, Message: "no"})
		var re *RemoteError
		if err := recv(t, done); !errors.As(err, &re) || re.Info.Code != protocol.CodeForbidden {
			t.Errorf("%s: %v", req.Method, err)
		}
		covered++
	}
	if covered != 14 {
		t.Fatalf("%d methods covered, want 14", covered)
	}
}

func TestTheResultOfATypedMethodIsTheProtocolType(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	want := runInfo("Measuring", 7, nil)
	p.serve(func(req protocol.Request, _ any) answer { return ok(t, want) })
	got, err := c.RunGet(context.Background(), protocol.RunGetInput{RunID: testRun})
	if err != nil {
		t.Fatal(err)
	}
	if string(mustEncode(t, got)) != string(mustEncode(t, want)) {
		t.Fatalf("%+v", got)
	}
}

// ---- InterruptRun and AwaitRun ------------------------------------------------

func TestInterruptRunReadsTheRevisionAndDerivesTheKeyFromIt(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	rec := p.serve(func(req protocol.Request, _ any) answer {
		switch req.Method {
		case "run/get":
			return ok(t, runInfo("Executing", 7, nil))
		default:
			return ok(t, protocol.InterruptReceipt{ReceiptID: "rcp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001", RunID: testRun, SignalRecorded: true, ControlRevision: 8, Code: protocol.InterruptCancelRequested})
		}
	})
	got, err := c.InterruptRun(context.Background(), testRun, "stop.example.run")
	if err != nil || got.Code != protocol.InterruptCancelRequested {
		t.Fatalf("%+v %v", got, err)
	}
	in := rec.params("turn/interrupt")
	if len(in) != 1 {
		t.Fatalf("%v", rec.methods())
	}
	if i := in[0].(protocol.InterruptInput); i.ExpectedControlRevision != 7 || i.IdempotencyKey != "stop.example.run.r7" || i.RunID != testRun {
		t.Fatalf("%+v", i)
	}
	// A prefix that does not make a valid key is refused before anything is sent.
	if _, err := c.InterruptRun(context.Background(), testRun, "x"); protocol.CodeOf(err) != protocol.CodeInvalidParams {
		t.Fatalf("%v", err)
	}
}

// stopScript is a Harness whose Run is stopped by the first stop signal that carries the
// revision it has: the first signal finds the revision moved (as by a concurrent change)
// and is refused with REVISION_CONFLICT.
type stopScript struct {
	t        *testing.T
	mu       sync.Mutex
	rev      int64
	stopped  bool
	refuseAt int // the Nth turn/interrupt is refused as a conflict
	n        int
	never    bool // the Run does not end after the signal
	forbid   bool // the signal is refused
}

func (s *stopScript) handle(req protocol.Request, params any) answer {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch req.Method {
	case "run/get":
		if s.stopped && !s.never {
			return ok(s.t, runInfo("Terminal", s.rev, cancelledResult()))
		}
		return ok(s.t, runInfo("Executing", s.rev, nil))
	case "turn/interrupt":
		if s.forbid {
			return refuse(protocol.CodeForbidden, "no")
		}
		s.n++
		in := params.(protocol.InterruptInput)
		if s.n == s.refuseAt || in.ExpectedControlRevision != s.rev {
			s.rev++ // a concurrent change moved the revision
			return refuse(protocol.CodeRevisionConflict, "the control revision moved")
		}
		s.stopped = true
		s.rev++
		return ok(s.t, protocol.InterruptReceipt{ReceiptID: "rcp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001", RunID: testRun, SignalRecorded: true, ControlRevision: s.rev, Code: protocol.InterruptCancelRequested})
	}
	return refuse(protocol.CodeInternal, "unexpected")
}

func TestInterruptRunReadsAgainWhenTheRevisionMoved(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	s := &stopScript{t: t, rev: 3, refuseAt: 1}
	rec := p.serve(s.handle)
	got, err := c.InterruptRun(context.Background(), testRun, "stop.example.run")
	if err != nil || !got.SignalRecorded {
		t.Fatalf("%+v %v", got, err)
	}
	in := rec.params("turn/interrupt")
	if len(in) != 2 || in[0].(protocol.InterruptInput).IdempotencyKey != "stop.example.run.r3" || in[1].(protocol.InterruptInput).IdempotencyKey != "stop.example.run.r4" ||
		in[1].(protocol.InterruptInput).ExpectedControlRevision != 4 {
		t.Fatalf("%v", in)
	}
}

func TestInterruptRunGivesUpAfterThreeConflicts(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	s := &stopScript{t: t, rev: 3, refuseAt: -1}
	// Every signal finds the revision moved.
	rec := p.serve(func(req protocol.Request, params any) answer {
		if req.Method == "turn/interrupt" {
			s.mu.Lock()
			s.rev++
			s.mu.Unlock()
			return refuse(protocol.CodeRevisionConflict, "moved")
		}
		return s.handle(req, params)
	})
	if _, err := c.InterruptRun(context.Background(), testRun, "stop.example.run"); protocol.CodeOf(err) != protocol.CodeRevisionConflict {
		t.Fatalf("%v", err)
	}
	if n := len(rec.params("turn/interrupt")); n != 3 {
		t.Fatalf("%d signals were sent", n)
	}
}

func TestAwaitRunReturnsTheRunResultOfRunGetWhenTheEndIsNotified(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	var mu sync.Mutex
	gets := 0
	p.serve(func(req protocol.Request, _ any) answer {
		mu.Lock()
		defer mu.Unlock()
		gets++
		if gets == 1 {
			// Only the notification can end the wait: the poll is an hour away.
			go p.notifyEvent(terminalEvent(t, testRun, 9))
			return ok(t, runInfo("Executing", 0, nil))
		}
		return ok(t, runInfo("Terminal", 1, cancelledResult()))
	})
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	res, err := c.AwaitRun(ctx, testRun, AwaitOptions{PollInterval: time.Hour})
	if err != nil || res.Status != "cancelled" || res.RunID != testRun {
		t.Fatalf("%+v %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gets != 2 {
		t.Fatalf("%d run/get calls", gets)
	}
}

func TestAwaitRunPollsWhenNothingIsNotified(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	var mu sync.Mutex
	gets := 0
	p.serve(func(req protocol.Request, _ any) answer {
		mu.Lock()
		defer mu.Unlock()
		gets++
		if gets < 3 {
			return ok(t, runInfo("Executing", 0, nil))
		}
		return ok(t, runInfo("Terminal", 0, cancelledResult()))
	})
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if res, err := c.AwaitRun(ctx, testRun, AwaitOptions{PollInterval: 10 * time.Millisecond}); err != nil || res.Status != "cancelled" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestAwaitRunWithoutAnInterruptKeyOnlyEndsTheWait(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	rec := p.serve(func(protocol.Request, any) answer { return ok(t, runInfo("Executing", 0, nil)) })
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := c.AwaitRun(ctx, testRun, AwaitOptions{PollInterval: time.Hour})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrRunStillActive) {
		t.Fatalf("%v", err)
	}
	for _, m := range rec.methods() {
		if m != "run/get" {
			t.Fatalf("the Run was touched: %v", rec.methods())
		}
	}
}

func TestAwaitRunTurnsTheEndOfItsContextIntoOneStopSignalAndWaitsForTheEnd(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	s := &stopScript{t: t, rev: 3, refuseAt: 1}
	rec := p.serve(s.handle)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	res, err := c.AwaitRun(ctx, testRun, AwaitOptions{InterruptKeyPrefix: "stop.example.run", PollInterval: time.Hour, CancelGrace: wait})
	if err != nil || res.Status != "cancelled" {
		t.Fatalf("%+v %v", res, err)
	}
	in := rec.params("turn/interrupt")
	if len(in) != 2 {
		t.Fatalf("the stop signal was sent %d times (one refused as a conflict, one recorded): %v", len(in), rec.methods())
	}
	if ctx.Err() == nil {
		t.Fatal("the context did not end")
	}
}

func TestAwaitRunReportsARunThatDoesNotEndAfterTheStopSignal(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	s := &stopScript{t: t, rev: 3, never: true}
	p.serve(s.handle)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res, err := c.AwaitRun(ctx, testRun, AwaitOptions{InterruptKeyPrefix: "stop.example.run", PollInterval: 10 * time.Millisecond, CancelGrace: 150 * time.Millisecond})
	if !errors.Is(err, ErrRunStillActive) || !errors.Is(err, context.DeadlineExceeded) || res.RunID != "" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestAwaitRunSaysWhenTheStopSignalCouldNotBeRecorded(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	s := &stopScript{t: t, rev: 3, forbid: true}
	p.serve(s.handle)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.AwaitRun(ctx, testRun, AwaitOptions{InterruptKeyPrefix: "stop.example.run", PollInterval: time.Hour, CancelGrace: wait})
	if !errors.Is(err, context.DeadlineExceeded) || protocol.CodeOf(err) != protocol.CodeForbidden || errors.Is(err, ErrRunStillActive) {
		t.Fatalf("%v", err)
	}
}

func TestAwaitRunEndsWithTheConnection(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	p.serve(func(protocol.Request, any) answer { return ok(t, runInfo("Executing", 0, nil)) })
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = p.wr.(interface{ Close() error }).Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if _, err := c.AwaitRun(ctx, testRun, AwaitOptions{PollInterval: time.Hour}); !errors.Is(err, ErrClosed) {
		t.Fatalf("%v", err)
	}
}

func TestAwaitRunReturnsTheEndOfTheConnectionWhenItComesDuringTheGrace(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	s := &stopScript{t: t, rev: 3, never: true} // the signal is recorded, the Run does not end
	p.serve(s.handle)
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = p.wr.(io.Closer).Close() // the Harness goes away while the stop is awaited
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.AwaitRun(ctx, testRun, AwaitOptions{InterruptKeyPrefix: "stop.example.run", PollInterval: 10 * time.Millisecond, CancelGrace: wait})
	if !errors.Is(err, ErrClosed) || errors.Is(err, ErrRunStillActive) {
		t.Fatalf("%v", err)
	}
}
