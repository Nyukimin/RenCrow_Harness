package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/service"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The exit codes of a command that drove a Run to its end (PROTOCOL section 2). 0 for a Run
// that completed says nothing of the Task: read the result's verification.
const (
	ExitIncomplete      = 2
	ExitBlocked         = 3 // rejected and blocked
	ExitCancelled       = 4
	ExitFailed          = 5
	ExitRestartRequired = 6
)

// exitCodeOfStatus is the exit code of a Run that ended with status.
func exitCodeOfStatus(status string) int {
	switch status {
	case "completed":
		return ExitOK
	case "incomplete":
		return ExitIncomplete
	case "rejected", "blocked":
		return ExitBlocked
	case "cancelled":
		return ExitCancelled
	case "failed":
		return ExitFailed
	case "restart_required":
		return ExitRestartRequired
	}
	return ExitFailure
}

// orphanedAfter is how long a Run that is not over and that no driver of this process holds is
// waited for: another process may be driving it, and then it is its own to end, and this command
// says so rather than wait without a bound.
const orphanedAfter = 3 * time.Second

// stopGrace is how long a command that was told to stop waits for the Run to end: a stop is a
// request that is recorded at once and acted on as the Run reaches a place it can, and what
// the Run did before it saw the request is part of the result.
const stopGrace = 30 * time.Second

// newKey is a fresh idempotency key: one operation, never repeated by chance.
func newKey(prefix string) string {
	return prefix + "." + strings.ReplaceAll(identity.NewReceiptID().String(), "_", ".")
}

// lockedWriter serializes the writes of several goroutines to one stream: a line is never
// cut by another's.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// runSink is the notifier of a command's connection: it hands what the Service announces of the
// Runs the process drives (the committed events, and the provisional text of a generation) to the
// command.
type runSink struct {
	onEvent func(protocol.Event)
	onDelta func(protocol.ProgressDelta)
	onReset func(protocol.ProgressReset)
}

func (s *runSink) ProgressDelta(d protocol.ProgressDelta) {
	if s.onDelta != nil {
		s.onDelta(d)
	}
}

func (s *runSink) ProgressReset(r protocol.ProgressReset) {
	if s.onReset != nil {
		s.onReset(r)
	}
}

func (s *runSink) Event(ev protocol.Event) {
	if s.onEvent != nil {
		s.onEvent(ev)
	}
}

// serviceWith builds the Service of a command that drives Runs: its inputs are those of the
// entrypoint, and declared is the operator's explicit declaration of their origin.
func (r *runtime) serviceWith(entrypoint, declared string) (*service.Service, *exitError) {
	svc, err := service.New(service.Options{Deployment: r.dep, Store: r.store, Entrypoint: entrypoint, BuildRevision: buildRevision(),
		Writers: r.writers, Model: r.model, DeclaredOrigin: declared})
	if err != nil {
		return nil, failure("the service cannot be built: %v", err)
	}
	return svc, nil
}

// The execution mode of a session a command opens when none is asked for: the one that
// grants the least.
const defaultMode = protocol.ModeStructuredOnly

// workspaceFor resolves the workspace a command was pointed at to the configured workspace
// that is it: the path must be absolute and clean, and its real path exactly one of the roots
// the configuration allows. Anything else is not a workspace of this deployment.
func workspaceFor(dep *config.Deployment, path string) (string, config.Workspace, *exitError) {
	if e := requireAbs("--workspace", path); e != nil {
		return "", config.Workspace{}, e
	}
	if filepath.Clean(path) != path {
		return "", config.Workspace{}, usageErr("--workspace must be written in clean form")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", config.Workspace{}, usageErr("--workspace is not a directory that exists")
	}
	ws, ok := dep.Workspace(real)
	if !ok {
		return "", config.Workspace{}, usageErr("--workspace is not a workspace the configuration allows")
	}
	return real, ws, nil
}

