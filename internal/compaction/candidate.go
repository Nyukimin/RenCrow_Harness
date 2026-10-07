package compaction

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The ways a candidate is refused. ErrIntegrity (types.go) is the fourth: a contradiction
// in the records the candidate was made from.
var (
	// ErrNoFit: the candidate's verified prompt does not fit the usable budget.
	ErrNoFit = errors.New("compaction: the candidate does not fit")
	// ErrNotShrunk: the candidate's prompt is not shown to be smaller than the one it
	// replaces. Fewer bytes are not enough.
	ErrNotShrunk = errors.New("compaction: the candidate is not shown to be smaller")
	// ErrUnverified: a count is not verified (an estimate, or no count).
	ErrUnverified = errors.New("compaction: a count is not verified")
)

// SnapshotDigest is the digest of the context a candidate was made from (BYTE_CONTRACTS
// section 6): the Thread's revisions, every source of the live context in application
// order, and the latest durable and semantic checkpoints.
func SnapshotDigest(p *Prepared) (string, error) {
	var durable, semantic *string
	if p.Snapshot.Prior != nil {
		durable = protocol.Str(p.Snapshot.Prior.Candidate.CheckpointID)
	}
	if p.Summary != nil {
		semantic = protocol.Str(p.Summary.SourceCheckpointID)
	}
	srcs := p.AppliedSources
	if srcs == nil {
		srcs = []protocol.SourceRef{}
	}
	v, err := toValue(struct {
		ThreadID        string               `json:"thread_id"`
		ContextRevision int64                `json:"context_revision"`
		ControlRevision int64                `json:"control_revision"`
		WriterEpoch     int64                `json:"writer_epoch"`
		PolicyRevision  string               `json:"policy_revision"`
		BindingRevision string               `json:"binding_revision"`
		AppliedSources  []protocol.SourceRef `json:"applied_sources"`
		Durable         *string              `json:"latest_durable_checkpoint_id"`
		Semantic        *string              `json:"latest_semantic_checkpoint_id"`
	}{p.Snapshot.ThreadID, p.Snapshot.ContextRevision, p.Snapshot.ControlRevision, p.Snapshot.WriterEpoch, p.Snapshot.PolicyRevision, p.Snapshot.BindingRevision,
		srcs, durable, semantic})
	if err != nil {
		return "", err
	}
	return canon.D("rencrow-context-snapshot/v1", v)
}

// Counts are the two counts a candidate carries: the prompt it replaces and its own.
type Counts struct {
	Before, After modelport.MeasureResult
}

func baseCandidate(p *Prepared, id, mode string, c Counts) (Candidate, error) {
	digest, err := SnapshotDigest(p)
	if err != nil {
		return Candidate{}, err
	}
	s := p.Snapshot
	cand := Candidate{
		FormatVersion: CheckpointFormat, CheckpointID: id, ThreadID: s.ThreadID, TaskID: s.TaskID, RunID: s.RunID, Mode: mode,
		Expected:        Expected{ContextRevision: s.ContextRevision, ControlRevision: s.ControlRevision, WriterEpoch: s.WriterEpoch, PolicyRevision: s.PolicyRevision, BindingRevision: s.BindingRevision},
		DurableBoundary: p.DurableBoundary, SnapshotDigest: digest, BeforeCount: c.Before, AfterCount: c.After,
	}
	if s.Prior != nil {
		cand.ParentCheckpointID = protocol.Str(s.Prior.Candidate.CheckpointID)
	}
	return cand, nil
}

// retainedRefs is what a candidate must list as retained_exact_refs: every source of its
// instruction and protected entries, in order.
func retainedRefs(entries []contextplan.ProjectionEntry) []protocol.SourceRef {
	refs := []protocol.SourceRef{}
	for _, e := range entries {
		if e.Kind == KindInstruction || e.Kind == KindProtected {
			for _, s := range e.MessageSources {
				refs = append(refs, s...)
			}
		}
	}
	return refs
}

