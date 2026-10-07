package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// TestARetryThatEndsUnknownBlocksTheTaskAndAResumeGeneratesNothing is A52 for the second
// Attempt: the retry's generation has no known end, so the Run is blocked with it
// unresolved (one Action, whose second Attempt is the unknown one), the unknown is counted
// once, and a resume of the Task ends at once, blocked, without a call to the model
// side: the retry is never sent again to find out.
func TestARetryThatEndsUnknownBlocksTheTaskAndAResumeGeneratesNothing(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(reasoningOnly(), harnesstest.Reply{Kind: harnesstest.KindNoTerminal, Text: "partial"}, harnesstest.Final("never asked for"))
	r, _ := modelRig(t, fake)
	info := r.openSession("unkretry.open.000000001")
	start := r.startRun(info.ThreadID, "unkretry.start.00000001", "go")
	run := r.waitTerminal(start.RunID)
	res := run.Result
	if res.Status != "blocked" || res.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" || len(res.UnresolvedActionIDs) != 1 || !res.Resumable ||
		run.GenerationAttemptsUsed != 2 || run.GenerationAttemptsUnknown != 1 || len(fake.Generates()) != 2 {
		t.Fatalf("%+v used=%d unknown=%d", res, run.GenerationAttemptsUsed, run.GenerationAttemptsUnknown)
	}
	if got := r.val(`SELECT group_concat(a.ordinal||':'||a.state, ',') FROM (SELECT * FROM attempts ORDER BY ordinal) a`); got != "0:failed,1:unknown" {
		t.Fatal(got)
	}
	if got := r.val(`SELECT backend_attempts IS NULL FROM model_calls WHERE attempt_ordinal=1`); got != "1" {
		t.Fatal("an unknown generation records backend_attempts as unknown, not as a count")
	}
	if got := r.val(`SELECT backend_attempts FROM model_calls WHERE attempt_ordinal=0`); got != "1" {
		t.Fatalf("the first Attempt's generation was known to have ended: %s", got)
	}

	describes, measures, generates := fake.Describes(), len(fake.Measures()), len(fake.Generates())
	rr := decode[protocol.ResumeResult](t, r.mustCall("run/resume", resumeInput(start.TaskID, start.RunID, "unkretry.resume.0000001")))
	next := r.waitTerminal(rr.RunID)
	if resultKey(next.Result) != "blocked/MODEL_GENERATION_OUTCOME_UNKNOWN/resumable/unresolved" || next.GenerationAttemptsUsed != 0 {
		t.Fatalf("%+v", next.Result)
	}
	if fake.Describes() != describes || len(fake.Measures()) != measures || len(fake.Generates()) != generates {
		t.Fatal("a resume called the model port while a generation of the Task is unknown")
	}
}

