package service_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/service"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// hookRig is a Tool rig whose first Service has Tool runtime hooks: where the test's
// function returns tools.ErrKilled the driver stops dead, as if its process had died.
func hookRig(t *testing.T, fake *harnesstest.Fake, c toolConfig, hook func(p tools.Point, tool string) error) (*rig, *recorder) {
	t.Helper()
	r := newModelRig(t, fake, nil, c.edit)
	_ = r.writers.Close()
	r.toolHooks = tools.Hooks{Fault: hook}
	r.reconciler = tools.NewReconciler(nil)
	r.store, r.svc = r.process()
	rec := &recorder{}
	r.conn = r.svc.NewConn(rec)
	return r, rec
}

// restart is the process dying and another one starting on the same data root: the
// first process's locks are dropped, and a second Service (without the fault hooks)
// takes over. It returns a function that calls it.
func (r *rig) restart() (*service.Service, *service.Conn, *recorder) {
	r.t.Helper()
	_ = r.writers.Close()
	r.toolHooks = tools.Hooks{}
	_, svc := r.process()
	rec := &recorder{}
	conn := svc.NewConn(rec)
	mustHandle(r.t, svc, conn, "initialize", protocol.InitializeInput{ClientName: "t", ClientVersion: "0", ProtocolVersion: protocol.ProtocolVersion})
	return svc, conn, rec
}

// takeOver is a client's first request to a Thread whose driver died: the take-over
// settles the dead Run (and applies its Tool exchange, so the context moves), and a
// request that expected the context as it was is refused for it, as it must be; the
// client reads the Thread again and asks again. It returns the events the take-over
// committed (as the connection was told them) and the started Run.
func (r *rig) takeOver(svc *service.Service, conn *service.Conn, rec *recorder, thread, key string) ([]protocol.Event, protocol.StartResult) {
	r.t.Helper()
	_, err := svc.Handle(context.Background(), conn, request(r.t, "turn/start", startParamsAt(thread, key+"-probe", "probe", 999)))
	wantCode(r.t, err, protocol.CodeRevisionConflict)
	settled, _ := rec.seen()
	cur := decode[protocol.SessionInfo](r.t, mustHandle(r.t, svc, conn, "session/get", protocol.SessionGetInput{ThreadID: thread}))
	res := mustHandle(r.t, svc, conn, "turn/start", startParamsAt(thread, key, "again", cur.ContextRevision))
	res.Done()
	return settled, decode[protocol.StartResult](r.t, res)
}

// rw is a writable handle on the store, for the test to do what a cancellation does.
func (r *rig) rw() *sql.DB {
	r.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(r.dep.DataRoot, sqlite.DatabaseFile)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		r.t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	r.t.Cleanup(func() { _ = db.Close() })
	return db
}

// reached returns a hook function that tells the test when a point was reached, and
// kills the driver there.
func killAt(point tools.Point, hit chan<- string) func(tools.Point, string) error {
	var once sync.Once
	return func(p tools.Point, tool string) error {
		if p != point {
			return nil
		}
		var err error
		once.Do(func() {
			hit <- tool
			err = tools.ErrKilled
		})
		return err
	}
}

func lines(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
}

