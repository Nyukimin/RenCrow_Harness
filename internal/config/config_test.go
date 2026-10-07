package config_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func load(t testing.TB, f *fixture) (*config.Deployment, error) {
	t.Helper()
	return config.Load(f.Write())
}

func mustLoad(t testing.TB, f *fixture) *config.Deployment {
	t.Helper()
	d, err := load(t, f)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func wantInvalid(t testing.TB, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("configuration accepted")
	}
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("error does not wrap ErrInvalid: %v", err)
	}
}

func TestDesignExampleConfigLoadsOnceItsPathsAreReal(t *testing.T) {
	f := newFixture(t)
	d := mustLoad(t, f)
	if d.DataRoot != f.Data || d.BackupRoot != f.Backup {
		t.Fatalf("roots %s %s", d.DataRoot, d.BackupRoot)
	}
	// recovery_policy_revision of the design example (start_result.json), computed
	// by the stage 1 function from the same two profiles.
	if d.RecoveryRevision != "db87835a3c36bf3f845441203fc0a5f90baa4d42bb05a7298935bdbef0edf0e9" {
		t.Fatalf("recovery revision %s", d.RecoveryRevision)
	}
	want, err := protocol.CallerProfileDigest(protocol.CallerProfile{
		Principal: "user:ren", DefaultOrigin: "human",
		ReadableSessionOwners: []string{"user:ren", "core:local"}, ControllableSessionOwners: []string{"user:ren", "core:local"},
		RelayIssuers: []protocol.RelayIssuer{{Issuer: "core:fixture", KeyID: "fixture-key-1", Audience: "user:ren"}},
	})
	if err != nil || d.Caller.ProfileDigest != want {
		t.Fatalf("caller digest %s, want %s (%v)", d.Caller.ProfileDigest, want, err)
	}
	if !d.Caller.CanControl("core:local") || d.Caller.CanControl("user:other") {
		t.Fatal("session owners were not applied")
	}
	if len(d.Workspaces()) != 1 || d.Workspaces()[0].Root != f.Work {
		t.Fatalf("%+v", d.Workspaces())
	}
	// The key file was read: a proof signed with it must verify through the Caller.
	if d.Config.Gateway.RequiredContract != "harness-v1" {
		t.Fatal("gateway section not decoded")
	}
}

func TestConfigNeedsNoRelayIssuers(t *testing.T) {
	f := newFixture(t)
	f.caller()["relay_issuers"] = []any{}
	d := mustLoad(t, f)
	if d.Caller.ProfileDigest == "" {
		t.Fatal("no digest")
	}
}

func TestConfigRejectsMalformedAndUnknownContent(t *testing.T) {
	f := newFixture(t)
	f.Write()
	good, err := os.ReadFile(f.Config)
	if err != nil {
		t.Fatal(err)
	}
	text := string(good)
	cases := map[string][]byte{
		"duplicate key":          []byte(strings.Replace(text, `"data_root"`, `"data_root":"/x","data_root"`, 1)),
		"invalid utf-8":          []byte(strings.Replace(text, `"gateway"`, "\"gate\xffway\"", 1)),
		"unpaired surrogate":     []byte(strings.Replace(text, `"gateway"`, `"gate\ud800way"`, 1)),
		"bom":                    append([]byte("\xef\xbb\xbf"), good...),
		"trailing value":         append(append([]byte{}, good...), []byte(" {}")...),
		"not json":               []byte("data_root: x"),
		"empty":                  nil,
		"unknown config version": []byte(strings.Replace(text, "rencrow-harness-config/v1", "rencrow-harness-config/v2", 1)),
		"unknown top-level key":  []byte(strings.Replace(text, `"data_root"`, `"surprise":true,"data_root"`, 1)),
		"unknown gateway key":    []byte(strings.Replace(text, `"required_contract"`, `"proxy":"x","required_contract"`, 1)),
		"unknown extension key":  []byte(strings.Replace(text, `"skill_roots"`, `"plugins":[],"skill_roots"`, 1)),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := config.Parse(f.Config, data)
			wantInvalid(t, err)
		})
	}
}

