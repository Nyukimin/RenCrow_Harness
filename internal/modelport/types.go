// Package modelport is the Harness side of the model boundary: the wire types of
// the strict LLM contract (schemas/llm_contract.schema.json), the ModelPort the
// kernel calls, and AssembleStrictStream, which turns one strict SSE response into
// a staged completion.
//
// The package holds no client. The one real implementation is internal/modelclient,
// which talks to RenCrow_LLM over its loopback Gateway and which the `serve`
// composition builds from its configuration; the other ModelPort is the test double
// in internal/harnesstest, which no production composition imports.
//
// Nothing here dispatches a Tool. A completion carries the Tool calls the model
// asked for as intents, staged and never acted on, and only when the whole stream
// ended well.
package modelport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

// ContractVersion is the only LLM contract version this build speaks.
const ContractVersion = "harness-v1"

// Stages of one model request. Only act is generated in this build.
const (
	StageAct       = "act"
	StageSelection = "instruction_selection"
	StageSummary   = "work_summary"
)

// Generation states of one attempt (ERROR_MAPPING section 1).
const (
	StateNotStarted = "not_started"
	StateTerminal   = "terminal"
	StateUnknown    = "unknown"
)

// Recovery profile ids.
const (
	ProfileSameRequest        = "same_request"
	ProfileTerminalOutputOnce = "terminal_output_once"
)

// Normalized failure codes (ERROR_MAPPING). A code is data for the kernel's
// classification, not a message.
const (
	CodeReasoningOnly         = "REASONING_ONLY"
	CodeEmptyFinalContent     = "EMPTY_FINAL_CONTENT"
	CodeRawToolMarkup         = "RAW_TOOL_MARKUP"
	CodeOutputSchemaInvalid   = "MODEL_OUTPUT_SCHEMA_INVALID"
	CodeOutputDegenerate      = "MODEL_OUTPUT_DEGENERATE"
	CodeConnectFailed         = "CONNECT_FAILED"
	CodeUpstreamTransient     = "UPSTREAM_TRANSIENT"
	CodeRateLimited           = "RATE_LIMITED"
	CodeQueueTimeout          = "QUEUE_TIMEOUT"
	CodeContextLimit          = "CONTEXT_LIMIT_EXCEEDED"
	CodeLength                = "LENGTH"
	CodeIncomplete            = "INCOMPLETE"
	CodeRefused               = "REFUSED"
	CodeBindingChanged        = "BINDING_CHANGED"
	CodeUnsupportedContract   = "UNSUPPORTED_CONTRACT"
	CodeUnsupportedRecovery   = "UNSUPPORTED_RECOVERY_PROFILE"
	CodeAuthFailed            = "AUTH_FAILED"
	CodeInputDigestMismatch   = "INPUT_DIGEST_MISMATCH"
	CodeRequestDigestMismatch = "REQUEST_DIGEST_MISMATCH"
	CodeModelUnavailable      = "MODEL_UNAVAILABLE"
	CodeContractFailed        = "MODEL_CONTRACT_FAILED"
	CodeOutcomeUnknown        = "MODEL_GENERATION_OUTCOME_UNKNOWN"
	CodeCancelled             = "CANCELLED"
	CodePermitRevoked         = "PERMIT_REVOKED"
	CodeBudgetUnverified      = "BUDGET_UNVERIFIED"
)

// ChatMessage is one message of a Chat request. The shape depends on the role and
// is closed: system, developer and user carry a string content; assistant carries
// a content (a string or null) and always its tool_calls (possibly empty); tool
// carries the tool_call_id and a string content. Content nil means JSON null and
// is valid for an assistant only.
type ChatMessage struct {
	Role       string
	Content    *string
	ToolCalls  []ToolCall
	ToolCallID string
}

// ToolCall is one Tool call of an assistant message.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is the function of a ToolCall; Arguments is one JSON text kept as
// the string it was received as.
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Str returns a pointer to s.
func Str(s string) *string { return &s }

// System, Developer, User, Assistant and Tool build messages.
func System(text string) ChatMessage    { return ChatMessage{Role: "system", Content: &text} }
func Developer(text string) ChatMessage { return ChatMessage{Role: "developer", Content: &text} }
func User(text string) ChatMessage      { return ChatMessage{Role: "user", Content: &text} }

// Assistant is an assistant message with a text body and no Tool call.
func Assistant(text string) ChatMessage {
	return ChatMessage{Role: "assistant", Content: &text, ToolCalls: []ToolCall{}}
}

