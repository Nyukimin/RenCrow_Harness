package protocol_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func designEvents(t testing.TB) []json.RawMessage {
	t.Helper()
	var f struct {
		Events []json.RawMessage `json:"events"`
	}
	exampleJSON(t, "wire/event_payloads.json", &f)
	return f.Events
}

// A decoded design event, rebuilt through BuildTypedEvent from its typed payload
// and common fields, must come out byte-for-byte identical in canonical form.
func TestBuildTypedEventReproducesAll15DesignEvents(t *testing.T) {
	types := map[string]bool{}
	for _, raw := range designEvents(t) {
		ev, err := protocol.Decode[protocol.Event](raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		payload, err := ev.TypedPayload()
		if err != nil {
			t.Fatalf("%s: %v", ev.Type, err)
		}
		if payload.EventType() != ev.Type {
			t.Fatalf("payload type %s, event type %s", payload.EventType(), ev.Type)
		}
		built, err := protocol.BuildTypedEvent(protocol.EventCommon{
			EventID: ev.EventID, EventSeq: ev.EventSeq, ThreadID: ev.ThreadID, TaskID: ev.TaskID, RunID: ev.RunID,
			ReceiptID: ev.ReceiptID, EvidenceID: ev.EvidenceID, MessageID: ev.MessageID, Code: ev.Code, RecordedAt: ev.RecordedAt,
		}, payload)
		if err != nil {
			t.Fatalf("%s: build: %v", ev.Type, err)
		}
		if !bytes.Equal(built.Payload, ev.Payload) {
			t.Fatalf("%s: payload differs\n got %s\nwant %s", ev.Type, built.Payload, ev.Payload)
		}
		gotWhole, _ := protocol.Encode(built)
		wantWhole, _ := protocol.EncodeCanonicalContract(raw)
		if !bytes.Equal(gotWhole, wantWhole) {
			t.Fatalf("%s: event differs\n got %s\nwant %s", ev.Type, gotWhole, wantWhole)
		}
		types[ev.Type] = true
	}
	if len(types) != 15 {
		t.Fatalf("design examples cover %d event types, want 15", len(types))
	}
}

func TestEventPayloadIsCJ1OfThePayloadOnly(t *testing.T) {
	ev, err := protocol.BuildTypedEvent(protocol.EventCommon{
		EventID: "evt_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001", EventSeq: 1, ThreadID: "thr_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001",
		TaskID: protocol.Str("tsk_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"), RunID: protocol.Str("run_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"),
		RecordedAt: "2026-10-07T01:00:00Z",
	}, protocol.ControlCancelRequestedPayload{
		RunID: "run_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001", ControlRevision: 1, Reason: "USER_REQUESTED", Principal: "user:ren",
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"control_revision":1,"principal":"user:ren","reason":"USER_REQUESTED","run_id":"run_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"}`
	if string(ev.Payload) != want {
		t.Fatalf("payload %s, want %s", ev.Payload, want)
	}
	if strings.Contains(string(ev.Payload), `"type"`) || strings.Contains(string(ev.Payload), "event_id") {
		t.Fatal("the Event must not be nested into its own payload")
	}
}

func TestBuildTypedEventRefusals(t *testing.T) {
	const (
		thr = "thr_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"
		tsk = "tsk_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"
		run = "run_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"
		evd = "evd_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"
	)
	common := func() protocol.EventCommon {
		return protocol.EventCommon{EventID: "evt_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001", EventSeq: 1, ThreadID: thr,
			TaskID: protocol.Str(tsk), RunID: protocol.Str(run), EvidenceID: protocol.Str(evd), RecordedAt: "2026-10-07T01:00:00Z"}
	}
	started := protocol.ModelAttemptStartedPayload{ActionID: "act_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001", AttemptID: "att_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001",
		Ordinal: 0, RequestID: "req_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001", GenerationAttemptsUsed: 1, DeadlineAt: "2026-10-07T02:00:00Z"}
	if _, err := protocol.BuildTypedEvent(common(), started); err != nil {
		t.Fatalf("valid event refused: %v", err)
	}
	bad := started
	bad.ActionID = "att_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001" // ID of another kind
	cases := map[string]func() error{
		"nil payload":              func() error { _, err := protocol.BuildTypedEvent(common(), nil); return err },
		"wrong ID kind in payload": func() error { _, err := protocol.BuildTypedEvent(common(), bad); return err },
		"ordinal beyond 1": func() error {
			p := started
			p.Ordinal = 2
			_, err := protocol.BuildTypedEvent(common(), p)
			return err
		},
		"generation attempts below 1": func() error {
			p := started
			p.GenerationAttemptsUsed = 0
			_, err := protocol.BuildTypedEvent(common(), p)
			return err
		},
		"missing run on an execution event": func() error {
			c := common()
			c.RunID = nil
			_, err := protocol.BuildTypedEvent(c, started)
			return err
		},
		"event_seq 0": func() error {
			c := common()
			c.EventSeq = 0
			_, err := protocol.BuildTypedEvent(c, started)
			return err
		},
		"recorded_at with offset": func() error {
			c := common()
			c.RecordedAt = "2026-10-07T01:00:00+09:00"
			_, err := protocol.BuildTypedEvent(c, started)
			return err
		},
		"thread id of another kind": func() error {
			c := common()
			c.ThreadID = tsk
			_, err := protocol.BuildTypedEvent(c, started)
			return err
		},
		"session.created with a task": func() error {
			_, err := protocol.BuildTypedEvent(common(), protocol.SessionCreatedPayload{
				SessionID: "ses_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001", Mode: "structured_only",
				Binding: protocol.Binding{Kind: "model_route", Selector: "x", ProfileRevision: "r1"},
			})
			return err
		},
		"cancel payload run differs from common run": func() error {
			_, err := protocol.BuildTypedEvent(common(), protocol.ControlCancelRequestedPayload{
				RunID: "run_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0099", ControlRevision: 1, Reason: "USER_REQUESTED", Principal: "user:ren"})
			return err
		},
		"run.terminal code differs from common code": func() error {
			c := common()
			c.Code = protocol.Str("OTHER")
			_, err := protocol.BuildTypedEvent(c, protocol.RunTerminalPayload{Status: "completed", Code: "FINAL_RESPONSE_ACCEPTED", ResultEvidenceID: evd})
			return err
		},
		"run.terminal with an unknown status": func() error {
			c := common()
			c.Code = protocol.Str("X")
			_, err := protocol.BuildTypedEvent(c, protocol.RunTerminalPayload{Status: "done", Code: "X", ResultEvidenceID: evd})
			return err
		},
		"retry next_ordinal other than 1": func() error {
			_, err := protocol.BuildTypedEvent(common(), protocol.ModelRetryScheduledPayload{
				ActionID: "act_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001", FailedAttemptID: "att_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001",
				NextOrdinal: 2, TriggerCode: "REASONING_ONLY", RecoveryProfile: "same_request", RecoveryProfileRevision: "r"})
			return err
		},
		"message_id on an event that has none": func() error {
			c := common()
			c.MessageID = protocol.Str("msg_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001")
			_, err := protocol.BuildTypedEvent(c, started)
			return err
		},
		"code on an event that has none": func() error {
			c := common()
			c.Code = protocol.Str("SOMETHING")
			_, err := protocol.BuildTypedEvent(c, started)
			return err
		},
		"action.completed with a nil evidence list": func() error {
			_, err := protocol.BuildTypedEvent(common(), protocol.ActionCompletedPayload{
				ActionID: "act_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001", AttemptID: "att_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001", EffectState: "completed"})
			return err
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			if err := f(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestDecodeEventRefusals(t *testing.T) {
	events := designEvents(t)
	byType := map[string][]byte{}
	for _, raw := range events {
		var h struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &h)
		byType[h.Type] = raw
	}
	cases := map[string]func(m map[string]any){
		"unknown key in the payload": func(m map[string]any) { m["payload"].(map[string]any)["extra"] = 1 },
		"missing key in the payload": func(m map[string]any) { delete(m["payload"].(map[string]any), "action_id") },
		"unknown type":               func(m map[string]any) { m["type"] = "run.paused" },
		"payload of another type":    func(m map[string]any) { m["type"] = "model.completed" },
		"raw text in the payload":    func(m map[string]any) { m["payload"].(map[string]any)["final_text"] = "secret" },
		"unknown common field":       func(m map[string]any) { m["raw"] = "x" },
		"event_seq 0":                func(m map[string]any) { m["event_seq"] = 0 },
	}
	base := byType[protocol.EventModelRequested]
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := protocol.Decode[protocol.Event](mutate(t, base, f)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	semantic := map[string]struct {
		base string
		f    func(m map[string]any)
	}{
		"task.created payload task differs": {protocol.EventTaskCreated, func(m map[string]any) {
			m["payload"].(map[string]any)["task_id"] = "tsk_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0099"
		}},
		"run.started payload run differs": {protocol.EventRunStarted, func(m map[string]any) {
			m["payload"].(map[string]any)["run_id"] = "run_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0099"
		}},
		"run.started without a run": {protocol.EventRunStarted, func(m map[string]any) { m["run_id"] = nil }},
		"model.completed evidence differs": {protocol.EventModelCompleted, func(m map[string]any) {
			m["evidence_id"] = "evd_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0099"
		}},
		"input.accepted intake thread differs": {protocol.EventInputAccepted, func(m map[string]any) {
			m["thread_id"] = "thr_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0099"
		}},
		"input.accepted intake message differs": {protocol.EventInputAccepted, func(m map[string]any) { m["message_id"] = nil }},
		"session.created with a run": {protocol.EventSessionCreated, func(m map[string]any) {
			m["run_id"] = "run_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"
		}},
		"recorded_at not a timestamp": {protocol.EventRunStarted, func(m map[string]any) { m["recorded_at"] = "yesterday" }},
	}
	for name, c := range semantic {
		t.Run(name, func(t *testing.T) {
			if _, err := protocol.Decode[protocol.Event](mutate(t, byType[c.base], c.f)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// A payload that is valid JSON but not the canonical bytes must not pass as the
// stored form: events.payload_json is hashed, compared and re-served as stored.
func TestEventPayloadMustBeCanonical(t *testing.T) {
	base := designEvents(t)[0]
	ev, err := protocol.Decode[protocol.Event](base)
	if err != nil {
		t.Fatal(err)
	}
	spaced := ev
	spaced.Payload = json.RawMessage(strings.Replace(string(ev.Payload), `,"`, `, "`, 1))
	if err := spaced.Validate(); err == nil {
		t.Fatal("a payload with extra whitespace was accepted")
	}
	reordered := ev
	reordered.Payload = json.RawMessage(`{"mode":"structured_only","session_id":"` + "ses_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001" + `","binding":{"agent_id":null,"execution_role":null,"kind":"model_route","profile_revision":"fixture-v1","selector":"fixture-local"}}`)
	if err := reordered.Validate(); err == nil {
		t.Fatal("a payload with unsorted keys was accepted")
	}
}
