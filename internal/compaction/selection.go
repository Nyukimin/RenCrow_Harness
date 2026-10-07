package compaction

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

const maxPieces = 8192

// PieceMeta is the Host's record of one presented piece: the unit and segment it comes
// from and the exact range of the stored text it is. The model is given the piece; it
// never gives back a range, an ID or a hash.
type PieceMeta struct {
	Piece      Piece
	Unit       int
	Segment    int
	Start, End int                // byte range inside the segment's text
	Ref        protocol.SourceRef // the exact range of the stored text
}

// LinkMeta is the Host's record of an offered completion: the piece it completes and
// the stored sources of the call and of its answer.
type LinkMeta struct {
	Link        CompletionLink
	TargetPiece int
	CallRef     protocol.SourceRef
	OutputRef   protocol.SourceRef
}

// SelectionInput is what the Selection stage is given and the Host's record of it.
type SelectionInput struct {
	Dataset SelectionDataset
	Pieces  []PieceMeta
	Links   []LinkMeta
}

// HasCandidates says whether a selection can change anything: a later fragment can
// supersede an earlier one, or a completion is offered. One instruction alone and no
// completion leave nothing to select, and the stage is not requested.
func (in *SelectionInput) HasCandidates() bool {
	return len(in.Pieces) >= 2 || len(in.Links) > 0
}

// BuildSelectionDataset (F07) presents the effective instructions of the live context
// as pieces (handle presented-N, 0-based and without gaps, in application order, then
// the range of the stored text, then chunk order), each text at most MaxPieceBytes, and
// the completion links the Host can verify. Only instructions of verified human or
// automation origin are presented, so nothing else can be removed; a fragment the Host
// marks as a continuing constraint is presented with that flag, which no answer can
// clear. The data being too large for one request is ErrSemantic: a part is never left
// out and treated as done.
func BuildSelectionDataset(p *Prepared) (*SelectionInput, error) {
	in := &SelectionInput{Dataset: SelectionDataset{FormatVersion: selectionFormat, PresentedSources: []Piece{}, CompletionLinks: []CompletionLink{}}}
	for ui, u := range p.Units {
		if u.Entry.Kind != KindInstruction || (u.Entry.Origin != protocol.OriginHuman && u.Entry.Origin != protocol.OriginAutomation) {
			continue
		}
		for si, seg := range u.Segments {
			chunks := SplitText(seg.Text, MaxPieceBytes)
			for ci, ch := range chunks {
				if len(in.Pieces) >= maxPieces {
					return nil, semanticf("there are more than %d fragments to present", maxPieces)
				}
				ref := seg.Ref
				ref.Range = protocol.ByteRange{Start: seg.Ref.Range.Start + uint64(ch.Start), End: seg.Ref.Range.Start + uint64(ch.End)}
				piece := Piece{Handle: fmt.Sprintf("presented-%d", len(in.Pieces)), Origin: u.Entry.Origin, Sequence: u.Seq(), ChunkIndex: int64(ci),
					ChunkCount: int64(len(chunks)), Text: ch.Text, ContinuingConstraint: u.Entry.Protected}
				in.Pieces = append(in.Pieces, PieceMeta{Piece: piece, Unit: ui, Segment: si, Start: ch.Start, End: ch.End, Ref: ref})
				in.Dataset.PresentedSources = append(in.Dataset.PresentedSources, piece)
			}
		}
	}
	in.Links = completionLinks(p, in.Pieces)
	for _, l := range in.Links {
		in.Dataset.CompletionLinks = append(in.Dataset.CompletionLinks, l.Link)
	}
	return in, nil
}

// execPair is a process.exec call that ran to an end, with its answer in full.
type execPair struct {
	unit            int
	command, output string
	exit            int64
	callRef, outRef protocol.SourceRef
}

type execView struct {
	EffectState string `json:"effect_state"`
	ExitCode    *int64 `json:"exit_code"`
	Result      *struct {
		TimedOut bool `json:"timed_out"`
		Signaled bool `json:"signaled"`
		Stdout   struct {
			CaptureComplete  bool   `json:"capture_complete"`
			Preview          string `json:"preview"`
			PreviewTruncated bool   `json:"preview_truncated"`
		} `json:"stdout"`
		Stderr struct {
			CaptureComplete  bool   `json:"capture_complete"`
			Preview          string `json:"preview"`
			PreviewTruncated bool   `json:"preview_truncated"`
		} `json:"stderr"`
	} `json:"result"`
}

