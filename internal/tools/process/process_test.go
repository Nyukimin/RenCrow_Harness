package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolerr"
)

// TestHelperProcess is not a test: it is the child the tests start (the test binary
// itself), so no shell, no system program and no network is needed. It acts only when
// the environment profile the test passes sets the marker.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("RENCROW_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	mode := ""
	if len(args) > 0 {
		mode = args[0]
	}
	switch mode {
	case "echo":
		fmt.Fprint(os.Stdout, "hello out")
		fmt.Fprint(os.Stderr, "hello err")
		os.Exit(0)
	case "exit":
		n, _ := strconv.Atoi(args[1])
		os.Exit(n)
	case "env":
		env := os.Environ()
		sort.Strings(env)
		fmt.Fprint(os.Stdout, strings.Join(env, "\n"))
		os.Exit(0)
	case "args":
		fmt.Fprint(os.Stdout, strings.Join(args[1:], "\x1f"))
		os.Exit(0)
	case "cwd":
		d, _ := os.Getwd()
		fmt.Fprint(os.Stdout, d)
		os.Exit(0)
	case "big":
		n, _ := strconv.Atoi(args[1])
		chunk := bytes.Repeat([]byte("x"), 4096)
		for n > 0 {
			k := min(n, len(chunk))
			if _, err := os.Stdout.Write(chunk[:k]); err != nil {
				os.Exit(9)
			}
			n -= k
		}
		time.Sleep(30 * time.Second) // still running when the limit is reached
		os.Exit(0)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "tree":
		// A grandchild in the same group, whose PID is printed; then wait.
		exe, _ := os.Executable()
		c := exec.Command(exe, "-test.run=TestHelperProcess", "--", "sleep")
		c.Env = os.Environ()
		if err := c.Start(); err != nil {
			os.Exit(8)
		}
		fmt.Fprintf(os.Stdout, "%d\n", c.Process.Pid)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "escape":
		// Leaves its process group (joins the one its parent is in), so a signal to its
		// own group reaches nobody, and waits.
		escapeGroup()
		fmt.Fprint(os.Stdout, "escaped")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "leave":
		// Starts a grandchild, prints its PID and exits at once, leaving it behind.
		exe, _ := os.Executable()
		c := exec.Command(exe, "-test.run=TestHelperProcess", "--", "sleep")
		c.Env = os.Environ()
		if err := c.Start(); err != nil {
			os.Exit(8)
		}
		fmt.Fprintf(os.Stdout, "%d\n", c.Process.Pid)
		os.Exit(0)
	}
	os.Exit(2)
}

func helperExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

var helperEnv = []string{"RENCROW_HELPER=1"}

func helperArgs(mode string, rest ...string) []string {
	return append([]string{"-test.run=TestHelperProcess", "--", mode}, rest...)
}

