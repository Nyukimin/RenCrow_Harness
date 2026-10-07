package modelport_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func decodeRequest(t testing.TB, name string) modelport.ChatRequest {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(wire(t, name)))
	dec.DisallowUnknownFields()
	var r modelport.ChatRequest
	if err := dec.Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestWireRequestsRoundTripAndDigest holds the typed request to the design's
// vectors: the canonical form is the fixture's, and the Harness's own logical
// input digest is the one the vector records (the metadata and expected_* values
// are not part of it).
func TestWireRequestsRoundTripAndDigest(t *testing.T) {
	for _, tc := range []struct {
		file, input string
	}{
		{"stream_request.json", "9a0742630d64d3fc648f24a0d92b22362e17d40af325efb2196396481599615c"},
		{"generation_request.json", "3dae8769d753391e270b48f531d8a1abbcc8ea21696fbea0d5a3761965ab54ef"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			r := decodeRequest(t, tc.file)
			got, err := r.Canonical()
			if err != nil {
				t.Fatal(err)
			}
			want, err := protocol.EncodeCanonicalContract(wire(t, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("the typed request does not reproduce the fixture\n got %s\nwant %s", got, want)
			}
			d, err := r.LogicalInputDigest()
			if err != nil || d != tc.input {
				t.Fatalf("input digest %q (%v), want %s", d, err, tc.input)
			}
			if err := r.Validate(true); err != nil {
				t.Fatalf("the vector does not satisfy GenerationRequest: %v", err)
			}
		})
	}
}

