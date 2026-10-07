package protocol

import (
	"regexp"
	"slices"
	"strings"

	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
)

// principalPattern is the principal grammar, identical to the Principal pattern in
// schemas/protocol.schema.json. Go's $ matches only at the end of the text, as
// the schema regex dialect requires.
var principalPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}:[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidatePrincipal checks the authenticated-subject label grammar, for example
// "user:ren" or "core:local". Case is significant and nothing is trimmed.
func ValidatePrincipal(s string) error {
	if !principalPattern.MatchString(s) {
		return invalid("principal does not match the principal grammar")
	}
	return nil
}

// RelayIssuer is the part of a configured relay issuer that identifies it. The
// key file path and the key bytes are intentionally not representable here.
type RelayIssuer struct {
	Issuer   string `json:"issuer"`
	KeyID    string `json:"key_id"`
	Audience string `json:"audience"`
}

// CallerProfile is the part of the configured caller that the digest covers.
type CallerProfile struct {
	Principal                 string        `json:"principal"`
	DefaultOrigin             string        `json:"default_origin"`
	ReadableSessionOwners     []string      `json:"readable_session_owners"`
	ControllableSessionOwners []string      `json:"controllable_session_owners"`
	RelayIssuers              []RelayIssuer `json:"relay_issuers"`
}

// CallerProfileDigest is D("rencrow-caller-profile/v1", C) with C holding the
// principal, default origin, both owner lists and the relay issuers. Owner lists
// and relay issuers are sets: duplicates are rejected and the order is normalized
// to UTF-8 byte order (relay issuers by issuer, key_id, audience), so reordering
// the configuration does not change the digest. Key file paths and secret bytes
// never enter it, so a key change must come with a new key_id. nil and empty lists
// are the same empty array. The caller's slices are not modified.
func CallerProfileDigest(c CallerProfile) (string, error) {
	if err := ValidatePrincipal(c.Principal); err != nil {
		return "", err
	}
	if c.DefaultOrigin != "human" && c.DefaultOrigin != "automation" {
		return "", invalid("default_origin must be human or automation")
	}
	readable, err := sortedUniquePrincipals("readable_session_owners", c.ReadableSessionOwners)
	if err != nil {
		return "", err
	}
	controllable, err := sortedUniquePrincipals("controllable_session_owners", c.ControllableSessionOwners)
	if err != nil {
		return "", err
	}
	relays := slices.Clone(c.RelayIssuers)
	for _, r := range relays {
		if r.Issuer == "" || r.KeyID == "" || r.Audience == "" {
			return "", invalid("relay issuer, key_id and audience must be non-empty")
		}
		if err := checkUTF8("relay issuer", r.Issuer, r.KeyID, r.Audience); err != nil {
			return "", err
		}
	}
	slices.SortFunc(relays, func(a, b RelayIssuer) int {
		if d := strings.Compare(a.Issuer, b.Issuer); d != 0 {
			return d
		}
		if d := strings.Compare(a.KeyID, b.KeyID); d != 0 {
			return d
		}
		return strings.Compare(a.Audience, b.Audience)
	})
	relayValues := make([]any, len(relays))
	for i, r := range relays {
		if i > 0 && r == relays[i-1] {
			return "", invalid("relay_issuers has a duplicate entry")
		}
		relayValues[i] = map[string]any{"issuer": r.Issuer, "key_id": r.KeyID, "audience": r.Audience}
	}
	digest, err := canon.D("rencrow-caller-profile/v1", map[string]any{
		"principal":                   c.Principal,
		"default_origin":              c.DefaultOrigin,
		"readable_session_owners":     readable,
		"controllable_session_owners": controllable,
		"relay_issuers":               relayValues,
	})
	if err != nil {
		return "", invalidWrap(err, "caller profile")
	}
	return digest, nil
}

func sortedUniquePrincipals(field string, owners []string) ([]any, error) {
	sorted := slices.Clone(owners)
	slices.Sort(sorted)
	out := make([]any, len(sorted))
	for i, o := range sorted {
		if err := ValidatePrincipal(o); err != nil {
			return nil, invalid("%s has an entry that is not a principal", field)
		}
		if i > 0 && o == sorted[i-1] {
			return nil, invalid("%s has a duplicate entry", field)
		}
		out[i] = o
	}
	return out, nil
}
