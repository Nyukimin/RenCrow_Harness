package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The lifecycle tests run the real process management against a stand-in Harness: this
// test binary itself, started as `serve --stdio --config PATH` with fakeEnv in its
// environment, which makes TestMain run fakeHarness instead of the tests.
const fakeEnv = "RENCROW_CLIENT_FAKE_HARNESS"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeEnv); mode != "" {
		os.Exit(fakeHarness(mode))
	}
	code := m.Run()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// fakeHarness is the stand-in. mode picks how it misbehaves; "ok" is a Harness that
// answers initialize and service/shutdown and ends when its input ends.
func fakeHarness(mode string) int {
	out := bufio.NewWriter(os.Stdout)
	send := func(b []byte) {
		_, _ = out.Write(b)
		_ = out.WriteByte('\n')
		_ = out.Flush()
	}
	reply := func(id string, body []byte) { send(idFrame(id, "result", body)) }
	enc := func(v []byte, err error) []byte {
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake: cannot encode")
			os.Exit(91)
		}
		return v
	}
	if mode == "exit-before-init" {
		return 3
	}
	if mode == "sleeper" {
		time.Sleep(8 * time.Second) // holds the standard output it inherited, then ends by itself
		return 0
	}
	if mode == "stderr" {
		fmt.Fprintln(os.Stderr, "fake: serving")
	}
	hangOnEOF := mode == "hang" || mode == "garbage" || mode == "silent"
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			if hangOnEOF {
				time.Sleep(time.Hour) // only a kill ends this one
			}
			return 0 // the input closed: the same ending as service/shutdown
		}
		req, err := protocol.DecodeRequest(bytes.TrimRight(line, "\n"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake: the client sent an invalid request")
			return 90
		}
		if mode == "silent" {
			continue
		}
		switch req.Method {
		case "initialize":
			if mode == "badversion" {
				// Not encodable through protocol.Encode, which refuses another version.
				raw, err := json.Marshal(fakeCapabilities(mode))
				reply(req.ID, enc(raw, err))
			} else {
				reply(req.ID, enc(protocol.Encode(fakeCapabilities(mode))))
			}
			switch mode {
			case "events":
				evs := designEventsNoT()
				for i := 0; i < 3; i++ {
					send(notification("event/recorded", enc(protocol.Encode(evs[i]))))
					if i == 0 {
						send(notification("progress/delta", enc(protocol.Encode(protocol.ProgressDelta{RunID: testRun, AttemptID: testAttempt, Ordinal: 0, Text: "t", Provisional: true}))))
					}
				}
			case "garbage":
				send([]byte("this is not json"))
			}
		case "service/shutdown":
			reply(req.ID, enc(protocol.Encode(protocol.ShutdownResult{Accepted: true})))
			switch mode {
			case "hang-after-shutdown":
				time.Sleep(time.Hour)
			case "slow-shutdown":
				time.Sleep(300 * time.Millisecond)
				return 0
			case "grandchild":
				// Leave a descendant that keeps the standard output open, and exit.
				exe, err := os.Executable()
				if err != nil {
					return 93
				}
				gc := exec.Command(exe)
				gc.Env = []string{fakeEnv + "=sleeper"}
				gc.Stdout = os.Stdout
				if err := gc.Start(); err != nil {
					return 94
				}
				return 0
			case "shutdown-fail":
				return 1
			}
			return 0
		default:
			if mode == "die-after-init" {
				return 7
			}
			// every other request is left unanswered
		}
	}
}

func notification(method string, params []byte) []byte {
	return []byte(`{"jsonrpc":"2.0","method":"` + method + `","params":` + string(params) + `}`)
}

