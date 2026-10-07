package canon_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

type canonicalVectors struct {
	Positive []struct {
		Name           string `json:"name"`
		InputJSON      string `json:"input_json"`
		ExpectedUTF8   string `json:"expected_cj_utf8"`
		ExpectedHex    string `json:"expected_cj_hex"`
		ExpectedSHA256 string `json:"expected_sha256"`
	} `json:"positive"`
	NegativeJSON []string `json:"negative_json"`
}

func loadVectors(t *testing.T) canonicalVectors {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/contract/examples/wire/canonical_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v canonicalVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestGoldenPositiveVectors(t *testing.T) {
	v := loadVectors(t)
	if len(v.Positive) != 12 {
		t.Fatalf("expected 12 positive vectors, found %d", len(v.Positive))
	}
	for _, c := range v.Positive {
		t.Run(c.Name, func(t *testing.T) {
			decoded, err := strictjson.Decode([]byte(c.InputJSON))
			if err != nil {
				t.Fatal(err)
			}
			got, err := canon.Encode(decoded)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.ExpectedUTF8 {
				t.Fatalf("bytes\n got %q\nwant %q", got, c.ExpectedUTF8)
			}
			if hex.EncodeToString(got) != c.ExpectedHex {
				t.Fatalf("hex\n got %x\nwant %s", got, c.ExpectedHex)
			}
			sum := sha256.Sum256(got)
			if hex.EncodeToString(sum[:]) != c.ExpectedSHA256 {
				t.Fatalf("sha256 got %x want %s", sum, c.ExpectedSHA256)
			}
		})
	}
}

func TestGoldenNegativeVectors(t *testing.T) {
	v := loadVectors(t)
	if len(v.NegativeJSON) != 7 {
		t.Fatalf("expected 7 negative vectors, found %d", len(v.NegativeJSON))
	}
	for i, in := range v.NegativeJSON {
		decoded, err := strictjson.Decode([]byte(in))
		if err == nil {
			// Reaching the encoder with a value the decoder accepted would be a bug.
			if _, encErr := canon.Encode(decoded); encErr == nil {
				t.Errorf("negative vector %d (%q) was accepted end to end", i, in)
			}
		}
	}
}

func TestEncodeEscaping(t *testing.T) {
	var want strings.Builder
	var in strings.Builder
	for c := 0; c < 0x20; c++ {
		in.WriteByte(byte(c))
		fmt.Fprintf(&want, `\u00%02x`, c)
	}
	got, err := canon.Encode(in.String())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `"`+want.String()+`"` {
		t.Fatalf("control characters: %s", got)
	}
	cases := map[string]string{
		"\"":           `"\""`,
		`\`:            `"\\"`,
		"/<>&":         `"/<>&"`,
		"\u007f":       "\"\u007f\"",
		"\u2028\u2029": "\"\u2028\u2029\"",
		"\uffff":       "\"\uffff\"",
		"\ufeff":       "\"\ufeff\"",  // BOM-like scalar inside a string is data
		"e\u0301":      "\"e\u0301\"", // no normalization
		"é":            "\"é\"",
		"\U0001F600":   "\"\U0001F600\"",
		"a\r\nb":       `"a\u000d\u000ab"`,
		"\t":           `"\u0009"`,
		"":             `""`,
	}
	for in, want := range cases {
		got, err := canon.Encode(in)
		if err != nil || string(got) != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestEncodeKeyOrderIsUTF8ByteOrder(t *testing.T) {
	v := map[string]any{
		"\U0001F600": json.Number("0"), "é": json.Number("1"), "a": json.Number("2"), "B": json.Number("3"), "": json.Number("4"),
		"ab": json.Number("5"), "a\u0000": json.Number("6"), "\uffff": json.Number("7"), "z": map[string]any{"b": nil, "a": []any{}},
	}
	got, err := canon.Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"":4,"B":3,"a":2,"a\u0000":6,"ab":5,"z":{"a":[],"b":null},"é":1,"` + "\uffff" + `":7,"` + "\U0001F600" + `":0}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestEncodeNumbersAndIntegers(t *testing.T) {
	got, err := canon.Encode([]any{json.Number("1.0"), json.Number("-0"), json.Number("1e-3"), 7, int64(-8), uint64(18446744073709551615), true, false, nil})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `[1,0,0.001,7,-8,18446744073709551615,true,false,null]` {
		t.Fatalf("got %s", got)
	}
}

func TestEncodeRejects(t *testing.T) {
	deep := func(n int) any {
		var v any = nil
		for i := 0; i < n; i++ {
			v = []any{v}
		}
		return v
	}
	cases := []struct {
		name string
		v    any
		want error
	}{
		{"invalid utf8 value", "a\xffb", canon.ErrInvalidString},
		{"invalid utf8 key", map[string]any{"a\xff": nil}, canon.ErrInvalidString},
		{"lone surrogate bytes", "\xed\xa0\x80", canon.ErrInvalidString},
		{"float64 is never rounded in", 1.5, canon.ErrUnsupported},
		{"float32", float32(1), canon.ErrUnsupported},
		{"string slice", []string{"a"}, canon.ErrUnsupported},
		{"struct", struct{}{}, canon.ErrUnsupported},
		{"map with typed values", map[string]string{"a": "b"}, canon.ErrUnsupported},
		{"bad number text", json.Number("NaN"), strictjson.ErrSyntax},
		{"huge exponent", json.Number("1e5000"), strictjson.ErrNumber},
		{"too deep", deep(65), strictjson.ErrDepth},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, err := canon.Encode(c.v); err == nil || !errors.Is(err, c.want) {
				t.Fatalf("got %q, %v; want %v", got, err, c.want)
			}
		})
	}
	if _, err := canon.Encode(deep(64)); err != nil {
		t.Fatalf("depth 64 must be accepted: %v", err)
	}
}

func TestLP(t *testing.T) {
	if got := canon.LP(nil); !bytes.Equal(got, make([]byte, 8)) {
		t.Fatalf("LP(empty) = %x", got)
	}
	got := canon.LP([]byte("日本語"))
	want := append([]byte{0, 0, 0, 0, 0, 0, 0, 9}, []byte("日本語")...)
	if !bytes.Equal(got, want) {
		t.Fatalf("LP counts bytes, not characters: %x", got)
	}
	big := canon.LP(make([]byte, 0x01020304))
	if !bytes.Equal(big[:8], []byte{0, 0, 0, 0, 1, 2, 3, 4}) {
		t.Fatalf("length must be 8-byte big endian: %x", big[:8])
	}
}

func TestD(t *testing.T) {
	// Reference value assembled independently in Python from hand-written CJ1 bytes:
	// LP("rencrow-test/v1") + LP({"a":[null,"é"],"b":1}) + LP("x") + LP(5).
	got, err := canon.D("rencrow-test/v1", map[string]any{"b": json.Number("1"), "a": []any{nil, "é"}}, "x", json.Number("5"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "0fe28ee25b6dbc80ea61628c6ebc9664b46142c47c68ccd8f5c3b0fd6e99115e" {
		t.Fatalf("D = %s", got)
	}
	// No values: only the length-prefixed domain, with no trailing NUL.
	got, err = canon.D("rencrow-test/v1")
	if err != nil || got != "af79bfe401d3e60bbcf2867eb248a84dfc98ae273e2975127b2dd4f8729bf2a9" {
		t.Fatalf("D without values = %s, %v", got, err)
	}
	// Each value is length-prefixed on its own, so concatenation cannot alias.
	a, _ := canon.D("d", "ab", "c")
	b, _ := canon.D("d", "a", "bc")
	if a == b {
		t.Fatal("value boundaries must be part of the digest")
	}
	if _, err := canon.D("d", 1.5); !errors.Is(err, canon.ErrUnsupported) {
		t.Fatalf("D must surface encoder errors: %v", err)
	}
	if _, err := canon.D("bad\xff"); !errors.Is(err, canon.ErrInvalidString) {
		t.Fatalf("domain must be valid UTF-8: %v", err)
	}
}
