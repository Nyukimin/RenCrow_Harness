package client

import (
	"sync"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// NotificationKind says which member of a Notification is set.
type NotificationKind string

const (
	// NotificationEvent is a confirmed Event (event/recorded). It is announced after the
	// commit that made it; a duplicate delivery of the same event_id is removed by the
	// client.
	NotificationEvent NotificationKind = "event"
	// NotificationProgressDelta is provisional text (progress/delta). It is not durable
	// and has no event_seq: the final text is read with RunGet.
	NotificationProgressDelta NotificationKind = "progress_delta"
	// NotificationProgressReset tells the consumer to discard the provisional text of a
	// failed Attempt (progress/reset).
	NotificationProgressReset NotificationKind = "progress_reset"
	// NotificationProgressGap says provisional deltas were missed (progress/gap): read
	// the final text with RunGet.
	NotificationProgressGap NotificationKind = "progress_gap"
)

// Notification is one notification of the Harness, in the order it arrived. Exactly one
// of Event, Delta, Reset and Gap is set, as Kind says.
type Notification struct {
	Kind  NotificationKind
	Event *protocol.Event
	Delta *protocol.ProgressDelta
	Reset *protocol.ProgressReset
	Gap   *protocol.ProgressGap
	// LocalGap is true for a NotificationProgressGap the client made itself, because the
	// consumer did not keep up and deltas were not kept; the Harness did not send it.
	LocalGap bool
}

// NotificationLimits bound what the client holds for a consumer that does not keep up.
// A zero field means its default.
type NotificationLimits struct {
	// MaxEvents and MaxEventBytes bound the queued confirmed events (and the resets and
	// gaps the Harness sent), which are never dropped: past either limit the connection
	// ends with ErrNotificationOverflow. Defaults 4096 and 64 MiB.
	MaxEvents     int
	MaxEventBytes int
	// MaxProgress bounds the queued provisional deltas. Past it deltas are not kept; they
	// become one NotificationProgressGap (LocalGap) in the place of the first one missed.
	// Default 1024.
	MaxProgress int
}

func (l NotificationLimits) withDefaults() NotificationLimits {
	if l.MaxEvents <= 0 {
		l.MaxEvents = 4096
	}
	if l.MaxEventBytes <= 0 {
		l.MaxEventBytes = 64 << 20
	}
	if l.MaxProgress <= 0 {
		l.MaxProgress = 1024
	}
	return l
}

type gapKey struct{ run, attempt string }

type gapRecord struct {
	key      gapKey
	from, to int64
}

// queued is one place of the queue: a notification, or the placeholder of a local gap
// whose range may still grow.
type queued struct {
	n    Notification
	size int
	gap  *gapRecord
	prog bool // counts against MaxProgress
}

// notifyQueue is the FIFO between the reader of the connection and the consumer. It
// keeps the order of arrival across kinds. The reader never waits on it: a put either
// queues, or merges a delta into a gap, or reports overflow.
type notifyQueue struct {
	lim NotificationLimits

	mu         sync.Mutex
	cond       *sync.Cond
	items      []queued
	events     int
	eventBytes int
	progress   int
	pending    map[gapKey]*gapRecord
	closed     bool // no more puts; what is queued is still delivered
	discarded  bool // what is queued is dropped
}

func newNotifyQueue(lim NotificationLimits) *notifyQueue {
	q := &notifyQueue{lim: lim.withDefaults(), pending: map[gapKey]*gapRecord{}}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// putConfirmed queues an event, a reset or a gap the Harness sent. It reports false when
// the queue cannot keep it, which ends the connection: it is never dropped silently.
func (q *notifyQueue) putConfirmed(n Notification, size int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return true
	}
	if q.events >= q.lim.MaxEvents || q.eventBytes+size > q.lim.MaxEventBytes {
		return false
	}
	q.items = append(q.items, queued{n: n, size: size})
	q.events++
	q.eventBytes += size
	q.cond.Broadcast()
	return true
}

// putDelta queues a provisional delta, or records that it was not kept.
func (q *notifyQueue) putDelta(d protocol.ProgressDelta) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	key := gapKey{d.RunID, d.AttemptID}
	if g, ok := q.pending[key]; ok {
		g.from, g.to = min(g.from, d.Ordinal), max(g.to, d.Ordinal)
		return
	}
	if q.progress >= q.lim.MaxProgress {
		g := &gapRecord{key: key, from: d.Ordinal, to: d.Ordinal}
		q.pending[key] = g
		q.items = append(q.items, queued{gap: g})
		q.cond.Broadcast()
		return
	}
	q.items = append(q.items, queued{n: Notification{Kind: NotificationProgressDelta, Delta: &d}, prog: true})
	q.progress++
	q.cond.Broadcast()
}

// close ends the intake. What is queued is still delivered by next.
func (q *notifyQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// discard ends the intake and drops what is queued.
func (q *notifyQueue) discard() {
	q.mu.Lock()
	q.closed, q.discarded = true, true
	q.items = nil
	q.cond.Broadcast()
	q.mu.Unlock()
}

// next blocks until there is a notification and takes it. It reports false once the
// queue is closed and empty (or discarded).
func (q *notifyQueue) next() (Notification, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if q.discarded {
			return Notification{}, false
		}
		if len(q.items) > 0 {
			it := q.items[0]
			q.items[0] = queued{}
			q.items = q.items[1:]
			switch {
			case it.gap != nil:
				delete(q.pending, it.gap.key)
				return Notification{Kind: NotificationProgressGap, LocalGap: true, Gap: &protocol.ProgressGap{
					RunID: it.gap.key.run, AttemptID: it.gap.key.attempt, FromOrdinal: it.gap.from, ToOrdinal: it.gap.to,
				}}, true
			case it.prog:
				q.progress--
			default:
				q.events--
				q.eventBytes -= it.size
			}
			return it.n, true
		}
		if q.closed {
			return Notification{}, false
		}
		q.cond.Wait()
	}
}
