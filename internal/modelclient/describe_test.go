package modelclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// descriptorWire is the descriptor the Fake model publishes for the test binding,
// as a status entry.
func descriptorWire(t testing.TB, edit func(m map[string]any)) map[string]any {
	t.Helper()
	d, err := harnesstest.NewFake().Describe(context.Background(), testBinding)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(m)
	}
	return m
}

func advertisement(bindings ...any) map[string]any {
	if bindings == nil {
		bindings = []any{}
	}
	return map[string]any{
		"normalization": "strict-v1", "measure": true, "generation_retry": "disabled", "attempt_receipt": true, "explicit_recovery": true,
		"bindings": bindings,
	}
}

// status is a Gateway status with its legacy members and the given contracts.
func status(t testing.TB, contracts any) []byte {
	m := map[string]any{"status": "ok", "aliases": map[string]any{"mio": map[string]any{"state": "ready"}}, "model_routes": map[string]any{}}
	if contracts != nil {
		m["contracts"] = contracts
	}
	return mustJSON(t, m)
}

func TestDescribeReturnsTheDescriptorOfTheBinding(t *testing.T) {
	g := newGateway(t)
	other := descriptorWire(t, func(m map[string]any) {
		m["binding"].(map[string]any)["selector"] = "another-route"
		m["binding_fingerprint"] = "bfp-v1:" + strings.Repeat("9", 64)
	})
	g.reply(statusKey, 200, "application/json", status(t, map[string]any{"harness-v1": advertisement(other, descriptorWire(t, nil))}))

	got, err := g.client().Describe(t.Context(), testBinding)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := harnesstest.NewFake().Describe(context.Background(), testBinding)
	if got.BindingFingerprint != want.BindingFingerprint || got.Binding.Selector != testBinding.Selector ||
		got.Binding.ProfileRevision != testBinding.ProfileRevision || got.StageOptions.Act.MaxTokens != 4096 ||
		!got.Measure || got.Normalization != "strict-v1" {
		t.Fatalf("the descriptor is not the listed one: %+v", got)
	}
	if _, ok := got.RecoveryProfile(modelport.ProfileSameRequest, modelport.StageAct); !ok {
		t.Fatal("the recovery profiles were lost")
	}
	if g.count(statusKey) != 1 || g.requests() != 1 {
		t.Fatalf("Describe made %d requests, want exactly the one GET /status", g.requests())
	}
	if g.header(statusKey, 0).Get("Accept") != "application/json" {
		t.Error("Accept")
	}
}

