package compaction

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// TextReader gives the exact text of a stored Evidence, checked against the hash and the
// size it was stored with. The Summary data needs the text of an Observation whose
// content the live context no longer holds (it was replaced by a reference).
type TextReader func(evidenceID string) (string, error)

// ObsMeta is the Host's record of one Observation presented to Summary.
type ObsMeta struct {
	Handle   string
	Unit     int
	Slot     int
	Ref      contextplan.ObservationReference
	Excerpts []Chunk
	// IsExec says the call was a process.exec, and ExecOK that it ran to a successful end
	// (exit code 0): the only Tool answer a verification may rest on.
	IsExec, ExecOK bool
}

// SummaryInput is what the Summary stage is given and the Host's record of it: for every
// handle the exact stored sources it stands for.
type SummaryInput struct {
	Dataset      SummaryDataset
	Manifest     map[string][]protocol.SourceRef
	Observations []ObsMeta
	// CompletionOK is, per completion handle, whether it ended with exit code 0.
	CompletionOK map[string]bool
	// WorkPieces is how many Work pieces were presented.
	WorkPieces int
	prior      *contextplan.StoredSummary
}

// BuildSummaryInput (F09) builds the data of the Summary stage from the live context and
// the retention a selection left: the Work not yet summarized (split into pieces), the
// Observations of that Work as excerpts, the effective instructions as references, the
// one previous accepted Summary without its handles, the completions the selection used,
// and what is protected. No inventory, hash, ID, range or removed text is part of it:
// those stay with the Host (the manifest). Handles are 0-based without gaps in each of
// the five namespaces, and exist for this request only.
func BuildSummaryInput(p *Prepared, ret *Retention, sel *SelectionInput, read TextReader) (*SummaryInput, error) {
	in := &SummaryInput{Manifest: map[string][]protocol.SourceRef{}, CompletionOK: map[string]bool{}, prior: p.Summary}
	d := &in.Dataset
	d.FormatVersion = summaryFormat
	d.Work, d.Observations, d.Instructions = []Piece{}, []ObservationData{}, []Piece{}
	d.PriorSummaries, d.Completions, d.ProtectedState = []PriorSummary{}, []Completion{}, []ProtectedData{}

	for _, ui := range p.WorkUnits() {
		u := p.Units[ui]
		if len(u.Entry.MessageSources) == 0 || len(u.Entry.MessageSources[0]) != 1 {
			return nil, integrityf("a Work entry has no single source for its first message")
		}
		ref := u.Entry.MessageSources[0][0]
		text, err := workText(u)
		if err != nil {
			return nil, err
		}
		chunks := SplitText(text, MaxPieceBytes)
		for ci, ch := range chunks {
			if len(d.Work) >= maxPieces {
				return nil, semanticf("there are more than %d pieces of Work to present", maxPieces)
			}
			h := fmt.Sprintf("work-%d", len(d.Work))
			d.Work = append(d.Work, Piece{Handle: h, Origin: OriginAgent, Sequence: u.Seq(), ChunkIndex: int64(ci), ChunkCount: int64(len(chunks)), Text: ch.Text})
			in.Manifest[h] = []protocol.SourceRef{narrow(ref, ch.Start, ch.End)}
		}
		for si, slot := range u.Entry.Observations {
			text := u.Entry.Messages[slot.MessageOffset].Text()
			if strings.HasPrefix(text, "RENCROW_OBSERVATION_REFERENCE_V1\n") {
				if read == nil {
					return nil, integrityf("an Observation's text is needed and cannot be read")
				}
				var err error
				if text, err = read(slot.Reference.EvidenceID); err != nil {
					return nil, err
				}
			}
			sum := sha256.Sum256([]byte(text))
			if int64(len(text)) != slot.Reference.TotalBytes || hex.EncodeToString(sum[:]) != slot.Reference.RawHash {
				return nil, integrityf("an Observation's text is not the one its reference names")
			}
			osrc := u.Entry.MessageSources[slot.MessageOffset][0]
			meta := ObsMeta{Handle: fmt.Sprintf("observation-%d", len(d.Observations)), Unit: ui, Slot: si, Ref: cloneRef(slot.Reference), Excerpts: Edges(text)}
			meta.IsExec, meta.ExecOK = execOK(u, slot)
			obs := ObservationData{Handle: meta.Handle, Tool: slot.Reference.Tool, Sequence: u.Seq(), TotalBytes: slot.Reference.TotalBytes,
				CaptureComplete: slot.Reference.CaptureComplete}
			for _, ex := range meta.Excerpts {
				obs.Excerpts = append(obs.Excerpts, Excerpt{Range: contextplan.Range{Start: int64(ex.Start), End: int64(ex.End)}, Text: ex.Text})
				in.Manifest[meta.Handle] = append(in.Manifest[meta.Handle], narrow(osrc, ex.Start, ex.End))
			}
			obs.Partial = !(len(meta.Excerpts) == 1 && meta.Excerpts[0].Start == 0 && int64(meta.Excerpts[0].End) == slot.Reference.TotalBytes)
			d.Observations = append(d.Observations, obs)
			in.Observations = append(in.Observations, meta)
		}
	}

	instrHandle := map[int][]string{} // index of the unit in p.Units -> its instruction handles
	for k, u := range ret.Units {
		from := ret.From[k]
		switch {
		case u.Entry.Kind == KindInstruction:
			for _, seg := range u.Segments {
				chunks := SplitText(seg.Text, MaxPieceBytes)
				for ci, ch := range chunks {
					if len(d.Instructions) >= maxPieces {
						return nil, semanticf("there are more than %d instruction pieces to present", maxPieces)
					}
					h := fmt.Sprintf("instruction-%d", len(d.Instructions))
					d.Instructions = append(d.Instructions, Piece{Handle: h, Origin: u.Entry.Origin, Sequence: u.Seq(), ChunkIndex: int64(ci), ChunkCount: int64(len(chunks)),
						Text: ch.Text, ContinuingConstraint: u.Entry.Protected})
					in.Manifest[h] = []protocol.SourceRef{narrow(seg.Ref, ch.Start, ch.End)}
					instrHandle[from] = append(instrHandle[from], h)
				}
			}
		case u.Entry.Kind == KindProtected:
			_, text, err := contextplan.ParseInputEnvelope(u.Entry.Messages[0].Text())
			if err != nil {
				return nil, integrityf("a protected input is not in its envelope")
			}
			d.ProtectedState = append(d.ProtectedState, ProtectedData{Kind: "unknown", Description: "Input of unknown origin, kept as data", Text: &text, TotalBytes: int64(len(text))})
		}
	}

	if p.Summary != nil {
		src, err := p.summarySource()
		if err != nil {
			return nil, err
		}
		d.PriorSummaries = []PriorSummary{{Handle: "summary-0", Summary: summaryText(p.Summary.Summary)}}
		in.Manifest["summary-0"] = []protocol.SourceRef{{Owner: Owner, SourceID: src.Candidate.CheckpointID, RawHash: src.Hash, ProjectionVersion: "checkpoint/v1",
			Range: protocol.ByteRange{Start: 0, End: uint64(len(src.Bytes))}, Origin: OriginAgent, Sequence: uint64(p.SemanticBoundary)}}
	}

	for _, li := range ret.UsedLinks {
		l := sel.Links[li]
		handles := instrHandle[sel.Pieces[l.TargetPiece].Unit]
		if len(handles) == 0 {
			continue // an instruction with nothing left holds no completion
		}
		h := fmt.Sprintf("completion-%d", len(d.Completions))
		d.Completions = append(d.Completions, Completion{Handle: h, InstructionHandle: handles, CallText: l.Link.CallText, OutputText: l.Link.OutputText, ExitCode: l.Link.ExitCode})
		in.Manifest[h] = []protocol.SourceRef{l.CallRef, l.OutputRef}
		in.CompletionOK[h] = l.Link.ExitCode == 0
	}
	in.WorkPieces = len(d.Work)
	return in, nil
}

