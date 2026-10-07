package service_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/config"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolview"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// A compaction scenario: a Run reads a large file through a Tool, so that the next prompt
// does not fit, and the model is the fake with counts that follow the size of the prompt.
//
// The count is the size of the messages divided by four, so a prompt that carries 60,000 bytes
// of a file is about 15,000 tokens, and the Summary request that carries only its two edges
// is about a tenth of that. The context limit is chosen so that usable = 8,000 tokens: the
// output reserve (4,096) and the safety margin of the design config (2,048) are what is left
// of the limit after it.
const (
	bigFileBytes  = 60000
	usableTokens  = 8000
	contextLimit  = usableTokens + 4096 + 2048
	summaryHandle = "work-0"
)

func bigFile() string { return strings.Repeat("0123456789abcdef\n", bigFileBytes/17) }

type compactionRig struct {
	*rig
	fake *harnesstest.Fake
	rec  *recorder
	// stages are the requests the fake received for each compaction stage.
	stages map[string][]modelport.ChatRequest
	// acts are the act requests it received.
	acts []modelport.ChatRequest
}

// goodSummary is a Summary answer for the request of the scenario: it cites the Work (the
// record of the call), the Observation (the file) and the instruction.
const goodSummary = `{"current_work":[{"text":"大きなファイルを読んだ。","source_handles":["work-0","observation-0"]}],"decisions":[],"verification":[],"open_items":[{"text":"続きを実装する","source_handles":["instruction-0"]}],"next_steps":[{"text":"保存処理を書く","source_handles":["work-0"]}],"important_observation_handles":["observation-0"]}`

// newCompactionRig is a rig whose fake model reads the big file on its first act step, answers
// any stage with the given replies, and gives a final answer once the prompt has been dealt
// with. edit may change the fake before the Run starts.
func newCompactionRig(t *testing.T, stageReplies map[string]harnesstest.Reply, limit int64) *compactionRig {
	t.Helper()
	return newCompactionRigOn(t, stageReplies, limit, defaultRigBuild)
}

// rigBuild makes the rig a scenario runs on, over the fake model: the fixture policy with all
// the Tools (defaultRigBuild), or one with host hooks configured (hookRigBuild).
type rigBuild func(t *testing.T, fake *harnesstest.Fake) (*rig, *recorder)

func defaultRigBuild(t *testing.T, fake *harnesstest.Fake) (*rig, *recorder) {
	t.Helper()
	return toolRig(t, fake, toolConfig{})
}

// newCompactionRigOn is newCompactionRig on the rig that build makes.
func newCompactionRigOn(t *testing.T, stageReplies map[string]harnesstest.Reply, limit int64, build rigBuild) *compactionRig {
	t.Helper()
	fake := harnesstest.NewFake()
	fake.SetMeasure(harnesstest.MeasureConfig{Limit: limit})
	c := &compactionRig{fake: fake, stages: map[string][]modelport.ChatRequest{}}
	c.install(stageReplies)
	r, rec := build(t, fake)
	r.write("big.txt", bigFile())
	c.rig, c.rec = r, rec
	return c
}

// newReadOnlyCompactionRig is newCompactionRig for a policy that offers file.read and
// evidence.read only: a Run that cannot change the workspace takes no workspace lock, which a
// test that stands a second "process" up in the same OS process needs.
func newReadOnlyCompactionRig(t *testing.T, stageReplies map[string]harnesstest.Reply, limit int64) *compactionRig {
	t.Helper()
	fake := harnesstest.NewFake()
	fake.SetMeasure(harnesstest.MeasureConfig{Limit: limit})
	c := &compactionRig{fake: fake, stages: map[string][]modelport.ChatRequest{}}
	c.install(stageReplies)
	r, rec := toolRig(t, fake, toolConfig{tools: []string{"file.read", "evidence.read"}})
	r.write("big.txt", bigFile())
	c.rig, c.rec = r, rec
	return c
}

func (c *compactionRig) run(key, text string) (protocol.StartResult, protocol.RunInfo) {
	c.t.Helper()
	info := c.openSession(key + ".open")
	return c.runOn(info.ThreadID, key, text)
}

func (c *compactionRig) runOn(thread, key, text string) (protocol.StartResult, protocol.RunInfo) {
	c.t.Helper()
	start := c.startRun(thread, key, text)
	return start, c.waitTerminal(start.RunID)
}

func (r *rig) checkpoints() []string {
	r.t.Helper()
	v := r.val("SELECT group_concat(x, '|') FROM (SELECT checkpoint_id||'/'||mode||'/'||semantic_boundary||'/'||durable_boundary||'/'||context_revision AS x FROM checkpoints ORDER BY rowid)")
	if v == "" {
		return nil
	}
	return strings.Split(v, "|")
}

func eventTypesOf(evs []protocol.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

// TestAPromptThatDoesNotFitIsCompactedAndTheRunGoesOn is the whole of an automatic Normal
// compaction through a real Run, store and driver: the Tool answer makes the next prompt too
// large, a Summary request carries its edges, the Summary is validated and stored as a
// checkpoint in one commit, and the Run, prompted again from the checkpoint, completes.
func TestAPromptThatDoesNotFitIsCompactedAndTheRunGoesOn(t *testing.T) {
	c := newCompactionRig(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}, contextLimit)
	start, run := c.run("compact.normal.0001", "大きなファイルを読んで保存処理を実装する。")
	res := run.Result
	if res.Status != "completed" || res.FinalText != "compaction handled, the work goes on" {
		t.Fatalf("%+v", res)
	}
	cps := c.checkpoints()
	if len(cps) != 1 || !strings.Contains(cps[0], "/normal/") {
		t.Fatalf("checkpoints %v", cps)
	}
	if res.LastCheckpointID == nil || !strings.HasPrefix(cps[0], *res.LastCheckpointID+"/") {
		t.Fatalf("the result names the checkpoint the Run committed: %v", res.LastCheckpointID)
	}
	if c.val("SELECT current_checkpoint_id FROM threads") != *res.LastCheckpointID {
		t.Fatal("the Thread's pointer is the new checkpoint")
	}
	// What was asked: act, the Tool, the stage, then act again from the checkpoint; the stage
	// counts as a generation attempt and as no model step.
	if len(c.stages[modelport.StageSummary]) != 1 || len(c.stages[modelport.StageSelection]) != 0 || len(c.acts) != 2 {
		t.Fatalf("summary %d, selection %d, acts %d", len(c.stages[modelport.StageSummary]), len(c.stages[modelport.StageSelection]), len(c.acts))
	}
	if got := c.val("SELECT generation_attempts_used FROM runs WHERE run_id='" + start.RunID + "'"); got != "3" {
		t.Fatalf("attempts used %s: act, summary, act", got)
	}
	if got := c.val("SELECT COUNT(*) FROM actions WHERE run_id='" + start.RunID + "' AND name='act'"); got != "2" {
		t.Fatalf("model steps %s", got)
	}
	// The Summary request is the stage prompt and the stage data only, with no Tool.
	sr := c.stages[modelport.StageSummary][0]
	if len(sr.Messages) != 2 || sr.ToolChoice != "none" || len(sr.Tools) != 0 || sr.Stream || sr.ResponseFormat.Type != "json_object" || sr.Rencrow.Harness.Recovery.ProfileID != "same_request" {
		t.Fatalf("%+v", sr)
	}
	if !strings.HasPrefix(sr.Messages[1].Text(), "RENCROW_STAGE_DATA_V1\n") || strings.Contains(sr.Messages[1].Text(), strings.Repeat("0123456789abcdef\\n", 200)) {
		t.Fatal("the Summary data carries the edges of the file, not the file")
	}
	// The events of the Run, in order: the stage is an Action of its own, then the commit.
	types := eventTypesOf(c.threadEvents(c.val("SELECT thread_id FROM threads")))
	joined := strings.Join(types, ",")
	if !strings.Contains(joined, "model.requested,model.completed") || strings.Count(joined, "checkpoint.committed") != 1 {
		t.Fatalf("%s", joined)
	}
	// The act request after the checkpoint is built by the same function from the projection:
	// no Tool message with the file, the boundary and the Summary, the exact instruction.
	last := c.acts[len(c.acts)-1]
	var texts []string
	for _, m := range last.Messages {
		texts = append(texts, m.Role+":"+m.Text())
		if m.Role == "tool" && len(m.Text()) > 10000 {
			t.Fatal("the file is still in the prompt")
		}
	}
	all := strings.Join(texts, "\n")
	if !strings.Contains(all, "user:RENCROW_CONTEXT_BOUNDARY_V1") || !strings.Contains(all, "assistant:RENCROW_ACCEPTED_SUMMARY_V1") || !strings.Contains(all, `"text":"大きなファイルを読んで保存処理を実装する。"}`) ||
		!strings.Contains(all, "user:RENCROW_OBSERVATION_REFERENCE_V1") {
		t.Fatalf("the post-compaction prompt is not the projection:\n%.2000s", all)
	}
	// The checkpoint's bytes are the exact stored form and its hash is over them.
	row := c.val("SELECT candidate_hash FROM checkpoints")
	if len(row) != 64 {
		t.Fatal(row)
	}
	_ = json.Marshal
}

