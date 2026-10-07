package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/kernel"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// TestEveryEndOfARunHasItsExitCode: PROTOCOL section 2 for the seven statuses a Run ends in,
// and the codes 2 to 6 are not the ones of anything else.
func TestEveryEndOfARunHasItsExitCode(t *testing.T) {
	want := map[string]int{"completed": 0, "incomplete": 2, "rejected": 3, "blocked": 3, "cancelled": 4, "failed": 5, "restart_required": 6}
	statuses := kernel.Statuses()
	if len(statuses) != len(want) {
		t.Fatalf("%d statuses, %d codes", len(statuses), len(want))
	}
	for _, s := range statuses {
		if got, ok := want[s]; !ok || exitCodeOfStatus(s) != got {
			t.Errorf("%s: %d, want %d", s, exitCodeOfStatus(s), got)
		}
	}
	if exitCodeOfStatus("something else") != ExitFailure {
		t.Error("an unknown status is a failure of the operation, not a Run outcome")
	}
	for _, c := range []int{ExitOK, ExitFailure, ExitUsage} {
		for _, s := range statuses {
			if c != ExitOK && exitCodeOfStatus(s) == c {
				t.Errorf("%d is a Run outcome of %s and an exit code of something else", c, s)
			}
		}
	}
}

func TestLimitsOfAnEarlierRunAreHeldToWhatTheHostAllowsNow(t *testing.T) {
	caps := protocol.Limits{MaxModelSteps: 10, MaxToolCallsPerStep: 8, DeadlineSeconds: 600, MaxCaptureBytes: 1 << 20, MaxGenerationAttempts: 32}
	same, lowered := clampLimits(caps, caps)
	if lowered || same != caps {
		t.Fatalf("%+v %t", same, lowered)
	}
	lower := protocol.Limits{MaxModelSteps: 3, MaxToolCallsPerStep: 2, DeadlineSeconds: 60, MaxCaptureBytes: 2048, MaxGenerationAttempts: 4}
	if got, lowered := clampLimits(lower, caps); lowered || got != lower {
		t.Fatalf("limits under the caps were changed: %+v", got)
	}
	high := protocol.Limits{MaxModelSteps: 1000, MaxToolCallsPerStep: 100, DeadlineSeconds: 86400, MaxCaptureBytes: 1 << 26, MaxGenerationAttempts: 9999}
	if got, lowered := clampLimits(high, caps); !lowered || got != caps {
		t.Fatalf("%+v %t", got, lowered)
	}
	mixed := protocol.Limits{MaxModelSteps: 3, MaxToolCallsPerStep: 100, DeadlineSeconds: 60, MaxCaptureBytes: 2048, MaxGenerationAttempts: 4}
	if got, lowered := clampLimits(mixed, caps); !lowered || got.MaxModelSteps != 3 || got.MaxToolCallsPerStep != 8 || got.DeadlineSeconds != 60 {
		t.Fatalf("%+v", got)
	}
}

// chatRig is a chat over a real Service and the Gateway double, with no loop: the tests act on
// it one step at a time, which is what makes a race a fixed order.
func chatRig(t *testing.T, fake *harnesstest.Fake) (*chat, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	l := harnesstest.NewLayout(t, harnesstest.Options{})
	binding := l.Binding()
	gw := harnesstest.NewGateway(t, fake, protocol.Binding{Kind: binding["kind"].(string), Selector: binding["selector"].(string), ProfileRevision: binding["profile_revision"].(string)})
	l.Cfg["gateway"].(map[string]any)["base_url"] = gw.BaseURL()
	l.Write()
	ctx := context.Background()
	var out, errw bytes.Buffer
	if code := Run(ctx, []string{"init", "--config", l.Config, "--data-root", l.Data}, strings.NewReader(""), &out, &errw); code != 0 {
		t.Fatalf("init: %d %s", code, errw.String())
	}
	rt, e := openRuntime(ctx, l.Config, true)
	if e != nil {
		t.Fatal(e.msg)
	}
	t.Cleanup(rt.close)
	svc, e := rt.serviceWith(protocol.EntrypointCLIInteractive, "")
	if e != nil {
		t.Fatal(e.msg)
	}
	t.Cleanup(svc.Quiesce)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	c := &chat{svc: svc, rt: rt, out: &lockedWriter{w: stdout}, errw: &lockedWriter{w: stderr}, ends: make(chan string, 64)}
	c.sink = &runSink{onEvent: c.onEvent, onDelta: c.onDelta, onReset: c.onReset}
	conn := svc.NewConn(c.sink)
	t.Cleanup(conn.Close)
	sess, _, e := openSessionFor(ctx, svc, rt.dep, l.Work, "fixture-local", "", newKey("cli.test.open"))
	if e != nil {
		t.Fatal(e.msg)
	}
	c.sess = sess
	if c.limits, e = limitsFor(rt.dep, sess.PolicyRef); e != nil {
		t.Fatal(e.msg)
	}
	return c, stdout, stderr
}

