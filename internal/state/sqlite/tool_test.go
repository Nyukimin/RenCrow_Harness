package sqlite

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolview"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// toolRun is a Run in the phase a Tool exchange starts from.
func (e *env) toolRun(key string) *run {
	e.t.Helper()
	r := e.newRun(key)
	r.goTo("Generating")
	rsv := r.reserve()
	// The generation that asked for the calls ended: it is not what is unresolved.
	if _, err := e.s.RecordAttemptEnd(bg, r.fence, AttemptEnd{AttemptID: rsv.AttemptID, State: "completed", Outcome: "completed", GenerationState: "terminal", BackendAttempts: one(1)}); err != nil {
		e.t.Fatal(err)
	}
	r.phase("Generating", "PreparingAction")
	return r
}

func responseID() string { return identity.NewResponseID().String() }

func call(id string, ordinal int64, name, args string) ToolCall {
	return ToolCall{
		ActionID: identity.NewActionID().String(), AttemptID: identity.NewAttemptID().String(), ProviderToolCallID: id, Ordinal: ordinal, Name: name, ArgsBytes: []byte(args),
	}
}

func refuse(c ToolCall, code string) ToolCall {
	v, _ := toolview.Encode(toolview.View{Tool: c.Name, ActionID: c.ActionID, EffectState: "not_started", CaptureComplete: true, Error: &toolview.Error{Code: code, Message: "refused"}})
	c.Rejection = &ToolRejection{Code: code, ResultText: string(v)}
	return c
}

func batch(rsp string, calls ...ToolCall) BindToolBatchInput {
	return BindToolBatchInput{
		ResponseID: rsp, AssistantMessageID: identity.NewMessageID().String(), Calls: calls,
		AssistantRecord: []byte(`{"content":null,"tool_calls":[{"function":{"arguments":"{}","name":"file.read"},"id":"c1","type":"function"}]}`),
	}
}

func mustBind(t *testing.T, r *run, in BindToolBatchInput) BoundBatch {
	t.Helper()
	out, err := r.e.s.BindToolBatch(bg, r.fence, in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (r *run) startTool(c BoundCall) ToolStarted {
	r.e.t.Helper()
	out, err := r.e.s.StartToolAttempt(bg, r.fence, StartToolInput{ActionID: c.ActionID, AttemptID: c.AttemptID, PolicyRevision: r.e.policyRevision()})
	if err != nil {
		r.e.t.Fatal(err)
	}
	return out
}

func (e *env) policyRevision() string {
	return scalar[string](e.t, e.s.db, "SELECT policy_revision FROM threads WHERE thread_id=?", e.thread)
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func finish(c BoundCall, effect string, view string) CompleteToolInput {
	return CompleteToolInput{ActionID: c.ActionID, AttemptID: c.AttemptID, EffectState: effect, CaptureComplete: true, ResultText: view, ResultJSON: `{"effect_state":"` + effect + `"}`,
		ResponseID: "x", ProviderCallID: "x"}
}

func typesOf(evs []protocol.Event) string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return strings.Join(out, ",")
}

func TestBindingAResponseIsOneTransactionAndIdempotentOnTheCallKey(t *testing.T) {
	e := newEnv(t)
	r := e.toolRun("tool.key.0000000000001")
	rsp := responseID()
	c1, c2 := call("call-a", 0, "file.read", `{"path":"a"}`), call("call-b", 1, "file.create", `{"path":"b"}`)
	out := mustBind(t, r, batch(rsp, c1, c2))
	if len(out.Calls) != 2 || out.Calls[0].ActionID != c1.ActionID || out.Calls[1].ActionID != c2.ActionID || out.Calls[0].State != "prepared" || out.Calls[0].Existing ||
		typesOf(out.Events) != "action.prepared,action.prepared" {
		t.Fatalf("%+v", out)
	}
	// One Action and one prepared Attempt per call, linked by the call's key, with the
	// arguments' hash; the assistant message is raw history, not yet in the context.
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM actions WHERE kind='tool' AND status='prepared'") != 2 ||
		scalar[int](t, e.s.db, "SELECT COUNT(*) FROM attempts WHERE state='prepared'") != 2 ||
		scalar[string](t, e.s.db, "SELECT args_hash FROM tool_links WHERE provider_tool_call_id='call-a'") != sha(`{"path":"a"}`) ||
		scalar[int](t, e.s.db, "SELECT COUNT(*) FROM items WHERE json_extract(metadata_json,'$.kind')='tool_calls'") != 1 ||
		scalar[int](t, e.s.db, "SELECT COUNT(*) FROM context_entries") != 0 {
		t.Fatal("the records of a bound response are wrong")
	}
	policy := scalar[string](t, e.s.db, "SELECT json_extract(payload_json,'$.policy_revision') FROM events WHERE type='action.prepared' LIMIT 1")
	if policy != e.policyRevision() {
		t.Fatalf("action.prepared names policy revision %q", policy)
	}

	// The same calls again: the same Actions, nothing new.
	before := scalar[int](t, e.s.db, "SELECT COUNT(*) FROM events")
	again, err := e.s.BindToolBatch(bg, r.fence, BindToolBatchInput{ResponseID: rsp, AssistantMessageID: identity.NewMessageID().String(), AssistantRecord: []byte(`{}`),
		Calls: []ToolCall{call("call-a", 0, "file.read", `{"path":"a"}`), call("call-b", 1, "file.create", `{"path":"b"}`)}})
	if err != nil || again.Calls[0].ActionID != c1.ActionID || again.Calls[1].ActionID != c2.ActionID || !again.Calls[0].Existing || len(again.Events) != 0 ||
		scalar[int](t, e.s.db, "SELECT COUNT(*) FROM events") != before || scalar[int](t, e.s.db, "SELECT COUNT(*) FROM actions WHERE kind='tool'") != 2 ||
		scalar[int](t, e.s.db, "SELECT COUNT(*) FROM items WHERE json_extract(metadata_json,'$.kind')='tool_calls'") != 1 {
		t.Fatalf("a redelivered response was not the same Actions: %+v %v", again, err)
	}
	// The same key with other arguments is a conflict, and writes nothing.
	_, err = e.s.BindToolBatch(bg, r.fence, BindToolBatchInput{ResponseID: rsp, AssistantMessageID: identity.NewMessageID().String(), AssistantRecord: []byte(`{}`),
		Calls: []ToolCall{call("call-a", 0, "file.read", `{"path":"OTHER"}`)}})
	if !errors.Is(err, ErrToolIntentConflict) {
		t.Fatalf("%v", err)
	}
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM events") != before {
		t.Fatal("a refused bind wrote events")
	}
	// The same call ID in another response is another call.
	other := batch(responseID(), call("call-a", 0, "file.read", `{"path":"a"}`))
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM tool_links") != 2 {
		t.Fatal("setup")
	}
	_ = other
}

