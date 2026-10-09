package tools

import (
	"context"
	"errors"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/process"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// VerificationExecution is one host-owned check's durable result.
type VerificationExecution struct {
	Result             protocol.Verification
	Events             []protocol.Event
	UnresolvedActionID string
}

// Verify runs the policy's optional fixed verifier through the same process profile,
// environment, workspace, deadline, cancellation, capture and Action fences as
// process.exec. It creates no model Tool call or Thread history item.
func (r *RunTools) Verify(ctx context.Context) (VerificationExecution, error) {
	out := VerificationExecution{Result: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}}}
	plan, criteriaRevision, configured := r.pol.VerificationPlan()
	if !configured {
		return out, nil
	}
	out.Result.CriteriaRevision = protocol.Str(criteriaRevision)
	if criteriaRevision == "" {
		return out, errors.New("tools: the effective verification criteria were not frozen")
	}
	if ctx.Err() != nil {
		return out, nil
	}
	a := execArgs{Executable: plan.Executable, Argv: plan.Argv, Cwd: plan.Cwd, EnvProfileRef: plan.EnvProfileRef, TimeoutSeconds: plan.TimeoutSeconds}
	profile, envProfile, authErr := r.authorizeProcess(a)
	if authErr != nil || profile.Name != plan.ProcessProfileRef {
		return out, nil
	}
	env, err := process.BuildEnv(envProfile)
	if err != nil {
		return out, nil
	}
	dir, err := r.pol.Scope().ResolveDir(plan.Cwd)
	if err != nil {
		return out, nil
	}
	used, err := r.rt.store.RunCaptureBytes(ctx, r.rec.RunID)
	if err != nil {
		return out, err
	}
	captureLimit := max(r.rec.Limits.MaxCaptureBytes-used, 0)
	timeout := time.Duration(plan.TimeoutSeconds) * time.Second
	if left := r.rec.DeadlineAt.Sub(r.rt.store.Now()); left < timeout {
		timeout = max(left, time.Millisecond)
	}
	if ctx.Err() != nil {
		return out, nil
	}
	actionID, attemptID := identity.NewActionID().String(), identity.NewAttemptID().String()
	started, err := r.rt.store.StartVerificationAttempt(dbCtx(ctx), r.fence, sqlite.StartVerificationInput{
		ActionID: actionID, AttemptID: attemptID, PolicyRevision: r.pol.Revision, CriteriaRevision: criteriaRevision,
	})
	switch {
	case errors.Is(err, sqlite.ErrControlChanged), errors.Is(err, sqlite.ErrDeadlinePassed):
		return out, nil
	case errors.Is(err, sqlite.ErrPolicyChanged):
		return out, nil
	case err != nil:
		return out, err
	}
	out.Events = append(out.Events, started.Events...)

	pctx, stop := context.WithCancel(ctx)
	defer stop()
	sink := func(purpose string) *captureSink {
		return &captureSink{ctx: ctx, store: r.rt.store, fence: r.fence, attemptID: attemptID, purpose: purpose,
			evidenceID: identity.NewEvidenceID().String(), stop: stop}
	}
	stdout, stderr := sink(sqlite.PurposeVerificationStdout), sink(sqlite.PurposeVerificationStderr)
	var startErr error
	res, runErr := process.Run(pctx, process.Spec{
		Executable: plan.Executable, Argv: plan.Argv, Dir: dir.Abs, Env: env, Timeout: timeout, CaptureLimit: captureLimit,
		Stdout: stdout, Stderr: stderr,
		OnStart: func(id process.Identity) error {
			startErr = r.rt.store.RecordProcessStart(dbCtx(ctx), r.fence, attemptID, id.Incarnation, id.Encode())
			return startErr
		},
	})
	werr := errors.Join(stdout.finish(), stderr.finish())
	captureComplete := res.CaptureComplete()
	captures := []sqlite.SealCapture{
		{EvidenceID: stdout.evidenceID, Created: stdout.created, Purpose: sqlite.PurposeVerificationStdout},
		{EvidenceID: stderr.evidenceID, Created: stderr.created, Purpose: sqlite.PurposeVerificationStderr},
	}
	var exitCode *int64
	if res.ExitCode != nil {
		code := int64(*res.ExitCode)
		exitCode = &code
	}
	status, effect := "failed", "failed"
	switch {
	case werr != nil || startErr != nil:
		// The process existed, but capture or process identity could not be durably
		// observed. Keep the dispatched Action and close it as unknown when the store
		// is writable; recovery will do the same if it is not.
		status, effect = "unknown", "unknown"
	case runErr != nil && !res.Started:
		// A spawn refusal is known to have made no process effect.
	case runErr != nil:
		status, effect = "unknown", "unknown"
	case res.Cancelled:
		effect = "cancelled"
	case res.TimedOut, res.CaptureLimited, res.ExitCode == nil:
	case res.ExitCode != nil && *res.ExitCode == 0 && captureComplete:
		status, effect = "passed", "completed"
	}
	if status == "unknown" {
		captureComplete = false
	}
	completed, err := r.rt.store.CompleteVerificationAttempt(dbCtx(ctx), r.fence, sqlite.CompleteVerificationInput{
		ActionID: actionID, AttemptID: attemptID, CriteriaRevision: criteriaRevision, Status: status, EffectState: effect,
		ExitCode: exitCode, CaptureComplete: captureComplete, Captures: captures,
	})
	if err != nil {
		return out, err
	}
	out.Events = append(out.Events, completed.Events...)
	out.Result.Status = status
	out.Result.EvidenceIDs = append([]string{}, completed.ResultEvidenceIDs...)
	if status == "unknown" {
		out.UnresolvedActionID = actionID
	}
	return out, nil
}
