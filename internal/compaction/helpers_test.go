package compaction_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolview"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

const (
	threadID = "thr_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0015"
	taskID   = "tsk_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000c"
	runID    = "run_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0016"
	fakeFP   = "bfp-v1:cf25685e3cbff5f9364f8bdadb54d8b852578931ad2602a62e077613b3a6ef8d"
)

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// world is a synthetic Thread: a counter that hands out application coordinates and
// acceptance sequences, and the texts of the evidence it made (a stored text is read back
// by its ID).
type world struct {
	mu    sync.Mutex
	seq   int64 // context_seq
	item  int64 // items.sequence
	texts map[string]string
}

func newWorld() *world { return &world{texts: map[string]string{}} }

func (w *world) source(history, origin, itemKind, callID, text string) compaction.Source {
	w.seq++
	w.item++
	id := identity.NewEvidenceID().String()
	w.mu.Lock()
	w.texts[id] = text
	w.mu.Unlock()
	return compaction.Source{
		MessageID: identity.NewMessageID().String(), ContextSeq: w.seq, ItemSeq: w.item, HistoryKind: history, Origin: origin, ItemKind: itemKind, ToolCallID: callID,
		EvidenceID: id, RawHash: hash(text), TotalBytes: int64(len(text)), CaptureComplete: true, TextProjection: true, Text: text,
	}
}

func (w *world) human(text string) compaction.Source {
	return w.source(contextplan.HistoryHuman, protocol.OriginHuman, "", "", text)
}
func (w *world) automation(text string) compaction.Source {
	return w.source(contextplan.HistoryAutomation, protocol.OriginAutomation, "", "", text)
}
func (w *world) unknown(text string) compaction.Source {
	return w.source(contextplan.HistoryProtected, protocol.OriginUnknown, "", "", text)
}
func (w *world) work(text string) compaction.Source {
	return w.source(contextplan.HistoryWork, compaction.OriginAgent, "", "", text)
}

// read is what a store gives back for an Evidence ID.
func (w *world) read(id string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.texts[id]
	if !ok {
		return "", fmt.Errorf("%w: no such evidence", compaction.ErrIntegrity)
	}
	return t, nil
}

// view is the stored answer of a Tool call: the Tool view as the dispatcher writes it.
func view(tool, effect string, exit *int64, captureComplete bool, result any) string {
	var raw json.RawMessage
	if result != nil {
		raw, _ = json.Marshal(result)
	}
	text, err := toolview.Encode(toolview.View{Tool: tool, ActionID: identity.NewActionID().String(), EffectState: effect, ExitCode: exit, CaptureComplete: captureComplete, Result: raw})
	if err != nil {
		panic(err)
	}
	return string(text)
}

func i64(v int64) *int64 { return &v }

// execOpts shapes the answer of a process.exec.
type execOpts struct {
	exit      int64
	stdout    string
	stderr    string
	truncated bool // the previews do not show all of the output
	timedOut  bool
	partial   bool // the output capture is incomplete
}

func execViewWith(o execOpts) string {
	type stream struct {
		EvidenceID       string `json:"evidence_id"`
		TotalBytes       int    `json:"total_bytes"`
		CaptureComplete  bool   `json:"capture_complete"`
		Preview          string `json:"preview"`
		PreviewTruncated bool   `json:"preview_truncated"`
	}
	res := map[string]any{"exit_code": o.exit, "signaled": false, "timed_out": o.timedOut, "profile": "p",
		"stdout": stream{identity.NewEvidenceID().String(), len(o.stdout), !o.partial, o.stdout, o.truncated},
		"stderr": stream{identity.NewEvidenceID().String(), len(o.stderr), !o.partial, o.stderr, false}}
	effect := "completed"
	if o.exit != 0 || o.timedOut {
		effect = "failed"
	}
	return view("process.exec", effect, &o.exit, !o.partial, res)
}

// execView is the answer of a process.exec that ran to an end.
func execView(exit int64, stdout, stderr string) string {
	return execViewWith(execOpts{exit: exit, stdout: stdout, stderr: stderr})
}

// exchange is an assistant message that called Tools and the answers, as stored: a record
// of the calls and one answer text per call.
func (w *world) exchange(calls []modelport.ToolCall, answers []string) []compaction.Source {
	rec, err := contextplan.EncodeToolCalls(contextplan.ToolCallsRecord{ToolCalls: calls})
	if err != nil {
		panic(err)
	}
	out := []compaction.Source{w.source(contextplan.HistoryWork, compaction.OriginAgent, contextplan.ItemKindToolCalls, "", string(rec))}
	for i, c := range calls {
		out = append(out, w.source(contextplan.HistoryObservation, compaction.OriginTool, contextplan.ItemKindToolResult, c.ID, answers[i]))
	}
	return out
}

