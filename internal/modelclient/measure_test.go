package modelclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

func TestMeasureSendsTheRequestAndReadsTheFourStates(t *testing.T) {
	req := actRequest(t)
	unverified := func(m map[string]any) {
		m["state"], m["prompt_lower"], m["prompt_upper"], m["effective_context_limit"] = "unverified", nil, nil, nil
		m["binding_fingerprint"], m["evidence_ref"], m["reason"] = "unverified", nil, "COUNT_HANDLER_NOT_CONFIGURED"
	}
	for _, tc := range []struct {
		name  string
		edit  func(m map[string]any)
		check func(t *testing.T, r modelport.MeasureResult)
	}{
		{"verified_exact", nil, func(t *testing.T, r modelport.MeasureResult) {
			if *r.PromptLower != 100 || *r.PromptUpper != 100 || *r.EffectiveContextLimit != 32768 || r.BindingFingerprint != testFingerprint {
				t.Fatalf("%+v", r)
			}
		}},
		{"verified_bound", func(m map[string]any) { m["state"], m["prompt_upper"] = "verified_bound", 130 }, func(t *testing.T, r modelport.MeasureResult) {
			if r.State != "verified_bound" || *r.PromptLower != 100 || *r.PromptUpper != 130 {
				t.Fatalf("%+v", r)
			}
		}},
		{"estimated", func(m map[string]any) {
			m["state"], m["prompt_upper"], m["reason"] = "estimated", 140, "COUNT_EVIDENCE_MISSING"
		}, func(t *testing.T, r modelport.MeasureResult) {
			if r.State != "estimated" || *r.Reason != "COUNT_EVIDENCE_MISSING" {
				t.Fatalf("%+v", r)
			}
		}},
		{"unverified keeps the Gateway's fixed fingerprint", unverified, func(t *testing.T, r modelport.MeasureResult) {
			if r.State != "unverified" || r.PromptLower != nil || r.PromptUpper != nil || r.BindingFingerprint != "unverified" || *r.Reason != "COUNT_HANDLER_NOT_CONFIGURED" {
				t.Fatalf("%+v", r)
			}
		}},
		{"unverified with a real fingerprint (handler failed)", func(m map[string]any) {
			unverified(m)
			m["binding_fingerprint"], m["reason"] = testFingerprint, "COUNT_HANDLER_UNREACHABLE"
		}, func(t *testing.T, r modelport.MeasureResult) {
			if r.State != "unverified" || r.BindingFingerprint != testFingerprint {
				t.Fatalf("%+v", r)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateway(t)
			g.reply(measureKey, 200, "application/json", mustJSON(t, measureWire(req, tc.edit)))
			got, err := g.client().Measure(t.Context(), measureRequest(req))
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, got)
			if g.count(measureKey) != 1 || g.requests() != 1 {
				t.Fatalf("%d requests, want exactly one POST /context/measure", g.requests())
			}
		})
	}

	// The wire form of the request.
	g := newGateway(t)
	g.reply(measureKey, 200, "application/json", mustJSON(t, measureWire(req, nil)))
	if _, err := g.client().Measure(t.Context(), measureRequest(req)); err != nil {
		t.Fatal(err)
	}
	h := g.header(measureKey, 0)
	if h.Get("Content-Type") != "application/json" || h.Get("Accept") != "application/json" {
		t.Errorf("headers: %v", h)
	}
	var sent map[string]any
	if err := json.Unmarshal(g.body(measureKey, 0), &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent["contract_version"] != "harness-v1" || sent["safety_margin_tokens"] != float64(256) {
		t.Fatalf("the envelope: %v", sent)
	}
	chat := sent["request"].(map[string]any)
	harness := chat["rencrow"].(map[string]any)["harness"].(map[string]any)
	if _, has := harness["expected_input_digest"]; has {
		t.Error("a first measure carries no expected_* values")
	}
	if harness["max_backend_attempts"] != float64(1) || harness["stage"] != "act" || chat["stream"] != true || chat["tools"] == nil || chat["stop"] == nil {
		t.Errorf("the chat request: %v", chat)
	}
}

func TestMeasureHoldsTheAnswerToTheRequest(t *testing.T) {
	req := actRequest(t)
	for _, tc := range []struct {
		name     string
		edit     func(m map[string]any)
		wantCode string
	}{
		{"another input digest", func(m map[string]any) { m["input_digest"] = testRequestDigest }, "INPUT_DIGEST_MISMATCH"},
		{"safety margin not echoed", func(m map[string]any) { m["safety_margin_tokens"] = 0 }, "MODEL_CONTRACT_FAILED"},
		{"another reserved output", func(m map[string]any) { m["reserved_output_tokens"] = 1 }, "MODEL_CONTRACT_FAILED"},
		{"extra field", func(m map[string]any) { m["extra"] = 1 }, "MODEL_CONTRACT_FAILED"},
		{"missing field", func(m map[string]any) { delete(m, "reason") }, "MODEL_CONTRACT_FAILED"},
		{"another contract version", func(m map[string]any) { m["contract_version"] = "harness-v2" }, "MODEL_CONTRACT_FAILED"},
		{"unknown state", func(m map[string]any) { m["state"] = "guessed" }, "MODEL_CONTRACT_FAILED"},
		{"bad digest", func(m map[string]any) { m["request_digest"] = "ABC" }, "MODEL_CONTRACT_FAILED"},
		{"exact is not one number", func(m map[string]any) { m["prompt_upper"] = 101 }, "MODEL_CONTRACT_FAILED"},
		{"bound is not a range", func(m map[string]any) { m["state"], m["prompt_lower"], m["prompt_upper"] = "verified_bound", 9, 3 }, "MODEL_CONTRACT_FAILED"},
		{"verified without a context limit", func(m map[string]any) { m["effective_context_limit"] = nil }, "MODEL_CONTRACT_FAILED"},
		{"verified for an unverified binding", func(m map[string]any) { m["binding_fingerprint"] = "unverified" }, "MODEL_CONTRACT_FAILED"},
		{"estimated with no count", func(m map[string]any) { m["state"], m["prompt_lower"], m["prompt_upper"] = "estimated", nil, nil }, "MODEL_CONTRACT_FAILED"},
		{"unverified with a count", func(m map[string]any) {
			m["state"], m["binding_fingerprint"], m["reason"] = "unverified", "unverified", "COUNT_HANDLER_STATUS"
		}, "MODEL_CONTRACT_FAILED"},
		{"unverified without a reason", func(m map[string]any) {
			m["state"], m["prompt_lower"], m["prompt_upper"] = "unverified", nil, nil
			m["binding_fingerprint"] = "unverified"
		}, "MODEL_CONTRACT_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateway(t)
			g.reply(measureKey, 200, "application/json", mustJSON(t, measureWire(req, tc.edit)))
			_, err := g.client().Measure(t.Context(), measureRequest(req))
			se := asStrict(t, err)
			if se.Code != tc.wantCode {
				t.Fatalf("code %s, want %s (%s)", se.Code, tc.wantCode, se.Message)
			}
			notLeaking(t, g, err.Error(), se.Message)
		})
	}

	t.Run("expected values the request carries are compared", func(t *testing.T) {
		with := req.WithExpected(inputDigestOf(t, req), testRequestDigest, testFingerprint)
		for name, edit := range map[string]func(m map[string]any){
			"request digest": func(m map[string]any) { m["request_digest"] = "cd" + testRequestDigest[2:] },
			"fingerprint":    func(m map[string]any) { m["binding_fingerprint"] = "bfp-v1:" + testRequestDigest },
		} {
			g := newGateway(t)
			g.reply(measureKey, 200, "application/json", mustJSON(t, measureWire(with, edit)))
			_, err := g.client().Measure(t.Context(), measureRequest(with))
			if se := asStrict(t, err); se.Code != "MODEL_CONTRACT_FAILED" {
				t.Fatalf("%s: %s", name, se.Code)
			}
		}
		g := newGateway(t)
		g.reply(measureKey, 200, "application/json", mustJSON(t, measureWire(with, nil)))
		if _, err := g.client().Measure(t.Context(), measureRequest(with)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a body that is not strict JSON", func(t *testing.T) {
		for _, body := range []string{"nope", `{"state":"verified_exact","state":"unverified"}`, ""} {
			g := newGateway(t)
			g.reply(measureKey, 200, "application/json", []byte(body))
			_, err := g.client().Measure(t.Context(), measureRequest(req))
			if se := asStrict(t, err); se.Code != "MODEL_CONTRACT_FAILED" {
				t.Fatalf("%q: %s", body, se.Code)
			}
		}
	})
}

func TestMeasureRefusesLocallyWithoutSending(t *testing.T) {
	req := actRequest(t)
	wrongDigest := req.WithExpected(testRequestDigest, testRequestDigest, testFingerprint)
	badChat := actRequest(t)
	badChat.MaxTokens = 0
	for _, tc := range []struct {
		name string
		mr   modelport.MeasureRequest
		code string
	}{
		{"another contract version", modelport.MeasureRequest{ContractVersion: "harness-v2", Request: req, SafetyMarginTokens: 1}, "UNSUPPORTED_CONTRACT"},
		{"a request that violates the schema", measureRequest(badChat), "UNSUPPORTED_CONTRACT"},
		{"an expected input digest that is not the request's", measureRequest(wrongDigest), "INPUT_DIGEST_MISMATCH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateway(t)
			_, err := g.client().Measure(t.Context(), tc.mr)
			se := asStrict(t, err)
			if se.Code != tc.code || se.SourceCode == nil || *se.SourceCode != SourceNotSent {
				t.Fatalf("%+v", se)
			}
			if g.requests() != 0 {
				t.Fatalf("a refused request was sent (%d)", g.requests())
			}
		})
	}
}

