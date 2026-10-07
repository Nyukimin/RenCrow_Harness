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

	"github.com/Nyukimin/RenCrow_Harness/internal/extensions"
	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The host's fixed hooks at the points the Kernel owns (HOST_ASSETS section 4): before and
// after a model generation (an act step and each compaction stage), before and after a
// compaction, and the end of the Run, through real Runs. What they check is where each point
// sits relative to the records a generation, a compaction and a terminal make (a hook before
// something leaves nothing reserved, sent or compacted when it denies), and that no answer of
// a hook after something changes what was recorded, whatever the generation, the stop or the
// outcome was.

// hookLog records every input the hooks were called with, in order. Its callback continues
// everywhere, so it can stand first in the list and see a call that a later one denies.
type hookLog struct {
	mu   sync.Mutex
	seen []extensions.HookInput
}

func (l *hookLog) named() extensions.Named {
	return extensions.Named{Name: "log", Call: func(_ context.Context, in extensions.HookInput, _ extensions.Recorder) (extensions.HookResult, error) {
		l.mu.Lock()
		l.seen = append(l.seen, in)
		l.mu.Unlock()
		return extensions.Continue(), nil
	}}
}

func (l *hookLog) reset() {
	l.mu.Lock()
	l.seen = nil
	l.mu.Unlock()
}

func (l *hookLog) inputs() []extensions.HookInput {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]extensions.HookInput(nil), l.seen...)
}

// points is the order the points were called in, as one string.
func (l *hookLog) points() string {
	var out []string
	for _, in := range l.inputs() {
		out = append(out, string(in.Hook))
	}
	return strings.Join(out, ",")
}

func (l *hookLog) at(p extensions.HookPoint) []extensions.HookInput {
	var out []extensions.HookInput
	for _, in := range l.inputs() {
		if in.Hook == p {
			out = append(out, in)
		}
	}
	return out
}

// hookRigBuild is the rig of a scenario that has the hooks of hs.
func hookRigBuild(hs hookSetup) rigBuild {
	return func(t *testing.T, fake *harnesstest.Fake) (*rig, *recorder) {
		t.Helper()
		return extRig(t, fake, hs)
	}
}

func cbError(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
	return extensions.HookResult{}, errors.New("no")
}

func cbPanic(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
	panic("boom")
}

func cbHang(ctx context.Context, _ extensions.HookInput, _ extensions.Recorder) (extensions.HookResult, error) {
	<-ctx.Done()
	return extensions.Continue(), nil
}

func cbDeny(code string) extensions.Callback {
	return func(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
		return extensions.Deny(code), nil
	}
}

// notValid are the answers a hook at a point before something may not give: each is a failure
// of the hook there (a deny with a code is a valid answer at such a point, so it is not here).
func notValid() map[string]extensions.Callback {
	return map[string]extensions.Callback{
		"an error": cbError,
		"a deny without a code": func(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
			return extensions.HookResult{Decision: "deny"}, nil
		},
		"a decision that is neither": func(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
			return extensions.HookResult{Decision: "maybe"}, nil
		},
		"a panic":           cbPanic,
		"no return in time": cbHang,
	}
}

// failsAfter are the ways a hook at a point after something goes wrong, which include a deny
// (those points cannot deny, so a deny is not an answer there).
func failsAfter() map[string]extensions.Callback {
	return map[string]extensions.Callback{
		"an error":          cbError,
		"a deny":            cbDeny("TOO_LATE"),
		"a panic":           cbPanic,
		"no return in time": cbHang,
	}
}

func (r *rig) actionOf(runID, where string) string {
	r.t.Helper()
	return r.val("SELECT COALESCE(MIN(action_id),'') FROM actions WHERE run_id=? AND "+where, runID)
}

