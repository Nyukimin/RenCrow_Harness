package config

import (
	"errors"
	"strings"

	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
)

const (
	VerificationPlanVersion = "rencrow-verification-plan/v1"
	VerificationExitZero    = "exit_zero"
)

// VerificationPlan is an optional host-managed fixed process check attached to a
// policy. It is configuration data, not an RPC or model Tool contract.
type VerificationPlan struct {
	FormatVersion     string   `json:"format_version"`
	ProcessProfileRef string   `json:"process_profile_ref"`
	Executable        string   `json:"executable"`
	Argv              []string `json:"argv"`
	Cwd               string   `json:"cwd"`
	EnvProfileRef     string   `json:"env_profile_ref"`
	TimeoutSeconds    int64    `json:"timeout_seconds"`
	PassCondition     string   `json:"pass_condition"`
}

func encodeVerificationPlan(p VerificationPlan) ([]byte, error) {
	if p.FormatVersion != VerificationPlanVersion || p.ProcessProfileRef == "" || p.Executable == "" ||
		p.Argv == nil || p.Cwd == "" || p.EnvProfileRef == "" || p.TimeoutSeconds < 1 || p.PassCondition != VerificationExitZero {
		return nil, errors.New("config: invalid verification plan")
	}
	argv := make([]any, len(p.Argv))
	for i, arg := range p.Argv {
		if strings.ContainsRune(arg, 0) {
			return nil, errors.New("config: invalid verification argument")
		}
		argv[i] = arg
	}
	return canon.Encode(map[string]any{
		"format_version":      p.FormatVersion,
		"process_profile_ref": p.ProcessProfileRef,
		"executable":          p.Executable,
		"argv":                argv,
		"cwd":                 p.Cwd,
		"env_profile_ref":     p.EnvProfileRef,
		"timeout_seconds":     p.TimeoutSeconds,
		"pass_condition":      p.PassCondition,
	})
}
