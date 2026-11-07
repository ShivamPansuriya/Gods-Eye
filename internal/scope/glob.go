package scope

import (
	"regexp"
	"strings"
)

// Glob is a compiled path pattern with the same meaning as open-code-review's
// rule files: * stays inside one path segment, ** spans any number of
// segments, ? is one character, {a,b} is one of the alternatives. Matching
// ignores case.
type Glob struct {
	Src string
	re  *regexp.Regexp
}

// Compile turns a pattern into a Glob.
func Compile(pattern string) Glob {
	var alts []string
	for _, p := range expandBraces(strings.ToLower(strings.TrimPrefix(pattern, "./"))) {
		alts = append(alts, globRegexp(p))
	}
	return Glob{Src: pattern, re: regexp.MustCompile("^(?:" + strings.Join(alts, "|") + ")$")}
}

// Match reports whether path matches.
func (g Glob) Match(path string) bool {
	return g.re != nil && g.re.MatchString(strings.ToLower(path))
}

func compileAll(patterns []string) []Glob {
	out := make([]Glob, 0, len(patterns))
	for _, p := range patterns {
		if strings.TrimSpace(p) != "" {
			out = append(out, Compile(p))
		}
	}
	return out
}

func firstMatch(gs []Glob, path string) (Glob, bool) {
	for _, g := range gs {
		if g.Match(path) {
			return g, true
		}
	}
	return Glob{}, false
}

var braceRe = regexp.MustCompile(`\{([^{}]*)\}`)

func expandBraces(p string) []string {
	m := braceRe.FindStringSubmatchIndex(p)
	if m == nil {
		return []string{p}
	}
	var out []string
	for _, alt := range strings.Split(p[m[2]:m[3]], ",") {
		out = append(out, expandBraces(p[:m[0]]+alt+p[m[1]:])...)
	}
	return out
}

func globRegexp(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); {
		switch {
		case strings.HasPrefix(p[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(p[i:], "/**") && i+3 == len(p):
			b.WriteString("(?:/.*)?")
			i += 3
		case strings.HasPrefix(p[i:], "**"):
			b.WriteString(".*")
			i += 2
		case p[i] == '*':
			b.WriteString("[^/]*")
			i++
		case p[i] == '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(p[i : i+1]))
			i++
		}
	}
	return b.String()
}
