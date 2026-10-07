package process

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolerr"
)

// Defaults of a run.
const (
	// DefaultGrace is how long the tree gets, after a polite stop request, to end
	// before it is killed (unix only: a Job Object has no polite request).
	DefaultGrace = 2 * time.Second
	// pipeDrain is how long, after the tree is gone, the output pipes are still read.
	pipeDrain = 2 * time.Second
	// settle bounds the wait for a killed process to be reaped.
	settle = 10 * time.Second
	// goneWait bounds the wait, once the tree was stopped, for the tree to be shown
	// empty (see tree.gone).
	goneWait = 2 * time.Second
	readSize = 32 << 10
)

// Spec is one process to run.
type Spec struct {
	Executable string
	Argv       []string
	// Dir is the absolute working directory.
	Dir string
	// Env is the child's entire environment (see BuildEnv). It must not be nil.
	Env     []string
	Timeout time.Duration
	// CaptureLimit is the most output bytes, stdout and stderr together, that reach the
	// sinks. Reaching it stops the tree and marks the capture incomplete.
	CaptureLimit int64
	Stdout       io.Writer
	Stderr       io.Writer
	// Grace is the polite-stop period; zero means DefaultGrace.
	Grace time.Duration
	// OnStart is called once, right after the process exists, with its identity. If it
	// fails the tree is stopped and Run returns its error.
	OnStart func(Identity) error
}

// Result is how a run ended. A Result with Started false means no process was created.
type Result struct {
	Started bool
	// ExitCode is set only when the process exited by itself with a code.
	ExitCode *int
	// Signaled is set when the process was ended by a signal (or, on Windows, by a
	// forced termination), by the Harness or not.
	Signaled       bool
	TimedOut       bool
	Cancelled      bool
	CaptureLimited bool
	StdoutBytes    int64
	StderrBytes    int64
}

// CaptureComplete says every byte the child wrote reached the sinks. It is false
// whenever the limit was reached, whatever else happened.
func (r Result) CaptureComplete() bool { return !r.CaptureLimited }

// budget is the capture allowance two streams share.
type budget struct {
	mu      sync.Mutex
	left    int64
	limited atomic.Bool
	onLimit func()
}

// take returns how many of n bytes may be kept, and trips the limit when fewer.
func (b *budget) take(n int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	keep := n
	if int64(keep) > b.left {
		keep = int(b.left)
	}
	b.left -= int64(keep)
	if keep < n && !b.limited.Swap(true) {
		go b.onLimit()
	}
	return keep
}