// cloneSummary is a deep copy, so that a candidate never shares memory with the
// context it was made from.
func cloneSummary(s *contextplan.StoredSummary) *contextplan.StoredSummary {
	if s == nil {
		return nil
	}
	out := *s
	sm := s.Summary
	sm.CurrentWork = cloneItems(sm.CurrentWork)
	sm.OpenItems = cloneItems(sm.OpenItems)
	sm.NextSteps = cloneItems(sm.NextSteps)
	sm.Decisions = clone(sm.Decisions)
	for i := range sm.Decisions {
		sm.Decisions[i].SourceHandles = clone(sm.Decisions[i].SourceHandles)
	}
	sm.Verification = clone(sm.Verification)
	for i := range sm.Verification {
		sm.Verification[i].SourceHandles = clone(sm.Verification[i].SourceHandles)
	}
	sm.ImportantObservationHandles = clone(sm.ImportantObservationHandles)
	out.Summary = sm
	out.SourceMap = clone(s.SourceMap)
	for i := range out.SourceMap {
		out.SourceMap[i].Sources = clone(out.SourceMap[i].Sources)
	}
	return &out
}

func cloneItems(in []contextplan.SummaryItem) []contextplan.SummaryItem {
	out := clone(in)
	for i := range out {
		out[i].SourceHandles = clone(out[i].SourceHandles)
	}
	return out
}

func cloneInt64(v *int64) *int64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func contextBlocks(p *Prepared) []protocol.ContextBlock {
	return clone(p.Snapshot.Blocks)
}

// summarizedUnits are the indices of the units a Normal compaction summarizes, and the
// coordinate it covers up to: the last of them.
func summarizedUnits(p *Prepared) ([]int, int64) {
	idx := p.WorkUnits()
	anchor := p.SemanticBoundary
	for _, i := range idx {
		anchor = max(anchor, p.Units[i].Seq())
	}
	return idx, anchor
}

// LiveProjection is the live context as it stands, as the projection a checkpoint would
// hold if it kept everything: every unit, the prior Summary and its anchor. Rendered, it is
// the prompt the Run would have sent.
func LiveProjection(p *Prepared) contextplan.Projection {
	important := []contextplan.ObservationReference{}
	if p.Snapshot.Prior != nil {
		important = clone(p.Snapshot.Prior.Candidate.Projection.ImportantObservations)
		if important == nil {
			important = []contextplan.ObservationReference{}
		}
	}
	entries := make([]contextplan.ProjectionEntry, len(p.Units))
	for i, u := range p.Units {
		entries[i] = cloneEntry(u.Entry)
	}
	return contextplan.Projection{FormatVersion: contextplan.ProjectionFormat, ContextBlocks: contextBlocks(p), Summary: p.Summary, SummaryAnchorSequence: p.Anchor,
		ImportantObservations: important, Entries: entries}
}

// NormalEntries are the entries a Normal candidate keeps: the effective instructions and
// everything protected. The Work it summarizes is not among them.
func NormalEntries(ret *Retention) []contextplan.ProjectionEntry {
	out := []contextplan.ProjectionEntry{}
	for _, u := range ret.Units {
		if u.Entry.Kind == KindInstruction || u.Entry.Kind == KindProtected || u.Entry.Protected {
			out = append(out, cloneEntry(u.Entry))
		}
	}
	return out
}

// NormalProjection is the live context after a Normal compaction with the given Summary:
// the retained entries, the Summary and its anchor, and the important references. The
// Preflight counts it with the minimal Summary, and the candidate has it with the real
// one.
func NormalProjection(p *Prepared, ret *Retention, summary contextplan.StoredSummary, important []contextplan.ObservationReference) (contextplan.Projection, int64) {
	_, anchor := summarizedUnits(p)
	a := anchor
	if important == nil {
		important = []contextplan.ObservationReference{}
	}
	return contextplan.Projection{FormatVersion: contextplan.ProjectionFormat, ContextBlocks: contextBlocks(p), Summary: &summary, SummaryAnchorSequence: &a,
		ImportantObservations: important, Entries: NormalEntries(ret)}, anchor
}

