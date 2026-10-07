// Package tools is the Harness's Tool runtime: the catalog of the six built-in Tools a
// Run may offer the model, the validation of the calls the model makes, and the
// dispatcher (F16) that binds each call to an Action, authorizes it against the Run's
// frozen policy, records that it is about to run, runs it, and records how it ended.
//
// The file Tools live in package files and process.exec in package process; the
// dispatcher is the only code that connects them to Runs, Evidence and the store. A
// Tool is never run for a call that has not been recorded as an Action first, and a
// call whose dispatch is recorded is never run again, whatever happens to its result.
package tools

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	harness "github.com/Nyukimin/RenCrow_Harness"
	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

// Tool names.
const (
	FileRead     = "file.read"
	FileSearch   = "file.search"
	FileCreate   = "file.create"
	FileEdit     = "file.edit"
	ProcessExec  = "process.exec"
	EvidenceRead = "evidence.read"
)

// AllTools lists the six built-in Tools in catalog order.
func AllTools() []string {
	return []string{EvidenceRead, FileCreate, FileEdit, FileRead, FileSearch, ProcessExec}
}

var descriptions = map[string]string{
	FileRead: "Read a byte range of a file in the workspace. The result carries raw_hash (the SHA-256 of the whole file, to pass to file.edit as expected_hash), " +
		"total_bytes, the range actually returned and whether the result is partial. Bytes that are not valid UTF-8 are returned base64-encoded.",
	FileSearch: "Search the files under a workspace path for a literal string or a regular expression (RE2 syntax: no backreferences or lookaround). " +
		"A line matches once. Results and bytes are capped, and the result says whether it is partial and which limit was reached.",
	FileCreate: "Create a new UTF-8 file in an existing directory of the workspace. It fails with ALREADY_EXISTS if the path is taken; expected_absent must be true. No directory is created.",
	FileEdit: "Replace one exact occurrence of old_text in a file with new_text. expected_hash must be the raw_hash of the file as it is now (from file.read). " +
		"It fails if the hash differs or if old_text does not occur exactly once; there is no fuzzy matching and no line-number correction.",
	ProcessExec: "Run a program directly, without a shell. executable is an absolute path and argv its arguments (not including the program itself). " +
		"Only programs and argument prefixes the host's policy lists will run. cwd is a workspace-relative directory, env_profile_ref names an environment profile, " +
		"and the program gets only that profile's variables. Output is stored as Evidence: read it with evidence.read.",
	EvidenceRead: "Read a byte range (at most 65536 bytes) of stored Evidence of this work, for example the output of an earlier call. " +
		"raw/v1 reads the stored bytes; text/v1 reads text and needs a range that starts and ends on character boundaries. Nothing is run again.",
}

// spec is one Tool's declaration: its arguments schema, as the design package ships it.
type spec struct {
	name       string
	parameters json.RawMessage
	revision   string
}

var (
	specsOnce sync.Once
	specs     map[string]spec
	specsErr  error
)

func loadSpecs() (map[string]spec, error) {
	specsOnce.Do(func() {
		raw, err := harness.Schemas.ReadFile("schemas/tools.schema.json")
		if err != nil {
			specsErr = fmt.Errorf("tools: the tool schema is not embedded: %w", err)
			return
		}
		var doc struct {
			OneOf []struct {
				Properties struct {
					Name struct {
						Const string `json:"const"`
					} `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"properties"`
			} `json:"oneOf"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			specsErr = fmt.Errorf("tools: the tool schema cannot be read: %w", err)
			return
		}
		out := map[string]spec{}
		for _, o := range doc.OneOf {
			name := o.Properties.Name.Const
			params, err := strictjson.Decode(o.Properties.Arguments)
			if err != nil || name == "" {
				specsErr = fmt.Errorf("tools: the tool schema has an entry that cannot be read")
				return
			}
			canonical, err := canon.Encode(params)
			if err != nil {
				specsErr = fmt.Errorf("tools: the tool schema cannot be canonicalized: %w", err)
				return
			}
			out[name] = spec{name: name, parameters: json.RawMessage(o.Properties.Arguments), revision: sha256Hex(canonical)}
		}
		for _, n := range AllTools() {
			if _, ok := out[n]; !ok {
				specsErr = fmt.Errorf("tools: the tool schema does not declare %s", n)
				return
			}
		}
		specs = out
	})
	return specs, specsErr
}

// SchemaRevision is the revision of a Tool's argument schema: the SHA-256 of its
// canonical JSON. A call is validated against the schema of that revision.
func SchemaRevision(name string) (string, bool) {
	m, err := loadSpecs()
	if err != nil {
		return "", false
	}
	s, ok := m[name]
	return s.revision, ok
}

// Catalog is the Tools declared in a Run's act request: those the Run's policy allows,
// in the mode it runs in, in name order. The schema of each is the design package's
// (schemas/tools.schema.json) and the same schema validates the call that comes back.
// An empty catalog means the request offers no Tool.
func (p *RunPolicy) Catalog() ([]modelport.FunctionTool, error) {
	m, err := loadSpecs()
	if err != nil {
		return nil, err
	}
	out := []modelport.FunctionTool{}
	for _, name := range p.Names() {
		s := m[name]
		out = append(out, modelport.FunctionTool{Type: "function", Function: modelport.ToolDeclaration{
			Name: name, Description: descriptions[name], Parameters: slices.Clone(s.parameters), Strict: true,
		}})
	}
	return out, nil
}