func TestBindingIsRefusedOutsideItsPhaseLimitsAndAfterACancellation(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("tool.key.0000000000002")
	r.goTo("Generating")
	_ = r.reserve()
	if _, err := e.s.BindToolBatch(bg, r.fence, batch(responseID(), call("c", 0, "file.read", `{}`))); !errors.Is(err, ErrPhaseConflict) {
		t.Fatalf("a bind outside PreparingAction: %v", err)
	}
	r.phase("Generating", "PreparingAction")
	if _, err := e.s.BindToolBatch(bg, r.fence, batch(responseID())); !errors.Is(err, ErrToolCallLimit) {
		t.Fatalf("an empty response: %v", err)
	}
	var many []ToolCall
	for i := int64(0); i <= hostLimits.MaxToolCallsPerStep; i++ {
		many = append(many, call("c"+string(rune('a'+i)), i, "file.read", `{}`))
	}
	if _, err := e.s.BindToolBatch(bg, r.fence, batch(responseID(), many...)); !errors.Is(err, ErrToolCallLimit) {
		t.Fatalf("more calls than the Run's limit: %v", err)
	}
	// A stale writer binds nothing.
	stale := r.fence
	stale.Epoch = 99
	if _, err := e.s.BindToolBatch(bg, stale, batch(responseID(), call("c", 0, "file.read", `{}`))); !errors.Is(err, ErrWriterLost) {
		t.Fatalf("%v", err)
	}
	// A cancellation recorded first: the control revision moved.
	mustExec(t, e.s.db, "UPDATE threads SET control_revision=control_revision+1")
	if _, err := e.s.BindToolBatch(bg, r.fence, batch(responseID(), call("c", 0, "file.read", `{}`))); !errors.Is(err, ErrControlChanged) {
		t.Fatalf("%v", err)
	}
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM actions WHERE kind='tool'") != 0 {
		t.Fatal("a refused bind left an Action")
	}
}