// diagnoses are the diagnosis records a Run left: which point and which Action.
func (r *rig) diagnoses(runID string) []map[string]any {
	r.t.Helper()
	var out []map[string]any
	for _, id := range strings.Split(r.val("SELECT COALESCE(group_concat(evidence_id), '') FROM (SELECT evidence_id FROM evidence WHERE run_id=? AND json_extract(metadata_json,'$.purpose')='hook_diagnosis' ORDER BY rowid)", runID), ",") {
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

func (r *rig) eventsOfType(thread, typ string) []protocol.Event {
	r.t.Helper()
	var out []protocol.Event
	for _, ev := range r.threadEvents(thread) {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// modelCompletedOf is the payload of the model.completed event of an Attempt.
func (r *rig) modelCompletedOf(thread, runID string) []protocol.ModelCompletedPayload {
	r.t.Helper()
	var out []protocol.ModelCompletedPayload
	for _, ev := range r.eventsOfType(thread, protocol.EventModelCompleted) {
		if ev.RunID == nil || *ev.RunID != runID {
			continue
		}
		var p protocol.ModelCompletedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// facts are what the store holds of a Run's generations at one instant.
type facts struct {
	actions, attempts, generates int
	attemptsUsed                 string
	attemptState                 string
}

func (r *rig) factsOf(fake *harnesstest.Fake, runID string) facts {
	r.t.Helper()
	return facts{
		actions:      int(atoi(r.val("SELECT COUNT(*) FROM actions WHERE run_id=?", runID))),
		attempts:     int(atoi(r.val("SELECT COUNT(*) FROM attempts a JOIN actions ac ON ac.action_id=a.action_id WHERE ac.run_id=?", runID))),
		generates:    len(fake.Generates()),
		attemptsUsed: r.val("SELECT generation_attempts_used FROM runs WHERE run_id=?", runID),
		attemptState: r.val("SELECT COALESCE(group_concat(a.state), '') FROM attempts a JOIN actions ac ON ac.action_id=a.action_id WHERE ac.run_id=?", runID),
	}
}

// checkInputs: every input a hook was given is valid, names the Run and the Thread, and tells
// the control revision the Run holds.
func checkInputs(t *testing.T, ins []extensions.HookInput, thread, runID string) {
	t.Helper()
	for _, in := range ins {
		if err := in.Validate(); err != nil || in.RunID != runID || in.ThreadID != thread || in.ControlRevision != 0 {
			t.Fatalf("%+v %v", in, err)
		}
	}
}

// TestModelHooksSeeTheGenerationTheyAreAboutAndBeforeAnythingIsReserved: before_model is
// called when nothing of the generation exists (no Action, no Attempt, no attempt consumed, no
// request sent) and after_model once its Attempt is ended, naming the Action, the stage, the
// Attempt's state and the receipt Evidence that the model.completed event names.
func TestModelHooksSeeTheGenerationTheyAreAboutAndBeforeAnythingIsReserved(t *testing.T) {
	var r *rig
	fake := harnesstest.NewFake()
	fake.SetScript(harnesstest.Final("the answer"))
	log := &hookLog{}
	var mu sync.Mutex
	var before, after facts
	probe := extensions.Named{Name: "probe", Call: func(_ context.Context, in extensions.HookInput, _ extensions.Recorder) (extensions.HookResult, error) {
		f := r.factsOf(fake, in.RunID)
		mu.Lock()
		defer mu.Unlock()
		switch in.Hook {
		case extensions.BeforeModel:
			before = f
		case extensions.AfterModel:
			after = f
		}
		return extensions.Continue(), nil
	}}
	r, _ = extRig(t, fake, hookSetup{extra: []extensions.Named{log.named(), probe}})
	info := r.openSession("hook.model.open.0000001")
	start := r.startRun(info.ThreadID, "hook.model.start.000001", "answer")
	run := r.waitTerminal(start.RunID)
	if run.Result.Status != "completed" || run.Result.FinalText != "the answer" {
		t.Fatalf("%+v", run.Result)
	}
	if got := log.points(); got != "before_model,after_model,run_terminal" {
		t.Fatal(got)
	}
	mu.Lock()
	defer mu.Unlock()
	if (before != facts{attemptsUsed: "0"}) {
		t.Fatalf("before_model saw something of the generation: %+v", before)
	}
	if after.actions != 1 || after.attempts != 1 || after.generates != 1 || after.attemptsUsed != "1" || after.attemptState != "completed" {
		t.Fatalf("after_model: %+v", after)
	}
	checkInputs(t, log.inputs(), info.ThreadID, start.RunID)
	action := r.val("SELECT action_id FROM actions WHERE run_id=?", start.RunID)
	completed := r.modelCompletedOf(info.ThreadID, start.RunID)
	b, a, e := log.at(extensions.BeforeModel)[0], log.at(extensions.AfterModel)[0], log.at(extensions.RunTerminal)[0]
	if b.ActionID != nil || b.Stage == nil || *b.Stage != "act" || b.Code != nil || len(b.EvidenceIDs) != 0 {
		t.Fatalf("%+v", b)
	}
	if a.ActionID == nil || *a.ActionID != action || a.Stage == nil || *a.Stage != "act" || a.Code == nil || *a.Code != "completed" ||
		len(completed) != 1 || len(a.EvidenceIDs) != 1 || a.EvidenceIDs[0] != completed[0].AttemptReceiptEvidenceID {
		t.Fatalf("%+v %+v", a, completed)
	}
	if got := r.val("SELECT json_extract(metadata_json,'$.purpose') FROM evidence WHERE evidence_id=?", a.EvidenceIDs[0]); got != "model_attempt_receipt" {
		t.Fatal(got)
	}
	if e.ActionID != nil || e.Stage != nil || e.Code == nil || *e.Code != "FINAL_RESPONSE_ACCEPTED" || len(e.EvidenceIDs) != 0 {
		t.Fatalf("%+v", e)
	}
}

// TestAuditMetadataRecordsEveryPointOfARunInOrderAndOnlyIdentifiers: with the one hook the
// configuration may name, a Run that reads a file leaves a record at each of its points, in the
// order the Run goes through them, naming the Run and the revisions and nothing of its content,
// and the records are part of the Run's evidence the result lists (the end point's included).
func TestAuditMetadataRecordsEveryPointOfARunInOrderAndOnlyIdentifiers(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(calls(tc("c1", "file.read", readArgs("secret-name.txt", 0, 100, 100))), harnesstest.Final("done"))
	r, _ := extRig(t, fake, hookSetup{configured: []string{extensions.AuditMetadata}})
	r.write("secret-name.txt", "the content of the file\n")
	info := r.openSession("hook.audit7.open.000001")
	start := r.startRun(info.ThreadID, "hook.audit7.start.00001", "read it")
	run := r.waitTerminal(start.RunID)
	if run.Result.Status != "completed" || run.Result.FinalText != "done" || run.Result.Resumable {
		t.Fatalf("%+v", run.Result)
	}
	recs := r.auditRecords(start.RunID)
	var names []string
	for _, rec := range recs {
		names = append(names, rec["hook"].(string))
		if len(rec) != 11 || rec["format"] != "rencrow-hook-audit/v1" || rec["decision"] != "continue" || rec["run_id"] != start.RunID || rec["thread_id"] != info.ThreadID {
			t.Fatalf("%v", rec)
		}
	}
	if got := strings.Join(names, ","); got != "before_model,after_model,before_tool,after_tool,before_model,after_model,run_terminal" {
		t.Fatal(got)
	}
	last := recs[len(recs)-1]
	if last["code"] != "FINAL_RESPONSE_ACCEPTED" || last["action_id"] != nil || last["stage"] != nil {
		t.Fatalf("%v", last)
	}
	for _, id := range strings.Split(r.val("SELECT group_concat(evidence_id) FROM evidence WHERE run_id=? AND json_extract(metadata_json,'$.purpose')='hook_audit'", start.RunID), ",") {
		if body := r.evidence(id); strings.Contains(body, "secret-name") || strings.Contains(body, "content of the file") {
			t.Fatalf("a record carries a name or a content: %s", body)
		}
		found := false
		for _, e := range run.Result.EvidenceIDs {
			found = found || e == id
		}
		if !found {
			t.Fatalf("the result does not list the record %s", id)
		}
	}
}

// TestADenyBeforeAGenerationEndsTheRunBlockedWithNothingReservedOrSent: the Action, the
// Attempt, the consumed attempt and the request are all made by the one reservation, which the
// hook precedes, so a deny leaves none of them (not an Attempt of unknown outcome), the model
// is not asked, and the Run is blocked and can be resumed. A deny before a retry leaves the
// failed Attempt as it ended and spends nothing more.
func TestADenyBeforeAGenerationEndsTheRunBlockedWithNothingReservedOrSent(t *testing.T) {
	t.Run("the first attempt", func(t *testing.T) {
		fake := harnesstest.NewFake()
		fake.SetScript(harnesstest.Final("never asked for"))
		log := &hookLog{}
		r, _ := extRig(t, fake, hookSetup{extra: []extensions.Named{log.named(), onPoint(extensions.BeforeModel, cbDeny("NO_MODEL_TODAY"))}})
		info := r.openSession("hook.mdeny.open.0000001")
		start := r.startRun(info.ThreadID, "hook.mdeny.start.000001", "go")
		run := r.waitTerminal(start.RunID)
		if resultKey(run.Result) != "blocked/HOST_HOOK_DENIED/resumable/none" {
			t.Fatalf("%+v", run.Result)
		}
		if f := r.factsOf(fake, start.RunID); f != (facts{attemptsUsed: "0"}) || run.GenerationAttemptsUsed != 0 || run.GenerationAttemptsUnknown != 0 || r.count("model_calls") != 0 {
			t.Fatalf("%+v %+v", f, run)
		}
		if got := log.points(); got != "before_model,run_terminal" {
			t.Fatal(got)
		}
		if end := log.at(extensions.RunTerminal)[0]; end.Code == nil || *end.Code != "HOST_HOOK_DENIED" {
			t.Fatalf("%+v", end)
		}
	})
	t.Run("a retry", func(t *testing.T) {
		fake := harnesstest.NewFake()
		fake.SetScript(rawMarkup("<tool_call>broken</tool_call>"), harnesstest.Final("never asked for"))
		log := &hookLog{}
		var n atomic.Int32
		denySecond := onPoint(extensions.BeforeModel, func(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
			if n.Add(1) == 2 {
				return extensions.Deny("NOT_AGAIN"), nil
			}
			return extensions.Continue(), nil
		})
		r, _ := extRig(t, fake, hookSetup{extra: []extensions.Named{log.named(), denySecond}})
		info := r.openSession("hook.mdeny2.open.000001")
		start := r.startRun(info.ThreadID, "hook.mdeny2.start.00001", "go")
		run := r.waitTerminal(start.RunID)
		if resultKey(run.Result) != "blocked/HOST_HOOK_DENIED/resumable/none" {
			t.Fatalf("%+v", run.Result)
		}
		f := r.factsOf(fake, start.RunID)
		if f.actions != 1 || f.attempts != 1 || f.generates != 1 || f.attemptsUsed != "1" || f.attemptState != "failed" || run.GenerationAttemptsUnknown != 0 {
			t.Fatalf("%+v", f)
		}
		// The retry's Attempt belongs to the Action of the one that failed, which the hook is told.
		action := r.val("SELECT action_id FROM actions WHERE run_id=?", start.RunID)
		bm := log.at(extensions.BeforeModel)
		if len(bm) != 2 || bm[0].ActionID != nil || bm[1].ActionID == nil || *bm[1].ActionID != action {
			t.Fatalf("%+v", bm)
		}
		if got := log.points(); got != "before_model,after_model,before_model,run_terminal" {
			t.Fatal(got)
		}
	})
}

// TestAHookThatDoesNotAnswerValidlyBeforeAGenerationBlocksTheRun: an error, an answer that is
// not valid, a panic and a callback that does not return before the hard limit are each
// HOST_HOOK_FAILED, and none lets the generation start.
func TestAHookThatDoesNotAnswerValidlyBeforeAGenerationBlocksTheRun(t *testing.T) {
	for name, cb := range notValid() {
		t.Run(name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			fake.SetScript(harnesstest.Final("never asked for"))
			r, _ := extRig(t, fake, hookSetup{extra: []extensions.Named{onPoint(extensions.BeforeModel, cb)}, hard: 40 * time.Millisecond})
			info := r.openSession("hook.mfail.open.0000001")
			start := r.startRun(info.ThreadID, "hook.mfail.start.000001", "go")
			run := r.waitTerminal(start.RunID)
			if resultKey(run.Result) != "blocked/HOST_HOOK_FAILED/resumable/none" {
				t.Fatalf("%+v", run.Result)
			}
			if f := r.factsOf(fake, start.RunID); f != (facts{attemptsUsed: "0"}) {
				t.Fatalf("%+v", f)
			}
		})
	}
}

// TestAStopRecordedAroundAHookBeforeAGenerationIsTheRunsEnd: a stop that is recorded while the
// hook decides ends the Run cancelled, not blocked by the hook, and nothing is reserved or
// sent: whether the hook is still deciding (the stop ends it) or continues (the reservation's
// own compare-and-set on the control revision decides).
func TestAStopRecordedAroundAHookBeforeAGenerationIsTheRunsEnd(t *testing.T) {
	t.Run("the hook is still deciding", func(t *testing.T) {
		entered := make(chan struct{})
		var once sync.Once
		wait := onPoint(extensions.BeforeModel, func(ctx context.Context, _ extensions.HookInput, _ extensions.Recorder) (extensions.HookResult, error) {
			once.Do(func() { close(entered) })
			<-ctx.Done()
			return extensions.Continue(), nil
		})
		fake := harnesstest.NewFake()
		fake.SetScript(harnesstest.Final("never asked for"))
		r, _ := extRig(t, fake, hookSetup{extra: []extensions.Named{wait}, hard: 30 * time.Second})
		info := r.openSession("hook.mstop.open.00000001")
		start := r.startRun(info.ThreadID, "hook.mstop.start.0000001", "go")
		select {
		case <-entered:
		case <-time.After(20 * time.Second):
			t.Fatal("the hook was not called")
		}
		if rec := r.interrupt(start.RunID, 0, "hook.mstop.key.000000001"); rec.Code != "CANCEL_REQUESTED" {
			t.Fatalf("%+v", rec)
		}
		run := r.waitTerminal(start.RunID)
		if resultKey(run.Result) != "cancelled/CANCELLED/resumable/none" {
			t.Fatalf("%+v", run.Result)
		}
		if f := r.factsOf(fake, start.RunID); f != (facts{attemptsUsed: "0"}) {
			t.Fatalf("%+v", f)
		}
	})
	t.Run("the hook continues", func(t *testing.T) {
		var r *rig
		interrupter := onPoint(extensions.BeforeModel, func(_ context.Context, in extensions.HookInput, _ extensions.Recorder) (extensions.HookResult, error) {
			res, _, err := r.svc.TurnInterrupt(context.Background(), mustParams(protocol.InterruptInput{RunID: in.RunID, ExpectedControlRevision: 0, IdempotencyKey: "hook.mrace.key.000000001"}))
			if err != nil || res.Code != "CANCEL_REQUESTED" {
				return extensions.HookResult{}, errors.New("the stop was not recorded")
			}
			return extensions.Continue(), nil
		})
		fake := harnesstest.NewFake()
		fake.SetScript(harnesstest.Final("never asked for"))
		r, _ = extRig(t, fake, hookSetup{extra: []extensions.Named{interrupter}, hard: 30 * time.Second})
		info := r.openSession("hook.mrace.open.00000001")
		start := r.startRun(info.ThreadID, "hook.mrace.start.0000001", "go")
		run := r.waitTerminal(start.RunID)
		if resultKey(run.Result) != "cancelled/CANCELLED/resumable/none" {
			t.Fatalf("%+v", run.Result)
		}
		if f := r.factsOf(fake, start.RunID); f != (facts{attemptsUsed: "0"}) {
			t.Fatalf("%+v", f)
		}
	})
}

// TestAHookAfterAGenerationCannotChangeIt: whatever the hook does after an Attempt ended (fail,
// deny, run over, panic), the Attempt, the answer and the Run stand, and a diagnosis names the
// point and the Action.
func TestAHookAfterAGenerationCannotChangeIt(t *testing.T) {
	for name, cb := range failsAfter() {
		t.Run(name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			fake.SetScript(harnesstest.Final("all done"))
			r, _ := extRig(t, fake, hookSetup{extra: []extensions.Named{onPoint(extensions.AfterModel, cb)}, hard: 40 * time.Millisecond})
			info := r.openSession("hook.mafter.open.0000001")
			start := r.startRun(info.ThreadID, "hook.mafter.start.000001", "go")
			run := r.waitTerminal(start.RunID)
			if run.Result.Status != "completed" || run.Result.FinalText != "all done" || run.GenerationAttemptsUsed != 1 {
				t.Fatalf("%+v", run.Result)
			}
			if f := r.factsOf(fake, start.RunID); f.attemptState != "completed" || f.attempts != 1 {
				t.Fatalf("%+v", f)
			}
			diags := r.diagnoses(start.RunID)
			if len(diags) != 1 || diags[0]["hook"] != "after_model" || diags[0]["action_id"] != r.val("SELECT action_id FROM actions WHERE run_id=?", start.RunID) || diags[0]["reason"] == nil {
				t.Fatalf("%v", diags)
			}
		})
	}
}

// TestAGenerationOfUnknownOutcomeStaysUnknownWhateverTheHookAfterItDoes: the Attempt whose end
// is not known is recorded as such before after_model is called, the hook is told so (the
// Attempt's state is the code), it is never retried, and no answer of the hook turns it into
// something else: the Run ends exactly as it does with no hook at all.
func TestAGenerationOfUnknownOutcomeStaysUnknownWhateverTheHookAfterItDoes(t *testing.T) {
	run := func(t *testing.T, hs hookSetup) (string, *rig, protocol.RunInfo, *harnesstest.Fake) {
		fake := harnesstest.NewFake()
		fake.SetReply(harnesstest.Reply{Kind: harnesstest.KindTransportError})
		r, _ := extRig(t, fake, hs)
		info := r.openSession("hook.unk.open.000000001")
		start := r.startRun(info.ThreadID, "hook.unk.start.00000001", "go")
		got := r.waitTerminal(start.RunID)
		return info.ThreadID, r, got, fake
	}
	_, _, base, _ := run(t, hookSetup{})
	want := "blocked/MODEL_GENERATION_OUTCOME_UNKNOWN/resumable/unresolved"
	if resultKey(base.Result) != want || base.GenerationAttemptsUnknown != 1 {
		t.Fatalf("%+v", base.Result)
	}
	for name, cb := range failsAfter() {
		t.Run(name, func(t *testing.T) {
			log := &hookLog{}
			thread, r, got, fake := run(t, hookSetup{extra: []extensions.Named{log.named(), onPoint(extensions.AfterModel, cb)}, hard: 40 * time.Millisecond})
			if resultKey(got.Result) != want || got.GenerationAttemptsUnknown != 1 || len(got.Result.UnresolvedActionIDs) != 1 || len(fake.Generates()) != 1 {
				t.Fatalf("%+v", got)
			}
			if f := r.factsOf(fake, got.RunID); f.attemptState != "unknown" || r.val("SELECT status FROM actions WHERE run_id=?", got.RunID) != "unknown" {
				t.Fatalf("%+v", f)
			}
			am := log.at(extensions.AfterModel)
			completed := r.modelCompletedOf(thread, got.RunID)
			if len(am) != 1 || am[0].Code == nil || *am[0].Code != "unknown" || am[0].ActionID == nil || *am[0].ActionID != got.Result.UnresolvedActionIDs[0] ||
				len(completed) != 1 || completed[0].GenerationState != "unknown" || len(am[0].EvidenceIDs) != 1 || am[0].EvidenceIDs[0] != completed[0].AttemptReceiptEvidenceID {
				t.Fatalf("%+v %+v", am, completed)
			}
			if end := log.at(extensions.RunTerminal); len(end) != 1 || *end[0].Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" {
				t.Fatalf("%+v", end)
			}
		})
	}
}

// TestAStoreErrorOfAModelHooksRecordIsTheDriversAndNotTheHooks: a record that could not be
// written because the writer lost the Thread does not make the Run HOST_HOOK_FAILED, before a
// generation or after one; the driver lets go of the Run as it does for any write.
func TestAStoreErrorOfAModelHooksRecordIsTheDriversAndNotTheHooks(t *testing.T) {
	lost := func(context.Context, extensions.HookInput, extensions.Recorder) (extensions.HookResult, error) {
		return extensions.HookResult{}, extensions.Fatal(sqlite.ErrWriterLost)
	}
	for _, tc := range []struct {
		point      extensions.HookPoint
		generates  int
		attemptEnd string
	}{{extensions.BeforeModel, 0, ""}, {extensions.AfterModel, 1, "completed"}, {extensions.RunTerminal, 1, "completed"}} {
		t.Run(string(tc.point), func(t *testing.T) {
			fake := harnesstest.NewFake()
			fake.SetScript(harnesstest.Final("done"))
			r, _ := extRig(t, fake, hookSetup{extra: []extensions.Named{onPoint(tc.point, lost)}})
			info := r.openSession("hook.mlost.open.00000001")
			start := r.startRun(info.ThreadID, "hook.mlost.start.0000001", "go")
			phase := map[extensions.HookPoint]string{extensions.BeforeModel: "Generating", extensions.AfterModel: "Generating", extensions.RunTerminal: "PersistingResult"}[tc.point]
			r.waitFor("the driver to let go", func() bool { return r.val("SELECT phase FROM runs WHERE run_id=?", start.RunID) == phase })
			r.svc.Quiesce()
			if st := r.val("SELECT status FROM runs WHERE run_id=?", start.RunID); st != "running" {
				t.Fatalf("the run was ended (%s) by a driver that no longer held it", st)
			}
			if got := r.val("SELECT COALESCE(json_extract(result_json,'$.code'),'') FROM runs WHERE run_id=?", start.RunID); got != "" {
				t.Fatalf("a result was stored: %s", got)
			}
			if f := r.factsOf(fake, start.RunID); f.generates != tc.generates || f.attemptState != tc.attemptEnd {
				t.Fatalf("%+v", f)
			}
		})
	}
}

// compaction scenarios --------------------------------------------------------------------

func newHookCompactionRig(t *testing.T, summary harnesstest.Reply, hs hookSetup) *compactionRig {
	t.Helper()
	return newCompactionRigOn(t, map[string]harnesstest.Reply{modelport.StageSummary: summary}, contextLimit, hookRigBuild(hs))
}

const compactionSequence = "before_model,after_model,before_tool,after_tool,before_compact,before_model,after_model,after_compact,before_model,after_model,run_terminal"

// TestCompactionHooksAreCalledAroundACompactionAndItsStages: for a prompt that does not fit, the
// Run goes through the model, the Tool, the compaction (with its Summary generation as a model
// point of its own stage) and the model again, and each point is told what it is about: the
// compaction's hooks the count that led to it and, after the commit, the two counts of the
// checkpoint and the context revision the commit made.
func TestCompactionHooksAreCalledAroundACompactionAndItsStages(t *testing.T) {
	log := &hookLog{}
	c := newHookCompactionRig(t, harnesstest.Final(goodSummary), hookSetup{extra: []extensions.Named{log.named()}})
	start, run := c.run("hook.compact.0000001", "大きなファイルを読んで保存処理を実装する。")
	if run.Result.Status != "completed" || len(c.checkpoints()) != 1 {
		t.Fatalf("%+v %v", run.Result, c.checkpoints())
	}
	if got := log.points(); got != compactionSequence {
		t.Fatal(got)
	}
	thread := c.val("SELECT thread_id FROM threads")
	checkInputs(t, log.inputs(), thread, start.RunID)
	bc, ac := log.at(extensions.BeforeCompact)[0], log.at(extensions.AfterCompact)[0]
	if bc.ActionID != nil || bc.Stage != nil || bc.Code != nil || len(bc.EvidenceIDs) != 1 ||
		c.val("SELECT json_extract(metadata_json,'$.purpose') FROM evidence WHERE evidence_id=?", bc.EvidenceIDs[0]) != "measure_result" {
		t.Fatalf("%+v", bc)
	}
	committed := c.eventsOfType(thread, protocol.EventCheckpointCommitted)
	if len(committed) != 1 {
		t.Fatalf("%d checkpoint.committed events", len(committed))
	}
	var cp protocol.CheckpointCommittedPayload
	if err := json.Unmarshal(committed[0].Payload, &cp); err != nil {
		t.Fatal(err)
	}
	if ac.ActionID != nil || ac.Stage != nil || ac.Code == nil || *ac.Code != "normal" || ac.ContextRevision != cp.ContextRevision ||
		len(ac.EvidenceIDs) != 2 || ac.EvidenceIDs[0] != cp.BeforeCountEvidenceID || ac.EvidenceIDs[1] != cp.AfterCountEvidenceID {
		t.Fatalf("%+v %+v", ac, cp)
	}
	// The Summary generation is a model point of its own stage, and the Action it makes is the one
	// after_model names; the Run's act generations are told theirs.
	bms, ams := log.at(extensions.BeforeModel), log.at(extensions.AfterModel)
	summary := c.actionOf(start.RunID, "name='work_summary'")
	if *bms[1].Stage != modelport.StageSummary || bms[1].ActionID != nil || *ams[1].Stage != modelport.StageSummary || ams[1].ActionID == nil || *ams[1].ActionID != summary || *ams[1].Code != "completed" {
		t.Fatalf("%+v %+v", bms[1], ams[1])
	}
	acts := strings.Split(c.val("SELECT group_concat(action_id) FROM (SELECT action_id FROM actions WHERE run_id=? AND name='act' ORDER BY rowid)", start.RunID), ",")
	if len(acts) != 2 {
		t.Fatalf("%v", acts)
	}
	for k, i := range []int{0, 2} {
		if *bms[i].Stage != "act" || bms[i].ActionID != nil || *ams[i].Stage != "act" || *ams[i].ActionID != acts[k] {
			t.Fatalf("%+v %+v", bms[i], ams[i])
		}
	}
}

// TestADenyOrAFailureBeforeACompactionEndsTheRunBlockedAndNothingIsCompacted: before_compact is
// called before anything of the compaction is begun, and what a deny or a failure ends is the Run
// (blocked, resumable), with no Summary asked for and no checkpoint stored.
func TestADenyOrAFailureBeforeACompactionEndsTheRunBlockedAndNothingIsCompacted(t *testing.T) {
	cases := map[string]struct {
		cb   extensions.Callback
		code string
	}{
		"a deny":            {cbDeny("NO_COMPACTION"), "HOST_HOOK_DENIED"},
		"an error":          {cbError, "HOST_HOOK_FAILED"},
		"a panic":           {cbPanic, "HOST_HOOK_FAILED"},
		"no return in time": {cbHang, "HOST_HOOK_FAILED"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newHookCompactionRig(t, harnesstest.Final(goodSummary), hookSetup{extra: []extensions.Named{onPoint(extensions.BeforeCompact, tc.cb)}, hard: 40 * time.Millisecond})
			start, run := c.run("hook.cdeny.000000001", "大きなファイルを読んで保存処理を実装する。")
			if resultKey(run.Result) != "blocked/"+tc.code+"/resumable/none" || run.Result.LastCheckpointID != nil {
				t.Fatalf("%+v", run.Result)
			}
			if len(c.checkpoints()) != 0 || len(c.stages[modelport.StageSummary]) != 0 || len(c.acts) != 1 || run.GenerationAttemptsUsed != 1 || c.actionOf(start.RunID, "name='work_summary'") != "" {
				t.Fatalf("checkpoints %v, summaries %d, acts %d, attempts %d", c.checkpoints(), len(c.stages[modelport.StageSummary]), len(c.acts), run.GenerationAttemptsUsed)
			}
		})
	}
}

// TestADenyBeforeACompactionStageGenerationEndsTheRunWithTheStageNotStarted: the Summary
// generation is a model point; a deny there leaves its Action unreserved and the request unsent,
// no checkpoint is stored, and the Run is blocked by the hook (not carried on to the reduction
// that needs no model).
func TestADenyBeforeACompactionStageGenerationEndsTheRunWithTheStageNotStarted(t *testing.T) {
	deny := onPoint(extensions.BeforeModel, func(_ context.Context, in extensions.HookInput, _ extensions.Recorder) (extensions.HookResult, error) {
		if *in.Stage == modelport.StageSummary {
			return extensions.Deny("NO_SUMMARY"), nil
		}
		return extensions.Continue(), nil
	})
	log := &hookLog{}
	c := newHookCompactionRig(t, harnesstest.Final(goodSummary), hookSetup{extra: []extensions.Named{log.named(), deny}})
	start, run := c.run("hook.sdeny.000000001", "大きなファイルを読んで保存処理を実装する。")
	if resultKey(run.Result) != "blocked/HOST_HOOK_DENIED/resumable/none" || run.GenerationAttemptsUnknown != 0 {
		t.Fatalf("%+v", run.Result)
	}
	if len(c.checkpoints()) != 0 || len(c.stages[modelport.StageSummary]) != 0 || run.GenerationAttemptsUsed != 1 || c.actionOf(start.RunID, "name='work_summary'") != "" {
		t.Fatalf("checkpoints %v, summaries %d, attempts %d", c.checkpoints(), len(c.stages[modelport.StageSummary]), run.GenerationAttemptsUsed)
	}
	if got := log.points(); got != "before_model,after_model,before_tool,after_tool,before_compact,before_model,run_terminal" {
		t.Fatal(got)
	}
}

// TestADenyBeforeAnEarlyCompactionBlocksTheRunToo: a compaction started by the trigger ratio,
// while the prompt still fits, is denied like any other: the Run is not sent on with the prompt
// that fits (which it is when the compaction is merely unable to make a checkpoint).
func TestADenyBeforeAnEarlyCompactionBlocksTheRunToo(t *testing.T) {
	counts := func(n int) int64 {
		if n == 0 {
			return 100
		}
		return triggerBoundary
	}
	t.Run("without the hook the run completes", func(t *testing.T) {
		tr := newTriggerRigWith(t, 1, harnesstest.Final(goodSummary), counts, hookRigBuild(hookSetup{}))
		_, run := tr.run("hook.early.0000000001", "大きなファイルを読んで保存処理を実装する。")
		if run.Result.Status != "completed" || len(tr.checkpoints()) != 1 {
			t.Fatalf("%+v", run.Result)
		}
	})
	t.Run("a deny", func(t *testing.T) {
		tr := newTriggerRigWith(t, 1, harnesstest.Final(goodSummary), counts, hookRigBuild(hookSetup{extra: []extensions.Named{onPoint(extensions.BeforeCompact, cbDeny("NO_COMPACTION"))}}))
		_, run := tr.run("hook.early.0000000002", "大きなファイルを読んで保存処理を実装する。")
		if resultKey(run.Result) != "blocked/HOST_HOOK_DENIED/resumable/none" || len(tr.checkpoints()) != 0 || len(tr.acts) != 1 || len(tr.stages[modelport.StageSummary]) != 0 {
			t.Fatalf("%+v checkpoints %v acts %d", run.Result, tr.checkpoints(), len(tr.acts))
		}
	})
}

// TestAHookAfterACompactionCannotChangeTheCheckpoint: whatever it does (fail, deny, run over,
// panic), the checkpoint that was committed, the Thread's pointer and the Run stand, and a
// diagnosis is left.
func TestAHookAfterACompactionCannotChangeTheCheckpoint(t *testing.T) {
	for name, cb := range failsAfter() {
		t.Run(name, func(t *testing.T) {
			c := newHookCompactionRig(t, harnesstest.Final(goodSummary), hookSetup{extra: []extensions.Named{onPoint(extensions.AfterCompact, cb)}, hard: 40 * time.Millisecond})
			start, run := c.run("hook.cafter.00000001", "大きなファイルを読んで保存処理を実装する。")
			res := run.Result
			if res.Status != "completed" || res.FinalText != "compaction handled, the work goes on" || res.LastCheckpointID == nil || len(c.checkpoints()) != 1 {
				t.Fatalf("%+v", res)
			}
			if c.val("SELECT current_checkpoint_id FROM threads") != *res.LastCheckpointID {
				t.Fatal("the Thread's pointer is not the committed checkpoint")
			}
			diags := c.diagnoses(start.RunID)
			if len(diags) != 1 || diags[0]["hook"] != "after_compact" || diags[0]["action_id"] != nil {
				t.Fatalf("%v", diags)
			}
		})
	}
}

// TestACandidateThatWentStaleIsNotCompactedAndNoHookAfterItIsCalled: after_compact is a fact of a
// checkpoint that was stored. A stale candidate is made again (the compaction's own hook is
// called again, as it is a new compaction step), and only the one that is stored is told.
func TestACandidateThatWentStaleIsNotCompactedAndNoHookAfterItIsCalled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bumps  int
		status string
		after  int
	}{{"once", 1, "completed", 1}, {"twice", 2, "incomplete", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			log := &hookLog{}
			c := newHookCompactionRig(t, harnesstest.Final(goodSummary), hookSetup{extra: []extensions.Named{log.named()}})
			var n int
			c.fake.OnGenerate = func(_ context.Context, req modelport.ChatRequest) {
				if req.Rencrow.Harness.Stage == modelport.StageSummary && n < tc.bumps {
					n++
					c.exec("UPDATE threads SET context_revision=context_revision+1") // the context moved under the candidate
				}
			}
			_, run := c.run("hook.stale.00000000"+tc.name, "大きなファイルを読んで保存処理を実装する。")
			if run.Result.Status != tc.status {
				t.Fatalf("%+v", run.Result)
			}
			if got := len(log.at(extensions.AfterCompact)); got != tc.after || len(log.at(extensions.BeforeCompact)) != 2 || len(c.checkpoints()) != tc.after {
				t.Fatalf("after_compact %d, before_compact %d, checkpoints %v", got, len(log.at(extensions.BeforeCompact)), c.checkpoints())
			}
		})
	}
}

