// Package strictjson decodes JSON for contracts whose bytes are hashed.
//
// encoding/json is not usable for this: it replaces invalid UTF-8 and unpaired
// surrogate escapes with U+FFFD, keeps the last of duplicate keys, and gives no
// control over number text. Here a single-pass parser enforces the contract
// instead of layering checks over a second parser, so there is no parser
// differential to exploit. The grammar is RFC 8259; the extra rejections are
// duplicate keys (after unescaping), invalid UTF-8, unpaired surrogate escapes,
// a leading BOM, trailing values, nesting deeper than MaxDepth, and numbers over
// the limits in NormalizeNumber.
//
// Decoded values use only: nil, bool, string, json.Number (the original lexeme,
// already validated), []any and map[string]any.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Limits fixed by the byte contract.
const (
	// MaxDepth is the deepest array/object nesting accepted; the outermost
	// container is depth 1 and scalars add no depth.
	MaxDepth             = 64
	MaxNumberLexemeBytes = 1024
	MaxNumberExponent    = 4096 // |exponent| of the normalized integer mantissa
	MaxNumberExpandedLen = 4096 // plain decimal string, sign included
)

// Rejection reasons. Errors returned by this package wrap exactly one of them.
var (
	ErrSyntax            = errors.New("strictjson: syntax error")
	ErrInvalidUTF8       = errors.New("strictjson: invalid UTF-8")
	ErrUnpairedSurrogate = errors.New("strictjson: unpaired surrogate escape")
	ErrBOM               = errors.New("strictjson: byte order mark")
	ErrTrailingData      = errors.New("strictjson: data after the JSON value")
	ErrDuplicateKey      = errors.New("strictjson: duplicate object key")
	ErrDepth             = errors.New("strictjson: nesting too deep")
	ErrNumber            = errors.New("strictjson: number outside the allowed range")
)

func fail(reason error, offset int, detail string) error {
	if detail == "" {
		return fmt.Errorf("%w at offset %d", reason, offset)
	}
	return fmt.Errorf("%w at offset %d: %s", reason, offset, detail)
}

// Decode parses exactly one JSON value from data.
func Decode(data []byte) (any, error) {
	if bytes.HasPrefix(data, []byte("\xef\xbb\xbf")) {
		return nil, fail(ErrBOM, 0, "")
	}
	p := &parser{data: data}
	p.skipSpace()
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(data) {
		return nil, fail(ErrTrailingData, p.pos, "")
	}
	return v, nil
}

type parser struct {
	data []byte
	pos  int
}

func (p *parser) skipSpace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) value(depth int) (any, error) {
	if p.pos >= len(p.data) {
		return nil, fail(ErrSyntax, p.pos, "unexpected end of input")
	}
	switch c := p.data[p.pos]; {
	case c == '{':
		return p.object(depth + 1)
	case c == '[':
		return p.array(depth + 1)
	case c == '"':
		return p.str()
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	case c == 't':
		return p.literal("true", true)
	case c == 'f':
		return p.literal("false", false)
	case c == 'n':
		return p.literal("null", nil)
	default:
		if c >= utf8.RuneSelf {
			if r, size := utf8.DecodeRune(p.data[p.pos:]); r == utf8.RuneError && size <= 1 {
				return nil, fail(ErrInvalidUTF8, p.pos, "")
			}
		}
		return nil, fail(ErrSyntax, p.pos, "unexpected character")
	}
}

func (p *parser) literal(word string, v any) (any, error) {
	if !bytes.HasPrefix(p.data[p.pos:], []byte(word)) {
		return nil, fail(ErrSyntax, p.pos, "invalid literal")
	}
	p.pos += len(word)
	return v, nil
}

func (p *parser) array(depth int) (any, error) {
	if depth > MaxDepth {
		return nil, fail(ErrDepth, p.pos, "")
	}
	p.pos++ // [
	out := []any{}
	p.skipSpace()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		return out, nil
	}
	for {
		p.skipSpace()
		v, err := p.value(depth)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, fail(ErrSyntax, p.pos, "unterminated array")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return out, nil
		default:
			return nil, fail(ErrSyntax, p.pos, "expected , or ]")
		}
	}
}

