package rencrowharness

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The guard before a repository is made public: nothing of whoever built it, or of the
// network it was built on, may be in a file of it. A real path (it holds a user's name), an
// address of a private network (it holds the shape of a LAN) and a secret each make the test
// fail, and the failure names the file, the line and the kind, never the text it found (a
// log of a failed run is read by more people than the file was).
//
// What is looked for is the form, not a list of names: the test holds no name of any person,
// host or company, because a list of what must not be in the repository would itself be it.
// The way to write a path that is an example is a placeholder that is not a name (<user>,
// $HOME, %USERPROFILE%), and the way to write an address that is an example is one of the
// documentation ranges. What cannot be written that way (a test that must refuse an address of
// a private network has to be given one) is in the allowance below, each with the line it is
// on and the reason, and the test fails when an entry no longer matches anything, so that the
// list cannot outlive what it excuses.
//
// The whole tree is read except .git: a file that git would not commit is still a file a
// build, an archive or a copy of the directory would carry.

// Kinds of what is found.
const (
	kindRealPath  = "a real path (it names a user)"
	kindPrivateIP = "an address of a private network"
	kindSecret    = "something that looks like a secret"
	kindTooLarge  = "a file too large to be read (not scanned)"
)

// maxScanBytes is the largest file the guard reads. A larger text file is not skipped: it is a
// failure, so that a file can never become unscanned by growing.
const maxScanBytes = 16 << 20

// sniffBytes is how much of a file is looked at to tell binary from text.
const sniffBytes = 8000

// An allowance excuses one value on one line of one file.
type allowance struct {
	// File is the path from the repository root, with slashes.
	File string
	// Value is exactly the text that is found; Line is a fragment of the line it is on, which
	// pins the entry to that line (a line number would move with every edit).
	Value, Line string
	// Reason says why the value is not what the guard is for.
	Reason string
}

// ip writes an address without writing it, so that this file does not hold the text it excuses.
func ip(a, b, c, d int) string { return fmt.Sprintf("%d.%d.%d.%d", a, b, c, d) }

// allowances are the values that are in the repository on purpose. Each is a made-up address
// or credential that a test gives to code that must refuse it; none is an address of any
// network that exists or a credential of any account.
var allowances = []allowance{
	{"internal/config/config_test.go", ip(10, 0, 0, 5), `"http://` + ip(10, 0, 0, 5) + `:8090/v1": false`,
		"gateway.base_url validation: a base URL on a private network is not the loopback and must be refused; the address is made up"},
	{"internal/config/config_test.go", ip(192, 168, 1, 2), `"http://` + ip(192, 168, 1, 2) + `/v1": false`,
		"gateway.base_url validation: a private-range address that must be refused; the address is made up"},
	{"internal/modelclient/client_test.go", ip(10, 0, 0, 5), `"http://` + ip(10, 0, 0, 5) + `:8080/v1"`,
		"the client accepts only a loopback base URL: a private-network address must be rejected; the address is made up"},
	{"internal/modelclient/client_test.go", ip(192, 168, 1, 2), `"http://` + ip(192, 168, 1, 2) + `/v1"`,
		"the client accepts only a loopback base URL: a private-range address must be rejected; the address is made up"},
	{"internal/modelclient/client_test.go", ip(10, 1, 2, 3), `"` + ip(10, 1, 2, 3) + `:80": false`,
		"the loopback-only dialer must refuse a private-network address; the address is made up"},
	{"internal/config/config_test.go", "http://user:" + "secret@", `"http://user:` + `secret@` + `127.0.0.1:8090/v1": false`,
		"gateway.base_url validation: a base URL that carries a user and a password must be refused; both are made up"},
	{"internal/cli/cli_test.go", "http://user:" + "secret-marker@", `"http://user:` + `secret-marker@` + `127.0.0.1:8090/v1"`,
		"the CLI refuses a base URL that carries a credential, and the test checks that the marker is not echoed; both are made up"},
}

// A finding is where something was found and what kind of thing it is. It never holds the text.
type finding struct {
	File  string
	Line  int
	Kind  string
	Value string // kept for matching the allowance only; never printed
}

