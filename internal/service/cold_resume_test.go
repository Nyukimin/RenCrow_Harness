package service_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Cold resume (run/resume with a checkpoint, H03, H11 and H26 on their cold path): the
// checkpoint a Thread stands on is read back from the store exactly, before a new Run is
// admitted to continue it, and what does not read back stops the resume; no older checkpoint is
// used in its place.

type coldRig struct {
	*compactionRig
	thread, taskID, lastRun string
	cps                     []string // the Thread's checkpoints, oldest first
	hang                    bool
}

// coldScenario is a Thread with two checkpoints (two compactions inside one Run, the second
// the child of the first) whose Run was stopped at its third step: a Task whose last Run is
// cancelled and resumable (no generation of its own is left unknown), on a Thread that stands
// on the second checkpoint. The Run also carried a typed context block, which the cold look
// reads back with the rest of the Thread's applied context.
func coldScenario(t *testing.T) *coldRig {
	t.Helper()
	fake := harnesstest.NewFake()
	fake.SetMeasure(harnesstest.MeasureConfig{Limit: contextLimit})
	c := &compactionRig{fake: fake, stages: map[string][]modelport.ChatRequest{}}
	r, rec := toolRig(t, fake, toolConfig{tools: []string{"file.read", "evidence.read"}})
	r.write("big.txt", bigFile())
	r.write("big2.txt", bigFile())
	c.rig, c.rec = r, rec
	cr := &coldRig{compactionRig: c, hang: true}
	reached := make(chan struct{})
	var once sync.Once
	// The Run is stopped where no generation is in flight: while its third prompt is counted.
	fake.OnMeasure = func(ctx context.Context) {
		if cr.hang && len(c.checkpoints()) >= 2 {
			once.Do(func() { close(reached) })
			<-ctx.Done()
		}
	}
	fake.SetReply(harnesstest.Reply{Dynamic: func(req modelport.ChatRequest) harnesstest.Reply {
		if st := req.Rencrow.Harness.Stage; st != modelport.StageAct {
			c.stages[st] = append(c.stages[st], req)
			return harnesstest.Final(goodSummary)
		}
		c.acts = append(c.acts, req)
		for _, m := range req.Messages {
			if m.Role == "tool" && !strings.HasPrefix(m.Text(), "RENCROW_OBSERVATION_REFERENCE_V1") {
				return harnesstest.Final("読み終えました。")
			}
		}
		switch n := len(c.checkpoints()); {
		case n == 0:
			return calls(tc("call-1", "file.read", readArgs("big.txt", 0, bigFileBytes, bigFileBytes)))
		case n == 1:
			return calls(tc("call-2", "file.read", readArgs("big2.txt", 0, bigFileBytes, bigFileBytes)))
		}
		return harnesstest.Final("再開して終えました。")
	}})
	info := c.openSession("cold.scenario.open.1")
	const blockText = "安定した前提: ファイルは読み取り専用で扱う。"
	rev, err := protocol.ContextRevision(protocol.KindStableRuntimeContext, blockText, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := c.startRun(info.ThreadID, "cold.scenario.start.1", "大きなファイルを二つ読んで保存処理を実装する。", func(in *protocol.StartInput) {
		in.ContextBlocks = []protocol.ContextBlock{{Kind: protocol.KindStableRuntimeContext, Text: blockText, Revision: rev}}
	})
	<-reached
	cr.thread, cr.taskID, cr.lastRun = info.ThreadID, start.TaskID, start.RunID
	if rec := c.interrupt(start.RunID, 0, "cold.scenario.stop.0001"); rec.Code != "CANCEL_REQUESTED" {
		t.Fatalf("%+v", rec)
	}
	run := c.waitTerminal(start.RunID)
	if run.Result.Status != "cancelled" || !run.Result.Resumable || run.Result.LastCheckpointID == nil || len(run.Result.UnresolvedActionIDs) != 0 {
		t.Fatalf("%+v", run.Result)
	}
	for _, row := range c.checkpoints() {
		cr.cps = append(cr.cps, strings.SplitN(row, "/", 2)[0])
	}
	if len(cr.cps) != 2 || c.val("SELECT parent_checkpoint_id FROM checkpoints WHERE checkpoint_id=?", cr.cps[1]) != cr.cps[0] ||
		c.val("SELECT current_checkpoint_id FROM threads") != cr.cps[1] {
		t.Fatalf("checkpoints %v", cr.cps)
	}
	cr.hang = false
	return cr
}

func (c *coldRig) resumeInput(checkpoint *string, key string) protocol.ResumeInput {
	_, ctl := c.revisions(c.thread)
	return protocol.ResumeInput{TaskID: c.taskID, ExpectedLastRunID: c.lastRun, CheckpointID: checkpoint, ExpectedControlRevision: ctl, IdempotencyKey: key, Limits: startLimits}
}

func (c *coldRig) resume(in protocol.ResumeInput) (protocol.ResumeResult, error) {
	c.t.Helper()
	res, err := c.call("run/resume", in)
	if err != nil {
		return protocol.ResumeResult{}, err
	}
	res.Done()
	return decode[protocol.ResumeResult](c.t, res), nil
}

// reencode changes a stored checkpoint's bytes by f and stores the hash of the new bytes in
// the row too: the blob is internally consistent with its hash, and what is wrong with it is
// only that it is not the canonical form (or not the checkpoint its row describes).
func (c *coldRig) reencode(id string, f func(string) string) {
	c.t.Helper()
	db := c.rw()
	var blob []byte
	if err := db.QueryRow("SELECT candidate_bytes FROM checkpoints WHERE checkpoint_id=?", id).Scan(&blob); err != nil {
		c.t.Fatal(err)
	}
	changed := f(string(blob))
	if changed == string(blob) {
		c.t.Fatal("the change did nothing")
	}
	sum := sha256.Sum256([]byte(changed))
	if _, err := db.Exec("UPDATE checkpoints SET candidate_bytes=?, candidate_hash=? WHERE checkpoint_id=?", []byte(changed), hex.EncodeToString(sum[:]), id); err != nil {
		c.t.Fatal(err)
	}
}

// TestAColdResumeStandsOnTheCheckpointTheThreadStandsOn: a resume that names the Thread's current
// checkpoint, and one that names none, admit a new Run that stands on it (the result names it),
// is prompted from it without compacting again, and ends.
func TestAColdResumeStandsOnTheCheckpointTheThreadStandsOn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		named func(c *coldRig) *string
	}{
		{"naming the checkpoint", func(c *coldRig) *string { return protocol.Str(c.cps[1]) }},
		{"naming none", func(*coldRig) *string { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := coldScenario(t)
			summaries := len(c.stages[modelport.StageSummary])
			in := c.resumeInput(tc.named(c), "cold.resume.key.000001")
			res, err := c.resume(in)
			if err != nil {
				t.Fatal(err)
			}
			if res.CheckpointID == nil || *res.CheckpointID != c.cps[1] || res.PreviousRunID != c.lastRun || res.TaskID != c.taskID {
				t.Fatalf("%+v", res)
			}
			run := c.waitTerminal(res.RunID)
			if run.Result.Status != "completed" || run.Result.FinalText != "再開して終えました。" || len(c.stages[modelport.StageSummary]) != summaries || len(c.checkpoints()) != 2 {
				t.Fatalf("%+v summaries %d checkpoints %v", run.Result, len(c.stages[modelport.StageSummary]), c.checkpoints())
			}
			if p := promptOf(lastAct(c.compactionRig)); !strings.Contains(p, "user:RENCROW_CONTEXT_BOUNDARY_V1") || !strings.Contains(p, "assistant:RENCROW_ACCEPTED_SUMMARY_V1") {
				t.Fatalf("the resumed Run is not prompted from the checkpoint:\n%.1500s", p)
			}
			// The same request again is the first answer.
			if again, err := c.resume(in); err != nil || again.RunID != res.RunID {
				t.Fatalf("%+v %v", again, err)
			}
		})
	}
}

