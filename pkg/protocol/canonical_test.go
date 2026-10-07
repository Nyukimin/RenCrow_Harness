package protocol_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

type canonicalVectorFile struct {
	Positive []struct {
		Name           string `json:"name"`
		InputJSON      string `json:"input_json"`
		ExpectedUTF8   string `json:"expected_cj_utf8"`
		ExpectedHex    string `json:"expected_cj_hex"`
		ExpectedSHA256 string `json:"expected_sha256"`
	} `json:"positive"`
	NegativeJSON []string `json:"negative_json"`
}

func TestEncodeCanonicalContractGolden(t *testing.T) {
	var v canonicalVectorFile
	exampleJSON(t, "wire/canonical_vectors.json", &v)
	if len(v.Positive) != 12 || len(v.NegativeJSON) != 7 {
		t.Fatalf("vector file shape changed: %d positive, %d negative", len(v.Positive), len(v.NegativeJSON))
	}
	for _, c := range v.Positive {
		t.Run(c.Name, func(t *testing.T) {
			got, err := protocol.EncodeCanonicalContract([]byte(c.InputJSON))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.ExpectedUTF8 || hex.EncodeToString(got) != c.ExpectedHex {
				t.Fatalf("bytes\n got %q\nwant %q", got, c.ExpectedUTF8)
			}
			if sum := sha256.Sum256(got); hex.EncodeToString(sum[:]) != c.ExpectedSHA256 {
				t.Fatalf("sha256 %x want %s", sum, c.ExpectedSHA256)
			}
		})
	}
	for i, in := range v.NegativeJSON {
		if got, err := protocol.EncodeCanonicalContract([]byte(in)); err == nil {
			t.Errorf("negative vector %d %q accepted as %q", i, in, got)
		}
	}
}

func TestEncodeCanonicalContractIsIdempotent(t *testing.T) {
	first, err := protocol.EncodeCanonicalContract([]byte(` { "b" : [ 1.0 , "x" ] , "a" : 1e0 } `))
	if err != nil {
		t.Fatal(err)
	}
	second, err := protocol.EncodeCanonicalContract(first)
	if err != nil || string(first) != string(second) {
		t.Fatalf("not idempotent: %q then %q (%v)", first, second, err)
	}
	if strings.ContainsAny(string(first), " \n") {
		t.Fatalf("whitespace in output: %q", first)
	}
}
