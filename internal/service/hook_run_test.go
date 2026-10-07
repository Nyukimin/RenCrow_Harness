package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/extensions"
	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The host's fixed hooks at before_tool and after_tool (HOST_ASSETS section 4), through a
// Run: where they are called relative to the dispatch-start, what a deny, a failure and a
// timeout do before a call and after one, and that a hook leaves only identifiers behind.

type hookSetup struct {
	// configured is extensions.hooks of the configuration.
	configured []string
	// extra are callbacks registered beside them; hard is the longest a callback may take.
	extra []extensions.Named
	hard  time.Duration
}

func extRig(t *testing.T, fake *harnesstest.Fake, hs hookSetup) (*rig, *recorder) {
	t.Helper()
	layout := harnesstest.NewLayout(t, harnesstest.Options{CreateData: true})
	toolConfig{}.edit(layout)
	hooks := make([]any, 0, len(hs.configured))
	for _, h := range hs.configured {
		hooks = append(hooks, h)
	}
	layout.Cfg["extensions"] = m{"enabled": false, "trusted_workspace_roots": []any{}, "skill_roots": []any{}, "hooks": hooks}
	layout.Write()
	dep, err := config.Load(layout.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Init(context.Background(), dep.DataRoot); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, layout: layout, dep: dep, model: fake, extraHooks: hs.extra, hookHard: hs.hard}
	r.store, r.svc = r.process()
	rec := &recorder{}
	r.conn = r.svc.NewConn(rec)
	return r, rec
}

// onPoint makes a callback that acts only at one point and continues everywhere else.
func onPoint(p extensions.HookPoint, act extensions.Callback) extensions.Named {
	return extensions.Named{Name: "test-" + string(p), Call: func(ctx context.Context, in extensions.HookInput, rec extensions.Recorder) (extensions.HookResult, error) {
		if in.Hook != p {
			return extensions.Continue(), nil
		}
		return act(ctx, in, rec)
	}}
}

func mustParams(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// toolDispatchStarts is how many of the Run's Tool calls were dispatched (the Run's own
// generations are dispatched too, and are not counted).
func toolDispatchStarts(r *rig, thread, runID string) int {
	r.t.Helper()
	tools := map[string]bool{}
	for _, id := range strings.Split(r.val("SELECT COALESCE(group_concat(action_id), '') FROM actions WHERE run_id=? AND kind='tool'", runID), ",") {
		tools[id] = true
	}
	n := 0
	for _, e := range r.threadEvents(thread) {
		if e.Type != protocol.EventActionDispatchStarted || e.RunID == nil || *e.RunID != runID {
			continue
		}
		var p protocol.ActionDispatchStartedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			r.t.Fatal(err)
		}
		if tools[p.ActionID] {
			n++
		}
	}
	return n
}

func toolAttemptStates(r *rig, runID string) string {
	return r.val("SELECT COALESCE(group_concat(state), '') FROM (SELECT a.state AS state FROM attempts a JOIN actions ac ON ac.action_id=a.action_id WHERE ac.run_id=? AND ac.kind='tool' ORDER BY ac.rowid)", runID)
}

func (r *rig) auditRecords(runID string) []map[string]any {
	r.t.Helper()
	var out []map[string]any
	for _, id := range strings.Split(r.val("SELECT COALESCE(group_concat(evidence_id), '') FROM (SELECT evidence_id FROM evidence WHERE run_id=? AND json_extract(metadata_json,'$.purpose')='hook_audit' ORDER BY rowid)", runID), ",") {
		if id == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(r.evidence(id)), &rec); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, rec)
	}
	return out
}

// toolRecords are the audit records of the Tool points (the Run's model and end points have
// their own records, tested with them).
func toolRecords(recs []map[string]any) []map[string]any {
	var out []map[string]any
	for _, rec := range recs {
		if h, _ := rec["hook"].(string); h == "before_tool" || h == "after_tool" {
			out = append(out, rec)
		}
	}
	return out
}

