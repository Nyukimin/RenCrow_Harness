package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/service"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/internal/transport/stdio"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// cmdInit: init --config ABS --data-root ABS. Initialization is explicit and never
// touches an existing store. The data root named on the command line must be the
// one the configuration names, so the store is created exactly where the
// configuration will look for it. If the directory is missing it is created
// (owner-only; its parent must exist) so the configuration can be validated, and
// it is removed again if nothing was initialized.
func cmdInit(ctx context.Context, args []string, out io.Writer) *exitError {
	fs := newFlags("init")
	configPath := fs.String("config", "", "absolute path of the configuration file")
	dataRoot := fs.String("data-root", "", "absolute path of the data root")
	if e := parseFlags(fs, args); e != nil {
		return e
	}
	if e := requireAbs("--config", *configPath); e != nil {
		return e
	}
	if e := requireAbs("--data-root", *dataRoot); e != nil {
		return e
	}
	if filepath.Clean(*dataRoot) != *dataRoot {
		return usageErr("--data-root must be written in clean form")
	}

	created := false
	if _, err := os.Lstat(*dataRoot); errors.Is(err, os.ErrNotExist) {
		if err := fsperm.CreatePrivateDir(*dataRoot); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return usageErr("--data-root cannot be created (its parent must exist)")
			}
			if err := fsperm.CheckOwnerOnlyDir(*dataRoot); err != nil {
				return failure("the data root is not private")
			}
		} else {
			created = true
		}
	} else if err != nil {
		return failure("--data-root cannot be examined")
	}
	undo := func() {
		if created {
			_ = os.Remove(*dataRoot) // only if still empty
		}
	}

	dep, err := config.Load(*configPath)
	if err != nil {
		undo()
		return usageErr("the configuration is invalid: %v", err)
	}
	real, err := filepath.EvalSymlinks(*dataRoot)
	if err != nil || real != dep.DataRoot {
		undo()
		return usageErr("--data-root is not the data_root of the configuration")
	}
	switch err := sqlite.Init(ctx, dep.DataRoot); {
	case errors.Is(err, sqlite.ErrAlreadyInitialized):
		return failure("the data root already holds a store; nothing was changed")
	case errors.Is(err, sqlite.ErrMigrationBusy):
		return failure("another process holds the data root's migration lock")
	case err != nil:
		return failure("the store could not be created: %v", err)
	}
	fmt.Fprintf(out, "initialized the store (schema version %d)\n", sqlite.SchemaVersion)
	return nil
}

// cmdServe: serve --stdio --config ABS.
func cmdServe(ctx context.Context, args []string, in io.Reader, out, errw io.Writer) *exitError {
	fs := newFlags("serve")
	stdioFlag := fs.Bool("stdio", false, "serve the native protocol on stdin and stdout")
	configPath := fs.String("config", "", "absolute path of the configuration file")
	if e := parseFlags(fs, args); e != nil {
		return e
	}
	if !*stdioFlag {
		return usageErr("serve needs --stdio, the only transport")
	}
	rt, e := openRuntime(ctx, *configPath, true)
	if e != nil {
		return e
	}
	defer rt.close()
	svc, e := rt.service(service.StdioEntrypoint(rt.dep.Caller.Principal), true)
	if e != nil {
		return e
	}
	fmt.Fprintf(errw, "rencrow-harness: serving %s on stdio\n", protocol.ProtocolVersion)
	err := stdio.Serve(ctx, svc, in, out, stdio.Options{Diag: errw})
	// The Runs this process drives get the time the shutdown allowed to end; the ones
	// still going are stopped and record how they ended before the locks are released.
	svc.Quiesce()
	if err != nil {
		return failure("the connection ended abnormally: %v", err)
	}
	return nil
}

// cmdSessions: sessions list --config ABS.
func cmdSessions(ctx context.Context, args []string, out io.Writer) *exitError {
	if len(args) == 0 || args[0] != "list" {
		return usageErr("sessions: the only subcommand is `list`")
	}
	fs := newFlags("sessions list")
	configPath := fs.String("config", "", "absolute path of the configuration file")
	if e := parseFlags(fs, args[1:]); e != nil {
		return e
	}
	rt, e := openRuntime(ctx, *configPath, false)
	if e != nil {
		return e
	}
	defer rt.close()
	svc, e := rt.service(protocol.EntrypointCLIExec, false)
	if e != nil {
		return e
	}
	var cursor *string
	for {
		page, err := svc.SessionList(ctx, protocol.SessionListInput{Cursor: cursor, Limit: 100})
		if err != nil {
			return exitFor(err)
		}
		for _, s := range page.Sessions {
			active := "-"
			if s.ActiveRunID != nil {
				active = *s.ActiveRunID
			}
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", s.ThreadID, s.SessionID, s.ExecutionMode, active, s.WorkspacePath)
		}
		if page.NextCursor == nil {
			return nil
		}
		cursor = page.NextCursor
	}
}

