package files

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolerr"
)

// workspace is a real, symlink-free temporary workspace: the root the Tools are given
// is the real path, as the configuration validator guarantees.
func workspace(t *testing.T) Scope {
	t.Helper()
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return Scope{Root: real, ReadPrefixes: []string{"."}, WritePrefixes: []string{"."}}
}

func put(t *testing.T, sc Scope, rel, content string) string {
	t.Helper()
	p := filepath.Join(sc.Root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func wantCode(t *testing.T, err error, code string, class toolerr.Class) {
	t.Helper()
	te, ok := toolerr.As(err)
	if !ok {
		t.Fatalf("got %v, want a tool error %s", err, code)
	}
	if te.Code != code || te.Class != class {
		t.Fatalf("got %s (class %d): %s, want %s (class %d)", te.Code, te.Class, te.Message, code, class)
	}
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links are not available: %v", err)
	}
}

func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	p := filepath.Join(dir, "CaseProbe")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(p)
	_, err := os.Lstat(filepath.Join(dir, "caseprobe"))
	return err == nil
}

func TestNormalizeRefusesWhatIsNotAPortableRelativePath(t *testing.T) {
	bad := []string{
		"", "/abs", "../x", "a/../b", "a//b", "a/", "./a", "a/./b", "a\\b", "C:/x", "c:x", "a\x00b", "a\nb",
		"con", "CON.txt", "dir/NUL", "lpt1.log", "a:stream", "trailing.", "trailing ", "q?", "a*b", `a"b`, "a<b", "a|b",
		strings.Repeat("a", MaxPathBytes+1), "\xff\xfe",
	}
	for _, p := range bad {
		if _, err := Normalize(p); err == nil {
			t.Errorf("%q was accepted", p)
		} else {
			wantCode(t, err, toolerr.CodePathInvalid, toolerr.Rejected)
		}
	}
	for _, p := range []string{".", "a", "a/b.txt", "dir/sub dir/file-1.go", "日本語/ファイル.txt", "console.log", "a.b.c"} {
		if _, err := Normalize(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
}

func TestPrefixesAreComparedBySegmentNotBySubstring(t *testing.T) {
	sc := Scope{Root: "/", ReadPrefixes: []string{"src"}, WritePrefixes: []string{"src/out", "docs/a.md"}}
	for _, c := range []struct {
		path string
		a    Access
		ok   bool
	}{
		{"src", Read, true}, {"src/x/y.go", Read, true}, {"srcx/y.go", Read, false}, {"sr", Read, false}, {".", Read, false},
		{"src/out/z", Write, true}, {"src/outer", Write, false}, {"src/x", Write, false}, {"docs/a.md", Write, true}, {"docs/a.md/x", Write, true}, {"docs", Write, false},
		{"other", Read, false},
	} {
		_, err := sc.Check(c.path, c.a)
		if (err == nil) != c.ok {
			t.Errorf("%s (access %d): err=%v, want ok=%t", c.path, c.a, err, c.ok)
		} else if err != nil {
			wantCode(t, err, toolerr.CodePathOutside, toolerr.Rejected)
		}
	}
	// A prefix list that is empty allows nothing, in either direction.
	none := Scope{Root: "/", ReadPrefixes: nil, WritePrefixes: nil}
	if _, err := none.Check(".", Read); err == nil {
		t.Fatal("an empty prefix list allowed a read")
	}
	if _, err := none.Check("a", Write); err == nil {
		t.Fatal("an empty prefix list allowed a write")
	}
	// The whole workspace.
	all := Scope{Root: "/", ReadPrefixes: []string{"."}}
	if _, err := all.Check("a/b", Read); err != nil {
		t.Fatal(err)
	}
}

func TestResolveRefusesLinksAndCaseAliases(t *testing.T) {
	sc := workspace(t)
	put(t, sc, "real/file.txt", "x")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, filepath.Join(sc.Root, "escape"))
	symlinkOrSkip(t, filepath.Join(sc.Root, "real", "file.txt"), filepath.Join(sc.Root, "alias.txt"))
	symlinkOrSkip(t, filepath.Join(sc.Root, "real"), filepath.Join(sc.Root, "dirlink"))

	for _, p := range []string{"escape/secret.txt", "escape", "alias.txt", "dirlink/file.txt", "dirlink"} {
		if _, err := sc.Resolve(p, Read); err == nil {
			t.Errorf("%s resolved through a link", p)
		} else {
			wantCode(t, err, toolerr.CodePathEscape, toolerr.Rejected)
		}
	}
	// Tools that go through Resolve inherit the refusal.
	if _, err := ReadFile(context.Background(), sc, ReadArgs{Path: "escape/secret.txt", Range: Range{0, 10}, MaxBytes: 10}); err == nil {
		t.Fatal("file.read followed a link out of the workspace")
	}
	if _, err := CreateFile(context.Background(), sc, CreateArgs{Path: "escape/new.txt", Text: "x"}); err == nil {
		t.Fatal("file.create wrote through a link")
	}
	if _, err := os.Lstat(filepath.Join(outside, "new.txt")); err == nil {
		t.Fatal("a file appeared outside the workspace")
	}

	// A path spelled in another case is the same file on a case-insensitive filesystem;
	// it is refused there, and simply absent on a case-sensitive one.
	if caseInsensitive(t, sc.Root) {
		_, err := sc.Resolve("REAL/FILE.TXT", Read)
		wantCode(t, err, toolerr.CodePathEscape, toolerr.Rejected)
		_, err = CreateFile(context.Background(), sc, CreateArgs{Path: "real/File.txt", Text: "y"})
		wantCode(t, err, toolerr.CodePathEscape, toolerr.Rejected)
	} else {
		_, err := sc.Resolve("REAL/FILE.TXT", Read)
		if te, ok := toolerr.As(err); !ok || (te.Code != toolerr.CodeNotFound && te.Code != toolerr.CodeParentMissing) {
			t.Fatalf("%v", err)
		}
	}
	if _, err := sc.Resolve("real/file.txt", Read); err != nil {
		t.Fatalf("the exact spelling is refused: %v", err)
	}
	if _, err := sc.Resolve("real/missing.txt", Read); err == nil {
		t.Fatal("a missing file resolved")
	} else {
		wantCode(t, err, toolerr.CodeNotFound, toolerr.Failed)
	}
	if _, err := sc.Resolve("nodir/x.txt", Read); err == nil {
		t.Fatal("a missing directory resolved")
	} else {
		wantCode(t, err, toolerr.CodeParentMissing, toolerr.Failed)
	}
	if _, err := sc.Resolve("real/file.txt/below", Read); err == nil {
		t.Fatal("a path below a file resolved")
	} else {
		wantCode(t, err, toolerr.CodeNotDirectory, toolerr.Failed)
	}
}

func TestReadFileRangesHashAndEncoding(t *testing.T) {
	sc := workspace(t)
	content := "0123456789abcdefghij"
	put(t, sc, "d/a.txt", content)
	ctx := context.Background()

	res, err := ReadFile(ctx, sc, ReadArgs{Path: "d/a.txt", Range: Range{0, 1000}, MaxBytes: 65536})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != content || res.TotalBytes != 20 || res.RawHash != sha(content) || res.Partial || res.ReturnedRange != (Range{0, 20}) || res.Encoding != "utf-8" || res.Path != "d/a.txt" {
		t.Fatalf("%+v", res)
	}
	res, err = ReadFile(ctx, sc, ReadArgs{Path: "d/a.txt", Range: Range{5, 15}, MaxBytes: 65536})
	if err != nil || res.Content != "56789abcde" || !res.Partial || res.ReturnedRange != (Range{5, 15}) || res.RawHash != sha(content) {
		t.Fatalf("%+v %v", res, err)
	}
	// max_bytes cuts the range, and the returned range says so.
	res, err = ReadFile(ctx, sc, ReadArgs{Path: "d/a.txt", Range: Range{2, 20}, MaxBytes: 4})
	if err != nil || res.Content != "2345" || !res.Partial || res.ReturnedRange != (Range{2, 6}) {
		t.Fatalf("%+v %v", res, err)
	}
	// The end of the file: an empty range that is still the truth about the file.
	res, err = ReadFile(ctx, sc, ReadArgs{Path: "d/a.txt", Range: Range{20, 30}, MaxBytes: 4})
	if err != nil || res.Content != "" || res.ReturnedRange != (Range{20, 20}) || !res.Partial {
		t.Fatalf("%+v %v", res, err)
	}
	// Past the end, backwards, and a directory.
	_, err = ReadFile(ctx, sc, ReadArgs{Path: "d/a.txt", Range: Range{21, 30}, MaxBytes: 4})
	wantCode(t, err, toolerr.CodeInvalidRange, toolerr.Failed)
	_, err = ReadFile(ctx, sc, ReadArgs{Path: "d/a.txt", Range: Range{9, 3}, MaxBytes: 4})
	wantCode(t, err, toolerr.CodeInvalidRange, toolerr.Rejected)
	_, err = ReadFile(ctx, sc, ReadArgs{Path: "d", Range: Range{0, 3}, MaxBytes: 4})
	wantCode(t, err, toolerr.CodeNotFile, toolerr.Failed)

	// Bytes that are not UTF-8 are returned as they are, in base64.
	bin := string([]byte{0xff, 0xfe, 0x00, 'a'})
	put(t, sc, "bin.dat", bin)
	res, err = ReadFile(ctx, sc, ReadArgs{Path: "bin.dat", Range: Range{0, 100}, MaxBytes: 100})
	if err != nil || res.Encoding != "base64" || res.Content != base64.StdEncoding.EncodeToString([]byte(bin)) || res.RawHash != sha(bin) {
		t.Fatalf("%+v %v", res, err)
	}
	// A range that cuts a character in two is returned as the bytes it is.
	put(t, sc, "jp.txt", "あいう")
	res, err = ReadFile(ctx, sc, ReadArgs{Path: "jp.txt", Range: Range{0, 4}, MaxBytes: 100})
	if err != nil || res.Encoding != "base64" || res.ReturnedRange != (Range{0, 4}) {
		t.Fatalf("%+v %v", res, err)
	}
	// A cancelled read is not a result.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = ReadFile(cctx, sc, ReadArgs{Path: "d/a.txt", Range: Range{0, 3}, MaxBytes: 4})
	wantCode(t, err, toolerr.CodeCancelled, toolerr.Failed)
}

func TestReadFileHonoursTheReadPrefixes(t *testing.T) {
	sc := workspace(t)
	sc.ReadPrefixes = []string{"pub"}
	put(t, sc, "pub/a.txt", "ok")
	put(t, sc, "priv/b.txt", "no")
	if _, err := ReadFile(context.Background(), sc, ReadArgs{Path: "pub/a.txt", Range: Range{0, 9}, MaxBytes: 9}); err != nil {
		t.Fatal(err)
	}
	_, err := ReadFile(context.Background(), sc, ReadArgs{Path: "priv/b.txt", Range: Range{0, 9}, MaxBytes: 9})
	wantCode(t, err, toolerr.CodePathOutside, toolerr.Rejected)
	// Reading needs a read prefix: a write prefix is not enough.
	sc2 := workspace(t)
	sc2.ReadPrefixes = nil
	put(t, sc2, "a.txt", "x")
	_, err = ReadFile(context.Background(), sc2, ReadArgs{Path: "a.txt", Range: Range{0, 9}, MaxBytes: 9})
	wantCode(t, err, toolerr.CodePathOutside, toolerr.Rejected)
}

func TestCreateFileIsExclusiveAtomicAndReportsWhatItWrote(t *testing.T) {
	sc := workspace(t)
	ctx := context.Background()
	if err := os.Mkdir(filepath.Join(sc.Root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := CreateFile(ctx, sc, CreateArgs{Path: "d/new.txt", Text: "hello\n日本語"})
	if err != nil {
		t.Fatal(err)
	}
	want := "hello\n日本語"
	if res.BytesWritten != int64(len(want)) || res.RawHash != sha(want) || res.Path != "d/new.txt" {
		t.Fatalf("%+v", res)
	}
	got, _ := os.ReadFile(filepath.Join(sc.Root, "d", "new.txt"))
	if string(got) != want {
		t.Fatalf("%q", got)
	}
	// No staging file is left behind.
	entries, _ := os.ReadDir(filepath.Join(sc.Root, "d"))
	if len(entries) != 1 {
		t.Fatalf("%d entries in the directory", len(entries))
	}
	// An existing file is a conflict and is not touched.
	_, err = CreateFile(ctx, sc, CreateArgs{Path: "d/new.txt", Text: "other"})
	wantCode(t, err, toolerr.CodeAlreadyExists, toolerr.Failed)
	if got, _ := os.ReadFile(filepath.Join(sc.Root, "d", "new.txt")); string(got) != want {
		t.Fatal("an existing file was overwritten")
	}
	// An existing directory is a conflict too; so is a missing parent, and the root.
	_, err = CreateFile(ctx, sc, CreateArgs{Path: "d", Text: "x"})
	wantCode(t, err, toolerr.CodeAlreadyExists, toolerr.Failed)
	_, err = CreateFile(ctx, sc, CreateArgs{Path: "nodir/x.txt", Text: "x"})
	wantCode(t, err, toolerr.CodeParentMissing, toolerr.Failed)
	// Writing needs a write prefix.
	sc.WritePrefixes = []string{"d"}
	_, err = CreateFile(ctx, sc, CreateArgs{Path: "elsewhere.txt", Text: "x"})
	wantCode(t, err, toolerr.CodePathOutside, toolerr.Rejected)
	// An empty text is a valid file.
	if res, err := CreateFile(ctx, sc, CreateArgs{Path: "d/empty.txt", Text: ""}); err != nil || res.BytesWritten != 0 || res.RawHash != sha("") {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestEditFileNeedsTheHashAndOneExactMatch(t *testing.T) {
	sc := workspace(t)
	ctx := context.Background()
	orig := "alpha beta\ngamma beta\naaa\n"
	p := put(t, sc, "e.txt", orig)
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}

	// The hash must be the file's.
	_, err := EditFile(ctx, sc, EditArgs{Path: "e.txt", ExpectedHash: sha("something else"), OldText: "alpha", NewText: "A"})
	wantCode(t, err, toolerr.CodeHashMismatch, toolerr.Failed)
	_, err = EditFile(ctx, sc, EditArgs{Path: "e.txt", ExpectedHash: "ABC", OldText: "alpha", NewText: "A"})
	wantCode(t, err, toolerr.CodeHashMismatch, toolerr.Rejected)
	// old_text must exist, and exactly once: "beta" is twice, "aa" overlaps itself in "aaa".
	_, err = EditFile(ctx, sc, EditArgs{Path: "e.txt", ExpectedHash: sha(orig), OldText: "delta", NewText: "D"})
	wantCode(t, err, toolerr.CodeOldTextMissing, toolerr.Failed)
	_, err = EditFile(ctx, sc, EditArgs{Path: "e.txt", ExpectedHash: sha(orig), OldText: "beta", NewText: "B"})
	wantCode(t, err, toolerr.CodeOldTextAmbig, toolerr.Failed)
	_, err = EditFile(ctx, sc, EditArgs{Path: "e.txt", ExpectedHash: sha(orig), OldText: "aa", NewText: "B"})
	wantCode(t, err, toolerr.CodeOldTextAmbig, toolerr.Failed)
	// No fuzzy matching: other whitespace, other case and a line number are not a match.
	_, err = EditFile(ctx, sc, EditArgs{Path: "e.txt", ExpectedHash: sha(orig), OldText: "Alpha", NewText: "B"})
	wantCode(t, err, toolerr.CodeOldTextMissing, toolerr.Failed)
	_, err = EditFile(ctx, sc, EditArgs{Path: "e.txt", ExpectedHash: sha(orig), OldText: "alpha  beta", NewText: "B"})
	wantCode(t, err, toolerr.CodeOldTextMissing, toolerr.Failed)
	_, err = EditFile(ctx, sc, EditArgs{Path: "e.txt", ExpectedHash: sha(orig), OldText: "", NewText: "B"})
	wantCode(t, err, toolerr.CodeOldTextMissing, toolerr.Rejected)
	if got, _ := os.ReadFile(p); string(got) != orig {
		t.Fatal("a refused edit changed the file")
	}

	res, err := EditFile(ctx, sc, EditArgs{Path: "e.txt", ExpectedHash: sha(orig), OldText: "gamma beta", NewText: "G"})
	if err != nil {
		t.Fatal(err)
	}
	want := "alpha beta\nG\naaa\n"
	if got, _ := os.ReadFile(p); string(got) != want {
		t.Fatalf("%q", got)
	}
	if res.OldHash != sha(orig) || res.NewHash != sha(want) || res.NewTotalBytes != int64(len(want)) || res.ReplacedAt != 11 {
		t.Fatalf("%+v", res)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(p); info.Mode().Perm() != 0o640 {
			t.Fatalf("the file's mode changed: %v", info.Mode())
		}
	}
	entries, _ := os.ReadDir(sc.Root)
	if len(entries) != 1 {
		t.Fatalf("a staging file was left behind: %d entries", len(entries))
	}
	// The edited file has a new hash: the old one no longer applies.
	_, err = EditFile(ctx, sc, EditArgs{Path: "e.txt", ExpectedHash: sha(orig), OldText: "G", NewText: "H"})
	wantCode(t, err, toolerr.CodeHashMismatch, toolerr.Failed)
	// Deleting text is an edit with empty new_text.
	if _, err := EditFile(ctx, sc, EditArgs{Path: "e.txt", ExpectedHash: sha(want), OldText: "G\n", NewText: ""}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "alpha beta\naaa\n" {
		t.Fatalf("%q", got)
	}
	// A path that is not writable is refused before the file is read.
	sc.WritePrefixes = []string{"other"}
	_, err = EditFile(ctx, sc, EditArgs{Path: "e.txt", ExpectedHash: sha(want), OldText: "x", NewText: "y"})
	wantCode(t, err, toolerr.CodePathOutside, toolerr.Rejected)
}

func TestEditFileDoesNotOverwriteAChangeMadeWhileItWasPrepared(t *testing.T) {
	sc := workspace(t)
	orig := "one two three"
	p := put(t, sc, "c.txt", orig)
	beforeReplace = func() {
		if err := os.WriteFile(p, []byte("one two three, and a concurrent writer"), 0o644); err != nil {
			t.Error(err)
		}
	}
	defer func() { beforeReplace = nil }()
	_, err := EditFile(context.Background(), sc, EditArgs{Path: "c.txt", ExpectedHash: sha(orig), OldText: "two", NewText: "2"})
	wantCode(t, err, toolerr.CodeHashMismatch, toolerr.Failed)
	if got, _ := os.ReadFile(p); string(got) != "one two three, and a concurrent writer" {
		t.Fatalf("the concurrent change was overwritten: %q", got)
	}
	entries, _ := os.ReadDir(sc.Root)
	if len(entries) != 1 {
		t.Fatalf("a staging file was left behind: %d entries", len(entries))
	}
}

func TestEditFileRefusesALinkToAFileOutsideTheWorkspace(t *testing.T) {
	sc := workspace(t)
	outside := filepath.Join(t.TempDir(), "o.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, filepath.Join(sc.Root, "l.txt"))
	_, err := EditFile(context.Background(), sc, EditArgs{Path: "l.txt", ExpectedHash: sha("outside"), OldText: "outside", NewText: "changed"})
	wantCode(t, err, toolerr.CodePathEscape, toolerr.Rejected)
	if got, _ := os.ReadFile(outside); string(got) != "outside" {
		t.Fatal("a file outside the workspace was edited")
	}
}

func TestSearchFilesLiteralRegexAndTheLimitsOfTheWalk(t *testing.T) {
	sc := workspace(t)
	ctx := context.Background()
	put(t, sc, "a/one.txt", "first line\nneedle here\nlast\n")
	put(t, sc, "a/two.txt", "nothing\r\nanother needle\r\n")
	put(t, sc, "b/three.txt", "Needle upper\nneedle lower\n")
	put(t, sc, "bin.dat", "needle\x00binary")
	put(t, sc, "big.txt", strings.Repeat("needle\n", 5)+strings.Repeat("x", MaxSearchFileSize))

	res, err := SearchFiles(ctx, sc, SearchArgs{Path: ".", Query: "needle", Mode: "literal", MaxResults: 100, MaxBytes: 65536})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range res.Matches {
		got = append(got, m.Path+":"+itoa(m.Line)+":"+itoa(m.Column))
	}
	want := []string{"a/one.txt:2:0", "a/two.txt:2:8", "b/three.txt:2:0"}
	if strings.Join(got, ",") != strings.Join(want, ",") || res.Partial {
		t.Fatalf("%v %+v", got, res)
	}
	if res.Matches[1].Text != "another needle" || res.Matches[0].ByteOffset != 11 {
		t.Fatalf("%+v", res.Matches)
	}
	if res.FilesSkipped != 2 || strings.Join(res.SkippedReasons, ",") != "binary_file,file_too_large" {
		t.Fatalf("skips: %+v", res)
	}

	re, err := SearchFiles(ctx, sc, SearchArgs{Path: "b", Query: "(?i)^needle\\b", Mode: "regex", MaxResults: 100, MaxBytes: 65536})
	if err != nil || len(re.Matches) != 2 || re.Matches[0].Line != 1 {
		t.Fatalf("%+v %v", re, err)
	}
	_, err = SearchFiles(ctx, sc, SearchArgs{Path: ".", Query: "(", Mode: "regex", MaxResults: 1, MaxBytes: 1000})
	wantCode(t, err, toolerr.CodeRegexInvalid, toolerr.Rejected)
	// A pattern that would blow up a backtracking engine runs in linear time here.
	put(t, sc, "evil.txt", strings.Repeat("a", 5000)+"b\n")
	if _, err := SearchFiles(ctx, sc, SearchArgs{Path: "evil.txt", Query: "(a+)+$", Mode: "regex", MaxResults: 1, MaxBytes: 1000}); err != nil {
		t.Fatal(err)
	}

	// Limits: the count and the bytes, each named, never silently.
	lim, err := SearchFiles(ctx, sc, SearchArgs{Path: "a", Query: "e", Mode: "literal", MaxResults: 2, MaxBytes: 65536})
	if err != nil || !lim.Partial || len(lim.Matches) != 2 || strings.Join(lim.Limits, ",") != "max_results" {
		t.Fatalf("%+v %v", lim, err)
	}
	lim, err = SearchFiles(ctx, sc, SearchArgs{Path: "a", Query: "e", Mode: "literal", MaxResults: 100, MaxBytes: 150})
	if err != nil || !lim.Partial || len(lim.Matches) != 1 || strings.Join(lim.Limits, ",") != "max_bytes" {
		t.Fatalf("%+v %v", lim, err)
	}
	// A search of one file, and of a directory under a prefix that is below the root.
	one, err := SearchFiles(ctx, sc, SearchArgs{Path: "a/one.txt", Query: "line", Mode: "literal", MaxResults: 9, MaxBytes: 9999})
	if err != nil || len(one.Matches) != 1 || one.Matches[0].Line != 1 {
		t.Fatalf("%+v %v", one, err)
	}
	sc.ReadPrefixes = []string{"a"}
	under, err := SearchFiles(ctx, sc, SearchArgs{Path: ".", Query: "needle", Mode: "literal", MaxResults: 100, MaxBytes: 65536})
	if err != nil || len(under.Matches) != 2 {
		t.Fatalf("a search from the root must only see the read prefix: %+v %v", under, err)
	}
	_, err = SearchFiles(ctx, sc, SearchArgs{Path: "b", Query: "needle", Mode: "literal", MaxResults: 100, MaxBytes: 65536})
	wantCode(t, err, toolerr.CodePathOutside, toolerr.Rejected)
}

func TestSearchFilesSkipsLinksAndLimitsDepth(t *testing.T) {
	sc := workspace(t)
	ctx := context.Background()
	put(t, sc, "real/x.txt", "needle")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "o.txt"), []byte("needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, filepath.Join(sc.Root, "link"))
	symlinkOrSkip(t, filepath.Join(sc.Root, "real", "x.txt"), filepath.Join(sc.Root, "flink.txt"))
	res, err := SearchFiles(ctx, sc, SearchArgs{Path: ".", Query: "needle", Mode: "literal", MaxResults: 10, MaxBytes: 9999})
	if err != nil || len(res.Matches) != 1 || res.Matches[0].Path != "real/x.txt" || res.FilesSkipped != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	// A directory chain deeper than the limit stops the walk and says why.
	deep := sc.Root
	for i := 0; i < MaxSearchDepth+3; i++ {
		deep = filepath.Join(deep, "d")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Skipf("a very deep path cannot be made here: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deep, "bottom.txt"), []byte("needle"), 0o644); err != nil {
		t.Skip(err)
	}
	res, err = SearchFiles(ctx, sc, SearchArgs{Path: ".", Query: "needle", Mode: "literal", MaxResults: 10, MaxBytes: 9999})
	if err != nil || !res.Partial || strings.Join(res.Limits, ",") != "max_depth" {
		t.Fatalf("%+v %v", res, err)
	}
	for _, m := range res.Matches {
		if strings.HasSuffix(m.Path, "bottom.txt") {
			t.Fatal("the walk went past the depth limit")
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// TestADirectoryReplacedByALinkWhileAFileIsBeingOpenedIsCaught: the path is walked, the
// file opened, and the path walked again; a directory that became a link in between
// makes the second walk refuse, and nothing is read through it.
func TestADirectoryReplacedByALinkWhileAFileIsBeingOpenedIsCaught(t *testing.T) {
	sc := workspace(t)
	put(t, sc, "d/f.txt", "inside")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "f.txt"), []byte("outside secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	afterOpen = func() {
		if err := os.Rename(filepath.Join(sc.Root, "d"), filepath.Join(sc.Root, "d-real")); err != nil {
			t.Error(err)
			return
		}
		if err := os.Symlink(outside, filepath.Join(sc.Root, "d")); err != nil {
			t.Skipf("symbolic links are not available: %v", err)
		}
	}
	defer func() { afterOpen = nil }()
	res, err := ReadFile(context.Background(), sc, ReadArgs{Path: "d/f.txt", Range: Range{0, 100}, MaxBytes: 100})
	wantCode(t, err, toolerr.CodePathEscape, toolerr.Rejected)
	if strings.Contains(res.Content, "secret") {
		t.Fatal("a file outside the workspace was read")
	}
}
