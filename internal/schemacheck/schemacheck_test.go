package schemacheck_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

func decodeExample(t *testing.T, rel string) any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contract", "examples", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	v, err := strictjson.Decode(b)
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return v
}

func TestValidateAcceptsDesignExample(t *testing.T) {
	v := decodeExample(t, "core_start.json")
	if err := schemacheck.Validate(schemacheck.Protocol, "StartInput", v); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsUnknownFieldWithoutEchoingValues(t *testing.T) {
	v := decodeExample(t, "core_start.json").(map[string]any)
	v["leaked_secret_field"] = "sk-this-must-not-appear-in-errors"
	err := schemacheck.Validate(schemacheck.Protocol, "StartInput", v)
	if err == nil {
		t.Fatal("unknown field was accepted")
	}
	var viol *schemacheck.Violation
	if !errors.As(err, &viol) || !errors.Is(err, schemacheck.ErrViolation) {
		t.Fatalf("want *Violation wrapping ErrViolation, got %T %v", err, err)
	}
	if strings.Contains(err.Error(), "sk-this-must-not-appear") {
		t.Fatalf("error leaks the offending value: %v", err)
	}
}

func TestValidateWholeDocumentEventSchema(t *testing.T) {
	m := decodeExample(t, "wire/event_payloads.json").(map[string]any)
	for i, e := range m["events"].([]any) {
		if err := schemacheck.Validate(schemacheck.Event, "", e); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
}

func TestValidateUnknownDefIsNotAViolation(t *testing.T) {
	err := schemacheck.Validate(schemacheck.Protocol, "NoSuchDef", map[string]any{})
	if err == nil || errors.Is(err, schemacheck.ErrViolation) {
		t.Fatalf("a missing definition is a load error, not a violation: %v", err)
	}
}

// Each schema file carries its own copy of the shared $defs. Validation uses the
// copy in the file that names the type (protocol.schema.json for every public DTO,
// including Event), so the copies must not drift: this fails the day one is edited.
func TestSharedDefinitionsAreIdenticalInEverySchemaFile(t *testing.T) {
	read := func(name string) map[string]json.RawMessage {
		b, err := os.ReadFile(filepath.Join("..", "..", "schemas", name))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Defs map[string]json.RawMessage `json:"$defs"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Defs
	}
	canonical := func(raw json.RawMessage) string {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		out, _ := json.Marshal(v) // map keys are sorted by encoding/json
		return string(out)
	}
	base := read(schemacheck.Protocol)
	for _, file := range []string{schemacheck.Event, schemacheck.Config, schemacheck.PolicyRegistry, "host_assets.schema.json"} {
		other := read(file)
		for name, def := range base {
			got, ok := other[name]
			if !ok {
				t.Errorf("%s lacks the shared definition %s", file, name)
				continue
			}
			if canonical(got) != canonical(def) {
				t.Errorf("%s: definition %s differs from protocol.schema.json", file, name)
			}
		}
	}
}