func (f finding) String() string { return fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.Kind) }

var (
	// A path of a user's home: /Users/<name>/ (macOS, and WSL's /mnt/c/Users/<name>/), /home/<name>/
	// and C:\Users\<name>\ (also with the backslashes doubled as in JSON, and with slashes). A name
	// that is a placeholder (starting with <, $, %, {, [ or ~) is not a name and does not match.
	namePart = `[^/\\\s"'<>:*?|$%{}\[\]~` + "`" + `\x00-\x1f]+`
	// A name followed by a separator is a name whatever it is made of (a user's name may be in any
	// script). A name that ends the path (at the end of the line, at a quotation's end or a
	// sentence's) is looked for only in the plain form of a login name, because in prose a
	// slash-separated word is not one.
	loginName      = `[A-Za-z0-9._-]+`
	nameEnd        = `(?:$|[\s"'` + "`" + `),;:])`
	reUnixHome     = regexp.MustCompile(`/(?:Users|home)/(?:(` + namePart + `)/|(` + loginName + `)` + nameEnd + `)`)
	reWindowsHome  = regexp.MustCompile(`(?i)[A-Z]:[\\/]+Users[\\/]+(?:(` + namePart + `)[\\/]|(` + loginName + `)` + nameEnd + `)`)
	reDottedQuad   = regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3}`)
	reIPv6Token    = regexp.MustCompile(`[0-9A-Fa-f:]{3,}(?:%[A-Za-z0-9._-]+)?`)
	secretPatterns = []*regexp.Regexp{
		regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`),
		regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{30,}`),
		regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
		// An API key of the sk- family: a long unbroken run of letters and digits (the word-like
		// sentinels that tests use for a secret that must not be echoed are not that shape).
		regexp.MustCompile(`\bsk-[A-Za-z0-9]{32,}`),
		regexp.MustCompile(`\bsk-(?:proj|ant|svcacct|admin)-[A-Za-z0-9_-]{32,}`),
		regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`),
		regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}`),
		regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`),
		regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}`),
		regexp.MustCompile(`\bya29\.[A-Za-z0-9_-]{20,}`),
		// An SSH public or private key line (its comment is usually user@host), and a URL that
		// carries a user and a password.
		regexp.MustCompile(`\b(?:ssh-(?:rsa|ed25519|dss)|ecdsa-sha2-nistp\d+) AAAA[0-9A-Za-z+/=]{20,}`),
		regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+.-]*://[^/\s:@"'<>]+:[^/\s@"'<>]{3,}@`),
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
		regexp.MustCompile(`(?i)authorization:\s*bearer\s+[A-Za-z0-9._~+/=-]{20,}`),
	}
)

// lanRanges are the ranges of an address that is only meaningful inside a network of its own:
// RFC 1918, and the shared (carrier-grade NAT, which overlay networks use) and link-local
// ranges, which are as much a trace of a network as the private ones. The IPv6 forms (the
// unique local and the link-local ones) are found by their own methods.
var lanRanges = []netip.Prefix{
	netip.MustParsePrefix(ip(10, 0, 0, 0) + "/8"),
	netip.MustParsePrefix(ip(172, 16, 0, 0) + "/12"),
	netip.MustParsePrefix(ip(192, 168, 0, 0) + "/16"),
	netip.MustParsePrefix(ip(100, 64, 0, 0) + "/10"),
	netip.MustParsePrefix(ip(169, 254, 0, 0) + "/16"),
}

