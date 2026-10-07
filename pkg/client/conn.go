package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// dedupeWindow is how many recent event_ids the client remembers to drop a duplicate
// delivery (PROTOCOL section 7 allows one).
const dedupeWindow = 8192

// errPeerClosed is the cause of a connection whose output ended. Whether that was a
// clean end is decided from the exit of the child.
var errPeerClosed = errors.New("the Harness closed its output")

// reply is what the reader hands to the call a response answers.
type reply struct {
	result json.RawMessage
	rpcErr *RemoteError
	err    error // the connection ended first
}

// pendingCall is one request waiting for its response.
type pendingCall struct {
	ch   chan reply // buffered: the reader never waits for the caller
	sent atomic.Bool
}

// deliver hands the call its one answer. The first answer wins and a second is dropped:
// a response and the end of the connection can race to answer the same call, and neither
// may wait for a caller that has left.
func (p *pendingCall) deliver(rep reply) {
	select {
	case p.ch <- rep:
	default:
	}
}

// outFrame is one request handed to the writer.
type outFrame struct {
	data []byte
	call *pendingCall
}

// conn is the JSON-RPC 2.0 / NDJSON connection to one Harness: a reader, a writer, the
// requests waiting for a response, and the notifications. It does not know about the
// child process; onFail is how it asks for the child to be stopped when the connection
// cannot continue.
type conn struct {
	r io.Reader
	w io.WriteCloser

	queue  *notifyQueue
	ignore bool
	onFail func(cause error)

	out        chan *outFrame
	done       chan struct{} // closed once the connection is over
	readerDone chan struct{}

	mu       sync.Mutex
	pending  map[string]*pendingCall
	waiters  map[string]map[chan struct{}]struct{}
	failErr  error
	stopping bool
	seen     map[string]struct{}
	seenRing []string
	seenAt   int
}

type connOptions struct {
	limits NotificationLimits
	ignore bool
	onFail func(cause error)
}

func newConn(r io.Reader, w io.WriteCloser, o connOptions) *conn {
	return &conn{
		r: r, w: w,
		queue: newNotifyQueue(o.limits), ignore: o.ignore, onFail: o.onFail,
		out: make(chan *outFrame), done: make(chan struct{}), readerDone: make(chan struct{}),
		pending: map[string]*pendingCall{}, waiters: map[string]map[chan struct{}]struct{}{},
		seen: map[string]struct{}{}, seenRing: make([]string, dedupeWindow),
	}
}

// start runs the reader and the writer.
func (c *conn) start() {
	go c.readLoop()
	go c.writeLoop()
}

// fail ends the connection with cause. The first cause wins; later calls do nothing.
// In this order: the cause is recorded and the connection marked over; onFail is told so
// that the child is stopped (and so that its owner knows the cause before the child can
// be seen to exit); the input to the child is closed, so that nothing is written after
// the calls are answered; then every waiting call is answered with the cause. The
// notification queue is closed by the reader when it ends, so that what the Harness sent
// before is still delivered.
func (c *conn) fail(cause error) {
	c.mu.Lock()
	if c.failErr != nil {
		c.mu.Unlock()
		return
	}
	c.failErr = cause
	pending := c.pending
	c.pending = map[string]*pendingCall{}
	waiters := c.waiters
	c.waiters = map[string]map[chan struct{}]struct{}{}
	close(c.done)
	c.mu.Unlock()

	if c.onFail != nil {
		c.onFail(cause)
	}
	_ = c.w.Close()
	for _, cl := range pending {
		cl.deliver(reply{err: cause})
	}
	for _, set := range waiters {
		for ch := range set {
			wake(ch)
		}
	}
}

func (c *conn) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failErr
}

func (c *conn) protocolFail(format string, args ...any) {
	c.fail(fmt.Errorf("%w: %s", ErrProtocol, fmt.Sprintf(format, args...)))
}

// closedError is the error of a call that finds the connection over.
func closedError(cause error) error {
	return fmt.Errorf("%w: %w", ErrClosed, cause)
}

// beginStopping makes every later call refuse with ErrStopping (the internal ones, such
// as the shutdown request itself, excepted).
func (c *conn) beginStopping() {
	c.mu.Lock()
	c.stopping = true
	c.mu.Unlock()
}

func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("client: no request ID could be generated: %w", err)
	}
	// UUIDv7: 48 bits of Unix milliseconds, then random bits; version 7, RFC 4122 variant.
	ms := uint64(time.Now().UnixMilli())
	b[0], b[1], b[2], b[3], b[4], b[5] = byte(ms>>40), byte(ms>>32), byte(ms>>24), byte(ms>>16), byte(ms>>8), byte(ms)
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	var h [32]byte
	hex.Encode(h[:], b[:])
	return "req_" + string(h[0:8]) + "-" + string(h[8:12]) + "-" + string(h[12:16]) + "-" + string(h[16:20]) + "-" + string(h[20:32]), nil
}

