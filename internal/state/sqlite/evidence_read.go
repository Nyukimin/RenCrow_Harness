package sqlite

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// evidenceReadable decides whether the caller may read one Evidence: it is the
// caller's own, or it belongs to a Thread (through an item, or through the Run it
// was captured for) whose session owner the caller may read. An Evidence that
// belongs to no readable Thread and to another principal is not readable, and is
// not distinguishable from one that does not exist.
func (s *Store) evidenceReadable(ctx context.Context, caller intake.Caller, evidenceID, principal string) (bool, error) {
	if caller.Principal == principal {
		return true, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.principal FROM items i JOIN threads t ON t.thread_id=i.thread_id JOIN sessions s ON s.session_id=t.session_id WHERE i.evidence_id=?
		UNION
		SELECT s.principal FROM evidence e JOIN runs r ON r.run_id=e.run_id JOIN tasks tk ON tk.task_id=r.task_id
			JOIN threads t ON t.thread_id=tk.thread_id JOIN sessions s ON s.session_id=t.session_id WHERE e.evidence_id=?`, evidenceID, evidenceID)
	if err != nil {
		return false, wrapRead("read evidence owner", err)
	}
	defer rows.Close()
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return false, err
		}
		if caller.CanRead(owner) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ReadEvidence (evidence/read) returns one byte range of a sealed Evidence, at most
// 64 KiB, with the hashes and size of the whole. It reads stored bytes only: no
// Tool is run and nothing is written.
//
// raw/v1 is the stored bytes. text/v1 is available only where the stored bytes are
// themselves the text projection (a single text part, as every Evidence written so
// far is); its range must start and end on UTF-8 character boundaries, otherwise
// INVALID_RANGE. An Evidence that is still being captured is BUSY and retryable:
// its size and hashes do not exist yet. An incomplete capture is returned with
// partial=true even when every byte is.
func (s *Store) ReadEvidence(ctx context.Context, caller intake.Caller, in protocol.EvidenceReadInput) (protocol.EvidenceReadResult, error) {
	return s.readEvidence(ctx, in, func(principal string) (bool, error) {
		return s.evidenceReadable(ctx, caller, in.EvidenceID, principal)
	})
}

// evidenceInThread reports whether an Evidence belongs to the Thread: it is the text of
// one of its items, the Evidence of one of its Runs, or a source its fork imported.
func (s *Store) evidenceInThread(ctx context.Context, evidenceID, threadID string) (bool, error) {
	return evidenceInScope(ctx, s.db, evidenceID, threadID)
}

// ReadEvidenceInThread is the evidence.read Tool's read: the same bytes, ranges and
// hashes as evidence/read, for an Evidence of this Thread only. It is narrower than
// the method (which reads whatever the caller's profile may read): what a model asks
// for through a Tool is confined to the work it is doing. An Evidence of another Thread
// and one that does not exist are the same FORBIDDEN.
func (s *Store) ReadEvidenceInThread(ctx context.Context, threadID string, in protocol.EvidenceReadInput) (protocol.EvidenceReadResult, error) {
	return s.readEvidence(ctx, in, func(string) (bool, error) { return s.evidenceInThread(ctx, in.EvidenceID, threadID) })
}

func (s *Store) readEvidence(ctx context.Context, in protocol.EvidenceReadInput, readable func(principal string) (bool, error)) (protocol.EvidenceReadResult, error) {
	if err := in.Validate(); err != nil {
		return protocol.EvidenceReadResult{}, err
	}
	var principal, state, media string
	var complete, total sql.NullInt64
	var rawHash, projVersion, projHash sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT principal, state, media_type, capture_complete, total_bytes, raw_hash, projection_version, projection_hash
		FROM evidence WHERE evidence_id=?`, in.EvidenceID).Scan(&principal, &state, &media, &complete, &total, &rawHash, &projVersion, &projHash)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.EvidenceReadResult{}, forbidden()
	}
	if err != nil {
		return protocol.EvidenceReadResult{}, wrapRead("read evidence", err)
	}
	ok, err := readable(principal)
	if err != nil {
		return protocol.EvidenceReadResult{}, err
	}
	if !ok {
		return protocol.EvidenceReadResult{}, forbidden()
	}
	if state != "sealed" {
		return protocol.EvidenceReadResult{}, protocol.NewError(protocol.CodeBusy, "the evidence is still being captured").AsRetryable()
	}
	if !total.Valid || !rawHash.Valid || !complete.Valid {
		return protocol.EvidenceReadResult{}, protocol.NewError(protocol.CodeIntegrityBlocked, "a sealed evidence has no size or hash")
	}

	textProjection := false
	switch in.ProjectionVersion {
	case "raw/v1":
	case "text/v1":
		// The stored bytes are the text projection only when the store says so.
		if !projVersion.Valid || projVersion.String != "text/v1" || !strings.HasPrefix(media, "text/plain") || !projHash.Valid || projHash.String != rawHash.String {
			return protocol.EvidenceReadResult{}, protocol.NewError(protocol.CodeUnsupportedContract, "the evidence has no text projection")
		}
		textProjection = true
	default:
		return protocol.EvidenceReadResult{}, protocol.NewError(protocol.CodeInvalidParams, "unknown projection version")
	}

	size := uint64(total.Int64)
	start, end := in.Range.Start, in.Range.End
	if end > size {
		return protocol.EvidenceReadResult{}, protocol.NewError(protocol.CodeInvalidRange, "the range is past the end of the evidence")
	}
	// A text range is judged on the bytes at its two ends, so one byte past the end
	// is read as well (when there is one).
	readTo := end
	if textProjection && end < size {
		readTo = end + 1
	}
	window, err := s.readBytes(ctx, in.EvidenceID, start, readTo)
	if err != nil {
		return protocol.EvidenceReadResult{}, err
	}
	if textProjection {
		if (start < size && isUTF8Continuation(window[0])) || (end < size && isUTF8Continuation(window[end-start])) {
			return protocol.EvidenceReadResult{}, protocol.NewError(protocol.CodeInvalidRange, "the range does not start and end on character boundaries")
		}
	}
	data := window[:end-start]
	whole := start == 0 && end == size
	return protocol.EvidenceReadResult{
		EvidenceID: in.EvidenceID, ProjectionVersion: in.ProjectionVersion, DataBase64: base64.StdEncoding.EncodeToString(data),
		TotalBytes: total.Int64, ReturnedRange: protocol.ByteRange{Start: start, End: end}, Partial: !whole || complete.Int64 == 0,
		RawHash: rawHash.String, ProjectionHash: rawHash.String, CaptureComplete: complete.Int64 == 1,
	}, nil
}

