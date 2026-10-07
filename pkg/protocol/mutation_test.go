package protocol_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

type mutationVector struct {
	Method       string          `json:"method"`
	Principal    string          `json:"principal"`
	Params       json.RawMessage `json:"params"`
	ExpectedHash string          `json:"expected_hash"`
}

func loadMutationVectors(t *testing.T) []mutationVector {
	t.Helper()
	var vs []mutationVector
	exampleJSON(t, "wire/mutation_vectors.json", &vs)
	if len(vs) != 7 {
		t.Fatalf("expected 7 mutation vectors, got %d", len(vs))
	}
	return vs
}

func TestMutationPayloadHashGolden(t *testing.T) {
	for _, v := range loadMutationVectors(t) {
		t.Run(v.Method, func(t *testing.T) {
			got, err := protocol.MutationPayloadHash(v.Principal, v.Method, v.Params)
			if err != nil || got != v.ExpectedHash {
				t.Fatalf("got %s, %v; want %s", got, err, v.ExpectedHash)
			}
		})
	}
}

func turnStart(t *testing.T) mutationVector {
	t.Helper()
	for _, v := range loadMutationVectors(t) {
		if v.Method == "turn/start" {
			return v
		}
	}
	t.Fatal("no turn/start vector")
	return mutationVector{}
}

func hashOf(t *testing.T, principal, method, params string) string {
	t.Helper()
	got, err := protocol.MutationPayloadHash(principal, method, []byte(params))
	if err != nil {
		t.Fatalf("%s: %v", params, err)
	}
	return got
}

func TestMutationPayloadHashSemantics(t *testing.T) {
	v := turnStart(t)
	base := hashOf(t, v.Principal, v.Method, string(v.Params))

	// Same meaning on the wire: key order, whitespace and numeric spelling.
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(v.Params, &decoded); err != nil {
		t.Fatal(err)
	}
	reordered := `{"limits":` + string(decoded["limits"]) + `,"idempotency_key":` + string(decoded["idempotency_key"]) +
		`,"expected_control_revision":0.0,"expected_context_revision":0e5,"upstream":null,"context_blocks":[],` +
		`"input":` + string(decoded["input"]) + `,"thread_id":` + string(decoded["thread_id"]) + `}`
	if got := hashOf(t, v.Principal, v.Method, reordered); got != base {
		t.Fatalf("wire-level reordering changed the hash: %s vs %s", got, base)
	}

	// The top-level idempotency_key is excluded from its own hash.
	rekeyed := strings.Replace(string(v.Params), "fixture.turn.start.0001", "fixture.turn.start.9999", 1)
	if rekeyed == string(v.Params) {
		t.Fatal("test fixture did not change")
	}
	if got := hashOf(t, v.Principal, v.Method, rekeyed); got != base {
		t.Fatal("changing only the top-level idempotency_key must not change the hash")
	}

	// Everything else is included.
	changed := map[string]string{
		"input text":    strings.Replace(string(v.Params), "テスト失敗", "テスト成功", 1),
		"limits":        strings.Replace(string(v.Params), `"max_model_steps": 10`, `"max_model_steps": 11`, 1),
		"null to value": strings.Replace(string(v.Params), `"upstream": null`, `"upstream": {}`, 1),
		"revision":      strings.Replace(string(v.Params), `"expected_context_revision": 0`, `"expected_context_revision": 1`, 1),
		"blocks":        strings.Replace(string(v.Params), `"context_blocks": []`, `"context_blocks": [{}]`, 1),
	}
	for name, params := range changed {
		if params == string(v.Params) {
			t.Fatalf("%s: fixture did not change", name)
		}
		if got := hashOf(t, v.Principal, v.Method, params); got == base {
			t.Errorf("%s must change the hash", name)
		}
	}
	// Absent is not null.
	absent := strings.Replace(string(v.Params), `"upstream": null,`, ``, 1)
	if got := hashOf(t, v.Principal, v.Method, absent); got == base {
		t.Error("an omitted field must differ from an explicit null")
	}
	// Principal and method take part.
	if hashOf(t, "user:ren", v.Method, string(v.Params)) == base {
		t.Error("principal must take part")
	}
	if hashOf(t, v.Principal, "run/resume", string(v.Params)) == base {
		t.Error("method must take part")
	}
}

func TestMutationPayloadHashKeepsNestedIdempotencyKey(t *testing.T) {
	a := hashOf(t, "core:local", "session/open", `{"idempotency_key":"k","x":{"idempotency_key":"one"}}`)
	b := hashOf(t, "core:local", "session/open", `{"idempotency_key":"k","x":{"idempotency_key":"two"}}`)
	if a == b {
		t.Fatal("only the top-level idempotency_key may be dropped")
	}
}

func TestMutationPayloadHashRejects(t *testing.T) {
	const ok = `{"idempotency_key":"k"}`
	cases := map[string]struct{ principal, method, params string }{
		"read method":         {"core:local", "run/get", ok},
		"initialize":          {"core:local", "initialize", ok},
		"shutdown has no key": {"core:local", "service/shutdown", ok},
		"unknown method":      {"core:local", "turn/start ", ok},
		"empty method":        {"core:local", "", ok},
		"bad principal":       {"Core:local", "turn/start", ok},
		"empty principal":     {"", "turn/start", ok},
		"missing key":         {"core:local", "turn/start", `{"a":1}`},
		"null key":            {"core:local", "turn/start", `{"idempotency_key":null}`},
		"numeric key":         {"core:local", "turn/start", `{"idempotency_key":1}`},
		"empty key":           {"core:local", "turn/start", `{"idempotency_key":""}`},
		"params array":        {"core:local", "turn/start", `[{"idempotency_key":"k"}]`},
		"params null":         {"core:local", "turn/start", `null`},
		"duplicate key":       {"core:local", "turn/start", `{"idempotency_key":"k","a":1,"a":2}`},
		"duplicate top key":   {"core:local", "turn/start", `{"idempotency_key":"k","idempotency_key":"j"}`},
		"trailing value":      {"core:local", "turn/start", `{"idempotency_key":"k"}{}`},
		"invalid utf8":        {"core:local", "turn/start", "{\"idempotency_key\":\"k\",\"a\":\"\xff\"}"},
		"NaN":                 {"core:local", "turn/start", `{"idempotency_key":"k","a":NaN}`},
		"huge exponent":       {"core:local", "turn/start", `{"idempotency_key":"k","a":1e5000}`},
		"empty params":        {"core:local", "turn/start", ``},
		"unpaired surrogate":  {"core:local", "turn/start", `{"idempotency_key":"k","a":"\ud800"}`},
	}
	for name, c := range cases {
		if got, err := protocol.MutationPayloadHash(c.principal, c.method, []byte(c.params)); err == nil {
			t.Errorf("%s accepted: %s", name, got)
		} else if !errors.Is(err, protocol.ErrInvalidInput) {
			t.Errorf("%s: error does not wrap ErrInvalidInput: %v", name, err)
		}
	}
	// service/shutdown is explicitly outside the mutation hash.
	if _, err := protocol.MutationPayloadHash("core:local", "service/shutdown", []byte(`{}`)); !errors.Is(err, protocol.ErrNotMutationMethod) {
		t.Errorf("shutdown: %v", err)
	}
}