// TestAColdResumeThatNamesACheckpointTheThreadDoesNotStandOnIsRefusedAndNeverRewinds: the older
// checkpoint exists and is healthy; a resume on it is not made (the Thread is not rewound
// silently: to work from an older checkpoint is to fork it), and nothing is written.
func TestAColdResumeThatNamesACheckpointTheThreadDoesNotStandOnIsRefusedAndNeverRewinds(t *testing.T) {
	c := coldScenario(t)
	before := tableCounts(c.rig)
	i := 0
	for name, id := range map[string]string{
		"the older checkpoint of the Thread": c.cps[0],
		"a checkpoint that does not exist":   "ckp_00000000-0000-7000-8000-000000000001",
	} {
		i++
		_, err := c.resume(c.resumeInput(protocol.Str(id), "cold.rewind.key.000000"+string(rune('0'+i))))
		if protocol.CodeOf(err) != protocol.CodeRevisionConflict {
			t.Errorf("%s: %v", name, err)
		}
	}
	if now := tableCounts(c.rig); now["runs"] != before["runs"] || now["receipts"] != before["receipts"] || now["events"] != before["events"] ||
		c.val("SELECT current_checkpoint_id FROM threads") != c.cps[1] {
		t.Fatalf("%v -> %v", before, now)
	}
}

