package compaction_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

// rig is one compaction to run: a live context with a large Tool answer in it, the ports
// around it, and the count of the prompt that did not fit.
type rig struct {
	t     testing.TB
	w     *world
	p     *compaction.Prepared
	ports *fakePorts
	in    compaction.Input
}

// newRig builds a Thread of one instruction, some Work and one Tool answer of obsBytes bytes
// (about obsBytes/4 tokens), with a context limit under which that does not fit.
func newRig(t testing.TB, obsBytes int, extraInstructions ...string) *rig {
	t.Helper()
	w := newWorld()
	big := view("file.read", "completed", nil, true, map[string]string{"text": strings.Repeat("log line ", obsBytes/9)})
	srcs := all(human(t, w, "ログを残しながら保存処理を実装する。"), one(w.work("保存処理に着手した。")),
		w.exchange([]modelport.ToolCall{call("c1", "file.read", `{"path":"log.txt"}`)}, []string{big}), one(w.work("ログを読んだ。次は書き込み。")))
	srcs = append(srcs, human(t, w, extraInstructions...)...)
	p := prepare(t, nil, srcs)
	ports := newPorts(t, w)
	ports.limit = 40000 // usable 35,904 tokens
	r := &rig{t: t, w: w, p: p, ports: ports}
	before, err := ports.CountAct(context.Background(), compaction.LiveProjection(p))
	if err != nil {
		t.Fatal(err)
	}
	r.in = compaction.Input{Prepared: p, Before: before}
	ports.actCounts = nil
	return r
}

func (r *rig) run() compaction.Outcome {
	return compaction.NewEngine(r.ports).Compact(context.Background(), r.in)
}

// goodSummary is a valid answer for the rig's Summary request.
func (r *rig) goodSummary() string {
	return summaryAnswer(r.t, []item{it("保存処理を実装中。ログを読んだ。", "work-0", "work-2")}, nil, nil, []item{it("書き込みの実装", "work-2")}, []item{it("書き込みを実装する", "work-2")}, "observation-0")
}

func (r *rig) selectionReply(text string) {
	r.ports.replies[modelport.StageSelection] = compaction.StageReply{Text: text}
}
func (r *rig) summaryReply(text string) {
	r.ports.replies[modelport.StageSummary] = compaction.StageReply{Text: text}
}

func noMoreThanTwoStages(t *testing.T, r *rig) {
	t.Helper()
	if r.ports.runs[modelport.StageSelection] > 1 || r.ports.runs[modelport.StageSummary] > 1 || r.ports.generations() > compaction.MaxStageRequests {
		t.Fatalf("stage requests: selection %d, summary %d", r.ports.runs[modelport.StageSelection], r.ports.runs[modelport.StageSummary])
	}
}

func TestNormalCompactionMakesOneSummaryRequestAndACheckpointThatFits(t *testing.T) {
	r := newRig(t, 200000)
	if r.in.Before.Verdict != contextplan.VerdictNoFit {
		t.Fatalf("the prompt must not fit to begin with: %v", r.in.Before.Verdict)
	}
	r.summaryReply(r.goodSummary())
	out := r.run()
	if out.Status != compaction.StatusExecuted || out.Result != compaction.OutcomeNormal || out.Candidate == nil {
		t.Fatalf("%+v", out)
	}
	if out.Stages.Selection != 0 || out.Stages.Summary != 1 || r.ports.generations() != 1 {
		t.Fatalf("a single instruction offers nothing to select: %+v", out.Stages)
	}
	c := out.Candidate.Candidate()
	if c.Mode != "normal" || *c.AfterCount.PromptUpper >= *c.BeforeCount.PromptLower || !strings.Contains(strings.Join(r.ports.records, ","), "compaction_summary_data") {
		t.Fatalf("mode %s, after %d, before %d, records %v", c.Mode, *c.AfterCount.PromptUpper, *c.BeforeCount.PromptLower, r.ports.records)
	}
	if out.After.EvidenceID == "" || out.Before.EvidenceID == "" || out.NormalFailure != "" || out.UnknownGeneration {
		t.Fatalf("%+v", out)
	}
	// The request the Summary stage was sent is the stage prompt and the data, nothing else.
	req := r.ports.stageReqs[0]
	if req.Stage != modelport.StageSummary || len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" || len(req.DatasetDigest) != 64 {
		t.Fatalf("%+v", req)
	}
	// The data and the Host's manifest of it were kept as private Evidence.
	if !strings.Contains(strings.Join(r.ports.records, ","), "compaction_summary_data_manifest") {
		t.Fatal("the manifest is kept")
	}
	noMoreThanTwoStages(t, r)
}

