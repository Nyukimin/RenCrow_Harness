package compaction_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
)

func wirePath(t testing.TB, name string) string {
	t.Helper()
	return filepath.Join(harnesstest.ModuleRoot(t), "testdata", "contract", "examples", "wire", name)
}

func readWireFile(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(wirePath(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSplitMatchesTheDesignVectors holds SplitText to the design's golden: the empty
// text, an exact fit, a cut inside a three-byte and a four-byte character.
func TestSplitMatchesTheDesignVectors(t *testing.T) {
	var vectors []struct {
		Name             string   `json:"name"`
		Input            string   `json:"input"`
		ChunkByteLengths []int    `json:"chunk_byte_lengths"`
		ChunkSHA256      []string `json:"chunk_sha256"`
	}
	if err := json.Unmarshal(readWireFile(t, "split_vectors.json"), &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) < 5 {
		t.Fatalf("%d vectors", len(vectors))
	}
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			chunks := compaction.SplitText(v.Input, compaction.MaxPieceBytes)
			if len(chunks) != len(v.ChunkByteLengths) {
				t.Fatalf("%d chunks, want %d", len(chunks), len(v.ChunkByteLengths))
			}
			var joined strings.Builder
			for i, c := range chunks {
				if len(c.Text) != v.ChunkByteLengths[i] {
					t.Errorf("chunk %d is %d bytes, want %d", i, len(c.Text), v.ChunkByteLengths[i])
				}
				sum := sha256.Sum256([]byte(c.Text))
				if hex.EncodeToString(sum[:]) != v.ChunkSHA256[i] {
					t.Errorf("chunk %d has another digest", i)
				}
				if !utf8.ValidString(c.Text) || c.End-c.Start != len(c.Text) || v.Input[c.Start:c.End] != c.Text {
					t.Errorf("chunk %d is not the text of its range", i)
				}
				joined.WriteString(c.Text)
			}
			if joined.String() != v.Input {
				t.Fatal("the chunks joined are not the input")
			}
		})
	}
}

func TestSplitNeverCutsInsideACharacterAndIgnoresNewlines(t *testing.T) {
	text := strings.Repeat("a\n", 3999) + "日" + strings.Repeat("b", 50) // 日 occupies bytes 7998..8001
	chunks := compaction.SplitText(text, compaction.MaxPieceBytes)
	if len(chunks) != 2 || len(chunks[0].Text) != 7998 || !strings.HasPrefix(chunks[1].Text, "日") {
		t.Fatalf("%d chunks", len(chunks))
	}
	// The cut is where the character begins, not at a line chosen for it.
	if !strings.HasSuffix(chunks[0].Text, "\n") {
		t.Fatal("the first chunk must end where the character begins")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a limit under four bytes must not be accepted")
		}
	}()
	compaction.SplitText("x", 3)
}

// TestEdgesPresentWholeTextOrItsTwoEdges is the Observation excerpt rule: up to 2,048
// bytes the whole text, above it at most 1,024 bytes of the head and of the tail, each
// cut inward to a character boundary.
func TestEdgesPresentWholeTextOrItsTwoEdges(t *testing.T) {
	whole := strings.Repeat("x", compaction.WholeExcerptBytes)
	if got := compaction.Edges(whole); len(got) != 1 || got[0].Text != whole || got[0].Start != 0 || got[0].End != 2048 {
		t.Fatalf("2,048 bytes are one excerpt: %+v", got)
	}
	over := strings.Repeat("x", 2049)
	got := compaction.Edges(over)
	if len(got) != 2 || len(got[0].Text) != 1024 || len(got[1].Text) != 1024 || got[0].Start != 0 || got[1].End != 2049 || got[1].Start != 1025 {
		t.Fatalf("2,049 bytes are two excerpts: %+v", got)
	}
	// Japanese: three-byte characters. 1,024 is not a multiple of three, so each edge
	// gives up bytes toward the middle and never halves a character.
	ja := strings.Repeat("あ", 2000) // 6000 bytes
	edges := compaction.Edges(ja)
	if len(edges) != 2 {
		t.Fatalf("%d excerpts", len(edges))
	}
	for i, e := range edges {
		if !utf8.ValidString(e.Text) || len(e.Text) > compaction.EdgeExcerptBytes || ja[e.Start:e.End] != e.Text {
			t.Errorf("excerpt %d: %d bytes, valid=%v", i, len(e.Text), utf8.ValidString(e.Text))
		}
	}
	if len(edges[0].Text) != 1023 || len(edges[1].Text) != 1023 || edges[1].End != 6000 {
		t.Errorf("head %d, tail %d bytes", len(edges[0].Text), len(edges[1].Text))
	}
}
