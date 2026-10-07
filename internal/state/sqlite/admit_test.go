package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func TestAdmitStartAcceptsTheWholeInputInOneTransaction(t *testing.T) {
	e := newEnv(t)
	out, err := e.admit(e.thread, "start.key.0000000001", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := out.Result
	if out.Replayed || !r.Accepted {
		t.Fatalf("%+v", out)
	}
	for kind, id := range map[string]string{"rcp_": r.ReceiptID, "ses_": r.SessionID, "thr_": r.ThreadID, "turn_": r.TurnID, "tsk_": r.TaskID, "run_": r.RunID, "trc_": r.TraceID} {
		if !strings.HasPrefix(id, kind) {
			t.Errorf("%s is not a %s ID", id, kind)
		}
	}
	if r.SessionID != e.session || r.ThreadID != e.thread {
		t.Fatalf("session/thread %s %s", r.SessionID, r.ThreadID)
	}
	if r.EffectiveLimits != hostLimits || r.DeadlineAt != "2026-10-07T12:30:00Z" || r.RecoveryPolicyRevision != designRecoveryRevision {
		t.Fatalf("budget %+v %s %s", r.EffectiveLimits, r.DeadlineAt, r.RecoveryPolicyRevision)
	}
	in := r.Intake
	if in.ReceiptID != r.ReceiptID || in.ThreadID != e.thread || in.Principal != "core:local" || in.Entrypoint != protocol.EntrypointStdioCore ||
		in.CallerProfileDigest != e.caller.ProfileDigest || in.DeclaredOrigin != "automation" || in.EffectiveOrigin != "automation" ||
		in.ProofBasis != "automation" || in.ProofDigest != nil || in.AcceptedSequence != 1 || in.AcceptedAt != "2026-10-07T12:00:00Z" {
		t.Fatalf("intake receipt %+v", in)
	}
	if _, err := protocol.Encode(r); err != nil {
		t.Fatalf("the result is not a valid StartResult: %v", err)
	}

	want := map[string]int{"sessions": 1, "threads": 1, "evidence": 5, "evidence_chunks": 5, "items": 5, "turns": 1, "tasks": 1, "runs": 1, "receipts": 1, "events": 3, "relay_nonces": 0}
	for table, n := range e.counts() {
		if n != want[table] {
			t.Errorf("table %s has %d rows, want %d", table, n, want[table])
		}
	}
	db := e.s.db
	if v := queryString(t, db, "SELECT active_run_id FROM threads WHERE thread_id=?", e.thread); v != r.RunID {
		t.Fatalf("active_run_id %q", v)
	}
	if v := scalar[int](t, db, "SELECT event_seq FROM threads WHERE thread_id=?", e.thread); v != 3 {
		t.Fatalf("event_seq %d", v)
	}
	if rev := scalar[string](t, db, "SELECT context_revision||','||control_revision||','||queue_revision FROM threads WHERE thread_id=?", e.thread); rev != "0,0,0" {
		t.Fatalf("admission must not move revisions: %s", rev)
	}

	// CORE delegation: the parent Task is the upstream Task, in the Task row and its event.
	var upstream protocol.Upstream
	if err := json.Unmarshal(designFile(t, "core_start.json"), &struct {
		U *protocol.Upstream `json:"upstream"`
	}{&upstream}); err != nil {
		t.Fatal(err)
	}
	if v := queryString(t, db, "SELECT parent_task_id FROM tasks WHERE task_id=?", r.TaskID); v != upstream.TaskID {
		t.Fatalf("parent_task_id %q, want %q", v, upstream.TaskID)
	}
	if v := queryString(t, db, "SELECT parent_owner FROM tasks WHERE task_id=?", r.TaskID); v != "RenCrow_CORE" {
		t.Fatalf("parent_owner %q", v)
	}
	wantUpstream, _ := protocol.Encode(upstream)
	if v := queryString(t, db, "SELECT upstream_json FROM tasks WHERE task_id=?", r.TaskID); v != string(wantUpstream) {
		t.Fatalf("upstream_json %s", v)
	}
	if v := queryString(t, db, "SELECT root_task_id||'|'||source_message_id FROM turns WHERE turn_id=?", r.TurnID); v != r.TaskID+"|"+in.MessageID {
		t.Fatalf("turn %s", v)
	}

	// The Run is frozen with everything it needs.
	limitsJSON, _ := protocol.Encode(hostLimits)
	recoveryJSON, _ := protocol.Encode(testRecovery)
	got := queryString(t, db, `SELECT phase||'|'||status||'|'||writer_epoch||'|'||trace_id||'|'||limits_json||'|'||deadline_at||'|'||recovery_policy_json||'|'||recovery_policy_revision||'|'||generation_attempts_used||'|'||generation_attempts_unknown
		FROM runs WHERE run_id=?`, r.RunID)
	wantRun := strings.Join([]string{"Admitting", "running", "1", r.TraceID, string(limitsJSON), r.DeadlineAt, string(recoveryJSON), designRecoveryRevision, "0", "0"}, "|")
	if got != wantRun {
		t.Fatalf("run row\n got %s\nwant %s", got, wantRun)
	}

	// Events: the three of an admission, in order, as events/read returns them.
	events, more, err := e.s.EventsAfter(context.Background(), e.thread, 0, 100)
	if err != nil || more {
		t.Fatalf("%v %v", err, more)
	}
	types := make([]string, len(events))
	for i, ev := range events {
		types[i] = ev.Type
		if ev.EventSeq != int64(i+1) || ev.ReceiptID == nil || *ev.ReceiptID != r.ReceiptID || ev.EvidenceID == nil || *ev.EvidenceID != in.EvidenceID {
			t.Fatalf("event %d: %+v", i, ev)
		}
		stored := queryString(t, db, "SELECT payload_json FROM events WHERE event_id=?", ev.EventID)
		if stored != string(ev.Payload) {
			t.Fatalf("stored payload differs from the API payload\n %s\n %s", stored, ev.Payload)
		}
	}
	if strings.Join(types, ",") != "input.accepted,task.created,run.started" {
		t.Fatalf("events %v", types)
	}
	if events[0].MessageID == nil || *events[0].MessageID != in.MessageID || events[1].RunID != nil || events[2].RunID == nil {
		t.Fatalf("event common fields: %+v", events)
	}
	if v := queryString(t, db, "SELECT causation_event_id||'|'||dependency_event_ids_json FROM events WHERE event_id=?", events[2].EventID); v != events[1].EventID+`|["`+events[1].EventID+`"]` {
		t.Fatalf("run.started causation %s", v)
	}
	var accepted protocol.InputAcceptedPayload
	if err := json.Unmarshal(events[0].Payload, &accepted); err != nil || accepted.Intake != in || accepted.Disposition != "initial" || accepted.QueueItemID != nil {
		t.Fatalf("input.accepted payload %+v %v", accepted, err)
	}
	var created protocol.TaskCreatedPayload
	if err := json.Unmarshal(events[1].Payload, &created); err != nil || created.ParentTaskID == nil || *created.ParentTaskID != upstream.TaskID || created.Kind != "work" {
		t.Fatalf("task.created payload %+v %v", created, err)
	}

	// Everything the acceptance made is a closed, intact recovery point.
	report, err := e.s.VerifyClosure(context.Background())
	if err != nil || !report.OK {
		t.Fatalf("closure: %v %+v", err, report)
	}
}

func TestAdmitStartKeepsTheRawInputByteForByte(t *testing.T) {
	texts := map[string]string{
		"leading and trailing space": "  \n\tkeep me  \n\n",
		"carriage returns":           "line one\r\nline two\r",
		"decomposed unicode":         "Café é",
		"emoji and CJK":              "修正して 🙂 テスト",
		"html specials":              "</script> & <b>    ",
		"a NUL byte":                 "a\x00b",
		"empty":                      "",
		"just whitespace":            " ",
		"larger than a chunk":        strings.Repeat("0123456789abcdef", 160*1024), // 2.5 MiB
	}
	for name, text := range texts {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			out := e.mustAdmit(e.thread, "raw.key.000000000001", func(m map[string]any) { m["input"].(map[string]any)["text"] = text })
			id := out.Result.Intake.EvidenceID
			rows, err := e.s.db.Query("SELECT ordinal, byte_start, data FROM evidence_chunks WHERE evidence_id=? ORDER BY ordinal", id)
			if err != nil {
				t.Fatal(err)
			}
			var got []byte
			for n := 0; rows.Next(); n++ {
				var ord, start int
				var data []byte
				if err := rows.Scan(&ord, &start, &data); err != nil {
					t.Fatal(err)
				}
				if ord != n || start != len(got) {
					t.Fatalf("chunk %d starts at %d, want %d", ord, start, len(got))
				}
				got = append(got, data...)
			}
			_ = rows.Close()
			if !bytes.Equal(got, []byte(text)) {
				t.Fatalf("stored %d bytes, want %d: the text was altered", len(got), len(text))
			}
			sum := sha256.Sum256([]byte(text))
			var state, rawHash, projHash string
			var total int
			if err := e.s.db.QueryRow("SELECT state, raw_hash, projection_hash, total_bytes FROM evidence WHERE evidence_id=?", id).Scan(&state, &rawHash, &projHash, &total); err != nil {
				t.Fatal(err)
			}
			if state != "sealed" || rawHash != hex.EncodeToString(sum[:]) || projHash != rawHash || total != len(text) {
				t.Fatalf("evidence row: %s %s %s %d", state, rawHash, projHash, total)
			}
			// The text never reaches the public event payloads.
			if len(text) > 8 && len(text) < 100 {
				for _, p := range queryAll(t, e.s.db, "SELECT payload_json FROM events") {
					if strings.Contains(p, strings.TrimSpace(text)) {
						t.Fatalf("raw text leaked into an event payload: %s", p)
					}
				}
			}
			if report, err := e.s.VerifyClosure(context.Background()); err != nil || !report.OK {
				t.Fatalf("closure: %v %+v", err, report)
			}
		})
	}
}

