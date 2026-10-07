// Package intake holds the pure decisions of input acceptance: who the caller is
// allowed to be, where an input's origin comes from, and what budget a Run gets.
//
// Everything here is a function of its arguments. The clock is a parameter, nothing
// reads a store, a file or the environment, and the only error values are
// *protocol.Error with the codes the protocol names. The effects that must be
// atomic with the decision (the nonce record, the receipt, the rows) belong to the
// store, which calls these functions inside its transaction.
package intake

import (
	"time"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Clock is the source of acceptance time. Production uses SystemClock; tests
// inject a fixed one. Code that decides something takes the instant as a value.
type Clock interface {
	Now() time.Time
}

// SystemClock is the wall clock.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// FixedClock always returns the same instant.
type FixedClock time.Time

// Now returns the fixed instant.
func (c FixedClock) Now() time.Time { return time.Time(c) }

// RelayKey is one trusted relay issuer: the allowlist entry together with its key.
type RelayKey struct {
	Issuer   string
	KeyID    string
	Audience string
	Key      protocol.OriginKey
}

// CallerConfig is the authenticated connection profile, fixed when the process
// starts. Nothing in a request payload can change it.
type CallerConfig struct {
	Principal                 string
	DefaultOrigin             string // the highest origin this profile may declare locally: human or automation
	ReadableSessionOwners     []string
	ControllableSessionOwners []string
	Relays                    []RelayKey
}

// Caller is a validated CallerConfig with its profile digest.
type Caller struct {
	Principal     string
	DefaultOrigin string
	ProfileDigest string

	readable     map[string]struct{}
	controllable map[string]struct{}
	relays       []RelayKey
}

// NewCaller validates the profile and computes caller_profile_digest with the
// stage 1 function, so the digest ignores list order and key material. A relay
// whose audience is not this profile's principal can never verify a proof, so it
// is refused as a configuration error, and an (issuer, key_id) pair names exactly
// one key.
func NewCaller(cfg CallerConfig) (Caller, error) {
	issuers := make([]protocol.RelayIssuer, len(cfg.Relays))
	seen := map[[2]string]bool{}
	for i, r := range cfg.Relays {
		if r.Audience != cfg.Principal {
			return Caller{}, protocol.NewError(protocol.CodeInvalidRequest, "relay issuer audience must be the caller principal").
				Wrap(invalid("relay audience is not the principal"))
		}
		id := [2]string{r.Issuer, r.KeyID}
		if seen[id] {
			return Caller{}, protocol.NewError(protocol.CodeInvalidRequest, "relay issuer and key_id must be unique").
				Wrap(invalid("duplicate relay issuer and key_id"))
		}
		seen[id] = true
		issuers[i] = protocol.RelayIssuer{Issuer: r.Issuer, KeyID: r.KeyID, Audience: r.Audience}
	}
	digest, err := protocol.CallerProfileDigest(protocol.CallerProfile{
		Principal:                 cfg.Principal,
		DefaultOrigin:             cfg.DefaultOrigin,
		ReadableSessionOwners:     cfg.ReadableSessionOwners,
		ControllableSessionOwners: cfg.ControllableSessionOwners,
		RelayIssuers:              issuers,
	})
	if err != nil {
		return Caller{}, protocol.NewError(protocol.CodeInvalidRequest, "caller profile is invalid").Wrap(err)
	}
	c := Caller{
		Principal:     cfg.Principal,
		DefaultOrigin: cfg.DefaultOrigin,
		ProfileDigest: digest,
		readable:      toSet(cfg.ReadableSessionOwners),
		controllable:  toSet(cfg.ControllableSessionOwners),
		relays:        append([]RelayKey(nil), cfg.Relays...),
	}
	return c, nil
}

func toSet(items []string) map[string]struct{} {
	m := make(map[string]struct{}, len(items))
	for _, s := range items {
		m[s] = struct{}{}
	}
	return m
}

// CanRead reports whether the caller may read sessions owned by owner: its own,
// the ones listed as readable, and the ones it may control. An empty list is not
// allow-all.
func (c Caller) CanRead(owner string) bool {
	if owner == "" {
		return false
	}
	if owner == c.Principal {
		return true
	}
	if _, ok := c.readable[owner]; ok {
		return true
	}
	return c.CanControl(owner)
}

// CanControl reports whether the caller may start work in, stop or resume sessions
// owned by owner: its own and the ones listed as controllable.
func (c Caller) CanControl(owner string) bool {
	if owner == "" {
		return false
	}
	if owner == c.Principal {
		return true
	}
	_, ok := c.controllable[owner]
	return ok
}
