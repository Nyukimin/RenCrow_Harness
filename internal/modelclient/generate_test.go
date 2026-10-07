package modelclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

// fakeGatewayRequest is an act request consistent with the Fake model's own
// digests: counted by the Fake, so that the Fake accepts it as a generation.
func fakeGatewayRequest(t testing.TB, f *harnesstest.Fake) modelport.ChatRequest {
	t.Helper()
	req := actRequest(t)
	res, err := f.Measure(context.Background(), measureRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	return req.WithExpected(res.InputDigest, res.RequestDigest, res.BindingFingerprint)
}

func strictErrorWireOf(t testing.TB, se *modelport.StrictError) []byte {
	return mustJSON(t, map[string]any{
		"error":   map[string]any{"code": se.Code, "message": se.Message, "retryable": se.Retryable, "request_id": se.RequestID, "source_code": se.SourceCode},
		"rencrow": map[string]any{"harness_receipt": se.Receipt},
	})
}

// fakeGenerate serves POST /chat/completions with the Fake model: the strict
// stream it scripts, or the strict error it refuses with, in the Gateway's forms.
func fakeGenerate(t testing.TB, f *harnesstest.Fake) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req modelport.ChatRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "unreadable", http.StatusBadRequest)
			return
		}
		rc, err := f.Generate(r.Context(), req)
		var se *modelport.StrictError
		if errors.As(err, &se) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write(strictErrorWireOf(t, se))
			return
		}
		if err != nil {
			http.Error(w, "failed", http.StatusInternalServerError)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, rc)
	}
}

func TestGenerateReturnsTheStrictStreamUnreadAndItAssemblesAsStated(t *testing.T) {
	one := int64(1)
	for _, tc := range []struct {
		name      string
		reply     harnesstest.Reply
		wantKind  string
		wantCode  string
		wantState string
		hidden    bool
		terminal  bool
		done      bool
	}{
		{"final", harnesstest.Final("hello こんにちは"), modelport.KindFinal, "", "terminal", false, true, true},
		{"tool call", harnesstest.Reply{Kind: harnesstest.KindToolCall, Name: "file.read", Args: `{"path":"a"}`}, modelport.KindToolCalls, "", "terminal", false, true, true},
		{"length", harnesstest.Reply{Kind: harnesstest.KindLength, Text: "cut"}, modelport.KindIncomplete, "LENGTH", "terminal", false, true, true},
		{"refusal", harnesstest.Reply{Kind: harnesstest.KindRefusal}, modelport.KindRefused, "REFUSED", "terminal", false, true, true},
		{"reasoning only", harnesstest.Reply{Kind: harnesstest.KindReasoningOnly, Reasoning: "thinking"}, modelport.KindError, "REASONING_ONLY", "terminal", false, true, true},
		{"error outcome", harnesstest.Reply{Kind: harnesstest.KindErrorOutcome, Code: "UPSTREAM_TRANSIENT", Text: "x"}, modelport.KindError, "UPSTREAM_TRANSIENT", "terminal", false, true, true},
		{"EOF before the terminal frame", harnesstest.Reply{Kind: harnesstest.KindNoTerminal, Text: "partial"}, modelport.KindError, "MODEL_GENERATION_OUTCOME_UNKNOWN", "unknown", false, false, false},
		{"[DONE] without a terminal frame", harnesstest.Reply{Kind: harnesstest.KindDoneOnly, Text: "x"}, modelport.KindError, "MODEL_CONTRACT_FAILED", "unknown", false, false, true},
		{"hidden retry in the terminal", harnesstest.Reply{Kind: harnesstest.KindFinal, Text: "x", Mutate: func(m map[string]any) {
			m["harness_receipt"].(map[string]any)["hidden_retry"] = true
		}}, modelport.KindError, "MODEL_CONTRACT_FAILED", "unknown", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := harnesstest.NewFake()
			f.SetReply(tc.reply)
			g := newGateway(t)
			g.on(generateKey, fakeGenerate(t, f))
			req := fakeGatewayRequest(t, f)

			rc, err := g.client().Generate(t.Context(), req)
			if err != nil {
				t.Fatalf("the response is a stream and is returned as one: %v", err)
			}
			c := assemble(t, rc)
			if c.TerminalKind != tc.wantKind || c.FailureCode != tc.wantCode || c.GenerationState != tc.wantState ||
				c.HiddenRetry != tc.hidden || c.TerminalObserved != tc.terminal || c.DoneObserved != tc.done {
				t.Fatalf("completion %+v", c)
			}
			if tc.wantState == "terminal" && (c.BackendAttempts == nil || *c.BackendAttempts != one) {
				t.Fatalf("a terminal state carries one backend attempt: %v", c.BackendAttempts)
			}
			if tc.wantState == "unknown" && c.BackendAttempts != nil {
				t.Fatalf("an unknown state counts no attempt: %v", *c.BackendAttempts)
			}
			if g.requests() != 1 || g.count(generateKey) != 1 {
				t.Fatalf("%d HTTP requests for one Generate, want 1", g.requests())
			}
		})
	}
}