// Tool is the result message of one Tool call.
func Tool(callID, content string) ChatMessage {
	return ChatMessage{Role: "tool", ToolCallID: callID, Content: &content}
}

// Text returns the content, or "" when it is null.
func (m ChatMessage) Text() string {
	if m.Content == nil {
		return ""
	}
	return *m.Content
}

// MarshalJSON writes exactly the fields of the message's role.
func (m ChatMessage) MarshalJSON() ([]byte, error) {
	switch m.Role {
	case "system", "developer", "user":
		if m.Content == nil || len(m.ToolCalls) != 0 || m.ToolCallID != "" {
			return nil, fmt.Errorf("modelport: a %s message carries only a string content", m.Role)
		}
		return marshal(struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{m.Role, *m.Content})
	case "assistant":
		if m.ToolCallID != "" {
			return nil, errors.New("modelport: an assistant message has no tool_call_id")
		}
		calls := m.ToolCalls
		if calls == nil {
			calls = []ToolCall{}
		}
		return marshal(struct {
			Role      string     `json:"role"`
			Content   *string    `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		}{m.Role, m.Content, calls})
	case "tool":
		if m.Content == nil || m.ToolCallID == "" || len(m.ToolCalls) != 0 {
			return nil, errors.New("modelport: a tool message carries a tool_call_id and a string content")
		}
		return marshal(struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			Content    string `json:"content"`
		}{m.Role, m.ToolCallID, *m.Content})
	}
	return nil, fmt.Errorf("modelport: unknown message role")
}

// UnmarshalJSON reads one message strictly: the fields of its role, all required,
// none extra.
func (m *ChatMessage) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("modelport: message: %w", err)
	}
	var role string
	if r, ok := raw["role"]; !ok || json.Unmarshal(r, &role) != nil {
		return errors.New("modelport: message has no role")
	}
	var want []string
	switch role {
	case "system", "developer", "user":
		want = []string{"role", "content"}
	case "assistant":
		want = []string{"role", "content", "tool_calls"}
	case "tool":
		want = []string{"role", "tool_call_id", "content"}
	default:
		return errors.New("modelport: unknown message role")
	}
	if len(raw) != len(want) {
		return errors.New("modelport: message fields do not match its role")
	}
	for _, k := range want {
		if _, ok := raw[k]; !ok {
			return errors.New("modelport: message fields do not match its role")
		}
	}
	out := ChatMessage{Role: role}
	if c := raw["content"]; string(c) != "null" {
		var s string
		if err := json.Unmarshal(c, &s); err != nil {
			return errors.New("modelport: message content is not a string")
		}
		out.Content = &s
	} else if role != "assistant" {
		return errors.New("modelport: only an assistant message has a null content")
	}
	if role == "assistant" {
		out.ToolCalls = []ToolCall{}
		d := json.NewDecoder(bytes.NewReader(raw["tool_calls"]))
		d.DisallowUnknownFields()
		if err := d.Decode(&out.ToolCalls); err != nil || out.ToolCalls == nil {
			return errors.New("modelport: message tool_calls is not a list of tool calls")
		}
	}
	if role == "tool" {
		if err := json.Unmarshal(raw["tool_call_id"], &out.ToolCallID); err != nil || out.ToolCallID == "" {
			return errors.New("modelport: tool message has no tool_call_id")
		}
	}
	*m = out
	return nil
}

// FunctionTool is one Tool the model may call.
type FunctionTool struct {
	Type     string          `json:"type"`
	Function ToolDeclaration `json:"function"`
}

// ToolDeclaration is the declaration of a function Tool (name, description and the
// JSON Schema of its arguments).
type ToolDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

// RequestStreamOptions is the stream_options of a streaming request.
type RequestStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ResponseFormat is the response_format of a request.
type ResponseFormat struct {
	Type string `json:"type"`
}

// RecoveryRequest names the recovery profile of one request.
type RecoveryRequest struct {
	ProfileID        string  `json:"profile_id"`
	ProfileRevision  string  `json:"profile_revision"`
	RetryOfRequestID *string `json:"retry_of_request_id"`
	TriggerCode      *string `json:"trigger_code"`
}

// HarnessExtension is rencrow.harness of a request. The expected_* values are
// absent from a measure request and all present on a generation request.
type HarnessExtension struct {
	ContractVersion            string          `json:"contract_version"`
	Stage                      string          `json:"stage"`
	MaxBackendAttempts         int64           `json:"max_backend_attempts"`
	Recovery                   RecoveryRequest `json:"recovery"`
	ExpectedInputDigest        string          `json:"expected_input_digest,omitempty"`
	ExpectedRequestDigest      string          `json:"expected_request_digest,omitempty"`
	ExpectedBindingFingerprint string          `json:"expected_binding_fingerprint,omitempty"`
}

// RencrowMetadata is the rencrow member of a request. A nil pointer is JSON null.
type RencrowMetadata struct {
	RequestID      *string          `json:"request_id"`
	TraceID        *string          `json:"trace_id"`
	TaskID         *string          `json:"task_id"`
	SessionID      *string          `json:"session_id"`
	Initiator      *string          `json:"initiator"`
	Caller         *string          `json:"caller"`
	Purpose        *string          `json:"purpose"`
	AgentID        *string          `json:"agent_id"`
	ExecutionRole  *string          `json:"execution_role"`
	ExecutionAlias *string          `json:"execution_alias"`
	Harness        HarnessExtension `json:"harness"`
}

// ChatRequest is one Chat request of the strict contract. Every field is sent; a
// nil Temperature, TopP or Seed is JSON null and means "use the value of the fixed
// base profile". Numbers keep their written form (json.Number).
type ChatRequest struct {
	Model          string                `json:"model"`
	Messages       []ChatMessage         `json:"messages"`
	Tools          []FunctionTool        `json:"tools"`
	ToolChoice     string                `json:"tool_choice"`
	ResponseFormat ResponseFormat        `json:"response_format"`
	Stream         bool                  `json:"stream"`
	StreamOptions  *RequestStreamOptions `json:"stream_options"`
	MaxTokens      int64                 `json:"max_tokens"`
	Temperature    *json.Number          `json:"temperature"`
	TopP           *json.Number          `json:"top_p"`
	Seed           *int64                `json:"seed"`
	Stop           []string              `json:"stop"`
	Rencrow        RencrowMetadata       `json:"rencrow"`
}

// Canonical returns the request as CJ1 bytes: the form that is stored as Evidence
// and from which the logical input digest is taken.
func (r ChatRequest) Canonical() ([]byte, error) {
	if r.Tools == nil {
		r.Tools = []FunctionTool{}
	}
	if r.Stop == nil {
		r.Stop = []string{}
	}
	raw, err := marshal(r)
	if err != nil {
		return nil, err
	}
	v, err := strictjson.Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("modelport: request: %w", err)
	}
	return canon.Encode(v)
}

// MeasureRequest is the body of a measure call: a Chat request and the safety
// margin, which is not part of either digest.
type MeasureRequest struct {
	ContractVersion    string      `json:"contract_version"`
	Request            ChatRequest `json:"request"`
	SafetyMarginTokens int64       `json:"safety_margin_tokens"`
}

// MeasureResult is the answer of a measure call (schemas/measure.schema.json).
type MeasureResult struct {
	ContractVersion       string  `json:"contract_version"`
	State                 string  `json:"state"`
	PromptLower           *int64  `json:"prompt_lower"`
	PromptUpper           *int64  `json:"prompt_upper"`
	EffectiveContextLimit *int64  `json:"effective_context_limit"`
	ReservedOutputTokens  int64   `json:"reserved_output_tokens"`
	SafetyMarginTokens    int64   `json:"safety_margin_tokens"`
	RequestDigest         string  `json:"request_digest"`
	BindingFingerprint    string  `json:"binding_fingerprint"`
	EvidenceRef           *string `json:"evidence_ref"`
	Reason                *string `json:"reason"`
	InputDigest           string  `json:"input_digest"`
}

// GenerationOptions are the generation options one stage of a binding fixes.
type GenerationOptions struct {
	MaxTokens   int64        `json:"max_tokens"`
	Temperature *json.Number `json:"temperature"`
	TopP        *json.Number `json:"top_p"`
	Seed        *int64       `json:"seed"`
	Stop        []string     `json:"stop"`
}

// StageOptions are the generation options of the three stages.
type StageOptions struct {
	Act                  GenerationOptions `json:"act"`
	InstructionSelection GenerationOptions `json:"instruction_selection"`
	WorkSummary          GenerationOptions `json:"work_summary"`
}

// RecoveryProfile describes one recovery profile a binding offers.
type RecoveryProfile struct {
	ProfileID       string   `json:"profile_id"`
	ProfileRevision string   `json:"profile_revision"`
	Stages          []string `json:"stages"`
	Transformations []string `json:"transformations"`
	Codes           []string `json:"codes"`
}

// DescribedBinding is the binding a descriptor answers for.
type DescribedBinding struct {
	Kind            string  `json:"kind"`
	Selector        string  `json:"selector"`
	ProfileRevision string  `json:"profile_revision"`
	AgentID         *string `json:"agent_id"`
	ExecutionRole   *string `json:"execution_role"`
}

// BindingDescriptor is what the LLM owner publishes about one binding: its
// fingerprint, the generation options of each stage and the recovery profiles it
// offers. The Harness takes every option from here and invents none.
type BindingDescriptor struct {
	Binding            DescribedBinding  `json:"binding"`
	BindingFingerprint string            `json:"binding_fingerprint"`
	StageOptions       StageOptions      `json:"stage_options"`
	RecoveryProfiles   []RecoveryProfile `json:"recovery_profiles"`
	Measure            bool              `json:"measure"`
	Normalization      string            `json:"normalization"`
}

// RecoveryProfile returns the profile with this id that serves the stage.
func (d BindingDescriptor) RecoveryProfile(id, stage string) (RecoveryProfile, bool) {
	for _, p := range d.RecoveryProfiles {
		if p.ProfileID != id {
			continue
		}
		for _, s := range p.Stages {
			if s == stage {
				return p, true
			}
		}
	}
	return RecoveryProfile{}, false
}

// Usage is the token usage of one generation; an unknown figure is nil.
type Usage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	CachedTokens     *int64 `json:"cached_tokens"`
}

// GatewayAttemptReceipt is what the Gateway reports about one generation attempt.
type GatewayAttemptReceipt struct {
	ContractVersion         string   `json:"contract_version"`
	Stage                   string   `json:"stage"`
	BindingFingerprint      *string  `json:"binding_fingerprint"`
	RequestDigest           *string  `json:"request_digest"`
	LogicalRequests         int64    `json:"logical_requests"`
	BackendAttempts         *int64   `json:"backend_attempts"`
	GenerationState         string   `json:"generation_state"`
	HiddenRetry             bool     `json:"hidden_retry"`
	RecoveryProfile         string   `json:"recovery_profile"`
	RecoveryProfileRevision string   `json:"recovery_profile_revision"`
	AppliedTransformations  []string `json:"applied_transformations"`
	UsageComplete           bool     `json:"usage_complete"`
	InputDigest             *string  `json:"input_digest"`
}

// StreamTerminal is the single terminal frame of a strict stream.
type StreamTerminal struct {
	ContractVersion    string                `json:"contract_version"`
	Outcome            string                `json:"outcome"`
	FinishReason       *string               `json:"finish_reason"`
	Code               *string               `json:"code"`
	ProviderResponseID *string               `json:"provider_response_id"`
	Usage              *Usage                `json:"usage"`
	HarnessReceipt     GatewayAttemptReceipt `json:"harness_receipt"`
}

// StrictError is a refusal the Gateway or Runtime gave before generating (a non-2xx
// strict error), or the error of a measure call. Receipt is nil when the request
// could not even be parsed far enough to have one.
type StrictError struct {
	Code       string
	Message    string
	Retryable  bool
	RequestID  *string
	SourceCode *string
	Receipt    *GatewayAttemptReceipt
	// Body is the refusal's own response body, as received, for a caller that keeps
	// what the model side said as Evidence: at most MaxFailureBodyBytes of it, and
	// BodyTruncated says there was more. It is nil when the error did not come from a
	// response (a refusal the client made itself, a call that never got an answer). It
	// is never part of Error().
	Body          []byte
	BodyTruncated bool
}

// MaxFailureBodyBytes is the most of a non-2xx response body that is kept as Evidence.
// A strict error is a few hundred bytes; a body longer than this is not a strict error
// and its remainder is not kept.
const MaxFailureBodyBytes = 64 << 10

func (e *StrictError) Error() string { return "modelport: " + e.Code }

// GenerationState returns the generation state this error establishes: what its
// receipt says, or unknown when there is no receipt. A pre-generation refusal
// without a receipt is not assumed to be not_started.
func (e *StrictError) GenerationState() string {
	if e.Receipt == nil {
		return StateUnknown
	}
	return e.Receipt.GenerationState
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
