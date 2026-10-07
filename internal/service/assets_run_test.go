package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/harnesstest"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// F27 through turn/start: the AGENTS.md of a trusted workspace and the skill catalog become
// blocks of the Run the host adds beside the caller's; a limit or a link refuses the Run
// before anything is made; the same request again is answered from its receipt whatever the
// files have become since.

type extCfg struct {
	enabled    bool
	trusted    bool
	skillRoots []string
	tools      []string
}

// assetRig is a rig over a workspace the test fills before the first turn.
func assetRig(t *testing.T, fake *harnesstest.Fake, c extCfg) *rig {
	t.Helper()
	layout := harnesstest.NewLayout(t, harnesstest.Options{CreateData: true})
	toolConfig{tools: c.tools}.edit(layout)
	trusted := []any{}
	if c.trusted {
		trusted = append(trusted, layout.Work)
	}
	roots := []any{}
	for _, r := range c.skillRoots {
		roots = append(roots, r)
	}
	layout.Cfg["extensions"] = m{"enabled": c.enabled, "trusted_workspace_roots": trusted, "skill_roots": roots, "hooks": []any{}}
	layout.Write()
	dep, err := config.Load(layout.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Init(context.Background(), dep.DataRoot); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, layout: layout, dep: dep, model: fake}
	r.store, r.svc = r.process()
	r.conn = r.svc.NewConn(&recorder{})
	return r
}

func skillText(name, desc, body string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n" + body
}

func firstPrompt(fake *harnesstest.Fake) []modelport.ChatMessage {
	g := fake.Generates()
	if len(g) == 0 {
		return nil
	}
	return g[0].Messages
}

func find(msgs []modelport.ChatMessage, contains string) (modelport.ChatMessage, int) {
	for i, m := range msgs {
		if strings.Contains(m.Text(), contains) {
			return m, i
		}
	}
	return modelport.ChatMessage{}, -1
}

func TestTheAgentsFileOfATrustedWorkspaceIsGivenToTheRunAsAScopedHostBlock(t *testing.T) {
	fake := harnesstest.NewFake()
	r := assetRig(t, fake, extCfg{enabled: true, trusted: true})
	r.write("AGENTS.md", "Always run the formatter.\n")
	info := r.openSession("asset.agents.open.000001")
	const blockText = "caller's own stable block"
	rev, _ := protocol.ContextRevision(protocol.KindStableRuntimeContext, blockText, nil)
	start := r.startRun(info.ThreadID, "asset.agents.start.00001", "do the work", func(in *protocol.StartInput) {
		in.ContextBlocks = []protocol.ContextBlock{{Kind: protocol.KindStableRuntimeContext, Text: blockText, Revision: rev}}
	})
	if run := r.waitTerminal(start.RunID); run.Result.Status != "completed" {
		t.Fatalf("%+v", run.Result)
	}
	msgs := firstPrompt(fake)
	agentsMsg, at := find(msgs, "rencrow-host-agents/v1")
	callerMsg, callerAt := find(msgs, blockText)
	if at < 0 || callerAt < 0 || at < callerAt {
		t.Fatalf("the host's block is missing or comes before the caller's (%d, %d)", at, callerAt)
	}
	// The same kind and the same projection as a caller's stable block: nothing says it is the user's.
	if agentsMsg.Role != callerMsg.Role {
		t.Fatalf("%q %q", agentsMsg.Role, callerMsg.Role)
	}
	var env struct {
		Notice string `json:"notice"`
		Files  []struct{ Path, Scope, Text string }
	}
	if err := json.Unmarshal([]byte(agentsMsg.Text()), &env); err != nil || len(env.Files) != 1 || env.Files[0].Text != "Always run the formatter.\n" || env.Files[0].Scope != "." {
		t.Fatalf("%v %s", err, agentsMsg.Text())
	}
	if strings.Contains(agentsMsg.Text(), r.layout.Work) {
		t.Fatal("a path of the host is in the block")
	}
	// What was stored: the block is an item of the Thread with no source (origin host), and
	// the record of hashes is private Evidence of the Run.
	if r.val("SELECT COUNT(*) FROM items WHERE thread_id=? AND history_kind='HostContext' AND origin='host'", info.ThreadID) != "2" {
		t.Fatalf("the Run's blocks are not the caller's and the host's")
	}
	if !strings.Contains(r.purposes(start.RunID), "host_assets_manifest") {
		t.Fatalf("%s", r.purposes(start.RunID))
	}
	var manifest map[string]any
	id := r.val("SELECT evidence_id FROM evidence WHERE run_id=? AND json_extract(metadata_json,'$.purpose')='host_assets_manifest'", start.RunID)
	if err := json.Unmarshal([]byte(r.evidence(id)), &manifest); err != nil {
		t.Fatal(err)
	}
	ag := manifest["agents"].(map[string]any)
	sum := ag["files"].([]any)[0].(map[string]any)["sha256"]
	if sum == "" || ag["snapshot_hash"] == "" || strings.Contains(r.evidence(id), r.layout.Work) {
		t.Fatalf("%v", manifest)
	}
	// The Model editing AGENTS.md later does not change this Run's rules: it was read once.
	r.write("AGENTS.md", "Changed after the Run began.\n")
	if _, text := find(firstPrompt(fake), "Changed after"); text >= 0 {
		t.Fatal("a later edit reached a Run that had begun")
	}
}

