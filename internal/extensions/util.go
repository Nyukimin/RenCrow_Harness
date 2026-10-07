package extensions

import "encoding/json"

// mustJSON encodes a value built from strings, numbers, nil and slices of strings, which
// cannot fail to encode.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("extensions: a value that cannot be encoded: " + err.Error())
	}
	return b
}
