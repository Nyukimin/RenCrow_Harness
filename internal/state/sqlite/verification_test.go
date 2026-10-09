package sqlite

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func TestStaleVerifierAttemptIsSealedUnknownAndNeverRedispatched(t *testing.T) {
	runStaleVerifierResume(t, true)
}

func TestZeroOutputStaleVerifierStillBlocksResume(t *testing.T) {
	runStaleVerifierResume(t, false)
}

func runStaleVerifierResume(t *testing.T, partialOutput bool) {
	t.Helper()
	e := newEnv(t)
	r := e.newRun("verify.stale.0000000001")
	r.goTo("PersistingResult")
	policyRevision, criteriaRevision := strings.Repeat("a", 64), strings.Repeat("b", 64)
	mustExec(t, e.s.db, "UPDATE threads SET policy_revision=? WHERE thread_id=?", policyRevision, e.thread)
	actionID, attemptID := identity.NewActionID().String(), identity.NewAttemptID().String()
	started, err := e.s.StartVerificationAttempt(bg, r.fence, StartVerificationInput{
		ActionID: actionID, AttemptID: attemptID, PolicyRevision: policyRevision, CriteriaRevision: criteriaRevision,
	})
	if err != nil || len(started.Events) != 2 {
		t.Fatalf("start fixed verification Action: %+v %v", started, err)
	}
	// A partial capture cannot be turned into a passing result by an in-process caller.
	if _, err := e.s.CompleteVerificationAttempt(bg, r.fence, CompleteVerificationInput{
		ActionID: actionID, AttemptID: attemptID, CriteriaRevision: criteriaRevision, Status: "passed", EffectState: "completed",
		ExitCode: one(0), CaptureComplete: false, Captures: []SealCapture{
			{EvidenceID: identity.NewEvidenceID().String(), Purpose: PurposeVerificationStdout},
			{EvidenceID: identity.NewEvidenceID().String(), Purpose: PurposeVerificationStderr},
		},
	}); err == nil {
		t.Fatal("a passing verifier with incomplete capture was accepted")
	}
	var expectedEvidenceIDs []string
	if partialOutput {
		partialEvidence := identity.NewEvidenceID().String()
		if err := e.s.AppendCaptureChunk(bg, r.fence, CaptureChunk{
			EvidenceID: partialEvidence, AttemptID: attemptID, Purpose: PurposeVerificationStdout, Ordinal: 0, ByteStart: 0, Data: []byte("partial output"),
		}); err != nil {
			t.Fatal(err)
		}
		expectedEvidenceIDs = append(expectedEvidenceIDs, partialEvidence)
	}
	// The writer epoch changed while this process may still have been running. Recovery
	// closes the one verifier attempt as unknown, seals its partial bytes, and does not
	// create another Action or invoke the command again.
	mustExec(t, e.s.db, "UPDATE threads SET writer_epoch=2 WHERE thread_id=?", e.thread)
	var stale StaleRun
	events, err := e.s.TerminalizeStale(bg, e.thread, 2, nil, func(got StaleRun) (TerminalInput, error) {
		stale = got
		return TerminalInput{ResultEvidenceID: identity.NewEvidenceID().String(), Result: protocol.RunResult{
			RunID: got.RunID, TaskID: got.TaskID, Status: "blocked", Code: "EFFECT_OUTCOME_UNKNOWN",
			Verification: *got.Verification, EvidenceIDs: got.EvidenceIDs, UnresolvedActionIDs: got.UnresolvedTools, Resumable: true,
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stale.Verification == nil || stale.Verification.Status != "unknown" || stale.Verification.CriteriaRevision == nil ||
		*stale.Verification.CriteriaRevision != criteriaRevision || !slices.Equal(stale.Verification.EvidenceIDs, expectedEvidenceIDs) ||
		len(stale.UnresolvedTools) != 1 || stale.UnresolvedTools[0] != actionID {
		t.Fatalf("stale verifier did not remain unresolved with its captures: %+v", stale)
	}
	if partialOutput {
		if got := scalar[string](t, e.s.db, "SELECT a.state||'/'||json_extract(a.result_json,'$.status')||'/'||e.capture_complete FROM attempts a JOIN evidence e ON e.attempt_id=a.attempt_id WHERE a.attempt_id=?", attemptID); got != "unknown/unknown/0" {
			t.Fatalf("the attempt or capture was promoted: %s", got)
		}
	} else if got := scalar[string](t, e.s.db, "SELECT a.state||'/'||json_extract(a.result_json,'$.status') FROM attempts a WHERE a.attempt_id=?", attemptID); got != "unknown/unknown" {
		t.Fatalf("the zero-output attempt was not retained as unknown: %s", got)
	}
	var reconciled protocol.ActionCompletedPayload
	for _, ev := range events {
		if ev.Type == protocol.EventActionCompleted {
			if err := json.Unmarshal(ev.Payload, &reconciled); err != nil {
				t.Fatal(err)
			}
		}
	}
	if reconciled.ActionID != actionID || reconciled.EffectState != "unknown" || reconciled.CaptureComplete || !slices.Equal(reconciled.ResultEvidenceIDs, expectedEvidenceIDs) {
		t.Fatalf("the recovered Action does not say unknown with its exact capture: %+v", reconciled)
	}
	if count := scalar[int](t, e.s.db, "SELECT COUNT(*) FROM actions WHERE run_id=? AND kind='verification'", r.start.RunID); count != 1 {
		t.Fatalf("recovery created or retried a verifier Action: %d", count)
	}
	if again, err := e.s.TerminalizeStale(bg, e.thread, 2, nil, func(StaleRun) (TerminalInput, error) {
		return TerminalInput{}, errors.New("a terminal Run must not be settled again")
	}); err != nil || len(again) != 0 {
		t.Fatalf("recovery reran after terminalization: %v %v", again, err)
	}
	oldResult := scalar[string](t, e.s.db, "SELECT result_json FROM runs WHERE run_id=?", r.start.RunID)
	resume, err := e.s.AdmitResume(bg, e.resumeAdm(2, map[string]string{attemptID: "not_applicable"}), resumeParams(
		r.start.TaskID, r.start.RunID, 0, "verify.stale.resume.0001", nil))
	if err != nil || !resume.Blocked || resume.Result.PreviousRunID != r.start.RunID {
		t.Fatalf("a same-Task resume must remain blocked by the unknown verifier: %+v %v", resume, err)
	}
	info, err := e.s.GetRun(bg, e.caller, resume.Result.RunID)
	if err != nil || !info.Terminal || info.Result.Status != "blocked" || info.Result.Code != "EFFECT_OUTCOME_UNKNOWN" ||
		len(info.Result.UnresolvedActionIDs) != 1 || info.Result.UnresolvedActionIDs[0] != actionID || info.Result.Verification.Status != "unknown" ||
		info.Result.Verification.CriteriaRevision == nil || *info.Result.Verification.CriteriaRevision != criteriaRevision ||
		!slices.Equal(info.Result.Verification.EvidenceIDs, expectedEvidenceIDs) {
		t.Fatalf("the blocked resume lost the original criterion or its Evidence: %+v (%v)", info.Result, err)
	}
	if partialOutput && (!slices.Contains(info.Result.EvidenceIDs, expectedEvidenceIDs[0])) {
		t.Fatal("the blocked resume did not retain the original partial verifier Evidence")
	}
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM attempts WHERE action_id=?", actionID) != 1 ||
		scalar[int](t, e.s.db, "SELECT COUNT(*) FROM actions WHERE run_id=? AND kind='verification'", r.start.RunID) != 1 ||
		scalar[string](t, e.s.db, "SELECT result_json FROM runs WHERE run_id=?", r.start.RunID) != oldResult {
		t.Fatal("resume resent the verifier or changed its original result")
	}
}
