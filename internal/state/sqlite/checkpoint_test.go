package sqlite

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// compactingRun is a Run that has applied its input and stands in the phase a checkpoint is
// committed from.
func (e *env) compactingRun(key string) *run {
	e.t.Helper()
	r := e.newRun(key)
	r.phase("Admitting", "Loading")
	if _, err := e.s.ApplyInput(bg, r.fence); err != nil {
		e.t.Fatal(err)
	}
	r.phase("Loading", "Assembling")
	r.phase("Assembling", "Measuring")
	r.phase("Measuring", "Compacting")
	return r
}

func (e *env) commitInput(r *run, blob string) CheckpointCommit {
	e.t.Helper()
	sum := sha256.Sum256([]byte(blob))
	evidence := r.start.Intake.EvidenceID
	raw := scalar[string](e.t, e.s.db, "SELECT raw_hash FROM evidence WHERE evidence_id='"+evidence+"'")
	total := uint64(scalar[int](e.t, e.s.db, "SELECT total_bytes FROM evidence WHERE evidence_id='"+evidence+"'"))
	before, err := e.s.RecordEvidence(bg, r.fence, "measure_result", "application/json", false, []byte(`{"count":"before"}`))
	if err != nil {
		e.t.Fatal(err)
	}
	after, err := e.s.RecordEvidence(bg, r.fence, "measure_result", "application/json", false, []byte(`{"count":"after"}`))
	if err != nil {
		e.t.Fatal(err)
	}
	return CheckpointCommit{
		CheckpointID: identity.NewCheckpointID().String(), Mode: "normal", Blob: []byte(blob), Hash: hex.EncodeToString(sum[:]),
		ExpectedContextRevision: scalar[int64](e.t, e.s.db, "SELECT context_revision FROM threads"),
		PolicyRevision:          scalar[string](e.t, e.s.db, "SELECT policy_revision FROM threads"),
		BindingRevision:         scalar[string](e.t, e.s.db, "SELECT binding_revision FROM threads"),
		SemanticBoundary:        1, DurableBoundary: 1, CountJSON: `{"after":{},"before":{}}`,
		BeforeEvidenceID: before, AfterEvidenceID: after,
		Sources: []SourceCheck{{SourceID: evidence, RawHash: raw, ProjectionVersion: "text/v1", End: total}},
	}
}

func (e *env) fenceOf(r *run) Fence {
	f := r.fence
	f.ControlRevision = scalar[int64](e.t, e.s.db, "SELECT control_revision FROM threads")
	return f
}

func (e *env) checkpointCount() int {
	return scalar[int](e.t, e.s.db, "SELECT COUNT(*) FROM checkpoints")
}

