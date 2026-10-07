package service_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/extensions"
	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/internal/kernel"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/service"
	"github.com/Nyukimin/RenCrow_Harness/internal/session"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

var testNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// rig is one Service over a throw-away deployment, with one connection.
type rig struct {
	t      *testing.T
	layout *harnesstest.Layout
	dep    *config.Deployment
	store  *sqlite.Store
	svc    *service.Service
	conn   *service.Conn
	// model, when set before process runs, is the model port of every Service the rig
	// builds; clock, when set, is the store's clock (the default is a fixed instant).
	model   modelport.ModelPort
	clock   intake.Clock
	writers *session.Writers
	// reconciler checks the processes of Tool calls a dead driver left (nil: none, for
	// the writers), and toolHooks are the Tool runtime's test seams.
	reconciler kernel.StaleReconciler
	toolHooks  tools.Hooks
	// controlPoll, when set, is how often the drivers of the rig's Services look for a
	// stop another process recorded.
	controlPoll time.Duration
	// waitHook, when set, replaces the rig's retry backoff (which by default returns at
	// once and records the delay it was asked to wait). It is called with the delay.
	waitHook func(ctx context.Context, d time.Duration) error
	// fault, when set before process runs, is the store's test seam (sqlite.Options.Fault).
	fault func(point string) error
	// extraHooks and hookHard are the Services' test seams of the host's fixed hooks: callbacks
	// registered beside the configured ones, and the longest a callback may take.
	extraHooks []extensions.Named
	hookHard   time.Duration

	waitMu sync.Mutex
	waited []time.Duration
}

// wait is the backoff of the drivers of the rig's Services: the delay is recorded and,
// unless a test holds it with waitHook, not waited for.
func (r *rig) wait(ctx context.Context, d time.Duration) error {
	r.waitMu.Lock()
	r.waited = append(r.waited, d)
	hook := r.waitHook
	r.waitMu.Unlock()
	if hook != nil {
		return hook(ctx, d)
	}
	return ctx.Err()
}

// delays are the backoffs the drivers were asked to wait, in order.
func (r *rig) delays() []time.Duration {
	r.waitMu.Lock()
	defer r.waitMu.Unlock()
	return append([]time.Duration(nil), r.waited...)
}

func newRig(t *testing.T, edit func(l *harnesstest.Layout)) *rig {
	t.Helper()
	layout := harnesstest.NewLayout(t, harnesstest.Options{CreateData: true})
	if edit != nil {
		edit(layout)
		layout.Write()
	}
	dep, err := config.Load(layout.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Init(context.Background(), dep.DataRoot); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, layout: layout, dep: dep}
	r.store, r.svc = r.process()
	r.conn = r.svc.NewConn(nil)
	return r
}

// newModelRig is newRig with a model port, so that admitted Runs are executed. An
// optional clock replaces the fixed one the store reads.
func newModelRig(t *testing.T, model modelport.ModelPort, clock intake.Clock, edit func(l *harnesstest.Layout)) *rig {
	t.Helper()
	layout := harnesstest.NewLayout(t, harnesstest.Options{CreateData: true})
	// A model rig offers no Tool unless the test's edit gives its policy some: the
	// tests of a Run that only answers do not depend on what the fixture policy lists.
	layout.Reg["policies"].([]any)[0].(map[string]any)["tools"] = []any{}
	layout.Write()
	if edit != nil {
		edit(layout)
		layout.Write()
	}
	dep, err := config.Load(layout.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Init(context.Background(), dep.DataRoot); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, layout: layout, dep: dep, model: model, clock: clock}
	r.store, r.svc = r.process()
	r.conn = r.svc.NewConn(nil)
	return r
}

// process opens the store and a Service over it, as one more process on the same data root.
func (r *rig) process() (*sqlite.Store, *service.Service) {
	r.t.Helper()
	var clock intake.Clock = intake.FixedClock(testNow)
	if r.clock != nil {
		clock = r.clock
	}
	store, err := sqlite.Open(context.Background(), r.dep.DataRoot, sqlite.Options{Clock: clock, Fault: r.fault})
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = store.Close() })
	writers, err := session.NewWriters(store, session.WithReconciler(r.reconciler))
	if err != nil {
		r.t.Fatal(err)
	}
	r.writers = writers
	r.t.Cleanup(func() { _ = writers.Close() })
	svc, err := service.New(service.Options{
		Deployment: r.dep, Store: store, Writers: writers, Entrypoint: service.StdioEntrypoint(r.dep.Caller.Principal), BuildRevision: "test-build",
		Model: r.model, Reconciler: r.reconciler, ToolHooks: r.toolHooks, Diag: r.diag, ControlPoll: r.controlPoll, RetryWait: r.wait,
		ExtraHooks: r.extraHooks, HookHardLimit: r.hookHard,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return store, svc
}

func request(t testing.TB, method string, params any) protocol.Request {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": identity.NewRequestID().String(), "method": method, "params": json.RawMessage(raw)})
	if err != nil {
		t.Fatal(err)
	}
	req, err := protocol.DecodeRequest(frame)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return req
}

func (r *rig) call(method string, params any) (service.Result, error) {
	r.t.Helper()
	return r.svc.Handle(context.Background(), r.conn, request(r.t, method, params))
}

func (r *rig) mustCall(method string, params any) service.Result {
	r.t.Helper()
	res, err := r.call(method, params)
	if err != nil {
		r.t.Fatalf("%s: %v", method, err)
	}
	return res
}

func decode[T protocol.Message](t testing.TB, res service.Result) T {
	t.Helper()
	v, err := protocol.Decode[T](res.Body)
	if err != nil {
		t.Fatalf("result is not valid: %v\n%s", err, res.Body)
	}
	return v
}

func wantCode(t testing.TB, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted, want %s", code)
	}
	if got := protocol.CodeOf(err); got != code {
		t.Fatalf("code %q (%v), want %s", got, err, code)
	}
}

