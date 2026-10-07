package sqlite

import (
	"errors"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
)

// failedFirstAttempt takes a Run to the point a retry is scheduled from: its first
// Attempt ended as a failure of a finished generation, and the Run waits in RetryWaiting.
func (r *run) failedFirstAttempt() Reservation {
	r.e.t.Helper()
	r.goTo("Generating")
	rsv := r.reserve()
	code := "REASONING_ONLY"
	if _, err := r.e.s.RecordAttemptEnd(bg, r.fence, AttemptEnd{
		AttemptID: rsv.AttemptID, State: "failed", Outcome: "error", GenerationState: "terminal", BackendAttempts: one(1), FailureCode: &code,
	}); err != nil {
		r.e.t.Fatal(err)
	}
	r.phase("Generating", "RetryWaiting")
	return rsv
}

func (r *run) schedule(failed string) RetryScheduled {
	r.e.t.Helper()
	out, err := r.e.s.ScheduleRetry(bg, r.fence, retrySchedule(failed))
	if err != nil {
		r.e.t.Fatal(err)
	}
	return out
}

func retrySchedule(failed string) RetrySchedule {
	return RetrySchedule{FailedAttemptID: failed, TriggerCode: "REASONING_ONLY", DelayMS: 0, Profile: "same_request", ProfileRevision: "builtin-v1",
		AttemptsUsed: 1, MaxAttempts: 32}
}

func (r *run) retryReserve(failed string) RetryReserve {
	r.e.t.Helper()
	return RetryReserve{
		FailedAttemptID: failed, RequestID: identity.NewRequestID().String(), RequestBytes: []byte(`{"retry":"bytes"}`),
		InputDigest: strings.Repeat("a", 64), RequestDigest: strings.Repeat("b", 64), BindingFingerprint: "bfp-v1:fixture",
		ProfileID: "same_request", ProfileRevision: "builtin-v1", Stream: true,
		BindingRevision: scalar[string](r.e.t, r.e.s.db, "SELECT binding_revision FROM threads"), PolicyRevision: scalar[string](r.e.t, r.e.s.db, "SELECT policy_revision FROM threads"),
	}
}

// ready moves a Run waiting for its retry to the point the retry is reserved from.
func (r *run) ready() {
	r.e.t.Helper()
	r.phase("RetryWaiting", "Measuring")
	r.phase("Measuring", "Generating")
}

