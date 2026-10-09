// Package sourcecandidate builds and verifies a private snapshot of the public
// source tree used by the Harness check plan.
package sourcecandidate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/files"
)

const (
	ManifestSchema  = "rencrow.harness-source-candidate.v1"
	StatusCandidate = "candidate"
	ManifestName    = "manifest.json"
	CandidateName   = "candidate"
	maxManifestSize = 128 << 20
)

var excludedRoots = []string{".git", "Tmp"}

// Manifest describes the exact source snapshot. It contains no absolute paths,
// timestamps, or host identifiers.
type Manifest struct {
	Schema        string   `json:"schema"`
	Status        string   `json:"status"`
	CandidateID   string   `json:"candidateId"`
	ExcludedRoots []string `json:"excludedRoots"`
	Entries       []Entry  `json:"entries"`
}

// Entry is one regular directory or file in the candidate, using slash paths.
type Entry struct {
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	PortableMode string `json:"portableMode"`
	ModeSource   string `json:"modeSource,omitempty"`
	WorkingMode  string `json:"workingMode,omitempty"`
	ModeDirty    bool   `json:"modeDirty,omitempty"`
}

type sourceEntry struct {
	Entry
	absPath string
	info    fs.FileInfo
	rawMode fs.FileMode
}

type manifestPayload struct {
	Schema        string   `json:"schema"`
	ExcludedRoots []string `json:"excludedRoots"`
	Entries       []Entry  `json:"entries"`
}

// Capture creates one fresh private workspace under ownerRunsRoot. The source
// must be the root of its Git repository. Only root .git and root Tmp are
// omitted; ignored files and nested Tmp/.git trees remain source entries.
func Capture(sourceRoot, ownerRunsRoot, workspace string) (Manifest, error) {
	if err := fsperm.CheckProcessOwner(); err != nil {
		return Manifest{}, errors.New("source candidate process owner is not verifiable")
	}
	source, err := existingRealDirectory(sourceRoot)
	if err != nil {
		return Manifest{}, errors.New("source root must be an existing real directory")
	}
	runs, err := existingRealDirectory(ownerRunsRoot)
	if err != nil {
		return Manifest{}, errors.New("owner _runs root must be an existing real directory")
	}
	if err := validateOwnerRunsRoot(source, runs); err != nil {
		return Manifest{}, err
	}
	workspaceAbs, err := validateFreshWorkspacePath(runs, workspace)
	if err != nil {
		return Manifest{}, err
	}
	if err := requireGitRoot(source); err != nil {
		return Manifest{}, err
	}
	if err := rejectTrackedRootTmp(source); err != nil {
		return Manifest{}, err
	}
	indexModesBefore, err := gitIndexModes(source)
	if err != nil {
		return Manifest{}, err
	}

	before, err := inventory(source, true, indexModesBefore, nil)
	if err != nil {
		return Manifest{}, err
	}
	if err := fsperm.CreatePrivateDir(workspaceAbs); err != nil {
		return Manifest{}, errors.New("private source workspace could not be created")
	}
	created := true
	complete := false
	defer func() {
		if created && !complete {
			_ = os.RemoveAll(workspaceAbs)
		}
	}()

	candidate := filepath.Join(workspaceAbs, CandidateName)
	if err := fsperm.CreatePrivateDir(candidate); err != nil {
		return Manifest{}, errors.New("private candidate directory could not be created")
	}
	if err := copyInventory(source, before, candidate); err != nil {
		return Manifest{}, err
	}

	after, err := inventory(source, true, indexModesBefore, nil)
	if err != nil {
		return Manifest{}, err
	}
	if !sameSourceState(before, after) {
		return Manifest{}, errors.New("source changed while the candidate was being captured")
	}
	if err := rejectTrackedRootTmp(source); err != nil {
		return Manifest{}, err
	}
	indexModesAfter, err := gitIndexModes(source)
	if err != nil {
		return Manifest{}, err
	}
	if !sameGitModes(indexModesBefore, indexModesAfter) {
		return Manifest{}, errors.New("Git index file modes changed while the candidate was being captured")
	}
	copied, err := inventory(candidate, false, nil, entriesOnly(before))
	if err != nil {
		return Manifest{}, err
	}
	if !sameEntries(entriesOnly(before), entriesOnly(copied)) {
		return Manifest{}, errors.New("copied candidate does not match the source snapshot")
	}
	if err := fsperm.CheckOwnerOnlyDirForChildren(workspaceAbs); err != nil {
		return Manifest{}, errors.New("private source workspace could not be verified")
	}
	if err := fsperm.CheckOwnerOnlyDirForChildren(candidate); err != nil {
		return Manifest{}, errors.New("private candidate directory could not be verified")
	}

	manifest := Manifest{
		Schema:        ManifestSchema,
		Status:        StatusCandidate,
		ExcludedRoots: append([]string{}, excludedRoots...),
		Entries:       entriesOnly(copied),
	}
	manifest.CandidateID, err = candidateID(manifestPayload{
		Schema:        manifest.Schema,
		ExcludedRoots: manifest.ExcludedRoots,
		Entries:       manifest.Entries,
	})
	if err != nil {
		return Manifest{}, errors.New("candidate manifest identity could not be calculated")
	}
	if err := writeManifest(filepath.Join(workspaceAbs, ManifestName), manifest); err != nil {
		return Manifest{}, err
	}
	complete = true
	return manifest, nil
}

