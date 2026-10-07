package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// cmdResume: resume --config ABS --task TASK_ID --last-run RUN_ID [--json] [--idempotency-key KEY].
//
// A new Run of an ended Task (run/resume), in the same Thread, and driven by this process to
// its end exactly as exec drives its own: the same output, the same exit codes. The Task is
// the one named and the last Run is the one named, or the request is refused (it is the
// Service that holds that: the Task's last Run must be the one the command says it is, the
// Task must be one that can be resumed, and what is unknown of it is never run again).
//
// The Run is sent complete limits: those the earlier Run was given, held to what the host and
// the session's policy allow now (a limit that was higher is lowered, and the command says so);
// the new Run's deadline is counted from the request. The checkpoint is the one the Thread
// stands on and the binding the one it had. The control revision is the Thread's now, read
// just before.
func cmdResume(ctx context.Context, args []string, out, errw io.Writer) *exitError {
	fs := newFlags("resume")
	configPath := fs.String("config", "", "absolute path of the configuration file")
	task := fs.String("task", "", "the task to resume")
	lastRun := fs.String("last-run", "", "the last run of the task")
	asJSON := fs.Bool("json", false, "write JSON lines: the events and then the result")
	key := fs.String("idempotency-key", "", "the key of the operation (default: a new one)")
	if e := parseFlags(fs, args); e != nil {
		return e
	}
	if _, err := identity.ParseTaskID(*task); err != nil {
		return usageErr("--task is not a task ID")
	}
	if _, err := identity.ParseRunID(*lastRun); err != nil {
		return usageErr("--last-run is not a run ID")
	}
	if *key == "" {
		*key = newKey("cli.resume")
	} else if len(*key) > 120 {
		return usageErr("--idempotency-key is too long")
	}

	rt, e := openRuntime(ctx, *configPath, true)
	if e != nil {
		return e
	}
	defer rt.close()
	svc, e := rt.serviceWith(protocol.EntrypointCLIExec, "")
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

	prior, err := svc.RunGet(ctx, protocol.RunGetInput{RunID: *lastRun})
	if err != nil {
		return exitFor(err)
	}
	if prior.TaskID != *task {
		return usageErr("--last-run is not a run of --task")
	}
	sess, err := svc.SessionGet(ctx, protocol.SessionGetInput{ThreadID: prior.ThreadID})
	if err != nil {
		return exitFor(err)
	}
	caps, e := limitsFor(rt.dep, sess.PolicyRef)
	if e != nil {
		return e
	}
	limits, lowered := clampLimits(prior.EffectiveLimits, caps)
	if lowered {
		fmt.Fprintln(stderr, "the limits of the earlier run were lowered to what the host allows now")
	}
	in := protocol.ResumeInput{TaskID: *task, ExpectedLastRunID: *lastRun, CheckpointID: nil, ExpectedControlRevision: sess.ControlRevision,
		Binding: nil, IdempotencyKey: *key, Limits: limits}
	params, err := protocol.Encode(in)
	if err != nil {
		return exitFor(err)
	}
	res, _, begin, err := svc.RunResumeDeferred(ctx, params)
	if err != nil {
		return exitFor(err)
	}
	// The stream is the new Run's events, from where the earlier Run's own ended.
	newRun := res.RunID
	tail := &eventTail{svc: svc, threadID: res.ThreadID, after: prior.LastEventSeq, emit: emit,
		keep: func(ev protocol.Event) bool { return ev.RunID != nil && *ev.RunID == newRun }}
	return finishRun(ctx, svc, tail, wake, newRun, begin, *asJSON, stdout, stderr, &emitErr)
}