// TestAuditMetadataRecordsOnlyIdentifiersAroundEachToolCall: with the one hook the
// configuration may name, a call that reads a file leaves two private records at the Tool
// points, one before the dispatch and one after the end, naming the Run, the Action and the
// revisions: no file name, no content, no argument. The Run ends as it would without the hook.
func TestAuditMetadataRecordsOnlyIdentifiersAroundEachToolCall(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(calls(tc("c1", "file.read", readArgs("secret-name.txt", 0, 100, 100))), harnesstest.Final("done"))
	r, _ := extRig(t, fake, hookSetup{configured: []string{extensions.AuditMetadata}})
	r.write("secret-name.txt", "the content of the file\n")
	info := r.openSession("hook.audit.open.0000001")
	start := r.startRun(info.ThreadID, "hook.audit.start.000001", "read it")
	run := r.waitTerminal(start.RunID)
	if run.Result.Status != "completed" || run.Result.FinalText != "done" {
		t.Fatalf("%+v", run.Result)
	}
	recs := toolRecords(r.auditRecords(start.RunID))
	if len(recs) != 2 {
		t.Fatalf("%d records: %v", len(recs), recs)
	}
	action := r.val("SELECT action_id FROM actions WHERE run_id=? AND kind='tool'", start.RunID)
	before, after := recs[0], recs[1]
	if before["hook"] != "before_tool" || after["hook"] != "after_tool" {
		t.Fatalf("%v %v", before, after)
	}
	for _, rec := range recs {
		if rec["run_id"] != start.RunID || rec["thread_id"] != info.ThreadID || rec["action_id"] != action || rec["stage"] != nil || rec["decision"] != "continue" || rec["format"] != "rencrow-hook-audit/v1" {
			t.Fatalf("%v", rec)
		}
		if len(rec) != 11 {
			t.Fatalf("the record has fields beyond the input's: %v", rec)
		}
	}
	if before["code"] != nil || after["code"] != "completed" || len(before["evidence_ids"].([]any)) != 0 || len(after["evidence_ids"].([]any)) == 0 {
		t.Fatalf("%v %v", before, after)
	}
	if after["context_revision"].(float64) < before["context_revision"].(float64) {
		t.Fatalf("%v %v", before, after)
	}
	for _, id := range strings.Split(r.val("SELECT group_concat(evidence_id) FROM evidence WHERE run_id=? AND json_extract(metadata_json,'$.purpose')='hook_audit'", start.RunID), ",") {
		if body := r.evidence(id); strings.Contains(body, "secret-name") || strings.Contains(body, "content of the file") {
			t.Fatalf("a record carries a name or a content: %s", body)
		}
	}
	// The order is the dispatch's own: the hook before the start, the hook after the end.
	var seq []string
	for _, ev := range r.threadEvents(info.ThreadID) {
		if ev.RunID != nil && *ev.RunID == start.RunID && strings.HasPrefix(ev.Type, "action.") {
			seq = append(seq, ev.Type)
		}
	}
	if got := strings.Join(seq, ","); !strings.Contains(got, "action.prepared") || !strings.Contains(got, "action.dispatch_started") || !strings.Contains(got, "action.completed") {
		t.Fatalf("%s", got)
	}
	if run.Result.Resumable {
		t.Fatalf("%+v", run.Result)
	}
}

func TestWithNoHookConfiguredNothingIsRecorded(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(calls(tc("c1", "file.create", createArgs("made.txt", "x"))), harnesstest.Final("done"))
	r, _ := extRig(t, fake, hookSetup{})
	info := r.openSession("hook.none.open.00000001")
	start := r.startRun(info.ThreadID, "hook.none.start.0000001", "go")
	if run := r.waitTerminal(start.RunID); run.Result.Status != "completed" {
		t.Fatalf("%+v", run.Result)
	}
	if got := r.purposes(start.RunID); strings.Contains(got, "hook_") {
		t.Fatalf("%s", got)
	}
}

