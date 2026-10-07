package modelport_test

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

func wire(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(harnesstest.ModuleRoot(t), "testdata", "contract", "examples", "wire", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// terminalOf returns the fixture's terminal frame edited by f, as one SSE event.
func terminalOf(t testing.TB, f func(m map[string]any)) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(wire(t, "stream_terminal.json"), &m); err != nil {
		t.Fatal(err)
	}
	if f != nil {
		f(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return "event: rencrow.terminal\ndata: " + string(b) + "\n\n"
}

func receipt(m map[string]any) map[string]any { return m["harness_receipt"].(map[string]any) }

func chunk(finish string, delta string) string {
	f := "null"
	if finish != "" {
		f = `"` + finish + `"`
	}
	return `data: {"choices":[{"delta":` + delta + `,"finish_reason":` + f + `,"index":0}],"id":"fixture-chat-1"}` + "\n\n"
}

const done = "data: [DONE]\n\n"

func toolStream(t testing.TB, finish string, terminal string, args ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(chunk("", `{"role":"assistant","tool_calls":[{"function":{"arguments":`+jstr(args[0])+`,"name":"file.read"},"id":"fixture-read-1","index":0,"type":"function"}]}`))
	for _, a := range args[1:] {
		b.WriteString(chunk("", `{"tool_calls":[{"function":{"arguments":`+jstr(a)+`},"index":0}]}`))
	}
	b.WriteString(chunk(finish, `{}`))
	b.WriteString(terminal)
	b.WriteString(done)
	return b.String()
}

func jstr(s string) string { b, _ := json.Marshal(s); return string(b) }

func assemble(s string) modelport.Completion {
	return modelport.AssembleStrictStream(strings.NewReader(s), modelport.AssembleOptions{})
}

func finalStream(t testing.TB, text string) string {
	t.Helper()
	term := terminalOf(t, func(m map[string]any) { m["finish_reason"] = "stop" })
	return chunk("", `{"role":"assistant","content":`+jstr(text)+`}`) + chunk("stop", `{}`) + term + done
}

func TestFixtureToolStreamIsAssembledAndStaged(t *testing.T) {
	c := modelport.AssembleStrictStream(strings.NewReader(string(wire(t, "act_tool_stream.sse"))), modelport.AssembleOptions{})
	if c.TerminalKind != modelport.KindToolCalls || c.FailureCode != "" || c.Violation != "" {
		t.Fatalf("%+v", c)
	}
	if len(c.ToolIntents) != 1 {
		t.Fatalf("intents: %+v", c.ToolIntents)
	}
	in := c.ToolIntents[0]
	if in.ProviderToolCallID != "fixture-read-1" || in.Ordinal != 0 || in.Name != "file.read" || in.ArgumentsJSON != `{"path":"demo.txt"}` {
		t.Fatalf("intent: %+v", in)
	}
	if c.GenerationState != modelport.StateTerminal || c.BackendAttempts == nil || *c.BackendAttempts != 1 ||
		!c.TerminalObserved || !c.DoneObserved || c.ProviderResponseID != "fixture-chat-1" || c.FinishReason != "tool_calls" {
		t.Fatalf("state: %+v", c)
	}
	if c.Usage == nil || *c.Usage.PromptTokens != 8000 || c.Receipt == nil || c.Receipt.HiddenRetry {
		t.Fatalf("usage or receipt: %+v", c)
	}
}

func TestFinalTextStreamAndProvisionalDeltas(t *testing.T) {
	var got []string
	var ords []int64
	s := chunk("", `{"role":"assistant","reasoning_content":"private thoughts"}`) +
		chunk("", `{"content":"Hello, "}`) + chunk("", `{"content":null}`) + chunk("", `{"content":"world\n"}`) + chunk("stop", `{}`) +
		terminalOf(t, func(m map[string]any) { m["finish_reason"] = "stop" }) + done
	c := modelport.AssembleStrictStream(strings.NewReader(s), modelport.AssembleOptions{OnContent: func(o int64, text string) {
		ords, got = append(ords, o), append(got, text)
	}})
	if c.TerminalKind != modelport.KindFinal || c.FinalText != "Hello, world\n" || len(c.ToolIntents) != 0 || c.FailureCode != "" {
		t.Fatalf("%+v", c)
	}
	if strings.Join(got, "|") != "Hello, |world\n" || len(ords) != 2 || ords[0] != 0 || ords[1] != 1 {
		t.Fatalf("deltas %q %v", got, ords)
	}
	for _, g := range got {
		if strings.Contains(g, "private") {
			t.Fatal("reasoning reached a delta")
		}
	}
	if c.ReasoningBytes != len("private thoughts") {
		t.Fatalf("reasoning bytes %d", c.ReasoningBytes)
	}
}

func TestSSEFramingDetails(t *testing.T) {
	// CRLF line ends, comments, a data frame split over two data lines, and an id
	// field that is not part of the contract.
	term := strings.ReplaceAll(terminalOf(t, func(m map[string]any) { m["finish_reason"] = "stop" }), "\n", "\r\n")
	s := ": keep-alive\r\n\r\n" +
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\r\ndata: \"content\":\"hi\"},\"finish_reason\":null,\"index\":0}],\"id\":\"fixture-chat-1\"}\r\n\r\n" +
		strings.ReplaceAll(chunk("stop", `{}`), "\n", "\r\n") + term + "data: [DONE]\r\n\r\n"
	// The two data lines are joined with LF, which is a valid JSON space.
	c := assemble(s)
	if c.TerminalKind != modelport.KindFinal || c.FinalText != "hi" {
		t.Fatalf("%+v", c)
	}
	c = assemble("id: 5\n\n" + finalStream(t, "x"))
	if c.TerminalKind != modelport.KindError || c.Violation == "" {
		t.Fatalf("an id field is not part of the contract: %+v", c)
	}
}

type negative struct {
	name     string
	stream   func(t *testing.T) string
	kind     string
	code     string
	state    string // generation state
	noIntent bool
}

func TestNegativeStreams(t *testing.T) {
	longArgs := `{"path":"demo.txt"}`
	cases := []negative{
		{"EOF before the terminal is unknown", func(t *testing.T) string {
			return chunk("", `{"role":"assistant","content":"partial"}`)
		}, modelport.KindError, modelport.CodeOutcomeUnknown, modelport.StateUnknown, true},
		{"EOF after the terminal and before [DONE] is incomplete, state kept", func(t *testing.T) string {
			return chunk("", `{"role":"assistant","content":"x"}`) + chunk("stop", `{}`) + terminalOf(t, func(m map[string]any) { m["finish_reason"] = "stop" })
		}, modelport.KindIncomplete, modelport.CodeIncomplete, modelport.StateTerminal, true},
		{"[DONE] without the terminal frame", func(t *testing.T) string {
			return chunk("", `{"role":"assistant","content":"x"}`) + chunk("stop", `{}`) + done
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateUnknown, true},
		{"length keeps valid-looking arguments away from the intents", func(t *testing.T) string {
			term := terminalOf(t, func(m map[string]any) {
				m["outcome"], m["finish_reason"] = "incomplete", "length"
			})
			return toolStream(t, "length", term, longArgs)
		}, modelport.KindIncomplete, modelport.CodeLength, modelport.StateTerminal, true},
		{"incomplete outcome", func(t *testing.T) string {
			term := terminalOf(t, func(m map[string]any) { m["outcome"], m["finish_reason"] = "incomplete", "incomplete" })
			return toolStream(t, "", term, longArgs)
		}, modelport.KindIncomplete, modelport.CodeIncomplete, modelport.StateTerminal, true},
		{"the terminal frame twice", func(t *testing.T) string {
			return finalStream(t, "x")[:len(finalStream(t, "x"))-len(done)] + terminalOf(t, func(m map[string]any) { m["finish_reason"] = "stop" }) + done
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateTerminal, true},
		{"a chunk after the terminal frame", func(t *testing.T) string {
			s := finalStream(t, "x")
			return s[:len(s)-len(done)] + chunk("", `{"content":"late"}`) + done
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateTerminal, true},
		{"tool call indexes skip", func(t *testing.T) string {
			return chunk("", `{"role":"assistant","tool_calls":[{"function":{"arguments":"{}","name":"file.read"},"id":"c1","index":1,"type":"function"}]}`) +
				chunk("tool_calls", `{}`) + terminalOf(t, nil) + done
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateUnknown, true},
		{"a later fragment changes the id", func(t *testing.T) string {
			return chunk("", `{"role":"assistant","tool_calls":[{"function":{"arguments":"{","name":"file.read"},"id":"c1","index":0,"type":"function"}]}`) +
				chunk("", `{"tool_calls":[{"function":{"arguments":"}"},"id":"c2","index":0}]}`) +
				chunk("tool_calls", `{}`) + terminalOf(t, nil) + done
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateUnknown, true},
		{"a new call without its header", func(t *testing.T) string {
			return chunk("", `{"role":"assistant","tool_calls":[{"function":{"arguments":"{}"},"index":0}]}`) +
				chunk("tool_calls", `{}`) + terminalOf(t, nil) + done
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateUnknown, true},
		{"a closed call receives more fragments", func(t *testing.T) string {
			return chunk("", `{"role":"assistant","tool_calls":[{"function":{"arguments":"{}","name":"a"},"id":"c1","index":0,"type":"function"},{"function":{"arguments":"{}","name":"b"},"id":"c2","index":1,"type":"function"}]}`) +
				chunk("", `{"tool_calls":[{"function":{"arguments":" "},"index":0}]}`) + chunk("tool_calls", `{}`) + terminalOf(t, nil) + done
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateUnknown, true},
		{"the response id changes", func(t *testing.T) string {
			return chunk("", `{"role":"assistant","content":"a"}`) +
				`data: {"choices":[{"delta":{"content":"b"},"finish_reason":null,"index":0}],"id":"other"}` + "\n\n" + done
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateUnknown, true},
		{"two choices", func(t *testing.T) string {
			return `data: {"choices":[{"delta":{"content":"a"},"finish_reason":null,"index":0},{"delta":{"content":"b"},"finish_reason":null,"index":1}],"id":"x"}` + "\n\n"
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateUnknown, true},
		{"invalid UTF-8", func(t *testing.T) string {
			return "data: {\"choices\":[{\"delta\":{\"content\":\"\xff\"},\"finish_reason\":null,\"index\":0}],\"id\":\"x\"}\n\n"
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateUnknown, true},
		{"a duplicate JSON key", func(t *testing.T) string {
			return `data: {"choices":[{"delta":{"content":"a","content":"b"},"finish_reason":null,"index":0}],"id":"x"}` + "\n\n"
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateUnknown, true},
		{"a frame over the limit", func(t *testing.T) string {
			return chunk("", `{"role":"assistant","content":"`+strings.Repeat("a", modelport.MaxFrameBytes)+`"}`)
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateUnknown, true},
		{"the final text over the limit", func(t *testing.T) string {
			half := strings.Repeat("a", modelport.MaxFinalBytes/2+1)
			return chunk("", `{"role":"assistant","content":"`+half+`"}`) + chunk("", `{"content":"`+half+`"}`)
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateUnknown, true},
		{"finish_reason differs from the terminal's", func(t *testing.T) string {
			return toolStream(t, "stop", terminalOf(t, nil), `{}`)
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateTerminal, true},
		{"arguments that are not one JSON object", func(t *testing.T) string {
			return toolStream(t, "tool_calls", terminalOf(t, nil), `{"path":`)
		}, modelport.KindError, modelport.CodeOutputSchemaInvalid, modelport.StateTerminal, true},
		{"a refusal beside a completed outcome", func(t *testing.T) string {
			term := terminalOf(t, func(m map[string]any) { m["finish_reason"] = "stop" })
			return chunk("", `{"role":"assistant","content":"ok","refusal":"no"}`) + chunk("stop", `{}`) + term + done
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateTerminal, true},
		{"reasoning only", func(t *testing.T) string {
			term := terminalOf(t, func(m map[string]any) { m["finish_reason"] = "stop" })
			return chunk("", `{"role":"assistant","reasoning_content":"thinking"}`) + chunk("stop", `{}`) + term + done
		}, modelport.KindError, modelport.CodeReasoningOnly, modelport.StateTerminal, true},
		{"an empty final", func(t *testing.T) string {
			term := terminalOf(t, func(m map[string]any) { m["finish_reason"] = "stop" })
			return chunk("", `{"role":"assistant","content":" \n"}`) + chunk("stop", `{}`) + term + done
		}, modelport.KindError, modelport.CodeEmptyFinalContent, modelport.StateTerminal, true},
		{"tool calls beside finish_reason stop", func(t *testing.T) string {
			return toolStream(t, "stop", terminalOf(t, func(m map[string]any) { m["finish_reason"] = "stop" }), `{}`)
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateTerminal, true},
		{"a refusal", func(t *testing.T) string {
			term := terminalOf(t, func(m map[string]any) { m["outcome"], m["finish_reason"] = "refused", "stop" })
			return chunk("", `{"role":"assistant","refusal":"I cannot"}`) + chunk("stop", `{}`) + term + done
		}, modelport.KindRefused, modelport.CodeRefused, modelport.StateTerminal, true},
		{"an error outcome keeps its code and state", func(t *testing.T) string {
			term := terminalOf(t, func(m map[string]any) {
				m["outcome"], m["code"], m["finish_reason"] = "error", "RAW_TOOL_MARKUP", nil
			})
			return chunk("", `{"role":"assistant","content":"<tool_call>"}`) + term + done
		}, modelport.KindError, "RAW_TOOL_MARKUP", modelport.StateTerminal, true},
		{"a hidden retry in the receipt", func(t *testing.T) string {
			term := terminalOf(t, func(m map[string]any) { receipt(m)["hidden_retry"] = true })
			return toolStream(t, "tool_calls", term, `{}`)
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateUnknown, true},
		{"a terminal frame that breaks its schema", func(t *testing.T) string {
			term := terminalOf(t, func(m map[string]any) { delete(m, "usage") })
			return toolStream(t, "tool_calls", term, `{}`)
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateUnknown, true},
		{"a terminal that contradicts the stream's response id", func(t *testing.T) string {
			term := terminalOf(t, func(m map[string]any) { m["provider_response_id"] = "elsewhere" })
			return toolStream(t, "tool_calls", term, `{}`)
		}, modelport.KindError, modelport.CodeContractFailed, modelport.StateTerminal, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assemble(tc.stream(t))
			if c.TerminalKind != tc.kind || c.FailureCode != tc.code || c.GenerationState != tc.state {
				t.Fatalf("kind=%s code=%s state=%s violation=%q; want %s %s %s", c.TerminalKind, c.FailureCode, c.GenerationState, c.Violation, tc.kind, tc.code, tc.state)
			}
			if tc.noIntent && len(c.ToolIntents) != 0 {
				t.Fatalf("a failed stream staged Tool intents: %+v", c.ToolIntents)
			}
			if c.FinalText != "" {
				t.Fatalf("a failed stream carries final text %q", c.FinalText)
			}
			if (c.GenerationState == modelport.StateUnknown) != (c.BackendAttempts == nil) {
				t.Fatalf("backend_attempts %v does not match state %s", c.BackendAttempts, c.GenerationState)
			}
		})
	}
}

func TestHiddenRetryIsFlagged(t *testing.T) {
	term := terminalOf(t, func(m map[string]any) { receipt(m)["hidden_retry"] = true })
	if c := assemble(toolStream(t, "tool_calls", term, `{}`)); !c.HiddenRetry {
		t.Fatalf("%+v", c)
	}
}

func TestReadErrorBeforeTheTerminalIsUnknown(t *testing.T) {
	r := io.MultiReader(strings.NewReader(chunk("", `{"role":"assistant","content":"x"}`)), iotest.ErrReader(errors.New("connection reset")))
	c := modelport.AssembleStrictStream(r, modelport.AssembleOptions{})
	if c.TerminalKind != modelport.KindError || c.GenerationState != modelport.StateUnknown || c.FailureCode != modelport.CodeOutcomeUnknown {
		t.Fatalf("%+v", c)
	}
}

func TestToolCallLimitFollowsTheRunLimit(t *testing.T) {
	s := chunk("", `{"role":"assistant","tool_calls":[{"function":{"arguments":"{}","name":"a"},"id":"c1","index":0,"type":"function"},{"function":{"arguments":"{}","name":"a"},"id":"c2","index":1,"type":"function"}]}`) +
		chunk("tool_calls", `{}`) + terminalOf(t, nil) + done
	if c := modelport.AssembleStrictStream(strings.NewReader(s), modelport.AssembleOptions{MaxToolCalls: 2}); c.TerminalKind != modelport.KindToolCalls || len(c.ToolIntents) != 2 {
		t.Fatalf("%+v", c)
	}
	if c := modelport.AssembleStrictStream(strings.NewReader(s), modelport.AssembleOptions{MaxToolCalls: 1}); c.TerminalKind != modelport.KindError || len(c.ToolIntents) != 0 {
		t.Fatalf("%+v", c)
	}
}

func TestToolCallsAreOrdinalOrdered(t *testing.T) {
	s2 := chunk("", `{"role":"assistant","tool_calls":[{"function":{"arguments":"{\"a\":","name":"x"},"id":"c1","index":0,"type":"function"}]}`) +
		chunk("", `{"tool_calls":[{"function":{"arguments":"1}","name":"x"},"index":0,"id":"c1","type":"function"},{"function":{"arguments":"{}","name":"y"},"id":"c2","index":1,"type":"function"}]}`) +
		chunk("tool_calls", `{}`) + terminalOf(t, nil) + done
	c := assemble(s2)
	if c.TerminalKind != modelport.KindToolCalls || len(c.ToolIntents) != 2 || c.ToolIntents[0].ArgumentsJSON != `{"a":1}` ||
		c.ToolIntents[1].Ordinal != 1 || c.ToolIntents[1].Name != "y" {
		t.Fatalf("%+v", c)
	}
}
