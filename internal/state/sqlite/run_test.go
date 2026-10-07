package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

var bg = context.Background()

// run is an admitted Run and the fence of its driver (the thread is seeded at epoch 1).
type run struct {
	e     *env
	start protocol.StartResult
	fence Fence
}

func (e *env) newRun(key string) *run {
	e.t.Helper()
	out := e.mustAdmit(e.thread, key, nil)
	return &run{e: e, start: out.Result, fence: Fence{ThreadID: e.thread, RunID: out.Result.RunID, Epoch: 1}}
}

func (r *run) phase(from, to string) {
	r.e.t.Helper()
	if err := r.e.s.SetPhase(bg, r.fence, from, to); err != nil {
		r.e.t.Fatalf("%s -> %s: %v", from, to, err)
	}
}

func (r *run) goTo(phase string) {
	r.e.t.Helper()
	prev := "Admitting"
	for _, p := range []string{"Loading", "Assembling", "Measuring", "Generating", "ValidatingFinal", "PersistingResult"} {
		r.phase(prev, p)
		prev = p
		if p == phase {
			return
		}
	}
}

func (r *run) reserve() Reservation {
	r.e.t.Helper()
	in := reserveInput()
	rsv, err := r.e.s.ReserveGeneration(bg, r.fence, in)
	if err != nil {
		r.e.t.Fatal(err)
	}
	return rsv
}

func reserveInput() ReserveInput {
	return ReserveInput{
		Stage: "act", RequestID: identity.NewRequestID().String(), ArgsBytes: []byte(`{"logical":"input"}`), RequestBytes: []byte(`{"request":"bytes"}`),
		InputDigest: strings.Repeat("a", 64), RequestDigest: strings.Repeat("b", 64), BindingFingerprint: "bfp-v1:fixture",
		ProfileID: "same_request", ProfileRevision: "builtin-v1", Stream: true,
	}
}

