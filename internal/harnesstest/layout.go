// Package harnesstest builds throw-away deployments for tests: a private tree under
// t.TempDir() with a configuration, a policy registry, a data root and a workspace,
// derived from the design package's example configuration. Nothing outside the
// temporary tree is read or written, and no real credential or key is involved.
//
// It is test support only; no production package imports it.
package harnesstest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// Layout is one throw-away deployment tree.
type Layout struct {
	t        testing.TB
	Dir      string // real path of the private temp root
	Data     string // data_root (created by NewLayout only if asked)
	Backup   string
	Work     string // the workspace root
	Secrets  string // directory of the config and the registry
	Registry string
	Config   string
	// Cfg and Reg are the documents as they will be written; tests may edit them
	// before Write.
	Cfg map[string]any
	Reg map[string]any
}

// Options select how much of the tree exists before the test starts.
type Options struct {
	// CreateData creates the (empty, owner-only) data root. Leave it false to let
	// `init` create it.
	CreateData bool
}

// ModuleRoot returns the root of the Go module, found from the test's working
// directory, so tests in any package can read the design examples.
func ModuleRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the working directory")
		}
		dir = parent
	}
}

func example(t testing.TB, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ModuleRoot(t), "testdata", "contract", "examples", name))
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

// ClosedLoopbackBaseURL returns a Gateway base URL (http://127.0.0.1:<port>/v1) whose port
// nothing listens on: the port is taken from the system and released again before the URL
// is returned. It is what a Gateway that is down looks like, and unlike a fixed port (the
// example configuration names the one a real Gateway serves on) it cannot reach a Gateway
// that happens to run on the host the tests run on.
func ClosedLoopbackBaseURL(t testing.TB) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("http://127.0.0.1:%d/v1", port)
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

// NewLayout builds the tree and writes the configuration files.
func NewLayout(t testing.TB, opts Options) *Layout {
	t.Helper()
	dir := t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	l := &Layout{t: t, Dir: dir}
	l.Data = filepath.Join(dir, "data")
	l.Backup = filepath.Join(dir, "backup")
	l.Work = filepath.Join(dir, "work")
	l.Secrets = filepath.Join(dir, "secrets")
	mkdir(t, l.Backup, 0o700)
	mkdir(t, l.Work, 0o755)
	mkdir(t, l.Secrets, 0o700)
	if opts.CreateData {
		mkdir(t, l.Data, 0o700)
	}
	l.Registry = filepath.Join(l.Secrets, "policies.json")
	l.Config = filepath.Join(l.Secrets, "config.json")

	l.Cfg = example(t, "config.json")
	l.Reg = example(t, "policies.json")
	l.Cfg["data_root"] = l.Data
	// The Gateway is down unless a test starts a double and names it: never the address of
	// the example configuration, which a real Gateway on this host would answer.
	l.Cfg["gateway"].(map[string]any)["base_url"] = ClosedLoopbackBaseURL(t)
	l.Cfg["storage"].(map[string]any)["backup_root"] = l.Backup
	l.Cfg["workspaces"].([]any)[0].(map[string]any)["root"] = l.Work
	l.Cfg["policy_registry_path"] = l.Registry
	l.Write()
	return l
}

// Write persists the (possibly edited) documents.
func (l *Layout) Write() {
	l.t.Helper()
	for path, doc := range map[string]map[string]any{l.Registry: l.Reg, l.Config: l.Cfg} {
		b, err := json.Marshal(doc)
		if err != nil {
			l.t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			l.t.Fatal(err)
		}
	}
}

// Principal is the caller principal of the example configuration.
func (l *Layout) Principal() string {
	return l.Cfg["caller"].(map[string]any)["principal"].(string)
}

// WorkspacePolicy returns the policy_ref the example workspace is bound to.
func (l *Layout) WorkspacePolicy() string {
	return l.Cfg["workspaces"].([]any)[0].(map[string]any)["policy_ref"].(string)
}

// Binding returns the binding of the example configuration.
func (l *Layout) Binding() map[string]any {
	return l.Cfg["bindings"].([]any)[0].(map[string]any)["binding"].(map[string]any)
}