// pump copies one pipe to its sink under the shared budget. After the limit the pipe
// is still drained (and discarded) so the child is not stuck on a full pipe while the
// tree is being stopped.
func pump(r io.Reader, w io.Writer, b *budget, count *int64, done chan<- error) {
	buf := make([]byte, readSize)
	var werr error
	for {
		n, err := r.Read(buf)
		if n > 0 && werr == nil {
			if keep := b.take(n); keep > 0 {
				if _, e := w.Write(buf[:keep]); e != nil {
					werr = e
				} else {
					atomic.AddInt64(count, int64(keep))
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				err = nil
			}
			if werr != nil {
				err = werr
			}
			done <- err
			return
		}
	}
}

// Run starts the process and waits for it, or for the timeout, the context or the
// capture limit, whichever comes first, then stops what is left of the tree. It never
// goes through a shell: Executable is started directly with Argv as given.
func Run(ctx context.Context, s Spec) (Result, error) {
	var res Result
	if s.Env == nil {
		return res, toolerr.Reject(toolerr.CodeEnvUnknown, "a process needs an explicit environment")
	}
	if s.Timeout <= 0 || s.CaptureLimit < 0 || s.Stdout == nil || s.Stderr == nil {
		return res, toolerr.Reject(toolerr.CodePolicyRejected, "a process needs a timeout, a capture limit and output sinks")
	}
	if err := ctx.Err(); err != nil {
		return res, toolerr.Fail(toolerr.CodeCancelled, "the run was stopped before the process started")
	}
	grace := s.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}

	outR, outW, err := os.Pipe()
	if err != nil {
		return res, toolerr.Fail(toolerr.CodeStartFailed, "the output pipes could not be made")
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_, _ = outR.Close(), outW.Close()
		return res, toolerr.Fail(toolerr.CodeStartFailed, "the output pipes could not be made")
	}
	closeAll := func() { _, _, _, _ = outR.Close(), outW.Close(), errR.Close(), errW.Close() }

	cmd := exec.Command(s.Executable, s.Argv...)
	cmd.Dir, cmd.Env = s.Dir, s.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, outW, errW
	configure(cmd)
	if err := cmd.Start(); err != nil {
		closeAll()
		return res, toolerr.Fail(toolerr.CodeStartFailed, "the process could not be started")
	}
	_, _ = outW.Close(), errW.Close() // the child holds its own ends now
	res.Started = true

	tree, terr := attach(cmd.Process)
	exited := make(chan *os.ProcessState, 1)
	go func() {
		ps, _ := cmd.Process.Wait()
		exited <- ps
	}()
	stopTree := func() {
		if tree != nil {
			tree.terminate()
		} else {
			_ = cmd.Process.Kill()
		}
	}
	// killTree takes the tree and the process itself: a child that left its group (or
	// its job) is still the process this run started.
	killTree := func() {
		if tree != nil {
			tree.kill()
		}
		_ = cmd.Process.Kill()
	}

	b := &budget{left: s.CaptureLimit}
	limitHit := make(chan struct{})
	var limitOnce sync.Once
	b.onLimit = func() { limitOnce.Do(func() { close(limitHit) }) }
	pumped := make(chan error, 2)
	go pump(outR, s.Stdout, b, &res.StdoutBytes, pumped)
	go pump(errR, s.Stderr, b, &res.StderrBytes, pumped)

	// stopping is set when the Harness itself had to end the process: a record that
	// failed, the timeout, the context, the capture limit. A process that exited by itself
	// is not asked whether its tree is gone (what it leaves behind is killed below, as
	// before); one the Harness stopped must be shown to be gone, or its effect is unknown.
	stopping := false
	var startErr error
	if terr != nil {
		startErr = toolerr.Uncertain(toolerr.CodeStartFailed, "the process started but its tree could not be tracked")
	} else if s.OnStart != nil {
		if ierr := s.OnStart(identify(cmd.Process.Pid)); ierr != nil {
			startErr = toolerr.Uncertain(toolerr.CodeStartFailed, "the process started but could not be recorded")
		}
	}

	timer := time.NewTimer(s.Timeout)
	defer timer.Stop()
	var ps *os.ProcessState
	if startErr != nil {
		stopping = true
		killTree()
		select {
		case ps = <-exited:
		case <-time.After(settle):
		}
	} else {
		select {
		case ps = <-exited:
		case <-timer.C:
			res.TimedOut = true
		case <-ctx.Done():
			res.Cancelled = true
		case <-limitHit:
		}
		if ps == nil {
			stopping = true
			stopTree()
			select {
			case ps = <-exited:
			case <-time.After(grace):
				killTree()
				select {
				case ps = <-exited:
				case <-time.After(settle):
				}
			}
		}
	}
	// Whatever ended the process, nothing of its tree is left running.
	killTree()
	if tree != nil {
		defer tree.close()
	}
	treeGone := true
	if stopping && tree != nil {
		treeGone = shownGone(tree, goneWait)
	}

	// Read what the pipes still hold; a descendant that kept a pipe open past the kill
	// does not hold the run for ever.
	var perr error
	got := 0
	drained := time.After(pipeDrain)
	for got < 2 {
		select {
		case e := <-pumped:
			perr = errors.Join(perr, e)
			got++
		case <-drained:
			_, _ = outR.Close(), errR.Close()
			drained = nil
			for ; got < 2; got++ {
				<-pumped // a closed pipe ends its reader at once
			}
		}
	}
	closeAll()

	res.CaptureLimited = b.limited.Load()
	if ps != nil {
		if code := ps.ExitCode(); code >= 0 && ps.Exited() && !killedByHarness(ps) {
			res.ExitCode = &code
		} else {
			res.Signaled = true
		}
	}
	switch {
	case startErr != nil:
		return res, startErr
	case ps == nil:
		// Not even a kill brought it to an end that could be observed: what it did, and
		// whether it is still doing it, is not known, and is not reported as anything else.
		return res, toolerr.Uncertain(toolerr.CodeIO, "the process could not be shown to have stopped")
	case !treeGone:
		// The Harness ended the process and the tree it leads was still not empty after
		// it was killed (a process that does not die, or one nobody has reaped): whether
		// anything of the command is still running is not known.
		return res, toolerr.Uncertain(toolerr.CodeIO, "the process tree could not be shown to have stopped")
	case perr != nil:
		return res, toolerr.Uncertain(toolerr.CodeIO, "the output could not be stored")
	}
	return res, nil
}

// shownGone is how Run asks whether a tree it stopped is gone; a test replaces it to
// stage a tree that cannot be shown to be.
var shownGone = waitGone

// waitGone waits up to limit for the tree to be empty.
func waitGone(t *tree, limit time.Duration) bool {
	stop := time.Now().Add(limit)
	for {
		if t.gone() {
			return true
		}
		if !time.Now().Before(stop) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
