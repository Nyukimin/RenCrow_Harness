package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// noFrameWithin reports whether the client wrote nothing for d. Use it last: a read that
// timed out stays pending until the pipe closes.
func (p *peer) noFrameWithin(d time.Duration) bool {
	got := make(chan struct{}, 1)
	go func() {
		if _, err := p.rd.ReadBytes('\n'); err == nil {
			got <- struct{}{}
		}
	}()
	select {
	case <-got:
		return false
	case <-time.After(d):
		return true
	}
}

// ---- framing --------------------------------------------------------------

func TestReadFrame(t *testing.T) {
	read := func(in string, max int) ([]string, error) {
		br := bufio.NewReaderSize(strings.NewReader(in), 16)
		var out []string
		for {
			f, err := readFrame(br, max)
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			if err != nil {
				return out, err
			}
			out = append(out, string(f))
		}
	}
	long := strings.Repeat("x", 100)
	for _, c := range []struct {
		name string
		in   string
		max  int
		want []string
		err  error
	}{
		{"lines", "a\nbb\n", 10, []string{"a", "bb"}, nil},
		{"final line without a newline is a frame", "a\nb", 10, []string{"a", "b"}, nil},
		{"empty line", "a\n\nb\n", 10, []string{"a", "", "b"}, nil},
		{"line longer than the read buffer", long + "\n", 200, []string{long}, nil},
		{"line at the limit", strings.Repeat("y", 10) + "\n", 10, []string{strings.Repeat("y", 10)}, nil},
		{"line over the limit", strings.Repeat("y", 11) + "\n", 10, nil, errFrameTooLong},
		{"unterminated line over the limit", strings.Repeat("y", 50), 10, nil, errFrameTooLong},
		{"nothing", "", 10, nil, nil},
	} {
		got, err := read(c.in, c.max)
		if !errors.Is(err, c.err) || strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: got %q, %v; want %q, %v", c.name, got, err, c.want, c.err)
		}
	}
}

