// Package schemacheck validates already strictly decoded JSON values against the
// embedded design schemas.
//
// The value model is the one of internal/strictjson (nil, bool, string,
// json.Number, []any, map[string]any). Strict decoding comes first because JSON
// Schema cannot see duplicate keys, invalid UTF-8 or trailing values; this
// package only answers "does this value satisfy the schema".
//
// Error text deliberately names only where and which keyword failed, never the
// offending value, so a rejected request body or secret cannot leak through an
// error message.
package schemacheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	harness "github.com/Nyukimin/RenCrow_Harness"
	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
)

// Schema files shipped in schemas/.
const (
	Protocol       = "protocol.schema.json"
	Event          = "event.schema.json"
	Config         = "config.schema.json"
	PolicyRegistry = "policy_registry.schema.json"
	LLMContract    = "llm_contract.schema.json"
	Measure        = "measure.schema.json"
	Checkpoint     = "checkpoint.schema.json"
	// StageData is the schema of the data the two compaction stages are given;
	// Selection and Summary are the schemas of what each must answer.
	StageData = "stage_data.schema.json"
	Selection = "selection.schema.json"
	Summary   = "summary.schema.json"
	// Tools is the schema of the built-in Tools' arguments.
	Tools = "tools.schema.json"
	// HostAssets is the schema of the fixed hooks' input and result and of a Skill's metadata.
	HostAssets = "host_assets.schema.json"
)

const baseURL = "https://rencrow.invalid/harness/v1/"

// ErrViolation is wrapped by every schema violation.
var ErrViolation = errors.New("schema violation")

// Violation lists where an instance failed a schema.
type Violation struct {
	Schema string
	Def    string
	// Findings are "instance-location: keyword-path" lines, sorted, never values.
	Findings []string
}

func (v *Violation) Error() string {
	target := v.Schema
	if v.Def != "" {
		target += "#/$defs/" + v.Def
	}
	return fmt.Sprintf("%s: %s: %s", ErrViolation.Error(), target, strings.Join(v.Findings, "; "))
}

// Unwrap lets errors.Is(err, ErrViolation) match.
func (v *Violation) Unwrap() error { return ErrViolation }

var (
	mu       sync.Mutex
	compiler *jsonschema.Compiler
	loaded   = map[string]bool{}
	compiled = map[string]*jsonschema.Schema{}
)

func compile(file, def string) (*jsonschema.Schema, error) {
	mu.Lock()
	defer mu.Unlock()
	key := file + "#" + def
	if s, ok := compiled[key]; ok {
		return s, nil
	}
	if compiler == nil {
		compiler = jsonschema.NewCompiler()
	}
	url := baseURL + file
	if !loaded[file] {
		raw, err := harness.Schemas.ReadFile("schemas/" + file)
		if err != nil {
			return nil, fmt.Errorf("schemacheck: embedded schema %s: %w", file, err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("schemacheck: parse schema %s: %w", file, err)
		}
		// protocol.schema.json declares its own $id, which is the same URL.
		if err := compiler.AddResource(url, doc); err != nil {
			return nil, fmt.Errorf("schemacheck: add schema %s: %w", file, err)
		}
		loaded[file] = true
	}
	target := url
	if def != "" {
		target += "#/$defs/" + def
	}
	s, err := compiler.Compile(target)
	if err != nil {
		return nil, fmt.Errorf("schemacheck: compile %s: %w", target, err)
	}
	compiled[key] = s
	return s, nil
}

// Validate checks instance against the schema file (whole document when def is
// empty, otherwise "#/$defs/<def>"). A violation is returned as *Violation; a
// schema that cannot be loaded or compiled is an ordinary error.
func Validate(file, def string, instance any) error {
	s, err := compile(file, def)
	if err != nil {
		return err
	}
	err = s.Validate(instance)
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return fmt.Errorf("schemacheck: validate: %w", err)
	}
	set := map[string]struct{}{}
	collect(ve, set)
	findings := make([]string, 0, len(set))
	for f := range set {
		findings = append(findings, f)
	}
	sort.Strings(findings)
	if len(findings) > 8 {
		findings = append(findings[:8], fmt.Sprintf("... %d more", len(findings)-8))
	}
	return &Violation{Schema: file, Def: def, Findings: findings}
}

func collect(ve *jsonschema.ValidationError, out map[string]struct{}) {
	if len(ve.Causes) == 0 {
		loc := "/" + strings.Join(ve.InstanceLocation, "/")
		kw := strings.Join(ve.ErrorKind.KeywordPath(), "/")
		out[loc+": "+kw] = struct{}{}
		return
	}
	for _, c := range ve.Causes {
		collect(c, out)
	}
}

// ErrDecode is wrapped when a value that satisfies the schema still cannot be
// read into the Go type given to Unmarshal (a Go type that has drifted from the
// schema).
var ErrDecode = errors.New("schema-valid value does not fit the Go type")

// Unmarshal validates v against the schema and reads it into out, a pointer to a
// struct, refusing any key out has no field for.
//
// The value is re-encoded as CJ1 first. That turns an integer written "1.0" or
// "1e0" (valid JSON Schema integers) into "1" so encoding/json can read it, and
// changes no string, so the Go value holds exactly the text that was received.
func Unmarshal(file, def string, v any, out any) error {
	if err := Validate(file, def, v); err != nil {
		return err
	}
	canonical, err := canon.Encode(v)
	if err != nil {
		return fmt.Errorf("schemacheck: canonical form: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(canonical))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: %w", ErrDecode, err)
	}
	return nil
}