// TestAColdResumeOfACheckpointThatDoesNotReadBackIsIntegrityBlockedAndNeverFallsBack is H03, H11
// and H26 on their cold path: bytes changed, a hash that is not the bytes', another
// serialization with a hash that matches it, a metadata key omitted, a row that does not
// describe its checkpoint, a source Evidence that is not what the checkpoint names, a typed
// block that is not what was stored: each is INTEGRITY_BLOCKED at the admission, with no Run,
// no receipt and no event, and the healthy checkpoint behind the one that fails is not used
// (naming it is a conflict, naming none is the same refusal).
func TestAColdResumeOfACheckpointThatDoesNotReadBackIsIntegrityBlockedAndNeverFallsBack(t *testing.T) {
	type tamper struct {
		name string
		do   func(c *coldRig, newest string)
	}
	sqlOn := func(q string) func(c *coldRig, newest string) {
		return func(c *coldRig, newest string) { c.exec(q, newest) }
	}
	for _, tc := range []tamper{
		{"the bytes", sqlOn("UPDATE checkpoints SET candidate_bytes=candidate_bytes||x'20' WHERE checkpoint_id=?")},
		{"the hash", sqlOn("UPDATE checkpoints SET candidate_hash='" + strings.Repeat("0", 64) + "' WHERE checkpoint_id=?")},
		{"another serialization with its own hash", func(c *coldRig, id string) {
			c.reencode(id, func(s string) string {
				prefix, body, _ := strings.Cut(s, "\x00")
				return prefix + "\x00" + strings.Replace(body, `{"`, "{ \"", 1)
			})
		}},
		{"a byte order mark", func(c *coldRig, id string) {
			c.reencode(id, func(s string) string {
				prefix, body, _ := strings.Cut(s, "\x00")
				return prefix + "\x00\xef\xbb\xbf" + body
			})
		}},
		{"a metadata key omitted", func(c *coldRig, id string) {
			c.reencode(id, func(s string) string { return strings.Replace(s, `"reason":null,`, "", 1) })
		}},
		{"a default written out", func(c *coldRig, id string) {
			c.reencode(id, func(s string) string { return strings.Replace(s, `"reason":null,`, `"reason":null,"reason2":null,`, 1) })
		}},
		{"the mode of the row", sqlOn("UPDATE checkpoints SET mode='emergency' WHERE checkpoint_id=?")},
		{"the boundary of the row", sqlOn("UPDATE checkpoints SET durable_boundary=durable_boundary+1 WHERE checkpoint_id=?")},
		{"the revision of the row", sqlOn("UPDATE checkpoints SET context_revision=context_revision+1 WHERE checkpoint_id=?")},
		{"the parent of the row", sqlOn("UPDATE checkpoints SET parent_checkpoint_id=NULL WHERE checkpoint_id=?")},
		{"a source Evidence", func(c *coldRig, _ string) {
			id := c.val("SELECT json_extract(value,'$.evidence_id') FROM json_each((SELECT json_extract(CAST(substr(candidate_bytes, instr(candidate_bytes, char(0))+1) AS TEXT),'$.observation_inventory') FROM checkpoints ORDER BY rowid DESC LIMIT 1)) LIMIT 1")
			c.exec("DROP TRIGGER evidence_sealed_no_update")
			c.exec("UPDATE evidence SET raw_hash=? WHERE evidence_id=?", strings.Repeat("b", 64), id)
		}},
		{"a typed context block", func(c *coldRig, _ string) {
			id := c.val("SELECT evidence_id FROM items WHERE thread_id=? AND history_kind='HostContext' LIMIT 1", c.thread)
			c.exec("DROP TRIGGER chunks_no_update")
			c.exec("UPDATE evidence_chunks SET data=x'41' WHERE evidence_id=? AND ordinal=0", id)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := coldScenario(t)
			c.exec("DROP TRIGGER checkpoints_no_update")
			tc.do(c, c.cps[1])
			before := tableCounts(c.rig)
			acts := len(c.acts)
			i := 0
			for name, named := range map[string]*string{"naming it": protocol.Str(c.cps[1]), "naming none": nil} {
				i++
				_, err := c.resume(c.resumeInput(named, "cold.tamper.key.000000"+string(rune('0'+i))))
				if protocol.CodeOf(err) != protocol.CodeIntegrityBlocked {
					t.Errorf("%s: %v, want INTEGRITY_BLOCKED", name, err)
				}
			}
			// The healthy parent is not a way around it.
			if _, err := c.resume(c.resumeInput(protocol.Str(c.cps[0]), "cold.tamper.key.0000009")); protocol.CodeOf(err) != protocol.CodeRevisionConflict && protocol.CodeOf(err) != protocol.CodeIntegrityBlocked {
				t.Errorf("the parent: %v", err)
			}
			if now := tableCounts(c.rig); now["runs"] != before["runs"] || now["receipts"] != before["receipts"] || now["events"] != before["events"] || now["tasks"] != before["tasks"] || len(c.acts) != acts {
				t.Fatalf("a refused resume wrote or generated: %v -> %v (%d -> %d acts)", before, now, acts, len(c.acts))
			}
			if got := c.val("SELECT current_checkpoint_id FROM threads"); got != c.cps[1] {
				t.Fatalf("the Thread was moved to %s", got)
			}
			if got := c.val("SELECT COALESCE(active_run_id,'none') FROM threads"); got != "none" {
				t.Fatalf("a refused resume left a Run: %s", got)
			}
		})
	}
}

