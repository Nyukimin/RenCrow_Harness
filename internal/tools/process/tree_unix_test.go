//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package process

import (
	"os/exec"
	"testing"
	"time"
)

// TestWaitGoneSaysWhetherAnyoneIsLeftInTheGroup is the primitive under "stopped is shown,
// not assumed": a group with a live member is not gone, however long it is waited for
// (within the limit), and is gone once its members have ended and been reaped.
func TestWaitGoneSaysWhetherAnyoneIsLeftInTheGroup(t *testing.T) {
	cmd := exec.Command(helperExe(t), "-test.run=TestHelperProcess", "--", "sleep")
	cmd.Env = append(cmd.Environ(), helperEnv...)
	configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tr, err := attach(cmd.Process)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { tr.kill(); _ = cmd.Wait() }()
	begin := time.Now()
	if waitGone(tr, 150*time.Millisecond) {
		t.Fatal("a group with a live member was shown to be gone")
	}
	if time.Since(begin) < 140*time.Millisecond {
		t.Fatal("the wait did not last as long as it was given")
	}
	tr.kill()
	_ = cmd.Wait() // reaped: the leader is not a zombie any more
	if !waitGone(tr, 2*time.Second) {
		t.Fatal("a group whose members ended was not shown to be gone")
	}
}