// exec runs a statement on the store as some other writer would (a test's way of changing
// what a driver relied on).
func (r *rig) exec(query string, args ...any) {
	r.t.Helper()
	db, err := sql.Open("sqlite", "file:"+r.dep.DataRoot+"/"+sqlite.DatabaseFile+"?_pragma=busy_timeout(5000)")
	if err != nil {
		r.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query, args...); err != nil {
		r.t.Fatalf("%s: %v", query, err)
	}
}

func (r *rig) purposes(runID string) string {
	r.t.Helper()
	return r.val("SELECT group_concat(p, ',') FROM (SELECT json_extract(metadata_json,'$.purpose') AS p FROM evidence WHERE run_id=? ORDER BY rowid)", runID)
}

func lastAct(c *compactionRig) modelport.ChatRequest { return c.acts[len(c.acts)-1] }

func promptOf(req modelport.ChatRequest) string {
	var texts []string
	for _, m := range req.Messages {
		texts = append(texts, m.Role+":"+m.Text())
	}
	return strings.Join(texts, "\n")
}

// TestAFailedSummaryGoesToTheEmergencyReductionAndTheRunGoesOn is H01 and H12 through a Run:
// the Summary carries a Tool call as text, nothing is asked again, the answer becomes a
// reference, the checkpoint says Emergency and keeps the semantic boundary where it was, and
// why the Normal compaction was not used is on record.
func TestAFailedSummaryGoesToTheEmergencyReductionAndTheRunGoesOn(t *testing.T) {
	bad := strings.Replace(goodSummary, "大きなファイルを読んだ。", "<tool_call>{\"name\":\"file.read\"}</tool_call>", 1)
	c := newCompactionRig(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(bad)}, contextLimit)
	start, run := c.run("compact.emergency.001", "大きなファイルを読んで保存処理を実装する。")
	res := run.Result
	if res.Status != "completed" || res.FinalText != "compaction handled, the work goes on" {
		t.Fatalf("%+v", res)
	}
	cps := c.checkpoints()
	if len(cps) != 1 || !strings.Contains(cps[0], "/emergency/0/") || res.LastCheckpointID == nil {
		t.Fatalf("checkpoints %v", cps)
	}
	if len(c.stages[modelport.StageSummary]) != 1 || len(c.acts) != 2 {
		t.Fatalf("the Summary was asked %d times, %d act steps", len(c.stages[modelport.StageSummary]), len(c.acts))
	}
	// The answer is now a reference to the stored Evidence, which can still be read in full.
	prompt := promptOf(lastAct(c))
	if !strings.Contains(prompt, "tool:RENCROW_OBSERVATION_REFERENCE_V1") || strings.Contains(prompt, "0123456789abcdef") || strings.Contains(prompt, "assistant:RENCROW_ACCEPTED_SUMMARY_V1") {
		t.Fatalf("the Emergency prompt keeps no summary and replaces the answer:\n%.1500s", prompt)
	}
	var marker struct {
		EvidenceID string `json:"evidence_id"`
		TotalBytes int64  `json:"total_bytes"`
		Partial    bool   `json:"partial"`
	}
	for _, m := range lastAct(c).Messages {
		if m.Role == "tool" {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(m.Text(), "RENCROW_OBSERVATION_REFERENCE_V1\n")), &marker); err != nil {
				t.Fatal(err)
			}
		}
	}
	if marker.EvidenceID == "" || !marker.Partial || marker.TotalBytes < bigFileBytes {
		t.Fatalf("%+v", marker)
	}
	if got := c.evidence(marker.EvidenceID); int64(len(got)) != marker.TotalBytes || !strings.Contains(got, "0123456789abcdef") {
		t.Fatalf("the reference reads back the %d stored bytes: %d", marker.TotalBytes, len(got))
	}
	// Normal's failure is kept as Evidence of the Run, with the compaction's own result.
	if p := c.purposes(start.RunID); !strings.Contains(p, "compaction_normal_failure") || !strings.Contains(p, "compaction_result") || !strings.Contains(p, "compaction_summary_data") {
		t.Fatalf("evidence: %s", p)
	}
	if !strings.Contains(c.val("SELECT count(*) FROM actions WHERE name='work_summary' AND status='completed'"), "1") {
		t.Fatal("the Summary generation itself ended as a generation")
	}
}

// TestACompactionStoppedMidGenerationIsCancelledWithTheGenerationUnknown is the interrupt
// during a compaction: nothing is committed, the generation stays unknown and listed, and the
// Run is the cancelled Run that a stop makes of it.
func TestACompactionStoppedMidGenerationIsCancelledWithTheGenerationUnknown(t *testing.T) {
	c := newCompactionRig(t, map[string]harnesstest.Reply{modelport.StageSummary: {Kind: harnesstest.KindHang}}, contextLimit)
	reached := make(chan struct{})
	var once sync.Once
	c.fake.OnGenerate = func(_ context.Context, req modelport.ChatRequest) {
		if req.Rencrow.Harness.Stage == modelport.StageSummary {
			once.Do(func() { close(reached) })
		}
	}
	info := c.openSession("compact.interrupt.open")
	start := c.startRun(info.ThreadID, "compact.interrupt.001", "大きなファイルを読んで保存処理を実装する。")
	<-reached
	if receipt := c.interrupt(start.RunID, 0, "compact.interrupt.k1"); receipt.Code != "CANCEL_REQUESTED" {
		t.Fatalf("%+v", receipt)
	}
	run := c.waitTerminal(start.RunID)
	res := run.Result
	if res.Status != "cancelled" || res.Code != "CANCELLED" || len(res.UnresolvedActionIDs) != 1 || run.GenerationAttemptsUnknown != 1 || run.GenerationAttemptsUsed != 2 {
		t.Fatalf("%+v %+v", res, run)
	}
	if got := c.val("SELECT name||'/'||status FROM actions WHERE action_id=?", res.UnresolvedActionIDs[0]); got != "work_summary/unknown" {
		t.Fatalf("the unresolved generation is the Summary: %s", got)
	}
	if len(c.checkpoints()) != 0 || c.val("SELECT COALESCE(current_checkpoint_id,'none') FROM threads") != "none" || res.LastCheckpointID != nil {
		t.Fatal("a checkpoint was committed after the stop")
	}
	if got := len(c.acts); got != 1 {
		t.Fatalf("no act generation followed the stop: %d", got)
	}
	// What the compaction had made so far is on record, and so is that it was stopped.
	if p := c.purposes(start.RunID); !strings.Contains(p, "model_attempt_receipt") || strings.Contains(p, "compaction_result") && !strings.Contains(p, "measure_result") {
		t.Fatalf("%s", p)
	}
}

