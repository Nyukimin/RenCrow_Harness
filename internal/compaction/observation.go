package compaction

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

// UnionRanges is the union of two lists of half-open byte ranges: sorted, with ranges that
// overlap or touch joined, and empty ranges dropped.
func UnionRanges(a, b []contextplan.Range) []contextplan.Range {
	all := make([]contextplan.Range, 0, len(a)+len(b))
	for _, r := range append(clone(a), b...) {
		if r.End > r.Start {
			all = append(all, r)
		}
	}
	slices.SortFunc(all, func(x, y contextplan.Range) int {
		return cmp.Or(cmp.Compare(x.Start, y.Start), cmp.Compare(x.End, y.End))
	})
	out := []contextplan.Range{}
	for _, r := range all {
		if n := len(out); n > 0 && r.Start <= out[n-1].End {
			out[n-1].End = max(out[n-1].End, r.End)
			continue
		}
		out = append(out, r)
	}
	return out
}

// RefOnly (F11) is the reference-only form of an Observation reference: nothing of it
// is presented, so it is partial; what was seen and what a Summary covered stay as they
// were. These ranges say what was shown, never that it was understood.
func RefOnly(r contextplan.ObservationReference) contextplan.ObservationReference {
	r = cloneRef(r)
	r.PresentedRanges, r.Partial = []contextplan.Range{}, true
	return r
}

// Marker is the text that stands for the Observation in the live context: the marker of
// its reference-only form, serialized in full (its length is never assumed).
func Marker(r contextplan.ObservationReference) (string, error) {
	return contextplan.ObservationMarker(RefOnly(r))
}

// IsMarker says whether a tool message's content is already a reference marker.
func IsMarker(content string) bool {
	return strings.HasPrefix(content, "RENCROW_OBSERVATION_REFERENCE_V1\n")
}

// slotRef is the position of one eligible Observation in the live context.
type slotRef struct {
	unit, slot int
	key        slotKey
}

type slotKey struct {
	seq      int64
	start    uint64
	evidence string
	offset   int64
}

func (a slotKey) compare(b slotKey) int {
	return cmp.Or(cmp.Compare(a.seq, b.seq), cmp.Compare(a.start, b.start), strings.Compare(a.evidence, b.evidence), cmp.Compare(a.offset, b.offset))
}

// Replacement is one Observation answer the reduction replaced with its reference.
type Replacement struct {
	Unit, Slot              int
	BeforeBytes, AfterBytes int
}

// EmergencyResult is the live context after the reduction and what it replaced.
type EmergencyResult struct {
	Units    []Unit
	Replaced []Replacement
}

// Emergency (F12) is the reduction that needs no model. Every Observation answer that is
// eligible is replaced with its reference marker, in one fixed order, when the marker
// is shorter in bytes than the answer: an answer is eligible when it is text that ran
// to an end, is not protected, is not already a marker, and matches what its reference
// says (size and hash) so that it is the stored original that is being replaced. The
// order is (application coordinate, start of the source range, Evidence ID by its UTF-8
// bytes, message offset), and two slots with the same key contradict the Host's own
// records (ErrIntegrity). All of them are replaced: nothing stops early on a guess that
// enough was saved. The summary, its anchor and boundary, every instruction, every
// protected entry and all the Work not yet summarized are carried over untouched, and
// the roles, call IDs and calls of the messages never change. Fewer bytes are not proof
// of fewer tokens: the caller counts the whole candidate.
func Emergency(p *Prepared) (EmergencyResult, error) {
	units := make([]Unit, len(p.Units))
	for i, u := range p.Units {
		units[i] = u
		units[i].Entry = cloneEntry(u.Entry)
	}
	var slots []slotRef
	for ui, u := range units {
		if u.Entry.Kind != KindToolExchange || u.Entry.Protected {
			continue
		}
		for si, slot := range u.Entry.Observations {
			if !slot.Eligible {
				continue
			}
			src := u.Entry.MessageSources[slot.MessageOffset]
			if len(src) != 1 || src[0].SourceID != slot.Reference.EvidenceID {
				return EmergencyResult{}, integrityf("an Observation's source is not its Evidence")
			}
			slots = append(slots, slotRef{ui, si, slotKey{u.Seq(), src[0].Range.Start, slot.Reference.EvidenceID, slot.MessageOffset}})
		}
	}
	slices.SortFunc(slots, func(a, b slotRef) int { return a.key.compare(b.key) })
	for i := 1; i < len(slots); i++ {
		if slots[i].key == slots[i-1].key {
			return EmergencyResult{}, integrityf("two Observations have the same position")
		}
	}
	var out EmergencyResult
	for _, s := range slots {
		e := &units[s.unit].Entry
		slot := &e.Observations[s.slot]
		content := e.Messages[slot.MessageOffset].Text()
		if IsMarker(content) {
			continue
		}
		sum := sha256.Sum256([]byte(content))
		if int64(len(content)) != slot.Reference.TotalBytes || hex.EncodeToString(sum[:]) != slot.Reference.RawHash {
			return EmergencyResult{}, integrityf("an Observation's content is not the stored original its reference names")
		}
		marker, err := Marker(slot.Reference)
		if err != nil {
			return EmergencyResult{}, integrityf("an Observation's reference cannot be made into a marker")
		}
		if len(marker) >= len(content) {
			continue // the same length or longer is kept
		}
		e.Messages[slot.MessageOffset] = modelport.Tool(e.Messages[slot.MessageOffset].ToolCallID, marker)
		slot.Reference = RefOnly(slot.Reference)
		out.Replaced = append(out.Replaced, Replacement{Unit: s.unit, Slot: s.slot, BeforeBytes: len(content), AfterBytes: len(marker)})
	}
	out.Units = units
	return out, nil
}
