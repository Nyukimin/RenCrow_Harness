package protocol_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

type originProofVector struct {
	KeyHex string               `json:"key_hex"`
	Proof  protocol.OriginProof `json:"proof"`
}

func loadOriginProofVector(t *testing.T) (protocol.OriginKey, protocol.OriginProof) {
	t.Helper()
	var v originProofVector
	exampleJSON(t, "origin_proof_vector.json", &v)
	key, err := protocol.ParseOriginKeyFile([]byte(v.KeyHex))
	if err != nil {
		t.Fatal(err)
	}
	return key, v.Proof
}

func TestOriginKeyFileFormat(t *testing.T) {
	const keyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	file := example(t, "wire/synthetic_origin_key.hex")
	if string(file) != keyHex+"\n" {
		t.Fatalf("fixture key file is not hex64+LF: %q", file)
	}
	withLF, err := protocol.ParseOriginKeyFile(file)
	if err != nil {
		t.Fatal(err)
	}
	withoutLF, err := protocol.ParseOriginKeyFile([]byte(keyHex))
	if err != nil {
		t.Fatal(err)
	}
	if withLF != withoutLF {
		t.Fatal("the optional trailing LF must not change the key")
	}
	vectorKey, _ := loadOriginProofVector(t)
	if vectorKey != withLF {
		t.Fatal("key file and vector key differ")
	}

	raw := make([]byte, 32)
	bad := map[string][]byte{
		"empty":              {},
		"LF only":            []byte("\n"),
		"CRLF":               []byte(keyHex + "\r\n"),
		"two LF":             []byte(keyHex + "\n\n"),
		"trailing space":     []byte(keyHex + " "),
		"leading space":      []byte(" " + keyHex),
		"leading LF":         []byte("\n" + keyHex),
		"uppercase":          []byte(strings.ToUpper(keyHex)),
		"mixed case":         []byte("A" + keyHex[1:]),
		"BOM":                append([]byte("\xef\xbb\xbf"), keyHex...),
		"0x prefix":          []byte("0x" + keyHex[2:]),
		"63 chars":           []byte(keyHex[1:]),
		"65 chars":           []byte(keyHex + "0"),
		"raw 32 bytes":       raw,
		"raw random bytes":   bytes.Repeat([]byte{0xff}, 32),
		"base64 of 32 bytes": []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="),
		"non-hex digit":      []byte(strings.Replace(keyHex, "0", "g", 1)),
		"embedded NUL":       []byte(keyHex[:10] + "\x00" + keyHex[11:]),
		"quoted":             []byte(`"` + keyHex + `"`),
		"hex with spaces":    []byte(strings.Join(strings.Split(keyHex, ""), " ")),
		"line-wrapped":       []byte(keyHex[:32] + "\n" + keyHex[32:]),
	}
	for name, in := range bad {
		if _, err := protocol.ParseOriginKeyFile(in); !errors.Is(err, protocol.ErrInvalidKeyFile) {
			t.Errorf("%s: got %v, want ErrInvalidKeyFile", name, err)
		}
	}
}

func TestOriginKeyIsRedactedWhenPrinted(t *testing.T) {
	key, _ := loadOriginProofVector(t)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%d", "%q"} {
		if out := fmt.Sprintf(verb, key); out != "OriginKey[REDACTED]" {
			t.Errorf("%s prints %q", verb, out)
		}
	}
	if out := fmt.Sprint(&key); out != "OriginKey[REDACTED]" {
		t.Errorf("pointer prints %q", out)
	}
}

func TestOriginProofMACGolden(t *testing.T) {
	key, proof := loadOriginProofVector(t)
	if proof.Sequence != 1 || proof.Origin != "human" {
		t.Fatalf("vector decoded unexpectedly: %+v", proof)
	}
	const want = "80123e0116cc3d5baa2146b25ba225c9dd008bbcc5b247bdfb005035cc956a87"
	got, err := protocol.OriginProofMAC(key, proof)
	if err != nil || got != want {
		t.Fatalf("got %s, %v; want %s", got, err, want)
	}
	if proof.MAC != want {
		t.Fatal("vector mac drifted from the expectation in this test")
	}
	// The mac field is output, not input.
	proof.MAC = ""
	if again, err := protocol.OriginProofMAC(key, proof); err != nil || again != want {
		t.Fatalf("MAC must not depend on the mac field: %s, %v", again, err)
	}
	proof.MAC = "garbage"
	if again, err := protocol.OriginProofMAC(key, proof); err != nil || again != want {
		t.Fatalf("MAC must not depend on the mac field: %s, %v", again, err)
	}
}

