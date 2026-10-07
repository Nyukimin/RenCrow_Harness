package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"

	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
)

// RecoveryPolicyRevision is the revision of the recovery policy a Run freezes at
// acceptance:
//
//	hex(SHA256("rencrow-recovery-policy/v1\0" + LP(contract_version) +
//	           LP(decimal(max_attempts_per_act)) + LP(profile)... in byte order))
//
// The profile set is a set: order does not matter, duplicates and an empty set are
// rejected. The caller's slice is not modified. This is a different version from
// the LLM side's profile_revision.
func RecoveryPolicyRevision(contractVersion string, maxAttemptsPerAct int, allowedProfiles []string) (string, error) {
	if err := checkUTF8("recovery policy", append([]string{contractVersion}, allowedProfiles...)...); err != nil {
		return "", err
	}
	if maxAttemptsPerAct < 0 {
		return "", invalid("max_attempts_per_act must not be negative")
	}
	if len(allowedProfiles) == 0 {
		return "", invalid("allowed_profiles must not be empty")
	}
	profiles := slices.Clone(allowedProfiles)
	slices.Sort(profiles)
	for i := 1; i < len(profiles); i++ {
		if profiles[i] == profiles[i-1] {
			return "", invalid("allowed_profiles has a duplicate entry")
		}
	}
	var b bytes.Buffer
	b.WriteString("rencrow-recovery-policy/v1\x00")
	b.Write(canon.LP([]byte(contractVersion)))
	b.Write(canon.LP([]byte(strconv.Itoa(maxAttemptsPerAct))))
	for _, p := range profiles {
		b.Write(canon.LP([]byte(p)))
	}
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:]), nil
}
