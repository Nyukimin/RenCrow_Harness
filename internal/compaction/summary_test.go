package compaction_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

// scene is a Thread with instructions, Work and an exec exchange: what a Summary is
// asked about.
type scene struct {
	w   *world
	p   *compaction.Prepared
	ret *compaction.Retention
	sel *compaction.SelectionInput
	in  *compaction.SummaryInput
}

func newScene(t testing.TB, extra ...[]compaction.Source) *scene {
	t.Helper()
	w := newWorld()
	srcs := all(human(t, w, "go test ./... で確認しながら保存処理を実装する。"), one(w.work("保存処理を実装中。")),
		execExchange(w, "go test ./...", execView(0, "ok  \tpkg\t0.1s\n", "")), one(w.work("試験は通った。次は再開試験。")))
	srcs = append(srcs, all(extra...)...)
	p := prepare(t, nil, srcs)
	sel, err := compaction.BuildSelectionDataset(p)
	if err != nil {
		t.Fatal(err)
	}
	ret := compaction.NoSelection(p)
	in, err := compaction.BuildSummaryInput(p, ret, sel, func(id string) (string, error) { return w.read(id) })
	if err != nil {
		t.Fatal(err)
	}
	return &scene{w: w, p: p, ret: ret, sel: sel, in: in}
}

type item map[string]any

func summaryAnswer(t testing.TB, cur, dec, ver, open, next []item, important ...string) string {
	t.Helper()
	nz := func(i []item) []item {
		if i == nil {
			return []item{}
		}
		return i
	}
	if important == nil {
		important = []string{}
	}
	b, err := json.Marshal(map[string]any{"current_work": nz(cur), "decisions": nz(dec), "verification": nz(ver), "open_items": nz(open), "next_steps": nz(next), "important_observation_handles": important})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func it(text string, handles ...string) item { return item{"text": text, "source_handles": handles} }
func verify(text, state string, handles ...string) item {
	return item{"text": text, "state": state, "source_handles": handles}
}

// TestSummaryDataHasFiveNamespacesAndNothingOfTheHosts is F09 and A22/A61: the handles of
// the five namespaces are 0-based without gaps and local to the request, the Work is
// what was not yet summarized, the Observation is shown as excerpts, and no hash, ID or
// range of the Host is in the data.
func TestSummaryDataHasFiveNamespacesAndNothingOfTheHosts(t *testing.T) {
	long := view("file.read", "completed", nil, true, map[string]string{"text": strings.Repeat("あ", 2000)})
	s := newScene(t, execExchange(newWorldAfter(t), "go vet ./...", execView(0, "", "")))
	_ = long
	d := s.in.Dataset
	if d.FormatVersion != "rencrow-summary-data/v1" || len(d.Instructions) != 1 || len(d.Observations) != 2 || len(d.PriorSummaries) != 0 || len(d.Completions) != 0 || len(d.ProtectedState) != 0 {
		t.Fatalf("%+v", d)
	}
	// Work: the model's final messages and the record of its Tool calls, in order.
	var handles []string
	for _, p := range d.Work {
		handles = append(handles, p.Handle)
		if p.Origin != "agent" || p.ChunkCount != 1 || p.ContinuingConstraint {
			t.Errorf("%+v", p)
		}
	}
	if strings.Join(handles, ",") != "work-0,work-1,work-2,work-3" || d.Work[0].Text != "保存処理を実装中。" || !strings.Contains(d.Work[1].Text, `"tool_calls"`) {
		t.Fatalf("work %v", handles)
	}
	if d.Instructions[0].Handle != "instruction-0" || d.Observations[0].Handle != "observation-0" || d.Observations[1].Handle != "observation-1" {
		t.Fatal("handles are 0-based in each namespace")
	}
	o := d.Observations[0]
	if o.Tool != "process.exec" || o.Partial || len(o.Excerpts) != 1 || o.Excerpts[0].Range.Start != 0 || o.TotalBytes != int64(o.Excerpts[0].Range.End) || !o.CaptureComplete {
		t.Fatalf("%+v", o)
	}
	raw, _ := json.Marshal(d)
	// A Tool's own answer names its output Evidence (so the model can read it back); the
	// Host's records of the data are none of the data.
	for _, forbidden := range []string{"ckp_", "msg_", "raw_hash", "projection_hash", "source_id", "source_ref", "digest", "sequence_ref"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("the data carries %q, which stays with the Host", forbidden)
		}
	}
	// The Host's manifest says exactly what each handle stands for.
	for _, h := range []string{"work-0", "work-3", "instruction-0", "observation-0", "observation-1"} {
		if len(s.in.Manifest[h]) == 0 {
			t.Errorf("no sources for %s", h)
		}
	}
	if _, ok := s.in.Manifest["work-4"]; ok {
		t.Fatal("a handle that was not presented has no sources")
	}
	if s.in.Manifest["work-0"][0].Range.End != uint64(len("保存処理を実装中。")) {
		t.Fatal("a Work handle is the exact range of the stored text")
	}
	if _, _, err := compaction.StageMessages(modelport.StageSummary, d); err != nil {
		t.Fatalf("the data satisfies the stage schema: %v", err)
	}
}

