package service_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/transport/stdio"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// wireClient is the client end of an in-process stdio connection to a Service.
type wireClient struct {
	t     *testing.T
	in    io.WriteCloser
	lines chan map[string]any
	done  chan error
}

func dial(t *testing.T, serve func(ctx context.Context, in io.Reader, out io.Writer) error) *wireClient {
	t.Helper()
	cr, cw := io.Pipe() // client -> server
	sr, sw := io.Pipe() // server -> client
	c := &wireClient{t: t, in: cw, lines: make(chan map[string]any, 1024), done: make(chan error, 1)}
	go func() {
		c.done <- serve(context.Background(), cr, sw)
		_ = sw.Close()
	}()
	go func() {
		br := bufio.NewReaderSize(sr, 1<<20)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				dec := json.NewDecoder(strings.NewReader(line))
				dec.UseNumber()
				var m map[string]any
				if dec.Decode(&m) != nil {
					t.Errorf("not JSON: %q", line)
				}
				c.lines <- m
			}
			if err != nil {
				close(c.lines)
				return
			}
		}
	}()
	t.Cleanup(func() { _ = cw.Close() })
	return c
}

func (c *wireClient) send(method string, params any) string {
	c.t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		c.t.Fatal(err)
	}
	id := identity.NewRequestID().String()
	frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": json.RawMessage(raw)})
	if _, err := c.in.Write(append(frame, '\n')); err != nil {
		c.t.Fatal(err)
	}
	return id
}

func (c *wireClient) next(within time.Duration) (map[string]any, bool) {
	select {
	case m, ok := <-c.lines:
		return m, ok
	case <-time.After(within):
		return nil, false
	}
}

func TestARunOverStdioAnnouncesItsEventsInOrderAndItsTextProvisionally(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Reply{Kind: harnesstest.KindFinal, Chunks: []string{"alpha ", "beta ", "gamma"}})
	r := newModelRig(t, fake, nil, nil)
	c := dial(t, func(ctx context.Context, in io.Reader, out io.Writer) error {
		return stdio.Serve(ctx, r.svc, in, out, stdio.Options{})
	})

	var events []protocol.Event
	var deltas []protocol.ProgressDelta
	var responses = map[string]map[string]any{}
	// pump reads frames until done reports true.
	pump := func(done func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !done() {
			m, ok := c.next(time.Until(deadline))
			if !ok {
				t.Fatalf("the connection gave nothing (events=%d)", len(events))
			}
			switch m["method"] {
			case "event/recorded":
				raw, _ := json.Marshal(m["params"])
				ev, err := protocol.Decode[protocol.Event](raw)
				if err != nil {
					t.Fatal(err)
				}
				events = append(events, ev)
			case "progress/delta":
				raw, _ := json.Marshal(m["params"])
				var d protocol.ProgressDelta
				if err := json.Unmarshal(raw, &d); err != nil {
					t.Fatal(err)
				}
				deltas = append(deltas, d)
			case nil:
				responses[m["id"].(string)] = m
			}
		}
	}
	call := func(method string, params any) map[string]any {
		id := c.send(method, params)
		pump(func() bool { return responses[id] != nil })
		if responses[id]["error"] != nil {
			t.Fatalf("%s: %v", method, responses[id]["error"])
		}
		return responses[id]["result"].(map[string]any)
	}
	call("initialize", protocol.InitializeInput{ClientName: "wire", ClientVersion: "0", ProtocolVersion: protocol.ProtocolVersion})
	open := call("session/open", r.openParams("wire.open.0000000000000001"))
	thread := open["session"].(map[string]any)["thread_id"].(string)
	start := call("turn/start", startParams(thread, "wire.start.00000000000001", "go"))

	pump(func() bool {
		for _, e := range events {
			if e.Type == protocol.EventRunTerminal {
				return true
			}
		}
		return false
	})
	// The text of the generation arrives as provisional deltas, in order, tied to the attempt.
	pump(func() bool { return len(deltas) >= 3 })
	if len(deltas) != 3 || deltas[0].Text != "alpha " || deltas[1].Text != "beta " || deltas[2].Text != "gamma" {
		t.Fatalf("%+v", deltas)
	}
	for i, d := range deltas {
		if d.Ordinal != int64(i) || !d.Provisional || d.RunID != start["run_id"] {
			t.Fatalf("%+v", d)
		}
	}

	// Every event of the Thread was announced once, in commit order, with the events
	// that admitted the Run first and none of the driver's overtaking them.
	var types []string
	for i, e := range events {
		types = append(types, e.Type)
		if e.EventSeq != int64(i+1) {
			t.Fatalf("event %d has event_seq %d: announcements are out of order or missing (%v)", i, e.EventSeq, types)
		}
	}
	if got := strings.Join(types, ","); got != "session.created,input.accepted,task.created,run.started,input.applied,action.prepared,action.dispatch_started,"+
		"model.attempt_started,model.requested,model.completed,run.terminal" {
		t.Fatalf("%s", got)
	}
	// What was announced is what events/read returns.
	read := call("events/read", protocol.EventsReadInput{ThreadID: thread, AfterSeq: 0, Limit: 100})
	stored := read["events"].([]any)
	if len(stored) != len(events) {
		t.Fatalf("%d stored, %d announced", len(stored), len(events))
	}
	for i, s := range stored {
		if s.(map[string]any)["event_id"] != events[i].EventID {
			t.Fatalf("event %d differs", i)
		}
	}
	run := call("run/get", protocol.RunGetInput{RunID: start["run_id"].(string)})
	if res := run["result"].(map[string]any); res["status"] != "completed" || res["final_text"] != "alpha beta gamma" || run["phase"] != "Terminal" {
		t.Fatalf("%v", run)
	}
	capsAfter := call("service/capabilities", protocol.EmptyInput{})
	for _, c := range capsAfter["capabilities"].([]any) {
		m := c.(map[string]any)
		if (m["name"] == "turn.execution" || m["name"] == "model.generation") && m["status"] != "ready" {
			t.Fatalf("a process with a model port executes its runs: %v", m)
		}
	}

	call("service/shutdown", protocol.ShutdownInput{Mode: "drain", DeadlineSeconds: 5})
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not stop")
	}
	r.svc.Quiesce()
}