func queryAll(t testing.TB, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestContextBlocksAreStoredAsHostContext(t *testing.T) {
	e := newEnv(t)
	source := map[string]any{
		"owner": "RenCrow_CORE", "source_id": "src-1", "raw_hash": strings.Repeat("a", 64), "projection_version": "text/v1",
		"range": map[string]any{"start": 0, "end": 4}, "origin": "human", "sequence": 3,
	}
	out := e.mustAdmit(e.thread, "blocks.key.00000000001", func(m map[string]any) {
		blocks := m["context_blocks"].([]any)
		b := blocks[2].(map[string]any)
		b["source"] = source
		rev, err := protocol.ContextRevision(b["kind"].(string), b["text"].(string), &protocol.SourceRef{
			Owner: "RenCrow_CORE", SourceID: "src-1", RawHash: strings.Repeat("a", 64), ProjectionVersion: "text/v1",
			Range: protocol.ByteRange{Start: 0, End: 4}, Origin: "human", Sequence: 3,
		})
		if err != nil {
			t.Fatal(err)
		}
		b["revision"] = rev
	})
	rows, err := e.s.db.Query(`SELECT i.sequence, i.history_kind, i.origin, i.metadata_json, e.raw_hash FROM items i JOIN evidence e ON e.evidence_id=i.evidence_id
		WHERE i.thread_id=? ORDER BY i.sequence`, e.thread)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type item struct {
		seq           int
		kind, origin  string
		meta, rawHash string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.seq, &it.kind, &it.origin, &it.meta, &it.rawHash); err != nil {
			t.Fatal(err)
		}
		items = append(items, it)
	}
	if len(items) != 5 {
		t.Fatalf("%d items", len(items))
	}
	if items[0].seq != 1 || items[0].kind != "AutomationInstruction" || items[0].origin != "automation" {
		t.Fatalf("the input is the first item: %+v", items[0])
	}
	for i, it := range items[1:] {
		if it.seq != i+2 || it.kind != "HostContext" {
			t.Fatalf("block %d: %+v", i, it)
		}
		var meta map[string]any
		if err := json.Unmarshal([]byte(it.meta), &meta); err != nil {
			t.Fatal(err)
		}
		block := e.blockOfDesign(i)
		digest, _ := protocol.TextDigest(block.Text)
		if meta["block_kind"] != block.Kind || meta["computed_text_digest"] != digest || it.rawHash != digest ||
			meta["intake_receipt_ref"] != out.Result.ReceiptID || meta["kind"] != "context_block" {
			t.Fatalf("block %d metadata: %v", i, meta)
		}
		if i == 2 { // the block that claims a source
			if it.origin != "unknown" || meta["claimed_source"] == nil {
				t.Fatalf("a claimed source is data, not authority: %+v %v", it, meta)
			}
		} else if it.origin != "host" || meta["claimed_source"] != nil {
			t.Fatalf("a block without a source is host context: %+v", it)
		}
	}
}

