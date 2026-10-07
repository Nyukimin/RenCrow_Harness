package harnesstest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// FakeFingerprint is the binding fingerprint the fake model publishes.
const FakeFingerprint = "bfp-v1:cf25685e3cbff5f9364f8bdadb54d8b852578931ad2602a62e077613b3a6ef8d"

// Kind selects what the fake model does when it is asked to generate.
type Kind int

const (
	// KindFinal streams the Reply's text and ends well.
	KindFinal Kind = iota
	// KindToolCall streams one Tool call (Name, Args) and ends well with tool_calls.
	KindToolCall
	// KindLength streams the text and ends by length: incomplete.
	KindLength
	// KindRefusal ends with a refusal.
	KindRefusal
	// KindReasoningOnly streams reasoning and no content, and ends as if completed.
	KindReasoningOnly
	// KindErrorOutcome ends with the terminal outcome error and the Reply's Code.
	KindErrorOutcome
	// KindNoTerminal streams the text and then just ends: no terminal frame, no [DONE].
	KindNoTerminal
	// KindDoneOnly streams the text and [DONE] without the terminal frame.
	KindDoneOnly
	// KindRaw writes Raw as the whole response.
	KindRaw
	// KindStrictError refuses before generating, with a strict error (Code, and the
	// receipt's State: not_started with a receipt, or no receipt when NoReceipt).
	KindStrictError
	// KindTransportError fails the call itself with an error that says nothing.
	KindTransportError
	// KindHang opens a stream that gives nothing until it is closed or the context ends.
	KindHang
)

// Call is one Tool call of a KindToolCall reply.
type Call struct {
	ID   string // "fake-call-<n>" when empty
	Name string
	Args string
}

// Reply is one scripted answer of the fake model.
type Reply struct {
	Kind      Kind
	Text      string   // content of a final or length reply, or the text that comes with Tool calls
	Chunks    []string // when set, the content in these pieces
	Reasoning string
	Name      string // KindToolCall
	Args      string
	Calls     []Call // KindToolCall: several calls in one response (Name and Args stand for one)
	Code      string // KindErrorOutcome, KindStrictError
	NoReceipt bool   // KindStrictError
	// State is the generation state of a KindStrictError's receipt: not_started (the
	// default), terminal (one backend attempt) or unknown (none counted).
	State string
	// Body, when set with KindStrictError, is the body of the refusal: the bytes the error
	// carries as its response (and the Gateway double writes verbatim). BodyTruncated
	// says the body the error carries is only the first part of a longer one.
	Body          string
	BodyTruncated bool
	Raw           string // KindRaw
	// Stream, when set with KindRaw, makes the whole response from the digests of the
	// request it answers (what a recorded stream needs to be a valid answer to it).
	Stream func(inputDigest, requestDigest string) string
	// Dynamic, when set, makes the reply from the request it answers: what a model that
	// reads the answer of an earlier Tool call and asks for the next does.
	Dynamic func(req modelport.ChatRequest) Reply
	// Mutate edits the terminal frame before it is sent (a test of a bad receipt).
	Mutate func(terminal map[string]any)
}

// Final is a reply that streams text and ends well.
func Final(text string) Reply { return Reply{Kind: KindFinal, Text: text} }

// MeasureConfig is what the fake model's counting says.
type MeasureConfig struct {
	State string // verified_exact (default), verified_bound, estimated or unverified
	// Lower and Upper, when set, are the counted prompt tokens; when both are zero the
	// count is the size of the messages divided by four.
	Lower, Upper int64
	Limit        int64 // effective context limit; 32768 when zero
}

