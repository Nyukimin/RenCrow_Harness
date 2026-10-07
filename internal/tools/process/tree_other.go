//go:build !windows && !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package process

import (
	"os"
	"os/exec"
)

// An OS without a process tree adapter runs and stops the single process only; the
// build refuses nothing, but the tree stop is the process itself.
type tree struct{ p *os.Process }

func configure(*exec.Cmd) {}

func attach(p *os.Process) (*tree, error) { return &tree{p: p}, nil }

func (t *tree) terminate() { _ = t.p.Kill() }

func (t *tree) kill() { _ = t.p.Kill() }

func (t *tree) close() {}

// gone: this adapter knows no tree beyond the process, which Run has waited for.
func (t *tree) gone() bool { return true }

func killedByHarness(*os.ProcessState) bool { return false }

func stopTree(pid int, _ string) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
