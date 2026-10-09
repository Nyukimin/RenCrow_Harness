// Package service is the one place every entrypoint goes through: the stdio
// server and the CLI both call it, so they share one set of rules for access,
// idempotency, admission and what is returned.
//
// The Service holds the connection profile that was fixed when the process
// started (who the caller is, which workspaces, bindings and limits it may use).
// Nothing in a request payload can change that profile; client_name, role names
// and request text are never used to decide who someone is. The Service knows
// nothing about framing or notifications: Handle returns the result bytes and the
// events the call committed, and the transport decides how to deliver them.
//
// turn/start admits the Turn, Task and Run durably and returns; a driver of the
// work kernel then runs the Run to its end on its own goroutine when the Service was
// given a model port (`serve` gives it the RenCrow_LLM Gateway client of its
// configuration). A Service built without one (the commands that only read) admits
// and leaves the Run in phase Admitting; the next driver that takes the Thread
// settles it. run/resume admits a
// new Run of an ended Task the same way, after the checkpoint its Thread stands on was read
// back exactly. context/compact admits a system Task and Run that compacts the idle Thread
// (or, with dry_run, only looks); session/fork makes a new Thread from a checkpoint of
// another, importing the provenance its checkpoint names. turn/interrupt and input/append
// only record (a stop signal; one more input of the running Run) and need no writer role:
// the driver of the Run, here or in another process, acts on what was recorded. The
// capabilities say what is and is not there, and a method whose parts do not exist in the
// process (no model port, compaction disabled) refuses with UNSUPPORTED_CONTRACT without
// writing anything.
package service

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/extensions"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/internal/kernel"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/session"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/process"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Options configure New.
type Options struct {
	// Deployment is the validated profile the process runs as.
	Deployment *config.Deployment
	// Store is the open execution store.
	Store *sqlite.Store
	// Writers hands out the writer role of Threads. A Service without it can read
	// and open sessions but cannot admit work (the CLI's read commands).
	Writers *session.Writers
	// Entrypoint is the protocol entrypoint of the input this Service accepts,
	// fixed by whoever started the process (see StdioEntrypoint).
	Entrypoint string
	// BuildRevision identifies this build in the capabilities.
	BuildRevision string
	// Model is the model port the Runs this Service admits are executed against: the
	// RenCrow_LLM Gateway client in the production composition (rencrow-harness serve),
	// the double of internal/harnesstest in a test. Without one a Run is admitted and
	// not executed (the commands that only read have none).
	Model modelport.ModelPort
	// Diag receives short diagnostics of the drivers: never request or model text, a
	// key or a path. Nil discards them.
	Diag func(format string, args ...any)
	// Reconciler checks the processes of Tool calls a dead driver left dispatched before
	// the Runs that held them are settled. Nil is the running OS's.
	Reconciler kernel.StaleReconciler
	// ToolHooks are test seams of the Tool runtime. Production leaves them zero.
	ToolHooks tools.Hooks
	// ExtraHooks are callbacks a test registers at the host's fixed hook points beside the
	// ones the configuration lists (extensions.hooks). Production leaves it nil: the
	// configuration can only name a callback this product has.
	ExtraHooks []extensions.Named
	// HookHardLimit is the longest a hook callback may take (a test seam; zero is
	// extensions.DefaultHardLimit).
	HookHardLimit time.Duration
	// DeclaredOrigin is the operator's explicit declaration of where the inputs of this
	// process come from (the CLI's --origin): "human", "automation", "unknown" or "" for
	// none. It is never more than the caller profile allows (intake.Decide) and a stdio
	// entrypoint cannot carry one.
	DeclaredOrigin string
	// ControlPoll is how often the driver of a Run looks for a stop that another process
	// recorded (see kernel.DriverOptions). Zero is the driver's default.
	ControlPoll time.Duration
	// RetryWait is a test seam of the clock for a retry's backoff (see
	// kernel.DriverOptions). Production leaves it nil: a real timer.
	RetryWait func(ctx context.Context, d time.Duration) error
}

// ShutdownRequest tells the transport how to stop after service/shutdown.
type ShutdownRequest struct {
	Mode     string // drain or cancel
	Deadline time.Duration
}

// Result is what one call returns. Body is the canonical JSON of the result
// value. Events are the events this call committed, in order, for the transport to
// announce; a replay commits nothing and has none. Shutdown is set by
// service/shutdown.
type Result struct {
	Body     []byte
	Events   []protocol.Event
	Shutdown *ShutdownRequest
	// Delivered, when set, must be called by the transport once the response and
	// Events are queued for the client. A Run that turn/start admitted is only
	// started then, so that the events its driver commits never overtake the events
	// that announced it. A nil Result.Delivered is not an error: call Done.
	Delivered func()
}

// Done calls Delivered if there is one. It is safe to call on every Result.
func (r Result) Done() {
	if r.Delivered != nil {
		r.Delivered()
	}
}

