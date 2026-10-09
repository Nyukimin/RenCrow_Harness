package sourcecandidate

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
)

func TestCaptureExcludesOnlyRootGitAndTmpAndIncludesIgnoredNestedTmp(t *testing.T) {
	root := newGitSource(t)
	writeSourceFile(t, root, ".gitignore", "ignored.txt\n")
	writeSourceFile(t, root, "ignored.txt", "included despite ignore\n")
	writeSourceFile(t, root, ".git/private-marker", "root git excluded\n")
	writeSourceFile(t, root, "Tmp/private-marker", "root Tmp excluded\n")
	writeSourceFile(t, root, "internal/Tmp/nested.txt", "nested Tmp included\n")
	writeSourceFile(t, root, "internal/.git/config", "nested git included\n")
	writeSourceFile(t, root, "tracked.go", "package sample\n")
	runGit(t, root, "add", ".gitignore", "tracked.go")

	workspace, candidate, manifestPath := candidatePaths(t, root)
	manifest, err := Capture(root, filepath.Join(root, "Tmp", "test-runtime", "_runs"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Status != StatusCandidate {
		t.Fatalf("status = %q, want %q", manifest.Status, StatusCandidate)
	}
	if _, err := os.Stat(filepath.Join(candidate, "ignored.txt")); err != nil {
		t.Fatalf("ignored source file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(candidate, "internal", "Tmp", "nested.txt")); err != nil {
		t.Fatalf("nested Tmp source missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(candidate, "internal", ".git", "config")); err != nil {
		t.Fatalf("nested .git source missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(candidate, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root .git was copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(candidate, "Tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root Tmp was copied: %v", err)
	}
	if _, err := Verify(filepath.Join(root, "Tmp", "test-runtime", "_runs"), workspace); err != nil {
		t.Fatalf("fresh candidate did not verify: %v", err)
	}

	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), root) {
		t.Fatal("manifest contains the source's absolute path")
	}
	var decoded Manifest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.CandidateID != manifest.CandidateID {
		t.Fatalf("candidate id = %q, want %q", decoded.CandidateID, manifest.CandidateID)
	}
}

func TestCaptureRefusesTrackedRootTmpContent(t *testing.T) {
	root := newGitSource(t)
	relative := "Tmp/tracked.txt"
	writeSourceFile(t, root, relative, "must not be silently omitted\n")
	runGit(t, root, "add", "-f", relative)
	override := t.TempDir()
	t.Setenv("GIT_DIR", filepath.Join(override, "not-a-repository"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(override, "empty-index"))
	workspace, _, manifestPath := candidatePaths(t, root)
	err := captureErr(root, workspace)
	if err == nil || !strings.Contains(err.Error(), "tracked root Tmp") {
		t.Fatalf("Capture error = %v, want tracked root Tmp rejection", err)
	}
	if _, err := os.Stat(manifestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed capture wrote a manifest: %v", err)
	}
}

func TestCaptureRequiresTheGitRepositoryRoot(t *testing.T) {
	root := newGitSource(t)
	subdirectory := filepath.Join(root, "nested")
	if err := os.Mkdir(subdirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(subdirectory, "Tmp", "test-runtime", "_runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(subdirectory, "Tmp", "test-runtime", "_runs", "source-work")
	err := captureErrWithRuns(subdirectory, filepath.Join(subdirectory, "Tmp", "test-runtime", "_runs"), workspace)
	if err == nil || !strings.Contains(err.Error(), "Git repository root") {
		t.Fatalf("Capture error = %v, want Git repository root rejection", err)
	}
}

func TestCaptureRefusesSiblingNamesThatCollideAcrossOperatingSystems(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("case-only sibling names cannot coexist on Windows")
	}
	root := newGitSource(t)
	writeSourceFile(t, root, "Settings/first.go", "package sample\n")
	writeSourceFile(t, root, "settings/second.go", "package sample\n")
	workspace, _, _ := candidatePaths(t, root)
	err := captureErr(root, workspace)
	if err == nil || !strings.Contains(err.Error(), "collide across operating systems") {
		t.Fatalf("Capture error = %v, want portable name collision rejection", err)
	}
}

func TestCaptureRejectsCaseFoldedRootExclusionAliases(t *testing.T) {
	for _, name := range []string{"tmp", "TMP", ".GiT"} {
		t.Run(name, func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("case-only root names cannot coexist with the canonical excluded roots on Windows")
			}
			root := newGitSource(t)
			writeSourceFile(t, root, filepath.ToSlash(filepath.Join(name, "marker.txt")), "must not be omitted\n")
			workspace, _, _ := candidatePaths(t, root)
			err := captureErr(root, workspace)
			if err == nil || !strings.Contains(err.Error(), "ambiguous root exclusion name") {
				t.Fatalf("Capture error = %v, want portable root exclusion ambiguity rejection", err)
			}
		})
	}
}

func TestRootExclusionAliasesAreCaseSensitiveAndPortable(t *testing.T) {
	for _, test := range []struct {
		path string
		want bool
	}{
		{path: ".git", want: false},
		{path: "Tmp", want: false},
		{path: "tmp", want: true},
		{path: "TMP", want: true},
		{path: ".GiT", want: true},
		{path: "nested/tmp", want: false},
		{path: "nested/.GiT", want: false},
	} {
		t.Run(test.path, func(t *testing.T) {
			if got := isRootExclusionAlias(test.path); got != test.want {
				t.Fatalf("isRootExclusionAlias(%q) = %t, want %t", test.path, got, test.want)
			}
		})
	}
}

func TestTrackedRootExclusionAliasesFailClosedBeforeFilesystemTraversal(t *testing.T) {
	for _, test := range []struct {
		path string
		want string
	}{
		{path: "tmp/tracked.txt", want: "ambiguous root exclusion name"},
		{path: "TMP/tracked.txt", want: "ambiguous root exclusion name"},
		{path: ".GiT/config", want: "ambiguous root exclusion name"},
		{path: "Tmp/tracked.txt", want: "tracked root Tmp"},
		{path: "nested/Tmp/tracked.txt"},
		{path: ".git/config"},
	} {
		t.Run(test.path, func(t *testing.T) {
			err := validateTrackedRootPath(test.path)
			if test.want == "" {
				if err != nil {
					t.Fatalf("validateTrackedRootPath(%q) = %v, want no rejection", test.path, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateTrackedRootPath(%q) = %v, want %q rejection", test.path, err, test.want)
			}
		})
	}
}

func TestCaptureRejectsSpecialPermissionBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix special permission bits")
	}
	for _, test := range []struct {
		name string
		path string
		mode os.FileMode
	}{
		{name: "setuid", path: "setuid.txt", mode: os.ModeSetuid | 0o755},
		{name: "setgid", path: "setgid.txt", mode: os.ModeSetgid | 0o755},
		{name: "sticky-directory", path: "sticky", mode: os.ModeSticky | 0o755},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := newGitSource(t)
			path := filepath.Join(root, filepath.FromSlash(test.path))
			if test.name == "sticky-directory" {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				writeSourceFile(t, root, test.path, "content\n")
			}
			if err := os.Chmod(path, test.mode); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			bit := os.ModeSetuid | os.ModeSetgid | os.ModeSticky
			if info.Mode()&bit == 0 {
				t.Skip("filesystem does not preserve the requested special permission bit")
			}
			workspace, _, _ := candidatePaths(t, root)
			err = captureErr(root, workspace)
			if err == nil || !strings.Contains(err.Error(), "non-portable permission mode") {
				t.Fatalf("Capture error = %v, want special permission bit rejection", err)
			}
		})
	}
}

func TestVerifyRejectsSpecialPermissionBitAddedToCandidate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix special permission bits")
	}
	root := newGitSource(t)
	writeSourceFile(t, root, "source.txt", "content\n")
	workspace, candidate, _ := candidatePaths(t, root)
	if _, err := Capture(root, filepath.Join(root, "Tmp", "test-runtime", "_runs"), workspace); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(candidate, "source.txt")
	if err := os.Chmod(path, os.ModeSetuid|0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Skip("filesystem does not preserve setuid on a candidate file")
	}
	if _, err := Verify(filepath.Join(root, "Tmp", "test-runtime", "_runs"), workspace); err == nil || !strings.Contains(err.Error(), "non-portable permission mode") {
		t.Fatalf("Verify error = %v, want candidate special permission bit rejection", err)
	}
}

