package tools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolerr"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

const evidenceID = "evd_01a1151c-5a6f-7e06-996b-c21d45447041"

func policy(mode string, mut func(*config.EffectivePolicy)) *RunPolicy {
	ep := config.EffectivePolicy{
		Policy: config.Policy{
			ID: "p", AllowedModes: []string{"structured_only", "trusted_host"}, Tools: AllTools(), ReadPrefixes: []string{"."}, WritePrefixes: []string{"."},
			ProcessProfiles: []string{"go-test", "go-any"}, EnvProfiles: []string{"clean", "ci"},
		},
		ProcessProfiles: []config.ProcessProfile{
			{Name: "go-test", Executable: "/opt/go", ArgvPrefix: []string{"test"}},
			{Name: "sh", Executable: "/bin/sh", IsShell: true, ArgvPrefix: []string{"-c"}},
		},
		EnvProfiles:   []config.EnvProfile{{Name: "clean", Values: map[string]string{}}, {Name: "ci", Values: map[string]string{"CI": "1"}}},
		WorkspaceRoot: "/ws", Mode: mode, Revision: "rev-1",
	}
	ep.Policy.ProcessProfiles = []string{"go-test", "sh"}
	if mut != nil {
		mut(&ep)
	}
	return newRunPolicy(ep)
}

func TestTheCatalogIsWhatThePolicyAndTheModeLeave(t *testing.T) {
	all := []string{"evidence.read", "file.create", "file.edit", "file.read", "file.search", "process.exec"}
	if got := policy("trusted_host", nil).Names(); strings.Join(got, ",") != strings.Join(all, ",") {
		t.Fatalf("trusted_host: %v", got)
	}
	// structured_only never offers process.exec, whatever the policy lists.
	if got := policy("structured_only", nil).Names(); strings.Join(got, ",") != "evidence.read,file.create,file.edit,file.read,file.search" {
		t.Fatalf("structured_only: %v", got)
	}
	// process.exec needs a process profile.
	if p := policy("trusted_host", func(ep *config.EffectivePolicy) { ep.ProcessProfiles, ep.Policy.ProcessProfiles = nil, nil }); p.Enabled(ProcessExec) {
		t.Fatal("process.exec was offered with no profile to run")
	}
	// A Tool the policy does not list is not offered.
	p := policy("trusted_host", func(ep *config.EffectivePolicy) { ep.Policy.Tools = []string{"file.read", "evidence.read"} })
	if strings.Join(p.Names(), ",") != "evidence.read,file.read" {
		t.Fatalf("%v", p.Names())
	}
	// A Tool that needs a prefix the policy does not give has nothing to work on.
	p = policy("trusted_host", func(ep *config.EffectivePolicy) { ep.Policy.WritePrefixes = nil })
	if p.Enabled(FileCreate) || p.Enabled(FileEdit) || !p.Enabled(FileRead) || !p.Enabled(ProcessExec) || !p.CanMutate() {
		t.Fatalf("%v", p.Names())
	}
	p = policy("structured_only", func(ep *config.EffectivePolicy) { ep.Policy.WritePrefixes = nil })
	if p.CanMutate() {
		t.Fatal("a run that can only read holds no workspace lock")
	}
	p = policy("trusted_host", func(ep *config.EffectivePolicy) { ep.Policy.ReadPrefixes = nil })
	if p.Enabled(FileRead) || p.Enabled(FileSearch) || !p.Enabled(EvidenceRead) {
		t.Fatalf("%v", p.Names())
	}
	// No Tool at all: an empty catalog, which is a request that offers none.
	p = policy("structured_only", func(ep *config.EffectivePolicy) { ep.Policy.Tools = nil })
	if cat, err := p.Catalog(); err != nil || cat == nil || len(cat) != 0 {
		t.Fatalf("%v %v", cat, err)
	}
}

