package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/extensions"
	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/oslock"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// DriverOptions configure a Driver.
type DriverOptions struct {
	Store *sqlite.Store
	Model modelport.ModelPort
	// Tools is the Tool runtime. Without one the requests offer no Tool and a Tool call
	// is a contract failure, as in a build that has none.
	Tools *tools.Runtime
	// Hooks runs the host's fixed hooks at the model, compaction and Run-end points (the
	// Tool runtime's own points use the same Runner). Nil, or a Runner with nothing to call,
	// leaves every step as it was.
	Hooks *extensions.Runner
	// SafetyMarginTokens is the margin every count carries (compaction.safety_margin_tokens).
	SafetyMarginTokens int64
	// Initiator is the caller principal, recorded in the metadata of every request.
	Initiator string
	// Publish receives the events a commit made, after the commit, in order.
	Publish func([]protocol.Event)
	// Progress receives provisional text of a generation as it arrives.
	Progress func(protocol.ProgressDelta)
	// Reset is told, when a retry starts, that the failed Attempt's provisional text is
	// to be discarded (progress/reset): the new Attempt's text is never joined to it.
	Reset func(protocol.ProgressReset)
	// RetryWait waits out a retry's backoff and returns at once, with the context's
	// error, when ctx ends. Nil is a real timer; a test replaces it. It is a seam of the
	// clock only: the delay it is given is always the one the decision recorded.
	RetryWait func(ctx context.Context, d time.Duration) error
	// Diag receives short diagnostics: never request or model text, a key or a path.
	Diag func(format string, args ...any)
	// Compaction lets a Run compact a prompt that does not fit (compaction.enabled). Without
	// it such a prompt ends the Run blocked for capacity, as before.
	Compaction bool
	// TriggerRatio (compaction.trigger_ratio) is the fraction of the usable prompt budget at
	// which a compaction is started before the prompt is full: a verified count of a prompt
	// that fits and whose upper bound has reached it starts one. Zero (or anything outside
	// (0, 1)) disables the early trigger; the configuration validator never gives such a
	// ratio, so only a driver built by hand has none.
	TriggerRatio float64
	// ControlPoll is how often a Run's driver looks at the Thread's control revision to
	// learn of a stop recorded by someone else (another process). A stop recorded through
	// this driver's own Interrupt does not wait for it. Zero is DefaultControlPoll.
	ControlPoll time.Duration
}

// DefaultControlPoll is how often a driver looks for a stop recorded elsewhere.
const DefaultControlPoll = 250 * time.Millisecond

// Driver carries out what the machine asks for. It holds no state of its own about a
// Run beyond the handle that lets Interrupt reach the Runs it is driving: everything
// it learns it keeps for the duration of one Drive call.
type Driver struct {
	o DriverOptions

	mu      sync.Mutex
	running map[string]func() // the Runs being driven, by ID: stops the Run's work
}

// NewDriver builds a Driver. The Store and the Model are required.
func NewDriver(o DriverOptions) (*Driver, error) {
	if o.Store == nil || o.Model == nil {
		return nil, errors.New("kernel: a driver needs a store and a model port")
	}
	if o.Publish == nil {
		o.Publish = func([]protocol.Event) {}
	}
	if o.Progress == nil {
		o.Progress = func(protocol.ProgressDelta) {}
	}
	if o.Reset == nil {
		o.Reset = func(protocol.ProgressReset) {}
	}
	if o.RetryWait == nil {
		o.RetryWait = timerWait
	}
	if o.Diag == nil {
		o.Diag = func(string, ...any) {}
	}
	if o.ControlPoll <= 0 {
		o.ControlPoll = DefaultControlPoll
	}
	return &Driver{o: o, running: map[string]func(){}}, nil
}

// timerWait waits d on the real clock, or until ctx ends.
func timerWait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Interrupt tells the driver of the Run, when this driver is driving it, that a stop
// was recorded for it: whatever the Run is doing right now (a count, a generation, a
// Tool call, a process) is told to stop at once, instead of at the next time the
// Thread's control revision is looked at. The record itself is the store's; this is
// only the wake-up, so a missed or repeated call changes nothing about how the Run ends.
// It reports whether the Run was being driven here.
func (d *Driver) Interrupt(runID string) bool {
	d.mu.Lock()
	stop, ok := d.running[runID]
	d.mu.Unlock()
	if ok {
		stop()
	}
	return ok
}

func (d *Driver) track(runID string, stop func()) {
	d.mu.Lock()
	d.running[runID] = stop
	d.mu.Unlock()
}

func (d *Driver) untrack(runID string) {
	d.mu.Lock()
	delete(d.running, runID)
	d.mu.Unlock()
}

// watchControl stops the Run's work when the Thread's control revision is no longer the
// one the driver holds: a stop was recorded, here or by another process. It ends with
// the Run (ctx). A revision that cannot be read this time is read again next time.
func (d *Driver) watchControl(ctx context.Context, f sqlite.Fence, stop func()) {
	t := time.NewTicker(d.o.ControlPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if rev, err := d.o.Store.ControlRevision(d.dbCtx(ctx), f.ThreadID); err == nil && rev != f.ControlRevision {
				stop()
				return
			}
		}
	}
}

// Handle names the Run to drive and the writer epoch the driver holds for its Thread.
type Handle struct {
	RunID    string
	ThreadID string
	Epoch    int64
}

// runState is what one Drive call learns along the way.
type runState struct {
	rec   sqlite.RunRecord
	fence sqlite.Fence

	snap    sqlite.Snapshot
	desc    modelport.BindingDescriptor
	profile modelport.RecoveryProfile // the profile of the Attempt being measured or generated
	// base is the act request of the Attempt being measured or generated, without a
	// request ID or expected values: the first Attempt's, or, for the retry, the same one
	// with the recovery profile it was scheduled with.
	base        modelport.ChatRequest
	inputDigest string
	est         contextplan.BudgetEstimate
	reserved    sqlite.Reservation
	gen         modelport.Completion
	finalText   string

	// firstRequestDigest is the request digest the first Attempt of the Action was
	// counted with: what a retry of the same request must be counted as, and the base
	// request digest of the retry.
	firstRequestDigest string
	// retry is the retry that is scheduled and not yet sent; nil for a first Attempt.
	retry *retryState

	// measured is the count of the act request just counted, with the Evidence that records
	// it: what a checkpoint made now replaces.
	measured   modelport.MeasureResult
	measuredID string

	// checkpoint is the Thread's current checkpoint, loaded and held to its row; the
	// snapshot's applied history is what came after it.
	checkpoint    *compaction.Checkpoint
	summarySource *compaction.Checkpoint // the checkpoint that accepted its Summary, when another
	// The candidate a compaction made and is to commit, and what the compaction came to.
	candidate *compaction.ValidatedCandidate
	cOutcome  compaction.Outcome
	// lastCheckpointID is the last checkpoint this Run committed.
	lastCheckpointID *string
	// unresolvedModel are the Actions of the Run's generations that ended with their outcome
	// unknown.
	unresolvedModel []string

	// The Tool exchange of the response being acted on.
	pol                          *tools.RunPolicy
	run                          *tools.RunTools
	wsLock                       *oslock.Lock
	validated                    []tools.Call
	responseID                   string
	assistantText                string
	batch                        *tools.Batch
	verification                 *protocol.Verification
	verificationUnresolvedAction string
}

// retryState is what the driver keeps of a scheduled retry until it is sent.
type retryState struct {
	failedAttemptID string
	trigger         string
}

// release lets go of what the Run holds outside the store: the workspace lock.
func (rs *runState) release() {
	if rs.wsLock != nil {
		_ = rs.wsLock.Release()
		rs.wsLock = nil
	}
}

// stepResult is what one effect gave: the event for the machine, or the decision to
// stop writing because the driver no longer owns the Run.
type stepResult struct {
	ev      Event
	abandon bool
}

