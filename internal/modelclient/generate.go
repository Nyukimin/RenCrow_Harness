package modelclient

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

// Generate sends one strict generation request: POST /chat/completions with the
// harness extension the request carries. One call is one HTTP request and at most
// one backend generation: nothing here retries, follows a redirect, reuses a
// connection or sends a second request, whatever the outcome.
//
// The request must already be a complete generation request (every expected_* value
// from the measure and the descriptor, and the input digest the Harness computed).
// A request that is not is refused before anything is sent, and says so with a
// receipt of a generation that never started.
//
// What comes back:
//
//   - the strict SSE response of an act request, unread, for the caller to read to
//     its end with modelport.AssembleStrictStream and to close. It is returned only
//     when the answer is a 2xx event stream; the stream's own terminal frame and
//     [DONE] are the assembler's to check. A response that ends early is the
//     assembler's unknown, never a terminal;
//   - for a request without a stream (the Selection and Summary stages), the same
//     strict stream built from the Gateway's one JSON answer after its receipt was
//     held to the request, so that one assembler reads both. The stream is not
//     sent progressively and nothing about it is a measure of speed;
//   - a *modelport.StrictError for a refusal the Gateway stated, with the receipt it
//     stated when that receipt is valid and consistent with the request, and
//     MODEL_CONTRACT_FAILED without a receipt (so: unknown) when it is not;
//   - a CONNECT_FAILED StrictError the client observed itself, when no connection to
//     the Gateway was established and so no byte of the request was written (its
//     SourceCode is SourceNotConnected);
//   - otherwise a *TransportError or the context's error: the generation's state is
//     unknown, because a timeout, a reset or an EOF shows nothing about it.
//
// The caller's context stops the HTTP exchange, including the reading of the
// returned stream; stopping it proves nothing about the generation, which stays
// unknown until a receipt says otherwise.
func (c *Client) Generate(ctx context.Context, req modelport.ChatRequest) (io.ReadCloser, error) {
	digest, err := req.LogicalInputDigest()
	exp := expectationOf(req, digest)
	switch {
	case err != nil || req.Validate(true) != nil:
		return nil, localRefusal(exp, modelport.CodeUnsupportedContract, "the request does not satisfy the strict contract", false)
	case req.Rencrow.Harness.ExpectedInputDigest != digest:
		return nil, localRefusal(exp, modelport.CodeInputDigestMismatch, "expected_input_digest is not the digest of the request", false)
	}
	body, err := marshalJSON(wireRequest(req))
	if err != nil {
		return nil, localRefusal(exp, modelport.CodeUnsupportedContract, "the request cannot be encoded", false)
	}
	accept := "application/json"
	if req.Stream {
		accept = "text/event-stream"
	}

	ex, err := c.roundTrip(ctx, "generate", http.MethodPost, "/chat/completions", body, accept)
	if err != nil {
		var te *TransportError
		if errors.As(err, &te) && !te.Connected && ctx.Err() == nil {
			return nil, notConnected(exp)
		}
		return nil, err
	}
	resp := ex.resp

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		failure, over, err := readPrefix(resp.Body, maxFailureBytes)
		switch {
		case err != nil:
			return nil, &TransportError{Op: "generate", Connected: true, Err: cause(err)}
		case over:
			return nil, attachBody(contractFailed(nil, "the refusal is over the size limit"), failure, true)
		}
		return nil, attachBody(decodeFailure(failure, exp, resp.StatusCode), failure, false)
	}

	if req.Stream {
		if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "text/event-stream" {
			resp.Body.Close()
			return nil, contractFailed(nil, "a stream request was answered with something other than an event stream")
		}
		return resp.Body, nil
	}

	defer resp.Body.Close()
	data, over, err := readBody(resp.Body, maxAnswerBytes)
	switch {
	case err != nil:
		return nil, &TransportError{Op: "generate", Connected: true, Err: cause(err)}
	case over:
		return nil, contractFailed(nil, "the answer is over the size limit")
	}
	return answerStream(data, req, exp)
}

// notConnected is the CONNECT_FAILED of a request that never left the client: the
// connection to the Gateway could not be made, so nothing was dispatched. It is the
// client's own observation, and says so in its source code.
func notConnected(exp expectation) *modelport.StrictError {
	e := localRefusal(exp, modelport.CodeConnectFailed, "no connection to the Gateway could be made, so nothing was sent", true)
	src := SourceNotConnected
	e.SourceCode = &src
	return e
}
