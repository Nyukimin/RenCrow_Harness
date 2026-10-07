package sqlite

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func compactParams(thread, key string, ctxRev, ctl int64, dry bool) []byte {
	b, err := json.Marshal(map[string]any{"thread_id": thread, "expected_context_revision": ctxRev, "expected_control_revision": ctl, "dry_run": dry, "idempotency_key": key})
	if err != nil {
		panic(err)
	}
	return b
}

func (e *env) readerOnly() intake.Caller {
	return e.newCaller(func(c *intake.CallerConfig) {
		c.Principal, c.Relays, c.ReadableSessionOwners = "user:reader", nil, []string{"core:local"}
	})
}

// TestAdmitCompactMakesASystemTaskAndRunAndRefusesWhatItMust: one transaction makes the system
// Task (no Turn, no input), its Run, the receipt that holds no result yet, task.created and
// run.started; the Run is read back as a system Run (no input to apply, the operation's receipt,
// the Thread's latest typed blocks); the order of the checks is that of turn/start.
func TestAdmitCompactMakesASystemTaskAndRunAndRefusesWhatItMust(t *testing.T) {
	e := newEnv(t)
	params := compactParams(e.thread, "cmp.key.000000000001", 0, 0, false)
	out, err := e.s.AdmitCompact(bg, e.adm, params)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Result.Accepted || out.Replayed || out.RunID == "" || len(out.Events) != 2 || out.Events[0].Type != "task.created" || out.Events[1].Type != "run.started" {
		t.Fatalf("%+v", out)
	}
	if got := scalar[string](t, e.s.db, "SELECT t.kind||'/'||COALESCE(t.origin_turn_id,'-')||'/'||r.phase||'/'||r.status FROM tasks t JOIN runs r ON r.task_id=t.task_id WHERE r.run_id=?", out.RunID); got != "compaction/-/Admitting/running" {
		t.Fatal(got)
	}
	if got := scalar[string](t, e.s.db, "SELECT stage||'/'||operation||'/'||COALESCE(result_json,'none') FROM receipts WHERE receipt_id=?", out.Result.ReceiptID); got != "accepted/context/compact/none" {
		t.Fatal(got)
	}
	// Its limits are its own: the two stage requests, a model step it never takes.
	rec, err := e.s.LoadRun(bg, out.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.System || rec.StartMessageID != "" || rec.InputEvidenceID != "" || rec.StartReceiptID != out.Result.ReceiptID || rec.Limits.MaxGenerationAttempts != 2 || rec.Limits.MaxModelSteps != 1 {
		t.Fatalf("%+v", rec)
	}
	for _, ev := range out.Events {
		if ev.ReceiptID == nil || *ev.ReceiptID != out.Result.ReceiptID {
			t.Fatalf("%s names no receipt of the operation", ev.Type)
		}
	}
	// A system Run has no input to apply, and the call changes nothing.
	f := Fence{ThreadID: e.thread, RunID: out.RunID, Epoch: 1}
	revs := e.counts()
	if events, err := e.s.ApplyInput(bg, f); err != nil || len(events) != 0 {
		t.Fatalf("%v %v", events, err)
	}
	e.wantNoChange(revs)
	// The operation is running once the Run is driven.
	if err := e.s.SetPhase(bg, f, "Admitting", "Loading"); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, e.s.db, "SELECT stage FROM receipts WHERE receipt_id=?", out.Result.ReceiptID); got != "running" {
		t.Fatal(got)
	}

	// The same request is the same receipt, even now that the Thread has an active Run; another
	// payload under the key is a conflict; a new key finds the Thread busy.
	again, err := e.s.AdmitCompact(bg, e.adm, params)
	if err != nil || !again.Replayed || again.Result.ReceiptID != out.Result.ReceiptID || len(again.Events) != 0 {
		t.Fatalf("%+v %v", again, err)
	}
	if looked, found, err := e.s.LookupCompact(bg, e.adm, params); err != nil || !found || looked.Result.ReceiptID != out.Result.ReceiptID {
		t.Fatalf("%+v %v", looked, err)
	}
	_, _, err = e.s.LookupCompact(bg, e.adm, compactParams(e.thread, "cmp.key.000000000001", 0, 0, true))
	wantCode(t, err, protocol.CodeIdempotencyConflict)
	_, err = e.s.AdmitCompact(bg, e.adm, compactParams(e.thread, "cmp.key.000000000001", 1, 0, false))
	wantCode(t, err, protocol.CodeIdempotencyConflict)
	if _, err := e.s.AdmitCompact(bg, e.adm, compactParams(e.thread, "cmp.key.000000000009", 0, 0, true)); err == nil {
		t.Fatal("a dry run is recorded by AdmitCompactDryRun and makes no Run")
	}
	_, err = e.s.AdmitCompact(bg, e.adm, compactParams(e.thread, "cmp.key.000000000002", 0, 0, false))
	wantCode(t, err, protocol.CodeBusy)
}

func TestAdmitCompactRefusalsWriteNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		params func(e *env) []byte
		adm    func(e *env) Admission
		want   string
	}{
		"a context revision that is not the Thread's": {func(e *env) []byte { return compactParams(e.thread, "cmp.refuse.000000001", 1, 0, false) }, nil, protocol.CodeRevisionConflict},
		"a control revision that is not the Thread's": {func(e *env) []byte { return compactParams(e.thread, "cmp.refuse.000000002", 0, 1, false) }, nil, protocol.CodeRevisionConflict},
		"a Thread that does not exist": {func(e *env) []byte {
			return compactParams("thr_00000000-0000-7000-8000-000000000001", "cmp.refuse.000000003", 0, 0, false)
		}, nil, protocol.CodeForbidden},
		"a caller that may only read": {func(e *env) []byte { return compactParams(e.thread, "cmp.refuse.000000004", 0, 0, false) }, func(e *env) Admission {
			a := e.adm
			a.Caller = e.readerOnly()
			return a
		}, protocol.CodeForbidden},
		"a writer epoch that is not the driver's": {func(e *env) []byte { return compactParams(e.thread, "cmp.refuse.000000005", 0, 0, false) }, func(e *env) Admission {
			a := e.adm
			a.WriterEpoch = 2
			return a
		}, protocol.CodeRevisionConflict},
		"a limit resolver that is missing": {func(e *env) []byte { return compactParams(e.thread, "cmp.refuse.000000006", 0, 0, false) }, func(e *env) Admission {
			a := e.adm
			a.Caps = nil
			return a
		}, protocol.CodeInternal},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			adm := e.adm
			if tc.adm != nil {
				adm = tc.adm(e)
			}
			before := e.counts()
			_, err := e.s.AdmitCompact(bg, adm, tc.params(e))
			wantCode(t, err, tc.want)
			e.wantNoChange(before)
		})
	}
}

