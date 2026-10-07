package extensions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// tree is a throw-away workspace.
type tree struct {
	t    *testing.T
	root string
}

func newTree(t *testing.T) *tree {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &tree{t: t, root: dir}
}

func (w *tree) write(rel, content string) string {
	w.t.Helper()
	p := filepath.Join(w.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
	return p
}

func (w *tree) mkdir(rel string) {
	w.t.Helper()
	if err := os.MkdirAll(filepath.Join(w.root, filepath.FromSlash(rel)), 0o755); err != nil {
		w.t.Fatal(err)
	}
}

func (w *tree) symlink(target, rel string) {
	w.t.Helper()
	if runtime.GOOS == "windows" {
		w.t.Skip("a symbolic link needs a privilege on Windows")
	}
	if err := os.Symlink(target, filepath.Join(w.root, filepath.FromSlash(rel))); err != nil {
		w.t.Fatal(err)
	}
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func trustedCfg(w *tree, skillRoots ...string) config.ExtensionsConfig {
	return config.ExtensionsConfig{Enabled: true, TrustedWorkspaceRoots: []string{w.root}, SkillRoots: skillRoots, Hooks: []string{}}
}

func skillFile(name, desc string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n# " + name + "\nBody.\n"
}

// TestInstructionAssetsAreOnlyLookedForInATrustedWorkspace: trust is the extensions being
// enabled and the workspace's real root being, exactly, a trusted root. The files being there,
// or a .git directory, is no evidence of it.
func TestInstructionAssetsAreOnlyLookedForInATrustedWorkspace(t *testing.T) {
	w := newTree(t)
	w.write("AGENTS.md", "rules\n")
	w.mkdir(".git")
	w.write(".agents/skills/s/SKILL.md", skillFile("s", "a skill"))
	for name, cfg := range map[string]config.ExtensionsConfig{
		"extensions disabled":     {Enabled: false, TrustedWorkspaceRoots: []string{w.root}, SkillRoots: []string{}, Hooks: []string{}},
		"no trusted root":         {Enabled: true, TrustedWorkspaceRoots: []string{}, SkillRoots: []string{}, Hooks: []string{}},
		"another root":            {Enabled: true, TrustedWorkspaceRoots: []string{filepath.Join(w.root, "other")}, SkillRoots: []string{}, Hooks: []string{}},
		"a root spelled with /":   {Enabled: true, TrustedWorkspaceRoots: []string{w.root + "/"}, SkillRoots: []string{}, Hooks: []string{}},
		"the parent of the root":  {Enabled: true, TrustedWorkspaceRoots: []string{filepath.Dir(w.root)}, SkillRoots: []string{}, Hooks: []string{}},
		"a root in other letters": {Enabled: true, TrustedWorkspaceRoots: []string{strings.ToUpper(w.root)}, SkillRoots: []string{}, Hooks: []string{}},
	} {
		if strings.ToUpper(w.root) == w.root && name == "a root in other letters" {
			continue
		}
		if Trusted(cfg, w.root) {
			t.Errorf("%s: trusted", name)
		}
		a, err := Load(cfg, w.root, LoadOptions{WithSkills: true})
		if err != nil || a != nil {
			t.Errorf("%s: %+v %v", name, a, err)
		}
	}
	a, err := Load(trustedCfg(w), w.root, LoadOptions{WithSkills: true})
	if err != nil || a == nil || len(a.Agents.Files) != 1 || len(a.Skills.Skills) != 1 {
		t.Fatalf("%+v %v", a, err)
	}
}

// TestOnlyTheChainFromTheRootToTheWorkingDirectoryIsRead: the root first, the deepest last;
// not a sibling, not what is below the working directory, not what is above the root, and
// not another name.
func TestOnlyTheChainFromTheRootToTheWorkingDirectoryIsRead(t *testing.T) {
	w := newTree(t)
	w.write("AGENTS.md", "root rules\n")
	w.write("a/AGENTS.md", "a rules\n")
	w.write("a/b/AGENTS.md", "b rules\n")
	w.write("a/b/c/AGENTS.md", "c rules\n")
	w.write("a/x/AGENTS.md", "sibling rules\n")
	w.write("z/AGENTS.md", "another tree\n")
	w.write("a/CLAUDE.md", "not an AGENTS.md\n")
	w.write("a/b/agents.md.txt", "not either\n")
	// Above the workspace root.
	parent := filepath.Dir(w.root)
	if err := os.WriteFile(filepath.Join(parent, "AGENTS.md"), []byte("above the root\n"), 0o644); err == nil {
		t.Cleanup(func() { _ = os.Remove(filepath.Join(parent, "AGENTS.md")) })
	}

	got, err := LoadAgents(w.root, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	var paths, scopes []string
	for _, f := range got.Files {
		paths, scopes = append(paths, f.Path), append(scopes, f.Scope)
	}
	if strings.Join(paths, ",") != "AGENTS.md,a/AGENTS.md,a/b/AGENTS.md" || strings.Join(scopes, ",") != ".,a,a/b" {
		t.Fatalf("%v %v", paths, scopes)
	}
	if got.Files[0].SHA256 != sha("root rules\n") || got.Files[2].Bytes != len("b rules\n") || got.TotalBytes != len("root rules\n")+len("a rules\n")+len("b rules\n") {
		t.Fatalf("%+v", got)
	}
	// The root alone is the chain of a working directory that is the root.
	if only, err := LoadAgents(w.root, "."); err != nil || len(only.Files) != 1 {
		t.Fatalf("%+v %v", only, err)
	}
	// A directory with no AGENTS.md is a gap, not an error.
	w.mkdir("empty/deeper")
	if gap, err := LoadAgents(w.root, "empty/deeper"); err != nil || len(gap.Files) != 1 || gap.Files[0].Path != "AGENTS.md" {
		t.Fatalf("%+v %v", gap, err)
	}
}

// TestAWorkingDirectoryOutsideTheWorkspaceIsRefused: nothing above the root, nothing through
// a link, nothing that is not a directory.
func TestAWorkingDirectoryOutsideTheWorkspaceIsRefused(t *testing.T) {
	w := newTree(t)
	w.write("AGENTS.md", "rules\n")
	w.write("file.txt", "x")
	w.mkdir("real")
	outside := newTree(t)
	outside.write("AGENTS.md", "outside rules\n")
	w.symlink(outside.root, "link")
	for name, cwd := range map[string]string{
		"above the root":      "..",
		"deeper then back up": "real/../..",
		"an absolute path":    outside.root,
		"a link":              "link",
		"a file":              "file.txt",
		"a missing directory": "nope",
		"a backslash":         `real\x`,
		"a NUL":               "re\x00al",
	} {
		_, err := LoadAgents(w.root, cwd)
		if !errors.Is(err, ErrInvalidAsset) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := LoadAgents("relative/root", "."); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("a relative workspace root: %v", err)
	}
	if _, err := LoadAgents(w.root, strings.Repeat("a/", 2100)); !errors.Is(err, ErrAssetTooLarge) {
		t.Errorf("a path over 4096 bytes: %v", err)
	}
}

func TestAnAgentsFileThatIsNotARegularFileOfTheWorkspaceIsRefused(t *testing.T) {
	outside := newTree(t)
	outside.write("secret.md", "not for the model\n")

	w := newTree(t)
	w.symlink(filepath.Join(outside.root, "secret.md"), "AGENTS.md")
	if _, err := LoadAgents(w.root, "."); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("a link to a file outside: %v", err)
	}

	w2 := newTree(t)
	w2.write("real.md", "inside\n")
	w2.symlink(filepath.Join(w2.root, "real.md"), "AGENTS.md")
	if _, err := LoadAgents(w2.root, "."); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("a link to a file inside: %v", err)
	}

	w3 := newTree(t)
	w3.mkdir("AGENTS.md")
	if _, err := LoadAgents(w3.root, "."); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("a directory: %v", err)
	}

	w4 := newTree(t)
	w4.write("AGENTS.md", "ok\xffnot utf-8\n")
	if _, err := LoadAgents(w4.root, "."); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("invalid UTF-8: %v", err)
	}

	// A link in the middle of the chain.
	w5 := newTree(t)
	outside.write("AGENTS.md", "outside\n")
	w5.symlink(outside.root, "dir")
	if _, err := LoadAgents(w5.root, "dir"); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("a link directory in the chain: %v", err)
	}
}