func TestNewActRequestBuildsTheStrictRequest(t *testing.T) {
	fixture := decodeRequest(t, "stream_request.json")
	desc := modelport.BindingDescriptor{StageOptions: modelport.StageOptions{Act: modelport.GenerationOptions{
		MaxTokens: 4096, Stop: []string{},
	}}}
	binding := protocol.Binding{Kind: "model_route", Selector: "fixture-local--host", ProfileRevision: "fixture-profile-1"}
	req, err := modelport.NewActRequest(modelport.ActParams{
		Binding: binding, Descriptor: desc, Messages: fixture.Messages, Tools: fixture.Tools,
		Meta: modelport.RequestMeta{
			RequestID: "req_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000a", TraceID: "trc_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000b",
			TaskID: "tsk_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000c", SessionID: "ses_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000d",
			Initiator: "user:ren", Caller: "harness.fixture",
		},
		Recovery: modelport.RecoveryRequest{ProfileID: "same_request", ProfileRevision: "builtin-v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := req.Validate(false); err != nil {
		t.Fatalf("a measure request must satisfy ChatRequest: %v", err)
	}
	d, err := req.LogicalInputDigest()
	if err != nil || d != "9a0742630d64d3fc648f24a0d92b22362e17d40af325efb2196396481599615c" {
		t.Fatalf("digest %s (%v)", d, err)
	}
	if req.ToolChoice != "auto" || !req.Stream || req.StreamOptions == nil || !req.StreamOptions.IncludeUsage || req.Rencrow.Harness.MaxBackendAttempts != 1 {
		t.Fatalf("%+v", req)
	}
	if err := req.Validate(true); err == nil {
		t.Fatal("a request without expected_* values passed as a generation request")
	}
	gen := req.WithExpected(d, "fec1d0c41d2ece109a38e21c9c10d4a6598e169292515aaae4eaa12739a5a88a", "bfp-v1:cf25685e3cbff5f9364f8bdadb54d8b852578931ad2602a62e077613b3a6ef8d")
	if err := gen.Validate(true); err != nil {
		t.Fatal(err)
	}
	if req.Rencrow.Harness.ExpectedInputDigest != "" {
		t.Fatal("WithExpected changed the original request")
	}
	want, _ := fixture.Canonical()
	got, _ := gen.Canonical()
	if !bytes.Equal(got, want) {
		t.Fatalf("the built request is not the fixture\n got %s\nwant %s", got, want)
	}

	// With no Tool the request names tool_choice none; options are the descriptor's.
	none, err := modelport.NewActRequest(modelport.ActParams{Binding: binding, Descriptor: desc, Messages: fixture.Messages, Recovery: modelport.RecoveryRequest{ProfileID: "same_request", ProfileRevision: "builtin-v1"}})
	if err != nil || none.ToolChoice != "none" || len(none.Tools) != 0 {
		t.Fatalf("%+v %v", none, err)
	}
	if _, err := modelport.NewActRequest(modelport.ActParams{Binding: binding, Messages: fixture.Messages}); err == nil {
		t.Fatal("a descriptor without act max_tokens was accepted: the Harness invents no option")
	}
}

func TestChatMessageShapesAreClosed(t *testing.T) {
	for _, tc := range []struct {
		name, js string
		ok       bool
	}{
		{"user", `{"role":"user","content":"x"}`, true},
		{"assistant with null content", `{"role":"assistant","content":null,"tool_calls":[]}`, true},
		{"assistant without tool_calls", `{"role":"assistant","content":"x"}`, false},
		{"user with null content", `{"role":"user","content":null}`, false},
		{"user with tool_calls", `{"role":"user","content":"x","tool_calls":[]}`, false},
		{"tool without id", `{"role":"tool","content":"x"}`, false},
		{"tool", `{"role":"tool","tool_call_id":"c","content":"x"}`, true},
		{"unknown role", `{"role":"narrator","content":"x"}`, false},
		{"extra key", `{"role":"user","content":"x","name":"n"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m modelport.ChatMessage
			err := json.Unmarshal([]byte(tc.js), &m)
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v, want ok=%v", err, tc.ok)
			}
			if tc.ok {
				out, err := json.Marshal(m)
				if err != nil || strings.ReplaceAll(string(out), " ", "") != tc.js {
					t.Fatalf("re-marshal %s (%v), want %s", out, err, tc.js)
				}
			}
		})
	}
}

func TestVerifyReceiptHoldsTheReceiptToTheRequest(t *testing.T) {
	var term modelport.StreamTerminal
	if err := json.Unmarshal(wire(t, "stream_terminal.json"), &term); err != nil {
		t.Fatal(err)
	}
	exp := modelport.Expected{
		Stage: "act", InputDigest: *term.HarnessReceipt.InputDigest, RequestDigest: *term.HarnessReceipt.RequestDigest,
		BindingFingerprint: *term.HarnessReceipt.BindingFingerprint, ProfileID: "same_request", ProfileRevision: "builtin-v1",
	}
	if err := modelport.VerifyReceipt(term.HarnessReceipt, exp); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*modelport.Expected, *modelport.GatewayAttemptReceipt){
		"stage": func(e *modelport.Expected, r *modelport.GatewayAttemptReceipt) { r.Stage = "work_summary" },
		"input": func(e *modelport.Expected, r *modelport.GatewayAttemptReceipt) {
			e.InputDigest = strings.Repeat("0", 64)
		},
		"request": func(e *modelport.Expected, r *modelport.GatewayAttemptReceipt) {
			e.RequestDigest = strings.Repeat("0", 64)
		},
		"binding": func(e *modelport.Expected, r *modelport.GatewayAttemptReceipt) { e.BindingFingerprint = "bfp-v1:other" },
		"profile": func(e *modelport.Expected, r *modelport.GatewayAttemptReceipt) {
			r.RecoveryProfile = "terminal_output_once"
		},
		"revision": func(e *modelport.Expected, r *modelport.GatewayAttemptReceipt) { r.RecoveryProfileRevision = "other" },
		"hidden":   func(e *modelport.Expected, r *modelport.GatewayAttemptReceipt) { r.HiddenRetry = true },
		"logical":  func(e *modelport.Expected, r *modelport.GatewayAttemptReceipt) { r.LogicalRequests = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			e, r := exp, term.HarnessReceipt
			mut(&e, &r)
			if err := modelport.VerifyReceipt(r, e); err == nil {
				t.Fatal("a contradicting receipt was accepted")
			}
		})
	}
	// A digest the receipt does not carry is not compared.
	r := term.HarnessReceipt
	r.InputDigest, r.RequestDigest, r.BindingFingerprint = nil, nil, nil
	if err := modelport.VerifyReceipt(r, exp); err != nil {
		t.Fatal(err)
	}
}

// TestNewStageRequestBuildsTheNoToolsJSONRequestOfACompactionStage: a stage declares no Tool,
// chooses none, answers one JSON object, is not streamed, takes its options from the stage's
// own entry of the descriptor, and is never a retry.
func TestNewStageRequestBuildsTheNoToolsJSONRequestOfACompactionStage(t *testing.T) {
	fixture := decodeRequest(t, "stream_request.json")
	temp := json.Number("0")
	desc := modelport.BindingDescriptor{StageOptions: modelport.StageOptions{
		Act:                  modelport.GenerationOptions{MaxTokens: 4096, Stop: []string{}},
		InstructionSelection: modelport.GenerationOptions{MaxTokens: 1024, Temperature: &temp, Stop: []string{}},
		WorkSummary:          modelport.GenerationOptions{MaxTokens: 2048, Stop: []string{}},
	}}
	binding := protocol.Binding{Kind: "alias", Selector: "fixture-alias", ProfileRevision: "fixture-profile-1"}
	meta := modelport.RequestMeta{RequestID: "req_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000a", TraceID: "trc_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000b",
		TaskID: "tsk_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000c", SessionID: "ses_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000d", Initiator: "user:ren", Caller: "harness.fixture"}
	same := modelport.RecoveryRequest{ProfileID: "same_request", ProfileRevision: "builtin-v1"}
	for stage, want := range map[string]int64{modelport.StageSelection: 1024, modelport.StageSummary: 2048} {
		t.Run(stage, func(t *testing.T) {
			req, err := modelport.NewStageRequest(modelport.StageParams{Binding: binding, Descriptor: desc, Stage: stage, Messages: fixture.Messages[:2], Meta: meta, Recovery: same})
			if err != nil {
				t.Fatal(err)
			}
			if req.Stream || req.StreamOptions != nil || req.ToolChoice != "none" || len(req.Tools) != 0 || req.ResponseFormat.Type != "json_object" || req.MaxTokens != want ||
				req.Rencrow.Harness.Stage != stage || *req.Rencrow.Purpose != stage || req.Rencrow.Harness.MaxBackendAttempts != 1 || req.Rencrow.ExecutionAlias == nil || *req.Rencrow.ExecutionAlias != "fixture-alias" {
				t.Fatalf("%+v", req)
			}
			if err := req.Validate(false); err != nil {
				t.Fatalf("a measure request of a stage must satisfy ChatRequest: %v", err)
			}
			d, err := req.LogicalInputDigest()
			if err != nil {
				t.Fatal(err)
			}
			if err := req.WithExpected(d, strings.Repeat("a", 64), "bfp-v1:fixture").Validate(true); err != nil {
				t.Fatalf("a generation request of a stage must satisfy GenerationRequest: %v", err)
			}
		})
	}
	// Options come from the stage's own entry: the temperature of the selection stage is not the summary's.
	sel, _ := modelport.NewStageRequest(modelport.StageParams{Binding: binding, Descriptor: desc, Stage: modelport.StageSelection, Messages: fixture.Messages[:2], Meta: meta, Recovery: same})
	sum, _ := modelport.NewStageRequest(modelport.StageParams{Binding: binding, Descriptor: desc, Stage: modelport.StageSummary, Messages: fixture.Messages[:2], Meta: meta, Recovery: same})
	if sel.Temperature == nil || sum.Temperature != nil {
		t.Fatal("each stage has its own options")
	}
	retry := same
	retry.RetryOfRequestID, retry.TriggerCode = protocol.Str("req_x"), protocol.Str("REASONING_ONLY")
	for name, p := range map[string]modelport.StageParams{
		"act":               {Binding: binding, Descriptor: desc, Stage: modelport.StageAct, Messages: fixture.Messages[:2], Recovery: same},
		"a retry":           {Binding: binding, Descriptor: desc, Stage: modelport.StageSummary, Messages: fixture.Messages[:2], Recovery: retry},
		"the other profile": {Binding: binding, Descriptor: desc, Stage: modelport.StageSummary, Messages: fixture.Messages[:2], Recovery: modelport.RecoveryRequest{ProfileID: "terminal_output_once", ProfileRevision: "x"}},
		"no messages":       {Binding: binding, Descriptor: desc, Stage: modelport.StageSummary, Recovery: same},
		"no max_tokens":     {Binding: binding, Stage: modelport.StageSummary, Messages: fixture.Messages[:2], Recovery: same},
		"no selector":       {Descriptor: desc, Stage: modelport.StageSummary, Messages: fixture.Messages[:2], Recovery: same},
	} {
		if _, err := modelport.NewStageRequest(p); err == nil {
			t.Errorf("%s: a stage request was built", name)
		}
	}
}