// narrow is the SourceRef of bytes a..b of the text ref names, which starts at ref's range
// start.
func narrow(ref protocol.SourceRef, a, b int) protocol.SourceRef {
	ref.Range = protocol.ByteRange{Start: ref.Range.Start + uint64(a), End: ref.Range.Start + uint64(b)}
	return ref
}

// workText is the text of a Work unit's first message as it is stored: the final text
// of the model, or the record of the Tool calls it made.
func workText(u Unit) (string, error) {
	m := u.Entry.Messages[0]
	if u.Entry.Kind == KindWork {
		return m.Text(), nil
	}
	b, err := contextplan.EncodeToolCalls(contextplan.ToolCallsRecord{Content: m.Content, ToolCalls: m.ToolCalls})
	if err != nil {
		return "", integrityf("a Tool call record cannot be encoded")
	}
	return string(b), nil
}

// execOK says whether the slot is the answer of a process.exec call and whether it ended
// successfully: effect completed and exit code 0.
func execOK(u Unit, slot contextplan.ObservationSlot) (isExec, ok bool) {
	k := int(slot.MessageOffset) - 1
	if k < 0 || k >= len(u.Entry.Messages[0].ToolCalls) || u.Entry.Messages[0].ToolCalls[k].Function.Name != "process.exec" {
		return false, false
	}
	var v struct {
		EffectState string `json:"effect_state"`
		ExitCode    *int64 `json:"exit_code"`
	}
	text := u.Entry.Messages[slot.MessageOffset].Text()
	if json.Unmarshal([]byte(text), &v) != nil {
		return true, false
	}
	return true, v.EffectState == "completed" && v.ExitCode != nil && *v.ExitCode == 0
}