func (e *env) eventTypes() string {
	e.t.Helper()
	evs, _, err := e.s.EventsAfter(bg, e.thread, 0, 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	types := make([]string, len(evs))
	for i, ev := range evs {
		types[i] = ev.Type
	}
	return strings.Join(types, ",")
}

func one(n int64) *int64 { return &n }

func TestTheStoreStepsOfARunFromAdmissionToItsResult(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("run.key.00000000000001")

	rec, err := e.s.LoadRun(bg, r.start.RunID)
	if err != nil || rec.Phase != "Admitting" || rec.Status != "running" || rec.RunEpoch != 1 || rec.ThreadEpoch != 1 || rec.Limits != hostLimits ||
		rec.Binding.Selector != "fixture-local" || rec.StartReceiptID != r.start.ReceiptID || rec.StartMessageID != r.start.Intake.MessageID ||
		rec.InputEvidenceID != r.start.Intake.EvidenceID || rec.Owner != "core:local" || rec.ModelSteps != 0 || rec.AttemptsUsed != 0 ||
		!rec.DeadlineAt.Equal(time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC)) || rec.RecoveryRevision != designRecoveryRevision {
		t.Fatalf("%+v %v", rec, err)
	}

	r.phase("Admitting", "Loading")
	if err := e.s.SetPhase(bg, r.fence, "Admitting", "Assembling"); !errors.Is(err, ErrPhaseConflict) {
		t.Fatalf("a phase that is not the current one: %v", err)
	}
	evs, err := e.s.ApplyInput(bg, r.fence)
	if err != nil || len(evs) != 1 || evs[0].Type != "input.applied" || evs[0].MessageID == nil || *evs[0].MessageID != r.start.Intake.MessageID {
		t.Fatalf("%v %v", evs, err)
	}
	if v := scalar[string](t, e.s.db, "SELECT context_revision||'/'||event_seq FROM threads WHERE thread_id=?", e.thread); v != "1/4" {
		t.Fatalf("revision/event_seq %s", v)
	}
	if again, err := e.s.ApplyInput(bg, r.fence); err != nil || len(again) != 0 || scalar[int](t, e.s.db, "SELECT COUNT(*) FROM context_entries") != 1 {
		t.Fatalf("applying the input twice changed something: %v %v", again, err)
	}

	snap, err := e.s.LoadSnapshot(bg, r.start.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ContextRevision != 1 || snap.PendingInputs != 0 || snap.CurrentCheckpointID != nil || len(snap.Applied) != 1 || snap.WriterEpoch != 1 {
		t.Fatalf("%+v", snap)
	}
	a := snap.Applied[0]
	if a.MessageID != r.start.Intake.MessageID || a.ContextSeq != 1 || a.HistoryKind != "AutomationInstruction" || a.Origin != "automation" || a.Text == "" {
		t.Fatalf("%+v", a)
	}
	// The four typed blocks of the design example, in the order they were given, with
	// the exact text and the revision they were admitted with.
	if len(snap.Blocks) != 4 {
		t.Fatalf("%d blocks", len(snap.Blocks))
	}
	for _, b := range snap.Blocks {
		rev, err := protocol.ContextRevision(b.Kind, b.Text, b.Source)
		if err != nil || rev != b.Revision {
			t.Fatalf("%+v", b)
		}
	}

	r.phase("Loading", "Assembling")
	r.phase("Assembling", "Measuring")
	if _, err := e.s.RecordEvidence(bg, r.fence, "measure_result", "application/json", false, []byte(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	r.phase("Measuring", "Generating")
	rsv := r.reserve()
	if rsv.Ordinal != 0 || rsv.AttemptsUsed != 1 || len(rsv.Events) != 4 {
		t.Fatalf("%+v", rsv)
	}
	var got []string
	for _, ev := range rsv.Events {
		got = append(got, ev.Type)
	}
	if strings.Join(got, ",") != "action.prepared,action.dispatch_started,model.attempt_started,model.requested" {
		t.Fatalf("%v", got)
	}
	if v := scalar[string](t, e.s.db, "SELECT state FROM attempts WHERE attempt_id=?", rsv.AttemptID); v != "dispatch_started" {
		t.Fatalf("a reserved attempt is a dispatched one: %s", v)
	}
	if v := scalar[string](t, e.s.db, "SELECT generation_state||'/'||(backend_attempts IS NULL) FROM model_calls WHERE attempt_id=?", rsv.AttemptID); v != "unknown/1" {
		t.Fatalf("until it ends, what the generation did is unknown: %s", v)
	}
	if v := scalar[int](t, e.s.db, "SELECT generation_attempts_used FROM runs WHERE run_id=?", r.start.RunID); v != 1 {
		t.Fatalf("attempts used %d", v)
	}

	usage := []byte(`{"cached_tokens":null,"completion_tokens":2,"prompt_tokens":9}`)
	end, err := e.s.RecordAttemptEnd(bg, r.fence, AttemptEnd{
		AttemptID: rsv.AttemptID, State: "completed", Outcome: "completed", GenerationState: "terminal", BackendAttempts: one(1),
		ResponseID: protocol.Str(identity.NewResponseID().String()), RawResponse: []byte("data: [DONE]\n\n"), UsageJSON: usage, UsageComplete: true,
	})
	if err != nil || len(end.Events) != 1 || end.Events[0].Type != "model.completed" || end.ResponseEvidenceID == nil || end.ReceiptEvidenceID == "" {
		t.Fatalf("%+v %v", end, err)
	}
	if end.Events[0].EvidenceID == nil || *end.Events[0].EvidenceID != end.ReceiptEvidenceID {
		t.Fatal("model.completed traces the attempt receipt")
	}
	if _, err := e.s.RecordAttemptEnd(bg, r.fence, AttemptEnd{AttemptID: rsv.AttemptID, State: "failed", Outcome: "error", GenerationState: "terminal", BackendAttempts: one(1)}); !errors.Is(err, ErrAttemptEnded) {
		t.Fatalf("an attempt ends once: %v", err)
	}
	if v := scalar[string](t, e.s.db, "SELECT state FROM attempts WHERE attempt_id=?", rsv.AttemptID); v != "completed" {
		t.Fatalf("the second end changed the attempt: %s", v)
	}

	r.phase("Generating", "ValidatingFinal")
	r.phase("ValidatingFinal", "PersistingResult")
	final := identity.NewMessageID().String()
	finalEv, resultEv := identity.NewEvidenceID().String(), identity.NewEvidenceID().String()
	evidence, err := e.s.RunEvidenceIDs(bg, r.start.RunID)
	if err != nil || len(evidence) != 4 {
		t.Fatalf("%v %v", evidence, err)
	}
	result := protocol.RunResult{
		RunID: r.start.RunID, TaskID: r.start.TaskID, Status: "completed", Code: "FINAL_RESPONSE_ACCEPTED", FinalMessageID: &final, FinalText: "the answer",
		Verification: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}}, EvidenceIDs: append(evidence, finalEv), UnresolvedActionIDs: []string{},
	}
	bad := TerminalInput{Result: result, FinalText: "the answer", FinalEvidenceID: finalEv, ResultEvidenceID: resultEv}
	wrong := bad
	wrong.FinalText = ""
	if _, err := e.s.PersistTerminal(bg, r.fence, wrong); err == nil {
		t.Fatal("a result that names a final message without its text was stored")
	}
	if scalar[string](t, e.s.db, "SELECT status FROM runs WHERE run_id=?", r.start.RunID) != "running" {
		t.Fatal("a refused result changed the Run")
	}
	events, err := e.s.PersistTerminal(bg, r.fence, bad)
	if err != nil || len(events) != 1 || events[0].Type != "run.terminal" || events[0].Code == nil || *events[0].Code != "FINAL_RESPONSE_ACCEPTED" {
		t.Fatalf("%v %v", events, err)
	}
	if _, err := e.s.PersistTerminal(bg, r.fence, bad); !errors.Is(err, ErrRunNotRunning) {
		t.Fatalf("a Run ends once: %v", err)
	}
	if v := scalar[string](t, e.s.db, "SELECT COUNT(*) FROM events WHERE type='run.terminal'"); v != "1" {
		t.Fatalf("%s run.terminal events", v)
	}
	if v := scalar[string](t, e.s.db, "SELECT phase||'/'||status||'/'||(ended_at IS NOT NULL)||'/'||(result_json IS NOT NULL) FROM runs"); v != "Terminal/completed/1/1" {
		t.Fatal(v)
	}
	if v := scalar[string](t, e.s.db, "SELECT COALESCE(active_run_id,'none')||'/'||context_revision FROM threads"); v != "none/2" {
		t.Fatalf("the Thread is free and the final message is applied: %s", v)
	}
	// The result is the one run/get returns, and the final text is Evidence.
	info, err := e.s.GetRun(bg, e.caller, r.start.RunID)
	if err != nil || !info.Terminal || info.Result == nil || info.Result.FinalText != "the answer" || info.Phase != "Terminal" {
		t.Fatalf("%+v %v", info, err)
	}
	if e.eventTypes() != "input.accepted,task.created,run.started,input.applied,action.prepared,action.dispatch_started,model.attempt_started,model.requested,model.completed,run.terminal" {
		t.Fatalf("%s", e.eventTypes())
	}
}

func TestADriverThatLostTheThreadOrWasCancelledWritesNothing(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("run.key.00000000000002")
	r.goTo("Generating")
	before := e.counts()
	beforeActions := scalar[int](t, e.s.db, "SELECT COUNT(*) FROM actions")

	// Another driver took the Thread over: every write of this one is refused.
	if _, err := e.s.BumpWriterEpoch(bg, e.thread); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ReserveGeneration(bg, r.fence, reserveInput()); !errors.Is(err, ErrWriterLost) {
		t.Fatalf("reserve: %v", err)
	}
	if err := e.s.SetPhase(bg, r.fence, "Generating", "ValidatingFinal"); !errors.Is(err, ErrWriterLost) {
		t.Fatalf("phase: %v", err)
	}
	if _, err := e.s.ApplyInput(bg, r.fence); !errors.Is(err, ErrWriterLost) {
		t.Fatalf("apply: %v", err)
	}
	if _, err := e.s.RecordEvidence(bg, r.fence, "x", "application/json", false, []byte("{}")); !errors.Is(err, ErrWriterLost) {
		t.Fatalf("evidence: %v", err)
	}
	if _, err := e.s.PersistTerminal(bg, r.fence, TerminalInput{}); !errors.Is(err, ErrWriterLost) {
		t.Fatalf("terminal: %v", err)
	}
	e.wantNoChange(before)
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM actions") != beforeActions {
		t.Fatal("a refused reservation left an action")
	}

	// Cancelled: the control revision moved after the driver read it, so nothing is dispatched.
	e2 := newEnv(t)
	r2 := e2.newRun("run.key.00000000000003")
	r2.goTo("Generating")
	mustExec(t, e2.s.db, "UPDATE threads SET control_revision=control_revision+1")
	n := e2.counts()
	if _, err := e2.s.ReserveGeneration(bg, r2.fence, reserveInput()); !errors.Is(err, ErrControlChanged) {
		t.Fatalf("%v", err)
	}
	e2.wantNoChange(n)
	if scalar[int](t, e2.s.db, "SELECT generation_attempts_used FROM runs") != 0 || scalar[int](t, e2.s.db, "SELECT COUNT(*) FROM attempts") != 0 {
		t.Fatal("a cancelled reservation consumed an attempt")
	}
}

