//go:build windows

package process

import (
	"errors"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobKillExitCode is the exit code the Harness gives the processes of a job it ends.
// A forced termination is otherwise an ordinary exit code on Windows, and a program
// that exits by itself with 1 must not look like one that was stopped.
const jobKillExitCode = 0xC1A05EED

// tree is a Job Object with the child assigned to it. Closing the job's last handle
// ends every process in it (KILL_ON_JOB_CLOSE), so a Harness that dies takes its
// children with it. A Job Object is a way to stop a process tree, not a sandbox: it
// confines neither files nor network.
type tree struct{ job windows.Handle }

func configure(*exec.Cmd) {}

// attach puts the started process into a new Job Object. A child that spawns another
// process in the instant before it is assigned is not covered, and neither are the
// descendants of such a child; that window is the price of starting without a
// suspended main thread.
func attach(p *os.Process) (*tree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &tree{job: job}, nil
}

func (t *tree) terminate() { t.kill() }

func (t *tree) kill() { _ = windows.TerminateJobObject(t.job, jobKillExitCode) }

func (t *tree) close() { _ = windows.CloseHandle(t.job) }

// jobAccounting is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION (golang.org/x/sys/windows
// names the information class but not the structure).
type jobAccounting struct {
	TotalUserTime, TotalKernelTime                     int64
	ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
	TotalPageFaultCount, TotalProcesses                uint32
	ActiveProcesses, TotalTerminatedProcesses          uint32
}

// gone reports whether the job has no process left. A job that cannot be asked is not
// shown to be empty.
func (t *tree) gone() bool {
	var info jobAccounting
	if err := windows.QueryInformationJobObject(t.job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return false
	}
	return info.ActiveProcesses == 0
}

// killedByHarness reports a process that ended with the code the Harness gives the
// processes it terminates.
func killedByHarness(ps *os.ProcessState) bool { return ps.ExitCode() == int(jobKillExitCode) }

// stopTree ends one process by PID, but only if, on the very handle it terminates, the
// process is still running and was created at the instant the caller matched: the
// check and the termination are made on one handle, so a PID that is reused in between
// cannot be reached. A process whose Job Object was already closed has no children
// left to reach.
func stopTree(pid int, start string) error {
	if pid <= 4 {
		return errors.New("process: refusing to terminate this PID")
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return ErrNoSuchProcess
	}
	defer windows.CloseHandle(h)
	tok, active, err := handleStart(h)
	if err != nil || !active || tok != start {
		return ErrNoSuchProcess
	}
	return windows.TerminateProcess(h, jobKillExitCode)
}
