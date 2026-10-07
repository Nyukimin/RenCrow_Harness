package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/extensions"
	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/files"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/process"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolerr"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolview"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Point is a place in a dispatch where a test can stop the process.
type Point string

// The points of a dispatch, in order.
const (
	// PointAfterBind: every call of the response is an Action; none has started.
	PointAfterBind Point = "after_bind"
	// PointAfterDispatchStart: the dispatch-start is durable; the Tool has not run.
	PointAfterDispatchStart Point = "after_dispatch_start"
	// PointAfterExecute: the Tool has run and its effect exists; nothing of its result
	// is recorded.
	PointAfterExecute Point = "after_execute"
)

// ErrKilled is what a fault hook returns to stop a dispatch where it stands, as if the
// process had died there: nothing more is written and nothing more is run.
var ErrKilled = errors.New("tools: stopped at a fault-injection point")

// Hooks are test seams. Production code leaves them zero.
type Hooks struct {
	Fault func(p Point, tool string) error
}

// Options configure NewRuntime.
type Options struct {
	Deployment *config.Deployment
	Store      *sqlite.Store
	Hooks      Hooks
	// HookRunner runs the host's fixed hooks at before_tool and after_tool (HOST_ASSETS
	// section 4). Nil, or a runner with nothing to call, leaves every dispatch as it was.
	HookRunner *extensions.Runner
	// Diag receives short diagnostics: never request text, a key or a path. Nil discards them.
	Diag func(format string, args ...any)
}

// Runtime is the Tool runtime of one process: it resolves each Run's frozen policy and
// gives each Run its dispatcher.
type Runtime struct {
	dep    *config.Deployment
	store  *sqlite.Store
	hooks  Hooks
	runner *extensions.Runner
	diag   func(format string, args ...any)
}

// NewRuntime builds the runtime. The deployment and the store are required.
func NewRuntime(o Options) (*Runtime, error) {
	if o.Deployment == nil || o.Store == nil {
		return nil, errors.New("tools: the runtime needs a deployment and a store")
	}
	if _, err := loadSpecs(); err != nil {
		return nil, err
	}
	diag := o.Diag
	if diag == nil {
		diag = func(string, ...any) {}
	}
	return &Runtime{dep: o.Deployment, store: o.Store, hooks: o.Hooks, runner: o.HookRunner, diag: diag}, nil
}

// PolicyFor resolves the Tool authority of a Run from the deployment and holds it to
// the revision the Run's Thread froze: a registry, a profile or a workspace that
// changed since is ErrPolicyChanged, never a quietly different authority.
func (rt *Runtime) PolicyFor(rec sqlite.RunRecord) (*RunPolicy, error) {
	ep, err := rt.dep.EffectivePolicy(rec.PolicyRef, rec.WorkspacePath, rec.ExecutionMode)
	if err != nil || ep.Revision != rec.PolicyRevision {
		return nil, ErrPolicyChanged
	}
	return newRunPolicy(ep), nil
}

// RunTools dispatches the Tool calls of one Run.
type RunTools struct {
	rt    *Runtime
	rec   sqlite.RunRecord
	fence sqlite.Fence
	pol   *RunPolicy
	// contextRevision is the Thread's context revision the Run last read: what a hook is told.
	contextRevision int64
}

// SetContextRevision tells the dispatcher the Thread's context revision the Run is working
// at, for the hooks' input. It changes nothing a dispatch decides.
func (r *RunTools) SetContextRevision(rev int64) { r.contextRevision = rev }

// ForRun makes the dispatcher of a Run, which acts under fence and pol.
func (rt *Runtime) ForRun(rec sqlite.RunRecord, fence sqlite.Fence, pol *RunPolicy) *RunTools {
	return &RunTools{rt: rt, rec: rec, fence: fence, pol: pol}
}

func (r *RunTools) fault(p Point, tool string) error {
	if f := r.rt.hooks.Fault; f != nil {
		return f(p, tool)
	}
	return nil
}