// BuildNormal makes the Normal candidate from an accepted Summary.
func BuildNormal(p *Prepared, ret *Retention, in *SummaryInput, res *SummaryResult, id string, c Counts) (Candidate, error) {
	cand, err := baseCandidate(p, id, ModeNormal, c)
	if err != nil {
		return Candidate{}, err
	}
	cited := map[int]bool{}
	for _, i := range res.Cited {
		cited[i] = true
	}
	important := map[string]bool{}
	for _, h := range res.Output.ImportantObservationHandles {
		important[h] = true
	}
	updated := map[string]contextplan.ObservationReference{}
	var refs []contextplan.ObservationReference
	for i, o := range in.Observations {
		ref := RefOnly(o.Ref)
		if cited[i] {
			for _, ex := range o.Excerpts {
				ref.SummaryCoveredRange = UnionRanges(ref.SummaryCoveredRange, []contextplan.Range{{Start: int64(ex.Start), End: int64(ex.End)}})
			}
		}
		updated[ref.EvidenceID] = ref
		if important[o.Handle] {
			refs = append(refs, ref)
		}
	}
	proj, anchor := NormalProjection(p, ret, res.Stored, refs)
	cand.Projection = proj
	cand.SemanticBoundary = anchor
	cand.RetainedExactRefs = retainedRefs(proj.Entries)
	cand.AppliedSelection = clone(ret.Applied)
	cand.ObservationInventory = mergeInventory(p.Inventory, updated)
	return cand, nil
}

// mergeInventory replaces, in place, the inventory entries the update names, and appends
// the ones it adds, in the order given by the update map's own insertion into refs
// (callers pass an update with a stable order of keys through the inventory itself).
func mergeInventory(inv []contextplan.ObservationReference, upd map[string]contextplan.ObservationReference) []contextplan.ObservationReference {
	out := make([]contextplan.ObservationReference, 0, len(inv)+len(upd))
	seen := map[string]bool{}
	for _, r := range inv {
		if u, ok := upd[r.EvidenceID]; ok {
			r = u
		}
		seen[r.EvidenceID] = true
		out = append(out, cloneRef(r))
	}
	var extra []string
	for id := range upd {
		if !seen[id] {
			extra = append(extra, id)
		}
	}
	slices.Sort(extra)
	for _, id := range extra {
		out = append(out, cloneRef(upd[id]))
	}
	return out
}

// BuildEmergency makes the Emergency candidate: the context as it is, with the
// replacements the reduction made. The summary, its anchor and the semantic boundary are
// those of the prior checkpoint, unchanged; the Work not yet summarized stays where it is.
func BuildEmergency(p *Prepared, em EmergencyResult, id string, c Counts) (Candidate, error) {
	cand, err := baseCandidate(p, id, ModeEmergency, c)
	if err != nil {
		return Candidate{}, err
	}
	entries := make([]contextplan.ProjectionEntry, len(em.Units))
	updated := map[string]contextplan.ObservationReference{}
	for i, u := range em.Units {
		entries[i] = cloneEntry(u.Entry)
		for _, o := range u.Entry.Observations {
			updated[o.Reference.EvidenceID] = cloneRef(o.Reference)
		}
	}
	var important []contextplan.ObservationReference
	var prior contextplan.Projection
	if p.Snapshot.Prior != nil {
		prior = p.Snapshot.Prior.Candidate.Projection
	}
	important = clone(prior.ImportantObservations)
	if important == nil {
		important = []contextplan.ObservationReference{}
	}
	cand.Projection = contextplan.Projection{FormatVersion: contextplan.ProjectionFormat, ContextBlocks: contextBlocks(p), Summary: cloneSummary(p.Summary),
		SummaryAnchorSequence: cloneInt64(p.Anchor), ImportantObservations: important, Entries: entries}
	cand.SemanticBoundary = p.SemanticBoundary
	cand.RetainedExactRefs = retainedRefs(entries)
	cand.AppliedSelection = []AppliedOp{}
	cand.ObservationInventory = mergeInventory(p.Inventory, updated)
	return cand, nil
}