func isLAN(a netip.Addr) bool {
	a = a.Unmap()
	if a.Is6() {
		return a.IsPrivate() || a.IsLinkLocalUnicast()
	}
	for _, p := range lanRanges {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// privateAddresses are the addresses of a line that are of a network of its own (see
// lanRanges). A dotted quad that is part of a longer dotted number (a version, an OID) is not
// an address, and an IPv6 token is one only if it parses as one.
func privateAddresses(line string) []string {
	var out []string
	for _, loc := range reDottedQuad.FindAllStringIndex(line, -1) {
		start, end := loc[0], loc[1]
		if start > 0 && (isDigit(line[start-1]) || line[start-1] == '.' && start > 1 && isDigit(line[start-2])) {
			continue
		}
		if end < len(line) && (isDigit(line[end]) || line[end] == '.' && end+1 < len(line) && isDigit(line[end+1])) {
			continue
		}
		addr, err := netip.ParseAddr(line[start:end]) // refuses an octet above 255 and a leading zero
		if err == nil && isLAN(addr) {
			out = append(out, line[start:end])
		}
	}
	for _, tok := range reIPv6Token.FindAllString(line, -1) {
		if strings.Count(tok, ":") < 2 {
			continue
		}
		if addr, err := netip.ParseAddr(tok); err == nil && addr.Is6() && isLAN(addr) {
			out = append(out, tok)
		}
	}
	return out
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// scanLine is everything the guard finds on one line.
func scanLine(file string, n int, line string) []finding {
	var out []finding
	add := func(kind, value string) { out = append(out, finding{File: file, Line: n, Kind: kind, Value: value}) }
	// One finding per line for a path, whichever forms it is in (a drive letter and a slash
	// path are the same path).
	for _, re := range []*regexp.Regexp{reWindowsHome, reUnixHome} {
		reported := false
		for _, m := range re.FindAllStringSubmatch(line, -1) {
			if strings.Trim(m[1]+m[2], ".") == "" {
				continue // "..." after a slash is an ellipsis, not a name
			}
			add(kindRealPath, m[0])
			reported = true
			break
		}
		if reported {
			break
		}
	}
	for _, a := range privateAddresses(line) {
		add(kindPrivateIP, a)
	}
	for _, re := range secretPatterns {
		for _, m := range re.FindAllString(line, -1) {
			add(kindSecret, m)
		}
	}
	return out
}

// scanTree reads every file under root except what is in a .git, and returns what it finds
// with the allowance applied, and the entries of the allowance that excused nothing.
func scanTree(root string, allow []allowance) (found []finding, unused []allowance, err error) {
	used := make([]bool, len(allow))
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil // a worktree's .git file holds the path of the repository it belongs to
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 {
			// Git commits the text of a link, which can name a path of its own as well as a file
			// can hold one. The link is not followed: what it points at is not in the tree.
			target, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			found = append(found, scanLine(rel, 0, target)...)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		data, rerr := readScannable(path, info.Size())
		switch {
		case errors.Is(rerr, errBinary):
			return nil // a binary file has no lines to read
		case errors.Is(rerr, errTooLarge):
			found = append(found, finding{File: rel, Line: 0, Kind: kindTooLarge})
			return nil
		case rerr != nil:
			return rerr
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, f := range scanLine(rel, i+1, line) {
				if k := excusedBy(allow, f, line); k >= 0 {
					used[k] = true
					continue
				}
				found = append(found, f)
			}
		}
		return nil
	})
	for i, a := range allow {
		if !used[i] {
			unused = append(unused, a)
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].File != found[j].File {
			return found[i].File < found[j].File
		}
		return found[i].Line < found[j].Line
	})
	return found, unused, err
}

var (
	errBinary   = errors.New("binary")
	errTooLarge = errors.New("too large")
)

// readScannable reads a file's text. A binary file (a NUL in its first bytes) is errBinary
// whatever its size, so a large build output in the tree is not a failure; a text file larger
// than maxScanBytes is errTooLarge, so that a file never becomes unscanned by growing.
func readScannable(path string, size int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, min(size, sniffBytes))
	if _, err := io.ReadFull(f, head); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return nil, errBinary
	}
	if size > maxScanBytes {
		return nil, errTooLarge
	}
	rest, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return append(head, rest...), nil
}

// excusedBy is the index of the allowance that excuses the finding on this line, or -1.
func excusedBy(allow []allowance, f finding, line string) int {
	for i, a := range allow {
		if a.File == f.File && a.Value == f.Value && strings.Contains(line, a.Line) {
			return i
		}
	}
	return -1
}