// dbCtx is the context of a write that must happen even when the Run is being stopped.
func dbCtx(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

// CallState is one call of a bound response.
type CallState struct {
	Call  Call
	Bound sqlite.BoundCall
	// Refusal is the policy's refusal, decided before the call was bound.
	Refusal *toolerr.Error
	profile config.ProcessProfile
}

// Batch is the Tool calls of one accepted response, bound to Actions.
type Batch struct {
	ResponseID         string
	AssistantMessageID string
	Calls              []*CallState
	// Unresolved are the Actions whose outcome is unknown.
	Unresolved []string
	// Applied is set once the exchange is part of the context.
	Applied bool
}

// Stop is why a Run cannot go on after a call: the code of the failure that ends it.
type Stop struct{ Code string }

// Codes of a Stop that the Tool runtime itself names.
const (
	StopEffectUnknown   = "EFFECT_OUTCOME_UNKNOWN"
	StopPolicyAmbiguous = "POLICY_AMBIGUOUS"
	StopPolicyChanged   = "POLICY_CHANGED"
	StopCancelled       = "CANCELLED"
	StopDeadline        = "DEADLINE_EXCEEDED"
	StopDriver          = "DRIVER_STOPPED"
	// StopHookDenied and StopHookFailed: a hook before the call denied it, or did not
	// answer validly in time. The call was not dispatched.
	StopHookDenied = extensions.CodeHookDenied
	StopHookFailed = extensions.CodeHookFailed
)

// End is how one call ended, for the machine that decides what the Run does next.
type End struct {
	// Effect is the call's effect state: not_started (refused or never started),
	// completed, failed, cancelled or unknown.
	Effect string
	// Stop, when set, ends the Run.
	Stop *Stop
}

// Succeeded: the call ran to completion. Anything else stops the rest of its response.
func (e End) Succeeded() bool { return e.Effect == "completed" && e.Stop == nil }

// decide is the part of F16 that needs no filesystem and no clock: the Tool is
// available, the arguments are inside the policy's paths and profiles. It refuses; it
// never repairs a request into one that would pass.
func (r *RunTools) decide(c Call) (config.ProcessProfile, *toolerr.Error) {
	var none config.ProcessProfile
	if !r.pol.Enabled(c.Name) {
		return none, toolerr.Reject(toolerr.CodeToolNotAllowed, "the Tool is not available under this policy")
	}
	sc := r.pol.Scope()
	asErr := func(err error) *toolerr.Error {
		if err == nil {
			return nil
		}
		if te, ok := toolerr.As(err); ok {
			return te
		}
		return toolerr.Reject(toolerr.CodePolicyRejected, "the request was refused")
	}
	switch a := c.args.(type) {
	case readArgs:
		_, err := sc.Check(a.Path, files.Read)
		return none, asErr(err)
	case searchArgs:
		if _, err := sc.CheckSearch(a.Path); err != nil {
			return none, asErr(err)
		}
		if a.Mode == "regex" {
			if _, err := regexp.Compile(a.Query); err != nil {
				return none, toolerr.Reject(toolerr.CodeRegexInvalid, "the regular expression is not valid")
			}
		}
		return none, nil
	case createArgs:
		_, err := sc.Check(a.Path, files.Write)
		return none, asErr(err)
	case editArgs:
		_, err := sc.Check(a.Path, files.Write)
		return none, asErr(err)
	case evidenceArgs:
		return none, nil
	case execArgs:
		if r.pol.Mode != protocol.ModeTrustedHost {
			return none, toolerr.Reject(toolerr.CodeModeUnavailable, "process.exec needs trusted_host mode")
		}
		prof, err := process.Resolve(r.pol.processes, a.Executable, a.Argv)
		if err != nil {
			return none, asErr(err)
		}
		if !slices.Contains(r.pol.policy.EnvProfiles, a.EnvProfileRef) {
			return none, toolerr.Reject(toolerr.CodeEnvUnknown, "the environment profile is not one the policy allows")
		}
		if _, ok := r.pol.envs[a.EnvProfileRef]; !ok {
			return none, toolerr.Reject(toolerr.CodeEnvUnknown, "the environment profile is not defined")
		}
		if _, err := files.Normalize(a.Cwd); err != nil {
			return none, asErr(err)
		}
		segs, _ := files.Normalize(a.Cwd)
		if !sc.Allows(segs, files.Read) && !sc.Allows(segs, files.Write) {
			return none, toolerr.Reject(toolerr.CodePathOutside, "the directory is outside what the policy allows")
		}
		return prof, nil
	}
	return none, toolerr.Reject(toolerr.CodePolicyRejected, "the request was refused")
}

func viewError(e *toolerr.Error) *toolview.Error {
	if e == nil {
		return nil
	}
	return &toolview.Error{Code: e.Code, Message: e.Message}
}

// Bind (the first half of F16) records every Tool call of one accepted response as an
// Action. Each call is judged against the policy first, with nothing but the frozen
// policy: a refused call is recorded as refused (its Attempt failed, its answer an
// error the model reads) and never becomes dispatchable. The calls are bound once, in
// one transaction, with the assistant message that asked for them. responseID is the
// Harness's ID of the accepted response and text the visible text that came with the
// calls.
func (r *RunTools) Bind(ctx context.Context, responseID, text string, calls []Call) (*Batch, []protocol.Event, error) {
	if len(calls) == 0 {
		return nil, nil, errors.New("tools: nothing to bind")
	}
	rec := contextplan.ToolCallsRecord{ToolCalls: make([]modelport.ToolCall, 0, len(calls))}
	if text != "" {
		rec.Content = &text
	}
	for _, c := range calls {
		rec.ToolCalls = append(rec.ToolCalls, modelport.ToolCall{ID: c.ProviderToolCallID, Type: "function", Function: modelport.ToolFunction{Name: c.Name, Arguments: c.ArgumentsText}})
	}
	recordText, err := contextplan.EncodeToolCalls(rec)
	if err != nil {
		return nil, nil, err
	}
	batch := &Batch{ResponseID: responseID}
	in := sqlite.BindToolBatchInput{ResponseID: responseID, AssistantRecord: recordText, AssistantMessageID: identity.NewMessageID().String()}
	for _, c := range calls {
		cs := &CallState{Call: c}
		var refusal *toolerr.Error
		cs.profile, refusal = r.decide(c)
		cs.Refusal = refusal
		tc := sqlite.ToolCall{ActionID: identity.NewActionID().String(), AttemptID: identity.NewAttemptID().String(), ProviderToolCallID: c.ProviderToolCallID,
			Ordinal: c.Ordinal, Name: c.Name, ArgsBytes: c.ArgsBytes}
		if refusal != nil {
			text, err := toolview.Encode(toolview.View{Tool: c.Name, ActionID: tc.ActionID, EffectState: "not_started", CaptureComplete: true, Error: viewError(refusal)})
			if err != nil {
				return nil, nil, err
			}
			tc.Rejection = &sqlite.ToolRejection{Code: refusal.Code, ResultText: string(text)}
		}
		in.Calls = append(in.Calls, tc)
		batch.Calls = append(batch.Calls, cs)
	}
	bound, err := r.rt.store.BindToolBatch(dbCtx(ctx), r.fence, in)
	if err != nil {
		return nil, nil, err
	}
	batch.AssistantMessageID = bound.AssistantMessageID
	for i, b := range bound.Calls {
		batch.Calls[i].Bound = b
	}
	if err := r.fault(PointAfterBind, ""); err != nil {
		return batch, bound.Events, err
	}
	return batch, bound.Events, nil
}

func effectOfState(state string) string {
	switch state {
	case sqlite.ToolCompleted:
		return "completed"
	case sqlite.ToolFailed:
		return "failed"
	case sqlite.ToolCancelled:
		return "cancelled"
	case sqlite.ToolUnknown:
		return "unknown"
	}
	return "unknown"
}

// Execute (the rest of F16) dispatches call i of the batch. In order: the call must
// still be prepared (a call already dispatched or closed is never started again); the
// dispatch-start is made durable under the writer epoch, the control revision and the
// policy revision, in one transaction (a cancellation that came first wins and nothing
// runs); the Tool runs with no database lock held; and how it ended is recorded: output
// Evidence sealed, the answer kept, the Attempt and the Action ended.
//
// The returned events were committed. A non-nil error is a failure of the store or the
// fence (or ErrKilled), after which the driver writes nothing more for the call.
func (r *RunTools) Execute(ctx context.Context, b *Batch, i int) (End, []protocol.Event, error) {
	cs := b.Calls[i]
	if cs.Bound.State != sqlite.ToolPrepared {
		// Refused when bound, or dispatched before: it is not run now.
		end := End{Effect: effectOfState(cs.Bound.State)}
		if cs.Bound.State == sqlite.ToolFailed && cs.Refusal != nil {
			end.Effect = "not_started"
			if cs.Refusal.Code == toolerr.CodePolicyAmbiguous {
				end.Stop = &Stop{Code: StopPolicyAmbiguous}
			}
		} else if end.Effect == "unknown" || cs.Bound.State == sqlite.ToolDispatched || cs.Bound.State == sqlite.ToolRunning {
			end.Effect, end.Stop = "unknown", &Stop{Code: StopEffectUnknown}
			b.Unresolved = appendUnique(b.Unresolved, cs.Bound.ActionID)
		}
		return end, nil, nil
	}

	// before_tool comes before the dispatch-start: nothing is durable yet, so a hook that
	// denies leaves a call that was never started (it is closed as "not run" with the rest
	// of the response when the Run ends), and a stop that is recorded while the hook runs is
	// still settled by the dispatch-start's own compare-and-set below.
	if end, err := r.beforeTool(ctx, cs); err != nil || end != nil {
		if end == nil {
			return End{}, nil, err
		}
		return *end, nil, nil
	}

	started, err := r.rt.store.StartToolAttempt(dbCtx(ctx), r.fence, sqlite.StartToolInput{ActionID: cs.Bound.ActionID, AttemptID: cs.Bound.AttemptID, PolicyRevision: r.pol.Revision})
	switch {
	case errors.Is(err, sqlite.ErrControlChanged):
		return End{Effect: "not_started", Stop: &Stop{Code: StopCancelled}}, nil, nil
	case errors.Is(err, sqlite.ErrPolicyChanged):
		return End{Effect: "not_started", Stop: &Stop{Code: StopPolicyChanged}}, nil, nil
	case errors.Is(err, sqlite.ErrDeadlinePassed):
		return End{Effect: "not_started", Stop: &Stop{Code: StopDeadline}}, nil, nil
	case err != nil:
		return End{}, nil, err
	}
	events := started.Events
	if err := r.fault(PointAfterDispatchStart, cs.Call.Name); err != nil {
		return End{}, events, err
	}

	out := r.run(ctx, cs)
	if err := r.fault(PointAfterExecute, cs.Call.Name); err != nil {
		return End{}, events, err
	}
	if out.storeErr != nil {
		return End{}, events, out.storeErr
	}

	view := toolview.View{Tool: cs.Call.Name, ActionID: cs.Bound.ActionID, EffectState: out.effect, ExitCode: out.exit, CaptureComplete: out.captureComplete,
		EvidenceIDs: captureIDs(out.captures), Error: viewError(out.err)}
	if out.result != nil {
		raw, merr := json.Marshal(out.result)
		if merr != nil {
			return End{}, events, fmt.Errorf("tools: a result cannot be encoded: %w", merr)
		}
		view.Result = raw
	}
	text, err := toolview.Encode(view)
	if err != nil {
		return End{}, events, err
	}
	record := map[string]any{"effect_state": out.effect}
	if out.err != nil {
		record["code"] = out.err.Code
	}
	if out.exit != nil {
		record["exit_code"] = *out.exit
	}
	recordJSON, err := json.Marshal(record)
	if err == nil {
		recordJSON, err = protocol.EncodeCanonicalContract(recordJSON)
	}
	if err != nil {
		return End{}, events, err
	}
	done, err := r.rt.store.CompleteToolAttempt(dbCtx(ctx), r.fence, sqlite.CompleteToolInput{
		ActionID: cs.Bound.ActionID, AttemptID: cs.Bound.AttemptID, EffectState: out.effect, ExitCode: out.exit, CaptureComplete: out.captureComplete,
		Captures: out.captures, ResultText: string(text), ResponseID: b.ResponseID, ProviderCallID: cs.Call.ProviderToolCallID, CallOrdinal: cs.Call.Ordinal,
		ResultJSON: string(recordJSON),
	})
	if err != nil {
		return End{}, events, err
	}
	events = append(events, done.Events...)
	// The call is over and recorded; what a hook does after it cannot change that.
	r.afterTool(ctx, cs, out.effect, completedEvidence(done.Events))
	end := End{Effect: out.effect}
	switch out.effect {
	case "unknown":
		end.Stop = &Stop{Code: StopEffectUnknown}
		b.Unresolved = appendUnique(b.Unresolved, cs.Bound.ActionID)
	case "cancelled":
		// Why the call was stopped: a stop recorded for the Run (the context's cause), the
		// Run's deadline, or the driver's process.
		switch {
		case errors.Is(context.Cause(ctx), sqlite.ErrControlChanged):
			end.Stop = &Stop{Code: StopCancelled}
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			end.Stop = &Stop{Code: StopDeadline}
		default:
			end.Stop = &Stop{Code: StopDriver}
		}
	}
	return end, events, nil
}

func appendUnique(s []string, v string) []string {
	if slices.Contains(s, v) {
		return s
	}
	return append(s, v)
}

func captureIDs(c []sqlite.SealCapture) []string {
	var out []string
	for _, x := range c {
		out = append(out, x.EvidenceID)
	}
	return out
}

// Finish applies the response's Tool exchange to the Thread's context: the calls that
// are over keep their answers, a call that was never reached gets a "not run" answer
// for the reason given, and the assistant message and every answer enter the context
// together. It is idempotent.
func (r *RunTools) Finish(ctx context.Context, b *Batch, reason string) ([]protocol.Event, error) {
	if b == nil || b.Applied {
		return nil, nil
	}
	res, err := r.rt.store.PersistToolObservation(dbCtx(ctx), r.fence, b.ResponseID, reason)
	if err != nil {
		return nil, err
	}
	b.Applied = true
	for _, id := range res.Unknown {
		b.Unresolved = appendUnique(b.Unresolved, id)
	}
	return res.Events, nil
}

// outcome is what a Tool did, before it is recorded.
type outcome struct {
	effect          string
	exit            *int64
	err             *toolerr.Error
	result          any
	captureComplete bool
	captures        []sqlite.SealCapture
	storeErr        error
}

// failure turns an error of a Tool into the outcome it stands for.
func failure(err error) outcome {
	te, ok := toolerr.As(err)
	if !ok {
		te = toolerr.Fail(toolerr.CodeIO, "the Tool failed")
	}
	o := outcome{err: te, captureComplete: true}
	switch {
	case te.Code == toolerr.CodeCancelled:
		o.effect = "cancelled"
	case te.Class == toolerr.Unknown:
		o.effect = "unknown"
	default:
		o.effect = "failed"
	}
	return o
}

func success(result any) outcome {
	return outcome{effect: "completed", result: result, captureComplete: true}
}

// run executes the Tool of a dispatched call. The Tool's paths are resolved again here,
// against the real filesystem, whatever was checked when the call was bound.
func (r *RunTools) run(ctx context.Context, cs *CallState) outcome {
	sc := r.pol.Scope()
	switch a := cs.Call.args.(type) {
	case readArgs:
		res, err := files.ReadFile(ctx, sc, files.ReadArgs{Path: a.Path, Range: a.Range, MaxBytes: a.MaxBytes})
		if err != nil {
			return failure(err)
		}
		return success(res)
	case searchArgs:
		res, err := files.SearchFiles(ctx, sc, files.SearchArgs{Path: a.Path, Query: a.Query, Mode: a.Mode, MaxResults: a.MaxResults, MaxBytes: a.MaxBytes})
		if err != nil {
			return failure(err)
		}
		return success(res)
	case createArgs:
		res, err := files.CreateFile(ctx, sc, files.CreateArgs{Path: a.Path, Text: a.Text})
		if err != nil {
			return failure(err)
		}
		return success(res)
	case editArgs:
		res, err := files.EditFile(ctx, sc, files.EditArgs{Path: a.Path, ExpectedHash: a.ExpectedHash, OldText: a.OldText, NewText: a.NewText})
		if err != nil {
			return failure(err)
		}
		return success(res)
	case evidenceArgs:
		return r.readEvidence(ctx, a)
	case execArgs:
		return r.execProcess(ctx, cs, a)
	}
	return failure(toolerr.Fail(toolerr.CodeInternal, "the Tool is unknown"))
}

// evidenceResult is the result of evidence.read.
type evidenceResult struct {
	EvidenceID        string      `json:"evidence_id"`
	ProjectionVersion string      `json:"projection_version"`
	ReturnedRange     files.Range `json:"returned_range"`
	TotalBytes        int64       `json:"total_bytes"`
	RawHash           string      `json:"raw_hash"`
	ProjectionHash    string      `json:"projection_hash"`
	CaptureComplete   bool        `json:"capture_complete"`
	Partial           bool        `json:"partial"`
	Encoding          string      `json:"encoding"`
	Data              string      `json:"data"`
}

// readEvidence is evidence.read: the stored bytes of an Evidence of this Thread, by the
// same rules as the evidence/read method. No Tool is run and nothing is written.
func (r *RunTools) readEvidence(ctx context.Context, a evidenceArgs) outcome {
	res, err := r.rt.store.ReadEvidenceInThread(ctx, r.rec.ThreadID, protocol.EvidenceReadInput{
		EvidenceID: a.EvidenceID, ProjectionVersion: a.ProjectionVersion, Range: protocol.ByteRange{Start: a.Range.Start, End: a.Range.End}})
	if err != nil {
		switch protocol.CodeOf(err) {
		case protocol.CodeForbidden:
			return failure(toolerr.Fail(toolerr.CodeEvidenceDenied, "no such evidence is readable here"))
		case protocol.CodeInvalidRange, protocol.CodeInvalidParams:
			return failure(toolerr.Fail(toolerr.CodeInvalidRange, "the range is not one this evidence can be read in"))
		case protocol.CodeBusy:
			return failure(toolerr.Fail(toolerr.CodeIO, "the evidence is still being captured"))
		case protocol.CodeUnsupportedContract:
			return failure(toolerr.Fail(toolerr.CodeProjection, "the evidence has no such projection"))
		case protocol.CodeIntegrityBlocked:
			return failure(toolerr.Fail(toolerr.CodeIO, "the evidence does not match its record"))
		}
		var pe *protocol.Error
		if errors.As(err, &pe) {
			return failure(toolerr.Fail(toolerr.CodeIO, "the evidence could not be read"))
		}
		return outcome{effect: "failed", err: toolerr.Fail(toolerr.CodeIO, "the evidence could not be read"), storeErr: nil, captureComplete: true}
	}
	data, err := base64.StdEncoding.DecodeString(res.DataBase64)
	if err != nil {
		return failure(toolerr.Fail(toolerr.CodeIO, "the evidence could not be decoded"))
	}
	er := evidenceResult{EvidenceID: res.EvidenceID, ProjectionVersion: res.ProjectionVersion, ReturnedRange: files.Range{Start: res.ReturnedRange.Start, End: res.ReturnedRange.End},
		TotalBytes: res.TotalBytes, RawHash: res.RawHash, ProjectionHash: res.ProjectionHash, CaptureComplete: res.CaptureComplete, Partial: res.Partial}
	if utf8.Valid(data) {
		er.Encoding, er.Data = "utf-8", string(data)
	} else {
		er.Encoding, er.Data = "base64", res.DataBase64
	}
	return success(er)
}

// Process output captured: how much of each stream is shown to the model in the
// answer, and the size of one stored chunk.
const (
	previewBytes = 4096
	chunkBytes   = 64 << 10
)

// captureSink stores a process's output as it arrives, in bounded chunks, and keeps the
// head of it for the answer. A chunk that cannot be stored ends the capture: the error
// is kept and the run is stopped.
type captureSink struct {
	ctx        context.Context
	store      *sqlite.Store
	fence      sqlite.Fence
	attemptID  string
	purpose    string
	evidenceID string
	stop       context.CancelFunc

	buf     []byte
	head    []byte
	ordinal int64
	offset  int64
	created bool
	err     error
}

func (c *captureSink) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if room := previewBytes - len(c.head); room > 0 {
		c.head = append(c.head, p[:min(len(p), room)]...)
	}
	c.buf = append(c.buf, p...)
	for len(c.buf) >= chunkBytes {
		if err := c.flush(c.buf[:chunkBytes]); err != nil {
			return 0, err
		}
		c.buf = c.buf[chunkBytes:]
	}
	return len(p), nil
}

