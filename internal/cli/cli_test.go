package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/cli"
	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/internal/service"
	"github.com/Nyukimin/RenCrow_Harness/internal/session"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// run executes one CLI invocation and returns its exit code and both streams.
func run(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errw bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code = cli.Run(ctx, args, strings.NewReader(stdin), &out, &errw)
	return code, out.String(), errw.String()
}

// deployment is an initialized throw-away deployment.
func deployment(t *testing.T) *harnesstest.Layout {
	t.Helper()
	l := harnesstest.NewLayout(t, harnesstest.Options{})
	if code, out, errs := run(t, "", "init", "--config", l.Config, "--data-root", l.Data); code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errs)
	}
	return l
}

// seeded opens the store the way a server would and creates one session with one
// accepted turn, returning what the CLI commands should find.
type seeded struct {
	info  protocol.SessionInfo
	start protocol.StartResult
}

func seed(t *testing.T, l *harnesstest.Layout, text string) seeded {
	t.Helper()
	dep, err := config.Load(l.Config)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(context.Background(), dep.DataRoot, sqlite.Options{Clock: intake.SystemClock{}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	writers, err := session.NewWriters(store)
	if err != nil {
		t.Fatal(err)
	}
	defer writers.Close()
	svc, err := service.New(service.Options{Deployment: dep, Store: store, Writers: writers, Entrypoint: protocol.EntrypointCLIExec, BuildRevision: "t"})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := dep.Binding("fixture-local")
	if err != nil {
		t.Fatal(err)
	}
	openJSON, _ := json.Marshal(protocol.SessionOpenInput{WorkspacePath: l.Work, Binding: binding, PolicyRef: l.WorkspacePolicy(), ExecutionMode: protocol.ModeTrustedHost,
		IdempotencyKey: "cli.seed.open." + identity.NewRequestID().String()})
	open, _, err := svc.SessionOpen(context.Background(), openJSON)
	if err != nil {
		t.Fatal(err)
	}
	in := protocol.StartInput{ThreadID: open.Session.ThreadID, Input: protocol.InputMessage{Text: text}, ContextBlocks: []protocol.ContextBlock{},
		IdempotencyKey: "cli.seed.start." + identity.NewRequestID().String(),
		Limits:         protocol.Limits{MaxModelSteps: 10, MaxToolCallsPerStep: 8, DeadlineSeconds: 1800, MaxCaptureBytes: 67108864, MaxGenerationAttempts: 32}}
	startJSON, _ := json.Marshal(in)
	start, _, err := svc.TurnStart(context.Background(), in, startJSON)
	if err != nil {
		t.Fatal(err)
	}
	return seeded{info: open.Session, start: start}
}

func TestUsageErrorsAreExit64(t *testing.T) {
	l := deployment(t)
	for name, args := range map[string][]string{
		"no command":              {},
		"unknown command":         {"frobnicate"},
		"serve without --stdio":   {"serve", "--config", l.Config},
		"serve without config":    {"serve", "--stdio"},
		"relative config":         {"serve", "--stdio", "--config", "config.json"},
		"init without data root":  {"init", "--config", l.Config},
		"init relative data root": {"init", "--config", l.Config, "--data-root", "data"},
		"unknown flag":            {"serve", "--stdio", "--config", l.Config, "--bogus"},
		"sessions without sub":    {"sessions", "--config", l.Config},
		"sessions unknown sub":    {"sessions", "purge", "--config", l.Config},
		"inspect without run":     {"inspect", "--config", l.Config, "--json"},
		"inspect bad run id":      {"inspect", "--config", l.Config, "--run", "not-a-run", "--json"},
		"evidence without range":  {"evidence", "--config", l.Config, "--id", "evd_00000000-0000-7000-8000-000000000001"},
		"evidence bad id":         {"evidence", "--config", l.Config, "--id", "x", "--start", "0", "--end", "1"},
		"evidence inverted":       {"evidence", "--config", l.Config, "--id", "evd_00000000-0000-7000-8000-000000000001", "--start", "5", "--end", "1"},
		"compact without thread":  {"compact", "--config", l.Config},
		"compact bad thread":      {"compact", "--config", l.Config, "--thread", "not-a-thread"},
		"compact relative config": {"compact", "--config", "config.json", "--thread", "thr_00000000-0000-7000-8000-000000000001"},
		"compact unknown flag":    {"compact", "--config", l.Config, "--thread", "thr_00000000-0000-7000-8000-000000000001", "--bogus"},
	} {
		code, out, errs := run(t, "", args...)
		if code != 64 || out != "" || errs == "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, out, errs)
		}
	}
}

// TestTheRunCommandsRefuseAWrongRequestWithExit64AndNothingOnStdout: chat, exec and resume
// refuse what is wrong in their command line, in what it points at (a workspace, a binding, a
// file, a task) or in a value, before any session is made, with the invalid-request code and
// not one of the Run outcomes 2 to 6 (no Run existed), and write nothing to standard output
// (exec --json promises JSON lines and nothing else).
func TestTheRunCommandsRefuseAWrongRequestWithExit64AndNothingOnStdout(t *testing.T) {
	l := deployment(t)
	good := filepath.Join(l.Dir, "in.txt")
	if err := os.WriteFile(good, []byte("do it\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(l.Dir, "empty.txt")
	bad := filepath.Join(l.Dir, "bad.txt")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("a\xffb"), 0o600); err != nil {
		t.Fatal(err)
	}
	notWorkspace := filepath.Join(l.Dir, "not-a-workspace")
	if err := os.Mkdir(notWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	execArgs := func(extra ...string) []string {
		return append([]string{"exec", "--config", l.Config, "--workspace", l.Work, "--binding", "fixture-local", "--input-file", good, "--json"}, extra...)
	}
	for name, args := range map[string][]string{
		"exec with nothing":                  {"exec"},
		"exec without an input file":         {"exec", "--config", l.Config, "--workspace", l.Work, "--binding", "fixture-local", "--json"},
		"exec with a relative input file":    {"exec", "--config", l.Config, "--workspace", l.Work, "--binding", "fixture-local", "--input-file", "in.txt"},
		"exec with an input file not there":  {"exec", "--config", l.Config, "--workspace", l.Work, "--binding", "fixture-local", "--input-file", filepath.Join(l.Dir, "none.txt")},
		"exec with a directory as the input": {"exec", "--config", l.Config, "--workspace", l.Work, "--binding", "fixture-local", "--input-file", l.Dir},
		"exec with an empty input":           {"exec", "--config", l.Config, "--workspace", l.Work, "--binding", "fixture-local", "--input-file", empty},
		"exec with an input of bad UTF-8":    {"exec", "--config", l.Config, "--workspace", l.Work, "--binding", "fixture-local", "--input-file", bad},
		"exec with a binding not named":      {"exec", "--config", l.Config, "--workspace", l.Work, "--binding", "nope", "--input-file", good, "--json"},
		"exec without a binding":             {"exec", "--config", l.Config, "--workspace", l.Work, "--input-file", good, "--json"},
		"exec with a workspace not allowed":  {"exec", "--config", l.Config, "--workspace", notWorkspace, "--binding", "fixture-local", "--input-file", good, "--json"},
		"exec with a relative workspace":     {"exec", "--config", l.Config, "--workspace", "work", "--binding", "fixture-local", "--input-file", good, "--json"},
		"exec with a workspace not clean":    {"exec", "--config", l.Config, "--workspace", l.Work + "/../work", "--binding", "fixture-local", "--input-file", good, "--json"},
		"exec with a mode not known":         execArgs("--mode", "everything"),
		"exec with an origin not known":      execArgs("--origin", "root"),
		"exec with a stray argument":         execArgs("extra"),
		"exec with an unknown flag":          execArgs("--bogus"),
		"exec with a relative config":        {"exec", "--config", "config.json", "--workspace", l.Work, "--binding", "fixture-local", "--input-file", good},
		"chat with nothing":                  {"chat"},
		"chat with a binding not named":      {"chat", "--config", l.Config, "--workspace", l.Work, "--binding", "nope"},
		"chat with a workspace not allowed":  {"chat", "--config", l.Config, "--workspace", notWorkspace, "--binding", "fixture-local"},
		"chat with a mode not known":         {"chat", "--config", l.Config, "--workspace", l.Work, "--binding", "fixture-local", "--mode", "everything"},
		"chat with an unknown flag":          {"chat", "--config", l.Config, "--workspace", l.Work, "--binding", "fixture-local", "--bogus"},
		"resume with nothing":                {"resume"},
		"resume with a task that is not one": {"resume", "--config", l.Config, "--task", "x", "--last-run", "run_00000000-0000-7000-8000-000000000001"},
		"resume with a run that is not one":  {"resume", "--config", l.Config, "--task", "tsk_00000000-0000-7000-8000-000000000001", "--last-run", "x"},
		"resume without a last run":          {"resume", "--config", l.Config, "--task", "tsk_00000000-0000-7000-8000-000000000001"},
	} {
		code, out, errs := run(t, "some input\n", args...)
		// 2 to 6 are Run outcomes; no Run existed, so none of them may be used.
		if code != 64 || out != "" || errs == "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, out, errs)
		}
	}
	// A request that is well formed and points at something that does not exist is a failure
	// of the operation (1), not of the command line.
	code, out, errs := run(t, "", "resume", "--config", l.Config, "--task", "tsk_00000000-0000-7000-8000-000000000001", "--last-run", "run_00000000-0000-7000-8000-000000000001", "--json")
	if code != 1 || out != "" || errs == "" {
		t.Errorf("resume of a run that does not exist: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if n := countRows(t, l, "sessions"); n != 0 {
		t.Fatalf("a refused command created %d sessions", n)
	}
}

func countRows(t *testing.T, l *harnesstest.Layout, table string) int {
	t.Helper()
	code, out, _ := run(t, "", "sessions", "list", "--config", l.Config)
	if code != 0 {
		t.Fatal("sessions list failed")
	}
	_ = table
	return strings.Count(out, "\n")
}

func TestInitCreatesTheStoreAndNeverTouchesAnExistingOne(t *testing.T) {
	l := harnesstest.NewLayout(t, harnesstest.Options{})
	if _, err := os.Stat(l.Data); err == nil {
		t.Fatal("the fixture must start without a data root")
	}
	code, out, errs := run(t, "", "init", "--config", l.Config, "--data-root", l.Data)
	if code != 0 || !strings.Contains(out, "initialized") || errs != "" {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	info, err := os.Stat(l.Data)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the data root must be owner-only: %v %v", info, err)
	}
	dbPath := filepath.Join(l.Data, sqlite.DatabaseFile)
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	code, out, errs = run(t, "", "init", "--config", l.Config, "--data-root", l.Data)
	if code != 1 || out != "" || !strings.Contains(errs, "already") {
		t.Fatalf("a second init: %d %q %q", code, out, errs)
	}
	after, _ := os.ReadFile(dbPath)
	if !bytes.Equal(before, after) {
		t.Fatal("a refused init changed the existing store")
	}
}

func TestInitRefusesBeforeCreatingAnythingWhenTheConfigDoesNotMatch(t *testing.T) {
	l := harnesstest.NewLayout(t, harnesstest.Options{})
	other := filepath.Join(l.Dir, "somewhere-else")
	code, out, errs := run(t, "", "init", "--config", l.Config, "--data-root", other)
	if code != 64 || out != "" || errs == "" {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	if _, err := os.Stat(other); err == nil {
		t.Fatal("a data root that the config does not name was created")
	}
	if _, err := os.Stat(l.Data); err == nil {
		t.Fatal("init created the config's data root although the arguments disagreed")
	}

	// A config that does not validate leaves no data root behind either.
	l.Cfg["gateway"].(map[string]any)["base_url"] = "http://203.0.113.9:8090/v1"
	l.Write()
	code, _, errs = run(t, "", "init", "--config", l.Config, "--data-root", l.Data)
	if code != 64 || errs == "" {
		t.Fatalf("%d %q", code, errs)
	}
	if _, err := os.Stat(l.Data); err == nil {
		t.Fatal("init left a data root behind after an invalid config")
	}
	// A data root whose parent is missing is a usage error, not a created tree.
	l2 := harnesstest.NewLayout(t, harnesstest.Options{})
	deep := filepath.Join(l2.Dir, "no", "such", "parent", "data")
	l2.Cfg["data_root"] = deep
	l2.Write()
	if code, _, _ := run(t, "", "init", "--config", l2.Config, "--data-root", deep); code != 64 {
		t.Fatalf("%d", code)
	}
	if _, err := os.Stat(filepath.Join(l2.Dir, "no")); err == nil {
		t.Fatal("init created missing parent directories")
	}
}

func TestSessionsListShowsWhatTheCallerCanRead(t *testing.T) {
	l := deployment(t)
	if code, out, errs := run(t, "", "sessions", "list", "--config", l.Config); code != 0 || out != "" || errs != "" {
		t.Fatalf("an empty store lists nothing: %d %q %q", code, out, errs)
	}
	s := seed(t, l, "go")
	code, out, errs := run(t, "", "sessions", "list", "--config", l.Config)
	if code != 0 || errs != "" {
		t.Fatalf("%d %q", code, errs)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("%q", out)
	}
	fields := strings.Split(lines[0], "\t")
	real, _ := filepath.EvalSymlinks(l.Work)
	want := []string{s.info.ThreadID, s.info.SessionID, "trusted_host", s.start.RunID, real}
	if len(fields) != len(want) {
		t.Fatalf("%q", lines[0])
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Errorf("field %d is %q, want %q", i, fields[i], want[i])
		}
	}
}

func TestStoreCommandsNeedAnInitializedStore(t *testing.T) {
	l := harnesstest.NewLayout(t, harnesstest.Options{CreateData: true})
	for _, args := range [][]string{
		{"sessions", "list", "--config", l.Config},
		{"inspect", "--config", l.Config, "--run", "run_00000000-0000-7000-8000-000000000001", "--json"},
		{"evidence", "--config", l.Config, "--id", "evd_00000000-0000-7000-8000-000000000001", "--start", "0", "--end", "1"},
		{"serve", "--stdio", "--config", l.Config},
	} {
		code, out, errs := run(t, "", args...)
		if code != 1 || out != "" || !strings.Contains(errs, "init") {
			t.Errorf("%v: %d %q %q", args, code, out, errs)
		}
	}
	if _, err := os.Stat(filepath.Join(l.Data, sqlite.DatabaseFile)); err == nil {
		t.Fatal("a read command created a store")
	}
}

func TestInspectPrintsTheRunAsCanonicalJSON(t *testing.T) {
	l := deployment(t)
	s := seed(t, l, "go")
	code, out, errs := run(t, "", "inspect", "--config", l.Config, "--run", s.start.RunID, "--json")
	if code != 0 || errs != "" {
		t.Fatalf("%d %q", code, errs)
	}
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("one JSON object on one line: %q", out)
	}
	info, err := protocol.Decode[protocol.RunInfo]([]byte(out))
	if err != nil || info.RunID != s.start.RunID || info.Phase != "Admitting" || info.Terminal {
		t.Fatalf("%v %+v", err, info)
	}
	// Without --json the same facts are printed for a person.
	code, out, _ = run(t, "", "inspect", "--config", l.Config, "--run", s.start.RunID)
	if code != 0 || !strings.Contains(out, s.start.RunID) || !strings.Contains(out, "Admitting") || strings.HasPrefix(out, "{") {
		t.Fatalf("%d %q", code, out)
	}
	// A run that does not exist (or is not readable) is a failure, not a usage error.
	code, out, errs = run(t, "", "inspect", "--config", l.Config, "--run", "run_00000000-0000-7000-8000-0000000000aa", "--json")
	if code != 1 || out != "" || !strings.Contains(errs, "FORBIDDEN") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
}

func TestEvidencePrintsExactlyTheRequestedBytes(t *testing.T) {
	l := deployment(t)
	text := "日本語のtext"
	s := seed(t, l, text)
	id := s.start.Intake.EvidenceID
	n := fmt.Sprint(len(text))

	code, out, errs := run(t, "", "evidence", "--config", l.Config, "--id", id, "--start", "0", "--end", n)
	if code != 0 || out != text {
		t.Fatalf("%d %q", code, out)
	}
	if !strings.Contains(errs, "total=") || !strings.Contains(errs, "partial=false") || strings.Contains(errs, "日本語") {
		t.Fatalf("metadata, but never the content, goes to stderr: %q", errs)
	}
	code, out, _ = run(t, "", "evidence", "--config", l.Config, "--id", id, "--start", "12", "--end", "16")
	if code != 0 || out != "text" {
		t.Fatalf("%d %q", code, out)
	}
	// A text range cannot cut a character; the raw projection can.
	code, out, errs = run(t, "", "evidence", "--config", l.Config, "--id", id, "--start", "1", "--end", "3")
	if code != 64 || out != "" || !strings.Contains(errs, "INVALID_RANGE") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	code, out, _ = run(t, "", "evidence", "--config", l.Config, "--id", id, "--start", "1", "--end", "3", "--projection", "raw")
	if code != 0 || out != text[1:3] {
		t.Fatalf("%d %q", code, out)
	}
	code, _, errs = run(t, "", "evidence", "--config", l.Config, "--id", "evd_00000000-0000-7000-8000-0000000000aa", "--start", "0", "--end", "1")
	if code != 1 || !strings.Contains(errs, "FORBIDDEN") {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestServeStdioSpeaksTheProtocolOnStdoutOnly(t *testing.T) {
	l := deployment(t)
	rid := func(n int) string { return fmt.Sprintf("req_00000000-0000-7000-8000-%012x", n) }
	input := strings.Join([]string{
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"initialize","params":{"client_name":"t","client_version":"1","protocol_version":"rencrow-harness/v1"}}`, rid(1)),
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"service/shutdown","params":{"mode":"drain","deadline_seconds":5}}`, rid(2)),
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"service/capabilities","params":{}}`, rid(3)),
	}, "\n") + "\n"
	code, out, errs := run(t, input, "serve", "--stdio", "--config", l.Config)
	if code != 0 {
		t.Fatalf("%d %q", code, errs)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("the request after shutdown is not answered: %q", out)
	}
	for i, id := range []string{rid(1), rid(2)} {
		var m map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &m); err != nil || m["jsonrpc"] != "2.0" || m["id"] != id || m["result"] == nil {
			t.Fatalf("line %d: %q (%v)", i, lines[i], err)
		}
	}
	if strings.Contains(errs, l.Dir) || strings.Contains(errs, "client_name") {
		t.Fatalf("stderr carries a path or request content: %q", errs)
	}
}

func TestServeEndsCleanlyAtEndOfInputAndReleasesTheStore(t *testing.T) {
	l := deployment(t)
	code, out, _ := run(t, "", "serve", "--stdio", "--config", l.Config)
	if code != 0 || out != "" {
		t.Fatalf("%d %q", code, out)
	}
	// Everything the server held is released: another one can start, and so can a reader.
	if code, _, errs := run(t, "", "serve", "--stdio", "--config", l.Config); code != 0 {
		t.Fatalf("%d %q", code, errs)
	}
	if code, _, errs := run(t, "", "sessions", "list", "--config", l.Config); code != 0 {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestRunDoesNotReadStdinForCommandsThatDoNotNeedIt(t *testing.T) {
	l := deployment(t)
	var out, errw bytes.Buffer
	code := cli.Run(context.Background(), []string{"sessions", "list", "--config", l.Config}, panicReader{}, &out, &errw)
	if code != 0 {
		t.Fatalf("%d %q", code, errw.String())
	}
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("stdin was read") }

// TestServeFailsClosedOnAGatewayItCannotUse: a process that drives Runs is given the client
// of the configured Gateway or does not start. A configuration the client refuses (a base
// URL that is not a plain loopback http(s) URL) stops `serve` before it serves or touches
// the store, with a usage error that does not repeat the address; one that names a
// Gateway that is merely not there is not an error of the configuration: the process
// starts without having contacted it.
func TestServeFailsClosedOnAGatewayItCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"a path with a dot segment", "http://127.0.0.1:8090/v1/../x"},
		{"an empty path segment", "http://127.0.0.1:8090/v1//x"},
		{"an escaped path", "http://127.0.0.1:8090/v1%2Fx"},
		{"a host that is not loopback", "http://203.0.113.9:8090/v1"},
		{"a credential in the address", "http://user:secret-marker@127.0.0.1:8090/v1"},
		{"a query", "http://127.0.0.1:8090/v1?token=secret-marker"},
		{"another scheme", "ftp://127.0.0.1:8090/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := deployment(t)
			l.Cfg["gateway"].(map[string]any)["base_url"] = tc.url
			l.Write()
			dbBefore, _ := os.ReadFile(filepath.Join(l.Data, sqlite.DatabaseFile))
			code, out, errs := run(t, "", "serve", "--stdio", "--config", l.Config)
			if code != 64 || out != "" || errs == "" || strings.Contains(errs, "secret-marker") || strings.Contains(errs, "8090") {
				t.Fatalf("%d %q %q", code, out, errs)
			}
			if after, _ := os.ReadFile(filepath.Join(l.Data, sqlite.DatabaseFile)); !bytes.Equal(dbBefore, after) {
				t.Fatal("a refused serve changed the store")
			}
		})
	}
	t.Run("a Gateway that is not there does not stop the process", func(t *testing.T) {
		l := deployment(t)
		l.Cfg["gateway"].(map[string]any)["base_url"] = "http://127.0.0.1:1/v1"
		l.Write()
		code, out, errs := run(t, "", "serve", "--stdio", "--config", l.Config)
		if code != 0 || out != "" || !strings.Contains(errs, "serving") {
			t.Fatalf("%d %q %q", code, out, errs)
		}
	})
	t.Run("the commands that only read do not build a client at all", func(t *testing.T) {
		l := deployment(t)
		l.Cfg["gateway"].(map[string]any)["base_url"] = "http://127.0.0.1:8090/v1/../x"
		l.Write()
		if code, out, errs := run(t, "", "sessions", "list", "--config", l.Config); code != 0 || out != "" || errs != "" {
			t.Fatalf("%d %q %q", code, out, errs)
		}
	})
}
