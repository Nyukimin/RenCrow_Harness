package modelport

import (
	"context"
	"io"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// ModelPort is everything the kernel needs from the model side, and all it may
// call. It is the Gateway's strict contract seen from the Harness: describe a
// binding, count a final request without generating, generate once.
//
// Errors are typed so the kernel can classify them without reading text: a
// *StrictError is a refusal the Gateway gave (and carries the receipt it could
// give), a context error is the caller's cancellation or deadline, and any other
// error is a failure of the connection whose effect on a generation is unknown.
// A port never retries on its own: one Generate call is one generation attempt.
type ModelPort interface {
	// Describe returns the descriptor of the binding: its fingerprint, the
	// generation options of each stage and the recovery profiles it offers.
	Describe(ctx context.Context, binding protocol.Binding) (BindingDescriptor, error)

	// Measure counts the final request exactly as Generate would see it. It does not
	// generate and it spends no generation attempt.
	Measure(ctx context.Context, req MeasureRequest) (MeasureResult, error)

	// Generate sends one request and returns its strict SSE response, which the
	// caller reads to the end and closes. The request carries every expected_*
	// value; a port that finds the binding or a digest changed refuses before
	// generating, with a *StrictError.
	Generate(ctx context.Context, req ChatRequest) (io.ReadCloser, error)
}