func (c *captureSink) flush(data []byte) error {
	err := c.store.AppendCaptureChunk(dbCtx(c.ctx), c.fence, sqlite.CaptureChunk{
		EvidenceID: c.evidenceID, AttemptID: c.attemptID, Purpose: c.purpose, Ordinal: c.ordinal, ByteStart: c.offset, Data: slices.Clone(data)})
	if err != nil {
		c.err = err
		c.stop()
		return err
	}
	c.created = true
	c.ordinal++
	c.offset += int64(len(data))
	return nil
}

func (c *captureSink) finish() error {
	if c.err == nil && len(c.buf) > 0 {
		_ = c.flush(c.buf)
		c.buf = nil
	}
	return c.err
}

func (c *captureSink) total() int64 { return c.offset + int64(len(c.buf)) }

// preview is the head of the stream as text: cut at a character boundary, and with
// anything that is not UTF-8 replaced (the stored Evidence has the exact bytes).
func (c *captureSink) preview() (string, bool) {
	h := c.head
	truncated := c.total() > int64(len(h))
	// A character cut by the end of the head is dropped whole rather than shown half.
	for i := 1; i <= 3 && i <= len(h); i++ {
		if utf8.RuneStart(h[len(h)-i]) {
			if !utf8.FullRune(h[len(h)-i:]) {
				h = h[:len(h)-i]
			}
			break
		}
	}
	return strings.ToValidUTF8(string(h), "\uFFFD"), truncated
}