// Notifier is how the Service reaches the client for provisional progress. It is
// implemented by the transport; confirmed events do not use it, they come back in
// Result.Events.
type Notifier interface {
	ProgressDelta(protocol.ProgressDelta)
	ProgressReset(protocol.ProgressReset)
	// Event announces one confirmed event that no call returned: the events a Run's
	// driver commits after turn/start has answered. It is called only after the
	// commit, in the order of the commits.
	Event(protocol.Event)
}

// Conn is the state of one client connection: only whether the handshake is done.
// Who the connection is comes from the profile, never from the connection.
type Conn struct {
	mu          sync.Mutex
	initialized bool
	notifier    Notifier
	svc         *Service
}

// Notifier returns the progress notifier of the connection, or nil.
func (c *Conn) Notifier() Notifier { return c.notifier }

// Close ends the connection's part in the Service: no further event or progress is
// announced to it. It is idempotent and safe on a nil Conn.
func (c *Conn) Close() {
	if c == nil || c.svc == nil {
		return
	}
	c.svc.connsMu.Lock()
	delete(c.svc.conns, c)
	c.svc.connsMu.Unlock()
}

// Service is safe for concurrent use.
type Service struct {
	dep        *config.Deployment
	store      *sqlite.Store
	writers    *session.Writers
	caller     intake.Caller
	entrypoint string
	build      string
	declared   string
	model      modelport.ModelPort
	driver     *kernel.Driver
	reconciler kernel.StaleReconciler
	diagf      func(format string, args ...any)

	mu       sync.Mutex
	shutdown *ShutdownRequest
	closing  bool
	driving  map[string]struct{} // the Runs a driver goroutine of this Service holds

	connsMu sync.Mutex
	conns   map[*Conn]struct{}

	runCtx   context.Context
	stopRuns context.CancelFunc
	runs     sync.WaitGroup
}

var entrypoints = map[string]bool{
	protocol.EntrypointCLIInteractive:  true,
	protocol.EntrypointCLIExec:         true,
	protocol.EntrypointCLIPipe:         true,
	protocol.EntrypointStdioCore:       true,
	protocol.EntrypointStdioAutomation: true,
}

// StdioEntrypoint is the entrypoint of a stdio connection, derived from the
// connection profile: the CORE profile (principal namespace "core") is stdio_core,
// any other profile is stdio_automation. It is only a label on the intake receipt;
// the origin of an input comes from the profile's default origin and from a
// verified proof, never from the entrypoint or from the client.
func StdioEntrypoint(principal string) string {
	if ns, _, ok := strings.Cut(principal, ":"); ok && ns == "core" {
		return protocol.EntrypointStdioCore
	}
	return protocol.EntrypointStdioAutomation
}

// New builds the Service of one process.
func New(o Options) (*Service, error) {
	switch {
	case o.Deployment == nil:
		return nil, errors.New("service: a deployment is required")
	case o.Store == nil:
		return nil, errors.New("service: a store is required")
	case !entrypoints[o.Entrypoint]:
		return nil, errors.New("service: unknown entrypoint")
	case o.DeclaredOrigin != "" && !slices.Contains([]string{protocol.OriginHuman, protocol.OriginAutomation, protocol.OriginUnknown}, o.DeclaredOrigin):
		return nil, errors.New("service: the declared origin is not human, automation or unknown")
	case o.DeclaredOrigin != "" && o.Entrypoint != protocol.EntrypointCLIInteractive && o.Entrypoint != protocol.EntrypointCLIExec && o.Entrypoint != protocol.EntrypointCLIPipe:
		return nil, errors.New("service: only a CLI entrypoint carries a declared origin")
	case o.BuildRevision == "" || len(o.BuildRevision) > 128:
		return nil, errors.New("service: the build revision must be 1 to 128 characters")
	}
	s := &Service{
		dep: o.Deployment, store: o.Store, writers: o.Writers, caller: o.Deployment.Caller,
		entrypoint: o.Entrypoint, build: o.BuildRevision, declared: o.DeclaredOrigin, model: o.Model, conns: map[*Conn]struct{}{}, driving: map[string]struct{}{},
	}
	s.diagf = o.Diag
	if s.reconciler = o.Reconciler; s.reconciler == nil {
		s.reconciler = tools.NewReconciler(nil)
	}
	s.runCtx, s.stopRuns = context.WithCancel(context.Background())
	if o.Model != nil {
		hooks, err := extensions.NewRunner(o.Deployment.Config.Extensions.Hooks, extensions.Options{Extra: o.ExtraHooks, HardLimit: o.HookHardLimit})
		if err != nil {
			return nil, err
		}
		rt, err := tools.NewRuntime(tools.Options{Deployment: o.Deployment, Store: o.Store, Hooks: o.ToolHooks, HookRunner: hooks, Diag: o.Diag})
		if err != nil {
			return nil, err
		}
		d, err := kernel.NewDriver(kernel.DriverOptions{
			Store: o.Store, Model: o.Model, Tools: rt, Hooks: hooks, SafetyMarginTokens: o.Deployment.Config.Compaction.SafetyMarginTokens, Compaction: o.Deployment.Config.Compaction.Enabled,
			TriggerRatio: o.Deployment.Config.Compaction.TriggerRatio,
			Initiator:    o.Deployment.Caller.Principal, Publish: s.publishEvents, Progress: s.publishProgress, Reset: s.publishReset, Diag: o.Diag, ControlPoll: o.ControlPoll,
			RetryWait: o.RetryWait,
		})
		if err != nil {
			return nil, err
		}
		s.driver = d
	}
	return s, nil
}