func call(id, name, args string) modelport.ToolCall {
	return modelport.ToolCall{ID: id, Type: "function", Function: modelport.ToolFunction{Name: name, Arguments: args}}
}

func execCall(id, executable string, argv ...string) modelport.ToolCall {
	a, _ := json.Marshal(map[string]any{"executable": executable, "argv": argv, "cwd": ".", "env_profile_ref": "clean", "timeout_seconds": 60})
	return call(id, "process.exec", string(a))
}

func all(groups ...[]compaction.Source) []compaction.Source {
	var out []compaction.Source
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}
func one(s compaction.Source) []compaction.Source { return []compaction.Source{s} }

var blocks = []protocol.ContextBlock{stable("stable standing text")}

func stable(text string) protocol.ContextBlock {
	rev, err := protocol.ContextRevision(protocol.KindStableRuntimeContext, text, nil)
	if err != nil {
		panic(err)
	}
	return protocol.ContextBlock{Kind: protocol.KindStableRuntimeContext, Text: text, Revision: rev}
}

func snapshot(prior *compaction.Checkpoint, tail []compaction.Source) compaction.Snapshot {
	s := compaction.Snapshot{ThreadID: threadID, TaskID: taskID, RunID: runID, ContextRevision: 7, ControlRevision: 0, WriterEpoch: 1,
		PolicyRevision: "policy-1", BindingRevision: "binding-1", Blocks: blocks, Prior: prior, Tail: tail}
	if prior != nil && prior.Candidate.Projection.Summary != nil && prior.Candidate.Projection.Summary.SourceCheckpointID != prior.Candidate.CheckpointID {
		// An Emergency checkpoint keeps the Summary an earlier checkpoint accepted: stand that one in.
		s.SummaryCheckpoint = &compaction.Checkpoint{Candidate: compaction.Candidate{CheckpointID: prior.Candidate.Projection.Summary.SourceCheckpointID},
			Bytes: []byte("an earlier checkpoint"), Hash: hash("an earlier checkpoint")}
	}
	return s
}

