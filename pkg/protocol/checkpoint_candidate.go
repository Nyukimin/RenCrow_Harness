package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"

	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

const checkpointCandidatePrefix = "rencrow-checkpoint-candidate/v1"

// EncodeCheckpointCandidate returns the exact bytes that are stored and hashed for
// a checkpoint candidate: UTF8("rencrow-checkpoint-candidate/v1") + 0x00 +
// CJ1(candidate). The input is the candidate's JSON text; it must be an object
// and must not carry its own hash. Store these bytes as they are: re-encoding the
// same JSON with another serializer yields different bytes and a different hash.
// Validating the candidate against its schema is a separate step.
func EncodeCheckpointCandidate(candidate []byte) ([]byte, error) {
	v, err := strictjson.Decode(candidate)
	if err != nil {
		return nil, invalidWrap(err, "checkpoint candidate")
	}
	return encodeCandidateValue(v)
}

func encodeCandidateValue(v any) ([]byte, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, invalid("checkpoint candidate must be a JSON object")
	}
	if _, has := obj["candidate_hash"]; has {
		return nil, invalid("checkpoint candidate must not contain its own hash")
	}
	cj, err := canon.Encode(obj)
	if err != nil {
		return nil, invalidWrap(err, "checkpoint candidate")
	}
	out := make([]byte, 0, len(checkpointCandidatePrefix)+1+len(cj))
	out = append(out, checkpointCandidatePrefix...)
	out = append(out, 0x00)
	return append(out, cj...), nil
}

// CheckpointCandidateHash is the lowercase hex SHA-256 of the exact stored bytes.
func CheckpointCandidateHash(blob []byte) string {
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

// VerifyCheckpointCandidateBlob is the load-side check that precedes schema and
// hash verification: the version prefix and NUL, strict JSON, and a CJ1 re-encode
// that reproduces the stored bytes exactly. A pretty-printed, newline-terminated,
// BOM-prefixed, differently ordered or float-spelled copy of the same JSON is
// refused, and an unknown version is refused rather than interpreted.
func VerifyCheckpointCandidateBlob(blob []byte) error {
	prefix, body, found := bytes.Cut(blob, []byte{0x00})
	if !found || string(prefix) != checkpointCandidatePrefix {
		return invalid("checkpoint candidate has an unknown or missing version prefix")
	}
	v, err := strictjson.Decode(body)
	if err != nil {
		return invalidWrap(err, "checkpoint candidate")
	}
	again, err := encodeCandidateValue(v)
	if err != nil {
		return err
	}
	if !bytes.Equal(again, blob) {
		return invalid("checkpoint candidate is not in canonical form")
	}
	return nil
}