func (e *env) blockOfDesign(i int) protocol.ContextBlock {
	var v struct {
		Blocks []protocol.ContextBlock `json:"context_blocks"`
	}
	if err := json.Unmarshal(designFile(e.t, "core_start.json"), &v); err != nil {
		e.t.Fatal(err)
	}
	return v.Blocks[i]
}

func TestSecondStartContinuesTheThreadsSequences(t *testing.T) {
	e := newEnv(t)
	first := e.mustAdmit(e.thread, "seq.key.00000000000001", nil)
	// The first Run ends (the Kernel's job); the Thread is free for the next Turn.
	mustExec(t, e.s.db, "UPDATE runs SET status='completed', phase='Terminal' WHERE run_id=?", first.Result.RunID)
	mustExec(t, e.s.db, "UPDATE threads SET active_run_id=NULL WHERE thread_id=?", e.thread)
	second := e.mustAdmit(e.thread, "seq.key.00000000000002", nil)
	if second.Result.Intake.AcceptedSequence != 6 {
		t.Fatalf("second input sequence %d, want 6 (five items before it)", second.Result.Intake.AcceptedSequence)
	}
	events, _, err := e.s.EventsAfter(context.Background(), e.thread, 0, 100)
	if err != nil || len(events) != 6 {
		t.Fatalf("%d events, %v", len(events), err)
	}
	for i, ev := range events {
		if ev.EventSeq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, ev.EventSeq)
		}
	}
	// Another thread has its own numbering.
	_, other := e.newThread("core:local")
	third := e.mustAdmit(other, "seq.key.00000000000003", nil)
	if third.Result.Intake.AcceptedSequence != 1 {
		t.Fatalf("a new thread starts at 1, got %d", third.Result.Intake.AcceptedSequence)
	}
	if report, err := e.s.VerifyClosure(context.Background()); err != nil || !report.OK {
		t.Fatalf("closure: %v %+v", err, report)
	}
}