// TestCommitCheckpointStoresTheBytesAndMovesThePointerTogether is F14: the exact bytes, the
// pointer, the context revision (by exactly one) and the event, in one transaction, and a
// stored checkpoint is read back whole by the snapshot, which then holds only what came after
// its durable boundary.
func TestCommitCheckpointStoresTheBytesAndMovesThePointerTogether(t *testing.T) {
	e := newEnv(t)
	r := e.compactingRun("cp.run.00000000000001")
	r.phase("Compacting", "CommittingCheckpoint")
	in := e.commitInput(r, "rencrow-checkpoint-candidate/v1\x00{\"synthetic\":true}")
	evBefore := scalar[int](t, e.s.db, "SELECT event_seq FROM threads")
	revBefore := scalar[int64](t, e.s.db, "SELECT context_revision FROM threads")

	res, err := e.s.CommitCheckpoint(bg, e.fenceOf(r), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.NewContextRevision != revBefore+1 || len(res.Events) != 1 || res.Events[0].Type != "checkpoint.committed" {
		t.Fatalf("%+v", res)
	}
	p := payloadOf[protocol.CheckpointCommittedPayload](t, res.Events[0])
	if p.CheckpointID != in.CheckpointID || p.Mode != "normal" || p.CandidateHash != in.Hash || p.ContextRevision != revBefore+1 || p.SemanticBoundary != 1 || p.DurableBoundary != 1 ||
		p.BeforeCountEvidenceID != in.BeforeEvidenceID || p.AfterCountEvidenceID != in.AfterEvidenceID {
		t.Fatalf("%+v", p)
	}
	if got := scalar[string](t, e.s.db, "SELECT current_checkpoint_id||'/'||context_revision||'/'||event_seq FROM threads"); got != in.CheckpointID+"/"+strconv.FormatInt(revBefore+1, 10)+"/"+strconv.Itoa(evBefore+1) {
		t.Fatalf("thread %s", got)
	}
	row, err := e.s.LoadCheckpoint(bg, in.CheckpointID)
	if err != nil || string(row.Blob) != string(in.Blob) || row.Hash != in.Hash || row.ContextRevision != revBefore+1 || row.Mode != "normal" || row.SemanticBoundary != 1 ||
		row.DurableBoundary != 1 || row.ParentCheckpointID != nil || row.RunID != r.start.RunID || row.ThreadID != e.thread {
		t.Fatalf("%+v %v", row, err)
	}
	// The snapshot stands on it, and what it covers is not applied history any more.
	snap, err := e.s.LoadSnapshot(bg, r.start.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Checkpoint == nil || snap.Checkpoint.CheckpointID != in.CheckpointID || len(snap.Applied) != 0 || snap.ContextRevision != revBefore+1 {
		t.Fatalf("%+v", snap)
	}
}

// TestCommitCheckpointIsRefusedWhenTheThreadMovedAndWritesNothing: each fact the candidate was
// made against, changed, is its own refusal, and nothing is stored.
func TestCommitCheckpointIsRefusedWhenTheThreadMovedAndWritesNothing(t *testing.T) {
	cases := map[string]struct {
		prepare func(e *env, r *run, in *CheckpointCommit) Fence
		want    error
	}{
		"the context moved": {func(e *env, r *run, in *CheckpointCommit) Fence { in.ExpectedContextRevision--; return e.fenceOf(r) }, ErrStale},
		"another parent": {func(e *env, r *run, in *CheckpointCommit) Fence {
			in.ParentCheckpointID = protocol.Str(identity.NewCheckpointID().String())
			return e.fenceOf(r)
		}, ErrStale},
		"the policy":  {func(e *env, r *run, in *CheckpointCommit) Fence { in.PolicyRevision = "other"; return e.fenceOf(r) }, ErrPolicyChanged},
		"the binding": {func(e *env, r *run, in *CheckpointCommit) Fence { in.BindingRevision = "other"; return e.fenceOf(r) }, ErrBindingChanged},
		"a stop recorded first": {func(e *env, r *run, in *CheckpointCommit) Fence {
			f := e.fenceOf(r)
			if _, err := e.s.RecordInterrupt(bg, e.caller, interruptParams(r.start.RunID, f.ControlRevision, "cp.int.0000000000000001")); err != nil {
				t.Fatal(err)
			}
			return f // the driver still holds the control revision it read before the stop
		}, ErrControlChanged},
		"the writer epoch": {func(e *env, r *run, in *CheckpointCommit) Fence {
			f := e.fenceOf(r)
			f.Epoch = 9
			return f
		}, ErrWriterLost},
		"a source that does not exist": {func(e *env, r *run, in *CheckpointCommit) Fence {
			in.Sources = append(in.Sources, SourceCheck{SourceID: identity.NewEvidenceID().String(), RawHash: strings.Repeat("a", 64), ProjectionVersion: "text/v1", End: 1})
			return e.fenceOf(r)
		}, nil},
		"a source with another hash": {func(e *env, r *run, in *CheckpointCommit) Fence {
			in.Sources[0].RawHash = strings.Repeat("a", 64)
			return e.fenceOf(r)
		}, nil},
		"a source shorter than its range": {func(e *env, r *run, in *CheckpointCommit) Fence {
			in.Sources[0].End += 10
			return e.fenceOf(r)
		}, nil},
		"bytes that are not the hash": {func(e *env, r *run, in *CheckpointCommit) Fence {
			in.Blob = append(in.Blob, '!')
			return e.fenceOf(r)
		}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			r := e.compactingRun("cp.refuse.000000000001")
			r.phase("Compacting", "CommittingCheckpoint")
			in := e.commitInput(r, "rencrow-checkpoint-candidate/v1\x00{}")
			f := tc.prepare(e, r, &in)
			counts := e.counts()
			_, err := e.s.CommitCheckpoint(bg, f, in)
			if err == nil {
				t.Fatal("the commit was accepted")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("%v, want %v", err, tc.want)
			}
			if tc.want == nil && protocol.CodeOf(err) != protocol.CodeIntegrityBlocked && protocol.CodeOf(err) != protocol.CodeInternal {
				t.Fatalf("%v", err)
			}
			if e.checkpointCount() != 0 || scalar[string](t, e.s.db, "SELECT COALESCE(current_checkpoint_id,'none') FROM threads") != "none" {
				t.Fatal("something was stored")
			}
			if tc.want != ErrControlChanged {
				e.wantNoChange(counts)
			}
		})
	}
}

// TestACandidateIsNotStaleBecauseAnInputWaitsInTheQueue is H13/A06: an input that is only
// queued moves the queue revision and nothing the candidate was made against, so a candidate
// that is otherwise good is committed; a change of the context itself is what makes one stale.
func TestACandidateIsNotStaleBecauseAnInputWaitsInTheQueue(t *testing.T) {
	e := newEnv(t)
	r := e.compactingRun("cp.queue.0000000000001")
	in := e.commitInput(r, "rencrow-checkpoint-candidate/v1\x00{}")
	e.appendInput(r, "later", protocol.DispositionNextTurn, 0, "cp.append.00000000000001")
	e.appendInput(r, "next", protocol.DispositionNextStep, 0, "cp.append.00000000000002")
	if scalar[int](t, e.s.db, "SELECT queue_revision FROM threads") != 2 {
		t.Fatal("setup")
	}
	r.phase("Compacting", "CommittingCheckpoint")
	if _, err := e.s.CommitCheckpoint(bg, e.fenceOf(r), in); err != nil {
		t.Fatalf("a queued input made a good candidate stale: %v", err)
	}
	// What waited stays waiting: a checkpoint does not apply or drop it.
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM queue_inputs WHERE delivery_state='queued'") != 2 {
		t.Fatal("queued inputs were touched")
	}
	// Applying a queued input moves the context revision: a candidate made before that is stale.
	e2 := newEnv(t)
	r2 := e2.compactingRun("cp.queue.0000000000002")
	in2 := e2.commitInput(r2, "rencrow-checkpoint-candidate/v1\x00{}")
	e2.appendInput(r2, "applied", protocol.DispositionNextStep, 0, "cp.append.00000000000003")
	r2.phase("Compacting", "Assembling") // the next step applies it
	if _, _, err := e2.s.ApplyNextSteps(bg, r2.fence); err != nil {
		t.Fatal(err)
	}
	r2.phase("Assembling", "Measuring")
	r2.phase("Measuring", "Compacting")
	r2.phase("Compacting", "CommittingCheckpoint")
	if _, err := e2.s.CommitCheckpoint(bg, e2.fenceOf(r2), in2); !errors.Is(err, ErrStale) {
		t.Fatalf("%v", err)
	}
}