// TestADryRunReceiptHoldsTheAnswerAndNothingElseIsWritten: the one row a dry run stores is its
// receipt, terminal at once with the CompactResult; it is refused for revisions that are not the
// Thread's and a retried one is the same receipt.
func TestADryRunReceiptHoldsTheAnswerAndNothingElseIsWritten(t *testing.T) {
	e := newEnv(t)
	before := e.counts()
	params := compactParams(e.thread, "cmp.dry.0000000000001", 0, 0, true)
	n := int64(10)
	result := protocol.CompactResult{Status: "dry_run", Before: &protocol.BudgetReport{State: "verified_exact", PromptLower: &n, PromptUpper: &n, ContextLimit: &n,
		RequestDigest: strings.Repeat("a", 64), NormalizationFingerprint: "fp"}}
	out, err := e.s.AdmitCompactDryRun(bg, e.adm, params, result)
	if err != nil || !out.DryRun || out.RunID != "" || len(out.Events) != 0 {
		t.Fatalf("%+v %v", out, err)
	}
	after := e.counts()
	for tb, n := range before {
		want := n
		if tb == "receipts" {
			want++
		}
		if after[tb] != want {
			t.Errorf("%s: %d -> %d", tb, n, after[tb])
		}
	}
	rec, err := e.s.GetReceipt(bg, e.caller, out.Result.ReceiptID)
	if err != nil || rec.Stage != "terminal" || rec.Operation != "context/compact" || rec.Result == nil || rec.Result.Type != protocol.ReceiptCompactResult {
		t.Fatalf("%+v %v", rec, err)
	}
	again, err := e.s.AdmitCompactDryRun(bg, e.adm, params, result)
	if err != nil || !again.Replayed || again.Result.ReceiptID != out.Result.ReceiptID {
		t.Fatalf("%+v %v", again, err)
	}
	_, err = e.s.AdmitCompactDryRun(bg, e.adm, compactParams(e.thread, "cmp.dry.0000000000002", 3, 0, true), result)
	wantCode(t, err, protocol.CodeRevisionConflict)
	if _, err := e.s.AdmitCompactDryRun(bg, e.adm, compactParams(e.thread, "cmp.dry.0000000000003", 0, 0, true), protocol.CompactResult{Status: "executed", Outcome: protocol.Str("NormalCompacted")}); err == nil {
		t.Fatal("a dry run records only the answer of a dry run")
	}
	if _, err := e.s.AdmitCompactDryRun(bg, e.adm, compactParams(e.thread, "cmp.dry.0000000000004", 0, 0, false), result); err == nil {
		t.Fatal("a dry-run answer is recorded for a dry-run request only")
	}
}

