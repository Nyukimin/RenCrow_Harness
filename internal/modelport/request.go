package modelport

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// RequestMeta is the operational metadata of one request. None of it takes part in
// the logical input digest.
type RequestMeta struct {
	RequestID string
	TraceID   string
	TaskID    string
	SessionID string
	Initiator string
	Caller    string
}

// ActParams are the inputs of one act request.
type ActParams struct {
	Binding    protocol.Binding
	Descriptor BindingDescriptor
	Messages   []ChatMessage
	// Tools are the Tools the model may call. None in this build.
	Tools    []FunctionTool
	Meta     RequestMeta
	Recovery RecoveryRequest
}

// NewActRequest builds the strict act request: streaming with usage, the options
// the binding's descriptor fixes for the act stage and no others, and the harness
// extension without expected_* values. The messages and tools are copied.
func NewActRequest(p ActParams) (ChatRequest, error) {
	if p.Binding.Selector == "" {
		return ChatRequest{}, errors.New("modelport: the binding has no selector")
	}
	if len(p.Messages) == 0 {
		return ChatRequest{}, errors.New("modelport: an act request needs messages")
	}
	opt := p.Descriptor.StageOptions.Act
	if opt.MaxTokens < 1 {
		return ChatRequest{}, errors.New("modelport: the binding fixes no act max_tokens")
	}
	choice := "none"
	if len(p.Tools) > 0 {
		choice = "auto"
	}
	var alias *string
	if p.Binding.Kind == "alias" {
		alias = Str(p.Binding.Selector)
	}
	opt2 := func(s string) *string { return &s }
	req := ChatRequest{
		Model:          p.Binding.Selector,
		Messages:       append([]ChatMessage{}, p.Messages...),
		Tools:          append([]FunctionTool{}, p.Tools...),
		ToolChoice:     choice,
		ResponseFormat: ResponseFormat{Type: "text"},
		Stream:         true,
		StreamOptions:  &RequestStreamOptions{IncludeUsage: true},
		MaxTokens:      opt.MaxTokens,
		Temperature:    copyNumber(opt.Temperature),
		TopP:           copyNumber(opt.TopP),
		Seed:           copyInt(opt.Seed),
		Stop:           append([]string{}, opt.Stop...),
		Rencrow: RencrowMetadata{
			RequestID: opt2(p.Meta.RequestID), TraceID: opt2(p.Meta.TraceID), TaskID: opt2(p.Meta.TaskID), SessionID: opt2(p.Meta.SessionID),
			Initiator: opt2(p.Meta.Initiator), Caller: opt2(p.Meta.Caller), Purpose: opt2(StageAct),
			AgentID: p.Binding.AgentID, ExecutionRole: p.Binding.ExecutionRole, ExecutionAlias: alias,
			Harness: HarnessExtension{
				ContractVersion: ContractVersion, Stage: StageAct, MaxBackendAttempts: 1, Recovery: p.Recovery,
			},
		},
	}
	return req, nil
}

// StageParams are the inputs of one compaction stage request.
type StageParams struct {
	Binding    protocol.Binding
	Descriptor BindingDescriptor
	// Stage is StageSelection or StageSummary.
	Stage    string
	Messages []ChatMessage
	Meta     RequestMeta
	Recovery RecoveryRequest
}

