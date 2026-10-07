package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

var (
	// ErrStale: what a checkpoint candidate was made against is no longer so (the
	// context revision or the current checkpoint moved). Nothing was written; a candidate
	// made from a new snapshot may be tried.
	ErrStale = errors.New("sqlite: the thread changed since the candidate's snapshot")
)

// CheckpointRow is a stored checkpoint: its bytes exactly as they were stored and the
// facts the row keeps beside them (the DB's own copies, which a loader compares with
// the parsed candidate).
type CheckpointRow struct {
	CheckpointID       string
	ThreadID           string
	RunID              string
	ParentCheckpointID *string
	Mode               string
	// ContextRevision is the Thread's context revision after the commit (the candidate's
	// expected revision plus one), ControlRevision the one the commit was made at.
	ContextRevision, ControlRevision  int64
	SemanticBoundary, DurableBoundary int64
	Blob                              []byte
	Hash                              string
	CountJSON                         string
}

// LoadCheckpoint reads a checkpoint and checks its bytes against its recorded hash. A
// checkpoint that is named and cannot be read back exactly is INTEGRITY_BLOCKED: it is
// never taken for no checkpoint.
func (s *Store) LoadCheckpoint(ctx context.Context, checkpointID string) (CheckpointRow, error) {
	return loadCheckpoint(ctx, s.db, checkpointID)
}

func loadCheckpoint(ctx context.Context, q queryer, checkpointID string) (CheckpointRow, error) {
	var r CheckpointRow
	var parent sql.NullString
	err := q.QueryRowContext(ctx, `SELECT checkpoint_id, thread_id, run_id, parent_checkpoint_id, mode, context_revision, control_revision,
		semantic_boundary, durable_boundary, candidate_bytes, candidate_hash, count_json FROM checkpoints WHERE checkpoint_id=?`, checkpointID).
		Scan(&r.CheckpointID, &r.ThreadID, &r.RunID, &parent, &r.Mode, &r.ContextRevision, &r.ControlRevision, &r.SemanticBoundary, &r.DurableBoundary, &r.Blob, &r.Hash, &r.CountJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return r, protocol.NewError(protocol.CodeIntegrityBlocked, "a checkpoint that is named does not exist")
	}
	if err != nil {
		return r, wrapRead("read checkpoint", err)
	}
	r.ParentCheckpointID = nullStr(parent)
	sum := sha256.Sum256(r.Blob)
	if hex.EncodeToString(sum[:]) != r.Hash {
		return r, protocol.NewError(protocol.CodeIntegrityBlocked, "a stored checkpoint does not match its recorded hash")
	}
	return r, nil
}

// SourceCheck is a stored source a candidate names, to be found again as it was: the
// Evidence (or checkpoint) it points at must exist, be what its hash says and be long enough
// for the range.
type SourceCheck struct {
	SourceID, RawHash, ProjectionVersion string
	End                                  uint64
}

// verifySources finds every named source again. A source that does not exist, is not sealed,
// has another hash, or is shorter than its range is INTEGRITY_BLOCKED. A source of the
// checkpoint kind (the previous Summary's) is a checkpoint of the Thread, by hash and length.
func verifySources(ctx context.Context, q queryer, threadID string, refs []SourceCheck) error {
	bad := func(what string) error { return protocol.NewError(protocol.CodeIntegrityBlocked, "%s", what) }
	seen := map[SourceCheck]bool{}
	for _, r := range refs {
		if seen[r] {
			continue
		}
		seen[r] = true
		if r.ProjectionVersion == "checkpoint/v1" {
			cp, err := loadCheckpoint(ctx, q, r.SourceID)
			if err != nil {
				return err
			}
			if cp.Hash != r.RawHash || uint64(len(cp.Blob)) < r.End {
				return bad("a source names a checkpoint that is not as it says")
			}
			if ok, err := checkpointInScope(ctx, q, r.SourceID, threadID); err != nil {
				return err
			} else if !ok {
				return bad("a source names a checkpoint of another Thread")
			}
			continue
		}
		var state string
		var total sql.NullInt64
		var raw, proj sql.NullString
		err := q.QueryRowContext(ctx, `SELECT state, total_bytes, raw_hash, projection_version FROM evidence WHERE evidence_id=?`, r.SourceID).Scan(&state, &total, &raw, &proj)
		if errors.Is(err, sql.ErrNoRows) {
			return bad("a source names an Evidence that does not exist")
		}
		if err != nil {
			return wrapRead("verify source", err)
		}
		if state != "sealed" || !raw.Valid || raw.String != r.RawHash || !proj.Valid || proj.String != r.ProjectionVersion || !total.Valid || uint64(total.Int64) < r.End {
			return bad("a source is not the sealed Evidence its reference names")
		}
		if ok, err := evidenceInScope(ctx, q, r.SourceID, threadID); err != nil {
			return err
		} else if !ok {
			return bad("a source is the Evidence of another Thread")
		}
	}
	return nil
}