// execPairs lists the terminal process.exec pairs of the live context that may serve as
// a completion: the call ran to an end with a known exit code, was not stopped by a
// timeout or a signal, its output is complete and shown in full, the call and the
// output are each at most MaxLinkBytes, and the answer is still the full text (a
// reference marker is not evidence of a run).
func execPairs(p *Prepared) []execPair {
	var out []execPair
	for ui, u := range p.Units {
		if u.Entry.Kind != KindToolExchange || u.Entry.Protected || len(u.Entry.Messages) < 2 {
			continue
		}
		calls := u.Entry.Messages[0].ToolCalls
		for _, slot := range u.Entry.Observations {
			k := int(slot.MessageOffset) - 1
			if k < 0 || k >= len(calls) || !slot.Eligible || !slot.Reference.CaptureComplete || calls[k].Function.Name != "process.exec" {
				continue
			}
			content := u.Entry.Messages[slot.MessageOffset].Text()
			if strings.HasPrefix(content, "RENCROW_OBSERVATION_REFERENCE_V1\n") {
				continue
			}
			var v execView
			if err := json.Unmarshal([]byte(content), &v); err != nil || v.ExitCode == nil || v.Result == nil || (v.EffectState != "completed" && v.EffectState != "failed") {
				continue
			}
			r := v.Result
			if r.TimedOut || r.Signaled || !r.Stdout.CaptureComplete || !r.Stderr.CaptureComplete || r.Stdout.PreviewTruncated || r.Stderr.PreviewTruncated {
				continue
			}
			args, err := strictjson.Decode([]byte(calls[k].Function.Arguments))
			obj, ok := args.(map[string]any)
			if err != nil || !ok {
				continue
			}
			exe, ok := obj["executable"].(string)
			if !ok || exe == "" {
				continue
			}
			words := []string{path.Base(exe)}
			if argv, ok := obj["argv"].([]any); ok {
				for _, a := range argv {
					s, ok := a.(string)
					if !ok {
						words = nil
						break
					}
					words = append(words, s)
				}
			}
			if words == nil {
				continue
			}
			command, output := strings.Join(words, " "), r.Stdout.Preview+r.Stderr.Preview
			if len(command) > MaxLinkBytes || len(output) > MaxLinkBytes {
				continue
			}
			out = append(out, execPair{unit: ui, command: command, output: output, exit: *v.ExitCode,
				callRef: u.Entry.MessageSources[0][0], outRef: u.Entry.MessageSources[slot.MessageOffset][0]})
		}
	}
	return out
}

// namesCommand says whether a fragment names a command explicitly: the whole command is set
// off as code (between backticks) or stands alone on a line, with at most a shell prompt
// before it. A command that only occurs inside a sentence is not named: the sentence may
// forbid it as well as ask for it.
func namesCommand(text, command string) bool {
	if command == "" {
		return false
	}
	if strings.Contains(text, "`"+command+"`") {
		return true
	}
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		l = strings.TrimPrefix(strings.TrimPrefix(l, "$ "), "> ")
		if l == command {
			return true
		}
	}
	return false
}

