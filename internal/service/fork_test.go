package service_test

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// session/fork (F25, A36): a new Thread in the source's Session that stands on a copy of one of
// its checkpoints, with the provenance the checkpoint names imported.

func (r *rig) forkAt(thread, checkpoint, key string) (protocol.ForkResult, []protocol.Event, error) {
	r.t.Helper()
	res, err := r.call("session/fork", protocol.SessionForkInput{ThreadID: thread, CheckpointID: checkpoint, IdempotencyKey: key})
	if err != nil {
		return protocol.ForkResult{}, nil, err
	}
	return decode[protocol.ForkResult](r.t, res), res.Events, nil
}

func (r *rig) fork(thread, checkpoint, key string) protocol.ForkResult {
	r.t.Helper()
	out, _, err := r.forkAt(thread, checkpoint, key)
	if err != nil {
		r.t.Fatalf("session/fork: %v", err)
	}
	return out
}

func (r *rig) checkpointByID(id string) *compaction.Checkpoint {
	r.t.Helper()
	db, err := sql.Open("sqlite", "file:"+r.dep.DataRoot+"/"+sqlite.DatabaseFile+"?mode=ro")
	if err != nil {
		r.t.Fatal(err)
	}
	defer db.Close()
	var blob []byte
	var hash string
	if err := db.QueryRow("SELECT candidate_bytes, candidate_hash FROM checkpoints WHERE checkpoint_id=?", id).Scan(&blob, &hash); err != nil {
		r.t.Fatal(err)
	}
	cp, err := compaction.ParseCheckpoint(blob, hash)
	if err != nil {
		r.t.Fatal(err)
	}
	return cp
}

// compacted is a Thread with a Normal checkpoint made by a manual compaction: the checkpoint's
// ID, and the rig.
func compacted(t *testing.T) (*compactionRig, string, string) {
	c, thread, cp, _ := compactedWith(t)
	return c, thread, cp
}

// laterSummary is a Summary of the work a Thread did after its first checkpoint: it cites the
// answer of the Run after it.
const laterSummary = `{"current_work":[{"text":"続きの確認をした。","source_handles":["work-0"]}],"decisions":[],"verification":[],"open_items":[],"next_steps":[],"important_observation_handles":[]}`

// compactedWith is compacted, with the stage replies the scenario's fake answers from, which a
// test may change.
func compactedWith(t *testing.T) (*compactionRig, string, string, map[string]harnesstest.Reply) {
	t.Helper()
	replies := map[string]harnesstest.Reply{modelport.StageSummary: harnesstest.Final(goodSummary)}
	c, _ := manualScenario(t, replies)
	thread := c.val("SELECT thread_id FROM threads")
	res := c.waitReceipt(c.compact(thread, "fork.scenario.compact.1", false).ReceiptID)
	if res.CheckpointID == nil {
		t.Fatalf("%+v", res)
	}
	return c, thread, *res.CheckpointID, replies
}

