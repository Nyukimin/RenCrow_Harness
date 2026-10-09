package service_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/extensions"
	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Manual compaction (context/compact, WP06 stage 2): a system Task and Run, the result in the
// receipt, the dry run that stores nothing but that receipt.

// manualLimit is a context limit so large that nothing in these scenarios needs a compaction
// by itself: the one that happens is the one that was asked for.
const manualLimit = 1 << 20

func (r *rig) revisions(thread string) (ctxRev, ctl int64) {
	r.t.Helper()
	info := decode[protocol.SessionInfo](r.t, r.mustCall("session/get", protocol.SessionGetInput{ThreadID: thread}))
	return info.ContextRevision, info.ControlRevision
}

func (r *rig) compactInput(thread, key string, dry bool) protocol.CompactInput {
	r.t.Helper()
	c, k := r.revisions(thread)
	return protocol.CompactInput{ThreadID: thread, ExpectedContextRevision: c, ExpectedControlRevision: k, DryRun: dry, IdempotencyKey: key}
}

// compactAt asks for a manual compaction with the given input and starts its Run.
func (r *rig) compactAt(in protocol.CompactInput) (protocol.OperationAccepted, error) {
	r.t.Helper()
	res, err := r.call("context/compact", in)
	if err != nil {
		return protocol.OperationAccepted{}, err
	}
	res.Done()
	return decode[protocol.OperationAccepted](r.t, res), nil
}

func (r *rig) compact(thread, key string, dry bool) protocol.OperationAccepted {
	r.t.Helper()
	acc, err := r.compactAt(r.compactInput(thread, key, dry))
	if err != nil {
		r.t.Fatalf("context/compact: %v", err)
	}
	return acc
}

func (r *rig) receipt(id string) protocol.ReceiptRecord {
	r.t.Helper()
	return decode[protocol.ReceiptRecord](r.t, r.mustCall("receipt/get", protocol.ReceiptGetInput{ReceiptID: id}))
}

