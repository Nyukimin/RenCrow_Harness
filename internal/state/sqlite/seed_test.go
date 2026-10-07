package sqlite

import (
	"testing"
)

// seedThread inserts a session and one thread directly, standing in for
// session/open, which is not part of this stage. The thread starts with
// writer_epoch 1, as if a driver had taken the thread's writer lock.
func seedThread(t testing.TB, s *Store, owner, sessionID, threadID string) {
	t.Helper()
	mustExec(t, s.db, "INSERT INTO sessions VALUES(?,?,?,?,?,?)", sessionID, owner, "2026-10-07T00:00:00Z", "/work", "fixture-workspace-write", "trusted_host")
	mustExec(t, s.db, `INSERT INTO threads(thread_id, session_id, binding_json, policy_revision, binding_revision, writer_epoch)
		VALUES(?,?,?,?,?,1)`, threadID, sessionID,
		`{"kind":"model_route","selector":"fixture-local","profile_revision":"fixture-v1","agent_id":null,"execution_role":null}`,
		"policy-rev-1", "binding-rev-1")
}
