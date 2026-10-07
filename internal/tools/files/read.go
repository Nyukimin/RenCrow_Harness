package files

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"os"
	"unicode/utf8"

	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolerr"
)

// Range is a half-open byte range [Start, End).
type Range struct {
	Start uint64 `json:"start"`
	End   uint64 `json:"end"`
}

// ReadArgs are the arguments of file.read.
type ReadArgs struct {
	Path     string
	Range    Range
	MaxBytes int64
}

// ReadResult is the result of file.read: the hash and size of the whole file, the
// range actually returned, and whether it is less than the whole.
type ReadResult struct {
	Path          string `json:"path"`
	TotalBytes    int64  `json:"total_bytes"`
	RawHash       string `json:"raw_hash"`
	ReturnedRange Range  `json:"returned_range"`
	Partial       bool   `json:"partial"`
	// Encoding is utf-8 when the returned bytes are valid UTF-8 and base64 otherwise,
	// so the bytes are never altered to fit a text field.
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

// ReadFile is file.read. The raw hash is the SHA-256 of the whole file's bytes, the
// value file.edit takes as expected_hash.
func ReadFile(ctx context.Context, sc Scope, a ReadArgs) (ReadResult, error) {
	if a.Range.Start > a.Range.End {
		return ReadResult{}, toolerr.Reject(toolerr.CodeInvalidRange, "the range starts after it ends")
	}
	if a.MaxBytes < 1 {
		return ReadResult{}, toolerr.Reject(toolerr.CodeInvalidRange, "max_bytes must be at least 1")
	}
	r, f, info, err := sc.openExisting(a.Path, Read)
	if err != nil {
		return ReadResult{}, err
	}
	defer f.Close()
	total := info.Size()
	if total > MaxFileBytes {
		return ReadResult{}, toolerr.Fail(toolerr.CodeFileTooLarge, "the file is larger than file.read handles")
	}
	if a.Range.Start > uint64(total) {
		return ReadResult{}, toolerr.Fail(toolerr.CodeInvalidRange, "the range starts past the end of the file")
	}
	start := int64(a.Range.Start)
	end := min(int64(min(a.Range.End, uint64(total))), start+a.MaxBytes)

	h := sha256.New()
	buf := make([]byte, 0, end-start)
	chunk := make([]byte, 256<<10)
	var pos int64
	for {
		if err := ctx.Err(); err != nil {
			return ReadResult{}, toolerr.Fail(toolerr.CodeCancelled, "the read was stopped")
		}
		n, rerr := f.Read(chunk)
		if n > 0 {
			h.Write(chunk[:n])
			lo, hi := max(start-pos, 0), min(end-pos, int64(n))
			if lo < hi {
				buf = append(buf, chunk[lo:hi]...)
			}
			pos += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return ReadResult{}, toolerr.Fail(toolerr.CodeIO, "the file could not be read")
		}
	}
	if pos != total {
		// The size changed while it was read: the hash would describe neither.
		return ReadResult{}, toolerr.Fail(toolerr.CodeIO, "the file changed while it was being read")
	}
	res := ReadResult{
		Path: r.Rel, TotalBytes: total, RawHash: hex.EncodeToString(h.Sum(nil)),
		ReturnedRange: Range{Start: uint64(start), End: uint64(end)},
		Partial:       !(start == 0 && end == total),
	}
	if utf8.Valid(buf) {
		res.Encoding, res.Content = "utf-8", string(buf)
	} else {
		res.Encoding, res.Content = "base64", base64.StdEncoding.EncodeToString(buf)
	}
	return res, nil
}

// readWhole opens the regular file at a path (see openExisting) and reads it in full, up
// to limit bytes, and returns where it is, its bytes and their SHA-256.
func readWhole(sc Scope, p string, a Access, limit int64) (Resolved, []byte, string, os.FileInfo, error) {
	r, f, info, err := sc.openExisting(p, a)
	if err != nil {
		return Resolved{}, nil, "", nil, err
	}
	defer f.Close()
	if info.Size() > limit {
		return Resolved{}, nil, "", nil, toolerr.Fail(toolerr.CodeFileTooLarge, "the file is larger than this Tool handles")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return Resolved{}, nil, "", nil, toolerr.Fail(toolerr.CodeIO, "the file could not be read")
	}
	if int64(len(data)) != info.Size() {
		return Resolved{}, nil, "", nil, toolerr.Fail(toolerr.CodeIO, "the file changed while it was being read")
	}
	sum := sha256.Sum256(data)
	return r, data, hex.EncodeToString(sum[:]), info, nil
}

// ReadWhole reads the regular file at a workspace-relative path in full, up to limit bytes,
// with the same containment as file.read (a component that is a link, or is spelled
// differently from the name on disk, is refused; the open is checked against what was
// inspected). It returns the normalized path, the bytes and their SHA-256. A file over limit
// is CodeFileTooLarge. It is for the host's own reading of files it is told about (the
// AGENTS.md and SKILL.md assets), not a Tool.
func ReadWhole(sc Scope, p string, limit int64) (string, []byte, string, error) {
	r, data, sum, _, err := readWhole(sc, p, Read, limit)
	if err != nil {
		return "", nil, "", err
	}
	return r.Rel, data, sum, nil
}