// waitReceipt waits for the receipt of a manual compaction to hold its result.
func (r *rig) waitReceipt(id string) protocol.CompactResult {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		rec := r.receipt(id)
		if rec.Stage == "terminal" {
			if rec.Result == nil || rec.Result.Type != protocol.ReceiptCompactResult {
				r.t.Fatalf("a terminal compaction receipt holds a CompactResult: %+v", rec)
			}
			res, err := protocol.Decode[protocol.CompactResult](rec.Result.Value)
			if err != nil {
				r.t.Fatal(err)
			}
			return res
		}
		if rec.Result != nil {
			r.t.Fatalf("a receipt that is not terminal holds no result: %+v", rec)
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the compaction did not end; its receipt is %s", rec.Stage)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// systemRun is the Run of the Thread's compaction Task (the only one a test makes).
func (r *rig) systemRun() string {
	r.t.Helper()
	return r.val("SELECT r.run_id FROM runs r JOIN tasks t ON t.task_id=r.task_id WHERE t.kind='compaction' ORDER BY r.rowid DESC LIMIT 1")
}

// manualScenario is a Thread whose Run has read the big file (a Tool exchange) and ended with a
// final answer, with no compaction made: the context a manual compaction has Work in.
func manualScenario(t *testing.T, stageReplies map[string]harnesstest.Reply) (*compactionRig, protocol.StartResult) {
	t.Helper()
	return manualScenarioWith(t, stageReplies, defaultRigBuild)
}

// manualScenarioWith is manualScenario on the rig that build makes.
func manualScenarioWith(t *testing.T, stageReplies map[string]harnesstest.Reply, build rigBuild) (*compactionRig, protocol.StartResult) {
	t.Helper()
	c := newCompactionRigOn(t, stageReplies, manualLimit, build)
	start, run := c.run("manual.scenario.0001", "大きなファイルを読んで保存処理を実装する。")
	if run.Result.Status != "completed" || len(c.checkpoints()) != 0 || len(c.stages[modelport.StageSummary]) != 0 {
		t.Fatalf("the scenario made a compaction by itself: %+v %v", run.Result, c.checkpoints())
	}
	return c, start
}

func tableCounts(r *rig) map[string]int {
	out := map[string]int{}
	for _, tb := range []string{"sessions", "threads", "turns", "tasks", "runs", "actions", "attempts", "evidence", "items", "context_entries", "checkpoints", "receipts",
		"queue_inputs", "events", "model_calls", "source_imports"} {
		out[tb] = r.count(tb)
	}
	return out
}

// TestAManualCompactionMakesACheckpointInASystemRunAndTheReceiptHoldsTheResult is the manual
// path of A06, A17 and A60 through the Service, the store and the driver: the Thread must be
// idle; a system Task and Run (no input, no Turn) make the Normal checkpoint; the receipt that
// context/compact named is accepted, then running, then terminal with the CompactResult; the
// next Run stands on the checkpoint.
func TestAManualCompactionMakesACheckpointInASystemRunAndTheReceiptHoldsTheResult(t *testing.T) {
	c, start := manualScenario(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)})
	thread := c.val("SELECT thread_id FROM threads")
	before := tableCounts(c.rig)
	ctxBefore, ctlBefore := c.revisions(thread)
	acts := len(c.acts)

	acc := c.compact(thread, "manual.normal.00000001", false)
	if !acc.Accepted || acc.ReceiptID == "" {
		t.Fatalf("%+v", acc)
	}
	if rec := c.receipt(acc.ReceiptID); rec.Operation != "context/compact" || rec.Stage == "terminal" && rec.Result == nil || rec.Stage != "terminal" && rec.Result != nil {
		t.Fatalf("%+v", rec)
	}
	res := c.waitReceipt(acc.ReceiptID)
	if res.Status != "executed" || res.Outcome == nil || *res.Outcome != "NormalCompacted" || res.CheckpointID == nil || res.Error != nil || res.Before == nil || res.After == nil ||
		res.SemanticBoundary == nil || res.DurableBoundary == nil || *res.SemanticBoundary > *res.DurableBoundary {
		t.Fatalf("%+v", res)
	}
	if res.Before.State != "verified_exact" || res.After.State != "verified_exact" || *res.After.PromptUpper >= *res.Before.PromptLower {
		t.Fatalf("a compaction shrinks, verified: before %+v after %+v", res.Before, res.After)
	}
	cps := c.checkpoints()
	if len(cps) != 1 || !strings.HasPrefix(cps[0], *res.CheckpointID+"/normal/") || c.val("SELECT current_checkpoint_id FROM threads") != *res.CheckpointID {
		t.Fatalf("checkpoints %v result %+v", cps, res)
	}

	// The Run is a system Run: its own Task (kind compaction, no Turn, no input), no model step,
	// the one Summary generation, completed with the checkpoint named and no final message.
	runID := c.systemRun()
	if got := c.val("SELECT t.kind||'/'||COALESCE(t.origin_turn_id,'noturn')||'/'||t.status FROM tasks t JOIN runs r ON r.task_id=t.task_id WHERE r.run_id=?", runID); got != "compaction/noturn/run_ended" {
		t.Fatal(got)
	}
	info := decode[protocol.RunInfo](t, c.mustCall("run/get", protocol.RunGetInput{RunID: runID}))
	rr := info.Result
	if !info.Terminal || rr.Status != "completed" || rr.Code != "COMPACTION_COMMITTED" || rr.FinalMessageID != nil || rr.FinalText != "" || rr.Resumable ||
		rr.LastCheckpointID == nil || *rr.LastCheckpointID != *res.CheckpointID || len(rr.UnresolvedActionIDs) != 0 || rr.Verification.Status != "not_run" {
		t.Fatalf("%+v", rr)
	}
	if info.GenerationAttemptsUsed != 1 || c.val("SELECT COUNT(*) FROM actions WHERE run_id=? AND name='act'", runID) != "0" || len(c.stages[modelport.StageSummary]) != 1 || len(c.acts) != acts {
		t.Fatalf("attempts %d, summary requests %d, act generations %d -> %d", info.GenerationAttemptsUsed, len(c.stages[modelport.StageSummary]), acts, len(c.acts))
	}
	if info.Result.Resumable || c.val("SELECT COALESCE(active_run_id,'none') FROM threads") != "none" {
		t.Fatal("the Thread is idle again")
	}
	// The commit moved the context revision by one and nothing else; no input was applied.
	ctxAfter, ctlAfter := c.revisions(thread)
	if ctxAfter != ctxBefore+1 || ctlAfter != ctlBefore || c.count("items") != before["items"] || c.count("context_entries") != before["context_entries"] {
		t.Fatalf("context %d -> %d, control %d -> %d", ctxBefore, ctxAfter, ctlBefore, ctlAfter)
	}
	if c.count("turns") != before["turns"] || c.count("tasks") != before["tasks"]+1 || c.count("runs") != before["runs"]+1 || c.count("receipts") != before["receipts"]+1 {
		t.Fatalf("%v -> %v", before, tableCounts(c.rig))
	}

	// The events of the system Run, in order, all naming the one receipt of the operation.
	var types []string
	for _, ev := range c.threadEvents(thread) {
		if ev.TaskID != nil && c.val("SELECT kind FROM tasks WHERE task_id=?", *ev.TaskID) == "compaction" {
			types = append(types, ev.Type)
			if ev.ReceiptID == nil || *ev.ReceiptID != acc.ReceiptID {
				t.Fatalf("%s names receipt %v, not the operation's", ev.Type, ev.ReceiptID)
			}
		}
	}
	if got := strings.Join(types, ","); got != "task.created,run.started,action.prepared,action.dispatch_started,model.attempt_started,model.requested,model.completed,checkpoint.committed,run.terminal" {
		t.Fatal(got)
	}

	// The Thread's next Run stands on the checkpoint: no second compaction, the Summary and the
	// boundary in its prompt.
	next := c.startNext(thread, "manual.next.0000000001", "続けてください。")
	if r := c.waitTerminal(next.RunID); r.Result.Status != "completed" || len(c.stages[modelport.StageSummary]) != 1 {
		t.Fatalf("%+v", r.Result)
	}
	if p := promptOf(lastAct(c)); !strings.Contains(p, "user:RENCROW_CONTEXT_BOUNDARY_V1") || !strings.Contains(p, "assistant:RENCROW_ACCEPTED_SUMMARY_V1") {
		t.Fatalf("the next Run is not prompted from the checkpoint:\n%.1500s", p)
	}
	_ = start
}

