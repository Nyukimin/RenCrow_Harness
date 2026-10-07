package modelport_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

// The design's terminal frame (wire/stream_terminal.json, the bytes the Gateway's own tests
// are written against) with each outcome other than the answer it carries: the receipt of a
// stream that ended in an error, an incomplete or a refusal is held to the same request as
// the receipt of an answer.

func fixtureExpected(t testing.TB) modelport.Expected {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(wire(t, "stream_terminal.json"), &m); err != nil {
		t.Fatal(err)
	}
	r := receipt(m)
	return modelport.Expected{
		Stage: r["stage"].(string), InputDigest: r["input_digest"].(string), RequestDigest: r["request_digest"].(string),
		BindingFingerprint: r["binding_fingerprint"].(string), ProfileID: r["recovery_profile"].(string), ProfileRevision: r["recovery_profile_revision"].(string),
	}
}

func TestTheDesignsTerminalFrameInEveryOutcomeIsHeldToItsRequest(t *testing.T) {
	exp := fixtureExpected(t)
	asError := func(code string, edit func(m map[string]any)) string {
		return terminalOf(t, func(m map[string]any) {
			m["outcome"], m["finish_reason"], m["code"] = "error", nil, code
			if edit != nil {
				edit(m)
			}
		})
	}
	stream := func(terminal string) modelport.Completion {
		return modelport.AssembleStrictStream(strings.NewReader(chunk("", `{"role":"assistant","content":"x"}`)+terminal+done), modelport.AssembleOptions{})
	}

	t.Run("an error that matches the request is kept as it was stated", func(t *testing.T) {
		c := modelport.CheckCompletionReceipt(stream(asError("UPSTREAM_TRANSIENT", nil)), exp, nil)
		if c.TerminalKind != modelport.KindError || c.FailureCode != "UPSTREAM_TRANSIENT" || c.GenerationState != "terminal" || c.Violation != "" {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("the same error for another request is a contract failure", func(t *testing.T) {
		other := exp
		other.RequestDigest = "0000000000000000000000000000000000000000000000000000000000000000"
		c := modelport.CheckCompletionReceipt(stream(asError("UPSTREAM_TRANSIENT", nil)), other, nil)
		if c.TerminalKind != modelport.KindError || c.FailureCode != modelport.CodeContractFailed || c.GenerationState != "terminal" {
			t.Fatalf("%+v", c)
		}
		other = exp
		other.ProfileID, other.ProfileRevision = modelport.ProfileTerminalOutputOnce, "r"
		if c := modelport.CheckCompletionReceipt(stream(asError("UPSTREAM_TRANSIENT", nil)), other, nil); c.FailureCode != modelport.CodeContractFailed {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("a code that cannot come with a finished generation contradicts the frame", func(t *testing.T) {
		c := modelport.CheckCompletionReceipt(stream(asError("RATE_LIMITED", nil)), exp, nil)
		if c.FailureCode != modelport.CodeContractFailed || c.GenerationState != modelport.StateUnknown || c.BackendAttempts != nil {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("an incomplete and a refusal are held to the request too", func(t *testing.T) {
		other := exp
		other.BindingFingerprint = "bfp-v1:0000000000000000000000000000000000000000000000000000000000000000"
		incomplete := terminalOf(t, func(m map[string]any) { m["outcome"], m["finish_reason"], m["code"] = "incomplete", "length", nil })
		refused := terminalOf(t, func(m map[string]any) { m["outcome"], m["finish_reason"], m["code"] = "refused", "stop", nil })
		for name, term := range map[string]string{"incomplete": incomplete, "refused": refused} {
			if c := modelport.CheckCompletionReceipt(stream(term), exp, nil); c.TerminalKind == modelport.KindError {
				t.Fatalf("%s that matches: %+v", name, c)
			}
			if c := modelport.CheckCompletionReceipt(stream(term), other, nil); c.FailureCode != modelport.CodeContractFailed {
				t.Fatalf("%s for another binding: %+v", name, c)
			}
		}
	})
}
