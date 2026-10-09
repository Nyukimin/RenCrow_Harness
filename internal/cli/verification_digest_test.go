package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
)

func TestVerificationDigestPrintsOnlyTheEffectiveCriteriaHash(t *testing.T) {
	l := harnesstest.NewLayout(t, harnesstest.Options{})
	fixtureVolume := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		fixtureVolume = `C:\`
	}
	executable := filepath.Join(fixtureVolume, "opt", "fixture", "go")
	policy := l.Reg["policies"].([]any)[0].(map[string]any)
	policy["tools"] = append(policy["tools"].([]any), "process.exec")
	policy["process_profiles"] = []any{"go-test"}
	policy["env_profiles"] = []any{"clean", "ci"}
	policy["verification"] = map[string]any{
		"format_version": "rencrow-verification-plan/v1", "process_profile_ref": "go-test", "executable": executable,
		"argv": []any{"go", "test", "./..."}, "cwd": ".", "env_profile_ref": "ci", "timeout_seconds": 300,
		"pass_condition": "exit_zero",
	}
	l.Cfg["process_profiles"] = []any{map[string]any{
		"name": "go-test", "executable": executable, "is_shell": false, "argv_prefix": []any{"go"},
	}}
	l.Cfg["env_profiles"] = []any{
		map[string]any{"name": "clean", "values": map[string]any{}},
		map[string]any{"name": "ci", "values": map[string]any{"TOKEN": "fixture-secret"}},
	}
	if err := os.Mkdir(l.Data, 0o700); err != nil {
		t.Fatal(err)
	}
	l.Write()

	code, stdout, stderr := run(t, "", "verification-digest", "--config", l.Config, "--policy-ref", l.WorkspacePolicy(), "--workspace", l.Work, "--mode", "trusted_host")
	if code != 0 {
		t.Fatalf("digest: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var output map[string]string
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("digest output is not JSON: %v: %q", err, stdout)
	}
	digest := output["criteria_revision"]
	if len(output) != 1 || len(digest) != 64 || strings.ToLower(digest) != digest {
		t.Fatalf("unexpected digest output: %q", stdout)
	}
	dep, err := config.Load(l.Config)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := dep.EffectivePolicy(l.WorkspacePolicy(), l.Work, "trusted_host")
	if err != nil || digest != effective.VerificationCriteriaRevision {
		t.Fatalf("owner CLI did not emit the effective criteria digest: %q != %q (%v)", digest, effective.VerificationCriteriaRevision, err)
	}
	for _, forbidden := range []string{"fixture-secret", executable, "./...", "TOKEN", l.Work} {
		if strings.Contains(stdout, forbidden) {
			t.Fatalf("digest output disclosed %q: %q", forbidden, stdout)
		}
	}
	if entries, err := os.ReadDir(l.Data); err != nil || len(entries) != 0 {
		t.Fatalf("digest command opened or created storage files: entries=%v err=%v", entries, err)
	}
}

func TestVerificationDigestRefusesPolicyWithoutPlan(t *testing.T) {
	l := harnesstest.NewLayout(t, harnesstest.Options{})
	code, stdout, _ := run(t, "", "verification-digest", "--config", l.Config, "--policy-ref", l.WorkspacePolicy(), "--workspace", l.Work)
	if code != 64 || stdout != "" {
		t.Fatalf("unconfigured verifier: code=%d stdout=%q", code, stdout)
	}
}
