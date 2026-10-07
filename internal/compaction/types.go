// Package compaction decides how a Thread's context is made smaller (F06-F14): it
// prepares what is stored into units and an inventory, builds the data of the two
// model stages (Selection and Summary), applies and validates what the stages answer,
// builds the Emergency reduction that needs no model, and turns the result into a
// checkpoint candidate whose bytes are what is stored.
//
// Everything that decides is a function of its arguments. What has to touch the
// outside world (counting a prompt, one generation of a stage, reading a stored text
// back, recording Evidence) is reached through the Ports the Engine is given, so the
// whole flow, the five results included, is testable without a store or a model. The
// Engine commits nothing: it returns a ValidatedCandidate that the caller stores with
// one short compare-and-set transaction (F14).
package compaction

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Owner is the SourceRef owner of everything the Harness stored itself.
const Owner = "RenCrow_Harness"

// TextProjection is the only projection of a source this build reads: the text of a
// text Evidence is its own bytes.
const TextProjection = "text/v1"

// CheckpointFormat is the format_version of a checkpoint candidate.
const CheckpointFormat = "rencrow-checkpoint/v1"

// Origins a SourceRef or entry has beside the three of protocol: the model's own
// messages and the answers of Tools.
const (
	OriginAgent = "agent"
	OriginTool  = "tool"
)

// Modes of a checkpoint.
const (
	ModeNormal    = "normal"
	ModeEmergency = "emergency"
)

// Kinds of a ProjectionEntry.
const (
	KindInstruction  = "instruction"
	KindWork         = "work"
	KindToolExchange = "tool_exchange"
	KindProtected    = "protected"
)

// The five outcomes of an executed compaction (CompactResult.outcome), and the
// statuses that are none of them.
const (
	OutcomeNormal    = "NormalCompacted"
	OutcomeEmergency = "EmergencyCompacted"
	OutcomeCapacity  = "CapacityBlocked"
	OutcomeIntegrity = "IntegrityBlocked"
	OutcomeRestart   = "RestartRequired"

	StatusExecuted    = "executed"
	StatusCancelled   = "cancelled"
	StatusStale       = "stale"
	StatusUnavailable = "unavailable"
	// StatusError is the status of a compaction that was ended by an error of something it
	// reached (a store that failed): the caller classifies it. It is not a CompactResult
	// status.
	StatusError = "error"
)

// ErrIntegrity marks a stored fact that contradicts another: the compaction stops
// (IntegrityBlocked), and nothing is repaired by guessing.
var ErrIntegrity = errors.New("compaction: stored state is inconsistent")

func integrityf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrIntegrity, fmt.Sprintf(format, args...))
}

// ErrSemantic marks output of a model stage (or a dataset that cannot be made) that
// the Normal compaction cannot use; the Engine answers it with the Emergency
// reduction.
var ErrSemantic = errors.New("compaction: the normal compaction cannot use this")

func semanticf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrSemantic, fmt.Sprintf(format, args...))
}

// Source is one stored item of a Thread's history as the store read it back: its text
// (already checked against the recorded hash) and where it is stored.
type Source struct {
	MessageID   string
	ContextSeq  int64
	ItemSeq     int64
	HistoryKind string
	Origin      string
	// ItemKind and ToolCallID are the item's record: the two halves of a Tool exchange.
	ItemKind   string
	ToolCallID string

	EvidenceID      string
	RawHash         string
	TotalBytes      int64
	CaptureComplete bool
	// TextProjection says the Evidence has the text/v1 projection. Anything else is a
	// part this build does not read as text and never shrinks.
	TextProjection bool
	Text           string
	// ContinuingConstraint is the Host's flag that this instruction is a standing
	// constraint, which no selection removes. Nothing sets it in this build; the field is
	// where a Host that does will say so.
	ContinuingConstraint bool
	// Presented says a generation was sent after this item was applied: the model has
	// seen it. An item that was applied and never sent is not claimed as seen.
	Presented bool
}

// Ref is the SourceRef of the whole of the source's text.
func (s Source) Ref() protocol.SourceRef {
	return protocol.SourceRef{Owner: Owner, SourceID: s.EvidenceID, RawHash: s.RawHash, ProjectionVersion: TextProjection,
		Range: protocol.ByteRange{Start: 0, End: uint64(s.TotalBytes)}, Origin: s.Origin, Sequence: uint64(s.ItemSeq)}
}

// Segment is one contiguous range of one stored text, with that text.
type Segment struct {
	Ref  protocol.SourceRef
	Text string
}

// Unit is one piece of the live context, in the order it was applied: what a
// checkpoint calls a ProjectionEntry, with the exact text of an instruction kept as
// the ranges it is made of.
type Unit struct {
	Entry contextplan.ProjectionEntry
	// Segments is the retained text of an instruction by range (empty for other kinds).
	Segments []Segment
	// Inherited is set for a unit that was an entry of the prior checkpoint.
	Inherited bool
}

// Seq is the unit's application coordinate (ProjectionEntry.sequence).
func (u Unit) Seq() int64 { return u.Entry.Sequence }

// Expected are the revisions a candidate was made against (the values before the
// commit).
type Expected struct {
	ContextRevision int64  `json:"context_revision"`
	ControlRevision int64  `json:"control_revision"`
	WriterEpoch     int64  `json:"writer_epoch"`
	PolicyRevision  string `json:"policy_revision"`
	BindingRevision string `json:"binding_revision"`
}

