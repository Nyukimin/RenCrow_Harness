package service

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Nyukimin/RenCrow_Harness/internal/compaction"
	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// SessionFork is session/fork (F25, PROTOCOL section 6): a new Thread, in the source's Session,
// that stands on a copy of one of the source's checkpoints. The new work starts at the next
// turn/start on it; nothing else is copied: no active Run, no Tool state, no queued input,
// and the files are the workspace's as they are now (a fork does not restore them to what
// they were).
//
// The order is that of turn/start: a request already accepted is answered from its receipt
// first; then the caller must control the source Thread (a fork writes a Thread beside it,
// into its Session, so it is no wider than the source's own access). It is not refused
// because the source is busy: a checkpoint is immutable, and what a Run in flight holds is
// not in it. The checkpoint must be a checkpoint of that Thread and must read back exactly
// (INTEGRITY_BLOCKED otherwise, and no other checkpoint is looked at in its place), and the
// process needs a model port: the copied projection's count is held to the new Thread's
// binding, and a count that cannot be verified is not copied.
func (s *Service) SessionFork(ctx context.Context, in protocol.SessionForkInput, params []byte) (protocol.ForkResult, []protocol.Event, error) {
	if s.shuttingDown() {
		return protocol.ForkResult{}, nil, errShuttingDown()
	}
	adm := s.admission(0)
	if prior, found, err := s.store.LookupFork(ctx, adm, params); err != nil {
		return protocol.ForkResult{}, nil, err
	} else if found {
		return prior.Result, nil, nil
	}
	access, err := s.store.ThreadAccess(ctx, s.caller, in.ThreadID)
	if err != nil {
		return protocol.ForkResult{}, nil, err
	}
	if !access.CanControl {
		return protocol.ForkResult{}, nil, protocol.NewError(protocol.CodeForbidden, "the thread is not accessible to this caller")
	}
	if s.driver == nil {
		return protocol.ForkResult{}, nil, protocol.NewError(protocol.CodeUnsupportedContract, "this process has no model port, so it cannot count the copied context for the new thread")
	}
	src, err := s.store.LoadForkSource(ctx, s.caller, in.ThreadID, in.CheckpointID)
	if err != nil {
		return protocol.ForkResult{}, nil, err
	}
	cp, err := compaction.ParseCheckpoint(src.Checkpoint.Blob, src.Checkpoint.Hash)
	if err == nil {
		row := src.Checkpoint
		err = cp.CheckStored(compaction.StoredFacts{CheckpointID: row.CheckpointID, ThreadID: row.ThreadID, RunID: row.RunID, ParentCheckpointID: row.ParentCheckpointID,
			Mode: row.Mode, ContextRevision: row.ContextRevision, SemanticBoundary: row.SemanticBoundary, DurableBoundary: row.DurableBoundary, Hash: row.Hash})
	}
	if err != nil {
		return protocol.ForkResult{}, nil, protocol.NewError(protocol.CodeIntegrityBlocked, "the checkpoint to fork cannot be read back exactly")
	}

	// Every source the checkpoint names is the source Thread's (its own, or one it imported):
	// they are verified again inside the transaction, and imported by the new Thread.
	checks, imports, err := s.forkSources(ctx, in.ThreadID, cp)
	if err != nil {
		return protocol.ForkResult{}, nil, err
	}

	rev, err := s.store.ThreadRevisions(ctx, in.ThreadID)
	if err != nil {
		return protocol.ForkResult{}, nil, err
	}
	after, err := s.driver.ForkCount(ctx, in.ThreadID, rev, cp.Candidate.Projection, cp.Candidate.AfterCount)
	if err != nil {
		return protocol.ForkResult{}, nil, err
	}
	commit := sqlite.ForkCommit{
		ThreadID: identity.NewThreadID().String(), TaskID: identity.NewTaskID().String(), RunID: identity.NewRunID().String(), CheckpointID: identity.NewCheckpointID().String(),
		SourceThreadID: in.ThreadID, SourceCheckpointID: in.CheckpointID, SourceBindingJSON: src.BindingJSON,
		SourcePolicyRevision: src.PolicyRevision, SourceBindingRevision: src.BindingRevision,
		SemanticBoundary: cp.Candidate.SemanticBoundary, DurableBoundary: cp.Candidate.DurableBoundary, Sources: checks, Imports: imports,
	}
	fc, err := compaction.BuildFork(compaction.ForkInput{
		Source: cp, CheckpointID: commit.CheckpointID, ThreadID: commit.ThreadID, TaskID: commit.TaskID, RunID: commit.RunID,
		Expected: compaction.Expected{ContextRevision: 0, ControlRevision: 0, WriterEpoch: 1, PolicyRevision: src.PolicyRevision, BindingRevision: src.BindingRevision},
		After:    after,
	})
	if err != nil {
		switch {
		case errors.Is(err, compaction.ErrNoFit):
			return protocol.ForkResult{}, nil, protocol.NewError(protocol.CodeInvalidRequest, "the checkpoint's context does not fit the binding of the new thread")
		case errors.Is(err, compaction.ErrUnverified):
			return protocol.ForkResult{}, nil, protocol.NewError(protocol.CodeBudgetUnverified, "the count of the copied context is not verified")
		}
		return protocol.ForkResult{}, nil, protocol.NewError(protocol.CodeIntegrityBlocked, "the checkpoint to fork cannot be copied").Wrap(err)
	}
	commit.Blob, commit.Hash = fc.Bytes, fc.Hash
	if commit.BeforeCountJSON, commit.AfterCountJSON, commit.CountJSON, err = countsJSON(fc.Candidate); err != nil {
		return protocol.ForkResult{}, nil, protocol.NewError(protocol.CodeInternal, "the counts of a fork cannot be recorded").Wrap(err)
	}
	out, err := s.store.AdmitFork(ctx, adm, params, commit)
	if err != nil {
		return protocol.ForkResult{}, nil, err
	}
	return out.Result, out.Events, nil
}