// TestAColdResumeOfAThreadWithNoCheckpointResumesAsBeforeAndNamingOneIsAConflict: the cold look
// reads the checkpoint and what stands on it, and a Thread that has none resumes at once, the
// result naming none.
func TestAColdResumeOfAThreadWithNoCheckpointResumesAsBeforeAndNamingOneIsAConflict(t *testing.T) {
	fake := harnesstest.NewFake()
	reached := make(chan struct{})
	var once sync.Once
	fake.OnMeasure = func(ctx context.Context) {
		once.Do(func() { close(reached) })
		<-ctx.Done()
	}
	r, _ := modelRig(t, fake)
	info := r.openSession("cold.plain.open.0001")
	start := r.startRun(info.ThreadID, "cold.plain.start.0001", "質問です。")
	<-reached
	r.interrupt(start.RunID, 0, "cold.plain.stop.00001")
	r.waitTerminal(start.RunID)
	fake.OnMeasure = nil
	fake.SetReply(harnesstest.Final("答えです。"))
	_, ctl := r.revisions(info.ThreadID)
	in := protocol.ResumeInput{TaskID: start.TaskID, ExpectedLastRunID: start.RunID, ExpectedControlRevision: ctl, IdempotencyKey: "cold.plain.resume.0001", Limits: startLimits}
	// A checkpoint named for a Thread that stands on none is a conflict, and writes nothing.
	named := in
	named.IdempotencyKey, named.CheckpointID = "cold.plain.resume.0002", protocol.Str("ckp_00000000-0000-7000-8000-000000000001")
	if _, err := r.call("run/resume", named); protocol.CodeOf(err) != protocol.CodeRevisionConflict {
		t.Fatalf("%v", err)
	}
	res := r.mustCall("run/resume", in)
	res.Done()
	got := decode[protocol.ResumeResult](t, res)
	if got.CheckpointID != nil {
		t.Fatalf("%+v", got)
	}
	if run := r.waitTerminal(got.RunID); run.Result.Status != "completed" {
		t.Fatalf("%+v", run.Result)
	}
}
