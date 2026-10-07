package kernel

import (
	"context"
	"errors"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// This file is what counts a Thread's context without a Run: the dry run of a manual
// compaction, and the count of a fork's copied projection. Both are made with the model
// port's describe and measure only (no generation) and store nothing, no Evidence of the
// counts included: nothing of them belongs to a Run.

// countingState is the state a count needs, for a Thread that no Run is reading: its binding,
// the Tool authority its policy gives (the catalog is part of the prompt that is counted),
// and the binding's descriptor. blocks are the typed blocks the prompt carries.
func (d *Driver) countingState(ctx context.Context, threadID string, rev sqlite.ThreadRevisions, blocks []protocol.ContextBlock) (*runState, string, error) {
	binding, err := protocol.Decode[protocol.Binding]([]byte(rev.BindingJSON))
	if err != nil {
		return nil, CodeIntegrityBlocked, nil
	}
	rec := sqlite.RunRecord{
		ThreadID: threadID, SessionID: rev.SessionID, TaskID: identity.NewTaskID().String(), TraceID: identity.NewTraceID().String(), Binding: binding,
		BindingRevision: rev.BindingRevision, PolicyRevision: rev.PolicyRevision, PolicyRef: rev.PolicyRef, WorkspacePath: rev.WorkspacePath, ExecutionMode: rev.ExecMode, System: true,
	}
	rs := &runState{rec: rec, snap: sqlite.Snapshot{Blocks: blocks}}
	if d.o.Tools != nil {
		pol, err := d.o.Tools.PolicyFor(rec)
		if err != nil {
			return nil, CodePolicyChanged, nil
		}
		rs.pol = pol
	}
	desc, code, err := d.describeBinding(ctx, binding)
	if err != nil {
		return nil, "", err
	}
	if code != "" {
		return nil, code, nil
	}
	profile, ok := desc.RecoveryProfile(modelport.ProfileSameRequest, modelport.StageAct)
	if !ok {
		return nil, modelport.CodeUnsupportedRecovery, nil
	}
	rs.desc, rs.profile = desc, profile
	return rs, "", nil
}

// DryRunCompact answers a manual compaction asked with dry_run=true (PROTOCOL section 6): it
// looks at the Thread's source (the checkpoint it stands on read back exactly, the context
// prepared from it and the applied history) and counts the live prompt. It generates nothing
// and commits nothing, and it stores nothing: the one thing recorded is its answer, by the
// caller, as the receipt of the request.
//
// The answer is a CompactResult of status dry_run, outcome null and no checkpoint: before is
// the verified count of the live prompt, and nothing else is claimed (the capacity numbers
// belong to a CapacityBlocked result, which a dry run is not, and a candidate that was never
// made has no after). What cannot be looked at is unavailable (an unverifiable count, a
// binding the model side cannot count, a stored context that does not read back), never a
// dry_run made to look like a result of a compaction.
func (d *Driver) DryRunCompact(ctx context.Context, threadID string, rev sqlite.ThreadRevisions, snap sqlite.Snapshot) protocol.CompactResult {
	unavailable := func(code string) protocol.CompactResult {
		return protocol.CompactResult{Status: compaction.StatusUnavailable, Error: compactError(code)}
	}
	rs, code, err := d.countingState(ctx, threadID, rev, snap.Blocks)
	switch {
	case err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		return unavailable(CodeDriverStopped)
	case err != nil:
		return unavailable(modelport.CodeModelUnavailable)
	case code != "":
		return unavailable(code)
	}
	rs.snap = snap
	cp, ss, err := loadStanding(ctx, d.o.Store, threadID, snap)
	if err != nil {
		return unavailable(CodeIntegrityBlocked)
	}
	prepared, err := compaction.Prepare(compaction.Snapshot{
		ThreadID: threadID, ContextRevision: snap.ContextRevision, ControlRevision: snap.ControlRevision, WriterEpoch: snap.WriterEpoch,
		PolicyRevision: snap.PolicyRevision, BindingRevision: snap.BindingRevision, Blocks: snap.Blocks, Prior: cp, SummaryCheckpoint: ss, Tail: sourcesOf(snap.Applied),
	})
	if err != nil {
		return unavailable(CodeIntegrityBlocked)
	}
	if cp != nil {
		// The sources the checkpoint names are found again as they were: a dry run looks at the
		// source (PROTOCOL section 6), not only at the checkpoint's own bytes.
		if err := d.o.Store.VerifySources(ctx, threadID, coldChecks(cp.Candidate)); err != nil {
			return unavailable(CodeIntegrityBlocked)
		}
	}
	ports := &compactPorts{d: d, ctx: ctx, runCtx: ctx, rs: rs, dry: true}
	count := func(p contextplan.Projection) (compaction.Count, string) {
		c, err := ports.CountAct(ctx, p)
		var un *compaction.UnavailableError
		switch {
		case err == nil && compaction.VerifiedCount(c):
			return c, ""
		case err == nil:
			return c, modelport.CodeBudgetUnverified
		case errors.As(err, &un):
			return c, un.Code
		case errors.Is(err, compaction.ErrIntegrity):
			return c, CodeIntegrityBlocked
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
			return c, CodeDriverStopped
		}
		return c, modelport.CodeBudgetUnverified
	}
	before, code := count(compaction.LiveProjection(prepared))
	if code != "" {
		return unavailable(code)
	}
	report := contextplan.ReportOf(before.Result)
	return protocol.CompactResult{Status: "dry_run", Before: &report}
}

// ForkCount is the count a fork's copied projection carries for the new Thread: the source
// checkpoint's own count when it is about the very request the new Thread's binding would
// be sent (the same logical input and the same binding fingerprint: the count carries over
// and nothing is asked), and a new verified count of the copied projection otherwise
// (CHECKPOINT_FORMAT section 4). It never says a count that cannot be verified is one.
//
// err is a protocol error: BUDGET_UNVERIFIED when the count cannot be made verified (or the
// binding cannot be described at all), UNSUPPORTED_CONTRACT when the model side cannot serve
// this binding in the strict contract.
func (d *Driver) ForkCount(ctx context.Context, threadID string, rev sqlite.ThreadRevisions, proj contextplan.Projection, source modelport.MeasureResult) (modelport.MeasureResult, error) {
	rs, code, err := d.countingState(ctx, threadID, rev, proj.ContextBlocks)
	switch {
	case err != nil:
		return modelport.MeasureResult{}, protocol.NewError(protocol.CodeBudgetUnverified, "the binding cannot be described, so the count of the copied projection cannot be checked").Wrap(err)
	case code == CodeIntegrityBlocked:
		return modelport.MeasureResult{}, protocol.NewError(protocol.CodeIntegrityBlocked, "the source thread's binding does not read back")
	case code != "":
		return modelport.MeasureResult{}, protocol.NewError(protocol.CodeUnsupportedContract, "the binding cannot be counted in the strict contract (%s)", code)
	}
	ports := &compactPorts{d: d, ctx: ctx, runCtx: ctx, rs: rs, dry: true}
	req, err := ports.actRequestOf(proj)
	if err != nil {
		return modelport.MeasureResult{}, protocol.NewError(protocol.CodeIntegrityBlocked, "the checkpoint to fork cannot be made into a prompt").Wrap(err)
	}
	digest, err := req.LogicalInputDigest()
	if err != nil {
		return modelport.MeasureResult{}, protocol.NewError(protocol.CodeUnsupportedContract, "the request of the copied projection cannot be identified").Wrap(err)
	}
	if source.InputDigest == digest && source.BindingFingerprint == rs.desc.BindingFingerprint && (source.State == "verified_exact" || source.State == "verified_bound") {
		return source, nil
	}
	c, _, err := ports.count(req)
	var un *compaction.UnavailableError
	switch {
	case errors.As(err, &un):
		return modelport.MeasureResult{}, protocol.NewError(protocol.CodeBudgetUnverified, "the copied projection could not be counted (%s)", un.Code)
	case err != nil:
		return modelport.MeasureResult{}, protocol.NewError(protocol.CodeBudgetUnverified, "the copied projection could not be counted").Wrap(err)
	case !compaction.VerifiedCount(c):
		return modelport.MeasureResult{}, protocol.NewError(protocol.CodeBudgetUnverified, "the count of the copied projection is not verified")
	}
	return c.Result, nil
}