func TestCaptureUsesGitIndexModeAndRecordsUnixWorkingModeDrift(t *testing.T) {
	root := newGitSource(t)
	writeSourceFile(t, root, "internal/run-tool.sh", "#!/bin/sh\nexit 0\n")
	writeSourceFile(t, root, "internal/dirty-mode.sh", "#!/bin/sh\nexit 0\n")
	runGit(t, root, "add", "internal/run-tool.sh")
	runGit(t, root, "update-index", "--chmod=+x", "--", "internal/run-tool.sh")
	runGit(t, root, "add", "internal/dirty-mode.sh")
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(root, "internal", "dirty-mode.sh"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	workspace, _, _ := candidatePaths(t, root)
	manifest, err := Capture(root, filepath.Join(root, "Tmp", "test-runtime", "_runs"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := findManifestEntry(manifest, "internal/run-tool.sh")
	if !ok {
		t.Fatal("tracked executable is missing from the candidate manifest")
	}
	if entry.PortableMode != "0755" || entry.ModeSource != "git-index" {
		t.Fatalf("tracked executable mode = %#v, want Git index mode 0755", entry)
	}
	if runtime.GOOS == "windows" {
		if entry.ModeDirty || entry.WorkingMode != "" {
			t.Fatalf("Windows mode observation was incorrectly treated as executable intent: %#v", entry)
		}
	} else if !entry.ModeDirty || entry.WorkingMode != "0644" {
		t.Fatalf("Unix working-tree drift was not recorded: %#v", entry)
	}
	dirtyEntry, ok := findManifestEntry(manifest, "internal/dirty-mode.sh")
	if !ok || dirtyEntry.PortableMode != "0644" || dirtyEntry.ModeSource != "git-index" {
		t.Fatalf("tracked non-executable mode was not sourced from Git: %#v", dirtyEntry)
	}
	if runtime.GOOS == "windows" {
		if dirtyEntry.ModeDirty || dirtyEntry.WorkingMode != "" {
			t.Fatalf("Windows permissions were incorrectly treated as executable intent: %#v", dirtyEntry)
		}
	} else if !dirtyEntry.ModeDirty || dirtyEntry.WorkingMode != "0755" {
		t.Fatalf("Unix executable worktree drift was not recorded: %#v", dirtyEntry)
	}
	if _, err := Verify(filepath.Join(root, "Tmp", "test-runtime", "_runs"), workspace); err != nil {
		t.Fatalf("candidate with Git-owned mode did not verify: %v", err)
	}
}

func TestCaptureUsesPlatformModeForUntrackedFiles(t *testing.T) {
	root := newGitSource(t)
	writeSourceFile(t, root, "untracked-tool.sh", "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(root, "untracked-tool.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace, _, _ := candidatePaths(t, root)
	manifest, err := Capture(root, filepath.Join(root, "Tmp", "test-runtime", "_runs"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := findManifestEntry(manifest, "untracked-tool.sh")
	if !ok {
		t.Fatal("untracked file is missing from the candidate manifest")
	}
	wantMode, wantSource := "0755", "working-tree"
	if runtime.GOOS == "windows" {
		wantMode, wantSource = "0644", "platform-default"
	}
	if entry.PortableMode != wantMode || entry.ModeSource != wantSource || entry.ModeDirty || entry.WorkingMode != "" {
		t.Fatalf("untracked mode = %#v, want mode %s from %s", entry, wantMode, wantSource)
	}
}

func TestGitIndexModeComparisonRejectsChanges(t *testing.T) {
	before := map[string]string{"tool.sh": "100644", "main.go": "100644"}
	after := map[string]string{"tool.sh": "100755", "main.go": "100644"}
	if sameGitModes(before, after) {
		t.Fatal("Git index executable-mode change was accepted")
	}
	if !sameGitModes(before, map[string]string{"main.go": "100644", "tool.sh": "100644"}) {
		t.Fatal("Git index mode comparison depends on map iteration order")
	}
}

func findManifestEntry(manifest Manifest, path string) (Entry, bool) {
	for _, entry := range manifest.Entries {
		if entry.Path == path {
			return entry, true
		}
	}
	return Entry{}, false
}

func TestCaptureRefusesSymlinkSource(t *testing.T) {
	root := newGitSource(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	workspace, _, _ := candidatePaths(t, root)
	err := captureErr(root, workspace)
	if err == nil || !strings.Contains(err.Error(), "symlink or reparse point") {
		t.Fatalf("Capture error = %v, want symlink rejection", err)
	}
}

func TestVerifyRejectsCandidateMutation(t *testing.T) {
	root := newGitSource(t)
	writeSourceFile(t, root, "source.txt", "before\n")
	workspace, candidate, _ := candidatePaths(t, root)
	if _, err := Capture(root, filepath.Join(root, "Tmp", "test-runtime", "_runs"), workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "source.txt"), []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(filepath.Join(root, "Tmp", "test-runtime", "_runs"), workspace); err == nil {
		t.Fatal("Verify accepted a changed candidate")
	}
}

func TestCaptureRequiresFreshDestinationInsideOwnerRuns(t *testing.T) {
	root := newGitSource(t)
	writeSourceFile(t, root, "source.txt", "source\n")
	workspace, _, _ := candidatePaths(t, root)
	if err := fsperm.CreatePrivateDir(workspace); err != nil {
		t.Fatal(err)
	}
	err := captureErr(root, workspace)
	if err == nil || !strings.Contains(err.Error(), "source workspace must be fresh") {
		t.Fatalf("Capture error = %v, want fresh destination rejection", err)
	}
}

func TestPrepareLogDirectoryCreatesPrivateContainedEvidenceDirectory(t *testing.T) {
	root := newGitSource(t)
	runs := filepath.Join(root, "Tmp", "test-runtime", "_runs")
	workspace, _, _ := candidatePaths(t, root)
	logDirectory := filepath.Join(filepath.Dir(workspace), "source-candidate-logs")
	if err := PrepareLogDirectory(runs, logDirectory); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckOwnerOnlyDirForChildren(logDirectory); err != nil {
		t.Fatalf("source-candidate log directory is not private: %v", err)
	}
	if err := PrepareLogDirectory(runs, logDirectory); err == nil {
		t.Fatal("PrepareLogDirectory accepted an existing evidence directory")
	}
	outside := filepath.Join(t.TempDir(), "logs")
	if err := PrepareLogDirectory(runs, outside); err == nil {
		t.Fatal("PrepareLogDirectory accepted a directory outside owner _runs")
	}
}

func captureErr(root, workspace string) error {
	return captureErrWithRuns(root, filepath.Join(root, "Tmp", "test-runtime", "_runs"), workspace)
}

func captureErrWithRuns(root, runs, workspace string) error {
	_, err := Capture(root, runs, workspace)
	return err
}

func candidatePaths(t *testing.T, root string) (workspace, candidate, manifest string) {
	t.Helper()
	runs := filepath.Join(root, "Tmp", "test-runtime", "_runs", "run")
	if err := os.MkdirAll(runs, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace = filepath.Join(runs, "source-work")
	return workspace, filepath.Join(workspace, "candidate"), filepath.Join(workspace, "manifest.json")
}

func newGitSource(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "--quiet")
	if err := os.MkdirAll(filepath.Join(root, "Tmp", "test-runtime", "_runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v: %s", args, err, output)
	}
}

func writeSourceFile(t *testing.T, root, relative, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
