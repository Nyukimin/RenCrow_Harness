package protocol_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	harness "github.com/Nyukimin/RenCrow_Harness"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Every public DTO must carry exactly the fields its schema definition has: a Go
// type that gained or lost a field without the schema (or the reverse) would be
// silently accepted by Decode only as far as DisallowUnknownFields notices, and
// would never be noticed on the way out. The check is by field set, and by
// requiredness: the schema requires every field it lists.
func TestDTOFieldsMatchTheSchemaDefinitions(t *testing.T) {
	raw, err := harness.Schemas.ReadFile("schemas/protocol.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	types := map[string]any{
		"ErrorInfo": protocol.ErrorInfo{}, "ByteRange": protocol.ByteRange{}, "SourceRef": protocol.SourceRef{},
		"ContextBlock": protocol.ContextBlock{}, "Binding": protocol.Binding{}, "Upstream": protocol.Upstream{},
		"OriginProof": protocol.OriginProof{}, "InputMessage": protocol.InputMessage{}, "Limits": protocol.Limits{},
		"StartInput": protocol.StartInput{}, "StartResult": protocol.StartResult{}, "ResumeInput": protocol.ResumeInput{},
		"ResumeResult": protocol.ResumeResult{}, "InputAppendInput": protocol.InputAppendInput{}, "InputReceipt": protocol.InputReceipt{},
		"BudgetReport": protocol.BudgetReport{}, "CompactInput": protocol.CompactInput{}, "CompactResult": protocol.CompactResult{},
		"Verification": protocol.Verification{}, "RunResult": protocol.RunResult{}, "RunInfo": protocol.RunInfo{},
		"SessionInfo": protocol.SessionInfo{}, "InitializeInput": protocol.InitializeInput{}, "Capability": protocol.Capability{},
		"CapabilitiesResult": protocol.CapabilitiesResult{}, "SessionOpenInput": protocol.SessionOpenInput{},
		"SessionOpenResult": protocol.SessionOpenResult{}, "SessionListInput": protocol.SessionListInput{},
		"SessionListResult": protocol.SessionListResult{}, "SessionGetInput": protocol.SessionGetInput{},
		"SessionForkInput": protocol.SessionForkInput{}, "ForkResult": protocol.ForkResult{}, "InterruptInput": protocol.InterruptInput{},
		"InterruptReceipt": protocol.InterruptReceipt{}, "RunGetInput": protocol.RunGetInput{}, "OperationAccepted": protocol.OperationAccepted{},
		"ReceiptGetInput": protocol.ReceiptGetInput{}, "ReceiptRecord": protocol.ReceiptRecord{}, "EventsReadInput": protocol.EventsReadInput{},
		"Event": protocol.Event{}, "EventsReadResult": protocol.EventsReadResult{}, "EvidenceReadInput": protocol.EvidenceReadInput{},
		"EvidenceReadResult": protocol.EvidenceReadResult{}, "ShutdownInput": protocol.ShutdownInput{}, "ShutdownResult": protocol.ShutdownResult{},
		"EmptyInput": protocol.EmptyInput{}, "RecoveryPolicy": protocol.RecoveryPolicy{}, "IntakeReceipt": protocol.IntakeReceipt{},
		"ModelAttemptReceipt": protocol.ModelAttemptReceipt{}, "ProgressDelta": protocol.ProgressDelta{}, "ProgressReset": protocol.ProgressReset{},
		"ProgressGap": protocol.ProgressGap{},
	}
	for name, v := range types {
		def, ok := doc.Defs[name]
		if !ok {
			t.Errorf("%s: no such definition in the schema", name)
			continue
		}
		var want []string
		for k := range def.Properties {
			want = append(want, k)
		}
		sort.Strings(want)
		got := jsonFields(reflect.TypeOf(v))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Go fields %v, schema properties %v", name, got, want)
		}
		req := append([]string(nil), def.Required...)
		sort.Strings(req)
		if len(req) != len(want) || !reflect.DeepEqual(req, want) {
			t.Errorf("%s: the schema does not require every field it lists", name)
		}
	}
	// ReceiptPayload is a tagged union with its own shape.
	if got := jsonFields(reflect.TypeOf(protocol.ReceiptPayload{})); !reflect.DeepEqual(got, []string{"type", "value"}) {
		t.Errorf("ReceiptPayload fields %v", got)
	}
	t.Logf("DTOs checked against the schema: %d", len(types)+1)
}

func jsonFields(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := tag
		for j := 0; j < len(tag); j++ {
			if tag[j] == ',' {
				name = tag[:j]
				break
			}
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// The Event payload structs must match the type-specific schemas the same way.
func TestEventPayloadStructsMatchTheSchema(t *testing.T) {
	raw, err := harness.Schemas.ReadFile("schemas/event.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs struct {
			Event struct {
				AllOf []struct {
					If struct {
						Properties struct {
							Type struct {
								Const string `json:"const"`
							} `json:"type"`
						} `json:"properties"`
					} `json:"if"`
					Then struct {
						Properties struct {
							Payload struct {
								Properties map[string]json.RawMessage `json:"properties"`
								Required   []string                   `json:"required"`
							} `json:"payload"`
						} `json:"properties"`
					} `json:"then"`
				} `json:"allOf"`
			} `json:"Event"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	payloads := []protocol.EventPayload{
		protocol.SessionCreatedPayload{}, protocol.InputAcceptedPayload{}, protocol.InputAppliedPayload{}, protocol.TaskCreatedPayload{},
		protocol.RunStartedPayload{}, protocol.ModelRequestedPayload{}, protocol.ModelCompletedPayload{}, protocol.ActionPreparedPayload{},
		protocol.ActionDispatchStartedPayload{}, protocol.ActionCompletedPayload{}, protocol.CheckpointCommittedPayload{},
		protocol.ControlCancelRequestedPayload{}, protocol.RunTerminalPayload{}, protocol.ModelRetryScheduledPayload{}, protocol.ModelAttemptStartedPayload{},
	}
	byType := map[string]protocol.EventPayload{}
	for _, p := range payloads {
		byType[p.EventType()] = p
	}
	seen := 0
	for _, c := range doc.Defs.Event.AllOf {
		typ := c.If.Properties.Type.Const
		p, ok := byType[typ]
		if !ok {
			t.Errorf("schema has event type %s with no payload struct", typ)
			continue
		}
		var want []string
		for k := range c.Then.Properties.Payload.Properties {
			want = append(want, k)
		}
		sort.Strings(want)
		if got := jsonFields(reflect.TypeOf(p)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Go fields %v, schema properties %v", typ, got, want)
		}
		req := append([]string(nil), c.Then.Properties.Payload.Required...)
		sort.Strings(req)
		if !reflect.DeepEqual(req, want) {
			t.Errorf("%s: not every payload field is required", typ)
		}
		seen++
	}
	if seen != 15 || len(byType) != 15 {
		t.Fatalf("%d schema payloads, %d structs; want 15 and 15", seen, len(byType))
	}
}