// NewConn starts the state of one connection. notifier may be nil. The connection
// receives the events and progress of the Runs this Service drives until it is closed.
func (s *Service) NewConn(n Notifier) *Conn {
	if s == nil { // a transport test that has no Service still gets a connection
		return &Conn{notifier: n}
	}
	c := &Conn{notifier: n, svc: s}
	s.connsMu.Lock()
	s.conns[c] = struct{}{}
	s.connsMu.Unlock()
	return c
}

// publishEvents announces committed events to every open connection that can read
// them. This Service has one caller profile, so a connection that can read one Thread
// of it can read them all.
func (s *Service) publishEvents(events []protocol.Event) {
	if len(events) == 0 {
		return
	}
	s.connsMu.Lock()
	targets := make([]Notifier, 0, len(s.conns))
	for c := range s.conns {
		if c.notifier != nil {
			targets = append(targets, c.notifier)
		}
	}
	s.connsMu.Unlock()
	for _, n := range targets {
		for _, ev := range events {
			n.Event(ev)
		}
	}
}

// publishReset tells every open connection that a retry started and the failed
// Attempt's provisional text is discarded.
func (s *Service) publishReset(r protocol.ProgressReset) {
	s.connsMu.Lock()
	targets := make([]Notifier, 0, len(s.conns))
	for c := range s.conns {
		if c.notifier != nil {
			targets = append(targets, c.notifier)
		}
	}
	s.connsMu.Unlock()
	for _, n := range targets {
		n.ProgressReset(r)
	}
}

func (s *Service) publishProgress(d protocol.ProgressDelta) {
	s.connsMu.Lock()
	targets := make([]Notifier, 0, len(s.conns))
	for c := range s.conns {
		if c.notifier != nil {
			targets = append(targets, c.notifier)
		}
	}
	s.connsMu.Unlock()
	for _, n := range targets {
		n.ProgressDelta(d)
	}
}

var methodSet = map[string]bool{
	"initialize": true, "service/capabilities": true, "session/open": true, "session/list": true, "session/get": true, "session/fork": true,
	"turn/start": true, "input/append": true, "turn/interrupt": true, "run/get": true, "run/resume": true, "context/compact": true,
	"receipt/get": true, "events/read": true, "evidence/read": true, "service/shutdown": true,
}

// HasMethod reports whether name is one of the 16 native methods. A name that is
// not is an unknown method for the transport (-32601), whatever its params are.
func HasMethod(name string) bool { return methodSet[name] }

// HasMethod is HasMethod for a transport that holds the Service.
func (s *Service) HasMethod(name string) bool { return HasMethod(name) }

func (s *Service) shuttingDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutdown != nil || s.closing
}

func (s *Service) diag(format string, args ...any) {
	if s.diagf != nil {
		s.diagf(format, args...)
	}
}

// isDriving reports whether a driver goroutine of this Service holds the Run.
func (s *Service) isDriving(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.driving[runID]
	return ok
}

// DefaultQuiesce is how long Quiesce lets running Runs finish when nobody asked for a
// shutdown (the client simply went away).
const DefaultQuiesce = 5 * time.Second

// Quiesce ends the Service's part in the Runs it drives: no new work is accepted, the
// Runs get until the grace period to finish (what service/shutdown asked for, or
// DefaultQuiesce), and any Run still going is stopped and waited for. A stopped Run
// records how it ended (stopped with its driver, or blocked with an unknown generation
// if one was in flight) before Quiesce returns. It is idempotent.
func (s *Service) Quiesce() {
	s.mu.Lock()
	s.closing = true
	grace := DefaultQuiesce
	if s.shutdown != nil {
		grace = s.shutdown.Deadline
		if s.shutdown.Mode == "cancel" {
			grace = 0
		}
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.runs.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-time.After(grace):
	}
	s.stopRuns()
	<-done
}

func errShuttingDown() error {
	return protocol.NewError(protocol.CodeBusy, "the service is shutting down and accepts no new work")
}

