package config_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
)

func TestPolicyRegistryRejections(t *testing.T) {
	cases := map[string]func(f *fixture){
		"unknown format":        func(f *fixture) { f.reg["format_version"] = "rencrow-policy-registry/v2" },
		"unknown registry key":  func(f *fixture) { f.reg["note"] = "x" },
		"unknown policy key":    func(f *fixture) { f.policy()["inherit"] = "base" },
		"no policies":           func(f *fixture) { f.reg["policies"] = []any{} },
		"duplicate id":          func(f *fixture) { f.reg["policies"] = append(f.reg["policies"].([]any), f.policy()) },
		"unknown tool":          func(f *fixture) { f.policy()["tools"] = []any{"file.delete"} },
		"unknown mode":          func(f *fixture) { f.policy()["allowed_modes"] = []any{"sandboxed"} },
		"duplicate tool":        func(f *fixture) { f.policy()["tools"] = []any{"file.read", "file.read"} },
		"duplicate mode":        func(f *fixture) { f.policy()["allowed_modes"] = []any{"trusted_host", "trusted_host"} },
		"duplicate read prefix": func(f *fixture) { f.policy()["read_prefixes"] = []any{"src", "src"} },
		"duplicate env profile": func(f *fixture) { f.policy()["env_profiles"] = []any{"clean", "clean"} },
		"missing limits":        func(f *fixture) { delete(f.policy(), "limits") },
		"zero limit":            func(f *fixture) { f.policy()["limits"].(map[string]any)["max_model_steps"] = 0 },
		"workspace policy_ref is not registered": func(f *fixture) {
			f.workspace()["policy_ref"] = "other"
		},
		"policy_ref matches only by prefix": func(f *fixture) { f.workspace()["policy_ref"] = "fixture-workspace" },
		"policy_ref differs by case":        func(f *fixture) { f.workspace()["policy_ref"] = "Fixture-Workspace-Write" },
		"policy_ref is a path":              func(f *fixture) { f.workspace()["policy_ref"] = "./policies.json" },
		"no mode in common": func(f *fixture) {
			f.workspace()["allowed_modes"] = []any{"isolated"}
		},
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			mod(f)
			_, err := load(t, f)
			wantInvalid(t, err)
		})
	}
}

func TestPolicyRegistryFileConditions(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		f := newFixture(t)
		f.cfg["policy_registry_path"] = f.Secrets + "/absent.json"
		f.writeJSON(f.Config, f.cfg)
		_, err := config.Load(f.Config)
		wantInvalid(t, err)
	})
	t.Run("malformed registry", func(t *testing.T) {
		for name, content := range map[string]string{
			"duplicate key": `{"format_version":"rencrow-policy-registry/v1","format_version":"x","policies":[]}`,
			"not json":      `policies: []`,
			"bom":           "\xef\xbb\xbf{}",
			"empty":         ``,
		} {
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				f.writeJSON(f.Config, f.cfg)
				f.writeBytes(f.Registry, []byte(content))
				_, err := config.Load(f.Config)
				wantInvalid(t, err)
			})
		}
	})
	t.Run("a directory", func(t *testing.T) {
		f := newFixture(t)
		f.cfg["policy_registry_path"] = f.Secrets
		_, err := load(t, f)
		wantInvalid(t, err)
	})
}

func TestPolicyPrefixes(t *testing.T) {
	good := []string{".", "src", "src/pkg", "docs/a-b_c.d", "a b/c", "日本語/ディレクトリ", ".hidden", "a..b", "..a", "a.."}
	bad := map[string]string{
		"empty":           "",
		"absolute":        "/etc",
		"windows drive":   "C:/Users",
		"windows drive2":  "c:",
		"backslash":       `src\pkg`,
		"unc":             `\\server\share`,
		"dot dot":         "..",
		"dot dot inside":  "src/../etc",
		"dot dot leading": "../src",
		"dot dot ending":  "src/..",
		"empty segment":   "a//b",
		"trailing slash":  "src/",
		"leading slash":   "/src",
		"dot segment":     "src/./pkg",
		"leading dot":     "./src",
		"only slash":      "/",
		"nul":             "a\x00b",
		"newline":         "a\nb",
		"tab":             "a\tb",
	}
	for _, p := range good {
		if err := config.ValidatePrefix(p); err != nil {
			t.Errorf("%q refused: %v", p, err)
		}
	}
	for name, p := range bad {
		if err := config.ValidatePrefix(p); !errors.Is(err, config.ErrInvalid) {
			t.Errorf("%s (%q) accepted or not ErrInvalid: %v", name, p, err)
		}
	}
	// And the same rule applies to what the registry file says, for both lists.
	for name, p := range bad {
		if name == "empty" {
			continue
		}
		for _, field := range []string{"read_prefixes", "write_prefixes"} {
			t.Run(field+"/"+name, func(t *testing.T) {
				f := newFixture(t)
				f.policy()[field] = []any{p}
				_, err := load(t, f)
				wantInvalid(t, err)
			})
		}
	}
	// An empty array is valid: it grants no path at all.
	f := newFixture(t)
	f.policy()["read_prefixes"] = []any{}
	f.policy()["write_prefixes"] = []any{}
	mustLoad(t, f)
}

