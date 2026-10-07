// Package canon implements the CJ1 canonical JSON encoding and the LP / D
// digest primitives from the byte contract.
//
// CJ1 is not RFC 8785. Object keys are ordered by their UTF-8 bytes at every
// depth, strings escape only the quote, the backslash and U+0000-U+001F (always
// as lowercase \u00xx), numbers are plain decimals with no float rounding, and
// there is no whitespace. Nothing is normalized, trimmed or converted.
//
// Values are the decoded-JSON model of internal/strictjson (nil, bool, string,
// json.Number, []any, map[string]any) plus int, int64 and uint64 so callers can
// build values without formatting numbers by hand. Floats and every other type
// are refused rather than rounded or reflected.
package canon

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

var (
	// ErrUnsupported is returned for a value outside the JSON value model.
	ErrUnsupported = errors.New("canon: unsupported value type")
	// ErrInvalidString is returned for a string or key that is not valid UTF-8.
	ErrInvalidString = errors.New("canon: string is not valid UTF-8")
)

// Encode returns the CJ1 bytes of v.
func Encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encode(&buf, v, 0); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encode(buf *bytes.Buffer, v any, depth int) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		return writeString(buf, x)
	case json.Number:
		n, err := strictjson.NormalizeNumber(string(x))
		if err != nil {
			return err
		}
		buf.WriteString(n)
	case int:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		buf.WriteString(strconv.FormatInt(x, 10))
	case uint64:
		buf.WriteString(strconv.FormatUint(x, 10))
	case []any:
		if depth+1 > strictjson.MaxDepth {
			return fmt.Errorf("%w: arrays and objects nest deeper than %d", strictjson.ErrDepth, strictjson.MaxDepth)
		}
		buf.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encode(buf, item, depth+1); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		if depth+1 > strictjson.MaxDepth {
			return fmt.Errorf("%w: arrays and objects nest deeper than %d", strictjson.ErrDepth, strictjson.MaxDepth)
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // Go compares strings bytewise: UTF-8 byte order
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := encode(buf, x[k], depth+1); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("%w: %T", ErrUnsupported, v)
	}
	return nil
}

const hexDigits = "0123456789abcdef"

func writeString(buf *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return ErrInvalidString
	}
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			buf.WriteString(`\"`)
		case c == '\\':
			buf.WriteString(`\\`)
		case c < 0x20:
			buf.WriteString(`\u00`)
			buf.WriteByte(hexDigits[c>>4])
			buf.WriteByte(hexDigits[c&0x0f])
		default:
			buf.WriteByte(c)
		}
	}
	buf.WriteByte('"')
	return nil
}

// LP is the 8-byte big-endian byte length of b followed by b.
func LP(b []byte) []byte {
	out := make([]byte, 8, 8+len(b))
	binary.BigEndian.PutUint64(out, uint64(len(b)))
	return append(out, b...)
}

// D is lowercase_hex(SHA-256(LP(domain) + LP(CJ1(v1)) + ... + LP(CJ1(vn)))).
// The domain is plain UTF-8 text with no NUL appended. Every value is length
// prefixed on its own, so value boundaries are part of the digest.
func D(domain string, values ...any) (string, error) {
	if !utf8.ValidString(domain) {
		return "", ErrInvalidString
	}
	buf := LP([]byte(domain))
	for _, v := range values {
		enc, err := Encode(v)
		if err != nil {
			return "", err
		}
		buf = append(buf, LP(enc)...)
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}