// TestTheRepositoryHoldsNoRealPathPrivateAddressOrSecret is the guard itself, run over the
// repository.
func TestTheRepositoryHoldsNoRealPathPrivateAddressOrSecret(t *testing.T) {
	found, unused, err := scanTree(".", allowances)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		t.Errorf("%s", f)
	}
	for _, a := range unused {
		t.Errorf("the allowance for %s (%s) excuses nothing: remove it", a.File, a.Reason)
	}
	if len(found) > 0 {
		t.Log("Write an example path as a placeholder (<user>, $HOME) and an example address from a documentation range; " +
			"an address that a test must be given to refuse goes in the allowance of repo_hygiene_test.go with its reason.")
	}
}

// TestTheAllowanceIsMinimalAndEveryEntryIsExplained: an entry names a file that exists, the
// line it is on, the value, and a reason that says something; no entry is there twice.
func TestTheAllowanceIsMinimalAndEveryEntryIsExplained(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range allowances {
		key := a.File + "\x00" + a.Value + "\x00" + a.Line
		switch {
		case a.File == "" || a.Value == "" || a.Line == "" || len(a.Reason) < 40:
			t.Errorf("an entry of %q has no value, line or reason that explains it", a.File)
		case seen[key]:
			t.Errorf("an entry of %q is there twice", a.File)
		case !strings.Contains(a.Line, a.Value):
			t.Errorf("the line fragment of an entry of %q does not hold its value", a.File)
		}
		seen[key] = true
		if _, err := os.Stat(filepath.FromSlash(a.File)); err != nil {
			t.Errorf("an entry names %q, which cannot be read: %v", a.File, err)
		}
		if len(scanLine(a.File, 1, a.Value)) == 0 {
			t.Errorf("an entry of %q excuses a value that the guard would not find", a.File)
		}
	}
}