func newWorldAfter(t testing.TB) *world { w := newWorld(); w.seq, w.item = 100, 100; return w }

// TestObservationsAreExcerptsOfAtMostTwoKilobytes: the whole answer up to 2,048 bytes, else
// its head and tail of 1,024 each at character boundaries, and partial says which.
func TestObservationsAreExcerptsOfAtMostTwoKilobytes(t *testing.T) {
	w := newWorld()
	big := view("file.read", "completed", nil, true, map[string]string{"text": strings.Repeat("あ", 2000)})
	huge := view("file.read", "completed", nil, true, map[string]string{"text": strings.Repeat("x", 300000)})
	srcs := all(human(t, w, "read"), w.exchange([]modelport.ToolCall{call("c1", "file.read", `{}`), call("c2", "file.read", `{}`)}, []string{big, huge}), one(w.work("ok")))
	p := prepare(t, nil, srcs)
	in, err := compaction.BuildSummaryInput(p, compaction.NoSelection(p), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, o := range in.Dataset.Observations {
		if len(o.Excerpts) != 2 || !o.Partial {
			t.Fatalf("observation %d: %d excerpts, partial %v", i, len(o.Excerpts), o.Partial)
		}
		for _, e := range o.Excerpts {
			if len(e.Text) > 1024 || !utf8.ValidString(e.Text) || int64(e.Range.End-e.Range.Start) != int64(len(e.Text)) {
				t.Errorf("excerpt %d: %d bytes", i, len(e.Text))
			}
		}
		if o.Excerpts[0].Range.Start != 0 || o.Excerpts[1].Range.End != o.TotalBytes {
			t.Errorf("the excerpts are the two ends of the answer")
		}
	}
	// The manifest has one stored range per excerpt.
	if len(in.Manifest["observation-0"]) != 2 || in.Manifest["observation-0"][1].Range.End != uint64(in.Dataset.Observations[0].TotalBytes) {
		t.Fatal("an observation's handle is its excerpt ranges")
	}
}

// TestAnObservationAlreadyReplacedByAReferenceIsReadBackFromItsEvidence: the Summary data
// needs the text, and takes it from the stored original, checked against the reference.
func TestAnObservationAlreadyReplacedByAReferenceIsReadBackFromItsEvidence(t *testing.T) {
	w := newWorld()
	big := view("file.read", "completed", nil, true, map[string]string{"text": strings.Repeat("x", 5000)})
	srcs := all(human(t, w, "read"), w.exchange([]modelport.ToolCall{call("c1", "file.read", `{}`)}, []string{big}), one(w.work("ok")))
	p := prepare(t, nil, srcs)
	em, err := compaction.Emergency(p)
	if err != nil || len(em.Replaced) != 1 {
		t.Fatalf("%v %+v", err, em.Replaced)
	}
	p.Units = em.Units
	in, err := compaction.BuildSummaryInput(p, compaction.NoSelection(p), nil, func(id string) (string, error) { return w.read(id) })
	if err != nil {
		t.Fatal(err)
	}
	if o := in.Dataset.Observations[0]; len(o.Excerpts) != 2 || !strings.HasPrefix(o.Excerpts[0].Text, `{"action_id"`) {
		t.Fatalf("the excerpts come from the original text: %+v", o)
	}
	if _, err := compaction.BuildSummaryInput(p, compaction.NoSelection(p), nil, nil); !errors.Is(err, compaction.ErrIntegrity) {
		t.Fatalf("a reference with no way to read the original: %v", err)
	}
	other := func(string) (string, error) { return strings.Repeat("y", 5000), nil }
	if _, err := compaction.BuildSummaryInput(p, compaction.NoSelection(p), nil, other); !errors.Is(err, compaction.ErrIntegrity) {
		t.Fatalf("text that is not the original: %v", err)
	}
}

// TestRevokedTextNeverReachesTheSummaryData is H04: the text a selection removed is in no
// array of the Summary data.
func TestRevokedTextNeverReachesTheSummaryData(t *testing.T) {
	w := newWorld()
	p := prepare(t, nil, human(t, w, "方式Aを実装する。ログは削除しない。", "方式Aはやめる。"), one(w.work("方式Bを検討した。")))
	ret, sel := apply(t, p, opJSON(drop("presented-0", "方式Aを実装する。", "presented-1", "方式Aはやめる。", "revocation")))
	in, err := compaction.BuildSummaryInput(p, ret, sel, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(in.Dataset)
	if strings.Contains(string(raw), "方式Aを実装する") {
		t.Fatal("revoked text is in the Summary data")
	}
	if len(in.Dataset.Instructions) != 2 || in.Dataset.Instructions[0].Text != "ログは削除しない。" || in.Dataset.Instructions[1].Text != "方式Aはやめる。" {
		t.Fatalf("%+v", in.Dataset.Instructions)
	}
	// The reason for the removal is kept as an instruction, and the removal is of the exact
	// range: the instruction's piece is a range of the stored text.
	ref := in.Manifest["instruction-0"][0]
	if got := p.Units[0].Segments[0].Text[ref.Range.Start:ref.Range.End]; got != "ログは削除しない。" {
		t.Fatalf("%q", got)
	}
}

// TestThePreviousSummaryIsGivenOnceWithoutItsHandlesAndProtectedStateIsShown is F09's
// remaining namespaces.
func TestThePreviousSummaryIsGivenOnceWithoutItsHandlesAndProtectedStateIsShown(t *testing.T) {
	prior := loadGoldenCheckpoint(t)
	w := newWorld()
	w.seq, w.item = 5, 5
	p := prepare(t, prior, one(w.unknown("誰かのメモ")), one(w.work("さらに進めた。")))
	in, err := compaction.BuildSummaryInput(p, compaction.NoSelection(p), nil, func(id string) (string, error) {
		return "", errors.New("must not be read: the answer is a live reference")
	})
	if err == nil {
		t.Log("the golden's observation is a marker and needs its original")
	}
	_ = in
	// Give it a reader: the original is the 11,712-byte log of the design.
	log := decodeWire[contextplan.Projection](t, "projection_input.json").Entries[1].Messages[1].Text()
	in, err = compaction.BuildSummaryInput(p, compaction.NoSelection(p), nil, func(id string) (string, error) { return log, nil })
	if err != nil {
		t.Fatal(err)
	}
	d := in.Dataset
	if len(d.PriorSummaries) != 1 || d.PriorSummaries[0].Handle != "summary-0" || len(d.PriorSummaries[0].Summary.CurrentWork) != 1 {
		t.Fatalf("%+v", d.PriorSummaries)
	}
	raw, _ := json.Marshal(d.PriorSummaries)
	if strings.Contains(string(raw), "source_handles") || strings.Contains(string(raw), "important_observation") {
		t.Fatal("the previous Summary is given as text only, without its handles")
	}
	if len(d.ProtectedState) != 1 || d.ProtectedState[0].Kind != "unknown" || d.ProtectedState[0].Text == nil || *d.ProtectedState[0].Text != "誰かのメモ" || d.ProtectedState[0].Partial {
		t.Fatalf("%+v", d.ProtectedState)
	}
	if len(in.Manifest["summary-0"]) != 1 || in.Manifest["summary-0"][0].SourceID != prior.Candidate.Projection.Summary.SourceCheckpointID {
		t.Fatal("summary-0 stands for the checkpoint that accepted it")
	}
	// Work: the golden's own Work (tool exchange at 4, work at 5) is after the semantic boundary
	// 2, and the new Work after the checkpoint; the instruction is not Work.
	if len(d.Work) != 3 {
		t.Fatalf("%d work pieces", len(d.Work))
	}
	if len(d.Instructions) != 1 || d.Instructions[0].Text != "ログは削除しない。\n" {
		t.Fatalf("%+v", d.Instructions)
	}
}

// TestSummaryHandlesAreOnlyTheOnesPresented is A22: a handle that was not presented, has a
// gap, is of another stage or is a second previous summary is refused, and an important
// observation must be an Observation.
func TestSummaryHandlesAreOnlyTheOnesPresented(t *testing.T) {
	s := newScene(t)
	good := summaryAnswer(t, []item{it("保存を実装", "work-0", "instruction-0")}, nil, nil, []item{it("再開試験", "work-2")}, nil, "observation-0")
	res, err := compaction.ValidateSummary(s.in, good, "ckp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b9999")
	if err != nil {
		t.Fatal(err)
	}
	var mapped []string
	for _, m := range res.Stored.SourceMap {
		mapped = append(mapped, m.Handle)
		if len(m.Sources) == 0 {
			t.Errorf("%s has no sources", m.Handle)
		}
	}
	if strings.Join(mapped, ",") != "work-0,instruction-0,work-2,observation-0" {
		t.Fatalf("the source map has what is cited, in order of use: %v", mapped)
	}
	if res.Stored.SourceCheckpointID != "ckp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b9999" || len(res.Cited) != 1 || res.Cited[0] != 0 {
		t.Fatalf("%+v %v", res.Stored.SourceCheckpointID, res.Cited)
	}
	for name, ans := range map[string]string{
		"a handle beyond the last":                        summaryAnswer(t, []item{it("x", "work-3")}, nil, nil, nil, nil),
		"a handle with a leading zero":                    summaryAnswer(t, []item{it("x", "work-00")}, nil, nil, nil, nil),
		"another stage's handle":                          summaryAnswer(t, []item{it("x", "presented-0")}, nil, nil, nil, nil),
		"a previous summary there was not":                summaryAnswer(t, []item{it("x", "summary-0")}, nil, nil, nil, nil),
		"a second previous summary":                       summaryAnswer(t, []item{it("x", "summary-1")}, nil, nil, nil, nil),
		"a completion there was not":                      summaryAnswer(t, []item{it("x", "completion-0")}, nil, nil, nil, nil),
		"an important instruction":                        summaryAnswer(t, []item{it("x", "work-0")}, nil, nil, nil, nil, "instruction-0"),
		"an important observation that was not presented": summaryAnswer(t, []item{it("x", "work-0")}, nil, nil, nil, nil, "observation-9"),
		"a handle that is none of five":                   summaryAnswer(t, []item{it("x", "hash-0")}, nil, nil, nil, nil),
	} {
		if _, err := compaction.ValidateSummary(s.in, ans, "ckp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b9999"); !errors.Is(err, compaction.ErrSemantic) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestASummaryThatIsNotExactlyTheRequestedObjectIsRefused: the shape, as for Selection, and
// no Tool call written as text (H01, H12, H21).
func TestASummaryThatIsNotExactlyTheRequestedObjectIsRefused(t *testing.T) {
	s := newScene(t)
	ok := func(text string) string { return summaryAnswer(t, []item{it(text, "work-0")}, nil, nil, nil, nil) }
	if _, err := compaction.ValidateSummary(s.in, ok("進捗"), "ckp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b9999"); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"prose":                "Here is the summary.",
		"a fence":              "```json\n" + ok("x") + "\n```",
		"a second object":      ok("x") + ok("x"),
		"an extra key":         strings.Replace(ok("x"), `"decisions"`, `"notes":[],"decisions"`, 1),
		"a missing key":        `{"current_work":[]}`,
		"an unknown state":     summaryAnswer(t, nil, nil, []item{verify("x", "done", "work-0")}, nil, nil),
		"an empty handle list": summaryAnswer(t, []item{it("x")}, nil, nil, nil, nil),
		"an empty text":        ok(""),
		"a text over 8,192":    ok(strings.Repeat("a", 8193)),
		"a role item":          `{"role":"assistant","content":"x","tool_calls":[]}`,
	}
	for _, markup := range []string{`<tool_call>{"name":"x"}</tool_call>`, `<function=file.read>`, `[TOOL_CALLS][{"name":"x"}]`, `<|tool_call|>`, `<invoke name="x">`, "Before <TOOL_CALL> after"} {
		bad["tool markup "+markup] = ok("進捗 " + markup)
	}
	bad["markup in a reason"] = summaryAnswer(t, nil, []item{{"text": "x", "reason": "<tool_call>", "source_handles": []string{"work-0"}}}, nil, nil, nil)
	for name, ans := range bad {
		if _, err := compaction.ValidateSummary(s.in, ans, "ckp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b9999"); !errors.Is(err, compaction.ErrSemantic) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestPassedNeedsExecutionEvidenceThatSucceeded is H08: a verification may be passed only
// on an explicit execution that succeeded; a run that failed contradicts it; and a Summary
// that says nothing about Work it was given is not a Summary.
func TestPassedNeedsExecutionEvidenceThatSucceeded(t *testing.T) {
	w := newWorld()
	srcs := all(human(t, w, "`go test ./...` を確認する"), execExchange(w, "go build ./...", execView(2, "", "boom")), execExchange(w, "go test ./...", execView(0, "ok", "")), one(w.work("ok")))
	p := prepare(t, nil, srcs)
	sel, _ := compaction.BuildSelectionDataset(p)
	ret := compaction.NoSelection(p)
	in, err := compaction.BuildSummaryInput(p, ret, sel, nil)
	if err != nil {
		t.Fatal(err)
	}
	// observation-0 is the failed build, observation-1 the passing test; work-1 is a record.
	check := func(ans string) error {
		_, err := compaction.ValidateSummary(in, ans, "ckp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b9999")
		return err
	}
	for name, ans := range map[string]string{
		"passed on the work alone":           summaryAnswer(t, nil, nil, []item{verify("test", "passed", "work-0")}, nil, nil),
		"passed on a failed run":             summaryAnswer(t, nil, nil, []item{verify("build", "passed", "observation-0")}, nil, nil),
		"passed on both, one of them failed": summaryAnswer(t, nil, nil, []item{verify("all", "passed", "observation-0", "observation-1")}, nil, nil),
		"passed on an instruction":           summaryAnswer(t, nil, nil, []item{verify("test", "passed", "instruction-0")}, nil, nil),
		"nothing at all about the work":      summaryAnswer(t, nil, nil, nil, nil, nil),
	} {
		if err := check(ans); !errors.Is(err, compaction.ErrSemantic) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, ans := range map[string]string{
		"passed on a run that succeeded": summaryAnswer(t, nil, nil, []item{verify("test", "passed", "observation-1")}, nil, nil),
		"failed on a failed run":         summaryAnswer(t, nil, nil, []item{verify("build", "failed", "observation-0")}, nil, nil),
		"not run, with no evidence":      summaryAnswer(t, nil, nil, []item{verify("再開試験", "not_run", "work-0")}, nil, nil),
		"unknown, with no evidence":      summaryAnswer(t, nil, nil, []item{verify("lint", "unknown", "work-0")}, nil, nil),
	} {
		if err := check(ans); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestACompletionThatDidNotSucceedIsNoGroundForPassed and a previous Summary's passed is
// continued only when it itself rested on an execution.
func TestAPreviousSummariesPassedIsContinuedOnlyWhenItRestedOnAnExecution(t *testing.T) {
	prior := loadGoldenCheckpoint(t)
	log := decodeWire[contextplan.Projection](t, "projection_input.json").Entries[1].Messages[1].Text()
	build := func(verification contextplan.VerificationItem) *compaction.SummaryInput {
		cp := loadGoldenCheckpoint(t)
		cp.Candidate.Projection.Summary.Summary.Verification = []contextplan.VerificationItem{verification}
		w := newWorld()
		w.seq, w.item = 5, 5
		p := prepare(t, cp, one(w.work("続き")))
		in, err := compaction.BuildSummaryInput(p, compaction.NoSelection(p), nil, func(string) (string, error) { return log, nil })
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
	_ = prior
	ans := summaryAnswer(t, []item{it("続き", "work-0")}, nil, []item{verify("再開試験は通った", "passed", "summary-0")}, nil, nil)
	// The golden's summary cites only work: nothing executed stands behind its verification.
	in := build(contextplan.VerificationItem{Text: "x", State: "passed", SourceHandles: []string{"work-0"}})
	if _, err := compaction.ValidateSummary(in, ans, "ckp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b9999"); !errors.Is(err, compaction.ErrSemantic) {
		t.Fatalf("a passed that rested on work alone: %v", err)
	}
	in = build(contextplan.VerificationItem{Text: "x", State: "passed", SourceHandles: []string{"observation-0"}})
	if _, err := compaction.ValidateSummary(in, ans, "ckp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b9999"); err != nil {
		t.Fatalf("a passed that rested on an execution: %v", err)
	}
	notRun := summaryAnswer(t, []item{it("続き", "work-0")}, nil, []item{verify("再開試験は未実施", "not_run", "summary-0")}, nil, nil)
	in = build(contextplan.VerificationItem{Text: "x", State: "not_run", SourceHandles: []string{"work-0"}})
	if _, err := compaction.ValidateSummary(in, notRun, "ckp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b9999"); err != nil {
		t.Fatalf("what was not run stays not run: %v", err)
	}
	_ = fmt.Sprint
}