// ValidatedCandidate is a candidate that passed ValidateCandidate, with the exact bytes
// that were checked and their hash. It has no exported constructor: the only way to
// have one is to pass the checks, and the commit takes nothing else. The commit checks
// again what depends on the stored state.
type ValidatedCandidate struct {
	cand Candidate
	blob []byte
	hash string
}

// Candidate returns a copy of the validated candidate.
func (v ValidatedCandidate) Candidate() Candidate {
	c := v.cand
	c.normalize()
	return c
}

// Bytes are the exact bytes to store.
func (v ValidatedCandidate) Bytes() []byte { return slices.Clone(v.blob) }

// Hash is the SHA-256 of Bytes.
func (v ValidatedCandidate) Hash() string { return v.hash }

// ID is the checkpoint ID the candidate was made with.
func (v ValidatedCandidate) ID() string { return v.cand.CheckpointID }

// Shrinks says whether the after count shows a smaller prompt than the before count: the
// upper of the after below the lower of the before, or, for two exact counts, the after
// below the before.
func Shrinks(before, after modelport.MeasureResult) bool {
	if before.PromptLower == nil || before.PromptUpper == nil || after.PromptUpper == nil || after.PromptLower == nil {
		return false
	}
	if *after.PromptUpper < *before.PromptLower {
		return true
	}
	return before.State == "verified_exact" && after.State == "verified_exact" && *after.PromptUpper < *before.PromptUpper
}

// Usable is the prompt budget a count was taken against: the context limit less the
// reserved output and the margin. ok is false when the count has no limit.
func Usable(m modelport.MeasureResult) (int64, bool) {
	if m.EffectiveContextLimit == nil {
		return 0, false
	}
	return *m.EffectiveContextLimit - m.ReservedOutputTokens - m.SafetyMarginTokens, true
}

func verified(m modelport.MeasureResult) bool {
	return (m.State == "verified_exact" || m.State == "verified_bound") && m.PromptLower != nil && m.PromptUpper != nil && m.EffectiveContextLimit != nil
}

// ValidateInput is what ValidateCandidate checks a candidate against: the live context it
// was made from and, for a Normal candidate, the retention and the Summary input; for an
// Emergency one, the reduction.
type ValidateInput struct {
	Candidate Candidate
	Prepared  *Prepared
	Retention *Retention
	Emergency *EmergencyResult
	Blocks    []protocol.ContextBlock
}

