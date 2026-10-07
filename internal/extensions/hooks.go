// Package extensions is F27 LoadTrustedExtensions and the fixed hook boundary of
// HOST_ASSETS: the AGENTS.md and SKILL.md files the host lets a Run see, and the
// callbacks the host runs at seven points of a Run.
//
// Nothing here gives anything authority. An AGENTS.md or a SKILL.md is data the model may
// read, scoped to where it was found; it is never the user's instruction and never the
// host's policy. A hook is a callback that is part of this product (the configuration can
// only name one that exists here), runs with a bounded time, sees identifiers and
// revisions only (never text, arguments or credentials), and cannot change the prompt,
// the arguments, the model, the policy or a checkpoint candidate.
package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// HookPoint is one of the seven places a hook is called (HOST_ASSETS section 4).
type HookPoint string

// The seven hook points.
const (
	BeforeModel   HookPoint = "before_model"
	AfterModel    HookPoint = "after_model"
	BeforeTool    HookPoint = "before_tool"
	AfterTool     HookPoint = "after_tool"
	BeforeCompact HookPoint = "before_compact"
	AfterCompact  HookPoint = "after_compact"
	RunTerminal   HookPoint = "run_terminal"
)

// HookPoints lists the seven points.
func HookPoints() []HookPoint {
	return []HookPoint{BeforeModel, AfterModel, BeforeTool, AfterTool, BeforeCompact, AfterCompact, RunTerminal}
}

// Valid reports whether p is one of the seven points.
func (p HookPoint) Valid() bool { return slices.Contains(HookPoints(), p) }

// CanDeny reports whether a hook at p may deny: only the points before something is done.
// A hook after a point never undoes what was done.
func (p HookPoint) CanDeny() bool { return p == BeforeModel || p == BeforeTool || p == BeforeCompact }

// The decisions of a hook.
const (
	DecisionContinue = "continue"
	DecisionDeny     = "deny"
)

// Codes of a Run that a hook ended (the Run is blocked: a host decision, not a failure of
// the work).
const (
	// CodeHookDenied: a hook at a point before something denied it.
	CodeHookDenied = "HOST_HOOK_DENIED"
	// CodeHookFailed: a hook at a point before something did not give a valid answer in time.
	CodeHookFailed = "HOST_HOOK_FAILED"
)

// Limits on a hook's input, from the schema.
const (
	maxHookEvidenceIDs = 128
	maxHookCodeBytes   = 128
)

// HookInput is everything a hook is told (host_assets.schema.json HookInput): identifiers
// and revisions, never a body, an argument or a credential.
type HookInput struct {
	Hook            HookPoint `json:"hook"`
	ThreadID        string    `json:"thread_id"`
	RunID           string    `json:"run_id"`
	ActionID        *string   `json:"action_id"`
	Stage           *string   `json:"stage"`
	ContextRevision int64     `json:"context_revision"`
	ControlRevision int64     `json:"control_revision"`
	EvidenceIDs     []string  `json:"evidence_ids"`
	Code            *string   `json:"code"`
}

// Validate checks the input against the schema's rules and the identifiers' grammar.
func (in HookInput) Validate() error {
	bad := func(what string) error { return fmt.Errorf("extensions: hook input: %s", what) }
	if !in.Hook.Valid() {
		return bad("unknown hook point")
	}
	if _, err := identity.ParseThreadID(in.ThreadID); err != nil {
		return bad("thread_id")
	}
	if _, err := identity.ParseRunID(in.RunID); err != nil {
		return bad("run_id")
	}
	if in.ActionID != nil {
		if _, err := identity.ParseActionID(*in.ActionID); err != nil {
			return bad("action_id")
		}
	}
	if in.Stage != nil && !slices.Contains([]string{"act", "instruction_selection", "work_summary"}, *in.Stage) {
		return bad("stage")
	}
	if in.ContextRevision < 0 || in.ControlRevision < 0 {
		return bad("a revision is negative")
	}
	if in.EvidenceIDs == nil || len(in.EvidenceIDs) > maxHookEvidenceIDs {
		return bad("evidence_ids")
	}
	for _, id := range in.EvidenceIDs {
		if _, err := identity.ParseEvidenceID(id); err != nil {
			return bad("evidence_ids")
		}
	}
	if in.Code != nil && (*in.Code == "" || len(*in.Code) > maxHookCodeBytes || !utf8.ValidString(*in.Code)) {
		return bad("code")
	}
	return nil
}

