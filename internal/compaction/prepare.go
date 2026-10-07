package compaction

import (
	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Checkpoint is a stored checkpoint after it was loaded and checked: the candidate as
// parsed, and the exact bytes and hash it is stored as.
type Checkpoint struct {
	Candidate Candidate
	Bytes     []byte
	Hash      string
}

// Snapshot is everything one compaction is made from, read in one view: the revisions
// of the Thread (which the commit compares again), the checkpoint the live context
// stands on (nil when it stands on none), and the applied history after that
// checkpoint's durable boundary (all of it when there is none).
type Snapshot struct {
	ThreadID, TaskID, RunID                       string
	ContextRevision, ControlRevision, WriterEpoch int64
	PolicyRevision, BindingRevision               string
	// Blocks are the context blocks of the Run being compacted: what its prompts carry
	// and what the checkpoint keeps as its provenance.
	Blocks []protocol.ContextBlock
	Prior  *Checkpoint
	// SummaryCheckpoint is the checkpoint that accepted the Summary the context stands on,
	// when that is not Prior itself (an Emergency checkpoint keeps the Summary of an earlier
	// one). Only its identity, hash and length are used.
	SummaryCheckpoint *Checkpoint
	Tail              []Source
}

// Prepared is the live context of a Snapshot (F06): its units in application order, the
// accepted Summary it stands on, its boundaries, and the inventory of every Observation
// the Host knows of.
type Prepared struct {
	Snapshot Snapshot
	Units    []Unit
	// Summary is the last accepted Summary (nil when none was accepted) and Anchor the
	// application coordinate it covers up to. SemanticBoundary is that coordinate, or 0.
	Summary          *contextplan.StoredSummary
	Anchor           *int64
	SemanticBoundary int64
	// DurableBoundary is the application coordinate of the last entry of the live
	// context: what a checkpoint made now covers.
	DurableBoundary int64
	// Inventory lists, by Evidence, what is known of every Observation: the prior
	// checkpoint's inventory first, then the live ones, in the order first known.
	Inventory []contextplan.ObservationReference
	// AppliedSources is every source of the live context in application order (the input
	// of the snapshot digest).
	AppliedSources []protocol.SourceRef
}

// Prepare (F06) reads the live context of a snapshot into units and an inventory. It
// checks what a later step takes for granted: the prior checkpoint is of this Thread
// and consistent with itself, its entries and the tail are in strictly increasing
// application order and do not overlap, and every SourceRef is well formed. A
// contradiction is ErrIntegrity. Nothing here reads the store or the model.
func Prepare(s Snapshot) (*Prepared, error) {
	p := &Prepared{Snapshot: s}
	last := int64(0)
	if s.Prior != nil {
		c := s.Prior.Candidate
		pr := c.Projection
		switch {
		case c.FormatVersion != CheckpointFormat || c.ThreadID != s.ThreadID:
			return nil, integrityf("the current checkpoint is not this Thread's")
		case c.Mode != ModeNormal && c.Mode != ModeEmergency && c.Mode != ModeFork:
			return nil, integrityf("the current checkpoint has a mode a compaction cannot stand on")
		case (pr.Summary == nil) != (pr.SummaryAnchorSequence == nil):
			return nil, integrityf("the current checkpoint has a summary exactly when it has an anchor")
		case c.SemanticBoundary < 0 || c.DurableBoundary < c.SemanticBoundary:
			return nil, integrityf("the current checkpoint's boundaries contradict each other")
		case pr.Summary != nil && *pr.SummaryAnchorSequence != c.SemanticBoundary:
			return nil, integrityf("the current checkpoint's anchor is not its semantic boundary")
		case pr.Summary == nil && c.SemanticBoundary != 0:
			return nil, integrityf("the current checkpoint has a semantic boundary and no summary")
		}
		inherited, err := InheritedUnits(pr)
		if err != nil {
			return nil, err
		}
		for _, u := range inherited {
			if u.Seq() <= last || u.Seq() > c.DurableBoundary {
				return nil, integrityf("the current checkpoint's entries are not within its boundaries in order")
			}
			last = u.Seq()
		}
		p.Units = inherited
		p.Summary, p.Anchor, p.SemanticBoundary, p.DurableBoundary = pr.Summary, pr.SummaryAnchorSequence, c.SemanticBoundary, c.DurableBoundary
		for _, r := range c.ObservationInventory {
			p.noteObservation(r)
		}
		last = c.DurableBoundary
	}
	tail, err := BuildUnits(s.Tail)
	if err != nil {
		return nil, err
	}
	for _, u := range tail {
		if u.Seq() <= last {
			return nil, integrityf("the applied history after the checkpoint is not after it")
		}
		last = u.Seq()
		p.Units = append(p.Units, u)
	}
	if last > p.DurableBoundary {
		p.DurableBoundary = last
	}
	for _, u := range p.Units {
		for _, srcs := range u.Entry.MessageSources {
			for _, r := range srcs {
				if err := r.Validate(); err != nil {
					return nil, integrityf("a source reference is not well formed")
				}
				p.AppliedSources = append(p.AppliedSources, r)
			}
		}
		for _, o := range u.Entry.Observations {
			p.noteObservation(o.Reference)
		}
	}
	return p, nil
}

// noteObservation records what is known of an Observation: a newer statement about the
// same Evidence replaces an older one in place.
func (p *Prepared) noteObservation(r contextplan.ObservationReference) {
	r = cloneRef(r)
	for i := range p.Inventory {
		if p.Inventory[i].EvidenceID == r.EvidenceID {
			p.Inventory[i] = r
			return
		}
	}
	p.Inventory = append(p.Inventory, r)
}

// WorkUnits are the units not yet covered by a Summary: Work and Tool exchanges after
// the semantic boundary that are not protected. These are what a Normal compaction
// summarizes.
func (p *Prepared) WorkUnits() []int {
	var idx []int
	for i, u := range p.Units {
		if (u.Entry.Kind == KindWork || u.Entry.Kind == KindToolExchange) && !u.Entry.Protected && u.Seq() > p.SemanticBoundary {
			idx = append(idx, i)
		}
	}
	return idx
}

// StoredFacts are what the store's checkpoint row says beside the checkpoint's bytes.
type StoredFacts struct {
	CheckpointID, ThreadID, RunID string
	ParentCheckpointID            *string
	Mode                          string
	// ContextRevision is the Thread's context revision after the commit.
	ContextRevision                   int64
	SemanticBoundary, DurableBoundary int64
	Hash                              string
}

// CheckStored holds a loaded checkpoint to the row it was stored in: the same IDs, mode,
// parent, boundaries and hash, and a context revision that is exactly one above the one the
// candidate was made against (the candidate says the value before the commit, the row the
// value after: they are never compared as equal). Any difference is ErrIntegrity.
func (c *Checkpoint) CheckStored(f StoredFacts) error {
	x := c.Candidate
	if x.CheckpointID != f.CheckpointID || x.ThreadID != f.ThreadID || x.RunID != f.RunID || x.Mode != f.Mode || !equalPtr(x.ParentCheckpointID, f.ParentCheckpointID) ||
		x.SemanticBoundary != f.SemanticBoundary || x.DurableBoundary != f.DurableBoundary || c.Hash != f.Hash || x.Expected.ContextRevision+1 != f.ContextRevision {
		return integrityf("a stored checkpoint is not the checkpoint its row describes")
	}
	return nil
}

// summarySource is the checkpoint that accepted the Summary the live context stands on.
func (p *Prepared) summarySource() (*Checkpoint, error) {
	if p.Summary == nil {
		return nil, nil
	}
	id := p.Summary.SourceCheckpointID
	if pr := p.Snapshot.Prior; pr != nil && pr.Candidate.CheckpointID == id {
		return pr, nil
	}
	if sc := p.Snapshot.SummaryCheckpoint; sc != nil && sc.Candidate.CheckpointID == id {
		return sc, nil
	}
	return nil, integrityf("the checkpoint that accepted the current Summary is not available")
}