type buf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *buf) Write(p []byte) (int, error) { b.mu.Lock(); defer b.mu.Unlock(); return b.b.Write(p) }
func (b *buf) String() string              { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

func spec(t *testing.T, mode string, rest ...string) (Spec, *buf, *buf) {
	t.Helper()
	var out, errb buf
	return Spec{
		Executable: helperExe(t), Argv: helperArgs(mode, rest...), Dir: t.TempDir(), Env: helperEnv, Timeout: 20 * time.Second,
		CaptureLimit: 1 << 20, Stdout: &out, Stderr: &errb, Grace: 200 * time.Millisecond,
	}, &out, &errb
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	te, ok := toolerr.As(err)
	if !ok || te.Code != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

func TestResolveFindsExactlyOneProfile(t *testing.T) {
	p := func(name, exe string, shell bool, prefix ...string) config.ProcessProfile {
		return config.ProcessProfile{Name: name, Executable: exe, IsShell: shell, ArgvPrefix: prefix}
	}
	abs := func(s string) string { return strings.ReplaceAll(s, "/", string(os.PathSeparator)) }
	_ = abs
	tool, other := "/opt/tool", "/opt/other"
	allowed := []config.ProcessProfile{
		p("tool-test", tool, false, "test", "./..."), p("tool-any", tool, false), p("sh", "/bin/sh", true, "-c"), p("other-a", other, false, "a"), p("other-a-too", other, false, "a"),
	}
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses unix absolute paths")
	}
	cases := []struct {
		name string
		exe  string
		argv []string
		want string // profile name, or an error code
	}{
		{"the longer prefix and the empty prefix both match", tool, []string{"test", "./...", "-v"}, toolerr.CodePolicyAmbiguous},
		{"only the empty prefix matches", tool, []string{"build"}, "tool-any"},
		{"no argv at all", tool, nil, "tool-any"},
		{"argv shorter than a prefix", other, []string{}, toolerr.CodePolicyRejected},
		{"the prefix must match element for element", tool, []string{"tes"}, "tool-any"},
		{"two identical profiles are ambiguous", other, []string{"a", "x"}, toolerr.CodePolicyAmbiguous},
		{"a shell profile resolves like any other", "/bin/sh", []string{"-c", "echo hi"}, "sh"},
		{"the shell profile needs its prefix", "/bin/sh", []string{"echo hi"}, toolerr.CodePolicyRejected},
		{"an executable no profile names", "/opt/unknown", []string{"a"}, toolerr.CodePolicyRejected},
		{"a relative executable", "tool", nil, toolerr.CodePolicyRejected},
		{"a NUL in the arguments", tool, []string{"a\x00b"}, toolerr.CodePolicyRejected},
		{"a path that only looks like the executable", "/opt/tool/../tool2", nil, toolerr.CodePolicyRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(allowed, tc.exe, tc.argv)
			if strings.HasPrefix(tc.want, "POLICY_") {
				wantCode(t, err, tc.want)
				return
			}
			if err != nil || got.Name != tc.want {
				t.Fatalf("%+v %v, want %s", got, err, tc.want)
			}
		})
	}
	// An empty allowed set refuses everything.
	if _, err := Resolve(nil, tool, nil); err == nil {
		t.Fatal("no profile at all resolved")
	}
	// The Host adds nothing to the argv: the profile it resolves to carries a prefix
	// as a condition, and the argv the caller passes on is the one it was given.
	argv := []string{"test", "./..."}
	prof, err := Resolve([]config.ProcessProfile{p("t", tool, false, "test")}, tool, argv)
	if err != nil || len(argv) != 2 || prof.ArgvPrefix[0] != "test" {
		t.Fatalf("%+v %v %v", prof, err, argv)
	}
}

