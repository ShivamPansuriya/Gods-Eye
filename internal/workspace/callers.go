package workspace

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/ShivamPansuriya/gods-eye/internal/diff"
)

// Symbol is a name the PR defines or changes.
type Symbol struct {
	Name  string
	File  string
	Hunk  string
	Users []string // "path:line: code" outside the changed file
	Total int
}

var defRes = []*regexp.Regexp{
	regexp.MustCompile(`^\s*func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)\s*[\[(]`),                                                                                        // Go
	regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\*?\s+([A-Za-z_$][\w$]*)`),                                                            // JS/TS
	regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*=>`),                               // arrow fn
	regexp.MustCompile(`^\s*(?:public|private|protected|static|final|abstract|synchronized|default|\s)*(?:<[^>]+>\s*)?[\w.<>\[\],? ]+\s+([a-zA-Z_]\w*)\s*\([^;]*$`), // Java/C#/Kotlin-ish method
	regexp.MustCompile(`^\s*(?:public\s+|private\s+|protected\s+|abstract\s+|final\s+|static\s+|sealed\s+|export\s+)*(?:class|interface|record|enum|struct|trait|type)\s+([A-Z]\w*)`),
	regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)`),                         // Python
	regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?fn\s+([A-Za-z_]\w*)`), // Rust
}

var skipNames = map[string]bool{
	"main": true, "init": true, "test": true, "setup": true, "get": true, "set": true, "run": true,
	"toString": true, "equals": true, "hashCode": true, "if": true, "for": true, "while": true,
	"switch": true, "return": true, "new": true, "catch": true, "constructor": true, "render": true,
	"String": true, "Error": true, "value": true, "build": true, "apply": true, "accept": true,
}

// Symbols finds names defined or changed by the diff: definitions on added
// lines, and the enclosing function named in each hunk header.
func Symbols(d *diff.Diff) []Symbol {
	seen := map[string]bool{}
	var out []Symbol
	add := func(name, file, hunk string) {
		if len(name) < 4 || skipNames[name] || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, Symbol{Name: name, File: file, Hunk: hunk})
	}
	for _, f := range d.Files {
		if isDocOrData(f.Path) {
			continue
		}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if l.Kind != diff.Add && l.Kind != diff.Del {
					continue
				}
				if n := defName(l.Text); n != "" {
					add(n, f.Path, h.ID)
				}
			}
			if n := defName(h.Header); n != "" {
				add(n, f.Path, h.ID)
			}
		}
	}
	return out
}

func defName(s string) string {
	t := strings.TrimSpace(s)
	if t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "#") ||
		strings.HasPrefix(t, "return ") || strings.HasPrefix(t, "if ") || strings.HasPrefix(t, "else") || strings.HasPrefix(t, "new ") {
		return ""
	}
	for _, re := range defRes {
		if m := re.FindStringSubmatch(s); m != nil {
			return m[1]
		}
	}
	return ""
}

func isDocOrData(p string) bool {
	l := strings.ToLower(p)
	for _, s := range []string{".md", ".txt", ".json", ".lock", ".sum", ".yaml", ".yml", ".csv", ".svg", ".png", ".properties", ".sql", ".xml"} {
		if strings.HasSuffix(l, s) {
			return true
		}
	}
	return false
}

// Callers looks up where each symbol is used outside the file that defines it.
// This is the deterministic "blast radius" evidence the review can cite.
func (w *Workspace) Callers(syms []Symbol, maxSyms, perSym int) []Symbol {
	if len(syms) > maxSyms {
		syms = syms[:maxSyms]
	}
	type res struct {
		i int
		s Symbol
	}
	ch := make(chan res, len(syms))
	sem := make(chan struct{}, 8)
	for i, s := range syms {
		sem <- struct{}{}
		go func(i int, s Symbol) {
			defer func() { <-sem }()
			out := w.Grep(`\b`+regexp.QuoteMeta(s.Name)+`\b`, "")
			for _, line := range strings.Split(out, "\n") {
				if line == "" || strings.HasPrefix(line, "…") || line == "no matches" || strings.HasPrefix(line, "error") {
					continue
				}
				if strings.HasPrefix(line, s.File+":") {
					continue
				}
				s.Total++
				if len(s.Users) < perSym {
					s.Users = append(s.Users, strings.TrimSpace(line))
				}
			}
			if strings.Contains(out, "more matches") {
				var more int
				fmt.Sscanf(out[strings.LastIndex(out, "… ")+len("… "):], "%d", &more)
				s.Total += more
			}
			ch <- res{i, s}
		}(i, s)
	}
	for range syms {
		r := <-ch
		syms[r.i] = r.s
	}
	sort.SliceStable(syms, func(a, b int) bool { return syms[a].Total > syms[b].Total })
	return syms
}

// FormatCallers renders the caller index for the prompt.
func FormatCallers(syms []Symbol) string {
	var b strings.Builder
	for _, s := range syms {
		if s.Total == 0 {
			fmt.Fprintf(&b, "%s (defined in %s, %s): no uses outside its file\n", s.Name, s.File, s.Hunk)
			continue
		}
		fmt.Fprintf(&b, "%s (defined in %s, %s): %d uses outside its file\n", s.Name, s.File, s.Hunk, s.Total)
		for _, u := range s.Users {
			fmt.Fprintf(&b, "    %s\n", u)
		}
	}
	return b.String()
}
