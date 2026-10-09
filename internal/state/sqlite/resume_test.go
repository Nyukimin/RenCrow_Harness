package sqlite

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

var resumeLimits = map[string]any{"max_model_steps": 5, "max_tool_calls_per_step": 4, "deadline_seconds": 600, "max_capture_bytes": 1048576, "max_generation_attempts": 8}

func resumeParams(taskID, lastRun string, expectedControl int64, key string, mod func(m map[string]any)) []byte {
	limits := map[string]any{}
	for k, v := range resumeLimits {
		limits[k] = v
	}
	m := map[string]any{
		"task_id": taskID, "expected_last_run_id": lastRun, "checkpoint_id": nil, "expected_control_revision": expectedControl,
		"binding": nil, "idempotency_key": key, "limits": limits,
	}
	if mod != nil {
		mod(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

// resumeAdm is the admission of a driver that holds the Thread at epoch.
func (e *env) resumeAdm(epoch int64, reconciled map[string]string) ResumeAdmission {
	adm := e.adm
	adm.WriterEpoch = epoch
	return ResumeAdmission{
		Admission:      adm,
		BindingAllowed: func(b protocol.Binding) bool { return b.Selector == "fixture-local" },
		Reconciled:     reconciled,
		// The same classification used by the kernel for an unresolved generation or
		// fixed verifier; the Run is blocked and keeps each unresolved Action visible.
		EndsOnUnknownGeneration: func(st StaleRun) (TerminalInput, error) {
			code := "EFFECT_OUTCOME_UNKNOWN"
			if len(st.Unresolved) > 0 {
				code = "MODEL_GENERATION_OUTCOME_UNKNOWN"
			}
			verification := protocol.Verification{Status: "not_run", EvidenceIDs: []string{}}
			if st.Verification != nil {
				verification = *st.Verification
			}
			res := protocol.RunResult{RunID: st.RunID, TaskID: st.TaskID, Status: "blocked", Code: code,
				Verification: verification, EvidenceIDs: st.EvidenceIDs, Resumable: true, UnresolvedActionIDs: []string{}}
			for _, u := range st.Unresolved {
				res.UnresolvedActionIDs = append(res.UnresolvedActionIDs, u.ActionID)
			}
			res.UnresolvedActionIDs = append(res.UnresolvedActionIDs, st.UnresolvedTools...)
			return TerminalInput{Result: res, ResultEvidenceID: identity.NewEvidenceID().String()}, nil
		},
	}
}

// resumable is a Run that used one model step and ended incomplete, with its Thread
// freed (epoch 2).
func (e *env) resumable(key string) *run {
	e.t.Helper()
	r := e.newRun(key)
	r.phase("Admitting", "Loading")
	if _, err := e.s.ApplyInput(bg, r.fence); err != nil {
		e.t.Fatal(err)
	}
	for _, step := range [][2]string{{"Loading", "Assembling"}, {"Assembling", "Measuring"}, {"Measuring", "Generating"}} {
		r.phase(step[0], step[1])
	}
	rsv := r.reserve()
	if _, err := e.s.RecordAttemptEnd(bg, r.fence, AttemptEnd{AttemptID: rsv.AttemptID, State: "completed", Outcome: "completed", GenerationState: "terminal", BackendAttempts: one(1)}); err != nil {
		e.t.Fatal(err)
	}
	e.endRun(r, "incomplete", "STEP_BUDGET_EXHAUSTED", true)
	return r
}

// TestAdmitResumeMakesANewRunOfTheSameTask is run/resume's contract (A01, A11, A47): the
// same Task and Thread, a new Run with a new Trace pointing back at the old one, the
// limits the request gave and a deadline that counts from now; the old Run is not
// touched and what it consumed is carried beside the new Run's zeros; the new Run's own
// events trace to the receipt of the resume; and the same request is the same answer.
func TestAdmitResumeMakesANewRunOfTheSameTask(t *testing.T) {
	e := newEnv(t)
	r := e.resumable("res.run.0000000000000001")
	oldResult := scalar[string](t, e.s.db, "SELECT result_json FROM runs WHERE run_id=?", r.start.RunID)
	oldRow := scalar[string](t, e.s.db, "SELECT phase||'|'||status||'|'||ended_at||'|'||trace_id||'|'||generation_attempts_used FROM runs WHERE run_id=?", r.start.RunID)
	before := e.counts()

	params := resumeParams(r.start.TaskID, r.start.RunID, 0, "res.key.0000000000000001", nil)
	out, err := e.s.AdmitResume(bg, e.resumeAdm(2, nil), params)
	if err != nil {
		t.Fatal(err)
	}
	res := out.Result
	if out.Replayed || res.TaskID != r.start.TaskID || res.ThreadID != e.thread || res.PreviousRunID != r.start.RunID || res.RunID == r.start.RunID || !strings.HasPrefix(res.RunID, "run_") ||
		res.TraceID == r.start.TraceID || !strings.HasPrefix(res.TraceID, "trc_") || res.CheckpointID != nil || !strings.HasPrefix(res.ReceiptID, "rcp_") ||
		res.DeadlineAt != "2026-10-07T12:10:00Z" || res.RecoveryPolicyRevision != designRecoveryRevision ||
		res.EffectiveLimits != (protocol.Limits{MaxModelSteps: 5, MaxToolCallsPerStep: 4, DeadlineSeconds: 600, MaxCaptureBytes: 1048576, MaxGenerationAttempts: 8}) {
		t.Fatalf("%+v", res)
	}
	// What the Task had consumed is returned next to the new Run's own zeros.
	if out.Prior != (intake.PriorUsage{ModelStepsUsed: 1, GenerationAttemptsUsed: 1}) {
		t.Fatalf("%+v", out.Prior)
	}
	// The new Run, frozen with the request's limits and the host's recovery policy.
	limitsJSON, _ := protocol.Encode(res.EffectiveLimits)
	got := queryString(t, e.s.db, `SELECT phase||'|'||status||'|'||writer_epoch||'|'||resume_source_run_id||'|'||trace_id||'|'||limits_json||'|'||deadline_at||'|'||recovery_policy_revision||'|'||generation_attempts_used||'|'||generation_attempts_unknown
		FROM runs WHERE run_id=?`, res.RunID)
	want := strings.Join([]string{"Admitting", "running", "2", r.start.RunID, res.TraceID, string(limitsJSON), res.DeadlineAt, designRecoveryRevision, "0", "0"}, "|")
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	// The old Run is exactly as it was.
	if scalar[string](t, e.s.db, "SELECT result_json FROM runs WHERE run_id=?", r.start.RunID) != oldResult ||
		scalar[string](t, e.s.db, "SELECT phase||'|'||status||'|'||ended_at||'|'||trace_id||'|'||generation_attempts_used FROM runs WHERE run_id=?", r.start.RunID) != oldRow {
		t.Fatal("a resume changed the Run it resumes")
	}
	if got := scalar[string](t, e.s.db, "SELECT t.status||'|'||t.last_run_id||'|'||u.status||'|'||th.active_run_id||'|'||th.context_revision||'|'||th.control_revision FROM tasks t JOIN turns u ON u.turn_id=t.origin_turn_id JOIN threads th ON th.thread_id=t.thread_id"); got != "running|"+res.RunID+"|active|"+res.RunID+"|1|0" {
		t.Fatalf("%s", got)
	}
	// One event: run.started of the new Run, pointing back, under the receipt of the resume.
	if len(out.Events) != 1 || out.Events[0].Type != "run.started" {
		t.Fatalf("%+v", out.Events)
	}
	p := payloadOf[protocol.RunStartedPayload](t, out.Events[0])
	ev := out.Events[0]
	if p.RunID != res.RunID || p.PreviousRunID == nil || *p.PreviousRunID != r.start.RunID || p.TraceID != res.TraceID || p.WriterEpoch != 2 || p.EffectiveLimits != res.EffectiveLimits ||
		p.DeadlineAt != res.DeadlineAt || ev.TaskID == nil || *ev.TaskID != r.start.TaskID || ev.RunID == nil || *ev.RunID != res.RunID || ev.ReceiptID == nil || *ev.ReceiptID != res.ReceiptID ||
		ev.EvidenceID != nil {
		t.Fatalf("%+v %+v", p, ev)
	}
	// The receipt of the resume has the result, and is not the Task's first receipt.
	if got := scalar[string](t, e.s.db, "SELECT operation||'|'||stage FROM receipts WHERE receipt_id=?", res.ReceiptID); got != "run/resume|accepted" {
		t.Fatal(got)
	}
	after := e.counts()
	if after["runs"] != before["runs"]+1 || after["events"] != before["events"]+1 || after["receipts"] != before["receipts"]+1 || after["tasks"] != before["tasks"] || after["turns"] != before["turns"] ||
		after["items"] != before["items"] {
		t.Fatalf("%v -> %v", before, after)
	}

	// The new Run's own writes trace to the receipt of the resume, not the Task's first.
	fence := Fence{ThreadID: e.thread, RunID: res.RunID, Epoch: 2}
	for _, step := range [][2]string{{"Admitting", "Loading"}, {"Loading", "Assembling"}, {"Assembling", "Measuring"}, {"Measuring", "Generating"}} {
		if err := e.s.SetPhase(bg, fence, step[0], step[1]); err != nil {
			t.Fatal(err)
		}
	}
	if evs, err := e.s.ApplyInput(bg, fence); err != nil || len(evs) != 0 {
		t.Fatalf("the Task's input was applied by its first Run: %v %v", evs, err)
	}
	rsv, err := e.s.ReserveGeneration(bg, fence, reserveInput())
	if err != nil || len(rsv.Events) != 4 || rsv.AttemptsUsed != 1 {
		t.Fatalf("%+v %v", rsv, err)
	}
	for _, rev := range rsv.Events {
		if rev.ReceiptID == nil || *rev.ReceiptID != res.ReceiptID {
			t.Fatalf("an event of a resumed Run names receipt %v, not the resume's", rev.ReceiptID)
		}
	}
	rec, err := e.s.LoadRun(bg, res.RunID)
	if err != nil || rec.StartReceiptID != r.start.ReceiptID || rec.StartMessageID != r.start.Intake.MessageID || rec.ModelSteps != 1 || rec.AttemptsUsed != 1 {
		t.Fatalf("%+v %v", rec, err)
	}

	// The same request again: the same answer, nothing written, nothing started.
	n := e.counts()
	again, err := e.s.AdmitResume(bg, e.resumeAdm(2, nil), params)
	if err != nil || !again.Replayed || !reflect.DeepEqual(again.Result, res) || len(again.Events) != 0 {
		t.Fatalf("%+v %v", again, err)
	}
	e.wantNoChange(n)
	// The same key with other limits is a conflict, and still no second Run.
	if _, err := e.s.AdmitResume(bg, e.resumeAdm(2, nil), resumeParams(r.start.TaskID, r.start.RunID, 0, "res.key.0000000000000001", func(m map[string]any) {
		m["limits"].(map[string]any)["deadline_seconds"] = 601
	})); protocol.CodeOf(err) != protocol.CodeIdempotencyConflict {
		t.Fatalf("%v", err)
	}
	// Another key while the new Run is the Thread's active Run: BUSY.
	if _, err := e.s.AdmitResume(bg, e.resumeAdm(2, nil), resumeParams(r.start.TaskID, res.RunID, 0, "res.key.0000000000000002", nil)); protocol.CodeOf(err) != protocol.CodeBusy {
		t.Fatalf("%v", err)
	}
	e.wantNoChange(n)
	if rep, err := e.s.VerifyClosure(bg); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}

// TestAdmitResumeRefuses is PROTOCOL section 6 (ResumeInput) and F35: what a resume is
// refused for, each time with nothing written.
func TestAdmitResumeRefuses(t *testing.T) {
	type tc struct {
		name  string
		setup func(e *env) (*run, ResumeAdmission, []byte) // returns the Run, the admission and the params
		want  string
	}
	ended := func(e *env) *run { return e.resumable("res.run.000000000000000" + "2") }
	cases := []tc{
		{"a completed Task is not run again", func(e *env) (*run, ResumeAdmission, []byte) {
			r := e.newRun("res.run.0000000000000003")
			e.endRun(r, "rejected", "MODEL_REFUSED", false)
			return r, e.resumeAdm(2, nil), resumeParams(r.start.TaskID, r.start.RunID, 0, "res.key.0000000000000011", nil)
		}, protocol.CodeInvalidRequest},
		{"the last Run is not the one expected", func(e *env) (*run, ResumeAdmission, []byte) {
			r := ended(e)
			return r, e.resumeAdm(2, nil), resumeParams(r.start.TaskID, "run_00000000-0000-7000-8000-000000000001", 0, "res.key.0000000000000012", nil)
		}, protocol.CodeRevisionConflict},
		{"the Thread has an active Run", func(e *env) (*run, ResumeAdmission, []byte) {
			r := ended(e)
			adm := e.adm
			adm.WriterEpoch = 2
			if _, err := e.s.AdmitStart(bg, adm, e.params(e.thread, "res.start.00000000000001", func(m map[string]any) { m["expected_context_revision"] = 1 })); err != nil {
				t.Fatal(err)
			}
			return r, e.resumeAdm(2, nil), resumeParams(r.start.TaskID, r.start.RunID, 0, "res.key.0000000000000013", nil)
		}, protocol.CodeBusy},
		{"a stale control revision", func(e *env) (*run, ResumeAdmission, []byte) {
			r := ended(e)
			return r, e.resumeAdm(2, nil), resumeParams(r.start.TaskID, r.start.RunID, 5, "res.key.0000000000000014", nil)
		}, protocol.CodeRevisionConflict},
		{"a checkpoint the thread does not stand on", func(e *env) (*run, ResumeAdmission, []byte) {
			r := ended(e)
			return r, e.resumeAdm(2, nil), resumeParams(r.start.TaskID, r.start.RunID, 0, "res.key.0000000000000015", func(m map[string]any) {
				m["checkpoint_id"] = "ckp_00000000-0000-7000-8000-000000000001"
			})
		}, protocol.CodeRevisionConflict},
		{"a binding the host did not allow", func(e *env) (*run, ResumeAdmission, []byte) {
			r := ended(e)
			return r, e.resumeAdm(2, nil), resumeParams(r.start.TaskID, r.start.RunID, 0, "res.key.0000000000000016", func(m map[string]any) {
				m["binding"] = map[string]any{"kind": "model_route", "selector": "somewhere-else", "profile_revision": "r1", "agent_id": nil, "execution_role": nil}
			})
		}, protocol.CodeForbidden},
		{"a limit of zero", func(e *env) (*run, ResumeAdmission, []byte) {
			r := ended(e)
			return r, e.resumeAdm(2, nil), resumeParams(r.start.TaskID, r.start.RunID, 0, "res.key.0000000000000017", func(m map[string]any) {
				m["limits"].(map[string]any)["max_model_steps"] = 0
			})
		}, protocol.CodeInvalidParams},
		{"a limit above the host's", func(e *env) (*run, ResumeAdmission, []byte) {
			r := ended(e)
			return r, e.resumeAdm(2, nil), resumeParams(r.start.TaskID, r.start.RunID, 0, "res.key.0000000000000018", func(m map[string]any) {
				m["limits"].(map[string]any)["deadline_seconds"] = 1801
			})
		}, protocol.CodeInvalidLimits},
		{"a driver that does not hold the Thread", func(e *env) (*run, ResumeAdmission, []byte) {
			r := ended(e)
			return r, e.resumeAdm(1, nil), resumeParams(r.start.TaskID, r.start.RunID, 0, "res.key.0000000000000019", nil)
		}, protocol.CodeRevisionConflict},
		{"no such Task", func(e *env) (*run, ResumeAdmission, []byte) {
			r := ended(e)
			return r, e.resumeAdm(2, nil), resumeParams("tsk_00000000-0000-7000-8000-000000000001", r.start.RunID, 0, "res.key.0000000000000020", nil)
		}, protocol.CodeForbidden},
		{"a caller that cannot control the Thread", func(e *env) (*run, ResumeAdmission, []byte) {
			r := ended(e)
			adm := e.resumeAdm(2, nil)
			adm.Caller = e.newCaller(func(c *intake.CallerConfig) {
				c.Principal, c.Relays, c.ReadableSessionOwners = "user:reader", nil, []string{"core:local"}
			})
			return r, adm, resumeParams(r.start.TaskID, r.start.RunID, 0, "res.key.0000000000000021", nil)
		}, protocol.CodeForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			_, adm, params := c.setup(e)
			n := e.counts()
			if _, err := e.s.AdmitResume(bg, adm, params); protocol.CodeOf(err) != c.want {
				t.Fatalf("%v, want %s", err, c.want)
			}
			e.wantNoChange(n)
		})
	}
}

// TestAResumeLooksAtWhatTheTaskLeftUnknownAndPromotesNothing: a Tool call whose effect is
// unknown, and a generation whose end is unknown, stay unknown. The resume keeps what it
// found as one Evidence of the new Run (the host's verdict on each process, the
// generations it cannot ask about), names it in the new Run's run.started and makes the
// old Run's own end and the unknown call's event the dependencies of that event. No
// second action.completed is written for a call: its end is its end.
func TestAResumeLooksAtWhatTheTaskLeftUnknownAndPromotesNothing(t *testing.T) {
	e := newEnv(t)
	r := e.toolRun("res.run.0000000000000010")
	out := mustBind(t, r, batch(responseID(), call("c1", 0, "process.exec", `{}`)))
	r.phase("PreparingAction", "Executing")
	started := out.Calls[0]
	r.startTool(started)
	if err := e.s.RecordProcessStart(bg, r.fence, started.AttemptID, "boot-1", `{"pid":4242}`); err != nil {
		t.Fatal(err)
	}
	epoch, err := e.s.BumpWriterEpoch(bg, e.thread)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.TerminalizeStale(bg, e.thread, epoch, map[string]string{started.AttemptID: "gone"}, func(st StaleRun) (TerminalInput, error) {
		res := protocol.RunResult{RunID: st.RunID, TaskID: st.TaskID, Status: "blocked", Code: "EFFECT_OUTCOME_UNKNOWN", Verification: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}},
			EvidenceIDs: st.EvidenceIDs, Resumable: true, UnresolvedActionIDs: st.UnresolvedTools}
		return TerminalInput{Result: res, ResultEvidenceID: identity.NewEvidenceID().String()}, nil
	}); err != nil {
		t.Fatal(err)
	}
	completedEvents := scalar[int](t, e.s.db, "SELECT COUNT(*) FROM events WHERE type='action.completed' AND json_extract(payload_json,'$.attempt_id')=?", started.AttemptID)
	if completedEvents != 1 {
		t.Fatalf("setup: %d", completedEvents)
	}

	// The plan names the unknown call, so the host can be asked about its process before
	// the transaction.
	adm := e.resumeAdm(epoch, map[string]string{started.AttemptID: "gone"})
	params := resumeParams(r.start.TaskID, r.start.RunID, 0, "res.key.0000000000000031", nil)
	plan, err := e.s.PreflightResume(bg, adm, params)
	if err != nil || plan.ThreadID != e.thread || plan.LastRunID != r.start.RunID || len(plan.Unknown) != 1 || plan.Unknown[0].AttemptID != started.AttemptID ||
		plan.Unknown[0].Tool != "process.exec" || plan.Unknown[0].State != "unknown" || plan.Unknown[0].HostIncarnation != "boot-1" || plan.Unknown[0].ProcessToken != `{"pid":4242}` {
		t.Fatalf("%+v %v", plan, err)
	}

	out2, err := e.s.AdmitResume(bg, adm, params)
	if err != nil {
		t.Fatal(err)
	}
	ev := out2.Events[0]
	if len(out2.Events) != 1 || ev.Type != "run.started" || ev.EvidenceID == nil {
		t.Fatalf("%+v", out2.Events)
	}
	var rec struct {
		Kind          string `json:"kind"`
		PreviousRunID string `json:"previous_run_id"`
		Tools         []struct {
			RunID     string `json:"run_id"`
			ActionID  string `json:"action_id"`
			AttemptID string `json:"attempt_id"`
			Tool      string `json:"tool"`
			Effect    string `json:"effect"`
			Verdict   string `json:"verdict"`
		} `json:"tool_attempts"`
		Models []json.RawMessage `json:"model_attempts"`
	}
	if err := json.Unmarshal([]byte(readEvidence(t, e, *ev.EvidenceID)), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Kind != "resume_reconciliation" || rec.PreviousRunID != r.start.RunID || len(rec.Models) != 0 || len(rec.Tools) != 1 ||
		rec.Tools[0].ActionID != started.ActionID || rec.Tools[0].AttemptID != started.AttemptID || rec.Tools[0].Tool != "process.exec" || rec.Tools[0].Effect != "unknown" ||
		rec.Tools[0].Verdict != "gone" || rec.Tools[0].RunID != r.start.RunID {
		t.Fatalf("%+v", rec)
	}
	// The Evidence is the new Run's.
	if scalar[string](t, e.s.db, "SELECT run_id||'|'||state FROM evidence WHERE evidence_id=?", *ev.EvidenceID) != out2.Result.RunID+"|sealed" {
		t.Fatal("the reconciliation is not sealed Evidence of the new Run")
	}
	// The unknown call is exactly as it was: still unknown, one end, never a second one.
	if got := scalar[string](t, e.s.db, "SELECT state FROM attempts WHERE attempt_id=?", started.AttemptID); got != "unknown" {
		t.Fatalf("%s", got)
	}
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM events WHERE type='action.completed' AND json_extract(payload_json,'$.attempt_id')=?", started.AttemptID) != 1 {
		t.Fatal("a resume wrote a second end for a call")
	}
	// The dependencies of run.started: the old Run's end first (also its cause), then the
	// unknown call's event.
	terminal := scalar[string](t, e.s.db, "SELECT event_id FROM events WHERE type='run.terminal' AND run_id=?", r.start.RunID)
	completed := scalar[string](t, e.s.db, "SELECT event_id FROM events WHERE type='action.completed' AND json_extract(payload_json,'$.attempt_id')=?", started.AttemptID)
	var deps []string
	if err := json.Unmarshal([]byte(scalar[string](t, e.s.db, "SELECT dependency_event_ids_json FROM events WHERE event_id=?", ev.EventID)), &deps); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deps, []string{terminal, completed}) || scalar[string](t, e.s.db, "SELECT causation_event_id FROM events WHERE event_id=?", ev.EventID) != terminal {
		t.Fatalf("%v", deps)
	}
	if rep, err := e.s.VerifyClosure(bg); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}

	// The new Run ends too; a later resume looks at the Task's unknown call again (it is
	// still unknown) and keeps what it finds, once more, in its own Evidence.
	e.endRun(nil, "incomplete", "DRIVER_STOPPED", true)
	again, err := e.s.AdmitResume(bg, e.resumeAdm(epoch+1, map[string]string{started.AttemptID: "unverifiable"}), resumeParams(r.start.TaskID, out2.Result.RunID, 0, "res.key.0000000000000032", nil))
	if err != nil || again.Events[0].EvidenceID == nil || *again.Events[0].EvidenceID == *ev.EvidenceID {
		t.Fatalf("%+v %v", again, err)
	}
	if !strings.Contains(readEvidence(t, e, *again.Events[0].EvidenceID), `"verdict":"unverifiable"`) {
		t.Fatal("the second resume did not keep its own verdict")
	}
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM events WHERE type='action.completed' AND json_extract(payload_json,'$.attempt_id')=?", started.AttemptID) != 1 {
		t.Fatal("a second resume wrote a second end for a call")
	}
}

