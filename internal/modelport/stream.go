package modelport

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

// Limits of one strict stream (STREAM_CONTRACT section 2).
const (
	MaxFrameBytes   = 1 << 20  // one SSE event's data
	MaxFinalBytes   = 1 << 20  // the aggregated final text
	MaxToolArgBytes = 1 << 20  // all Tool arguments together
	MaxToolCalls    = 128      // calls in one response
	MaxStreamBytes  = 32 << 20 // the whole response, reasoning included
)

// Terminal kinds of a completion (INTERNAL_CONTRACTS ModelCompletion).
const (
	KindFinal      = "final"
	KindToolCalls  = "tool_calls"
	KindIncomplete = "incomplete"
	KindRefused    = "refused"
	KindError      = "error"
)

// ToolIntent is one Tool call the model asked for. It is staged data: nothing in
// this package or the kernel runs it, and it exists only when the whole stream
// ended well. ArgumentsJSON is one complete JSON object.
type ToolIntent struct {
	ProviderToolCallID string
	Ordinal            int64
	Name               string
	ArgumentsJSON      string
}

// Completion is the staged result of one strict stream (ModelCompletion). It is
// built only from the stream: whether it is acceptable for the Run (digests,
// binding, whether a Tool may be called at all) is the caller's to decide.
type Completion struct {
	// TerminalKind is final, tool_calls, incomplete, refused or error. Only final
	// carries FinalText and only tool_calls carries ToolIntents.
	TerminalKind string
	// FailureCode is the normalized code (ERROR_MAPPING) of every kind but final and
	// tool_calls.
	FailureCode string
	FinalText   string
	// ContentText is the visible text that came with Tool calls (the assistant message's
	// content), empty when there was none. It is never an answer.
	ContentText string
	ToolIntents []ToolIntent

	// GenerationState and BackendAttempts are what the stream established: the
	// receipt's, or unknown when no valid terminal receipt was observed. Reaching
	// the end of the stream or an error on it never proves the generation ended.
	GenerationState string
	BackendAttempts *int64
	Receipt         *GatewayAttemptReceipt
	Terminal        *StreamTerminal
	Usage           *Usage

	ProviderResponseID string
	FinishReason       string
	TerminalObserved   bool
	DoneObserved       bool
	// HiddenRetry is set when the terminal receipt admitted a retry the contract
	// forbids.
	HiddenRetry bool
	// Violation says, without any model text, what in the stream broke the
	// contract. Empty when the stream conformed.
	Violation string
	// ReasoningBytes and ContentBytes size what was received; the reasoning itself
	// is never kept or shown.
	ReasoningBytes int
	ContentBytes   int
}

// AssembleOptions configure AssembleStrictStream.
type AssembleOptions struct {
	// MaxToolCalls is the most Tool calls one response may carry: the Run's
	// max_tool_calls_per_step, at most 128. Zero means 128.
	MaxToolCalls int
	// OnContent receives each content fragment as provisional text, with its
	// ordinal (0, 1, ...). Reasoning never reaches it.
	OnContent func(ordinal int64, text string)
}

var chunkKeys = map[string]bool{
	"id": true, "object": true, "created": true, "model": true, "system_fingerprint": true, "choices": true, "usage": true,
}

// AssembleStrictStream (F40) reads one strict SSE response to its end and returns
// the staged completion. The stream is strict: UTF-8 only, data frames of at most
// 1 MiB, one choice, one response id, Tool calls numbered from 0 with their header
// in the first fragment, one rencrow.terminal frame followed by [DONE].
//
// It never executes anything. The arguments of a Tool call are joined and checked
// only once the stream ended, and only a completion that ended with finish_reason
// tool_calls and a valid terminal receipt carries ToolIntents; a stream that ended
// by length, incomplete, refusal, error, EOF or any violation carries none, however
// valid the arguments it received look.
//
// The generation state is the terminal receipt's. A stream that ends before the
// receipt (EOF, a read error, a violation, [DONE] alone) is unknown: the end of the
// connection does not show that the generation stopped.
func AssembleStrictStream(r io.Reader, opts AssembleOptions) Completion {
	a := &assembler{opts: opts, maxCalls: MaxToolCalls}
	if opts.MaxToolCalls > 0 && opts.MaxToolCalls < MaxToolCalls {
		a.maxCalls = opts.MaxToolCalls
	}
	a.read(r)
	return a.completion()
}