// VerifySources is verifySources for a caller outside a transaction.
func (s *Store) VerifySources(ctx context.Context, threadID string, refs []SourceCheck) error {
	return verifySources(ctx, s.db, threadID, refs)
}

// CheckpointCommit is a validated candidate, ready to store (F14), with the revisions it was
// made against. The bytes are stored as they are: they are never re-serialized here.
type CheckpointCommit struct {
	CheckpointID       string
	ParentCheckpointID *string
	Mode               string
	Blob               []byte
	Hash               string
	// Expected are the Thread's revisions the candidate was made against.
	ExpectedContextRevision int64
	PolicyRevision          string
	BindingRevision         string
	SemanticBoundary        int64
	DurableBoundary         int64
	// CountJSON is the CJ1 of the two counts; the Evidence IDs are those of their records.
	CountJSON        string
	BeforeEvidenceID string
	AfterEvidenceID  string
	// Sources are every source the candidate names, found again inside the commit.
	Sources []SourceCheck
}

// CommitResult is what a commit made.
type CommitResult struct {
	Events             []protocol.Event
	NewContextRevision int64
}

// CommitCheckpoint (F14) stores a checkpoint and makes it the Thread's current one, in
// one short BEGIN IMMEDIATE transaction that is also the linearization point against a
// stop: the writer epoch, the Run (running, committing, in time) and the control revision
// are those of the driver's fence (a stop recorded first is ErrControlChanged and nothing
// is stored), and the Thread's context revision, binding and policy revisions and current
// checkpoint are exactly what the candidate was made against (anything else is ErrStale,
// ErrBindingChanged or ErrPolicyChanged and nothing is stored). Inputs that only wait in the
// queue do not move the context revision and do not make a candidate stale. The sources
// the candidate names are found again; the bytes' hash is computed again. The context
// revision moves by exactly one, the pointer and the checkpoint row and the
// checkpoint.committed event are written together, and no checkpoint row is ever updated.
func (s *Store) CommitCheckpoint(ctx context.Context, f Fence, in CheckpointCommit) (CommitResult, error) {
	var out CommitResult
	err := s.fencedWrite(ctx, f, true, func(tx *sql.Tx, now time.Time, th threadRow, run runRow, start startRecord) error {
		if run.phase != "CommittingCheckpoint" {
			return ErrPhaseConflict
		}
		if !now.Before(run.deadline) {
			return ErrDeadlinePassed
		}
		sum := sha256.Sum256(in.Blob)
		if hex.EncodeToString(sum[:]) != in.Hash {
			return protocol.NewError(protocol.CodeInternal, "the bytes of a checkpoint do not match its hash")
		}
		var binding, policy string
		var current sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT binding_revision, policy_revision, current_checkpoint_id FROM threads WHERE thread_id=?`, f.ThreadID).Scan(&binding, &policy, &current); err != nil {
			return fmt.Errorf("sqlite: read thread revisions: %w", mapSQLiteError(err))
		}
		switch {
		case policy != in.PolicyRevision:
			return ErrPolicyChanged
		case binding != in.BindingRevision:
			return ErrBindingChanged
		case th.contextRev != in.ExpectedContextRevision || (current.Valid != (in.ParentCheckpointID != nil)) || (current.Valid && current.String != *in.ParentCheckpointID):
			return ErrStale
		}
		if err := verifySources(ctx, tx, f.ThreadID, in.Sources); err != nil {
			return err
		}
		newRev := th.contextRev + 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoints(checkpoint_id, thread_id, run_id, parent_checkpoint_id, mode, context_revision, control_revision,
			semantic_boundary, durable_boundary, candidate_bytes, candidate_hash, count_json, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			in.CheckpointID, f.ThreadID, f.RunID, in.ParentCheckpointID, in.Mode, newRev, th.controlRev, in.SemanticBoundary, in.DurableBoundary, in.Blob, in.Hash, in.CountJSON,
			protocol.FormatTimestamp(now)); err != nil {
			return fmt.Errorf("sqlite: store checkpoint: %w", mapSQLiteError(err))
		}
		seq := &eventSeq{base: th.eventSeq}
		ev, err := appendEvent(ctx, tx, seq, eventRecord{common: runCommon(now, f.ThreadID, run, start, protocol.Str(in.AfterEvidenceID)), payload: protocol.CheckpointCommittedPayload{
			CheckpointID: in.CheckpointID, Mode: in.Mode, CandidateHash: in.Hash, ContextRevision: newRev, SemanticBoundary: in.SemanticBoundary, DurableBoundary: in.DurableBoundary,
			BeforeCountEvidenceID: in.BeforeEvidenceID, AfterCountEvidenceID: in.AfterEvidenceID}})
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE threads SET event_seq=?, context_revision=?, current_checkpoint_id=?
			WHERE thread_id=? AND writer_epoch=? AND context_revision=? AND control_revision=? AND event_seq=?
			AND ((current_checkpoint_id IS NULL AND ? IS NULL) OR current_checkpoint_id=?)`,
			seq.last(), newRev, in.CheckpointID, f.ThreadID, th.writerEpoch, th.contextRev, th.controlRev, th.eventSeq, in.ParentCheckpointID, in.ParentCheckpointID)
		if err != nil {
			return fmt.Errorf("sqlite: move checkpoint pointer: %w", mapSQLiteError(err))
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return protocol.NewError(protocol.CodeRevisionConflict, "the thread changed during a checkpoint commit")
		}
		if s.fault != nil {
			if ferr := s.fault("checkpoint.before_commit"); ferr != nil {
				return protocol.NewError(protocol.CodePersistenceUncertain, "the commit did not complete").AsRetryable().Wrap(ferr)
			}
		}
		out = CommitResult{Events: []protocol.Event{ev}, NewContextRevision: newRev}
		return nil
	})
	if err == nil && s.fault != nil {
		if ferr := s.fault("checkpoint.after_commit"); ferr != nil {
			return CommitResult{}, protocol.NewError(protocol.CodePersistenceUncertain, "the commit's answer was lost").AsRetryable().Wrap(ferr)
		}
	}
	return out, err
}