func TestCommitCheckpointNeedsItsPhaseAndTime(t *testing.T) {
	e := newEnv(t)
	r := e.compactingRun("cp.phase.0000000000001")
	in := e.commitInput(r, "rencrow-checkpoint-candidate/v1\x00{}")
	if _, err := e.s.CommitCheckpoint(bg, e.fenceOf(r), in); !errors.Is(err, ErrPhaseConflict) {
		t.Fatalf("from Compacting: %v", err)
	}
	r.phase("Compacting", "CommittingCheckpoint")
	mustExec(t, e.s.db, "UPDATE runs SET deadline_at='2026-10-07T11:59:59Z'") // the store's clock reads 12:00:00
	if _, err := e.s.CommitCheckpoint(bg, e.fenceOf(r), in); !errors.Is(err, ErrDeadlinePassed) {
		t.Fatalf("past the deadline: %v", err)
	}
	if e.checkpointCount() != 0 {
		t.Fatal("a checkpoint was stored")
	}
}

// TestACheckpointIsImmutableAndAnUnreadableOneIsNeverNone is A28: a stored checkpoint can be
// neither changed nor deleted, and one that is named and cannot be read back exactly stops the
// load; it is not read as "no checkpoint".
func TestACheckpointIsImmutableAndAnUnreadableOneIsNeverNone(t *testing.T) {
	e := newEnv(t)
	r := e.compactingRun("cp.immutable.000000001")
	r.phase("Compacting", "CommittingCheckpoint")
	in := e.commitInput(r, "rencrow-checkpoint-candidate/v1\x00{}")
	if _, err := e.s.CommitCheckpoint(bg, e.fenceOf(r), in); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"UPDATE checkpoints SET mode='emergency'", "DELETE FROM checkpoints", "UPDATE checkpoints SET candidate_bytes=x'00'"} {
		if _, err := e.s.db.Exec(q); err == nil || !strings.Contains(err.Error(), "checkpoint is immutable") {
			t.Errorf("%s: %v", q, err)
		}
	}
	// The pointer is the only thing that moves, and only through the commit.
	// Force the damage the triggers would otherwise refuse, to see what the load does.
	mustExec(t, e.s.db, "DROP TRIGGER checkpoints_no_update")
	mustExec(t, e.s.db, "UPDATE checkpoints SET candidate_bytes=x'0102'")
	if _, err := e.s.LoadCheckpoint(bg, in.CheckpointID); protocol.CodeOf(err) != protocol.CodeIntegrityBlocked {
		t.Fatalf("%v", err)
	}
	if _, err := e.s.LoadSnapshot(bg, r.start.RunID); protocol.CodeOf(err) != protocol.CodeIntegrityBlocked {
		t.Fatalf("a checkpoint that cannot be read is not no checkpoint: %v", err)
	}
	if _, err := e.s.LoadCheckpoint(bg, identity.NewCheckpointID().String()); protocol.CodeOf(err) != protocol.CodeIntegrityBlocked {
		t.Fatalf("a named checkpoint that does not exist: %v", err)
	}
}

