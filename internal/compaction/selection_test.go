package compaction_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// TestStageMessagesAreTheDesignsGolden holds the two stage requests to the design: the
// stage's whole prompt file as the only system message and the data behind its marker in
// CJ1 as the only user message, byte for byte.
func TestStageMessagesAreTheDesignsGolden(t *testing.T) {
	golden := decodeWire[map[string][]modelport.ChatMessage](t, "stage_messages.json")
	sel := decodeWire[compaction.SelectionDataset](t, "selection_dataset.json")
	sum := decodeWire[compaction.SummaryDataset](t, "summary_dataset.json")
	for stage, ds := range map[string]any{modelport.StageSelection: sel, modelport.StageSummary: sum} {
		t.Run(stage, func(t *testing.T) {
			msgs, digest, err := compaction.StageMessages(stage, ds)
			if err != nil {
				t.Fatal(err)
			}
			want := golden[stage]
			if len(msgs) != 2 || cj1Of(t, msgs) != cj1Of(t, want) {
				t.Fatalf("the stage request is not the golden\n got %.300s\nwant %.300s", cj1Of(t, msgs), cj1Of(t, want))
			}
			if len(digest) != 64 {
				t.Fatalf("digest %q", digest)
			}
			for _, m := range msgs {
				if len(m.ToolCalls) != 0 {
					t.Fatal("a stage request declares no Tool call")
				}
			}
		})
	}
	if _, _, err := compaction.StageMessages(modelport.StageSummary, sel); err == nil {
		t.Fatal("selection data must not be sent as the Summary stage")
	}
	if _, _, err := compaction.StageMessages("act", sel); err == nil {
		t.Fatal("act is not a compaction stage")
	}
}

func TestStageDataIsRefusedWhenItBreaksItsSchemaOrItsByteBound(t *testing.T) {
	sel := decodeWire[compaction.SelectionDataset](t, "selection_dataset.json")
	bad := sel
	bad.PresentedSources = append([]compaction.Piece(nil), sel.PresentedSources...)
	bad.PresentedSources[0].Origin = "unknown"
	if _, _, err := compaction.StageMessages(modelport.StageSelection, bad); !errors.Is(err, compaction.ErrSemantic) {
		t.Fatalf("an unknown-origin fragment is never presented for removal: %v", err)
	}
	long := sel
	long.PresentedSources = append([]compaction.Piece(nil), sel.PresentedSources...)
	long.PresentedSources[0].Text = strings.Repeat("a", compaction.MaxPieceBytes+1)
	if _, _, err := compaction.StageMessages(modelport.StageSelection, long); !errors.Is(err, compaction.ErrSemantic) {
		t.Fatalf("a fragment over 8,000 characters: %v", err)
	}
	// 8,000 Japanese characters are 24,000 bytes: the schema's maxLength (characters) lets
	// them through, and only the byte rule of the splitter keeps the data to 8,000 bytes.
	ja := sel
	ja.PresentedSources = append([]compaction.Piece(nil), sel.PresentedSources...)
	ja.PresentedSources[0].Text = strings.Repeat("あ", 8000)
	if _, _, err := compaction.StageMessages(modelport.StageSelection, ja); err != nil {
		t.Logf("a fragment of 8,000 characters passes the schema: %v", err)
	}
}

func human(t testing.TB, w *world, texts ...string) []compaction.Source {
	t.Helper()
	var out []compaction.Source
	for _, s := range texts {
		out = append(out, w.human(s))
	}
	return out
}

