package protocol

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// request_vector.json publishes the exact CJ1 bytes of the LogicalInput, so the
// field set and extraction are pinned apart from the hashing step.
func TestLogicalInputBytes(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "contract", "examples", "wire")
	vecRaw, err := os.ReadFile(filepath.Join(dir, "request_vector.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vec struct {
		LogicalInputCJHex string `json:"logical_input_cj_hex"`
	}
	if err := json.Unmarshal(vecRaw, &vec); err != nil {
		t.Fatal(err)
	}
	reqRaw, err := os.ReadFile(filepath.Join(dir, "generation_request.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := logicalInputCJ1(reqRaw)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != vec.LogicalInputCJHex {
		t.Fatalf("logical input bytes differ\n got %x\nwant %s", got, vec.LogicalInputCJHex)
	}
}