// Fake is a test double of modelport.ModelPort. It is test support only: no
// production composition imports it, and nothing in a configuration can select it.
// It counts and generates what it is told to, checks the expected_* values the way
// the Gateway does, and records every call.
type Fake struct {
	mu      sync.Mutex
	reply   Reply
	measure MeasureConfig
	// DescribeErr, MeasureErr and GenerateErr, when set, make the call fail.
	DescribeErr error
	MeasureErr  error
	// MeasureHook may replace the answer (or fail) after the fake computed it.
	MeasureHook func(req modelport.MeasureRequest, res *modelport.MeasureResult) error
	// OnMeasure runs when Measure is called, before it answers, with the caller's
	// context. A test blocks here to hold a Run in its Measuring phase.
	OnMeasure func(ctx context.Context)
	// OnGenerate runs when Generate is called, after the call was recorded and the
	// expected_* values were checked, before anything is returned. A test blocks here
	// to hold a generation in flight.
	OnGenerate func(ctx context.Context, req modelport.ChatRequest)
	// Fingerprint overrides the published binding fingerprint.
	Fingerprint string
	// BreakBindingAfterMeasure changes the fingerprint after the first measure, as if
	// the template changed between the count and the generation.
	BreakBindingAfterMeasure bool
	// OfferTerminalOutputOnce makes the descriptor publish the terminal_output_once
	// profile (format_suffix, for the four format codes). Without it the binding offers
	// only same_request.
	OfferTerminalOutputOnce bool
	// Profiles, when set, replaces the recovery profiles the descriptor publishes.
	Profiles []modelport.RecoveryProfile
	// Applied, when set, replaces the transformations the receipt of a terminal_output_once
	// generation states (default format_suffix).
	Applied []string

	script    []Reply
	scriptAt  int // the number of generations before the script was set
	describes int
	measures  []modelport.MeasureRequest
	generates []modelport.ChatRequest
}

// NewFake returns a fake model that answers every request with a short final text.
func NewFake() *Fake {
	return &Fake{reply: Final("done"), Fingerprint: FakeFingerprint}
}

// SetReply sets the answer of every following generation.
func (f *Fake) SetReply(r Reply) { f.mu.Lock(); f.reply, f.script = r, nil; f.mu.Unlock() }

// SetScript sets the answers of the following generations in order: the first
// generation after the call gets the first reply, the second the second, and the last reply answers
// every generation after that.
func (f *Fake) SetScript(r ...Reply) {
	f.mu.Lock()
	f.script, f.scriptAt = append([]Reply(nil), r...), len(f.generates)
	f.mu.Unlock()
}

// SetMeasure sets what the following counts say.
func (f *Fake) SetMeasure(m MeasureConfig) { f.mu.Lock(); f.measure = m; f.mu.Unlock() }

// Measures returns the measure requests received so far.
func (f *Fake) Measures() []modelport.MeasureRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]modelport.MeasureRequest(nil), f.measures...)
}

// Generates returns the generation requests received so far.
func (f *Fake) Generates() []modelport.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]modelport.ChatRequest(nil), f.generates...)
}

// Describes returns how many times a binding was described.
func (f *Fake) Describes() int { f.mu.Lock(); defer f.mu.Unlock(); return f.describes }

func (f *Fake) fingerprint() string {
	if f.Fingerprint != "" {
		return f.Fingerprint
	}
	return FakeFingerprint
}

// Describe publishes a descriptor with the options of one strict binding.
func (f *Fake) Describe(_ context.Context, b protocol.Binding) (modelport.BindingDescriptor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.describes++
	if f.DescribeErr != nil {
		return modelport.BindingDescriptor{}, f.DescribeErr
	}
	opts := modelport.GenerationOptions{MaxTokens: 4096, Stop: []string{}}
	return modelport.BindingDescriptor{
		Binding:            modelport.DescribedBinding{Kind: b.Kind, Selector: b.Selector, ProfileRevision: b.ProfileRevision, AgentID: b.AgentID, ExecutionRole: b.ExecutionRole},
		BindingFingerprint: f.fingerprint(),
		StageOptions:       modelport.StageOptions{Act: opts, InstructionSelection: opts, WorkSummary: opts},
		RecoveryProfiles:   f.profiles(),
		Measure:            true, Normalization: "strict-v1",
	}, nil
}

// TerminalOutputOnceRevision is the revision of the terminal_output_once profile the
// fake publishes when it offers one.
const TerminalOutputOnceRevision = "fake-tooo-1"

var formatCodes = []string{"REASONING_ONLY", "EMPTY_FINAL_CONTENT", "RAW_TOOL_MARKUP", "MODEL_OUTPUT_SCHEMA_INVALID"}