// cmdInspect: inspect --config ABS --run RUN_ID [--json].
func cmdInspect(ctx context.Context, args []string, out io.Writer) *exitError {
	fs := newFlags("inspect")
	configPath := fs.String("config", "", "absolute path of the configuration file")
	runID := fs.String("run", "", "the run to show")
	asJSON := fs.Bool("json", false, "print the RunInfo as one JSON object")
	if e := parseFlags(fs, args); e != nil {
		return e
	}
	if *runID == "" {
		return usageErr("--run is required")
	}
	if _, err := identity.ParseRunID(*runID); err != nil {
		return usageErr("--run is not a run ID")
	}
	rt, e := openRuntime(ctx, *configPath, false)
	if e != nil {
		return e
	}
	defer rt.close()
	svc, e := rt.service(protocol.EntrypointCLIExec, false)
	if e != nil {
		return e
	}
	info, err := svc.RunGet(ctx, protocol.RunGetInput{RunID: *runID})
	if err != nil {
		return exitFor(err)
	}
	if *asJSON {
		body, err := protocol.Encode(info)
		if err != nil {
			return exitFor(err)
		}
		fmt.Fprintf(out, "%s\n", body)
		return nil
	}
	fmt.Fprintf(out, "run_id: %s\ntask_id: %s\nthread_id: %s\nphase: %s\nterminal: %t\n", info.RunID, info.TaskID, info.ThreadID, info.Phase, info.Terminal)
	if info.Result != nil {
		fmt.Fprintf(out, "result: %s %s\n", info.Result.Status, info.Result.Code)
		fmt.Fprintf(out, "verification: %s\n", verificationSummary(info.Result.Verification))
	} else {
		fmt.Fprintf(out, "result: none\n")
	}
	fmt.Fprintf(out, "deadline_at: %s\ncontext_revision: %d\ncontrol_revision: %d\nlast_event_seq: %d\ngeneration_attempts: used=%d unknown=%d\n",
		info.DeadlineAt, info.ContextRevision, info.ControlRevision, info.LastEventSeq, info.GenerationAttemptsUsed, info.GenerationAttemptsUnknown)
	return nil
}

// cmdEvidence: evidence --config ABS --id EVIDENCE_ID --start N --end N. The bytes of
// the range go to stdout exactly as stored; what describes them goes to stderr.
func cmdEvidence(ctx context.Context, args []string, out, errw io.Writer) *exitError {
	fs := newFlags("evidence")
	configPath := fs.String("config", "", "absolute path of the configuration file")
	id := fs.String("id", "", "the evidence to read")
	start := fs.Uint64("start", 0, "first byte of the range")
	end := fs.Uint64("end", 0, "end of the range (exclusive)")
	projection := fs.String("projection", "text", "text (character ranges) or raw (bytes)")
	if e := parseFlags(fs, args); e != nil {
		return e
	}
	if !visited(fs, "start") || !visited(fs, "end") {
		return usageErr("--start and --end are required")
	}
	if _, err := identity.ParseEvidenceID(*id); err != nil {
		return usageErr("--id is not an evidence ID")
	}
	version := ""
	switch *projection {
	case "text":
		version = "text/v1"
	case "raw":
		version = "raw/v1"
	default:
		return usageErr("--projection is text or raw")
	}
	in := protocol.EvidenceReadInput{EvidenceID: *id, ProjectionVersion: version, Range: protocol.ByteRange{Start: *start, End: *end}}
	if err := in.Validate(); err != nil {
		return exitFor(err)
	}
	rt, e := openRuntime(ctx, *configPath, false)
	if e != nil {
		return e
	}
	defer rt.close()
	svc, e := rt.service(protocol.EntrypointCLIExec, false)
	if e != nil {
		return e
	}
	res, err := svc.EvidenceRead(ctx, in)
	if err != nil {
		return exitFor(err)
	}
	data, err := base64.StdEncoding.DecodeString(res.DataBase64)
	if err != nil {
		return failure("the stored evidence could not be decoded")
	}
	if _, err := out.Write(data); err != nil {
		return failure("the bytes could not be written")
	}
	fmt.Fprintf(errw, "evidence %s: projection=%s range=[%d,%d) total=%d partial=%t capture_complete=%t raw_hash=%s\n",
		res.EvidenceID, res.ProjectionVersion, res.ReturnedRange.Start, res.ReturnedRange.End, res.TotalBytes, res.Partial, res.CaptureComplete, res.RawHash)
	return nil
}

