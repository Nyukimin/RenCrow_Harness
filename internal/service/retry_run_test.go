package service_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The tests of the one retry of an act Action (F31, WP05): which failures are retried,
// with which profile and after how long; that the retry is the same Action's second
// Attempt, counted as an attempt and not as a step; and that a stop, a deadline, an
// unknown generation, a refusal and every other failure outside the table end the Run
// as they always did, with no second generation.

// typesAfter is the types of the events from the first one of type from.
func typesAfter(events []protocol.Event, from string) []string {
	var out []string
	started := false
	for _, e := range events {
		started = started || e.Type == from
		if started {
			out = append(out, e.Type)
		}
	}
	return out
}

func reasoningOnly() harnesstest.Reply {
	return harnesstest.Reply{Kind: harnesstest.KindReasoningOnly, Reasoning: "thinking, and no answer"}
}

func rawMarkup(text string) harnesstest.Reply {
	return harnesstest.Reply{Kind: harnesstest.KindErrorOutcome, Code: "RAW_TOOL_MARKUP", Text: text}
}

func transient() harnesstest.Reply {
	return harnesstest.Reply{Kind: harnesstest.KindErrorOutcome, Code: "UPSTREAM_TRANSIENT"}
}

// refusedBeforeGenerating is a strict error of the code, with the receipt of a generation
// that never started.
func refusedBeforeGenerating(code string) harnesstest.Reply {
	return harnesstest.Reply{Kind: harnesstest.KindStrictError, Code: code}
}