// Drive runs one admitted Run to its end. It returns when the Run is Terminal, or
// when it can no longer write for the Run (another driver took the Thread over, or the
// store failed in a way that cannot be recorded): a Run left that way is settled by the
// next driver that takes the Thread (RecoverStale).
//
// ctx is the process's: cancelling it stops the Run (DRIVER_STOPPED, or an unknown
// generation if one was in flight), and the Run is given until its deadline.
func (d *Driver) Drive(ctx context.Context, h Handle) {
	rec, err := d.o.Store.LoadRun(d.dbCtx(ctx), h.RunID)
	if err != nil {
		d.o.Diag("a run could not be loaded to be driven")
		return
	}
	if rec.Status != StatusRunning || rec.Phase != string(PhaseAdmitting) || rec.ThreadID != h.ThreadID {
		d.o.Diag("a run that is not waiting to start was handed to a driver")
		return
	}
	rs := &runState{rec: rec, fence: sqlite.Fence{ThreadID: h.ThreadID, RunID: h.RunID, Epoch: h.Epoch, ControlRevision: rec.ControlRevision}}
	defer rs.release()
	// A stop is the control revision moving: the work of the Run is told to stop with
	// ErrControlChanged as the cause, which is how a stopped Run is told from one that
	// ran out of time or whose process was stopped (see stopCode).
	base, stopRun := context.WithCancelCause(ctx)
	defer stopRun(nil)
	interrupt := func() { stopRun(sqlite.ErrControlChanged) }
	// The time left is measured on the store's clock and waited out on the real one:
	// a deadline is an instant of the clock the Run was admitted by, so it is not
	// handed to a timer of another clock as an absolute time.
	runCtx, cancel := context.WithTimeout(base, max(rec.DeadlineAt.Sub(d.o.Store.Now()), 0))
	defer cancel()
	d.track(h.RunID, interrupt)
	defer d.untrack(h.RunID)
	// A stop recorded before the driver started (the Run was admitted, then interrupted,
	// then driven) is a stop: the driver would otherwise read the already moved revision as
	// the one it was admitted at. Nothing of the Run is dispatched under it.
	if pending, err := d.o.Store.CancelRequested(d.dbCtx(ctx), h.RunID); err != nil {
		d.o.Diag("a run's stop request could not be read")
		return
	} else if pending {
		rs.fence.ControlRevision = -1
		interrupt()
	}
	go d.watchControl(runCtx, rs.fence, interrupt)

	state := NewState(rec.Limits, rec.DeadlineAt, rec.ModelSteps, rec.AttemptsUsed)
	state.Compaction = d.o.Compaction
	// The Run of a manual compaction (a system Run) compacts and ends: it generates no
	// answer, and it takes no input.
	state.Manual = rec.System
	ev := Event{Kind: EvStart, At: d.o.Store.Now()}
	for {
		next, effects, err := Transition(state, ev)
		if err != nil {
			d.o.Diag("the kernel refused a transition: %v", err)
			return
		}
		if next.Phase != state.Phase && next.Phase != PhaseTerminal {
			if err := d.o.Store.SetPhase(d.dbCtx(ctx), rs.fence, string(state.Phase), string(next.Phase)); err != nil {
				d.o.Diag("a phase could not be recorded: %v", describe(err))
				return
			}
		}
		state = next
		if state.Phase == PhaseTerminal {
			return
		}
		if len(effects) != 1 {
			d.o.Diag("the kernel asked for %d effects, not one", len(effects))
			return
		}
		res := d.execute(ctx, runCtx, rs, state, effects[0])
		if res.abandon {
			return
		}
		ev = res.ev
		ev.At = d.o.Store.Now()
	}
}

func describe(err error) string {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	switch {
	case errors.Is(err, sqlite.ErrWriterLost):
		return "writer epoch lost"
	case errors.Is(err, sqlite.ErrRunNotRunning):
		return "run already ended"
	case errors.Is(err, sqlite.ErrPhaseConflict):
		return "phase conflict"
	}
	return "store error"
}

