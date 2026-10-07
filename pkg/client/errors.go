package client

import (
	"errors"
	"fmt"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// The errors of the client. Every one is a sentinel for errors.Is; a failure that has a
// cause wraps it, so the cause is reachable with errors.Is / errors.As as well.
var (
	// ErrInvalidConfig is returned by Start for a Config that is not usable. Nothing was
	// started.
	ErrInvalidConfig = errors.New("client: invalid configuration")

	// ErrIncompatible is returned by Start when the Harness answers initialize with
	// another protocol version, or lacks a capability the Config requires. The child is
	// stopped; a client never continues against a Harness it does not know.
	ErrIncompatible = errors.New("client: the Harness is not compatible with this client")

	// ErrClosed is returned by a call that was refused or ended because the connection is
	// over: the child exited, the client was aborted, or the connection failed. The cause
	// is Client.Err once Done is closed. A call that had been written when this happened
	// also wraps ErrOutcomeUnknown.
	ErrClosed = errors.New("client: the connection is closed")

	// ErrStopping is returned by a call made after Shutdown began. The call was not sent.
	ErrStopping = errors.New("client: the client is shutting down")

	// ErrAborted is the cause of a connection ended by Abort (Err and Wait report it).
	ErrAborted = errors.New("client: aborted")

	// ErrOutcomeUnknown is wrapped by the error of a call whose request may have reached
	// the Harness but whose answer did not arrive: the context ended after the request was
	// written, or the connection ended before the response. For a mutation this means it
	// may have happened. Do not start the work again under a new idempotency key: repeat
	// the call with the same input (the same key and payload), which the Harness answers
	// from its receipt, or read the receipt.
	ErrOutcomeUnknown = errors.New("client: the request may have been delivered; its outcome is unknown")

	// ErrProtocol is the cause of a connection ended because the Harness broke the
	// protocol (a frame that is not JSON-RPC 2.0, an unknown notification, a response that
	// nothing waits for, a result that does not satisfy the schema, ...). A call whose
	// result did not satisfy the schema wraps it too.
	ErrProtocol = errors.New("client: the Harness broke the protocol")

	// ErrNotificationOverflow is the cause of a connection ended because the consumer of
	// Notifications did not keep up and a confirmed event could not be kept. Nothing
	// durable is lost: read the events again with EventsRead after the last seq seen.
	ErrNotificationOverflow = errors.New("client: the notification consumer does not keep up; the connection was closed")

	// ErrProcessExited is the cause of a connection ended because the child exited when
	// no shutdown had been asked for, or exited with a failure. It wraps the exit error.
	ErrProcessExited = errors.New("client: the Harness process exited")

	// ErrShutdownTimeout is the result of Shutdown when the child did not exit in the time
	// it was given and was killed.
	ErrShutdownTimeout = errors.New("client: the Harness did not exit in time and was killed")

	// ErrRequestTooLarge is returned for a request that would be over the 16 MiB frame
	// limit of the protocol. The request was not sent.
	ErrRequestTooLarge = errors.New("client: the request is over the frame size limit")

	// ErrRunStillActive is returned by AwaitRun when its context ended, the stop signal
	// was recorded, and the Run did not reach its end within the grace period.
	ErrRunStillActive = errors.New("client: the run has not ended")
)

// RemoteError is a failure the Harness answered a call with (a JSON-RPC error response).
// It unwraps to a *protocol.Error, so protocol.CodeOf(err) gives the domain code
// (BUSY, REVISION_CONFLICT, IDEMPOTENCY_CONFLICT, ...). The call was refused: for a
// mutation, nothing was written, except where the code says otherwise
// (PERSISTENCE_UNCERTAIN).
type RemoteError struct {
	// RPCCode is the JSON-RPC code: -32000 for a domain error, or the standard code.
	RPCCode int
	// Info is the ErrorInfo of the response; it is the zero value when the response
	// carried none.
	Info protocol.ErrorInfo
}

// Error implements error. The message is the Harness's own short diagnostic.
func (e *RemoteError) Error() string {
	return fmt.Sprintf("client: the Harness refused the call (%d %s): %s", e.RPCCode, e.Info.Code, e.Info.Message)
}

// Unwrap returns the failure as the protocol's typed error.
func (e *RemoteError) Unwrap() error {
	return &protocol.Error{Code: e.Info.Code, Message: e.Info.Message, Retryable: e.Info.Retryable}
}

// outcomeUnknown wraps cause so that it is also ErrOutcomeUnknown.
func outcomeUnknown(cause error) error {
	return fmt.Errorf("%w: %w", ErrOutcomeUnknown, cause)
}
