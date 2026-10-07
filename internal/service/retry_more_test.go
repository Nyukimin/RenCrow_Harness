package service_test

import (
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// TestTheSameRequestIsCountedAsTheSameRequest: a retry of the same request that the model
// side counts to another request digest is not what failed, and nothing is sent on it.
func TestTheSameRequestIsCountedAsTheSameRequest(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(reasoningOnly(), harnesstest.Final("never asked for"))
	var counts atomic.Int32
	fake.MeasureHook = func(_ modelport.MeasureRequest, res *modelport.MeasureResult) error {
		if counts.Add(1) == 2 {
			res.RequestDigest = strings.Repeat("9", 64)
		}
		return nil
	}
	r, _ := modelRig(t, fake)
	info := r.openSession("samereq.open.000000001")
	run := r.waitTerminal(r.startRun(info.ThreadID, "samereq.start.00000001", "go").RunID)
	res := run.Result
	if res.Status != "blocked" || res.Code != "REQUEST_DIGEST_MISMATCH" || len(fake.Generates()) != 1 || len(fake.Measures()) != 2 || r.count("attempts") != 1 {
		t.Fatalf("%+v", res)
	}
}

// TestTheEventsOfARetryFollowFromEachOther: the retry is scheduled because the first
// Attempt ended, and is dispatched because it was scheduled; the events say so in their
// causation and dependencies, and every event of the chain names the Evidence it traces.
func TestTheEventsOfARetryFollowFromEachOther(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(reasoningOnly(), harnesstest.Final("fine"))
	r, _ := modelRig(t, fake)
	info := r.openSession("causal.open.0000000001")
	r.waitTerminal(r.startRun(info.ThreadID, "causal.start.000000001", "go").RunID)
	events := r.threadEvents(info.ThreadID)
	id := func(typ string, nth int) string { return ofType(events, typ)[nth].EventID }
	cause := func(eventID string) string {
		return r.val(`SELECT COALESCE(causation_event_id,'') FROM events WHERE event_id=?`, eventID)
	}
	deps := func(eventID string) string {
		return r.val(`SELECT dependency_event_ids_json FROM events WHERE event_id=?`, eventID)
	}
	scheduled := id("model.retry_scheduled", 0)
	if cause(scheduled) != id("model.completed", 0) || deps(scheduled) != `["`+id("model.completed", 0)+`"]` {
		t.Fatalf("the retry was not scheduled because the first Attempt ended: %s %s", cause(scheduled), deps(scheduled))
	}
	dispatch := id("action.dispatch_started", 1)
	if cause(dispatch) != scheduled {
		t.Fatalf("the retry was not dispatched because it was scheduled: %s", cause(dispatch))
	}
	if cause(id("model.attempt_started", 1)) != dispatch || cause(id("model.requested", 1)) != id("model.attempt_started", 1) {
		t.Fatal("the events of the retry's reservation do not follow one another")
	}
	for _, typ := range []string{"model.retry_scheduled", "model.attempt_started", "model.requested", "model.completed"} {
		for _, ev := range ofType(events, typ) {
			if ev.EvidenceID == nil {
				t.Fatalf("%s names no Evidence", typ)
			}
		}
	}
	// A reader can get from the retry event to the failed Attempt's receipt, and from there to
	// the failed response.
	var decision map[string]any
	if err := json.Unmarshal([]byte(r.evidence(*ofType(events, "model.retry_scheduled")[0].EvidenceID)), &decision); err != nil {
		t.Fatal(err)
	}
	var receipt protocol.ModelAttemptReceipt
	if err := json.Unmarshal([]byte(r.evidence(decision["attempt_receipt_evidence_id"].(string))), &receipt); err != nil || receipt.ResponseEvidenceID == nil {
		t.Fatalf("%v %+v", err, receipt)
	}
	if got := r.val(`SELECT media_type FROM evidence WHERE evidence_id=?`, *receipt.ResponseEvidenceID); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatal(got)
	}
}