// TestACompactionGenerationOfUnknownOutcomeStillMakesItsCheckpointAndThenBlocksTheRun: the
// reduction that needs no model is done and stored, and the Run, whose own generation has no
// known end, does not generate again.
func TestACompactionGenerationOfUnknownOutcomeStillMakesItsCheckpointAndThenBlocksTheRun(t *testing.T) {
	c := newCompactionRig(t, map[string]harnesstest.Reply{modelport.StageSummary: {Kind: harnesstest.KindTransportError}}, contextLimit)
	start, run := c.run("compact.unknown.0001", "大きなファイルを読んで保存処理を実装する。")
	res := run.Result
	if res.Status != "blocked" || res.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" || !res.Resumable || len(res.UnresolvedActionIDs) != 1 || res.LastCheckpointID == nil {
		t.Fatalf("%+v", res)
	}
	if got := c.val("SELECT name||'/'||status FROM actions WHERE action_id=?", res.UnresolvedActionIDs[0]); got != "work_summary/unknown" {
		t.Fatal(got)
	}
	cps := c.checkpoints()
	if len(cps) != 1 || !strings.Contains(cps[0], "/emergency/") || len(c.acts) != 1 {
		t.Fatalf("checkpoints %v, act steps %d", cps, len(c.acts))
	}
	if run.GenerationAttemptsUnknown != 1 || c.val("SELECT phase FROM runs WHERE run_id='"+start.RunID+"'") != "Terminal" {
		t.Fatalf("%+v", run)
	}
	// A new Run of the Task is refused its generation while that one is unresolved.
	resume := c.mustCall("run/resume", protocol.ResumeInput{TaskID: start.TaskID, ExpectedLastRunID: start.RunID, ExpectedControlRevision: 0, IdempotencyKey: "compact.resume.key.001", Limits: startLimits})
	resumed := decode[protocol.ResumeResult](c.t, resume)
	resume.Done()
	r2 := c.waitTerminal(resumed.RunID)
	if r2.Result.Status != "blocked" || r2.Result.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" || len(c.acts) != 1 {
		t.Fatalf("%+v", r2.Result)
	}
}

// TestThePreflightOfARunBlocksAtOnceWhenTheSmallestCandidateDoesNotFit is A24 through a Run:
// capacity is blocked, there is no Summary generation and no Emergency.
func TestThePreflightOfARunBlocksAtOnceWhenTheSmallestCandidateDoesNotFit(t *testing.T) {
	small := 4096 + 2048 + 400
	c := newCompactionRig(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}, int64(small))
	c.write("big.txt", strings.Repeat("x", 3000)) // an answer that makes the next prompt too large, and that is all
	start, run := c.run("compact.preflight.001", "大きなファイルを読んで保存処理を実装する。")
	res := run.Result
	if res.Status != "blocked" || res.Code != "CAPACITY_BLOCKED" || !res.Resumable || res.LastCheckpointID != nil {
		t.Fatalf("%+v", res)
	}
	if len(c.stages[modelport.StageSummary]) != 0 || len(c.stages[modelport.StageSelection]) != 0 || len(c.checkpoints()) != 0 {
		t.Fatal("the Preflight spends no generation and stores nothing")
	}
	if got := run.GenerationAttemptsUsed; got != 1 {
		t.Fatalf("attempts used %d: the act that asked for the Tool only", got)
	}
	if !strings.Contains(c.purposes(start.RunID), "compaction_result") {
		t.Fatalf("the block is on record: %s", c.purposes(start.RunID))
	}
}

// startNext starts a Run on a thread that already has history: the revisions it names are
// the thread's.
func (r *rig) startNext(thread, key, text string) protocol.StartResult {
	r.t.Helper()
	rev, _ := strconv.ParseInt(r.val("SELECT context_revision FROM threads WHERE thread_id=?", thread), 10, 64)
	ctl, _ := strconv.ParseInt(r.val("SELECT control_revision FROM threads WHERE thread_id=?", thread), 10, 64)
	return r.startRun(thread, key, text, func(in *protocol.StartInput) { in.ExpectedContextRevision, in.ExpectedControlRevision = rev, ctl })
}

func (r *rig) compactionReason(runID string) string {
	r.t.Helper()
	id := r.val("SELECT evidence_id FROM evidence WHERE run_id=? AND json_extract(metadata_json,'$.purpose')='compaction_result' ORDER BY rowid DESC LIMIT 1", runID)
	return r.evidence(id)
}

// TestACommitThatFailsOrLosesItsAnswerIsResolvedByTheCheckpointItself is A07 and H25 through a
// Run: a commit that did not take effect ends the Run restart_required with no checkpoint,
// and one that took effect and lost its answer is found again by its issued ID and bytes and
// the Run goes on; no second checkpoint is made either way.
func TestACommitThatFailsOrLosesItsAnswerIsResolvedByTheCheckpointItself(t *testing.T) {
	t.Run("the commit did not take effect", func(t *testing.T) {
		c := newCompactionRigWith(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}, contextLimit, func(r *rig) {
			r.fault = func(point string) error {
				if point == "checkpoint.before_commit" {
					return errors.New("the disk is gone")
				}
				return nil
			}
		})
		_, run := c.run("compact.fault.0000001", "大きなファイルを読んで保存処理を実装する。")
		res := run.Result
		if res.Status != "restart_required" || res.Code != "PERSISTENCE_UNCERTAIN" || !res.Resumable || res.LastCheckpointID != nil {
			t.Fatalf("%+v", res)
		}
		if len(c.checkpoints()) != 0 || len(c.acts) != 1 {
			t.Fatalf("checkpoints %v, acts %d: nothing is stored and nothing more is generated", c.checkpoints(), len(c.acts))
		}
	})
	t.Run("the commit took effect and its answer was lost", func(t *testing.T) {
		c := newCompactionRigWith(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}, contextLimit, func(r *rig) {
			r.fault = func(point string) error {
				if point == "checkpoint.after_commit" {
					return errors.New("the connection dropped")
				}
				return nil
			}
		})
		_, run := c.run("compact.fault.0000002", "大きなファイルを読んで保存処理を実装する。")
		res := run.Result
		if res.Status != "completed" || res.LastCheckpointID == nil || len(c.checkpoints()) != 1 || len(c.acts) != 2 {
			t.Fatalf("%+v checkpoints %v", res, c.checkpoints())
		}
		if got := len(c.stages[modelport.StageSummary]); got != 1 {
			t.Fatalf("a second candidate was made: %d Summary requests", got)
		}
	})
}

