package extensions

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits of a SKILL.md (HOST_ASSETS section 3).
const (
	MaxSkillFileBytes        = 64 << 10
	MaxSkillFrontmatterBytes = 8 << 10
	MaxSkills                = 128
	maxSkillTextBytes        = 1024
	maxSkillMetadataEntries  = 32
)

// SkillMetadata is the front matter of a SKILL.md: the keys the subset has, nothing else.
type SkillMetadata struct {
	Name          string
	Description   string
	License       *string
	Compatibility *string
	Metadata      map[string]string
}

var (
	skillNameRE   = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	topKeyRE      = regexp.MustCompile(`^([A-Za-z0-9_-]+):(?:[ \t]+(.*))?$`)
	metaKeyRE     = regexp.MustCompile(`^([A-Za-z0-9_.-]+):(?:[ \t]+(.*))?$`)
	intRE         = regexp.MustCompile(`^[-+]?(?:0|[1-9][0-9_]*)$`)
	zeroPrefixRE  = regexp.MustCompile(`^[-+]?0[0-9_]+$`)
	hexOctBinRE   = regexp.MustCompile(`^[-+]?0[xXoObB][0-9a-fA-F_]+$`)
	floatRE       = regexp.MustCompile(`^[-+]?(?:[0-9][0-9_]*\.?[0-9_]*|\.[0-9][0-9_]*)(?:[eE][-+]?[0-9]+)?$`)
	infNanRE      = regexp.MustCompile(`^[-+]?\.(?:inf|Inf|INF)$|^\.(?:nan|NaN|NAN)$`)
	nullBoolWords = map[string]bool{"": true, "~": true, "null": true, "Null": true, "NULL": true,
		"true": true, "True": true, "TRUE": true, "false": true, "False": true, "FALSE": true}
)

func badYAML(format string, args ...any) error {
	return invalidAsset("the front matter is outside the YAML subset: %s", fmt.Sprintf(format, args...))
}

// ParseSkill reads the front matter of a SKILL.md and holds it to the subset HOST_ASSETS
// names, which is a good deal smaller than YAML: the first line is `---`, a later line
// `---` closes it, and between them are only `key: value` lines of the keys name,
// description, license, compatibility and metadata (a mapping of string to string, one
// indented `key: value` per line). Every value is a string: a plain or a quoted scalar on one
// line, and a plain scalar that YAML would read as a number, a boolean or null is refused
// (quote it). Refused outright, not interpreted: a key twice, a key this subset does not
// have, anchors, aliases, tags, merge keys, block scalars, flow collections, a second
// document, a value that goes on to another line, and a tab for indentation. The body after
// the closing line is not read. A header over 8 KiB is ErrAssetTooLarge; the rest is
// ErrInvalidAsset.
func ParseSkill(data []byte) (SkillMetadata, error) {
	var md SkillMetadata
	if !utf8.Valid(data) {
		return md, invalidAsset("a SKILL.md is not valid UTF-8")
	}
	text := string(data)
	first, rest, ok := strings.Cut(text, "\n")
	if !ok || strings.TrimSuffix(first, "\r") != "---" {
		return md, invalidAsset("a SKILL.md does not start with the --- line of its front matter")
	}
	var body []string
	size, closed := 0, false
	for _, line := range strings.Split(rest, "\n") {
		trimmed := strings.TrimSuffix(line, "\r")
		if trimmed == "---" {
			closed = true
			break
		}
		if size += len(line) + 1; size > MaxSkillFrontmatterBytes {
			return md, tooLarge("a front matter is larger than %d bytes", MaxSkillFrontmatterBytes)
		}
		body = append(body, trimmed)
	}
	if !closed {
		return md, invalidAsset("the front matter of a SKILL.md is not closed by a --- line")
	}

	seen := map[string]bool{}
	for i := 0; i < len(body); i++ {
		line := body[i]
		switch {
		case strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimLeft(line, " "), "#"):
			continue
		case line == "..." || strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "%"):
			return md, badYAML("a document marker or a directive")
		case line[0] == ' ' || line[0] == '\t':
			return md, badYAML("an indented line outside a mapping")
		}
		m := topKeyRE.FindStringSubmatch(line)
		if m == nil {
			return md, badYAML("a line that is not `key: value`")
		}
		key, raw := m[1], m[2]
		switch key {
		case "name", "description", "license", "compatibility", "metadata":
		default:
			return md, badYAML("a key this subset does not have")
		}
		if seen[key] {
			return md, badYAML("a key twice")
		}
		seen[key] = true
		if key == "metadata" {
			if strings.TrimSpace(stripPlainComment(raw)) != "" {
				return md, badYAML("metadata must be a block mapping")
			}
			entries, next, err := parseMetadata(body, i+1)
			if err != nil {
				return md, err
			}
			md.Metadata, i = entries, next-1
			continue
		}
		val, err := parseScalar(raw)
		if err != nil {
			return md, err
		}
		if !validText(val) {
			return md, invalidAsset("a front matter value has a control character")
		}
		switch key {
		case "name":
			md.Name = val
		case "description":
			md.Description = val
		case "license":
			md.License = &val
		case "compatibility":
			md.Compatibility = &val
		}
	}
	if !seen["name"] || !seen["description"] {
		return md, invalidAsset("a SKILL.md needs a name and a description")
	}
	if len(md.Name) < 1 || len(md.Name) > 64 || !skillNameRE.MatchString(md.Name) {
		return md, invalidAsset("the skill name is not 1 to 64 lowercase ASCII letters, digits and single hyphens")
	}
	if len(md.Description) < 1 || len(md.Description) > maxSkillTextBytes {
		return md, invalidAsset("the description is not 1 to %d bytes", maxSkillTextBytes)
	}
	for _, v := range []*string{md.License, md.Compatibility} {
		if v != nil && len(*v) > maxSkillTextBytes {
			return md, invalidAsset("a license or compatibility is over %d bytes", maxSkillTextBytes)
		}
	}
	return md, nil
}