func TestAReservationChecksPhaseDeadlineAndBudgetsBeforeItWritesAnything(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(e *env, r *run)
		want  error
	}{
		"not in Generating": {func(e *env, r *run) {}, ErrPhaseConflict},
		"the deadline passed": {func(e *env, r *run) {
			r.goTo("Generating")
			mustExec(t, e.s.db, "UPDATE runs SET deadline_at='2026-10-07T12:00:00Z'") // the store's clock reads 12:00:00
		}, ErrDeadlinePassed},
		"the attempt budget is spent": {func(e *env, r *run) {
			r.goTo("Generating")
			mustExec(t, e.s.db, "UPDATE runs SET generation_attempts_used=32")
		}, ErrGenerationBudget},
		"the model steps are spent": {func(e *env, r *run) {
			r.goTo("Generating")
			mustExec(t, e.s.db, `UPDATE runs SET limits_json=replace(limits_json,'"max_model_steps":10','"max_model_steps":1')`)
			mustExec(t, e.s.db, `INSERT INTO actions(action_id,run_id,kind,name,args_bytes,args_hash,status,created_at) VALUES(?,?,'model','act',x'7b7d',?,'completed','2026-10-07T12:00:00Z')`,
				identity.NewActionID().String(), r.start.RunID, strings.Repeat("c", 64))
		}, ErrStepBudget},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			r := e.newRun("run.key.00000000000004")
			tc.setup(e, r)
			actions := scalar[int](t, e.s.db, "SELECT COUNT(*) FROM actions")
			events := scalar[int](t, e.s.db, "SELECT COUNT(*) FROM events")
			used := scalar[int](t, e.s.db, "SELECT generation_attempts_used FROM runs")
			if _, err := e.s.ReserveGeneration(bg, r.fence, reserveInput()); !errors.Is(err, tc.want) {
				t.Fatalf("%v, want %v", err, tc.want)
			}
			if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM actions") != actions || scalar[int](t, e.s.db, "SELECT COUNT(*) FROM events") != events ||
				scalar[int](t, e.s.db, "SELECT generation_attempts_used FROM runs") != used || scalar[int](t, e.s.db, "SELECT COUNT(*) FROM attempts") != 0 {
				t.Fatal("a refused reservation wrote something")
			}
		})
	}
}