func TestParseEnvelope(t *testing.T) {
	ok := []struct{ name, in string }{
		{"response", `{"jsonrpc":"2.0","id":"req_x","result":{"a":1}}`},
		{"error response", `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"m"}}`},
		{"notification", `{"jsonrpc":"2.0","method":"event/recorded","params":{}}`},
		{"trailing whitespace", `{"jsonrpc":"2.0","id":"x","result":1}  `},
	}
	for _, c := range ok {
		if _, err := parseEnvelope([]byte(c.in)); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	bad := []struct{ name, in string }{
		{"duplicate member", `{"jsonrpc":"2.0","id":"x","result":1,"result":2}`},
		{"duplicate id", `{"jsonrpc":"2.0","id":"x","id":"y","result":1}`},
		{"unknown member", `{"jsonrpc":"2.0","id":"x","result":1,"extra":true}`},
		{"trailing value", `{"jsonrpc":"2.0","id":"x","result":1} {}`},
		{"trailing garbage", `{"jsonrpc":"2.0","id":"x","result":1}x`},
		{"batch", `[{"jsonrpc":"2.0","id":"x","result":1}]`},
		{"not an object", `"x"`},
		{"not JSON", `{"jsonrpc"`},
		{"wrong version", `{"jsonrpc":"1.0","id":"x","result":1}`},
		{"no version", `{"id":"x","result":1}`},
		{"invalid UTF-8", "{\"jsonrpc\":\"2.0\",\"id\":\"\xff\",\"result\":1}"},
		{"method not a string", `{"jsonrpc":"2.0","method":1,"params":{}}`},
	}
	for _, c := range bad {
		if _, err := parseEnvelope([]byte(c.in)); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}

// ---- calls ----------------------------------------------------------------

func TestResponsesAreMatchedByIDWhateverTheirOrder(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	type out struct {
		build string
		err   error
	}
	results := make(chan out, 2)
	for range 2 {
		go func() {
			r, err := c.ServiceCapabilities(ctx)
			results <- out{r.BuildRevision, err}
		}()
	}
	a, b := p.readRequest(), p.readRequest()
	if a.ID == b.ID {
		t.Fatalf("two requests share the ID %s", a.ID)
	}
	respond(p, b.ID, capabilities("second"))
	respond(p, a.ID, capabilities("first"))
	got := map[string]bool{}
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		got[r.build] = true
	}
	if !got["first"] || !got["second"] {
		t.Fatalf("%v", got)
	}
}

func TestADomainErrorIsARemoteErrorWithItsCode(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	go func() {
		req := p.readRequest()
		p.respondError(req.ID, -32000, protocol.ErrorInfo{Code: protocol.CodeBusy, Message: "a run is active", Retryable: true})
		req = p.readRequest()
		p.write(idFrame(req.ID, "error", []byte(`{"code":-32603,"message":"internal error"}`)))
	}()
	_, err := c.RunGet(context.Background(), protocol.RunGetInput{RunID: testRun})
	var re *RemoteError
	if !errors.As(err, &re) || re.RPCCode != -32000 || re.Info.Code != protocol.CodeBusy || !re.Info.Retryable {
		t.Fatalf("%#v", err)
	}
	if protocol.CodeOf(err) != protocol.CodeBusy {
		t.Fatalf("CodeOf = %q", protocol.CodeOf(err))
	}
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Message != "a run is active" || !pe.Retryable {
		t.Fatalf("the failure is not a *protocol.Error: %#v", err)
	}
	// An error response without data is still a refusal, and the connection goes on.
	_, err = c.RunGet(context.Background(), protocol.RunGetInput{RunID: testRun})
	if !errors.As(err, &re) || re.RPCCode != -32603 || re.Info.Message != "internal error" || re.Info.Code != "" {
		t.Fatalf("%#v", err)
	}
}

func TestAMalformedInputIsRefusedBeforeItIsSent(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	_, err := c.RunGet(context.Background(), protocol.RunGetInput{RunID: "not-a-run-id"})
	if code := protocol.CodeOf(err); code != protocol.CodeInvalidParams {
		t.Fatalf("%v", err)
	}
	start := protocol.StartInput{} // empty: not a StartInput
	if _, err := c.TurnStart(context.Background(), start); err == nil || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("%v", err)
	}
	if !p.noFrameWithin(100 * time.Millisecond) {
		t.Fatal("an invalid input was sent")
	}
}

func TestAResultThatBreaksTheSchemaFailsTheCallAndNotTheConnection(t *testing.T) {
	c, p, failures := newPipeClient(t, connOptions{})
	go func() {
		req := p.readRequest()
		p.write(idFrame(req.ID, "result", []byte(`{"run_id":"`+testRun+`"}`))) // not a RunInfo
		req = p.readRequest()
		respond(p, req.ID, runInfo("Measuring", 0, nil))
	}()
	_, err := c.RunGet(context.Background(), protocol.RunGetInput{RunID: testRun})
	if !errors.Is(err, ErrProtocol) || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("%v", err)
	}
	if info, err := c.RunGet(context.Background(), protocol.RunGetInput{RunID: testRun}); err != nil || info.Phase != "Measuring" {
		t.Fatalf("the connection did not go on: %v", err)
	}
	select {
	case cause := <-failures:
		t.Fatalf("the connection failed: %v", cause)
	default:
	}
}

func TestARequestOverTheFrameLimitIsNotSent(t *testing.T) {
	cn, p, _ := newPipeConn(t, connOptions{})
	_, err := cn.do(context.Background(), "turn/start", bytes.Repeat([]byte("a"), maxFrameBytes), false)
	if !errors.Is(err, ErrRequestTooLarge) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("%v", err)
	}
	if !p.noFrameWithin(100 * time.Millisecond) {
		t.Fatal("it was sent")
	}
}

