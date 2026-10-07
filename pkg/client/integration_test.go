package client

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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

// The integration tests run the client against the real rencrow-harness binary, built
// from this module, in the production composition: the real Service, the real RenCrow_LLM
// client and the strict wire over loopback HTTP, against a Gateway double that serves the
// fake model. Nothing in the client is replaced.

var (
	buildOnce sync.Once
	binPath   string
	binDir    string
	buildErr  error
)

func harnessBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs the real binary")
	}
	buildOnce.Do(func() {
		binDir, buildErr = os.MkdirTemp("", "rencrow-harness-client-")
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

// harness is a deployment: a configuration whose Gateway is a double serving the fake
// model, and an initialized store.
type harness struct {
	l         *harnesstest.Layout
	fake      *harnesstest.Fake
	gw        *harnesstest.Gateway
	measuring chan struct{}
}

// newHarness builds the deployment. With hold, the Gateway double never answers a count:
// a Run stays in its Measuring phase until it is stopped.
func newHarness(t *testing.T, fake *harnesstest.Fake, hold bool) *harness {
	t.Helper()
	bin := harnessBinary(t)
	l := harnesstest.NewLayout(t, harnesstest.Options{})
	h := &harness{l: l, fake: fake, measuring: make(chan struct{}, 16)}
	if hold {
		fake.OnMeasure = func(ctx context.Context) {
			select {
			case h.measuring <- struct{}{}:
			default:
			}
			<-ctx.Done()
		}
	}
	b := l.Binding()
	h.gw = harnesstest.NewGateway(t, fake, protocol.Binding{Kind: b["kind"].(string), Selector: b["selector"].(string), ProfileRevision: b["profile_revision"].(string)})
	l.Cfg["gateway"].(map[string]any)["base_url"] = h.gw.BaseURL()
	l.Write()
	cmd := exec.Command(bin, "init", "--config", l.Config, "--data-root", l.Data)
	cmd.Dir = t.TempDir()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	return h
}

func (h *harness) waitMeasuring(t *testing.T) {
	t.Helper()
	select {
	case <-h.measuring:
	case <-time.After(wait):
		t.Fatal("no count reached the Gateway")
	}
}

// start starts a Client on the deployment, with an empty environment: it is aborted when
// the test ends.
func (h *harness) start(t *testing.T, stderr *syncBuffer) *Client {
	t.Helper()
	cfg := Config{
		Binary: binPath, ConfigPath: h.l.Config, Dir: t.TempDir(),
		ClientName: "client-test", ClientVersion: "1", ExitGrace: 10 * time.Second,
	}
	if stderr != nil {
		cfg.Stderr = stderr
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	c, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Abort)
	return c
}

func (h *harness) binding() protocol.Binding {
	b := h.l.Binding()
	return protocol.Binding{Kind: b["kind"].(string), Selector: b["selector"].(string), ProfileRevision: b["profile_revision"].(string)}
}

func (h *harness) openInput(key string) protocol.SessionOpenInput {
	return protocol.SessionOpenInput{
		WorkspacePath: h.l.Work, Binding: h.binding(), PolicyRef: h.l.WorkspacePolicy(),
		ExecutionMode: protocol.ModeTrustedHost, IdempotencyKey: key,
	}
}

var testLimits = protocol.Limits{MaxModelSteps: 10, MaxToolCallsPerStep: 8, DeadlineSeconds: 1800, MaxCaptureBytes: 67108864, MaxGenerationAttempts: 32}

func startInput(t *testing.T, thread, key, text string, contextRevision int64) protocol.StartInput {
	t.Helper()
	const blockText = "host policy decides what may run"
	rev, err := protocol.ContextRevision(protocol.KindStableRuntimeContext, blockText, nil)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.StartInput{
		ThreadID: thread, Input: protocol.InputMessage{Text: text},
		ContextBlocks:           []protocol.ContextBlock{{Kind: protocol.KindStableRuntimeContext, Text: blockText, Revision: rev}},
		ExpectedContextRevision: contextRevision, IdempotencyKey: key, Limits: testLimits,
	}
}

// next reads one notification, failing if none comes.
func next(t *testing.T, c *Client) Notification {
	t.Helper()
	select {
	case n, ok := <-c.Notifications():
		if !ok {
			t.Fatalf("Notifications closed; Err=%v", c.Err())
		}
		return n
	case <-time.After(wait):
		t.Fatal("no notification")
	}
	return Notification{}
}

// nextEvents reads n notifications that must all be confirmed events.
func nextEvents(t *testing.T, c *Client, n int) []protocol.Event {
	t.Helper()
	var out []protocol.Event
	for range n {
		note := next(t, c)
		if note.Kind != NotificationEvent {
			t.Fatalf("expected an event, got %s", describe(note))
		}
		out = append(out, *note.Event)
	}
	return out
}

func types(evs []protocol.Event) string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return strings.Join(out, ",")
}

// quiet reports whether no notification arrives for d.
func quiet(c *Client, d time.Duration) bool {
	select {
	case <-c.Notifications():
		return false
	case <-time.After(d):
		return true
	}
}

func TestRealHarnessRunLifecycleThroughTheTypedAPI(t *testing.T) {
	h := newHarness(t, harnesstest.NewFake(), true) // a Run is held in its count until it is stopped
	var stderr syncBuffer
	c := h.start(t, &stderr)
	ctx, cancel := context.WithTimeout(context.Background(), 2*wait)
	defer cancel()

	// initialize, as Start did: this build, this protocol, what the CORE needs ready.
	caps := c.Capabilities()
	if caps.ProtocolVersion != protocol.ProtocolVersion || caps.BuildRevision == "" {
		t.Fatalf("%+v", caps)
	}
	again, err := c.ServiceCapabilities(ctx)
	if err != nil || len(again.Capabilities) != len(caps.Capabilities) {
		t.Fatalf("%v", err)
	}

	open, err := c.SessionOpen(ctx, h.openInput("client.open.000000000001"))
	if err != nil {
		t.Fatal(err)
	}
	thread := open.Session.ThreadID
	if ev := nextEvents(t, c, 1); ev[0].Type != protocol.EventSessionCreated || ev[0].ThreadID != thread || ev[0].EventSeq != 1 {
		t.Fatalf("%+v", ev)
	}
	if list, err := c.SessionList(ctx, protocol.SessionListInput{Limit: 10}); err != nil || len(list.Sessions) != 1 || list.Sessions[0].ThreadID != thread {
		t.Fatalf("%+v %v", list, err)
	}
	if got, err := c.SessionGet(ctx, protocol.SessionGetInput{ThreadID: thread}); err != nil || got.ActiveRunID != nil {
		t.Fatalf("%+v %v", got, err)
	}

	// turn/start: the acceptance, then the events in the order the Harness committed them.
	const text = "この隔離workspaceのテストを修正する"
	in := startInput(t, thread, "client.start.00000000001", text, 0)
	start, err := c.TurnStart(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	started := nextEvents(t, c, 4)
	if types(started) != "input.accepted,task.created,run.started,input.applied" || started[0].EventSeq != 2 || started[3].EventSeq != 5 {
		t.Fatalf("%s", types(started))
	}
	h.waitMeasuring(t)
	if !start.Accepted || start.Intake.Entrypoint != protocol.EntrypointStdioAutomation || start.Intake.EffectiveOrigin != protocol.OriginAutomation {
		t.Fatalf("%+v", start)
	}

	// The same key and payload: the original result, and nothing announced.
	replay, err := c.TurnStart(ctx, in)
	if err != nil || string(mustEncode(t, replay)) != string(mustEncode(t, start)) {
		t.Fatalf("a replay must return the original result: %v", err)
	}
	if !quiet(c, 200*time.Millisecond) {
		t.Fatal("a replay announced events")
	}
	// The same key and another payload; a new key while the Run is active.
	changed := in
	changed.Input.Text = text + " さらに"
	if _, err := c.TurnStart(ctx, changed); protocol.CodeOf(err) != protocol.CodeIdempotencyConflict {
		t.Fatalf("%v", err)
	}
	var re *RemoteError
	if _, err := c.TurnStart(ctx, startInput(t, thread, "client.start.00000000002", "next", 0)); !errors.As(err, &re) || re.RPCCode != -32000 || re.Info.Code != protocol.CodeBusy {
		t.Fatalf("%v", err)
	}

	run, err := c.RunGet(ctx, protocol.RunGetInput{RunID: start.RunID})
	if err != nil || run.Phase != "Measuring" || run.Terminal || run.Result != nil || run.ThreadID != thread {
		t.Fatalf("%+v %v", run, err)
	}
	rec, err := c.ReceiptGet(ctx, protocol.ReceiptGetInput{ReceiptID: start.ReceiptID})
	if err != nil || rec.Operation != "turn/start" || rec.Result == nil || rec.Result.Type != "StartResult" {
		t.Fatalf("%+v %v", rec, err)
	}
	read, err := c.EventsRead(ctx, protocol.EventsReadInput{ThreadID: thread, AfterSeq: 0, Limit: 100})
	if err != nil || len(read.Events) != 5 || read.HasMore || read.Events[1].EventID != started[0].EventID {
		t.Fatalf("%+v %v", read, err)
	}
	ev, err := c.EvidenceRead(ctx, protocol.EvidenceReadInput{EvidenceID: start.Intake.EvidenceID, ProjectionVersion: "text/v1", Range: protocol.ByteRange{Start: 0, End: uint64(len(text))}})
	if err != nil {
		t.Fatal(err)
	}
	if data, err := base64.StdEncoding.DecodeString(ev.DataBase64); err != nil || string(data) != text || ev.Partial {
		t.Fatalf("%v %+v", err, ev)
	}

	// input/append: stored with its classification, announced once.
	appendIn := protocol.InputAppendInput{ThreadID: thread, RunID: start.RunID, Input: protocol.InputMessage{Text: "more"}, Disposition: protocol.DispositionNextStep, IdempotencyKey: "client.append.0000000001"}
	appended, err := c.InputAppend(ctx, appendIn)
	if err != nil || appended.DeliveryState != protocol.DeliveryQueued || appended.Origin != protocol.OriginAutomation {
		t.Fatalf("%+v %v", appended, err)
	}
	if ev := nextEvents(t, c, 1); ev[0].Type != protocol.EventInputAccepted {
		t.Fatalf("%+v", ev)
	}
	// session/fork and context/compact are methods of this build; what they say about this
	// Thread is the Harness's to say, and arrives as the Harness says it.
	if _, err := c.SessionFork(ctx, protocol.SessionForkInput{ThreadID: thread, CheckpointID: "ckp_00000000-0000-7000-8000-000000000001", IdempotencyKey: "client.fork.0000000000001"}); protocol.CodeOf(err) != protocol.CodeInvalidRequest {
		t.Fatalf("%v", err)
	}
	if _, err := c.ContextCompact(ctx, protocol.CompactInput{ThreadID: thread, ExpectedContextRevision: 0, ExpectedControlRevision: 0, DryRun: true, IdempotencyKey: "client.compact.00000000001"}); err != nil {
		var re *RemoteError
		if !errors.As(err, &re) {
			t.Fatalf("a refusal of the Harness is a *RemoteError: %v", err)
		}
	}

	// Stop it: the signal is a record; the end is the run.terminal event and the RunResult.
	receipt, err := c.InterruptRun(ctx, start.RunID, "stop."+start.RunID)
	if err != nil || receipt.Code != protocol.InterruptCancelRequested || !receipt.SignalRecorded {
		t.Fatalf("%+v %v", receipt, err)
	}
	ended := nextEvents(t, c, 2)
	if ended[0].Type != protocol.EventControlCancelRequest || ended[1].Type != protocol.EventRunTerminal || ended[1].Code == nil || *ended[1].Code != "CANCELLED" {
		t.Fatalf("%s", types(ended))
	}
	res, err := c.AwaitRun(ctx, start.RunID, AwaitOptions{PollInterval: time.Hour})
	if err != nil || res.Status != "cancelled" || res.Code != "CANCELLED" || !res.Resumable {
		t.Fatalf("%+v %v", res, err)
	}
	// Asking again for the same revision is answered from the receipt: the same signal.
	if again, err := c.TurnInterrupt(ctx, protocol.InterruptInput{RunID: start.RunID, ExpectedControlRevision: 0, IdempotencyKey: "stop." + start.RunID + ".r0"}); err != nil || again.ReceiptID != receipt.ReceiptID {
		t.Fatalf("%+v %v", again, err)
	}

	// run/resume: a new Run of the same Task, held again.
	last, err := c.RunGet(ctx, protocol.RunGetInput{RunID: start.RunID})
	if err != nil {
		t.Fatal(err)
	}
	resume := protocol.ResumeInput{
		TaskID: start.TaskID, ExpectedLastRunID: start.RunID, ExpectedControlRevision: last.ControlRevision,
		IdempotencyKey: "client.resume.000000000001", Limits: testLimits,
	}
	resumed, err := c.RunResume(ctx, resume)
	if err != nil || resumed.PreviousRunID != start.RunID || resumed.RunID == start.RunID {
		t.Fatalf("%+v %v", resumed, err)
	}
	if ev := nextEvents(t, c, 1); ev[0].Type != protocol.EventRunStarted || *ev[0].RunID != resumed.RunID {
		t.Fatalf("%+v", ev)
	}
	h.waitMeasuring(t)

	// A context that ends without an interrupt key ends the wait and leaves the Run alone.
	short, cancelShort := context.WithTimeout(ctx, 200*time.Millisecond)
	if _, err := c.AwaitRun(short, resumed.RunID, AwaitOptions{PollInterval: time.Hour}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	cancelShort()
	if info, err := c.RunGet(ctx, protocol.RunGetInput{RunID: resumed.RunID}); err != nil || info.Terminal {
		t.Fatalf("the Run was touched: %+v %v", info, err)
	}
	// With one, the end of the context is one stop signal, and the wait goes on to the end.
	stop, cancelStop := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelStop()
	res, err = c.AwaitRun(stop, resumed.RunID, AwaitOptions{InterruptKeyPrefix: "stop." + resumed.RunID, PollInterval: time.Hour, CancelGrace: wait})
	if err != nil || res.RunID != resumed.RunID || res.Status != "cancelled" {
		t.Fatalf("%+v %v", res, err)
	}

	// An orderly end: nothing is running, the child exits 0, the channel closes.
	if err := c.Shutdown(ctx, protocol.ShutdownInput{Mode: "drain", DeadlineSeconds: 2}); err != nil {
		t.Fatalf("%v\nstderr: %s", err, stderr.String())
	}
	if c.Err() != nil || !isDone(c) {
		t.Fatalf("%v", c.Err())
	}
	for range c.Notifications() { // what was left
	}
	if _, err := c.RunGet(ctx, protocol.RunGetInput{RunID: start.RunID}); !errors.Is(err, ErrClosed) {
		t.Fatalf("%v", err)
	}
	if s := stderr.String(); strings.Contains(s, h.l.Dir) || strings.Contains(s, "テスト") || strings.Contains(s, "client.start") {
		t.Fatalf("the diagnostics carry a path or request content: %q", s)
	}
}

func TestRealHarnessACompletedRunIsAwaitedFromItsNotification(t *testing.T) {
	h := newHarness(t, harnesstest.NewFake(), false) // the fake model answers "done"
	c := h.start(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*wait)
	defer cancel()
	open, err := c.SessionOpen(ctx, h.openInput("client.open.000000000002"))
	if err != nil {
		t.Fatal(err)
	}
	thread := open.Session.ThreadID
	start, err := c.TurnStart(ctx, startInput(t, thread, "client.start.00000000003", "say done", 0))
	if err != nil {
		t.Fatal(err)
	}
	// The poll is an hour away: the end is learned from run.terminal, and the result is
	// read from the Harness.
	res, err := c.AwaitRun(ctx, start.RunID, AwaitOptions{PollInterval: time.Hour})
	if err != nil || res.Status != "completed" || res.FinalText != "done" {
		t.Fatalf("%+v %v", res, err)
	}
	// The notifications are the Run's own, in the order they were committed, ending with
	// the run.terminal event that woke AwaitRun, and the provisional text came before it.
	var kinds []string
	sawText := false
	for {
		n := next(t, c)
		kinds = append(kinds, describe(n))
		if n.Kind == NotificationProgressDelta {
			sawText = true
		}
		if n.Kind == NotificationEvent && n.Event.Type == protocol.EventRunTerminal {
			break
		}
	}
	if kinds[len(kinds)-1] != "event:run.terminal" || !sawText || kinds[0] != "event:session.created" {
		t.Fatalf("%v", kinds)
	}
	if err := c.Shutdown(ctx, protocol.ShutdownInput{Mode: "drain", DeadlineSeconds: 2}); err != nil {
		t.Fatal(err)
	}
}

func TestRealHarnessShutdownCancelStopsARunAndItRecordsHowItEnded(t *testing.T) {
	h := newHarness(t, harnesstest.NewFake(), true)
	c := h.start(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*wait)
	defer cancel()
	open, err := c.SessionOpen(ctx, h.openInput("client.open.000000000003"))
	if err != nil {
		t.Fatal(err)
	}
	start, err := c.TurnStart(ctx, startInput(t, open.Session.ThreadID, "client.start.00000000004", "go", 0))
	if err != nil {
		t.Fatal(err)
	}
	h.waitMeasuring(t)
	if err := c.Shutdown(ctx, protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 5}); err != nil {
		t.Fatal(err)
	}
	// Another connection to the same deployment reads how the Run ended: stopped with its driver.
	c2 := h.start(t, nil)
	info, err := c2.RunGet(ctx, protocol.RunGetInput{RunID: start.RunID})
	if err != nil || !info.Terminal || info.Result == nil || info.Result.Code != "DRIVER_STOPPED" || !info.Result.Resumable {
		t.Fatalf("%+v %v", info, err)
	}
	if err := c2.Shutdown(ctx, protocol.ShutdownInput{Mode: "drain", DeadlineSeconds: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestRealHarnessAbortLeavesTheRunToTheNextProcess(t *testing.T) {
	h := newHarness(t, harnesstest.NewFake(), true)
	c := h.start(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*wait)
	defer cancel()
	open, err := c.SessionOpen(ctx, h.openInput("client.open.000000000004"))
	if err != nil {
		t.Fatal(err)
	}
	thread := open.Session.ThreadID
	start, err := c.TurnStart(ctx, startInput(t, thread, "client.start.00000000005", "go", 0))
	if err != nil {
		t.Fatal(err)
	}
	h.waitMeasuring(t)
	c.Abort()
	if !errors.Is(c.Err(), ErrAborted) {
		t.Fatalf("%v", c.Err())
	}
	// Abort asked the Harness for nothing: the Run is still the Thread's active Run in
	// the store, for whoever takes the writer role next. The same request, sent again with
	// its key, is answered from its receipt.
	c2 := h.start(t, nil)
	sess, err := c2.SessionGet(ctx, protocol.SessionGetInput{ThreadID: thread})
	if err != nil || sess.ActiveRunID == nil || *sess.ActiveRunID != start.RunID {
		t.Fatalf("%+v %v", sess, err)
	}
	replay, err := c2.TurnStart(ctx, startInput(t, thread, "client.start.00000000005", "go", 0))
	if err != nil || replay.RunID != start.RunID || replay.ReceiptID != start.ReceiptID {
		t.Fatalf("%+v %v", replay, err)
	}
	if err := c2.Shutdown(ctx, protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1}); err != nil {
		t.Fatal(err)
	}
}
