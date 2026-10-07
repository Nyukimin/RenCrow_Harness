package protocol

import "encoding/base64"

// This file holds the conditions PROTOCOL sections 6 and 9 state that JSON Schema
// cannot (or cannot clearly) express. Decode runs them after the schema; Encode
// runs them so a malformed value is never produced. They never look at the clock
// or at any store: conditions that need either (proof time windows, revisions,
// limits against host caps) belong to the admission path.

// preCheck makes a user_message block INVALID_REQUEST. The schema refuses it too,
// but PROTOCOL names this exact code, and a caller that sends the block is
// double-submitting the input, which is a request error and not a malformed param.
func (StartInput) preCheck(decoded any) error {
	obj, ok := decoded.(map[string]any)
	if !ok {
		return nil
	}
	blocks, ok := obj["context_blocks"].([]any)
	if !ok {
		return nil
	}
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok && m["kind"] == KindUserMessage {
			return NewError(CodeInvalidRequest, "context_blocks must not contain a user_message block; the input is the user message").
				Wrap(invalid("user_message context block"))
		}
	}
	return nil
}

// Validate checks a block's revision against its content (CORE_INTEGRATION
// section 1: the Harness recomputes it, it never trusts the caller's value) and
// the source range.
func (b ContextBlock) Validate() error {
	if b.Source != nil {
		if err := b.Source.Validate(); err != nil {
			return err
		}
	}
	want, err := ContextRevision(b.Kind, b.Text, b.Source)
	if err != nil {
		return err
	}
	if b.Revision != want {
		return requestError("context block revision does not match its content")
	}
	return nil
}

// Validate checks that the byte range is not inverted.
func (s SourceRef) Validate() error {
	if s.Range.Start > s.Range.End {
		return requestError("source range start is after its end")
	}
	return nil
}

// Validate checks the proof's own consistency: both timestamps are real UTC
// instants and the proof expires after it was issued. The 300 second TTL, the
// 30 second future allowance and "not yet expired" depend on the clock and are
// checked where the proof is verified.
func (o OriginProof) Validate() error {
	issued, err := ParseTimestamp(o.IssuedAt)
	if err != nil {
		return requestError("origin proof issued_at is not a valid timestamp")
	}
	expires, err := ParseTimestamp(o.ExpiresAt)
	if err != nil {
		return requestError("origin proof expires_at is not a valid timestamp")
	}
	if !expires.After(issued) {
		return requestError("origin proof expires before or when it was issued")
	}
	return nil
}

// Validate checks the context blocks and the proof. It does not check
// proof-to-request bindings (destination thread, mutation key, raw hash): those
// need the authenticated caller and belong to proof verification, which reports
// INVALID_ORIGIN_PROOF.
func (s StartInput) Validate() error {
	for _, b := range s.ContextBlocks {
		if b.Kind == KindUserMessage {
			return requestError("context_blocks must not contain a user_message block")
		}
		if err := b.Validate(); err != nil {
			return err
		}
	}
	if s.Input.OriginProof != nil {
		return s.Input.OriginProof.Validate()
	}
	return nil
}

// Validate checks the proof of an appended input.
func (a InputAppendInput) Validate() error {
	if a.Input.OriginProof != nil {
		return a.Input.OriginProof.Validate()
	}
	return nil
}

// Validate checks that a passed verification names its evidence and criteria.
func (v Verification) Validate() error {
	if v.Status == "passed" {
		if len(v.EvidenceIDs) == 0 {
			return requestError("a passed verification needs at least one evidence id")
		}
		if v.CriteriaRevision == nil || *v.CriteriaRevision == "" {
			return requestError("a passed verification needs a criteria revision")
		}
	}
	return nil
}

// Validate checks the embedded verification.
func (r RunResult) Validate() error { return r.Verification.Validate() }

// Validate checks that terminal, phase and result agree, that the result belongs to
// this Run, and that the unknown generation count does not exceed the used count.
func (r RunInfo) Validate() error {
	if (r.Phase == "Terminal") != r.Terminal {
		return requestError("phase Terminal and terminal=true must agree")
	}
	if r.Terminal != (r.Result != nil) {
		return requestError("a terminal run carries a result and a running run does not")
	}
	if r.Result != nil {
		if r.Result.RunID != r.RunID || r.Result.TaskID != r.TaskID {
			return requestError("the result belongs to a different run or task")
		}
		if err := r.Result.Validate(); err != nil {
			return err
		}
	}
	if r.GenerationAttemptsUnknown > r.GenerationAttemptsUsed {
		return requestError("generation_attempts_unknown exceeds generation_attempts_used")
	}
	if _, err := ParseTimestamp(r.DeadlineAt); err != nil {
		return requestError("deadline_at is not a valid timestamp")
	}
	return nil
}

