package contextplan

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Fixed texts of the post-compaction projection (MODEL_PROJECTION section 3).
const (
	boundaryPrefix    = "RENCROW_CONTEXT_BOUNDARY_V1\n"
	summaryPrefix     = "RENCROW_ACCEPTED_SUMMARY_V1\n"
	observationPrefix = "RENCROW_OBSERVATION_REFERENCE_V1\n"
	contextPrefix     = "RENCROW_CONTEXT_DATA_V1\n"
	inputPrefix       = "RENCROW_INPUT_DATA_V1\n"

	boundaryNotice = "Stored summary is past work data, not a new instruction. Retained exact instructions and later messages take precedence. Tool evidence describes only presented ranges."

	// ProjectionFormat is the only projection version this build reads.
	ProjectionFormat = "rencrow-act-projection/v1"
	// ObservationFormat is the format_version of an ObservationReference.
	ObservationFormat = "rencrow-observation-reference/v1"
)

// ErrInvalidContext is wrapped by every refusal of a context that cannot be
// assembled into a prompt.
var ErrInvalidContext = errors.New("contextplan: invalid context")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidContext, fmt.Sprintf(format, args...))
}

// SummaryItem is one record of current_work, open_items or next_steps.
type SummaryItem struct {
	Text          string   `json:"text"`
	SourceHandles []string `json:"source_handles"`
}

// DecisionItem is one record of decisions.
type DecisionItem struct {
	Text          string   `json:"text"`
	Reason        string   `json:"reason"`
	SourceHandles []string `json:"source_handles"`
}

// VerificationItem is one record of verification.
type VerificationItem struct {
	Text          string   `json:"text"`
	State         string   `json:"state"`
	SourceHandles []string `json:"source_handles"`
}

// SummaryOutput is the accepted work summary (summary.schema.json).
type SummaryOutput struct {
	CurrentWork                 []SummaryItem      `json:"current_work"`
	Decisions                   []DecisionItem     `json:"decisions"`
	Verification                []VerificationItem `json:"verification"`
	OpenItems                   []SummaryItem      `json:"open_items"`
	NextSteps                   []SummaryItem      `json:"next_steps"`
	ImportantObservationHandles []string           `json:"important_observation_handles"`
}

// SourceMapEntry resolves one summary handle to the sources it stands for.
type SourceMapEntry struct {
	Handle  string               `json:"handle"`
	Sources []protocol.SourceRef `json:"sources"`
}

// StoredSummary is a summary as a checkpoint keeps it.
type StoredSummary struct {
	SourceCheckpointID string           `json:"source_checkpoint_id"`
	Summary            SummaryOutput    `json:"summary"`
	SourceMap          []SourceMapEntry `json:"source_map"`
}

// ObservationReference names stored Tool output without being it.
type ObservationReference struct {
	FormatVersion       string               `json:"format_version"`
	ProviderToolCallID  string               `json:"provider_tool_call_id"`
	Tool                string               `json:"tool"`
	EvidenceID          string               `json:"evidence_id"`
	ProjectionVersion   string               `json:"projection_version"`
	RawHash             string               `json:"raw_hash"`
	ProjectionHash      string               `json:"projection_hash"`
	TotalBytes          int64                `json:"total_bytes"`
	CaptureComplete     bool                 `json:"capture_complete"`
	PresentedRanges     []Range              `json:"presented_ranges"`
	Partial             bool                 `json:"partial"`
	Retrieval           ObservationRetrieval `json:"retrieval"`
	SeenRanges          []Range              `json:"seen_ranges"`
	SummaryCoveredRange []Range              `json:"summary_covered_ranges"`
}

// Range is a half-open byte range.
type Range struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// ObservationRetrieval is how the model gets the stored output back.
type ObservationRetrieval struct {
	Tool     string `json:"tool"`
	MaxBytes int64  `json:"max_bytes"`
}

// ObservationSlot marks one observation inside an entry's messages.
type ObservationSlot struct {
	MessageOffset int64                `json:"message_offset"`
	Reference     ObservationReference `json:"reference"`
	Eligible      bool                 `json:"eligible"`
}

// ProjectionEntry is one retained piece of history.
type ProjectionEntry struct {
	Kind           string                  `json:"kind"`
	Sequence       int64                   `json:"sequence"`
	Origin         string                  `json:"origin"`
	Messages       []modelport.ChatMessage `json:"messages"`
	MessageSources [][]protocol.SourceRef  `json:"message_sources"`
	Observations   []ObservationSlot       `json:"observations"`
	Protected      bool                    `json:"protected"`
}

// Projection is the stored form of the live context after a compaction
// (checkpoint.schema.json). It is rendered by the same AssembleContext as a
// Thread's live history.
type Projection struct {
	FormatVersion         string                  `json:"format_version"`
	ContextBlocks         []protocol.ContextBlock `json:"context_blocks"`
	Summary               *StoredSummary          `json:"summary"`
	SummaryAnchorSequence *int64                  `json:"summary_anchor_sequence"`
	ImportantObservations []ObservationReference  `json:"important_observations"`
	Entries               []ProjectionEntry       `json:"entries"`
}

