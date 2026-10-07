package modelclient

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

func completionWire(t testing.TB, req modelport.ChatRequest, message map[string]any, finish string) map[string]any {
	return map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion", "created": 1, "model": "fixture-local--host",
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		"rencrow": map[string]any{"harness_receipt": terminalReceipt(t, req)},
	}
}

func expectedOf(req modelport.ChatRequest) modelport.Expected {
	h := req.Rencrow.Harness
	return modelport.Expected{
		Stage: h.Stage, InputDigest: h.ExpectedInputDigest, RequestDigest: h.ExpectedRequestDigest, BindingFingerprint: h.ExpectedBindingFingerprint,
		ProfileID: h.Recovery.ProfileID, ProfileRevision: h.Recovery.ProfileRevision,
	}
}

func TestNonStreamStagesAreReadThroughTheSameAssembler(t *testing.T) {
	for _, stage := range []string{"instruction_selection", "work_summary"} {
		t.Run(stage, func(t *testing.T) {
			req := stageRequest(t, stage)
			body := mustJSON(t, completionWire(t, req, map[string]any{"role": "assistant", "content": `{"selected":["a","b"]}`}, "stop"))
			g := newGateway(t)
			g.reply(generateKey, 200, "application/json", body)

			rc, err := g.client().Generate(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if raw, ok := rc.(interface{ RawBody() []byte }); !ok || string(raw.RawBody()) != string(body) {
				t.Fatal("the stream made of the answer does not give the answer back byte for byte")
			}
			c := assemble(t, rc)
			if c.TerminalKind != modelport.KindFinal || c.FinalText != `{"selected":["a","b"]}` || !c.TerminalObserved || !c.DoneObserved {
				t.Fatalf("%+v", c)
			}
			if c.GenerationState != "terminal" || c.BackendAttempts == nil || *c.BackendAttempts != 1 || c.ProviderResponseID != "chatcmpl-1" {
				t.Fatalf("%+v", c)
			}
			if c.Usage == nil || *c.Usage.PromptTokens != 10 || *c.Usage.CompletionTokens != 5 || c.Usage.CachedTokens != nil {
				t.Fatalf("usage %+v", c.Usage)
			}
			if err := modelport.VerifyReceipt(*c.Receipt, expectedOf(req)); err != nil {
				t.Fatal(err)
			}
			h := g.header(generateKey, 0)
			if h.Get("Accept") != "application/json" {
				t.Errorf("Accept %q", h.Get("Accept"))
			}
			sent := string(g.body(generateKey, 0))
			for _, want := range []string{`"stream":false`, `"stream_options":null`, `"tool_choice":"none"`, `"response_format":{"type":"json_object"}`, `"stage":"` + stage + `"`} {
				if !strings.Contains(sent, want) {
					t.Errorf("the request lacks %s: %s", want, sent)
				}
			}
			if g.requests() != 1 {
				t.Fatalf("%d requests", g.requests())
			}
		})
	}
}

func TestNonStreamActAnswerWithToolCallsAndReasoning(t *testing.T) {
	req := decodeRequest(t, "stream_request.json")
	req.Stream, req.StreamOptions = false, nil
	req = req.WithExpected(inputDigestOf(t, req), testRequestDigest, testFingerprint)
	args := `{"path":"demo.txt"}`
	msg := map[string]any{"role": "assistant", "content": nil, "reasoning_content": "hmm", "tool_calls": []any{
		map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "file.read", "arguments": args}},
	}}
	g := newGateway(t)
	g.reply(generateKey, 200, "application/json", mustJSON(t, completionWire(t, req, msg, "tool_calls")))
	rc, err := g.client().Generate(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	c := assemble(t, rc)
	if c.TerminalKind != modelport.KindToolCalls || len(c.ToolIntents) != 1 || c.ToolIntents[0].Name != "file.read" ||
		c.ToolIntents[0].ArgumentsJSON != args || c.ToolIntents[0].ProviderToolCallID != "call-1" || c.ReasoningBytes != 3 {
		t.Fatalf("%+v", c)
	}
}