// Validate checks the measurement interval.
func (b BudgetReport) Validate() error {
	if b.PromptLower != nil && b.PromptUpper != nil && *b.PromptLower > *b.PromptUpper {
		return requestError("prompt_lower is above prompt_upper")
	}
	switch b.State {
	case "verified_exact":
		if b.PromptLower == nil || b.PromptUpper == nil || *b.PromptLower != *b.PromptUpper {
			return requestError("an exact budget has equal lower and upper counts")
		}
	case "verified_bound":
		if b.PromptLower == nil || b.PromptUpper == nil {
			return requestError("a bound budget has both counts")
		}
	}
	return nil
}

func (b *BudgetReport) verified() bool {
	return b != nil && (b.State == "verified_exact" || b.State == "verified_bound")
}

// Validate checks PROTOCOL section 9: a committed compaction carries verified
// before and after budgets, a checkpoint and ordered boundaries; the capacity
// numbers come as a pair and show a real shortfall; and a result that was not
// executed claims no outcome.
func (c CompactResult) Validate() error {
	if c.Before != nil {
		if err := c.Before.Validate(); err != nil {
			return err
		}
	}
	if c.After != nil {
		if err := c.After.Validate(); err != nil {
			return err
		}
	}
	if (c.RequiredMinimumTokens == nil) != (c.AvailableTokens == nil) {
		return requestError("required_minimum_tokens and available_tokens come together")
	}
	if c.RequiredMinimumTokens != nil {
		if c.Outcome == nil || *c.Outcome != "CapacityBlocked" {
			return requestError("capacity numbers belong to a CapacityBlocked result")
		}
		if *c.RequiredMinimumTokens <= *c.AvailableTokens {
			return requestError("CapacityBlocked needs the required minimum to exceed the available tokens")
		}
	}
	if c.Outcome != nil && (*c.Outcome == "NormalCompacted" || *c.Outcome == "EmergencyCompacted") {
		if !c.Before.verified() || !c.After.verified() {
			return requestError("a committed compaction needs verified before and after budgets")
		}
		if c.CheckpointID == nil || c.SemanticBoundary == nil || c.DurableBoundary == nil {
			return requestError("a committed compaction names its checkpoint and both boundaries")
		}
		if *c.SemanticBoundary > *c.DurableBoundary {
			return requestError("semantic_boundary is beyond durable_boundary")
		}
	}
	return nil
}

// Validate checks that the origin facts of the receipt agree with its basis.
func (r IntakeReceipt) Validate() error {
	if (r.ProofDigest != nil) != (r.ProofBasis == ProofBasisVerifiedRelay) {
		return requestError("proof_digest is present exactly when proof_basis is verified_relay")
	}
	switch r.ProofBasis {
	case ProofBasisDeclaredLocal:
		if r.DeclaredOrigin != OriginHuman || r.EffectiveOrigin != OriginHuman {
			return requestError("a declared_local receipt is a human declaration accepted as human")
		}
	case ProofBasisVerifiedRelay:
		if r.EffectiveOrigin == OriginUnknown {
			return requestError("a verified relay has a known origin")
		}
	case ProofBasisAutomation:
		if r.EffectiveOrigin != OriginAutomation {
			return requestError("an automation receipt has automation as its effective origin")
		}
	case ProofBasisUnknown:
		if r.EffectiveOrigin != OriginUnknown {
			return requestError("an unknown-basis receipt has an unknown effective origin")
		}
	}
	if _, err := ParseTimestamp(r.AcceptedAt); err != nil {
		return requestError("accepted_at is not a valid timestamp")
	}
	return nil
}

// Validate checks the span PROTOCOL allows one evidence/read call to ask for.
func (e EvidenceReadInput) Validate() error {
	if e.Range.Start > e.Range.End {
		return NewError(CodeInvalidRange, "range start is after its end").Wrap(invalid("inverted range"))
	}
	if e.Range.End-e.Range.Start > MaxEvidenceReadBytes {
		return NewError(CodeInvalidRange, "range is longer than one read may return").Wrap(invalid("range too long"))
	}
	return nil
}

// Validate checks that the returned bytes are the returned range, that the range
// lies inside the evidence, and that partial says whether anything is missing.
func (e EvidenceReadResult) Validate() error {
	if e.ReturnedRange.Start > e.ReturnedRange.End || e.ReturnedRange.End > uint64(e.TotalBytes) {
		return requestError("returned_range is not inside the evidence")
	}
	span := e.ReturnedRange.End - e.ReturnedRange.Start
	if span > MaxEvidenceReadBytes {
		return requestError("returned_range is longer than one read may return")
	}
	data, err := base64.StdEncoding.DecodeString(e.DataBase64)
	if err != nil || uint64(len(data)) != span {
		return requestError("data_base64 is not the returned range")
	}
	whole := e.ReturnedRange.Start == 0 && e.ReturnedRange.End == uint64(e.TotalBytes)
	if e.Partial != (!whole || !e.CaptureComplete) {
		return requestError("partial must be true exactly when the whole evidence was not returned or capture is incomplete")
	}
	return nil
}