func TestGenerateStreamReplaysTheDesignsRawFrames(t *testing.T) {
	req := decodeRequest(t, "stream_request.json")
	fixture := string(wireFixture(t, "act_tool_stream.sse"))
	withoutDone := strings.TrimSuffix(fixture, "data: [DONE]\n\n")
	terminalOnly := withoutDone[strings.Index(withoutDone, "event: rencrow.terminal"):]
	cutInsideTerminal := strings.TrimSuffix(withoutDone, "\n\n")

	for _, tc := range []struct {
		name      string
		body      string
		wantKind  string
		wantCode  string
		wantState string
		terminal  bool
		done      bool
	}{
		{"the fixture", fixture, modelport.KindToolCalls, "", "terminal", true, true},
		{"CRLF line ends", strings.ReplaceAll(fixture, "\n", "\r\n"), modelport.KindToolCalls, "", "terminal", true, true},
		{"terminal seen, [DONE] missing", withoutDone, modelport.KindIncomplete, "INCOMPLETE", "terminal", true, false},
		{"cut inside the terminal frame", cutInsideTerminal, modelport.KindError, "MODEL_GENERATION_OUTCOME_UNKNOWN", "unknown", false, false},
		{"EOF right after the first chunk", fixture[:strings.Index(fixture, "\n\n")+2], modelport.KindError, "MODEL_GENERATION_OUTCOME_UNKNOWN", "unknown", false, false},
		{"nothing at all", "", modelport.KindError, "MODEL_GENERATION_OUTCOME_UNKNOWN", "unknown", false, false},
		{"a terminal frame with no chunks", terminalOnly + "data: [DONE]\n\n", modelport.KindError, "MODEL_CONTRACT_FAILED", "terminal", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateway(t)
			g.on(generateKey, sseHandler(tc.body))
			rc, err := g.client().Generate(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			c := assemble(t, rc)
			if c.TerminalKind != tc.wantKind || c.FailureCode != tc.wantCode || c.GenerationState != tc.wantState || c.TerminalObserved != tc.terminal || c.DoneObserved != tc.done {
				t.Fatalf("completion kind=%s code=%s state=%s terminal=%v done=%v (%s)", c.TerminalKind, c.FailureCode, c.GenerationState, c.TerminalObserved, c.DoneObserved, c.Violation)
			}
		})
	}
}

func TestGenerateSendsTheStrictRequestOnce(t *testing.T) {
	req := genRequest(t, actRequest(t))
	g := newGateway(t)
	g.on(generateKey, sseHandler("data: [DONE]\n\n"))
	rc, err := g.client().Generate(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = assemble(t, rc)
	h := g.header(generateKey, 0)
	if h.Get("Content-Type") != "application/json" || h.Get("Accept") != "text/event-stream" || h.Get("Connection") != "close" {
		t.Errorf("headers: %v", h)
	}
	body := g.body(generateKey, 0)

	var sent modelport.ChatRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sent); err != nil {
		t.Fatalf("the body is not a ChatRequest of the contract: %v", err)
	}
	if err := sent.Validate(true); err != nil {
		t.Fatalf("the body is not a GenerationRequest: %v", err)
	}
	want, _ := req.Canonical()
	got, _ := sent.Canonical()
	if string(got) != string(want) {
		t.Fatalf("the body is not the request\n got %s\nwant %s", got, want)
	}
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	ext := raw["rencrow"].(map[string]any)["harness"].(map[string]any)
	if ext["max_backend_attempts"] != float64(1) || ext["expected_request_digest"] != testRequestDigest || ext["expected_binding_fingerprint"] != testFingerprint ||
		raw["stream"] != true || raw["stream_options"].(map[string]any)["include_usage"] != true || raw["tools"] == nil || raw["stop"] == nil ||
		ext["recovery"].(map[string]any)["profile_id"] != "same_request" {
		t.Fatalf("the harness extension or the stream options are wrong: %s", body)
	}
}

