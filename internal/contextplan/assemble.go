// Package contextplan turns what a Thread holds into what a model sees, and
// judges whether that fits: AssembleContext (F04) builds the prompt of one act
// request from the typed context blocks and the Thread's applied history or a
// checkpoint's projection, and EstimateBudget (F05) holds a measure answer to the
// request it was asked about and decides fit or no-fit.
//
// Both are pure. The prompt is a function of its inputs in a fixed order; nothing
// here reads a store, a clock or the network, and no text is trimmed, normalized
// or dropped on the way.
package contextplan

import (
	"fmt"
	"strings"
	"sync"

	harness "github.com/Nyukimin/RenCrow_Harness"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

var (
	systemOnce sync.Once
	systemText string
	systemErr  error
)

// ActSystemPrompt is the whole text of prompts/act_system.md: the Harness's common
// output and data rules, which open every act prompt as one system message.
func ActSystemPrompt() (string, error) {
	systemOnce.Do(func() {
		b, err := harness.Prompts.ReadFile("prompts/act_system.md")
		if err != nil {
			systemErr = fmt.Errorf("contextplan: the act system prompt is not embedded: %w", err)
			return
		}
		systemText = string(b)
	})
	return systemText, systemErr
}

// History kinds the live history may hold (INTERNAL_CONTRACTS).
const (
	HistoryHuman      = "HumanInstruction"
	HistoryAutomation = "AutomationInstruction"
	HistoryProtected  = "Protected"
	HistoryWork       = "Work"
)

// HistoryItem is one applied entry of a Thread, in the order it was applied to the
// context. ContextSeq is the application order (context_entries.context_seq), not
// the order the raw text was received in.
type HistoryItem struct {
	ContextSeq  int64
	MessageID   string
	HistoryKind string
	Origin      string // human, automation, unknown or agent
	Text        string
	// ItemKind is what the item is within its history kind: ItemKindToolCalls or
	// ItemKindToolResult for the two halves of a Tool exchange, empty otherwise.
	ItemKind string
	// ToolCallID is the call a ItemKindToolResult answers.
	ToolCallID string
}

// Input is everything one prompt is made of. Blocks are the typed context blocks
// of the Run's start. A Projection, when there is one, stands for the history up to
// a checkpoint and brings its own context blocks (Blocks must then be empty);
// History is whatever was applied after it, or the whole history when there is no
// projection.
type Input struct {
	SystemPrompt string
	Blocks       []protocol.ContextBlock
	Projection   *Projection
	History      []HistoryItem
}

// ManifestEntry describes one logical piece of the prompt for diagnostics: what
// kind it was and the digest of its text. It is a record about the prompt, never a
// part of it.
type ManifestEntry struct {
	Kind       string
	Revision   string
	TextDigest string
}

// PromptPlan is the assembled prompt of one act request.
type PromptPlan struct {
	Messages []modelport.ChatMessage
	Manifest []ManifestEntry
}

var kindRank = map[string]int{
	protocol.KindCharacterSystemPrompt:  0,
	protocol.KindStableRuntimeContext:   1,
	protocol.KindRecallPack:             2,
	protocol.KindVariableRuntimeContext: 3,
}

// AssembleContext (F04) builds the prompt in the one order MODEL_PROJECTION fixes:
// the system prompt; then the typed blocks by kind (Character as system, Stable as
// developer, Recall and Variable as user data envelopes), several blocks of a kind
// in the order given, each its own message and none merged or dropped, an empty
// text included; then the history in application order. The latest user message is
// never moved to the end.
//
// The result has every Tool call answered by its results in order, or it is an
// ErrInvalidContext. The inputs are copied.
func AssembleContext(in Input) (PromptPlan, error) {
	if in.SystemPrompt == "" {
		return PromptPlan{}, invalidf("the system prompt is empty")
	}
	blocks := in.Blocks
	if in.Projection != nil {
		if len(in.Blocks) != 0 {
			return PromptPlan{}, invalidf("a projection brings its own context blocks")
		}
		blocks = in.Projection.ContextBlocks
	}
	var plan PromptPlan
	plan.Messages = append(plan.Messages, modelport.System(in.SystemPrompt))
	digest, err := protocol.TextDigest(in.SystemPrompt)
	if err != nil {
		return PromptPlan{}, invalidf("the system prompt is not valid UTF-8")
	}
	plan.Manifest = append(plan.Manifest, ManifestEntry{Kind: "system_prompt", TextDigest: digest})

	for rank := 0; rank <= 3; rank++ {
		for _, b := range blocks {
			r, ok := kindRank[b.Kind]
			if !ok {
				return PromptPlan{}, invalidf("a context block has a kind that is not a typed context kind")
			}
			if r != rank {
				continue
			}
			msg, err := blockMessage(b)
			if err != nil {
				return PromptPlan{}, err
			}
			d, err := protocol.TextDigest(b.Text)
			if err != nil {
				return PromptPlan{}, invalidf("a context block is not valid UTF-8")
			}
			plan.Messages = append(plan.Messages, msg)
			plan.Manifest = append(plan.Manifest, ManifestEntry{Kind: b.Kind, Revision: b.Revision, TextDigest: d})
		}
	}

	if in.Projection != nil {
		msgs, err := renderProjection(in.Projection)
		if err != nil {
			return PromptPlan{}, err
		}
		plan.Messages = append(plan.Messages, msgs...)
	}
	last := int64(0)
	for _, h := range in.History {
		if h.ContextSeq <= last {
			return PromptPlan{}, invalidf("the history is not in strictly increasing application order")
		}
		last = h.ContextSeq
		msg, err := historyMessage(h)
		if err != nil {
			return PromptPlan{}, err
		}
		plan.Messages = append(plan.Messages, msg)
	}
	if err := checkToolPairing(plan.Messages); err != nil {
		return PromptPlan{}, err
	}
	return plan, nil
}

// blockMessage is the single message of one typed block.
func blockMessage(b protocol.ContextBlock) (modelport.ChatMessage, error) {
	switch b.Kind {
	case protocol.KindCharacterSystemPrompt:
		return modelport.System(b.Text), nil
	case protocol.KindStableRuntimeContext:
		return modelport.Developer(b.Text), nil
	}
	body, err := cj1(b)
	if err != nil {
		return modelport.ChatMessage{}, invalidf("a context block cannot be encoded")
	}
	return modelport.User(contextPrefix + body), nil
}

// HistoryMessage is the message one applied entry stands for: the same rendering the
// live history gets, so a checkpoint's entry and the history it was made from are the
// same bytes.
func HistoryMessage(h HistoryItem) (modelport.ChatMessage, error) { return historyMessage(h) }

// ParseInputEnvelope reads the content of an enveloped input (automation or unknown
// origin) back into the origin and the text it carries. Anything that is not exactly
// the envelope historyMessage writes is an ErrInvalidContext.
func ParseInputEnvelope(content string) (origin, text string, err error) {
	body, ok := strings.CutPrefix(content, inputPrefix)
	if !ok {
		return "", "", invalidf("an input is not in the input envelope")
	}
	v, derr := strictjson.Decode([]byte(body))
	obj, isObj := v.(map[string]any)
	if derr != nil || !isObj || len(obj) != 2 {
		return "", "", invalidf("an input envelope has the wrong fields")
	}
	origin, ok1 := obj["origin"].(string)
	text, ok2 := obj["text"].(string)
	if !ok1 || !ok2 {
		return "", "", invalidf("an input envelope has the wrong fields")
	}
	again, cerr := cj1(struct {
		Origin string `json:"origin"`
		Text   string `json:"text"`
	}{origin, text})
	if cerr != nil || inputPrefix+again != content {
		return "", "", invalidf("an input envelope is not in canonical form")
	}
	return origin, text, nil
}

// historyMessage is the message one applied entry stands for. A human instruction
// is the user's exact text; an automation or unknown-origin input is data in an
// envelope that names its origin and so confers no authority; the model's own
// earlier answer is an assistant message.
func historyMessage(h HistoryItem) (modelport.ChatMessage, error) {
	switch h.ItemKind {
	case ItemKindToolCalls:
		rec, err := DecodeToolCalls(h.Text)
		if err != nil {
			return modelport.ChatMessage{}, err
		}
		return modelport.ChatMessage{Role: "assistant", Content: rec.Content, ToolCalls: rec.ToolCalls}, nil
	case ItemKindToolResult:
		if h.ToolCallID == "" {
			return modelport.ChatMessage{}, invalidf("a Tool result names no Tool call")
		}
		return modelport.Tool(h.ToolCallID, h.Text), nil
	}
	switch h.HistoryKind {
	case HistoryHuman:
		return modelport.User(h.Text), nil
	case HistoryAutomation, HistoryProtected:
		origin := h.Origin
		if h.HistoryKind == HistoryProtected {
			origin = protocol.OriginUnknown
		}
		if origin != protocol.OriginAutomation && origin != protocol.OriginUnknown {
			return modelport.ChatMessage{}, invalidf("an enveloped input has an origin that is not automation or unknown")
		}
		body, err := cj1(struct {
			Origin string `json:"origin"`
			Text   string `json:"text"`
		}{origin, h.Text})
		if err != nil {
			return modelport.ChatMessage{}, invalidf("an input cannot be encoded")
		}
		return modelport.User(inputPrefix + body), nil
	case HistoryWork:
		return modelport.Assistant(h.Text), nil
	}
	return modelport.ChatMessage{}, invalidf("a history entry has a kind a prompt cannot hold")
}

// checkToolPairing requires every assistant Tool call to be answered by the
// matching tool messages, in order and directly after it, and no tool message to
// stand alone.
func checkToolPairing(msgs []modelport.ChatMessage) error {
	var pending []string
	for _, m := range msgs {
		if len(pending) > 0 {
			if m.Role != "tool" || m.ToolCallID != pending[0] {
				return invalidf("a Tool call is not answered by its result in order")
			}
			pending = pending[1:]
			continue
		}
		switch m.Role {
		case "tool":
			return invalidf("a tool message answers no Tool call")
		case "assistant":
			for _, c := range m.ToolCalls {
				pending = append(pending, c.ID)
			}
		}
	}
	if len(pending) > 0 {
		return invalidf("a Tool call has no result")
	}
	return nil
}
