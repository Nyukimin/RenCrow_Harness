package stdio

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

var (
	// ErrBackpressure is returned when the client does not read fast enough for
	// the connection to keep what it must deliver: a response or a confirmed event
	// could not be queued. The connection is closed; nothing durable is lost, the
	// client re-reads events with events/read.
	ErrBackpressure = errors.New("stdio: the client does not read fast enough; the connection was closed")
	// ErrDrainTimeout is returned when the output could not be flushed before the
	// deadline of a shutdown or of the end of input.
	ErrDrainTimeout = errors.New("stdio: the output was not flushed before the deadline")
	errClosed       = errors.New("stdio: the connection is closed")
)

// Limits bound what the outbound queue holds for a client that has stopped
// reading.
type Limits struct {
	// PriorityMessages and PriorityBytes bound the queue of responses and confirmed
	// events. It is never dropped from: if it is full the connection is closed.
	PriorityMessages int
	PriorityBytes    int
	// ProgressMessages bounds the queue of provisional progress notifications.
	ProgressMessages int
}

// outbound is the queue between the connection and its single writer. It has two
// classes:
//
//   - the priority class holds every response and every confirmed event, in the
//     order they were produced. Nothing is dropped from it; a put that does not fit
//     fails with ErrBackpressure and the connection is closed, because a confirmed
//     event must never be lost silently and a response that cannot be sent leaves
//     the client unable to tell what happened to its request.
//   - the progress class holds provisional progress. It is served only when the
//     priority class is empty, and it is the part that gives way: a delta that does
//     not fit, or that arrives while the priority class is half full, is not queued.
//     It becomes one merged progress/gap notice, queued in the position where the
//     first missing delta would have been, and the attempt's later deltas join that
//     notice until it has been sent.
//
// All frames are encoded by the producer; the writer only writes bytes.
type outbound struct {
	lim Limits

	mu        sync.Mutex
	cond      *sync.Cond
	prio      [][]byte
	prioBytes int
	prog      []progressItem
	pending   map[gapKey]*gapRecord
	writing   bool
	closed    bool
	err       error
	timedOut  bool
	failCh    chan struct{}
}

type gapKey struct{ run, attempt string }

type gapRecord struct {
	key      gapKey
	from, to int64
}

// progressItem is a queued frame, or the placeholder of a gap notice whose range is
// still growing.
type progressItem struct {
	frame []byte
	gap   *gapRecord
}

func newOutbound(l Limits) *outbound {
	o := &outbound{lim: l, pending: map[gapKey]*gapRecord{}, failCh: make(chan struct{})}
	o.cond = sync.NewCond(&o.mu)
	return o
}

// putPriority queues a response or a confirmed event.
func (o *outbound) putPriority(b []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return o.closedErr()
	}
	if len(o.prio) >= o.lim.PriorityMessages || o.prioBytes+len(b) > o.lim.PriorityBytes {
		return ErrBackpressure
	}
	o.prio = append(o.prio, b)
	o.prioBytes += len(b)
	o.cond.Broadcast()
	return nil
}

// putDelta queues a progress/delta frame, or records that it did not fit.
func (o *outbound) putDelta(d protocol.ProgressDelta, frame []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	key := gapKey{d.RunID, d.AttemptID}
	if g, ok := o.pending[key]; ok {
		g.from, g.to = min(g.from, d.Ordinal), max(g.to, d.Ordinal)
		return
	}
	if len(o.prog) >= o.lim.ProgressMessages || len(o.prio) >= o.lim.PriorityMessages/2 {
		g := &gapRecord{key: key, from: d.Ordinal, to: d.Ordinal}
		o.pending[key] = g
		o.prog = append(o.prog, progressItem{gap: g})
		o.cond.Broadcast()
		return
	}
	o.prog = append(o.prog, progressItem{frame: frame})
	o.cond.Broadcast()
}

// putReset queues a progress/reset frame. A reset is rare (one per retry) and tells
// the client to discard text, so it is not subject to the progress cap.
func (o *outbound) putReset(frame []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.prog = append(o.prog, progressItem{frame: frame})
	o.cond.Broadcast()
}

func (o *outbound) closedErr() error {
	if o.err != nil {
		return o.err
	}
	return errClosed
}

