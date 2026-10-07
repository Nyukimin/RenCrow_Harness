package compaction_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

// TestBuildUnitsMakesOneUnitPerMessageAndOnePerToolExchange is F06: the live history becomes
// units in application order, and a Tool exchange stays whole, its coordinate that of its
// last answer.
func TestBuildUnitsMakesOneUnitPerMessageAndOnePerToolExchange(t *testing.T) {
	w := newWorld()
	h := w.human("do A")
	xs := w.exchange([]modelport.ToolCall{execCall("c1", "/usr/bin/go", "test", "./..."), call("c2", "file.read", `{"path":"a.txt"}`)},
		[]string{execView(0, "ok\n", ""), view("file.read", "completed", nil, true, nil)})
	wk := w.work("done")
	au := w.automation("autom")
	un := w.unknown("who knows")
	units, err := compaction.BuildUnits(all(one(h), xs, one(wk), one(au), one(un)))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, u := range units {
		kinds = append(kinds, u.Entry.Kind)
	}
	if got := strings.Join(kinds, ","); got != "instruction,tool_exchange,work,instruction,protected" {
		t.Fatalf("kinds %s", got)
	}
	ex := units[1]
	if ex.Seq() != xs[2].ContextSeq || len(ex.Entry.Messages) != 3 || len(ex.Entry.Observations) != 2 || ex.Entry.Observations[0].MessageOffset != 1 || ex.Entry.Observations[1].MessageOffset != 2 {
		t.Fatalf("the exchange: seq %d, %d messages, %d slots", ex.Seq(), len(ex.Entry.Messages), len(ex.Entry.Observations))
	}
	if !ex.Entry.Observations[0].Eligible || !ex.Entry.Observations[1].Eligible {
		t.Fatal("answers of calls that ran to an end are eligible")
	}
	for i, ref := range []contextplan.ObservationReference{ex.Entry.Observations[0].Reference, ex.Entry.Observations[1].Reference} {
		src := xs[1+i]
		if ref.EvidenceID != src.EvidenceID || ref.RawHash != src.RawHash || ref.ProjectionHash != src.RawHash || ref.TotalBytes != src.TotalBytes || ref.ProjectionVersion != "text/v1" ||
			ref.Partial || len(ref.PresentedRanges) != 1 || len(ref.SeenRanges) != 0 || len(ref.SummaryCoveredRange) != 0 || ref.Retrieval.Tool != "evidence.read" || ref.Retrieval.MaxBytes != 65536 {
			t.Errorf("reference %d: %+v", i, ref)
		}
	}
	if ex.Entry.Observations[0].Reference.Tool != "process.exec" || ex.Entry.Observations[1].Reference.Tool != "file.read" {
		t.Error("the tool of a reference is the tool of its call")
	}
	// An instruction is the exact text with the range it was stored in; an unknown input is
	// protected and enveloped, never presented as a human instruction.
	if units[0].Entry.Messages[0].Text() != "do A" || len(units[0].Segments) != 1 || units[0].Segments[0].Ref.Range.End != 4 {
		t.Fatal("a human instruction is its own exact text")
	}
	if !units[4].Entry.Protected || !strings.HasPrefix(units[4].Entry.Messages[0].Text(), "RENCROW_INPUT_DATA_V1\n") || !strings.Contains(units[4].Entry.Messages[0].Text(), `"origin":"unknown"`) {
		t.Fatal("an unknown input is protected data")
	}
}

func TestBuildUnitsRefusesWhatIsStoredInconsistently(t *testing.T) {
	w := newWorld()
	xs := w.exchange([]modelport.ToolCall{call("c1", "file.read", `{"path":"a"}`), call("c2", "file.read", `{"path":"b"}`)}, []string{view("file.read", "completed", nil, true, nil), view("file.read", "completed", nil, true, nil)})
	swap := func(s []compaction.Source) []compaction.Source {
		out := append([]compaction.Source(nil), s...)
		out[1].ToolCallID, out[2].ToolCallID = out[2].ToolCallID, out[1].ToolCallID
		return out
	}
	notText := w.human("x")
	notText.TextProjection = false
	for name, srcs := range map[string][]compaction.Source{
		"a call without an answer":        xs[:2],
		"answers in another order":        swap(xs),
		"an answer without its call":      xs[1:2],
		"not increasing":                  {xs[0], xs[0]},
		"a message that is not text":      {notText},
		"an unknown history kind":         {func() compaction.Source { s := w.human("y"); s.HistoryKind = "Something"; return s }()},
		"a record that is not Tool calls": {func() compaction.Source { s := xs[0]; s.Text = `{"x":1}`; return s }()},
	} {
		if _, err := compaction.BuildUnits(srcs); !errors.Is(err, compaction.ErrIntegrity) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestAnAnswerIsEligibleOnlyWhenTheCallRanToAnEnd: an answer that says the call did not
// run, or that its outcome is unknown, says so in its text; swapping it for a reference
// would take that away.
func TestAnAnswerIsEligibleOnlyWhenTheCallRanToAnEnd(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		eligible     bool
	}{
		{"completed", view("file.read", "completed", nil, true, nil), true},
		{"failed", execView(2, "", "boom"), true},
		{"not started", view("file.read", "not_started", nil, true, nil), false},
		{"unknown", view("file.read", "unknown", nil, false, nil), false},
		{"not a view", "just text", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld()
			xs := w.exchange([]modelport.ToolCall{call("c1", "file.read", `{}`)}, []string{tc.answer})
			units, err := compaction.BuildUnits(xs)
			if err != nil {
				t.Fatal(err)
			}
			if got := units[0].Entry.Observations[0].Eligible; got != tc.eligible {
				t.Fatalf("eligible %v, want %v", got, tc.eligible)
			}
		})
	}
}

