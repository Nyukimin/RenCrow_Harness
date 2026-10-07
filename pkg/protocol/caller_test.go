package protocol_test

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func TestCallerProfileDigestGolden(t *testing.T) {
	var v struct {
		Caller         protocol.CallerProfile `json:"caller"`
		ExpectedDigest string                 `json:"expected_digest"`
	}
	exampleJSON(t, "wire/caller_vector.json", &v)
	got, err := protocol.CallerProfileDigest(v.Caller)
	if err != nil || got != v.ExpectedDigest {
		t.Fatalf("got %s, %v; want %s", got, err, v.ExpectedDigest)
	}
	// The vector lists the owners as [user:ren, core:local]; the digest sorts them.
	if v.Caller.ReadableSessionOwners[0] != "user:ren" {
		t.Fatal("vector no longer exercises owner reordering")
	}
}

func relayProfile() protocol.CallerProfile {
	return protocol.CallerProfile{
		Principal:                 "user:ren",
		DefaultOrigin:             "human",
		ReadableSessionOwners:     []string{"user:ren", "core:local"},
		ControllableSessionOwners: []string{"user:ren"},
		RelayIssuers: []protocol.RelayIssuer{
			{Issuer: "core:fixture", KeyID: "fixture-key-2", Audience: "user:ren"},
			{Issuer: "core:fixture", KeyID: "fixture-key-1", Audience: "user:ren"},
		},
	}
}

func TestCallerProfileDigestWithRelayIssuers(t *testing.T) {
	// Assembled independently in Python: sorted owners, sorted relay tuples, no key file.
	const want = "eb7eede37543810f6413ab77aa70fdf56254a84a7d8725f1fa1329566fc813d5"
	got, err := protocol.CallerProfileDigest(relayProfile())
	if err != nil || got != want {
		t.Fatalf("got %s, %v; want %s", got, err, want)
	}
}

func TestCallerProfileDigestIgnoresListOrderButNotMembership(t *testing.T) {
	base, _ := protocol.CallerProfileDigest(relayProfile())

	shuffled := relayProfile()
	shuffled.ReadableSessionOwners = []string{"core:local", "user:ren"}
	shuffled.RelayIssuers[0], shuffled.RelayIssuers[1] = shuffled.RelayIssuers[1], shuffled.RelayIssuers[0]
	if got, err := protocol.CallerProfileDigest(shuffled); err != nil || got != base {
		t.Fatalf("ACL order must not matter: %s, %v", got, err)
	}

	caller := relayProfile()
	input := append([]string(nil), caller.ReadableSessionOwners...)
	if _, err := protocol.CallerProfileDigest(caller); err != nil {
		t.Fatal(err)
	}
	if strings.Join(caller.ReadableSessionOwners, ",") != strings.Join(input, ",") {
		t.Fatal("the caller's slice must not be reordered")
	}

	mutations := map[string]func(c *protocol.CallerProfile){
		"principal":    func(c *protocol.CallerProfile) { c.Principal = "user:other" },
		"origin":       func(c *protocol.CallerProfile) { c.DefaultOrigin = "automation" },
		"readable":     func(c *protocol.CallerProfile) { c.ReadableSessionOwners = append(c.ReadableSessionOwners, "user:x") },
		"controllable": func(c *protocol.CallerProfile) { c.ControllableSessionOwners = nil },
		"key id":       func(c *protocol.CallerProfile) { c.RelayIssuers[0].KeyID = "fixture-key-9" },
		"issuer":       func(c *protocol.CallerProfile) { c.RelayIssuers[0].Issuer = "core:other" },
		"audience":     func(c *protocol.CallerProfile) { c.RelayIssuers[0].Audience = "core:local" },
		"drop relay":   func(c *protocol.CallerProfile) { c.RelayIssuers = c.RelayIssuers[:1] },
	}
	for name, mutate := range mutations {
		c := relayProfile()
		mutate(&c)
		if got, err := protocol.CallerProfileDigest(c); err != nil || got == base {
			t.Errorf("%s must change the digest (got %s, %v)", name, got, err)
		}
	}
}

func TestCallerProfileNilAndEmptyListsAreTheSameEmptyArray(t *testing.T) {
	a := relayProfile()
	a.ControllableSessionOwners, a.RelayIssuers = nil, nil
	b := relayProfile()
	b.ControllableSessionOwners, b.RelayIssuers = []string{}, []protocol.RelayIssuer{}
	da, errA := protocol.CallerProfileDigest(a)
	db, errB := protocol.CallerProfileDigest(b)
	if errA != nil || errB != nil || da != db {
		t.Fatalf("nil and empty must both mean []: %s %s %v %v", da, db, errA, errB)
	}
}