// TestTheEndOfASystemRunMakesItsReceiptTerminalInTheSameTransaction: a manual compaction's Run
// cannot end without its result (the operation never reads as running after its Run ended), a
// result is given for no other kind of Run, and a Run settled by another driver ends its receipt
// with what the decision gives.
func TestTheEndOfASystemRunMakesItsReceiptTerminalInTheSameTransaction(t *testing.T) {
	end := func(e *env, out CompactOutcome) (Fence, TerminalInput) {
		f := Fence{ThreadID: e.thread, RunID: out.RunID, Epoch: 1}
		for _, p := range [][2]string{{"Admitting", "Loading"}, {"Loading", "PersistingResult"}} {
			if err := e.s.SetPhase(bg, f, p[0], p[1]); err != nil {
				t.Fatal(err)
			}
		}
		res := protocol.RunResult{RunID: out.RunID, TaskID: out.TaskID, Status: "completed", Code: "COMPACTION_COMMITTED", FinalText: "",
			Verification: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}}, EvidenceIDs: []string{}, UnresolvedActionIDs: []string{}}
		return f, TerminalInput{Result: res, ResultEvidenceID: "evd_00000000-0000-7000-8000-0000000000aa"}
	}
	admit := func(e *env, key string) CompactOutcome {
		out, err := e.s.AdmitCompact(bg, e.adm, compactParams(e.thread, key, 0, 0, false))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	t.Run("without a result it is refused", func(t *testing.T) {
		e := newEnv(t)
		f, in := end(e, admit(e, "cmp.end.000000000001"))
		before := e.counts()
		if _, err := e.s.PersistTerminal(bg, f, in); protocol.CodeOf(err) != protocol.CodeInternal {
			t.Fatalf("%v", err)
		}
		e.wantNoChange(before)
	})
	t.Run("with its result", func(t *testing.T) {
		e := newEnv(t)
		out := admit(e, "cmp.end.000000000002")
		f, in := end(e, out)
		in.CompactResult = &protocol.CompactResult{Status: "unavailable", Error: &protocol.ErrorInfo{Code: "BUDGET_UNVERIFIED", Message: "the size of the context could not be verified"}}
		events, err := e.s.PersistTerminal(bg, f, in)
		if err != nil || len(events) != 1 || events[0].Type != "run.terminal" {
			t.Fatalf("%v %v", events, err)
		}
		rec, err := e.s.GetReceipt(bg, e.caller, out.Result.ReceiptID)
		if err != nil || rec.Stage != "terminal" || rec.Result == nil || rec.Result.Type != protocol.ReceiptCompactResult {
			t.Fatalf("%+v %v", rec, err)
		}
		if got := scalar[string](t, e.s.db, "SELECT COALESCE(active_run_id,'-') FROM threads WHERE thread_id=?", e.thread); got != "-" {
			t.Fatal(got)
		}
	})
	t.Run("a result for a Run that does not compact", func(t *testing.T) {
		e := newEnv(t)
		r := e.newRun("cmp.end.work.0000000001")
		r.goTo("PersistingResult")
		res := protocol.RunResult{RunID: r.start.RunID, TaskID: r.start.TaskID, Status: "incomplete", Code: "DRIVER_STOPPED", Verification: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}},
			EvidenceIDs: []string{}, UnresolvedActionIDs: []string{}, Resumable: true}
		_, err := e.s.PersistTerminal(bg, r.fence, TerminalInput{Result: res, ResultEvidenceID: "evd_00000000-0000-7000-8000-0000000000bb",
			CompactResult: &protocol.CompactResult{Status: "cancelled"}})
		wantCode(t, err, protocol.CodeInternal)
	})
	t.Run("a Run settled by another driver", func(t *testing.T) {
		e := newEnv(t)
		out := admit(e, "cmp.end.000000000003")
		if _, err := e.s.BumpWriterEpoch(bg, e.thread); err != nil {
			t.Fatal(err)
		}
		var saw StaleRun
		events, err := e.s.TerminalizeStale(bg, e.thread, 2, nil, func(st StaleRun) (TerminalInput, error) {
			saw = st
			res := protocol.RunResult{RunID: st.RunID, TaskID: st.TaskID, Status: "incomplete", Code: "DRIVER_STOPPED", Verification: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}},
				EvidenceIDs: st.EvidenceIDs, UnresolvedActionIDs: []string{}, Resumable: true}
			return TerminalInput{Result: res, ResultEvidenceID: "evd_00000000-0000-7000-8000-0000000000cc",
				CompactResult: &protocol.CompactResult{Status: "cancelled", Error: &protocol.ErrorInfo{Code: "DRIVER_STOPPED", Message: "stopped", Retryable: true}}}, nil
		})
		if err != nil || len(events) != 1 || !saw.System || !saw.Compaction || saw.LastCheckpoint != nil || saw.RunID != out.RunID {
			t.Fatalf("%v %v %+v", events, err, saw)
		}
		rec, err := e.s.GetReceipt(bg, e.caller, out.Result.ReceiptID)
		if err != nil || rec.Stage != "terminal" || rec.Result == nil {
			t.Fatalf("%+v %v", rec, err)
		}
	})
}

// TestNothingIsAppliedAtOrBeforeTheBoundaryOfTheCheckpointAThreadStandsOn: the schema itself
// refuses an entry at or before the durable boundary of the current checkpoint (it would
// never be seen).
func TestNothingIsAppliedAtOrBeforeTheBoundaryOfTheCheckpointAThreadStandsOn(t *testing.T) {
	e := newEnv(t)
	r := e.compactingRun("bnd.run.0000000000001")
	r.phase("Compacting", "CommittingCheckpoint")
	in := e.commitInput(r, "rencrow-checkpoint-candidate/v1\x00{\"synthetic\":true}")
	in.SemanticBoundary, in.DurableBoundary = 5, 5
	if _, err := e.s.CommitCheckpoint(bg, e.fenceOf(r), in); err != nil {
		t.Fatal(err)
	}
	msg := scalar[string](t, e.s.db, "SELECT message_id FROM items WHERE thread_id=? LIMIT 1", e.thread)
	err := func() error {
		_, err := e.s.db.ExecContext(bg, "INSERT INTO context_entries(thread_id, context_seq, message_id, applied_context_revision) VALUES(?,?,?,?)", e.thread, 3, msg, 9)
		return err
	}()
	if err == nil || !strings.Contains(err.Error(), "durable boundary") {
		t.Fatalf("an entry before the boundary was accepted: %v", err)
	}
	// (That the code itself chooses the coordinate after the boundary is held by the fork test of
	// the Service, whose Thread has no entry of its own: its first is the boundary plus one.)
}

