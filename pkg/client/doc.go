// Package client is the Go client of the RenCrow_Harness native protocol: it starts
// `rencrow-harness serve --stdio` as a child process, speaks JSON-RPC 2.0 over NDJSON
// with it (PROTOCOL section 1), and exposes the protocol's methods as a typed API. It
// runs no engine, owns no store and decides nothing: every request is validated and made
// canonical by package protocol before it is sent, and every result is validated by
// package protocol before it is returned. It depends on the standard library and package
// protocol only.
//
// # Starting and stopping
//
// Start launches the child exactly as Config says (the binary, the one configuration
// path, the working directory and the whole environment are the caller's; nothing is
// inherited and no secret goes on the command line), performs initialize and checks the
// protocol version and the capabilities the caller requires. A mismatch stops the child
// and fails; the client never continues against a Harness it does not know.
//
// Stopping has two stages. Abort is the immediate one: it refuses new calls, fails the
// waiting ones, drops the queued notifications and kills the child. Shutdown is the
// orderly one: it refuses new calls, asks the Harness to stop its Runs (service/shutdown,
// mode drain or cancel, with a deadline) and waits for the child, and if the child does
// not exit in time it escalates to the kill. Both are idempotent and safe to call
// concurrently, and neither can be left half done: Done is closed once the child is gone.
//
// # Calls and cancellation
//
// Calls take a context. A context that ends before a request was handed to the writer
// means the request was not sent. After it, the request may be on the wire and the
// error also says ErrOutcomeUnknown: repeat the call with the same input, the same
// idempotency key and the same payload, which the Harness answers from its receipt. A
// context never stops a Run by itself, because a Run outlives the call that admitted it:
// the stop is the explicit TurnInterrupt (or InterruptRun, which finds the control
// revision), or AwaitRun with an interrupt key, which turns the end of its context into
// one stop signal and then waits for the Run to end. The receipt of a stop is a record,
// not proof that anything stopped.
//
// # Notifications
//
// Confirmed events (event/recorded) and provisional progress arrive on Notifications in
// the order the Harness sent them. The reader of the connection never waits for the
// consumer, so a slow consumer never delays a response or a stop. Past its limits,
// progress is replaced by a gap marker and a confirmed event that cannot be kept ends
// the connection with ErrNotificationOverflow; events are durable, and EventsRead
// returns what was missed. A repeated event_id within the last few thousand events is
// dropped; across a reconnection the consumer deduplicates by event_id.
//
// The client does not reconnect, retry or sign anything. The OriginProof of a
// request, the idempotency keys and the limits are the caller's, sent as given.
package client
