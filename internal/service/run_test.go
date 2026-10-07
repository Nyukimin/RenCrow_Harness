package service_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/service"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// recorder is a connection's notifier: it keeps what the driver announced.
type recorder struct {
	mu     sync.Mutex
	events []protocol.Event
	deltas []protocol.ProgressDelta
	resets []protocol.ProgressReset
	// order is every notification in the order it arrived: "delta:<attempt>", "reset:<new attempt>"
	// or "event:<type>".
	order []string
}

func (r *recorder) ProgressDelta(d protocol.ProgressDelta) {
	r.mu.Lock()
	r.deltas = append(r.deltas, d)
	r.order = append(r.order, "delta:"+d.AttemptID)
	r.mu.Unlock()
}
func (r *recorder) ProgressReset(p protocol.ProgressReset) {
	r.mu.Lock()
	r.resets = append(r.resets, p)
	r.order = append(r.order, "reset:"+p.NewAttemptID)
	r.mu.Unlock()
}
func (r *recorder) Event(ev protocol.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.order = append(r.order, "event:"+ev.Type)
	r.mu.Unlock()
}

// notifications are the resets and the order of everything the recorder was told.
func (r *recorder) notifications() ([]protocol.ProgressReset, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]protocol.ProgressReset(nil), r.resets...), append([]string(nil), r.order...)
}
func (r *recorder) seen() ([]protocol.Event, []protocol.ProgressDelta) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]protocol.Event(nil), r.events...), append([]protocol.ProgressDelta(nil), r.deltas...)
}

// testClock is a clock a test moves.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// modelRig is a rig with a fake model whose Service announces to a recorder.
func modelRig(t *testing.T, fake *harnesstest.Fake) (*rig, *recorder) {
	t.Helper()
	r := newModelRig(t, fake, nil, nil)
	rec := &recorder{}
	r.conn = r.svc.NewConn(rec)
	return r, rec
}

func (r *rig) startRun(thread, key, text string, edit ...func(*protocol.StartInput)) protocol.StartResult {
	r.t.Helper()
	in := startParams(thread, key, text)
	for _, e := range edit {
		e(&in)
	}
	res := r.mustCall("turn/start", in)
	res.Done()
	return decode[protocol.StartResult](r.t, res)
}

