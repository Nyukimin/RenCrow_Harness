package modelport

import "fmt"

// Expected is what the Harness knows before a generation: the values the terminal
// receipt must echo.
type Expected struct {
	Stage              string
	InputDigest        string
	RequestDigest      string
	BindingFingerprint string
	ProfileID          string
	ProfileRevision    string
}

// VerifyReceipt checks the receipt of a completion against what was expected. A
// receipt that contradicts the request (another stage, digest, binding or
// recovery profile, more than one logical request, a hidden retry) is a contract
// failure, whatever the output looks like. Digests the receipt does not carry
// (null, as an error before normalization may leave them) are not compared: an
// expected value is never taken for an observed one.
func VerifyReceipt(r GatewayAttemptReceipt, exp Expected) error {
	bad := func(what string) error { return fmt.Errorf("modelport: the receipt %s", what) }
	switch {
	case r.ContractVersion != ContractVersion:
		return bad("has another contract version")
	case r.Stage != exp.Stage:
		return bad("is for another stage")
	case r.LogicalRequests != 1:
		return bad("does not show one logical request")
	case r.HiddenRetry:
		return bad("admits a hidden retry")
	case r.RecoveryProfile != exp.ProfileID || r.RecoveryProfileRevision != exp.ProfileRevision:
		return bad("names another recovery profile")
	case r.InputDigest != nil && *r.InputDigest != exp.InputDigest:
		return bad("has another input digest")
	case r.RequestDigest != nil && *r.RequestDigest != exp.RequestDigest:
		return bad("has another request digest")
	case r.BindingFingerprint != nil && *r.BindingFingerprint != exp.BindingFingerprint:
		return bad("has another binding fingerprint")
	}
	return nil
}

// RawBodier is implemented by a response that was built from a non-stream answer of
// the model side: RawBody is that answer as the model side sent it, byte for byte, so a
// caller that keeps the response as Evidence keeps it and not the stream made of it.
type RawBodier interface {
	RawBody() []byte
}

// VerifyTransformations holds the transformations a receipt says were applied to the
// ones the binding's recovery profile allows. A transformation the profile does not list
// is a change nobody authorized; the same_request profile allows none.
func VerifyTransformations(applied, allowed []string) error {
	for _, a := range applied {
		found := false
		for _, ok := range allowed {
			if a == ok {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("modelport: the receipt applied a transformation the recovery profile does not allow")
		}
	}
	return nil
}

// CheckCompletionReceipt holds the terminal receipt of a completion to the request it
// answers, whatever way the generation ended: a completed one, an error, an incomplete
// or refused one. Only a receipt that shows what it must is believed:
//
//   - a receipt that contradicts the request (another stage, digest, binding or
//     recovery profile, a transformation the profile does not allow) makes the
//     completion a model contract failure; the state the receipt stated is kept, as
//     for any other contract failure, and nothing the stream carried is adopted;
//   - a failure code that the mapping lists but that cannot come with the generation
//     state the receipt states (RATE_LIMITED after a finished generation, a plain
//     UPSTREAM_TRANSIENT with an unknown one) is a frame that contradicts itself: the
//     state of that generation cannot be read from it, so it is unknown.
//
// A completion without a receipt is returned as it is (its state is already unknown).
func CheckCompletionReceipt(c Completion, exp Expected, allowedTransformations []string) Completion {
	if c.Receipt == nil {
		return c
	}
	if err := VerifyReceipt(*c.Receipt, exp); err != nil || VerifyTransformations(c.Receipt.AppliedTransformations, allowedTransformations) != nil {
		c.TerminalKind, c.FailureCode, c.FinalText, c.ToolIntents, c.ContentText = KindError, CodeContractFailed, "", nil, ""
		c.Violation = "the receipt contradicts the request"
		return c
	}
	if c.FailureCode != "" {
		if fact, known := FactOf(c.FailureCode); known && !fact.States.Has(c.GenerationState) {
			c.TerminalKind, c.FailureCode, c.FinalText, c.ToolIntents, c.ContentText = KindError, CodeContractFailed, "", nil, ""
			c.GenerationState, c.BackendAttempts = StateUnknown, nil
			c.Violation = "the failure code cannot come with the generation state of the receipt"
		}
	}
	return c
}
