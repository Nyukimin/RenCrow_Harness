package modelclient

import (
	"bytes"
	"encoding/json"
	"io"
	"unicode/utf8"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

// pieceBytes bounds one synthesized content or argument fragment, far below the
// frame limit even when every byte is escaped.
const pieceBytes = 16 << 10

// answerStream turns the non-stream success of the Gateway (one Chat completion and
// its rencrow.harness_receipt) into the strict stream the assembler reads, after the
// receipt was held to the request: the same stage, profile, digests and binding as
// expected, one logical request, no hidden retry, and a finished generation (state
// terminal, one backend attempt, all three values measured). The completion's own
// shape is read closed.
//
// The stream is made, not received: its terminal frame is built from the receipt the
// Gateway stated and the completion's finish_reason and usage, and nothing else is
// added. A Gateway answer that fails any of this is a contract failure; with a valid
// receipt the receipt is kept, so its state is not lost, and without one the
// generation is unknown.
func answerStream(data []byte, req modelport.ChatRequest, exp expectation) (io.ReadCloser, error) {
	v, err := strictjson.Decode(data)
	if err != nil {
		return nil, contractFailed(nil, "the answer is not strict JSON")
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, contractFailed(nil, "the answer is not an object")
	}
	if hiddenRetryStated(root) {
		return nil, hiddenRetryError(root, exp, nil)
	}
	ext, ok := root["rencrow"].(map[string]any)
	if !ok || len(ext) != 1 {
		return nil, contractFailed(nil, "the answer carries no receipt")
	}
	var receipt modelport.GatewayAttemptReceipt
	if err := schemacheck.Unmarshal(schemacheck.LLMContract, "GatewayAttemptReceipt", ext["harness_receipt"], &receipt); err != nil {
		return nil, contractFailed(nil, "the answer's receipt is not valid")
	}
	if err := modelport.VerifyReceipt(receipt, exp.expected()); err != nil {
		return nil, contractFailed(nil, err.Error())
	}
	if receipt.GenerationState != modelport.StateTerminal || receipt.BackendAttempts == nil || *receipt.BackendAttempts != 1 ||
		receipt.InputDigest == nil || receipt.RequestDigest == nil || receipt.BindingFingerprint == nil {
		return nil, contractFailed(nil, "the answer's receipt does not show a finished generation with its digests")
	}

	fail := func(message string) (io.ReadCloser, error) {
		e := contractFailed(nil, message)
		e.Receipt = &receipt
		return nil, e
	}
	choice, finish, ok := firstChoice(root)
	if !ok {
		return fail("the answer has no usable choice")
	}
	if finish != "stop" && finish != "tool_calls" {
		return fail("a success has a finish_reason other than stop or tool_calls")
	}
	id, ok := responseID(root)
	if !ok {
		return fail("the answer's id is not a string")
	}
	msg, ok := choice["message"].(map[string]any)
	if !ok {
		return fail("the answer has no message")
	}
	var b bytes.Buffer
	if !writeMessage(&b, id, msg) {
		return fail("the answer's message is not a message")
	}
	chunk(&b, id, map[string]any{}, &finish)
	terminal, err := marshalJSON(map[string]any{
		"contract_version": modelport.ContractVersion, "outcome": "completed", "finish_reason": finish, "code": nil,
		"provider_response_id": id, "usage": usageOf(root["usage"]), "harness_receipt": receipt,
	})
	if err != nil {
		return fail("the closing frame cannot be built")
	}
	b.WriteString("event: rencrow.terminal\ndata: ")
	b.Write(terminal)
	b.WriteString("\n\ndata: [DONE]\n\n")
	return &builtStream{Reader: &b, raw: data}, nil
}

// builtStream is the strict stream made of a non-stream answer. RawBody gives the
// answer as the Gateway sent it, so that a caller that keeps the response as
// Evidence can keep that and not the stream made of it.
type builtStream struct {
	io.Reader
	raw []byte
}

func (s *builtStream) Close() error { return nil }

// RawBody returns the Gateway's answer, byte for byte.
func (s *builtStream) RawBody() []byte { return s.raw }

func firstChoice(root map[string]any) (map[string]any, string, bool) {
	list, ok := root["choices"].([]any)
	if !ok || len(list) != 1 {
		return nil, "", false
	}
	choice, ok := list[0].(map[string]any)
	if !ok {
		return nil, "", false
	}
	finish, ok := choice["finish_reason"].(string)
	return choice, finish, ok && finish != ""
}

// responseID returns the completion's id (nil when it has none) and whether it is
// a usable one.
func responseID(root map[string]any) (any, bool) {
	raw, present := root["id"]
	if !present || raw == nil {
		return nil, true
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return nil, false
	}
	return s, true
}

// writeMessage writes the chunks of one completion message: the role, then its text,
// reasoning and Tool calls in fragments. It reports whether the message was one.
func writeMessage(b *bytes.Buffer, id any, msg map[string]any) bool {
	chunk(b, id, map[string]any{"role": "assistant"}, nil)
	if content, present := msg["content"]; present && content != nil {
		s, ok := content.(string)
		if !ok {
			return false
		}
		for _, p := range pieces(s) {
			chunk(b, id, map[string]any{"content": p}, nil)
		}
	}
	if reasoning, present := msg["reasoning_content"]; present && reasoning != nil {
		s, ok := reasoning.(string)
		if !ok {
			return false
		}
		for _, p := range pieces(s) {
			chunk(b, id, map[string]any{"reasoning_content": p}, nil)
		}
	}
	if raw, present := msg["tool_calls"]; present && raw != nil {
		calls, ok := raw.([]any)
		if !ok {
			return false
		}
		for i, c := range calls {
			if !writeCall(b, id, i, c) {
				return false
			}
		}
	}
	return true
}

func writeCall(b *bytes.Buffer, id any, index int, v any) bool {
	call, ok := v.(map[string]any)
	if !ok {
		return false
	}
	fn, ok := call["function"].(map[string]any)
	if !ok {
		return false
	}
	callID, ok1 := call["id"].(string)
	typ, ok2 := call["type"].(string)
	name, ok3 := fn["name"].(string)
	args, ok4 := fn["arguments"].(string)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return false
	}
	parts := pieces(args)
	first := ""
	if len(parts) > 0 {
		first, parts = parts[0], parts[1:]
	}
	chunk(b, id, map[string]any{"tool_calls": []any{map[string]any{
		"index": index, "id": callID, "type": typ, "function": map[string]any{"name": name, "arguments": first},
	}}}, nil)
	for _, p := range parts {
		chunk(b, id, map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "function": map[string]any{"arguments": p},
		}}}, nil)
	}
	return true
}