func TestManualCompactionCheckpointCommitSurvivesLateStopAndDeadline(t *testing.T) {
	for _, tc := range []struct {
		name            string
		deadlineSeconds int64
		stopAfterCommit bool
	}{
		{name: "late cancellation", stopAfterCommit: true},
		{name: "deadline after commit", deadlineSeconds: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c *compactionRig
			var thread, committedCheckpoint string
			lateStop := onPoint(extensions.AfterCompact, func(_ context.Context, in extensions.HookInput, _ extensions.Recorder) (extensions.HookResult, error) {
				committedCheckpoint = c.val("SELECT COALESCE(current_checkpoint_id,'') FROM threads WHERE thread_id=?", thread)
				if committedCheckpoint == "" || c.val("SELECT COUNT(*) FROM checkpoints WHERE checkpoint_id=?", committedCheckpoint) != "1" {
					return extensions.HookResult{}, errors.New("the checkpoint was not stored before after_compact")
				}
				if tc.stopAfterCommit {
					_, control := c.revisions(thread)
					if got := c.interrupt(in.RunID, control, "manual.postcommit.cancel.000001").Code; got != "CANCEL_REQUESTED" {
						return extensions.HookResult{}, errors.New("the late stop was not recorded")
					}
				} else {
					time.Sleep(1100 * time.Millisecond)
				}
				return extensions.Continue(), nil
			})
			hs := hookSetup{extra: []extensions.Named{lateStop}, hard: 5 * time.Second}
			c, _ = manualScenarioWith(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}, hookRigBuild(hs))
			thread = c.val("SELECT thread_id FROM threads")
			if tc.deadlineSeconds > 0 {
				// The Service retains this Deployment pointer; lowering the host cap after the
				// setup Run gives just the compaction Run a short real deadline.
				c.dep.Config.Limits.DeadlineSeconds = tc.deadlineSeconds
			}
			acc := c.compact(thread, "manual.postcommit."+strings.ReplaceAll(tc.name, " ", "."), false)
			res := c.waitReceipt(acc.ReceiptID)
			if res.Status != "executed" || res.Outcome == nil || *res.Outcome != "NormalCompacted" || res.CheckpointID == nil || *res.CheckpointID != committedCheckpoint {
				t.Fatalf("the committed checkpoint remains the compaction result: %+v checkpoint=%q", res, committedCheckpoint)
			}
			run := decode[protocol.RunInfo](t, c.mustCall("run/get", protocol.RunGetInput{RunID: c.systemRun()}))
			if !run.Terminal || run.Result.Status != "completed" || run.Result.Code != "COMPACTION_COMMITTED" || run.Result.FinalMessageID != nil || run.Result.FinalText != "" ||
				run.Result.LastCheckpointID == nil || *run.Result.LastCheckpointID != committedCheckpoint || c.val("SELECT current_checkpoint_id FROM threads WHERE thread_id=?", thread) != committedCheckpoint {
				t.Fatalf("the system Run retains its committed checkpoint result: %+v", run.Result)
			}
		})
	}
}

// TestARetriedManualCompactionIsAnsweredByTheFirstReceiptWhateverMovedSince is A07 through the
// idempotency key: the client lost the answer, asks again with the same key and the revisions
// it had (which the commit itself moved), and gets the same receipt and the same result, with
// no second Run and no second checkpoint; another payload under the key is a conflict.
func TestARetriedManualCompactionIsAnsweredByTheFirstReceiptWhateverMovedSince(t *testing.T) {
	c, _ := manualScenario(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)})
	thread := c.val("SELECT thread_id FROM threads")
	in := c.compactInput(thread, "manual.retry.00000001", false)
	first, err := c.compactAt(in)
	if err != nil {
		t.Fatal(err)
	}
	want := c.waitReceipt(first.ReceiptID)
	counts := tableCounts(c.rig)

	// The same request again: the revisions in it are the old ones.
	again, err := c.compactAt(in)
	if err != nil || again.ReceiptID != first.ReceiptID {
		t.Fatalf("%+v %v", again, err)
	}
	if got := c.waitReceipt(again.ReceiptID); *got.CheckpointID != *want.CheckpointID || got.Status != want.Status {
		t.Fatalf("%+v vs %+v", got, want)
	}
	if now := tableCounts(c.rig); len(c.checkpoints()) != 1 || now["runs"] != counts["runs"] || now["receipts"] != counts["receipts"] || now["events"] != counts["events"] || len(c.stages[modelport.StageSummary]) != 1 {
		t.Fatalf("a retry made something: %v -> %v", counts, now)
	}
	// The same key for another request (another revision, or a dry run) is a conflict.
	other := in
	other.ExpectedControlRevision++
	if _, err := c.compactAt(other); protocol.CodeOf(err) != protocol.CodeIdempotencyConflict {
		t.Fatalf("%v", err)
	}
	dry := in
	dry.DryRun = true
	if _, err := c.compactAt(dry); protocol.CodeOf(err) != protocol.CodeIdempotencyConflict {
		t.Fatalf("%v", err)
	}
	// And the moved revisions are refused for a key that is new.
	if _, err := c.compactAt(protocol.CompactInput{ThreadID: thread, ExpectedContextRevision: in.ExpectedContextRevision, ExpectedControlRevision: in.ExpectedControlRevision, IdempotencyKey: "manual.retry.00000002"}); protocol.CodeOf(err) != protocol.CodeRevisionConflict {
		t.Fatalf("%v", err)
	}
}