func TestNonStreamLargeAnswersSurviveTheFragmentation(t *testing.T) {
	req := stageRequest(t, "work_summary")
	text := strings.Repeat("あ\"\\\n<&>é", 20000) // about 200 KB of escaped, multi-byte text
	if len(text) <= 4*pieceBytes {
		t.Fatal("the test text must need several fragments")
	}
	g := newGateway(t)
	g.reply(generateKey, 200, "application/json", mustJSON(t, completionWire(t, req, map[string]any{"role": "assistant", "content": text}, "stop")))
	rc, err := g.client().Generate(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if c := assemble(t, rc); c.TerminalKind != modelport.KindFinal || c.FinalText != text {
		t.Fatalf("the text was changed in the stream: kind=%s len=%d want %d (%s)", c.TerminalKind, len(c.FinalText), len(text), c.Violation)
	}

	act := decodeRequest(t, "stream_request.json")
	act.Stream, act.StreamOptions = false, nil
	act = act.WithExpected(inputDigestOf(t, act), testRequestDigest, testFingerprint)
	args := `{"text":"` + strings.Repeat("あ", 30000) + `"}`
	msg := map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
		map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "file.write", "arguments": args}},
		map[string]any{"id": "call-2", "type": "function", "function": map[string]any{"name": "file.read", "arguments": "{}"}},
	}}
	g2 := newGateway(t)
	g2.reply(generateKey, 200, "application/json", mustJSON(t, completionWire(t, act, msg, "tool_calls")))
	rc, err = g2.client().Generate(t.Context(), act)
	if err != nil {
		t.Fatal(err)
	}
	c := assemble(t, rc)
	if c.TerminalKind != modelport.KindToolCalls || len(c.ToolIntents) != 2 || c.ToolIntents[0].ArgumentsJSON != args || c.ToolIntents[1].Ordinal != 1 {
		t.Fatalf("kind=%s intents=%d (%s)", c.TerminalKind, len(c.ToolIntents), c.Violation)
	}
}

func TestNonStreamAnswersThatBreakTheContractAreNotAdopted(t *testing.T) {
	req := stageRequest(t, "instruction_selection")
	msg := map[string]any{"role": "assistant", "content": "{}"}
	good := func() map[string]any { return completionWire(t, req, msg, "stop") }
	receiptOf := func(m map[string]any) map[string]any {
		return m["rencrow"].(map[string]any)["harness_receipt"].(map[string]any)
	}
	withReceipt := func(edit func(r map[string]any)) map[string]any {
		m := good()
		edit(receiptOf(m))
		return m
	}
	type result struct {
		code    string
		state   string
		receipt bool
		hidden  bool
	}
	for _, tc := range []struct {
		name string
		body []byte
		result
	}{
		{"hidden retry", mustJSON(t, withReceipt(func(r map[string]any) { r["hidden_retry"] = true })), result{"MODEL_CONTRACT_FAILED", "unknown", true, true}},
		{"a receipt for another stage", mustJSON(t, withReceipt(func(r map[string]any) { r["stage"] = "work_summary" })), result{"MODEL_CONTRACT_FAILED", "unknown", false, false}},
		{"another input digest", mustJSON(t, withReceipt(func(r map[string]any) { r["input_digest"] = testRequestDigest })), result{"MODEL_CONTRACT_FAILED", "unknown", false, false}},
		{"another request digest", mustJSON(t, withReceipt(func(r map[string]any) { r["request_digest"] = "cd" + testRequestDigest[2:] })), result{"MODEL_CONTRACT_FAILED", "unknown", false, false}},
		{"another fingerprint", mustJSON(t, withReceipt(func(r map[string]any) { r["binding_fingerprint"] = "bfp-v1:x" })), result{"MODEL_CONTRACT_FAILED", "unknown", false, false}},
		{"a generation that did not finish", mustJSON(t, withReceipt(func(r map[string]any) {
			r["generation_state"], r["backend_attempts"], r["usage_complete"] = "not_started", 0, false
		})), result{"MODEL_CONTRACT_FAILED", "unknown", false, false}},
		{"a success without the measured request digest", mustJSON(t, withReceipt(func(r map[string]any) { r["request_digest"] = nil })), result{"MODEL_CONTRACT_FAILED", "unknown", false, false}},
		{"no receipt member", mustJSON(t, func() map[string]any { m := good(); delete(m, "rencrow"); return m }()), result{"MODEL_CONTRACT_FAILED", "unknown", false, false}},
		{"another member next to the receipt", mustJSON(t, func() map[string]any { m := good(); m["rencrow"].(map[string]any)["x"] = 1; return m }()), result{"MODEL_CONTRACT_FAILED", "unknown", false, false}},
		{"not JSON", []byte("ok"), result{"MODEL_CONTRACT_FAILED", "unknown", false, false}},
		{"a duplicate key", []byte(`{"choices":[],"choices":[]}`), result{"MODEL_CONTRACT_FAILED", "unknown", false, false}},
		{"not an object", []byte(`[]`), result{"MODEL_CONTRACT_FAILED", "unknown", false, false}},

		{"a success that is length", mustJSON(t, completionWire(t, req, msg, "length")), result{"MODEL_CONTRACT_FAILED", "terminal", true, false}},
		{"two choices", mustJSON(t, func() map[string]any {
			m := good()
			c := m["choices"].([]any)
			m["choices"] = append(c, c[0])
			return m
		}()), result{"MODEL_CONTRACT_FAILED", "terminal", true, false}},
		{"no choices", mustJSON(t, func() map[string]any { m := good(); m["choices"] = []any{}; return m }()), result{"MODEL_CONTRACT_FAILED", "terminal", true, false}},
		{"a content that is not text", mustJSON(t, completionWire(t, req, map[string]any{"role": "assistant", "content": 7}, "stop")), result{"MODEL_CONTRACT_FAILED", "terminal", true, false}},
		{"an id that is not text", mustJSON(t, func() map[string]any { m := good(); m["id"] = 5; return m }()), result{"MODEL_CONTRACT_FAILED", "terminal", true, false}},
		{"no message", mustJSON(t, func() map[string]any {
			m := good()
			m["choices"] = []any{map[string]any{"index": 0, "finish_reason": "stop"}}
			return m
		}()), result{"MODEL_CONTRACT_FAILED", "terminal", true, false}},
		{"a Tool call that is not one", mustJSON(t, completionWire(t, req, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{"x"}}, "tool_calls")), result{"MODEL_CONTRACT_FAILED", "terminal", true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateway(t)
			g.reply(generateKey, 200, "application/json", tc.body)
			rc, err := g.client().Generate(t.Context(), req)
			if rc != nil {
				t.Fatal("an answer that breaks the contract is not a stream")
			}
			se := asStrict(t, err)
			if se.Code != tc.code || se.GenerationState() != tc.state || (se.Receipt != nil) != tc.receipt || (se.Receipt != nil && se.Receipt.HiddenRetry) != tc.hidden {
				t.Fatalf("code=%s state=%s receipt=%v (%s)", se.Code, se.GenerationState(), se.Receipt != nil, se.Message)
			}
			if g.requests() != 1 {
				t.Fatalf("%d requests", g.requests())
			}
			notLeaking(t, g, err.Error(), se.Message)
		})
	}

	t.Run("a Tool call without an id reaches the assembler and fails there", func(t *testing.T) {
		g := newGateway(t)
		msg := map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "", "type": "function", "function": map[string]any{"name": "file.read", "arguments": "{}"}},
		}}
		g.reply(generateKey, 200, "application/json", mustJSON(t, completionWire(t, req, msg, "tool_calls")))
		rc, err := g.client().Generate(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if c := assemble(t, rc); c.TerminalKind != modelport.KindError || c.FailureCode != "MODEL_CONTRACT_FAILED" || len(c.ToolIntents) != 0 {
			t.Fatalf("%+v", c)
		}
	})
}