func TestConfigMissingRequiredSections(t *testing.T) {
	for _, key := range []string{
		"config_version", "data_root", "gateway", "caller", "workspaces", "bindings", "process_profiles", "limits",
		"compaction", "storage", "recovery", "policy_registry_path", "env_profiles", "extensions",
	} {
		t.Run(key, func(t *testing.T) {
			f := newFixture(t)
			delete(f.cfg, key)
			_, err := load(t, f)
			wantInvalid(t, err)
		})
	}
}

func TestPathsMustBeAbsoluteAndLiteral(t *testing.T) {
	cases := map[string]func(f *fixture, bad string){
		"data_root":            func(f *fixture, bad string) { f.cfg["data_root"] = bad },
		"backup_root":          func(f *fixture, bad string) { f.cfg["storage"].(map[string]any)["backup_root"] = bad },
		"policy_registry_path": func(f *fixture, bad string) { f.cfg["policy_registry_path"] = bad },
		"workspace root":       func(f *fixture, bad string) { f.workspace()["root"] = bad },
		"relay key_file": func(f *fixture, bad string) {
			f.caller()["relay_issuers"].([]any)[0].(map[string]any)["key_file"] = bad
		},
		"trusted root": func(f *fixture, bad string) {
			f.cfg["extensions"].(map[string]any)["trusted_workspace_roots"] = []any{bad}
		},
		"skill root": func(f *fixture, bad string) { f.cfg["extensions"].(map[string]any)["skill_roots"] = []any{bad} },
	}
	bads := []string{"data", "./data", "~/data", "$HOME/data", "${HOME}/data", "%APPDATA%\\data", "file:///data", "http://host/data", ""}
	for field, set := range cases {
		for _, bad := range bads {
			t.Run(field+"/"+bad, func(t *testing.T) {
				f := newFixture(t)
				set(f, bad)
				_, err := load(t, f)
				wantInvalid(t, err)
			})
		}
		t.Run(field+"/dot-dot", func(t *testing.T) {
			f := newFixture(t)
			set(f, f.Dir+"/data/../data")
			_, err := load(t, f)
			wantInvalid(t, err)
		})
		t.Run(field+"/not-clean", func(t *testing.T) {
			f := newFixture(t)
			set(f, f.Dir+"//data")
			_, err := load(t, f)
			wantInvalid(t, err)
		})
	}
}

// A path that merely looks like a variable is a literal directory name. Nothing in
// the configuration is expanded from the environment.
func TestEnvironmentVariablesAreNeverExpanded(t *testing.T) {
	f := newFixture(t)
	literal := filepath.Join(f.Dir, "$HARNESS_TEST_ROOT")
	mkdir(t, literal, 0o700)
	t.Setenv("HARNESS_TEST_ROOT", filepath.Join(f.Dir, "elsewhere"))
	f.cfg["data_root"] = literal
	d := mustLoad(t, f)
	if d.DataRoot != literal {
		t.Fatalf("data_root %s, want the literal %s", d.DataRoot, literal)
	}
}

