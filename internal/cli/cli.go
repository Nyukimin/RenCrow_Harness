// Package cli is the command line of rencrow-harness. It is a thin user interface:
// every command that touches the store goes through the same Service as the stdio
// server, so a person and CORE are subject to the same rules.
//
// Exit codes (PROTOCOL section 2): 0 success, 64 for an invalid command line,
// invalid configuration or an invalid request value, 1 for an operational failure
// (a store that is not initialized or is busy, a refused or failed operation).
// The Run-outcome codes 2 to 6 belong to exec and resume, which drive a Run to its end, and
// are never used for anything else: a command that did not start a Run must not look like
// one that did.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelclient"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/oslock"
	"github.com/Nyukimin/RenCrow_Harness/internal/service"
	"github.com/Nyukimin/RenCrow_Harness/internal/session"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 64
)

// BuildRevision identifies the build. A release build sets it with
// -ldflags "-X github.com/Nyukimin/RenCrow_Harness/internal/cli.BuildRevision=...";
// otherwise it is the VCS revision the Go toolchain recorded, or "devel".
var BuildRevision string

func buildRevision() string {
	if BuildRevision != "" {
		return BuildRevision
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" {
			if len(rev) > 40 {
				rev = rev[:40]
			}
			if dirty {
				rev += "+dirty"
			}
			return rev
		}
	}
	return "devel"
}

// exitError is a failure together with the exit code it ends the command with.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func usageErr(format string, args ...any) *exitError {
	return &exitError{ExitUsage, fmt.Sprintf(format, args...)}
}

func failure(format string, args ...any) *exitError {
	return &exitError{ExitFailure, fmt.Sprintf(format, args...)}
}

const usageText = `usage: rencrow-harness <command> [flags]

commands:
  init      --config ABS --data-root ABS      create the store (explicit; never touches an existing one)
  serve     --stdio --config ABS              serve the native protocol on stdin/stdout
  sessions  list --config ABS                 list the sessions this profile can read
  inspect   --config ABS --run RUN_ID [--json]  show a run
  evidence  --config ABS --id EVIDENCE_ID --start N --end N [--projection text|raw]  print a byte range
  compact   --config ABS --thread THREAD_ID [--dry-run] [--idempotency-key KEY]  compact an idle thread (dry-run only counts)
  chat      --config ABS --workspace ABS --binding PROFILE [--mode MODE]  talk to a run, line by line (/help lists the commands)
  exec      --config ABS --workspace ABS --binding PROFILE --input-file ABS [--json] [--mode MODE] [--origin ORIGIN] [--idempotency-key KEY]  one run, no questions
  resume    --config ABS --task TASK_ID --last-run RUN_ID [--json] [--idempotency-key KEY]  a new run of an ended task

exit codes of exec and resume: 0 completed, 2 incomplete, 3 rejected or blocked, 4 cancelled, 5 failed, 6 restart_required;
64 invalid command line or configuration; 1 for anything that did not start a run
`

// Run executes one command line (without the program name) and returns its exit
// code. Standard output carries only the command's result (for serve, only
// protocol frames); every message for a person goes to errw.
func Run(ctx context.Context, args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errw, usageText)
		return ExitUsage
	}
	cmd, rest := args[0], args[1:]
	var err *exitError
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(out, usageText)
		return ExitOK
	case "init":
		err = cmdInit(ctx, rest, out)
	case "serve":
		err = cmdServe(ctx, rest, in, out, errw)
	case "sessions":
		err = cmdSessions(ctx, rest, out)
	case "inspect":
		err = cmdInspect(ctx, rest, out)
	case "evidence":
		err = cmdEvidence(ctx, rest, out, errw)
	case "compact":
		err = cmdCompact(ctx, rest, out, errw)
	case "chat":
		err = cmdChat(ctx, rest, in, out, errw)
	case "exec":
		err = cmdExec(ctx, rest, out, errw)
	case "resume":
		err = cmdResume(ctx, rest, out, errw)
	default:
		err = usageErr("unknown command %q\n%s", cmd, usageText)
	}
	if err != nil {
		fmt.Fprintf(errw, "rencrow-harness: %s\n", err.msg)
		return err.code
	}
	return ExitOK
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseFlags parses args and rejects anything left over.
func parseFlags(fs *flag.FlagSet, args []string) *exitError {
	if err := fs.Parse(args); err != nil {
		return usageErr("%s: %v", fs.Name(), err)
	}
	if fs.NArg() != 0 {
		return usageErr("%s: unexpected argument %q", fs.Name(), fs.Arg(0))
	}
	return nil
}