// TestAlostCommitAnswerIsResolvedByTheIssuedIDAndTheBytes is A07's store half: stored with
// the hash is the same commit, absent is a commit that did not happen, and stored with other
// bytes is a contradiction; no second checkpoint is made to find out.
func TestALostCommitAnswerIsResolvedByTheIssuedIDAndTheBytes(t *testing.T) {
	e := newEnv(t)
	r := e.compactingRun("cp.resolve.0000000000001")
	r.phase("Compacting", "CommittingCheckpoint")
	in := e.commitInput(r, "rencrow-checkpoint-candidate/v1\x00{}")
	if st, err := e.s.ResolveCheckpoint(bg, in.CheckpointID, in.Hash); err != nil || st != CommitAbsent {
		t.Fatalf("before: %v %v", st, err)
	}
	if _, err := e.s.CommitCheckpoint(bg, e.fenceOf(r), in); err != nil {
		t.Fatal(err)
	}
	if st, err := e.s.ResolveCheckpoint(bg, in.CheckpointID, in.Hash); err != nil || st != CommitStored {
		t.Fatalf("after: %v %v", st, err)
	}
	if _, err := e.s.ResolveCheckpoint(bg, in.CheckpointID, strings.Repeat("0", 64)); protocol.CodeOf(err) != protocol.CodeIntegrityBlocked {
		t.Fatalf("other bytes: %v", err)
	}
	if e.checkpointCount() != 1 {
		t.Fatal("asking made a checkpoint")
	}
}