func (r *rig) waitTerminal(runID string) protocol.RunInfo {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		info := decode[protocol.RunInfo](r.t, r.mustCall("run/get", protocol.RunGetInput{RunID: runID}))
		if info.Terminal {
			return info
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the run did not end; it is in phase %s", info.Phase)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *rig) threadEvents(thread string) []protocol.Event {
	r.t.Helper()
	var out []protocol.Event
	after := int64(0)
	for {
		page := decode[protocol.EventsReadResult](r.t, r.mustCall("events/read", protocol.EventsReadInput{ThreadID: thread, AfterSeq: after, Limit: 1000}))
		out = append(out, page.Events...)
		if !page.HasMore {
			return out
		}
		after = page.NextAfterSeq
	}
}

// val returns the first column of the first row of a read-only query as text.
func (r *rig) val(query string, args ...any) string {
	r.t.Helper()
	db, err := sql.Open("sqlite", "file:"+r.dep.DataRoot+"/"+sqlite.DatabaseFile+"?mode=ro")
	if err != nil {
		r.t.Fatal(err)
	}
	defer db.Close()
	var v sql.NullString
	if err := db.QueryRow(query, args...).Scan(&v); err != nil {
		r.t.Fatalf("%s: %v", query, err)
	}
	return v.String
}

func (r *rig) evidence(id string) string {
	r.t.Helper()
	// Read the whole Evidence in 64 KiB ranges as raw bytes.
	var out []byte
	for start := uint64(0); ; {
		res := decode[protocol.EvidenceReadResult](r.t, r.mustCall("evidence/read", protocol.EvidenceReadInput{
			EvidenceID: id, ProjectionVersion: "raw/v1", Range: protocol.ByteRange{Start: start, End: min(start+protocol.MaxEvidenceReadBytes, uint64Total(r, id))},
		}))
		b, err := base64.StdEncoding.DecodeString(res.DataBase64)
		if err != nil {
			r.t.Fatal(err)
		}
		out = append(out, b...)
		start += uint64(len(b))
		if start >= uint64(res.TotalBytes) {
			return string(out)
		}
	}
}

func uint64Total(r *rig, id string) uint64 {
	var n uint64
	for _, c := range r.val("SELECT total_bytes FROM evidence WHERE evidence_id=?", id) {
		n = n*10 + uint64(c-'0')
	}
	return n
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

func ofType(events []protocol.Event, typ string) []protocol.Event {
	var out []protocol.Event
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func envelope(text string) string {
	b, _ := json.Marshal(map[string]string{"origin": "automation", "text": text})
	canonical, _ := protocol.EncodeCanonicalContract(b)
	return "RENCROW_INPUT_DATA_V1\n" + string(canonical)
}

// TestARunIsExecutedToItsEnd is the path of this build from turn/start to run/get:
// every phase, the stored attempt, the events, the evidence, and the one run.terminal.
func TestARunIsExecutedToItsEnd(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Reply{Kind: harnesstest.KindFinal, Chunks: []string{"All ", "done."}, Reasoning: "never shown"})
	r, rec := modelRig(t, fake)
	info := r.openSession("run.open.000000000001")
	start := r.startRun(info.ThreadID, "run.start.00000000001", "make it so")

	run := r.waitTerminal(start.RunID)
	res := run.Result
	if run.Phase != "Terminal" || res == nil || res.Status != "completed" || res.Code != "FINAL_RESPONSE_ACCEPTED" || res.FinalText != "All done." ||
		res.FinalMessageID == nil || res.Resumable || res.LastCheckpointID != nil || len(res.UnresolvedActionIDs) != 0 {
		t.Fatalf("%+v", run)
	}
	// The Run's end is not the Task's meaning: nothing was verified, and it says so.
	if res.Verification.Status != "not_run" || len(res.Verification.EvidenceIDs) != 0 || res.Verification.CriteriaRevision != nil {
		t.Fatalf("verification must stay not_run: %+v", res.Verification)
	}
	if run.GenerationAttemptsUsed != 1 || run.GenerationAttemptsUnknown != 0 || run.ContextRevision != 2 {
		t.Fatalf("%+v", run)
	}

	events := r.threadEvents(info.ThreadID)
	if got := eventTypes(events); got != "session.created,input.accepted,task.created,run.started,input.applied,action.prepared,action.dispatch_started,"+
		"model.attempt_started,model.requested,model.completed,run.terminal" {
		t.Fatalf("events %s", got)
	}
	for i, e := range events {
		if e.EventSeq != int64(i+1) {
			t.Fatalf("event_seq is not 1..n: %d at %d", e.EventSeq, i)
		}
	}
	if n := len(ofType(events, "run.terminal")); n != 1 {
		t.Fatalf("%d run.terminal events", n)
	}
	// The events never carry the final text.
	for _, e := range events {
		if strings.Contains(string(e.Payload), "All done.") || strings.Contains(string(e.Payload), "make it so") {
			t.Fatalf("an event carries a body: %s", e.Payload)
		}
	}

	term := payloadOf[protocol.RunTerminalPayload](t, ofType(events, "run.terminal")[0])
	if term.Status != "completed" || term.Code != "FINAL_RESPONSE_ACCEPTED" || term.LastCheckpointID != nil {
		t.Fatalf("%+v", term)
	}
	// The result evidence is the same result run/get returns, and the final text is
	// sealed as Evidence of its own.
	var fromEvidence protocol.RunResult
	if err := json.Unmarshal([]byte(r.evidence(term.ResultEvidenceID)), &fromEvidence); err != nil || fromEvidence.Code != res.Code || fromEvidence.FinalText != "All done." ||
		fromEvidence.RunID != start.RunID {
		t.Fatalf("%v %+v", err, fromEvidence)
	}
	finalEvidence := r.val("SELECT evidence_id FROM items WHERE message_id=?", *res.FinalMessageID)
	if r.evidence(finalEvidence) != "All done." {
		t.Fatal("the final text is not sealed as Evidence")
	}
	listed := false
	for _, id := range res.EvidenceIDs {
		listed = listed || id == finalEvidence
	}
	if !listed {
		t.Fatalf("the result does not list the Evidence of its final text: %v", res.EvidenceIDs)
	}

	// model.requested and model.completed tie the attempt to what was measured and received.
	req := payloadOf[protocol.ModelRequestedPayload](t, ofType(events, "model.requested")[0])
	done := payloadOf[protocol.ModelCompletedPayload](t, ofType(events, "model.completed")[0])
	if req.Stage != "act" || req.Ordinal != 0 || !req.Stream || done.Outcome != "completed" || done.GenerationState != "terminal" || done.FailureCode != nil ||
		done.ActionID != req.ActionID || done.AttemptID != req.AttemptID {
		t.Fatalf("%+v %+v", req, done)
	}
	var receipt protocol.ModelAttemptReceipt
	if err := json.Unmarshal([]byte(r.evidence(done.AttemptReceiptEvidenceID)), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.InputDigest != req.InputDigest || receipt.RequestDigest != req.RequestDigest || receipt.BindingFingerprint != harnesstest.FakeFingerprint ||
		receipt.GenerationState != "terminal" || receipt.BackendAttempts == nil || *receipt.BackendAttempts != 1 || receipt.RecoveryProfile != "same_request" ||
		receipt.ResponseID == nil || !strings.HasPrefix(*receipt.ResponseID, "rsp_") || receipt.ResponseEvidenceID == nil || receipt.Ordinal != 0 {
		t.Fatalf("%+v", receipt)
	}
	if !strings.Contains(r.evidence(*receipt.ResponseEvidenceID), "rencrow.terminal") {
		t.Fatal("the response is not kept as received")
	}

	// What the model was asked: one describe, one count, one generation, no Tool, and
	// the expected_* values of the count.
	if fake.Describes() != 1 || len(fake.Measures()) != 1 || len(fake.Generates()) != 1 {
		t.Fatalf("describes=%d measures=%d generates=%d", fake.Describes(), len(fake.Measures()), len(fake.Generates()))
	}
	g := fake.Generates()[0]
	sys, _ := contextplan.ActSystemPrompt()
	if len(g.Tools) != 0 || g.ToolChoice != "none" || !g.Stream || g.StreamOptions == nil || !g.StreamOptions.IncludeUsage || g.Model != r.binding().Selector ||
		len(g.Messages) != 2 || g.Messages[0].Role != "system" || g.Messages[0].Text() != sys || g.Messages[1].Text() != envelope("make it so") ||
		g.Rencrow.Harness.ExpectedInputDigest != req.InputDigest || g.Rencrow.Harness.ExpectedRequestDigest != req.RequestDigest ||
		g.Rencrow.Harness.ExpectedBindingFingerprint != req.BindingFingerprint || g.Rencrow.Harness.MaxBackendAttempts != 1 ||
		g.Rencrow.Harness.Recovery.ProfileID != "same_request" || g.Rencrow.Harness.Recovery.RetryOfRequestID != nil {
		t.Fatalf("%+v", g)
	}
	m := fake.Measures()[0]
	if m.SafetyMarginTokens != r.dep.Config.Compaction.SafetyMarginTokens || m.Request.Rencrow.Harness.ExpectedInputDigest != "" ||
		*m.Request.Rencrow.RequestID == *g.Rencrow.RequestID || *g.Rencrow.RequestID != req.RequestID {
		t.Fatalf("a count and a generation are separate requests: %+v", m.Request.Rencrow)
	}

	// The store: one model Action with one Attempt and one model call, all ended.
	if got := r.val("SELECT kind||'/'||name||'/'||status FROM actions"); got != "model/act/completed" || r.count("actions") != 1 {
		t.Fatalf("actions %s", got)
	}
	if got := r.val("SELECT ordinal||'/'||state FROM attempts"); got != "0/completed" || r.count("attempts") != 1 {
		t.Fatalf("attempts %s", got)
	}
	if got := r.val("SELECT generation_state||'/'||backend_attempts||'/'||attempt_ordinal FROM model_calls"); got != "terminal/1/0" || r.count("model_calls") != 1 {
		t.Fatalf("model_calls %s", got)
	}
	if got := r.val("SELECT status FROM tasks"); got != "run_ended" {
		t.Fatalf("the Task is only marked as having had its Run end: %s", got)
	}
	if got := r.val("SELECT status FROM turns"); got != "ended" {
		t.Fatalf("turn %s", got)
	}
	if r.count("context_entries") != 2 || r.count("items") != 2 {
		t.Fatalf("the input and the final message are applied: entries=%d items=%d", r.count("context_entries"), r.count("items"))
	}
	if sess := decode[protocol.SessionInfo](t, r.mustCall("session/get", protocol.SessionGetInput{ThreadID: info.ThreadID})); sess.ActiveRunID != nil || sess.ContextRevision != 2 {
		t.Fatalf("%+v", sess)
	}

	// The connection was told about the commits the driver made, in order, and about the
	// text as it arrived, provisionally.
	announced, deltas := rec.seen()
	if len(announced) != 7 || announced[0].Type != "input.applied" || announced[6].Type != "run.terminal" {
		t.Fatalf("%s", eventTypes(announced))
	}
	for i, e := range announced {
		if e.EventID != events[4+i].EventID {
			t.Fatalf("announced event %d is not the committed one", i)
		}
	}
	if len(deltas) != 2 || deltas[0].Text != "All " || deltas[1].Text != "done." || deltas[0].Ordinal != 0 || deltas[1].Ordinal != 1 ||
		!deltas[0].Provisional || deltas[0].AttemptID != req.AttemptID || deltas[0].RunID != start.RunID {
		t.Fatalf("%+v", deltas)
	}

	// A replay is the original answer and starts nothing.
	again := r.mustCall("turn/start", startParams(info.ThreadID, "run.start.00000000001", "make it so"))
	again.Done()
	if len(again.Events) != 0 || len(fake.Generates()) != 1 || len(r.threadEvents(info.ThreadID)) != len(events) {
		t.Fatal("a replay started another run")
	}
}

// TestASecondTurnSeesTheFirst: what the model said is part of the Thread's history,
// in the order it was applied.
func TestASecondTurnSeesTheFirst(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Final("first answer"))
	r, _ := modelRig(t, fake)
	info := r.openSession("two.open.00000000000001")
	first := r.startRun(info.ThreadID, "two.start.0000000000001", "one")
	r.waitTerminal(first.RunID)

	fake.SetReply(harnesstest.Final("second answer"))
	second := r.startRun(info.ThreadID, "two.start.0000000000002", "two", func(in *protocol.StartInput) { in.ExpectedContextRevision = 2 })
	run := r.waitTerminal(second.RunID)
	if run.Result.Status != "completed" || run.Result.FinalText != "second answer" || run.ContextRevision != 4 {
		t.Fatalf("%+v", run)
	}
	g := fake.Generates()[1]
	var got []string
	for _, m := range g.Messages[1:] {
		got = append(got, m.Role+":"+m.Text())
	}
	want := []string{"user:" + envelope("one"), "assistant:first answer", "user:" + envelope("two")}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	if g.Messages[2].ToolCalls == nil || len(g.Messages[2].ToolCalls) != 0 {
		t.Fatal("an adopted assistant text carries tool_calls: []")
	}
	if n := len(ofType(r.threadEvents(info.ThreadID), "run.terminal")); n != 2 {
		t.Fatalf("one run.terminal per run, got %d", n)
	}
	// A stale expected revision is refused as it always was.
	_, err := r.call("turn/start", startParams(info.ThreadID, "two.start.0000000000003", "three"))
	wantCode(t, err, protocol.CodeRevisionConflict)
}

type outcomeCase struct {
	name       string
	setup      func(f *harnesstest.Fake)
	status     string
	code       string
	resumable  bool
	used       int64
	unknown    int64
	unresolved bool
	generates  int
	// attempt, when set, is "outcome/generation_state/failure_code" of model.completed.
	attempt string
}

// TestRunsEndAsTheirFailuresClassify is the end of a Run for each way the model side
// can go wrong: one generation at most, no retry, no Tool, one run.terminal, a free
// Thread, and an unknown generation never shown as anything else.
func TestRunsEndAsTheirFailuresClassify(t *testing.T) {
	hold := func(f *harnesstest.Fake, m harnesstest.MeasureConfig) { f.SetMeasure(m) }
	cases := []outcomeCase{
		{name: "a tool call where none was offered", setup: func(f *harnesstest.Fake) {
			f.SetReply(harnesstest.Reply{Kind: harnesstest.KindToolCall, Name: "file.read", Args: `{"path":"x"}`})
		}, status: "failed", code: "MODEL_CONTRACT_FAILED", resumable: true, used: 1, generates: 1, attempt: "error/terminal/MODEL_CONTRACT_FAILED"},
		{name: "length", setup: func(f *harnesstest.Fake) { f.SetReply(harnesstest.Reply{Kind: harnesstest.KindLength, Text: "cut o"}) },
			status: "incomplete", code: "MODEL_OUTPUT_TRUNCATED", resumable: true, used: 1, generates: 1, attempt: "incomplete/terminal/LENGTH"},
		{name: "a refusal", setup: func(f *harnesstest.Fake) { f.SetReply(harnesstest.Reply{Kind: harnesstest.KindRefusal}) },
			status: "rejected", code: "MODEL_REFUSED", resumable: false, used: 1, generates: 1, attempt: "refused/terminal/REFUSED"},
		// The failures F31 retries once end the Run as the failure classifies when the
		// retry fails the same way: two generations, never a third.
		{name: "reasoning only, twice", setup: func(f *harnesstest.Fake) {
			f.SetReply(harnesstest.Reply{Kind: harnesstest.KindReasoningOnly, Reasoning: "thinking"})
		}, status: "failed", code: "MODEL_OUTPUT_INVALID", resumable: true, used: 2, generates: 2, attempt: "error/terminal/REASONING_ONLY"},
		{name: "raw tool markup, twice", setup: func(f *harnesstest.Fake) {
			f.SetReply(harnesstest.Reply{Kind: harnesstest.KindErrorOutcome, Code: "RAW_TOOL_MARKUP", Text: "<tool_call>"})
		}, status: "failed", code: "MODEL_OUTPUT_INVALID", resumable: true, used: 2, generates: 2, attempt: "error/terminal/RAW_TOOL_MARKUP"},
		{name: "a transient upstream failure, twice", setup: func(f *harnesstest.Fake) {
			f.SetReply(harnesstest.Reply{Kind: harnesstest.KindErrorOutcome, Code: "UPSTREAM_TRANSIENT"})
		}, status: "failed", code: "MODEL_TRANSPORT_FAILED", resumable: true, used: 2, generates: 2, attempt: "error/terminal/UPSTREAM_TRANSIENT"},
		{name: "rate limited before generating, twice", setup: func(f *harnesstest.Fake) {
			f.SetReply(harnesstest.Reply{Kind: harnesstest.KindStrictError, Code: "RATE_LIMITED"})
		}, status: "incomplete", code: "MODEL_TEMPORARILY_UNAVAILABLE", resumable: true, used: 2, generates: 2, attempt: "error/not_started/RATE_LIMITED"},
		{name: "a rate limit that claims a finished generation contradicts itself and is not retried", setup: func(f *harnesstest.Fake) {
			f.SetReply(harnesstest.Reply{Kind: harnesstest.KindErrorOutcome, Code: "RATE_LIMITED"})
		}, status: "blocked", code: "MODEL_GENERATION_OUTCOME_UNKNOWN", resumable: true, used: 1, unknown: 1, unresolved: true, generates: 1,
			attempt: "error/unknown/MODEL_CONTRACT_FAILED"},
		{name: "a stream with no terminal frame", setup: func(f *harnesstest.Fake) {
			f.SetReply(harnesstest.Reply{Kind: harnesstest.KindNoTerminal, Text: "partial"})
		},
			status: "blocked", code: "MODEL_GENERATION_OUTCOME_UNKNOWN", resumable: true, used: 1, unknown: 1, unresolved: true, generates: 1,
			attempt: "error/unknown/MODEL_GENERATION_OUTCOME_UNKNOWN"},
		{name: "[DONE] without the terminal frame", setup: func(f *harnesstest.Fake) { f.SetReply(harnesstest.Reply{Kind: harnesstest.KindDoneOnly, Text: "x"}) },
			status: "blocked", code: "MODEL_GENERATION_OUTCOME_UNKNOWN", resumable: true, used: 1, unknown: 1, unresolved: true, generates: 1,
			attempt: "error/unknown/MODEL_CONTRACT_FAILED"},
		{name: "the call fails without a word", setup: func(f *harnesstest.Fake) { f.SetReply(harnesstest.Reply{Kind: harnesstest.KindTransportError}) },
			status: "blocked", code: "MODEL_GENERATION_OUTCOME_UNKNOWN", resumable: true, used: 1, unknown: 1, unresolved: true, generates: 1,
			attempt: "error/unknown/MODEL_GENERATION_OUTCOME_UNKNOWN"},
		{name: "a refusal before generating, with its receipt", setup: func(f *harnesstest.Fake) {
			f.SetReply(harnesstest.Reply{Kind: harnesstest.KindStrictError, Code: "AUTH_FAILED"})
		}, status: "blocked", code: "AUTH_FAILED", resumable: true, used: 1, generates: 1, attempt: "error/not_started/AUTH_FAILED"},
		{name: "a refusal before generating, without a receipt, is not taken for not_started", setup: func(f *harnesstest.Fake) {
			f.SetReply(harnesstest.Reply{Kind: harnesstest.KindStrictError, Code: "AUTH_FAILED", NoReceipt: true})
		}, status: "blocked", code: "MODEL_GENERATION_OUTCOME_UNKNOWN", resumable: true, used: 1, unknown: 1, unresolved: true, generates: 1,
			attempt: "error/unknown/AUTH_FAILED"},
		{name: "a receipt that contradicts the request", setup: func(f *harnesstest.Fake) {
			f.SetReply(harnesstest.Reply{Kind: harnesstest.KindFinal, Text: "ok", Mutate: func(m map[string]any) {
				m["harness_receipt"].(map[string]any)["request_digest"] = strings.Repeat("0", 64)
			}})
		}, status: "failed", code: "MODEL_CONTRACT_FAILED", resumable: true, used: 1, generates: 1, attempt: "error/terminal/MODEL_CONTRACT_FAILED"},
		{name: "a hidden retry", setup: func(f *harnesstest.Fake) {
			f.SetReply(harnesstest.Reply{Kind: harnesstest.KindFinal, Text: "ok", Mutate: func(m map[string]any) {
				m["harness_receipt"].(map[string]any)["hidden_retry"] = true
			}})
		}, status: "blocked", code: "MODEL_CONTRACT_FAILED", resumable: true, used: 1, unknown: 1, unresolved: true, generates: 1,
			attempt: "error/unknown/MODEL_CONTRACT_FAILED"},
		{name: "the binding changes between the count and the generation", setup: func(f *harnesstest.Fake) { f.BreakBindingAfterMeasure = true },
			status: "blocked", code: "BINDING_CHANGED", resumable: true, used: 1, generates: 1, attempt: "error/not_started/BINDING_CHANGED"},

		{name: "the prompt does not fit", setup: func(f *harnesstest.Fake) { hold(f, harnesstest.MeasureConfig{Lower: 40000, Upper: 40000}) },
			status: "blocked", code: "CAPACITY_BLOCKED", resumable: true},
		{name: "an interval that crosses the budget", setup: func(f *harnesstest.Fake) {
			hold(f, harnesstest.MeasureConfig{State: "verified_bound", Lower: 26000, Upper: 27000})
		}, status: "blocked", code: "BUDGET_UNVERIFIED", resumable: true},
		{name: "an estimate", setup: func(f *harnesstest.Fake) {
			hold(f, harnesstest.MeasureConfig{State: "estimated", Lower: 10, Upper: 20})
		},
			status: "blocked", code: "BUDGET_UNVERIFIED", resumable: true},
		{name: "an unverified count", setup: func(f *harnesstest.Fake) { hold(f, harnesstest.MeasureConfig{State: "unverified"}) },
			status: "blocked", code: "BUDGET_UNVERIFIED", resumable: true},
		{name: "a count of another input", setup: func(f *harnesstest.Fake) {
			f.MeasureHook = func(_ modelport.MeasureRequest, res *modelport.MeasureResult) error {
				res.InputDigest = strings.Repeat("1", 64)
				return nil
			}
		}, status: "blocked", code: "INPUT_DIGEST_MISMATCH", resumable: true},
		{name: "a count that is not valid", setup: func(f *harnesstest.Fake) {
			f.MeasureHook = func(_ modelport.MeasureRequest, res *modelport.MeasureResult) error {
				res.ContractVersion = "other"
				return nil
			}
		}, status: "failed", code: "MODEL_CONTRACT_FAILED", resumable: true},
		{name: "a binding that cannot be described", setup: func(f *harnesstest.Fake) { f.DescribeErr = context.DeadlineExceeded },
			status: "incomplete", code: "DEADLINE_EXCEEDED", resumable: true},
		{name: "a binding the model side does not offer", setup: func(f *harnesstest.Fake) { f.DescribeErr = &modelport.StrictError{Code: "MODEL_UNAVAILABLE"} },
			status: "blocked", code: "MODEL_UNAVAILABLE", resumable: true},
		{name: "a count that fails without a word", setup: func(f *harnesstest.Fake) { f.MeasureErr = context.Canceled },
			status: "incomplete", code: "DRIVER_STOPPED", resumable: true},
		{name: "a count the model side refuses", setup: func(f *harnesstest.Fake) { f.MeasureErr = &modelport.StrictError{Code: "UNSUPPORTED_CONTRACT"} },
			status: "blocked", code: "UNSUPPORTED_CONTRACT", resumable: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			tc.setup(fake)
			r, _ := modelRig(t, fake)
			info := r.openSession("outcome.open.0000000001")
			start := r.startRun(info.ThreadID, "outcome.start.000000001", "do it")
			run := r.waitTerminal(start.RunID)
			res := run.Result
			if res.Status != tc.status || res.Code != tc.code || res.Resumable != tc.resumable || (len(res.UnresolvedActionIDs) > 0) != tc.unresolved ||
				res.FinalText != "" || res.FinalMessageID != nil || res.Verification.Status != "not_run" {
				t.Fatalf("%+v", res)
			}
			if run.GenerationAttemptsUsed != tc.used || run.GenerationAttemptsUnknown != tc.unknown || len(fake.Generates()) != tc.generates {
				t.Fatalf("used=%d unknown=%d generates=%d", run.GenerationAttemptsUsed, run.GenerationAttemptsUnknown, len(fake.Generates()))
			}
			events := r.threadEvents(info.ThreadID)
			if n := len(ofType(events, "run.terminal")); n != 1 || events[len(events)-1].Type != "run.terminal" {
				t.Fatalf("run.terminal must be the last event, once: %s", eventTypes(events))
			}
			if term := payloadOf[protocol.RunTerminalPayload](t, events[len(events)-1]); term.Status != tc.status || term.Code != tc.code {
				t.Fatalf("%+v", term)
			}
			if tc.generates > 0 {
				done := ofType(events, "model.completed")
				if len(done) != tc.generates {
					t.Fatalf("one attempt, one model.completed: %s", eventTypes(events))
				}
				p := payloadOf[protocol.ModelCompletedPayload](t, done[len(done)-1])
				fc := ""
				if p.FailureCode != nil {
					fc = *p.FailureCode
				}
				if got := p.Outcome + "/" + p.GenerationState + "/" + fc; got != tc.attempt {
					t.Fatalf("model.completed %s, want %s", got, tc.attempt)
				}
				if got := r.val("SELECT state FROM attempts ORDER BY ordinal DESC LIMIT 1"); (tc.unknown > 0) != (got == "unknown") {
					t.Fatalf("attempt state %s", got)
				}
				if tc.unknown > 0 && r.val("SELECT backend_attempts IS NULL FROM model_calls ORDER BY attempt_ordinal DESC LIMIT 1") != "1" {
					t.Fatal("an unknown generation records backend_attempts as unknown, not as a count")
				}
			} else if r.count("actions") != 0 || r.count("attempts") != 0 || r.count("model_calls") != 0 {
				t.Fatal("nothing was dispatched, so no action or attempt exists")
			}
			if r.count("actions") > 1 {
				t.Fatal("a Tool action was created")
			}
			// The Thread is free again: nothing is left active, and the next request is admitted.
			if sess := decode[protocol.SessionInfo](t, r.mustCall("session/get", protocol.SessionGetInput{ThreadID: info.ThreadID})); sess.ActiveRunID != nil {
				t.Fatalf("%+v", sess)
			}
		})
	}
}

// TestADeadlineThatPassesBeforeGenerationEndsTheRun: the deadline is checked on the
// store's clock before the Run sends anything, and a Run that ends this way has
// dispatched nothing.
func TestADeadlineThatPassesBeforeGenerationEndsTheRun(t *testing.T) {
	fake := harnesstest.NewFake()
	clock := &testClock{now: testNow}
	r := newModelRig(t, fake, clock, nil)
	r.conn = r.svc.NewConn(&recorder{})
	fake.MeasureHook = func(modelport.MeasureRequest, *modelport.MeasureResult) error {
		clock.Advance(31 * time.Minute) // the Run's deadline is 30 minutes after admission
		return nil
	}
	info := r.openSession("late.open.000000000000001")
	start := r.startRun(info.ThreadID, "late.start.0000000000001", "x")
	run := r.waitTerminal(start.RunID)
	if res := run.Result; res.Status != "incomplete" || res.Code != "DEADLINE_EXCEEDED" || !res.Resumable || len(fake.Generates()) != 0 || run.GenerationAttemptsUsed != 0 {
		t.Fatalf("%+v %d", res, len(fake.Generates()))
	}
}

// TestAGenerationThatOutlivesTheDeadlineIsUnknown: stopping the connection at the
// deadline does not show that the generation stopped, so the Run is blocked with the
// generation's outcome unknown, not reported as a plain deadline.
func TestAGenerationThatOutlivesTheDeadlineIsUnknown(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Reply{Kind: harnesstest.KindHang})
	r, _ := modelRig(t, fake)
	info := r.openSession("hang.open.00000000000001")
	start := r.startRun(info.ThreadID, "hang.start.000000000001", "x", func(in *protocol.StartInput) { in.Limits.DeadlineSeconds = 1 })
	run := r.waitTerminal(start.RunID)
	res := run.Result
	if res.Status != "blocked" || res.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" || len(res.UnresolvedActionIDs) != 1 || run.GenerationAttemptsUnknown != 1 || !res.Resumable {
		t.Fatalf("%+v", res)
	}
	done := payloadOf[protocol.ModelCompletedPayload](t, ofType(r.threadEvents(info.ThreadID), "model.completed")[0])
	if done.FailureCode == nil || *done.FailureCode != "DEADLINE_EXCEEDED" || done.GenerationState != "unknown" {
		t.Fatalf("%+v", done)
	}
}