func TestGenerateRefusalsCarryWhatTheGatewayStated(t *testing.T) {
	// The Fake model refuses before generating with a receipt of its own.
	for _, tc := range []struct {
		name   string
		edit   func(*modelport.ChatRequest)
		fake   func(*harnesstest.Fake)
		reply  *harnesstest.Reply
		code   string
		source string
	}{
		{"binding changed", nil, func(f *harnesstest.Fake) { f.Fingerprint = "bfp-v1:" + strings.Repeat("d", 64) }, nil, "BINDING_CHANGED", ""},
		{"request digest mismatch", func(r *modelport.ChatRequest) { r.Rencrow.Harness.ExpectedRequestDigest = testRequestDigest }, nil, nil, "REQUEST_DIGEST_MISMATCH", ""},
		{"scripted code", nil, nil, &harnesstest.Reply{Kind: harnesstest.KindStrictError, Code: "BUDGET_UNVERIFIED"}, "BUDGET_UNVERIFIED", ""},
		{"scripted queue timeout", nil, nil, &harnesstest.Reply{Kind: harnesstest.KindStrictError, Code: "QUEUE_TIMEOUT"}, "QUEUE_TIMEOUT", ""},
		{"scripted unlisted code", nil, nil, &harnesstest.Reply{Kind: harnesstest.KindStrictError, Code: "MODEL_IDENTITY_MISMATCH"}, "MODEL_CONTRACT_FAILED", "MODEL_IDENTITY_MISMATCH"},
		{"scripted refusal without a receipt", nil, nil, &harnesstest.Reply{Kind: harnesstest.KindStrictError, Code: "UNSUPPORTED_CONTRACT", NoReceipt: true}, "MODEL_CONTRACT_FAILED", "UNSUPPORTED_CONTRACT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := harnesstest.NewFake()
			if tc.reply != nil {
				f.SetReply(*tc.reply)
			}
			req := fakeGatewayRequest(t, f)
			if tc.edit != nil {
				tc.edit(&req)
			}
			if tc.fake != nil {
				tc.fake(f)
			}
			g := newGateway(t)
			g.on(generateKey, fakeGenerate(t, f))
			rc, err := g.client().Generate(t.Context(), req)
			if rc != nil {
				t.Fatal("a refusal is not a stream")
			}
			se := asStrict(t, err)
			if se.Code != tc.code {
				t.Fatalf("code %s, want %s (%s)", se.Code, tc.code, se.Message)
			}
			if tc.source != "" && (se.SourceCode == nil || *se.SourceCode != tc.source) {
				t.Fatalf("source %v, want %s", se.SourceCode, tc.source)
			}
			if tc.reply != nil && tc.reply.NoReceipt {
				if se.Receipt != nil || se.GenerationState() != "unknown" {
					t.Fatalf("no receipt means an unknown generation: %+v", se)
				}
			} else if se.Receipt == nil || se.GenerationState() != "not_started" || se.Receipt.BackendAttempts == nil || *se.Receipt.BackendAttempts != 0 {
				t.Fatalf("the Gateway's not_started receipt was not kept: %+v", se.Receipt)
			}
			if g.requests() != 1 {
				t.Fatalf("%d requests", g.requests())
			}
			notLeaking(t, g, err.Error(), se.Message)
		})
	}
}

