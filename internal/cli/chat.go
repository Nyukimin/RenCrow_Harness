package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/service"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

const chatHelp = `commands (a line that does not start with / is a message):
  <text>              a new turn when no run is active; otherwise one more input for the run (next_step)
  /interrupt <text>   stop the run now and hold <text> for the next turn (interrupt_current)
  /later <text>       hold <text> for the next turn without stopping the run (next_turn)
  /stop               record a stop for the run (Ctrl-C does the same)
  /compact            compact the thread (only when no run is active; it is never queued)
  /status             show the session, the thread and the run
  /exit               leave (a run that is active is stopped first)
  //text              send a message that starts with a slash
`

// maxChatLine is the longest line chat reads: the protocol's limit on an input.
const maxChatLine = 8 << 20

// cmdChat: chat --config ABS --workspace ABS --binding PROFILE [--mode MODE].
//
// An interactive line interface to one session. Lines are read from standard input; what
// the model says is written to standard output as it arrives, and everything else (prompts,
// the events of the Run, what a command answered) to standard error. Whether standard input
// is a terminal decides nothing: each line is the operator's declaration, and the origin it
// is recorded as is the profile's (declared_local at most, never proof of a person).
//
// While a Run is going on, a plain line is one more input for it (next_step), /interrupt is
// interrupt_current, /later is next_turn, and /stop and Ctrl-C are turn/interrupt, which only
// records the stop: chat says the Run ended when the Run did. A /interrupt does not start the
// next Run: the text is held, and the Thread applies it before the next message. A second
// Ctrl-C (or one at the prompt) leaves, and so does the end of standard input, which is the
// same as a stop for a Run that is going on (the pipe was cut). The exit code is 0 for a
// normal leave; for a leave that stopped a Run it is the code of how that Run ended (4 when it
// was cancelled), and 1 when it did not end in time.
func cmdChat(ctx context.Context, args []string, in io.Reader, out, errw io.Writer) *exitError {
	fs := newFlags("chat")
	configPath := fs.String("config", "", "absolute path of the configuration file")
	workspace := fs.String("workspace", "", "absolute path of the workspace")
	profile := fs.String("binding", "", "the binding profile the configuration names")
	mode := fs.String("mode", "", "the execution mode (default structured_only)")
	if e := parseFlags(fs, args); e != nil {
		return e
	}
	rt, e := openRuntime(ctx, *configPath, true)
	if e != nil {
		return e
	}
	defer rt.close()
	svc, e := rt.serviceWith(protocol.EntrypointCLIInteractive, "")
	if e != nil {
		return e
	}
	defer svc.Quiesce()

	c := &chat{svc: svc, rt: rt, out: &lockedWriter{w: out}, errw: &lockedWriter{w: errw}, ends: make(chan string, 64)}
	c.sink = &runSink{onEvent: c.onEvent, onDelta: c.onDelta, onReset: c.onReset}
	conn := svc.NewConn(c.sink)
	defer conn.Close()

	sess, opened, e := openSessionFor(ctx, svc, rt.dep, *workspace, *profile, *mode, newKey("cli.chat.open"))
	if e != nil {
		return e
	}
	c.sess = sess
	for _, ev := range opened {
		c.onEvent(ev)
	}
	if c.limits, e = limitsFor(rt.dep, sess.PolicyRef); e != nil {
		return e
	}
	fmt.Fprintf(c.errw, "chat: session %s, thread %s, mode %s. /help lists the commands.\n", sess.SessionID, sess.ThreadID, sess.ExecutionMode)

	// Signals are the chat's own: Ctrl-C is a stop of the Run, not the end of the process, so
	// the process's cancellable context (which the first Ctrl-C cancels) is not what chat waits
	// on.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	return c.loop(context.WithoutCancel(ctx), readLines(in), sigs)
}

