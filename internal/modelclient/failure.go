package modelclient

import (
	"errors"
	"regexp"
	"strconv"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

// Source codes the client itself gives an error it made. They tell an error the
// client observed from one the Gateway stated. The "/" keeps them apart for good: a
// code the Gateway states is only ever kept as a source when it is a plain
// upper-case code (see plainCode), which never has one.
const (
	// SourceNotConnected marks a CONNECT_FAILED the client observed itself: no
	// connection to the Gateway was established, so no request byte was sent.
	SourceNotConnected = "HARNESS_CLIENT/NOT_CONNECTED"
	// SourceNotSent marks a refusal the client made before sending anything.
	SourceNotSent = "HARNESS_CLIENT/REQUEST_NOT_SENT"
)

// expectation is what the client knows of the request an answer is held to. An
// empty digest is one the client does not know and does not compare.
type expectation struct {
	stage           string
	profileID       string
	profileRevision string
	requestID       *string
	inputDigest     string
	requestDigest   string
	fingerprint     string
}

func expectationOf(req modelport.ChatRequest, inputDigest string) expectation {
	h := req.Rencrow.Harness
	return expectation{
		stage: h.Stage, profileID: h.Recovery.ProfileID, profileRevision: h.Recovery.ProfileRevision, requestID: req.Rencrow.RequestID,
		inputDigest: inputDigest, requestDigest: h.ExpectedRequestDigest, fingerprint: h.ExpectedBindingFingerprint,
	}
}

func (e expectation) expected() modelport.Expected {
	return modelport.Expected{
		Stage: e.stage, InputDigest: e.inputDigest, RequestDigest: e.requestDigest, BindingFingerprint: e.fingerprint,
		ProfileID: e.profileID, ProfileRevision: e.profileRevision,
	}
}

var sourcePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,79}$`)

// contractFailed is the error of an answer that does not satisfy the strict
// contract: it names no generation state (no receipt), so the kernel reads the
// generation as unknown. source is the code the Gateway stated, when it is a plain
// code; it is kept for the diagnosis and never used to choose the normalized code.
func contractFailed(source *string, message string) *modelport.StrictError {
	return &modelport.StrictError{Code: modelport.CodeContractFailed, Message: message, SourceCode: source}
}

// localRefusal is a refusal the client made before it sent anything. Nothing was
// dispatched, which the client knows because it did not dispatch: the receipt it
// carries is the client's own statement of that, with no digest it did not measure.
func localRefusal(exp expectation, code, message string, retryable bool) *modelport.StrictError {
	zero := int64(0)
	source := SourceNotSent
	return &modelport.StrictError{
		Code: code, Message: message, Retryable: retryable, RequestID: exp.requestID, SourceCode: &source,
		Receipt: &modelport.GatewayAttemptReceipt{
			ContractVersion: modelport.ContractVersion, Stage: exp.stage, LogicalRequests: 1, BackendAttempts: &zero,
			GenerationState: modelport.StateNotStarted, RecoveryProfile: exp.profileID, RecoveryProfileRevision: exp.profileRevision,
			AppliedTransformations: []string{},
		},
	}
}

// hiddenRetryError is the error of an answer whose receipt admits a retry the
// contract forbids. The count of generations behind it cannot be trusted, so the
// receipt it carries keeps what the Gateway stated, says hidden_retry, and says the
// state is unknown with no attempt count, as an assembled stream does. When the
// stated receipt is itself unreadable, the receipt is only that statement and the
// request's own stage and profile.
func hiddenRetryError(v any, exp expectation, source *string) *modelport.StrictError {
	receipt := statedReceipt(v)
	if receipt == nil {
		receipt = &modelport.GatewayAttemptReceipt{
			ContractVersion: modelport.ContractVersion, Stage: exp.stage, LogicalRequests: 1,
			RecoveryProfile: exp.profileID, RecoveryProfileRevision: exp.profileRevision, AppliedTransformations: []string{},
		}
	}
	receipt.HiddenRetry, receipt.GenerationState, receipt.BackendAttempts = true, modelport.StateUnknown, nil
	return &modelport.StrictError{
		Code: modelport.CodeContractFailed, Message: "the receipt admits a hidden retry", RequestID: exp.requestID, SourceCode: source, Receipt: receipt,
	}
}

// statedReceipt reads the receipt of an answer that states hidden_retry, as it is
// stated: the schema forbids exactly that flag, so it is read with the flag cleared.
// It is nil when the receipt is not otherwise a valid one.
func statedReceipt(v any) *modelport.GatewayAttemptReceipt {
	obj, _ := v.(map[string]any)
	ext, _ := obj["rencrow"].(map[string]any)
	rec, _ := ext["harness_receipt"].(map[string]any)
	if rec == nil {
		return nil
	}
	cleared := make(map[string]any, len(rec))
	for k, x := range rec {
		cleared[k] = x
	}
	cleared["hidden_retry"] = false
	var out modelport.GatewayAttemptReceipt
	if err := schemacheck.Unmarshal(schemacheck.LLMContract, "GatewayAttemptReceipt", cleared, &out); err != nil {
		return nil
	}
	return &out
}

// strictErrorWire is the closed StrictError of the contract.
type strictErrorWire struct {
	Error struct {
		Code       string  `json:"code"`
		Message    string  `json:"message"`
		Retryable  bool    `json:"retryable"`
		RequestID  *string `json:"request_id"`
		SourceCode *string `json:"source_code"`
	} `json:"error"`
	Rencrow struct {
		HarnessReceipt modelport.GatewayAttemptReceipt `json:"harness_receipt"`
	} `json:"rencrow"`
}

// decodeFailure reads the body of a non-2xx answer. What it returns is always a
// *StrictError, because a refusal that is not a valid strict error is itself a
// contract failure, and what it says of the generation is only what a valid receipt
// stated:
//
//   - no strict receipt (a body that is not a strict error, a receipt that fails its
//     schema, one that contradicts the request or its own code): MODEL_CONTRACT_FAILED
//     with no receipt, so the generation is unknown. The HTTP status and the text of
//     a refusal are not evidence, and a generic INVALID_REQUEST is not a refusal
//     before generating that may be sent again;
//   - a receipt that admits a hidden retry: MODEL_CONTRACT_FAILED, hidden_retry;
//   - a valid receipt with a code the Harness does not map: MODEL_CONTRACT_FAILED
//     with the receipt, so the known state is kept, and the stated code in SourceCode;
//   - otherwise the code and the receipt as stated.
func decodeFailure(body []byte, exp expectation, status int) *modelport.StrictError {
	v, err := strictjson.Decode(body)
	if err != nil {
		return contractFailed(httpSource(status), "the refusal is not strict JSON")
	}
	source := plainCode(v)
	if source == nil {
		source = httpSource(status)
	}
	if hiddenRetryStated(v) {
		return hiddenRetryError(v, exp, source)
	}
	var wire strictErrorWire
	if err := schemacheck.Unmarshal(schemacheck.LLMContract, "StrictError", v, &wire); err != nil {
		return contractFailed(source, "the refusal is not a strict error")
	}
	code, receipt := wire.Error.Code, wire.Rencrow.HarnessReceipt
	if exp.requestID != nil && wire.Error.RequestID != nil && *exp.requestID != *wire.Error.RequestID {
		return contractFailed(source, "the refusal answers another request")
	}
	if err := checkReceipt(receipt, exp, code); err != nil {
		return contractFailed(source, err.Error())
	}
	out := &modelport.StrictError{
		Code: code, Message: wire.Error.Message, RequestID: wire.Error.RequestID, SourceCode: wire.Error.SourceCode, Receipt: &receipt,
	}
	fact, known := modelport.FactOf(code)
	switch {
	case !known:
		out.Code, out.SourceCode, out.Message = modelport.CodeContractFailed, source, "the refusal has a code the contract does not list"
	case !fact.States.Has(receipt.GenerationState):
		return contractFailed(source, "the refusal states a generation state that its code cannot have")
	default:
		out.Retryable = wire.Error.Retryable && fact.Retry && receipt.GenerationState != modelport.StateUnknown
	}
	return out
}

var errReceipt = errors.New("the receipt contradicts the request")

// checkReceipt holds a receipt to the request it answers. A refusal that measured a
// value the request had expected a different one of (the binding, the request
// digest, the input digest) carries the measured value, so that digest is not
// compared; every other stated digest must be the expected one.
func checkReceipt(r modelport.GatewayAttemptReceipt, exp expectation, code string) error {
	e := exp.expected()
	own := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	if e.InputDigest == "" || code == modelport.CodeInputDigestMismatch {
		e.InputDigest = own(r.InputDigest)
	}
	if e.RequestDigest == "" || code == modelport.CodeRequestDigestMismatch || code == modelport.CodeBindingChanged {
		e.RequestDigest = own(r.RequestDigest)
	}
	if e.BindingFingerprint == "" || code == modelport.CodeBindingChanged {
		e.BindingFingerprint = own(r.BindingFingerprint)
	}
	if err := modelport.VerifyReceipt(r, e); err != nil {
		return errReceipt
	}
	return nil
}

// httpSource names the HTTP status of a refusal that states no usable code. It is a
// diagnostic only: the status is not evidence of anything about a generation.
func httpSource(status int) *string {
	if status < 100 || status > 599 {
		return nil
	}
	s := "HTTP_" + strconv.Itoa(status)
	return &s
}

// plainCode returns error.code of a loosely read refusal when it is a plain code,
// else nil. It is only ever a diagnostic.
func plainCode(v any) *string {
	obj, _ := v.(map[string]any)
	inner, _ := obj["error"].(map[string]any)
	code, _ := inner["code"].(string)
	if !sourcePattern.MatchString(code) {
		return nil
	}
	return &code
}

// hiddenRetryStated reports whether a refusal's receipt, however else it reads,
// says hidden_retry is true.
func hiddenRetryStated(v any) bool {
	obj, _ := v.(map[string]any)
	ext, _ := obj["rencrow"].(map[string]any)
	rec, _ := ext["harness_receipt"].(map[string]any)
	return rec["hidden_retry"] == true
}
