package contextplan_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func wire(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(harnesstest.ModuleRoot(t), "testdata", "contract", "examples", "wire", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decode[T any](t testing.TB, raw []byte) T {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var v T
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func cj1(t testing.TB, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	out, err := protocol.EncodeCanonicalContract(raw)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func systemPrompt(t testing.TB) string {
	t.Helper()
	s, err := contextplan.ActSystemPrompt()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestActSystemPromptIsTheWholeFileAndTheGoldensOpening(t *testing.T) {
	golden := decode[[]modelport.ChatMessage](t, wire(t, "projection_golden.json"))
	if golden[0].Role != "system" || golden[0].Text() != systemPrompt(t) {
		t.Fatal("prompts/act_system.md is not the system message the golden opens with")
	}
	disk, err := os.ReadFile(filepath.Join(harnesstest.ModuleRoot(t), "prompts", "act_system.md"))
	if err != nil || string(disk) != systemPrompt(t) {
		t.Fatalf("the embedded prompt differs from the file (%v)", err)
	}
}

// TestProjectionGoldens renders the design's projections: the golden is the exact
// message list, and nothing is trimmed, reordered or added.
func TestProjectionGoldens(t *testing.T) {
	for _, tc := range []struct{ in, golden string }{
		{"projection_input.json", "projection_golden.json"},
		{"emergency_projection.json", "emergency_golden.json"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			proj := decode[contextplan.Projection](t, wire(t, tc.in))
			plan, err := contextplan.AssembleContext(contextplan.Input{SystemPrompt: systemPrompt(t), Projection: &proj})
			if err != nil {
				t.Fatal(err)
			}
			want, err := protocol.EncodeCanonicalContract(wire(t, tc.golden))
			if err != nil {
				t.Fatal(err)
			}
			if got := cj1(t, plan.Messages); got != string(want) {
				t.Fatalf("the rendered prompt is not the golden\n got %.600s\nwant %.600s", got, want)
			}
			if len(plan.Messages) != 8 {
				t.Fatalf("%d messages", len(plan.Messages))
			}
		})
	}
}

func TestObservationMarkerMatchesTheGolden(t *testing.T) {
	proj := decode[contextplan.Projection](t, wire(t, "projection_input.json"))
	ref := proj.Entries[1].Observations[0].Reference
	// A reference-only marker: nothing presented, everything seen so far kept.
	ref.PresentedRanges = []contextplan.Range{}
	ref.Partial = true
	got, err := contextplan.ObservationMarker(ref)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSuffix(string(wire(t, "observation_marker.txt")), "\n"); got != want {
		t.Fatalf("marker\n got %s\nwant %s", got, want)
	}
	if strings.HasSuffix(got, "\n") {
		t.Fatal("a marker has no trailing newline")
	}
}

func TestImportantObservationsAreEmittedAsUserDataBetweenSummaryAndTail(t *testing.T) {
	proj := decode[contextplan.Projection](t, wire(t, "projection_input.json"))
	ref := proj.Entries[1].Observations[0].Reference
	ref.PresentedRanges, ref.Partial = []contextplan.Range{}, true
	proj.ImportantObservations = []contextplan.ObservationReference{ref}
	plan, err := contextplan.AssembleContext(contextplan.Input{SystemPrompt: systemPrompt(t), Projection: &proj})
	if err != nil {
		t.Fatal(err)
	}
	// system, developer, retained instruction, boundary, summary, marker, tool exchange (2), tail.
	if len(plan.Messages) != 9 {
		t.Fatalf("%d messages", len(plan.Messages))
	}
	m := plan.Messages[5]
	if m.Role != "user" || !strings.HasPrefix(m.Text(), "RENCROW_OBSERVATION_REFERENCE_V1\n{") {
		t.Fatalf("%+v", m)
	}
	if plan.Messages[6].Role != "assistant" || len(plan.Messages[6].ToolCalls) != 1 {
		t.Fatal("the tool exchange must follow the marker unbroken")
	}
}

func block(kind, text string) protocol.ContextBlock {
	rev, err := protocol.ContextRevision(kind, text, nil)
	if err != nil {
		panic(err)
	}
	return protocol.ContextBlock{Kind: kind, Text: text, Revision: rev}
}

// TestFiveClassesKeepTheirOrderRolesAndText is A03/A59: Character is system, Stable
// is developer, Recall and Variable are data envelopes, several blocks of a kind
// stay separate messages in the order given, an empty text is kept, and the
// human's exact text sits where it was applied.
func TestFiveClassesKeepTheirOrderRolesAndText(t *testing.T) {
	src := &protocol.SourceRef{Owner: "RenCrow_CORE", SourceID: "m-1", RawHash: strings.Repeat("a", 64), ProjectionVersion: "text/v1",
		Range: protocol.ByteRange{Start: 0, End: 3}, Origin: "unknown", Sequence: 1}
	recall := block(protocol.KindRecallPack, "  recalled\n")
	recall.Source = src
	blocks := []protocol.ContextBlock{
		block(protocol.KindVariableRuntimeContext, "now=12:00"),
		recall,
		block(protocol.KindStableRuntimeContext, "stable A"),
		block(protocol.KindCharacterSystemPrompt, ""),
		block(protocol.KindStableRuntimeContext, ""),
		block(protocol.KindCharacterSystemPrompt, " persona\n"),
		block(protocol.KindRecallPack, "second recall"),
	}
	plan, err := contextplan.AssembleContext(contextplan.Input{
		SystemPrompt: "SYS", Blocks: blocks,
		History: []contextplan.HistoryItem{
			{ContextSeq: 3, HistoryKind: contextplan.HistoryHuman, Origin: "human", Text: "  Hello\r\nworld \n"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range plan.Messages {
		got = append(got, m.Role+":"+m.Text())
	}
	ctxData := func(b protocol.ContextBlock) string { return "user:RENCROW_CONTEXT_DATA_V1\n" + cj1(t, b) }
	want := []string{
		"system:SYS",
		"system:", "system: persona\n", // Character, in the order given, the empty one kept
		"developer:stable A", "developer:", // Stable
		ctxData(blocks[1]), ctxData(blocks[6]), // Recall
		ctxData(blocks[0]),         // Variable
		"user:  Hello\r\nworld \n", // the human's exact text
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	// The block's claimed source is data inside the envelope, not an authority.
	if !strings.Contains(got[5], `"source":{"origin":"unknown"`) {
		t.Fatalf("the recall block's source was lost: %s", got[5])
	}
	if len(plan.Manifest) != 1+len(blocks) || plan.Manifest[1].Kind != protocol.KindCharacterSystemPrompt {
		t.Fatalf("manifest %+v", plan.Manifest)
	}
}

func TestAutomationAndUnknownInputAreEnvelopedAndWorkIsAssistant(t *testing.T) {
	plan, err := contextplan.AssembleContext(contextplan.Input{
		SystemPrompt: "SYS",
		History: []contextplan.HistoryItem{
			{ContextSeq: 1, HistoryKind: contextplan.HistoryAutomation, Origin: "automation", Text: "delegated <task> & \"more\""},
			{ContextSeq: 2, HistoryKind: contextplan.HistoryWork, Origin: "agent", Text: "done"},
			{ContextSeq: 5, HistoryKind: contextplan.HistoryProtected, Origin: "unknown", Text: "who knows"},
			{ContextSeq: 6, HistoryKind: contextplan.HistoryHuman, Origin: "human", Text: "latest"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := plan.Messages
	if m[1].Role != "user" || m[1].Text() != "RENCROW_INPUT_DATA_V1\n"+`{"origin":"automation","text":"delegated <task> & \"more\""}` {
		t.Fatalf("%q", m[1].Text())
	}
	if m[2].Role != "assistant" || m[2].Text() != "done" || m[2].ToolCalls == nil || len(m[2].ToolCalls) != 0 {
		t.Fatalf("%+v", m[2])
	}
	if m[3].Text() != "RENCROW_INPUT_DATA_V1\n"+`{"origin":"unknown","text":"who knows"}` {
		t.Fatalf("%q", m[3].Text())
	}
	if m[4].Text() != "latest" || len(m) != 5 {
		t.Fatalf("the latest human message must stay at its place: %+v", m)
	}
}

func TestInvalidContextsAreRefused(t *testing.T) {
	sys := "SYS"
	hist := func(seq int64, kind, text string) contextplan.HistoryItem {
		return contextplan.HistoryItem{ContextSeq: seq, HistoryKind: kind, Origin: "human", Text: text}
	}
	proj := decode[contextplan.Projection](t, wire(t, "projection_input.json"))
	for name, in := range map[string]contextplan.Input{
		"no system prompt":             {},
		"user_message as a block":      {SystemPrompt: sys, Blocks: []protocol.ContextBlock{block(protocol.KindUserMessage, "x")}},
		"unknown block kind":           {SystemPrompt: sys, Blocks: []protocol.ContextBlock{{Kind: "other", Text: "x"}}},
		"invalid UTF-8":                {SystemPrompt: sys, Blocks: []protocol.ContextBlock{{Kind: protocol.KindRecallPack, Text: "\xff"}}},
		"history out of order":         {SystemPrompt: sys, History: []contextplan.HistoryItem{hist(2, contextplan.HistoryHuman, "a"), hist(2, contextplan.HistoryHuman, "b")}},
		"host context as history":      {SystemPrompt: sys, History: []contextplan.HistoryItem{hist(1, "HostContext", "a")}},
		"projection and blocks":        {SystemPrompt: sys, Projection: &proj, Blocks: []protocol.ContextBlock{block(protocol.KindRecallPack, "x")}},
		"envelope with a human origin": {SystemPrompt: sys, History: []contextplan.HistoryItem{{ContextSeq: 1, HistoryKind: contextplan.HistoryAutomation, Origin: "human", Text: "x"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := contextplan.AssembleContext(in); !errors.Is(err, contextplan.ErrInvalidContext) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestProjectionFaultsAreRefused(t *testing.T) {
	for name, edit := range map[string]func(p *contextplan.Projection){
		"unknown format":             func(p *contextplan.Projection) { p.FormatVersion = "other" },
		"summary without anchor":     func(p *contextplan.Projection) { p.SummaryAnchorSequence = nil },
		"anchor without summary":     func(p *contextplan.Projection) { p.Summary = nil },
		"entries out of order":       func(p *contextplan.Projection) { p.Entries[0], p.Entries[2] = p.Entries[2], p.Entries[0] },
		"handle not in source map":   func(p *contextplan.Projection) { p.Summary.SourceMap = nil },
		"sources not per message":    func(p *contextplan.Projection) { p.Entries[1].MessageSources = p.Entries[1].MessageSources[:1] },
		"slot not at a tool message": func(p *contextplan.Projection) { p.Entries[1].Observations[0].MessageOffset = 0 },
		"orphan tool message": func(p *contextplan.Projection) {
			p.Entries[1].Messages = p.Entries[1].Messages[1:]
			p.Entries[1].MessageSources = p.Entries[1].MessageSources[1:]
			p.Entries[1].Observations = nil
		},
		"tool call without result": func(p *contextplan.Projection) {
			p.Entries[1].Messages = p.Entries[1].Messages[:1]
			p.Entries[1].MessageSources = p.Entries[1].MessageSources[:1]
			p.Entries[1].Observations = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			proj := decode[contextplan.Projection](t, wire(t, "projection_input.json"))
			if name == "anchor without summary" {
				proj.ImportantObservations = nil
			}
			edit(&proj)
			if _, err := contextplan.AssembleContext(contextplan.Input{SystemPrompt: "SYS", Projection: &proj}); !errors.Is(err, contextplan.ErrInvalidContext) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestAssembleDoesNotShareTheCallersSlices(t *testing.T) {
	blocks := []protocol.ContextBlock{block(protocol.KindRecallPack, "a")}
	plan, err := contextplan.AssembleContext(contextplan.Input{SystemPrompt: "SYS", Blocks: blocks})
	if err != nil {
		t.Fatal(err)
	}
	before := plan.Messages[1].Text()
	blocks[0].Text = "changed"
	if plan.Messages[1].Text() != before {
		t.Fatal("the plan shares the caller's data")
	}
	again, _ := contextplan.AssembleContext(contextplan.Input{SystemPrompt: "SYS", Blocks: blocks})
	if again.Messages[1].Text() == before {
		t.Fatal("the plan did not follow its inputs")
	}
}
