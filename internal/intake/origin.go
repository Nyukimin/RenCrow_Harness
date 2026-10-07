package intake

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Limits on a relay proof's lifetime (CORE_INTEGRATION section 2).
const (
	MaxProofTTL        = 300 * time.Second
	MaxProofFutureSkew = 30 * time.Second
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", protocol.ErrInvalidInput, fmt.Sprintf(format, args...))
}

func proofRejected(reason string) error {
	return protocol.NewError(protocol.CodeInvalidOriginProof, "origin proof rejected: %s", reason).
		Wrap(invalid("origin proof: %s", reason))
}

// Claim is everything the entrypoint presents about one input: where it came
// from, what the operator declared (CLI only) and the proof, if any, together
// with the operation it must be bound to.
type Claim struct {
	// Entrypoint is one of the five protocol entrypoints. It is set by the code
	// that accepted the input (CLI command or stdio profile), not by the payload,
	// and never inferred from whether stdin is a terminal.
	Entrypoint string
	// DeclaredOrigin is the operator's explicit declaration (--origin) at a CLI
	// entrypoint: "human", "automation", "unknown", or "" for none. Stdio
	// entrypoints cannot carry one.
	DeclaredOrigin string

	Method              string // "turn/start" or "input/append"
	IdempotencyKey      string
	DestinationThreadID string
	Text                string // the raw input text exactly as received
	Proof               *protocol.OriginProof
}

// Decision is the origin of one accepted input and why.
type Decision struct {
	DeclaredOrigin  string
	EffectiveOrigin string
	ProofBasis      string
	// ProofDigest and Proof are set only for a verified relay. Proof is returned
	// so the caller can record its nonce; it is never part of a rejected decision.
	ProofDigest *string
	Proof       *protocol.OriginProof
}

var entrypoints = map[string]bool{
	protocol.EntrypointCLIInteractive:  true,
	protocol.EntrypointCLIExec:         true,
	protocol.EntrypointCLIPipe:         true,
	protocol.EntrypointStdioCore:       true,
	protocol.EntrypointStdioAutomation: true,
}

func isCLI(entrypoint string) bool {
	return entrypoint == protocol.EntrypointCLIInteractive || entrypoint == protocol.EntrypointCLIExec || entrypoint == protocol.EntrypointCLIPipe
}

// Decide (the pure half of F34) settles the origin of one input.
//
// A proof, when present, is verified or the input is refused with
// INVALID_ORIGIN_PROOF: a proof that claims human and does not verify is never
// downgraded to automation. Without a proof:
//
//   - an interactive line is a human declaration, an exec or pipe input is
//     automation unless the operator explicitly declared otherwise, and a stdio
//     input (CORE delegation or another automation client) is automation;
//   - the effective origin is the declared one capped by the profile's
//     default_origin, so a human declaration under an automation profile is
//     recorded as declared human, effective automation, and never exceeds the host;
//   - "human" is never effective without either a human profile declaring it
//     (declared_local, which does not prove the physical person) or a verified proof.
//
// Nonce uniqueness needs the store and is the caller's step after this succeeds.
func (c Caller) Decide(cl Claim, now time.Time) (Decision, error) {
	if !entrypoints[cl.Entrypoint] {
		return Decision{}, protocol.NewError(protocol.CodeInvalidRequest, "unknown entrypoint").Wrap(invalid("entrypoint"))
	}
	switch cl.DeclaredOrigin {
	case "", protocol.OriginHuman, protocol.OriginAutomation, protocol.OriginUnknown:
	default:
		return Decision{}, protocol.NewError(protocol.CodeInvalidRequest, "unknown declared origin").Wrap(invalid("declared origin"))
	}
	if cl.DeclaredOrigin != "" && !isCLI(cl.Entrypoint) {
		return Decision{}, protocol.NewError(protocol.CodeInvalidRequest, "a stdio entrypoint cannot carry an operator origin declaration").
			Wrap(invalid("declaration on a stdio entrypoint"))
	}
	if cl.Proof != nil {
		return c.decideRelay(cl, now)
	}

	declared := cl.DeclaredOrigin
	if declared == "" {
		declared = protocol.OriginAutomation
		if cl.Entrypoint == protocol.EntrypointCLIInteractive {
			declared = protocol.OriginHuman
		}
	}
	switch declared {
	case protocol.OriginUnknown:
		return Decision{DeclaredOrigin: declared, EffectiveOrigin: protocol.OriginUnknown, ProofBasis: protocol.ProofBasisUnknown}, nil
	case protocol.OriginHuman:
		if c.DefaultOrigin == protocol.OriginHuman {
			return Decision{DeclaredOrigin: declared, EffectiveOrigin: protocol.OriginHuman, ProofBasis: protocol.ProofBasisDeclaredLocal}, nil
		}
	}
	return Decision{DeclaredOrigin: declared, EffectiveOrigin: protocol.OriginAutomation, ProofBasis: protocol.ProofBasisAutomation}, nil
}