// newCompactionRigWith is newCompactionRig for a test that must change the rig before its
// services are built.
func newCompactionRigWith(t *testing.T, stageReplies map[string]harnesstest.Reply, limit int64, before func(r *rig)) *compactionRig {
	t.Helper()
	fake := harnesstest.NewFake()
	fake.SetMeasure(harnesstest.MeasureConfig{Limit: limit})
	c := &compactionRig{fake: fake, stages: map[string][]modelport.ChatRequest{}}
	c.install(stageReplies)
	r := &rig{}
	if before != nil {
		before(r)
	}
	built, rec := toolRigWith(t, fake, toolConfig{}, r)
	built.write("big.txt", bigFile())
	c.rig, c.rec = built, rec
	return c
}

// toolRigWith is toolRig for a rig some of whose settings were chosen first.
func toolRigWith(t *testing.T, fake *harnesstest.Fake, c toolConfig, first *rig) (*rig, *recorder) {
	t.Helper()
	layout := harnesstest.NewLayout(t, harnesstest.Options{CreateData: true})
	layout.Reg["policies"].([]any)[0].(map[string]any)["tools"] = []any{}
	layout.Write()
	c.edit(layout)
	layout.Write()
	dep, err := config.Load(layout.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Init(context.Background(), dep.DataRoot); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, layout: layout, dep: dep, model: fake, fault: first.fault, controlPoll: first.controlPoll}
	r.store, r.svc = r.process()
	rec := &recorder{}
	r.conn = r.svc.NewConn(rec)
	return r, rec
}

// install is the fake's behavior of the scenario: read the big file first, give the stage
// replies when a stage is asked, and answer once the prompt has been dealt with.
func (c *compactionRig) install(stageReplies map[string]harnesstest.Reply) {
	c.fake.SetReply(harnesstest.Reply{Dynamic: func(req modelport.ChatRequest) harnesstest.Reply {
		stage := req.Rencrow.Harness.Stage
		if stage != modelport.StageAct {
			c.stages[stage] = append(c.stages[stage], req)
			if r, ok := stageReplies[stage]; ok {
				return r
			}
			return harnesstest.Final(`{"operations":[]}`)
		}
		c.acts = append(c.acts, req)
		for _, m := range req.Messages {
			if m.Role == "tool" || strings.HasPrefix(m.Text(), "RENCROW_CONTEXT_BOUNDARY_V1") {
				return harnesstest.Final("compaction handled, the work goes on")
			}
		}
		return calls(tc("call-1", "file.read", readArgs("big.txt", 0, bigFileBytes, bigFileBytes)))
	}})
}

// TestACandidateIsMadeAgainWhenTheContextMovedAndTheSecondStaleOneEndsTheRun is H13 and A06:
// the context revision changing while the Summary is generated makes the candidate stale, it
// is not stored, the step is prepared and counted again and a new candidate follows (spending
// its own generation attempt); a second stale candidate ends the Run, with nothing stored.
func TestACandidateIsMadeAgainWhenTheContextMovedAndTheSecondStaleOneEndsTheRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bumps  int
		status string
		code   string
		sums   int
	}{{"once", 1, "completed", "FINAL_RESPONSE_ACCEPTED", 2}, {"twice", 2, "incomplete", "COMPACTION_STALE", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCompactionRig(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}, contextLimit)
			var n int
			c.fake.OnGenerate = func(_ context.Context, req modelport.ChatRequest) {
				if req.Rencrow.Harness.Stage == modelport.StageSummary && n < tc.bumps {
					n++
					c.exec("UPDATE threads SET context_revision=context_revision+1") // the context moved under the candidate
				}
			}
			start, run := c.run("compact.stale.0000001"+tc.name, "大きなファイルを読んで保存処理を実装する。")
			res := run.Result
			if res.Status != tc.status || res.Code != tc.code {
				t.Fatalf("%+v", res)
			}
			if got := len(c.stages[modelport.StageSummary]); got != tc.sums || len(c.acts) < 1 {
				t.Fatalf("%d Summary requests", got)
			}
			if tc.status == "completed" {
				if len(c.checkpoints()) != 1 || res.LastCheckpointID == nil || run.GenerationAttemptsUsed != 4 {
					t.Fatalf("checkpoints %v, attempts %d: act, summary, summary, act", c.checkpoints(), run.GenerationAttemptsUsed)
				}
			} else if len(c.checkpoints()) != 0 || res.LastCheckpointID != nil || run.GenerationAttemptsUsed != 3 || !res.Resumable {
				t.Fatalf("checkpoints %v, attempts %d", c.checkpoints(), run.GenerationAttemptsUsed)
			}
			_ = start
		})
	}
}

// TestAnInputThatOnlyWaitsInTheQueueDoesNotMakeACandidateStale is H13: an input appended while
// the Summary is generated moves the queue revision and nothing the candidate was made
// against; it is committed, and the input stays queued for the next Run.
func TestAnInputThatOnlyWaitsInTheQueueDoesNotMakeACandidateStale(t *testing.T) {
	c := newCompactionRig(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}, contextLimit)
	var runID string
	var once sync.Once
	c.fake.OnGenerate = func(_ context.Context, req modelport.ChatRequest) {
		if req.Rencrow.Harness.Stage == modelport.StageSummary {
			once.Do(func() {
				res := c.mustCall("input/append", protocol.InputAppendInput{ThreadID: c.val("SELECT thread_id FROM threads"), RunID: runID, Input: protocol.InputMessage{Text: "later"},
					Disposition: protocol.DispositionNextTurn, ExpectedControlRevision: 0, IdempotencyKey: "compact.queue.append.1"})
				res.Done()
			})
		}
	}
	info := c.openSession("compact.queue.open")
	start := c.startRunWith(info.ThreadID, "compact.queue.001", "大きなファイルを読んで保存処理を実装する。", func(id string) { runID = id })
	run := c.waitTerminal(start.RunID)
	if run.Result.Status != "completed" || len(c.checkpoints()) != 1 || len(c.stages[modelport.StageSummary]) != 1 {
		t.Fatalf("%+v checkpoints %v", run.Result, c.checkpoints())
	}
	if c.val("SELECT delivery_state FROM queue_inputs") != "queued" {
		t.Fatal("the queued input was touched by the checkpoint")
	}
	// The next Run starts from the checkpoint and applies the queued input first.
	next := c.startNext(info.ThreadID, "compact.queue.002", "続きをお願いします。")
	if r2 := c.waitTerminal(next.RunID); r2.Result.Status != "completed" {
		t.Fatalf("%+v", r2.Result)
	}
	p := promptOf(lastAct(c))
	if !(strings.Index(p, `"text":"later"`) > 0 && strings.Index(p, `"text":"later"`) < strings.Index(p, "続きをお願いします。")) || !strings.Contains(p, "user:RENCROW_CONTEXT_BOUNDARY_V1") {
		t.Fatalf("%.1500s", p)
	}
}

