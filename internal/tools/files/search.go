package files

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolerr"
)

// Limits of one file.search. The walk is bounded in the number of files, the depth of
// directories, the size of a file it reads and the text it returns, so a search can
// neither recurse without end nor read without bound.
const (
	MaxSearchFiles    = 20000
	MaxSearchDepth    = 64
	MaxSearchFileSize = 8 << 20
	maxMatchText      = 256
	binarySniffBytes  = 8192
)

// SearchArgs are the arguments of file.search.
type SearchArgs struct {
	Path       string
	Query      string
	Mode       string // literal or regex
	MaxResults int
	MaxBytes   int
}

// Match is one line that matches. Line is 1-based; Column is the byte offset of the
// first match in the line; ByteOffset is the offset of that match in the file.
type Match struct {
	Path          string `json:"path"`
	Line          int    `json:"line"`
	Column        int    `json:"column"`
	ByteOffset    int64  `json:"byte_offset"`
	Text          string `json:"text"`
	TextTruncated bool   `json:"text_truncated"`
}

// SearchResult says what was found and how much of the scope was looked at: a search
// that stopped at a limit is partial and names the limit, never presented as complete.
type SearchResult struct {
	Matches        []Match  `json:"matches"`
	Partial        bool     `json:"partial"`
	Limits         []string `json:"limits_reached"`
	FilesScanned   int      `json:"files_scanned"`
	FilesSkipped   int      `json:"files_skipped"`
	ReturnedBytes  int      `json:"returned_bytes"`
	ReturnedCount  int      `json:"returned_count"`
	SkippedReasons []string `json:"skipped_reasons"`
}

type searcher struct {
	ctx      context.Context
	sc       Scope
	args     SearchArgs
	lit      []byte
	re       *regexp.Regexp
	res      SearchResult
	limits   map[string]bool
	skipped  map[string]bool
	files    int
	stop     bool
	maxBytes int
}

func (s *searcher) limit(name string) {
	s.limits[name] = true
	s.stop = true
}