func (c *chat) waitEnded(t *testing.T, runID string) protocol.RunResult {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		info, err := c.svc.RunGet(context.Background(), protocol.RunGetInput{RunID: runID})
		if err == nil && info.Terminal {
			return *info.Result
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the run did not end")
	return protocol.RunResult{}
}

// TestALineThatArrivesAsTheRunEndsIsSentAsANewTurnAndNeverLost: the chat still believes a Run is
// active (the announcement of its end has not been handled) when the line comes. The state of
// the Run, not the belief, decides: the line starts the next turn, and says so.
func TestALineThatArrivesAsTheRunEndsIsSentAsANewTurnAndNeverLost(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Final("an answer"))
	c, _, stderr := chatRig(t, fake)
	ctx := context.Background()
	c.startTurn(ctx, "first")
	if c.active == nil {
		t.Fatalf("no Run was started: %s", stderr.String())
	}
	first := c.active.runID
	c.waitEnded(t, first) // ended, and the chat has not heard of it: c.active is still set

	c.message(ctx, "the late line")
	if c.active == nil || c.active.runID == first {
		t.Fatalf("the line was not sent as a new turn: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "the run had ended; the line is sent as a new turn") {
		t.Fatalf("%s", stderr.String())
	}
	second := c.active.runID
	if res := c.waitEnded(t, second); res.Status != "completed" {
		t.Fatalf("%+v", res)
	}
	gen := fake.Generates()
	if len(gen) != 2 {
		t.Fatalf("%d generations", len(gen))
	}
	found := false
	for _, m := range gen[1].Messages {
		found = found || strings.Contains(m.Text(), "the late line")
	}
	if !found {
		t.Fatal("the line did not reach the model")
	}
}

// TestInterruptAndLaterAreNeverResentAsAnotherKindOfInput: when the Run has ended, nothing is
// recorded for them and the person is told, because what they mean is about that Run.
func TestInterruptAndLaterAreNeverResentAsAnotherKindOfInput(t *testing.T) {
	fake := harnesstest.NewFake()
	c, _, stderr := chatRig(t, fake)
	ctx := context.Background()
	c.startTurn(ctx, "first")
	run := c.active.runID
	c.waitEnded(t, run)
	before := countQueued(t, c)

	for _, cmd := range []string{"/interrupt change course", "/later after this"} {
		if c.active == nil {
			c.active = &activeRun{runID: run}
		}
		c.line(ctx, cmd, nil)
		if !strings.Contains(stderr.String(), "the run had ended: nothing was recorded; send the text as a message") {
			t.Fatalf("%s: %s", cmd, stderr.String())
		}
	}
	if countQueued(t, c) != before || len(fake.Generates()) != 1 {
		t.Fatalf("something was recorded or generated for a Run that had ended")
	}
}

func countQueued(t *testing.T, c *chat) int {
	t.Helper()
	sess, err := c.svc.SessionGet(context.Background(), protocol.SessionGetInput{ThreadID: c.sess.ThreadID})
	if err != nil {
		t.Fatal(err)
	}
	return int(sess.ContextRevision)*1000 + int(sess.ControlRevision)
}

func TestTheEventsOfARunAreWordedForAPersonOnlyWhenTheyAreWorthAWord(t *testing.T) {
	mk := func(typ string, payload string, code *string) protocol.Event {
		return protocol.Event{Type: typ, Payload: []byte(payload), Code: code}
	}
	cancelled := "CANCELLED"
	for _, tc := range []struct {
		ev   protocol.Event
		want string
	}{
		{mk(protocol.EventActionPrepared, `{"kind":"tool","name":"file.read"}`, nil), "tool file.read"},
		{mk(protocol.EventActionPrepared, `{"kind":"model","name":"act"}`, nil), ""},
		{mk(protocol.EventRunTerminal, `{}`, &cancelled), "run ended CANCELLED"},
		{mk(protocol.EventModelRequested, `{}`, nil), ""},
		{mk(protocol.EventInputApplied, `{}`, nil), ""},
	} {
		if got := humanEvent(tc.ev); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.ev.Type, got, tc.want)
		}
	}
}