// TestEveryActionHasItsOwnFirstAttemptAndItsOwnRetry: a retry that answered with Tool calls
// is followed by a new Action, whose Attempt counting starts again, so that Action can
// itself be retried once; the Run's model steps are the Actions, not the Attempts.
func TestEveryActionHasItsOwnFirstAttemptAndItsOwnRetry(t *testing.T) {
	fake := harnesstest.NewFake()
	r, _ := toolRig(t, fake, toolConfig{})
	r.write("demo.txt", "demo content")
	fake.SetScript(
		reasoningOnly(), // step 1, first Attempt
		calls(tc("c1", "file.read", readArgs("demo.txt", 0, 100, 100))), // step 1, its retry
		rawMarkup("<tool_call>"),  // step 2, first Attempt
		harnesstest.Final("done"), // step 2, its retry
	)
	info := r.openSession("steps.open.000000000001")
	run := r.waitTerminal(r.startRun(info.ThreadID, "steps.start.00000000001", "go", func(in *protocol.StartInput) { in.Limits.MaxModelSteps = 2 }).RunID)
	if run.Result.Status != "completed" || run.Result.FinalText != "done" || run.GenerationAttemptsUsed != 4 || len(fake.Generates()) != 4 {
		t.Fatalf("%+v used=%d", run.Result, run.GenerationAttemptsUsed)
	}
	if got := r.val(`SELECT COUNT(*) FROM actions WHERE kind='model'`); got != "2" {
		t.Fatalf("%s model actions", got)
	}
	perAction := r.val(`SELECT group_concat(n, ',') FROM (SELECT COUNT(*) n FROM attempts a JOIN actions ac ON ac.action_id=a.action_id WHERE ac.kind='model' GROUP BY ac.action_id)`)
	if perAction != "2,2" {
		t.Fatalf("attempts per model action: %s", perAction)
	}
	// Two retries were scheduled, one per Action, and the Tool ran once between them.
	events := r.threadEvents(info.ThreadID)
	if n := len(ofType(events, "model.retry_scheduled")); n != 2 {
		t.Fatalf("%d retries: %s", n, eventTypes(events))
	}
	a, b := payloadOf[protocol.ModelRetryScheduledPayload](t, ofType(events, "model.retry_scheduled")[0]), payloadOf[protocol.ModelRetryScheduledPayload](t, ofType(events, "model.retry_scheduled")[1])
	if a.ActionID == b.ActionID || a.TriggerCode != "REASONING_ONLY" || b.TriggerCode != "RAW_TOOL_MARKUP" {
		t.Fatalf("%+v %+v", a, b)
	}
	if got := r.val(`SELECT COUNT(*) FROM tool_links`); got != "1" {
		t.Fatal(got)
	}
	// The failed outputs are in neither prompt that followed them: the third request carries
	// the Tool's exchange and nothing of the two failures.
	third := fake.Generates()[2]
	var roles []string
	for _, m := range third.Messages {
		roles = append(roles, m.Role)
	}
	if !slices.Equal(roles[len(roles)-2:], []string{"assistant", "tool"}) {
		t.Fatalf("%v", roles)
	}
}

// TestWhyAGenerationEndedAsItDidIsKeptBesideItsReceipt: an Attempt that ended as a contract
// failure or as an unknown generation keeps a private diagnosis, without any model text,
// of what in the response broke the contract; one that ended well keeps none.
func TestWhyAGenerationEndedAsItDidIsKeptBesideItsReceipt(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Reply{Kind: harnesstest.KindDoneOnly, Text: "a secret that must not be repeated"})
	r, _ := modelRig(t, fake)
	info := r.openSession("diag.open.0000000000001")
	run := r.waitTerminal(r.startRun(info.ThreadID, "diag.start.000000000001", "go").RunID)
	if run.Result.Status != "blocked" || run.Result.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" {
		t.Fatalf("%+v", run.Result)
	}
	id := r.val(`SELECT evidence_id FROM evidence WHERE json_extract(metadata_json,'$.purpose')='model_attempt_diagnosis'`)
	var d map[string]any
	if err := json.Unmarshal([]byte(r.evidence(id)), &d); err != nil {
		t.Fatal(err)
	}
	if d["kind"] != "model_attempt_diagnosis" || d["failure_code"] != "MODEL_CONTRACT_FAILED" || d["generation_state"] != "unknown" ||
		d["violation"] != "the stream ended without the terminal frame" || d["attempt_id"] != r.val(`SELECT attempt_id FROM attempts`) {
		t.Fatalf("%v", d)
	}
	if strings.Contains(r.evidence(id), "secret") {
		t.Fatal("the diagnosis repeats what the model said")
	}

	ok := harnesstest.NewFake()
	r2, _ := modelRig(t, ok)
	info2 := r2.openSession("diag2.open.000000000001")
	r2.waitTerminal(r2.startRun(info2.ThreadID, "diag2.start.00000000001", "go").RunID)
	if got := r2.val(`SELECT COUNT(*) FROM evidence WHERE json_extract(metadata_json,'$.purpose')='model_attempt_diagnosis'`); got != "0" {
		t.Fatalf("an Attempt that ended well kept a diagnosis: %s", got)
	}
}