// chat is the state of one chat. Everything but the stream bookkeeping is touched only by the
// goroutine of the main loop.
type chat struct {
	svc    *service.Service
	rt     *runtime
	out    *lockedWriter
	errw   *lockedWriter
	sess   protocol.SessionInfo
	limits protocol.Limits
	sink   *runSink
	// ends carries the IDs of Runs whose terminal event was announced.
	ends chan string

	active *activeRun
	// leaving is set once the chat is on its way out: a Run that is going on is waited for
	// (bounded), and nothing more is started.
	leaving     bool
	leaveGrace  <-chan time.Time
	leaveStatus string // how the Run that the leave stopped ended, for the exit code

	streamMu sync.Mutex
	streamed strings.Builder // what was written of the current attempt's text
}

type activeRun struct {
	runID        string
	stopRecorded bool
	orphaned     int
}

// onEvent shows the events of a Run worth a line, and tells the main loop a Run ended.
func (c *chat) onEvent(ev protocol.Event) {
	if ev.Type == protocol.EventRunTerminal && ev.RunID != nil {
		select {
		case c.ends <- *ev.RunID:
		default: // the main loop also polls the Run, so a missed announcement only costs a moment
		}
		return
	}
	if ev.Type == protocol.EventRunStarted && ev.RunID != nil {
		fmt.Fprintf(c.errw, "[run %s started]\n", *ev.RunID)
		return
	}
	if line := humanEvent(ev); line != "" {
		fmt.Fprintf(c.errw, "[%s]\n", line)
	}
}

func (c *chat) onDelta(d protocol.ProgressDelta) {
	c.streamMu.Lock()
	c.streamed.WriteString(d.Text)
	c.streamMu.Unlock()
	fmt.Fprint(c.out, d.Text)
}

func (c *chat) onReset(protocol.ProgressReset) {
	c.streamMu.Lock()
	c.streamed.Reset()
	c.streamMu.Unlock()
	fmt.Fprintln(c.errw, "[the text above is discarded: the generation is retried]")
}

// readLines reads the lines of in on its own goroutine; the channel is closed at the end of
// the input (or at a line that is over the limit).
func readLines(in io.Reader) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 0, 64<<10), maxChatLine+1)
		for sc.Scan() {
			ch <- sc.Text()
		}
	}()
	return ch
}

func (c *chat) say(format string, args ...any) { fmt.Fprintf(c.errw, format+"\n", args...) }

// loop is the main loop: the one goroutine that decides what a line, a signal and the end of a
// Run mean, so that none of them is judged against a state another is changing.
func (c *chat) loop(ctx context.Context, lines <-chan string, sigs <-chan os.Signal) *exitError {
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()
	for {
		if c.leaving && c.active == nil {
			return c.leftWith()
		}
		if c.active == nil && !c.leaving {
			fmt.Fprint(c.errw, "> ")
		}
		select {
		case line, ok := <-lines:
			if !ok {
				lines = nil
				if c.active != nil {
					c.say("standard input ended: the run is stopped")
					c.stopActive(ctx)
				}
				c.leave()
				continue
			}
			if c.leaving {
				continue
			}
			if quit := c.line(ctx, line, sigs); quit {
				c.leave()
			}
		case s := <-sigs:
			if s == syscall.SIGTERM {
				c.stopActive(ctx)
				c.leave()
				continue
			}
			switch {
			case c.leaving:
				// The process does not wait for the Run any more; what it holds is stopped on the
				// way out (Quiesce), and the stop that was recorded for it is what it ends as.
				c.say("leaving without waiting for the run to end: it is stopped on the way out")
				return &exitError{code: ExitCancelled, msg: "left before the run ended"}
			case c.active != nil && !c.active.stopRecorded:
				c.stopActive(ctx)
			default:
				c.leave()
			}
		case runID := <-c.ends:
			c.ended(runID)
		case <-poll.C:
			c.pollActive()
		case <-c.leaveGrace:
			c.leaveGrace = nil
			c.say("the run did not end within %s of its stop", stopGrace)
			return failure("the run did not end after it was told to stop")
		}
	}
}