type streamResult struct {
	EvidenceID       string `json:"evidence_id"`
	TotalBytes       int64  `json:"total_bytes"`
	CaptureComplete  bool   `json:"capture_complete"`
	Preview          string `json:"preview"`
	PreviewTruncated bool   `json:"preview_truncated"`
}

type execResult struct {
	ExitCode *int64       `json:"exit_code"`
	Signaled bool         `json:"signaled"`
	TimedOut bool         `json:"timed_out"`
	Stdout   streamResult `json:"stdout"`
	Stderr   streamResult `json:"stderr"`
	Profile  string       `json:"profile"`
}

// execProcess is process.exec: the one profile the request resolved to, an environment
// of exactly the profile's values, the workspace directory it asked for, the output
// captured in chunks under the Run's capture limit, and the process tree stopped at the
// timeout, the capture limit or the Run's end.
func (r *RunTools) execProcess(ctx context.Context, cs *CallState, a execArgs) outcome {
	prof, err := process.Resolve(r.pol.processes, a.Executable, a.Argv)
	if err != nil || prof.Name != cs.profile.Name {
		return failure(toolerr.Reject(toolerr.CodePolicyRejected, "the process profile is not the one the call was bound to"))
	}
	env, err := process.BuildEnv(r.pol.envs[a.EnvProfileRef])
	if err != nil {
		return failure(err)
	}
	dir, err := r.pol.Scope().ResolveDir(a.Cwd)
	if err != nil {
		return failure(err)
	}
	used, err := r.rt.store.RunCaptureBytes(ctx, r.rec.RunID)
	if err != nil {
		return outcome{storeErr: err}
	}
	limit := max(r.rec.Limits.MaxCaptureBytes-used, 0)
	timeout := time.Duration(a.TimeoutSeconds) * time.Second
	if left := r.rec.DeadlineAt.Sub(r.rt.store.Now()); left < timeout {
		timeout = max(left, time.Millisecond)
	}

	pctx, stop := context.WithCancel(ctx)
	defer stop()
	sink := func(purpose string) *captureSink {
		return &captureSink{ctx: ctx, store: r.rt.store, fence: r.fence, attemptID: cs.Bound.AttemptID, purpose: purpose,
			evidenceID: identity.NewEvidenceID().String(), stop: stop}
	}
	stdout, stderr := sink(sqlite.PurposeToolStdout), sink(sqlite.PurposeToolStderr)
	var startErr error
	res, runErr := process.Run(pctx, process.Spec{
		Executable: a.Executable, Argv: a.Argv, Dir: dir.Abs, Env: env, Timeout: timeout, CaptureLimit: limit, Stdout: stdout, Stderr: stderr,
		OnStart: func(id process.Identity) error {
			startErr = r.rt.store.RecordProcessStart(dbCtx(ctx), r.fence, cs.Bound.AttemptID, id.Incarnation, id.Encode())
			return startErr
		},
	})
	werr := errors.Join(stdout.finish(), stderr.finish())
	if werr != nil || startErr != nil {
		// What the process printed could not be kept (or its start could not be
		// recorded): the writer lost its Thread, or the store failed. The driver stops
		// writing for this call; what the process did is left unknown.
		return outcome{effect: "unknown", storeErr: errors.Join(werr, startErr)}
	}

	o := outcome{captureComplete: res.CaptureComplete()}
	o.captures = []sqlite.SealCapture{
		{EvidenceID: stdout.evidenceID, Created: stdout.created, Purpose: sqlite.PurposeToolStdout},
		{EvidenceID: stderr.evidenceID, Created: stderr.created, Purpose: sqlite.PurposeToolStderr},
	}
	stream := func(c *captureSink) streamResult {
		p, trunc := c.preview()
		return streamResult{EvidenceID: c.evidenceID, TotalBytes: c.total(), CaptureComplete: res.CaptureComplete(), Preview: p, PreviewTruncated: trunc}
	}
	er := execResult{Signaled: res.Signaled, TimedOut: res.TimedOut, Stdout: stream(stdout), Stderr: stream(stderr), Profile: prof.Name}
	if res.ExitCode != nil {
		code := int64(*res.ExitCode)
		er.ExitCode, o.exit = &code, &code
	}
	o.result = er
	switch {
	case runErr != nil && !res.Started:
		o2 := failure(runErr)
		o2.captures, o2.captureComplete = nil, true
		return o2
	case runErr != nil:
		// The process started and what it did is not known: it could not be recorded, or its
		// tree could not be shown to have stopped. The error says which.
		o.effect, o.err = "unknown", toolerr.Uncertain(toolerr.CodeStartFailed, "the process started and its record could not be completed")
		if te, ok := toolerr.As(runErr); ok && te.Class == toolerr.Unknown {
			o.err = te
		}
	case res.Cancelled:
		o.effect, o.err = "cancelled", toolerr.Fail(toolerr.CodeCancelled, "the process was stopped because the run was stopped")
	case res.TimedOut:
		o.effect, o.err = "failed", toolerr.Fail(toolerr.CodeTimeout, "the process was stopped at its timeout")
	case res.CaptureLimited:
		o.effect, o.err = "failed", toolerr.Fail(toolerr.CodeCaptureLimit, "the process was stopped at the run's capture limit; its output is incomplete")
	case res.ExitCode == nil:
		o.effect, o.err = "failed", toolerr.Fail(toolerr.CodeIO, "the process ended by a signal")
	default:
		o.effect = "completed"
	}
	return o
}