// chunk writes one data frame: a Chat chunk with one choice.
func chunk(b *bytes.Buffer, id any, delta map[string]any, finish *string) {
	var fr any
	if finish != nil {
		fr = *finish
	}
	m := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fr}}}
	if id != nil {
		m["id"] = id
	}
	raw, _ := marshalJSON(m)
	b.WriteString("data: ")
	b.Write(raw)
	b.WriteString("\n\n")
}

// pieces splits s into fragments of at most pieceBytes bytes, never inside a rune.
func pieces(s string) []string {
	var out []string
	for len(s) > pieceBytes {
		cut := pieceBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// usageOf reads the completion's usage into the closed usage of a terminal frame;
// what the completion does not state is null.
func usageOf(v any) any {
	u, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	count := func(x any) any {
		n, ok := x.(json.Number)
		if !ok {
			return nil
		}
		i, err := n.Int64()
		if err != nil || i < 0 {
			return nil
		}
		return i
	}
	cached := count(u["cached_tokens"])
	if details, ok := u["prompt_tokens_details"].(map[string]any); ok && cached == nil {
		cached = count(details["cached_tokens"])
	}
	return map[string]any{"prompt_tokens": count(u["prompt_tokens"]), "completion_tokens": count(u["completion_tokens"]), "cached_tokens": cached}
}