// TestAResumeOfATaskWithAnUnknownGenerationIsBlockedAndGeneratesNothing is the fail-closed
// half of resume (ERROR_MAPPING: while a generation is not known to have ended, the next act
// generation is not started; RETRY_CONTRACT: a new Run keeps the Task's unknown): the new Run
// is created, with its Trace, its limits and the resume's receipt, and ended in the same
// transaction as blocked with MODEL_GENERATION_OUTCOME_UNKNOWN, listing the unknown
// generations. It is never the Thread's active Run, so nothing drives it. The generations
// are named in the Evidence as not queried (the model side cannot be asked from here). Each
// resume of such a Task is the same, and none lifts the block.
func TestAResumeOfATaskWithAnUnknownGenerationIsBlockedAndGeneratesNothing(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("res.run.0000000000000011")
	r.goTo("Generating")
	rsv := r.reserve()
	epoch, err := e.s.BumpWriterEpoch(bg, e.thread)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.TerminalizeStale(bg, e.thread, epoch, nil, func(st StaleRun) (TerminalInput, error) {
		res := protocol.RunResult{RunID: st.RunID, TaskID: st.TaskID, Status: "blocked", Code: "MODEL_GENERATION_OUTCOME_UNKNOWN", Verification: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}},
			EvidenceIDs: st.EvidenceIDs, Resumable: true, UnresolvedActionIDs: []string{st.Unresolved[0].ActionID}}
		return TerminalInput{Result: res, ResultEvidenceID: identity.NewEvidenceID().String()}, nil
	}); err != nil {
		t.Fatal(err)
	}
	oldResult := scalar[string](t, e.s.db, "SELECT result_json FROM runs WHERE run_id=?", r.start.RunID)
	params := resumeParams(r.start.TaskID, r.start.RunID, 0, "res.key.0000000000000041", nil)

	// Without a decision to end such a Run, nothing is written: the resume cannot proceed.
	n := e.counts()
	bare := e.resumeAdm(epoch, nil)
	bare.EndsOnUnknownGeneration = nil
	if _, err := e.s.AdmitResume(bg, bare, params); protocol.CodeOf(err) != protocol.CodeInternal {
		t.Fatalf("%v", err)
	}
	e.wantNoChange(n)

	out, err := e.s.AdmitResume(bg, e.resumeAdm(epoch, nil), params)
	if err != nil || !out.Blocked || out.Prior.GenerationAttemptsUsed != 1 || out.Prior.GenerationAttemptsUnknown != 1 || out.Prior.ModelStepsUsed != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	if got := typesOf(out.Events); got != "run.started,run.terminal" {
		t.Fatalf("%s", got)
	}
	started, ended := out.Events[0], out.Events[1]
	term := payloadOf[protocol.RunTerminalPayload](t, ended)
	if term.Status != "blocked" || term.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" || ended.RunID == nil || *ended.RunID != out.Result.RunID || ended.ReceiptID == nil || *ended.ReceiptID != out.Result.ReceiptID ||
		started.EvidenceID == nil {
		t.Fatalf("%+v %+v", term, ended)
	}
	// The new Run is a Run of the same Task, ended: its result lists the unknown generation,
	// and the Thread is free (it was never the active Run).
	info, err := e.s.GetRun(bg, e.caller, out.Result.RunID)
	if err != nil || !info.Terminal || info.TaskID != r.start.TaskID || info.Result.Status != "blocked" || info.Result.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" || !info.Result.Resumable ||
		len(info.Result.UnresolvedActionIDs) != 1 || info.Result.UnresolvedActionIDs[0] != rsv.ActionID || info.Result.FinalText != "" || info.GenerationAttemptsUsed != 0 {
		t.Fatalf("%+v %v", info, err)
	}
	if got := scalar[string](t, e.s.db, "SELECT COALESCE(active_run_id,'none')||'|'||context_revision FROM threads"); got != "none|0" {
		t.Fatalf("%s", got)
	}
	if got := scalar[string](t, e.s.db, "SELECT t.status||'|'||t.last_run_id||'|'||u.status||'|'||r.status||'|'||r.phase||'|'||r.resume_source_run_id FROM tasks t JOIN turns u ON u.turn_id=t.origin_turn_id JOIN runs r ON r.run_id=t.last_run_id"); got != "run_ended|"+out.Result.RunID+"|ended|blocked|Terminal|"+r.start.RunID {
		t.Fatalf("%s", got)
	}
	// No generation was reserved for it: the Run has no model Action, and the old Run is as it was.
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM actions WHERE run_id=?", out.Result.RunID) != 0 ||
		scalar[string](t, e.s.db, "SELECT result_json FROM runs WHERE run_id=?", r.start.RunID) != oldResult ||
		scalar[string](t, e.s.db, "SELECT generation_attempts_used||'/'||generation_attempts_unknown FROM runs WHERE run_id=?", r.start.RunID) != "1/1" {
		t.Fatal("the blocked Run reserved a generation, or the old Run changed")
	}
	// The Evidence names the generation as unknown and not queried.
	var rec struct {
		Tools  []json.RawMessage `json:"tool_attempts"`
		Models []struct {
			ActionID  string `json:"action_id"`
			AttemptID string `json:"attempt_id"`
			State     string `json:"generation_state"`
			Queried   bool   `json:"queried"`
		} `json:"model_attempts"`
	}
	if err := json.Unmarshal([]byte(readEvidence(t, e, *started.EvidenceID)), &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Tools) != 0 || len(rec.Models) != 1 || rec.Models[0].AttemptID != rsv.AttemptID || rec.Models[0].ActionID != rsv.ActionID || rec.Models[0].State != "unknown" || rec.Models[0].Queried {
		t.Fatalf("%+v", rec)
	}
	// The same request again is the same answer; another resume is another blocked Run.
	n = e.counts()
	again, err := e.s.AdmitResume(bg, e.resumeAdm(epoch, nil), params)
	if err != nil || !again.Replayed || again.Blocked || !reflect.DeepEqual(again.Result, out.Result) || len(again.Events) != 0 {
		t.Fatalf("%+v %v", again, err)
	}
	e.wantNoChange(n)
	second, err := e.s.AdmitResume(bg, e.resumeAdm(epoch, nil), resumeParams(r.start.TaskID, out.Result.RunID, 0, "res.key.0000000000000042", nil))
	if err != nil || !second.Blocked || second.Result.RunID == out.Result.RunID || second.Result.PreviousRunID != out.Result.RunID {
		t.Fatalf("%+v %v", second, err)
	}
	if rep, err := e.s.VerifyClosure(bg); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}

func readEvidence(t *testing.T, e *env, id string) string {
	t.Helper()
	text, err := e.s.readEvidenceText(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	return text
}