type callAcc struct {
	id, name string
	args     strings.Builder
}

type assembler struct {
	opts     AssembleOptions
	maxCalls int

	violation string
	done      bool
	terminal  *StreamTerminal
	hidden    bool

	respID       string
	rolesSeen    bool
	content      strings.Builder
	reasoningLen int
	refusal      strings.Builder
	calls        []*callAcc
	argBytes     int
	finish       *string
	chunkUsage   *Usage
	ordinal      int64
}

func (a *assembler) fail(reason string) {
	if a.violation == "" {
		a.violation = reason
	}
}

// read consumes the stream line by line until the end, [DONE] or a violation.
func (a *assembler) read(r io.Reader) {
	counted := &countingReader{r: r, limit: MaxStreamBytes}
	br := bufio.NewReaderSize(counted, 64<<10)
	var evType string
	var data []string
	dataSize := 0
	hasEvent := false
	for a.violation == "" && !a.done {
		line, err := readLine(br, MaxFrameBytes+len("data: ")+2)
		if counted.over {
			a.fail("the response exceeds the size limit")
			return
		}
		if errors.Is(err, errLineTooLong) {
			a.fail("a line exceeds the frame size limit")
			return
		}
		if err != nil {
			return // EOF or a read error: whatever is pending is an incomplete event
		}
		if !utf8.Valid(line) {
			a.fail("the stream is not valid UTF-8")
			return
		}
		if len(line) == 0 {
			if hasEvent {
				a.dispatch(evType, strings.Join(data, "\n"), len(data) > 0)
			}
			evType, data, dataSize, hasEvent = "", nil, 0, false
			continue
		}
		if line[0] == ':' {
			continue // comment
		}
		field, value, _ := strings.Cut(string(line), ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			dataSize += len(value) + 1
			if dataSize > MaxFrameBytes+1 {
				a.fail("a data frame exceeds the frame size limit")
				return
			}
			data = append(data, value)
			hasEvent = true
		case "event":
			evType = value
			hasEvent = true
		default:
			a.fail("the stream has a field that is not data or event")
			return
		}
	}
}

var errLineTooLong = errors.New("line too long")

// readLine returns one line without its LF (and CR). A final line without a line
// ending is an incomplete event and is reported as io.EOF.
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > limit {
			return nil, errLineTooLong
		}
		switch {
		case err == nil:
			buf = bytes.TrimSuffix(buf[:len(buf)-1], []byte{'\r'})
			return buf, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}

