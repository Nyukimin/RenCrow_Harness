package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/extensions"
	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// adoptSnapshot makes a loaded snapshot the Run's. When the Thread stands on a checkpoint it
// is parsed (the exact bytes, the schema, the hash) and held to the row it was stored in, and
// the checkpoint that accepted its Summary is found again when that is another one. A
// checkpoint that is named and cannot be read back is the end of the Run (INTEGRITY_BLOCKED):
// it is never taken for none, and an older one is never used in its place.
func (d *Driver) adoptSnapshot(rs *runState, snap sqlite.Snapshot) *Failure {
	rs.snap, rs.checkpoint, rs.summarySource = snap, nil, nil
	cp, ss, err := loadStanding(context.WithoutCancel(context.Background()), d.o.Store, rs.rec.ThreadID, snap)
	if err != nil {
		d.o.Diag("%s", err.Error())
		return &Failure{Code: CodeIntegrityBlocked}
	}
	rs.checkpoint, rs.summarySource = cp, ss
	return nil
}

// loadStanding reads back the checkpoint a snapshot stands on, and the checkpoint that accepted
// its Summary when that is another one (an Emergency checkpoint keeps the Summary of an
// earlier one, and a forked Thread's checkpoint keeps the one of the Thread it was forked from,
// which the Thread imported). Everything is held to what the store recorded: the exact bytes
// and their hash, the format, the row's IDs, mode, parent, boundaries and revision. A
// checkpoint that is named and cannot be read back exactly is an error, never "no
// checkpoint", and no other checkpoint is looked at in its place.
func loadStanding(ctx context.Context, store *sqlite.Store, threadID string, snap sqlite.Snapshot) (cp, summarySource *compaction.Checkpoint, err error) {
	row := snap.Checkpoint
	if row == nil {
		return nil, nil, nil
	}
	cp, err = compaction.ParseCheckpoint(row.Blob, row.Hash)
	if err == nil {
		err = cp.CheckStored(compaction.StoredFacts{CheckpointID: row.CheckpointID, ThreadID: row.ThreadID, RunID: row.RunID, ParentCheckpointID: row.ParentCheckpointID,
			Mode: row.Mode, ContextRevision: row.ContextRevision, SemanticBoundary: row.SemanticBoundary, DurableBoundary: row.DurableBoundary, Hash: row.Hash})
	}
	if err != nil {
		return nil, nil, errors.New("the current checkpoint cannot be read back exactly")
	}
	if s := cp.Candidate.Projection.Summary; s != nil && s.SourceCheckpointID != cp.Candidate.CheckpointID {
		src, err := store.LoadScopedCheckpoint(ctx, threadID, s.SourceCheckpointID)
		if err == nil {
			summarySource, err = compaction.ParseCheckpoint(src.Blob, src.Hash)
		}
		if err != nil {
			return nil, nil, errors.New("the checkpoint that accepted the current summary cannot be read back")
		}
	}
	return cp, summarySource, nil
}

func sourcesOf(entries []sqlite.AppliedEntry) []compaction.Source {
	out := make([]compaction.Source, 0, len(entries))
	for _, e := range entries {
		out = append(out, compaction.Source{
			MessageID: e.MessageID, ContextSeq: e.ContextSeq, ItemSeq: e.Sequence, HistoryKind: e.HistoryKind, Origin: e.Origin, ItemKind: e.ItemKind, ToolCallID: e.ToolCallID,
			EvidenceID: e.EvidenceID, RawHash: e.RawHash, TotalBytes: e.TotalBytes, CaptureComplete: e.CaptureComplete, TextProjection: e.TextProjection, Text: e.Text,
			Presented: e.Presented,
		})
	}
	return out
}