// dbCtx is the context of a store write that must happen even when the Run is being
// stopped: a Run that is told to stop still records how it ended. The store's own busy
// timeout bounds the wait.
func (d *Driver) dbCtx(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

func failed(f Failure) stepResult { return stepResult{ev: Event{Kind: EvFailed, Failure: f}} }

// storeFailure turns a store error into the failure it is for the Run, or abandons the
// Run when the driver no longer owns it.
func (d *Driver) storeFailure(err error) stepResult {
	switch {
	case errors.Is(err, sqlite.ErrWriterLost), errors.Is(err, sqlite.ErrRunNotRunning), errors.Is(err, sqlite.ErrPhaseConflict), errors.Is(err, sqlite.ErrAttemptEnded):
		d.o.Diag("the run is no longer this driver's: %s", describe(err))
		return stepResult{abandon: true}
	case errors.Is(err, sqlite.ErrControlChanged):
		return failed(Failure{Code: modelport.CodeCancelled})
	case errors.Is(err, sqlite.ErrDeadlinePassed):
		return failed(Failure{Code: CodeDeadlineExceeded})
	case errors.Is(err, sqlite.ErrStepBudget):
		return failed(Failure{Code: CodeStepBudgetExhausted})
	case errors.Is(err, sqlite.ErrPolicyChanged):
		return failed(Failure{Code: CodePolicyChanged})
	case errors.Is(err, sqlite.ErrBindingChanged):
		return failed(Failure{Code: modelport.CodeBindingChanged})
	case errors.Is(err, sqlite.ErrProfileNotAllowed):
		return failed(Failure{Code: modelport.CodeUnsupportedRecovery})
	case errors.Is(err, sqlite.ErrToolIntentConflict), errors.Is(err, sqlite.ErrToolCallLimit), errors.Is(err, sqlite.ErrAttemptState):
		// The records of the Run's own Tool calls contradict each other: not a state to
		// work around.
		return failed(Failure{Code: CodeIntegrityBlocked})
	case errors.Is(err, sqlite.ErrGenerationBudget):
		return failed(Failure{Code: CodeGenerationBudgetExhausted})
	}
	switch protocol.CodeOf(err) {
	case protocol.CodeIntegrityBlocked:
		return failed(Failure{Code: CodeIntegrityBlocked})
	case protocol.CodePersistenceUncertain:
		return failed(Failure{Code: CodePersistenceUncertain})
	}
	d.o.Diag("a store operation failed: %s", describe(err))
	return failed(Failure{Code: CodeInternalError})
}

// modelFailure turns an error of the model port into a failure. stage says what was
// being asked: describing and counting involve no generation; a generation that fails
// without a word about what it did is unknown.
func (d *Driver) modelFailure(ctx context.Context, err error, stage string) stepResult {
	var se *modelport.StrictError
	switch {
	case errors.As(err, &se):
		state := ""
		if stage == "generate" {
			state = se.GenerationState()
		}
		return failed(Failure{Code: se.Code, GenerationState: state})
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || ctx.Err() != nil:
		return failed(Failure{Code: stopCode(ctx, err), GenerationState: generationState(stage)})
	}
	switch stage {
	case "describe":
		return failed(Failure{Code: modelport.CodeModelUnavailable})
	case "measure":
		return failed(Failure{Code: modelport.CodeBudgetUnverified})
	}
	return failed(Failure{Code: modelport.CodeOutcomeUnknown, GenerationState: modelport.StateUnknown})
}

// stopCode is the code of a Run whose work was stopped through its context: a stop that
// was recorded for the Run (the context's cause is the control revision having moved),
// the Run's deadline, or the driver's own process being stopped. A recorded stop is
// named first: it is the reason the work was told to stop, and a deadline that comes
// while the work is stopping does not rename it.
func stopCode(ctx context.Context, err error) string {
	switch {
	case errors.Is(context.Cause(ctx), sqlite.ErrControlChanged):
		return modelport.CodeCancelled
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		return CodeDeadlineExceeded
	}
	return CodeDriverStopped
}

// generationState is the state a stopped generation is in: unknown, since stopping the
// connection does not show that the generation stopped; a stage that does not
// generate has none.
func generationState(stage string) string {
	if stage == "generate" {
		return modelport.StateUnknown
	}
	return ""
}

func (d *Driver) execute(ctx, runCtx context.Context, rs *runState, s State, eff Effect) stepResult {
	switch eff.Kind {
	case EffLoad:
		return d.load(ctx, rs)
	case EffAssemble:
		return d.assemble(runCtx, rs)
	case EffMeasure:
		return d.measure(ctx, runCtx, rs)
	case EffGenerate:
		return d.generate(ctx, runCtx, rs)
	case EffValidateFinal:
		return d.validateFinal(rs)
	case EffPrepareBatch:
		return d.prepareBatch(ctx, runCtx, rs)
	case EffExecuteTool:
		return d.executeTool(ctx, runCtx, rs, s.ToolNext)
	case EffPersistObservation:
		return d.persistObservation(ctx, rs, eff.Reason)
	case EffWaitRetry:
		if eff.Retry == nil {
			d.o.Diag("a retry was asked for without a decision")
			return stepResult{abandon: true}
		}
		return d.waitRetry(ctx, runCtx, rs, *eff.Retry)
	case EffCompact:
		return d.compact(ctx, runCtx, rs, s.Early)
	case EffCommitCheckpoint:
		return d.commitCheckpoint(ctx, rs)
	case EffPersistResult:
		return d.persist(ctx, runCtx, rs, s.Pending)
	}
	d.o.Diag("the kernel asked for an effect this driver does not know")
	return stepResult{abandon: true}
}

// load is F02: the input is applied to the Thread's context and the snapshot read.
func (d *Driver) load(ctx context.Context, rs *runState) stepResult {
	if !rs.rec.System {
		events, err := d.o.Store.ApplyInput(d.dbCtx(ctx), rs.fence)
		if err != nil {
			return d.storeFailure(err)
		}
		d.o.Publish(events)
	}
	snap, err := d.o.Store.LoadSnapshot(d.dbCtx(ctx), rs.rec.RunID)
	if err != nil {
		return d.storeFailure(err)
	}
	if f := d.adoptSnapshot(rs, snap); f != nil {
		return failed(*f)
	}
	// A Thread that stands on a checkpoint is held, as a Run begins, to everything the
	// checkpoint names (CHECKPOINT_FORMAT section 1: the load ends with the check of its
	// sources and metadata; an older checkpoint is never used in the place of one that does
	// not hold, and none is taken for no checkpoint): the same check run/resume makes before
	// it admits a Run. It is made once, here, not at every step.
	if rs.checkpoint != nil {
		if err := verifyStanding(d.dbCtx(ctx), d.o.Store, rs.rec.ThreadID, rs.snap, rs.checkpoint, rs.summarySource); err != nil {
			if protocol.CodeOf(err) == protocol.CodeIntegrityBlocked {
				d.o.Diag("the checkpoint the thread stands on does not hold: %s", describe(err))
				return failed(Failure{Code: CodeIntegrityBlocked})
			}
			return d.storeFailure(err)
		}
	}
	if f := d.openTools(rs); f != nil {
		return failed(*f)
	}
	return stepResult{ev: Event{Kind: EvLoaded}}
}

// openTools resolves the Run's Tool authority from the deployment, once, and holds it
// to the revision the Thread froze; a Run that may change the workspace takes the
// workspace's exclusive lock and keeps it until it ends, so two Runs never change one
// directory at once. It returns the failure that ends the Run, or nil.
func (d *Driver) openTools(rs *runState) *Failure {
	if d.o.Tools == nil {
		return nil
	}
	pol, err := d.o.Tools.PolicyFor(rs.rec)
	if err != nil {
		d.o.Diag("a run's policy is not the one it froze")
		return &Failure{Code: CodePolicyChanged}
	}
	rs.pol, rs.run = pol, d.o.Tools.ForRun(rs.rec, rs.fence, pol)
	if !pol.CanMutate() || rs.rec.System {
		// A system Run counts the prompt the Tool catalog is part of, and never runs a Tool:
		// it has no use for the workspace, and it does not hold it against a Run that has.
		return nil
	}
	path, err := sqlite.WorkspaceLockPath(d.o.Store.Root(), pol.Workspace)
	if err != nil {
		return &Failure{Code: CodeInternalError}
	}
	lock, err := oslock.TryLock(path, oslock.Exclusive)
	switch {
	case errors.Is(err, oslock.ErrLocked):
		return &Failure{Code: CodeWorkspaceBusy}
	case err != nil:
		d.o.Diag("the workspace lock could not be taken")
		return &Failure{Code: CodeInternalError}
	}
	rs.wsLock = lock
	return nil
}

// describeBinding asks the model side to describe the binding and holds the answer to what
// a strict, counted generation needs: it is the binding that was asked about, it can count,
// it speaks the strict contract and it publishes the fingerprint every count is held to. err
// is the model port's own error; code is the contract failure ("" for none).
func (d *Driver) describeBinding(ctx context.Context, b protocol.Binding) (modelport.BindingDescriptor, string, error) {
	desc, err := d.o.Model.Describe(ctx, b)
	if err != nil {
		return desc, "", err
	}
	switch {
	case !sameBinding(desc.Binding, b):
		return desc, modelport.CodeBindingChanged, nil
	case !desc.Measure || desc.Normalization != "strict-v1" || desc.BindingFingerprint == "":
		return desc, modelport.CodeUnsupportedContract, nil
	}
	return desc, "", nil
}

func sameBinding(a modelport.DescribedBinding, b protocol.Binding) bool {
	eq := func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.Kind == b.Kind && a.Selector == b.Selector && a.ProfileRevision == b.ProfileRevision && eq(a.AgentID, b.AgentID) && eq(a.ExecutionRole, b.ExecutionRole)
}

// assemble describes the binding, builds the prompt (F04) and the act request, and
// takes its logical input digest.
func (d *Driver) assemble(ctx context.Context, rs *runState) stepResult {
	// F23, at the boundary of a step: what was appended for this Run while it worked
	// (next_step) is applied to the context now, so this step's prompt carries it. A Run
	// that is being stopped is not fed more input (ErrControlChanged).
	var steps []protocol.Event
	var applied int
	if !rs.rec.System {
		var err error
		if steps, applied, err = d.o.Store.ApplyNextSteps(d.dbCtx(ctx), rs.fence); err != nil {
			return d.storeFailure(err)
		}
	}
	d.o.Publish(steps)
	if applied > 0 {
		snap, err := d.o.Store.LoadSnapshot(d.dbCtx(ctx), rs.rec.RunID)
		if err != nil {
			return d.storeFailure(err)
		}
		if f := d.adoptSnapshot(rs, snap); f != nil {
			return failed(*f)
		}
	}
	desc, code, err := d.describeBinding(ctx, rs.rec.Binding)
	if err != nil {
		return d.modelFailure(ctx, err, "describe")
	}
	if code != "" {
		return failed(Failure{Code: code})
	}
	profile, ok := desc.RecoveryProfile(modelport.ProfileSameRequest, modelport.StageAct)
	if !ok || !slices.Contains(rs.rec.Recovery.AllowedProfiles, modelport.ProfileSameRequest) {
		return failed(Failure{Code: modelport.CodeUnsupportedRecovery})
	}
	system, err := contextplan.ActSystemPrompt()
	if err != nil {
		return failed(Failure{Code: CodeInternalError})
	}
	history := make([]contextplan.HistoryItem, 0, len(rs.snap.Applied))
	for _, e := range rs.snap.Applied {
		history = append(history, contextplan.HistoryItem{ContextSeq: e.ContextSeq, MessageID: e.MessageID, HistoryKind: e.HistoryKind, Origin: e.Origin, Text: e.Text,
			ItemKind: e.ItemKind, ToolCallID: e.ToolCallID})
	}
	in := contextplan.Input{SystemPrompt: system, History: history}
	if rs.checkpoint != nil {
		// The Thread stands on a checkpoint: its projection, then what was applied after it,
		// rendered by the same function as a live history. The blocks are the Run's own: a
		// checkpoint keeps the ones it was made with, as where they came from, and what the
		// Run was started with is what its prompts carry.
		proj := rs.checkpoint.Candidate.Projection
		proj.ContextBlocks = rs.snap.Blocks
		in.Projection = &proj
	} else {
		in.Blocks = rs.snap.Blocks
	}
	plan, err := contextplan.AssembleContext(in)
	if err != nil {
		// What is stored cannot be put in a prompt: not a guess to work around.
		return failed(Failure{Code: CodeIntegrityBlocked})
	}
	rs.desc = desc
	req, err := d.actRequest(rs, plan.Messages, modelport.RecoveryRequest{ProfileID: modelport.ProfileSameRequest, ProfileRevision: profile.ProfileRevision})
	if err != nil {
		if errors.Is(err, errOffered) {
			return failed(Failure{Code: CodeInternalError})
		}
		return failed(Failure{Code: modelport.CodeUnsupportedContract})
	}
	digest, err := req.LogicalInputDigest()
	if err != nil {
		return failed(Failure{Code: CodeInternalError})
	}
	rs.desc, rs.profile, rs.base, rs.inputDigest = desc, profile, req, digest
	rs.retry, rs.firstRequestDigest = nil, "" // a new act Action: its first Attempt
	return stepResult{ev: Event{Kind: EvAssembled}}
}

var errOffered = errors.New("kernel: the Tool catalog could not be made")

// actRequest is the act request of the given prompt messages: the Tools the Run's policy
// offers, the options the binding's descriptor fixes for the act stage, and the recovery the
// Attempt is made with.
func (d *Driver) actRequest(rs *runState, msgs []modelport.ChatMessage, recovery modelport.RecoveryRequest) (modelport.ChatRequest, error) {
	var offered []modelport.FunctionTool
	if rs.pol != nil {
		var err error
		if offered, err = rs.pol.Catalog(); err != nil {
			return modelport.ChatRequest{}, errOffered
		}
	}
	return modelport.NewActRequest(modelport.ActParams{
		Binding: rs.rec.Binding, Descriptor: rs.desc, Messages: msgs, Tools: offered,
		Meta:     modelport.RequestMeta{TraceID: rs.rec.TraceID, TaskID: rs.rec.TaskID, SessionID: rs.rec.SessionID, Initiator: d.o.Initiator, Caller: "rencrow-harness"},
		Recovery: recovery,
	})
}

func withRequestID(r modelport.ChatRequest, id string) modelport.ChatRequest {
	r.Rencrow.RequestID = &id
	return r
}

// measure is F24 and F05: the final request is counted without generating, the answer
// is held to the request it was asked about, and the capacity verdict follows.
func (d *Driver) measure(ctx, runCtx context.Context, rs *runState) stepResult {
	if err := rs.base.Validate(false); err != nil {
		return failed(Failure{Code: modelport.CodeUnsupportedContract})
	}
	req := modelport.MeasureRequest{
		ContractVersion: modelport.ContractVersion, Request: withRequestID(rs.base, identity.NewRequestID().String()), SafetyMarginTokens: d.o.SafetyMarginTokens,
	}
	res, err := d.o.Model.Measure(runCtx, req)
	if err != nil {
		return d.modelFailure(runCtx, err, "measure")
	}
	if err := modelport.ValidateMeasureResult(res); err != nil {
		return failed(Failure{Code: modelport.CodeContractFailed})
	}
	est, err := contextplan.EstimateBudget(contextplan.BudgetInput{
		Measure: res, InputDigest: rs.inputDigest, BindingFingerprint: rs.desc.BindingFingerprint,
		ReservedOutputTokens: rs.base.MaxTokens, SafetyMarginTokens: d.o.SafetyMarginTokens,
	})
	var mm *contextplan.MismatchError
	if errors.As(err, &mm) {
		return failed(Failure{Code: mm.Code})
	}
	if err != nil {
		return failed(Failure{Code: CodeInternalError})
	}
	switch {
	case rs.retry == nil:
		rs.firstRequestDigest = est.Report.RequestDigest
	case rs.profile.ProfileID == modelport.ProfileSameRequest && est.Report.RequestDigest != rs.firstRequestDigest:
		// The same request counts as the same request (RETRY_CONTRACT section 4): another
		// digest means what would be sent is not what failed, and nothing is sent on it.
		return failed(Failure{Code: modelport.CodeRequestDigestMismatch})
	}
	raw, err := json.Marshal(res)
	if err == nil {
		raw, err = protocol.EncodeCanonicalContract(raw)
	}
	if err != nil {
		return failed(Failure{Code: CodeInternalError})
	}
	evidenceID, err := d.o.Store.RecordEvidence(d.dbCtx(ctx), rs.fence, "measure_result", "application/json", false, raw)
	if err != nil {
		return d.storeFailure(err)
	}
	rs.est, rs.measured, rs.measuredID = est, res, evidenceID
	return stepResult{ev: Event{Kind: EvMeasured, Fit: est.Verdict, Early: contextplan.ReachesTrigger(est, d.o.TriggerRatio)}}
}

// captureReader keeps what it passes on, up to a bound, so the response can be sealed
// as Evidence exactly as it arrived.
type captureReader struct {
	r   io.Reader
	buf bytes.Buffer
	max int
}

func (c *captureReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		if room := c.max - c.buf.Len(); room > 0 {
			c.buf.Write(p[:min(n, room)])
		}
	}
	return n, err
}