// ValidateCandidate (F13) is the last check before a candidate may be stored. It refuses
// anything that the schema alone cannot show to be wrong: the Thread, Task, Run, parent,
// revisions and boundaries; every instruction and protected entry kept exactly, with
// its text matching the stored ranges it names and the list of retained references
// equal to those ranges; every Tool call answered; the references of the Observations
// consistent with what the messages show; a Summary whose handles are all resolved; and
// the counts: both verified, the candidate within the usable budget, and smaller than
// what it replaces. The candidate is serialized once and the bytes that are checked are
// the bytes returned.
func ValidateCandidate(in ValidateInput) (ValidatedCandidate, error) {
	c, p := in.Candidate, in.Prepared
	c.normalize()
	s := p.Snapshot
	switch {
	case c.FormatVersion != CheckpointFormat:
		return ValidatedCandidate{}, integrityf("the candidate has another format version")
	case c.ThreadID != s.ThreadID || c.TaskID != s.TaskID || c.RunID != s.RunID:
		return ValidatedCandidate{}, integrityf("the candidate is for another Thread, Task or Run")
	case !equalPtr(c.ParentCheckpointID, parentID(p)):
		return ValidatedCandidate{}, integrityf("the candidate's parent is not the current checkpoint")
	case c.Expected != Expected{ContextRevision: s.ContextRevision, ControlRevision: s.ControlRevision, WriterEpoch: s.WriterEpoch, PolicyRevision: s.PolicyRevision, BindingRevision: s.BindingRevision}:
		return ValidatedCandidate{}, integrityf("the candidate was made against other revisions than the snapshot's")
	case c.DurableBoundary != p.DurableBoundary || c.SemanticBoundary > c.DurableBoundary || c.SemanticBoundary < p.SemanticBoundary:
		return ValidatedCandidate{}, integrityf("the candidate's boundaries are not the snapshot's")
	case c.Projection.FormatVersion != contextplan.ProjectionFormat:
		return ValidatedCandidate{}, integrityf("the projection has another format version")
	}
	if want, err := SnapshotDigest(p); err != nil || c.SnapshotDigest != want {
		return ValidatedCandidate{}, integrityf("the candidate's snapshot digest is not the snapshot's")
	}
	if !reflect.DeepEqual(norm(c.Projection.ContextBlocks), norm(in.Blocks)) {
		return ValidatedCandidate{}, integrityf("the candidate's context blocks are not the Run's")
	}
	switch c.Mode {
	case ModeNormal:
		if err := checkNormal(c, in); err != nil {
			return ValidatedCandidate{}, err
		}
	case ModeEmergency:
		if err := checkEmergency(c, in); err != nil {
			return ValidatedCandidate{}, err
		}
	default:
		return ValidatedCandidate{}, integrityf("the candidate has a mode a compaction does not make")
	}
	if err := checkEntries(c); err != nil {
		return ValidatedCandidate{}, err
	}
	if !reflect.DeepEqual(norm(c.RetainedExactRefs), norm(retainedRefs(c.Projection.Entries))) {
		return ValidatedCandidate{}, integrityf("the retained references are not those of the retained entries")
	}
	if err := checkInventory(c); err != nil {
		return ValidatedCandidate{}, err
	}
	if err := checkSummary(c); err != nil {
		return ValidatedCandidate{}, err
	}
	if !verified(c.BeforeCount) || !verified(c.AfterCount) {
		return ValidatedCandidate{}, fmt.Errorf("%w: a count of the candidate is not verified", ErrUnverified)
	}
	usable, _ := Usable(c.AfterCount)
	if *c.AfterCount.PromptUpper > usable {
		return ValidatedCandidate{}, fmt.Errorf("%w: the candidate's prompt is over the usable budget", ErrNoFit)
	}
	if !Shrinks(c.BeforeCount, c.AfterCount) {
		return ValidatedCandidate{}, fmt.Errorf("%w", ErrNotShrunk)
	}
	raw, err := c.JSON()
	if err != nil {
		return ValidatedCandidate{}, integrityf("the candidate cannot be encoded")
	}
	val, err := strictjson.Decode(raw)
	if err != nil {
		return ValidatedCandidate{}, integrityf("the candidate cannot be encoded")
	}
	if err := schemacheck.Validate(schemacheck.Checkpoint, "CheckpointCandidate", val); err != nil {
		return ValidatedCandidate{}, integrityf("the candidate does not satisfy its schema: %v", err)
	}
	blob, err := protocol.EncodeCheckpointCandidate(raw)
	if err != nil {
		return ValidatedCandidate{}, integrityf("the candidate cannot be serialized")
	}
	if err := protocol.VerifyCheckpointCandidateBlob(blob); err != nil {
		return ValidatedCandidate{}, integrityf("the candidate's bytes do not verify")
	}
	return ValidatedCandidate{cand: c, blob: blob, hash: protocol.CheckpointCandidateHash(blob)}, nil
}