// TestARunStartedAfterACheckpointIsPromptedFromItAndTheTailAfterIt is A60: the checkpoint's
// projection and what was applied after it, rendered by the one function, with no Work that
// the Summary covers and no answer of a Tool in full.
func TestARunStartedAfterACheckpointIsPromptedFromItAndTheTailAfterIt(t *testing.T) {
	c := newCompactionRig(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}, contextLimit)
	info := c.openSession("compact.after.open")
	first := c.startRun(info.ThreadID, "compact.after.001", "大きなファイルを読んで保存処理を実装する。")
	if r := c.waitTerminal(first.RunID); r.Result.Status != "completed" || len(c.checkpoints()) != 1 {
		t.Fatalf("%+v", r.Result)
	}
	second := c.startNext(info.ThreadID, "compact.after.002", "テストも書いてください。")
	r2 := c.waitTerminal(second.RunID)
	if r2.Result.Status != "completed" || len(c.checkpoints()) != 1 || len(c.stages[modelport.StageSummary]) != 1 {
		t.Fatalf("%+v checkpoints %v", r2.Result, c.checkpoints())
	}
	p := promptOf(lastAct(c))
	order := []string{`"text":"大きなファイルを読んで保存処理を実装する。"`, "user:RENCROW_CONTEXT_BOUNDARY_V1", "assistant:RENCROW_ACCEPTED_SUMMARY_V1", "user:RENCROW_OBSERVATION_REFERENCE_V1", `"text":"テストも書いてください。"`}
	at := -1
	for _, want := range order {
		i := strings.Index(p, want)
		if i <= at {
			t.Fatalf("%q is not after the one before it in the prompt:\n%.2500s", want, p)
		}
		at = i
	}
	if strings.Contains(p, "0123456789abcdef") || strings.Contains(p, "compaction handled, the work goes on") && !strings.Contains(p, "assistant:compaction handled") {
		t.Fatal("covered Work or a Tool answer is in the prompt")
	}
	// The first Run's final answer came after the checkpoint and is in the tail of the second.
	if !strings.Contains(p, "assistant:compaction handled, the work goes on") {
		t.Fatal("what was applied after the checkpoint is its tail")
	}
	if got := c.val("SELECT generation_attempts_used FROM runs WHERE run_id='" + second.RunID + "'"); got != "1" {
		t.Fatalf("the second Run needed no compaction: %s attempts", got)
	}
}

func (r *rig) storedCheckpoint() *compaction.Checkpoint {
	r.t.Helper()
	db, err := sql.Open("sqlite", "file:"+r.dep.DataRoot+"/"+sqlite.DatabaseFile+"?mode=ro")
	if err != nil {
		r.t.Fatal(err)
	}
	defer db.Close()
	var blob []byte
	var hash string
	if err := db.QueryRow("SELECT candidate_bytes, candidate_hash FROM checkpoints ORDER BY rowid DESC LIMIT 1").Scan(&blob, &hash); err != nil {
		r.t.Fatal(err)
	}
	cp, err := compaction.ParseCheckpoint(blob, hash)
	if err != nil {
		r.t.Fatal(err)
	}
	return cp
}

// TestTheCountOfACandidateIsTheCountOfTheRequestThatIsThenSent is A25 through a Run: the
// candidate's after count was taken of the very request the next act step sends: the same
// messages, Tools and options, so the same logical input, whose digest is in the count.
func TestTheCountOfACandidateIsTheCountOfTheRequestThatIsThenSent(t *testing.T) {
	c := newCompactionRig(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}, contextLimit)
	_, run := c.run("compact.count.0000001", "大きなファイルを読んで保存処理を実装する。")
	if run.Result.Status != "completed" {
		t.Fatalf("%+v", run.Result)
	}
	cp := c.storedCheckpoint()
	sent := lastAct(c)
	digest, err := sent.LogicalInputDigest()
	if err != nil {
		t.Fatal(err)
	}
	cand := cp.Candidate
	if cand.AfterCount.InputDigest != digest || cand.AfterCount.BindingFingerprint != harnesstest.FakeFingerprint || cand.AfterCount.State != "verified_exact" ||
		cand.AfterCount.ReservedOutputTokens != sent.MaxTokens {
		t.Fatalf("after %+v, sent input digest %s", cand.AfterCount, digest)
	}
	// The before count is that of the prompt that did not fit: the one the Tool answer made.
	if *cand.BeforeCount.PromptUpper <= usableTokens || *cand.AfterCount.PromptUpper > usableTokens || *cand.AfterCount.PromptUpper >= *cand.BeforeCount.PromptLower {
		t.Fatalf("before %d after %d usable %d", *cand.BeforeCount.PromptUpper, *cand.AfterCount.PromptUpper, usableTokens)
	}
	// And both counts are the Evidence the event names.
	ev := ofType(c.threadEvents(c.val("SELECT thread_id FROM threads")), "checkpoint.committed")
	if len(ev) != 1 {
		t.Fatal("one checkpoint.committed")
	}
	p := payloadOf[protocol.CheckpointCommittedPayload](t, ev[0])
	if p.CandidateHash != cp.Hash || p.CheckpointID != cand.CheckpointID || p.SemanticBoundary != cand.SemanticBoundary || p.DurableBoundary != cand.DurableBoundary ||
		!strings.Contains(c.evidence(p.BeforeCountEvidenceID), `"prompt_upper"`) || !strings.Contains(c.evidence(p.AfterCountEvidenceID), `"prompt_upper"`) {
		t.Fatalf("%+v", p)
	}
}

// TestSelectionAndSummaryAreEachAskedOnceAndTheSelectionIsApplied is A22/A23/A45 through a
// Run: two instructions of the Thread give the Selection stage something to select, its
// operation is applied by exact quote, what it removed is not in the Summary data, and the
// checkpoint records the selection and keeps the reason.
func TestSelectionAndSummaryAreEachAskedOnceAndTheSelectionIsApplied(t *testing.T) {
	selection := `{"operations":[{"operation":"drop_superseded","target_handle":"presented-0","target_quote":"方式Aで","replacement_handle":"presented-1","replacement_quote":"方式Aはやめる","basis":"revocation"}]}`
	c := newCompactionRig(t, map[string]harnesstest.Reply{modelport.StageSelection: harnesstest.Final(selection), modelport.StageSummary: harnesstest.Final(goodSummary)}, contextLimit)
	info := c.openSession("compact.select.open")
	// A first Run of the Thread is small: it reads nothing (the fake answers at once).
	c.fake.SetReply(harnesstest.Final("了解しました。"))
	first := c.startRun(info.ThreadID, "compact.select.001", "方式Aで保存処理を実装する。ログは残す。")
	if r := c.waitTerminal(first.RunID); r.Result.Status != "completed" {
		t.Fatalf("%+v", r.Result)
	}
	c.install(map[string]harnesstest.Reply{modelport.StageSelection: harnesstest.Final(selection), modelport.StageSummary: harnesstest.Final(goodSummary)})
	second := c.startNext(info.ThreadID, "compact.select.002", "方式Aはやめる。大きなファイルを読む。")
	run := c.waitTerminal(second.RunID)
	if run.Result.Status != "completed" || len(c.stages[modelport.StageSelection]) != 1 || len(c.stages[modelport.StageSummary]) != 1 {
		t.Fatalf("%+v selection %d summary %d", run.Result, len(c.stages[modelport.StageSelection]), len(c.stages[modelport.StageSummary]))
	}
	cand := c.storedCheckpoint().Candidate
	if cand.Mode != "normal" || len(cand.AppliedSelection) != 1 || cand.AppliedSelection[0].Basis != "revocation" {
		t.Fatalf("%+v", cand.AppliedSelection)
	}
	var texts []string
	for _, e := range cand.Projection.Entries {
		if e.Kind == "instruction" {
			texts = append(texts, e.Messages[0].Text())
		}
	}
	joined := strings.Join(texts, "|")
	if strings.Contains(joined, "方式Aで") || !strings.Contains(joined, "保存処理を実装する。ログは残す。") || !strings.Contains(joined, "方式Aはやめる。大きなファイルを読む。") {
		t.Fatalf("retained instructions: %s", joined)
	}
	if strings.Contains(c.stages[modelport.StageSummary][0].Messages[1].Text(), "方式Aで") {
		t.Fatal("removed text is in the Summary data")
	}
	// Selection's data and manifest, and the Summary's, are all kept.
	p := c.purposes(second.RunID)
	for _, want := range []string{"compaction_selection_data", "compaction_selection_data_manifest", "compaction_summary_data", "compaction_summary_data_manifest", "compaction_result"} {
		if !strings.Contains(p, want) {
			t.Errorf("%s is not among the Evidence: %s", want, p)
		}
	}
	if got := run.GenerationAttemptsUsed; got != 4 {
		t.Fatalf("attempts %d: act, selection, summary, act", got)
	}
}