func TestCallerProfileKeyFileNeverReachesTheDigest(t *testing.T) {
	// The config entry carries key_file; the DTO has no field for it, so decoding the
	// config shape cannot let a path or secret into the hash.
	var cfg struct {
		Caller protocol.CallerProfile `json:"caller"`
	}
	text := `{"caller":{"principal":"user:ren","default_origin":"human","readable_session_owners":[],"controllable_session_owners":[],
	  "relay_issuers":[{"issuer":"core:fixture","key_file":"/KEY/ONE","key_id":"k1","audience":"user:ren"}]}}`
	if err := json.Unmarshal([]byte(text), &cfg); err != nil {
		t.Fatal(err)
	}
	a, err := protocol.CallerProfileDigest(cfg.Caller)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(strings.Replace(text, "/KEY/ONE", "/KEY/TWO", 1)), &cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := protocol.CallerProfileDigest(cfg.Caller)
	if a != b {
		t.Fatal("the key file path must not influence the digest")
	}
}

func TestCallerProfileDigestRejects(t *testing.T) {
	cases := map[string]func(c *protocol.CallerProfile){
		"bad principal":        func(c *protocol.CallerProfile) { c.Principal = "User:ren" },
		"empty principal":      func(c *protocol.CallerProfile) { c.Principal = "" },
		"bad origin":           func(c *protocol.CallerProfile) { c.DefaultOrigin = "Human" },
		"empty origin":         func(c *protocol.CallerProfile) { c.DefaultOrigin = "" },
		"duplicate readable":   func(c *protocol.CallerProfile) { c.ReadableSessionOwners = []string{"user:ren", "user:ren"} },
		"duplicate controllab": func(c *protocol.CallerProfile) { c.ControllableSessionOwners = []string{"core:local", "core:local"} },
		"bad owner":            func(c *protocol.CallerProfile) { c.ReadableSessionOwners = []string{"user:ren", "not a principal"} },
		"duplicate relay":      func(c *protocol.CallerProfile) { c.RelayIssuers[1] = c.RelayIssuers[0] },
		"empty issuer":         func(c *protocol.CallerProfile) { c.RelayIssuers[0].Issuer = "" },
		"empty key id":         func(c *protocol.CallerProfile) { c.RelayIssuers[0].KeyID = "" },
		"empty audience":       func(c *protocol.CallerProfile) { c.RelayIssuers[0].Audience = "" },
		"invalid utf8 issuer":  func(c *protocol.CallerProfile) { c.RelayIssuers[0].Issuer = "a\xff" },
	}
	for name, mutate := range cases {
		c := relayProfile()
		mutate(&c)
		if got, err := protocol.CallerProfileDigest(c); !errors.Is(err, protocol.ErrInvalidInput) {
			t.Errorf("%s: got %s, %v; want ErrInvalidInput", name, got, err)
		}
	}
}

func TestPrincipalGrammar(t *testing.T) {
	ok := []string{
		"user:ren", "core:local", "a:b", "user:Ren", "a_b-c:X.y_z-0", "u:0", "user:a.b",
		strings.Repeat("a", 32) + ":" + strings.Repeat("B", 64),
	}
	bad := []string{
		"", "user", ":ren", "user:", "User:ren", "1user:ren", "_user:ren", "-user:ren", "user:-ren", "user:.ren", "user:_ren",
		"user:ren ", " user:ren", "user:ren\n", "user: ren", "user:ren:x", "user ren", "user:re/n", "user:re@n", "user:re+n",
		"user:ren\x00", "ユーザー:ren", "user:rén", strings.Repeat("a", 33) + ":x", "a:" + strings.Repeat("b", 65),
	}
	for _, s := range ok {
		if err := protocol.ValidatePrincipal(s); err != nil {
			t.Errorf("%q rejected: %v", s, err)
		}
	}
	for _, s := range bad {
		if err := protocol.ValidatePrincipal(s); !errors.Is(err, protocol.ErrInvalidInput) {
			t.Errorf("%q accepted or wrong error: %v", s, err)
		}
	}
}

func TestPrincipalGrammarMatchesSchemaPattern(t *testing.T) {
	raw, err := os.ReadFile(schemaPath("schemas", "protocol.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	pattern := doc.Defs["Principal"].Pattern
	if pattern != `^[a-z][a-z0-9_-]{0,31}:[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` {
		t.Fatalf("schema Principal pattern changed: %q", pattern)
	}
	re := regexp.MustCompile(pattern)
	for _, s := range []string{
		"user:ren", "User:ren", "user:ren\n", "u:", "u:-", "ab:cd.ef", "é:x", "u:é", strings.Repeat("a", 33) + ":x",
		"a:" + strings.Repeat("b", 64), "a:" + strings.Repeat("b", 65), "a:b:c", "",
	} {
		if got, want := protocol.ValidatePrincipal(s) == nil, re.MatchString(s); got != want {
			t.Errorf("%q: ValidatePrincipal=%v, schema pattern=%v", s, got, want)
		}
	}
}