// profiles is what the descriptor publishes: same_request for the format and the
// transient codes, and terminal_output_once when it is offered. Callers hold f.mu.
func (f *Fake) profiles() []modelport.RecoveryProfile {
	if f.Profiles != nil {
		return append([]modelport.RecoveryProfile(nil), f.Profiles...)
	}
	out := []modelport.RecoveryProfile{{ProfileID: "same_request", ProfileRevision: "builtin-v1", Stages: []string{"act", "instruction_selection", "work_summary"}, Transformations: []string{},
		Codes: append(append([]string{}, formatCodes...), "CONNECT_FAILED", "UPSTREAM_TRANSIENT", "RATE_LIMITED", "QUEUE_TIMEOUT")}}
	if f.OfferTerminalOutputOnce {
		out = append(out, modelport.RecoveryProfile{ProfileID: "terminal_output_once", ProfileRevision: TerminalOutputOnceRevision, Stages: []string{"act"},
			Transformations: []string{"format_suffix"}, Codes: append([]string{}, formatCodes...)})
	}
	return out
}

// checkRecovery is what the Gateway checks of the recovery a request names: the profile
// exists for the stage at its published revision, the retry markers are both set or both
// null, terminal_output_once is only a retry's, and the trigger is a code the profile
// answers. It returns the refusing code, or "". Callers hold f.mu.
func (f *Fake) checkRecovery(h modelport.HarnessExtension) string {
	r := h.Recovery
	retry := r.RetryOfRequestID != nil
	if retry != (r.TriggerCode != nil) {
		return "INVALID_REQUEST"
	}
	for _, p := range f.profiles() {
		if p.ProfileID != r.ProfileID || p.ProfileRevision != r.ProfileRevision {
			continue
		}
		stage := false
		for _, st := range p.Stages {
			stage = stage || st == h.Stage
		}
		if !stage || (retry && h.Stage != "act") || (r.ProfileID == "terminal_output_once" && !retry) {
			return modelport.CodeUnsupportedRecovery
		}
		if retry {
			for _, c := range p.Codes {
				if c == *r.TriggerCode {
					return ""
				}
			}
			return modelport.CodeUnsupportedRecovery
		}
		return ""
	}
	return modelport.CodeUnsupportedRecovery
}

// applied is what the receipt of a generation under this profile states.
func (f *Fake) applied(profile string) []string {
	if profile != "terminal_output_once" {
		return []string{}
	}
	if f.Applied != nil {
		return append([]string{}, f.Applied...)
	}
	return []string{"format_suffix"}
}

func (f *Fake) requestDigest(inputDigest string) string {
	sum := sha256.Sum256([]byte("fake-normalized-request/" + inputDigest + "/" + f.fingerprint()))
	return hex.EncodeToString(sum[:])
}

func messageBytes(req modelport.ChatRequest) int64 {
	var n int64
	for _, m := range req.Messages {
		n += int64(len(m.Text()))
		for _, c := range m.ToolCalls {
			n += int64(len(c.Function.Arguments))
		}
	}
	return n
}