// SearchFiles is file.search over a file or a directory tree under the read prefixes.
// Entries are visited in byte order of their names. Links, devices and other
// irregular entries, files that look binary (a NUL in the first 8 KiB) and files
// larger than 8 MiB are skipped and counted. A line matches once, whatever the number
// of occurrences in it; the regular expression engine is Go's RE2, which runs in time
// linear in the text.
func SearchFiles(ctx context.Context, sc Scope, a SearchArgs) (SearchResult, error) {
	if a.MaxResults < 1 || a.MaxBytes < 1 || a.Query == "" {
		return SearchResult{}, toolerr.Reject(toolerr.CodePathInvalid, "the search needs a query and positive limits")
	}
	s := &searcher{ctx: ctx, sc: sc, args: a, limits: map[string]bool{}, skipped: map[string]bool{}, maxBytes: a.MaxBytes}
	switch a.Mode {
	case "literal":
		s.lit = []byte(a.Query)
	case "regex":
		re, err := regexp.Compile(a.Query)
		if err != nil {
			return SearchResult{}, toolerr.Reject(toolerr.CodeRegexInvalid, "the regular expression is not valid")
		}
		s.re = re
	default:
		return SearchResult{}, toolerr.Reject(toolerr.CodePathInvalid, "the mode is literal or regex")
	}
	// A directory that only lies above a read prefix is searchable: the entries below it
	// are judged one by one.
	segs, err := sc.CheckSearch(a.Path)
	if err != nil {
		return SearchResult{}, err
	}
	r, err := sc.walk(segs, false)
	if err != nil {
		return SearchResult{}, err
	}
	switch {
	case r.Info.IsDir():
		s.dir(r.Abs, r.Segs, 0)
	case r.Info.Mode().IsRegular():
		s.file(r)
	default:
		return SearchResult{}, toolerr.Reject(toolerr.CodePathEscape, "the path is not a file or a directory")
	}
	if err := ctx.Err(); err != nil {
		return SearchResult{}, toolerr.Fail(toolerr.CodeCancelled, "the search was stopped")
	}
	s.res.FilesScanned, s.res.ReturnedCount = s.files, len(s.res.Matches)
	s.res.Limits = sortedKeys(s.limits)
	s.res.SkippedReasons = sortedKeys(s.skipped)
	s.res.Partial = len(s.res.Limits) > 0
	if s.res.Matches == nil {
		s.res.Matches = []Match{}
	}
	return s.res, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (s *searcher) skip(reason string) {
	s.res.FilesSkipped++
	s.skipped[reason] = true
}

func (s *searcher) dir(abs string, segs []string, depth int) {
	if s.stop || s.ctx.Err() != nil {
		return
	}
	if depth > MaxSearchDepth {
		s.limit("max_depth")
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		s.skip("unreadable_directory")
		return
	}
	entries, err := f.ReadDir(-1)
	_ = f.Close()
	if err != nil {
		s.skip("unreadable_directory")
		return
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, e := range entries {
		if s.stop || s.ctx.Err() != nil {
			return
		}
		name := e.Name()
		child := append(slices.Clone(segs), name)
		if unsafeSegment(name) {
			s.skip("unaddressable_name")
			continue
		}
		info, err := e.Info()
		if err != nil {
			s.skip("unreadable_entry")
			continue
		}
		if isLink(info) {
			s.skip("link_or_irregular")
			continue
		}
		switch {
		case info.IsDir():
			if s.sc.allowsDir(child, Read) {
				s.dir(filepath.Join(abs, name), child, depth+1)
			}
		case info.Mode().IsRegular():
			if s.sc.Allows(child, Read) {
				s.file(Resolved{Abs: filepath.Join(abs, name), Rel: strings.Join(child, "/"), Segs: child, Exists: true, Info: info})
			}
		}
	}
}

func (s *searcher) file(r Resolved) {
	if s.stop {
		return
	}
	if s.files >= MaxSearchFiles {
		s.limit("max_files")
		return
	}
	if r.Info.Size() > MaxSearchFileSize {
		s.skip("file_too_large")
		return
	}
	f, _, err := openRegular(r)
	if err != nil {
		s.skip("unreadable_file")
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxSearchFileSize+1))
	_ = f.Close()
	if err != nil || int64(len(data)) > MaxSearchFileSize {
		s.skip("unreadable_file")
		return
	}
	s.files++
	head := data[:min(len(data), binarySniffBytes)]
	if bytes.IndexByte(head, 0) >= 0 {
		s.skip("binary_file")
		return
	}
	offset := int64(0)
	for line := 1; len(data) > 0; line++ {
		if s.stop || s.ctx.Err() != nil {
			return
		}
		end := bytes.IndexByte(data, '\n')
		text := data
		next := len(data)
		if end >= 0 {
			text, next = data[:end], end+1
		}
		text = bytes.TrimSuffix(text, []byte{'\r'})
		if col := s.find(text); col >= 0 {
			if !s.add(r.Rel, line, col, offset+int64(col), text) {
				return
			}
		}
		offset += int64(next)
		data = data[next:]
	}
}

func (s *searcher) find(line []byte) int {
	if s.re != nil {
		if loc := s.re.FindIndex(line); loc != nil {
			return loc[0]
		}
		return -1
	}
	return bytes.Index(line, s.lit)
}

// add records a match, or stops the search at the result limits. It reports whether
// the search goes on.
func (s *searcher) add(path string, line, col int, off int64, text []byte) bool {
	if len(s.res.Matches) >= s.args.MaxResults {
		s.limit("max_results")
		return false
	}
	truncated := false
	if len(text) > maxMatchText {
		cut := maxMatchText
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text, truncated = text[:cut], true
	}
	clean := strings.ToValidUTF8(string(text), "�")
	cost := len(path) + len(clean) + 96
	if s.res.ReturnedBytes+cost > s.maxBytes {
		s.limit("max_bytes")
		return false
	}
	s.res.ReturnedBytes += cost
	s.res.Matches = append(s.res.Matches, Match{Path: path, Line: line, Column: col, ByteOffset: off, Text: clean, TextTruncated: truncated})
	return true
}