type countingReader struct {
	r     io.Reader
	n     int64
	limit int64
	over  bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.limit {
		c.over = true
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (a *assembler) dispatch(evType, data string, hasData bool) {
	if !hasData {
		a.fail("an event has no data")
		return
	}
	switch evType {
	case "", "message":
		if data == "[DONE]" {
			a.done = true
			return
		}
		if a.terminal != nil {
			a.fail("a frame follows the terminal frame")
			return
		}
		a.chunk(data)
	case "rencrow.terminal":
		a.onTerminal(data)
	default:
		a.fail("an event of an unknown type")
	}
}

func asObject(v any) (map[string]any, bool) { m, ok := v.(map[string]any); return m, ok }

func (a *assembler) chunk(data string) {
	v, err := strictjson.Decode([]byte(data))
	if err != nil {
		a.fail("a chunk is not strict JSON")
		return
	}
	obj, ok := asObject(v)
	if !ok {
		a.fail("a chunk is not an object")
		return
	}
	for k := range obj {
		if !chunkKeys[k] {
			a.fail("a chunk has an undeclared field")
			return
		}
	}
	if id, present := obj["id"]; present {
		s, isStr := id.(string)
		switch {
		case !isStr || s == "":
			a.fail("a chunk id is not a non-empty string")
			return
		case a.respID == "":
			a.respID = s
		case a.respID != s:
			a.fail("the response id changed within the stream")
			return
		}
	}
	if u, present := obj["usage"]; present && u != nil {
		usage, ok := parseUsage(u)
		if !ok {
			a.fail("a chunk usage is not valid")
			return
		}
		a.chunkUsage = usage
	}
	choices, ok := obj["choices"].([]any)
	if !ok {
		a.fail("a chunk has no choices array")
		return
	}
	if len(choices) == 0 {
		if obj["usage"] == nil {
			a.fail("a chunk without a choice is not a usage chunk")
		}
		return
	}
	if len(choices) != 1 {
		a.fail("a chunk has more than one choice")
		return
	}
	a.choice(choices[0])
}

func parseUsage(v any) (*Usage, bool) {
	m, ok := asObject(v)
	if !ok {
		return nil, false
	}
	u := &Usage{}
	for k, p := range map[string]**int64{"prompt_tokens": &u.PromptTokens, "completion_tokens": &u.CompletionTokens, "cached_tokens": &u.CachedTokens} {
		if x, present := m[k]; present && x != nil {
			n, ok := x.(json.Number)
			if !ok {
				return nil, false
			}
			i, err := n.Int64()
			if err != nil || i < 0 {
				return nil, false
			}
			*p = &i
		}
	}
	return u, true
}

func (a *assembler) choice(v any) {
	ch, ok := asObject(v)
	if !ok {
		a.fail("a choice is not an object")
		return
	}
	for k := range ch {
		switch k {
		case "index", "delta", "finish_reason", "logprobs":
		default:
			a.fail("a choice has an undeclared field")
			return
		}
	}
	if idx, ok := ch["index"].(json.Number); !ok || idx.String() != "0" {
		a.fail("a choice index is not 0")
		return
	}
	delta, _ := asObject(ch["delta"])
	if _, present := ch["delta"]; present && delta == nil {
		a.fail("a choice delta is not an object")
		return
	}
	var finish *string
	if f, present := ch["finish_reason"]; present && f != nil {
		s, ok := f.(string)
		if !ok || s == "" {
			a.fail("a finish_reason is not a string")
			return
		}
		finish = &s
	}
	if a.finish != nil && (finish != nil || len(delta) != 0) {
		a.fail("content follows the finish_reason")
		return
	}
	a.applyDelta(delta)
	if a.violation != "" {
		return
	}
	if finish != nil {
		a.finish = finish
	}
}

func (a *assembler) applyDelta(delta map[string]any) {
	for k, v := range delta {
		switch k {
		case "role":
			if a.rolesSeen || v != "assistant" {
				a.fail("a delta role is repeated or not assistant")
				return
			}
		case "content":
			if v == nil {
				continue
			}
			s, ok := v.(string)
			if !ok {
				a.fail("a delta content is not a string")
				return
			}
			if a.content.Len()+len(s) > MaxFinalBytes {
				a.fail("the final text exceeds its size limit")
				return
			}
			a.content.WriteString(s)
			if s != "" && a.opts.OnContent != nil {
				a.opts.OnContent(a.ordinal, s)
				a.ordinal++
			}
		case "reasoning_content":
			if v == nil {
				continue
			}
			s, ok := v.(string)
			if !ok {
				a.fail("a delta reasoning_content is not a string")
				return
			}
			a.reasoningLen += len(s) // counted, never kept or shown
		case "refusal":
			if v == nil {
				continue
			}
			s, ok := v.(string)
			if !ok {
				a.fail("a delta refusal is not a string")
				return
			}
			if a.refusal.Len()+len(s) > MaxFinalBytes {
				a.fail("a refusal exceeds its size limit")
				return
			}
			a.refusal.WriteString(s)
		case "tool_calls":
			list, ok := v.([]any)
			if !ok {
				a.fail("a delta tool_calls is not an array")
				return
			}
			for _, f := range list {
				a.toolFragment(f)
				if a.violation != "" {
					return
				}
			}
		default:
			a.fail("a delta has an undeclared field")
			return
		}
	}
	if _, has := delta["role"]; has {
		a.rolesSeen = true
	}
}

func (a *assembler) toolFragment(v any) {
	f, ok := asObject(v)
	if !ok {
		a.fail("a tool call fragment is not an object")
		return
	}
	for k := range f {
		switch k {
		case "index", "id", "type", "function":
		default:
			a.fail("a tool call fragment has an undeclared field")
			return
		}
	}
	num, ok := f["index"].(json.Number)
	if !ok {
		a.fail("a tool call fragment has no index")
		return
	}
	idx, err := num.Int64()
	if err != nil || idx < 0 || idx > int64(len(a.calls)) {
		// A new call is the next index; an earlier call than the current one is
		// closed once a later one started.
		a.fail("tool call indexes are not consecutive")
		return
	}
	var fn map[string]any
	if raw, present := f["function"]; present {
		if fn, ok = asObject(raw); !ok {
			a.fail("a tool call function is not an object")
			return
		}
		for k := range fn {
			if k != "name" && k != "arguments" {
				a.fail("a tool call function has an undeclared field")
				return
			}
		}
	}
	id, hasID := f["id"].(string)
	typ, hasType := f["type"].(string)
	name, hasName := fn["name"].(string)
	if _, present := f["id"]; present && !hasID {
		a.fail("a tool call id is not a string")
		return
	}
	if _, present := f["type"]; present && !hasType {
		a.fail("a tool call type is not a string")
		return
	}
	if _, present := fn["name"]; present && !hasName {
		a.fail("a tool call name is not a string")
		return
	}
	if int(idx) == len(a.calls) {
		// The header (id, type function, name) is complete in the first fragment.
		if !hasID || id == "" || !hasType || typ != "function" || !hasName || name == "" {
			a.fail("a new tool call does not start with a complete header")
			return
		}
		if len(a.calls) >= a.maxCalls {
			a.fail("the response has more tool calls than allowed")
			return
		}
		for _, c := range a.calls {
			if c.id == id {
				a.fail("a tool call id is repeated")
				return
			}
		}
		a.calls = append(a.calls, &callAcc{id: id, name: name})
	} else {
		if int(idx) != len(a.calls)-1 {
			a.fail("a closed tool call received more fragments")
			return
		}
		c := a.calls[idx]
		// A repeated header must be identical; a name is never concatenated.
		if (hasID && id != c.id) || (hasType && typ != "function") || (hasName && name != c.name) {
			a.fail("a tool call header changed")
			return
		}
	}
	if raw, present := fn["arguments"]; present {
		s, ok := raw.(string)
		if !ok {
			a.fail("tool call arguments are not a string")
			return
		}
		if a.argBytes+len(s) > MaxToolArgBytes {
			a.fail("the tool arguments exceed their size limit")
			return
		}
		a.argBytes += len(s)
		a.calls[idx].args.WriteString(s)
	}
}

func (a *assembler) onTerminal(data string) {
	if a.terminal != nil {
		a.fail("the terminal frame is repeated")
		return
	}
	v, err := strictjson.Decode([]byte(data))
	if err != nil {
		a.fail("the terminal frame is not strict JSON")
		return
	}
	if obj, ok := asObject(v); ok {
		if rec, ok := asObject(obj["harness_receipt"]); ok && rec["hidden_retry"] == true {
			a.hidden = true
			a.fail("the receipt admits a hidden retry")
			return
		}
	}
	if err := schemacheck.Validate(schemacheck.LLMContract, "StreamTerminal", v); err != nil {
		a.fail("the terminal frame does not satisfy the contract")
		return
	}
	var t StreamTerminal
	if err := json.Unmarshal([]byte(data), &t); err != nil {
		a.fail("the terminal frame cannot be read")
		return
	}
	a.terminal = &t
}

func parseArguments(s string) bool {
	v, err := strictjson.Decode([]byte(s))
	if err != nil {
		return false
	}
	_, ok := v.(map[string]any)
	return ok
}

// completion decides what the stream amounts to.
func (a *assembler) completion() Completion {
	c := Completion{
		GenerationState: StateUnknown, DoneObserved: a.done, TerminalObserved: a.terminal != nil,
		ProviderResponseID: a.respID, ReasoningBytes: a.reasoningLen, ContentBytes: a.content.Len(), HiddenRetry: a.hidden,
	}
	if a.terminal != nil {
		t := a.terminal
		c.Terminal, c.Receipt, c.Usage = t, &t.HarnessReceipt, t.Usage
		c.GenerationState, c.BackendAttempts = t.HarnessReceipt.GenerationState, t.HarnessReceipt.BackendAttempts
		if t.FinishReason != nil {
			c.FinishReason = *t.FinishReason
		}
	} else if a.chunkUsage != nil {
		c.Usage = a.chunkUsage
	}
	contractFailure := func(why string) Completion {
		c.TerminalKind, c.FailureCode, c.Violation = KindError, CodeContractFailed, why
		return c
	}
	switch {
	case a.violation != "":
		return contractFailure(a.violation)
	case a.terminal == nil && a.done:
		return contractFailure("the stream ended without the terminal frame")
	case a.terminal == nil:
		// EOF or a read error before the end: the generation may still be running.
		c.TerminalKind, c.FailureCode = KindError, CodeOutcomeUnknown
		c.Violation = "the stream ended before the terminal frame"
		return c
	case !a.done:
		// The receipt is there but [DONE] is not: the output as a whole is not adopted.
		c.TerminalKind, c.FailureCode = KindIncomplete, CodeIncomplete
		c.Violation = "the stream ended before [DONE]"
		return c
	}

	t := a.terminal
	if t.ProviderResponseID != nil && a.respID != "" && *t.ProviderResponseID != a.respID {
		return contractFailure("the terminal response id differs from the stream's")
	}
	switch t.Outcome {
	case "completed":
		return a.completed(c, contractFailure)
	case "incomplete":
		c.TerminalKind, c.FailureCode = KindIncomplete, CodeIncomplete
		if t.FinishReason != nil && *t.FinishReason == "length" {
			c.FailureCode = CodeLength
		}
		return c
	case "refused":
		c.TerminalKind, c.FailureCode = KindRefused, CodeRefused
		return c
	case "error":
		c.TerminalKind, c.FailureCode = KindError, *t.Code
		return c
	}
	return contractFailure("the terminal outcome is unknown")
}

func (a *assembler) completed(c Completion, contractFailure func(string) Completion) Completion {
	t := a.terminal
	if t.HarnessReceipt.BackendAttempts == nil || *t.HarnessReceipt.BackendAttempts != 1 {
		return contractFailure("a completed receipt does not show one backend attempt")
	}
	if a.finish == nil || t.FinishReason == nil || *a.finish != *t.FinishReason {
		return contractFailure("the stream's finish_reason differs from the terminal's")
	}
	if a.refusal.Len() > 0 {
		return contractFailure("a refusal came with a completed outcome")
	}
	switch *t.FinishReason {
	case "stop":
		if len(a.calls) != 0 {
			return contractFailure("tool calls came with finish_reason stop")
		}
		text := a.content.String()
		if strings.TrimSpace(text) == "" {
			c.TerminalKind = KindError
			c.FailureCode = CodeEmptyFinalContent
			if a.reasoningLen > 0 {
				c.FailureCode = CodeReasoningOnly
			}
			return c
		}
		c.TerminalKind, c.FinalText = KindFinal, text
		return c
	case "tool_calls":
		if len(a.calls) == 0 {
			return contractFailure("finish_reason tool_calls without a tool call")
		}
		intents := make([]ToolIntent, 0, len(a.calls))
		for i, call := range a.calls {
			args := call.args.String()
			if !parseArguments(args) {
				c.TerminalKind, c.FailureCode = KindError, CodeOutputSchemaInvalid
				return c
			}
			intents = append(intents, ToolIntent{ProviderToolCallID: call.id, Ordinal: int64(i), Name: call.name, ArgumentsJSON: args})
		}
		c.TerminalKind, c.ToolIntents, c.ContentText = KindToolCalls, intents, a.content.String()
		return c
	}
	return contractFailure("a completed outcome with an unexpected finish_reason")
}
