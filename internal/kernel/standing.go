package kernel

import (
	"context"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// coldChecks lists every source a checkpoint names, to be found again as it was: the
// references of its entries, selections and Summary, and every Observation it keeps a
// reference to. (A checkpoint is committed on the sources its own references name; a cold
// look at one that is already stored also holds its inventory to the originals.)
func coldChecks(c compaction.Candidate) []sqlite.SourceCheck {
	var out []sqlite.SourceCheck
	for _, n := range c.Sources() {
		if n.Checkpoint && n.RawHash == "" {
			continue // a checkpoint named only by identity (the Summary's own source) is loaded by scope, not by hash
		}
		v := n.ProjectionVersion
		out = append(out, sqlite.SourceCheck{SourceID: n.ID, RawHash: n.RawHash, ProjectionVersion: v, End: n.End})
	}
	return out
}

// VerifyStanding is the cold look at what a Thread stands on, made before a Run is admitted
// to continue it (run/resume with a checkpoint, H03, H11 and H26 on their cold path): the
// Thread's snapshot is read back (every applied text against its hash, every typed block
// against its digest), the checkpoint it stands on is parsed (the prefix and NUL, strict JSON,
// the canonical re-encoding that reproduces the stored bytes, the schema, the hash) and held
// to its row, the checkpoint that accepted its Summary is found, the live context is
// prepared from the two (the entries and the tail in order, within the boundaries), and every
// source the checkpoint names is found again as it was.
//
// What is wrong is INTEGRITY_BLOCKED, whatever it is, and nothing is repaired by guessing: no
// older checkpoint is used in the place of one that does not read back, and none is taken
// for "no checkpoint". It returns the ID of the checkpoint the Thread stands on, nil when
// it stands on none.
func VerifyStanding(ctx context.Context, store *sqlite.Store, threadID, runID string) (*string, error) {
	snap, err := store.LoadSnapshot(ctx, runID)
	if err != nil {
		return nil, err
	}
	cp, ss, err := loadStanding(ctx, store, threadID, snap)
	if err != nil {
		return nil, protocol.NewError(protocol.CodeIntegrityBlocked, "%s", err.Error())
	}
	if cp == nil {
		return nil, nil
	}
	if err := verifyStanding(ctx, store, threadID, snap, cp, ss); err != nil {
		return nil, err
	}
	return protocol.Str(cp.Candidate.CheckpointID), nil
}

// verifyStanding is what is looked at, beyond the checkpoint itself reading back, of a Thread
// that stands on one: the live context is prepared from the checkpoint and the tail (every
// entry and boundary in order, which is what refuses a boundary that is not known rather than
// letting it through), and every source the checkpoint names is found again as it was. It is
// the one check, made when a Run begins (a turn/start Run or a resumed one alike, in the
// driver's Load) and again by VerifyStanding before a resume is admitted.
func verifyStanding(ctx context.Context, store *sqlite.Store, threadID string, snap sqlite.Snapshot, cp, ss *compaction.Checkpoint) error {
	if _, err := compaction.Prepare(compaction.Snapshot{
		ThreadID: threadID, ContextRevision: snap.ContextRevision, ControlRevision: snap.ControlRevision, WriterEpoch: snap.WriterEpoch,
		PolicyRevision: snap.PolicyRevision, BindingRevision: snap.BindingRevision, Blocks: snap.Blocks, Prior: cp, SummaryCheckpoint: ss, Tail: sourcesOf(snap.Applied),
	}); err != nil {
		return protocol.NewError(protocol.CodeIntegrityBlocked, "the context the thread stands on is inconsistent").Wrap(err)
	}
	return store.VerifySources(ctx, threadID, coldChecks(cp.Candidate))
}
