package service_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func verifierPolicy(operation []string, timeout int64) func(*harnesstest.Layout) {
	return func(l *harnesstest.Layout) {
		(toolConfig{tools: []string{}, process: true}).edit(l)
		policy := l.Reg["policies"].([]any)[0].(map[string]any)
		executable, err := os.Executable()
		if err != nil {
			panic(err)
		}
		argv := []any{"-test.run=TestHelperProcess", "--"}
		for _, arg := range operation {
			argv = append(argv, arg)
		}
		policy["verification"] = map[string]any{
			"format_version": "rencrow-verification-plan/v1", "process_profile_ref": "helper", "executable": executable,
			"argv": argv, "cwd": ".", "env_profile_ref": "helper", "timeout_seconds": timeout, "pass_condition": "exit_zero",
		}
	}
}

func waitForVerifierRunning(t *testing.T, r *rig, runID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		state := r.val("SELECT COALESCE((SELECT a.state FROM actions ac JOIN attempts a ON a.action_id=ac.action_id WHERE ac.run_id=? AND ac.kind='verification' LIMIT 1),'not_started')", runID)
		if state == "running" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the verifier did not reach a running process attempt; state=%q", state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFixedVerifierRunsAfterTheFinalAndStoresItsProcessEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, operation, wantStatus, wantStdout, wantStderr string
		wantExit                                            int64
	}{
		{name: "exit zero", operation: "echo", wantStatus: "passed", wantStdout: "hello out", wantStderr: "hello err", wantExit: 0},
		{name: "nonzero exit", operation: "exit", wantStatus: "failed", wantExit: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			fake.SetReply(harnesstest.Final("The model claims the tests passed."))
			r := newModelRig(t, fake, nil, verifierPolicy([]string{tc.operation, "7"}, 30))
			info := r.openSession("verify.open." + strings.ReplaceAll(tc.name, " ", "."))
			start := r.startRun(info.ThreadID, "verify.start."+strings.ReplaceAll(tc.name, " ", "."), "run the fixed check")
			run := r.waitTerminal(start.RunID)
			if run.Result == nil || run.Result.Status != "completed" || run.Result.FinalText != "The model claims the tests passed." {
				t.Fatalf("the model's final is retained separately from verification: %+v", run.Result)
			}
			v := run.Result.Verification
			if v.Status != tc.wantStatus || len(v.EvidenceIDs) != 2 || v.CriteriaRevision == nil || len(*v.CriteriaRevision) != 64 {
				t.Fatalf("unexpected verifier result: %+v", v)
			}
			if got := r.val("SELECT COUNT(*) FROM actions WHERE run_id=? AND kind='verification'", start.RunID); got != "1" {
				t.Fatalf("verification Actions: %s", got)
			}
			if got := r.val("SELECT COUNT(*) FROM actions WHERE run_id=? AND kind='tool'", start.RunID); got != "0" {
				t.Fatalf("the verifier became a model Tool call: %s", got)
			}
			var actionArgs map[string]string
			rawArgs := r.val("SELECT CAST(args_bytes AS TEXT) FROM actions WHERE run_id=? AND kind='verification'", start.RunID)
			if err := json.Unmarshal([]byte(rawArgs), &actionArgs); err != nil || len(actionArgs) != 1 || actionArgs["criteria_revision"] != *v.CriteriaRevision {
				t.Fatalf("the Action must retain only the criteria digest: %q (%v)", rawArgs, err)
			}
			for _, forbidden := range []string{"TestHelperProcess", tc.operation, r.layout.Work, "RENCROW_HELPER"} {
				if strings.Contains(rawArgs, forbidden) {
					t.Fatalf("Action arguments disclosed %q: %s", forbidden, rawArgs)
				}
			}

			events := r.threadEvents(info.ThreadID)
			var prepared, dispatched, completed protocol.Event
			for _, ev := range events {
				switch ev.Type {
				case "action.prepared":
					p := payloadOf[protocol.ActionPreparedPayload](t, ev)
					if p.Kind == "verification" {
						prepared = ev
						if p.Name != "process.exec" {
							t.Fatalf("unexpected verification action name: %+v", p)
						}
					}
				case "action.dispatch_started":
					p := payloadOf[protocol.ActionDispatchStartedPayload](t, ev)
					if p.ActionID != "" && prepared.EventID != "" {
						prior := payloadOf[protocol.ActionPreparedPayload](t, prepared)
						if p.ActionID == prior.ActionID {
							dispatched = ev
						}
					}
				case "action.completed":
					p := payloadOf[protocol.ActionCompletedPayload](t, ev)
					if len(p.ResultEvidenceIDs) == 2 {
						completed = ev
						if p.ExitCode == nil || *p.ExitCode != tc.wantExit || p.CaptureComplete != true {
							t.Fatalf("unexpected verification Action result: %+v", p)
						}
						if !equalStrings(p.ResultEvidenceIDs, v.EvidenceIDs) {
							t.Fatalf("Action evidence differs from RunResult: %v / %v", p.ResultEvidenceIDs, v.EvidenceIDs)
						}
					}
				}
			}
			modelDone := eventIndex(events, "model.completed")
			terminal := eventIndex(events, "run.terminal")
			if prepared.EventID == "" || dispatched.EventID == "" || completed.EventID == "" || !(modelDone < eventPosition(events, prepared.EventID) && eventPosition(events, prepared.EventID) < eventPosition(events, dispatched.EventID) && eventPosition(events, dispatched.EventID) < eventPosition(events, completed.EventID) && eventPosition(events, completed.EventID) < terminal) {
				t.Fatalf("verifier Actions must follow the final model result and precede run.terminal: %s", eventTypes(events))
			}
			if tc.wantStdout != "" && (r.evidence(v.EvidenceIDs[0]) != tc.wantStdout || r.evidence(v.EvidenceIDs[1]) != tc.wantStderr) {
				t.Fatalf("the verifier's actual process output was not sealed: %q / %q", r.evidence(v.EvidenceIDs[0]), r.evidence(v.EvidenceIDs[1]))
			}
			term := payloadOf[protocol.RunTerminalPayload](t, ofType(events, "run.terminal")[0])
			var persisted protocol.RunResult
			if err := json.Unmarshal([]byte(r.evidence(term.ResultEvidenceID)), &persisted); err != nil || persisted.Verification.Status != tc.wantStatus || persisted.Verification.CriteriaRevision == nil || *persisted.Verification.CriteriaRevision != *v.CriteriaRevision {
				t.Fatalf("reopened result evidence lost the verifier outcome: %+v (%v)", persisted.Verification, err)
			}
		})
	}
}

