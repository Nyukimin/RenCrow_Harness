package modelclient

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

// A refusal's own bytes are kept on the error, up to the bound, so that the caller can seal
// them as the Attempt's response Evidence. They are the Gateway's bytes as it sent them,
// never a rendering of them, and they are not part of any message.

func TestARefusalCarriesTheBodyTheGatewayStated(t *testing.T) {
	f := harnesstest.NewFake()
	req := fakeGatewayRequest(t, f)
	zero := notStartedReceipt(t, req)
	body := strictBody(t, "RATE_LIMITED", true, zero)
	g := newGateway(t)
	g.reply(generateKey, 429, "application/json", body)

	_, err := g.client().Generate(t.Context(), req)
	se := asStrict(t, err)
	if se.Code != "RATE_LIMITED" || !bytes.Equal(se.Body, body) || se.BodyTruncated {
		t.Fatalf("%s %q truncated=%t", se.Code, se.Body, se.BodyTruncated)
	}
	if strings.Contains(se.Error(), string(body)) || strings.Contains(se.Message, "RATE_LIMITED}") {
		t.Fatal("the body is part of an error text")
	}
	// The body is a copy: changing it changes nothing the client holds.
	se.Body[0] = 'X'
	_, err = g.client().Generate(t.Context(), req)
	if !bytes.Equal(asStrict(t, err).Body, body) {
		t.Fatal("two refusals share one body")
	}
}

func TestARefusalThatIsNotAStrictErrorStillCarriesWhatCameBack(t *testing.T) {
	f := harnesstest.NewFake()
	req := fakeGatewayRequest(t, f)
	g := newGateway(t)
	g.reply(generateKey, 502, "text/html", []byte("<html>bad gateway</html>"))
	_, err := g.client().Generate(t.Context(), req)
	se := asStrict(t, err)
	if se.Code != modelport.CodeContractFailed || se.Receipt != nil || string(se.Body) != "<html>bad gateway</html>" {
		t.Fatalf("%s %+v %q", se.Code, se.Receipt, se.Body)
	}
	if se.GenerationState() != modelport.StateUnknown {
		t.Fatal("a refusal without a receipt is not taken for not started")
	}
}

func TestABodyLongerThanTheBoundIsCutAndSaysSo(t *testing.T) {
	f := harnesstest.NewFake()
	req := fakeGatewayRequest(t, f)
	// Inside what the client reads (1 MiB) but over what Evidence keeps (64 KiB).
	mid := bytes.Repeat([]byte("m"), modelport.MaxFailureBodyBytes+100)
	g := newGateway(t)
	g.reply(generateKey, 500, "application/json", mid)
	_, err := g.client().Generate(t.Context(), req)
	se := asStrict(t, err)
	if se.Code != modelport.CodeContractFailed || len(se.Body) != modelport.MaxFailureBodyBytes || !se.BodyTruncated || !bytes.Equal(se.Body, mid[:modelport.MaxFailureBodyBytes]) {
		t.Fatalf("%s len=%d truncated=%t", se.Code, len(se.Body), se.BodyTruncated)
	}

	// Over what the client reads at all: the refusal is still a contract failure and its first
	// bytes are what is kept.
	huge := bytes.Repeat([]byte("h"), maxFailureBytes+10)
	g2 := newGateway(t)
	g2.reply(generateKey, 500, "application/json", huge)
	_, err = g2.client().Generate(t.Context(), req)
	se = asStrict(t, err)
	if se.Code != modelport.CodeContractFailed || len(se.Body) != modelport.MaxFailureBodyBytes || !se.BodyTruncated || se.Receipt != nil {
		t.Fatalf("%s len=%d truncated=%t", se.Code, len(se.Body), se.BodyTruncated)
	}
}

func TestNoBodyIsCarriedWhereNothingCameBack(t *testing.T) {
	f := harnesstest.NewFake()
	req := fakeGatewayRequest(t, f)
	// A request the client refuses before it sends anything.
	bad := req
	bad.Rencrow.Harness.ExpectedInputDigest = strings.Repeat("0", 64)
	g := newGateway(t)
	_, err := g.client().Generate(t.Context(), bad)
	if se := asStrict(t, err); len(se.Body) != 0 || se.BodyTruncated {
		t.Fatalf("%+v", se)
	}
	if g.requests() != 0 {
		t.Fatal("a refused request was sent")
	}
	// No connection at all.
	c, err := New("http://127.0.0.1:1/v1", Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Generate(t.Context(), req)
	if se := asStrict(t, err); se.Code != modelport.CodeConnectFailed || len(se.Body) != 0 {
		t.Fatalf("%+v", se)
	}
}

func TestAMeasureRefusalCarriesItsBodyToo(t *testing.T) {
	f := harnesstest.NewFake()
	req := actRequest(t)
	body := strictBody(t, "UNSUPPORTED_RECOVERY_PROFILE", false, notStartedReceipt(t, genRequest(t, req)))
	g := newGateway(t)
	g.reply(measureKey, 409, "application/json", body)
	_, err := g.client().Measure(t.Context(), measureRequest(req))
	if se := asStrict(t, err); !bytes.Equal(se.Body, body) {
		t.Fatalf("%q", se.Body)
	}
	_ = f
}

// A non-stream answer keeps the answer itself, byte for byte, beside the stream made of it.
func TestANonStreamAnswerKeepsTheGatewaysBytes(t *testing.T) {
	req := stageRequest(t, modelport.StageSummary)
	answer := mustJSON(t, map[string]any{
		"id": "chatcmpl-1", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "{\"summary\":\"x\"}"}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 3},
		"rencrow": map[string]any{"harness_receipt": terminalReceiptFor(req)},
	})
	g := newGateway(t)
	g.reply(generateKey, 200, "application/json", answer)
	rc, err := g.client().Generate(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	raw, ok := rc.(modelport.RawBodier)
	if !ok || !bytes.Equal(raw.RawBody(), answer) {
		t.Fatal("the answer was not kept as the Gateway sent it")
	}
	if c := assemble(t, rc); c.TerminalKind != modelport.KindFinal || c.FinalText != "{\"summary\":\"x\"}" {
		t.Fatalf("%+v", c)
	}
}

func terminalReceiptFor(req modelport.ChatRequest) map[string]any {
	h := req.Rencrow.Harness
	return receiptWire(h.Stage, "terminal", 1, h.ExpectedInputDigest, h.ExpectedRequestDigest, h.ExpectedBindingFingerprint)
}