// leave starts leaving: a Run that is going on is waited for, bounded.
func (c *chat) leave() {
	if c.leaving {
		return
	}
	c.leaving = true
	if c.active != nil {
		c.leaveGrace = time.After(stopGrace)
	}
}

// leftWith is the exit code of a chat that is over: 0, or how the Run that the leave stopped ended.
func (c *chat) leftWith() *exitError {
	if c.leaveStatus != "" {
		if code := exitCodeOfStatus(c.leaveStatus); code != ExitOK {
			return &exitError{code: code, msg: "the run that was going on ended " + c.leaveStatus}
		}
	}
	return nil
}

// line acts on one line of input. It reports whether the chat is to be left.
func (c *chat) line(ctx context.Context, line string, sigs <-chan os.Signal) (quit bool) {
	text := strings.TrimRight(line, "\r")
	if strings.TrimSpace(text) == "" {
		return false
	}
	if strings.HasPrefix(text, "//") {
		c.message(ctx, text[1:])
		return false
	}
	if !strings.HasPrefix(text, "/") {
		c.message(ctx, text)
		return false
	}
	name, rest, _ := strings.Cut(text, " ")
	rest = strings.TrimSpace(rest)
	switch name {
	case "/help":
		fmt.Fprint(c.errw, chatHelp)
	case "/exit", "/quit":
		if c.active != nil {
			c.stopActive(ctx)
		}
		return true
	case "/stop":
		if c.active == nil {
			c.say("no run is active")
		} else {
			c.stopActive(ctx)
		}
	case "/status":
		c.status(ctx)
	case "/compact":
		if rest != "" {
			c.say("/compact takes no argument")
		} else {
			c.compact(ctx, sigs)
		}
	case "/interrupt":
		c.control(ctx, rest, "interrupt_current")
	case "/later":
		c.control(ctx, rest, "next_turn")
	default:
		c.say("unknown command %s (/help lists them; start a message with // to send a slash)", name)
	}
	return false
}

// message sends a plain line: a new turn when nothing is active, one more input for the Run
// that is.
func (c *chat) message(ctx context.Context, text string) {
	if c.active == nil {
		c.startTurn(ctx, text)
		return
	}
	info, ok := c.activeInfo()
	if !ok {
		return
	}
	if info.Terminal {
		// The Run ended as the line arrived: the line is what the person meant to say next.
		c.ended(info.RunID)
		c.say("the run had ended; the line is sent as a new turn")
		c.startTurn(ctx, text)
		return
	}
	switch err := c.appendInput(ctx, info, text, "next_step"); {
	case err == nil:
		c.say("[queued for the next step of the run]")
	case protocol.CodeOf(err) == protocol.CodeInvalidRequest:
		// Not the active Run any more: the true state is the Run's, not the first look.
		if again, ok := c.activeInfo(); ok && again.Terminal {
			c.ended(again.RunID)
			c.say("the run had ended; the line is sent as a new turn")
			c.startTurn(ctx, text)
			return
		}
		c.say("the line was not sent: %s", errCode(err))
	default:
		c.say("the line was not sent: %s", errCode(err))
	}
}

// control sends /interrupt and /later: only while a Run is going on, and never resent as
// something else, because what they mean is about that Run.
func (c *chat) control(ctx context.Context, text, disposition string) {
	name := map[string]string{"interrupt_current": "/interrupt", "next_turn": "/later"}[disposition]
	if text == "" {
		c.say("%s needs a text", name)
		return
	}
	if c.active == nil {
		c.say("no run is active: send the text as a message")
		return
	}
	info, ok := c.activeInfo()
	if !ok {
		return
	}
	if info.Terminal {
		c.ended(info.RunID)
		c.say("the run had ended: nothing was recorded; send the text as a message")
		return
	}
	switch err := c.appendInput(ctx, info, text, disposition); {
	case err == nil && disposition == "interrupt_current":
		c.active.stopRecorded = true
		c.say("[interrupt recorded: the run is stopping; the text is held and is applied before your next message]")
	case err == nil:
		c.say("[held for the next turn]")
	case protocol.CodeOf(err) == protocol.CodeInvalidRequest:
		c.say("the run is not active any more: nothing was recorded")
	default:
		c.say("not recorded: %s", errCode(err))
	}
}