// NewStageRequest builds the strict request of a compaction stage: no Tool is declared and
// none may be chosen, the answer is one JSON object, nothing is streamed, the options are
// the ones the binding's descriptor fixes for that stage, and the harness extension carries
// the stage and no expected_* values. A stage is asked once; its recovery is always the
// same request, and the retry markers are null.
func NewStageRequest(p StageParams) (ChatRequest, error) {
	if p.Binding.Selector == "" {
		return ChatRequest{}, errors.New("modelport: the binding has no selector")
	}
	if len(p.Messages) == 0 {
		return ChatRequest{}, errors.New("modelport: a stage request needs messages")
	}
	var opt GenerationOptions
	switch p.Stage {
	case StageSelection:
		opt = p.Descriptor.StageOptions.InstructionSelection
	case StageSummary:
		opt = p.Descriptor.StageOptions.WorkSummary
	default:
		return ChatRequest{}, errors.New("modelport: not a compaction stage")
	}
	if opt.MaxTokens < 1 {
		return ChatRequest{}, errors.New("modelport: the binding fixes no max_tokens for this stage")
	}
	if p.Recovery.RetryOfRequestID != nil || p.Recovery.TriggerCode != nil || p.Recovery.ProfileID != ProfileSameRequest {
		return ChatRequest{}, errors.New("modelport: a stage is never a retry")
	}
	var alias *string
	if p.Binding.Kind == "alias" {
		alias = Str(p.Binding.Selector)
	}
	return ChatRequest{
		Model:          p.Binding.Selector,
		Messages:       append([]ChatMessage{}, p.Messages...),
		Tools:          []FunctionTool{},
		ToolChoice:     "none",
		ResponseFormat: ResponseFormat{Type: "json_object"},
		Stream:         false,
		MaxTokens:      opt.MaxTokens,
		Temperature:    copyNumber(opt.Temperature),
		TopP:           copyNumber(opt.TopP),
		Seed:           copyInt(opt.Seed),
		Stop:           append([]string{}, opt.Stop...),
		Rencrow: RencrowMetadata{
			RequestID: Str(p.Meta.RequestID), TraceID: Str(p.Meta.TraceID), TaskID: Str(p.Meta.TaskID), SessionID: Str(p.Meta.SessionID),
			Initiator: Str(p.Meta.Initiator), Caller: Str(p.Meta.Caller), Purpose: Str(p.Stage),
			AgentID: p.Binding.AgentID, ExecutionRole: p.Binding.ExecutionRole, ExecutionAlias: alias,
			Harness: HarnessExtension{ContractVersion: ContractVersion, Stage: p.Stage, MaxBackendAttempts: 1, Recovery: p.Recovery},
		},
	}, nil
}

func copyNumber(n *json.Number) *json.Number {
	if n == nil {
		return nil
	}
	c := *n
	return &c
}

func copyInt(n *int64) *int64 {
	if n == nil {
		return nil
	}
	c := *n
	return &c
}

// WithExpected returns a copy of the request carrying the three expected_* values a
// generation request must have. They come from the measure answer and the
// descriptor, never from the request itself.
func (r ChatRequest) WithExpected(inputDigest, requestDigest, bindingFingerprint string) ChatRequest {
	r.Rencrow.Harness.ExpectedInputDigest = inputDigest
	r.Rencrow.Harness.ExpectedRequestDigest = requestDigest
	r.Rencrow.Harness.ExpectedBindingFingerprint = bindingFingerprint
	return r
}

// LogicalInputDigest is the input digest of the request, computed by the Harness
// itself from the request as it would be sent.
func (r ChatRequest) LogicalInputDigest() (string, error) {
	raw, err := r.Canonical()
	if err != nil {
		return "", err
	}
	return protocol.InputDigest(raw)
}

// Validate checks the request against the strict contract schema: ChatRequest for a
// measure request (expected_* may be absent), GenerationRequest when generation is
// true (all of them required).
func (r ChatRequest) Validate(generation bool) error {
	raw, err := r.Canonical()
	if err != nil {
		return err
	}
	v, err := strictjson.Decode(raw)
	if err != nil {
		return fmt.Errorf("modelport: request: %w", err)
	}
	def := "ChatRequest"
	if generation {
		def = "GenerationRequest"
	}
	return schemacheck.Validate(schemacheck.LLMContract, def, v)
}

// ValidateMeasureResult checks one measure answer against its schema.
func ValidateMeasureResult(m MeasureResult) error {
	raw, err := marshal(m)
	if err != nil {
		return err
	}
	v, err := strictjson.Decode(raw)
	if err != nil {
		return fmt.Errorf("modelport: measure result: %w", err)
	}
	return schemacheck.Validate(schemacheck.Measure, "", v)
}