// TestOpenRefusesAStoreThatLacksWhatThisBuildNeedsAndSourceImportsAreImmutable: a store made by
// an earlier build of the draft schema is refused at Open, never converted; and the provenance a
// fork imports is written once.
func TestOpenRefusesAStoreThatLacksWhatThisBuildNeedsAndSourceImportsAreImmutable(t *testing.T) {
	for _, drop := range []string{"TABLE source_imports", "TRIGGER context_entries_after_checkpoint", "TRIGGER source_imports_no_update"} {
		t.Run(drop, func(t *testing.T) {
			s, root := newStore(t)
			if _, err := s.db.ExecContext(bg, "DROP "+drop); err != nil {
				t.Fatal(err)
			}
			_, _ = s.db.ExecContext(bg, "PRAGMA wal_checkpoint(TRUNCATE)")
			_ = s.Close()
			if _, err := Open(bg, root, Options{Clock: intake.FixedClock(testNow)}); !errors.Is(err, ErrSchemaOutdated) {
				t.Fatalf("%v", err)
			}
		})
	}
	e := newEnv(t)
	thread := e.thread
	// An import needs rows to point at: a checkpoint of the Thread.
	r := e.compactingRun("imp.run.00000000000001")
	r.phase("Compacting", "CommittingCheckpoint")
	in := e.commitInput(r, "rencrow-checkpoint-candidate/v1\x00{\"synthetic\":true}")
	if _, err := e.s.CommitCheckpoint(bg, e.fenceOf(r), in); err != nil {
		t.Fatal(err)
	}
	evidence := scalar[string](t, e.s.db, "SELECT evidence_id FROM evidence LIMIT 1")
	_, other := e.newThread("core:local")
	if _, err := e.s.db.ExecContext(bg, "INSERT INTO source_imports(thread_id, source_kind, source_id, from_thread_id, from_checkpoint_id, fork_checkpoint_id) VALUES(?,?,?,?,?,?)",
		other, "evidence", evidence, thread, in.CheckpointID, in.CheckpointID); err != nil {
		t.Fatal(err)
	}
	if ok, err := evidenceInScope(bg, e.s.db, evidence, other); err != nil || !ok {
		t.Fatalf("an imported source is in the Thread's scope: %v %v", ok, err)
	}
	if ok, _ := checkpointInScope(bg, e.s.db, in.CheckpointID, other); ok {
		t.Fatal("a checkpoint is in the scope of the Thread that imported it only when it imported it as one")
	}
	for _, q := range []string{"UPDATE source_imports SET source_id='x'", "DELETE FROM source_imports"} {
		if _, err := e.s.db.ExecContext(bg, q); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("%s: %v", q, err)
		}
	}
	if _, err := e.s.db.ExecContext(bg, "INSERT INTO source_imports(thread_id, source_kind, source_id, from_thread_id, from_checkpoint_id, fork_checkpoint_id) VALUES(?,?,?,?,?,?)",
		other, "file", evidence, thread, in.CheckpointID, in.CheckpointID); err == nil {
		t.Fatal("a kind of source the schema does not know was accepted")
	}
}

