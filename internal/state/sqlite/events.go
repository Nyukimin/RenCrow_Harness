package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// eventSeq hands out the next event_seq values of one Thread inside a transaction.
// The Thread row holds the counter; the caller writes the final value back with a
// compare-and-set (see admitTx) in the same transaction.
type eventSeq struct {
	base int64
	used int64
}

func (e *eventSeq) next() int64 {
	e.used++
	return e.base + e.used
}

func (e *eventSeq) last() int64 { return e.base + e.used }

// eventRecord is one event to store: the common fields without event_seq, the
// payload struct, and the real dependencies it has on earlier events.
type eventRecord struct {
	common       protocol.EventCommon
	payload      protocol.EventPayload
	causation    *string  // the one event this one directly follows from, if any
	dependencies []string // every event it depends on
}

// appendEvent (the storage half of F38) builds the typed Event, which validates it
// against the schema and its type's conditions, and inserts it with the payload
// stored as the canonical JSON of the payload only. It returns the Event exactly as
// events/read will return it.
func appendEvent(ctx context.Context, tx *sql.Tx, seq *eventSeq, rec eventRecord) (protocol.Event, error) {
	rec.common.EventSeq = seq.next()
	ev, err := protocol.BuildTypedEvent(rec.common, rec.payload)
	if err != nil {
		return protocol.Event{}, err
	}
	deps := rec.dependencies
	if deps == nil {
		deps = []string{}
	}
	depsJSON, err := json.Marshal(deps)
	if err != nil {
		return protocol.Event{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO events(event_id, thread_id, event_seq, task_id, run_id, type, causation_event_id,
		dependency_event_ids_json, receipt_id, evidence_id, payload_json, recorded_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		ev.EventID, ev.ThreadID, ev.EventSeq, ev.TaskID, ev.RunID, ev.Type, rec.causation,
		string(depsJSON), ev.ReceiptID, ev.EvidenceID, string(ev.Payload), ev.RecordedAt)
	if err != nil {
		return protocol.Event{}, fmt.Errorf("sqlite: insert event: %w", mapSQLiteError(err))
	}
	return ev, nil
}

// eventFromRow rebuilds the Event a row stands for. message_id and code have no
// column: they are the values inside the payload, so they are derived, which makes
// the stored row and the API value one and the same.
func eventFromRow(ev protocol.Event) (protocol.Event, error) {
	msg, code, err := protocol.DerivedCommon(ev.Type, ev.Payload)
	if err != nil {
		return protocol.Event{}, err
	}
	ev.MessageID, ev.Code = msg, code
	return ev, nil
}

// validateStoredEvent rebuilds the Event of a row and holds it to the whole
// contract: the schema, the type's conditions, and that the stored payload bytes
// are the canonical JSON themselves (Encode alone would canonicalize them and hide a
// payload that was written any other way).
func validateStoredEvent(ev protocol.Event) (protocol.Event, error) {
	full, err := eventFromRow(ev)
	if err != nil {
		return protocol.Event{}, err
	}
	if err := full.Validate(); err != nil {
		return protocol.Event{}, err
	}
	if _, err := protocol.Encode(full); err != nil {
		return protocol.Event{}, err
	}
	return full, nil
}

// checkStoredEvent is the closure check of one stored event.
func checkStoredEvent(ev protocol.Event) error {
	_, err := validateStoredEvent(ev)
	return err
}

// EventsAfter returns up to limit committed events of the thread with
// event_seq > afterSeq, in order, and whether more remain. Each is re-validated on
// the way out, so a row that no longer satisfies the contract is an error and is
// never served. It does no access control: that is the caller's.
func (s *Store) EventsAfter(ctx context.Context, threadID string, afterSeq int64, limit int) ([]protocol.Event, bool, error) {
	if limit < 1 || limit > 1000 {
		return nil, false, protocol.NewError(protocol.CodeInvalidParams, "limit must be 1 to 1000")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, event_seq, thread_id, task_id, run_id, type, receipt_id, evidence_id, payload_json, recorded_at
		FROM events WHERE thread_id=? AND event_seq>? ORDER BY event_seq LIMIT ?`, threadID, afterSeq, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: read events: %w", mapSQLiteError(err))
	}
	defer rows.Close()
	var out []protocol.Event
	for rows.Next() {
		var ev protocol.Event
		var task, run, receipt, evidence sql.NullString
		var payload string
		if err := rows.Scan(&ev.EventID, &ev.EventSeq, &ev.ThreadID, &task, &run, &ev.Type, &receipt, &evidence, &payload, &ev.RecordedAt); err != nil {
			return nil, false, err
		}
		ev.TaskID, ev.RunID, ev.ReceiptID, ev.EvidenceID = nullStr(task), nullStr(run), nullStr(receipt), nullStr(evidence)
		ev.Payload = json.RawMessage(payload)
		full, err := validateStoredEvent(ev)
		if err != nil {
			return nil, false, protocol.NewError(protocol.CodeIntegrityBlocked, "a stored event does not satisfy the contract").Wrap(err)
		}
		out = append(out, full)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("sqlite: read events: %w", mapSQLiteError(err))
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}