// TestACheckpointThatCannotBeReadBackStopsTheRunAndIsNeverReadAsNone is H03, H11 and H25 through
// a Run: the Thread's checkpoint, changed in any way the format does not allow, ends the next
// Run INTEGRITY_BLOCKED; no earlier state is used in its place.
func TestACheckpointThatCannotBeReadBackStopsTheRunAndIsNeverReadAsNone(t *testing.T) {
	tamper := map[string]string{
		"the bytes":      "UPDATE checkpoints SET candidate_bytes=candidate_bytes||x'20'",
		"the hash":       "UPDATE checkpoints SET candidate_hash='" + strings.Repeat("0", 64) + "'",
		"the mode":       "UPDATE checkpoints SET mode='emergency'",
		"the boundary":   "UPDATE checkpoints SET durable_boundary=durable_boundary+1",
		"the revision":   "UPDATE checkpoints SET context_revision=context_revision+1",
		"a metadata key": "UPDATE checkpoints SET candidate_bytes=replace(candidate_bytes, CAST('\"mode\":' AS BLOB), CAST('\"modx\":' AS BLOB))",
	}
	for name, q := range tamper {
		t.Run(name, func(t *testing.T) {
			c := newCompactionRig(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}, contextLimit)
			info := c.openSession("compact.tamper." + strings.ReplaceAll(name, " ", "") + ".o")
			first := c.startRun(info.ThreadID, "compact.tamper.00000001", "大きなファイルを読んで保存処理を実装する。")
			if r := c.waitTerminal(first.RunID); r.Result.Status != "completed" || len(c.checkpoints()) != 1 {
				t.Fatalf("%+v", r.Result)
			}
			c.exec("DROP TRIGGER checkpoints_no_update")
			c.exec(q)
			acts := len(c.acts)
			second := c.startNext(info.ThreadID, "compact.tamper.00000002", "続けてください。")
			run := c.waitTerminal(second.RunID)
			if run.Result.Status != "blocked" || run.Result.Code != "INTEGRITY_BLOCKED" || run.Result.Resumable || len(c.acts) != acts {
				t.Fatalf("%+v: the Run must stop before it generates", run.Result)
			}
		})
	}
}

// TestACompactionIsOnlyAttemptedWhenTheDeploymentEnablesIt: with compaction disabled a prompt
// that does not fit ends the Run blocked for capacity, as it always did, with no stage request.
func TestACompactionIsOnlyAttemptedWhenTheDeploymentEnablesIt(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetMeasure(harnesstest.MeasureConfig{Limit: contextLimit})
	c := &compactionRig{fake: fake, stages: map[string][]modelport.ChatRequest{}}
	c.install(map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)})
	layout := harnesstest.NewLayout(t, harnesstest.Options{CreateData: true})
	layout.Reg["policies"].([]any)[0].(map[string]any)["tools"] = []any{}
	layout.Write()
	toolConfig{}.edit(layout)
	layout.Cfg["compaction"].(map[string]any)["enabled"] = false
	layout.Write()
	dep, err := config.Load(layout.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Init(context.Background(), dep.DataRoot); err != nil {
		t.Fatal(err)
	}
	rr := &rig{t: t, layout: layout, dep: dep, model: fake}
	rr.store, rr.svc = rr.process()
	rr.conn = rr.svc.NewConn(&recorder{})
	rr.write("big.txt", bigFile())
	c.rig = rr
	_, run := c.run("compact.disabled.001", "大きなファイルを読んで保存処理を実装する。")
	if run.Result.Status != "blocked" || run.Result.Code != "CAPACITY_BLOCKED" || len(c.stages[modelport.StageSummary]) != 0 || len(c.checkpoints()) != 0 {
		t.Fatalf("%+v", run.Result)
	}
}

// TestAStageAnswerThatIsAToolCallOrOnlyThinkingIsNotASummary is H21 and A26 through a Run: the
// stage declares no Tool, so a Tool call in its answer, or an answer with no text, is a failure
// of the stage and the reduction without a model takes over; no Tool of it is run.
func TestAStageAnswerThatIsAToolCallOrOnlyThinkingIsNotASummary(t *testing.T) {
	for name, reply := range map[string]harnesstest.Reply{
		"a Tool call":    {Kind: harnesstest.KindToolCall, Calls: []harnesstest.Call{tc("c-x", "file.read", readArgs("big.txt", 0, 10, 10))}},
		"only reasoning": {Kind: harnesstest.KindReasoningOnly, Reasoning: "thinking about the summary"},
		"a length stop":  {Kind: harnesstest.KindLength, Text: `{"current_work":[`},
		"a refusal":      {Kind: harnesstest.KindRefusal},
		"an empty text":  harnesstest.Final(""),
	} {
		t.Run(name, func(t *testing.T) {
			c := newCompactionRig(t, map[string]harnesstest.Reply{modelport.StageSummary: reply}, contextLimit)
			start, run := c.run("compact.item.0000001", "大きなファイルを読んで保存処理を実装する。")
			res := run.Result
			if res.Status != "completed" || res.LastCheckpointID == nil || len(c.stages[modelport.StageSummary]) != 1 {
				t.Fatalf("%+v", res)
			}
			if cps := c.checkpoints(); len(cps) != 1 || !strings.Contains(cps[0], "/emergency/") {
				t.Fatalf("%v", cps)
			}
			if c.val("SELECT COUNT(*) FROM actions WHERE run_id=? AND kind='tool'", start.RunID) != "1" {
				t.Fatal("a Tool call of the stage's answer was run")
			}
		})
	}
}

