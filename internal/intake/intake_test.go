package intake_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The relay vector is a synthetic proof with a synthetic key. now sits inside its
// five minute window.
type relayVector struct {
	KeyHex string               `json:"key_hex"`
	Text   string               `json:"text"`
	Proof  protocol.OriginProof `json:"proof"`
	Digest string               `json:"mac_input_sha256"`
}

func loadVector(t testing.TB) relayVector {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contract", "examples", "origin_proof_vector.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v relayVector
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (v relayVector) key(t testing.TB) protocol.OriginKey {
	t.Helper()
	k, err := protocol.ParseOriginKeyFile([]byte(v.KeyHex + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var vectorNow = time.Date(2026, 10, 7, 0, 1, 0, 0, time.UTC)

const (
	coreThread = "thr_00000000-0000-7000-8000-000000000001"
	coreKey    = "fixture.turn.start.0001"
)

func newCaller(t testing.TB, v relayVector, mod func(*intake.CallerConfig)) intake.Caller {
	t.Helper()
	cfg := intake.CallerConfig{
		Principal:                 "core:local",
		DefaultOrigin:             protocol.OriginAutomation,
		ReadableSessionOwners:     []string{"core:local", "user:ren"},
		ControllableSessionOwners: []string{"core:local"},
		Relays:                    []intake.RelayKey{{Issuer: "core:fixture", KeyID: "fixture-key-1", Audience: "core:local", Key: v.key(t)}},
	}
	if mod != nil {
		mod(&cfg)
	}
	c, err := intake.NewCaller(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func relayClaim(v relayVector) intake.Claim {
	p := v.Proof
	return intake.Claim{
		Entrypoint: protocol.EntrypointStdioCore, Method: "turn/start", IdempotencyKey: coreKey,
		DestinationThreadID: coreThread, Text: v.Text, Proof: &p,
	}
}

// resign recomputes the MAC after a field was changed, so a test can isolate the
// check it targets from the MAC check.
func resign(t testing.TB, v relayVector, mod func(p *protocol.OriginProof)) *protocol.OriginProof {
	t.Helper()
	p := v.Proof
	mod(&p)
	mac, err := protocol.OriginProofMAC(v.key(t), p)
	if err != nil {
		t.Fatal(err)
	}
	p.MAC = mac
	return &p
}

func wantProofRejected(t testing.TB, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("proof accepted")
	}
	if code := protocol.CodeOf(err); code != protocol.CodeInvalidOriginProof {
		t.Fatalf("code %q (%v), want INVALID_ORIGIN_PROOF", code, err)
	}
}

// wantProofRejectedBecause also checks the reason named in the message, so a case
// that fails for another reason (a fixture that drifted, an earlier check firing
// first) cannot pass as the condition it claims to test.
func wantProofRejectedBecause(t testing.TB, err error, reason string) {
	t.Helper()
	wantProofRejected(t, err)
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("rejected, but not because of %q: %v", reason, err)
	}
}

func TestValidRelayProofIsVerifiedRelay(t *testing.T) {
	v := loadVector(t)
	d, err := newCaller(t, v, nil).Decide(relayClaim(v), vectorNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.EffectiveOrigin != protocol.OriginHuman || d.DeclaredOrigin != protocol.OriginHuman || d.ProofBasis != protocol.ProofBasisVerifiedRelay {
		t.Fatalf("%+v", d)
	}
	if d.ProofDigest == nil || *d.ProofDigest != v.Digest {
		t.Fatalf("proof digest %v, want %s", d.ProofDigest, v.Digest)
	}
	if d.Proof == nil || d.Proof.Nonce != v.Proof.Nonce {
		t.Fatal("the verified proof must be returned for nonce recording")
	}
	sum := sha256.Sum256([]byte(v.Text))
	if hex.EncodeToString(sum[:]) != v.Proof.RawHash {
		t.Fatal("vector no longer binds the text by SHA-256")
	}
}

func TestAutomationRelayProofKeepsAutomation(t *testing.T) {
	v := loadVector(t)
	cl := relayClaim(v)
	cl.Proof = resign(t, v, func(p *protocol.OriginProof) { p.Origin = protocol.OriginAutomation })
	d, err := newCaller(t, v, nil).Decide(cl, vectorNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.EffectiveOrigin != protocol.OriginAutomation || d.ProofBasis != protocol.ProofBasisVerifiedRelay || d.ProofDigest == nil {
		t.Fatalf("%+v", d)
	}
}

func TestRelayProofRejectionsEachNamedCondition(t *testing.T) {
	v := loadVector(t)
	caller := newCaller(t, v, nil)
	tampered := func(mod func(p *protocol.OriginProof)) intake.Claim {
		cl := relayClaim(v)
		p := v.Proof
		mod(&p)
		cl.Proof = &p
		return cl
	}
	cases := map[string]intake.Claim{
		// Fields changed without re-signing: the MAC must catch every one.
		"tampered mac":           tampered(func(p *protocol.OriginProof) { p.MAC = strings.Repeat("0", 64) }),
		"tampered nonce":         tampered(func(p *protocol.OriginProof) { p.Nonce = "fixture-nonce-00000009" }),
		"tampered sequence":      tampered(func(p *protocol.OriginProof) { p.Sequence = 2 }),
		"tampered origin":        tampered(func(p *protocol.OriginProof) { p.Origin = protocol.OriginAutomation }),
		"tampered issued_at":     tampered(func(p *protocol.OriginProof) { p.IssuedAt = "2026-10-07T00:00:01Z" }),
		"tampered expires_at":    tampered(func(p *protocol.OriginProof) { p.ExpiresAt = "2026-10-07T01:00:00Z" }),
		"tampered source":        tampered(func(p *protocol.OriginProof) { p.SourceMessageID = "msg_00000000-0000-7000-8000-000000000999" }),
		"tampered source thread": tampered(func(p *protocol.OriginProof) { p.SourceThreadID = "thr_00000000-0000-7000-8000-000000000999" }),
		// Fields changed and re-signed with the right key: the binding checks must catch them.
		"different audience": func() intake.Claim {
			cl := relayClaim(v)
			cl.Proof = resign(t, v, func(p *protocol.OriginProof) { p.Audience = "user:ren" })
			return cl
		}(),
		"different destination thread": func() intake.Claim {
			cl := relayClaim(v)
			cl.Proof = resign(t, v, func(p *protocol.OriginProof) { p.DestinationThreadID = "thr_00000000-0000-7000-8000-000000000002" })
			return cl
		}(),
		"different mutation key": func() intake.Claim {
			cl := relayClaim(v)
			cl.Proof = resign(t, v, func(p *protocol.OriginProof) { p.MutationKey = "fixture.turn.start.0002" })
			return cl
		}(),
		"different raw hash": func() intake.Claim {
			cl := relayClaim(v)
			cl.Proof = resign(t, v, func(p *protocol.OriginProof) { p.RawHash = strings.Repeat("a", 64) })
			return cl
		}(),
		"unknown issuer": func() intake.Claim {
			cl := relayClaim(v)
			cl.Proof = resign(t, v, func(p *protocol.OriginProof) { p.Issuer = "core:other" })
			return cl
		}(),
		"unknown key id (rotated key)": func() intake.Claim {
			cl := relayClaim(v)
			cl.Proof = resign(t, v, func(p *protocol.OriginProof) { p.KeyID = "fixture-key-2" })
			return cl
		}(),
		// The text the proof vouches for was changed after signing.
		"text edited after signing": func() intake.Claim {
			cl := relayClaim(v)
			cl.Text += " (edited)"
			return cl
		}(),
		"text whitespace normalized": func() intake.Claim {
			cl := relayClaim(v)
			cl.Text = strings.TrimSpace(cl.Text) + "\n"
			return cl
		}(),
	}
	reasons := map[string]string{
		"tampered mac": "MAC", "tampered nonce": "MAC", "tampered sequence": "MAC", "tampered origin": "MAC", "tampered issued_at": "MAC",
		"tampered expires_at": "MAC", "tampered source": "MAC", "tampered source thread": "MAC",
		"different audience": "audience", "different destination thread": "different thread", "different mutation key": "different operation",
		"different raw hash": "cover this text", "unknown issuer": "allowed relay", "unknown key id (rotated key)": "allowed relay",
		"text edited after signing": "cover this text", "text whitespace normalized": "cover this text",
	}
	for name, cl := range cases {
		t.Run(name, func(t *testing.T) {
			d, err := caller.Decide(cl, vectorNow)
			if reasons[name] == "" {
				t.Fatalf("case %q has no expected reason", name)
			}
			wantProofRejectedBecause(t, err, reasons[name])
			if d.EffectiveOrigin != "" {
				t.Fatalf("a rejected proof must not yield a decision: %+v", d)
			}
		})
	}
}

func TestRelayProofWithoutConfiguredIssuersIsRejected(t *testing.T) {
	v := loadVector(t)
	caller := newCaller(t, v, func(c *intake.CallerConfig) { c.Relays = nil })
	_, err := caller.Decide(relayClaim(v), vectorNow)
	wantProofRejectedBecause(t, err, "allowed relay")
}

func TestRelayProofKeyMustMatchKeyID(t *testing.T) {
	v := loadVector(t)
	wrong, err := protocol.ParseOriginKeyFile([]byte(strings.Repeat("ab", 32)))
	if err != nil {
		t.Fatal(err)
	}
	caller := newCaller(t, v, func(c *intake.CallerConfig) { c.Relays[0].Key = wrong })
	_, err = caller.Decide(relayClaim(v), vectorNow)
	wantProofRejectedBecause(t, err, "MAC")
}

func TestRelayProofTimeWindow(t *testing.T) {
	v := loadVector(t)
	caller := newCaller(t, v, nil)
	issued := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	window := func(issuedAt, expiresAt time.Time) intake.Claim {
		cl := relayClaim(v)
		cl.Proof = resign(t, v, func(p *protocol.OriginProof) {
			p.IssuedAt, p.ExpiresAt = protocol.FormatTimestamp(issuedAt), protocol.FormatTimestamp(expiresAt)
		})
		return cl
	}
	at := func(base time.Time, d time.Duration) time.Time { return base.Add(d) }
	ok := map[string]struct {
		cl  intake.Claim
		now time.Time
	}{
		"inside the window":               {window(issued, at(issued, 300*time.Second)), at(issued, 60*time.Second)},
		"exactly at expiry":               {window(issued, at(issued, 300*time.Second)), at(issued, 300*time.Second)},
		"ttl of exactly 300 seconds":      {window(issued, at(issued, 300*time.Second)), issued},
		"issued 30 seconds in the future": {window(at(issued, 30*time.Second), at(issued, 90*time.Second)), issued},
	}
	for name, c := range ok {
		t.Run("accepts "+name, func(t *testing.T) {
			if _, err := caller.Decide(c.cl, c.now); err != nil {
				t.Fatal(err)
			}
		})
	}
	bad := map[string]struct {
		cl     intake.Claim
		now    time.Time
		reason string
	}{
		"one second after expiry":         {window(issued, at(issued, 300*time.Second)), at(issued, 301*time.Second), "expired"},
		"ttl of 301 seconds":              {window(issued, at(issued, 301*time.Second)), issued, "longer than 300"},
		"issued 31 seconds in the future": {window(at(issued, 31*time.Second), at(issued, 90*time.Second)), issued, "in the future"},
		"expires at issue time":           {window(issued, issued), issued, "before it is issued"},
		"expires before issue":            {window(at(issued, 10*time.Second), issued), issued, "before it is issued"},
	}
	for name, c := range bad {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := caller.Decide(c.cl, c.now)
			wantProofRejectedBecause(t, err, c.reason)
		})
	}
}

func TestOriginWithoutProof(t *testing.T) {
	v := loadVector(t)
	humanCap := func(c *intake.CallerConfig) { c.DefaultOrigin = protocol.OriginHuman }
	type want struct{ declared, effective, basis string }
	cases := []struct {
		name       string
		mod        func(*intake.CallerConfig)
		entrypoint string
		declared   string
		want       want
	}{
		{"interactive line under a human profile", humanCap, protocol.EntrypointCLIInteractive, "", want{"human", "human", "declared_local"}},
		{"interactive line under an automation profile is capped", nil, protocol.EntrypointCLIInteractive, "", want{"human", "automation", "automation"}},
		{"interactive explicit automation", humanCap, protocol.EntrypointCLIInteractive, "automation", want{"automation", "automation", "automation"}},
		{"exec without a declaration is automation", humanCap, protocol.EntrypointCLIExec, "", want{"automation", "automation", "automation"}},
		{"exec explicit human under a human profile", humanCap, protocol.EntrypointCLIExec, "human", want{"human", "human", "declared_local"}},
		{"exec explicit human under an automation profile is capped", nil, protocol.EntrypointCLIExec, "human", want{"human", "automation", "automation"}},
		{"pipe without a declaration is automation even for a human profile", humanCap, protocol.EntrypointCLIPipe, "", want{"automation", "automation", "automation"}},
		{"pipe explicit human under a human profile", humanCap, protocol.EntrypointCLIPipe, "human", want{"human", "human", "declared_local"}},
		{"explicit unknown stays unknown", humanCap, protocol.EntrypointCLIExec, "unknown", want{"unknown", "unknown", "unknown"}},
		{"CORE delegation without a proof is automation", humanCap, protocol.EntrypointStdioCore, "", want{"automation", "automation", "automation"}},
		{"stdio automation client", humanCap, protocol.EntrypointStdioAutomation, "", want{"automation", "automation", "automation"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := newCaller(t, v, c.mod).Decide(intake.Claim{
				Entrypoint: c.entrypoint, DeclaredOrigin: c.declared, Method: "turn/start", IdempotencyKey: coreKey,
				DestinationThreadID: coreThread, Text: "hello",
			}, vectorNow)
			if err != nil {
				t.Fatal(err)
			}
			if got := (want{d.DeclaredOrigin, d.EffectiveOrigin, d.ProofBasis}); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
			if d.ProofDigest != nil || d.Proof != nil {
				t.Fatal("no proof, so no proof digest")
			}
		})
	}
}

func TestClaimRefusals(t *testing.T) {
	v := loadVector(t)
	base := intake.Claim{Entrypoint: protocol.EntrypointCLIExec, Method: "turn/start", IdempotencyKey: coreKey, DestinationThreadID: coreThread, Text: "x"}
	for name, mod := range map[string]func(c *intake.Claim){
		"stdio cannot carry an operator declaration": func(c *intake.Claim) { c.Entrypoint, c.DeclaredOrigin = protocol.EntrypointStdioCore, "human" },
		"stdio automation cannot declare": func(c *intake.Claim) {
			c.Entrypoint, c.DeclaredOrigin = protocol.EntrypointStdioAutomation, "automation"
		},
		"unknown entrypoint":  func(c *intake.Claim) { c.Entrypoint = "tui" },
		"unknown declaration": func(c *intake.Claim) { c.DeclaredOrigin = "root" },
		"declaration disagreeing with the proof": func(c *intake.Claim) {
			*c = relayClaim(v)
			c.Entrypoint, c.DeclaredOrigin = protocol.EntrypointCLIExec, "automation"
		},
	} {
		t.Run(name, func(t *testing.T) {
			cl := base
			mod(&cl)
			_, err := newCaller(t, v, nil).Decide(cl, vectorNow)
			if err == nil {
				t.Fatal("accepted")
			}
			if code := protocol.CodeOf(err); code != protocol.CodeInvalidRequest && code != protocol.CodeInvalidOriginProof {
				t.Fatalf("code %s", code)
			}
		})
	}
}

func TestNewCallerValidation(t *testing.T) {
	v := loadVector(t)
	good := func(mod func(c *intake.CallerConfig)) error {
		cfg := intake.CallerConfig{
			Principal: "core:local", DefaultOrigin: protocol.OriginAutomation,
			Relays: []intake.RelayKey{{Issuer: "core:fixture", KeyID: "k1", Audience: "core:local", Key: v.key(t)}},
		}
		mod(&cfg)
		_, err := intake.NewCaller(cfg)
		return err
	}
	if err := good(func(*intake.CallerConfig) {}); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(c *intake.CallerConfig){
		"bad principal":                 func(c *intake.CallerConfig) { c.Principal = "Core" },
		"bad default origin":            func(c *intake.CallerConfig) { c.DefaultOrigin = "unknown" },
		"audience is not the principal": func(c *intake.CallerConfig) { c.Relays[0].Audience = "user:ren" },
		"duplicate issuer and key id": func(c *intake.CallerConfig) {
			c.Relays = append(c.Relays, c.Relays[0])
			c.Relays[1].Audience = "core:local"
		},
		"same key id under two audiences": func(c *intake.CallerConfig) {
			c.Relays = append(c.Relays, intake.RelayKey{Issuer: "core:fixture", KeyID: "k1", Audience: "core:other", Key: v.key(t)})
		},
		"duplicate owner":               func(c *intake.CallerConfig) { c.ReadableSessionOwners = []string{"user:ren", "user:ren"} },
		"owner that is not a principal": func(c *intake.CallerConfig) { c.ControllableSessionOwners = []string{"everyone"} },
	} {
		t.Run(name, func(t *testing.T) {
			if err := good(mod); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestCallerProfileDigestIgnoresKeysAndOrder(t *testing.T) {
	v := loadVector(t)
	a := newCaller(t, v, nil)
	other, err := protocol.ParseOriginKeyFile([]byte(strings.Repeat("cd", 32)))
	if err != nil {
		t.Fatal(err)
	}
	b := newCaller(t, v, func(c *intake.CallerConfig) {
		c.Relays[0].Key = other
		c.ReadableSessionOwners = []string{"user:ren", "core:local"}
	})
	if a.ProfileDigest == "" || a.ProfileDigest != b.ProfileDigest {
		t.Fatalf("digest depends on key bytes or owner order: %s vs %s", a.ProfileDigest, b.ProfileDigest)
	}
	want, err := protocol.CallerProfileDigest(protocol.CallerProfile{
		Principal: "core:local", DefaultOrigin: protocol.OriginAutomation,
		ReadableSessionOwners: []string{"core:local", "user:ren"}, ControllableSessionOwners: []string{"core:local"},
		RelayIssuers: []protocol.RelayIssuer{{Issuer: "core:fixture", KeyID: "fixture-key-1", Audience: "core:local"}},
	})
	if err != nil || a.ProfileDigest != want {
		t.Fatalf("digest %s, want the stage 1 digest %s (%v)", a.ProfileDigest, want, err)
	}
	c := newCaller(t, v, func(c *intake.CallerConfig) { c.DefaultOrigin = protocol.OriginHuman })
	if c.ProfileDigest == a.ProfileDigest {
		t.Fatal("default_origin must be part of the digest")
	}
}

func TestSessionAccessControl(t *testing.T) {
	v := loadVector(t)
	c := newCaller(t, v, func(c *intake.CallerConfig) {
		c.ReadableSessionOwners = []string{"user:ren"}
		c.ControllableSessionOwners = []string{"user:mio"}
	})
	cases := []struct {
		owner         string
		read, control bool
	}{
		{"core:local", true, true}, // a caller always has its own sessions
		{"user:ren", true, false},  // readable only
		{"user:mio", true, true},   // controllable implies readable
		{"user:other", false, false},
		{"", false, false},
	}
	for _, tc := range cases {
		if got := c.CanRead(tc.owner); got != tc.read {
			t.Errorf("CanRead(%q) = %v", tc.owner, got)
		}
		if got := c.CanControl(tc.owner); got != tc.control {
			t.Errorf("CanControl(%q) = %v", tc.owner, got)
		}
	}
	empty := newCaller(t, v, func(c *intake.CallerConfig) { c.ReadableSessionOwners, c.ControllableSessionOwners = nil, nil })
	if empty.CanRead("user:ren") || empty.CanControl("user:ren") {
		t.Fatal("empty lists must not mean allow-all")
	}
}