func TestModelClaimsAndPriorSuccessfulProcessCallsCannotPassTheFixedVerifier(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Reply{Dynamic: func(req modelport.ChatRequest) harnesstest.Reply {
		for _, msg := range req.Messages {
			if msg.Role == "tool" {
				return harnesstest.Final("I already ran a successful check, so verification passed.")
			}
		}
		return calls(tc("prior-check", "process.exec", execArgs([]string{"exit", "0"}, "helper", 10)))
	}})
	// The model's separate process.exec call exits zero. The fixed host check exits
	// seven and must remain failed.
	r := newModelRig(t, fake, nil, verifierPolicy([]string{"exit", "7"}, 30))
	info := r.openSession("verify.claim.open.0001")
	start := r.startRun(info.ThreadID, "verify.claim.start.0001", "try to claim this task")
	result := r.waitTerminal(start.RunID).Result
	if result == nil || result.Status != "completed" || result.FinalText != "I already ran a successful check, so verification passed." || result.Verification.Status != "failed" {
		t.Fatalf("model text and a prior successful Tool call cannot pass the fixed verifier: %+v", result)
	}
	if got := r.val("SELECT COUNT(*) FROM actions WHERE run_id=? AND kind='tool'", start.RunID); got != "1" {
		t.Fatalf("the earlier successful process Tool was not recorded: %s", got)
	}
	if got := r.val("SELECT COUNT(*) FROM actions WHERE run_id=? AND kind='verification'", start.RunID); got != "1" {
		t.Fatalf("the host verifier was not recorded separately: %s", got)
	}
	if got := len(result.Verification.EvidenceIDs); got != 2 {
		t.Fatalf("the failed host check must name its two sealed captures: %d", got)
	}
}