// completionLinks offers a completion only where the Host can tie one call to one
// instruction by what the instruction itself says: the instruction is one fragment that
// names the command (its program and arguments, as written, set off as code or on a line of
// its own) in full, the call came
// after it, and neither of the two matches anything else: the fragment names no other
// call, and the call is named by no other fragment. An instruction split into
// several fragments never gets one, since no fragment can be shown to hold its whole
// condition. A link is a candidate for the model and the Host to weigh, never a proof
// that the request was met.
func completionLinks(p *Prepared, pieces []PieceMeta) []LinkMeta {
	pairs := execPairs(p)
	type match struct{ piece, pair int }
	var matches []match
	for pi, pm := range pieces {
		if pm.Piece.ChunkCount != 1 || len(p.Units[pm.Unit].Segments) != 1 {
			continue
		}
		for xi, x := range pairs {
			if p.Units[x.unit].Seq() > p.Units[pm.Unit].Seq() && namesCommand(pm.Piece.Text, x.command) {
				matches = append(matches, match{pi, xi})
			}
		}
	}
	perPiece, perPair := map[int]int{}, map[int]int{}
	for _, m := range matches {
		perPiece[m.piece]++
		perPair[m.pair]++
	}
	var out []LinkMeta
	for _, m := range matches {
		if perPiece[m.piece] != 1 || perPair[m.pair] != 1 {
			continue
		}
		x := pairs[m.pair]
		out = append(out, LinkMeta{
			Link: CompletionLink{Handle: fmt.Sprintf("completion-%d", len(out)), TargetHandle: pieces[m.piece].Piece.Handle,
				CallText: x.command, OutputText: x.output, ExitCode: x.exit},
			TargetPiece: m.piece, CallRef: x.callRef, OutputRef: x.outRef})
	}
	return out
}

// Rejection is a selection operation the Host did not adopt, and why. It names the
// operation by position and never repeats what the model wrote.
type Rejection struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// Retention is the effective instructions after a selection (RetentionPlan): the units
// with what was removed taken out of them, the selections that were applied, and the
// completions the Summary is told of. The same Retention feeds the Summary data and the
// candidate, so the two are never selected twice.
type Retention struct {
	Units []Unit
	// From is, for each retained unit, its index in the Prepared units it was made from.
	From      []int
	Applied   []AppliedOp
	UsedLinks []int
	Rejected  []Rejection
}

// NoSelection is the retention of a context nothing was selected from: every unit stays.
func NoSelection(p *Prepared) *Retention {
	r := &Retention{Units: clone(p.Units), From: make([]int, len(p.Units))}
	for i := range r.From {
		r.From[i] = i
	}
	return r
}

// interval is a byte range inside one segment's text.
type interval struct{ a, b int }

type opKey struct{ unit, seg int }