// openSessionFor opens a new session on the workspace with the binding the profile name
// stands for, through the Service (session/open), and returns it with the events the call
// committed.
func openSessionFor(ctx context.Context, svc *service.Service, dep *config.Deployment, workspace, profile, mode, key string) (protocol.SessionInfo, []protocol.Event, *exitError) {
	real, ws, e := workspaceFor(dep, workspace)
	if e != nil {
		return protocol.SessionInfo{}, nil, e
	}
	if profile == "" {
		return protocol.SessionInfo{}, nil, usageErr("--binding is required")
	}
	binding, err := dep.Binding(profile)
	if err != nil {
		return protocol.SessionInfo{}, nil, usageErr("--binding is not a binding profile the configuration names")
	}
	switch mode {
	case "":
		mode = defaultMode
	case protocol.ModeStructuredOnly, protocol.ModeTrustedHost, protocol.ModeIsolated:
	default:
		return protocol.SessionInfo{}, nil, usageErr("--mode is structured_only, trusted_host or isolated")
	}
	in := protocol.SessionOpenInput{WorkspacePath: real, Binding: binding, PolicyRef: ws.PolicyRef, ExecutionMode: mode, IdempotencyKey: key}
	params, err := protocol.Encode(in)
	if err != nil {
		return protocol.SessionInfo{}, nil, exitFor(err)
	}
	res, events, err := svc.SessionOpen(ctx, params)
	if err != nil {
		return protocol.SessionInfo{}, nil, exitFor(err)
	}
	return res.Session, events, nil
}

// limitsFor is the limits of a Run the command starts when it is given none: the most the
// session's policy and the host allow, which is what the configuration says a Run may have.
func limitsFor(dep *config.Deployment, policyRef string) (protocol.Limits, *exitError) {
	caps, err := dep.EffectiveCaps(policyRef)
	if err != nil {
		return protocol.Limits{}, failure("the limits of the session's policy cannot be resolved")
	}
	return caps, nil
}

// clampLimits holds limits that were stored with an earlier Run to what the host allows now:
// a Run is never sent more than the current caps, and what was lowered is said.
func clampLimits(l, caps protocol.Limits) (out protocol.Limits, lowered bool) {
	out = l
	lower := func(v *int64, limit int64) {
		if *v > limit {
			*v, lowered = limit, true
		}
	}
	lower(&out.MaxModelSteps, caps.MaxModelSteps)
	lower(&out.MaxToolCallsPerStep, caps.MaxToolCallsPerStep)
	lower(&out.DeadlineSeconds, caps.DeadlineSeconds)
	lower(&out.MaxCaptureBytes, caps.MaxCaptureBytes)
	lower(&out.MaxGenerationAttempts, caps.MaxGenerationAttempts)
	return out, lowered
}

// humanEvent is the one line a person sees of an event that is worth one, or "".
func humanEvent(ev protocol.Event) string {
	switch ev.Type {
	case protocol.EventRunStarted:
		return "run started"
	case protocol.EventActionPrepared:
		var p protocol.ActionPreparedPayload
		if err := decodePayload(ev, &p); err == nil && p.Kind == "tool" {
			return "tool " + p.Name
		}
	case protocol.EventModelRetryScheduled:
		return "retrying the generation"
	case protocol.EventCheckpointCommitted:
		return "context compacted"
	case protocol.EventControlCancelRequest:
		return "stop recorded"
	case protocol.EventRunTerminal:
		code := ""
		if ev.Code != nil {
			code = *ev.Code
		}
		return "run ended " + code
	}
	return ""
}

// emitEvent writes one event as a line of the JSON-lines stream of exec.
func emitEvent(w io.Writer, ev protocol.Event) error {
	body, err := protocol.Encode(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "{\"type\":\"event\",\"event\":%s}\n", body)
	return err
}

// emitResult writes the closing line of the stream: the Run's result.
func emitResult(w io.Writer, res protocol.RunResult) error {
	body, err := protocol.Encode(res)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "{\"type\":\"result\",\"result\":%s}\n", body)
	return err
}

// eventTail reads the committed events of a Thread in the order they were committed, once
// each. Reading them from the store, and not from what the Service announces, is what keeps
// the stream in order whichever call or driver committed them: a stop recorded by a call and
// the end of the Run its driver commits right after are announced by two paths that can pass
// one another, and the store has one order.
type eventTail struct {
	svc      *service.Service
	threadID string
	after    int64
	keep     func(protocol.Event) bool
	emit     func(protocol.Event)
}