// compact (F06-F13) makes the candidate of a compaction for the prompt that did not fit, from
// a snapshot read again now. It stores nothing but private Evidence; whatever it came to is
// recorded as one Evidence of the Run, and the step's result is either a candidate to commit
// or the end of the Run with the code that says why.
func (d *Driver) compact(ctx, runCtx context.Context, rs *runState, early bool) stepResult {
	// before_compact: before anything of the compaction is begun (no snapshot read for it, no
	// count, no stage, no candidate), so a hook that denies leaves nothing of it behind. A deny
	// ends the Run blocked, an early compaction's too: the Run is not sent on with the prompt
	// that fits, because what the host denied is the compaction. The hook is told the count that
	// led to it. The commit of a candidate is the store's one short transaction and has no hook
	// of its own before it: a hook there could only make the candidate stale.
	if end := d.beforeHook(runCtx, rs, d.hookInput(rs, extensions.BeforeCompact, nil, nil, []string{rs.measuredID}, nil)); end != nil {
		return end.stepOf()
	}
	snap, err := d.o.Store.LoadSnapshot(d.dbCtx(ctx), rs.rec.RunID)
	if err != nil {
		return d.storeFailure(err)
	}
	if f := d.adoptSnapshot(rs, snap); f != nil {
		return failed(*f)
	}
	rs.candidate = nil
	cs := compaction.Snapshot{
		ThreadID: rs.rec.ThreadID, TaskID: rs.rec.TaskID, RunID: rs.rec.RunID,
		ContextRevision: snap.ContextRevision, ControlRevision: rs.fence.ControlRevision, WriterEpoch: rs.fence.Epoch,
		PolicyRevision: rs.rec.PolicyRevision, BindingRevision: rs.rec.BindingRevision,
		Blocks: snap.Blocks, Prior: rs.checkpoint, SummaryCheckpoint: rs.summarySource, Tail: sourcesOf(snap.Applied),
	}
	prepared, err := compaction.Prepare(cs)
	if err != nil {
		d.recordCompactResult(ctx, rs, compaction.Outcome{Status: compaction.StatusExecuted, Result: compaction.OutcomeIntegrity, Reason: "the live context is inconsistent"})
		return failed(Failure{Code: CodeIntegrityBlocked})
	}
	ports := &compactPorts{d: d, ctx: ctx, runCtx: runCtx, rs: rs}
	before := compaction.Count{Result: rs.measured, Verdict: rs.est.Verdict, EvidenceID: rs.measuredID}
	out := compaction.NewEngine(ports).Compact(runCtx, compaction.Input{Prepared: prepared, Before: before})
	if ports.stepErr != nil {
		return *ports.stepErr
	}
	return d.compactResult(ctx, rs, out, early)
}

// compactResult turns what a compaction came to into the machine's next event.
//
// A compaction that was started early (the trigger ratio, a prompt that still fits) is an
// optimization, and what it could not do is not the Run's end: where it came to no
// checkpoint for want of capacity or of a verifiable count, the Run goes on with the prompt
// it has (EvCompactSkipped). What a Run cannot go on past is the same as ever: a stop, a
// store that failed, stored state that contradicts itself, and a stage generation whose
// outcome is unknown (the Run does not generate again while one is unresolved).
func (d *Driver) compactResult(ctx context.Context, rs *runState, out compaction.Outcome, early bool) stepResult {
	rs.cOutcome = out
	if f := d.recordCompactResult(ctx, rs, out); f != nil {
		return *f
	}
	state := ""
	if out.UnknownGeneration {
		state = modelport.StateUnknown
	}
	if early && out.Candidate == nil && !out.UnknownGeneration && out.Status != compaction.StatusError && out.Status != compaction.StatusCancelled &&
		out.Result != compaction.OutcomeIntegrity {
		return stepResult{ev: Event{Kind: EvCompactSkipped, Compact: CompactReady{Generations: int64(out.Stages.Selection + out.Stages.Summary)}}}
	}
	switch out.Status {
	case compaction.StatusError:
		return d.storeFailure(out.Err)
	case compaction.StatusCancelled, compaction.StatusUnavailable:
		return failed(Failure{Code: out.Code, GenerationState: state})
	}
	switch out.Result {
	case compaction.OutcomeIntegrity:
		return failed(Failure{Code: CodeIntegrityBlocked, GenerationState: state})
	case compaction.OutcomeCapacity:
		return failed(Failure{Code: CodeCapacityBlocked, GenerationState: state})
	case compaction.OutcomeNormal, compaction.OutcomeEmergency:
		if out.Candidate != nil {
			rs.candidate = out.Candidate
			return stepResult{ev: Event{Kind: EvCompactReady, Compact: CompactReady{Generations: int64(out.Stages.Selection + out.Stages.Summary), Unknown: out.UnknownGeneration}}}
		}
	}
	d.o.Diag("a compaction came to something this driver does not know")
	return failed(Failure{Code: CodeInternalError, GenerationState: state})
}