// AppliedOp is one selection that was applied: which text was removed, why, and which
// sources show the reason.
type AppliedOp struct {
	Target   protocol.SourceRef   `json:"target"`
	Basis    string               `json:"basis"`
	Evidence []protocol.SourceRef `json:"evidence"`
}

// Candidate is a checkpoint (checkpoint.schema.json CheckpointCandidate), the value
// whose canonical bytes are stored.
type Candidate struct {
	FormatVersion        string                             `json:"format_version"`
	CheckpointID         string                             `json:"checkpoint_id"`
	ThreadID             string                             `json:"thread_id"`
	TaskID               string                             `json:"task_id"`
	RunID                string                             `json:"run_id"`
	ParentCheckpointID   *string                            `json:"parent_checkpoint_id"`
	Mode                 string                             `json:"mode"`
	Expected             Expected                           `json:"expected"`
	SemanticBoundary     int64                              `json:"semantic_boundary"`
	DurableBoundary      int64                              `json:"durable_boundary"`
	SnapshotDigest       string                             `json:"snapshot_digest"`
	RetainedExactRefs    []protocol.SourceRef               `json:"retained_exact_refs"`
	AppliedSelection     []AppliedOp                        `json:"applied_selection"`
	Projection           contextplan.Projection             `json:"projection"`
	ObservationInventory []contextplan.ObservationReference `json:"observation_inventory"`
	BeforeCount          modelport.MeasureResult            `json:"before_count"`
	AfterCount           modelport.MeasureResult            `json:"after_count"`
}

// normalize makes every list a list: the stored form has no null where the schema
// wants an array.
func (c *Candidate) normalize() {
	if c.RetainedExactRefs == nil {
		c.RetainedExactRefs = []protocol.SourceRef{}
	}
	if c.AppliedSelection == nil {
		c.AppliedSelection = []AppliedOp{}
	}
	for i := range c.AppliedSelection {
		if c.AppliedSelection[i].Evidence == nil {
			c.AppliedSelection[i].Evidence = []protocol.SourceRef{}
		}
	}
	if c.ObservationInventory == nil {
		c.ObservationInventory = []contextplan.ObservationReference{}
	}
	for i := range c.ObservationInventory {
		normalizeRef(&c.ObservationInventory[i])
	}
	p := &c.Projection
	if p.ContextBlocks == nil {
		p.ContextBlocks = []protocol.ContextBlock{}
	}
	if p.ImportantObservations == nil {
		p.ImportantObservations = []contextplan.ObservationReference{}
	}
	for i := range p.ImportantObservations {
		normalizeRef(&p.ImportantObservations[i])
	}
	if p.Entries == nil {
		p.Entries = []contextplan.ProjectionEntry{}
	}
	for i := range p.Entries {
		e := &p.Entries[i]
		if e.Observations == nil {
			e.Observations = []contextplan.ObservationSlot{}
		}
		for j := range e.Observations {
			normalizeRef(&e.Observations[j].Reference)
		}
		for j := range e.Messages {
			if e.Messages[j].Role == "assistant" && e.Messages[j].ToolCalls == nil {
				e.Messages[j].ToolCalls = []modelport.ToolCall{}
			}
		}
	}
	if p.Summary != nil {
		s := p.Summary.Summary
		if s.CurrentWork == nil {
			s.CurrentWork = []contextplan.SummaryItem{}
		}
		if s.Decisions == nil {
			s.Decisions = []contextplan.DecisionItem{}
		}
		if s.Verification == nil {
			s.Verification = []contextplan.VerificationItem{}
		}
		if s.OpenItems == nil {
			s.OpenItems = []contextplan.SummaryItem{}
		}
		if s.NextSteps == nil {
			s.NextSteps = []contextplan.SummaryItem{}
		}
		if s.ImportantObservationHandles == nil {
			s.ImportantObservationHandles = []string{}
		}
		p.Summary.Summary = s
		if p.Summary.SourceMap == nil {
			p.Summary.SourceMap = []contextplan.SourceMapEntry{}
		}
	}
}

func normalizeRef(r *contextplan.ObservationReference) {
	if r.PresentedRanges == nil {
		r.PresentedRanges = []contextplan.Range{}
	}
	if r.SeenRanges == nil {
		r.SeenRanges = []contextplan.Range{}
	}
	if r.SummaryCoveredRange == nil {
		r.SummaryCoveredRange = []contextplan.Range{}
	}
}

// JSON is the candidate as JSON text (the input of the canonical encoding).
func (c Candidate) JSON() ([]byte, error) {
	c.normalize()
	return json.Marshal(c)
}

// Bytes is the exact stored form: prefix, NUL and the canonical JSON.
func (c Candidate) Bytes() ([]byte, error) {
	raw, err := c.JSON()
	if err != nil {
		return nil, err
	}
	return protocol.EncodeCheckpointCandidate(raw)
}

// toValue turns any value encoding/json can marshal into the strict JSON value model
// canon encodes.
func toValue(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return strictjson.Decode(raw)
}

// cj1 is the CJ1 text of a value.
func cj1(v any) (string, error) {
	val, err := toValue(v)
	if err != nil {
		return "", err
	}
	out, err := canon.Encode(val)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// rangeLen is the byte length of a SourceRef's range.
func rangeLen(r protocol.SourceRef) int64 { return int64(r.Range.End) - int64(r.Range.Start) }

func clone[T any](in []T) []T {
	if in == nil {
		return nil
	}
	return append([]T{}, in...)
}