// Verify checks the private workspace, manifest identity, and every candidate
// entry. It does not read or select source files.
func Verify(ownerRunsRoot, workspace string) (Manifest, error) {
	if err := fsperm.CheckProcessOwner(); err != nil {
		return Manifest{}, errors.New("source candidate process owner is not verifiable")
	}
	runs, err := existingRealDirectory(ownerRunsRoot)
	if err != nil {
		return Manifest{}, errors.New("owner _runs root must be an existing real directory")
	}
	workspaceAbs, err := existingContainedDirectory(runs, workspace)
	if err != nil {
		return Manifest{}, errors.New("source workspace is outside owner _runs or is not a real directory")
	}
	candidate := filepath.Join(workspaceAbs, CandidateName)
	manifestPath := filepath.Join(workspaceAbs, ManifestName)
	if err := fsperm.CheckOwnerOnlyDirForChildren(workspaceAbs); err != nil {
		return Manifest{}, errors.New("private source workspace could not be verified")
	}
	if err := fsperm.CheckOwnerOnlyDirForChildren(candidate); err != nil {
		return Manifest{}, errors.New("private candidate directory could not be verified")
	}
	if err := fsperm.CheckOwnerOnlyFile(manifestPath); err != nil {
		return Manifest{}, errors.New("private candidate manifest could not be verified")
	}
	manifest, err := readManifest(manifestPath)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	actual, err := inventory(candidate, false, nil, manifest.Entries)
	if err != nil {
		return Manifest{}, err
	}
	if !sameEntries(manifest.Entries, entriesOnly(actual)) {
		return Manifest{}, errors.New("candidate contents changed after capture")
	}
	return manifest, nil
}

// PrepareLogDirectory creates one fresh owner-only directory for source-candidate
// command evidence. The path must resolve below this repository's owner _runs root.
func PrepareLogDirectory(ownerRunsRoot, directory string) error {
	if err := fsperm.CheckProcessOwner(); err != nil {
		return errors.New("source candidate process owner is not verifiable")
	}
	runs, err := existingRealDirectory(ownerRunsRoot)
	if err != nil {
		return errors.New("owner _runs root must be an existing real directory")
	}
	source := filepath.Dir(filepath.Dir(filepath.Dir(runs)))
	if err := validateOwnerRunsRoot(source, runs); err != nil {
		return err
	}
	logDirectory, err := validateFreshWorkspacePath(runs, directory)
	if err != nil {
		return errors.New("source candidate log directory must be fresh and inside owner _runs")
	}
	if err := fsperm.CreatePrivateDir(logDirectory); err != nil {
		return errors.New("private source candidate log directory could not be created")
	}
	if err := fsperm.CheckOwnerOnlyDirForChildren(logDirectory); err != nil {
		return errors.New("private source candidate log directory could not be verified")
	}
	return nil
}

func validateOwnerRunsRoot(source, runs string) error {
	expected := filepath.Join(source, "Tmp", "test-runtime", "_runs")
	expected, err := filepath.Abs(expected)
	if err != nil || !samePath(expected, runs) {
		return errors.New("owner _runs root must be source-root Tmp/test-runtime/_runs")
	}
	return nil
}