func buildFrame(id, method string, params []byte) []byte {
	b := make([]byte, 0, len(params)+len(method)+len(id)+64)
	b = append(b, `{"jsonrpc":"2.0","id":"`...)
	b = append(b, id...)
	b = append(b, `","method":"`...)
	b = append(b, method...)
	b = append(b, `","params":`...)
	b = append(b, params...)
	return append(b, "}\n"...)
}

// do sends one request and waits for its response. params is the canonical JSON of the
// params. internal calls (the shutdown request) are allowed while the client is
// stopping.
//
// Where ctx ends decides what is said: before the request was handed to the writer it
// was not sent, and the error is ctx's alone; after, it may be on the wire, and the
// error is also ErrOutcomeUnknown. A response that comes later is read and dropped.
func (c *conn) do(ctx context.Context, method string, params []byte, internal bool) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := newRequestID()
	if err != nil {
		return nil, err
	}
	frame := buildFrame(id, method, params)
	if len(frame)-1 > maxFrameBytes {
		return nil, ErrRequestTooLarge
	}
	cl := &pendingCall{ch: make(chan reply, 1)}
	c.mu.Lock()
	switch {
	case c.failErr != nil:
		cause := c.failErr
		c.mu.Unlock()
		return nil, closedError(cause)
	case c.stopping && !internal:
		c.mu.Unlock()
		return nil, ErrStopping
	}
	c.pending[id] = cl
	c.mu.Unlock()

	select {
	case c.out <- &outFrame{data: frame, call: cl}:
	case <-ctx.Done():
		c.forget(id)
		return nil, ctx.Err()
	case <-c.done:
		c.forget(id)
		return nil, closedError(c.failure())
	}
	answer := func(rep reply) (json.RawMessage, error) {
		switch {
		case rep.err != nil:
			if cl.sent.Load() {
				return nil, outcomeUnknown(closedError(rep.err))
			}
			return nil, closedError(rep.err)
		case rep.rpcErr != nil:
			return nil, rep.rpcErr
		}
		return rep.result, nil
	}
	select {
	case rep := <-cl.ch:
		return answer(rep)
	case <-ctx.Done():
		// An answer that is already there wins over the end of the context.
		select {
		case rep := <-cl.ch:
			return answer(rep)
		default:
		}
		// The entry stays, so that the late response is read and dropped.
		return nil, outcomeUnknown(ctx.Err())
	}
}