func parentID(p *Prepared) *string {
	if p.Snapshot.Prior == nil {
		return nil
	}
	return protocol.Str(p.Snapshot.Prior.Candidate.CheckpointID)
}

func equalPtr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// norm makes nil and empty lists equal for comparison.
func norm[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}

// checkNormal checks what is specific to a Normal candidate: its entries are exactly the
// retained ones, its Summary is the new one and is anchored at its semantic boundary.
func checkNormal(c Candidate, in ValidateInput) error {
	p := in.Prepared
	if in.Retention == nil || c.Projection.Summary == nil || c.Projection.SummaryAnchorSequence == nil {
		return integrityf("a Normal candidate has a retention and a Summary")
	}
	_, anchor := summarizedUnits(p)
	switch {
	case c.SemanticBoundary != anchor || *c.Projection.SummaryAnchorSequence != anchor:
		return integrityf("a Normal candidate's boundary is not the end of the Work it summarized")
	case c.Projection.Summary.SourceCheckpointID != c.CheckpointID:
		return integrityf("a new Summary names this checkpoint as its source")
	case !reflect.DeepEqual(norm(c.Projection.Entries), norm(NormalEntries(in.Retention))):
		return integrityf("a Normal candidate's entries are not the retained ones")
	case !reflect.DeepEqual(norm(c.AppliedSelection), norm(in.Retention.Applied)):
		return integrityf("a Normal candidate's applied selection is not the retention's")
	}
	for _, u := range in.Retention.Units {
		if (u.Entry.Kind == KindInstruction || u.Entry.Kind == KindProtected) && u.Seq() > c.DurableBoundary {
			return integrityf("an instruction lies beyond the candidate's durable boundary")
		}
		if u.Entry.Kind == KindInstruction {
			if err := exactInstruction(p, u); err != nil {
				return err
			}
		}
	}
	return nil
}

// exactInstruction checks that a retained instruction's message is exactly the stored
// text of the ranges it names.
func exactInstruction(p *Prepared, u Unit) error {
	var want strings.Builder
	for _, ref := range u.Entry.MessageSources[0] {
		found := false
		for _, orig := range p.Units {
			for _, seg := range orig.Segments {
				if seg.Ref.SourceID == ref.SourceID && seg.Ref.RawHash == ref.RawHash && ref.Range.Start >= seg.Ref.Range.Start && ref.Range.End <= seg.Ref.Range.End {
					want.WriteString(seg.Text[ref.Range.Start-seg.Ref.Range.Start : ref.Range.End-seg.Ref.Range.Start])
					found = true
				}
			}
		}
		if !found {
			return integrityf("a retained instruction names a source that is not in the live context")
		}
	}
	msg, err := contextplan.HistoryMessage(contextplan.HistoryItem{HistoryKind: kindOfOrigin(u.Entry.Origin), Origin: u.Entry.Origin, Text: want.String(), ContextSeq: u.Seq()})
	if err != nil || msg.Text() != u.Entry.Messages[0].Text() {
		return integrityf("a retained instruction is not the exact text of its sources")
	}
	return nil
}

func kindOfOrigin(origin string) string {
	if origin == protocol.OriginAutomation {
		return contextplan.HistoryAutomation
	}
	return contextplan.HistoryHuman
}