func TestARetryIsTheSameActionsSecondAttemptAndSpendsAnAttemptNotAStep(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("retry.key.0000000000001")
	first := r.failedFirstAttempt()
	scheduled := r.schedule(first.AttemptID)
	if scheduled.EvidenceID == "" || len(scheduled.Events) != 1 || scheduled.Events[0].Type != "model.retry_scheduled" || scheduled.Events[0].EvidenceID == nil || *scheduled.Events[0].EvidenceID != scheduled.EvidenceID {
		t.Fatalf("%+v", scheduled)
	}
	// Scheduling spends nothing and creates no Attempt.
	if scalar[int](t, e.s.db, "SELECT generation_attempts_used FROM runs") != 1 || scalar[int](t, e.s.db, "SELECT COUNT(*) FROM attempts") != 1 {
		t.Fatal("a scheduled retry spent or created something")
	}
	r.ready()
	rsv, err := e.s.ReserveRetry(bg, r.fence, r.retryReserve(first.AttemptID))
	if err != nil {
		t.Fatal(err)
	}
	if rsv.ActionID != first.ActionID || rsv.AttemptID == first.AttemptID || rsv.Ordinal != 1 || rsv.AttemptsUsed != 2 || len(rsv.Events) != 3 {
		t.Fatalf("%+v", rsv)
	}
	if got := e.eventTypes(); !strings.HasSuffix(got, "model.completed,model.retry_scheduled,action.dispatch_started,model.attempt_started,model.requested") {
		t.Fatal(got)
	}
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM actions") != 1 || scalar[int](t, e.s.db, "SELECT COUNT(*) FROM attempts") != 2 {
		t.Fatal("a retry is a second Attempt of the same Action")
	}
	if v := scalar[string](t, e.s.db, "SELECT status||'/'||current_attempt_id FROM actions"); v != "in_flight/"+rsv.AttemptID {
		t.Fatal(v)
	}
	if v := scalar[string](t, e.s.db, "SELECT ordinal||'/'||state FROM attempts WHERE attempt_id='"+rsv.AttemptID+"'"); v != "1/dispatch_started" {
		t.Fatal(v)
	}
	if v := scalar[string](t, e.s.db, "SELECT attempt_ordinal||'/'||generation_state||'/'||base_request_digest||'/'||request_digest FROM model_calls WHERE attempt_id='"+rsv.AttemptID+"'"); v !=
		"1/unknown/"+strings.Repeat("b", 64)+"/"+strings.Repeat("b", 64) {
		t.Fatal(v)
	}
	if scalar[int](t, e.s.db, "SELECT generation_attempts_used FROM runs") != 2 {
		t.Fatal("the retry did not spend an attempt")
	}
	// The retry's request is private Evidence of its own Attempt.
	if v := scalar[string](t, e.s.db, "SELECT COUNT(*) FROM evidence WHERE attempt_id='"+rsv.AttemptID+"' AND json_extract(metadata_json,'$.purpose')='model_request'"); v != "1" {
		t.Fatal(v)
	}
	// The retry receipt is Evidence of the failed Attempt, and names its receipt.
	if v := scalar[string](t, e.s.db, "SELECT json_extract(metadata_json,'$.purpose') FROM evidence WHERE evidence_id='"+scheduled.EvidenceID+"'"); v != "model_retry_receipt" {
		t.Fatal(v)
	}
	// It ends as any Attempt does, and the Action follows its last Attempt.
	if _, err := e.s.RecordAttemptEnd(bg, r.fence, AttemptEnd{AttemptID: rsv.AttemptID, State: "completed", Outcome: "completed", GenerationState: "terminal", BackendAttempts: one(1)}); err != nil {
		t.Fatal(err)
	}
	if v := scalar[string](t, e.s.db, "SELECT status FROM actions"); v != "completed" {
		t.Fatal(v)
	}
	if rep, err := e.s.VerifyClosure(bg); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestARetryIsRefusedAndWritesNothingWhereItsPreconditionsFail(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(e *env, r *run, first Reservation)
		step  string // schedule or reserve
		want  error
	}{
		"scheduling in another phase": {func(e *env, r *run, first Reservation) { r.phase("RetryWaiting", "Measuring") }, "schedule", ErrPhaseConflict},
		"scheduling after the deadline": {func(e *env, r *run, first Reservation) {
			mustExec(t, e.s.db, "UPDATE runs SET deadline_at='2026-10-07T12:00:00Z'")
		}, "schedule", ErrDeadlinePassed},
		"scheduling after a stop was recorded": {func(e *env, r *run, first Reservation) {
			mustExec(t, e.s.db, "UPDATE threads SET control_revision=control_revision+1")
		}, "schedule", ErrControlChanged},
		"scheduling by a driver that lost the thread": {func(e *env, r *run, first Reservation) {
			mustExec(t, e.s.db, "UPDATE threads SET writer_epoch=writer_epoch+1")
		}, "schedule", ErrWriterLost},
		"reserving in another phase": {func(e *env, r *run, first Reservation) {}, "reserve", ErrPhaseConflict},
		"reserving after the deadline": {func(e *env, r *run, first Reservation) {
			r.ready()
			mustExec(t, e.s.db, "UPDATE runs SET deadline_at='2026-10-07T12:00:00Z'")
		}, "reserve", ErrDeadlinePassed},
		"reserving with no attempt left": {func(e *env, r *run, first Reservation) {
			r.ready()
			mustExec(t, e.s.db, "UPDATE runs SET generation_attempts_used=32")
		}, "reserve", ErrGenerationBudget},
		"reserving after a stop was recorded": {func(e *env, r *run, first Reservation) {
			r.ready()
			mustExec(t, e.s.db, "UPDATE threads SET control_revision=control_revision+1")
		}, "reserve", ErrControlChanged},
		"reserving on a policy that changed": {func(e *env, r *run, first Reservation) {
			r.ready()
			mustExec(t, e.s.db, "UPDATE threads SET policy_revision='other-policy'")
		}, "reserve", ErrPolicyChanged},
		"reserving on a binding that changed": {func(e *env, r *run, first Reservation) {
			r.ready()
			mustExec(t, e.s.db, "UPDATE threads SET binding_revision='other-binding'")
		}, "reserve", ErrBindingChanged},
		"scheduling a profile the run's policy does not allow": {func(e *env, r *run, first Reservation) {
			mustExec(t, e.s.db, `UPDATE runs SET recovery_policy_json='{"allowed_profiles":["same_request"],"contract_version":"act-recovery/v1","max_attempts_per_act":2}'`)
		}, "schedule", ErrProfileNotAllowed},
		"reserving a profile the run's policy does not allow": {func(e *env, r *run, first Reservation) {
			r.ready()
			mustExec(t, e.s.db, `UPDATE runs SET recovery_policy_json='{"allowed_profiles":["same_request"],"contract_version":"act-recovery/v1","max_attempts_per_act":2}'`)
		}, "reserve", ErrProfileNotAllowed},
		"reserving by a driver that lost the thread": {func(e *env, r *run, first Reservation) {
			r.ready()
			mustExec(t, e.s.db, "UPDATE threads SET writer_epoch=writer_epoch+1")
		}, "reserve", ErrWriterLost},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			r := e.newRun("retry.key.0000000000002")
			first := r.failedFirstAttempt()
			in := r.retryReserve(first.AttemptID) // the revisions the Run was admitted on, before the test moves one
			tc.setup(e, r, first)
			rows := func() [4]int {
				return [4]int{scalar[int](t, e.s.db, "SELECT COUNT(*) FROM events"), scalar[int](t, e.s.db, "SELECT COUNT(*) FROM attempts"),
					scalar[int](t, e.s.db, "SELECT COUNT(*) FROM evidence"), scalar[int](t, e.s.db, "SELECT generation_attempts_used FROM runs")}
			}
			before := rows()
			var err error
			// The one profile of these cases that a Run's policy may leave out.
			profile := "same_request"
			if tc.want == ErrProfileNotAllowed {
				profile = "terminal_output_once"
			}
			if tc.step == "schedule" {
				sched := retrySchedule(first.AttemptID)
				sched.Profile = profile
				_, err = e.s.ScheduleRetry(bg, r.fence, sched)
			} else {
				in.ProfileID = profile
				_, err = e.s.ReserveRetry(bg, r.fence, in)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("%v, want %v", err, tc.want)
			}
			if rows() != before {
				t.Fatalf("a refused retry wrote something: %v -> %v", before, rows())
			}
		})
	}
}