func existingRealDirectory(candidate string) (string, error) {
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	if err := rejectReparseAncestors(abs); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(resolved)
	reparse := false
	if err == nil {
		reparse, err = isReparsePoint(resolved, info)
	}
	if err != nil || !info.IsDir() || reparse {
		return "", errors.New("not a plain directory")
	}
	return filepath.Clean(resolved), nil
}

func validateFreshWorkspacePath(runs, workspace string) (string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil || !withinPath(runs, abs) {
		return "", errors.New("source workspace must be inside owner _runs")
	}
	if _, err := os.Lstat(abs); err == nil || !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("source workspace must be fresh")
	}
	parent, err := existingRealDirectory(filepath.Dir(abs))
	if err != nil || !withinPath(runs, parent) {
		return "", errors.New("source workspace parent must resolve inside owner _runs")
	}
	if err := rejectReparseAncestors(filepath.Dir(abs)); err != nil {
		return "", errors.New("source workspace parent must not contain a symlink or reparse point")
	}
	return filepath.Clean(abs), nil
}

func existingContainedDirectory(parent, candidate string) (string, error) {
	abs, err := filepath.Abs(candidate)
	if err != nil || !withinPath(parent, abs) {
		return "", errors.New("directory is outside owner _runs")
	}
	if err := rejectReparseAncestors(abs); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil || !withinPath(parent, resolved) {
		return "", errors.New("directory does not resolve inside owner _runs")
	}
	info, err := os.Lstat(resolved)
	reparse := false
	if err == nil {
		reparse, err = isReparsePoint(resolved, info)
	}
	if err != nil || !info.IsDir() || reparse {
		return "", errors.New("directory is not plain")
	}
	return filepath.Clean(resolved), nil
}

func requireGitRoot(source string) error {
	command := gitCommand(source, "rev-parse", "--show-prefix")
	output, err := command.Output()
	if err != nil || string(output) != "\n" && string(output) != "\r\n" {
		return errors.New("source root must be a Git repository root")
	}
	return nil
}

func rejectTrackedRootTmp(source string) error {
	command := gitCommand(source, "ls-files", "--cached", "-z")
	output, err := command.Output()
	if err != nil {
		return errors.New("tracked root Tmp content could not be checked")
	}
	for _, tracked := range strings.Split(string(output), "\x00") {
		if tracked == "" {
			continue
		}
		if err := validateTrackedRootPath(tracked); err != nil {
			return err
		}
	}
	return nil
}

func validateTrackedRootPath(tracked string) error {
	rootName, _, _ := strings.Cut(filepath.ToSlash(tracked), "/")
	if isRootExclusionAlias(rootName) {
		return errors.New("source tree contains an ambiguous root exclusion name")
	}
	if sameRootName(rootName, "Tmp") {
		return errors.New("tracked root Tmp content cannot be excluded from the candidate")
	}
	return nil
}

func gitCommand(source string, args ...string) *exec.Cmd {
	commandArgs := append([]string{"-C", source}, args...)
	command := exec.Command("git", commandArgs...)
	cleanEnv := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(key, "GIT_") {
			cleanEnv = append(cleanEnv, value)
		}
	}
	command.Env = append(cleanEnv, "GIT_OPTIONAL_LOCKS=0")
	return command
}

