package client

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Client is one connection to a Harness child process (`rencrow-harness serve --stdio`).
// It is safe for concurrent use: calls are pipelined and answered in the order the
// Harness handles them, which is the order they were written.
//
// A Client does not reconnect. When the connection is over (Done is closed) it stays
// over: Start a new one, and repeat what may not have happened with the same idempotency
// key and payload. The Harness's store is durable, and a request it already accepted is
// answered from its receipt.
//
// The child ends by itself when the process that started it ends (its input closes), but
// a caller should always end a Client with Shutdown or Abort.
type Client struct {
	cfg  Config
	conn *conn
	proc *proc
	caps protocol.CapabilitiesResult

	notifications chan Notification
	abortCh       chan struct{}
	abortOnce     sync.Once

	mu           sync.Mutex
	shutdownBy   time.Time // when the child must have exited, once Shutdown began
	shutdownGo   bool
	shutdownDone chan struct{}
	shutdownErr  error
	cause        error // the first failure that was not the end of the output

	expectExit atomic.Bool

	finished chan struct{}
	err      error // valid once finished is closed
}

// Start starts the Harness as cfg says, performs the initialize handshake and checks
// that the Harness speaks this protocol and offers what cfg requires. It fails closed:
// on any mismatch the child is stopped and an error returned. ctx bounds the start and
// the handshake only; it does not govern the life of the connection.
func Start(ctx context.Context, cfg Config) (*Client, error) {
	cfg, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	p, err := startProc(cfg)
	if err != nil {
		return nil, err
	}
	c := newClient(cfg, p)
	c.conn.start()
	go c.pump()
	go c.supervise()

	hello, err := invoke[protocol.InitializeInput, protocol.CapabilitiesResult](ctx, c, "initialize", protocol.InitializeInput{
		ClientName: cfg.ClientName, ClientVersion: cfg.ClientVersion, ProtocolVersion: protocol.ProtocolVersion,
	}, true)
	if err != nil {
		c.Abort()
		if errors.Is(err, ErrProtocol) {
			// What answered initialize is not a Harness of this protocol.
			err = fmt.Errorf("%w: %w", ErrIncompatible, err)
		}
		return nil, fmt.Errorf("client: initialize: %w", err)
	}
	if err := checkCapabilities(hello, cfg.RequireCapabilities); err != nil {
		c.Abort()
		return nil, err
	}
	c.caps = hello
	return c, nil
}

func newClient(cfg Config, p *proc) *Client {
	c := &Client{
		cfg: cfg, proc: p,
		notifications: make(chan Notification),
		abortCh:       make(chan struct{}),
		shutdownDone:  make(chan struct{}),
		finished:      make(chan struct{}),
	}
	c.conn = newConn(p.stdout, p.stdin, connOptions{limits: cfg.Notifications, ignore: cfg.IgnoreNotifications, onFail: c.onFail})
	return c
}

