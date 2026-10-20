package main_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func TestGitProfilesThroughTheCanonicalHarnessCLIAndGateway(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is required for the local bare-remote E2E")
	}
	gitPath, err = filepath.Abs(gitPath)
	if err != nil {
		t.Fatal(err)
	}

	const authorName = "Git Env Value Must Not Reach Tool Description 57af4b"
	const authorEmail = "private-git-env-57af4b@example.invalid"
	fake := harnesstest.NewFake()
	makeGitCall := func(id string, argv ...string) harnesstest.Reply {
		t.Helper()
		args, err := json.Marshal(map[string]any{
			"executable": gitPath, "argv": argv, "cwd": ".", "env_profile_ref": "git-local", "timeout_seconds": 30,
		})
		if err != nil {
			t.Fatal(err)
		}
		return harnesstest.Reply{Kind: harnesstest.KindToolCall, Calls: []harnesstest.Call{{ID: id, Name: "process.exec", Args: string(args)}}}
	}
	fake.SetScript(
		makeGitCall("git-status", "status", "--short"),
		makeGitCall("git-diff", "diff", "--", "tracked.txt"),
		makeGitCall("git-add", "add", "--", "tracked.txt"),
		makeGitCall("git-commit", "commit", "-m", "Harness E2E update"),
		makeGitCall("git-push", "push", "origin", "HEAD:refs/heads/main"),
		harnesstest.Final("The local Git update was committed and pushed."),
	)

	f := newGatewayFixtureWith(t, fake, false, func(l *harnesstest.Layout) {
		globalConfig := filepath.Join(l.Dir, "empty.gitconfig")
		if err := os.WriteFile(globalConfig, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		policy := l.Reg["policies"].([]any)[0].(map[string]any)
		tools := policy["tools"].([]any)
		if !slices.Contains(tools, "process.exec") {
			policy["tools"] = append(tools, "process.exec")
		}
		policy["process_profiles"] = []any{"git-any"}
		policy["env_profiles"] = []any{"git-local"}
		l.Cfg["process_profiles"] = []any{map[string]any{
			"name": "git-any", "executable": gitPath, "is_shell": false, "argv_prefix": []any{},
		}}
		l.Cfg["env_profiles"] = []any{
			map[string]any{"name": "clean", "values": map[string]string{}},
			map[string]any{"name": "git-local", "values": map[string]string{
				"GIT_AUTHOR_NAME": authorName, "GIT_AUTHOR_EMAIL": authorEmail,
				"GIT_COMMITTER_NAME": authorName, "GIT_COMMITTER_EMAIL": authorEmail,
				"GIT_CONFIG_GLOBAL": globalConfig, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0",
			}},
		}
	})

	setupEnv := mergeHarnessGitEnv(map[string]string{
		"GIT_CONFIG_GLOBAL":   filepath.Join(f.l.Dir, "empty.gitconfig"),
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_TERMINAL_PROMPT": "0",
	})
	remote := filepath.Join(f.l.Dir, "remote.git")
	setupHarnessGit(t, gitPath, f.l.Work, setupEnv, "init", "--quiet", "--template=")
	setupHarnessGit(t, gitPath, f.l.Dir, setupEnv, "init", "--bare", "--quiet", "--template=", remote)
	if err := os.WriteFile(filepath.Join(f.l.Work, "tracked.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setupHarnessGit(t, gitPath, f.l.Work, setupEnv, "add", "--", "tracked.txt")
	setupHarnessGit(t, gitPath, f.l.Work, setupEnv, "-c", "user.name=Initial Author", "-c", "user.email=initial@example.invalid", "commit", "--quiet", "-m", "seed")
	setupHarnessGit(t, gitPath, f.l.Work, setupEnv, "branch", "-M", "main")
	setupHarnessGit(t, gitPath, f.l.Work, setupEnv, "remote", "add", "origin", remote)
	if err := os.WriteFile(filepath.Join(f.l.Work, "tracked.txt"), []byte("updated by process.exec\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	input := writeFile(t, f.l.Dir, "input.txt", "Use the configured Git process profile to inspect and publish this local change.")
	code, out, errs := runCLI(t, execArgsOf(f, input, "--mode", "trusted_host", "--json")...)
	if code != 0 {
		t.Fatalf("exec exited %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	events, result := checkStream(t, out)
	if result["status"] != "completed" || result["final_text"] != "The local Git update was committed and pushed." {
		t.Fatalf("run result: %v", result)
	}

	generated := fake.Generates()
	if len(generated) != 6 {
		t.Fatalf("Gateway saw %d generation requests, want 6", len(generated))
	}
	for i, request := range generated {
		var processDescription string
		for _, declaration := range request.Tools {
			if declaration.Function.Name == "process.exec" {
				processDescription = declaration.Function.Description
			}
		}
		if processDescription == "" || !strings.Contains(processDescription, fmt.Sprintf(`"git-any": executable=%q, is_shell=false, argv_prefix=[]`, gitPath)) ||
			!strings.Contains(processDescription, `Allowed env_profile_ref names: ["git-local"].`) {
			t.Fatalf("generation %d did not receive the Run's Git authority: %q", i, processDescription)
		}
		if strings.Contains(processDescription, authorName) || strings.Contains(processDescription, authorEmail) {
			t.Fatalf("generation %d received an environment value in the Tool description: %q", i, processDescription)
		}
	}

	var prepared []protocol.ActionPreparedPayload
	var started []protocol.ActionDispatchStartedPayload
	var completed []protocol.ActionCompletedPayload
	for _, line := range events {
		event := line
		payload, err := json.Marshal(event["payload"])
		if err != nil {
			t.Fatal(err)
		}
		switch event["type"] {
		case protocol.EventActionPrepared:
			var value protocol.ActionPreparedPayload
			if err := json.Unmarshal(payload, &value); err != nil {
				t.Fatal(err)
			}
			if value.Kind == "tool" {
				prepared = append(prepared, value)
			}
		case protocol.EventActionDispatchStarted:
			var value protocol.ActionDispatchStartedPayload
			if err := json.Unmarshal(payload, &value); err != nil {
				t.Fatal(err)
			}
			started = append(started, value)
		case protocol.EventActionCompleted:
			var value protocol.ActionCompletedPayload
			if err := json.Unmarshal(payload, &value); err != nil {
				t.Fatal(err)
			}
			if len(value.ResultEvidenceIDs) > 0 {
				completed = append(completed, value)
			}
		}
	}
	preparedIDs := make(map[string]struct{}, len(prepared))
	for _, value := range prepared {
		preparedIDs[value.ActionID] = struct{}{}
	}
	var toolStarted []protocol.ActionDispatchStartedPayload
	for _, value := range started {
		if _, ok := preparedIDs[value.ActionID]; ok {
			toolStarted = append(toolStarted, value)
		}
	}
	if len(prepared) != 5 || len(toolStarted) != 5 || len(completed) != 5 {
		t.Fatalf("Tool Action/Attempt lifecycle counts: prepared=%d started=%d completed=%d", len(prepared), len(toolStarted), len(completed))
	}
	var captured strings.Builder
	for i := range prepared {
		if prepared[i].Name != "process.exec" || prepared[i].ActionID != toolStarted[i].ActionID || prepared[i].ActionID != completed[i].ActionID ||
			prepared[i].AttemptID != toolStarted[i].AttemptID || prepared[i].AttemptID != completed[i].AttemptID ||
			completed[i].EffectState != "completed" || completed[i].ExitCode == nil || *completed[i].ExitCode != 0 || !completed[i].CaptureComplete || len(completed[i].ResultEvidenceIDs) < 2 {
			t.Fatalf("Git call %d did not complete as one Action/Attempt with stdout and stderr Evidence: prepared=%+v started=%+v completed=%+v", i, prepared[i], toolStarted[i], completed[i])
		}
		for _, evidenceID := range completed[i].ResultEvidenceIDs {
			captured.Write(readHarnessGitEvidence(t, f.l.Config, evidenceID))
		}
	}
	for _, want := range []string{" M tracked.txt", "+updated by process.exec", "Harness E2E update", "HEAD -> main"} {
		if !strings.Contains(captured.String(), want) {
			t.Fatalf("stored process Evidence does not contain %q:\n%s", want, captured.String())
		}
	}

	author := strings.TrimSpace(setupHarnessGit(t, gitPath, f.l.Work, setupEnv, "show", "-s", "--format=%an", "HEAD"))
	localHead := strings.TrimSpace(setupHarnessGit(t, gitPath, f.l.Work, setupEnv, "rev-parse", "HEAD"))
	remoteHead := strings.TrimSpace(setupHarnessGit(t, gitPath, f.l.Dir, setupEnv, "--git-dir", remote, "rev-parse", "refs/heads/main"))
	if author != authorName || localHead == "" || remoteHead != localHead {
		t.Fatalf("Git commit/push mismatch: author=%q local=%q remote=%q", author, localHead, remoteHead)
	}
}

func TestMergeHarnessGitEnvDropsInheritedGitVariables(t *testing.T) {
	inherited := []string{
		"PATH=/usr/bin",
		"HOME=/tmp/fixture-home",
		"GIT_DIR=/outside/repository.git",
		"GIT_INDEX_FILE=/outside/index",
		"GIT_WORK_TREE=/outside/worktree",
		"GIT_CONFIG_GLOBAL=/outside/global.gitconfig",
		"GIT_CONFIG_NOSYSTEM=0",
		"GIT_TERMINAL_PROMPT=1",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.hooksPath",
		"GIT_CONFIG_VALUE_0=/outside/hooks",
		"GIT_AUTHOR_NAME=inherited author",
	}
	overrides := map[string]string{
		"GIT_CONFIG_GLOBAL":   "/fixture/empty.gitconfig",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_TERMINAL_PROMPT": "0",
	}

	got := mergeHarnessGitEnvFrom(inherited, overrides)
	values := make(map[string]string, len(got))
	for _, entry := range got {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("malformed environment entry %q", entry)
		}
		values[key] = value
	}
	for key, want := range map[string]string{
		"PATH":                "/usr/bin",
		"HOME":                "/tmp/fixture-home",
		"GIT_CONFIG_GLOBAL":   "/fixture/empty.gitconfig",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_TERMINAL_PROMPT": "0",
	} {
		if values[key] != want {
			t.Fatalf("environment %s = %q, want %q", key, values[key], want)
		}
	}
	for _, key := range []string{
		"GIT_DIR", "GIT_INDEX_FILE", "GIT_WORK_TREE", "GIT_CONFIG_COUNT",
		"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_AUTHOR_NAME",
	} {
		if _, ok := values[key]; ok {
			t.Fatalf("inherited %s reached fixture environment: %v", key, values)
		}
	}
}

func mergeHarnessGitEnv(overrides map[string]string) []string {
	return mergeHarnessGitEnvFrom(os.Environ(), overrides)
}

func mergeHarnessGitEnvFrom(inherited []string, overrides map[string]string) []string {
	values := make(map[string]string, len(inherited)+len(overrides))
	for _, entry := range inherited {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(key, "GIT_") {
			continue
		}
		if _, overridden := overrides[key]; !overridden {
			values[key] = value
		}
	}
	keys := make([]string, 0, len(overrides)+len(values))
	for key, value := range overrides {
		values[key] = value
	}
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func setupHarnessGit(t *testing.T, executable, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(executable, args...)
	cmd.Dir, cmd.Env = dir, env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func readHarnessGitEvidence(t *testing.T, configPath, evidenceID string) []byte {
	t.Helper()
	args := []string{"evidence", "--config", configPath, "--id", evidenceID, "--start", "0", "--end", "0", "--projection", "raw"}
	code, _, metadata := runCLI(t, args...)
	if code != 0 {
		t.Fatalf("evidence metadata read exited %d: %s", code, metadata)
	}
	const totalMarker = "total="
	at := strings.Index(metadata, totalMarker)
	if at < 0 {
		t.Fatalf("evidence metadata has no total: %s", metadata)
	}
	field := strings.Fields(metadata[at+len(totalMarker):])
	if len(field) == 0 {
		t.Fatalf("evidence metadata has no total value: %s", metadata)
	}
	total, err := strconv.ParseUint(strings.TrimSuffix(field[0], ")"), 10, 64)
	if err != nil {
		t.Fatalf("evidence total is invalid: %s (%v)", metadata, err)
	}
	args[8] = strconv.FormatUint(total, 10)
	code, data, metadata := runCLI(t, args...)
	if code != 0 || uint64(len(data)) != total {
		t.Fatalf("evidence read exited %d with %d of %d bytes: %s", code, len(data), total, metadata)
	}
	return []byte(data)
}
