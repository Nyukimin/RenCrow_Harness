package extensions

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Nyukimin/RenCrow_Harness/internal/tools/files"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolerr"
)

// maxRootEntries bounds the listing of one skill root: a directory with more entries than
// this is not a skills directory the host reads.
const maxRootEntries = 4096

// SkillRoot is one directory skills are looked for in: each directory directly inside it
// that holds a SKILL.md is one skill.
type SkillRoot struct {
	// Label names the root in the records ("workspace", "root-1"): never its path.
	Label string
	// Workspace is the real root of the workspace a root inside it belongs to; Base is the
	// root's workspace-relative path (".agents/skills"). For a root of the host's own
	// (extensions.skill_roots) Workspace is the root itself and Base is ".".
	Workspace string
	Base      string
	// Optional: a root that does not exist is no skills, not an error (the workspace's own
	// .agents/skills is not required to be there).
	Optional bool
}

// Skill is one SKILL.md as it was, with its header read.
type Skill struct {
	SkillMetadata
	Root   string
	Dir    string
	SHA256 string
	Bytes  []byte
	// EvidenceID is the Evidence the whole file is sealed as; set when the assets are planned.
	EvidenceID string
}

// SkillCatalog is every skill of the roots, in the order of the roots and, inside a root,
// of the directory names in UTF-8 order.
type SkillCatalog struct{ Skills []Skill }

// LoadSkills reads the skills of the roots. Only a SKILL.md directly inside a directory
// directly inside a root is read; a link anywhere on the way (a root's entry, the SKILL.md,
// a component of the path) is refused, and so is a file that is not valid UTF-8, a header
// outside the subset (ParseSkill), two skills with one name (the catalog is ambiguous and
// is refused whole), and a root that is listed but cannot be read. More than 128 skills,
// a file over 64 KiB and a header over 8 KiB are ErrAssetTooLarge. Nothing is run and nothing
// in a file is followed: a link in the body is text.
func LoadSkills(roots []SkillRoot) (SkillCatalog, error) {
	var cat SkillCatalog
	names := map[string]bool{}
	for _, root := range roots {
		got, err := loadSkillRoot(root)
		if err != nil {
			return SkillCatalog{}, err
		}
		for _, sk := range got {
			if names[sk.Name] {
				return SkillCatalog{}, invalidAsset("two skills have one name, so the catalog is ambiguous")
			}
			names[sk.Name] = true
			cat.Skills = append(cat.Skills, sk)
			if len(cat.Skills) > MaxSkills {
				return SkillCatalog{}, tooLarge("there are more than %d skills", MaxSkills)
			}
		}
	}
	return cat, nil
}

func loadSkillRoot(root SkillRoot) ([]Skill, error) {
	sc := files.Scope{Root: root.Workspace, ReadPrefixes: []string{"."}}
	dir, err := sc.ResolveDir(root.Base)
	if err != nil {
		te, _ := toolerr.As(err)
		if root.Optional && te != nil && (te.Code == toolerr.CodeNotFound || te.Code == toolerr.CodeParentMissing) {
			return nil, nil
		}
		return nil, invalidAsset("a skill root cannot be read")
	}
	entries, err := os.ReadDir(dir.Abs)
	if err != nil {
		return nil, invalidAsset("a skill root cannot be listed")
	}
	if len(entries) > maxRootEntries {
		return nil, tooLarge("a skill root has more than %d entries", maxRootEntries)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		switch {
		case e.Type()&os.ModeSymlink != 0 || e.Type()&os.ModeIrregular != 0:
			return nil, invalidAsset("a skill root has a link in it")
		case e.IsDir():
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []Skill
	for _, name := range names {
		rel := name + "/SKILL.md"
		if root.Base != "." {
			rel = root.Base + "/" + rel
		}
		if len(rel) > MaxAssetPathBytes {
			return nil, tooLarge("a path is longer than %d bytes", MaxAssetPathBytes)
		}
		// Only a file spelled exactly SKILL.md is one (see LoadAgents).
		skillDir, err := sc.ResolveDir(strings.TrimSuffix(rel, "/SKILL.md"))
		if err != nil {
			return nil, invalidAsset("a directory of a skill root cannot be read")
		}
		if ok, err := hasEntry(skillDir.Abs, "SKILL.md"); err != nil {
			return nil, invalidAsset("a directory of a skill root cannot be listed")
		} else if !ok {
			continue // a directory that holds no SKILL.md is not a skill
		}
		_, data, sum, err := files.ReadWhole(sc, rel, MaxSkillFileBytes)
		if err != nil {
			te, _ := toolerr.As(err)
			switch {
			case te != nil && (te.Code == toolerr.CodeNotFound || te.Code == toolerr.CodeParentMissing):
				continue // a directory that holds no SKILL.md is not a skill
			case te != nil && te.Code == toolerr.CodeFileTooLarge:
				return nil, tooLarge("a SKILL.md is larger than %d bytes", MaxSkillFileBytes)
			}
			return nil, invalidAsset("a SKILL.md is not a regular file inside its root")
		}
		md, err := ParseSkill(data)
		if err != nil {
			return nil, err
		}
		out = append(out, Skill{SkillMetadata: md, Root: root.Label, Dir: name, SHA256: sum, Bytes: data})
	}
	return out, nil
}

// realSkillRoot resolves a configured skill root (an absolute path the host chose) to the
// directory it is: the host's own roots may be reached through a link of its own making
// (a temporary directory, a mounted volume), and the walk inside the root then never
// follows another.
func realSkillRoot(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", invalidAsset("a skill root is not an absolute path")
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", invalidAsset("a skill root does not exist")
		}
		return "", invalidAsset("a skill root cannot be resolved")
	}
	return real, nil
}
