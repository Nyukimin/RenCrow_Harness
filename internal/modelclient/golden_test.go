package modelclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The design's wire vectors are byte-identical to the ones RenCrow_LLM's Gateway
// tests are written against (its harnesscontract/testdata, copied from the same
// design package), so a round trip over these bytes is a round trip with the
// Gateway's own forms: what the client sends is the vector, and what it is served is
// read to the vector's meaning.

func canonical(t testing.TB, raw []byte) []byte {
	t.Helper()
	b, err := protocol.EncodeCanonicalContract(raw)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGoldenMeasureRoundTrip(t *testing.T) {
	req := decodeRequest(t, "generation_request.json")
	fixture := wireFixture(t, "measure_result.json")
	g := newGateway(t)
	g.reply(measureKey, 200, "application/json", fixture)

	res, err := g.client().Measure(t.Context(), modelport.MeasureRequest{ContractVersion: "harness-v1", Request: req, SafetyMarginTokens: 256})
	if err != nil {
		t.Fatal(err)
	}
	// What was sent is the design's request inside the measure envelope.
	envelope := append(append([]byte(`{"contract_version":"harness-v1","request":`), wireFixture(t, "generation_request.json")...), []byte(`,"safety_margin_tokens":256}`)...)
	if got, want := canonical(t, g.body(measureKey, 0)), canonical(t, envelope); !bytes.Equal(got, want) {
		t.Fatalf("the measure request is not the vector\n got %s\nwant %s", got, want)
	}
	// What was served is read to the vector's meaning.
	var want modelport.MeasureResult
	if err := json.Unmarshal(fixture, &want); err != nil {
		t.Fatal(err)
	}
	if res.State != "verified_exact" || *res.PromptLower != 8000 || res.InputDigest != "3dae8769d753391e270b48f531d8a1abbcc8ea21696fbea0d5a3761965ab54ef" ||
		res.RequestDigest != "67c6b1a6b70afa58e302faa146a89dffc30276f0162c8667621cb91ec9722ad5" || res.BindingFingerprint != want.BindingFingerprint ||
		*res.EffectiveContextLimit != 32768 || res.ReservedOutputTokens != 4096 || res.SafetyMarginTokens != 256 || *res.EvidenceRef != "synthetic-fixture-not-tokenizer" {
		t.Fatalf("%+v", res)
	}
	if d, _ := req.LogicalInputDigest(); d != res.InputDigest {
		t.Fatal("the Harness's own digest is not the vector's")
	}

	// The same measure without the expected_* values (the first measure of an Attempt).
	first := req
	first.Rencrow.Harness.ExpectedInputDigest, first.Rencrow.Harness.ExpectedRequestDigest, first.Rencrow.Harness.ExpectedBindingFingerprint = "", "", ""
	g2 := newGateway(t)
	g2.reply(measureKey, 200, "application/json", fixture)
	if _, err := g2.client().Measure(t.Context(), modelport.MeasureRequest{ContractVersion: "harness-v1", Request: first, SafetyMarginTokens: 256}); err != nil {
		t.Fatal(err)
	}
}

func TestGoldenActStreamRoundTrip(t *testing.T) {
	req := decodeRequest(t, "stream_request.json")
	fixture := wireFixture(t, "act_tool_stream.sse")
	g := newGateway(t)
	g.reply(generateKey, 200, "text/event-stream", fixture)

	rc, err := g.client().Generate(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := canonical(t, g.body(generateKey, 0)), canonical(t, wireFixture(t, "stream_request.json")); !bytes.Equal(got, want) {
		t.Fatalf("the generation request is not the vector\n got %s\nwant %s", got, want)
	}

	// The stream is passed on unread and unchanged.
	raw, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(raw, fixture) {
		t.Fatalf("the stream was changed on its way (%v)", err)
	}

	g2 := newGateway(t)
	g2.reply(generateKey, 200, "text/event-stream", fixture)
	rc, err = g2.client().Generate(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	c := assemble(t, rc)
	if c.TerminalKind != modelport.KindToolCalls || !c.TerminalObserved || !c.DoneObserved || c.GenerationState != "terminal" || c.HiddenRetry {
		t.Fatalf("%+v", c)
	}
	if len(c.ToolIntents) != 1 || c.ToolIntents[0].Name != "file.read" || c.ToolIntents[0].ArgumentsJSON != `{"path":"demo.txt"}` || c.ToolIntents[0].ProviderToolCallID != "fixture-read-1" {
		t.Fatalf("%+v", c.ToolIntents)
	}
	h := req.Rencrow.Harness
	exp := modelport.Expected{
		Stage: h.Stage, InputDigest: h.ExpectedInputDigest, RequestDigest: h.ExpectedRequestDigest, BindingFingerprint: h.ExpectedBindingFingerprint,
		ProfileID: h.Recovery.ProfileID, ProfileRevision: h.Recovery.ProfileRevision,
	}
	if err := modelport.VerifyReceipt(*c.Receipt, exp); err != nil {
		t.Fatalf("the vector's receipt is not the request's: %v", err)
	}
	if *c.Usage.PromptTokens != 8000 || *c.Usage.CompletionTokens != 20 || c.Usage.CachedTokens != nil || !c.Receipt.UsageComplete {
		t.Fatalf("%+v", c.Usage)
	}
}

// The three calls of one Attempt, in order, against the Fake model standing in as
// the Gateway: one GET, one measure POST, one generation POST, nothing more.
func TestOneAttemptIsOneStatusOneMeasureOneGeneration(t *testing.T) {
	f := harnesstest.NewFake()
	f.SetReply(harnesstest.Final("done こんにちは"))
	g := newGateway(t)
	g.on(statusKey, func(w http.ResponseWriter, _ *http.Request) {
		d, _ := f.Describe(context.Background(), testBinding)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(status(t, map[string]any{"harness-v1": advertisement(mustMarshal(t, d))}))
	})
	g.on(measureKey, func(w http.ResponseWriter, r *http.Request) {
		var mr modelport.MeasureRequest
		if err := json.NewDecoder(r.Body).Decode(&mr); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		res, err := f.Measure(r.Context(), mr)
		if err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})
	g.on(generateKey, fakeGenerate(t, f))
	c := g.client()
	ctx := t.Context()

	desc, err := c.Describe(ctx, testBinding)
	if err != nil {
		t.Fatal(err)
	}
	req, err := modelport.NewActRequest(modelport.ActParams{
		Binding: testBinding, Descriptor: desc, Messages: []modelport.ChatMessage{modelport.User("hi")},
		Meta:     modelport.RequestMeta{RequestID: "req_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000a", TraceID: "trc_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000b", TaskID: "tsk_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000c", SessionID: "ses_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000d", Initiator: "user:ren", Caller: "harness.test"},
		Recovery: modelport.RecoveryRequest{ProfileID: "same_request", ProfileRevision: "builtin-v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := req.LogicalInputDigest()
	res, err := c.Measure(ctx, modelport.MeasureRequest{ContractVersion: "harness-v1", Request: req, SafetyMarginTokens: 256})
	if err != nil || res.InputDigest != digest {
		t.Fatalf("%v %+v", err, res)
	}
	gen := req.WithExpected(digest, res.RequestDigest, desc.BindingFingerprint)
	rc, err := c.Generate(ctx, gen)
	if err != nil {
		t.Fatal(err)
	}
	comp := assemble(t, rc)
	if comp.TerminalKind != modelport.KindFinal || comp.FinalText != "done こんにちは" {
		t.Fatalf("%+v", comp)
	}
	h := gen.Rencrow.Harness
	if err := modelport.VerifyReceipt(*comp.Receipt, modelport.Expected{Stage: h.Stage, InputDigest: digest, RequestDigest: res.RequestDigest, BindingFingerprint: desc.BindingFingerprint, ProfileID: h.Recovery.ProfileID, ProfileRevision: h.Recovery.ProfileRevision}); err != nil {
		t.Fatal(err)
	}
	if g.count(statusKey) != 1 || g.count(measureKey) != 1 || g.count(generateKey) != 1 || g.requests() != 3 {
		t.Fatalf("status=%d measure=%d generate=%d total=%d", g.count(statusKey), g.count(measureKey), g.count(generateKey), g.requests())
	}

	// A binding that changed between the count and the generation is refused by the
	// Gateway before it generates, and the client reports that and nothing else.
	f.Fingerprint = "bfp-v1:" + testRequestDigest
	rc, err = c.Generate(ctx, gen)
	var se *modelport.StrictError
	if rc != nil || !errors.As(err, &se) || se.Code != "BINDING_CHANGED" || se.GenerationState() != "not_started" {
		t.Fatalf("%v", err)
	}
	if g.count(generateKey) != 2 {
		t.Fatalf("%d generation requests", g.count(generateKey))
	}
}

func mustMarshal(t testing.TB, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
