package client

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// wait is the longest a test waits for something that must happen.
const wait = 20 * time.Second

// peer is the Harness side of an in-memory connection: it reads the frames the client
// writes and writes frames the client reads. It is used from the test goroutine, or from
// one goroutine at a time.
type peer struct {
	t  *testing.T
	rd *bufio.Reader
	wr io.Writer
	mu sync.Mutex
}

// newPipeConn is a started conn over two in-memory pipes and its peer. failures
// receives the cause of every failure the conn reports to its owner.
func newPipeConn(t *testing.T, o connOptions) (*conn, *peer, <-chan error) {
	t.Helper()
	clientIn, peerOut := io.Pipe()
	peerIn, clientOut := io.Pipe()
	failures := make(chan error, 8)
	o.onFail = func(cause error) { failures <- cause }
	cn := newConn(clientIn, clientOut, o)
	cn.start()
	t.Cleanup(func() {
		_ = peerOut.Close()
		_ = peerIn.Close()
		cn.fail(ErrAborted)
	})
	return cn, &peer{t: t, rd: bufio.NewReaderSize(peerIn, 1<<20), wr: peerOut}, failures
}

// newPipeClient is a Client with no child process: just the connection, for the calls.
func newPipeClient(t *testing.T, o connOptions) (*Client, *peer, <-chan error) {
	t.Helper()
	cn, p, failures := newPipeConn(t, o)
	return &Client{conn: cn}, p, failures
}

// readRequest reads the next frame and requires it to be a valid native request.
func (p *peer) readRequest() protocol.Request {
	p.t.Helper()
	line, err := p.readLine()
	if err != nil {
		p.t.Fatalf("the client wrote no request: %v", err)
	}
	req, err := protocol.DecodeRequest(line)
	if err != nil {
		p.t.Fatalf("the client sent a frame that is not a valid native request: %v\n%s", err, line)
	}
	return req
}

func (p *peer) readLine() ([]byte, error) {
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		b, err := p.rd.ReadBytes('\n')
		ch <- res{bytes.TrimRight(b, "\n"), err}
	}()
	select {
	case r := <-ch:
		return r.b, r.err
	case <-time.After(wait):
		return nil, os.ErrDeadlineExceeded
	}
}

// write sends one frame to the client. The pipe is synchronous, so a write fails only
// when the pipe was closed (by the test's end or by the test itself); that is not
// reported, because write also runs in goroutines that outlive their test.
func (p *peer) write(frame []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = p.wr.Write(append(append([]byte{}, frame...), '\n'))
}

// raw writes a line as it is.
func (p *peer) raw(line string) { p.t.Helper(); p.write([]byte(line)) }

func mustEncode[T protocol.Message](t testing.TB, v T) []byte {
	t.Helper()
	b, err := protocol.Encode(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}

func idFrame(id string, member string, body []byte) []byte {
	idJSON, _ := json.Marshal(id)
	return []byte(`{"jsonrpc":"2.0","id":` + string(idJSON) + `,"` + member + `":` + string(body) + `}`)
}

// respond answers a request with a result.
func respond[T protocol.Message](p *peer, id string, result T) {
	p.t.Helper()
	p.write(idFrame(id, "result", mustEncode(p.t, result)))
}

// respondError answers a request with a domain error (-32000, with an ErrorInfo).
func (p *peer) respondError(id string, rpcCode int, info protocol.ErrorInfo) {
	p.t.Helper()
	data, err := json.Marshal(info)
	if err != nil {
		p.t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"code": rpcCode, "message": info.Message, "data": json.RawMessage(data)})
	p.write(idFrame(id, "error", body))
}

// notify writes a notification.
func (p *peer) notify(method string, params []byte) {
	p.t.Helper()
	p.write([]byte(`{"jsonrpc":"2.0","method":"` + method + `","params":` + string(params) + `}`))
}

