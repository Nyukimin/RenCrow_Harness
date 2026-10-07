package protocol

import "encoding/json"

// The DTOs below are the public types of schemas/protocol.schema.json. Identifiers
// and enumerations are plain strings: the schema patterns are the definition of a
// well-formed ID, and Decode applies them before any value reaches a field. A
// field the schema marks nullable is a pointer, so "absent" can never be confused
// with "null" (the schema requires every field to be present).
//
// Marshaling a DTO with encoding/json always emits every field. Use Encode to get
// bytes that are also validated against the schema and canonical (CJ1).

// Enumerations used in more than one place.
const (
	OriginHuman      = "human"
	OriginAutomation = "automation"
	OriginUnknown    = "unknown"

	KindCharacterSystemPrompt  = "character_system_prompt"
	KindStableRuntimeContext   = "stable_runtime_context"
	KindRecallPack             = "recall_pack"
	KindVariableRuntimeContext = "variable_runtime_context"
	KindUserMessage            = "user_message"

	ProofBasisDeclaredLocal = "declared_local"
	ProofBasisVerifiedRelay = "verified_relay"
	ProofBasisAutomation    = "automation"
	ProofBasisUnknown       = "unknown"

	EntrypointCLIInteractive  = "cli_interactive"
	EntrypointCLIExec         = "cli_exec"
	EntrypointCLIPipe         = "cli_pipe"
	EntrypointStdioCore       = "stdio_core"
	EntrypointStdioAutomation = "stdio_automation"

	ModeStructuredOnly = "structured_only"
	ModeTrustedHost    = "trusted_host"
	ModeIsolated       = "isolated"

	// The three dispositions of an appended input and the delivery states of its
	// receipt (PROTOCOL section 5, InputAppendInput and InputReceipt).
	DispositionNextStep         = "next_step"
	DispositionInterruptCurrent = "interrupt_current"
	DispositionNextTurn         = "next_turn"
	DeliveryQueued              = "queued"
	DeliveryApplied             = "applied"
	DeliveryDeferred            = "deferred"

	// The two codes of an InterruptReceipt: the stop signal was recorded, or the Run had
	// already ended and nothing was recorded.
	InterruptCancelRequested = "CANCEL_REQUESTED"
	InterruptAlreadyTerminal = "ALREADY_TERMINAL"

	// ProtocolVersion is the only protocol_version initialize accepts.
	ProtocolVersion = "rencrow-harness/v1"

	// MaxEvidenceReadBytes is the most one evidence/read call may return.
	MaxEvidenceReadBytes = 64 * 1024
)

// ErrorInfo is the wire form of a domain error.
type ErrorInfo struct {
	Code       string  `json:"code"`
	Message    string  `json:"message"`
	Retryable  bool    `json:"retryable"`
	EvidenceID *string `json:"evidence_id"`
}

// ContextBlock is one typed Host-supplied context block.
type ContextBlock struct {
	Kind     string     `json:"kind"`
	Text     string     `json:"text"`
	Revision string     `json:"revision"`
	Source   *SourceRef `json:"source"`
}

// Binding names the model binding a Thread uses.
type Binding struct {
	Kind            string  `json:"kind"`
	Selector        string  `json:"selector"`
	ProfileRevision string  `json:"profile_revision"`
	AgentID         *string `json:"agent_id"`
	ExecutionRole   *string `json:"execution_role"`
}

// Upstream is the caller-side correlation of a delegated Task.
type Upstream struct {
	Owner     string  `json:"owner"`
	TaskID    string  `json:"task_id"`
	TraceID   string  `json:"trace_id"`
	SessionID *string `json:"session_id"`
	ThreadID  *string `json:"thread_id"`
	TurnID    *string `json:"turn_id"`
	ActionID  *string `json:"action_id"`
	AttemptID *string `json:"attempt_id"`
}

// InputMessage is the raw user input and, for a CORE relay, its origin proof.
type InputMessage struct {
	Text        string       `json:"text"`
	OriginProof *OriginProof `json:"origin_proof"`
}

// Limits are the per-Run budgets. Every field is always sent.
type Limits struct {
	MaxModelSteps         int64 `json:"max_model_steps"`
	MaxToolCallsPerStep   int64 `json:"max_tool_calls_per_step"`
	DeadlineSeconds       int64 `json:"deadline_seconds"`
	MaxCaptureBytes       int64 `json:"max_capture_bytes"`
	MaxGenerationAttempts int64 `json:"max_generation_attempts"`
}