// TestAStageOfUnknownOutcomeAndItsCheckpointAreNotChangedByHooks: the Summary generation has no
// known end, the reduction that needs no model is stored, and the Run, which does not generate
// again while one of its generations is unresolved, ends blocked with it listed. The hooks are
// told each of these and, whether they continue or fail, the Run ends as it does with none.
func TestAStageOfUnknownOutcomeAndItsCheckpointAreNotChangedByHooks(t *testing.T) {
	want := "blocked/MODEL_GENERATION_OUTCOME_UNKNOWN/resumable/unresolved"
	for name, extra := range map[string]func(*hookLog) []extensions.Named{
		"hooks that continue": func(l *hookLog) []extensions.Named { return []extensions.Named{l.named()} },
		"hooks after that fail": func(l *hookLog) []extensions.Named {
			return []extensions.Named{l.named(), onPoint(extensions.AfterModel, cbError), onPoint(extensions.AfterCompact, cbPanic), onPoint(extensions.RunTerminal, cbDeny("LATE"))}
		},
	} {
		t.Run(name, func(t *testing.T) {
			log := &hookLog{}
			c := newHookCompactionRig(t, harnesstest.Reply{Kind: harnesstest.KindTransportError}, hookSetup{extra: extra(log), hard: 40 * time.Millisecond})
			start, run := c.run("hook.sunk.0000000001", "大きなファイルを読んで保存処理を実装する。")
			res := run.Result
			if resultKey(res) != want || res.LastCheckpointID == nil || run.GenerationAttemptsUnknown != 1 || len(res.UnresolvedActionIDs) != 1 {
				t.Fatalf("%+v", res)
			}
			if cps := c.checkpoints(); len(cps) != 1 || !strings.Contains(cps[0], "/emergency/") || len(c.acts) != 1 {
				t.Fatalf("checkpoints %v, act steps %d", cps, len(c.acts))
			}
			if got := log.points(); got != "before_model,after_model,before_tool,after_tool,before_compact,before_model,after_model,after_compact,run_terminal" {
				t.Fatal(got)
			}
			am := log.at(extensions.AfterModel)[1]
			if *am.Stage != modelport.StageSummary || *am.Code != "unknown" || *am.ActionID != res.UnresolvedActionIDs[0] {
				t.Fatalf("%+v", am)
			}
			if ac := log.at(extensions.AfterCompact)[0]; *ac.Code != "emergency" {
				t.Fatalf("%+v", ac)
			}
			if end := log.at(extensions.RunTerminal)[0]; *end.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" {
				t.Fatalf("%+v", end)
			}
			_ = start
		})
	}
}