func TestGenerateHoldsEveryStatementToTheRequest(t *testing.T) {
	req := genRequest(t, actRequest(t))
	h := req.Rencrow.Harness
	in, rq, fp := h.ExpectedInputDigest, h.ExpectedRequestDigest, h.ExpectedBindingFingerprint
	other := "cd" + testRequestDigest[2:]
	body := func(code string, retryable bool, receipt map[string]any) []byte {
		return strictBody(t, code, retryable, receipt)
	}
	ns := func(input, request, binding any) map[string]any {
		return receiptWire("act", "not_started", 0, input, request, binding)
	}
	term := func(input, request, binding any) map[string]any {
		return receiptWire("act", "terminal", 1, input, request, binding)
	}
	edit := func(m map[string]any, k string, v any) map[string]any { m[k] = v; return m }

	type want struct {
		code      string
		state     string
		receipt   bool
		retryable bool
		source    string
		hidden    bool
	}
	for _, tc := range []struct {
		name   string
		status int
		body   []byte
		want
	}{
		{"queue timeout is retryable before generation", 504, body("QUEUE_TIMEOUT", true, ns(in, nil, nil)), want{"QUEUE_TIMEOUT", "not_started", true, true, "", false}},
		{"a retry hint on a code that cannot retry is dropped", 409, body("BINDING_CHANGED", true, ns(in, other, "bfp-v1:"+other)), want{"BINDING_CHANGED", "not_started", true, false, "", false}},
		{"a retry hint on an unknown generation is dropped", 502, body("RAW_TOOL_MARKUP", true, receiptWire("act", "unknown", nil, in, rq, fp)), want{"RAW_TOOL_MARKUP", "unknown", true, false, "", false}},
		{"upstream transient after a finished attempt", 502, body("UPSTREAM_TRANSIENT", true, term(in, rq, fp)), want{"UPSTREAM_TRANSIENT", "terminal", true, true, "", false}},
		{"empty final content", 502, body("EMPTY_FINAL_CONTENT", true, term(in, rq, fp)), want{"EMPTY_FINAL_CONTENT", "terminal", true, true, "", false}},
		{"length", 502, body("LENGTH", false, term(in, rq, fp)), want{"LENGTH", "terminal", true, false, "", false}},
		{"context limit before generation", 400, body("CONTEXT_LIMIT_EXCEEDED", false, ns(in, rq, fp)), want{"CONTEXT_LIMIT_EXCEEDED", "not_started", true, false, "", false}},
		{"outcome unknown", 502, body("MODEL_GENERATION_OUTCOME_UNKNOWN", false, receiptWire("act", "unknown", nil, in, nil, nil)), want{"MODEL_GENERATION_OUTCOME_UNKNOWN", "unknown", true, false, "", false}},
		{"binding changed states the measured fingerprint", 409, body("BINDING_CHANGED", false, ns(in, nil, "bfp-v1:"+other)), want{"BINDING_CHANGED", "not_started", true, false, "", false}},
		{"request digest mismatch states the measured digest", 409, body("REQUEST_DIGEST_MISMATCH", false, ns(in, other, fp)), want{"REQUEST_DIGEST_MISMATCH", "not_started", true, false, "", false}},
		{"input digest mismatch states the recomputed digest", 400, body("INPUT_DIGEST_MISMATCH", false, ns(other, nil, nil)), want{"INPUT_DIGEST_MISMATCH", "not_started", true, false, "", false}},

		{"a digest that is not the expected one under another code", 400, body("CONTEXT_LIMIT_EXCEEDED", false, ns(in, other, fp)), want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "CONTEXT_LIMIT_EXCEEDED", false}},
		{"another input digest under another code", 400, body("UNSUPPORTED_CONTRACT", false, ns(other, nil, nil)), want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "UNSUPPORTED_CONTRACT", false}},
		{"a state its code cannot have", 409, body("BINDING_CHANGED", false, term(in, rq, fp)), want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "BINDING_CHANGED", false}},
		{"queue timeout that claims a finished generation", 504, body("QUEUE_TIMEOUT", true, term(in, rq, fp)), want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "QUEUE_TIMEOUT", false}},
		{"a receipt for another stage", 400, body("BUDGET_UNVERIFIED", false, edit(ns(in, nil, nil), "stage", "work_summary")), want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "BUDGET_UNVERIFIED", false}},
		{"a receipt for another recovery profile", 400, body("BUDGET_UNVERIFIED", false, edit(ns(in, nil, nil), "recovery_profile_revision", "builtin-v9")), want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "BUDGET_UNVERIFIED", false}},
		{"a receipt that counts two logical requests", 400, body("BUDGET_UNVERIFIED", false, edit(ns(in, nil, nil), "logical_requests", 2)), want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "BUDGET_UNVERIFIED", false}},
		{"a receipt of a finished generation with no attempt", 502, body("EMPTY_FINAL_CONTENT", false, edit(term(in, rq, fp), "backend_attempts", 0)), want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "EMPTY_FINAL_CONTENT", false}},
		{"another contract version in the receipt", 400, body("BUDGET_UNVERIFIED", false, edit(ns(in, nil, nil), "contract_version", "harness-v2")), want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "BUDGET_UNVERIFIED", false}},
		{"a receipt that misses a field", 400, body("BUDGET_UNVERIFIED", false, func() map[string]any { m := ns(in, nil, nil); delete(m, "usage_complete"); return m }()), want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "BUDGET_UNVERIFIED", false}},
		{"a refusal for another request", 400, mustJSON(t, map[string]any{
			"error":   map[string]any{"code": "BUDGET_UNVERIFIED", "message": "m", "retryable": false, "request_id": "req_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b00ff", "source_code": nil},
			"rencrow": map[string]any{"harness_receipt": ns(in, nil, nil)},
		}), want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "BUDGET_UNVERIFIED", false}},
		{"an undeclared member", 400, mustJSON(t, map[string]any{
			"error":   map[string]any{"code": "BUDGET_UNVERIFIED", "message": "m", "retryable": false, "request_id": nil, "source_code": nil},
			"rencrow": map[string]any{"harness_receipt": ns(in, nil, nil)}, "debug": "x",
		}), want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "BUDGET_UNVERIFIED", false}},

		{"a code the Harness does not map keeps the known state", 400, body("MODEL_IDENTITY_MISMATCH", false, ns(in, nil, nil)), want{"MODEL_CONTRACT_FAILED", "not_started", true, false, "MODEL_IDENTITY_MISMATCH", false}},
		{"INVALID_MESSAGE_LAYOUT is not guessed at", 400, body("INVALID_MESSAGE_LAYOUT", false, ns(in, nil, nil)), want{"MODEL_CONTRACT_FAILED", "not_started", true, false, "INVALID_MESSAGE_LAYOUT", false}},
		{"a Harness-only code from the Gateway", 400, body("CANCELLED", false, ns(in, nil, nil)), want{"MODEL_CONTRACT_FAILED", "not_started", true, false, "CANCELLED", false}},
		{"a generic refusal has no strict receipt", 400, []byte(`{"error":{"code":"INVALID_REQUEST","message":"bad","retryable":false}}`), want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "INVALID_REQUEST", false}},
		{"a refusal with a receipt-less retryable code is not not_started", 503, []byte(`{"error":{"code":"CONNECT_FAILED","message":"x","retryable":true,"request_id":null,"source_code":null}}`), want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "CONNECT_FAILED", false}},
		{"auth refusal without a receipt", 401, nil, want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "HTTP_401", false}},
		{"a proxy page", 502, []byte("<html>502</html>"), want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "HTTP_502", false}},
		{"rate limited without a body", 429, nil, want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "HTTP_429", false}},
		{"a refusal over the size limit", 400, []byte(strings.Repeat(" ", maxFailureBytes+10)), want{"MODEL_CONTRACT_FAILED", "unknown", false, false, "", false}},

		{"hidden retry on a finished attempt", 502, body("UPSTREAM_TRANSIENT", false, edit(term(in, rq, fp), "hidden_retry", true)), want{"MODEL_CONTRACT_FAILED", "unknown", true, false, "UPSTREAM_TRANSIENT", true}},
		{"hidden retry on a receipt that is otherwise broken", 502, body("UPSTREAM_TRANSIENT", false, edit(edit(term(in, rq, fp), "hidden_retry", true), "stage", 7)), want{"MODEL_CONTRACT_FAILED", "unknown", true, false, "UPSTREAM_TRANSIENT", true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateway(t)
			g.reply(generateKey, tc.status, "application/json", tc.body)
			rc, err := g.client().Generate(t.Context(), req)
			if rc != nil {
				t.Fatal("a refusal is not a stream")
			}
			se := asStrict(t, err)
			if se.Code != tc.code || se.GenerationState() != tc.state || (se.Receipt != nil) != tc.receipt || se.Retryable != tc.retryable {
				t.Fatalf("got code=%s state=%s receipt=%v retryable=%v (%s); want %+v", se.Code, se.GenerationState(), se.Receipt != nil, se.Retryable, se.Message, tc.want)
			}
			if tc.source == "" && se.SourceCode != nil {
				t.Fatalf("source %v, want none", *se.SourceCode)
			}
			if tc.source != "" && (se.SourceCode == nil || *se.SourceCode != tc.source) {
				t.Fatalf("source %v, want %s", se.SourceCode, tc.source)
			}
			if tc.hidden {
				r := se.Receipt
				if !r.HiddenRetry || r.BackendAttempts != nil || r.GenerationState != "unknown" {
					t.Fatalf("a hidden retry is carried as hidden_retry with an unknown count: %+v", r)
				}
			}
			if tc.receipt && !tc.hidden {
				// Whatever the Gateway stated about the generation is what is returned.
				if tc.state == "not_started" && *se.Receipt.BackendAttempts != 0 || tc.state == "terminal" && *se.Receipt.BackendAttempts != 1 {
					t.Fatalf("attempts %v", *se.Receipt.BackendAttempts)
				}
			}
			if g.requests() != 1 {
				t.Fatalf("%d requests", g.requests())
			}
			notLeaking(t, g, err.Error(), se.Message)
		})
	}
}