func TestIdempotentReplayReturnsTheOriginalWithoutNewRows(t *testing.T) {
	e := newEnv(t)
	first := e.mustAdmit(e.thread, "replay.key.000000000001", nil)
	before := e.counts()
	// The Thread now has an active Run and the clock has moved on: neither matters.
	e.s.clock = intake.FixedClock(testNow.Add(24 * time.Hour))
	again, err := e.admit(e.thread, "replay.key.000000000001", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || !reflect.DeepEqual(again.Result, first.Result) {
		t.Fatalf("replay differs:\n%+v\n%+v", again, first)
	}
	e.wantNoChange(before)
	// A different JSON spelling of the same params is the same request.
	reordered := e.params(e.thread, "replay.key.000000000001", func(m map[string]any) {
		lim := m["limits"].(map[string]any)
		m["limits"] = map[string]any{"max_generation_attempts": lim["max_generation_attempts"], "max_capture_bytes": lim["max_capture_bytes"],
			"deadline_seconds": json.Number("1800.0"), "max_tool_calls_per_step": lim["max_tool_calls_per_step"], "max_model_steps": lim["max_model_steps"]}
	})
	if again, err := e.s.AdmitStart(context.Background(), e.adm, reordered); err != nil || !again.Replayed || !reflect.DeepEqual(again.Result, first.Result) {
		t.Fatalf("an equivalent spelling must replay: %v %+v", err, again)
	}
	e.wantNoChange(before)
}

func TestSameKeyWithAnotherPayloadIsAConflict(t *testing.T) {
	e := newEnv(t)
	e.mustAdmit(e.thread, "conflict.key.0000000001", nil)
	before := e.counts()
	changes := map[string]func(m map[string]any){
		"another text":   func(m map[string]any) { m["input"].(map[string]any)["text"] = "different" },
		"other limits":   func(m map[string]any) { m["limits"].(map[string]any)["max_model_steps"] = 9 },
		"other revision": func(m map[string]any) { m["expected_context_revision"] = 1 },
		"no upstream":    func(m map[string]any) { m["upstream"] = nil },
		"fewer blocks":   func(m map[string]any) { m["context_blocks"] = []any{} },
		"another thread": nil,
	}
	for name, mod := range changes {
		t.Run(name, func(t *testing.T) {
			thread := e.thread
			if mod == nil {
				_, thread = e.newThread("core:local")
				before = e.counts()
			}
			_, err := e.admit(thread, "conflict.key.0000000001", mod)
			wantCode(t, err, protocol.CodeIdempotencyConflict)
			e.wantNoChange(before)
		})
	}
}

func TestIdempotencyKeyIsScopedToThePrincipal(t *testing.T) {
	e := newEnv(t)
	e.mustAdmit(e.thread, "scoped.key.0000000000001", nil)
	// Another principal may use the same key text on its own thread: it is another scope.
	other := e.newCaller(func(c *intake.CallerConfig) { c.Principal = "user:ren"; c.Relays = nil })
	session, thread := identity.NewSessionID().String(), identity.NewThreadID().String()
	seedThread(t, e.s, "user:ren", session, thread)
	adm := e.adm
	adm.Caller = other
	adm.Entrypoint = protocol.EntrypointCLIExec
	out, err := e.s.AdmitStart(context.Background(), adm, e.params(thread, "scoped.key.0000000000001", func(m map[string]any) { m["upstream"] = nil }))
	if err != nil || out.Replayed {
		t.Fatalf("%v %+v", err, out)
	}
	if out.Result.Intake.Principal != "user:ren" {
		t.Fatalf("%+v", out.Result.Intake)
	}
}

func TestRefusalsLeaveNothingBehind(t *testing.T) {
	e := newEnv(t)
	busyKey := e.mustAdmit(e.thread, "busy.setup.0000000000001", nil)
	_ = busyKey
	_, free := e.newThread("core:local")
	_, foreign := e.newThread("user:somebody-else")
	_, readOnly := e.newThread("user:reader")
	readCaller := e.newCaller(func(c *intake.CallerConfig) { c.ReadableSessionOwners = []string{"user:reader"} })

	type refusal struct {
		name   string
		thread string
		adm    func(a *Admission)
		mod    func(m map[string]any)
		code   string
	}
	cases := []refusal{
		{"an active run", e.thread, nil, nil, protocol.CodeBusy},
		{"a thread that does not exist", identity.NewThreadID().String(), nil, nil, protocol.CodeForbidden},
		{"a session of an unrelated owner", foreign, nil, nil, protocol.CodeForbidden},
		{"a read-only owner", readOnly, func(a *Admission) { a.Caller = readCaller }, nil, protocol.CodeForbidden},
		{"another driver's writer epoch", free, func(a *Admission) { a.WriterEpoch = 2 }, nil, protocol.CodeRevisionConflict},
		{"a stale context revision", free, nil, func(m map[string]any) { m["expected_context_revision"] = 3 }, protocol.CodeRevisionConflict},
		{"a stale control revision", free, nil, func(m map[string]any) { m["expected_control_revision"] = 1 }, protocol.CodeRevisionConflict},
		{"steps above the host limit", free, nil, func(m map[string]any) { m["limits"].(map[string]any)["max_model_steps"] = 11 }, protocol.CodeInvalidLimits},
		{"a longer deadline than allowed", free, nil, func(m map[string]any) { m["limits"].(map[string]any)["deadline_seconds"] = 1801 }, protocol.CodeInvalidLimits},
		{"a policy that is not available", free, func(a *Admission) {
			a.Caps = func(string) (protocol.Limits, error) {
				return protocol.Limits{}, fmt.Errorf("not registered: %w", intake.ErrPolicyUnavailable)
			}
		}, nil, protocol.CodeForbidden},
		{"a limit resolver that fails for its own reasons", free, func(a *Admission) {
			a.Caps = func(string) (protocol.Limits, error) { return protocol.Limits{}, fmt.Errorf("registry unreadable") }
		}, nil, protocol.CodeInternal},
		{"a user_message block", free, nil, func(m map[string]any) {
			m["context_blocks"] = append(m["context_blocks"].([]any), map[string]any{"kind": "user_message", "text": "x", "revision": "ctx-v1:" + strings.Repeat("0", 64), "source": nil})
		}, protocol.CodeInvalidRequest},
		{"a block whose revision is not its content's", free, nil, func(m map[string]any) {
			m["context_blocks"].([]any)[0].(map[string]any)["text"] = "tampered"
		}, protocol.CodeInvalidRequest},
		{"an unknown field", free, nil, func(m map[string]any) { m["extra"] = 1 }, protocol.CodeInvalidParams},
		{"a missing field", free, nil, func(m map[string]any) { delete(m, "limits") }, protocol.CodeInvalidParams},
		{"a thread id of another kind", free, nil, func(m map[string]any) { m["thread_id"] = identity.NewTaskID().String() }, protocol.CodeInvalidParams},
		{"a writer epoch of zero", free, func(a *Admission) { a.WriterEpoch = 0 }, nil, protocol.CodeInvalidRequest},
		{"an unknown entrypoint", free, func(a *Admission) { a.Entrypoint = "tui" }, nil, protocol.CodeInvalidRequest},
		{"an operator declaration over stdio", free, func(a *Admission) { a.DeclaredOrigin = "human" }, nil, protocol.CodeInvalidRequest},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := e.counts()
			adm := e.adm
			if c.adm != nil {
				c.adm(&adm)
			}
			key := fmt.Sprintf("refuse.key.%016d", i)
			params := e.params(c.thread, key, c.mod)
			_, err := e.s.AdmitStart(context.Background(), adm, params)
			wantCode(t, err, c.code)
			e.wantNoChange(before)
			if v := queryString(t, e.s.db, "SELECT COUNT(*) FROM receipts WHERE idempotency_key=?", key); v != "0" {
				t.Fatalf("a refused request left a receipt")
			}
		})
	}
	// The thread that stayed free is still admissible after all those refusals.
	if _, err := e.admit(free, "refuse.final.0000000001", nil); err != nil {
		t.Fatalf("refusals must not poison the thread: %v", err)
	}
}