// TestHooksAroundAManualCompactionAreTheSameAsAnAutomaticOnes: the manual compaction's system Run
// goes through the compaction points (and its Summary's model points) and ends at the Run's end
// point, with no Tool point; a deny before it is the CompactResult's error and nothing is stored.
func TestHooksAroundAManualCompactionAreTheSameAsAnAutomaticOnes(t *testing.T) {
	t.Run("the points", func(t *testing.T) {
		log := &hookLog{}
		c, _ := manualScenarioWith(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}, hookRigBuild(hookSetup{extra: []extensions.Named{log.named()}}))
		thread := c.val("SELECT thread_id FROM threads")
		log.reset()
		acc := c.compact(thread, "manual.hook.00000001", false)
		res := c.waitReceipt(acc.ReceiptID)
		if res.Status != "executed" || res.CheckpointID == nil {
			t.Fatalf("%+v", res)
		}
		// The receipt turns terminal in the Run's own terminal transaction, which the end point
		// precedes: every point has been called by the time the result can be read.
		if got := log.points(); got != "before_compact,before_model,after_model,after_compact,run_terminal" {
			t.Fatal(got)
		}
		checkInputs(t, log.inputs(), thread, c.systemRun())
		if end := log.at(extensions.RunTerminal)[0]; *end.Code != "COMPACTION_COMMITTED" {
			t.Fatalf("%+v", end)
		}
	})
	t.Run("a deny", func(t *testing.T) {
		c, _ := manualScenarioWith(t, map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}, hookRigBuild(hookSetup{extra: []extensions.Named{onPoint(extensions.BeforeCompact, cbDeny("NO_COMPACTION"))}}))
		thread := c.val("SELECT thread_id FROM threads")
		acc := c.compact(thread, "manual.hook.00000002", false)
		res := c.waitReceipt(acc.ReceiptID)
		if res.Status != "unavailable" || res.Error == nil || res.Error.Code != "HOST_HOOK_DENIED" || res.CheckpointID != nil || res.Outcome != nil {
			t.Fatalf("%+v", res)
		}
		if len(c.checkpoints()) != 0 || len(c.stages[modelport.StageSummary]) != 0 {
			t.Fatalf("checkpoints %v", c.checkpoints())
		}
		if got := c.val("SELECT status||'/'||COALESCE(json_extract(result_json,'$.code'),'') FROM runs WHERE run_id=?", c.systemRun()); got != "blocked/HOST_HOOK_DENIED" {
			t.Fatal(got)
		}
	})
}