// StartInput is the params of turn/start.
type StartInput struct {
	ThreadID                string         `json:"thread_id"`
	Input                   InputMessage   `json:"input"`
	ContextBlocks           []ContextBlock `json:"context_blocks"`
	Upstream                *Upstream      `json:"upstream"`
	ExpectedContextRevision int64          `json:"expected_context_revision"`
	ExpectedControlRevision int64          `json:"expected_control_revision"`
	IdempotencyKey          string         `json:"idempotency_key"`
	Limits                  Limits         `json:"limits"`
}

// StartResult is the result of turn/start: the durable acceptance, not completion.
type StartResult struct {
	ReceiptID              string        `json:"receipt_id"`
	Accepted               bool          `json:"accepted"`
	SessionID              string        `json:"session_id"`
	ThreadID               string        `json:"thread_id"`
	TurnID                 string        `json:"turn_id"`
	TaskID                 string        `json:"task_id"`
	RunID                  string        `json:"run_id"`
	TraceID                string        `json:"trace_id"`
	EffectiveLimits        Limits        `json:"effective_limits"`
	DeadlineAt             string        `json:"deadline_at"`
	RecoveryPolicyRevision string        `json:"recovery_policy_revision"`
	Intake                 IntakeReceipt `json:"intake"`
}

// ResumeInput is the params of run/resume.
type ResumeInput struct {
	TaskID                  string   `json:"task_id"`
	ExpectedLastRunID       string   `json:"expected_last_run_id"`
	CheckpointID            *string  `json:"checkpoint_id"`
	ExpectedControlRevision int64    `json:"expected_control_revision"`
	Binding                 *Binding `json:"binding"`
	IdempotencyKey          string   `json:"idempotency_key"`
	Limits                  Limits   `json:"limits"`
}

// ResumeResult is the result of run/resume.
type ResumeResult struct {
	ReceiptID              string  `json:"receipt_id"`
	TaskID                 string  `json:"task_id"`
	ThreadID               string  `json:"thread_id"`
	PreviousRunID          string  `json:"previous_run_id"`
	RunID                  string  `json:"run_id"`
	TraceID                string  `json:"trace_id"`
	CheckpointID           *string `json:"checkpoint_id"`
	EffectiveLimits        Limits  `json:"effective_limits"`
	DeadlineAt             string  `json:"deadline_at"`
	RecoveryPolicyRevision string  `json:"recovery_policy_revision"`
}

// InputAppendInput is the params of input/append.
type InputAppendInput struct {
	ThreadID                string       `json:"thread_id"`
	RunID                   string       `json:"run_id"`
	Input                   InputMessage `json:"input"`
	Disposition             string       `json:"disposition"`
	ExpectedControlRevision int64        `json:"expected_control_revision"`
	IdempotencyKey          string       `json:"idempotency_key"`
}

// InputReceipt is the result of input/append.
type InputReceipt struct {
	ReceiptID       string        `json:"receipt_id"`
	QueueItemID     string        `json:"queue_item_id"`
	MessageID       string        `json:"message_id"`
	Origin          string        `json:"origin"`
	Disposition     string        `json:"disposition"`
	DeliveryState   string        `json:"delivery_state"`
	QueueRevision   int64         `json:"queue_revision"`
	ControlRevision int64         `json:"control_revision"`
	Intake          IntakeReceipt `json:"intake"`
}

// BudgetReport is one token-budget measurement.
type BudgetReport struct {
	State                    string  `json:"state"`
	PromptLower              *int64  `json:"prompt_lower"`
	PromptUpper              *int64  `json:"prompt_upper"`
	ContextLimit             *int64  `json:"context_limit"`
	ReservedOutputTokens     int64   `json:"reserved_output_tokens"`
	SafetyMarginTokens       int64   `json:"safety_margin_tokens"`
	RequestDigest            string  `json:"request_digest"`
	NormalizationFingerprint string  `json:"normalization_fingerprint"`
	EvidenceRef              *string `json:"evidence_ref"`
}

