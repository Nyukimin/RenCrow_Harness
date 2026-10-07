package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/files"
)

// Kinds of a response whose Tool calls cannot be used.
const (
	// InvalidUndeclared: a call names a Tool the request did not declare.
	InvalidUndeclared = "undeclared_tool"
	// InvalidArguments: a call's arguments do not satisfy the Tool's schema.
	InvalidArguments = "invalid_arguments"
)

// InvalidError says a response's Tool calls are not acceptable. Nothing of the response
// is used: one bad call rejects them all, and none is run.
type InvalidError struct {
	Kind string
	// Ordinal is the position of the first call found wrong.
	Ordinal int
	// Reason is a short statement with no model text in it.
	Reason string
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("tools: call %d: %s: %s", e.Ordinal, e.Kind, e.Reason)
}

// Call is one validated Tool call of a response: its place in the response, the schema
// revision it was validated against, and its arguments both as the canonical JSON that
// is stored and as the typed value the Tool takes.
type Call struct {
	ProviderToolCallID string
	Ordinal            int64
	Name               string
	// ArgumentsText is the arguments as the model wrote them: what goes back into the
	// context in the assistant message.
	ArgumentsText  string
	SchemaRevision string
	// ArgsBytes is the CJ1 of the arguments object.
	ArgsBytes []byte
	args      any
}

type readArgs struct {
	Path     string      `json:"path"`
	Range    files.Range `json:"range"`
	MaxBytes int64       `json:"max_bytes"`
}

type searchArgs struct {
	Path       string `json:"path"`
	Query      string `json:"query"`
	Mode       string `json:"mode"`
	MaxResults int    `json:"max_results"`
	MaxBytes   int    `json:"max_bytes"`
}

type createArgs struct {
	Path           string `json:"path"`
	Text           string `json:"text"`
	ExpectedAbsent bool   `json:"expected_absent"`
}

type editArgs struct {
	Path         string `json:"path"`
	ExpectedHash string `json:"expected_hash"`
	OldText      string `json:"old_text"`
	NewText      string `json:"new_text"`
}

type execArgs struct {
	Executable     string   `json:"executable"`
	Argv           []string `json:"argv"`
	Cwd            string   `json:"cwd"`
	EnvProfileRef  string   `json:"env_profile_ref"`
	TimeoutSeconds int64    `json:"timeout_seconds"`
}

type evidenceArgs struct {
	EvidenceID        string      `json:"evidence_id"`
	ProjectionVersion string      `json:"projection_version"`
	Range             files.Range `json:"range"`
}

func decodeInto[T any](text string) (T, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, err
	}
	return v, nil
}

// ValidateCalls checks every Tool call of one response before any is acted on. A call
// is acceptable when its Tool is one the Run's policy offers, its arguments satisfy that
// Tool's schema (the design package's, the one the request declared) and the few
// conditions a schema cannot say (a range that does not run backwards, an Evidence ID
// that is canonical), and its call ID is present and not used twice in the response.
// All must be acceptable: a response with one bad call is refused whole, and its other
// calls are not run (a half-valid batch is not a smaller valid one).
func (p *RunPolicy) ValidateCalls(intents []modelport.ToolIntent) ([]Call, error) {
	m, err := loadSpecs()
	if err != nil {
		return nil, err
	}
	if len(intents) == 0 {
		return nil, &InvalidError{Kind: InvalidArguments, Reason: "the response has no Tool call"}
	}
	seen := map[string]bool{}
	out := make([]Call, 0, len(intents))
	for i, in := range intents {
		bad := func(kind, reason string) error { return &InvalidError{Kind: kind, Ordinal: i, Reason: reason} }
		if !p.Enabled(in.Name) {
			return nil, bad(InvalidUndeclared, "the Tool is not one the request offered")
		}
		if in.ProviderToolCallID == "" || len(in.ProviderToolCallID) > 256 || seen[in.ProviderToolCallID] {
			return nil, bad(InvalidArguments, "the call has no ID, or its ID is used twice")
		}
		seen[in.ProviderToolCallID] = true
		v, err := strictjson.Decode([]byte(in.ArgumentsJSON))
		if err != nil {
			return nil, bad(InvalidArguments, "the arguments are not one JSON object")
		}
		if _, ok := v.(map[string]any); !ok {
			return nil, bad(InvalidArguments, "the arguments are not one JSON object")
		}
		if err := schemacheck.Validate(schemacheck.Tools, "", map[string]any{"name": in.Name, "arguments": v}); err != nil {
			return nil, bad(InvalidArguments, "the arguments do not satisfy the Tool's schema")
		}
		canonical, err := canon.Encode(v)
		if err != nil {
			return nil, bad(InvalidArguments, "the arguments cannot be canonicalized")
		}
		typed, err := typedArgs(in.Name, in.ArgumentsJSON)
		if err != nil {
			return nil, bad(InvalidArguments, err.Error())
		}
		out = append(out, Call{
			ProviderToolCallID: in.ProviderToolCallID, Ordinal: in.Ordinal, Name: in.Name, ArgumentsText: in.ArgumentsJSON,
			SchemaRevision: m[in.Name].revision, ArgsBytes: canonical, args: typed,
		})
	}
	return out, nil
}

// typedArgs decodes already schema-valid arguments into the Tool's own type and checks
// what a schema cannot.
func typedArgs(name, text string) (any, error) {
	switch name {
	case FileRead:
		a, err := decodeInto[readArgs](text)
		if err != nil {
			return nil, errors.New("the arguments do not decode")
		}
		if a.Range.Start > a.Range.End {
			return nil, errors.New("the range runs backwards")
		}
		return a, nil
	case FileSearch:
		a, err := decodeInto[searchArgs](text)
		if err != nil {
			return nil, errors.New("the arguments do not decode")
		}
		return a, nil
	case FileCreate:
		a, err := decodeInto[createArgs](text)
		if err != nil {
			return nil, errors.New("the arguments do not decode")
		}
		return a, nil
	case FileEdit:
		a, err := decodeInto[editArgs](text)
		if err != nil {
			return nil, errors.New("the arguments do not decode")
		}
		return a, nil
	case ProcessExec:
		a, err := decodeInto[execArgs](text)
		if err != nil {
			return nil, errors.New("the arguments do not decode")
		}
		return a, nil
	case EvidenceRead:
		a, err := decodeInto[evidenceArgs](text)
		if err != nil {
			return nil, errors.New("the arguments do not decode")
		}
		if a.Range.Start > a.Range.End {
			return nil, errors.New("the range runs backwards")
		}
		if _, err := identity.ParseEvidenceID(a.EvidenceID); err != nil {
			return nil, errors.New("the evidence ID is not canonical")
		}
		return a, nil
	}
	return nil, errors.New("the Tool is unknown")
}