// recordCompactResult keeps what a compaction came to as private Evidence of the Run: the
// status and outcome, why the Normal compaction was not used when it was not, how many stage
// requests were made, and whether one of them is unresolved. It carries no model text.
func (d *Driver) recordCompactResult(ctx context.Context, rs *runState, out compaction.Outcome) *stepResult {
	type result struct {
		Kind            string  `json:"kind"`
		Status          string  `json:"status"`
		Outcome         *string `json:"outcome"`
		Code            *string `json:"code"`
		Reason          string  `json:"reason"`
		NormalFailure   string  `json:"normal_failure"`
		Selection       int     `json:"selection_requests"`
		Summary         int     `json:"summary_requests"`
		Unknown         bool    `json:"unknown_generation"`
		Rejected        int     `json:"rejected_selection_operations"`
		Required        *int64  `json:"required_minimum_tokens"`
		Available       *int64  `json:"available_tokens"`
		CheckpointID    *string `json:"checkpoint_id"`
		SemanticBoundry *int64  `json:"semantic_boundary"`
		DurableBoundary *int64  `json:"durable_boundary"`
		BeforeEvidence  *string `json:"before_count_evidence_id"`
		AfterEvidence   *string `json:"after_count_evidence_id"`
	}
	r := result{Kind: "compaction_result", Status: out.Status, Reason: out.Reason, NormalFailure: out.NormalFailure, Selection: out.Stages.Selection, Summary: out.Stages.Summary,
		Unknown: out.UnknownGeneration, Rejected: len(out.Rejected), Required: out.Required, Available: out.Available}
	if out.Result != "" {
		r.Outcome = protocol.Str(out.Result)
	}
	if out.Code != "" {
		r.Code = protocol.Str(out.Code)
	}
	if out.Candidate != nil {
		c := out.Candidate.Candidate()
		r.CheckpointID, r.SemanticBoundry, r.DurableBoundary = protocol.Str(c.CheckpointID), &c.SemanticBoundary, &c.DurableBoundary
		r.BeforeEvidence, r.AfterEvidence = protocol.Str(out.Before.EvidenceID), protocol.Str(out.After.EvidenceID)
	}
	raw, err := json.Marshal(r)
	if err == nil {
		raw, err = protocol.EncodeCanonicalContract(raw)
	}
	if err != nil {
		f := failed(Failure{Code: CodeInternalError})
		return &f
	}
	if _, err := d.o.Store.RecordEvidence(d.dbCtx(ctx), rs.fence, "compaction_result", "application/json", false, raw); err != nil {
		f := d.storeFailure(err)
		return &f
	}
	return nil
}

// sourceChecks lists every source a candidate names, for the commit to find again.
func sourceChecks(c compaction.Candidate) []sqlite.SourceCheck {
	var out []sqlite.SourceCheck
	add := func(r protocol.SourceRef) {
		out = append(out, sqlite.SourceCheck{SourceID: r.SourceID, RawHash: r.RawHash, ProjectionVersion: r.ProjectionVersion, End: r.Range.End})
	}
	for _, r := range c.RetainedExactRefs {
		add(r)
	}
	for _, op := range c.AppliedSelection {
		add(op.Target)
		for _, r := range op.Evidence {
			add(r)
		}
	}
	if s := c.Projection.Summary; s != nil {
		for _, m := range s.SourceMap {
			for _, r := range m.Sources {
				add(r)
			}
		}
	}
	for _, e := range c.Projection.Entries {
		for _, srcs := range e.MessageSources {
			for _, r := range srcs {
				add(r)
			}
		}
	}
	return out
}