// designEventsNoT reads the design events outside a test (the stand-in has no *testing.T).
func designEventsNoT() []protocol.Event {
	b, err := os.ReadFile(filepath.Join(os.Getenv("RENCROW_CLIENT_FAKE_ROOT"), "testdata", "contract", "examples", "wire", "event_payloads.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake: no events")
		os.Exit(92)
	}
	var f struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		os.Exit(92)
	}
	var out []protocol.Event
	for _, raw := range f.Events {
		ev, err := protocol.Decode[protocol.Event](raw)
		if err != nil {
			os.Exit(92)
		}
		out = append(out, ev)
	}
	return out
}

func fakeCapabilities(mode string) protocol.CapabilitiesResult {
	version := protocol.ProtocolVersion
	if mode == "badversion" {
		version = "rencrow-harness/v2"
	}
	var caps []protocol.Capability
	for _, name := range DefaultRequiredCapabilities() {
		status := "ready"
		if mode == "nocaps" && name == "turn.execution" {
			status = "unavailable"
		}
		caps = append(caps, protocol.Capability{Name: name, Status: status, Basis: "declared"})
	}
	// The probe tells the test how this process was started.
	wd, _ := os.Getwd()
	probe, _ := json.Marshal(map[string]any{"args": os.Args[1:], "env": os.Environ(), "cwd": wd})
	caps = append(caps, protocol.Capability{Name: "probe", Status: "ready", Basis: "declared", Reason: protocol.Str(string(probe))})
	return protocol.CapabilitiesResult{ProtocolVersion: version, BuildRevision: "fake", Capabilities: caps}
}

// ---- helpers --------------------------------------------------------------

func moduleRoot(t testing.TB) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func fakeConfig(t *testing.T, mode string) Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return Config{
		Binary: exe, ConfigPath: filepath.Join(dir, "config.json"), Dir: dir,
		Env:        []string{fakeEnv + "=" + mode, "RENCROW_CLIENT_FAKE_ROOT=" + moduleRoot(t)},
		ClientName: "client-test", ClientVersion: "1", ExitGrace: 200 * time.Millisecond,
	}
}