// TestAnUnknownAttemptIsRecordedAsUnknownAndNeverAsACount keeps the DDL's own rule in
// view: an attempt whose end is not known has no backend count.
func TestAnUnknownAttemptIsRecordedAsUnknownAndNeverAsACount(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("run.key.00000000000005")
	r.goTo("Generating")
	rsv := r.reserve()
	code := "MODEL_GENERATION_OUTCOME_UNKNOWN"
	end, err := e.s.RecordAttemptEnd(bg, r.fence, AttemptEnd{AttemptID: rsv.AttemptID, State: "unknown", Outcome: "error", GenerationState: "unknown", FailureCode: &code})
	if err != nil || len(end.Events) != 1 {
		t.Fatalf("%v", err)
	}
	if v := scalar[string](t, e.s.db, "SELECT state FROM attempts"); v != "unknown" {
		t.Fatal(v)
	}
	if v := scalar[string](t, e.s.db, "SELECT generation_attempts_used||'/'||generation_attempts_unknown FROM runs"); v != "1/1" {
		t.Fatalf("an unknown attempt stays consumed and is counted unknown: %s", v)
	}
	// An end that contradicts itself is refused by the schema, not stored.
	e2 := newEnv(t)
	r2 := e2.newRun("run.key.00000000000006")
	r2.goTo("Generating")
	rsv2 := r2.reserve()
	if _, err := e2.s.RecordAttemptEnd(bg, r2.fence, AttemptEnd{AttemptID: rsv2.AttemptID, State: "completed", Outcome: "completed", GenerationState: "unknown", BackendAttempts: one(1)}); err == nil {
		t.Fatal("an unknown generation with a backend count was stored")
	}
	if v := scalar[string](t, e2.s.db, "SELECT state FROM attempts"); v != "dispatch_started" {
		t.Fatalf("a refused end changed the attempt: %s", v)
	}
}