// ApplySelection (F08) applies what the Selection stage answered. The answer must be
// one strict JSON object that satisfies selection.schema.json; anything else is
// ErrSemantic. Each operation is then judged on its own and adopted or refused:
//
//   - a handle must be one that was presented, and the quote must occur exactly once in
//     the piece it is quoted from (a quote that cannot be matched, or that is cut by a
//     piece boundary, removes nothing);
//   - a fragment the Host marks as a continuing constraint is never removed;
//   - drop_superseded needs a later fragment as its reason, from an author who may
//     retire the target: a human instruction only by a human, an automation instruction
//     by a human or an automation. "Later" is the order the inputs were accepted in. A
//     bare "stop doing A" is a reason; no replacement is needed;
//   - replace_completed needs a completion that was offered for exactly that fragment;
//   - the text a removal rests on is never removed by another operation.
//
// Only the quoted text is removed, never the rest of its source. The offsets are the
// Host's, computed from the pieces; no range, hash or ID comes from the model.
func ApplySelection(p *Prepared, in *SelectionInput, answer string) (*Retention, error) {
	v, err := decodeStageOutput(schemacheck.Selection, answer)
	if err != nil {
		return nil, err
	}
	rawOps, _ := v.(map[string]any)["operations"].([]any)
	byHandle := map[string]int{}
	for i, pm := range in.Pieces {
		byHandle[pm.Piece.Handle] = i
	}
	linkByHandle := map[string]int{}
	for i, l := range in.Links {
		linkByHandle[l.Link.Handle] = i
	}
	ret := &Retention{}
	removed := map[opKey][]interval{}
	type basis struct{ piece, a, b int } // a quote the adopted removals rest on, inside a piece
	var bases []basis
	var targets []basis

	overlaps := func(x, y basis) bool { return x.piece == y.piece && x.a < y.b && y.a < x.b }
	reject := func(i int, reason string) { ret.Rejected = append(ret.Rejected, Rejection{Index: i, Reason: reason}) }

	for i, ro := range rawOps {
		op := ro.(map[string]any)
		tpi, ok := byHandle[str(op["target_handle"])]
		if !ok {
			reject(i, "the target is not a presented fragment")
			continue
		}
		target := in.Pieces[tpi]
		if target.Piece.ContinuingConstraint {
			reject(i, "the target is a continuing constraint")
			continue
		}
		ta, found := uniqueIndex(target.Piece.Text, str(op["target_quote"]))
		if !found {
			reject(i, "the target quote does not occur exactly once in its fragment")
			continue
		}
		tq := basis{tpi, ta, ta + len(str(op["target_quote"]))}
		var (
			evidence []protocol.SourceRef
			kind     string
			rep      *basis
			link     = -1
		)
		switch str(op["operation"]) {
		case "drop_superseded":
			rpi, ok := byHandle[str(op["replacement_handle"])]
			if !ok {
				reject(i, "the reason is not a presented fragment")
				continue
			}
			reason := in.Pieces[rpi]
			ra, found := uniqueIndex(reason.Piece.Text, str(op["replacement_quote"]))
			if !found {
				reject(i, "the reason quote does not occur exactly once in its fragment")
				continue
			}
			if !mayRetire(target.Piece.Origin, reason.Piece.Origin) {
				reject(i, "the reason is not from an author who may retire this instruction")
				continue
			}
			if !later(reason.Ref, target.Ref) {
				reject(i, "the reason does not come after the target")
				continue
			}
			kind = str(op["basis"])
			rb := basis{rpi, ra, ra + len(str(op["replacement_quote"]))}
			rep = &rb
			rr := reason.Ref
			rr.Range = protocol.ByteRange{Start: reason.Ref.Range.Start + uint64(rb.a), End: reason.Ref.Range.Start + uint64(rb.b)}
			evidence = []protocol.SourceRef{rr}
		case "replace_completed":
			li, ok := linkByHandle[str(op["completion_handle"])]
			if !ok || in.Links[li].TargetPiece != tpi {
				reject(i, "no completion was offered for this fragment")
				continue
			}
			kind, link = "completion", li
			evidence = []protocol.SourceRef{in.Links[li].CallRef, in.Links[li].OutputRef}
		default:
			reject(i, "an operation this build does not know")
			continue
		}
		clash := false
		for _, b := range bases {
			clash = clash || overlaps(tq, b)
		}
		for _, t := range targets {
			clash = clash || (rep != nil && overlaps(*rep, t))
		}
		if clash || (rep != nil && overlaps(tq, *rep)) {
			reject(i, "the removal and the text another removal rests on overlap")
			continue
		}
		if link >= 0 && wholeOfMessage(p, in, tpi, tq, removed) {
			// The completed text is all that is left of the instruction: there would be
			// nothing for the completion to be attached to, so it stays.
			reject(i, "the completion would remove the whole instruction")
			continue
		}
		pm := in.Pieces[tpi]
		k := opKey{pm.Unit, pm.Segment}
		removed[k] = append(removed[k], interval{pm.Start + tq.a, pm.Start + tq.b})
		targets = append(targets, tq)
		if rep != nil {
			bases = append(bases, *rep)
		}
		tref := pm.Ref
		tref.Range = protocol.ByteRange{Start: pm.Ref.Range.Start + uint64(tq.a), End: pm.Ref.Range.Start + uint64(tq.b)}
		ret.Applied = append(ret.Applied, AppliedOp{Target: tref, Basis: kind, Evidence: evidence})
		if link >= 0 {
			ret.UsedLinks = append(ret.UsedLinks, link)
		}
	}
	units, from, err := removeText(p.Units, removed)
	if err != nil {
		return nil, err
	}
	ret.Units, ret.From = units, from
	return ret, nil
}

// wholeOfMessage reports whether removing q (and what was already removed) would leave
// no text of the message the piece belongs to.
func wholeOfMessage(p *Prepared, in *SelectionInput, piece int, q struct{ piece, a, b int }, removed map[opKey][]interval) bool {
	pm := in.Pieces[piece]
	u := p.Units[pm.Unit]
	trial := map[opKey][]interval{}
	for k, v := range removed {
		trial[k] = clone(v)
	}
	k := opKey{pm.Unit, pm.Segment}
	trial[k] = append(trial[k], interval{pm.Start + q.a, pm.Start + q.b})
	for si, seg := range u.Segments {
		if len(remaining(seg.Text, trial[opKey{pm.Unit, si}])) != 0 {
			return false
		}
	}
	return true
}