func TestEveryDeclarationIsTheDesignSchemaAndAValidFunctionTool(t *testing.T) {
	cat, err := policy("trusted_host", nil).Catalog()
	if err != nil || len(cat) != 6 {
		t.Fatalf("%v %v", cat, err)
	}
	var design struct {
		OneOf []struct {
			Properties struct {
				Name      struct{ Const string } `json:"name"`
				Arguments json.RawMessage        `json:"arguments"`
			} `json:"properties"`
		} `json:"oneOf"`
	}
	raw := readSchema(t)
	if err := json.Unmarshal(raw, &design); err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, o := range design.OneOf {
		byName[o.Properties.Name.Const] = canonicalText(t, o.Properties.Arguments)
	}
	for _, d := range cat {
		if d.Type != "function" || !d.Function.Strict || d.Function.Description == "" || len(d.Function.Description) > 8192 {
			t.Errorf("%s: %+v", d.Function.Name, d)
		}
		if canonicalText(t, d.Function.Parameters) != byName[d.Function.Name] {
			t.Errorf("%s: the declaration is not the design package's arguments schema", d.Function.Name)
		}
		body, _ := json.Marshal(d)
		v, err := strictjson.Decode(body)
		if err != nil {
			t.Fatal(err)
		}
		if err := schemacheck.Validate(schemacheck.LLMContract, "FunctionTool", v); err != nil {
			t.Errorf("%s: %v", d.Function.Name, err)
		}
		if rev, ok := SchemaRevision(d.Function.Name); !ok || len(rev) != 64 {
			t.Errorf("%s: revision %q", d.Function.Name, rev)
		}
	}
	// The order is the same every time: the request's digest depends on it.
	again, _ := policy("trusted_host", nil).Catalog()
	a, _ := json.Marshal(cat)
	b, _ := json.Marshal(again)
	if string(a) != string(b) {
		t.Fatal("the catalog is not deterministic")
	}
	if _, ok := SchemaRevision("file.rename"); ok {
		t.Fatal("a revision for a Tool that does not exist")
	}
}

func intent(i int, id, name, args string) modelport.ToolIntent {
	return modelport.ToolIntent{ProviderToolCallID: id, Ordinal: int64(i), Name: name, ArgumentsJSON: args}
}

func TestValidCallsOfEveryToolAreAcceptedWithTheirCanonicalArguments(t *testing.T) {
	p := policy("trusted_host", nil)
	hash := strings.Repeat("a", 64)
	calls := []modelport.ToolIntent{
		intent(0, "c0", FileRead, `{"max_bytes":10,"path":"a.txt","range":{"end":10,"start":0}}`),
		intent(1, "c1", FileSearch, `{"path":".","query":"x","mode":"regex","max_results":5,"max_bytes":1000}`),
		intent(2, "c2", FileCreate, `{"path":"n.txt","text":"日本語\n","expected_absent":true}`),
		intent(3, "c3", FileEdit, `{"path":"a.txt","expected_hash":"`+hash+`","old_text":"a","new_text":""}`),
		intent(4, "c4", ProcessExec, `{"executable":"/opt/go","argv":["test","./..."],"cwd":".","env_profile_ref":"clean","timeout_seconds":60}`),
		intent(5, "c5", EvidenceRead, `{"evidence_id":"`+evidenceID+`","projection_version":"text/v1","range":{"start":0,"end":65536}}`),
	}
	got, err := p.ValidateCalls(calls)
	if err != nil || len(got) != 6 {
		t.Fatalf("%v %v", got, err)
	}
	for i, c := range got {
		if c.Name != calls[i].Name || c.ProviderToolCallID != calls[i].ProviderToolCallID || c.Ordinal != int64(i) || c.ArgumentsText != calls[i].ArgumentsJSON || len(c.SchemaRevision) != 64 || c.args == nil {
			t.Errorf("%d: %+v", i, c)
		}
	}
	// The stored arguments are canonical: the same call with its keys in another order
	// is the same bytes (and so the same args_hash).
	other, err := p.ValidateCalls([]modelport.ToolIntent{intent(0, "x", FileRead, `{"path":"a.txt","range":{"start":0,"end":10},"max_bytes":10}`)})
	if err != nil || string(other[0].ArgsBytes) != string(got[0].ArgsBytes) {
		t.Fatalf("%s vs %s (%v)", other[0].ArgsBytes, got[0].ArgsBytes, err)
	}
	if string(got[0].ArgsBytes) != `{"max_bytes":10,"path":"a.txt","range":{"end":10,"start":0}}` {
		t.Fatalf("%s", got[0].ArgsBytes)
	}
}