// TestLoadForkSourceNeedsControlOfTheThreadAndACheckpointOfIt: the caller must control the
// Thread; a checkpoint of another Thread or none at all is INVALID_REQUEST; a checkpoint that
// does not read back is INTEGRITY_BLOCKED.
func TestLoadForkSourceNeedsControlOfTheThreadAndACheckpointOfIt(t *testing.T) {
	e := newEnv(t)
	r := e.compactingRun("fsrc.run.0000000000001")
	r.phase("Compacting", "CommittingCheckpoint")
	in := e.commitInput(r, "rencrow-checkpoint-candidate/v1\x00{\"synthetic\":true}")
	if _, err := e.s.CommitCheckpoint(bg, e.fenceOf(r), in); err != nil {
		t.Fatal(err)
	}
	src, err := e.s.LoadForkSource(bg, e.caller, e.thread, in.CheckpointID)
	if err != nil || src.Checkpoint.CheckpointID != in.CheckpointID || src.SessionID != e.session || src.BindingRevision == "" || src.PolicyRevision == "" {
		t.Fatalf("%+v %v", src, err)
	}
	_, err = e.s.LoadForkSource(bg, e.readerOnly(), e.thread, in.CheckpointID)
	wantCode(t, err, protocol.CodeForbidden)
	_, err = e.s.LoadForkSource(bg, e.caller, "thr_00000000-0000-7000-8000-000000000001", in.CheckpointID)
	wantCode(t, err, protocol.CodeForbidden)
	_, err = e.s.LoadForkSource(bg, e.caller, e.thread, "ckp_00000000-0000-7000-8000-000000000001")
	wantCode(t, err, protocol.CodeInvalidRequest)
	_, otherThread := e.newThread("core:local")
	_, err = e.s.LoadForkSource(bg, e.caller, otherThread, in.CheckpointID)
	wantCode(t, err, protocol.CodeInvalidRequest)
	if _, err := e.s.db.ExecContext(bg, "DROP TRIGGER checkpoints_no_update"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.ExecContext(bg, "UPDATE checkpoints SET candidate_bytes=candidate_bytes||x'20' WHERE checkpoint_id=?", in.CheckpointID); err != nil {
		t.Fatal(err)
	}
	_, err = e.s.LoadForkSource(bg, e.caller, e.thread, in.CheckpointID)
	wantCode(t, err, protocol.CodeIntegrityBlocked)
	_ = time.Second
}

// TestTheRestoreClosureHoldsWhatAForkImportedToTheSameStandard: an import whose source is not in
// the recovery point, or whose fork checkpoint is not a fork checkpoint of its Thread, is a
// finding of the closure check, as any reference that does not resolve is.
func TestTheRestoreClosureHoldsWhatAForkImportedToTheSameStandard(t *testing.T) {
	e := newEnv(t)
	r := e.compactingRun("clo.run.00000000000001")
	r.phase("Compacting", "CommittingCheckpoint")
	in := e.commitInput(r, "rencrow-checkpoint-candidate/v1\x00{\"synthetic\":true}")
	if _, err := e.s.CommitCheckpoint(bg, e.fenceOf(r), in); err != nil {
		t.Fatal(err)
	}
	if rep, err := e.s.VerifyClosure(bg); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
	_, other := e.newThread("core:local")
	if _, err := e.s.db.ExecContext(bg, "INSERT INTO source_imports(thread_id, source_kind, source_id, from_thread_id, from_checkpoint_id, fork_checkpoint_id) VALUES(?,?,?,?,?,?)",
		other, "evidence", "evd_00000000-0000-7000-8000-0000000000ee", e.thread, in.CheckpointID, in.CheckpointID); err != nil {
		t.Fatal(err)
	}
	rep, err := e.s.VerifyClosure(bg)
	if err != nil || rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
	joined := strings.Join(rep.Issues, "\n")
	if !strings.Contains(joined, "source_imports evidence") || !strings.Contains(joined, "source_imports fork checkpoint") || rep.Counts["source_imports"] != 1 {
		t.Fatalf("%s %v", joined, rep.Counts)
	}
}
