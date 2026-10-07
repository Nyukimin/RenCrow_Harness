package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/internal/oslock"
	"github.com/Nyukimin/RenCrow_Harness/internal/session"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// rig is a store with one Thread, opened twice: each opening and its Writers stand
// for one process driving the same data root.
type rig struct {
	t      *testing.T
	root   string
	thread string
	caller intake.Caller
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root := filepath.Join(t.TempDir(), "data")
	if err := sqlite.Init(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	caller, err := intake.NewCaller(intake.CallerConfig{Principal: "user:ren", DefaultOrigin: protocol.OriginHuman})
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, root: real, caller: caller}
	s := r.open()
	work := filepath.Join(t.TempDir(), "work")
	params, err := json.Marshal(protocol.SessionOpenInput{
		WorkspacePath: work, Binding: protocol.Binding{Kind: "model_route", Selector: "s", ProfileRevision: "r"},
		PolicyRef: "p", ExecutionMode: "structured_only", IdempotencyKey: "writer.test.key.0001",
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.OpenSession(context.Background(), sqlite.SessionAdmission{Caller: caller, Resolve: func(in protocol.SessionOpenInput) (sqlite.SessionPlan, error) {
		return sqlite.SessionPlan{WorkspacePath: work, PolicyRef: in.PolicyRef, ExecutionMode: in.ExecutionMode, PolicyRevision: "rev"}, nil
	}}, params)
	if err != nil {
		t.Fatal(err)
	}
	r.thread = out.Result.Session.ThreadID
	return r
}

// open opens the store as a new "process" and returns its Writers.
func (r *rig) open() *sqlite.Store {
	r.t.Helper()
	s, err := sqlite.Open(context.Background(), r.root, sqlite.Options{})
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = s.Close() })
	return s
}

func (r *rig) writers() *session.Writers {
	r.t.Helper()
	w, err := session.NewWriters(r.open())
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = w.Close() })
	return w
}

func epochOf(t *testing.T, s *sqlite.Store, thread string) int64 {
	t.Helper()
	n, err := s.BumpWriterEpoch(context.Background(), thread)
	if err != nil {
		t.Fatal(err)
	}
	return n - 1 // the value before this probe's own bump
}

func TestAcquireTakesTheLockAndBumpsTheEpochOnce(t *testing.T) {
	r := newRig(t)
	w := r.writers()
	ctx := context.Background()
	a, err := w.Acquire(ctx, r.thread)
	if err != nil || a.Epoch != 1 || a.ThreadID != r.thread {
		t.Fatalf("%v %+v", err, a)
	}
	// The same process asking again is the same driver: same lease, no new epoch.
	b, err := w.Acquire(ctx, r.thread)
	if err != nil || b != a || b.Epoch != 1 {
		t.Fatalf("%v %+v", err, b)
	}
	// The lock is really held at the OS level.
	path, _ := sqlite.ThreadLockPath(r.root, r.thread)
	if _, err := oslock.TryLock(path, oslock.Exclusive); !errors.Is(err, oslock.ErrLocked) {
		t.Fatalf("the OS lock is not held: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("lock file %v %v", info, err)
	}
}

func TestASecondDriverIsBusyAndDoesNotTouchTheEpoch(t *testing.T) {
	r := newRig(t)
	first := r.writers()
	if _, err := first.Acquire(context.Background(), r.thread); err != nil {
		t.Fatal(err)
	}
	other := r.open()
	second, err := session.NewWriters(other)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	_, err = second.Acquire(context.Background(), r.thread)
	if protocol.CodeOf(err) != protocol.CodeBusy {
		t.Fatalf("a second driver: %v", err)
	}
	var pe *protocol.Error
	if !errors.As(err, &pe) || !pe.Retryable {
		t.Fatalf("BUSY from a held lock is retryable: %v", err)
	}
	// Nothing was bumped by the refused driver: the next real bump is epoch 2.
	if got := epochOf(t, other, r.thread); got != 1 {
		t.Fatalf("epoch before the probe was %d, want 1", got)
	}
}

func TestTheLockIsReleasedWithTheWritersAndTheOldEpochIsStale(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	first := r.writers()
	old, err := first.Acquire(ctx, r.thread)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close is idempotent: %v", err)
	}
	if _, err := first.Acquire(ctx, r.thread); protocol.CodeOf(err) != protocol.CodeBusy {
		t.Fatalf("a closed Writers must not drive: %v", err)
	}
	second := r.writers()
	fresh, err := second.Acquire(ctx, r.thread)
	if err != nil || fresh.Epoch != old.Epoch+1 {
		t.Fatalf("%v %+v (old %+v)", err, fresh, old)
	}
}

func TestAnExpiredFileNeverHandsOverTheLock(t *testing.T) {
	r := newRig(t)
	first := r.writers()
	if _, err := first.Acquire(context.Background(), r.thread); err != nil {
		t.Fatal(err)
	}
	path, _ := sqlite.ThreadLockPath(r.root, r.thread)
	// Whatever the file says (an ancient time, another PID), only the OS lock counts.
	if err := os.WriteFile(path, []byte("pid=999999 expires=1970-01-01T00:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := r.writers()
	if _, err := second.Acquire(context.Background(), r.thread); protocol.CodeOf(err) != protocol.CodeBusy {
		t.Fatalf("the lock was taken over because of the file's content: %v", err)
	}
}

func TestAFailedEpochBumpReleasesTheLock(t *testing.T) {
	r := newRig(t)
	w := r.writers()
	ghost := identity.NewThreadID().String()
	if _, err := w.Acquire(context.Background(), ghost); err == nil {
		t.Fatal("a Thread that does not exist was acquired")
	}
	path, _ := sqlite.ThreadLockPath(r.root, ghost)
	l, err := oslock.TryLock(path, oslock.Exclusive)
	if err != nil {
		t.Fatalf("the failed acquisition left the lock held: %v", err)
	}
	_ = l.Release()
}

func TestAcquireRefusesAMalformedThreadIDBeforeTouchingTheFilesystem(t *testing.T) {
	r := newRig(t)
	w := r.writers()
	before, _ := os.ReadDir(filepath.Join(r.root, "locks"))
	for _, bad := range []string{"", "../../etc/passwd", "thr_x", "run_00000000-0000-7000-8000-000000000001"} {
		if _, err := w.Acquire(context.Background(), bad); err == nil {
			t.Errorf("Acquire(%q) succeeded", bad)
		}
	}
	after, _ := os.ReadDir(filepath.Join(r.root, "locks"))
	if len(after) != len(before) {
		t.Fatalf("a refused Acquire created files: %d -> %d", len(before), len(after))
	}
}

func TestNewWritersRefusesALocksDirectoryThatIsNotPrivate(t *testing.T) {
	r := newRig(t)
	dir := filepath.Join(r.root, "locks")
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := r.open() // the data root itself is still private
	if _, err := session.NewWriters(s); !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		t.Fatalf("%v", err)
	}
	_ = os.Chmod(dir, 0o700)
}