func (r *rig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func resultOfRun(t *testing.T, svc *service.Service, conn *service.Conn, runID string) protocol.RunInfo {
	t.Helper()
	return decode[protocol.RunInfo](t, mustHandle(t, svc, conn, "run/get", protocol.RunGetInput{RunID: runID}))
}

// TestAKillBetweenTheEffectAndItsRecordLeavesTheCallUnknownAndItIsNeverRunAgain is A10:
// the program ran and added its line; the process died before the result was recorded.
// The next driver settles the Run with the call's effect unknown, and does not run the
// command to find out: the file has one line, before and after.
func TestAKillBetweenTheEffectAndItsRecordLeavesTheCallUnknownAndItIsNeverRunAgain(t *testing.T) {
	hit := make(chan string, 1)
	fake := harnesstest.NewFake()
	var counter string
	r, _ := hookRig(t, fake, toolConfig{process: true}, killAt(tools.PointAfterExecute, hit))
	counter = filepath.Join(r.layout.Work, "count.log")
	fake.SetScript(calls(tc("c1", "process.exec", execArgs([]string{"append", counter}, "helper", 20))), harnesstest.Final("settled"))
	info := r.openSession("kill.open.0000000000001")
	start := r.startRun(info.ThreadID, "kill.start.000000000001", "go")
	if tool := <-hit; tool != "process.exec" {
		t.Fatalf("%s", tool)
	}
	r.waitFor("the driver to stop", func() bool { return len(fake.Generates()) == 1 })
	time.Sleep(50 * time.Millisecond)
	if lines(counter) != 1 {
		t.Fatalf("the program ran %d times", lines(counter))
	}
	// The Run is where the kill left it: executing, its call dispatched, no result.
	if run := decode[protocol.RunInfo](t, r.mustCall("run/get", protocol.RunGetInput{RunID: start.RunID})); run.Terminal || run.Phase != "Executing" {
		t.Fatalf("%+v", run)
	}
	if r.val("SELECT status||'/'||(SELECT state FROM attempts WHERE action_id=actions.action_id) FROM actions WHERE kind='tool'") != "in_flight/running" {
		t.Fatalf("%s", r.val("SELECT status FROM actions WHERE kind='tool'"))
	}

	svc2, conn2, rec2 := r.restart()
	settled, next := r.takeOver(svc2, conn2, rec2, info.ThreadID, "kill.start.000000000002")
	// The take-over committed the call's end, then the Run's.
	if got := eventTypes(settled); got != "action.completed,run.terminal" {
		t.Fatalf("%s", got)
	}
	unknown := payloadOf[protocol.ActionCompletedPayload](t, settled[0])
	old := resultOfRun(t, svc2, conn2, start.RunID)
	rr := old.Result
	if unknown.EffectState != "unknown" || unknown.CaptureComplete || unknown.ExitCode != nil || !old.Terminal || rr.Status != "blocked" || rr.Code != "EFFECT_OUTCOME_UNKNOWN" || !rr.Resumable ||
		len(rr.UnresolvedActionIDs) != 1 || rr.UnresolvedActionIDs[0] != unknown.ActionID || rr.FinalText != "" {
		t.Fatalf("%+v\n%+v", unknown, rr)
	}
	if r.val("SELECT state FROM attempts WHERE action_id=?", unknown.ActionID) != "unknown" || r.val("SELECT status FROM actions WHERE action_id=?", unknown.ActionID) != "unknown" {
		t.Fatal("the call is not recorded as unknown")
	}
	// The process had ended on its own: the host check found nothing to stop, and nothing was signalled.
	if r.val("SELECT json_extract(result_json,'$.reconcile') FROM attempts WHERE action_id=?", unknown.ActionID) != "gone" {
		t.Fatalf("verdict %s", r.val("SELECT result_json FROM attempts WHERE action_id=?", unknown.ActionID))
	}
	// What was done is in front of the next Run, which is told the outcome is unknown.
	if r.waitTerminalOn(svc2, conn2, next.RunID).Result.Status != "completed" {
		t.Fatal("the next Run did not complete")
	}
	last := fake.Generates()[len(fake.Generates())-1]
	var sawUnknown bool
	for _, v := range toolMessages(t, last) {
		sawUnknown = sawUnknown || (v.Error != nil && v.Error.Code == "EFFECT_OUTCOME_UNKNOWN")
	}
	if !sawUnknown {
		t.Fatal("the next Run's prompt does not carry the unknown outcome")
	}
	// And the command was not run again, by anyone.
	if lines(counter) != 1 {
		t.Fatalf("the program ran %d times", lines(counter))
	}
	if rep, err := r.store.VerifyClosure(t.Context()); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}

func (r *rig) waitTerminalOn(svc *service.Service, conn *service.Conn, runID string) protocol.RunInfo {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		info := resultOfRun(r.t, svc, conn, runID)
		if info.Terminal {
			return info
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the run did not end; it is in phase %s", info.Phase)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAKillAfterTheDispatchStartIsUnknownNotNotStartedEvenThoughNothingRan: the call
// never ran (the file does not exist), but nothing proves it, so it is unknown, and it
// is not run by the next driver either.
func TestAKillAfterTheDispatchStartIsUnknownNotNotStartedEvenThoughNothingRan(t *testing.T) {
	hit := make(chan string, 1)
	fake := harnesstest.NewFake()
	r, _ := hookRig(t, fake, toolConfig{}, killAt(tools.PointAfterDispatchStart, hit))
	fake.SetScript(calls(tc("c1", "file.create", createArgs("never.txt", "x"))), harnesstest.Final("ok"))
	info := r.openSession("kill2.open.000000000001")
	start := r.startRun(info.ThreadID, "kill2.start.00000000001", "go")
	<-hit
	time.Sleep(50 * time.Millisecond)
	if _, ok := r.read("never.txt"); ok {
		t.Fatal("the call ran")
	}
	svc2, conn2, rec2 := r.restart()
	settled, next := r.takeOver(svc2, conn2, rec2, info.ThreadID, "kill2.start.00000000002")
	old := resultOfRun(t, svc2, conn2, start.RunID)
	if old.Result.Status != "blocked" || old.Result.Code != "EFFECT_OUTCOME_UNKNOWN" || len(old.Result.UnresolvedActionIDs) != 1 ||
		r.val("SELECT state FROM attempts WHERE action_id=?", old.Result.UnresolvedActionIDs[0]) != "unknown" {
		t.Fatalf("%+v", old.Result)
	}
	unknown := payloadOf[protocol.ActionCompletedPayload](t, settled[0])
	if unknown.EffectState != "unknown" {
		t.Fatalf("%+v", unknown)
	}
	// Only a file tool: there was no process to check.
	if r.val("SELECT json_extract(result_json,'$.reconcile') FROM attempts WHERE action_id=?", unknown.ActionID) != "not_applicable" {
		t.Fatalf("%s", r.val("SELECT result_json FROM attempts WHERE action_id=?", unknown.ActionID))
	}
	_ = r.waitTerminalOn(svc2, conn2, next.RunID)
	if _, ok := r.read("never.txt"); ok {
		t.Fatal("the call was run by the next driver")
	}
}

// TestAKillBeforeAnyDispatchClosesTheCallsAsNotRun: bound but never dispatched, a call
// is provably not started, so the Run is only stopped, and the calls are answered as
// not run (never unknown).
func TestAKillBeforeAnyDispatchClosesTheCallsAsNotRun(t *testing.T) {
	hit := make(chan string, 1)
	fake := harnesstest.NewFake()
	r, _ := hookRig(t, fake, toolConfig{}, killAt(tools.PointAfterBind, hit))
	fake.SetScript(calls(tc("c1", "file.create", createArgs("a.txt", "x")), tc("c2", "file.create", createArgs("b.txt", "y"))), harnesstest.Final("ok"))
	info := r.openSession("kill3.open.000000000001")
	start := r.startRun(info.ThreadID, "kill3.start.00000000001", "go")
	<-hit
	time.Sleep(50 * time.Millisecond)
	// Both calls are Actions already, prepared.
	if r.val("SELECT COUNT(*) FROM attempts WHERE state='prepared'") != "2" {
		t.Fatalf("%s", r.val("SELECT group_concat(state) FROM attempts"))
	}
	svc2, conn2, rec2 := r.restart()
	settled, next := r.takeOver(svc2, conn2, rec2, info.ThreadID, "kill3.start.00000000002")
	old := resultOfRun(t, svc2, conn2, start.RunID)
	if old.Result.Status != "incomplete" || old.Result.Code != "DRIVER_STOPPED" || len(old.Result.UnresolvedActionIDs) != 0 || !old.Result.Resumable {
		t.Fatalf("%+v", old.Result)
	}
	if got := eventTypes(settled); got != "action.completed,action.completed,run.terminal" {
		t.Fatalf("%s", got)
	}
	for _, e := range settled[:2] {
		if p := payloadOf[protocol.ActionCompletedPayload](t, e); p.EffectState != "not_started" || !p.CaptureComplete {
			t.Fatalf("%+v", p)
		}
	}
	_ = r.waitTerminalOn(svc2, conn2, next.RunID)
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, ok := r.read(f); ok {
			t.Fatalf("%s was created by a call that was never dispatched", f)
		}
	}
	// The next prompt carries the two calls and their "not run" answers.
	last := fake.Generates()[len(fake.Generates())-1]
	views := toolMessages(t, last)
	if len(views) != 2 || views[0].Error == nil || views[0].Error.Code != "NOT_EXECUTED" || views[1].Error.Code != "NOT_EXECUTED" {
		t.Fatalf("%+v", views)
	}
}

// TestACancellationBeforeTheDispatchStartStopsTheCallAndOneAfterIsTooLate is STORAGE
// section 6 through a Run.
func TestACancellationBeforeTheDispatchStartStopsTheCallAndOneAfterIsTooLate(t *testing.T) {
	cancelAt := func(point tools.Point, rwdb func() *sql.DB) func(tools.Point, string) error {
		return func(p tools.Point, _ string) error {
			if p == point {
				if _, err := rwdb().Exec("UPDATE threads SET control_revision=control_revision+1"); err != nil {
					panic(err)
				}
			}
			return nil
		}
	}
	t.Run("before", func(t *testing.T) {
		fake := harnesstest.NewFake()
		var r *rig
		r, _ = hookRig(t, fake, toolConfig{}, cancelAt(tools.PointAfterBind, func() *sql.DB { return r.rw() }))
		fake.SetScript(calls(tc("c1", "file.create", createArgs("a.txt", "x"))), harnesstest.Final("ok"))
		info := r.openSession("cancel.open.00000000001")
		run := r.waitTerminal(r.startRun(info.ThreadID, "cancel.start.0000000001", "go").RunID)
		res := run.Result
		if res.Status != "cancelled" || res.Code != "CANCELLED" || len(res.UnresolvedActionIDs) != 0 || len(fake.Generates()) != 1 {
			t.Fatalf("%+v", res)
		}
		if _, ok := r.read("a.txt"); ok {
			t.Fatal("a cancelled call ran")
		}
		if r.val("SELECT state FROM attempts WHERE action_id=(SELECT action_id FROM actions WHERE kind='tool')") != "cancelled" ||
			r.val("SELECT COUNT(*) FROM events WHERE type='action.dispatch_started'") != "1" { // the model's own
			t.Fatal("the cancelled call was dispatched")
		}
		// The exchange is closed in the context, with the reason.
		next := r.startRun(info.ThreadID, "cancel.start.0000000002", "after", func(in *protocol.StartInput) {
			in.ExpectedContextRevision, in.ExpectedControlRevision = run.ContextRevision, 1
		})
		_ = r.waitTerminal(next.RunID)
		views := toolMessages(t, fake.Generates()[1])
		if len(views) != 1 || views[0].Error == nil || views[0].Error.Code != "NOT_EXECUTED" || !strings.Contains(views[0].Error.Message, "cancelled") {
			t.Fatalf("%+v", views)
		}
	})
	t.Run("after", func(t *testing.T) {
		fake := harnesstest.NewFake()
		var r *rig
		r, _ = hookRig(t, fake, toolConfig{}, cancelAt(tools.PointAfterDispatchStart, func() *sql.DB { return r.rw() }))
		fake.SetReply(calls(tc("c1", "file.create", createArgs("a.txt", "x"))))
		info := r.openSession("cancel2.open.0000000001")
		run := r.waitTerminal(r.startRun(info.ThreadID, "cancel2.start.000000001", "go").RunID)
		res := run.Result
		// The dispatch was recorded first, so the call is a call that started: it ran, and
		// its result is recorded; the Run stops at its next step.
		if res.Status != "cancelled" || res.Code != "CANCELLED" || len(res.UnresolvedActionIDs) != 0 || len(fake.Generates()) != 1 {
			t.Fatalf("%+v", res)
		}
		if c, ok := r.read("a.txt"); !ok || c != "x" {
			t.Fatal("a call that had started did not run")
		}
		if r.val("SELECT state FROM attempts WHERE action_id=(SELECT action_id FROM actions WHERE kind='tool')") != "completed" {
			t.Fatal("the result of a call that had started was not recorded")
		}
	})
}

// TestAProcessSurvivingItsDriverIsStoppedAndAReusedPIDIsLeftAlone is F29 against a real
// process: the take-over finds the Attempt running, checks the host, and stops the
// process only when its identity is the recorded one.
func TestAProcessSurvivingItsDriverIsStoppedAndAReusedPIDIsLeftAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("liveness is probed with a unix signal")
	}
	alive := processAlive
	run := func(t *testing.T, forge bool) {
		fake := harnesstest.NewFake()
		r, _ := hookRig(t, fake, toolConfig{process: true}, nil)
		fake.SetScript(calls(tc("c1", "process.exec", execArgs([]string{"sleep"}, "helper", 50))), harnesstest.Final("ok"))
		info := r.openSession("pid.open.000000000000001")
		first := r.firstService()
		start := r.startRun(info.ThreadID, "pid.start.00000000000001", "go")
		r.waitFor("the process to run", func() bool {
			return r.val("SELECT COALESCE((SELECT process_token FROM attempts WHERE state='running' AND process_token IS NOT NULL),'')") != ""
		})
		var id struct {
			PID int `json:"pid"`
		}
		if err := json.Unmarshal([]byte(r.val("SELECT process_token FROM attempts WHERE state='running'")), &id); err != nil || id.PID < 2 || !alive(id.PID) {
			t.Fatalf("%v %d", err, id.PID)
		}
		t.Cleanup(func() { killProcess(id.PID) })
		if forge {
			// The PID now belongs to a process that started later than the one the record is about.
			if _, err := r.rw().Exec(`UPDATE attempts SET process_token=json_set(process_token,'$.start','forged-an-earlier-start') WHERE state='running'`); err != nil {
				t.Fatal(err)
			}
		}
		svc2, conn2, rec2 := r.restart()
		_, next := r.takeOver(svc2, conn2, rec2, info.ThreadID, "pid.start.00000000000002")
		old := resultOfRun(t, svc2, conn2, start.RunID)
		if old.Result.Status != "blocked" || old.Result.Code != "EFFECT_OUTCOME_UNKNOWN" || len(old.Result.UnresolvedActionIDs) != 1 {
			t.Fatalf("%+v", old.Result)
		}
		verdict := r.val("SELECT json_extract(result_json,'$.reconcile') FROM attempts WHERE state='unknown'")
		if forge {
			if verdict != "gone" {
				t.Fatalf("verdict %s", verdict)
			}
			time.Sleep(200 * time.Millisecond)
			if !alive(id.PID) {
				t.Fatal("an unrelated process was stopped")
			}
		} else {
			if verdict != "stopped" {
				t.Fatalf("verdict %s", verdict)
			}
			r.waitFor("the process to be stopped", func() bool {
				reapProcess(id.PID)
				return !alive(id.PID)
			})
		}
		// The old driver, which lost the Thread, wrote nothing more: its Run has one end.
		r.mustCall("service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
		first.Quiesce()
		if r.val("SELECT COUNT(*) FROM events WHERE type='run.terminal' AND run_id=?", start.RunID) != "1" {
			t.Fatal("the Run ended more than once")
		}
		_ = r.waitTerminalOn(svc2, conn2, next.RunID)
	}
	t.Run("the recorded process", func(t *testing.T) { run(t, false) })
	t.Run("a reused PID", func(t *testing.T) { run(t, true) })
}

func (r *rig) firstService() *service.Service { return r.svc }

// TestARunThatMayChangeTheWorkspaceHoldsItsLock: a second Run in the same workspace
// that may change it is blocked (WORKSPACE_BUSY) while the first goes on, and the lock is
// released when the first ends.
func TestARunThatMayChangeTheWorkspaceHoldsItsLock(t *testing.T) {
	fake := harnesstest.NewFake()
	reached := make(chan struct{})
	release := make(chan struct{})
	var first sync.Once
	fake.OnMeasure = func(ctx context.Context) {
		held := false
		first.Do(func() { held = true })
		if !held {
			return
		}
		close(reached)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	r, _ := toolRig(t, fake, toolConfig{})
	infoA := r.openSession("lock.open.a.0000000001")
	infoB := decode[protocol.SessionOpenResult](t, r.mustCall("session/open", r.openParams("lock.open.b.0000000001"))).Session
	runA := r.startRun(infoA.ThreadID, "lock.start.a.000000001", "go")
	<-reached // A is past its load: it holds the workspace

	runB := r.startRun(infoB.ThreadID, "lock.start.b.000000001", "go")
	resB := r.waitTerminal(runB.RunID)
	if resB.Result.Status != "blocked" || resB.Result.Code != "WORKSPACE_BUSY" || !resB.Result.Resumable || len(fake.Generates()) != 0 {
		t.Fatalf("%+v", resB.Result)
	}
	// B was refused by the lock, not by anything it did: its own input is in its context.
	if resB.ContextRevision != 1 {
		t.Fatalf("%+v", resB)
	}

	close(release)
	if r.waitTerminal(runA.RunID).Result.Status != "completed" {
		t.Fatal("the first Run did not complete")
	}
	// The lock went with the first Run.
	again := r.startRun(infoB.ThreadID, "lock.start.b.000000002", "again", func(in *protocol.StartInput) { in.ExpectedContextRevision = 1 })
	if r.waitTerminal(again.RunID).Result.Status != "completed" {
		t.Fatal("the lock was not released")
	}
}

// TestPolicyChangedBetweenAdmissionAndRunBlocksTheRun: the registry is frozen into the
// Thread as a revision; a process that starts later with another registry cannot give a
// Run that Thread's authority.
func TestPolicyChangedBetweenAdmissionAndRunBlocksTheRun(t *testing.T) {
	fake := harnesstest.NewFake()
	r, _ := toolRig(t, fake, toolConfig{})
	info := r.openSession("pol.open.00000000000001")
	_ = r.writers.Close()
	// The registry now grants one more read prefix: another revision.
	pol := r.layout.Reg["policies"].([]any)[0].(map[string]any)
	pol["read_prefixes"] = []any{".", "extra"}
	r.layout.Write()
	dep, err := config.Load(r.layout.Config)
	if err != nil {
		t.Fatal(err)
	}
	r.dep = dep
	svc, conn, _ := r.restart()
	res := mustHandle(t, svc, conn, "turn/start", startParams(info.ThreadID, "pol.start.00000000000001", "go"))
	res.Done()
	run := r.waitTerminalOn(svc, conn, decode[protocol.StartResult](t, res).RunID)
	if run.Result.Status != "blocked" || run.Result.Code != "POLICY_CHANGED" || !run.Result.Resumable || len(fake.Generates()) != 0 {
		t.Fatalf("%+v", run.Result)
	}
	if r.count("tool_links") != 0 {
		t.Fatal("a Tool call was bound under a policy that is not the Run's")
	}
}