func TestVerifyOriginProofMAC(t *testing.T) {
	key, proof := loadOriginProofVector(t)
	if err := protocol.VerifyOriginProofMAC(key, proof); err != nil {
		t.Fatalf("valid proof rejected: %v", err)
	}

	tampered := map[string]func(p *protocol.OriginProof){
		"issuer":                func(p *protocol.OriginProof) { p.Issuer = "core:other" },
		"key_id":                func(p *protocol.OriginProof) { p.KeyID = "fixture-key-2" },
		"audience":              func(p *protocol.OriginProof) { p.Audience = "core:remote" },
		"origin":                func(p *protocol.OriginProof) { p.Origin = "automation" },
		"source_message_id":     func(p *protocol.OriginProof) { p.SourceMessageID = "msg_00000000-0000-7000-8000-000000000102" },
		"source_thread_id":      func(p *protocol.OriginProof) { p.SourceThreadID = "thr_00000000-0000-7000-8000-000000000102" },
		"destination_thread_id": func(p *protocol.OriginProof) { p.DestinationThreadID = "thr_00000000-0000-7000-8000-000000000002" },
		"mutation_key":          func(p *protocol.OriginProof) { p.MutationKey = "fixture.turn.start.0002" },
		"raw_hash":              func(p *protocol.OriginProof) { p.RawHash = strings.Repeat("a", 64) },
		"sequence":              func(p *protocol.OriginProof) { p.Sequence++ },
		"issued_at":             func(p *protocol.OriginProof) { p.IssuedAt = "2026-10-07T00:00:01Z" },
		"expires_at":            func(p *protocol.OriginProof) { p.ExpiresAt = "2026-10-07T00:05:01Z" },
		"nonce":                 func(p *protocol.OriginProof) { p.Nonce += "x" },
	}
	for name, mutate := range tampered {
		p := proof
		mutate(&p)
		if err := protocol.VerifyOriginProofMAC(key, p); !errors.Is(err, protocol.ErrOriginProofMAC) {
			t.Errorf("tampered %s: got %v, want ErrOriginProofMAC", name, err)
		}
	}

	otherKey, err := protocol.ParseOriginKeyFile([]byte(strings.Repeat("ab", 32)))
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.VerifyOriginProofMAC(otherKey, proof); !errors.Is(err, protocol.ErrOriginProofMAC) {
		t.Errorf("different key: %v", err)
	}

	malformed := map[string]string{
		"empty":     "",
		"uppercase": strings.ToUpper(proof.MAC),
		"short":     proof.MAC[:62],
		"long":      proof.MAC + "00",
		"spaced":    " " + proof.MAC,
		"newline":   proof.MAC + "\n",
		"non-hex":   strings.Replace(proof.MAC, proof.MAC[:1], "g", 1),
		"prefix":    "sha256:" + proof.MAC,
	}
	for name, mac := range malformed {
		p := proof
		p.MAC = mac
		if err := protocol.VerifyOriginProofMAC(key, p); !errors.Is(err, protocol.ErrOriginProofMAC) {
			t.Errorf("malformed mac %s: got %v, want ErrOriginProofMAC", name, err)
		}
	}
}

func TestOriginProofMACRejectsInvalidUTF8(t *testing.T) {
	key, proof := loadOriginProofVector(t)
	proof.Nonce = "n\xff"
	if _, err := protocol.OriginProofMAC(key, proof); !errors.Is(err, protocol.ErrInvalidInput) {
		t.Fatalf("got %v", err)
	}
}

func TestOriginProofMACFieldBoundaries(t *testing.T) {
	// Moving bytes between adjacent fields must change the MAC (LP framing).
	key, proof := loadOriginProofVector(t)
	a, b := proof, proof
	a.Issuer, a.KeyID = "core:fixtu", "refixture-key-1"
	b.Issuer, b.KeyID = "core:fixture", "fixture-key-1"
	macA, _ := protocol.OriginProofMAC(key, a)
	macB, _ := protocol.OriginProofMAC(key, b)
	if macA == macB {
		t.Fatal("field boundaries must be length prefixed")
	}
}
