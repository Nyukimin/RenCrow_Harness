package modelclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

const (
	testRequestDigest = "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
	testFingerprint   = harnesstest.FakeFingerprint
	secretMarker      = "SECRET-MARKER-7f3a"
)

var testBinding = protocol.Binding{Kind: "model_route", Selector: "fixture-local--host", ProfileRevision: "fixture-profile-1"}

// wireFixture reads one file of the design's wire vectors.
func wireFixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(harnesstest.ModuleRoot(t), "testdata", "contract", "examples", "wire", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeRequest(t testing.TB, name string) modelport.ChatRequest {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(wireFixture(t, name)))
	dec.DisallowUnknownFields()
	var r modelport.ChatRequest
	if err := dec.Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := marshalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// gateway is a fake RenCrow_LLM Gateway on a loopback port that counts every HTTP
// request it receives, so a test can say how many a call made.
type gateway struct {
	t        testing.TB
	srv      *httptest.Server
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	hits     map[string]int
	bodies   map[string][][]byte
	headers  map[string][]http.Header
	total    int
}

func newGateway(t testing.TB) *gateway {
	g := &gateway{t: t, handlers: map[string]http.HandlerFunc{}, hits: map[string]int{}, bodies: map[string][][]byte{}, headers: map[string][]http.Header{}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gateway) serve(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	body, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	g.total++
	g.hits[key]++
	g.bodies[key] = append(g.bodies[key], body)
	g.headers[key] = append(g.headers[key], r.Header.Clone())
	h := g.handlers[key]
	g.mu.Unlock()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	h(w, r)
}

func (g *gateway) on(key string, h http.HandlerFunc) {
	g.mu.Lock()
	g.handlers[key] = h
	g.mu.Unlock()
}

// reply makes the Gateway answer one call with a fixed status, type and body.
func (g *gateway) reply(key string, status int, contentType string, body []byte) {
	g.on(key, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

func (g *gateway) count(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits[key]
}

func (g *gateway) requests() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.total
}

func (g *gateway) body(key string, i int) []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.bodies[key][i]
}

func (g *gateway) header(key string, i int) http.Header {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.headers[key][i]
}

func (g *gateway) url() string { return g.srv.URL + "/v1" }

func (g *gateway) client() *Client {
	g.t.Helper()
	c, err := New(g.url(), Options{})
	if err != nil {
		g.t.Fatal(err)
	}
	return c
}

const (
	statusKey   = "GET /v1/status"
	measureKey  = "POST /v1/context/measure"
	generateKey = "POST /v1/chat/completions"
)

// actRequest is a strict act request on the test binding, with a Japanese message
// and a marker that must never appear in any error text.
func actRequest(t testing.TB) modelport.ChatRequest {
	t.Helper()
	desc, err := harnesstest.NewFake().Describe(context.Background(), testBinding)
	if err != nil {
		t.Fatal(err)
	}
	req, err := modelport.NewActRequest(modelport.ActParams{
		Binding: testBinding, Descriptor: desc,
		Messages: []modelport.ChatMessage{modelport.System("system text"), modelport.User("こんにちは " + secretMarker)},
		Meta: modelport.RequestMeta{
			RequestID: "req_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000a", TraceID: "trc_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000b",
			TaskID: "tsk_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000c", SessionID: "ses_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000d",
			Initiator: "user:ren", Caller: "harness.test",
		},
		Recovery: modelport.RecoveryRequest{ProfileID: "same_request", ProfileRevision: "builtin-v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// genRequest completes an act request into a generation request.
func genRequest(t testing.TB, req modelport.ChatRequest) modelport.ChatRequest {
	t.Helper()
	d, err := req.LogicalInputDigest()
	if err != nil {
		t.Fatal(err)
	}
	return req.WithExpected(d, testRequestDigest, testFingerprint)
}

// stageRequest turns an act request into a no-tools stage request (non-stream).
func stageRequest(t testing.TB, stage string) modelport.ChatRequest {
	t.Helper()
	req := actRequest(t)
	req.Stream, req.StreamOptions = false, nil
	req.ToolChoice = "none"
	req.ResponseFormat = modelport.ResponseFormat{Type: "json_object"}
	req.Rencrow.Harness.Stage = stage
	req.Rencrow.Purpose = modelport.Str(stage)
	return genRequest(t, req)
}

func inputDigestOf(t testing.TB, req modelport.ChatRequest) string {
	t.Helper()
	d, err := req.LogicalInputDigest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// receiptWire is a GatewayAttemptReceipt as the Gateway states it.
func receiptWire(stage, state string, attempts any, input, request, fp any) map[string]any {
	return map[string]any{
		"contract_version": "harness-v1", "stage": stage, "binding_fingerprint": fp, "request_digest": request, "logical_requests": 1,
		"backend_attempts": attempts, "generation_state": state, "hidden_retry": false, "recovery_profile": "same_request",
		"recovery_profile_revision": "builtin-v1", "applied_transformations": []any{}, "usage_complete": state == "terminal", "input_digest": input,
	}
}

func terminalReceipt(t testing.TB, req modelport.ChatRequest) map[string]any {
	h := req.Rencrow.Harness
	return receiptWire(h.Stage, "terminal", 1, h.ExpectedInputDigest, h.ExpectedRequestDigest, h.ExpectedBindingFingerprint)
}

func notStartedReceipt(t testing.TB, req modelport.ChatRequest) map[string]any {
	h := req.Rencrow.Harness
	return receiptWire(h.Stage, "not_started", 0, h.ExpectedInputDigest, h.ExpectedRequestDigest, h.ExpectedBindingFingerprint)
}

func unknownReceipt(req modelport.ChatRequest) map[string]any {
	return receiptWire(req.Rencrow.Harness.Stage, "unknown", nil, nil, nil, nil)
}

// strictBody is a StrictError as the Gateway sends it.
func strictBody(t testing.TB, code string, retryable bool, receipt map[string]any) []byte {
	return mustJSON(t, map[string]any{
		"error":   map[string]any{"code": code, "message": "refused", "retryable": retryable, "request_id": nil, "source_code": nil},
		"rencrow": map[string]any{"harness_receipt": receipt},
	})
}

// measureWire is a verified_exact measure result for the request.
func measureWire(req modelport.ChatRequest, edit func(m map[string]any)) map[string]any {
	d, _ := req.LogicalInputDigest()
	m := map[string]any{
		"contract_version": "harness-v1", "state": "verified_exact", "prompt_lower": 100, "prompt_upper": 100, "effective_context_limit": 32768,
		"reserved_output_tokens": req.MaxTokens, "safety_margin_tokens": 256, "request_digest": testRequestDigest,
		"binding_fingerprint": testFingerprint, "evidence_ref": "synthetic-test", "reason": nil, "input_digest": d,
	}
	if edit != nil {
		edit(m)
	}
	return m
}

func measureRequest(req modelport.ChatRequest) modelport.MeasureRequest {
	return modelport.MeasureRequest{ContractVersion: "harness-v1", Request: req, SafetyMarginTokens: 256}
}

func asStrict(t testing.TB, err error) *modelport.StrictError {
	t.Helper()
	var se *modelport.StrictError
	if !errors.As(err, &se) {
		t.Fatalf("want a *StrictError, got %T %v", err, err)
	}
	return se
}

// stateOf is how the kernel reads an error from Generate: the receipt's state, and
// unknown for anything that is not a strict refusal.
func stateOf(err error) string {
	var se *modelport.StrictError
	if errors.As(err, &se) {
		return se.GenerationState()
	}
	return modelport.StateUnknown
}

// assemble reads a returned stream the way the kernel does.
func assemble(t testing.TB, rc io.ReadCloser) modelport.Completion {
	t.Helper()
	defer rc.Close()
	return modelport.AssembleStrictStream(rc, modelport.AssembleOptions{})
}

func sseHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}
}

func notLeaking(t testing.TB, g *gateway, texts ...string) {
	t.Helper()
	host := strings.TrimPrefix(g.srv.URL, "http://")
	for _, s := range texts {
		for _, bad := range []string{secretMarker, host, "127.0.0.1"} {
			if strings.Contains(s, bad) {
				t.Fatalf("an error text leaks %q: %s", bad, s)
			}
		}
	}
}

// closeConnection takes the connection and closes it without writing a byte: the
// Gateway died after the request arrived.
func closeConnection(w http.ResponseWriter, _ *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic("the test server cannot hijack")
	}
	conn, _, err := hj.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func jn(s string) json.Number { return json.Number(s) }