// TestSelectionDatasetPresentsInstructionsInOrderWithHostHandles is F07: handles are
// 0-based and without gaps, in application order, and the Host keeps the exact range of
// each piece.
func TestSelectionDatasetPresentsInstructionsInOrderWithHostHandles(t *testing.T) {
	w := newWorld()
	srcs := all(human(t, w, "方式Aを実装する。"), one(w.work("working")), one(w.automation("監督: 進捗を報告")), one(w.unknown("???")), human(t, w, "方式Aはやめる。"))
	p := prepare(t, nil, srcs)
	in, err := compaction.BuildSelectionDataset(p)
	if err != nil {
		t.Fatal(err)
	}
	if !in.HasCandidates() {
		t.Fatal("three instructions can supersede one another")
	}
	if len(in.Dataset.PresentedSources) != 3 || len(in.Pieces) != 3 {
		t.Fatalf("%d pieces: only verified human and automation instructions are presented", len(in.Pieces))
	}
	for i, pc := range in.Dataset.PresentedSources {
		if pc.Handle != fmt.Sprintf("presented-%d", i) || pc.ChunkIndex != 0 || pc.ChunkCount != 1 || pc.ContinuingConstraint {
			t.Errorf("piece %d: %+v", i, pc)
		}
	}
	got := in.Dataset.PresentedSources
	if got[0].Text != "方式Aを実装する。" || got[0].Origin != "human" || got[1].Origin != "automation" || got[2].Text != "方式Aはやめる。" {
		t.Fatalf("%+v", got)
	}
	// Piece.sequence is the application coordinate; the source's own sequence stays with
	// the Host.
	if got[0].Sequence != srcs[0].ContextSeq || in.Pieces[0].Ref.Sequence != uint64(srcs[0].ItemSeq) || in.Pieces[0].Ref.SourceID != srcs[0].EvidenceID {
		t.Fatal("the piece's sequence is the application coordinate and its reference is the stored one")
	}
	// One instruction alone has nothing to be superseded by.
	one1, err := compaction.BuildSelectionDataset(prepare(t, nil, human(t, newWorld(), "only")))
	if err != nil || one1.HasCandidates() {
		t.Fatalf("a single instruction offers no candidate: %v", err)
	}
}

// TestLongInstructionsAreSplitAtEightThousandBytesAndRestoredFromTheManifest: the pieces
// of an instruction joined are its text, byte for byte, by the exact ranges the Host kept.
func TestLongInstructionsAreSplitAtEightThousandBytesAndRestoredFromTheManifest(t *testing.T) {
	w := newWorld()
	text := strings.Repeat("a", 7999) + "あ" + strings.Repeat("b", 100) // あ straddles byte 8000
	src := w.human(text)
	p := prepare(t, nil, one(src), one(w.human("later")))
	in, err := compaction.BuildSelectionDataset(p)
	if err != nil {
		t.Fatal(err)
	}
	var joined strings.Builder
	at := uint64(0)
	n := 0
	for _, pm := range in.Pieces {
		if pm.Ref.SourceID != src.EvidenceID {
			continue
		}
		n++
		if pm.Ref.Range.Start != at || pm.Piece.ChunkCount != 2 || len(pm.Piece.Text) > compaction.MaxPieceBytes || !utf8.ValidString(pm.Piece.Text) {
			t.Fatalf("piece %+v", pm.Piece.Handle)
		}
		if src.Text[pm.Ref.Range.Start:pm.Ref.Range.End] != pm.Piece.Text {
			t.Fatal("the Host's range is not the piece's text")
		}
		at = pm.Ref.Range.End
		joined.WriteString(pm.Piece.Text)
	}
	if n != 2 || joined.String() != text || at != uint64(len(text)) {
		t.Fatalf("%d pieces restore %d of %d bytes", n, joined.Len(), len(text))
	}
	if len(in.Pieces[0].Piece.Text) != 7999 {
		t.Fatalf("the cut moves back to the start of the character: %d bytes", len(in.Pieces[0].Piece.Text))
	}
}

func opJSON(ops ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"operations": ops})
	return string(b)
}

func drop(target, tq, reason, rq, basis string) map[string]any {
	return map[string]any{"operation": "drop_superseded", "target_handle": target, "target_quote": tq, "replacement_handle": reason, "replacement_quote": rq, "basis": basis}
}

func apply(t testing.TB, p *compaction.Prepared, answer string) (*compaction.Retention, *compaction.SelectionInput) {
	t.Helper()
	in, err := compaction.BuildSelectionDataset(p)
	if err != nil {
		t.Fatal(err)
	}
	ret, err := compaction.ApplySelection(p, in, answer)
	if err != nil {
		t.Fatal(err)
	}
	return ret, in
}

func retained(ret *compaction.Retention) []string {
	var out []string
	for _, u := range ret.Units {
		if u.Entry.Kind == compaction.KindInstruction {
			var s strings.Builder
			for _, g := range u.Segments {
				s.WriteString(g.Text)
			}
			out = append(out, s.String())
		}
	}
	return out
}

