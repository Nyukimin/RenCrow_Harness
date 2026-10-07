package protocol_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// decodeRequest keeps numbers as written so a test can vary only the intended part.
func decodeRequest(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func encodeRequest(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sub(m map[string]any, key string) map[string]any { return m[key].(map[string]any) }

func variant(t *testing.T, base []byte, edit func(m map[string]any)) []byte {
	t.Helper()
	m := decodeRequest(t, base)
	edit(m)
	return encodeRequest(t, m)
}

func digestOf(t *testing.T, raw []byte) string {
	t.Helper()
	d, err := protocol.InputDigest(raw)
	if err != nil {
		t.Fatalf("InputDigest: %v", err)
	}
	return d
}

func TestInputDigestGolden(t *testing.T) {
	var vec struct {
		InputDigest string `json:"input_digest"`
	}
	exampleJSON(t, "wire/request_vector.json", &vec)
	got, err := protocol.InputDigest(example(t, "wire/generation_request.json"))
	if err != nil || got != vec.InputDigest {
		t.Fatalf("generation_request: got %s, %v; want %s", got, err, vec.InputDigest)
	}

	// A second request with tools: its own expected_input_digest is the golden.
	var stream struct {
		Rencrow struct {
			Harness struct {
				ExpectedInputDigest string `json:"expected_input_digest"`
			} `json:"harness"`
		} `json:"rencrow"`
	}
	exampleJSON(t, "wire/stream_request.json", &stream)
	got, err = protocol.InputDigest(example(t, "wire/stream_request.json"))
	if err != nil || got != stream.Rencrow.Harness.ExpectedInputDigest {
		t.Fatalf("stream_request: got %s, %v; want %s", got, err, stream.Rencrow.Harness.ExpectedInputDigest)
	}
	if vec.InputDigest == stream.Rencrow.Harness.ExpectedInputDigest {
		t.Fatal("the two vectors must be distinct requests")
	}
}

func TestInputDigestIgnoresOperationalMetadata(t *testing.T) {
	raw := example(t, "wire/generation_request.json")
	base := digestOf(t, raw)
	same := map[string]func(m map[string]any){
		"request_id": func(m map[string]any) { sub(m, "rencrow")["request_id"] = "req_00000000-0000-7000-8000-0000000000ff" },
		"trace_id":   func(m map[string]any) { sub(m, "rencrow")["trace_id"] = nil },
		"task_id":    func(m map[string]any) { sub(m, "rencrow")["task_id"] = "tsk_x" },
		"session_id": func(m map[string]any) { sub(m, "rencrow")["session_id"] = "" },
		"initiator":  func(m map[string]any) { sub(m, "rencrow")["initiator"] = "core:local" },
		"caller":     func(m map[string]any) { sub(m, "rencrow")["caller"] = "someone" },
		"purpose":    func(m map[string]any) { sub(m, "rencrow")["purpose"] = "other" },
		"expected_input_digest": func(m map[string]any) {
			sub(sub(m, "rencrow"), "harness")["expected_input_digest"] = "0000000000000000000000000000000000000000000000000000000000000000"
		},
		"expected_request_digest": func(m map[string]any) { sub(sub(m, "rencrow"), "harness")["expected_request_digest"] = nil },
		"expected_binding_fingerprint": func(m map[string]any) {
			delete(sub(sub(m, "rencrow"), "harness"), "expected_binding_fingerprint")
		},
		"retry_of_request_id": func(m map[string]any) {
			sub(sub(sub(m, "rencrow"), "harness"), "recovery")["retry_of_request_id"] = "req_00000000-0000-7000-8000-0000000000fe"
		},
		"trigger_code": func(m map[string]any) { sub(sub(sub(m, "rencrow"), "harness"), "recovery")["trigger_code"] = "LENGTH" },
	}
	for name, edit := range same {
		if got := digestOf(t, variant(t, raw, edit)); got != base {
			t.Errorf("%s must not change the input digest", name)
		}
	}
	// Re-serializing the same request in another key order and spelling is the same request.
	if got := digestOf(t, encodeRequest(t, decodeRequest(t, raw))); got != base {
		t.Error("wire-level key order must not matter")
	}
	spelled := variant(t, raw, func(m map[string]any) { m["max_tokens"] = json.Number("4.096e3") })
	if digestOf(t, spelled) != base {
		t.Error("numeric spelling must not matter (4096 and 4.096e3)")
	}
}

func TestInputDigestCoversEveryDeclaredField(t *testing.T) {
	raw := example(t, "wire/generation_request.json")
	base := digestOf(t, raw)
	changes := map[string]func(m map[string]any){
		"model":           func(m map[string]any) { m["model"] = "other-model" },
		"message content": func(m map[string]any) { m["messages"].([]any)[1].(map[string]any)["content"] = "changed" },
		"message order": func(m map[string]any) {
			ms := m["messages"].([]any)
			ms[1], ms[2] = ms[2], ms[1]
		},
		"messages dropped": func(m map[string]any) { m["messages"] = m["messages"].([]any)[:2] },
		"tools":            func(m map[string]any) { m["tools"] = []any{map[string]any{"type": "function"}} },
		"tool_choice":      func(m map[string]any) { m["tool_choice"] = "auto" },
		"response_format":  func(m map[string]any) { m["response_format"] = map[string]any{"type": "json_object"} },
		"stream": func(m map[string]any) {
			m["stream"] = false
			m["stream_options"] = nil
		},
		"max_tokens":  func(m map[string]any) { m["max_tokens"] = json.Number("4097") },
		"temperature": func(m map[string]any) { m["temperature"] = json.Number("0") },
		"top_p":       func(m map[string]any) { m["top_p"] = json.Number("1") },
		"seed":        func(m map[string]any) { m["seed"] = json.Number("0") },
		"stop":        func(m map[string]any) { m["stop"] = []any{"END"} },
		"agent_id":    func(m map[string]any) { sub(m, "rencrow")["agent_id"] = "shiro" },
		"role":        func(m map[string]any) { sub(m, "rencrow")["execution_role"] = "coder" },
		"alias":       func(m map[string]any) { sub(m, "rencrow")["execution_alias"] = "alias" },
		"stage":       func(m map[string]any) { sub(sub(m, "rencrow"), "harness")["stage"] = "work_summary" },
		"profile id": func(m map[string]any) {
			sub(sub(sub(m, "rencrow"), "harness"), "recovery")["profile_id"] = "terminal_output_once"
		},
		"profile revision": func(m map[string]any) {
			sub(sub(sub(m, "rencrow"), "harness"), "recovery")["profile_revision"] = "builtin-v2"
		},
	}
	seen := map[string]string{base: "base"}
	for name, edit := range changes {
		got := digestOf(t, variant(t, raw, edit))
		if prev, dup := seen[got]; dup {
			t.Errorf("%s did not change the digest relative to %s", name, prev)
		}
		seen[got] = name
	}
	// null and a concrete value are different requests: null means the fixed base profile value.
	withTemp := digestOf(t, variant(t, raw, func(m map[string]any) { m["temperature"] = json.Number("0") }))
	if withTemp == base {
		t.Error("temperature null and 0 must differ")
	}
	// Empty stop list and absent stop are different shapes; absent is rejected below.
}

func TestInputDigestRejects(t *testing.T) {
	raw := example(t, "wire/generation_request.json")
	topLevel := []string{"model", "messages", "tools", "tool_choice", "response_format", "stream", "stream_options",
		"max_tokens", "temperature", "top_p", "seed", "stop", "rencrow"}
	rencrow := []string{"request_id", "trace_id", "task_id", "session_id", "initiator", "caller", "purpose",
		"agent_id", "execution_role", "execution_alias", "harness"}
	harness := []string{"contract_version", "stage", "max_backend_attempts", "recovery"}
	recovery := []string{"profile_id", "profile_revision", "retry_of_request_id", "trigger_code"}

	cases := map[string]func(m map[string]any){
		"unknown top-level field":  func(m map[string]any) { m["n"] = json.Number("1") },
		"unknown rencrow field":    func(m map[string]any) { sub(m, "rencrow")["extra"] = "x" },
		"unknown harness field":    func(m map[string]any) { sub(sub(m, "rencrow"), "harness")["extra"] = "x" },
		"unknown recovery field":   func(m map[string]any) { sub(sub(sub(m, "rencrow"), "harness"), "recovery")["extra"] = "x" },
		"bad contract version":     func(m map[string]any) { sub(sub(m, "rencrow"), "harness")["contract_version"] = "harness-v2" },
		"bad stage":                func(m map[string]any) { sub(sub(m, "rencrow"), "harness")["stage"] = "plan" },
		"bad backend attempts":     func(m map[string]any) { sub(sub(m, "rencrow"), "harness")["max_backend_attempts"] = json.Number("2") },
		"bad profile id":           func(m map[string]any) { sub(sub(sub(m, "rencrow"), "harness"), "recovery")["profile_id"] = "other" },
		"empty profile revision":   func(m map[string]any) { sub(sub(sub(m, "rencrow"), "harness"), "recovery")["profile_revision"] = "" },
		"stream with null options": func(m map[string]any) { m["stream_options"] = nil },
		"non-stream with usage": func(m map[string]any) {
			m["stream"] = false
		},
		"model not a string":    func(m map[string]any) { m["model"] = json.Number("1") },
		"messages not an array": func(m map[string]any) { m["messages"] = "x" },
		"messages empty":        func(m map[string]any) { m["messages"] = []any{} },
		"tools null":            func(m map[string]any) { m["tools"] = nil },
		"stop null":             func(m map[string]any) { m["stop"] = nil },
		"stream not bool":       func(m map[string]any) { m["stream"] = "true" },
		"max_tokens fractional": func(m map[string]any) { m["max_tokens"] = json.Number("4096.5") },
		"max_tokens string":     func(m map[string]any) { m["max_tokens"] = "4096" },
		"temperature string":    func(m map[string]any) { m["temperature"] = "0" },
		"seed fractional":       func(m map[string]any) { m["seed"] = json.Number("1.5") },
		"agent_id number":       func(m map[string]any) { sub(m, "rencrow")["agent_id"] = json.Number("1") },
		"rencrow not object":    func(m map[string]any) { m["rencrow"] = nil },
		"harness not object":    func(m map[string]any) { sub(m, "rencrow")["harness"] = nil },
	}
	for _, f := range topLevel {
		f := f
		cases["missing "+f] = func(m map[string]any) { delete(m, f) }
	}
	for _, f := range rencrow {
		f := f
		cases["missing rencrow."+f] = func(m map[string]any) { delete(sub(m, "rencrow"), f) }
	}
	for _, f := range harness {
		f := f
		cases["missing harness."+f] = func(m map[string]any) { delete(sub(sub(m, "rencrow"), "harness"), f) }
	}
	for _, f := range recovery {
		f := f
		cases["missing recovery."+f] = func(m map[string]any) { delete(sub(sub(sub(m, "rencrow"), "harness"), "recovery"), f) }
	}
	for name, edit := range cases {
		got, err := protocol.InputDigest(variant(t, raw, edit))
		if err == nil {
			t.Errorf("%s accepted: %s", name, got)
		} else if !errors.Is(err, protocol.ErrInvalidInput) {
			t.Errorf("%s: error does not wrap ErrInvalidInput: %v", name, err)
		}
	}

	for name, text := range map[string]string{
		"empty":         ``,
		"array":         `[]`,
		"null":          `null`,
		"duplicate key": `{"model":"a","model":"b"}`,
		"trailing":      string(raw) + `{}`,
		"BOM":           "\xef\xbb\xbf" + string(raw),
		"invalid utf8":  string(bytes.Replace(raw, []byte("fixture-local--host"), []byte("fixture-\xff-host"), 1)),
		"huge exponent": string(bytes.Replace(raw, []byte(`"max_tokens": 4096`), []byte(`"max_tokens": 1e5000`), 1)),
	} {
		if _, err := protocol.InputDigest([]byte(text)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
