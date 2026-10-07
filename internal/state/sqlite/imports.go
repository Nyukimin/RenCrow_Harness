package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Provenance import (F25, CHECKPOINT_FORMAT section 2). A forked Thread's checkpoint names
// sources that belong to the Thread it was forked from: Evidence, and the checkpoint that
// accepted its Summary. The fork imports them explicitly, in the transaction that creates
// the Thread (source_imports): from then on the Thread may read, name and verify exactly
// those sources, and no other source of another Thread. Nothing is copied and nothing
// the source Thread did afterwards becomes readable.

const (
	importEvidence   = "evidence"
	importCheckpoint = "checkpoint"
)

// evidenceInScope reports whether an Evidence is the Thread's own (it is the text of one of
// its items, or the Evidence of one of its Runs) or one it imported when it was forked.
func evidenceInScope(ctx context.Context, q queryer, evidenceID, threadID string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM items WHERE evidence_id=? AND thread_id=?) +
		(SELECT COUNT(*) FROM evidence e JOIN runs r ON r.run_id=e.run_id JOIN tasks tk ON tk.task_id=r.task_id WHERE e.evidence_id=? AND tk.thread_id=?) +
		(SELECT COUNT(*) FROM source_imports WHERE thread_id=? AND source_kind='evidence' AND source_id=?)`,
		evidenceID, threadID, evidenceID, threadID, threadID, evidenceID).Scan(&n)
	if err != nil {
		return false, wrapRead("read evidence thread", err)
	}
	return n > 0, nil
}

// checkpointInScope reports whether a checkpoint is the Thread's own or one it imported.
func checkpointInScope(ctx context.Context, q queryer, checkpointID, threadID string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM checkpoints WHERE checkpoint_id=? AND thread_id=?) +
		(SELECT COUNT(*) FROM source_imports WHERE thread_id=? AND source_kind='checkpoint' AND source_id=?)`,
		checkpointID, threadID, threadID, checkpointID).Scan(&n)
	if err != nil {
		return false, wrapRead("read checkpoint thread", err)
	}
	return n > 0, nil
}

// ErrSchemaOutdated: the store was created by a build whose schema lacks an object this
// build needs (the draft DDL grew). It is refused at Open and never converted: the
// schema is a draft, and a store of it is re-initialized, not migrated.
var ErrSchemaOutdated = errors.New("sqlite: the store's schema lacks an object this build needs; initialize a new store")

// requiredObjects are the tables and triggers added to the draft DDL after its first
// version. Open refuses a store without them, so that a store never answers some questions
// from a schema that cannot hold the answer.
var requiredObjects = []struct{ kind, name string }{
	{"table", "source_imports"},
	{"trigger", "source_imports_no_update"},
	{"trigger", "source_imports_no_delete"},
	{"trigger", "context_entries_after_checkpoint"},
}

func (s *Store) verifyObjects(ctx context.Context) error {
	for _, o := range requiredObjects {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type=? AND name=?`, o.kind, o.name).Scan(&n); err != nil {
			return wrapRead("verify schema objects", err)
		}
		if n != 1 {
			return ErrSchemaOutdated
		}
	}
	return nil
}

// importSources records, in the transaction that creates a forked Thread, the sources its
// fork checkpoint imports from the Thread it was forked from.
func importSources(ctx context.Context, tx *sql.Tx, threadID, fromThreadID, fromCheckpointID, forkCheckpointID string, sources []ImportedSource) error {
	seen := map[ImportedSource]bool{}
	for _, src := range sources {
		if seen[src] {
			continue
		}
		seen[src] = true
		if src.Kind != importEvidence && src.Kind != importCheckpoint {
			return protocol.NewError(protocol.CodeInternal, "an import names a kind of source this build does not know")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_imports(thread_id, source_kind, source_id, from_thread_id, from_checkpoint_id, fork_checkpoint_id)
			VALUES(?,?,?,?,?,?)`, threadID, src.Kind, src.ID, fromThreadID, fromCheckpointID, forkCheckpointID); err != nil {
			return wrapRead("import source", err)
		}
	}
	return nil
}

// ImportedSource is one source a fork imports: the Evidence or the checkpoint, by ID.
type ImportedSource struct {
	Kind string
	ID   string
}

// ImportEvidence and ImportCheckpoint are the two kinds of source a fork imports.
func ImportEvidence(id string) ImportedSource { return ImportedSource{Kind: importEvidence, ID: id} }
func ImportCheckpoint(id string) ImportedSource {
	return ImportedSource{Kind: importCheckpoint, ID: id}
}