// plant writes files of a tree the guard is shown.
func plant(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for rel, data := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTheGuardFailsOnWhatItIsMeantToFind is the control of the guard: a tree with a real path
// of each form, an address of each private range, and a secret of each shape fails, with the
// kind and the line and none of the text; a tree of placeholders and of addresses that are not
// private passes. (The planted text is built from parts, so that this file does not hold it.)
func TestTheGuardFailsOnWhatItIsMeantToFind(t *testing.T) {
	user := "al" + "ice"
	mustFind := map[string]struct {
		text string
		kind string
	}{
		"mac.md":      {"cd /Us" + "ers/" + user + "/work", kindRealPath},
		"linux.txt":   {"cd /ho" + "me/" + user + "/work", kindRealPath},
		"wsl.txt":     {"cd /mnt/c/Us" + "ers/" + user + "/work", kindRealPath},
		"windows.txt": {`C:\Us` + `ers\` + user + `\work`, kindRealPath},
		"unixend.txt": {"home is /Us" + "ers/" + user, kindRealPath},
		"quoted.txt":  {`path = "/ho` + `me/` + user + `"`, kindRealPath},
		"winend.txt":  {`set HOME=C:\Us` + `ers\` + user, kindRealPath},
		"json.json":   {`{"p":"C:\\Us` + `ers\\` + user + `\\work"}`, kindRealPath},
		"slash.txt":   {"d:/Us" + "ers/" + user + "/work", kindRealPath},
		"ip10.txt":    {"gateway " + ip(10, 20, 30, 40), kindPrivateIP},
		"ip172.txt":   {"gateway " + ip(172, 16, 0, 1) + " and " + ip(172, 31, 255, 254), kindPrivateIP},
		"ip192.txt":   {"gateway http://" + ip(192, 168, 0, 10) + ":8080/v1", kindPrivateIP},
		"cgnat.txt":   {"overlay " + ip(100, 64, 0, 7) + " and " + ip(100, 127, 255, 1), kindPrivateIP},
		"linkloc.txt": {"link " + ip(169, 254, 10, 20), kindPrivateIP},
		"ula6.txt":    {"host fd" + "12:3456:789a::1 on the LAN", kindPrivateIP},
		"link6.txt":   {"if fe" + "80::1ff:fe23:4567:890a", kindPrivateIP},
		"key.pem.txt": {"-----BEGIN " + "RSA PRIVATE KEY-----\nMIIB\n-----END " + "RSA PRIVATE KEY-----", kindSecret},
		"key2.txt":    {"-----BEGIN " + "PRIVATE KEY-----", kindSecret},
		"aws.txt":     {"key AKI" + "A" + strings.Repeat("A", 16), kindSecret},
		"gh.txt":      {"token gh" + "p_" + strings.Repeat("a", 36), kindSecret},
		"slack.txt":   {"xo" + "xb-" + "1234567890-abcdefghij", kindSecret},
		"sk.txt":      {"key s" + "k-" + strings.Repeat("a", 40), kindSecret},
		"skproj.txt":  {"key s" + "k-proj-" + strings.Repeat("a_-", 14), kindSecret},
		"jwt.txt":     {"ey" + "J" + strings.Repeat("a", 12) + ".ey" + "J" + strings.Repeat("b", 12) + "." + strings.Repeat("c", 12), kindSecret},
		"bearer.txt":  {"Authorization: Bear" + "er " + strings.Repeat("a", 30), kindSecret},
		"hf.txt":      {"token h" + "f_" + strings.Repeat("a", 34), kindSecret},
		"gitlab.txt":  {"token glp" + "at-" + strings.Repeat("a", 24), kindSecret},
		"npm.txt":     {"token np" + "m_" + strings.Repeat("a", 36), kindSecret},
		"google.txt":  {"token ya" + "29." + strings.Repeat("a", 30), kindSecret},
		"ssh.txt":     {"ssh-" + "ed25519 AAAA" + strings.Repeat("C", 40) + " someone@somewhere", kindSecret},
		"urlcred.txt": {"git clone https://bu" + "ild:" + "hunter22@host.example/x.git", kindSecret},
	}
	dir := t.TempDir()
	files := map[string][]byte{}
	for rel, c := range mustFind {
		files[rel] = []byte("line one\n" + c.text + "\nline three\n")
	}
	plant(t, dir, files)
	found, unused, err := scanTree(dir, nil)
	if err != nil || len(unused) != 0 {
		t.Fatalf("%v %v", err, unused)
	}
	got := map[string][]finding{}
	for _, f := range found {
		got[f.File] = append(got[f.File], f)
	}
	for rel, c := range mustFind {
		here := got[rel]
		if len(here) == 0 {
			t.Errorf("%s: nothing was found", rel)
			continue
		}
		for _, f := range here {
			if f.Kind != c.kind || f.Line != 2 {
				t.Errorf("%s: %s", rel, f)
			}
			if strings.Contains(f.String(), user) || strings.Contains(f.String(), "AAAA") {
				t.Errorf("the finding prints what it found: %s", f)
			}
		}
	}
	if len(got["ip172.txt"]) != 2 {
		t.Errorf("both addresses of the line are findings: %v", got["ip172.txt"])
	}

	// What is not to be found: placeholders, addresses that are not private, numbers that are not
	// addresses, a binary file, and whatever is in a .git.
	clean := t.TempDir()
	plant(t, clean, map[string][]byte{
		"docs/paths.md":    []byte("cd /Us" + "ers/<user>/work\ncd /ho" + "me/$USER/work\ncd %USERPROFILE%\\work\nC:\\Us" + "ers\\<name>\\x\ncd ~/work\ncd /Us" + "ers/{name}/x\n"),
		"docs/network.md":  []byte("loopback " + ip(127, 0, 0, 1) + " documentation " + ip(192, 0, 2, 7) + " public " + ip(8, 8, 8, 8) + " " + ip(172, 32, 0, 1) + " " + ip(192, 169, 0, 1) + " " + ip(11, 0, 0, 1) + "\nversion 1." + ip(10, 0, 0, 5) + ".7 and " + ip(10, 0, 0, 999) + "\n"),
		"bin/data.bin":     append([]byte("binary\x00 /Us"+"ers/"+user+"/x "), 0),
		".git/config":      []byte("path = /Us" + "ers/" + user + "/repo\n"),
		"docs/ok.md":       []byte("nothing here\n"),
		"docs/ellipsis.md": []byte("under `/mnt/c/Us" + "ers/...` and \"/ho" + "me/...\"\n"),
		"docs/public6.md":  []byte("documentation 20" + "01:db8::1, loopback ::1, a time 12:34:56, a mac aa:bb:cc:dd:ee:ff, " + ip(100, 63, 255, 255) + " and " + ip(100, 128, 0, 1) + " are not LAN addresses\n"),
		"docs/url.md":      []byte("see https://example.org/docs and https://example.org:8443/x and ssh://git@example.org/x.git\n"),
		"bin/big.bin":      append(bytes.Repeat([]byte{0}, 16), bytes.Repeat([]byte("/Us"+"ers/"+user+"/x\n"), (maxScanBytes/20)+1)...),
		"docs/fake.md":     []byte("the sentinel s" + "k-live-do-not-echo-0123456789 is no key, and nor is task-" + strings.Repeat("a", 40) + "\n"),
	})
	if found, _, err := scanTree(clean, nil); err != nil || len(found) != 0 {
		t.Fatalf("%v %v", err, found)
	}
}

// TestAnAllowanceExcusesExactlyItsLineAndAStaleOneIsAFailure: an entry excuses its value on its
// line and nothing else (not the same value on another line, not another value on the line,
// not the same line of another file), and an entry that excuses nothing is reported.
func TestAnAllowanceExcusesExactlyItsLineAndAStaleOneIsAFailure(t *testing.T) {
	dir := t.TempDir()
	a, b := ip(10, 0, 0, 5), ip(10, 0, 0, 6)
	plant(t, dir, map[string][]byte{
		"a_test.go": []byte("refuse(\"http://" + a + "/v1\")\nuse(\"" + a + "\")\nrefuse(\"http://" + a + "/v1\") // and " + b + "\n"),
		"b_test.go": []byte("refuse(\"http://" + a + "/v1\")\n"),
	})
	allow := []allowance{
		{"a_test.go", a, `refuse("http://` + a + `/v1")`, "a made-up address that the code under test must refuse, in this file only"},
		{"a_test.go", a, `never on any line of the tree`, "an entry that excuses nothing, which the guard must report"},
	}
	found, unused, err := scanTree(dir, allow)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range found {
		got = append(got, f.String())
	}
	// a_test.go:1 holds the excused value on the excused fragment, and :3 does too, but :3 holds
	// another address as well, which nothing excuses; :2 holds the value on another line; b_test.go
	// has no entry.
	want := []string{"a_test.go:2: " + kindPrivateIP, "a_test.go:3: " + kindPrivateIP, "b_test.go:1: " + kindPrivateIP}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(unused) != 1 || unused[0].Line != "never on any line of the tree" {
		t.Fatalf("%v", unused)
	}
}

// TestAFileTooLargeToBeReadIsAFailureAndNotASkip.
func TestAFileTooLargeToBeReadIsAFailureAndNotASkip(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir, map[string][]byte{"big.txt": bytes.Repeat([]byte("a"), maxScanBytes+1)})
	found, _, err := scanTree(dir, nil)
	if err != nil || len(found) != 1 || found[0].Kind != kindTooLarge {
		t.Fatalf("%v %v", err, found)
	}
}

// TestALinkIsReadAsTheTextItHoldsAndNotFollowed: git commits where a link points, so a link
// whose target is a path of a user is a finding; a link to a placeholder is not.
func TestALinkIsReadAsTheTextItHoldsAndNotFollowed(t *testing.T) {
	dir := t.TempDir()
	user := "al" + "ice"
	if err := os.Symlink("/Us"+"ers/"+user+"/work", filepath.Join(dir, "real")); err != nil {
		t.Skipf("no symbolic link here: %v", err)
	}
	if err := os.Symlink("../<user>/work", filepath.Join(dir, "placeholder")); err != nil {
		t.Fatal(err)
	}
	found, _, err := scanTree(dir, nil)
	if err != nil || len(found) != 1 || found[0].File != "real" || found[0].Kind != kindRealPath {
		t.Fatalf("%v %v", err, found)
	}
}
