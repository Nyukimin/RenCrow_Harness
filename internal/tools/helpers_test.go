package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

func readSchema(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "tools.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func canonicalText(t *testing.T, raw []byte) string {
	t.Helper()
	v, err := strictjson.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := canon.Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