func TestBuildEnvIsExactlyTheProfile(t *testing.T) {
	env, err := BuildEnv(config.EnvProfile{Name: "clean", Values: map[string]string{}})
	if err != nil || env == nil || len(env) != 0 {
		t.Fatalf("the clean profile is an empty, non-nil environment (nil would inherit the host's): %#v %v", env, err)
	}
	env, err = BuildEnv(config.EnvProfile{Name: "p", Values: map[string]string{"B": "2", "A": "1", "EMPTY": ""}})
	if err != nil || strings.Join(env, ";") != "A=1;B=2;EMPTY=" {
		t.Fatalf("%v %v", env, err)
	}
	for _, bad := range []map[string]string{{"": "x"}, {"A=B": "x"}, {"A\x00": "x"}, {"A": "x\x00y"}} {
		if _, err := BuildEnv(config.EnvProfile{Name: "bad", Values: bad}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestRunCapturesBothStreamsAndTheExitCode(t *testing.T) {
	s, out, errb := spec(t, "echo")
	res, err := Run(context.Background(), s)
	if err != nil || !res.Started || res.ExitCode == nil || *res.ExitCode != 0 || res.TimedOut || res.CaptureLimited || !res.CaptureComplete() {
		t.Fatalf("%+v %v", res, err)
	}
	if out.String() != "hello out" || errb.String() != "hello err" || res.StdoutBytes != 9 || res.StderrBytes != 9 {
		t.Fatalf("%q %q %+v", out.String(), errb.String(), res)
	}
	s, _, _ = spec(t, "exit", "3")
	res, err = Run(context.Background(), s)
	if err != nil || res.ExitCode == nil || *res.ExitCode != 3 || res.Signaled {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestRunGivesTheChildOnlyTheProfileEnvironmentAndNeverAShell(t *testing.T) {
	t.Setenv("RENCROW_SECRET_CANARY", "gateway-token-value")
	t.Setenv("PATH", os.Getenv("PATH"))
	s, out, _ := spec(t, "env")
	res, err := Run(context.Background(), s)
	if err != nil || res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	got := out.String()
	if strings.Contains(got, "gateway-token-value") || strings.Contains(got, "CANARY") || strings.Contains(got, "PATH=") || strings.Contains(got, "HOME=") {
		t.Fatalf("the host environment reached the child:\n%s", got)
	}
	if !strings.Contains(got, "RENCROW_HELPER=1") {
		t.Fatalf("the profile's value is missing:\n%s", got)
	}
	if runtime.GOOS != "windows" {
		for _, line := range strings.Split(got, "\n") {
			if !strings.HasPrefix(line, "RENCROW_HELPER=") && !strings.HasPrefix(line, "PWD=") && !strings.HasPrefix(line, "GOCOVERDIR=") && !strings.HasPrefix(line, "LLVM_PROFILE_FILE=") {
				t.Errorf("an unexpected variable in the child: %q", line)
			}
		}
	}

	// Arguments are passed as they are: shell syntax in them is just text.
	s, out, _ = spec(t, "args", "a b", "$(echo injected)", "x;y", "`id`", "*", "")
	if _, err := Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if want := strings.Join([]string{"a b", "$(echo injected)", "x;y", "`id`", "*", ""}, "\x1f"); out.String() != want {
		t.Fatalf("%q", out.String())
	}
	// The working directory is the one given.
	s, out, _ = spec(t, "cwd")
	if _, err := Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if real, _ := filepath.EvalSymlinks(s.Dir); out.String() != s.Dir && out.String() != real {
		t.Fatalf("cwd %q, want %q", out.String(), s.Dir)
	}
}

func TestRunRefusesWhatIsNotAnExplicitRequest(t *testing.T) {
	s, _, _ := spec(t, "echo")
	s.Env = nil
	_, err := Run(context.Background(), s)
	wantCode(t, err, toolerr.CodeEnvUnknown)
	s, _, _ = spec(t, "echo")
	s.Timeout = 0
	if _, err := Run(context.Background(), s); err == nil {
		t.Fatal("a run without a timeout started")
	}
	s, _, _ = spec(t, "echo")
	s.Executable = s.Executable + "-does-not-exist"
	res, err := Run(context.Background(), s)
	wantCode(t, err, toolerr.CodeStartFailed)
	if res.Started {
		t.Fatal("a process that could not start is reported as started")
	}
	if te, _ := toolerr.As(err); te.Class != toolerr.Failed {
		t.Fatalf("a start failure changed nothing and must say failed: %d", te.Class)
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, _, _ = spec(t, "echo")
	res, err = Run(cctx, s)
	wantCode(t, err, toolerr.CodeCancelled)
	if res.Started {
		t.Fatal("a cancelled run started a process")
	}
}

func TestRunTimeoutStopsTheWholeTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the PID liveness probe is unix-only")
	}
	s, out, _ := spec(t, "tree")
	s.Timeout = 700 * time.Millisecond
	start := time.Now()
	res, err := Run(context.Background(), s)
	if err != nil || !res.TimedOut || !res.Started || res.ExitCode != nil || !res.Signaled {
		t.Fatalf("%+v %v", res, err)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatalf("the timeout took %v", time.Since(start))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatalf("the grandchild's PID was not printed: %q", out.String())
	}
	waitDead(t, pid)
}

// TestRunCancellationStopsTheWholeTreeAndShowsItGone is the stop of an interrupted Run
// (A12): the context ends while the command and a grandchild of it are running; the
// whole tree is stopped, the result says the run was cancelled and not that it timed out,
// and the grandchild is dead, which Run itself confirmed before it returned (an effect
// is only called cancelled when the tree is shown to be gone).
func TestRunCancellationStopsTheWholeTreeAndShowsItGone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the PID liveness probe is unix-only")
	}
	s, out, _ := spec(t, "tree")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.OnStart = func(Identity) error {
		go func() {
			// Cancel once the helper has started its grandchild and printed its PID.
			for start := time.Now(); time.Since(start) < 10*time.Second; time.Sleep(10 * time.Millisecond) {
				if strings.Contains(out.String(), "\n") {
					cancel()
					return
				}
			}
			cancel()
		}()
		return nil
	}
	begin := time.Now()
	res, err := Run(ctx, s)
	if err != nil || !res.Cancelled || !res.Started || res.TimedOut || res.ExitCode != nil || !res.Signaled {
		t.Fatalf("%+v %v", res, err)
	}
	if time.Since(begin) > 10*time.Second {
		t.Fatalf("the cancellation took %v", time.Since(begin))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatalf("the grandchild's PID was not printed: %q", out.String())
	}
	// No waiting: Run returned only after the tree was shown to be gone.
	if alive(pid) {
		t.Fatalf("the grandchild %d is still alive when Run returned", pid)
	}
}

// TestATreeThatCannotBeShownGoneIsAnUncertainStopNotACancellation: the Harness stopped the
// process (here by cancelling it) and the tree could not be shown to be empty. The effect
// is not "cancelled": Run says so as an uncertain failure, which the Tool runtime records
// as an unknown effect. A process that exited by itself is not asked.
func TestATreeThatCannotBeShownGoneIsAnUncertainStopNotACancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stage is unix-shaped")
	}
	old := shownGone
	t.Cleanup(func() { shownGone = old })
	asked := 0
	shownGone = func(*tree, time.Duration) bool { asked++; return false }

	s, _, _ := spec(t, "sleep")
	ctx, cancel := context.WithCancel(context.Background())
	s.OnStart = func(Identity) error { cancel(); return nil }
	res, err := Run(ctx, s)
	te, ok := toolerr.As(err)
	if !ok || te.Class != toolerr.Unknown || te.Code != toolerr.CodeIO || !res.Started || !res.Cancelled || asked != 1 {
		t.Fatalf("%+v %v asked=%d", res, err, asked)
	}

	// Nothing was stopped by the Harness: the process ended on its own, and the tree is not asked about.
	asked = 0
	s, _, _ = spec(t, "echo")
	res, err = Run(context.Background(), s)
	if err != nil || res.ExitCode == nil || *res.ExitCode != 0 || asked != 0 {
		t.Fatalf("%+v %v asked=%d", res, err, asked)
	}
}