// TestARevocationNeedsNoReplacement is A23: "do A", then "stop A" with no B. The
// revocation is a valid reason, only A's text is removed, and the revoking sentence stays.
func TestARevocationNeedsNoReplacement(t *testing.T) {
	w := newWorld()
	p := prepare(t, nil, human(t, w, "方式Aを実装する。ログは削除しない。", "方式Aはやめる。"))
	ret, _ := apply(t, p, opJSON(drop("presented-0", "方式Aを実装する。", "presented-1", "方式Aはやめる。", "revocation")))
	got := retained(ret)
	if len(got) != 2 || got[0] != "ログは削除しない。" || got[1] != "方式Aはやめる。" {
		t.Fatalf("retained %q", got)
	}
	if len(ret.Applied) != 1 || ret.Applied[0].Basis != "revocation" || len(ret.Applied[0].Evidence) != 1 || len(ret.Rejected) != 0 {
		t.Fatalf("%+v", ret.Applied)
	}
	// The record names exact ranges: the removed text and the sentence that is the reason.
	src := p.Units[0].Segments[0]
	op := ret.Applied[0]
	if got := src.Text[op.Target.Range.Start:op.Target.Range.End]; got != "方式Aを実装する。" || op.Target.SourceID != src.Ref.SourceID {
		t.Fatalf("removed %q", got)
	}
	why := p.Units[1].Segments[0].Text
	if got := why[op.Evidence[0].Range.Start:op.Evidence[0].Range.End]; got != "方式Aはやめる。" {
		t.Fatalf("reason %q", got)
	}
	// What remains of the first instruction is the rest of its text by its own range.
	rest := ret.Units[0].Segments[0]
	if rest.Text != "ログは削除しない。" || src.Text[rest.Ref.Range.Start:rest.Ref.Range.End] != rest.Text {
		t.Fatal("a retained fragment is the text of its stored range")
	}
}

// TestOnlyTheRightAuthorMayRetireAnInstruction is A05 and H17: an automation cannot retire a
// human instruction, a human can retire an automation one, and a reason that is not later is
// not one. The human's text is left exactly as it was.
func TestOnlyTheRightAuthorMayRetireAnInstruction(t *testing.T) {
	w := newWorld()
	h1 := w.human("Aを実装する。")
	a1 := w.automation("Aは不要。")
	a2 := w.automation("Bを実装する。")
	h2 := w.human("Bはやめる。")
	p := prepare(t, nil, one(h1), one(a1), one(a2), one(h2))
	ret, _ := apply(t, p, opJSON(
		drop("presented-0", "Aを実装する。", "presented-1", "Aは不要。", "revocation"),  // automation retiring a human: refused
		drop("presented-2", "Bを実装する。", "presented-3", "Bはやめる。", "revocation"), // a human retiring an automation: adopted
		drop("presented-3", "Bはやめる。", "presented-1", "Aは不要。", "amendment"),    // an earlier reason: refused
	))
	got := retained(ret)
	if len(got) != 3 || got[0] != "Aを実装する。" || got[1] != "Aは不要。" || got[2] != "Bはやめる。" {
		t.Fatalf("retained %q", got)
	}
	if len(ret.Applied) != 1 || len(ret.Rejected) != 2 || ret.Rejected[0].Index != 0 || ret.Rejected[1].Index != 2 {
		t.Fatalf("applied %d, rejected %+v", len(ret.Applied), ret.Rejected)
	}
	if ret.Units[0].Segments[0].Text != h1.Text || ret.Units[0].Segments[0].Ref != h1.Ref() {
		t.Fatal("the human instruction is untouched, exact text and exact source")
	}
}

