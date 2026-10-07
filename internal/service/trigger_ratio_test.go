package service_test

import (
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

// The trigger ratio (compaction.trigger_ratio, IMPLEMENTATION_SPEC section 8): a verified count
// of a prompt that still fits, whose upper bound has reached the ratio of the usable budget,
// starts a compaction before the prompt is full. The design config's ratio is 0.85 and the
// usable budget of these scenarios is usableTokens (8,000), so the boundary is 6,800.

const triggerBoundary = usableTokens * 85 / 100 // 6,800

// triggerRig is a Run that reads the big file as many times as it is told to and then answers;
// what each prompt counts is told by counts, by the number of Tool answers it carries (0 for the
// first step) and whether it stands on a checkpoint (what a checkpointed prompt counts is
// summarized).
type triggerRig struct {
	*compactionRig
	reads int
	// counts is the token count of the act prompt of the step that has read n files.
	counts func(n int) int64
	// stageCount is what every stage request counts.
	stageCount int64
	// stageState, when set, is the state the stage counts have.
	stageState string
}

func newTriggerRig(t *testing.T, reads int, summaryReply harnesstest.Reply, counts func(n int) int64) *triggerRig {
	t.Helper()
	return newTriggerRigWith(t, reads, summaryReply, counts, defaultRigBuild)
}

// newTriggerRigWith is newTriggerRig on the rig that build makes.
func newTriggerRigWith(t *testing.T, reads int, summaryReply harnesstest.Reply, counts func(n int) int64, build rigBuild) *triggerRig {
	t.Helper()
	fake := harnesstest.NewFake()
	fake.SetMeasure(harnesstest.MeasureConfig{Limit: contextLimit})
	c := &compactionRig{fake: fake, stages: map[string][]modelport.ChatRequest{}}
	r, rec := build(t, fake)
	r.write("big.txt", bigFile())
	c.rig, c.rec = r, rec
	tr := &triggerRig{compactionRig: c, reads: reads, counts: counts, stageCount: 100}
	fake.SetReply(harnesstest.Reply{Dynamic: func(req modelport.ChatRequest) harnesstest.Reply {
		if st := req.Rencrow.Harness.Stage; st != modelport.StageAct {
			c.stages[st] = append(c.stages[st], req)
			return summaryReply
		}
		c.acts = append(c.acts, req)
		if n := toolAnswers(req); n < tr.reads && !promptHasCheckpoint(req) {
			return calls(tc("call-"+string(rune('a'+n)), "file.read", readArgs("big.txt", 0, bigFileBytes, bigFileBytes)))
		}
		return harnesstest.Final("done")
	}})
	fake.MeasureHook = func(req modelport.MeasureRequest, res *modelport.MeasureResult) error {
		var n int64
		switch {
		case req.Request.Rencrow.Harness.Stage != modelport.StageAct:
			n = tr.stageCount
			if tr.stageState != "" {
				res.State = tr.stageState
			}
		case promptHasCheckpoint(req.Request):
			n = 3000
		default:
			n = tr.counts(toolAnswers(req.Request))
		}
		res.PromptLower, res.PromptUpper = &n, &n
		return nil
	}
	return tr
}

// toolAnswers is how many Tool answers (not references to them) a prompt carries.
func toolAnswers(req modelport.ChatRequest) int {
	n := 0
	for _, m := range req.Messages {
		if m.Role == "tool" && !strings.HasPrefix(m.Text(), "RENCROW_OBSERVATION_REFERENCE_V1") {
			n++
		}
	}
	return n
}

func promptHasCheckpoint(req modelport.ChatRequest) bool {
	for _, m := range req.Messages {
		if strings.HasPrefix(m.Text(), "RENCROW_CONTEXT_BOUNDARY_V1") {
			return true
		}
	}
	return false
}

// TestAVerifiedFitThatReachesTheTriggerRatioIsCompactedBeforeItIsFull: the second step's prompt
// counts exactly the boundary; it is compacted (the prompt is not sent), and the Run goes on
// from the checkpoint. One token under, it is sent as it is and nothing is compacted.
func TestAVerifiedFitThatReachesTheTriggerRatioIsCompactedBeforeItIsFull(t *testing.T) {
	t.Run("on the boundary", func(t *testing.T) {
		tr := newTriggerRig(t, 1, harnesstest.Final(goodSummary), func(n int) int64 {
			if n == 0 {
				return 100
			}
			return triggerBoundary
		})
		start, run := tr.run("trigger.edge.000000001", "大きなファイルを読んで保存処理を実装する。")
		res := run.Result
		if res.Status != "completed" || res.LastCheckpointID == nil || len(tr.checkpoints()) != 1 || !strings.Contains(tr.checkpoints()[0], "/normal/") {
			t.Fatalf("%+v checkpoints %v", res, tr.checkpoints())
		}
		// The prompt that reached the ratio was not sent: the act steps are the first (the Tool)
		// and the one from the checkpoint; the Summary was asked once, and counted as an attempt
		// and not as a model step.
		if len(tr.acts) != 2 || len(tr.stages[modelport.StageSummary]) != 1 || !promptHasCheckpoint(tr.acts[1]) || toolAnswers(tr.acts[1]) != 0 {
			t.Fatalf("acts %d, summary requests %d", len(tr.acts), len(tr.stages[modelport.StageSummary]))
		}
		if got := tr.val("SELECT generation_attempts_used FROM runs WHERE run_id=?", start.RunID); got != "3" {
			t.Fatalf("attempts used %s: act, summary, act", got)
		}
		if got := tr.val("SELECT COUNT(*) FROM actions WHERE run_id=? AND name='act'", start.RunID); got != "2" {
			t.Fatalf("model steps %s", got)
		}
		// The count of the candidate's before is the one that reached the ratio, verified.
		cp := tr.storedCheckpoint()
		if cp.Candidate.BeforeCount.PromptUpper == nil || *cp.Candidate.BeforeCount.PromptUpper != triggerBoundary || cp.Candidate.BeforeCount.State != "verified_exact" {
			t.Fatalf("%+v", cp.Candidate.BeforeCount)
		}
		// After the checkpoint, the step is counted again (the old count is not reused): the request
		// that was sent is one whose input digest a count was taken of.
		digest, err := tr.acts[1].LogicalInputDigest()
		if err != nil {
			t.Fatal(err)
		}
		counted := false
		for _, m := range tr.fake.Measures() {
			if d, err := m.Request.LogicalInputDigest(); err == nil && d == digest {
				counted = true
			}
		}
		if !counted {
			t.Fatal("the step from the checkpoint was sent without being counted")
		}
	})
	t.Run("one token under", func(t *testing.T) {
		tr := newTriggerRig(t, 1, harnesstest.Final(goodSummary), func(n int) int64 {
			if n == 0 {
				return 100
			}
			return triggerBoundary - 1
		})
		_, run := tr.run("trigger.under.00000001", "大きなファイルを読んで保存処理を実装する。")
		if run.Result.Status != "completed" || len(tr.checkpoints()) != 0 || len(tr.stages[modelport.StageSummary]) != 0 || len(tr.acts) != 2 || promptHasCheckpoint(tr.acts[1]) {
			t.Fatalf("%+v checkpoints %v", run.Result, tr.checkpoints())
		}
	})
	t.Run("a prompt that does not fit is compacted as ever", func(t *testing.T) {
		tr := newTriggerRig(t, 1, harnesstest.Final(goodSummary), func(n int) int64 {
			if n == 0 {
				return 100
			}
			return usableTokens + 1
		})
		_, run := tr.run("trigger.over.000000001", "大きなファイルを読んで保存処理を実装する。")
		if run.Result.Status != "completed" || len(tr.checkpoints()) != 1 {
			t.Fatalf("%+v checkpoints %v", run.Result, tr.checkpoints())
		}
	})
}

// TestAnEarlyCompactionThatCannotBeMadeLetsTheRunGoOnWithThePromptThatFitsAndIsNotTriedAgain: the
// Summary request does not fit (CapacityBlocked) or cannot be counted (unavailable): the
// prompt fits, so the Run sends it. The trigger is not tried again in that Run, and a prompt
// that later does not fit is still compacted the way a Run always did (and blocks here, for
// the same reason).
func TestAnEarlyCompactionThatCannotBeMadeLetsTheRunGoOnWithThePromptThatFitsAndIsNotTriedAgain(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stageCount int64
		stageState string
	}{
		{"the Summary request does not fit", usableTokens * 10, ""},
		{"the Summary request cannot be counted", 100, "estimated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTriggerRig(t, 2, harnesstest.Final(goodSummary), func(n int) int64 {
				switch n {
				case 0:
					return 100
				case 1:
					return triggerBoundary + 100 // the trigger: tried, and given up
				}
				return triggerBoundary + 500 // the trigger again: not tried
			})
			tr.stageCount, tr.stageState = tc.stageCount, tc.stageState
			start, run := tr.run("trigger.skip.00000001", "大きなファイルを読んで保存処理を実装する。")
			res := run.Result
			if res.Status != "completed" || res.FinalText != "done" || res.LastCheckpointID != nil || len(tr.checkpoints()) != 0 || len(tr.stages[modelport.StageSummary]) != 0 {
				t.Fatalf("%+v checkpoints %v", res, tr.checkpoints())
			}
			// The three steps were sent as they were counted, and only the first step of the
			// ratio asked for a Summary request to be counted.
			if len(tr.acts) != 3 {
				t.Fatalf("act generations %d", len(tr.acts))
			}
			stageCounts := 0
			for _, m := range tr.fake.Measures() {
				if m.Request.Rencrow.Harness.Stage != modelport.StageAct {
					stageCounts++
				}
			}
			if stageCounts != 1 {
				t.Fatalf("the trigger was tried %d times in one Run", stageCounts)
			}
			if got := tr.val("SELECT generation_attempts_used FROM runs WHERE run_id=?", start.RunID); got != "3" {
				t.Fatalf("a compaction that sent nothing spent nothing: %s", got)
			}
			reason := tr.compactionReason(start.RunID)
			if !strings.Contains(reason, `"checkpoint_id":null`) {
				t.Fatalf("%s", reason)
			}
		})
	}

	t.Run("a prompt that later does not fit is still compacted", func(t *testing.T) {
		tr := newTriggerRig(t, 2, harnesstest.Final(goodSummary), func(n int) int64 {
			switch n {
			case 0:
				return 100
			case 1:
				return triggerBoundary + 100
			}
			return usableTokens + 100
		})
		tr.stageCount = usableTokens * 10
		_, run := tr.run("trigger.later.00000001", "大きなファイルを読んで保存処理を実装する。")
		stageCounts := 0
		for _, m := range tr.fake.Measures() {
			if m.Request.Rencrow.Harness.Stage != modelport.StageAct {
				stageCounts++
			}
		}
		if run.Result.Status != "blocked" || run.Result.Code != "CAPACITY_BLOCKED" || stageCounts != 2 || len(tr.acts) != 2 {
			t.Fatalf("%+v stage counts %d acts %d", run.Result, stageCounts, len(tr.acts))
		}
	})
}

