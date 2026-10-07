package modelclient

import (
	"context"
	"net/http"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

// Measure states of the contract (schemas/measure.schema.json).
const (
	stateVerifiedExact = "verified_exact"
	stateVerifiedBound = "verified_bound"
	stateEstimated     = "estimated"
	stateUnverified    = "unverified"
)

// Measure sends POST /context/measure: the Gateway counts the final request the way
// a generation of it would see it, and generates nothing.
//
// The Harness computes the logical input digest itself, from the request, before
// anything is sent, and holds the answer to it: an answer for another input is
// INPUT_DIGEST_MISMATCH and nothing may be generated on it. The other two values
// (the request digest and the binding fingerprint) are the Gateway's and are only
// carried back; they are compared here only with the expected_* values the request
// itself carries, which a measure may.
//
// The answer is read as the closed MeasureResult of the contract, and its state is
// taken at its word only as far as the state's own rules go: verified_exact has one
// number, verified_bound a range, estimated is no proof, unverified counts nothing.
// An unverified answer is an answer, not an error: the caller decides what an
// unverified budget means. Whether a verified count fits is the caller's, too.
func (c *Client) Measure(ctx context.Context, req modelport.MeasureRequest) (modelport.MeasureResult, error) {
	digest, err := req.Request.LogicalInputDigest()
	exp := expectationOf(req.Request, digest)
	switch {
	case req.ContractVersion != modelport.ContractVersion:
		return modelport.MeasureResult{}, localRefusal(exp, modelport.CodeUnsupportedContract, "the measure request speaks another contract version", false)
	case err != nil || req.Request.Validate(false) != nil:
		return modelport.MeasureResult{}, localRefusal(exp, modelport.CodeUnsupportedContract, "the request does not satisfy the strict contract", false)
	case req.Request.Rencrow.Harness.ExpectedInputDigest != "" && req.Request.Rencrow.Harness.ExpectedInputDigest != digest:
		return modelport.MeasureResult{}, localRefusal(exp, modelport.CodeInputDigestMismatch, "expected_input_digest is not the digest of the request", false)
	}
	req.Request = wireRequest(req.Request)
	body, err := marshalJSON(req)
	if err != nil {
		return modelport.MeasureResult{}, localRefusal(exp, modelport.CodeUnsupportedContract, "the request cannot be encoded", false)
	}

	ex, err := c.roundTrip(ctx, "measure", http.MethodPost, "/context/measure", body, "application/json")
	if err != nil {
		return modelport.MeasureResult{}, err
	}
	defer ex.resp.Body.Close()

	if ex.resp.StatusCode < 200 || ex.resp.StatusCode > 299 {
		failure, over, err := readPrefix(ex.resp.Body, maxFailureBytes)
		switch {
		case err != nil:
			return modelport.MeasureResult{}, &TransportError{Op: "measure", Connected: true, Err: cause(err)}
		case over:
			return modelport.MeasureResult{}, attachBody(contractFailed(nil, "the refusal is over the size limit"), failure, true)
		}
		return modelport.MeasureResult{}, attachBody(decodeFailure(failure, exp, ex.resp.StatusCode), failure, false)
	}
	data, over, err := readBody(ex.resp.Body, maxMeasureBytes)
	switch {
	case err != nil:
		return modelport.MeasureResult{}, &TransportError{Op: "measure", Connected: true, Err: cause(err)}
	case over:
		return modelport.MeasureResult{}, contractFailed(nil, "the measure result is over the size limit")
	}
	return readMeasure(data, req, digest)
}

// readMeasure reads the measure answer and holds it to the request.
func readMeasure(data []byte, req modelport.MeasureRequest, digest string) (modelport.MeasureResult, error) {
	v, err := strictjson.Decode(data)
	if err != nil {
		return modelport.MeasureResult{}, contractFailed(nil, "the measure result is not strict JSON")
	}
	var res modelport.MeasureResult
	if err := schemacheck.Unmarshal(schemacheck.Measure, "", v, &res); err != nil {
		return modelport.MeasureResult{}, contractFailed(nil, "the measure result does not satisfy its schema")
	}
	if msg := measureStateFault(res); msg != "" {
		return modelport.MeasureResult{}, contractFailed(nil, msg)
	}
	h := req.Request.Rencrow.Harness
	switch {
	case res.InputDigest != digest:
		return modelport.MeasureResult{}, &modelport.StrictError{
			Code: modelport.CodeInputDigestMismatch, Message: "the measure result is for another input than the request",
		}
	case res.SafetyMarginTokens != req.SafetyMarginTokens:
		return modelport.MeasureResult{}, contractFailed(nil, "the measure result did not echo the safety margin")
	case res.ReservedOutputTokens != req.Request.MaxTokens:
		return modelport.MeasureResult{}, contractFailed(nil, "the measure result reserved another output than max_tokens")
	case h.ExpectedRequestDigest != "" && h.ExpectedRequestDigest != res.RequestDigest:
		return modelport.MeasureResult{}, contractFailed(nil, "the measure result is for another request digest than the expected one")
	case h.ExpectedBindingFingerprint != "" && h.ExpectedBindingFingerprint != res.BindingFingerprint:
		return modelport.MeasureResult{}, contractFailed(nil, "the measure result is for another binding than the expected one")
	}
	return res, nil
}

// measureStateFault returns what is wrong with a measure result in the terms of
// its own state, or "". The schema fixes the shape; the state fixes what the
// numbers mean.
func measureStateFault(r modelport.MeasureResult) string {
	lower, upper := r.PromptLower, r.PromptUpper
	if r.State != stateUnverified && r.BindingFingerprint == unverifiedFingerprint {
		return "a counted measure result carries the fingerprint of an unverified binding"
	}
	switch r.State {
	case stateUnverified:
		switch {
		case lower != nil || upper != nil:
			return "an unverified measure result carries a count"
		case r.Reason == nil:
			return "an unverified measure result states no reason"
		}
	case stateVerifiedExact:
		switch {
		case lower == nil || upper == nil || *lower != *upper:
			return "a verified_exact measure result is not one number"
		case r.EffectiveContextLimit == nil:
			return "a verified measure result has no context limit"
		}
	case stateVerifiedBound:
		switch {
		case lower == nil || upper == nil || *lower > *upper:
			return "a verified_bound measure result is not a range"
		case r.EffectiveContextLimit == nil:
			return "a verified measure result has no context limit"
		}
	case stateEstimated:
		if lower == nil || upper == nil || *lower > *upper {
			return "an estimated measure result is not a range"
		}
	default:
		return "a measure result has an unknown state"
	}
	return ""
}
