package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// A synthetic relay key and issuer, as in the design vector. Never a real key.
const relayKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

var hostLimits = protocol.Limits{MaxModelSteps: 10, MaxToolCallsPerStep: 8, DeadlineSeconds: 1800, MaxCaptureBytes: 67108864, MaxGenerationAttempts: 32}

var testRecovery = protocol.RecoveryPolicy{
	ContractVersion: "act-recovery/v1", MaxAttemptsPerAct: 2, AllowedProfiles: []string{"same_request", "terminal_output_once"},
}

// designRecoveryRevision is recovery_policy_revision of the design example for that policy.
const designRecoveryRevision = "db87835a3c36bf3f845441203fc0a5f90baa4d42bb05a7298935bdbef0edf0e9"

// env is a store with one thread, a caller that owns it, and the admission
// parameters a Service would pass.
type env struct {
	t       testing.TB
	s       *Store
	root    string
	caller  intake.Caller
	adm     Admission
	key     protocol.OriginKey
	session string
	thread  string
}

func newEnv(t testing.TB) *env {
	t.Helper()
	s, root := newStore(t)
	key, err := protocol.ParseOriginKeyFile([]byte(relayKeyHex + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, s: s, root: root, key: key}
	e.caller = e.newCaller(func(c *intake.CallerConfig) {})
	e.adm = Admission{
		Caller: e.caller, Entrypoint: protocol.EntrypointStdioCore, WriterEpoch: 1, Recovery: testRecovery,
		Caps: func(string) (protocol.Limits, error) { return hostLimits, nil },
	}
	e.session, e.thread = e.newThread("core:local")
	return e
}

func (e *env) newCaller(mod func(c *intake.CallerConfig)) intake.Caller {
	cfg := intake.CallerConfig{
		Principal: "core:local", DefaultOrigin: protocol.OriginAutomation,
		Relays: []intake.RelayKey{{Issuer: "core:fixture", KeyID: "fixture-key-1", Audience: "core:local", Key: e.key}},
	}
	mod(&cfg)
	c, err := intake.NewCaller(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// newThread seeds a session owned by owner and one thread in it.
func (e *env) newThread(owner string) (session, thread string) {
	session, thread = identity.NewSessionID().String(), identity.NewThreadID().String()
	seedThread(e.t, e.s, owner, session, thread)
	return
}

func designFile(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "contract", "examples", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// params builds turn/start params from the design example (four context blocks and
// a CORE upstream), pointed at thread and key, then lets mod change them.
func (e *env) params(thread, key string, mod func(m map[string]any)) []byte {
	e.t.Helper()
	dec := json.NewDecoder(bytes.NewReader(designFile(e.t, "core_start.json")))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		e.t.Fatal(err)
	}
	m["thread_id"], m["idempotency_key"] = thread, key
	if mod != nil {
		mod(m)
	}
	out, err := json.Marshal(m)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) admit(thread, key string, mod func(m map[string]any)) (StartOutcome, error) {
	e.t.Helper()
	return e.s.AdmitStart(context.Background(), e.adm, e.params(thread, key, mod))
}

func (e *env) mustAdmit(thread, key string, mod func(m map[string]any)) StartOutcome {
	e.t.Helper()
	out, err := e.admit(thread, key, mod)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

var counted = []string{"sessions", "threads", "evidence", "evidence_chunks", "items", "turns", "tasks", "runs", "receipts", "events", "relay_nonces"}

// counts returns the row count of every table an admission touches.
func (e *env) counts() map[string]int {
	e.t.Helper()
	m := map[string]int{}
	for _, table := range counted {
		m[table] = scalar[int](e.t, e.s.db, "SELECT COUNT(*) FROM "+table)
	}
	return m
}

func (e *env) wantNoChange(before map[string]int) {
	e.t.Helper()
	after := e.counts()
	for table, n := range before {
		if after[table] != n {
			e.t.Errorf("table %s: %d rows, was %d", table, after[table], n)
		}
	}
}

func wantCode(t testing.TB, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted, want %s", code)
	}
	if got := protocol.CodeOf(err); got != code {
		t.Fatalf("code %q (%v), want %s", got, err, code)
	}
}

func queryString(t testing.TB, db *sql.DB, q string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRowContext(context.Background(), q, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s.String
}