// Measure counts like the configured fake: it never generates.
func (f *Fake) Measure(ctx context.Context, req modelport.MeasureRequest) (modelport.MeasureResult, error) {
	f.mu.Lock()
	onMeasure := f.OnMeasure
	f.mu.Unlock()
	if onMeasure != nil {
		onMeasure(ctx)
	}
	if err := ctx.Err(); err != nil { // a call whose context ended gives no answer
		return modelport.MeasureResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.measures = append(f.measures, req)
	if f.MeasureErr != nil {
		return modelport.MeasureResult{}, f.MeasureErr
	}
	if bad := f.checkRecovery(req.Request.Rencrow.Harness); bad != "" {
		zero := int64(0)
		input, _ := req.Request.LogicalInputDigest()
		r := f.receipt(req.Request.Rencrow.Harness, input, f.requestDigest(input), modelport.StateNotStarted, &zero, false)
		return modelport.MeasureResult{}, &modelport.StrictError{Code: bad, Message: "refused before counting", Receipt: &r}
	}
	digest, err := req.Request.LogicalInputDigest()
	if err != nil {
		return modelport.MeasureResult{}, &modelport.StrictError{Code: modelport.CodeContractFailed, Message: "the request is not valid"}
	}
	cfg := f.measure
	state := cfg.State
	if state == "" {
		state = "verified_exact"
	}
	lower, upper := cfg.Lower, cfg.Upper
	if lower == 0 && upper == 0 {
		lower = messageBytes(req.Request)/4 + 7
		upper = lower
	}
	limit := cfg.Limit
	if limit == 0 {
		limit = 32768
	}
	res := modelport.MeasureResult{
		ContractVersion: modelport.ContractVersion, State: state, PromptLower: &lower, PromptUpper: &upper, EffectiveContextLimit: &limit,
		ReservedOutputTokens: req.Request.MaxTokens, SafetyMarginTokens: req.SafetyMarginTokens, RequestDigest: f.requestDigest(digest),
		BindingFingerprint: f.fingerprint(), EvidenceRef: modelport.Str("synthetic-fake-not-tokenizer"), InputDigest: digest,
	}
	if state == "unverified" {
		res.PromptLower, res.PromptUpper, res.EffectiveContextLimit = nil, nil, nil
	}
	if f.MeasureHook != nil {
		if err := f.MeasureHook(req, &res); err != nil {
			return modelport.MeasureResult{}, err
		}
	}
	if f.BreakBindingAfterMeasure {
		f.Fingerprint = "bfp-v1:" + strings.Repeat("e", 64)
	}
	return res, nil
}

func (f *Fake) receipt(h modelport.HarnessExtension, input, request string, state string, attempts *int64, usage bool) modelport.GatewayAttemptReceipt {
	fp := f.fingerprint()
	applied := []string{}
	if state != modelport.StateNotStarted {
		applied = f.applied(h.Recovery.ProfileID)
	}
	return modelport.GatewayAttemptReceipt{
		ContractVersion: modelport.ContractVersion, Stage: h.Stage, BindingFingerprint: &fp, RequestDigest: &request, LogicalRequests: 1,
		BackendAttempts: attempts, GenerationState: state, HiddenRetry: false, RecoveryProfile: h.Recovery.ProfileID, RecoveryProfileRevision: h.Recovery.ProfileRevision,
		AppliedTransformations: applied, UsageComplete: usage, InputDigest: &input,
	}
}

// Generate checks what the Gateway checks before generating, then does what the
// scripted reply says.
func (f *Fake) Generate(ctx context.Context, req modelport.ChatRequest) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.generates = append(f.generates, req)
	reply := f.reply
	if len(f.script) > 0 {
		reply = f.script[min(len(f.generates)-1-f.scriptAt, len(f.script)-1)]
	}
	hook := f.OnGenerate
	if reply.Dynamic != nil {
		reply = reply.Dynamic(req)
	}
	h := req.Rencrow.Harness
	input, _ := req.LogicalInputDigest()
	request := f.requestDigest(input)
	fp := f.fingerprint()
	f.mu.Unlock()

	refuse := func(code string) (io.ReadCloser, error) {
		zero := int64(0)
		r := f.receipt(h, input, request, modelport.StateNotStarted, &zero, false)
		return nil, &modelport.StrictError{Code: code, Message: "refused before generating", Receipt: &r}
	}
	f.mu.Lock()
	bad := f.checkRecovery(h)
	f.mu.Unlock()
	switch {
	case bad != "":
		return refuse(bad)
	case h.ExpectedBindingFingerprint != fp:
		return refuse(modelport.CodeBindingChanged)
	case h.ExpectedInputDigest != input:
		return refuse(modelport.CodeInputDigestMismatch)
	case h.ExpectedRequestDigest != request:
		return refuse(modelport.CodeRequestDigestMismatch)
	}
	if hook != nil {
		hook(ctx, req)
	}
	one := int64(1)
	terminal := func(outcome string, finish *string, code *string) map[string]any {
		rc := f.receipt(h, input, request, modelport.StateTerminal, &one, true)
		var rm map[string]any
		raw, _ := json.Marshal(rc)
		_ = json.Unmarshal(raw, &rm)
		t := map[string]any{
			"contract_version": modelport.ContractVersion, "outcome": outcome, "finish_reason": finish, "code": code,
			"provider_response_id": "fake-chat-1", "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 10, "cached_tokens": nil},
			"harness_receipt": rm,
		}
		if reply.Mutate != nil {
			reply.Mutate(t)
		}
		return t
	}
	var b bytes.Buffer
	chunk := func(finish *string, delta map[string]any) {
		c := map[string]any{"id": "fake-chat-1", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		raw, _ := json.Marshal(c)
		fmt.Fprintf(&b, "data: %s\n\n", raw)
	}
	term := func(t map[string]any) {
		raw, _ := json.Marshal(t)
		fmt.Fprintf(&b, "event: rencrow.terminal\ndata: %s\n\n", raw)
	}
	s := func(v string) *string { return &v }
	content := func() {
		chunks := reply.Chunks
		if chunks == nil {
			chunks = []string{reply.Text}
		}
		for i, c := range chunks {
			d := map[string]any{"content": c}
			if i == 0 {
				d["role"] = "assistant"
			}
			chunk(nil, d)
		}
	}
	switch reply.Kind {
	case KindFinal:
		content()
		chunk(s("stop"), map[string]any{})
		term(terminal("completed", s("stop"), nil))
		b.WriteString("data: [DONE]\n\n")
	case KindToolCall:
		calls := reply.Calls
		if len(calls) == 0 {
			calls = []Call{{Name: reply.Name, Args: reply.Args}}
		}
		if reply.Text != "" {
			chunk(nil, map[string]any{"role": "assistant", "content": reply.Text})
		}
		for i, c := range calls {
			id := c.ID
			if id == "" {
				id = fmt.Sprintf("fake-call-%d", i+1)
			}
			d := map[string]any{"tool_calls": []any{map[string]any{
				"index": i, "id": id, "type": "function", "function": map[string]any{"name": c.Name, "arguments": c.Args}}}}
			if i == 0 && reply.Text == "" {
				d["role"] = "assistant"
			}
			chunk(nil, d)
		}
		chunk(s("tool_calls"), map[string]any{})
		term(terminal("completed", s("tool_calls"), nil))
		b.WriteString("data: [DONE]\n\n")
	case KindLength:
		content()
		chunk(s("length"), map[string]any{})
		term(terminal("incomplete", s("length"), nil))
		b.WriteString("data: [DONE]\n\n")
	case KindRefusal:
		chunk(nil, map[string]any{"role": "assistant", "refusal": "I cannot help with that."})
		chunk(s("stop"), map[string]any{})
		term(terminal("refused", s("stop"), nil))
		b.WriteString("data: [DONE]\n\n")
	case KindReasoningOnly:
		chunk(nil, map[string]any{"role": "assistant", "reasoning_content": reply.Reasoning})
		chunk(s("stop"), map[string]any{})
		term(terminal("completed", s("stop"), nil))
		b.WriteString("data: [DONE]\n\n")
	case KindErrorOutcome:
		content()
		term(terminal("error", nil, s(reply.Code)))
		b.WriteString("data: [DONE]\n\n")
	case KindNoTerminal:
		content()
	case KindDoneOnly:
		content()
		chunk(s("stop"), map[string]any{})
		b.WriteString("data: [DONE]\n\n")
	case KindRaw:
		if reply.Stream != nil {
			b.WriteString(reply.Stream(input, request))
		} else {
			b.WriteString(reply.Raw)
		}
	case KindStrictError:
		e := &modelport.StrictError{Code: reply.Code, Message: "refused before generating"}
		if reply.Body != "" {
			e.Body, e.BodyTruncated = []byte(reply.Body), reply.BodyTruncated
		}
		if !reply.NoReceipt {
			state, attempts := modelport.StateNotStarted, int64(0)
			switch reply.State {
			case modelport.StateTerminal:
				state, attempts = modelport.StateTerminal, 1
			case modelport.StateUnknown:
				state = modelport.StateUnknown
			}
			var n *int64
			if state != modelport.StateUnknown {
				n = &attempts
			}
			r := f.receipt(h, input, request, state, n, false)
			e.Receipt = &r
		}
		return nil, e
	case KindTransportError:
		return nil, errors.New("connection reset")
	case KindHang:
		return newHang(ctx), nil
	default:
		return nil, errors.New("harnesstest: unknown reply kind")
	}
	return io.NopCloser(bytes.NewReader(b.Bytes())), nil
}

// hang is a response that yields nothing until it is closed.
type hang struct {
	once   sync.Once
	closed chan struct{}
}

func newHang(ctx context.Context) *hang {
	h := &hang{closed: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			h.Close()
		case <-h.closed:
		}
	}()
	return h
}

func (h *hang) Read([]byte) (int, error) {
	<-h.closed
	return 0, io.ErrClosedPipe
}

func (h *hang) Close() error {
	h.once.Do(func() { close(h.closed) })
	return nil
}