func isUTF8Continuation(b byte) bool { return b&0xC0 == 0x80 }

// readBytes assembles bytes [from, to) of an Evidence from its chunks. A chunk that
// is missing is damage, not a shorter answer.
func (s *Store) readBytes(ctx context.Context, evidenceID string, from, to uint64) ([]byte, error) {
	out := make([]byte, 0, to-from)
	if to == from {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT byte_start, data FROM evidence_chunks
		WHERE evidence_id=? AND byte_start<? AND byte_start+length(data)>? ORDER BY ordinal`, evidenceID, to, from)
	if err != nil {
		return nil, wrapRead("read evidence chunks", err)
	}
	defer rows.Close()
	next := from
	for rows.Next() {
		var chunkStart int64
		var data []byte
		if err := rows.Scan(&chunkStart, &data); err != nil {
			return nil, wrapRead("read evidence chunks", err)
		}
		cs := uint64(chunkStart)
		if cs > next {
			return nil, protocol.NewError(protocol.CodeIntegrityBlocked, "an evidence chunk is missing")
		}
		lo := next - cs
		hi := min(to-cs, uint64(len(data)))
		out = append(out, data[lo:hi]...)
		next = cs + hi
	}
	if err := rows.Err(); err != nil {
		return nil, wrapRead("read evidence chunks", err)
	}
	if next != to {
		return nil, protocol.NewError(protocol.CodeIntegrityBlocked, "the evidence is shorter than its recorded size")
	}
	return out, nil
}