// TestAnObservationIsSeenOnlyWhenAGenerationWasSentAfterIt: what was never sent to the
// model is not claimed as seen.
func TestAnObservationIsSeenOnlyWhenAGenerationWasSentAfterIt(t *testing.T) {
	w := newWorld()
	xs := w.exchange([]modelport.ToolCall{call("c1", "file.read", `{}`)}, []string{view("file.read", "completed", nil, true, nil)})
	xs[1].Presented = true
	units, err := compaction.BuildUnits(xs)
	if err != nil {
		t.Fatal(err)
	}
	ref := units[0].Entry.Observations[0].Reference
	if len(ref.SeenRanges) != 1 || ref.SeenRanges[0].End != xs[1].TotalBytes {
		t.Fatalf("seen %v", ref.SeenRanges)
	}
}

func TestPrepareCheckpointFixtureIsTheStateTheGoldenDescribes(t *testing.T) {
	prior := loadGoldenCheckpoint(t)
	w := newWorld()
	w.seq, w.item = 5, 5
	tail := one(w.human("next"))
	snap := snapshot(prior, tail)
	snap.ThreadID = prior.Candidate.ThreadID
	p, err := compaction.Prepare(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Units) != 4 || p.SemanticBoundary != 2 || p.DurableBoundary != 6 || p.Anchor == nil || *p.Anchor != 2 || p.Summary == nil {
		t.Fatalf("units %d, semantic %d, durable %d", len(p.Units), p.SemanticBoundary, p.DurableBoundary)
	}
	if !p.Units[0].Inherited || p.Units[3].Inherited || len(p.Units[0].Segments) != 1 || p.Units[0].Segments[0].Text != "ログは削除しない。\n" {
		t.Fatal("inherited units come first and an instruction's text is taken back from its ranges")
	}
	if len(p.Inventory) != 1 || p.Inventory[0].EvidenceID != "evd_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0004" {
		t.Fatalf("inventory %+v", p.Inventory)
	}
	// The work not yet summarized is what lies after the semantic boundary.
	if idx := p.WorkUnits(); len(idx) != 2 || p.Units[idx[0]].Entry.Kind != "tool_exchange" || p.Units[idx[1]].Entry.Kind != "work" {
		t.Fatalf("work units %v", idx)
	}
}

func TestPrepareRefusesContradictoryState(t *testing.T) {
	good := loadGoldenCheckpoint(t)
	mutate := func(f func(c *compaction.Candidate)) *compaction.Checkpoint {
		cp := loadGoldenCheckpoint(t) // a fresh copy: the candidate's lists are shared by a shallow copy
		f(&cp.Candidate)
		return cp
	}
	w := newWorld()
	cases := map[string]struct {
		prior *compaction.Checkpoint
		tail  []compaction.Source
	}{
		"another thread":                        {mutate(func(c *compaction.Candidate) { c.ThreadID = "thr_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b00ff" }), nil},
		"a summary without anchor":              {mutate(func(c *compaction.Candidate) { c.Projection.SummaryAnchorSequence = nil }), nil},
		"an anchor that is not the boundary":    {mutate(func(c *compaction.Candidate) { c.SemanticBoundary = 3 }), nil},
		"a boundary beyond durable":             {mutate(func(c *compaction.Candidate) { c.DurableBoundary = 1 }), nil},
		"a mode a compaction does not stand on": {mutate(func(c *compaction.Candidate) { c.Mode = "summary" }), nil},
		"an entry beyond durable":               {mutate(func(c *compaction.Candidate) { c.DurableBoundary = 4 }), nil},
		"a tail inside the boundary":            {good, func() []compaction.Source { s := w.human("old"); s.ContextSeq = 3; return one(s) }()},
		"an entry whose ranges are not its text": {mutate(func(c *compaction.Candidate) {
			c.Projection.Entries[0].MessageSources[0][0].Range.End = 3
		}), nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			snap := snapshot(tc.prior, tc.tail)
			snap.ThreadID = good.Candidate.ThreadID
			if _, err := compaction.Prepare(snap); !errors.Is(err, compaction.ErrIntegrity) {
				t.Fatalf("%v", err)
			}
		})
	}
}
