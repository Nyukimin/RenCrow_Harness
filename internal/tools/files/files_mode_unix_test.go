//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package files

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestEditFilePreservesModeUnderStrictUmask(t *testing.T) {
	if os.Getenv("RENCROW_TEST_EDIT_STRICT_UMASK") == "1" {
		syscall.Umask(0o077)
		sc := workspace(t)
		original := "before\n"
		path := put(t, sc, "edit.txt", original)
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}

		result, err := EditFile(context.Background(), sc, EditArgs{
			Path: "edit.txt", ExpectedHash: sha(original), OldText: "before", NewText: "after",
		})
		if err != nil {
			t.Fatal(err)
		}
		edited, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(edited) != "after\n" || result.OldHash != sha(original) || result.NewHash != sha("after\n") {
			t.Fatalf("edit bytes or hashes are wrong: bytes=%q result=%+v", edited, result)
		}
		if got := info.Mode().Perm(); got != 0o640 {
			t.Fatalf("edit mode = %04o, want 0640", got)
		}

		if _, err := CreateFile(context.Background(), sc, CreateArgs{Path: "created.txt", Text: "created"}); err != nil {
			t.Fatal(err)
		}
		created, err := os.Stat(filepath.Join(sc.Root, "created.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if got := created.Mode().Perm(); got != 0o600 {
			t.Fatalf("create mode = %04o, want umask-filtered 0600", got)
		}
		return
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestEditFilePreservesModeUnderStrictUmask$")
	command.Env = append(os.Environ(), "RENCROW_TEST_EDIT_STRICT_UMASK=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("strict-umask subprocess failed (GOOS=%s): %v\n%s", runtime.GOOS, err, output)
	}
}