func TestGenerateRefusesLocallyAndSendsNothing(t *testing.T) {
	complete := genRequest(t, actRequest(t))
	noExpected := actRequest(t)
	wrongInput := actRequest(t).WithExpected(testRequestDigest, testRequestDigest, testFingerprint)
	badRecovery := complete
	badRecovery.Rencrow.Harness.Recovery.ProfileID = "no_such_profile"
	manyAttempts := complete
	manyAttempts.Rencrow.Harness.MaxBackendAttempts = 2
	noMax := complete
	noMax.MaxTokens = 0
	for _, tc := range []struct {
		name string
		req  modelport.ChatRequest
		code string
	}{
		{"no expected values", noExpected, "UNSUPPORTED_CONTRACT"},
		{"an expected input digest that is not the request's", wrongInput, "INPUT_DIGEST_MISMATCH"},
		{"a recovery profile the contract does not know", badRecovery, "UNSUPPORTED_CONTRACT"},
		{"two backend attempts", manyAttempts, "UNSUPPORTED_CONTRACT"},
		{"no output budget", noMax, "UNSUPPORTED_CONTRACT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateway(t)
			rc, err := g.client().Generate(t.Context(), tc.req)
			se := asStrict(t, err)
			if rc != nil || se.Code != tc.code || se.SourceCode == nil || *se.SourceCode != SourceNotSent {
				t.Fatalf("%+v", se)
			}
			// Nothing was sent, which the client knows: a generation that never started.
			r := se.Receipt
			if r == nil || r.GenerationState != "not_started" || r.BackendAttempts == nil || *r.BackendAttempts != 0 ||
				r.BindingFingerprint != nil || r.RequestDigest != nil || r.InputDigest != nil || r.HiddenRetry || r.LogicalRequests != 1 {
				t.Fatalf("the local receipt states what was not measured: %+v", r)
			}
			if g.requests() != 0 {
				t.Fatalf("a refused request was sent (%d)", g.requests())
			}
			notLeaking(t, g, err.Error(), se.Message)
		})
	}
}