// the end of the Run ---------------------------------------------------------------------

// TestTheRunEndHookIsToldHowTheRunEndedOnceWhateverTheEndWas: completed, blocked by a hook and
// cancelled, each Run calls it once with the code it ends with, and the record of the audit hook
// at that point is part of the evidence the result lists.
func TestTheRunEndHookIsToldHowTheRunEndedOnceWhateverTheEndWas(t *testing.T) {
	check := func(t *testing.T, r *rig, log *hookLog, run protocol.RunInfo) {
		t.Helper()
		end := log.at(extensions.RunTerminal)
		if len(end) != 1 || end[0].Code == nil || *end[0].Code != run.Result.Code || end[0].RunID != run.RunID {
			t.Fatalf("%+v %+v", end, run.Result)
		}
		recs := r.auditRecords(run.RunID)
		if len(recs) == 0 || recs[len(recs)-1]["hook"] != "run_terminal" || recs[len(recs)-1]["code"] != run.Result.Code {
			t.Fatalf("%v", recs)
		}
		id := r.val("SELECT evidence_id FROM evidence WHERE run_id=? AND json_extract(metadata_json,'$.purpose')='hook_audit' ORDER BY rowid DESC LIMIT 1", run.RunID)
		listed := false
		for _, e := range run.Result.EvidenceIDs {
			listed = listed || e == id
		}
		if !listed {
			t.Fatalf("the result does not list the end record %s", id)
		}
	}
	t.Run("completed", func(t *testing.T) {
		fake := harnesstest.NewFake()
		fake.SetScript(harnesstest.Final("done"))
		log := &hookLog{}
		r, _ := extRig(t, fake, hookSetup{configured: []string{extensions.AuditMetadata}, extra: []extensions.Named{log.named()}})
		info := r.openSession("hook.end1.open.000000001")
		run := r.waitTerminal(r.startRun(info.ThreadID, "hook.end1.start.00000001", "go").RunID)
		if run.Result.Status != "completed" {
			t.Fatalf("%+v", run.Result)
		}
		check(t, r, log, run)
	})
	t.Run("blocked by a hook", func(t *testing.T) {
		fake := harnesstest.NewFake()
		fake.SetScript(harnesstest.Final("never"))
		log := &hookLog{}
		r, _ := extRig(t, fake, hookSetup{configured: []string{extensions.AuditMetadata}, extra: []extensions.Named{log.named(), onPoint(extensions.BeforeModel, cbDeny("NO"))}})
		info := r.openSession("hook.end2.open.000000001")
		run := r.waitTerminal(r.startRun(info.ThreadID, "hook.end2.start.00000001", "go").RunID)
		if resultKey(run.Result) != "blocked/HOST_HOOK_DENIED/resumable/none" {
			t.Fatalf("%+v", run.Result)
		}
		check(t, r, log, run)
	})
	t.Run("cancelled", func(t *testing.T) {
		entered := make(chan struct{})
		var once sync.Once
		wait := onPoint(extensions.BeforeModel, func(ctx context.Context, _ extensions.HookInput, _ extensions.Recorder) (extensions.HookResult, error) {
			once.Do(func() { close(entered) })
			<-ctx.Done()
			return extensions.Continue(), nil
		})
		fake := harnesstest.NewFake()
		fake.SetScript(harnesstest.Final("never"))
		log := &hookLog{}
		r, _ := extRig(t, fake, hookSetup{configured: []string{extensions.AuditMetadata}, extra: []extensions.Named{log.named(), wait}, hard: 30 * time.Second})
		info := r.openSession("hook.end3.open.000000001")
		start := r.startRun(info.ThreadID, "hook.end3.start.00000001", "go")
		select {
		case <-entered:
		case <-time.After(20 * time.Second):
			t.Fatal("the hook was not called")
		}
		r.interrupt(start.RunID, 0, "hook.end3.key.0000000001")
		run := r.waitTerminal(start.RunID)
		if run.Result.Status != "cancelled" {
			t.Fatalf("%+v", run.Result)
		}
		check(t, r, log, run)
	})
}

