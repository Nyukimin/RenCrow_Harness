package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func verificationFixtureVolume() string {
	if runtime.GOOS == "windows" {
		return `C:\`
	}
	return string(filepath.Separator)
}

func verificationFixtureWorkspaceRoot() string {
	return filepath.Join(verificationFixtureVolume(), "workspace")
}

func verificationFixture() (Policy, []ProcessProfile, []EnvProfile) {
	executable := filepath.Join(verificationFixtureVolume(), "opt", "fixture", "go")
	plan := &VerificationPlan{
		FormatVersion: VerificationPlanVersion, ProcessProfileRef: "go-test", Executable: executable,
		Argv: []string{"go", "test", "./..."}, Cwd: "tests", EnvProfileRef: "ci", TimeoutSeconds: 300,
		PassCondition: VerificationExitZero,
	}
	p := Policy{
		ID: "fixture-verification", AllowedModes: []string{protocol.ModeTrustedHost}, Tools: []string{"process.exec"},
		ReadPrefixes: []string{"."}, WritePrefixes: []string{"."}, ProcessProfiles: []string{"go-test"}, EnvProfiles: []string{"ci"},
		Limits:       protocol.Limits{MaxModelSteps: 1, MaxToolCallsPerStep: 1, DeadlineSeconds: 600, MaxCaptureBytes: 4096, MaxGenerationAttempts: 1},
		Verification: plan,
	}
	processes := []ProcessProfile{{Name: "go-test", Executable: executable, IsShell: false, ArgvPrefix: []string{"go"}}}
	envs := []EnvProfile{{Name: "ci", Values: map[string]string{"CI": "1", "TOKEN": "fixture-secret"}}}
	return p, processes, envs
}

func TestVerificationPlanRequiresGrantedSafeProcessAndWorkspace(t *testing.T) {
	p, profiles, _ := verificationFixture()
	if err := p.validate(); err != nil {
		t.Fatalf("valid plan: %v", err)
	}
	if err := validateVerificationGrant(p, *p.Verification, map[string]ProcessProfile{profiles[0].Name: profiles[0]}); err != nil {
		t.Fatalf("valid process grant: %v", err)
	}
	cases := map[string]func(*Policy){
		"untrusted mode":                     func(p *Policy) { p.AllowedModes = []string{protocol.ModeStructuredOnly} },
		"process tool absent":                func(p *Policy) { p.Tools = nil },
		"profile grant absent":               func(p *Policy) { p.ProcessProfiles = nil },
		"environment grant absent":           func(p *Policy) { p.EnvProfiles = nil },
		"working directory outside prefixes": func(p *Policy) { p.ReadPrefixes, p.WritePrefixes = []string{"src"}, nil },
		"unsafe cwd":                         func(p *Policy) { p.Verification.Cwd = "../outside" },
		"unsupported condition":              func(p *Policy) { p.Verification.PassCondition = "stdout_contains" },
		"nil argv":                           func(p *Policy) { p.Verification.Argv = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			casePolicy, _, _ := verificationFixture()
			mutate(&casePolicy)
			if err := casePolicy.validate(); err == nil {
				t.Fatal("invalid verifier policy was accepted")
			}
		})
	}
	badProfiles := map[string]ProcessProfile{
		"go-test": {Name: "go-test", Executable: profiles[0].Executable, IsShell: true, ArgvPrefix: []string{"go"}},
	}
	if err := validateVerificationGrant(p, *p.Verification, badProfiles); err == nil {
		t.Fatal("shell process profile was accepted")
	}
	badProfiles["go-test"] = ProcessProfile{Name: "go-test", Executable: filepath.Join(verificationFixtureVolume(), "opt", "other", "go"), ArgvPrefix: []string{"go"}}
	if err := validateVerificationGrant(p, *p.Verification, badProfiles); err == nil {
		t.Fatal("retargeted executable was accepted")
	}
	badProfiles = map[string]ProcessProfile{
		"go-test":      profiles[0],
		"go-test-copy": {Name: "go-test-copy", Executable: profiles[0].Executable, ArgvPrefix: []string{"go"}},
	}
	ambiguous := p
	ambiguous.ProcessProfiles = []string{"go-test", "go-test-copy"}
	if err := validateVerificationGrant(ambiguous, *p.Verification, badProfiles); err == nil {
		t.Fatal("ambiguous process profile selection was accepted")
	}
}

func TestVerificationCriteriaRevisionPinsEveryResolvedInput(t *testing.T) {
	p, processes, envs := verificationFixture()
	input := EffectivePolicyInput{Policy: p, ProcessProfiles: processes, EnvProfiles: envs, WorkspaceRoot: verificationFixtureWorkspaceRoot(), Mode: protocol.ModeTrustedHost}
	policyRevision, err := EffectivePolicyRevision(input)
	if err != nil {
		t.Fatal(err)
	}
	base := EffectivePolicy{Policy: p, ProcessProfiles: processes, EnvProfiles: envs, WorkspaceRoot: input.WorkspaceRoot, Mode: input.Mode, Revision: policyRevision}
	got, err := VerificationCriteriaRevision(base)
	if err != nil {
		t.Fatal(err)
	}
	wants := map[string]string{
		"linux":   "8c1dc35869a84ab6b5c617cf1e49f1f2457af5935cf95dbccb3ceeb38dac5719",
		"darwin":  "8c1dc35869a84ab6b5c617cf1e49f1f2457af5935cf95dbccb3ceeb38dac5719",
		"windows": "b91e084f385a989978565403240f455dff1b010e85901053dd0a1e47f1421fa6",
	}
	want, ok := wants[runtime.GOOS]
	if !ok {
		t.Fatalf("no verifier digest golden for %s", runtime.GOOS)
	}
	if got != want {
		t.Fatalf("criteria revision %s, want golden %s", got, want)
	}
	variants := map[string]func(*EffectivePolicy){
		"argv":                         func(ep *EffectivePolicy) { ep.Policy.Verification.Argv[2] = "./internal/..." },
		"resolved profile argv prefix": func(ep *EffectivePolicy) { ep.ProcessProfiles[0].ArgvPrefix = []string{"go", "test"} },
		"resolved environment value":   func(ep *EffectivePolicy) { ep.EnvProfiles[0].Values["TOKEN"] = "changed-secret" },
		"resolved cwd":                 func(ep *EffectivePolicy) { ep.Policy.Verification.Cwd = "src" },
		"workspace root": func(ep *EffectivePolicy) {
			ep.WorkspaceRoot = filepath.Join(verificationFixtureVolume(), "other-workspace")
		},
		"policy selection":          func(ep *EffectivePolicy) { ep.Policy.ID = "other-policy" },
		"policy authority revision": func(ep *EffectivePolicy) { ep.Revision = strings.Repeat("a", 64) },
	}
	for name, mutate := range variants {
		t.Run(name, func(t *testing.T) {
			variant := cloneEffectivePolicyForVerification(base)
			mutate(&variant)
			next, err := VerificationCriteriaRevision(variant)
			if err != nil {
				t.Fatalf("criteria calculation: %v", err)
			}
			if next == got {
				t.Fatal("meaningful verification input change did not change the revision")
			}
		})
	}
	unsafe := cloneEffectivePolicyForVerification(base)
	unsafe.ProcessProfiles[0].IsShell = true
	if _, err := VerificationCriteriaRevision(unsafe); err == nil {
		t.Fatal("shell profile produced a criteria revision")
	}
	unsafe = cloneEffectivePolicyForVerification(base)
	unsafe.Mode = protocol.ModeStructuredOnly
	if _, err := VerificationCriteriaRevision(unsafe); err == nil {
		t.Fatal("structured-only mode produced a criteria revision")
	}
}

func TestEffectivePolicyRevisionIncludesPlanSemantics(t *testing.T) {
	p, processes, envs := verificationFixture()
	input := EffectivePolicyInput{Policy: p, ProcessProfiles: processes, EnvProfiles: envs, WorkspaceRoot: verificationFixtureWorkspaceRoot(), Mode: protocol.ModeTrustedHost}
	withPlan, err := EffectivePolicyRevision(input)
	if err != nil {
		t.Fatal(err)
	}
	withoutPlanInput := input
	withoutPlanInput.Policy.Verification = nil
	withoutPlan, err := EffectivePolicyRevision(withoutPlanInput)
	if err != nil {
		t.Fatal(err)
	}
	if withPlan == withoutPlan {
		t.Fatal("the verification plan did not change effective policy authority")
	}
	changedInput := input
	changedPlan := *input.Policy.Verification
	changedPlan.TimeoutSeconds++
	changedInput.Policy.Verification = &changedPlan
	changed, err := EffectivePolicyRevision(changedInput)
	if err != nil {
		t.Fatal(err)
	}
	if changed == withPlan {
		t.Fatal("changing a verification criterion did not change effective policy authority")
	}
}

func cloneEffectivePolicyForVerification(in EffectivePolicy) EffectivePolicy {
	out := in
	policy := *in.Policy.Verification
	policy.Argv = append([]string(nil), in.Policy.Verification.Argv...)
	out.Policy.Verification = &policy
	out.ProcessProfiles = append([]ProcessProfile(nil), in.ProcessProfiles...)
	out.ProcessProfiles[0].ArgvPrefix = append([]string(nil), in.ProcessProfiles[0].ArgvPrefix...)
	out.EnvProfiles = append([]EnvProfile(nil), in.EnvProfiles...)
	out.EnvProfiles[0].Values = map[string]string{}
	for key, value := range in.EnvProfiles[0].Values {
		out.EnvProfiles[0].Values[key] = value
	}
	return out
}
