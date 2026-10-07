package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

var testBinding = protocol.Binding{Kind: "model_route", Selector: "fixture-local", ProfileRevision: "fixture-v1"}

// sessionEnv is a store plus the caller and resolver a Service would pass to OpenSession.
type sessionEnv struct {
	*env
	work      string // a placeholder workspace root under the test's temp dir
	adm       SessionAdmission
	resolved  int
	resolveFn func(protocol.SessionOpenInput) (SessionPlan, error)
}

func newSessionEnv(t testing.TB) *sessionEnv {
	t.Helper()
	e := &sessionEnv{env: newEnv(t)}
	e.work = filepath.Join(t.TempDir(), "work")
	e.resolveFn = func(in protocol.SessionOpenInput) (SessionPlan, error) {
		return SessionPlan{WorkspacePath: e.work, PolicyRef: in.PolicyRef, ExecutionMode: in.ExecutionMode, PolicyRevision: "policy-rev-1"}, nil
	}
	e.adm = SessionAdmission{Caller: e.caller, Resolve: func(in protocol.SessionOpenInput) (SessionPlan, error) {
		e.resolved++
		return e.resolveFn(in)
	}}
	return e
}

func (e *sessionEnv) openParams(key string, mod func(m map[string]any)) []byte {
	t := e.t
	t.Helper()
	b, err := json.Marshal(protocol.SessionOpenInput{WorkspacePath: e.work, Binding: testBinding, PolicyRef: "fixture-workspace-write", ExecutionMode: "trusted_host", IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if mod == nil {
		return b
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	mod(m)
	if b, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	return b
}

func (e *sessionEnv) open(key string) SessionOutcome {
	e.t.Helper()
	out, err := e.s.OpenSession(context.Background(), e.adm, e.openParams(key, nil))
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestOpenSessionCreatesSessionThreadEventAndReceiptTogether(t *testing.T) {
	e := newSessionEnv(t)
	before := e.counts()
	out := e.open("open.key.000000000001")
	if out.Replayed {
		t.Fatal("a first open was reported as a replay")
	}
	info := out.Result.Session
	if !strings.HasPrefix(info.SessionID, "ses_") || !strings.HasPrefix(info.ThreadID, "thr_") || !strings.HasPrefix(out.Result.ReceiptID, "rcp_") {
		t.Fatalf("ids %+v", out.Result)
	}
	if info.WorkspacePath != e.work || info.PolicyRef != "fixture-workspace-write" || info.ExecutionMode != "trusted_host" ||
		info.ContextRevision != 0 || info.ControlRevision != 0 || info.ActiveRunID != nil || info.Binding != testBinding {
		t.Fatalf("session info %+v", info)
	}
	if _, err := protocol.Encode(out.Result); err != nil {
		t.Fatalf("not a valid SessionOpenResult: %v", err)
	}
	after := e.counts()
	if after["sessions"] != before["sessions"]+1 || after["threads"] != before["threads"]+1 || after["receipts"] != 1 || after["events"] != 1 ||
		after["runs"] != 0 || after["items"] != 0 {
		t.Fatalf("rows %v", after)
	}
	db := e.s.db
	if got := queryString(t, db, "SELECT principal||'|'||workspace_path||'|'||policy_ref||'|'||execution_mode FROM sessions WHERE session_id=?", info.SessionID); got != "core:local|"+e.work+"|fixture-workspace-write|trusted_host" {
		t.Fatalf("session row %s", got)
	}
	if got := queryString(t, db, "SELECT policy_revision||'|'||binding_revision||'|'||writer_epoch||'|'||event_seq||'|'||context_revision||'|'||control_revision FROM threads WHERE thread_id=?", info.ThreadID); got != "policy-rev-1|fixture-v1|0|1|0|0" {
		t.Fatalf("thread row %s", got)
	}
	if got := queryString(t, db, "SELECT operation||'|'||stage FROM receipts WHERE receipt_id=?", out.Result.ReceiptID); got != "session/open|terminal" {
		t.Fatalf("receipt row %s", got)
	}

	events, more, err := e.s.EventsAfter(context.Background(), info.ThreadID, 0, 10)
	if err != nil || more || len(events) != 1 {
		t.Fatalf("events %v %v %v", events, more, err)
	}
	ev := events[0]
	if ev.Type != protocol.EventSessionCreated || ev.EventSeq != 1 || ev.TaskID != nil || ev.RunID != nil || ev.ReceiptID == nil || *ev.ReceiptID != out.Result.ReceiptID {
		t.Fatalf("event %+v", ev)
	}
	var payload protocol.SessionCreatedPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil || payload.SessionID != info.SessionID || payload.Binding != testBinding || payload.Mode != "trusted_host" {
		t.Fatalf("payload %+v %v", payload, err)
	}
	if len(out.Events) != 1 || !reflect.DeepEqual(out.Events[0], ev) {
		t.Fatalf("the outcome must carry the committed event exactly as events/read returns it: %+v", out.Events)
	}
	if report, err := e.s.VerifyClosure(context.Background()); err != nil || !report.OK {
		t.Fatalf("closure %v %+v", err, report)
	}
}

func TestOpenSessionReplayIsAnsweredBeforeThePlanIsResolved(t *testing.T) {
	e := newSessionEnv(t)
	first := e.open("open.key.000000000002")
	if e.resolved != 1 {
		t.Fatalf("resolved %d", e.resolved)
	}
	before := e.counts()
	// Whatever the configuration says now, the original answer stands.
	e.resolveFn = func(protocol.SessionOpenInput) (SessionPlan, error) {
		return SessionPlan{}, protocol.NewError(protocol.CodeForbidden, "the workspace is no longer configured")
	}
	again, err := e.s.OpenSession(context.Background(), e.adm, e.openParams("open.key.000000000002", nil))
	if err != nil || !again.Replayed || !reflect.DeepEqual(again.Result, first.Result) || again.Events != nil {
		t.Fatalf("replay %v %+v", err, again)
	}
	if e.resolved != 1 {
		t.Fatal("a replay resolved the plan again")
	}
	e.wantNoChange(before)
	// The same request spelled differently is the same request.
	reordered := e.openParams("open.key.000000000002", func(m map[string]any) {
		m["binding"] = map[string]any{"execution_role": nil, "agent_id": nil, "profile_revision": "fixture-v1", "selector": "fixture-local", "kind": "model_route"}
	})
	if again, err := e.s.OpenSession(context.Background(), e.adm, reordered); err != nil || !again.Replayed {
		t.Fatalf("equivalent spelling: %v %+v", err, again)
	}
}

func TestOpenSessionSameKeyOtherPayloadAndOtherOperationConflict(t *testing.T) {
	e := newSessionEnv(t)
	e.open("open.key.000000000003")
	before := e.counts()
	for name, mod := range map[string]func(m map[string]any){
		"other workspace": func(m map[string]any) { m["workspace_path"] = e.work + "-other" },
		"other mode":      func(m map[string]any) { m["execution_mode"] = "structured_only" },
		"other binding":   func(m map[string]any) { m["binding"].(map[string]any)["selector"] = "other" },
	} {
		_, err := e.s.OpenSession(context.Background(), e.adm, e.openParams("open.key.000000000003", mod))
		if protocol.CodeOf(err) != protocol.CodeIdempotencyConflict {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The same key used for turn/start is another operation: conflict, not a replay.
	_, err := e.admit(e.thread, "open.key.000000000003", nil)
	wantCode(t, err, protocol.CodeIdempotencyConflict)
	e.wantNoChange(before)
}

func TestOpenSessionRefusalsLeaveNothingBehind(t *testing.T) {
	e := newSessionEnv(t)
	before := e.counts()
	e.resolveFn = func(protocol.SessionOpenInput) (SessionPlan, error) {
		return SessionPlan{}, protocol.NewError(protocol.CodeForbidden, "workspace is not configured")
	}
	_, err := e.s.OpenSession(context.Background(), e.adm, e.openParams("open.key.000000000004", nil))
	wantCode(t, err, protocol.CodeForbidden)
	e.wantNoChange(before)
	// The refused key is still free for a request that is accepted.
	e.resolveFn = func(in protocol.SessionOpenInput) (SessionPlan, error) {
		return SessionPlan{WorkspacePath: e.work, PolicyRef: in.PolicyRef, ExecutionMode: in.ExecutionMode, PolicyRevision: "policy-rev-1"}, nil
	}
	e.open("open.key.000000000004")

	for name, params := range map[string][]byte{
		"unknown field": e.openParams("open.key.000000000005", func(m map[string]any) { m["extra"] = 1 }),
		"no key":        e.openParams("open.key.000000000005", func(m map[string]any) { delete(m, "idempotency_key") }),
		"not json":      []byte(`{"workspace_path":`),
	} {
		before := e.counts()
		_, err := e.s.OpenSession(context.Background(), e.adm, params)
		if protocol.CodeOf(err) != protocol.CodeInvalidParams {
			t.Errorf("%s: %v", name, err)
		}
		e.wantNoChange(before)
	}
	if _, err := e.s.OpenSession(context.Background(), SessionAdmission{Caller: e.caller}, e.openParams("open.key.000000000006", nil)); protocol.CodeOf(err) != protocol.CodeInternal {
		t.Fatalf("no resolver: %v", err)
	}
}

func TestIdempotencyKeysOfSessionOpenAreScopedToThePrincipal(t *testing.T) {
	e := newSessionEnv(t)
	first := e.open("open.key.000000000007")
	other := e.newCaller(func(c *intake.CallerConfig) { c.Principal = "user:ren"; c.Relays = nil })
	adm := e.adm
	adm.Caller = other
	out, err := e.s.OpenSession(context.Background(), adm, e.openParams("open.key.000000000007", nil))
	if err != nil || out.Replayed || out.Result.Session.SessionID == first.Result.Session.SessionID {
		t.Fatalf("%v %+v", err, out)
	}
	if got := queryString(t, e.s.db, "SELECT principal FROM sessions WHERE session_id=?", out.Result.Session.SessionID); got != "user:ren" {
		t.Fatalf("session owner %s", got)
	}
}

func TestGetSessionIsReadScopedAndHidesExistence(t *testing.T) {
	e := newSessionEnv(t)
	out := e.open("open.key.000000000008")
	got, err := e.s.GetSession(context.Background(), e.caller, out.Result.Session.ThreadID)
	if err != nil || !reflect.DeepEqual(got, out.Result.Session) {
		t.Fatalf("%v %+v", err, got)
	}
	// After a start the snapshot shows the active Run.
	start := e.mustAdmitOn(out.Result.Session.ThreadID, "start.key.0000000000001")
	got, err = e.s.GetSession(context.Background(), e.caller, out.Result.Session.ThreadID)
	if err != nil || got.ActiveRunID == nil || *got.ActiveRunID != start.Result.RunID {
		t.Fatalf("%v %+v", err, got)
	}
	stranger := e.newCaller(func(c *intake.CallerConfig) { c.Principal = "user:ren"; c.Relays = nil })
	_, errForbidden := e.s.GetSession(context.Background(), stranger, out.Result.Session.ThreadID)
	_, errMissing := e.s.GetSession(context.Background(), stranger, "thr_00000000-0000-7000-8000-0000000000aa")
	wantCode(t, errForbidden, protocol.CodeForbidden)
	wantCode(t, errMissing, protocol.CodeForbidden)
	if errForbidden.Error() != errMissing.Error() {
		t.Fatal("a missing and a forbidden thread must look the same")
	}
	reader := e.newCaller(func(c *intake.CallerConfig) {
		c.Principal = "user:ren"
		c.Relays = nil
		c.ReadableSessionOwners = []string{"core:local"}
	})
	if _, err := e.s.GetSession(context.Background(), reader, out.Result.Session.ThreadID); err != nil {
		t.Fatalf("a readable owner: %v", err)
	}
}

// mustAdmitOn starts a turn on thread (owned by the env's caller, writer epoch 1).
func (e *sessionEnv) mustAdmitOn(thread, key string) StartOutcome {
	e.t.Helper()
	if _, err := e.s.BumpWriterEpoch(context.Background(), thread); err != nil {
		e.t.Fatal(err)
	}
	out, err := e.s.AdmitStart(context.Background(), e.adm2(1), e.params(thread, key, func(m map[string]any) { m["upstream"] = nil }))
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *sessionEnv) adm2(epoch int64) Admission {
	a := e.env.adm
	a.WriterEpoch = epoch
	return a
}

func TestListSessionsPagesFiltersAndBindsTheCursor(t *testing.T) {
	e := newSessionEnv(t)
	var mine []string
	for i := 0; i < 5; i++ {
		mine = append(mine, e.open("list.key.00000000000"+string(rune('a'+i))).Result.Session.ThreadID)
	}
	// Threads of an owner the caller may not read never appear and never shift the pages.
	for i := 0; i < 3; i++ {
		e.newThread("user:hidden")
	}
	_, seededMine := e.newThread("core:local") // the env's own seeded thread comes first in time
	_ = seededMine

	ctx := context.Background()
	var seen []string
	var cursor *string
	for pages := 0; pages < 20; pages++ {
		res, err := e.s.ListSessions(ctx, e.caller, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if res.Sessions == nil {
			t.Fatal("sessions must be an empty array, not null")
		}
		for _, s := range res.Sessions {
			seen = append(seen, s.ThreadID)
		}
		if res.NextCursor == nil {
			break
		}
		if len(res.Sessions) != 2 {
			t.Fatalf("a page with a next cursor must be full: %d", len(res.Sessions))
		}
		cursor = res.NextCursor
	}
	for _, id := range mine {
		found := false
		for _, s := range seen {
			found = found || s == id
		}
		if !found {
			t.Fatalf("thread %s is missing from the listing %v", id, seen)
		}
	}
	if len(seen) != 7 {
		t.Fatalf("the caller reads 7 threads (5 opened, the env's seeded one and the second seeded one), got %d", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i-1] >= seen[i] {
			t.Fatalf("not strictly ordered: %v", seen)
		}
	}

	// A page that ends exactly at the last readable thread has no next cursor, even
	// though unreadable threads exist after it.
	res, err := e.s.ListSessions(ctx, e.caller, nil, 100)
	if err != nil || res.NextCursor != nil || len(res.Sessions) != 7 {
		t.Fatalf("%v %+v", err, res)
	}

	// The cursor is valid for the caller it was made for only.
	first, _ := e.s.ListSessions(ctx, e.caller, nil, 1)
	if first.NextCursor == nil {
		t.Fatal("no cursor")
	}
	other := e.newCaller(func(c *intake.CallerConfig) {
		c.Principal = "user:ren"
		c.Relays = nil
		c.ReadableSessionOwners = []string{"core:local"}
	})
	_, err = e.s.ListSessions(ctx, other, first.NextCursor, 1)
	wantCode(t, err, protocol.CodeInvalidParams)
	bad := "not-a-cursor"
	_, err = e.s.ListSessions(ctx, e.caller, &bad, 1)
	wantCode(t, err, protocol.CodeInvalidParams)
	if _, err := e.s.ListSessions(ctx, e.caller, nil, 0); protocol.CodeOf(err) != protocol.CodeInvalidParams {
		t.Fatalf("limit 0: %v", err)
	}
	none := e.newCaller(func(c *intake.CallerConfig) { c.Principal = "user:ren"; c.Relays = nil })
	if res, err := e.s.ListSessions(ctx, none, nil, 10); err != nil || len(res.Sessions) != 0 || res.NextCursor != nil || res.Sessions == nil {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestGetReceiptIsScopedToItsPrincipal(t *testing.T) {
	e := newSessionEnv(t)
	open := e.open("open.key.000000000009")
	start := e.mustAdmitOn(open.Result.Session.ThreadID, "start.key.0000000000002")

	ctx := context.Background()
	rec, err := e.s.GetReceipt(ctx, e.caller, open.Result.ReceiptID)
	if err != nil || rec.Operation != "session/open" || rec.Stage != "terminal" || rec.Result == nil || rec.Result.Type != protocol.ReceiptSessionOpenResult || rec.Error != nil {
		t.Fatalf("%v %+v", err, rec)
	}
	rec, err = e.s.GetReceipt(ctx, e.caller, start.Result.ReceiptID)
	if err != nil || rec.Operation != "turn/start" || rec.Stage != "accepted" || rec.Result == nil {
		t.Fatalf("%v %+v", err, rec)
	}
	sr, err := rec.Result.StartResult()
	if err != nil || !reflect.DeepEqual(sr, start.Result) {
		t.Fatalf("%v %+v", err, sr)
	}
	if _, err := protocol.Encode(rec); err != nil {
		t.Fatalf("not a valid ReceiptRecord: %v", err)
	}

	stranger := e.newCaller(func(c *intake.CallerConfig) {
		c.Principal = "user:ren"
		c.Relays = nil
		c.ReadableSessionOwners = []string{"core:local"}
	})
	_, errOther := e.s.GetReceipt(ctx, stranger, start.Result.ReceiptID)
	_, errMissing := e.s.GetReceipt(ctx, e.caller, "rcp_00000000-0000-7000-8000-0000000000aa")
	wantCode(t, errOther, protocol.CodeForbidden)
	wantCode(t, errMissing, protocol.CodeForbidden)
	if errOther.Error() != errMissing.Error() {
		t.Fatal("another principal's receipt and a missing receipt must look the same")
	}
}

func TestGetRun(t *testing.T) {
	e := newSessionEnv(t)
	open := e.open("open.key.00000000000a")
	thread := open.Result.Session.ThreadID
	start := e.mustAdmitOn(thread, "start.key.0000000000003")
	ctx := context.Background()

	info, err := e.s.GetRun(ctx, e.caller, start.Result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if info.RunID != start.Result.RunID || info.TaskID != start.Result.TaskID || info.ThreadID != thread || info.Phase != "Admitting" || info.Terminal || info.Result != nil ||
		info.ContextRevision != 0 || info.ControlRevision != 0 || info.EffectiveLimits != hostLimits || info.DeadlineAt != start.Result.DeadlineAt ||
		info.RecoveryPolicyRevision != designRecoveryRevision || info.GenerationAttemptsUsed != 0 || info.GenerationAttemptsUnknown != 0 {
		t.Fatalf("%+v", info)
	}
	// session.created is seq 1 and does not belong to the Run; the Run's last event is run.started (seq 4).
	if info.LastEventSeq != 4 {
		t.Fatalf("last_event_seq %d", info.LastEventSeq)
	}
	if _, err := protocol.Encode(info); err != nil {
		t.Fatalf("not a valid RunInfo: %v", err)
	}

	stranger := e.newCaller(func(c *intake.CallerConfig) { c.Principal = "user:ren"; c.Relays = nil })
	_, errOther := e.s.GetRun(ctx, stranger, start.Result.RunID)
	_, errMissing := e.s.GetRun(ctx, e.caller, "run_00000000-0000-7000-8000-0000000000aa")
	wantCode(t, errOther, protocol.CodeForbidden)
	wantCode(t, errMissing, protocol.CodeForbidden)

	// A terminal Run carries its stored result.
	result := protocol.RunResult{RunID: info.RunID, TaskID: info.TaskID, Status: "cancelled", Code: "CANCELLED", FinalText: "", Verification: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}},
		EvidenceIDs: []string{}, UnresolvedActionIDs: []string{}}
	raw, err := protocol.Encode(result)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.s.db, "UPDATE runs SET phase='Terminal', status='cancelled', ended_at=?, result_json=? WHERE run_id=?", "2026-10-07T12:01:00Z", string(raw), info.RunID)
	term, err := e.s.GetRun(ctx, e.caller, info.RunID)
	if err != nil || !term.Terminal || term.Phase != "Terminal" || term.Result == nil || term.Result.Status != "cancelled" {
		t.Fatalf("%v %+v", err, term)
	}
	// A terminal phase without a stored result is damage, never an empty answer.
	mustExec(t, e.s.db, "UPDATE runs SET result_json=NULL WHERE run_id=?", info.RunID)
	_, err = e.s.GetRun(ctx, e.caller, info.RunID)
	wantCode(t, err, protocol.CodeIntegrityBlocked)
}

func TestBumpWriterEpoch(t *testing.T) {
	e := newSessionEnv(t)
	open := e.open("open.key.00000000000b")
	thread := open.Result.Session.ThreadID
	ctx := context.Background()
	for want := int64(1); want <= 3; want++ {
		got, err := e.s.BumpWriterEpoch(ctx, thread)
		if err != nil || got != want {
			t.Fatalf("epoch %d %v, want %d", got, err, want)
		}
	}
	if _, err := e.s.BumpWriterEpoch(ctx, "thr_00000000-0000-7000-8000-0000000000aa"); err == nil {
		t.Fatal("an unknown thread got an epoch")
	}
	// A driver holding an older epoch is refused by admission.
	_, err := e.s.AdmitStart(ctx, e.adm2(1), e.params(thread, "start.key.0000000000004", func(m map[string]any) { m["upstream"] = nil }))
	wantCode(t, err, protocol.CodeRevisionConflict)
}

func TestLookupStartAnswersReplaysOnly(t *testing.T) {
	e := newSessionEnv(t)
	open := e.open("open.key.00000000000c")
	thread := open.Result.Session.ThreadID
	first := e.mustAdmitOn(thread, "start.key.0000000000005")
	ctx := context.Background()
	params := e.params(thread, "start.key.0000000000005", func(m map[string]any) { m["upstream"] = nil })
	before := e.counts()

	out, found, err := e.s.LookupStart(ctx, e.adm2(0), params)
	if err != nil || !found || !out.Replayed || !reflect.DeepEqual(out.Result, first.Result) {
		t.Fatalf("%v %v %+v", err, found, out)
	}
	_, found, err = e.s.LookupStart(ctx, e.adm2(0), e.params(thread, "start.key.0000000000006", nil))
	if err != nil || found {
		t.Fatalf("an unseen key: %v %v", err, found)
	}
	_, _, err = e.s.LookupStart(ctx, e.adm2(0), e.params(thread, "start.key.0000000000005", func(m map[string]any) { m["upstream"] = nil; m["input"].(map[string]any)["text"] = "x" }))
	wantCode(t, err, protocol.CodeIdempotencyConflict)
	// A caller with no access learns nothing, not even that the key exists.
	none := e.newCaller(func(c *intake.CallerConfig) { c.Principal = "user:ren"; c.Relays = nil })
	adm := e.adm2(0)
	adm.Caller = none
	_, _, err = e.s.LookupStart(ctx, adm, params)
	wantCode(t, err, protocol.CodeForbidden)
	e.wantNoChange(before)
}

func TestAdmitStartOutcomeCarriesItsCommittedEvents(t *testing.T) {
	e := newEnv(t)
	out := e.mustAdmit(e.thread, "events.key.000000000001", nil)
	stored, _, err := e.s.EventsAfter(context.Background(), e.thread, 0, 10)
	if err != nil || len(out.Events) != 3 || !reflect.DeepEqual(out.Events, stored) {
		t.Fatalf("%v\n%+v\n%+v", err, out.Events, stored)
	}
	again := e.mustAdmit(e.thread, "events.key.000000000001", nil)
	if !again.Replayed || again.Events != nil {
		t.Fatalf("a replay creates no events: %+v", again)
	}
}

func TestThreadAccessIsThePreLockCheck(t *testing.T) {
	// ThreadAccess is the pre-lock check of the Service.
	e := newSessionEnv(t)
	open := e.open("open.key.00000000000d")
	ctx := context.Background()
	acc, err := e.s.ThreadAccess(ctx, e.caller, open.Result.Session.ThreadID)
	if err != nil || !acc.CanControl {
		t.Fatalf("%v %+v", err, acc)
	}
	reader := e.newCaller(func(c *intake.CallerConfig) {
		c.Principal = "user:ren"
		c.Relays = nil
		c.ReadableSessionOwners = []string{"core:local"}
	})
	acc, err = e.s.ThreadAccess(ctx, reader, open.Result.Session.ThreadID)
	if err != nil || acc.CanControl {
		t.Fatalf("a reader must not control: %v %+v", err, acc)
	}
	none := e.newCaller(func(c *intake.CallerConfig) { c.Principal = "user:ren"; c.Relays = nil })
	_, err = e.s.ThreadAccess(ctx, none, open.Result.Session.ThreadID)
	wantCode(t, err, protocol.CodeForbidden)
	if errors.Is(err, ErrNotInitialized) {
		t.Fatal("unexpected")
	}
}
