package tools

import (
	"context"

	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/process"
)

// VerdictNotApplicable is the verdict of an Attempt that has no process to check.
const VerdictNotApplicable = "not_applicable"

// Reconciler checks, for the Tool Attempts a take-over finds unresolved, whether the
// process each started is still there, and stops it if (and only if) it is the same
// process. It decides nothing about the effect: every such Attempt stays unknown.
type Reconciler struct{ prober process.Prober }

// NewReconciler builds a Reconciler over a host prober; nil is the running OS.
func NewReconciler(p process.Prober) *Reconciler {
	if p == nil {
		p = process.OSProber{}
	}
	return &Reconciler{prober: p}
}

// Reconcile (F29) returns, per Attempt ID, the verdict of the host check of its
// process. Only process.exec Attempts have one; for the others the verdict is
// not_applicable. The check runs here, before the transaction that settles the Run,
// and never inside it.
func (r *Reconciler) Reconcile(_ context.Context, attempts []sqlite.ToolAttemptRef) map[string]string {
	out := make(map[string]string, len(attempts))
	for _, a := range attempts {
		if a.Tool != ProcessExec {
			out[a.AttemptID] = VerdictNotApplicable
			continue
		}
		id, err := process.DecodeIdentity(a.ProcessToken)
		if err != nil || id.Incarnation != a.HostIncarnation {
			out[a.AttemptID] = process.VerdictUnverifiable
			continue
		}
		out[a.AttemptID] = process.Reconcile(r.prober, id)
	}
	return out
}
