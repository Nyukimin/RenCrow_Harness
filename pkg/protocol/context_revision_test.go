package protocol_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

type revisionBlock struct {
	Kind     string `json:"kind"`
	Text     string `json:"text"`
	Revision string `json:"revision"`
}

func TestContextRevisionGolden(t *testing.T) {
	var vectors []struct {
		Block              revisionBlock `json:"block"`
		ComputedTextDigest string        `json:"computed_text_digest"`
	}
	exampleJSON(t, "context_revision_vectors.json", &vectors)
	if len(vectors) != 5 {
		t.Fatalf("expected 5 vectors, got %d", len(vectors))
	}
	seen := map[string]bool{}
	for i, v := range vectors {
		rev, err := protocol.ContextRevision(v.Block.Kind, v.Block.Text, nil)
		if err != nil || rev != v.Block.Revision {
			t.Errorf("vector %d revision: got %s, %v; want %s", i, rev, err, v.Block.Revision)
		}
		dig, err := protocol.TextDigest(v.Block.Text)
		if err != nil || dig != v.ComputedTextDigest {
			t.Errorf("vector %d text digest: got %s, %v; want %s", i, dig, err, v.ComputedTextDigest)
		}
		seen[rev] = true
	}
	if len(seen) != 5 {
		t.Fatal("whitespace, newline and Unicode-form variants must all differ")
	}

	// The four CORE blocks of the start examples.
	var start struct {
		ContextBlocks []revisionBlock `json:"context_blocks"`
	}
	exampleJSON(t, "core_start.json", &start)
	if len(start.ContextBlocks) != 4 {
		t.Fatalf("expected 4 CORE blocks, got %d", len(start.ContextBlocks))
	}
	for _, b := range start.ContextBlocks {
		if rev, err := protocol.ContextRevision(b.Kind, b.Text, nil); err != nil || rev != b.Revision {
			t.Errorf("%s: got %s, %v; want %s", b.Kind, rev, err, b.Revision)
		}
	}
}

func TestContextRevisionNFCAndNFDDiffer(t *testing.T) {
	a, _ := protocol.ContextRevision("stable_runtime_context", nfc(), nil)
	b, _ := protocol.ContextRevision("stable_runtime_context", nfd(), nil)
	if a == b {
		t.Fatal("Unicode normalization must not be applied")
	}
}

func fixtureSource() *protocol.SourceRef {
	return &protocol.SourceRef{
		Owner: "RenCrow_CORE", SourceID: "msg_00000000-0000-7000-8000-000000000101",
		RawHash:           "5eba7f706ae54dc35ff984acac2be22f04c257a958a1af6722ac29ca29aabf8d",
		ProjectionVersion: "text/v1", Range: protocol.ByteRange{Start: 0, End: 30}, Origin: "human", Sequence: 1,
	}
}

func TestContextRevisionWithSource(t *testing.T) {
	// The design package has no golden for the source branch. This value was assembled
	// independently in Python from the formula in CORE_INTEGRATION section 1.
	const want = "ctx-v1:b5196983991f20fadd10bb49eadfb279d3e372b77dadf1d7df0772db3838d3e4"
	got, err := protocol.ContextRevision("recall_pack", "過去の記録\n", fixtureSource())
	if err != nil || got != want {
		t.Fatalf("got %s, %v; want %s", got, err, want)
	}
	mutations := map[string]func(s *protocol.SourceRef){
		"owner":              func(s *protocol.SourceRef) { s.Owner += "x" },
		"source_id":          func(s *protocol.SourceRef) { s.SourceID += "x" },
		"raw_hash":           func(s *protocol.SourceRef) { s.RawHash = strings.Repeat("0", 64) },
		"projection_version": func(s *protocol.SourceRef) { s.ProjectionVersion = "text/v2" },
		"range.start":        func(s *protocol.SourceRef) { s.Range.Start++ },
		"range.end":          func(s *protocol.SourceRef) { s.Range.End++ },
		"origin":             func(s *protocol.SourceRef) { s.Origin = "agent" },
		"sequence":           func(s *protocol.SourceRef) { s.Sequence++ },
	}
	for name, mutate := range mutations {
		s := *fixtureSource()
		mutate(&s)
		rev, err := protocol.ContextRevision("recall_pack", "過去の記録\n", &s)
		if err != nil || rev == got {
			t.Errorf("changing %s must change the revision (got %s, %v)", name, rev, err)
		}
	}
}

func TestContextRevisionNullSourceIsNotEmptySource(t *testing.T) {
	none, _ := protocol.ContextRevision("recall_pack", "x", nil)
	empty, err := protocol.ContextRevision("recall_pack", "x", &protocol.SourceRef{})
	if err != nil || none == empty {
		t.Fatalf("NULL tag and an all-empty source must differ: %s %s %v", none, empty, err)
	}
}

func TestContextRevisionFieldBoundaries(t *testing.T) {
	a, _ := protocol.ContextRevision("ab", "c", nil)
	b, _ := protocol.ContextRevision("a", "bc", nil)
	if a == b {
		t.Fatal("kind and text boundaries must be length prefixed")
	}
}

func TestContextRevisionRejectsInvalidUTF8(t *testing.T) {
	if _, err := protocol.ContextRevision("k", "a\xffb", nil); err == nil {
		t.Error("invalid UTF-8 text accepted")
	}
	if _, err := protocol.ContextRevision("k\xff", "a", nil); err == nil {
		t.Error("invalid UTF-8 kind accepted")
	}
	s := fixtureSource()
	s.Owner = "o\xff"
	if _, err := protocol.ContextRevision("k", "a", s); err == nil {
		t.Error("invalid UTF-8 source accepted")
	}
	if _, err := protocol.TextDigest("a\xff"); !errors.Is(err, protocol.ErrInvalidInput) {
		t.Errorf("text digest of invalid UTF-8: %v", err)
	}
}

func TestTextDigestIsPlainSHA256(t *testing.T) {
	// SHA-256 of the empty string.
	got, err := protocol.TextDigest("")
	if err != nil || got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("got %s, %v", got, err)
	}
}
