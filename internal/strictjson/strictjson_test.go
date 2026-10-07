package strictjson_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

func TestDecodeValues(t *testing.T) {
	got, err := strictjson.Decode([]byte(" \t\r\n{\"b\":[1,2.50,-0,true,false,null],\"a\":{\"x\":\"\\u0041\\n\\\"\\\\\\/\"},\"e\":\"\"} \n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"b": []any{json.Number("1"), json.Number("2.50"), json.Number("-0"), true, false, nil},
		"a": map[string]any{"x": "A\n\"\\/"},
		"e": "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestDecodeScalarsAndEmptyContainers(t *testing.T) {
	cases := map[string]any{
		`null`: nil, `true`: true, `"x"`: "x", `0`: json.Number("0"), `[]`: []any{}, `{}`: map[string]any{},
		`"\u0000"`: "\x00", "\"\U0001F600\"": "\U0001F600", `"\ud83d\ude00"`: "\U0001F600", `"\uD83D\uDE00"`: "\U0001F600", "\"\uFFFD\"": "\uFFFD",
		"\"\uFEFF\"": "\uFEFF", // a BOM-like scalar inside a string is data; only a leading BOM is rejected
	}
	for in, want := range cases {
		got, err := strictjson.Decode([]byte(in))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %#v, %v; want %#v", in, got, err, want)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"duplicate key", `{"a":1,"a":2}`, strictjson.ErrDuplicateKey},
		{"duplicate key by escape", `{"a":1,"\u0061":2}`, strictjson.ErrDuplicateKey},
		{"duplicate key nested", `{"o":{"k":1,"k":1}}`, strictjson.ErrDuplicateKey},
		{"duplicate key raw vs escaped", "{\"\u00e9\":1,\"\\u00e9\":2}", strictjson.ErrDuplicateKey},
		{"invalid utf8 in string", "\"\xff\"", strictjson.ErrInvalidUTF8},
		{"invalid utf8 in key", "{\"\xff\":1}", strictjson.ErrInvalidUTF8},
		{"overlong", "\"\xc0\x80\"", strictjson.ErrInvalidUTF8},
		{"truncated sequence", "\"\xe3\x81\"", strictjson.ErrInvalidUTF8},
		{"encoded surrogate", "\"\xed\xa0\x80\"", strictjson.ErrInvalidUTF8},
		{"beyond max rune", "\"\xf4\x90\x80\x80\"", strictjson.ErrInvalidUTF8},
		{"lone high surrogate", `"\ud800"`, strictjson.ErrUnpairedSurrogate},
		{"lone low surrogate", `"\udc00"`, strictjson.ErrUnpairedSurrogate},
		{"high then non-surrogate escape", `"\ud800\u0041"`, strictjson.ErrUnpairedSurrogate},
		{"high then text", `"\ud800x"`, strictjson.ErrUnpairedSurrogate},
		{"high then high", `"\ud800\ud800"`, strictjson.ErrUnpairedSurrogate},
		{"surrogate in key", `{"\ud800":1}`, strictjson.ErrUnpairedSurrogate},
		{"leading BOM", "\xef\xbb\xbf{}", strictjson.ErrBOM},
		{"trailing value", `{}{}`, strictjson.ErrTrailingData},
		{"trailing garbage", `{} x`, strictjson.ErrTrailingData},
		{"trailing comma at top", `{},`, strictjson.ErrTrailingData},
		{"empty", ``, strictjson.ErrSyntax},
		{"whitespace only", " \n", strictjson.ErrSyntax},
		{"NaN", `NaN`, strictjson.ErrSyntax},
		{"Infinity", `Infinity`, strictjson.ErrSyntax},
		{"negative Infinity", `-Infinity`, strictjson.ErrSyntax},
		{"leading zero", `01`, strictjson.ErrSyntax},
		{"plus sign", `+1`, strictjson.ErrSyntax},
		{"bare fraction", `.5`, strictjson.ErrSyntax},
		{"empty fraction", `1.`, strictjson.ErrSyntax},
		{"empty exponent", `1e`, strictjson.ErrSyntax},
		{"empty exponent sign", `1e+`, strictjson.ErrSyntax},
		{"lone minus", `-`, strictjson.ErrSyntax},
		{"hex", `0x10`, strictjson.ErrSyntax},
		{"exponent too large", `1e5000`, strictjson.ErrNumber},
		{"raw control char", "\"a\x01b\"", strictjson.ErrSyntax},
		{"raw newline in string", "\"a\nb\"", strictjson.ErrSyntax},
		{"bad escape", `"\x"`, strictjson.ErrSyntax},
		{"short unicode escape", `"\u12"`, strictjson.ErrSyntax},
		{"non-hex unicode escape", `"\u12G4"`, strictjson.ErrSyntax},
		{"unterminated string", `"abc`, strictjson.ErrSyntax},
		{"unterminated object", `{"a":1`, strictjson.ErrSyntax},
		{"unterminated array", `[1,`, strictjson.ErrSyntax},
		{"trailing comma in array", `[1,]`, strictjson.ErrSyntax},
		{"trailing comma in object", `{"a":1,}`, strictjson.ErrSyntax},
		{"missing colon", `{"a" 1}`, strictjson.ErrSyntax},
		{"non-string key", `{1:2}`, strictjson.ErrSyntax},
		{"single quotes", `{'a':1}`, strictjson.ErrSyntax},
		{"truncated literal", `tru`, strictjson.ErrSyntax},
		{"misspelled literal", `nul`, strictjson.ErrSyntax},
		{"literal with suffix", `truex`, strictjson.ErrTrailingData},
		{"comment", `{} // x`, strictjson.ErrTrailingData},
		{"form feed whitespace", "\f{}", strictjson.ErrSyntax},
		{"non-breaking space is not whitespace", "\u00a0{}", strictjson.ErrSyntax},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := strictjson.Decode([]byte(c.in))
			if err == nil {
				t.Fatalf("accepted, got %#v", v)
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

func TestDecodeDepthLimit(t *testing.T) {
	nest := func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }
	if _, err := strictjson.Decode([]byte(nest(64))); err != nil {
		t.Fatalf("depth 64 must be accepted: %v", err)
	}
	if _, err := strictjson.Decode([]byte(nest(65))); !errors.Is(err, strictjson.ErrDepth) {
		t.Fatalf("depth 65: %v", err)
	}
	obj := strings.Repeat(`{"a":`, 64) + "1" + strings.Repeat("}", 64)
	if _, err := strictjson.Decode([]byte(obj)); err != nil {
		t.Fatalf("object depth 64 must be accepted: %v", err)
	}
	if _, err := strictjson.Decode([]byte(strings.Repeat(`{"a":`, 65) + "1" + strings.Repeat("}", 65))); !errors.Is(err, strictjson.ErrDepth) {
		t.Fatalf("object depth 65: %v", err)
	}
	// Hostile input must fail at the limit without recursing to its own depth.
	if _, err := strictjson.Decode([]byte(strings.Repeat("[", 1_000_000))); !errors.Is(err, strictjson.ErrDepth) {
		t.Fatalf("million-deep input: %v", err)
	}
}

func TestNumberLexemeLimit(t *testing.T) {
	ok := strings.Repeat("1", strictjson.MaxNumberLexemeBytes)
	if _, err := strictjson.Decode([]byte(ok)); err != nil {
		t.Fatalf("1024-byte lexeme must be accepted: %v", err)
	}
	if _, err := strictjson.Decode([]byte(ok + "1")); !errors.Is(err, strictjson.ErrNumber) {
		t.Fatalf("1025-byte lexeme: %v", err)
	}
	if _, err := strictjson.Decode([]byte("[" + ok + "1]")); !errors.Is(err, strictjson.ErrNumber) {
		t.Fatalf("1025-byte lexeme in array: %v", err)
	}
}

func TestNormalizeNumber(t *testing.T) {
	ok := map[string]string{
		"0": "0", "-0": "0", "0.0": "0", "-0.0": "0", "-0.00": "0", "0e10": "0", "0e-10": "0", "0.000e5": "0",
		"1": "1", "1.0": "1", "1e0": "1", "1E0": "1", "1e+0": "1", "-1": "-1", "10": "10", "100": "100",
		"1e-3": "0.001", "1.2300e2": "123", "1E+3": "1000", "-1.200e-2": "-0.012", "10e-1": "1", "0.5": "0.5",
		"0.50": "0.5", "5e-1": "0.5", "123.456e-2": "1.23456", "1.5e1": "15", "1e2": "100", "12e1": "120",
		"9007199254740993":               "9007199254740993",
		"1.0000000000000000001":          "1.0000000000000000001",
		"123456789012345678901234567890": "123456789012345678901234567890",
		"0.000001":                       "0.000001", "1e-6": "0.000001", "1e6": "1000000", "100e-2": "1",
	}
	for in, want := range ok {
		got, err := strictjson.NormalizeNumber(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	// Leading zeros in the exponent are lexically fine and do not count as length.
	if got, err := strictjson.NormalizeNumber("1e0000000000000000000005"); err != nil || got != "100000" {
		t.Errorf("padded exponent: %q, %v", got, err)
	}
	for _, in := range []string{"", "00", "01", "07", "-", "+1", ".5", "1.", "1e", "NaN", "Infinity", "-Infinity", "0x1", "1 ", " 1", "1\n", "\u0661"} {
		if _, err := strictjson.NormalizeNumber(in); !errors.Is(err, strictjson.ErrSyntax) {
			t.Errorf("%q: got %v, want syntax error", in, err)
		}
	}
}

// The limits apply to the normalized value, not to how it was written:
// the lexeme is at most 1024 bytes, and the exponent of the integer mantissa
// (trailing zeros removed, fraction digits subtracted) and the expanded plain
// decimal string (sign included) are at most 4096.
func TestNormalizeNumberLimits(t *testing.T) {
	zeros := func(n int) string { return strings.Repeat("0", n) }
	accept := []struct{ in, want string }{
		{"1e4095", "1" + zeros(4095)},                      // exactly 4096 characters
		{"1e-4094", "0." + zeros(4093) + "1"},              // exactly 4096 characters
		{"5e-324", "0." + zeros(323) + "5"},                // smallest double
		{"0." + zeros(1000) + "1e5000", "1" + zeros(3999)}, // exponent 5000 is cancelled by the fraction digits
		{"1." + zeros(300) + "e-4000", "0." + zeros(3999) + "1"},
		{"0e4096", "0"}, {"0e-4096", "0"},
	}
	for _, c := range accept {
		got, err := strictjson.NormalizeNumber(c.in)
		if err != nil || got != c.want {
			t.Errorf("%.30s: got len %d err %v, want len %d", c.in, len(got), err, len(c.want))
		}
	}
	reject := []string{
		"1e5000", "1e-5000", "1e4097", "-1e5000",
		"1e4096",                        // expands to 4097 characters
		"-1e4095",                       // the sign counts: 4097 characters
		"1e-4095",                       // "0." + 4094 zeros + "1" = 4097 characters
		"0e5000", "0e-5000", "0.0e5000", // zero is judged by the same exponent bound, conservatively
		"1e10000", "1e99999999999999999999999", "1e-99999999999999999999999",
		"1e1" + zeros(900), // exponent literal with hundreds of digits: rejected without being parsed into an integer
	}
	for _, in := range reject {
		if _, err := strictjson.NormalizeNumber(in); !errors.Is(err, strictjson.ErrNumber) {
			t.Errorf("%.30s: got %v, want ErrNumber", in, err)
		}
	}
	if _, err := strictjson.NormalizeNumber(strings.Repeat("1", 1025)); !errors.Is(err, strictjson.ErrNumber) {
		t.Errorf("1025-byte lexeme: %v", err)
	}
	if got, err := strictjson.NormalizeNumber(strings.Repeat("1", 1024)); err != nil || got != strings.Repeat("1", 1024) {
		t.Errorf("1024-byte lexeme: len %d, %v", len(got), err)
	}
}

// The decoder is deliberately stricter than encoding/json. Differential check:
// anything we accept json.Valid accepts, and whatever json.Valid accepts but we
// refuse must be one of the documented extra rejections.
func checkAgainstJSONValid(t *testing.T, data []byte) {
	t.Helper()
	_, err := strictjson.Decode(data)
	valid := json.Valid(data)
	if err == nil && !valid {
		t.Fatalf("accepted what encoding/json rejects: %q", data)
	}
	if err != nil && valid {
		for _, allowed := range []error{
			strictjson.ErrDuplicateKey, strictjson.ErrInvalidUTF8, strictjson.ErrUnpairedSurrogate,
			strictjson.ErrBOM, strictjson.ErrDepth, strictjson.ErrNumber,
		} {
			if errors.Is(err, allowed) {
				return
			}
		}
		t.Fatalf("rejected valid JSON for an undocumented reason (%v): %q", err, data)
	}
}

func TestDifferentialAgainstEncodingJSON(t *testing.T) {
	for _, s := range []string{
		`{"a":[1,2,{"b":null}],"c":"\u00e9"}`, `[]`, `{}`, `"x"`, `-0.5e-3`, `[1,2`, `{"a":}`, `01`, `1.`, `tru`, `{} {}`,
		`{"a":1,"a":2}`, `"\ud800"`, "\"\xff\"", "\xef\xbb\xbf{}", `1e5000`, `[1,]`, `{"a":1,}`, `"\x"`, " \t\r\n[ ] ",
	} {
		checkAgainstJSONValid(t, []byte(s))
	}
}

func FuzzDecodeAgainstEncodingJSON(f *testing.F) {
	for _, s := range []string{`{"a":[1,2,{"b":null}]}`, `"\ud83d\ude00"`, `1e5000`, "\xff", `{"a":1,"a":2}`, `[[[[`, `-0.0e-0`, `[1.5,"x",true]`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) { checkAgainstJSONValid(t, data) })
}