// TestADryRunStoresOnlyTheReceiptThatHoldsItsAnswer: a dry run counts the context and says what
// it found, with no generation (the model got a count and nothing else), no Task, Run, Evidence,
// event, checkpoint or revision change, on a busy Thread too; its retry is answered by the same
// receipt, and the revisions it names must be the Thread's.
func TestADryRunStoresOnlyTheReceiptThatHoldsItsAnswer(t *testing.T) {
	c, _ := manualScenario(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)})
	thread := c.val("SELECT thread_id FROM threads")
	before := tableCounts(c.rig)
	generates := len(c.fake.Generates())
	ctxRev, ctlRev := c.revisions(thread)

	acc := c.compact(thread, "manual.dry.000000001", true)
	res := c.waitReceipt(acc.ReceiptID)
	if res.Status != "dry_run" || res.Outcome != nil || res.CheckpointID != nil || res.After != nil || res.Error != nil || res.SemanticBoundary != nil || res.DurableBoundary != nil {
		t.Fatalf("%+v", res)
	}
	if res.Before == nil || res.Before.State != "verified_exact" || res.Before.PromptUpper == nil || res.RequiredMinimumTokens != nil || res.AvailableTokens != nil {
		t.Fatalf("a dry run says what the context counts, and claims nothing of a candidate it did not make: %+v", res)
	}
	now := tableCounts(c.rig)
	for tb, n := range before {
		want := n
		if tb == "receipts" {
			want++
		}
		if now[tb] != want {
			t.Errorf("table %s: %d -> %d", tb, n, now[tb])
		}
	}
	if c.val("SELECT stage||'/'||operation FROM receipts WHERE receipt_id=?", acc.ReceiptID) != "terminal/context/compact" {
		t.Fatal("the receipt of a dry run is terminal at once")
	}
	if len(c.fake.Generates()) != generates || len(c.stages[modelport.StageSummary]) != 0 {
		t.Fatal("a dry run generated")
	}
	if a, b := c.revisions(thread); a != ctxRev || b != ctlRev || len(c.checkpoints()) != 0 {
		t.Fatal("a dry run moved the Thread")
	}
	// The retry is the same receipt, and stores nothing more.
	in := c.compactInput(thread, "manual.dry.000000001", true)
	again, err := c.compactAt(in)
	if err != nil || again.ReceiptID != acc.ReceiptID || c.count("receipts") != now["receipts"] {
		t.Fatalf("%+v %v", again, err)
	}
	// The revisions it names must be the Thread's: the answer is about that context.
	stale := c.compactInput(thread, "manual.dry.000000002", true)
	stale.ExpectedContextRevision++
	if _, err := c.compactAt(stale); protocol.CodeOf(err) != protocol.CodeRevisionConflict || c.count("receipts") != now["receipts"] {
		t.Fatalf("%v", err)
	}
}

// TestADryRunThatCannotCountSaysItIsUnavailableAndNeverPassesAsAResult: a count that is not
// verified is status unavailable with BUDGET_UNVERIFIED, and no checkpoint-bearing result.
func TestADryRunThatCannotCountSaysItIsUnavailableAndNeverPassesAsAResult(t *testing.T) {
	c, _ := manualScenario(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)})
	thread := c.val("SELECT thread_id FROM threads")
	c.fake.SetMeasure(harnesstest.MeasureConfig{State: "estimated", Lower: 100, Upper: 100, Limit: manualLimit})
	res := c.waitReceipt(c.compact(thread, "manual.dry.unverified.1", true).ReceiptID)
	if res.Status != "unavailable" || res.Outcome != nil || res.CheckpointID != nil || res.Before != nil || res.Error == nil || res.Error.Code != "BUDGET_UNVERIFIED" {
		t.Fatalf("%+v", res)
	}
}