func TestOneBadCallRefusesTheWholeResponse(t *testing.T) {
	p := policy("structured_only", nil)
	hash := strings.Repeat("a", 64)
	good := intent(0, "c0", FileRead, `{"path":"a.txt","range":{"start":0,"end":10},"max_bytes":10}`)
	cases := []struct {
		name string
		bad  modelport.ToolIntent
		kind string
	}{
		{"a Tool the request did not declare", intent(1, "c1", ProcessExec, `{"executable":"/bin/ls","argv":[],"cwd":".","env_profile_ref":"clean","timeout_seconds":1}`), InvalidUndeclared},
		{"a Tool that does not exist", intent(1, "c1", "file.delete", `{"path":"a"}`), InvalidUndeclared},
		{"a missing argument", intent(1, "c1", FileRead, `{"path":"a.txt","max_bytes":10}`), InvalidArguments},
		{"an argument that is not in the schema", intent(1, "c1", FileRead, `{"path":"a.txt","range":{"start":0,"end":1},"max_bytes":10,"mode":"r"}`), InvalidArguments},
		{"a wrong type", intent(1, "c1", FileRead, `{"path":7,"range":{"start":0,"end":1},"max_bytes":10}`), InvalidArguments},
		{"a number past its maximum", intent(1, "c1", FileRead, `{"path":"a","range":{"start":0,"end":1},"max_bytes":65537}`), InvalidArguments},
		{"a range that runs backwards", intent(1, "c1", FileRead, `{"path":"a","range":{"start":5,"end":1},"max_bytes":10}`), InvalidArguments},
		{"an empty path", intent(1, "c1", FileRead, `{"path":"","range":{"start":0,"end":1},"max_bytes":10}`), InvalidArguments},
		{"expected_absent that is false", intent(1, "c1", FileCreate, `{"path":"a","text":"x","expected_absent":false}`), InvalidArguments},
		{"a hash that is not a SHA-256", intent(1, "c1", FileEdit, `{"path":"a","expected_hash":"`+hash[:63]+`","old_text":"a","new_text":"b"}`), InvalidArguments},
		{"an empty old_text", intent(1, "c1", FileEdit, `{"path":"a","expected_hash":"`+hash+`","old_text":"","new_text":"b"}`), InvalidArguments},
		{"a search mode that is neither", intent(1, "c1", FileSearch, `{"path":".","query":"x","mode":"fuzzy","max_results":5,"max_bytes":1000}`), InvalidArguments},
		{"an evidence ID that is not canonical", intent(1, "c1", EvidenceRead, `{"evidence_id":"evd_nope","projection_version":"raw/v1","range":{"start":0,"end":1}}`), InvalidArguments},
		{"a projection that does not exist", intent(1, "c1", EvidenceRead, `{"evidence_id":"`+evidenceID+`","projection_version":"html/v1","range":{"start":0,"end":1}}`), InvalidArguments},
		{"a duplicate key", intent(1, "c1", FileRead, `{"path":"a","path":"b","range":{"start":0,"end":1},"max_bytes":10}`), InvalidArguments},
		{"arguments that are not an object", intent(1, "c1", FileRead, `["a"]`), InvalidArguments},
		{"arguments that are not JSON", intent(1, "c1", FileRead, `{"path":`), InvalidArguments},
		{"a call ID used twice", intent(1, "c0", FileRead, `{"path":"a","range":{"start":0,"end":1},"max_bytes":10}`), InvalidArguments},
		{"a call with no ID", intent(1, "", FileRead, `{"path":"a","range":{"start":0,"end":1},"max_bytes":10}`), InvalidArguments},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The good call comes first: it is not run, and not kept, for the bad one after it.
			got, err := p.ValidateCalls([]modelport.ToolIntent{good, tc.bad})
			var ie *InvalidError
			if !errors.As(err, &ie) || ie.Kind != tc.kind || ie.Ordinal != 1 || got != nil {
				t.Fatalf("%v %v", got, err)
			}
			if strings.Contains(ie.Error(), "a.txt") {
				t.Fatalf("an argument leaked into the error: %v", ie)
			}
		})
	}
	if _, err := p.ValidateCalls(nil); err == nil {
		t.Fatal("a response with no call was accepted")
	}
}