func str(v any) string { s, _ := v.(string); return s }

// uniqueIndex is where quote occurs in text when it occurs exactly once, overlapping
// occurrences included.
func uniqueIndex(text, quote string) (int, bool) {
	if quote == "" {
		return 0, false
	}
	i := strings.Index(text, quote)
	if i < 0 || strings.Contains(text[i+1:], quote) {
		return 0, false
	}
	return i, true
}

// mayRetire says whether a later fragment of origin by may retire an earlier one of
// origin target: a human instruction only by a human, an automation instruction by a
// human or an automation. Nothing else is ever presented as a target.
func mayRetire(target, by string) bool {
	switch target {
	case protocol.OriginHuman:
		return by == protocol.OriginHuman
	case protocol.OriginAutomation:
		return by == protocol.OriginHuman || by == protocol.OriginAutomation
	}
	return false
}

// later says whether b was accepted after a: the acceptance order of the stored inputs,
// then the position inside the input.
func later(b, a protocol.SourceRef) bool {
	if b.Sequence != a.Sequence {
		return b.Sequence > a.Sequence
	}
	return b.Range.Start > a.Range.Start
}

// remaining is the parts of text outside the removed intervals, as intervals, in order.
func remaining(text string, removed []interval) []interval {
	rs := clone(removed)
	sort.Slice(rs, func(i, j int) bool { return rs[i].a < rs[j].a })
	var out []interval
	at := 0
	for _, r := range rs {
		if r.a > at {
			out = append(out, interval{at, r.a})
		}
		at = max(at, r.b)
	}
	if at < len(text) {
		out = append(out, interval{at, len(text)})
	}
	return out
}

// removeText takes the removed intervals out of the instruction units. A segment left in
// parts becomes one segment per part; an instruction with nothing left is dropped; the
// message and its sources are made again from what is left, by the same rendering the
// live history gets.
func removeText(units []Unit, removed map[opKey][]interval) ([]Unit, []int, error) {
	out := make([]Unit, 0, len(units))
	from := make([]int, 0, len(units))
	for ui, u := range units {
		touched := false
		for si := range u.Segments {
			if len(removed[opKey{ui, si}]) > 0 {
				touched = true
			}
		}
		if !touched {
			out, from = append(out, u), append(from, ui)
			continue
		}
		var segs []Segment
		for si, seg := range u.Segments {
			for _, r := range remaining(seg.Text, removed[opKey{ui, si}]) {
				ref := seg.Ref
				ref.Range = protocol.ByteRange{Start: seg.Ref.Range.Start + uint64(r.a), End: seg.Ref.Range.Start + uint64(r.b)}
				segs = append(segs, Segment{Ref: ref, Text: seg.Text[r.a:r.b]})
			}
		}
		if len(segs) == 0 {
			continue
		}
		next, err := instructionUnit(u, segs)
		if err != nil {
			return nil, nil, err
		}
		out, from = append(out, next), append(from, ui)
	}
	return out, from, nil
}

// instructionUnit is an instruction unit made of the given segments, keeping what the
// unit was otherwise (sequence, origin, protection).
func instructionUnit(like Unit, segs []Segment) (Unit, error) {
	var text strings.Builder
	refs := make([]protocol.SourceRef, 0, len(segs))
	for _, s := range segs {
		text.WriteString(s.Text)
		refs = append(refs, s.Ref)
	}
	kind := contextplan.HistoryHuman
	if like.Entry.Origin == protocol.OriginAutomation {
		kind = contextplan.HistoryAutomation
	}
	msg, err := contextplan.HistoryMessage(contextplan.HistoryItem{ContextSeq: like.Entry.Sequence, HistoryKind: kind, Origin: like.Entry.Origin, Text: text.String()})
	if err != nil {
		return Unit{}, integrityf("a retained instruction cannot be rendered")
	}
	e := like.Entry
	e.Messages, e.MessageSources, e.Observations = []modelport.ChatMessage{msg}, [][]protocol.SourceRef{refs}, []contextplan.ObservationSlot{}
	return Unit{Entry: e, Segments: segs, Inherited: like.Inherited}, nil
}