func TestModeResolutionNeverFallsBack(t *testing.T) {
	f := newFixture(t)
	d := mustLoad(t, f)
	for mode, ok := range map[string]bool{"structured_only": true, "trusted_host": true} {
		got, err := d.ResolveMode(f.Work, "fixture-workspace-write", mode)
		if err != nil || got != mode || !ok {
			t.Fatalf("%s: %q %v", mode, got, err)
		}
	}
	// isolated is in neither allowed list of this fixture, so it is refused, and
	// the refusal does not become structured_only.
	got, err := d.ResolveMode(f.Work, "fixture-workspace-write", "isolated")
	if err == nil || got != "" {
		t.Fatalf("isolated not allowed here: %q %v", got, err)
	}
	if _, err := d.ResolveMode(f.Work, "fixture-workspace-write", "yolo"); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	if _, err := d.ResolveMode(f.Work, "fixture-workspace-write", ""); err == nil {
		t.Fatal("no requested mode is not a default to pick")
	}
	if _, err := d.ResolveMode(f.Dir, "fixture-workspace-write", "trusted_host"); err == nil {
		t.Fatal("a root that is not a configured workspace was accepted")
	}
	if _, err := d.ResolveMode(f.Work, "not-registered", "trusted_host"); !errors.Is(err, config.ErrUnregisteredPolicy) {
		t.Fatalf("%v", err)
	}
	// A mode both lists allow is still unavailable until an isolation adapter exists.
	f2 := newFixture(t)
	f2.workspace()["allowed_modes"] = []any{"structured_only", "isolated"}
	f2.policy()["allowed_modes"] = []any{"structured_only", "isolated"}
	d2 := mustLoad(t, f2)
	if _, err := d2.ResolveMode(f2.Work, "fixture-workspace-write", "isolated"); !errors.Is(err, config.ErrModeUnavailable) {
		t.Fatalf("isolated must be unavailable without an adapter: %v", err)
	}
	// A workspace that restricts to structured_only refuses trusted_host even if the policy allows it.
	f3 := newFixture(t)
	f3.workspace()["allowed_modes"] = []any{"structured_only"}
	d3 := mustLoad(t, f3)
	if _, err := d3.ResolveMode(f3.Work, "fixture-workspace-write", "trusted_host"); err == nil {
		t.Fatal("the intersection of workspace and policy modes was not applied")
	}
}

// Expected values below were computed independently of the repository codec:
// SHA-256 over LP("rencrow-effective-policy/v1") + LP(CJ1(value)) with the value
// {policy, process_profiles, env_profiles, workspace_root, mode}, keys in byte
// order, every set sorted by name or by UTF-8 order.
const (
	revisionClean = "9fc0a94fcea13672067389ea531fba4bff561286a0b74cdfd22a2680cc22fb25"
	revisionBuild = "a4a2df49d276530214738ce4089356cb51116e5daac5c662f7623e0e8373c370"
)

func cleanInput() config.EffectivePolicyInput {
	return config.EffectivePolicyInput{
		Policy: config.Policy{
			ID:              "fixture-workspace-write",
			AllowedModes:    []string{"trusted_host", "structured_only"},
			Tools:           []string{"file.search", "file.read", "evidence.read", "file.edit", "file.create"},
			ReadPrefixes:    []string{"."},
			WritePrefixes:   []string{"."},
			ProcessProfiles: []string{},
			EnvProfiles:     []string{"clean"},
			Limits:          limits10(),
		},
		EnvProfiles:   []config.EnvProfile{{Name: "clean", Values: map[string]string{}}},
		WorkspaceRoot: "/ws",
		Mode:          "trusted_host",
	}
}

