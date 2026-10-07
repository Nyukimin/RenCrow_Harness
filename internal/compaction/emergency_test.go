package compaction_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// TestEmergencyMatchesTheDesignGolden is F12 held to the design's vector: the projection
// of the 11,712-byte log becomes the projection whose answer is the 604-byte marker, byte
// for byte, with the one change the design lists.
func TestEmergencyMatchesTheDesignGolden(t *testing.T) {
	in := decodeWire[contextplan.Projection](t, "projection_input.json")
	want := decodeWire[contextplan.Projection](t, "emergency_projection.json")
	changes := decodeWire[[]struct {
		EntryIndex    int `json:"entry_index"`
		MessageOffset int `json:"message_offset"`
		BeforeBytes   int `json:"before_bytes"`
		AfterBytes    int `json:"after_bytes"`
	}](t, "emergency_changes.json")
	p := preparedFromProjection(t, in, 2, 5)
	res, err := compaction.Emergency(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Replaced) != len(changes) || len(res.Replaced) != 1 {
		t.Fatalf("%d replacements", len(res.Replaced))
	}
	r, c := res.Replaced[0], changes[0]
	if r.Unit != c.EntryIndex || r.BeforeBytes != c.BeforeBytes || r.AfterBytes != c.AfterBytes || res.Units[r.Unit].Entry.Observations[r.Slot].MessageOffset != int64(c.MessageOffset) {
		t.Fatalf("replacement %+v, design %+v", r, c)
	}
	got := make([]contextplan.ProjectionEntry, len(res.Units))
	for i, u := range res.Units {
		got[i] = u.Entry
	}
	if cj1Of(t, got) != cj1Of(t, want.Entries) {
		t.Fatalf("the entries are not the design's\n got %.400s\nwant %.400s", cj1Of(t, got), cj1Of(t, want.Entries))
	}
	// The original is untouched: the reduction works on copies.
	if len(in.Entries[1].Messages[1].Text()) != 11712 || p.Units[1].Entry.Observations[0].Reference.Partial {
		t.Fatal("the live context was changed in place")
	}
}

// obsUnit builds a Tool exchange unit with one answer of the given text, as stored.
func obsUnits(t testing.TB, w *world, answers ...string) []compaction.Unit {
	t.Helper()
	var srcs []compaction.Source
	for i, a := range answers {
		srcs = append(srcs, w.exchange([]modelport.ToolCall{call("c"+string(rune('a'+i)), "file.read", `{"path":"x"}`)}, []string{a})...)
	}
	units, err := compaction.BuildUnits(srcs)
	if err != nil {
		t.Fatal(err)
	}
	return units
}

func bigAnswer(n int) string {
	return view("file.read", "completed", nil, true, map[string]string{"text": strings.Repeat("x", n)})
}

// TestEmergencyReplacesEveryEligibleAnswerThatShrinksInOneOrder: all of them, never stopping
// on a guess that enough was saved; an answer the marker is not shorter than is kept; the
// order is the design's.
func TestEmergencyReplacesEveryEligibleAnswerThatShrinksInOneOrder(t *testing.T) {
	w := newWorld()
	small := view("file.read", "completed", nil, true, nil) // shorter than any marker
	units := obsUnits(t, w, bigAnswer(3000), small, bigAnswer(5000), bigAnswer(4000))
	p := &compaction.Prepared{Snapshot: snapshot(nil, nil), Units: units, DurableBoundary: units[len(units)-1].Seq()}
	res, err := compaction.Emergency(p)
	if err != nil {
		t.Fatal(err)
	}
	var order []int
	for _, r := range res.Replaced {
		order = append(order, r.Unit)
		if r.AfterBytes >= r.BeforeBytes {
			t.Errorf("a replacement that is not shorter: %+v", r)
		}
	}
	if len(order) != 3 || order[0] != 0 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("replaced %v: the small answer stays and the rest go in application order", order)
	}
	for i, u := range res.Units {
		text := u.Entry.Messages[1].Text()
		wantMarker := i != 1
		if compaction.IsMarker(text) != wantMarker {
			t.Errorf("unit %d: marker %v, want %v", i, compaction.IsMarker(text), wantMarker)
		}
		if u.Entry.Messages[1].Role != "tool" || u.Entry.Messages[1].ToolCallID != units[i].Entry.Messages[1].ToolCallID || len(u.Entry.Messages[0].ToolCalls) != 1 {
			t.Errorf("unit %d: role, call ID or call changed", i)
		}
		if wantMarker && (len(u.Entry.Observations[0].Reference.PresentedRanges) != 0 || !u.Entry.Observations[0].Reference.Partial) {
			t.Errorf("unit %d: the reference of a replaced answer presents nothing and is partial", i)
		}
	}
	// A marker is exactly the one the reference serializes to, whole.
	want, err := contextplan.ObservationMarker(compaction.RefOnly(units[0].Entry.Observations[0].Reference))
	if err != nil || res.Units[0].Entry.Messages[1].Text() != want {
		t.Fatal("the replacement is the marker of the reference-only form")
	}
}