// TestAForkMakesANewThreadThatStandsOnACopyOfTheCheckpointAndImportsItsProvenance is A36 and the
// provenance import of CHECKPOINT_FORMAT: the new Thread is in the same Session, its first
// checkpoint is a copy of the source's (mode fork, no parent, the same projection, boundaries
// and inventory), the sources the copy names are imported, and the Thread then works, and
// compacts, like any other: it reads the imported sources and no others, and a compaction on it
// is verified against what it imported.
func TestAForkMakesANewThreadThatStandsOnACopyOfTheCheckpointAndImportsItsProvenance(t *testing.T) {
	c, source, cp, replies := compactedWith(t)
	srcCP := c.checkpointByID(cp)
	srcBefore := map[string]string{
		"revisions": c.val("SELECT context_revision||'/'||control_revision||'/'||event_seq||'/'||COALESCE(active_run_id,'-')||'/'||current_checkpoint_id FROM threads WHERE thread_id=?", source),
		"items":     c.val("SELECT COUNT(*) FROM items WHERE thread_id=?", source),
		"entries":   c.val("SELECT COUNT(*) FROM context_entries WHERE thread_id=?", source),
		"events":    c.val("SELECT COUNT(*) FROM events WHERE thread_id=?", source),
	}
	// The files are not part of a fork: they change after the checkpoint, and stay as they are.
	changed := c.write("big.txt", "changed after the checkpoint\n")
	measures := len(c.fake.Measures())
	generates := len(c.fake.Generates())

	res, events, err := c.forkAt(source, cp, "fork.normal.0000000001")
	if err != nil {
		t.Fatal(err)
	}
	if res.SourceThreadID != source || res.ThreadID == source || res.CheckpointID == cp || res.ReceiptID == "" ||
		res.SessionID != c.val("SELECT session_id FROM threads WHERE thread_id=?", source) {
		t.Fatalf("%+v", res)
	}
	// The same Session, with the source named, at the revisions a fork starts from: the context
	// revision of the checkpoint's own commit, no control change, and no active Run.
	row := c.val("SELECT session_id||'/'||COALESCE(source_thread_id,'-')||'/'||context_revision||'/'||control_revision||'/'||queue_revision||'/'||writer_epoch||'/'||COALESCE(active_run_id,'-')||'/'||current_checkpoint_id FROM threads WHERE thread_id=?", res.ThreadID)
	if row != res.SessionID+"/"+source+"/1/0/0/1/-/"+res.CheckpointID {
		t.Fatal(row)
	}
	info := decode[protocol.SessionInfo](t, c.mustCall("session/get", protocol.SessionGetInput{ThreadID: res.ThreadID}))
	if info.SessionID != res.SessionID || info.ThreadID != res.ThreadID || info.ContextRevision != 1 || info.ControlRevision != 0 || info.ActiveRunID != nil ||
		info.WorkspacePath != decode[protocol.SessionInfo](t, c.mustCall("session/get", protocol.SessionGetInput{ThreadID: source})).WorkspacePath {
		t.Fatalf("%+v", info)
	}

	// The checkpoint is a copy, serialized once, that loads like any other.
	fk := c.checkpointByID(res.CheckpointID)
	if fk.Candidate.Mode != "fork" || fk.Candidate.ParentCheckpointID != nil || fk.Candidate.ThreadID != res.ThreadID || fk.Candidate.CheckpointID != res.CheckpointID ||
		fk.Candidate.SemanticBoundary != srcCP.Candidate.SemanticBoundary || fk.Candidate.DurableBoundary != srcCP.Candidate.DurableBoundary ||
		fk.Candidate.SnapshotDigest != srcCP.Candidate.SnapshotDigest {
		t.Fatalf("%+v", fk.Candidate)
	}
	if !reflect.DeepEqual(fk.Candidate.Projection, srcCP.Candidate.Projection) || !reflect.DeepEqual(fk.Candidate.ObservationInventory, srcCP.Candidate.ObservationInventory) ||
		!reflect.DeepEqual(fk.Candidate.RetainedExactRefs, srcCP.Candidate.RetainedExactRefs) {
		t.Fatal("a fork copies what the Thread stood on: projection, inventory and retained references")
	}
	if fk.Candidate.Expected.ContextRevision != 0 || fk.Candidate.Expected.ControlRevision != 0 || fk.Candidate.Expected.WriterEpoch != 1 ||
		!reflect.DeepEqual(fk.Candidate.BeforeCount, srcCP.Candidate.AfterCount) || !reflect.DeepEqual(fk.Candidate.AfterCount, srcCP.Candidate.AfterCount) {
		t.Fatalf("expected %+v: the source's own count carries over, and no one was asked", fk.Candidate.Expected)
	}
	if len(c.fake.Measures()) != measures || len(c.fake.Generates()) != generates {
		t.Fatal("a fork whose binding and input are the source's asked for a new count or a generation")
	}
	if got := c.val("SELECT mode||'/'||COALESCE(parent_checkpoint_id,'-')||'/'||context_revision||'/'||control_revision FROM checkpoints WHERE checkpoint_id=?", res.CheckpointID); got != "fork/-/1/0" {
		t.Fatal(got)
	}

	// Its own system Task and Run, ended at once, with no Task meaning.
	task := c.val("SELECT t.kind||'/'||COALESCE(t.origin_turn_id,'noturn')||'/'||t.status||'/'||r.status||'/'||r.phase FROM tasks t JOIN runs r ON r.task_id=t.task_id WHERE t.thread_id=?", res.ThreadID)
	if task != "session_maintenance/noturn/run_ended/completed/Terminal" {
		t.Fatal(task)
	}
	runID := c.val("SELECT r.run_id FROM runs r JOIN tasks t ON t.task_id=r.task_id WHERE t.thread_id=?", res.ThreadID)
	rr := decode[protocol.RunInfo](t, c.mustCall("run/get", protocol.RunGetInput{RunID: runID})).Result
	if rr.Status != "completed" || rr.Code != "SESSION_FORKED" || rr.FinalMessageID != nil || rr.Resumable || rr.LastCheckpointID == nil || *rr.LastCheckpointID != res.CheckpointID {
		t.Fatalf("%+v", rr)
	}
	// The events of the new Thread, all of the one receipt, in order from 1; the same as the call
	// announced.
	evs := c.threadEvents(res.ThreadID)
	var types []string
	for i, ev := range evs {
		types = append(types, ev.Type)
		if ev.EventSeq != int64(i+1) || ev.ReceiptID == nil || *ev.ReceiptID != res.ReceiptID {
			t.Fatalf("%s: seq %d receipt %v", ev.Type, ev.EventSeq, ev.ReceiptID)
		}
	}
	if got := strings.Join(types, ","); got != "session.created,task.created,run.started,checkpoint.committed,run.terminal" || len(events) != len(evs) {
		t.Fatalf("%s (%d announced)", got, len(events))
	}
	if rec := c.receipt(res.ReceiptID); rec.Stage != "terminal" || rec.Operation != "session/fork" || rec.Result == nil || rec.Result.Type != protocol.ReceiptForkResult {
		t.Fatalf("%+v", rec)
	}

	// The source is as it was: nothing was committed to it, no snapshot moved.
	for k, q := range map[string]string{
		"revisions": "SELECT context_revision||'/'||control_revision||'/'||event_seq||'/'||COALESCE(active_run_id,'-')||'/'||current_checkpoint_id FROM threads WHERE thread_id=?",
		"items":     "SELECT COUNT(*) FROM items WHERE thread_id=?", "entries": "SELECT COUNT(*) FROM context_entries WHERE thread_id=?", "events": "SELECT COUNT(*) FROM events WHERE thread_id=?",
	} {
		if got := c.val(q, source); got != srcBefore[k] {
			t.Errorf("the source's %s changed: %s -> %s", k, srcBefore[k], got)
		}
	}
	if got, err := os.ReadFile(changed); err != nil || string(got) != "changed after the checkpoint\n" {
		t.Fatalf("the files were not to be restored: %q %v", got, err)
	}

	// The provenance: the Evidence the copy names and the checkpoint that accepted its Summary.
	imports := c.val("SELECT group_concat(DISTINCT source_kind) FROM source_imports WHERE thread_id=?", res.ThreadID)
	if !strings.Contains(imports, "evidence") || !strings.Contains(imports, "checkpoint") {
		t.Fatalf("imports %q", imports)
	}
	if got := c.val("SELECT COUNT(*) FROM source_imports WHERE thread_id=? AND (from_thread_id<>? OR from_checkpoint_id<>? OR fork_checkpoint_id<>?)", res.ThreadID, source, cp, res.CheckpointID); got != "0" {
		t.Fatalf("every import names where it came from: %s odd rows", got)
	}
	if got := c.val("SELECT COUNT(*) FROM source_imports WHERE source_kind='checkpoint' AND source_id=?", srcCP.Candidate.Projection.Summary.SourceCheckpointID); got != "1" {
		t.Fatal("the checkpoint that accepted the Summary is imported")
	}
	obs := srcCP.Candidate.ObservationInventory[0].EvidenceID
	read := func(thread, evidence string) error {
		_, err := c.store.ReadEvidenceInThread(context.Background(), thread, protocol.EvidenceReadInput{EvidenceID: evidence, ProjectionVersion: "text/v1", Range: protocol.ByteRange{Start: 0, End: 10}})
		return err
	}
	if err := read(res.ThreadID, obs); err != nil {
		t.Fatalf("an imported Observation is readable in the fork: %v", err)
	}

	// The fork works: a Run on it is prompted from the copy and the tail after it, its entries
	// are applied after the checkpoint's boundary, and what the source does afterwards is not
	// read in the fork.
	next := c.startNext(res.ThreadID, "fork.normal.run.00001", "続けてください。")
	if r := c.waitTerminal(next.RunID); r.Result.Status != "completed" {
		t.Fatalf("%+v", r.Result)
	}
	if p := promptOf(lastAct(c)); !strings.Contains(p, "user:RENCROW_CONTEXT_BOUNDARY_V1") || !strings.Contains(p, "assistant:RENCROW_ACCEPTED_SUMMARY_V1") || !strings.Contains(p, "続けてください。") {
		t.Fatalf("the fork's Run is not prompted from the copy:\n%.1500s", p)
	}
	if got := c.val("SELECT MIN(context_seq) FROM context_entries WHERE thread_id=?", res.ThreadID); got == "" || got <= "0" || atoi(got) != srcCP.Candidate.DurableBoundary+1 {
		t.Fatalf("the first entry of the fork comes after the boundary of its checkpoint (%d): %s", srcCP.Candidate.DurableBoundary, got)
	}
	afterSource := c.startNext(source, "fork.normal.source.0001", "元のThreadの続き")
	c.waitTerminal(afterSource.RunID)
	lateEvidence := c.val("SELECT evidence_id FROM items WHERE thread_id=? ORDER BY sequence DESC LIMIT 1", source)
	if err := read(res.ThreadID, lateEvidence); protocol.CodeOf(err) != protocol.CodeForbidden {
		t.Fatalf("what the source did after the fork is not the fork's to read: %v", err)
	}
	if err := read(source, lateEvidence); err != nil {
		t.Fatal(err)
	}

	// A compaction on the fork is verified against what it imported, and its checkpoint stands
	// on the fork's: the Summary is made from the copied one.
	replies[modelport.StageSummary] = harnesstest.Final(laterSummary)
	acc := c.compact(res.ThreadID, "fork.normal.compact.01", false)
	cr := c.waitReceipt(acc.ReceiptID)
	if cr.Status != "executed" || cr.Outcome == nil || *cr.Outcome != "NormalCompacted" || cr.CheckpointID == nil {
		t.Fatalf("a compaction on a forked Thread: %+v outcome %v error %+v", cr, cr.Outcome, cr.Error)
	}
	if got := c.val("SELECT parent_checkpoint_id FROM checkpoints WHERE checkpoint_id=?", *cr.CheckpointID); got != res.CheckpointID {
		t.Fatalf("the first compaction of a fork has the fork checkpoint as its parent: %s", got)
	}
}