func TestDescribeRefusesWhatTheStatusDoesNotPromise(t *testing.T) {
	ok := func(e func(m map[string]any)) any { return advertisement(descriptorWire(t, e)) }
	for _, tc := range []struct {
		name      string
		contracts any
		wantCode  string
	}{
		{"no contracts member", nil, "UNSUPPORTED_CONTRACT"},
		{"no harness-v1", map[string]any{"other-v9": map[string]any{}}, "UNSUPPORTED_CONTRACT"},
		{"empty contracts", map[string]any{}, "UNSUPPORTED_CONTRACT"},
		{"measure off", map[string]any{"harness-v1": func() any { a := advertisement(descriptorWire(t, nil)); a["measure"] = false; return a }()}, "UNSUPPORTED_CONTRACT"},
		{"receipts off", map[string]any{"harness-v1": func() any { a := advertisement(descriptorWire(t, nil)); a["attempt_receipt"] = false; return a }()}, "UNSUPPORTED_CONTRACT"},
		{"recovery off", map[string]any{"harness-v1": func() any { a := advertisement(descriptorWire(t, nil)); a["explicit_recovery"] = false; return a }()}, "UNSUPPORTED_CONTRACT"},
		{"generation retry on", map[string]any{"harness-v1": func() any { a := advertisement(descriptorWire(t, nil)); a["generation_retry"] = "enabled"; return a }()}, "UNSUPPORTED_CONTRACT"},
		{"another normalization", map[string]any{"harness-v1": func() any { a := advertisement(descriptorWire(t, nil)); a["normalization"] = "loose"; return a }()}, "UNSUPPORTED_CONTRACT"},
		{"binding not listed", map[string]any{"harness-v1": advertisement()}, "UNSUPPORTED_CONTRACT"},
		{"only another binding", map[string]any{"harness-v1": advertisement(descriptorWire(t, func(m map[string]any) { m["binding"].(map[string]any)["selector"] = "x" }))}, "UNSUPPORTED_CONTRACT"},
		{"same selector, another kind", map[string]any{"harness-v1": advertisement(descriptorWire(t, func(m map[string]any) { m["binding"].(map[string]any)["kind"] = "alias" }))}, "UNSUPPORTED_CONTRACT"},
		{"no bindings list", map[string]any{"harness-v1": func() any { a := advertisement(); delete(a, "bindings"); return a }()}, "MODEL_CONTRACT_FAILED"},
		{"contracts is a list", []any{}, "MODEL_CONTRACT_FAILED"},
		{"advertisement is a string", map[string]any{"harness-v1": "yes"}, "MODEL_CONTRACT_FAILED"},
		{"described twice", map[string]any{"harness-v1": advertisement(descriptorWire(t, nil), descriptorWire(t, nil))}, "MODEL_CONTRACT_FAILED"},
		{"extra descriptor field", map[string]any{"harness-v1": ok(func(m map[string]any) { m["hidden"] = true })}, "MODEL_CONTRACT_FAILED"},
		{"missing descriptor field", map[string]any{"harness-v1": ok(func(m map[string]any) { delete(m, "stage_options") })}, "MODEL_CONTRACT_FAILED"},
		{"act options without max_tokens", map[string]any{"harness-v1": ok(func(m map[string]any) { m["stage_options"].(map[string]any)["act"].(map[string]any)["max_tokens"] = 0 })}, "MODEL_CONTRACT_FAILED"},
		{"unverified fingerprint", map[string]any{"harness-v1": ok(func(m map[string]any) { m["binding_fingerprint"] = "unverified" })}, "MODEL_CONTRACT_FAILED"},
		{"normalization of the descriptor", map[string]any{"harness-v1": ok(func(m map[string]any) { m["normalization"] = "legacy" })}, "MODEL_CONTRACT_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateway(t)
			g.reply(statusKey, 200, "application/json", status(t, tc.contracts))
			_, err := g.client().Describe(t.Context(), testBinding)
			se := asStrict(t, err)
			if se.Code != tc.wantCode {
				t.Fatalf("code %s, want %s (%s)", se.Code, tc.wantCode, se.Message)
			}
			if g.requests() != 1 {
				t.Fatalf("%d requests", g.requests())
			}
			notLeaking(t, g, err.Error(), se.Message)
		})
	}
}

func TestDescribeIgnoresAnotherBindingsBadEntry(t *testing.T) {
	g := newGateway(t)
	bad := map[string]any{"binding": map[string]any{"kind": "alias", "selector": "broken"}, "junk": 1}
	g.reply(statusKey, 200, "application/json", status(t, map[string]any{"harness-v1": advertisement(bad, descriptorWire(t, nil))}))
	if _, err := g.client().Describe(t.Context(), testBinding); err != nil {
		t.Fatalf("a neighbour's malformed entry is not this binding's fault: %v", err)
	}
}