func (p *parser) object(depth int) (any, error) {
	if depth > MaxDepth {
		return nil, fail(ErrDepth, p.pos, "")
	}
	p.pos++ // {
	out := map[string]any{}
	p.skipSpace()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		return out, nil
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return nil, fail(ErrSyntax, p.pos, "expected object key")
		}
		keyPos := p.pos
		key, err := p.str()
		if err != nil {
			return nil, err
		}
		if _, dup := out[key]; dup {
			return nil, fail(ErrDuplicateKey, keyPos, "")
		}
		p.skipSpace()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return nil, fail(ErrSyntax, p.pos, "expected :")
		}
		p.pos++
		p.skipSpace()
		v, err := p.value(depth)
		if err != nil {
			return nil, err
		}
		out[key] = v
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, fail(ErrSyntax, p.pos, "unterminated object")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return out, nil
		default:
			return nil, fail(ErrSyntax, p.pos, "expected , or }")
		}
	}
}

// str parses a string whose opening quote is at p.pos.
func (p *parser) str() (string, error) {
	p.pos++ // opening quote
	start := p.pos
	// Fast path: no escapes.
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		switch {
		case c == '"':
			s := string(p.data[start:p.pos])
			p.pos++
			return s, nil
		case c == '\\':
			return p.strSlow(start)
		case c < 0x20:
			return "", fail(ErrSyntax, p.pos, "control character in string")
		case c < utf8.RuneSelf:
			p.pos++
		default:
			if err := p.checkRune(); err != nil {
				return "", err
			}
		}
	}
	return "", fail(ErrSyntax, p.pos, "unterminated string")
}

// checkRune validates and skips one multi-byte UTF-8 sequence at p.pos.
func (p *parser) checkRune() error {
	r, size := utf8.DecodeRune(p.data[p.pos:])
	if r == utf8.RuneError && size <= 1 {
		return fail(ErrInvalidUTF8, p.pos, "")
	}
	p.pos += size
	return nil
}

func (p *parser) strSlow(start int) (string, error) {
	var b strings.Builder
	b.Write(p.data[start:p.pos])
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			return b.String(), nil
		case c < 0x20:
			return "", fail(ErrSyntax, p.pos, "control character in string")
		case c == '\\':
			p.pos++
			if p.pos >= len(p.data) {
				return "", fail(ErrSyntax, p.pos, "unterminated escape")
			}
			esc := p.data[p.pos]
			p.pos++
			switch esc {
			case '"', '\\', '/':
				b.WriteByte(esc)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				r, err := p.unicodeEscape()
				if err != nil {
					return "", err
				}
				b.WriteRune(r)
			default:
				return "", fail(ErrSyntax, p.pos-1, "invalid escape")
			}
		case c < utf8.RuneSelf:
			b.WriteByte(c)
			p.pos++
		default:
			from := p.pos
			if err := p.checkRune(); err != nil {
				return "", err
			}
			b.Write(p.data[from:p.pos])
		}
	}
	return "", fail(ErrSyntax, p.pos, "unterminated string")
}

// unicodeEscape reads the four hex digits after \u (p.pos is just past the 'u')
// and, for a high surrogate, the mandatory \uXXXX low surrogate that follows.
func (p *parser) unicodeEscape() (rune, error) {
	at := p.pos - 2
	u, ok := p.hex4()
	if !ok {
		return 0, fail(ErrSyntax, at, "invalid \\u escape")
	}
	switch {
	case u >= 0xDC00 && u <= 0xDFFF:
		return 0, fail(ErrUnpairedSurrogate, at, "")
	case u >= 0xD800 && u <= 0xDBFF:
		if p.pos+1 < len(p.data) && p.data[p.pos] == '\\' && p.data[p.pos+1] == 'u' {
			save := p.pos
			p.pos += 2
			lo, ok := p.hex4()
			if ok && lo >= 0xDC00 && lo <= 0xDFFF {
				return 0x10000 + (u-0xD800)<<10 + (lo - 0xDC00), nil
			}
			p.pos = save
		}
		return 0, fail(ErrUnpairedSurrogate, at, "")
	}
	return u, nil
}

func (p *parser) hex4() (rune, bool) {
	if p.pos+4 > len(p.data) {
		return 0, false
	}
	var u rune
	for i := 0; i < 4; i++ {
		c := p.data[p.pos+i]
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		u = u<<4 | rune(d)
	}
	p.pos += 4
	return u, true
}

func (p *parser) number() (any, error) {
	end, ok := scanNumber(p.data, p.pos)
	if !ok {
		return nil, fail(ErrSyntax, p.pos, "invalid number")
	}
	if end < len(p.data) {
		// A number must end at a delimiter: reject 01, 0x10, 1.5.2, 1e5e, 1-2.
		if c := p.data[end]; c == '.' || c == '+' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			return nil, fail(ErrSyntax, end, "invalid number")
		}
	}
	lexeme := string(p.data[p.pos:end])
	if _, err := NormalizeNumber(lexeme); err != nil {
		if errors.Is(err, ErrNumber) {
			return nil, fail(ErrNumber, p.pos, "")
		}
		return nil, fail(ErrSyntax, p.pos, "invalid number")
	}
	p.pos = end
	return json.Number(lexeme), nil
}

