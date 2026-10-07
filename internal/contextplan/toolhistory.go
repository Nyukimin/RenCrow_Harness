package contextplan

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Item kinds of the Tool exchange in a Thread's history (items.metadata_json "kind").
// Both are applied to the context together, as one batch, so a prompt never holds a
// Tool call that has no answer.
const (
	// ItemKindToolCalls is the assistant message that asked for Tools.
	ItemKindToolCalls = "tool_calls"
	// ItemKindToolResult is the answer to one Tool call: a role=tool message.
	ItemKindToolResult = "tool_result"
	// HistoryObservation is the history kind of a Tool result.
	HistoryObservation = "Observation"
)

// ToolCallsRecord is the stored form of an assistant message that asked for Tools: its
// text (null when it had none) and its calls exactly as the model made them, the
// arguments kept as the text they arrived in.
type ToolCallsRecord struct {
	Content   *string              `json:"content"`
	ToolCalls []modelport.ToolCall `json:"tool_calls"`
}

// EncodeToolCalls is the canonical JSON of the record, the text stored as the item's
// Evidence.
func EncodeToolCalls(r ToolCallsRecord) ([]byte, error) {
	if len(r.ToolCalls) == 0 {
		return nil, errors.New("contextplan: an assistant message with Tool calls has at least one")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return protocol.EncodeCanonicalContract(raw)
}

// DecodeToolCalls reads a stored record strictly: its two fields, nothing else, and
// every call a function call with an ID, a name and its arguments.
func DecodeToolCalls(text string) (ToolCallsRecord, error) {
	v, err := strictjson.Decode([]byte(text))
	if err != nil {
		return ToolCallsRecord{}, invalidf("a stored Tool call record is not valid JSON")
	}
	if m, ok := v.(map[string]any); !ok || len(m) != 2 {
		return ToolCallsRecord{}, invalidf("a stored Tool call record has the wrong fields")
	}
	var rec ToolCallsRecord
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil || len(rec.ToolCalls) == 0 {
		return ToolCallsRecord{}, invalidf("a stored Tool call record does not hold Tool calls")
	}
	seen := map[string]bool{}
	for _, c := range rec.ToolCalls {
		if c.Type != "function" || c.ID == "" || c.Function.Name == "" || seen[c.ID] {
			return ToolCallsRecord{}, invalidf("a stored Tool call is not a function call with a unique ID")
		}
		seen[c.ID] = true
	}
	return rec, nil
}