func TestNothingIsGivenWhenTheExtensionsAreOffOrTheWorkspaceIsNotTrusted(t *testing.T) {
	for name, c := range map[string]extCfg{
		"the extensions are off":       {enabled: false, trusted: true},
		"the workspace is not trusted": {enabled: true, trusted: false},
	} {
		t.Run(name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			r := assetRig(t, fake, c)
			r.write("AGENTS.md", "rules the host must not pass on\n")
			r.write(".agents/skills/s/SKILL.md", skillText("s", "d", "body"))
			info := r.openSession("asset.off.open.0000000001")
			start := r.startRun(info.ThreadID, "asset.off.start.000000001", "go")
			if run := r.waitTerminal(start.RunID); run.Result.Status != "completed" {
				t.Fatalf("%+v", run.Result)
			}
			if _, at := find(firstPrompt(fake), "rules the host must not pass on"); at >= 0 {
				t.Fatal("the AGENTS.md was given")
			}
			if _, at := find(firstPrompt(fake), "rencrow-host-skills"); at >= 0 {
				t.Fatal("the skill catalog was given")
			}
			if strings.Contains(r.purposes(start.RunID), "host_assets_manifest") || strings.Contains(r.purposes(start.RunID), "skill_file") {
				t.Fatalf("%s", r.purposes(start.RunID))
			}
			// The file stays readable as data by the ordinary Tool.
		})
	}
}

// TestTheModelReadsASkillWithTheEvidenceToolAndNothingElse: the catalog names a skill by an
// Evidence ID; the model reads it with evidence.read as it reads any Evidence, and the
// answer is the file's text. No Tool was added.
func TestTheModelReadsASkillWithTheEvidenceToolAndNothingElse(t *testing.T) {
	fake := harnesstest.NewFake()
	fake.SetReply(harnesstest.Reply{Dynamic: func(req modelport.ChatRequest) harnesstest.Reply {
		for _, msg := range req.Messages {
			if msg.Role == "tool" {
				return harnesstest.Final("read it")
			}
		}
		// Ask for the skill the catalog names.
		for _, msg := range req.Messages {
			if strings.Contains(msg.Text(), "rencrow-host-skills/v1") {
				body := strings.TrimPrefix(msg.Text(), "RENCROW_CONTEXT_DATA_V1\n")
				var data struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal([]byte(body), &data); err != nil {
					return harnesstest.Final("the catalog is not in the data envelope: " + err.Error())
				}
				var cat struct {
					Skills []struct {
						Name       string `json:"name"`
						EvidenceID string `json:"evidence_id"`
						Total      int    `json:"total_bytes"`
					} `json:"skills"`
				}
				if err := json.Unmarshal([]byte(data.Text), &cat); err != nil || len(cat.Skills) != 1 {
					return harnesstest.Final("the catalog is not readable")
				}
				return calls(tc("c1", "evidence.read", evidenceArgs(cat.Skills[0].EvidenceID, "text/v1", 0, cat.Skills[0].Total)))
			}
		}
		return harnesstest.Final("no catalog")
	}})
	r := assetRig(t, fake, extCfg{enabled: true, trusted: true})
	const skill = "---\nname: format-code\ndescription: How to format the code of this project\n---\n# Formatting\nRun `gofmt -l .` first.\n"
	r.write(".agents/skills/format-code/SKILL.md", skill)
	info := r.openSession("asset.skill.open.000000001")
	start := r.startRun(info.ThreadID, "asset.skill.start.00000001", "format it")
	run := r.waitTerminal(start.RunID)
	if run.Result.Status != "completed" || run.Result.FinalText != "read it" {
		t.Fatalf("%+v", run.Result)
	}
	g := fake.Generates()
	if len(g) != 2 {
		t.Fatalf("%d generations", len(g))
	}
	// The Tool catalog of the request is the policy's: no skill Tool was added.
	for _, tl := range g[0].Tools {
		if strings.Contains(tl.Function.Name, "skill") {
			t.Fatalf("a Tool was added: %s", tl.Function.Name)
		}
	}
	views := toolMessages(t, g[1])
	if len(views) != 1 || views[0].EffectState != "completed" {
		t.Fatalf("%+v %+v", views, views[0].Error)
	}
	res := resultOf(t, views[0])
	if res["data"] != skill || res["raw_hash"] != sha(skill) {
		t.Fatalf("%v", res)
	}
	// The catalog message carries the name and the description, not the body.
	cat, _ := find(g[0].Messages, "rencrow-host-skills/v1")
	if strings.Contains(cat.Text(), "gofmt") || !strings.Contains(cat.Text(), "How to format the code") {
		t.Fatalf("%s", cat.Text())
	}
	if !strings.Contains(r.purposes(start.RunID), "skill_file") {
		t.Fatalf("%s", r.purposes(start.RunID))
	}
}

