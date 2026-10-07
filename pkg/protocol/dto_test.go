package protocol_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// rawList splits a JSON array fixture into its element texts.
func rawList(t testing.TB, rel string) []json.RawMessage {
	t.Helper()
	var list []json.RawMessage
	exampleJSON(t, rel, &list)
	return list
}

// mutate decodes object JSON, applies f and encodes it again, keeping numbers
// exact. It deliberately uses encoding/json, independent of the strict decoder.
func mutate(t testing.TB, raw []byte, f func(m map[string]any)) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	f(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func wantCode(t testing.TB, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted, want %s", code)
	}
	if got := protocol.CodeOf(err); got != code {
		t.Fatalf("code %q (%v), want %s", got, err, code)
	}
	if !errors.Is(err, protocol.ErrInvalidInput) && code != protocol.CodeInternal {
		t.Fatalf("error does not wrap ErrInvalidInput: %v", err)
	}
}

// TestDecodeDesignExamples decodes every positive design example into its typed
// DTO, re-encodes it, and checks the canonical bytes equal the canonical form of
// the fixture. It reports how many examples were checked.
func TestDecodeDesignExamples(t *testing.T) {
	count := 0
	roundTrip := func(t *testing.T, name string, raw []byte, decode func([]byte) (any, error), encode func(any) ([]byte, error)) {
		t.Helper()
		v, err := decode(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out, err := encode(v)
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}
		want, err := protocol.EncodeCanonicalContract(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, want) {
			t.Fatalf("%s: round trip changed the value\n got %s\nwant %s", name, out, want)
		}
		count++
	}
	single := []struct {
		file string
		dec  func([]byte) (any, error)
		enc  func(any) ([]byte, error)
	}{
		{"core_start.json", dec[protocol.StartInput], enc[protocol.StartInput]},
		{"core_relay_start.json", dec[protocol.StartInput], enc[protocol.StartInput]},
		{"start_result.json", dec[protocol.StartResult], enc[protocol.StartResult]},
		{"resume_result.json", dec[protocol.ResumeResult], enc[protocol.ResumeResult]},
		{"run_result.json", dec[protocol.RunResult], enc[protocol.RunResult]},
		{"intake_receipt.json", dec[protocol.IntakeReceipt], enc[protocol.IntakeReceipt]},
		{"progress_reset.json", dec[protocol.ProgressReset], enc[protocol.ProgressReset]},
	}
	for _, c := range single {
		t.Run(c.file, func(t *testing.T) { roundTrip(t, c.file, example(t, c.file), c.dec, c.enc) })
	}
	for i, raw := range rawList(t, "compact_results.json") {
		roundTrip(t, "compact_results", raw, dec[protocol.CompactResult], enc[protocol.CompactResult])
		_ = i
	}
	for _, raw := range rawList(t, "model_attempt_receipts.json") {
		roundTrip(t, "model_attempt_receipts", raw, dec[protocol.ModelAttemptReceipt], enc[protocol.ModelAttemptReceipt])
	}
	var events struct {
		Events []json.RawMessage `json:"events"`
	}
	exampleJSON(t, "wire/event_payloads.json", &events)
	for _, raw := range events.Events {
		roundTrip(t, "event_payloads", raw, dec[protocol.Event], enc[protocol.Event])
	}
	t.Logf("positive design examples decoded and round-tripped: %d", count)
	if want := 7 + 9 + 3 + 15; count != want {
		t.Fatalf("checked %d examples, want %d (fixtures changed?)", count, want)
	}
}

func dec[T protocol.Message](raw []byte) (any, error) { return protocol.Decode[T](raw) }

func enc[T protocol.Message](v any) ([]byte, error) { return protocol.Encode(v.(T)) }
