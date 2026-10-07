package contextplan_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

func record(t *testing.T, content *string, ids ...string) string {
	t.Helper()
	var calls []modelport.ToolCall
	for _, id := range ids {
		calls = append(calls, modelport.ToolCall{ID: id, Type: "function", Function: modelport.ToolFunction{Name: "file.read", Arguments: `{"path":"a"}`}})
	}
	raw, err := contextplan.EncodeToolCalls(contextplan.ToolCallsRecord{Content: content, ToolCalls: calls})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func toolItems(t *testing.T, content *string, ids ...string) []contextplan.HistoryItem {
	t.Helper()
	items := []contextplan.HistoryItem{
		{ContextSeq: 1, MessageID: "m1", HistoryKind: contextplan.HistoryAutomation, Origin: "automation", Text: "go"},
		{ContextSeq: 2, MessageID: "m2", HistoryKind: contextplan.HistoryWork, Origin: "agent", ItemKind: contextplan.ItemKindToolCalls, Text: record(t, content, ids...)},
	}
	for i, id := range ids {
		items = append(items, contextplan.HistoryItem{ContextSeq: int64(3 + i), MessageID: "r" + id, HistoryKind: contextplan.HistoryObservation, Origin: "tool",
			ItemKind: contextplan.ItemKindToolResult, ToolCallID: id, Text: `{"answer":"` + id + `"}`})
	}
	return items
}

func TestAToolExchangeIsOneAssistantMessageAndItsAnswersInTheOrderOfTheCalls(t *testing.T) {
	text := "looking"
	plan, err := contextplan.AssembleContext(contextplan.Input{SystemPrompt: "sys", History: toolItems(t, &text, "c1", "c2", "c3")})
	if err != nil {
		t.Fatal(err)
	}
	msgs := plan.Messages
	if len(msgs) != 6 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("%d messages", len(msgs))
	}
	asst := msgs[2]
	if asst.Role != "assistant" || asst.Text() != "looking" || len(asst.ToolCalls) != 3 {
		t.Fatalf("%+v", asst)
	}
	for i, id := range []string{"c1", "c2", "c3"} {
		if asst.ToolCalls[i].ID != id || asst.ToolCalls[i].Type != "function" || asst.ToolCalls[i].Function.Name != "file.read" || asst.ToolCalls[i].Function.Arguments != `{"path":"a"}` {
			t.Fatalf("call %d: %+v", i, asst.ToolCalls[i])
		}
		if m := msgs[3+i]; m.Role != "tool" || m.ToolCallID != id || m.Text() != `{"answer":"`+id+`"}` {
			t.Fatalf("answer %d: %+v", i, m)
		}
	}
	// With no text the assistant message's content is null, as the model's was.
	plan, err = contextplan.AssembleContext(contextplan.Input{SystemPrompt: "sys", History: toolItems(t, nil, "c1")})
	if err != nil || plan.Messages[2].Content != nil || len(plan.Messages[2].ToolCalls) != 1 {
		t.Fatalf("%+v %v", plan.Messages, err)
	}
}

func TestAPromptNeverHoldsACallWithoutItsAnswerOrAnAnswerWithoutItsCall(t *testing.T) {
	items := toolItems(t, nil, "c1", "c2")
	for name, in := range map[string][]contextplan.HistoryItem{
		"a call with no answer":             items[:3],
		"answers out of order":              {items[0], items[1], items[3], items[2]},
		"an answer to another call":         {items[0], items[1], items[2], {ContextSeq: 4, MessageID: "x", HistoryKind: contextplan.HistoryObservation, ItemKind: contextplan.ItemKindToolResult, ToolCallID: "zz", Text: "{}"}},
		"an answer with no call":            {items[0], items[2]},
		"a message between call and answer": {items[0], items[1], {ContextSeq: 3, MessageID: "w", HistoryKind: contextplan.HistoryWork, Origin: "agent", Text: "interlude"}, {ContextSeq: 4, MessageID: "a", HistoryKind: contextplan.HistoryObservation, ItemKind: contextplan.ItemKindToolResult, ToolCallID: "c1", Text: "{}"}, {ContextSeq: 5, MessageID: "b", HistoryKind: contextplan.HistoryObservation, ItemKind: contextplan.ItemKindToolResult, ToolCallID: "c2", Text: "{}"}},
		"an answer that names no call":      {items[0], items[1], items[2], items[3], {ContextSeq: 5, MessageID: "n", HistoryKind: contextplan.HistoryObservation, ItemKind: contextplan.ItemKindToolResult, Text: "{}"}},
	} {
		if _, err := contextplan.AssembleContext(contextplan.Input{SystemPrompt: "sys", History: in}); !errors.Is(err, contextplan.ErrInvalidContext) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAStoredToolCallRecordIsReadStrictly(t *testing.T) {
	good := record(t, nil, "c1")
	if _, err := contextplan.DecodeToolCalls(good); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"not JSON":                   `{"content":`,
		"an extra field":             strings.Replace(good, `{"content"`, `{"extra":1,"content"`, 1),
		"no calls":                   `{"content":null,"tool_calls":[]}`,
		"a duplicate key":            `{"content":null,"content":null,"tool_calls":[{"function":{"arguments":"{}","name":"n"},"id":"c","type":"function"}]}`,
		"a call with no ID":          `{"content":null,"tool_calls":[{"function":{"arguments":"{}","name":"n"},"id":"","type":"function"}]}`,
		"a call of another type":     `{"content":null,"tool_calls":[{"function":{"arguments":"{}","name":"n"},"id":"c","type":"custom"}]}`,
		"a call with no name":        `{"content":null,"tool_calls":[{"function":{"arguments":"{}","name":""},"id":"c","type":"function"}]}`,
		"an ID used twice":           `{"content":null,"tool_calls":[{"function":{"arguments":"{}","name":"n"},"id":"c","type":"function"},{"function":{"arguments":"{}","name":"n"},"id":"c","type":"function"}]}`,
		"a call with an extra field": `{"content":null,"tool_calls":[{"function":{"arguments":"{}","name":"n"},"id":"c","type":"function","index":0}]}`,
	} {
		if _, err := contextplan.DecodeToolCalls(bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := contextplan.EncodeToolCalls(contextplan.ToolCallsRecord{}); err == nil {
		t.Fatal("a record with no call was encoded")
	}
	// A result item with no call ID, and a stored record that does not decode, are not prompts.
	if _, err := contextplan.AssembleContext(contextplan.Input{SystemPrompt: "s", History: []contextplan.HistoryItem{{ContextSeq: 1, HistoryKind: contextplan.HistoryWork, ItemKind: contextplan.ItemKindToolCalls, Text: "{}"}}}); !errors.Is(err, contextplan.ErrInvalidContext) {
		t.Fatalf("%v", err)
	}
}