func TestEffectivePolicyRevisionGolden(t *testing.T) {
	got, err := config.EffectivePolicyRevision(cleanInput())
	if err != nil || got != revisionClean {
		t.Fatalf("got %s (%v), want %s", got, err, revisionClean)
	}
	in := cleanInput()
	in.Policy.Tools = []string{"evidence.read", "file.read", "process.exec"}
	in.Policy.ReadPrefixes = []string{"src", "docs"}
	in.Policy.WritePrefixes = []string{"src"}
	in.Policy.ProcessProfiles = []string{"make", "go"}
	in.Policy.EnvProfiles = []string{"clean", "build"}
	in.ProcessProfiles = []config.ProcessProfile{
		{Name: "make", Executable: "/opt/fixture/bin/make", IsShell: false, ArgvPrefix: []string{}},
		{Name: "go", Executable: "/opt/fixture/bin/go", IsShell: false, ArgvPrefix: []string{"go", "test"}},
	}
	in.EnvProfiles = []config.EnvProfile{
		{Name: "clean", Values: map[string]string{}},
		{Name: "build", Values: map[string]string{"LANG": "C.UTF-8", "GOFLAGS": "-mod=mod"}},
	}
	got, err = config.EffectivePolicyRevision(in)
	if err != nil || got != revisionBuild {
		t.Fatalf("got %s (%v), want %s", got, err, revisionBuild)
	}
}

func TestEffectivePolicyRevisionIgnoresSetOrderOnly(t *testing.T) {
	base, _ := config.EffectivePolicyRevision(cleanInput())
	shuffled := cleanInput()
	shuffled.Policy.Tools = []string{"evidence.read", "file.create", "file.edit", "file.read", "file.search"}
	shuffled.Policy.AllowedModes = []string{"structured_only", "trusted_host"}
	if got, err := config.EffectivePolicyRevision(shuffled); err != nil || got != base {
		t.Fatalf("set order changed the revision: %s %v", got, err)
	}
	// Anything that changes what is allowed changes the revision.
	changes := map[string]func(in *config.EffectivePolicyInput){
		"mode":           func(in *config.EffectivePolicyInput) { in.Mode = "structured_only" },
		"workspace root": func(in *config.EffectivePolicyInput) { in.WorkspaceRoot = "/ws2" },
		"a tool":         func(in *config.EffectivePolicyInput) { in.Policy.Tools = in.Policy.Tools[1:] },
		"a read prefix":  func(in *config.EffectivePolicyInput) { in.Policy.ReadPrefixes = []string{"src"} },
		"a write prefix": func(in *config.EffectivePolicyInput) { in.Policy.WritePrefixes = []string{} },
		"a limit":        func(in *config.EffectivePolicyInput) { in.Policy.Limits.MaxModelSteps++ },
		"an env value":   func(in *config.EffectivePolicyInput) { in.EnvProfiles[0].Values["PATH"] = "/bin" },
		"the policy id":  func(in *config.EffectivePolicyInput) { in.Policy.ID = "other" },
		"allowed modes":  func(in *config.EffectivePolicyInput) { in.Policy.AllowedModes = []string{"trusted_host"} },
	}
	for name, mod := range changes {
		in := cleanInput()
		in.EnvProfiles = []config.EnvProfile{{Name: "clean", Values: map[string]string{}}}
		mod(&in)
		if got, err := config.EffectivePolicyRevision(in); err != nil || got == base {
			t.Errorf("changing %s did not change the revision (%v)", name, err)
		}
	}
	// argv order is meaning, not a set: reordering the prefix changes the revision.
	in := cleanInput()
	in.Policy.ProcessProfiles = []string{"go"}
	in.ProcessProfiles = []config.ProcessProfile{{Name: "go", Executable: "/opt/fixture/bin/go", ArgvPrefix: []string{"go", "test"}}}
	a, _ := config.EffectivePolicyRevision(in)
	in.ProcessProfiles[0].ArgvPrefix = []string{"test", "go"}
	b, _ := config.EffectivePolicyRevision(in)
	if a == b {
		t.Fatal("argv_prefix order must be part of the revision")
	}
}