// TestSizeLimitsAreRefusedAndNeverCut: one file up to 32 KiB, 128 KiB together; over either
// is ErrAssetTooLarge and nothing of it is loaded.
func TestSizeLimitsAreRefusedAndNeverCut(t *testing.T) {
	exact := strings.Repeat("a", MaxAgentsFileBytes)
	w := newTree(t)
	w.write("AGENTS.md", exact)
	got, err := LoadAgents(w.root, ".")
	if err != nil || got.Files[0].Bytes != MaxAgentsFileBytes || got.Files[0].Text != exact {
		t.Fatalf("a file of exactly 32 KiB: %v", err)
	}
	w.write("AGENTS.md", exact+"a")
	if got, err := LoadAgents(w.root, "."); !errors.Is(err, ErrAssetTooLarge) || len(got.Files) != 0 {
		t.Fatalf("a file of 32 KiB + 1: %v %+v", err, got)
	}

	// Four files of exactly 32 KiB are exactly 128 KiB; a fifth byte over is refused.
	w2 := newTree(t)
	for _, d := range []string{"", "a/", "a/b/", "a/b/c/"} {
		w2.write(d+"AGENTS.md", exact)
	}
	if got, err := LoadAgents(w2.root, "a/b/c"); err != nil || got.TotalBytes != MaxAgentsTotalBytes {
		t.Fatalf("128 KiB together: %v", err)
	}
	w2.write("a/b/c/d/AGENTS.md", "x")
	if got, err := LoadAgents(w2.root, "a/b/c/d"); !errors.Is(err, ErrAssetTooLarge) || len(got.Files) != 0 {
		t.Fatalf("128 KiB + 1 together: %v %+v", err, got)
	}
	// The same refusal at Load: the Run is not admitted.
	if _, err := Load(trustedCfg(w2), w2.root, LoadOptions{Cwd: "a/b/c/d"}); !errors.Is(err, ErrAssetTooLarge) {
		t.Fatalf("%v", err)
	}
}