// TestEmergencyKeepsWhatItMayNotTouch: protected entries, answers that did not run to an
// end, answers that are already markers and everything that is not an answer.
func TestEmergencyKeepsWhatItMayNotTouch(t *testing.T) {
	w := newWorld()
	units := obsUnits(t, w, bigAnswer(3000), view("file.read", "unknown", nil, false, map[string]string{"text": strings.Repeat("x", 3000)}), bigAnswer(3000))
	units[0].Entry.Protected = true // an active Tool, say
	already, err := compaction.Marker(units[2].Entry.Observations[0].Reference)
	if err != nil {
		t.Fatal(err)
	}
	units[2].Entry.Messages[1] = modelport.Tool(units[2].Entry.Messages[1].ToolCallID, already)
	hum := w.human("keep me")
	humUnits, _ := compaction.BuildUnits(one(hum))
	units = append(units, humUnits...)
	p := &compaction.Prepared{Snapshot: snapshot(nil, nil), Units: units, DurableBoundary: units[3].Seq()}
	res, err := compaction.Emergency(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Replaced) != 0 {
		t.Fatalf("nothing here may be replaced: %+v", res.Replaced)
	}
	if res.Units[3].Entry.Messages[0].Text() != "keep me" {
		t.Fatal("an instruction is never reduced")
	}
}

func TestEmergencyRefusesWhatContradictsTheStoredRecords(t *testing.T) {
	w := newWorld()
	t.Run("two answers at the same position", func(t *testing.T) {
		units := obsUnits(t, w, bigAnswer(3000), bigAnswer(3000))
		// The second unit claims the first's place, evidence and source range.
		units[1].Entry = units[0].Entry
		p := &compaction.Prepared{Snapshot: snapshot(nil, nil), Units: units}
		if _, err := compaction.Emergency(p); !errors.Is(err, compaction.ErrIntegrity) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("an answer that is not the original its reference names", func(t *testing.T) {
		units := obsUnits(t, w, bigAnswer(3000))
		msg := units[0].Entry.Messages[1]
		units[0].Entry.Messages[1] = modelport.Tool(msg.ToolCallID, strings.Replace(msg.Text(), "x", "y", 1))
		p := &compaction.Prepared{Snapshot: snapshot(nil, nil), Units: units}
		if _, err := compaction.Emergency(p); !errors.Is(err, compaction.ErrIntegrity) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a source that is not the answer's evidence", func(t *testing.T) {
		units := obsUnits(t, w, bigAnswer(3000))
		units[0].Entry.MessageSources[1][0].SourceID = "evd_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0abc"
		p := &compaction.Prepared{Snapshot: snapshot(nil, nil), Units: units}
		if _, err := compaction.Emergency(p); !errors.Is(err, compaction.ErrIntegrity) {
			t.Fatalf("%v", err)
		}
	})
}

func TestUnionRangesJoinsOverlapAndTouch(t *testing.T) {
	r := func(a, b int64) contextplan.Range { return contextplan.Range{Start: a, End: b} }
	got := compaction.UnionRanges([]contextplan.Range{r(10, 20), r(0, 5)}, []contextplan.Range{r(5, 8), r(18, 30), r(40, 40), r(50, 60)})
	want := []contextplan.Range{r(0, 8), r(10, 30), r(50, 60)}
	if cj1Of(t, got) != cj1Of(t, want) {
		t.Fatalf("%v", got)
	}
	if got := compaction.UnionRanges(nil, nil); got == nil || len(got) != 0 {
		t.Fatal("the union of nothing is an empty list, not null")
	}
}

func TestMarkerOfAReferenceOnlyFormKeepsWhatWasSeenAndCovered(t *testing.T) {
	w := newWorld()
	units := obsUnits(t, w, bigAnswer(3000))
	ref := units[0].Entry.Observations[0].Reference
	ref.SeenRanges = []contextplan.Range{{Start: 0, End: 100}}
	ref.SummaryCoveredRange = []contextplan.Range{{Start: 0, End: 50}}
	ro := compaction.RefOnly(ref)
	if len(ro.PresentedRanges) != 0 || !ro.Partial || len(ro.SeenRanges) != 1 || len(ro.SummaryCoveredRange) != 1 {
		t.Fatalf("%+v", ro)
	}
	if len(ref.PresentedRanges) != 1 {
		t.Fatal("RefOnly must not change its argument")
	}
	text, err := compaction.Marker(ref)
	if err != nil || !strings.HasPrefix(text, "RENCROW_OBSERVATION_REFERENCE_V1\n{") || strings.HasSuffix(text, "\n") {
		t.Fatal("a marker is the prefix and the CJ1, with no trailing newline")
	}
	_ = protocol.OriginHuman
}