// TestAManualCompactionIsNeverQueuedBehindARunAndNeverSeesTheOldRevisions: BUSY while the Thread
// has an active Run (nothing written, the Thread's Run goes on), REVISION_CONFLICT for
// revisions that are not the Thread's, FORBIDDEN for what the caller may not touch.
func TestAManualCompactionIsNeverQueuedBehindARunAndNeverSeesTheOldRevisions(t *testing.T) {
	c := newCompactionRig(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}, manualLimit)
	info := c.openSession("manual.busy.open")
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.fake.OnMeasure = func(ctx context.Context) {
		once.Do(func() { close(reached) })
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	start := c.startRun(info.ThreadID, "manual.busy.start", "大きなファイルを読んで保存処理を実装する。")
	<-reached
	before := tableCounts(c.rig)
	if _, err := c.compactAt(c.compactInput(info.ThreadID, "manual.busy.key.001", false)); protocol.CodeOf(err) != protocol.CodeBusy {
		t.Fatalf("%v", err)
	}
	if now := tableCounts(c.rig); now["runs"] != before["runs"] || now["tasks"] != before["tasks"] || now["receipts"] != before["receipts"] {
		t.Fatalf("a refused compaction wrote: %v -> %v", before, now)
	}
	close(release)
	if r := c.waitTerminal(start.RunID); r.Result.Status != "completed" {
		t.Fatalf("%+v", r.Result)
	}
	// Idle now: the same compaction is accepted, and a stale revision of either kind is not.
	good := c.compactInput(info.ThreadID, "manual.busy.key.002", false)
	for name, in := range map[string]protocol.CompactInput{
		"context revision": {ThreadID: good.ThreadID, ExpectedContextRevision: good.ExpectedContextRevision - 1, ExpectedControlRevision: good.ExpectedControlRevision, IdempotencyKey: "manual.busy.key.003"},
		"control revision": {ThreadID: good.ThreadID, ExpectedContextRevision: good.ExpectedContextRevision, ExpectedControlRevision: good.ExpectedControlRevision + 1, IdempotencyKey: "manual.busy.key.004"},
	} {
		if _, err := c.compactAt(in); protocol.CodeOf(err) != protocol.CodeRevisionConflict {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.compactAt(protocol.CompactInput{ThreadID: "thr_00000000-0000-7000-8000-000000000001", ExpectedContextRevision: 0, ExpectedControlRevision: 0, IdempotencyKey: "manual.busy.key.005"}); protocol.CodeOf(err) != protocol.CodeForbidden {
		t.Fatalf("%v", err)
	}
	acc, err := c.compactAt(good)
	if err != nil {
		t.Fatal(err)
	}
	if res := c.waitReceipt(acc.ReceiptID); res.Status != "executed" || res.Outcome == nil || *res.Outcome != "NormalCompacted" {
		t.Fatalf("%+v", res)
	}
}

// TestAThreadWhoseManualCompactionIsRunningRefusesATurnAndKeepsAnAppendedInputForTheNextRun:
// the manual compaction is a Run of the Thread: turn/start is BUSY, an input appended to it is
// kept (never lost, never applied by the compaction), and it is applied by the next work Run,
// whose prompt stands on the new checkpoint and carries it.
func TestAThreadWhoseManualCompactionIsRunningRefusesATurnAndKeepsAnAppendedInputForTheNextRun(t *testing.T) {
	c, _ := manualScenario(t, map[string]harnesstest.Reply{modelport.StageSummary: {Kind: harnesstest.KindHang}})
	thread := c.val("SELECT thread_id FROM threads")
	reached := make(chan struct{})
	var once sync.Once
	c.fake.OnGenerate = func(_ context.Context, req modelport.ChatRequest) {
		if req.Rencrow.Harness.Stage == modelport.StageSummary {
			once.Do(func() { close(reached) })
		}
	}
	acc := c.compact(thread, "manual.running.000001", false)
	<-reached
	runID := c.systemRun()
	if _, err := c.call("turn/start", startParams(thread, "manual.running.start1", "割り込み")); protocol.CodeOf(err) != protocol.CodeBusy {
		t.Fatalf("%v", err)
	}
	_, ctl := c.revisions(thread)
	rec := c.appendInput(thread, runID, "後で読んでください。", "next_turn", ctl, "manual.running.append1")
	if rec.DeliveryState != "queued" && rec.DeliveryState != "deferred" {
		t.Fatalf("%+v", rec)
	}
	if rec := c.receipt(acc.ReceiptID); rec.Stage != "running" || rec.Result != nil {
		t.Fatalf("the operation of a driven system Run is running: %+v", rec)
	}
	if got := c.val("SELECT COUNT(*) FROM context_entries WHERE message_id=?", rec2message(t, c, thread)); got != "0" {
		t.Fatal("the compaction applied an input")
	}
	// Stop it: cancelled, and the queued input is still queued.
	c.interrupt(runID, ctl, "manual.running.stop001")
	res := c.waitReceipt(acc.ReceiptID)
	if res.Status != "cancelled" || res.Outcome != nil || res.CheckpointID != nil || res.Error == nil || res.Error.Code != "CANCELLED" {
		t.Fatalf("%+v", res)
	}
	if got := c.val("SELECT delivery_state FROM queue_inputs WHERE thread_id=?", thread); got == "applied" {
		t.Fatal("a stopped compaction applied the input")
	}
	c.fake.OnGenerate = nil
	next := c.startNext(thread, "manual.running.next001", "続けてください。")
	c.waitTerminal(next.RunID)
	if got := c.val("SELECT delivery_state FROM queue_inputs WHERE thread_id=?", thread); got != "applied" {
		t.Fatalf("the next Run applies the input: %s", got)
	}
}

func rec2message(t *testing.T, c *compactionRig, thread string) string {
	t.Helper()
	return c.val("SELECT message_id FROM queue_inputs WHERE thread_id=? ORDER BY rowid DESC LIMIT 1", thread)
}

// TestAManualCompactionStoppedMidGenerationIsCancelledAndStoresNothing: the stop is a
// cancellation, with the Summary generation unknown on the Run's record, no checkpoint, and the
// Thread idle again.
func TestAManualCompactionStoppedMidGenerationIsCancelledAndStoresNothing(t *testing.T) {
	c, _ := manualScenario(t, map[string]harnesstest.Reply{modelport.StageSummary: {Kind: harnesstest.KindHang}})
	thread := c.val("SELECT thread_id FROM threads")
	reached := make(chan struct{})
	var once sync.Once
	c.fake.OnGenerate = func(_ context.Context, req modelport.ChatRequest) {
		if req.Rencrow.Harness.Stage == modelport.StageSummary {
			once.Do(func() { close(reached) })
		}
	}
	_, ctl := c.revisions(thread)
	acc := c.compact(thread, "manual.cancel.0000001", false)
	<-reached
	runID := c.systemRun()
	if r := c.interrupt(runID, ctl, "manual.cancel.stop0001"); r.Code != "CANCEL_REQUESTED" {
		t.Fatalf("%+v", r)
	}
	res := c.waitReceipt(acc.ReceiptID)
	if res.Status != "cancelled" || res.Outcome != nil || res.CheckpointID != nil || res.Error == nil || res.Error.Code != "CANCELLED" {
		t.Fatalf("%+v", res)
	}
	run := c.waitTerminal(runID)
	if run.Result.Status != "cancelled" || len(run.Result.UnresolvedActionIDs) != 1 || run.GenerationAttemptsUnknown != 1 || len(c.checkpoints()) != 0 || c.val("SELECT COALESCE(active_run_id,'none') FROM threads") != "none" {
		t.Fatalf("%+v %+v", run.Result, run)
	}
}

// TestAManualCompactionWhoseSummaryFailsIsEmergencyOrCapacityBlockedAndNeverGuessed: a Summary the
// Host cannot use sends a manual compaction to the reduction that needs no model, exactly as
// an automatic one: a context with Tool output to put behind a reference makes an Emergency
// checkpoint; one with nothing to put behind a reference is CapacityBlocked, executed, with no
// checkpoint, and the Thread untouched.
func TestAManualCompactionWhoseSummaryFailsIsEmergencyOrCapacityBlockedAndNeverGuessed(t *testing.T) {
	t.Run("a Tool answer to put behind a reference", func(t *testing.T) {
		c, _ := manualScenario(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final("not json")})
		thread := c.val("SELECT thread_id FROM threads")
		res := c.waitReceipt(c.compact(thread, "manual.emergency.00001", false).ReceiptID)
		if res.Status != "executed" || res.Outcome == nil || *res.Outcome != "EmergencyCompacted" || res.CheckpointID == nil || res.Before == nil || res.After == nil {
			t.Fatalf("%+v", res)
		}
		if cps := c.checkpoints(); len(cps) != 1 || !strings.Contains(cps[0], "/emergency/") || len(c.stages[modelport.StageSummary]) != 1 {
			t.Fatalf("%v", cps)
		}
	})
	t.Run("nothing to put behind a reference", func(t *testing.T) {
		fake := harnesstest.NewFake()
		fake.SetMeasure(harnesstest.MeasureConfig{Limit: manualLimit})
		fake.SetReply(harnesstest.Reply{Dynamic: func(req modelport.ChatRequest) harnesstest.Reply {
			if req.Rencrow.Harness.Stage != modelport.StageAct {
				return harnesstest.Final("not json")
			}
			return harnesstest.Final("answered")
		}})
		r, _ := modelRig(t, fake)
		info := r.openSession("manual.capacity.open")
		start := r.startRun(info.ThreadID, "manual.capacity.start", "質問です。")
		if run := r.waitTerminal(start.RunID); run.Result.Status != "completed" {
			t.Fatalf("%+v", run.Result)
		}
		before := tableCounts(r)
		res := r.waitReceipt(r.compact(info.ThreadID, "manual.capacity.00001", false).ReceiptID)
		if res.Status != "executed" || res.Outcome == nil || *res.Outcome != "CapacityBlocked" || res.CheckpointID != nil || res.After != nil || res.Error == nil || res.Error.Code != "CAPACITY_BLOCKED" {
			t.Fatalf("%+v", res)
		}
		run := decode[protocol.RunInfo](t, r.mustCall("run/get", protocol.RunGetInput{RunID: r.systemRun()}))
		if run.Result.Status != "blocked" || run.Result.Code != "CAPACITY_BLOCKED" || r.count("checkpoints") != 0 || r.count("items") != before["items"] || r.count("context_entries") != before["context_entries"] {
			t.Fatalf("%+v", run.Result)
		}
	})
}

// TestAManualCompactionThatCannotVerifyTheCountIsUnavailableAndStoresNothing: the live prompt's
// count is not verified, so the Run is blocked with BUDGET_UNVERIFIED and the receipt says
// unavailable: no stage request, no checkpoint.
func TestAManualCompactionThatCannotVerifyTheCountIsUnavailableAndStoresNothing(t *testing.T) {
	c, _ := manualScenario(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)})
	thread := c.val("SELECT thread_id FROM threads")
	c.fake.SetMeasure(harnesstest.MeasureConfig{State: "estimated", Lower: 100, Upper: 100, Limit: manualLimit})
	res := c.waitReceipt(c.compact(thread, "manual.unverified.0001", false).ReceiptID)
	if res.Status != "unavailable" || res.Outcome != nil || res.CheckpointID != nil || res.Error == nil || res.Error.Code != "BUDGET_UNVERIFIED" || len(c.checkpoints()) != 0 || len(c.stages[modelport.StageSummary]) != 0 {
		t.Fatalf("%+v", res)
	}
}