func TestTerminalizeStaleEndsEveryRunOfAnOlderEpochAndNothingElse(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("run.key.00000000000007")
	r.goTo("Generating")
	rsv := r.reserve()

	decide := func(seen *StaleRun) func(StaleRun) (TerminalInput, error) {
		return func(st StaleRun) (TerminalInput, error) {
			*seen = st
			result := protocol.RunResult{
				RunID: st.RunID, TaskID: st.TaskID, Status: "blocked", Code: "MODEL_GENERATION_OUTCOME_UNKNOWN",
				Verification: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}}, EvidenceIDs: st.EvidenceIDs, Resumable: true,
				UnresolvedActionIDs: []string{},
			}
			for _, u := range st.Unresolved {
				result.UnresolvedActionIDs = append(result.UnresolvedActionIDs, u.ActionID)
			}
			return TerminalInput{Result: result, ResultEvidenceID: identity.NewEvidenceID().String()}, nil
		}
	}
	var seen StaleRun
	// Nothing is stale while the epoch is the Run's own: nothing is written.
	n := e.counts()
	if evs, err := e.s.TerminalizeStale(bg, e.thread, 1, nil, decide(&seen)); err != nil || len(evs) != 0 || seen.RunID != "" {
		t.Fatalf("%v %v", evs, err)
	}
	e.wantNoChange(n)

	// The epoch the caller names must be the Thread's.
	if _, err := e.s.TerminalizeStale(bg, e.thread, 2, nil, decide(&seen)); !errors.Is(err, ErrWriterLost) {
		t.Fatalf("%v", err)
	}

	if _, err := e.s.BumpWriterEpoch(bg, e.thread); err != nil {
		t.Fatal(err)
	}
	evs, err := e.s.TerminalizeStale(bg, e.thread, 2, nil, decide(&seen))
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, ev := range evs {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != "model.completed,run.terminal" {
		t.Fatalf("%v", types)
	}
	if seen.RunID != r.start.RunID || seen.Phase != "Generating" || len(seen.Unresolved) != 1 || seen.Unresolved[0].AttemptID != rsv.AttemptID ||
		seen.Unresolved[0].ActionID != rsv.ActionID || !seen.Now.Equal(testNow) || !seen.DeadlineAt.Equal(testNow.Add(30*time.Minute)) {
		t.Fatalf("%+v", seen)
	}
	// The receipt of the unknown attempt was written before the decision, so the decision lists it.
	listed := false
	for _, id := range seen.EvidenceIDs {
		listed = listed || id == *evs[0].EvidenceID
	}
	if !listed {
		t.Fatal("the unknown attempt's receipt Evidence is not among the Run's Evidence")
	}
	if v := scalar[string](t, e.s.db, "SELECT status||'/'||generation_attempts_unknown FROM runs"); v != "blocked/1" {
		t.Fatal(v)
	}
	if v := scalar[string](t, e.s.db, "SELECT COALESCE(active_run_id,'none') FROM threads"); v != "none" {
		t.Fatalf("the Thread must be free: %s", v)
	}
	// A second call finds nothing.
	if evs, err := e.s.TerminalizeStale(bg, e.thread, 2, nil, decide(&seen)); err != nil || len(evs) != 0 {
		t.Fatalf("%v %v", evs, err)
	}
	// A Run of the current epoch is not stale, however old it is.
	adm := e.adm
	adm.WriterEpoch = 2
	out, err := e.s.AdmitStart(bg, adm, e.params(e.thread, "run.key.00000000000008", nil))
	if err != nil {
		t.Fatal(err)
	}
	if evs, err := e.s.TerminalizeStale(bg, e.thread, 2, nil, decide(&seen)); err != nil || len(evs) != 0 {
		t.Fatalf("%v %v", evs, err)
	}
	if v := scalar[string](t, e.s.db, "SELECT status FROM runs WHERE run_id=?", out.Result.RunID); v != "running" {
		t.Fatalf("a Run of the current epoch was settled: %s", v)
	}

	// A decision that cannot be made leaves everything as it was.
	e3 := newEnv(t)
	r3 := e3.newRun("run.key.00000000000009")
	r3.goTo("Generating")
	r3.reserve()
	if _, err := e3.s.BumpWriterEpoch(bg, e3.thread); err != nil {
		t.Fatal(err)
	}
	n3 := e3.counts()
	if _, err := e3.s.TerminalizeStale(bg, e3.thread, 2, nil, func(StaleRun) (TerminalInput, error) { return TerminalInput{}, errors.New("no") }); err == nil {
		t.Fatal("a failed decision was stored")
	}
	e3.wantNoChange(n3)
	if scalar[string](t, e3.s.db, "SELECT state FROM attempts") != "dispatch_started" {
		t.Fatal("a failed settlement changed the attempt")
	}
}