func TestACallThePolicyRefusedIsRecordedAsRefusedAndNeverDispatchable(t *testing.T) {
	e := newEnv(t)
	r := e.toolRun("tool.key.0000000000003")
	rsp := responseID()
	bad, good := refuse(call("c1", 0, "file.read", `{"path":"x"}`), "PATH_OUTSIDE_SCOPE"), call("c2", 1, "file.read", `{"path":"y"}`)
	out := mustBind(t, r, batch(rsp, bad, good))
	if typesOf(out.Events) != "action.prepared,action.completed,action.prepared" || out.Calls[0].State != "failed" || out.Calls[1].State != "prepared" {
		t.Fatalf("%+v", out)
	}
	done := payloadOf[protocol.ActionCompletedPayload](t, out.Events[1])
	if done.EffectState != "not_started" || done.ExitCode != nil || !done.CaptureComplete || len(done.ResultEvidenceIDs) != 1 || out.Events[1].EvidenceID == nil {
		t.Fatalf("%+v", done)
	}
	if scalar[string](t, e.s.db, "SELECT status FROM actions WHERE action_id=?", bad.ActionID) != "rejected" ||
		scalar[string](t, e.s.db, "SELECT state FROM attempts WHERE attempt_id=?", bad.AttemptID) != "failed" ||
		scalar[int](t, e.s.db, "SELECT COUNT(*) FROM items WHERE json_extract(metadata_json,'$.kind')='tool_result'") != 1 {
		t.Fatal("the refused call is not recorded as refused")
	}
	// It cannot be started: it is not prepared.
	r.phase("PreparingAction", "Executing")
	_, err := e.s.StartToolAttempt(bg, r.fence, StartToolInput{ActionID: bad.ActionID, AttemptID: bad.AttemptID, PolicyRevision: e.policyRevision()})
	if !errors.Is(err, ErrAttemptState) {
		t.Fatalf("a refused call was started: %v", err)
	}
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM events WHERE type='action.dispatch_started' AND json_extract(payload_json,'$.action_id') IN (SELECT action_id FROM actions WHERE kind='tool')") != 0 {
		t.Fatal("a dispatch was recorded for a refused call")
	}
}

// TestCancellationAndDispatchStartAreLinearizedOnTheThreadRow is STORAGE section 6: the
// dispatch-start compares the Thread row's epoch and control revision in its own
// transaction, so a cancellation that came first stops the call and one that came after
// finds it started.
func TestCancellationAndDispatchStartAreLinearizedOnTheThreadRow(t *testing.T) {
	e := newEnv(t)
	r := e.toolRun("tool.key.0000000000004")
	out := mustBind(t, r, batch(responseID(), call("c1", 0, "file.create", `{}`), call("c2", 1, "file.create", `{}`)))
	r.phase("PreparingAction", "Executing")

	// The first call starts before the cancellation: it is started, and recorded so.
	started := r.startTool(out.Calls[0])
	ev := payloadOf[protocol.ActionDispatchStartedPayload](t, started.Events[0])
	if ev.ActionID != out.Calls[0].ActionID || ev.WriterEpoch != 1 || ev.ControlRevision != 0 || started.WriterEpoch != 1 {
		t.Fatalf("%+v", ev)
	}
	if scalar[string](t, e.s.db, "SELECT state FROM attempts WHERE attempt_id=?", out.Calls[0].AttemptID) != "dispatch_started" {
		t.Fatal("the Attempt is not recorded as started")
	}
	// A call is dispatched at most once.
	if _, err := e.s.StartToolAttempt(bg, r.fence, StartToolInput{ActionID: out.Calls[0].ActionID, AttemptID: out.Calls[0].AttemptID, PolicyRevision: e.policyRevision()}); !errors.Is(err, ErrAttemptState) {
		t.Fatalf("a started call was started again: %v", err)
	}

	// The cancellation lands. The second call, not yet started, can no longer start.
	mustExec(t, e.s.db, "UPDATE threads SET control_revision=control_revision+1")
	_, err := e.s.StartToolAttempt(bg, r.fence, StartToolInput{ActionID: out.Calls[1].ActionID, AttemptID: out.Calls[1].AttemptID, PolicyRevision: e.policyRevision()})
	if !errors.Is(err, ErrControlChanged) {
		t.Fatalf("%v", err)
	}
	if scalar[string](t, e.s.db, "SELECT state FROM attempts WHERE attempt_id=?", out.Calls[1].AttemptID) != "prepared" ||
		scalar[int](t, e.s.db, "SELECT COUNT(*) FROM events WHERE type='action.dispatch_started' AND json_extract(payload_json,'$.action_id') IN (SELECT action_id FROM actions WHERE kind='tool')") != 1 {
		t.Fatal("a cancelled dispatch wrote something")
	}
	// The first call's result is still recorded: a result is not refused for the cancellation.
	done, err := e.s.CompleteToolAttempt(bg, r.fence, finish(out.Calls[0], "completed", `{"tool":"file.create"}`))
	if err != nil || typesOf(done.Events) != "action.completed" {
		t.Fatalf("%v %v", done, err)
	}
}

