package kernel

import (
	"context"
	"errors"

	"github.com/Nyukimin/RenCrow_Harness/internal/extensions"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The host's fixed hooks at the points the Kernel owns (HOST_ASSETS section 4): before and
// after a model generation, before and after a compaction, and the end of the Run. The
// points of a Tool call are the Tool runtime's (internal/tools), and both use the one Runner.
//
// What a hook point may do is fixed by the point, and the Driver keeps to it:
//
//   - before_model and before_compact are called before the thing is begun, and a deny or
//     a failure ends the Run blocked (HOST_HOOK_DENIED, HOST_HOOK_FAILED) with nothing
//     begun: no generation attempt was reserved, no request was sent, no candidate was
//     made. They sit before the transaction that reserves a generation (which records the
//     Action, the dispatch and the consumed attempt together) and before the compaction's
//     snapshot is read, never between the two halves of a commit.
//   - after_model, after_compact and run_terminal are called after the thing is recorded
//     and can only continue. They run even when the Run is being stopped; a hook that does
//     not answer validly leaves a diagnosis and changes nothing the Run recorded.
//
// A hook is told identifiers and revisions only. Nothing here reads a hook's answer for
// anything but the decision.

// hookEnd is how a hook before something ended the work: the code of the Run's end, or that
// the driver no longer owns the Run.
type hookEnd struct {
	code    string
	abandon bool
}

// hooksOn reports whether there is a hook to call. A Driver without a Runner (a test's, or a
// deployment that lists none) calls nothing, and the Kernel's own safety handling is the same.
func (d *Driver) hooksOn() bool { return d.o.Hooks.Active() }

// hookInput is what a hook at a point of this Run is told: identifiers and revisions. The
// control revision is the one the Run holds (its fence's; a stop that was already pending
// when the driver began is held as -1 and told as 0), not a fresh read. An empty Evidence ID
// is left out: a record that does not exist is not named.
func (d *Driver) hookInput(rs *runState, p extensions.HookPoint, actionID, stage *string, evidence []string, code *string) extensions.HookInput {
	ids := make([]string, 0, len(evidence))
	for _, id := range evidence {
		if id != "" && len(ids) < 128 {
			ids = append(ids, id)
		}
	}
	return extensions.HookInput{
		Hook: p, ThreadID: rs.rec.ThreadID, RunID: rs.rec.RunID, ActionID: actionID, Stage: stage,
		ContextRevision: max(rs.snap.ContextRevision, 0), ControlRevision: max(rs.fence.ControlRevision, 0), EvidenceIDs: ids, Code: code,
	}
}

// hookRecorder is how a hook leaves a private record of the Run: an Evidence under the Run's
// fence. A store error that says the writer lost the Thread (or that the Run is no longer
// running) is the driver's to see, not the hook's failure.
func (d *Driver) hookRecorder(rs *runState) extensions.Recorder {
	return func(ctx context.Context, purpose string, data []byte) (string, error) {
		id, err := d.o.Store.RecordEvidence(ctx, rs.fence, purpose, "application/json", false, data)
		if errors.Is(err, sqlite.ErrWriterLost) || errors.Is(err, sqlite.ErrRunNotRunning) {
			return "", extensions.Fatal(err)
		}
		return id, err
	}
}

// beforeHook calls the hooks of a point before something is begun. It returns nil when the
// work may go on. runCtx is the Run's context: a stop recorded while a hook decides is the
// Run's end (cancelled, not the hook's failure), and a hook that is still deciding when the
// Run's time is up is the deadline's.
func (d *Driver) beforeHook(runCtx context.Context, rs *runState, in extensions.HookInput) *hookEnd {
	if !d.hooksOn() {
		return nil
	}
	v := d.o.Hooks.Call(runCtx, in, d.hookRecorder(rs))
	if v.Over {
		d.o.Diag("a hook went over its design budget")
	}
	switch {
	case v.Fatal != nil:
		d.o.Diag("the run is no longer this driver's: a hook's record found the writer gone")
		return &hookEnd{abandon: true}
	case v.Deny:
		return &hookEnd{code: CodeHostHookDenied}
	case v.Ended:
		// The Run is being stopped: its stop, not the hook, is why the work was not begun.
		return &hookEnd{code: stopCode(runCtx, nil)}
	case v.Failed:
		d.o.Diag("a hook at %s did not answer validly: %s", in.Hook, v.Reason)
		return &hookEnd{code: CodeHostHookFailed}
	}
	return nil
}

// afterHook calls the hooks of a point after something was recorded. It reports whether the
// driver must let go of the Run (the writer is gone); nothing else a hook does is seen.
func (d *Driver) afterHook(ctx context.Context, rs *runState, in extensions.HookInput) (abandon bool) {
	if !d.hooksOn() {
		return false
	}
	if err := d.o.Hooks.CallAfter(ctx, in, d.hookRecorder(rs), d.o.Diag); err != nil {
		d.o.Diag("the run is no longer this driver's: a hook's record found the writer gone")
		return true
	}
	return false
}

// stepOf is the step result a hook that ended the work gives: the Run's failure, with no
// generation state (nothing was sent), or the abandonment of a Run the driver lost.
func (e *hookEnd) stepOf() stepResult {
	if e.abandon {
		return stepResult{abandon: true}
	}
	return failed(Failure{Code: e.code})
}

// modelHookInput is the input of a hook at a model generation: the stage, and the Action
// when there is one (a retry's Attempt belongs to the Action of the Attempt that failed; the
// first Attempt of an Action has none before the reservation that makes it).
func (d *Driver) modelHookInput(rs *runState, p extensions.HookPoint, stage string, actionID *string, evidence []string, code *string) extensions.HookInput {
	return d.hookInput(rs, p, actionID, protocol.Str(stage), evidence, code)
}