// drain emits every event committed since the last call.
func (t *eventTail) drain() *exitError {
	for {
		page, err := t.svc.EventsRead(context.Background(), protocol.EventsReadInput{ThreadID: t.threadID, AfterSeq: t.after, Limit: 500})
		if err != nil {
			return exitFor(err)
		}
		for _, ev := range page.Events {
			if t.keep == nil || t.keep(ev) {
				t.emit(ev)
			}
		}
		t.after = page.NextAfterSeq
		if !page.HasMore {
			return nil
		}
	}
}

// waitRun waits for a Run to be over, emitting its events as they are committed, and returns
// it. stop is the command's own signal that it was told to stop (Ctrl-C, SIGTERM): the Run's
// stop is recorded once, and the wait goes on for a bounded time, because a stop is a request
// and what the Run did meanwhile is the result. wake is told when the Service announces an
// event (the poll is only the fallback). A Run that is not over and that no driver of this
// process holds (it was admitted by another request, or its driver gave up) is reported, not
// waited for for ever.
func waitRun(svc *service.Service, runID string, tail *eventTail, wake <-chan struct{}, stop <-chan struct{}, note func(string)) (protocol.RunInfo, *exitError) {
	bg := context.Background()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	var grace <-chan time.Time
	stopped := false
	var orphanSince time.Time
	for {
		if e := tail.drain(); e != nil {
			return protocol.RunInfo{}, e
		}
		info, err := svc.RunGet(bg, protocol.RunGetInput{RunID: runID})
		if err != nil {
			return protocol.RunInfo{}, exitFor(err)
		}
		if info.Terminal {
			// What was committed up to and with the Run's end, all of it, before the result.
			if e := tail.drain(); e != nil {
				return protocol.RunInfo{}, e
			}
			return info, nil
		}
		switch {
		case svc.Driving(runID):
			orphanSince = time.Time{}
		case orphanSince.IsZero():
			orphanSince = time.Now()
		case time.Since(orphanSince) > orphanedAfter:
			return info, failure("the run is not driven by this process and has not ended (inspect it with `inspect --run %s`)", runID)
		}
		select {
		case <-stop:
			stop = nil
			if !stopped {
				stopped = true
				grace = time.After(stopGrace)
				recordStop(svc, info, note)
			}
		case <-grace:
			return info, failure("the run did not end within %s of the stop that was recorded for it", stopGrace)
		case <-wake:
		case <-tick.C:
		}
	}
}

// recordStop records a stop for the Run (turn/interrupt), once, at the control revision the
// Run is at now. It only records: that the Run stops is for the Run to show.
func recordStop(svc *service.Service, info protocol.RunInfo, note func(string)) {
	bg := context.Background()
	for attempt := 0; attempt < 2; attempt++ {
		in := protocol.InterruptInput{RunID: info.RunID, ExpectedControlRevision: info.ControlRevision, IdempotencyKey: newKey("cli.stop")}
		params, err := protocol.Encode(in)
		if err != nil {
			return
		}
		rec, _, err := svc.TurnInterrupt(bg, params)
		switch {
		case err == nil:
			note(fmt.Sprintf("stop recorded for the run (%s); waiting for it to end", rec.Code))
			return
		case protocol.CodeOf(err) == protocol.CodeRevisionConflict && attempt == 0:
			fresh, gerr := svc.RunGet(bg, protocol.RunGetInput{RunID: info.RunID})
			if gerr != nil || fresh.Terminal {
				return
			}
			info = fresh
		default:
			note("the stop could not be recorded: " + errCode(err))
			return
		}
	}
}

func errCode(err error) string {
	if c := protocol.CodeOf(err); c != "" {
		return c
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "DEADLINE_EXCEEDED"
	}
	return "the operation failed"
}

// readInputFile reads the text of an exec from a file: an absolute path to a regular file,
// at most the protocol's limit, valid UTF-8, with something in it.
func readInputFile(path string) (string, *exitError) {
	if e := requireAbs("--input-file", path); e != nil {
		return "", e
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", usageErr("--input-file is not a file that can be read")
	}
	const max = 8 << 20
	if info.Size() > max {
		return "", usageErr("--input-file is larger than the protocol allows (%d bytes)", max)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", usageErr("--input-file cannot be read")
	}
	text := string(data)
	if !validUTF8(text) {
		return "", usageErr("--input-file is not valid UTF-8")
	}
	if strings.TrimSpace(text) == "" {
		return "", usageErr("--input-file has no text")
	}
	return text, nil
}