func TestDispatchStartChecksTheEpochPolicyDeadlineAndPhase(t *testing.T) {
	e := newEnv(t)
	r := e.toolRun("tool.key.0000000000005")
	out := mustBind(t, r, batch(responseID(), call("c1", 0, "file.create", `{}`)))
	in := StartToolInput{ActionID: out.Calls[0].ActionID, AttemptID: out.Calls[0].AttemptID, PolicyRevision: e.policyRevision()}
	if _, err := e.s.StartToolAttempt(bg, r.fence, in); !errors.Is(err, ErrPhaseConflict) {
		t.Fatalf("a start outside Executing: %v", err)
	}
	r.phase("PreparingAction", "Executing")
	bad := in
	bad.PolicyRevision = "another-revision"
	if _, err := e.s.StartToolAttempt(bg, r.fence, bad); !errors.Is(err, ErrPolicyChanged) {
		t.Fatalf("%v", err)
	}
	stale := r.fence
	stale.Epoch = 7
	if _, err := e.s.StartToolAttempt(bg, stale, in); !errors.Is(err, ErrWriterLost) {
		t.Fatalf("%v", err)
	}
	foreign := in
	foreign.AttemptID = identity.NewAttemptID().String()
	if _, err := e.s.StartToolAttempt(bg, r.fence, foreign); protocol.CodeOf(err) != protocol.CodeIntegrityBlocked {
		t.Fatalf("an attempt of no Run: %v", err)
	}
	if scalar[string](t, e.s.db, "SELECT state FROM attempts WHERE attempt_id=?", in.AttemptID) != "prepared" {
		t.Fatal("a refused start changed the Attempt")
	}
}

func TestAProcessIdentityIsRecordedOnceAndCaptureChunksNeedTheFence(t *testing.T) {
	e := newEnv(t)
	r := e.toolRun("tool.key.0000000000006")
	out := mustBind(t, r, batch(responseID(), call("c1", 0, "process.exec", `{}`)))
	r.phase("PreparingAction", "Executing")
	c := out.Calls[0]
	if err := e.s.RecordProcessStart(bg, r.fence, c.AttemptID, "boot-1", `{"pid":42}`); !errors.Is(err, ErrAttemptState) {
		t.Fatalf("an identity for a call that was not dispatched: %v", err)
	}
	r.startTool(c)
	if err := e.s.RecordProcessStart(bg, r.fence, c.AttemptID, "boot-1", `{"pid":42}`); err != nil {
		t.Fatal(err)
	}
	if scalar[string](t, e.s.db, "SELECT state||'/'||host_incarnation||'/'||process_token FROM attempts WHERE attempt_id=?", c.AttemptID) != `running/boot-1/{"pid":42}` {
		t.Fatal("the process identity is not recorded")
	}
	if err := e.s.RecordProcessStart(bg, r.fence, c.AttemptID, "boot-1", "x"); !errors.Is(err, ErrAttemptState) {
		t.Fatalf("an identity recorded twice: %v", err)
	}
	// Output chunks are stored under the fence: a driver that lost the Thread stores none.
	id := identity.NewEvidenceID().String()
	stale := r.fence
	stale.Epoch = 5
	if err := e.s.AppendCaptureChunk(bg, stale, CaptureChunk{EvidenceID: id, AttemptID: c.AttemptID, Purpose: PurposeToolStdout, Ordinal: 0, Data: []byte("x")}); !errors.Is(err, ErrWriterLost) {
		t.Fatalf("%v", err)
	}
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM evidence WHERE evidence_id=?", id) != 0 {
		t.Fatal("a stale driver stored output")
	}
	// Chunks of anything but a capture are refused, and so is extending a capture that is sealed.
	if err := e.s.AppendCaptureChunk(bg, r.fence, CaptureChunk{EvidenceID: id, AttemptID: c.AttemptID, Purpose: "model_request", Ordinal: 0, Data: []byte("x")}); err == nil {
		t.Fatal("a chunk of another purpose was stored")
	}
	if err := e.s.AppendCaptureChunk(bg, r.fence, CaptureChunk{EvidenceID: id, AttemptID: c.AttemptID, Purpose: PurposeToolStdout, Ordinal: 0, Data: []byte("abc")}); err != nil {
		t.Fatal(err)
	}
	in := finish(c, "completed", `{}`)
	in.Captures = []SealCapture{{EvidenceID: id, Created: true, Purpose: PurposeToolStdout}}
	if _, err := e.s.CompleteToolAttempt(bg, r.fence, in); err != nil {
		t.Fatal(err)
	}
	if err := e.s.AppendCaptureChunk(bg, r.fence, CaptureChunk{EvidenceID: id, AttemptID: c.AttemptID, Purpose: PurposeToolStdout, Ordinal: 1, ByteStart: 3, Data: []byte("d")}); err == nil {
		t.Fatal("a sealed capture was extended")
	}
}

