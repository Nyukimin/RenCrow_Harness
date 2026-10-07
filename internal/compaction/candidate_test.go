package compaction_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

const newID = "ckp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b9999"

// counts are two verified exact counts, the first the prompt that did not fit.
func counts(before, after int64) compaction.Counts {
	mk := func(n int64) modelport.MeasureResult {
		limit := int64(200000)
		return modelport.MeasureResult{ContractVersion: modelport.ContractVersion, State: "verified_exact", PromptLower: &n, PromptUpper: &n, EffectiveContextLimit: &limit,
			ReservedOutputTokens: 4096, SafetyMarginTokens: 0, RequestDigest: strings.Repeat("a", 64), BindingFingerprint: fakeFP, EvidenceRef: modelport.Str("synthetic"), InputDigest: strings.Repeat("b", 64)}
	}
	return compaction.Counts{Before: mk(before), After: mk(after)}
}

// normalCandidate builds a Normal candidate through the pure steps, and the input it is
// validated against.
func normalCandidate(t testing.TB) (compaction.Candidate, compaction.ValidateInput) {
	t.Helper()
	s := newScene(t)
	res, err := compaction.ValidateSummary(s.in, summaryAnswer(t,
		[]item{it("保存処理を実装", "work-0", "instruction-0")}, nil,
		[]item{verify("go test は通った", "passed", "observation-0")}, []item{it("再開試験", "work-2")}, []item{it("再開試験を書く", "work-2")}, "observation-0"), newID)
	if err != nil {
		t.Fatal(err)
	}
	cand, err := compaction.BuildNormal(s.p, s.ret, s.in, res, newID, counts(50000, 4000))
	if err != nil {
		t.Fatal(err)
	}
	return cand, compaction.ValidateInput{Candidate: cand, Prepared: s.p, Retention: s.ret, Blocks: blocks}
}

func TestANormalCandidateIsMadeAndValidatedAndItsBytesRoundTrip(t *testing.T) {
	cand, in := normalCandidate(t)
	v, err := compaction.ValidateCandidate(in)
	if err != nil {
		t.Fatal(err)
	}
	c := v.Candidate()
	if c.Mode != "normal" || c.CheckpointID != newID || c.ParentCheckpointID != nil || c.Projection.Summary == nil || c.Projection.Summary.SourceCheckpointID != newID {
		t.Fatalf("%+v", c.Mode)
	}
	// The candidate keeps the instruction exactly and none of the Work it summarized.
	if len(c.Projection.Entries) != 1 || c.Projection.Entries[0].Kind != "instruction" || c.Projection.Entries[0].Messages[0].Text() != "go test ./... で確認しながら保存処理を実装する。" {
		t.Fatalf("entries %+v", c.Projection.Entries)
	}
	last := in.Prepared.Units[len(in.Prepared.Units)-1].Seq()
	if c.SemanticBoundary != last || c.DurableBoundary != last || *c.Projection.SummaryAnchorSequence != last {
		t.Fatalf("boundaries %d/%d anchor %d, want %d", c.SemanticBoundary, c.DurableBoundary, *c.Projection.SummaryAnchorSequence, last)
	}
	if len(c.RetainedExactRefs) != 1 || c.RetainedExactRefs[0] != in.Prepared.Units[0].Segments[0].Ref {
		t.Fatalf("retained %+v", c.RetainedExactRefs)
	}
	// The important Observation is a reference only, with what the Summary was given of it.
	if len(c.Projection.ImportantObservations) != 1 {
		t.Fatal("one important observation")
	}
	imp := c.Projection.ImportantObservations[0]
	if len(imp.PresentedRanges) != 0 || !imp.Partial || len(imp.SummaryCoveredRange) != 1 || imp.SummaryCoveredRange[0].Start != 0 {
		t.Fatalf("%+v", imp)
	}
	// Every Observation is in the inventory, the cited one covered and the others not.
	if len(c.ObservationInventory) != 1 {
		t.Fatalf("inventory %d", len(c.ObservationInventory))
	}
	// The bytes are the prefix, a NUL and the canonical JSON; they verify, hash and parse.
	blob := v.Bytes()
	if !bytes.HasPrefix(blob, []byte("rencrow-checkpoint-candidate/v1\x00{")) || bytes.HasSuffix(blob, []byte("\n")) {
		t.Fatal("the stored form is the prefix, NUL and the JSON, with no newline")
	}
	if v.Hash() != protocol.CheckpointCandidateHash(blob) {
		t.Fatal("the hash is over the exact bytes")
	}
	parsed, err := compaction.ParseCheckpoint(blob, v.Hash())
	if err != nil {
		t.Fatal(err)
	}
	again, err := parsed.Candidate.Bytes()
	if err != nil || !bytes.Equal(again, blob) {
		t.Fatal("a stored checkpoint, read and written again, is the same bytes")
	}
	if cj1Of(t, parsed.Candidate) != cj1Of(t, cand) && cj1Of(t, parsed.Candidate.Projection) != cj1Of(t, cand.Projection) {
		t.Fatal("the parsed candidate is not the made one")
	}
	// What is returned is a copy: changing it changes nothing stored.
	blob[40] ^= 0xff
	if bytes.Equal(v.Bytes(), blob) {
		t.Fatal("Bytes must be a copy")
	}
}

