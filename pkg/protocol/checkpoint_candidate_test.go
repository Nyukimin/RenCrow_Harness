package protocol_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func TestCheckpointCandidateGolden(t *testing.T) {
	var vec struct {
		CandidateSHA256 string `json:"candidate_sha256"`
		CandidateLength int    `json:"candidate_length"`
	}
	exampleJSON(t, "wire/checkpoint_vector.json", &vec)

	golden := example(t, "wire/checkpoint_candidate.bin")
	if len(golden) != vec.CandidateLength {
		t.Fatalf("golden blob is %d bytes, vector says %d", len(golden), vec.CandidateLength)
	}
	blob, err := protocol.EncodeCheckpointCandidate(example(t, "wire/checkpoint_candidate.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(blob, golden) {
		t.Fatalf("candidate bytes differ from the golden blob (got %d bytes, want %d)", len(blob), len(golden))
	}
	if got := protocol.CheckpointCandidateHash(blob); got != vec.CandidateSHA256 {
		t.Fatalf("hash %s, want %s", got, vec.CandidateSHA256)
	}
	sum := sha256.Sum256(golden)
	if hex.EncodeToString(sum[:]) != vec.CandidateSHA256 {
		t.Fatal("vector hash is not the SHA-256 of the golden blob")
	}
	if !bytes.HasPrefix(blob, []byte("rencrow-checkpoint-candidate/v1\x00{")) {
		t.Fatalf("blob must be prefix, NUL, then JSON: %q", blob[:40])
	}
	if bytes.HasSuffix(blob, []byte("\n")) || bytes.HasPrefix(blob, []byte("\xef\xbb\xbf")) {
		t.Fatal("no trailing newline and no BOM")
	}
	if err := protocol.VerifyCheckpointCandidateBlob(golden); err != nil {
		t.Fatalf("the golden blob must verify: %v", err)
	}
}

func TestCheckpointCandidateHashIsOverTheExactBytes(t *testing.T) {
	blob, _ := protocol.EncodeCheckpointCandidate(example(t, "wire/checkpoint_candidate.json"))
	other := append(append([]byte(nil), blob...), '\n')
	if protocol.CheckpointCandidateHash(blob) == protocol.CheckpointCandidateHash(other) {
		t.Fatal("hash must cover every byte")
	}
}

func TestVerifyCheckpointCandidateBlobRejectsOtherSerializations(t *testing.T) {
	golden := example(t, "wire/checkpoint_candidate.bin")
	idx := bytes.IndexByte(golden, 0)
	if idx < 0 {
		t.Fatal("golden has no NUL")
	}
	prefix, body := golden[:idx+1], golden[idx+1:]
	bad := map[string][]byte{
		"empty":             {},
		"no NUL":            bytes.Replace(golden, []byte{0}, []byte{' '}, 1),
		"wrong prefix":      append([]byte("rencrow-checkpoint-candidate/v2\x00"), body...),
		"prefix only":       prefix,
		"trailing newline":  append(append([]byte(nil), golden...), '\n'),
		"BOM before prefix": append([]byte("\xef\xbb\xbf"), golden...),
		"BOM after NUL":     append(append(append([]byte(nil), prefix...), "\xef\xbb\xbf"...), body...),
		"pretty printed":    append(append([]byte(nil), prefix...), bytes.Replace(body, []byte(`,"`), []byte(", \""), 1)...),
		"leading space":     append(append([]byte(nil), prefix...), append([]byte(" "), body...)...),
		"float spelling":    append(append([]byte(nil), prefix...), bytes.Replace(body, []byte(`"writer_epoch":1`), []byte(`"writer_epoch":1.0`), 1)...),
		"short escape":      append(append([]byte(nil), prefix...), bytes.Replace(body, []byte(`\u000a`), []byte(`\n`), -1)...),
		"truncated":         golden[:len(golden)-1],
		"two documents":     append(append([]byte(nil), golden...), body...),
		"not an object":     append(append([]byte(nil), prefix...), []byte(`[]`)...),
		"key order changed": append(append([]byte(nil), prefix...), []byte(`{"b":1,"a":2}`)...),
	}
	// The substitution cases are only meaningful if they actually changed something.
	for _, name := range []string{"float spelling", "pretty printed"} {
		if bytes.Equal(bad[name], golden) {
			t.Fatalf("%s: fixture substitution did nothing", name)
		}
	}
	for name, blob := range bad {
		if err := protocol.VerifyCheckpointCandidateBlob(blob); !errors.Is(err, protocol.ErrInvalidInput) {
			t.Errorf("%s: got %v, want ErrInvalidInput", name, err)
		}
	}
}

func TestEncodeCheckpointCandidateRejects(t *testing.T) {
	good := string(example(t, "wire/checkpoint_candidate.json"))
	cases := map[string]string{
		"empty":               "",
		"array":               `[]`,
		"hash inside payload": strings.Replace(good, `"format_version"`, `"candidate_hash":"`+strings.Repeat("0", 64)+`","format_version"`, 1),
		"duplicate key":       strings.Replace(good, `"mode"`, `"mode":"x","mode"`, 1),
		"trailing value":      good + `{}`,
		"BOM":                 "\xef\xbb\xbf" + good,
		"invalid utf8":        strings.Replace(good, "保存処理", "\xff", 1),
		"unpaired surrogate":  strings.Replace(good, "保存処理", `\ud800`, 1),
		"huge exponent":       strings.Replace(good, `"context_revision": 5`, `"context_revision": 1e5000`, 1),
	}
	for name, in := range cases {
		if in == good && name != "empty" {
			t.Fatalf("%s: fixture substitution did nothing", name)
		}
		if _, err := protocol.EncodeCheckpointCandidate([]byte(in)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