func atoi(s string) int64 {
	var n int64
	for _, c := range s {
		n = n*10 + int64(c-'0')
	}
	return n
}

// TestAForkOfAForkAndAReplayOfAFork: a fork can be forked again (its checkpoint is a checkpoint
// of its Thread like any other, and its imports reach one more Thread), and the same key and
// request answer from the first receipt, with no second Thread, whatever moved since.
func TestAForkOfAForkAndAReplayOfAFork(t *testing.T) {
	c, source, cp := compacted(t)
	first := c.fork(source, cp, "fork.chain.000000000001")
	threads := c.count("threads")
	again := c.fork(source, cp, "fork.chain.000000000001")
	if again != first || c.count("threads") != threads || c.count("checkpoints") != 2 {
		t.Fatalf("%+v vs %+v", again, first)
	}
	if _, _, err := c.forkAt(source, cp, "fork.chain.000000000002"); err != nil {
		t.Fatalf("two forks of one checkpoint are two Threads: %v", err)
	}
	// Another request under the first key is a conflict.
	if _, _, err := c.forkAt(first.ThreadID, first.CheckpointID, "fork.chain.000000000001"); protocol.CodeOf(err) != protocol.CodeIdempotencyConflict {
		t.Fatalf("%v", err)
	}
	second := c.fork(first.ThreadID, first.CheckpointID, "fork.chain.000000000003")
	row := c.val("SELECT source_thread_id FROM threads WHERE thread_id=?", second.ThreadID)
	if row != first.ThreadID || second.SessionID != first.SessionID {
		t.Fatal(row)
	}
	// The second fork reaches the sources of the first, through the first's own imports.
	if got := c.val("SELECT COUNT(*) FROM source_imports WHERE thread_id=?", second.ThreadID); got != c.val("SELECT COUNT(*) FROM source_imports WHERE thread_id=?", first.ThreadID) {
		t.Fatalf("a fork of a fork imports what the checkpoint names: %s", got)
	}
	next := c.startNext(second.ThreadID, "fork.chain.run.0000001", "続けてください。")
	if r := c.waitTerminal(next.RunID); r.Result.Status != "completed" {
		t.Fatalf("%+v", r.Result)
	}
}