// TestACandidateIsRefusedWhenAnyFactOfItIsChanged is A65 and F13: each change of one field
// of an otherwise valid candidate is refused, with the kind of refusal that belongs to it.
func TestACandidateIsRefusedWhenAnyFactOfItIsChanged(t *testing.T) {
	mutations := map[string]func(c *compaction.Candidate, in *compaction.ValidateInput){
		"format version": func(c *compaction.Candidate, _ *compaction.ValidateInput) { c.FormatVersion = "rencrow-checkpoint/v2" },
		"thread": func(c *compaction.Candidate, _ *compaction.ValidateInput) {
			c.ThreadID = "thr_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b00ff"
		},
		"task": func(c *compaction.Candidate, _ *compaction.ValidateInput) {
			c.TaskID = "tsk_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b00ff"
		},
		"run": func(c *compaction.Candidate, _ *compaction.ValidateInput) {
			c.RunID = "run_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b00ff"
		},
		"a parent that is none": func(c *compaction.Candidate, _ *compaction.ValidateInput) {
			c.ParentCheckpointID = protocol.Str("ckp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b00ff")
		},
		"mode":                   func(c *compaction.Candidate, _ *compaction.ValidateInput) { c.Mode = "fork" },
		"context revision":       func(c *compaction.Candidate, _ *compaction.ValidateInput) { c.Expected.ContextRevision++ },
		"control revision":       func(c *compaction.Candidate, _ *compaction.ValidateInput) { c.Expected.ControlRevision++ },
		"writer epoch":           func(c *compaction.Candidate, _ *compaction.ValidateInput) { c.Expected.WriterEpoch++ },
		"policy revision":        func(c *compaction.Candidate, _ *compaction.ValidateInput) { c.Expected.PolicyRevision = "policy-2" },
		"binding revision":       func(c *compaction.Candidate, _ *compaction.ValidateInput) { c.Expected.BindingRevision = "binding-2" },
		"semantic boundary":      func(c *compaction.Candidate, _ *compaction.ValidateInput) { c.SemanticBoundary-- },
		"durable boundary":       func(c *compaction.Candidate, _ *compaction.ValidateInput) { c.DurableBoundary++ },
		"snapshot digest":        func(c *compaction.Candidate, _ *compaction.ValidateInput) { c.SnapshotDigest = strings.Repeat("c", 64) },
		"context blocks":         func(c *compaction.Candidate, _ *compaction.ValidateInput) { c.Projection.ContextBlocks = nil },
		"projection version":     func(c *compaction.Candidate, _ *compaction.ValidateInput) { c.Projection.FormatVersion = "x" },
		"an instruction dropped": func(c *compaction.Candidate, _ *compaction.ValidateInput) { c.Projection.Entries = nil },
		"an instruction's text": func(c *compaction.Candidate, _ *compaction.ValidateInput) {
			c.Projection.Entries[0].Messages[0] = modelport.User("do something else")
		},
		"an instruction's range": func(c *compaction.Candidate, _ *compaction.ValidateInput) {
			c.Projection.Entries[0].MessageSources[0][0].Range.End--
		},
		"an instruction's source": func(c *compaction.Candidate, _ *compaction.ValidateInput) {
			c.Projection.Entries[0].MessageSources[0][0].SourceID = "evd_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0abc"
		},
		"the retained references": func(c *compaction.Candidate, _ *compaction.ValidateInput) { c.RetainedExactRefs = nil },
		"the applied selection": func(c *compaction.Candidate, _ *compaction.ValidateInput) {
			c.AppliedSelection = []compaction.AppliedOp{{Target: c.RetainedExactRefs[0], Basis: "revocation", Evidence: []protocol.SourceRef{c.RetainedExactRefs[0]}}}
		},
		"the summary's source checkpoint": func(c *compaction.Candidate, _ *compaction.ValidateInput) {
			c.Projection.Summary.SourceCheckpointID = "ckp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b00ff"
		},
		"the anchor": func(c *compaction.Candidate, _ *compaction.ValidateInput) { *c.Projection.SummaryAnchorSequence-- },
		"a handle with no source map": func(c *compaction.Candidate, _ *compaction.ValidateInput) {
			c.Projection.Summary.SourceMap = c.Projection.Summary.SourceMap[1:]
		},
		"a source map entry twice": func(c *compaction.Candidate, _ *compaction.ValidateInput) {
			m := c.Projection.Summary.SourceMap
			c.Projection.Summary.SourceMap = append(m, m[0])
		},
		"a source map entry with no source": func(c *compaction.Candidate, _ *compaction.ValidateInput) {
			c.Projection.Summary.SourceMap[0].Sources = nil
		},
		"an important reference not in the inventory": func(c *compaction.Candidate, _ *compaction.ValidateInput) { c.ObservationInventory = nil },
		"an inventory entry twice": func(c *compaction.Candidate, _ *compaction.ValidateInput) {
			c.ObservationInventory = append(c.ObservationInventory, c.ObservationInventory[0])
		},
		"an important reference with another text": func(c *compaction.Candidate, _ *compaction.ValidateInput) {
			c.Projection.ImportantObservations[0].Tool = ""
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			cand, in := normalCandidate(t)
			mutate(&cand, &in)
			in.Candidate = cand
			_, err := compaction.ValidateCandidate(in)
			if err == nil {
				t.Fatal("the changed candidate was accepted")
			}
			if !errors.Is(err, compaction.ErrIntegrity) && !errors.Is(err, compaction.ErrNoFit) && !errors.Is(err, compaction.ErrNotShrunk) && !errors.Is(err, compaction.ErrUnverified) {
				t.Fatalf("an error of no known kind: %v", err)
			}
		})
	}
}