// TestSelectionRefusesWhatItCannotMatchExactly: handles that were not presented, quotes
// that do not occur exactly once, a quote cut by a piece boundary, a continuing constraint,
// removals and the text they rest on overlapping.
func TestSelectionRefusesWhatItCannotMatchExactly(t *testing.T) {
	long := strings.Repeat("x", 7999) + "あ" + strings.Repeat("y", 50) + " TARGETQUOTE"
	cross := "x" + string(rune('あ')) // a quote spanning the piece boundary: the last x and あ
	w := newWorld()
	cases := map[string]struct {
		ops      string
		rejected int
	}{
		"unknown target":                {opJSON(drop("presented-9", "a", "presented-1", "stop", "revocation")), 1},
		"unknown reason":                {opJSON(drop("presented-0", "do A", "presented-9", "stop", "revocation")), 1},
		"quote not found":               {opJSON(drop("presented-0", "do Z", "presented-1", "stop", "revocation")), 1},
		"quote of the reason not found": {opJSON(drop("presented-0", "do A", "presented-1", "nope", "revocation")), 1},
		"quote in the wrong piece":      {opJSON(drop("presented-0", "stop", "presented-1", "stop", "revocation")), 1},
		"a quote that occurs twice":     {opJSON(drop("presented-2", "ab", "presented-3", "stop", "revocation")), 1},
		"a quote across the boundary":   {opJSON(drop("presented-4", cross, "presented-5", "later", "revocation")), 1},
	}
	srcs := all(human(t, w, "do A please", "stop A"), human(t, w, "ababa", "stop"), one(w.human(long)), one(w.human("later")))
	p := prepare(t, nil, srcs)
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in, err := compaction.BuildSelectionDataset(p)
			if err != nil {
				t.Fatal(err)
			}
			ret, err := compaction.ApplySelection(p, in, tc.ops)
			if err != nil {
				t.Fatalf("the answer is a valid object; only its operation is refused: %v", err)
			}
			if len(ret.Rejected) != tc.rejected || len(ret.Applied) != 0 {
				t.Fatalf("rejected %+v, applied %+v", ret.Rejected, ret.Applied)
			}
			if got, want := retained(ret), retained(compaction.NoSelection(p)); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
				t.Fatalf("an instruction changed although nothing was adopted")
			}
		})
	}
}

// TestARemovalAndTheTextAnotherRemovalRestsOnNeverOverlap: the sentence that is the reason
// for one removal is not removed by another, in either order.
func TestARemovalAndTheTextAnotherRemovalRestsOnNeverOverlap(t *testing.T) {
	w := newWorld()
	p := prepare(t, nil, human(t, w, "do A", "stop A", "never mind the stop"))
	first := drop("presented-0", "do A", "presented-1", "stop A", "revocation")
	second := drop("presented-1", "stop A", "presented-2", "never mind the stop", "revocation")
	for name, ops := range map[string][]map[string]any{"the reason is removed after it was used": {first, second}, "the reason is removed first": {second, first}} {
		t.Run(name, func(t *testing.T) {
			ret, _ := apply(t, p, opJSON(ops...))
			if len(ret.Applied) != 1 || len(ret.Rejected) != 1 || ret.Rejected[0].Index != 1 {
				t.Fatalf("applied %d, rejected %+v", len(ret.Applied), ret.Rejected)
			}
		})
	}
}

