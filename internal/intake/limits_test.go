package intake_test

import (
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

var hostCap = protocol.Limits{
	MaxModelSteps: 10, MaxToolCallsPerStep: 8, DeadlineSeconds: 1800, MaxCaptureBytes: 67108864, MaxGenerationAttempts: 32,
}

func TestResolveLimitsFreezesAnExplicitBudget(t *testing.T) {
	now := time.Date(2026, 10, 7, 9, 30, 15, 900_000_000, time.UTC)
	req := protocol.Limits{MaxModelSteps: 4, MaxToolCallsPerStep: 2, DeadlineSeconds: 600, MaxCaptureBytes: 1 << 20, MaxGenerationAttempts: 8}
	got, err := intake.ResolveLimits(req, hostCap, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Limits != req {
		t.Fatalf("effective limits %+v, want exactly the request %+v", got.Limits, req)
	}
	// The deadline counts from acceptance, whole seconds, never rounded up.
	if got.DeadlineAt != "2026-10-07T09:40:15Z" {
		t.Fatalf("deadline_at %s", got.DeadlineAt)
	}
	equal, err := intake.ResolveLimits(hostCap, hostCap, now)
	if err != nil || equal.Limits != hostCap {
		t.Fatalf("limits equal to the cap are allowed: %+v, %v", equal, err)
	}
}

func TestResolveLimitsNeverClamps(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	over := map[string]func(l *protocol.Limits){
		"steps":       func(l *protocol.Limits) { l.MaxModelSteps = hostCap.MaxModelSteps + 1 },
		"tool calls":  func(l *protocol.Limits) { l.MaxToolCallsPerStep = hostCap.MaxToolCallsPerStep + 1 },
		"deadline":    func(l *protocol.Limits) { l.DeadlineSeconds = hostCap.DeadlineSeconds + 1 },
		"capture":     func(l *protocol.Limits) { l.MaxCaptureBytes = hostCap.MaxCaptureBytes + 1 },
		"generations": func(l *protocol.Limits) { l.MaxGenerationAttempts = hostCap.MaxGenerationAttempts + 1 },
	}
	for name, f := range over {
		t.Run("over "+name, func(t *testing.T) {
			req := hostCap
			f(&req)
			got, err := intake.ResolveLimits(req, hostCap, now)
			if protocol.CodeOf(err) != protocol.CodeInvalidLimits {
				t.Fatalf("code %q (%v), want INVALID_LIMITS", protocol.CodeOf(err), err)
			}
			if got.DeadlineAt != "" || got.Limits != (protocol.Limits{}) {
				t.Fatalf("a refused request must not leave a clamped budget: %+v", got)
			}
		})
	}
	// Values that the schema would already refuse must still be refused here: the
	// function is the authority on the budget, not the decoder.
	for name, f := range map[string]func(l *protocol.Limits){
		"zero steps":       func(l *protocol.Limits) { l.MaxModelSteps = 0 },
		"zero tool calls":  func(l *protocol.Limits) { l.MaxToolCallsPerStep = 0 },
		"zero deadline":    func(l *protocol.Limits) { l.DeadlineSeconds = 0 },
		"zero capture":     func(l *protocol.Limits) { l.MaxCaptureBytes = 0 },
		"zero generations": func(l *protocol.Limits) { l.MaxGenerationAttempts = 0 },
		"negative":         func(l *protocol.Limits) { l.MaxModelSteps = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			req := hostCap
			f(&req)
			if _, err := intake.ResolveLimits(req, hostCap, now); protocol.CodeOf(err) != protocol.CodeInvalidLimits {
				t.Fatalf("code %q (%v), want INVALID_LIMITS", protocol.CodeOf(err), err)
			}
		})
	}
	if _, err := intake.ResolveLimits(protocol.Limits{}, hostCap, now); protocol.CodeOf(err) != protocol.CodeInvalidLimits {
		t.Fatal("omitted limits (all zero) must be refused, never defaulted")
	}
}

func TestEffectiveCapsAreTheSmallerOfHostAndPolicy(t *testing.T) {
	policy := protocol.Limits{MaxModelSteps: 20, MaxToolCallsPerStep: 4, DeadlineSeconds: 900, MaxCaptureBytes: 1 << 30, MaxGenerationAttempts: 16}
	got := intake.MinLimits(hostCap, policy)
	want := protocol.Limits{MaxModelSteps: 10, MaxToolCallsPerStep: 4, DeadlineSeconds: 900, MaxCaptureBytes: 67108864, MaxGenerationAttempts: 16}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if intake.MinLimits(policy, hostCap) != got {
		t.Fatal("MinLimits must be symmetric")
	}
}

func TestResolveResumeLimitsStartsANewBudgetAndKeepsThePast(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	prior := intake.PriorUsage{ModelStepsUsed: 7, GenerationAttemptsUsed: 20, GenerationAttemptsUnknown: 2}
	req := protocol.Limits{MaxModelSteps: 5, MaxToolCallsPerStep: 8, DeadlineSeconds: 1800, MaxCaptureBytes: 1 << 20, MaxGenerationAttempts: 10}
	got, err := intake.ResolveResumeLimits(req, hostCap, now, prior)
	if err != nil {
		t.Fatal(err)
	}
	if got.Limits != req || got.DeadlineAt != "2026-10-07T12:30:00Z" {
		t.Fatalf("%+v", got)
	}
	if got.New.ModelStepsUsed != 0 || got.New.GenerationAttemptsUsed != 0 || got.New.GenerationAttemptsUnknown != 0 {
		t.Fatalf("a resumed Run starts from zero consumption: %+v", got.New)
	}
	if got.Prior != prior {
		t.Fatalf("prior consumption must be carried unchanged: %+v", got.Prior)
	}
	// Prior consumption does not shrink the new budget: it is a new Run's own limit.
	if got.Limits.MaxGenerationAttempts != 10 {
		t.Fatal("the new budget is the explicit one")
	}
	// Refusals are the same as for a start.
	over := req
	over.MaxGenerationAttempts = hostCap.MaxGenerationAttempts + 1
	if _, err := intake.ResolveResumeLimits(over, hostCap, now, prior); protocol.CodeOf(err) != protocol.CodeInvalidLimits {
		t.Fatalf("code %q", protocol.CodeOf(err))
	}
	if _, err := intake.ResolveResumeLimits(protocol.Limits{}, hostCap, now, prior); protocol.CodeOf(err) != protocol.CodeInvalidLimits {
		t.Fatal("omitted limits must be refused")
	}
}

func TestResumeNeverReusesAnExpiredDeadline(t *testing.T) {
	// The same request resolved at two acceptance times gets two deadlines.
	req := hostCap
	a, _ := intake.ResolveResumeLimits(req, hostCap, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), intake.PriorUsage{})
	b, _ := intake.ResolveResumeLimits(req, hostCap, time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), intake.PriorUsage{})
	if a.DeadlineAt == b.DeadlineAt || b.DeadlineAt != "2026-10-08T00:30:00Z" {
		t.Fatalf("%s %s", a.DeadlineAt, b.DeadlineAt)
	}
}
