package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/Nyukimin/RenCrow_Harness/internal/service"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func decodePayload(ev protocol.Event, out any) error { return json.Unmarshal(ev.Payload, out) }

func validUTF8(s string) bool { return utf8.ValidString(s) }

// cmdExec: exec --config ABS --workspace ABS --binding PROFILE --input-file ABS [--json]
// [--mode MODE] [--origin human|automation|unknown] [--idempotency-key KEY].
//
// One non-interactive Run: a new session on the workspace, one turn with the text of the file,
// driven by this process to its end. With --json standard output is JSON lines and nothing
// else: one {"type":"event","event":Event} for each event of the Run in the order they were
// committed, then one {"type":"result","result":RunResult}; without it, the final text of the
// Run goes to standard output and a few lines for a person to standard error.
//
// The exit code says how the Run ended (PROTOCOL section 2): 0 completed, 2 incomplete, 3
// rejected or blocked, 4 cancelled, 5 failed, 6 restart_required; 64 for an invalid command
// line, configuration or request value, and 1 for an operation that did not start a Run (a
// store that is not initialized or is busy, a refused request). A 0 is the Run's end, not the
// Task's: the result's verification says what was checked. Ctrl-C and SIGTERM record a stop for
// the Run, once, and wait for it to end for a bounded time; the stop being recorded does not
// make the Run cancelled, the Run's end does.
//
// The input is an explicit file, never standard input, and the input of an exec is automation
// unless the operator declares otherwise with --origin (and the profile allows it): whether
// standard input is a terminal decides nothing.
func cmdExec(ctx context.Context, args []string, out, errw io.Writer) *exitError {
	fs := newFlags("exec")
	configPath := fs.String("config", "", "absolute path of the configuration file")
	workspace := fs.String("workspace", "", "absolute path of the workspace")
	profile := fs.String("binding", "", "the binding profile the configuration names")
	inputFile := fs.String("input-file", "", "absolute path of the file whose text is the input")
	asJSON := fs.Bool("json", false, "write JSON lines: the events and then the result")
	mode := fs.String("mode", "", "the execution mode (default structured_only)")
	origin := fs.String("origin", "", "the operator's declaration of the input's origin (human, automation or unknown)")
	key := fs.String("idempotency-key", "", "the key of the operation (default: a new one)")
	if e := parseFlags(fs, args); e != nil {
		return e
	}
	switch *origin {
	case "", protocol.OriginHuman, protocol.OriginAutomation, protocol.OriginUnknown:
	default:
		return usageErr("--origin is human, automation or unknown")
	}
	text, e := readInputFile(*inputFile)
	if e != nil {
		return e
	}
	base := *key
	if base == "" {
		base = newKey("cli.exec")
	} else if len(base) > 100 {
		return usageErr("--idempotency-key is too long")
	}

	rt, e := openRuntime(ctx, *configPath, true)
	if e != nil {
		return e
	}
	defer rt.close()
	svc, e := rt.serviceWith(protocol.EntrypointCLIExec, *origin)
	if e != nil {
		return e
	}
	defer svc.Quiesce()

	stdout := &lockedWriter{w: out}
	stderr := &lockedWriter{w: errw}
	var emitErr error
	emit := eventEmitter(*asJSON, stdout, stderr, &emitErr)
	wake := make(chan struct{}, 1)
	conn := svc.NewConn(&runSink{onEvent: func(protocol.Event) { nudge(wake) }})
	defer conn.Close()

	sess, _, e := openSessionFor(ctx, svc, rt.dep, *workspace, *profile, *mode, base+".open")
	if e != nil {
		return e
	}
	limits, e := limitsFor(rt.dep, sess.PolicyRef)
	if e != nil {
		return e
	}
	in := protocol.StartInput{
		ThreadID: sess.ThreadID, Input: protocol.InputMessage{Text: text}, ContextBlocks: []protocol.ContextBlock{}, Upstream: nil,
		ExpectedContextRevision: sess.ContextRevision, ExpectedControlRevision: sess.ControlRevision, IdempotencyKey: base + ".start", Limits: limits,
	}
	params, err := protocol.Encode(in)
	if err != nil {
		return exitFor(err)
	}
	start, _, begin, err := svc.TurnStartDeferred(ctx, in, params)
	if err != nil {
		return exitFor(err)
	}
	// The stream is the Thread's events from the first (the session's creation, the Task's, the
	// Run's) and no other Run's or Task's: a key that is used again later, after the Task was
	// resumed, still gives the stream of the Run it names.
	runID, taskID := start.RunID, start.TaskID
	tail := &eventTail{svc: svc, threadID: sess.ThreadID, emit: emit, keep: func(ev protocol.Event) bool {
		return (ev.RunID == nil || *ev.RunID == runID) && (ev.TaskID == nil || *ev.TaskID == taskID)
	}}
	return finishRun(ctx, svc, tail, wake, start.RunID, begin, *asJSON, stdout, stderr, &emitErr)
}

// nudge wakes a waiter, once, without waiting for it.
func nudge(wake chan<- struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// eventEmitter is what a command does with each event of its Run: a JSON line on standard
// output, or, for a person, a short line on standard error for the ones worth one. A write that
// fails is kept (the first one) and ends the command with a failure.
func eventEmitter(asJSON bool, stdout, stderr io.Writer, emitErr *error) func(protocol.Event) {
	return func(ev protocol.Event) {
		if asJSON {
			if err := emitEvent(stdout, ev); err != nil && *emitErr == nil {
				*emitErr = err
			}
			return
		}
		if line := humanEvent(ev); line != "" {
			fmt.Fprintf(stderr, "[%s]\n", line)
		}
	}
}

// finishRun is the end of exec and of resume: the Run is started, and the command waits for
// it (emitting its events as they are committed), writes its result and returns the exit code
// of how it ended.
func finishRun(ctx context.Context, svc *service.Service, tail *eventTail, wake <-chan struct{}, runID string, begin func(),
	asJSON bool, stdout, stderr io.Writer, emitErr *error) *exitError {
	if begin != nil {
		begin()
	}
	info, e := waitRun(svc, runID, tail, wake, ctx.Done(), func(s string) { fmt.Fprintf(stderr, "%s\n", s) })
	if e != nil {
		return e
	}
	if info.Result == nil {
		return failure("the run is over and has no result")
	}
	res := *info.Result
	if asJSON {
		if err := emitResult(stdout, res); err != nil {
			return failure("the result could not be written")
		}
	} else if _, err := fmt.Fprint(stdout, res.FinalText); err != nil {
		return failure("the result could not be written")
	} else if res.FinalText != "" {
		fmt.Fprintln(stdout)
	}
	if *emitErr != nil {
		return failure("the events could not be written")
	}
	fmt.Fprintf(stderr, "run %s %s %s (verification: %s)\n", res.RunID, res.Status, res.Code, verificationSummary(res.Verification))
	if code := exitCodeOfStatus(res.Status); code != ExitOK {
		return &exitError{code: code, msg: fmt.Sprintf("the run ended %s (%s)", res.Status, res.Code)}
	}
	return nil
}