func TestRunStopsWhatTheCommandLeftBehind(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the PID liveness probe is unix-only")
	}
	s, out, _ := spec(t, "leave")
	start := time.Now()
	res, err := Run(context.Background(), s)
	if err != nil || res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatalf("a descendant holding the pipes kept the run for %v", time.Since(start))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatal(out.String())
	}
	waitDead(t, pid)
}

func TestRunStopsAtTheCaptureLimitAndSaysTheCaptureIsIncomplete(t *testing.T) {
	s, out, _ := spec(t, "big", "100000")
	s.CaptureLimit = 10000
	res, err := Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.CaptureLimited || res.CaptureComplete() || !res.Signaled || res.ExitCode != nil {
		t.Fatalf("%+v", res)
	}
	if res.StdoutBytes != 10000 || len(out.String()) != 10000 {
		t.Fatalf("%d bytes kept, %d reported", len(out.String()), res.StdoutBytes)
	}
	// Output exactly at the limit is complete.
	s, out, _ = spec(t, "echo")
	s.CaptureLimit = 18
	res, err = Run(context.Background(), s)
	if err != nil || res.CaptureLimited || !res.CaptureComplete() || out.String() != "hello out" {
		t.Fatalf("%+v %v", res, err)
	}
	// One byte less and it is not.
	s, _, _ = spec(t, "echo")
	s.CaptureLimit = 17
	res, err = Run(context.Background(), s)
	if err != nil || !res.CaptureLimited {
		t.Fatalf("%+v %v", res, err)
	}
	// A limit of zero keeps nothing and still ends the process.
	s, out, _ = spec(t, "big", "10")
	s.CaptureLimit = 0
	res, err = Run(context.Background(), s)
	if err != nil || !res.CaptureLimited || out.String() != "" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestRunCancellationAndAFailedRecordStopTheTree(t *testing.T) {
	s, _, _ := spec(t, "sleep")
	ctx, cancel := context.WithCancel(context.Background())
	s.OnStart = func(Identity) error { cancel(); return nil }
	res, err := Run(ctx, s)
	if err != nil || !res.Cancelled || !res.Started || res.ExitCode != nil {
		t.Fatalf("%+v %v", res, err)
	}

	s, _, _ = spec(t, "sleep")
	var id Identity
	s.OnStart = func(i Identity) error { id = i; return errors.New("the record could not be written") }
	res, err = Run(context.Background(), s)
	wantCode(t, err, toolerr.CodeStartFailed)
	if te, _ := toolerr.As(err); te.Class != toolerr.Unknown || !res.Started {
		t.Fatalf("a process that started and could not be recorded is an unknown effect: %+v %v", res, err)
	}
	if runtime.GOOS != "windows" {
		waitDead(t, id.PID)
	}
}

func TestRunReportsTheIdentityOfTheProcess(t *testing.T) {
	s, _, _ := spec(t, "echo")
	var id Identity
	s.OnStart = func(i Identity) error { id = i; return nil }
	if _, err := Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if id.PID <= 1 || id.Nonce == "" || len(id.Nonce) != 16 {
		t.Fatalf("%+v", id)
	}
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
		if id.Incarnation == "" || id.Start == "" {
			t.Fatalf("this OS must give the incarnation and the start token: %+v", id)
		}
	}
	back, err := DecodeIdentity(id.Encode())
	if err != nil || back != id {
		t.Fatalf("%+v %v", back, err)
	}
	for _, bad := range []string{"", "{", `{"pid":1,"extra":true}`, "[]"} {
		if _, err := DecodeIdentity(bad); err == nil {
			t.Errorf("%q decoded", bad)
		}
	}
}