// CommitState is what a look at the store says about a commit whose answer was lost.
type CommitState int

const (
	// CommitAbsent: no checkpoint with the ID is stored. A commit is one transaction, so
	// the commit did not take effect.
	CommitAbsent CommitState = iota
	// CommitStored: the checkpoint is stored, with the hash that was asked about.
	CommitStored
)

// ResolveCheckpoint finds out whether a commit whose answer was lost took effect, by the
// checkpoint ID that was issued for it and the hash of its bytes: stored with that hash is
// the same commit; absent is a commit that did not happen; stored with another hash is a
// contradiction (INTEGRITY_BLOCKED). It asks the stored bytes, not another serializer. A
// store that cannot be read gives an error, and the caller says what it cannot say.
func (s *Store) ResolveCheckpoint(ctx context.Context, checkpointID, hash string) (CommitState, error) {
	var stored string
	err := s.db.QueryRowContext(ctx, `SELECT candidate_hash FROM checkpoints WHERE checkpoint_id=?`, checkpointID).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return CommitAbsent, nil
	case err != nil:
		return CommitAbsent, wrapRead("resolve checkpoint", err)
	case stored != hash:
		return CommitAbsent, protocol.NewError(protocol.CodeIntegrityBlocked, "a checkpoint with this ID is stored with other bytes")
	}
	return CommitStored, nil
}

// ReadEvidenceText is the full text of a sealed text Evidence of the Thread, checked
// against its recorded hash. An Evidence of another Thread, or one that is not text, is
// INTEGRITY_BLOCKED: what a compaction names as its source must be the Thread's own.
func (s *Store) ReadEvidenceText(ctx context.Context, threadID, evidenceID string) (string, error) {
	ok, err := s.evidenceInThread(ctx, evidenceID, threadID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", protocol.NewError(protocol.CodeIntegrityBlocked, "a source is the Evidence of another Thread")
	}
	return s.readEvidenceText(ctx, evidenceID)
}

// LoadScopedCheckpoint is LoadCheckpoint for a checkpoint the Thread may name: its own, or one
// it imported when it was forked. Any other is INTEGRITY_BLOCKED: what a checkpoint names as
// the source of its Summary must be in the Thread's own provenance.
func (s *Store) LoadScopedCheckpoint(ctx context.Context, threadID, checkpointID string) (CheckpointRow, error) {
	row, err := loadCheckpoint(ctx, s.db, checkpointID)
	if err != nil {
		return row, err
	}
	ok, err := checkpointInScope(ctx, s.db, checkpointID, threadID)
	if err != nil {
		return CheckpointRow{}, err
	}
	if !ok {
		return CheckpointRow{}, protocol.NewError(protocol.CodeIntegrityBlocked, "a checkpoint is named that is of another Thread")
	}
	return row, nil
}