// appendInput is input/append at the Run's control revision; a stale revision is read again
// and tried once more.
func (c *chat) appendInput(ctx context.Context, info protocol.RunInfo, text, disposition string) error {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		in := protocol.InputAppendInput{ThreadID: info.ThreadID, RunID: info.RunID, Input: protocol.InputMessage{Text: text}, Disposition: disposition,
			ExpectedControlRevision: info.ControlRevision, IdempotencyKey: newKey("cli.input")}
		var params []byte
		if params, err = protocol.Encode(in); err != nil {
			return err
		}
		var events []protocol.Event
		if _, events, err = c.svc.InputAppend(ctx, in, params); err == nil {
			for _, ev := range events {
				c.onEvent(ev)
			}
			return nil
		}
		if protocol.CodeOf(err) != protocol.CodeRevisionConflict {
			return err
		}
		fresh, gerr := c.svc.RunGet(ctx, protocol.RunGetInput{RunID: info.RunID})
		if gerr != nil || fresh.Terminal {
			return err
		}
		info = fresh
	}
	return err
}

// startTurn is turn/start for a new turn of the session, at the Thread's revisions now.
func (c *chat) startTurn(ctx context.Context, text string) {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var sess protocol.SessionInfo
		if sess, err = c.svc.SessionGet(ctx, protocol.SessionGetInput{ThreadID: c.sess.ThreadID}); err != nil {
			break
		}
		in := protocol.StartInput{ThreadID: sess.ThreadID, Input: protocol.InputMessage{Text: text}, ContextBlocks: []protocol.ContextBlock{}, Upstream: nil,
			ExpectedContextRevision: sess.ContextRevision, ExpectedControlRevision: sess.ControlRevision, IdempotencyKey: newKey("cli.turn"), Limits: c.limits}
		var params []byte
		if params, err = protocol.Encode(in); err != nil {
			break
		}
		start, admitted, begin, serr := c.svc.TurnStartDeferred(ctx, in, params)
		if serr != nil {
			if err = serr; protocol.CodeOf(err) == protocol.CodeRevisionConflict {
				continue
			}
			break
		}
		c.streamMu.Lock()
		c.streamed.Reset()
		c.streamMu.Unlock()
		c.active = &activeRun{runID: start.RunID}
		for _, ev := range admitted {
			c.onEvent(ev)
		}
		if begin != nil {
			begin()
		}
		return
	}
	c.say("the turn was not started: %s", errCode(err))
}

// activeInfo reads the active Run. It reports false when it cannot be read (and says so).
func (c *chat) activeInfo() (protocol.RunInfo, bool) {
	info, err := c.svc.RunGet(context.Background(), protocol.RunGetInput{RunID: c.active.runID})
	if err != nil {
		c.say("the run cannot be read: %s", errCode(err))
		return protocol.RunInfo{}, false
	}
	return info, true
}

// stopActive records a stop for the active Run, once.
func (c *chat) stopActive(ctx context.Context) {
	if c.active == nil || c.active.stopRecorded {
		return
	}
	info, ok := c.activeInfo()
	if !ok {
		return
	}
	if info.Terminal {
		c.ended(info.RunID)
		return
	}
	c.active.stopRecorded = true
	recordStop(c.svc, info, func(s string) { c.say("%s", s) })
}

