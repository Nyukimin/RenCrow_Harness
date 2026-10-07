package sqlite

import (
	"errors"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func endsAsExpired(st StaleRun) (TerminalInput, error) {
	return TerminalInput{
		Result: protocol.RunResult{
			RunID: st.RunID, TaskID: st.TaskID, Status: "incomplete", Code: "DEADLINE_EXCEEDED",
			Verification: protocol.Verification{Status: "not_run", EvidenceIDs: []string{}}, EvidenceIDs: st.EvidenceIDs, UnresolvedActionIDs: []string{}, Resumable: true,
		},
		ResultEvidenceID: identity.NewEvidenceID().String(),
	}, nil
}

func TestTerminalizeExpiredEndsOnlyUndrivenRunsOfTheOwnEpochPastTheirDeadline(t *testing.T) {
	e := newEnv(t)
	r := e.newRun("run.key.00000000000020")

	// Not yet past its deadline: nothing happens.
	n := e.counts()
	if evs, err := e.s.TerminalizeExpired(bg, e.thread, 1, nil, nil, endsAsExpired); err != nil || len(evs) != 0 {
		t.Fatalf("%v %v", evs, err)
	}
	e.wantNoChange(n)

	mustExec(t, e.s.db, "UPDATE runs SET deadline_at='2026-10-07T11:59:59Z'") // the store's clock reads 12:00:00
	// A Run a driver holds belongs to that driver.
	if evs, err := e.s.TerminalizeExpired(bg, e.thread, 1, func(id string) bool { return id == r.start.RunID }, nil, endsAsExpired); err != nil || len(evs) != 0 {
		t.Fatalf("%v %v", evs, err)
	}
	e.wantNoChange(n)
	// The epoch named must be the Thread's.
	if _, err := e.s.TerminalizeExpired(bg, e.thread, 2, nil, nil, endsAsExpired); !errors.Is(err, ErrWriterLost) {
		t.Fatalf("%v", err)
	}

	evs, err := e.s.TerminalizeExpired(bg, e.thread, 1, func(string) bool { return false }, nil, endsAsExpired)
	if err != nil || len(evs) != 1 || evs[0].Type != "run.terminal" {
		t.Fatalf("%v %v", evs, err)
	}
	if v := scalar[string](t, e.s.db, "SELECT status||'/'||phase FROM runs"); v != "incomplete/Terminal" {
		t.Fatal(v)
	}
	if v := scalar[string](t, e.s.db, "SELECT COALESCE(active_run_id,'none') FROM threads"); v != "none" {
		t.Fatalf("the Thread must be free: %s", v)
	}
}