func TestGenerateWithNoConnectionIsAConnectFailureNotStarted(t *testing.T) {
	req := genRequest(t, actRequest(t))
	g := newGateway(t)
	c := g.client()
	g.srv.Close() // nothing listens any more: no connection can be made

	rc, err := c.Generate(t.Context(), req)
	if rc != nil {
		t.Fatal("no stream")
	}
	se := asStrict(t, err)
	if se.Code != "CONNECT_FAILED" || !se.Retryable || se.SourceCode == nil || *se.SourceCode != SourceNotConnected {
		t.Fatalf("%+v", se)
	}
	r := se.Receipt
	if r == nil || r.GenerationState != "not_started" || r.BackendAttempts == nil || *r.BackendAttempts != 0 || r.BindingFingerprint != nil || r.RequestDigest != nil || r.InputDigest != nil {
		t.Fatalf("the client's own receipt: %+v", r)
	}
	notLeaking(t, g, err.Error(), se.Message)
}

func TestGenerateAfterAConnectionAnythingThatGoesWrongIsUnknown(t *testing.T) {
	req := genRequest(t, actRequest(t))
	t.Run("the connection closes without an answer", func(t *testing.T) {
		g := newGateway(t)
		g.on(generateKey, closeConnection)
		rc, err := g.client().Generate(t.Context(), req)
		var te *TransportError
		if rc != nil || !errors.As(err, &te) || !te.Connected || stateOf(err) != "unknown" {
			t.Fatalf("%v", err)
		}
		if g.count(generateKey) != 1 {
			t.Fatalf("the request was sent %d times: nothing may send it again", g.count(generateKey))
		}
		notLeaking(t, g, err.Error())
	})
	t.Run("the deadline ends while the Gateway is silent", func(t *testing.T) {
		g := newGateway(t)
		g.on(generateKey, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
		defer cancel()
		_, err := g.client().Generate(ctx, req)
		if !errors.Is(err, context.DeadlineExceeded) || stateOf(err) != "unknown" {
			t.Fatalf("%v", err)
		}
		if g.count(generateKey) != 1 {
			t.Fatalf("%d requests", g.count(generateKey))
		}
	})
	t.Run("the caller cancels while the Gateway is silent", func(t *testing.T) {
		g := newGateway(t)
		g.on(generateKey, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			for g.count(generateKey) == 0 {
				time.Sleep(time.Millisecond)
			}
			cancel()
		}()
		_, err := g.client().Generate(ctx, req)
		if !errors.Is(err, context.Canceled) || stateOf(err) != "unknown" {
			t.Fatalf("%v", err)
		}
		var se *modelport.StrictError
		if errors.As(err, &se) {
			t.Fatal("a cancellation is not a refusal of the Gateway")
		}
	})
	t.Run("a stream request answered with JSON", func(t *testing.T) {
		g := newGateway(t)
		g.reply(generateKey, 200, "application/json", []byte(`{"choices":[]}`))
		rc, err := g.client().Generate(t.Context(), req)
		se := asStrict(t, err)
		if rc != nil || se.Code != "MODEL_CONTRACT_FAILED" || se.Receipt != nil || stateOf(err) != "unknown" {
			t.Fatalf("%+v", se)
		}
	})
	t.Run("a stream request answered with no content type", func(t *testing.T) {
		g := newGateway(t)
		g.on(generateKey, func(w http.ResponseWriter, _ *http.Request) {
			w.Header()["Content-Type"] = nil
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		})
		rc, err := g.client().Generate(t.Context(), req)
		if rc != nil || asStrict(t, err).Code != "MODEL_CONTRACT_FAILED" {
			t.Fatalf("%v", err)
		}
	})
}

func TestCancellingTheContextStopsTheStreamAndLeavesTheGenerationUnknown(t *testing.T) {
	req := genRequest(t, actRequest(t))
	g := newGateway(t)
	g.on(generateKey, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rc, err := g.client().Generate(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	c := assemble(t, rc)
	if c.TerminalKind != modelport.KindError || c.FailureCode != "MODEL_GENERATION_OUTCOME_UNKNOWN" || c.GenerationState != "unknown" || c.TerminalObserved {
		t.Fatalf("a stopped stream is an unknown generation: %+v", c)
	}
	if g.count(generateKey) != 1 {
		t.Fatalf("%d requests", g.count(generateKey))
	}
}
