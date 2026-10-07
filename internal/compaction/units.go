package compaction

import (
	"encoding/json"

	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolview"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// BuildUnits turns the applied history into units, in application order (the first
// half of F06). An instruction, a Work message and an unknown-origin input are one unit
// each; the assistant message that asked for Tools and the answers to its calls are
// one unit, kept whole so a boundary never falls inside a Tool exchange. A Tool call
// without its answer, an answer without its call or in another order, or a kind of
// history a prompt cannot hold is a contradiction in what is stored (ErrIntegrity), not
// something to work around.
func BuildUnits(sources []Source) ([]Unit, error) {
	var units []Unit
	last := int64(0)
	for i := 0; i < len(sources); {
		s := sources[i]
		if s.ContextSeq <= last {
			return nil, integrityf("the applied history is not in strictly increasing application order")
		}
		var (
			u   Unit
			n   = 1
			err error
		)
		switch s.ItemKind {
		case contextplan.ItemKindToolCalls:
			u, n, err = toolExchange(sources[i:])
		case contextplan.ItemKindToolResult:
			err = integrityf("a Tool result stands without its call")
		default:
			u, err = single(s)
		}
		if err != nil {
			return nil, err
		}
		units = append(units, u)
		last = u.Seq()
		i += n
	}
	return units, nil
}

func historyItem(s Source) contextplan.HistoryItem {
	return contextplan.HistoryItem{ContextSeq: s.ContextSeq, MessageID: s.MessageID, HistoryKind: s.HistoryKind, Origin: s.Origin, Text: s.Text,
		ItemKind: s.ItemKind, ToolCallID: s.ToolCallID}
}

// single is the unit of one stored message that is not part of a Tool exchange.
func single(s Source) (Unit, error) {
	if !s.TextProjection {
		return Unit{}, integrityf("a message is not stored as text")
	}
	msg, err := contextplan.HistoryMessage(historyItem(s))
	if err != nil {
		return Unit{}, integrityf("a stored message cannot be rendered: %v", err)
	}
	ref := s.Ref()
	e := contextplan.ProjectionEntry{Sequence: s.ContextSeq, Origin: s.Origin, Messages: []modelport.ChatMessage{msg},
		MessageSources: [][]protocol.SourceRef{{ref}}, Observations: []contextplan.ObservationSlot{}}
	var segments []Segment
	switch s.HistoryKind {
	case contextplan.HistoryHuman, contextplan.HistoryAutomation:
		e.Kind, e.Protected = KindInstruction, s.ContinuingConstraint
		segments = []Segment{{Ref: ref, Text: s.Text}}
	case contextplan.HistoryProtected:
		e.Kind, e.Protected = KindProtected, true
	case contextplan.HistoryWork:
		e.Kind = KindWork
	default:
		return Unit{}, integrityf("a history entry has a kind a checkpoint cannot hold")
	}
	return Unit{Entry: e, Segments: segments}, nil
}

// toolExchange is the unit of an assistant message that asked for Tools together with
// the answers that follow it, one per call and in the order of the calls. It reports
// how many sources it consumed.
func toolExchange(sources []Source) (Unit, int, error) {
	head := sources[0]
	if !head.TextProjection {
		return Unit{}, 0, integrityf("a Tool call record is not stored as text")
	}
	rec, err := contextplan.DecodeToolCalls(head.Text)
	if err != nil {
		return Unit{}, 0, integrityf("a stored Tool call record is not valid")
	}
	n := 1 + len(rec.ToolCalls)
	if len(sources) < n {
		return Unit{}, 0, integrityf("a Tool call has no answer in the applied history")
	}
	e := contextplan.ProjectionEntry{Kind: KindToolExchange, Origin: OriginTool, Observations: []contextplan.ObservationSlot{},
		Messages:       []modelport.ChatMessage{{Role: "assistant", Content: rec.Content, ToolCalls: rec.ToolCalls}},
		MessageSources: [][]protocol.SourceRef{{head.Ref()}}}
	for k, call := range rec.ToolCalls {
		res := sources[1+k]
		if res.ItemKind != contextplan.ItemKindToolResult || res.ToolCallID != call.ID || !res.TextProjection {
			return Unit{}, 0, integrityf("the answers of a Tool exchange do not follow its calls in order")
		}
		e.Messages = append(e.Messages, modelport.Tool(call.ID, res.Text))
		e.MessageSources = append(e.MessageSources, []protocol.SourceRef{res.Ref()})
		ref, eligible := observationRef(call, res)
		e.Observations = append(e.Observations, contextplan.ObservationSlot{MessageOffset: int64(1 + k), Reference: ref, Eligible: eligible})
		e.Sequence = res.ContextSeq
	}
	return Unit{Entry: e}, n, nil
}

// observationRef is the reference of one Tool answer as it is live in the context:
// all of it presented, and seen when a generation was sent after it was applied. It is
// eligible for the reduction that replaces it with a reference only when it is the
// answer of a call that ran to an end (completed or failed): an answer that says the
// call did not run, or that its outcome is unknown, says it in its text, and that text
// is never swapped for a reference.
func observationRef(call modelport.ToolCall, res Source) (contextplan.ObservationReference, bool) {
	total := res.TotalBytes
	whole := []contextplan.Range{{Start: 0, End: total}}
	ref := contextplan.ObservationReference{
		FormatVersion: contextplan.ObservationFormat, ProviderToolCallID: call.ID, Tool: call.Function.Name, EvidenceID: res.EvidenceID,
		ProjectionVersion: TextProjection, RawHash: res.RawHash, ProjectionHash: res.RawHash, TotalBytes: total, CaptureComplete: res.CaptureComplete,
		PresentedRanges: whole, SeenRanges: []contextplan.Range{}, SummaryCoveredRange: []contextplan.Range{}, Partial: false,
		Retrieval: contextplan.ObservationRetrieval{Tool: "evidence.read", MaxBytes: 65536},
	}
	if res.Presented && total > 0 {
		ref.SeenRanges = clone(whole)
	}
	var view toolview.View
	if err := json.Unmarshal([]byte(res.Text), &view); err != nil {
		return ref, false
	}
	ref.CaptureComplete = res.CaptureComplete && view.CaptureComplete
	return ref, res.TextProjection && (view.EffectState == "completed" || view.EffectState == "failed")
}

// InheritedUnits turns the entries of the prior checkpoint into units. An
// instruction's retained text is taken back from its message and the ranges it was
// made from: the ranges, in order, are exactly the bytes of the message (the text of
// an automation input being the text inside its envelope), or the entry is
// contradictory.
func InheritedUnits(p contextplan.Projection) ([]Unit, error) {
	units := make([]Unit, 0, len(p.Entries))
	for _, e := range p.Entries {
		u := Unit{Entry: cloneEntry(e), Inherited: true}
		if e.Kind == KindInstruction {
			segs, err := segmentsOf(e)
			if err != nil {
				return nil, err
			}
			u.Segments = segs
		}
		units = append(units, u)
	}
	return units, nil
}

func segmentsOf(e contextplan.ProjectionEntry) ([]Segment, error) {
	if len(e.Messages) != 1 || len(e.MessageSources) != 1 || len(e.MessageSources[0]) == 0 || e.Messages[0].Role != "user" {
		return nil, integrityf("a retained instruction is not one message with its sources")
	}
	text := e.Messages[0].Text()
	switch e.Origin {
	case protocol.OriginHuman:
	case protocol.OriginAutomation:
		origin, inner, err := contextplan.ParseInputEnvelope(text)
		if err != nil || origin != protocol.OriginAutomation {
			return nil, integrityf("a retained automation instruction is not in its envelope")
		}
		text = inner
	default:
		return nil, integrityf("a retained instruction has an origin that is neither human nor automation")
	}
	var segs []Segment
	at := int64(0)
	for _, ref := range e.MessageSources[0] {
		n := rangeLen(ref)
		if n < 0 || at+n > int64(len(text)) {
			return nil, integrityf("the sources of a retained instruction are longer than its text")
		}
		segs = append(segs, Segment{Ref: ref, Text: text[at : at+n]})
		at += n
	}
	if at != int64(len(text)) {
		return nil, integrityf("the sources of a retained instruction do not make up its text")
	}
	return segs, nil
}

func cloneEntry(e contextplan.ProjectionEntry) contextplan.ProjectionEntry {
	out := e
	out.Messages = make([]modelport.ChatMessage, len(e.Messages))
	for i, m := range e.Messages {
		m.ToolCalls = clone(m.ToolCalls)
		if m.Content != nil {
			c := *m.Content
			m.Content = &c
		}
		out.Messages[i] = m
	}
	out.MessageSources = make([][]protocol.SourceRef, len(e.MessageSources))
	for i, s := range e.MessageSources {
		out.MessageSources[i] = clone(s)
	}
	out.Observations = make([]contextplan.ObservationSlot, len(e.Observations))
	for i, o := range e.Observations {
		o.Reference = cloneRef(o.Reference)
		out.Observations[i] = o
	}
	return out
}

func cloneRef(r contextplan.ObservationReference) contextplan.ObservationReference {
	r.PresentedRanges = clone(r.PresentedRanges)
	r.SeenRanges = clone(r.SeenRanges)
	r.SummaryCoveredRange = clone(r.SummaryCoveredRange)
	normalizeRef(&r)
	return r
}
