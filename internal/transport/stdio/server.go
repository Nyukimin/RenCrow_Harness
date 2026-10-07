package stdio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
	"github.com/Nyukimin/RenCrow_Harness/internal/service"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Backend is what the transport calls: the Service.
type Backend interface {
	// HasMethod reports whether name is one of the native methods; any other name
	// is answered -32601 without reaching Handle.
	HasMethod(name string) bool
	// NewConn starts the state of this connection. The notifier is how progress
	// reaches the client.
	NewConn(service.Notifier) *service.Conn
	// Handle runs one request whose envelope the transport has validated.
	Handle(ctx context.Context, c *service.Conn, req protocol.Request) (service.Result, error)
}

// Options configure Serve. Zero values mean the defaults.
type Options struct {
	// MaxFrameBytes is the largest accepted frame (default 16 MiB).
	MaxFrameBytes int
	// PriorityMessages and PriorityBytes bound the queue of responses and confirmed
	// events (defaults 1024 and 64 MiB); ProgressMessages bounds provisional
	// progress (default 256).
	PriorityMessages int
	PriorityBytes    int
	ProgressMessages int
	// ShutdownGrace is how long the output may take to flush when the input ends
	// or the context is cancelled (default 5 s). service/shutdown brings its own
	// deadline.
	ShutdownGrace time.Duration
	// Diag receives short diagnostics: never request content, a key, a credential
	// or a path. Nil discards them.
	Diag io.Writer
}

func (o Options) withDefaults() Options {
	if o.MaxFrameBytes <= 0 {
		o.MaxFrameBytes = DefaultMaxFrameBytes
	}
	if o.PriorityMessages <= 0 {
		o.PriorityMessages = 1024
	}
	if o.PriorityBytes <= 0 {
		o.PriorityBytes = 64 << 20
	}
	if o.ProgressMessages <= 0 {
		o.ProgressMessages = 256
	}
	if o.ShutdownGrace <= 0 {
		o.ShutdownGrace = 5 * time.Second
	}
	if o.Diag == nil {
		o.Diag = io.Discard
	}
	return o
}

type readResult struct {
	f   frame
	err error
}

type server struct {
	be   Backend
	conn *service.Conn
	o    *outbound
	opts Options
	ctx  context.Context
}

func (s *server) diag(format string, args ...any) {
	fmt.Fprintf(s.opts.Diag, "rencrow-harness: "+format+"\n", args...)
}

