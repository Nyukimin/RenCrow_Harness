package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The vector publishes the SHA-256 of the exact MAC input, which pins the byte
// layout (domain with NUL, then LP of each value in the fixed order) separately
// from the HMAC step.
func TestOriginProofMACInputBytes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contract", "examples", "origin_proof_vector.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Proof          OriginProof `json:"proof"`
		MACInputSHA256 string      `json:"mac_input_sha256"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	input, err := originProofMACInput(v.Proof)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(input)
	if got := hex.EncodeToString(sum[:]); got != v.MACInputSHA256 {
		t.Fatalf("MAC input sha256 %s, want %s", got, v.MACInputSHA256)
	}
	const prefix = "rencrow-origin-proof/v1\x00"
	if string(input[:len(prefix)]) != prefix {
		t.Fatalf("MAC input must start with the domain and NUL, got %q", input[:len(prefix)])
	}
}