func requireAbs(flagName, value string) *exitError {
	switch {
	case value == "":
		return usageErr("%s is required", flagName)
	case strings.ContainsRune(value, 0) || !filepath.IsAbs(value):
		return usageErr("%s must be an absolute path", flagName)
	}
	return nil
}

func visited(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) { found = found || f.Name == name })
	return found
}

// runtime is an opened store and everything held for as long as the command runs.
type runtime struct {
	dep     *config.Deployment
	store   *sqlite.Store
	writers *session.Writers
	gate    *oslock.Lock
	// model is the port of the Runs this process drives (nil for a command that only
	// reads).
	model modelport.ModelPort
}

func (r *runtime) close() {
	if r.writers != nil {
		_ = r.writers.Close()
	}
	_ = r.store.Close()
	_ = r.gate.Release()
}

// openRuntime loads the configuration and opens the existing store. It holds the
// migration lock shared for as long as the runtime lives, so a migration cannot
// run under it. With drive it also prepares the writer role.
func openRuntime(ctx context.Context, configPath string, drive bool) (*runtime, *exitError) {
	if e := requireAbs("--config", configPath); e != nil {
		return nil, e
	}
	dep, err := config.Load(configPath)
	if err != nil {
		return nil, usageErr("the configuration is invalid: %v", err)
	}
	// A process that drives Runs needs its model port, and what is wrong with the
	// configuration of it is found before the store is touched.
	var model modelport.ModelPort
	if drive {
		var e *exitError
		if model, e = modelPortOf(dep); e != nil {
			return nil, e
		}
	}
	store, err := sqlite.Open(ctx, dep.DataRoot, sqlite.Options{MaxDatabaseBytes: dep.Config.Storage.MaxDatabaseBytes})
	if errors.Is(err, sqlite.ErrNotInitialized) {
		return nil, failure("the data root holds no store; run `rencrow-harness init` first")
	}
	if err != nil {
		return nil, failure("the store cannot be opened: %v", err)
	}
	gate, err := sqlite.LockMigrationShared(store.Root())
	if errors.Is(err, sqlite.ErrMigrationBusy) {
		_ = store.Close()
		return nil, failure("the store is being initialized or migrated by another process")
	}
	if err != nil {
		_ = store.Close()
		return nil, failure("the store's migration lock cannot be taken: %v", err)
	}
	r := &runtime{dep: dep, store: store, gate: gate, model: model}
	if drive {
		if r.writers, err = session.NewWriters(store, session.WithReconciler(tools.NewReconciler(nil))); err != nil {
			r.close()
			return nil, failure("the writer role cannot be prepared: %v", err)
		}
	}
	return r, nil
}

func (r *runtime) service(entrypoint string, drive bool) (*service.Service, *exitError) {
	opts := service.Options{Deployment: r.dep, Store: r.store, Entrypoint: entrypoint, BuildRevision: buildRevision()}
	if drive {
		opts.Writers = r.writers
		opts.Model = r.model
	}
	svc, err := service.New(opts)
	if err != nil {
		return nil, failure("the service cannot be built: %v", err)
	}
	return svc, nil
}

// modelPortOf builds the model port of a process that drives Runs: the client of the
// RenCrow_LLM Gateway the configuration names (gateway.base_url, at the contract it
// requires). It is a composition check and nothing more: no connection is made here, and a
// Gateway that is down is found by the Run that needs it. What is wrong with the
// configuration is found now: a contract this build does not speak, or an address the
// client refuses (it must be a plain loopback http(s) URL), stops the process before it
// serves anything, and nothing stands in for the client that was configured.
func modelPortOf(dep *config.Deployment) (modelport.ModelPort, *exitError) {
	g := dep.Config.Gateway
	if g.RequiredContract != modelport.ContractVersion {
		return nil, usageErr("the configuration is invalid: gateway.required_contract is not a contract this build speaks")
	}
	client, err := modelclient.New(g.BaseURL, modelclient.Options{})
	if err != nil {
		return nil, usageErr("the configuration is invalid: gateway.base_url: %v", err)
	}
	return client, nil
}

// exitFor turns a Service error into the command's failure. A value the caller got
// wrong is a usage error; anything else is an operational failure. The message is
// the protocol's own, which carries no content, key or path.
func exitFor(err error) *exitError {
	switch protocol.CodeOf(err) {
	case protocol.CodeInvalidParams, protocol.CodeInvalidRequest, protocol.CodeInvalidRange:
		return usageErr("%v", err)
	}
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return failure("%v", pe)
	}
	return failure("the operation failed")
}