// generate is F15: the attempt is reserved (budget consumed, fence checked) in one
// transaction right before it is sent, one request is sent, the strict response is
// read to its end, and how the attempt ended is recorded, whatever it was. Nothing in
// the response is acted on: the machine decides what the completion means. The first
// Attempt of an act Action and its one retry are reserved by different transactions
// (ReserveGeneration, ReserveRetry); the rest is the same for both.
func (d *Driver) generate(ctx, runCtx context.Context, rs *runState) stepResult {
	gen := withRequestID(rs.base, identity.NewRequestID().String()).
		WithExpected(rs.inputDigest, rs.est.Report.RequestDigest, rs.desc.BindingFingerprint)
	if err := gen.Validate(true); err != nil {
		return failed(Failure{Code: modelport.CodeUnsupportedContract})
	}
	reqBytes, err := gen.Canonical()
	if err != nil {
		return failed(Failure{Code: CodeInternalError})
	}
	retry := rs.retry
	// before_model: before the reservation, which records the Action, its dispatch and the
	// consumed attempt in one transaction, so a hook that denies leaves nothing reserved and
	// nothing sent (not an attempt whose outcome would be unknown). A retry's Attempt belongs to
	// the Action of the one that failed; the first Attempt has no Action until it is reserved.
	var hookAction *string
	if retry != nil {
		hookAction = protocol.Str(rs.reserved.ActionID)
	}
	if end := d.beforeHook(runCtx, rs, d.modelHookInput(rs, extensions.BeforeModel, modelport.StageAct, hookAction, nil, nil)); end != nil {
		return end.stepOf()
	}
	var rsv sqlite.Reservation
	if retry == nil {
		args, err := protocol.LogicalInputCJ1(reqBytes)
		if err != nil {
			return failed(Failure{Code: CodeInternalError})
		}
		rsv, err = d.o.Store.ReserveGeneration(d.dbCtx(ctx), rs.fence, sqlite.ReserveInput{
			Stage: modelport.StageAct, RequestID: *gen.Rencrow.RequestID, ArgsBytes: args, RequestBytes: reqBytes,
			InputDigest: rs.inputDigest, RequestDigest: rs.est.Report.RequestDigest, BindingFingerprint: rs.desc.BindingFingerprint,
			ProfileID: rs.profile.ProfileID, ProfileRevision: rs.profile.ProfileRevision, Stream: gen.Stream,
		})
		if err != nil {
			return d.storeFailure(err)
		}
	} else {
		rsv, err = d.o.Store.ReserveRetry(d.dbCtx(ctx), rs.fence, sqlite.RetryReserve{
			FailedAttemptID: retry.failedAttemptID, RequestID: *gen.Rencrow.RequestID, RequestBytes: reqBytes,
			InputDigest: rs.inputDigest, RequestDigest: rs.est.Report.RequestDigest, BindingFingerprint: rs.desc.BindingFingerprint,
			ProfileID: rs.profile.ProfileID, ProfileRevision: rs.profile.ProfileRevision, Stream: gen.Stream,
			BindingRevision: rs.rec.BindingRevision, PolicyRevision: rs.rec.PolicyRevision,
		})
		if err != nil {
			return d.storeFailure(err)
		}
	}
	rs.reserved = rsv
	rs.retry = nil
	d.o.Publish(rsv.Events)
	if retry != nil {
		// The retry is committed and is about to be sent: whatever the failed Attempt had
		// shown as provisional text is discarded now, before the new Attempt's first text,
		// and is never joined to it.
		d.o.Reset(protocol.ProgressReset{RunID: rs.rec.RunID, OldAttemptID: retry.failedAttemptID, NewAttemptID: rsv.AttemptID, Reason: retry.trigger, Provisional: true})
	}

	exp := modelport.Expected{
		Stage: modelport.StageAct, InputDigest: rs.inputDigest, RequestDigest: rs.est.Report.RequestDigest,
		BindingFingerprint: rs.desc.BindingFingerprint, ProfileID: rs.profile.ProfileID, ProfileRevision: rs.profile.ProfileRevision,
	}
	end := sqlite.AttemptEnd{AttemptID: rsv.AttemptID}
	var completion modelport.Completion

	stream, err := d.o.Model.Generate(runCtx, gen)
	switch {
	case err != nil:
		completion = d.refusedCompletion(runCtx, rs, err, &end)
	default:
		completion = d.readStream(runCtx, rs, stream, exp, &end)
	}
	toolsOffered := rs.pol != nil && len(rs.pol.Names()) > 0
	var calls []tools.Call
	if completion.TerminalKind == modelport.KindToolCalls && toolsOffered {
		// The whole response is checked before any call of it is acted on: a call to a Tool
		// the request did not offer, or arguments that do not satisfy the schema, make the
		// response output the model gave that cannot be used, and none of its calls runs.
		var verr error
		if calls, verr = rs.pol.ValidateCalls(completion.ToolIntents); verr != nil {
			completion.TerminalKind, completion.FailureCode, completion.ToolIntents = modelport.KindError, modelport.CodeOutputSchemaInvalid, nil
			completion.Violation = "the Tool calls do not satisfy the request's Tools"
			calls = nil
		}
	}
	judged := Generated{Kind: completion.TerminalKind, FailureCode: completion.FailureCode, GenerationState: completion.GenerationState, HiddenRetry: completion.HiddenRetry,
		ToolsOffered: toolsOffered, ToolCalls: len(calls), Offers: d.recoveryOffers(rs, completion.FailureCode)}
	j := Judge(judged)

	if f := d.finishAttempt(ctx, rs, modelport.StageAct, rs.profile, &end, completion, j, rsv.ActionID); f != nil {
		return *f
	}
	rs.gen = completion
	if j.Usable && !j.Tools {
		rs.finalText = completion.FinalText
	}
	if j.Tools && end.ResponseID != nil {
		rs.validated, rs.responseID, rs.assistantText = calls, *end.ResponseID, completion.ContentText
	}
	return stepResult{ev: Event{Kind: EvGenerated, Gen: judged}}
}

