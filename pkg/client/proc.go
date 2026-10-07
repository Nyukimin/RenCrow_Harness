package client

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// processWaitDelay bounds how long Wait may take to close the child's pipes after the
// child exited (a grandchild that holds one open must not hold the client).
const processWaitDelay = 2 * time.Second

// proc is the Harness child process with its two pipes. The pipes are plain files made
// here, not exec's pipes, so that the reader of the output and Wait are independent.
type proc struct {
	cmd    *exec.Cmd
	stdin  *os.File // the write end of the child's standard input
	stdout *os.File // the read end of the child's standard output

	exited  chan struct{} // closed once Wait returned
	waitErr error         // valid after exited is closed

	killOnce sync.Once
}

// startError is a failure to start the child. Its message is fixed: the operating
// system's own message names the paths it was given, and this client does not repeat a
// path. The cause is reachable with errors.Is and errors.As.
type startError struct{ cause error }

func (startError) Error() string   { return "client: cannot start the Harness" }
func (e startError) Unwrap() error { return e.cause }

// startProc starts the child exactly as cfg says.
func startProc(cfg Config) (*proc, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("client: cannot create a pipe: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, fmt.Errorf("client: cannot create a pipe: %w", err)
	}
	cmd := exec.Command(cfg.Binary, "serve", "--stdio", "--config", cfg.ConfigPath)
	cmd.Dir = cfg.Dir
	cmd.Env = cfg.Env // never nil (see Config.validate): the environment is not inherited
	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = cfg.Stderr
	cmd.WaitDelay = processWaitDelay
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{inR, inW, outR, outW} {
			_ = f.Close()
		}
		return nil, startError{err}
	}
	// The child has its ends; the parent keeps the others.
	_ = inR.Close()
	_ = outW.Close()
	p := &proc{cmd: cmd, stdin: inW, stdout: outR, exited: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// kill stops the child at once. It is safe to call any number of times, and after the
// child exited.
func (p *proc) kill() {
	p.killOnce.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	})
}