func TestReplayNeedsOnlyReadAccess(t *testing.T) {
	e := newEnv(t)
	_, thread := e.newThread("user:mio")
	control := e.newCaller(func(c *intake.CallerConfig) { c.ControllableSessionOwners = []string{"user:mio"} })
	adm := e.adm
	adm.Caller = control
	first, err := e.s.AdmitStart(context.Background(), adm, e.params(thread, "acl.key.00000000000001", nil))
	if err != nil {
		t.Fatal(err)
	}
	// The profile's control scope is narrowed to read-only: the receipt is still its own to read.
	readOnly := e.newCaller(func(c *intake.CallerConfig) { c.ReadableSessionOwners = []string{"user:mio"} })
	adm.Caller = readOnly
	again, err := e.s.AdmitStart(context.Background(), adm, e.params(thread, "acl.key.00000000000001", nil))
	if err != nil || !again.Replayed || again.Result.RunID != first.Result.RunID {
		t.Fatalf("replay under read-only access: %v %+v", err, again)
	}
	// A new key is a new operation and needs control.
	_, err = e.s.AdmitStart(context.Background(), adm, e.params(thread, "acl.key.00000000000002", nil))
	wantCode(t, err, protocol.CodeForbidden)
	// No access at all: the replay is refused too, so receipts leak nothing.
	none := e.newCaller(func(c *intake.CallerConfig) {})
	adm.Caller = none
	_, err = e.s.AdmitStart(context.Background(), adm, e.params(thread, "acl.key.00000000000001", nil))
	wantCode(t, err, protocol.CodeForbidden)
}

func TestOriginDecisionIsRecordedInTheReceipt(t *testing.T) {
	human := func(c *intake.CallerConfig) {
		c.DefaultOrigin = protocol.OriginHuman
		c.Principal = "user:ren"
		c.Relays = nil
	}
	cases := []struct {
		name       string
		caller     func(c *intake.CallerConfig)
		entrypoint string
		declared   string
		wantOrigin string
		wantBasis  string
		wantKind   string
	}{
		{"interactive line, human profile", human, protocol.EntrypointCLIInteractive, "", "human", "declared_local", "HumanInstruction"},
		{"exec, human profile, no declaration", human, protocol.EntrypointCLIExec, "", "automation", "automation", "AutomationInstruction"},
		{"exec, human profile, --origin human", human, protocol.EntrypointCLIExec, "human", "human", "declared_local", "HumanInstruction"},
		{"pipe, human profile", human, protocol.EntrypointCLIPipe, "", "automation", "automation", "AutomationInstruction"},
		{"interactive line, automation profile", func(c *intake.CallerConfig) { c.Principal = "user:ren"; c.Relays = nil }, protocol.EntrypointCLIInteractive, "", "automation", "automation", "AutomationInstruction"},
		{"explicit unknown", human, protocol.EntrypointCLIExec, "unknown", "unknown", "unknown", "Protected"},
		{"CORE delegation without a proof", func(c *intake.CallerConfig) {}, protocol.EntrypointStdioCore, "", "automation", "automation", "AutomationInstruction"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			caller := e.newCaller(c.caller)
			session, thread := identity.NewSessionID().String(), identity.NewThreadID().String()
			seedThread(t, e.s, caller.Principal, session, thread)
			adm := e.adm
			adm.Caller, adm.Entrypoint, adm.DeclaredOrigin = caller, c.entrypoint, c.declared
			out, err := e.s.AdmitStart(context.Background(), adm, e.params(thread, "origin.key.0000000000001", nil))
			if err != nil {
				t.Fatal(err)
			}
			in := out.Result.Intake
			if in.EffectiveOrigin != c.wantOrigin || in.ProofBasis != c.wantBasis || in.Entrypoint != c.entrypoint {
				t.Fatalf("%+v", in)
			}
			kind := queryString(t, e.s.db, "SELECT history_kind||'|'||origin FROM items WHERE message_id=?", in.MessageID)
			if kind != c.wantKind+"|"+c.wantOrigin {
				t.Fatalf("item %s", kind)
			}
		})
	}
}