func TestSelectionRunsOnlyWhenThereIsSomethingToSelectAndOnce(t *testing.T) {
	r := newRig(t, 200000, "方式Aではなく方式Bで実装する。")
	r.selectionReply(opJSON(drop("presented-0", "ログを残しながら", "presented-1", "方式Bで実装する", "amendment")))
	r.summaryReply(r.goodSummary())
	out := r.run()
	if out.Result != compaction.OutcomeNormal || out.Stages.Selection != 1 || out.Stages.Summary != 1 || r.ports.generations() != 2 {
		t.Fatalf("%+v", out)
	}
	c := out.Candidate.Candidate()
	if len(c.AppliedSelection) != 1 || c.AppliedSelection[0].Basis != "amendment" {
		t.Fatalf("%+v", c.AppliedSelection)
	}
	// What the selection removed is not in the Summary request, and what is kept is exact.
	sum := r.ports.stageReqs[1]
	if strings.Contains(sum.Messages[1].Text(), "ログを残しながら") || !strings.Contains(sum.Messages[1].Text(), "保存処理を実装する。") {
		t.Fatal("the Summary data carries removed text, or loses kept text")
	}
	if got := c.Projection.Entries[0].Messages[0].Text(); got != "保存処理を実装する。" {
		t.Fatalf("%q", got)
	}
	noMoreThanTwoStages(t, r)
}

// TestAFailedStageGoesToEmergencyWithNoFurtherGeneration is A45 and H12: Selection at most
// once and Summary at most once; whatever fails, nothing is asked again and the reduction
// without a model takes over.
func TestAFailedStageGoesToEmergencyWithNoFurtherGeneration(t *testing.T) {
	t.Run("the Summary is a Tool call written as text", func(t *testing.T) {
		r := newRig(t, 200000)
		r.summaryReply(summaryAnswer(t, []item{it("<tool_call>{}</tool_call>", "work-0")}, nil, nil, nil, nil))
		out := r.run()
		if out.Result != compaction.OutcomeEmergency || out.Candidate == nil || r.ports.generations() != 1 || !strings.HasPrefix(out.NormalFailure, "semantic") && !strings.Contains(out.NormalFailure, "error") && out.NormalFailure == "" {
			t.Fatalf("%+v", out)
		}
		if out.Candidate.Candidate().Mode != "emergency" || out.Candidate.Candidate().SemanticBoundary != 0 || out.Candidate.Candidate().Projection.Summary != nil {
			t.Fatal("the Emergency checkpoint of a context with no Summary has none, and the boundary stays")
		}
		if len(out.Rejected) != 0 || !strings.Contains(strings.Join(r.ports.records, ","), "compaction_normal_failure") {
			t.Fatal("the reason the Normal compaction failed is recorded")
		}
		noMoreThanTwoStages(t, r)
	})
	t.Run("Selection fails and Summary is then not asked", func(t *testing.T) {
		r := newRig(t, 200000, "もう一つの指示")
		r.selectionReply("not json")
		r.summaryReply(r.goodSummary())
		out := r.run()
		if out.Result != compaction.OutcomeEmergency || out.Stages.Selection != 1 || out.Stages.Summary != 0 || r.ports.generations() != 1 {
			t.Fatalf("%+v", out)
		}
	})
	for _, code := range []string{"REASONING_ONLY", "EMPTY_FINAL_CONTENT", "RAW_TOOL_MARKUP", "MODEL_OUTPUT_SCHEMA_INVALID", "MODEL_OUTPUT_DEGENERATE", "LENGTH", "INCOMPLETE", "REFUSED",
		"CONNECT_FAILED", "UPSTREAM_TRANSIENT", "RATE_LIMITED", "QUEUE_TIMEOUT", "MODEL_UNAVAILABLE", "MODEL_CONTRACT_FAILED", "CONTEXT_LIMIT_EXCEEDED"} {
		t.Run("the Summary generation ends as "+code, func(t *testing.T) {
			r := newRig(t, 200000)
			r.ports.replies[modelport.StageSummary] = compaction.StageReply{Code: code, GenerationState: modelport.StateTerminal}
			out := r.run()
			if out.Status != compaction.StatusExecuted || out.Result != compaction.OutcomeEmergency || r.ports.runs[modelport.StageSummary] != 1 || r.ports.generations() != 1 {
				t.Fatalf("%+v (generations %d)", out, r.ports.generations())
			}
			if out.NormalFailure != "summary_failed_"+code {
				t.Fatalf("%s", out.NormalFailure)
			}
		})
	}
}