// TestARunStoppedWithItsDriverWhileTheRetryWaitsIsIncompleteAndNothingIsUnknown: the
// process is told to stop while the backoff runs; the failed Attempt had ended and was
// known, so the Run is incomplete (stopped with its driver), nothing is unresolved, and
// no second Attempt exists.
func TestARunStoppedWithItsDriverWhileTheRetryWaitsIsIncompleteAndNothingIsUnknown(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(transient(), harnesstest.Final("never asked for"))
	r, _ := modelRig(t, fake)
	waiting := make(chan struct{}, 1)
	r.waitHook = func(ctx context.Context, _ time.Duration) error {
		waiting <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	info := r.openSession("drvstop.open.000000001")
	start := r.startRun(info.ThreadID, "drvstop.start.00000001", "go")
	<-waiting
	r.mustCall("service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
	r.svc.Quiesce()
	run := r.waitTerminal(start.RunID)
	res := run.Result
	if res.Status != "incomplete" || res.Code != "DRIVER_STOPPED" || len(res.UnresolvedActionIDs) != 0 || !res.Resumable || len(fake.Generates()) != 1 ||
		run.GenerationAttemptsUnknown != 0 || r.count("attempts") != 1 {
		t.Fatalf("%+v", res)
	}
}

// TestAnErrorTerminalsReceiptIsHeldToTheRequestLikeAnyOther is A52 for the terminals that
// are not answers: a stream that ends in an error, an incomplete or a refused outcome
// carries a receipt, and the receipt is believed only as far as it matches the request and
// the table: another request, binding, stage or profile is a contract failure whatever code
// it carries, and a code that cannot come with the state it states is a frame that
// contradicts itself. None of them is retried.
func TestAnErrorTerminalsReceiptIsHeldToTheRequestLikeAnyOther(t *testing.T) {
	receipt := func(m map[string]any) map[string]any { return m["harness_receipt"].(map[string]any) }
	zero := strings.Repeat("0", 64)
	upstream := func(mutate func(m map[string]any)) harnesstest.Reply {
		return harnesstest.Reply{Kind: harnesstest.KindErrorOutcome, Code: "UPSTREAM_TRANSIENT", Mutate: mutate}
	}
	for _, tc := range []struct {
		name    string
		reply   harnesstest.Reply
		status  string
		code    string
		unknown int64
	}{
		{"the request digest of another request", upstream(func(m map[string]any) { receipt(m)["request_digest"] = zero }), "failed", "MODEL_CONTRACT_FAILED", 0},
		{"the input digest of another request", upstream(func(m map[string]any) { receipt(m)["input_digest"] = zero }), "failed", "MODEL_CONTRACT_FAILED", 0},
		{"another binding", upstream(func(m map[string]any) { receipt(m)["binding_fingerprint"] = "bfp-v1:" + zero }), "failed", "MODEL_CONTRACT_FAILED", 0},
		{"another stage", upstream(func(m map[string]any) { receipt(m)["stage"] = "work_summary" }), "failed", "MODEL_CONTRACT_FAILED", 0},
		{"another recovery profile", upstream(func(m map[string]any) {
			receipt(m)["recovery_profile"], receipt(m)["recovery_profile_revision"] = "terminal_output_once", "x-1"
			receipt(m)["applied_transformations"] = []any{"format_suffix"}
		}), "failed", "MODEL_CONTRACT_FAILED", 0},
		{"a hidden retry", upstream(func(m map[string]any) { receipt(m)["hidden_retry"] = true }), "blocked", "MODEL_CONTRACT_FAILED", 1},
		{"two backend attempts", upstream(func(m map[string]any) { receipt(m)["backend_attempts"] = 2 }), "blocked", "MODEL_GENERATION_OUTCOME_UNKNOWN", 1},
		{"a finished generation that counts none", upstream(func(m map[string]any) { receipt(m)["backend_attempts"] = 0 }), "blocked", "MODEL_GENERATION_OUTCOME_UNKNOWN", 1},
		{"a rate limit after a finished generation", harnesstest.Reply{Kind: harnesstest.KindErrorOutcome, Code: "RATE_LIMITED"}, "blocked", "MODEL_GENERATION_OUTCOME_UNKNOWN", 1},
		{"an unknown outcome stated for a finished generation", harnesstest.Reply{Kind: harnesstest.KindErrorOutcome, Code: "MODEL_GENERATION_OUTCOME_UNKNOWN"}, "blocked", "MODEL_GENERATION_OUTCOME_UNKNOWN", 1},
		{"a connection failure after a finished generation", harnesstest.Reply{Kind: harnesstest.KindErrorOutcome, Code: "CONNECT_FAILED"}, "blocked", "MODEL_GENERATION_OUTCOME_UNKNOWN", 1},
		{"a refusal with another request's digest", harnesstest.Reply{Kind: harnesstest.KindRefusal, Mutate: func(m map[string]any) { receipt(m)["request_digest"] = zero }}, "failed", "MODEL_CONTRACT_FAILED", 0},
		{"an incomplete with another binding", harnesstest.Reply{Kind: harnesstest.KindLength, Text: "cut", Mutate: func(m map[string]any) { receipt(m)["binding_fingerprint"] = "bfp-v1:" + zero }}, "failed", "MODEL_CONTRACT_FAILED", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			fake.SetScript(tc.reply, harnesstest.Final("a retry would have answered"))
			r, _ := modelRig(t, fake)
			info := r.openSession("rcpt.open.0000000000000001")
			run := r.waitTerminal(r.startRun(info.ThreadID, "rcpt.start.00000000000001", "go").RunID)
			res := run.Result
			if res.Status != tc.status || res.Code != tc.code || run.GenerationAttemptsUnknown != tc.unknown || len(fake.Generates()) != 1 || res.FinalText != "" {
				t.Fatalf("%+v unknown=%d generates=%d", res, run.GenerationAttemptsUnknown, len(fake.Generates()))
			}
			if n := len(ofType(r.threadEvents(info.ThreadID), "model.retry_scheduled")); n != 0 {
				t.Fatal("a retry was scheduled on a receipt that cannot be believed")
			}
		})
	}
}
