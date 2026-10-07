package compaction

import (
	"context"
	"errors"
	"fmt"

	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// MaxStageRequests is how many logical requests one compaction makes to the model:
// Selection at most once and Summary at most once. A failed stage is never asked again.
const MaxStageRequests = 2

// Count is a count of the act request a projection makes, held to the request it was
// asked about (F05), with the Evidence that records it.
type Count struct {
	Result     modelport.MeasureResult
	Verdict    contextplan.Verdict
	EvidenceID string
}

// StageRequest is one stage request to run: its stage, the whole prompt, and the digest
// of its data (a fact of the stage's receipt).
type StageRequest struct {
	Stage         string
	Messages      []modelport.ChatMessage
	DatasetDigest string
}

// StageReply is how one stage generation ended. Text is the whole final answer when the
// generation gave exactly one. Otherwise Code says why not (a normalized code) and
// GenerationState what is known of the generation: terminal, not_started, or unknown.
type StageReply struct {
	Text            string
	Code            string
	GenerationState string
}

// StopError says the Run was stopped while the compaction worked: Code is the Run's stop
// code (CANCELLED, DEADLINE_EXCEEDED or DRIVER_STOPPED). Work in flight is stopped.
type StopError struct{ Code string }

func (e *StopError) Error() string { return "compaction: the run was stopped: " + e.Code }

// UnavailableError says something a compaction needs is not there: a count that can be
// verified, the strict contract, the binding it was made for. It is not a shortage of
// capacity and is not answered by reducing without the model either.
type UnavailableError struct{ Code string }

func (e *UnavailableError) Error() string { return "compaction: unavailable: " + e.Code }

// Ports are what the Engine reaches outside itself through. The kernel implements them
// over the store and the model port; a test over fakes.
type Ports interface {
	// CountAct counts the act request the projection would make. The answer is held to
	// the request; an estimate or an interval over the budget is a verdict, not an error.
	CountAct(ctx context.Context, p contextplan.Projection) (Count, error)
	// CountStage counts a stage request, so that one that cannot fit is not sent.
	CountStage(ctx context.Context, req StageRequest) (Count, error)
	// RunStage makes one stage generation (one request, no retry) and says how it ended.
	RunStage(ctx context.Context, req StageRequest) (StageReply, error)
	// ReadText is the exact text of a stored Evidence, checked against its hash.
	ReadText(ctx context.Context, evidenceID string) (string, error)
	// Record seals private Evidence of the compaction and returns its ID.
	Record(ctx context.Context, purpose, mediaType string, data []byte) (string, error)
	// NewCheckpointID issues the ID a checkpoint is made with, once per compaction.
	NewCheckpointID() string
}

// Input is one compaction: the live context, and the verified count of the act request
// that did not fit (what the checkpoint replaces).
type Input struct {
	Prepared *Prepared
	Before   Count
}

// Stages counts the stage generations a compaction asked for.
type Stages struct{ Selection, Summary int }

// Outcome is what one compaction came to, before anything is stored.
type Outcome struct {
	// Status is executed, cancelled or unavailable (stale is decided when committing).
	Status string
	// Result is, for an executed compaction, one of the outcomes except RestartRequired,
	// which only a commit can give.
	Result string
	// Candidate is the candidate to store, for NormalCompacted and EmergencyCompacted.
	Candidate *ValidatedCandidate
	// Code is the Run's stop code for a cancelled compaction and the missing capability
	// for an unavailable one.
	Code string
	// Reason says why a block or a fallback happened, in words that carry no model text.
	Reason string
	// NormalFailure is why the Normal compaction was not used when the Emergency reduction
	// was (empty when Normal succeeded or was never tried).
	NormalFailure string
	// UnknownGeneration: a stage generation ended unknown. The Run must not generate again
	// until it is resolved, whatever else this compaction came to.
	UnknownGeneration bool
	// Required and Available are the verified tokens a CapacityBlocked result needed and had.
	Required, Available *int64
	Stages              Stages
	Before, After       Count
	// Rejected are the selection operations the Host did not adopt.
	Rejected []Rejection
	// Err is the error that ended the compaction when Status is StatusError.
	Err error
}

// Engine runs a compaction over a set of Ports.
type Engine struct{ ports Ports }

// NewEngine returns an Engine over the ports.
func NewEngine(ports Ports) *Engine { return &Engine{ports: ports} }

// verifiedCount reports whether a count can be acted on, and the code an unverified one
// is answered with.
func verifiedCount(c Count) bool {
	return verified(c.Result) && (c.Verdict == contextplan.VerdictFit || c.Verdict == contextplan.VerdictNoFit)
}

// VerifiedCount reports whether a count can be acted on: it is a verified exact or bound
// count, and its verdict is a fit or a no-fit (never an estimate, nor an interval across the
// budget). A caller that only looks at the context (a dry run) holds a count to the same
// rule the Engine holds its own to.
func VerifiedCount(c Count) bool { return verifiedCount(c) }

// Compact makes the candidate of one compaction (the flow of IMPLEMENTATION_SPEC section
// 10 and the table of section 11.3):
//
//	Prepare, Selection (when something can be selected), Preflight, Summary, candidate,
//	validation: the Normal compaction.
//	A failure of the model or of its output, or a candidate that does not fit or shrink:
//	the Emergency reduction, which uses no model.
//	The smallest Normal candidate that does not fit, and a Summary request that does not
//	fit: CapacityBlocked, with no further generation and no Emergency. A reduction that
//	does not fit or shrink: CapacityBlocked.
//	A contradiction in what is stored: IntegrityBlocked. A count that cannot be verified,
//	or a capability that is missing: unavailable. The Run being stopped: cancelled.
//
// It stores nothing but private Evidence. The caller commits the candidate.
func (e *Engine) Compact(ctx context.Context, in Input) Outcome {
	o := Outcome{Status: StatusExecuted, Before: in.Before}
	p := in.Prepared
	if !verifiedCount(in.Before) {
		return Outcome{Status: StatusUnavailable, Code: modelport.CodeBudgetUnverified, Reason: "the prompt that did not fit has no verified count", Before: in.Before}
	}
	id := e.ports.NewCheckpointID()
	if len(p.WorkUnits()) == 0 {
		o.NormalFailure = "no_work_to_summarize"
	} else {
		cand, term, why := e.normal(ctx, in, id, &o)
		switch {
		case term != nil:
			return *term
		case cand != nil:
			o.Result, o.Candidate = OutcomeNormal, cand
			return o
		default:
			o.NormalFailure = why
		}
	}
	e.recordFallback(ctx, &o)
	return e.emergency(ctx, in, id, o)
}

// fail turns an error of a port or of a check into the terminal outcome it is, or reports
// that it is not one (a semantic failure, answered by falling back).
func (e *Engine) terminal(o *Outcome, err error) (*Outcome, bool) {
	var stop *StopError
	var unavail *UnavailableError
	switch {
	case errors.As(err, &stop):
		out := *o
		out.Status, out.Code = StatusCancelled, stop.Code
		return &out, true
	case errors.As(err, &unavail):
		out := *o
		out.Status, out.Code, out.Reason = StatusUnavailable, unavail.Code, "a capability the compaction needs is not available"
		return &out, true
	case errors.Is(err, ErrIntegrity):
		out := *o
		out.Result, out.Reason = OutcomeIntegrity, err.Error()
		return &out, true
	case errors.Is(err, ErrUnverified):
		out := *o
		out.Status, out.Code, out.Reason = StatusUnavailable, modelport.CodeBudgetUnverified, "a count of the compaction could not be verified"
		return &out, true
	case errors.Is(err, ErrSemantic), errors.Is(err, ErrNoFit), errors.Is(err, ErrNotShrunk):
		return nil, false // the Normal compaction cannot use this: the reduction takes over
	}
	// Anything else (a store that failed, say) is not a reason to try another way: the
	// compaction ends with that error, and the caller says what it means.
	out := *o
	out.Status, out.Err = StatusError, err
	return &out, true
}

// normal makes the Normal candidate. It returns the candidate, or a terminal outcome, or
// (neither) the reason the Normal compaction cannot be used and the Emergency reduction
// takes over.
func (e *Engine) normal(ctx context.Context, in Input, id string, o *Outcome) (*ValidatedCandidate, *Outcome, string) {
	p := in.Prepared
	semantic := func(why string) (*ValidatedCandidate, *Outcome, string) { return nil, nil, why }
	fail := func(err error) (*ValidatedCandidate, *Outcome, string) {
		if t, ok := e.terminal(o, err); ok {
			return nil, t, ""
		}
		return nil, nil, reasonOf(err)
	}

	ret := NoSelection(p)
	var sel *SelectionInput
	{
		s, err := BuildSelectionDataset(p)
		if err != nil {
			return fail(err)
		}
		if s.HasCandidates() {
			msgs, digest, err := StageMessages(modelport.StageSelection, s.Dataset)
			if err != nil {
				return fail(err)
			}
			req := StageRequest{Stage: modelport.StageSelection, Messages: msgs, DatasetDigest: digest}
			e.recordStageData(ctx, "compaction_selection_data", s.Dataset, s.manifest())
			answer, term, why := e.stage(ctx, req, o, &o.Stages.Selection)
			if term != nil {
				return nil, term, ""
			}
			if why != "" {
				return semantic("selection_" + why)
			}
			r, err := ApplySelection(p, s, answer)
			if err != nil {
				return fail(err)
			}
			ret, sel = r, s
			o.Rejected = r.Rejected
		}
	}

	// Preflight: the smallest valid Normal candidate must fit. If it does not, nothing the
	// Summary could say would make it fit, and the reduction without a model is not tried.
	minProj, _ := NormalProjection(p, ret, MinimalSummary(id), nil)
	minCount, err := e.ports.CountAct(ctx, minProj)
	if err != nil {
		return fail(err)
	}
	if !verifiedCount(minCount) {
		return nil, &Outcome{Status: StatusUnavailable, Code: modelport.CodeBudgetUnverified, Reason: "the smallest candidate could not be counted", Before: o.Before,
			Stages: o.Stages, UnknownGeneration: o.UnknownGeneration}, ""
	}
	if minCount.Verdict == contextplan.VerdictNoFit {
		out := *o
		out.Result, out.Reason = OutcomeCapacity, "the smallest valid candidate does not fit"
		out.Required, out.Available = minCount.Result.PromptUpper, i64(usableOf(minCount.Result))
		return nil, &out, ""
	}

	in2, err := BuildSummaryInput(p, ret, sel, func(evidenceID string) (string, error) { return e.ports.ReadText(ctx, evidenceID) })
	if err != nil {
		return fail(err)
	}
	msgs, digest, err := StageMessages(modelport.StageSummary, in2.Dataset)
	if err != nil {
		return fail(err)
	}
	req := StageRequest{Stage: modelport.StageSummary, Messages: msgs, DatasetDigest: digest}
	e.recordStageData(ctx, "compaction_summary_data", in2.Dataset, in2.Manifest)
	answer, term, why := e.stage(ctx, req, o, &o.Stages.Summary)
	if term != nil {
		return nil, term, ""
	}
	if why != "" {
		return semantic("summary_" + why)
	}
	res, err := ValidateSummary(in2, answer, id)
	if err != nil {
		return fail(err)
	}
	cand, err := BuildNormal(p, ret, in2, res, id, Counts{Before: in.Before.Result})
	if err != nil {
		return fail(err)
	}
	after, err := e.ports.CountAct(ctx, cand.Projection)
	if err != nil {
		return fail(err)
	}
	if !verifiedCount(after) {
		return nil, &Outcome{Status: StatusUnavailable, Code: modelport.CodeBudgetUnverified, Reason: "the candidate could not be counted", Before: o.Before, Stages: o.Stages,
			UnknownGeneration: o.UnknownGeneration}, ""
	}
	cand.AfterCount = after.Result
	o.After = after
	v, err := ValidateCandidate(ValidateInput{Candidate: cand, Prepared: p, Retention: ret, Blocks: p.Snapshot.Blocks})
	if err != nil {
		if errors.Is(err, ErrNoFit) || errors.Is(err, ErrNotShrunk) {
			return semantic("candidate_" + reasonOf(err))
		}
		return fail(err)
	}
	return &v, nil, ""
}

// stage counts and runs one stage request and returns its answer. A request that cannot
// fit is not sent. why is the reason the answer cannot be used, when it cannot; term is a
// terminal outcome (stopped, unavailable).
func (e *Engine) stage(ctx context.Context, req StageRequest, o *Outcome, counter *int) (answer string, term *Outcome, why string) {
	count, err := e.ports.CountStage(ctx, req)
	if err != nil {
		if t, ok := e.terminal(o, err); ok {
			return "", t, ""
		}
		return "", nil, "request_" + reasonOf(err)
	}
	if !verifiedCount(count) {
		out := *o
		out.Status, out.Code, out.Reason = StatusUnavailable, modelport.CodeBudgetUnverified, "a stage request could not be counted"
		return "", &out, ""
	}
	if count.Verdict == contextplan.VerdictNoFit {
		if req.Stage == modelport.StageSummary {
			// The Summary request does not fit by its tokens. IMPLEMENTATION_SPEC section 8 would
			// rebuild a legal projection of it first, and no document defines how (the excerpts
			// of an Observation are already the smallest legal form of it): there is nothing to
			// rebuild with, so the Summary request cannot be sent. That is a shortage of
			// capacity, which is answered with CapacityBlocked: no generation, and no
			// reduction without a model either. A dataset that breaks its own bounds (bytes,
			// depth, lengths of lists) is another thing, and is a Normal failure.
			out := *o
			out.Result, out.Reason = OutcomeCapacity, "the Summary request does not fit"
			out.Required, out.Available = count.Result.PromptUpper, i64(usableOf(count.Result))
			return "", &out, ""
		}
		// A Selection request that does not fit is not the capacity floor either: the Normal
		// compaction cannot make its selection (a part is never left out and called done), and
		// the reduction without a model, which has no use for a selection, may still fit.
		return "", nil, "request_does_not_fit"
	}
	if *counter+1 > 1 || o.Stages.Selection+o.Stages.Summary >= MaxStageRequests {
		panic("compaction: a stage was asked for twice")
	}
	*counter++
	reply, err := e.ports.RunStage(ctx, req)
	if reply.GenerationState == modelport.StateUnknown {
		o.UnknownGeneration = true
	}
	if err != nil {
		if t, ok := e.terminal(o, err); ok {
			return "", t, ""
		}
		return "", nil, "generation_" + reasonOf(err)
	}
	switch reply.Code {
	case "":
		return reply.Text, nil, ""
	case modelport.CodeCancelled, modelport.CodePermitRevoked:
		out := *o
		out.Status, out.Code = StatusCancelled, reply.Code
		return "", &out, ""
	case modelport.CodeBudgetUnverified, modelport.CodeUnsupportedContract, modelport.CodeUnsupportedRecovery, modelport.CodeBindingChanged, modelport.CodeAuthFailed,
		modelport.CodeInputDigestMismatch, modelport.CodeRequestDigestMismatch:
		out := *o
		out.Status, out.Code, out.Reason = StatusUnavailable, reply.Code, "the model side cannot serve this stage request"
		return "", &out, ""
	}
	return "", nil, "failed_" + reply.Code
}

func usableOf(m modelport.MeasureResult) int64 {
	u, _ := Usable(m)
	return u
}

func reasonOf(err error) string {
	switch {
	case errors.Is(err, ErrNoFit):
		return "no_fit"
	case errors.Is(err, ErrNotShrunk):
		return "not_shrunk"
	case errors.Is(err, ErrSemantic):
		return "semantic"
	}
	return "error"
}

// emergency makes the candidate of the reduction that needs no model.
func (e *Engine) emergency(ctx context.Context, in Input, id string, o Outcome) Outcome {
	p := in.Prepared
	em, err := Emergency(p)
	if err != nil {
		t, _ := e.terminal(&o, err)
		return *t
	}
	if len(em.Replaced) == 0 {
		o.Result, o.Reason = OutcomeCapacity, "nothing can be replaced by a reference"
		return o
	}
	cand, err := BuildEmergency(p, em, id, Counts{Before: in.Before.Result})
	if err != nil {
		t, _ := e.terminal(&o, fmt.Errorf("%w: %v", ErrIntegrity, err))
		return *t
	}
	after, err := e.ports.CountAct(ctx, cand.Projection)
	if err != nil {
		if t, ok := e.terminal(&o, err); ok {
			return *t
		}
		o.Status, o.Code, o.Reason = StatusUnavailable, modelport.CodeBudgetUnverified, "the reduction could not be counted"
		return o
	}
	o.After = after
	if !verifiedCount(after) {
		o.Status, o.Code, o.Reason = StatusUnavailable, modelport.CodeBudgetUnverified, "the reduction could not be counted"
		return o
	}
	cand.AfterCount = after.Result
	v, err := ValidateCandidate(ValidateInput{Candidate: cand, Prepared: p, Emergency: &em, Blocks: p.Snapshot.Blocks})
	switch {
	case err == nil:
		o.Result, o.Candidate = OutcomeEmergency, &v
	case errors.Is(err, ErrNoFit) || errors.Is(err, ErrNotShrunk):
		o.Result, o.Reason = OutcomeCapacity, "the reduction "+reasonOf(err)
		o.Required, o.Available = after.Result.PromptUpper, i64(usableOf(after.Result))
		if *o.Required <= *o.Available {
			o.Required, o.Available = nil, nil // a shortfall is only claimed when there is one
		}
	default:
		if t, ok := e.terminal(&o, err); ok {
			return *t
		}
		o.Result, o.Reason = OutcomeIntegrity, err.Error()
	}
	return o
}

// recordFallback keeps why the Normal compaction was not used as private Evidence (a
// short code, never model text), so that the Emergency checkpoint can be read back
// to its cause.
func (e *Engine) recordFallback(ctx context.Context, o *Outcome) {
	raw, err := cj1(struct {
		Kind          string `json:"kind"`
		Reason        string `json:"reason"`
		Selection     int    `json:"selection_requests"`
		Summary       int    `json:"summary_requests"`
		Unknown       bool   `json:"unknown_generation"`
		RejectedCount int    `json:"rejected_selection_operations"`
	}{"compaction_normal_failure", o.NormalFailure, o.Stages.Selection, o.Stages.Summary, o.UnknownGeneration, len(o.Rejected)})
	if err == nil {
		_, _ = e.ports.Record(ctx, "compaction_normal_failure", "application/json", []byte(raw))
	}
}

// recordStageData keeps the data a stage was given and the Host's manifest for it as
// private Evidence: what each handle stood for is read back from there.
func (e *Engine) recordStageData(ctx context.Context, purpose string, dataset any, manifest any) {
	for _, item := range []struct {
		purpose string
		v       any
	}{{purpose, dataset}, {purpose + "_manifest", manifest}} {
		raw, err := cj1(item.v)
		if err != nil {
			continue
		}
		_, _ = e.ports.Record(ctx, item.purpose, "application/json", []byte(raw))
	}
}

// manifest is the Host's record of what the presented handles stand for.
func (s *SelectionInput) manifest() any {
	type entry struct {
		Handle string             `json:"handle"`
		Ref    protocol.SourceRef `json:"source_ref"`
	}
	out := []entry{}
	for _, p := range s.Pieces {
		out = append(out, entry{p.Piece.Handle, p.Ref})
	}
	for _, l := range s.Links {
		out = append(out, entry{l.Link.Handle + "/call", l.CallRef}, entry{l.Link.Handle + "/output", l.OutputRef})
	}
	return out
}

func i64(v int64) *int64 { return &v }