// fakeHost is a Prober that stages a host.
type fakeHost struct {
	// reuseAfterCheck replaces the process under the PID (a start time that is not the
	// recorded one) as soon as the start time was read once: the PID is reused between
	// the check and the stop.
	reuseAfterCheck bool
	incarnation     string
	incErr          error
	procs           map[int]string // pid -> start token of the live process
	startErr        error
	stopErr         error
	stopped         []int
}

func (f *fakeHost) HostIncarnation() (string, error) { return f.incarnation, f.incErr }
func (f *fakeHost) StartToken(pid int) (string, error) {
	if f.startErr != nil {
		return "", f.startErr
	}
	s, ok := f.procs[pid]
	if !ok {
		return "", ErrNoSuchProcess
	}
	if f.reuseAfterCheck {
		f.procs[pid] = "start-of-another-process"
	}
	return s, nil
}
func (f *fakeHost) StopTree(pid int, start string) error {
	if f.stopErr != nil {
		return f.stopErr
	}
	if f.procs[pid] != start {
		return ErrNoSuchProcess
	}
	f.stopped = append(f.stopped, pid)
	delete(f.procs, pid)
	return nil
}

func TestReconcileStopsOnlyTheProcessItRecorded(t *testing.T) {
	rec := Identity{Incarnation: "boot-1", PID: 4242, Start: "start-A", Nonce: "0123456789abcdef"}
	cases := []struct {
		name     string
		host     *fakeHost
		id       Identity
		want     string
		stopPIDs []int
	}{
		{"the recorded process is still running", &fakeHost{incarnation: "boot-1", procs: map[int]string{4242: "start-A"}}, rec, VerdictStopped, []int{4242}},
		{"the PID was reused by a process that started later", &fakeHost{incarnation: "boot-1", procs: map[int]string{4242: "start-B"}}, rec, VerdictGone, nil},
		{"the PID is free", &fakeHost{incarnation: "boot-1", procs: map[int]string{}}, rec, VerdictGone, nil},
		{"the host was restarted and the PID is taken by anything", &fakeHost{incarnation: "boot-2", procs: map[int]string{4242: "start-A"}}, rec, VerdictGone, nil},
		{"the host's incarnation cannot be read", &fakeHost{incErr: ErrUnsupported, procs: map[int]string{4242: "start-A"}}, rec, VerdictUnverifiable, nil},
		{"the start time cannot be read", &fakeHost{incarnation: "boot-1", startErr: ErrUnsupported, procs: map[int]string{4242: "start-A"}}, rec, VerdictUnverifiable, nil},
		{"nothing was recorded", &fakeHost{incarnation: "boot-1", procs: map[int]string{4242: "start-A"}}, Identity{}, VerdictUnverifiable, nil},
		{"no start token was recorded", &fakeHost{incarnation: "boot-1", procs: map[int]string{4242: "x"}}, Identity{Incarnation: "boot-1", PID: 4242, Nonce: "n"}, VerdictUnverifiable, nil},
		{"no incarnation was recorded", &fakeHost{incarnation: "boot-1", procs: map[int]string{4242: "start-A"}}, Identity{PID: 4242, Start: "start-A", Nonce: "n"}, VerdictUnverifiable, nil},
		{"no nonce was recorded", &fakeHost{incarnation: "boot-1", procs: map[int]string{4242: "start-A"}}, Identity{Incarnation: "boot-1", PID: 4242, Start: "start-A"}, VerdictUnverifiable, nil},
		{"PID 1 is never signalled", &fakeHost{incarnation: "boot-1", procs: map[int]string{1: "start-A"}}, Identity{Incarnation: "boot-1", PID: 1, Start: "start-A", Nonce: "n"}, VerdictUnverifiable, nil},
		{"the PID is reused between the check and the stop", &fakeHost{incarnation: "boot-1", procs: map[int]string{4242: "start-A"}, reuseAfterCheck: true}, rec, VerdictGone, nil},
		{"the process cannot be stopped", &fakeHost{incarnation: "boot-1", procs: map[int]string{4242: "start-A"}, stopErr: errors.New("denied")}, rec, VerdictStopFailed, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Reconcile(tc.host, tc.id); got != tc.want {
				t.Fatalf("%s, want %s", got, tc.want)
			}
			if fmt.Sprint(tc.host.stopped) != fmt.Sprint(tc.stopPIDs) && !(len(tc.host.stopped) == 0 && len(tc.stopPIDs) == 0) {
				t.Fatalf("stopped %v, want %v", tc.host.stopped, tc.stopPIDs)
			}
		})
	}
}