func (p *peer) notifyEvent(ev protocol.Event) {
	p.t.Helper()
	p.notify("event/recorded", mustEncode(p.t, ev))
}
func (p *peer) notifyDelta(d protocol.ProgressDelta) {
	p.t.Helper()
	p.notify("progress/delta", mustEncode(p.t, d))
}

// ---- fixtures -------------------------------------------------------------

const (
	testThread  = "thr_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"
	testTask    = "tsk_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"
	testRun     = "run_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"
	testAttempt = "att_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"
	testDigest  = "0000000000000000000000000000000000000000000000000000000000000000"
)

// designEvents are the 15 events of the design package, in order, one of each type.
func designEvents(t testing.TB) []protocol.Event {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contract", "examples", "wire", "event_payloads.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	var out []protocol.Event
	for _, raw := range f.Events {
		ev, err := protocol.Decode[protocol.Event](raw)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

// eventN is a valid event (a cancel request of a Run) with a distinct ID and sequence.
func eventN(t testing.TB, n int) protocol.Event {
	t.Helper()
	id := "evt_018f1c2d-3e4f-7a5b-8c6d-" + hex12(n)
	ev, err := protocol.BuildTypedEvent(protocol.EventCommon{
		EventID: id, EventSeq: int64(n), ThreadID: testThread, TaskID: protocol.Str(testTask), RunID: protocol.Str(testRun),
		RecordedAt: "2026-10-07T01:00:00Z",
	}, protocol.ControlCancelRequestedPayload{RunID: testRun, ControlRevision: int64(n), Reason: "USER_REQUESTED", Principal: "user:ren"})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func hex12(n int) string {
	const digits = "0123456789abcdef"
	b := []byte("000000000000")
	for i := 11; i >= 0 && n > 0; i-- {
		b[i] = digits[n&15]
		n >>= 4
	}
	return string(b)
}

func terminalEvent(t testing.TB, runID string, seq int) protocol.Event {
	t.Helper()
	ev, err := protocol.BuildTypedEvent(protocol.EventCommon{
		EventID: "evt_018f1c2d-3e4f-7a5b-8c6d-" + hex12(0x1000+seq), EventSeq: int64(seq), ThreadID: testThread,
		TaskID: protocol.Str(testTask), RunID: protocol.Str(runID),
		EvidenceID: protocol.Str("evd_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"), Code: protocol.Str("CANCELLED"),
		RecordedAt: "2026-10-07T01:00:00Z",
	}, protocol.RunTerminalPayload{Status: "cancelled", Code: "CANCELLED", ResultEvidenceID: "evd_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func runInfo(phase string, control int64, result *protocol.RunResult) protocol.RunInfo {
	return protocol.RunInfo{
		RunID: testRun, TaskID: testTask, ThreadID: testThread, Phase: phase, Terminal: result != nil, Result: result,
		ContextRevision: 1, ControlRevision: control, LastEventSeq: 5,
		EffectiveLimits: protocol.Limits{MaxModelSteps: 10, MaxToolCallsPerStep: 8, DeadlineSeconds: 1800, MaxCaptureBytes: 67108864, MaxGenerationAttempts: 32},
		DeadlineAt:      "2026-10-07T00:30:00Z", RecoveryPolicyRevision: testDigest,
	}
}

func cancelledResult() *protocol.RunResult {
	return &protocol.RunResult{
		RunID: testRun, TaskID: testTask, Status: "cancelled", Code: "CANCELLED", FinalText: "",
		Verification: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}}, EvidenceIDs: []string{}, UnresolvedActionIDs: []string{}, Resumable: true,
	}
}

func capabilities(build string) protocol.CapabilitiesResult {
	return protocol.CapabilitiesResult{ProtocolVersion: protocol.ProtocolVersion, BuildRevision: build, Capabilities: []protocol.Capability{
		{Name: "initialize", Status: "ready", Basis: "declared"},
	}}
}

// recv takes the next value of ch, failing the test if none comes.
func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(wait):
		t.Fatal("timed out waiting for a value")
	}
	var zero T
	return zero
}
