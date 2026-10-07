package client

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// maxFrameBytes is the largest frame the Harness sends (PROTOCOL section 1): a line
// without its newline.
const maxFrameBytes = 16 << 20

var errFrameTooLong = errors.New("a frame is over the size limit")

// readFrame reads the next line, without its newline. A line over max is an error and is
// not buffered in full. At the end of input a final line without a newline is still a
// frame; after that, io.EOF.
func readFrame(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	total := 0
	for {
		chunk, err := r.ReadSlice('\n')
		complete := err == nil
		if complete {
			chunk = chunk[:len(chunk)-1]
		}
		total += len(chunk)
		if total > max {
			return nil, errFrameTooLong
		}
		buf = append(buf, chunk...)
		switch {
		case complete:
			return buf, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if total == 0 {
				return nil, io.EOF
			}
			return buf, nil
		default:
			return nil, err
		}
	}
}

// envelope is one JSON-RPC 2.0 message of the Harness, with each member kept raw.
type envelope struct {
	jsonrpc string
	id      json.RawMessage
	method  string
	params  json.RawMessage
	result  json.RawMessage
	err     json.RawMessage
	hasID   bool
	hasErr  bool
	hasRes  bool
	hasPar  bool
	hasMeth bool
}

// parseEnvelope reads one frame as a JSON-RPC 2.0 object. It refuses what the Harness
// itself refuses on its input side: invalid UTF-8, a value that is not an object,
// a repeated or unknown member, and trailing data. The members' own content is checked
// where it is used (protocol.Decode is strict).
func parseEnvelope(frame []byte) (envelope, error) {
	var e envelope
	if !utf8.Valid(frame) {
		return e, errors.New("the frame is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(frame))
	tok, err := dec.Token()
	if err != nil {
		return e, fmt.Errorf("the frame is not JSON: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return e, errors.New("the frame is not a JSON object")
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return e, fmt.Errorf("the frame is not JSON: %w", err)
		}
		key, _ := kt.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return e, fmt.Errorf("the frame is not JSON: %w", err)
		}
		switch key {
		case "jsonrpc":
			if e.jsonrpc != "" {
				return e, errors.New("a repeated member")
			}
			if err := json.Unmarshal(raw, &e.jsonrpc); err != nil || e.jsonrpc == "" {
				return e, errors.New("jsonrpc is not a string")
			}
		case "id":
			if e.hasID {
				return e, errors.New("a repeated member")
			}
			e.hasID, e.id = true, raw
		case "method":
			if e.hasMeth {
				return e, errors.New("a repeated member")
			}
			e.hasMeth = true
			if err := json.Unmarshal(raw, &e.method); err != nil {
				return e, errors.New("method is not a string")
			}
		case "params":
			if e.hasPar {
				return e, errors.New("a repeated member")
			}
			e.hasPar, e.params = true, raw
		case "result":
			if e.hasRes {
				return e, errors.New("a repeated member")
			}
			e.hasRes, e.result = true, raw
		case "error":
			if e.hasErr {
				return e, errors.New("a repeated member")
			}
			e.hasErr, e.err = true, raw
		default:
			return e, errors.New("an unknown member")
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return e, fmt.Errorf("the frame is not JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return e, errors.New("data after the object")
	}
	if e.jsonrpc != "2.0" {
		return e, errors.New(`jsonrpc is not "2.0"`)
	}
	return e, nil
}
