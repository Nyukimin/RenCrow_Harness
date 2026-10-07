package oslock_test

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Nyukimin/RenCrow_Harness/internal/oslock"
)

const (
	helperEnv  = "OSLOCK_TEST_HELPER_PATH"
	helperMode = "OSLOCK_TEST_HELPER_MODE"
)

// TestMain doubles as the child process of the cross-process tests: with the
// helper variables set it takes the lock, announces it and holds it until killed.
func TestMain(m *testing.M) {
	if path := os.Getenv(helperEnv); path != "" {
		mode := oslock.Exclusive
		if os.Getenv(helperMode) == "shared" {
			mode = oslock.Shared
		}
		l, err := oslock.TryLock(path, mode)
		if err != nil {
			os.Stdout.WriteString("error\n")
			os.Exit(2)
		}
		_ = l
		os.Stdout.WriteString("locked\n")
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "x.lock")
}

func TestExclusiveExcludesEverything(t *testing.T) {
	p := lockPath(t)
	a, err := oslock.TryLock(p, oslock.Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oslock.TryLock(p, oslock.Exclusive); !errors.Is(err, oslock.ErrLocked) {
		t.Fatalf("second exclusive lock: %v", err)
	}
	if _, err := oslock.TryLock(p, oslock.Shared); !errors.Is(err, oslock.ErrLocked) {
		t.Fatalf("shared lock under an exclusive one: %v", err)
	}
	if err := a.Release(); err != nil {
		t.Fatal(err)
	}
	b, err := oslock.TryLock(p, oslock.Exclusive)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = b.Release()
}

func TestSharedLocksCoexistAndBlockExclusive(t *testing.T) {
	p := lockPath(t)
	a, err := oslock.TryLock(p, oslock.Shared)
	if err != nil {
		t.Fatal(err)
	}
	b, err := oslock.TryLock(p, oslock.Shared)
	if err != nil {
		t.Fatalf("second shared lock: %v", err)
	}
	if _, err := oslock.TryLock(p, oslock.Exclusive); !errors.Is(err, oslock.ErrLocked) {
		t.Fatalf("exclusive lock under shared ones: %v", err)
	}
	_ = a.Release()
	if _, err := oslock.TryLock(p, oslock.Exclusive); !errors.Is(err, oslock.ErrLocked) {
		t.Fatalf("exclusive lock while one shared lock remains: %v", err)
	}
	_ = b.Release()
	c, err := oslock.TryLock(p, oslock.Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Release()
}

func TestReleaseIsIdempotent(t *testing.T) {
	l, err := oslock.TryLock(lockPath(t), oslock.Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
}

func TestLockFileIsOwnerOnlyAndContentIsNotOwnership(t *testing.T) {
	p := lockPath(t)
	// A stale file that names a (dead) PID and an old time: the content is never
	// read, so it cannot make anyone the owner or keep the lock held.
	if err := os.WriteFile(p, []byte("pid=1 started=1970-01-01T00:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := oslock.TryLock(p, oslock.Exclusive)
	if err != nil {
		t.Fatalf("a stale lock file with old content blocked the lock: %v", err)
	}
	defer l.Release()
	if info, err := os.Stat(p); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("lock file mode %v %v", info.Mode(), err)
	}
}

func TestRefusesNonRegularLockPath(t *testing.T) {
	dir := t.TempDir()
	if _, err := oslock.TryLock(dir, oslock.Exclusive); err == nil || errors.Is(err, oslock.ErrLocked) {
		t.Fatalf("a directory was accepted as a lock file: %v", err)
	}
	real := filepath.Join(dir, "real.lock")
	if err := os.WriteFile(real, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.lock")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symbolic links are not available")
	}
	if _, err := oslock.TryLock(link, oslock.Exclusive); err == nil || errors.Is(err, oslock.ErrLocked) {
		t.Fatalf("a symbolic link was accepted as a lock file: %v", err)
	}
	if _, err := oslock.TryLock("relative.lock", oslock.Exclusive); err == nil {
		t.Fatal("a relative path was accepted")
	}
}

func startHolder(t *testing.T, path, mode string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"="+path, helperMode+"="+mode)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("helper did not take the lock: %q %v", line, err)
	}
	return cmd
}

func TestAnotherProcessHoldsTheLock(t *testing.T) {
	p := lockPath(t)
	holder := startHolder(t, p, "exclusive")
	if _, err := oslock.TryLock(p, oslock.Exclusive); !errors.Is(err, oslock.ErrLocked) {
		t.Fatalf("lock held by another process was granted: %v", err)
	}
	if _, err := oslock.TryLock(p, oslock.Shared); !errors.Is(err, oslock.ErrLocked) {
		t.Fatalf("shared lock under another process's exclusive lock: %v", err)
	}
	// The holder dies without releasing: the OS drops the lock, nothing else does.
	_ = holder.Process.Kill()
	_ = holder.Wait()
	var l *oslock.Lock
	var err error
	for i := 0; i < 50; i++ {
		if l, err = oslock.TryLock(p, oslock.Exclusive); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("lock of a killed process was never released: %v", err)
	}
	_ = l.Release()
}

func TestSharedLockAcrossProcesses(t *testing.T) {
	p := lockPath(t)
	startHolder(t, p, "shared")
	l, err := oslock.TryLock(p, oslock.Shared)
	if err != nil {
		t.Fatalf("shared lock beside another process's shared lock: %v", err)
	}
	_ = l.Release()
	if _, err := oslock.TryLock(p, oslock.Exclusive); !errors.Is(err, oslock.ErrLocked) {
		t.Fatalf("exclusive lock beside another process's shared lock: %v", err)
	}
}