// ---- context --------------------------------------------------------------

func TestAContextThatEndsBeforeTheWriterHasTheRequestMeansItWasNotSent(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	// The first call is handed to the writer, which blocks writing it: the peer does not read yet.
	first := make(chan error, 1)
	go func() {
		_, err := c.RunGet(context.Background(), protocol.RunGetInput{RunID: testRun})
		first <- err
	}()
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.RunGet(ctx, protocol.RunGetInput{RunID: testRun})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("a request the writer never had must be reported as not sent: %v", err)
	}
	// Now the peer reads: it gets the first request only.
	req := p.readRequest()
	respond(p, req.ID, runInfo("Measuring", 0, nil))
	if err := recv(t, first); err != nil {
		t.Fatal(err)
	}
	if !p.noFrameWithin(150 * time.Millisecond) {
		t.Fatal("the request whose context ended was sent after all")
	}
}

func TestAContextThatEndsAfterTheRequestWasWrittenMeansTheOutcomeIsUnknownAndALateResponseIsDropped(t *testing.T) {
	c, p, failures := newPipeClient(t, connOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.RunGet(ctx, protocol.RunGetInput{RunID: testRun})
		done <- err
	}()
	late := p.readRequest() // written
	cancel()
	err := recv(t, done)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("%v", err)
	}
	// The answer comes after the caller left. It is read and dropped, and is not a
	// protocol violation; the next call is answered normally.
	respond(p, late.ID, runInfo("Measuring", 0, nil))
	go func() {
		req := p.readRequest()
		respond(p, req.ID, runInfo("Generating", 0, nil))
	}()
	info, err := c.RunGet(context.Background(), protocol.RunGetInput{RunID: testRun})
	if err != nil || info.Phase != "Generating" {
		t.Fatalf("%v %v", info.Phase, err)
	}
	select {
	case cause := <-failures:
		t.Fatalf("the connection failed: %v", cause)
	default:
	}
}

func TestACallWhoseContextHasEndedIsNotStarted(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.RunGet(ctx, protocol.RunGetInput{RunID: testRun}); !errors.Is(err, context.Canceled) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("%v", err)
	}
	if !p.noFrameWithin(100 * time.Millisecond) {
		t.Fatal("it was sent")
	}
}

// ---- the end of the connection --------------------------------------------

func TestTheEndOfTheOutputFailsTheWaitingCallAsPossiblyDelivered(t *testing.T) {
	c, p, failures := newPipeClient(t, connOptions{})
	done := make(chan error, 1)
	go func() {
		_, err := c.RunGet(context.Background(), protocol.RunGetInput{RunID: testRun})
		done <- err
	}()
	p.readRequest()
	_ = p.wr.(io.Closer).Close() // the Harness dies
	err := recv(t, done)
	if !errors.Is(err, ErrClosed) || !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, errPeerClosed) {
		t.Fatalf("%v", err)
	}
	if cause := recv(t, failures); !errors.Is(cause, errPeerClosed) {
		t.Fatalf("%v", cause)
	}
	// A call after that was never sent.
	_, err = c.RunGet(context.Background(), protocol.RunGetInput{RunID: testRun})
	if !errors.Is(err, ErrClosed) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("%v", err)
	}
}

