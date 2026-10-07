package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
)

// ByteRange is a half-open UTF-8 byte range.
type ByteRange struct {
	Start uint64 `json:"start"`
	End   uint64 `json:"end"`
}

// SourceRef names where a piece of context came from. Its fields are part of a
// ContextBlock's revision when present.
type SourceRef struct {
	Owner             string    `json:"owner"`
	SourceID          string    `json:"source_id"`
	RawHash           string    `json:"raw_hash"`
	ProjectionVersion string    `json:"projection_version"`
	Range             ByteRange `json:"range"`
	Origin            string    `json:"origin"`
	Sequence          uint64    `json:"sequence"`
}

// ContextRevision returns the content-addressed revision of one ContextBlock:
//
//	"ctx-v1:" + hex(SHA256("rencrow-context-block/v1\0" + LP(kind) + LP(text) + SOURCE(source)))
//
// A nil source is the single byte 0x00, which is not the same as an all-empty
// SourceRef (0x01 followed by its fields). The text is hashed exactly as given:
// no trimming, newline conversion or Unicode normalization. The revision is a
// content address, not a version order and not proof of who authored the text.
func ContextRevision(kind, text string, source *SourceRef) (string, error) {
	if err := checkUTF8("context block kind and text", kind, text); err != nil {
		return "", err
	}
	var b bytes.Buffer
	b.WriteString("rencrow-context-block/v1\x00")
	b.Write(canon.LP([]byte(kind)))
	b.Write(canon.LP([]byte(text)))
	if source == nil {
		b.WriteByte(0x00)
	} else {
		if err := checkUTF8("context block source", source.Owner, source.SourceID, source.RawHash, source.ProjectionVersion, source.Origin); err != nil {
			return "", err
		}
		b.WriteByte(0x01)
		for _, s := range []string{
			source.Owner, source.SourceID, source.RawHash, source.ProjectionVersion,
			strconv.FormatUint(source.Range.Start, 10), strconv.FormatUint(source.Range.End, 10),
			source.Origin, strconv.FormatUint(source.Sequence, 10),
		} {
			b.Write(canon.LP([]byte(s)))
		}
	}
	sum := sha256.Sum256(b.Bytes())
	return "ctx-v1:" + hex.EncodeToString(sum[:]), nil
}

// TextDigest is the lowercase hex SHA-256 of the UTF-8 text, with no prefix. It is
// the Host's own text_digest of a ContextBlock; SourceRef.raw_hash is the hash of
// the whole original and is not comparable to it.
func TextDigest(text string) (string, error) {
	if err := checkUTF8("text", text); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:]), nil
}
