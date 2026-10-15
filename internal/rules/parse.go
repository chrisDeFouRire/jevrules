package rules

import (
	"strings"
)

// frontmatter holds the raw keys we care about. The rule files are not
// valid YAML in every case (`globs: "a","b"`, unquoted `**/*.java`), so the
// header is parsed line by line instead of with a YAML decoder.
type frontmatter struct {
	present     bool
	description string
	globsRaw    string
	hasGlobs    bool
	alwaysApply bool
}

// splitFrontmatter separates a leading `---` block from the body.
func splitFrontmatter(src string) (frontmatter, string) {
	var fm frontmatter
	s := strings.TrimPrefix(src, "\ufeff")
	lines := strings.SplitAfter(s, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return fm, s
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return fm, s
	}
	fm.present = true
	for _, l := range lines[1:end] {
		key, val, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "description":
			fm.description = unquote(val)
		case "globs":
			fm.globsRaw = val
			fm.hasGlobs = true
		case "alwaysApply":
			fm.alwaysApply = strings.EqualFold(unquote(val), "true")
		case "apply": // legacy form seen in older .md rules: `apply: always`
			if strings.EqualFold(unquote(val), "always") {
				fm.alwaysApply = true
			}
		}
	}
	return fm, strings.Join(lines[end+1:], "")
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// SplitGlobs parses every `globs:` variant observed in the repo:
//
//	**/*.java
//	"**/*.java"
//	"**/*.ts,**/*.html"        a.java,b.java        a.md, b.md
//	"**/a/**","**/b/**"
//	**/*.{html,ts}             (commas inside braces are not separators)
//	["a", "b"]                 (YAML flow list)
func SplitGlobs(raw string) []string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		raw = raw[1 : len(raw)-1]
	}
	var out []string
	var cur strings.Builder
	depth := 0
	flush := func() {
		if g := strings.TrimSpace(cur.String()); g != "" {
			out = append(out, g)
		}
		cur.Reset()
	}
	// Quotes only group; a comma inside `"a,b"` still separates globs.
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '"' || c == '\'':
		case c == '{':
			depth++
			cur.WriteByte(c)
		case c == '}':
			if depth > 0 {
				depth--
			}
			cur.WriteByte(c)
		case c == ',' && depth == 0:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}