// commitCheckpoint (F14) stores the validated candidate: one short transaction that compares
// everything the candidate was made against, including the control revision, so that a stop
// recorded first wins and a candidate made against a context that moved is not stored. A
// candidate that went stale is made again from the new context (once). A commit whose answer
// was lost is resolved by the checkpoint's issued ID and the hash of its bytes: stored, it is
// the same commit; not stored, or not readable, the Run ends restart_required.
func (d *Driver) commitCheckpoint(ctx context.Context, rs *runState) stepResult {
	if rs.candidate == nil {
		d.o.Diag("a commit was asked for with no candidate")
		return failed(Failure{Code: CodeInternalError})
	}
	c := rs.candidate.Candidate()
	counts, err := json.Marshal(struct {
		Before modelport.MeasureResult `json:"before"`
		After  modelport.MeasureResult `json:"after"`
	}{c.BeforeCount, c.AfterCount})
	if err == nil {
		counts, err = protocol.EncodeCanonicalContract(counts)
	}
	if err != nil {
		return failed(Failure{Code: CodeInternalError})
	}
	in := sqlite.CheckpointCommit{
		CheckpointID: c.CheckpointID, ParentCheckpointID: c.ParentCheckpointID, Mode: c.Mode, Blob: rs.candidate.Bytes(), Hash: rs.candidate.Hash(),
		ExpectedContextRevision: c.Expected.ContextRevision, PolicyRevision: c.Expected.PolicyRevision, BindingRevision: c.Expected.BindingRevision,
		SemanticBoundary: c.SemanticBoundary, DurableBoundary: c.DurableBoundary, CountJSON: string(counts),
		BeforeEvidenceID: rs.cOutcome.Before.EvidenceID, AfterEvidenceID: rs.cOutcome.After.EvidenceID, Sources: sourceChecks(c),
	}
	res, err := d.o.Store.CommitCheckpoint(d.dbCtx(ctx), rs.fence, in)
	// The context revision moves by exactly one at a commit; it is what the commit made.
	committedRevision := in.ExpectedContextRevision + 1
	switch {
	case err == nil:
		committedRevision = res.NewContextRevision
		d.o.Publish(res.Events)
	case errors.Is(err, sqlite.ErrStale):
		rs.candidate = nil
		return stepResult{ev: Event{Kind: EvCommitStale}}
	case protocol.CodeOf(err) == protocol.CodePersistenceUncertain:
		state, rerr := d.o.Store.ResolveCheckpoint(d.dbCtx(ctx), in.CheckpointID, in.Hash)
		if protocol.CodeOf(rerr) == protocol.CodeIntegrityBlocked {
			// A checkpoint of this ID is stored with other bytes: a contradiction in what is
			// stored, not an unknown.
			return failed(Failure{Code: CodeIntegrityBlocked})
		}
		if rerr != nil || state != sqlite.CommitStored {
			d.o.Diag("a checkpoint commit's outcome could not be established")
			return failed(Failure{Code: CodePersistenceUncertain})
		}
	default:
		return d.storeFailure(err)
	}
	rs.lastCheckpointID = protocol.Str(c.CheckpointID)
	rs.candidate = nil
	// after_compact: the checkpoint is committed (a commit that was found stored after its answer
	// was lost is the same commit), and what a hook answers changes nothing of it. It is called
	// right after the commit and before the context is read again, so that a read that fails
	// does not leave a committed checkpoint without it. It is not called for a candidate that
	// went stale or was not stored: nothing was compacted. The code is the checkpoint's mode.
	hin := d.hookInput(rs, extensions.AfterCompact, nil, nil, []string{in.BeforeEvidenceID, in.AfterEvidenceID}, protocol.Str(in.Mode))
	hin.ContextRevision = max(committedRevision, 0)
	if d.afterHook(ctx, rs, hin) {
		return stepResult{abandon: true}
	}
	snap, err := d.o.Store.LoadSnapshot(d.dbCtx(ctx), rs.rec.RunID)
	if err != nil {
		return d.storeFailure(err)
	}
	if f := d.adoptSnapshot(rs, snap); f != nil {
		return failed(*f)
	}
	return stepResult{ev: Event{Kind: EvCommitted}}
}