// checkCapabilities is the handshake's fail-closed check.
func checkCapabilities(got protocol.CapabilitiesResult, required []string) error {
	if got.ProtocolVersion != protocol.ProtocolVersion {
		return fmt.Errorf("%w: the Harness speaks another protocol version", ErrIncompatible)
	}
	var missing []string
	for _, name := range required {
		ready := slices.ContainsFunc(got.Capabilities, func(c protocol.Capability) bool { return c.Name == name && c.Status == "ready" })
		if !ready {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: capabilities not ready: %v", ErrIncompatible, missing)
	}
	return nil
}

// Capabilities returns what the Harness declared in initialize. Use ServiceCapabilities
// to ask again.
func (c *Client) Capabilities() protocol.CapabilitiesResult { return c.caps }

// Notifications returns the notifications of the Harness (confirmed events, and
// provisional progress), in the order they arrived. The channel is closed once the
// connection is over and everything that was queued has been delivered, or at once by
// Abort. A consumer must read it until it is closed (or Abort). It carries nothing when
// the Config says IgnoreNotifications.
//
// The reader of the connection never waits for the consumer, so a slow consumer never
// delays a response or a stop: past the limits of Config.Notifications, progress is
// replaced by a NotificationProgressGap (LocalGap), and a confirmed event that cannot
// be kept ends the connection with ErrNotificationOverflow, never silently lost.
func (c *Client) Notifications() <-chan Notification { return c.notifications }

// Done is closed once the connection is over and the child has exited.
func (c *Client) Done() <-chan struct{} { return c.finished }

// Err is nil until Done is closed. Then it is nil if the Harness ended after a Shutdown,
// ErrAborted after Abort, and otherwise the cause: ErrProcessExited, ErrProtocol,
// ErrNotificationOverflow, or a failure of the pipe.
func (c *Client) Err() error {
	select {
	case <-c.finished:
		return c.err
	default:
		return nil
	}
}

// Wait blocks until Done is closed and returns Err.
func (c *Client) Wait() error {
	<-c.finished
	return c.err
}

// onFail is told by the connection that it ended with cause. The end of the output is how
// a clean exit looks and needs no action; any other cause leaves a child that must be
// stopped.
func (c *Client) onFail(cause error) {
	if errors.Is(cause, errPeerClosed) {
		return
	}
	c.mu.Lock()
	if c.cause == nil {
		c.cause = cause
	}
	c.mu.Unlock()
	c.proc.kill()
}

// pump delivers the queue to the channel, in order.
func (c *Client) pump() {
	defer close(c.notifications)
	for {
		n, ok := c.conn.queue.next()
		if !ok {
			return
		}
		select {
		case c.notifications <- n:
		case <-c.abortCh:
			c.conn.queue.discard()
			return
		}
	}
}

// exitWait is how long the child may take to exit after its output ended.
func (c *Client) exitWait() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shutdownGo {
		return max(time.Until(c.shutdownBy), 0)
	}
	return c.cfg.ExitGrace
}

// supervise waits for the end of the output and for the child, kills the child if it
// does not exit, and decides how the connection ended.
func (c *Client) supervise() {
	select {
	case <-c.conn.readerDone:
	case <-c.proc.exited:
		// The child is gone, and what it wrote is in the pipe. A descendant that kept the
		// pipe open would hold the reader for ever, so the reader gets a moment to finish
		// and is then cut off by closing the read end.
		select {
		case <-c.conn.readerDone:
		case <-time.After(processWaitDelay):
			_ = c.proc.stdout.Close()
			select {
			case <-c.conn.readerDone:
			case <-time.After(processWaitDelay):
			}
		}
	}
	// The connection is over; make sure of it, and of the queue, whatever the reader did.
	c.conn.fail(errPeerClosed)
	c.conn.queue.close()
	timer := time.NewTimer(c.exitWait())
	defer timer.Stop()
	select {
	case <-c.proc.exited:
	case <-timer.C:
		c.proc.kill()
		<-c.proc.exited
	}
	_ = c.proc.stdout.Close()

	c.mu.Lock()
	cause := c.cause
	c.mu.Unlock()
	switch {
	case cause != nil:
		c.err = cause
	case c.proc.waitErr != nil:
		c.err = fmt.Errorf("%w: %w", ErrProcessExited, c.proc.waitErr)
	case !c.expectExit.Load():
		c.err = fmt.Errorf("%w: it ended although no shutdown was asked for", ErrProcessExited)
	}
	close(c.finished)
}