func TestASnapshotThatDoesNotMatchItsRecordsIsIntegrityBlocked(t *testing.T) {
	t.Run("an input whose bytes changed", func(t *testing.T) {
		e := newEnv(t)
		r := e.newRun("run.key.00000000000010")
		r.phase("Admitting", "Loading")
		if _, err := e.s.ApplyInput(bg, r.fence); err != nil {
			t.Fatal(err)
		}
		mustExec(t, e.s.db, "DROP TRIGGER evidence_sealed_no_update")
		mustExec(t, e.s.db, "UPDATE evidence SET raw_hash=? WHERE evidence_id=?", strings.Repeat("0", 64), r.start.Intake.EvidenceID)
		_, err := e.s.LoadSnapshot(bg, r.start.RunID)
		wantCode(t, err, protocol.CodeIntegrityBlocked)
	})
	t.Run("a typed block whose revision does not match", func(t *testing.T) {
		e := newEnv(t)
		r := e.newRun("run.key.00000000000011")
		mustExec(t, e.s.db, "DROP TRIGGER items_no_update")
		mustExec(t, e.s.db, `UPDATE items SET metadata_json=json_set(metadata_json,'$.revision','ctx-v1:'||?) WHERE history_kind='HostContext' AND json_extract(metadata_json,'$.block_kind')='recall_pack'`,
			strings.Repeat("0", 64))
		_, err := e.s.LoadSnapshot(bg, r.start.RunID)
		wantCode(t, err, protocol.CodeIntegrityBlocked)
	})
	t.Run("an evidence that is not there", func(t *testing.T) {
		e := newEnv(t)
		r := e.newRun("run.key.00000000000012")
		r.phase("Admitting", "Loading")
		if _, err := e.s.ApplyInput(bg, r.fence); err != nil {
			t.Fatal(err)
		}
		mustExec(t, e.s.db, "PRAGMA foreign_keys=OFF")
		mustExec(t, e.s.db, "DROP TRIGGER chunks_no_delete")
		mustExec(t, e.s.db, "DELETE FROM evidence_chunks WHERE evidence_id=?", r.start.Intake.EvidenceID)
		_, err := e.s.LoadSnapshot(bg, r.start.RunID)
		wantCode(t, err, protocol.CodeIntegrityBlocked)
	})
}

func TestEvidenceOfARunIsReadableByTheThreadOwnerAndNotAsText(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("run.key.00000000000013")
	r.goTo("Generating")
	rsv := r.reserve()
	for _, tc := range []struct {
		version string
		ok      bool
	}{{"raw/v1", true}, {"text/v1", false}} {
		_, err := e.s.ReadEvidence(bg, e.caller, protocol.EvidenceReadInput{EvidenceID: rsv.RequestEvidenceID, ProjectionVersion: tc.version, Range: protocol.ByteRange{Start: 0, End: 5}})
		if tc.ok && err != nil {
			t.Fatalf("%s: %v", tc.version, err)
		}
		if !tc.ok {
			wantCode(t, err, protocol.CodeUnsupportedContract)
		}
	}
}