// TestAHookSeesTheCallItIsAboutAndNothingMore: the input a callback gets names the Run, the
// Thread, the Action and the revisions the Run holds; before a call it has no evidence and no
// code, after it the effect state.
func TestAHookSeesTheCallItIsAboutAndNothingMore(t *testing.T) {
	var mu sync.Mutex
	var seen []extensions.HookInput
	spy := extensions.Named{Name: "spy", Call: func(_ context.Context, in extensions.HookInput, _ extensions.Recorder) (extensions.HookResult, error) {
		mu.Lock()
		seen = append(seen, in)
		mu.Unlock()
		return extensions.Continue(), nil
	}}
	fake := harnesstest.NewFake()
	fake.SetScript(calls(tc("c1", "file.read", readArgs("missing.txt", 0, 10, 10))), harnesstest.Final("done"))
	r, _ := extRig(t, fake, hookSetup{extra: []extensions.Named{spy}})
	info := r.openSession("hook.spy.open.000000001")
	start := r.startRun(info.ThreadID, "hook.spy.start.00000001", "go")
	r.waitTerminal(start.RunID)
	mu.Lock()
	defer mu.Unlock()
	var toolPoints []extensions.HookInput
	for _, in := range seen {
		if in.Hook == extensions.BeforeTool || in.Hook == extensions.AfterTool {
			toolPoints = append(toolPoints, in)
		}
	}
	seen = toolPoints
	if len(seen) != 2 {
		t.Fatalf("%d calls: %+v", len(seen), seen)
	}
	action := r.val("SELECT action_id FROM actions WHERE run_id=? AND kind='tool'", start.RunID)
	b, a := seen[0], seen[1]
	if b.Hook != extensions.BeforeTool || a.Hook != extensions.AfterTool {
		t.Fatalf("%+v %+v", b, a)
	}
	for _, in := range seen {
		if err := in.Validate(); err != nil || in.RunID != start.RunID || in.ThreadID != info.ThreadID || in.ActionID == nil || *in.ActionID != action || in.Stage != nil || in.ContextRevision < 1 || in.ControlRevision != 0 {
			t.Fatalf("%+v %v", in, err)
		}
	}
	// A read of a file that is not there failed: the hook after is told that, and it is
	// still called.
	if b.Code != nil || len(b.EvidenceIDs) != 0 || a.Code == nil || *a.Code != "failed" {
		t.Fatalf("%+v %+v", b, a)
	}
}

// TestADenyBeforeACallEndsTheRunBlockedAndTheCallNeverStarts: nothing was dispatched, so the
// call is closed as one that was not run (not as one whose effect is unknown), the calls
// after it in the response are not run either, and the model is not asked again.
func TestADenyBeforeACallEndsTheRunBlockedAndTheCallNeverStarts(t *testing.T) {
	var calls1 atomic.Int32
	deny := onPoint(extensions.BeforeTool, func(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
		calls1.Add(1)
		return extensions.Deny("NOT_ALLOWED_HERE"), nil
	})
	fake := harnesstest.NewFake()
	fake.SetScript(calls(tc("c1", "file.create", createArgs("first.txt", "1")), tc("c2", "file.create", createArgs("second.txt", "2"))), harnesstest.Final("never asked for"))
	r, _ := extRig(t, fake, hookSetup{extra: []extensions.Named{deny}})
	info := r.openSession("hook.deny.open.00000001")
	start := r.startRun(info.ThreadID, "hook.deny.start.0000001", "go")
	run := r.waitTerminal(start.RunID)
	res := run.Result
	if res.Status != "blocked" || res.Code != "HOST_HOOK_DENIED" || !res.Resumable || len(res.UnresolvedActionIDs) != 0 {
		t.Fatalf("%+v", res)
	}
	if _, ok := r.read("first.txt"); ok {
		t.Fatal("a denied call ran")
	}
	if _, ok := r.read("second.txt"); ok {
		t.Fatal("a call after a denied one ran")
	}
	if n := toolDispatchStarts(r, info.ThreadID, start.RunID); n != 0 {
		t.Fatalf("%d dispatch-starts", n)
	}
	if got := toolAttemptStates(r, start.RunID); got != "cancelled,cancelled" {
		t.Fatalf("%s", got)
	}
	if r.val("SELECT COUNT(*) FROM actions WHERE run_id=? AND kind='tool' AND status NOT IN ('failed','cancelled','rejected')", start.RunID) != "0" {
		t.Fatalf("an action was left open: %s", r.val("SELECT group_concat(status) FROM actions WHERE run_id=?", start.RunID))
	}
	if calls1.Load() != 1 || len(fake.Generates()) != 1 {
		t.Fatalf("hook calls %d, generations %d", calls1.Load(), len(fake.Generates()))
	}
	// The exchange is part of the context, with the call answered as not run (so that a
	// resume reads a consistent history).
	if r.count("context_entries") != 4 { // the input, the assistant message, two answers
		t.Fatalf("%d entries", r.count("context_entries"))
	}
}