// Serve runs one connection: it reads frames from in, answers them in order through
// the Backend and writes responses, confirmed events and progress to out. It
// returns nil when the client ends the input or asks for shutdown and everything
// was flushed; ErrBackpressure when the client did not keep up and the connection
// was closed; ErrDrainTimeout when the output could not be flushed in time; or the
// error of a failed read or write. When it gives up on a client it also closes out
// if out can be closed, so a writer blocked on a stuck client is released.
//
// Requests are handled one at a time, in arrival order, but the output has its own
// writer: a client that stops reading never stalls the handling of control
// requests, it fills a bounded queue and is then disconnected. stdin EOF and
// shutdown share one ending: stop reading, flush within the deadline, return.
func Serve(ctx context.Context, be Backend, in io.Reader, out io.Writer, opts Options) error {
	opts = opts.withDefaults()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	o := newOutbound(Limits{PriorityMessages: opts.PriorityMessages, PriorityBytes: opts.PriorityBytes, ProgressMessages: opts.ProgressMessages})
	s := &server{be: be, o: o, opts: opts, ctx: ctx}
	s.conn = be.NewConn(notifier{o})
	defer s.conn.Close()
	go func() { _ = o.writeLoop(out) }()

	frames := make(chan readResult)
	go func() {
		br := bufio.NewReaderSize(in, 64<<10)
		for {
			f, err := readFrame(br, opts.MaxFrameBytes)
			select {
			case frames <- readResult{f, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	closeOut := func() {
		if c, ok := out.(io.Closer); ok {
			_ = c.Close()
		}
	}
	end := func(grace time.Duration) error {
		if !o.drain(grace) {
			if err := o.failure(); err != nil {
				closeOut()
				return err
			}
			o.close()
			closeOut()
			return ErrDrainTimeout
		}
		o.close()
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return end(opts.ShutdownGrace)
		case <-o.failed():
			err := o.failure()
			closeOut()
			if errors.Is(err, ErrBackpressure) {
				s.diag("the client does not read fast enough; the connection was closed")
			} else {
				s.diag("the output failed; the connection was closed")
			}
			return err
		case r := <-frames:
			if r.err != nil {
				if errors.Is(r.err, io.EOF) {
					return end(opts.ShutdownGrace)
				}
				o.close()
				closeOut()
				s.diag("the input failed")
				return fmt.Errorf("stdio: read: %w", r.err)
			}
			shutdown, err := s.process(r.f)
			if err != nil {
				o.fail(err)
				closeOut()
				s.diag("the connection was closed: %s", describe(err))
				return err
			}
			if shutdown != nil {
				return end(shutdown.Deadline)
			}
		}
	}
}

func describe(err error) string {
	if errors.Is(err, ErrBackpressure) {
		return "the client does not read fast enough"
	}
	return "an event could not be delivered"
}

// process handles one frame and queues whatever it produces. It returns an error
// only when the connection cannot continue (something confirmed could not be
// queued); every problem with the frame itself is answered to the client.
func (s *server) process(f frame) (*service.ShutdownRequest, error) {
	respond := func(b []byte) error { return s.o.putPriority(b) }
	protocolError := func(id *string, code int, msg string) error {
		return respond(errorFrame(id, rpcErrorObject{Code: code, Message: msg}))
	}

	if f.tooLong {
		s.diag("a frame over the size limit was refused")
		return nil, protocolError(nil, codeParseError, "frame is too large")
	}
	if len(bytes.TrimSpace(f.data)) == 0 {
		return nil, nil // a blank line is not a frame
	}
	v, err := strictjson.Decode(f.data)
	if err != nil {
		return nil, protocolError(nil, codeParseError, "parse error")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		if _, batch := v.([]any); batch {
			return nil, protocolError(nil, codeInvalidRequest, "batch requests are not supported")
		}
		return nil, protocolError(nil, codeInvalidRequest, "a request is a JSON object")
	}

	// The id is echoed only when it is a Canonical RequestID; otherwise the answer
	// carries null.
	var id *string
	if raw, ok := obj["id"].(string); ok {
		if _, err := identity.ParseRequestID(raw); err == nil {
			id = &raw
		}
	}
	for k := range obj {
		switch k {
		case "jsonrpc", "id", "method", "params":
		default:
			return nil, protocolError(id, codeInvalidRequest, "unknown member in the request")
		}
	}
	if id == nil {
		return nil, protocolError(nil, codeInvalidRequest, "id must be a request ID")
	}
	if obj["jsonrpc"] != "2.0" {
		return nil, protocolError(id, codeInvalidRequest, `jsonrpc must be "2.0"`)
	}
	method, ok := obj["method"].(string)
	if !ok {
		return nil, protocolError(id, codeInvalidRequest, "method must be a string")
	}
	if !s.be.HasMethod(method) {
		return nil, protocolError(id, codeUnknownMethod, "unknown method")
	}
	req := protocol.Request{JSONRPC: "2.0", ID: *id, Method: method}
	if params, present := obj["params"]; present {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, protocolError(id, codeInvalidParams, "params cannot be read")
		}
		req.Params = raw
	}

	res, err := s.handle(req)
	if err != nil {
		return nil, respond(errorFrame(id, toRPCError(err)))
	}
	// Whatever the call admitted is started only after its response and events are
	// queued (or the queue failed): the Run is committed either way, and the events its
	// driver commits must not overtake the ones that announced it.
	defer res.Done()
	if err := respond(responseFrame(*id, res.Body)); err != nil {
		return nil, err
	}
	// Confirmed events are announced after the commit that made them, in order,
	// after the response of the call that produced them. A duplicate delivery is
	// allowed (the client de-duplicates by event_id); a missing one is not, so an
	// event that cannot be encoded or queued ends the connection.
	for _, ev := range res.Events {
		body, err := protocol.Encode(ev)
		if err != nil {
			return nil, fmt.Errorf("stdio: a committed event cannot be encoded: %w", err)
		}
		if err := respond(notificationFrameRaw("event/recorded", body)); err != nil {
			return nil, err
		}
	}
	return res.Shutdown, nil
}

// handle calls the Backend and turns a panic into an internal error: one bad
// request must not take the whole connection, and its text (which can hold
// anything) is not repeated.
func (s *server) handle(req protocol.Request) (res service.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.diag("a request handler failed (%s)", req.Method)
			res, err = service.Result{}, errors.New("handler panic")
		}
	}()
	res, err = s.be.Handle(s.ctx, s.conn, req)
	if err != nil {
		var pe *protocol.Error
		if !errors.As(err, &pe) {
			s.diag("an internal error occurred in %s", req.Method)
		}
	}
	return res, err
}