func TestNonStreamRefusalsAreStrictErrorsLikeAnyOther(t *testing.T) {
	req := stageRequest(t, "work_summary")
	g := newGateway(t)
	g.reply(generateKey, 502, "application/json", strictBody(t, "EMPTY_FINAL_CONTENT", true, terminalReceipt(t, req)))
	rc, err := g.client().Generate(t.Context(), req)
	se := asStrict(t, err)
	if rc != nil || se.Code != "EMPTY_FINAL_CONTENT" || !se.Retryable || se.GenerationState() != "terminal" || *se.Receipt.BackendAttempts != 1 {
		t.Fatalf("%+v", se)
	}
}

func TestPiecesNeverSplitARuneAndLoseNothing(t *testing.T) {
	for _, s := range []string{
		"", "a", strings.Repeat("a", pieceBytes), strings.Repeat("a", pieceBytes+1),
		strings.Repeat("あ", pieceBytes), strings.Repeat("é", pieceBytes+3), strings.Repeat("\U0001F600", pieceBytes/2+5),
		"x" + strings.Repeat("\U0001F600", pieceBytes),
	} {
		parts := pieces(s)
		if strings.Join(parts, "") != s {
			t.Fatalf("pieces of %d bytes lose text", len(s))
		}
		for _, p := range parts {
			if len(p) > pieceBytes || len(p) == 0 || !utf8.ValidString(p) {
				t.Fatalf("a bad piece of %d bytes", len(p))
			}
		}
	}
}

func TestAnswerUsageReadsOnlyWhatIsStated(t *testing.T) {
	cached := func(u map[string]any) any { return usageOf(u).(map[string]any)["cached_tokens"] }
	if usageOf(nil) != nil || usageOf("x") != nil {
		t.Fatal("no usage is no usage")
	}
	if got := cached(map[string]any{}); got != nil {
		t.Fatalf("an unstated figure is null: %v", got)
	}
	if got := usageOf(map[string]any{"prompt_tokens": jn("-1")}).(map[string]any)["prompt_tokens"]; got != nil {
		t.Fatalf("a negative figure is not a figure: %v", got)
	}
	if got := cached(map[string]any{"prompt_tokens_details": map[string]any{"cached_tokens": jn("7")}}); got != int64(7) {
		t.Fatalf("details: %v", got)
	}
	if got := cached(map[string]any{"cached_tokens": jn("3"), "prompt_tokens_details": map[string]any{"cached_tokens": jn("7")}}); got != int64(3) {
		t.Fatalf("top level wins: %v", got)
	}
}