// TestACompactionStageIsReservedFromCompactingAndIsNoModelStep: it spends a generation attempt
// and nothing else, the request is kept with its stage and the context revision it was
// sent at, and an act request does the same, which is how what the model was shown is known.
func TestACompactionStageIsReservedFromCompactingAndIsNoModelStep(t *testing.T) {
	e := newEnv(t)
	r := e.compactingRun("cp.stage.00000000000001")
	in := reserveInput()
	in.Stage, in.Stream = "work_summary", false
	if _, err := e.s.ReserveGeneration(bg, r.fence, ReserveInput{Stage: "act", RequestID: in.RequestID, ArgsBytes: in.ArgsBytes, RequestBytes: in.RequestBytes, InputDigest: in.InputDigest,
		RequestDigest: in.RequestDigest, BindingFingerprint: in.BindingFingerprint, ProfileID: in.ProfileID, ProfileRevision: in.ProfileRevision}); !errors.Is(err, ErrPhaseConflict) {
		t.Fatalf("an act generation from Compacting: %v", err)
	}
	rsv, err := e.s.ReserveGeneration(bg, r.fence, in)
	if err != nil {
		t.Fatal(err)
	}
	if rsv.AttemptsUsed != 1 || len(rsv.Events) != 4 {
		t.Fatalf("%+v", rsv)
	}
	rec, err := e.s.LoadRun(bg, r.start.RunID)
	if err != nil || rec.ModelSteps != 0 || rec.AttemptsUsed != 1 {
		t.Fatalf("a stage is not a model step: %+v %v", rec, err)
	}
	if got := scalar[string](t, e.s.db, "SELECT stage||'/'||attempt_ordinal||'/'||recovery_profile FROM model_calls WHERE attempt_id='"+rsv.AttemptID+"'"); got != "work_summary/0/same_request" {
		t.Fatal(got)
	}
	meta := scalar[string](t, e.s.db, "SELECT metadata_json FROM evidence WHERE evidence_id='"+rsv.RequestEvidenceID+"'")
	if !strings.Contains(meta, `"stage":"work_summary"`) || !strings.Contains(meta, `"context_revision":1`) {
		t.Fatalf("%s", meta)
	}
	end := AttemptEnd{AttemptID: rsv.AttemptID, State: "completed", Outcome: "completed", GenerationState: "terminal", BackendAttempts: one(1)}
	if _, err := e.s.RecordAttemptEnd(bg, r.fence, end); err != nil {
		t.Fatal(err)
	}
	// It is also budgeted: with no attempt left a stage is refused.
	e2 := newEnv(t)
	r2 := e2.compactingRun("cp.stage.00000000000002")
	mustExec(t, e2.s.db, "UPDATE runs SET generation_attempts_used=32")
	if _, err := e2.s.ReserveGeneration(bg, r2.fence, in); !errors.Is(err, ErrGenerationBudget) {
		t.Fatalf("%v", err)
	}
}

// TestTheSnapshotSaysWhichEntriesTheModelWasShown: an entry applied at or before the context
// revision of an act request that was sent has been presented, and one applied after has not.
func TestTheSnapshotSaysWhichEntriesTheModelWasShown(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("cp.seen.00000000000000001")
	r.phase("Admitting", "Loading")
	if _, err := e.s.ApplyInput(bg, r.fence); err != nil {
		t.Fatal(err)
	}
	snap, err := e.s.LoadSnapshot(bg, r.start.RunID)
	if err != nil || len(snap.Applied) != 1 || snap.Applied[0].Presented {
		t.Fatalf("an entry nothing was sent after is not claimed as shown: %v", err)
	}
	r.phase("Loading", "Assembling")
	r.phase("Assembling", "Measuring")
	r.phase("Measuring", "Generating")
	rsv := r.reserve()
	if _, err := e.s.RecordAttemptEnd(bg, r.fence, AttemptEnd{AttemptID: rsv.AttemptID, State: "completed", Outcome: "completed", GenerationState: "terminal", BackendAttempts: one(1)}); err != nil {
		t.Fatal(err)
	}
	snap, err = e.s.LoadSnapshot(bg, r.start.RunID)
	if err != nil || !snap.Applied[0].Presented {
		t.Fatalf("an entry applied before a generation was sent was shown: %v", err)
	}
	a := snap.Applied[0]
	if a.RawHash == "" || a.TotalBytes == 0 || !a.CaptureComplete || !a.TextProjection || a.AppliedRevision != 1 {
		t.Fatalf("%+v", a)
	}
	// An input applied after that generation was not shown to the model by it.
	e.appendInput(r, "late", protocol.DispositionNextStep, 0, "cp.seen.append.0000000001")
	r.phase("Generating", "ValidatingFinal")
	if _, _, err := e.s.ApplyNextSteps(bg, r.fence); err != nil {
		t.Fatal(err)
	}
	snap, err = e.s.LoadSnapshot(bg, r.start.RunID)
	if err != nil || len(snap.Applied) != 2 || !snap.Applied[0].Presented || snap.Applied[1].Presented || snap.Applied[1].AppliedRevision != 2 {
		t.Fatalf("%+v %v", snap.Applied, err)
	}
}