// TestAGenerationThatEndedUnknownIsRemembered: the reduction without a model still runs, and
// the caller is told that no new generation may follow until the unknown one is resolved.
func TestAGenerationThatEndedUnknownIsRemembered(t *testing.T) {
	r := newRig(t, 200000)
	r.ports.replies[modelport.StageSummary] = compaction.StageReply{Code: modelport.CodeOutcomeUnknown, GenerationState: modelport.StateUnknown}
	out := r.run()
	if out.Result != compaction.OutcomeEmergency || !out.UnknownGeneration || out.Candidate == nil {
		t.Fatalf("%+v", out)
	}
	// The same unknown with the Run being stopped is a cancellation that still says so.
	r = newRig(t, 200000)
	r.ports.replies[modelport.StageSummary] = compaction.StageReply{GenerationState: modelport.StateUnknown}
	r.ports.errs[modelport.StageSummary] = &compaction.StopError{Code: modelport.CodeCancelled}
	out = r.run()
	if out.Status != compaction.StatusCancelled || out.Code != "CANCELLED" || !out.UnknownGeneration || out.Candidate != nil || r.ports.generations() != 1 {
		t.Fatalf("%+v", out)
	}
}

func TestACompactionStoppedAnywhereIsCancelledAndMakesNothing(t *testing.T) {
	r := newRig(t, 200000)
	r.ports.countErr = &compaction.StopError{Code: "DEADLINE_EXCEEDED"}
	out := r.run()
	if out.Status != compaction.StatusCancelled || out.Code != "DEADLINE_EXCEEDED" || out.Candidate != nil || r.ports.generations() != 0 {
		t.Fatalf("%+v", out)
	}
	r = newRig(t, 200000)
	r.ports.replies[modelport.StageSummary] = compaction.StageReply{Code: modelport.CodeCancelled, GenerationState: modelport.StateNotStarted}
	if out := r.run(); out.Status != compaction.StatusCancelled || out.Code != "CANCELLED" || out.Candidate != nil {
		t.Fatalf("%+v", out)
	}
}

// TestWhatTheModelSideCannotServeIsUnavailableNotCapacity: no fallback hides a missing
// capability, and the code says which.
func TestWhatTheModelSideCannotServeIsUnavailableNotCapacity(t *testing.T) {
	for _, code := range []string{"BUDGET_UNVERIFIED", "UNSUPPORTED_CONTRACT", "UNSUPPORTED_RECOVERY_PROFILE", "BINDING_CHANGED", "AUTH_FAILED", "INPUT_DIGEST_MISMATCH", "REQUEST_DIGEST_MISMATCH"} {
		t.Run(code, func(t *testing.T) {
			r := newRig(t, 200000)
			r.ports.replies[modelport.StageSummary] = compaction.StageReply{Code: code, GenerationState: modelport.StateNotStarted}
			out := r.run()
			if out.Status != compaction.StatusUnavailable || out.Code != code || out.Candidate != nil || out.Result != "" {
				t.Fatalf("%+v", out)
			}
		})
	}
	t.Run("a port that says so", func(t *testing.T) {
		r := newRig(t, 200000)
		r.ports.countErr = &compaction.UnavailableError{Code: "UNSUPPORTED_CONTRACT"}
		if out := r.run(); out.Status != compaction.StatusUnavailable || out.Code != "UNSUPPORTED_CONTRACT" {
			t.Fatalf("%+v", out)
		}
	})
}

// TestACountThatCannotBeVerifiedIsUnavailable: no estimate is taken for a fit or a no-fit
// anywhere in the flow.
func TestACountThatCannotBeVerifiedIsUnavailable(t *testing.T) {
	r := newRig(t, 200000)
	r.in.Before.Result.State, r.in.Before.Verdict = "estimated", contextplan.VerdictUnverified
	if out := r.run(); out.Status != compaction.StatusUnavailable || out.Code != "BUDGET_UNVERIFIED" || r.ports.generations() != 0 {
		t.Fatalf("an unverified before: %+v", out)
	}
	r = newRig(t, 200000)
	r.ports.state = "unverified" // every count from here on cannot be verified
	if out := r.run(); out.Status != compaction.StatusUnavailable || out.Code != "BUDGET_UNVERIFIED" || r.ports.generations() != 0 {
		t.Fatalf("an unverified preflight: %+v", out)
	}
}