func summaryText(s contextplan.SummaryOutput) SummaryText {
	out := SummaryText{CurrentWork: []textItem{}, Decisions: []decisionItem{}, Verification: []verifyItem{}, OpenItems: []textItem{}, NextSteps: []textItem{}}
	for _, i := range s.CurrentWork {
		out.CurrentWork = append(out.CurrentWork, textItem{i.Text})
	}
	for _, i := range s.Decisions {
		out.Decisions = append(out.Decisions, decisionItem{i.Text, i.Reason})
	}
	for _, i := range s.Verification {
		out.Verification = append(out.Verification, verifyItem{i.Text, i.State})
	}
	for _, i := range s.OpenItems {
		out.OpenItems = append(out.OpenItems, textItem{i.Text})
	}
	for _, i := range s.NextSteps {
		out.NextSteps = append(out.NextSteps, textItem{i.Text})
	}
	return out
}

// toolMarkup are the spellings of a Tool call written out as text. Whatever the model
// side already caught, a Summary that carries one is not adopted: it would put a call that
// was never made into the context as if it had been.
var toolMarkup = []string{"<tool_call", "</tool_call", "<function=", "<function_call", "<|tool_call", "<tools>", "[tool_calls]", "<|python_tag|>", "<invoke", "<tool_response"}

