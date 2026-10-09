package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// cmdVerificationDigest prints only the digest Core pins into its Task profile.
// It loads the effective host policy but opens no store and emits no command,
// argv, environment value, or workspace path.
func cmdVerificationDigest(args []string, out io.Writer) *exitError {
	fs := newFlags("verification-digest")
	configPath := fs.String("config", "", "absolute path of the Harness configuration")
	policyRef := fs.String("policy-ref", "", "the workspace policy reference")
	workspace := fs.String("workspace", "", "absolute path of the workspace")
	mode := fs.String("mode", protocol.ModeTrustedHost, "the effective execution mode")
	if e := parseFlags(fs, args); e != nil {
		return e
	}
	if e := requireAbs("--config", *configPath); e != nil {
		return e
	}
	if e := requireAbs("--workspace", *workspace); e != nil {
		return e
	}
	if *policyRef == "" {
		return usageErr("--policy-ref is required")
	}
	if *mode != protocol.ModeTrustedHost {
		return usageErr("--mode must be trusted_host for a process verifier")
	}
	dep, err := config.Load(*configPath)
	if err != nil {
		return usageErr("the configuration is invalid: %v", err)
	}
	effective, err := dep.EffectivePolicy(*policyRef, *workspace, *mode)
	if err != nil {
		return usageErr("the selected policy is not an effective verifier policy")
	}
	if effective.Policy.Verification == nil || effective.VerificationCriteriaRevision == "" {
		return usageErr("the selected policy has no valid verification plan")
	}
	body, err := json.Marshal(struct {
		CriteriaRevision string `json:"criteria_revision"`
	}{effective.VerificationCriteriaRevision})
	if err != nil {
		return failure("the verification digest could not be encoded")
	}
	if _, err := fmt.Fprintf(out, "%s\n", body); err != nil {
		return failure("the verification digest could not be written")
	}
	return nil
}