// finishAttempt records how one generation attempt ended, whatever the stage: its outcome and
// state as the machine judged the completion, what the receipt said was applied (kept as a
// fact of the Attempt only when the profile allows it; a statement of anything else stays in
// the response Evidence as it was stated, and the Attempt ended as a contract failure), the
// usage when it is known, and, when the end is unknown, the Action as unresolved. It returns
// the step result that ends the driver's work, or nil.
func (d *Driver) finishAttempt(ctx context.Context, rs *runState, stage string, profile modelport.RecoveryProfile, end *sqlite.AttemptEnd, completion modelport.Completion, j Judgement, actionID string) *stepResult {
	end.Outcome, end.GenerationState, end.BackendAttempts = j.Outcome, completion.GenerationState, completion.BackendAttempts
	end.Diagnosis = completion.Violation
	if j.FailureCode != "" {
		end.FailureCode = protocol.Str(j.FailureCode)
	}
	switch {
	case j.Usable:
		end.State = "completed"
	case completion.GenerationState == modelport.StateUnknown:
		end.State = "unknown"
	default:
		end.State = "failed"
	}
	if completion.Receipt != nil {
		end.UsageComplete = completion.Receipt.UsageComplete
		if modelport.VerifyTransformations(completion.Receipt.AppliedTransformations, profile.Transformations) == nil {
			end.AppliedTransformations = completion.Receipt.AppliedTransformations
		}
	}
	if completion.Usage != nil {
		if raw, err := json.Marshal(completion.Usage); err == nil {
			end.UsageJSON, _ = protocol.EncodeCanonicalContract(raw)
		}
	}
	rec, err := d.o.Store.RecordAttemptEnd(d.dbCtx(ctx), rs.fence, *end)
	if err != nil {
		r := d.storeFailure(err)
		return &r
	}
	d.o.Publish(rec.Events)
	if end.State == "unknown" {
		rs.unresolvedModel = append(rs.unresolvedModel, actionID)
	}
	// after_model: the attempt's end is recorded (an unknown one too: the generation was
	// asked for), and what a hook answers changes none of it. The code is the Attempt's state
	// as recorded (completed, failed or unknown), as after_tool's is the call's effect state.
	if d.afterHook(ctx, rs, d.modelHookInput(rs, extensions.AfterModel, stage, protocol.Str(actionID), []string{rec.ReceiptEvidenceID}, protocol.Str(end.State))) {
		return &stepResult{abandon: true}
	}
	return nil
}

// recoveryOffers works out, for a failure with this code, which of the two public
// recovery profiles may be used to retry it: what the Run's frozen policy allows, what
// the binding's descriptor publishes for the act stage, and whether the profile's codes
// include the failure's (RETRY_CONTRACT section 4). It decides nothing: the machine
// chooses from these answers.
func (d *Driver) recoveryOffers(rs *runState, code string) RecoveryOffers {
	offer := func(id string) Offer {
		if !slices.Contains(rs.rec.Recovery.AllowedProfiles, id) {
			return OfferNotAllowed
		}
		p, ok := rs.desc.RecoveryProfile(id, modelport.StageAct)
		switch {
		case !ok:
			return OfferNotSupported
		case !slices.Contains(p.Codes, code):
			return OfferCodeNotCovered
		}
		return OfferAllowed
	}
	return RecoveryOffers{SameRequest: offer(modelport.ProfileSameRequest), TerminalOutputOnce: offer(modelport.ProfileTerminalOutputOnce)}
}

