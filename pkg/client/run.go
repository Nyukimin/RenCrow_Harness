package client

import (
	"context"
	"fmt"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

const (
	// DefaultCancelGrace is how long AwaitRun waits for a Run to end after it recorded the
	// stop signal for a context that ended.
	DefaultCancelGrace = 30 * time.Second
	// DefaultPollInterval is how often AwaitRun asks the Harness for the state of the Run,
	// as a safety net next to the run.terminal notification.
	DefaultPollInterval = 5 * time.Second

	maxInterruptTries = 3
)

// InterruptRun records the stop signal of a Run (turn/interrupt) without the caller
// knowing its control revision: it reads the revision with run/get and sends the signal
// with it, and if the revision moved in between (REVISION_CONFLICT) it reads again, up to
// three times. The idempotency key of each try is keyPrefix + ".r" + the revision it
// expected, so asking again for the same revision is answered from the Harness's receipt
// and asking after the revision moved is a new, harmless stop signal. keyPrefix must make
// a valid key together with that suffix (16 to 128 characters of A-Za-z0-9_.:-); use one
// that names the Run, such as "stop." + runID.
//
// The receipt says the signal is recorded, or that the Run had already ended
// (ALREADY_TERMINAL); it does not say that anything stopped. The Run ends with its
// run.terminal event: wait for it with AwaitRun.
func (c *Client) InterruptRun(ctx context.Context, runID, keyPrefix string) (protocol.InterruptReceipt, error) {
	var last error
	for range maxInterruptTries {
		info, err := c.RunGet(ctx, protocol.RunGetInput{RunID: runID})
		if err != nil {
			return protocol.InterruptReceipt{}, err
		}
		rec, err := c.TurnInterrupt(ctx, protocol.InterruptInput{
			RunID: runID, ExpectedControlRevision: info.ControlRevision,
			IdempotencyKey: fmt.Sprintf("%s.r%d", keyPrefix, info.ControlRevision),
		})
		if protocol.CodeOf(err) == protocol.CodeRevisionConflict {
			last = err
			continue
		}
		return rec, err
	}
	return protocol.InterruptReceipt{}, last
}

// AwaitOptions are the options of AwaitRun. The zero value is valid.
type AwaitOptions struct {
	// InterruptKeyPrefix, when not empty, ties the context to the Run: if ctx ends before
	// the Run does, the stop signal is recorded once with InterruptRun (using this prefix)
	// and the Run is then waited for, up to CancelGrace, with a context of its own. When
	// empty, a context that ends only ends the wait: the Run is not stopped.
	InterruptKeyPrefix string
	// CancelGrace is how long to wait for the Run to end after the stop signal. Default
	// DefaultCancelGrace.
	CancelGrace time.Duration
	// PollInterval is the period of the safety poll. Default DefaultPollInterval.
	PollInterval time.Duration
}

// AwaitRun waits for the Run to reach its end and returns its RunResult, read from the
// Harness (run/get), not rebuilt from events. The end is learned from the run.terminal
// notification, and by a slow poll as a safety net: the Run may be driven by another
// process, which notifies nobody here.
//
// The result is a statement of how the Run ended (Status and Code), not of whether the
// work succeeded: read Verification, and do not take completed for the Task's success.
//
// If ctx ends first and AwaitOptions.InterruptKeyPrefix is empty, AwaitRun returns
// ctx.Err() and the Run goes on. If it is set, the stop signal is recorded (and, being a
// record, is not proof of a stop), the Run is waited for up to CancelGrace, and AwaitRun
// returns the RunResult it ended with (cancelled, normally; whatever it actually was if it
// ended otherwise first), or an error that is ErrRunStillActive and ctx.Err() if it had not
// ended by then. If the connection ends, the error is ErrClosed.
func (c *Client) AwaitRun(ctx context.Context, runID string, opts AwaitOptions) (protocol.RunResult, error) {
	if opts.CancelGrace <= 0 {
		opts.CancelGrace = DefaultCancelGrace
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	res, err := c.awaitTerminal(ctx, runID, opts.PollInterval)
	if err == nil || ctx.Err() == nil || opts.InterruptKeyPrefix == "" {
		return res, err
	}
	cause := ctx.Err()
	grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), opts.CancelGrace)
	defer cancel()
	if _, err := c.InterruptRun(grace, runID, opts.InterruptKeyPrefix); err != nil {
		return protocol.RunResult{}, fmt.Errorf("%w: the stop signal could not be recorded: %w", cause, err)
	}
	res, err = c.awaitTerminal(grace, runID, opts.PollInterval)
	if err != nil {
		if grace.Err() == nil {
			return protocol.RunResult{}, err // not the grace running out: the connection, most likely
		}
		return protocol.RunResult{}, fmt.Errorf("%w: %w", ErrRunStillActive, cause)
	}
	return res, nil
}

// awaitTerminal waits for the Run to be terminal. It registers for the notification
// before it asks, so that the end cannot fall between the two.
func (c *Client) awaitTerminal(ctx context.Context, runID string, poll time.Duration) (protocol.RunResult, error) {
	woken, unwatch := c.conn.watchTerminal(runID)
	defer unwatch()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		info, err := c.RunGet(ctx, protocol.RunGetInput{RunID: runID})
		if err != nil {
			return protocol.RunResult{}, err
		}
		if info.Terminal && info.Result != nil {
			return *info.Result, nil
		}
		select {
		case <-woken:
		case <-ticker.C:
		case <-ctx.Done():
			return protocol.RunResult{}, ctx.Err()
		}
	}
}
