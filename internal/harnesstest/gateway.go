package harnesstest

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Gateway is a fake RenCrow_LLM Gateway on a loopback port: it serves a Fake model in the
// Gateway's own wire forms (GET /v1/status with the harness-v1 advertisement, POST
// /v1/context/measure, POST /v1/chat/completions with the strict stream or the strict
// error), so that a test can run the real client and the whole production composition
// against it. It counts every HTTP request it receives. Test support only: nothing in
// a configuration can select it.
type Gateway struct {
	t       testing.TB
	srv     *httptest.Server
	fake    *Fake
	binding protocol.Binding

	mu   sync.Mutex
	hits map[string]int
}

// NewGateway starts the Gateway for the binding, answering with the Fake. It is stopped
// when the test ends.
func NewGateway(t testing.TB, f *Fake, binding protocol.Binding) *Gateway {
	g := &Gateway{t: t, fake: f, binding: binding, hits: map[string]int{}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

// BaseURL is the base URL a configuration names (http://127.0.0.1:<port>/v1).
func (g *Gateway) BaseURL() string { return g.srv.URL + "/v1" }

// Hits is how many requests a path ("/status", "/context/measure", "/chat/completions")
// has received.
func (g *Gateway) Hits(path string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits[path]
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if len(path) >= 3 && path[:3] == "/v1" {
		path = path[3:]
	}
	g.mu.Lock()
	g.hits[path]++
	g.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && path == "/status":
		g.status(w, r)
	case r.Method == http.MethodPost && path == "/context/measure":
		g.measure(w, r)
	case r.Method == http.MethodPost && path == "/chat/completions":
		g.generate(w, r)
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "unencodable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func (g *Gateway) status(w http.ResponseWriter, r *http.Request) {
	d, err := g.fake.Describe(r.Context(), g.binding)
	bindings := []any{}
	if err == nil {
		bindings = append(bindings, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "aliases": map[string]any{}, "model_routes": map[string]any{},
		"contracts": map[string]any{"harness-v1": map[string]any{
			"normalization": "strict-v1", "measure": true, "generation_retry": "disabled", "attempt_receipt": true, "explicit_recovery": true,
			"bindings": bindings,
		}},
	})
}

func (g *Gateway) measure(w http.ResponseWriter, r *http.Request) {
	var req modelport.MeasureRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "unreadable", http.StatusBadRequest)
		return
	}
	res, err := g.fake.Measure(r.Context(), req)
	var se *modelport.StrictError
	switch {
	case errors.As(err, &se):
		writeStrict(w, se, http.StatusConflict)
	case err != nil:
		http.Error(w, "failed", http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

func (g *Gateway) generate(w http.ResponseWriter, r *http.Request) {
	var req modelport.ChatRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "unreadable", http.StatusBadRequest)
		return
	}
	rc, err := g.fake.Generate(r.Context(), req)
	var se *modelport.StrictError
	switch {
	case errors.As(err, &se):
		writeStrict(w, se, http.StatusConflict)
		return
	case err != nil:
		// The Gateway went away after the request arrived: the connection is cut with no
		// word of what the generation did.
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, herr := hj.Hijack(); herr == nil {
				_ = conn.Close()
				return
			}
		}
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	if !req.Stream {
		g.answer(w, rc)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// answer is the Gateway's non-stream answer to a request that asked for none (a compaction
// stage): one Chat completion with the text of the fake's final answer and the receipt it
// stated. Only an answer that ended well is made into one; anything else is a failure of the
// double that no test of a stage should depend on, and is not dressed up as an answer.
func (g *Gateway) answer(w http.ResponseWriter, rc io.Reader) {
	c := modelport.AssembleStrictStream(rc, modelport.AssembleOptions{})
	if c.TerminalKind != modelport.KindFinal || c.Receipt == nil {
		http.Error(w, "the fake did not answer with a final text", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": "fake-chat-1", "object": "chat.completion",
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": c.FinalText}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 10},
		"rencrow": map[string]any{"harness_receipt": c.Receipt},
	})
}

// writeStrict writes a strict error as the Gateway does: the closed StrictError body
// with the receipt, or the body a scripted reply carries verbatim.
func writeStrict(w http.ResponseWriter, se *modelport.StrictError, status int) {
	if len(se.Body) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(se.Body)
		return
	}
	body := map[string]any{
		"error":   map[string]any{"code": se.Code, "message": se.Message, "retryable": se.Retryable, "request_id": se.RequestID, "source_code": se.SourceCode},
		"rencrow": map[string]any{"harness_receipt": se.Receipt},
	}
	writeJSON(w, status, body)
}
