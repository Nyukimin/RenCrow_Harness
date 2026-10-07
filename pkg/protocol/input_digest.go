package protocol

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

var (
	chatRequestFields = []string{
		"model", "messages", "tools", "tool_choice", "response_format", "stream", "stream_options",
		"max_tokens", "temperature", "top_p", "seed", "stop", "rencrow",
	}
	rencrowMetadataFields = []string{
		"request_id", "trace_id", "task_id", "session_id", "initiator", "caller", "purpose",
		"agent_id", "execution_role", "execution_alias",
	}
	harnessRequiredFields = []string{"contract_version", "stage", "max_backend_attempts", "recovery"}
	harnessOptionalFields = []string{"expected_input_digest", "expected_request_digest", "expected_binding_fingerprint"}
	recoveryFields        = []string{"profile_id", "profile_revision", "retry_of_request_id", "trigger_code"}
)

// InputDigest is the digest of the logical model input that the Harness and the
// Gateway compare before any generation:
//
//	D("rencrow-model-input/v1", LogicalInput)
//
// chatRequest is the JSON text of one ChatRequest. LogicalInput is exactly the
// generation fields (model, messages, tools, tool_choice, response_format, stream,
// stream_options, max_tokens, temperature, top_p, seed, stop), the routing triple
// (agent_id, execution_role, execution_alias) and the harness contract (contract
// version, stage, max_backend_attempts and the recovery profile id and revision).
// Operational metadata (request, trace, task and session ids, initiator, caller,
// purpose), every expected_* value and the retry markers are excluded, so they
// can change without changing the digest. A null generation option is itself a
// value meaning "use the fixed base profile value".
//
// The top-level and metadata field sets are checked exactly and the fields that
// take part are type-checked; the full ChatRequest schema (message shapes, ranges)
// is the caller's to validate. request_digest is not computed here: the Runtime
// owns it and the Harness only echoes it.
func InputDigest(chatRequest []byte) (string, error) {
	logical, err := logicalInput(chatRequest)
	if err != nil {
		return "", err
	}
	digest, err := canon.D("rencrow-model-input/v1", logical)
	if err != nil {
		return "", invalidWrap(err, "chat request")
	}
	return digest, nil
}

// LogicalInputCJ1 returns the CJ1 text of the logical input of one ChatRequest: the
// exact value InputDigest hashes, for a caller that keeps it as the immutable
// snapshot of a logical request. It adds no wire form; it exposes the existing one.
func LogicalInputCJ1(chatRequest []byte) ([]byte, error) { return logicalInputCJ1(chatRequest) }

// logicalInputCJ1 returns the CJ1 bytes that InputDigest hashes.
func logicalInputCJ1(chatRequest []byte) ([]byte, error) {
	logical, err := logicalInput(chatRequest)
	if err != nil {
		return nil, err
	}
	out, err := canon.Encode(logical)
	if err != nil {
		return nil, invalidWrap(err, "chat request")
	}
	return out, nil
}