func TestMeasureRefusalsKeepTheGatewaysStatement(t *testing.T) {
	req := actRequest(t)
	exp := req.WithExpected(inputDigestOf(t, req), testRequestDigest, testFingerprint)
	for _, tc := range []struct {
		name   string
		status int
		body   []byte
		code   string
		state  string
	}{
		{"binding changed with the measured values", 409, strictBody(t, "BINDING_CHANGED", false, receiptWire("act", "not_started", 0, inputDigestOf(t, req), "cd"+testRequestDigest[2:], "bfp-v1:"+testRequestDigest)), "BINDING_CHANGED", "not_started"},
		{"budget unverified", 503, strictBody(t, "BUDGET_UNVERIFIED", false, receiptWire("act", "not_started", 0, inputDigestOf(t, req), nil, nil)), "BUDGET_UNVERIFIED", "not_started"},
		{"unsupported recovery profile", 400, strictBody(t, "UNSUPPORTED_RECOVERY_PROFILE", false, receiptWire("act", "not_started", 0, inputDigestOf(t, req), nil, nil)), "UNSUPPORTED_RECOVERY_PROFILE", "not_started"},
		{"a code the contract does not list", 400, strictBody(t, "INVALID_MESSAGE_LAYOUT", false, receiptWire("act", "not_started", 0, inputDigestOf(t, req), nil, nil)), "MODEL_CONTRACT_FAILED", "not_started"},
		{"no strict receipt at all", 400, []byte(`{"error":{"code":"INVALID_REQUEST","message":"bad","retryable":false}}`), "MODEL_CONTRACT_FAILED", "unknown"},
		{"a proxy page", 502, []byte("<html>bad gateway</html>"), "MODEL_CONTRACT_FAILED", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateway(t)
			g.reply(measureKey, tc.status, "application/json", tc.body)
			_, err := g.client().Measure(t.Context(), measureRequest(exp))
			se := asStrict(t, err)
			if se.Code != tc.code || se.GenerationState() != tc.state {
				t.Fatalf("%s/%s, want %s/%s", se.Code, se.GenerationState(), tc.code, tc.state)
			}
			if g.requests() != 1 {
				t.Fatalf("%d requests", g.requests())
			}
		})
	}
}

func TestMeasureTransportFailuresAreNotStrictErrors(t *testing.T) {
	req := actRequest(t)
	t.Run("deadline", func(t *testing.T) {
		g := newGateway(t)
		g.on(measureKey, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		_, err := g.client().Measure(ctx, measureRequest(req))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want a deadline: %v", err)
		}
		if g.count(measureKey) != 1 {
			t.Fatalf("%d requests", g.count(measureKey))
		}
	})
	t.Run("connection closed without an answer", func(t *testing.T) {
		g := newGateway(t)
		g.on(measureKey, closeConnection)
		_, err := g.client().Measure(t.Context(), measureRequest(req))
		var te *TransportError
		if !errors.As(err, &te) || !te.Connected {
			t.Fatalf("want a transport error after a connection: %v", err)
		}
		if g.count(measureKey) != 1 {
			t.Fatalf("the failed request was sent %d times", g.count(measureKey))
		}
		notLeaking(t, g, err.Error())
	})
}