// HookResult is what a hook answers (host_assets.schema.json HookResult).
type HookResult struct {
	Decision string  `json:"decision"`
	Code     *string `json:"code"`
}

// Continue is the answer that lets the Run go on.
func Continue() HookResult { return HookResult{Decision: DecisionContinue} }

// Deny is the answer that stops what was about to be done, with a short code for the audit.
func Deny(code string) HookResult { return HookResult{Decision: DecisionDeny, Code: &code} }

// validate checks a result a callback gave: a decision of the two, a deny with a code, and
// a deny only where the point lets one.
func (r HookResult) validate(p HookPoint) error {
	switch r.Decision {
	case DecisionContinue:
	case DecisionDeny:
		if !p.CanDeny() {
			return errors.New("a hook after a point cannot deny")
		}
		if r.Code == nil || *r.Code == "" {
			return errors.New("a deny has a code")
		}
	default:
		return errors.New("the decision is not one of the two")
	}
	if r.Code != nil && (len(*r.Code) > maxHookCodeBytes || !utf8.ValidString(*r.Code)) {
		return errors.New("the code is not valid")
	}
	return nil
}

// Recorder writes a private record of the Run (a private Evidence). It is how a hook leaves
// anything behind; the host binds it to the Run the hook is called for.
type Recorder func(ctx context.Context, purpose string, data []byte) (evidenceID string, err error)

// Callback is one fixed hook of this product. It must return when ctx ends and must not wait
// for anything that has no bound: the Runner gives it a context with a deadline and does not
// pretend that a thread can be killed, so a callback that ignores the context holds its
// goroutine (and only that) until it returns.
type Callback func(ctx context.Context, in HookInput, rec Recorder) (HookResult, error)

// Named is a callback with the name the configuration refers to it by.
type Named struct {
	Name string
	Call Callback
}

// AuditMetadata is the name of the one hook the configuration may list.
const AuditMetadata = "audit_metadata"

// Timing of a hook (HOST_ASSETS: "usually a 10 ms design budget, no unbounded wait on real
// I/O"). The design budget is a figure to be measured against: a hook that goes over it is
// counted and said, not stopped, because a hook that records to the store (audit_metadata)
// can honestly take longer than that on a loaded disk. What is enforced is the hard limit,
// and it is far enough above the budget that only a hook that is stuck reaches it.
const (
	DesignBudget     = 10 * time.Millisecond
	DefaultHardLimit = 250 * time.Millisecond
)

// ErrFatal is wrapped by an error of the Recorder that is not a failure of the hook: the
// store no longer belongs to the driver (another writer took the Thread, the Run ended). It
// is not hidden as HOST_HOOK_FAILED: the driver finds out what happened as it does for any
// write.
var ErrFatal = errors.New("extensions: the store no longer belongs to this run")

// Fatal marks err as an error of the store that the driver must see (see ErrFatal).
func Fatal(err error) error { return fmt.Errorf("%w: %w", ErrFatal, err) }

// Verdict is what the Runner makes of one call.
type Verdict struct {
	// Deny is set when a callback denied (and the point lets it).
	Deny bool
	// Code is the callback's code of a deny, or "".
	Code string
	// Failed is set when a hook did not give a valid answer: it did not return within the
	// hard limit, it returned an error or a result that is not valid, or it panicked. Reason
	// says which, in a few words with no content.
	Failed bool
	Reason string
	// Ended is set when the parent context ended while the hook ran (the Run is being
	// stopped): the Run's stop, not the hook, is why it did not finish.
	Ended bool
	// Fatal is a store error of the Recorder that the caller must treat as one of its own
	// writes (see ErrFatal).
	Fatal error
	// Over says the call took longer than the design budget.
	Over    bool
	Elapsed time.Duration
}