// checkEmergency checks that an Emergency candidate is the live context with only the
// replacements the reduction is allowed: the prior Summary, anchor, important references
// and semantic boundary unchanged, every unit present in order, and a tool message
// different from its original only where it is the reference marker of its own
// reference-only form.
func checkEmergency(c Candidate, in ValidateInput) error {
	p := in.Prepared
	pr := contextplan.Projection{}
	if p.Snapshot.Prior != nil {
		pr = p.Snapshot.Prior.Candidate.Projection
	}
	switch {
	case c.SemanticBoundary != p.SemanticBoundary:
		return integrityf("an Emergency candidate moved the semantic boundary")
	case !reflect.DeepEqual(c.Projection.Summary, p.Summary) || !equalInt64Ptr(c.Projection.SummaryAnchorSequence, p.Anchor):
		return integrityf("an Emergency candidate changed the Summary or its anchor")
	case !reflect.DeepEqual(norm(c.Projection.ImportantObservations), norm(pr.ImportantObservations)):
		return integrityf("an Emergency candidate changed the important references")
	case len(c.AppliedSelection) != 0:
		return integrityf("an Emergency candidate applies no selection")
	case len(c.Projection.Entries) != len(p.Units):
		return integrityf("an Emergency candidate does not keep every entry")
	}
	for i, e := range c.Projection.Entries {
		o := p.Units[i].Entry
		if e.Kind != o.Kind || e.Sequence != o.Sequence || e.Origin != o.Origin || e.Protected != o.Protected || len(e.Messages) != len(o.Messages) ||
			!reflect.DeepEqual(e.MessageSources, o.MessageSources) || len(e.Observations) != len(o.Observations) {
			return integrityf("an Emergency candidate changed an entry other than by replacements")
		}
		replaced := map[int64]bool{}
		for j, so := range o.Observations {
			sn := e.Observations[j]
			if sn.MessageOffset != so.MessageOffset || sn.Eligible != so.Eligible {
				return integrityf("an Emergency candidate changed an Observation slot")
			}
			if reflect.DeepEqual(cloneRef(sn.Reference), cloneRef(so.Reference)) {
				continue
			}
			if !so.Eligible || o.Protected || !reflect.DeepEqual(sn.Reference, RefOnly(so.Reference)) {
				return integrityf("an Emergency candidate replaced an Observation that may not be replaced")
			}
			want, err := Marker(so.Reference)
			if err != nil || e.Messages[sn.MessageOffset].Text() != want || e.Messages[sn.MessageOffset].ToolCallID != o.Messages[sn.MessageOffset].ToolCallID {
				return integrityf("a replaced Observation is not its reference marker")
			}
			if len(want) >= len(o.Messages[sn.MessageOffset].Text()) {
				return integrityf("a replacement is not shorter than what it replaces")
			}
			replaced[sn.MessageOffset] = true
		}
		for k := range e.Messages {
			if !replaced[int64(k)] && !reflect.DeepEqual(e.Messages[k], o.Messages[k]) {
				return integrityf("an Emergency candidate changed a message other than by replacement")
			}
		}
	}
	return nil
}

