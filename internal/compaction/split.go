package compaction

import "unicode/utf8"

// Byte bounds of the stage data (STAGE_DATA). They are UTF-8 byte counts: the schema's
// maxLength counts characters, so none of them is enforced by a schema alone.
const (
	// MaxPieceBytes is the most text one Piece (a presented instruction or a Work item)
	// holds; longer text is split.
	MaxPieceBytes = 8000
	// MaxLinkBytes is the bound of each of a completion's call text and output text. It
	// is a bound on each of the two, never on their sum.
	MaxLinkBytes = 2048
	// WholeExcerptBytes is the size up to which an Observation is presented as one
	// excerpt; above it the head and the tail are presented, EdgeExcerptBytes each at
	// most.
	WholeExcerptBytes = 2048
	EdgeExcerptBytes  = 1024
)

// Chunk is one piece of a split text with the byte range of the text it covers.
type Chunk struct {
	Text       string
	Start, End int
}

// SplitText cuts text into chunks of at most limit UTF-8 bytes (STAGE_DATA section 1):
// each chunk is the longest prefix of what is left, moved back to the start of a
// character only when the cut would fall inside one. A newline is not a better place to
// cut. The empty text is one empty chunk, and the chunks joined are the text, byte for
// byte. limit must be at least 4, the size of the largest character.
func SplitText(text string, limit int) []Chunk {
	if limit < utf8.UTFMax {
		panic("compaction: a split limit under four bytes cannot hold a character")
	}
	if text == "" {
		return []Chunk{{}}
	}
	var out []Chunk
	for start := 0; start < len(text); {
		end := min(start+limit, len(text))
		for end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		out = append(out, Chunk{Text: text[start:end], Start: start, End: end})
		start = end
	}
	return out
}

// Edges returns the excerpts of a text for the Summary data: the whole text when it is
// at most WholeExcerptBytes, otherwise its head and its tail of at most EdgeExcerptBytes
// each, cut inward to character boundaries. Above WholeExcerptBytes the two can never
// touch, so there is no overlap to merge. Each excerpt is the text of its byte range.
func Edges(text string) []Chunk {
	n := len(text)
	if n <= WholeExcerptBytes {
		return []Chunk{{Text: text, Start: 0, End: n}}
	}
	headEnd := EdgeExcerptBytes
	for headEnd > 0 && !utf8.RuneStart(text[headEnd]) {
		headEnd--
	}
	tailStart := n - EdgeExcerptBytes
	for tailStart < n && !utf8.RuneStart(text[tailStart]) {
		tailStart++
	}
	return []Chunk{{Text: text[:headEnd], Start: 0, End: headEnd}, {Text: text[tailStart:], Start: tailStart, End: n}}
}
