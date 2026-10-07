//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package process

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// tree is the process group the child leads. The child is started as the leader of a
// new group, so every descendant that does not move to another group is reachable by
// signalling the group, and nothing outside it is.
type tree struct{ pgid int }

func configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func attach(p *os.Process) (*tree, error) { return &tree{pgid: p.Pid}, nil }

func (t *tree) terminate() { _ = syscall.Kill(-t.pgid, syscall.SIGTERM) }

func (t *tree) kill() { _ = syscall.Kill(-t.pgid, syscall.SIGKILL) }

func (t *tree) close() {}

// gone reports whether the group has no process left: signal 0 finds nobody. A process
// that was killed and not yet reaped still counts, so a group is gone only when its
// members have really ended.
func (t *tree) gone() bool { return errors.Is(syscall.Kill(-t.pgid, 0), syscall.ESRCH) }

// killedByHarness: on a unix system a process the Harness killed ended by a signal,
// which ProcessState already says.
func killedByHarness(*os.ProcessState) bool { return false }

// stopTree stops a process by PID, but only while its start time is still the one the
// caller matched (the check is repeated here, right before the signal, so a PID that
// was freed and reused in between is not signalled). It takes the whole group when the
// process leads one, and only the process otherwise.
func stopTree(pid int, start string) error {
	if pid <= 1 {
		return errors.New("process: refusing to signal this PID")
	}
	if now, err := startToken(pid); err != nil || now != start {
		return ErrNoSuchProcess
	}
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		return syscall.Kill(-pid, syscall.SIGKILL)
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}
