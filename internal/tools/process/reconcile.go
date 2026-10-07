package process

import "errors"

// Prober is what Reconcile asks the host. The OS implements it (OSProber); a test
// replaces it to stage a rebooted host or a reused PID.
type Prober interface {
	// HostIncarnation identifies this boot of the host.
	HostIncarnation() (string, error)
	// StartToken is the start time token of a live process, or ErrNoSuchProcess.
	StartToken(pid int) (string, error)
	// StopTree stops the process and the tree it leads, but only if its start time is
	// still start: a PID reused since is not stopped.
	StopTree(pid int, start string) error
}

// Verdicts of Reconcile. None of them says what the process did: an attempt whose
// process cannot be shown to have finished is still unknown, and the command is never
// run again to find out.
const (
	// VerdictGone: the recorded process no longer exists (the host was restarted, the PID
	// is free, or the PID now belongs to a process that started later). Nothing was
	// signalled.
	VerdictGone = "gone"
	// VerdictStopped: the recorded process was still running and its tree was stopped.
	VerdictStopped = "stopped"
	// VerdictStopFailed: the recorded process is still running and could not be stopped.
	VerdictStopFailed = "stop_failed"
	// VerdictUnverifiable: the identity cannot be checked (nothing was recorded, or the OS
	// cannot read it). Nothing was signalled, because a PID alone is not a process.
	VerdictUnverifiable = "unverifiable"
)

// Reconcile (F29) decides, from the identity recorded when the process started,
// whether a process found today is that process, and stops it only if it is. It
// signals a PID only after the host incarnation, the PID's start time and the token
// all agree; a PID that was reused by an unrelated process, or one that cannot be
// matched, is left alone.
func Reconcile(p Prober, id Identity) string {
	if id.PID <= 1 || id.Start == "" || id.Incarnation == "" || id.Nonce == "" {
		return VerdictUnverifiable
	}
	now, err := p.HostIncarnation()
	if err != nil || now == "" {
		return VerdictUnverifiable
	}
	if now != id.Incarnation {
		return VerdictGone // another boot: its processes did not survive
	}
	start, err := p.StartToken(id.PID)
	switch {
	case errors.Is(err, ErrNoSuchProcess):
		return VerdictGone
	case err != nil || start == "":
		return VerdictUnverifiable
	case start != id.Start:
		return VerdictGone // the PID was reused by a process that started later
	}
	if err := p.StopTree(id.PID, id.Start); err != nil {
		if errors.Is(err, ErrNoSuchProcess) {
			return VerdictGone
		}
		// It may have ended in the meantime; look again before saying it did not stop.
		if _, perr := p.StartToken(id.PID); errors.Is(perr, ErrNoSuchProcess) {
			return VerdictStopped
		}
		return VerdictStopFailed
	}
	return VerdictStopped
}