func TestDescribeNeedsAnAnswerThatIsStrictJSON(t *testing.T) {
	for name, body := range map[string]string{
		"not json":           "<html>gateway</html>",
		"duplicate key":      `{"contracts":{},"contracts":{}}`,
		"not an object":      `[1]`,
		"trailing data":      `{"contracts":{}} {}`,
		"duplicate in entry": `{"contracts":{"harness-v1":{"measure":true,"measure":true}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			g := newGateway(t)
			g.reply(statusKey, 200, "application/json", []byte(body))
			_, err := g.client().Describe(t.Context(), testBinding)
			if se := asStrict(t, err); se.Code != "MODEL_CONTRACT_FAILED" {
				t.Fatalf("code %s", se.Code)
			}
		})
	}
}

func TestDescribeFailuresThatAreNotRefusalsAreNotStrictErrors(t *testing.T) {
	t.Run("http status", func(t *testing.T) {
		g := newGateway(t)
		g.reply(statusKey, 503, "text/plain", []byte("down"))
		_, err := g.client().Describe(t.Context(), testBinding)
		var se *modelport.StrictError
		if err == nil || errors.As(err, &se) {
			t.Fatalf("a failing status is a plain error: %v", err)
		}
		notLeaking(t, g, err.Error())
	})
	t.Run("not found", func(t *testing.T) {
		g := newGateway(t) // no handler: 404
		_, err := g.client().Describe(t.Context(), testBinding)
		var se *modelport.StrictError
		if err == nil || errors.As(err, &se) {
			t.Fatalf("a 404 is a plain error: %v", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		g := newGateway(t)
		c := g.client()
		g.srv.Close()
		_, err := c.Describe(t.Context(), testBinding)
		var te *TransportError
		if !errors.As(err, &te) || te.Connected {
			t.Fatalf("want a transport error without a connection: %v", err)
		}
		notLeaking(t, g, err.Error())
	})
	t.Run("cancelled", func(t *testing.T) {
		g := newGateway(t)
		g.on(statusKey, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			for g.count(statusKey) == 0 {
				time.Sleep(time.Millisecond)
			}
			cancel()
		}()
		_, err := g.client().Describe(ctx, testBinding)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled: %v", err)
		}
		if g.count(statusKey) != 1 {
			t.Fatalf("%d requests", g.count(statusKey))
		}
	})
}

// An alias binding as RenCrow_LLM describes it: its Agent and role, stage options
// with sampled values and a stop list, and both recovery profiles.
func TestDescribeReadsAnAliasDescriptorAsTheGatewayWritesIt(t *testing.T) {
	alias := protocol.Binding{Kind: "alias", Selector: "coder3", ProfileRevision: "coder-r1", AgentID: modelport.Str("shiro"), ExecutionRole: modelport.Str("coder")}
	entry := []byte(`{"binding":{"kind":"alias","selector":"coder3","profile_revision":"coder-r1","agent_id":"shiro","execution_role":"coder"},` +
		`"binding_fingerprint":"bfp-v1:` + strings.Repeat("c", 64) + `",` +
		`"stage_options":{"act":{"max_tokens":4096,"temperature":0.7,"top_p":0.95,"seed":42,"stop":["</s>"]},` +
		`"instruction_selection":{"max_tokens":1024,"temperature":0,"top_p":null,"seed":null,"stop":[]},` +
		`"work_summary":{"max_tokens":2048,"temperature":null,"top_p":1.0,"seed":0,"stop":[]}},` +
		`"recovery_profiles":[{"profile_id":"same_request","profile_revision":"builtin-v1","stages":["act","instruction_selection","work_summary"],"transformations":[],"codes":["CONNECT_FAILED","UPSTREAM_TRANSIENT","RATE_LIMITED","QUEUE_TIMEOUT","REASONING_ONLY","EMPTY_FINAL_CONTENT","RAW_TOOL_MARKUP","MODEL_OUTPUT_SCHEMA_INVALID"]},` +
		`{"profile_id":"terminal_output_once","profile_revision":"qwen-r2","stages":["act"],"transformations":["format_suffix","thinking_disabled"],"codes":["REASONING_ONLY","EMPTY_FINAL_CONTENT","RAW_TOOL_MARKUP","MODEL_OUTPUT_SCHEMA_INVALID"]}],` +
		`"measure":true,"normalization":"strict-v1"}`)
	body := []byte(`{"status":"ok","contracts":{"harness-v1":{"normalization":"strict-v1","measure":true,"generation_retry":"disabled","attempt_receipt":true,"explicit_recovery":true,"bindings":[` + string(entry) + `]}}}`)
	g := newGateway(t)
	g.reply(statusKey, 200, "application/json", body)

	d, err := g.client().Describe(t.Context(), alias)
	if err != nil {
		t.Fatal(err)
	}
	act := d.StageOptions.Act
	if *d.Binding.AgentID != "shiro" || *d.Binding.ExecutionRole != "coder" || act.MaxTokens != 4096 || act.Temperature.String() != "0.7" || act.TopP.String() != "0.95" ||
		*act.Seed != 42 || len(act.Stop) != 1 || act.Stop[0] != "</s>" {
		t.Fatalf("%+v", act)
	}
	sel, sum := d.StageOptions.InstructionSelection, d.StageOptions.WorkSummary
	if sel.Temperature.String() != "0" || sel.TopP != nil || sel.Seed != nil || sum.Temperature != nil || sum.TopP.String() != "1" || *sum.Seed != 0 {
		t.Fatalf("null and zero options must stay distinct: %+v %+v", sel, sum)
	}
	p, ok := d.RecoveryProfile(modelport.ProfileTerminalOutputOnce, modelport.StageAct)
	if !ok || p.ProfileRevision != "qwen-r2" || len(p.Transformations) != 2 {
		t.Fatalf("%+v", p)
	}
	if _, ok := d.RecoveryProfile(modelport.ProfileTerminalOutputOnce, modelport.StageSummary); ok {
		t.Fatal("terminal_output_once is act only")
	}
}