// TestReconcileOnTheRealOSNeverTouchesAnUnrelatedProcess is the PID-reuse case against
// real processes: a live process whose PID a stale record names, with a start time
// that is not the record's, is left running; the process the record is about is
// stopped.
func TestReconcileOnTheRealOSNeverTouchesAnUnrelatedProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("real-process reconcile is exercised on linux and darwin")
	}
	start := func() *exec.Cmd {
		c := exec.Command(helperExe(t), helperArgs("sleep")...)
		c.Env = helperEnv
		configure(c)
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Process.Kill(); _, _ = c.Process.Wait() })
		return c
	}
	var prober OSProber

	// The process a record is about: stopped.
	own := start()
	id := identify(own.Process.Pid)
	if id.Start == "" || id.Incarnation == "" {
		t.Fatalf("%+v", id)
	}
	if got := Reconcile(prober, id); got != VerdictStopped {
		t.Fatalf("%s", got)
	}
	done := make(chan struct{})
	go func() { _, _ = own.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the recorded process is still running")
	}

	// An unrelated process under a PID a stale record names: left alone.
	other := start()
	stale := identify(other.Process.Pid)
	stale.Start = "forged-earlier-start"
	if got := Reconcile(prober, stale); got != VerdictGone {
		t.Fatalf("%s", got)
	}
	if !alive(other.Process.Pid) {
		t.Fatal("an unrelated process was stopped")
	}
	// A record of another boot: nothing is signalled either.
	stale = identify(other.Process.Pid)
	stale.Incarnation = "another-boot"
	if got := Reconcile(prober, stale); got != VerdictGone || !alive(other.Process.Pid) {
		t.Fatalf("%s", got)
	}
}

func TestRunStopsAProcessThatLeftItsGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are a unix notion")
	}
	s, out, _ := spec(t, "escape")
	s.Timeout = 700 * time.Millisecond
	var id Identity
	s.OnStart = func(i Identity) error { id = i; return nil }
	start := time.Now()
	res, err := Run(context.Background(), s)
	if err != nil || !res.TimedOut || !res.Signaled || res.ExitCode != nil {
		t.Fatalf("%+v %v", res, err)
	}
	if out.String() != "escaped" {
		t.Fatalf("the helper did not leave its group: %q", out.String())
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	waitDead(t, id.PID)
}