// A proof for this thread and key, signed with the relay key, for the text.
func (e *env) proof(thread, key, text, nonce, origin string, issued time.Time, ttl time.Duration) map[string]any {
	sum := sha256.Sum256([]byte(text))
	p := protocol.OriginProof{
		Issuer: "core:fixture", KeyID: "fixture-key-1", Audience: "core:local", Origin: origin,
		SourceMessageID: identity.NewMessageID().String(), SourceThreadID: identity.NewThreadID().String(),
		DestinationThreadID: thread, MutationKey: key, RawHash: hex.EncodeToString(sum[:]), Sequence: 1,
		IssuedAt: protocol.FormatTimestamp(issued), ExpiresAt: protocol.FormatTimestamp(issued.Add(ttl)), Nonce: nonce,
	}
	mac, err := protocol.OriginProofMAC(e.key, p)
	if err != nil {
		e.t.Fatal(err)
	}
	p.MAC = mac
	raw, _ := json.Marshal(p)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

func (e *env) withProof(proof func(text string) map[string]any) func(m map[string]any) {
	return func(m map[string]any) {
		in := m["input"].(map[string]any)
		in["origin_proof"] = proof(in["text"].(string))
	}
}

func TestRelayAdmissionVerifiesProofAndRecordsNonce(t *testing.T) {
	e := newEnv(t)
	const key = "relay.key.00000000000001"
	mod := e.withProof(func(text string) map[string]any {
		return e.proof(e.thread, key, text, "nonce-aaaaaaaaaaaaaaaa", "human", testNow.Add(-time.Minute), 5*time.Minute)
	})
	out := e.mustAdmit(e.thread, key, mod)
	in := out.Result.Intake
	if in.ProofBasis != "verified_relay" || in.EffectiveOrigin != "human" || in.ProofDigest == nil {
		t.Fatalf("%+v", in)
	}
	// The digest is of the proof as sent: recompute it from the stored record.
	var rec struct {
		Proof *protocol.OriginProof `json:"origin_proof"`
		Hash  string                `json:"mutation_payload_hash"`
	}
	if err := json.Unmarshal([]byte(queryString(t, e.s.db, "SELECT metadata_json FROM items WHERE message_id=?", in.MessageID)), &rec); err != nil || rec.Proof == nil {
		t.Fatalf("%v %+v", err, rec)
	}
	if d, _ := protocol.OriginProofDigest(*rec.Proof); d != *in.ProofDigest {
		t.Fatalf("proof digest %s, receipt %s", d, *in.ProofDigest)
	}
	if v := queryString(t, e.s.db, "SELECT issuer||'|'||key_id||'|'||nonce||'|'||receipt_id FROM relay_nonces"); v != "core:fixture|fixture-key-1|nonce-aaaaaaaaaaaaaaaa|"+out.Result.ReceiptID {
		t.Fatalf("nonce row %q", v)
	}
	if kind := queryString(t, e.s.db, "SELECT history_kind FROM items WHERE message_id=?", in.MessageID); kind != "HumanInstruction" {
		t.Fatal(kind)
	}
}

func TestRelayReplayWorksAfterExpiryAndNonceUse(t *testing.T) {
	e := newEnv(t)
	const key = "relay.key.00000000000002"
	// A client keeps and resends the first payload byte for byte: the proof is made once.
	signed := map[string]map[string]any{}
	mod := e.withProof(func(text string) map[string]any {
		if signed[text] == nil {
			signed[text] = e.proof(e.thread, key, text, "nonce-bbbbbbbbbbbbbbbb", "human", testNow, 5*time.Minute)
		}
		return signed[text]
	})
	first := e.mustAdmit(e.thread, key, mod)
	before := e.counts()
	e.s.clock = intake.FixedClock(testNow.Add(2 * time.Hour)) // far past expiry
	again, err := e.admit(e.thread, key, mod)
	if err != nil || !again.Replayed || !reflect.DeepEqual(again.Result, first.Result) {
		t.Fatalf("a confirmed receipt must be retrievable after expiry: %v %+v", err, again)
	}
	e.wantNoChange(before)
	// A new proof (new nonce, same key) is a different payload: conflict, not a second Run.
	_, err = e.admit(e.thread, key, e.withProof(func(text string) map[string]any {
		return e.proof(e.thread, key, text, "nonce-cccccccccccccccc", "human", testNow.Add(2*time.Hour), 5*time.Minute)
	}))
	wantCode(t, err, protocol.CodeIdempotencyConflict)
	e.wantNoChange(before)
}

func TestRelayProofRejectionsThroughAdmission(t *testing.T) {
	reasons := map[string]string{
		"expired": "expired", "issued in the future": "in the future", "ttl too long": "longer than 300", "another thread": "different thread",
		"another operation key": "different operation", "another text": "cover this text", "tampered mac": "MAC", "unknown issuer": "allowed relay",
	}
	cases := map[string]func(e *env, thread, key string) func(m map[string]any){
		"expired": func(e *env, thread, key string) func(m map[string]any) {
			return e.withProof(func(text string) map[string]any {
				return e.proof(thread, key, text, "nonce-dddddddddddddddd", "human", testNow.Add(-10*time.Minute), 5*time.Minute)
			})
		},
		"issued in the future": func(e *env, thread, key string) func(m map[string]any) {
			return e.withProof(func(text string) map[string]any {
				return e.proof(thread, key, text, "nonce-dddddddddddddddd", "human", testNow.Add(time.Minute), 5*time.Minute)
			})
		},
		"ttl too long": func(e *env, thread, key string) func(m map[string]any) {
			return e.withProof(func(text string) map[string]any {
				return e.proof(thread, key, text, "nonce-dddddddddddddddd", "human", testNow, 301*time.Second)
			})
		},
		"another thread": func(e *env, thread, key string) func(m map[string]any) {
			return e.withProof(func(text string) map[string]any {
				return e.proof(identity.NewThreadID().String(), key, text, "nonce-dddddddddddddddd", "human", testNow, time.Minute)
			})
		},
		"another operation key": func(e *env, thread, key string) func(m map[string]any) {
			return e.withProof(func(text string) map[string]any {
				return e.proof(thread, "some.other.operation.key", text, "nonce-dddddddddddddddd", "human", testNow, time.Minute)
			})
		},
		"another text": func(e *env, thread, key string) func(m map[string]any) {
			return e.withProof(func(text string) map[string]any {
				return e.proof(thread, key, text+" ", "nonce-dddddddddddddddd", "human", testNow, time.Minute)
			})
		},
		"tampered mac": func(e *env, thread, key string) func(m map[string]any) {
			return e.withProof(func(text string) map[string]any {
				p := e.proof(thread, key, text, "nonce-dddddddddddddddd", "human", testNow, time.Minute)
				p["mac"] = strings.Repeat("0", 64)
				return p
			})
		},
		"unknown issuer": func(e *env, thread, key string) func(m map[string]any) {
			return e.withProof(func(text string) map[string]any {
				p := e.proof(thread, key, text, "nonce-dddddddddddddddd", "human", testNow, time.Minute)
				p["issuer"] = "core:unknown"
				return p
			})
		},
	}
	for name, mkMod := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			const key = "relay.key.00000000000003"
			before := e.counts()
			_, err := e.admit(e.thread, key, mkMod(e, e.thread, key))
			wantCode(t, err, protocol.CodeInvalidOriginProof)
			if reasons[name] == "" || !strings.Contains(err.Error(), reasons[name]) {
				t.Fatalf("rejected, but not because of %q: %v", reasons[name], err)
			}
			e.wantNoChange(before)
			// And a human claim that fails is never accepted as automation under another key.
			if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM relay_nonces") != 0 {
				t.Fatal("a rejected proof consumed a nonce")
			}
		})
	}
}

