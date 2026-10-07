package stdio

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// gatedWriter is a client that does not read until it is released.
type gatedWriter struct {
	mu      sync.Mutex
	gate    chan struct{}
	opened  bool
	lines   []string
	partial strings.Builder
	arrived chan struct{}
}

func newGated() *gatedWriter {
	return &gatedWriter{gate: make(chan struct{}), arrived: make(chan struct{}, 1024)}
}

func (g *gatedWriter) Write(p []byte) (int, error) {
	<-g.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.partial.Write(p)
	for {
		s := g.partial.String()
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		g.lines = append(g.lines, s[:i])
		g.partial.Reset()
		g.partial.WriteString(s[i+1:])
		select {
		case g.arrived <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

func (g *gatedWriter) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.opened {
		g.opened = true
		close(g.gate)
	}
}

func (g *gatedWriter) snapshot() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.lines...)
}

func waitForLines(t *testing.T, g *gatedWriter, n int) []string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if got := g.snapshot(); len(got) >= n {
			return got
		}
		select {
		case <-g.arrived:
		case <-deadline:
			t.Fatalf("only %d of %d lines arrived: %q", len(g.snapshot()), n, g.snapshot())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func startWriter(o *outbound, w io.Writer) (done chan error) {
	done = make(chan error, 1)
	go func() { done <- o.writeLoop(w) }()
	return done
}

func method(t *testing.T, line string) (m string, params map[string]any) {
	t.Helper()
	var v struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &v); err != nil {
		t.Fatalf("%q: %v", line, err)
	}
	return v.Method, v.Params
}

func delta(ordinal int64) (protocol.ProgressDelta, []byte) {
	d := protocol.ProgressDelta{RunID: "run_00000000-0000-7000-8000-000000000001", AttemptID: "att_00000000-0000-7000-8000-000000000001", Ordinal: ordinal, Text: "t", Provisional: true}
	return d, notificationFrame("progress/delta", d)
}