// compactPorts is what the Engine reaches the Run's model and store through. Anything the
// Engine cannot be told in its own terms (a store that failed) is kept in stepErr, and ends
// the step with the result the driver gives such a failure.
type compactPorts struct {
	d      *Driver
	ctx    context.Context // outlives the Run's stop: what is written must be written
	runCtx context.Context
	rs     *runState

	stepErr *stepResult
	stages  map[string]*stagePrep
	// dry says nothing of this is stored: the counts of a dry run and of a fork's projection
	// are not Evidence of any Run.
	dry bool
}

// stagePrep is a stage request that was counted, kept for the generation that follows.
type stagePrep struct {
	req      modelport.ChatRequest
	input    string
	request  string
	profile  modelport.RecoveryProfile
	messages []modelport.ChatMessage
}

var errPortStore = errors.New("kernel: a store operation of the compaction failed")

// storeFail keeps a store failure as the result of the step and returns the error the Engine
// ends with.
func (p *compactPorts) storeFail(err error) error {
	r := p.d.storeFailure(err)
	p.stepErr = &r
	return fmt.Errorf("%w", errPortStore)
}

// modelErr is what an error of the model port is to the compaction: the Run being stopped,
// or a capability the model side does not give.
func (p *compactPorts) modelErr(err error) error {
	var se *modelport.StrictError
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || p.runCtx.Err() != nil:
		return &compaction.StopError{Code: stopCode(p.runCtx, err)}
	case errors.As(err, &se):
		return &compaction.UnavailableError{Code: se.Code}
	}
	return &compaction.UnavailableError{Code: modelport.CodeBudgetUnverified}
}

// count measures a request the compaction asks about: no generation, the answer held to the
// request it was asked about, and the measure recorded as Evidence.
func (p *compactPorts) count(req modelport.ChatRequest) (compaction.Count, string, error) {
	d, rs := p.d, p.rs
	if err := req.Validate(false); err != nil {
		return compaction.Count{}, "", &compaction.UnavailableError{Code: modelport.CodeUnsupportedContract}
	}
	digest, err := req.LogicalInputDigest()
	if err != nil {
		return compaction.Count{}, "", &compaction.UnavailableError{Code: modelport.CodeUnsupportedContract}
	}
	res, err := d.o.Model.Measure(p.runCtx, modelport.MeasureRequest{ContractVersion: modelport.ContractVersion, Request: withRequestID(req, identity.NewRequestID().String()), SafetyMarginTokens: d.o.SafetyMarginTokens})
	if err != nil {
		return compaction.Count{}, "", p.modelErr(err)
	}
	if err := modelport.ValidateMeasureResult(res); err != nil {
		return compaction.Count{}, "", &compaction.UnavailableError{Code: modelport.CodeContractFailed}
	}
	est, err := contextplan.EstimateBudget(contextplan.BudgetInput{Measure: res, InputDigest: digest, BindingFingerprint: rs.desc.BindingFingerprint,
		ReservedOutputTokens: req.MaxTokens, SafetyMarginTokens: d.o.SafetyMarginTokens})
	var mm *contextplan.MismatchError
	if errors.As(err, &mm) {
		return compaction.Count{}, "", &compaction.UnavailableError{Code: mm.Code}
	}
	if err != nil {
		return compaction.Count{}, "", &compaction.UnavailableError{Code: modelport.CodeContractFailed}
	}
	raw, err := json.Marshal(res)
	if err == nil {
		raw, err = protocol.EncodeCanonicalContract(raw)
	}
	if err != nil {
		return compaction.Count{}, "", p.storeFail(err)
	}
	evidenceID := ""
	if !p.dry {
		if evidenceID, err = d.o.Store.RecordEvidence(d.dbCtx(p.ctx), rs.fence, "measure_result", "application/json", false, raw); err != nil {
			return compaction.Count{}, "", p.storeFail(err)
		}
	}
	return compaction.Count{Result: res, Verdict: est.Verdict, EvidenceID: evidenceID}, digest, nil
}