func TestUnexecutedConfiguredVerifierStaysNotRun(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Reply{Kind: harnesstest.KindErrorOutcome, Code: modelport.CodeContractFailed})
	r := newModelRig(t, fake, nil, verifierPolicy([]string{"echo"}, 30))
	info := r.openSession("verify.notrun.open.0001")
	start := r.startRun(info.ThreadID, "verify.notrun.start.0001", "the model does not complete")
	run := r.waitTerminal(start.RunID)
	if run.Result == nil || run.Result.Verification.Status != "not_run" || len(run.Result.Verification.EvidenceIDs) != 0 || run.Result.Verification.CriteriaRevision == nil {
		t.Fatalf("an unexecuted configured plan is not_run: %+v", run.Result)
	}
	if got := r.val("SELECT COUNT(*) FROM actions WHERE run_id=? AND kind='verification'", start.RunID); got != "0" {
		t.Fatalf("the plan ran without an accepted model final: %s", got)
	}
}

func TestIncompleteVerifierCaptureCannotPass(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Final("the model says it passed"))
	r := newModelRig(t, fake, nil, verifierPolicy([]string{"big", "4096"}, 30))
	info := r.openSession("verify.capture.open.0001")
	start := r.startRun(info.ThreadID, "verify.capture.start.0001", "run with a small capture budget", func(in *protocol.StartInput) {
		in.Limits.MaxCaptureBytes = 1024
	})
	run := r.waitTerminal(start.RunID)
	if run.Result == nil || run.Result.Verification.Status != "failed" || len(run.Result.Verification.EvidenceIDs) != 2 {
		t.Fatalf("a partial process capture is not a passing verification: %+v", run.Result)
	}
	for _, id := range run.Result.Verification.EvidenceIDs {
		total := uint64Total(r, id)
		capture := decode[protocol.EvidenceReadResult](t, r.mustCall("evidence/read", protocol.EvidenceReadInput{
			EvidenceID: id, ProjectionVersion: "raw/v1", Range: protocol.ByteRange{Start: 0, End: total},
		}))
		if !capture.Partial || capture.CaptureComplete {
			t.Fatalf("the capture must retain its incomplete marker: %+v", capture)
		}
	}
}

func TestRunDeadlineDuringVerifierDoesNotAdoptModelFinal(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Final("the model completed before the check"))
	r := newModelRig(t, fake, nil, verifierPolicy([]string{"sleep"}, 30))
	info := r.openSession("verify.deadline.open.0001")
	start := r.startRun(info.ThreadID, "verify.deadline.start.0001", "run a slow verifier", func(in *protocol.StartInput) {
		in.Limits.DeadlineSeconds = 5
	})
	waitForVerifierRunning(t, r, start.RunID)
	run := r.waitTerminal(start.RunID)
	if run.Result == nil || run.Result.Status != "incomplete" || run.Result.Code != "DEADLINE_EXCEEDED" || run.Result.FinalText != "" || run.Result.FinalMessageID != nil ||
		run.Result.Verification.Status != "failed" || len(run.Result.UnresolvedActionIDs) != 0 {
		t.Fatalf("a verifier that reaches the Run deadline cannot leave the accepted final completed: %+v", run.Result)
	}
	if got := r.val("SELECT a.state FROM actions ac JOIN attempts a ON a.action_id=ac.action_id WHERE ac.run_id=? AND ac.kind='verification'", start.RunID); got != "cancelled" {
		t.Fatalf("the deadline-stopped verifier is not durably cancelled: %s", got)
	}
}