func equalInt64Ptr(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// checkEntries checks the entries as a prompt would need them: increasing coordinates,
// every message with its sources, every Observation slot on a tool message with a
// reference that matches its message, and every Tool call answered.
func checkEntries(c Candidate) error {
	last := int64(-1)
	for _, e := range c.Projection.Entries {
		if e.Sequence <= last || e.Sequence > c.DurableBoundary {
			return integrityf("the entries are not in increasing order within the durable boundary")
		}
		last = e.Sequence
		if len(e.Messages) == 0 || len(e.MessageSources) != len(e.Messages) {
			return integrityf("an entry needs one source list for each message")
		}
		for _, o := range e.Observations {
			if o.MessageOffset < 0 || o.MessageOffset >= int64(len(e.Messages)) || e.Messages[o.MessageOffset].Role != "tool" {
				return integrityf("an Observation slot is not on a tool message")
			}
			ref := o.Reference
			src := e.MessageSources[o.MessageOffset]
			if len(src) != 1 || src[0].SourceID != ref.EvidenceID || src[0].RawHash != ref.RawHash || int64(src[0].Range.End) != ref.TotalBytes {
				return integrityf("an Observation's reference does not match its source")
			}
			text := e.Messages[o.MessageOffset].Text()
			if len(ref.PresentedRanges) == 0 {
				if want, err := Marker(ref); err != nil || want != text {
					return integrityf("an Observation shown only by reference is not its marker")
				}
			} else if IsMarker(text) {
				return integrityf("an Observation shown in full is a marker")
			}
		}
	}
	return nil
}

// checkInventory: every Observation of the entries and every important reference is in
// the inventory, once.
func checkInventory(c Candidate) error {
	known := map[string]int{}
	for _, r := range c.ObservationInventory {
		known[r.EvidenceID]++
	}
	for id, n := range known {
		if n != 1 {
			return integrityf("an Observation is in the inventory twice (%s)", id)
		}
	}
	for _, e := range c.Projection.Entries {
		for _, o := range e.Observations {
			if known[o.Reference.EvidenceID] != 1 {
				return integrityf("an Observation of the entries is not in the inventory")
			}
		}
	}
	for _, r := range c.Projection.ImportantObservations {
		if known[r.EvidenceID] != 1 {
			return integrityf("an important reference is not in the inventory")
		}
	}
	return nil
}

var handleRE = func(h string) bool {
	for _, p := range []string{"work-", "observation-", "instruction-", "summary-", "completion-"} {
		if strings.HasPrefix(h, p) && len(h) > len(p) {
			return true
		}
	}
	return false
}

// checkSummary: every handle the Summary cites has an entry in the source map, the map has
// no entry twice or without sources, and nothing in it is an unresolved handle.
func checkSummary(c Candidate) error {
	s := c.Projection.Summary
	if s == nil {
		return nil
	}
	have := map[string]bool{}
	for _, m := range s.SourceMap {
		if have[m.Handle] || len(m.Sources) == 0 || len(m.Sources) > 64 || !handleRE(m.Handle) {
			return integrityf("the Summary's source map is not well formed")
		}
		have[m.Handle] = true
	}
	need := func(hs []string) error {
		for _, h := range hs {
			if !have[h] {
				return integrityf("the Summary cites a handle its source map does not resolve")
			}
		}
		return nil
	}
	for _, i := range s.Summary.CurrentWork {
		if err := need(i.SourceHandles); err != nil {
			return err
		}
	}
	for _, i := range s.Summary.Decisions {
		if err := need(i.SourceHandles); err != nil {
			return err
		}
	}
	for _, i := range s.Summary.Verification {
		if err := need(i.SourceHandles); err != nil {
			return err
		}
	}
	for _, i := range s.Summary.OpenItems {
		if err := need(i.SourceHandles); err != nil {
			return err
		}
	}
	for _, i := range s.Summary.NextSteps {
		if err := need(i.SourceHandles); err != nil {
			return err
		}
	}
	return need(s.Summary.ImportantObservationHandles)
}

// ParseCheckpoint is the load side of the checkpoint format: the version prefix and NUL,
// strict JSON, a CJ1 re-encoding that reproduces the stored bytes, the schema, and the
// hash. A blob that fails any of them is ErrIntegrity: a checkpoint that exists and is
// broken is never read as no checkpoint, and an unknown version is never interpreted.
func ParseCheckpoint(blob []byte, hash string) (*Checkpoint, error) {
	if protocol.CheckpointCandidateHash(blob) != hash {
		return nil, integrityf("a stored checkpoint does not match its hash")
	}
	if err := protocol.VerifyCheckpointCandidateBlob(blob); err != nil {
		return nil, integrityf("a stored checkpoint is not in its canonical form")
	}
	_, body, _ := bytes.Cut(blob, []byte{0})
	val, err := strictjson.Decode(body)
	if err != nil {
		return nil, integrityf("a stored checkpoint is not strict JSON")
	}
	if err := schemacheck.Validate(schemacheck.Checkpoint, "CheckpointCandidate", val); err != nil {
		return nil, integrityf("a stored checkpoint does not satisfy its schema")
	}
	var c Candidate
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, integrityf("a stored checkpoint cannot be read")
	}
	return &Checkpoint{Candidate: c, Bytes: slices.Clone(blob), Hash: hash}, nil
}