func TestSealingACaptureChecksItsChunksAndDecidesTheProjection(t *testing.T) {
	e := newEnv(t)
	r := e.toolRun("tool.key.0000000000007")
	out := mustBind(t, r, batch(responseID(), call("c1", 0, "process.exec", `{}`)))
	r.phase("PreparingAction", "Executing")
	c := out.Calls[0]
	r.startTool(c)
	outID, errID, emptyID := identity.NewEvidenceID().String(), identity.NewEvidenceID().String(), identity.NewEvidenceID().String()
	text := "héllo wörld\n"
	// "é" is split across two chunks: the whole is still text.
	b := []byte(text)
	cut := strings.Index(text, "é") + 1
	for i, part := range [][]byte{b[:cut], b[cut:]} {
		start := int64(0)
		if i == 1 {
			start = int64(cut)
		}
		if err := e.s.AppendCaptureChunk(bg, r.fence, CaptureChunk{EvidenceID: outID, AttemptID: c.AttemptID, Purpose: PurposeToolStdout, Ordinal: int64(i), ByteStart: start, Data: part}); err != nil {
			t.Fatal(err)
		}
	}
	binary := []byte{0xff, 0xfe, 'a'}
	if err := e.s.AppendCaptureChunk(bg, r.fence, CaptureChunk{EvidenceID: errID, AttemptID: c.AttemptID, Purpose: PurposeToolStderr, Ordinal: 0, ByteStart: 0, Data: binary}); err != nil {
		t.Fatal(err)
	}
	in := finish(c, "completed", `{"tool":"process.exec"}`)
	zero := int64(0)
	in.ExitCode = &zero
	in.Captures = []SealCapture{{EvidenceID: outID, Created: true, Purpose: PurposeToolStdout}, {EvidenceID: errID, Created: true, Purpose: PurposeToolStderr}, {EvidenceID: emptyID, Created: false, Purpose: PurposeToolStdout}}
	done, err := e.s.CompleteToolAttempt(bg, r.fence, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(done.ResultEvidenceIDs) != 4 || done.ResultEvidenceIDs[1] != outID {
		t.Fatalf("%v", done.ResultEvidenceIDs)
	}
	row := func(id string) string {
		return scalar[string](t, e.s.db, "SELECT state||'|'||media_type||'|'||COALESCE(projection_version,'-')||'|'||total_bytes||'|'||raw_hash||'|'||capture_complete FROM evidence WHERE evidence_id=?", id)
	}
	if got, want := row(outID), "sealed|text/plain; charset=utf-8|text/v1|"+strconv.Itoa(len(text))+"|"+sha(text)+"|1"; got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	if got, want := row(errID), "sealed|application/octet-stream|-|3|"+sha(string(binary))+"|1"; got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	if got, want := row(emptyID), "sealed|text/plain; charset=utf-8|text/v1|0|"+sha("")+"|1"; got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	// The Attempt, the Action, the answer item and the event all committed together.
	if scalar[string](t, e.s.db, "SELECT a.state||'/'||ac.status FROM attempts a JOIN actions ac ON ac.action_id=a.action_id WHERE a.attempt_id=?", c.AttemptID) != "completed/completed" ||
		scalar[int](t, e.s.db, "SELECT COUNT(*) FROM items WHERE json_extract(metadata_json,'$.action_id')=?", c.ActionID) != 1 {
		t.Fatal("the end of the call is not recorded")
	}
	ev := payloadOf[protocol.ActionCompletedPayload](t, done.Events[0])
	if ev.EffectState != "completed" || ev.ExitCode == nil || *ev.ExitCode != 0 || !ev.CaptureComplete || len(ev.ResultEvidenceIDs) != 4 {
		t.Fatalf("%+v", ev)
	}
	// An Attempt ends once.
	if _, err := e.s.CompleteToolAttempt(bg, r.fence, in); !errors.Is(err, ErrAttemptEnded) {
		t.Fatalf("%v", err)
	}
	// Every sealed Evidence agrees with its chunks: the restore-closure check passes.
	rep, err := e.s.VerifyClosure(bg)
	if err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestAnIncompleteCaptureIsSealedAsIncompleteAndNeverAsText(t *testing.T) {
	e := newEnv(t)
	r := e.toolRun("tool.key.0000000000008")
	out := mustBind(t, r, batch(responseID(), call("c1", 0, "process.exec", `{}`)))
	r.phase("PreparingAction", "Executing")
	c := out.Calls[0]
	r.startTool(c)
	id := identity.NewEvidenceID().String()
	if err := e.s.AppendCaptureChunk(bg, r.fence, CaptureChunk{EvidenceID: id, AttemptID: c.AttemptID, Purpose: PurposeToolStdout, Ordinal: 0, ByteStart: 0, Data: []byte("plain text")}); err != nil {
		t.Fatal(err)
	}
	in := finish(c, "failed", `{}`)
	in.CaptureComplete = false
	in.Captures = []SealCapture{{EvidenceID: id, Created: true, Purpose: PurposeToolStdout}}
	done, err := e.s.CompleteToolAttempt(bg, r.fence, in)
	if err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, e.s.db, "SELECT capture_complete||'|'||media_type||'|'||COALESCE(projection_version,'-') FROM evidence WHERE evidence_id=?", id); got != "0|application/octet-stream|-" {
		t.Fatalf("an incomplete capture must not be a complete text: %s", got)
	}
	if ev := payloadOf[protocol.ActionCompletedPayload](t, done.Events[0]); ev.CaptureComplete || ev.EffectState != "failed" {
		t.Fatalf("%+v", ev)
	}
}

func TestApplyingAnExchangeClosesWhatWasNotReachedAndAppliesItAllInOrder(t *testing.T) {
	e := newEnv(t)
	r := e.toolRun("tool.key.0000000000010")
	rsp := responseID()
	out := mustBind(t, r, batch(rsp, call("c1", 0, "file.create", `{}`), call("c2", 1, "file.create", `{}`), call("c3", 2, "file.create", `{}`)))
	r.phase("PreparingAction", "Executing")
	r.startTool(out.Calls[0])
	if _, err := e.s.CompleteToolAttempt(bg, r.fence, func() CompleteToolInput {
		in := finish(out.Calls[0], "failed", `{"tool":"file.create","error":"x"}`)
		in.ResponseID, in.ProviderCallID = rsp, "c1"
		return in
	}()); err != nil {
		t.Fatal(err)
	}
	r.phase("Executing", "PersistingObservation")
	rev0 := scalar[int](t, e.s.db, "SELECT context_revision FROM threads")
	res, err := e.s.PersistToolObservation(bg, r.fence, rsp, "earlier_call_failed")
	if err != nil {
		t.Fatal(err)
	}
	if typesOf(res.Events) != "action.completed,action.completed" || len(res.Unknown) != 0 {
		t.Fatalf("%v %v", typesOf(res.Events), res.Unknown)
	}
	for _, ev := range res.Events {
		if p := payloadOf[protocol.ActionCompletedPayload](t, ev); p.EffectState != "not_started" || p.ExitCode != nil || !p.CaptureComplete {
			t.Fatalf("%+v", p)
		}
	}
	// The unreached calls were never dispatched: cancelled attempts, no dispatch event.
	for _, c := range out.Calls[1:] {
		if scalar[string](t, e.s.db, "SELECT state FROM attempts WHERE attempt_id=?", c.AttemptID) != "cancelled" ||
			scalar[string](t, e.s.db, "SELECT status FROM actions WHERE action_id=?", c.ActionID) != "cancelled" {
			t.Fatal("an unreached call is not closed as not run")
		}
	}
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM events WHERE type='action.dispatch_started' AND json_extract(payload_json,'$.action_id') IN (SELECT action_id FROM actions WHERE kind='tool')") != 1 {
		t.Fatal("a call that was never reached was dispatched")
	}
	// The exchange is in the context, the assistant message first and the answers in the
	// order of the calls, each moving the revision by one.
	if got := scalar[int](t, e.s.db, "SELECT context_revision FROM threads") - rev0; got != 4 {
		t.Fatalf("the context moved by %d", got)
	}
	snap, err := e.s.LoadSnapshot(bg, r.start.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, a := range snap.Applied {
		kinds = append(kinds, a.ItemKind+":"+a.ToolCallID)
	}
	if got := strings.Join(kinds, ","); got != "tool_calls:,tool_result:c1,tool_result:c2,tool_result:c3" {
		t.Fatalf("%s", got)
	}
	if !strings.Contains(snap.Applied[2].Text, "NOT_EXECUTED") || !strings.Contains(snap.Applied[2].Text, "an earlier call of the same response did not succeed") {
		t.Fatalf("%s", snap.Applied[2].Text)
	}
	// Applying again changes nothing.
	again, err := e.s.PersistToolObservation(bg, r.fence, rsp, "run_ended")
	if err != nil || len(again.Events) != 0 || scalar[int](t, e.s.db, "SELECT context_revision FROM threads")-rev0 != 4 {
		t.Fatalf("%v %v", again, err)
	}
	if rep, err := e.s.VerifyClosure(bg); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestACallLeftDispatchedWhenTheExchangeIsAppliedIsUnknownNeverNotRun(t *testing.T) {
	e := newEnv(t)
	r := e.toolRun("tool.key.0000000000011")
	rsp := responseID()
	out := mustBind(t, r, batch(rsp, call("c1", 0, "process.exec", `{}`)))
	r.phase("PreparingAction", "Executing")
	r.startTool(out.Calls[0])
	id := identity.NewEvidenceID().String()
	if err := e.s.AppendCaptureChunk(bg, r.fence, CaptureChunk{EvidenceID: id, AttemptID: out.Calls[0].AttemptID, Purpose: PurposeToolStdout, Ordinal: 0, ByteStart: 0, Data: []byte("partial")}); err != nil {
		t.Fatal(err)
	}
	res, err := e.s.PersistToolObservation(bg, r.fence, rsp, "run_ended")
	if err != nil || len(res.Unknown) != 1 || res.Unknown[0] != out.Calls[0].ActionID {
		t.Fatalf("%+v %v", res, err)
	}
	p := payloadOf[protocol.ActionCompletedPayload](t, res.Events[0])
	if p.EffectState != "unknown" || p.CaptureComplete || len(p.ResultEvidenceIDs) != 2 {
		t.Fatalf("%+v", p)
	}
	if scalar[string](t, e.s.db, "SELECT state FROM attempts WHERE attempt_id=?", out.Calls[0].AttemptID) != "unknown" ||
		scalar[string](t, e.s.db, "SELECT state||'|'||capture_complete FROM evidence WHERE evidence_id=?", id) != "sealed|0" {
		t.Fatal("an unresolved call is not unknown, or its capture was not sealed as incomplete")
	}
	snap, _ := e.s.LoadSnapshot(bg, r.start.RunID)
	last := snap.Applied[len(snap.Applied)-1]
	if last.ItemKind != "tool_result" || !strings.Contains(last.Text, "EFFECT_OUTCOME_UNKNOWN") || !strings.Contains(last.Text, "It was not run again") {
		t.Fatalf("%+v", last)
	}
}

func TestReadingEvidenceThroughATaskIsConfinedToTheThread(t *testing.T) {
	e := newEnv(t)
	r := e.toolRun("tool.key.0000000000012")
	out := mustBind(t, r, batch(responseID(), call("c1", 0, "file.read", `{}`)))
	inThread := scalar[string](t, e.s.db, "SELECT evidence_id FROM items WHERE json_extract(metadata_json,'$.kind')='tool_calls'")
	_ = out
	got, err := e.s.ReadEvidenceInThread(bg, e.thread, protocol.EvidenceReadInput{EvidenceID: inThread, ProjectionVersion: "text/v1", Range: protocol.ByteRange{Start: 0, End: 10}})
	if err != nil || got.EvidenceID != inThread || got.TotalBytes == 0 {
		t.Fatalf("%+v %v", got, err)
	}
	// Another Thread's evidence, and one that does not exist, are the same refusal.
	for _, id := range []string{r.start.Intake.EvidenceID, identity.NewEvidenceID().String()} {
		_, err := e.s.ReadEvidenceInThread(bg, "thr_01a1151c-5a6f-7e06-996b-c21d45447041", protocol.EvidenceReadInput{EvidenceID: id, ProjectionVersion: "raw/v1", Range: protocol.ByteRange{Start: 0, End: 1}})
		if protocol.CodeOf(err) != protocol.CodeForbidden {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

func payloadOf[T any](t testing.TB, ev protocol.Event) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(strings.NewReader(string(ev.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestADeadDriversToolCallsAreSettledAsUnknownAndNotRun is the take-over of a Run that
// died in the middle of a Tool exchange: a call that was never dispatched is closed as
// not run, one that was dispatched is unknown (with what it had printed sealed as an
// incomplete capture), the exchange is applied to the context so the next Run sees what
// was done, and the verdict of the host check of the process is kept.
func TestADeadDriversToolCallsAreSettledAsUnknownAndNotRun(t *testing.T) {
	e := newEnv(t)
	r := e.toolRun("tool.key.0000000000020")
	rsp := responseID()
	out := mustBind(t, r, batch(rsp, call("c1", 0, "process.exec", `{}`), call("c2", 1, "file.create", `{}`)))
	r.phase("PreparingAction", "Executing")
	started := out.Calls[0]
	r.startTool(started)
	if err := e.s.RecordProcessStart(bg, r.fence, started.AttemptID, "boot-1", `{"pid":4242}`); err != nil {
		t.Fatal(err)
	}
	capID := identity.NewEvidenceID().String()
	if err := e.s.AppendCaptureChunk(bg, r.fence, CaptureChunk{EvidenceID: capID, AttemptID: started.AttemptID, Purpose: PurposeToolStdout, Ordinal: 0, Data: []byte("half")}); err != nil {
		t.Fatal(err)
	}

	// The process of the new driver takes the Thread.
	epoch, err := e.s.BumpWriterEpoch(bg, e.thread)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := e.s.StaleToolAttempts(bg, e.thread, epoch, false, nil)
	if err != nil || len(refs) != 1 || refs[0].AttemptID != started.AttemptID || refs[0].Tool != "process.exec" || refs[0].State != "running" ||
		refs[0].HostIncarnation != "boot-1" || refs[0].ProcessToken != `{"pid":4242}` {
		t.Fatalf("only the dispatched call is checked against the host: %+v %v", refs, err)
	}
	var seen StaleRun
	evs, err := e.s.TerminalizeStale(bg, e.thread, epoch, map[string]string{started.AttemptID: "gone"}, func(st StaleRun) (TerminalInput, error) {
		seen = st
		res := protocol.RunResult{RunID: st.RunID, TaskID: st.TaskID, Status: "blocked", Code: "EFFECT_OUTCOME_UNKNOWN", Verification: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}},
			EvidenceIDs: st.EvidenceIDs, Resumable: true, UnresolvedActionIDs: st.UnresolvedTools}
		return TerminalInput{Result: res, ResultEvidenceID: identity.NewEvidenceID().String()}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if typesOf(evs) != "action.completed,action.completed,run.terminal" {
		t.Fatalf("%s", typesOf(evs))
	}
	if len(seen.UnresolvedTools) != 1 || seen.UnresolvedTools[0] != started.ActionID || len(seen.Unresolved) != 0 {
		t.Fatalf("%+v", seen)
	}
	unknown := payloadOf[protocol.ActionCompletedPayload](t, evs[0])
	notRun := payloadOf[protocol.ActionCompletedPayload](t, evs[1])
	if unknown.ActionID != started.ActionID || unknown.EffectState != "unknown" || unknown.CaptureComplete || notRun.ActionID != out.Calls[1].ActionID || notRun.EffectState != "not_started" {
		t.Fatalf("%+v %+v", unknown, notRun)
	}
	if got := scalar[string](t, e.s.db, "SELECT state||'|'||json_extract(result_json,'$.reconcile') FROM attempts WHERE attempt_id=?", started.AttemptID); got != "unknown|gone" {
		t.Fatalf("%s", got)
	}
	if got := scalar[string](t, e.s.db, "SELECT state||'|'||capture_complete||'|'||total_bytes FROM evidence WHERE evidence_id=?", capID); got != "sealed|0|4" {
		t.Fatalf("what the process had printed: %s", got)
	}
	if scalar[string](t, e.s.db, "SELECT state FROM attempts WHERE attempt_id=?", out.Calls[1].AttemptID) != "cancelled" {
		t.Fatal("a call that was never dispatched is not closed as not run")
	}
	// The exchange is in the context; the Thread is free; the store is consistent.
	snap, err := e.s.LoadSnapshot(bg, r.start.RunID)
	if err != nil || len(snap.Applied) != 3 || snap.Applied[0].ItemKind != "tool_calls" || snap.Applied[1].ToolCallID != "c1" || snap.Applied[2].ToolCallID != "c2" ||
		!strings.Contains(snap.Applied[1].Text, "EFFECT_OUTCOME_UNKNOWN") {
		t.Fatalf("%+v %v", snap.Applied, err)
	}
	if scalar[string](t, e.s.db, "SELECT COALESCE(active_run_id,'none') FROM threads") != "none" || scalar[string](t, e.s.db, "SELECT status FROM runs") != "blocked" {
		t.Fatal("the Run is not settled")
	}
	if rep, err := e.s.VerifyClosure(bg); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
	// Nothing is left to settle.
	if again, err := e.s.StaleToolAttempts(bg, e.thread, epoch, false, nil); err != nil || len(again) != 0 {
		t.Fatalf("%+v %v", again, err)
	}
}