// CompactInput is the params of context/compact.
type CompactInput struct {
	ThreadID                string `json:"thread_id"`
	ExpectedContextRevision int64  `json:"expected_context_revision"`
	ExpectedControlRevision int64  `json:"expected_control_revision"`
	DryRun                  bool   `json:"dry_run"`
	IdempotencyKey          string `json:"idempotency_key"`
}

// CompactResult is the outcome of one compaction operation.
type CompactResult struct {
	Status                string        `json:"status"`
	Outcome               *string       `json:"outcome"`
	CheckpointID          *string       `json:"checkpoint_id"`
	Before                *BudgetReport `json:"before"`
	After                 *BudgetReport `json:"after"`
	RequiredMinimumTokens *int64        `json:"required_minimum_tokens"`
	AvailableTokens       *int64        `json:"available_tokens"`
	SemanticBoundary      *int64        `json:"semantic_boundary"`
	DurableBoundary       *int64        `json:"durable_boundary"`
	Error                 *ErrorInfo    `json:"error"`
}

// Verification records what was checked, separately from the Run status.
type Verification struct {
	Status           string   `json:"status"`
	EvidenceIDs      []string `json:"evidence_ids"`
	CriteriaRevision *string  `json:"criteria_revision"`
}

// RunResult is the determined end of a Run.
type RunResult struct {
	RunID               string       `json:"run_id"`
	TaskID              string       `json:"task_id"`
	Status              string       `json:"status"`
	Code                string       `json:"code"`
	FinalMessageID      *string      `json:"final_message_id"`
	FinalText           string       `json:"final_text"`
	Verification        Verification `json:"verification"`
	EvidenceIDs         []string     `json:"evidence_ids"`
	UnresolvedActionIDs []string     `json:"unresolved_action_ids"`
	LastCheckpointID    *string      `json:"last_checkpoint_id"`
	Resumable           bool         `json:"resumable"`
}

// RunInfo is the result of run/get.
type RunInfo struct {
	RunID                     string     `json:"run_id"`
	TaskID                    string     `json:"task_id"`
	ThreadID                  string     `json:"thread_id"`
	Phase                     string     `json:"phase"`
	Terminal                  bool       `json:"terminal"`
	Result                    *RunResult `json:"result"`
	ContextRevision           int64      `json:"context_revision"`
	ControlRevision           int64      `json:"control_revision"`
	LastEventSeq              int64      `json:"last_event_seq"`
	EffectiveLimits           Limits     `json:"effective_limits"`
	DeadlineAt                string     `json:"deadline_at"`
	RecoveryPolicyRevision    string     `json:"recovery_policy_revision"`
	GenerationAttemptsUsed    int64      `json:"generation_attempts_used"`
	GenerationAttemptsUnknown int64      `json:"generation_attempts_unknown"`
}

// SessionInfo is the public snapshot of one Thread.
type SessionInfo struct {
	SessionID       string  `json:"session_id"`
	ThreadID        string  `json:"thread_id"`
	WorkspacePath   string  `json:"workspace_path"`
	Binding         Binding `json:"binding"`
	PolicyRef       string  `json:"policy_ref"`
	ExecutionMode   string  `json:"execution_mode"`
	ContextRevision int64   `json:"context_revision"`
	ControlRevision int64   `json:"control_revision"`
	ActiveRunID     *string `json:"active_run_id"`
}

// InitializeInput is the params of initialize.
type InitializeInput struct {
	ClientName      string `json:"client_name"`
	ClientVersion   string `json:"client_version"`
	ProtocolVersion string `json:"protocol_version"`
}

// Capability is one reported capability and the basis for its status.
type Capability struct {
	Name   string  `json:"name"`
	Status string  `json:"status"`
	Basis  string  `json:"basis"`
	Reason *string `json:"reason"`
}

// CapabilitiesResult is the result of initialize and service/capabilities.
type CapabilitiesResult struct {
	ProtocolVersion string       `json:"protocol_version"`
	BuildRevision   string       `json:"build_revision"`
	Capabilities    []Capability `json:"capabilities"`
}

// SessionOpenInput is the params of session/open.
type SessionOpenInput struct {
	WorkspacePath  string  `json:"workspace_path"`
	Binding        Binding `json:"binding"`
	PolicyRef      string  `json:"policy_ref"`
	ExecutionMode  string  `json:"execution_mode"`
	IdempotencyKey string  `json:"idempotency_key"`
}