// scanNumber returns the end of the JSON number starting at pos, per the RFC 8259
// grammar: -? (0 | [1-9][0-9]*) (. [0-9]+)? ([eE] [+-]? [0-9]+)?
func scanNumber[T ~string | ~[]byte](s T, pos int) (int, bool) {
	i := pos
	if i < len(s) && s[i] == '-' {
		i++
	}
	if i >= len(s) {
		return 0, false
	}
	switch {
	case s[i] == '0':
		i++
	case s[i] >= '1' && s[i] <= '9':
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	default:
		return 0, false
	}
	if i < len(s) && s[i] == '.' {
		i++
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return 0, false
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return 0, false
		}
	}
	return i, true
}

// NormalizeNumber returns the plain decimal form of a JSON number lexeme:
// no exponent, no trailing fraction zeros, no needless decimal point, and "0" for
// every zero including -0. The value is never converted to a float, and limits are
// decided by arithmetic on the digit counts before anything is expanded.
//
// A lexeme over MaxNumberLexemeBytes, a normalized mantissa exponent whose
// absolute value is over MaxNumberExponent, or an expanded string (sign included)
// over MaxNumberExpandedLen wraps ErrNumber. A lexeme that is not a JSON number
// wraps ErrSyntax.
func NormalizeNumber(lexeme string) (string, error) {
	if len(lexeme) > MaxNumberLexemeBytes {
		return "", fmt.Errorf("%w: lexeme is %d bytes, limit %d", ErrNumber, len(lexeme), MaxNumberLexemeBytes)
	}
	end, ok := scanNumber(lexeme, 0)
	if !ok || end != len(lexeme) {
		return "", fmt.Errorf("%w: not a JSON number", ErrSyntax)
	}
	neg := lexeme[0] == '-'
	rest := strings.TrimPrefix(lexeme, "-")
	mantissa, expText, _ := strings.Cut(strings.ToLower(rest), "e")
	intPart, fracPart, _ := strings.Cut(mantissa, ".")

	// Exponent literal, saturated so that hundreds of digits are never converted.
	exp := 0
	if expText != "" {
		expNeg := expText[0] == '-'
		digits := strings.TrimLeft(strings.TrimLeft(expText, "+-"), "0")
		if len(digits) > 7 {
			exp = 10_000_000
		} else {
			for i := 0; i < len(digits); i++ {
				exp = exp*10 + int(digits[i]-'0')
			}
		}
		if expNeg {
			exp = -exp
		}
	}

	// value = digits x 10^e with digits an integer; |e| and lengths stay small.
	digits := intPart + fracPart
	e := exp - len(fracPart)
	lead := len(digits) - len(strings.TrimLeft(digits, "0"))
	digits = digits[lead:]
	if digits == "" { // zero
		if abs(e) > MaxNumberExponent {
			return "", fmt.Errorf("%w: exponent %d outside +-%d", ErrNumber, e, MaxNumberExponent)
		}
		return "0", nil
	}
	trimmed := strings.TrimRight(digits, "0")
	e += len(digits) - len(trimmed)
	digits = trimmed
	if abs(e) > MaxNumberExponent {
		return "", fmt.Errorf("%w: exponent %d outside +-%d", ErrNumber, e, MaxNumberExponent)
	}

	n := len(digits)
	var length int
	switch {
	case e >= 0:
		length = n + e
	case -e < n:
		length = n + 1
	default:
		length = 2 + (-e)
	}
	if neg {
		length++
	}
	if length > MaxNumberExpandedLen {
		return "", fmt.Errorf("%w: expanded form is %d characters, limit %d", ErrNumber, length, MaxNumberExpandedLen)
	}

	var b strings.Builder
	b.Grow(length)
	if neg {
		b.WriteByte('-')
	}
	switch {
	case e >= 0:
		b.WriteString(digits)
		b.WriteString(strings.Repeat("0", e))
	case -e < n:
		b.WriteString(digits[:n+e])
		b.WriteByte('.')
		b.WriteString(digits[n+e:])
	default:
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", -e-n))
		b.WriteString(digits)
	}
	return b.String(), nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