// decideRelay verifies a relay proof. The order is allowlist, MAC, bindings,
// lifetime: nothing derived from a field is trusted before the MAC covering it has
// been checked, and every check after the MAC compares authenticated values.
func (c Caller) decideRelay(cl Claim, now time.Time) (Decision, error) {
	p := *cl.Proof
	if cl.DeclaredOrigin != "" && cl.DeclaredOrigin != p.Origin {
		return Decision{}, proofRejected("the operator declaration disagrees with the proof")
	}

	var relay *RelayKey
	for i := range c.relays {
		if c.relays[i].Issuer == p.Issuer && c.relays[i].KeyID == p.KeyID {
			relay = &c.relays[i]
			break
		}
	}
	if relay == nil {
		return Decision{}, proofRejected("issuer and key id are not an allowed relay")
	}
	if p.Audience != c.Principal || p.Audience != relay.Audience {
		return Decision{}, proofRejected("proof is for a different audience")
	}
	if err := protocol.VerifyOriginProofMAC(relay.Key, p); err != nil {
		return Decision{}, proofRejected("MAC does not verify")
	}

	if p.DestinationThreadID != cl.DestinationThreadID {
		return Decision{}, proofRejected("proof is bound to a different thread")
	}
	if p.MutationKey != cl.IdempotencyKey {
		return Decision{}, proofRejected("proof is bound to a different operation")
	}
	sum := sha256.Sum256([]byte(cl.Text))
	if p.RawHash != hex.EncodeToString(sum[:]) {
		return Decision{}, proofRejected("proof does not cover this text")
	}

	issued, err := protocol.ParseTimestamp(p.IssuedAt)
	if err != nil {
		return Decision{}, proofRejected("issued_at is not a valid timestamp")
	}
	expires, err := protocol.ParseTimestamp(p.ExpiresAt)
	if err != nil {
		return Decision{}, proofRejected("expires_at is not a valid timestamp")
	}
	ttl := expires.Sub(issued)
	if ttl <= 0 {
		return Decision{}, proofRejected("proof expires before it is issued")
	}
	if ttl > MaxProofTTL {
		return Decision{}, proofRejected("proof lifetime is longer than 300 seconds")
	}
	if issued.After(now.Add(MaxProofFutureSkew)) {
		return Decision{}, proofRejected("proof is issued in the future")
	}
	if now.After(expires) {
		return Decision{}, proofRejected("proof has expired")
	}

	// The proof also names the original message (source_message_id, source_thread_id,
	// sequence). Those values are authenticated by the MAC and their form is checked
	// by the schema, but whether they match the original is a fact only CORE's
	// accepted-original store can confirm, and CORE_INTEGRATION section 2 puts that
	// comparison on the signer's side. Nothing more can be checked here.

	digest, err := protocol.OriginProofDigest(p)
	if err != nil {
		return Decision{}, proofRejected("proof cannot be digested")
	}
	return Decision{
		DeclaredOrigin:  p.Origin,
		EffectiveOrigin: p.Origin,
		ProofBasis:      protocol.ProofBasisVerifiedRelay,
		ProofDigest:     &digest,
		Proof:           &p,
	}, nil
}