func gitIndexModes(source string) (map[string]string, error) {
	command := gitCommand(source, "ls-files", "--stage", "-z")
	output, err := command.Output()
	if err != nil || len(output) > 0 && output[len(output)-1] != 0 {
		return nil, errors.New("Git index file modes could not be read completely")
	}
	modes := make(map[string]string)
	for _, record := range strings.Split(string(output), "\x00") {
		if record == "" {
			continue
		}
		header, pathName, ok := strings.Cut(record, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 || fields[2] != "0" || pathName == "" {
			return nil, errors.New("Git index contains an unsupported stage or entry")
		}
		if _, exists := modes[pathName]; exists {
			return nil, errors.New("Git index contains duplicate source paths")
		}
		modes[pathName] = fields[0]
	}
	return modes, nil
}

func sameGitModes(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for pathName, mode := range left {
		if right[pathName] != mode {
			return false
		}
	}
	return true
}

func inventory(root string, skipSourceRoots bool, indexModes map[string]string, expectedEntries []Entry) ([]sourceEntry, error) {
	scope := files.Scope{Root: root, ReadPrefixes: []string{"."}}
	var entries []sourceEntry
	foldedNames := make(map[string]string)
	expectedByPath := make(map[string]Entry, len(expectedEntries))
	for _, entry := range expectedEntries {
		expectedByPath[entry.Path] = entry
	}
	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("source tree could not be enumerated")
		}
		if abs == root {
			return nil
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return errors.New("source entry path could not be normalized")
		}
		rel = filepath.ToSlash(rel)
		if skipSourceRoots && isRootExclusionAlias(rel) {
			return errors.New("source tree contains an ambiguous root exclusion name")
		}
		if skipSourceRoots && (rel == ".git" || rel == "Tmp") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !utf8.ValidString(rel) || !portableRelativePath(rel) {
			return errors.New("source tree contains a non-portable relative path")
		}
		if err := rejectCaseFoldedPathAlias(rel, foldedNames); err != nil {
			return err
		}
		info, err := os.Lstat(abs)
		if err != nil {
			return errors.New("source entry could not be inspected")
		}
		reparse, err := isReparsePoint(abs, info)
		if err != nil {
			return errors.New("source entry reparse state could not be verified")
		}
		if reparse || info.Mode()&fs.ModeSymlink != 0 {
			return errors.New("source tree contains a symlink or reparse point")
		}
		resolved, err := scope.Resolve(rel, files.Read)
		if err != nil || !resolved.Exists || !os.SameFile(info, resolved.Info) {
			return errors.New("source path containment could not be verified")
		}
		entry := sourceEntry{absPath: abs, info: info, rawMode: info.Mode().Perm()}
		switch {
		case info.IsDir():
			if info.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
				return errors.New("source tree contains a non-portable permission mode")
			}
			entry.Entry = Entry{Path: rel, Kind: "directory", Size: 0, PortableMode: portableDirectoryMode(info.Mode())}
		case info.Mode().IsRegular():
			if info.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
				return errors.New("source tree contains a non-portable permission mode")
			}
			mode := portableFileModeForOS(info.Mode())
			modeSource := "working-tree"
			workingMode := ""
			modeDirty := false
			if expected, ok := expectedByPath[rel]; ok && expected.Kind == "file" {
				mode = expected.PortableMode
				modeSource = expected.ModeSource
				workingMode = expected.WorkingMode
				modeDirty = expected.ModeDirty
				if !candidatePortableModeMatches(info, mode) {
					return errors.New("candidate file mode does not match its portable mode")
				}
			} else if gitMode, tracked := indexModes[rel]; tracked {
				var supported bool
				mode, supported = portableModeForGitIndex(gitMode)
				if !supported {
					return errors.New("tracked source file has an unsupported Git index mode")
				}
				modeSource = "git-index"
				if runtime.GOOS != "windows" {
					workingMode = portableFileModeForOS(info.Mode())
					if workingMode != mode {
						modeDirty = true
					} else {
						workingMode = ""
					}
				}
			} else if runtime.GOOS == "windows" {
				modeSource = "platform-default"
			}
			size, hash, err := hashRegularFile(scope, rel, abs, info)
			if err != nil {
				return err
			}
			entry.Entry = Entry{
				Path:         rel,
				Kind:         "file",
				Size:         size,
				SHA256:       hash,
				PortableMode: mode,
				ModeSource:   modeSource,
				WorkingMode:  workingMode,
				ModeDirty:    modeDirty,
			}
		default:
			return errors.New("source tree contains a non-regular entry")
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func hashRegularFile(scope files.Scope, relative, pathName string, observed fs.FileInfo) (int64, string, error) {
	resolved, err := scope.Resolve(relative, files.Read)
	if err != nil || !resolved.Exists || !os.SameFile(observed, resolved.Info) {
		return 0, "", errors.New("source path changed before it was read")
	}
	file, err := openRegularNoFollow(pathName)
	if err != nil {
		return 0, "", errors.New("source file could not be opened")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(observed, opened) || isReparseOrSymlink(pathName, opened) {
		return 0, "", errors.New("source file changed during inspection")
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return 0, "", errors.New("source file could not be read")
	}
	after, err := file.Stat()
	afterPath, pathErr := scope.Resolve(relative, files.Read)
	if err != nil || pathErr != nil || !afterPath.Exists || !os.SameFile(afterPath.Info, after) ||
		!os.SameFile(opened, after) || size != after.Size() || after.Mode().Perm() != observed.Mode().Perm() {
		return 0, "", errors.New("source file changed during inspection")
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func copyInventory(sourceRoot string, entries []sourceEntry, destination string) error {
	sourceScope := files.Scope{Root: sourceRoot, ReadPrefixes: []string{"."}}
	for _, entry := range entries {
		if entry.Kind != "directory" {
			continue
		}
		pathName := filepath.Join(destination, filepath.FromSlash(entry.Path))
		if err := fsperm.CreatePrivateDir(pathName); err != nil {
			return errors.New("candidate directory could not be created privately")
		}
	}
	for _, entry := range entries {
		if entry.Kind != "file" {
			continue
		}
		sourceInfo, err := os.Lstat(entry.absPath)
		resolved, resolveErr := sourceScope.Resolve(entry.Path, files.Read)
		if err != nil || resolveErr != nil || !resolved.Exists || !os.SameFile(entry.info, sourceInfo) || !os.SameFile(resolved.Info, sourceInfo) ||
			isReparseOrSymlink(entry.absPath, sourceInfo) || sourceInfo.Mode().Perm() != entry.rawMode || !sourcePortableModeMatches(sourceInfo, entry.Entry) {
			return errors.New("source file changed before it was copied")
		}
		input, err := openRegularNoFollow(entry.absPath)
		if err != nil {
			return errors.New("source file could not be opened for copying")
		}
		opened, err := input.Stat()
		if err != nil || !opened.Mode().IsRegular() || !os.SameFile(entry.info, opened) || isReparseOrSymlink(entry.absPath, opened) {
			_ = input.Close()
			return errors.New("source file changed before it was copied")
		}
		outPath := filepath.Join(destination, filepath.FromSlash(entry.Path))
		output, err := fsperm.CreatePrivateFile(outPath)
		if err != nil {
			_ = input.Close()
			return errors.New("candidate file could not be created privately")
		}
		hash := sha256.New()
		size, copyErr := io.Copy(io.MultiWriter(output, hash), input)
		afterInput, statErr := input.Stat()
		closeInputErr := input.Close()
		syncErr := output.Sync()
		closeOutputErr := output.Close()
		if copyErr != nil || statErr != nil || closeInputErr != nil || syncErr != nil || closeOutputErr != nil {
			return errors.New("source file could not be copied completely")
		}
		afterPath, pathErr := sourceScope.Resolve(entry.Path, files.Read)
		if pathErr != nil || !afterPath.Exists || !os.SameFile(afterPath.Info, afterInput) ||
			!os.SameFile(opened, afterInput) || size != entry.Size || afterInput.Size() != entry.Size ||
			afterInput.Mode().Perm() != entry.rawMode || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 ||
			!sourcePortableModeMatches(afterInput, entry.Entry) {
			return errors.New("source file changed while it was copied")
		}
		if err := chmodPortable(outPath, modeFromPortable(entry.PortableMode)); err != nil {
			return errors.New("candidate file portable mode could not be set")
		}
	}
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry.Kind != "directory" {
			continue
		}
		pathName := filepath.Join(destination, filepath.FromSlash(entry.Path))
		if err := chmodPortable(pathName, directoryModeFromPortable(entry.PortableMode)); err != nil {
			return errors.New("candidate directory portable mode could not be set")
		}
	}
	return nil
}

func writeManifest(pathName string, manifest Manifest) error {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(manifest); err != nil || encoded.Len() > maxManifestSize {
		return errors.New("candidate manifest could not be encoded within its size limit")
	}
	file, err := fsperm.CreatePrivateFile(pathName)
	if err != nil {
		return errors.New("candidate manifest could not be created privately")
	}
	written, err := file.Write(encoded.Bytes())
	if err != nil || written != encoded.Len() {
		_ = file.Close()
		return errors.New("candidate manifest could not be written completely")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("candidate manifest could not be flushed")
	}
	if err := file.Close(); err != nil {
		return errors.New("candidate manifest could not be closed")
	}
	if err := fsperm.CheckOwnerOnlyFile(pathName); err != nil {
		return errors.New("private candidate manifest could not be verified")
	}
	return nil
}

func readManifest(pathName string) (Manifest, error) {
	observed, err := os.Lstat(pathName)
	if err != nil || !observed.Mode().IsRegular() || isReparseOrSymlink(pathName, observed) || observed.Size() > maxManifestSize {
		return Manifest{}, errors.New("candidate manifest has an invalid file shape")
	}
	file, err := openRegularNoFollow(pathName)
	if err != nil {
		return Manifest{}, errors.New("candidate manifest could not be opened")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(observed, info) || info.Size() > maxManifestSize || isReparseOrSymlink(pathName, info) {
		return Manifest{}, errors.New("candidate manifest has an invalid file shape")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxManifestSize+1))
	if err != nil || len(raw) > maxManifestSize {
		return Manifest{}, errors.New("candidate manifest could not be read completely")
	}
	after, err := file.Stat()
	afterPath, pathErr := os.Lstat(pathName)
	if err != nil || pathErr != nil || !after.Mode().IsRegular() || !os.SameFile(info, after) || !os.SameFile(afterPath, after) ||
		after.Size() != int64(len(raw)) || after.Size() > maxManifestSize || isReparseOrSymlink(pathName, afterPath) {
		return Manifest{}, errors.New("candidate manifest changed while it was read")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, errors.New("candidate manifest is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Manifest{}, errors.New("candidate manifest contains trailing data")
	}
	return manifest, nil
}

func validateManifest(manifest Manifest) error {
	if manifest.Schema != ManifestSchema || manifest.Status != StatusCandidate || !sameStrings(manifest.ExcludedRoots, excludedRoots) {
		return errors.New("candidate manifest contract is invalid")
	}
	if len(manifest.CandidateID) != sha256.Size*2 {
		return errors.New("candidate manifest identity is invalid")
	}
	if _, err := hex.DecodeString(manifest.CandidateID); err != nil {
		return errors.New("candidate manifest identity is invalid")
	}
	for index, entry := range manifest.Entries {
		if !utf8.ValidString(entry.Path) || !portableRelativePath(entry.Path) ||
			isExcludedSourcePath(entry.Path) {
			return errors.New("candidate manifest contains a non-portable or excluded path")
		}
		if index > 0 && manifest.Entries[index-1].Path >= entry.Path {
			return errors.New("candidate manifest entries are not uniquely sorted")
		}
		switch entry.Kind {
		case "directory":
			if entry.Size != 0 || entry.SHA256 != "" || !validPortableDirectoryMode(entry.PortableMode) ||
				entry.ModeSource != "" || entry.WorkingMode != "" || entry.ModeDirty {
				return errors.New("candidate directory entry is invalid")
			}
		case "file":
			if entry.Size < 0 || entry.PortableMode != "0644" && entry.PortableMode != "0755" || len(entry.SHA256) != sha256.Size*2 {
				return errors.New("candidate file entry is invalid")
			}
			if entry.ModeSource != "git-index" && entry.ModeSource != "working-tree" && entry.ModeSource != "platform-default" {
				return errors.New("candidate file mode source is invalid")
			}
			if runtime.GOOS == "windows" && (entry.ModeSource == "working-tree" || entry.ModeDirty) ||
				runtime.GOOS != "windows" && entry.ModeSource == "platform-default" {
				return errors.New("candidate file mode source does not match the current platform")
			}
			if entry.ModeDirty {
				if entry.ModeSource != "git-index" || entry.WorkingMode != "0644" && entry.WorkingMode != "0755" || entry.WorkingMode == entry.PortableMode {
					return errors.New("candidate file dirty mode record is invalid")
				}
			} else if entry.WorkingMode != "" {
				return errors.New("candidate file contains an unused working mode")
			}
			if entry.ModeSource == "platform-default" && entry.PortableMode != "0644" {
				return errors.New("candidate file platform-default mode is invalid")
			}
			if _, err := hex.DecodeString(entry.SHA256); err != nil {
				return errors.New("candidate file digest is invalid")
			}
		default:
			return errors.New("candidate entry kind is invalid")
		}
	}
	if manifest.CandidateID != identityFor(manifest) {
		return errors.New("candidate manifest identity does not match its entries")
	}
	return nil
}

func candidateID(payload manifestPayload) (string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func identityFor(manifest Manifest) string {
	id, err := candidateID(manifestPayload{
		Schema:        manifest.Schema,
		ExcludedRoots: manifest.ExcludedRoots,
		Entries:       manifest.Entries,
	})
	if err != nil {
		return ""
	}
	return id
}

func entriesOnly(entries []sourceEntry) []Entry {
	result := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Entry)
	}
	return result
}

func sameEntries(left, right []Entry) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameSourceState(left, right []sourceEntry) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Entry != right[index].Entry || left[index].rawMode != right[index].rawMode {
			return false
		}
	}
	return true
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func modeFromPortable(mode string) fs.FileMode {
	if mode == "0755" {
		return 0o755
	}
	return 0o644
}

func portableModeForGitIndex(mode string) (string, bool) {
	switch mode {
	case "100644":
		return "0644", true
	case "100755":
		return "0755", true
	default:
		return "", false
	}
}

func sourcePortableModeMatches(info fs.FileInfo, entry Entry) bool {
	if runtime.GOOS == "windows" && entry.ModeSource == "git-index" {
		return true
	}
	observed := portableFileModeForOS(info.Mode())
	if entry.ModeDirty {
		return observed == entry.WorkingMode
	}
	return observed == entry.PortableMode
}

func candidatePortableModeMatches(info fs.FileInfo, portableMode string) bool {
	if runtime.GOOS == "windows" {
		// Windows does not expose the Git executable bit as a file permission.
		// Candidate files are private and writable; the manifest carries the
		// portable mode supplied by Git for tracked files.
		return info.Mode().Perm()&0o200 != 0
	}
	return info.Mode().Perm() == modeFromPortable(portableMode)
}

func validPortableDirectoryMode(mode string) bool {
	if len(mode) != 4 || mode[0] != '0' {
		return false
	}
	for _, digit := range mode[1:] {
		if digit < '0' || digit > '7' {
			return false
		}
	}
	return true
}

func portableRelativePath(value string) bool {
	_, err := files.Normalize(value)
	return err == nil
}

func sameRootName(relative, rootName string) bool {
	if strings.Contains(relative, "/") {
		return false
	}
	return relative == rootName
}

func isExcludedSourcePath(relative string) bool {
	first, _, _ := strings.Cut(relative, "/")
	return sameRootName(first, ".git") || sameRootName(first, "Tmp") || isRootExclusionAlias(first)
}

func isRootExclusionAlias(relative string) bool {
	if strings.Contains(relative, "/") {
		return false
	}
	return strings.EqualFold(relative, ".git") && relative != ".git" ||
		strings.EqualFold(relative, "Tmp") && relative != "Tmp"
}

func rejectCaseFoldedPathAlias(relative string, foldedNames map[string]string) error {
	parent := ""
	for _, segment := range strings.Split(relative, "/") {
		key := parent + "\x00" + simpleFold(segment)
		if previous, exists := foldedNames[key]; exists && previous != segment {
			return errors.New("source tree contains names that collide across operating systems")
		}
		foldedNames[key] = segment
		if parent == "" {
			parent = segment
		} else {
			parent += "/" + segment
		}
	}
	return nil
}

func simpleFold(value string) string {
	var folded strings.Builder
	for _, current := range value {
		minimum := current
		for candidate := unicode.SimpleFold(current); candidate != current; candidate = unicode.SimpleFold(candidate) {
			if candidate < minimum {
				minimum = candidate
			}
		}
		folded.WriteRune(minimum)
	}
	return folded.String()
}

func withinPath(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}

func samePath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func rejectReparseAncestors(pathName string) error {
	abs, err := filepath.Abs(pathName)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	remainder := strings.TrimPrefix(abs, volume)
	current := volume + string(filepath.Separator)
	for _, segment := range strings.Split(strings.Trim(remainder, string(filepath.Separator)), string(filepath.Separator)) {
		if segment == "" {
			continue
		}
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		reparse, err := isReparsePoint(current, info)
		if err != nil {
			return err
		}
		if reparse || info.Mode()&fs.ModeSymlink != 0 {
			return errors.New("path contains a symlink or reparse point")
		}
	}
	return nil
}

func isReparseOrSymlink(pathName string, info fs.FileInfo) bool {
	reparse, err := isReparsePoint(pathName, info)
	return err != nil || reparse || info.Mode()&fs.ModeSymlink != 0
}