func logicalInput(chatRequest []byte) (map[string]any, error) {
	v, err := strictjson.Decode(chatRequest)
	if err != nil {
		return nil, invalidWrap(err, "chat request")
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, invalid("chat request must be a JSON object")
	}
	if err := exactFields("chat request", root, chatRequestFields, nil); err != nil {
		return nil, err
	}

	if s, ok := root["model"].(string); !ok || s == "" {
		return nil, invalid("model must be a non-empty string")
	}
	if m, ok := root["messages"].([]any); !ok || len(m) == 0 {
		return nil, invalid("messages must be a non-empty array")
	}
	if _, ok := root["tools"].([]any); !ok {
		return nil, invalid("tools must be an array")
	}
	if s, ok := root["tool_choice"].(string); !ok || (s != "auto" && s != "none") {
		return nil, invalid("tool_choice must be auto or none")
	}
	rf, ok := root["response_format"].(map[string]any)
	if !ok {
		return nil, invalid("response_format must be an object")
	}
	if err := exactFields("response_format", rf, []string{"type"}, nil); err != nil {
		return nil, err
	}
	if s, ok := rf["type"].(string); !ok || (s != "text" && s != "json_object") {
		return nil, invalid("response_format.type must be text or json_object")
	}
	if err := checkStreamOptions(root["stream"], root["stream_options"]); err != nil {
		return nil, err
	}
	if err := checkInteger("max_tokens", root["max_tokens"], false); err != nil {
		return nil, err
	}
	if err := checkNumber("temperature", root["temperature"], true); err != nil {
		return nil, err
	}
	if err := checkNumber("top_p", root["top_p"], true); err != nil {
		return nil, err
	}
	if err := checkInteger("seed", root["seed"], true); err != nil {
		return nil, err
	}
	stop, ok := root["stop"].([]any)
	if !ok {
		return nil, invalid("stop must be an array")
	}
	for _, s := range stop {
		if _, ok := s.(string); !ok {
			return nil, invalid("stop entries must be strings")
		}
	}

	rencrow, ok := root["rencrow"].(map[string]any)
	if !ok {
		return nil, invalid("rencrow must be an object")
	}
	if err := exactFields("rencrow", rencrow, append(slices.Clone(rencrowMetadataFields), "harness"), nil); err != nil {
		return nil, err
	}
	for _, f := range rencrowMetadataFields {
		if err := checkStringOrNull("rencrow."+f, rencrow[f]); err != nil {
			return nil, err
		}
	}
	harness, ok := rencrow["harness"].(map[string]any)
	if !ok {
		return nil, invalid("rencrow.harness must be an object")
	}
	if err := exactFields("rencrow.harness", harness, harnessRequiredFields, harnessOptionalFields); err != nil {
		return nil, err
	}
	if harness["contract_version"] != "harness-v1" {
		return nil, invalid("harness.contract_version must be harness-v1")
	}
	if s, ok := harness["stage"].(string); !ok || !slices.Contains([]string{"act", "instruction_selection", "work_summary"}, s) {
		return nil, invalid("harness.stage is not a known stage")
	}
	if n, ok := harness["max_backend_attempts"].(json.Number); !ok || normalized(n) != "1" {
		return nil, invalid("harness.max_backend_attempts must be 1")
	}
	recovery, ok := harness["recovery"].(map[string]any)
	if !ok {
		return nil, invalid("harness.recovery must be an object")
	}
	if err := exactFields("harness.recovery", recovery, recoveryFields, nil); err != nil {
		return nil, err
	}
	if s, ok := recovery["profile_id"].(string); !ok || (s != "same_request" && s != "terminal_output_once") {
		return nil, invalid("recovery.profile_id is not a known profile")
	}
	if s, ok := recovery["profile_revision"].(string); !ok || s == "" {
		return nil, invalid("recovery.profile_revision must be a non-empty string")
	}
	for _, f := range []string{"retry_of_request_id", "trigger_code"} {
		if err := checkStringOrNull("recovery."+f, recovery[f]); err != nil {
			return nil, err
		}
	}

	return map[string]any{
		"model":           root["model"],
		"messages":        root["messages"],
		"tools":           root["tools"],
		"tool_choice":     root["tool_choice"],
		"response_format": root["response_format"],
		"stream":          root["stream"],
		"stream_options":  root["stream_options"],
		"max_tokens":      root["max_tokens"],
		"temperature":     root["temperature"],
		"top_p":           root["top_p"],
		"seed":            root["seed"],
		"stop":            root["stop"],
		"routing": map[string]any{
			"agent_id":        rencrow["agent_id"],
			"execution_role":  rencrow["execution_role"],
			"execution_alias": rencrow["execution_alias"],
		},
		"harness": map[string]any{
			"contract_version":     harness["contract_version"],
			"stage":                harness["stage"],
			"max_backend_attempts": harness["max_backend_attempts"],
			"recovery": map[string]any{
				"profile_id":       recovery["profile_id"],
				"profile_revision": recovery["profile_revision"],
			},
		},
	}, nil
}

// exactFields requires every field in required, allows those in optional and
// rejects anything else.
func exactFields(what string, obj map[string]any, required, optional []string) error {
	for _, f := range required {
		if _, ok := obj[f]; !ok {
			return invalid("%s is missing %q", what, f)
		}
	}
	for k := range obj {
		if !slices.Contains(required, k) && !slices.Contains(optional, k) {
			return invalid("%s has an undeclared field %q", what, k)
		}
	}
	return nil
}

func checkStreamOptions(stream, options any) error {
	on, ok := stream.(bool)
	if !ok {
		return invalid("stream must be a boolean")
	}
	if !on {
		if options != nil {
			return invalid("stream_options must be null when stream is false")
		}
		return nil
	}
	o, ok := options.(map[string]any)
	if !ok || len(o) != 1 || o["include_usage"] != true {
		return invalid("stream_options must be {include_usage:true} when stream is true")
	}
	return nil
}

func normalized(n json.Number) string {
	s, err := strictjson.NormalizeNumber(string(n))
	if err != nil {
		return ""
	}
	return s
}

func checkNumber(field string, v any, nullable bool) error {
	if v == nil && nullable {
		return nil
	}
	if _, ok := v.(json.Number); !ok {
		return invalid("%s must be a number", field)
	}
	return nil
}

func checkInteger(field string, v any, nullable bool) error {
	if v == nil && nullable {
		return nil
	}
	n, ok := v.(json.Number)
	if !ok || normalized(n) == "" || strings.Contains(normalized(n), ".") {
		return invalid("%s must be an integer", field)
	}
	return nil
}

func checkStringOrNull(field string, v any) error {
	if v == nil {
		return nil
	}
	if _, ok := v.(string); !ok {
		return invalid("%s must be a string or null", field)
	}
	return nil
}