func prepare(t testing.TB, prior *compaction.Checkpoint, tail ...[]compaction.Source) *compaction.Prepared {
	t.Helper()
	p, err := compaction.Prepare(snapshot(prior, all(tail...)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// systemPrompt is the act prompt text the fake counter renders with.
func systemPrompt(t testing.TB) string {
	t.Helper()
	s, err := contextplan.ActSystemPrompt()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fakePorts is the outside world of the Engine: it counts a prompt as its size in bytes
// divided by four (so a smaller prompt really is a smaller count), answers each stage with
// a script, and keeps what it was asked.
type fakePorts struct {
	t      testing.TB
	w      *world
	limit  int64 // effective context limit
	margin int64
	// state is the count state ("" is verified_exact).
	state string

	mu        sync.Mutex
	actCounts []int64
	stageReqs []compaction.StageRequest
	records   []string
	runs      map[string]int
	replies   map[string]compaction.StageReply
	errs      map[string]error // by stage: the error RunStage gives
	countErr  error
	stageSize map[string]int64 // forced token count of a stage request
	// afterOverride may force the token count of an act prompt made from a projection.
	afterOverride func(p contextplan.Projection) *int64
	nextID        string
}

func newPorts(t testing.TB, w *world) *fakePorts {
	return &fakePorts{t: t, w: w, limit: 200000, runs: map[string]int{}, replies: map[string]compaction.StageReply{}, errs: map[string]error{}, stageSize: map[string]int64{},
		nextID: "ckp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b9999"}
}

func (f *fakePorts) measure(messages []modelport.ChatMessage, forced int64) (compaction.Count, error) {
	if f.countErr != nil {
		return compaction.Count{}, f.countErr
	}
	var n int64
	for _, m := range messages {
		n += int64(len(m.Text())) + 8
		for _, c := range m.ToolCalls {
			n += int64(len(c.Function.Arguments)) + 8
		}
	}
	tokens := n / 4
	if forced > 0 {
		tokens = forced
	}
	limit := f.limit
	state := f.state
	if state == "" {
		state = "verified_exact"
	}
	res := modelport.MeasureResult{ContractVersion: modelport.ContractVersion, State: state, PromptLower: &tokens, PromptUpper: &tokens, EffectiveContextLimit: &limit,
		ReservedOutputTokens: 4096, SafetyMarginTokens: f.margin, RequestDigest: strings.Repeat("a", 64), BindingFingerprint: fakeFP, EvidenceRef: modelport.Str("synthetic"),
		InputDigest: strings.Repeat("b", 64)}
	if state == "unverified" || state == "estimated" {
		if state == "unverified" {
			res.PromptLower, res.PromptUpper, res.EffectiveContextLimit = nil, nil, nil
		}
		return compaction.Count{Result: res, Verdict: contextplan.VerdictUnverified}, nil
	}
	usable := limit - 4096 - f.margin
	verdict := contextplan.VerdictFit
	if tokens > usable {
		verdict = contextplan.VerdictNoFit
	}
	return compaction.Count{Result: res, Verdict: verdict, EvidenceID: identity.NewEvidenceID().String()}, nil
}

func (f *fakePorts) CountAct(_ context.Context, p contextplan.Projection) (compaction.Count, error) {
	plan, err := contextplan.AssembleContext(contextplan.Input{SystemPrompt: systemPrompt(f.t), Projection: &p})
	if err != nil {
		f.t.Fatalf("a candidate projection cannot be rendered: %v", err)
	}
	var forced int64
	if f.afterOverride != nil {
		if n := f.afterOverride(p); n != nil {
			forced = *n
		}
	}
	c, err := f.measure(plan.Messages, forced)
	f.mu.Lock()
	if err == nil && c.Result.PromptUpper != nil {
		f.actCounts = append(f.actCounts, *c.Result.PromptUpper)
	}
	f.mu.Unlock()
	return c, err
}

func (f *fakePorts) CountStage(_ context.Context, req compaction.StageRequest) (compaction.Count, error) {
	return f.measure(req.Messages, f.stageSize[req.Stage])
}

func (f *fakePorts) RunStage(_ context.Context, req compaction.StageRequest) (compaction.StageReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stageReqs = append(f.stageReqs, req)
	f.runs[req.Stage]++
	return f.replies[req.Stage], f.errs[req.Stage]
}

func (f *fakePorts) ReadText(_ context.Context, id string) (string, error) { return f.w.read(id) }

func (f *fakePorts) Record(_ context.Context, purpose, _ string, data []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, purpose)
	return identity.NewEvidenceID().String(), nil
}

func (f *fakePorts) NewCheckpointID() string { return f.nextID }

func (f *fakePorts) generations() int {
	return f.runs[modelport.StageSelection] + f.runs[modelport.StageSummary]
}

// loadGoldenCheckpoint is the design's checkpoint candidate (an Emergency checkpoint of a
// synthetic Thread), loaded the way a stored one is.
func loadGoldenCheckpoint(t testing.TB) *compaction.Checkpoint {
	t.Helper()
	var vec struct {
		Sha string `json:"candidate_sha256"`
	}
	if err := json.Unmarshal(readWire(t, "checkpoint_vector.json"), &vec); err != nil {
		t.Fatal(err)
	}
	cp, err := compaction.ParseCheckpoint(readWire(t, "checkpoint_candidate.bin"), vec.Sha)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

func readWire(t testing.TB, name string) []byte { return readWireFile(t, name) }

func decodeWire[T any](t testing.TB, name string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(readWire(t, name), &v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v
}

// cj1Of is the CJ1 text of any value.
func cj1Of(t testing.TB, v any) string {
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

// preparedFromProjection is a Prepared whose live context is a stored projection's
// entries, as an Emergency compaction of a checkpoint would see it.
func preparedFromProjection(t testing.TB, pr contextplan.Projection, semantic, durable int64) *compaction.Prepared {
	t.Helper()
	units, err := compaction.InheritedUnits(pr)
	if err != nil {
		t.Fatal(err)
	}
	p := &compaction.Prepared{Snapshot: snapshot(nil, nil), Units: units, Summary: pr.Summary, Anchor: pr.SummaryAnchorSequence, SemanticBoundary: semantic, DurableBoundary: durable}
	for _, u := range units {
		for _, o := range u.Entry.Observations {
			p.Inventory = append(p.Inventory, o.Reference)
		}
		for _, srcs := range u.Entry.MessageSources {
			p.AppliedSources = append(p.AppliedSources, srcs...)
		}
	}
	return p
}
