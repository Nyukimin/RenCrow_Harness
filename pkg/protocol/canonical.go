package protocol

import (
	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

// EncodeCanonicalContract (F37) returns the CJ1 canonical form of one JSON text.
//
// CJ1 is the encoding every hashed wire contract is built from: object keys in
// UTF-8 byte order at every depth, only the quote, the backslash and U+0000-U+001F
// escaped (always as lowercase \u00xx), plain decimal numbers without float
// rounding, no whitespace. It is not RFC 8785. The input is decoded strictly, so
// duplicate keys, invalid UTF-8, unpaired surrogates, a BOM, trailing data,
// nesting over 64 and out-of-range numbers are errors.
func EncodeCanonicalContract(jsonText []byte) ([]byte, error) {
	v, err := strictjson.Decode(jsonText)
	if err != nil {
		return nil, invalidWrap(err, "canonical contract")
	}
	out, err := canon.Encode(v)
	if err != nil {
		return nil, invalidWrap(err, "canonical contract")
	}
	return out, nil
}
