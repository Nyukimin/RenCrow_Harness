package service

import (
	"context"

	"github.com/Nyukimin/RenCrow_Harness/internal/kernel"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// ContextCompact is context/compact (PROTOCOL section 6, IMPLEMENTATION_SPEC section 11), the
// manual compaction of an idle Thread. It accepts the request and returns: the result is read
// from the receipt it names (receipt/get), when its system Run has ended.
//
// The order is the one of turn/start. A request that was already accepted is answered from
// its receipt first, whatever has happened to the Thread since (the commit of the first
// answer moved the context revision, and a retried request is not refused for the revision it
// was made at). Then the caller must be allowed to control the Thread, and what the request
// needs must exist in this process: a model port (the count and the stages are asked of it)
// and a deployment that enables compaction; a process without them refuses with
// UNSUPPORTED_CONTRACT and writes nothing, as the capabilities say.
//
// A dry run only looks (see kernel.DriverDryRun) and stores one receipt. Otherwise this
// process takes the Thread's writer role (BUSY if another holds it; a newly taken role first
// settles what an earlier driver left running), the admission makes the system Task and Run
// (BUSY if the Thread has an active Run: a manual compaction is never queued behind one, and
// its expected revisions must be the Thread's), and a driver starts the Run once the
// acceptance has been delivered.
func (s *Service) ContextCompact(ctx context.Context, in protocol.CompactInput, params []byte) (protocol.OperationAccepted, []protocol.Event, error) {
	out, events, begin, err := s.contextCompact(ctx, in, params)
	if begin != nil {
		begin()
	}
	return out, events, err
}

func (s *Service) contextCompact(ctx context.Context, in protocol.CompactInput, params []byte) (protocol.OperationAccepted, []protocol.Event, func(), error) {
	if s.shuttingDown() {
		return protocol.OperationAccepted{}, nil, nil, errShuttingDown()
	}
	adm := s.admission(0)
	if prior, found, err := s.store.LookupCompact(ctx, adm, params); err != nil {
		return protocol.OperationAccepted{}, nil, nil, err
	} else if found {
		return prior.Result, nil, nil, nil
	}
	access, err := s.store.ThreadAccess(ctx, s.caller, in.ThreadID)
	if err != nil {
		return protocol.OperationAccepted{}, nil, nil, err
	}
	if !access.CanControl {
		return protocol.OperationAccepted{}, nil, nil, protocol.NewError(protocol.CodeForbidden, "the thread is not accessible to this caller")
	}
	if err := s.compactionAvailable(); err != nil {
		return protocol.OperationAccepted{}, nil, nil, err
	}
	if in.DryRun {
		out, err := s.compactDryRun(ctx, in, params)
		return out, nil, nil, err
	}
	if s.writers == nil {
		return protocol.OperationAccepted{}, nil, nil, protocol.NewError(protocol.CodeUnsupportedContract, "this service has no writer role and cannot admit work")
	}
	lease, settled, err := s.takeThread(ctx, in.ThreadID)
	if err != nil {
		return protocol.OperationAccepted{}, nil, nil, err
	}
	out, err := s.store.AdmitCompact(ctx, s.admission(lease.Epoch), params)
	if err != nil {
		s.publishEvents(settled) // committed, though the compaction was refused
		return protocol.OperationAccepted{}, nil, nil, err
	}
	events := append(settled, out.Events...)
	if out.Replayed || out.RunID == "" {
		return out.Result, events, nil, nil
	}
	return out.Result, events, s.driverStarter(kernel.Handle{RunID: out.RunID, ThreadID: in.ThreadID, Epoch: lease.Epoch}), nil
}

// compactionAvailable says whether this process can compact on demand: it has a model port,
// and the deployment enables compaction.
func (s *Service) compactionAvailable() error {
	switch {
	case s.driver == nil:
		return protocol.NewError(protocol.CodeUnsupportedContract, "this process has no model port, so it cannot count or compact a context")
	case !s.dep.Config.Compaction.Enabled:
		return protocol.NewError(protocol.CodeUnsupportedContract, "the deployment disables compaction (compaction.enabled=false)")
	}
	return nil
}

// compactDryRun answers a dry run: what the Thread's context would be compacted from, counted,
// with no generation and nothing stored but the receipt that holds the answer. The Thread
// may be busy; the answer is about the context at the revisions the request names, and a
// request made at others is REVISION_CONFLICT.
func (s *Service) compactDryRun(ctx context.Context, in protocol.CompactInput, params []byte) (protocol.OperationAccepted, error) {
	rev, err := s.store.ThreadRevisions(ctx, in.ThreadID)
	if err != nil {
		return protocol.OperationAccepted{}, err
	}
	if rev.ContextRevision != in.ExpectedContextRevision || rev.ControlRevision != in.ExpectedControlRevision {
		return protocol.OperationAccepted{}, protocol.NewError(protocol.CodeRevisionConflict, "the expected revisions are not the thread's current revisions")
	}
	snap, err := s.store.LoadThreadSnapshot(ctx, in.ThreadID)
	if err != nil {
		if protocol.CodeOf(err) == protocol.CodeIntegrityBlocked {
			return s.recordDryRun(ctx, params, protocol.CompactResult{Status: "unavailable", Error: &protocol.ErrorInfo{Code: kernel.CodeIntegrityBlocked,
				Message: "what is stored for the context contradicts itself"}})
		}
		return protocol.OperationAccepted{}, err
	}
	res := s.driver.DryRunCompact(ctx, in.ThreadID, rev, snap)
	if res.Status == "unavailable" && res.Error != nil && res.Error.Retryable {
		// The model side could not be reached, or the process was stopping: that is not an answer
		// about the context, and a receipt that holds it would give the same non-answer to every
		// retry of the key. Nothing is recorded, and the same request may be asked again.
		return protocol.OperationAccepted{}, protocol.NewError(protocol.CodeBudgetUnverified, "the context could not be counted now (%s); nothing was recorded, ask again", res.Error.Code).AsRetryable()
	}
	return s.recordDryRun(ctx, params, res)
}

func (s *Service) recordDryRun(ctx context.Context, params []byte, result protocol.CompactResult) (protocol.OperationAccepted, error) {
	out, err := s.store.AdmitCompactDryRun(ctx, s.admission(0), params, result)
	if err != nil {
		return protocol.OperationAccepted{}, err
	}
	return out.Result, nil
}
