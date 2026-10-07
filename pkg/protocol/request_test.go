package protocol_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// All 16 native methods, each with the params type the schema binds to it.
var nativeMethods = map[string]string{
	"initialize":           "InitializeInput",
	"service/capabilities": "EmptyInput",
	"session/open":         "SessionOpenInput",
	"session/list":         "SessionListInput",
	"session/get":          "SessionGetInput",
	"session/fork":         "SessionForkInput",
	"turn/start":           "StartInput",
	"input/append":         "InputAppendInput",
	"turn/interrupt":       "InterruptInput",
	"run/get":              "RunGetInput",
	"run/resume":           "ResumeInput",
	"context/compact":      "CompactInput",
	"receipt/get":          "ReceiptGetInput",
	"events/read":          "EventsReadInput",
	"evidence/read":        "EvidenceReadInput",
	"service/shutdown":     "ShutdownInput",
}

func TestNativeRequestsDecodeForAll16Methods(t *testing.T) {
	seen := map[string]bool{}
	for _, raw := range rawList(t, "native_requests.json") {
		req, err := protocol.DecodeRequest(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		params, err := req.DecodeParams()
		if err != nil {
			t.Fatalf("%s params: %v", req.Method, err)
		}
		if got := typeName(params); got != nativeMethods[req.Method] {
			t.Fatalf("%s decoded as %s, want %s", req.Method, got, nativeMethods[req.Method])
		}
		seen[req.Method] = true
	}
	if len(seen) != 16 {
		t.Fatalf("native_requests.json covers %d methods, want 16", len(seen))
	}
}

func typeName(v any) string { return strings.TrimPrefix(fmt.Sprintf("%T", v), "protocol.") }

func TestRequestEnvelopeRejections(t *testing.T) {
	good := string(rawList(t, "native_requests.json")[0])
	cases := map[string]string{
		"batch request":            "[" + good + "]",
		"unknown method":           `{"jsonrpc":"2.0","id":"req_00000000-0000-7000-8000-000000000001","method":"turn/steer","params":{}}`,
		"wrong jsonrpc":            strings.Replace(good, `"2.0"`, `"1.0"`, 1),
		"unsupported version":      strings.Replace(good, `rencrow-harness/v1`, `rencrow-harness/v2`, 1),
		"id not a RequestID":       strings.Replace(good, `req_00000000-0000-7000-8000-000000000001`, `1`, 1),
		"missing params":           `{"jsonrpc":"2.0","id":"req_00000000-0000-7000-8000-000000000001","method":"service/capabilities"}`,
		"unknown envelope field":   strings.Replace(good, `"jsonrpc"`, `"extra":1,"jsonrpc"`, 1),
		"duplicate key":            strings.Replace(good, `"method"`, `"method":"initialize","method"`, 1),
		"trailing value":           good + ` {}`,
		"params of another method": `{"jsonrpc":"2.0","id":"req_00000000-0000-7000-8000-000000000001","method":"run/get","params":{"thread_id":"thr_00000000-0000-7000-8000-000000000001"}}`,
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := protocol.DecodeRequest([]byte(text))
			wantCode(t, err, protocol.CodeInvalidParams)
		})
	}
}

func TestRequestMutationPayloadHashMatchesVectors(t *testing.T) {
	var vectors []struct {
		Method       string          `json:"method"`
		Principal    string          `json:"principal"`
		Params       json.RawMessage `json:"params"`
		ExpectedHash string          `json:"expected_hash"`
	}
	exampleJSON(t, "wire/mutation_vectors.json", &vectors)
	checked := 0
	for _, v := range vectors {
		// Only vectors whose params are valid for the real method can become a request.
		env := `{"jsonrpc":"2.0","id":"req_00000000-0000-7000-8000-0000000000aa","method":"` + v.Method + `","params":` + string(v.Params) + `}`
		req, err := protocol.DecodeRequest([]byte(env))
		if err != nil {
			continue
		}
		got, err := req.MutationPayloadHash(v.Principal)
		if err != nil || got != v.ExpectedHash {
			t.Fatalf("%s: got %s, %v; want %s", v.Method, got, err, v.ExpectedHash)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no mutation vector became a valid request")
	}
	t.Logf("mutation vectors checked through Request: %d of %d", checked, len(vectors))
}

func TestRequestMutationPayloadHashRefusesReads(t *testing.T) {
	req, err := protocol.DecodeRequest([]byte(`{"jsonrpc":"2.0","id":"req_00000000-0000-7000-8000-0000000000aa","method":"run/get","params":{"run_id":"run_00000000-0000-7000-8000-000000000001"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := req.MutationPayloadHash("core:local"); err == nil {
		t.Fatal("run/get has no mutation payload hash")
	}
}