func TestEffectivePolicyRevisionRefusals(t *testing.T) {
	for name, mod := range map[string]func(in *config.EffectivePolicyInput){
		"duplicate tool": func(in *config.EffectivePolicyInput) { in.Policy.Tools = append(in.Policy.Tools, "file.read") },
		"duplicate mode": func(in *config.EffectivePolicyInput) {
			in.Policy.AllowedModes = []string{"trusted_host", "trusted_host"}
		},
		"duplicate prefix":       func(in *config.EffectivePolicyInput) { in.Policy.ReadPrefixes = []string{"a", "a"} },
		"duplicate env profile":  func(in *config.EffectivePolicyInput) { in.EnvProfiles = append(in.EnvProfiles, in.EnvProfiles[0]) },
		"missing env definition": func(in *config.EffectivePolicyInput) { in.EnvProfiles = nil },
		"extra env definition": func(in *config.EffectivePolicyInput) {
			in.EnvProfiles = append(in.EnvProfiles, config.EnvProfile{Name: "other", Values: map[string]string{}})
		},
		"missing process definition": func(in *config.EffectivePolicyInput) { in.Policy.ProcessProfiles = []string{"go"} },
		"invalid prefix":             func(in *config.EffectivePolicyInput) { in.Policy.ReadPrefixes = []string{"../x"} },
		"mode outside the policy":    func(in *config.EffectivePolicyInput) { in.Mode = "isolated" },
		"relative workspace root":    func(in *config.EffectivePolicyInput) { in.WorkspaceRoot = "ws" },
	} {
		t.Run(name, func(t *testing.T) {
			in := cleanInput()
			mod(&in)
			if _, err := config.EffectivePolicyRevision(in); !errors.Is(err, config.ErrInvalid) {
				t.Fatalf("accepted or not ErrInvalid: %v", err)
			}
		})
	}
}

func TestDeploymentPolicyRevisionResolvesReferences(t *testing.T) {
	f := newFixture(t)
	f.cfg["process_profiles"] = []any{map[string]any{"name": "go", "executable": "/opt/fixture/bin/go", "is_shell": false, "argv_prefix": []any{"go", "test"}}}
	f.cfg["env_profiles"] = []any{
		map[string]any{"name": "clean", "values": map[string]any{}},
		map[string]any{"name": "build", "values": map[string]any{"GOFLAGS": "-mod=mod", "LANG": "C.UTF-8"}},
	}
	p := f.policy()
	p["tools"] = []any{"evidence.read", "file.read", "process.exec"}
	p["read_prefixes"] = []any{"docs", "src"}
	p["write_prefixes"] = []any{"src"}
	p["process_profiles"] = []any{"go"}
	p["env_profiles"] = []any{"build", "clean"}
	d := mustLoad(t, f)
	got, err := d.PolicyRevision("fixture-workspace-write", f.Work, "trusted_host")
	if err != nil {
		t.Fatal(err)
	}
	want, err := config.EffectivePolicyRevision(config.EffectivePolicyInput{
		Policy: config.Policy{
			ID: "fixture-workspace-write", AllowedModes: []string{"structured_only", "trusted_host"},
			Tools: []string{"evidence.read", "file.read", "process.exec"}, ReadPrefixes: []string{"docs", "src"}, WritePrefixes: []string{"src"},
			ProcessProfiles: []string{"go"}, EnvProfiles: []string{"build", "clean"}, Limits: limits10(),
		},
		ProcessProfiles: []config.ProcessProfile{{Name: "go", Executable: "/opt/fixture/bin/go", ArgvPrefix: []string{"go", "test"}}},
		EnvProfiles: []config.EnvProfile{
			{Name: "clean", Values: map[string]string{}}, {Name: "build", Values: map[string]string{"GOFLAGS": "-mod=mod", "LANG": "C.UTF-8"}},
		},
		WorkspaceRoot: f.Work, Mode: "trusted_host",
	})
	if err != nil || got != want {
		t.Fatalf("got %s want %s (%v)", got, want, err)
	}
	if _, err := d.PolicyRevision("not-registered", f.Work, "trusted_host"); !errors.Is(err, config.ErrUnregisteredPolicy) {
		t.Fatalf("%v", err)
	}
	// Editing the registry file after load cannot loosen a loaded deployment.
	if err := os.WriteFile(f.Registry, []byte(strings.Replace(mustRead(t, f.Registry), `"src"`, `"."`, -1)), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := d.PolicyRevision("fixture-workspace-write", f.Work, "trusted_host")
	if err != nil || again != got {
		t.Fatalf("a loaded deployment re-read the registry: %s %v", again, err)
	}
}

func mustRead(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