// forkSources lists the sources a fork checkpoint names, to be found again as they were in the
// source Thread's own provenance, and to be imported. A checkpoint that the Summary names as
// its source is found by identity (it must be one the source Thread may name) and then by the
// hash it has.
func (s *Service) forkSources(ctx context.Context, threadID string, cp *compaction.Checkpoint) ([]sqlite.SourceCheck, []sqlite.ImportedSource, error) {
	var checks []sqlite.SourceCheck
	var imports []sqlite.ImportedSource
	for _, n := range cp.Candidate.Sources() {
		switch {
		case !n.Checkpoint:
			checks = append(checks, sqlite.SourceCheck{SourceID: n.ID, RawHash: n.RawHash, ProjectionVersion: n.ProjectionVersion, End: n.End})
			imports = append(imports, sqlite.ImportEvidence(n.ID))
		default:
			row, err := s.store.LoadScopedCheckpoint(ctx, threadID, n.ID)
			if err != nil {
				return nil, nil, err
			}
			if n.RawHash != "" && n.RawHash != row.Hash {
				return nil, nil, protocol.NewError(protocol.CodeIntegrityBlocked, "a source names a checkpoint that is not as it says")
			}
			checks = append(checks, sqlite.SourceCheck{SourceID: n.ID, RawHash: row.Hash, ProjectionVersion: "checkpoint/v1", End: uint64(len(row.Blob))})
			imports = append(imports, sqlite.ImportCheckpoint(n.ID))
		}
	}
	return checks, imports, nil
}

// countsJSON is the CJ1 of the two counts a fork checkpoint holds, each alone (the Evidence of
// the maintenance Run) and together (the row's count_json, in the form a compaction's is).
func countsJSON(c compaction.Candidate) (before, after, both string, err error) {
	cj := func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		out, err := protocol.EncodeCanonicalContract(raw)
		return string(out), err
	}
	if before, err = cj(c.BeforeCount); err != nil {
		return
	}
	if after, err = cj(c.AfterCount); err != nil {
		return
	}
	both, err = cj(struct {
		Before any `json:"before"`
		After  any `json:"after"`
	}{c.BeforeCount, c.AfterCount})
	return
}