func TestDataRootConditions(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		f := newFixture(t)
		f.cfg["data_root"] = filepath.Join(f.Dir, "absent")
		_, err := load(t, f)
		wantInvalid(t, err)
	})
	t.Run("a file", func(t *testing.T) {
		f := newFixture(t)
		p := filepath.Join(f.Dir, "afile")
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		f.cfg["data_root"] = p
		_, err := load(t, f)
		wantInvalid(t, err)
	})
	for _, mode := range []os.FileMode{0o750, 0o755, 0o770, 0o777} {
		t.Run("mode "+mode.String(), func(t *testing.T) {
			f := newFixture(t)
			if err := os.Chmod(f.Data, mode); err != nil {
				t.Fatal(err)
			}
			_, err := load(t, f)
			wantInvalid(t, err)
			if !errors.Is(err, fsperm.ErrNotOwnerOnly) {
				t.Fatalf("the permission failure must be identifiable: %v", err)
			}
		})
	}
	t.Run("a symbolic link", func(t *testing.T) {
		f := newFixture(t)
		link := filepath.Join(f.Dir, "datalink")
		if err := os.Symlink(f.Data, link); err != nil {
			t.Fatal(err)
		}
		f.cfg["data_root"] = link
		d, err := load(t, f)
		// The link is resolved to the real directory, which is private; the
		// store always works on the resolved path.
		if err != nil {
			t.Fatal(err)
		}
		if d.DataRoot != f.Data {
			t.Fatalf("data root %s, want the resolved %s", d.DataRoot, f.Data)
		}
	})
}

