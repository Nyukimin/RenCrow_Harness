package intake

import (
	"errors"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// ErrPolicyUnavailable is what a limit resolver returns when the session's policy
// cannot be used at all (it is not registered). Any other resolver failure is an
// internal fault and is not reported as a refusal of the caller.
var ErrPolicyUnavailable = errors.New("intake: the session's policy is not available")

// Budget is a Run's frozen budget: the explicit limits and the instant they end.
type Budget struct {
	Limits     protocol.Limits
	DeadlineAt string
}

// MinLimits is the field-wise smaller of two limit sets: the cap that applies when
// both the host configuration and the policy bound a Run.
func MinLimits(a, b protocol.Limits) protocol.Limits {
	return protocol.Limits{
		MaxModelSteps:         min(a.MaxModelSteps, b.MaxModelSteps),
		MaxToolCallsPerStep:   min(a.MaxToolCallsPerStep, b.MaxToolCallsPerStep),
		DeadlineSeconds:       min(a.DeadlineSeconds, b.DeadlineSeconds),
		MaxCaptureBytes:       min(a.MaxCaptureBytes, b.MaxCaptureBytes),
		MaxGenerationAttempts: min(a.MaxGenerationAttempts, b.MaxGenerationAttempts),
	}
}

func limitsRejected(reason string) error {
	return protocol.NewError(protocol.CodeInvalidLimits, "limits rejected: %s", reason).Wrap(invalid("limits: %s", reason))
}

type limitField struct {
	name        string
	got, capped int64
}

// ResolveLimits (F35) freezes the budget of a new Run. Every limit must be
// explicit, positive and not above the cap; nothing is defaulted, inherited or
// clamped, so a request that asks for more than the host allows is refused whole.
// The deadline is the acceptance instant plus deadline_seconds, in whole seconds
// and truncated, so it is never later than the request asked for.
func ResolveLimits(requested, caps protocol.Limits, now time.Time) (Budget, error) {
	for _, f := range []limitField{
		{"max_model_steps", requested.MaxModelSteps, caps.MaxModelSteps},
		{"max_tool_calls_per_step", requested.MaxToolCallsPerStep, caps.MaxToolCallsPerStep},
		{"deadline_seconds", requested.DeadlineSeconds, caps.DeadlineSeconds},
		{"max_capture_bytes", requested.MaxCaptureBytes, caps.MaxCaptureBytes},
		{"max_generation_attempts", requested.MaxGenerationAttempts, caps.MaxGenerationAttempts},
	} {
		if f.got < 1 {
			return Budget{}, limitsRejected(f.name + " must be a positive integer")
		}
		if f.got > f.capped {
			return Budget{}, limitsRejected(f.name + " is above the host limit")
		}
	}
	deadline := now.UTC().Truncate(time.Second).Add(time.Duration(requested.DeadlineSeconds) * time.Second)
	return Budget{Limits: requested, DeadlineAt: protocol.FormatTimestamp(deadline)}, nil
}

// PriorUsage is what earlier Runs of the same Task consumed.
type PriorUsage struct {
	ModelStepsUsed            int64
	GenerationAttemptsUsed    int64
	GenerationAttemptsUnknown int64
}

// ResumeBudget is the budget of a resumed Run: a new explicit budget with fresh
// consumption counters, next to the earlier consumption it must not erase.
type ResumeBudget struct {
	Budget
	New   PriorUsage // the new Run's own consumption: always zero at acceptance
	Prior PriorUsage // the earlier Runs' consumption, carried unchanged
}

// ResolveResumeLimits (F35) is ResolveLimits for run/resume. limits are required in
// full and an expired deadline is never reused: the new deadline counts from the
// acceptance of the resume. The earlier Runs' steps, generation attempts and
// unknown generations are returned beside the new zeros, never merged into them
// and never subtracted from the new limits.
func ResolveResumeLimits(requested, caps protocol.Limits, now time.Time, prior PriorUsage) (ResumeBudget, error) {
	b, err := ResolveLimits(requested, caps, now)
	if err != nil {
		return ResumeBudget{}, err
	}
	return ResumeBudget{Budget: b, Prior: prior}, nil
}