func TestProtocolViolationsEndTheConnection(t *testing.T) {
	eventOK := mustEncode(t, designEvents(t)[0])
	for _, c := range []struct {
		name string
		send func(p *peer, id string)
	}{
		{"not JSON", func(p *peer, _ string) { p.raw("this is not json") }},
		{"a batch", func(p *peer, _ string) { p.raw(`[]`) }},
		{"a message that is neither", func(p *peer, id string) {
			p.raw(`{"jsonrpc":"2.0","id":"` + id + `","method":"event/recorded","params":{}}`)
		}},
		{"a request from the Harness", func(p *peer, _ string) {
			p.raw(`{"jsonrpc":"2.0","id":"req_00000000-0000-7000-8000-000000000001","method":"ping","params":{}}`)
		}},
		{"an unknown notification", func(p *peer, _ string) { p.notify("run/finished", []byte(`{}`)) }},
		{"a notification without params", func(p *peer, _ string) { p.raw(`{"jsonrpc":"2.0","method":"event/recorded"}`) }},
		{"an event that breaks the schema", func(p *peer, _ string) { p.notify("event/recorded", []byte(`{"event_id":"x"}`)) }},
		{"progress that breaks the schema", func(p *peer, _ string) { p.notify("progress/delta", []byte(`{"text":1}`)) }},
		{"a response nobody waits for", func(p *peer, _ string) {
			p.write(idFrame("req_00000000-0000-7000-8000-0000000000ff", "result", []byte(`{}`)))
		}},
		{"an error without an id", func(p *peer, _ string) {
			p.raw(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`)
		}},
		{"a response with neither result nor error", func(p *peer, id string) { p.raw(`{"jsonrpc":"2.0","id":"` + id + `"}`) }},
		{"a response with both", func(p *peer, id string) {
			p.raw(`{"jsonrpc":"2.0","id":"` + id + `","result":{},"error":{"code":1,"message":"m"}}`)
		}},
		{"an error member that is not an error", func(p *peer, id string) { p.raw(`{"jsonrpc":"2.0","id":"` + id + `","error":"bad"}`) }},
		{"an error whose data is not an ErrorInfo", func(p *peer, id string) {
			p.raw(`{"jsonrpc":"2.0","id":"` + id + `","error":{"code":-32000,"message":"m","data":{"code":1}}}`)
		}},
		{"a repeated member", func(p *peer, id string) { p.raw(`{"jsonrpc":"2.0","id":"` + id + `","result":{},"result":{}}`) }},
		{"invalid UTF-8", func(p *peer, _ string) { p.raw("{\"jsonrpc\":\"2.0\",\"id\":\"\xff\",\"result\":{}}") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cn, p, failures := newPipeConn(t, connOptions{})
			done := make(chan error, 1)
			go func() {
				_, err := cn.do(context.Background(), "run/get", []byte(`{"run_id":"`+testRun+`"}`), false)
				done <- err
			}()
			req := p.readRequest()
			// An event the Harness could send, first: it is delivered even though what follows is not.
			p.notify("event/recorded", eventOK)
			c.send(p, req.ID)
			err := recv(t, done)
			if !errors.Is(err, ErrProtocol) || !errors.Is(err, ErrClosed) || !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("the waiting call: %v", err)
			}
			if cause := recv(t, failures); !errors.Is(cause, ErrProtocol) {
				t.Fatalf("cause: %v", cause)
			}
			if n, ok := cn.queue.next(); !ok || n.Kind != NotificationEvent {
				t.Fatalf("what the Harness sent before the violation was lost: %v %v", n, ok)
			}
			if _, ok := cn.queue.next(); ok {
				t.Fatal("the queue was not closed")
			}
		})
	}
}

func TestAFrameOverTheLimitIsAProtocolViolation(t *testing.T) {
	cn, p, failures := newPipeConn(t, connOptions{})
	go func() {
		// The Harness never sends this; the client must not buffer it.
		line := append(bytes.Repeat([]byte("x"), maxFrameBytes+1), '\n')
		_, _ = p.wr.Write(line)
	}()
	select {
	case cause := <-failures:
		if !errors.Is(cause, ErrProtocol) {
			t.Fatalf("%v", cause)
		}
	case <-time.After(wait):
		t.Fatal("an oversize frame did not end the connection")
	}
	_ = cn
}

// ---- notifications --------------------------------------------------------

// takeN reads n notifications from the queue, failing if they do not come.
func takeN(t *testing.T, q *notifyQueue, n int) []Notification {
	t.Helper()
	out := make(chan []Notification, 1)
	go func() {
		var got []Notification
		for len(got) < n {
			x, ok := q.next()
			if !ok {
				break
			}
			got = append(got, x)
		}
		out <- got
	}()
	select {
	case got := <-out:
		if len(got) != n {
			t.Fatalf("got %d notifications, want %d", len(got), n)
		}
		return got
	case <-time.After(wait):
		t.Fatalf("the %d notifications did not arrive", n)
	}
	return nil
}

// describe is the kind of a notification, and for an event its type or sequence.
func describe(n Notification) string {
	switch n.Kind {
	case NotificationEvent:
		return "event:" + n.Event.Type
	case NotificationProgressDelta:
		return "delta:" + string(rune('0'+n.Delta.Ordinal))
	case NotificationProgressReset:
		return "reset"
	case NotificationProgressGap:
		if n.LocalGap {
			return "localgap:" + string(rune('0'+n.Gap.FromOrdinal)) + "-" + string(rune('0'+n.Gap.ToOrdinal))
		}
		return "gap"
	}
	return "?"
}

func TestNotificationsKeepTheOrderTheHarnessSentThemAcrossKinds(t *testing.T) {
	cn, p, _ := newPipeConn(t, connOptions{})
	evs := designEvents(t)
	delta := func(n int64) protocol.ProgressDelta {
		return protocol.ProgressDelta{RunID: testRun, AttemptID: testAttempt, Ordinal: n, Text: "t", Provisional: true}
	}
	p.notifyEvent(evs[0])
	p.notifyDelta(delta(0))
	p.notifyEvent(evs[1])
	p.notifyDelta(delta(1))
	p.notify("progress/reset", mustEncode(t, protocol.ProgressReset{RunID: testRun, OldAttemptID: testAttempt, NewAttemptID: "att_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0002", Reason: "RAW_TOOL_MARKUP", Provisional: true}))
	p.notifyDelta(delta(2))
	p.notify("progress/gap", mustEncode(t, protocol.ProgressGap{RunID: testRun, AttemptID: testAttempt, FromOrdinal: 3, ToOrdinal: 4}))
	p.notifyEvent(evs[2])
	got := takeN(t, cn.queue, 8)
	var names []string
	for _, n := range got {
		names = append(names, describe(n))
	}
	want := []string{"event:" + evs[0].Type, "delta:0", "event:" + evs[1].Type, "delta:1", "reset", "delta:2", "gap", "event:" + evs[2].Type}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("got  %v\nwant %v", names, want)
	}
	if got[6].LocalGap {
		t.Fatal("a gap the Harness sent was marked as the client's own")
	}
}

func TestADuplicateEventIsDeliveredOnce(t *testing.T) {
	cn, p, _ := newPipeConn(t, connOptions{})
	e1, e2 := eventN(t, 1), eventN(t, 2)
	p.notifyEvent(e1)
	p.notifyEvent(e1)
	p.notifyEvent(e2)
	p.notifyEvent(e1)
	p.notifyEvent(eventN(t, 3))
	got := takeN(t, cn.queue, 3)
	if got[0].Event.EventID != e1.EventID || got[1].Event.EventID != e2.EventID || got[2].Event.EventSeq != 3 {
		t.Fatalf("%v %v %v", describe(got[0]), describe(got[1]), describe(got[2]))
	}
}

func TestAConsumerThatDoesNotReadNeverDelaysAResponseAndProgressBecomesALocalGap(t *testing.T) {
	cn, p, failures := newPipeConn(t, connOptions{limits: NotificationLimits{MaxProgress: 3}})
	delta := func(n int64, attempt string) protocol.ProgressDelta {
		return protocol.ProgressDelta{RunID: testRun, AttemptID: attempt, Ordinal: n, Text: "t", Provisional: true}
	}
	// 3 deltas fit; the next 6 do not, and are merged into one gap in their place; an event
	// after them keeps its position; a delta of another Attempt is a gap of its own.
	for i := int64(0); i < 9; i++ {
		p.notifyDelta(delta(i, testAttempt))
	}
	p.notifyEvent(eventN(t, 1))
	other := "att_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0002"
	p.notifyDelta(delta(0, other))

	// A call is answered while nothing was read from the queue.
	c := &Client{conn: cn}
	go func() {
		req := p.readRequest()
		respond(p, req.ID, runInfo("Measuring", 0, nil))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if _, err := c.RunGet(ctx, protocol.RunGetInput{RunID: testRun}); err != nil {
		t.Fatalf("a consumer that does not read delayed a response: %v", err)
	}

	got := takeN(t, cn.queue, 6)
	var names []string
	for _, n := range got {
		names = append(names, describe(n))
	}
	want := "delta:0,delta:1,delta:2,localgap:3-8,event:control.cancel_requested,localgap:0-0"
	if strings.Join(names, ",") != want {
		t.Fatalf("got  %v\nwant %s", names, want)
	}
	if !got[3].LocalGap || got[3].Gap.RunID != testRun || got[3].Gap.AttemptID != testAttempt {
		t.Fatalf("%+v", got[3])
	}
	select {
	case cause := <-failures:
		t.Fatalf("losing progress ended the connection: %v", cause)
	default:
	}
}

func TestAConfirmedEventThatCannotBeKeptEndsTheConnectionAndNothingBeforeItIsLost(t *testing.T) {
	cn, p, failures := newPipeConn(t, connOptions{limits: NotificationLimits{MaxEvents: 3}})
	done := make(chan error, 1)
	go func() {
		_, err := cn.do(context.Background(), "run/get", []byte(`{"run_id":"`+testRun+`"}`), false)
		done <- err
	}()
	req := p.readRequest()
	for i := 1; i <= 4; i++ {
		p.notifyEvent(eventN(t, i)) // the 4th does not fit
	}
	_ = req
	if cause := recv(t, failures); !errors.Is(cause, ErrNotificationOverflow) {
		t.Fatalf("%v", cause)
	}
	if err := recv(t, done); !errors.Is(err, ErrNotificationOverflow) || !errors.Is(err, ErrClosed) {
		t.Fatalf("%v", err)
	}
	got := takeN(t, cn.queue, 3)
	if got[0].Event.EventSeq != 1 || got[2].Event.EventSeq != 3 {
		t.Fatalf("the events kept are not the first three, in order")
	}
	if _, ok := cn.queue.next(); ok {
		t.Fatal("the queue holds more than it was given")
	}
}

func TestEventBytesAreBoundedToo(t *testing.T) {
	ev := eventN(t, 1)
	size := len(mustEncode(t, ev))
	cn, p, failures := newPipeConn(t, connOptions{limits: NotificationLimits{MaxEventBytes: 2*size + size/2}})
	p.notifyEvent(eventN(t, 1))
	p.notifyEvent(eventN(t, 2))
	p.notifyEvent(eventN(t, 3))
	if cause := recv(t, failures); !errors.Is(cause, ErrNotificationOverflow) {
		t.Fatalf("%v", cause)
	}
	takeN(t, cn.queue, 2)
}

func TestIgnoredNotificationsAreDiscardedButTheEndOfARunIsStillSeen(t *testing.T) {
	cn, p, failures := newPipeConn(t, connOptions{ignore: true, limits: NotificationLimits{MaxEvents: 1, MaxProgress: 1}})
	woken, unwatch := cn.watchTerminal(testRun)
	defer unwatch()
	for i := 1; i <= 5; i++ {
		p.notifyEvent(eventN(t, i))
		p.notifyDelta(protocol.ProgressDelta{RunID: testRun, AttemptID: testAttempt, Ordinal: int64(i), Text: "t", Provisional: true})
	}
	p.notifyEvent(terminalEvent(t, testRun, 6))
	select {
	case <-woken:
	case <-time.After(wait):
		t.Fatal("the run.terminal event did not reach its waiter")
	}
	select {
	case cause := <-failures:
		t.Fatalf("%v", cause)
	default:
	}
	cn.queue.mu.Lock()
	n := len(cn.queue.items)
	cn.queue.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d notifications were queued although nobody reads them", n)
	}
}

func TestAWaiterIsWokenByTheEndOfItsRunOnlyAndByTheEndOfTheConnection(t *testing.T) {
	cn, p, _ := newPipeConn(t, connOptions{})
	otherRun := "run_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0099"
	mine, unwatch := cn.watchTerminal(testRun)
	defer unwatch()
	other, unwatchOther := cn.watchTerminal(otherRun)
	defer unwatchOther()
	p.notifyEvent(terminalEvent(t, otherRun, 1))
	select {
	case <-other:
	case <-time.After(wait):
		t.Fatal("not woken")
	}
	select {
	case <-mine:
		t.Fatal("woken by the end of another run")
	case <-time.After(100 * time.Millisecond):
	}
	_ = p.wr.(io.Closer).Close()
	select {
	case <-mine:
	case <-time.After(wait):
		t.Fatal("not woken by the end of the connection")
	}
	// A waiter registered after the end is woken at once.
	late, unwatchLate := cn.watchTerminal(testRun)
	defer unwatchLate()
	select {
	case <-late:
	case <-time.After(wait):
		t.Fatal("a waiter registered on an ended connection waits for ever")
	}
}

// ---- stopping, without a process --------------------------------------------

func TestCallsAreRefusedWhileStoppingExceptTheInternalOnes(t *testing.T) {
	cn, p, _ := newPipeConn(t, connOptions{})
	cn.beginStopping()
	if _, err := cn.do(context.Background(), "run/get", []byte(`{"run_id":"`+testRun+`"}`), false); !errors.Is(err, ErrStopping) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("%v", err)
	}
	done := make(chan error, 1)
	go func() {
		raw, err := cn.do(context.Background(), "service/shutdown", []byte(`{"mode":"drain","deadline_seconds":1}`), true)
		if err == nil {
			_, err = protocol.Decode[protocol.ShutdownResult](raw)
		}
		done <- err
	}()
	req := p.readRequest()
	if req.Method != "service/shutdown" {
		t.Fatalf("%s", req.Method)
	}
	respond(p, req.ID, protocol.ShutdownResult{Accepted: true})
	if err := recv(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCallsAreAllWrittenWhole(t *testing.T) {
	c, p, _ := newPipeClient(t, connOptions{})
	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.SessionGet(context.Background(), protocol.SessionGetInput{ThreadID: testThread})
			errs <- err
		}()
	}
	seen := map[string]bool{}
	for range n {
		req := p.readRequest() // every frame decodes: none is interleaved with another
		seen[req.ID] = true
		p.respondError(req.ID, -32000, protocol.ErrorInfo{Code: protocol.CodeForbidden, Message: "no"})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if protocol.CodeOf(err) != protocol.CodeForbidden {
			t.Fatalf("%v", err)
		}
	}
	if len(seen) != n {
		t.Fatalf("%d distinct request IDs", len(seen))
	}
}

// keep the compiler honest about helpers only some builds use
var _ = json.Marshal

func TestAnAnswerIsDeliveredOnceAndNeverBlocks(t *testing.T) {
	cl := &pendingCall{ch: make(chan reply, 1)}
	done := make(chan struct{})
	go func() {
		cl.deliver(reply{result: []byte("1")})
		cl.deliver(reply{err: errors.New("second")}) // nobody reads: it must not wait
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(wait):
		t.Fatal("deliver blocked")
	}
	if rep := <-cl.ch; string(rep.result) != "1" || rep.err != nil {
		t.Fatalf("%+v", rep)
	}
}