// refusedCompletion is the completion of a Generate call that returned an error: a
// refusal before generating (with whatever receipt it carried), or a failure that
// says nothing about what the generation did, which is unknown. What the model side
// answered with is kept, up to its bound, as the response of the Attempt (end).
func (d *Driver) refusedCompletion(ctx context.Context, rs *runState, err error, end *sqlite.AttemptEnd) modelport.Completion {
	c := modelport.Completion{TerminalKind: modelport.KindError, GenerationState: modelport.StateUnknown}
	var se *modelport.StrictError
	switch {
	case errors.As(err, &se):
		c.FailureCode = se.Code
		c.GenerationState = se.GenerationState()
		c.Receipt = se.Receipt
		if se.Receipt != nil {
			c.BackendAttempts = se.Receipt.BackendAttempts
			c.HiddenRetry = se.Receipt.HiddenRetry
			if modelport.VerifyTransformations(se.Receipt.AppliedTransformations, rs.profile.Transformations) != nil {
				// A change nobody authorized: nothing the refusal says is taken at its word.
				c.FailureCode, c.HiddenRetry = modelport.CodeContractFailed, false
				c.Violation = "the receipt applied a transformation the recovery profile does not allow"
			}
		}
		end.SourceCode = se.SourceCode
		if len(se.Body) > 0 {
			// What came back is kept as it came, and labelled as what it is: a body that is not
			// JSON (a proxy's page) is not a JSON document.
			media := "application/json"
			if !json.Valid(se.Body) {
				media = "text/plain; charset=utf-8"
			}
			id := identity.NewResponseID().String()
			end.ResponseID, end.RawResponse, end.RawResponseMediaType, end.RawResponsePartial = &id, se.Body, media, se.BodyTruncated
		}
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || ctx.Err() != nil:
		c.FailureCode = stopCode(ctx, err)
	default:
		c.FailureCode = modelport.CodeOutcomeUnknown
	}
	return c
}

// readStream reads the response to its end and holds its receipt to the request,
// whatever way the generation ended.
func (d *Driver) readStream(ctx context.Context, rs *runState, stream io.ReadCloser, exp modelport.Expected, end *sqlite.AttemptEnd) modelport.Completion {
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() }) // a stopped Run stops reading
	defer stop()
	defer stream.Close()
	capture := &captureReader{r: stream, max: modelport.MaxStreamBytes + 64<<10}
	c := modelport.AssembleStrictStream(capture, modelport.AssembleOptions{
		MaxToolCalls: int(rs.rec.Limits.MaxToolCallsPerStep),
		OnContent: func(ordinal int64, text string) {
			d.o.Progress(protocol.ProgressDelta{RunID: rs.rec.RunID, AttemptID: rs.reserved.AttemptID, Ordinal: ordinal, Text: text, Provisional: true})
		},
	})
	end.RawResponse = capture.buf.Bytes()
	if rb, ok := stream.(modelport.RawBodier); ok {
		// The stream was made of a non-stream answer: that answer, as the model side sent
		// it, is what is kept as the response, not the stream made of it.
		end.RawResponse, end.RawResponseMediaType = rb.RawBody(), "application/json"
	}
	id := identity.NewResponseID().String()
	end.ResponseID = &id
	// A stream that ended because the Run was stopped says nothing more than that.
	if c.TerminalKind == modelport.KindError && !c.TerminalObserved && ctx.Err() != nil {
		c.FailureCode = stopCode(ctx, nil)
	}
	return modelport.CheckCompletionReceipt(c, exp, rs.profile.Transformations)
}

// waitRetry carries out a scheduled retry up to its being due: it records the retry (the
// decision as Evidence and model.retry_scheduled, in one transaction that a stop recorded
// before it makes void), builds the request of the retry (the same request with the
// recovery profile the decision names), and waits out the backoff, which ends at once
// when the Run is stopped. A Run that is stopped here has dispatched nothing more: the
// failed Attempt is already ended and known, so nothing is left unresolved.
func (d *Driver) waitRetry(ctx, runCtx context.Context, rs *runState, plan RetryDecision) stepResult {
	prof, ok := rs.desc.RecoveryProfile(plan.Profile, modelport.StageAct)
	if !ok || rs.reserved.AttemptID == "" {
		d.o.Diag("a retry was decided on a profile the binding does not offer")
		return failed(Failure{Code: CodeInternalError})
	}
	sched, err := d.o.Store.ScheduleRetry(d.dbCtx(ctx), rs.fence, sqlite.RetrySchedule{
		FailedAttemptID: rs.reserved.AttemptID, TriggerCode: plan.TriggerCode, DelayMS: plan.DelayMS, Profile: plan.Profile, ProfileRevision: prof.ProfileRevision,
		FallbackFrom: plan.FallbackFrom, FallbackReason: string(plan.FallbackReason), AttemptsUsed: rs.reserved.AttemptsUsed, MaxAttempts: rs.rec.Limits.MaxGenerationAttempts,
	})
	if err != nil {
		return d.storeFailure(err)
	}
	d.o.Publish(sched.Events)

	failedRequestID := rs.reserved.RequestID
	trigger := plan.TriggerCode
	req := rs.base
	req.Rencrow.Harness.Recovery = modelport.RecoveryRequest{
		ProfileID: prof.ProfileID, ProfileRevision: prof.ProfileRevision, RetryOfRequestID: &failedRequestID, TriggerCode: &trigger,
	}
	digest, err := req.LogicalInputDigest()
	if err != nil {
		return failed(Failure{Code: CodeInternalError})
	}
	rs.base, rs.profile, rs.inputDigest = req, prof, digest
	rs.retry = &retryState{failedAttemptID: rs.reserved.AttemptID, trigger: trigger}

	if err := d.o.RetryWait(runCtx, time.Duration(plan.DelayMS)*time.Millisecond); err != nil {
		return failed(Failure{Code: stopCode(runCtx, err)})
	}
	return stepResult{ev: Event{Kind: EvRetryDue}}
}

// prepareBatch is the first half of F16: every Tool call of the accepted response is
// bound to an Action, once, with the policy's verdict on each.
func (d *Driver) prepareBatch(ctx, runCtx context.Context, rs *runState) stepResult {
	if rs.run == nil || len(rs.validated) == 0 || rs.responseID == "" {
		d.o.Diag("Tool calls were accepted without a Tool runtime to bind them")
		return failed(Failure{Code: CodeInternalError})
	}
	batch, events, err := rs.run.Bind(runCtx, rs.responseID, rs.assistantText, rs.validated)
	d.o.Publish(events)
	if errors.Is(err, tools.ErrKilled) {
		return stepResult{abandon: true}
	}
	if err != nil {
		return d.storeFailure(err)
	}
	rs.batch = batch
	return stepResult{ev: Event{Kind: EvPrepared}}
}

// stopFailure is the failure that ends a Run for a Stop the Tool runtime names.
func stopFailure(code string) Failure {
	switch code {
	case tools.StopCancelled:
		return Failure{Code: modelport.CodeCancelled}
	case tools.StopDeadline:
		return Failure{Code: CodeDeadlineExceeded}
	case tools.StopDriver:
		return Failure{Code: CodeDriverStopped}
	case tools.StopEffectUnknown:
		return Failure{Code: CodeEffectOutcomeUnknown}
	case tools.StopPolicyAmbiguous:
		return Failure{Code: CodePolicyAmbiguous}
	case tools.StopPolicyChanged:
		return Failure{Code: CodePolicyChanged}
	case tools.StopHookDenied:
		return Failure{Code: CodeHostHookDenied}
	case tools.StopHookFailed:
		return Failure{Code: CodeHostHookFailed}
	}
	return Failure{Code: CodeInternalError}
}

// executeTool runs call i of the batch (the rest of F16).
func (d *Driver) executeTool(ctx, runCtx context.Context, rs *runState, i int) stepResult {
	if rs.run == nil || rs.batch == nil || i >= len(rs.batch.Calls) {
		d.o.Diag("a Tool call was asked for that is not bound")
		return failed(Failure{Code: CodeInternalError})
	}
	rs.run.SetContextRevision(rs.snap.ContextRevision)
	end, events, err := rs.run.Execute(runCtx, rs.batch, i)
	d.o.Publish(events)
	if errors.Is(err, tools.ErrKilled) {
		return stepResult{abandon: true}
	}
	if err != nil {
		return d.storeFailure(err)
	}
	te := ToolEnd{Effect: end.Effect}
	if end.Stop != nil {
		f := stopFailure(end.Stop.Code)
		te.Failure = &f
	}
	return stepResult{ev: Event{Kind: EvToolSettled, Tool: te}}
}