// TestAnEarlyCompactionIsOnlyMadeWhereCompactionIsEnabledAndTheCountIsVerified: a deployment that
// disables compaction never starts one early, and an estimated count of a prompt (which is no
// fit) never does either: it ends the Run as it always did.
func TestAnEarlyCompactionIsOnlyMadeWhereCompactionIsEnabledAndTheCountIsVerified(t *testing.T) {
	t.Run("compaction disabled", func(t *testing.T) {
		fake := harnesstest.NewFake()
		fake.SetMeasure(harnesstest.MeasureConfig{Limit: contextLimit})
		fake.MeasureHook = func(req modelport.MeasureRequest, res *modelport.MeasureResult) error {
			n := int64(triggerBoundary + 100)
			res.PromptLower, res.PromptUpper = &n, &n
			return nil
		}
		r := newModelRig(t, fake, nil, func(l *harnesstest.Layout) { l.Cfg["compaction"].(map[string]any)["enabled"] = false })
		info := r.openSession("trigger.disabled.open")
		start := r.startRun(info.ThreadID, "trigger.disabled.start", "質問です。")
		if run := r.waitTerminal(start.RunID); run.Result.Status != "completed" || r.count("checkpoints") != 0 || len(fake.Generates()) != 1 {
			t.Fatalf("%+v", run.Result)
		}
	})
	t.Run("an estimated count", func(t *testing.T) {
		tr := newTriggerRig(t, 1, harnesstest.Final(goodSummary), func(n int) int64 { return triggerBoundary + 100 })
		tr.fake.SetMeasure(harnesstest.MeasureConfig{State: "estimated", Lower: triggerBoundary + 100, Upper: triggerBoundary + 100, Limit: contextLimit})
		_, run := tr.run("trigger.estimated.0001", "大きなファイルを読んで保存処理を実装する。")
		if run.Result.Status != "blocked" || run.Result.Code != "BUDGET_UNVERIFIED" || len(tr.checkpoints()) != 0 || len(tr.stages[modelport.StageSummary]) != 0 {
			t.Fatalf("%+v", run.Result)
		}
	})
}

// TestAnEarlyCompactionWhoseGenerationIsUnknownEndsTheRunLikeAnyOther: a stage generation whose
// outcome is unknown cannot be skipped past (the Run does not generate again while one of its
// own is unresolved): the reduction without a model is stored, and the Run is blocked.
func TestAnEarlyCompactionWhoseGenerationIsUnknownEndsTheRunLikeAnyOther(t *testing.T) {
	tr := newTriggerRig(t, 1, harnesstest.Reply{Kind: harnesstest.KindTransportError}, func(n int) int64 {
		if n == 0 {
			return 100
		}
		return triggerBoundary + 1
	})
	_, run := tr.run("trigger.unknown.000001", "大きなファイルを読んで保存処理を実装する。")
	res := run.Result
	if res.Status != "blocked" || res.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" || len(res.UnresolvedActionIDs) != 1 || len(tr.acts) != 1 {
		t.Fatalf("%+v acts %d", res, len(tr.acts))
	}
}