// startFake starts a Client on the stand-in; it is aborted when the test ends.
func startFake(t *testing.T, mode string, edit ...func(*Config)) (*Client, error) {
	t.Helper()
	cfg := fakeConfig(t, mode)
	for _, e := range edit {
		e(&cfg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	c, err := Start(ctx, cfg)
	if c != nil {
		t.Cleanup(c.Abort)
	}
	return c, err
}

func mustStartFake(t *testing.T, mode string, edit ...func(*Config)) *Client {
	t.Helper()
	c, err := startFake(t, mode, edit...)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return c
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func shutdownIn(seconds int64) protocol.ShutdownInput {
	return protocol.ShutdownInput{Mode: "drain", DeadlineSeconds: seconds}
}

func isDone(c *Client) bool {
	select {
	case <-c.Done():
		return true
	default:
		return false
	}
}

// ---- Start ----------------------------------------------------------------

func TestStartGivesTheChildExactlyWhatTheConfigSaysAndNothingElse(t *testing.T) {
	t.Setenv("RENCROW_CLIENT_LEAK", "must-not-reach-the-child")
	c := mustStartFake(t, "ok", func(cfg *Config) { cfg.Env = append(cfg.Env, "KEEP=1") })
	var probe struct {
		Args []string `json:"args"`
		Env  []string `json:"env"`
		Cwd  string   `json:"cwd"`
	}
	var found bool
	for _, cp := range c.Capabilities().Capabilities {
		if cp.Name == "probe" {
			found = true
			if err := json.Unmarshal([]byte(*cp.Reason), &probe); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found {
		t.Fatal("no probe")
	}
	if len(probe.Args) != 4 || probe.Args[0] != "serve" || probe.Args[1] != "--stdio" || probe.Args[2] != "--config" || !filepath.IsAbs(probe.Args[3]) {
		t.Fatalf("args %v", probe.Args)
	}
	for _, e := range probe.Env {
		if strings.HasPrefix(e, "RENCROW_CLIENT_LEAK=") || strings.HasPrefix(e, "HOME=") || strings.HasPrefix(e, "PATH=") {
			t.Fatalf("the child inherited %q", strings.SplitN(e, "=", 2)[0])
		}
	}
	if !slices.Contains(probe.Env, "KEEP=1") || !slices.Contains(probe.Env, fakeEnv+"=ok") {
		t.Fatalf("the child lacks what the Config gave it: %v", probe.Env)
	}
	if runtime.GOOS != "windows" && len(probe.Env) != 3 {
		t.Fatalf("the child's environment is not exactly the Config's: %v", probe.Env)
	}
	// The working directory is the Config's, not the test's.
	want, _ := filepath.EvalSymlinks(strings.TrimSuffix(probe.Args[3], string(filepath.Separator)+"config.json"))
	got, _ := filepath.EvalSymlinks(probe.Cwd)
	if got != want {
		t.Fatalf("cwd %q, want %q", got, want)
	}
	if err := c.Shutdown(context.Background(), shutdownIn(1)); err != nil {
		t.Fatal(err)
	}
}

func TestStartRefusesAConfigThatIsNotExplicit(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*Config)
	}{
		{"relative binary", func(c *Config) { c.Binary = "rencrow-harness" }},
		{"no binary", func(c *Config) { c.Binary = "" }},
		{"relative config", func(c *Config) { c.ConfigPath = "config.json" }},
		{"no working directory", func(c *Config) { c.Dir = "" }},
		{"relative working directory", func(c *Config) { c.Dir = "." }},
		{"no client name", func(c *Config) { c.ClientName = "" }},
		{"client name over 80", func(c *Config) { c.ClientName = strings.Repeat("n", 81) }},
		{"no client version", func(c *Config) { c.ClientVersion = "" }},
		{"env without =", func(c *Config) { c.Env = []string{"NOVALUE"} }},
		{"env without a name", func(c *Config) { c.Env = []string{"=x"} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := fakeConfig(t, "ok")
			c.edit(&cfg)
			got, err := Start(context.Background(), cfg)
			if !errors.Is(err, ErrInvalidConfig) || got != nil {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestStartFailsWhenTheBinaryCannotBeStarted(t *testing.T) {
	cfg := fakeConfig(t, "ok")
	cfg.Binary = filepath.Join(cfg.Dir, "no-such-binary")
	c, err := Start(context.Background(), cfg)
	if err == nil || c != nil || errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), cfg.Dir) || strings.Contains(err.Error(), "no-such-binary") {
		t.Fatalf("the message repeats a path: %q", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the cause is not reachable: %v", err)
	}
}

func TestStartFailsClosedOnAHarnessItDoesNotKnow(t *testing.T) {
	for _, c := range []struct {
		mode string
		want error
	}{
		{"badversion", ErrIncompatible}, // another protocol version
		{"nocaps", ErrIncompatible},     // a required capability is not ready
		{"exit-before-init", ErrClosed}, // dies before it answers
	} {
		t.Run(c.mode, func(t *testing.T) {
			cl, err := startFake(t, c.mode)
			if !errors.Is(err, c.want) || cl != nil {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestStartRequiresNothingWhenTheConfigSaysSo(t *testing.T) {
	c := mustStartFake(t, "nocaps", func(cfg *Config) { cfg.RequireCapabilities = []string{} })
	if err := c.Shutdown(context.Background(), shutdownIn(1)); err != nil {
		t.Fatal(err)
	}
	// A named capability that is missing fails it.
	if _, err := startFake(t, "ok", func(cfg *Config) { cfg.RequireCapabilities = []string{"no.such.capability"} }); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("%v", err)
	}
}

func TestCheckCapabilities(t *testing.T) {
	good := capabilities("x")
	good.Capabilities = append(good.Capabilities, protocol.Capability{Name: "a", Status: "ready", Basis: "declared"}, protocol.Capability{Name: "b", Status: "unavailable", Basis: "declared"})
	if err := checkCapabilities(good, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := checkCapabilities(good, []string{"a", "b"}); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("an unavailable capability passed: %v", err)
	}
	if err := checkCapabilities(good, []string{"c"}); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("a missing capability passed: %v", err)
	}
	other := good
	other.ProtocolVersion = "rencrow-harness/v2"
	if err := checkCapabilities(other, nil); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("another protocol version passed: %v", err)
	}
}

func TestStartEndsWhenItsContextEndsAndLeavesNoChild(t *testing.T) {
	cfg := fakeConfig(t, "silent")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	c, err := Start(ctx, cfg)
	if !errors.Is(err, context.DeadlineExceeded) || c != nil {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("Start did not return when its context ended")
	}
}

func TestTheChildsDiagnosticsGoToTheConfiguredWriterOnly(t *testing.T) {
	var buf syncBuffer
	c := mustStartFake(t, "stderr", func(cfg *Config) { cfg.Stderr = &buf })
	if err := c.Shutdown(context.Background(), shutdownIn(1)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "fake: serving") {
		t.Fatalf("stderr: %q", buf.String())
	}
	// With no writer the diagnostics are discarded, and nothing breaks.
	c2 := mustStartFake(t, "stderr")
	if err := c2.Shutdown(context.Background(), shutdownIn(1)); err != nil {
		t.Fatal(err)
	}
}

// ---- an end nobody asked for ------------------------------------------------

func TestAChildThatExitsOnItsOwnEndsTheConnectionWithItsExit(t *testing.T) {
	c := mustStartFake(t, "die-after-init")
	_, err := c.RunGet(context.Background(), protocol.RunGetInput{RunID: testRun})
	if !errors.Is(err, ErrClosed) || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("the call that was waiting: %v", err)
	}
	select {
	case <-c.Done():
	case <-time.After(wait):
		t.Fatal("Done was not closed")
	}
	var ee *exec.ExitError
	if err := c.Err(); !errors.Is(err, ErrProcessExited) || !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("%v", err)
	}
	if _, ok := <-c.Notifications(); ok {
		t.Fatal("Notifications is not closed")
	}
	if _, err := c.RunGet(context.Background(), protocol.RunGetInput{RunID: testRun}); !errors.Is(err, ErrClosed) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("a call after the end: %v", err)
	}
	// Stopping what is stopped changes nothing and says why it ended.
	c.Abort()
	if err := c.Shutdown(context.Background(), shutdownIn(1)); !errors.Is(err, ErrProcessExited) {
		t.Fatalf("%v", err)
	}
	if err := c.Wait(); !errors.Is(err, ErrProcessExited) {
		t.Fatalf("%v", err)
	}
}

func TestAProtocolViolationEndsTheConnectionAndKillsTheChild(t *testing.T) {
	// A child that would otherwise never exit, and no grace to wait for it: only the kill
	// the connection asks for when it fails can end it.
	c := mustStartFake(t, "garbage", func(cfg *Config) { cfg.ExitGrace = time.Hour })
	select {
	case <-c.Done():
	case <-time.After(wait):
		t.Fatal("the child was not stopped")
	}
	if err := c.Err(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("%v", err)
	}
}

// ---- notifications through the process --------------------------------------

func TestNotificationsArriveInOrderAndAbortDropsWhatIsQueued(t *testing.T) {
	c := mustStartFake(t, "events")
	var got []string
	for len(got) < 4 {
		select {
		case n, ok := <-c.Notifications():
			if !ok {
				t.Fatal("Notifications closed early")
			}
			got = append(got, describe(n))
		case <-time.After(wait):
			t.Fatalf("got %v", got)
		}
	}
	evs := designEvents(t)
	want := []string{"event:" + evs[0].Type, "delta:0", "event:" + evs[1].Type, "event:" + evs[2].Type}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	c.Abort()
	select {
	case _, ok := <-c.Notifications():
		if ok {
			t.Fatal("a notification after Abort")
		}
	case <-time.After(wait):
		t.Fatal("Notifications was not closed by Abort")
	}
}

// ---- the two stages of stopping ---------------------------------------------

func TestAbortIsImmediateIdempotentAndEndsWhatWaits(t *testing.T) {
	c := mustStartFake(t, "hang") // answers initialize, then nothing, and does not end by itself
	pending := make(chan error, 1)
	go func() {
		_, err := c.RunGet(context.Background(), protocol.RunGetInput{RunID: testRun})
		pending <- err
	}()
	time.Sleep(100 * time.Millisecond)

	var wg sync.WaitGroup
	for range 5 { // concurrent, repeated
		wg.Add(1)
		go func() { defer wg.Done(); c.Abort() }()
	}
	wg.Wait()
	if !isDone(c) {
		t.Fatal("Abort returned before the child was gone")
	}
	if err := c.Err(); !errors.Is(err, ErrAborted) {
		t.Fatalf("%v", err)
	}
	if err := recv(t, pending); !errors.Is(err, ErrClosed) || !errors.Is(err, ErrAborted) || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("the waiting call: %v", err)
	}
	c.Abort() // once more
	if err := c.Shutdown(context.Background(), shutdownIn(1)); !errors.Is(err, ErrAborted) {
		t.Fatalf("Shutdown after Abort: %v", err)
	}
}

func TestShutdownIsOrderlyAndIdempotent(t *testing.T) {
	c := mustStartFake(t, "ok")
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 { // concurrent
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.Shutdown(context.Background(), shutdownIn(1))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if !isDone(c) || c.Err() != nil || c.Wait() != nil {
		t.Fatalf("Done=%v Err=%v", isDone(c), c.Err())
	}
	if _, ok := <-c.Notifications(); ok {
		t.Fatal("Notifications is not closed")
	}
	if err := c.Shutdown(context.Background(), shutdownIn(1)); err != nil { // again, after the end
		t.Fatal(err)
	}
	c.Abort() // a no-op after a clean end: it must not turn it into ErrAborted
	if c.Err() != nil {
		t.Fatalf("Abort after a clean Shutdown changed the result: %v", c.Err())
	}
	if _, err := c.RunGet(context.Background(), protocol.RunGetInput{RunID: testRun}); !errors.Is(err, ErrClosed) {
		t.Fatalf("%v", err)
	}
}

func TestCallsAreRefusedOnceShutdownBeganAndItsResultStaysAvailable(t *testing.T) {
	c := mustStartFake(t, "slow-shutdown") // the child lingers 300 ms after it answered
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// The caller stops waiting; the shutdown goes on.
	if err := c.Shutdown(ctx, shutdownIn(1)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	if _, err := c.RunGet(context.Background(), protocol.RunGetInput{RunID: testRun}); !errors.Is(err, ErrStopping) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("a call during shutdown: %v", err)
	}
	if err := c.Shutdown(context.Background(), shutdownIn(1)); err != nil {
		t.Fatalf("the shutdown that went on: %v", err)
	}
	if c.Err() != nil {
		t.Fatalf("%v", c.Err())
	}
}

func TestShutdownKillsAChildThatDoesNotExitInTheTimeItWasGiven(t *testing.T) {
	c := mustStartFake(t, "hang-after-shutdown")
	start := time.Now()
	err := c.Shutdown(context.Background(), shutdownIn(1)) // 2 x 1 s + the 200 ms grace
	if !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d < 2*time.Second || d > 15*time.Second {
		t.Fatalf("the kill came after %v, want about 2.2 s", d)
	}
	if !isDone(c) {
		t.Fatal("the child is not gone")
	}
	if !errors.Is(c.Err(), ErrShutdownTimeout) {
		t.Fatalf("Err says %v, Shutdown said it was killed for running out of time", c.Err())
	}
}

func TestShutdownReportsAChildThatEndsWithAFailure(t *testing.T) {
	c := mustStartFake(t, "shutdown-fail")
	var ee *exec.ExitError
	if err := c.Shutdown(context.Background(), shutdownIn(1)); !errors.Is(err, ErrProcessExited) || !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("%v", err)
	}
}

func TestAbortCutsAShutdownShort(t *testing.T) {
	c := mustStartFake(t, "hang-after-shutdown", func(cfg *Config) { cfg.ExitGrace = time.Hour })
	res := make(chan error, 1)
	go func() { res <- c.Shutdown(context.Background(), shutdownIn(300)) }()
	time.Sleep(300 * time.Millisecond)
	c.Abort()
	select {
	case err := <-res:
		if !errors.Is(err, ErrAborted) {
			t.Fatalf("%v", err)
		}
	case <-time.After(wait):
		t.Fatal("Shutdown did not end when Abort killed the child")
	}
}

func TestShutdownRefusesAnInvalidInputAndChangesNothing(t *testing.T) {
	c := mustStartFake(t, "ok")
	for _, in := range []protocol.ShutdownInput{{Mode: "drain", DeadlineSeconds: 0}, {Mode: "drain", DeadlineSeconds: 301}, {Mode: "soon", DeadlineSeconds: 1}} {
		if err := c.Shutdown(context.Background(), in); protocol.CodeOf(err) != protocol.CodeInvalidParams {
			t.Fatalf("%+v: %v", in, err)
		}
	}
	if isDone(c) {
		t.Fatal("an invalid Shutdown ended the connection")
	}
	if err := c.Shutdown(context.Background(), shutdownIn(1)); err != nil {
		t.Fatal(err)
	}
}

func TestADeadChildLeftOnTheClientIsReapedByTheEndOfItsInput(t *testing.T) {
	// The caller forgets to stop the client but the process that started the child ends:
	// the child's input closes and the "ok" Harness ends by itself. Here the closing is
	// done by abandoning the Client to Abort via its pipe, which is what a dead parent does.
	c := mustStartFake(t, "ok")
	_ = c.proc.stdin.Close()
	select {
	case <-c.Done():
	case <-time.After(wait):
		t.Fatal("the child did not end with its input")
	}
	// It ended, but nobody asked for it: that is reported, not hidden.
	if err := c.Err(); !errors.Is(err, ErrProcessExited) {
		t.Fatalf("%v", err)
	}
}

func TestADescendantHoldingTheOutputOpenDoesNotHoldTheClient(t *testing.T) {
	c := mustStartFake(t, "grandchild")
	start := time.Now()
	if err := c.Shutdown(context.Background(), shutdownIn(1)); err != nil {
		t.Fatalf("%v", err)
	}
	// The child exited at once; its descendant keeps the pipe for 8 seconds. The client
	// cuts the reader off after a short wait and does not wait for the pipe to close.
	if d := time.Since(start); d > 7*time.Second {
		t.Fatalf("Shutdown waited %v for a pipe a descendant held", d)
	}
	if c.Err() != nil || !isDone(c) {
		t.Fatalf("%v", c.Err())
	}
	if _, ok := <-c.Notifications(); ok {
		t.Fatal("Notifications is not closed")
	}
}

func TestAbortReleasesAConsumerThatDidNotReadAfterAnOrderlyEnd(t *testing.T) {
	c := mustStartFake(t, "events") // four notifications are queued and nobody reads them
	if err := c.Shutdown(context.Background(), shutdownIn(1)); err != nil {
		t.Fatal(err)
	}
	c.Abort()
	select {
	case _, ok := <-c.Notifications():
		if ok {
			t.Fatal("a notification after Abort")
		}
	case <-time.After(wait):
		t.Fatal("Notifications was not closed")
	}
	if c.Err() != nil {
		t.Fatalf("Abort changed how a clean end ended: %v", c.Err())
	}
}