func TestRootsMustStayOutsideTheWorkspace(t *testing.T) {
	inside := func(f *fixture, name string) string {
		p := filepath.Join(f.Work, name)
		mkdir(t, p, 0o700)
		return p
	}
	cases := map[string]func(f *fixture){
		"data_root inside a workspace": func(f *fixture) { f.cfg["data_root"] = inside(f, "data") },
		"data_root is the workspace":   func(f *fixture) { f.cfg["data_root"] = f.Work; _ = os.Chmod(f.Work, 0o700) },
		"workspace inside data_root": func(f *fixture) {
			p := filepath.Join(f.Data, "ws")
			mkdir(t, p, 0o755)
			f.workspace()["root"] = p
		},
		"backup_root inside a workspace": func(f *fixture) { f.cfg["storage"].(map[string]any)["backup_root"] = inside(f, "backup") },
		"backup_root is the data_root":   func(f *fixture) { f.cfg["storage"].(map[string]any)["backup_root"] = f.Data },
		"backup_root inside data_root": func(f *fixture) {
			p := filepath.Join(f.Data, "backup")
			mkdir(t, p, 0o700)
			f.cfg["storage"].(map[string]any)["backup_root"] = p
		},
		"data_root inside backup_root": func(f *fixture) {
			p := filepath.Join(f.Backup, "data")
			mkdir(t, p, 0o700)
			f.cfg["data_root"] = p
		},
		"policy registry inside a workspace": func(f *fixture) {
			f.Registry = filepath.Join(f.Work, "policies.json")
			f.cfg["policy_registry_path"] = f.Registry
		},
		"relay key inside a workspace": func(f *fixture) {
			f.KeyFile = filepath.Join(f.Work, "relay.key")
			f.WriteKey([]byte(strings.Repeat("ab", 32)), 0o600)
			f.caller()["relay_issuers"].([]any)[0].(map[string]any)["key_file"] = f.KeyFile
		},
		"config file inside a workspace": func(f *fixture) { f.Config = filepath.Join(f.Work, "config.json") },
		"workspace is a parent of the secrets": func(f *fixture) {
			f.workspace()["root"] = f.Dir
		},
		"two workspaces sharing a root": func(f *fixture) {
			f.cfg["workspaces"] = append(f.cfg["workspaces"].([]any), map[string]any{
				"root": f.Work, "allowed_modes": []any{"structured_only"}, "policy_ref": "fixture-workspace-write",
			})
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

func TestWorkspaceMustExist(t *testing.T) {
	f := newFixture(t)
	f.workspace()["root"] = filepath.Join(f.Dir, "no-such-workspace")
	_, err := load(t, f)
	wantInvalid(t, err)
}

func TestGatewayMustBeLoopback(t *testing.T) {
	for url, ok := range map[string]bool{
		"http://127.0.0.1:8090/v1": true, "http://localhost:8090/v1": true, "http://[::1]:8090/v1": true,
		"https://127.0.0.1:8443/v1": true, "http://127.1.2.3:80/v1": true,
		"http://example.com/v1": false, "http://10.0.0.5:8090/v1": false, "http://192.168.1.2/v1": false,
		"http://0.0.0.0:8090/v1": false, "http://127.0.0.1.evil.example/v1": false, "http://localhost.evil.example/v1": false,
		"http://user:secret@127.0.0.1:8090/v1": false, "ftp://127.0.0.1/v1": false, "127.0.0.1:8090": false,
		"http://127.0.0.1:8090/v1#frag": false, "": false, "unix:///run/gateway.sock": false,
	} {
		t.Run(url, func(t *testing.T) {
			f := newFixture(t)
			f.cfg["gateway"].(map[string]any)["base_url"] = url
			_, err := load(t, f)
			if ok && err != nil {
				t.Fatal(err)
			}
			if !ok {
				wantInvalid(t, err)
			}
		})
	}
}

func TestRelayIssuerConditions(t *testing.T) {
	issuer := func(f *fixture) map[string]any { return f.caller()["relay_issuers"].([]any)[0].(map[string]any) }
	t.Run("audience is not the principal", func(t *testing.T) {
		f := newFixture(t)
		issuer(f)["audience"] = "core:local"
		_, err := load(t, f)
		wantInvalid(t, err)
	})
	t.Run("duplicate issuer and key id", func(t *testing.T) {
		f := newFixture(t)
		f.caller()["relay_issuers"] = append(f.caller()["relay_issuers"].([]any), issuer(f))
		_, err := load(t, f)
		wantInvalid(t, err)
	})
	keyCases := map[string][]byte{
		"crlf":          []byte(strings.Repeat("ab", 32) + "\r\n"),
		"uppercase":     []byte(strings.Repeat("AB", 32) + "\n"),
		"base64":        []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n"),
		"too short":     []byte(strings.Repeat("ab", 31) + "\n"),
		"two newlines":  []byte(strings.Repeat("ab", 32) + "\n\n"),
		"leading space": []byte(" " + strings.Repeat("ab", 32)),
		"empty":         nil,
		"raw binary":    []byte(strings.Repeat("\x01", 32)),
		"bom":           append([]byte("\xef\xbb\xbf"), strings.Repeat("ab", 32)...),
	}
	for name, content := range keyCases {
		t.Run("key file "+name, func(t *testing.T) {
			f := newFixture(t)
			f.WriteKey(content, 0o600)
			_, err := load(t, f)
			wantInvalid(t, err)
			if !errors.Is(err, protocol.ErrInvalidKeyFile) {
				t.Fatalf("the key format failure must be identifiable: %v", err)
			}
		})
	}
	for _, content := range [][]byte{[]byte(strings.Repeat("ab", 32)), []byte(strings.Repeat("ab", 32) + "\n")} {
		f := newFixture(t)
		f.WriteKey(content, 0o600)
		if _, err := load(t, f); err != nil {
			t.Fatalf("a valid key file (%d bytes) was refused: %v", len(content), err)
		}
	}
	for _, mode := range []os.FileMode{0o640, 0o644, 0o604, 0o666} {
		t.Run("key file mode "+mode.String(), func(t *testing.T) {
			f := newFixture(t)
			f.WriteKey([]byte(strings.Repeat("ab", 32)), mode)
			_, err := load(t, f)
			wantInvalid(t, err)
			if !errors.Is(err, fsperm.ErrNotOwnerOnly) {
				t.Fatalf("%v", err)
			}
		})
	}
	t.Run("key file missing", func(t *testing.T) {
		f := newFixture(t)
		if err := os.Remove(f.KeyFile); err != nil {
			t.Fatal(err)
		}
		_, err := load(t, f)
		wantInvalid(t, err)
	})
	t.Run("key file is a symbolic link", func(t *testing.T) {
		f := newFixture(t)
		real := filepath.Join(f.Secrets, "real.key")
		if err := os.WriteFile(real, []byte(strings.Repeat("ab", 32)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(f.KeyFile); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, f.KeyFile); err != nil {
			t.Fatal(err)
		}
		_, err := load(t, f)
		wantInvalid(t, err)
	})
}

func TestBindingConditions(t *testing.T) {
	binding := func(f *fixture) map[string]any {
		return f.cfg["bindings"].([]any)[0].(map[string]any)["binding"].(map[string]any)
	}
	cases := map[string]func(f *fixture){
		"alias without an agent":  func(f *fixture) { b := binding(f); b["kind"] = "alias"; b["execution_role"] = "act" },
		"alias without a role":    func(f *fixture) { b := binding(f); b["kind"] = "alias"; b["agent_id"] = "shiro" },
		"latest profile revision": func(f *fixture) { binding(f)["profile_revision"] = "latest" },
		"duplicate profile name": func(f *fixture) {
			f.cfg["bindings"] = append(f.cfg["bindings"].([]any), f.cfg["bindings"].([]any)[0])
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
	f := newFixture(t)
	b := binding(f)
	b["kind"], b["agent_id"], b["execution_role"] = "alias", "shiro", "act"
	d := mustLoad(t, f)
	got, err := d.Binding("fixture-local")
	if err != nil || got.Kind != "alias" || got.AgentID == nil || *got.AgentID != "shiro" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := d.Binding("other"); err == nil {
		t.Fatal("an unknown profile name must not resolve to a default binding")
	}
}

func TestEnvProfileConditions(t *testing.T) {
	cases := map[string]func(f *fixture){
		"clean missing": func(f *fixture) {
			f.cfg["env_profiles"] = []any{map[string]any{"name": "build", "values": map[string]any{}}}
			f.policy()["env_profiles"] = []any{"build"}
		},
		"clean not empty": func(f *fixture) {
			f.envProfiles()[0].(map[string]any)["values"] = map[string]any{"PATH": "/opt/fixture/bin"}
		},
		"duplicate profile": func(f *fixture) { f.cfg["env_profiles"] = append(f.envProfiles(), f.envProfiles()[0]) },
		"name with equals":  func(f *fixture) { f.envProfiles()[0].(map[string]any)["values"] = map[string]any{"A=B": "x"} },
		"empty name": func(f *fixture) {
			f.cfg["env_profiles"] = append(f.envProfiles(), map[string]any{"name": "b", "values": map[string]any{"": "x"}})
		},
		"value with nul": func(f *fixture) {
			f.cfg["env_profiles"] = append(f.envProfiles(), map[string]any{"name": "b", "values": map[string]any{"A": "x\u0000y"}})
		},
		"value not a string": func(f *fixture) {
			f.cfg["env_profiles"] = append(f.envProfiles(), map[string]any{"name": "b", "values": map[string]any{"A": 1}})
		},
		"policy names a missing profile": func(f *fixture) { f.policy()["env_profiles"] = []any{"clean", "ghost"} },
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

func TestProcessProfileConditions(t *testing.T) {
	proc := func(name, exe string) map[string]any {
		return map[string]any{"name": name, "executable": exe, "is_shell": false, "argv_prefix": []any{"go", "test"}}
	}
	cases := map[string]func(f *fixture){
		"relative executable": func(f *fixture) { f.cfg["process_profiles"] = []any{proc("go", "go")} },
		"duplicate name": func(f *fixture) {
			f.cfg["process_profiles"] = []any{proc("go", "/opt/fixture/bin/go"), proc("go", "/opt/fixture/bin/make")}
		},
		"policy names ghost":  func(f *fixture) { f.policy()["process_profiles"] = []any{"ghost"} },
		"executable with nul": func(f *fixture) { f.cfg["process_profiles"] = []any{proc("go", "/opt/fixture/bin/go\u0000")} },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			mod(f)
			_, err := load(t, f)
			wantInvalid(t, err)
		})
	}
	f := newFixture(t)
	f.cfg["process_profiles"] = []any{proc("go", "/opt/fixture/bin/go")}
	f.policy()["process_profiles"] = []any{"go"}
	mustLoad(t, f)
}

func TestLimitsAndCompactionBounds(t *testing.T) {
	cases := map[string]func(f *fixture){
		"zero steps":               func(f *fixture) { f.cfg["limits"].(map[string]any)["max_model_steps"] = 0 },
		"deadline above a day":     func(f *fixture) { f.cfg["limits"].(map[string]any)["deadline_seconds"] = 86401 },
		"missing generation limit": func(f *fixture) { delete(f.cfg["limits"].(map[string]any), "max_generation_attempts") },
		"trigger ratio one":        func(f *fixture) { f.cfg["compaction"].(map[string]any)["trigger_ratio"] = 1 },
		"trigger ratio zero":       func(f *fixture) { f.cfg["compaction"].(map[string]any)["trigger_ratio"] = 0 },
		"three stage requests":     func(f *fixture) { f.cfg["compaction"].(map[string]any)["max_logical_stage_requests"] = 3 },
		"quota below ten MiB":      func(f *fixture) { f.cfg["storage"].(map[string]any)["max_database_bytes"] = 1024 },
		"three attempts per act":   func(f *fixture) { f.cfg["recovery"].(map[string]any)["max_attempts_per_act"] = 3 },
		"unknown recovery profile": func(f *fixture) {
			f.cfg["recovery"].(map[string]any)["allowed_profiles"] = []any{"same_request", "other_model"}
		},
		"recovery without same_request": func(f *fixture) {
			f.cfg["recovery"].(map[string]any)["allowed_profiles"] = []any{"terminal_output_once"}
		},
		"hook that runs a command": func(f *fixture) { f.cfg["extensions"].(map[string]any)["hooks"] = []any{"curl"} },
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

func TestRecoveryRevisionFollowsTheConfiguredProfiles(t *testing.T) {
	f := newFixture(t)
	f.cfg["recovery"].(map[string]any)["allowed_profiles"] = []any{"same_request"}
	d := mustLoad(t, f)
	want, err := protocol.RecoveryPolicyRevision("act-recovery/v1", 2, []string{"same_request"})
	if err != nil || d.RecoveryRevision != want {
		t.Fatalf("%s want %s (%v)", d.RecoveryRevision, want, err)
	}
	if d.RecoveryRevision == "db87835a3c36bf3f845441203fc0a5f90baa4d42bb05a7298935bdbef0edf0e9" {
		t.Fatal("the revision must change with the allowed profile set")
	}
}

func TestLoadNeedsAnAbsoluteConfigPath(t *testing.T) {
	f := newFixture(t)
	f.Write()
	rel, err := filepath.Rel(mustGetwd(t), f.Config)
	if err != nil {
		t.Skip("no relative form of the temp path")
	}
	if _, err := config.Load(rel); err == nil {
		t.Fatal("a relative config path was accepted")
	}
	if _, err := config.Load(filepath.Join(f.Dir, "absent.json")); err == nil {
		t.Fatal("a missing config file was accepted")
	}
}

func mustGetwd(t testing.TB) string {
	t.Helper()
	d, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestWorkspaceLookupAndEffectiveCaps(t *testing.T) {
	f := newFixture(t)
	f.cfg["limits"] = map[string]any{"max_model_steps": 10, "max_tool_calls_per_step": 8, "deadline_seconds": 1800, "max_capture_bytes": 67108864, "max_generation_attempts": 32}
	f.policy()["limits"] = map[string]any{"max_model_steps": 20, "max_tool_calls_per_step": 4, "deadline_seconds": 900, "max_capture_bytes": 1048576, "max_generation_attempts": 16}
	d := mustLoad(t, f)
	caps, err := d.EffectiveCaps("fixture-workspace-write")
	if err != nil {
		t.Fatal(err)
	}
	want := protocol.Limits{MaxModelSteps: 10, MaxToolCallsPerStep: 4, DeadlineSeconds: 900, MaxCaptureBytes: 1048576, MaxGenerationAttempts: 16}
	if caps != want {
		t.Fatalf("caps %+v, want the smaller of host and policy %+v", caps, want)
	}
	if _, err := d.EffectiveCaps("not-registered"); !errors.Is(err, config.ErrUnregisteredPolicy) {
		t.Fatalf("an unregistered policy_ref must be refused: %v", err)
	}
	ws, ok := d.Workspace(f.Work)
	if !ok || ws.PolicyRef != "fixture-workspace-write" {
		t.Fatalf("%+v %v", ws, ok)
	}
	if _, ok := d.Workspace(filepath.Join(f.Work, "sub")); ok {
		t.Fatal("a sub-directory of a workspace is not itself a registered workspace root")
	}
}

func TestBackupRootMayBeCreatedLater(t *testing.T) {
	f := newFixture(t)
	later := filepath.Join(f.Dir, "backups-later")
	f.cfg["storage"].(map[string]any)["backup_root"] = later
	d := mustLoad(t, f)
	if d.BackupRoot != later {
		t.Fatalf("backup root %s, want %s", d.BackupRoot, later)
	}
	if _, err := os.Stat(later); err == nil {
		t.Fatal("validating the configuration must not create the backup root")
	}
	// A missing parent is not enough: the location itself must be real.
	f.cfg["storage"].(map[string]any)["backup_root"] = filepath.Join(f.Dir, "no-parent", "backups")
	_, err := load(t, f)
	wantInvalid(t, err)
	// And a file in its place is refused.
	asFile := filepath.Join(f.Dir, "backup-is-a-file")
	if err := os.WriteFile(asFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.cfg["storage"].(map[string]any)["backup_root"] = asFile
	_, err = load(t, f)
	wantInvalid(t, err)
}

func TestValidationNeverCreatesOrChangesAnything(t *testing.T) {
	f := newFixture(t)
	f.Write()
	snapshot := func() string {
		var names []string
		_ = filepath.Walk(f.Dir, func(p string, info os.FileInfo, err error) error {
			if err == nil {
				names = append(names, fmt.Sprintf("%s|%v|%d|%d", p, info.Mode(), info.Size(), info.ModTime().UnixNano()))
			}
			return nil
		})
		return strings.Join(names, "\n")
	}
	before := snapshot()
	if _, err := config.Load(f.Config); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("Load changed the tree:\n%s\n---\n%s", before, after)
	}
}

// An operator's directory layout is not for error text, which can reach logs and
// clients. The cause (not found, permission) is kept; the path is not.
func TestErrorsDoNotCarryTheDirectoryLayout(t *testing.T) {
	cases := map[string]func(f *fixture){
		"data_root missing":        func(f *fixture) { f.cfg["data_root"] = filepath.Join(f.Dir, "absent-data") },
		"workspace missing":        func(f *fixture) { f.workspace()["root"] = filepath.Join(f.Dir, "absent-work") },
		"registry missing":         func(f *fixture) { f.cfg["policy_registry_path"] = filepath.Join(f.Secrets, "absent.json") },
		"key file missing":         func(f *fixture) { _ = os.Remove(f.KeyFile) },
		"data_root not owner-only": func(f *fixture) { _ = os.Chmod(f.Data, 0o755) },
		"key file not owner-only":  func(f *fixture) { f.WriteKey([]byte(strings.Repeat("ab", 32)), 0o644) },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			mod(f)
			_, err := load(t, f)
			wantInvalid(t, err)
			if strings.Contains(err.Error(), f.Dir) {
				t.Fatalf("the error carries the directory layout: %v", err)
			}
		})
	}
	t.Run("config file missing", func(t *testing.T) {
		f := newFixture(t)
		_, err := config.Load(filepath.Join(f.Secrets, "absent.json"))
		wantInvalid(t, err)
		if strings.Contains(err.Error(), f.Dir) {
			t.Fatalf("%v", err)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the cause must stay identifiable: %v", err)
		}
	})
}