// TestTheAgentsBlockIsAScopedStableBlockThatIsTheSameForTheSameFiles.
func TestTheAgentsBlockIsAScopedStableBlockThatIsTheSameForTheSameFiles(t *testing.T) {
	w := newTree(t)
	w.write("AGENTS.md", "root \"rules\"\n")
	w.write("sub/AGENTS.md", "sub rules\n")
	a1, err := Load(trustedCfg(w), w.root, LoadOptions{Cwd: "sub"})
	if err != nil || a1 == nil || len(a1.Blocks) != 1 {
		t.Fatalf("%+v %v", a1, err)
	}
	a2, _ := Load(trustedCfg(w), w.root, LoadOptions{Cwd: "sub"})
	b := a1.Blocks[0]
	if b.Kind != protocol.KindStableRuntimeContext || b.Source != nil || b.Text != a2.Blocks[0].Text || b.Revision != a2.Blocks[0].Revision {
		t.Fatalf("%+v", b)
	}
	if want, _ := protocol.ContextRevision(b.Kind, b.Text, nil); b.Revision != want || a1.AgentsHash != sha(b.Text) {
		t.Fatalf("the revision or the snapshot hash is not the block's")
	}
	var env struct {
		Format string `json:"format"`
		Notice string `json:"notice"`
		Files  []struct {
			Path, Scope, Sha256, Text string
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(b.Text), &env); err != nil {
		t.Fatal(err)
	}
	if env.Format != "rencrow-host-agents/v1" || len(env.Files) != 2 || env.Files[0].Scope != "." || env.Files[1].Scope != "sub" || env.Files[1].Text != "sub rules\n" ||
		env.Files[0].Sha256 != sha("root \"rules\"\n") || !strings.Contains(env.Notice, "not the user's instruction") {
		t.Fatalf("%+v", env)
	}
	// No path of the host is in what the model is shown.
	for _, s := range []string{b.Text, string(a1.Manifest)} {
		if strings.Contains(s, w.root) {
			t.Fatalf("a path of the host is in the record: %s", s)
		}
	}
	// A change of a file is a different block.
	w.write("sub/AGENTS.md", "sub rules, changed\n")
	a3, _ := Load(trustedCfg(w), w.root, LoadOptions{Cwd: "sub"})
	if a3.Blocks[0].Revision == b.Revision || a3.AgentsHash == a1.AgentsHash {
		t.Fatal("a changed file gave the same snapshot")
	}
}

func TestACaseDifferentNameIsNotAnAgentsFileOnAnyOS(t *testing.T) {
	w := newTree(t)
	w.write("agents.md", "lower case\n")
	got, err := LoadAgents(w.root, ".")
	if err != nil || len(got.Files) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}

// ---- skills ----

func TestSkillsAreFoundOneLevelDownInRootOrderAndNameOrder(t *testing.T) {
	w := newTree(t)
	w.write(".agents/skills/zeta/SKILL.md", skillFile("zeta", "last by directory"))
	w.write(".agents/skills/alpha/SKILL.md", skillFile("alpha", "first by directory"))
	w.write(".agents/skills/alpha/nested/SKILL.md", skillFile("nested", "two levels down: not read"))
	w.write(".agents/skills/not-a-skill/README.md", "no SKILL.md here\n")
	w.write(".agents/skills/loose.md", "a file in the root\n")
	host := newTree(t)
	host.write("managed/SKILL.md", skillFile("managed", "from a host root"))
	host.write("beta/SKILL.md", skillFile("beta", "from a host root, too"))

	a, err := Load(trustedCfg(w, host.root), w.root, LoadOptions{WithSkills: true})
	if err != nil || a == nil {
		t.Fatalf("%+v %v", a, err)
	}
	var names []string
	for _, s := range a.Skills.Skills {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "alpha,zeta,beta,managed" {
		t.Fatalf("%v", names)
	}
	// The catalog: only what HOST_ASSETS gives the model, and the files sealed under the IDs it names.
	var cat struct {
		Skills []map[string]any `json:"skills"`
	}
	var catBlock protocol.ContextBlock
	for _, b := range a.Blocks {
		if b.Kind == protocol.KindVariableRuntimeContext {
			catBlock = b
		}
	}
	if err := json.Unmarshal([]byte(catBlock.Text), &cat); err != nil || len(cat.Skills) != 4 {
		t.Fatalf("%v %s", err, catBlock.Text)
	}
	for i, e := range cat.Skills {
		if len(e) != 5 || e["projection_version"] != "text/v1" || e["evidence_id"] != a.Files[i].EvidenceID || int(e["total_bytes"].(float64)) != len(a.Files[i].Data) || e["name"] != names[i] {
			t.Fatalf("%v", e)
		}
		if a.Files[i].Purpose != PurposeSkillFile || a.Files[i].MediaType == "" {
			t.Fatalf("%+v", a.Files[i])
		}
	}
	if strings.Contains(catBlock.Text, "Body.") || strings.Contains(catBlock.Text, w.root) || strings.Contains(catBlock.Text, host.root) {
		t.Fatal("the catalog carries a body or a path")
	}
	if a.CatalogHash != sha(catBlock.Text) || !strings.Contains(catBlock.Text, "not an instruction of the user") {
		t.Fatalf("%s", catBlock.Text)
	}
	// The whole file is sealed, byte for byte, and the manifest records its hash.
	if string(a.Files[0].Data) != skillFile("alpha", "first by directory") || !strings.Contains(string(a.Manifest), sha(skillFile("alpha", "first by directory"))) ||
		strings.Contains(string(a.Manifest), w.root) || strings.Contains(string(a.Manifest), host.root) {
		t.Fatalf("%s", a.Manifest)
	}
	// Without the Tool to read them, no catalog is given.
	b, err := Load(trustedCfg(w, host.root), w.root, LoadOptions{WithSkills: false})
	if err != nil || b != nil {
		t.Fatalf("%+v %v", b, err)
	}
}

func TestAnUntrustedWorkspaceGetsTheHostsSkillRootsAndNotItsOwn(t *testing.T) {
	w := newTree(t)
	w.write(".agents/skills/mine/SKILL.md", skillFile("mine", "the workspace's own"))
	w.write("AGENTS.md", "rules\n")
	host := newTree(t)
	host.write("shared/SKILL.md", skillFile("shared", "the host's"))
	cfg := config.ExtensionsConfig{Enabled: true, TrustedWorkspaceRoots: []string{}, SkillRoots: []string{host.root}, Hooks: []string{}}
	a, err := Load(cfg, w.root, LoadOptions{WithSkills: true})
	if err != nil || a == nil || len(a.Skills.Skills) != 1 || a.Skills.Skills[0].Name != "shared" || len(a.Agents.Files) != 0 || len(a.Blocks) != 1 {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestADuplicateSkillNameRefusesTheWholeCatalog(t *testing.T) {
	w := newTree(t)
	w.write(".agents/skills/one/SKILL.md", skillFile("same-name", "one"))
	w.write(".agents/skills/two/SKILL.md", skillFile("same-name", "two"))
	if _, err := Load(trustedCfg(w), w.root, LoadOptions{WithSkills: true}); !errors.Is(err, ErrInvalidAsset) {
		t.Fatalf("%v", err)
	}
	// Across roots, too.
	w2 := newTree(t)
	w2.write(".agents/skills/one/SKILL.md", skillFile("same-name", "one"))
	host := newTree(t)
	host.write("other/SKILL.md", skillFile("same-name", "other"))
	if _, err := Load(trustedCfg(w2, host.root), w2.root, LoadOptions{WithSkills: true}); !errors.Is(err, ErrInvalidAsset) {
		t.Fatalf("%v", err)
	}
}

func TestASkillRootOrFileThatIsALinkIsRefused(t *testing.T) {
	outside := newTree(t)
	outside.write("evil/SKILL.md", skillFile("evil", "outside the root"))

	// A directory of the root that is a link.
	w := newTree(t)
	w.mkdir(".agents/skills")
	w.symlink(filepath.Join(outside.root, "evil"), ".agents/skills/evil")
	if _, err := Load(trustedCfg(w), w.root, LoadOptions{WithSkills: true}); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("a link directory: %v", err)
	}
	// A SKILL.md that is a link.
	w2 := newTree(t)
	w2.mkdir(".agents/skills/s")
	w2.symlink(filepath.Join(outside.root, "evil", "SKILL.md"), ".agents/skills/s/SKILL.md")
	if _, err := Load(trustedCfg(w2), w2.root, LoadOptions{WithSkills: true}); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("a link SKILL.md: %v", err)
	}
	// The workspace's .agents itself a link out of the workspace.
	w3 := newTree(t)
	outside.mkdir("agents-out")
	if err := os.MkdirAll(filepath.Join(outside.root, "agents-out", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	w3.symlink(filepath.Join(outside.root, "agents-out"), ".agents")
	if _, err := Load(trustedCfg(w3), w3.root, LoadOptions{WithSkills: true}); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("a link .agents: %v", err)
	}
}

func TestASkillRootTheHostNamedThatIsNotThereIsAnError(t *testing.T) {
	w := newTree(t)
	missing := filepath.Join(w.root, "no-such-root")
	if _, err := Load(trustedCfg(w, missing), w.root, LoadOptions{WithSkills: true}); !errors.Is(err, ErrInvalidAsset) {
		t.Fatalf("%v", err)
	}
	// The workspace's own .agents/skills may be absent.
	if a, err := Load(trustedCfg(w), w.root, LoadOptions{WithSkills: true}); err != nil || a != nil {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := Load(trustedCfg(w, "relative/root"), w.root, LoadOptions{WithSkills: true}); !errors.Is(err, ErrInvalidAsset) {
		t.Fatalf("a relative skill root: %v", err)
	}
}

func TestSkillLimitsAreRefusedAndNeverCut(t *testing.T) {
	// A file of exactly 64 KiB is read; one byte more is refused.
	w := newTree(t)
	head := skillFile("big", "a big one")
	exact := head + strings.Repeat("x", MaxSkillFileBytes-len(head))
	w.write(".agents/skills/big/SKILL.md", exact)
	a, err := Load(trustedCfg(w), w.root, LoadOptions{WithSkills: true})
	if err != nil || a == nil || len(a.Skills.Skills[0].Bytes) != MaxSkillFileBytes {
		t.Fatalf("64 KiB: %v", err)
	}
	w.write(".agents/skills/big/SKILL.md", exact+"x")
	if _, err := Load(trustedCfg(w), w.root, LoadOptions{WithSkills: true}); !errors.Is(err, ErrAssetTooLarge) {
		t.Fatalf("64 KiB + 1: %v", err)
	}

	// 128 skills are read; 129 are refused.
	w2 := newTree(t)
	for i := 0; i < MaxSkills; i++ {
		name := "skill-" + strings.Repeat("a", 0) + string(rune('a'+i/26)) + string(rune('a'+i%26))
		w2.write(".agents/skills/"+name+"/SKILL.md", skillFile(name, "d"))
	}
	if a, err := Load(trustedCfg(w2), w2.root, LoadOptions{WithSkills: true}); err != nil || len(a.Skills.Skills) != MaxSkills {
		t.Fatalf("128 skills: %v", err)
	}
	w2.write(".agents/skills/skill-zz/SKILL.md", skillFile("skill-zz", "d"))
	if _, err := Load(trustedCfg(w2), w2.root, LoadOptions{WithSkills: true}); !errors.Is(err, ErrAssetTooLarge) {
		t.Fatalf("129 skills: %v", err)
	}
}

func TestAHeaderThatIsNotInTheSubsetRefusesTheCatalog(t *testing.T) {
	w := newTree(t)
	w.write(".agents/skills/s/SKILL.md", "---\nname: s\ndescription: d\nextra: 1\n---\n")
	if _, err := Load(trustedCfg(w), w.root, LoadOptions{WithSkills: true}); !errors.Is(err, ErrInvalidAsset) {
		t.Fatalf("%v", err)
	}
	w2 := newTree(t)
	w2.write(".agents/skills/s/SKILL.md", "---\nname: s\ndescription: d\n---\nbody\xff\n")
	if _, err := Load(trustedCfg(w2), w2.root, LoadOptions{WithSkills: true}); !errors.Is(err, ErrInvalidAsset) {
		t.Fatalf("invalid UTF-8 in the body: %v", err)
	}
	// A directory whose SKILL.md is spelled in other letters is not a skill (and nothing is read through it).
	w3 := newTree(t)
	w3.write(".agents/skills/s/skill.md", skillFile("s", "d"))
	if a, err := Load(trustedCfg(w3), w3.root, LoadOptions{WithSkills: true}); err != nil || a != nil {
		t.Fatalf("%+v %v", a, err)
	}
}