// TestThePreflightBlocksAtOnceWhenTheSmallestCandidateDoesNotFit is A24: the verified smallest
// Normal candidate over the budget is CapacityBlocked with no Summary generation and no
// Emergency, and what Selection already ran is counted apart.
func TestThePreflightBlocksAtOnceWhenTheSmallestCandidateDoesNotFit(t *testing.T) {
	t.Run("one instruction too large to ever fit", func(t *testing.T) {
		r := newRig(t, 200000, strings.Repeat("x", 150000))
		r.ports.stageSize[modelport.StageSelection] = 1000 // the Selection request is small enough to be sent
		r.selectionReply(`{"operations":[]}`)
		r.summaryReply(r.goodSummary())
		out := r.run()
		if out.Status != compaction.StatusExecuted || out.Result != compaction.OutcomeCapacity || out.Candidate != nil {
			t.Fatalf("%+v", out)
		}
		if r.ports.generations() != 1 || r.ports.runs[modelport.StageSelection] != 1 || r.ports.runs[modelport.StageSummary] != 0 || out.Stages.Summary != 0 || out.Stages.Selection != 1 {
			t.Fatalf("Summary generations %d, all %d", r.ports.runs[modelport.StageSummary], r.ports.generations())
		}
		if out.Required == nil || out.Available == nil || *out.Required <= *out.Available || *out.Available != 40000-4096 {
			t.Fatalf("required %v available %v", out.Required, out.Available)
		}
		if len(r.ports.actCounts) != 1 {
			t.Fatalf("only the preflight was counted, not an Emergency candidate: %v", r.ports.actCounts)
		}
	})
	t.Run("with nothing to select, no generation at all", func(t *testing.T) {
		r := newRig(t, 200000)
		r.ports.limit = 4200 // usable 104 tokens: not even the system prompt fits
		out := r.run()
		if out.Result != compaction.OutcomeCapacity || r.ports.generations() != 0 || out.Candidate != nil {
			t.Fatalf("%+v", out)
		}
	})
}

// TestASummaryRequestThatDoesNotFitByTokensIsCapacityBlocked is IMPLEMENTATION_SPEC section 8 and
// 10.5: the Summary request is counted apart from the candidate, and one that does not fit is
// never sent; no document defines a legal projection to rebuild it as, so there is none, and
// the result is CapacityBlocked with the shortfall: no Summary generation and no reduction
// without a model (its candidate is not even counted).
func TestASummaryRequestThatDoesNotFitByTokensIsCapacityBlocked(t *testing.T) {
	r := newRig(t, 200000)
	r.ports.stageSize[modelport.StageSummary] = 100000 // the Summary request alone is over the budget
	r.summaryReply(r.goodSummary())
	out := r.run()
	if out.Status != compaction.StatusExecuted || out.Result != compaction.OutcomeCapacity || out.Candidate != nil || r.ports.generations() != 0 || out.NormalFailure != "" {
		t.Fatalf("%+v", out)
	}
	if out.Required == nil || out.Available == nil || *out.Required != 100000 || *out.Available != 40000-4096 || len(r.ports.actCounts) != 1 {
		t.Fatalf("required %v available %v; act counts %v (only the Preflight)", out.Required, out.Available, r.ports.actCounts)
	}
	// With a Selection that ran first, that one generation is counted apart.
	w := newRig(t, 200000, "もう一つの指示")
	w.ports.stageSize[modelport.StageSelection] = 1000
	w.ports.stageSize[modelport.StageSummary] = 100000
	w.selectionReply(`{"operations":[]}`)
	w.summaryReply(w.goodSummary())
	out = w.run()
	if out.Result != compaction.OutcomeCapacity || out.Stages.Selection != 1 || out.Stages.Summary != 0 || w.ports.generations() != 1 || len(w.ports.actCounts) != 1 {
		t.Fatalf("%+v", out)
	}
}

// TestASelectionRequestThatDoesNotFitByTokensIsANormalFailure: only the Summary request has a
// capacity answer in the documents; a Selection that cannot be sent is a Normal that cannot be
// made, and the reduction without a model (which needs no selection) takes over, with no
// generation.
func TestASelectionRequestThatDoesNotFitByTokensIsANormalFailure(t *testing.T) {
	r := newRig(t, 200000, "もう一つの指示")
	r.ports.stageSize[modelport.StageSelection] = 100000
	r.selectionReply(`{"operations":[]}`)
	r.summaryReply(r.goodSummary())
	out := r.run()
	if out.Result != compaction.OutcomeEmergency || out.NormalFailure != "selection_request_does_not_fit" || r.ports.generations() != 0 {
		t.Fatalf("%+v", out)
	}
}