// TestADenyOfALaterCallLeavesTheEarlierOnesAsTheyWere: the first call ran and keeps its
// effect and its answer; the second is the one denied.
func TestADenyOfALaterCallLeavesTheEarlierOnesAsTheyWere(t *testing.T) {
	var n atomic.Int32
	denySecond := onPoint(extensions.BeforeTool, func(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
		if n.Add(1) == 2 {
			return extensions.Deny("SECOND"), nil
		}
		return extensions.Continue(), nil
	})
	fake := harnesstest.NewFake()
	fake.SetScript(calls(tc("c1", "file.create", createArgs("first.txt", "1")), tc("c2", "file.create", createArgs("second.txt", "2"))), harnesstest.Final("never"))
	r, _ := extRig(t, fake, hookSetup{extra: []extensions.Named{denySecond}})
	info := r.openSession("hook.deny2.open.0000001")
	start := r.startRun(info.ThreadID, "hook.deny2.start.000001", "go")
	res := r.waitTerminal(start.RunID).Result
	if res.Status != "blocked" || res.Code != "HOST_HOOK_DENIED" {
		t.Fatalf("%+v", res)
	}
	if got, ok := r.read("first.txt"); !ok || got != "1" {
		t.Fatal("the call before the denied one did not keep its effect")
	}
	if _, ok := r.read("second.txt"); ok {
		t.Fatal("the denied call ran")
	}
	if got := toolAttemptStates(r, start.RunID); got != "completed,cancelled" {
		t.Fatalf("%s", got)
	}
	if n := toolDispatchStarts(r, info.ThreadID, start.RunID); n != 1 {
		t.Fatalf("%d dispatch-starts", n)
	}
}

// TestAHookThatDoesNotAnswerValidlyBeforeACallBlocksTheRun: an error, a result that is not
// valid, a panic, and a callback that does not return before the hard limit are each
// HOST_HOOK_FAILED, and none of them lets the call start.
func TestAHookThatDoesNotAnswerValidlyBeforeACallBlocksTheRun(t *testing.T) {
	for name, cb := range map[string]extensions.Callback{
		"an error": func(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
			return extensions.HookResult{}, errors.New("no")
		},
		"a deny without a code": func(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
			return extensions.HookResult{Decision: "deny"}, nil
		},
		"a decision that is neither": func(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
			return extensions.HookResult{Decision: "maybe"}, nil
		},
		"a panic": func(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
			panic("boom")
		},
		"no return in time": func(ctx context.Context, _ extensions.HookInput, _ extensions.Recorder) (extensions.HookResult, error) {
			<-ctx.Done()
			return extensions.Continue(), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			fake.SetScript(calls(tc("c1", "file.create", createArgs("never.txt", "x"))), harnesstest.Final("never"))
			r, _ := extRig(t, fake, hookSetup{extra: []extensions.Named{onPoint(extensions.BeforeTool, cb)}, hard: 40 * time.Millisecond})
			info := r.openSession("hook.fail.open.00000001")
			start := r.startRun(info.ThreadID, "hook.fail.start.0000001", "go")
			res := r.waitTerminal(start.RunID).Result
			if res.Status != "blocked" || res.Code != "HOST_HOOK_FAILED" || !res.Resumable {
				t.Fatalf("%+v", res)
			}
			if _, ok := r.read("never.txt"); ok {
				t.Fatal("the call started")
			}
			if n := toolDispatchStarts(r, info.ThreadID, start.RunID); n != 0 {
				t.Fatalf("%d dispatch-starts", n)
			}
		})
	}
}