// Handle runs one request. The handshake comes first: until initialize has
// succeeded on the connection every other method is refused, and a second
// initialize is refused too. A failed call never leaves the handshake half done.
func (s *Service) Handle(ctx context.Context, c *Conn, req protocol.Request) (Result, error) {
	if !HasMethod(req.Method) {
		return Result{}, protocol.NewError(protocol.CodeInvalidParams, "unknown method")
	}
	params, err := req.DecodeParams()
	if err != nil {
		return Result{}, err
	}
	c.mu.Lock()
	ready := c.initialized
	c.mu.Unlock()
	if req.Method == "initialize" {
		if ready {
			return Result{}, protocol.NewError(protocol.CodeInvalidRequest, "initialize must be the first request and is accepted once")
		}
		res, err := encode(s.Capabilities())
		if err != nil {
			return Result{}, err
		}
		c.mu.Lock()
		c.initialized = true
		c.mu.Unlock()
		return res, nil
	}
	if !ready {
		return Result{}, protocol.NewError(protocol.CodeInvalidRequest, "initialize must be the first request")
	}

	switch in := params.(type) {
	case protocol.EmptyInput:
		return encode(s.Capabilities())
	case protocol.SessionOpenInput:
		out, events, err := s.SessionOpen(ctx, req.Params)
		return encodeWithEvents(out, events, err)
	case protocol.SessionListInput:
		out, err := s.SessionList(ctx, in)
		return encodeWithEvents(out, nil, err)
	case protocol.SessionGetInput:
		out, err := s.SessionGet(ctx, in)
		return encodeWithEvents(out, nil, err)
	case protocol.StartInput:
		out, events, begin, err := s.turnStart(ctx, in, req.Params)
		res, err := encodeWithEvents(out, events, err)
		if err != nil {
			// An error has no admitted Run to start; the stale Runs this call settled are
			// committed, and turnStart announced them through publishEvents.
			return res, err
		}
		res.Delivered = begin
		return res, nil
	case protocol.InterruptInput:
		out, events, err := s.TurnInterrupt(ctx, req.Params)
		return encodeWithEvents(out, events, err)
	case protocol.InputAppendInput:
		out, events, err := s.InputAppend(ctx, in, req.Params)
		return encodeWithEvents(out, events, err)
	case protocol.ResumeInput:
		out, events, begin, err := s.runResume(ctx, req.Params)
		res, err := encodeWithEvents(out, events, err)
		if err != nil {
			return res, err
		}
		res.Delivered = begin
		return res, nil
	case protocol.RunGetInput:
		out, err := s.RunGet(ctx, in)
		return encodeWithEvents(out, nil, err)
	case protocol.ReceiptGetInput:
		out, err := s.ReceiptGet(ctx, in)
		return encodeWithEvents(out, nil, err)
	case protocol.EventsReadInput:
		out, err := s.EventsRead(ctx, in)
		return encodeWithEvents(out, nil, err)
	case protocol.EvidenceReadInput:
		out, err := s.EvidenceRead(ctx, in)
		return encodeWithEvents(out, nil, err)
	case protocol.CompactInput:
		out, events, begin, err := s.contextCompact(ctx, in, req.Params)
		res, err := encodeWithEvents(out, events, err)
		if err != nil {
			return res, err
		}
		res.Delivered = begin
		return res, nil
	case protocol.SessionForkInput:
		out, events, err := s.SessionFork(ctx, in, req.Params)
		return encodeWithEvents(out, events, err)
	case protocol.ShutdownInput:
		return s.Shutdown(in)
	}
	return Result{}, protocol.NewError(protocol.CodeInternal, "method has no handler")
}

func encode[T protocol.Message](v T) (Result, error) {
	body, err := protocol.Encode(v)
	if err != nil {
		return Result{}, err
	}
	return Result{Body: body}, nil
}

func encodeWithEvents[T protocol.Message](v T, events []protocol.Event, err error) (Result, error) {
	if err != nil {
		return Result{}, err
	}
	res, err := encode(v)
	res.Events = events
	return res, err
}

// SessionOpen is session/open. params is the exact params text of the request.
func (s *Service) SessionOpen(ctx context.Context, params []byte) (protocol.SessionOpenResult, []protocol.Event, error) {
	if s.shuttingDown() {
		return protocol.SessionOpenResult{}, nil, errShuttingDown()
	}
	out, err := s.store.OpenSession(ctx, sqlite.SessionAdmission{Caller: s.caller, Resolve: s.planSession}, params)
	if err != nil {
		return protocol.SessionOpenResult{}, nil, err
	}
	return out.Result, out.Events, nil
}

