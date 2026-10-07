// Package files implements the four file Tools (file.read, file.search, file.create,
// file.edit) over one workspace.
//
// Every path is a workspace-relative, "/"-separated, normalized path. Containment is
// decided on the real filesystem, one component at a time from the workspace root: a
// component that is a symbolic link, a junction or any other reparse point is refused,
// and so is one whose spelling differs from the name stored in its directory (a case
// or Unicode-normalization alias on a case-insensitive filesystem). A string prefix
// test on the path alone never proves containment. Policy prefixes are then compared
// segment by segment.
//
// A hard link inside the workspace to a file outside it cannot be told from a path:
// containment is a property of names, and a hard link has none to inspect. The Tools
// do not claim to detect one.
//
// The package knows nothing about Runs, Evidence or the store: the dispatcher hands it
// the arguments and keeps what it returns.
package files

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolerr"
)

// Access is what a path is needed for.
type Access int

const (
	// Read needs a read prefix.
	Read Access = iota
	// Write needs a write prefix.
	Write
)

// Limits of the file Tools.
const (
	// MaxPathBytes is the longest path the Tools accept (the schema's limit).
	MaxPathBytes = 4096
	// MaxFileBytes is the largest file file.read hashes and reads a range of.
	MaxFileBytes = 256 << 20
	// MaxEditBytes is the largest file file.edit loads: it must hold the whole file.
	MaxEditBytes = 32 << 20
	// maxDirEntries bounds a directory listing made to verify a name's spelling.
	maxDirEntries = 1 << 20
)

// Scope is the workspace and the prefixes a Run may use in it.
type Scope struct {
	// Root is the real, absolute workspace root.
	Root          string
	ReadPrefixes  []string
	WritePrefixes []string
}

// Resolved is a path that passed containment.
type Resolved struct {
	// Abs is the absolute path; Rel the normalized relative path ("." for the root).
	Abs, Rel string
	Segs     []string
	Exists   bool
	// Info is the Lstat of the path when it exists.
	Info os.FileInfo
}

type resolveOpts struct {
	access Access
	// missingOK lets the last component not exist (its parent must).
	missingOK bool
}

// reservedNames are the device names Windows refuses as file names; they are refused
// on every OS so one path means the same thing everywhere.
var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

func unsafeSegment(seg string) bool {
	if strings.ContainsAny(seg, "<>:\"|?*") {
		return true
	}
	if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
		return true
	}
	base := seg
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return reservedNames[strings.ToLower(strings.TrimRight(base, " "))]
}

// Normalize checks the grammar of a Tool path and returns its segments. It reads no
// filesystem. NUL, an absolute path, a drive letter, a backslash, "..", "." inside a
// path, an empty segment, a name Windows cannot hold and invalid UTF-8 are refused.
func Normalize(p string) ([]string, error) {
	if len(p) > MaxPathBytes {
		return nil, toolerr.Reject(toolerr.CodePathInvalid, "the path is too long")
	}
	if !utf8.ValidString(p) {
		return nil, toolerr.Reject(toolerr.CodePathInvalid, "the path is not valid UTF-8")
	}
	if err := config.ValidatePrefix(p); err != nil {
		return nil, toolerr.Reject(toolerr.CodePathInvalid, "the path is not a normalized workspace-relative path")
	}
	if p == "." {
		return nil, nil
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if unsafeSegment(s) {
			return nil, toolerr.Reject(toolerr.CodePathInvalid, "the path has a name that is not portable")
		}
	}
	return segs, nil
}

func hasPrefixSegs(segs, prefix []string) bool {
	return len(segs) >= len(prefix) && slices.Equal(segs[:len(prefix)], prefix)
}

func prefixSegs(p string) []string {
	if p == "." {
		return nil
	}
	return strings.Split(p, "/")
}

func (s Scope) prefixes(a Access) []string {
	if a == Write {
		return s.WritePrefixes
	}
	return s.ReadPrefixes
}

// Allows reports whether the policy prefixes cover the path itself: it is equal to a
// prefix or below one. An empty prefix list covers nothing.
func (s Scope) Allows(segs []string, a Access) bool {
	for _, p := range s.prefixes(a) {
		if hasPrefixSegs(segs, prefixSegs(p)) {
			return true
		}
	}
	return false
}

// allowsDir reports whether a directory may be entered: it is covered, or it lies
// above a prefix (it holds something that is). Whether each entry below it is covered
// is decided entry by entry.
func (s Scope) allowsDir(segs []string, a Access) bool {
	for _, p := range s.prefixes(a) {
		ps := prefixSegs(p)
		if hasPrefixSegs(segs, ps) || hasPrefixSegs(ps, segs) {
			return true
		}
	}
	return false
}

// Check is the policy half of resolution: grammar and prefix, no filesystem.
func (s Scope) Check(p string, a Access) ([]string, error) {
	segs, err := Normalize(p)
	if err != nil {
		return nil, err
	}
	if !s.Allows(segs, a) {
		return nil, toolerr.Reject(toolerr.CodePathOutside, "the path is outside what the policy allows")
	}
	return segs, nil
}