// persistObservation applies the Tool exchange to the context (calls that were not run
// are answered as such) and reads the context again, so the next prompt carries it.
func (d *Driver) persistObservation(ctx context.Context, rs *runState, reason string) stepResult {
	if reason == "" {
		reason = "run_ended"
	}
	events, err := rs.run.Finish(d.dbCtx(ctx), rs.batch, reason)
	d.o.Publish(events)
	if err != nil {
		return d.storeFailure(err)
	}
	snap, err := d.o.Store.LoadSnapshot(d.dbCtx(ctx), rs.rec.RunID)
	if err != nil {
		return d.storeFailure(err)
	}
	if f := d.adoptSnapshot(rs, snap); f != nil {
		return failed(*f)
	}
	rs.batch, rs.validated, rs.responseID, rs.assistantText = nil, nil, "", ""
	return stepResult{ev: Event{Kind: EvObserved}}
}

// terminalReason is why the calls a Run never reached were not run, from how it ended.
func terminalReason(o Outcome) string {
	switch o.Code {
	case modelport.CodeCancelled, modelport.CodePermitRevoked:
		return "cancelled"
	case CodeDeadlineExceeded:
		return "deadline"
	case CodePolicyChanged:
		return "policy_changed"
	case CodeDriverStopped:
		return "driver_stopped"
	case CodeHostHookDenied:
		return "hook_denied"
	case CodeHostHookFailed:
		return "hook_failed"
	}
	return "run_ended"
}

// validateFinal is the last check before a final answer is adopted: it is non-empty
// text from a generation that is shown to have ended once.
func (d *Driver) validateFinal(rs *runState) stepResult {
	c := rs.gen
	if rs.finalText == "" || c.TerminalKind != modelport.KindFinal || c.GenerationState != modelport.StateTerminal || c.HiddenRetry ||
		c.BackendAttempts == nil || *c.BackendAttempts != 1 || len(c.ToolIntents) != 0 {
		return failed(Failure{Code: modelport.CodeContractFailed, GenerationState: c.GenerationState})
	}
	return stepResult{ev: Event{Kind: EvFinalValid}}
}

// persist ends the Run: the run_terminal hook is called, the result is built from the outcome
// and the records of the Run (F20), and stored with run.terminal in one transaction.
func (d *Driver) persist(ctx, runCtx context.Context, rs *runState, pending *Outcome) stepResult {
	if pending == nil {
		d.o.Diag("a result was asked for without an outcome")
		return stepResult{abandon: true}
	}
	dbctx := d.dbCtx(ctx)
	var unresolvedTools []string
	if rs.batch != nil {
		// A Run that ends in the middle of a Tool exchange still tells its Thread what was
		// done: the calls that ran keep their answers, the others are answered "not run",
		// and the exchange is applied before the Run's result is stored.
		events, err := rs.run.Finish(dbctx, rs.batch, terminalReason(*pending))
		d.o.Publish(events)
		if err != nil {
			if errors.Is(err, sqlite.ErrWriterLost) || errors.Is(err, sqlite.ErrRunNotRunning) {
				d.o.Diag("the run is no longer this driver's: %s", describe(err))
				return stepResult{abandon: true}
			}
			d.o.Diag("a Tool exchange could not be applied before the run ended: %s", describe(err))
		}
		unresolvedTools = slices.Clone(rs.batch.Unresolved)
	}
	if !rs.rec.System && rs.pol != nil {
		if _, revision, configured := rs.pol.VerificationPlan(); configured && revision != "" {
			rs.verification = &protocol.Verification{Status: "not_run", EvidenceIDs: []string{}, CriteriaRevision: protocol.Str(revision)}
		}
	}
	if pending.Status == StatusCompleted && !rs.rec.System && rs.run != nil {
		execution, err := rs.run.Verify(runCtx)
		if err != nil {
			d.o.Diag("the host verification outcome could not be recorded")
			return stepResult{abandon: true}
		}
		d.o.Publish(execution.Events)
		rs.verification = &execution.Result
		rs.verificationUnresolvedAction = execution.UnresolvedActionID
		if execution.UnresolvedActionID != "" {
			// A process whose outcome is unknown cannot leave the Run completed: the
			// accepted model text stays in its response Evidence, while the terminal
			// result blocks and names the unresolved verification Action.
			*pending = ClassifyFailure(Failure{Code: CodeEffectOutcomeUnknown})
		}
	}
	if pending.Status == StatusCompleted && !rs.rec.System {
		if stopErr := runCtx.Err(); stopErr != nil {
			// The verifier is work inside this user Run. If its deadline or driver context
			// stopped the work, a known failed process result does not make the Run
			// completed; only a genuinely unresolved process outcome takes precedence.
			*pending = ClassifyFailure(Failure{Code: stopCode(runCtx, stopErr)})
		}
	}
	// run_terminal: the Run's end is decided and is about to be stored. The hook is called
	// before the terminal transaction because that is the last moment the Run can still write
	// a private record (a Run that has ended is no longer running, and a record of it is
	// refused), and the record is then part of the Run's evidence the result lists. The code
	// it is told is the outcome this driver is about to store; the stored result is the
	// record (see the stop recorded at the last instant, below). The hook only continues.
	var endCode *string
	if pending.Code != "" {
		endCode = protocol.Str(pending.Code)
	}
	if d.afterHook(ctx, rs, d.hookInput(rs, extensions.RunTerminal, nil, nil, nil, endCode)) {
		return stepResult{abandon: true}
	}
	evidence, err := d.o.Store.RunEvidenceIDs(dbctx, rs.rec.RunID)
	if err != nil {
		d.o.Diag("the run's evidence could not be listed")
		return stepResult{abandon: true}
	}
	in := sqlite.TerminalInput{ResultEvidenceID: identity.NewEvidenceID().String()}
	facts := ResultFacts{RunID: rs.rec.RunID, TaskID: rs.rec.TaskID, Outcome: *pending, EvidenceIDs: evidence, LastCheckpointID: rs.lastCheckpointID, System: rs.rec.System}
	facts.Verification = rs.verification
	if pending.UnresolvedModelAction {
		facts.UnresolvedActionIDs = slices.Clone(rs.unresolvedModel)
	}
	facts.UnresolvedActionIDs = append(facts.UnresolvedActionIDs, unresolvedTools...)
	if rs.verificationUnresolvedAction != "" {
		facts.UnresolvedActionIDs = append(facts.UnresolvedActionIDs, rs.verificationUnresolvedAction)
	}
	if pending.Status == StatusCompleted && !rs.rec.System {
		msg := identity.NewMessageID().String()
		in.FinalText, in.FinalEvidenceID = rs.finalText, identity.NewEvidenceID().String()
		facts.FinalMessageID, facts.FinalText = &msg, rs.finalText
		facts.EvidenceIDs = append(slices.Clone(evidence), in.FinalEvidenceID)
	}
	result, err := BuildRunResult(facts)
	if err != nil {
		d.o.Diag("a result could not be built from the run's records")
		return stepResult{abandon: true}
	}
	in.Result = result
	if rs.rec.System {
		in.CompactResult = d.compactResultOfRun(rs, *pending)
	}
	events, err := d.o.Store.PersistTerminal(dbctx, rs.fence, in)
	if (errors.Is(err, sqlite.ErrControlChanged) || errors.Is(err, sqlite.ErrDeadlinePassed)) && pending.Status == StatusCompleted {
		// A stop or deadline arrived before the final answer could be adopted: the Run
		// ends as the stopped Run it is, and the answer remains response Evidence only.
		stopCode := modelport.CodeCancelled
		if errors.Is(err, sqlite.ErrDeadlinePassed) {
			stopCode = CodeDeadlineExceeded
		}
		stopped := ClassifyFailure(Failure{Code: stopCode})
		if d.hooksOn() {
			d.o.Diag("a run ended as %s after its run_terminal hook was told it would end as %s", stopped.Code, pending.Code)
		}
		facts := ResultFacts{RunID: rs.rec.RunID, TaskID: rs.rec.TaskID, Outcome: stopped, EvidenceIDs: evidence, UnresolvedActionIDs: slices.Clone(unresolvedTools), Verification: rs.verification}
		if rs.verificationUnresolvedAction != "" {
			facts.UnresolvedActionIDs = append(facts.UnresolvedActionIDs, rs.verificationUnresolvedAction)
		}
		result, rerr := BuildRunResult(facts)
		if rerr != nil {
			d.o.Diag("a result could not be built from the run's records")
			return stepResult{abandon: true}
		}
		events, err = d.o.Store.PersistTerminal(dbctx, rs.fence, sqlite.TerminalInput{Result: result, ResultEvidenceID: identity.NewEvidenceID().String()})
	}
	if err != nil {
		d.o.Diag("the result could not be stored: %s", describe(err))
		return stepResult{abandon: true}
	}
	d.o.Publish(events)
	// Keep the workspace lock through verification and the terminal commit so another
	// Harness Run cannot change the checked state between the process result and receipt.
	rs.release()
	return stepResult{ev: Event{Kind: EvPersisted}}
}