// TestACandidateIsRefusedUnlessItsCountsShowItFitsAndIsSmaller: H07 and the capacity rule.
func TestACandidateIsRefusedUnlessItsCountsShowItFitsAndIsSmaller(t *testing.T) {
	cases := map[string]struct {
		mutate func(c *compaction.Candidate)
		want   error
	}{
		"an estimated before": {func(c *compaction.Candidate) { c.BeforeCount.State = "estimated" }, compaction.ErrUnverified},
		"an unverified after": {func(c *compaction.Candidate) {
			c.AfterCount.State, c.AfterCount.PromptLower, c.AfterCount.PromptUpper = "unverified", nil, nil
		}, compaction.ErrUnverified},
		"a count with no limit":  {func(c *compaction.Candidate) { c.AfterCount.EffectiveContextLimit = nil }, compaction.ErrUnverified},
		"over the usable budget": {func(c *compaction.Candidate) { *c.AfterCount.PromptUpper, *c.AfterCount.PromptLower = 195905, 195905 }, compaction.ErrNoFit},
		"the same size": {func(c *compaction.Candidate) {
			n := *c.BeforeCount.PromptUpper
			*c.AfterCount.PromptUpper, *c.AfterCount.PromptLower = n, n
		}, compaction.ErrNotShrunk},
		"bigger": {func(c *compaction.Candidate) {
			n := *c.BeforeCount.PromptUpper + 1
			*c.AfterCount.PromptUpper, *c.AfterCount.PromptLower = n, n
		}, compaction.ErrNotShrunk},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cand, in := normalCandidate(t)
			// Counts are pointers shared with the made candidate: take a copy of each.
			for _, m := range []*modelport.MeasureResult{&cand.BeforeCount, &cand.AfterCount} {
				lo, up, lim := *m.PromptLower, *m.PromptUpper, *m.EffectiveContextLimit
				m.PromptLower, m.PromptUpper, m.EffectiveContextLimit = &lo, &up, &lim
			}
			tc.mutate(&cand)
			in.Candidate = cand
			if _, err := compaction.ValidateCandidate(in); !errors.Is(err, tc.want) {
				t.Fatalf("%v, want %v", err, tc.want)
			}
		})
	}
	// Bounds: an interval is a shrink only when the after's upper is below the before's lower.
	two := func(lo, up int64) modelport.MeasureResult {
		limit := int64(200000)
		return modelport.MeasureResult{State: "verified_bound", PromptLower: &lo, PromptUpper: &up, EffectiveContextLimit: &limit}
	}
	if compaction.Shrinks(two(900, 1100), two(800, 1000)) || !compaction.Shrinks(two(900, 1100), two(700, 899)) {
		t.Fatal("an interval shrinks only when it is wholly below the other")
	}
}

