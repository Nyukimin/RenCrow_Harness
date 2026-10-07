// Package stdio is the native transport of the Harness: JSON-RPC 2.0 over standard
// input and output, one JSON object per line (NDJSON).
//
// Standard output carries protocol frames only; every diagnostic goes to a separate
// writer (standard error in production) and never contains request content, a key,
// a credential or a path. A frame is at most 16 MiB and 64 levels deep; a frame with
// a duplicate key, trailing data or invalid UTF-8 is refused before it can reach the
// Service, so nothing it carries can cause a write. Batches are not supported.
//
// The transport knows nothing about what the methods mean. It validates the
// JSON-RPC envelope, hands a typed Request to a Backend (the Service) and delivers
// what comes back: the response, then each event the call committed, as an
// event/recorded notification.
package stdio

import (
	"bufio"
	"errors"
	"io"
)

// DefaultMaxFrameBytes is the largest frame (a line without its newline).
const DefaultMaxFrameBytes = 16 << 20

// frame is one line of input. A line over the limit is reported with tooLong set
// and without its content: it is read and discarded, never buffered in full.
type frame struct {
	data    []byte
	tooLong bool
}

// readFrame reads the next line. The newline ends the frame and a single carriage
// return before it is part of the line ending, so CRLF input works and the limit
// counts the JSON only. At the end of input a final line without a newline is
// still a frame; after that, io.EOF.
func readFrame(r *bufio.Reader, max int) (frame, error) {
	var buf []byte
	total := 0
	tooLong := false
	for {
		chunk, err := r.ReadSlice('\n')
		complete := err == nil
		if complete {
			chunk = chunk[:len(chunk)-1]
		}
		if len(chunk) > 0 || complete {
			total += len(chunk)
			// One byte of slack is the carriage return of a CRLF line ending.
			if total > max+1 {
				tooLong = true
				buf = nil
			}
			if !tooLong {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case complete:
			return finish(buf, tooLong, max), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if total == 0 && len(buf) == 0 && !tooLong {
				return frame{}, io.EOF
			}
			return finish(buf, tooLong, max), nil
		default:
			return frame{}, err
		}
	}
}

func finish(buf []byte, tooLong bool, max int) frame {
	if tooLong {
		return frame{tooLong: true}
	}
	if n := len(buf); n > 0 && buf[n-1] == '\r' {
		buf = buf[:n-1]
	}
	if len(buf) > max {
		return frame{tooLong: true}
	}
	if buf == nil {
		buf = []byte{}
	}
	return frame{data: buf}
}