// hookInput is what a hook at a point of this call is told: identifiers and revisions only.
// The control revision is the one the Run holds (its fence's, read when it began; a stop that
// was already pending then is held as -1 by the driver and is told as 0), not a fresh read:
// what changes it is the dispatch-start's own compare-and-set, not a hook.
func (r *RunTools) hookInput(p extensions.HookPoint, cs *CallState, evidenceIDs []string, code *string) extensions.HookInput {
	if evidenceIDs == nil {
		evidenceIDs = []string{}
	}
	if len(evidenceIDs) > 128 {
		evidenceIDs = evidenceIDs[:128]
	}
	return extensions.HookInput{
		Hook: p, ThreadID: r.fence.ThreadID, RunID: r.fence.RunID, ActionID: protocol.Str(cs.Bound.ActionID), Stage: nil,
		ContextRevision: max(r.contextRevision, 0), ControlRevision: max(r.fence.ControlRevision, 0), EvidenceIDs: evidenceIDs, Code: code,
	}
}

// recorder is how a hook leaves a private record of the Run: an Evidence under the Run's
// fence. An error that says the writer lost the Thread is the driver's to see (it is not
// the hook's failure); any other error is the hook's.
func (r *RunTools) recorder() extensions.Recorder {
	return func(ctx context.Context, purpose string, data []byte) (string, error) {
		id, err := r.rt.store.RecordEvidence(ctx, r.fence, purpose, "application/json", false, data)
		if errors.Is(err, sqlite.ErrWriterLost) || errors.Is(err, sqlite.ErrRunNotRunning) {
			return "", extensions.Fatal(err)
		}
		return id, err
	}
}