func TestRelayNonceIsSingleUse(t *testing.T) {
	e := newEnv(t)
	signed := func(thread, key string) func(m map[string]any) {
		return e.withProof(func(text string) map[string]any {
			return e.proof(thread, key, text, "nonce-eeeeeeeeeeeeeeee", "human", testNow, 5*time.Minute)
		})
	}
	e.mustAdmit(e.thread, "nonce.key.000000000001", signed(e.thread, "nonce.key.000000000001"))
	_, other := e.newThread("core:local")
	before := e.counts()
	// A fresh, correctly signed proof for another operation that reuses the nonce.
	_, err := e.admit(other, "nonce.key.000000000002", signed(other, "nonce.key.000000000002"))
	wantCode(t, err, protocol.CodeInvalidOriginProof)
	if !strings.Contains(err.Error(), "nonce was already used") {
		t.Fatalf("rejected, but not as a replayed nonce: %v", err)
	}
	e.wantNoChange(before)
}

func TestRejectedAdmissionDoesNotConsumeTheNonce(t *testing.T) {
	e := newEnv(t)
	const key = "nonce.key.000000000010"
	mod := func(extra func(m map[string]any)) func(m map[string]any) {
		sign := e.withProof(func(text string) map[string]any {
			return e.proof(e.thread, key, text, "nonce-ffffffffffffffff", "human", testNow, 5*time.Minute)
		})
		return func(m map[string]any) { extra(m); sign(m) }
	}
	// The limits are refused before the origin is looked at, so the proof is untouched ...
	_, err := e.admit(e.thread, key, mod(func(m map[string]any) { m["limits"].(map[string]any)["max_model_steps"] = 99 }))
	wantCode(t, err, protocol.CodeInvalidLimits)
	if scalar[int](t, e.s.db, "SELECT COUNT(*) FROM relay_nonces") != 0 {
		t.Fatal("nonce consumed by a refused admission")
	}
	// ... and the same proof, unchanged, works on the corrected request.
	if _, err := e.admit(e.thread, key, mod(func(m map[string]any) {})); err != nil {
		t.Fatalf("the nonce should be unused: %v", err)
	}
}

func TestAutomationProofIsVerifiedButNotHuman(t *testing.T) {
	e := newEnv(t)
	const key = "relay.key.00000000000004"
	out := e.mustAdmit(e.thread, key, e.withProof(func(text string) map[string]any {
		return e.proof(e.thread, key, text, "nonce-gggggggggggggggg", "automation", testNow, time.Minute)
	}))
	in := out.Result.Intake
	if in.ProofBasis != "verified_relay" || in.EffectiveOrigin != "automation" || in.ProofDigest == nil {
		t.Fatalf("%+v", in)
	}
}

func TestAProofWithoutConfiguredIssuersIsRejected(t *testing.T) {
	e := newEnv(t)
	e.adm.Caller = e.newCaller(func(c *intake.CallerConfig) { c.Relays = nil })
	const key = "relay.key.00000000000005"
	_, err := e.admit(e.thread, key, e.withProof(func(text string) map[string]any {
		return e.proof(e.thread, key, text, "nonce-hhhhhhhhhhhhhhhh", "human", testNow, time.Minute)
	}))
	wantCode(t, err, protocol.CodeInvalidOriginProof)
}

