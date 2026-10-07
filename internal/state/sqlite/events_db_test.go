package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// A66: each of the 15 event types is built from a typed payload, stored, read back
// through the API read path, and is the same Event with the same payload bytes.
func TestAll15EventTypesSurviveTheDatabase(t *testing.T) {
	s, _ := newStore(t)
	var f struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(designFile(t, "wire/event_payloads.json"), &f); err != nil {
		t.Fatal(err)
	}
	// The graph the design events point at: thread, task, run, receipt, evidence.
	const (
		sess = "ses_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"
		thr  = "thr_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"
		tsk  = "tsk_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"
		run  = "run_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"
		rcp  = "rcp_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"
		evd  = "evd_018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b0001"
	)
	seedThread(t, s, "user:a", sess, thr)
	mustExec(t, s.db, "INSERT INTO tasks(task_id,thread_id,kind,status,created_at) VALUES(?,?,'work','running','t')", tsk, thr)
	mustExec(t, s.db, `INSERT INTO runs(run_id,task_id,trace_id,phase,status,writer_epoch,started_at,limits_json,deadline_at,recovery_policy_json,recovery_policy_revision)
		VALUES(?,?,'trc','Admitting','running',1,'t','{}','t','{}',?)`, run, tsk, strings.Repeat("a", 64))
	mustExec(t, s.db, "INSERT INTO receipts(receipt_id,principal,idempotency_key,operation,payload_hash,stage,created_at,updated_at) VALUES(?,'p','k','op',?,'accepted','t','t')", rcp, strings.Repeat("b", 64))
	mustExec(t, s.db, "INSERT INTO evidence(evidence_id,principal,owner,state,media_type,metadata_json) VALUES(?,'p','o','building','text/plain','{}')", evd)
	mustExec(t, s.db, "UPDATE evidence SET state='sealed',capture_complete=1,total_bytes=0,raw_hash=? WHERE evidence_id=?", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", evd)

	ctx := context.Background()
	var want []protocol.Event
	if err := s.write(ctx, func(tx *sql.Tx, _ time.Time) error {
		seq := &eventSeq{}
		for _, raw := range f.Events {
			fixture, err := protocol.Decode[protocol.Event](raw)
			if err != nil {
				return err
			}
			payload, err := fixture.TypedPayload()
			if err != nil {
				return err
			}
			ev, err := appendEvent(ctx, tx, seq, eventRecord{common: protocol.EventCommon{
				EventID: fixture.EventID, ThreadID: fixture.ThreadID, TaskID: fixture.TaskID, RunID: fixture.RunID, ReceiptID: fixture.ReceiptID,
				EvidenceID: fixture.EvidenceID, MessageID: fixture.MessageID, Code: fixture.Code, RecordedAt: fixture.RecordedAt,
			}, payload: payload})
			if err != nil {
				return err
			}
			want = append(want, ev)
		}
		_, err := tx.ExecContext(ctx, "UPDATE threads SET event_seq=? WHERE thread_id=?", seq.last(), thr)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(want) != 15 {
		t.Fatalf("%d events built", len(want))
	}

	got, more, err := s.EventsAfter(ctx, thr, 0, 100)
	if err != nil || more || len(got) != 15 {
		t.Fatalf("%d events, more=%v, %v", len(got), more, err)
	}
	types := map[string]bool{}
	for i := range got {
		types[got[i].Type] = true
		gb, _ := protocol.Encode(got[i])
		wb, _ := protocol.EncodeCanonicalContract(f.Events[i])
		if !bytes.Equal(gb, wb) {
			t.Fatalf("event %d (%s) differs from the design example after a round trip\n got %s\nwant %s", i, got[i].Type, gb, wb)
		}
		stored := queryString(t, s.db, "SELECT payload_json FROM events WHERE event_id=?", got[i].EventID)
		if stored != string(got[i].Payload) || stored != string(want[i].Payload) {
			t.Fatalf("event %d: stored payload %s, API payload %s", i, stored, got[i].Payload)
		}
		if got[i].EventSeq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, got[i].EventSeq)
		}
	}
	if len(types) != 15 {
		t.Fatalf("%d distinct types read back", len(types))
	}
	report, err := s.VerifyClosure(ctx)
	if err != nil || !report.OK {
		t.Fatalf("closure: %v %+v", err, report)
	}

	// Paging is by event_seq and says whether more remain.
	page, more, err := s.EventsAfter(ctx, thr, 4, 3)
	if err != nil || !more || len(page) != 3 || page[0].EventSeq != 5 || page[2].EventSeq != 7 {
		t.Fatalf("page %v %v %v", len(page), more, err)
	}
	last, more, err := s.EventsAfter(ctx, thr, 14, 10)
	if err != nil || more || len(last) != 1 {
		t.Fatalf("last page %d %v %v", len(last), more, err)
	}
	for _, limit := range []int{0, -1, 1001} {
		if _, _, err := s.EventsAfter(ctx, thr, 0, limit); err == nil {
			t.Fatalf("limit %d accepted", limit)
		}
	}

	// A row that no longer satisfies the contract is not served.
	mustExec(t, s.db, "DROP TRIGGER events_no_update")
	mustExec(t, s.db, `UPDATE events SET payload_json=replace(payload_json,'"stage":"act"','"stage":"act","raw":"x"') WHERE type='model.requested'`)
	if _, _, err := s.EventsAfter(ctx, thr, 0, 100); protocol.CodeOf(err) != protocol.CodeIntegrityBlocked {
		t.Fatalf("a stored event with an unknown key was served: %v", err)
	}
}