func TestDriverContextStopDuringVerifierDoesNotAdoptModelFinal(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Final("the model completed before the check"))
	r := newModelRig(t, fake, nil, verifierPolicy([]string{"sleep"}, 30))
	info := r.openSession("verify.driverstop.open.0001")
	start := r.startRun(info.ThreadID, "verify.driverstop.start.0001", "run a slow verifier")
	waitForVerifierRunning(t, r, start.RunID)
	r.mustCall("service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
	r.svc.Quiesce()
	run := r.waitTerminal(start.RunID)
	if run.Result == nil || run.Result.Status != "incomplete" || run.Result.Code != "DRIVER_STOPPED" || run.Result.FinalText != "" || run.Result.FinalMessageID != nil ||
		run.Result.Verification.Status != "failed" || len(run.Result.UnresolvedActionIDs) != 0 {
		t.Fatalf("a stopped verifier cannot leave the accepted final completed: %+v", run.Result)
	}
	if got := r.val("SELECT a.state FROM actions ac JOIN attempts a ON a.action_id=ac.action_id WHERE ac.run_id=? AND ac.kind='verification'", start.RunID); got != "cancelled" {
		t.Fatalf("the stopped verifier is not durably cancelled: %s", got)
	}
}

func TestUnknownVerifierPersistsBlockedResultAndNeverResendsOnResume(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Final("the model's raw final response"))
	r := newModelRig(t, fake, nil, verifierPolicy([]string{"sleep"}, 30))
	info := r.openSession("verify.unknown.open.0001")
	// Make recording the process identity fail after dispatch. The process runner
	// therefore reports a started but untrackable process; the verifier must close
	// that Action as unknown if the store remains writable.
	r.exec(`CREATE TRIGGER test_verification_start_unknown BEFORE UPDATE OF state ON attempts
		WHEN NEW.state='running' AND OLD.state='dispatch_started'
		AND (SELECT kind FROM actions WHERE action_id=NEW.action_id)='verification'
		BEGIN SELECT RAISE(ABORT,'test process identity failure'); END`)
	start := r.startRun(info.ThreadID, "verify.unknown.start.0001", "run the fixed check")
	first := r.waitTerminal(start.RunID)
	result := first.Result
	if result == nil || result.Status != "blocked" || result.Code != "EFFECT_OUTCOME_UNKNOWN" || !result.Resumable ||
		result.FinalText != "" || result.FinalMessageID != nil || len(result.UnresolvedActionIDs) != 1 ||
		result.Verification.Status != "unknown" || len(result.Verification.EvidenceIDs) != 2 || result.Verification.CriteriaRevision == nil {
		t.Fatalf("an unknown verifier must block without adopting the model final: %+v", result)
	}
	actionID := result.UnresolvedActionIDs[0]
	if got := r.val("SELECT state FROM attempts WHERE action_id=?", actionID); got != "unknown" {
		t.Fatalf("immediate terminal result did not close the process attempt as unknown: %s", got)
	}
	if got := r.val("SELECT status FROM actions WHERE action_id=?", actionID); got != "unknown" {
		t.Fatalf("immediate terminal result did not close the verification Action as unknown: %s", got)
	}
	if got := r.val("SELECT COUNT(*) FROM evidence WHERE run_id=? AND json_extract(metadata_json,'$.purpose')='model_response'", start.RunID); got != "1" {
		t.Fatalf("the model's raw final response was not retained as existing response Evidence: %s", got)
	}
	if got := len(ofType(r.threadEvents(info.ThreadID), "run.terminal")); got != 1 {
		t.Fatalf("the unknown terminal result was not persisted immediately: %d terminal events", got)
	}

	resume := r.mustCall("run/resume", resumeInput(start.TaskID, start.RunID, "verify.unknown.resume.0001"))
	nextStart := decode[protocol.ResumeResult](t, resume)
	next := r.waitTerminal(nextStart.RunID)
	if next.Result == nil || next.Result.Status != "blocked" || next.Result.Code != "EFFECT_OUTCOME_UNKNOWN" ||
		len(next.Result.UnresolvedActionIDs) != 1 || next.Result.UnresolvedActionIDs[0] != actionID || len(fake.Generates()) != 1 ||
		next.Result.Verification.Status != "unknown" || next.Result.Verification.CriteriaRevision == nil ||
		*next.Result.Verification.CriteriaRevision != *result.Verification.CriteriaRevision || !equalStrings(next.Result.Verification.EvidenceIDs, result.Verification.EvidenceIDs) {
		t.Fatalf("a same-Task resume must stay blocked without asking the model again: %+v calls=%d", next.Result, len(fake.Generates()))
	}
	if got := r.val("SELECT COUNT(*) FROM actions WHERE run_id=? AND kind='verification'", start.RunID); got != "1" {
		t.Fatalf("the unknown fixed command was resent: %s verification Actions", got)
	}
	if got := r.val("SELECT COUNT(*) FROM attempts WHERE action_id=?", actionID); got != "1" {
		t.Fatalf("the unknown verification Action gained an Attempt: %s", got)
	}
	if oldAgain := r.waitTerminal(start.RunID).Result; oldAgain == nil || oldAgain.Verification.Status != "unknown" ||
		!equalStrings(oldAgain.Verification.EvidenceIDs, result.Verification.EvidenceIDs) || oldAgain.Code != result.Code {
		t.Fatalf("the original unknown result or its Evidence changed after resume: %+v", oldAgain)
	}
}

