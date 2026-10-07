package protocol_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// example returns the bytes of a design-package vector under testdata/contract/examples.
func example(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contract", "examples", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// exampleJSON loads a vector with encoding/json, deliberately independent of the
// strict decoder under test.
func exampleJSON(t testing.TB, rel string, into any) {
	t.Helper()
	if err := json.Unmarshal(example(t, rel), into); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
}

func schemaPath(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

// nfd is "Cafe" + U+0301, built without a literal combining mark in the source.
func nfd() string { return "Cafe" + string(rune(0x301)) }

// nfc is "Caf" + U+00E9.
func nfc() string { return "Caf" + string(rune(0xe9)) }