func TestControlAndEventsArePutAheadOfProgressAndKeepTheirOrder(t *testing.T) {
	o := newOutbound(Limits{PriorityMessages: 16, PriorityBytes: 1 << 20, ProgressMessages: 4})
	g := newGated()
	done := startWriter(o, g)

	// The writer takes the first message and stalls inside Write: the client is slow.
	if err := o.putPriority([]byte(`{"first":1}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	for i := int64(0); i < 3; i++ {
		d, f := delta(i)
		o.putDelta(d, f)
	}
	for _, s := range []string{`{"r":"response-1"}`, `{"r":"event-1"}`, `{"r":"response-2"}`} {
		if err := o.putPriority([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	g.open()
	lines := waitForLines(t, g, 7)
	want := []string{`{"first":1}`, `{"r":"response-1"}`, `{"r":"event-1"}`, `{"r":"response-2"}`}
	for i, w := range want {
		if lines[i] != w {
			t.Fatalf("line %d is %q, want %q (control and confirmed events go first, in order)\n%q", i, lines[i], w, lines)
		}
	}
	for i := 4; i < 7; i++ {
		if m, p := method(t, lines[i]); m != "progress/delta" || int64(p["ordinal"].(float64)) != int64(i-4) {
			t.Fatalf("line %d: %s", i, lines[i])
		}
	}
	o.close()
	<-done
}

func TestProgressPastTheCapBecomesOneMergedGapAfterTheQueuedDeltas(t *testing.T) {
	o := newOutbound(Limits{PriorityMessages: 16, PriorityBytes: 1 << 20, ProgressMessages: 3})
	g := newGated()
	done := startWriter(o, g)
	if err := o.putPriority([]byte(`{"first":1}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	for i := int64(0); i < 10; i++ {
		d, f := delta(i)
		o.putDelta(d, f)
	}
	// While the gap notice is still queued, an attempt's further deltas join it.
	g.open()
	lines := waitForLines(t, g, 5)
	for i := 1; i <= 3; i++ {
		if m, p := method(t, lines[i]); m != "progress/delta" || int64(p["ordinal"].(float64)) != int64(i-1) {
			t.Fatalf("line %d: %s", i, lines[i])
		}
	}
	m, p := method(t, lines[4])
	if m != "progress/gap" || int64(p["from_ordinal"].(float64)) != 3 || int64(p["to_ordinal"].(float64)) != 9 ||
		p["run_id"] != "run_00000000-0000-7000-8000-000000000001" || p["attempt_id"] != "att_00000000-0000-7000-8000-000000000001" {
		t.Fatalf("gap notice %s", lines[4])
	}
	if _, err := protocol.Decode[protocol.ProgressGap]([]byte(mustJSON(t, p))); err != nil {
		t.Fatalf("the gap notice is not a ProgressGap: %v", err)
	}
	// After the gap went out, progress resumes with later ordinals.
	time.Sleep(50 * time.Millisecond)
	d, f := delta(10)
	o.putDelta(d, f)
	lines = waitForLines(t, g, 6)
	if m, p := method(t, lines[5]); m != "progress/delta" || int64(p["ordinal"].(float64)) != 10 {
		t.Fatalf("%s", lines[5])
	}
	o.close()
	<-done
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestProgressIsStoppedWhenTheConfirmedQueueIsHalfFull(t *testing.T) {
	o := newOutbound(Limits{PriorityMessages: 8, PriorityBytes: 1 << 20, ProgressMessages: 100})
	g := newGated()
	done := startWriter(o, g)
	if err := o.putPriority([]byte(`{"first":1}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 4; i++ {
		if err := o.putPriority([]byte(`{"event":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	for i := int64(0); i < 5; i++ {
		d, f := delta(i)
		o.putDelta(d, f)
	}
	g.open()
	lines := waitForLines(t, g, 6)
	if m, p := method(t, lines[5]); m != "progress/gap" || int64(p["from_ordinal"].(float64)) != 0 || int64(p["to_ordinal"].(float64)) != 4 {
		t.Fatalf("progress must not compete with confirmed events: %q", lines)
	}
	o.close()
	<-done
}

func TestConfirmedMessagesNeverDropTheyOverflowTheConnection(t *testing.T) {
	o := newOutbound(Limits{PriorityMessages: 3, PriorityBytes: 1 << 20, ProgressMessages: 4})
	g := newGated() // never opened: the client reads nothing
	done := startWriter(o, g)
	if err := o.putPriority([]byte(`{"n":0}`)); err != nil { // taken by the stalled writer
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	for i := 1; i <= 3; i++ {
		if err := o.putPriority([]byte(`{"n":1}`)); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	if err := o.putPriority([]byte(`{"n":4}`)); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("an event that cannot be queued must not be dropped silently: %v", err)
	}
	// The byte cap is the same kind of limit.
	small := newOutbound(Limits{PriorityMessages: 100, PriorityBytes: 10, ProgressMessages: 4})
	if err := small.putPriority([]byte(strings.Repeat("a", 11))); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("%v", err)
	}
	o.fail(ErrBackpressure)
	g.open()
	if err := <-done; err != nil && !errors.Is(err, ErrBackpressure) {
		t.Fatalf("%v", err)
	}
}

func TestProgressResetIsNotDroppedByTheCap(t *testing.T) {
	o := newOutbound(Limits{PriorityMessages: 8, PriorityBytes: 1 << 20, ProgressMessages: 1})
	g := newGated()
	done := startWriter(o, g)
	if err := o.putPriority([]byte(`{"first":1}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	d, f := delta(0)
	o.putDelta(d, f)
	reset := protocol.ProgressReset{RunID: d.RunID, OldAttemptID: d.AttemptID, NewAttemptID: "att_00000000-0000-7000-8000-000000000002", Reason: "retry", Provisional: true}
	o.putReset(notificationFrame("progress/reset", reset))
	g.open()
	lines := waitForLines(t, g, 3)
	if m, _ := method(t, lines[2]); m != "progress/reset" {
		t.Fatalf("%q", lines)
	}
	o.close()
	<-done
}

func TestDrainWaitsForTheQueueAndGivesUpAtTheDeadline(t *testing.T) {
	o := newOutbound(Limits{PriorityMessages: 8, PriorityBytes: 1 << 20, ProgressMessages: 4})
	var sink strings.Builder
	done := startWriter(o, &sink)
	for i := 0; i < 5; i++ {
		if err := o.putPriority([]byte(`{"x":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	if !o.drain(2 * time.Second) {
		t.Fatal("a draining queue with a live client must finish")
	}
	if got := strings.Count(sink.String(), "\n"); got != 5 {
		t.Fatalf("%d lines were flushed", got)
	}
	o.close()
	<-done

	stalled := newOutbound(Limits{PriorityMessages: 8, PriorityBytes: 1 << 20, ProgressMessages: 4})
	g := newGated()
	done2 := startWriter(stalled, g)
	_ = stalled.putPriority([]byte(`{"x":1}`))
	_ = stalled.putPriority([]byte(`{"x":2}`))
	start := time.Now()
	if stalled.drain(100 * time.Millisecond) {
		t.Fatal("a stalled client cannot have been drained")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the drain deadline was not honoured")
	}
	stalled.close()
	g.open()
	<-done2
}

func TestWriteErrorFailsTheConnection(t *testing.T) {
	o := newOutbound(Limits{PriorityMessages: 8, PriorityBytes: 1 << 20, ProgressMessages: 4})
	pr, pw := io.Pipe()
	_ = pr.Close() // the client is gone
	done := startWriter(o, pw)
	_ = o.putPriority([]byte(`{"x":1}`))
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a failed write must end the writer with an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the writer did not stop")
	}
	if err := o.putPriority([]byte(`{"x":2}`)); err == nil {
		t.Fatal("a failed connection accepted more output")
	}
	select {
	case <-o.failed():
	default:
		t.Fatal("failed() must be closed")
	}
}