// Resolve checks a path against the policy and then against the real filesystem.
func (s Scope) resolve(p string, o resolveOpts) (Resolved, error) {
	segs, err := s.Check(p, o.access)
	if err != nil {
		return Resolved{}, err
	}
	return s.walk(segs, o.missingOK)
}

// Resolve checks a path for the access and returns what is there.
func (s Scope) Resolve(p string, a Access) (Resolved, error) {
	return s.resolve(p, resolveOpts{access: a})
}

// walk follows the segments from the workspace root without ever following a link.
func (s Scope) walk(segs []string, missingOK bool) (Resolved, error) {
	if s.Root == "" || !filepath.IsAbs(s.Root) {
		return Resolved{}, toolerr.Fail(toolerr.CodeInternal, "the workspace root is not an absolute path")
	}
	cur := s.Root
	rootInfo, err := os.Lstat(cur)
	if err != nil || !rootInfo.IsDir() || isLink(rootInfo) {
		return Resolved{}, toolerr.Fail(toolerr.CodeIO, "the workspace root is not a directory")
	}
	r := Resolved{Abs: cur, Rel: ".", Segs: segs, Exists: true, Info: rootInfo}
	for i, seg := range segs {
		last := i == len(segs)-1
		next := filepath.Join(cur, seg)
		info, err := os.Lstat(next)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if last && missingOK {
				// A name that differs only in case from an existing one would be the same
				// file on a case-insensitive filesystem: it is not a free name.
				if _, folded, lerr := lookupName(cur, seg); lerr != nil {
					return Resolved{}, lerr
				} else if folded {
					return Resolved{}, toolerr.Fail(toolerr.CodeAlreadyExists, "a name that differs only in case already exists")
				}
				return Resolved{Abs: next, Rel: strings.Join(segs, "/"), Segs: segs, Exists: false}, nil
			}
			if last {
				return Resolved{}, toolerr.Fail(toolerr.CodeNotFound, "the path does not exist")
			}
			return Resolved{}, toolerr.Fail(toolerr.CodeParentMissing, "a directory of the path does not exist")
		case err != nil:
			return Resolved{}, toolerr.Fail(toolerr.CodeIO, "the path could not be inspected")
		}
		if isLink(info) {
			return Resolved{}, toolerr.Reject(toolerr.CodePathEscape, "the path goes through a link or a reparse point")
		}
		exact, _, lerr := lookupName(cur, seg)
		if lerr != nil {
			return Resolved{}, lerr
		}
		if !exact {
			return Resolved{}, toolerr.Reject(toolerr.CodePathEscape, "the path is spelled differently from the name on disk")
		}
		if !last && !info.IsDir() {
			return Resolved{}, toolerr.Fail(toolerr.CodeNotDirectory, "a component of the path is not a directory")
		}
		cur = next
		r = Resolved{Abs: cur, Rel: strings.Join(segs[:i+1], "/"), Segs: segs[:i+1], Exists: true, Info: info}
	}
	return r, nil
}

// lookupName lists dir to see whether it holds name exactly, and whether it holds a
// name that is equal to it only when case is folded (folded is complete only when
// exact is false: the listing stops at the exact name). A directory too large to list is
// refused rather than trusted.
func lookupName(dir, name string) (exact, folded bool, err error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, false, toolerr.Fail(toolerr.CodeIO, "a directory could not be listed")
	}
	defer f.Close()
	n := 0
	for {
		names, rerr := f.Readdirnames(512)
		for _, e := range names {
			if e == name {
				exact = true
			} else if strings.EqualFold(e, name) {
				folded = true
			}
		}
		n += len(names)
		if exact {
			// Callers look for a folded twin only when the exact name is absent.
			return true, folded, nil
		}
		if n > maxDirEntries {
			return false, false, toolerr.Fail(toolerr.CodeIO, "a directory is too large to verify")
		}
		if errors.Is(rerr, io.EOF) {
			return exact, folded, nil
		}
		if rerr != nil {
			return false, false, toolerr.Fail(toolerr.CodeIO, "a directory could not be listed")
		}
	}
}

// ResolveDir checks that a path is an existing directory the policy lets a process
// work in: covered by a read or a write prefix, and contained as any path is.
func (s Scope) ResolveDir(p string) (Resolved, error) {
	segs, err := Normalize(p)
	if err != nil {
		return Resolved{}, err
	}
	if !s.Allows(segs, Read) && !s.Allows(segs, Write) {
		return Resolved{}, toolerr.Reject(toolerr.CodePathOutside, "the directory is outside what the policy allows")
	}
	r, err := s.walk(segs, false)
	if err != nil {
		return Resolved{}, err
	}
	if !r.Info.IsDir() {
		return Resolved{}, toolerr.Fail(toolerr.CodeNotDirectory, "the path is not a directory")
	}
	return r, nil
}

// CheckSearch is Check for the scope of a search: a path the policy covers, or a
// directory above a read prefix (which the walk then confines to what is covered).
func (s Scope) CheckSearch(p string) ([]string, error) {
	segs, err := Normalize(p)
	if err != nil {
		return nil, err
	}
	if !s.Allows(segs, Read) && !s.allowsDir(segs, Read) {
		return nil, toolerr.Reject(toolerr.CodePathOutside, "the path is outside what the policy allows")
	}
	return segs, nil
}