// TestAHookAtTheRunEndCannotChangeTheResult: whatever it does (fail, deny, run over, panic), the
// result is the one the Run was ending with, and a diagnosis is left.
func TestAHookAtTheRunEndCannotChangeTheResult(t *testing.T) {
	for name, cb := range failsAfter() {
		t.Run(name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			fake.SetScript(harnesstest.Final("all done"))
			r, _ := extRig(t, fake, hookSetup{extra: []extensions.Named{onPoint(extensions.RunTerminal, cb)}, hard: 40 * time.Millisecond})
			info := r.openSession("hook.endx.open.000000001")
			start := r.startRun(info.ThreadID, "hook.endx.start.00000001", "go")
			run := r.waitTerminal(start.RunID)
			if run.Result.Status != "completed" || run.Result.FinalText != "all done" || run.Result.Code != "FINAL_RESPONSE_ACCEPTED" {
				t.Fatalf("%+v", run.Result)
			}
			diags := r.diagnoses(start.RunID)
			if len(diags) != 1 || diags[0]["hook"] != "run_terminal" || diags[0]["action_id"] != nil {
				t.Fatalf("%v", diags)
			}
			listed := false
			for _, e := range run.Result.EvidenceIDs {
				listed = listed || strings.Contains(r.val("SELECT json_extract(metadata_json,'$.purpose') FROM evidence WHERE evidence_id=?", e), "hook_diagnosis")
			}
			if !listed {
				t.Fatal("the result does not list the diagnosis")
			}
		})
	}
}

