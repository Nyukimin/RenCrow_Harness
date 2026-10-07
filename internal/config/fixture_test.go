package config_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func limits10() protocol.Limits {
	return protocol.Limits{MaxModelSteps: 10, MaxToolCallsPerStep: 8, DeadlineSeconds: 1800, MaxCaptureBytes: 67108864, MaxGenerationAttempts: 32}
}

// fixture is a throw-away deployment tree built from the design example config.
// The example holds placeholder paths; here they point at real directories under
// t.TempDir(), so nothing outside the temporary tree is read or written.
type fixture struct {
	t        testing.TB
	Dir      string // private (0700) temp root
	Data     string
	Backup   string
	Work     string // the workspace root
	Secrets  string
	Registry string // policy registry file
	KeyFile  string
	Config   string // config file path
	cfg      map[string]any
	reg      map[string]any
}

func designJSON(t testing.TB, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contract", "examples", name))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func mkdir(t testing.TB, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	dir := t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	f := &fixture{t: t, Dir: dir}
	f.Data = filepath.Join(dir, "data")
	f.Backup = filepath.Join(dir, "backup")
	f.Work = filepath.Join(dir, "work")
	f.Secrets = filepath.Join(dir, "secrets")
	mkdir(t, f.Data, 0o700)
	mkdir(t, f.Backup, 0o700)
	mkdir(t, f.Work, 0o755)
	mkdir(t, f.Secrets, 0o700)
	f.Registry = filepath.Join(f.Secrets, "policies.json")
	f.KeyFile = filepath.Join(f.Secrets, "relay.key")
	f.Config = filepath.Join(f.Secrets, "config.json")

	f.cfg = designJSON(t, "config.json")
	f.reg = designJSON(t, "policies.json")
	f.cfg["data_root"] = f.Data
	f.cfg["storage"].(map[string]any)["backup_root"] = f.Backup
	f.cfg["workspaces"].([]any)[0].(map[string]any)["root"] = f.Work
	f.cfg["policy_registry_path"] = f.Registry
	// A relay issuer with a key file, so the key path can be exercised. The audience
	// is the caller principal, as the profile requires.
	f.cfg["caller"].(map[string]any)["relay_issuers"] = []any{map[string]any{
		"issuer": "core:fixture", "key_id": "fixture-key-1", "audience": "user:ren", "key_file": f.KeyFile,
	}}
	f.WriteKey([]byte("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f\n"), 0o600)
	return f
}

// WriteKey writes the relay key file with the given bytes and mode.
func (f *fixture) WriteKey(b []byte, mode os.FileMode) {
	f.t.Helper()
	_ = os.Remove(f.KeyFile)
	if err := os.WriteFile(f.KeyFile, b, mode); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(f.KeyFile, mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) writeJSON(path string, v any) {
	f.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Fatal(err)
	}
	f.writeBytes(path, b)
}

func (f *fixture) writeBytes(path string, b []byte) {
	f.t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// Write persists config and registry as currently modified and returns the config path.
func (f *fixture) Write() string {
	f.t.Helper()
	f.writeJSON(f.Registry, f.reg)
	f.writeJSON(f.Config, f.cfg)
	return f.Config
}

func (f *fixture) workspace() map[string]any { return f.cfg["workspaces"].([]any)[0].(map[string]any) }
func (f *fixture) caller() map[string]any    { return f.cfg["caller"].(map[string]any) }
func (f *fixture) policy() map[string]any    { return f.reg["policies"].([]any)[0].(map[string]any) }
func (f *fixture) envProfiles() []any        { return f.cfg["env_profiles"].([]any) }