// planSession resolves a session/open request against the host configuration. The
// binding must be one the host configured, the workspace must resolve to a
// configured root, and the policy and mode must be allowed for it. Every refusal is
// the same generic FORBIDDEN, so a client cannot probe the host's layout; an
// isolated mode the host allows but this build cannot provide is
// UNSUPPORTED_CONTRACT.
func (s *Service) planSession(in protocol.SessionOpenInput) (sqlite.SessionPlan, error) {
	notAllowed := func() error {
		return protocol.NewError(protocol.CodeForbidden, "the binding, workspace, policy or mode is not allowed by the host configuration")
	}
	if !s.bindingConfigured(in.Binding) {
		return sqlite.SessionPlan{}, notAllowed()
	}
	root, ok := realWorkspacePath(in.WorkspacePath)
	if !ok {
		return sqlite.SessionPlan{}, notAllowed()
	}
	mode, err := s.dep.ResolveMode(root, in.PolicyRef, in.ExecutionMode)
	switch {
	case errors.Is(err, config.ErrModeUnavailable):
		return sqlite.SessionPlan{}, protocol.NewError(protocol.CodeUnsupportedContract, "this execution mode is not available in this build")
	case errors.Is(err, config.ErrInvalid), errors.Is(err, config.ErrUnregisteredPolicy):
		return sqlite.SessionPlan{}, notAllowed()
	case err != nil:
		return sqlite.SessionPlan{}, protocol.NewError(protocol.CodeInternal, "the session could not be planned").Wrap(err)
	}
	rev, err := s.dep.PolicyRevision(in.PolicyRef, root, mode)
	if err != nil {
		return sqlite.SessionPlan{}, protocol.NewError(protocol.CodeInternal, "the policy revision could not be computed").Wrap(err)
	}
	return sqlite.SessionPlan{WorkspacePath: root, PolicyRef: in.PolicyRef, ExecutionMode: mode, PolicyRevision: rev}, nil
}

// realWorkspacePath resolves a requested workspace path to its real path. Only an
// absolute path in clean form is considered.
func realWorkspacePath(p string) (string, bool) {
	if p == "" || strings.ContainsRune(p, 0) || !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return "", false
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", false
	}
	return real, true
}

func (s *Service) bindingConfigured(b protocol.Binding) bool {
	for _, e := range s.dep.Config.Bindings {
		if sameBinding(e.Binding, b) {
			return true
		}
	}
	return false
}

func sameBinding(a, b protocol.Binding) bool {
	return a.Kind == b.Kind && a.Selector == b.Selector && a.ProfileRevision == b.ProfileRevision &&
		sameOptional(a.AgentID, b.AgentID) && sameOptional(a.ExecutionRole, b.ExecutionRole)
}

