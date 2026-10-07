package client

import (
	"context"
	"fmt"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// invoke is the one path of every typed call: the params are validated and made
// canonical (protocol.Encode) before anything is sent, the result is validated
// (protocol.Decode) before it is returned, and nothing in between is the client's to
// interpret. internal calls are the handshake and the shutdown request, allowed while
// the client is stopping.
func invoke[P, R protocol.Message](ctx context.Context, c *Client, method string, in P, internal bool) (R, error) {
	var zero R
	params, err := protocol.Encode(in)
	if err != nil {
		return zero, err // not sent
	}
	raw, err := c.conn.do(ctx, method, params, internal)
	if err != nil {
		return zero, err
	}
	out, err := protocol.Decode[R](raw)
	if err != nil {
		// The Harness answered, so the call may have had its effect; what the answer said is
		// not known.
		return zero, outcomeUnknown(fmt.Errorf("%w: the result of %s does not satisfy the schema: %w", ErrProtocol, method, err))
	}
	return out, nil
}

// The typed API: one method for each method of the native protocol (PROTOCOL section 3),
// named after it, taking and returning the types of package protocol. Every method
// refuses a malformed input before sending it (a *protocol.Error, INVALID_PARAMS or
// INVALID_REQUEST) and returns a *RemoteError when the Harness refuses the call
// (protocol.CodeOf(err) is its code). A call that was written and not answered because
// ctx ended or the connection ended is also ErrOutcomeUnknown.
//
// The seven mutations (SessionOpen, SessionFork, TurnStart, InputAppend, TurnInterrupt,
// RunResume, ContextCompact) carry an idempotency_key. To repeat one after an unknown
// outcome, send the same input again: the same key and the same payload are answered from
// the Harness's receipt, and the same key with another payload is IDEMPOTENCY_CONFLICT.
// Keep the first input (an OriginProof included) and send it again unchanged; the client
// adds nothing to it and signs nothing.

// ServiceCapabilities is service/capabilities: what the Harness can do now.
func (c *Client) ServiceCapabilities(ctx context.Context) (protocol.CapabilitiesResult, error) {
	return invoke[protocol.EmptyInput, protocol.CapabilitiesResult](ctx, c, "service/capabilities", protocol.EmptyInput{}, false)
}

// SessionOpen is session/open: a new Session and Thread, durably. No model is called.
func (c *Client) SessionOpen(ctx context.Context, in protocol.SessionOpenInput) (protocol.SessionOpenResult, error) {
	return invoke[protocol.SessionOpenInput, protocol.SessionOpenResult](ctx, c, "session/open", in, false)
}

// SessionList is session/list: the Sessions the caller can read.
func (c *Client) SessionList(ctx context.Context, in protocol.SessionListInput) (protocol.SessionListResult, error) {
	return invoke[protocol.SessionListInput, protocol.SessionListResult](ctx, c, "session/list", in, false)
}

// SessionGet is session/get: the current snapshot of one Thread.
func (c *Client) SessionGet(ctx context.Context, in protocol.SessionGetInput) (protocol.SessionInfo, error) {
	return invoke[protocol.SessionGetInput, protocol.SessionInfo](ctx, c, "session/get", in, false)
}

// SessionFork is session/fork: a new Thread from a checkpoint of another.
func (c *Client) SessionFork(ctx context.Context, in protocol.SessionForkInput) (protocol.ForkResult, error) {
	return invoke[protocol.SessionForkInput, protocol.ForkResult](ctx, c, "session/fork", in, false)
}

// TurnStart is turn/start: it admits a new Turn, Task and Run. The result says the Run
// was accepted, not that the work is done: follow the Run with AwaitRun or RunGet. The
// input's origin proof, if any, is sent as given.
func (c *Client) TurnStart(ctx context.Context, in protocol.StartInput) (protocol.StartResult, error) {
	return invoke[protocol.StartInput, protocol.StartResult](ctx, c, "turn/start", in, false)
}

// InputAppend is input/append: one more input for the running Run.
func (c *Client) InputAppend(ctx context.Context, in protocol.InputAppendInput) (protocol.InputReceipt, error) {
	return invoke[protocol.InputAppendInput, protocol.InputReceipt](ctx, c, "input/append", in, false)
}

// TurnInterrupt is turn/interrupt: it records the stop signal of a Run. The receipt says
// the signal is recorded, not that anything has stopped: the Run ends when its
// run.terminal event says so (AwaitRun, RunGet). InterruptRun finds the control revision
// for you.
func (c *Client) TurnInterrupt(ctx context.Context, in protocol.InterruptInput) (protocol.InterruptReceipt, error) {
	return invoke[protocol.InterruptInput, protocol.InterruptReceipt](ctx, c, "turn/interrupt", in, false)
}

// RunGet is run/get: the phase of a Run, or its determined RunResult.
func (c *Client) RunGet(ctx context.Context, in protocol.RunGetInput) (protocol.RunInfo, error) {
	return invoke[protocol.RunGetInput, protocol.RunInfo](ctx, c, "run/get", in, false)
}

// RunResume is run/resume: a new Run of an ended Task.
func (c *Client) RunResume(ctx context.Context, in protocol.ResumeInput) (protocol.ResumeResult, error) {
	return invoke[protocol.ResumeInput, protocol.ResumeResult](ctx, c, "run/resume", in, false)
}

// ContextCompact is context/compact: the manual compaction of an idle Thread. Its result
// is read with ReceiptGet.
func (c *Client) ContextCompact(ctx context.Context, in protocol.CompactInput) (protocol.OperationAccepted, error) {
	return invoke[protocol.CompactInput, protocol.OperationAccepted](ctx, c, "context/compact", in, false)
}

// ReceiptGet is receipt/get: the result of an operation, also after its response was
// lost.
func (c *Client) ReceiptGet(ctx context.Context, in protocol.ReceiptGetInput) (protocol.ReceiptRecord, error) {
	return invoke[protocol.ReceiptGetInput, protocol.ReceiptRecord](ctx, c, "receipt/get", in, false)
}

// EventsRead is events/read: the confirmed events of a Thread after a sequence number.
// It is how a consumer catches up after a reconnection or ErrNotificationOverflow.
func (c *Client) EventsRead(ctx context.Context, in protocol.EventsReadInput) (protocol.EventsReadResult, error) {
	return invoke[protocol.EventsReadInput, protocol.EventsReadResult](ctx, c, "events/read", in, false)
}

// EvidenceRead is evidence/read: up to 64 KiB of an Evidence's bytes.
func (c *Client) EvidenceRead(ctx context.Context, in protocol.EvidenceReadInput) (protocol.EvidenceReadResult, error) {
	return invoke[protocol.EvidenceReadInput, protocol.EvidenceReadResult](ctx, c, "evidence/read", in, false)
}