// TestAReferenceCanBeFollowedToTheStoredOriginal is A60 and A13 through a Run: the model reads
// the reference's Evidence through evidence.read with the range the reference allows, and gets
// the bytes that were stored; the Tool is not run again.
func TestAReferenceCanBeFollowedToTheStoredOriginal(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetMeasure(harnesstest.MeasureConfig{Limit: contextLimit})
	var reads []string
	fake.SetReply(harnesstest.Reply{Dynamic: func(req modelport.ChatRequest) harnesstest.Reply {
		if req.Rencrow.Harness.Stage != modelport.StageAct {
			return harnesstest.Final(`{"current_work":[{"text":"<tool_call>","source_handles":["work-0"]}],"decisions":[],"verification":[],"open_items":[],"next_steps":[],"important_observation_handles":[]}`)
		}
		var marker struct {
			EvidenceID string `json:"evidence_id"`
		}
		var last string
		for _, m := range req.Messages {
			if m.Role == "tool" {
				last = m.Text()
			}
		}
		switch {
		case last == "":
			return calls(tc("call-1", "file.read", readArgs("big.txt", 0, bigFileBytes, bigFileBytes)))
		case strings.HasPrefix(last, "RENCROW_OBSERVATION_REFERENCE_V1\n"):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(last, "RENCROW_OBSERVATION_REFERENCE_V1\n")), &marker); err != nil {
				t.Errorf("%v", err)
			}
			return calls(tc("call-2", "evidence.read", evidenceArgs(marker.EvidenceID, "text/v1", 0, 3000)))
		default:
			reads = append(reads, last)
			return harnesstest.Final("read it back")
		}
	}})
	r, _ := toolRig(t, fake, toolConfig{})
	r.write("big.txt", bigFile())
	info := r.openSession("compact.follow.open")
	start := r.startRun(info.ThreadID, "compact.follow.001", "大きなファイルを読んで保存処理を実装する。")
	run := r.waitTerminal(start.RunID)
	if run.Result.Status != "completed" || len(reads) != 1 {
		t.Fatalf("%+v reads %d", run.Result, len(reads))
	}
	var v toolview.View
	if err := json.Unmarshal([]byte(reads[0]), &v); err != nil || v.Tool != "evidence.read" || v.EffectState != "completed" {
		t.Fatalf("%v %+v", err, v)
	}
	if got := resultOf(t, v); !strings.Contains(j(got), "0123456789abcdef") && !strings.Contains(string(v.Result), "MDEyMzQ1Njc4OWFiY2RlZg") {
		t.Fatalf("the original's bytes: %.300s", string(v.Result))
	}
	// The file Tool ran once: following a reference does not run the Tool again.
	if r.val("SELECT COUNT(*) FROM actions WHERE run_id=? AND name='file.read'", start.RunID) != "1" {
		t.Fatal("the Tool was run again")
	}
}

// TestACompactionAfterAnEmergencyCheckpointReadsTheOriginalBackAndStandsOnIt is H19 and H14
// through two Runs of one Thread: the first Run's Summary fails and leaves an Emergency
// checkpoint whose answer is a reference; the second Run's compaction is a Normal one, that
// reads the first answer's stored original back for the Summary data, stands on the first
// checkpoint (its parent), and moves the semantic boundary forward from where the Emergency
// one left it.
func TestACompactionAfterAnEmergencyCheckpointReadsTheOriginalBackAndStandsOnIt(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetMeasure(harnesstest.MeasureConfig{Limit: contextLimit})
	var summaries int
	var stageData []string
	readRE := regexp.MustCompile(`READ-([a-z0-9]+\.txt)`)
	fake.SetReply(harnesstest.Reply{Dynamic: func(req modelport.ChatRequest) harnesstest.Reply {
		if req.Rencrow.Harness.Stage == modelport.StageSelection {
			return harnesstest.Final(`{"operations":[]}`)
		}
		if req.Rencrow.Harness.Stage == modelport.StageSummary {
			summaries++
			stageData = append(stageData, req.Messages[1].Text())
			if summaries == 1 {
				return harnesstest.Final("this is not json")
			}
			return harnesstest.Final(`{"current_work":[{"text":"二つのファイルを読んだ。","source_handles":["work-0","observation-0"]}],"decisions":[],"verification":[],"open_items":[],"next_steps":[],"important_observation_handles":["observation-0"]}`)
		}
		want := ""
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if m := readRE.FindStringSubmatch(req.Messages[i].Text()); m != nil && req.Messages[i].Role == "user" {
				want = m[1]
				break
			}
			// What was read is either in the prompt as an answer, or folded into a Summary.
			if req.Messages[i].Role == "tool" || strings.HasPrefix(req.Messages[i].Text(), "RENCROW_CONTEXT_BOUNDARY_V1") {
				return harnesstest.Final("ok")
			}
		}
		if want == "" {
			return harnesstest.Final("nothing to read")
		}
		return calls(tc("call-"+want, "file.read", readArgs(want, 0, bigFileBytes, bigFileBytes)))
	}})
	r, _ := toolRig(t, fake, toolConfig{})
	r.write("one.txt", strings.Repeat("first file line\n", bigFileBytes/16))
	r.write("two.txt", strings.Repeat("second file line\n", bigFileBytes/17))
	info := r.openSession("compact.chain.open")
	first := r.startRun(info.ThreadID, "compact.chain.001", "READ-one.txt と報告してください。")
	if res := r.waitTerminal(first.RunID).Result; res.Status != "completed" {
		t.Fatalf("%+v", res)
	}
	cps := r.checkpoints()
	if len(cps) != 1 || !strings.Contains(cps[0], "/emergency/0/") {
		t.Fatalf("%v", cps)
	}
	firstID := strings.Split(cps[0], "/")[0]
	second := r.startNext(info.ThreadID, "compact.chain.002", "READ-two.txt と報告してください。")
	res := r.waitTerminal(second.RunID).Result
	if res.Status != "completed" || summaries != 2 {
		t.Fatalf("%s/%s, summaries %d", res.Status, res.Code, summaries)
	}
	cp := r.storedCheckpoint()
	c := cp.Candidate
	if c.Mode != "normal" || c.ParentCheckpointID == nil || *c.ParentCheckpointID != firstID || c.SemanticBoundary == 0 || c.Projection.Summary == nil {
		t.Fatalf("mode %s parent %v boundary %d", c.Mode, c.ParentCheckpointID, c.SemanticBoundary)
	}
	// The Summary data of the second compaction carries the edges of the first file, which was
	// only a reference by then: read back from the stored original.
	if !strings.Contains(stageData[1], "first file line") || !strings.Contains(stageData[1], "second file line") {
		t.Fatalf("the Summary data lacks an original")
	}
	if len(r.checkpoints()) != 2 {
		t.Fatalf("%v", r.checkpoints())
	}
	// Both observations are in the inventory; the one the Summary cited is covered by its excerpts.
	if len(c.ObservationInventory) != 2 {
		t.Fatalf("%d inventory entries", len(c.ObservationInventory))
	}
	var covered int
	for _, o := range c.ObservationInventory {
		if len(o.PresentedRanges) != 0 || !o.Partial {
			t.Fatalf("a reference shows nothing in full: %+v", o)
		}
		covered += len(o.SummaryCoveredRange)
	}
	if covered == 0 {
		t.Fatal("what the Summary was given is recorded as covered")
	}
}