// Options configure NewRunner.
type Options struct {
	// HardLimit is the longest a callback may take. Zero is DefaultHardLimit.
	HardLimit time.Duration
	// Extra are callbacks a test registers in addition to the configured ones. Production
	// leaves it nil: nothing outside this package can add a callback to a deployment.
	Extra []Named
	// Now is the clock of the timing (a test seam). Nil is time.Now.
	Now func() time.Time
}

// Runner runs the callbacks of one deployment. It is safe for concurrent use.
type Runner struct {
	callbacks []Named
	hard      time.Duration
	now       func() time.Time

	mu   sync.Mutex
	over int64
}

// registry is every callback this product has. The configuration selects among them by
// name; an unknown name is a configuration error, never a lookup of code by name.
func registry() map[string]Callback {
	return map[string]Callback{AuditMetadata: auditMetadata}
}

// NewRunner builds the runner of the hooks a configuration lists (config.extensions.hooks:
// empty, or audit_metadata). A name this product does not have, and a name listed twice,
// are errors.
func NewRunner(names []string, o Options) (*Runner, error) {
	r := &Runner{hard: o.HardLimit, now: o.Now}
	if r.hard <= 0 {
		r.hard = DefaultHardLimit
	}
	if r.now == nil {
		r.now = time.Now
	}
	reg := registry()
	seen := map[string]bool{}
	for _, n := range names {
		cb, ok := reg[n]
		switch {
		case !ok:
			return nil, fmt.Errorf("extensions: %q is not a hook this product has", n)
		case seen[n]:
			return nil, fmt.Errorf("extensions: the hook %q is listed twice", n)
		}
		seen[n] = true
		r.callbacks = append(r.callbacks, Named{Name: n, Call: cb})
	}
	r.callbacks = append(r.callbacks, o.Extra...)
	return r, nil
}

// Active reports whether there is a callback to call. A Runner that is not active is not
// called at all: an empty list still leaves the Kernel's own safety handling in place.
func (r *Runner) Active() bool { return r != nil && len(r.callbacks) > 0 }

// OverBudget is how many calls went over the design budget.
func (r *Runner) OverBudget() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.over
}

type callResult struct {
	res HookResult
	err error
}

// Call runs every callback for one point, in the order they are listed, and stops at the
// first that denies or fails. It never panics and never waits longer than the hard limit
// for any one callback. rec is how the callbacks leave a record; it may be nil.
func (r *Runner) Call(ctx context.Context, in HookInput, rec Recorder) (v Verdict) {
	if !r.Active() {
		return v
	}
	if err := in.Validate(); err != nil {
		return Verdict{Failed: true, Reason: "invalid_input"}
	}
	start := r.now()
	defer func() {
		v.Elapsed = r.now().Sub(start)
		if v.Over = v.Elapsed > DesignBudget; v.Over {
			r.mu.Lock()
			r.over++
			r.mu.Unlock()
		}
	}()
	for _, cb := range r.callbacks {
		if ctx.Err() != nil {
			return Verdict{Failed: true, Ended: true, Reason: "run_stopped"}
		}
		res, err, timedOut := r.invoke(ctx, cb, in, rec)
		switch {
		case timedOut && ctx.Err() != nil:
			return Verdict{Failed: true, Ended: true, Reason: "run_stopped"}
		case timedOut:
			return Verdict{Failed: true, Reason: "timeout"}
		case err != nil && errors.Is(err, ErrFatal):
			return Verdict{Failed: true, Reason: "store", Fatal: err}
		case err != nil:
			return Verdict{Failed: true, Reason: "error"}
		}
		if err := res.validate(in.Hook); err != nil {
			return Verdict{Failed: true, Reason: "invalid_result"}
		}
		if res.Decision == DecisionDeny {
			return Verdict{Deny: true, Code: *res.Code}
		}
	}
	return v
}