// TestOnlyAGenerationThatEndedTerminalShowedTheModelAnything: a request refused before
// generating (not started) and one whose outcome is unknown are not claimed to have shown
// the entries it carried.
func TestOnlyAGenerationThatEndedTerminalShowedTheModelAnything(t *testing.T) {
	zero := int64(0)
	for name, tc := range map[string]struct {
		end       AttemptEnd
		presented bool
	}{
		"terminal":    {AttemptEnd{State: "completed", Outcome: "completed", GenerationState: "terminal", BackendAttempts: one(1)}, true},
		"not started": {AttemptEnd{State: "failed", Outcome: "error", GenerationState: "not_started", BackendAttempts: &zero, FailureCode: protocol.Str("AUTH_FAILED")}, false},
		"unknown":     {AttemptEnd{State: "unknown", Outcome: "error", GenerationState: "unknown", FailureCode: protocol.Str("MODEL_GENERATION_OUTCOME_UNKNOWN")}, false},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			r := e.newRun("cp.shown.0000000000000001")
			r.phase("Admitting", "Loading")
			if _, err := e.s.ApplyInput(bg, r.fence); err != nil {
				t.Fatal(err)
			}
			r.phase("Loading", "Assembling")
			r.phase("Assembling", "Measuring")
			r.phase("Measuring", "Generating")
			rsv := r.reserve()
			tc.end.AttemptID = rsv.AttemptID
			if _, err := e.s.RecordAttemptEnd(bg, r.fence, tc.end); err != nil {
				t.Fatal(err)
			}
			snap, err := e.s.LoadSnapshot(bg, r.start.RunID)
			if err != nil || len(snap.Applied) != 1 || snap.Applied[0].Presented != tc.presented {
				t.Fatalf("presented %v, want %v (%v)", snap.Applied[0].Presented, tc.presented, err)
			}
		})
	}
}

// TestABackupAfterACheckpointIsAConsistentRestorePoint is A07 and A28's backup side: the closure
// check of a store that holds a checkpoint finds its bytes, its hash and the thread's pointer
// consistent, and finds it again when its bytes are not the ones the hash says.
func TestABackupAfterACheckpointIsAConsistentRestorePoint(t *testing.T) {
	e := newEnv(t)
	r := e.compactingRun("cp.backup.00000000001")
	r.phase("Compacting", "CommittingCheckpoint")
	in := e.commitInput(r, "rencrow-checkpoint-candidate/v1\x00{}")
	if _, err := e.s.CommitCheckpoint(bg, e.fenceOf(r), in); err != nil {
		t.Fatal(err)
	}
	rec, err := e.s.Backup(bg, privateDir(t, "backups"))
	if err != nil || !rec.Closure.OK || rec.Closure.Counts["checkpoints"] != 1 {
		t.Fatalf("%v %+v", err, rec.Closure)
	}
	mustExec(t, e.s.db, "DROP TRIGGER checkpoints_no_update")
	mustExec(t, e.s.db, "UPDATE checkpoints SET candidate_bytes=x'00'")
	if _, err := e.s.Backup(bg, privateDir(t, "backups2")); err == nil {
		t.Fatal("a backup of a store whose checkpoint does not match its hash was accepted")
	}
}