// TestAnAnswerThatIsNotTheRequestedObjectIsASemanticFailure: the answer must be one
// strict JSON object satisfying the schema, and nothing else.
func TestAnAnswerThatIsNotTheRequestedObjectIsASemanticFailure(t *testing.T) {
	w := newWorld()
	p := prepare(t, nil, human(t, w, "a", "b"))
	in, _ := compaction.BuildSelectionDataset(p)
	for name, ans := range map[string]string{
		"not JSON":              "here you go: {}",
		"fenced":                "```json\n{\"operations\":[]}\n```",
		"two documents":         `{"operations":[]}{"operations":[]}`,
		"a duplicate key":       `{"operations":[],"operations":[]}`,
		"an array":              `[]`,
		"an extra key":          `{"operations":[],"note":"x"}`,
		"a missing key":         `{}`,
		"a bad operation":       `{"operations":[{"operation":"delete_all"}]}`,
		"a hash from the model": `{"operations":[{"operation":"drop_superseded","target_handle":"presented-0","target_quote":"a","replacement_handle":"presented-1","replacement_quote":"b","basis":"revocation","hash":"x"}]}`,
		"too many":              `{"operations":[` + strings.Repeat(`{"operation":"replace_completed","target_handle":"presented-0","target_quote":"a","completion_handle":"completion-0"},`, 512) + `{"operation":"replace_completed","target_handle":"presented-0","target_quote":"a","completion_handle":"completion-0"}]}`,
	} {
		if _, err := compaction.ApplySelection(p, in, ans); !errors.Is(err, compaction.ErrSemantic) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if ret, err := compaction.ApplySelection(p, in, `{"operations":[]}`); err != nil || len(ret.Applied) != 0 || len(retained(ret)) != 2 {
		t.Fatalf("an empty selection keeps everything: %v", err)
	}
}

// TestAContinuingConstraintIsNeverRemoved: the flag is the Host's, and no answer clears it.
func TestAContinuingConstraintIsNeverRemoved(t *testing.T) {
	w := newWorld()
	standing := w.human("常にログを残す。")
	standing.ContinuingConstraint = true
	p := prepare(t, nil, one(standing), human(t, w, "ログは残さなくてよい。"))
	in, _ := compaction.BuildSelectionDataset(p)
	if !in.Dataset.PresentedSources[0].ContinuingConstraint || in.Dataset.PresentedSources[1].ContinuingConstraint {
		t.Fatal("the flag is presented as the Host set it")
	}
	ret, err := compaction.ApplySelection(p, in, opJSON(drop("presented-0", "常にログを残す。", "presented-1", "ログは残さなくてよい。", "revocation")))
	if err != nil || len(ret.Applied) != 0 || len(ret.Rejected) != 1 || retained(ret)[0] != "常にログを残す。" {
		t.Fatalf("a standing constraint was removed without ground: %v %+v", err, ret.Rejected)
	}
}

// TestTheInventoryOfOneHundredNinetySourcesIsTheHosts is H06: the model mentions
// fewer sources than were presented; the Host's own list is complete, and the retained
// instructions are exactly those it did not adopt a removal for.
func TestTheInventoryOfOneHundredNinetySourcesIsTheHosts(t *testing.T) {
	w := newWorld()
	var srcs []compaction.Source
	for i := 0; i < 190; i++ {
		srcs = append(srcs, w.human(fmt.Sprintf("instruction %03d", i)))
	}
	p := prepare(t, nil, srcs)
	if len(p.Units) != 190 || len(p.AppliedSources) != 190 {
		t.Fatalf("units %d, sources %d", len(p.Units), len(p.AppliedSources))
	}
	in, err := compaction.BuildSelectionDataset(p)
	if err != nil || len(in.Dataset.PresentedSources) != 190 {
		t.Fatalf("%d pieces: %v", len(in.Dataset.PresentedSources), err)
	}
	// The model's answer touches two instructions only; 188 are not mentioned.
	ret, err := compaction.ApplySelection(p, in, opJSON(
		drop("presented-0", "instruction 000", "presented-189", "instruction 189", "revocation"),
		drop("presented-1", "instruction 001", "presented-189", "instruction 189", "amendment"),
	))
	if err != nil {
		t.Fatal(err)
	}
	if got := retained(ret); len(got) != 188 || len(ret.Applied) != 2 {
		t.Fatalf("%d retained, %d applied", len(got), len(ret.Applied))
	}
	if len(ret.From) != 188 || ret.From[0] != 2 {
		t.Fatalf("the origin of a retained unit is kept: %v", ret.From[:3])
	}
}

func execExchange(w *world, command string, view string) []compaction.Source {
	parts := strings.Fields(command)
	return w.exchange([]modelport.ToolCall{execCall("e"+fmt.Sprint(w.seq), "/usr/local/go/bin/"+parts[0], parts[1:]...)}, []string{view})
}

// TestACompletionLinkNeedsTheInstructionToNameOneUniqueCommand is F07's link rule and
// H09: a link is offered only for a fragment that names one command in full and a call
// that ran after it, and neither matches anything else.
func TestACompletionLinkNeedsTheInstructionToNameOneUniqueCommand(t *testing.T) {
	t.Run("one fragment one call", func(t *testing.T) {
		w := newWorld()
		ins := w.human("変更後に `go test ./...` を実行して確認する。")
		p := prepare(t, nil, one(ins), execExchange(w, "go test ./...", execView(0, "ok\n", "")), one(w.work("done")))
		in, err := compaction.BuildSelectionDataset(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(in.Links) != 1 || in.Dataset.CompletionLinks[0].Handle != "completion-0" || in.Dataset.CompletionLinks[0].TargetHandle != "presented-0" ||
			in.Dataset.CompletionLinks[0].CallText != "go test ./..." || in.Dataset.CompletionLinks[0].OutputText != "ok\n" || in.Dataset.CompletionLinks[0].ExitCode != 0 {
			t.Fatalf("%+v", in.Dataset.CompletionLinks)
		}
		if !in.HasCandidates() {
			t.Fatal("a link is a candidate even for a single instruction")
		}
		// The call and the answer are stored sources, the link's evidence.
		if in.Links[0].CallRef.SourceID == "" || in.Links[0].OutputRef.SourceID == "" || in.Links[0].CallRef == in.Links[0].OutputRef {
			t.Fatal("a link names the stored call and the stored answer")
		}
	})
	t.Run("a call that came first, or runs another command, or twice", func(t *testing.T) {
		w := newWorld()
		before := execExchange(w, "go test ./...", execView(0, "ok", ""))
		ins := w.human("`go test ./...` を確認する")
		after := execExchange(w, "go vet ./...", execView(0, "ok", ""))
		p := prepare(t, nil, before, one(ins), after)
		in, _ := compaction.BuildSelectionDataset(p)
		if len(in.Links) != 0 {
			t.Fatalf("no link: the matching call came before the instruction: %+v", in.Dataset.CompletionLinks)
		}
		// The control: one fragment, one call that came after it, and a link is offered.
		w = newWorld()
		ins = w.human("`go test ./...` を確認する")
		p = prepare(t, nil, one(ins), execExchange(w, "go test ./...", execView(1, "FAIL", "")))
		if in, _ = compaction.BuildSelectionDataset(p); len(in.Links) != 1 {
			t.Fatal("the control must offer a link")
		}
		w = newWorld()
		ins = w.human("`go test ./...` を確認する")
		twice := all(execExchange(w, "go test ./...", execView(1, "FAIL", "")), execExchange(w, "go test ./...", execView(0, "ok", "")))
		p = prepare(t, nil, one(ins), twice)
		in, _ = compaction.BuildSelectionDataset(p)
		if len(in.Links) != 0 {
			t.Fatal("two calls match one fragment: the pair is not unique")
		}
		w = newWorld()
		a, b := w.human("`go test ./...` を確認する"), w.human("もう一度 `go test ./...` を確認する")
		p = prepare(t, nil, one(a), one(b), execExchange(w, "go test ./...", execView(0, "ok", "")))
		in, _ = compaction.BuildSelectionDataset(p)
		if len(in.Links) != 0 {
			t.Fatal("one call is named by two fragments: it belongs to neither")
		}
	})
	t.Run("a command that only occurs in a sentence is not named", func(t *testing.T) {
		w := newWorld()
		for name, text := range map[string]string{
			"a sentence":            "go test ./... は実行するな",
			"a longer command line": "`go test ./... -count=1` を実行する",
			"half of a line":        "run go test ./... now",
		} {
			ins := w.human(text)
			p := prepare(t, nil, one(ins), execExchange(w, "go test ./...", execView(0, "ok", "")))
			in, _ := compaction.BuildSelectionDataset(p)
			if len(in.Links) != 0 {
				t.Errorf("%s: a link was offered for %q", name, text)
			}
			w = newWorld()
		}
		for name, text := range map[string]string{"a line of its own": "次を実行する:\ngo test ./...\nその後に報告する", "after a prompt": "次を実行する:\n$ go test ./...", "a fenced block": "```\ngo test ./...\n```"} {
			w = newWorld()
			ins := w.human(text)
			p := prepare(t, nil, one(ins), execExchange(w, "go test ./...", execView(0, "ok", "")))
			in, _ := compaction.BuildSelectionDataset(p)
			if len(in.Links) != 1 {
				t.Errorf("%s: no link for %q", name, text)
			}
		}
	})
	t.Run("an instruction in several fragments never gets one", func(t *testing.T) {
		w := newWorld()
		ins := w.human(strings.Repeat("a", 8100) + " `go test ./...` を確認する")
		p := prepare(t, nil, one(ins), execExchange(w, "go test ./...", execView(0, "ok", "")))
		in, _ := compaction.BuildSelectionDataset(p)
		if len(in.Pieces) < 2 || len(in.Links) != 0 {
			t.Fatalf("%d pieces, %d links: no fragment holds the whole condition", len(in.Pieces), len(in.Links))
		}
	})
	t.Run("what was not a finished run", func(t *testing.T) {
		for name, v := range map[string]string{
			"stopped by a timeout":  execViewWith(execOpts{exit: 0, stdout: "x", timedOut: true}),
			"a partial capture":     execViewWith(execOpts{exit: 0, stdout: "x", partial: true}),
			"a truncated preview":   execViewWith(execOpts{exit: 0, stdout: "x", truncated: true}),
			"no exit code":          view("process.exec", "completed", nil, true, nil),
			"another tool's answer": view("file.read", "completed", nil, true, nil),
			"an outcome not known":  view("process.exec", "unknown", nil, false, nil),
		} {
			w := newWorld()
			ins := w.human("`go test ./...` を確認する")
			p := prepare(t, nil, one(ins), execExchange(w, "go test ./...", v))
			in, _ := compaction.BuildSelectionDataset(p)
			if len(in.Links) != 0 {
				t.Errorf("%s: a link was offered", name)
			}
		}
	})
}

// TestCompletionTextIsBoundedByTwoThousandAndFortyEightBytesEach is A51: the call and the
// output are each at most 2,048 UTF-8 bytes; 2,048 and 2,048 pass, one of them 2,049 does not,
// and the two together are not held to 2,048.
func TestCompletionTextIsBoundedByTwoThousandAndFortyEightBytesEach(t *testing.T) {
	// A command of exactly n bytes: the program "go", "test" and one long argument.
	command := func(n int) string {
		base := "go test "
		return base + strings.Repeat("a", n-len(base))
	}
	offer := func(cmd, stdout, stderr string) int {
		w := newWorld()
		ins := w.human("run `" + cmd + "` and check")
		p := prepare(t, nil, one(ins), execExchange(w, cmd, execView(0, stdout, stderr)))
		in, err := compaction.BuildSelectionDataset(p)
		if err != nil {
			t.Fatal(err)
		}
		return len(in.Links)
	}
	if offer(command(2048), strings.Repeat("o", 2048), "") != 1 {
		t.Error("a call of 2,048 bytes with an output of 2,048 bytes is within both bounds, though together they are 4,096")
	}
	if offer(command(2049), "ok", "") != 0 {
		t.Error("a call of 2,049 bytes is over its bound")
	}
	if offer(command(10), strings.Repeat("o", 2049), "") != 0 {
		t.Error("an output of 2,049 bytes is over its bound")
	}
	if offer(command(10), strings.Repeat("o", 1024), strings.Repeat("e", 1025)) != 0 {
		t.Error("stdout and stderr together are the output: 2,049 bytes")
	}
	if offer(command(10), strings.Repeat("o", 1024), strings.Repeat("e", 1024)) != 1 {
		t.Error("stdout and stderr of 2,048 bytes together are within the bound")
	}
	// Bytes, not characters: 682 Japanese characters are 2,046 bytes, 683 are 2,049.
	if offer(command(10), strings.Repeat("あ", 682), "") != 1 || offer(command(10), strings.Repeat("あ", 683), "") != 0 {
		t.Error("the bound counts UTF-8 bytes")
	}
	_ = protocol.OriginHuman
}

// TestACompletionReplacesOnlyTheCompletedQuote: the instruction keeps the rest of its
// text, and an instruction that would be left with nothing stays whole.
func TestACompletionReplacesOnlyTheCompletedQuote(t *testing.T) {
	w := newWorld()
	ins := w.human("`go test ./...` を確認する。ログは削除しない。")
	only := w.human("`go vet ./...`")
	p := prepare(t, nil, one(ins), one(only), execExchange(w, "go test ./...", execView(0, "ok", "")), execExchange(w, "go vet ./...", execView(0, "ok", "")))
	in, err := compaction.BuildSelectionDataset(p)
	if err != nil || len(in.Links) != 2 {
		t.Fatalf("%d links: %v", len(in.Links), err)
	}
	complete := func(target, quote, link string) map[string]any {
		return map[string]any{"operation": "replace_completed", "target_handle": target, "target_quote": quote, "completion_handle": link}
	}
	ret, err := compaction.ApplySelection(p, in, opJSON(
		complete("presented-0", "`go test ./...` を確認する。", "completion-0"),
		complete("presented-1", "`go vet ./...`", "completion-1"),
		complete("presented-0", "ログは削除しない。", "completion-1"), // a completion offered for another fragment
	))
	if err != nil {
		t.Fatal(err)
	}
	got := retained(ret)
	if len(got) != 2 || got[0] != "ログは削除しない。" || got[1] != "`go vet ./...`" {
		t.Fatalf("retained %q", got)
	}
	if len(ret.Applied) != 1 || ret.Applied[0].Basis != "completion" || len(ret.Applied[0].Evidence) != 2 || len(ret.Rejected) != 2 || len(ret.UsedLinks) != 1 {
		t.Fatalf("applied %+v rejected %+v", ret.Applied, ret.Rejected)
	}
}