// next blocks until there is a frame to write and takes it, priority class first.
// It reports false once the queue is closed. While it returns a frame the writer
// counts as busy until done is called.
func (o *outbound) next() ([]byte, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for {
		if o.closed {
			return nil, false
		}
		if len(o.prio) > 0 {
			b := o.prio[0]
			o.prio[0] = nil
			o.prio = o.prio[1:]
			o.prioBytes -= len(b)
			o.writing = true
			return b, true
		}
		if len(o.prog) > 0 {
			it := o.prog[0]
			o.prog[0] = progressItem{}
			o.prog = o.prog[1:]
			o.writing = true
			if it.gap != nil {
				delete(o.pending, it.gap.key)
				return notificationFrame("progress/gap", protocol.ProgressGap{
					RunID: it.gap.key.run, AttemptID: it.gap.key.attempt, FromOrdinal: it.gap.from, ToOrdinal: it.gap.to,
				}), true
			}
			return it.frame, true
		}
		o.cond.Wait()
	}
}

// idle reports, under the lock, whether nothing is queued.
func (o *outbound) idleLocked() bool { return len(o.prio) == 0 && len(o.prog) == 0 }

func (o *outbound) done() {
	o.mu.Lock()
	o.writing = false
	o.cond.Broadcast()
	o.mu.Unlock()
}

// writeLoop is the one writer: it takes frames in order and writes them, one per
// line, flushing whenever the queue runs empty. It returns when the queue is
// closed or a write fails.
func (o *outbound) writeLoop(w io.Writer) error {
	bw := bufio.NewWriterSize(w, 64<<10)
	for {
		b, ok := o.next()
		if !ok {
			return o.failure()
		}
		_, err := bw.Write(b)
		if err == nil {
			err = bw.WriteByte('\n')
		}
		if err == nil {
			o.mu.Lock()
			empty := o.idleLocked()
			o.mu.Unlock()
			if empty {
				err = bw.Flush()
			}
		}
		if err != nil {
			o.fail(err)
			o.done()
			return err
		}
		o.done()
	}
}

// fail records why the connection ended and closes the queue. Only the first
// failure is kept.
func (o *outbound) fail(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err == nil {
		o.err = err
		close(o.failCh)
	}
	o.closed = true
	o.cond.Broadcast()
}

func (o *outbound) failure() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

// failed is closed when the connection has failed (a write error or an overflow).
func (o *outbound) failed() <-chan struct{} { return o.failCh }

// close ends the queue without waiting; whatever is still queued is discarded.
func (o *outbound) close() {
	o.mu.Lock()
	o.closed = true
	o.cond.Broadcast()
	o.mu.Unlock()
}

// drain waits until everything queued has been written and flushed, the
// connection failed, or d has passed. It reports whether the queue was emptied.
func (o *outbound) drain(d time.Duration) bool {
	timer := time.AfterFunc(d, func() {
		o.mu.Lock()
		o.timedOut = true
		o.cond.Broadcast()
		o.mu.Unlock()
	})
	defer timer.Stop()
	o.mu.Lock()
	defer o.mu.Unlock()
	for {
		if o.err != nil {
			return false
		}
		if o.idleLocked() && !o.writing {
			return true
		}
		if o.timedOut {
			return false
		}
		o.cond.Wait()
	}
}

// notificationFrame is a JSON-RPC notification: no id, never answered.
func notificationFrame(method string, params any) []byte {
	raw, err := json.Marshal(params)
	if err != nil {
		// The params are fixed DTO structs; an encoding failure is a programming
		// error and must not become a silently empty frame.
		panic("stdio: notification params cannot be encoded: " + err.Error())
	}
	return notificationFrameRaw(method, raw)
}

func notificationFrameRaw(method string, params []byte) []byte {
	out := make([]byte, 0, len(params)+64)
	out = append(out, `{"jsonrpc":"2.0","method":`...)
	m, _ := json.Marshal(method)
	out = append(out, m...)
	out = append(out, `,"params":`...)
	out = append(out, params...)
	return append(out, '}')
}

// notifier is the Service's handle on the connection: progress, and the confirmed
// events that no call returned. It satisfies service.Notifier.
type notifier struct{ o *outbound }

// Event queues one confirmed event of a Run's driver, in the priority class: like
// the events a response is followed by, it is never dropped. A queue that cannot take
// it ends the connection (ErrBackpressure), and the client re-reads with events/read.
func (n notifier) Event(ev protocol.Event) {
	body, err := protocol.Encode(ev)
	if err == nil {
		err = n.o.putPriority(notificationFrameRaw("event/recorded", body))
	}
	if err != nil {
		n.o.fail(err)
	}
}

func (n notifier) ProgressDelta(d protocol.ProgressDelta) {
	n.o.putDelta(d, notificationFrame("progress/delta", d))
}

func (n notifier) ProgressReset(r protocol.ProgressReset) {
	n.o.putReset(notificationFrame("progress/reset", r))
}