func TestAnAttemptThatIsNotAKnownFailureOfAFirstAttemptIsNeverRetried(t *testing.T) {
	t.Run("one whose generation ended unknown", func(t *testing.T) {
		e := newEnv(t)
		r := e.newRun("retry.key.0000000000003")
		r.goTo("Generating")
		rsv := r.reserve()
		code := "MODEL_GENERATION_OUTCOME_UNKNOWN"
		if _, err := e.s.RecordAttemptEnd(bg, r.fence, AttemptEnd{AttemptID: rsv.AttemptID, State: "unknown", Outcome: "error", GenerationState: "unknown", FailureCode: &code}); err != nil {
			t.Fatal(err)
		}
		r.phase("Generating", "RetryWaiting")
		if _, err := e.s.ScheduleRetry(bg, r.fence, retrySchedule(rsv.AttemptID)); err == nil {
			t.Fatal("an unknown generation was scheduled for a retry")
		}
	})
	t.Run("one that has not ended", func(t *testing.T) {
		e := newEnv(t)
		r := e.newRun("retry.key.0000000000004")
		r.goTo("Generating")
		rsv := r.reserve()
		r.phase("Generating", "RetryWaiting")
		if _, err := e.s.ScheduleRetry(bg, r.fence, retrySchedule(rsv.AttemptID)); err == nil {
			t.Fatal("an attempt in flight was scheduled for a retry")
		}
	})
	t.Run("one that is not an attempt of the run", func(t *testing.T) {
		e := newEnv(t)
		r := e.newRun("retry.key.0000000000005")
		r.failedFirstAttempt()
		if _, err := e.s.ScheduleRetry(bg, r.fence, retrySchedule(identity.NewAttemptID().String())); err == nil {
			t.Fatal("an attempt that does not exist was scheduled for a retry")
		}
	})
	t.Run("one that already has its retry, or is the retry", func(t *testing.T) {
		e := newEnv(t)
		r := e.newRun("retry.key.0000000000006")
		first := r.failedFirstAttempt()
		r.schedule(first.AttemptID)
		r.ready()
		second, err := e.s.ReserveRetry(bg, r.fence, r.retryReserve(first.AttemptID))
		if err != nil {
			t.Fatal(err)
		}
		// A second retry of the same Attempt: the Action has its one retry.
		code := "REASONING_ONLY"
		if _, err := e.s.RecordAttemptEnd(bg, r.fence, AttemptEnd{AttemptID: second.AttemptID, State: "failed", Outcome: "error", GenerationState: "terminal", BackendAttempts: one(1), FailureCode: &code}); err != nil {
			t.Fatal(err)
		}
		r.phase("Generating", "RetryWaiting")
		if _, err := e.s.ScheduleRetry(bg, r.fence, retrySchedule(first.AttemptID)); err == nil {
			t.Fatal("the same Attempt was scheduled for a second retry")
		}
		// The retry itself is never retried.
		if _, err := e.s.ScheduleRetry(bg, r.fence, retrySchedule(second.AttemptID)); err == nil {
			t.Fatal("a retry was scheduled for a retry")
		}
		r.phase("RetryWaiting", "Measuring")
		r.phase("Measuring", "Generating")
		if _, err := e.s.ReserveRetry(bg, r.fence, r.retryReserve(second.AttemptID)); err == nil {
			t.Fatal("a third Attempt was reserved")
		}
		if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM attempts") != 2 {
			t.Fatal("an Action has two Attempts at most")
		}
	})
}