// retryDecisionOf reads the retry receipt Evidence model.retry_scheduled names.
func retryDecisionOf(t *testing.T, r *rig, ev protocol.Event) map[string]any {
	t.Helper()
	if ev.EvidenceID == nil {
		t.Fatal("model.retry_scheduled names no Evidence")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(r.evidence(*ev.EvidenceID)), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestAFirstAttemptThatGaveNoAnswerIsRetriedOnceInTheSameAction is A41 for the success
// of the second Attempt: the same Action has two Attempts, the second is a new request
// that names the first, the context the model sees is the same and the failed output is
// not in it, the retry costs an attempt and not a step, and the events and Evidence say
// so.
func TestAFirstAttemptThatGaveNoAnswerIsRetriedOnceInTheSameAction(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(rawMarkup("<tool_call>broken</tool_call>"), harnesstest.Final("the answer"))
	r, rec := modelRig(t, fake)
	info := r.openSession("retry.open.0000000000001")
	start := r.startRun(info.ThreadID, "retry.start.000000000001", "do it")
	run := r.waitTerminal(start.RunID)
	res := run.Result
	if res.Status != "completed" || res.FinalText != "the answer" || len(res.UnresolvedActionIDs) != 0 {
		t.Fatalf("%+v", res)
	}
	if run.GenerationAttemptsUsed != 2 || run.GenerationAttemptsUnknown != 0 || len(fake.Generates()) != 2 {
		t.Fatalf("used=%d unknown=%d generates=%d", run.GenerationAttemptsUsed, run.GenerationAttemptsUnknown, len(fake.Generates()))
	}
	// One Action, two Attempts of it; the second is ordinal 1 and a retry.
	if r.count("actions") != 1 || r.count("attempts") != 2 || r.count("model_calls") != 2 {
		t.Fatalf("actions=%d attempts=%d model_calls=%d", r.count("actions"), r.count("attempts"), r.count("model_calls"))
	}
	if got := r.val(`SELECT group_concat(a.ordinal||':'||a.state, ',') FROM (SELECT * FROM attempts ORDER BY ordinal) a`); got != "0:failed,1:completed" {
		t.Fatal(got)
	}
	if got := r.val(`SELECT status FROM actions`); got != "completed" {
		t.Fatalf("the Action's status is its last Attempt's: %s", got)
	}
	if r.val(`SELECT current_attempt_id FROM actions`) != r.val(`SELECT attempt_id FROM attempts WHERE ordinal=1`) {
		t.Fatal("the Action does not point at its retry")
	}

	// Each Attempt is counted and then generated: two measures and two generations, one
	// description of the binding.
	if len(fake.Measures()) != 2 || fake.Describes() != 1 {
		t.Fatalf("measures=%d describes=%d", len(fake.Measures()), fake.Describes())
	}
	first, second := fake.Generates()[0], fake.Generates()[1]
	if !slices.EqualFunc(first.Messages, second.Messages, func(a, b modelport.ChatMessage) bool { return a.Role == b.Role && a.Text() == b.Text() }) {
		t.Fatal("the retry is sent another context than the first Attempt")
	}
	rc1, rc2 := first.Rencrow.Harness.Recovery, second.Rencrow.Harness.Recovery
	if rc1.ProfileID != "same_request" || rc1.RetryOfRequestID != nil || rc1.TriggerCode != nil {
		t.Fatalf("%+v", rc1)
	}
	if rc2.ProfileID != "same_request" || rc2.RetryOfRequestID == nil || *rc2.RetryOfRequestID != *first.Rencrow.RequestID || rc2.TriggerCode == nil || *rc2.TriggerCode != "RAW_TOOL_MARKUP" {
		t.Fatalf("%+v", rc2)
	}
	if *second.Rencrow.RequestID == *first.Rencrow.RequestID {
		t.Fatal("the retry is a new request")
	}
	if m := fake.Measures()[1].Request.Rencrow.Harness.Recovery; m.RetryOfRequestID == nil || m.TriggerCode == nil || *m.TriggerCode != "RAW_TOOL_MARKUP" {
		t.Fatalf("the retry was counted as another request than it is sent as: %+v", m)
	}
	// The same request again: the same logical input and the same request digest.
	if first.Rencrow.Harness.ExpectedInputDigest != second.Rencrow.Harness.ExpectedInputDigest || first.Rencrow.Harness.ExpectedRequestDigest != second.Rencrow.Harness.ExpectedRequestDigest {
		t.Fatal("the same request has other digests the second time")
	}

	// The events: the failed Attempt ends, the retry is scheduled, then it is dispatched
	// as a second Attempt of the same Action.
	events := r.threadEvents(info.ThreadID)
	if got := typesAfter(events, "model.completed"); !slices.Equal(got, []string{"model.completed", "model.retry_scheduled", "action.dispatch_started", "model.attempt_started", "model.requested", "model.completed", "run.terminal"}) {
		t.Fatalf("%v", got)
	}
	scheduled := payloadOf[protocol.ModelRetryScheduledPayload](t, ofType(events, "model.retry_scheduled")[0])
	started := ofType(events, "model.attempt_started")
	if len(started) != 2 {
		t.Fatalf("%s", eventTypes(events))
	}
	firstStart, secondStart := payloadOf[protocol.ModelAttemptStartedPayload](t, started[0]), payloadOf[protocol.ModelAttemptStartedPayload](t, started[1])
	if scheduled.ActionID != firstStart.ActionID || scheduled.FailedAttemptID != firstStart.AttemptID || scheduled.NextOrdinal != 1 || scheduled.TriggerCode != "RAW_TOOL_MARKUP" ||
		scheduled.DelayMS != 0 || scheduled.RecoveryProfile != "same_request" || scheduled.RecoveryProfileRevision != "builtin-v1" {
		t.Fatalf("%+v", scheduled)
	}
	if secondStart.ActionID != firstStart.ActionID || secondStart.AttemptID == firstStart.AttemptID || secondStart.Ordinal != 1 || secondStart.GenerationAttemptsUsed != 2 || firstStart.GenerationAttemptsUsed != 1 {
		t.Fatalf("%+v %+v", firstStart, secondStart)
	}
	if delays := r.delays(); len(delays) != 1 || delays[0] != 0 {
		t.Fatalf("a format failure waits for nothing: %v", delays)
	}
	// model.retry_scheduled names the retry receipt, which names the failed Attempt's
	// receipt; terminal_output_once was not used, and why is recorded.
	decision := retryDecisionOf(t, r, ofType(events, "model.retry_scheduled")[0])
	if decision["failed_attempt_id"] != firstStart.AttemptID || decision["trigger_code"] != "RAW_TOOL_MARKUP" || decision["next_ordinal"] != float64(1) ||
		decision["recovery_profile"] != "same_request" || decision["fallback_from"] != "terminal_output_once" || decision["fallback_reason"] != "not_supported_by_binding" ||
		decision["failure_generation_state"] != "terminal" || decision["generation_attempts_used"] != float64(1) {
		t.Fatalf("%v", decision)
	}
	failedReceipt := decision["attempt_receipt_evidence_id"].(string)
	var fr protocol.ModelAttemptReceipt
	if err := json.Unmarshal([]byte(r.evidence(failedReceipt)), &fr); err != nil || fr.AttemptID != firstStart.AttemptID || fr.Ordinal != 0 ||
		fr.FailureCode == nil || *fr.FailureCode != "RAW_TOOL_MARKUP" || fr.GenerationState != "terminal" {
		t.Fatalf("%v %+v", err, fr)
	}
	// The retry's own receipt: ordinal 1, the first Attempt's request digest as its base.
	var sr protocol.ModelAttemptReceipt
	secondReceipt := r.val(`SELECT json_extract(result_json,'$.receipt_evidence_id') FROM attempts WHERE ordinal=1`)
	if err := json.Unmarshal([]byte(r.evidence(secondReceipt)), &sr); err != nil || sr.Ordinal != 1 || sr.AttemptID != secondStart.AttemptID || sr.FailureCode != nil ||
		sr.BaseRequestDigest != fr.RequestDigest || sr.RequestDigest != fr.RequestDigest || sr.RecoveryProfile != "same_request" || len(sr.AppliedTransformations) != 0 ||
		sr.GenerationState != "terminal" || sr.BackendAttempts == nil || *sr.BackendAttempts != 1 {
		t.Fatalf("%v %+v", err, sr)
	}

	// The failed output is kept as Evidence of its Attempt and is not part of the
	// context: the thread holds the input and the answer, nothing of the broken output.
	if r.count("context_entries") != 2 {
		t.Fatalf("%d context entries", r.count("context_entries"))
	}
	if got := r.val(`SELECT COUNT(*) FROM evidence WHERE attempt_id=? AND json_extract(metadata_json,'$.purpose')='model_response'`, firstStart.AttemptID); got != "1" {
		t.Fatalf("the failed Attempt's response is not kept: %s", got)
	}

	// The provisional text of the failed Attempt is discarded when the retry starts, before
	// any text of the retry, and the retry's text is the Attempt's own.
	resets, order := rec.notifications()
	if len(resets) != 1 || resets[0].RunID != start.RunID || resets[0].OldAttemptID != firstStart.AttemptID || resets[0].NewAttemptID != secondStart.AttemptID ||
		resets[0].Reason != "RAW_TOOL_MARKUP" || !resets[0].Provisional {
		t.Fatalf("%+v", resets)
	}
	resetAt, firstOfRetry, lastOfFirst := -1, -1, -1
	for i, n := range order {
		switch {
		case n == "reset:"+secondStart.AttemptID:
			resetAt = i
		case n == "delta:"+secondStart.AttemptID && firstOfRetry < 0:
			firstOfRetry = i
		case n == "delta:"+firstStart.AttemptID:
			lastOfFirst = i
		}
	}
	if resetAt < 0 || firstOfRetry < 0 || lastOfFirst < 0 || !(lastOfFirst < resetAt && resetAt < firstOfRetry) {
		t.Fatalf("the reset is not between the two Attempts' text: %v", order)
	}
}

// TestTheRetryThatFailsTooEndsTheRunAndThereIsNoThirdAttempt is A41 for the failure of
// the second Attempt: two generations at most, the Run ends as the failure classifies,
// the failed bodies never enter the context, and the third reply of the script is never
// asked for.
func TestTheRetryThatFailsTooEndsTheRunAndThereIsNoThirdAttempt(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(reasoningOnly(), reasoningOnly(), harnesstest.Final("a third attempt would have answered"))
	r, _ := modelRig(t, fake)
	info := r.openSession("retry2.open.000000000001")
	run := r.waitTerminal(r.startRun(info.ThreadID, "retry2.start.00000000001", "do it").RunID)
	res := run.Result
	if res.Status != "failed" || res.Code != "MODEL_OUTPUT_INVALID" || !res.Resumable || res.FinalText != "" || len(fake.Generates()) != 2 || run.GenerationAttemptsUsed != 2 {
		t.Fatalf("%+v used=%d", res, run.GenerationAttemptsUsed)
	}
	if r.count("actions") != 1 || r.count("attempts") != 2 || r.count("context_entries") != 1 {
		t.Fatalf("actions=%d attempts=%d context=%d", r.count("actions"), r.count("attempts"), r.count("context_entries"))
	}
	events := r.threadEvents(info.ThreadID)
	if n := len(ofType(events, "model.retry_scheduled")); n != 1 {
		t.Fatalf("%d retries scheduled", n)
	}
	if n := len(ofType(events, "model.completed")); n != 2 {
		t.Fatalf("%d attempts ended", n)
	}
	// The database refuses a third Attempt of an Action on its own account.
	db := openDB(t, r)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO attempts(attempt_id, action_id, ordinal, state, started_at) SELECT 'att_x', action_id, 1, 'prepared', 'x' FROM actions`); err == nil {
		t.Fatal("a second Attempt with ordinal 1 was accepted")
	}
	if _, err := db.Exec(`UPDATE model_calls SET attempt_ordinal=2`); err == nil {
		t.Fatal("a model call of ordinal 2 was accepted")
	}
}

// TestTerminalOutputOnceIsTheRetryOfAFormatFailureWhenEveryoneOffersIt is A42 for the
// allowed and supported case: the retry carries the profile at the binding's revision,
// is counted again with it, keeps the binding, and the receipt states what was applied.
func TestTerminalOutputOnceIsTheRetryOfAFormatFailureWhenEveryoneOffersIt(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.OfferTerminalOutputOnce = true
	fake.SetScript(reasoningOnly(), harnesstest.Final("now in the right form"))
	r, _ := modelRig(t, fake)
	info := r.openSession("once.open.0000000000000001")
	run := r.waitTerminal(r.startRun(info.ThreadID, "once.start.00000000000001", "do it").RunID)
	if run.Result.Status != "completed" || run.Result.FinalText != "now in the right form" || len(fake.Generates()) != 2 {
		t.Fatalf("%+v", run.Result)
	}
	first, second := fake.Generates()[0], fake.Generates()[1]
	rc := second.Rencrow.Harness.Recovery
	if rc.ProfileID != "terminal_output_once" || rc.ProfileRevision != harnesstest.TerminalOutputOnceRevision || rc.RetryOfRequestID == nil || rc.TriggerCode == nil || *rc.TriggerCode != "REASONING_ONLY" {
		t.Fatalf("%+v", rc)
	}
	// The profile is part of the logical input, so the digests are the profile's own; the
	// binding is the same one, counted again with the profile before it was sent.
	h1, h2 := first.Rencrow.Harness, second.Rencrow.Harness
	if h1.ExpectedInputDigest == h2.ExpectedInputDigest || h1.ExpectedRequestDigest == h2.ExpectedRequestDigest || h1.ExpectedBindingFingerprint != h2.ExpectedBindingFingerprint {
		t.Fatalf("%+v %+v", h1, h2)
	}
	if len(fake.Measures()) != 2 || fake.Measures()[1].Request.Rencrow.Harness.Recovery.ProfileID != "terminal_output_once" {
		t.Fatal("the retry was not counted with the profile it is sent with")
	}
	events := r.threadEvents(info.ThreadID)
	scheduled := ofType(events, "model.retry_scheduled")[0]
	p := payloadOf[protocol.ModelRetryScheduledPayload](t, scheduled)
	if p.RecoveryProfile != "terminal_output_once" || p.RecoveryProfileRevision != harnesstest.TerminalOutputOnceRevision || p.DelayMS != 0 || p.TriggerCode != "REASONING_ONLY" {
		t.Fatalf("%+v", p)
	}
	if d := retryDecisionOf(t, r, scheduled); d["fallback_from"] != nil || d["fallback_reason"] != nil || d["recovery_profile"] != "terminal_output_once" {
		t.Fatalf("%v", d)
	}
	// The receipt of the retry: the profile, what was applied, the first Attempt's request
	// as its base, and a request digest that is the profile's.
	var sr, fr protocol.ModelAttemptReceipt
	if err := json.Unmarshal([]byte(r.evidence(r.val(`SELECT json_extract(result_json,'$.receipt_evidence_id') FROM attempts WHERE ordinal=1`))), &sr); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(r.evidence(r.val(`SELECT json_extract(result_json,'$.receipt_evidence_id') FROM attempts WHERE ordinal=0`))), &fr); err != nil {
		t.Fatal(err)
	}
	if sr.RecoveryProfile != "terminal_output_once" || sr.RecoveryProfileRevision != harnesstest.TerminalOutputOnceRevision || !slices.Equal(sr.AppliedTransformations, []string{"format_suffix"}) ||
		sr.BaseRequestDigest != fr.RequestDigest || sr.RequestDigest == fr.RequestDigest || sr.BindingFingerprint != fr.BindingFingerprint || sr.InputDigest == fr.InputDigest {
		t.Fatalf("%+v\n%+v", sr, fr)
	}
	if got := r.val(`SELECT applied_transformations_json FROM model_calls WHERE attempt_ordinal=1`); got != `["format_suffix"]` {
		t.Fatal(got)
	}
	if got := r.val(`SELECT recovery_profile||'/'||recovery_profile_revision FROM model_calls WHERE attempt_ordinal=1`); got != "terminal_output_once/"+harnesstest.TerminalOutputOnceRevision {
		t.Fatal(got)
	}
}

// TestWhichProfileTheRetryUsesAndWhyTheOtherWasNot is A42 for the profiles that are not
// used: not allowed by the Run's frozen policy, not published by the binding, not
// answering the code; the same request is the retry and the reason is recorded. A
// transient failure never takes terminal_output_once, whatever is offered.
func TestWhichProfileTheRetryUsesAndWhyTheOtherWasNot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fake    func(f *harnesstest.Fake)
		edit    func(l *harnesstest.Layout)
		first   harnesstest.Reply
		profile string
		reason  any
	}{
		{"the policy does not allow it", func(f *harnesstest.Fake) { f.OfferTerminalOutputOnce = true },
			func(l *harnesstest.Layout) {
				l.Cfg["recovery"].(map[string]any)["allowed_profiles"] = []any{"same_request"}
			}, reasoningOnly(), "same_request", "not_allowed_by_policy"},
		{"the binding does not publish it", func(f *harnesstest.Fake) {}, nil, reasoningOnly(), "same_request", "not_supported_by_binding"},
		{"the binding publishes it for other codes", func(f *harnesstest.Fake) {
			f.OfferTerminalOutputOnce = true
			f.Profiles = []modelport.RecoveryProfile{
				{ProfileID: "same_request", ProfileRevision: "builtin-v1", Stages: []string{"act"}, Transformations: []string{}, Codes: []string{"REASONING_ONLY", "RAW_TOOL_MARKUP"}},
				{ProfileID: "terminal_output_once", ProfileRevision: harnesstest.TerminalOutputOnceRevision, Stages: []string{"act"}, Transformations: []string{"format_suffix"}, Codes: []string{"RAW_TOOL_MARKUP"}},
			}
		}, nil, reasoningOnly(), "same_request", "code_not_covered_by_profile"},
		{"offered, but the failure is a transient one", func(f *harnesstest.Fake) { f.OfferTerminalOutputOnce = true }, nil, transient(), "same_request", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			tc.fake(fake)
			fake.SetScript(tc.first, harnesstest.Final("ok"))
			r := newModelRig(t, fake, nil, tc.edit)
			r.conn = r.svc.NewConn(&recorder{})
			info := r.openSession("prof.open.00000000000001")
			run := r.waitTerminal(r.startRun(info.ThreadID, "prof.start.0000000000001", "go").RunID)
			if run.Result.Status != "completed" || len(fake.Generates()) != 2 {
				t.Fatalf("%+v", run.Result)
			}
			if rc := fake.Generates()[1].Rencrow.Harness.Recovery; rc.ProfileID != tc.profile {
				t.Fatalf("%+v", rc)
			}
			d := retryDecisionOf(t, r, ofType(r.threadEvents(info.ThreadID), "model.retry_scheduled")[0])
			if d["fallback_reason"] != tc.reason {
				t.Fatalf("%v", d)
			}
			if tc.reason != nil && d["fallback_from"] != "terminal_output_once" {
				t.Fatalf("%v", d)
			}
		})
	}
}

// TestAReceiptThatStatesAChangeNobodyAuthorizedIsAContractFailure is A42's other half:
// only the transformations the profile lists may be applied, and the same request applies
// none.
func TestAReceiptThatStatesAChangeNobodyAuthorizedIsAContractFailure(t *testing.T) {
	t.Run("the same request applied a transformation: not even a valid receipt", func(t *testing.T) {
		// The wire schema already forbids it (same_request has no transformations), so the
		// terminal frame is not a frame the Harness reads: its generation is unknown.
		fake := harnesstest.NewFake()
		fake.SetScript(reasoningOnly(), harnesstest.Reply{Kind: harnesstest.KindFinal, Text: "answer", Mutate: func(m map[string]any) {
			m["harness_receipt"].(map[string]any)["applied_transformations"] = []any{"format_suffix"}
		}})
		r, _ := modelRig(t, fake)
		info := r.openSession("auth.open.00000000000001")
		run := r.waitTerminal(r.startRun(info.ThreadID, "auth.start.0000000000001", "go").RunID)
		res := run.Result
		if res.Status != "blocked" || res.Code != "MODEL_GENERATION_OUTCOME_UNKNOWN" || res.FinalText != "" || len(fake.Generates()) != 2 || r.count("context_entries") != 1 {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("terminal_output_once applied a change it does not list", func(t *testing.T) {
		fake := harnesstest.NewFake()
		fake.OfferTerminalOutputOnce = true
		fake.Applied = []string{"thinking_disabled"}
		fake.SetScript(reasoningOnly(), harnesstest.Final("answer"))
		r, _ := modelRig(t, fake)
		info := r.openSession("auth2.open.0000000000001")
		res := r.waitTerminal(r.startRun(info.ThreadID, "auth2.start.000000000001", "go").RunID).Result
		if res.Status != "failed" || res.Code != "MODEL_CONTRACT_FAILED" || res.FinalText != "" || len(fake.Generates()) != 2 {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("a refusal before generating that applied one", func(t *testing.T) {
		fake := harnesstest.NewFake()
		fake.SetReply(harnesstest.Reply{Kind: harnesstest.KindStrictError, Code: "AUTH_FAILED"})
		fake.Applied = []string{"format_suffix"} // not used for the first request's profile
		r, _ := modelRig(t, fake)
		info := r.openSession("auth3.open.0000000000001")
		res := r.waitTerminal(r.startRun(info.ThreadID, "auth3.start.000000000001", "go").RunID).Result
		if res.Status != "blocked" || res.Code != "AUTH_FAILED" || len(fake.Generates()) != 1 {
			t.Fatalf("%+v", res)
		}
	})
}

// TestTheBackoffsOfTheTransientFailures is the wait of the table: nothing for a format
// failure, one second for a connection or an upstream failure, one second at least for a
// rate limit or a full queue. The delay the decision recorded is the delay waited.
func TestTheBackoffsOfTheTransientFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		first   harnesstest.Reply
		delay   time.Duration
		trigger string
	}{
		{"a format failure", reasoningOnly(), 0, "REASONING_ONLY"},
		{"a connection that could not be made", refusedBeforeGenerating("CONNECT_FAILED"), time.Second, "CONNECT_FAILED"},
		{"an upstream failure of a finished generation", transient(), time.Second, "UPSTREAM_TRANSIENT"},
		{"an upstream failure stated as a refusal", harnesstest.Reply{Kind: harnesstest.KindStrictError, Code: "UPSTREAM_TRANSIENT", State: "terminal"}, time.Second, "UPSTREAM_TRANSIENT"},
		{"rate limited", refusedBeforeGenerating("RATE_LIMITED"), time.Second, "RATE_LIMITED"},
		{"a queue that timed out", refusedBeforeGenerating("QUEUE_TIMEOUT"), time.Second, "QUEUE_TIMEOUT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			fake.SetScript(tc.first, harnesstest.Final("fine"))
			r, _ := modelRig(t, fake)
			info := r.openSession("backoff.open.000000000001")
			run := r.waitTerminal(r.startRun(info.ThreadID, "backoff.start.00000000001", "go").RunID)
			if run.Result.Status != "completed" || len(fake.Generates()) != 2 || run.GenerationAttemptsUsed != 2 {
				t.Fatalf("%+v", run.Result)
			}
			if delays := r.delays(); len(delays) != 1 || delays[0] != tc.delay {
				t.Fatalf("%v", delays)
			}
			p := payloadOf[protocol.ModelRetryScheduledPayload](t, ofType(r.threadEvents(info.ThreadID), "model.retry_scheduled")[0])
			if p.TriggerCode != tc.trigger || p.DelayMS != tc.delay.Milliseconds() || p.RecoveryProfile != "same_request" {
				t.Fatalf("%+v", p)
			}
			if fake.Generates()[1].Rencrow.Harness.Recovery.ProfileID != "same_request" {
				t.Fatal("a transient failure is retried as the same request")
			}
		})
	}
}

// TestFailuresOutsideTheTableAreNeverRetried: unknown outcomes, refusals, truncation,
// the context limit, a degenerate output and every contract or binding failure end the
// Run on the first generation, with no retry scheduled and no second request.
func TestFailuresOutsideTheTableAreNeverRetried(t *testing.T) {
	cases := []struct {
		name   string
		reply  harnesstest.Reply
		status string
		code   string
	}{
		{"a refusal", harnesstest.Reply{Kind: harnesstest.KindRefusal}, "rejected", "MODEL_REFUSED"},
		{"truncation", harnesstest.Reply{Kind: harnesstest.KindLength, Text: "cut"}, "incomplete", "MODEL_OUTPUT_TRUNCATED"},
		{"a degenerate output", harnesstest.Reply{Kind: harnesstest.KindErrorOutcome, Code: "MODEL_OUTPUT_DEGENERATE"}, "failed", "MODEL_OUTPUT_DEGENERATE"},
		{"an upstream failure whose generation is unknown", harnesstest.Reply{Kind: harnesstest.KindStrictError, Code: "UPSTREAM_TRANSIENT", State: "unknown"}, "blocked", "MODEL_GENERATION_OUTCOME_UNKNOWN"},
		{"a connection that broke without a word", harnesstest.Reply{Kind: harnesstest.KindTransportError}, "blocked", "MODEL_GENERATION_OUTCOME_UNKNOWN"},
		{"a stream that ended before its terminal frame", harnesstest.Reply{Kind: harnesstest.KindNoTerminal, Text: "part"}, "blocked", "MODEL_GENERATION_OUTCOME_UNKNOWN"},
		{"a hidden retry", harnesstest.Reply{Kind: harnesstest.KindFinal, Text: "x", Mutate: func(m map[string]any) { m["harness_receipt"].(map[string]any)["hidden_retry"] = true }}, "blocked", "MODEL_CONTRACT_FAILED"},
		{"the context limit", refusedBeforeGenerating("CONTEXT_LIMIT_EXCEEDED"), "blocked", "CAPACITY_BLOCKED"},
		{"an unsupported contract", refusedBeforeGenerating("UNSUPPORTED_CONTRACT"), "blocked", "UNSUPPORTED_CONTRACT"},
		{"authentication", refusedBeforeGenerating("AUTH_FAILED"), "blocked", "AUTH_FAILED"},
		{"a binding that changed", refusedBeforeGenerating("BINDING_CHANGED"), "blocked", "BINDING_CHANGED"},
		{"a recovery profile the model side does not support", refusedBeforeGenerating("UNSUPPORTED_RECOVERY_PROFILE"), "blocked", "UNSUPPORTED_RECOVERY_PROFILE"},
		{"a model that is not alive", refusedBeforeGenerating("MODEL_UNAVAILABLE"), "blocked", "MODEL_UNAVAILABLE"},
		{"a rate limit that claims a finished generation", harnesstest.Reply{Kind: harnesstest.KindErrorOutcome, Code: "RATE_LIMITED"}, "blocked", "MODEL_GENERATION_OUTCOME_UNKNOWN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			fake.SetScript(tc.reply, harnesstest.Final("a second generation would have answered"))
			r, _ := modelRig(t, fake)
			info := r.openSession("never.open.000000000001")
			run := r.waitTerminal(r.startRun(info.ThreadID, "never.start.00000000001", "go").RunID)
			res := run.Result
			if res.Status != tc.status || res.Code != tc.code || len(fake.Generates()) != 1 || run.GenerationAttemptsUsed != 1 || len(fake.Measures()) != 1 || res.FinalText != "" {
				t.Fatalf("%+v generates=%d", res, len(fake.Generates()))
			}
			events := r.threadEvents(info.ThreadID)
			if n := len(ofType(events, "model.retry_scheduled")); n != 0 {
				t.Fatalf("a retry was scheduled")
			}
			if r.count("attempts") != 1 || r.count("model_calls") != 1 || len(r.delays()) != 0 {
				t.Fatalf("attempts=%d delays=%v", r.count("attempts"), r.delays())
			}
		})
	}
}

// TestAStopWhileTheRetryWaitsEndsTheRunAtOnceAndNoSecondAttemptIsMade is A43 for the
// cancel in the backoff: the Run is in RetryWaiting, the stop is recorded, the Run ends
// cancelled at once (not after the backoff), the failed Attempt is not shown as unknown,
// and no second Attempt, request or reset ever exists.
func TestAStopWhileTheRetryWaitsEndsTheRunAndNoSecondAttemptIsMade(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(transient(), harnesstest.Final("never asked for"))
	r, rec := modelRig(t, fake)
	waiting := make(chan struct{}, 1)
	r.waitHook = func(ctx context.Context, d time.Duration) error {
		if d != time.Second {
			t.Errorf("the backoff of a transient failure is one second, got %v", d)
		}
		waiting <- struct{}{}
		<-ctx.Done() // only a stop of the Run ends this wait, never the backoff itself
		return ctx.Err()
	}
	info := r.openSession("stopwait.open.0000000001")
	start := r.startRun(info.ThreadID, "stopwait.start.000000001", "go")
	<-waiting
	r.waitFor("RetryWaiting", func() bool {
		return decode[protocol.RunInfo](t, r.mustCall("run/get", protocol.RunGetInput{RunID: start.RunID})).Phase == "RetryWaiting"
	})
	begin := time.Now()
	if receipt := r.interrupt(start.RunID, 0, "stopwait.key.0000000000001"); !receipt.SignalRecorded || receipt.Code != "CANCEL_REQUESTED" {
		t.Fatalf("%+v", receipt)
	}
	run := r.waitTerminal(start.RunID)
	if time.Since(begin) > 5*time.Second {
		t.Fatalf("the stop waited for the backoff: %v", time.Since(begin))
	}
	res := run.Result
	if res.Status != "cancelled" || res.Code != "CANCELLED" || !res.Resumable || len(res.UnresolvedActionIDs) != 0 || len(fake.Generates()) != 1 || len(fake.Measures()) != 1 {
		t.Fatalf("%+v", res)
	}
	if run.GenerationAttemptsUsed != 1 || run.GenerationAttemptsUnknown != 0 || r.count("attempts") != 1 || r.count("model_calls") != 1 {
		t.Fatalf("used=%d unknown=%d attempts=%d", run.GenerationAttemptsUsed, run.GenerationAttemptsUnknown, r.count("attempts"))
	}
	events := r.threadEvents(info.ThreadID)
	if n := len(ofType(events, "model.retry_scheduled")); n != 1 {
		t.Fatalf("%d retries scheduled", n)
	}
	if n := len(ofType(events, "model.attempt_started")); n != 1 {
		t.Fatalf("%d attempts started: a second Attempt was made", n)
	}
	if got := r.val(`SELECT state FROM attempts`); got != "failed" {
		t.Fatalf("the failed Attempt is %s", got)
	}
	if resets, _ := rec.notifications(); len(resets) != 0 {
		t.Fatalf("a reset without a retry: %+v", resets)
	}
}

// TestAStopRecordedByAnotherProcessEndsTheBackoffToo: the same, when the stop was
// recorded by a process that is not this one and is found by the control check.
func TestAStopRecordedByAnotherProcessEndsTheBackoffToo(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(transient(), harnesstest.Final("never asked for"))
	r, _ := modelRig(t, fake)
	r.withControlPoll(20 * time.Millisecond)
	waiting := make(chan struct{}, 1)
	r.waitHook = func(ctx context.Context, d time.Duration) error {
		waiting <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	info := r.openSession("otherstop.open.000000001")
	start := r.startRun(info.ThreadID, "otherstop.start.00000001", "go")
	<-waiting
	other, conn := r.otherProcess()
	res := mustHandle(t, other, conn, "turn/interrupt", protocol.InterruptInput{RunID: start.RunID, ExpectedControlRevision: 0, IdempotencyKey: "otherstop.key.0000000000001"})
	if rc := decode[protocol.InterruptReceipt](t, res); !rc.SignalRecorded {
		t.Fatalf("%+v", rc)
	}
	run := r.waitTerminal(start.RunID)
	if run.Result.Status != "cancelled" || len(fake.Generates()) != 1 || r.count("attempts") != 1 {
		t.Fatalf("%+v", run.Result)
	}
}

// TestADeadlineThatPassesWhileTheRetryWaitsEndsTheRun is A44 for a wait the deadline
// overtakes: a Run that is still waiting when its time is up ends as late, with the
// failed Attempt as it was and nothing sent.
func TestADeadlineThatPassesWhileTheRetryWaitsEndsTheRun(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(reasoningOnly(), harnesstest.Final("never asked for"))
	r, _ := modelRig(t, fake)
	r.waitHook = func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }
	info := r.openSession("late.open.00000000000001x")
	start := r.startRun(info.ThreadID, "late.start.0000000000001x", "go", func(in *protocol.StartInput) { in.Limits.DeadlineSeconds = 1 })
	run := r.waitTerminal(start.RunID)
	res := run.Result
	if res.Status != "incomplete" || res.Code != "DEADLINE_EXCEEDED" || !res.Resumable || len(res.UnresolvedActionIDs) != 0 || len(fake.Generates()) != 1 || r.count("attempts") != 1 {
		t.Fatalf("%+v", res)
	}
}

// TestABackoffThatWouldReachTheDeadlineIsNotScheduled: one second of backoff against a
// Run with one second to live is not waited for; the failure ends the Run as it
// classifies, with no retry scheduled.
func TestABackoffThatWouldReachTheDeadlineIsNotScheduled(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(transient(), harnesstest.Final("never asked for"))
	r, _ := modelRig(t, fake)
	info := r.openSession("short.open.0000000000001x")
	run := r.waitTerminal(r.startRun(info.ThreadID, "short.start.000000000001x", "go", func(in *protocol.StartInput) { in.Limits.DeadlineSeconds = 1 }).RunID)
	res := run.Result
	if res.Status != "failed" || res.Code != "MODEL_TRANSPORT_FAILED" || len(fake.Generates()) != 1 || len(r.delays()) != 0 {
		t.Fatalf("%+v", res)
	}
	if n := len(ofType(r.threadEvents(info.ThreadID), "model.retry_scheduled")); n != 0 {
		t.Fatal("a retry was scheduled")
	}
}

// TestABindingThatChangesBetweenTheAttemptsIsNotSentOn is A43 for another binding: the
// retry is counted again, the count is for a binding that is not the one that was
// described, and nothing is sent on it.
func TestABindingThatChangesBetweenTheAttemptsIsNotSentOn(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetScript(reasoningOnly(), harnesstest.Final("never asked for"))
	var counts atomic.Int32
	fake.MeasureHook = func(_ modelport.MeasureRequest, res *modelport.MeasureResult) error {
		if counts.Add(1) == 2 { // the count of the retry is for another binding than the one described
			res.BindingFingerprint = "bfp-v1:" + strings.Repeat("d", 64)
		}
		return nil
	}
	r, _ := modelRig(t, fake)
	info := r.openSession("bind.open.0000000000000001")
	run := r.waitTerminal(r.startRun(info.ThreadID, "bind.start.00000000000001", "go").RunID)
	res := run.Result
	if res.Status != "blocked" || res.Code != "BINDING_CHANGED" || len(fake.Generates()) != 1 || len(fake.Measures()) != 2 || r.count("attempts") != 1 {
		t.Fatalf("%+v measures=%d", res, len(fake.Measures()))
	}
	events := r.threadEvents(info.ThreadID)
	if n := len(ofType(events, "model.retry_scheduled")); n != 1 {
		t.Fatalf("the retry was scheduled and then refused: %d", n)
	}
}

// TestARetryNeedsNoStepButNeedsAnAttemptOfTheBudget is A44 at the Run level: the last
// model step is retried (a retry is the same step), the last attempt of the budget is
// retried, and a budget that ends with the first Attempt is not, and then the Run ends as
// the failure classifies, not as a spent budget; no new Action is made to get around it.
func TestARetryNeedsNoStepButNeedsAnAttemptOfTheBudget(t *testing.T) {
	t.Run("the only model step is retried", func(t *testing.T) {
		fake := harnesstest.NewFake()
		fake.SetScript(reasoningOnly(), harnesstest.Final("done"))
		r, _ := modelRig(t, fake)
		info := r.openSession("step.open.00000000000001x")
		run := r.waitTerminal(r.startRun(info.ThreadID, "step.start.0000000000001x", "go", func(in *protocol.StartInput) { in.Limits.MaxModelSteps = 1 }).RunID)
		if run.Result.Status != "completed" || run.GenerationAttemptsUsed != 2 || r.count("actions") != 1 {
			t.Fatalf("%+v", run.Result)
		}
	})
	t.Run("the last attempt of the budget is retried", func(t *testing.T) {
		fake := harnesstest.NewFake()
		fake.SetScript(reasoningOnly(), harnesstest.Final("done"))
		r, _ := modelRig(t, fake)
		info := r.openSession("cap2.open.000000000000001")
		run := r.waitTerminal(r.startRun(info.ThreadID, "cap2.start.00000000000001", "go", func(in *protocol.StartInput) { in.Limits.MaxGenerationAttempts = 2 }).RunID)
		if run.Result.Status != "completed" || run.GenerationAttemptsUsed != 2 {
			t.Fatalf("%+v", run.Result)
		}
	})
	t.Run("a budget of one attempt is not retried and the failure ends the Run", func(t *testing.T) {
		fake := harnesstest.NewFake()
		fake.SetScript(reasoningOnly(), harnesstest.Final("never asked for"))
		r, _ := modelRig(t, fake)
		info := r.openSession("cap1.open.000000000000001")
		run := r.waitTerminal(r.startRun(info.ThreadID, "cap1.start.00000000000001", "go", func(in *protocol.StartInput) { in.Limits.MaxGenerationAttempts = 1 }).RunID)
		res := run.Result
		if res.Status != "failed" || res.Code != "MODEL_OUTPUT_INVALID" || len(fake.Generates()) != 1 || r.count("actions") != 1 || run.GenerationAttemptsUsed != 1 {
			t.Fatalf("%+v", res)
		}
		if n := len(ofType(r.threadEvents(info.ThreadID), "model.retry_scheduled")); n != 0 {
			t.Fatal("a retry was scheduled")
		}
	})
	t.Run("the retry of a later step counts that step's attempts, and its first Attempt is a new Action's", func(t *testing.T) {
		fake := harnesstest.NewFake()
		r, _ := toolRig(t, fake, toolConfig{})
		r.write("demo.txt", "demo content")
		fake.SetScript(calls(tc("c1", "file.read", readArgs("demo.txt", 0, 100, 100))), reasoningOnly(), harnesstest.Final("read it"))
		info := r.openSession("later.open.0000000000000001")
		run := r.waitTerminal(r.startRun(info.ThreadID, "later.start.000000000000001", "go", func(in *protocol.StartInput) { in.Limits.MaxModelSteps = 2 }).RunID)
		if run.Result.Status != "completed" || run.Result.FinalText != "read it" || len(fake.Generates()) != 3 || run.GenerationAttemptsUsed != 3 {
			t.Fatalf("%+v", run.Result)
		}
		// Two model Actions (the two steps), the second with two Attempts, and one Tool.
		if got := r.val(`SELECT COUNT(*) FROM actions WHERE kind='model'`); got != "2" {
			t.Fatalf("%s model actions", got)
		}
		if got := r.val(`SELECT group_concat(n, ',') FROM (SELECT COUNT(*) n FROM attempts a JOIN actions ac ON ac.action_id=a.action_id WHERE ac.kind='model' GROUP BY ac.action_id ORDER BY MIN(ac.created_at), ac.action_id)`); got != "1,2" && got != "2,1" {
			t.Fatalf("attempts per model action: %s", got)
		}
	})
}

// TestAFailedFirstResponseWithToolCallsRunsNone is A41's note for a completion that
// carries Tool intents: the response that did not satisfy the Tools is retried as a
// whole, and the file its first call would have made does not exist; only the retry's
// own calls run.
func TestAFailedFirstResponseWithToolCallsRunsNone(t *testing.T) {
	fake := harnesstest.NewFake()
	r, _ := toolRig(t, fake, toolConfig{})
	fake.SetScript(
		calls(tc("c1", "file.create", createArgs("never.txt", "x")), tc("c2", "file.delete", `{"path":"x"}`)),
		calls(tc("c3", "file.create", createArgs("made.txt", "y"))),
		harnesstest.Final("done"))
	info := r.openSession("tools.open.00000000000001")
	run := r.waitTerminal(r.startRun(info.ThreadID, "tools.start.0000000000001", "go").RunID)
	if run.Result.Status != "completed" || len(fake.Generates()) != 3 {
		t.Fatalf("%+v generates=%d", run.Result, len(fake.Generates()))
	}
	if _, ok := r.read("never.txt"); ok {
		t.Fatal("a call of the failed response ran")
	}
	if text, ok := r.read("made.txt"); !ok || text != "y" {
		t.Fatalf("the retry's call did not run: %q", text)
	}
	if got := r.val(`SELECT COUNT(*) FROM tool_links`); got != "1" {
		t.Fatalf("%s tool links", got)
	}
}

// TestAResponseBodyOfARefusalIsKeptAsEvidenceWithinItsBound: what the model side said when
// it refused, as it said it, is the Attempt's response Evidence (a long body only as
// far as the bound, shown as an incomplete capture).
func TestAResponseBodyOfARefusalIsKeptAsEvidenceWithinItsBound(t *testing.T) {
	body := `{"error":{"code":"AUTH_FAILED","message":"refused","retryable":false,"request_id":null,"source_code":null}}`
	for _, tc := range []struct {
		name      string
		body      string
		truncated bool
		media     string
	}{
		{"a whole body", body, false, "application/json"},
		// A body that is cut is not a JSON document any more, and is labelled as the text it is.
		{"a body cut at the bound", `{"error":"` + strings.Repeat("x", modelport.MaxFailureBodyBytes-10), true, "text/plain; charset=utf-8"},
		{"a page that is not JSON", "<html>bad gateway</html>", false, "text/plain; charset=utf-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			fake.SetReply(harnesstest.Reply{Kind: harnesstest.KindStrictError, Code: "AUTH_FAILED", Body: tc.body, BodyTruncated: tc.truncated})
			r, _ := modelRig(t, fake)
			info := r.openSession("body.open.0000000000000001")
			run := r.waitTerminal(r.startRun(info.ThreadID, "body.start.00000000000001", "go").RunID)
			if run.Result.Status != "blocked" || run.Result.Code != "AUTH_FAILED" {
				t.Fatalf("%+v", run.Result)
			}
			id := r.val(`SELECT evidence_id FROM evidence WHERE json_extract(metadata_json,'$.purpose')='model_response'`)
			if id == "" {
				t.Fatal("the refusal's body was not kept")
			}
			if got := r.evidence(id); got != tc.body {
				t.Fatalf("the body was changed on its way into Evidence (%d bytes)", len(got))
			}
			complete := "1"
			if tc.truncated {
				complete = "0"
			}
			if got := r.val(`SELECT media_type||'/'||capture_complete||'/'||total_bytes FROM evidence WHERE evidence_id=?`, id); got != tc.media+"/"+complete+"/"+itoa(len(tc.body)) {
				t.Fatal(got)
			}
			// The Attempt's receipt names it.
			var rc protocol.ModelAttemptReceipt
			if err := json.Unmarshal([]byte(r.evidence(r.val(`SELECT json_extract(result_json,'$.receipt_evidence_id') FROM attempts`))), &rc); err != nil || rc.ResponseEvidenceID == nil || *rc.ResponseEvidenceID != id || rc.ResponseID == nil {
				t.Fatalf("%v %+v", err, rc)
			}
		})
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// rawStream is a response made of a non-stream answer: it is read as the strict stream
// and keeps the answer it was made of.
type rawStream struct {
	*strings.Reader
	raw []byte
}

func (s *rawStream) Close() error    { return nil }
func (s *rawStream) RawBody() []byte { return s.raw }

type rawPort struct {
	*harnesstest.Fake
	raw []byte
}

func (p *rawPort) Generate(ctx context.Context, req modelport.ChatRequest) (io.ReadCloser, error) {
	rc, err := p.Fake.Generate(ctx, req)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := rc.Read(buf)
		b.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	_ = rc.Close()
	return &rawStream{Reader: strings.NewReader(b.String()), raw: p.raw}, nil
}

// TestANonStreamAnswerIsKeptAsTheModelSideSentItAndNotAsTheStreamMadeOfIt: the
// response Evidence of an Attempt whose response was made of a non-stream answer is that
// answer.
func TestANonStreamAnswerIsKeptAsTheModelSideSentItAndNotAsTheStreamMadeOfIt(t *testing.T) {
	raw := []byte(`{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Final("hello"))
	port := &rawPort{Fake: fake, raw: raw}
	r := newModelRig(t, port, nil, nil)
	r.conn = r.svc.NewConn(&recorder{})
	info := r.openSession("raw.open.000000000000001")
	run := r.waitTerminal(r.startRun(info.ThreadID, "raw.start.0000000000001", "go").RunID)
	if run.Result.Status != "completed" || run.Result.FinalText != "hello" {
		t.Fatalf("%+v", run.Result)
	}
	id := r.val(`SELECT evidence_id FROM evidence WHERE json_extract(metadata_json,'$.purpose')='model_response'`)
	if got := r.evidence(id); got != string(raw) {
		t.Fatalf("%q", got)
	}
	if got := r.val(`SELECT media_type||'/'||capture_complete FROM evidence WHERE evidence_id=?`, id); got != "application/json/1" {
		t.Fatal(got)
	}
}

// openDB opens the Run's database for writing, to try what its own constraints refuse.
func openDB(t *testing.T, r *rig) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+r.dep.DataRoot+"/"+sqlite.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// TestCapabilitiesSayWhetherAProcessExecutesItsRuns: a process with a model port says its
// runs are executed and generate (declared, with the Gateway's own reachability left to the
// run that needs it), one without says it admits and does not execute; compaction of a
// run's prompt follows the same, and so do the method that compacts on demand and the one
// that forks (both count through the model port).
func TestCapabilitiesSayWhetherAProcessExecutesItsRuns(t *testing.T) {
	with, _ := modelRig(t, harnesstest.NewFake())
	without := newRig(t, nil)
	for _, tc := range []struct {
		name   string
		r      *rig
		status string
	}{{"with a model port", with, "ready"}, {"without one", without, "unavailable"}} {
		t.Run(tc.name, func(t *testing.T) {
			tc.r.initialize()
			byName := map[string]protocol.Capability{}
			for _, c := range decode[protocol.CapabilitiesResult](t, tc.r.mustCall("service/capabilities", protocol.EmptyInput{})).Capabilities {
				byName[c.Name] = c
			}
			for _, n := range []string{"turn.execution", "model.generation"} {
				c := byName[n]
				if c.Status != tc.status || c.Basis != "declared" || c.Reason == nil || *c.Reason == "" {
					t.Fatalf("%s: %+v", n, c)
				}
			}
			if c := byName["context.compaction"]; c.Status != tc.status || c.Basis != "declared" || c.Reason == nil || *c.Reason == "" {
				t.Fatalf("a process that executes its runs compacts them, and one that does not, does not: %+v", c)
			}
			if c := byName["context/compact"]; c.Status != tc.status || c.Basis != "declared" || c.Reason == nil || *c.Reason == "" {
				t.Fatalf("compaction on demand needs what compaction needs (a model port and the deployment's switch): %+v", c)
			}
			if c := byName["session/fork"]; c.Status != tc.status || c.Basis != "declared" || c.Reason == nil || *c.Reason == "" {
				t.Fatalf("a fork needs a model port to count the copied context: %+v", c)
			}
			if tc.status == "ready" {
				if r := *byName["model.generation"].Reason; !strings.Contains(r, "not when the process starts") {
					t.Fatalf("it must not claim the Gateway was reached: %s", r)
				}
			}
		})
	}
}