// ended is the end of a Run, once it is known: what it came to is said, the text that was not
// shown as it streamed is shown, and the chat is idle again.
func (c *chat) ended(runID string) {
	if c.active == nil || c.active.runID != runID {
		return
	}
	info, err := c.svc.RunGet(context.Background(), protocol.RunGetInput{RunID: runID})
	if err != nil || !info.Terminal || info.Result == nil {
		return
	}
	c.active = nil
	res := *info.Result
	c.streamMu.Lock()
	shown := c.streamed.String()
	c.streamed.Reset()
	c.streamMu.Unlock()
	switch {
	case res.Status == "completed" && shown == res.FinalText:
		fmt.Fprintln(c.out)
	case res.Status == "completed":
		// The text on the screen is what was streamed; the Run's own answer is this one.
		fmt.Fprintf(c.out, "\n%s\n", res.FinalText)
	case shown != "":
		fmt.Fprintln(c.out)
	}
	c.say("[run %s ended %s %s (verification: %s)]", res.RunID, res.Status, res.Code, res.Verification.Status)
	if c.leaving {
		c.leaveStatus = res.Status
	}
}

// pollActive covers an announcement that was missed, and a Run that nobody drives.
func (c *chat) pollActive() {
	if c.active == nil {
		return
	}
	info, ok := c.activeInfo()
	if !ok {
		return
	}
	if info.Terminal {
		c.ended(info.RunID)
		return
	}
	if c.svc.Driving(info.RunID) {
		c.active.orphaned = 0
		return
	}
	if c.active.orphaned++; c.active.orphaned == 20 {
		c.say("the run %s is not driven by this process and has not ended (inspect --run shows it); it is given up on here", info.RunID)
		c.active = nil
	}
}

func (c *chat) status(ctx context.Context) {
	sess, err := c.svc.SessionGet(ctx, protocol.SessionGetInput{ThreadID: c.sess.ThreadID})
	if err != nil {
		c.say("the session cannot be read: %s", errCode(err))
		return
	}
	c.say("session %s thread %s mode %s context_revision %d control_revision %d", sess.SessionID, sess.ThreadID, sess.ExecutionMode, sess.ContextRevision, sess.ControlRevision)
	if c.active == nil {
		c.say("no run is active")
		return
	}
	if info, ok := c.activeInfo(); ok {
		c.say("run %s phase %s terminal %t generation_attempts used=%d unknown=%d", info.RunID, info.Phase, info.Terminal, info.GenerationAttemptsUsed, info.GenerationAttemptsUnknown)
	}
}

// compact is the manual compaction of an idle Thread: never queued behind a Run. It holds
// the main loop until it has its result (lines typed meanwhile wait), and a signal records a
// stop for it.
func (c *chat) compact(ctx context.Context, sigs <-chan os.Signal) {
	if c.active != nil {
		c.say("a run is active: /stop it or wait for it first (a compaction is not queued behind a run)")
		return
	}
	sess, err := c.svc.SessionGet(ctx, protocol.SessionGetInput{ThreadID: c.sess.ThreadID})
	if err != nil {
		c.say("the compaction was not started: %s", errCode(err))
		return
	}
	in := protocol.CompactInput{ThreadID: sess.ThreadID, ExpectedContextRevision: sess.ContextRevision, ExpectedControlRevision: sess.ControlRevision, DryRun: false, IdempotencyKey: newKey("cli.compact")}
	params, err := protocol.Encode(in)
	if err != nil {
		c.say("the compaction was not started: %s", errCode(err))
		return
	}
	accepted, events, err := c.svc.ContextCompact(ctx, in, params)
	if err != nil {
		c.say("the compaction was not started: %s", errCode(err))
		return
	}
	runID := ""
	for _, ev := range events {
		if ev.Type == protocol.EventRunStarted && ev.RunID != nil {
			runID = *ev.RunID
		}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		select {
		case <-sigs:
			close(stop)
		case <-done:
		}
	}()
	result, e := waitCompactResult(stop, c.svc, accepted.ReceiptID, runID, sess.ControlRevision)
	close(done)
	if e != nil {
		c.say("the compaction: %s", e.msg)
		return
	}
	outcome := "-"
	if result.Outcome != nil {
		outcome = *result.Outcome
	}
	c.say("[compaction %s %s]", result.Status, outcome)
}
