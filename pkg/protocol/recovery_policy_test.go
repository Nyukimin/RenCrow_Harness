package protocol_test

import (
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func TestRecoveryPolicyRevisionGolden(t *testing.T) {
	var cfg struct {
		Recovery struct {
			ContractVersion string   `json:"contract_version"`
			MaxAttempts     int      `json:"max_attempts_per_act"`
			AllowedProfiles []string `json:"allowed_profiles"`
		} `json:"recovery"`
	}
	exampleJSON(t, "config.json", &cfg)
	var start struct {
		Revision string `json:"recovery_policy_revision"`
	}
	exampleJSON(t, "start_result.json", &start)
	var resume struct {
		Revision string `json:"recovery_policy_revision"`
	}
	exampleJSON(t, "resume_result.json", &resume)
	const want = "db87835a3c36bf3f845441203fc0a5f90baa4d42bb05a7298935bdbef0edf0e9"
	if start.Revision != want || resume.Revision != want {
		t.Fatalf("vector drifted: %s %s", start.Revision, resume.Revision)
	}
	r := cfg.Recovery
	got, err := protocol.RecoveryPolicyRevision(r.ContractVersion, r.MaxAttempts, r.AllowedProfiles)
	if err != nil || got != want {
		t.Fatalf("got %s, %v; want %s", got, err, want)
	}
}

func TestRecoveryPolicyRevisionProperties(t *testing.T) {
	base, err := protocol.RecoveryPolicyRevision("act-recovery/v1", 2, []string{"same_request", "terminal_output_once"})
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := protocol.RecoveryPolicyRevision("act-recovery/v1", 2, []string{"terminal_output_once", "same_request"})
	if err != nil || reordered != base {
		t.Fatalf("profile order must not matter: %s, %v", reordered, err)
	}
	differ := map[string]func() (string, error){
		"contract version": func() (string, error) {
			return protocol.RecoveryPolicyRevision("act-recovery/v2", 2, []string{"same_request", "terminal_output_once"})
		},
		"attempts": func() (string, error) {
			return protocol.RecoveryPolicyRevision("act-recovery/v1", 3, []string{"same_request", "terminal_output_once"})
		},
		"fewer profiles": func() (string, error) {
			return protocol.RecoveryPolicyRevision("act-recovery/v1", 2, []string{"same_request"})
		},
		"other profile": func() (string, error) {
			return protocol.RecoveryPolicyRevision("act-recovery/v1", 2, []string{"same_request", "other"})
		},
		// Length prefixing keeps these two apart: ["ab","c"] and ["a","bc"].
		"boundary a": func() (string, error) { return protocol.RecoveryPolicyRevision("v", 1, []string{"ab", "c"}) },
		"boundary b": func() (string, error) { return protocol.RecoveryPolicyRevision("v", 1, []string{"a", "bc"}) },
	}
	seen := map[string]string{base: "base"}
	for name, f := range differ {
		got, err := f()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%s collides with %s", name, prev)
		}
		seen[got] = name
	}

	input := []string{"terminal_output_once", "same_request"}
	if _, err := protocol.RecoveryPolicyRevision("act-recovery/v1", 2, input); err != nil {
		t.Fatal(err)
	}
	if input[0] != "terminal_output_once" {
		t.Fatal("the caller's slice must not be reordered")
	}
}

func TestRecoveryPolicyRevisionRejects(t *testing.T) {
	cases := map[string]func() (string, error){
		"duplicate profile": func() (string, error) {
			return protocol.RecoveryPolicyRevision("act-recovery/v1", 2, []string{"same_request", "same_request"})
		},
		"no profiles": func() (string, error) { return protocol.RecoveryPolicyRevision("act-recovery/v1", 2, nil) },
		"negative attempts": func() (string, error) {
			return protocol.RecoveryPolicyRevision("act-recovery/v1", -1, []string{"same_request"})
		},
		"invalid utf8 version": func() (string, error) {
			return protocol.RecoveryPolicyRevision("a\xff", 2, []string{"same_request"})
		},
		"invalid utf8 profile": func() (string, error) {
			return protocol.RecoveryPolicyRevision("act-recovery/v1", 2, []string{"a\xff"})
		},
	}
	for name, f := range cases {
		if got, err := f(); err == nil {
			t.Errorf("%s accepted: %s", name, got)
		}
	}
}