// TestADatasetOverItsBoundsIsANormalFailureAndNotACapacityBlock is STAGE_DATA section 1: the
// bytes, depth and list lengths of the data are bounds of the data, not of the context. Data
// that breaks one is not cut and treated as done, and the Normal compaction is not used: the
// reduction without a model takes over. (The token shortage of a request that is within its
// bounds is the other test.)
func TestADatasetOverItsBoundsIsANormalFailureAndNotACapacityBlock(t *testing.T) {
	big := func(w *world) string {
		return view("file.read", "completed", nil, true, map[string]string{"text": strings.Repeat("log line ", 30000)})
	}
	for name, mk := range map[string]func(w *world) []compaction.Source{
		"more pieces of Work than a list may hold": func(w *world) []compaction.Source {
			srcs := append(human(t, w, "go"), w.exchange([]modelport.ToolCall{call("c1", "file.read", `{}`)}, []string{big(w)})...)
			for i := 0; i < 8200; i++ {
				srcs = append(srcs, w.work("x"))
			}
			return srcs
		},
		"more bytes than the data may hold": func(w *world) []compaction.Source {
			srcs := append(human(t, w, "go"), w.exchange([]modelport.ToolCall{call("c1", "file.read", `{}`)}, []string{big(w)})...)
			return append(srcs, w.work(strings.Repeat("a", compaction.MaxDatasetBytes+1000)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			p := prepare(t, nil, mk(w))
			ports := newPorts(t, w)
			ports.limit = 40000
			before, _ := ports.CountAct(context.Background(), compaction.LiveProjection(p))
			if before.Verdict != contextplan.VerdictNoFit {
				t.Fatalf("the context must not fit to begin with: %v", before.Verdict)
			}
			out := compaction.NewEngine(ports).Compact(context.Background(), compaction.Input{Prepared: p, Before: before})
			if out.Result == compaction.OutcomeCapacity && out.Reason == "the Summary request does not fit" {
				t.Fatal("a dataset over its bounds was taken for a shortage of capacity")
			}
			if ports.generations() != 0 || out.NormalFailure == "" {
				t.Fatalf("generations %d, %+v", ports.generations(), out)
			}
		})
	}
}

// TestACandidateThatIsNotSmallerFallsBackToEmergency is H07: a Summary that makes the prompt
// no smaller is not a compaction, however it looks.
func TestACandidateThatIsNotSmallerFallsBackToEmergency(t *testing.T) {
	r := newRig(t, 120000)
	huge := strings.Repeat("あ", 7000)
	var cur []item
	for i := 0; i < 40; i++ {
		cur = append(cur, it(huge, "work-0"))
	}
	r.summaryReply(summaryAnswer(t, cur, nil, nil, nil, nil))
	out := r.run()
	if out.Result != compaction.OutcomeEmergency || !strings.HasPrefix(out.NormalFailure, "candidate_") {
		t.Fatalf("%+v", out)
	}
	if after := *out.Candidate.Candidate().AfterCount.PromptUpper; after >= *r.in.Before.Result.PromptLower {
		t.Fatalf("the Emergency candidate is smaller: %d", after)
	}
}

// TestEmergencyIsBlockedWhenItCannotShowASmallerPrompt is A62 and H19: bytes saved are not a
// reduction unless the whole candidate's count is smaller, and a candidate that is not is
// CapacityBlocked, not a success.
func TestEmergencyIsBlockedWhenItCannotShowASmallerPrompt(t *testing.T) {
	t.Run("nothing to replace", func(t *testing.T) {
		w := newWorld()
		srcs := all(human(t, w, strings.Repeat("あ", 30000)), one(w.work(strings.Repeat("い", 30000))))
		p := prepare(t, nil, srcs)
		ports := newPorts(t, w)
		ports.limit = 20000
		before, _ := ports.CountAct(context.Background(), compaction.LiveProjection(p))
		ports.replies[modelport.StageSummary] = compaction.StageReply{Code: "REFUSED", GenerationState: modelport.StateTerminal}
		out := compaction.NewEngine(ports).Compact(context.Background(), compaction.Input{Prepared: p, Before: before})
		if out.Result != compaction.OutcomeCapacity || out.Candidate != nil {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("fewer bytes and not fewer tokens", func(t *testing.T) {
		r := newRig(t, 200000)
		r.ports.replies[modelport.StageSummary] = compaction.StageReply{Code: "REFUSED", GenerationState: modelport.StateTerminal}
		r.ports.afterOverride = func(p contextplan.Projection) *int64 {
			if p.Summary == nil && hasMarker(p) {
				n := int64(60000) // the reduced prompt counts higher than the original did
				return &n
			}
			return nil
		}
		out := r.run()
		if out.Result != compaction.OutcomeCapacity || out.Candidate != nil || out.Required == nil || *out.Required <= *out.Available {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("smaller but still too large", func(t *testing.T) {
		r := newRig(t, 200000)
		r.ports.replies[modelport.StageSummary] = compaction.StageReply{Code: "REFUSED", GenerationState: modelport.StateTerminal}
		r.ports.afterOverride = func(p contextplan.Projection) *int64 {
			if p.Summary == nil && hasMarker(p) {
				n := int64(39000)
				return &n
			}
			return nil
		}
		if out := r.run(); out.Result != compaction.OutcomeCapacity {
			t.Fatalf("%+v", out)
		}
	})
}

// TestEmergencyKeepsTheLastSummaryAndAnchorAcrossRepeatedFailures is H19: Normal fails again
// and again, the semantic boundary and the Summary never move, the new Work stays, and the
// older excerpts become references.
func TestEmergencyKeepsTheLastSummaryAndAnchorAcrossRepeatedFailures(t *testing.T) {
	prior := loadGoldenCheckpoint(t)
	w := newWorld()
	w.seq, w.item = 5, 5
	big := view("file.read", "completed", nil, true, map[string]string{"text": strings.Repeat("log line ", 30000)})
	p := prepare(t, prior, w.exchange([]modelport.ToolCall{call("c9", "file.read", `{}`)}, []string{big}), one(w.work("さらに進めた。")))
	ports := newPorts(t, w)
	ports.limit = 40000
	log := decodeWire[contextplan.Projection](t, "projection_input.json").Entries[1].Messages[1].Text()
	ports.w.texts["evd_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0004"] = log
	before, _ := ports.CountAct(context.Background(), compaction.LiveProjection(p))
	if before.Verdict != contextplan.VerdictNoFit {
		t.Fatalf("%v", before.Verdict)
	}
	ports.replies[modelport.StageSummary] = compaction.StageReply{Code: "MODEL_OUTPUT_SCHEMA_INVALID", GenerationState: modelport.StateTerminal}
	out := compaction.NewEngine(ports).Compact(context.Background(), compaction.Input{Prepared: p, Before: before})
	if out.Result != compaction.OutcomeEmergency {
		t.Fatalf("%+v", out)
	}
	c := out.Candidate.Candidate()
	if c.SemanticBoundary != 2 || c.DurableBoundary != p.DurableBoundary || *c.Projection.SummaryAnchorSequence != 2 || c.Projection.Summary.SourceCheckpointID != prior.Candidate.Projection.Summary.SourceCheckpointID {
		t.Fatalf("semantic %d durable %d", c.SemanticBoundary, c.DurableBoundary)
	}
	if c.ParentCheckpointID == nil || *c.ParentCheckpointID != prior.Candidate.CheckpointID {
		t.Fatal("the parent is the checkpoint it was made from")
	}
	if len(c.Projection.Entries) != len(p.Units) {
		t.Fatalf("%d entries for %d units: nothing is dropped", len(c.Projection.Entries), len(p.Units))
	}
	// The new answer became a reference, and the unsummarized Work after the anchor stays.
	last := c.Projection.Entries[3]
	if !compaction.IsMarker(last.Messages[1].Text()) || c.Projection.Entries[4].Messages[0].Text() != "さらに進めた。" {
		t.Fatal("the new answer is a reference and the new Work is kept")
	}
	// Two checkpoints in a row: the candidate stands on the first one as it is.
	v2, err := compaction.ParseCheckpoint(out.Candidate.Bytes(), out.Candidate.Hash())
	if err != nil {
		t.Fatal(err)
	}
	p2, err := compaction.Prepare(snapshotOf(p, v2))
	if err != nil || p2.SemanticBoundary != 2 {
		t.Fatalf("%v", err)
	}
}

func snapshotOf(p *compaction.Prepared, prior *compaction.Checkpoint) compaction.Snapshot {
	s := p.Snapshot
	s.Prior, s.Tail = prior, nil
	s.ThreadID, s.TaskID, s.RunID = prior.Candidate.ThreadID, p.Snapshot.TaskID, p.Snapshot.RunID
	return s
}

func TestAContradictionInTheStoredRecordsIsIntegrityBlocked(t *testing.T) {
	r := newRig(t, 200000)
	r.ports.w = newWorld() // the reader no longer has the originals
	// An answer replaced by a reference needs its original; take the live answer for a marker.
	em, err := compaction.Emergency(r.p)
	if err != nil {
		t.Fatal(err)
	}
	r.p.Units = em.Units
	r.summaryReply(r.goodSummary())
	out := r.run()
	if out.Status != compaction.StatusExecuted || out.Result != compaction.OutcomeIntegrity || out.Candidate != nil || r.ports.generations() != 0 {
		t.Fatalf("%+v", out)
	}
	_ = errors.New
}

func hasMarker(p contextplan.Projection) bool {
	for _, e := range p.Entries {
		for _, m := range e.Messages {
			if m.Role == "tool" && compaction.IsMarker(m.Text()) {
				return true
			}
		}
	}
	return false
}

// TestStageDataThatBreaksItsBoundsIsNeverSent is H02: a dataset that does not satisfy its
// schema or its bounds is refused before anything is sent, and nothing of it is cut and
// treated as done: the Normal compaction does not happen and the reduction takes over.
func TestStageDataThatBreaksItsBoundsIsNeverSent(t *testing.T) {
	w := newWorld()
	big := view("file.read", "completed", nil, true, map[string]string{"text": strings.Repeat("log line ", 30000)})
	var srcs []compaction.Source
	for i := 0; i < 8200; i++ {
		srcs = append(srcs, w.human("x"))
	}
	srcs = append(srcs, w.exchange([]modelport.ToolCall{call("c1", "file.read", `{}`)}, []string{big})...)
	srcs = append(srcs, w.work("done"))
	p := prepare(t, nil, srcs)
	ports := newPorts(t, w)
	ports.limit = 40000
	before, _ := ports.CountAct(context.Background(), compaction.LiveProjection(p))
	if before.Verdict != contextplan.VerdictNoFit {
		t.Fatalf("%v", before.Verdict)
	}
	out := compaction.NewEngine(ports).Compact(context.Background(), compaction.Input{Prepared: p, Before: before})
	if out.Result != compaction.OutcomeEmergency || ports.generations() != 0 || !strings.HasPrefix(out.NormalFailure, "semantic") && out.NormalFailure == "" {
		t.Fatalf("%+v (generations %d)", out, ports.generations())
	}
}

// TestACheckpointStandsOnItsParentAndAFurtherCompactionInheritsWhatItKept is H14 and H03: the
// second compaction starts from the first one's entries and Summary (given to the Summary
// stage once, as the previous Summary), a revocation made by the first stays made, the
// semantic boundary only moves forward, and the parent of the second is the first.
func TestACheckpointStandsOnItsParentAndAFurtherCompactionInheritsWhatItKept(t *testing.T) {
	r := newRig(t, 200000, "方式Aで実装する。", "方式Aはやめる。")
	r.selectionReply(opJSON(drop("presented-1", "方式Aで", "presented-2", "方式Aはやめる", "revocation")))
	r.summaryReply(r.goodSummary())
	first := r.run()
	if first.Result != compaction.OutcomeNormal || len(first.Candidate.Candidate().AppliedSelection) != 1 {
		t.Fatalf("%+v", first)
	}
	cp, err := compaction.ParseCheckpoint(first.Candidate.Bytes(), first.Candidate.Hash())
	if err != nil {
		t.Fatal(err)
	}
	// More happens after the checkpoint: a new instruction and another big answer.
	w := r.w
	big := view("file.read", "completed", nil, true, map[string]string{"text": strings.Repeat("second ", 40000)})
	tail := all(one(w.human("ログも残す。")), w.exchange([]modelport.ToolCall{call("c2", "file.read", `{}`)}, []string{big}), one(w.work("2つ目を読んだ。")))
	snap := snapshot(cp, tail)
	snap.ThreadID = cp.Candidate.ThreadID
	snap.ContextRevision = cp.Candidate.Expected.ContextRevision + 1
	p, err := compaction.Prepare(snap)
	if err != nil {
		t.Fatal(err)
	}
	if p.SemanticBoundary != first.Candidate.Candidate().SemanticBoundary || p.Summary == nil || p.Summary.SourceCheckpointID != cp.Candidate.CheckpointID {
		t.Fatalf("boundary %d summary %+v", p.SemanticBoundary, p.Summary)
	}
	for _, u := range p.Units {
		for _, g := range u.Segments {
			if strings.Contains(g.Text, "方式Aで") {
				t.Fatal("a removed instruction is back")
			}
		}
	}
	ports2 := newPorts(t, w)
	ports2.limit = 40000
	ports2.nextID = "ckp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b8888"
	before, _ := ports2.CountAct(context.Background(), compaction.LiveProjection(p))
	if before.Verdict != contextplan.VerdictNoFit {
		t.Fatalf("the second context must not fit: %v", before.Verdict)
	}
	ports2.replies[modelport.StageSelection] = compaction.StageReply{Text: `{"operations":[]}`}
	ports2.replies[modelport.StageSummary] = compaction.StageReply{Text: summaryAnswer(t, []item{it("続き", "work-1", "summary-0")}, nil, nil, []item{it("残り", "work-1")}, nil, "observation-0")}
	out := compaction.NewEngine(ports2).Compact(context.Background(), compaction.Input{Prepared: p, Before: before})
	if out.Result != compaction.OutcomeNormal || out.Stages.Summary != 1 {
		t.Fatalf("%+v", out)
	}
	c := out.Candidate.Candidate()
	if c.ParentCheckpointID == nil || *c.ParentCheckpointID != cp.Candidate.CheckpointID || c.SemanticBoundary <= first.Candidate.Candidate().SemanticBoundary || c.Projection.Summary.SourceCheckpointID != c.CheckpointID {
		t.Fatalf("parent %v boundary %d", c.ParentCheckpointID, c.SemanticBoundary)
	}
	// The previous Summary reached the stage once, as text, and is a source by the checkpoint that accepted it.
	req := ports2.stageReqs[len(ports2.stageReqs)-1]
	if req.Stage != modelport.StageSummary || strings.Count(req.Messages[1].Text(), `"summary-0"`) != 1 {
		t.Fatal("the previous Summary is given once")
	}
	var found bool
	for _, m := range c.Projection.Summary.SourceMap {
		if m.Handle == "summary-0" {
			found = m.Sources[0].SourceID == cp.Candidate.CheckpointID && m.Sources[0].ProjectionVersion == "checkpoint/v1" && m.Sources[0].RawHash == cp.Hash
		}
	}
	if !found {
		t.Fatal("summary-0 stands for the checkpoint that accepted it, by its hash")
	}
	// The instructions: those of the first checkpoint's entries, none removed text, and the new one.
	var texts []string
	for _, e := range c.Projection.Entries {
		if e.Kind == "instruction" {
			texts = append(texts, e.Messages[0].Text())
		}
	}
	// The first instruction, what is left of the revoked one, the revocation and the new one.
	if len(texts) != 4 || texts[1] != "実装する。" || texts[2] != "方式Aはやめる。" || texts[3] != "ログも残す。" || strings.Contains(strings.Join(texts, "|"), "方式Aで") {
		t.Fatalf("%q", texts)
	}
}

func TestACheckpointIsHeldToTheRowItWasStoredIn(t *testing.T) {
	cp := loadGoldenCheckpoint(t)
	x := cp.Candidate
	good := compaction.StoredFacts{CheckpointID: x.CheckpointID, ThreadID: x.ThreadID, RunID: x.RunID, ParentCheckpointID: x.ParentCheckpointID, Mode: x.Mode,
		ContextRevision: x.Expected.ContextRevision + 1, SemanticBoundary: x.SemanticBoundary, DurableBoundary: x.DurableBoundary, Hash: cp.Hash}
	if err := cp.CheckStored(good); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(f *compaction.StoredFacts){
		"id":       func(f *compaction.StoredFacts) { f.CheckpointID = "ckp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b00ff" },
		"thread":   func(f *compaction.StoredFacts) { f.ThreadID = "thr_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b00ff" },
		"run":      func(f *compaction.StoredFacts) { f.RunID = "run_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b00ff" },
		"parent":   func(f *compaction.StoredFacts) { f.ParentCheckpointID = nil },
		"mode":     func(f *compaction.StoredFacts) { f.Mode = "normal" },
		"revision": func(f *compaction.StoredFacts) { f.ContextRevision = x.Expected.ContextRevision }, // the before value is not the row's
		"semantic": func(f *compaction.StoredFacts) { f.SemanticBoundary++ },
		"durable":  func(f *compaction.StoredFacts) { f.DurableBoundary++ },
		"hash":     func(f *compaction.StoredFacts) { f.Hash = strings.Repeat("0", 64) },
	} {
		f := good
		mutate(&f)
		if err := cp.CheckStored(f); !errors.Is(err, compaction.ErrIntegrity) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestAnAnswerWhoseCaptureWasIncompleteIsReferencedAsIncomplete is A39: the reference says
// so, no completion link is offered from it, and nothing is made up for what was not
// captured.
func TestAnAnswerWhoseCaptureWasIncompleteIsReferencedAsIncomplete(t *testing.T) {
	w := newWorld()
	ins := w.human("`go test ./...` を確認する")
	xs := execExchange(w, "go test ./...", execViewWith(execOpts{exit: 0, stdout: "ok", partial: true}))
	p := prepare(t, nil, one(ins), xs)
	ref := p.Units[1].Entry.Observations[0].Reference
	if ref.CaptureComplete {
		t.Fatal("the view says the capture was partial")
	}
	sel, err := compaction.BuildSelectionDataset(p)
	if err != nil || len(sel.Links) != 0 {
		t.Fatalf("%d links: a partial capture is never the full evidence of a run (%v)", len(sel.Links), err)
	}
	m, err := compaction.Marker(ref)
	if err != nil || !strings.Contains(m, `"capture_complete":false`) {
		t.Fatalf("%s %v", m, err)
	}
}