// TestACommitOfAManualCompactionWhoseAnswerIsLostOrWhichNeverHappenedIsResolved is A07 for the
// manual path: a commit that took effect and lost its answer is found again by its issued ID
// and its bytes (executed, the checkpoint named, one checkpoint); one that did not take effect
// is RestartRequired (executed, no checkpoint named); and asking again with the key answers
// from the same receipt without a second checkpoint.
func TestACommitOfAManualCompactionWhoseAnswerIsLostOrWhichNeverHappenedIsResolved(t *testing.T) {
	for _, tc := range []struct {
		name, point string
		outcome     string
		checkpoints int
	}{
		{"the answer was lost", "checkpoint.after_commit", "NormalCompacted", 1},
		{"it did not take effect", "checkpoint.before_commit", "RestartRequired", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			armed := false
			c := newCompactionRigWith(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}, manualLimit, func(r *rig) {
				r.fault = func(point string) error {
					if armed && point == tc.point {
						return errors.New("the disk is gone")
					}
					return nil
				}
			})
			if _, run := c.run("manual.fault.scenario.1", "大きなファイルを読んで保存処理を実装する。"); run.Result.Status != "completed" {
				t.Fatalf("%+v", run.Result)
			}
			thread := c.val("SELECT thread_id FROM threads")
			armed = true
			in := c.compactInput(thread, "manual.fault.key.000001", false)
			acc, err := c.compactAt(in)
			if err != nil {
				t.Fatal(err)
			}
			res := c.waitReceipt(acc.ReceiptID)
			if res.Status != "executed" || res.Outcome == nil || *res.Outcome != tc.outcome || len(c.checkpoints()) != tc.checkpoints {
				t.Fatalf("%+v checkpoints %v", res, c.checkpoints())
			}
			if tc.checkpoints == 1 && (res.CheckpointID == nil || res.Error != nil) {
				t.Fatalf("%+v", res)
			}
			if tc.checkpoints == 0 {
				run := decode[protocol.RunInfo](t, c.mustCall("run/get", protocol.RunGetInput{RunID: c.systemRun()}))
				if res.CheckpointID != nil || res.Error == nil || res.Error.Code != "PERSISTENCE_UNCERTAIN" || run.Result.Status != "restart_required" {
					t.Fatalf("a commit that is not known to have happened names no checkpoint: %+v %+v", res, run.Result)
				}
			}
			armed = false
			again, err := c.compactAt(in)
			if err != nil || again.ReceiptID != acc.ReceiptID || len(c.checkpoints()) != tc.checkpoints || len(c.stages[modelport.StageSummary]) != 1 {
				t.Fatalf("%+v %v checkpoints %v", again, err, c.checkpoints())
			}
		})
	}
}

