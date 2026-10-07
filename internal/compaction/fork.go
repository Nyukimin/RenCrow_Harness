package compaction

import (
	"encoding/json"
	"fmt"

	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// ModeFork is the mode of the checkpoint a fork makes: the first checkpoint of a new Thread,
// a copy of a checkpoint of another one.
const ModeFork = "fork"

// ForkInput is what the fork checkpoint of a new Thread is made of (F25): the checkpoint of the
// source Thread, loaded and held to its row, the IDs the new Thread, its system Task and Run
// and the checkpoint were issued, the new Thread's revisions before the checkpoint is
// committed, and the verified count of the projection for the new Thread's binding.
type ForkInput struct {
	Source                                *Checkpoint
	CheckpointID, ThreadID, TaskID, RunID string
	Expected                              Expected
	// After is the count of the copied projection as the new Thread would send it: the source
	// checkpoint's own count when the binding is the one it was made for, a new one when not.
	After modelport.MeasureResult
}

// ForkCandidate is the fork checkpoint as it is stored: the candidate and its exact bytes and
// hash (serialized once, as every checkpoint is).
type ForkCandidate struct {
	Candidate Candidate
	Bytes     []byte
	Hash      string
}

// BuildFork makes the fork checkpoint (CHECKPOINT_FORMAT sections 2 and 4). The projection,
// the boundaries, the inventory, the retained references and the snapshot digest are the
// source checkpoint's, exactly: a fork copies what a Thread stood on, it does not summarize
// it again, and it is not held to the no-shrink condition of a compaction (the copy has the
// token count it had). What changes is whose it is: the new Thread, its Task and Run, no
// parent (the fork is the root of a new Thread's chain), the new Thread's revisions and the
// mode. Its before count is the source's own, and its after count the count for the new
// Thread's binding, which must fit: the fit condition is not waived for a fork, and neither
// is the check that every source it names is the one it says (the caller's, against the
// store).
func BuildFork(in ForkInput) (ForkCandidate, error) {
	src := in.Source
	if src == nil {
		return ForkCandidate{}, integrityf("a fork needs the checkpoint it copies")
	}
	if src.Candidate.FormatVersion != CheckpointFormat {
		return ForkCandidate{}, integrityf("the checkpoint to fork is of a format this build does not read")
	}
	if !verified(in.After) {
		return ForkCandidate{}, fmt.Errorf("%w: the count of the copied projection is not verified", ErrUnverified)
	}
	if usable, ok := Usable(in.After); !ok || *in.After.PromptUpper > usable {
		return ForkCandidate{}, fmt.Errorf("%w: the copied projection does not fit the new thread's binding", ErrNoFit)
	}
	// A deep copy through the candidate's own JSON, so that nothing of the source is shared.
	raw, err := src.Candidate.JSON()
	if err != nil {
		return ForkCandidate{}, integrityf("the checkpoint to fork cannot be copied")
	}
	var c Candidate
	if err := json.Unmarshal(raw, &c); err != nil {
		return ForkCandidate{}, integrityf("the checkpoint to fork cannot be copied")
	}
	c.CheckpointID, c.ThreadID, c.TaskID, c.RunID = in.CheckpointID, in.ThreadID, in.TaskID, in.RunID
	c.ParentCheckpointID, c.Mode, c.Expected = nil, ModeFork, in.Expected
	c.BeforeCount, c.AfterCount = src.Candidate.AfterCount, in.After
	c.normalize()
	if err := checkEntries(c); err != nil {
		return ForkCandidate{}, err
	}
	if err := checkInventory(c); err != nil {
		return ForkCandidate{}, err
	}
	if err := checkSummary(c); err != nil {
		return ForkCandidate{}, err
	}
	blob, err := c.Bytes()
	if err != nil {
		return ForkCandidate{}, fmt.Errorf("compaction: a fork checkpoint cannot be serialized: %w", err)
	}
	hash := protocol.CheckpointCandidateHash(blob)
	// What is stored must load: the bytes are read back through the same loader a cold resume
	// uses, before anything is committed.
	back, err := ParseCheckpoint(blob, hash)
	if err != nil {
		return ForkCandidate{}, err
	}
	return ForkCandidate{Candidate: back.Candidate, Bytes: blob, Hash: hash}, nil
}

// SourceNeed is one source a checkpoint names, found again as it was when the checkpoint is
// verified: an Evidence (with the hash, projection and length it was named with) or a
// checkpoint (by ID; Hash is empty when only its identity is named).
type SourceNeed struct {
	Checkpoint        bool
	ID                string
	RawHash           string
	ProjectionVersion string
	End               uint64
}

// Sources lists every source the checkpoint names, in the order it names them: the retained
// exact references, the selections it applied, the source maps of its Summary and the Summary's
// own source checkpoint, the sources of every entry's messages, and every Observation it
// keeps a reference to (the inventory, the important ones, the ones inside entries). A fork
// imports exactly these, because they are what the new Thread's prompts, source maps and
// evidence.read can name.
func (c Candidate) Sources() []SourceNeed {
	var out []SourceNeed
	add := func(r protocol.SourceRef) {
		out = append(out, SourceNeed{Checkpoint: r.ProjectionVersion == "checkpoint/v1", ID: r.SourceID, RawHash: r.RawHash, ProjectionVersion: r.ProjectionVersion, End: r.Range.End})
	}
	observation := func(r contextplan.ObservationReference) {
		out = append(out, SourceNeed{ID: r.EvidenceID, RawHash: r.RawHash, ProjectionVersion: r.ProjectionVersion, End: uint64(max(r.TotalBytes, 0))})
	}
	for _, r := range c.RetainedExactRefs {
		add(r)
	}
	for _, op := range c.AppliedSelection {
		add(op.Target)
		for _, r := range op.Evidence {
			add(r)
		}
	}
	if s := c.Projection.Summary; s != nil {
		out = append(out, SourceNeed{Checkpoint: true, ID: s.SourceCheckpointID})
		for _, m := range s.SourceMap {
			for _, r := range m.Sources {
				add(r)
			}
		}
	}
	for _, e := range c.Projection.Entries {
		for _, srcs := range e.MessageSources {
			for _, r := range srcs {
				add(r)
			}
		}
		for _, o := range e.Observations {
			observation(o.Reference)
		}
	}
	for _, r := range c.ObservationInventory {
		observation(r)
	}
	for _, r := range c.Projection.ImportantObservations {
		observation(r)
	}
	return out
}