// cmdCompact: compact --config ABS --thread THREAD_ID [--dry-run] [--idempotency-key KEY].
// It asks the Service for the manual compaction of one idle Thread (context/compact) and
// waits for its result: the CompactResult goes to standard output as one canonical JSON
// object, a line for a person to standard error. The process drives the compaction's system
// Run itself, as `serve` would, so the Gateway of the configuration is reached from here.
//
// Exit codes: 0 when the compaction made a checkpoint (NormalCompacted or EmergencyCompacted)
// or the dry run answered; 1 for any other end (CapacityBlocked, IntegrityBlocked,
// RestartRequired, stale, cancelled, unavailable) or a refusal, with the result still on
// standard output when there is one; 64 for an invalid command line. The Run-outcome codes 2 to
// 6 belong to exec and are not used here.
//
// The revisions the request is made at are the Thread's current ones, read just before: a
// Thread that moves in between is REVISION_CONFLICT, not a compaction of another context. A
// key given with --idempotency-key makes the command repeatable (the same key and the same
// revisions answer from the first receipt); without one each invocation is its own operation.
// Ctrl-C records a stop for the Run and waits for it to end.
func cmdCompact(ctx context.Context, args []string, out, errw io.Writer) *exitError {
	fs := newFlags("compact")
	configPath := fs.String("config", "", "absolute path of the configuration file")
	thread := fs.String("thread", "", "the thread to compact")
	dry := fs.Bool("dry-run", false, "only count the context: no generation, no checkpoint")
	key := fs.String("idempotency-key", "", "the key of the operation (default: a new one)")
	if e := parseFlags(fs, args); e != nil {
		return e
	}
	if _, err := identity.ParseThreadID(*thread); err != nil {
		return usageErr("--thread is not a thread ID")
	}
	if *key == "" {
		*key = "cli.compact." + strings.ReplaceAll(identity.NewReceiptID().String(), "_", ".")
	}
	rt, e := openRuntime(ctx, *configPath, true)
	if e != nil {
		return e
	}
	defer rt.close()
	svc, e := rt.service(protocol.EntrypointCLIExec, true)
	if e != nil {
		return e
	}
	defer svc.Quiesce()

	info, err := svc.SessionGet(ctx, protocol.SessionGetInput{ThreadID: *thread})
	if err != nil {
		return exitFor(err)
	}
	in := protocol.CompactInput{ThreadID: *thread, ExpectedContextRevision: info.ContextRevision, ExpectedControlRevision: info.ControlRevision, DryRun: *dry, IdempotencyKey: *key}
	params, err := protocol.Encode(in)
	if err != nil {
		return exitFor(err)
	}
	accepted, events, err := svc.ContextCompact(ctx, in, params)
	if err != nil {
		return exitFor(err)
	}
	runID := ""
	for _, ev := range events {
		if ev.Type == protocol.EventRunStarted && ev.RunID != nil {
			runID = *ev.RunID
		}
	}
	result, e := waitCompactResult(ctx.Done(), svc, accepted.ReceiptID, runID, info.ControlRevision)
	if e != nil {
		return e
	}
	body, err := protocol.Encode(result)
	if err != nil {
		return exitFor(err)
	}
	fmt.Fprintf(out, "%s\n", body)
	outcome := "-"
	if result.Outcome != nil {
		outcome = *result.Outcome
	}
	fmt.Fprintf(errw, "compaction: %s %s receipt=%s\n", result.Status, outcome, accepted.ReceiptID)
	if result.Status == "dry_run" || result.Outcome != nil && (*result.Outcome == "NormalCompacted" || *result.Outcome == "EmergencyCompacted") {
		return nil
	}
	code := "-"
	if result.Error != nil {
		code = result.Error.Code
	}
	return failure("the compaction made no checkpoint (%s %s)", result.Status, code)
}

// waitCompactResult waits for the receipt of a manual compaction to be terminal and returns its
// CompactResult. If stop is signalled meanwhile (the command was interrupted), the stop of the
// compaction's Run is recorded (once) and the wait goes on for a bounded time: a stop is a
// request, and what the Run did before it saw it is part of the result.
func waitCompactResult(stop <-chan struct{}, svc *service.Service, receiptID, runID string, controlRevision int64) (protocol.CompactResult, *exitError) {
	bg := context.Background()
	stopped := false
	var deadline <-chan time.Time
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		rec, err := svc.ReceiptGet(bg, protocol.ReceiptGetInput{ReceiptID: receiptID})
		if err != nil {
			return protocol.CompactResult{}, exitFor(err)
		}
		if rec.Stage == "terminal" && rec.Result != nil {
			if rec.Result.Type != protocol.ReceiptCompactResult {
				return protocol.CompactResult{}, failure("the receipt does not hold a compaction result")
			}
			res, err := protocol.Decode[protocol.CompactResult](rec.Result.Value)
			if err != nil {
				return protocol.CompactResult{}, exitFor(err)
			}
			return res, nil
		}
		select {
		case <-stop:
			stop = nil
			if !stopped {
				stopped = true
				deadline = time.After(stopGrace)
				if runID != "" {
					if run, err := svc.RunGet(bg, protocol.RunGetInput{RunID: runID}); err == nil {
						stopIn := protocol.InterruptInput{RunID: runID, ExpectedControlRevision: run.ControlRevision, IdempotencyKey: "cli.compact.stop." + strings.ReplaceAll(identity.NewReceiptID().String(), "_", ".")}
						if params, err := protocol.Encode(stopIn); err == nil {
							_, _, _ = svc.TurnInterrupt(bg, params)
						}
					}
				}
			}
		case <-deadline:
			return protocol.CompactResult{}, failure("the compaction did not end after it was told to stop")
		case <-tick.C:
		}
	}
}