func hasToolMarkup(s string) bool {
	l := strings.ToLower(s)
	for _, m := range toolMarkup {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// SummaryResult is a Summary the Host accepts: what the model wrote, and the stored form
// with every handle resolved to the stored sources it stands for.
type SummaryResult struct {
	Output contextplan.SummaryOutput
	Stored contextplan.StoredSummary
	// Cited are the indices (into SummaryInput.Observations) of the Observations the
	// Summary cites, by a source handle or as important.
	Cited []int
}

// ValidateSummary (F10) accepts a Summary only when it is exactly what was asked for
// and is consistent with what the Host knows. The answer must be one strict JSON object
// that satisfies summary.schema.json, and then:
//
//   - every handle it cites is one that was presented in this request (a handle that was
//     not presented, has a gap, or comes from another request is refused), and
//     important_observation_handles are Observations;
//   - no text carries a Tool call written out as markup;
//   - a verification may be "passed" only if what it cites shows an explicit execution
//     that succeeded: a verified completion or a process.exec answer with exit code 0
//     (or the previous Summary, when that had such a passed verification itself); a
//     citation that contradicts it (a failed run) is refused;
//   - when there is Work, a Summary that says nothing at all is not a Summary.
//
// That the JSON is well formed does not make it correct: these are the checks the Host
// can make, and no more is claimed.
func ValidateSummary(in *SummaryInput, answer, newCheckpointID string) (*SummaryResult, error) {
	v, err := decodeStageOutput(schemacheck.Summary, answer)
	if err != nil {
		return nil, err
	}
	var out contextplan.SummaryOutput
	dec := json.NewDecoder(bytes.NewReader([]byte(answer)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return nil, semanticf("the Summary cannot be read")
	}
	_ = v
	var used []string
	note := func(h string) { used = append(used, h) }
	check := func(handles []string) error {
		for _, h := range handles {
			if _, ok := in.Manifest[h]; !ok {
				return semanticf("the Summary cites a handle that was not presented")
			}
			note(h)
		}
		return nil
	}
	texts := []string{}
	for _, i := range out.CurrentWork {
		texts = append(texts, i.Text)
		if err := check(i.SourceHandles); err != nil {
			return nil, err
		}
	}
	for _, i := range out.Decisions {
		texts = append(texts, i.Text, i.Reason)
		if err := check(i.SourceHandles); err != nil {
			return nil, err
		}
	}
	for _, i := range out.Verification {
		texts = append(texts, i.Text)
		if err := check(i.SourceHandles); err != nil {
			return nil, err
		}
		if i.State == "passed" {
			if err := in.passedHasEvidence(i.SourceHandles); err != nil {
				return nil, err
			}
		}
	}
	for _, i := range out.OpenItems {
		texts = append(texts, i.Text)
		if err := check(i.SourceHandles); err != nil {
			return nil, err
		}
	}
	for _, i := range out.NextSteps {
		texts = append(texts, i.Text)
		if err := check(i.SourceHandles); err != nil {
			return nil, err
		}
	}
	for _, h := range out.ImportantObservationHandles {
		if !strings.HasPrefix(h, "observation-") {
			return nil, semanticf("an important observation is not an Observation")
		}
		if err := check([]string{h}); err != nil {
			return nil, err
		}
	}
	for _, t := range texts {
		if hasToolMarkup(t) {
			return nil, semanticf("the Summary carries a Tool call written out as text")
		}
	}
	if in.WorkPieces > 0 && len(out.CurrentWork)+len(out.Decisions)+len(out.Verification)+len(out.OpenItems)+len(out.NextSteps) == 0 {
		return nil, semanticf("the Summary says nothing about the Work it was given")
	}

	res := &SummaryResult{Output: out}
	seen := map[string]bool{}
	stored := contextplan.StoredSummary{SourceCheckpointID: newCheckpointID, Summary: out, SourceMap: []contextplan.SourceMapEntry{}}
	for _, h := range used {
		if seen[h] {
			continue
		}
		seen[h] = true
		stored.SourceMap = append(stored.SourceMap, contextplan.SourceMapEntry{Handle: h, Sources: clone(in.Manifest[h])})
	}
	for i, o := range in.Observations {
		if seen[o.Handle] {
			res.Cited = append(res.Cited, i)
		}
	}
	res.Stored = stored
	return res, nil
}

// passedHasEvidence is the rule for a verification that says "passed".
func (in *SummaryInput) passedHasEvidence(handles []string) error {
	proven := false
	for _, h := range handles {
		switch {
		case strings.HasPrefix(h, "completion-"):
			if !in.CompletionOK[h] {
				return semanticf("a verification is passed on a completion that did not succeed")
			}
			proven = true
		case strings.HasPrefix(h, "observation-"):
			for _, o := range in.Observations {
				if o.Handle != h {
					continue
				}
				if o.IsExec && !o.ExecOK {
					return semanticf("a verification is passed on a run that did not succeed")
				}
				proven = proven || (o.IsExec && o.ExecOK)
			}
		case h == "summary-0":
			if in.prior != nil && priorProvedSomething(in.prior) {
				proven = true
			}
		}
	}
	if !proven {
		return semanticf("a verification is passed with no execution evidence behind it")
	}
	return nil
}

// priorProvedSomething says the previous Summary had a passed verification that itself
// rested on an executed Tool answer or a completion.
func priorProvedSomething(s *contextplan.StoredSummary) bool {
	for _, v := range s.Summary.Verification {
		if v.State != "passed" {
			continue
		}
		for _, h := range v.SourceHandles {
			if strings.HasPrefix(h, "completion-") || strings.HasPrefix(h, "observation-") {
				return true
			}
		}
	}
	return false
}

// MinimalSummary is the smallest valid Summary: every list empty. It is the floor the
// Preflight counts, a structure and not a meaning.
func MinimalSummary(checkpointID string) contextplan.StoredSummary {
	return contextplan.StoredSummary{SourceCheckpointID: checkpointID, SourceMap: []contextplan.SourceMapEntry{}, Summary: contextplan.SummaryOutput{
		CurrentWork: []contextplan.SummaryItem{}, Decisions: []contextplan.DecisionItem{}, Verification: []contextplan.VerificationItem{},
		OpenItems: []contextplan.SummaryItem{}, NextSteps: []contextplan.SummaryItem{}, ImportantObservationHandles: []string{}}}
}