func TestNoCatalogIsGivenToARunThatCannotReadEvidence(t *testing.T) {
	fake := harnesstest.NewFake()
	r := assetRig(t, fake, extCfg{enabled: true, trusted: true, tools: []string{"file.read"}})
	r.write(".agents/skills/s/SKILL.md", skillText("s", "d", "body"))
	r.write("AGENTS.md", "rules\n")
	info := r.openSession("asset.noev.open.0000000001")
	start := r.startRun(info.ThreadID, "asset.noev.start.000000001", "go")
	r.waitTerminal(start.RunID)
	if _, at := find(firstPrompt(fake), "rencrow-host-skills"); at >= 0 {
		t.Fatal("a catalog was given to a Run that cannot follow it")
	}
	if _, at := find(firstPrompt(fake), "rencrow-host-agents"); at < 0 {
		t.Fatal("the AGENTS.md was left out")
	}
}

func TestTheHostsOwnSkillRootsAreGivenToAWorkspaceThatIsNotTrusted(t *testing.T) {
	host := t.TempDir()
	host, _ = filepath.EvalSymlinks(host)
	if err := os.MkdirAll(filepath.Join(host, "managed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "managed", "SKILL.md"), []byte(skillText("managed", "the host's own", "body")), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := harnesstest.NewFake()
	r := assetRig(t, fake, extCfg{enabled: true, trusted: false, skillRoots: []string{host}})
	r.write(".agents/skills/mine/SKILL.md", skillText("mine", "the workspace's own", "body"))
	r.write("AGENTS.md", "not trusted\n")
	info := r.openSession("asset.root.open.0000000001")
	start := r.startRun(info.ThreadID, "asset.root.start.000000001", "go")
	r.waitTerminal(start.RunID)
	cat, at := find(firstPrompt(fake), "rencrow-host-skills")
	if at < 0 || !strings.Contains(cat.Text(), "managed") || strings.Contains(cat.Text(), "the workspace's own") || strings.Contains(cat.Text(), host) {
		t.Fatalf("%v", cat.Text())
	}
	if _, at := find(firstPrompt(fake), "not trusted"); at >= 0 {
		t.Fatal("an untrusted workspace's AGENTS.md was given")
	}
}

// TestAnAssetOverALimitRefusesTheRunBeforeAnythingIsMade: CONTEXT_ASSET_TOO_LARGE, and
// nothing exists afterwards (no turn, no Run, no item, no Evidence), the Thread is as it was.
func TestAnAssetOverALimitRefusesTheRunBeforeAnythingIsMade(t *testing.T) {
	for name, setup := range map[string]func(r *rig){
		"an AGENTS.md over 32 KiB": func(r *rig) { r.write("AGENTS.md", strings.Repeat("a", 32*1024+1)) },
		"a SKILL.md over 64 KiB":   func(r *rig) { r.write(".agents/skills/s/SKILL.md", skillText("s", "d", strings.Repeat("a", 64*1024))) },
		"a header over 8 KiB": func(r *rig) {
			var meta strings.Builder
			for i := 0; i < 9; i++ {
				meta.WriteString("  k" + string(rune('a'+i)) + ": " + strings.Repeat("v", 1000) + "\n")
			}
			r.write(".agents/skills/s/SKILL.md", "---\nname: s\ndescription: d\nmetadata:\n"+meta.String()+"---\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := harnesstest.NewFake()
			r := assetRig(t, fake, extCfg{enabled: true, trusted: true})
			setup(r)
			info := r.openSession("asset.big.open.00000000001")
			before := [...]string{r.val("SELECT COUNT(*) FROM turns"), r.val("SELECT COUNT(*) FROM runs"), r.val("SELECT COUNT(*) FROM items"), r.val("SELECT COUNT(*) FROM evidence"),
				r.val("SELECT writer_epoch FROM threads"), r.val("SELECT event_seq FROM threads")}
			_, err := r.call("turn/start", startParams(info.ThreadID, "asset.big.start.0000000001", "go"))
			var pe *protocol.Error
			if !errors.As(err, &pe) || pe.Code != protocol.CodeContextAssetTooLarge || pe.Retryable {
				t.Fatalf("%v", err)
			}
			after := [...]string{r.val("SELECT COUNT(*) FROM turns"), r.val("SELECT COUNT(*) FROM runs"), r.val("SELECT COUNT(*) FROM items"), r.val("SELECT COUNT(*) FROM evidence"),
				r.val("SELECT writer_epoch FROM threads"), r.val("SELECT event_seq FROM threads")}
			if before != after {
				t.Fatalf("a refused Run left something behind: %v -> %v", before, after)
			}
			if len(fake.Generates()) != 0 {
				t.Fatal("a refused Run reached the model")
			}
		})
	}
}

func TestAnAssetThatIsNotAcceptableRefusesTheRun(t *testing.T) {
	fake := harnesstest.NewFake()
	r := assetRig(t, fake, extCfg{enabled: true, trusted: true})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "AGENTS.md"), []byte("outside\n"), 0o644); err != nil {
		t.Skip(err)
	}
	if err := os.Symlink(filepath.Join(outside, "AGENTS.md"), filepath.Join(r.layout.Work, "AGENTS.md")); err != nil {
		t.Skip("a symbolic link cannot be made here")
	}
	info := r.openSession("asset.link.open.00000000001")
	_, err := r.call("turn/start", startParams(info.ThreadID, "asset.link.start.0000000001", "go"))
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Code != protocol.CodeInvalidExtension {
		t.Fatalf("%v", err)
	}
	if r.val("SELECT COUNT(*) FROM runs") != "0" || len(fake.Generates()) != 0 {
		t.Fatal("a refused Run was made")
	}
	// Removing what was wrong makes the same Thread work.
	if err := os.Remove(filepath.Join(r.layout.Work, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	start := r.startRun(info.ThreadID, "asset.link.start.0000000002", "go")
	if run := r.waitTerminal(start.RunID); run.Result.Status != "completed" {
		t.Fatalf("%+v", run.Result)
	}
}

// TestTheSameRequestAgainIsAnsweredFromItsReceiptWhateverTheFilesAreNow: a replay needs no
// asset, so a file that has become too large since does not turn a request that was
// accepted into a refused one.
func TestTheSameRequestAgainIsAnsweredFromItsReceiptWhateverTheFilesAreNow(t *testing.T) {
	fake := harnesstest.NewFake()
	r := assetRig(t, fake, extCfg{enabled: true, trusted: true})
	r.write("AGENTS.md", "small\n")
	info := r.openSession("asset.replay.open.0000000001")
	params := startParams(info.ThreadID, "asset.replay.start.000000001", "go")
	firstRes := r.mustCall("turn/start", params)
	first := decode[protocol.StartResult](t, firstRes)
	firstRes.Done()
	r.waitTerminal(first.RunID)
	r.write("AGENTS.md", strings.Repeat("b", 40*1024))
	res, err := r.call("turn/start", params)
	if err != nil {
		t.Fatalf("a replay was refused: %v", err)
	}
	if again := decode[protocol.StartResult](t, res); again.RunID != first.RunID || again.ReceiptID != first.ReceiptID || len(res.Events) != 0 {
		t.Fatalf("%+v", again)
	}
	if r.val("SELECT COUNT(*) FROM runs") != "1" {
		t.Fatal("a replay made another Run")
	}
}