// TestAHookAfterACallCannotChangeIt: whatever the hook does after a call (fail, deny, run
// over, panic), the call's effect, its answer and the Run stand; a diagnosis is left.
func TestAHookAfterACallCannotChangeIt(t *testing.T) {
	for name, cb := range map[string]extensions.Callback{
		"an error": func(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
			return extensions.HookResult{}, errors.New("no")
		},
		"a deny": func(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
			return extensions.Deny("TOO_LATE"), nil
		},
		"a panic": func(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
			panic("boom")
		},
		"no return in time": func(ctx context.Context, _ extensions.HookInput, _ extensions.Recorder) (extensions.HookResult, error) {
			<-ctx.Done()
			return extensions.Continue(), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			fake.SetScript(calls(tc("c1", "file.create", createArgs("made.txt", "made"))), harnesstest.Final("all done"))
			r, _ := extRig(t, fake, hookSetup{extra: []extensions.Named{onPoint(extensions.AfterTool, cb)}, hard: 40 * time.Millisecond})
			info := r.openSession("hook.after.open.0000001")
			start := r.startRun(info.ThreadID, "hook.after.start.000001", "go")
			res := r.waitTerminal(start.RunID).Result
			if res.Status != "completed" || res.FinalText != "all done" {
				t.Fatalf("%+v", res)
			}
			if got, ok := r.read("made.txt"); !ok || got != "made" {
				t.Fatal("the call's effect is gone")
			}
			if got := toolAttemptStates(r, start.RunID); got != "completed" {
				t.Fatalf("%s", got)
			}
			if !strings.Contains(r.purposes(start.RunID), "hook_diagnosis") {
				t.Fatalf("no diagnosis was left: %s", r.purposes(start.RunID))
			}
			id := r.val("SELECT evidence_id FROM evidence WHERE run_id=? AND json_extract(metadata_json,'$.purpose')='hook_diagnosis'", start.RunID)
			var diag map[string]any
			if err := json.Unmarshal([]byte(r.evidence(id)), &diag); err != nil || diag["hook"] != "after_tool" || diag["reason"] == nil || strings.Contains(r.evidence(id), "made.txt") {
				t.Fatalf("%v %v", diag, err)
			}
		})
	}
}

// TestAStopRecordedWhileAHookBeforeACallRunsIsTheRunsEnd: the hook does not finish; the stop
// that was recorded (turn/interrupt) ends the Run as cancelled, the call never starts, and
// the hook is not blamed for it.
func TestAStopRecordedWhileAHookBeforeACallRunsIsTheRunsEnd(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	wait := onPoint(extensions.BeforeTool, func(ctx context.Context, _ extensions.HookInput, _ extensions.Recorder) (extensions.HookResult, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return extensions.Continue(), nil
	})
	fake := harnesstest.NewFake()
	fake.SetScript(calls(tc("c1", "file.create", createArgs("never.txt", "x"))), harnesstest.Final("never"))
	r, _ := extRig(t, fake, hookSetup{extra: []extensions.Named{wait}, hard: 30 * time.Second})
	info := r.openSession("hook.stop.open.00000001")
	start := r.startRun(info.ThreadID, "hook.stop.start.0000001", "go")
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the hook was not called")
	}
	if rec := r.interrupt(start.RunID, 0, "hook.stop.key.000000001"); rec.Code != "CANCEL_REQUESTED" {
		t.Fatalf("%+v", rec)
	}
	res := r.waitTerminal(start.RunID).Result
	if res.Status != "cancelled" || res.Code != "CANCELLED" || len(res.UnresolvedActionIDs) != 0 {
		t.Fatalf("%+v", res)
	}
	if _, ok := r.read("never.txt"); ok {
		t.Fatal("the call started")
	}
	if n := toolDispatchStarts(r, info.ThreadID, start.RunID); n != 0 {
		t.Fatalf("%d dispatch-starts", n)
	}
	if got := toolAttemptStates(r, start.RunID); got != "cancelled" {
		t.Fatalf("%s", got)
	}
}