// TestAnActiveRunKeepsTheThreadBusyAndStopIsRecorded: while a generation is in
// flight the Thread is BUSY, and a Run that is stopped records how it ended (an
// unknown generation, left unresolved) before the Service lets go.
func TestAnActiveRunKeepsTheThreadBusyAndStopIsRecorded(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Reply{Kind: harnesstest.KindHang})
	inFlight := make(chan struct{})
	var once sync.Once
	fake.OnGenerate = func(context.Context, modelport.ChatRequest) { once.Do(func() { close(inFlight) }) }
	r, _ := modelRig(t, fake)
	info := r.openSession("stop.open.00000000000001")
	start := r.startRun(info.ThreadID, "stop.start.000000000001", "x")
	select {
	case <-inFlight:
	case <-time.After(10 * time.Second):
		t.Fatal("the generation never started")
	}
	_, err := r.call("turn/start", startParams(info.ThreadID, "stop.start.000000000002", "second"))
	wantCode(t, err, protocol.CodeBusy)
	if run := decode[protocol.RunInfo](t, r.mustCall("run/get", protocol.RunGetInput{RunID: start.RunID})); run.Terminal || run.Phase != "Generating" {
		t.Fatalf("a Run in flight is not settled by another request: %+v", run)
	}

	r.mustCall("service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
	r.svc.Quiesce()
	run := r.waitTerminal(start.RunID)
	res := run.Result
	if res.Status != "blocked" || res.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" || len(res.UnresolvedActionIDs) != 1 || !res.Resumable || run.GenerationAttemptsUnknown != 1 {
		t.Fatalf("%+v", res)
	}
	done := payloadOf[protocol.ModelCompletedPayload](t, ofType(r.threadEvents(info.ThreadID), "model.completed")[0])
	if done.FailureCode == nil || *done.FailureCode != "DRIVER_STOPPED" {
		t.Fatalf("%+v", done)
	}
}

