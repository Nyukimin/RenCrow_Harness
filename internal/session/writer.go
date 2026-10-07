// Package session holds what a driver of a Thread owns: the writer role.
//
// The writer of a Thread is the one process that holds the Thread's OS advisory
// lock and has raised its writer_epoch in the store. The lock decides who may
// drive; the epoch lets every later write prove it is still the driver (a write
// compares the epoch it holds and a driver that was taken over finds out there).
// Neither a PID, a heartbeat nor an expiry decides anything: a lock another process
// holds is BUSY, however old the file looks, and the OS drops the lock of a process
// that has ended.
//
// Taking the role is also what settles the past. A Run that an earlier driver left
// running (a process that died between admitting it and ending it) would otherwise
// keep the Thread busy for ever, so the acquisition that raises the epoch ends every
// Run of an older epoch deterministically (kernel.RecoverStale) before it returns.
package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
	"github.com/Nyukimin/RenCrow_Harness/internal/kernel"
	"github.com/Nyukimin/RenCrow_Harness/internal/oslock"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Lease is this process's writer role for one Thread.
type Lease struct {
	ThreadID string
	// Epoch is the writer_epoch this process raised the Thread to. It is passed to
	// every admission, which refuses a stale one.
	Epoch int64

	lock *oslock.Lock

	mu        sync.Mutex
	recovered []protocol.Event
}

// TakeRecovered returns the events the acquisition committed when it settled the
// Runs of earlier epochs, once: they are announced by whoever takes them, and a later
// call (the lease is shared by every request for the Thread) gets none.
func (l *Lease) TakeRecovered() []protocol.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.recovered
	l.recovered = nil
	return out
}

// Writers hands out the writer role of Threads to one process and keeps it until
// the process closes it. Safe for concurrent use.
type Writers struct {
	store      *sqlite.Store
	reconciler kernel.StaleReconciler

	mu     sync.Mutex
	leases map[string]*Lease
	closed bool
}

// Option configures NewWriters.
type Option func(*Writers)

// WithReconciler makes the acquisition that settles a dead driver's Runs check the
// processes of their dispatched Tool calls against the host (and stop the ones that are
// still the same processes) before the Runs are settled. Without it nothing is
// checked: such calls are settled as unknown all the same.
func WithReconciler(r kernel.StaleReconciler) Option { return func(w *Writers) { w.reconciler = r } }

// NewWriters prepares writer acquisition on the store's data root. The locks
// directory must be owner-only; it is checked, never repaired.
func NewWriters(store *sqlite.Store, opts ...Option) (*Writers, error) {
	dir := filepath.Join(store.Root(), "locks")
	if err := fsperm.CheckOwnerOnlyDir(dir); err != nil {
		return nil, fmt.Errorf("session: locks directory: %w", fsperm.WithoutPath(err))
	}
	w := &Writers{store: store, leases: map[string]*Lease{}}
	for _, o := range opts {
		o(w)
	}
	return w, nil
}

// Acquire makes this process the driver of the Thread. The caller has already
// established that the Thread exists and that its caller may control it. A process
// that already drives the Thread gets its existing lease and no new epoch.
//
// The OS lock is taken first, without waiting, and only then is the epoch raised.
// If another process holds the lock the answer is BUSY (retryable) and nothing was
// changed; if the epoch cannot be raised, or the Runs of the older epochs cannot be
// settled, the lock is released again, so a failed acquisition leaves neither a held
// lock nor a half-taken role. (A raised epoch stays raised: the next acquisition
// raises it again and settles what is still there.)
func (w *Writers) Acquire(ctx context.Context, threadID string) (*Lease, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, protocol.NewError(protocol.CodeBusy, "the service is shutting down")
	}
	if l, ok := w.leases[threadID]; ok {
		return l, nil
	}
	path, err := sqlite.ThreadLockPath(w.store.Root(), threadID)
	if err != nil {
		return nil, protocol.NewError(protocol.CodeInvalidParams, "thread_id is not a thread ID").Wrap(err)
	}
	lock, err := oslock.TryLock(path, oslock.Exclusive)
	if errors.Is(err, oslock.ErrLocked) {
		return nil, protocol.NewError(protocol.CodeBusy, "another process drives this thread").AsRetryable().Wrap(err)
	}
	if err != nil {
		return nil, protocol.NewError(protocol.CodeInternal, "the thread's writer lock could not be taken").Wrap(err)
	}
	epoch, err := w.store.BumpWriterEpoch(ctx, threadID)
	if err != nil {
		_ = lock.Release()
		return nil, err
	}
	settled, err := kernel.RecoverStale(ctx, w.store, threadID, epoch, w.reconciler)
	if err != nil {
		_ = lock.Release()
		var pe *protocol.Error
		if errors.As(err, &pe) {
			return nil, err
		}
		return nil, protocol.NewError(protocol.CodeInternal, "the runs an earlier driver left could not be settled").Wrap(err)
	}
	l := &Lease{ThreadID: threadID, Epoch: epoch, lock: lock, recovered: settled}
	w.leases[threadID] = l
	return l, nil
}

// Close releases every lock this process holds and refuses further acquisition.
// It is idempotent.
func (w *Writers) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	var errs []error
	for id, l := range w.leases {
		errs = append(errs, l.lock.Release())
		delete(w.leases, id)
	}
	return errors.Join(errs...)
}