func TestThePolicyDecidesBeforeAnythingIsBound(t *testing.T) {
	p := policy("trusted_host", func(ep *config.EffectivePolicy) {
		ep.Policy.ReadPrefixes = []string{"src", "docs"}
		ep.Policy.WritePrefixes = []string{"out"}
		ep.Policy.EnvProfiles = []string{"clean"}
		ep.ProcessProfiles = append(ep.ProcessProfiles, config.ProcessProfile{Name: "go-any", Executable: "/opt/go"}, config.ProcessProfile{Name: "twin-a", Executable: "/opt/twin"}, config.ProcessProfile{Name: "twin-b", Executable: "/opt/twin"})
		ep.Policy.ProcessProfiles = []string{"go-test", "go-any", "sh", "twin-a", "twin-b"}
	})
	r := &RunTools{pol: p}
	check := func(name string, raw string, want string) {
		t.Helper()
		calls, err := p.ValidateCalls([]modelport.ToolIntent{intent(0, "c", name, raw)})
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		_, te := r.decide(calls[0])
		switch {
		case want == "" && te != nil:
			t.Errorf("%s: refused: %v", raw, te)
		case want != "" && (te == nil || te.Code != want || te.Class != toolerr.Rejected):
			t.Errorf("%s: %v, want %s", raw, te, want)
		}
	}
	rng := `"range":{"start":0,"end":1},"max_bytes":10`
	check(FileRead, `{"path":"src/a.go",`+rng+`}`, "")
	check(FileRead, `{"path":"out/a.go",`+rng+`}`, toolerr.CodePathOutside)
	check(FileRead, `{"path":"../x",`+rng+`}`, toolerr.CodePathInvalid)
	check(FileRead, `{"path":"/etc/passwd",`+rng+`}`, toolerr.CodePathInvalid)
	check(FileSearch, `{"path":".","query":"x","mode":"literal","max_results":1,"max_bytes":10}`, "") // above the prefixes: the walk confines it
	check(FileSearch, `{"path":"out","query":"x","mode":"literal","max_results":1,"max_bytes":10}`, toolerr.CodePathOutside)
	check(FileSearch, `{"path":"src","query":"(","mode":"regex","max_results":1,"max_bytes":10}`, toolerr.CodeRegexInvalid)
	check(FileCreate, `{"path":"out/n.txt","text":"x","expected_absent":true}`, "")
	check(FileCreate, `{"path":"src/n.txt","text":"x","expected_absent":true}`, toolerr.CodePathOutside) // read prefix only
	check(FileEdit, `{"path":"src/a.go","expected_hash":"`+strings.Repeat("a", 64)+`","old_text":"a","new_text":"b"}`, toolerr.CodePathOutside)
	// process.exec: one profile, an environment the policy allows, a directory it covers.
	exec := func(exe, argv, env, cwd string) string {
		return `{"executable":"` + exe + `","argv":` + argv + `,"cwd":"` + cwd + `","env_profile_ref":"` + env + `","timeout_seconds":5}`
	}
	check(ProcessExec, exec("/opt/go", `["build"]`, "clean", "src"), "")
	check(ProcessExec, exec("/opt/go", `["test"]`, "clean", "src"), toolerr.CodePolicyAmbiguous) // go-test and go-any both match
	check(ProcessExec, exec("/opt/twin", `[]`, "clean", "src"), toolerr.CodePolicyAmbiguous)
	check(ProcessExec, exec("/opt/unknown", `[]`, "clean", "src"), toolerr.CodePolicyRejected)
	check(ProcessExec, exec("/bin/sh", `["-c","echo hi"]`, "clean", "out"), "")
	check(ProcessExec, exec("/bin/sh", `["echo hi"]`, "clean", "out"), toolerr.CodePolicyRejected)
	check(ProcessExec, exec("/opt/go", `["build"]`, "ci", "src"), toolerr.CodeEnvUnknown)       // defined, but not granted by the policy
	check(ProcessExec, exec("/opt/go", `["build"]`, "nonesuch", "src"), toolerr.CodeEnvUnknown) // not defined at all
	check(ProcessExec, exec("/opt/go", `["build"]`, "clean", "elsewhere"), toolerr.CodePathOutside)
	check(ProcessExec, exec("/opt/go", `["build"]`, "clean", ".."), toolerr.CodePathInvalid)
	check(EvidenceRead, `{"evidence_id":"`+evidenceID+`","projection_version":"raw/v1","range":{"start":0,"end":1}}`, "")

	// structured_only has no process.exec: asking for it is refused even when the call
	// is built by hand, without going through the declared catalog.
	so := &RunTools{pol: policy("structured_only", nil)}
	_, te := so.decide(Call{Name: ProcessExec, args: execArgs{Executable: "/opt/go", Cwd: "."}})
	if te == nil || te.Code != toolerr.CodeToolNotAllowed {
		t.Fatalf("%v", te)
	}
	// The mode is checked again where the profile is resolved.
	forced := policy("structured_only", nil)
	forced.enabled = append(forced.enabled, ProcessExec)
	_, te = (&RunTools{pol: forced}).decide(Call{Name: ProcessExec, args: execArgs{Executable: "/opt/go", Cwd: ".", EnvProfileRef: "clean"}})
	if te == nil || te.Code != toolerr.CodeModeUnavailable {
		t.Fatalf("%v", te)
	}
}

func TestAResponsesCallsNeverReachTheStoreAsATypedValueTheyDidNotValidate(t *testing.T) {
	// The Tool is identified by name only after the schema accepted the arguments of that
	// very Tool: arguments valid for one Tool do not pass under another's name.
	p := policy("trusted_host", nil)
	if _, err := p.ValidateCalls([]modelport.ToolIntent{intent(0, "c", FileCreate, `{"path":"a","range":{"start":0,"end":1},"max_bytes":10}`)}); err == nil {
		t.Fatal("file.read's arguments were accepted as file.create's")
	}
	_ = protocol.ModeTrustedHost
}