// actRequestOf is the act request the projection would make: the same prompt function as a
// live history, with the Run's own blocks, and the same Tools and options as an act step.
func (p *compactPorts) actRequestOf(proj contextplan.Projection) (modelport.ChatRequest, error) {
	d, rs := p.d, p.rs
	system, err := contextplan.ActSystemPrompt()
	if err != nil {
		return modelport.ChatRequest{}, p.storeFail(err)
	}
	proj.ContextBlocks = rs.snap.Blocks
	plan, err := contextplan.AssembleContext(contextplan.Input{SystemPrompt: system, Projection: &proj})
	if err != nil {
		return modelport.ChatRequest{}, fmt.Errorf("%w: a candidate cannot be made into a prompt", compaction.ErrIntegrity)
	}
	req, err := d.actRequest(rs, plan.Messages, modelport.RecoveryRequest{ProfileID: modelport.ProfileSameRequest, ProfileRevision: rs.profile.ProfileRevision})
	if err != nil {
		return modelport.ChatRequest{}, &compaction.UnavailableError{Code: modelport.CodeUnsupportedContract}
	}
	return req, nil
}

// CountAct counts the act request the projection would make.
func (p *compactPorts) CountAct(_ context.Context, proj contextplan.Projection) (compaction.Count, error) {
	req, err := p.actRequestOf(proj)
	if err != nil {
		return compaction.Count{}, err
	}
	c, _, err := p.count(req)
	return c, err
}

// stageRequest is the request of a stage, with the recovery profile the Run's policy and the
// binding both allow (the same request: a stage is never retried).
func (p *compactPorts) stageRequest(sr compaction.StageRequest) (*stagePrep, error) {
	d, rs := p.d, p.rs
	prof, ok := rs.desc.RecoveryProfile(modelport.ProfileSameRequest, sr.Stage)
	if !ok || !slices.Contains(rs.rec.Recovery.AllowedProfiles, modelport.ProfileSameRequest) {
		return nil, &compaction.UnavailableError{Code: modelport.CodeUnsupportedRecovery}
	}
	req, err := modelport.NewStageRequest(modelport.StageParams{
		Binding: rs.rec.Binding, Descriptor: rs.desc, Stage: sr.Stage, Messages: sr.Messages,
		Meta:     modelport.RequestMeta{TraceID: rs.rec.TraceID, TaskID: rs.rec.TaskID, SessionID: rs.rec.SessionID, Initiator: d.o.Initiator, Caller: "rencrow-harness"},
		Recovery: modelport.RecoveryRequest{ProfileID: prof.ProfileID, ProfileRevision: prof.ProfileRevision},
	})
	if err != nil {
		return nil, &compaction.UnavailableError{Code: modelport.CodeUnsupportedContract}
	}
	return &stagePrep{req: req, profile: prof, messages: sr.Messages}, nil
}

// CountStage counts a stage request so that one that cannot fit is not sent, and keeps the
// request and its count for the generation.
func (p *compactPorts) CountStage(_ context.Context, sr compaction.StageRequest) (compaction.Count, error) {
	prep, err := p.stageRequest(sr)
	if err != nil {
		return compaction.Count{}, err
	}
	c, digest, err := p.count(prep.req)
	if err != nil {
		return compaction.Count{}, err
	}
	prep.input, prep.request = digest, c.Result.RequestDigest
	if p.stages == nil {
		p.stages = map[string]*stagePrep{}
	}
	p.stages[sr.Stage] = prep
	return c, nil
}