func sameOptional(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// SessionList is session/list.
func (s *Service) SessionList(ctx context.Context, in protocol.SessionListInput) (protocol.SessionListResult, error) {
	return s.store.ListSessions(ctx, s.caller, in.Cursor, int(in.Limit))
}

// SessionGet is session/get.
func (s *Service) SessionGet(ctx context.Context, in protocol.SessionGetInput) (protocol.SessionInfo, error) {
	return s.store.GetSession(ctx, s.caller, in.ThreadID)
}

// RunGet is run/get.
func (s *Service) RunGet(ctx context.Context, in protocol.RunGetInput) (protocol.RunInfo, error) {
	return s.store.GetRun(ctx, s.caller, in.RunID)
}

// ReceiptGet is receipt/get.
func (s *Service) ReceiptGet(ctx context.Context, in protocol.ReceiptGetInput) (protocol.ReceiptRecord, error) {
	return s.store.GetReceipt(ctx, s.caller, in.ReceiptID)
}

// EvidenceRead is evidence/read. It returns stored bytes; no Tool is run.
func (s *Service) EvidenceRead(ctx context.Context, in protocol.EvidenceReadInput) (protocol.EvidenceReadResult, error) {
	return s.store.ReadEvidence(ctx, s.caller, in)
}

// EventsRead is events/read: committed events of a Thread the caller can read,
// after after_seq. A Thread outside the caller's scope and one that does not exist
// are the same FORBIDDEN, and has_more only ever speaks about this Thread's events.
func (s *Service) EventsRead(ctx context.Context, in protocol.EventsReadInput) (protocol.EventsReadResult, error) {
	if _, err := s.store.ThreadAccess(ctx, s.caller, in.ThreadID); err != nil {
		return protocol.EventsReadResult{}, err
	}
	events, more, err := s.store.EventsAfter(ctx, in.ThreadID, in.AfterSeq, int(in.Limit))
	if err != nil {
		return protocol.EventsReadResult{}, err
	}
	if events == nil {
		events = []protocol.Event{}
	}
	next := in.AfterSeq
	if n := len(events); n > 0 {
		next = events[n-1].EventSeq
	}
	return protocol.EventsReadResult{Events: events, NextAfterSeq: next, HasMore: more}, nil
}

// admission is what the store needs to know about this process for one admission.
func (s *Service) admission(epoch int64) sqlite.Admission {
	return sqlite.Admission{
		Caller: s.caller, Entrypoint: s.entrypoint, DeclaredOrigin: s.declared, WriterEpoch: epoch, Recovery: s.dep.Config.Recovery,
		Caps: s.dep.EffectiveCaps,
	}
}

// TurnStart is turn/start: it admits the Turn, Task and Run durably and returns, and
// then, when the Service has a model port, starts the driver that runs the Run.
//
// The order is the one PROTOCOL gives: a request that was already accepted is
// answered from its receipt first (it needs only read access and no writer role),
// then the caller must be allowed to control the Thread, then this process takes
// the Thread's writer role (BUSY if another process holds it; a newly taken role
// first settles whatever Run an earlier driver left running) and the admission
// itself checks the revisions and the active Run.
//
// A caller that delivers the events itself (the transport) uses Handle, which
// returns the start as Result.Delivered so that it follows the delivery of the
// admission events. TurnStart starts the Run at once.
func (s *Service) TurnStart(ctx context.Context, in protocol.StartInput, params []byte) (protocol.StartResult, []protocol.Event, error) {
	out, events, begin, err := s.turnStart(ctx, in, params)
	if begin != nil {
		begin()
	}
	return out, events, err
}

// TurnStartDeferred is TurnStart for a caller that announces the events of the admission
// itself (the CLI, as the transport does through Result.Delivered): it admits the Run and
// returns the function that starts its driver, which the caller calls once the admission's
// events are announced, so that the events the driver commits never overtake them. begin is
// nil when there is nothing to start (a request answered from its receipt, a Service with
// no model port).
func (s *Service) TurnStartDeferred(ctx context.Context, in protocol.StartInput, params []byte) (protocol.StartResult, []protocol.Event, func(), error) {
	return s.turnStart(ctx, in, params)
}

// RunResumeDeferred is RunResume with the start of the Run left to the caller (see
// TurnStartDeferred).
func (s *Service) RunResumeDeferred(ctx context.Context, params []byte) (protocol.ResumeResult, []protocol.Event, func(), error) {
	return s.runResume(ctx, params)
}

// Driving reports whether a driver of this Service holds the Run: a Run that is not over
// and is not driven here is waiting for another process, or was left.
func (s *Service) Driving(runID string) bool { return s.isDriving(runID) }

// turnStart is TurnStart without the start of the Run: begin, when not nil, starts
// the driver of the admitted Run, exactly once.
func (s *Service) turnStart(ctx context.Context, in protocol.StartInput, params []byte) (protocol.StartResult, []protocol.Event, func(), error) {
	if s.shuttingDown() {
		return protocol.StartResult{}, nil, nil, errShuttingDown()
	}
	if prior, found, err := s.store.LookupStart(ctx, s.admission(0), params); err != nil {
		return protocol.StartResult{}, nil, nil, err
	} else if found {
		return prior.Result, nil, nil, nil
	}
	access, err := s.store.ThreadAccess(ctx, s.caller, in.ThreadID)
	if err != nil {
		return protocol.StartResult{}, nil, nil, err
	}
	if !access.CanControl {
		return protocol.StartResult{}, nil, nil, protocol.NewError(protocol.CodeForbidden, "the thread is not accessible to this caller")
	}
	if s.writers == nil {
		return protocol.StartResult{}, nil, nil, protocol.NewError(protocol.CodeUnsupportedContract, "this service has no writer role and cannot admit work")
	}
	// What the host adds to the Run is read before the Thread's writer role is taken, and a
	// request that cannot be admitted for it (an asset over its limit) takes nothing.
	assets, err := s.hostAssets(ctx, in.ThreadID)
	if err != nil {
		return protocol.StartResult{}, nil, nil, err
	}
	lease, settled, err := s.takeThread(ctx, in.ThreadID)
	if err != nil {
		return protocol.StartResult{}, nil, nil, err
	}
	adm := s.admission(lease.Epoch)
	adm.HostAssets = assets
	out, err := s.store.AdmitStart(ctx, adm, params)
	if err != nil {
		s.publishEvents(settled) // committed, though the admission was refused
		return protocol.StartResult{}, nil, nil, err
	}
	events := append(settled, out.Events...)
	if s.driver == nil {
		return out.Result, events, nil, nil
	}
	return out.Result, events, s.driverStarter(kernel.Handle{RunID: out.Result.RunID, ThreadID: in.ThreadID, Epoch: lease.Epoch}), nil
}

// driverStarter returns the function that starts the driver of an admitted Run, exactly
// once. Calling it after the Service began to let go of its Runs starts nothing: the Run
// stays as it was admitted and the next driver of the Thread settles it.
func (s *Service) driverStarter(h kernel.Handle) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.closing {
				s.mu.Unlock()
				return
			}
			s.runs.Add(1)
			s.driving[h.RunID] = struct{}{}
			s.mu.Unlock()
			go func() {
				defer func() {
					// One Run's driver failing must not take the process, and every other
					// Run on it, down: the Run is left as it was and is ended by its
					// deadline or by the next driver of the Thread.
					if recover() != nil {
						s.diag("a run driver failed; its run is left to be settled")
					}
					s.mu.Lock()
					delete(s.driving, h.RunID)
					s.mu.Unlock()
					s.runs.Done()
				}()
				s.driver.Drive(s.runCtx, h)
			}()
		})
	}
}