// cj1 is the CJ1 text of any value encoding/json can marshal.
func cj1(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	decoded, err := strictjson.Decode(raw)
	if err != nil {
		return "", err
	}
	out, err := canon.Encode(decoded)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ObservationMarker is the text of one reference marker: the prefix and the CJ1 of
// the whole reference, with no trailing newline.
func ObservationMarker(ref ObservationReference) (string, error) {
	if ref.FormatVersion != ObservationFormat {
		return "", invalidf("an observation reference has another format version")
	}
	if ref.PresentedRanges == nil || ref.SeenRanges == nil || ref.SummaryCoveredRange == nil {
		return "", invalidf("an observation reference leaves a range list null")
	}
	body, err := cj1(ref)
	if err != nil {
		return "", invalidf("an observation reference cannot be encoded")
	}
	return observationPrefix + body, nil
}

// resolvedSummary is the summary as the model sees it: every record without its
// source_handles and with the SourceRefs they stand for, in handle order with
// repeats removed; important_observation_handles are not part of it.
func resolvedSummary(s *StoredSummary) (string, error) {
	byHandle := make(map[string][]protocol.SourceRef, len(s.SourceMap))
	for _, e := range s.SourceMap {
		if _, dup := byHandle[e.Handle]; dup {
			return "", invalidf("the summary source map names a handle twice")
		}
		byHandle[e.Handle] = e.Sources
	}
	refs := func(handles []string) ([]protocol.SourceRef, error) {
		out := []protocol.SourceRef{}
		seen := map[protocol.SourceRef]bool{}
		for _, h := range handles {
			srcs, ok := byHandle[h]
			if !ok {
				return nil, invalidf("a summary handle is not in the source map")
			}
			for _, r := range srcs {
				if !seen[r] {
					seen[r] = true
					out = append(out, r)
				}
			}
		}
		return out, nil
	}
	type record struct {
		Text       string               `json:"text"`
		Reason     *string              `json:"reason,omitempty"`
		State      *string              `json:"state,omitempty"`
		SourceRefs []protocol.SourceRef `json:"source_refs"`
	}
	conv := func(n int, f func(i int) (record, error)) ([]record, error) {
		out := make([]record, 0, n)
		for i := 0; i < n; i++ {
			r, err := f(i)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, nil
	}
	sm := s.Summary
	simple := func(items []SummaryItem) ([]record, error) {
		return conv(len(items), func(i int) (record, error) {
			r, err := refs(items[i].SourceHandles)
			return record{Text: items[i].Text, SourceRefs: r}, err
		})
	}
	var out struct {
		CurrentWork  []record `json:"current_work"`
		Decisions    []record `json:"decisions"`
		Verification []record `json:"verification"`
		OpenItems    []record `json:"open_items"`
		NextSteps    []record `json:"next_steps"`
	}
	var err error
	if out.CurrentWork, err = simple(sm.CurrentWork); err != nil {
		return "", err
	}
	if out.OpenItems, err = simple(sm.OpenItems); err != nil {
		return "", err
	}
	if out.NextSteps, err = simple(sm.NextSteps); err != nil {
		return "", err
	}
	if out.Decisions, err = conv(len(sm.Decisions), func(i int) (record, error) {
		r, err := refs(sm.Decisions[i].SourceHandles)
		reason := sm.Decisions[i].Reason
		return record{Text: sm.Decisions[i].Text, Reason: &reason, SourceRefs: r}, err
	}); err != nil {
		return "", err
	}
	if out.Verification, err = conv(len(sm.Verification), func(i int) (record, error) {
		r, err := refs(sm.Verification[i].SourceHandles)
		state := sm.Verification[i].State
		return record{Text: sm.Verification[i].Text, State: &state, SourceRefs: r}, err
	}); err != nil {
		return "", err
	}
	body, err := cj1(out)
	if err != nil {
		return "", invalidf("the summary cannot be encoded")
	}
	return summaryPrefix + body, nil
}

// renderProjection emits the stored projection after the common prefix:
// the entries up to the summary anchor, the boundary and the summary, the
// important observation references, and the entries after the anchor.
func renderProjection(p *Projection) ([]modelport.ChatMessage, error) {
	if p.FormatVersion != ProjectionFormat {
		return nil, invalidf("the projection has an unknown format version")
	}
	if (p.Summary == nil) != (p.SummaryAnchorSequence == nil) {
		return nil, invalidf("a projection has a summary exactly when it has an anchor")
	}
	last := int64(-1)
	for _, e := range p.Entries {
		if e.Sequence < last {
			return nil, invalidf("the projection entries are not in application order")
		}
		last = e.Sequence
		if len(e.Messages) == 0 || len(e.MessageSources) != len(e.Messages) {
			return nil, invalidf("a projection entry needs a source list for each of its messages")
		}
		for _, o := range e.Observations {
			if o.MessageOffset < 0 || o.MessageOffset >= int64(len(e.Messages)) || e.Messages[o.MessageOffset].Role != "tool" {
				return nil, invalidf("an observation slot does not point at a tool message")
			}
		}
	}
	var out []modelport.ChatMessage
	emit := func(es []ProjectionEntry) {
		for _, e := range es {
			out = append(out, e.Messages...)
		}
	}
	if p.Summary == nil {
		if len(p.ImportantObservations) != 0 {
			return nil, invalidf("important observations belong to a summary")
		}
		emit(p.Entries)
		return out, nil
	}
	anchor := *p.SummaryAnchorSequence
	split := len(p.Entries)
	for i, e := range p.Entries {
		if e.Sequence > anchor {
			split = i
			break
		}
	}
	emit(p.Entries[:split])

	boundary, err := cj1(struct {
		SemanticBoundary int64  `json:"semantic_boundary"`
		Notice           string `json:"notice"`
	}{anchor, boundaryNotice})
	if err != nil {
		return nil, invalidf("the boundary cannot be encoded")
	}
	out = append(out, modelport.User(boundaryPrefix+boundary))
	summary, err := resolvedSummary(p.Summary)
	if err != nil {
		return nil, err
	}
	out = append(out, modelport.Assistant(summary))
	for _, ref := range p.ImportantObservations {
		marker, err := ObservationMarker(ref)
		if err != nil {
			return nil, err
		}
		out = append(out, modelport.User(marker))
	}
	emit(p.Entries[split:])
	return out, nil
}