// TestAStoredCheckpointIsReadOnlyInItsExactForm is the load side: the design's golden blob
// loads; the same JSON in any other serialization, a changed version or hash, or a value that
// breaks the schema does not, and a broken checkpoint is an integrity error, never "none".
func TestAStoredCheckpointIsReadOnlyInItsExactForm(t *testing.T) {
	golden := readWire(t, "checkpoint_candidate.bin")
	var vec struct {
		Sha string `json:"candidate_sha256"`
	}
	if err := json.Unmarshal(readWire(t, "checkpoint_vector.json"), &vec); err != nil {
		t.Fatal(err)
	}
	cp, err := compaction.ParseCheckpoint(golden, vec.Sha)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Candidate.Mode != "emergency" || cp.Candidate.SemanticBoundary != 2 || cp.Candidate.DurableBoundary != 5 || cp.Hash != vec.Sha {
		t.Fatalf("%+v", cp.Candidate.Mode)
	}
	again, err := cp.Candidate.Bytes()
	if err != nil || !bytes.Equal(again, golden) {
		t.Fatalf("the design's candidate, written again, is its golden bytes: %v", err)
	}
	idx := bytes.IndexByte(golden, 0)
	prefix, body := golden[:idx+1], golden[idx+1:]
	rehash := func(b []byte) string { return protocol.CheckpointCandidateHash(b) }
	canonical := func(replace func(string) string) []byte {
		raw := replace(string(body))
		out, err := protocol.EncodeCheckpointCandidate([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	bad := map[string][]byte{
		"a trailing newline":    append(append([]byte(nil), golden...), '\n'),
		"a BOM":                 append([]byte("\xef\xbb\xbf"), golden...),
		"another version":       append([]byte("rencrow-checkpoint-candidate/v2\x00"), body...),
		"pretty printed":        append(append([]byte(nil), prefix...), bytes.Replace(body, []byte(`,"`), []byte(", \""), 1)...),
		"a float spelling":      append(append([]byte(nil), prefix...), bytes.Replace(body, []byte(`"writer_epoch":1`), []byte(`"writer_epoch":1.0`), 1)...),
		"keys in another order": append(append([]byte(nil), prefix...), []byte(`{"mode":"emergency","format_version":"rencrow-checkpoint/v1"}`)...),
		"truncated":             golden[:len(golden)-3],
		"empty":                 {},
		// Canonical, and still not a checkpoint: the schema is what refuses these.
		"a mode that is none": canonical(func(s string) string { return strings.Replace(s, `"mode":"emergency"`, `"mode":"other"`, 1) }),
		"a missing field": canonical(func(s string) string {
			return strings.Replace(s, `"snapshot_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",`, ``, 1)
		}),
		"an unknown field":         canonical(func(s string) string { return strings.Replace(s, `{"after_count"`, `{"zz":1,"after_count"`, 1) }),
		"a count that is a string": canonical(func(s string) string { return strings.Replace(s, `"prompt_lower":8000`, `"prompt_lower":"8000"`, 1) }),
	}
	for name, blob := range bad {
		t.Run(name, func(t *testing.T) {
			// Whatever hash the caller holds, a blob that is not the exact form is refused; with the
			// hash of the changed bytes (so only the form can refuse it) it must still be.
			for _, h := range []string{vec.Sha, rehash(blob)} {
				if _, err := compaction.ParseCheckpoint(blob, h); !errors.Is(err, compaction.ErrIntegrity) {
					t.Fatalf("hash %.8s: %v", h, err)
				}
			}
		})
	}
	// The hash of the exact bytes is the only hash that loads.
	if _, err := compaction.ParseCheckpoint(golden, strings.Repeat("0", 64)); !errors.Is(err, compaction.ErrIntegrity) {
		t.Fatalf("%v", err)
	}
}

// TestEmergencyCandidateMustBeTheLiveContextWithOnlyTheAllowedReplacements is F13 for the
// Emergency mode: the Summary, anchor, boundary and every entry as they were, and a tool
// message different from its original only where it is its own reference marker.
func TestEmergencyCandidateMustBeTheLiveContextWithOnlyTheAllowedReplacements(t *testing.T) {
	build := func() (compaction.Candidate, compaction.ValidateInput) {
		in := decodeWire[contextplan.Projection](t, "projection_input.json")
		p := preparedFromProjection(t, in, 2, 5)
		p.Snapshot.ThreadID, p.Snapshot.TaskID, p.Snapshot.RunID = threadID, taskID, runID
		p.Snapshot.Blocks = in.ContextBlocks
		em, err := compaction.Emergency(p)
		if err != nil {
			t.Fatal(err)
		}
		cand, err := compaction.BuildEmergency(p, em, newID, counts(20000, 8000))
		if err != nil {
			t.Fatal(err)
		}
		return cand, compaction.ValidateInput{Candidate: cand, Prepared: p, Emergency: &em, Blocks: in.ContextBlocks}
	}
	cand, in := build()
	v, err := compaction.ValidateCandidate(in)
	if err != nil {
		t.Fatal(err)
	}
	if v.Candidate().Mode != "emergency" || v.Candidate().SemanticBoundary != 2 || v.Candidate().Projection.Summary == nil {
		t.Fatal("the Emergency candidate keeps the Summary and its boundary")
	}
	_ = cand
	mutations := map[string]func(c *compaction.Candidate){
		"the summary changed":   func(c *compaction.Candidate) { c.Projection.Summary.Summary.CurrentWork[0].Text = "changed" },
		"the summary dropped":   func(c *compaction.Candidate) { c.Projection.Summary, c.Projection.SummaryAnchorSequence = nil, nil },
		"the anchor moved":      func(c *compaction.Candidate) { *c.Projection.SummaryAnchorSequence = 3 },
		"the semantic boundary": func(c *compaction.Candidate) { c.SemanticBoundary = 3 },
		"a selection applied": func(c *compaction.Candidate) {
			c.AppliedSelection = []compaction.AppliedOp{{Target: c.RetainedExactRefs[0], Basis: "revocation", Evidence: []protocol.SourceRef{c.RetainedExactRefs[0]}}}
		},
		"an entry dropped":       func(c *compaction.Candidate) { c.Projection.Entries = c.Projection.Entries[:2] },
		"an entry's sequence":    func(c *compaction.Candidate) { c.Projection.Entries[2].Sequence = 4 },
		"the work changed":       func(c *compaction.Candidate) { c.Projection.Entries[2].Messages[0] = modelport.Assistant("rewritten") },
		"an instruction changed": func(c *compaction.Candidate) { c.Projection.Entries[0].Messages[0] = modelport.User("rewritten") },
		"a marker of other text": func(c *compaction.Candidate) {
			c.Projection.Entries[1].Messages[1] = modelport.Tool("fixture-call-1", "RENCROW_OBSERVATION_REFERENCE_V1\n{}")
		},
		"the call ID of an answer": func(c *compaction.Candidate) { c.Projection.Entries[1].Messages[1].ToolCallID = "other" },
		"the call replaced": func(c *compaction.Candidate) {
			c.Projection.Entries[1].Messages[0].ToolCalls[0].Function.Arguments = "{}"
		},
		"the important references": func(c *compaction.Candidate) { c.Projection.ImportantObservations = c.ObservationInventory },
		"a reference shown in full with a marker": func(c *compaction.Candidate) {
			c.Projection.Entries[1].Observations[0].Reference.PresentedRanges = []contextplan.Range{{Start: 0, End: 11712}}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			cand, in := build()
			mutate(&cand)
			in.Candidate = cand
			if _, err := compaction.ValidateCandidate(in); !errors.Is(err, compaction.ErrIntegrity) {
				t.Fatalf("%v", err)
			}
		})
	}
}