// parseMetadata reads the indented `key: value` lines that follow `metadata:`, from line
// index start, and returns where the mapping ended (the index of the first line that is
// not part of it).
func parseMetadata(body []string, start int) (map[string]string, int, error) {
	out := map[string]string{}
	indent := -1
	i := start
	for ; i < len(body); i++ {
		line := body[i]
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimLeft(line, " "), "#") {
			continue
		}
		if line[0] != ' ' {
			break
		}
		n := len(line) - len(strings.TrimLeft(line, " "))
		if strings.HasPrefix(line[n:], "\t") {
			return nil, 0, badYAML("a tab used for indentation")
		}
		if indent == -1 {
			indent = n
		} else if n != indent {
			return nil, 0, badYAML("metadata entries are not indented alike")
		}
		m := metaKeyRE.FindStringSubmatch(line[n:])
		if m == nil {
			return nil, 0, badYAML("a metadata line that is not `key: value`")
		}
		if _, dup := out[m[1]]; dup {
			return nil, 0, badYAML("a metadata key twice")
		}
		val, err := parseScalar(m[2])
		if err != nil {
			return nil, 0, err
		}
		if len(m[1]) > maxSkillTextBytes || len(val) > maxSkillTextBytes || !validText(val) {
			return nil, 0, invalidAsset("a metadata key or value is over %d bytes or has a control character", maxSkillTextBytes)
		}
		out[m[1]] = val
		if len(out) > maxSkillMetadataEntries {
			return nil, 0, invalidAsset("metadata has more than %d entries", maxSkillMetadataEntries)
		}
	}
	if len(out) == 0 {
		return nil, 0, badYAML("metadata with no entry")
	}
	return out, i, nil
}

// validText refuses a control character (a tab is allowed).
func validText(s string) bool {
	for _, r := range s {
		if (r < 0x20 && r != '\t') || r == 0x7f {
			return false
		}
	}
	return true
}

// stripPlainComment removes a ` #` comment from a plain value.
func stripPlainComment(s string) string {
	if strings.HasPrefix(s, "#") {
		return ""
	}
	for i := 0; i+1 < len(s); i++ {
		if (s[i] == ' ' || s[i] == '\t') && s[i+1] == '#' {
			return s[:i]
		}
	}
	return s
}

// parseScalar reads one single-line value as a string, or refuses it.
func parseScalar(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", badYAML("an empty value (null)")
	}
	switch s[0] {
	case '"':
		return parseDoubleQuoted(s)
	case '\'':
		return parseSingleQuoted(s)
	case '&', '*', '!', '|', '>', '[', ']', '{', '}', '%', '@', '`', ',', '?':
		return "", badYAML("an anchor, an alias, a tag, a block scalar, a flow collection or a reserved indicator")
	}
	if (s[0] == '-' || s[0] == ':') && (len(s) == 1 || s[1] == ' ' || s[1] == '\t') {
		return "", badYAML("a sequence entry or a complex key")
	}
	plain := strings.TrimSpace(stripPlainComment(s))
	if strings.Contains(plain, ": ") || strings.HasSuffix(plain, ":") {
		return "", badYAML("a plain value with `: ` (a nested mapping)")
	}
	if plain == "<<" {
		return "", badYAML("a merge key")
	}
	if nullBoolWords[plain] || intRE.MatchString(plain) || zeroPrefixRE.MatchString(plain) || hexOctBinRE.MatchString(plain) || infNanRE.MatchString(plain) ||
		floatRE.MatchString(plain) {
		return "", badYAML("a plain value that YAML reads as null, a boolean or a number (quote it)")
	}
	return plain, nil
}

func parseSingleQuoted(s string) (string, error) {
	var b strings.Builder
	i := 1
	for i < len(s) {
		c := s[i]
		if c == '\'' {
			if i+1 < len(s) && s[i+1] == '\'' {
				b.WriteByte('\'')
				i += 2
				continue
			}
			if rest := strings.TrimSpace(s[i+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
				return "", badYAML("text after a quoted value")
			}
			return b.String(), nil
		}
		b.WriteByte(c)
		i++
	}
	return "", badYAML("a quoted value that does not close on its line")
}

func parseDoubleQuoted(s string) (string, error) {
	var b strings.Builder
	i := 1
	for i < len(s) {
		c := s[i]
		switch c {
		case '"':
			if rest := strings.TrimSpace(s[i+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
				return "", badYAML("text after a quoted value")
			}
			return b.String(), nil
		case '\\':
			if i+1 >= len(s) {
				return "", badYAML("a quoted value that does not close on its line")
			}
			i++
			switch e := s[i]; e {
			case '\\', '"', '/':
				b.WriteByte(e)
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '0':
				b.WriteByte(0)
			case 'x', 'u', 'U':
				n := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
				if i+n >= len(s) {
					return "", badYAML("a bad escape")
				}
				v, err := strconv.ParseUint(s[i+1:i+1+n], 16, 32)
				if err != nil || !utf8.ValidRune(rune(v)) {
					return "", badYAML("a bad escape")
				}
				b.WriteRune(rune(v))
				i += n
			default:
				return "", badYAML("an escape this subset does not have")
			}
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return "", badYAML("a quoted value that does not close on its line")
}