func TestAdmissionIsAtomic(t *testing.T) {
	e := newEnv(t)
	// Sabotage the last event insert: everything written before it must disappear.
	mustExec(t, e.s.db, `CREATE TRIGGER sabotage BEFORE INSERT ON events WHEN NEW.type='run.started'
		BEGIN SELECT RAISE(ABORT,'injected failure'); END`)
	before := e.counts()
	const key = "atomic.key.0000000000001"
	_, err := e.admit(e.thread, key, e.withProof(func(text string) map[string]any {
		return e.proof(e.thread, key, text, "nonce-iiiiiiiiiiiiiiii", "human", testNow, time.Minute)
	}))
	if err == nil {
		t.Fatal("the injected failure was not reported")
	}
	e.wantNoChange(before)
	if v := queryString(t, e.s.db, "SELECT COALESCE(active_run_id,'')||'|'||event_seq FROM threads WHERE thread_id=?", e.thread); v != "|0" {
		t.Fatalf("thread changed: %s", v)
	}
	mustExec(t, e.s.db, "DROP TRIGGER sabotage")
	// The same key, same request, now succeeds: no half-made receipt or nonce stands in the way.
	out, err := e.admit(e.thread, key, e.withProof(func(text string) map[string]any {
		return e.proof(e.thread, key, text, "nonce-iiiiiiiiiiiiiiii", "human", testNow, time.Minute)
	}))
	if err != nil || out.Replayed {
		t.Fatalf("%v %+v", err, out)
	}
	if report, err := e.s.VerifyClosure(context.Background()); err != nil || !report.OK {
		t.Fatalf("closure: %v %+v", err, report)
	}
}

// Two stores on one data root stand in for two processes: SQLite's write lock, not
// Go, must make one winner.
func TestConcurrentAdmissionsHaveOneWinner(t *testing.T) {
	e := newEnv(t)
	second, err := Open(context.Background(), e.root, Options{Clock: intake.FixedClock(testNow)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })

	t.Run("same key", func(t *testing.T) {
		stores := []*Store{e.s, second}
		var wg sync.WaitGroup
		results := make([]StartOutcome, 8)
		errs := make([]error, 8)
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i], errs[i] = stores[i%2].AdmitStart(context.Background(), e.adm, e.params(e.thread, "race.key.00000000000001", nil))
			}(i)
		}
		wg.Wait()
		fresh, replayed := 0, 0
		for i := range results {
			if errs[i] != nil {
				t.Fatalf("attempt %d: %v", i, errs[i])
			}
			if results[i].Replayed {
				replayed++
			} else {
				fresh++
			}
			if !reflect.DeepEqual(results[i].Result, results[0].Result) {
				t.Fatalf("attempt %d saw a different result", i)
			}
		}
		if fresh != 1 || replayed != 7 {
			t.Fatalf("%d accepted, %d replayed; want 1 and 7", fresh, replayed)
		}
		if n := scalar[int](t, e.s.db, "SELECT COUNT(*) FROM runs"); n != 1 {
			t.Fatalf("%d runs", n)
		}
	})
	t.Run("different keys", func(t *testing.T) {
		_, thread := e.newThread("core:local")
		stores := []*Store{e.s, second}
		var wg sync.WaitGroup
		errs := make([]error, 6)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = stores[i%2].AdmitStart(context.Background(), e.adm, e.params(thread, fmt.Sprintf("race.key.%016d", 100+i), nil))
			}(i)
		}
		wg.Wait()
		ok, busy := 0, 0
		for _, err := range errs {
			switch protocol.CodeOf(err) {
			case "":
				ok++
			case protocol.CodeBusy:
				if !strings.Contains(err.Error(), "active run") {
					t.Fatalf("BUSY must come from the active run, not from lock contention: %v", err)
				}
				busy++
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if ok != 1 || busy != 5 {
			t.Fatalf("%d accepted, %d busy; want 1 and 5", ok, busy)
		}
		if n := scalar[int](t, e.s.db, "SELECT COUNT(*) FROM runs WHERE task_id IN (SELECT task_id FROM tasks WHERE thread_id=?)", thread); n != 1 {
			t.Fatalf("%d runs on the thread", n)
		}
	})
	if report, err := e.s.VerifyClosure(context.Background()); err != nil || !report.OK {
		t.Fatalf("closure: %v %+v", err, report)
	}
}

// BEGIN IMMEDIATE means a second writer waits (or is refused as busy) as soon as the
// first transaction has begun, even before it wrote anything.
func TestWriteTransactionsBeginImmediate(t *testing.T) {
	e := newEnv(t)
	other, err := Open(context.Background(), e.root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.db.Exec("PRAGMA busy_timeout=50"); err != nil {
		t.Fatal(err)
	}
	holding := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- e.s.write(context.Background(), func(tx *sql.Tx, _ time.Time) error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding
	err = other.write(context.Background(), func(tx *sql.Tx, _ time.Time) error { return nil })
	close(release)
	if werr := <-done; werr != nil {
		t.Fatal(werr)
	}
	if protocol.CodeOf(err) != protocol.CodeBusy {
		t.Fatalf("a second writer began while the first held the write lock without writing: %v", err)
	}
}