// RunStage makes one stage generation: the attempt is reserved in one transaction right before
// it is sent (a stop recorded first sends nothing), one request is sent, the strict response
// is read to its end and held to the request, and how it ended is recorded whatever it was.
// A stage is never retried. A stage is asked of the model only for text: an answer that is
// anything but one final answer is a failure, whatever else it carries.
func (p *compactPorts) RunStage(_ context.Context, sr compaction.StageRequest) (compaction.StageReply, error) {
	d, rs := p.d, p.rs
	prep := p.stages[sr.Stage]
	if prep == nil || len(prep.messages) != len(sr.Messages) {
		return compaction.StageReply{}, p.storeFail(errors.New("a stage was generated that was not counted"))
	}
	gen := withRequestID(prep.req, identity.NewRequestID().String()).WithExpected(prep.input, prep.request, rs.desc.BindingFingerprint)
	if err := gen.Validate(true); err != nil {
		return compaction.StageReply{}, &compaction.UnavailableError{Code: modelport.CodeUnsupportedContract}
	}
	reqBytes, err := gen.Canonical()
	if err != nil {
		return compaction.StageReply{}, p.storeFail(err)
	}
	args, err := protocol.LogicalInputCJ1(reqBytes)
	if err != nil {
		return compaction.StageReply{}, p.storeFail(err)
	}
	// before_model, as for an act generation: before the reservation, so a hook that denies
	// leaves no attempt reserved and nothing sent. The compaction ends with the Run's stop or
	// the hook's code; the stage was not started.
	if end := d.beforeHook(p.runCtx, rs, d.modelHookInput(rs, extensions.BeforeModel, sr.Stage, nil, nil, nil)); end != nil {
		if end.abandon {
			r := end.stepOf()
			p.stepErr = &r
			return compaction.StageReply{}, fmt.Errorf("%w", errPortStore)
		}
		return compaction.StageReply{GenerationState: modelport.StateNotStarted}, &compaction.StopError{Code: end.code}
	}
	rsv, err := d.o.Store.ReserveGeneration(d.dbCtx(p.ctx), rs.fence, sqlite.ReserveInput{
		Stage: sr.Stage, RequestID: *gen.Rencrow.RequestID, ArgsBytes: args, RequestBytes: reqBytes,
		InputDigest: prep.input, RequestDigest: prep.request, BindingFingerprint: rs.desc.BindingFingerprint,
		ProfileID: prep.profile.ProfileID, ProfileRevision: prep.profile.ProfileRevision, Stream: false,
	})
	switch {
	case err == nil:
	case errors.Is(err, sqlite.ErrControlChanged):
		return compaction.StageReply{GenerationState: modelport.StateNotStarted}, &compaction.StopError{Code: modelport.CodeCancelled}
	case errors.Is(err, sqlite.ErrDeadlinePassed):
		return compaction.StageReply{GenerationState: modelport.StateNotStarted}, &compaction.StopError{Code: CodeDeadlineExceeded}
	case errors.Is(err, sqlite.ErrGenerationBudget):
		// No generation attempt is left: the Normal compaction cannot ask the model, and the
		// reduction without a model is what is left.
		return compaction.StageReply{Code: CodeGenerationBudgetExhausted, GenerationState: modelport.StateNotStarted}, nil
	default:
		return compaction.StageReply{}, p.storeFail(err)
	}
	d.o.Publish(rsv.Events)

	exp := modelport.Expected{Stage: sr.Stage, InputDigest: prep.input, RequestDigest: prep.request, BindingFingerprint: rs.desc.BindingFingerprint,
		ProfileID: prep.profile.ProfileID, ProfileRevision: prep.profile.ProfileRevision}
	end := sqlite.AttemptEnd{AttemptID: rsv.AttemptID}
	srs := &runState{profile: prep.profile}
	var completion modelport.Completion
	stream, err := d.o.Model.Generate(p.runCtx, gen)
	if err != nil {
		completion = d.refusedCompletion(p.runCtx, srs, err, &end)
	} else {
		completion = d.readStage(p.runCtx, srs, stream, exp, &end)
	}
	judged := Generated{Kind: completion.TerminalKind, FailureCode: completion.FailureCode, GenerationState: completion.GenerationState, HiddenRetry: completion.HiddenRetry}
	j := Judge(judged)
	if f := d.finishAttempt(p.ctx, rs, sr.Stage, prep.profile, &end, completion, j, rsv.ActionID); f != nil {
		p.stepErr = f
		return compaction.StageReply{}, fmt.Errorf("%w", errPortStore)
	}
	reply := compaction.StageReply{GenerationState: completion.GenerationState}
	if j.Usable && completion.FinalText != "" {
		reply.Text = completion.FinalText
		return reply, nil
	}
	code := j.FailureCode
	if code == "" {
		code = modelport.CodeContractFailed
	}
	switch code {
	case modelport.CodeCancelled, CodeDeadlineExceeded, CodeDriverStopped:
		return reply, &compaction.StopError{Code: code}
	}
	reply.Code = code
	return reply, nil
}