// TestAStopRecordedAtTheLastInstantIsTheStoredResultAndNotWhatTheEndHookWasTold: the end hook is
// told the outcome the driver is about to store; the stored result is the record. A stop that is
// recorded while the hook runs makes the compare-and-set of the terminal transaction refuse a
// completed result, as it does without a hook, and the Run ends cancelled with the answer not
// adopted. The hook is not called again.
func TestAStopRecordedAtTheLastInstantIsTheStoredResultAndNotWhatTheEndHookWasTold(t *testing.T) {
	var r *rig
	log := &hookLog{}
	interrupter := onPoint(extensions.RunTerminal, func(_ context.Context, in extensions.HookInput, _ extensions.Recorder) (extensions.HookResult, error) {
		res, _, err := r.svc.TurnInterrupt(context.Background(), mustParams(protocol.InterruptInput{RunID: in.RunID, ExpectedControlRevision: 0, IdempotencyKey: "hook.endrace.key.00001"}))
		if err != nil || res.Code != "CANCEL_REQUESTED" {
			return extensions.HookResult{}, errors.New("the stop was not recorded")
		}
		return extensions.Continue(), nil
	})
	fake := harnesstest.NewFake()
	fake.SetScript(harnesstest.Final("the answer"))
	r, _ = extRig(t, fake, hookSetup{extra: []extensions.Named{log.named(), interrupter}, hard: 30 * time.Second})
	info := r.openSession("hook.endrace.open.0000001")
	start := r.startRun(info.ThreadID, "hook.endrace.start.000001", "go")
	run := r.waitTerminal(start.RunID)
	if run.Result.Status != "cancelled" || run.Result.Code != "CANCELLED" || run.Result.FinalText != "" || run.Result.FinalMessageID != nil {
		t.Fatalf("%+v", run.Result)
	}
	end := log.at(extensions.RunTerminal)
	if len(end) != 1 || *end[0].Code != "FINAL_RESPONSE_ACCEPTED" {
		t.Fatalf("%+v", end)
	}
}