// TestAStopRecordedWhileAHookContinuesWinsAtTheDispatchStart: the hook continues, but a stop
// was recorded while it was deciding, and the dispatch-start's own compare-and-set decides:
// the stop came first, so nothing is dispatched. (The hook is not what guards the call; it is
// only an earlier gate.)
func TestAStopRecordedWhileAHookContinuesWinsAtTheDispatchStart(t *testing.T) {
	var r *rig
	interrupter := onPoint(extensions.BeforeTool, func(ctx context.Context, in extensions.HookInput, _ extensions.Recorder) (extensions.HookResult, error) {
		// Another party records the stop while the hook is deciding to continue.
		res, _, err := r.svc.TurnInterrupt(context.Background(), mustParams(protocol.InterruptInput{RunID: in.RunID, ExpectedControlRevision: 0, IdempotencyKey: "hook.race.key.00000001"}))
		if err != nil || res.Code != "CANCEL_REQUESTED" {
			return extensions.HookResult{}, errors.New("the stop was not recorded")
		}
		return extensions.Continue(), nil
	})
	fake := harnesstest.NewFake()
	fake.SetScript(calls(tc("c1", "file.create", createArgs("never.txt", "x"))), harnesstest.Final("never"))
	r, _ = extRig(t, fake, hookSetup{extra: []extensions.Named{interrupter}, hard: 30 * time.Second})
	info := r.openSession("hook.race.open.00000001")
	start := r.startRun(info.ThreadID, "hook.race.start.0000001", "go")
	res := r.waitTerminal(start.RunID).Result
	if res.Status != "cancelled" || res.Code != "CANCELLED" {
		t.Fatalf("%+v", res)
	}
	if _, ok := r.read("never.txt"); ok {
		t.Fatal("a call was dispatched after the stop was recorded")
	}
	if n := toolDispatchStarts(r, info.ThreadID, start.RunID); n != 0 {
		t.Fatalf("%d dispatch-starts", n)
	}
}

// TestAStoreErrorOfAHooksRecordIsTheDriversAndNotTheHooks: a callback whose record could not be
// written because the writer lost the Thread does not make the Run HOST_HOOK_FAILED; the
// driver finds that out as it does for any write, and lets go of the Run.
func TestAStoreErrorOfAHooksRecordIsTheDriversAndNotTheHooks(t *testing.T) {
	lost := onPoint(extensions.BeforeTool, func(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
		return extensions.HookResult{}, extensions.Fatal(sqlite.ErrWriterLost)
	})
	fake := harnesstest.NewFake()
	fake.SetScript(calls(tc("c1", "file.create", createArgs("never.txt", "x"))), harnesstest.Final("never"))
	r, _ := extRig(t, fake, hookSetup{extra: []extensions.Named{lost}})
	info := r.openSession("hook.lost.open.00000001")
	start := r.startRun(info.ThreadID, "hook.lost.start.0000001", "go")
	r.waitFor("the driver to let go", func() bool { return r.val("SELECT phase FROM runs WHERE run_id=?", start.RunID) == "Executing" })
	r.svc.Quiesce()
	if st := r.val("SELECT status FROM runs WHERE run_id=?", start.RunID); st != "running" && st != "incomplete" {
		t.Fatalf("the run was ended as a hook failure: %s", st)
	}
	if got := r.val("SELECT COALESCE(json_extract(result_json,'$.code'),'') FROM runs WHERE run_id=?", start.RunID); got == "HOST_HOOK_FAILED" {
		t.Fatal("a store error was reported as the hook's failure")
	}
	if _, ok := r.read("never.txt"); ok {
		t.Fatal("the call started")
	}
}
