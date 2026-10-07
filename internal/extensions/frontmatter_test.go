package extensions

import (
	"errors"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

func fm(lines ...string) []byte {
	return []byte("---\n" + strings.Join(lines, "\n") + "\n---\n# body\n")
}

func TestAHeaderInTheSubsetIsRead(t *testing.T) {
	md, err := ParseSkill(fm(
		"# a comment",
		"name: run-tests",
		"description: Run the tests of the project  # not part of the value",
		"license: \"MIT\"",
		"compatibility: 'works with ''any'' shell'",
		"metadata:",
		"  author: someone",
		"  version: \"1.0\"",
		"",
		"  note: 'quoted: with colon'",
	))
	if err != nil {
		t.Fatal(err)
	}
	if md.Name != "run-tests" || md.Description != "Run the tests of the project" || md.License == nil || *md.License != "MIT" ||
		md.Compatibility == nil || *md.Compatibility != "works with 'any' shell" ||
		md.Metadata["author"] != "someone" || md.Metadata["version"] != "1.0" || md.Metadata["note"] != "quoted: with colon" || len(md.Metadata) != 3 {
		t.Fatalf("%+v", md)
	}
	// CRLF line ends are the same header.
	crlf := strings.ReplaceAll(string(fm("name: s", "description: d")), "\n", "\r\n")
	if md, err := ParseSkill([]byte(crlf)); err != nil || md.Name != "s" || md.Description != "d" {
		t.Fatalf("%+v %v", md, err)
	}
	// Escapes of a double-quoted value.
	if md, err := ParseSkill(fm("name: s", `description: "tab\there \"quoted\" é \x41"`)); err != nil || md.Description != "tab\there \"quoted\" é A" {
		t.Fatalf("%q %v", md.Description, err)
	}
	// The body after the closing line is not read.
	body := []byte("---\nname: s\ndescription: d\n---\nname: [not: yaml\n*alias &anchor !tag\n---\nmore\n")
	if _, err := ParseSkill(body); err != nil {
		t.Fatalf("the body was read as YAML: %v", err)
	}
}

// TestWhatIsOutsideTheSubsetIsRefusedNotInterpreted: each of these would be accepted by a
// YAML library that is left to its defaults.
func TestWhatIsOutsideTheSubsetIsRefusedNotInterpreted(t *testing.T) {
	for name, data := range map[string][]byte{
		"a key twice":                   fm("name: s", "description: d", "name: t"),
		"an unknown key":                fm("name: s", "description: d", "version: x"),
		"an anchor":                     fm("name: s", "description: &a text"),
		"an alias":                      fm("name: s", "description: *a"),
		"a tag":                         fm("name: s", "description: !!str text"),
		"a local tag":                   fm("name: s", "description: !custom text"),
		"a merge key":                   fm("name: s", "description: d", "<<: *base"),
		"a merge value":                 fm("name: s", "description: <<"),
		"a block scalar":                fm("name: s", "description: |", "  text"),
		"a folded scalar":               fm("name: s", "description: >-", "  text"),
		"a flow sequence":               fm("name: s", "description: [a, b]"),
		"a flow mapping":                fm("name: s", "description: d", "metadata: {a: b}"),
		"a second document marker":      fm("name: s", "description: d", "..."),
		"a document marker with text":   fm("name: s", "description: d", "--- other"),
		"a directive":                   fm("%YAML 1.2", "name: s", "description: d"),
		"a value on two lines":          fm("name: s", "description: first", "  second"),
		"a sequence":                    fm("name: s", "description: d", "- a"),
		"a plain value with a colon":    fm("name: s", "description: use it: when needed"),
		"a plain value ending a colon":  fm("name: s", "description: d:"),
		"a number as name":              fm("name: 123", "description: d"),
		"a number as description":       fm("name: s", "description: 12.5"),
		"a hex number":                  fm("name: s", "description: 0x1F"),
		"an octal-looking number":       fm("name: s", "description: 0755"),
		"a float with an exponent":      fm("name: s", "description: 1e3"),
		"infinity":                      fm("name: s", "description: .inf"),
		"a boolean":                     fm("name: s", "description: true"),
		"a boolean in capitals":         fm("name: s", "description: FALSE"),
		"null":                          fm("name: s", "description: null"),
		"a tilde":                       fm("name: s", "description: ~"),
		"an empty value":                fm("name: s", "description:"),
		"a number in the metadata":      fm("name: s", "description: d", "metadata:", "  count: 3"),
		"a boolean in the metadata":     fm("name: s", "description: d", "metadata:", "  on: yes", "  flag: true"),
		"metadata with nothing in it":   fm("name: s", "description: d", "metadata:"),
		"metadata as a flow mapping":    fm("name: s", "description: d", "metadata: {}"),
		"metadata keys twice":           fm("name: s", "description: d", "metadata:", "  a: x", "  a: y"),
		"metadata indented unevenly":    fm("name: s", "description: d", "metadata:", "  a: x", "    b: y"),
		"metadata nested":               fm("name: s", "description: d", "metadata:", "  a:", "    b: y"),
		"a tab for indentation":         fm("name: s", "description: d", "metadata:", "\ta: x"),
		"a bad escape":                  fm("name: s", `description: "bad \q"`),
		"an unclosed quote":             fm("name: s", `description: "never closed`),
		"text after a quote":            fm("name: s", `description: "closed" and more`),
		"a key with no space":           fm("name:s", "description: d"),
		"a quoted key":                  fm(`"name": s`, "description: d"),
		"a control character":           fm("name: s", "description: a\x01b"),
		"no name":                       fm("description: d"),
		"no description":                fm("name: s"),
		"a name in capitals":            fm("name: Skill", "description: d"),
		"a name with an underscore":     fm("name: my_skill", "description: d"),
		"a name with a leading hyphen":  fm("name: -skill", "description: d"),
		"a name with a trailing hyphen": fm("name: skill-", "description: d"),
		"a name with two hyphens":       fm("name: my--skill", "description: d"),
		"a name over 64 bytes":          fm("name: "+strings.Repeat("a", 65), "description: d"),
		"a description over 1024":       fm("name: s", "description: "+strings.Repeat("d", 1025)),
		"a license over 1024":           fm("name: s", "description: d", "license: "+strings.Repeat("l", 1025)),
		"a compatibility over 1024":     fm("name: s", "description: d", "compatibility: "+strings.Repeat("c", 1025)),
		"no opening line":               []byte("name: s\ndescription: d\n"),
		"a BOM before the opening":      append([]byte("\xef\xbb\xbf"), fm("name: s", "description: d")...),
		"an opening with text":          []byte("--- \nname: s\ndescription: d\n---\n"),
		"no closing line":               []byte("---\nname: s\ndescription: d\n"),
		"invalid UTF-8":                 []byte("---\nname: s\ndescription: d\xff\n---\n"),
		"an empty file":                 nil,
	} {
		if _, err := ParseSkill(data); !errors.Is(err, ErrInvalidAsset) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTheBoundariesOfTheSubsetAreExact(t *testing.T) {
	// 1 byte and 64 bytes of name, 1 and 1024 bytes of description are fine.
	for _, c := range []struct{ name, desc string }{{"a", "d"}, {strings.Repeat("a", 64), strings.Repeat("d", 1024)}, {"a-b-c", "ünïcode ✓"}} {
		if md, err := ParseSkill(fm("name: "+c.name, "description: "+c.desc)); err != nil || md.Name != c.name || md.Description != c.desc {
			t.Errorf("%q %q: %v", c.name, c.desc, err)
		}
	}
	// 1024 bytes of UTF-8, not 1024 characters.
	if _, err := ParseSkill(fm("name: s", "description: "+strings.Repeat("é", 513))); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("1026 bytes in 513 characters: %v", err)
	}
	// 32 metadata entries are fine, 33 are not; a value of 1024 bytes is, 1025 is not.
	var lines []string
	for i := 0; i < 32; i++ {
		lines = append(lines, "  k"+strings.Repeat("x", i)+": v")
	}
	if md, err := ParseSkill(fm(append([]string{"name: s", "description: d", "metadata:"}, lines...)...)); err != nil || len(md.Metadata) != 32 {
		t.Fatalf("32 entries: %v", err)
	}
	lines = append(lines, "  k-last: v")
	if _, err := ParseSkill(fm(append([]string{"name: s", "description: d", "metadata:"}, lines...)...)); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("33 entries: %v", err)
	}
	if _, err := ParseSkill(fm("name: s", "description: d", "metadata:", "  k: "+strings.Repeat("v", 1024))); err != nil {
		t.Errorf("a value of 1024: %v", err)
	}
	if _, err := ParseSkill(fm("name: s", "description: d", "metadata:", "  k: "+strings.Repeat("v", 1025))); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("a value of 1025: %v", err)
	}
	if _, err := ParseSkill(fm("name: s", "description: d", "metadata:", "  "+strings.Repeat("k", 1025)+": v")); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("a key of 1025: %v", err)
	}
	// A header over 8 KiB is too large, not merely invalid.
	big := fm("name: s", "description: d", "license: "+strings.Repeat("l", 1000), "compatibility: "+strings.Repeat("c", 1000), "metadata:",
		"  a: "+strings.Repeat("a", 1000), "  b: "+strings.Repeat("b", 1000), "  c: "+strings.Repeat("c", 1000), "  d: "+strings.Repeat("d", 1000),
		"  e: "+strings.Repeat("e", 1000), "  f: "+strings.Repeat("f", 1000), "  g: "+strings.Repeat("g", 100))
	if _, err := ParseSkill(big); !errors.Is(err, ErrAssetTooLarge) {
		t.Errorf("a header over 8 KiB: %v", err)
	}
	// ... while one that is long and under the limit is fine.
	if _, err := ParseSkill(fm("name: s", "description: d", "license: "+strings.Repeat("l", 1000), "compatibility: "+strings.Repeat("c", 1000), "metadata:",
		"  a: "+strings.Repeat("a", 1000), "  b: "+strings.Repeat("b", 1000), "  c: "+strings.Repeat("c", 1000), "  d: "+strings.Repeat("d", 1000))); err != nil {
		t.Errorf("a header under 8 KiB: %v", err)
	}
}

// TestTheParsedMetadataIsWhatTheSchemaAccepts: the Go checks and host_assets.schema.json
// SkillMetadata agree on what a header may hold.
func TestTheParsedMetadataIsWhatTheSchemaAccepts(t *testing.T) {
	md, err := ParseSkill(fm("name: a-skill", "description: d", "license: MIT", "metadata:", "  k: v"))
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"name": md.Name, "description": md.Description, "license": *md.License, "metadata": map[string]any{"k": md.Metadata["k"]}}
	v, err := strictjson.Decode(mustJSON(doc))
	if err != nil {
		t.Fatal(err)
	}
	if err := schemacheck.Validate(schemacheck.HostAssets, "SkillMetadata", v); err != nil {
		t.Fatal(err)
	}
	bad, _ := strictjson.Decode(mustJSON(map[string]any{"name": "Bad_Name", "description": "d"}))
	if err := schemacheck.Validate(schemacheck.HostAssets, "SkillMetadata", bad); err == nil {
		t.Fatal("the schema accepted a bad name")
	}
}