// Shutdown is service/shutdown. The first accepted request fixes the state and
// every later one returns it unchanged. New work is refused from then on; reads
// continue until the transport stops.
func (s *Service) Shutdown(in protocol.ShutdownInput) (Result, error) {
	s.mu.Lock()
	if s.shutdown == nil {
		s.shutdown = &ShutdownRequest{Mode: in.Mode, Deadline: time.Duration(in.DeadlineSeconds) * time.Second}
	}
	accepted := *s.shutdown
	s.mu.Unlock()
	res, err := encode(protocol.ShutdownResult{Accepted: true})
	if err != nil {
		return Result{}, err
	}
	res.Shutdown = &accepted
	return res, nil
}

// TurnInterrupt is turn/interrupt: it records the stop signal of a Run (see
// sqlite.RecordInterrupt) and nothing more. The receipt says the signal is recorded, not
// that anything has stopped. Afterwards, best effort and never changing the answer:
//
//   - the driver of the Run, when this process drives it, is woken so that the work it is
//     in the middle of (a count, a generation, a process) is told to stop now;
//   - a Run that nobody here drives is ended now, as the cancelled Run it is (or with the
//     unknown outcome it left), by taking the Thread's writer role for that: another
//     process that holds it drives the Run and finds the signal itself.
//
// It is accepted while the Service is shutting down: it only ever stops work.
func (s *Service) TurnInterrupt(ctx context.Context, params []byte) (protocol.InterruptReceipt, []protocol.Event, error) {
	out, err := s.store.RecordInterrupt(ctx, s.caller, params)
	if err != nil {
		return protocol.InterruptReceipt{}, nil, err
	}
	events := out.Events
	if out.Result.Code == protocol.InterruptCancelRequested {
		events = append(events, s.afterStop(ctx, out.ThreadID, out.Result.RunID)...)
	}
	return out.Result, events, nil
}

// afterStop acts on a stop that was recorded for a Run, without deciding anything about
// it (see TurnInterrupt). It returns the events it committed.
func (s *Service) afterStop(ctx context.Context, threadID, runID string) []protocol.Event {
	if s.driver != nil && s.driver.Interrupt(runID) {
		return nil
	}
	if s.writers == nil || s.isDriving(runID) {
		return nil
	}
	lease, err := s.writers.Acquire(ctx, threadID)
	if err != nil {
		return nil // another process drives the Thread (BUSY), or it cannot be taken now
	}
	settled := lease.TakeRecovered()
	cancelled, err := kernel.RecoverCancelled(ctx, s.store, threadID, lease.Epoch, s.isDriving, s.reconciler)
	if err != nil {
		s.diag("a run whose stop was requested could not be ended")
	}
	return append(settled, cancelled...)
}

// InputAppend is input/append: it stores one more input of a Run that is still going,
// with its classification (see sqlite.AppendInput). For interrupt_current the stop it
// records is acted on as turn/interrupt's is. New work is refused while the Service is
// shutting down.
func (s *Service) InputAppend(ctx context.Context, in protocol.InputAppendInput, params []byte) (protocol.InputReceipt, []protocol.Event, error) {
	if s.shuttingDown() {
		return protocol.InputReceipt{}, nil, errShuttingDown()
	}
	out, err := s.store.AppendInput(ctx, s.admission(0), params)
	if err != nil {
		return protocol.InputReceipt{}, nil, err
	}
	events := out.Events
	if out.StopRequested {
		events = append(events, s.afterStop(ctx, in.ThreadID, in.RunID)...)
	}
	return out.Result, events, nil
}

// RunResume is run/resume: a new Run of an ended Task (see sqlite.AdmitResume), started
// on the model port as turn/start's Run is.
func (s *Service) RunResume(ctx context.Context, params []byte) (protocol.ResumeResult, []protocol.Event, error) {
	out, events, begin, err := s.runResume(ctx, params)
	if begin != nil {
		begin()
	}
	return out, events, err
}