// readStage reads a stage's response to its end and holds its receipt to the request. Its
// text is no provisional output of the Run, so nothing is announced as progress.
func (d *Driver) readStage(ctx context.Context, rs *runState, stream io.ReadCloser, exp modelport.Expected, end *sqlite.AttemptEnd) modelport.Completion {
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	defer stream.Close()
	capture := &captureReader{r: stream, max: modelport.MaxStreamBytes + 64<<10}
	c := modelport.AssembleStrictStream(capture, modelport.AssembleOptions{MaxToolCalls: 0})
	end.RawResponse = capture.buf.Bytes()
	if rb, ok := stream.(modelport.RawBodier); ok {
		end.RawResponse, end.RawResponseMediaType = rb.RawBody(), "application/json"
	}
	id := identity.NewResponseID().String()
	end.ResponseID = &id
	if c.TerminalKind == modelport.KindError && !c.TerminalObserved && ctx.Err() != nil {
		c.FailureCode = stopCode(ctx, nil)
	}
	return modelport.CheckCompletionReceipt(c, exp, rs.profile.Transformations)
}

// ReadText is the full text of a stored Evidence of the Thread, checked against its hash.
func (p *compactPorts) ReadText(_ context.Context, evidenceID string) (string, error) {
	text, err := p.d.o.Store.ReadEvidenceText(p.d.dbCtx(p.ctx), p.rs.rec.ThreadID, evidenceID)
	if err != nil {
		if protocol.CodeOf(err) == protocol.CodeIntegrityBlocked {
			return "", fmt.Errorf("%w: a source cannot be read back", compaction.ErrIntegrity)
		}
		return "", p.storeFail(err)
	}
	return text, nil
}

// Record seals private Evidence of the compaction. A failure is not the compaction's to
// answer: the next write finds the Run's state out.
func (p *compactPorts) Record(_ context.Context, purpose, mediaType string, data []byte) (string, error) {
	return p.d.o.Store.RecordEvidence(p.d.dbCtx(p.ctx), p.rs.fence, purpose, mediaType, false, data)
}

// NewCheckpointID issues the checkpoint's ID, once, before the candidate is made.
func (p *compactPorts) NewCheckpointID() string { return identity.NewCheckpointID().String() }

// compactResultOfRun is the CompactResult of a manual compaction's Run that this driver
// ended: what the Engine came to and the checkpoint the Run committed, if it committed one.
func (d *Driver) compactResultOfRun(rs *runState, end Outcome) *protocol.CompactResult {
	f := CompactFacts{RunStatus: end.Status, RunCode: end.Code, Outcome: &rs.cOutcome}
	if rs.lastCheckpointID != nil && rs.cOutcome.Candidate != nil {
		c := rs.cOutcome.Candidate.Candidate()
		f.Checkpoint = &CommittedCheckpoint{ID: c.CheckpointID, Mode: c.Mode, SemanticBoundary: c.SemanticBoundary, DurableBoundary: c.DurableBoundary,
			Before: c.BeforeCount, After: c.AfterCount}
	}
	res := BuildCompactResult(f)
	return &res
}