// TestAStoppedRunWithNothingDispatchedIsIncomplete is the same stop before a
// generation was sent: nothing is unresolved, and the Run is only incomplete.
func TestAStoppedRunWithNothingDispatchedIsIncomplete(t *testing.T) {
	fake := harnesstest.NewFake()
	reached := make(chan struct{})
	var once sync.Once
	fake.OnMeasure = func(ctx context.Context) {
		once.Do(func() { close(reached) })
		<-ctx.Done()
	}
	r, _ := modelRig(t, fake)
	info := r.openSession("stop2.open.0000000000001")
	start := r.startRun(info.ThreadID, "stop2.start.00000000001", "x")
	<-reached
	r.mustCall("service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
	r.svc.Quiesce()
	run := r.waitTerminal(start.RunID)
	if res := run.Result; res.Status != "incomplete" || res.Code != "DRIVER_STOPPED" || len(res.UnresolvedActionIDs) != 0 || !res.Resumable || len(fake.Generates()) != 0 || run.GenerationAttemptsUsed != 0 {
		t.Fatalf("%+v", res)
	}
}

func resultKey(r *protocol.RunResult) string {
	return r.Status + "/" + r.Code + "/" + map[bool]string{true: "resumable", false: "final"}[r.Resumable] + "/" + map[bool]string{true: "unresolved", false: "none"}[len(r.UnresolvedActionIDs) > 0]
}

// TestARunLeftByADeadProcessIsSettledByTheNextDriver is the Admitting problem: a Run
// that was admitted and never ended must not keep the Thread busy for ever. Taking the
// writer role at a new epoch ends it, deterministically, and the way it ends is the
// way the same Run ends when its own driver is stopped.
func TestARunLeftByADeadProcessIsSettledByTheNextDriver(t *testing.T) {
	t.Run("admitted and never driven", func(t *testing.T) {
		r := newRig(t, nil)
		info := r.openSession("orphan.open.000000000001")
		start := decode[protocol.StartResult](t, r.mustCall("turn/start", startParams(info.ThreadID, "orphan.start.00000000001", "first")))
		_ = r.writers.Close() // the process ends: the OS drops its locks

		_, svc2 := r.process()
		conn2 := svc2.NewConn(nil)
		call := func(method string, params any) service.Result {
			res, err := svc2.Handle(context.Background(), conn2, request(t, method, params))
			if err != nil {
				t.Fatalf("%s: %v", method, err)
			}
			return res
		}
		call("initialize", protocol.InitializeInput{ClientName: "t", ClientVersion: "0", ProtocolVersion: protocol.ProtocolVersion})
		// Until a driver takes the Thread, the Run is as it was left.
		if run := decode[protocol.RunInfo](t, call("run/get", protocol.RunGetInput{RunID: start.RunID})); run.Terminal || run.Phase != "Admitting" {
			t.Fatalf("%+v", run)
		}
		res := call("turn/start", startParams(info.ThreadID, "orphan.start.00000000002", "second"))
		if got := eventTypes(res.Events); got != "run.terminal,input.accepted,task.created,run.started" {
			t.Fatalf("the settled Run's end precedes the new admission: %s", got)
		}
		old := decode[protocol.RunInfo](t, call("run/get", protocol.RunGetInput{RunID: start.RunID}))
		if !old.Terminal || old.Phase != "Terminal" || old.Result == nil || resultKey(old.Result) != "incomplete/DRIVER_STOPPED/resumable/none" ||
			old.Result.FinalText != "" || old.Result.FinalMessageID != nil || old.Result.Verification.Status != "not_run" {
			t.Fatalf("%+v", old)
		}
		term := payloadOf[protocol.RunTerminalPayload](t, res.Events[0])
		if term.Status != "incomplete" || term.Code != "DRIVER_STOPPED" || res.Events[0].RunID == nil || *res.Events[0].RunID != start.RunID {
			t.Fatalf("%+v", term)
		}
		newStart := decode[protocol.StartResult](t, res)
		if sess := decode[protocol.SessionInfo](t, call("session/get", protocol.SessionGetInput{ThreadID: info.ThreadID})); sess.ActiveRunID == nil || *sess.ActiveRunID != newStart.RunID {
			t.Fatalf("%+v", sess)
		}
		if got := r.val("SELECT COUNT(*) FROM events WHERE type='run.terminal' AND run_id=?", start.RunID); got != "1" {
			t.Fatalf("%s run.terminal events for the settled Run", got)
		}
	})

	t.Run("past its deadline", func(t *testing.T) {
		clock := &testClock{now: testNow}
		r := newModelRig(t, nil, clock, nil)
		info := r.openSession("orphan2.open.00000000001")
		start := decode[protocol.StartResult](t, r.mustCall("turn/start", startParams(info.ThreadID, "orphan2.start.0000000001", "first")))
		_ = r.writers.Close()
		clock.Advance(31 * time.Minute)
		_, svc2 := r.process()
		conn2 := svc2.NewConn(nil)
		for _, m := range []struct {
			method string
			params any
		}{
			{"initialize", protocol.InitializeInput{ClientName: "t", ClientVersion: "0", ProtocolVersion: protocol.ProtocolVersion}},
			{"turn/start", startParams(info.ThreadID, "orphan2.start.0000000002", "second")},
		} {
			if _, err := svc2.Handle(context.Background(), conn2, request(t, m.method, m.params)); err != nil {
				t.Fatal(err)
			}
		}
		old := decode[protocol.RunInfo](t, mustHandle(t, svc2, conn2, "run/get", protocol.RunGetInput{RunID: start.RunID}))
		if resultKey(old.Result) != "incomplete/DEADLINE_EXCEEDED/resumable/none" {
			t.Fatalf("%+v", old.Result)
		}
	})

	t.Run("with a generation in flight", func(t *testing.T) {
		fake := harnesstest.NewFake()
		fake.SetReply(harnesstest.Reply{Kind: harnesstest.KindHang})
		inFlight := make(chan struct{})
		var once sync.Once
		fake.OnGenerate = func(context.Context, modelport.ChatRequest) { once.Do(func() { close(inFlight) }) }
		r, _ := modelRig(t, fake)
		info := r.openSession("orphan3.open.00000000001")
		start := r.startRun(info.ThreadID, "orphan3.start.0000000001", "first")
		<-inFlight
		firstSvc := r.svc
		_ = r.writers.Close() // the process dies with the generation in flight

		r.model = nil
		_, svc2 := r.process()
		conn2 := svc2.NewConn(nil)
		mustHandle(t, svc2, conn2, "initialize", protocol.InitializeInput{ClientName: "t", ClientVersion: "0", ProtocolVersion: protocol.ProtocolVersion})
		// The first Run's input was applied: the Thread's context is at revision 1.
		res := mustHandle(t, svc2, conn2, "turn/start", startParamsAt(info.ThreadID, "orphan3.start.0000000002", "second", 1))
		if got := eventTypes(res.Events); got != "model.completed,run.terminal,input.accepted,task.created,run.started" {
			t.Fatalf("%s", got)
		}
		done := payloadOf[protocol.ModelCompletedPayload](t, res.Events[0])
		if done.Outcome != "error" || done.GenerationState != "unknown" || done.FailureCode == nil || *done.FailureCode != "MODEL_GENERATION_OUTCOME_UNKNOWN" {
			t.Fatalf("%+v", done)
		}
		old := decode[protocol.RunInfo](t, mustHandle(t, svc2, conn2, "run/get", protocol.RunGetInput{RunID: start.RunID}))
		if resultKey(old.Result) != "blocked/MODEL_GENERATION_OUTCOME_UNKNOWN/resumable/unresolved" || old.GenerationAttemptsUnknown != 1 || old.GenerationAttemptsUsed != 1 {
			t.Fatalf("%+v", old)
		}
		if r.val("SELECT state FROM attempts") != "unknown" || r.val("SELECT generation_state||'/'||(backend_attempts IS NULL) FROM model_calls") != "unknown/1" {
			t.Fatal("the attempt must be recorded as unknown")
		}
		before := len(r.threadEvents(info.ThreadID))
		// The driver that lost the Thread is still waiting. When it is stopped it finds
		// it no longer owns the Run and writes nothing.
		r.mustCall("service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
		firstSvc.Quiesce()
		time.Sleep(50 * time.Millisecond)
		if after := len(r.threadEvents(info.ThreadID)); after != before {
			t.Fatalf("a driver that lost the Thread wrote %d more events", after-before)
		}
		if got := r.val("SELECT COUNT(*) FROM events WHERE type='run.terminal' AND run_id=?", start.RunID); got != "1" {
			t.Fatalf("%s run.terminal events", got)
		}
		if r.val("SELECT status FROM runs WHERE run_id=?", start.RunID) != "blocked" {
			t.Fatal("the settled Run changed")
		}
	})
}

func startParamsAt(thread, key, text string, contextRevision int64) protocol.StartInput {
	in := startParams(thread, key, text)
	in.ExpectedContextRevision = contextRevision
	return in
}

func mustHandle(t *testing.T, svc *service.Service, conn *service.Conn, method string, params any) service.Result {
	t.Helper()
	res, err := svc.Handle(context.Background(), conn, request(t, method, params))
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return res
}

// TestAStoppedRunAndAnOrphanedRunEndTheSameWay: the two ways a Run can lose its
// driver (it is stopped, or the process dies) are classified by the same function, so
// the same facts give the same result.
func TestAStoppedRunAndAnOrphanedRunEndTheSameWay(t *testing.T) {
	stopped := func(kind harnesstest.Kind, inFlight bool) string {
		fake := harnesstest.NewFake()
		fake.SetReply(harnesstest.Reply{Kind: kind})
		reached := make(chan struct{})
		var once sync.Once
		if inFlight {
			fake.OnGenerate = func(context.Context, modelport.ChatRequest) { once.Do(func() { close(reached) }) }
		} else {
			fake.OnMeasure = func(ctx context.Context) { once.Do(func() { close(reached) }); <-ctx.Done() }
		}
		r, _ := modelRig(t, fake)
		info := r.openSession("same.open.0000000000000001")
		start := r.startRun(info.ThreadID, "same.start.00000000000001", "x")
		<-reached
		r.mustCall("service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
		r.svc.Quiesce()
		return resultKey(r.waitTerminal(start.RunID).Result)
	}
	orphaned := func(kind harnesstest.Kind, inFlight bool) string {
		fake := harnesstest.NewFake()
		fake.SetReply(harnesstest.Reply{Kind: kind})
		reached := make(chan struct{})
		var once sync.Once
		if inFlight {
			fake.OnGenerate = func(context.Context, modelport.ChatRequest) { once.Do(func() { close(reached) }) }
		} else {
			fake.OnMeasure = func(ctx context.Context) { once.Do(func() { close(reached) }); <-ctx.Done() }
		}
		r, _ := modelRig(t, fake)
		info := r.openSession("same.open.0000000000000002")
		start := r.startRun(info.ThreadID, "same.start.00000000000002", "x")
		<-reached
		// A Run that loaded its input has applied it; one stopped before measuring has too.
		contextRevision := int64(1)
		_ = r.writers.Close()
		r.model = nil
		_, svc2 := r.process()
		conn2 := svc2.NewConn(nil)
		mustHandle(t, svc2, conn2, "initialize", protocol.InitializeInput{ClientName: "t", ClientVersion: "0", ProtocolVersion: protocol.ProtocolVersion})
		mustHandle(t, svc2, conn2, "turn/start", startParamsAt(info.ThreadID, "same.start.00000000000003", "y", contextRevision))
		key := resultKey(decode[protocol.RunInfo](t, mustHandle(t, svc2, conn2, "run/get", protocol.RunGetInput{RunID: start.RunID})).Result)
		r.mustCall("service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
		r.svc.Quiesce()
		return key
	}
	for name, tc := range map[string]struct {
		kind     harnesstest.Kind
		inFlight bool
		want     string
	}{
		"nothing dispatched":     {harnesstest.KindFinal, false, "incomplete/DRIVER_STOPPED/resumable/none"},
		"a generation in flight": {harnesstest.KindHang, true, "blocked/MODEL_GENERATION_OUTCOME_UNKNOWN/resumable/unresolved"},
	} {
		t.Run(name, func(t *testing.T) {
			a, b := stopped(tc.kind, tc.inFlight), orphaned(tc.kind, tc.inFlight)
			if a != tc.want || b != tc.want {
				t.Fatalf("stopped %s, orphaned %s, want %s", a, b, tc.want)
			}
		})
	}
}

// TestTheTypedBlocksReachTheModelInTheirOrder is A03 at the end of the whole path: the
// blocks a caller sent with the input are stored as they were, read back, and put in
// the prompt in the one order the projection fixes, with the input where it was applied.
func TestTheTypedBlocksReachTheModelInTheirOrder(t *testing.T) {
	fake := harnesstest.NewFake()
	r, _ := modelRig(t, fake)
	info := r.openSession("blocks.open.000000000001")
	mk := func(kind, text string) protocol.ContextBlock {
		rev, err := protocol.ContextRevision(kind, text, nil)
		if err != nil {
			t.Fatal(err)
		}
		return protocol.ContextBlock{Kind: kind, Text: text, Revision: rev}
	}
	blocks := []protocol.ContextBlock{
		mk(protocol.KindVariableRuntimeContext, "now=noon"), mk(protocol.KindRecallPack, "earlier: a\n"),
		mk(protocol.KindStableRuntimeContext, ""), mk(protocol.KindCharacterSystemPrompt, " persona\n"), mk(protocol.KindRecallPack, "earlier: b"),
	}
	start := r.startRun(info.ThreadID, "blocks.start.00000000001", "  the task\r\n", func(in *protocol.StartInput) { in.ContextBlocks = blocks })
	if res := r.waitTerminal(start.RunID).Result; res.Status != "completed" {
		t.Fatalf("%+v", res)
	}
	g := fake.Generates()[0]
	var got []string
	for _, m := range g.Messages {
		got = append(got, m.Role)
	}
	if strings.Join(got, ",") != "system,system,developer,user,user,user,user" {
		t.Fatalf("roles %v", got)
	}
	sys, _ := contextplan.ActSystemPrompt()
	ctxData := func(b protocol.ContextBlock) string {
		raw, _ := json.Marshal(b)
		out, _ := protocol.EncodeCanonicalContract(raw)
		return "RENCROW_CONTEXT_DATA_V1\n" + string(out)
	}
	want := []string{sys, " persona\n", "", ctxData(blocks[1]), ctxData(blocks[4]), ctxData(blocks[0]), envelope("  the task\r\n")}
	for i, m := range g.Messages {
		if m.Text() != want[i] {
			t.Fatalf("message %d:\n got %q\nwant %q", i, m.Text(), want[i])
		}
	}
}