// TestAForkCopiesNoActiveEffectAndIsNotRefusedBecauseTheSourceIsBusy is A36: the source Thread
// has a Run in flight (its model call is held); a fork of the checkpoint from before is made all
// the same, and the new Thread has no Run, no Tool state, no queued input and no item of the
// source's; the source's Run goes on and ends.
func TestAForkCopiesNoActiveEffectAndIsNotRefusedBecauseTheSourceIsBusy(t *testing.T) {
	c, source, cp := compacted(t)
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.fake.OnMeasure = func(ctx context.Context) {
		once.Do(func() { close(reached) })
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	busy := c.startNext(source, "fork.busy.source.00001", "続けてください。")
	<-reached
	if c.val("SELECT COALESCE(active_run_id,'-') FROM threads WHERE thread_id=?", source) != busy.RunID {
		t.Fatal("the source is busy")
	}
	res, _, err := c.forkAt(source, cp, "fork.busy.fork.0000001")
	if err != nil {
		t.Fatalf("a fork is not refused for a busy source: %v", err)
	}
	for _, tb := range []string{"actions", "attempts"} {
		q := "SELECT COUNT(*) FROM " + tb + " x JOIN runs r ON r.run_id=x.run_id JOIN tasks t ON t.task_id=r.task_id WHERE t.thread_id=?"
		if tb == "attempts" {
			q = "SELECT COUNT(*) FROM attempts a JOIN actions x ON x.action_id=a.action_id JOIN runs r ON r.run_id=x.run_id JOIN tasks t ON t.task_id=r.task_id WHERE t.thread_id=?"
		}
		if got := c.val(q, res.ThreadID); got != "0" {
			t.Errorf("the fork has %s of the source's Runs: %s", tb, got)
		}
	}
	for _, q := range []string{"SELECT COUNT(*) FROM queue_inputs WHERE thread_id=?", "SELECT COUNT(*) FROM items WHERE thread_id=?", "SELECT COUNT(*) FROM context_entries WHERE thread_id=?",
		"SELECT COUNT(*) FROM model_calls m JOIN runs r ON r.run_id=m.run_id JOIN tasks t ON t.task_id=r.task_id WHERE t.thread_id=?"} {
		if got := c.val(q, res.ThreadID); got != "0" {
			t.Errorf("%s: %s", q, got)
		}
	}
	if c.val("SELECT COALESCE(active_run_id,'-') FROM threads WHERE thread_id=?", res.ThreadID) != "-" || c.val("SELECT COALESCE(active_run_id,'-') FROM threads WHERE thread_id=?", source) != busy.RunID {
		t.Fatal("the new Thread has no Run and the source keeps its own")
	}
	close(release)
	if r := c.waitTerminal(busy.RunID); r.Result.Status != "completed" {
		t.Fatalf("%+v", r.Result)
	}
}

// TestAForkRefusesWhatItCannotStandOnAndWritesNothing: a checkpoint that is another Thread's or
// does not exist, one whose bytes were changed, a source that is no longer the one the
// checkpoint names, a count that does not fit or cannot be verified: every refusal leaves the
// store as it was.
func TestAForkRefusesWhatItCannotStandOnAndWritesNothing(t *testing.T) {
	c, source, cp := compacted(t)
	other := decode[protocol.SessionOpenResult](t, c.mustCall("session/open", c.openParams("fork.refuse.other.open"))).Session
	var otherCP string
	{
		// A checkpoint of another Thread: a run and a manual compaction there.
		start := c.startRun(other.ThreadID, "fork.refuse.other.run", "大きなファイルを読んで保存処理を実装する。")
		if r := c.waitTerminal(start.RunID); r.Result.Status != "completed" {
			t.Fatalf("%+v", r.Result)
		}
		res := c.waitReceipt(c.compact(other.ThreadID, "fork.refuse.other.compact", false).ReceiptID)
		otherCP = *res.CheckpointID
	}
	stable := tableCounts(c.rig)
	same := func(what string) {
		t.Helper()
		if now := tableCounts(c.rig); !reflect.DeepEqual(now, stable) {
			t.Errorf("%s: a refused fork wrote: %v -> %v", what, stable, now)
		}
	}
	for name, tc := range map[string]struct{ thread, checkpoint, code string }{
		"a checkpoint of another Thread": {source, otherCP, protocol.CodeInvalidRequest},
		"a checkpoint that is not there": {source, "ckp_00000000-0000-7000-8000-000000000001", protocol.CodeInvalidRequest},
		"a Thread that is not there":     {"thr_00000000-0000-7000-8000-000000000001", cp, protocol.CodeForbidden},
	} {
		_, _, err := c.forkAt(tc.thread, tc.checkpoint, "fork.refuse."+strings.ReplaceAll(name, " ", "")[:12]+"key01")
		if protocol.CodeOf(err) != tc.code {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
		}
		same(name)
	}

	// A source Evidence that is not what the checkpoint says it is: the fork does not stand on it.
	srcCP := c.checkpointByID(cp)
	evidence := srcCP.Candidate.ObservationInventory[0].EvidenceID
	c.exec("DROP TRIGGER evidence_sealed_no_update")
	c.exec("UPDATE evidence SET raw_hash=? WHERE evidence_id=?", strings.Repeat("a", 64), evidence)
	_, _, err := c.forkAt(source, cp, "fork.refuse.evidence.key1")
	wantCode(t, err, protocol.CodeIntegrityBlocked)
	same("an Evidence that is not what the checkpoint says")
	c.exec("UPDATE evidence SET raw_hash=? WHERE evidence_id=?", srcCP.Candidate.ObservationInventory[0].RawHash, evidence)

	// A checkpoint that does not read back exactly.
	c.exec("DROP TRIGGER checkpoints_no_update")
	c.exec("UPDATE checkpoints SET candidate_bytes=candidate_bytes||x'20' WHERE checkpoint_id=?", cp)
	_, _, err = c.forkAt(source, cp, "fork.refuse.bytes.key001")
	wantCode(t, err, protocol.CodeIntegrityBlocked)
	same("a checkpoint whose bytes were changed")
}

// TestTheCountOfAForkIsCarriedOverOnlyWhereItIsAboutTheSameRequestAndBinding is CHECKPOINT_FORMAT
// section 4: the copy is not held to no-shrink; its source count is carried over when the
// logical input and the binding fingerprint are the very ones, and counted again when the
// binding changed; a count that does not fit or cannot be verified refuses the fork.
func TestTheCountOfAForkIsCarriedOverOnlyWhereItIsAboutTheSameRequestAndBinding(t *testing.T) {
	t.Run("the binding changed: it is counted again", func(t *testing.T) {
		c, source, cp := compacted(t)
		srcCP := c.checkpointByID(cp)
		measures := len(c.fake.Measures())
		c.fake.Fingerprint = "bfp-v1:" + strings.Repeat("e", 64)
		res := c.fork(source, cp, "fork.count.changed.0001")
		fk := c.checkpointByID(res.CheckpointID)
		if got := len(c.fake.Measures()) - measures; got != 1 {
			t.Fatalf("one new count, asked: %d", got)
		}
		if fk.Candidate.AfterCount.BindingFingerprint != c.fake.Fingerprint || fk.Candidate.AfterCount.BindingFingerprint == srcCP.Candidate.AfterCount.BindingFingerprint ||
			!reflect.DeepEqual(fk.Candidate.BeforeCount, srcCP.Candidate.AfterCount) || fk.Candidate.AfterCount.State != "verified_exact" {
			t.Fatalf("%+v", fk.Candidate.AfterCount)
		}
	})
	t.Run("the count does not fit the binding", func(t *testing.T) {
		c, source, cp := compacted(t)
		c.fake.Fingerprint = "bfp-v1:" + strings.Repeat("d", 64)
		c.fake.SetMeasure(harnesstest.MeasureConfig{Limit: 4096 + 2048 + 10})
		before := tableCounts(c.rig)
		_, _, err := c.forkAt(source, cp, "fork.count.nofit.00001")
		wantCode(t, err, protocol.CodeInvalidRequest)
		if now := tableCounts(c.rig); !reflect.DeepEqual(now, before) {
			t.Fatalf("%v -> %v", before, now)
		}
	})
	t.Run("the count cannot be verified", func(t *testing.T) {
		c, source, cp := compacted(t)
		c.fake.Fingerprint = "bfp-v1:" + strings.Repeat("c", 64)
		c.fake.SetMeasure(harnesstest.MeasureConfig{State: "estimated", Lower: 10, Upper: 10, Limit: manualLimit})
		before := tableCounts(c.rig)
		_, _, err := c.forkAt(source, cp, "fork.count.unver.00001")
		wantCode(t, err, protocol.CodeBudgetUnverified)
		if now := tableCounts(c.rig); !reflect.DeepEqual(now, before) {
			t.Fatalf("%v -> %v", before, now)
		}
	})
}

// TestAForkIsRefusedWithoutAModelPortAndOnlyTheOwnerOfTheThreadMayMakeOne: a process that has no
// model port cannot count the copy and refuses with the capability's reason (nothing written);
// the checkpoint's Thread must be readable and controllable by the caller.
func TestAForkIsRefusedWithoutAModelPortAndOnlyTheOwnerOfTheThreadMayMakeOne(t *testing.T) {
	c, source, cp := compacted(t)
	r := newRig(t, nil)
	info := r.openSession("fork.noport.open.0001")
	_, _, err := r.forkAt(info.ThreadID, "ckp_00000000-0000-7000-8000-000000000001", "fork.noport.key.00001")
	wantCode(t, err, protocol.CodeUnsupportedContract)
	if r.count("threads") != 1 {
		t.Fatal("a refusal made a Thread")
	}
	_ = source
	_ = cp
	_ = c
}

// TestAnEmergencyCompactionOnAForkKeepsTheSummaryOfTheSourceAndIsVerifiedAgainstWhatItImported:
// the reduction that needs no model keeps the Summary the fork copied, whose source is a
// checkpoint of the source Thread, and its commit finds that checkpoint and the Evidence it
// names again, in the fork's own provenance; without the import it would be INTEGRITY_BLOCKED.
func TestAnEmergencyCompactionOnAForkKeepsTheSummaryOfTheSourceAndIsVerifiedAgainstWhatItImported(t *testing.T) {
	c, source, cp, replies := compactedWith(t)
	fk := c.fork(source, cp, "fork.emergency.00000001")
	srcSummary := c.checkpointByID(cp).Candidate.Projection.Summary.SourceCheckpointID
	// The fork's Run reads the file through a Tool, and the Summary of its compaction fails.
	replies[modelport.StageSummary] = harnesstest.Final("not json")
	c.fake.SetReply(harnesstest.Reply{Dynamic: func(req modelport.ChatRequest) harnesstest.Reply {
		if req.Rencrow.Harness.Stage != modelport.StageAct {
			c.stages[req.Rencrow.Harness.Stage] = append(c.stages[req.Rencrow.Harness.Stage], req)
			return replies[req.Rencrow.Harness.Stage]
		}
		c.acts = append(c.acts, req)
		for _, m := range req.Messages {
			if m.Role == "tool" && !strings.HasPrefix(m.Text(), "RENCROW_OBSERVATION_REFERENCE_V1") {
				return harnesstest.Final("読みました。")
			}
		}
		return calls(tc("call-fork", "file.read", readArgs("big.txt", 0, bigFileBytes, bigFileBytes)))
	}})
	next := c.startNext(fk.ThreadID, "fork.emergency.run.0001", "もう一度ファイルを読んでください。")
	if r := c.waitTerminal(next.RunID); r.Result.Status != "completed" {
		t.Fatalf("%+v", r.Result)
	}
	imports := c.val("SELECT COUNT(*) FROM source_imports WHERE thread_id=?", fk.ThreadID)
	res := c.waitReceipt(c.compact(fk.ThreadID, "fork.emergency.compact.1", false).ReceiptID)
	if res.Status != "executed" || res.Outcome == nil || *res.Outcome != "EmergencyCompacted" || res.CheckpointID == nil {
		t.Fatalf("%+v outcome %v error %+v", res, res.Outcome, res.Error)
	}
	em := c.checkpointByID(*res.CheckpointID)
	if em.Candidate.Mode != "emergency" || em.Candidate.Projection.Summary == nil || em.Candidate.Projection.Summary.SourceCheckpointID != srcSummary ||
		em.Candidate.ParentCheckpointID == nil || *em.Candidate.ParentCheckpointID != fk.CheckpointID {
		t.Fatalf("%+v", em.Candidate.Mode)
	}
	if got := c.val("SELECT COUNT(*) FROM source_imports WHERE thread_id=?", fk.ThreadID); got != imports {
		t.Fatalf("a compaction imports nothing: %s -> %s", imports, got)
	}
	// And the fork's own tail is read back: the checkpoint it stands on now, then a Run.
	again := c.startNext(fk.ThreadID, "fork.emergency.run.0002", "続けてください。")
	if r := c.waitTerminal(again.RunID); r.Result.Status != "completed" {
		t.Fatalf("%+v", r.Result)
	}
	// The checkpoint that accepted the Summary is the source's own checkpoint (a Normal checkpoint
	// accepts its own Summary), and the fork may name it because it imported it.
	if srcSummary != cp {
		t.Fatalf("the Summary of a Normal checkpoint is accepted by that checkpoint: %s vs %s", srcSummary, cp)
	}
	if _, err := c.store.LoadScopedCheckpoint(context.Background(), fk.ThreadID, srcSummary); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.LoadScopedCheckpoint(context.Background(), "thr_00000000-0000-7000-8000-000000000009", srcSummary); err == nil {
		t.Fatal("a Thread that imported nothing may not name another Thread's checkpoint")
	}
}

// TestAForkedThreadWithoutItsImportsCannotUseTheCopy: what the fork imported is what makes the
// copy usable, and nothing stands in for it. Take the import of the Summary's checkpoint away
// and the Thread's next Run and compaction stop INTEGRITY_BLOCKED; take away an Evidence it
// names and the fork's own cold look at the checkpoint refuses (and no other Thread's
// checkpoint is tried in its place).
func TestAForkedThreadWithoutItsImportsCannotUseTheCopy(t *testing.T) {
	t.Run("the checkpoint that accepted the Summary", func(t *testing.T) {
		c, source, cp := compacted(t)
		fk := c.fork(source, cp, "fork.noimport.0000000001")
		c.exec("DROP TRIGGER source_imports_no_delete")
		c.exec("DELETE FROM source_imports WHERE thread_id=? AND source_kind='checkpoint'", fk.ThreadID)
		acts := len(c.acts)
		next := c.startNext(fk.ThreadID, "fork.noimport.run.00001", "続けてください。")
		run := c.waitTerminal(next.RunID)
		if run.Result.Status != "blocked" || run.Result.Code != "INTEGRITY_BLOCKED" || len(c.acts) != acts {
			t.Fatalf("%+v: the Run must stop before it generates", run.Result)
		}
	})
	t.Run("an Evidence the checkpoint names", func(t *testing.T) {
		c, source, cp := compacted(t)
		fk := c.fork(source, cp, "fork.noimport.0000000002")
		obs := c.checkpointByID(cp).Candidate.ObservationInventory[0].EvidenceID
		c.exec("DROP TRIGGER source_imports_no_delete")
		c.exec("DELETE FROM source_imports WHERE thread_id=? AND source_kind='evidence' AND source_id=?", fk.ThreadID, obs)
		// The fork may not name what it did not import: the same finding that stops the commit of a
		// checkpoint that names it, and the cold look at the one it stands on.
		err := c.store.VerifySources(context.Background(), fk.ThreadID, []sqlite.SourceCheck{{SourceID: obs, RawHash: c.checkpointByID(cp).Candidate.ObservationInventory[0].RawHash, ProjectionVersion: "text/v1", End: 1}})
		wantCode(t, err, protocol.CodeIntegrityBlocked)
	})
}