func (r *rig) initialize() {
	r.t.Helper()
	r.mustCall("initialize", protocol.InitializeInput{ClientName: "test", ClientVersion: "0", ProtocolVersion: protocol.ProtocolVersion})
}

func (r *rig) binding() protocol.Binding {
	b, err := r.dep.Binding("fixture-local")
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

func (r *rig) openParams(key string) protocol.SessionOpenInput {
	return protocol.SessionOpenInput{
		WorkspacePath: r.layout.Work, Binding: r.binding(), PolicyRef: r.layout.WorkspacePolicy(), ExecutionMode: protocol.ModeTrustedHost, IdempotencyKey: key,
	}
}

func (r *rig) openSession(key string) protocol.SessionInfo {
	r.t.Helper()
	r.initialize()
	return decode[protocol.SessionOpenResult](r.t, r.mustCall("session/open", r.openParams(key))).Session
}

var startLimits = protocol.Limits{MaxModelSteps: 10, MaxToolCallsPerStep: 8, DeadlineSeconds: 1800, MaxCaptureBytes: 67108864, MaxGenerationAttempts: 32}

func startParams(thread, key, text string) protocol.StartInput {
	return protocol.StartInput{
		ThreadID: thread, Input: protocol.InputMessage{Text: text}, ContextBlocks: []protocol.ContextBlock{},
		IdempotencyKey: key, Limits: startLimits,
	}
}

func (r *rig) count(table string) int {
	r.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(r.dep.DataRoot, sqlite.DatabaseFile)+"?mode=ro")
	if err != nil {
		r.t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// The Service is transport-neutral and starts with the handshake.

func TestTheFirstRequestMustBeInitialize(t *testing.T) {
	r := newRig(t, nil)
	_, err := r.call("service/capabilities", protocol.EmptyInput{})
	wantCode(t, err, protocol.CodeInvalidRequest)
	_, err = r.call("session/open", r.openParams("handshake.key.00000001"))
	wantCode(t, err, protocol.CodeInvalidRequest)
	if r.count("sessions") != 0 {
		t.Fatal("a request before initialize changed the store")
	}

	res := r.mustCall("initialize", protocol.InitializeInput{ClientName: "claims-to-be-core", ClientVersion: "9", ProtocolVersion: protocol.ProtocolVersion})
	caps := decode[protocol.CapabilitiesResult](t, res)
	if caps.ProtocolVersion != protocol.ProtocolVersion || caps.BuildRevision != "test-build" {
		t.Fatalf("%+v", caps)
	}
	// initialize is the first request only once; the client name decides nothing.
	_, err = r.call("initialize", protocol.InitializeInput{ClientName: "x", ClientVersion: "1", ProtocolVersion: protocol.ProtocolVersion})
	wantCode(t, err, protocol.CodeInvalidRequest)
	if _, err := r.call("service/capabilities", protocol.EmptyInput{}); err != nil {
		t.Fatal(err)
	}
	// Another connection starts from the beginning.
	other := r.svc.NewConn(nil)
	_, err = r.svc.Handle(context.Background(), other, request(t, "service/capabilities", protocol.EmptyInput{}))
	wantCode(t, err, protocol.CodeInvalidRequest)
}

func TestCapabilitiesReportOnlyWhatIsImplemented(t *testing.T) {
	r := newRig(t, nil)
	r.initialize()
	caps := decode[protocol.CapabilitiesResult](t, r.mustCall("service/capabilities", protocol.EmptyInput{}))
	byName := map[string]protocol.Capability{}
	for _, c := range caps.Capabilities {
		if _, dup := byName[c.Name]; dup {
			t.Fatalf("capability %s is listed twice", c.Name)
		}
		byName[c.Name] = c
		if c.Basis != "declared" {
			t.Errorf("%s: basis %s; nothing here has an accepted contract test yet", c.Name, c.Basis)
		}
	}
	ready := []string{"initialize", "service/capabilities", "session/open", "session/list", "session/get", "turn/start", "input/append", "turn/interrupt", "run/get", "run/resume",
		"receipt/get", "events/read", "evidence/read", "service/shutdown"}
	notReady := []string{"session/fork", "context/compact"}
	for _, m := range ready {
		if c, ok := byName[m]; !ok || c.Status != "ready" {
			t.Errorf("%s: %+v", m, c)
		}
	}
	for _, m := range notReady {
		c, ok := byName[m]
		if !ok || c.Status != "unavailable" || c.Reason == nil || *c.Reason == "" {
			t.Errorf("%s: %+v", m, c)
		}
	}
	for _, f := range []string{"turn.execution", "model.generation", "context.compaction", "tool.runtime"} {
		c, ok := byName[f]
		if !ok || c.Status != "unavailable" || c.Reason == nil || *c.Reason == "" {
			t.Errorf("%s must be unavailable with a reason: %+v", f, c)
		}
	}
	if c := byName["turn/start"]; c.Reason == nil || !strings.Contains(*c.Reason, "not executed") {
		t.Fatalf("turn/start must say that it only admits: %+v", c)
	}
	if len(ready)+len(notReady) != 16 {
		t.Fatal("the test must cover all 16 methods")
	}
	// service/capabilities and initialize report the same thing.
	again := decode[protocol.CapabilitiesResult](t, r.mustCall("service/capabilities", protocol.EmptyInput{}))
	if !reflect.DeepEqual(again, caps) {
		t.Fatal("capabilities changed between two calls")
	}
}

func TestMethodsOfTheServiceAreExactlyTheSixteenNativeMethods(t *testing.T) {
	methods := []string{"initialize", "service/capabilities", "session/open", "session/list", "session/get", "session/fork", "turn/start", "input/append",
		"turn/interrupt", "run/get", "run/resume", "context/compact", "receipt/get", "events/read", "evidence/read", "service/shutdown"}
	for _, m := range methods {
		if !service.HasMethod(m) {
			t.Errorf("%s is not a method of the service", m)
		}
	}
	for _, m := range []string{"", "turn/stop", "Initialize", "session/open ", "rpc.discover", "tools/call"} {
		if service.HasMethod(m) {
			t.Errorf("%q must not be a method", m)
		}
	}
}

// TestMethodsThatNeedAModelPortAreRefusedWithoutOneAndWriteNothing: in a process that has no
// model port (a read-only command), the methods that count and compact are refused with the
// capability's own reason, and nothing is written, so that a process that has one answers the same
// key properly later.
func TestMethodsThatNeedAModelPortAreRefusedWithoutOneAndWriteNothing(t *testing.T) {
	r := newRig(t, nil)
	info := r.openSession("unimpl.open.0000001")
	start := decode[protocol.StartResult](t, r.mustCall("turn/start", startParams(info.ThreadID, "unimpl.start.00001", "work")))
	tables := []string{"sessions", "threads", "receipts", "events", "runs", "items", "queue_inputs", "checkpoints", "evidence"}
	before := map[string]int{}
	for _, tb := range tables {
		before[tb] = r.count(tb)
	}
	caps := decode[protocol.CapabilitiesResult](t, r.mustCall("service/capabilities", protocol.EmptyInput{}))
	reasons := map[string]string{}
	for _, c := range caps.Capabilities {
		if c.Reason != nil {
			reasons[c.Name] = *c.Reason
		}
	}

	key := "unimpl.key.000000000001"
	_ = start
	cases := map[string]any{
		"session/fork":    protocol.SessionForkInput{ThreadID: info.ThreadID, CheckpointID: "ckp_00000000-0000-7000-8000-000000000001", IdempotencyKey: key},
		"context/compact": protocol.CompactInput{ThreadID: info.ThreadID, DryRun: true, IdempotencyKey: key},
	}
	for method, params := range cases {
		_, err := r.call(method, params)
		wantCode(t, err, protocol.CodeUnsupportedContract)
		var pe *protocol.Error
		if !asError(err, &pe) || pe.Retryable {
			t.Errorf("%s: a missing feature is not retryable: %v", method, err)
		}
		if reason, ok := reasons[method]; !ok || !strings.Contains(err.Error(), reason) {
			t.Errorf("%s: the refusal %q must carry the capability reason %q", method, err, reasons[method])
		}
	}
	for _, tb := range tables {
		if got := r.count(tb); got != before[tb] {
			t.Errorf("table %s changed: %d -> %d", tb, before[tb], got)
		}
	}
	// A 3b-ready retry of the same key later is not shadowed by a stored refusal.
	if r.count("receipts") != before["receipts"] {
		t.Fatal("a refusal stored a receipt")
	}
}

func asError(err error, target **protocol.Error) bool {
	for err != nil {
		if pe, ok := err.(*protocol.Error); ok {
			*target = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestSessionOpenListGet(t *testing.T) {
	r := newRig(t, nil)
	r.initialize()
	res := r.mustCall("session/open", r.openParams("open.key.0000000000001"))
	open := decode[protocol.SessionOpenResult](t, res)
	info := open.Session
	real, err := filepath.EvalSymlinks(r.layout.Work)
	if err != nil {
		t.Fatal(err)
	}
	if info.WorkspacePath != real || info.Binding != r.binding() || info.ExecutionMode != protocol.ModeTrustedHost || info.PolicyRef != r.layout.WorkspacePolicy() {
		t.Fatalf("%+v", info)
	}
	if len(res.Events) != 1 || res.Events[0].Type != protocol.EventSessionCreated || res.Events[0].ThreadID != info.ThreadID {
		t.Fatalf("the committed event must be handed out for notification: %+v", res.Events)
	}

	again := r.mustCall("session/open", r.openParams("open.key.0000000000001"))
	if string(again.Body) != string(res.Body) || len(again.Events) != 0 {
		t.Fatalf("a replay must return the original result and announce nothing:\n%s\n%s", again.Body, res.Body)
	}
	other := r.openParams("open.key.0000000000001")
	other.ExecutionMode = protocol.ModeStructuredOnly
	_, err = r.call("session/open", other)
	wantCode(t, err, protocol.CodeIdempotencyConflict)

	got := decode[protocol.SessionInfo](t, r.mustCall("session/get", protocol.SessionGetInput{ThreadID: info.ThreadID}))
	if !reflect.DeepEqual(got, info) {
		t.Fatalf("%+v\n%+v", got, info)
	}
	list := decode[protocol.SessionListResult](t, r.mustCall("session/list", protocol.SessionListInput{Limit: 10}))
	if len(list.Sessions) != 1 || list.Sessions[0].ThreadID != info.ThreadID || list.NextCursor != nil {
		t.Fatalf("%+v", list)
	}
	_, err = r.call("session/get", protocol.SessionGetInput{ThreadID: "thr_00000000-0000-7000-8000-0000000000aa"})
	wantCode(t, err, protocol.CodeForbidden)
}

func TestSessionOpenRefusesWhatTheHostDoesNotAllow(t *testing.T) {
	r := newRig(t, func(l *harnesstest.Layout) {
		// isolated is allowed by configuration, but this build has no isolation adapter.
		l.Cfg["workspaces"].([]any)[0].(map[string]any)["allowed_modes"] = []any{"structured_only", "trusted_host", "isolated"}
		l.Reg["policies"].([]any)[0].(map[string]any)["allowed_modes"] = []any{"structured_only", "trusted_host", "isolated"}
	})
	r.initialize()
	before := r.count("sessions")

	cases := map[string]struct {
		mod  func(in *protocol.SessionOpenInput)
		code string
	}{
		"unknown binding":  {func(in *protocol.SessionOpenInput) { in.Binding.Selector = "not-configured" }, protocol.CodeForbidden},
		"binding revision": {func(in *protocol.SessionOpenInput) { in.Binding.ProfileRevision = "other" }, protocol.CodeForbidden},
		"alias claim": {func(in *protocol.SessionOpenInput) {
			in.Binding.Kind = "alias"
			in.Binding.AgentID = protocol.Str("a")
			in.Binding.ExecutionRole = protocol.Str("r")
		}, protocol.CodeForbidden},
		"unconfigured path":       {func(in *protocol.SessionOpenInput) { in.WorkspacePath = filepath.Join(r.layout.Dir, "elsewhere") }, protocol.CodeForbidden},
		"subdirectory":            {func(in *protocol.SessionOpenInput) { in.WorkspacePath = filepath.Join(r.layout.Work, "sub") }, protocol.CodeForbidden},
		"the data root":           {func(in *protocol.SessionOpenInput) { in.WorkspacePath = r.dep.DataRoot }, protocol.CodeForbidden},
		"relative path":           {func(in *protocol.SessionOpenInput) { in.WorkspacePath = "work" }, protocol.CodeForbidden},
		"unregistered policy":     {func(in *protocol.SessionOpenInput) { in.PolicyRef = "no-such-policy" }, protocol.CodeForbidden},
		"policy ref with space":   {func(in *protocol.SessionOpenInput) { in.PolicyRef = "fixture-workspace-write " }, protocol.CodeForbidden},
		"isolated has no adapter": {func(in *protocol.SessionOpenInput) { in.ExecutionMode = protocol.ModeIsolated }, protocol.CodeUnsupportedContract},
	}
	for name, c := range cases {
		in := r.openParams("refusal." + strings.ReplaceAll(name, " ", "") + ".0001")
		c.mod(&in)
		_, err := r.call("session/open", in)
		if protocol.CodeOf(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
			continue
		}
		if strings.Contains(err.Error(), r.layout.Dir) {
			t.Errorf("%s: the refusal leaks a path: %v", name, err)
		}
	}
	if r.count("sessions") != before {
		t.Fatal("a refused session/open changed the store")
	}
}

func TestSessionOpenResolvesAWorkspaceSymlinkToItsRealRoot(t *testing.T) {
	r := newRig(t, nil)
	r.initialize()
	link := filepath.Join(r.layout.Dir, "work-link")
	if err := os.Symlink(r.layout.Work, link); err != nil {
		t.Skip("symbolic links are not available")
	}
	in := r.openParams("symlink.key.000000001")
	in.WorkspacePath = link
	open := decode[protocol.SessionOpenResult](t, r.mustCall("session/open", in))
	if open.Session.WorkspacePath != r.layout.Work {
		t.Fatalf("the session must record the real root: %s", open.Session.WorkspacePath)
	}
}

func TestTurnStartAdmitsAndTheRunStaysInAdmitting(t *testing.T) {
	r := newRig(t, nil)
	info := r.openSession("flow.open.00000000001")

	res := r.mustCall("turn/start", startParams(info.ThreadID, "flow.start.0000000001", "build the thing"))
	start := decode[protocol.StartResult](t, res)
	if !start.Accepted || start.ThreadID != info.ThreadID || start.SessionID != info.SessionID || start.EffectiveLimits != startLimits {
		t.Fatalf("%+v", start)
	}
	in := start.Intake
	if in.Principal != r.dep.Caller.Principal || in.Entrypoint != protocol.EntrypointStdioAutomation || in.CallerProfileDigest != r.dep.Caller.ProfileDigest ||
		in.EffectiveOrigin != protocol.OriginAutomation || in.ProofBasis != protocol.ProofBasisAutomation {
		t.Fatalf("the origin and the entrypoint come from the profile, not the client: %+v", in)
	}
	if types := eventTypes(res.Events); types != "input.accepted,task.created,run.started" {
		t.Fatalf("events %s", types)
	}

	run := decode[protocol.RunInfo](t, r.mustCall("run/get", protocol.RunGetInput{RunID: start.RunID}))
	if run.Phase != "Admitting" || run.Terminal || run.Result != nil || run.ThreadID != info.ThreadID || run.EffectiveLimits != startLimits {
		t.Fatalf("%+v", run)
	}
	rec := decode[protocol.ReceiptRecord](t, r.mustCall("receipt/get", protocol.ReceiptGetInput{ReceiptID: start.ReceiptID}))
	if rec.Operation != "turn/start" || rec.Stage != "accepted" || rec.Result == nil {
		t.Fatalf("%+v", rec)
	}
	if sr, err := rec.Result.StartResult(); err != nil || !reflect.DeepEqual(sr, start) {
		t.Fatalf("%v %+v", err, sr)
	}
	// The session now shows its active Run.
	got := decode[protocol.SessionInfo](t, r.mustCall("session/get", protocol.SessionGetInput{ThreadID: info.ThreadID}))
	if got.ActiveRunID == nil || *got.ActiveRunID != start.RunID {
		t.Fatalf("%+v", got)
	}

	// Replay: the original, nothing new, nothing announced.
	rows := r.count("receipts")
	again := r.mustCall("turn/start", startParams(info.ThreadID, "flow.start.0000000001", "build the thing"))
	if string(again.Body) != string(res.Body) || len(again.Events) != 0 || r.count("receipts") != rows {
		t.Fatal("a replay must return the original result and add nothing")
	}
	_, err := r.call("turn/start", startParams(info.ThreadID, "flow.start.0000000001", "build another thing"))
	wantCode(t, err, protocol.CodeIdempotencyConflict)
	// A new key while the Run is active: BUSY, as PROTOCOL says (use input/append).
	_, err = r.call("turn/start", startParams(info.ThreadID, "flow.start.0000000002", "second"))
	wantCode(t, err, protocol.CodeBusy)
	// Not a thread of this caller's scope.
	_, err = r.call("turn/start", startParams("thr_00000000-0000-7000-8000-0000000000aa", "flow.start.0000000003", "x"))
	wantCode(t, err, protocol.CodeForbidden)
}

func TestStartLimitsAboveTheHostsAreRefusedWhole(t *testing.T) {
	r := newRig(t, nil)
	info := r.openSession("limits.open.000000001")
	over := startParams(info.ThreadID, "limits.start.00000001", "x")
	over.Limits.MaxModelSteps = 11
	_, err := r.call("turn/start", over)
	wantCode(t, err, protocol.CodeInvalidLimits)
	if r.count("runs") != 0 || r.count("receipts") != 1 {
		t.Fatal("a refused start left rows behind")
	}
	// The same key is still free for a request that fits.
	r.mustCall("turn/start", startParams(info.ThreadID, "limits.start.00000001", "x"))
}

func eventTypes(events []protocol.Event) string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	return strings.Join(types, ",")
}

func TestEventsReadPagesAndReturnsConfirmedEventsOnly(t *testing.T) {
	r := newRig(t, nil)
	info := r.openSession("events.open.0000000001")
	r.mustCall("turn/start", startParams(info.ThreadID, "events.start.000000001", "go"))

	all := decode[protocol.EventsReadResult](t, r.mustCall("events/read", protocol.EventsReadInput{ThreadID: info.ThreadID, AfterSeq: 0, Limit: 100}))
	if eventTypes(all.Events) != "session.created,input.accepted,task.created,run.started" || all.HasMore || all.NextAfterSeq != 4 {
		t.Fatalf("%+v", all)
	}
	first := decode[protocol.EventsReadResult](t, r.mustCall("events/read", protocol.EventsReadInput{ThreadID: info.ThreadID, AfterSeq: 0, Limit: 3}))
	if len(first.Events) != 3 || !first.HasMore || first.NextAfterSeq != 3 {
		t.Fatalf("%+v", first)
	}
	rest := decode[protocol.EventsReadResult](t, r.mustCall("events/read", protocol.EventsReadInput{ThreadID: info.ThreadID, AfterSeq: first.NextAfterSeq, Limit: 3}))
	if len(rest.Events) != 1 || rest.HasMore || rest.NextAfterSeq != 4 || rest.Events[0].Type != protocol.EventRunStarted {
		t.Fatalf("%+v", rest)
	}
	// Past the end: no events, the cursor stays where the client was.
	none := decode[protocol.EventsReadResult](t, r.mustCall("events/read", protocol.EventsReadInput{ThreadID: info.ThreadID, AfterSeq: 4, Limit: 3}))
	if none.Events == nil || len(none.Events) != 0 || none.HasMore || none.NextAfterSeq != 4 {
		t.Fatalf("%+v", none)
	}
	far := decode[protocol.EventsReadResult](t, r.mustCall("events/read", protocol.EventsReadInput{ThreadID: info.ThreadID, AfterSeq: 99, Limit: 3}))
	if len(far.Events) != 0 || far.NextAfterSeq != 99 {
		t.Fatalf("%+v", far)
	}
	// A thread outside the scope looks the same as one that does not exist.
	_, err := r.call("events/read", protocol.EventsReadInput{ThreadID: "thr_00000000-0000-7000-8000-0000000000aa", AfterSeq: 0, Limit: 3})
	wantCode(t, err, protocol.CodeForbidden)
}

func TestEvidenceReadReturnsStoredBytesWithoutRunningAnything(t *testing.T) {
	r := newRig(t, nil)
	info := r.openSession("evid.open.00000000001")
	text := "日本語のinput本文"
	start := decode[protocol.StartResult](t, r.mustCall("turn/start", startParams(info.ThreadID, "evid.start.000000001", text)))
	id := start.Intake.EvidenceID

	whole := decode[protocol.EvidenceReadResult](t, r.mustCall("evidence/read", protocol.EvidenceReadInput{
		EvidenceID: id, ProjectionVersion: "text/v1", Range: protocol.ByteRange{Start: 0, End: uint64(len(text))}}))
	if whole.Partial || whole.TotalBytes != int64(len(text)) || !whole.CaptureComplete {
		t.Fatalf("%+v", whole)
	}
	// 日 is three bytes: a text range may not start inside it.
	_, err := r.call("evidence/read", protocol.EvidenceReadInput{EvidenceID: id, ProjectionVersion: "text/v1", Range: protocol.ByteRange{Start: 1, End: 3}})
	wantCode(t, err, protocol.CodeInvalidRange)
	_, err = r.call("evidence/read", protocol.EvidenceReadInput{EvidenceID: id, ProjectionVersion: "raw/v1", Range: protocol.ByteRange{Start: 1, End: 3}})
	if err != nil {
		t.Fatalf("raw bytes have no character boundary: %v", err)
	}
	_, err = r.call("evidence/read", protocol.EvidenceReadInput{EvidenceID: id, ProjectionVersion: "raw/v1", Range: protocol.ByteRange{Start: 0, End: 1 << 20}})
	wantCode(t, err, protocol.CodeInvalidRange)
	_, err = r.call("evidence/read", protocol.EvidenceReadInput{EvidenceID: "evd_00000000-0000-7000-8000-0000000000aa", ProjectionVersion: "raw/v1", Range: protocol.ByteRange{Start: 0, End: 1}})
	wantCode(t, err, protocol.CodeForbidden)
}

func TestShutdownStopsNewWorkAndKeepsReadsWorking(t *testing.T) {
	r := newRig(t, nil)
	info := r.openSession("down.open.000000000001")

	res := r.mustCall("service/shutdown", protocol.ShutdownInput{Mode: "drain", DeadlineSeconds: 7})
	if got := decode[protocol.ShutdownResult](t, res); !got.Accepted {
		t.Fatalf("%+v", got)
	}
	if res.Shutdown == nil || res.Shutdown.Mode != "drain" || res.Shutdown.Deadline != 7*time.Second {
		t.Fatalf("the transport must be told how to stop: %+v", res.Shutdown)
	}
	// Idempotent: the accepted state is returned again, not a new shutdown.
	again := r.mustCall("service/shutdown", protocol.ShutdownInput{Mode: "cancel", DeadlineSeconds: 1})
	if string(again.Body) != string(res.Body) || again.Shutdown == nil || again.Shutdown.Mode != "drain" || again.Shutdown.Deadline != 7*time.Second {
		t.Fatalf("a second shutdown must return the accepted one: %+v %s", again.Shutdown, again.Body)
	}

	rows := r.count("sessions")
	_, err := r.call("session/open", r.openParams("down.open.000000000002"))
	wantCode(t, err, protocol.CodeBusy)
	_, err = r.call("turn/start", startParams(info.ThreadID, "down.start.00000000001", "late"))
	wantCode(t, err, protocol.CodeBusy)
	if r.count("sessions") != rows || r.count("runs") != 0 {
		t.Fatal("work was admitted after shutdown")
	}
	if _, err := r.call("session/get", protocol.SessionGetInput{ThreadID: info.ThreadID}); err != nil {
		t.Fatalf("reads keep working while draining: %v", err)
	}
}

func TestAnotherProcessThatDrivesTheThreadMakesNewStartsBusyButReplaysStillAnswer(t *testing.T) {
	r := newRig(t, nil)
	info := r.openSession("dual.open.00000000001")
	first := r.mustCall("turn/start", startParams(info.ThreadID, "dual.start.0000000001", "first"))

	// A second process on the same data root, with its own connection.
	_, svc2 := r.process()
	conn2 := svc2.NewConn(nil)
	call2 := func(method string, params any) (service.Result, error) {
		return svc2.Handle(context.Background(), conn2, request(t, method, params))
	}
	if _, err := call2("initialize", protocol.InitializeInput{ClientName: "b", ClientVersion: "1", ProtocolVersion: protocol.ProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	// A new request needs the writer role, which the first process holds.
	rows := r.count("receipts")
	_, err := call2("turn/start", startParams(info.ThreadID, "dual.start.0000000002", "second"))
	wantCode(t, err, protocol.CodeBusy)
	var pe *protocol.Error
	if !asError(err, &pe) || !pe.Retryable {
		t.Fatalf("BUSY from a held lock is retryable: %v", err)
	}
	// A request that was already accepted is answered from its receipt, without the role.
	res, err := call2("turn/start", startParams(info.ThreadID, "dual.start.0000000001", "first"))
	if err != nil || string(res.Body) != string(first.Body) || len(res.Events) != 0 {
		t.Fatalf("%v\n%s\n%s", err, res.Body, first.Body)
	}
	// The same key with another payload is a conflict even there.
	_, err = call2("turn/start", startParams(info.ThreadID, "dual.start.0000000001", "changed"))
	wantCode(t, err, protocol.CodeIdempotencyConflict)
	if r.count("receipts") != rows {
		t.Fatal("the busy process wrote something")
	}
}

func TestTheDriverRoleIsTakenOnlyForRequestsThatNeedIt(t *testing.T) {
	r := newRig(t, nil)
	info := r.openSession("role.open.0000000001")
	// session/open, reads and refusals never take the writer role: the first
	// turn/start still finds the thread at epoch 0 and raises it to 1.
	_, _ = r.call("turn/start", startParams("thr_00000000-0000-7000-8000-0000000000aa", "role.start.000000001", "x"))
	r.mustCall("session/get", protocol.SessionGetInput{ThreadID: info.ThreadID})
	r.mustCall("turn/start", startParams(info.ThreadID, "role.start.000000002", "x"))
	epoch, err := r.store.BumpWriterEpoch(context.Background(), info.ThreadID)
	if err != nil || epoch != 2 {
		t.Fatalf("epoch %d %v: the thread must have been driven exactly once", epoch, err)
	}
}

func TestStdioEntrypointComesFromTheProfileNamespace(t *testing.T) {
	for principal, want := range map[string]string{
		"core:local":   protocol.EntrypointStdioCore,
		"core:staging": protocol.EntrypointStdioCore,
		"user:ren":     protocol.EntrypointStdioAutomation,
		"corelike:x":   protocol.EntrypointStdioAutomation,
		"ci:core":      protocol.EntrypointStdioAutomation,
	} {
		if got := service.StdioEntrypoint(principal); got != want {
			t.Errorf("%s: %s, want %s", principal, got, want)
		}
	}
}

func TestNewRefusesAnIncompleteOrUnknownProfile(t *testing.T) {
	r := newRig(t, nil)
	good := service.Options{Deployment: r.dep, Store: r.store, Entrypoint: protocol.EntrypointStdioCore, BuildRevision: "b"}
	readOnly, err := service.New(good)
	if err != nil {
		t.Fatalf("a read-only service (no writers) is valid: %v", err)
	}
	// Such a service cannot drive a thread; it says so instead of pretending.
	conn := readOnly.NewConn(nil)
	if _, err := readOnly.Handle(context.Background(), conn, request(t, "initialize", protocol.InitializeInput{ClientName: "c", ClientVersion: "1", ProtocolVersion: protocol.ProtocolVersion})); err != nil {
		t.Fatal(err)
	}
	info := r.openSession("readonly.open.000000001")
	_, err = readOnly.Handle(context.Background(), conn, request(t, "turn/start", startParams(info.ThreadID, "readonly.start.00000001", "x")))
	wantCode(t, err, protocol.CodeUnsupportedContract)
	for name, mod := range map[string]func(o *service.Options){
		"no deployment":  func(o *service.Options) { o.Deployment = nil },
		"no store":       func(o *service.Options) { o.Store = nil },
		"bad entrypoint": func(o *service.Options) { o.Entrypoint = "stdio_other" },
		"no build":       func(o *service.Options) { o.BuildRevision = "" },
	} {
		o := good
		mod(&o)
		if _, err := service.New(o); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestTypedReadMethodsServeTheCLIWithoutAConnection(t *testing.T) {
	r := newRig(t, nil)
	info := r.openSession("cli.open.0000000000001")
	start := decode[protocol.StartResult](t, r.mustCall("turn/start", startParams(info.ThreadID, "cli.start.000000000001", "go")))
	ctx := context.Background()

	list, err := r.svc.SessionList(ctx, protocol.SessionListInput{Limit: 10})
	if err != nil || len(list.Sessions) != 1 {
		t.Fatalf("%v %+v", err, list)
	}
	run, err := r.svc.RunGet(ctx, protocol.RunGetInput{RunID: start.RunID})
	if err != nil || run.Phase != "Admitting" {
		t.Fatalf("%v %+v", err, run)
	}
	ev, err := r.svc.EvidenceRead(ctx, protocol.EvidenceReadInput{EvidenceID: start.Intake.EvidenceID, ProjectionVersion: "text/v1", Range: protocol.ByteRange{Start: 0, End: 2}})
	if err != nil || ev.TotalBytes != 2 {
		t.Fatalf("%v %+v", err, ev)
	}
}

// diag keeps the diagnostics of the drivers where a failing test can show them (in
// verbose mode only; a driver may outlive its test, and then it logs nowhere).
func (r *rig) diag(format string, args ...any) {
	if !testing.Verbose() {
		return
	}
	defer func() { _ = recover() }()
	r.t.Logf("diag: "+format, args...)
}

// TestToolRuntimeCapabilitiesFollowTheModelPortAndIsolatedIsNeverStoodInFor: the Tool
// runtime is a capability of a process that can run a Run, per mode; isolated is
// unavailable whatever else is, and says it is not replaced by another mode.
func TestToolRuntimeCapabilitiesFollowTheModelPortAndIsolatedIsNeverStoodInFor(t *testing.T) {
	status := func(r *rig) map[string]protocol.Capability {
		r.initialize()
		out := map[string]protocol.Capability{}
		for _, c := range decode[protocol.CapabilitiesResult](t, r.mustCall("service/capabilities", protocol.EmptyInput{})).Capabilities {
			out[c.Name] = c
		}
		return out
	}
	names := []string{"tool.runtime", "tool.runtime.structured_only", "tool.runtime.trusted_host", "tool.runtime.isolated"}
	without := status(newRig(t, nil))
	for _, n := range names {
		if c, ok := without[n]; !ok || c.Status != "unavailable" || c.Reason == nil || *c.Reason == "" {
			t.Errorf("without a model port, %s: %+v", n, c)
		}
	}
	with := status(newModelRig(t, harnesstest.NewFake(), nil, nil))
	for n, want := range map[string]string{"tool.runtime": "ready", "tool.runtime.structured_only": "ready", "tool.runtime.trusted_host": "ready", "tool.runtime.isolated": "unavailable"} {
		if c, ok := with[n]; !ok || c.Status != want || c.Basis != "declared" || c.Reason == nil || *c.Reason == "" {
			t.Errorf("with a model port, %s: %+v", n, c)
		}
	}
	if !strings.Contains(*with["tool.runtime.trusted_host"].Reason, "nothing isolates") || !strings.Contains(*with["tool.runtime.isolated"].Reason, "does not fall back") {
		t.Fatalf("%+v %+v", with["tool.runtime.trusted_host"], with["tool.runtime.isolated"])
	}
}