// Abort stops the Harness at once: later calls are refused, every waiting call fails
// (with ErrOutcomeUnknown where its request was written), the queued notifications are
// dropped and the channel of Notifications is closed, and the child is killed. It returns
// when the child is gone. It is idempotent, safe to call concurrently and at any time, and
// the way to end a Shutdown that takes too long. After the connection ended on its own
// (Done is closed), Abort changes nothing about how it ended, and only releases a consumer
// that did not read Notifications to the end.
//
// Abort asks the Harness for nothing, so it can not record how its Runs ended: a Run that
// was being driven stays non-terminal in the store until the next process settles it, and
// its Tool processes are the Harness's to stop, not this client's. Use Shutdown for an
// orderly end.
func (c *Client) Abort() {
	c.abortOnce.Do(func() {
		select {
		case <-c.finished: // already over: there is nothing to stop
		default:
			c.mu.Lock()
			if c.cause == nil {
				c.cause = ErrAborted
			}
			c.mu.Unlock()
			c.conn.fail(ErrAborted)
			c.proc.kill()
		}
		close(c.abortCh)
		c.conn.queue.discard()
	})
	<-c.finished
}

// Shutdown ends the Harness in an orderly way: calls made from now on are refused with
// ErrStopping, the Harness is sent service/shutdown with in (mode drain or cancel, and
// the deadline in seconds, 1 to 300), and the child is waited for. In drain mode its
// Runs get the deadline to end before they are stopped; in cancel mode they are stopped
// at once; either way each records how it ended before the child exits. If the child has
// not exited within twice the deadline plus Config.ExitGrace it is killed and the result
// (and Err) is ErrShutdownTimeout.
//
// To learn how a Run ended, await it (AwaitRun) before shutting down: notifications after
// the shutdown request are not guaranteed to be delivered, and the end of every Run is
// durable and readable afterwards through a new connection or the CLI. What was already
// queued is still delivered: read Notifications until it is closed, or call Abort, which
// drops it.
//
// Shutdown is idempotent and safe to call concurrently: the first call's arguments
// win and every call returns its result. If ctx ends first Shutdown returns ctx.Err() and
// the shutdown goes on; call Abort to cut it short. It returns nil for a clean end, the
// ErrAborted of an earlier Abort, or the cause of an earlier failure.
func (c *Client) Shutdown(ctx context.Context, in protocol.ShutdownInput) error {
	select {
	case <-c.finished:
		return c.err
	default:
	}
	params, err := protocol.Encode(in)
	if err != nil {
		return err
	}
	c.mu.Lock()
	first := !c.shutdownGo
	if first {
		c.shutdownGo = true
		total := 2*time.Duration(in.DeadlineSeconds)*time.Second + c.cfg.ExitGrace
		c.shutdownBy = time.Now().Add(total)
	}
	by := c.shutdownBy
	c.mu.Unlock()
	if first {
		c.conn.beginStopping()
		c.expectExit.Store(true)
		go c.runShutdown(params, by)
	}
	select {
	case <-c.shutdownDone:
		return c.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runShutdown does the shutdown, whoever asked and however long they wait.
func (c *Client) runShutdown(params []byte, by time.Time) {
	defer close(c.shutdownDone)
	ctx, cancel := context.WithDeadline(context.Background(), by)
	defer cancel()
	var askErr error
	raw, err := c.conn.do(ctx, "service/shutdown", params, true)
	if err == nil {
		if _, derr := protocol.Decode[protocol.ShutdownResult](raw); derr != nil {
			askErr = fmt.Errorf("%w: the result of service/shutdown does not satisfy the schema: %w", ErrProtocol, derr)
		}
	} else {
		askErr = err
	}
	// The input closes in any case: it is the same ending for the Harness, and it ends a
	// Harness that did not hear the request.
	_ = c.conn.w.Close()
	timer := time.NewTimer(time.Until(by))
	defer timer.Stop()
	select {
	case <-c.proc.exited:
	case <-timer.C:
		c.mu.Lock()
		if c.cause == nil {
			c.cause = ErrShutdownTimeout
		}
		c.mu.Unlock()
		c.proc.kill()
		<-c.finished
		c.shutdownErr = c.err
		return
	}
	<-c.finished
	switch {
	case c.err != nil:
		c.shutdownErr = c.err
	case askErr != nil && !errors.Is(askErr, ErrClosed):
		c.shutdownErr = askErr
	}
}
