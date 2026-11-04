// Package diff parses unified diffs (as produced by `git diff` / `gh pr diff`)
// into files and hunks with stable IDs, so review units can point at them.
package diff

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// LineKind is the role of a diff line.
type LineKind string

const (
	Context LineKind = "ctx"
	Add     LineKind = "add"
	Del     LineKind = "del"
)

// Line is one line inside a hunk. Old/New are 0 when not applicable.
type Line struct {
	Kind LineKind `json:"kind"`
	Old  int      `json:"old"`
	New  int      `json:"new"`
	Text string   `json:"text"`
}

// Hunk is one @@ block.
type Hunk struct {
	ID       string `json:"id"` // H1, H2, ... across the whole diff
	File     string `json:"file"`
	OldStart int    `json:"oldStart"`
	OldLines int    `json:"oldLines"`
	NewStart int    `json:"newStart"`
	NewLines int    `json:"newLines"`
	Header   string `json:"header"` // text after the second @@ (often the enclosing function)
	Lines    []Line `json:"lines"`
	Adds     int    `json:"adds"`
	Dels     int    `json:"dels"`
}

// File is one changed file.
type File struct {
	Path    string  `json:"path"`
	OldPath string  `json:"oldPath,omitempty"`
	Status  string  `json:"status"` // modified | added | deleted | renamed | binary
	Hunks   []*Hunk `json:"hunks"`
	Adds    int     `json:"adds"`
	Dels    int     `json:"dels"`
}

// Diff is a parsed unified diff.
type Diff struct {
	Files []*File `json:"files"`
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@ ?(.*)$`)

// Parse parses a unified diff.
func Parse(text string) (*Diff, error) {
	d := &Diff{}
	var cur *File
	var h *Hunk
	var oldN, newN int
	hunkN := 0

	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			cur = &File{Status: "modified"}
			d.Files = append(d.Files, cur)
			h = nil
			if a, b, ok := splitGitPaths(strings.TrimPrefix(line, "diff --git ")); ok {
				cur.OldPath, cur.Path = a, b
			}
		case cur == nil:
			continue
		case h == nil && strings.HasPrefix(line, "new file mode"):
			cur.Status = "added"
		case h == nil && strings.HasPrefix(line, "deleted file mode"):
			cur.Status = "deleted"
		case h == nil && strings.HasPrefix(line, "rename from "):
			cur.Status = "renamed"
			cur.OldPath = strings.TrimPrefix(line, "rename from ")
		case h == nil && strings.HasPrefix(line, "rename to "):
			cur.Path = strings.TrimPrefix(line, "rename to ")
		case h == nil && strings.HasPrefix(line, "Binary files "):
			cur.Status = "binary"
		case strings.HasPrefix(line, "--- ") && h == nil:
			if p := stripPrefix(strings.TrimPrefix(line, "--- ")); p != "" {
				cur.OldPath = p
			}
		case strings.HasPrefix(line, "+++ ") && h == nil:
			if p := stripPrefix(strings.TrimPrefix(line, "+++ ")); p != "" {
				cur.Path = p
			}
		case strings.HasPrefix(line, "@@"):
			m := hunkRe.FindStringSubmatch(line)
			if m == nil {
				return nil, fmt.Errorf("bad hunk header in %s: %q", cur.Path, line)
			}
			hunkN++
			h = &Hunk{
				ID:       fmt.Sprintf("H%d", hunkN),
				File:     cur.Path,
				OldStart: atoi(m[1]),
				OldLines: atoiDefault(m[2], 1),
				NewStart: atoi(m[3]),
				NewLines: atoiDefault(m[4], 1),
				Header:   strings.TrimSpace(m[5]),
			}
			cur.Hunks = append(cur.Hunks, h)
			oldN, newN = h.OldStart, h.NewStart
		case h != nil && strings.HasPrefix(line, "+"):
			h.Lines = append(h.Lines, Line{Kind: Add, New: newN, Text: line[1:]})
			newN++
			h.Adds++
			cur.Adds++
		case h != nil && strings.HasPrefix(line, "-"):
			h.Lines = append(h.Lines, Line{Kind: Del, Old: oldN, Text: line[1:]})
			oldN++
			h.Dels++
			cur.Dels++
		case h != nil && strings.HasPrefix(line, " "):
			h.Lines = append(h.Lines, Line{Kind: Context, Old: oldN, New: newN, Text: line[1:]})
			oldN++
			newN++
		case h != nil && line == "":
			// Some tools strip the single space on empty context lines.
			h.Lines = append(h.Lines, Line{Kind: Context, Old: oldN, New: newN})
			oldN++
			newN++
		}
		// "\ No newline at end of file" and other noise is ignored.
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return d, nil
}

// Hunks returns all hunks in order.
func (d *Diff) Hunks() []*Hunk {
	var out []*Hunk
	for _, f := range d.Files {
		out = append(out, f.Hunks...)
	}
	return out
}

// HunkByID indexes hunks by ID.
func (d *Diff) HunkByID() map[string]*Hunk {
	m := map[string]*Hunk{}
	for _, h := range d.Hunks() {
		m[h.ID] = h
	}
	return m
}

// Annotated renders the diff for the model: every hunk is labelled with its ID
// and every line with its new-file (or old-file) line number, so the model can
// cite exact locations without guessing.
func (d *Diff) Annotated() string {
	var b strings.Builder
	for _, f := range d.Files {
		fmt.Fprintf(&b, "=== FILE %s (%s, +%d -%d)\n", f.Path, f.Status, f.Adds, f.Dels)
		for _, h := range f.Hunks {
			fmt.Fprintf(&b, "--- HUNK %s @@ -%d,%d +%d,%d @@ %s\n", h.ID, h.OldStart, h.OldLines, h.NewStart, h.NewLines, h.Header)
			for _, l := range h.Lines {
				switch l.Kind {
				case Add:
					fmt.Fprintf(&b, "%5d + %s\n", l.New, l.Text)
				case Del:
					fmt.Fprintf(&b, "%5s - %s\n", fmt.Sprintf("o%d", l.Old), l.Text)
				default:
					fmt.Fprintf(&b, "%5d   %s\n", l.New, l.Text)
				}
			}
		}
	}
	return b.String()
}

func splitGitPaths(s string) (string, string, bool) {
	// "a/x b/x" — paths with spaces are rare; take the split at " b/".
	i := strings.Index(s, " b/")
	if i < 0 || !strings.HasPrefix(s, "a/") {
		return "", "", false
	}
	return s[2:i], s[i+3:], true
}

func stripPrefix(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	if p == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(p, "a/") || strings.HasPrefix(p, "b/") {
		return p[2:]
	}
	return p
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	return atoi(s)
}

// AnnotatedFiles is Annotated limited to the given paths.
func (d *Diff) AnnotatedFiles(paths map[string]bool) string {
	sub := &Diff{}
	for _, f := range d.Files {
		if paths[f.Path] {
			sub.Files = append(sub.Files, f)
		}
	}
	return sub.Annotated()
}

// Summary lists every changed file with its size, one per line.
func (d *Diff) Summary() string {
	var b strings.Builder
	for _, f := range d.Files {
		ids := make([]string, len(f.Hunks))
		for i, h := range f.Hunks {
			ids[i] = h.ID
		}
		fmt.Fprintf(&b, "%s (%s, +%d -%d; %s)\n", f.Path, f.Status, f.Adds, f.Dels, strings.Join(ids, " "))
	}
	return b.String()
}