// TestAManualCompactionWhoseProcessDiedIsSettledAndItsReceiptEnds: the process that ran the
// compaction stopped; the next driver of the Thread settles the system Run, and the receipt
// that was running becomes terminal with a result that says what is known: cancelled when the
// Run left no checkpoint, executed with the checkpoint when its commit had been made.
func TestAManualCompactionWhoseProcessDiedIsSettledAndItsReceiptEnds(t *testing.T) {
	c := newReadOnlyCompactionRig(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}, manualLimit)
	start, run := c.run("manual.crash.scenario.1", "大きなファイルを読んで保存処理を実装する。")
	if run.Result.Status != "completed" {
		t.Fatalf("%+v", run.Result)
	}
	thread := start.ThreadID
	reached := make(chan struct{})
	var once sync.Once
	hang := true
	c.fake.SetReply(harnesstest.Reply{Dynamic: func(req modelport.ChatRequest) harnesstest.Reply {
		if st := req.Rencrow.Harness.Stage; st != modelport.StageAct {
			c.stages[st] = append(c.stages[st], req)
			if hang {
				once.Do(func() { close(reached) })
				return harnesstest.Reply{Kind: harnesstest.KindHang}
			}
			return harnesstest.Final(goodSummary)
		}
		c.acts = append(c.acts, req)
		return harnesstest.Final("carried on")
	}})
	acc := c.compact(thread, "manual.crash.key.0000001", false)
	<-reached
	if got := c.val("SELECT phase FROM runs WHERE run_id=?", c.systemRun()); got != "Compacting" {
		t.Fatalf("the Run is where the kill left it: %s", got)
	}
	svc2, conn2, rec2 := c.restart()
	hang = false
	_, again := c.takeOver(svc2, conn2, rec2, thread, "manual.crash.again.0001")
	_ = again
	c.rig.svc, c.rig.conn = svc2, conn2
	res := c.waitReceipt(acc.ReceiptID)
	old := resultOfRun(t, svc2, conn2, c.systemRun())
	if res.Status != "unavailable" || res.Outcome != nil || res.CheckpointID != nil || res.Error == nil || res.Error.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" ||
		resultKey(old.Result) != "blocked/MODEL_GENERATION_OUTCOME_UNKNOWN/resumable/unresolved" {
		t.Fatalf("%+v %s", res, resultKey(old.Result))
	}
	mustHandle(t, svc2, conn2, "service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
	svc2.Quiesce()
}