// beforeTool is the hook before a call is dispatched. It returns the end of the call when
// the hook stopped it (nothing was started), a store error the driver must see, or neither
// when the call may go on.
func (r *RunTools) beforeTool(ctx context.Context, cs *CallState) (*End, error) {
	if !r.rt.runner.Active() {
		return nil, nil
	}
	v := r.rt.runner.Call(ctx, r.hookInput(extensions.BeforeTool, cs, nil, nil), r.recorder())
	if v.Over {
		r.rt.diag("a hook went over its design budget")
	}
	switch {
	case v.Fatal != nil:
		return nil, v.Fatal
	case v.Deny:
		return &End{Effect: "not_started", Stop: &Stop{Code: StopHookDenied}}, nil
	case v.Ended:
		// The Run is being stopped: its stop, not the hook, is why the call was not started.
		switch {
		case errors.Is(context.Cause(ctx), sqlite.ErrControlChanged):
			return &End{Effect: "not_started", Stop: &Stop{Code: StopCancelled}}, nil
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return &End{Effect: "not_started", Stop: &Stop{Code: StopDeadline}}, nil
		}
		return &End{Effect: "not_started", Stop: &Stop{Code: StopDriver}}, nil
	case v.Failed:
		r.rt.diag("a hook before a call did not answer validly: %s", v.Reason)
		return &End{Effect: "not_started", Stop: &Stop{Code: StopHookFailed}}, nil
	}
	return nil, nil
}

// afterTool is the hook after a call ended and its end was recorded. It can only continue:
// it does not change the call, its answer or the Run. It runs even when the Run is being
// stopped (the call happened), under a context that is not cancelled with the Run and is
// bounded by the hard limit. A hook that fails leaves a diagnosis behind and nothing else.
func (r *RunTools) afterTool(ctx context.Context, cs *CallState, effect string, evidenceIDs []string) {
	if !r.rt.runner.Active() {
		return
	}
	// A store error that says the writer lost the Thread is not this call's to handle: the
	// driver finds it out at its next write, as it does for any other.
	_ = r.rt.runner.CallAfter(ctx, r.hookInput(extensions.AfterTool, cs, evidenceIDs, protocol.Str(effect)), r.recorder(), r.rt.diag)
}

// completedEvidence is the Evidence the action.completed event names.
func completedEvidence(events []protocol.Event) []string {
	for _, ev := range events {
		if ev.Type != protocol.EventActionCompleted {
			continue
		}
		var p protocol.ActionCompletedPayload
		if err := json.Unmarshal(ev.Payload, &p); err == nil {
			return p.ResultEvidenceIDs
		}
	}
	return nil
}