func TestVerifierHoldsWorkspaceLockUntilItsResultIsTerminal(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Final("model complete"))
	r := newModelRig(t, fake, nil, verifierPolicy([]string{"sleep"}, 30))
	firstSession := r.openSession("verify.lock.open.0001")
	secondSession := decode[protocol.SessionOpenResult](t, r.mustCall("session/open", r.openParams("verify.lock.open.0002"))).Session
	first := r.startRun(firstSession.ThreadID, "verify.lock.start.0001", "run verifier")
	deadline := time.Now().Add(10 * time.Second)
	for {
		var started bool
		for _, ev := range r.threadEvents(firstSession.ThreadID) {
			if ev.Type != "action.prepared" {
				continue
			}
			if payloadOf[protocol.ActionPreparedPayload](t, ev).Kind != "verification" {
				continue
			}
			for _, candidate := range r.threadEvents(firstSession.ThreadID) {
				if candidate.Type == "action.dispatch_started" && payloadOf[protocol.ActionDispatchStartedPayload](t, candidate).ActionID == payloadOf[protocol.ActionPreparedPayload](t, ev).ActionID {
					started = true
				}
			}
		}
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fixed verifier never began dispatch")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for r.val("SELECT a.state FROM actions ac JOIN attempts a ON a.action_id=ac.action_id WHERE ac.run_id=? AND ac.kind='verification'", first.RunID) != "running" {
		if time.Now().After(deadline) {
			t.Fatal("the verifier process start was not recorded")
		}
		time.Sleep(5 * time.Millisecond)
	}
	second := r.startRun(secondSession.ThreadID, "verify.lock.start.0002", "try a concurrent run")
	secondResult := r.waitTerminal(second.RunID).Result
	if secondResult == nil || secondResult.Status != "blocked" || secondResult.Code != "WORKSPACE_BUSY" {
		t.Fatalf("a concurrent Run changed the workspace during verification: %+v", secondResult)
	}
	if got := len(fake.Generates()); got != 1 {
		t.Fatalf("the concurrent Run reached the model while the verifier held the lock: %d model calls", got)
	}
	if receipt := r.interrupt(first.RunID, 0, "verify.lock.interrupt.0001"); receipt.Code != "CANCEL_REQUESTED" {
		t.Fatalf("could not stop the long check: %+v", receipt)
	}
	firstResult := r.waitTerminal(first.RunID).Result
	if firstResult == nil || firstResult.Status != "cancelled" || firstResult.Verification.Status != "failed" || len(firstResult.UnresolvedActionIDs) != 0 {
		t.Fatalf("a cancelled verifier is recorded as failed, not passed: %+v", firstResult)
	}
}

func eventIndex(events []protocol.Event, typ string) int {
	for i, ev := range events {
		if ev.Type == typ {
			return i
		}
	}
	return -1
}

func eventPosition(events []protocol.Event, id string) int {
	for i, ev := range events {
		if ev.EventID == id {
			return i
		}
	}
	return -1
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