// runResume is RunResume without the start of the Run. The order is that of turn/start:
// a request already accepted is answered from its receipt first; then the caller must be
// allowed to control the Thread of the Task; then this process takes the Thread's writer
// role (BUSY if another process holds it; a newly taken role first settles whatever Run
// an earlier driver left running, which is what makes the Task's last Run an ended one);
// then the Task is checked as it now is, the stored context and the checkpoint it stands on
// are read back against their originals (INTEGRITY_BLOCKED, with nothing else done), the host
// is asked about the processes of the Task's unknown Tool calls (they are never run again; one
// that is still running and cannot be stopped is BUSY, and nothing is written), and only then
// does the admission's transaction decide, fencing the checkpoint that was verified. A Task
// with an unknown generation or fixed verification outcome gets its new Run ended at once,
// blocked, and never driven: the model is not asked again and the verifier is never resent.
func (s *Service) runResume(ctx context.Context, params []byte) (protocol.ResumeResult, []protocol.Event, func(), error) {
	if s.shuttingDown() {
		return protocol.ResumeResult{}, nil, nil, errShuttingDown()
	}
	adm := s.admission(0)
	if prior, found, err := s.store.LookupResume(ctx, adm, params); err != nil {
		return protocol.ResumeResult{}, nil, nil, err
	} else if found {
		return prior.Result, nil, nil, nil
	}
	in, err := protocol.Decode[protocol.ResumeInput](params)
	if err != nil {
		return protocol.ResumeResult{}, nil, nil, err
	}
	access, err := s.store.TaskAccess(ctx, s.caller, in.TaskID)
	if err != nil {
		return protocol.ResumeResult{}, nil, nil, err
	}
	if !access.CanControl {
		return protocol.ResumeResult{}, nil, nil, protocol.NewError(protocol.CodeForbidden, "the thread is not accessible to this caller")
	}
	if s.writers == nil {
		return protocol.ResumeResult{}, nil, nil, protocol.NewError(protocol.CodeUnsupportedContract, "this service has no writer role and cannot admit work")
	}
	lease, settled, err := s.takeThread(ctx, access.ThreadID)
	if err != nil {
		return protocol.ResumeResult{}, nil, nil, err
	}
	fail := func(err error) (protocol.ResumeResult, []protocol.Event, func(), error) {
		s.publishEvents(settled) // committed, though the resume was refused
		return protocol.ResumeResult{}, nil, nil, err
	}

	resumeAdm := sqlite.ResumeAdmission{Admission: adm, BindingAllowed: s.bindingConfigured}
	plan, err := s.store.PreflightResume(ctx, resumeAdm, params)
	if err != nil {
		return fail(err)
	}
	// The cold look comes before the host is asked anything (STORAGE section 7: the originals and
	// the checkpoint are verified, then the earlier unknown calls are asked about): everything
	// the Thread stands on is read back from the store as it is, and anything that does not read
	// back exactly refuses the resume (INTEGRITY_BLOCKED) with nothing else done. No older
	// checkpoint is tried in the place of one that fails, and none is taken for no checkpoint.
	standing, err := kernel.VerifyStanding(ctx, s.store, plan.ThreadID, plan.LastRunID)
	if err != nil {
		return fail(err)
	}
	var verdicts map[string]string
	if len(plan.Unknown) > 0 {
		verdicts = s.reconciler.Reconcile(ctx, plan.Unknown)
		for _, v := range verdicts {
			if v == process.VerdictStopFailed {
				return fail(protocol.NewError(protocol.CodeBusy, "a process of an earlier call is still running and could not be stopped").AsRetryable())
			}
		}
	}
	out, err := s.store.AdmitResume(ctx, sqlite.ResumeAdmission{Admission: s.admission(lease.Epoch), BindingAllowed: s.bindingConfigured, Reconciled: verdicts,
		EndsOnUnknownGeneration: kernel.SettleUnknownGeneration, Standing: &sqlite.Standing{CheckpointID: standing}}, params)
	if err != nil {
		return fail(err)
	}
	events := append(settled, out.Events...)
	if s.driver == nil || out.Replayed || out.Blocked {
		return out.Result, events, nil, nil
	}
	return out.Result, events, s.driverStarter(kernel.Handle{RunID: out.Result.RunID, ThreadID: access.ThreadID, Epoch: lease.Epoch}), nil
}

// takeThread takes this process's writer role of the Thread and settles what an earlier driver
// left on it, in the order an admission needs: the Runs of earlier writer epochs (taking the
// role does that), then the Runs of this process that are past their deadline and that nobody
// drives (so a Thread is busy no longer than a Run's own deadline), then the Runs whose stop was
// recorded and that nobody drives (the process that was to act on the stop could not take
// the Thread then, and the Run is ended now, not at its deadline). It returns what it settled,
// for the caller to announce once its own operation is known; when it fails it has announced
// them itself.
func (s *Service) takeThread(ctx context.Context, threadID string) (*session.Lease, []protocol.Event, error) {
	lease, err := s.writers.Acquire(ctx, threadID)
	if err != nil {
		return nil, nil, err
	}
	settled := lease.TakeRecovered()
	expired, err := kernel.RecoverExpired(ctx, s.store, threadID, lease.Epoch, s.isDriving, s.reconciler)
	if err != nil {
		s.publishEvents(settled)
		return nil, nil, err
	}
	settled = append(settled, expired...)
	cancelled, err := kernel.RecoverCancelled(ctx, s.store, threadID, lease.Epoch, s.isDriving, s.reconciler)
	if err != nil {
		s.publishEvents(settled)
		return nil, nil, err
	}
	return lease, append(settled, cancelled...), nil
}
