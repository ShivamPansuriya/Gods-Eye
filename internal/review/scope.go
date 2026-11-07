package review

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ShivamPansuriya/gods-eye/internal/diff"
	"github.com/ShivamPansuriya/gods-eye/internal/scope"
)

// Scope decides what the review covers and returns the diff without credential
// files, so no agent, report or PR comment ever contains them. read returns a
// repository file (for checklists kept in .md files) and may be nil.
func Scope(d *diff.Diff, cfg scope.Config, read func(string) (string, bool)) (*diff.Diff, *scope.Plan) {
	paths := make([]string, len(d.Files))
	binary := map[string]bool{}
	for i, f := range d.Files {
		paths[i] = f.Path
		binary[f.Path] = f.Status == "binary"
	}
	plan := scope.Decide(paths, func(p string) bool { return binary[p] }, cfg, read)
	return withoutSecrets(d, plan), plan
}

func withoutSecrets(d *diff.Diff, plan *scope.Plan) *diff.Diff {
	keep := map[string]bool{}
	drop := false
	for _, f := range d.Files {
		if plan.Of(f.Path).Kind == scope.Secret || plan.Of(f.OldPath).Kind == scope.Secret {
			drop = true
			continue
		}
		keep[f.Path] = true
	}
	if !drop {
		return d
	}
	return d.Only(keep)
}

// intersect returns the paths in set that are also in only (all of set when only is nil).
func intersect(set, only map[string]bool) map[string]bool {
	if only == nil {
		return set
	}
	out := map[string]bool{}
	for p := range set {
		if only[p] {
			out[p] = true
		}
	}
	return out
}

// maxChecklistChars bounds the checklists one hunter reads, so a slice that
// spans many languages does not pay for all of them in full.
const maxChecklistChars = 24_000

// checklistsFor returns the review checklists for a set of files, each once,
// with the files it applies to.
func checklistsFor(plan *scope.Plan, files map[string]bool) string {
	byName := map[string][]string{}
	for p := range files {
		if n := plan.Of(p).Checklist; n != "" {
			byName[n] = append(byName[n], p)
		}
	}
	if len(byName) == 0 {
		return ""
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Slice(names, func(a, b int) bool { return len(byName[names[a]]) > len(byName[names[b]]) })
	var b strings.Builder
	b.WriteString("\n### Review checklists for your files\nPatterns that often hide defects in these languages (from open-code-review's language rules) and this repository's own rules. Use them to look harder; still report only real defects with a concrete scenario, never style, naming or typos.\n")
	for _, n := range names {
		text := plan.Checklists[n]
		if text == "" {
			continue
		}
		if b.Len()+len(text) > maxChecklistChars {
			fmt.Fprintf(&b, "\n#### %s (left out: the checklists for this slice are too long)\n", n)
			continue
		}
		ps := byName[n]
		sort.Strings(ps)
		fmt.Fprintf(&b, "\n#### Checklist: %s (applies to %d of your files)\n%s\n", n, len(ps), text)
	}
	return b.String()
}