// settle is the decision both recoveries share: the Run ends as OrphanOutcome says,
// with the Evidence and unresolved actions it left.
func settle(st sqlite.StaleRun) (sqlite.TerminalInput, error) {
	o := OrphanOutcomeCancelled(len(st.Unresolved), len(st.UnresolvedTools), st.CancelRequested, st.Now, st.DeadlineAt)
	facts := ResultFacts{RunID: st.RunID, TaskID: st.TaskID, Outcome: o, EvidenceIDs: st.EvidenceIDs, LastCheckpointID: st.LastCheckpointID, Verification: st.Verification}
	for _, u := range st.Unresolved {
		facts.UnresolvedActionIDs = append(facts.UnresolvedActionIDs, u.ActionID)
	}
	facts.UnresolvedActionIDs = append(facts.UnresolvedActionIDs, st.UnresolvedTools...)
	facts.System = st.System
	r, err := BuildRunResult(facts)
	if err != nil {
		return sqlite.TerminalInput{}, fmt.Errorf("kernel: a settled run's result: %w", err)
	}
	in := sqlite.TerminalInput{Result: r, ResultEvidenceID: identity.NewEvidenceID().String()}
	if st.Compaction {
		// The operation a manual compaction answers has to end with its Run: what is known of it
		// now is the Run's end and the checkpoint it left, if it left one.
		res := compactResultOfStale(st, r)
		in.CompactResult = &res
	}
	return in, nil
}

// SettleUnknownGeneration is the end of a Run admitted while its Task has an unknown model
// generation or fixed verification process (see sqlite.ResumeAdmission). It preserves the
// corresponding existing blocked classification, unresolved Actions, and verifier result.
// The new Run generates nothing and never resends the verification process.
func SettleUnknownGeneration(st sqlite.StaleRun) (sqlite.TerminalInput, error) {
	failure := Failure{Code: CodeEffectOutcomeUnknown}
	if len(st.Unresolved) > 0 {
		failure = Failure{Code: modelport.CodeOutcomeUnknown, GenerationState: modelport.StateUnknown}
	}
	o := ClassifyFailure(failure)
	facts := ResultFacts{RunID: st.RunID, TaskID: st.TaskID, Outcome: o, EvidenceIDs: st.EvidenceIDs, LastCheckpointID: st.LastCheckpointID,
		Verification: st.Verification}
	for _, u := range st.Unresolved {
		facts.UnresolvedActionIDs = append(facts.UnresolvedActionIDs, u.ActionID)
	}
	facts.UnresolvedActionIDs = append(facts.UnresolvedActionIDs, st.UnresolvedTools...)
	r, err := BuildRunResult(facts)
	if err != nil {
		return sqlite.TerminalInput{}, fmt.Errorf("kernel: the result of a run blocked by an unknown generation: %w", err)
	}
	return sqlite.TerminalInput{Result: r, ResultEvidenceID: identity.NewEvidenceID().String()}, nil
}

// StaleReconciler checks, before a Run is settled, the processes of its dispatched Tool
// calls against the host and stops those that are still the processes they were. The
// verdicts it returns (by Attempt ID) are recorded; none of them decides an effect.
type StaleReconciler interface {
	Reconcile(ctx context.Context, attempts []sqlite.ToolAttemptRef) map[string]string
}

func reconcile(ctx context.Context, rec StaleReconciler, find func() ([]sqlite.ToolAttemptRef, error)) (map[string]string, error) {
	if rec == nil {
		return nil, nil
	}
	attempts, err := find()
	if err != nil || len(attempts) == 0 {
		return nil, err
	}
	return rec.Reconcile(ctx, attempts), nil
}

// RecoverStale settles the Runs an earlier writer epoch left running on the Thread,
// for the driver that has just taken the Thread at epoch. Each ends as OrphanOutcome
// says, in one transaction with its run.terminal. It returns the events it committed.
// The processes of the Run's dispatched Tool calls are checked first, outside any
// transaction (see StaleReconciler); a nil reconciler checks none.
func RecoverStale(ctx context.Context, store *sqlite.Store, threadID string, epoch int64, rec StaleReconciler) ([]protocol.Event, error) {
	verdicts, err := reconcile(ctx, rec, func() ([]sqlite.ToolAttemptRef, error) {
		return store.StaleToolAttempts(ctx, threadID, epoch, false, nil)
	})
	if err != nil {
		return nil, err
	}
	return store.TerminalizeStale(ctx, threadID, epoch, verdicts, settle)
}

// RecoverExpired settles the Runs of the driver's own epoch that are past their
// deadline and that nobody is driving (driving says which are driven). A Run with a
// driver ends itself at its deadline; one without (this process has no model port, or
// its driver stopped without ending it) would otherwise keep the Thread busy for ever.
func RecoverExpired(ctx context.Context, store *sqlite.Store, threadID string, epoch int64, driving func(runID string) bool, rec StaleReconciler) ([]protocol.Event, error) {
	verdicts, err := reconcile(ctx, rec, func() ([]sqlite.ToolAttemptRef, error) {
		return store.StaleToolAttempts(ctx, threadID, epoch, true, driving)
	})
	if err != nil {
		return nil, err
	}
	return store.TerminalizeExpired(ctx, threadID, epoch, driving, verdicts, settle)
}

// RecoverCancelled settles the Runs of the driver's own epoch whose stop was requested
// and that nobody is driving: a Run this process admitted and never drove (it has no
// model port), or one whose driver stopped without ending it. Each ends as
// OrphanOutcomeCancelled says: cancelled when nothing it did is unresolved, and with the
// unknown outcome it left otherwise. The processes of its dispatched Tool calls are
// checked first (see StaleReconciler).
func RecoverCancelled(ctx context.Context, store *sqlite.Store, threadID string, epoch int64, driving func(runID string) bool, rec StaleReconciler) ([]protocol.Event, error) {
	verdicts, err := reconcile(ctx, rec, func() ([]sqlite.ToolAttemptRef, error) {
		return store.StaleToolAttemptsFor(ctx, threadID, epoch, sqlite.SelectCancelled, driving)
	})
	if err != nil {
		return nil, err
	}
	return store.TerminalizeCancelled(ctx, threadID, epoch, driving, verdicts, settle)
}