// CallAfter calls the hooks of a point that comes after something was done (after_tool,
// after_model, after_compact, run_terminal): the thing happened, and what a hook answers
// does not change it. It runs even when the Run is being stopped (the call was made), under a
// context that is not cancelled with the Run and is bounded by the hard limit. A hook that
// does not answer validly (or that denies, which these points do not allow) leaves a private
// diagnosis behind and nothing else: not the Run's state, not what was recorded.
//
// The returned error is not a hook's failure: it is a store error of the Recorder that says
// the writer no longer holds the Thread (see ErrFatal), which the caller treats as one of its
// own writes. diag, when set, is told (with no content) that a hook went over its design
// budget or did not answer validly.
func (r *Runner) CallAfter(ctx context.Context, in HookInput, rec Recorder, diag func(format string, args ...any)) error {
	if !r.Active() {
		return nil
	}
	if diag == nil {
		diag = func(string, ...any) {}
	}
	detached := context.WithoutCancel(ctx)
	v := r.Call(detached, in, rec)
	if v.Over {
		diag("a hook went over its design budget")
	}
	switch {
	case v.Fatal != nil:
		return v.Fatal
	case !v.Failed:
		return nil
	}
	diag("a hook at %s did not answer validly: %s", in.Hook, v.Reason)
	body, err := EncodeDiagnosis(in.Hook, in.ActionID, v.Reason)
	if err != nil || rec == nil {
		return nil
	}
	dctx, cancel := context.WithTimeout(detached, DefaultHardLimit)
	defer cancel()
	if _, err := rec(dctx, PurposeHookDiagnosis, body); err != nil && errors.Is(err, ErrFatal) {
		return err
	}
	return nil
}

// EncodeDiagnosis is the private record a hook after something leaves when it did not answer
// validly: which point, which Action (when the point has one), and why in a few words. It has
// no text of the Run, an argument or a path.
func EncodeDiagnosis(p HookPoint, actionID *string, reason string) ([]byte, error) {
	raw, err := json.Marshal(map[string]any{"format": "rencrow-hook-diagnosis/v1", "hook": string(p), "action_id": actionID, "reason": reason})
	if err != nil {
		return nil, err
	}
	return protocol.EncodeCanonicalContract(raw)
}

// invoke runs one callback on its own goroutine under the hard limit. It returns when the
// callback does, or when the limit (or the parent context) ends, whichever comes first; a
// callback that is still running then is left to finish by itself, with a context that has
// ended, and what it answers later is dropped.
func (r *Runner) invoke(parent context.Context, cb Named, in HookInput, rec Recorder) (res HookResult, err error, timedOut bool) {
	cctx, cancel := context.WithTimeout(parent, r.hard)
	defer cancel()
	done := make(chan callResult, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- callResult{err: errors.New("the callback panicked")}
			}
		}()
		out, e := cb.Call(cctx, in, rec)
		done <- callResult{res: out, err: e}
	}()
	select {
	case got := <-done:
		// An answer that came once the limit (or the Run's stop) had ended the callback's
		// context is not one: it is late, whatever it says.
		if cctx.Err() != nil {
			return HookResult{}, nil, true
		}
		return got.res, got.err, false
	case <-cctx.Done():
		return HookResult{}, nil, true
	}
}

// auditMetadata is the one callback this product has: it continues at every point and
// records the metadata it was given (identifiers and revisions, no body) as a private
// Evidence of the Run. It decides nothing.
func auditMetadata(ctx context.Context, in HookInput, rec Recorder) (HookResult, error) {
	if rec == nil {
		return HookResult{}, errors.New("there is no record to write to")
	}
	raw, err := protocol.EncodeCanonicalContract(mustJSON(map[string]any{
		"format": "rencrow-hook-audit/v1", "hook": string(in.Hook), "thread_id": in.ThreadID, "run_id": in.RunID, "action_id": in.ActionID, "stage": in.Stage,
		"context_revision": in.ContextRevision, "control_revision": in.ControlRevision, "evidence_ids": in.EvidenceIDs, "code": in.Code,
		"decision": DecisionContinue,
	}))
	if err != nil {
		return HookResult{}, err
	}
	if _, err := rec(ctx, PurposeHookAudit, raw); err != nil {
		return HookResult{}, err
	}
	return Continue(), nil
}

// Purposes of the private Evidence the hook boundary writes.
const (
	PurposeHookAudit     = "hook_audit"
	PurposeHookDiagnosis = "hook_diagnosis"
)