// TestAManualCompactionIsRefusedWhereTheProcessCannotMakeIt: a deployment that disables
// compaction (and a process with no model port) refuses context/compact with UNSUPPORTED_CONTRACT
// carrying the capability's own reason, and writes nothing.
func TestAManualCompactionIsRefusedWhereTheProcessCannotMakeIt(t *testing.T) {
	fake := harnesstest.NewFake()
	r := newModelRig(t, fake, nil, func(l *harnesstest.Layout) { l.Cfg["compaction"].(map[string]any)["enabled"] = false })
	info := r.openSession("manual.disabled.open")
	before := tableCounts(r)
	for _, dry := range []bool{false, true} {
		_, err := r.compactAt(protocol.CompactInput{ThreadID: info.ThreadID, ExpectedContextRevision: 0, ExpectedControlRevision: 0, DryRun: dry, IdempotencyKey: "manual.disabled.key.0001"})
		wantCode(t, err, protocol.CodeUnsupportedContract)
		if !strings.Contains(err.Error(), "compaction.enabled=false") {
			t.Fatalf("%v", err)
		}
	}
	for _, c := range decode[protocol.CapabilitiesResult](t, r.mustCall("service/capabilities", protocol.EmptyInput{})).Capabilities {
		if c.Name == "context/compact" && (c.Status != "unavailable" || c.Reason == nil || !strings.Contains(*c.Reason, "compaction.enabled=false")) {
			t.Fatalf("%+v", c)
		}
	}
	if now := tableCounts(r); now["receipts"] != before["receipts"] || now["tasks"] != before["tasks"] {
		t.Fatalf("%v -> %v", before, now)
	}
}

// TestADryRunThatCouldNotReachTheModelSideRecordsNothingAndMayBeAskedAgain: not being able to count
// is no answer about the context; the same request, asked again, is answered when the model side is
// back (a receipt that held the non-answer would give it to every retry of the key).
func TestADryRunThatCouldNotReachTheModelSideRecordsNothingAndMayBeAskedAgain(t *testing.T) {
	c, _ := manualScenario(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)})
	thread := c.val("SELECT thread_id FROM threads")
	in := c.compactInput(thread, "manual.dry.transient.01", true)
	receipts := c.count("receipts")
	c.fake.DescribeErr = errors.New("the Gateway is down")
	_, err := c.compactAt(in)
	if protocol.CodeOf(err) != protocol.CodeBudgetUnverified || !strings.Contains(err.Error(), "nothing was recorded") || c.count("receipts") != receipts {
		t.Fatalf("%v (%d receipts)", err, c.count("receipts"))
	}
	c.fake.DescribeErr = nil
	acc, err := c.compactAt(in)
	if err != nil {
		t.Fatal(err)
	}
	if res := c.waitReceipt(acc.ReceiptID); res.Status != "dry_run" || res.Before == nil {
		t.Fatalf("%+v", res)
	}
}

// TestADryRunLooksAtTheSourcesTheCheckpointNames: a checkpoint whose source is no longer what it
// names is a finding of the dry run (unavailable, INTEGRITY_BLOCKED), not a clean answer.
func TestADryRunLooksAtTheSourcesTheCheckpointNames(t *testing.T) {
	c, thread, cp := compacted(t)
	evidence := c.checkpointByID(cp).Candidate.ObservationInventory[0].EvidenceID
	c.exec("DROP TRIGGER evidence_sealed_no_update")
	c.exec("UPDATE evidence SET raw_hash=? WHERE evidence_id=?", strings.Repeat("a", 64), evidence)
	res := c.waitReceipt(c.compact(thread, "manual.dry.sources.001", true).ReceiptID)
	if res.Status != "unavailable" || res.Error == nil || res.Error.Code != "INTEGRITY_BLOCKED" || res.Before != nil {
		t.Fatalf("%+v", res)
	}
}