func (c *conn) forget(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// writeLoop is the one writer: it writes the requests it is handed, one line each.
func (c *conn) writeLoop() {
	for {
		select {
		case f := <-c.out:
			select {
			case <-c.done:
				continue // the connection is over; the call was answered with that
			default:
			}
			f.call.sent.Store(true)
			if _, err := c.w.Write(f.data); err != nil {
				c.fail(fmt.Errorf("the request could not be written: %w", err))
			}
		case <-c.done:
			return
		}
	}
}

// readLoop is the one reader. It never waits for a consumer: a response goes to a
// buffered channel, a notification to the queue.
func (c *conn) readLoop() {
	defer close(c.readerDone)
	defer c.queue.close()
	br := bufio.NewReaderSize(c.r, 64<<10)
	for {
		frame, err := readFrame(br, maxFrameBytes)
		if err != nil {
			switch {
			case errors.Is(err, io.EOF), errors.Is(err, os.ErrClosed):
				// The Harness closed its output, or this client closed the read end because the
				// child was gone and something else held the pipe open.
				c.fail(errPeerClosed)
			case errors.Is(err, errFrameTooLong):
				c.protocolFail("a frame is over the size limit")
			default:
				c.fail(fmt.Errorf("the output could not be read: %w", err))
			}
			return
		}
		if len(bytes.TrimSpace(frame)) == 0 {
			continue
		}
		env, err := parseEnvelope(frame)
		if err != nil {
			c.protocolFail("%v", err)
			return
		}
		switch {
		case env.hasMeth && !env.hasID:
			if !c.notification(env) {
				return
			}
		case env.hasID && !env.hasMeth:
			if !c.response(env) {
				return
			}
		default:
			c.protocolFail("a message that is neither a response nor a notification")
			return
		}
	}
}

// response answers the call a response belongs to. It reports false when the
// connection ended.
func (c *conn) response(env envelope) bool {
	var id string
	if err := json.Unmarshal(env.id, &id); err != nil || id == "" {
		// A null id is an error the Harness could not attribute to a request.
		c.protocolFail("a response that has no request ID")
		return false
	}
	c.mu.Lock()
	cl := c.pending[id]
	c.mu.Unlock()
	if cl == nil {
		c.protocolFail("a response that nothing waits for")
		return false
	}
	var rep reply
	switch {
	case env.hasRes && !env.hasErr:
		rep = reply{result: env.result}
	case env.hasErr && !env.hasRes:
		re, err := parseRemoteError(env.err)
		if err != nil {
			c.protocolFail("%v", err) // the call is still pending: it is answered with this
			return false
		}
		rep = reply{rpcErr: re}
	default:
		c.protocolFail("a response must have a result or an error, and not both")
		return false
	}
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
	cl.deliver(rep)
	return true
}

// parseRemoteError reads the error member of a response.
func parseRemoteError(raw json.RawMessage) (*RemoteError, error) {
	var wire struct {
		Code    *int            `json:"code"`
		Message *string         `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil || wire.Code == nil || wire.Message == nil {
		return nil, errors.New("an error member that is not a JSON-RPC error")
	}
	re := &RemoteError{RPCCode: *wire.Code, Info: protocol.ErrorInfo{Message: *wire.Message}}
	if len(wire.Data) > 0 && !bytes.Equal(bytes.TrimSpace(wire.Data), []byte("null")) {
		info, err := protocol.Decode[protocol.ErrorInfo](wire.Data)
		if err != nil {
			return nil, errors.New("an error member whose data is not an ErrorInfo")
		}
		re.Info = info
	}
	return re, nil
}

// notification handles one notification. It reports false when the connection ended.
func (c *conn) notification(env envelope) bool {
	if !env.hasPar {
		c.protocolFail("a notification without params")
		return false
	}
	size := len(env.params)
	switch env.method {
	case "event/recorded":
		ev, err := protocol.Decode[protocol.Event](env.params)
		if err != nil {
			c.protocolFail("an event that does not satisfy the schema")
			return false
		}
		c.mu.Lock()
		dup := c.seenEvent(ev.EventID)
		c.mu.Unlock()
		if dup {
			return true
		}
		if ev.Type == protocol.EventRunTerminal && ev.RunID != nil {
			c.signal(*ev.RunID)
		}
		if c.ignore {
			return true
		}
		if !c.queue.putConfirmed(Notification{Kind: NotificationEvent, Event: &ev}, size) {
			c.fail(ErrNotificationOverflow)
			return false
		}
	case "progress/delta":
		d, err := protocol.Decode[protocol.ProgressDelta](env.params)
		if err != nil {
			c.protocolFail("progress that does not satisfy the schema")
			return false
		}
		if !c.ignore {
			c.queue.putDelta(d)
		}
	case "progress/reset":
		r, err := protocol.Decode[protocol.ProgressReset](env.params)
		if err != nil {
			c.protocolFail("progress that does not satisfy the schema")
			return false
		}
		if !c.ignore && !c.queue.putConfirmed(Notification{Kind: NotificationProgressReset, Reset: &r}, size) {
			c.fail(ErrNotificationOverflow)
			return false
		}
	case "progress/gap":
		g, err := protocol.Decode[protocol.ProgressGap](env.params)
		if err != nil {
			c.protocolFail("progress that does not satisfy the schema")
			return false
		}
		if !c.ignore && !c.queue.putConfirmed(Notification{Kind: NotificationProgressGap, Gap: &g}, size) {
			c.fail(ErrNotificationOverflow)
			return false
		}
	default:
		c.protocolFail("an unknown notification")
		return false
	}
	return true
}

// seenEvent records id and reports whether it was already seen in the window. c.mu is held.
func (c *conn) seenEvent(id string) bool {
	if _, dup := c.seen[id]; dup {
		return true
	}
	if old := c.seenRing[c.seenAt]; old != "" {
		delete(c.seen, old)
	}
	c.seenRing[c.seenAt] = id
	c.seenAt = (c.seenAt + 1) % len(c.seenRing)
	c.seen[id] = struct{}{}
	return false
}

// watchTerminal registers interest in the run.terminal event of a Run. The channel is
// signalled when it arrives, or when the connection ends. Call the returned function to
// withdraw.
func (c *conn) watchTerminal(runID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	c.mu.Lock()
	if c.failErr != nil {
		c.mu.Unlock()
		wake(ch)
		return ch, func() {}
	}
	set := c.waiters[runID]
	if set == nil {
		set = map[chan struct{}]struct{}{}
		c.waiters[runID] = set
	}
	set[ch] = struct{}{}
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		if set := c.waiters[runID]; set != nil {
			delete(set, ch)
			if len(set) == 0 {
				delete(c.waiters, runID)
			}
		}
		c.mu.Unlock()
	}
}

func (c *conn) signal(runID string) {
	c.mu.Lock()
	var chans []chan struct{}
	for ch := range c.waiters[runID] {
		chans = append(chans, ch)
	}
	c.mu.Unlock()
	for _, ch := range chans {
		wake(ch)
	}
}

func wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
