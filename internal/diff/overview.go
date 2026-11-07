package diff

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Overview is the annotated diff cut to fit limit characters, for agents that
// read the whole PR. Every file and every hunk ID is always listed, so nothing
// becomes invisible. Bodies are added in order of value: code before data
// files, smaller files first; first as changed lines only, then with context.
// A file whose body does not fit is marked [summarized] and the agent reads it
// with read_file. It returns the text and how many files were summarized.
func (d *Diff) Overview(limit int) (string, int) {
	if full := d.Annotated(); len(full) <= limit {
		return full, 0
	}
	type view struct {
		skel, changed, full string
		level               int // 0 summarized, 1 changed lines, 2 full
	}
	vs := make([]view, len(d.Files))
	size := 0
	for i, f := range d.Files {
		var sk, ch, fu strings.Builder
		fmt.Fprintf(&sk, "=== FILE %s (%s, +%d -%d) [summarized: body not shown, read it with read_file]\n", f.Path, f.Status, f.Adds, f.Dels)
		fmt.Fprintf(&ch, "=== FILE %s (%s, +%d -%d) [changed lines only]\n", f.Path, f.Status, f.Adds, f.Dels)
		writeFile(&fu, f)
		for _, h := range f.Hunks {
			hdr := fmt.Sprintf("--- HUNK %s @@ -%d,%d +%d,%d @@ %s\n", h.ID, h.OldStart, h.OldLines, h.NewStart, h.NewLines, h.Header)
			sk.WriteString(hdr)
			ch.WriteString(hdr)
			for _, l := range h.Lines {
				if l.Kind != Context {
					writeLine(&ch, l)
				}
			}
		}
		vs[i] = view{skel: sk.String(), changed: ch.String(), full: fu.String()}
		size += len(vs[i].skel)
	}
	order := make([]int, len(d.Files))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		fa, fb := d.Files[order[a]], d.Files[order[b]]
		if da, db := isData(fa.Path), isData(fb.Path); da != db {
			return !da
		}
		return len(vs[order[a]].changed) < len(vs[order[b]].changed)
	})
	for lvl := 1; lvl <= 2; lvl++ {
		for _, i := range order {
			v := &vs[i]
			cur, next := v.skel, v.changed
			if lvl == 2 {
				cur, next = v.changed, v.full
			}
			if v.level != lvl-1 {
				continue
			}
			if size-len(cur)+len(next) <= limit {
				size += len(next) - len(cur)
				v.level = lvl
			}
		}
	}
	var b strings.Builder
	summarized := 0
	for _, v := range vs {
		switch v.level {
		case 0:
			b.WriteString(v.skel)
			summarized++
		case 1:
			b.WriteString(v.changed)
		default:
			b.WriteString(v.full)
		}
	}
	return b.String(), summarized
}

// Only returns the part of the diff that touches the given files.
func (d *Diff) Only(paths map[string]bool) *Diff {
	o := &Diff{}
	for _, f := range d.Files {
		if paths[f.Path] {
			o.Files = append(o.Files, f)
		}
	}
	return o
}

// isData reports files that are mostly data, not logic: fixtures, seeds,
// snapshots, docs. They are the first to be summarized.
func isData(p string) bool {
	l := strings.ToLower(p)
	switch path.Ext(l) {
	case ".json", ".csv", ".tsv", ".md", ".txt", ".svg", ".snap", ".lock", ".ndjson":
		return true
	}
	return strings.Contains(l, "/fixtures/") || strings.Contains(l, "/testdata/") || strings.Contains(l, "/generated/")
}