// TestAProcessThatDiesDuringACompactionLeavesNothingStoredButEvidence is the crash side of A07:
// the driver's process ends while the Summary is generated. The next driver settles the Run as
// blocked with that generation unknown, there is no checkpoint, and a new Run of the Thread makes
// its own compaction.
func TestAProcessThatDiesDuringACompactionLeavesNothingStoredButEvidence(t *testing.T) {
	c := newReadOnlyCompactionRig(t, map[string]harnesstest.Reply{modelport.StageSummary: {Kind: harnesstest.KindHang}}, contextLimit)
	reached := make(chan struct{})
	var once sync.Once
	c.fake.OnGenerate = func(_ context.Context, req modelport.ChatRequest) {
		if req.Rencrow.Harness.Stage == modelport.StageSummary {
			once.Do(func() { close(reached) })
		}
	}
	info := c.openSession("compact.crash.open")
	start := c.startRun(info.ThreadID, "compact.crash.001", "大きなファイルを読んで保存処理を実装する。")
	<-reached
	if got := c.val("SELECT phase FROM runs WHERE run_id=?", start.RunID); got != "Compacting" {
		t.Fatalf("the Run is where the kill left it: %s", got)
	}

	svc2, conn2, rec2 := c.restart()
	c.install(map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)})
	settled, again := c.takeOver(svc2, conn2, rec2, info.ThreadID, "compact.crash.again.01")
	if len(ofType(settled, "run.terminal")) != 1 {
		t.Fatalf("the take-over settled the dead Run: %v", eventTypesOf(settled))
	}
	old := resultOfRun(t, svc2, conn2, start.RunID)
	if resultKey(old.Result) != "blocked/MODEL_GENERATION_OUTCOME_UNKNOWN/resumable/unresolved" || old.GenerationAttemptsUnknown != 1 || old.Result.LastCheckpointID != nil {
		t.Fatalf("%s %+v", resultKey(old.Result), old)
	}
	if got := c.val("SELECT name||'/'||status FROM actions WHERE action_id=?", old.Result.UnresolvedActionIDs[0]); got != "work_summary/unknown" {
		t.Fatal(got)
	}
	c.rig.svc, c.rig.conn = svc2, conn2
	var run protocol.RunInfo
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		run = resultOfRun(t, svc2, conn2, again.RunID)
		if run.Terminal {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !run.Terminal || run.Result.Status != "completed" || len(c.checkpoints()) != 1 {
		t.Fatalf("terminal %v status %s/%s checkpoints %v", run.Terminal, run.Result.Status, run.Result.Code, c.checkpoints())
	}
	mustHandle(t, svc2, conn2, "service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
	svc2.Quiesce()
}

// TestAProcessThatDiesAfterTheCommitLeavesTheCheckpointAndTheNextRunStandsOnIt: the checkpoint
// is one transaction, so a process that dies right after it leaves the checkpoint whole, and
// the next Run of the Thread is prompted from it without compacting again.
func TestAProcessThatDiesAfterTheCommitLeavesTheCheckpointAndTheNextRunStandsOnIt(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetMeasure(harnesstest.MeasureConfig{Limit: contextLimit})
	c := &compactionRig{fake: fake, stages: map[string][]modelport.ChatRequest{}}
	reached := make(chan struct{})
	var once sync.Once
	hangAfter := true
	fake.SetReply(harnesstest.Reply{Dynamic: func(req modelport.ChatRequest) harnesstest.Reply {
		if st := req.Rencrow.Harness.Stage; st != modelport.StageAct {
			c.stages[st] = append(c.stages[st], req)
			return harnesstest.Final(goodSummary)
		}
		c.acts = append(c.acts, req)
		folded, answered := false, false
		for _, m := range req.Messages {
			folded = folded || strings.HasPrefix(m.Text(), "RENCROW_CONTEXT_BOUNDARY_V1")
			answered = answered || m.Role == "tool"
		}
		switch {
		case folded && hangAfter:
			once.Do(func() { close(reached) })
			return harnesstest.Reply{Kind: harnesstest.KindHang}
		case folded || answered:
			return harnesstest.Final("carried on")
		}
		return calls(tc("call-1", "file.read", readArgs("big.txt", 0, bigFileBytes, bigFileBytes)))
	}})
	r, _ := toolRig(t, fake, toolConfig{tools: []string{"file.read", "evidence.read"}})
	r.write("big.txt", bigFile())
	c.rig = r
	info := c.openSession("compact.crash2.open")
	start := c.startRun(info.ThreadID, "compact.crash2.001", "大きなファイルを読んで保存処理を実装する。")
	<-reached
	cps := c.checkpoints()
	if len(cps) != 1 {
		t.Fatalf("%v", cps)
	}
	svc2, conn2, rec2 := c.restart()
	hangAfter = false
	settled, again := c.takeOver(svc2, conn2, rec2, info.ThreadID, "compact.crash2.again.1")
	_ = settled
	old := resultOfRun(t, svc2, conn2, start.RunID)
	if old.Result.Status != "blocked" || old.Result.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" || old.Result.LastCheckpointID == nil || !strings.HasPrefix(cps[0], *old.Result.LastCheckpointID+"/") {
		t.Fatalf("%s/%s", old.Result.Status, old.Result.Code)
	}
	var run protocol.RunInfo
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		run = resultOfRun(t, svc2, conn2, again.RunID)
		if run.Terminal {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !run.Terminal || run.Result.Status != "completed" {
		t.Fatalf("%v", run.Terminal)
	}
	if got := len(c.stages[modelport.StageSummary]); got != 1 || len(c.checkpoints()) != 1 {
		t.Fatalf("the next Run compacted again: %d Summary requests, checkpoints %v", got, c.checkpoints())
	}
	if p := promptOf(c.acts[len(c.acts)-1]); !strings.Contains(p, "user:RENCROW_CONTEXT_BOUNDARY_V1") {
		t.Fatalf("the next Run is prompted from the checkpoint:\n%.1500s", p)
	}
	mustHandle(t, svc2, conn2, "service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
	svc2.Quiesce()
}

// TestASummaryRequestThatDoesNotFitEndsTheRunBlockedForCapacity through a Run: the model side counts
// the Summary request over the budget; it is not sent, no checkpoint is made and no reduction
// without a model is tried: blocked / CAPACITY_BLOCKED, resumable, with only the act that asked
// for the Tool generated.
func TestASummaryRequestThatDoesNotFitEndsTheRunBlockedForCapacity(t *testing.T) {
	c := newCompactionRig(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}, contextLimit)
	c.fake.MeasureHook = func(req modelport.MeasureRequest, res *modelport.MeasureResult) error {
		if req.Request.Rencrow.Harness.Stage == modelport.StageSummary {
			n := int64(usableTokens * 10)
			res.PromptLower, res.PromptUpper = &n, &n
		}
		return nil
	}
	start, run := c.run("compact.sumcap.0000001", "大きなファイルを読んで保存処理を実装する。")
	res := run.Result
	if res.Status != "blocked" || res.Code != "CAPACITY_BLOCKED" || !res.Resumable || res.LastCheckpointID != nil {
		t.Fatalf("%s/%s", res.Status, res.Code)
	}
	if len(c.stages[modelport.StageSummary]) != 0 || len(c.checkpoints()) != 0 || len(c.acts) != 1 || run.GenerationAttemptsUsed != 1 {
		t.Fatalf("Summary requests %d, checkpoints %v, acts %d, attempts %d", len(c.stages[modelport.StageSummary]), c.checkpoints(), len(c.acts), run.GenerationAttemptsUsed)
	}
	if got := c.compactionReason(start.RunID); !strings.Contains(got, `"outcome":"CapacityBlocked"`) || !strings.Contains(got, "the Summary request does not fit") || !strings.Contains(got, `"required_minimum_tokens":80000`) {
		t.Fatalf("%s", got)
	}
}
