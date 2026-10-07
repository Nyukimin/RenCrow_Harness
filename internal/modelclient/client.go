// Package modelclient is the one real modelport.ModelPort: the Harness side of the
// strict LLM contract (harness-v1), spoken to the RenCrow_LLM Gateway over HTTP.
//
// It is a client and nothing else. It owns no retry, no recovery profile choice, no
// budget and no ID: one Generate call is one HTTP request and at most one backend
// generation, one Measure call is one HTTP request that generates nothing, and
// whatever the Gateway states about a generation is handed on as it was stated.
// What it adds is the checking the contract asks of the caller: the closed shape of
// every answer, the echo of the digests, the receipt held to the request, and a
// generation state that is only ever claimed from evidence.
//
// The Gateway is reached only through a configured loopback base URL. The client
// follows no redirect, uses no proxy and keeps no connection, so a request is never
// sent twice behind the caller's back and never sent anywhere else than where it
// was configured to go.
//
// Nothing here logs. A request, a response body and a URL are never put into an
// error text: an error names a code and what failed, and carries the cause only as
// a wrapped value for errors.Is.
package modelclient

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"context"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
)

// Options tune the transport. The zero value is the standard configuration.
type Options struct {
	// DialTimeout bounds establishing one connection to the Gateway (default 5s). A
	// connection that was not established means no request byte was sent.
	DialTimeout time.Duration
}

const defaultDialTimeout = 5 * time.Second

// Response size limits. A strict answer is small: the generation itself is at most
// 1 MiB of text, and a stream is read by the caller, not here.
const (
	maxStatusBytes  = 8 << 20
	maxMeasureBytes = 1 << 20
	maxFailureBytes = 1 << 20
	maxAnswerBytes  = 8 << 20
)

// Client is the Gateway client. It is safe for concurrent use.
type Client struct {
	base string // the configured base URL without its trailing slash
	http *http.Client
}

var _ modelport.ModelPort = (*Client)(nil)

// New returns a client for the Gateway at baseURL, which must be a loopback http(s)
// URL such as http://127.0.0.1:<port>/v1. The endpoints are the base with one
// trailing slash removed and the path of the call appended; the base is never
// corrected (a /v1/v1 stays /v1/v1). A non-loopback Gateway needs TLS and client
// authentication that this client does not provide, and is refused.
func New(baseURL string, opts Options) (*Client, error) {
	base, err := parseBase(baseURL)
	if err != nil {
		return nil, err
	}
	timeout := opts.DialTimeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	dialer := &net.Dialer{Timeout: timeout, Control: loopbackOnly}
	transport := &http.Transport{
		Proxy:                  nil, // never the environment's proxy
		DialContext:            dialer.DialContext,
		DisableKeepAlives:      true, // one request, one connection
		DisableCompression:     true,
		ForceAttemptHTTP2:      false,
		TLSNextProto:           map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSHandshakeTimeout:    timeout,
		MaxResponseHeaderBytes: 64 << 10,
	}
	return &Client{
		base: base,
		http: &http.Client{
			Transport: transport,
			// A redirect is an answer, not an instruction: following it would send the
			// request body to a place nobody configured.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// parseBase checks the base URL and returns it without its trailing slash. The
// messages never repeat the URL, which may carry what must not be shown.
func parseBase(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" {
		return "", errors.New("modelclient: the Gateway base URL is not an http(s) URL")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("modelclient: the Gateway base URL must not carry credentials, a query or a fragment")
	}
	host := u.Hostname()
	if host != "localhost" {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return "", errors.New("modelclient: the Gateway base URL must be a loopback address")
		}
	}
	p := strings.TrimSuffix(u.Path, "/")
	if u.RawPath != "" || strings.ContainsAny(p, "%?#\\") || strings.Contains(p, "//") || strings.HasSuffix(p, "/") || hasDotSegment(p) {
		return "", errors.New("modelclient: the Gateway base URL path is not a plain path")
	}
	return u.Scheme + "://" + u.Host + p, nil
}

func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// loopbackOnly refuses to connect anywhere but a loopback address, whatever the
// host name resolved to.
func loopbackOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("modelclient: the address is not a host and port")
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return errors.New("modelclient: the Gateway address is not a loopback address")
	}
	return nil
}

// endpoint returns the URL of one call.
func (c *Client) endpoint(path string) string { return c.base + path }

// TransportError is a failure of the exchange with the Gateway that says nothing
// about a generation: the connection could not be made, broke, timed out or was
// stopped by the caller's context. The Gateway's own refusals are *StrictError.
// The effect on a generation is unknown, which is how the kernel reads any error
// that is not a *StrictError.
type TransportError struct {
	// Op is describe, measure or generate.
	Op string
	// Connected is whether a connection to the Gateway was established before the
	// failure. When it is false no byte of the request was written.
	Connected bool
	Err       error
}

func (e *TransportError) Error() string {
	if !e.Connected {
		return "modelclient: " + e.Op + ": the Gateway could not be reached"
	}
	return "modelclient: " + e.Op + ": the exchange with the Gateway failed"
}

// Unwrap returns the cause without the URL the HTTP client had put around it.
func (e *TransportError) Unwrap() error { return e.Err }

// exchange is one finished HTTP exchange up to its response headers.
type exchange struct {
	resp      *http.Response
	connected bool
}

// roundTrip sends one request. It never sends twice: no redirect, no retry, no
// connection reuse (see New). Whether a connection was established is observed, so
// a failure before it can be told from one after it.
func (c *Client) roundTrip(ctx context.Context, op, method, path string, body []byte, accept string) (exchange, error) {
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), rd)
	if err != nil {
		return exchange{}, &TransportError{Op: op, Err: errors.New("the request could not be built")}
	}
	req.GetBody = nil // nothing may rewind the body and send it again
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "rencrow-harness")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return exchange{connected: connected.Load()}, &TransportError{Op: op, Connected: connected.Load(), Err: cause(err)}
	}
	return exchange{resp: resp, connected: connected.Load()}, nil
}

// cause strips the *url.Error the HTTP client wraps every failure in (it carries
// the URL) and keeps what is below it for errors.Is.
func cause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// readBody reads at most limit bytes and reports whether the body was longer.
func readBody(r io.Reader, limit int64) ([]byte, bool, error) {
	data, over, err := readPrefix(r, limit)
	if over {
		return nil, true, err
	}
	return data, false, err
}

// readPrefix reads at most limit bytes. When the body is longer it returns the first
// limit bytes and over=true, so a caller that keeps the refusal as Evidence still has
// what came first.
func readPrefix(r io.Reader, limit int64) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}

// attachBody keeps the bytes of a refusal on the error it was read as, up to what
// Evidence may hold (modelport.MaxFailureBodyBytes). over says the body was longer than
// what was read.
func attachBody(e *modelport.StrictError, body []byte, over bool) *modelport.StrictError {
	if len(body) > modelport.MaxFailureBodyBytes {
		body, over = body[:modelport.MaxFailureBodyBytes], true
	}
	e.Body, e.BodyTruncated = bytes.Clone(body), over
	return e
}

// marshalJSON encodes a request body: compact, with no HTML escaping, as the
// digest-bearing text is written everywhere in the contract.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// wireRequest returns the request as it is sent: the empty lists written as lists,
// never as null.
func wireRequest(r modelport.ChatRequest) modelport.ChatRequest {
	if r.Tools == nil {
		r.Tools = []modelport.FunctionTool{}
	}
	if r.Stop == nil {
		r.Stop = []string{}
	}
	return r
}

func errf(format string, args ...any) error { return fmt.Errorf("modelclient: "+format, args...) }
