// Package protocol holds the wire contract of RenCrow_Harness that other modules
// (RenCrow_CORE, the Gateway) must reproduce identically: the byte-exact digests,
// and the public request, result and event types.
//
// Byte contracts (pure functions over the design package's byte contracts):
//
//   - EncodeCanonicalContract: CJ1 canonical JSON of a JSON text.
//   - ContextRevision, TextDigest: ContextBlock revision and text digest.
//   - ParseOriginKeyFile, OriginProofMAC, VerifyOriginProofMAC, OriginProofDigest: Human relay proof.
//   - RecoveryPolicyRevision: Run recovery policy snapshot revision.
//   - MutationPayloadHash: idempotency payload identity of a native mutation.
//   - CallerProfileDigest, ValidatePrincipal: caller profile identity.
//   - InputDigest: the logical model input shared by Harness and Gateway.
//   - EncodeCheckpointCandidate, CheckpointCandidateHash, VerifyCheckpointCandidateBlob.
//
// The generic digest primitives (LP and D) are deliberately not exported: a new
// digest domain is a new wire contract and has to be added here, with a vector,
// rather than invented by a caller. request_digest is owned by the LLM Runtime;
// the Harness only echoes it and never computes it.
//
// Types (schemas/protocol.schema.json):
//
//   - The DTOs (StartInput, StartResult, RunResult, IntakeReceipt, Event, ...) use plain
//     strings for IDs and enumerations; a nullable schema field is a pointer.
//   - Decode parses one JSON value into a DTO: strict decoding first, then the JSON
//     Schema, then the type's semantic conditions (Validate) that the schema cannot
//     say, such as a user_message block in StartInput, a context block whose revision
//     is not its content's, a Run that is terminal without a result, or a passed
//     verification without evidence. Encode applies the same checks on the way out
//     and returns canonical (CJ1) bytes.
//   - DecodeRequest and Request.DecodeParams cover the 16 native methods.
//   - BuildTypedEvent (F38) builds one of the 15 Event types from a typed payload.
//
// Failures are *Error with the protocol's codes (INVALID_PARAMS for a value that does
// not satisfy the schema, INVALID_REQUEST for a semantic refusal, and so on); no
// message repeats an offending value. Every error wraps ErrInvalidInput unless
// documented.
package protocol