func TestAnEndRecordsTheTransformationsTheReceiptStatedAndABodyAsPartialWhenItIsCut(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("retry.key.0000000000007")
	r.goTo("Generating")
	rsv := r.reserve()
	code := "AUTH_FAILED"
	if _, err := e.s.RecordAttemptEnd(bg, r.fence, AttemptEnd{
		AttemptID: rsv.AttemptID, State: "failed", Outcome: "error", GenerationState: "not_started", BackendAttempts: one(0), FailureCode: &code,
		RawResponse: []byte(`{"error":"x"}`), RawResponseMediaType: "application/json", RawResponsePartial: true,
	}); err != nil {
		t.Fatal(err)
	}
	if v := scalar[string](t, e.s.db, "SELECT media_type||'/'||capture_complete||'/'||total_bytes FROM evidence WHERE json_extract(metadata_json,'$.purpose')='model_response'"); v != "application/json/0/13" {
		t.Fatal(v)
	}
	if v := scalar[string](t, e.s.db, "SELECT applied_transformations_json FROM model_calls"); v != "[]" {
		t.Fatal(v)
	}

	e2 := newEnv(t)
	r2 := e2.newRun("retry.key.0000000000008")
	first := r2.failedFirstAttempt()
	r2.schedule(first.AttemptID)
	r2.ready()
	in := r2.retryReserve(first.AttemptID)
	in.ProfileID, in.ProfileRevision = "terminal_output_once", "fake-tooo-1"
	second, err := e2.s.ReserveRetry(bg, r2.fence, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e2.s.RecordAttemptEnd(bg, r2.fence, AttemptEnd{AttemptID: second.AttemptID, State: "completed", Outcome: "completed", GenerationState: "terminal",
		BackendAttempts: one(1), AppliedTransformations: []string{"format_suffix"}}); err != nil {
		t.Fatal(err)
	}
	if v := scalar[string](t, e2.s.db, "SELECT recovery_profile||'/'||applied_transformations_json FROM model_calls WHERE attempt_ordinal=1"); v != `terminal_output_once/["format_suffix"]` {
		t.Fatal(v)
	}
}

func TestAnEndKeepsAPrivateDiagnosisOfWhyItEndedAsItDidAndNothingWhenThereIsNone(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("retry.key.0000000000009")
	r.goTo("Generating")
	rsv := r.reserve()
	code := "MODEL_CONTRACT_FAILED"
	source := "HTTP_502"
	if _, err := e.s.RecordAttemptEnd(bg, r.fence, AttemptEnd{
		AttemptID: rsv.AttemptID, State: "unknown", Outcome: "error", GenerationState: "unknown", FailureCode: &code,
		Diagnosis: "the refusal is not strict JSON", SourceCode: &source,
	}); err != nil {
		t.Fatal(err)
	}
	if v := scalar[string](t, e.s.db, "SELECT json_extract(CAST(group_concat(data) AS TEXT),'$.violation')||'/'||json_extract(CAST(group_concat(data) AS TEXT),'$.source_code') FROM evidence_chunks WHERE evidence_id IN (SELECT evidence_id FROM evidence WHERE json_extract(metadata_json,'$.purpose')='model_attempt_diagnosis')"); v != "the refusal is not strict JSON/HTTP_502" {
		t.Fatal(v)
	}

	e2 := newEnv(t)
	r2 := e2.newRun("retry.key.0000000000010")
	r2.goTo("Generating")
	rsv2 := r2.reserve()
	if _, err := e2.s.RecordAttemptEnd(bg, r2.fence, AttemptEnd{AttemptID: rsv2.AttemptID, State: "completed", Outcome: "completed", GenerationState: "terminal", BackendAttempts: one(1)}); err != nil {
		t.Fatal(err)
	}
	if v := scalar[int](t, e2.s.db, "SELECT COUNT(*) FROM evidence WHERE json_extract(metadata_json,'$.purpose')='model_attempt_diagnosis'"); v != 0 {
		t.Fatal("an end with nothing to explain kept a diagnosis")
	}
}