// SessionOpenResult is the result of session/open.
type SessionOpenResult struct {
	ReceiptID string      `json:"receipt_id"`
	Session   SessionInfo `json:"session"`
}

// SessionListInput is the params of session/list.
type SessionListInput struct {
	Cursor *string `json:"cursor"`
	Limit  int64   `json:"limit"`
}

// SessionListResult is the result of session/list.
type SessionListResult struct {
	Sessions   []SessionInfo `json:"sessions"`
	NextCursor *string       `json:"next_cursor"`
}

// SessionGetInput is the params of session/get.
type SessionGetInput struct {
	ThreadID string `json:"thread_id"`
}

// SessionForkInput is the params of session/fork.
type SessionForkInput struct {
	ThreadID       string `json:"thread_id"`
	CheckpointID   string `json:"checkpoint_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

// ForkResult is the result of session/fork.
type ForkResult struct {
	ReceiptID      string `json:"receipt_id"`
	SourceThreadID string `json:"source_thread_id"`
	ThreadID       string `json:"thread_id"`
	SessionID      string `json:"session_id"`
	CheckpointID   string `json:"checkpoint_id"`
}

// InterruptInput is the params of turn/interrupt.
type InterruptInput struct {
	RunID                   string `json:"run_id"`
	ExpectedControlRevision int64  `json:"expected_control_revision"`
	IdempotencyKey          string `json:"idempotency_key"`
}

// InterruptReceipt is the result of turn/interrupt.
type InterruptReceipt struct {
	ReceiptID       string `json:"receipt_id"`
	RunID           string `json:"run_id"`
	SignalRecorded  bool   `json:"signal_recorded"`
	ControlRevision int64  `json:"control_revision"`
	Code            string `json:"code"`
}

// RunGetInput is the params of run/get.
type RunGetInput struct {
	RunID string `json:"run_id"`
}

// OperationAccepted is the result of an operation accepted for later completion.
type OperationAccepted struct {
	ReceiptID string `json:"receipt_id"`
	Accepted  bool   `json:"accepted"`
}

// ReceiptGetInput is the params of receipt/get.
type ReceiptGetInput struct {
	ReceiptID string `json:"receipt_id"`
}

// ReceiptPayload is the tagged union of operation results a receipt can hold.
// Type is the schema discriminator and Value the canonical JSON of that result.
type ReceiptPayload struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// ReceiptRecord is the result of receipt/get.
type ReceiptRecord struct {
	ReceiptID string          `json:"receipt_id"`
	Operation string          `json:"operation"`
	Stage     string          `json:"stage"`
	Result    *ReceiptPayload `json:"result"`
	Error     *ErrorInfo      `json:"error"`
}

// EventsReadInput is the params of events/read.
type EventsReadInput struct {
	ThreadID string `json:"thread_id"`
	AfterSeq int64  `json:"after_seq"`
	Limit    int64  `json:"limit"`
}

// EventsReadResult is the result of events/read.
type EventsReadResult struct {
	Events       []Event `json:"events"`
	NextAfterSeq int64   `json:"next_after_seq"`
	HasMore      bool    `json:"has_more"`
}

// EvidenceReadInput is the params of evidence/read.
type EvidenceReadInput struct {
	EvidenceID        string    `json:"evidence_id"`
	ProjectionVersion string    `json:"projection_version"`
	Range             ByteRange `json:"range"`
}

// EvidenceReadResult is the result of evidence/read.
type EvidenceReadResult struct {
	EvidenceID        string    `json:"evidence_id"`
	ProjectionVersion string    `json:"projection_version"`
	DataBase64        string    `json:"data_base64"`
	TotalBytes        int64     `json:"total_bytes"`
	ReturnedRange     ByteRange `json:"returned_range"`
	Partial           bool      `json:"partial"`
	RawHash           string    `json:"raw_hash"`
	ProjectionHash    string    `json:"projection_hash"`
	CaptureComplete   bool      `json:"capture_complete"`
}

// ShutdownInput is the params of service/shutdown.
type ShutdownInput struct {
	Mode            string `json:"mode"`
	DeadlineSeconds int64  `json:"deadline_seconds"`
}

// ShutdownResult is the result of service/shutdown.
type ShutdownResult struct {
	Accepted bool `json:"accepted"`
}

// EmptyInput is the params of methods that take none.
type EmptyInput struct{}

// RecoveryPolicy is the act recovery policy a Run freezes.
type RecoveryPolicy struct {
	ContractVersion   string   `json:"contract_version"`
	MaxAttemptsPerAct int64    `json:"max_attempts_per_act"`
	AllowedProfiles   []string `json:"allowed_profiles"`
}

// IntakeReceipt is the durable record of one accepted input and where its origin
// came from.
type IntakeReceipt struct {
	ReceiptID           string  `json:"receipt_id"`
	MessageID           string  `json:"message_id"`
	ThreadID            string  `json:"thread_id"`
	EvidenceID          string  `json:"evidence_id"`
	Principal           string  `json:"principal"`
	Entrypoint          string  `json:"entrypoint"`
	CallerProfileDigest string  `json:"caller_profile_digest"`
	DeclaredOrigin      string  `json:"declared_origin"`
	EffectiveOrigin     string  `json:"effective_origin"`
	ProofBasis          string  `json:"proof_basis"`
	ProofDigest         *string `json:"proof_digest"`
	AcceptedSequence    int64   `json:"accepted_sequence"`
	AcceptedAt          string  `json:"accepted_at"`
}

// ModelAttemptReceipt is the Harness record of one physical model generation request.
type ModelAttemptReceipt struct {
	ActionID                string   `json:"action_id"`
	AttemptID               string   `json:"attempt_id"`
	Ordinal                 int64    `json:"ordinal"`
	RequestID               string   `json:"request_id"`
	ResponseID              *string  `json:"response_id"`
	Stage                   string   `json:"stage"`
	BaseRequestDigest       string   `json:"base_request_digest"`
	RequestDigest           string   `json:"request_digest"`
	BindingFingerprint      string   `json:"binding_fingerprint"`
	RecoveryProfile         string   `json:"recovery_profile"`
	RecoveryProfileRevision string   `json:"recovery_profile_revision"`
	AppliedTransformations  []string `json:"applied_transformations"`
	FailureCode             *string  `json:"failure_code"`
	GenerationState         string   `json:"generation_state"`
	BackendAttempts         *int64   `json:"backend_attempts"`
	UsageComplete           bool     `json:"usage_complete"`
	RequestEvidenceID       string   `json:"request_evidence_id"`
	ResponseEvidenceID      *string  `json:"response_evidence_id"`
	InputDigest             string   `json:"input_digest"`
}

// ProgressDelta is a provisional text delta notification (progress/delta).
type ProgressDelta struct {
	RunID       string `json:"run_id"`
	AttemptID   string `json:"attempt_id"`
	Ordinal     int64  `json:"ordinal"`
	Text        string `json:"text"`
	Provisional bool   `json:"provisional"`
}

// ProgressReset discards a failed Attempt's provisional text (progress/reset).
type ProgressReset struct {
	RunID        string `json:"run_id"`
	OldAttemptID string `json:"old_attempt_id"`
	NewAttemptID string `json:"new_attempt_id"`
	Reason       string `json:"reason"`
	Provisional  bool   `json:"provisional"`
}

// ProgressGap marks missed progress deltas (progress/gap).
type ProgressGap struct {
	RunID       string `json:"run_id"`
	AttemptID   string `json:"attempt_id"`
	FromOrdinal int64  `json:"from_ordinal"`
	ToOrdinal   int64  `json:"to_ordinal"`
}

// Event is one durable Thread event. Payload is the canonical (CJ1) JSON of the
// type-specific payload, byte-for-byte what events.payload_json stores.
type Event struct {
	EventID    string          `json:"event_id"`
	EventSeq   int64           `json:"event_seq"`
	ThreadID   string          `json:"thread_id"`
	TaskID     *string         `json:"task_id"`
	RunID      *string         `json:"run_id"`
	Type       string          `json:"type"`
	ReceiptID  *string         `json:"receipt_id"`
	EvidenceID *string         `json:"evidence_id"`
	MessageID  *string         `json:"message_id"`
	Code       *string         `json:"code"`
	RecordedAt string          `json:"recorded_at"`
	Payload    json.RawMessage `json:"payload"`
}

// Str returns a pointer to s, for filling a nullable field.
func Str(s string) *string { return &s }

// Int returns a pointer to n, for filling a nullable field.
func Int(n int64) *int64 { return &n }
