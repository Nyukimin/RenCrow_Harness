package service_test

import (
	"strings"
	"testing"
)

// A Run that begins on a Thread that stands on a checkpoint is held to everything the
// checkpoint names, as a resumed Run is (CHECKPOINT_FORMAT section 1: "the load ends with the
// check of its sources and metadata"; STORAGE section 7; F02 LoadSnapshot checks the
// checkpoint and the raw it stands on): a source that is not what the checkpoint names
// is INTEGRITY_BLOCKED at the Run's Load, with nothing generated, and no older checkpoint is
// used in the place of the one that fails.

// TestARunOfAThreadThatStandsOnACheckpointWhoseSourceIsNotWhatItNamesIsIntegrityBlocked is
// H03, H11 and H26 on the turn/start path.
func TestARunOfAThreadThatStandsOnACheckpointWhoseSourceIsNotWhatItNamesIsIntegrityBlocked(t *testing.T) {
	inventoryEvidence := "SELECT json_extract(value,'$.evidence_id') FROM json_each((SELECT json_extract(CAST(substr(candidate_bytes, instr(candidate_bytes, char(0))+1) AS TEXT),'$.observation_inventory') FROM checkpoints ORDER BY rowid DESC LIMIT 1)) LIMIT 1"
	for name, tamper := range map[string]func(c *coldRig){
		"a source Evidence whose hash is not the one recorded": func(c *coldRig) {
			id := c.val(inventoryEvidence)
			c.exec("DROP TRIGGER evidence_sealed_no_update")
			c.exec("UPDATE evidence SET raw_hash=? WHERE evidence_id=?", strings.Repeat("b", 64), id)
		},
		"a source Evidence that is shorter than the range the checkpoint names": func(c *coldRig) {
			id := c.val(inventoryEvidence)
			c.exec("DROP TRIGGER evidence_sealed_no_update")
			c.exec("UPDATE evidence SET total_bytes=1 WHERE evidence_id=?", id)
		},
		"a source Evidence that is not sealed": func(c *coldRig) {
			id := c.val(inventoryEvidence)
			c.exec("DROP TRIGGER evidence_sealed_no_update")
			c.exec("UPDATE evidence SET state='building' WHERE evidence_id=?", id)
		},
		"the bytes of the checkpoint": func(c *coldRig) {
			c.exec("DROP TRIGGER checkpoints_no_update")
			c.exec("UPDATE checkpoints SET candidate_bytes=candidate_bytes||x'20' WHERE checkpoint_id=?", c.cps[1])
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := coldScenario(t)
			tamper(c)
			acts, runs := len(c.acts), c.count("runs")
			start := c.startNext(c.thread, "turn.standing.key.000001", "続きをお願いします。")
			run := c.waitTerminal(start.RunID)
			res := run.Result
			if res.Status != "blocked" || res.Code != "INTEGRITY_BLOCKED" || res.Resumable || len(res.UnresolvedActionIDs) != 0 {
				t.Fatalf("%+v", res)
			}
			if len(c.acts) != acts {
				t.Fatalf("a Run on a checkpoint that does not hold generated: %d -> %d acts", acts, len(c.acts))
			}
			if c.count("runs") != runs+1 {
				t.Fatalf("%d runs", c.count("runs"))
			}
			// The older, healthy checkpoint is not a way around it, and the Thread was not moved.
			if got := c.val("SELECT current_checkpoint_id FROM threads"); got != c.cps[1] {
				t.Fatalf("the Thread was moved to %s", got)
			}
			if got := c.val("SELECT COALESCE(active_run_id,'none') FROM threads"); got != "none" {
				t.Fatalf("the Thread is held by %s", got)
			}
		})
	}
}

// TestARunOfAThreadWhoseCheckpointHoldsIsPromptedFromItAsBefore is the control: the same
// Thread, untouched, goes on from its checkpoint and generates.
func TestARunOfAThreadWhoseCheckpointHoldsIsPromptedFromItAsBefore(t *testing.T) {
	c := coldScenario(t)
	acts := len(c.acts)
	start := c.startNext(c.thread, "turn.standing.key.000002", "続きをお願いします。")
	run := c.waitTerminal(start.RunID)
	if run.Result.Status != "completed" || len(c.acts) != acts+1 {
		t.Fatalf("%+v (%d -> %d acts)", run.Result, acts, len(c.acts))
	}
	if p := promptOf(lastAct(c.compactionRig)); !strings.Contains(p, "user:RENCROW_CONTEXT_BOUNDARY_V1") {
		t.Fatalf("not prompted from the checkpoint:\n%.800s", p)
	}
}

// TestARunOnAForkedThreadStandsOnTheCheckpointItWasForkedFromWithoutBeingBlocked: the sources
// of the checkpoint are the source Thread's, imported by the fork; the check a Run makes as it
// begins finds them through the import, and the forked Thread works.
func TestARunOnAForkedThreadStandsOnTheCheckpointItWasForkedFromWithoutBeingBlocked(t *testing.T) {
	c := coldScenario(t)
	forked := c.fork(c.thread, c.cps[1], "turn.standing.fork.key.01")
	acts := len(c.acts)
	start := c.startNext(forked.ThreadID, "turn.standing.key.000003", "分岐先で続けます。")
	run := c.waitTerminal(start.RunID)
	if run.Result.Status != "completed" || len(c.acts) != acts+1 {
		t.Fatalf("%+v (%d -> %d acts)", run.Result, acts, len(c.acts))
	}
}
