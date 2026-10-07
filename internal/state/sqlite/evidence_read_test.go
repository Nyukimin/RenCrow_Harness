package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func hexSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func readReq(id, projection string, start, end uint64) protocol.EvidenceReadInput {
	return protocol.EvidenceReadInput{EvidenceID: id, ProjectionVersion: projection, Range: protocol.ByteRange{Start: start, End: end}}
}

func decodeData(t testing.TB, r protocol.EvidenceReadResult) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(r.DataBase64)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// storeEvidence seals data as Evidence of the env's caller, outside any thread.
func (e *env) storeEvidence(data []byte) string {
	e.t.Helper()
	var id string
	err := e.s.write(context.Background(), func(tx *sql.Tx, _ time.Time) error {
		var err error
		id, err = writeEvidence(context.Background(), tx, e.caller.Principal, evidenceMeta{Purpose: "tool_output"}, data)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestReadEvidenceWholeAndRangesAreByteExact(t *testing.T) {
	e := newEnv(t)
	start := e.mustAdmit(e.thread, "evid.key.0000000000001", nil)
	id := start.Result.Intake.EvidenceID
	text := "この隔離workspaceのテスト失敗を修正し、実際の検査結果を報告する。"
	ctx := context.Background()

	for _, projection := range []string{"raw/v1", "text/v1"} {
		r, err := e.s.ReadEvidence(ctx, e.caller, readReq(id, projection, 0, uint64(len(text))))
		if err != nil {
			t.Fatalf("%s: %v", projection, err)
		}
		if string(decodeData(t, r)) != text || r.TotalBytes != int64(len(text)) || r.Partial || !r.CaptureComplete ||
			r.RawHash != hexSHA([]byte(text)) || r.ProjectionHash != hexSHA([]byte(text)) || r.EvidenceID != id || r.ProjectionVersion != projection ||
			r.ReturnedRange != (protocol.ByteRange{Start: 0, End: uint64(len(text))}) {
			t.Fatalf("%s: %+v", projection, r)
		}
		if _, err := protocol.Encode(r); err != nil {
			t.Fatalf("%s: not a valid EvidenceReadResult: %v", projection, err)
		}
	}

	// A range of ASCII bytes inside the text: partial, and the hashes still cover the whole.
	asciiStart := strings.Index(text, "workspace")
	r, err := e.s.ReadEvidence(ctx, e.caller, readReq(id, "raw/v1", uint64(asciiStart), uint64(asciiStart+len("workspace"))))
	if err != nil || string(decodeData(t, r)) != "workspace" || !r.Partial || r.RawHash != hexSHA([]byte(text)) || r.TotalBytes != int64(len(text)) {
		t.Fatalf("%v %+v", err, r)
	}
	// Empty ranges at both ends are valid and return no bytes.
	for _, at := range []uint64{0, uint64(len(text))} {
		r, err := e.s.ReadEvidence(ctx, e.caller, readReq(id, "text/v1", at, at))
		if err != nil || len(decodeData(t, r)) != 0 || !r.Partial {
			t.Fatalf("empty range at %d: %v %+v", at, err, r)
		}
	}
}

func TestTextProjectionRangesMustBeOnCharacterBoundaries(t *testing.T) {
	e := newEnv(t)
	id := e.storeEvidence([]byte("a日本b")) // a=1, 日=3, 本=3, b=1
	ctx := context.Background()
	// Boundaries: 0 1 4 7 8.
	for _, ok := range [][2]uint64{{0, 8}, {1, 4}, {1, 7}, {4, 8}, {0, 1}} {
		if _, err := e.s.ReadEvidence(ctx, e.caller, readReq(id, "text/v1", ok[0], ok[1])); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for _, bad := range [][2]uint64{{2, 4}, {3, 4}, {1, 5}, {1, 6}, {2, 3}, {5, 8}} {
		_, err := e.s.ReadEvidence(ctx, e.caller, readReq(id, "text/v1", bad[0], bad[1]))
		if protocol.CodeOf(err) != protocol.CodeInvalidRange {
			t.Errorf("text %v: %v", bad, err)
		}
		// The raw projection has no character boundary: the same range is fine.
		if _, err := e.s.ReadEvidence(ctx, e.caller, readReq(id, "raw/v1", bad[0], bad[1])); err != nil {
			t.Errorf("raw %v: %v", bad, err)
		}
	}
}

func TestEvidenceRangeLimits(t *testing.T) {
	e := newEnv(t)
	id := e.storeEvidence(bytes.Repeat([]byte("x"), 100))
	ctx := context.Background()
	for name, in := range map[string]protocol.EvidenceReadInput{
		"past the end":     readReq(id, "raw/v1", 0, 101),
		"start past end":   readReq(id, "raw/v1", 101, 101),
		"inverted":         readReq(id, "raw/v1", 10, 5),
		"over one read":    readReq(id, "raw/v1", 0, protocol.MaxEvidenceReadBytes+1),
		"far beyond total": readReq(id, "raw/v1", 0, 1<<53),
	} {
		_, err := e.s.ReadEvidence(ctx, e.caller, in)
		if protocol.CodeOf(err) != protocol.CodeInvalidRange {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestEvidenceReadCrossesChunkBoundaries(t *testing.T) {
	e := newEnv(t)
	total := evidenceChunkSize + 100000
	data := make([]byte, total)
	for i := range data {
		data[i] = byte('a' + i%26)
	}
	id := e.storeEvidence(data)
	ctx := context.Background()
	if n := scalar[int](t, e.s.db, "SELECT COUNT(*) FROM evidence_chunks WHERE evidence_id=?", id); n != 2 {
		t.Fatalf("expected two chunks, got %d", n)
	}
	// 64 KiB straddling the chunk boundary.
	start := uint64(evidenceChunkSize - 100)
	end := start + protocol.MaxEvidenceReadBytes
	r, err := e.s.ReadEvidence(ctx, e.caller, readReq(id, "raw/v1", start, end))
	if err != nil || !bytes.Equal(decodeData(t, r), data[start:end]) || !r.Partial || r.RawHash != hexSHA(data) || r.TotalBytes != int64(total) {
		t.Fatalf("%v partial=%v total=%d", err, r.Partial, r.TotalBytes)
	}
	// The last bytes of the second chunk.
	r, err = e.s.ReadEvidence(ctx, e.caller, readReq(id, "text/v1", uint64(total-10), uint64(total)))
	if err != nil || !bytes.Equal(decodeData(t, r), data[total-10:]) {
		t.Fatalf("%v", err)
	}
}

func TestEvidenceReadIsScopedAndHidesExistence(t *testing.T) {
	e := newEnv(t)
	start := e.mustAdmit(e.thread, "evid.key.0000000000002", nil)
	threadEvidence := start.Result.Intake.EvidenceID
	loose := e.storeEvidence([]byte("loose"))
	ctx := context.Background()

	missing := "evd_00000000-0000-7000-8000-0000000000aa"
	stranger := e.newCaller(func(c *intake.CallerConfig) { c.Principal = "user:ren"; c.Relays = nil })
	for _, id := range []string{threadEvidence, loose} {
		_, err := e.s.ReadEvidence(ctx, stranger, readReq(id, "raw/v1", 0, 1))
		wantCode(t, err, protocol.CodeForbidden)
	}
	_, errMissing := e.s.ReadEvidence(ctx, stranger, readReq(missing, "raw/v1", 0, 1))
	wantCode(t, errMissing, protocol.CodeForbidden)

	// A caller that may read the session owner reads the Evidence of that session's items.
	reader := e.newCaller(func(c *intake.CallerConfig) {
		c.Principal = "user:ren"
		c.Relays = nil
		c.ReadableSessionOwners = []string{"core:local"}
	})
	if _, err := e.s.ReadEvidence(ctx, reader, readReq(threadEvidence, "raw/v1", 0, 1)); err != nil {
		t.Fatalf("a readable owner: %v", err)
	}
	// Evidence that is not tied to a thread belongs to the principal that stored it.
	_, err := e.s.ReadEvidence(ctx, reader, readReq(loose, "raw/v1", 0, 1))
	wantCode(t, err, protocol.CodeForbidden)
	if _, err := e.s.ReadEvidence(ctx, e.caller, readReq(loose, "raw/v1", 0, 5)); err != nil {
		t.Fatal(err)
	}
}

func TestEvidenceThatIsNotSealedOrHasNoTextProjection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	building := "evd_00000000-0000-7000-8000-0000000000b1"
	mustExec(t, e.s.db, `INSERT INTO evidence(evidence_id, principal, owner, state, media_type, metadata_json) VALUES(?,?,?,'building','text/plain; charset=utf-8','{}')`,
		building, e.caller.Principal, storeOwner)
	mustExec(t, e.s.db, `INSERT INTO evidence_chunks(evidence_id, ordinal, byte_start, data) VALUES(?,0,0,?)`, building, []byte("half"))
	_, err := e.s.ReadEvidence(ctx, e.caller, readReq(building, "raw/v1", 0, 4))
	wantCode(t, err, protocol.CodeBusy)
	if !strings.Contains(err.Error(), "captured") {
		t.Fatalf("the refusal should say the capture is not finished: %v", err)
	}

	binary := []byte{0xff, 0x00, 0xfe, 0x01}
	bin := "evd_00000000-0000-7000-8000-0000000000b2"
	mustExec(t, e.s.db, `INSERT INTO evidence(evidence_id, principal, owner, state, media_type, metadata_json) VALUES(?,?,?,'building','application/octet-stream','{}')`,
		bin, e.caller.Principal, storeOwner)
	mustExec(t, e.s.db, `INSERT INTO evidence_chunks(evidence_id, ordinal, byte_start, data) VALUES(?,0,0,?)`, bin, binary)
	mustExec(t, e.s.db, `UPDATE evidence SET state='sealed', capture_complete=0, total_bytes=4, raw_hash=? WHERE evidence_id=?`, hexSHA(binary), bin)
	// No text projection exists for binary data: it is refused, never decoded as text.
	_, err = e.s.ReadEvidence(ctx, e.caller, readReq(bin, "text/v1", 0, 4))
	wantCode(t, err, protocol.CodeUnsupportedContract)
	// The raw bytes are readable; an incomplete capture is partial even when the whole range is returned.
	r, err := e.s.ReadEvidence(ctx, e.caller, readReq(bin, "raw/v1", 0, 4))
	if err != nil || !bytes.Equal(decodeData(t, r), binary) || r.CaptureComplete || !r.Partial || r.RawHash != hexSHA(binary) || r.ProjectionHash != hexSHA(binary) {
		t.Fatalf("%v %+v", err, r)
	}
	if _, err := protocol.Encode(r); err != nil {
		t.Fatalf("not a valid EvidenceReadResult: %v", err)
	}
}

func TestReadEvidenceNeverWritesAnything(t *testing.T) {
	e := newEnv(t)
	start := e.mustAdmit(e.thread, "evid.key.0000000000003", nil)
	before := e.counts()
	if _, err := e.s.ReadEvidence(context.Background(), e.caller, readReq(start.Result.Intake.EvidenceID, "raw/v1", 0, 3)); err != nil {
		t.Fatal(err)
	}
	e.wantNoChange(before)
}
